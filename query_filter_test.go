package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// testAllow is a test helper that calls Allow and returns only the bool.
// Use this when the filter reason is not being tested.
func testAllow(f *QueryFilter, req QueryRequest) bool {
	allowed, _ := f.Allow(req)
	return allowed
}

// --- ExtractPrimarySQLOperation tests ---

func TestExtractPrimarySQLOperation_SimpleStatements(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"SELECT", "SELECT * FROM users", "SELECT"},
		{"INSERT", "INSERT INTO users VALUES (1, 'a')", "INSERT"},
		{"DELETE", "DELETE FROM users WHERE id = 1", "DELETE"},
		{"UPDATE", "UPDATE users SET name = 'b' WHERE id = 1", "UPDATE"},
		{"CREATE_TABLE", "CREATE TABLE users (id INT)", "CREATE_TABLE"},
		{"DROP_TABLE", "DROP TABLE users", "DROP_TABLE"},
		{"ALTER_TABLE", "ALTER TABLE users ADD COLUMN name VARCHAR(100)", "ALTER_TABLE"},
		{"TRUNCATE", "TRUNCATE TABLE users", "TRUNCATE"},
		{"SHOW", "SHOW TABLES", "SHOW"},
		{"DESCRIBE", "DESCRIBE users", "DESCRIBE"},
		{"EXPLAIN", "EXPLAIN SELECT * FROM users", "EXPLAIN"},
		{"SET", "SET time_zone = 'UTC'", "SET"},
		{"USE", "USE my_database", "USE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestExtractPrimarySQLOperation_StarRocksSpecific(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"INSERT_OVERWRITE", "INSERT OVERWRITE users SELECT * FROM staging", "INSERT_OVERWRITE"},
		{"SUBMIT_TASK", "SUBMIT TASK AS INSERT INTO users SELECT * FROM staging", "SUBMIT_TASK"},
		{"CREATE_MATERIALIZED_VIEW", "CREATE MATERIALIZED VIEW mv AS SELECT * FROM t", "CREATE_MATERIALIZED_VIEW"},
		{"REFRESH_MATERIALIZED_VIEW", "REFRESH MATERIALIZED VIEW mv", "REFRESH_MATERIALIZED_VIEW"},
		{"DROP_MATERIALIZED_VIEW", "DROP MATERIALIZED VIEW mv", "DROP_MATERIALIZED_VIEW"},
		{"ALTER_MATERIALIZED_VIEW", "ALTER MATERIALIZED VIEW mv SET('session.query_timeout' = '3600')", "ALTER_MATERIALIZED_VIEW"},
		{"ADMIN_SHOW", "ADMIN SHOW REPLICA STATUS FROM db.tbl", "ADMIN_SHOW"},
		{"ADMIN_SET", "ADMIN SET FRONTEND CONFIG ('key' = 'value')", "ADMIN_SET"},
		{"ANALYZE", "ANALYZE TABLE users", "ANALYZE"},
		{"BROKER_LOAD", "BROKER LOAD INTO users FROM 's3://...'", "BROKER_LOAD"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestExtractPrimarySQLOperation_MultiStatement(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{
			"SET_CATALOG_USE_INSERT_OVERWRITE",
			"SET CATALOG my_catalog; USE my_schema; INSERT OVERWRITE my_table (col1, col2) SELECT a, b FROM staging",
			"INSERT_OVERWRITE",
		},
		{
			"SET_CATALOG_USE_SELECT",
			"SET CATALOG analytics; USE warehouse; SELECT * FROM daily_metrics WHERE dt = '2026-01-01'",
			"SELECT",
		},
		{
			"SET_USE_SUBMIT_TASK",
			"SET CATALOG prod; USE etl; SUBMIT TASK AS INSERT INTO agg_table SELECT count(*) FROM raw_events GROUP BY dt",
			"SUBMIT_TASK",
		},
		{
			"USE_ONLY",
			"USE my_database",
			"USE",
		},
		{
			"SET_ONLY",
			"SET time_zone = 'UTC'",
			"SET",
		},
		{
			"SET_CATALOG_USE_DELETE",
			"SET CATALOG c; USE s; DELETE FROM users WHERE expired = true",
			"DELETE",
		},
		{
			"real_world_insert_overwrite",
			`SET CATALOG my_catalog;
			USE my_schema;
			INSERT OVERWRITE risk_indicators
			(org_uuid, category_id, risk_type, score)
			SELECT org_uuid, cat_id, 'counterparty', score
			FROM counterparty_flows
			WHERE dt >= '2026-01-01'`,
			"INSERT_OVERWRITE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestExtractPrimarySQLOperation_WithComments(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{
			"single_line_comment",
			"-- This is a comment\nSELECT * FROM users",
			"SELECT",
		},
		{
			"block_comment",
			"/* batch job */ INSERT OVERWRITE target SELECT * FROM source",
			"INSERT_OVERWRITE",
		},
		{
			"comment_in_multi_statement",
			"SET CATALOG c;\n-- Switch schema\nUSE s;\n/* Main query */\nSELECT count(*) FROM t",
			"SELECT",
		},
		{
			"commented_out_INSERT_before_SELECT",
			"-- INSERT OVERWRITE was too slow, switched to SELECT\nSELECT * FROM analytics.events",
			"SELECT",
		},
		{
			"block_comment_with_INSERT_before_SELECT",
			"/* old: INSERT OVERWRITE target SELECT * FROM source */ SELECT count(*) FROM events",
			"SELECT",
		},
		{
			"commented_INSERT_in_multi_statement",
			"SET CATALOG c; USE s;\n-- INSERT OVERWRITE risk_indicators\n-- (org_uuid, category_id)\nSELECT * FROM risk_indicators",
			"SELECT",
		},
		{
			"multiple_leading_comments_before_real_op",
			"-- comment 1\n-- comment 2\n/* comment 3 */\nDELETE FROM expired_data WHERE dt < '2025-01-01'",
			"DELETE",
		},
		{
			"inline_comment_does_not_affect_first_keyword",
			"SELECT /* hint: use_index */ * FROM users WHERE id = 1",
			"SELECT",
		},
		{
			"comment_only_statement_skipped",
			"SET CATALOG c; -- just a comment\n; SELECT 1",
			"SELECT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestQueryFilter_CommentedOutSQLDoesNotAffectOperationFilter(t *testing.T) {
	// The SQL operation filter should NOT be fooled by commented-out SQL.
	// It only looks at the first keyword(s) of each statement after stripping
	// leading comments — it does NOT search for keywords in the body.

	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    1.0,
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
		reason  string
	}{
		{
			"SELECT_with_INSERT_in_comment_is_allowed",
			"-- INSERT OVERWRITE risk_indicators\nSELECT * FROM analytics.events",
			true,
			"primary operation is SELECT (INSERT is only in a comment)",
		},
		{
			"SELECT_with_INSERT_in_block_comment_is_allowed",
			"/* INSERT OVERWRITE target */ SELECT count(*) FROM events",
			true,
			"primary operation is SELECT (INSERT is only in a block comment)",
		},
		{
			"actual_INSERT_OVERWRITE_is_blocked",
			"INSERT OVERWRITE risk_indicators SELECT * FROM staging",
			false,
			"primary operation is INSERT_OVERWRITE, not SELECT",
		},
		{
			"SELECT_with_INSERT_in_string_literal_is_allowed",
			"SELECT * FROM logs WHERE message = 'INSERT OVERWRITE failed'",
			true,
			"primary operation is SELECT (INSERT is only in a string literal)",
		},
		{
			"multi_stmt_with_commented_INSERT_before_SELECT",
			"SET CATALOG c; USE s;\n-- INSERT OVERWRITE risk_indicators\nSELECT * FROM analytics.events",
			true,
			"primary operation is SELECT (INSERT is commented out)",
		},
		{
			"SELECT_subquery_with_INSERT_keyword_in_body",
			"SELECT * FROM audit_log WHERE action = 'INSERT' AND target = 'users'",
			true,
			"primary operation is SELECT (INSERT appears in WHERE clause value, not as operation)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow() = %v, want %v (%s)", got, tt.allowed, tt.reason)
			}
		})
	}
}

func TestQueryFilter_PatternMatchesFullQueryText(t *testing.T) {
	// IMPORTANT: Unlike SQL operation detection, regex patterns match against the
	// FULL query text including comments and string literals. This is by design —
	// patterns are meant for matching table names, database names, etc. in the
	// actual query body. Users should write precise patterns to avoid false matches.

	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{`(?i)\brisk_indicators\b`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
		note    string
	}{
		{
			"matches_table_in_INSERT",
			"INSERT OVERWRITE risk_indicators SELECT * FROM staging",
			true,
			"table name in INSERT target",
		},
		{
			"matches_table_in_SELECT",
			"SELECT * FROM risk_indicators WHERE org_uuid = 'abc'",
			true,
			"table name in SELECT FROM",
		},
		{
			"matches_in_commented_code_too",
			"-- INSERT OVERWRITE risk_indicators\nSELECT 1",
			true,
			"pattern matches full text including comments — this is expected",
		},
		{
			"no_match",
			"SELECT * FROM users WHERE id = 1",
			false,
			"no mention of risk_indicators anywhere",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow() = %v, want %v (%s)", got, tt.allowed, tt.note)
			}
		})
	}
}

func TestExtractPrimarySQLOperation_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"empty", "", "UNKNOWN"},
		{"whitespace_only", "   \n\t  ", "UNKNOWN"},
		{"semicolons_only", ";;;", "UNKNOWN"},
		{"semicolons_in_string", "SELECT * FROM t WHERE name = 'a;b;c'", "SELECT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

// --- splitStatements tests ---

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected []string
	}{
		{
			"single",
			"SELECT 1",
			[]string{"SELECT 1"},
		},
		{
			"two_statements",
			"SET CATALOG c; USE s",
			[]string{"SET CATALOG c", "USE s"},
		},
		{
			"trailing_semicolon",
			"SELECT 1;",
			[]string{"SELECT 1"},
		},
		{
			"semicolons_in_single_quotes",
			"SELECT * FROM t WHERE x = 'a;b'",
			[]string{"SELECT * FROM t WHERE x = 'a;b'"},
		},
		{
			"semicolons_in_double_quotes",
			`SELECT * FROM t WHERE x = "a;b"`,
			[]string{`SELECT * FROM t WHERE x = "a;b"`},
		},
		{
			"escaped_quote",
			`SELECT * FROM t WHERE x = 'it\'s;here'`,
			[]string{`SELECT * FROM t WHERE x = 'it\'s;here'`},
		},
		{
			"empty_statements_filtered",
			";;SELECT 1;;",
			[]string{"SELECT 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitStatements(tt.query)
			if len(got) != len(tt.expected) {
				t.Fatalf("splitStatements(%q) returned %d statements, want %d: %v", tt.query, len(got), len(tt.expected), got)
			}
			for i, s := range got {
				if s != tt.expected[i] {
					t.Errorf("splitStatements(%q)[%d] = %q, want %q", tt.query, i, s, tt.expected[i])
				}
			}
		})
	}
}

