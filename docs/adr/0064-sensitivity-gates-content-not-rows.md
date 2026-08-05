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

### Amended 2026-08-05 (#881, PR #913) — the workflow half shipped; the grant path is still deferred, and there is now a marker capability holding the seam open

**Do not "clean up" the capability `content.access.request`. It confers nothing on purpose.**

#881 shipped the request *workflow*: a "Request access" affordance on the restricted
placeholder, a notification to the asset's owner, and an **ownership disjunct** on the decide
gate so the artist whose work was requested can answer — previously they could not, unless an
operator handed them a global `share.grant`.

That disjunct nearly shipped as a privilege-escalation route, and the reason is the hazard this
very section already names. `resource_request.requested_capability` is **requester-controlled**;
`Grant` inserts it into `user_capability_grants` **verbatim and globally** (its own comment:
*"team_id is NULL (global grant) … the grant matches what the requester asked for without further
scoping"*). While only `share.grant` / `system.admin` holders could decide, that was contained —
those people are already trusted. **Widening the gate to every asset owner would have let anyone
submit `system.admin` against a stranger's asset and talk them into approving it from a panel
that looks like it is about a picture.**

So the owner disjunct is **capability-scoped**: an owner may decide only requests naming
`content.access.request` (migration 00035), and the submit endpoint defaults to that marker so
the UI never names a capability at all. An explicitly-named code is still accepted — the endpoint
predates the button and admin tooling asks for specific capabilities — but such a request remains
decidable only by a real approver.

**The marker confers nothing.** `ContentReadable` consults exactly two codes (`system.admin`,
`content.read.all`) and no gate anywhere reads this one. A granted request therefore means *the
owner agreed*, not *and now you can see it* — and the dialog says so in as many words rather than
leaving the user to discover it.

**The deferral below is therefore still in force**, and its stated blocker is only half gone: the
vocabulary problem was resolved by #434, but the **mechanism** — a grant that means *this object*
— does not exist, because `user_capability_grants` has no per-object scope at all (only
`team_id`). That is **#912**, which also carries the design question of whether ADR 0010 Layer 6's
additive ACL model is already the right shape rather than a new dimension on capabilities.

`content.access.request` is the seam between the two halves. When #912 lands, it is the thing that
should start meaning something — not the thing to delete.

### Why the grant path is deferred (amended 2026-07-19)

The obvious rule — "an approved `resource_request` unlocks the bytes" — is **unsafe as the
schema stands**, and the reason is not cosmetic:

- `resource_request.requested_capability` is `type: string` in the OpenAPI schema with **no enum
  and no pattern**, while `state` two lines below it carries a proper enum. There is **no
  capability vocabulary at all**: zero rows exist, zero distinct values have ever been stored,
  and the submit handler stores the field verbatim with no validation.
  **Partly resolved 2026-07-19 (#434, PR #440).** The field is now constrained by a foreign key
  to `capabilities(code)` (`ON DELETE RESTRICT`, migration 00009), and `Submit` rejects an
  unknown code with a 400 before the insert. A foreign key rather than an enum on purpose:
  `capabilities` *is* the registry every cap-seed migration writes to, so a hand-maintained list
  would be a second vocabulary that drifts.
  **This narrows the hole; it does not close it.** The field went from *any string the requester
  chooses* to *any valid capability code the requester chooses* — nothing stops a request naming
  `system.admin`. **The grant path therefore stays deferred**, and the remaining question is
  unchanged in kind: not "is this a real capability" but "is this one a user may ask for."
  A `capabilities.requestable` flag was considered for the same migration and **deliberately
  declined**: the column is the cheap part, and deciding which of ~60 capabilities carry the flag
  is the *same* decision the grant path is blocked on. Either default would assert something
  undecided — all-false breaks the requests surface, all-true grants the premise. It belongs in
  the change that can populate it correctly.
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

### Amendment: membership never widens (recorded 2026-08-04, #883)

"Sensitivity gates content, not rows" is a rule about an asset addressed **on its own**. It says
nothing about an asset reached **through a container** — a post's members, a collection's
contents — and the gap that left was live: `post_assets` joined the asset row with
`deleted_at IS NULL` and nothing else, so a **public** post carrying a **restricted** member handed
every caller, anonymous included, that member's title, description, file extension, byte size,
free-form `metadata` (EXIF, GPS) and thumbhash. Measured, not inferred — see the baseline recorded
on #883.

The rule for that path:

> **A member is readable iff the caller could have reached the asset standalone AND is entitled to
> its content tier.** Row plane ∧ content plane, for the same caller.

It lives in `visibility.MemberReadable`, and the three surfaces that expose a member (post
contents, collection contents, IIIF collection manifests) all route through it, on the same
argument as ADR 0063's predicate: one rule, one place.

Three things worth recording, because each is easy to get backwards:

- **It is a CONJUNCTION, so it can only ever be narrower than either plane.** That is what makes it
  compatible with this ADR rather than a revision of it. Nothing here changes what a *standalone*
  asset request returns, and nothing changes the browse feed. `assets.sensitivity` still gates
  content, not rows.

- **On the member path the content plane gates METADATA, not just bytes.** That does not follow
  from the rule above — it is the owner's decision (2026-08-03): *"if a user views a public post
  with a resource they don't have visibility for, it should show a placeholder... The placeholder
  should never leak info. Not even title. Only the owner's name."* The tier that gates the bytes
  therefore gates the columns too, on this path.

- **The placeholder is an anti-widening guarantee, not a secrecy guarantee.** An authenticated
  non-owner can still read that same title from `GET /assets/{id}` and from browse, because the
  authenticated branch of the row predicate is soft-delete only — the deferral this ADR records.
  For an ANONYMOUS caller, who cannot reach the row at all, it is both. The two converge if and
  when #210 / Phase 1.28 tightens the row plane; until then, do not describe the placeholder as
  making a title unreachable.

Two deliberate divergences:

- **Soft-delete is not in `MemberReadable`.** A deleted member is not restricted, it is gone;
  announcing "something was here" would be both untrue and its own small disclosure. The container
  queries drop those rows in SQL, and the matrix test asserts that division of labour by comparing
  against the predicate with `IncludeSoftDeleted()`, which waives exactly that one conjunct.

- **IIIF omits rather than placeholders.** Everywhere a person reads, a non-visible member renders
  as a visible placeholder — that is what makes "request access" (#881) meaningful, and why the
  member must not simply be dropped from the array. A IIIF Collection's `items` are
  dereferenceable manifest references, so a placeholder entry would be a link every conforming
  viewer follows and every one of them fails on. IIIF has no request-access affordance to make
  that worth anything.

A fourth surface was in scope and is not a serialization: a post's `search_text` folded in every
member asset's own document, so the **result count** confirmed a restricted member's title to a
caller who was never shown a field. Fixed in migration 00034, which also adds the trigger that
rebuilds a post's document when a member asset changes — absent since the baseline, and the reason
a renamed asset kept matching its old name in every post containing it.

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
