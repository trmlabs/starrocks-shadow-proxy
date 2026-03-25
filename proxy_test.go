package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Mock servers used by proxy integration tests ---

// MockTCPServer creates a mock TCP server for testing
type MockTCPServer struct {
	listener net.Listener
	received [][]byte
	response []byte
}

func NewMockTCPServer(response []byte) (*MockTCPServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	server := &MockTCPServer{
		listener: listener,
		received: make([][]byte, 0),
		response: response,
	}

	go server.serve()
	return server, nil
}

func (s *MockTCPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *MockTCPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		s.received = append(s.received, append([]byte(nil), buf[:n]...))
		if s.response != nil {
			conn.Write(s.response)
		}
	}
}

func (s *MockTCPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *MockTCPServer) Close() {
	s.listener.Close()
}

// MockMySQLServer creates a mock MySQL server that implements the MySQL handshake protocol
type MockMySQLServer struct {
	listener     net.Listener
	username     string
	password     string
	scramble     []byte
	received     [][]byte
	response     []byte
	authSuccess  bool
	authAttempts int
	mu           sync.Mutex
}

func NewMockMySQLServer(username, password string, response []byte) (*MockMySQLServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	// Generate random scramble (20 bytes) without null bytes
	// The production extractScrambleFromHandshake skips null bytes, so we avoid them in tests
	scramble := make([]byte, 20)
	rand.Read(scramble)
	for i := range scramble {
		if scramble[i] == 0 {
			scramble[i] = 0x42 // Replace null bytes with a non-null value
		}
	}

	server := &MockMySQLServer{
		listener:    listener,
		username:    username,
		password:    password,
		scramble:    scramble,
		received:    make([][]byte, 0),
		response:    response,
		authSuccess: false,
	}

	go server.serve()
	return server, nil
}

func (s *MockMySQLServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *MockMySQLServer) handleConn(conn net.Conn) {
	defer conn.Close()

	// Step 1: Send MySQL handshake packet
	handshake := s.buildHandshakePacket()
	if _, err := conn.Write(handshake); err != nil {
		return
	}

	// Step 2: Read auth packet from client
	authPacket := make([]byte, 4096)
	n, err := conn.Read(authPacket)
	if err != nil {
		return
	}

	s.mu.Lock()
	s.authAttempts++
	s.mu.Unlock()

	// Step 3: Verify authentication
	if s.verifyAuth(authPacket[:n]) {
		// Send OK packet
		okPacket := s.buildOKPacket()
		conn.Write(okPacket)
		s.mu.Lock()
		s.authSuccess = true
		s.mu.Unlock()
	} else {
		// Send ERROR packet
		errPacket := s.buildErrorPacket("Access denied for user")
		conn.Write(errPacket)
		return
	}

	// Step 4: Handle subsequent data (echo mode)
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.received = append(s.received, append([]byte(nil), buf[:n]...))
		s.mu.Unlock()
		if s.response != nil {
			conn.Write(s.response)
		}
	}
}

func (s *MockMySQLServer) buildHandshakePacket() []byte {
	payload := make([]byte, 0, 128)

	// Protocol version
	payload = append(payload, 10)

	// Server version (null-terminated)
	payload = append(payload, []byte("5.7.0-mock")...)
	payload = append(payload, 0)

	// Connection ID (4 bytes)
	payload = append(payload, 1, 0, 0, 0)

	// Scramble part 1 (8 bytes)
	payload = append(payload, s.scramble[:8]...)

	// Filler
	payload = append(payload, 0)

	// Capability flags lower (CLIENT_PROTOCOL_41 | CLIENT_SECURE_CONNECTION)
	payload = append(payload, 0x00, 0x82)

	// Charset (utf8 = 33)
	payload = append(payload, 33)

	// Status flags
	payload = append(payload, 0x02, 0x00)

	// Capability flags upper
	payload = append(payload, 0x00, 0x00)

	// Auth plugin data length (21 = 8 + 12 + 1 null)
	payload = append(payload, 21)

	// Reserved (10 bytes)
	payload = append(payload, make([]byte, 10)...)

	// Scramble part 2 (12 bytes + null terminator)
	payload = append(payload, s.scramble[8:20]...)
	payload = append(payload, 0)

	// Build full packet with header
	packet := make([]byte, 4+len(payload))
	packet[0] = byte(len(payload))
	packet[1] = byte(len(payload) >> 8)
	packet[2] = byte(len(payload) >> 16)
	packet[3] = 0 // sequence number
	copy(packet[4:], payload)

	return packet
}