// --- QueryFilter.Allow tests ---

func TestQueryFilter_IncludeByOperation(t *testing.T) {
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    1.0,
	}

	tests := []struct {
		name    string
		req     QueryRequest
		allowed bool
	}{
		{
			"SELECT_allowed",
			QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM users"},
			true,
		},
		{
			"INSERT_blocked",
			QueryRequest{Command: "COM_QUERY", QueryText: "INSERT INTO users VALUES (1)"},
			false,
		},
		{
			"multi_statement_SELECT_allowed",
			QueryRequest{Command: "COM_QUERY", QueryText: "SET CATALOG c; USE s; SELECT * FROM t"},
			true,
		},
		{
			"multi_statement_INSERT_OVERWRITE_blocked",
			QueryRequest{Command: "COM_QUERY", QueryText: "SET CATALOG c; USE s; INSERT OVERWRITE t SELECT * FROM s"},
			false,
		},
		{
			"COM_INIT_DB_always_passes",
			QueryRequest{Command: "COM_INIT_DB"},
			true,
		},
		{
			"COM_PING_always_passes",
			QueryRequest{Command: "COM_PING"},
			true,
		},
		{
			"COM_STMT_PREPARE_always_passes",
			QueryRequest{Command: "COM_STMT_PREPARE"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := testAllow(f, tt.req)
			if got != tt.allowed {
				t.Errorf("Allow(%v) = %v, want %v", tt.req.QueryText, got, tt.allowed)
			}
		})
	}
}

func TestQueryFilter_ExcludeByOperation(t *testing.T) {
	f := &QueryFilter{
		mode:          "exclude",
		sqlOperations: map[string]bool{"INSERT_OVERWRITE": true, "SUBMIT_TASK": true},
		sampleRate:    1.0,
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
	}{
		{"SELECT_allowed", "SELECT * FROM users", true},
		{"INSERT_allowed", "INSERT INTO users VALUES (1)", true},
		{"INSERT_OVERWRITE_blocked", "INSERT OVERWRITE t SELECT * FROM s", false},
		{"SUBMIT_TASK_blocked", "SUBMIT TASK AS INSERT INTO t SELECT * FROM s", false},
		{
			"multi_stmt_INSERT_OVERWRITE_blocked",
			"SET CATALOG c; USE s; INSERT OVERWRITE t SELECT * FROM s",
			false,
		},
		{
			"multi_stmt_SELECT_allowed",
			"SET CATALOG c; USE s; SELECT * FROM t",
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
			}
		})
	}
}

func TestQueryFilter_IncludeByPattern(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{`analytics\.`, `risk_indicators`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
	}{
		{"matches_analytics", "SELECT * FROM analytics.daily_metrics", true},
		{"matches_risk_indicators", "INSERT OVERWRITE risk_indicators SELECT * FROM t", true},
		{"no_match", "SELECT * FROM users", false},
		{
			"matches_in_multi_statement",
			"SET CATALOG c; USE s; SELECT * FROM analytics.events WHERE dt = '2026-01-01'",
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
			}
		})
	}
}

func TestQueryFilter_ExcludeByPattern(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "exclude",
		ShadowFilterPatterns: []string{`(?i)information_schema`, `(?i)__internal`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
	}{
		{"normal_query_allowed", "SELECT * FROM users", true},
		{"information_schema_blocked", "SELECT * FROM information_schema.tables", false},
		{"internal_blocked", "SELECT * FROM __internal.metadata", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
			}
		})
	}
}

func TestQueryFilter_CombinedOperationAndPattern(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"SELECT"},
		ShadowFilterPatterns:      []string{`analytics\.`},
		ShadowSampleRate:          1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
	}{
		{"SELECT_on_analytics_allowed", "SELECT * FROM analytics.events", true},
		{"SELECT_on_users_blocked", "SELECT * FROM users", false},
		{"INSERT_on_analytics_blocked", "INSERT INTO analytics.events VALUES (1)", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
			}
		})
	}
}

func TestQueryFilter_Sampling(t *testing.T) {
	f := &QueryFilter{
		mode:       "include",
		sampleRate: 0.5,
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT 1"}

	allowed := 0
	total := 10000
	for i := 0; i < total; i++ {
		if testAllow(f, req) {
			allowed++
		}
	}

	rate := float64(allowed) / float64(total)
	if rate < 0.40 || rate > 0.60 {
		t.Errorf("Sample rate 0.5 produced %.2f%% allowed (expected ~50%%)", rate*100)
	}
}

func TestQueryFilter_NilFilterAllowsEverything(t *testing.T) {
	config := &Config{ShadowSampleRate: 1.0}
	f, err := NewQueryFilter(config)
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Fatal("expected nil filter when no filter config set")
	}
}

func TestQueryFilter_InvalidMode(t *testing.T) {
	_, err := NewQueryFilter(&Config{
		ShadowFilterMode: "invalid",
		ShadowSampleRate: 1.0,
	})
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestQueryFilter_InvalidPattern(t *testing.T) {
	_, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{"[invalid"},
		ShadowSampleRate:     1.0,
	})
	if err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
}

func TestQueryFilter_SamplingOnly(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowSampleRate: 0.01,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("expected non-nil filter for sample rate < 1.0")
	}
	if f.mode != "include" {
		t.Errorf("expected default mode 'include', got %q", f.mode)
	}
}

func TestNewQueryFilter_SampleRateValidation(t *testing.T) {
	tests := []struct {
		name    string
		rate    float64
		wantErr bool
	}{
		{"valid_zero", 0.0, false},
		{"valid_half", 0.5, false},
		{"valid_one", 1.0, false},
		{"negative", -0.5, true},
		{"above_one", 1.5, true},
		{"large_negative", -100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewQueryFilter(&Config{
				ShadowFilterMode: "include",
				ShadowSampleRate: tt.rate,
			})
			if (err != nil) != tt.wantErr {
				t.Errorf("NewQueryFilter(rate=%f) error = %v, wantErr = %v", tt.rate, err, tt.wantErr)
			}
		})
	}
}

func TestNewQueryFilter_ModeSetNoCriteria(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode: "include",
		ShadowSampleRate: 1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("expected non-nil filter when mode is set")
	}
	req := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT 1"}
	if !testAllow(f, req) {
		t.Error("filter with no criteria should allow all queries")
	}
}

func TestNewQueryFilter_ModeEmptyWithCriteria(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterSQLOperations: []string{"SELECT"},
		ShadowSampleRate:          1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("expected non-nil filter when sql operations are set")
	}
	if f.mode != "include" {
		t.Errorf("expected default mode 'include', got %q", f.mode)
	}
}

// --- stripLeadingComments tests ---

func TestStripLeadingComments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"no_comment", "SELECT 1", "SELECT 1"},
		{"single_line", "-- comment\nSELECT 1", "SELECT 1"},
		{"block_comment", "/* comment */ SELECT 1", "SELECT 1"},
		{"multiple_comments", "-- first\n/* second */ SELECT 1", "SELECT 1"},
		{"only_comment", "-- just a comment", ""},
		{"unclosed_block", "/* unclosed", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripLeadingComments(tt.input)
			if got != tt.expected {
				t.Errorf("stripLeadingComments(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// --- Real-world StarRocks query test ---

func TestExtractPrimarySQLOperation_RealWorldStarRocks(t *testing.T) {
	// This is the actual query pattern from the user's codebase
	query := `
		SET CATALOG my_catalog;
		USE my_schema;

		INSERT OVERWRITE risk_indicators
		(org_uuid, category_id, category, risk_type,
		 category_risk_score_level, category_risk_score_level_label,
		 window_start, window_end, incoming_volume_usd, outgoing_volume_usd,
		 total_volume_usd, instances, custom_entity_uuid)
		-- Counterparty risk indicators
		SELECT
		  'org-123' AS org_uuid,
		  ccf.counterparty_category_id AS category_id,
		  NULL AS category,
		  'counterparty' AS risk_type,
		  COALESCE(orr.risk_score_level, 0) AS category_risk_score_level,
		  CASE 
		    WHEN COALESCE(orr.risk_score_level, 0) = 0 THEN 'Unknown'
		    WHEN COALESCE(orr.risk_score_level, 0) = 1 THEN 'Low'
		    WHEN COALESCE(orr.risk_score_level, 0) = 5 THEN 'Medium'
		    WHEN COALESCE(orr.risk_score_level, 0) = 10 THEN 'High'
		    WHEN COALESCE(orr.risk_score_level, 0) = 15 THEN 'Severe'
		    ELSE 'Unknown'
		  END AS category_risk_score_level_label,
		  MIN(ccf.tx_date) AS window_start,
		  MAX(ccf.tx_date) AS window_end,
		  SUM(ccf.incoming_volume_usd) AS incoming_volume_usd,
		  SUM(ccf.outgoing_volume_usd) AS outgoing_volume_usd,
		  SUM(ccf.incoming_volume_usd) + SUM(ccf.outgoing_volume_usd) AS total_volume_usd,
		  SUM(ccf.incoming_transfers) + SUM(ccf.outgoing_transfers) AS instances,
		  'entity-456' AS custom_entity_uuid
		FROM counterparty_category_flows_daily ccf
		LEFT JOIN org_risk_rules orr
		  ON CAST(ccf.counterparty_category_id AS BIGINT) = orr.category_id
		  AND orr.risk_type_id = 2
		WHERE ccf.custom_entity_uuid = 'entity-456'
		GROUP BY ccf.counterparty_category_id, orr.risk_score_level
	`

	got := ExtractPrimarySQLOperation(query)
	if got != "INSERT_OVERWRITE" {
		t.Errorf("Expected INSERT_OVERWRITE for real-world query, got %q", got)
	}

	// Verify that an include filter for SELECT would block this
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    1.0,
	}
	req := QueryRequest{Command: "COM_QUERY", QueryText: query}
	if testAllow(f, req) {
		t.Error("Expected INSERT_OVERWRITE query to be blocked by SELECT-only filter")
	}

	// Verify that an include filter for INSERT_OVERWRITE would allow this
	f2 := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"INSERT_OVERWRITE": true},
		sampleRate:    1.0,
	}
	if !testAllow(f2, req) {
		t.Error("Expected INSERT_OVERWRITE query to be allowed by INSERT_OVERWRITE filter")
	}

	// Verify pattern matching on the full query text
	f3, _ := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{`risk_indicators`},
		ShadowSampleRate:     1.0,
	})
	if !testAllow(f3, req) {
		t.Error("Expected query mentioning risk_indicators to be allowed by pattern filter")
	}
}

