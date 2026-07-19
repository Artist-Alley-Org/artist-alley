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
- **`restricted`** — permitted to the owner and `system.admin` **only**. The grant path is
  **deferred** — see below.
- **`embargo`** — as `restricted`, and denies by default.
- **`embargo`** — as `restricted`. The date-based auto-lift and the per-asset allowlist are ADR 0020 Phase 1.28 machinery that does not exist yet; **until it does, embargo denies by default.** Failing closed on the strictest tier is the correct interim.

### Why the grant path is deferred (amended 2026-07-19)

The obvious rule — "an approved `resource_request` unlocks the bytes" — is **unsafe as the
schema stands**, and the reason is not cosmetic:

- `resource_request.requested_capability` is `type: string` in the OpenAPI schema with **no enum
  and no pattern**, while `state` two lines below it carries a proper enum. There is **no
  capability vocabulary at all**: zero rows exist, zero distinct values have ever been stored,
  and the submit handler stores the field verbatim with no validation.
- The field is **requester-controlled input**. If the content checker treats some string as
  authorising bytes, then any account — and self-registration exists — can submit a request
  carrying exactly that string. The only thing between that and the file is an administrator
  clicking *grant* on a free-text value **the requester chose**.

That converts a metadata field into a privilege token whose value the attacker selects, with an
uninformed approval step. Whether it is acceptable depends on how the request flow presents that
value to the approver — a design question about the request UX, not something a binary-plane fix
should settle by picking a constant.

**So v1 is owner + `system.admin` only.** This regresses nothing: today *no* grant unlocks bytes,
because nothing checks anything. A grant-less checker is strictly better than the status quo, and
fails closed by construction. The grant path lands once `requested_capability` has a defined
vocabulary enforced by a CHECK or enum — which is its own finding, tracked separately.

### Corollary: `CanSee` is asymmetric between assets and collections (recorded 2026-07-19)

A consequence of "sensitivity gates content, not rows" that is easy to get backwards, and did
nearly produce a wrong test during PR #439:

- **`CanSee(EntityAsset, <authenticated>)`** admits **any** authenticated caller to **any**
  non-deleted asset. The authenticated asset branch of the predicate is `deleted_at IS NULL`
  and nothing else — deliberately, per this ADR. Sensitivity is *not* consulted for rows.
- **`CanSee(EntityCollection, <authenticated>)`** is a genuine owner-or-ACL check.

So a "an authenticated non-owner is denied" test is **correct for collections and wrong for
assets** — on the asset side it asserts the opposite of what this ADR decides, and making it
pass would contradict ADR 0020's requirement that restricted assets stay listed. The asset-side
equivalent worth testing is *anonymous* denial, not non-owner denial.

Until the row-level story changes (Phase 1.28 blur-and-reveal, or #210), that asymmetry is the
design, not a gap.

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
- ~~Anonymous callers are unaffected — ADR 0063's anonymous predicate already restricts them to
  `active` + `public` + `ready`, and they cannot reach the binary handlers at all.~~
  **Superseded 2026-07-19 (#415 item 5, PR #437).** The second clause was true only while the
  binary handlers each rejected anonymous at the door, which public mode required removing.
  Anonymous callers now *do* reach the binary plane and receive bytes for `public`-tier assets.
  **This is the rule above applied unchanged, not a new decision** — "`public` — permitted to
  anyone entitled to see the row" already covered anonymous once ADR 0063 made them entitled to
  public rows. Two implementation notes worth recording, because both are load-bearing:
  - The `team` branch needs an *explicit* anonymous deny before the membership lookup. The
    anonymous sentinel is `user_ref` 0, so an unguarded lookup would query `WHERE user_ref = 0`
    and could match a real row. Denying at the tier is not sufficient on its own.
  - The ownership comparison must stay behind `!caller.IsAnonymous` for the same reason — an
    asset owned by ref 0 would otherwise match the sentinel. That guard predates this change
    (#436) and is what made relaxing the anonymous short-circuit a one-line edit.
- Listing behaviour is unchanged, so no splice site moves and no caller breaks.
- The grant path is real work reusing existing tables (`resource_request`, `team_memberships`)
  rather than new schema.
- ADR 0020's blur pipeline, reveal action, embargo dates and scheduled actions remain Phase
  1.28. This ADR is a strict subset that closes the leak without pre-empting that design.
