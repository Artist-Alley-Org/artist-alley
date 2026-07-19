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

## Consequences

- Content visibility has exactly one definition; new read paths inherit it by construction,
  and a new *storage-shaped* read path that cannot splice a fragment is a design smell to
  resolve, not to work around.
- The contract test is the real deliverable of this work — more than the migration.
- Anonymous access remains unreachable in production until the anonymous API surface lands,
  so this change is behaviour-preserving on merge.
- The deferred sensitivity rule is a known, recorded gap rather than an oversight; it should
  be closed before any surface makes authenticated browsing broadly available to untrusted
  accounts.

## References

- ADR 0061 — admin surface visibility model (navigation tiers; distinct concern).
- Migration `00008` — adds the `public` tier to collections and posts.
- The public-mode arc: anonymous API surface, logged-out frontend, and the featured rail
  build on this predicate.