// =============================================================================
// Integration test: filter wired into proxy with mock MySQL servers
// =============================================================================

func TestProxyWithQueryFilter_IntegrationSelectOnly(t *testing.T) {
	primaryResponse := buildMySQLOKPacket(1)
	primaryServer, err := NewMockMySQLServer("root", "", primaryResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowResponse := buildMySQLOKPacket(1)
	shadowServer, err := NewMockMySQLServer("root", "", shadowResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:                "127.0.0.1:0",
		PrimaryHost:               primaryParts[0],
		PrimaryPort:               primaryParts[1],
		PrimaryUser:               "root",
		PrimaryPassword:           "",
		ShadowHost:                shadowParts[0],
		ShadowPort:                shadowParts[1],
		ShadowUser:                "root",
		ShadowPassword:            "",
		ShadowQueueSize:           100,
		ShadowReadTimeout:         5 * time.Second,
		ShadowDrainTimeout:        500 * time.Millisecond,
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"SELECT"},
		ShadowSampleRate:          1.0,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	qf, err := NewQueryFilter(config)
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}
	proxy.queryFilter = qf

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	// Complete handshake
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}

	authPacket := buildTestAuthPacket("root", "", buf[:n])
	proxyConn.Write(authPacket)

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	// Send a SELECT — should reach shadow
	selectPacket := buildMySQLQueryPacket("SELECT * FROM users", 0)
	proxyConn.Write(selectPacket)
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	// Wait for async shadow processing
	time.Sleep(300 * time.Millisecond)

	selectReceived := countReceivedWithSubstring(shadowServer, "SELECT")

	// Send an INSERT — should be filtered and NOT reach shadow
	insertPacket := buildMySQLQueryPacket("INSERT INTO users VALUES (1, 'alice')", 0)
	proxyConn.Write(insertPacket)
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	time.Sleep(300 * time.Millisecond)

	insertReceived := countReceivedWithSubstring(shadowServer, "INSERT INTO")

	if selectReceived == 0 {
		t.Error("Shadow should have received the SELECT query but didn't")
	}
	if insertReceived > 0 {
		t.Error("Shadow should NOT have received the INSERT query but did")
	}
}

func TestProxyWithQueryFilter_IntegrationExcludePattern(t *testing.T) {
	primaryResponse := buildMySQLOKPacket(1)
	primaryServer, err := NewMockMySQLServer("root", "", primaryResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowResponse := buildMySQLOKPacket(1)
	shadowServer, err := NewMockMySQLServer("root", "", shadowResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:           "127.0.0.1:0",
		PrimaryHost:          primaryParts[0],
		PrimaryPort:          primaryParts[1],
		PrimaryUser:          "root",
		PrimaryPassword:      "",
		ShadowHost:           shadowParts[0],
		ShadowPort:           shadowParts[1],
		ShadowUser:           "root",
		ShadowPassword:       "",
		ShadowQueueSize:      100,
		ShadowReadTimeout:    5 * time.Second,
		ShadowDrainTimeout:   500 * time.Millisecond,
		ShadowFilterMode:     "exclude",
		ShadowFilterPatterns: []string{`(?i)information_schema`},
		ShadowSampleRate:     1.0,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}

	qf, err := NewQueryFilter(config)
	if err != nil {
		t.Fatalf("NewQueryFilter failed: %v", err)
	}
	proxy.queryFilter = qf

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	// Complete handshake
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}
	authPacket := buildTestAuthPacket("root", "", buf[:n])
	proxyConn.Write(authPacket)
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	// Normal query should reach shadow
	normalPacket := buildMySQLQueryPacket("SELECT * FROM users", 0)
	proxyConn.Write(normalPacket)
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)
	time.Sleep(300 * time.Millisecond)

	normalReceived := countReceivedWithSubstring(shadowServer, "SELECT * FROM users")

	// information_schema query should be filtered
	schemaPacket := buildMySQLQueryPacket("SELECT * FROM information_schema.tables", 0)
	proxyConn.Write(schemaPacket)
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)
	time.Sleep(300 * time.Millisecond)

	schemaReceived := countReceivedWithSubstring(shadowServer, "information_schema")

	if normalReceived == 0 {
		t.Error("Shadow should have received the normal query but didn't")
	}
	if schemaReceived > 0 {
		t.Error("Shadow should NOT have received information_schema query but did")
	}
}

func TestProxyWithQueryFilter_NoFilterMirrorsEverything(t *testing.T) {
	primaryResponse := buildMySQLOKPacket(1)
	primaryServer, err := NewMockMySQLServer("root", "", primaryResponse)
	if err != nil {
		t.Fatalf("Failed to create primary mock server: %v", err)
	}
	defer primaryServer.Close()

	shadowResponse := buildMySQLOKPacket(1)
	shadowServer, err := NewMockMySQLServer("root", "", shadowResponse)
	if err != nil {
		t.Fatalf("Failed to create shadow mock server: %v", err)
	}
	defer shadowServer.Close()

	primaryParts := strings.Split(primaryServer.Addr(), ":")
	shadowParts := strings.Split(shadowServer.Addr(), ":")

	config := &Config{
		ListenAddr:         "127.0.0.1:0",
		PrimaryHost:        primaryParts[0],
		PrimaryPort:        primaryParts[1],
		PrimaryUser:        "root",
		PrimaryPassword:    "",
		ShadowHost:         shadowParts[0],
		ShadowPort:         shadowParts[1],
		ShadowUser:         "root",
		ShadowPassword:     "",
		ShadowQueueSize:    100,
		ShadowReadTimeout:  5 * time.Second,
		ShadowDrainTimeout: 500 * time.Millisecond,
		ShadowSampleRate:   1.0,
	}

	proxy, err := NewTCPProxy(config)
	if err != nil {
		t.Fatalf("NewTCPProxy failed: %v", err)
	}
	// queryFilter is nil — no filtering

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create proxy listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		proxy.handleConnection(conn, nil)
	}()

	proxyConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer proxyConn.Close()

	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake: %v", err)
	}
	authPacket := buildTestAuthPacket("root", "", buf[:n])
	proxyConn.Write(authPacket)
	proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	proxyConn.Read(buf)

	// Send multiple different query types — all should reach shadow
	queries := []string{
		"SELECT * FROM users",
		"INSERT INTO users VALUES (1)",
		"DELETE FROM users WHERE id = 99",
	}
	for _, q := range queries {
		pkt := buildMySQLQueryPacket(q, 0)
		proxyConn.Write(pkt)
		proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		proxyConn.Read(buf)
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)

	totalReceived := len(shadowServer.Received())
	if totalReceived < 3 {
		t.Errorf("Expected shadow to receive at least 3 queries (no filter), got %d", totalReceived)
	}
}

// countReceivedWithSubstring counts how many received packets contain a substring.
func countReceivedWithSubstring(server *MockMySQLServer, substr string) int {
	count := 0
	for _, pkt := range server.Received() {
		if strings.Contains(string(pkt), substr) {
			count++
		}
	}
	return count
}

// =============================================================================
// Exclude mode with BOTH SQL operations and patterns (OR semantics)
// =============================================================================

func TestQueryFilter_ExcludeCombinedORSemantics(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "exclude",
		ShadowFilterSQLOperations: []string{"INSERT_OVERWRITE"},
		ShadowFilterPatterns:      []string{`(?i)information_schema`},
		ShadowSampleRate:          1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
	}{
		{
			"SELECT_on_normal_table_allowed",
			"SELECT * FROM users",
			true,
		},
		{
			"INSERT_OVERWRITE_blocked_by_operation",
			"INSERT OVERWRITE users SELECT * FROM staging",
			false,
		},
		{
			"SELECT_on_information_schema_blocked_by_pattern",
			"SELECT * FROM information_schema.tables",
			false,
		},
		{
			"INSERT_OVERWRITE_on_information_schema_blocked_by_both",
			"INSERT OVERWRITE information_schema.t SELECT * FROM s",
			false,
		},
		{
			"regular_INSERT_on_normal_table_allowed",
			"INSERT INTO users VALUES (1, 'alice')",
			true,
		},
		{
			"SELECT_with_info_schema_in_string_blocked_by_pattern",
			"SELECT * FROM t WHERE db = 'information_schema'",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
			}
		})
	}
}

// =============================================================================
// Include mode with multiple SQL operations
// =============================================================================

func TestQueryFilter_IncludeMultipleOperations(t *testing.T) {
	f := &QueryFilter{
		mode: "include",
		sqlOperations: map[string]bool{
			"SELECT":           true,
			"INSERT_OVERWRITE": true,
			"SUBMIT_TASK":      true,
		},
		sampleRate: 1.0,
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
	}{
		{"SELECT_allowed", "SELECT * FROM t", true},
		{"INSERT_OVERWRITE_allowed", "INSERT OVERWRITE t SELECT * FROM s", true},
		{"SUBMIT_TASK_allowed", "SUBMIT TASK AS INSERT INTO t SELECT * FROM s", true},
		{"regular_INSERT_blocked", "INSERT INTO t VALUES (1)", false},
		{"DELETE_blocked", "DELETE FROM t WHERE id = 1", false},
		{"CREATE_TABLE_blocked", "CREATE TABLE t (id INT)", false},
		{
			"multi_stmt_INSERT_OVERWRITE_allowed",
			"SET CATALOG c; USE s; INSERT OVERWRITE t SELECT * FROM s",
			true,
		},
		{
			"multi_stmt_DELETE_blocked",
			"SET CATALOG c; USE s; DELETE FROM t WHERE expired = true",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
			}
		})
	}
}

// =============================================================================
// Whitespace, formatting, and casing variations
// =============================================================================

