// shadow_worker.go — ShadowWorker manages a single shadow connection per client.
// Each client gets exactly one shadow connection with a bounded queue for
// back-pressure. The worker goroutine processes queries asynchronously and
// supports graceful draining on client disconnect.
package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// ShadowWorker manages a single shadow connection with a queue for a client.
// Each client gets exactly one shadow connection, with a bounded queue for backpressure.
// This ensures N clients = N shadow connections (1:1 ratio).
type ShadowWorker struct {
	conn       net.Conn
	queue      chan QueryRequest // Bounded queue for query requests with metadata
	closeCh    chan struct{}     // Signals worker to stop immediately
	drainCh    chan struct{}     // Signals worker to drain queue then stop
	doneCh     chan struct{}     // Signals worker has finished
	config     *Config
	shadowAddr string
	tlsConfig  *tls.Config
	authFunc   func(net.Conn) (net.Conn, error)
	logger     *QueryLogger // Optional query logger for per-query tracking
	wg         sync.WaitGroup
	mu         sync.Mutex
	closed     bool
}

// NewShadowWorker creates a new shadow worker with a single connection and queue.
// The authFunc is called to establish and authenticate the connection.
// The logger is optional - if nil, query logging is disabled.
func NewShadowWorker(config *Config, shadowAddr string, tlsConfig *tls.Config, authFunc func(net.Conn) (net.Conn, error), logger *QueryLogger) *ShadowWorker {
	return &ShadowWorker{
		queue:      make(chan QueryRequest, config.ShadowQueueSize),
		closeCh:    make(chan struct{}),
		drainCh:    make(chan struct{}),
		doneCh:     make(chan struct{}),
		config:     config,
		shadowAddr: shadowAddr,
		tlsConfig:  tlsConfig,
		authFunc:   authFunc,
		logger:     logger,
	}
}

// Initialize establishes the shadow connection and starts the worker goroutine.
func (w *ShadowWorker) Initialize() error {
	// Always start with plain TCP - MySQL uses STARTTLS (upgrade during handshake)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := dialer.Dial("tcp", w.shadowAddr)
	if err != nil {
		return fmt.Errorf("failed to dial shadow: %w", err)
	}

	// Authenticate with shadow (may upgrade to TLS during handshake)
	conn := rawConn
	if w.authFunc != nil {
		conn, err = w.authFunc(rawConn)
		if err != nil {
			rawConn.Close()
			return fmt.Errorf("failed to authenticate with shadow: %w", err)
		}
	}

	w.conn = conn

	// Start the worker goroutine
	w.wg.Add(1)
	go w.worker()

	// Register worker for accurate queue depth tracking
	workerRegistryMu.Lock()
	workerRegistry[w] = struct{}{}
	workerRegistryMu.Unlock()

	log.Printf("ShadowWorker: initialized with 1 connection (queue_size=%d)", w.config.ShadowQueueSize)
	return nil
}

// Send queues a query request to be sent to the shadow cluster.
// This is non-blocking - if the queue is full, the request is dropped and false is returned.
func (w *ShadowWorker) Send(req QueryRequest) bool {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	w.mu.Unlock()

	select {
	case w.queue <- req:
		// Successfully queued - queue depth tracked via worker registry
		return true
	default:
		// Queue full - drop the request
		shadowQueueDrops.Inc()
		return false
	}
}

// Close gracefully shuts down the worker, draining any pending packets first.
func (w *ShadowWorker) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()

	// Unregister worker from registry - no longer counted in queue depth
	workerRegistryMu.Lock()
	delete(workerRegistry, w)
	workerRegistryMu.Unlock()

	// Signal worker to drain queue and exit
	close(w.drainCh)

	// Wait for worker to finish draining (with timeout)
	select {
	case <-w.doneCh:
		// Worker finished draining
	case <-time.After(w.config.ShadowDrainTimeout):
		log.Printf("ShadowWorker: drain timeout after %v, forcing close", w.config.ShadowDrainTimeout)
		shadowDrainTimeouts.Inc()
		close(w.closeCh)
	}

	// Wait for worker to fully exit BEFORE closing connection to prevent
	// "use of closed network connection" errors during shutdown.
	w.wg.Wait()

	// Now safe to close the connection
	if w.conn != nil {
		w.conn.Close()
	}
}

// worker is the goroutine that processes query requests from the queue.
func (w *ShadowWorker) worker() {
	defer w.wg.Done()
	defer close(w.doneCh)

	// Create a MySQL packet reader for protocol-aware response parsing
	reader := NewMySQLPacketReader(w.conn)

	for {
		select {
		case <-w.closeCh:
			// Immediate close requested
			return
		case <-w.drainCh:
			// Drain mode: process remaining requests then exit
			w.drainPendingRequests(reader)
			return
		case req, ok := <-w.queue:
			if !ok {
				return
			}
			w.processRequest(req, reader)
		}
	}
}

