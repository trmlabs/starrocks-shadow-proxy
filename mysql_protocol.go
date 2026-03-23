// mysql_protocol.go — MySQL protocol constants, command helpers, packet reading,
// and the buffered MySQLPacketReader for protocol-aware response parsing.
package main

import (
	"fmt"
	"io"
	"net"
	"strings"
)

// MySQL capability flags (relevant ones for SSL)
const (
	clientSSL              = 0x00000800 // Client supports SSL
	serverMoreResultsExist = 0x0008     // SERVER_MORE_RESULTS_EXISTS status flag
)

// MySQL command types (packet byte after 4-byte header)
const (
	comSleep            = 0x00
	comQuit             = 0x01
	comInitDB           = 0x02
	comQuery            = 0x03
	comFieldList        = 0x04
	comCreateDB         = 0x05
	comDropDB           = 0x06
	comRefresh          = 0x07
	comShutdown         = 0x08
	comStatistics       = 0x09
	comProcessInfo      = 0x0A
	comConnect          = 0x0B
	comProcessKill      = 0x0C
	comDebug            = 0x0D
	comPing             = 0x0E
	comTime             = 0x0F
	comDelayedInsert    = 0x10
	comChangeUser       = 0x11
	comBinlogDump       = 0x12
	comTableDump        = 0x13
	comConnectOut       = 0x14
	comRegisterSlave    = 0x15
	comStmtPrepare      = 0x16
	comStmtExecute      = 0x17
	comStmtSendLongData = 0x18
	comStmtClose        = 0x19
	comStmtReset        = 0x1A
	comSetOption        = 0x1B
	comStmtFetch        = 0x1C
	comDaemon           = 0x1D
	comBinlogDumpGTID   = 0x1E
	comResetConnection  = 0x1F
)

// getMySQLCommandName returns a human-readable name for a MySQL command byte
func getMySQLCommandName(cmd byte) string {
	switch cmd {
	case comSleep:
		return "COM_SLEEP"
	case comQuit:
		return "COM_QUIT"
	case comInitDB:
		return "COM_INIT_DB"
	case comQuery:
		return "COM_QUERY"
	case comFieldList:
		return "COM_FIELD_LIST"
	case comCreateDB:
		return "COM_CREATE_DB"
	case comDropDB:
		return "COM_DROP_DB"
	case comRefresh:
		return "COM_REFRESH"
	case comShutdown:
		return "COM_SHUTDOWN"
	case comStatistics:
		return "COM_STATISTICS"
	case comProcessInfo:
		return "COM_PROCESS_INFO"
	case comConnect:
		return "COM_CONNECT"
	case comProcessKill:
		return "COM_PROCESS_KILL"
	case comDebug:
		return "COM_DEBUG"
	case comPing:
		return "COM_PING"
	case comChangeUser:
		return "COM_CHANGE_USER"
	case comStmtPrepare:
		return "COM_STMT_PREPARE"
	case comStmtExecute:
		return "COM_STMT_EXECUTE"
	case comStmtSendLongData:
		return "COM_STMT_SEND_LONG_DATA"
	case comStmtClose:
		return "COM_STMT_CLOSE"
	case comStmtReset:
		return "COM_STMT_RESET"
	case comSetOption:
		return "COM_SET_OPTION"
	case comStmtFetch:
		return "COM_STMT_FETCH"
	case comResetConnection:
		return "COM_RESET_CONNECTION"
	default:
		return fmt.Sprintf("COM_UNKNOWN(0x%02X)", cmd)
	}
}

// isCountableCommand returns true if this MySQL command should be counted as a "query"
// for metrics purposes. This excludes administrative commands like PING, QUIT, etc.
func isCountableCommand(cmd byte) bool {
	switch cmd {
	case comQuery, comStmtPrepare, comStmtExecute:
		// These are actual queries that should be counted
		return true
	default:
		// Administrative commands (PING, QUIT, etc.) are not counted
		return false
	}
}