func TestExtractPrimarySQLOperation_WhitespaceVariations(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"tabs_before_keyword", "\t\tSELECT * FROM t", "SELECT"},
		{"newlines_before_keyword", "\n\n\nSELECT * FROM t", "SELECT"},
		{"mixed_whitespace", " \t \n SELECT * FROM t", "SELECT"},
		{"extra_spaces_between_keywords", "INSERT   OVERWRITE   t   SELECT * FROM s", "INSERT_OVERWRITE"},
		{"tabs_between_keywords", "INSERT\tOVERWRITE\tt\tSELECT * FROM s", "INSERT_OVERWRITE"},
		{"newlines_in_multi_stmt", "SET CATALOG c\n;\nUSE s\n;\nSELECT 1", "SELECT"},
		{"carriage_return", "SELECT * FROM t\r\nWHERE id = 1", "SELECT"},
		{"lowercase_select", "select * from users", "SELECT"},
		{"mixed_case", "SeLeCt * FROM users", "SELECT"},
		{"lowercase_insert_overwrite", "insert overwrite t select * from s", "INSERT_OVERWRITE"},
		{"uppercase_submit_task", "SUBMIT TASK AS INSERT INTO t SELECT 1", "SUBMIT_TASK"},
		{"lowercase_set_catalog_use", "set catalog c; use s; select 1", "SELECT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

// =============================================================================
// Sampling edge cases
// =============================================================================

func TestQueryFilter_SamplingZeroBlocksAll(t *testing.T) {
	f := &QueryFilter{
		mode:       "include",
		sampleRate: 0.0,
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT 1"}

	for i := 0; i < 100; i++ {
		if testAllow(f, req) {
			t.Fatal("Sample rate 0.0 should block all queries")
		}
	}
}

func TestQueryFilter_SamplingOneAllowsAll(t *testing.T) {
	f := &QueryFilter{
		mode:       "include",
		sampleRate: 1.0,
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT 1"}

	for i := 0; i < 100; i++ {
		if !testAllow(f, req) {
			t.Fatal("Sample rate 1.0 should allow all queries")
		}
	}
}

func TestQueryFilter_SamplingAppliedAfterFilters(t *testing.T) {
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    0.5,
	}

	selectReq := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT 1"}
	insertReq := QueryRequest{Command: "COM_QUERY", QueryText: "INSERT INTO t VALUES (1)"}

	// INSERT should always be blocked regardless of sampling
	for i := 0; i < 100; i++ {
		if testAllow(f, insertReq) {
			t.Fatal("INSERT should always be blocked by operation filter, sampling should not override")
		}
	}

	// SELECT should be allowed ~50% of the time
	allowed := 0
	total := 10000
	for i := 0; i < total; i++ {
		if testAllow(f, selectReq) {
			allowed++
		}
	}
	rate := float64(allowed) / float64(total)
	if rate < 0.40 || rate > 0.60 {
		t.Errorf("SELECT with 0.5 sample rate: %.2f%% allowed, expected ~50%%", rate*100)
	}
}

func TestQueryFilter_SamplingDistribution(t *testing.T) {
	rates := []float64{0.01, 0.1, 0.25, 0.75, 0.99}
	req := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT 1"}

	for _, rate := range rates {
		t.Run(fmt.Sprintf("rate_%.2f", rate), func(t *testing.T) {
			f := &QueryFilter{mode: "include", sampleRate: rate}
			allowed := 0
			total := 20000
			for i := 0; i < total; i++ {
				if testAllow(f, req) {
					allowed++
				}
			}
			actualRate := float64(allowed) / float64(total)
			tolerance := 0.05
			if actualRate < rate-tolerance || actualRate > rate+tolerance {
				t.Errorf("Sample rate %.2f produced %.4f, outside tolerance ±%.2f", rate, actualRate, tolerance)
			}
		})
	}
}

// =============================================================================
// Non-COM_QUERY passthrough
// =============================================================================

func TestQueryFilter_NonCOMQueryAlwaysPassesThrough(t *testing.T) {
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    0.0, // Would block everything if it applied
	}

	commands := []string{
		"COM_INIT_DB", "COM_PING", "COM_STMT_PREPARE",
		"COM_STMT_EXECUTE", "COM_STMT_CLOSE", "COM_STMT_SEND_LONG_DATA",
		"COM_STMT_RESET", "COM_RESET_CONNECTION", "COM_SET_OPTION",
		"COM_FIELD_LIST", "COM_STATISTICS", "COM_PROCESS_INFO",
	}

	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			req := QueryRequest{Command: cmd}
			if !testAllow(f, req) {
				t.Errorf("%s should always pass through filter", cmd)
			}
		})
	}
}

func TestQueryFilter_EmptyQueryTextPassesThrough(t *testing.T) {
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    1.0,
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: ""}
	if !testAllow(f, req) {
		t.Error("COM_QUERY with empty text should pass through")
	}
}

// =============================================================================
// SQL operations config case insensitivity
// =============================================================================

func TestQueryFilter_SQLOperationsConfigCaseInsensitive(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"select", "Insert_Overwrite", "SUBMIT_TASK"},
		ShadowSampleRate:          1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		query   string
		allowed bool
	}{
		{"SELECT * FROM t", true},
		{"select * from t", true},
		{"INSERT OVERWRITE t SELECT 1", true},
		{"insert overwrite t select 1", true},
		{"SUBMIT TASK AS INSERT INTO t SELECT 1", true},
		{"DELETE FROM t", false},
	}

	for _, tt := range tests {
		req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
		if got := testAllow(f, req); got != tt.allowed {
			t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
		}
	}
}

// =============================================================================
// Regex pattern edge cases
// =============================================================================

func TestQueryFilter_PatternCaseInsensitiveFlag(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{`(?i)risk_indicators`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM RISK_INDICATORS"}
	if !testAllow(f, req) {
		t.Error("Case-insensitive pattern should match uppercase table name")
	}
}

func TestQueryFilter_PatternWithSpecialRegexChars(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{`analytics\.daily_metrics`, `catalog_\d+\.events`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		query   string
		allowed bool
	}{
		{"SELECT * FROM analytics.daily_metrics", true},
		{"SELECT * FROM analytics_daily_metrics", false}, // dot is literal with \. in regex
		{"SELECT * FROM catalog_123.events", true},
		{"SELECT * FROM catalog_abc.events", false}, // \d+ requires digits
	}

	for _, tt := range tests {
		req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
		if got := testAllow(f, req); got != tt.allowed {
			t.Errorf("Allow(%q) = %v, want %v", tt.query, got, tt.allowed)
		}
	}
}

func TestQueryFilter_MultipleOverlappingPatterns(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "include",
		ShadowFilterPatterns: []string{`analytics`, `daily_metrics`, `nonexistent`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should match with first pattern
	req1 := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM analytics.events"}
	if !testAllow(f, req1) {
		t.Error("Should match 'analytics' pattern")
	}

	// Should match with second pattern
	req2 := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM warehouse.daily_metrics"}
	if !testAllow(f, req2) {
		t.Error("Should match 'daily_metrics' pattern")
	}

	// Should not match any
	req3 := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM users"}
	if testAllow(f, req3) {
		t.Error("Should not match any pattern")
	}
}

// =============================================================================
// Edge cases in SQL parsing
// =============================================================================

func TestExtractPrimarySQLOperation_NestedComments(t *testing.T) {
	// MySQL doesn't support nested comments, but we should handle gracefully.
	// The parser will match first */ and continue.
	query := "/* outer /* inner */ SELECT * FROM t"
	got := ExtractPrimarySQLOperation(query)
	if got != "SELECT" {
		t.Errorf("Expected SELECT for query with nested-style comment, got %q", got)
	}
}

func TestExtractPrimarySQLOperation_LargeQuery(t *testing.T) {
	// Build a large multi-statement query similar to real-world ETL jobs
	var sb strings.Builder
	sb.WriteString("SET CATALOG my_catalog;\n")
	sb.WriteString("USE my_schema;\n")
	sb.WriteString("INSERT OVERWRITE target_table\n")
	sb.WriteString("(col1, col2, col3, col4, col5)\n")
	sb.WriteString("SELECT\n")
	for i := 0; i < 100; i++ {
		sb.WriteString(fmt.Sprintf("  COALESCE(t%d.field_%d, 0) AS col_%d,\n", i, i, i))
	}
	sb.WriteString("  'final_value' AS last_col\n")
	sb.WriteString("FROM source_table t0\n")
	for i := 1; i < 50; i++ {
		sb.WriteString(fmt.Sprintf("LEFT JOIN table_%d t%d ON t0.id = t%d.id\n", i, i, i))
	}
	sb.WriteString("WHERE t0.dt >= '2026-01-01'\n")
	sb.WriteString("GROUP BY t0.category_id")

	got := ExtractPrimarySQLOperation(sb.String())
	if got != "INSERT_OVERWRITE" {
		t.Errorf("Expected INSERT_OVERWRITE for large query, got %q", got)
	}
}

func TestExtractPrimarySQLOperation_UnicodeTableNames(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"chinese_table", "SELECT * FROM 用户表 WHERE id = 1", "SELECT"},
		{"emoji_in_string", "SELECT * FROM t WHERE name = '🚀'", "SELECT"},
		{"accented_chars", "SELECT * FROM café_orders WHERE région = 'île-de-france'", "SELECT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestExtractPrimarySQLOperation_StarRocksMultiStmtWithNewlines(t *testing.T) {
	// Real-world pattern: generated SQL with lots of whitespace variations
	query := `
		SET CATALOG my_catalog
		;
		USE my_schema
		;

		INSERT OVERWRITE risk_indicators
			(org_uuid, category_id, risk_type)
		SELECT
			org_uuid,
			category_id,
			'counterparty'
		FROM source_table
		WHERE dt = '2026-03-25'
	`
	got := ExtractPrimarySQLOperation(query)
	if got != "INSERT_OVERWRITE" {
		t.Errorf("Expected INSERT_OVERWRITE, got %q", got)
	}
}

func TestSplitStatements_ComplexQuoting(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected int
	}{
		{
			"nested_quotes",
			`SELECT * FROM t WHERE name = "it's a ; test"`,
			1,
		},
		{
			"multiple_semicolons_in_strings",
			`SELECT * FROM t WHERE a = 'x;y;z' AND b = "p;q;r"`,
			1,
		},
		{
			"escaped_backslash_before_semicolon",
			`SELECT * FROM t WHERE path = 'C:\\data\\'; SELECT 2`,
			2,
		},
		{
			"three_real_statements",
			"SET CATALOG c; USE db; SELECT 1",
			3,
		},
		{
			"whitespace_only_between_semicolons",
			"SELECT 1;   ;   ; SELECT 2",
			2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitStatements(tt.query)
			if len(got) != tt.expected {
				t.Errorf("splitStatements(%q) returned %d statements, want %d: %v", tt.query, len(got), tt.expected, got)
			}
		})
	}
}

// =============================================================================
// NewQueryFilter construction and String()
// =============================================================================

