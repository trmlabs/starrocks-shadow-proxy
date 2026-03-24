package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// TestGetMySQLCommandName tests command name mapping
func TestGetMySQLCommandName(t *testing.T) {
	tests := []struct {
		cmd      byte
		expected string
	}{
		{comQuery, "COM_QUERY"},
		{comStmtPrepare, "COM_STMT_PREPARE"},
		{comStmtExecute, "COM_STMT_EXECUTE"},
		{comPing, "COM_PING"},
		{comQuit, "COM_QUIT"},
		{comInitDB, "COM_INIT_DB"},
		{0xFF, "COM_UNKNOWN(0xFF)"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := getMySQLCommandName(tt.cmd)
			if result != tt.expected {
				t.Errorf("getMySQLCommandName(0x%02X) = %s, expected %s", tt.cmd, result, tt.expected)
			}
		})
	}
}

// TestIsCountableCommand tests which commands are counted as queries
func TestIsCountableCommand(t *testing.T) {
	tests := []struct {
		cmd      byte
		name     string
		expected bool
	}{
		{comQuery, "COM_QUERY", true},
		{comStmtPrepare, "COM_STMT_PREPARE", true},
		{comStmtExecute, "COM_STMT_EXECUTE", true},
		{comPing, "COM_PING", false},
		{comQuit, "COM_QUIT", false},
		{comInitDB, "COM_INIT_DB", false},
		{comStmtClose, "COM_STMT_CLOSE", false},
		{comStmtReset, "COM_STMT_RESET", false},
		{comSetOption, "COM_SET_OPTION", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isCountableCommand(tt.cmd)
			if result != tt.expected {
				t.Errorf("isCountableCommand(%s) = %v, expected %v", tt.name, result, tt.expected)
			}
		})
	}
}

// TestGetMySQLCommand tests extraction of command byte from packet
func TestGetMySQLCommand(t *testing.T) {
	tests := []struct {
		name     string
		packet   []byte
		expected byte
		ok       bool
	}{
		{
			name:     "empty packet",
			packet:   []byte{},
			expected: 0,
			ok:       false,
		},
		{
			name:     "too short packet",
			packet:   []byte{0x01, 0x00, 0x00, 0x00},
			expected: 0,
			ok:       false,
		},
		{
			name: "COM_QUERY packet",
			// Header: length=6 (little endian), seq=0; Command: 0x03 (COM_QUERY)
			packet:   []byte{0x06, 0x00, 0x00, 0x00, 0x03, 'S', 'E', 'L', 'E', 'C'},
			expected: comQuery,
			ok:       true,
		},
		{
			name:     "COM_PING packet",
			packet:   []byte{0x01, 0x00, 0x00, 0x00, 0x0E},
			expected: comPing,
			ok:       true,
		},
		{
			name:     "COM_QUIT packet",
			packet:   []byte{0x01, 0x00, 0x00, 0x00, 0x01},
			expected: comQuit,
			ok:       true,
		},
		{
			name:     "COM_STMT_PREPARE packet",
			packet:   []byte{0x01, 0x00, 0x00, 0x00, 0x16},
			expected: comStmtPrepare,
			ok:       true,
		},
		{
			name:     "COM_STMT_EXECUTE packet",
			packet:   []byte{0x01, 0x00, 0x00, 0x00, 0x17},
			expected: comStmtExecute,
			ok:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := getMySQLCommand(tt.packet)
			if ok != tt.ok {
				t.Errorf("getMySQLCommand() ok = %v, expected %v", ok, tt.ok)
			}
			if ok && cmd != tt.expected {
				t.Errorf("getMySQLCommand() cmd = 0x%02X, expected 0x%02X", cmd, tt.expected)
			}
		})
	}
}

// TestMySQLPacketReader tests the buffered packet reader
func TestMySQLPacketReader(t *testing.T) {
	// Create a pipe for testing
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	reader := NewMySQLPacketReader(client)

	// Test case 1: Read a simple packet
	t.Run("simple packet", func(t *testing.T) {
		go func() {
			// Write a complete MySQL packet: header(4) + payload
			// Length=5, Seq=0, Payload="hello"
			server.Write([]byte{0x05, 0x00, 0x00, 0x00, 'h', 'e', 'l', 'l', 'o'})
		}()

		packet, err := reader.ReadPacket()
		if err != nil {
			t.Fatalf("ReadPacket failed: %v", err)
		}

		if len(packet) != 9 {
			t.Errorf("Expected packet length 9, got %d", len(packet))
		}

		payload := string(packet[4:])
		if payload != "hello" {
			t.Errorf("Expected payload 'hello', got '%s'", payload)
		}
	})
}

