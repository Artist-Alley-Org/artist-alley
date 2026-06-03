---
id: "0029"
title: Announcements home widget — operator-authored news + system events
status: accepted
date: 2026-05-30
area: ux
phases: 
  - "1.30"
  - "1.37"
supersedes: []
related: 
  - "0022"
tags:
  - ux
  - ai
  - infrastructure
  - 3d
excerpt: >-
  RS ships a news plugin that puts an operator-authored news strip on the homepage. The audit (2026-05-30) initially marked this as low- priority. User locked it in 2026-05-30.
---
## Context

RS ships a `news` plugin that puts an operator-authored news strip on
the homepage. The audit (2026-05-30) initially marked this as low-
priority. User locked it in 2026-05-30.

Studios have things to announce: new contractor onboarding, NDA
expirations, "the Tuesday review moves to Wednesday this week," release
windows, system maintenance, festival deadlines. Without a first-class
surface, this gets posted in Slack and then disappears.

## Decision

Add Phase 1.37 — Announcements home widget — as a small, focused
surface for operator-authored announcements + automated system events.

### Author surface

Admin shell gets a `/admin/announcements` page:

- Markdown editor with the same preview surface used elsewhere.
- Audience scoping: `org` / `team:{name}` / `role:{name}`.
- Optional `nbf` / `exp` so announcements appear and disappear on a
  schedule without admin remembering to delete them.
- Pinned flag — pinned announcements stay above the fold until
  expired or unpinned.
- Severity: `info` / `notice` / `warning` / `critical` — affects
  the badge color and whether the user must click to dismiss.

### Display

- Homepage hero strip: at most three active announcements, scrolling
  if more.
- Account dashboard tile: full list of announcements relevant to
  the user, with read / unread state per user.

### Auto-fed system events

Some classes of system event become announcements automatically:

- A planned maintenance window scheduled by the operator (a system
  log event becomes an `info` announcement two days before, a
  `warning` an hour before).
- A license upgrade arriving (`info`).
- A failed scheduled job that needs operator attention
  (`warning`, scoped to `role:admin`).
- A federation peer going offline (`warning`).

### Read state

Per-user `(announcement_id, user_id)` row marks read state. The
homepage hero hides announcements the user has dismissed.

### Federation

Announcements DO NOT replicate across federated peers — each instance
runs its own news. Cross-instance announcements are a v2 if needed.

## Consequences

**Positive**

- A real, structured surface for operator communications that
  outlives Slack scrollback.
- The auto-fed-from-system-events part removes "did the operator
  remember to tell us about the maintenance window?" as a class
  of failure.

**Negative**

- Audience scoping interacts with the team membership model; an
  announcement targeted at a team that gets renamed needs a stable
  reference. Mitigation: store team_id, not team name.

## Alternatives considered

- **Just use email.** Doesn't capture read state, doesn't compose
  into a UI surface.
- **Just use Slack.** Studios use a dozen Slack workspaces; the
  message disappears. Slack notifications (ADR 0022) STILL fire
  on new announcements, but the announcement itself lives in
  Artist Alley.

## Reference

- Phase 1.37 in [`docs/roadmap.md`](../roadmap.md).
- Chat platform integrations (Phase 1.30) — Slack mirrors new
  announcements.