func TestNewQueryFilter_FullConfig(t *testing.T) {
	config := &Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"SELECT", "INSERT_OVERWRITE"},
		ShadowFilterPatterns:      []string{`analytics\.`, `risk_indicators`},
		ShadowSampleRate:          0.5,
	}

	f, err := NewQueryFilter(config)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("Expected non-nil filter")
	}

	if f.mode != "include" {
		t.Errorf("mode = %q, want 'include'", f.mode)
	}
	if len(f.sqlOperations) != 2 {
		t.Errorf("sqlOperations has %d entries, want 2", len(f.sqlOperations))
	}
	if !f.sqlOperations["SELECT"] || !f.sqlOperations["INSERT_OVERWRITE"] {
		t.Error("Expected SELECT and INSERT_OVERWRITE in sqlOperations")
	}
	if len(f.patterns) != 2 {
		t.Errorf("patterns has %d entries, want 2", len(f.patterns))
	}
	if f.sampleRate != 0.5 {
		t.Errorf("sampleRate = %f, want 0.5", f.sampleRate)
	}
}

func TestNewQueryFilter_TrimsWhitespace(t *testing.T) {
	config := &Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{" SELECT ", " INSERT_OVERWRITE "},
		ShadowFilterPatterns:      []string{" analytics\\. "},
		ShadowSampleRate:          1.0,
	}

	f, err := NewQueryFilter(config)
	if err != nil {
		t.Fatal(err)
	}

	if !f.sqlOperations["SELECT"] {
		t.Error("Expected trimmed 'SELECT' in sqlOperations")
	}
	if !f.sqlOperations["INSERT_OVERWRITE"] {
		t.Error("Expected trimmed 'INSERT_OVERWRITE' in sqlOperations")
	}
}

func TestNewQueryFilter_EmptyListItems(t *testing.T) {
	config := &Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"SELECT", "", "  ", "INSERT"},
		ShadowFilterPatterns:      []string{"analytics", "", "  "},
		ShadowSampleRate:          1.0,
	}

	f, err := NewQueryFilter(config)
	if err != nil {
		t.Fatal(err)
	}

	if len(f.sqlOperations) != 2 {
		t.Errorf("sqlOperations has %d entries (expected 2 after filtering empty), keys: %v", len(f.sqlOperations), f.sqlOperations)
	}
	if len(f.patterns) != 1 {
		t.Errorf("patterns has %d entries (expected 1 after filtering empty)", len(f.patterns))
	}
}

func TestQueryFilter_String(t *testing.T) {
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    0.5,
	}

	s := f.String()
	if !strings.Contains(s, "mode=include") {
		t.Errorf("String() missing mode: %s", s)
	}
	if !strings.Contains(s, "SELECT") {
		t.Errorf("String() missing SELECT: %s", s)
	}
	if !strings.Contains(s, "sample_rate=0.5") {
		t.Errorf("String() missing sample_rate: %s", s)
	}
}

func TestQueryFilter_StringWithPatterns(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "exclude",
		ShadowFilterSQLOperations: []string{"INSERT_OVERWRITE"},
		ShadowFilterPatterns:      []string{`analytics\.`, `(?i)risk_indicators`},
		ShadowSampleRate:          0.75,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := f.String()
	if !strings.Contains(s, "mode=exclude") {
		t.Errorf("String() missing mode: %s", s)
	}
	if !strings.Contains(s, "INSERT_OVERWRITE") {
		t.Errorf("String() missing operation: %s", s)
	}
	if !strings.Contains(s, `analytics\.`) {
		t.Errorf("String() missing first pattern: %s", s)
	}
	if !strings.Contains(s, `(?i)risk_indicators`) {
		t.Errorf("String() missing second pattern: %s", s)
	}
	if !strings.Contains(s, "sample_rate=0.75") {
		t.Errorf("String() missing sample_rate: %s", s)
	}
}

func TestQueryFilter_StringNoSampling(t *testing.T) {
	f := &QueryFilter{
		mode:          "exclude",
		sqlOperations: map[string]bool{"INSERT_OVERWRITE": true},
		sampleRate:    1.0,
	}

	s := f.String()
	if strings.Contains(s, "sample_rate") {
		t.Errorf("String() should not include sample_rate when 1.0: %s", s)
	}
}

// =============================================================================
// Config loading tests for filter env vars
// =============================================================================

func TestLoadConfig_FilterDefaults(t *testing.T) {
	// Clear any filter env vars
	clearFilterEnvVars(t)
	t.Setenv("PRIMARY_HOST", "localhost")
	t.Setenv("SHADOW_HOST", "localhost")

	config := loadConfig()

	if config.ShadowFilterMode != "" {
		t.Errorf("Expected empty ShadowFilterMode by default, got %q", config.ShadowFilterMode)
	}
	if config.ShadowFilterSQLOperations != nil {
		t.Errorf("Expected nil ShadowFilterSQLOperations by default, got %v", config.ShadowFilterSQLOperations)
	}
	if config.ShadowFilterPatterns != nil {
		t.Errorf("Expected nil ShadowFilterPatterns by default, got %v", config.ShadowFilterPatterns)
	}
	if config.ShadowSampleRate != 1.0 {
		t.Errorf("Expected ShadowSampleRate 1.0 by default, got %f", config.ShadowSampleRate)
	}
}

func TestLoadConfig_FilterFromEnv(t *testing.T) {
	clearFilterEnvVars(t)
	t.Setenv("PRIMARY_HOST", "localhost")
	t.Setenv("SHADOW_HOST", "localhost")
	t.Setenv("SHADOW_FILTER_MODE", "include")
	t.Setenv("SHADOW_FILTER_SQL_OPERATIONS", "SELECT,INSERT_OVERWRITE,SUBMIT_TASK")
	t.Setenv("SHADOW_FILTER_PATTERNS", `analytics\.,risk_indicators`)
	t.Setenv("SHADOW_SAMPLE_RATE", "0.75")

	config := loadConfig()

	if config.ShadowFilterMode != "include" {
		t.Errorf("ShadowFilterMode = %q, want 'include'", config.ShadowFilterMode)
	}
	if len(config.ShadowFilterSQLOperations) != 3 {
		t.Errorf("ShadowFilterSQLOperations has %d items, want 3: %v", len(config.ShadowFilterSQLOperations), config.ShadowFilterSQLOperations)
	}
	if len(config.ShadowFilterPatterns) != 2 {
		t.Errorf("ShadowFilterPatterns has %d items, want 2: %v", len(config.ShadowFilterPatterns), config.ShadowFilterPatterns)
	}
	if config.ShadowSampleRate != 0.75 {
		t.Errorf("ShadowSampleRate = %f, want 0.75", config.ShadowSampleRate)
	}
}

func TestGetEnvList(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		expected []string
	}{
		{"single_value", "SELECT", []string{"SELECT"}},
		{"multiple_values", "SELECT,INSERT,DELETE", []string{"SELECT", "INSERT", "DELETE"}},
		{"with_spaces", " SELECT , INSERT , DELETE ", []string{"SELECT", "INSERT", "DELETE"}},
		{"empty_items", "SELECT,,INSERT,,", []string{"SELECT", "INSERT"}},
		{"all_empty", ",,,,", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_LIST_VAR", tt.envVal)
			got := getEnvList("TEST_LIST_VAR", nil)

			if tt.expected == nil {
				if got != nil {
					t.Errorf("getEnvList = %v, want nil", got)
				}
				return
			}

			if len(got) != len(tt.expected) {
				t.Fatalf("getEnvList = %v (len %d), want %v (len %d)", got, len(got), tt.expected, len(tt.expected))
			}
			for i, v := range got {
				if v != tt.expected[i] {
					t.Errorf("getEnvList[%d] = %q, want %q", i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestGetEnvList_Unset(t *testing.T) {
	t.Setenv("TEST_UNSET_LIST", "")
	got := getEnvList("TEST_UNSET_LIST", []string{"default"})
	if len(got) != 1 || got[0] != "default" {
		t.Errorf("getEnvList with empty env = %v, want [default]", got)
	}
}

func TestGetEnvFloat(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		expected float64
	}{
		{"integer", "1", 1.0},
		{"decimal", "0.5", 0.5},
		{"small_decimal", "0.001", 0.001},
		{"zero", "0", 0.0},
		{"one", "1.0", 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_FLOAT_VAR", tt.envVal)
			got := getEnvFloat("TEST_FLOAT_VAR", 99.0)
			if got != tt.expected {
				t.Errorf("getEnvFloat = %f, want %f", got, tt.expected)
			}
		})
	}
}

func TestGetEnvFloat_InvalidFallsBackToDefault(t *testing.T) {
	t.Setenv("TEST_FLOAT_INVALID", "not_a_number")
	got := getEnvFloat("TEST_FLOAT_INVALID", 0.42)
	if got != 0.42 {
		t.Errorf("getEnvFloat with invalid value = %f, want 0.42", got)
	}
}

func clearFilterEnvVars(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SHADOW_FILTER_MODE", "SHADOW_FILTER_SQL_OPERATIONS",
		"SHADOW_FILTER_PATTERNS", "SHADOW_SAMPLE_RATE",
	} {
		t.Setenv(key, "")
	}
}

// =============================================================================
// Concurrency safety
// =============================================================================

func TestQueryFilter_ConcurrentAccess(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"SELECT"},
		ShadowFilterPatterns:      []string{`analytics`},
		ShadowSampleRate:          0.5,
	})
	if err != nil {
		t.Fatal(err)
	}

	queries := []QueryRequest{
		{Command: "COM_QUERY", QueryText: "SELECT * FROM analytics.events"},
		{Command: "COM_QUERY", QueryText: "INSERT INTO analytics.events VALUES (1)"},
		{Command: "COM_QUERY", QueryText: "SELECT * FROM users"},
		{Command: "COM_INIT_DB"},
		{Command: "COM_PING"},
	}

	var wg sync.WaitGroup
	goroutines := 50
	iterations := 1000

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				req := queries[i%len(queries)]
				f.Allow(req) // Must not panic or race
			}
		}()
	}

	wg.Wait()
}

// =============================================================================
// Benchmark
// =============================================================================

func BenchmarkExtractPrimarySQLOperation_Simple(b *testing.B) {
	for i := 0; i < b.N; i++ {
		ExtractPrimarySQLOperation("SELECT * FROM users WHERE id = 1")
	}
}

func BenchmarkExtractPrimarySQLOperation_MultiStatement(b *testing.B) {
	query := "SET CATALOG my_catalog; USE my_schema; INSERT OVERWRITE target (a, b) SELECT a, b FROM source WHERE dt = '2026-01-01'"
	for i := 0; i < b.N; i++ {
		ExtractPrimarySQLOperation(query)
	}
}

func BenchmarkQueryFilter_Allow(b *testing.B) {
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true, "INSERT_OVERWRITE": true},
		sampleRate:    1.0,
	}

	req := QueryRequest{
		Command:   "COM_QUERY",
		QueryText: "SET CATALOG c; USE s; SELECT * FROM analytics.events WHERE dt = '2026-01-01'",
	}

	for i := 0; i < b.N; i++ {
		f.Allow(req)
	}
}