func (s *MockMySQLServer) buildOKPacket() []byte {
	// OK packet: header (0x00) + affected rows + last insert id + status + warnings
	payload := []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	packet := make([]byte, 4+len(payload))
	packet[0] = byte(len(payload))
	packet[1] = 0
	packet[2] = 0
	packet[3] = 2 // sequence number
	copy(packet[4:], payload)
	return packet
}

func (s *MockMySQLServer) buildErrorPacket(message string) []byte {
	// Error packet: 0xFF + error code (2 bytes) + sql state marker + sql state (5 bytes) + message
	payload := make([]byte, 0, 64)
	payload = append(payload, 0xFF)               // Error marker
	payload = append(payload, 0x15, 0x04)         // Error code 1045 (access denied)
	payload = append(payload, '#')                // SQL state marker
	payload = append(payload, []byte("28000")...) // SQL state
	payload = append(payload, []byte(message)...) // Error message

	packet := make([]byte, 4+len(payload))
	packet[0] = byte(len(payload))
	packet[1] = byte(len(payload) >> 8)
	packet[2] = byte(len(payload) >> 16)
	packet[3] = 2 // sequence number
	copy(packet[4:], payload)
	return packet
}

func (s *MockMySQLServer) verifyAuth(packet []byte) bool {
	// Skip header (4 bytes)
	if len(packet) < 36 {
		return false
	}

	// Skip capabilities (4 bytes) + max packet size (4 bytes) + charset (1 byte) + reserved (23 bytes)
	pos := 4 + 4 + 4 + 1 + 23

	// Read username (null-terminated)
	usernameEnd := pos
	for usernameEnd < len(packet) && packet[usernameEnd] != 0 {
		usernameEnd++
	}
	username := string(packet[pos:usernameEnd])
	pos = usernameEnd + 1

	if username != s.username {
		return false
	}

	// If no password expected
	if s.password == "" {
		// Check that auth response length is 0
		if pos < len(packet) && packet[pos] == 0 {
			return true
		}
		return false
	}

	// Read auth response length
	if pos >= len(packet) {
		return false
	}
	authLen := int(packet[pos])
	pos++

	if authLen == 0 {
		return false // Password expected but none provided
	}

	if pos+authLen > len(packet) {
		return false
	}

	clientHash := packet[pos : pos+authLen]

	// Compute expected hash: SHA1(password) XOR SHA1(scramble + SHA1(SHA1(password)))
	expectedHash := computeMySQLNativePassword(s.password, s.scramble)

	if len(clientHash) != len(expectedHash) {
		return false
	}

	for i := range clientHash {
		if clientHash[i] != expectedHash[i] {
			return false
		}
	}

	return true
}

func computeMySQLNativePassword(password string, scramble []byte) []byte {
	// SHA1(password)
	crypt := sha1.New()
	crypt.Write([]byte(password))
	hash1 := crypt.Sum(nil)

	// SHA1(SHA1(password))
	crypt.Reset()
	crypt.Write(hash1)
	hash2 := crypt.Sum(nil)

	// SHA1(scramble + SHA1(SHA1(password)))
	crypt.Reset()
	crypt.Write(scramble)
	crypt.Write(hash2)
	hash3 := crypt.Sum(nil)

	// XOR
	result := make([]byte, len(hash1))
	for i := range hash1 {
		result[i] = hash1[i] ^ hash3[i]
	}

	return result
}

func (s *MockMySQLServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *MockMySQLServer) Close() {
	s.listener.Close()
}

func (s *MockMySQLServer) AuthSuccess() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authSuccess
}

func (s *MockMySQLServer) AuthAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authAttempts
}

func (s *MockMySQLServer) Received() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

// SlowMockTCPServer is a mock server with configurable delay
type SlowMockTCPServer struct {
	listener net.Listener
	response []byte
	delay    time.Duration
}

func NewSlowMockTCPServer(response []byte, delay time.Duration) (*SlowMockTCPServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	server := &SlowMockTCPServer{
		listener: listener,
		response: response,
		delay:    delay,
	}

	go server.serve()
	return server, nil
}

func (s *SlowMockTCPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *SlowMockTCPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 4096)
	for {
		_, err := conn.Read(buf)
		if err != nil {
			return
		}
		// Introduce delay before responding
		time.Sleep(s.delay)
		if s.response != nil {
			conn.Write(s.response)
		}
	}
}

