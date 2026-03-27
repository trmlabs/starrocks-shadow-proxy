// proxy.go — TCPProxy handles TCP proxying with optional MySQL SSL termination.
// It manages client connections, authenticates with primary and shadow backends,
// and orchestrates mirrored query forwarding through ShadowWorkers.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// TCPProxy handles TCP proxying with optional MySQL SSL termination
type TCPProxy struct {
	config          *Config
	primaryAddr     string
	shadowAddr      string
	tlsConfig       *tls.Config
	shadowTLSConfig *tls.Config  // TLS config for shadow connections (proxy as client)
	queryFilter     *QueryFilter // Optional filter for selective shadow mirroring (nil = mirror all)
}

// NewTCPProxy creates a TCP-level proxy with optional TLS support
func NewTCPProxy(config *Config) (*TCPProxy, error) {
	proxy := &TCPProxy{
		config:      config,
		primaryAddr: fmt.Sprintf("%s:%s", config.PrimaryHost, config.PrimaryPort),
		shadowAddr:  fmt.Sprintf("%s:%s", config.ShadowHost, config.ShadowPort),
	}

	// Load TLS config for client connections (proxy as server)
	if config.TLSEnabled {
		cert, err := tls.LoadX509KeyPair(config.TLSCertFile, config.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS certificates: %w", err)
		}
		proxy.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		log.Printf("TLS enabled for client connections with cert: %s", config.TLSCertFile)
	}

	// Configure TLS for shadow connections (proxy as client)
	if config.ShadowTLSEnabled {
		proxy.shadowTLSConfig = &tls.Config{
			InsecureSkipVerify: config.ShadowTLSInsecure,
			MinVersion:         tls.VersionTLS12,
		}
		log.Printf("TLS enabled for shadow connections (insecure=%v)", config.ShadowTLSInsecure)
	}

	return proxy, nil
}

// dialBackend connects to a backend using plain TCP
func (p *TCPProxy) dialBackend(addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return dialer.Dial("tcp", addr)
}

// dialShadow connects to shadow with optional TLS.
// Currently unused — shadow connections are established during authentication
// in authenticateWithShadow. Retained for potential future use.
//
//nolint:unused
func (p *TCPProxy) dialShadow() (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if p.shadowTLSConfig != nil {
		return tls.DialWithDialer(dialer, "tcp", p.shadowAddr, p.shadowTLSConfig)
	}
	return dialer.Dial("tcp", p.shadowAddr)
}

// handleConnection proxies a single client connection with optional TLS
func (p *TCPProxy) handleConnection(clientConn net.Conn, logger *QueryLogger) {
	debugf(p.config, "handleConnection: new connection from %s, TLS=%v", clientConn.RemoteAddr(), p.tlsConfig != nil)
	defer clientConn.Close()
	activeConnections.Inc()
	totalConnections.Inc()
	defer activeConnections.Dec()

	// Connect to primary backend (plain TCP)
	primaryConn, err := p.dialBackend(p.primaryAddr)
	if err != nil {
		log.Printf("Failed to connect to primary: %v", err)
		connectionFailures.WithLabelValues("primary").Inc()
		return
	}
	defer primaryConn.Close()
	debugf(p.config, "handleConnection: connected to primary")

	// Handle MySQL protocol handshake
	if p.tlsConfig != nil {
		debugf(p.config, "handleConnection: TLS enabled, starting SSL upgrade")
		clientConn, err = p.handleMySQLSSLUpgrade(clientConn, primaryConn)
		if err != nil {
			log.Printf("MySQL SSL handshake failed: %v", err)
			tlsFailures.Inc()
			return
		}
		debugf(p.config, "handleConnection: SSL upgrade complete")
	} else {
		// For non-TLS connections, we still need to forward the MySQL handshake
		debugf(p.config, "handleConnection: forwarding MySQL handshake (non-TLS)")
		if err := p.forwardMySQLHandshake(clientConn, primaryConn); err != nil {
			log.Printf("MySQL handshake failed: %v", err)
			return
		}
		debugf(p.config, "handleConnection: handshake complete")
	}

	// Create shadow worker (single connection per client)
	debugf(p.config, "handleConnection: creating shadow worker (queue_size=%d)...", p.config.ShadowQueueSize)

	shadowWorker := NewShadowWorker(p.config, p.shadowAddr, p.shadowTLSConfig, p.authenticateWithShadow, logger)
	if err := shadowWorker.Initialize(); err != nil {
		log.Printf("Failed to initialize shadow worker (continuing without): %v", err)
		connectionFailures.WithLabelValues("shadow").Inc()
		connectionsWithoutShadow.Inc()
		p.proxyWithoutShadow(clientConn, primaryConn)
		return
	}
	defer shadowWorker.Close()

	debugf(p.config, "handleConnection: shadow worker initialized, starting mirrored proxy")

	// Track successful shadow connection
	connectionsWithShadow.Inc()

	// Proxy with worker-based mirroring
	p.proxyWithMirrorWorker(clientConn, primaryConn, shadowWorker, logger)
	debugf(p.config, "handleConnection: proxy session ended")
}

