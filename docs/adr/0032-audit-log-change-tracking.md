# ADR 0032: Audit log & change tracking — unified event log

- Date: 2026-05-30
- Status: Accepted

## Context

The audit-trail story has accreted across the roadmap. Phases 1.17,
1.20, 1.21, 1.24, 1.26, 1.27, 1.28, and 1.32 each carry an audit
hook — table-level diffs, share-link fetches, sensitivity reveals,
license events, bulk-op selection IDs, retention tombstones. There
is even an `app/internal/audit/` package in tree with `events.go`,
`models.go`, `queries.sql`. But there is no single phase, no single
admin surface, no single retention policy, no single export format
— the audit log is currently a pile of hooks looking for a system.

This ADR consolidates the audit story into one feature. The 7+ other
phases keep their audit-recording bullets but the **consumption**
(taxonomy, schema, admin UI, retention, export) moves here.

## Decision

Add Phase 1.40 — Audit log & change tracking — as the central event
log. Other phases emit events to this log; this phase owns the
schema, the capture API, the admin surface, and the export.

### Event row schema

```jsonc
{
  "id": "evt_01H...",              // ULID for sort + uniqueness
  "ts": "2026-05-30T12:00:00Z",    // server timestamp
  "actor": {
    "type": "user|service|system|anonymous",
    "user_id": 42,                 // null for non-user actors
    "session_id": "...",
    "ip": "203.0.113.7",
    "user_agent": "..."
  },
  "action": "asset.sensitivity.changed",  // dotted event type
  "target": {
    "type": "asset|post|collection|user|team|share-link|listing|...",
    "id": 1234,
    "display": "character_v3"      // denormalized for log readability
  },
  "outcome": "success|failure|partial",
  "changeset": {                   // null for non-edit events
    "before": { "sensitivity": "team" },
    "after":  { "sensitivity": "restricted" }
  },
  "metadata": {                    // event-type-specific bag
    "reason": "NDA expiry trigger",
    "scheduled_action_id": "sa_..."
  },
  "correlation_id": "req_...",     // groups events from one HTTP request / job
  "license_kid": "2026-01-key1"    // license at time of event (for compliance)
}
```

### Event taxonomy

Dotted naming. Categories:

| Category | Examples |
|---|---|
| `auth.*` | login, logout, mfa.challenge, session.expire |
| `user.*` | created, updated, role.assigned, approved, suspended, deleted |
| `team.*` | created, member.added, member.removed, role.changed |
| `asset.*` | uploaded, edited, deleted, sensitivity.changed, restored, reveal |
| `post.*` | created, published, archived, state.transitioned |
| `collection.*` | created, updated, items.added, items.removed, featured.set |
| `share.*` | link.created, link.opened, link.password.challenged, link.revoked |
| `comment.*` | created, flagged, hidden, restored, deleted |
| `annotation.*` | created, edited, deleted |
| `license.*` | issued, refreshed, expired, tier.changed, revoked |
| `commerce.*` | listing.created, order.placed, order.fulfilled, refunded |
| `platform.*` | connected, published, sync.drift, disconnected |
| `system.*` | startup, shutdown, config.changed, integrity.check, migration |
| `federation.*` | peer.connected, outbox.flushed, conflict.resolved |

Adding a new category is a code change. Adding a new event under an
existing category is a one-liner — the capture API allows arbitrary
`action` strings that lint-validate against the category prefix.

### Capture API

In Go:

```go
audit.Record(ctx, "asset.sensitivity.changed",
    audit.Target("asset", assetID, asset.Name),
    audit.Changeset(beforeMap, afterMap),
    audit.Metadata("reason", "NDA expiry trigger"),
)
```

`Record` is synchronous-write to a buffered channel, flushed by a
worker every 100ms or every 1k events. Failure to record an event
NEVER fails the originating operation — buffer overflow drops the
event and increments a counter (which itself is an event).

`actor`, `correlation_id`, and `license_kid` come from the context.
Middleware seeds them on every HTTP request and job-queue job.

### Admin surface

`/admin/audit`:

- **Filter** — faceted by actor, action prefix, target type, time range,
  outcome.