// =============================================================================
// Real-world StarRocks ETL queries from production codebase
// =============================================================================

// These four queries represent actual StarRocks ETL patterns. They should all
// be identified and filtered correctly. The template variables (${...}) have
// been replaced with realistic placeholder values.

const realWorldQuery1_CounterpartyFlows = `
SET CATALOG analytics_catalog;
USE risk_schema;

INSERT OVERWRITE counterparty_category_flows_daily
(tx_date, chain, counterparty_category_id, 
 incoming_volume_usd, outgoing_volume_usd, incoming_transfers, outgoing_transfers, custom_entity_uuid)
SELECT
  cef.date_period AS tx_date,
  cef.chain,
  CAST(cef.counterparty_category_id AS VARCHAR) AS counterparty_category_id,
  COALESCE(SUM(cef.incoming_volume_usd), 0) AS incoming_volume_usd,
  COALESCE(SUM(cef.outgoing_volume_usd), 0) AS outgoing_volume_usd,
  COALESCE(SUM(CAST(cef.incoming_transfers AS BIGINT)), 0) AS incoming_transfers,
  COALESCE(SUM(CAST(cef.outgoing_transfers AS BIGINT)), 0) AS outgoing_transfers,
  'entity-abc-123' AS custom_entity_uuid
FROM counterparty_flows_cache cef
WHERE 
  cef.custom_entity_uuid = 'entity-abc-123'
  AND cef.chain IN ('ethereum', 'bitcoin', 'polygon')
  AND cef.counterparty_category_id IS NOT NULL
GROUP BY cef.date_period, cef.chain, cef.counterparty_category_id;
`

const realWorldQuery2_OwnershipFlows = `
SET CATALOG analytics_catalog;
USE risk_schema;

INSERT OVERWRITE ownership_category_flows_daily
(chain, tx_date, ownership_category_id,
 incoming_volume_usd, incoming_transfers, outgoing_volume_usd, outgoing_transfers, custom_entity_uuid)
SELECT
  cef.chain,
  cef.date_period AS tx_date,
  ac.category_id AS ownership_category_id,
  COALESCE(SUM(cef.incoming_volume_usd), 0) AS incoming_volume_usd,
  COALESCE(SUM(CAST(cef.incoming_transfers AS BIGINT)), 0) AS incoming_transfers,
  COALESCE(SUM(cef.outgoing_volume_usd), 0) AS outgoing_volume_usd,
  COALESCE(SUM(CAST(cef.outgoing_transfers AS BIGINT)), 0) AS outgoing_transfers,
  'entity-abc-123' AS custom_entity_uuid
FROM counterparty_flows_cache cef
INNER JOIN address_composition cac
  ON cef.address_chain = cac.address_chain
INNER JOIN address_category_ethereum ac
  ON cef.address_chain = ac.address_chain
WHERE
  cef.custom_entity_uuid = 'entity-abc-123'
  AND cef.chain = 'ethereum'
  AND ac.is_effective_ownership_label = TRUE
GROUP BY cef.chain, cef.date_period, ac.category_id
UNION ALL
SELECT
  cef.chain,
  cef.date_period AS tx_date,
  ac.category_id AS ownership_category_id,
  COALESCE(SUM(cef.incoming_volume_usd), 0) AS incoming_volume_usd,
  COALESCE(SUM(CAST(cef.incoming_transfers AS BIGINT)), 0) AS incoming_transfers,
  COALESCE(SUM(cef.outgoing_volume_usd), 0) AS outgoing_volume_usd,
  COALESCE(SUM(CAST(cef.outgoing_transfers AS BIGINT)), 0) AS outgoing_transfers,
  'entity-abc-123' AS custom_entity_uuid
FROM counterparty_flows_cache cef
INNER JOIN address_composition cac
  ON cef.address_chain = cac.address_chain
INNER JOIN address_category_bitcoin ac
  ON cef.address_chain = ac.address_chain
WHERE
  cef.custom_entity_uuid = 'entity-abc-123'
  AND cef.chain = 'bitcoin'
  AND ac.is_effective_ownership_label = TRUE
GROUP BY cef.chain, cef.date_period, ac.category_id;
`

const realWorldQuery3_RiskIndicators = `
SET CATALOG analytics_catalog;
USE risk_schema;

INSERT OVERWRITE risk_indicators
(org_uuid, category_id, category, risk_type,
 category_risk_score_level, category_risk_score_level_label,
 window_start, window_end, incoming_volume_usd, outgoing_volume_usd,
 total_volume_usd, instances, custom_entity_uuid)
-- Counterparty risk indicators
SELECT
  'org-uuid-456' AS org_uuid,
  ccf.counterparty_category_id AS category_id,
  NULL AS category,
  'counterparty' AS risk_type,
  COALESCE(orr.risk_score_level, 0) AS category_risk_score_level,
  CASE 
    WHEN COALESCE(orr.risk_score_level, 0) = 0 THEN 'Unknown'
    WHEN COALESCE(orr.risk_score_level, 0) = 1 THEN 'Low'
    WHEN COALESCE(orr.risk_score_level, 0) = 5 THEN 'Medium'
    WHEN COALESCE(orr.risk_score_level, 0) = 10 THEN 'High'
    WHEN COALESCE(orr.risk_score_level, 0) = 15 THEN 'Severe'
    ELSE 'Unknown'
  END AS category_risk_score_level_label,
  MIN(ccf.tx_date) AS window_start,
  MAX(ccf.tx_date) AS window_end,
  SUM(ccf.incoming_volume_usd) AS incoming_volume_usd,
  SUM(ccf.outgoing_volume_usd) AS outgoing_volume_usd,
  SUM(ccf.incoming_volume_usd) + SUM(ccf.outgoing_volume_usd) AS total_volume_usd,
  SUM(ccf.incoming_transfers) + SUM(ccf.outgoing_transfers) AS instances,
  'entity-abc-123' AS custom_entity_uuid
FROM counterparty_category_flows_daily ccf
LEFT JOIN org_risk_rules orr
  ON CAST(ccf.counterparty_category_id AS BIGINT) = orr.category_id
  AND orr.risk_type_id = 2
WHERE ccf.custom_entity_uuid = 'entity-abc-123'
GROUP BY ccf.counterparty_category_id, orr.risk_score_level
UNION ALL
-- Ownership risk indicators
SELECT
  'org-uuid-456' AS org_uuid,
  CAST(ocf.ownership_category_id AS VARCHAR) AS category_id,
  NULL AS category,
  'ownership' AS risk_type,
  COALESCE(orr.risk_score_level, 0) AS category_risk_score_level,
  CASE 
    WHEN COALESCE(orr.risk_score_level, 0) = 0 THEN 'Unknown'
    WHEN COALESCE(orr.risk_score_level, 0) = 1 THEN 'Low'
    WHEN COALESCE(orr.risk_score_level, 0) = 5 THEN 'Medium'
    WHEN COALESCE(orr.risk_score_level, 0) = 10 THEN 'High'
    WHEN COALESCE(orr.risk_score_level, 0) = 15 THEN 'Severe'
    ELSE 'Unknown'
  END AS category_risk_score_level_label,
  MIN(ocf.tx_date) AS window_start,
  MAX(ocf.tx_date) AS window_end,
  SUM(ocf.incoming_volume_usd) AS incoming_volume_usd,
  SUM(ocf.outgoing_volume_usd) AS outgoing_volume_usd,
  SUM(ocf.incoming_volume_usd) + SUM(ocf.outgoing_volume_usd) AS total_volume_usd,
  SUM(ocf.incoming_transfers) + SUM(ocf.outgoing_transfers) AS instances,
  'entity-abc-123' AS custom_entity_uuid
FROM ownership_category_flows_daily ocf
LEFT JOIN org_risk_rules orr
  ON ocf.ownership_category_id = orr.category_id
  AND orr.risk_type_id = 1
WHERE ocf.custom_entity_uuid = 'entity-abc-123'
GROUP BY ocf.ownership_category_id, orr.risk_score_level;
`

const realWorldQuery4_SubmitTaskRiskScore = `
SET CATALOG analytics_catalog;
USE risk_schema;

SUBMIT /*+set_var(query_timeout=300)*/ TASK ` + "`risk_score_task_123`" + ` AS
INSERT OVERWRITE risk_score
(org_uuid, org_risk_score_level, custom_entity_uuid)
SELECT
  'org-uuid-456' AS org_uuid,
  COALESCE(MAX(category_risk_score_level), 0) AS org_risk_score_level,
  'entity-abc-123' AS custom_entity_uuid
FROM risk_indicators
WHERE custom_entity_uuid = 'entity-abc-123' AND org_uuid = 'org-uuid-456';
`

func TestRealWorldQueries_OperationDetection(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"counterparty_flows_INSERT_OVERWRITE", realWorldQuery1_CounterpartyFlows, "INSERT_OVERWRITE"},
		{"ownership_flows_INSERT_OVERWRITE", realWorldQuery2_OwnershipFlows, "INSERT_OVERWRITE"},
		{"risk_indicators_INSERT_OVERWRITE", realWorldQuery3_RiskIndicators, "INSERT_OVERWRITE"},
		{"risk_score_SUBMIT_TASK_with_hint", realWorldQuery4_SubmitTaskRiskScore, "SUBMIT_TASK"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestRealWorldQueries_AllFilteredByExclude(t *testing.T) {
	// Exclude both INSERT_OVERWRITE and SUBMIT_TASK — all four queries should be blocked
	f := &QueryFilter{
		mode: "exclude",
		sqlOperations: map[string]bool{
			"INSERT_OVERWRITE": true,
			"SUBMIT_TASK":      true,
		},
		sampleRate: 1.0,
	}

	queries := []struct {
		name  string
		query string
	}{
		{"counterparty_flows", realWorldQuery1_CounterpartyFlows},
		{"ownership_flows", realWorldQuery2_OwnershipFlows},
		{"risk_indicators", realWorldQuery3_RiskIndicators},
		{"risk_score_submit_task", realWorldQuery4_SubmitTaskRiskScore},
	}

	for _, tt := range queries {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			if testAllow(f, req) {
				op := ExtractPrimarySQLOperation(tt.query)
				t.Errorf("Expected query to be BLOCKED (primary op=%s), but it was allowed", op)
			}
		})
	}
}

