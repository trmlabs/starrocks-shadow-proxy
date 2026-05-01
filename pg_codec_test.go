package main

import (
	"bytes"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestReadPgStartupPacketPreservesRawSSLRequest(t *testing.T) {
	raw, err := (&pgproto3.SSLRequest{}).Encode(nil)
	if err != nil {
		t.Fatalf("encode SSLRequest: %v", err)
	}

	packet, err := readPgStartupPacket(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("readPgStartupPacket: %v", err)
	}

	if _, ok := packet.Message.(*pgproto3.SSLRequest); !ok {
		t.Fatalf("message type = %T, want *pgproto3.SSLRequest", packet.Message)
	}
	if !bytes.Equal(packet.Raw, raw) {
		t.Fatalf("raw bytes changed: got %x want %x", packet.Raw, raw)
	}
}

func TestReadPgFrontendPacketDecodesParseQueryText(t *testing.T) {
	raw, err := (&pgproto3.Parse{
		Name:          "stmt1",
		Query:         "select $1::int + 1",
		ParameterOIDs: []uint32{23},
	}).Encode(nil)
	if err != nil {
		t.Fatalf("encode Parse: %v", err)
	}

	packet, err := readPgFrontendPacket(bytes.NewReader(raw), 0)
	if err != nil {
		t.Fatalf("readPgFrontendPacket: %v", err)
	}

	msg, ok := packet.Message.(*pgproto3.Parse)
	if !ok {
		t.Fatalf("message type = %T, want *pgproto3.Parse", packet.Message)
	}
	if msg.Query != "select $1::int + 1" {
		t.Fatalf("decoded query = %q", msg.Query)
	}
	if packet.Command != "Parse" {
		t.Fatalf("command = %q, want Parse", packet.Command)
	}
	if packet.QueryText != "select $1::int + 1" {
		t.Fatalf("query text = %q", packet.QueryText)
	}
	if !bytes.Equal(packet.Raw, raw) {
		t.Fatalf("raw bytes changed: got %x want %x", packet.Raw, raw)
	}
}

func TestReadPgBackendPacketIdentifiesReadyForQuery(t *testing.T) {
	raw, err := (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(nil)
	if err != nil {
		t.Fatalf("encode ReadyForQuery: %v", err)
	}

	packet, err := readPgBackendPacket(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("readPgBackendPacket: %v", err)
	}

	if _, ok := packet.Message.(*pgproto3.ReadyForQuery); !ok {
		t.Fatalf("message type = %T, want *pgproto3.ReadyForQuery", packet.Message)
	}
	if !packet.ReadyForQuery {
		t.Fatalf("ReadyForQuery flag = false, want true")
	}
	if packet.CopyMode {
		t.Fatalf("CopyMode flag = true, want false")
	}
	if !bytes.Equal(packet.Raw, raw) {
		t.Fatalf("raw bytes changed: got %x want %x", packet.Raw, raw)
	}
}
