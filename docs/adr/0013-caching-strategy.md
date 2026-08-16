---
id: "0013"
title: Caching strategy — in-process LRU + Postgres LISTEN/NOTIFY, no Redis
status: accepted
date: 2026-05-26
area: infrastructure
phases: 
  - "1.5"
  - "1.9"
  - "1.10"
supersedes: []
related: 
  - "0010"
  - "0012"
tags:
  - infrastructure
  - ai
excerpt: >-
  At 2M+ assets per server, every hot read path needs to avoid hitting Postgres for unchanged data. Specific pain points the metadata work (ADR 0012) is about to make worse:
---
## Context

At 2M+ assets per server, every hot read path needs to avoid hitting
Postgres for unchanged data. Specific pain points the metadata work
(ADR 0012) is about to make worse:

- Every asset render or list-row resolves field definitions. Field
  defs are small (hundreds of rows per install), rarely change, and
  read on every request. Untreated, they're a JOIN on every asset
  page.
- Role + capability lookups happen on every authenticated request.
  Already in-memory in the request lifecycle but not cached across
  requests.
- Resource type list, system_config (site / SMTP), and the like —
  read often, written almost never.

Off-the-shelf answer: drop Redis in front. But Redis is another
container, another HA story, another piece of ops infrastructure
for self-hosters who just want to run artist-alley on a single VM.
Two-instance horizontal scaling will eventually want Redis, but not
yet.

This ADR locks in the cache strategy through MVP and one tier
beyond. Redis can be added later behind the same interface (ADR
addendum or 0013.1) without rearchitecting.

## Decision

### Two tiers, no Redis

**Tier 1: In-process LRU caches** (one per data domain) using
`github.com/hashicorp/golang-lru/v2`. Each cache:
- Holds the hot subset of one domain (field definitions, roles,
  resource types, recent asset_by_id reads).
- Has a configured max size in entries (defaults below).
- Supports TTL where freshness matters; pure LRU where invalidation
  is event-driven.

**Tier 2: Postgres `LISTEN/NOTIFY`** for cross-instance
invalidation. Each app instance subscribes on boot to one channel,
`cache_invalidate`, and listens for payloads of the form:

```json
{ "domain": "field_definition", "key": "<id-or-code>", "op": "upsert" }
```

When a write commits, the writer emits `NOTIFY cache_invalidate,
'<json>'` *after* the transaction is durable. Every subscribing
instance drops the matching LRU entry. Single instance is unaffected
(it invalidates its own cache directly on write).

Two-tier resolution per read:
1. Look up in in-process LRU. Hit → return.
2. Miss → query Postgres, populate LRU, return.

Writes:
1. Write to Postgres (transaction commits).
2. Invalidate local LRU entry.
3. `NOTIFY cache_invalidate` so peer instances drop their copy too.

### Cache namespaces (initial set)

| Namespace | Size | TTL | Notes |
|---|---|---|---|
| `field_definition` (by id, by code) | 5000 | none | invalidate on field upsert |
| `asset_type` | 100 | none | invalidate on asset_type change |
| `role` (by id, by name) | 500 | none | invalidate on role / role_capabilities change |
| `user_capabilities` (by user_ref) | 10000 | 5 min | TTL because grants/revokes happen ad-hoc |
| `asset_by_id` | 50000 | 1 min | hot read; invalidates on asset update |
| `system_config` (by key) | 50 | none | invalidate on system_config write |

Sizes are conservative starting points; adjust by metrics post-MVP.

### Cache package shape

