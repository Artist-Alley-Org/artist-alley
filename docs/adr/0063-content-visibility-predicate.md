---
id: "0063"
title: Content visibility — one predicate as the single enforcement point
status: accepted
date: 2026-07-18
area: security
phases: []
supersedes: []
related:
  - "0061"
  - "0043"
tags:
  - security
  - authorization
  - visibility
  - search
excerpt: >-
  Content visibility is decided in exactly one place — the visibility
  package's predicate — which is spliced into every read path. Anonymous
  callers see a public tier that now exists in the schema; the
  authenticated sensitivity rule is deliberately left undecided rather
  than guessed.
---

## Context

Two separate problems presented as one.

**There was no public tier.** `collections.visibility` and `posts.visibility` both
constrained to `private | org-only | followers | explicit-share` — no `public` value
anywhere. The predicate's post branch filtered `visibility = 'public'`, a value the CHECK
constraint forbade, so it was dead code that silently matched nothing. The collection branch
short-circuited anonymous callers to `AND (FALSE)`. "Public mode" had nothing to express.

**The predicate was wired but toothless.** An early reading suggested the `visibility`
package wasn't the enforcement path at all. That was wrong, and worth recording because the
mistake shaped the plan: `visibility.Filter` / `Predicate.ToSQL` is spliced into roughly
eleven sites across `search/query.go`, `search/suggest/suggest.go`,
`search/facet/aggregators_impl.go`, `search/http.go`, `search/saved/execute.go`, and
`search/vector/vector.go`. The predicate *was* the enforcement path. It simply had no teeth
for assets, because `EntityAsset` resolved to a soft-delete check and nothing else.

Note this is a different concern from [ADR 0061](0061-admin-surface-visibility-model.md),
which governs *admin navigation tiles*. This ADR governs *content*.

## Decision

**One predicate decides content visibility, for every entity, at every read path.**

Per-entity anonymous cases, defined only in `visibility.ToSQL`:

| Entity | Anonymous caller sees |
|---|---|
| Asset | `deleted_at IS NULL AND status='active' AND sensitivity='public' AND processing_status='ready'` |
| Collection | `deleted_at IS NULL AND visibility='public'` |
| Post | `deleted_at IS NULL AND visibility='public'` |

Migration `00008` adds `'public'` to both CHECK constraints without rewriting any existing
row — introducing the tier is separate from anyone opting into it.

Three consequences are load-bearing enough to state explicitly.

**1. The blast radius justifies the design.** Editing one entity branch changes behaviour at
~eleven call sites simultaneously. That is precisely why the rule belongs in one place — but
it also means the test that matters is a **contract test at the point of definition**
(entity × caller: anonymous / owner / authenticated non-owner / admin), not per-endpoint
assertions. A per-endpoint suite would give weaker coverage at eleven times the cost.

**2. Placeholder numbering is positional, not textual.** The anonymous branches bind **zero**
arguments where collections and posts previously always bound one. That is safe because of an
invariant every splice site satisfies: **no site uses a placeholder index greater than
`argOffset` except the fragment's own**, and predicate args are always appended last. Stated
loosely as "nothing hardcodes a placeholder after the fragment" this appears false — one site
has `LIMIT $2` sitting textually after the spliced fragment. It is nonetheless correct,
because `$2` resolves to the second element of the args slice regardless of where it appears
in the SQL text. The precise, checkable form of the invariant is the index bound, not textual
order. Any new splice site must preserve it.

**3. `ListAssetsPage` is the one exception, and it is being removed.** Every splice site is
hand-built SQL, because a runtime fragment can only be spliced into hand-built SQL.
`ListAssetsPage` is sqlc-generated static SQL and therefore *structurally cannot* accept the
fragment — so the asset browse path is the single read path the predicate does not reach.
Rather than express the rule a second time as sqlc parameters, that query is being converted
to hand-built SQL. Expressing a security rule in two languages and relying on humans to keep
them in step is the failure mode this project has already hit twice (a schema file drifting
from its migrations; the post branch filtering a value its constraint forbade). One
enforcement point is worth losing sqlc coverage on one query.

**Update 2026-07-19 — this became a repeatable conversion, not a one-off.** `ListAssetsPage`
converted in #429 (PR #430); `ListCollectionResourcesPage` followed in #438 (PR #442), taking
the splice-site count from eleven to **twelve**. Both landed the same shape, and it is now the
pattern for any read path the predicate cannot reach:

