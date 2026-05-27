// pg_proxy.go — PostgreSQL wire-protocol shadow proxy.
//
// MVP scope (PR #1):
//   - Forwards bytes between a client and the primary AlloyDB without modification.
//   - Parses frontend messages for per-query timing and structured logging.
//   - Records per-query latency by waiting for the backend's ReadyForQuery 'Z'.
//   - Does NOT terminate TLS — clients must connect with sslmode=disable.
//   - Does NOT mirror to a shadow cluster — that lands in PR #2 by wiring up
//     ShadowWorker (already implemented for the MySQL path).
//   - Does NOT support COPY with timing — the proxy gracefully degrades to plain
//     bidirectional io.Copy if it observes a CopyInResponse / CopyOutResponse,
//     sacrificing timing for that connection but staying correct.
package main

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"time"

	"github.com/google/uuid"
)

// pgwire backend (server→client) message types we inspect by name.
// (Some byte values overlap with frontend message types defined in
// pg_protocol.go, but the named constants are distinct.)
const (
	pgBackendAuthRequest      byte = 'R'
	pgBackendReadyForQuery    byte = 'Z'
	pgBackendErrorResponse    byte = 'E'
	pgBackendCopyInResponse   byte = 'G'
	pgBackendCopyOutResponse  byte = 'H'
	pgBackendCopyBothResponse byte = 'W'
)

// PgProxy is a pgwire transparent forwarder with optional TLS on either hop.
type PgProxy struct {
	config      *Config
	primaryAddr string
	listenerTLS *tls.Config  // nil → client TLS not terminated
	backendTLS  *tls.Config  // nil → backend dialed plaintext
	queryFilter *QueryFilter // nil → mirror every SQL-carrying frame to the shadow
}

func NewPgProxy(config *Config, listenerTLS, backendTLS *tls.Config) *PgProxy {
	return &PgProxy{
		config:      config,
		primaryAddr: fmt.Sprintf("%s:%s", config.PrimaryHost, config.PrimaryPort),
		listenerTLS: listenerTLS,
		backendTLS:  backendTLS,
	}
}

// Start begins accepting connections. Blocks until ctx is cancelled.
func (p *PgProxy) Start(ctx context.Context, logger *QueryLogger) error {
	listener, err := net.Listen("tcp", p.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to start listener: %w", err)
	}
	defer listener.Close()

	log.Printf("PgProxy listening on %s", p.config.ListenAddr)
	log.Printf("Primary: %s", p.primaryAddr)
	if p.config.ShadowHost != "" {
		log.Printf("Shadow:  %s:%s", p.config.ShadowHost, p.config.ShadowPort)
	}
	if logger != nil {
		log.Printf("Query logging enabled (GCS bucket: %s)", logger.bucket)
	}

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}
		go p.handleConnection(conn, logger)
	}
}

func (p *PgProxy) handleConnection(client net.Conn, logger *QueryLogger) {
	defer client.Close()
	activeConnections.Inc()
	totalConnections.Inc()
	defer activeConnections.Dec()

	clientAddr := client.RemoteAddr().String()
	debugf(p.config, "PgProxy: new connection from %s", clientAddr)

	primary, err := net.DialTimeout("tcp", p.primaryAddr, 10*time.Second)
	if err != nil {
		log.Printf("PgProxy: failed to connect to primary: %v", err)
		connectionFailures.WithLabelValues("primary").Inc()
		return
	}
	defer primary.Close()

	// Backend TLS must be established before any pgwire framing.
	var primaryConn net.Conn = primary
	if p.backendTLS != nil {
		tlsConn, err := upgradeBackendTLS(primary, p.backendTLS)
		if err != nil {
			log.Printf("PgProxy: backend TLS upgrade failed: %v", err)
			tlsFailures.Inc()
			return
		}
		primaryConn = tlsConn
		debugf(p.config, "PgProxy: backend TLS established to %s", p.primaryAddr)
	}

	clientConn, startup, err := p.forwardStartup(client, primaryConn)
	if err != nil {
		debugf(p.config, "PgProxy: startup err: %v", err)
		return
	}
	if startup == nil {
		return
	}

	if err := p.forwardAuthPhase(clientConn, primaryConn); err != nil {
		debugf(p.config, "PgProxy: auth phase err: %v", err)
		return
	}

	shadow := p.startShadowWorker(startup, logger)
	if shadow != nil {
		defer shadow.Close()
	}

	p.runQueryLoop(clientConn, primaryConn, clientAddr, logger, shadow)
}

