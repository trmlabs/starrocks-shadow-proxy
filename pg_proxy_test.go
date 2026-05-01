package main

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestPgProxyForwardsFlushResponseBeforeSync(t *testing.T) {
	primaryAddr, primaryDone := startPgPrimaryMock(t, func(t *testing.T, conn net.Conn) {
		expectPgStartup(t, conn)
		writePgBackendMessages(t, conn,
			&pgproto3.AuthenticationOk{},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		)

		parse := readFrontendForTest(t, conn)
		if _, ok := parse.Message.(*pgproto3.Parse); !ok {
			t.Fatalf("primary got %T, want Parse", parse.Message)
		}
		flush := readFrontendForTest(t, conn)
		if _, ok := flush.Message.(*pgproto3.Flush); !ok {
			t.Fatalf("primary got %T, want Flush", flush.Message)
		}
		writePgBackendMessages(t, conn, &pgproto3.ParseComplete{})

		syncMsg := readFrontendForTest(t, conn)
		if _, ok := syncMsg.Message.(*pgproto3.Sync); !ok {
			t.Fatalf("primary got %T, want Sync", syncMsg.Message)
		}
		writePgBackendMessages(t, conn, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	})

	client, proxySide := net.Pipe()
	defer client.Close()

	proxy := NewPgProxy(&Config{PrimaryHost: "127.0.0.1", PrimaryPort: primaryAddr.Port})
	go proxy.handleConnection(proxySide, nil)

	writePgFrontendMessages(t, client, &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "postgres", "database": "trm"},
	})
	readBackendUntilReady(t, client)

	writePgFrontendMessages(t, client,
		&pgproto3.Parse{Name: "stmt1", Query: "select 1"},
		&pgproto3.Flush{},
	)
	msg := readBackendForTest(t, client)
	if _, ok := msg.Message.(*pgproto3.ParseComplete); !ok {
		t.Fatalf("client got %T, want ParseComplete before Sync", msg.Message)
	}

	writePgFrontendMessages(t, client, &pgproto3.Sync{})
	readBackendUntilReady(t, client)
	_ = client.Close()
	waitForPrimaryMock(t, primaryDone)
}

func TestPgProxyForwardsNoticeWhileClientIdle(t *testing.T) {
	primaryAddr, primaryDone := startPgPrimaryMock(t, func(t *testing.T, conn net.Conn) {
		expectPgStartup(t, conn)
		writePgBackendMessages(t, conn,
			&pgproto3.AuthenticationOk{},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
			(*pgproto3.NoticeResponse)(&pgproto3.ErrorResponse{
				Severity: "NOTICE",
				Code:     "00000",
				Message:  "hello from primary",
			}),
		)
	})

	client, proxySide := net.Pipe()
	defer client.Close()

	proxy := NewPgProxy(&Config{PrimaryHost: "127.0.0.1", PrimaryPort: primaryAddr.Port})
	go proxy.handleConnection(proxySide, nil)

	writePgFrontendMessages(t, client, &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "postgres", "database": "trm"},
	})
	readBackendUntilReady(t, client)

	msg := readBackendForTest(t, client)
	notice, ok := msg.Message.(*pgproto3.NoticeResponse)
	if !ok {
		t.Fatalf("client got %T, want NoticeResponse while idle", msg.Message)
	}
	if notice.Message != "hello from primary" {
		t.Fatalf("notice message = %q", notice.Message)
	}
	_ = client.Close()
	waitForPrimaryMock(t, primaryDone)
}

type pgPrimaryAddr struct {
	Host string
	Port string
}

func startPgPrimaryMock(t *testing.T, handler func(*testing.T, net.Conn)) (pgPrimaryAddr, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen primary mock: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(t, conn)
	}()
	t.Cleanup(func() { _ = ln.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split primary mock addr: %v", err)
	}
	return pgPrimaryAddr{Host: "127.0.0.1", Port: port}, done
}

func waitForPrimaryMock(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("primary mock did not finish")
	}
}

func expectPgStartup(t *testing.T, conn net.Conn) {
	t.Helper()
	startup, err := readPgStartupPacket(conn)
	if err != nil {
		t.Fatalf("primary read startup: %v", err)
	}
	if _, ok := startup.Message.(*pgproto3.StartupMessage); !ok {
		t.Fatalf("primary got startup %T, want StartupMessage", startup.Message)
	}
}

func writePgFrontendMessages(t *testing.T, conn io.Writer, messages ...pgproto3.FrontendMessage) {
	t.Helper()
	for _, msg := range messages {
		raw, err := msg.Encode(nil)
		if err != nil {
			t.Fatalf("encode frontend %T: %v", msg, err)
		}
		if _, err := conn.Write(raw); err != nil {
			t.Fatalf("write frontend %T: %v", msg, err)
		}
	}
}

func writePgBackendMessages(t *testing.T, conn io.Writer, messages ...pgproto3.BackendMessage) {
	t.Helper()
	for _, msg := range messages {
		raw, err := msg.Encode(nil)
		if err != nil {
			t.Fatalf("encode backend %T: %v", msg, err)
		}
		if _, err := conn.Write(raw); err != nil {
			t.Fatalf("write backend %T: %v", msg, err)
		}
	}
}

func readFrontendForTest(t *testing.T, conn net.Conn) *pgFrontendPacket {
	t.Helper()
	packet, err := readPgFrontendPacket(conn, 0)
	if err != nil {
		t.Fatalf("read frontend: %v", err)
	}
	return packet
}

func readBackendForTest(t *testing.T, conn net.Conn) *pgBackendPacket {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer conn.SetReadDeadline(time.Time{})
	packet, err := readPgBackendPacket(conn)
	if err != nil {
		t.Fatalf("read backend: %v", err)
	}
	return packet
}

func readBackendUntilReady(t *testing.T, conn net.Conn) {
	t.Helper()
	for {
		packet := readBackendForTest(t, conn)
		if packet.ReadyForQuery {
			return
		}
	}
}

func TestWritePgFrontendMessagesRequiresStartupRawRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	writePgFrontendMessages(t, &buf, &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "postgres"},
	})
	packet, err := readPgStartupPacket(&buf)
	if err != nil {
		t.Fatalf("read startup round trip: %v", err)
	}
	if _, ok := packet.Message.(*pgproto3.StartupMessage); !ok {
		t.Fatalf("round-trip message = %T", packet.Message)
	}
}
