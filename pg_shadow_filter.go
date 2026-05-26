// pg_shadow_filter.go — Shadow-mirror admission check for the pgwire path.
//
// Mirrors the MySQL proxy's filter-then-Send pattern (proxy.go runQueryLoop):
// before enqueuing a frame to the PgShadowWorker, callers consult
// shouldShadowMirror to honor SHADOW_FILTER_MODE / SHADOW_FILTER_SQL_OPERATIONS
// / SHADOW_FILTER_PATTERNS / SHADOW_SAMPLE_RATE.
//
// Filtering only applies to SQL-carrying frames (Query, Parse). Other frames
// (Bind, Execute, Sync, etc.) always pass through to keep the shadow's
// prepared-statement state in sync with the primary.
package main

// shouldShadowMirror reports whether a pgwire frame should be enqueued to the
// shadow worker. Returns (false, reason) when filtered; reason is one of the
// FilterReason* constants for shadow_proxy_shadow_filtered_total labeling.
// A nil filter mirrors everything.
func shouldShadowMirror(req QueryRequest, filter *QueryFilter) (bool, string) {
	if filter == nil {
		return true, FilterReasonNone
	}
	return filter.Allow(req)
}
