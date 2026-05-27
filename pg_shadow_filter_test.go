package main

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// buildParseFrame constructs a wire-format Parse 'P' frame with the given
// statement name and query text. Wire layout (after the length prefix):
//
//	stmt_name:cstring  query:cstring  n_params:int16
//
// Used by the sticky-stmt-name tests to drive shouldMirrorPgFrame directly.
func buildParseFrame(stmtName, query string) *PgPacket {
	body := []byte(stmtName)
	body = append(body, 0)
	body = append(body, []byte(query)...)
	body = append(body, 0)
	body = append(body, 0, 0) // n_params = 0
	payload := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(payload[0:4], uint32(4+len(body)))
	copy(payload[4:], body)
	return &PgPacket{Type: pgMsgParse, Payload: payload}
}

// buildBindFrame constructs a Bind 'B' frame referencing the given portal
// and statement name. Wire layout: portal:cstring stmt_name:cstring then a
// 4-byte tail (n_param_formats=0, n_params=0) which we pad — extractPgBindStmtName
// only needs the first two cstrings.
func buildBindFrame(portal, stmtName string) *PgPacket {
	body := []byte(portal)
	body = append(body, 0)
	body = append(body, []byte(stmtName)...)
	body = append(body, 0)
	body = append(body, 0, 0, 0, 0) // n_param_formats=0, n_params=0
	payload := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(payload[0:4], uint32(4+len(body)))
	copy(payload[4:], body)
	return &PgPacket{Type: pgMsgBind, Payload: payload}
}

// buildExecuteFrame constructs an Execute 'E' frame referencing the given
// portal. extractPgQueryText doesn't apply to Execute so we don't need a
// dedicated stmt-name helper — Execute is dropped only via sticky tracking
// upstream of the Bind that referenced the filtered Parse.
func buildExecuteFrame(portal string) *PgPacket {
	body := []byte(portal)
	body = append(body, 0)
	body = append(body, 0, 0, 0, 0) // max_rows = 0
	payload := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(payload[0:4], uint32(4+len(body)))
	copy(payload[4:], body)
	return &PgPacket{Type: pgMsgExecute, Payload: payload}
}

// reqForParse fabricates the QueryRequest that runQueryLoop would build for a
// given Parse frame, including the extracted SQL text.
func reqForParse(p *PgPacket) QueryRequest {
	return QueryRequest{
		Command:   "Parse",
		QueryText: extractPgQueryText(p),
	}
}

// TestShouldShadowMirror_NilFilterAlwaysAllows asserts a nil filter mirrors
// every frame — this is the default (no SHADOW_FILTER_* / SHADOW_SAMPLE_RATE
// configured).
func TestShouldShadowMirror_NilFilterAlwaysAllows(t *testing.T) {
	req := QueryRequest{Command: "Query", QueryText: "SELECT 1"}
	allowed, reason := shouldShadowMirror(req, nil)
	if !allowed {
		t.Fatalf("nil filter should always allow, got allowed=false reason=%q", reason)
	}
	if reason != FilterReasonNone {
		t.Errorf("expected reason=%q, got %q", FilterReasonNone, reason)
	}
}

// TestShouldShadowMirror_SampleRateZeroIsDeterministicOnPgPath asserts the
// pgwire shouldShadowMirror is deterministic w.r.t. sampling — at rate=0 every
// frame still passes the per-frame check, because sampling is now applied once
// per CONNECTION in PgProxy.startShadowWorker (not per frame). The MySQL path
// continues to call QueryFilter.Allow directly, which still rolls per frame.
func TestShouldShadowMirror_SampleRateZeroIsDeterministicOnPgPath(t *testing.T) {
	f, err := NewQueryFilter(&Config{ShadowSampleRate: 0.0})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil filter for sample_rate=0")
	}

	req := QueryRequest{Command: "Query", QueryText: "SELECT 1"}
	for i := 0; i < 100; i++ {
		allowed, reason := shouldShadowMirror(req, f)
		if !allowed {
			t.Fatalf("iter %d: per-frame sampling should not drop here, got allowed=false reason=%q", i, reason)
		}
	}

	if got := f.SampleRate(); got != 0.0 {
		t.Errorf("SampleRate() = %v, want 0.0 (per-connection roll uses this)", got)
	}
}