```go
// internal/cache/cache.go

type Cache[K comparable, V any] interface {
    Get(K) (V, bool)
    Set(K, V)
    Delete(K)
    Purge()
}

// Registry binds cache domains to invalidation channels.
type Registry struct {
    Pool       *pgxpool.Pool
    Logger     *slog.Logger
    Domains    map[string]invalidator   // wraps the concrete LRU
    NotifyConn *pgx.Conn                 // long-lived LISTEN connection
}

// Start spins up the LISTEN goroutine. Idempotent.
func (r *Registry) Start(ctx context.Context) error { ... }

// EmitInvalidate runs after a write commits.
func (r *Registry) EmitInvalidate(ctx context.Context, domain, key string) error {
    _, err := r.Pool.Exec(ctx,
        `SELECT pg_notify('cache_invalidate', $1)`,
        fmt.Sprintf(`{"domain":%q,"key":%q,"op":"upsert"}`, domain, key))
    if err != nil { return err }
    // Drop locally too — NOTIFY doesn't echo to the emitting connection
    // unless we re-subscribe; just nil our own LRU entry directly.
    if inv, ok := r.Domains[domain]; ok {
        inv.Invalidate(key)
    }
    return nil
}
```

Generic over key/value type via Go generics. Per-domain LRUs are
constructed at boot, owned by the package that reads from them.

### Generation-counter pattern for set-wide invalidation

Some operations invalidate a whole namespace (e.g., a bulk field
definition change, or a taxonomy re-parent). Per-key invalidation
is wrong here — we'd need to enumerate every cached entry. The
pattern:

- Each cache namespace keeps a `generation` counter (atomic uint64
  in memory).
- Cache entries store the generation they were populated under.
- On read, if `entry.generation < current_generation`, treat as
  miss.
- Bumping the generation is O(1) and invalidates the whole namespace
  atomically.

Use sparingly — per-key invalidation is the default.

### Search index maintenance

`assets.search_text TSVECTOR` (per ADR 0012) is maintained by a
Postgres trigger on `asset_field_value` change. The trigger
recomputes search_text and updates `assets.updated_at`. The asset's
LRU entry is invalidated via the `asset_updated` NOTIFY hook that
fires from the trigger:

```sql
CREATE OR REPLACE FUNCTION asset_field_value_changed() RETURNS trigger AS $$
BEGIN
  -- (full search_text rebuild logic here)
  PERFORM pg_notify('cache_invalidate',
    json_build_object('domain', 'asset_by_id',
                      'key', NEW.asset_id::text,
                      'op', 'upsert')::text);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
```

So even DB-side writes propagate to every app instance without
application code being aware.

### What we do NOT cache

- Asset list query results. Too many filter dimensions; cache hit
  rate would be terrible. Postgres indexes (already in place) carry
  the load.
- Search results. Cache key would have to encode the full query;
  effort isn't worth it pre-Redis.
- Session lookups. Already a single indexed query; results are
  user-specific so per-instance cache wouldn't help much.
- Storage objects. Content-addressed; the bytes are immutable. The
  storage backend handles its own caching (HTTP Range, OS page
  cache, S3-side caching for the s3 backend).

### When Redis enters the picture

Triggered when any of these become true:
1. Multi-instance with shared session state (we'd want sessions in
   Redis rather than DB-only).
2. Cross-instance query result caching (list pages, search) becomes
   worth the maintenance cost.
3. Rate limiting needs cross-instance state (Phase 1.5's in-process
   token bucket is single-instance only).

When that happens: the existing cache.Cache interface gets a Redis
backend behind it, and per-domain config picks `in-process` vs
`redis`. No application code changes.

## Consequences

**Positive:**
- Three-container deployment (nginx + app + postgres) stays the
  same.
- Federation-friendly: NOTIFY broadcasts across all instances
  listening to the same Postgres, with no shared cache state.
- Trigger-driven invalidation means even SQL writes from outside Go
  (PHP coexistence, manual SQL fixes) propagate correctly.
- Generation counter pattern handles bulk operations without
  per-key churn.

**Negative:**
- Single Postgres NOTIFY channel for everything → all instances
  decode every event, ignore most. At 2M assets and steady-state
  edit volume this is still ≤ small-thousands of NOTIFYs per
  minute — well within Postgres's NOTIFY capacity. Mitigation if it
  becomes a problem: split channels per domain.
- In-process means each app process has its own cache; memory cost
  scales linearly with instance count. Sizes above are tuned for
  one or two instances.
- Cache cold-start is hot DB after a restart. Acceptable for now;
  background warmup pass on boot lands if it becomes painful.

