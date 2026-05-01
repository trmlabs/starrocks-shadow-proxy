package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
)

type PgProxy struct {
	config      *Config
	primaryAddr string
}

type pgActiveQuery struct {
	request   QueryRequest
	start     time.Time
	bytesSent int64
	bytesRecv int64
	err       error
}

func NewPgProxy(config *Config) *PgProxy {
	return &PgProxy{
		config:      config,
		primaryAddr: fmt.Sprintf("%s:%s", config.PrimaryHost, config.PrimaryPort),
	}
}

func (p *PgProxy) Start(ctx context.Context, logger *QueryLogger) error {
	listener, err := net.Listen("tcp", p.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to start postgres listener: %w", err)
	}
	defer listener.Close()

	log.Printf("Postgres proxy listening on %s", p.config.ListenAddr)
	log.Printf("Primary: %s", p.primaryAddr)

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
				log.Printf("Postgres accept error: %v", err)
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

	primary, err := net.DialTimeout("tcp", p.primaryAddr, 10*time.Second)
	if err != nil {
		log.Printf("PgProxy: failed to connect to primary: %v", err)
		connectionFailures.WithLabelValues("primary").Inc()
		return
	}
	defer primary.Close()

	started, err := p.forwardStartup(client, primary)
	if err != nil {
		debugf(p.config, "PgProxy: startup error: %v", err)
		return
	}
	if !started {
		return
	}

	if err := p.forwardAuthPhase(client, primary); err != nil {
		debugf(p.config, "PgProxy: auth error: %v", err)
		return
	}

	p.runDuplex(client, primary, client.RemoteAddr().String(), logger)
}

func (p *PgProxy) forwardStartup(client, primary net.Conn) (bool, error) {
	for {
		packet, err := readPgStartupPacket(client)
		if err != nil {
			return false, fmt.Errorf("read startup from client: %w", err)
		}

		switch packet.Message.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := client.Write([]byte{'N'}); err != nil {
				return false, fmt.Errorf("decline SSL/GSS request: %w", err)
			}
			continue
		case *pgproto3.CancelRequest:
			if _, err := primary.Write(packet.Raw); err != nil {
				return false, fmt.Errorf("forward cancel request to primary: %w", err)
			}
			return false, nil
		case *pgproto3.StartupMessage:
			if _, err := primary.Write(packet.Raw); err != nil {
				return false, fmt.Errorf("forward startup to primary: %w", err)
			}
			return true, nil
		default:
			return false, fmt.Errorf("unsupported startup message %T", packet.Message)
		}
	}
}

func (p *PgProxy) forwardAuthPhase(client, primary net.Conn) error {
	for {
		packet, err := readPgBackendPacket(primary)
		if err != nil {
			return fmt.Errorf("read auth message from primary: %w", err)
		}
		if _, err := client.Write(packet.Raw); err != nil {
			return fmt.Errorf("forward auth message to client: %w", err)
		}

		if packet.ReadyForQuery {
			return nil
		}
		if _, ok := packet.Message.(*pgproto3.ErrorResponse); ok {
			authFailures.WithLabelValues("primary").Inc()
			return fmt.Errorf("primary returned auth error")
		}
		if !pgAuthTypeExpectsFrontend(packet.AuthType) {
			continue
		}

		response, err := readPgFrontendPacket(client, packet.AuthType)
		if err != nil {
			return fmt.Errorf("read auth response from client: %w", err)
		}
		if _, err := primary.Write(response.Raw); err != nil {
			return fmt.Errorf("forward auth response to primary: %w", err)
		}
	}
}

func pgAuthTypeExpectsFrontend(authType uint32) bool {
	switch authType {
	case pgproto3.AuthTypeCleartextPassword,
		pgproto3.AuthTypeMD5Password,
		pgproto3.AuthTypeGSS,
		pgproto3.AuthTypeGSSCont,
		pgproto3.AuthTypeSASL,
		pgproto3.AuthTypeSASLContinue:
		return true
	default:
		return false
	}
}

func (p *PgProxy) runDuplex(client, primary net.Conn, clientAddr string, logger *QueryLogger) {
	var (
		mu     sync.Mutex
		active *pgActiveQuery
		done   = make(chan struct{}, 2)
	)

	finishActive := func(err error) {
		mu.Lock()
		query := active
		active = nil
		mu.Unlock()
		if query == nil {
			return
		}
		if err == nil {
			err = query.err
		}
		duration := time.Since(query.start)
		if err != nil {
			queryErrors.WithLabelValues("primary").Inc()
		} else {
			queryDuration.WithLabelValues("primary").Observe(duration.Seconds())
		}
		p.logEntry(logger, query.request, query.bytesSent, query.bytesRecv, duration, err)
	}

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			packet, err := readPgFrontendPacket(client, 0)
			if err != nil {
				if err != io.EOF {
					debugf(p.config, "PgProxy: client read error: %v", err)
				}
				return
			}

			pgPackets.WithLabelValues("primary").Inc()
			pgCommands.WithLabelValues("primary", packet.Command).Inc()

			n, err := primary.Write(packet.Raw)
			if err != nil {
				log.Printf("PgProxy: primary write error: %v", err)
				finishActive(err)
				return
			}
			bytesTotal.WithLabelValues("primary", "sent").Add(float64(n))

			if packet.CountableQuery {
				queryTotal.WithLabelValues("primary").Inc()
				mu.Lock()
				if active == nil {
					active = &pgActiveQuery{
						request:   newPgQueryRequest(packet, clientAddr),
						start:     time.Now(),
						bytesSent: int64(n),
					}
				} else if active.request.QueryText == "" && packet.QueryText != "" {
					active.request.QueryText = packet.QueryText
					active.request.QueryHash = md5Hex(packet.QueryText)
					active.bytesSent += int64(n)
				} else {
					active.bytesSent += int64(n)
				}
				mu.Unlock()
			}

			if _, ok := packet.Message.(*pgproto3.Terminate); ok {
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			packet, err := readPgBackendPacket(primary)
			if err != nil {
				if err != io.EOF {
					debugf(p.config, "PgProxy: primary read error: %v", err)
					finishActive(err)
				}
				return
			}

			n, err := client.Write(packet.Raw)
			if err != nil {
				finishActive(err)
				return
			}
			bytesTotal.WithLabelValues("primary", "received").Add(float64(n))

			_, isErrorResponse := packet.Message.(*pgproto3.ErrorResponse)
			mu.Lock()
			if active != nil {
				active.bytesRecv += int64(n)
				if isErrorResponse {
					active.err = fmt.Errorf("primary error response")
				}
			}
			hasActive := active != nil
			mu.Unlock()

			if isErrorResponse && !hasActive {
				queryErrors.WithLabelValues("primary").Inc()
			}
			if packet.ReadyForQuery {
				finishActive(nil)
			}
		}
	}()

	<-done
	_ = client.Close()
	_ = primary.Close()
	<-done
}

func newPgQueryRequest(packet *pgFrontendPacket, clientAddr string) QueryRequest {
	return QueryRequest{
		ID:         uuid.New().String(),
		QueryText:  packet.QueryText,
		QueryHash:  md5Hex(packet.QueryText),
		Command:    packet.Command,
		ClientAddr: clientAddr,
		ReceivedAt: time.Now(),
	}
}

func md5Hex(text string) string {
	if text == "" {
		return ""
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(text)))
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
