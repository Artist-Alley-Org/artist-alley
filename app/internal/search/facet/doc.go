// Package facet implements the parallel facet aggregators for the
// unified /search/facets endpoint. Phase 1.16.B-2.
//
// Each facet type has its own [Aggregator] implementation. The
// dispatcher runs aggregators in parallel via goroutines, with a
// per-request cap = 8 (one per seeded aggregator) so goroutine
// count is bounded. Each aggregator applies visibility.Filter
// BEFORE its GROUP BY — the visibility floor for facet counts.
//
// Aggregator timeout is per-request (default 500ms; configurable
// via search.facet_aggregator_timeout_ms sysconfig key). Timeout
// yields an empty bucket for that facet + a warn log; other facets
// still return. Never fail the whole facet response for one slow
// aggregator.
//
// Seeded aggregators in B-2:
//
//   - asset_type (bigint from assets.asset_type)
//   - tag (from post_tags for the post entity)
//   - sensitivity (from assets.sensitivity enum)
//   - owner (from assets.owner_user_ref → user.username lookup)
//   - extension (from assets.file_extension)
//
// Deliberately deferred to a later revision:
//
//   - date_range (needs UX design for bucket boundaries)
//   - team (federation-team is its own arc)
//   - custom_field (needs per-field-def GROUP BY dispatch)
//
// The dispatcher's shape (map[FacetType]Aggregator) makes adding
// the deferred ones a one-file edit + registration.
package facet