// handleMySQLSSLUpgrade handles the MySQL protocol SSL upgrade
// Returns the upgraded connection (TLS if client requested, plain if not)
func (p *TCPProxy) handleMySQLSSLUpgrade(clientConn net.Conn, primaryConn net.Conn) (net.Conn, error) {
	debugf(p.config, "handleMySQLSSLUpgrade: starting SSL upgrade flow")

	// Step 1: Read handshake from primary backend
	handshake, err := readMySQLPacket(primaryConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read handshake from primary: %w", err)
	}
	debugf(p.config, "handleMySQLSSLUpgrade: got handshake from primary (%d bytes)", len(handshake))

	// Step 2: Modify handshake to advertise SSL support
	handshake = modifyHandshakeForSSL(handshake)
	debugf(p.config, "handleMySQLSSLUpgrade: modified handshake for SSL")

	// Step 3: Send modified handshake to client
	if _, err := clientConn.Write(handshake); err != nil {
		return nil, fmt.Errorf("failed to send handshake to client: %w", err)
	}
	debugf(p.config, "handleMySQLSSLUpgrade: sent handshake to client")

	// Step 4: Read client's response
	clientResponse, err := readMySQLPacket(clientConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read client response: %w", err)
	}
	debugf(p.config, "handleMySQLSSLUpgrade: got client response (%d bytes)", len(clientResponse))

	// Step 5: Check if client wants SSL upgrade
	if isSSLRequest(clientResponse) {
		debugf(p.config, "handleMySQLSSLUpgrade: client requested SSL upgrade, performing TLS handshake")

		// Upgrade to TLS
		tlsConn := tls.Server(clientConn, p.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return nil, fmt.Errorf("TLS handshake failed: %w", err)
		}
		tlsUpgrades.Inc()
		debugf(p.config, "handleMySQLSSLUpgrade: TLS handshake successful")

		// Read the actual auth packet from client (now over TLS)
		debugf(p.config, "handleMySQLSSLUpgrade: reading auth packet from TLS connection...")
		clientResponse, err = readMySQLPacket(tlsConn)
		if err != nil {
			return nil, fmt.Errorf("failed to read auth packet after TLS upgrade: %w", err)
		}
		debugf(p.config, "handleMySQLSSLUpgrade: got auth packet (%d bytes, seq=%d)", len(clientResponse), clientResponse[3])

		// Remove unsupported capabilities (SSL, DEPRECATE_EOF) before forwarding to primary
		clientResponse = removeUnsupportedCapabilities(clientResponse)

		// Fix sequence number: client sent SSL request (seq 1) then auth (seq 2)
		// But primary only saw handshake (seq 0), so it expects seq 1
		clientResponse[3] = 1
		debugf(p.config, "handleMySQLSSLUpgrade: fixed seq to 1, forwarding to primary")

		// Forward auth packet to primary (plain TCP)
		if _, err := primaryConn.Write(clientResponse); err != nil {
			return nil, fmt.Errorf("failed to forward auth to primary: %w", err)
		}
		debugf(p.config, "handleMySQLSSLUpgrade: auth packet forwarded to primary")

		// Read auth response from primary and forward to client
		debugf(p.config, "handleMySQLSSLUpgrade: reading auth response from primary...")
		authResponse, err := readMySQLPacket(primaryConn)
		if err != nil {
			return nil, fmt.Errorf("failed to read auth response from primary: %w", err)
		}

		// Check if it's OK (0x00) or Error (0xFF) packet
		respType := byte(0)
		if len(authResponse) > 4 {
			respType = authResponse[4]
			switch respType {
			case 0xFF:
				log.Printf("handleMySQLSSLUpgrade: got ERROR response from primary")
			case 0x00:
				debugf(p.config, "handleMySQLSSLUpgrade: got OK response from primary")
			case 0xFE:
				debugf(p.config, "handleMySQLSSLUpgrade: got auth switch request from primary")
			}
		}

		// Fix sequence number for client:
		// Client sent: SSL request (seq 1), then auth (seq 2)
		// Primary saw: auth (seq 1), so it responds with seq 2
		// Client expects: seq 3
		// So we change seq from 2 to 3
		debugf(p.config, "handleMySQLSSLUpgrade: got auth response (%d bytes, seq=%d), adjusting seq to 3", len(authResponse), authResponse[3])
		authResponse[3] = 3

		if _, err := tlsConn.Write(authResponse); err != nil {
			return nil, fmt.Errorf("failed to forward auth response to client: %w", err)
		}
		debugf(p.config, "handleMySQLSSLUpgrade: session established (type=0x%02X)", respType)

		return tlsConn, nil
	}

	// Client doesn't want SSL, forward the auth packet to primary
	debugf(p.config, "handleMySQLSSLUpgrade: client did not request SSL, forwarding auth to primary")
	if _, err := primaryConn.Write(clientResponse); err != nil {
		return nil, fmt.Errorf("failed to forward auth to primary: %w", err)
	}

	// Read auth response from primary and forward to client
	authResponse, err := readMySQLPacket(primaryConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read auth response from primary (non-SSL): %w", err)
	}

	// Handle auth switch request (0xFE)
	if len(authResponse) > 4 && authResponse[4] == 0xFE {
		debugf(p.config, "handleMySQLSSLUpgrade: auth switch requested (non-SSL path)")
		if _, err := clientConn.Write(authResponse); err != nil {
			return nil, fmt.Errorf("failed to forward auth switch to client: %w", err)
		}
		switchResponse, err := readMySQLPacket(clientConn)
		if err != nil {
			return nil, fmt.Errorf("failed to read auth switch response: %w", err)
		}
		if _, err := primaryConn.Write(switchResponse); err != nil {
			return nil, fmt.Errorf("failed to forward auth switch response: %w", err)
		}
		authResponse, err = readMySQLPacket(primaryConn)
		if err != nil {
			return nil, fmt.Errorf("failed to read final auth response: %w", err)
		}
	}

	// Forward auth response to client
	if _, err := clientConn.Write(authResponse); err != nil {
		return nil, fmt.Errorf("failed to forward auth response to client (non-SSL): %w", err)
	}

	if len(authResponse) > 4 {
		if authResponse[4] == 0xFF {
			return nil, fmt.Errorf("primary authentication failed (non-SSL)")
		}
		debugf(p.config, "handleMySQLSSLUpgrade: auth successful (non-SSL, type=0x%02X)", authResponse[4])
	}

	return clientConn, nil
}

