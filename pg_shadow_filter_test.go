package main

import "testing"

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

// TestShouldShadowMirror_SampleRateZeroAlwaysSkips covers a SHADOW_SAMPLE_RATE
// of 0 — no frame should ever be mirrored for a SQL-carrying command.
func TestShouldShadowMirror_SampleRateZeroAlwaysSkips(t *testing.T) {
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
		if allowed {
			t.Fatalf("iter %d: sample_rate=0 should always skip, got allowed=true", i)
		}
		if reason != FilterReasonSampling {
			t.Errorf("iter %d: expected reason=%q, got %q", i, FilterReasonSampling, reason)
		}
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