// TestShouldShadowMirror_SampleRateOneAlwaysSends covers SHADOW_SAMPLE_RATE=1.0
// alongside an active SHADOW_FILTER_MODE — no random drops should occur.
func TestShouldShadowMirror_SampleRateOneAlwaysSends(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode: "include",
		// Include-mode with no criteria + sample_rate=1 is degenerate but
		// constructible (NewQueryFilter logs a warning, returns a filter that
		// matches everything). The point of the test is that nothing in the
		// sampling path drops frames at rate=1.
		ShadowSampleRate: 1.0,
	})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if f == nil {
		t.Fatal("expected non-nil filter with mode set")
	}

	req := QueryRequest{Command: "Query", QueryText: "SELECT 1"}
	for i := 0; i < 100; i++ {
		allowed, _ := shouldShadowMirror(req, f)
		if !allowed {
			t.Fatalf("iter %d: sample_rate=1 with no criteria should always send, got allowed=false", i)
		}
	}
}

// TestShouldShadowMirror_ExcludePatternSkipsMatch covers SHADOW_FILTER_MODE=exclude
// with a SHADOW_FILTER_PATTERNS that matches the query.
func TestShouldShadowMirror_ExcludePatternSkipsMatch(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "exclude",
		ShadowFilterPatterns: []string{`(?i)pg_catalog\.`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}

	match := QueryRequest{Command: "Query", QueryText: "SELECT * FROM pg_catalog.pg_class"}
	allowed, reason := shouldShadowMirror(match, f)
	if allowed {
		t.Errorf("exclude pattern should block matching query, got allowed=true")
	}
	if reason != FilterReasonPattern {
		t.Errorf("expected reason=%q, got %q", FilterReasonPattern, reason)
	}

	// A non-matching query should still flow through.
	pass := QueryRequest{Command: "Query", QueryText: "SELECT id FROM addresses LIMIT 1"}
	allowed, reason = shouldShadowMirror(pass, f)
	if !allowed {
		t.Errorf("exclude pattern should allow non-matching query, got allowed=false reason=%q", reason)
	}
}

// TestShouldShadowMirror_IncludePatternSkipsNonMatch covers SHADOW_FILTER_MODE=include
// with a SHADOW_FILTER_PATTERNS — only queries matching the pattern should mirror.
func TestShouldShadowMirror_IncludePatternSkipsNonMatch(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{`(?i)FROM\s+addresses`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}

	// Pattern matches → allowed.
	match := QueryRequest{Command: "Query", QueryText: "SELECT id FROM addresses LIMIT 1"}
	allowed, _ := shouldShadowMirror(match, f)
	if !allowed {
		t.Error("include pattern should allow matching query, got allowed=false")
	}

	// Pattern doesn't match → blocked with FilterReasonPattern.
	nonMatch := QueryRequest{Command: "Query", QueryText: "SELECT now()"}
	allowed, reason := shouldShadowMirror(nonMatch, f)
	if allowed {
		t.Errorf("include pattern should skip non-matching query, got allowed=true")
	}
	if reason != FilterReasonPattern {
		t.Errorf("expected reason=%q, got %q", FilterReasonPattern, reason)
	}
}

// TestShouldShadowMirror_NonSQLFrameAlwaysAllowed asserts that pg frames with no
// SQL text (Bind, Execute, Sync, etc.) always pass through, even with a
// restrictive filter — required to keep the shadow's prepared-statement state
// in sync with the primary.
func TestShouldShadowMirror_NonSQLFrameAlwaysAllowed(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"SELECT"},
		ShadowSampleRate:          0.0, // Would block everything if applied.
	})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}

	for _, cmd := range []string{"Bind", "Execute", "Sync", "Describe", "Close", "Flush"} {
		t.Run(cmd, func(t *testing.T) {
			req := QueryRequest{Command: cmd, QueryText: ""}
			allowed, reason := shouldShadowMirror(req, f)
			if !allowed {
				t.Errorf("%s frame should always pass through, got allowed=false reason=%q", cmd, reason)
			}
		})
	}
}

// --- Per-connection sampling tests (fix #2) ---