// forwardMySQLHandshake forwards the MySQL handshake between primary and client for non-TLS connections.
// This is necessary because the new proxyWithMirrorWorker architecture expects the handshake to be
// complete before entering the request-response loop.
func (p *TCPProxy) forwardMySQLHandshake(clientConn net.Conn, primaryConn net.Conn) error {
	// Step 1: Read handshake from primary
	handshake, err := readMySQLPacket(primaryConn)
	if err != nil {
		return fmt.Errorf("failed to read handshake from primary: %w", err)
	}
	debugf(p.config, "forwardMySQLHandshake: got handshake from primary (%d bytes)", len(handshake))

	// Step 2: Forward handshake to client
	if _, err := clientConn.Write(handshake); err != nil {
		return fmt.Errorf("failed to send handshake to client: %w", err)
	}

	// Step 3: Read auth response from client
	authPacket, err := readMySQLPacket(clientConn)
	if err != nil {
		return fmt.Errorf("failed to read auth from client: %w", err)
	}
	debugf(p.config, "forwardMySQLHandshake: got auth from client (%d bytes)", len(authPacket))

	// Step 4: Strip CLIENT_DEPRECATE_EOF before forwarding to primary.
	// The proxy's response parser expects traditional EOF markers in result sets.
	// Without this, the MySQL CLI negotiates EOF-less protocol and the parser
	// exits early, leaving bytes unforwarded to the client (deadlock).
	authPacket = removeUnsupportedCapabilities(authPacket)
	if _, err := primaryConn.Write(authPacket); err != nil {
		return fmt.Errorf("failed to forward auth to primary: %w", err)
	}

	// Step 5: Read auth response from primary
	authResponse, err := readMySQLPacket(primaryConn)
	if err != nil {
		return fmt.Errorf("failed to read auth response from primary: %w", err)
	}

	// Check if it's an auth switch request (0xFE) - need to handle multi-round auth
	if len(authResponse) > 4 && authResponse[4] == 0xFE {
		debugf(p.config, "forwardMySQLHandshake: auth switch requested, forwarding")
		// Forward auth switch to client
		if _, err := clientConn.Write(authResponse); err != nil {
			return fmt.Errorf("failed to forward auth switch to client: %w", err)
		}

		// Read client's auth switch response
		switchResponse, err := readMySQLPacket(clientConn)
		if err != nil {
			return fmt.Errorf("failed to read auth switch response: %w", err)
		}

		// Forward to primary
		if _, err := primaryConn.Write(switchResponse); err != nil {
			return fmt.Errorf("failed to forward auth switch response: %w", err)
		}

		// Read final auth result
		authResponse, err = readMySQLPacket(primaryConn)
		if err != nil {
			return fmt.Errorf("failed to read final auth response: %w", err)
		}
	}

	// Step 6: Forward auth response to client
	if _, err := clientConn.Write(authResponse); err != nil {
		return fmt.Errorf("failed to forward auth response to client: %w", err)
	}

	// Check if auth succeeded
	if len(authResponse) > 4 {
		if authResponse[4] == 0xFF {
			return fmt.Errorf("primary authentication failed")
		}
		debugf(p.config, "forwardMySQLHandshake: primary auth successful (type=0x%02X)", authResponse[4])
	}

	return nil
}

