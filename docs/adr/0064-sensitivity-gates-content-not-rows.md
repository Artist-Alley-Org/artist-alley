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

### Amendment 2026-08-09 (#931, PR #986) — a SECOND marker for restoration appeals, because the two deciders must not be interchangeable

The request machinery gained its second use: an owner whose item was deleted by someone else may
file a **restoration appeal** (`POST /account/trash/{kind}/{id}/request-restore` — one operation
over all three soft-deletable kinds; `resource_request` grew `target_kind` and its uuid column was
renamed `target_id` to stop being false for two of three values, migration 00042).

The appeal deliberately does **not** reuse `content.access.request`, and the reason is the decider
mapping, not tidiness:

| marker | who may decide it |
|---|---|
| `content.access.request` | the target's **owner** (plus the real approvers) |
| `content.restore.request` | whoever passes `auth.CanRestoreDeleted` — the **deleter**, or `system.admin`. Nobody else. |

The owner of a moderated item is precisely the person the moderation was against; one shared
marker would have let them approve their own appeal through the owner disjunct. And `share.grant`
— sufficient for access requests — is **excluded** here: authority over sharing is not authority
over moderation. The gate is an if/else on the marker, not an added disjunct, because the
pre-existing disjuncts short-circuit before `ownerMayDecide` runs (an owner holding `share.grant`
never reached it — immaterial for access requests, fatal for appeals; PR #986 caught this).

**Granting an appeal performs the restore and writes NO `user_capability_grants` row.** The #881
rule ("granting inserts the requested capability verbatim") now has a stated exception with a
mutation-tested assertion behind it: `count(*) FROM user_capability_grants` is unchanged across an
appeal grant. A granted appeal means *the item is back*, nothing more — same shape as the access
marker meaning *the owner agreed*, nothing more. `expires_at` on an appeal grant is a 400: a
performed restore cannot expire.

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

### Amendment 2026-08-06 (#922, PR #940) — the ATTACH rule has one home now

The membership amendment below governs what a container **shows**. There is a second, distinct
question — **what may a caller put INTO a container they control?** — and until now it had two
answers in two packages.

> **A caller may attach an asset iff they could have reached it standalone AND are entitled to its
> content tier** — the row plane and the content plane, conjoined, for the same caller.

That composition now lives in **`visibility.CanAttachAsset`**, beside the two planes it composes,
and both container packages call it through thin identity adapters (`collections.mayCollectAsset`,
`posts.mayAttachAsset`).

**Why it moved rather than being copied.** #882 built this inside `collections.Handler`. #922
needed the identical question on the post surface, and a second copy of a security rule is the
defect class epic #665 exists to remove — #892 and #904 each spent a sprint deleting one. So the
sprint that added the post gate also **retired the collection copy into the shared rule**. One
expression, two callers.

**It is a conjunction, so it can only ever narrow.** Nothing here changes what a *standalone* asset
request returns, and nothing changes any read path. It decides a **write**.

Two things worth recording because both were surprises:

- **The gate needed two call sites on posts, not one.** `CreatePost`'s members loop *and*
  `POST /posts/{id}/assets`, which previously gated the post via `canMutatePost` and never the
  asset. A create-only gate is not a gate.
- ⭐ **The gate CREATES indistinguishability; it does not preserve it.** #922 and its brief both
  described a `404 "asset not found"` as the existing behaviour for a bad member UUID. That path
  was **unreachable** for a single-member body: the cover defaults to `members[0]`, so the cover
  foreign key fires on the post INSERT first and surfaced as an unhandled **500**
  (`SQLSTATE 23503`). Refusing at the gate is what makes an unreadable asset and a nonexistent one
  answer alike — which is the property that closes the oracle.

~~**Still open**: the post's `cover_asset_id` / `cover_thumbnail_asset_id` are **not** routed through
this rule when supplied explicitly (**#941**).~~ **CLOSED — both columns now route through
`visibility.CanAttachAsset`.** `cover_asset_id` in **#941** (create and update), and
`cover_thumbnail_asset_id` in **#946 / PR #959** (`e4d699ee`).

⚠️ **They closed nearly three weeks apart, and the gap is the lesson.** #941 gated the column
everyone was looking at; the thumbnail sat beside it, declared in `openapi.yaml`, with a
`sqlc.narg` waiting for it, **never passed to `UpdatePostParams` at all** — so it answered 200 to
every write and changed nothing. A gate applied to one column of a pair is an invitation to wire
the other one ungated later. **When a rule covers a column, enumerate its siblings.**

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

It lives in `visibility.FieldsReadable`, and the three surfaces that expose a member (post
contents, collection contents, IIIF collection manifests) all route through it, on the same
argument as ADR 0063's predicate: one rule, one place.

*(Named `MemberReadable` when this amendment was written; renamed to `FieldsReadable` shortly
afterwards when #899 found the same leak on surfaces that are not "members" at all, making the
old name too narrow for what it decides. The rename is recorded in a comment at
`app/internal/visibility/fields.go:37`. Symbol updated here 2026-08-05 — the rule is unchanged.)*

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

  ✅ *Amended 2026-08-13 (#902, PR #1063) — **the sentence above was written as a limitation and is
  now, for the MATCH channel, closed.*** It said an authenticated non-owner *"can still read that
  same title from `GET /assets/{id}` and from browse"*, and treated the placeholder as
  anti-widening rather than secrecy. #902 was the sharper form of that limitation: the title was
  recoverable **word by word** through search, because `search_text` still contained it and `@@`
  still matched it — query a phrase only that title holds and the total moves 0→1, then walk the
  rest of it token by token.

  **Every full-text surface over `assets` now ANDs the field-plane rule onto the match**
  (`visibility.AssetSearchMatchSQL`, composed by `/search` hits, the `/search` COUNT and browse's
  `?q=`). A caller who fails `FieldsReadable` matches **none** of that asset's words.

  ⭐ **What this does NOT change, and the distinction matters:** an **unfiltered** browse still
  lists the row as a placeholder, exactly as this ADR requires. The row did not become invisible —
  it stopped **answering questions about text it does not expose.** The absence is
  value-independent (it matches no query, for every query equally), which is the same reasoning
  ADR 0056 §4b uses and the reason this is not a new oracle.

  So "the row stays listed" now carries two qualifications, and both are deliberate: a filtered
  search excludes it (§4b), and a text query no longer matches it (this amendment). Neither
  removes it from the unfiltered listing that "request access" (#881) attaches to.

  ⚠️ *Amended 2026-08-12 (#907, PR #1055) — **"the row stays listed" is now conditional, and a
  reader of this section must know where.*** Search grew facet **filtering**, and under an
  **active filter** an asset the caller cannot open is **excluded** from the result set rather
  than listed as a placeholder. Unfiltered search, `GET /assets/{id}`, browse and collection
  contents are all unchanged — the guarantee above holds everywhere a caller asks an open
  question. A *filter* is not an open question: `extension:png` asks about a specific withheld
  field, and answering it would return exactly what this ADR removes from the payload. The
  exclusion is **value-independent** (gated on whether any filter is present, never on which),
  so a withheld row answers nothing for every value and its absence is not an oracle. The full
  argument — and its cost, a team-scoped `assets.admin` holder being narrower under a filter than
  unfiltered, tracked as **#1056** — is in **ADR 0056 §4b**.

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

### Amendment: a display preference may only subtract (recorded 2026-08-05, #891, PR #919)

Everything above is about what a caller **may** see. #891 introduced the first thing that
changes what a caller **is shown** without changing what they may see: ~~an account preference
(`user_preferences.feed_filters.hide_restricted`) that drops~~ **(amended 2026-08-05, #921 —
the key is now `feed_filters.show_restricted` and hiding is the DEFAULT rather than an opt-in;
see the next amendment)** a browse feed that drops restricted members — and
all-restricted posts — instead of rendering the #883 placeholders.

A preference that removes rows from a visibility-filtered feed is close enough to the read rule
to be dangerous, so the boundary is worth stating rather than leaving to be inferred from the
one implementation:

> **A display preference may only ever SUBTRACT from what the read rule already returned. It
> must compose with the single readability evaluation rather than re-derive it, and it must not
> reach the detail path.**

Three clauses, each load-bearing:

- **Subtractive only.** The filter reads one already-computed field, `PostMember.Restricted`,
  and cannot widen: a restricted member carries no `asset` payload at all — it was withheld
  upstream, not blanked — so the most the preference can do is decline to render a placeholder.
  Turning it on can remove things from a response and can never add one. A filter that could add
  a row would be a second expression of the read rule.

- **Compose, do not re-derive.** `Restricted` is written in exactly one place, `enrichPreview`,
  off the same `visibility.FieldsReadable` call that decides `preview_available`,
  `ladder_available` and `scrub_available`. The filter is therefore *incapable* of disagreeing
  with the rule, because it does not know the rule. This is the defect class epic #665 exists
  for, and #892 and #904 each spent a sprint deleting one instance of it.

- **The feed only — NOT the detail path.** `applyHideRestricted` has exactly one call site, in
  `ListPosts`. This is the clause most likely to be "cleaned up" by someone who reads the
  preference as a global user setting and notices it is applied inconsistently, so the reason is
  recorded here: extending it to `GetPost` was **tried during implementation and reverted**,
  because it reproduces the outcome the design explicitly rejected. An all-restricted post that
  drops out of the feed does so precisely because an empty card is worse than a placeholder;
  applying the same filter on the post page puts that empty card back on the one screen the rule
  cannot reach, and takes #881/#913's **Request access** button with it. The preference's help
  text states that trade-off to the user rather than leaving it to be discovered.

Two corollaries recorded because both are real off-by-ones:

- **"No visible members" is not "no members."** A post with no members at all — an article,
  ADR 0073 — was never showing the caller something withheld, so there is nothing to hide.
  Only a post that *had* members and now shows none is dropped.

- **A caller's own post is never dropped.** A post can carry other people's restricted assets,
  so its author can be exactly the person who cannot read them. The rule above applied literally
  would delete an author's own work from their own feed over a display setting. Their members
  are still filtered — that is about what you can see, and does not change because you wrote the
  caption — but the post stays.

### Amendment: hiding is the DEFAULT, and the key is renamed to say so (recorded 2026-08-05, #921, PR #924)

The amendment above describes hiding as an **opt-in**. It is now the **default**, and the rest of
that amendment — the subtractive-only clause, the compose-do-not-re-derive clause, the
feed-only clause, and both corollaries — is **unchanged and still binding**. Only the default
moved.

**Why.** #891 shipped the machinery on the theory that the placeholder is the more informative
answer. It was measured on the stock seed dataset and it was not: `olaf.lindgren` (ref 23, zero
capabilities) got **82 posts of which 27 were entirely placeholders — a third of the grid**. An
instance where teams genuinely share within themselves is worse. The owner's framing
(2026-08-05): *"We shouldn't show the restricted file in the regular feed… it would only show in
reposted assets and stuff in collections. That way they can still see the collection and that
there are restricted files in there without flooding the regular feed."*

The principle the default now encodes, which is also the rule for every future surface:

> **A placeholder belongs where the user ASKED A QUESTION or OPENED A CONTAINER. Not where they
> were handed a feed.**

That is what makes the three surfaces consistent rather than inconsistent: `GET /posts/{id}` is a
question, a collection's contents are an opened container, and the browse feed is neither.

**What did NOT change — and this is the whole point.** The read rule is untouched. `ListPosts`
still *receives* every row the rule returns; `applyHideRestricted` subtracts afterwards off one
already-computed field. Two layers, and only the lower one moved:

| layer | before #921 | after #921 | changed? |
|---|---|---|---|
| the access **rule** | does not exclude rows; sensitivity gates content, not rows | identical | **NO** |
| the default **presentation** | renders every row the rule returned | subtracts restricted ones in the feed | **YES** |

ADR 0020's *"the row still exists in every feed"* is narrowed to match — see its 2026-08-05
amendment. Nothing about who may read what moves; what moves is what the feed chooses to draw.

**Inverted the KEY, did not flip the default.** `feed_filters.hide_restricted` became
`feed_filters.show_restricted`, still defaulting to `false`. Migration 00036's storage guarantee
is written around *an absent key means the build's default*, and defaulting a key called
`hide_restricted` to `true` would have left an absent key asserting the reverse of its own name.
The rule for the next key in that column: **name it so `false` is what the build does by
default.** Pre-release, no compatibility shim.

**The nil/error seam inverted with it.** `posts.showRestricted` answers `false` on a nil seam or
a failed prefs lookup — the same literal as before, the opposite behaviour, and correct for the
same reason both times: **both seams fail to the build's default.** Failing toward "show
everything" would now mean a prefs blip repaints an affected reader's feed as the wall of locked
doors this amendment exists to remove. Neither direction can leak: the redaction already ran in
`enrichPreview`, and a restricted member carries no `asset` payload at all.

**A forward fork, recorded so it is not rediscovered.** ADR 0020 specifies server-baked
**blurred** thumbnails with a lock icon for restricted assets in Phase 1.28. A blurred tile is a
genuinely different proposition from a "you cannot have this" placeholder — it shows the shape of
the work rather than only its absence. Whether hiding-by-default is still right once blur-and-reveal
lands is **an open question, deliberately not resolved here**. Revisit this amendment when 1.28 is
scoped.

**Consequence for the browse page.** #891 shipped a one-line note on `/` — *"items you don't have
access to are hidden by your preferences"* with a link to change it — because an opted-in
reader's grid was shorter than everyone else's. Under the default both halves became false (the
reader set no preference, and the feed is not shorter than anyone else's), and it would have
fired on every browse paint for every reader. It was removed in PR #924. A "there is more here
you cannot open" affordance, if wanted, is a fresh design question rather than that note inverted.

### ✅ DECIDED 2026-08-06 (#939) — a mutation capability confers the FIELD plane, never the BYTES

*(The seam recorded below was opened the same day and is now closed. Owner delegated the decision;
a prior-art pass supplied a third option neither of the framings below had considered.)*

**The question as originally posed — "does mutation imply readability?" — is malformed**, because
there is no single "readability" to imply. Mainstream products in this space treat
**view / search / download / edit as independent rights**: display-and-search restriction is
configured separately from download restriction, and metadata-field visibility is its own control.

**This ADR already splits at exactly that seam**, which is why the third option fits so cleanly:

| plane | mechanism | question |
|---|---|---|
| field | `visibility.FieldsReadable` | may they see title / description / tags? |
| binary | `CanReadContent` + the binary handlers | may they obtain the bytes? |

**Decision: a capability that permits mutation confers FIELD-plane readability for the objects it
governs. It never confers the binary plane.**

So a team-scoped `assets.admin` holder sees the title they are editing, and still cannot download
a `restricted` asset. That removes the absurdity — nobody deletes a thing they were never shown —
**without** turning an ADR 0010 capability grant into a content-tier grant, which is what
"mutation implies full readability" would have done and what every amendment in this ADR has
avoided.

Note the thumbnail lands on the **binary** side: the thumbhash is withheld precisely because
⭐ *Amended 2026-08-13 (#1066, PR #1068) — **the same reasoning reaches the EMBEDDING, and it took
until now to apply it.*** If a thumbhash is withheld because it is a low-fidelity copy of the image,
then a **768-dimension CLIP embedding is the same kind of thing** — lossier, but content-bearing,
and a similarity *score* exposes it a little at a time. Until #1066 the vector path gated on
`visibility.Filter(EntityAsset)`, which for an authenticated caller is **soft-delete only**, so
similarity search ranked restricted assets: supply a candidate image, watch one rank, and learn
that an asset whose picture you are refused resembles it. All three entry points
(`POST /search/by-image`, the `similar_to:` hybrid path, and `GET /assets/{id}/similar`) now gate
on `ContentReadableSQL` — the picture plane, deliberately stricter than #902's field plane, because
an embedding derives from the image and not the metadata.

⚠️ **And the gate has to run in BOTH directions.** Every one of those surfaces also leaked on the
**anchor** side — you could anchor on an asset you cannot read and harvest its neighbourhood. All
four anchor gates already carried a comment claiming to prevent exactly that; the row predicate
could not deliver it. **When gating a derived copy, gate what the query returns AND what it may be
anchored on.** Refusals are `404`, indistinguishable from "not embedded".

**The full list of derived copies is now closed:** `search_text` (#902), the facet buckets, the
`thumbhash` (this ADR), and the embedding (#1066). A fifth would be new work; the general rule —
*a withheld value has derived copies, and every copy must be withheld* — is recorded in ADR 0020's
three-channels amendment.

*"a thumbhash IS a blur"* (see the amendment above). So the result is a **richer placeholder** —
real fields, no picture — rather than a blank card. That also discharges the UI obligation the
orthogonal option would have carried, because the interface stops looking broken on its own.

#### ⛔ The implementation constraint that comes with it — applies regardless

Write-without-read is a legitimate, long-established pattern (file permissions exist to allow
modification without disclosure; the formal term for the operation is a *blind write*). But there
is a documented leak class: a system that ignores the **interaction** between read and write
privileges lets a caller submit writes evaluated against source data rather than only against data
they may see — **which lets them learn about data they cannot read**.

Applied here: validation errors, conflict responses and the returned representation are all
oracles. *"Title must differ from current"* discloses the current title to someone the read rule
would refuse.

> **A mutation response must never disclose more than the read plane would have.**

That holds under this decision and would have held under either alternative. It is the part most
likely to be missed, because it lives in error paths rather than in the gate.

#### As implemented (#939, 2026-08-06) — the field plane needed a THIRD predicate

Implementing the decision surfaced something the decision text assumes but the code did not
provide: **there was no "picture" plane to withhold**. The thumbhash and the three availability
flags (`preview_available`, `ladder_available`, `scrub_available`) were all derived from the single
`FieldsReadable` boolean at every surface. Widening that one function — the whole of the change as
originally scoped — would therefore have shipped the blur *and* flipped the flags true for exactly
the caller the decision refuses, at the browse list, search, post previews, collection contents and
`GET /assets/{id}` alike. "The thumbhash stays withheld" is not a property that survives on its
own; it had to be built.

So the conjunction was split in two:

| plane | mechanism | question | mutation cap? |
|---|---|---|---|
| field | `visibility.FieldsReadable` | title / description / tags / metadata / hash / size / dims | **yes** |
| picture | `visibility.PreviewReadable` | the thumbhash blur + the three availability flags | **no** |
| binary | `CanReadContent` + the binary handlers | the bytes themselves | **no** |

`PreviewReadable` is the *pre-#939* `FieldsReadable`, unchanged; `FieldsReadable` is now
`PreviewReadable OR CallerMayMutate`. Every surface makes two decisions from one row. The flags sit
on the picture plane for a second reason beyond the blur: they are a promise the binary handlers
must keep, and a `true` flag on gated bytes is a 403 the client walks straight into.

`ContentReadable` and its SQL twin `ContentReadableSQL` are **untouched**, which is the structural
check that the disjunct landed on the right plane — had it gone into `ContentReadable`, keeping
`TestContentReadableSQL_MatchesGo` green would have required transcribing it into the SQL twin, and
the bytes would have moved.

##### Amendment 2026-08-13 (#1026, PR #1069) — the PICTURE plane gained a SQL twin, by extraction rather than transcription

The table above gave the picture plane a Go form only. #1026's collection cover mosaic is the
first surface that cannot use it: the mosaic shows the first four members that produce a picture,
and **a member the caller may not see must be skipped rather than occupy a slot**. That makes the
readability decision determine *which rows the query returns at all*, so deciding it in Go would
mean fetching an unbounded prefix of the membership per collection and filtering it down, on a hub
page that renders fifty. In SQL it is a `ROW_NUMBER` over the already-filtered set with `LIMIT 4` —
exact, with no candidate cap to be wrong about.

So `visibility.PreviewReadableSQL` now joins `ContentReadableSQL`, `FieldsReadableSQL`,
`AssetSearchMatchSQL` and `OwnerDisplayNameSQL` in the twin family, held to its Go form by
`TestPreviewReadableSQL_MatchesGo`.

⭐ **What makes it a twin rather than a fifth expression of the rule: it was EXTRACTED, not
written.** `FieldsReadableSQL` already rendered the picture-plane fragment inline as its first
disjunct; that fragment is now a named `previewReadableExpr` that **both** call. With an empty
mutation scope the two fragments are textually identical, and a test asserts exactly that — the
cheapest available proof that this is the same plane and not a parallel one.

⛔ **Do not reach for `FieldsReadableSQL` with a zero `AssetMutationCaps` to get this.** It renders
the same text *today*. The day a non-mutation disjunct is added there, the cover mosaic would
silently begin handing out pictures this ADR withholds. `mut` is deliberately absent from
`PreviewReadableSQL`'s signature rather than ignored: §"a mutation capability confers the FIELD
plane, never the BYTES" means there is no value a caller could pass that should change this answer,
and a parameter would invite one.

**One implementation note, because it cost a crash.** The fragment short-circuits to the empty
string for `system.admin` / `content.read.all` — the callers who see everything — which means the
caller-ref placeholder it would otherwise have named goes unbound, and Postgres fails the whole
statement with `42P18` for exactly the two capabilities meant to be unrestricted. The composer
renders the ref as an `int64` literal instead. (A bound tautology is the other way out and is what
`search`'s COUNT does; here it would cost a placeholder that means nothing plus a comment on both
halves explaining why deleting either breaks the other.) **The general shape is worth remembering:
a gate that compiles to nothing for privileged callers puts those callers on a code path no
ordinary test exercises.**

**The capability is resolved in Go, not re-derived in SQL.** Computing "does this caller hold
`assets.admin` over this row's team" as an `EXISTS` against `user_capability_grants` in the SELECT
list looks cheaper and is wrong: `auth.EffectiveScopedCapabilitiesForUser` resolves a scoped
capability from **four** inputs — grants, `role_capabilities` reached through a recursive walk of
`roles.parent_id` carrying the `user_roles.team_id` that seeded it, `user_capability_revokes`
subtracted at the exact `(code, team_id)` pair NULLs-not-distinct, and only then the `team_closure`
fan-out. A grants-only expression misses every capability conferred through a **team-scoped role
assignment**, which is the ordinary way an operator confers one — silently, and in the direction
that half-ships the feature. The resolver's answer is read out of `Identity` instead
(`Identity.ScopedTeams`), carried as `visibility.AssetMutationCaps`, and matched against a `team_id`
column the field fragment now selects as raw data.

**`ContentCaps` stays two booleans and its `CacheKey` stays two bytes.** The mutation scope is a
third resolved struct rather than more fields on it, because this capability is team-scoped and its
honest resolved form is a *set*.

**The cache-staleness seam is closed rather than matched.** `is_team_member` has long had the
property that leaving a team does not evict a cached search result, and matching that precedent was
the cheap option. It was rejected because closing it costs nothing: `keyForQuery` already includes
the caller's `user_ref`, so folding per-caller capability state in causes **zero** cross-caller
cache fragmentation — it invalidates only that caller's entries, only when that caller's own grants
change. Left open, a **revoked** holder would keep being served the cached titles, descriptions and
tags of restricted assets for the rest of the TTL. The `is_team_member` instance remains open and
is recorded in `AssetMutationCaps.CacheKey` so the next sweep finds it already known.

On the oracle constraint: `UpdateAsset` returns its representation through `enrichAssetDerived`,
the same path `GET /assets/{id}` uses, so a write response is held to the read plane by
construction rather than by a second rule. There is no "title must differ" validation to leak
through, and the 409 conflict body carries only `updated_at`, which is reachable past
`canMutateAsset` and now readable anyway.

#### Historical — the seam as first recorded

### Open seam 2026-08-06 (#930, PR #936) — a mutation capability does not confer readability

`assets.admin` (ADR 0010 Layer 5, amended the same day) lets a team-scoped holder edit, delete and
restore a colleague's asset. **It grants no read access.** `visibility.FieldsReadable` does not
consult it, so such a holder can edit an asset's title and still be shown the withheld placeholder
when they open it.

**That is this ADR's model working as designed** — sensitivity gates *content*, and a mutation
capability is not a content-tier grant. Wiring it into `FieldsReadable` would make an
administrative grant a **read-widening** act, which is the coupling every amendment here has
avoided.

**It is also visibly odd**, and it is recorded as open rather than settled because the two answers
are different products:

- **Mutation implies readability** — intuitive, removes "I deleted a thing I was never shown", but
  granting a team lead the tidy-up right silently clears them for every restricted asset in their
  team.
- **They stay orthogonal** — an art director may reorganise a library including work they are not
  cleared to view, which is a real requirement where embargoed material exists. But then the
  interface owes that person an explanation, or the placeholder reads as a bug forever.

Tracked as **#939**. Whoever resolves it should record the reasoning here — the failure mode is a
future reader noticing the seam and "fixing" it in whichever direction they happen to prefer,
which is how a content-gating spine gets quietly repealed.

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
