// Package feedback implements the operator-facing search-result
// feedback loop (Phase 1.16.B-5-followup, closes #184).
//
// Users submit thumbs up/down on individual result hits from the
// /search page; admins get two anonymized aggregation views (top
// down-voted queries + under-ranked hits) plus a per-user audit
// page for abuse review.
//
// Composition with the rest of the search subsystem:
//
//   - Never invalidates the query result cache. Feedback events are
//     out-of-band ranking-quality signal; results stay stable for
//     the 60s cache TTL regardless of feedback activity.
//   - Reuses the B-2 visibility.Filter package via a caller-provided
//     Visibility seam so the Submit endpoint refuses feedback on
//     hits the caller can't see (prevents feedback-based existence
//     probing).
//   - Increments the shared search.Counter's Result classes for
//     feedback events so the /admin/search/health JSON surfaces
//     feedback traffic alongside query traffic.
//   - Uses auth.IPSubnetHash for threat-class correlation without
//     a per-IP audit log (mirrors 1.19.D lockout's pattern).
//
// Divergences from the brief that shipped with this PR:
//
//   - No pre-existing "coming soon" dashboard tile — this PR adds
//     the tile from scratch. Pre-audit Q1 caught the brief's stale
//     assumption; the brief was written as if B-5 had already
//     stubbed the surface, which it hadn't.
//   - No pre-existing search_feedback_events_total counter stub —
//     this PR adds the Result classes from scratch (same reason).
//   - No shared ResultCard component — thumb buttons mount inline
//     on /search/+page.svelte at the single result-render site.
//   - No pre-existing DSL canonicalization — this PR ships a minimal
//     one (trim + lowercase + collapse whitespace) suitable for
//     aggregation-key hashing; full AST canonicalization deferred.
//
// Never federates. Feedback rows are per-instance; no origin_server_id
// column, no outbox event.
package feedback
