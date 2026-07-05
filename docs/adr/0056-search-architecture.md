---
id: "0056"
title: Cross-entity BM25-shaped + hybrid-vector search with visibility floor
status: accepted
date: 2026-07-02
area: architecture
phases:
  - "1.16.B"
supersedes: []
related:
  - "0043"
  - "0049"
  - "0052"
  - "0053"
  - "0055"
tags:
  - search
  - ranking
  - vector
  - facets
  - saved-searches
  - visibility
excerpt: >-
  Locks the architecture of the 1.16.B search arc: unified /search endpoint over Postgres tsvector with field weighting, DSL parser with strict whitelist, cross-package visibility.Filter, pgvector hybrid ranking, LISTEN/NOTIFY cache invalidation, saved-searches with delta detection, and admin reindex + observability surface. Ships end-to-end via PRs #174 → #182.
---

## Status (2026-07-02)

**Accepted.** Search arc 1.16.B-1 through 1.16.B-5 shipped end-to-end via PRs #174, #176, #178, #180, #182 (dev head `b393eff2`). Issue #168 closed. This ADR captures the load-bearing architectural decisions the arc locked so future arcs (feedback loop, saved-search team-sharing, cross-instance search, ranking-engine swap per ADR 0055) build against a documented foundation instead of reverse-engineering the code.

## Context

Before the 1.16.B arc, `/search` was a `coming_soon` stub; per-entity list endpoints supported `?q=` free-text but had no unified surface, no cross-entity ranking, no result count, no facets, no vector-search integration, no saved searches, no admin observability. The RS-gap audit 2026-06-22 flagged this as the biggest workflow gap after basic browse — operators need discovery that composes across facets, natural language, similarity, and personal-watchlist workflows. Phase 1.14.B shipped CLIP image embeddings + `pgvector`, but those were largely unread through a bespoke helper. Phase 1.19.A-1 shipped the email substrate + template system that saved-search notifications could consume.

Five sub-phases (B-1 through B-5) sequenced foundation → advanced query surface → vector layer → workflow layer → operator polish + arc-close deferrals. Each sub-phase's brief locked its design; this ADR consolidates the cross-phase architectural pattern so the arc's shape survives the code.

## Decision

The 1.16.B search architecture is:

### 1. Unified `/search` endpoint over Postgres tsvector — foundation (B-1)

