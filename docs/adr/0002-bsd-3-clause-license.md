---
id: "0002"
title: BSD-3-Clause license, mirroring ResourceSpace
status: superseded
date: 2026-05-23
area: licensing
phases: []
supersedes: []
superseded_by: "0016"
related: []
tags:
  - licensing
  - ai
excerpt: >-
  artist-alley is forked from ResourceSpace (RS), which is distributed under BSD-3-Clause. Our additions must coexist with that license; we cannot relicense the inherited RS code, and a mismatched license on new code would create a confusing two-license repository.
---
## Context

artist-alley is forked from ResourceSpace (RS), which is distributed under
BSD-3-Clause. Our additions must coexist with that license; we cannot
relicense the inherited RS code, and a mismatched license on new code
would create a confusing two-license repository.

We also have no monetization intent for artist-alley itself (optional paid
support may follow but does not require a copyleft license to enable).
The project should remain easy for game studios to self-host, modify, and
re-distribute internally without legal review friction.

## Decision

- The entire artist-alley repository is licensed **BSD-3-Clause**.
- Our additions are copyright `Kenneth and artist-alley contributors`.
- The ResourceSpace `license.txt` and `documentation/licenses/` directory
  are preserved unchanged so the upstream attribution remains intact.
- The root `LICENSE` file documents our copyright and explicitly references
  `license.txt` for ResourceSpace's notice.

## Consequences

**Positive**
- One license to reason about across the whole repository.
- Maximum permissiveness — studios can fork, modify, deploy internally,
  and never need to publish back. (No AGPL trap.)
- Aligned with the OSS norms in the DAM and game-tools ecosystem.

**Negative**
- A hyperscaler could in principle stand up a managed artist-alley service
  without contributing back. We have decided this risk is acceptable given
  no monetization intent.

## Alternatives considered

- **MIT** — equally permissive but requires a license change from the
  inherited RS code, creating dual-license confusion. Rejected.
- **AGPL-3.0** — would force any hosted derivative to publish source.
  Conflicts with inherited BSD code (we cannot relicense RS sources) and
  doesn't match our "easy adoption" goal. Rejected.
- **Apache-2.0** — explicit patent grant is nice; but a license change
  from BSD adds friction without practical benefit at this stage. Rejected.
