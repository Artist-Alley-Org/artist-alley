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

## Status (updated 2026-07-06)

**Accepted.** Search arc 1.16.B-1 through 1.16.B-5 shipped end-to-end via PRs #174, #176, #178, #180, #182 (dev head `b393eff2`). Issue #168 closed. Followups then extended the arc: reverse-image search coverage (1.16.B-3-followup via PR #199 + 1.16.B-3-followup-4 via PR #205 + 1.16.B-3-followup-2 via PR #206) and the search feedback loop (1.16.B-5-followup via PR #208 on 2026-07-06, closing #184). **Arc fully closed — all 5 sub-phases plus 3 followups shipped.** This ADR captures the load-bearing architectural decisions the arc locked so future arcs (saved-search team-sharing, cross-instance search, ranking-engine swap per ADR 0055, learned-ranking layer consuming feedback signal) build against a documented foundation instead of reverse-engineering the code.

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

### 3b. Facets FILTER, not merely count — amendment 2026-08-12 (#907, PR #1055)

As accepted, this ADR specified the aggregators and the `Filters` struct but never joined them:
the DSL compiled `tag:foo` into `Filters` and **the Engine ignored it** (`Query.Advanced` was a
placeholder documented as ignored; `http.go` said the compiled query was "informational"). Every
facet bucket therefore showed a true count and did nothing, for two releases. `dsl/doc.go`
meanwhile *claimed* the Engine applied Filters "via ordinary WHERE clauses" — a comment asserting
a structural guarantee that was false, which is how it survived.

What now exists:

1. **One typed predicate set — `facet.Selection` — rendered by BOTH the aggregators and the
   Engine.** This is the load-bearing property, not the checkbox. A bucket's count and the filter
   that bucket labels are the same value applied to the same population, so **they cannot drift by
   being written twice.** Any future dimension must be added to the shared renderer, never to one
   side.
2. **Wire shape: repeated `filter=<dimension>:<value>`**, on `/search` and `/search/facets` alike —
   the facet endpoint takes the selection too, because a count computed without the active filter
   is the same defect one level up. Unknown dimensions are a `400`, never a silent drop.
   Rejected: `dsl=` (it replaces the free-text query and would force callers to hand-quote values)
   and per-dimension parameters (which make each new dimension a new parameter, handler and piece
   of frontend state).