// processRequest handles a single query request sent to the shadow cluster.
func (w *ShadowWorker) processRequest(req QueryRequest, reader *MySQLPacketReader) {
	// Check if we should stop before starting work
	select {
	case <-w.closeCh:
		return
	default:
	}

	shadowStart := time.Now()

	// Track packet count
	mysqlPackets.WithLabelValues("shadow").Inc()

	// Extract command and track it
	var isQuery bool
	if cmd, ok := getMySQLCommand(req.Packet); ok {
		cmdName := getMySQLCommandName(cmd)
		mysqlCommands.WithLabelValues("shadow", cmdName).Inc()

		// Only count actual queries (COM_QUERY, COM_STMT_PREPARE, COM_STMT_EXECUTE)
		isQuery = isCountableCommand(cmd)
		if isQuery {
			queryTotal.WithLabelValues("shadow").Inc()
		}
	}

	// Write complete packet to shadow
	nWritten, err := w.conn.Write(req.Packet)
	if err != nil {
		log.Printf("ShadowWorker: write error: %v", err)
		shadowWriteErrors.Inc()
		if isQuery {
			queryErrors.WithLabelValues("shadow").Inc()
		}
		// Log the failure if logger is configured
		w.logQueryExecution(req, int64(nWritten), 0, time.Since(shadowStart), err)
		return
	}
	bytesTotal.WithLabelValues("shadow", "sent").Add(float64(nWritten))

	// No-response commands (COM_QUIT, COM_STMT_CLOSE, COM_STMT_SEND_LONG_DATA)
	// do not produce a server response. Skip the read to avoid a 30-second timeout.
	cmd, _ := getMySQLCommand(req.Packet)
	if isNoResponseCommand(cmd) {
		w.logQueryExecution(req, int64(nWritten), 0, time.Since(shadowStart), nil)
		return
	}

	// Read response using command-aware dispatch.
	// Simple commands (COM_INIT_DB, COM_PING) always return a single OK/ERR packet,
	// so we bypass the full result-set parser to avoid hangs on unexpected status flags.
	w.conn.SetReadDeadline(time.Now().Add(w.config.ShadowReadTimeout))
	var bytesRead int64
	var readErr error
	if isSimpleResponseCommand(cmd) {
		bytesRead, readErr = reader.ReadSimpleResponse()
	} else {
		bytesRead, readErr = reader.ReadFullResponse()
	}
	duration := time.Since(shadowStart)

	if readErr != nil {
		if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
			shadowReadTimeouts.Inc()
			queryPreview := extractQueryPreview(req.Packet, 200)
			log.Printf("ShadowWorker: read timeout after %v, query_preview=%q", w.config.ShadowReadTimeout, queryPreview)
		} else {
			log.Printf("ShadowWorker: read error: %v", readErr)
		}
		queryDuration.WithLabelValues("shadow").Observe(duration.Seconds())
		w.logQueryExecution(req, int64(nWritten), bytesRead, duration, readErr)
		return
	}

	bytesTotal.WithLabelValues("shadow", "received").Add(float64(bytesRead))
	queryDuration.WithLabelValues("shadow").Observe(duration.Seconds())

	// Log successful execution
	w.logQueryExecution(req, int64(nWritten), bytesRead, duration, nil)
}

// logQueryExecution logs a query execution to the QueryLogger if configured
func (w *ShadowWorker) logQueryExecution(req QueryRequest, bytesSent, bytesRecv int64, duration time.Duration, err error) {
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
		DurationMs: float64(duration.Nanoseconds()) / 1e6, // Convert to milliseconds with precision
		BytesSent:  bytesSent,
		BytesRecv:  bytesRecv,
		Success:    err == nil,
		Error:      errorString(err),
		ClientAddr: req.ClientAddr,
	})
}

// drainPendingRequests processes all remaining requests in the queue before shutdown.
func (w *ShadowWorker) drainPendingRequests(reader *MySQLPacketReader) {
	for {
		select {
		case <-w.closeCh:
			// Force close requested - stop immediately
			remaining := len(w.queue)
			if remaining > 0 {
				log.Printf("ShadowWorker: abandoning %d queued requests due to force close", remaining)
				// No counter update needed - worker already unregistered from registry
			}
			return
		case req, ok := <-w.queue:
			if !ok {
				return
			}
			w.processRequest(req, reader)
		default:
			// No more pending requests
			return
		}
	}
}