// isSimpleResponseCommand returns true if the MySQL command always returns a single
// OK or ERR packet (never a result set). For these commands, we bypass the full
// result-set parser in ReadAndForwardResponse/ReadFullResponse to avoid hangs when
// the server sets status flags like SERVER_MORE_RESULTS_EXISTS in the OK packet
// (e.g., StarRocks USE catalog.database via COM_INIT_DB).
func isSimpleResponseCommand(cmd byte) bool {
	switch cmd {
	case comInitDB: // USE database / USE catalog.database
		return true
	case comPing: // Health check
		return true
	case comStmtReset: // Reset prepared statement
		return true
	case comResetConnection: // Reset session state
		return true
	default:
		return false
	}
}

// isNoResponseCommand returns true if the MySQL command expects no server response.
func isNoResponseCommand(cmd byte) bool {
	switch cmd {
	case comQuit: // Connection close
		return true
	case comStmtClose: // Close prepared statement
		return true
	case comStmtSendLongData: // Send blob data for prepared statement
		return true
	default:
		return false
	}
}

// getMySQLCommand extracts the command byte from a MySQL packet
// Returns the command byte and true if valid, or 0 and false if packet is too short
func getMySQLCommand(packet []byte) (byte, bool) {
	if len(packet) < 5 {
		return 0, false
	}
	return packet[4], true
}

// extractQueryPreview extracts a readable preview from a MySQL packet
// MySQL COM_QUERY packet: [4-byte header][1-byte command=0x03][query string]
func extractQueryPreview(packet []byte, maxLen int) string {
	if len(packet) < 5 {
		return "<packet too short>"
	}

	// Check if this is a COM_QUERY (0x03) packet
	cmd := packet[4]
	if cmd != 0x03 {
		return fmt.Sprintf("<not a query, cmd=0x%02x>", cmd)
	}

	// Extract query string (starts at byte 5)
	if len(packet) <= 5 {
		return "<empty query>"
	}

	query := string(packet[5:])

	// Truncate if needed
	if len(query) > maxLen {
		query = query[:maxLen] + "..."
	}

	// Replace newlines for cleaner logging
	query = strings.ReplaceAll(query, "\n", " ")
	query = strings.ReplaceAll(query, "\r", "")

	return query
}

// readMySQLPacket reads a single MySQL packet (4-byte header + payload)
func readMySQLPacket(conn net.Conn) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}

	// MySQL packet length is 3 bytes little-endian
	length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16

	packet := make([]byte, 4+length)
	copy(packet[:4], header)
	if _, err := io.ReadFull(conn, packet[4:]); err != nil {
		return nil, err
	}

	return packet, nil
}

// MySQLPacketReader provides buffered reading of complete MySQL packets from a connection.
// This ensures we always get complete packets even when TCP fragments them across reads.
type MySQLPacketReader struct {
	conn   net.Conn
	buf    []byte
	offset int // Current read position in buffer
	end    int // End of valid data in buffer
}

// NewMySQLPacketReader creates a new packet reader for a connection
func NewMySQLPacketReader(conn net.Conn) *MySQLPacketReader {
	return &MySQLPacketReader{
		conn: conn,
		buf:  make([]byte, 64*1024), // 64KB buffer
	}
}

// ReadPacket reads a complete MySQL packet (4-byte header + payload)
// Returns the complete packet including header
func (r *MySQLPacketReader) ReadPacket() ([]byte, error) {
	// Ensure we have at least 4 bytes for the header
	if err := r.ensureBytes(4); err != nil {
		return nil, err
	}

	// Parse packet length from header (3 bytes little-endian)
	length := int(r.buf[r.offset]) | int(r.buf[r.offset+1])<<8 | int(r.buf[r.offset+2])<<16
	totalLen := 4 + length

	// Ensure we have the complete packet
	if err := r.ensureBytes(totalLen); err != nil {
		return nil, err
	}

	// Copy the complete packet
	packet := make([]byte, totalLen)
	copy(packet, r.buf[r.offset:r.offset+totalLen])
	r.offset += totalLen

	return packet, nil
}

// ensureBytes ensures we have at least n bytes available in the buffer starting at offset
func (r *MySQLPacketReader) ensureBytes(n int) error {
	available := r.end - r.offset
	if available >= n {
		return nil
	}

	// Compact buffer if needed (move unread data to beginning)
	if r.offset > 0 {
		if available > 0 {
			copy(r.buf, r.buf[r.offset:r.end])
		}
		r.end = available
		r.offset = 0
	}

	// Grow buffer if needed
	if n > len(r.buf) {
		newBuf := make([]byte, n*2)
		copy(newBuf, r.buf[:r.end])
		r.buf = newBuf
	}

	// Read more data until we have enough
	for r.end < n {
		bytesRead, err := r.conn.Read(r.buf[r.end:])
		if err != nil {
			return err
		}
		r.end += bytesRead
	}

	return nil
}

