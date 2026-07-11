---
id: "0044"
title: Activities ledger — CQRS-lite federation backbone
status: accepted
date: 2026-06-05
area: architecture
phases:
  - "1.22.A-bis"
supersedes: []
related:
  - "0042"
  - "0043"
  - "0011"
  - "0024"
tags:
  - architecture
  - federation
  - cqrs
  - event-sourcing
  - go
excerpt: >-
  Every state-mutating federated social action emits an Activity in
  the same database transaction as its domain write. The activities
  table is the canonical record (source of truth) for federation;
  the domain tables (posts, comments, likes, user_follows,
  user_blocks, direct_messages, notifications) are kept in sync
  synchronously as optimized read projections. CQRS-lite — one
  ledger, multiple typed projections — without the operational
  cost of pure event-sourcing.
---

## Context

[ADR 0043](/adr/0043-federation-walled-garden-protocol/) committed
artist-alley to a walled-garden federation protocol. The protocol
needs to know: when a user likes a post, what is the canonical
record of that fact? The federation outbox needs to publish an
activity to every paired peer that holds a share for that post —
but the existing `likes` table has no AP-shape, no signature, no
addressing, no audit trail, no ability to be re-published from
disaster recovery.

Three patterns were on the table:

1. **Translator layer.** Keep domain tables as the source of
   truth. At federation-publish time, translate from the domain
   row into an AP envelope, sign, deliver. Don't store the
   activity locally.
2. **CQRS-lite / Mastodon-shape.** Domain tables are the source
   of truth for normal reads, but every mutation ALSO writes an
   AP-shape activity row to a separate ledger table in the same
   transaction. The activity row is what federation publishes;
   the domain row is what the UI reads. This is what Mastodon,
   Pixelfed, PeerTube, Lemmy actually do.
3. **Pure event-sourcing.** The activity ledger is the source of
   truth; domain tables don't exist (or are rebuilt by replaying
   the ledger). Every read goes through projection.

Pattern 1 was rejected because:
- No audit trail. "What activities did user X publish between
  Y and Z?" becomes a join across every domain table.
- No replay path. Disaster recovery / federation re-sync
  ("re-publish my full history to a newly-paired peer") becomes
  a per-domain-table walker.
- No idempotency anchor. Without a stable activity URI we can
  generate up front, delivery retries can't dedup at the receiver.
- Drift risk. Two code paths (domain write vs. federation
  publish) computing the same logical thing — they will diverge.

Pattern 3 was rejected because:
- Every read becomes a projection step. Hot paths (browse feed,
  post detail) can't afford this; we'd build caches around the
  projection rebuild, which defeats the conceptual purity.
- Schema migrations become projector-version migrations.
- Two-user concurrent edits become a CRDT problem; federation
  amplifies it across instances.
- One-person codebase maintenance burden is high. Mastodon /
  PeerTube / Pixelfed do NOT do this for the same reason.

Pattern 2 — CQRS-lite — is the working consensus among large
deployed federation systems. We adopt it.

## Decision

### The invariant

**Every state-mutating federated social action MUST emit an
Activity in the same database transaction as its domain write.**

If a handler mutates social state without calling
`activities.RecordActivity(ctx, tx, ...)` inside the same
`pgx.Tx`, it has written a bug. The code-review checklist
enforces this; the federation outbox dispatcher
([Phase 1.22.D](/roadmap/)) assumes it.

The activity row is the canonical record for federation. The
domain row is an optimized read projection, kept in sync
synchronously by virtue of being committed in the same
transaction.

### What "federated social action" means

The activities ledger records actions that have AP semantics —
the things peers might want to know about:

- Posts created / updated / deleted (`Create` / `Update` /
  `Delete` of `Note` / `Article` / `aa:Post` objects).
- Comments (`Create` of `Note` with `inReplyTo`).
- Likes / unlikes (`Like` / `Undo(Like)`).
- Follows / unfollows (`Follow` / `Undo(Follow)`). The auto-
  `Accept` for our walled garden also emits.
- Blocks / unblocks (`Block` / `Undo(Block)`). Per AP §6.9,
  `Block` MUST NOT be delivered to the blocked actor.
- Direct messages (`Create` of `Note` with `to:[recipient]`).
- Custom artist-alley activities per [ADR 0043 §"Custom activity
  types"]: `aa:Share`, `aa:Unshare`, `aa:Approve`,
  `aa:RequestChanges`, `aa:MarkReviewed`, `aa:Annotation`,
  `aa:WorkflowTransition`, `aa:AssetVersion`, `aa:Subscribe`,
  `aa:Mention`.

What is **NOT** in the ledger:

- Operator-internal state (sysconfig writes, job-queue mutations,
  cache invalidations, license uploads).
- Read-only events (a user viewing a post; profile page hits).
- Auth events that don't propagate federally (login attempts,
  session creation). These live in the audit log
  ([ADR 0033 / Phase 1.40](/roadmap/)).

