---
id: "0052"
title: Optimistic-concurrency edit-safety on mutable entities
status: accepted
date: 2026-06-26
area: backend
phases:
  - "1.16"
supersedes: []
related:
  - "0044"
tags:
  - backend
  - api
  - concurrency
  - data-integrity
excerpt: >-
  Mutable entities (asset, collection, post) carry an `updated_at` revision token; PATCH endpoints require `If-Unmodified-Since` and reject stale writes with 409 Conflict. Lock-free, federation-safe, replaces the original "resource lock" design from the RS-gap audit.
---

## Status (2026-06-26)

Shipped via PR #167 (Phase 1.16 — edit-safety half) on 2026-06-27. Pattern applied to `PATCH /assets/{id}`, `PATCH /collections/{id}`, `PATCH /posts/{id}`. Frontend modals updated to capture baseline + surface 409 with explicit "overwrite" action. Search-half of Phase 1.16 (`/search` activation + facets + saved searches) remains for follow-up issue.

## Context

The RS-gap audit (2026-06-22) flagged "resource locking" as a P1 fundamental — concurrent edits on the same asset silently lose data today (last-write-wins). RS's solution (`resource.lock_user` + `save_resource_data` guard) is a coarse pessimistic lock taken when a user opens an edit modal and released on save/cancel/timeout. The pattern works but has real downsides:

1. **Stranded locks** when a user closes the tab without saving — needs a TTL + admin force-release UI, which RS implements with operator burden.
2. **Federation-hostile** — a federated update arriving from a peer would have to wait on or break a local user's edit lock; the semantics don't compose with the ArchivePub model where any peer can mutate.
3. **UI brittleness** — the "X is editing this" banner needs polling or a websocket; without it the lock surfaces as "save denied" with no explanation.

A purely optimistic approach using HTTP conditional-request headers (RFC 7232) carries the same safety guarantee with no server-side lock state, composes with federation, and matches modern API conventions (GitHub, Linear, Notion all use this shape).

## Decision

Every mutable entity carries `updated_at TIMESTAMPTZ NOT NULL` (already present on asset / collection / post per ADR 0044's audit-ledger pattern). PATCH endpoints require an `If-Unmodified-Since` header carrying the client's baseline `updated_at`; if the row's current `updated_at` is later than the header value, the server returns **409 Conflict** with a body shape:

```jsonc
{
  "error": "stale_write",
  "current_updated_at": "2026-06-26T14:22:31.485Z",
  "your_baseline": "2026-06-26T14:21:08.112Z",
  "current_state": { /* the full current entity, so client can diff */ }
}
```

The client can then either: (a) discard the user's edits, (b) re-prompt with the diff and let the user merge, or (c) explicitly overwrite by re-submitting with the new `If-Unmodified-Since` value.

### Header choice

`If-Unmodified-Since` over `If-Match` because:

- `updated_at` is already authoritative and human-readable in audit logs.
- `If-Match` requires generating + tracking an opaque ETag; needless complexity when the timestamp already serves.
- HTTP date parsing handles the format; no custom token vocabulary needed.

### Frontend pattern

Edit modals capture the baseline `updated_at` at modal-open time, send it on save, and surface the 409 with a single explicit action: **"This was changed by someone else. View their changes / Overwrite with mine."** No silent re-prompting; no automatic merge — the user always sees what changed.

### Federation interaction

A federated update arriving via inbox is treated exactly like a local update for the purposes of `updated_at` advancement. A local user mid-edit who tries to save after a federated update lands gets the 409 with the federated state visible — same shape as any other concurrent edit. No special federation casing.

### Scope (1.16 edit-safety half)

Applied to PATCH on:

- `asset` (per-field updates, sensitivity changes, tag set changes)
- `collection` (membership changes, metadata, theme)
- `post` (body, asset list reordering, state transitions)

NOT applied (out of scope for 1.16):

- Comment edits — comment threading already uses append-only semantics; no replace surface to protect
- Notification mark-as-read — single-bit toggle; race is benign
- User profile edits — single-user surface; concurrent-edit scenarios negligible

### What this is NOT

- **Not a resource lock.** No server-side state tracks "who's editing what."
- **Not a merge engine.** The 409 hands the user the conflict; the human decides.
- **Not federation-specific.** Same pattern protects single-instance concurrent edits AND inbound federation updates.
- **Not a replacement for batch-edit isolation.** Batch metadata edit (separate 1.16 sub-phase) needs per-asset atomicity but is orthogonal to the optimistic-concurrency surface.

## Consequences

**Positive:**

- Zero server-side lock state to maintain, time out, or force-release.
- Federation composes naturally — peer updates and local edits share one ordering primitive (`updated_at`).
- Familiar pattern; future API consumers (mobile, third-party tools, MCP clients) hit a standard HTTP convention.
- 409 response carries the full current state so the client can diff intelligently without a second round-trip.

**Negative:**

- Bandwidth cost on conflicts (full entity in 409 body). Mitigated by conflicts being rare in practice.
- Client must remember the baseline timestamp per edit session. Frontend convention handles this; third-party API users need to follow it explicitly.
- Sub-second concurrent edits hitting the same exact millisecond can both succeed if `updated_at` precision is too coarse. Postgres `TIMESTAMPTZ` resolves to microseconds — adequate.

## Alternatives considered

- **RS-style pessimistic lock (`lock_user` + TTL).** Rejected for the federation-composition and stranded-lock reasons above.
- **Opaque ETag (`If-Match`).** Rejected — `updated_at` already serves as the version token without additional bookkeeping.
- **Last-write-wins with audit-log diffing.** Rejected — surfaces silent data loss to the user only after the fact; the optimistic check catches it at the moment that matters.
- **CRDT-based merge.** Rejected — overkill for human-edit cadences on these entities; would require materially different data shape.

## Reference

- RFC 7232 (HTTP Conditional Requests) — the `If-Unmodified-Since` header semantics
- ADR 0044 — Activities ledger / CQRS-lite; `updated_at` advancement is the projection-side equivalent of the ledger's event timestamp
- Phase 1.16 — Search + edit-safety (this ADR covers the edit-safety half; search activation is a separate follow-up)
- RS-gap audit 2026-06-22 — original "resource locking" P1 finding