func (s *SlowMockTCPServer) Addr() string {
	return s.listener.Addr().String()
}

func (s *SlowMockTCPServer) Close() {
	s.listener.Close()
}

// --- Test helper functions ---

// buildMySQLPacket wraps payload in a MySQL packet with header
func buildMySQLPacket(payload []byte, seqNum byte) []byte {
	packet := make([]byte, 4+len(payload))
	packet[0] = byte(len(payload))
	packet[1] = byte(len(payload) >> 8)
	packet[2] = byte(len(payload) >> 16)
	packet[3] = seqNum
	copy(packet[4:], payload)
	return packet
}

// buildMySQLQueryPacket builds a COM_QUERY packet with the given SQL
func buildMySQLQueryPacket(sql string, seqNum byte) []byte {
	// COM_QUERY format: command byte (0x03) + query string
	payload := make([]byte, 1+len(sql))
	payload[0] = comQuery // 0x03
	copy(payload[1:], sql)
	return buildMySQLPacket(payload, seqNum)
}

// buildMySQLOKPacket builds a MySQL OK packet for testing
func buildMySQLOKPacket(seqNum byte) []byte {
	payload := []byte{
		0x00,       // OK marker
		0x00,       // affected_rows = 0
		0x00,       // last_insert_id = 0
		0x00, 0x00, // status flags
		0x00, 0x00, // warnings
	}
	return buildMySQLPacket(payload, seqNum)
}

// buildTestAuthPacket builds a MySQL auth packet for testing
func buildTestAuthPacket(username, password string, handshakePacket []byte) []byte {
	// Extract scramble from handshake
	scramble := extractScrambleFromHandshake(handshakePacket)
	if scramble == nil {
		// Fallback scramble if extraction fails
		scramble = make([]byte, 20)
	}

	authPayload := make([]byte, 0, 128)

	// Capabilities: CLIENT_PROTOCOL_41 (0x200) | CLIENT_SECURE_CONNECTION (0x8000)
	caps := uint32(0x00008200)
	authPayload = append(authPayload, byte(caps), byte(caps>>8), byte(caps>>16), byte(caps>>24))

	// Max packet size
	authPayload = append(authPayload, 0x00, 0x00, 0x00, 0x01)

	// Charset
	authPayload = append(authPayload, 33)

	// Reserved (23 bytes)
	authPayload = append(authPayload, make([]byte, 23)...)

	// Username + null
	authPayload = append(authPayload, []byte(username)...)
	authPayload = append(authPayload, 0)

	// Password hash
	if password == "" {
		authPayload = append(authPayload, 0)
	} else {
		hash := mysqlNativePassword(password, scramble)
		authPayload = append(authPayload, byte(len(hash)))
		authPayload = append(authPayload, hash...)
	}

	// Build packet with header
	packet := make([]byte, 4+len(authPayload))
	packet[0] = byte(len(authPayload))
	packet[1] = byte(len(authPayload) >> 8)
	packet[2] = byte(len(authPayload) >> 16)
	packet[3] = 1 // sequence number
	copy(packet[4:], authPayload)

	return packet
}

// --- Proxy tests ---

