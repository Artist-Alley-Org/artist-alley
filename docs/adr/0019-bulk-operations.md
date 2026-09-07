---
id: "0019"
title: Bulk operations — multi-select, batch edit, exports, contact sheets
status: accepted
date: 2026-05-30
area: ux
phases: 
  - "1.12"
  - "1.18.A"
  - "1.26"
  - "1.27"
supersedes: []
related: 
  - "0017"
tags:
  - ux
  - ai
  - 3d
excerpt: >-
  Game studios manage 10k–500k+ assets. The current Artist Alley post detail flow assumes you act on one post or one asset at a time. Existing DAM tooling ships deep bulk-operation surfaces: multi-select edit, batch tag, batch delete, multi-row metadata import via CSV, configurable CSV export of search results, and printable contact-sheet generation.
---
## Context

Game studios manage 10k–500k+ assets. The current Artist Alley post
detail flow assumes you act on one post or one asset at a time. Existing
DAM tooling ships deep bulk-operation surfaces: multi-select edit, batch
tag, batch delete, multi-row metadata import via CSV, configurable CSV
export of search results, and printable contact-sheet generation. The
audit (2026-05-30) flagged these as the second-highest impact gap —
anyone with > ~1k assets discovers it inside a week.

## Decision

Add Phase 1.27 — Bulk operations — as a horizontal surface that hangs
off the browse feed and post views, not as a separate page. The cost of
a feature like this is dominated by getting the UI patterns right and
the safety rails strong (you do NOT want a "bulk delete 50k assets"
button without a strong confirmation).

### Selection model

- Browse feed grows a checkbox in each card / row. Headers gain
  "select all on page" + "select all in current filter" + "clear
  selection."
- Selection state persists across pagination within a session.
- Selection is visible at all times via a floating action bar that
  shows the count and the available actions, like Gmail.
- A "saved selection" can be stored on a collection so a curated batch
  can be re-acted-on later.

### Available bulk actions

- **Tag / untag** — apply a tag to N posts; chip-style entry, dry-run
  preview shows the diff before applying.
- **Set metadata field** — choose a field, set a value; respects per-
  field validation. Mass overwrite is irreversible without versioning
  (see ADR 0017 tier behaviour + future version history).
- **Move to collection** — bulk-add to one or more collections.
- **Change workflow state** — bulk-transition (e.g., draft → approved)
  subject to per-user capability.
- **Delete** — moves to trash with a configurable retention (default
  30 days). Hard delete requires a second confirmation; admins only.
- **Download as zip** — server-side packs the selection, streams as
  a single archive. License-tier gates concurrency.
- **Export CSV** — operator picks columns; CSV is computed via the
  search engine, not the post table, so the same column shape works
  on a saved-search result set.
- **Generate contact sheet** — configurable grid (rows × cols), per-
  thumbnail metadata footer, header / footer text, page orientation,
  output PDF.
- **Apply share link** — mint a single share link covering all selected
  items (Phase 1.26 wiring).

### Safety rails

- Every bulk action shows a preview: "this will affect 4,123 items."
- Destructive actions (delete, overwrite-metadata) require typing the
  count to confirm: "type 4123 to confirm."
- All bulk operations are submitted as a single job-queue job. The
  job is paused / cancellable / resumable. Progress streams to the UI.
- Audit log entries record the bulk operation as one row with the
  selection IDs preserved, so a single "undo" can revert the batch
  (where the action is reversible).

## Consequences

**Positive**

- Removes the largest single source of "this is unusable at scale"
  friction.
- Reuses existing primitives: search engine for selection,
  job queue for execution, audit log for traceability.
- Contact sheet generation is a single converter; ImageMagick + the
  existing preview pipeline cover it without a new worker.

**Negative**

- "Undo" semantics on irreversible operations (hard delete) are
  necessarily limited. Default-trash mitigates the common case but
  doesn't help "I overwrote 4,123 description fields." Mitigation: a
  light per-field history (touched in ADR 0017's tangled-derivation
  consumer list as `fieldHistoryCap`) so bulk overwrites are
  rollback-able for Pro+.

## Amendment 2026-09-04 — the four-mode batch metadata edit, as built