// TestMySQLPacketReaderFragmented tests reading a packet that arrives in fragments
func TestMySQLPacketReaderFragmented(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	reader := NewMySQLPacketReader(client)

	// Simulate TCP fragmentation by sending packet in small chunks
	go func() {
		// Packet: length=10, seq=0, payload="SELECT * !"
		fullPacket := []byte{0x0A, 0x00, 0x00, 0x00, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '*', ' ', '!'}

		// Send in 3 fragments
		server.Write(fullPacket[:3]) // First 3 bytes (partial header)
		time.Sleep(10 * time.Millisecond)
		server.Write(fullPacket[3:7]) // Rest of header + start of payload
		time.Sleep(10 * time.Millisecond)
		server.Write(fullPacket[7:]) // Rest of payload
	}()

	packet, err := reader.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket failed: %v", err)
	}

	if len(packet) != 14 {
		t.Errorf("Expected packet length 14, got %d", len(packet))
	}

	// Check command byte position
	payload := string(packet[4:])
	if payload != "SELECT * !" {
		t.Errorf("Expected payload 'SELECT * !', got '%s'", payload)
	}
}

// TestMySQLPacketReaderMultiplePackets tests reading multiple packets in sequence
func TestMySQLPacketReaderMultiplePackets(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	reader := NewMySQLPacketReader(client)

	go func() {
		// Write two packets back-to-back
		// Packet 1: length=4, seq=0, payload="PING"
		server.Write([]byte{0x04, 0x00, 0x00, 0x00, 'P', 'I', 'N', 'G'})
		// Packet 2: length=4, seq=1, payload="PONG"
		server.Write([]byte{0x04, 0x00, 0x00, 0x01, 'P', 'O', 'N', 'G'})
	}()

	// Read first packet
	packet1, err := reader.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket 1 failed: %v", err)
	}
	if string(packet1[4:]) != "PING" {
		t.Errorf("Expected first packet 'PING', got '%s'", packet1[4:])
	}

	// Read second packet
	packet2, err := reader.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket 2 failed: %v", err)
	}
	if string(packet2[4:]) != "PONG" {
		t.Errorf("Expected second packet 'PONG', got '%s'", packet2[4:])
	}
}

// TestMySQLPacketReaderLargePacket tests reading a larger packet
func TestMySQLPacketReaderLargePacket(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	reader := NewMySQLPacketReader(client)

	// Create a large payload (100KB)
	payloadSize := 100 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	go func() {
		// Write header
		header := []byte{
			byte(payloadSize),
			byte(payloadSize >> 8),
			byte(payloadSize >> 16),
			0x00, // seq
		}
		server.Write(header)
		// Write payload in chunks to simulate network behavior
		chunkSize := 1024
		for i := 0; i < len(payload); i += chunkSize {
			end := i + chunkSize
			if end > len(payload) {
				end = len(payload)
			}
			server.Write(payload[i:end])
		}
	}()

	packet, err := reader.ReadPacket()
	if err != nil {
		t.Fatalf("ReadPacket failed: %v", err)
	}

	expectedLen := 4 + payloadSize
	if len(packet) != expectedLen {
		t.Errorf("Expected packet length %d, got %d", expectedLen, len(packet))
	}

	// Verify payload content
	for i := 0; i < payloadSize; i++ {
		expected := byte(i % 256)
		if packet[4+i] != expected {
			t.Errorf("Payload mismatch at offset %d: got %d, expected %d", i, packet[4+i], expected)
			break
		}
	}
}

// TestReadMultiPacket tests reading multi-packet sequences (packets > 16MB)
func TestReadMultiPacket(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	reader := NewMySQLPacketReader(client)

	// Test a simple single packet (should work with ReadMultiPacket too)
	go func() {
		// Single packet, length < 0xFFFFFF
		server.Write([]byte{0x05, 0x00, 0x00, 0x00, 'h', 'e', 'l', 'l', 'o'})
	}()

	packet, err := reader.ReadMultiPacket()
	if err != nil {
		t.Fatalf("ReadMultiPacket failed: %v", err)
	}

	if len(packet) != 9 {
		t.Errorf("Expected packet length 9, got %d", len(packet))
	}
}

// TestIsSimpleResponseCommand tests which commands are identified as single-packet response
func TestIsSimpleResponseCommand(t *testing.T) {
	tests := []struct {
		cmd      byte
		name     string
		expected bool
	}{
		{comInitDB, "COM_INIT_DB", true},
		{comPing, "COM_PING", true},
		{comStmtReset, "COM_STMT_RESET", true},
		{comResetConnection, "COM_RESET_CONNECTION", true},
		{comQuery, "COM_QUERY", false},
		{comStmtPrepare, "COM_STMT_PREPARE", false},
		{comStmtExecute, "COM_STMT_EXECUTE", false},
		{comQuit, "COM_QUIT", false},
		{comStmtClose, "COM_STMT_CLOSE", false},
		{comFieldList, "COM_FIELD_LIST", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSimpleResponseCommand(tt.cmd)
			if result != tt.expected {
				t.Errorf("isSimpleResponseCommand(%s) = %v, expected %v", tt.name, result, tt.expected)
			}
		})
	}
}

