---
id: "0064"
title: Sensitivity gates content, not rows
status: accepted
date: 2026-07-19
area: security
phases: []
supersedes: []
related:
  - "0020"
  - "0063"
tags:
  - security
  - authorization
  - visibility
excerpt: >-
  Asset sensitivity is a content-access tier, not a row-exclusion tier.
  Restricted and embargoed assets remain listable; what is gated is the
  bytes. The enforcement point is the binary plane, which today has no
  check at all.
---

## Context

`assets.sensitivity` (`public | team | restricted | embargo`) has existed since the baseline
schema but is consumed only by the federation send/receive gates. Nothing reads it for
authorization, so **any authenticated caller can download any asset's bytes** — verified: the
binary handlers (`asset_file.go`, `hls.go`, `archive_entry.go`, `archive_bundle.go`,
`DownloadAssetFile`, `DownloadAssetVariant`) contain zero `visibility.` references and guard
only `identity != nil`.

[ADR 0063](0063-content-visibility-predicate.md) deferred "the authenticated sensitivity rule"
as an undecided product call. That framing was wrong, in an instructive way: the decision was
already made in [ADR 0020](0020-asset-gating-nda.md) and simply never implemented.

## Decision

**Sensitivity gates content, not rows.** These are two orthogonal axes, and conflating them is
the actual bug:

| Axis | Question | Governed by |
|---|---|---|
| **Row visibility** | may this caller know the asset exists, and see its metadata? | publication status, ownership, team, ACL — the ADR 0063 predicate |
| **Content access** | may this caller obtain the bytes? | `sensitivity` + grants — **this ADR** |

This follows from ADR 0020 rather than inventing a parallel model. That ADR specifies that
restricted and embargoed assets are **still listed**, with blurred thumbnails and a lock icon,
"including to users who have view capability", and that the blur is baked server-side "so even
a network-tap leak is blurred". A design in which such assets are *excluded from results*
contradicts it. What must never leak is the **content**.

### The content rule

For a caller requesting an asset's bytes (original or any derivative):

- **`public`** — permitted to anyone entitled to see the row.
- **`team`** — permitted to members of `assets.team_id` (via `team_memberships`), the owner, and `system.admin`.
- **`restricted`** — permitted to the owner, `system.admin`, or a caller holding an **active grant**: a `resource_request` row for this asset in an approved state whose `expires_at` is in the future. (`requests.default_expiry_days.restricted = 7` already exists.)
- **`embargo`** — as `restricted`. The date-based auto-lift and the per-asset allowlist are ADR 0020 Phase 1.28 machinery that does not exist yet; **until it does, embargo denies by default.** Failing closed on the strictest tier is the correct interim.

### Where it is enforced

At the **binary plane** — the handlers listed above — because that is where bytes are served and
where there is currently no check whatsoever. Not in the query predicate: pushing sensitivity
into row visibility would both contradict ADR 0020 and move behaviour at ~11 splice sites at
once, which ADR 0063 identifies as the high-blast-radius surface.

### What stays deferred, and why

Row-level sensitivity filtering for authenticated callers stays out — **not** because the rule
is undecided, but because ADR 0020 establishes it is the wrong instrument. The correct
row-level story is blur-and-reveal (Phase 1.28), not exclusion.

## Consequences

- The largest remaining visibility gap closes on the plane where it matters most: files rather
  than metadata. With self-registration available, "any authenticated caller" is a low bar.
- Anonymous callers are unaffected — ADR 0063's anonymous predicate already restricts them to
  `active` + `public` + `ready`, and they cannot reach the binary handlers at all.
- Listing behaviour is unchanged, so no splice site moves and no caller breaks.
- The grant path is real work reusing existing tables (`resource_request`, `team_memberships`)
  rather than new schema.
- ADR 0020's blur pipeline, reveal action, embargo dates and scheduled actions remain Phase
  1.28. This ADR is a strict subset that closes the leak without pre-empting that design.
