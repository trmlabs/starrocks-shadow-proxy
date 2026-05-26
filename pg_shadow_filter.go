// pg_shadow_filter.go — Shadow-mirror admission check for the pgwire path.
//
// Mirrors the MySQL proxy's filter-then-Send pattern (proxy.go runQueryLoop)
// with one critical difference: sampling is applied ONCE per client connection
// in startShadowWorker, not per frame. Per-frame sampling let a Parse get
// dropped while its companion Bind/Execute (carrying empty QueryText) sailed
// through the trivial filter and shipped to the shadow, breaking the session
// with "prepared statement S_N does not exist". SQL operation + pattern
// filters stay per-frame (they're deterministic), and the runQueryLoop tracks
// any Parse frames it filtered out so subsequent Bind/Execute/Describe/Close
// on the same statement name are also dropped.
//
// Filtering only applies to SQL-carrying frames (Query, Parse). Other frames
// pass through unless their stmt name was previously filtered.
package main

// shouldShadowMirror reports whether a pgwire frame should be enqueued to the
// shadow worker. Returns (false, reason) when filtered; reason is one of the
// FilterReason* constants for shadow_proxy_shadow_filtered_total labeling.
// A nil filter mirrors everything.
//
// IMPORTANT: this is the deterministic check (no sampling). The pgwire path
// rolls sampling once per connection in startShadowWorker. Callers must NOT
// expect this function to drop frames for FilterReasonSampling.
func shouldShadowMirror(req QueryRequest, filter *QueryFilter) (bool, string) {
	if filter == nil {
		return true, FilterReasonNone
	}
	return filter.AllowDeterministic(req)
}