- `GET /search` returns typed union results across `asset`, `collection`, `post`. Cursor pagination via opaque base64 payload `{last_score, last_id, last_type}`. Total count exact ≤10k, `"total_count_capped": true` beyond.
- Ranking via `ts_rank_cd` with per-entity field weighting: assets weighted `A=title / B=description / C=tags / D=custom-field-values`; collections weighted `A=name / B=description / C=tags`; posts weighted `A=title / B=body / C=tags`. Weight expressions live in migration 00022 (retrofit from B-1's initial unweighted ship).
- Cross-entity score normalisation: raw per-entity `ts_rank_cd` values divided by per-query per-entity max score → `[0, 1]` — cross-entity ordering apples-to-apples.
- `plainto_tsquery` for B-1 free-text; `to_tsquery` reached ONLY through the DSL compiler (see below). Never passes user input to `to_tsquery` unfiltered.
- GIN indexes on tsvector columns; trigger/generated columns match assets → collections → posts per pre-audit findings.
- **Real BM25 (per-doc IDF) deferred**; the ranking function is a single-package swap per B-1's locked abstraction. See ADR 0055 for the pg_search / paradedb research-record.

### 2. Advanced DSL parser — strict whitelist (B-2)

- User-facing syntax: `field:value`, `"exact phrase"`, `AND / OR / NOT`, parenthesised grouping, free-text default → `plainto_tsquery`.
- Field whitelist enforced at parse time: `title / description / body / tag / owner / type / sensitivity / extension / has_field:<field_id>`. Unknown field → 400 with valid-fields in error body.
- Parser produces a well-typed AST → compiler renders → `to_tsquery`-safe SQL + typed `Filters` struct. User text reaches SQL only through `plainto_tsquery` sub-expressions. **This is the injection floor.**
- `similar_to:<uuid>` node parsed by B-2, reserved with `DSLError{SimilarToNotImplemented}`; B-3 replaces with real compilation.

### 3. Faceted aggregation — parallel goroutines, visibility floor (B-2)

- One aggregator per facet type: `asset_type / file_extension / tag / sensitivity / owner / team / date_range / custom_field`.
- Aggregators run in parallel via `errgroup` capped at 8 concurrent (matches seeded facet count). Per-aggregator 500ms timeout (sysconfig `search.facet_aggregator_timeout_ms`). Slow facet → empty bucket + warn log; other facets still return.
- **Visibility floor: every facet query passes through `visibility.Filter(EntityType)` BEFORE `GROUP BY`.** A restricted asset with tag `unique_marker_restricted` MUST NOT contribute to `unique_marker_restricted` bucket count for any caller who can't see the asset. This is the highest-severity failure mode of the arc.

### 4. Shared `visibility` package — load-bearing floor (B-2)

- New `app/internal/visibility/` package with `Filter(ctx, EntityType) → Predicate → Predicate.ToSQL(alias)` interface.
- Predicate carries caller's effective visibility set (own / team / public / federated-remote-visible / restricted-via-share). Renders to a `WHERE` fragment + bound params.
- Consumed by: search Engine, facet aggregators, suggestion query, saved-search notifier at owner-actor context, IIIF manifest generator (future). **Not yet consumed by list handlers** — deferred per follow-up issue #185.
- Snapshot-test-driven when list-handler retrofit ships: pre-refactor list-handler behaviour captured; post-refactor MUST match byte-for-byte.

### 5. Autocomplete via `pg_trgm` (B-2)

- Extension `pg_trgm` added in migration 00022.
- Suggestion corpus: tag names (currently applied) + collection names + post titles + asset titles + owner display names (public only), all filtered through `visibility.Filter`.
- `similarity(prefix, candidate) > threshold` (default 0.3; sysconfig `search.suggest_similarity_threshold`); order by similarity DESC; LIMIT 10.
- Rate-limited 120 req/min per user (chatty typeahead).

### 6. Vector search — hybrid ranking (B-3)

- `similar_to:<uuid>` compiles by fetching asset embedding from `asset_embedding_d768` (existing 1.14.B table); populates `Query.SimilarityHint` + `Query.SimilarityHintID = "asset:<uuid>"`.
- `POST /search/by-image` reserved 501 in B-3 pending CLIP visual encoder sidecar. **Activated 2026-07-05 via PR #199 (1.16.B-3-followup)** with a load-bearing constraint: **two embedding spaces, two tables, zero cross-comparison.** The existing text-derived `asset_embedding_d768` (via Ollama nomic-embed-text, misleadingly named `clip_local` — a reserved-name artefact from before the visual encoder shipped) and the new `asset_visual_embedding` (via OpenCLIP ViT-L/14 in the `aa-clip-visual-local` sidecar) hold vectors from different embedding spaces. Cosine similarity between them is meaningless — physical table separation makes accidental cross-space queries impossible. `similar_to:<uuid>` semantics unchanged (continues to use text-derived embeddings). `POST /search/by-image` uses `Query.SimilarityHintID = "image:<sha256>"` and queries the visual table exclusively. Cross-modal (image query → text-descriptor asset match) would require CLIP text encoding + a full Engine surface rewrite; deliberately out-of-scope for v1.
- Hybrid ranking: `hybrid_score = (1 - w) * bm25_normalised + w * cosine_similarity`. Weight sysconfig-tunable (`search.hybrid_bm25_weight`; default 0.5).
- Result set is UNION — an asset with high BM25 but no vector similarity still ranks; an asset with high vector similarity but no BM25 match still ranks. Missing dimensions score 0.
- pgvector cosine distance flipped to similarity: `1 - (embedding <=> hint)`. Similarity threshold applied per-query (sysconfig `search.vector_similarity_threshold`; default 0.3).
- Over-fetch multiplier for pgvector hits (`search.vector_overfetch_multiplier`; default 5) before merge with BM25 hits → cursor pagination.
- **Visibility floor extends to vector queries.** Every pgvector similarity query joins `visibility_predicate_subquery` before ranking. Federated inbox writes trigger local embedding compute via same path.

### 7. Saved searches — delta detection (B-4)

- `saved_search` table (migration 00023) stores DSL string + `types_filter` + `facets_filter` + hybrid tuple + `email_frequency ∈ {off, immediate, hourly, daily, weekly}` + `last_result_hash` + `last_result_ids UUID[]` + `last_check_at` + `last_notified_at` + `last_error`.
- Delta via hash-of-sorted-ID-set + linear-merge diff — deterministic, replayable.
- Coordinator job self-re-enqueues via `ScheduledFor`; per-frequency batching; per-user coalescing (one digest email per user per digest window regardless of saved-search count).
- **Visibility at execution time**: notify job runs Engine.Query with `context.WithValue(ctx, ActorUserRef, owner_user_ref)` so `visibility.Filter` returns the OWNER's current predicate. Access lost between save + notify = hits silently absent from email.
- Email substrate reuse: template `notification_saved_search_digest.{subject,txt,html}.tmpl` registered via `templateForVerb` auto-resolution against 1.19.A-1's `email.RegisterTemplate` (agent-side improvement over the brief's direct-register call).
- Idempotency-keyed `notification.email` enqueue prevents duplicate sends on job retry.
- Query DSL string storage (NOT compiled query): saved-search survives query-engine evolution — recompilation happens at each notify run. DSL parse error at runtime → `last_error`, no email, admin failure queue.

