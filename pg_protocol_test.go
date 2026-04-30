package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildStartupMsg constructs a v3 StartupMessage with the given key/value pairs.
func buildStartupMsg(kv map[string]string) []byte {
	body := make([]byte, 4) // version placeholder
	binary.BigEndian.PutUint32(body, 196608)
	for k, v := range kv {
		body = append(body, []byte(k)...)
		body = append(body, 0)
		body = append(body, []byte(v)...)
		body = append(body, 0)
	}
	body = append(body, 0) // trailing null terminator

	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(4+len(body)))
	copy(out[4:], body)
	return out
}

func buildSSLRequest() []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out, 8)
	binary.BigEndian.PutUint32(out[4:], pgMagicSSLRequest)
	return out
}

func buildCancelRequest(pid, secret uint32) []byte {
	out := make([]byte, 16)
	binary.BigEndian.PutUint32(out, 16)
	binary.BigEndian.PutUint32(out[4:], pgMagicCancelRequest)
	binary.BigEndian.PutUint32(out[8:], pid)
	binary.BigEndian.PutUint32(out[12:], secret)
	return out
}

// buildMessage constructs a regular pgwire message.
func buildMessage(msgType byte, body []byte) []byte {
	out := make([]byte, 5+len(body))
	out[0] = msgType
	binary.BigEndian.PutUint32(out[1:], uint32(4+len(body)))
	copy(out[5:], body)
	return out
}

func TestReadStartupMessage(t *testing.T) {
	raw := buildStartupMsg(map[string]string{"user": "anand", "database": "trm"})
	pkt, err := ReadStartupMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStartupMessage: %v", err)
	}
	if pkt.Type != 0 {
		t.Errorf("expected Type=0 for startup, got %d", pkt.Type)
	}
	if !bytes.Equal(pkt.Payload, raw) {
		t.Errorf("payload mismatch: got %x want %x", pkt.Payload, raw)
	}
}

func TestIsSSLRequest(t *testing.T) {
	raw := buildSSLRequest()
	pkt, err := ReadStartupMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStartupMessage: %v", err)
	}
	if !IsSSLRequest(pkt) {
		t.Errorf("expected IsSSLRequest=true")
	}
	if IsCancelRequest(pkt) {
		t.Errorf("expected IsCancelRequest=false for SSLRequest")
	}
}

func TestIsCancelRequest(t *testing.T) {
	raw := buildCancelRequest(1234, 5678)
	pkt, err := ReadStartupMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStartupMessage: %v", err)
	}
	if !IsCancelRequest(pkt) {
		t.Errorf("expected IsCancelRequest=true")
	}
}

func TestReadMessage(t *testing.T) {
	body := append([]byte("SELECT 1"), 0)
	raw := buildMessage(pgMsgQuery, body)
	pkt, err := ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if pkt.Type != pgMsgQuery {
		t.Errorf("expected Type='Q', got %c", pkt.Type)
	}
	if !bytes.Equal(pkt.Bytes(), raw) {
		t.Errorf("round-trip mismatch: got %x want %x", pkt.Bytes(), raw)
	}
}

func TestReadMessageRejectsOversized(t *testing.T) {
	// 5-byte header announcing a payload larger than pgMaxMessageSize.
	raw := []byte{pgMsgQuery, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := ReadMessage(bytes.NewReader(raw)); err == nil {
		t.Errorf("expected error for oversized message, got nil")
	}
}

func TestExtractPgQueryTextQuery(t *testing.T) {
	body := append([]byte("SELECT * FROM t"), 0)
	raw := buildMessage(pgMsgQuery, body)
	pkt, err := ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got := extractPgQueryText(pkt); got != "SELECT * FROM t" {
		t.Errorf("Query: got %q want %q", got, "SELECT * FROM t")
	}
}

func TestExtractPgQueryTextParse(t *testing.T) {
	// Parse: [stmt_name:cstring][sql:cstring][n_params:int16][param_oids:int32*]
	body := []byte{}
	body = append(body, []byte("stmt1")...)
	body = append(body, 0)
	body = append(body, []byte("SELECT $1")...)
	body = append(body, 0)
	body = append(body, 0, 0) // n_params=0
	raw := buildMessage(pgMsgParse, body)
	pkt, err := ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got := extractPgQueryText(pkt); got != "SELECT $1" {
		t.Errorf("Parse: got %q want %q", got, "SELECT $1")
	}
}

func TestExtractPgQueryTextNonQueryReturnsEmpty(t *testing.T) {
	pkt := &PgPacket{Type: pgMsgBind, Payload: []byte{0, 0, 0, 4}}
	if got := extractPgQueryText(pkt); got != "" {
		t.Errorf("Bind: got %q want empty", got)
	}
}

func TestPgFrontendCommandName(t *testing.T) {
	cases := map[byte]string{
		pgMsgQuery:     "Query",
		pgMsgParse:     "Parse",
		pgMsgBind:      "Bind",
		pgMsgExecute:   "Execute",
		pgMsgTerminate: "Terminate",
		0xAB:           "Unknown(0xAB)",
	}
	for typ, want := range cases {
		if got := pgFrontendCommandName(typ); got != want {
			t.Errorf("%c: got %q want %q", typ, got, want)
		}
	}
}
