---
id: "0020"
title: Asset gating & NDA workflow — pre-release blur, scheduled actions
status: accepted
date: 2026-05-30
area: security
phases: 
  - "1.17"
  - "1.18.A"
  - "1.27"
  - "1.28"
supersedes: []
related: []
tags:
  - security
  - ai
  - infrastructure
excerpt: >-
  Game studios constantly handle pre-announcement material that must NOT be visible outside a small approved audience until a marketing date. The patterns are universal:
---
## Amendment (2026-08-04): 0020 governs the IMAGE. It does not govern the PAYLOAD — and where it implied one, it is narrowed (#899)

The "Sensitive asset gating" section below is written as though a
`restricted` asset stays **listed and identifiable**: a blurred thumbnail, a
lock icon, a "Reveal" button, reviewers commenting on blurred content, an
embargo card reading *"this content is embargoed until YYYY-MM-DD"*. Every one
of those describes what the tile LOOKS LIKE. None of them says anything about
what the JSON carries, and for three releases the answer was "everything".

⭐ *Further amended 2026-08-13 (#902, PR #1063).* The #899 narrowing above fixed what the
**payload** carried. It left a third channel open, and "listed and identifiable" turned out to
promise something on that channel too: the asset's own withheld title was still in its
`search_text`, so **any caller could recover it word by word** — query a phrase only that title
holds, watch the result total move 0→1, then walk the remaining tokens. Identifiable to the
*reader* had quietly meant identifiable to the *index*.

Every full-text surface over `assets` now ANDs the field-plane rule onto the match
(`visibility.AssetSearchMatchSQL`; see ADR 0056 §4c). A `restricted` asset **still appears in an
unfiltered browse with its blurred thumbnail and lock icon**, exactly as this section requires —
it simply no longer answers text queries about words it does not show you.

**The general form, worth carrying to the next tier decision:** a tier that *displays* something
withheld has at least three channels to close — what the tile renders, what the JSON carries, and
**what the index answers.** This ADR governed the first, #899 the second, #902 the third.

Verified on a live build on 2026-08-04, signed in as a user with
`capabilities: []` who owned none of the assets involved:
`GET /api/v1/assets/{id}` on someone else's `restricted` asset returned **200**
with the title, the description, the complete SHA-256, the exact byte size, the
original filename, and the whole free-form `metadata` blob. Search returned the
same title and description; `/search/suggest` completed the title from a
prefix; the SENSITIVITY facet counted it. The BYTES were correctly gated
throughout — `/file`, `/download` and `/variants/*` all 404'd — so ADR 0064's
content plane was working exactly as specified. The leak was never in the
binary plane. It was in every surface that describes a row.

**The split, stated so the next reader does not have to infer it:**

- **ADR 0020 (this ADR) governs the IMAGE.** Whether a tile renders blurred, whether
  there is a lock icon, whether a "Reveal" button appears, and how the blur is
  produced (server-baked variant, never CSS). Unchanged, and still the plan for
  Phase 1.28.
- **#883 / #899 govern the FIELDS.** Whether the payload behind that tile carries a
  title, a hash, a size, a filename. The rule is
  `visibility.FieldsReadable` — the conjunction of the row plane (ADR 0063) and
  the content plane (ADR 0064) — and a caller who fails it receives a
  placeholder whose complete key set is `id`, `restricted`,
  `owner_display_name`. The owner's rule, verbatim (2026-08-03): *"The
  placeholder should never leak info. Not even title. Only the owner's name."*

~~The row still exists in every feed, which is 0020's and 0064's shared position
and is what makes "request access" (#881) mean anything. Nothing here removes a
row.~~ **Narrowed 2026-08-05 (#921) — see the amendment below.**

## Amendment (2026-08-05): the RULE still returns the row; the default FEED no longer draws it (#921)

The struck sentence above conflated two layers, and #921 pulled them apart. Read as
a statement about the **access rule**, it is still exactly true and still 0020's and
0064's shared position. Read as a statement about **what the browse feed renders by
default**, it stopped being true when #921 made hiding restricted placeholders the
default rather than an opt-in.

| layer | before #921 | after #921 | changed? |
|---|---|---|---|
| the access **rule** | does not exclude rows; sensitivity gates content, not rows | identical | **NO** |
| the default **presentation** | renders every row the rule returned | subtracts restricted ones in the feed | **YES** |

`ListPosts` still *receives* every row the rule returns. `applyHideRestricted` subtracts
afterwards, reading one already-computed field (`PostMember.Restricted`, written in exactly
one place off the single `visibility.FieldsReadable` call). **Nothing about who may read what
moved.** What moved is what the feed chooses to draw.

**"Request access" still means something**, which was the struck sentence's real point. #913's
button lives on the placeholder, and the placeholder still renders on `GET /posts/{id}` and in
collection contents — the two surfaces where a reader **asked a question** or **opened a
container**. It is the feed, where they were handed a grid they did not ask for, that stopped
drawing them. Measured motivation: one seeded account's feed was 82 posts of which 27 were
entirely placeholders.

**A fork this ADR should reconsider when Phase 1.28 lands.** The Decision section below specifies
server-baked **blurred** thumbnails with a lock icon for `restricted` and `embargo` assets. A
blurred tile is a genuinely different proposition from a "you cannot have this" placeholder — it
shows the shape of the work rather than only its absence, so the busyness argument that motivated
#921 may not survive it. **Whether hiding-by-default is still right once blur-and-reveal ships is
an open question, deliberately left open here.** Do not treat #921 as having settled it.

Full reasoning, including the `hide_restricted` → `show_restricted` rename and the inverted
nil/error seam, lives in ADR 0064's 2026-08-05 amendment.

**Two places where the implementation is narrower than what 0020 says, deliberately:**

1. **The thumbhash is withheld.** A thumbhash IS a blur — a low-frequency
   reconstruction of the image — and shipping it to a caller who cannot open the
   asset is a client-side blur of exactly the kind the "Alternatives considered"
   section rejects as *"trivially defeated"*. It is withheld now. That does not
   pre-empt the server-baked blur variant this ADR specifies: that variant is a
   deliberate, operator-controlled derivative served under a capability, and
   Phase 1.28 remains free to serve it. The distinction is deliberate blur vs
   accidental blur.
2. **The embargo card's DATE is not currently sent.** *"This content is embargoed
   until YYYY-MM-DD"* is a fact about the item, and the 2026-08-03 rule permits
   only the owner's name on a placeholder. When Phase 1.28 builds the embargo
   card, adding `embargo_until` to the placeholder's allow-list is a deliberate
   decision to make then, with the owner — not something to assume from this
   ADR's prose. The allow-list is enforced by
   `assets/field_withholding_test.go`, so the decision cannot be made by
   accident.

Everything below stands as the Phase 1.28 design. Read it as the visual
specification it is.

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

Two related patterns from existing DAM tooling: sensitive-image
thumbnail blur until permitted to view, and scheduled actions on
resources (delete, restrict, archive at a future date). Neither is
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
  cron, now first-class. Replaces a large portion of typical bespoke
  plugin tooling.
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
