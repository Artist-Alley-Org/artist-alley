# ADR 0020: Asset gating & NDA workflow — pre-release blur, scheduled actions

- Date: 2026-05-30
- Status: Accepted

## Context

Game studios constantly handle pre-announcement material that must NOT
be visible outside a small approved audience until a marketing date.
The patterns are universal:

- A character design lands six months before reveal. The whole studio
  shouldn't see it casually — but the art director and lead artist
  need to review it.
- A trailer cut goes out under NDA to a publisher; on the announcement
  date, restrictions auto-lift.
- A contractor's NDA expires; their access to all their referenced
  assets must restrict automatically without an admin remembering.

RS supplies two relevant plugins: `sensitive_images` (blur thumbnails
until permitted to view) and `action_dates` (scheduled actions on
resources — delete, restrict, archive at a future date). Neither is
optional for a studio with any meaningful IP discipline.

## Decision

Add Phase 1.28 — Asset gating & NDA workflow — combining sensitive-
asset gating with a generic scheduled-action engine.

### Sensitive asset gating

- Every asset has a `sensitivity` tier: `public`, `team`, `restricted`,
  `embargo`. Default per resource type is configurable.
- `restricted` and `embargo` assets show **blurred** thumbnails + a
  lock icon in browse views — including to users who have view
  capability. The blur is server-side baked into a special preview
  variant so even a network-tap leak is blurred.
- A "Reveal" button on the asset removes the blur for the session;
  the reveal is logged.
- `embargo` assets are visible to a configurable list of users / roles
  / teams only; everyone else sees a "this content is embargoed until
  YYYY-MM-DD" placeholder card.
- Reviewers can comment + annotate on blurred content; the annotation
  overlay is rendered against the unblurred source for users who can
  reveal.

### Scheduled actions

A generic `scheduled_actions` table queues actions to run at a future
timestamp. Action shape:

```jsonc
{
  "id": "sa_abc123",
  "target": { "type": "asset|post|collection|user", "id": 123 },
  "action": "restrict|delete|change_state|change_sensitivity|notify",
  "params": { "to_state": "archived", "reason": "NDA expiry" },
  "scheduled_for": "2026-12-01T00:00:00Z",
  "created_by": 42,
  "created_at": "2026-05-30T12:00:00Z",
  "status": "pending|done|failed|cancelled",
  "executed_at": null,
  "trail": []
}
```

A daily job picks up pending actions whose `scheduled_for` is past,
executes them through the existing job queue, logs the trail.

### Common scheduled-action recipes

- **NDA expiry on contractor:** `change_sensitivity` of every asset
  the contractor uploaded to `team` on the NDA end date.
- **Reveal-on-announcement-date:** `change_sensitivity` from `embargo`
  → `public` at a marketing-team-supplied timestamp.
- **Auto-archive stale builds:** `change_state` to `archived` 90 days
  after a build is superseded.
- **Auto-delete trash:** `delete` 30 days after a soft delete (this is
  the same engine as Phase 1.27 bulk delete's trash retention).

### Discoverability

A "Scheduled actions" admin surface lists every pending action by
target, owner, and date. Filterable, cancellable, bulk-cancellable.

### Sensitivity vs license tier

The license tier does NOT bound sensitivity — Community users get the
same blur + embargo features as Enterprise. The Enterprise audit log
export does include scheduled-action history as part of compliance.

## Consequences

**Positive**

- Two patterns studios already write themselves with admin scripts +
  cron, now first-class. Replaces ~half of every studio's RS plugin
  set.
- The scheduled-action engine is reusable for trash retention,
  notification delivery, federation outbox flushes — one engine,
  many consumers.
- Sensitive-content blur is server-baked so it survives client-side
  inspection / network sniffing.

**Negative**

- A blurred preview is an additional preview variant per asset, +
  storage cost. Mitigation: only mint the blur variant for assets
  whose sensitivity is `restricted` or `embargo`; refresh on
  sensitivity change.
- The scheduled-action engine runs at daily granularity by default.
  Sub-day precision needs a tighter cron; configurable.

## Alternatives considered

- **Blur client-side via CSS only.** Trivially defeated by devtools.
  Rejected — gives a false sense of security.
- **Use existing workflow_states as the only sensitivity primitive.**
  Conflates two concerns (workflow vs visibility). Rejected — they
  evolve independently and an asset can be `approved`-state but still
  `embargo`-sensitivity.
- **No scheduled-action engine — admins remember to act.** Real-world
  this fails routinely; the cost of forgetting an NDA expiry is high.
  Rejected.

## Reference

- Phase 1.28 in [`docs/roadmap.md`](../roadmap.md).
- Job queue (Phase 1.18.A — shipped) executes scheduled actions.
- Audit log (Phase 1.17 + 1.20.A) records sensitivity changes + reveal
  events.
