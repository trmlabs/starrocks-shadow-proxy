// query_filter.go — Selective query filtering for shadow mirroring.
// Supports filtering by SQL operation type, regex patterns on full query text,
// and random sampling. StarRocks-aware: handles multi-statement queries where
// SET CATALOG / USE appear before the primary operation.
package main

import (
	"fmt"
	"log"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
)

// QueryFilter decides whether a query should be mirrored to the shadow cluster.
// Only COM_QUERY commands are subject to filtering; other MySQL commands always
// pass through to keep the shadow session synchronized.
type QueryFilter struct {
	mode          string           // "include" or "exclude"
	sqlOperations map[string]bool  // Canonical operation names: "SELECT", "INSERT_OVERWRITE", etc.
	patterns      []*regexp.Regexp // Regex patterns matched against full query text
	sampleRate    float64          // 0.0 to 1.0 (1.0 = no sampling)
}

// NewQueryFilter creates a QueryFilter from config. Returns nil if no filtering
// is configured, preserving the default behavior of shadowing everything.
func NewQueryFilter(config *Config) (*QueryFilter, error) {
	hasMode := config.ShadowFilterMode != ""
	hasSQLOps := len(config.ShadowFilterSQLOperations) > 0
	hasPatterns := len(config.ShadowFilterPatterns) > 0
	hasSampling := config.ShadowSampleRate < 1.0

	if !hasMode && !hasSQLOps && !hasPatterns && !hasSampling {
		return nil, nil
	}

	if config.ShadowSampleRate < 0.0 || config.ShadowSampleRate > 1.0 {
		return nil, fmt.Errorf("invalid SHADOW_SAMPLE_RATE %.4f: must be between 0.0 and 1.0", config.ShadowSampleRate)
	}

	mode := strings.ToLower(config.ShadowFilterMode)
	if mode == "" {
		mode = "include"
	}
	if mode != "include" && mode != "exclude" {
		return nil, fmt.Errorf("invalid SHADOW_FILTER_MODE %q: must be 'include' or 'exclude'", config.ShadowFilterMode)
	}

	f := &QueryFilter{
		mode:          mode,
		sqlOperations: make(map[string]bool),
		sampleRate:    config.ShadowSampleRate,
	}

	for _, op := range config.ShadowFilterSQLOperations {
		op = strings.TrimSpace(op)
		if op != "" {
			f.sqlOperations[strings.ToUpper(op)] = true
		}
	}

	for _, p := range config.ShadowFilterPatterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid SHADOW_FILTER_PATTERN %q: %w", p, err)
		}
		f.patterns = append(f.patterns, re)
	}

	if len(f.sqlOperations) == 0 && len(f.patterns) == 0 && f.sampleRate >= 1.0 {
		log.Printf("Warning: SHADOW_FILTER_MODE=%s set but no filter criteria configured", mode)
	}

	return f, nil
}

// Filter reason constants returned by Allow() when a query is blocked.
const (
	FilterReasonNone         = ""              // Query is allowed
	FilterReasonSQLOperation = "sql_operation" // Blocked by SQL operation type filter
	FilterReasonPattern      = "pattern"       // Blocked by regex pattern filter
	FilterReasonSampling     = "sampling"      // Dropped by random sampling
)

// Allow returns true if the query should be sent to the shadow cluster, along
// with the reason if filtered. The reason is one of the FilterReason* constants.
// Commands that don't carry SQL text (MySQL: COM_PING, COM_INIT_DB, prepared-stmt
// lifecycle; pgwire: Bind/Execute/Sync/etc.) always pass through to avoid
// desyncing the shadow session. We key off req.QueryText being empty, which is
// protocol-agnostic: NewQueryRequest only populates it for COM_QUERY on the
// MySQL path, and extractPgQueryText only populates it for Query/Parse on pg.
func (f *QueryFilter) Allow(req QueryRequest) (bool, string) {
	if req.QueryText == "" {
		return true, FilterReasonNone
	}

	if allowed, reason := f.passesFilter(req); !allowed {
		return false, reason
	}

	if f.sampleRate < 1.0 {
		if rand.Float64() >= f.sampleRate {
			return false, FilterReasonSampling
		}
	}

	return true, FilterReasonNone
}

func (f *QueryFilter) passesFilter(req QueryRequest) (bool, string) {
	switch f.mode {
	case "include":
		return f.matchInclude(req)
	case "exclude":
		return f.matchExclude(req)
	default:
		return true, FilterReasonNone
	}
}

// matchInclude: query is shadowed only if it matches ALL configured criteria.
// SQL operations and patterns are AND'd. Within each, OR logic applies.
func (f *QueryFilter) matchInclude(req QueryRequest) (bool, string) {
	if len(f.sqlOperations) > 0 {
		primaryOp := ExtractPrimarySQLOperation(req.QueryText)
		if !f.sqlOperations[primaryOp] {
			return false, FilterReasonSQLOperation
		}
	}

	if len(f.patterns) > 0 {
		if !f.matchAnyPattern(req.QueryText) {
			return false, FilterReasonPattern
		}
	}

	return true, FilterReasonNone
}