**Deferred:**
- Concrete `cache` package implementation lands in Phase 1.10,
  immediately after Phase 1.9 (metadata) needs it for
  field_definition lookups.
- Redis backend implementation behind the same interface, when
  multi-instance scaling demands it.
- Search-result caching — needs ADR 0010 (search DSL) first so the
  cache key shape is stable.

---

### Amendment 2026-08-06 (#935, PR #945) — writes that reach other domains through the SCHEMA

This ADR describes how a cache is invalidated by *the code that owns the write*. That is the
common case and it is unchanged. It does not cover the case where a write empties rows in a
package that never runs — and that gap produced two live staleness bugs.

**A hard delete is the one asset write that crosses domains through the database rather than
through code.** `asset_subtitle_tracks` and `post_assets` carry `ON DELETE CASCADE`, so deleting
an asset removes rows in packages the deleting code has never heard of. **The database ends up
consistent; the in-process LRUs those packages keep do not, and nothing in the database can tell
them.** A read-through cache goes on answering from the pre-delete world until the process
restarts.

The decision:

- **A write whose effects propagate via schema-level CASCADE owes an explicit cache fan-out.**
  Consistency in Postgres is not consistency in the caches, and the FK gives no callback.
- **The fan-out belongs at the composition root, behind a hook on the writing service** — here
  `softdelete.Service.OnAssetsHardDeleted`. Calling the affected domains directly would give a
  generic GC primitive imports of `subtitles`, `posts` and `iiif/presentation`, inverting the
  dependency direction for three domains at once. The hook keeps one readable list of which
  caches a hard delete touches.
- **Best-effort, never propagating.** The write has already committed; a failure to evict must
  not turn a completed job into a failed one. Log and continue — the same discipline the
  storage-unpin step already uses.

**The corollary that actually bit us: every mutating path needs the sweep, not just the dramatic
ones.** `UpdateAsset` — an ordinary metadata PATCH — invalidated **nothing at all**, so renaming
an asset left every holding post and its IIIF manifest serving the old title until a restart.
That is the same defect #920 fixed for delete and restore, on the path users actually hit, and it
survived because attention went to the destructive operations. The helper is now
`assets.invalidateDerivedCaches` and runs on **update, delete and restore**.

**A cross-package `Invalidate*` helper with no callers is a claim, not a mechanism.** Three such
helpers existed; two were genuinely unwired and one was correct-as-is, and *all three* had doc
comments asserting callers that did not exist. When adding one, wire it in the same change or say
in its doc why nothing calls it — see [[feedback_a_comment_is_not_a_call_site]].

---

### Amendment 2026-08-11 (#557, PR #1022) — a cross-caller cache must never hold caller-dependent data, and the mutation that moves a counter must invalidate the domain that serves it

Two rules, both learned the hard way on the same sprint.

**1. Caller-dependent redaction does not belong in a shared cache entry.** The post payload is
served from a cache shared by every caller. #557 needed an *author identity* on it — and identity
is exactly the kind of value that differs by who is asking: an anonymous reader must not see a
`hide_from_anonymous` user's name, and must not see the `fullname` rung at all (ADR 0070 §3).

Putting the resolved identity **into** the cached entry would mean **the first reader to warm it
decides what every later reader sees.** An authenticated reader warms the entry with full
identity; the next anonymous request is served that entry. The leak would be intermittent,
ordering-dependent, and invisible in any test that populates the cache from the same caller it
asserts with.

So the cache stores the caller-independent post, and identity is applied in an **enrichment pass
after the cache read**, per request, against that caller's own visibility. The rule generalises:

> **If a value depends on who is asking, it is computed after the cache, not stored in it.**
> A shared entry may hold only what every caller may see.

This is the caching-shaped statement of the same principle ADR 0064 applies to fields and ADR
0070 to profiles. Note that the brief for #557 said *"do it in the list query as a JOIN"* — which
is both impossible here (the list query only picks rows; payloads come from the cache, so a joined
column is discarded) and, had it worked, would have written caller-dependent data straight into
the shared entry.

