// pg_protocol.go — PostgreSQL wire protocol (v3) packet reader and helpers.
//
// pgwire is simpler than the MySQL protocol: every regular message is
// [type:1][length:4][payload]. The startup phase is the only oddity — the
// first message from the client has no type byte and is one of:
//
//   - StartupMessage (length, version 196608, key/value pairs)
//   - SSLRequest (length=8, magic=80877103)
//   - GSSENCRequest (length=8, magic=80877104)
//   - CancelRequest (length=16, magic=80877102, pid, secret)
//
// After the startup phase completes, all messages use the regular [type][length][payload]
// framing in both directions.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
)

// PgPacket is a raw pgwire message in wire form.
//
// For regular messages: Type is the message type byte (e.g. 'Q'), Payload is
// [length:4][body], where length includes the 4-byte length field but NOT the
// type byte (matching the on-wire encoding).
//
// For startup-phase messages: Type is 0 and Payload is [length:4][body], identical
// to the on-wire form (no type byte exists).
type PgPacket struct {
	Type    byte
	Payload []byte
}

// Bytes returns the full on-wire representation of the packet.
func (p *PgPacket) Bytes() []byte {
	if p.Type == 0 {
		return p.Payload
	}
	out := make([]byte, 1+len(p.Payload))
	out[0] = p.Type
	copy(out[1:], p.Payload)
	return out
}

const (
	// pgMaxMessageSize bounds payload reads at 256 MiB to avoid unbounded
	// allocation on a malformed length header. Real queries are bounded
	// by Postgres's 1 GiB max, but no legitimate payload approaches 256 MiB
	// for our workload.
	pgMaxMessageSize = 1 << 28
)

// pgwire startup magic numbers.
const (
	pgMagicSSLRequest    uint32 = 80877103
	pgMagicCancelRequest uint32 = 80877102
	pgMagicGSSENCRequest uint32 = 80877104
)

// ReadStartupMessage reads the first message from a fresh client connection.
// It cannot use ReadMessage because the startup-phase messages have no type byte.
func ReadStartupMessage(r io.Reader) (*PgPacket, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header)
	if length < 8 || length > pgMaxMessageSize {
		return nil, fmt.Errorf("invalid startup length: %d", length)
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	full := make([]byte, 4+len(body))
	copy(full, header)
	copy(full[4:], body)
	return &PgPacket{Type: 0, Payload: full}, nil
}