// matchExclude: query is blocked if it matches ANY exclusion criterion.
func (f *QueryFilter) matchExclude(req QueryRequest) (bool, string) {
	if len(f.sqlOperations) > 0 {
		primaryOp := ExtractPrimarySQLOperation(req.QueryText)
		if f.sqlOperations[primaryOp] {
			return false, FilterReasonSQLOperation
		}
	}

	if len(f.patterns) > 0 {
		if f.matchAnyPattern(req.QueryText) {
			return false, FilterReasonPattern
		}
	}

	return true, FilterReasonNone
}

func (f *QueryFilter) matchAnyPattern(queryText string) bool {
	for _, re := range f.patterns {
		if re.MatchString(queryText) {
			return true
		}
	}
	return false
}

// preambleOperations are session-setup statements that precede the real operation
// in StarRocks multi-statement queries.
var preambleOperations = map[string]bool{
	"SET":         true,
	"SET_CATALOG": true,
	"USE":         true,
}

// ExtractPrimarySQLOperation extracts the main SQL operation from a query,
// skipping StarRocks preamble statements (SET CATALOG, USE).
//
// For "SET CATALOG c; USE s; INSERT OVERWRITE t SELECT ..." returns "INSERT_OVERWRITE".
// For "SELECT * FROM t" returns "SELECT".
// For "SUBMIT TASK AS INSERT INTO t SELECT ..." returns "SUBMIT_TASK".
func ExtractPrimarySQLOperation(queryText string) string {
	statements := splitStatements(queryText)

	for i := len(statements) - 1; i >= 0; i-- {
		op := extractStatementOperation(statements[i])
		if op != "" && !preambleOperations[op] {
			return op
		}
	}

	// All statements are preamble — return the last one found
	for i := len(statements) - 1; i >= 0; i-- {
		if op := extractStatementOperation(statements[i]); op != "" {
			return op
		}
	}

	return "UNKNOWN"
}

// splitStatements splits SQL text on semicolons, respecting quoted strings
// and backtick-quoted identifiers (used by MySQL/StarRocks for identifiers).
func splitStatements(query string) []string {
	var statements []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false
	inBacktick := false
	escaped := false

	for i := 0; i < len(query); i++ {
		c := query[i]

		if escaped {
			current.WriteByte(c)
			escaped = false
			continue
		}

		if c == '\\' {
			current.WriteByte(c)
			escaped = true
			continue
		}

		if c == '\'' && !inDoubleQuote && !inBacktick {
			inSingleQuote = !inSingleQuote
			current.WriteByte(c)
			continue
		}

		if c == '"' && !inSingleQuote && !inBacktick {
			inDoubleQuote = !inDoubleQuote
			current.WriteByte(c)
			continue
		}

		if c == '`' && !inSingleQuote && !inDoubleQuote {
			inBacktick = !inBacktick
			current.WriteByte(c)
			continue
		}

		if c == ';' && !inSingleQuote && !inDoubleQuote && !inBacktick {
			if stmt := strings.TrimSpace(current.String()); stmt != "" {
				statements = append(statements, stmt)
			}
			current.Reset()
			continue
		}

		current.WriteByte(c)
	}

	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		statements = append(statements, stmt)
	}

	return statements
}