// TestPerConnectionSamplingZero asserts SampleRate()=0 — at the connection
// level, startShadowWorker uses rand.Float64() >= rate to decide. rate=0 means
// every connection loses the roll. We don't construct PgProxy here (network
// listener); instead we exercise the deterministic surface: AllowDeterministic
// must still allow per-frame, SampleRate must return 0 so startShadowWorker
// can route the connection primary-only.
func TestPerConnectionSamplingZero(t *testing.T) {
	f, err := NewQueryFilter(&Config{ShadowSampleRate: 0.0})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if f.SampleRate() != 0.0 {
		t.Errorf("SampleRate() = %v, want 0.0", f.SampleRate())
	}
	// AllowDeterministic ignores sampling — must return allowed.
	req := QueryRequest{Command: "Query", QueryText: "SELECT 1"}
	allowed, reason := f.AllowDeterministic(req)
	if !allowed {
		t.Errorf("AllowDeterministic should ignore sampling, got allowed=false reason=%q", reason)
	}
}

// TestPerConnectionSamplingOne asserts SampleRate()=1 — startShadowWorker
// never drops the connection. Combined with the AllowDeterministic path,
// every SQL-carrying frame on every connection ships.
func TestPerConnectionSamplingOne(t *testing.T) {
	// No filter constructed at all (default rate=1.0 with no criteria).
	f, err := NewQueryFilter(&Config{ShadowSampleRate: 1.0})
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	if f != nil {
		// If NewQueryFilter returns non-nil (because some other criteria
		// is set), confirm the rate is reported as 1.0.
		if f.SampleRate() != 1.0 {
			t.Errorf("SampleRate() = %v, want 1.0", f.SampleRate())
		}
	} else {
		// Nil filter == mirror everything. Confirm the helper on a nil
		// receiver still reports 1.0 (used by startShadowWorker before
		// it even dereferences p.queryFilter).
		var nilFilter *QueryFilter
		if got := nilFilter.SampleRate(); got != 1.0 {
			t.Errorf("nil filter SampleRate() = %v, want 1.0", got)
		}
	}
}

// TestStickyByStmtName_PatternFilter exercises the load-bearing sticky-stmt
// path: a Parse for "S_42" matches an exclude pattern → it's filtered AND
// "S_42" is added to filteredStmtNames. Subsequent Bind/Execute/Describe/Close
// referencing "S_42" must also be filtered with reason="sticky_stmt". A
// separate Parse for "S_43" (non-matching) passes through and its Bind ships.
func TestStickyByStmtName_PatternFilter(t *testing.T) {
	cfg := &Config{
		ShadowFilterMode:     "exclude",
		ShadowFilterPatterns: []string{`(?i)pg_catalog\.`},
		ShadowSampleRate:     1.0,
	}
	f, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	p := &PgProxy{config: &Config{}, queryFilter: f}
	filtered := map[string]struct{}{}

	// Parse S_42 — matches exclude pattern → filtered. Stmt name tracked.
	parse42 := buildParseFrame("S_42", "SELECT * FROM pg_catalog.pg_class")
	allowed, reason := p.shouldMirrorPgFrame(parse42, reqForParse(parse42), filtered)
	if allowed {
		t.Fatal("Parse S_42 (pg_catalog match) should be filtered")
	}
	if reason != FilterReasonPattern {
		t.Errorf("Parse S_42 reason = %q, want %q", reason, FilterReasonPattern)
	}
	if _, ok := filtered["S_42"]; !ok {
		t.Fatal("expected S_42 to be tracked in filteredStmtNames")
	}

	// Bind referencing S_42 → must be dropped as sticky_stmt.
	bind42 := buildBindFrame("portal_a", "S_42")
	allowed, reason = p.shouldMirrorPgFrame(bind42, QueryRequest{Command: "Bind"}, filtered)
	if allowed {
		t.Fatal("Bind for filtered S_42 should be dropped")
	}
	if reason != FilterReasonStickyStmt {
		t.Errorf("Bind sticky reason = %q, want %q", reason, FilterReasonStickyStmt)
	}

	// Describe('S', "S_42") → also dropped.
	descBody := append([]byte{'S'}, []byte("S_42\x00")...)
	descPayload := make([]byte, 4+len(descBody))
	binary.BigEndian.PutUint32(descPayload[0:4], uint32(4+len(descBody)))
	copy(descPayload[4:], descBody)
	describe42 := &PgPacket{Type: pgMsgDescribe, Payload: descPayload}
	allowed, reason = p.shouldMirrorPgFrame(describe42, QueryRequest{Command: "Describe"}, filtered)
	if allowed {
		t.Fatal("Describe for filtered S_42 should be dropped")
	}
	if reason != FilterReasonStickyStmt {
		t.Errorf("Describe sticky reason = %q, want %q", reason, FilterReasonStickyStmt)
	}

	// Parse S_43 — does NOT match exclude → passes through. Bind on S_43
	// must also pass.
	parse43 := buildParseFrame("S_43", "SELECT id FROM addresses LIMIT 1")
	allowed, _ = p.shouldMirrorPgFrame(parse43, reqForParse(parse43), filtered)
	if !allowed {
		t.Fatal("Parse S_43 (non-matching) should pass through")
	}
	if _, ok := filtered["S_43"]; ok {
		t.Fatal("S_43 should NOT be in filteredStmtNames")
	}
	bind43 := buildBindFrame("portal_b", "S_43")
	allowed, _ = p.shouldMirrorPgFrame(bind43, QueryRequest{Command: "Bind"}, filtered)
	if !allowed {
		t.Fatal("Bind for non-filtered S_43 should pass through")
	}

	// Close('S', "S_42") — sticky, drop AND remove tracking entry.
	closeBody := append([]byte{'S'}, []byte("S_42\x00")...)
	closePayload := make([]byte, 4+len(closeBody))
	binary.BigEndian.PutUint32(closePayload[0:4], uint32(4+len(closeBody)))
	copy(closePayload[4:], closeBody)
	close42 := &PgPacket{Type: pgMsgClose, Payload: closePayload}
	allowed, reason = p.shouldMirrorPgFrame(close42, QueryRequest{Command: "Close"}, filtered)
	if allowed {
		t.Fatal("Close for filtered S_42 should be dropped")
	}
	if reason != FilterReasonStickyStmt {
		t.Errorf("Close sticky reason = %q, want %q", reason, FilterReasonStickyStmt)
	}
	if _, ok := filtered["S_42"]; ok {
		t.Fatal("S_42 should be cleared from filteredStmtNames after Close")
	}
}

