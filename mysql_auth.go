// mysql_auth.go — MySQL SSL/TLS detection, handshake modification, scramble
// extraction, and native password hashing. These are standalone protocol helpers
// used by the proxy during connection setup.
package main

import (
	"crypto/sha1"
	"encoding/binary"
	"log"
)

// isSSLRequest checks if this is an SSL upgrade request packet
// MySQL SSL request: client sends capabilities with CLIENT_SSL flag but minimal auth data
// Packet format: 4-byte header + 4-byte capabilities + 4-byte max_packet + 1-byte charset + 23-byte reserved
// Total: 4 + 32 = 36 bytes, but some clients send slightly different sizes
func isSSLRequest(packet []byte) bool {
	if len(packet) < 8 {
		log.Printf("isSSLRequest: packet too short (%d bytes)", len(packet))
		return false
	}

	// Get capability flags from the packet (after 4-byte header)
	capLow := binary.LittleEndian.Uint16(packet[4:6])
	capHigh := binary.LittleEndian.Uint16(packet[6:8])
	capabilities := uint32(capLow) | uint32(capHigh)<<16

	// Check if CLIENT_SSL is set
	hasSSL := (capabilities & clientSSL) != 0

	// SSL request packet is typically 32-36 bytes (no username/auth data)
	// Full auth packets are much larger (contain username, db, auth response)
	isMinimalPacket := len(packet) <= 40

	log.Printf("isSSLRequest: len=%d, hasSSL=%v, isMinimal=%v, caps=0x%08x",
		len(packet), hasSSL, isMinimalPacket, capabilities)

	return hasSSL && isMinimalPacket
}

// clientDeprecateEOF is the CLIENT_DEPRECATE_EOF capability flag.
// When set, the server sends OK packets instead of EOF markers in result sets.
// The proxy's response parser doesn't handle this protocol variant, so we strip it.
const clientDeprecateEOF = 0x01000000

// removeUnsupportedCapabilities removes capability flags from a client auth packet
// that the proxy doesn't support. This includes:
//   - CLIENT_SSL: proxy terminates TLS, connects to backend with plain TCP
//   - CLIENT_DEPRECATE_EOF: proxy's response parser expects traditional EOF markers
func removeUnsupportedCapabilities(packet []byte) []byte {
	if len(packet) < 8 {
		return packet
	}
	// Create a copy to avoid modifying the original
	modified := make([]byte, len(packet))
	copy(modified, packet)

	// Capabilities are 4 bytes at offset 4 (after 4-byte header): low 2 bytes + high 2 bytes
	capLow := binary.LittleEndian.Uint16(modified[4:6])
	capHigh := binary.LittleEndian.Uint16(modified[6:8])

	// Remove CLIENT_SSL (bit in low bytes)
	capLow = capLow &^ uint16(clientSSL&0xFFFF)

	// Remove CLIENT_DEPRECATE_EOF (bit in high bytes)
	capHigh = capHigh &^ uint16((clientDeprecateEOF>>16)&0xFFFF)

	// Write back
	binary.LittleEndian.PutUint16(modified[4:6], capLow)
	binary.LittleEndian.PutUint16(modified[6:8], capHigh)

	return modified
}

// modifyHandshakeForSSL modifies the server handshake to advertise SSL capability
func modifyHandshakeForSSL(packet []byte) []byte {
	// MySQL handshake packet structure (after 4-byte header):
	// 1 byte: protocol version
	// null-terminated string: server version
	// 4 bytes: connection id
	// 8 bytes: auth-plugin-data-part-1
	// 1 byte: filler (0x00)
	// 2 bytes: capability flags (lower)
	// ... more data including upper capability flags later

	// Find the capability flags position
	// Skip: 1 (protocol) + server_version (null-terminated) + 4 (conn_id) + 8 (auth) + 1 (filler)
	pos := 5 // Start after header (4) and protocol version (1)
	for pos < len(packet) && packet[pos] != 0 {
		pos++ // Skip server version string
	}
	pos++            // Skip null terminator
	pos += 4 + 8 + 1 // Skip conn_id, auth-part-1, filler

	if pos+2 > len(packet) {
		return packet // Can't modify, return as-is
	}

	// Set CLIENT_SSL capability flag (lower 2 bytes)
	capLow := binary.LittleEndian.Uint16(packet[pos : pos+2])
	capLow |= uint16(clientSSL & 0xFFFF)
	binary.LittleEndian.PutUint16(packet[pos:pos+2], capLow)

	return packet
}

// extractScrambleFromHandshake extracts the 20-byte scramble from MySQL handshake packet
func extractScrambleFromHandshake(packet []byte) []byte {
	if len(packet) < 5 {
		return nil
	}

	// Skip header (4 bytes) and protocol version (1 byte)
	pos := 5

	// Skip server version (null-terminated string)
	for pos < len(packet) && packet[pos] != 0 {
		pos++
	}
	pos++ // skip null terminator

	if pos+4 > len(packet) {
		return nil
	}

	// Skip connection id (4 bytes)
	pos += 4

	// Read scramble part 1 (8 bytes)
	if pos+8 > len(packet) {
		return nil
	}
	scramble1 := packet[pos : pos+8]
	pos += 8

	// Skip filler (1 byte)
	pos++

	// Skip capability flags lower (2 bytes)
	pos += 2

	// Skip charset (1 byte)
	pos++

	// Skip status flags (2 bytes)
	pos += 2

	// Skip capability flags upper (2 bytes)
	pos += 2

	// Read auth plugin data length (1 byte)
	if pos >= len(packet) {
		// No more data, just return first 8 bytes
		return scramble1
	}
	authPluginDataLen := int(packet[pos])
	pos++

	// Skip reserved (10 bytes)
	pos += 10

	// Read scramble part 2
	scramble2Len := authPluginDataLen - 8
	if scramble2Len < 12 {
		scramble2Len = 12
	}
	if pos+scramble2Len > len(packet) {
		scramble2Len = len(packet) - pos
	}

	scramble2 := packet[pos : pos+scramble2Len]

	// Combine scramble parts (remove trailing null if present)
	scramble := make([]byte, 0, 20)
	scramble = append(scramble, scramble1...)
	for i := 0; i < len(scramble2) && len(scramble) < 20; i++ {
		if scramble2[i] != 0 {
			scramble = append(scramble, scramble2[i])
		}
	}

	return scramble
}

// mysqlNativePassword computes MySQL native password hash
// Formula: SHA1(password) XOR SHA1(scramble + SHA1(SHA1(password)))
func mysqlNativePassword(password string, scramble []byte) []byte {
	if password == "" {
		return nil
	}

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

	// XOR SHA1(password) with SHA1(scramble + SHA1(SHA1(password)))
	result := make([]byte, len(hash1))
	for i := range hash1 {
		result[i] = hash1[i] ^ hash3[i]
	}

	return result
}