// extractStatementOperation identifies the SQL operation type from a single statement.
// Handles StarRocks-specific operations like SUBMIT TASK, INSERT OVERWRITE, etc.
// Tolerant of whitespace variations (tabs, multiple spaces) between keywords,
// and inline optimizer hints like SUBMIT /*+set_var(...)*/ TASK.
func extractStatementOperation(stmt string) string {
	s := stripLeadingComments(stmt)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Strip all block comments (/* ... */) so that optimizer hints like
	// SUBMIT /*+set_var(query_timeout=300)*/ TASK are parsed correctly.
	s = stripAllBlockComments(s)

	// Normalize the prefix for multi-word matching: collapse runs of whitespace
	// into single spaces. We only need the first ~4 words for operation detection,
	// so we normalize a limited prefix to avoid touching the full query body.
	normalizedPrefix := normalizeWhitespacePrefix(strings.ToUpper(s), 4)

	// Multi-word operations, most specific first
	multiWordOps := []struct {
		prefix string
		name   string
	}{
		// StarRocks-specific
		{"SUBMIT TASK", "SUBMIT_TASK"},
		{"INSERT OVERWRITE", "INSERT_OVERWRITE"},
		{"REFRESH MATERIALIZED VIEW", "REFRESH_MATERIALIZED_VIEW"},
		{"CREATE MATERIALIZED VIEW", "CREATE_MATERIALIZED_VIEW"},
		{"DROP MATERIALIZED VIEW", "DROP_MATERIALIZED_VIEW"},
		{"ALTER MATERIALIZED VIEW", "ALTER_MATERIALIZED_VIEW"},
		{"CANCEL LOAD", "CANCEL_LOAD"},
		{"SHOW LOAD", "SHOW_LOAD"},
		{"BROKER LOAD", "BROKER_LOAD"},
		{"ROUTINE LOAD", "ROUTINE_LOAD"},
		{"ADMIN SET", "ADMIN_SET"},
		{"ADMIN SHOW", "ADMIN_SHOW"},
		{"ADMIN CHECK", "ADMIN_CHECK"},
		{"ADMIN REPAIR", "ADMIN_REPAIR"},
		{"SET CATALOG", "SET_CATALOG"},
		// Standard SQL multi-word
		{"INSERT INTO", "INSERT"},
		{"CREATE TABLE", "CREATE_TABLE"},
		{"CREATE EXTERNAL TABLE", "CREATE_TABLE"},
		{"CREATE VIEW", "CREATE_VIEW"},
		{"CREATE DATABASE", "CREATE_DATABASE"},
		{"CREATE INDEX", "CREATE_INDEX"},
		{"ALTER TABLE", "ALTER_TABLE"},
		{"ALTER DATABASE", "ALTER_DATABASE"},
		{"DROP TABLE", "DROP_TABLE"},
		{"DROP VIEW", "DROP_VIEW"},
		{"DROP DATABASE", "DROP_DATABASE"},
		{"DROP INDEX", "DROP_INDEX"},
		{"TRUNCATE TABLE", "TRUNCATE"},
		{"ANALYZE TABLE", "ANALYZE"},
		{"SHOW TABLES", "SHOW"},
		{"SHOW DATABASES", "SHOW"},
		{"SHOW CREATE", "SHOW"},
		{"SHOW COLUMNS", "SHOW"},
		{"SHOW PROCESSLIST", "SHOW"},
	}

	for _, op := range multiWordOps {
		if len(normalizedPrefix) >= len(op.prefix) && strings.HasPrefix(normalizedPrefix, op.prefix) {
			if len(normalizedPrefix) == len(op.prefix) || normalizedPrefix[len(op.prefix)] == ' ' || normalizedPrefix[len(op.prefix)] == '(' {
				return op.name
			}
		}
	}

	// Single-word fallback
	firstWord := normalizedPrefix
	if idx := strings.IndexAny(normalizedPrefix, " ("); idx > 0 {
		firstWord = normalizedPrefix[:idx]
	}

	return firstWord
}

// normalizeWhitespacePrefix extracts the first N whitespace-delimited words from s,
// returning them joined by single spaces. This allows multi-word operation matching
// to be tolerant of tabs, multiple spaces, and other whitespace between keywords.
func normalizeWhitespacePrefix(s string, maxWords int) string {
	var words []string
	i := 0
	for len(words) < maxWords && i < len(s) {
		// Skip whitespace
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) {
			break
		}
		// Non-alphabetic token (e.g., open paren) ends prefix extraction
		if s[i] == '(' {
			break
		}
		// Read word (stop at whitespace or open paren)
		start := i
		for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' && s[i] != '(' {
			i++
		}
		if i > start {
			words = append(words, s[start:i])
		}
	}
	// Append the rest of the string so single-word fallback and boundary checks work
	result := strings.Join(words, " ")
	if i < len(s) {
		result += s[i:]
	}
	return result
}

// stripAllBlockComments removes all /* ... */ block comments from a string.
// This is used before operation detection so that inline optimizer hints like
// SUBMIT /*+set_var(query_timeout=300)*/ TASK don't break keyword matching.
func stripAllBlockComments(s string) string {
	for {
		start := strings.Index(s, "/*")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start+2:], "*/")
		if end < 0 {
			return s
		}
		s = s[:start] + " " + s[start+2+end+2:]
	}
}

// stripLeadingComments removes SQL comments (-- and /* */) from the beginning of a statement.
func stripLeadingComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}

		if strings.HasPrefix(s, "--") {
			if idx := strings.IndexByte(s, '\n'); idx >= 0 {
				s = s[idx+1:]
				continue
			}
			return ""
		}

		if strings.HasPrefix(s, "/*") {
			if idx := strings.Index(s, "*/"); idx >= 0 {
				s = s[idx+2:]
				continue
			}
			return ""
		}

		return s
	}
}

// String returns a human-readable description for logging.
func (f *QueryFilter) String() string {
	parts := []string{fmt.Sprintf("mode=%s", f.mode)}

	if len(f.sqlOperations) > 0 {
		ops := make([]string, 0, len(f.sqlOperations))
		for op := range f.sqlOperations {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		parts = append(parts, fmt.Sprintf("sql_operations=[%s]", strings.Join(ops, ",")))
	}

	if len(f.patterns) > 0 {
		pats := make([]string, 0, len(f.patterns))
		for _, p := range f.patterns {
			pats = append(pats, p.String())
		}
		parts = append(parts, fmt.Sprintf("patterns=[%s]", strings.Join(pats, ",")))
	}

	if f.sampleRate < 1.0 {
		parts = append(parts, fmt.Sprintf("sample_rate=%.4f", f.sampleRate))
	}

	return strings.Join(parts, ", ")
}