**2. A mutation must invalidate the domain that SERVES the value it changed — not the domain it
belongs to.** `social`'s like and comment paths move `posts.like_count` via triggers, and
invalidated only their own follow/block domains. Nothing invalidated the post-by-id domain, so a
like updated the row and the API kept serving the stale count — measured at DB `like_count = 4`
against an API-served `3`. It survived because `like_count` was hover-overlay decoration nobody
watched; a like *button* turns it into a heart that fills over a number that never moves, even
after a reload.

The package boundary is what hid it: `social` cannot import `posts`, so the domain constant now
lives in the shared `cache` package and both sides name the same one. **When a write in package A
changes a value package B serves, the invalidation belongs on A's write path, and the domain
constant belongs somewhere neither owns.** A test asserting the constants match is cheap and was
mutation-tested here.

#### Applied again, unchanged, 2026-08-13 (#1026, PR #1069) — and the two paths took different answers

The collection cover mosaic is the second value to meet rule 1, and the *shape* of the answer is
worth recording because it was not uniform across the surfaces that carry it:

- `GET /collections/{id}` is served from the by-id LRU. A cover set **is** caller-dependent — a
  member the caller may not picture is absent from it — so the covers are composed in an
  **enrichment pass after the cache read**, exactly as #557's author identity is. Nothing
  caller-dependent enters the cached `Collection`.
- `GET /collections` is **query-served, not cache-served**, so its covers compose beside the query
  with no enrichment seam at all.

⭐ **The generalisable part: "does this value belong in the cache entry?" is not a property of the
value, it is a property of the PATH.** The same covers are safe inline on one surface and forbidden
inline on the other, and a reviewer who checks only "is this caller-dependent?" gets the second
half wrong. Ask what serves the payload first.

The `/search` collection hit is a third case and passes on different grounds: its result cache key
already includes the caller ref plus the content and post capability sets, which is precisely the
triple the cover composer reads — so the entry is not cross-caller to begin with. That is the same
argument `keyForQuery`'s own comment makes in general form: **every input the Engine reads is a
component of the key.** A value added to a cached payload must either be caller-independent or have
all of its inputs in the key; there is no third option.

---

### Amendment 2026-08-08 (#887, PR #971) — a declined cache: scratch buffers under a cgroup ceiling

The preview resampler's scratch buffers (`x/image/draw` kernel scalers) presented a textbook cache
opportunity: constructed scalers reuse their internal buffers, and the measured tuple hit rate on a
real render storm was **94.6%**. **The process-wide cache was declined anyway**, and the reasoning
is the decision worth recording:

- A scratch buffer is sized destination-width × **source**-height — the largest observed single
  buffer was **889 MB**. A process-wide cache pins its largest entries **resident forever**.
- Under a cgroup ceiling, **bounded churn beats permanent residency**: churn is reclaimable the
  moment pressure ends (measured: 3.49 GB released within one sample of storm end); a pinned cache
  is a permanent bite out of the ceiling that no GC returns.
- So reuse is **job-scoped only** — a scaler lives for one job (the 36-frame turntable loop) and
  dies with it.

**The companion decision: the scratch budget is tied to `GOMEMLIMIT`, not core count.** Concurrent
resamples share a byte-weighted semaphore with budget `GOMEMLIMIT/10`, clamped [128 MiB, 1 GiB],
logged at boot as `preview.scale_budget`. Bytes rather than worker-count because the cost
distribution is extreme (7% of ops are 62% of the bytes) — a count bound either starves the cheap
90% or fails to bound the expensive 7%. Tying to `GOMEMLIMIT` keeps one knob: resize the container
and every derived ceiling moves with it (same principle as #781's GOMEMLIMIT derivation).

**The general rule this adds to the ADR**: when deciding whether to cache under a memory ceiling,
weigh *reclaimability*, not just hit rate. A 94.6% hit rate argued for the cache; the 889 MB
pinned-forever tail argued against; the tail wins under a cgroup.
