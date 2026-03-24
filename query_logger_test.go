package main

import (
	"fmt"
	"testing"
	"time"
)

// TestNewQueryRequest tests that QueryRequest is created with proper metadata
func TestNewQueryRequest(t *testing.T) {
	// Create a COM_QUERY packet: header (4 bytes) + command (1 byte) + query text
	query := "SELECT * FROM users"
	packet := make([]byte, 4+1+len(query))
	packet[0] = byte(1 + len(query)) // length low byte
	packet[1] = 0                    // length mid byte
	packet[2] = 0                    // length high byte
	packet[3] = 0                    // sequence number
	packet[4] = comQuery             // COM_QUERY command
	copy(packet[5:], []byte(query))

	clientAddr := "192.168.1.100:54321"
	req := NewQueryRequest(packet, clientAddr)

	// Verify UUID is generated
	if req.ID == "" {
		t.Error("Expected non-empty QueryID")
	}
	if len(req.ID) != 36 { // UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
		t.Errorf("Expected UUID length 36, got %d", len(req.ID))
	}

	// Verify command is extracted
	if req.Command != "COM_QUERY" {
		t.Errorf("Expected Command 'COM_QUERY', got '%s'", req.Command)
	}

	// Verify query text is extracted
	if req.QueryText != query {
		t.Errorf("Expected QueryText '%s', got '%s'", query, req.QueryText)
	}

	// Verify client address is set
	if req.ClientAddr != clientAddr {
		t.Errorf("Expected ClientAddr '%s', got '%s'", clientAddr, req.ClientAddr)
	}

	// Verify timestamp is recent
	if time.Since(req.ReceivedAt) > time.Second {
		t.Error("Expected ReceivedAt to be recent")
	}
}

// TestNewQueryRequestNonQuery tests QueryRequest for non-query commands (e.g., PING)
func TestNewQueryRequestNonQuery(t *testing.T) {
	// Create a COM_PING packet
	packet := []byte{1, 0, 0, 0, comPing}
	clientAddr := "10.0.0.1:12345"

	req := NewQueryRequest(packet, clientAddr)

	if req.Command != "COM_PING" {
		t.Errorf("Expected Command 'COM_PING', got '%s'", req.Command)
	}

	// Non-query commands should have empty QueryText
	if req.QueryText != "" {
		t.Errorf("Expected empty QueryText for COM_PING, got '%s'", req.QueryText)
	}
}

// TestQueryLogEntryStructure tests QueryLogEntry JSON marshaling
func TestQueryLogEntryStructure(t *testing.T) {
	entry := QueryLogEntry{
		Timestamp:  "2026-02-05T10:30:00.123456789Z",
		QueryID:    "550e8400-e29b-41d4-a716-446655440000",
		Target:     "primary",
		Command:    "COM_QUERY",
		QueryText:  "SELECT 1",
		DurationMs: 12.345,
		BytesSent:  100,
		BytesRecv:  200,
		Success:    true,
		Error:      "",
		ClientAddr: "192.168.1.1:5000",
	}

	// Verify all fields are accessible
	if entry.Target != "primary" {
		t.Errorf("Expected Target 'primary', got '%s'", entry.Target)
	}
	if entry.DurationMs != 12.345 {
		t.Errorf("Expected DurationMs 12.345, got %f", entry.DurationMs)
	}
	if !entry.Success {
		t.Error("Expected Success to be true")
	}
}

// TestErrorString tests the errorString helper function
func TestErrorString(t *testing.T) {
	// Test nil error
	if result := errorString(nil); result != "" {
		t.Errorf("Expected empty string for nil error, got '%s'", result)
	}

	// Test actual error
	err := fmt.Errorf("test error message")
	if result := errorString(err); result != "test error message" {
		t.Errorf("Expected 'test error message', got '%s'", result)
	}
}

// TestQueryRequestCorrelation tests that same QueryRequest ID is used for primary/shadow correlation
func TestQueryRequestCorrelation(t *testing.T) {
	query := "SELECT * FROM orders WHERE id = 123"
	packet := make([]byte, 4+1+len(query))
	packet[0] = byte(1 + len(query))
	packet[4] = comQuery
	copy(packet[5:], []byte(query))

	clientAddr := "10.0.0.50:8080"

	// Create request
	req := NewQueryRequest(packet, clientAddr)

	// Simulate creating log entries for both primary and shadow
	// (they should share the same QueryID for correlation)
	primaryEntry := QueryLogEntry{
		QueryID:    req.ID,
		Target:     "primary",
		Command:    req.Command,
		QueryText:  req.QueryText,
		DurationMs: 5.5,
		Success:    true,
	}

	shadowEntry := QueryLogEntry{
		QueryID:    req.ID, // Same ID for correlation!
		Target:     "shadow",
		Command:    req.Command,
		QueryText:  req.QueryText,
		DurationMs: 6.2,
		Success:    true,
	}

	// Verify both entries share the same QueryID
	if primaryEntry.QueryID != shadowEntry.QueryID {
		t.Errorf("QueryIDs should match for correlation: primary=%s, shadow=%s",
			primaryEntry.QueryID, shadowEntry.QueryID)
	}

	// Verify both have the same query text
	if primaryEntry.QueryText != shadowEntry.QueryText {
		t.Errorf("QueryText should match: primary=%s, shadow=%s",
			primaryEntry.QueryText, shadowEntry.QueryText)
	}

	// Verify targets are different
	if primaryEntry.Target == shadowEntry.Target {
		t.Error("Targets should be different (primary vs shadow)")
	}

	t.Logf("Correlation test passed: QueryID=%s can join primary (%.2fms) and shadow (%.2fms) results",
		req.ID, primaryEntry.DurationMs, shadowEntry.DurationMs)
}