// authenticateWithShadow performs MySQL authentication with the shadow cluster
// This is needed because shadow is a fresh connection that expects the MySQL handshake flow
// If TLS is enabled, this function handles the MySQL STARTTLS upgrade and returns the TLS connection
func (p *TCPProxy) authenticateWithShadow(shadowConn net.Conn) (net.Conn, error) {
	// Step 1: Read handshake from shadow
	handshake, err := readMySQLPacket(shadowConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read handshake from shadow: %w", err)
	}
	debugf(p.config, "authenticateWithShadow: got handshake from shadow (%d bytes)", len(handshake))

	// Extract scramble (auth plugin data) from handshake
	scramble := extractScrambleFromHandshake(handshake)
	if scramble == nil {
		return nil, fmt.Errorf("failed to extract scramble from handshake")
	}
	debugf(p.config, "authenticateWithShadow: extracted scramble (%d bytes)", len(scramble))

	// The connection we'll use for auth (may be upgraded to TLS)
	var authConn net.Conn = shadowConn
	seqNum := byte(1) // sequence number 1 (after handshake which was 0)

	// Step 2: If TLS is enabled, send SSL request and upgrade connection
	if p.shadowTLSConfig != nil {
		debugf(p.config, "authenticateWithShadow: TLS enabled, sending SSL request to shadow")

		// Build SSL request packet (minimal packet with CLIENT_SSL flag)
		// Format: 4-byte capabilities + 4-byte max_packet + 1-byte charset + 23-byte reserved = 32 bytes
		sslRequest := make([]byte, 32)

		// Capabilities: CLIENT_PROTOCOL_41 (0x200) | CLIENT_SECURE_CONNECTION (0x8000) | CLIENT_SSL (0x800)
		caps := uint32(0x00008A00) // 0x8000 | 0x0800 | 0x0200
		sslRequest[0] = byte(caps)
		sslRequest[1] = byte(caps >> 8)
		sslRequest[2] = byte(caps >> 16)
		sslRequest[3] = byte(caps >> 24)

		// Max packet size (16MB)
		sslRequest[4] = 0x00
		sslRequest[5] = 0x00
		sslRequest[6] = 0x00
		sslRequest[7] = 0x01

		// Charset: utf8 (33)
		sslRequest[8] = 33

		// Reserved: 23 zero bytes (already zero from make)

		// Build the full packet with header
		fullSSLPacket := make([]byte, 4+len(sslRequest))
		fullSSLPacket[0] = byte(len(sslRequest))
		fullSSLPacket[1] = byte(len(sslRequest) >> 8)
		fullSSLPacket[2] = byte(len(sslRequest) >> 16)
		fullSSLPacket[3] = seqNum
		copy(fullSSLPacket[4:], sslRequest)

		// Send SSL request
		if _, err := shadowConn.Write(fullSSLPacket); err != nil {
			return nil, fmt.Errorf("failed to send SSL request to shadow: %w", err)
		}
		debugf(p.config, "authenticateWithShadow: sent SSL request packet to shadow")

		// Upgrade to TLS (proxy is the client here)
		tlsConn := tls.Client(shadowConn, p.shadowTLSConfig)
		if err := tlsConn.Handshake(); err != nil {
			tlsFailures.Inc()
			return nil, fmt.Errorf("TLS handshake with shadow failed: %w", err)
		}
		debugf(p.config, "authenticateWithShadow: TLS handshake with shadow successful")

		authConn = tlsConn
		seqNum = 2 // sequence number 2 (after SSL request which was 1)
	}

	// Step 3: Build auth packet with actual credentials
	username := p.config.ShadowUser
	password := p.config.ShadowPassword

	authPacket := make([]byte, 0, 128)

	// Capabilities: CLIENT_PROTOCOL_41 (0x200) | CLIENT_SECURE_CONNECTION (0x8000)
	// Add CLIENT_SSL if we upgraded
	caps := uint32(0x00008200)
	if p.shadowTLSConfig != nil {
		caps |= clientSSL
	}
	authPacket = append(authPacket, byte(caps), byte(caps>>8), byte(caps>>16), byte(caps>>24))

	// Max packet size
	authPacket = append(authPacket, 0x00, 0x00, 0x00, 0x01) // 16MB

	// Charset: utf8 (33)
	authPacket = append(authPacket, 33)

	// Reserved: 23 zero bytes
	authPacket = append(authPacket, make([]byte, 23)...)

	// Username + null terminator
	authPacket = append(authPacket, []byte(username)...)
	authPacket = append(authPacket, 0)

	// Auth response (password hash)
	if password == "" {
		// No password
		authPacket = append(authPacket, 0)
	} else {
		// MySQL native password: SHA1(password) XOR SHA1(scramble + SHA1(SHA1(password)))
		authResponse := mysqlNativePassword(password, scramble)
		authPacket = append(authPacket, byte(len(authResponse)))
		authPacket = append(authPacket, authResponse...)
	}

	// Build the full packet with header
	packetLen := len(authPacket)
	fullPacket := make([]byte, 4+packetLen)
	fullPacket[0] = byte(packetLen)
	fullPacket[1] = byte(packetLen >> 8)
	fullPacket[2] = byte(packetLen >> 16)
	fullPacket[3] = seqNum
	copy(fullPacket[4:], authPacket)

	// Step 4: Send auth packet to shadow
	if _, err := authConn.Write(fullPacket); err != nil {
		return nil, fmt.Errorf("failed to send auth to shadow: %w", err)
	}
	debugf(p.config, "authenticateWithShadow: sent auth packet to shadow (user=%s, hasPassword=%v, tls=%v)", username, password != "", p.shadowTLSConfig != nil)

	// Step 5: Read auth response from shadow
	response, err := readMySQLPacket(authConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read auth response from shadow: %w", err)
	}

	// Check response type (offset 4 is the packet type)
	if len(response) > 4 {
		respType := response[4]
		switch respType {
		case 0xFF:
			errMsg := "unknown error"
			if len(response) > 13 {
				errMsg = string(response[13:])
			}
			authFailures.WithLabelValues("shadow").Inc()
			return nil, fmt.Errorf("shadow auth failed: %s", errMsg)
		case 0x00:
			debugf(p.config, "authenticateWithShadow: shadow auth successful (OK packet, tls=%v)", p.shadowTLSConfig != nil)
		case 0xFE:
			authFailures.WithLabelValues("shadow").Inc()
			return nil, fmt.Errorf("shadow requested auth switch, not supported")
		}
	}

	return authConn, nil
}

