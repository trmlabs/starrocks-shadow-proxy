package main

import (
	"testing"
)

// TestIsSSLRequest tests the SSL request packet detection
func TestIsSSLRequest(t *testing.T) {
	tests := []struct {
		name     string
		packet   []byte
		expected bool
	}{
		{
			name:     "empty packet",
			packet:   []byte{},
			expected: false,
		},
		{
			name:     "too small packet",
			packet:   []byte{0x01, 0x02, 0x03, 0x04},
			expected: false,
		},
		{
			name: "SSL request packet (32 bytes payload + 4 header = 36 total)",
			// 4-byte header (length=32, seq=1) + capabilities with CLIENT_SSL flag
			packet: func() []byte {
				p := make([]byte, 36)
				// Header: length=32 (little endian), seq=1
				p[0] = 32
				p[1] = 0
				p[2] = 0
				p[3] = 1
				// Capabilities with CLIENT_SSL (0x0800) set
				p[4] = 0x00
				p[5] = 0x08 // CLIENT_SSL in low word
				p[6] = 0x00
				p[7] = 0x00
				return p
			}(),
			expected: true,
		},
		{
			name: "non-SSL auth packet (longer than 36 bytes)",
			packet: func() []byte {
				p := make([]byte, 100)
				p[0] = 96
				p[1] = 0
				p[2] = 0
				p[3] = 1
				// Has CLIENT_SSL flag but packet is too long
				p[4] = 0x00
				p[5] = 0x08
				p[6] = 0x00
				p[7] = 0x00
				return p
			}(),
			expected: false,
		},
		{
			name: "36 byte packet without CLIENT_SSL flag",
			packet: func() []byte {
				p := make([]byte, 36)
				p[0] = 32
				p[1] = 0
				p[2] = 0
				p[3] = 1
				// No CLIENT_SSL flag
				p[4] = 0x00
				p[5] = 0x00
				p[6] = 0x00
				p[7] = 0x00
				return p
			}(),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSSLRequest(tt.packet)
			if result != tt.expected {
				t.Errorf("isSSLRequest() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestModifyHandshakeForSSL tests that SSL capability is added to handshake
func TestModifyHandshakeForSSL(t *testing.T) {
	// Create a minimal MySQL handshake packet
	// Format: header(4) + protocol(1) + version(null-term) + conn_id(4) + auth(8) + filler(1) + caps(2)
	packet := []byte{
		// Header: length=30, seq=0
		30, 0, 0, 0,
		// Protocol version
		10,
		// Server version "8.0" + null
		'8', '.', '0', 0,
		// Connection ID (4 bytes)
		1, 0, 0, 0,
		// Auth plugin data part 1 (8 bytes)
		1, 2, 3, 4, 5, 6, 7, 8,
		// Filler
		0,
		// Capability flags (lower 2 bytes) - no SSL initially
		0x00, 0x00,
		// More data...
		0, 0, 0, 0, 0, 0,
	}

	modified := modifyHandshakeForSSL(packet)

	// Check that CLIENT_SSL flag is now set in capability flags
	// Position: 4 (header) + 1 (protocol) + 4 (version+null) + 4 (conn_id) + 8 (auth) + 1 (filler) = 22
	capPos := 22
	capLow := uint16(modified[capPos]) | uint16(modified[capPos+1])<<8

	if capLow&uint16(clientSSL) == 0 {
		t.Error("CLIENT_SSL flag was not set in modified handshake")
	}
}
