package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PgShadowWorker mirrors pgwire frames to a shadow backend asynchronously.
// Auth is delegated to pgconn (handles SCRAM-SHA-256, MD5, TLS, etc.); after
// the backend reaches Idle (ReadyForQuery), the underlying net.Conn is
// hijacked and we forward raw frames over it.
type PgShadowWorker struct {
	config  *Config
	conn    net.Conn
	queue   chan pgShadowFrame
	closeCh chan struct{}
	drainCh chan struct{}
	doneCh  chan struct{}
	logger  *QueryLogger
	wg      sync.WaitGroup
	mu      sync.Mutex
	closed  bool
	// dead latches on first I/O error; reconnecting mid-session would break
	// the shadow's prepared-statement lifecycle, so we drop until next conn.
	dead atomic.Bool
}

type pgShadowFrame struct {
	req             QueryRequest
	payload         []byte
	expectsResponse bool // Query and Sync trigger ReadyForQuery reads
}

func NewPgShadowWorker(config *Config, logger *QueryLogger) *PgShadowWorker {
	return &PgShadowWorker{
		config:  config,
		queue:   make(chan pgShadowFrame, config.ShadowQueueSize),
		closeCh: make(chan struct{}),
		drainCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
		logger:  logger,
	}
}

// Initialize connects to the shadow backend via pgconn (full auth handshake),
// then hijacks the underlying net.Conn for raw frame forwarding. dbname is
// taken from the client's StartupMessage so the shadow session targets the
// same database; user/password/host/port come from SHADOW_* config.
func (w *PgShadowWorker) Initialize(ctx context.Context, dbname string) error {
	if w.config.ShadowHost == "" {
		return fmt.Errorf("SHADOW_HOST not set")
	}
	if dbname == "" {
		dbname = w.config.ShadowUser // pg default: dbname == user
	}

	conf, err := pgconn.ParseConfig("")
	if err != nil {
		return fmt.Errorf("pgconn parse config: %w", err)
	}
	conf.Host = w.config.ShadowHost
	if port, perr := parsePort(w.config.ShadowPort); perr == nil {
		conf.Port = port
	}
	conf.User = w.config.ShadowUser
	conf.Password = w.config.ShadowPassword
	conf.Database = dbname
	conf.ConnectTimeout = 10 * time.Second
	if w.config.ShadowTLSEnabled {
		conf.TLSConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: w.config.ShadowTLSInsecure, //nolint:gosec // dev opt-in only
			ServerName:         w.config.ShadowHost,
		}
	} else {
		conf.TLSConfig = nil
	}

	pgConn, err := pgconn.ConnectConfig(ctx, conf)
	if err != nil {
		authFailures.WithLabelValues("shadow").Inc()
		return fmt.Errorf("shadow connect: %w", err)
	}

	hijacked, err := pgConn.Hijack()
	if err != nil {
		_ = pgConn.Close(ctx)
		return fmt.Errorf("hijack shadow conn: %w", err)
	}
	w.conn = hijacked.Conn

	w.wg.Add(1)
	go w.worker()

	pgShadowRegistryMu.Lock()
	pgShadowRegistry[w] = struct{}{}
	pgShadowRegistryMu.Unlock()

	return nil
}

// Send enqueues a frame. Non-blocking — drops on full queue or a dead worker.
func (w *PgShadowWorker) Send(frame pgShadowFrame) bool {
	if w.dead.Load() {
		shadowDropped.WithLabelValues("conn_dead").Inc()
		return false
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	w.mu.Unlock()

	select {
	case w.queue <- frame:
		return true
	default:
		shadowQueueDrops.Inc()
		return false
	}
}

// Close drains the queue (bounded by ShadowDrainTimeout) then closes the conn.
func (w *PgShadowWorker) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()

	pgShadowRegistryMu.Lock()
	delete(pgShadowRegistry, w)
	pgShadowRegistryMu.Unlock()

	close(w.drainCh)

	select {
	case <-w.doneCh:
	case <-time.After(w.config.ShadowDrainTimeout):
		shadowDrainTimeouts.Inc()
		close(w.closeCh)
	}
	w.wg.Wait()
	if w.conn != nil {
		_ = w.conn.Close()
	}
}

func (w *PgShadowWorker) worker() {
	defer w.wg.Done()
	defer close(w.doneCh)
	for {
		select {
		case <-w.closeCh:
			return
		case <-w.drainCh:
			w.drainQueue()
			return
		case frame, ok := <-w.queue:
			if !ok {
				return
			}
			w.processFrame(frame)
		}
	}
}