Sprint 20c builds the **Set metadata field** action above, and only it,
for **assets**. Everything else in the Available bulk actions list is
untouched. What shipped diverges from the 2026-05-30 design in ways that
are decisions rather than omissions, so this amendment records them
against the code (#1173, #1119).

### The four modes, and why the locked-fields cascade does not transfer

`overwrite`, `fill_empties`, `append` and `remove`, over ONE field with
ONE proposed value.

`append` and `remove` are `multi_select` ONLY. The other ten types —
text, longtext, rich_text, select, tree, number, boolean, date, datetime,
reference — refuse them batch-wide with 422 `mode_not_supported_for_type`
rather than being given an invented set semantics. "Append to a number"
has no meaning two people would agree on, and inventing one so that four
mode names look uniform across eleven types is how a catalogue ends up
full of concatenated dates.

There is no batch CLEAR. An empty `overwrite` and a clear are different
operations: an empty overwrite is a SET (the row exists afterwards, its
`set_at` advances, a history row is written, and R1's `requiredSetRefusal`
governs it), while a clear REMOVES the row and `requiredClearRefusal`
governs it. The one exception is `remove` emptying an OPTIONAL
`multi_select`, which deletes the row because writing `[]` into
`value_options` is a shape the single-target writer refuses.

The Decision's "respects per-field validation" is honoured by obtaining
the shipped rules rather than restating them, so the locked-fields
cascade the original design implies never arises: there is no second
copy of `read_only`, the pattern, the required rule, the vocabulary rule
or the mirrored-column rule to keep in step.

### SYNCHRONOUS AND BOUNDED — superseding the job-queue choice, for these four modes only

The Decision says "all bulk operations are submitted as a single
job-queue job… progress streams to the UI." For these four modes that is
**superseded**. They execute synchronously inside the request, and:

- **THERE IS NO LIVE PROGRESS.** The apply response is completion and
  result reporting.
- **The ceilings are the boundary.** At most **500 selection entries**,
  checked before any membership query runs, and at most **1,000 distinct
  expanded targets**, which is authoritative. Both refuse with 422
  (`selection_entry_ceiling` / `expanded_target_ceiling`) rather than
  trimming — a partial expansion would silently write a different set
  than the operator selected — and a single post whose membership alone
  exceeds the expanded ceiling is refused on the same terms.
- **The over-ceiling refusal names the TRUE distinct expanded count**,
  not the bound it was measured against. The count is computed in the
  database and the target ids are read only once it is known to fit, so
  the server neither executes a partial batch nor pulls an unbounded id
  set into memory to find out how far over the selection reaches. An
  operator told "1,001" when the real answer is 50,000 would trim one
  post at a time towards a target that was never within reach.
- **#39's remaining actions stay job-oriented.** Delete, zip, CSV export
  and contact sheets are unbounded by nature; this supersession does not
  reach them.

Measured at the 1,000-target ceiling: p95 apply 0.83 s against a 10 s
budget, one `rebuild_asset_search_text` and one `pg_notify` per written
row, four post-document rebuilds rather than one per written row, and a
39.6 KB audit envelope against a 128 KB budget. A concurrent ordinary
single-target write to another field ON A BATCH TARGET now queues for
the batch's duration rather than passing through it. See the 2026-09-06
amendment: the earlier figures on this line, and the framing that made
"an unrelated field" the relevant variable, were both wrong.

### The public wire contract

**Collections are outside this plane, on both sides.** They are not a
selection kind, and a field whose `subject_kind` is `collection` is NOT
FOUND on this endpoint — refused before any stored target value is
inspected, with no token issued. The single-target asset writer does not
ask this question (it writes `asset_field_value` whatever the definition
says, while its collection twin gates the discriminator explicitly);
that asymmetry is pre-existing, is NOT changed here, and is not a reason
to carry the gap onto a plane that reaches a thousand records at once.

**Preview** takes the mode, the field, the proposed value and a TYPED
selection of `{kind, id}` entries where kind is `asset` or `post`. The
SERVER expands posts through membership, dedupes to distinct assets, and
orders BY ASSET ID — not by selection order (client-supplied) and not by
`sort_order` (mutable), because asset id is the only ordering both
endpoints derive identically. Collections are not a selection kind.
Preview returns the six partition counts, a per-target partition with a
machine reason where one applies, a per-would_change-target concurrency
token (that value row's own `set_at`), an echo of the mode and the
resolved canonical value, and an opaque token with an expiry.

**Apply takes EXACTLY THREE MEMBERS**: the `token`, a required operator
`reason`, and a `confirm_count` that is required for `overwrite` and
`remove` and FORBIDDEN otherwise. **It sends no mode and no value** —
both are read from the token — **which is why no payload-mismatch
refusal code exists**. One must not be reintroduced, and the value must
not be added to apply in order to keep one alive.

### The operator reason, and its TWO DISTINCT BOUNDS

REQUIRED, where both shipped operator-reason fields elsewhere in the API
are optional. The divergence is deliberate: a bulk mutation reaches up to
1,000 records and there is no undo.

| | semantic limit | raw defensive ceiling |
|---|---|---|
| measured on | the value AFTER trimming | the value AS RECEIVED |
| unit | Unicode code points | Unicode code points |
| value | **500** | **2,000** |
| on violation | 400 `reason_too_long` | 400 `reason_payload_too_large` |
| in OpenAPI | stated in the description as authoritative | encoded as `maxLength` |

The schema's `maxLength` is the RAW ceiling and never the semantic one,
so a conforming external validator never rejects a value the server would
accept — whitespace around a 500-code-point body is valid. Handler order
is: raw ceiling, trim, requiredness, semantic limit. Code points, not
bytes. Recorded verbatim after trimming.

⚠️ `maxLength` is enforced NOWHERE in generated code. Every bound is
enforced in the handler or it is not enforced at all.

### The token lifecycle

Bound to its CALLER. SINGLE-USE. It binds the ordered target set with
each target's partition, the field's identity and configuration
fingerprint including the vocabulary options document, the mode, and the
exact canonical value. **It is never authority** — not to write, not to
mint a term, not evidence that a reference target is still alive; all of
those are re-evaluated against the current world at apply.

> **Token consumption, the durable field and vocabulary mutations, and
> the operation's single audit envelope are ONE ATOMIC COMMITTED
> OUTCOME.**

There are exactly two durable results and no third. A **pre-write
refusal** commits no field value, no term and no envelope, and LEAVES THE
TOKEN USABLE — so an operator who mistypes the count or omits the reason
corrects and retries without re-previewing. A **committed apply** —
including a partial one, and including one where `would_change` was zero
— commits its result, exactly one envelope and the consumption together.

A lost HTTP response therefore never makes a spent token spendable, and
a 200 is not the consumption boundary. A `would_change == 0` apply is a
REAL operation: it completes, consumes its token, and writes one envelope
recording the zero-change operation with its reason and counts.

### The complete validation precedence

> **No token-bound fact — the mode, the would_change count, the field,
> the target set, the expiry, the consumption state or the expected
> confirmation count — may influence any externally visible response
> until integrity AND caller binding have BOTH succeeded.**

Token-independent request-shape checks may run first, because they leak
nothing. Then, in exactly this order: integrity → caller binding →
consumed → expiry → mode-specific confirmation → current authority,
definition, configuration, vocabulary and reference liveness → the
committed apply.

**Malformed, unknown, tampered and another caller's tokens collapse to
ONE BYTE-IDENTICAL 403** `preview_token_invalid`, and stay
indistinguishable however else they differ. The attack this closes is
specific: apply does not send the mode, so a server that validated
`confirm_count` against the token's mode before checking ownership would
answer `confirm_count_required` versus `confirm_count_not_applicable`
about SOMEBODY ELSE'S preview. Repeat for expiry and consumption and it
is an enumeration oracle over every preview on the instance, built
entirely out of refusals.

**CONSUMED WINS OVER EXPIRED** for a caller-owned token, because it tells
the operator their operation ALREADY RAN where `preview_expired` would
invite a duplicate run.

### The status taxonomy

- **400** — the request could not have been valid for ANY state of the
  system.
- **403** — authorization, INCLUDING anything not provably this caller's
  token.
- **422** — well formed and understood, and this definition, value or
  scale refuses it.
- **409** (apply only) — the token is provably this caller's own but is
  spent or stale, or the world moved; the remedy is to re-preview.

`dangling_reference` (preview: it never resolved) and
`reference_invalidated` (apply: it resolved when the operator looked and
has since stopped) are deliberately NOT collapsed.

### The counts, and what the typed confirmation confirms

```
expanded = would_change + no_op + refused + inapplicable + unreadable + unauthorized
eligible = would_change + no_op
would_change = changed + conflict + gone + unauthorized_at_apply + error   [at apply]
```

Every target of a successful preview belongs to EXACTLY ONE partition.
The Decision's "type 4123 to confirm" is honoured, and the number is
**would_change, not eligible**: the number an operator types is the
number of records that will actually change.

`overwrite` reports `no_op` as ZERO even against a target already holding
the proposed value — a set advances `set_at` and writes a history row, so
it changes the record even where it does not change the value.

### The five-gate stack, and read-as-well-as-write

| | gate | resolution |
|---|---|---|
| G1 | the bulk instrument in the target's scope | per target |
| G2 | the ordinary subject authority rule | per target |
| G3 | applicability | per target |
| G4 | the field's own `write_capability` | batch-wide |
| G5 | the field's `read_capability` | per target |

Only below all five is any stored value inspected. **G5 is on the WRITE
path because the operation reports**: a batch that said "twelve of your
targets already hold this value" would have answered twelve questions
about a field the caller may not read. An `unreadable` target discloses
nothing — not emptiness, not membership, not equality, not `set_at`, and
not in the audit envelope.

Neither the bulk capability nor the subject rule replaces the field's own
write gate, and it does not replace them.

Apply re-checks G1, G2 and G5 per target and G3, G4 batch-wide, against
**EFFECTIVE permission and never raw grant-set equality** — a caller who
loses one direct grant while a role still confers the capability has lost
nothing. A caller whose GLOBAL bulk grant is revoked while a SCOPED grant
still covers part of the selection is NOT failed wholesale: the covered
targets proceed and the rest become `unauthorized_at_apply`.

`assets.metadata.bulk_edit` is TEAM-SCOPE-AWARE, and a TEAM-LESS asset
requires a GLOBAL holding — the nullable trap `visibility.MayMutate`
documents. The field's own `write_capability` and
`fields.vocabulary.extend` are reproduced at their SHIPPED GLOBAL-ONLY
scope; that asymmetry with the team-scope-aware read gate is real, is NOT
fixed here, and is recorded by a standalone preservation test so a later
fix moves both together.

### The four atomicity families

"Same transaction" is NOT sufficient at READ COMMITTED. The precondition
lives on `assets` and the mutation lands on `asset_field_value`, a
DIFFERENT TABLE, so 20a's single-statement guarded-write pattern does not
transfer. **The foreign key's implicit FOR KEY SHARE protects none of
it** — it conflicts only with FOR UPDATE, while ownership transfer, team
move and soft delete all take FOR NO KEY UPDATE — and **there is no
foreign key on `value_ref` at all**.

1. **Per-target subject.** The target's current owner, team and liveness,
   the caller's G1/G2/G5 verdicts, and the mutation they authorise are
   one atomic operation relative to competing transfer, team move and
   soft delete. Failure is per target.
2. **Batch-wide definition, configuration and vocabulary.** Exactly two
   valid serial outcomes: the external change wins and the batch refuses
   with ZERO writes and no mint, or the batch wins and the ENTIRE batch
   executes under one validated state. Never the first N under the old
   rules and the rest under the new.
3. **Proposed-reference liveness.** A pre-batch re-check is not
   sufficient; it establishes a fact that can stop being true before the
   last write lands.
4. **Effective authority.** The caller's effective verdict and every
   mutation it authorises are one atomic operation relative to a
   competing authority change. This covers ALL FOUR authorities the
   batch consumes — bulk-edit admission and per-target scope, subject
   authority, the field's own write capability, and mint authority —
   because all four are drawn from ONE effective-authority read.

Technique is implementation-owned. As built: ONE ascending FOR UPDATE
statement over the would-change subjects UNION any proposed reference
target, taken before a single value is written; FOR UPDATE on the field
definition taken BEFORE it is read; and, for authority, a
transaction-scoped ADVISORY LOCK taken BEFORE the authority read and
held to commit. The subject and reference locks were FOR SHARE, taken
one target at a time; the 2026-09-06 amendment records why that
deadlocked and what replaced it.

#### Amendment 2026-09-04 — re-resolving authority in the transaction is NOT serialization

The first implementation of family 4 re-read the caller's capabilities
from the apply's own transaction and treated that as sufficient. **It is
not, and this ADR said so about every other family while missing it
here.**

At READ COMMITTED each statement takes a fresh snapshot. Re-reading in
the transaction makes an authority change committed BEFORE the read
visible, and does nothing about one that commits AFTER it: that change
lands while the verdict is still being relied upon, and the writes it
authorised go through. The batch locked the field definition, the
reference target and each subject — and authority lives in none of
those. It lives in `user_roles`, `roles`, `role_capabilities`,
`user_capability_grants`, `user_capability_revokes` and `team_closure`.

⛔ **A row lock could not have closed it either.** The dangerous mutation
is frequently an INSERT — a revoke ADDS a row to
`user_capability_revokes` — and `FOR SHARE` locks rows that exist.
PostgreSQL has no predicate locking at READ COMMITTED, so no locking of
the rows the authority query reads can exclude a row that does not exist
yet. An advisory lock is not a shortcut here; it is the only mechanism
that can express the claim.

**As corrected.** One transaction-scoped advisory lock space, with a
reader half and a writer half. The batch takes the SHARED half — on the
caller's key and on a structural key — BEFORE resolving authority, and
holds both to commit.

⛔ **Both sides participate, and that is the whole mechanism.** A lock
only the batch took would prove nothing — which was the defect in the
original evidence for this family, whose test made its competing revoke
wait on a `field_definition` lock that no production revoke touches.

#### The authority-writer inventory, stated as it is

An earlier draft of this amendment said "every production path that
mutates authority takes the exclusive half". ⛔ **That was a universal
claim reached before the enumeration was complete**, and it was false:
`requests.Handler.Grant` writes `user_capability_grants` from the
requests package and had been missed. The inventory is therefore given
in full, with each entry either PARTICIPATING or carrying a stated
lifecycle exemption.

**Participating — take the exclusive half before the write:**

| writer | scope | key |
|---|---|---|
| `auth` · add a per-user grant | transaction | user ref |
| `auth` · remove a per-user grant | transaction | user ref |
| `auth` · add a per-user revoke | transaction | user ref |
| `auth` · remove a per-user revoke | transaction | user ref |
| `auth` · assign a global role | transaction | user ref |
| `auth` · capability expiry sweeper | transaction | structural |
| `requests` · approve a capability request | transaction | **requester's** ref, not the approver's |
| `teams` · add a team parent | transaction | structural |
| `teams` · remove a team parent | transaction | structural |
| `cmd/aa` · `aa seed` bootstrap admin | **session** | structural |
| `cmd/aa` · `aa seed --reset` (`resetContent`) | **session** | structural |
| `seed` · `Runner.applyTeams` (`team_closure`) | **session** | structural |
| `seed` · `Runner.applyFixturePrincipals` (`user_roles`) | **session** | structural |

The sweeper, the team-parent paths and every seed span use the structural
key because their blast radius is not one nameable user: the sweep reaps
across every user at once, re-parenting changes what every team-scoped
grant expands to WITHOUT TOUCHING A SINGLE GRANT ROW, and the reset
empties the authority tables outright.

#### `aa seed` is NOT lifecycle-exempt, and the session scope is why

An earlier draft of this inventory wrote it off as "offline maintenance
that also TRUNCATEs". ⛔ **Both halves were false**, and the mistake was
the same one that produced the false universal above: classifying by
command name instead of by call site and actual concurrency.

It is DESIGNED for a live instance. Its migrate step is documented as
safe "whether the server already migrated, is migrating right now, or was
never started", and `--reset` broadcasts a wildcard cache flush over
NOTIFY precisely because "the seeder is a separate process with no cache
Registry" and a server may be serving throughout. And the TRUNCATE is the
BLAST RADIUS rather than a reason to dismiss it: `seed.Reset`'s
`TRUNCATE ... CASCADE` and its `DELETE FROM teams` empty `user_roles`,
`user_capability_grants` and `user_capability_revokes` wholesale, and
`bootstrap.Run` then restores the admin's role.

⛔ **The seed spans take the lock at SESSION scope, and that is not a
detail.** Their authority mutations run as a series of autocommit
statements — a TRUNCATE, several DELETEs, a bootstrap restoration, then
the runner's own writes. A transaction-scoped lock would be released at
the end of whichever statement took it and would leave the rest of the
span unprotected: a lock that exists, that production really takes, and
that does not span the thing it protects. Session and transaction
advisory locks share one space and conflict identically; scope decides
only when a lock is released.

⛔ **A session lock makes its own release a correctness problem, not a
tidiness one.** `pgxpool.Conn.Release()` returns a connection TO THE POOL
FOR REUSE, and a session-scoped advisory lock survives until it is
explicitly unlocked or the PostgreSQL session is destroyed — an ordinary
release does neither. So a single failed unlock (a cancelled context, a
network blip, a statement timeout) would hand a still-locked session back
to the pool, where it would sit idle and unowned holding the structural
lock forever, and every batch apply after it would block indefinitely on
a lock with no owner. A swallowed error there is a total, permanent stall
of the feature with nothing to explain it.

The release therefore HIJACKS the connection out of the pool and CLOSES
it whenever the unlock fails, which destroys the backend session and is
what actually drops the lock, and logs at ERROR. The pool opens a
replacement on demand.

**Four separate, non-overlapping spans, each no wider than the authority
mutation it protects.** An earlier version wrapped the seed runner's
whole `Run`, which held the STRUCTURAL lock — the one that excludes every
batch authority reader — across catalogue loading, users, memberships,
follows, fields, collections, featured, ASSETS, POSTS, likes and
comments. A reseed runs for minutes, so an unrelated batch apply could
wait out its entire deadline while image files were loading. Only two of
those phases mutate authority.

The runner's protection lives IN the two phases rather than around the
call, so any caller gets it — including a test that drives one directly,
which is how the narrowing is proven rather than asserted.

⭐ **The phase, not the statement, is the semantic unit for both, and
that is a claim rather than a convenience.** `team_closure` describes a
HIERARCHY: it is only the shape the catalogue specifies once every team's
rows are in, so a reader resolving a scoped grant against a half-built
closure sees a real and different expansion. The fixture principals are
seeded as a SET, and a reader midway through sees a world that is neither
the old one nor the new one. Locking per statement would open exactly
those windows.

**The exhaustive check.** Every write to `user_roles`, `roles`,
`role_capabilities`, `user_capability_grants`, `user_capability_revokes`,
`team_closure` and `team_parents` in the seed package resolves to those
two queries and no others. `team_memberships` is deliberately absent:
`EffectiveScopedCapabilitiesForUser` does not read it, so it is not
authority-bearing for this invariant.

⚠️ **The operational consequence, stated rather than absorbed.** While the
structural lock is held, batch metadata applies WAIT — so what matters is
how long it is held, and it is held only for the authority spans above,
never across a whole command. The long parts of a reseed — catalogue
loading, users, memberships, follows, fields, collections, featured,
ASSETS, POSTS, likes and comments — run WITHOUT it.

An earlier revision wrapped the entire `Runner.Run`, and did have the
consequence an earlier version of this paragraph described: an unrelated
apply could wait out its whole request deadline while image files were
loading. After the narrowing that is no longer true, and the wording is
corrected rather than left standing — a stale operational warning invites
a fix for a problem that is not there.

The remaining waits are bounded by the reset and bootstrap spans, and a
collision fails CLOSED with nothing written. A `lock_timeout` on the
batch's acquisition is deliberately NOT taken: it would change the
batch's failure taxonomy, and the excessive-scope problem it would have
papered over is fixed at the source instead.

**Exempt, by construction — an in-flight batch for the affected
principal cannot coexist with these:**

| writer | why coexistence is impossible |
|---|---|
| first-boot setup assigns the initial admin role | the handler CREATES the user in the same request; a principal that does not exist cannot have a batch in flight |
| self-registration assigns the default role | same — the row is inserted immediately above, and the account cannot authenticate until it is verified |
| **server-startup** `bootstrap.Run` (`run()`) | executes after migrations and BEFORE the HTTP server accepts anything, so no request of any kind is in flight |

⭐ **One function, two call sites, two answers.** `bootstrap.Run` appears
three times. The startup call is exempt for the reason above. The two
inside `aa seed` are a different concurrency context entirely and are
covered by the seed spans in the participating table. The exemption
belongs to the CALL SITE, never to the function.

⭐ These are exemptions with reasons, not omissions. If any of them ever
gains a path that can run against a live principal, it joins the table
above.

Effective-capability semantics are unchanged: role-derived authority,
direct grants, revokes, team-scoped grants and closure inheritance all
still decide the verdict, and the decision is never reduced to raw
grant-row equality — a team-scoped ROLE assignment produces zero rows in
`user_capability_grants`, which is why `ScopedTeams` exists.

### Vocabulary

**A PREVIEW MUST NOT MUTATE.** The shipped `openOrCheckVocabulary`
mutates by design, so the preview resolves through the same index and
`resolveOrMint` with creation removed, and REPORTS mintable terms rather
than creating them.

The verdict SPLITS. Canonicalisation, unknown-slug handling and
mintability are BATCH-WIDE. The deprecated-or-archived grandfather test
needs the target's own held set — the same slug is grandfathered on a
target holding it and refused on a sibling that does not — so it is per
target and produces a target-level `refused`.

**THE MINT IS COUPLED TO A REAL WRITE.** A new term commits only if at
least one successful batch mutation actually stores it. "Preview
predicted would_change > 0" is insufficient: if every such target ends
conflict, gone, unauthorized or error, the options document stays
BYTE-IDENTICAL. The cache is invalidated once, after commit, only when a
term committed.

Preview binds the EXACT canonical slug set apply stores, which is what
prevents a PHANTOM would_change: a term that looks new but canonicalises
onto a slug the target already holds counts `no_op` at preview and stays
one at apply.

### Per-type semantic emptiness

Obtained from the shipped `valueIsEmpty`, all eleven types. FALSE is a
real boolean value and is never empty. `rich_text` is sanitised FIRST and
judged on the output, because the sanitiser strips no empty elements.

`select` and `tree` DO NOT treat whitespace like text, and the split is
the shipped behaviour rather than a batch invention: `""` returns nil
from `vocabularySlugs` and never enters the vocabulary pipeline, so it
stores as `''`, while `"   "` enters it as a slug, matches nothing, and
is refused as unknown. Nothing trims: a `text` field given `"   "` stores
`"   "`.

`fill_empties` with a semantically empty proposed value is refused
BATCH-WIDE on required AND optional fields, across every type — the mode
means "give the empty ones a value", and a value that is itself empty
makes it a contradiction. **No mode creates an accidental empty row.**

### Archive is not deletion, on BOTH planes

Only `deleted_at IS NOT NULL` removes a subject from the authority probe,
and only `deleted_at IS NOT NULL` invalidates a reference target. An
ARCHIVED asset is written, and an ARCHIVED asset is a valid reference
target.

### Audit

ONE envelope per apply, ever, committed in the same durable outcome as
the writes and the consumption. It records the initiating human, the
operation id tying preview to apply, the mode, the field, THE TRIMMED
REASON VERBATIM, the accepted confirmation count, all six partition
counts, both reconciliation equations, the outcome counts with their
sub-reasons, the terms that ACTUALLY committed, and the resolved target
ids.

**It records NO FIELD VALUE, old or new.** The audit log is read under
`system.audit.read`, a DIFFERENT POPULATION from the field's readers, so
putting values there would be a side channel around the field's own read
gate — a thousand records at a time. Every value is already in
`asset_field_value_history`, behind that gate. Unreadable and refused
targets contribute their id and a non-value-sensitive partition label
only.

Unlike the Decision's "a single undo can revert the batch", there is no
batch undo in this subset. The per-field history the Consequences
anticipate exists and is written per target; a bulk revert built on it is
not part of this amendment.

### Two narrow wire-hardening codes, discovered in implementation

`unknown_mode` and `unknown_selection_kind`, both 400. Neither is part
of the batch's product surface; both exist because nothing between the
wire and the handler validates a closed enum. There is no
spec-validation middleware, and the generated enum types are bare
strings whose `Valid()` has no callers, so an undefined `mode` or
selection `kind` arrives intact. Left unchecked, an undefined mode
matched no mode-specific arm — which let a REQUIRED field take a
semantically empty value without tripping R1, partitioned every target
as a no-op, and then died on a CHECK constraint as a 500.

They are recorded here so the enum is not re-derived without them, and
so the next reader knows they answer a shape question rather than a
product one.

### Deliberately out of scope

The single-target subject-authority gap (the ordinary field-value writer
still has no asset-level ownership check — tracked separately), the
`write_capability` team-scope fix, a foreign key on `value_ref`, a
required-bypass permission, batch undo, a generic batch clear,
collections as a selection kind or value plane, select-all-in-filter, and
the frontend selection and batch surfaces.

### Cited, not re-decided

ADR 0052, ADR 0012 (per-type emptiness and the rich-text survivor list),
ADR 0099 §§1, 5 and 8 (a hidden field is still writable; the
non-disclosure filter; the atomicity precedent), ADR 0083 §2, ADR 0092 §2
(`fields.vocabulary.extend` as a dial).

## Amendment 2026-09-06: the lock hierarchy, after a real deadlock

Sprint 20c-i's apply deadlocked against an ordinary single-target
metadata write. SQLSTATE 40P01, intermittently, on post-merge Integration
CI. It was not a test artifact and it was not contention: it was a
LOCK-ORDER INVERSION, and the edge that closed the cycle sat two trigger
levels below the statement the error named.

### The chain

```
INSERT asset_field_value ... ON CONFLICT
+- asset_field_value_search_text  (AFTER INSERT OR UPDATE, per row)
   +- rebuild_asset_search_text(asset_id)
      +- UPDATE assets SET search_text ...   -> FOR NO KEY UPDATE on
         |                                      assets[V], held to COMMIT
         +- assets_member_post_search_text  (AFTER UPDATE OF search_text,
            |                                sensitivity, status,
            |                                processing_status, deleted_at)
            +- asset_member_post_search_text_trigger()
               FOR r IN SELECT post_id FROM post_assets
                         WHERE asset_id = NEW.id      -- NO ORDER BY
               +- rebuild_post_search_text(post_id)
                  +- UPDATE posts SET search_text ->  FOR NO KEY UPDATE
                                                      on posts[P]
```

EVERY metadata write therefore takes an `assets` row and then a `posts`
row. The batch, which locked and wrote one target at a time, held a post
row from an earlier target while asking for the next asset row:

```
batch    holds posts[P]  -> waits assets[V]
ordinary holds assets[V] -> waits posts[P]
```

Two transactions, the same two tiers, opposite order. Nothing either one
did was individually wrong, which is what made the wait graph look
unclosable and is the reason this is recorded rather than fixed quietly.

### The correction

**Asset tier.** ONE statement locks the would-change subjects UNION any
proposed reference target, FOR UPDATE, ORDER BY id, before a single value
is written. It replaces both the per-target FOR SHARE probe and the
separately taken reference-target lock, and the batch never asks for an
`assets` row while holding a `posts` row again.

FOR UPDATE rather than FOR SHARE, and the reason is the same trigger
chain that caused the deadlock. Every write ends in an UPDATE of the
subject's `assets` row, which needs FOR NO KEY UPDATE, so a FOR SHARE
holder is a holder that must UPGRADE: two batches sharing one target
would each hold FOR SHARE and each block on the upgrade. FOR UPDATE is
also the weakest mode that makes a membership INSERT queue rather than
race, because a foreign key takes FOR KEY SHARE and that conflicts with
FOR UPDATE and not with FOR NO KEY UPDATE.

**Post tier.** The per-row asset-to-post propagation is SUPPRESSED inside
the batch transaction, with a `SET LOCAL` flag the trigger checks, and
the debt is paid once per DISTINCT containing post, ASCENDING, after all
writes. A thousand targets across four posts is four rebuilds. The
suppression is transaction-local by construction, so it cannot survive
into the next caller that borrows the connection.

**The shared rebuild function locks before it reads.**
`rebuild_post_search_text` takes its post FOR NO KEY UPDATE as its FIRST
statement. It bakes its document out of three reads that all precede its
UPDATE, and at READ COMMITTED blocking on the UPDATE's row lock
re-evaluates the target row and NOTHING ELSE: the local variables keep
whatever snapshot they were computed from. Without the entry lock a
transaction can compute a post document, wait, and then apply it over a
document another transaction just committed. That is reachable from a
membership removal and from the addition of a member that is not itself
being edited, and both now have regressions.

**The multi-post loops are sorted.**
`asset_member_post_search_text_trigger`, `assets_mature_sync` and
`assets_ai_provenance_sync` walk their posts ORDER BY post_id. The value
of that sort is INDEPENDENT of the batch editor: two ordinary writers,
editing two assets that belong to the same two posts whose membership
rows were written in opposite orders, deadlocked with no batch anywhere
near them. Migration 00067.

### The hierarchy, stated as an order

1. `field_definition[field]`, FOR UPDATE, row-scoped, and the shared half
   of the authority advisory lock.
2. Subjects UNION reference target: FOR UPDATE, ascending `assets.id`,
   ONE statement.
3. The field-value writes, and the asset search_text rebuild they set
   off: NO KEY UPDATE on rows already held at tier 2, and NO POST LOCK.
4. The coalesced post rebuild, ascending `post_id`, each call taking NO
   KEY UPDATE on its post at entry.
5. Vocabulary mint: FOR UPDATE on the vocabulary row, which is the
   `field_definition` row already held at tier 1.
6. The audit envelope: an append-only insert whose foreign keys take KEY
   SHARE on rows already held.

Acyclic, because the asset tier closes before any post lock is taken, no
path takes an `assets` row lock after a `posts` row lock, and every
multi-post acquirer walks ascending.

**Tier 3 holding no post lock is load-bearing and is not free.** `assets`
carries two other AFTER UPDATE triggers, `assets_mature_sync` and
`assets_ai_provenance_sync`, which both loop `post_assets` and touch
`posts`. Neither fires here only because both early-return unless
`mature`, `ai_provenance` or the nullness of `deleted_at` actually
changed, and a field-value write changes none of them. That is an
assumption about somebody else's trigger, so it is covered
BEHAVIOURALLY: with a real batch parked mid-write-phase, an independent
transaction takes FOR NO KEY UPDATE NOWAIT on every containing post and
must get it.

### What this costs, measured

At the 1,000-target four-post ceiling, on one host, 20 samples each:

| | before | after |
|---|---|---|
| apply p95, uninstrumented | 20.2 s, of which 18 runs sat at 3.5 s and two cold ones at 20 s and 22 s | 0.83 s, every run within 35 ms of the median |
| apply p95, under `-race` | 3.8 s | 1.19 s |
| ordinary write to another field ON A BATCH TARGET, during the batch | 7 ms | 19 ms in one run, 829 ms in another |

The apply got several times faster, and the spread collapsed. Both follow
from the same change: coalescing turns one 250-member post-document
rebuild PER WRITTEN ROW into four for the whole operation, and the two
cold outliers in the pre-correction run were that work meeting cold
statistics.

The contention figure moved the other way, and it is now a coin flip
rather than a constant. That is the intended trade and the honest
description of it: the batch holds every target FOR UPDATE for the length
of the operation, so a concurrent write to a TARGET either starts before
the batch reaches its lock pass and passes through, or arrives after and
waits. A write to an asset the batch is not editing is unaffected either
way. The ceiling test only LOGS this number; it asserts that the
concurrent write does not error and that its value lands, and that
contract is unchanged.

The old line in this ADR, "a concurrent ordinary single-target write to
an unrelated field completing in 4 ms", was doubly wrong. The figure is
not reproducible, and "unrelated field" was never the relevant variable:
the trigger chain runs identically whatever field was written, so the
contention was always about the SUBJECT and its posts and never about
the field.

### Preserved unchanged

The validation precedence and the zero-change rules above are untouched
by this work and are not re-decided here. A `would_change == 0` apply is
still a REAL operation: it still runs the field-definition seam and the
authority serialization, still consumes its token, and still writes
exactly one envelope. What N = 0 removes is only the work there is none
of, namely subject rows to lock and post documents to rebuild, and it is
NOT a bypass: a proposed reference target still joins the asset tier's
lock set, so a reference that has gone still refuses batch-wide.

### Out of scope, and recorded rather than fixed

`recompute_post_mature` and `recompute_post_ai_provenance` share the
read-before-lock shape corrected in `rebuild_post_search_text`. The batch
never changes their inputs, so there is no freshness interaction with
this work and they are left alone. They are named here so the next reader
does not have to rediscover them.

## Alternatives considered

- **CLI-only bulk surface.** Doesn't help the reviewer / coordinator
  audience. Rejected.
- **Reuse search export only, no UI selection.** Doesn't solve bulk
  *edit*, only bulk *export*. Rejected — incomplete.
- **Per-action separate pages.** The older DAM pattern; the UX is
  dated. The modern pattern is one floating action bar over the existing
  browse view. Adopted.

## Reference

- Phase 1.27 in [`docs/roadmap.md`](../roadmap.md).
- Search engine (Phase 1.12 Search 2.0) supplies the selection-by-
  filter mechanism.
- Job queue (Phase 1.18.A — shipped) executes bulk actions.
- License tier governs bulk concurrency + selection ceiling via the
  tangled-derivation model in ADR 0017.