func TestNewTCPProxy(t *testing.T) {
	config := &Config{
		ListenAddr:  ":3306",
		PrimaryHost: "primary.example.com",
		PrimaryPort: "9030",
		ShadowHost:  "shadow.example.com",
		ShadowPort:  "9030",
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	if proxy.primaryAddr != "primary.example.com:9030" {
		t.Errorf("Expected primaryAddr 'primary.example.com:9030', got '%s'", proxy.primaryAddr)
	}
	if proxy.shadowAddr != "shadow.example.com:9030" {
		t.Errorf("Expected shadowAddr 'shadow.example.com:9030', got '%s'", proxy.shadowAddr)
	}
}

// TestTLSConfigLoading tests that TLS config loads correctly
func TestTLSConfigLoading(t *testing.T) {
	// Test with TLS disabled
	config := &Config{
		PrimaryHost: "localhost",
		PrimaryPort: "9030",
		ShadowHost:  "localhost",
		ShadowPort:  "9030",
		TLSEnabled:  false,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	if proxy.tlsConfig != nil {
		t.Error("Expected tlsConfig to be nil when TLS disabled")
	}
}

// TestTLSConfigLoadingWithMissingCert tests error handling for missing certs
func TestTLSConfigLoadingWithMissingCert(t *testing.T) {
	config := &Config{
		PrimaryHost: "localhost",
		PrimaryPort: "9030",
		ShadowHost:  "localhost",
		ShadowPort:  "9030",
		TLSEnabled:  true,
		TLSCertFile: "/nonexistent/cert.pem",
		TLSKeyFile:  "/nonexistent/key.pem",
	}

	_, err := NewTCPProxy(config)
	if err == nil {
		t.Error("Expected error when TLS cert files don't exist")
	}
}

// TestProxyWithMockServers tests the proxy with mock MySQL servers
func TestProxyWithMockServers(t *testing.T) {
	// Create mock primary MySQL server with credentials
	primaryResponse := buildMySQLOKPacket(2)
	primaryServer, err := NewMockMySQLServer("root", "primarypass", primaryResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	// Create mock shadow MySQL server with credentials
	shadowResponse := buildMySQLOKPacket(2)
	shadowServer, err := NewMockMySQLServer("shadowuser", "shadowpass", shadowResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	// Parse addresses
	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "primarypass",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "shadowuser",
		ShadowPassword:     "shadowpass",
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake from proxy: %v", err)
	}

	authPacket := buildTestAuthPacket("root", "primarypass", buf[:n])
	_, err = proxyConn.Write(authPacket)
	if err != nil {
		t.Fatalf("Failed to write auth to proxy: %v", err)
	}

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read auth response from proxy: %v", err)
	}

	if len(buf) > 4 && buf[4] != 0x00 {
		t.Fatalf("Expected OK packet (0x00), got 0x%02X", buf[4])
	}

	queryPacket := buildMySQLQueryPacket("SELECT 1", 0)
	_, err = proxyConn.Write(queryPacket)
	if err != nil {
		t.Fatalf("Failed to write query to proxy: %v", err)
	}

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read from proxy: %v", err)
	}

	if n < 5 || buf[4] != 0x00 {
		t.Errorf("Expected MySQL OK packet (0x00), got 0x%02X (len=%d)", buf[4], n)
	}

	// Allow time for async shadow mirroring to complete (CI runners can be slow)
	for i := 0; i < 50; i++ {
		if len(shadowServer.Received()) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !shadowServer.AuthSuccess() {
		t.Error("Shadow server authentication failed - proxy did not authenticate with shadow")
	}

	if len(shadowServer.Received()) == 0 {
		t.Error("Shadow server did not receive mirrored data")
	}
}

// TestShadowAuthenticationWithCorrectCredentials verifies shadow authentication works with correct credentials
func TestShadowAuthenticationWithCorrectCredentials(t *testing.T) {
	primaryServer, err := NewMockMySQLServer("root", "primarypass", []byte("PRIMARY_OK"))
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowServer, err := NewMockMySQLServer("shadowuser", "shadowpass", []byte("SHADOW_OK"))
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "primarypass",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "shadowuser",
		ShadowPassword:     "shadowpass",
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}

	authPacket := buildTestAuthPacket("root", "primarypass", buf[:n])
	proxyConn.Write(authPacket)

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	queryPacket := buildMySQLQueryPacket("SELECT 1", 0)
	proxyConn.Write(queryPacket)

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	time.Sleep(300 * time.Millisecond)

	if !shadowServer.AuthSuccess() {
		t.Error("Shadow server authentication FAILED - expected success with correct credentials")
	}

	if shadowServer.AuthAttempts() == 0 {
		t.Error("Shadow server received no authentication attempts")
	}
}

// TestShadowAuthenticationWithWrongCredentials verifies shadow auth fails with wrong credentials
func TestShadowAuthenticationWithWrongCredentials(t *testing.T) {
	primaryServer, err := NewMockMySQLServer("root", "primarypass", []byte("PRIMARY_OK"))
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowServer, err := NewMockMySQLServer("shadowuser", "correctpassword", []byte("SHADOW_OK"))
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "primarypass",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "shadowuser",
		ShadowPassword:     "wrongpassword",
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}

	authPacket := buildTestAuthPacket("root", "primarypass", buf[:n])
	proxyConn.Write(authPacket)

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	time.Sleep(300 * time.Millisecond)

	if shadowServer.AuthSuccess() {
		t.Error("Shadow server authentication SUCCEEDED - expected failure with wrong credentials")
	}

	if shadowServer.AuthAttempts() == 0 {
		t.Error("Shadow server received no authentication attempts")
	}
}