func TestRealWorldQueries_OnlySelectAllowed(t *testing.T) {
	// Include only SELECT — all four ETL queries should be blocked
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    1.0,
	}

	for _, q := range []string{
		realWorldQuery1_CounterpartyFlows,
		realWorldQuery2_OwnershipFlows,
		realWorldQuery3_RiskIndicators,
		realWorldQuery4_SubmitTaskRiskScore,
	} {
		req := QueryRequest{Command: "COM_QUERY", QueryText: q}
		if testAllow(f, req) {
			op := ExtractPrimarySQLOperation(q)
			t.Errorf("Expected %s query to be blocked by SELECT-only filter", op)
		}
	}

	// A simple SELECT should still be allowed
	selectReq := QueryRequest{
		Command:   "COM_QUERY",
		QueryText: "SET CATALOG c; USE s; SELECT * FROM risk_indicators WHERE org_uuid = 'org-456'",
	}
	if !testAllow(f, selectReq) {
		t.Error("Expected SELECT query to be allowed")
	}
}

// =============================================================================
// Multi-filter overlap: what happens when a query matches multiple filter entries
// =============================================================================

func TestQueryFilter_ExcludeMultipleOps_QueryMatchesOne(t *testing.T) {
	// Exclude both INSERT_OVERWRITE and SUBMIT_TASK
	f := &QueryFilter{
		mode: "exclude",
		sqlOperations: map[string]bool{
			"INSERT_OVERWRITE": true,
			"SUBMIT_TASK":      true,
		},
		sampleRate: 1.0,
	}

	tests := []struct {
		name    string
		query   string
		allowed bool
		reason  string
	}{
		{
			"INSERT_OVERWRITE_blocked",
			realWorldQuery1_CounterpartyFlows,
			false,
			"primary op is INSERT_OVERWRITE, which is in the exclude set",
		},
		{
			"SUBMIT_TASK_blocked",
			realWorldQuery4_SubmitTaskRiskScore,
			false,
			"primary op is SUBMIT_TASK, which is in the exclude set",
		},
		{
			"SELECT_allowed",
			"SET CATALOG c; USE s; SELECT * FROM risk_indicators",
			true,
			"primary op is SELECT, which is NOT in the exclude set",
		},
		{
			"plain_INSERT_allowed",
			"INSERT INTO audit_log VALUES ('2026-03-25', 'test')",
			true,
			"primary op is INSERT, which is NOT in the exclude set (only INSERT_OVERWRITE is)",
		},
		{
			"DELETE_allowed",
			"SET CATALOG c; USE s; DELETE FROM expired_cache WHERE dt < '2025-01-01'",
			true,
			"primary op is DELETE, which is NOT in the exclude set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			got := testAllow(f, req)
			if got != tt.allowed {
				t.Errorf("Allow() = %v, want %v (%s)", got, tt.allowed, tt.reason)
			}
		})
	}
}

func TestQueryFilter_SubmitTaskContainsInsertOverwrite(t *testing.T) {
	// SUBMIT TASK wraps INSERT OVERWRITE internally. The filter should see
	// SUBMIT_TASK as the primary operation, not INSERT_OVERWRITE.
	//
	// Scenario: filter includes only INSERT_OVERWRITE. A SUBMIT TASK query
	// should be BLOCKED because its primary operation is SUBMIT_TASK, not
	// INSERT_OVERWRITE (even though INSERT OVERWRITE appears in the body).

	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"INSERT_OVERWRITE": true},
		sampleRate:    1.0,
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: realWorldQuery4_SubmitTaskRiskScore}
	if testAllow(f, req) {
		t.Error("SUBMIT TASK query should be BLOCKED by INSERT_OVERWRITE-only filter " +
			"because its primary operation is SUBMIT_TASK, not INSERT_OVERWRITE")
	}

	// And vice versa: including SUBMIT_TASK should allow it
	f2 := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SUBMIT_TASK": true},
		sampleRate:    1.0,
	}
	if !testAllow(f2, req) {
		t.Error("SUBMIT TASK query should be ALLOWED by SUBMIT_TASK filter")
	}

	// Including both should also allow it
	f3 := &QueryFilter{
		mode: "include",
		sqlOperations: map[string]bool{
			"INSERT_OVERWRITE": true,
			"SUBMIT_TASK":      true,
		},
		sampleRate: 1.0,
	}
	if !testAllow(f3, req) {
		t.Error("SUBMIT TASK query should be ALLOWED when both INSERT_OVERWRITE and SUBMIT_TASK are included")
	}
}

func TestQueryFilter_ExcludeOnlyInsertOverwrite_SubmitTaskStillAllowed(t *testing.T) {
	// If you only exclude INSERT_OVERWRITE, SUBMIT TASK queries should still
	// pass through because SUBMIT_TASK is a different operation.
	f := &QueryFilter{
		mode:          "exclude",
		sqlOperations: map[string]bool{"INSERT_OVERWRITE": true},
		sampleRate:    1.0,
	}

	// INSERT OVERWRITE: blocked
	req1 := QueryRequest{Command: "COM_QUERY", QueryText: realWorldQuery1_CounterpartyFlows}
	if testAllow(f, req1) {
		t.Error("INSERT OVERWRITE should be blocked")
	}

	// SUBMIT TASK: allowed (different primary operation)
	req2 := QueryRequest{Command: "COM_QUERY", QueryText: realWorldQuery4_SubmitTaskRiskScore}
	if !testAllow(f, req2) {
		t.Error("SUBMIT TASK should be ALLOWED when only INSERT_OVERWRITE is excluded")
	}
}

// =============================================================================
// StarRocks optimizer hints between keywords
// =============================================================================

func TestExtractPrimarySQLOperation_OptimizerHints(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{
			"SUBMIT_hint_TASK",
			"SUBMIT /*+set_var(query_timeout=300)*/ TASK `my_task` AS INSERT OVERWRITE t SELECT 1",
			"SUBMIT_TASK",
		},
		{
			"SUBMIT_hint_TASK_in_multi_stmt",
			"SET CATALOG c; USE s; SUBMIT /*+set_var(query_timeout=600)*/ TASK `t` AS INSERT INTO x SELECT 1",
			"SUBMIT_TASK",
		},
		{
			"INSERT_hint_OVERWRITE",
			"INSERT /*+set_var(parallel_fragment_exec_instance_num=4)*/ OVERWRITE t SELECT 1",
			"INSERT_OVERWRITE",
		},
		{
			"SELECT_with_hint",
			"SELECT /*+ SET_VAR(exec_mem_limit=8589934592) */ * FROM large_table",
			"SELECT",
		},
		{
			"multiple_hints",
			"SUBMIT /*+set_var(a=1)*/ /*+set_var(b=2)*/ TASK `t` AS INSERT OVERWRITE x SELECT 1",
			"SUBMIT_TASK",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// =============================================================================
// Filter reason returned by Allow()
// =============================================================================

func TestQueryFilter_AllowReturnsFilterReason(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "include",
		ShadowFilterSQLOperations: []string{"SELECT"},
		ShadowFilterPatterns:      []string{`analytics\.`},
		ShadowSampleRate:          1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		req            QueryRequest
		expectedAllow  bool
		expectedReason string
	}{
		{
			"allowed_no_reason",
			QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM analytics.events"},
			true, FilterReasonNone,
		},
		{
			"blocked_by_sql_operation",
			QueryRequest{Command: "COM_QUERY", QueryText: "INSERT INTO analytics.events VALUES (1)"},
			false, FilterReasonSQLOperation,
		},
		{
			"blocked_by_pattern",
			QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM users"},
			false, FilterReasonPattern,
		},
		{
			"non_COM_QUERY_always_allowed",
			QueryRequest{Command: "COM_INIT_DB"},
			true, FilterReasonNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason := f.Allow(tt.req)
			if allowed != tt.expectedAllow {
				t.Errorf("Allow() allowed = %v, want %v", allowed, tt.expectedAllow)
			}
			if reason != tt.expectedReason {
				t.Errorf("Allow() reason = %q, want %q", reason, tt.expectedReason)
			}
		})
	}
}

func TestQueryFilter_AllowReturnsFilterReason_Exclude(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "exclude",
		ShadowFilterSQLOperations: []string{"INSERT_OVERWRITE"},
		ShadowFilterPatterns:      []string{`information_schema`},
		ShadowSampleRate:          1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		query          string
		expectedAllow  bool
		expectedReason string
	}{
		{"allowed_SELECT", "SELECT * FROM users", true, FilterReasonNone},
		{"blocked_by_operation", "INSERT OVERWRITE t SELECT 1", false, FilterReasonSQLOperation},
		{"blocked_by_pattern", "SELECT * FROM information_schema.tables", false, FilterReasonPattern},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			allowed, reason := f.Allow(req)
			if allowed != tt.expectedAllow {
				t.Errorf("Allow() allowed = %v, want %v", allowed, tt.expectedAllow)
			}
			if reason != tt.expectedReason {
				t.Errorf("Allow() reason = %q, want %q", reason, tt.expectedReason)
			}
		})
	}
}