// TestStickyByStmtName_AcrossExecutes confirms that Execute frames for a
// filtered-in (passed-through) statement are NOT dropped just because some
// other statement was filtered. Execute carries a portal name, not a stmt
// name — we rely on Bind having already been filtered if its Parse was, so
// the shadow's portal table never had the entry to begin with. Execute
// frames always pass through.
func TestStickyByStmtName_AcrossExecutes(t *testing.T) {
	cfg := &Config{
		ShadowFilterMode:     "exclude",
		ShadowFilterPatterns: []string{`(?i)pg_catalog\.`},
		ShadowSampleRate:     1.0,
	}
	f, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	p := &PgProxy{config: &Config{}, queryFilter: f}
	filtered := map[string]struct{}{}

	// Parse a non-matching statement → passes through.
	parse := buildParseFrame("S_99", "SELECT id FROM addresses")
	allowed, _ := p.shouldMirrorPgFrame(parse, reqForParse(parse), filtered)
	if !allowed {
		t.Fatal("non-matching Parse should pass through")
	}

	// Bind passes through.
	bind := buildBindFrame("p99", "S_99")
	allowed, _ = p.shouldMirrorPgFrame(bind, QueryRequest{Command: "Bind"}, filtered)
	if !allowed {
		t.Fatal("Bind for non-filtered stmt should pass through")
	}

	// Repeated Execute on the same portal must always pass through.
	for i := 0; i < 5; i++ {
		exec := buildExecuteFrame("p99")
		allowed, reason := p.shouldMirrorPgFrame(exec, QueryRequest{Command: "Execute"}, filtered)
		if !allowed {
			t.Fatalf("iter %d: Execute for non-filtered portal should pass, got reason=%q", i, reason)
		}
	}

	// Filter a different statement (S_88) — Execute on the unrelated portal
	// p99 must still pass.
	parseDrop := buildParseFrame("S_88", "SELECT * FROM pg_catalog.pg_class")
	allowed, _ = p.shouldMirrorPgFrame(parseDrop, reqForParse(parseDrop), filtered)
	if allowed {
		t.Fatal("Parse with pg_catalog should be filtered")
	}
	exec := buildExecuteFrame("p99")
	allowed, _ = p.shouldMirrorPgFrame(exec, QueryRequest{Command: "Execute"}, filtered)
	if !allowed {
		t.Fatal("Execute on unrelated portal must remain pass-through after a different Parse was filtered")
	}
}