// TestPrimaryDown verifies behavior when primary server is unavailable
func TestPrimaryDown(t *testing.T) {
	shadowServer, err := NewMockTCPServer([]byte("SHADOW_RESPONSE"))
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        "127.0.0.1",
		PrimaryPort:        "59999",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	done := make(chan bool)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- true
			return
		}
		proxy.handleConnection(conn, nil)
		done <- true
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.Write([]byte("TEST_QUERY"))

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	_, err = proxyConn.Read(buf)

	if err == nil {
		t.Error("Expected error when primary is down, but got successful read")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Handler did not complete in time")
	}
}

// TestShadowDownDoesNotAffectPrimary verifies that shadow failure doesn't impact primary
func TestShadowDownDoesNotAffectPrimary(t *testing.T) {
	primaryResponse := []byte("PRIMARY_OK")
	primaryServer, err := NewMockMySQLServer("root", "primarypass", primaryResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "primarypass",
		ShadowHost:         "127.0.0.1",
		ShadowPort:         "59998",
		ShadowUser:         "root",
		ShadowPassword:     "shadowpass",
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))

	handshake, err := readMySQLPacket(proxyConn)
	if err != nil {
		t.Fatalf("Failed to read handshake from proxy: %v", err)
	}
	if len(handshake) < 5 {
		t.Fatalf("Handshake too short: %d bytes", len(handshake))
	}

	authPacket := buildTestAuthPacket("root", "primarypass", handshake)
	if _, err := proxyConn.Write(authPacket); err != nil {
		t.Fatalf("Failed to send auth packet: %v", err)
	}

	authResp, err := readMySQLPacket(proxyConn)
	if err != nil {
		t.Fatalf("Failed to read auth response: %v", err)
	}
	if len(authResp) > 4 && authResp[4] == 0xFF {
		t.Fatalf("Auth failed: error packet received")
	}

	query := buildMySQLQueryPacket("SELECT 1", 0)
	if _, err := proxyConn.Write(query); err != nil {
		t.Fatalf("Failed to send query: %v", err)
	}

	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read response from proxy: %v", err)
	}

	if n == 0 {
		t.Error("Expected response from primary, got empty response")
	}
	if string(buf[:n]) != "PRIMARY_OK" {
		t.Logf("Got response: %q (length=%d)", string(buf[:n]), n)
	}
}

// TestConcurrentConnections verifies proxy handles multiple simultaneous connections
func TestConcurrentConnections(t *testing.T) {
	primaryResponse := buildMySQLOKPacket(2)
	primaryServer, err := NewMockMySQLServer("root", "primarypass", primaryResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowResponse := buildMySQLOKPacket(2)
	shadowServer, err := NewMockMySQLServer("shadowuser", "shadowpass", shadowResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "primarypass",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "shadowuser",
		ShadowPassword:     "shadowpass",
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go proxy.handleConnection(conn, nil)
		}
	}()

	numClients := 10
	var wg sync.WaitGroup
	errors := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				errors <- fmt.Errorf("client %d: connect failed: %v", clientID, err)
				return
			}
			defer conn.Close()

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 4096)
			n, err := conn.Read(buf)
			if err != nil {
				errors <- fmt.Errorf("client %d: handshake read failed: %v", clientID, err)
				return
			}

			authPacket := buildTestAuthPacket("root", "primarypass", buf[:n])
			_, err = conn.Write(authPacket)
			if err != nil {
				errors <- fmt.Errorf("client %d: auth write failed: %v", clientID, err)
				return
			}

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err = conn.Read(buf)
			if err != nil {
				errors <- fmt.Errorf("client %d: auth response failed: %v", clientID, err)
				return
			}

			query := fmt.Sprintf("SELECT %d", clientID)
			queryPacket := buildMySQLQueryPacket(query, 0)
			_, err = conn.Write(queryPacket)
			if err != nil {
				errors <- fmt.Errorf("client %d: write failed: %v", clientID, err)
				return
			}

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err = conn.Read(buf)
			if err != nil {
				errors <- fmt.Errorf("client %d: read failed: %v", clientID, err)
				return
			}

			if n < 5 || buf[4] != 0x00 {
				errors <- fmt.Errorf("client %d: expected MySQL OK packet, got %d bytes with marker 0x%02X", clientID, n, buf[4])
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}

	time.Sleep(200 * time.Millisecond)

	if !shadowServer.AuthSuccess() {
		t.Error("Shadow server authentication failed")
	}
}