// ReadMultiPacket reads a potentially multi-packet MySQL command.
// MySQL uses 16MB max packet size, so large commands are split into multiple packets.
// This function reads all packets in a sequence (packets with length 0xFFFFFF continue).
func (r *MySQLPacketReader) ReadMultiPacket() ([]byte, error) {
	var result []byte

	for {
		packet, err := r.ReadPacket()
		if err != nil {
			return nil, err
		}

		result = append(result, packet...)

		// Check if this is a continuation packet (length == 0xFFFFFF = 16MB-1)
		length := int(packet[0]) | int(packet[1])<<8 | int(packet[2])<<16
		if length < 0xFFFFFF {
			// This is the last packet in the sequence
			break
		}
		// Length is exactly 0xFFFFFF, more packets follow
	}

	return result, nil
}

// ReadFullResponse reads a complete MySQL response by parsing the protocol.
// This eliminates the need for timeout-based detection of response completion.
// MySQL response types:
//   - OK packet (0x00): Single packet, query succeeded
//   - ERR packet (0xFF): Single packet, query failed
//   - Result set: Column count + column defs + EOF + rows + EOF/OK
//   - LOCAL INFILE (0xFB): Special case, not commonly used
//
// Handles multi-statement queries by checking SERVER_MORE_RESULTS_EXISTS flag.
func (r *MySQLPacketReader) ReadFullResponse() (int64, error) {
	var totalBytes int64

	// Loop to handle multiple result sets (multi-statement queries)
	for {
		// Read the first packet to determine response type
		packet, err := r.ReadPacket()
		if err != nil {
			return totalBytes, err
		}
		totalBytes += int64(len(packet))

		if len(packet) < 5 {
			return totalBytes, fmt.Errorf("packet too short: %d bytes", len(packet))
		}

		// First byte of payload (after 4-byte header) indicates response type
		responseType := packet[4]

		switch responseType {
		case 0x00: // OK packet
			// Check for SERVER_MORE_RESULTS_EXISTS flag
			if hasMoreResults := r.checkMoreResultsInOKPacket(packet); hasMoreResults {
				continue // Read next result set
			}
			return totalBytes, nil

		case 0xFF: // ERR packet
			// Single packet response, we're done (no more results after error)
			return totalBytes, nil

		case 0xFB: // LOCAL INFILE request
			// This is rare; for now just return (client would need to send file data)
			return totalBytes, nil

		case 0xFE: // EOF packet (can also be OK in newer MySQL)
			// Check for SERVER_MORE_RESULTS_EXISTS flag
			if hasMoreResults := r.checkMoreResultsInEOFPacket(packet); hasMoreResults {
				continue // Read next result set
			}
			return totalBytes, nil

		default:
			// Result set - first packet contains column count
			// Read column definitions until EOF
			columnCount := r.readLengthEncodedInt(packet[4:])

			// Read column definition packets
			for i := uint64(0); i < columnCount; i++ {
				pkt, err := r.ReadPacket()
				if err != nil {
					return totalBytes, err
				}
				totalBytes += int64(len(pkt))
			}

			// Read EOF packet after column definitions (unless CLIENT_DEPRECATE_EOF)
			pkt, err := r.ReadPacket()
			if err != nil {
				return totalBytes, err
			}
			totalBytes += int64(len(pkt))

			// Check if this is EOF (0xFE) or if columns were followed by OK (newer protocol)
			if len(pkt) >= 5 && pkt[4] == 0x00 {
				// OK packet - no rows follow, check for more result sets
				if hasMoreResults := r.checkMoreResultsInOKPacket(pkt); hasMoreResults {
					continue // Read next result set
				}
				return totalBytes, nil
			}

			// Read row packets until EOF or OK
			moreResults := false
		rowLoop:
			for {
				pkt, err := r.ReadPacket()
				if err != nil {
					return totalBytes, err
				}
				totalBytes += int64(len(pkt))

				if len(pkt) >= 5 {
					marker := pkt[4]
					payloadLen := int(pkt[0]) | int(pkt[1])<<8 | int(pkt[2])<<16
					// EOF packet (0xFE with small payload) marks end of rows.
					// We strip CLIENT_DEPRECATE_EOF during auth, so the server always
					// sends traditional EOF markers — no need to check for OK (0x00).
					if marker == 0xFE && payloadLen < 9 {
						moreResults = r.checkMoreResultsInEOFPacket(pkt)
						break rowLoop
					}
				}
			}

			if moreResults {
				continue // Read next result set
			}
			return totalBytes, nil
		}
	}
}

