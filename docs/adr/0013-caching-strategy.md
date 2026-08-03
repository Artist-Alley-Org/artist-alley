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
