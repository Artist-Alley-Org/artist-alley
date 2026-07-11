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
// Visibility: matches the existing per-entity list handlers. No
// shared VisibleFor helper exists in the codebase today (pre-audit
// Q2); rather than build one from scratch inside a foundation PR,
// the search queries replicate each entity's existing "handler
// composes visibility" pattern:
//
//   - assets: no visibility gate (matches ListAssetsPage)
//   - collections: owner OR explicit-share ACL grant
//   - posts: public visibility OR authored by caller
//
// This is a documented divergence from the brief's "same helper
// unmodified" requirement — the helper simply doesn't exist today.
// The search package's visibility replication is the load-bearing
// unit under test; any future shared VisibleFor helper is a
// straight substitution.
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