// startShadowWorker dials + auths the shadow backend (best-effort). Returns nil if
// SHADOW_HOST is unset, the dial fails, or the per-connection sample roll loses.
// Sampling is per-connection (not per-frame); see shouldMirrorPgFrame for why.
func (p *PgProxy) startShadowWorker(startup *PgPacket, logger *QueryLogger) *PgShadowWorker {
	if p.config.ShadowHost == "" {
		return nil
	}

	if p.queryFilter != nil {
		if rate := p.queryFilter.SampleRate(); rate < 1.0 {
			if rand.Float64() >= rate {
				shadowFilteredTotal.WithLabelValues(FilterReasonSampling).Inc()
				connectionsWithoutShadow.Inc()
				debugf(p.config, "PgProxy: shadow mirroring sampled out for this connection (rate=%.4f)", rate)
				return nil
			}
		}
	}

	params := parseStartupParams(startup)
	dbname := params["database"]
	sw := NewPgShadowWorker(p.config, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sw.Initialize(ctx, dbname); err != nil {
		log.Printf("PgProxy: shadow init failed, continuing primary-only: %v", err)
		connectionsWithoutShadow.Inc()
		return nil
	}
	connectionsWithShadow.Inc()
	debugf(p.config, "PgProxy: shadow worker ready (db=%s)", dbname)
	return sw
}

// forwardStartup handles the client-side startup phase: loops on libpq's
// GSSENCRequest / SSLRequest probes (terminating TLS if listenerTLS is set),
// then forwards the real StartupMessage to the backend. Returns the
// (possibly TLS-upgraded) client conn and the StartupMessage in effect, or
// (conn, nil, nil) if the client sent a CancelRequest.
func (p *PgProxy) forwardStartup(client, primary net.Conn) (net.Conn, *PgPacket, error) {
	for {
		startup, err := ReadStartupMessage(client)
		if err != nil {
			return client, nil, fmt.Errorf("read startup from client: %w", err)
		}

		switch {
		case IsSSLRequest(startup):
			if p.listenerTLS != nil {
				tlsConn, err := upgradeClientTLS(client, p.listenerTLS)
				if err != nil {
					tlsFailures.Inc()
					return client, nil, err
				}
				debugf(p.config, "PgProxy: client TLS terminated")
				client = tlsConn
				continue
			}
			if _, err := client.Write([]byte{'N'}); err != nil {
				return client, nil, fmt.Errorf("write 'N' SSL reject to client: %w", err)
			}
			continue

		case IsGSSENCRequest(startup):
			if _, err := client.Write([]byte{'N'}); err != nil {
				return client, nil, fmt.Errorf("write 'N' GSSENC reject to client: %w", err)
			}
			continue

		case IsCancelRequest(startup):
			if _, err := primary.Write(startup.Payload); err != nil {
				return client, nil, fmt.Errorf("forward cancel to primary: %w", err)
			}
			return client, nil, nil
		}

		// Real StartupMessage — forward to the (possibly TLS-wrapped) primary.
		if _, err := primary.Write(startup.Payload); err != nil {
			return client, nil, fmt.Errorf("forward startup to primary: %w", err)
		}
		return client, startup, nil
	}
}

// forwardAuthPhase relays AuthenticationXxx ↔ PasswordMessage exchanges between
// primary and client until the server emits ReadyForQuery 'Z' (auth complete) or
// ErrorResponse 'E' (auth failed).
//
// The proxy is transparent — it does not terminate auth. Clients must present
// credentials valid for the primary cluster.
func (p *PgProxy) forwardAuthPhase(client, primary net.Conn) error {
	for {
		msg, err := ReadMessage(primary)
		if err != nil {
			return fmt.Errorf("read auth msg from primary: %w", err)
		}
		if _, err := client.Write(msg.Bytes()); err != nil {
			return fmt.Errorf("forward auth msg to client: %w", err)
		}

		switch msg.Type {
		case pgBackendAuthRequest:
			if len(msg.Payload) < 8 {
				return fmt.Errorf("malformed AuthenticationRequest")
			}
			// Auth code: uint32 BE at payload[4:8].
			code := uint32(msg.Payload[4])<<24 | uint32(msg.Payload[5])<<16 |
				uint32(msg.Payload[6])<<8 | uint32(msg.Payload[7])
			switch code {
			case 0:
				// AuthenticationOk — server follows with ParameterStatus*, BackendKeyData, ReadyForQuery.
				continue
			case 12:
				// AuthenticationSASLFinal — server-to-client only; no client response expected.
				// The server will follow with AuthenticationOk (code 0).
				continue
			default:
				// CleartextPassword (3), MD5Password (5), SASL (10), SASLContinue (11), etc.
				// Each of these expects exactly one frontend PasswordMessage 'p' from the client.
				pwMsg, err := ReadMessage(client)
				if err != nil {
					return fmt.Errorf("read password msg from client (auth code %d): %w", code, err)
				}
				if _, err := primary.Write(pwMsg.Bytes()); err != nil {
					return fmt.Errorf("forward password to primary (auth code %d): %w", code, err)
				}
			}
		case pgBackendReadyForQuery:
			return nil
		case pgBackendErrorResponse:
			authFailures.WithLabelValues("primary").Inc()
			return fmt.Errorf("primary auth error response")
		}
		// Otherwise (ParameterStatus, BackendKeyData, NoticeResponse): keep reading.
	}
}

// pgStickyStmtMapCap caps the per-connection filteredStmtNames map. A noisy
// client (or one in front of pgbouncer in session pooling) can issue many
// Parse frames with unique names that never get Close('S')'d, growing the
// map for the lifetime of the connection. The cap bounds that growth; on
// overflow the map is cleared, which loses sticky tracking for currently-
// tracked Parses — Bind/Execute that arrive next will leak through to the
// shadow as if the Parse had never been filtered, which is the same
// graceful-degradation mode that already exists when Bind/Execute arrive
// without a preceding tracked Parse.
const pgStickyStmtMapCap = 4096

// runQueryLoop is the steady-state request/response loop with per-query timing.
//
// Loop invariant: at top of loop we are in "Idle" state (last server message
// was ReadyForQuery, or we just finished auth). We:
//  1. Read one frontend message from the client.
//  2. Forward it to the primary.
//  3. If the message triggers a server response (Query 'Q' or Sync 'S'), read
//     backend messages and forward them to the client until ReadyForQuery 'Z'.
//  4. Log the round-trip duration.
//
// Messages that don't trigger a response (Parse, Bind, Execute alone, Describe,
// Close, Flush) are forwarded without waiting; their timing is reported as just
// the write latency (essentially zero).
func (p *PgProxy) runQueryLoop(client, primary net.Conn, clientAddr string, logger *QueryLogger, shadow *PgShadowWorker) {
	// Sticky-stmt tracking; see shouldMirrorPgFrame for the policy.
	filteredStmtNames := map[string]struct{}{}

	for {
		msg, err := ReadMessage(client)
		if err != nil {
			if err != io.EOF {
				debugf(p.config, "PgProxy: client read err: %v", err)
			}
			return
		}

		pgPackets.WithLabelValues("primary").Inc()
		cmdName := pgFrontendCommandName(msg.Type)
		pgCommands.WithLabelValues("primary", cmdName).Inc()

		queryText := extractPgQueryText(msg)
		var queryHash string
		if queryText != "" {
			queryHash = fmt.Sprintf("%x", md5.Sum([]byte(queryText)))
		}

		req := QueryRequest{
			ID:         uuid.New().String(),
			QueryText:  queryText,
			QueryHash:  queryHash,
			Command:    cmdName,
			ClientAddr: clientAddr,
			ReceivedAt: time.Now(),
		}

		start := time.Now()
		nWritten, werr := primary.Write(msg.Bytes())
		if werr != nil {
			log.Printf("PgProxy: primary write err: %v", werr)
			queryErrors.WithLabelValues("primary").Inc()
			p.logEntry(logger, req, int64(nWritten), 0, time.Since(start), werr)
			return
		}
		bytesTotal.WithLabelValues("primary", "sent").Add(float64(nWritten))
		if isPgCountableQuery(msg.Type) {
			queryTotal.WithLabelValues("primary").Inc()
		}

		if shadow != nil && msg.Type != pgMsgTerminate {
			allowed, filterReason := p.shouldMirrorPgFrame(msg, req, filteredStmtNames)
			if !allowed {
				shadowFilteredTotal.WithLabelValues(filterReason).Inc()
				debugf(p.config, "PgProxy: shadow frame filtered (%s): %s", filterReason, req.Command)
				p.logFilteredShadow(logger, req, filterReason)
			} else {
				shadow.Send(pgShadowFrame{
					req:             req,
					payload:         append([]byte(nil), msg.Bytes()...),
					expectsResponse: msg.Type == pgMsgQuery || msg.Type == pgMsgSync,
				})
				if msg.Type == pgMsgClose {
					if kind, name := extractPgCloseTarget(msg); kind == 'S' && name != "" {
						delete(filteredStmtNames, name)
					}
				}
			}
		}

		if msg.Type == pgMsgTerminate {
			p.logEntry(logger, req, int64(nWritten), 0, time.Since(start), nil)
			return
		}

		// Only Query and Sync trigger a server response that ends in ReadyForQuery.
		if msg.Type != pgMsgQuery && msg.Type != pgMsgSync {
			p.logEntry(logger, req, int64(nWritten), 0, time.Since(start), nil)
			continue
		}

		bytesRecv, copyMode, rerr := p.forwardResponseUntilReady(primary, client)
		duration := time.Since(start)

		if rerr != nil {
			log.Printf("PgProxy: primary response err: %v", rerr)
			queryErrors.WithLabelValues("primary").Inc()
			p.logEntry(logger, req, int64(nWritten), bytesRecv, duration, rerr)
			return
		}

		bytesTotal.WithLabelValues("primary", "received").Add(float64(bytesRecv))
		queryDuration.WithLabelValues("primary").Observe(duration.Seconds())
		p.logEntry(logger, req, int64(nWritten), bytesRecv, duration, nil)

		if copyMode {
			debugf(p.config, "PgProxy: COPY observed, falling back to bidirectional io.Copy for %s", clientAddr)
			p.copyFallback(client, primary)
			return
		}
	}
}

// shouldMirrorPgFrame applies the shadow-filter policy to a pgwire frame.
//
// Why sticky-by-stmt-name: the extended query protocol splits a statement
// across Parse → Bind → Execute, but only Parse carries QueryText. Filtering
// per-frame would drop a Parse while its Bind/Execute (empty QueryText) sail
// through, breaking the shadow session with "prepared statement S_N does not
// exist". So we apply the deterministic filter to Query/Parse, remember the
// stmt name of any Parse we dropped, and drop downstream Bind/Describe/Close
// referencing that name. Sampling is handled even earlier — once per
// connection, in startShadowWorker. Other frames (Sync, Flush, Execute alone,
// CopyData) pass through to preserve pipeline ordering.
//
// Mutates filteredStmtNames in place.
func (p *PgProxy) shouldMirrorPgFrame(msg *PgPacket, req QueryRequest, filteredStmtNames map[string]struct{}) (bool, string) {
	switch msg.Type {
	case pgMsgQuery:
		return shouldShadowMirror(req, p.queryFilter)

	case pgMsgParse:
		allowed, reason := shouldShadowMirror(req, p.queryFilter)
		if !allowed {
			if name := extractPgParseStmtName(msg); name != "" {
				if len(filteredStmtNames) >= pgStickyStmtMapCap {
					clear(filteredStmtNames)
					pgStickyStmtMapResets.Inc()
				}
				filteredStmtNames[name] = struct{}{}
			}
		}
		return allowed, reason

	case pgMsgBind:
		if name := extractPgBindStmtName(msg); name != "" {
			if _, ok := filteredStmtNames[name]; ok {
				return false, FilterReasonStickyStmt
			}
		}
		return true, FilterReasonNone

	case pgMsgDescribe:
		if kind, name := extractPgDescribeTarget(msg); kind == 'S' && name != "" {
			if _, ok := filteredStmtNames[name]; ok {
				return false, FilterReasonStickyStmt
			}
		}
		return true, FilterReasonNone

	case pgMsgClose:
		if kind, name := extractPgCloseTarget(msg); kind == 'S' && name != "" {
			if _, ok := filteredStmtNames[name]; ok {
				delete(filteredStmtNames, name)
				return false, FilterReasonStickyStmt
			}
		}
		return true, FilterReasonNone

	default:
		return true, FilterReasonNone
	}
}

// forwardResponseUntilReady reads backend messages from primary, forwards them
// to client, and stops when ReadyForQuery 'Z' is observed (or COPY is entered).
// Returns (bytes-forwarded, entered-copy-mode, error).
func (p *PgProxy) forwardResponseUntilReady(primary, client net.Conn) (int64, bool, error) {
	var total int64
	for {
		msg, err := ReadMessage(primary)
		if err != nil {
			return total, false, err
		}
		buf := msg.Bytes()
		n, werr := client.Write(buf)
		total += int64(n)
		if werr != nil {
			return total, false, werr
		}

		switch msg.Type {
		case pgBackendReadyForQuery:
			return total, false, nil
		case pgBackendCopyInResponse, pgBackendCopyOutResponse, pgBackendCopyBothResponse:
			return total, true, nil
		}
	}
}

// copyFallback runs plain bidirectional io.Copy. Used when COPY mode is
// entered, since CopyData / CopyDone alter the message-flow contract.
func (p *PgProxy) copyFallback(client, primary net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(primary, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, primary)
		done <- struct{}{}
	}()
	<-done
	primary.Close()
	client.Close()
}

func (p *PgProxy) logEntry(logger *QueryLogger, req QueryRequest, bytesSent, bytesRecv int64, duration time.Duration, err error) {
	if logger == nil {
		return
	}
	logger.Log(QueryLogEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		QueryID:    req.ID,
		Target:     "primary",
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

// logFilteredShadow writes a target=shadow log entry for a frame that was
// filtered out of mirroring. Mirrors the MySQL path's logFilteredShadow so the
// BigQuery view of shadow vs. primary stays one-to-one.
func (p *PgProxy) logFilteredShadow(logger *QueryLogger, req QueryRequest, filterReason string) {
	if logger == nil {
		return
	}
	logger.Log(QueryLogEntry{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		QueryID:      req.ID,
		Target:       "shadow",
		Command:      req.Command,
		QueryText:    req.QueryText,
		QueryHash:    req.QueryHash,
		DurationMs:   0,
		BytesSent:    0,
		BytesRecv:    0,
		Success:      true,
		ClientAddr:   req.ClientAddr,
		Filtered:     true,
		FilterReason: filterReason,
	})
}
