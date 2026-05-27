// pg_shadow_filter.go — deterministic admission check for the pgwire path.
// Sampling lives in startShadowWorker (per-connection); sticky-stmt tracking
// lives in PgProxy.shouldMirrorPgFrame. See those for the full filter policy.
package main

// shouldShadowMirror returns (false, FilterReason*) when a pgwire frame
// should be dropped. Deterministic only — does not roll sampling.
func shouldShadowMirror(req QueryRequest, filter *QueryFilter) (bool, string) {
	if filter == nil {
		return true, FilterReasonNone
	}
	return filter.AllowDeterministic(req)
}