// ReadMessage reads a regular pgwire message: [type:1][length:4][body].
// Used for all messages after the startup phase has completed.
func ReadMessage(r io.Reader) (*PgPacket, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	msgType := header[0]
	length := binary.BigEndian.Uint32(header[1:])
	if length < 4 || length > pgMaxMessageSize {
		return nil, fmt.Errorf("invalid pgwire length: %d for type %q", length, msgType)
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	payload := make([]byte, 4+len(body))
	copy(payload, header[1:])
	copy(payload[4:], body)
	return &PgPacket{Type: msgType, Payload: payload}, nil
}

// IsSSLRequest reports whether the startup packet is an SSLRequest.
func IsSSLRequest(p *PgPacket) bool {
	return p != nil && p.Type == 0 && len(p.Payload) == 8 &&
		binary.BigEndian.Uint32(p.Payload[4:]) == pgMagicSSLRequest
}

// IsGSSENCRequest reports whether the startup packet is a GSSENCRequest.
func IsGSSENCRequest(p *PgPacket) bool {
	return p != nil && p.Type == 0 && len(p.Payload) == 8 &&
		binary.BigEndian.Uint32(p.Payload[4:]) == pgMagicGSSENCRequest
}

// IsCancelRequest reports whether the startup packet is a CancelRequest.
func IsCancelRequest(p *PgPacket) bool {
	return p != nil && p.Type == 0 && len(p.Payload) == 16 &&
		binary.BigEndian.Uint32(p.Payload[4:]) == pgMagicCancelRequest
}

// pgwire frontend (client→server) message types.
const (
	pgMsgQuery        byte = 'Q' // simple query protocol
	pgMsgParse        byte = 'P' // extended: parse a statement
	pgMsgBind         byte = 'B' // extended: bind values to a parsed statement
	pgMsgExecute      byte = 'E' // extended: execute a bound portal
	pgMsgSync         byte = 'S' // extended: end of pipeline
	pgMsgTerminate    byte = 'X' // close connection
	pgMsgPasswordMsg  byte = 'p' // password / SASL response
	pgMsgClose        byte = 'C' // close (a portal/statement) — frontend-only
	pgMsgDescribe     byte = 'D' // describe a portal/statement
	pgMsgFlush        byte = 'H' // flush
	pgMsgCopyData     byte = 'd' // shared (frontend & backend)
	pgMsgCopyDone     byte = 'c' // shared
	pgMsgCopyFail     byte = 'f' // frontend-only
	pgMsgFunctionCall byte = 'F'
)

// extractPgQueryText returns the SQL text from a Query or Parse message.
// Returns "" for messages that don't carry SQL text (Bind, Execute, etc.).
//
// Wire layout for Query:   [length:4][sql:cstring]
// Wire layout for Parse:   [length:4][stmt_name:cstring][sql:cstring][n_params:int16][param_oids:int32*]
func extractPgQueryText(p *PgPacket) string {
	if p == nil || len(p.Payload) <= 4 {
		return ""
	}
	body := p.Payload[4:]
	switch p.Type {
	case pgMsgQuery:
		return string(trimNullTerminator(body))
	case pgMsgParse:
		// Skip statement name (first cstring), then read SQL (second cstring).
		idx := indexNull(body)
		if idx < 0 {
			return ""
		}
		rest := body[idx+1:]
		end := indexNull(rest)
		if end < 0 {
			return string(rest)
		}
		return string(rest[:end])
	}
	return ""
}

// extractPgParseStmtName returns the statement name from a Parse 'P' frame.
// Wire layout (after the 4-byte length prefix in p.Payload[0:4]):
//
//	stmt_name:cstring  query:cstring  n_params:int16  param_oids:int32*
//
// An unnamed (one-shot) Parse uses the empty string as the stmt name. Returns
// "" both for the unnamed case and for any malformed input — callers treat
// "" as "no sticky tracking" since unnamed statements are scoped to the next
// Bind anyway and cannot be referenced later.
func extractPgParseStmtName(p *PgPacket) string {
	if p == nil || p.Type != pgMsgParse || len(p.Payload) <= 4 {
		return ""
	}
	body := p.Payload[4:]
	end := indexNull(body)
	if end < 0 {
		return ""
	}
	return string(body[:end])
}

// extractPgBindStmtName returns the statement name referenced by a Bind 'B'
// frame. Wire layout:
//
//	portal:cstring  stmt_name:cstring  ...
//
// Same caveats as extractPgParseStmtName for the empty / malformed cases.
func extractPgBindStmtName(p *PgPacket) string {
	if p == nil || p.Type != pgMsgBind || len(p.Payload) <= 4 {
		return ""
	}
	body := p.Payload[4:]
	portalEnd := indexNull(body)
	if portalEnd < 0 || portalEnd+1 >= len(body) {
		return ""
	}
	rest := body[portalEnd+1:]
	end := indexNull(rest)
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

// extractPgCloseTarget returns (kind, name) for a Close 'C' frame.
// Wire layout:
//
//	kind:byte ('S'=statement, 'P'=portal)  name:cstring
//
// kind is 0 on malformed input. Callers only sticky-track statement closes;
// portal closes don't reference a Parse and don't affect shadow stmt state.
func extractPgCloseTarget(p *PgPacket) (byte, string) {
	if p == nil || p.Type != pgMsgClose || len(p.Payload) <= 5 {
		return 0, ""
	}
	body := p.Payload[4:]
	kind := body[0]
	rest := body[1:]
	end := indexNull(rest)
	if end < 0 {
		return kind, ""
	}
	return kind, string(rest[:end])
}

// extractPgDescribeTarget mirrors extractPgCloseTarget for Describe 'D' frames.
// Wire layout is identical (kind byte + cstring name).
func extractPgDescribeTarget(p *PgPacket) (byte, string) {
	if p == nil || p.Type != pgMsgDescribe || len(p.Payload) <= 5 {
		return 0, ""
	}
	body := p.Payload[4:]
	kind := body[0]
	rest := body[1:]
	end := indexNull(rest)
	if end < 0 {
		return kind, ""
	}
	return kind, string(rest[:end])
}

// pgFrontendCommandName returns a human-readable label for a frontend message type.
func pgFrontendCommandName(t byte) string {
	switch t {
	case pgMsgQuery:
		return "Query"
	case pgMsgParse:
		return "Parse"
	case pgMsgBind:
		return "Bind"
	case pgMsgExecute:
		return "Execute"
	case pgMsgSync:
		return "Sync"
	case pgMsgTerminate:
		return "Terminate"
	case pgMsgPasswordMsg:
		return "Password"
	case pgMsgClose:
		return "Close"
	case pgMsgDescribe:
		return "Describe"
	case pgMsgFlush:
		return "Flush"
	case pgMsgCopyData:
		return "CopyData"
	case pgMsgCopyDone:
		return "CopyDone"
	case pgMsgCopyFail:
		return "CopyFail"
	case pgMsgFunctionCall:
		return "FunctionCall"
	default:
		return fmt.Sprintf("Unknown(0x%02X)", t)
	}
}

// isPgCountableQuery reports whether a frontend message represents an actual
// SQL query (and should be counted toward queries_total).
func isPgCountableQuery(t byte) bool {
	switch t {
	case pgMsgQuery, pgMsgParse, pgMsgExecute:
		return true
	}
	return false
}

func indexNull(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

func trimNullTerminator(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == 0 {
		return b[:n-1]
	}
	return b
}