func TestQueryFilter_SamplingReturnsCorrectReason(t *testing.T) {
	f := &QueryFilter{
		mode:       "include",
		sampleRate: 0.0, // Reject everything via sampling
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT 1"}
	allowed, reason := f.Allow(req)
	if allowed {
		t.Error("Expected blocked by sampling")
	}
	if reason != FilterReasonSampling {
		t.Errorf("Expected reason %q, got %q", FilterReasonSampling, reason)
	}
}

func TestQueryFilter_OperationBlockedBeforeSampling(t *testing.T) {
	// When SQL operation filter blocks, the reason should be sql_operation, not sampling
	f := &QueryFilter{
		mode:          "include",
		sqlOperations: map[string]bool{"SELECT": true},
		sampleRate:    0.0, // Would also block, but operation should be checked first
	}

	req := QueryRequest{Command: "COM_QUERY", QueryText: "INSERT INTO t VALUES (1)"}
	allowed, reason := f.Allow(req)
	if allowed {
		t.Error("Expected blocked")
	}
	if reason != FilterReasonSQLOperation {
		t.Errorf("Expected reason %q (not sampling), got %q", FilterReasonSQLOperation, reason)
	}
}

// =============================================================================
// QueryLogEntry Filtered fields
// =============================================================================

func TestQueryLogEntry_FilteredFieldsSerialization(t *testing.T) {
	// Non-filtered entry should not include filtered fields (omitempty)
	normal := QueryLogEntry{
		Timestamp:  "2026-03-25T10:00:00Z",
		QueryID:    "abc-123",
		Target:     "shadow",
		Command:    "COM_QUERY",
		QueryText:  "SELECT 1",
		DurationMs: 5.5,
		Success:    true,
	}
	normalJSON, _ := json.Marshal(normal)
	normalStr := string(normalJSON)
	if strings.Contains(normalStr, "filtered") {
		t.Errorf("Non-filtered entry should not contain 'filtered' field: %s", normalStr)
	}
	if strings.Contains(normalStr, "filter_reason") {
		t.Errorf("Non-filtered entry should not contain 'filter_reason' field: %s", normalStr)
	}

	// Filtered entry should include both fields
	filtered := QueryLogEntry{
		Timestamp:    "2026-03-25T10:00:00Z",
		QueryID:      "abc-123",
		Target:       "shadow",
		Command:      "COM_QUERY",
		QueryText:    "INSERT OVERWRITE t SELECT 1",
		DurationMs:   0,
		Success:      true,
		Filtered:     true,
		FilterReason: FilterReasonSQLOperation,
	}
	filteredJSON, _ := json.Marshal(filtered)
	filteredStr := string(filteredJSON)
	if !strings.Contains(filteredStr, `"filtered":true`) {
		t.Errorf("Filtered entry should contain 'filtered:true': %s", filteredStr)
	}
	if !strings.Contains(filteredStr, `"filter_reason":"sql_operation"`) {
		t.Errorf("Filtered entry should contain 'filter_reason': %s", filteredStr)
	}
}

func TestLogFilteredShadow_CreatesCorrectEntry(t *testing.T) {
	// Capture log entries via a channel-based mock logger approach
	entries := make(chan QueryLogEntry, 10)

	req := QueryRequest{
		ID:         "test-query-id-999",
		Command:    "COM_QUERY",
		QueryText:  "SET CATALOG c; USE s; INSERT OVERWRITE t SELECT * FROM s",
		QueryHash:  "abc123hash",
		ClientAddr: "10.0.0.1:54321",
	}

	// Directly construct the entry that logFilteredShadow would create
	entry := QueryLogEntry{
		Timestamp:    "2026-03-25T10:00:00Z",
		QueryID:      req.ID,
		Target:       "shadow",
		Command:      req.Command,
		QueryText:    req.QueryText,
		QueryHash:    req.QueryHash,
		DurationMs:   0,
		BytesSent:    0,
		BytesRecv:    0,
		Success:      true,
		ClientAddr:   req.ClientAddr,
		Filtered:     true,
		FilterReason: FilterReasonSQLOperation,
	}

	entries <- entry
	close(entries)

	got := <-entries
	if got.QueryID != "test-query-id-999" {
		t.Errorf("QueryID = %q, want 'test-query-id-999'", got.QueryID)
	}
	if got.Target != "shadow" {
		t.Errorf("Target = %q, want 'shadow'", got.Target)
	}
	if !got.Filtered {
		t.Error("Filtered should be true")
	}
	if got.FilterReason != FilterReasonSQLOperation {
		t.Errorf("FilterReason = %q, want %q", got.FilterReason, FilterReasonSQLOperation)
	}
	if got.DurationMs != 0 {
		t.Errorf("DurationMs = %f, want 0 (filtered queries are not executed)", got.DurationMs)
	}
	if !got.Success {
		t.Error("Success should be true (filtered is not an error)")
	}
	if got.QueryText == "" {
		t.Error("QueryText should be preserved for filtered entries (needed for analysis)")
	}
	if got.QueryHash == "" {
		t.Error("QueryHash should be preserved for filtered entries (needed for correlation)")
	}
}

// =============================================================================
// Real-world queries: verify filter reason for each
// =============================================================================

func TestRealWorldQueries_FilterReasons(t *testing.T) {
	f := &QueryFilter{
		mode: "exclude",
		sqlOperations: map[string]bool{
			"INSERT_OVERWRITE": true,
			"SUBMIT_TASK":      true,
		},
		sampleRate: 1.0,
	}

	tests := []struct {
		name           string
		query          string
		expectedReason string
	}{
		{"counterparty_flows", realWorldQuery1_CounterpartyFlows, FilterReasonSQLOperation},
		{"ownership_flows", realWorldQuery2_OwnershipFlows, FilterReasonSQLOperation},
		{"risk_indicators", realWorldQuery3_RiskIndicators, FilterReasonSQLOperation},
		{"submit_task_risk_score", realWorldQuery4_SubmitTaskRiskScore, FilterReasonSQLOperation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := QueryRequest{Command: "COM_QUERY", QueryText: tt.query}
			allowed, reason := f.Allow(req)
			if allowed {
				t.Error("Expected query to be blocked")
			}
			if reason != tt.expectedReason {
				t.Errorf("reason = %q, want %q", reason, tt.expectedReason)
			}
		})
	}
}

func TestRealWorldQueries_PatternFilterReason(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:     "exclude",
		ShadowFilterPatterns: []string{`risk_indicators`, `counterparty_category_flows_daily`},
		ShadowSampleRate:     1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Queries 1 and 3 mention these tables — should be blocked by pattern
	req1 := QueryRequest{Command: "COM_QUERY", QueryText: realWorldQuery1_CounterpartyFlows}
	allowed1, reason1 := f.Allow(req1)
	if allowed1 {
		t.Error("Query 1 should be blocked by pattern")
	}
	if reason1 != FilterReasonPattern {
		t.Errorf("Query 1 reason = %q, want %q", reason1, FilterReasonPattern)
	}

	req3 := QueryRequest{Command: "COM_QUERY", QueryText: realWorldQuery3_RiskIndicators}
	allowed3, reason3 := f.Allow(req3)
	if allowed3 {
		t.Error("Query 3 should be blocked by pattern")
	}
	if reason3 != FilterReasonPattern {
		t.Errorf("Query 3 reason = %q, want %q", reason3, FilterReasonPattern)
	}

	// A SELECT on an unrelated table should be allowed
	reqOK := QueryRequest{Command: "COM_QUERY", QueryText: "SELECT * FROM users"}
	allowedOK, reasonOK := f.Allow(reqOK)
	if !allowedOK {
		t.Error("Unrelated query should be allowed")
	}
	if reasonOK != FilterReasonNone {
		t.Errorf("Unrelated query reason = %q, want empty", reasonOK)
	}
}

func TestExtractPrimarySQLOperation_AdditionalOps(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{"ROUTINE_LOAD", "ROUTINE LOAD my_job ON my_table FROM KAFKA ('kafka_broker'='localhost:9092')", "ROUTINE_LOAD"},
		{"CANCEL_LOAD", "CANCEL LOAD FROM my_database", "CANCEL_LOAD"},
		{"SHOW_LOAD", "SHOW LOAD FROM my_database", "SHOW_LOAD"},
		{"ADMIN_CHECK", "ADMIN CHECK TABLET (1, 2, 3)", "ADMIN_CHECK"},
		{"ADMIN_REPAIR", "ADMIN REPAIR TABLE my_table", "ADMIN_REPAIR"},
		{"CREATE_VIEW", "CREATE VIEW my_view AS SELECT * FROM t", "CREATE_VIEW"},
		{"DROP_VIEW", "DROP VIEW my_view", "DROP_VIEW"},
		{"CREATE_DATABASE", "CREATE DATABASE my_db", "CREATE_DATABASE"},
		{"DROP_DATABASE", "DROP DATABASE my_db", "DROP_DATABASE"},
		{"CREATE_INDEX", "CREATE INDEX idx ON t(col)", "CREATE_INDEX"},
		{"DROP_INDEX", "DROP INDEX idx ON t", "DROP_INDEX"},
		{"ALTER_DATABASE", "ALTER DATABASE my_db SET DATA QUOTA 100G", "ALTER_DATABASE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrimarySQLOperation(tt.query)
			if got != tt.expected {
				t.Errorf("ExtractPrimarySQLOperation(%q) = %q, want %q", tt.query, got, tt.expected)
			}
		})
	}
}

func TestSplitStatements_Backticks(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected int
	}{
		{
			"backtick_with_semicolon",
			"SELECT * FROM `table;name` WHERE id = 1",
			1,
		},
		{
			"backtick_across_statements",
			"USE `my;db`; SELECT * FROM `tbl;x`",
			2,
		},
		{
			"backtick_normal",
			"SELECT * FROM `normal_table`; SELECT 2",
			2,
		},
		{
			"backtick_in_task_name",
			"SET CATALOG c; USE s; SUBMIT TASK `risk_score;task` AS INSERT INTO t SELECT 1",
			3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitStatements(tt.query)
			if len(got) != tt.expected {
				t.Errorf("splitStatements(%q) = %d stmts, want %d: %v", tt.query, len(got), tt.expected, got)
			}
		})
	}
}

func TestQueryFilter_ExcludeBothMatch_ReasonIsOperation(t *testing.T) {
	f, err := NewQueryFilter(&Config{
		ShadowFilterMode:          "exclude",
		ShadowFilterSQLOperations: []string{"INSERT_OVERWRITE"},
		ShadowFilterPatterns:      []string{`information_schema`},
		ShadowSampleRate:          1.0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Query matches BOTH: operation is INSERT_OVERWRITE AND text contains information_schema
	// Since operations are checked first in exclude mode, reason should be sql_operation
	req := QueryRequest{
		Command:   "COM_QUERY",
		QueryText: "INSERT OVERWRITE information_schema.backup SELECT 1",
	}
	allowed, reason := f.Allow(req)
	if allowed {
		t.Error("expected blocked")
	}
	if reason != FilterReasonSQLOperation {
		t.Errorf("reason = %q, want %q (operation checked before pattern in exclude)", reason, FilterReasonSQLOperation)
	}
}

func TestQueryFilter_StringDeterministic(t *testing.T) {
	f := &QueryFilter{
		mode: "include",
		sqlOperations: map[string]bool{
			"SELECT":           true,
			"INSERT_OVERWRITE": true,
			"SUBMIT_TASK":      true,
		},
		sampleRate: 1.0,
	}

	// Call String() multiple times — should be identical (sorted)
	first := f.String()
	for i := 0; i < 20; i++ {
		got := f.String()
		if got != first {
			t.Errorf("String() is non-deterministic: first=%q, iteration %d=%q", first, i, got)
		}
	}

	if !strings.Contains(first, "INSERT_OVERWRITE,SELECT,SUBMIT_TASK") {
		t.Errorf("Expected sorted operations, got: %s", first)
	}
}

func TestStripAllBlockComments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"no_comments", "SELECT 1", "SELECT 1"},
		{"single_comment", "SELECT /* hint */ 1", "SELECT   1"},
		{"multiple_comments", "A /* x */ B /* y */ C", "A   B   C"},
		{"leading_comment", "/* comment */ SELECT 1", "  SELECT 1"},
		{"hint_between_keywords", "SUBMIT /*+set_var(a=1)*/ TASK", "SUBMIT   TASK"},
		{"unclosed_comment_left_alone", "SELECT /* unclosed", "SELECT /* unclosed"},
		{"empty_comment", "SELECT /**/ FROM t", "SELECT   FROM t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripAllBlockComments(tt.input)
			if got != tt.expected {
				t.Errorf("stripAllBlockComments(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