// proxyWithoutShadow does simple bidirectional proxy when shadow is unavailable
func (p *TCPProxy) proxyWithoutShadow(client, primary net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Client -> Primary
	go func() {
		defer wg.Done()
		io.Copy(primary, client)
	}()

	// Primary -> Client
	go func() {
		defer wg.Done()
		io.Copy(client, primary)
	}()

	wg.Wait()
}

// proxyWithMirrorWorker proxies to primary and mirrors to shadow using a ShadowWorker.
// Each client gets exactly one shadow connection with a bounded queue for backpressure.
// IMPORTANT: This function reads complete MySQL packets and uses protocol-aware response
// reading to measure accurate per-query timing for the primary cluster.
func (p *TCPProxy) proxyWithMirrorWorker(client, primary net.Conn, shadowWorker *ShadowWorker, logger *QueryLogger) {
	clientReader := NewMySQLPacketReader(client)
	primaryReader := NewMySQLPacketReader(primary)
	clientAddr := client.RemoteAddr().String()

	// Single goroutine handles the full request-response cycle for accurate timing.
	// MySQL is strictly request-response over a single connection, so this is safe.
	for {
		// Read a complete MySQL packet from client (handles TCP fragmentation)
		packet, err := clientReader.ReadMultiPacket()
		if err != nil {
			if err != io.EOF {
				log.Printf("Client read error: %v", err)
			}
			return
		}

		// Create QueryRequest with correlation metadata
		req := NewQueryRequest(packet, clientAddr)

		// Track packet count
		mysqlPackets.WithLabelValues("primary").Inc()

		// Extract command and track it
		var isQuery bool
		if cmd, ok := getMySQLCommand(packet); ok {
			cmdName := getMySQLCommandName(cmd)
			mysqlCommands.WithLabelValues("primary", cmdName).Inc()
			isQuery = isCountableCommand(cmd)
			if isQuery {
				queryTotal.WithLabelValues("primary").Inc()
			}
		}

		// Start timing before sending to primary
		primaryStart := time.Now()

		// Send complete packet to primary
		nWritten, err := primary.Write(packet)
		if err != nil {
			log.Printf("Primary write error: %v", err)
			queryErrors.WithLabelValues("primary").Inc()
			// Log the failure
			p.logPrimaryExecution(logger, req, int64(nWritten), 0, time.Since(primaryStart), err)
			return
		}
		bytesTotal.WithLabelValues("primary", "sent").Add(float64(nWritten))

		// Determine command type for dispatch decisions
		cmd, _ := getMySQLCommand(packet)

		// Send QueryRequest to shadow worker IMMEDIATELY after writing to primary.
		// This ensures shadow executes in parallel with primary (not sequentially
		// after primary completes). The shadow worker processes the queue
		// asynchronously on its own goroutine and connection.
		//
		// Skip only COM_QUIT: the shadow worker manages its own connection lifecycle
		// and sending COM_QUIT would kill the shadow connection prematurely while
		// other packets may still be queued.
		//
		// COM_STMT_CLOSE and COM_STMT_SEND_LONG_DATA MUST still be mirrored to keep
		// prepared statement lifecycle in sync. Without COM_STMT_SEND_LONG_DATA the
		// shadow lacks parameter data for the subsequent COM_STMT_EXECUTE; without
		// COM_STMT_CLOSE the shadow leaks open statements. The shadow worker already
		// handles these correctly (writes the packet, skips reading a response).
		if cmd != comQuit {
			// Check if the query filter allows this request to be shadowed.
			// The filter only applies to COM_QUERY; other commands (COM_INIT_DB,
			// COM_STMT_PREPARE, etc.) always pass through to keep shadow in sync.
			allowed, filterReason := true, ""
			if p.queryFilter != nil {
				allowed, filterReason = p.queryFilter.Allow(req)
			}
			if !allowed {
				shadowFilteredTotal.WithLabelValues(filterReason).Inc()
				debugf(p.config, "Query filtered from shadow (%s): %s %s", filterReason, req.Command, extractQueryPreview(packet, 100))
				p.logFilteredShadow(logger, req, filterReason)
			} else {
				shadowReq := QueryRequest{
					ID:         req.ID, // Same ID for correlation!
					Packet:     make([]byte, len(packet)),
					QueryText:  req.QueryText,
					QueryHash:  req.QueryHash,
					Command:    req.Command,
					ClientAddr: req.ClientAddr,
					ReceivedAt: req.ReceivedAt,
				}
				copy(shadowReq.Packet, packet)

				if !shadowWorker.Send(shadowReq) {
					debugf(p.config, "Shadow queue full, dropping mirrored command: %s", getMySQLCommandName(cmd))
				}
			}
		}

		// No-response commands (COM_QUIT, COM_STMT_CLOSE, COM_STMT_SEND_LONG_DATA)
		// do not produce a server response. Skip the read and log immediately.
		if isNoResponseCommand(cmd) {
			primaryDuration := time.Since(primaryStart)
			p.logPrimaryExecution(logger, req, int64(nWritten), 0, primaryDuration, nil)
			// COM_QUIT means the client is disconnecting — exit the loop.
			if cmd == comQuit {
				return
			}
			continue
		}

		// Read response from primary and forward to client.
		// Use command-aware dispatch: simple commands (COM_INIT_DB, COM_PING) always
		// return a single OK/ERR packet, so we bypass the full result-set parser to
		// avoid hangs when the server sets unexpected status flags.
		var bytesRecv int64
		var readErr error
		if isSimpleResponseCommand(cmd) {
			bytesRecv, readErr = primaryReader.ReadAndForwardSimpleResponse(client)
		} else {
			bytesRecv, readErr = primaryReader.ReadAndForwardResponse(client)
		}
		primaryDuration := time.Since(primaryStart)

		if readErr != nil {
			log.Printf("Primary read/forward error: %v", readErr)
			queryErrors.WithLabelValues("primary").Inc()
			p.logPrimaryExecution(logger, req, int64(nWritten), bytesRecv, primaryDuration, readErr)
			return
		}

		bytesTotal.WithLabelValues("primary", "received").Add(float64(bytesRecv))
		queryDuration.WithLabelValues("primary").Observe(primaryDuration.Seconds())

		// Log successful primary execution
		p.logPrimaryExecution(logger, req, int64(nWritten), bytesRecv, primaryDuration, nil)
	}
}