func (w *PgShadowWorker) drainQueue() {
	for {
		select {
		case <-w.closeCh:
			return
		case frame, ok := <-w.queue:
			if !ok {
				return
			}
			w.processFrame(frame)
		default:
			return
		}
	}
}

func (w *PgShadowWorker) processFrame(frame pgShadowFrame) {
	select {
	case <-w.closeCh:
		return
	default:
	}

	if w.dead.Load() {
		shadowDropped.WithLabelValues("conn_dead").Inc()
		return
	}

	start := time.Now()
	pgPackets.WithLabelValues("shadow").Inc()
	pgCommands.WithLabelValues("shadow", pgFrontendCommandName(frame.payload[0])).Inc()

	nWritten, werr := w.conn.Write(frame.payload)
	if werr != nil {
		shadowWriteErrors.Inc()
		queryErrors.WithLabelValues("shadow").Inc()
		w.markDead()
		w.logExecution(frame.req, int64(nWritten), 0, time.Since(start), werr)
		return
	}
	bytesTotal.WithLabelValues("shadow", "sent").Add(float64(nWritten))

	if !frame.expectsResponse {
		w.logExecution(frame.req, int64(nWritten), 0, time.Since(start), nil)
		return
	}

	queryTotal.WithLabelValues("shadow").Inc()

	_ = w.conn.SetReadDeadline(time.Now().Add(w.config.ShadowReadTimeout))
	var bytesRead int64
	for {
		msg, err := ReadMessage(w.conn)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				shadowReadTimeouts.Inc()
			}
			queryErrors.WithLabelValues("shadow").Inc()
			w.markDead()
			w.logExecution(frame.req, int64(nWritten), bytesRead, time.Since(start), err)
			return
		}
		bytesRead += int64(len(msg.Bytes()))
		if msg.Type == pgBackendReadyForQuery {
			break
		}
	}
	_ = w.conn.SetReadDeadline(time.Time{})

	duration := time.Since(start)
	bytesTotal.WithLabelValues("shadow", "received").Add(float64(bytesRead))
	queryDuration.WithLabelValues("shadow").Observe(duration.Seconds())
	w.logExecution(frame.req, int64(nWritten), bytesRead, duration, nil)
}

// markDead latches the dead flag and closes the conn to unblock concurrent I/O. Idempotent.
func (w *PgShadowWorker) markDead() {
	if w.dead.Swap(true) {
		return
	}
	if w.conn != nil {
		_ = w.conn.Close()
	}
}

func (w *PgShadowWorker) logExecution(req QueryRequest, bytesSent, bytesRecv int64, duration time.Duration, err error) {
	if w.logger == nil {
		return
	}
	w.logger.Log(QueryLogEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		QueryID:    req.ID,
		Target:     "shadow",
		Command:    req.Command,
		QueryText:  req.QueryText,
		QueryHash:  req.QueryHash,
		DurationMs: float64(duration.Nanoseconds()) / 1e6,
		BytesSent:  bytesSent,
		BytesRecv:  bytesRecv,
		Success:    err == nil,
		Error:      errorString(err),
		ClientAddr: req.ClientAddr,
	})
}

// parseStartupParams extracts the key/value map embedded in a pgwire
// StartupMessage. Layout: [length:4][protocol:4](key\0value\0)+\0
func parseStartupParams(p *PgPacket) map[string]string {
	out := map[string]string{}
	if p == nil || len(p.Payload) < 9 {
		return out
	}
	kv := p.Payload[8:]
	parts := bytes.Split(kv, []byte{0})
	for i := 0; i+1 < len(parts); i += 2 {
		if len(parts[i]) == 0 {
			break
		}
		out[string(parts[i])] = string(parts[i+1])
	}
	return out
}

func parsePort(s string) (uint16, error) {
	if s == "" {
		return 5432, nil
	}
	var p uint16
	_, err := fmt.Sscanf(s, "%d", &p)
	return p, err
}

// Worker registry for queue-depth gauge.
var (
	pgShadowRegistryMu sync.RWMutex
	pgShadowRegistry   = make(map[*PgShadowWorker]struct{})
)

func getPgShadowQueueDepth() int64 {
	pgShadowRegistryMu.RLock()
	defer pgShadowRegistryMu.RUnlock()
	var total int64
	for w := range pgShadowRegistry {
		total += int64(len(w.queue))
	}
	return total
}