// checkMoreResultsInEOFPacket checks if SERVER_MORE_RESULTS_EXISTS is set in an EOF packet.
// EOF packet format (after 4-byte header): marker(1) + warnings(2) + status_flags(2)
func (r *MySQLPacketReader) checkMoreResultsInEOFPacket(pkt []byte) bool {
	// Need at least: 4 (header) + 1 (marker) + 2 (warnings) + 2 (status) = 9 bytes
	if len(pkt) < 9 {
		return false
	}
	// Status flags are at bytes 7-8 (0-indexed)
	statusFlags := uint16(pkt[7]) | uint16(pkt[8])<<8
	return (statusFlags & serverMoreResultsExist) != 0
}

// checkMoreResultsInOKPacket checks if SERVER_MORE_RESULTS_EXISTS is set in an OK packet.
// OK packet format (after 4-byte header): marker(1) + affected_rows(lenenc) + last_insert_id(lenenc) + status_flags(2) + warnings(2)
func (r *MySQLPacketReader) checkMoreResultsInOKPacket(pkt []byte) bool {
	if len(pkt) < 7 {
		return false
	}
	// Skip header (4 bytes) and marker (1 byte)
	pos := 5

	// Skip affected_rows (length-encoded integer)
	pos += r.lenencIntSize(pkt[pos:])
	if pos >= len(pkt) {
		return false
	}

	// Skip last_insert_id (length-encoded integer)
	pos += r.lenencIntSize(pkt[pos:])
	if pos+2 > len(pkt) {
		return false
	}

	// Status flags are now at pos
	statusFlags := uint16(pkt[pos]) | uint16(pkt[pos+1])<<8
	return (statusFlags & serverMoreResultsExist) != 0
}

// lenencIntSize returns the size in bytes of a length-encoded integer at the start of buf.
func (r *MySQLPacketReader) lenencIntSize(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	switch buf[0] {
	case 0xFC:
		return 3 // 1 byte marker + 2 byte value
	case 0xFD:
		return 4 // 1 byte marker + 3 byte value
	case 0xFE:
		return 9 // 1 byte marker + 8 byte value
	default:
		return 1 // Single byte value
	}
}

// readLengthEncodedInt reads a MySQL length-encoded integer from the buffer
func (r *MySQLPacketReader) readLengthEncodedInt(buf []byte) uint64 {
	if len(buf) == 0 {
		return 0
	}
	switch buf[0] {
	case 0xFC:
		if len(buf) >= 3 {
			return uint64(buf[1]) | uint64(buf[2])<<8
		}
	case 0xFD:
		if len(buf) >= 4 {
			return uint64(buf[1]) | uint64(buf[2])<<8 | uint64(buf[3])<<16
		}
	case 0xFE:
		if len(buf) >= 9 {
			return uint64(buf[1]) | uint64(buf[2])<<8 | uint64(buf[3])<<16 | uint64(buf[4])<<24 |
				uint64(buf[5])<<32 | uint64(buf[6])<<40 | uint64(buf[7])<<48 | uint64(buf[8])<<56
		}
	default:
		return uint64(buf[0])
	}
	return 0
}

// ReadAndForwardSimpleResponse reads a single MySQL response packet and forwards it.
// Use this for commands that always return a single OK or ERR packet (COM_INIT_DB,
// COM_PING, etc.) to avoid the complex result-set parser which can hang if the server
// sets unexpected status flags like SERVER_MORE_RESULTS_EXISTS.
func (r *MySQLPacketReader) ReadAndForwardSimpleResponse(dest io.Writer) (int64, error) {
	packet, err := r.ReadPacket()
	if err != nil {
		return 0, err
	}
	if _, err := dest.Write(packet); err != nil {
		return 0, fmt.Errorf("failed to forward simple response: %w", err)
	}
	return int64(len(packet)), nil
}