// TestStickyByStmtName_CapResetsMap bounds the per-connection sticky-stmt
// map. A noisy client issuing many unique-named Parse frames against an
// exclude pattern (and never Close('S')-ing) used to grow the map for the
// lifetime of the connection — concerning under pgbouncer's long-lived
// backend connections. The cap clears the map on overflow; the next entry
// proceeds. Sticky tracking for the cleared names is lost, which mirrors
// the documented behavior of a Bind/Execute arriving without a tracked
// Parse (see TestStickyByStmtName_PatternFilter).
func TestStickyByStmtName_CapResetsMap(t *testing.T) {
	cfg := &Config{
		ShadowFilterMode:     "exclude",
		ShadowFilterPatterns: []string{`(?i)pg_catalog\.`},
		ShadowSampleRate:     1.0,
	}
	f, err := NewQueryFilter(cfg)
	if err != nil {
		t.Fatalf("NewQueryFilter: %v", err)
	}
	p := &PgProxy{config: &Config{}, queryFilter: f}
	filtered := map[string]struct{}{}

	startResets := testutil.ToFloat64(pgStickyStmtMapResets)

	// Fill exactly to the cap with unique filtered Parses.
	for i := 0; i < pgStickyStmtMapCap; i++ {
		name := fmt.Sprintf("S_%d", i)
		parse := buildParseFrame(name, "SELECT * FROM pg_catalog.pg_class")
		allowed, reason := p.shouldMirrorPgFrame(parse, reqForParse(parse), filtered)
		if allowed {
			t.Fatalf("iter %d: filtered Parse should not be allowed", i)
		}
		if reason != FilterReasonPattern {
			t.Fatalf("iter %d: reason = %q, want %q", i, reason, FilterReasonPattern)
		}
	}
	if got := len(filtered); got != pgStickyStmtMapCap {
		t.Fatalf("pre-overflow map size = %d, want %d", got, pgStickyStmtMapCap)
	}
	if delta := testutil.ToFloat64(pgStickyStmtMapResets) - startResets; delta != 0 {
		t.Fatalf("unexpected resets before overflow: delta=%v", delta)
	}

	// One more filtered Parse → triggers reset, then inserts the new name.
	overflowName := "S_overflow"
	overflowParse := buildParseFrame(overflowName, "SELECT * FROM pg_catalog.pg_class")
	p.shouldMirrorPgFrame(overflowParse, reqForParse(overflowParse), filtered)

	if got := len(filtered); got != 1 {
		t.Fatalf("post-reset map size = %d, want 1", got)
	}
	if _, ok := filtered[overflowName]; !ok {
		t.Fatalf("overflow entry %q missing from map after reset", overflowName)
	}
	if delta := testutil.ToFloat64(pgStickyStmtMapResets) - startResets; delta != 1 {
		t.Fatalf("reset counter delta = %v, want 1", delta)
	}

	// Sticky tracking for evicted names is gone: Bind for S_0 leaks through
	// to the shadow. This is the documented degradation mode.
	leakedBind := buildBindFrame("portal_zero", "S_0")
	allowed, _ := p.shouldMirrorPgFrame(leakedBind, QueryRequest{Command: "Bind"}, filtered)
	if !allowed {
		t.Fatal("Bind for evicted-from-map name should leak through after reset")
	}

	// The surviving entry still works as a sticky filter for its own Bind.
	stickyBind := buildBindFrame("portal_overflow", overflowName)
	allowed, reason := p.shouldMirrorPgFrame(stickyBind, QueryRequest{Command: "Bind"}, filtered)
	if allowed {
		t.Fatalf("Bind for post-reset entry %q should still be sticky-filtered", overflowName)
	}
	if reason != FilterReasonStickyStmt {
		t.Fatalf("post-reset sticky reason = %q, want %q", reason, FilterReasonStickyStmt)
	}
}