// TestIsNoResponseCommand tests which commands expect no server response
func TestIsNoResponseCommand(t *testing.T) {
	tests := []struct {
		cmd      byte
		name     string
		expected bool
	}{
		{comQuit, "COM_QUIT", true},
		{comStmtClose, "COM_STMT_CLOSE", true},
		{comStmtSendLongData, "COM_STMT_SEND_LONG_DATA", true},
		{comQuery, "COM_QUERY", false},
		{comInitDB, "COM_INIT_DB", false},
		{comPing, "COM_PING", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isNoResponseCommand(tt.cmd)
			if result != tt.expected {
				t.Errorf("isNoResponseCommand(%s) = %v, expected %v", tt.name, result, tt.expected)
			}
		})
	}
}

// TestReadAndForwardSimpleResponse tests that simple response reading works for OK/ERR packets
func TestReadAndForwardSimpleResponse(t *testing.T) {
	t.Run("OK packet", func(t *testing.T) {
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()

		reader := NewMySQLPacketReader(client)

		// Send a standard OK packet
		go func() {
			// OK packet: marker=0x00, affected_rows=0, insert_id=0, status=0x0002, warnings=0
			payload := []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
			packet := make([]byte, 4+len(payload))
			packet[0] = byte(len(payload))
			packet[1] = 0
			packet[2] = 0
			packet[3] = 1 // seq
			copy(packet[4:], payload)
			server.Write(packet)
		}()

		// Use a pipe as the destination writer
		destServer, destClient := net.Pipe()
		defer destServer.Close()
		defer destClient.Close()

		var forwarded []byte
		done := make(chan struct{})
		go func() {
			buf := make([]byte, 4096)
			n, _ := destServer.Read(buf)
			forwarded = buf[:n]
			close(done)
		}()

		bytesRead, err := reader.ReadAndForwardSimpleResponse(destClient)
		if err != nil {
			t.Fatalf("ReadAndForwardSimpleResponse failed: %v", err)
		}
		if bytesRead != 11 { // 4 header + 7 payload
			t.Errorf("Expected 11 bytes read, got %d", bytesRead)
		}

		<-done
		if len(forwarded) != 11 {
			t.Errorf("Expected 11 bytes forwarded, got %d", len(forwarded))
		}
		if forwarded[4] != 0x00 {
			t.Errorf("Expected OK marker (0x00), got 0x%02X", forwarded[4])
		}
	})

	t.Run("ERR packet", func(t *testing.T) {
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()

		reader := NewMySQLPacketReader(client)

		// Send an ERR packet
		go func() {
			payload := []byte{0xFF, 0x48, 0x04, '#', '4', '2', '0', '0', '0', 'N', 'o'}
			packet := make([]byte, 4+len(payload))
			packet[0] = byte(len(payload))
			packet[1] = 0
			packet[2] = 0
			packet[3] = 1
			copy(packet[4:], payload)
			server.Write(packet)
		}()

		destServer, destClient := net.Pipe()
		defer destServer.Close()
		defer destClient.Close()

		var forwarded []byte
		done := make(chan struct{})
		go func() {
			buf := make([]byte, 4096)
			n, _ := destServer.Read(buf)
			forwarded = buf[:n]
			close(done)
		}()

		_, err := reader.ReadAndForwardSimpleResponse(destClient)
		if err != nil {
			t.Fatalf("ReadAndForwardSimpleResponse failed: %v", err)
		}

		<-done
		if forwarded[4] != 0xFF {
			t.Errorf("Expected ERR marker (0xFF), got 0x%02X", forwarded[4])
		}
	})

	t.Run("OK with SERVER_MORE_RESULTS_EXISTS does NOT hang", func(t *testing.T) {
		// This is the critical test: StarRocks may set SERVER_MORE_RESULTS_EXISTS
		// in the OK response for USE catalog.database. ReadAndForwardSimpleResponse
		// must return after one packet regardless of this flag.
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()

		reader := NewMySQLPacketReader(client)

		go func() {
			// OK packet with SERVER_MORE_RESULTS_EXISTS (0x0008) set in status flags
			// status = 0x000A = SERVER_STATUS_AUTOCOMMIT (0x0002) | SERVER_MORE_RESULTS_EXISTS (0x0008)
			payload := []byte{0x00, 0x00, 0x00, 0x0A, 0x00, 0x00, 0x00}
			packet := make([]byte, 4+len(payload))
			packet[0] = byte(len(payload))
			packet[1] = 0
			packet[2] = 0
			packet[3] = 1
			copy(packet[4:], payload)
			server.Write(packet)
			// Importantly: we do NOT send a second result set.
			// ReadAndForwardResponse would hang here. ReadAndForwardSimpleResponse must not.
		}()

		destServer, destClient := net.Pipe()
		defer destServer.Close()
		defer destClient.Close()

		var forwarded []byte
		done := make(chan struct{})
		go func() {
			buf := make([]byte, 4096)
			n, _ := destServer.Read(buf)
			forwarded = buf[:n]
			close(done)
		}()

		// This must complete without hanging
		resultCh := make(chan error, 1)
		go func() {
			_, err := reader.ReadAndForwardSimpleResponse(destClient)
			resultCh <- err
		}()

		select {
		case err := <-resultCh:
			if err != nil {
				t.Fatalf("ReadAndForwardSimpleResponse failed: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ReadAndForwardSimpleResponse HUNG — this is the bug we're fixing")
		}

		<-done
		if len(forwarded) != 11 {
			t.Errorf("Expected 11 bytes forwarded, got %d", len(forwarded))
		}
	})
}

// TestReadFullResponseHangsOnMoreResultsFlag demonstrates that ReadFullResponse
// would hang when SERVER_MORE_RESULTS_EXISTS is set but no second result follows.
// This is the exact scenario that COM_INIT_DB with StarRocks USE catalog.database triggers.
func TestReadFullResponseHangsOnMoreResultsFlag(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	reader := NewMySQLPacketReader(client)

	go func() {
		// OK packet with SERVER_MORE_RESULTS_EXISTS (0x0008)
		payload := []byte{0x00, 0x00, 0x00, 0x0A, 0x00, 0x00, 0x00}
		packet := make([]byte, 4+len(payload))
		packet[0] = byte(len(payload))
		packet[1] = 0
		packet[2] = 0
		packet[3] = 1
		copy(packet[4:], payload)
		server.Write(packet)
		// No second result set — ReadFullResponse will hang
	}()

	resultCh := make(chan error, 1)
	go func() {
		_, err := reader.ReadFullResponse()
		resultCh <- err
	}()

	select {
	case <-resultCh:
		t.Fatal("ReadFullResponse returned — expected it to hang (proving the bug exists)")
	case <-time.After(500 * time.Millisecond):
		// Expected: ReadFullResponse hangs because it's waiting for the next result set.
		// This confirms that using ReadFullResponse for COM_INIT_DB would be wrong.
		t.Log("Confirmed: ReadFullResponse hangs on SERVER_MORE_RESULTS_EXISTS without a second result set")
	}
}

// TestExtractQueryPreviewWithCommand tests query extraction for different command types
func TestExtractQueryPreviewWithCommand(t *testing.T) {
	tests := []struct {
		name     string
		packet   []byte
		maxLen   int
		contains string
	}{
		{
			name: "COM_QUERY with SELECT",
			// Header: length=12, seq=0; Command: 0x03; Query: "SELECT 1"
			packet:   []byte{0x09, 0x00, 0x00, 0x00, 0x03, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1'},
			maxLen:   100,
			contains: "SELECT 1",
		},
		{
			name:     "non-query command",
			packet:   []byte{0x01, 0x00, 0x00, 0x00, 0x0E}, // COM_PING
			maxLen:   100,
			contains: "not a query",
		},
		{
			name:     "packet too short",
			packet:   []byte{0x01, 0x00, 0x00},
			maxLen:   100,
			contains: "too short",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractQueryPreview(tt.packet, tt.maxLen)
			if !strings.Contains(result, tt.contains) {
				t.Errorf("extractQueryPreview() = %q, expected to contain %q", result, tt.contains)
			}
		})
	}
}

// TestReadMySQLPacket tests MySQL packet reading
func TestReadMySQLPacket(t *testing.T) {
	// Create a pipe for testing
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Write a MySQL packet from server side
	go func() {
		// Header: length=5, seq=0
		server.Write([]byte{5, 0, 0, 0})
		// Payload
		server.Write([]byte("hello"))
	}()

	// Read from client side
	packet, err := readMySQLPacket(client)
	if err != nil {
		t.Fatalf("readMySQLPacket failed: %v", err)
	}

	if len(packet) != 9 { // 4 header + 5 payload
		t.Errorf("Expected packet length 9, got %d", len(packet))
	}

	payload := string(packet[4:])
	if payload != "hello" {
		t.Errorf("Expected payload 'hello', got '%s'", payload)
	}
}