// TestConnectionTimeout verifies proxy handles slow connections gracefully
func TestConnectionTimeout(t *testing.T) {
	primaryResponse := buildMySQLOKPacket(2)
	primaryServer, err := NewMockMySQLServer("root", "", primaryResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowResponse := buildMySQLOKPacket(2)
	shadowServer, err := NewMockMySQLServer("shadowuser", "", shadowResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "shadowuser",
		ShadowPassword:     "",
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}

	authPacket := buildTestAuthPacket("root", "", buf[:n])
	proxyConn.Write(authPacket)

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read auth response: %v", err)
	}

	queryPacket := buildMySQLQueryPacket("SELECT 1", 0)
	proxyConn.Write(queryPacket)

	proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read from proxy: %v", err)
	}

	if n < 5 || buf[4] != 0x00 {
		t.Errorf("Expected MySQL OK packet (0x00), got %d bytes with marker 0x%02X", n, buf[4])
	}
}

// TestLargePayload verifies proxy handles large data transfers
func TestLargePayload(t *testing.T) {
	largeResponse := make([]byte, 1024*1024)
	for i := range largeResponse {
		largeResponse[i] = byte(i % 256)
	}

	primaryServer, err := NewMockMySQLServer("root", "", largeResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowServer, err := NewMockMySQLServer("shadowuser", "", []byte("SHADOW"))
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "shadowuser",
		ShadowPassword:     "",
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}

	authPacket := buildTestAuthPacket("root", "", buf[:n])
	proxyConn.Write(authPacket)

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read auth response: %v", err)
	}

	largeSQL := strings.Repeat("X", 64*1024)
	queryPacket := buildMySQLQueryPacket(largeSQL, 0)
	proxyConn.Write(queryPacket)

	proxyConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	received := make([]byte, 0)
	readBuf := make([]byte, 32*1024)

	for len(received) < len(largeResponse) {
		n, err := proxyConn.Read(readBuf)
		if err != nil {
			break
		}
		received = append(received, readBuf[:n]...)
	}

	if len(received) < len(largeResponse)/2 {
		t.Errorf("Expected at least %d bytes, got %d", len(largeResponse)/2, len(received))
	}
}

// buildMySQLOKPacketWithFlags builds a MySQL OK packet with custom status flags
func buildMySQLOKPacketWithFlags(seqNum byte, statusFlags uint16) []byte {
	payload := []byte{
		0x00,                   // OK marker
		0x00,                   // affected_rows = 0
		0x00,                   // last_insert_id = 0
		byte(statusFlags),      // status flags low byte
		byte(statusFlags >> 8), // status flags high byte
		0x00, 0x00,             // warnings
	}
	return buildMySQLPacket(payload, seqNum)
}

// buildMySQLInitDBPacket builds a COM_INIT_DB (USE database) packet
func buildMySQLInitDBPacket(database string, seqNum byte) []byte {
	payload := make([]byte, 1+len(database))
	payload[0] = comInitDB // 0x02
	copy(payload[1:], database)
	return buildMySQLPacket(payload, seqNum)
}

// MockMySQLServerWithCommandHandler is a mock MySQL server that can return different
// responses based on the MySQL command type. This is needed to test COM_INIT_DB
// with SERVER_MORE_RESULTS_EXISTS.
type MockMySQLServerWithCommandHandler struct {
	listener        net.Listener
	username        string
	password        string
	scramble        []byte
	defaultResponse []byte
	initDBResponse  []byte // Response specifically for COM_INIT_DB
	received        [][]byte
	mu              sync.Mutex
}

func NewMockMySQLServerWithCommandHandler(username, password string, defaultResponse, initDBResponse []byte) (*MockMySQLServerWithCommandHandler, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	scramble := make([]byte, 20)
	rand.Read(scramble)
	for i := range scramble {
		if scramble[i] == 0 {
			scramble[i] = 0x42
		}
	}

	server := &MockMySQLServerWithCommandHandler{
		listener:        listener,
		username:        username,
		password:        password,
		scramble:        scramble,
		defaultResponse: defaultResponse,
		initDBResponse:  initDBResponse,
		received:        make([][]byte, 0),
	}

	go server.serve()
	return server, nil
}