// ReadSimpleResponse reads and discards a single MySQL response packet.
// Use this on the shadow side for commands that always return a single OK or ERR packet.
func (r *MySQLPacketReader) ReadSimpleResponse() (int64, error) {
	packet, err := r.ReadPacket()
	if err != nil {
		return 0, err
	}
	return int64(len(packet)), nil
}

// ReadAndForwardResponse reads a complete MySQL response and forwards each packet
// to the destination writer immediately as it's read. This allows accurate timing
// measurement without buffering the entire response.
// Returns total bytes read and any error encountered.
func (r *MySQLPacketReader) ReadAndForwardResponse(dest io.Writer) (int64, error) {
	var totalBytes int64

	// Loop to handle multiple result sets (multi-statement queries)
	for {
		// Read the first packet to determine response type
		packet, err := r.ReadPacket()
		if err != nil {
			return totalBytes, err
		}

		// Forward immediately
		if _, err := dest.Write(packet); err != nil {
			return totalBytes, fmt.Errorf("failed to forward packet: %w", err)
		}
		totalBytes += int64(len(packet))

		if len(packet) < 5 {
			return totalBytes, fmt.Errorf("packet too short: %d bytes", len(packet))
		}

		// First byte of payload (after 4-byte header) indicates response type
		responseType := packet[4]

		switch responseType {
		case 0x00: // OK packet
			if hasMoreResults := r.checkMoreResultsInOKPacket(packet); hasMoreResults {
				continue
			}
			return totalBytes, nil

		case 0xFF: // ERR packet
			return totalBytes, nil

		case 0xFB: // LOCAL INFILE request
			return totalBytes, nil

		case 0xFE: // EOF packet
			if hasMoreResults := r.checkMoreResultsInEOFPacket(packet); hasMoreResults {
				continue
			}
			return totalBytes, nil

		default:
			// Result set - first packet contains column count
			columnCount := r.readLengthEncodedInt(packet[4:])

			// Read and forward column definition packets
			for i := uint64(0); i < columnCount; i++ {
				pkt, err := r.ReadPacket()
				if err != nil {
					return totalBytes, err
				}
				if _, err := dest.Write(pkt); err != nil {
					return totalBytes, fmt.Errorf("failed to forward column def: %w", err)
				}
				totalBytes += int64(len(pkt))
			}

			// Read and forward EOF/OK packet after column definitions
			pkt, err := r.ReadPacket()
			if err != nil {
				return totalBytes, err
			}
			if _, err := dest.Write(pkt); err != nil {
				return totalBytes, fmt.Errorf("failed to forward column EOF: %w", err)
			}
			totalBytes += int64(len(pkt))

			// Check if columns were followed by OK (newer protocol, no rows)
			if len(pkt) >= 5 && pkt[4] == 0x00 {
				if hasMoreResults := r.checkMoreResultsInOKPacket(pkt); hasMoreResults {
					continue
				}
				return totalBytes, nil
			}

			// Read and forward row packets until EOF or OK
			moreResults := false
		rowLoop:
			for {
				pkt, err := r.ReadPacket()
				if err != nil {
					return totalBytes, err
				}
				if _, err := dest.Write(pkt); err != nil {
					return totalBytes, fmt.Errorf("failed to forward row: %w", err)
				}
				totalBytes += int64(len(pkt))

				if len(pkt) >= 5 {
					marker := pkt[4]
					payloadLen := int(pkt[0]) | int(pkt[1])<<8 | int(pkt[2])<<16
					// EOF packet (0xFE with small payload) marks end of rows.
					// We strip CLIENT_DEPRECATE_EOF during auth, so the server always
					// sends traditional EOF markers — no need to check for OK (0x00).
					if marker == 0xFE && payloadLen < 9 {
						moreResults = r.checkMoreResultsInEOFPacket(pkt)
						break rowLoop
					}
				}
			}

			if moreResults {
				continue
			}
			return totalBytes, nil
		}
	}
}
