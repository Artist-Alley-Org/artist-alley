---
id: "0090"
title: Mature content is a second axis — a rating is not a clearance
status: accepted
date: 2026-08-16
area: security
phases: []
supersedes: []
related:
  - "0020"
  - "0063"
  - "0064"
tags:
  - security
  - authorization
  - visibility
  - content
excerpt: >-
  `sensitivity` answers "who is ALLOWED to see this"; `mature` answers "who has
  OPTED IN to seeing it". They are orthogonal — a public artwork can be mature
  and a restricted one need not be — so mature gets its own column, its own
  qualification predicate, and its own composition point on each plane. It
  reuses ADR 0020's display machinery rather than inventing a second one.
---

# Mature content is a second axis — a rating is not a clearance

## Context

Epic #1114 asks for operator-controlled mature/NSFW content: an instance toggle,
a per-viewer opt-in, mature work hidden from the feed of anyone who has not
opted in, and blurred previews with a blocked open where the checks fail.

Every one of those mechanisms already exists on this codebase for a *different*
question. [ADR 0020](0020-asset-gating-nda.md) specifies the blurred thumbnail
and the lock; [ADR 0064](0064-sensitivity-gates-content-not-rows.md) splits an
asset into a row plane and a content plane and puts `sensitivity` on the
content one; #921 established the "the rule returns the row; the feed doesn't
draw it" pattern for subtracting placeholders from a feed. The temptation — and
the reason this ADR exists before any of #1115's code — is to reach for the
nearest of those and extend it.

**That temptation is the bug.** `sensitivity` has four tiers and an obvious
empty slot at the bottom of the ladder, and "add `mature` as a fifth tier" is a
one-line change that would be wrong in a way no test would catch for months.

## Decision

### 1. Rating ⊥ clearance. Two axes, never conflated.

| Axis | Question | Column | Governed by |
|---|---|---|---|
| **Clearance** | who is ALLOWED to see this? | `assets.sensitivity` | ADR 0020 / 0064 |
| **Rating** | who has OPTED IN to seeing it? | `assets.mature` | this ADR |

They are independent in both directions, and both directions have real
instances:

- **A public artwork can be mature.** An artist posts adult work publicly; they
  are not restricting *who may* see it, they are labelling *what it is*. A
  design that expressed "mature" as a sensitivity tier would silently make
  every mature piece non-public, which changes federation behaviour, changes
  the content gate, and takes the artist's actual choice away from them.
- **A restricted asset need not be mature.** An unreleased game texture is
  restricted and rated nothing at all.

A single ordered ladder cannot express a product of two independent values, and
`sensitivity` is an ordered ladder — `ContentReadable`'s switch reads it as one.
So `mature` is a **boolean column beside it**, not a value inside it.

The consequence to hold on to: **the two are ANDed at read time and never
merged at write time.** A viewer sees an item when they are entitled to it
*and* qualified for it. There is no combined "effective tier" anywhere, because
the moment one exists someone will sort by it.

### 2. The qualification predicate

> A viewer QUALIFIES for mature content iff they are **signed in** AND have
> **opted in** AND the **instance allows** mature content.

Three conjuncts, all required, evaluated in that order for cheapness. Notes on
each, because each was a decision:

- **Signed in.** An anonymous viewer can never opt in — there is nowhere to
  store the answer and no one to hold to it. This is not a limitation to be
  worked around with a cookie: an instance-wide "show mature to anonymous
  visitors" switch is a *different* product decision (it is the operator
  answering for people who have not answered), and it is not in this arc.
- **Opted in.** The signed-in default is **hidden**, per the owner. The
  preference is a boolean on `user_preferences`, and its ZERO VALUE is the
  safe answer — a user with no preferences row, an empty blob, or a key this
  build has never heard of is *not* opted in. That is the same naming contract
  `FeedFilters` documents, and it is why the field is named for the permissive
  direction (`show_mature`) rather than the restrictive one.
- **The instance allows.** `system_config` gates the whole feature. With it
  off, nobody qualifies — *including* an opted-in user, because the operator's
  answer is about the install and outranks a reader's preference about
  themselves.

**Owner and admin are exempt** from disqualification, and this is a deliberate
asymmetry rather than a convenience. An artist must be able to see their own
work; if turning the instance toggle off made an artist's own uploads
invisible to them, the operator would have destroyed access to content the
artist owns by flipping a display switch. Same for `system.admin`, who has to
be able to moderate what the toggle hid.

### 3. Which plane each check composes

ADR 0064's two planes both get a check, and they get **different** ones. This
is the part a "just filter it out" implementation gets wrong.

| Plane | What mature does there | Pattern it reuses |
|---|---|---|
| **Row** | the browse feed does not RETURN a disqualified viewer's mature posts | #921 — the rule returns the row, the feed doesn't draw it |
| **Content / picture** | an addressed mature item renders BLURRED and its bytes are refused | ADR 0020's existing blur + lock machinery |

Why both, and why not one:

- **Row-plane only** would mean a mature item you reach by *name* — a deep
  link, a collection you opened, a post someone sent you — renders in full.
  The feed is not the only way to an item.