### 8. LISTEN/NOTIFY cache invalidation broadcast (B-1 + through)

- `QueryResultCache` + `FacetCountCache` + `SuggestionCache` + `SavedSearchCountCache` + `SavedSearchFailureCountCache` + `DiskUsageCache` (B-5) — all registered via `cache.Registry`.
- Cross-package invalidators exported: `search.InvalidateOnAssetWrite / OnCollectionWrite / OnPostWrite / OnTagChange / OnFieldValueWrite / OnUserWrite`. Called from each domain's write handler after commit.
- Postgres LISTEN/NOTIFY broadcast on channel `search_cache_invalidate` with payload `{scope: "all"|"query"|"facet"|"suggestion"|"vector"}` — coarse invalidation strategy; matches TTL cadence; federation-ready (peer writes broadcast to their own instance's cache only, not cross-peer).
- Cache-key floors: `user_id` in every cache key (user A's cached result NEVER served to user B); `SimilarityHintID` in vector cache-key (avoids cross-query pollution).

### 9. Admin observability + reindex tooling — arc close (B-5)

- Reindex controls: scope picker (`all` / `asset_type:<t>` / `collection:<id>` / `field:<f>` / `embedding_model:<m>`) + target (`tsvector` / `embedding` / `both`); one active run at a time; cancellable between batches; history via `search_reindex_run` table (migration 00024).
- Disk-usage view: `tsvector_bytes` per entity + `embedding_table_bytes` + `embedding_index_bytes` + `cache_footprint` + `saved_search_rows`; cached 30s.
- pg_stat gauges: `assets_pending_embedding`, `asset_embedding_row_count`, `asset_embedding_index_size_mb`, `saved_search_active_gauge{frequency}`.
- Federation-inbox embed hook (from B-3 deferral) — federated inbox writes trigger local embed job enqueue via same helper the HTTP path uses. Hook lives OUTSIDE `app/internal/federation/` — federation soak preserved.
- `/admin/search/dashboard` visualises `/admin/search/health` JSON grouped by subsystem (engine / facets / suggestions / vector / saved-searches / reindex / cache).
- Admin `/admin/saved-searches` + `/admin/saved-searches/failures` for cross-user management.

### 10. Raw chi routes over strict-server shims (B-1 through B-5)

- All new search endpoints (`/search`, `/search/facets`, `/search/suggest`, `/search/advanced`, `/search/by-image`, `/search/save-as-collection`, `/admin/search/*`, `/saved-searches/*`) mount as raw chi routes; OpenAPI schemas exist for frontend types but strict-server shim generation skipped.
- Rationale: cross-arc consistency + reduced shim maintenance burden. Documented in each PR body.

### 11. Federation posture — locally consistent, cross-instance out (B-1 through B-5)

- Search queries and their aggregations are local-only; each peer indexes its own corpus.
- Federated entities that arrived via inbox appear in local search when locally visible per `visibility.Filter`.
- Saved searches are per-instance (never federate).
- LISTEN/NOTIFY broadcast is per-instance (each peer's cache is independent).
- Cross-instance search declared OUT of v1 per gap-audit + arc plan. Reassess when operator demand emerges.

### What this is NOT

- **Not a full-text-search-only surface** — hybrid vector integration is architectural, not optional
- **Not a replacement for browse feed** — `/search` requires a query; browse feed serves discovery without one
- **Not a percolator surface** — saved-search delta detection is periodic re-execution, not real-time entity-write matching (though `immediate` frequency approximates it via LISTEN/NOTIFY triggers)
- **Not an ML-augmented ranking layer** — no learned-to-rank, no cross-encoder reranking, no query-understanding LLM
- **Not extensible via plugins** — search is core AA; extensibility lives in the AI provider abstraction + capability add-ons per ADR 0034
- **Not federation-crossing** — cross-instance queries out of v1

## Consequences

**Positive:**

- Single query engine for text + vector + facets + saved-searches → one visibility floor, one cache, one observability surface
- LISTEN/NOTIFY broadcast makes cache invalidation federation-ready without touching federation runtime
- `visibility.Filter` extraction closes the leak-vector risk that per-endpoint inline filters carry (partially — list-handler retrofit still pending in #185)
- Ranking-function swap remains a single-package change → future BM25 / other-engine adoption doesn't require pipeline rewrite
- Cursor pagination + score normalisation composes with future ML-augmented ranking
- Saved-search delta detection is deterministic + replayable via `last_result_hash` — auditable + testable

**Negative:**

- Two coexisting extension deps (`pg_trgm` since B-2 + `pgvector` from 1.14.B) increase operator setup surface
- Coarse cache invalidation (write any entity → clear whole cache slice) trades hit rate for simplicity; may need fine-grained invalidation at higher write volumes
- App-layer BM25+vector merge is simpler than in-SQL RRF (à la ParadeDB) but ties merge policy to Go code — swap requires code change, not config
- Saved-search top-100 tracking window bounds delta detection precision; hits ranking below top-100 that become new won't trigger notifications
- Raw chi route pattern skips strict-server shim benefits (typed request/response validation); frontend still gets types via OpenAPI schemas but backend edge cases surface at runtime

## Alternatives considered

- **Elasticsearch / OpenSearch** — rejected. Separate operational surface (JVM, cluster health), separate index-drift bug class, doesn't help federation, no measurable relevance benefit at AA's corpus size. See ADR 0055 research for full analysis of a related question.
- **pg_search / ParadeDB (BM25 extension)** — deferred. See ADR 0055 for the record-only research snapshot and seven revisit triggers.
- **Dual-engine support (tsvector + pg_search behind an interface)** — rejected. Maintenance tax + feature-parity temptation + support-conversation complexity. Weighed in 2026-07-02 planning discussion; conclusion recorded in ADR 0055.
- **In-SQL RRF for hybrid ranking (ParadeDB pattern)** — rejected in favour of app-layer merge. Reason: swap flexibility + no extension dependency + cleaner test surface. RRF remains a viable follow-up when a specific ranking-quality issue justifies it.
- **Per-endpoint search stubs (`/api/assets?q=`, `/api/collections?q=`)** — kept for backwards compat until v1.0.0 tag; `/search` unified surface is additive. Deprecation happens at v1.0 per ADR 0046 timing.
- **Server-side saved-search history** — deferred. B-1 shipped client-side localStorage history; server-side cross-device history is a future upgrade tied to notification preferences UX.

## Reference

- Phase 1.16.B-1 through 1.16.B-5 — sub-phase briefs shipped via PRs #174, #176, #178, #180, #182
- ADR 0043 — Federation walled-garden protocol (cross-instance search out of v1)
- ADR 0049 — Encrypted federation + dogfood (federation soak: search infra outside federation runtime tree)
- ADR 0052 — Optimistic-concurrency edit-safety (`updated_at` powers cache-key versioning + saved-search re-run freshness)
- ADR 0053 — IIIF interoperability (Content Search 2.0 unblocks when this arc closes; see 1.54.B / issue #170)
- ADR 0055 — pg_search / ParadeDB research-record (future ranking-engine option; not committed)
- Phase 1.14.B — CLIP embeddings + pgvector foundation
- Phase 1.19.A-1 — Email substrate (saved-search notifications ride this)
- RS-gap audit 2026-06-22 — original P0 findings for faceted search + saved searches
- Follow-up issues: #183 (CLIP visual encoder), #184 (feedback loop), #185 (visibility retrofit), #186 (AdminJobBackfillPage)