3. **Extensibility is the acceptance test for the shape.** #910 (search inside a collection) must
   be **one `FacetType` const plus one case in `facet.dimensionSQL`**. Prior art drove this: a
   mature photo DAM exposes container membership as an ordinary filter predicate (with the
   negation free), while a mature media DAM spends a bespoke `!collection<id>` parser to reach the
   same place. **We take the predicate. No `!bang` special syntax** — this project already has a
   typed `field:value` grammar with a parse-time whitelist, and a second vocabulary would be a
   second code path to keep honest.

   ✅ *Measured 2026-08-13 (#910, PR #1058) — **the prediction was close but not exact, and the
   difference is worth more than the prediction was.*** Actual cost: **5 files beyond the const
   and the case, 3 of them backend.** The extensibility claim holds — no second query path, no
   bespoke parameter, no change to the wire vocabulary — so `filter=` stands as the single query
   representation, and #911 and advanced search may be planned on it.

   **Both misses were properties of the VALUE'S TYPE, not of the mechanism**, and they generalise:

   1. **A typed value needs validating at the edge.** The five original dimensions render TEXT
      comparisons and are total over any string. A UUID reaches a `::UUID` cast, so a malformed
      one is a Postgres `22P02` raised mid-query — **a 500 on a caller's typo.** Any dimension
      whose value is not free text owes a parse step before it reaches SQL.
   2. **A value that names another ENTITY needs authorizing, and `dimensionSQL` cannot do it.**
      That renderer is caller-blind and emits exactly one placeholder per term; there is nowhere
      to put the caller's identity without changing the arity for *every* dimension. An
      entity-naming dimension therefore needs a gate at the execution chokepoints — see the
      ADR 0009 §3 amendment for the parent gate this produced.

   ⭐ **The rule for the next dimension:** ask what the value *is* before estimating. A property
   of the row (`extension`, `tag`, `sensitivity`) really is one const and one case. A value that
   is *typed* costs validation; a value that *names something carrying its own access control*
   costs a gate. Neither costs a new code path — which is the part of the original claim that
   mattered.

   Amended rather than left standing, because an unamended prediction in an accepted ADR is
   indistinguishable from a statement of fact — which is exactly how §7's `facets_filter` column,
   described here and never built, sent #907 looking for it.

⚠️ **Five pre-existing defects surfaced only when Filters became live**, all fixed in the same PR;
they are recorded because each was invisible while the Engine ignored the struct: the `tag`
aggregator counted `post_tags` **only** (ignoring ~2/3 of the corpus's tags, which made the
count-equals-results acceptance unsatisfiable for that dimension); `Filters.Owner` used
`fmt.Sscanf("%d")`, so `owner:alice` produced **no** filter and `owner:12abc` produced owner 12;
save-as-collection resolved no capabilities; the saved-search executor dropped `compiled.Filters`
(it would have emailed owners hits their own search does not return); and the vector path
re-admitted rows the filter had just excluded. **A struct nothing consumes accumulates bugs
silently — "unused" is not "correct".**

### 4. Shared `visibility` package — load-bearing floor (B-2)

- New `app/internal/visibility/` package with `Filter(ctx, EntityType) → Predicate → Predicate.ToSQL(alias)` interface.
- Predicate carries caller's effective visibility set (own / team / public / federated-remote-visible / restricted-via-share). Renders to a `WHERE` fragment + bound params.
- Consumed by: search Engine (assets/collections/posts), facet aggregators, suggestion query, saved-search notifier at owner-actor context, and — as of PR #213 — search feedback's `PoolVisibility.CanSee`.
- **Consolidation status (1.16.B-followup, PR #213).** Pre-audit of #185 found that the "four surfaces duplicating the visibility check" the follow-up assumed only had ONE genuine duplicate: feedback's `PoolVisibility` inline `SELECT EXISTS(SELECT 1 FROM assets WHERE id = $1 AND deleted_at IS NULL)`. That was retrofitted to call the new `visibility.CanSee(ctx, pool, EntityAsset, caller, id)` helper — SQL generated by the helper matches the pre-retrofit shape byte-for-byte (proven by unit test). The other three "surfaces" have different semantic shapes:
  - **IIIF anonymous gate** (`app/internal/iiif/presentation/loader.go`) is a **field-level** metadata gate (`isAnonymous bool` threads into loader; public-flagged metadata pairs only), not a row-level visibility check. Consolidation would require adding a `FieldVisibility` API to this package — deferred (issue #211).
  - **`POST /search/by-image` coarse floor** (`app/internal/search/by_image.go:filterVisibleAssetIDs`) filters anonymous callers to `sensitivity = 'public'` — a column `visibility.Filter(EntityAsset)` does not currently touch. Unifying would silently change search Engine behaviour for anonymous text queries (currently permissive). Deferred (issue #210).
  - **Base list handlers** (`/assets`, `/collections`, `/posts`) use sqlc-static queries with hardcoded WHERE fragments. They do not call `visibility.Filter` today. Retrofitting means abandoning sqlc for those queries — bigger scope with real observable-behaviour risk. Deferred (issue #212).
- Snapshot-test discipline preserved: the retrofit's compliance signal is the byte-for-byte error-response suite in `app/internal/search/feedback/snapshot_test.go` (Phase 1.16.B-followup). Every HTTP error path (401 / 400 / 403 / 404) is compared verbatim against captured golden bodies.

#### 3c. THE ADDRESS OWNS THE FETCH — amendment 2026-08-13 (#1060, PR #1062)

The search page had **two** things that could start a query: its own controls (a kind chip, a
facet tick, a submit) called `runSearch` directly, *and* — after #1053 — the URL adoption did too.
Two writers of one result set, with no defined order between them.

That is what made #1060 possible. SvelteKit captures a history entry's snapshot **inside the
navigation commit, for the entry being left**
(`@sveltejs/kit/src/runtime/client/client.js:1862-1863`, `update_scroll_positions` then
`capture_snapshot(previous_navigation_index)`). A control that applied its state and fetched
*before* navigating therefore caused the **departing** entry's snapshot to record the **arriving**
entry's results. Back then faithfully restored a snapshot that was already wrong when taken —
the defect is in the capture, not the restore.

**The rule now:**

1. **A control writes the address and stops.** It does not fetch. Six direct `runSearch` calls were
   removed from the controls to establish this.
2. **The URL adoption performs the single fetch.** One writer, one order.
3. **A snapshot carries the signature of the results INSIDE it** — taken from what the current hits
   were actually fetched for, never from the live controls, which may already have moved.
4. **A restore whose signature does not match the address is REFUSED**, together with its scroll
   offset: that offset was measured against hits that are not coming back, so restoring it alone
   would land the reader mid-way down a list that no longer exists.
5. **A back/forward adoption holds its fetch until `navigating` clears** — SvelteKit does that
   immediately after running restores, so by then a restore has either happened or never will.

⭐ **#584 is strengthened by this, not merely preserved.** It restored a snapshot without ever
checking that the snapshot belonged to the address being restored to; that verification did not
previously exist.

⚠️ **One deliberate consequence, accepted 2026-08-13:** re-submitting an **unchanged** query is now
a no-op rather than a refetch — the same query is the same address, so there is nothing to adopt.
Forcing a refetch on submit was considered and rejected: it would give submit a side effect the URL
does not express, which is precisely the second fetch path this amendment removes. If an in-app
refresh is wanted, it belongs as an **explicit Refresh control** — a distinct intent — not as a
hidden behaviour of the search box.

**For anyone adding a control to this page: write the address. Do not fetch.**

#### 3d. A FRESH SEARCH RESETS THE RESULTS REGION — amendment 2026-08-28 (#1298, #1354)

3c decides what a RESTORE does with a stored offset. It is silent on what a **fresh search** does
with the offset it is leaving behind, and that gap is what #1298 reports: refining a query left the
reader wherever the browser happened to put them.

**The rule: refining is a new address. The results region resets to its first row; the page chrome
does not move.**

1. **A refine resets the scroll offset to the top of the results.** The signature changed, so by
   3c's own logic the old offset was measured against hits that are not coming back. 3c refuses
   such an offset on a restore; this says a fresh search discards it too, rather than leaving it to
   the browser.
2. **Back navigation still restores**, subject to 3c's unchanged signature check. Same address,
   same signature, restore. That is the case where the standard expectation and the refusal rule do
   not conflict.
3. **The reset targets the SCROLLPORT, not the document.** This app never scrolls the window
   (`web/src/lib/util/scrollport.ts`, #1122), so `window.scrollTo` is the wrong instrument and does
   nothing at all. `scrollportOf` is the single definition.
4. **An append is not a refine.** The reader continuing down a list they are already reading keeps
   their place.

⚠️ **The destination was never DECIDED before this, which is why it varied by machine.** Measured
on `/search`: six accumulated pages at offset 4511 of a 6088px grid, refined to a 25-hit query,
landed on **330 — exactly `scrollHeight - clientHeight`**, the bottom of the new list, with every
hit the reader had just asked for above the fold. #1298 recorded the other outcome on a taller
refined wall: Chrome's scroll anchoring re-resolved the offset against reflowed content and landed
on 0 on one workstation and on 279 (39px FURTHER DOWN than it started) on the CI runner. Neither is
expressible as `min(before, max)`, and both are legitimate anchoring outcomes. A page that does not
decide gets whichever one the content happens to produce.

⭐ **The browse wall looked correct without this, and that is an argument FOR stating it.** Measured
on a 900-card wall at 29457px: refining landed on 0 at 1080p and at 390px. But nothing in the route
decided that. `items = []` collapses the wall to zero height in the same frame, so `<main>` briefly
has nothing to scroll and the BROWSER clamps. The landing is correct only while the chrome above
the wall stays shorter than the viewport, which is a coincidence of the featured rail's height.
`/search` is the same shape with the coincidence absent, because it swaps its hits in place.

⚠️ **The reset is ordered BEFORE the fetch, not after the results land.** At offset 0 scroll
anchoring has nothing to compensate, so the swap cannot re-resolve the offset underneath it; a
reset applied afterwards would be racing the mechanism it is trying to undo. It also makes the
refine's acknowledgement immediate rather than a jump arriving 100ms later.

⭐ **It also resolves the interaction with infinite scroll (#1354).** `/search` gained the browse
wall's paging rig in the same sprint, and the hazard `web/src/routes/+page.svelte` documents for
the restore path is an OFFSET sitting inside the sentinel's lookahead over a one-page list, which
parks the reader somewhere they never scrolled to. A reset to the top removes the offset, so what
remains is the loader filling its buffer below a reader who is at the first row: the lookahead
doing its job, which is what the browse wall has always done after a reset.

#### 4c. THE MATCH ITSELF IS GATED — amendment 2026-08-13 (#902, PR #1063)

§4b gates a *filtered* search. This gates the **match**, and it closes the leak that made #902 the
milestone's security item: a `restricted` asset's `search_text` contains its own withheld title,
so any caller could query a phrase only that title held, watch the total move 0→1, and walk the
title token by token — recovering, one word at a time, exactly what #899 removed from the payload.

**`visibility.AssetSearchMatchSQL` is now the ONE expression of "this asset's indexed text matches
this caller's query"**, and every full-text surface over `assets` composes its WHERE clause from it
— `/search` hits, the `/search` COUNT, and browse's `?q=`. It ANDs `FieldsReadableSQL` (the SQL
twin of `FieldsReadable`, carrying the ownership and team-scoped `assets.admin` disjuncts) onto the
`@@`.

⭐ **Why a conjunct rather than a second, reduced `tsvector` column** — the design this arc first
proposed, and why it was rejected on the merits rather than on cost:

1. **The reduced document would be empty.** `rebuild_asset_search_text` composes from exactly three
   ingredients — title (A), description (B), `searchable`+`active` field values (D) — and
   `FieldsReadable` withholds all three. `@@ AND readable` and `@@ reduced-document` therefore
   return the **identical row set** for every caller and every query.
2. ⭐ **A column MATERIALISES a security decision; a conjunct EVALUATES it live.** If
   `FieldsReadable`'s rule changed, every row's column would keep enforcing the old rule until
   rebuilt. The conjunct cannot go stale.

A mature search engine's remedy for this class is *"split documents by index"*, and that is right
**for that engine** — index separation is forced there by corpus-wide IDF and aggregation APIs.
Postgres `ts_rank_cd` ranks from the row's own `tsvector` and the query alone, with no corpus-wide
statistics, so the channel that forces index separation elsewhere **does not exist here**. Importing
the remedy without its reason is what produced the column design; do not re-import it.

**If a genuinely public ingredient is ever added to the document** — the owner's display name is the
obvious candidate, since the placeholder already carries it — it belongs in a reduced column, and
`AssetSearchMatchSQL` is the single function that has to learn about it.

⚠️ **The facet aggregators deliberately do NOT compose this**, and their safety is load-bearing
rather than incidental: all five asset aggregators AND `ContentReadableSQL` over the same row, so a
row the caller cannot open contributes to no bucket whatever the query text says. **If that clause
is ever narrowed or made conditional, all five become #902 again** — the exclusion is documented at
the site.

#### 4b. An ACTIVE FILTER narrows to what the caller can open — amendment 2026-08-12 (#907)

**Unfiltered search is unchanged and this amendment does not touch it.** ADR 0064 keeps a
restricted asset **listed** as a placeholder, and `total_count` deliberately counts rows the
caller cannot open, so that the number and the array agree and neither becomes a readability
oracle. That stands.

**Under an active facet filter, those rows are excluded** (`visibility.ContentReadableSQL`, the
same clause the aggregators use). The reasoning, because this looks at first like the narrowing
the `total_count` rule forbids:

- **A filter asks a question about a field.** `extension:png` means *"which of these is a png"*,
  and answering it about a row whose columns are withheld hands over the exact field #899 removed
  from that row's payload. With a narrow enough selection, **the filter is the item**.
- **The exclusion is VALUE-INDEPENDENT, which is what stops it being an oracle.** The conjunct is
  gated on the *presence* of a filter (`if !q.Filters.Empty()`), never on which filter. A withheld
  row therefore returns nothing for **every** value of **every** dimension, so its absence
  discloses nothing the caller did not already learn from seeing the placeholder in the
  unfiltered result.
- **It must match the aggregators' clause exactly**, or the rail's count stops equalling the
  result set that ticking it returns — which is the defect #907 existed to remove.

⚠️ The exactness has a known cost, recorded rather than hidden: `ContentReadableSQL` carries no
mutation disjunct, so a team-scoped `assets.admin` holder — owed the *fields* of assets they
administer (#939) — is slightly **narrower** under a filter than unfiltered. Widening only the
Engine would break the count/filter equality; both clauses must widen together. **#1056** tracks
it. The current behaviour errs narrow, which is the safe direction.

### 5. Autocomplete via `pg_trgm` (B-2)

- Extension `pg_trgm` added in migration 00022.
- Suggestion corpus: tag names (currently applied) + collection names + post titles + asset titles + owner display names (public only), all filtered through `visibility.Filter`.
  - ⛔ *Corrected 2026-08-13 (#1064/#1075, PR #1076) — **this line describes the INTENDED corpus and has been
    read as the built one.** Suggest has **four** sources, not five: `tags`, `collections`, `postTitles`,
    `assetTitles`. **There is no owner-display-name source** — `visibility.OwnerDisplayNameSQL` is used by
    `posts/handler.go`, `collections/resources_page.go` and `assets/list_page.go`, never by `search/suggest/`.
    The planning agent relayed this line into a brief as current code state and the coding agent had to correct
    it. Also false as written: "all filtered through `visibility.Filter`" — the tag source was filtered by
    NOTHING until #1075, which is the leak that issue records.*
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

- `saved_search` table (migration 00023) stores DSL string + `types_filter` + ~~`facets_filter`~~ + hybrid tuple + `email_frequency ∈ {off, immediate, hourly, daily, weekly}` + `last_result_hash` + `last_result_ids UUID[]` + `last_check_at` + `last_notified_at` + `last_error`.
- Delta via hash-of-sorted-ID-set + linear-merge diff — deterministic, replayable.

  ⛔ *Corrected 2026-08-12 (#907, PR #1055) — **`facets_filter` was never built.*** It is in no
  migration and in no sqlc model; the table carries `Dsl` and the types filter, and nothing else
  that resembles a stored facet selection. This section has asserted the column's existence since
  the ADR was accepted, and #907 went looking for it on that basis. Struck rather than deleted,
  because the *intent* is still right: now that a facet selection is a first-class typed value
  (`facet.Selection`, see §5 amendment), persisting one with a saved search is a small, obvious
  addition — it simply has not happened. **An accepted ADR describing a column that does not
  exist is worse than silence, because the next person plans against it.**
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

### 8.5. Search feedback loop — ranking-quality signal (B-5-followup, PR #208)

- `search_feedback` table (migration 00028) records thumbs up/down on individual search-result cards: `(id, query_hash, dsl_query, hit_asset_id, hit_position, direction, user_ref, ip_hash, feedback_at)` with `UNIQUE (user_ref, hit_asset_id, query_hash)` enforcing vote-flipping via `ON CONFLICT DO UPDATE`.
- Query hash: SHA-256 over trim + collapse-whitespace + lowercase canonical form. **NOT full AST canonicalization** — `cat AND dog` and `dog AND cat` produce distinct hashes. Sufficient for MVP grouping; upgrade path is a canonicalizing DSL formatter.
- Rate limit: 60 votes / user / 24h via `SELECT COUNT(*) WHERE user_ref = $1 AND feedback_at > NOW() - INTERVAL '24 hours'`. Undo (DELETE) refunds the token naturally by lowering the count — no separate refund bookkeeping; survives restarts. Soft cap (not hard security); admin abuse-review page handles sophisticated cases.
- **Enumeration-safe visibility floor.** `PoolVisibility` predicate (asset exists + non-deleted) checked before upsert. Both `not-visible` and `not-exists` collapse to 403 `hit_not_visible` — attacker can't probe UUID existence via feedback submits. Consolidation with `visibility.Filter` shipped 2026-07-06 (PR #213, §4 above) — `PoolVisibility` now delegates to `visibility.CanSee(EntityAsset, ...)`.
- **Anonymized-by-default aggregation.** `GET /admin/search/feedback` shows top down-voted queries + under-ranked hits (both use `latest_dsl` CTE for display-form DSL per `query_hash`); never exposes user_ref. Per-user log at `GET /admin/search/feedback/audit/{user_ref}` requires typing a ref explicitly AND fires an `admin.search.feedback.audit_viewed` audit event.
- **Query cache NOT invalidated on feedback events.** Deliberate: feedback is out-of-band ranking-quality signal, not a real-time input to ranking. Results stay stable for the 60s cache TTL regardless of vote activity.
- Per-instance state — never federates. No `origin_server_id`, no outbox event. Cross-peer aggregation would require federation-safe user identity across peers + a cross-instance query surface, both out of scope.
- Runtime-toggleable via sysconfig `search.feedback.enabled` (pointer-bool for fresh-install-defaults-true semantic); reads per-request; toggling takes effect on the next request.
- Shared infrastructure additions: `auth.IPSubnetHash` exported with a `domain` argument (1.19.D lockout path delegates; domain prefix prevents cross-subsystem hash collision on rotated salts). Five new `search.Counter` Result classes (`search_feedback_{up,down,undo,rate_limit,disabled}`) + `AsFeedbackCounter` adapter mirroring the saved-search pattern. `search_feedback_active_voters` gauge (DISTINCT user count in aggregation window) on `/admin/search/health`. New Feedback tile on `/admin/search/dashboard`.
- **Signal payoff.** With #208 shipped, the arc has structured data on ranking quality — 'which queries surface bad results,' 'which relevant hits are getting buried' — instead of vibes-based feedback. This is one of the named revisit triggers for ADR 0055 (pg_search research-record). Also positions AA to consume the signal via a future learned-ranking layer without touching the collection surface.

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
- `visibility.Filter` extraction closes the leak-vector risk that per-endpoint inline filters carry (feedback retrofit shipped PR #213; list-handler / IIIF field-level / by-image sensitivity-column consolidations tracked as follow-ups documented in §4 — deferred because each requires distinct API additions that would silently change behaviour without them)
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
- Follow-up issues: #183 (CLIP visual encoder — SHIPPED PR #199), #184 (feedback loop — SHIPPED PR #208), #185 (visibility retrofit — SHIPPED PR #213, scope-trimmed per pre-audit; see §4 for the three deferred sub-scopes), #186 (AdminBackfillPanel extraction — SHIPPED PR #215), #209 (`search.Counter` split — SHIPPED PR #217), #210 (sensitivity-column semantics for `visibility.Filter(EntityAsset)` — unify by-image coarse floor, deferred from #185 pre-audit), #211 (`FieldVisibility` API for IIIF metadata gating — deferred from #185 pre-audit), #212 (sqlc migration path for list-handler visibility consolidation — deferred from #185 pre-audit), #214 (MDX braced-identifier CI gate on docs PRs — deferred docs-tooling from PR #213)