- **Content-plane only** would mean a disqualified viewer's feed fills with
  blurred plates they never asked to be offered. #921 measured exactly this
  shape for restricted placeholders (82 posts, 27 of them entirely
  placeholders) and the owner's answer was to subtract them from the feed. The
  same answer applies here for the same reason.

The #921 line, restated for this axis: **a placeholder belongs where the user
asked a question or opened a container, not where they were handed a feed.**

### 4. The post value is DERIVED, and maintained as such

`mature` lives on **assets**. A post is mature iff **any** member asset is.

That is a derived value, and #902 is the ADR-adjacent lesson about derived
copies: it must be **maintained at write time**, never recomputed ad hoc per
reader. Recomputing per reader means an `EXISTS` subquery spliced into every
post predicate — a correlated subquery on the feed's hot path, and worse, a
second expression of the rule that can disagree with the first.

The maintenance point is a **database trigger** on `post_assets` (and on
`assets.mature` itself), not a Go write-path hook, and the reason is
enumerable: `post_assets` is written from post create, post update, the
add-asset endpoint, and the seeder — and `assets.mature` will be written from
upload, from the edit form, and from the operator override. A Go hook would
have to be attached at each, and the failure mode of forgetting one is a post
whose derived flag is stale and therefore a mature asset served to a
disqualified viewer. A trigger cannot be forgotten by a new call site.

`ANY member` rather than `all`: a bundle containing one mature piece is a
bundle a disqualified viewer must not be handed.

### 5. Search unfindability is deferred to #1117, and the obligation is named

**A withheld value has DERIVED COPIES, and every copy must be withheld from
the same viewer.** That rule cost #902 and #1066 a sprint each — `search_text`
answered for a withheld title, then the CLIP embedding did.

For mature content the copies are: `search_text` full-text matches, facet
counts, suggest completions, the similarity channel, and thumbhash/preview
placeholders beyond the deliberate blurred tile. **None of them are closed by
#1115**, which builds the predicate and the two composition points above and
nothing else.

This is a **deferral, not an omission**, and the distinction is recorded here
so the next reader does not mistake a half-built gate for a finished one:

> ⛔ Until #1117 lands, a disqualified viewer can still find a mature asset by
> searching its title, and can still see it counted in a facet. The row is not
> returned by browse and its bytes are refused; its *existence and its words*
> are not yet withheld.

The federation question — does the flag travel to a peer? — is also #1117's,
and ADR 0083's test applies to it.

## Consequences

- One new column, one new `system_config` section, one new preference field,
  one trigger. No change to `sensitivity`, no change to `ContentReadable`, no
  change to any existing predicate's shape — mature is a conjunct ANDed in,
  which can only ever narrow.
- The predicate has the same-item-opposite-verdicts test pair per arm
  (anonymous vs opted-in; opted-out vs opted-in; toggle on vs off), because a
  single-caller assertion passes on a gate that refuses everyone and on one
  that refuses no one.
