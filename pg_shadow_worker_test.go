package main

import (
	"errors"
	"net"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// failOnWriteConn is a minimal net.Conn that returns an error on every Write.
// Used to drive PgShadowWorker.processFrame down its dead-flag path without
// touching a real backend.
type failOnWriteConn struct {
	closed bool
}

func (f *failOnWriteConn) Read(b []byte) (int, error)         { return 0, errors.New("read not used") }
func (f *failOnWriteConn) Write(b []byte) (int, error)        { return 0, errors.New("write boom") }
func (f *failOnWriteConn) Close() error                       { f.closed = true; return nil }
func (f *failOnWriteConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (f *failOnWriteConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (f *failOnWriteConn) SetDeadline(t time.Time) error      { return nil }
func (f *failOnWriteConn) SetReadDeadline(t time.Time) error  { return nil }
func (f *failOnWriteConn) SetWriteDeadline(t time.Time) error { return nil }

// counterValue reads the current value of a labeled counter for assertions.
func counterValue(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter write: %v", err)
	}
	if m.Counter == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// TestPgShadowWorker_DeadFlagOnWriteError exercises the conn-dead path:
//   - First Send queues a frame (worker has not yet latched dead).
//   - The worker dequeues it, the fake conn errors on Write, processFrame
//     latches dead and closes the conn.
//   - A second Send returns false and increments
//     shadow_proxy_shadow_dropped_total{reason="conn_dead"}.
func TestPgShadowWorker_DeadFlagOnWriteError(t *testing.T) {
	cfg := &Config{
		ShadowQueueSize:    8,
		ShadowReadTimeout:  100 * time.Millisecond,
		ShadowDrainTimeout: 50 * time.Millisecond,
	}
	w := NewPgShadowWorker(cfg, nil)
	w.conn = &failOnWriteConn{}

	// processFrame indexes frame.payload[0] for the command name — give it a
	// real frontend byte (Query) so the test crashes on bad input, not nil.
	frame := pgShadowFrame{
		req:     QueryRequest{Command: "Query", QueryText: "SELECT 1"},
		payload: []byte{pgMsgQuery, 0, 0, 0, 4},
	}

	// Drive processFrame directly — no worker() goroutine to coordinate with.
	w.processFrame(frame)

	if !w.dead.Load() {
		t.Fatal("expected dead flag set after write error")
	}

	dropCounter := shadowDropped.WithLabelValues("conn_dead")
	before := counterValue(t, dropCounter)

	// Send while dead should fail-fast and increment the conn_dead counter.
	if w.Send(frame) {
		t.Fatal("Send should return false when worker is dead")
	}
	after := counterValue(t, dropCounter)
	if got, want := after-before, 1.0; got != want {
		t.Errorf("shadowDropped{conn_dead} delta = %v, want %v", got, want)
	}

	// processFrame on a second invocation should short-circuit too — verify
	// the counter ticks again without panicking on the closed conn.
	w.processFrame(frame)
	if got, want := counterValue(t, dropCounter)-before, 2.0; got != want {
		t.Errorf("shadowDropped{conn_dead} after second processFrame = %v, want %v", got, want)
	}
}

// TestPgShadowWorker_MarkDeadIdempotent confirms markDead is safe to call
// repeatedly. The first call closes the conn; subsequent calls are no-ops.
func TestPgShadowWorker_MarkDeadIdempotent(t *testing.T) {
	fc := &failOnWriteConn{}
	w := NewPgShadowWorker(&Config{ShadowQueueSize: 1}, nil)
	w.conn = fc

	w.markDead()
	if !w.dead.Load() {
		t.Fatal("dead flag not set after first markDead")
	}
	if !fc.closed {
		t.Fatal("conn not closed by first markDead")
	}

	// Calling again should be a no-op (no double-close, no panic).
	w.markDead()
	if !w.dead.Load() {
		t.Fatal("dead flag unexpectedly cleared")
	}
}