If in doubt: an action emits an activity iff a paired peer with
appropriate `federation_shares` access would care about it.

### Two sources, one ledger

The `activities.source` column distinguishes:

- `'local'` — emitted by handlers on this instance. The
  federation outbox dispatcher reads these to publish.
- `'https://{peer}'` — received from a federated peer via inbox
  delivery (Phase 1.22.D). Dispatch updates local projections
  the same way a local emit would, modulo the peer-owned-vs-
  local-owned distinction in AP §7.3.

One table, one query path for "show me everything that's happened
involving X." Audit, replay, federation outbox, and federation
inbox all consult the same ledger.

### Idempotency

`activity_uri` is `UNIQUE`. Re-recording the same activity is a
no-op via `ON CONFLICT (activity_uri) DO NOTHING` in the writer.
This applies to:

- Job retries (the outbox worker re-runs after a transient
  failure).
- Replay tool (operator runs `./aa activities replay --actor=X`
  to rebuild federation outbox for a peer re-sync).
- Peer redelivery (a peer retries an inbox POST after a network
  blip).

Idempotency is the contract. Handlers MUST generate stable
`activity_uri` values via `activities.MintActivityURI(baseURL)` +
their own uniqueness convention (one URI per logical action).

### Caching

Per-actor outbox feed is cached behind `activities.actor_outbox`
(`cache.Registry` NOTIFY broadcast on every emit so federated
peers' replicas stay in sync). Per-object timeline is cached per
`(object_kind, object_local_id)` for the admin audit drill-down.
Cold misses fall through to the indexed query.

The cache holds compact `CachedOutbox` summaries (URI + type +
object URI + timestamp) — not the full payload. Full payloads
fetch on demand from the indexed row, which is fast.

### Per-type emit helpers

Each activity type gets a typed helper in
`app/internal/activities/emit/` (lands in 1.22.A-bis-2 when we
wire handlers — the package is created now but populated then).
Pattern:

```go
// in app/internal/activities/emit/like.go
func Like(actor users.ActorRef, post *posts.Post) activities.Input {
    return activities.Input{
        Type:        federation.ActivityLike,
        ActivityURI: activities.MintActivityURI(actor.InstanceBaseURL),
        ActorUserRef: &actor.UserRef,
        ActorURI:    actor.URI,
        Object: &activities.ObjectRef{
            URI: post.CanonicalURI(),
            Kind: activities.ObjectKindPost,
            LocalID: post.ID.String(),
        },
        // Likes are public by AP convention (`to:[author]`, `cc:[Public]`).
        To: []string{post.Author.URI},
        CC: []string{activitystreams.PublicCollection},
    }
}
```

The handler then calls `writer.RecordActivity(ctx, tx, emit.Like(actor, post))` —
the typed helper builds the right shape, the writer validates +
persists.

### Backfill (deferred)

Historical rows (posts/comments/likes/follows/blocks/DMs that
existed before this ADR landed) have no corresponding activity
rows. Federation re-sync to a newly-paired peer would not include
them.

Per user direction, **backfill is deferred to a later migration**.
When it lands, it walks each domain table and synthesizes
activities with `published_at` set to the original row's
`created_at`. Until then, federation outbox can only publish
activities emitted after the ADR-0044 wiring lands.

This is acceptable because:
- Federation is not live yet (Phase 1.22.D wires delivery).
- Backfill correctness is a one-time, well-bounded migration —
  easier to write when we have a few more activity types wired
  + know all the per-table edge cases.
- Existing data continues to read correctly from domain tables
  the whole time.

### Wiring schedule

- **1.22.A-bis-1 (this commit)**: Migration 00049 + activities
  package skeleton + writer + idempotency + tests + this ADR.
  **No handlers wired yet.**
- **1.22.A-bis-2**: Wire every existing social handler to emit.
  Each emit lands in the same transaction as the existing domain
  write; rollback on either failure rolls back both. Notification
  fan-out moves inside the emit helper so it can't be forgotten.
- **1.22.A-bis-3**: Admin `/admin/activities` audit UI, `./aa
  activities replay` CLI, Prometheus metrics.
- **1.22.B onward**: Federation phases as planned. The outbox
  dispatcher in 1.22.D reads from this ledger.

### What changes for existing handler code

Today's typical handler:

```go
func (h *Handler) LikePost(ctx context.Context, req ...) {
    if err := New(h.Pool).LikeTarget(ctx, params); err != nil {
        return nil, err
    }
    return resp, nil
}
```

Post-1.22.A-bis-2 shape:

```go
func (h *Handler) LikePost(ctx context.Context, req ...) {
    tx, err := h.Pool.Begin(ctx)
    if err != nil { return nil, err }
    defer tx.Rollback(ctx)

    if err := New(tx).LikeTarget(ctx, params); err != nil {
        return nil, err
    }
    if _, err := h.activities.RecordActivity(ctx, tx, emit.Like(caller, post)); err != nil {
        return nil, err
    }
    return resp, tx.Commit(ctx)
}
```

Three line changes per handler. The compile-time enforcement
(handlers must take a `pgx.Tx` not a `*pgxpool.Pool` to call
`RecordActivity`) catches forgotten emits at build, not at
runtime.

## Consequences

### Positive

- **One ledger, three projections.** Federation outbox + admin
  audit + notifications all consult the activities table; domain
  tables remain optimized for read; cache layer fronts the hot
  paths. Single source of truth.
- **Federation thinness.** The outbox dispatcher is a `SELECT` +
  sign + ship. No translation layer to maintain.
- **Idempotent by construction.** `activity_uri` UNIQUE means
  retries, replays, and peer redeliveries are safe.
- **Audit-grade history.** Every social action is queryable by
  actor, object, type, date. DSAR ("what did user X publish") +
  abuse investigation ("what came in from this peer") + per-
  object timeline are all one-query.
- **Replay tool is free.** Federation re-sync after a peer pair
  is a `./aa activities replay --actor=X --peer=Y` invocation
  that reads the ledger, signs, and ships.
- **Transactional emit prevents drift.** Same `pgx.Tx` for the
  domain write + the activity insert. Either both commit or both
  roll back. The "domain row exists but its activity is missing"
  failure mode is structurally impossible.
- **Future activity types are additive.** A new activity type
  (`aa:Job` for federated workers, per
  [[project_federated_remote_workers]]) is one entry in the
  `federation.ActivityType` const + one CHECK clause update +
  one emit helper. Schema unchanged.

### Negative

- **One extra INSERT per social action.** Like + activity = two
  rows. Acceptable — the activity insert is small, indexed, and
  on the same connection as the existing domain write.
- **Handlers grow a transaction boundary.** Handlers that
  previously called `pool.Exec(...)` directly need to wrap in
  `pool.Begin` / `tx.Commit`. Mechanical refactor, lands in
  1.22.A-bis-2.
- **Activity URIs are forever.** Once published to a peer, the
  activity URI is a permanent cross-instance reference. Changing
  the URI scheme breaks every federated reference. The URI shape
  is pinned in
  [docs/spec/federation/v1.md §8.1](/spec/federation/v1.md).
- **Notification fan-out moves inside emit.** The existing
  `notifications.Notify` call sites get refactored to fire from
  inside the emit helper rather than from the handler. Real
  refactor in 1.22.A-bis-2; the upside is that a new handler
  CAN'T forget to fire notifications (the emit helper does it).
- **No backfill at v1.** Historical rows are invisible to
  federation until the deferred backfill migration runs. New
  peers pair against the post-ADR-0044 era only.
- **Per-activity-type emit-helper proliferation.** One file per
  type in `activities/emit/`. ~10 files at v1, more as we add
  types. Acceptable — each is small and gives a single chokepoint
  for the type's wire-format invariants.

## Alternatives considered

- **Pure event-sourcing (Pattern 3).** Rejected per the Context
  section. Mastodon/Pixelfed/PeerTube/Lemmy all rejected it for
  the same reasons; the deployed-federation-systems evidence is
  strong.
- **Translator layer (Pattern 1).** Rejected per the Context
  section. The audit, replay, and idempotency stories are too
  weak.
- **Activity ledger as a side-channel without transactional
  emit.** Considered. Rejected because the drift failure mode
  (domain commit + activity insert fail) silently breaks
  federation correctness — the publisher believes it's published
  state changes that the ledger doesn't record, so peers never
  see them.
- **Per-domain emit (separate ledger tables per feature).**
  Considered (e.g. `like_activities`, `follow_activities`,
  `comment_activities`). Rejected because the cross-cutting
  queries (audit, federation outbox, per-actor timeline) all
  want one query path. Per-domain tables forces UNION ALL.

## Implementation

[Sub-phase breakdown documented in the Decision section, 1.22.A-bis-1
through 1.22.A-bis-3. Federation phases 1.22.B through 1.22.J
resume after 1.22.A-bis-3.]

The catalogue meta-index at `docs/catalogs.md` gains an entry
for `ActivityObjectKind` (owned by `app/internal/activities/`)
per [ADR 0042](/adr/0042-distributed-catalogs-typed-per-package/).

## References

- [ADR 0042 — Distributed catalogs](/adr/0042-distributed-catalogs-typed-per-package/)
- [ADR 0043 — Federation walled-garden protocol](/adr/0043-federation-walled-garden-protocol/)
- [docs/spec/federation/v1.md](/spec/federation/v1.md) §3 (envelope
  shape), §6 (addressing), §7 (custom activity types), §8 (URI
  shape).
- [ActivityPub W3C Recommendation §6](https://www.w3.org/TR/activitypub/#client-to-server-interactions)
  — the side-effects spec our emit helpers implement.
- Mastodon, PeerTube, Pixelfed, Lemmy source-tree analysis NOT
  performed (clean-room methodology, ADR 0040). The CQRS-lite
  pattern attribution rests on the spec text + their PUBLISHED
  architecture writeups, not source reading.