- The post derivation is asserted **from the database** after a member is
  added and after one is removed — the persisted value, not the handler's echo
  (#946).
- With the instance toggle off, publishing a mature flag is **refused** rather
  than silently accepted-and-ignored. An accepted-but-inert write is how a
  library ends up full of flags nobody enforces.
- Turning the toggle back on restores every previously-set flag, because
  nothing clears them. The toggle governs enforcement and publication, not
  storage.

---

## Amendment, 2026-08-26 (#1292): the reader gets a VIEW-level control, and it can only narrow

The three conjuncts above decide **whether a viewer may be shown a mature row**. They do not give a
reader who has opted in any way to say *"not in this feed, right now"*. Owner ruling, 2026-08-26,
adding that as a third gating surface:

| # | layer | where | what it decides |
|---|---|---|---|
| 1 | **instance** | `/admin/system/mature-content`, tiled under Community & moderation (#1179) | does this install carry adult work at all |
| 2 | **account** | `/account/preferences` → `user_preferences.mature_content.show` | this reader opts in to being shown it |
| 3 | **view** | the browse filter menu (#1292) | include it in *these* results, right now |

**The cascade, and both rungs are ABSENCE rather than disablement:**

- **1 off ⇒ 2 is hidden.** Already true — `account/preferences/+page.svelte:508` wraps the whole
  mature block in `{#if auth.user?.matureContentAllowed}`.
- **2 off ⇒ 3 does not appear.** A control meaning "include mature in these results" is meaningless
  to a reader who has not consented; it could never do anything. It renders only when the instance
  allows **and** the account has opted in.

⛔ **Layer 3 NARROWS and never consents.** Layer 2 is the consent. Layer 3 therefore **defaults to
included**, so shipping it changes nothing for a reader who already opted in, and there is no path
by which the view control grants something the account control withheld. It is a filter over rows
the three conjuncts have *already* allowed — never a fourth conjunct, and never a widening.

⚠️ **The mechanisms behind layers 2 and 3 differ and the code must not pretend otherwise.** Layer 2
is a server-resolved account preference that roams between devices and gates rows before they are
returned. Its neighbour in the same menu — the AI filter — is device-local, client-side, and by
ADR 0094 §4 never gates. Presenting them as one category is right for the reader, who is answering
one question; building them on one mechanism is not.

⛔ **Writing the layer-2 preference is hazardous and any new writer must re-GET and merge.**
`UserPreferencesRequest` treats an absent member as a **reset**, so a partial write silently wipes
what it omits — the failure `account/preferences/+page.svelte:210-230` documents as *"a reader opts
in, changes a notification channel an hour later, and is silently opted back out of content they
had consented to see."*

## Amendment, 2026-08-28 (#1345): the view control renders on CAPABILITY TO RECEIVE, not on consent

The #1292 amendment above states rung 2 as *"2 off ⇒ 3 does not appear"*, and gives the reason:
a control meaning "leave mature out of these results" is *"meaningless to a reader who has not
consented; it could never do anything"*.

⭐ **Both rules are right, and together they left a hole.** §2 exempts owner and `system.admin`
from the mature disqualification, *"a deliberate asymmetry rather than a convenience"*, so a
moderator can see what the instance switch hid. **That means rows reach an exempt account
regardless of consent, so the stated reason for hiding the control does not hold for them.** The
one class of reader shown mature content without opting in was the one class offered no way to stop
seeing it. Found by sprint 16b (#1343) when its own test failed against the real exemption.

**The rule now: rung 2 asks whether this reader can RECEIVE mature rows, which is `opted in OR
holds the §2 exemption`.** Consent still answers it for every reader who has given one; the
exemption answers it for the reader the old spelling could not see.

### Three reader classes, three layer-3 defaults

The #1292 amendment's *"defaults to included"* was written when the only readers who could see the
row were readers who had consented. Extending the row to a new population is a new case, and a rule
settled on one class does not close the question for another:

| reader | row | layer-3 default |
| --- | --- | --- |
| instance forbids mature content | absent | n/a |
| allowed, opted in | present | **included** (unchanged, per #1292) |
| allowed, exempt for moderation, never opted in | **present (new)** | **excluded** |

The third defaults to **excluded** because that reader has never said yes to anything, and because
minimising a reviewer's exposure to distressing material is the standard for exactly this
population: trust-and-safety guidance treats it as a cost to be reduced, not a privilege of the
role, and no surveyed platform makes *"you are permitted to review this"* imply *"you will be shown
this by default"*. Personal moderation is modelled elsewhere as a layer separate from platform-wide
moderation that only ever narrows what the viewer receives (Jhaver et al., *Personalizing Content
Moderation on Social Media*, CSCW 2023, `10.1145/3610080`), which is what layer 3 already is.

It is a **per-view default, not a refusal**. One click gets an exempt reader the unfiltered wall
when they are actually moderating.

⛔ **Layer 3 still NARROWS and never consents, in every respect #1292 fixed.** An exempt reader
turning the row on has not granted themselves anything; turning it off has not revoked their
exemption. There is still no "include" value on the wire, and nothing here writes
`user_preferences`.

### ⚠️ The default varies by class, so the stored flag had to become TRI-STATE

`aa_browse_hide_mature` used to store `1` for "exclude" and remove the key otherwise, on the
reasoning that "included" is the one default and a stored false is a key that says nothing. With
two defaults that reasoning fails in both directions: an absent key can no longer mean one thing,
and removing the key on an explicit *include* would erase an exempt moderator's deliberate choice
and re-narrow their wall on the next load: a control that visibly forgets.

So the key now stores `1` for an explicit exclude, `0` for an explicit include, and **no key** is
the only spelling of "this device has not answered", which is the `local ?? class default` contract
the layout, tab and sort preferences already use. A device carrying a pre-#1345 `1` still reads as
exclude, so nothing stored had to move.

### What the client reads the exemption from

`auth.can('system.admin')`, which mirrors ONE server predicate: `posts.Handler.ListPosts` passes
`MatureAdmin: caller.Can(auth.SuperAdminCapability)` and `visibility.MatureItemVisible` waives the
qualification on exactly that flag. Not `canSeeAdmin`, which is the wider "may open some admin
surface" set and would offer the row to read-cap operators the gate does not exempt: a control that
could never do anything, which is the failure this cascade exists to prevent.

⚠️ **The OWNER half of the §2 exemption is deliberately not part of this.** It is per ROW (an
artist sees their own work), so it cannot be a property of the reader, and a browse wall is not a
question about one item. `MatureFilterSQL` evaluates it per row for the same reason.

⭐ **No server change was needed, and that is a property of the existing design rather than luck.**
`ListPostsPageParams.ExcludeMature` is documented as *not* waived for an admin, precisely because
"this is the moderator's own request about their own feed, and honouring the gate's exemption here
would mean a control that visibly refuses to do the one thing it says it does". The server was
already willing to honour `?mature=not_mature` from an exempt caller; only the client's decision to
offer the control was wrong.

### Out of scope

Whether a moderator wants a dedicated review surface rather than a filtered browse wall. Bigger
design, and not what #1345 reports.