func (s *MockMySQLServerWithCommandHandler) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *MockMySQLServerWithCommandHandler) handleConn(conn net.Conn) {
	defer conn.Close()

	// Send handshake
	handshake := s.buildHandshakePacket()
	if _, err := conn.Write(handshake); err != nil {
		return
	}

	// Read and verify auth
	authBuf := make([]byte, 4096)
	_, err := conn.Read(authBuf)
	if err != nil {
		return
	}

	// Send auth OK
	okPacket := buildMySQLOKPacket(2)
	conn.Write(okPacket)

	// Handle commands
	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}

		s.mu.Lock()
		s.received = append(s.received, append([]byte(nil), buf[:n]...))
		s.mu.Unlock()

		// Check command type
		if n >= 5 && buf[4] == comInitDB && s.initDBResponse != nil {
			conn.Write(s.initDBResponse)
		} else if s.defaultResponse != nil {
			conn.Write(s.defaultResponse)
		}
	}
}

func (s *MockMySQLServerWithCommandHandler) buildHandshakePacket() []byte {
	payload := make([]byte, 0, 128)
	payload = append(payload, 10)
	payload = append(payload, []byte("5.1.0-starrocks-mock")...)
	payload = append(payload, 0)
	payload = append(payload, 1, 0, 0, 0)
	payload = append(payload, s.scramble[:8]...)
	payload = append(payload, 0)
	payload = append(payload, 0x00, 0x82)
	payload = append(payload, 33)
	payload = append(payload, 0x02, 0x00)
	payload = append(payload, 0x00, 0x00)
	payload = append(payload, 21)
	payload = append(payload, make([]byte, 10)...)
	payload = append(payload, s.scramble[8:20]...)
	payload = append(payload, 0)

	packet := make([]byte, 4+len(payload))
	packet[0] = byte(len(payload))
	packet[1] = byte(len(payload) >> 8)
	packet[2] = byte(len(payload) >> 16)
	packet[3] = 0
	copy(packet[4:], payload)
	return packet
}

func (s *MockMySQLServerWithCommandHandler) Addr() string {
	return s.listener.Addr().String()
}

func (s *MockMySQLServerWithCommandHandler) Close() {
	s.listener.Close()
}

func (s *MockMySQLServerWithCommandHandler) Received() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

// TestCOMInitDBWithMoreResultsFlag tests that the proxy correctly handles
// StarRocks returning an OK packet with SERVER_MORE_RESULTS_EXISTS for
// USE catalog.database via COM_INIT_DB.
// Before the fix, the proxy would hang forever. After the fix, it returns immediately.
func TestCOMInitDBWithMoreResultsFlag(t *testing.T) {
	// Primary returns OK with SERVER_MORE_RESULTS_EXISTS for COM_INIT_DB
	// (simulating StarRocks USE catalog.database behavior)
	// STATUS = 0x000A = AUTOCOMMIT (0x0002) | MORE_RESULTS (0x0008)
	initDBResponse := buildMySQLOKPacketWithFlags(1, 0x000A)
	queryResponse := buildMySQLOKPacket(1)

	primaryServer, err := NewMockMySQLServerWithCommandHandler("root", "", queryResponse, initDBResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	// Shadow also returns OK with MORE_RESULTS for COM_INIT_DB
	shadowServer, err := NewMockMySQLServerWithCommandHandler("root", "", queryResponse, initDBResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "root",
		ShadowPassword:     "",
		ShadowQueueSize:    100,
		ShadowReadTimeout:  5 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	// Connect to proxy
	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	// Read handshake
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}

	// Send auth
	authPacket := buildTestAuthPacket("root", "", buf[:n])
	proxyConn.Write(authPacket)

	// Read auth response
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read auth response: %v", err)
	}

	// Send COM_INIT_DB (the command that was hanging before the fix)
	initDBPacket := buildMySQLInitDBPacket("analytics_catalog.my_database", 0)
	_, err = proxyConn.Write(initDBPacket)
	if err != nil {
		t.Fatalf("Failed to write COM_INIT_DB: %v", err)
	}

	// Read response — this MUST NOT hang
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("COM_INIT_DB response read failed (proxy hung?): %v", err)
	}

	// Verify we got an OK packet back
	if n < 5 {
		t.Fatalf("Response too short: %d bytes", n)
	}
	if buf[4] != 0x00 {
		t.Errorf("Expected OK packet (0x00), got 0x%02X", buf[4])
	}

	// Verify proxy still works: send a normal query after COM_INIT_DB
	queryPacket := buildMySQLQueryPacket("SELECT 1", 0)
	_, err = proxyConn.Write(queryPacket)
	if err != nil {
		t.Fatalf("Failed to write query after COM_INIT_DB: %v", err)
	}

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Query after COM_INIT_DB failed: %v", err)
	}
	if n < 5 || buf[4] != 0x00 {
		t.Errorf("Expected OK for query after COM_INIT_DB, got 0x%02X", buf[4])
	}

	// Wait for shadow to process
	time.Sleep(300 * time.Millisecond)

	// Verify shadow received the COM_INIT_DB
	shadowReceived := shadowServer.Received()
	foundInitDB := false
	for _, pkt := range shadowReceived {
		if len(pkt) >= 5 && pkt[4] == comInitDB {
			foundInitDB = true
			// Verify the database name was forwarded
			dbName := string(pkt[5:])
			if dbName != "analytics_catalog.my_database" {
				t.Errorf("Shadow received wrong database: %q", dbName)
			}
			break
		}
	}
	if !foundInitDB {
		t.Error("Shadow did not receive COM_INIT_DB — mirroring may be broken")
	}
}