- **Search** — full-text across the `changeset` and `metadata` blobs.
- **Detail** — click an event to see the full row + correlated
  request events (same `correlation_id` — the "who else was affected
  by this one request").
- **Export** — CSV / JSON with operator-picked columns + a date
  range. Streamed so 10M-row exports work.

### Signed export (Enterprise)

Enterprise tier per ADR 0017 adds a signed export variant:

```
GET /admin/audit/export.jsonl.sig
```

Returns the JSONL log concatenated with an Ed25519 signature over
the SHA256 hash of the content, using a key bound to the instance.
Public key is exposed at `/.well-known/audit-signing-key` so
auditors can verify off-line. This is the same signing-infrastructure
shape as ADR 0017's `.lic` files.

### Retention

- Default 7 years (legal floor in most jurisdictions).
- Configurable per category via admin (e.g., `auth.*` retained 90
  days because login spam, `commerce.*` retained 10 years because
  financial records).
- Per-row retention overrides for legal-hold events.
- Retention enforcement runs via the Phase 1.28 scheduled-action
  engine — one job per category per night.
- GDPR DSAR per ADR 0024 tombstones a user across all events but
  preserves the event rows themselves; the actor reference is
  reassigned to the `deleted-user-{id}` placeholder.

### Cross-phase wiring

The phases that already specify audit-recording bullets become
**callers** of the capture API. The phase descriptions stay as-is;
this phase delivers the consuming infrastructure they assume. Phase
1.17's "table-level change tracking with diffs" is exactly the
`changeset` block on the event row — same feature, now owned here.

### What this is NOT

- **Not application logging.** Audit events are user-meaningful
  actions; application logs are debugging traces. Different
  consumers, different retention, different volumes. Application
  logs go through the Phase 1.41 observability story.
- **Not an activity stream.** Activity streams are user-visible
  comments (Phase 1.21). The audit log can FEED an activity stream
  (filter to user-visible events, render as comments) but is the
  underlying source of truth, not the surface.

### Per-tier behaviour

- **Community + Pro:** full audit log + admin surface + CSV/JSON
  export. Same feature.
- **Enterprise:** + signed export for non-repudiation per ADR 0017,
  + per-category retention overrides, + multi-instance audit log
  federation (a parent instance can pull child-instance audit
  streams for org-wide compliance).

Enforced via the tangled-derivation model from ADR 0017.

## Consequences

**Positive**

- One coherent feature instead of seven scattered hooks. Operators
  and auditors have a single surface to learn.
- The `correlation_id` link lets a single request's downstream
  effects be reconstructed — the "what did this approval cascade
  trigger?" question becomes a single query.
- Signed export at Enterprise tier is a real compliance
  differentiator that's expensive to retrofit and easy to ship now.
- Capture API is `audit.Record(ctx, ...)` — a single line at each
  emission site keeps the cost of "log everything that matters"
  low.

**Negative**

- Buffered async writes mean the audit log is **not** a transactional
  guarantee. A crash between buffer enqueue and disk flush loses
  the event. Mitigation: the buffer is sized so 100ms of writes
  fit, and the flush worker fsyncs after each batch. For events
  that genuinely cannot be lost (Enterprise legal-hold use cases),
  a synchronous variant `audit.RecordSync` is available at a
  per-call latency cost.
- Schema evolution: adding fields to the event row is fine (JSON
  metadata); changing the schema requires a migration that
  understands historic rows.

## Alternatives considered

- **Use the existing `app/internal/audit/` package as-is.** It's
  already there but covers only some subsystems. This ADR is the
  consolidation, not a replacement — it extends the existing
  package with the schema + admin surface + export + tier behaviour.
- **Use a third-party audit service (Auth0 Audit Trail, Datadog
  Audit).** Vendor lock-in and the data trip leaves the self-hosted
  story. Rejected.
- **Synchronous writes only.** Hurts hot-path latency for every
  recorded event. Buffered async with a sync escape hatch is the
  better balance.

## Reference

- Phase 1.40 in [`docs/roadmap.md`](../roadmap.md).
- Existing `app/internal/audit/` package — the consolidation
  extends, not replaces.
- ADR 0017 monetization — Enterprise signed export uses the same
  signing infrastructure pattern.
- ADR 0024 privacy — DSAR tombstoning rules.
- Scheduled-action engine (Phase 1.28, ADR 0020) — retention
  enforcement.
- Observability (Phase 1.41, ADR 0033) — application logs &
  metrics are separate.
