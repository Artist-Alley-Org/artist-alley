// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package search implements the unified GET /search endpoint that
// spans assets, collections, and posts.
//
// Phase 1.16.B-1 ships the foundation:
//
//   - Cross-entity assembly: one query per entity → per-entity max
//     score → per-hit normalisation to [0,1] → merged + ordered
//     by (normalised_score, id, type)
//   - Cursor pagination on the ordered tuple; opaque base64-JSON
//   - COUNT-with-limit-10001 for exact-up-to-10k / capped-beyond
//   - QueryResultCache via cache.Registry: 60s TTL, LRU eviction,
//     cross-instance broadcast via the existing "cache_invalidate"
//     LISTEN/NOTIFY channel. Cache-key includes user_ref so User
//     A's cached result NEVER serves to User B.
//   - Coarse invalidation: any write to a searchable entity purges
//     the entire query-result cache. Matches the 60s TTL cadence
//     and avoids per-query dependency tracking.
//
// Ranking uses Postgres native ts_rank_cd against the tsvector
// columns (assets.search_text + posts.search_text from the 00001
// baseline; collections.search_text added in 00021). No new
// Postgres extensions required.
//
// Ranking is UNWEIGHTED in this phase — see the 00021 migration
// header for the divergence note. ts_rank_cd's default weight
// vector still delivers meaningful cover-density ranking without
// setweight. Field-weighted retrofit is a clean seam left for a
// later sub-phase.
//
// Visibility: every read path composes the SINGLE shared predicate,
// visibility.Filter (ADR 0063). This comment used to describe a
// pre-audit world where no shared helper existed and each query
// replicated a per-entity gate; that helper now exists and every
// entity routes through it, so the per-entity descriptions below are
// kept only as a map of what the predicate resolves to:
//
//   - assets, anonymous: not-deleted + status='active' +
//     sensitivity='public' + processing_status='ready'. (The earlier
//     "no visibility gate" note was wrong on both the anonymous and
//     authenticated halves — #210.)
//   - assets, authenticated: not-deleted only. The sensitivity rule is
//     no longer deferred here (#899) — it just does not act on the ROW.
//     ADR 0064 keeps a restricted asset in the result set, so the
//     predicate still returns it; what changed is that the PROJECTION
//     now runs visibility.FieldsReadable per row and a hit the caller
//     cannot open carries no title, no summary and no thumbhash. Same
//     rule on /search/suggest (which drops the row outright — a
//     completion is the title) and on the asset facets (which count
//     only rows the caller could open).
//   - collections: public OR owner OR a live ACL grant.
//   - posts: the full post read rule — authored by the caller, OR
//     public/org-only, OR private with posts.admin, OR followers where
//     the caller follows the author, OR a live post_acls grant. This
//     line used to read "public OR authored by caller", and it was
//     accurate: the browse feed composed the rich rule from an
//     unexported copy in the posts package, and these three surfaces
//     composed a coarser second one, so an org-only post you could open
//     from your feed did not exist in search — no error, no empty state,
//     just absence, with the tag facet and the completions wrong the
//     same way (#873). The rule now lives in visibility (post_rule.go)
//     and both sides splice it. Caps: the `private` disjunct needs the
//     caller's posts.admin, so Query/Request carry a resolved
//     visibility.PostCaps and the search cache key folds it in.
//
// by_image.go's anonymous branch was the last hand-rolled copy of the
// asset floor and now delegates to the predicate too (#210). Any
// remaining inline visibility SQL in this package is a bug.
//
// Federation: search runs against the local corpus. Federated
// entities (origin_server_id NOT NULL) appear in results iff they
// pass the same visibility gate as any local row. No cross-instance
// query federation in v1 — each peer searches its own corpus.
// Federated writes (inbox handlers) trigger cache invalidation
// through the same write commit paths as native writes, so the
// LISTEN/NOTIFY broadcast reaches every peer without extra wiring.
//
// Advanced DSL (field:value, phrases, AND/OR/NOT, parens) is out
// of scope for B-1 — the parser is plainto_tsquery verbatim. NEVER
// pass user input to to_tsquery: that's the injection floor. The
// advanced DSL lands in B-2 with a strict whitelist tokeniser.
package search