// logPrimaryExecution logs a primary query execution to the QueryLogger if configured
func (p *TCPProxy) logPrimaryExecution(logger *QueryLogger, req QueryRequest, bytesSent, bytesRecv int64, duration time.Duration, err error) {
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
		DurationMs: float64(duration.Nanoseconds()) / 1e6, // Convert to milliseconds with precision
		BytesSent:  bytesSent,
		BytesRecv:  bytesRecv,
		Success:    err == nil,
		Error:      errorString(err),
		ClientAddr: req.ClientAddr,
	})
}

// logFilteredShadow logs a shadow entry for a query that was filtered (not mirrored).
// This ensures every primary log entry has a corresponding shadow entry in GCS,
// making it easy to identify filtered queries in BigQuery analysis.
func (p *TCPProxy) logFilteredShadow(logger *QueryLogger, req QueryRequest, filterReason string) {
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

// Start starts the TCP proxy server
func (p *TCPProxy) Start(ctx context.Context, logger *QueryLogger) error {
	listener, err := net.Listen("tcp", p.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to start listener: %w", err)
	}
	defer listener.Close()

	if p.tlsConfig != nil {
		log.Printf("Shadow proxy listening on %s (TLS termination enabled)", p.config.ListenAddr)
	} else {
		log.Printf("Shadow proxy listening on %s (plain TCP)", p.config.ListenAddr)
	}
	log.Printf("Primary: %s, Shadow: %s", p.primaryAddr, p.shadowAddr)
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