// TestCOMQueryUSEWithMoreResultsFlag tests the real-world scenario:
// MySQL CLI sends "use analytics_catalog.my_database" as COM_QUERY (0x03),
// and StarRocks returns OK with SERVER_MORE_RESULTS_EXISTS.
func TestCOMQueryUSEWithMoreResultsFlag(t *testing.T) {
	// Primary returns OK with SERVER_MORE_RESULTS_EXISTS for any COM_QUERY containing "use"
	// STATUS = 0x000A = AUTOCOMMIT (0x0002) | MORE_RESULTS (0x0008)
	useResponse := buildMySQLOKPacketWithFlags(1, 0x000A)
	queryResponse := buildMySQLOKPacket(1)

	primaryServer, err := NewMockMySQLServerWithCommandHandler("root", "", queryResponse, useResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowServer, err := NewMockMySQLServerWithCommandHandler("root", "", queryResponse, useResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "root",
		ShadowPassword:     "",
		ShadowQueueSize:    100,
		ShadowReadTimeout:  5 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	// Handshake
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}

	authPacket := buildTestAuthPacket("root", "", buf[:n])
	proxyConn.Write(authPacket)

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read auth response: %v", err)
	}

	// Send "use analytics_catalog.my_database" as COM_QUERY (exactly what MySQL CLI does)
	usePacket := buildMySQLQueryPacket("use analytics_catalog.my_database", 0)
	_, err = proxyConn.Write(usePacket)
	if err != nil {
		t.Fatalf("Failed to write USE query: %v", err)
	}

	// Read response — this MUST NOT hang
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("COM_QUERY 'use ...' response read failed (proxy hung?): %v", err)
	}

	if n < 5 {
		t.Fatalf("Response too short: %d bytes", n)
	}
	if buf[4] != 0x00 {
		t.Errorf("Expected OK packet (0x00), got 0x%02X", buf[4])
	}

	// Verify proxy still works after USE
	queryPacket := buildMySQLQueryPacket("SELECT 1", 0)
	_, err = proxyConn.Write(queryPacket)
	if err != nil {
		t.Fatalf("Failed to write query after USE: %v", err)
	}

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Query after USE failed: %v", err)
	}
	if n < 5 || buf[4] != 0x00 {
		t.Errorf("Expected OK for query after USE, got 0x%02X", buf[4])
	}

	time.Sleep(300 * time.Millisecond)
}

// TestGracefulShutdown verifies proxy shuts down cleanly
func TestGracefulShutdown(t *testing.T) {
	primaryServer, err := NewMockTCPServer([]byte("PRIMARY"))
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowServer, err := NewMockTCPServer([]byte("SHADOW"))
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- proxy.Start(ctx, nil)
	}()

	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Proxy returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Proxy did not shut down within timeout")
	}
}

// BenchmarkProxyThroughput benchmarks the proxy throughput
func BenchmarkProxyThroughput(b *testing.B) {
	primaryServer, _ := NewMockTCPServer([]byte("OK"))
	defer primaryServer.Close()
	shadowServer, _ := NewMockTCPServer([]byte("OK"))
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowReadTimeout:  30 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		b.Fatalf("NewTCPProxy failed: %v", err)
	}

	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go proxy.handleConnection(conn, nil)
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, _ := net.Dial("tcp", listener.Addr().String())
		conn.Write([]byte("SELECT 1"))
		buf := make([]byte, 1024)
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		conn.Read(buf)
		conn.Close()
	}
}
