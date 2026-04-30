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
	"fmt"
	"io"
	"log"
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

// PgProxy is a pgwire transparent forwarder.
type PgProxy struct {
	config      *Config
	primaryAddr string
}

func NewPgProxy(config *Config) *PgProxy {
	return &PgProxy{
		config:      config,
		primaryAddr: fmt.Sprintf("%s:%s", config.PrimaryHost, config.PrimaryPort),
	}
}

// Start begins accepting connections. Blocks until ctx is cancelled.
func (p *PgProxy) Start(ctx context.Context, logger *QueryLogger) error {
	listener, err := net.Listen("tcp", p.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to start listener: %w", err)
	}
	defer listener.Close()

	log.Printf("PgProxy listening on %s (plain TCP, no shadow yet)", p.config.ListenAddr)
	log.Printf("Primary: %s", p.primaryAddr)
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

	startup, err := p.forwardStartup(client, primary)
	if err != nil {
		debugf(p.config, "PgProxy: startup err: %v", err)
		return
	}
	if startup == nil {
		// CancelRequest: server processes and closes the connection.
		return
	}

	if err := p.forwardAuthPhase(client, primary); err != nil {
		debugf(p.config, "PgProxy: auth phase err: %v", err)
		return
	}

	p.runQueryLoop(client, primary, clientAddr, logger)
}

// forwardStartup forwards the client's first message (StartupMessage / SSLRequest /
// GSSENCRequest / CancelRequest) to the primary. Returns the StartupMessage that
// will be in effect for the rest of the session, or nil if the connection should
// be closed (CancelRequest).
//
// libpq's default negotiation can send TWO probe messages before the
// StartupMessage:
//
//   - gssencmode=prefer (default): send GSSENCRequest first.
//   - sslmode=prefer (default):    send SSLRequest after GSSENC is declined.
//
// We loop on the probe messages and only stop once the server has acknowledged
// 'N' to whichever probe was last sent and the client follows up with the real
// StartupMessage. If the server agrees to SSL/GSS ('S'), we bail — terminating
// TLS at the proxy is out of scope for MVP. Configure clients with
// `sslmode=disable gssencmode=disable` to avoid those probes entirely.
func (p *PgProxy) forwardStartup(client, primary net.Conn) (*PgPacket, error) {
	for {
		startup, err := ReadStartupMessage(client)
		if err != nil {
			return nil, fmt.Errorf("read startup from client: %w", err)
		}

		if _, err := primary.Write(startup.Payload); err != nil {
			return nil, fmt.Errorf("forward startup to primary: %w", err)
		}

		if IsSSLRequest(startup) || IsGSSENCRequest(startup) {
			resp := make([]byte, 1)
			if _, err := io.ReadFull(primary, resp); err != nil {
				return nil, fmt.Errorf("read SSL/GSSENC response: %w", err)
			}
			if _, err := client.Write(resp); err != nil {
				return nil, fmt.Errorf("forward SSL/GSSENC response: %w", err)
			}
			if resp[0] == 'S' {
				tlsFailures.Inc()
				return nil, fmt.Errorf("server agreed to SSL/GSS; proxy does not terminate TLS in MVP — connect with sslmode=disable gssencmode=disable")
			}
			// 'N' or 'E' — the client may follow up with another probe (SSLRequest
			// after GSSENC was declined) or the real StartupMessage. Loop.
			continue
		}

		if IsCancelRequest(startup) {
			return nil, nil
		}

		return startup, nil
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
func (p *PgProxy) runQueryLoop(client, primary net.Conn, clientAddr string, logger *QueryLogger) {
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