- Add a hand-built `<Query>Gated` alongside the sqlc query (`assets/list_page.go`,
  `collections/resources_page.go`, `collections` list), mirroring the SELECT list exactly — rows
  scan positionally. **Third application 2026-07-20 (#449, PR #450):** `ListCollectionsPage` was
  the last ungated read path, and the only one opened to anonymous callers *before* conversion —
  an anonymous `GET /collections` returned every collection including private ones. Twelve splice
  sites, now thirteen.
- **Delete the inline visibility clauses the predicate now subsumes — but verify "subsumes"
  per branch, do not assume it.** The authenticated `EntityCollection` branch is `owner OR ACL`
  with **no `deleted_at` conjunct** — the only authenticated branch without one. Dropping the
  inline soft-delete clause there made every signed-in caller see soft-deleted collections and
  rendered `include_deleted` meaningless (#450 caught it with a parity test). ~~That clause stays
  until the branch gains its own conjunct — tracked as **#451**.~~ **Resolved 2026-07-21 (#451,
  PR #469):** the `deleted_at IS NULL` conjunct was hoisted to conjoin the whole authenticated
  `EntityCollection` predicate (`soft-delete + (public OR owner OR ACL)`), matching `EntityPost`
  exactly, and the now-subsumed inline clause in `ListCollectionsPageGated` was removed. All
  authenticated branches now carry the soft-delete conjunct uniformly.
- **Delete the inline visibility clauses the predicate now subsumes.** `ListAssetsPage` shed its
  `deleted_at IS NULL`; the resources query shed `a.deleted_at IS NULL`. Leaving one behind
  recreates the two-expressions-of-one-rule defect this ADR exists to prevent — the same class
  as #210 and #432.
- **Retain the sqlc query, uncalled, for its generated row shape**, with a comment marking it as
  not the enforcement path. This keeps the row struct in sync with the schema, at the cost of an
  ungated query sitting in the package. That trade is only safe while nothing calls it: a
  reviewer must confirm the sole references are comments, which was checked for #442.

## Escape hatches waive one dimension, never the predicate

The asset browse endpoint has a superadmin-only `include_deleted` flag. Honouring it required
a way to relax the soft-delete check — and the shape of that relaxation is a security decision,
not an implementation detail.

`visibility.IncludeSoftDeleted()` drops the `deleted_at IS NULL` conjunct **and only that
conjunct**. Publication status, sensitivity, processing state, ownership, and ACL grants all
still apply. The rejected alternative — skipping the predicate entirely when the flag is set —
fails in two ways:

- **It fails open.** If the caller's admin gate ever regresses, a bypass design hands an
  attacker a path with no predicate at all; the narrow waiver hands them one missing a single
  conjunct, so an anonymous caller is still held to publication and sensitivity.
- **It pre-installs a leak for a rule that does not exist yet.** The moment the deferred
  authenticated sensitivity rule lands (below), a bypass would silently skip that too. The flag
  means "also show me deleted rows"; it must be structurally unable to come to mean "skip
  authorization."

One further detail is load-bearing: the soft-delete conjunct is **not uniform across entities**
— authenticated collections assert none at all. So the waiver is applied per branch rather than
hoisted or globally subtracted; a global approach would silently alter fragments that never had
the conjunct. And where waiving empties a branch entirely, it emits `AND (TRUE)` rather than an
empty string, preserving the "every fragment begins with AND" contract each splice site relies on.

## Deliberately deferred: the authenticated sensitivity rule

An authenticated non-owner can currently list assets of any `sensitivity`
(`public | team | restricted | embargo`). That gap is **left open, consciously.**

`sensitivity` is today consumed only by the federation send/receive gates, never by read
authorization. Closing the gap requires deciding what `team`, `restricted`, and `embargo`
actually *mean* for reads — team membership? time-bounded? explicit grant? — and that is a
product decision nobody has made. Guessing has asymmetric, unattractive failure modes in both
directions: too tight silently breaks callers across all eleven sites at once; too loose is a
data leak. Neither is a good way to discover the intended semantics.

So the authenticated asset case remains `deleted_at IS NULL` — byte-for-byte what it was —
asserted by test so that an accidental tightening fails as loudly as an accidental loosening.
The plumbing now exists to close the gap in a single branch once the rule is decided.

## Deliberately deferred: authenticated visibility tiers (`followers` / `org-only` / `explicit-share`)

**Update 2026-07-20.** A second deferred gap, on a different column from the sensitivity one
above and easy to conflate with it. `posts.visibility` and `collections.visibility` range over
`private | org-only | followers | explicit-share | public`, but the authenticated read branches
collapse *everything non-public* to a single `author_user_ref = $N` disjunct (`EntityPost`,
`EntityCollection` in `predicate.go`). On read, a `followers`-visibility post is therefore
treated as author-only — a follower cannot see it through any gated read path — even though the
activity emitter (`activities/emit/post.go`) and the federation outbox
(`federation/outbox/resolver.go`) already treat `followers` as a first-class audience and
*deliver* it. Write, emit and federate know the tier; local read does not.

Same shape as the sensitivity deferral — an under-modelled tier the predicate must grow a branch
for — but here the **federated** half is already decided, and decided in another ADR, so it is
worth separating what is open from what is not:

- **The audience set is origin-canonical — decided, [ADR 0043](0043-federation-walled-garden-protocol.md).**
  "The originating instance maintains the canonical truth of who has access"; a peer holds a
  `followers`-scoped object only because the origin *delivered* it onto that peer's share list,
  and inbound activities are filtered against that list. **A peer never independently resolves
  another instance's follow graph** — "the follow graph is a special case of sharing." So growing
  a `followers` read branch needs *no* new federated mechanism: on the origin node the branch is a
  local join against the follow relationship; on a remote peer the row's mere presence already
  means delivery gated it.
- **The local read semantics are the open part.** What `org-only` resolves to (same
  workspace/org membership?), what `explicit-share` resolves to (an ACL grant table, as
  collections already carry?), and the exact follow-relationship join for `followers` are the
  unmade product calls — the visibility analogue of "what do team/restricted/embargo mean for
  reads." The predicate is the single place each becomes a branch once decided; guessing breaks
  all thirteen splice sites at once or leaks a tier.

Until then the authenticated post/collection branch stays `public OR author` — byte-for-byte —
asserted by test so an accidental widening fails as loudly as a tightening. **The decision is
tracked as #462; the mechanical consolidation that waits on it (routing the post list through
the predicate) is #212, which is therefore gated on #462, not shippable ahead of it.** One
distinction keeps the two straight: the post-feed *filter* `FeedFollowerRef` ("posts from accounts
I follow") is *curation*, not authorization, and correctly stays **out** of the predicate — a peer
cannot be trusted to run another user's feed query. The `followers` *visibility tier* is
authorization and belongs *in* the predicate. They share a word and nothing else.

**Update 2026-07-27 (#660).** The named query is gone: `ListPostsPage` was deleted from
`posts/queries.sql` and the feed is now `posts.Handler.ListPostsPageGated`, which splices
`posts/read_rule.go`. That is **not** #212 and does not pre-empt #462. #212 is "route the post
list through *this* predicate"; what #660 did is narrower — the post list now obtains the rule
the post *single-item* gate (`canReadPost`) was already applying, instead of restating a weaker
one. It answers no open product question; it only removes a disagreement inside the posts
package. The disagreement was live: the list took the caller's `?visibility=` straight into SQL
with no author or relationship conjunct, so any signed-in caller could read every author's
private posts while `GET /posts/{id}` refused them the same rows. Note what that implies about
the paragraph above — the deferral was recorded honestly for the *predicate*, and a second
expression outside the predicate leaked anyway. Deferring a rule in one place does not defer it
everywhere; it just moves where someone will express it.

**Update 2026-07-28 (#661).** Splicing-at-every-site turned out to be insufficient *in practice*,
and it is worth recording why rather than restating the rule. The mechanism is sound; what fails
is the assumption that a read path's author will reach for it. Three more sites were found by
sweep, none by a test, and each had been correct-looking on the day it was written:

- `GET /assets/{id}/similar` performed **no** identity check at all — a bare existence probe for
  the anchor and a `deleted_at IS NULL` re-fetch for the neighbours, returning the full Asset
  projection. It was unexploitable only because the embedding tables were empty, which is not the
  same as safe: seeding embeddings reproduced the leak immediately.
- The IIIF Presentation loaders (`iiif/presentation/loader.go`) read by id with `deleted_at IS
  NULL` — and `LoadCollection` not even that. So a draft asset served a full anonymous manifest,
  and **any signed-in caller could read the manifest and full member list of anyone else's private
  collection**. The route had a carefully-designed *content*-plane gate (ADR 0064) and no
  *row*-plane gate whatsoever; the presence of one plane is why nobody noticed the absence of the
  other.

Two things generalise. **First: a gate that lives in a different file from the query is not a
property of the query.** `LoadCollection` was safe for anonymous callers only because a switch in
`http.go` defaulted closed, and that switch had gone stale — it fell through on `public`, a value
migration 00008 added, so genuinely public collections 404'd while private ones leaked to
authenticated callers. One expression, two wrong answers. **Second: caller-scoped predicates and
shared caches interact.** The `EntityCollection` authenticated branch binds the caller's ref, so
the manifest cache — keyed only on `anonymous` vs `authenticated` — would have handed the first
entitled caller's manifest to every later one. Splicing the predicate into a read path is not
finished until the path's cache key is at least as specific as the predicate's inputs.

**Update 2026-08-04 (#873). The post branch is no longer `public OR author`, and the
paragraph above that promised it would stay so "byte-for-byte" is retired.** #212 is done,
and it was never actually gated on #462 the way this ADR recorded — that gating assumed the
local read semantics for `org-only`, `followers` and `explicit-share` were still unmade
product calls. They were made and shipped, in `posts/read_rule.go`, by #660 and #667. So the
open question was closed *outside* the predicate, and what remained was a copy: browse
composed the rich rule, `EntityPost` composed the coarse one, and the three splice sites that
read posts — search results, the tag facet, the suggest completions — silently returned less
than their caller could read. An org-only post, the *default* tier, was on your feed and
absent from your search results, with no error and no empty state to say so.

The rule now lives in `visibility/post_rule.go` and `posts` obtains it through `Filter` like
every other caller. Three things this settles, each of which the earlier deferral had assumed
was harder than it is:

- **Capabilities do not have to be threaded through `Filter`.** The `private` tier needs
  `posts.admin`, and the objection recorded above — that admitting a capability checker would
  move every splice site — is answered by resolving it to a value first, exactly as #899 did
  for the content plane. `PostCaps` is a two-state struct set by an `Option`; no other
  entity's branch reads it, so no other splice site moved.
- **The follow graph was never the obstacle.** It is an `EXISTS` against `user_follows` in
  the same statement, not a Go lookup.
- **A caller-scoped predicate needs a caller-scoped cache key** — the second lesson from #661,
  applied forward rather than after the fact. `posts.admin` widens which rows the cached
  search result contains, so the key folds in `PostCaps.CacheKey()`; a caller who loses the
  capability stops being served the wider page immediately rather than at TTL expiry.

Soft-delete stays outside the rule expression and inside `ToSQL`, so `IncludeSoftDeleted`
waives that conjunct and nothing else. That separation is structural on purpose: the admin
trash view must not be able to shed an authorization disjunct along with the soft-delete one,
and that failure returns extra rows which look exactly like the deleted ones the caller asked
for.

## Consequences

- Content visibility has exactly one definition; new read paths inherit it by construction,
  and a new *storage-shaped* read path that cannot splice a fragment is a design smell to
  resolve, not to work around.
- **Splicing is not self-enforcing.** Every instance in epic #665 was a path that never called
  `Filter` at all, so no amount of correctness *inside* the predicate would have caught them.
  What now prevents recurrence is the invariant test shape the sweep converged on: a list path
  may never return a row the corresponding single-item read would refuse, asserted per entity
  and per caller class. It is stated set-theoretically so it survives changes to the predicate
  itself, and it fails on a path that forgot the rule rather than on one that got it wrong.
- The contract test is the real deliverable of this work — more than the migration.
- Anonymous access remains unreachable in production until the anonymous API surface lands,
  so this change is behaviour-preserving on merge.
  **That condition is now satisfied (2026-07-19).** The anonymous surface landed in two parts:
  bytes for `public`-tier assets (#415 item 5, PR #437) and the four read operations
  `listAssets` / `getAsset` / `listCollections` / `getCollection` (PR #439). Anonymous access is
  live, and this predicate is what decides it.
- The deferred sensitivity rule is a known, recorded gap rather than an oversight; it should
  be closed before any surface makes authenticated browsing broadly available to untrusted
  accounts.

## References

- ADR 0061 — admin surface visibility model (navigation tiers; distinct concern).
- Migration `00008` — adds the `public` tier to collections and posts.
- The public-mode arc: anonymous API surface, logged-out frontend, and the featured rail
  build on this predicate.
