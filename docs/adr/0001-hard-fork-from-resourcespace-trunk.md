---
id: "0001"
title: Hard fork from ResourceSpace trunk @ r28830
status: accepted
date: 2026-05-23
area: process
phases: []
supersedes: []
related: 
  - "0003"
tags:
  - process
  - ai
  - 3d
excerpt: >-
  artist-alley is a self-hosted art review and archival tool for game studios. ResourceSpace (RS) is a 20-year-old open-source DAM that already solves most of the asset-management substrate we need: permissions, resource types, metadata fields, preview pipelines, plugin architec…
---
## Context

artist-alley is a self-hosted art review and archival tool for game studios.
ResourceSpace (RS) is a 20-year-old open-source DAM that already solves most
of the asset-management substrate we need: permissions, resource types,
metadata fields, preview pipelines, plugin architecture, audit logs.

Building those substrate pieces from scratch as a solo developer would take
multiple years before reaching any of the *differentiated* features
artist-alley is meant to deliver (artist-first UX, modern review mode,
AI-native search, frame-accurate video review, 3D viewer).

We considered three approaches:

1. **Greenfield rewrite** — own every line. Rejected: scope is hostile to
   a solo dev and discards 20 years of mature edge-case handling.
2. **Consume RS via its API, never touch its source** — pure additive
   integration. Rejected: too constraining; RS's UI and some assumptions
   block the experience we want, and we can't iterate on RS internals when
   needed.
3. **Strangler Fig with upstream tracking** — keep RS pristine in `upstream/`,
   layer changes in an `overlay/`, rsync-merge to a build dir. Rejected after
   evaluation: the overhead of maintaining the overlay seam, plus the
   "track-upstream" discipline, costs more than it returns when we have no
   need to merge their future releases.

We chose a fourth path: **hard fork**. Take a snapshot of RS at a specific
revision, modify the code in place from that point forward, do not track
upstream.

## Decision

- Fork from `http://svn.resourcespace.com/svn/rs/trunk` at revision **r28830**
  (HEAD on 2026-05-23, last actual change r28820 / 2026-05-21).
- Trunk chosen over `releases/10.7` because we want pre-v11 features
  (e.g. ticket q12562's expanded System Configuration page, r28315 language
  refresh, and other trunk-only work) and we accept the lack of a version-
  string pin in exchange.
- RS files live at the repository root (the fork *is* the application).
- We add new top-level directories that are unambiguously ours:
  `services/`, `web/`, `infra/`, `scripts/`, `docs/`.
- We will not pull future RS upstream changes wholesale. We may, on a
  per-case basis, cherry-pick a specific upstream fix or feature into our
  fork using `scripts/rs-diff.sh` as a comparison aid.

## Consequences

**Positive**
- Day-1 working application: RS is a complete DAM out of the box.
- Total control over the codebase — we can replace any module on our
  schedule without working around upstream conventions.
- No merge-discipline overhead. No `overlay/` or `merge.sh` to maintain.
- Simpler repository layout (single tree, no submodules, no SVN tooling
  required for day-to-day work).

**Negative**
- We forgo automatic upstream security patches. We must monitor RS releases
  and cherry-pick anything critical.
- We are responsible for our own bug fixes from this point forward.
- When v11 ships as a stable release, our fork will have diverged from it
  and we cannot trivially "bump" to it.

**Mitigations**
- `scripts/rs-diff.sh` lets us diff our fork against RS trunk or any release
  tag, so we can identify upstream patches worth cherry-picking.
- Internal Strangler Fig pattern (ADR 0003) means we are progressively
  replacing RS PHP modules with our own services, gradually reducing the
  surface area where upstream patches would even apply.

## Alternatives considered

See "Context" above for the three alternatives we rejected.

## Notes

The clean-room boundary documented in the project memory (re: a separate
work project the author is aware of) is preserved: this fork inherits only
public BSD-3-Clause ResourceSpace code. No proprietary derivatives or
naming from any third-party project are carried over.
