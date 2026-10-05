---
id: "0099"
title: A field can say when it appears, and that is composition rather than authorization
status: accepted
date: 2026-09-03
area: architecture
phases: []
supersedes: []
related:
  - "0012"
  - "0083"
  - "0092"
  - "0093"
tags:
  - metadata
  - fields
  - forms
  - security
excerpt: >-
  An operator could describe when a field should appear and nothing evaluated it,
  and the surface that would have evaluated it was receiving field values the
  caller was not entitled to read. This settles both: a stored condition is a
  form-composition hint with a whole-condition fail-open rule, the parser is one
  grammar with two conformant implementations, and the composition read path is
  required to derive readability on the server and to withhold the values behind
  it.
---

## Context

`field_definition` describes what a field is. Until now it could not describe **when the field
should be offered**. An operator running a commissions pipeline has a `commission_deadline` that
only means anything when `work_type` is `Commission`, and the only ways to express that were to
show the field to everybody always, or to not create it.

Sprint 18b added `edit_tab` for the coarser half of the same problem, a way to say which tab of an
edit surface a field belongs to. It was stored and validated and **had zero consumers**: no surface
in the product read it, so an operator could fill it in and nothing anywhere changed.

The two gaps are one gap. Both are the operator's intent about **form composition**, and neither
reached a person.

There is a third thing tangled with them, and it has to be settled first because building on it
without settling it would be a security regression rather than a feature. `GET /assets/{id}/fields`
contained **no per-field read check at all**: its only gate was a 401 on an anonymous caller, so any
authenticated caller received the values of every field on the asset including fields carrying a
`read_capability` they did not hold. `GET /collections/{id}/fields` did filter, but only against
GLOBALLY held capability codes, and it filtered by **dropping the row**, which makes a value that was
withheld and a value that was never set arrive as the same nothing.

A conditional-visibility engine reading either of those would be an oracle over protected metadata.
The dependent field's visibility is observable, the condition is stored, so watching whether the
dependent appears reads out the controller's value. That is why the readability rules below are part
of this decision and not a follow-up to it.

## Decision

### 1. `display_condition` is a form hint, never authorization

A nullable `jsonb` column on `field_definition`. When present it is a **JSON array of bare
`<code><op><value>` strings** with no `field:` prefix, combined with **AND**. `NULL` is the canonical
unset and the only representation of it: the CHECK constraint refuses anything that is not NULL and
not a non-empty array of non-empty strings, so `[]`, `{}`, `""` and JSON `null` are all rejected at
the storage layer.

**Nothing about access depends on it.** A field hidden by a condition keeps every value it had, keeps
its `read_capability` and `write_capability`, and can still be read and written through
`PUT`/`DELETE /assets/{id}/fields/{field_id}` exactly as before. This puts `display_condition` in the
same class as `display_order`, `display_group`, `show_on_card` and `edit_tab`: a client that ignores
it entirely is still correct, merely plainer. Recorded here because the opposite reading is the one
that would be reached for first, and because a composition rule mistaken for an access rule is a
security hole with an innocent-looking configuration screen in front of it.

It is **update-only**. `display_condition` and `clear_display_condition` appear on
`FieldDefinitionUpdate`; neither appears on `FieldDefinitionCreate`. That follows the shape the other
six participation properties already have (`edit_tab`, `read_only`, `regexp_filter`, `show_on_upload`,
`show_in_advanced_search`, `show_on_card`): a field is created, and then configured. It also removes
an entire ordering problem, since a condition names other definitions and a create body cannot
reference a graph that does not exist yet.

PATCH semantics are the `edit_tab` semantics, third of their kind after `clear_default` and
`clear_regexp_filter`:

| body | effect |
|---|---|
| neither member sent | unchanged |
| `display_condition` sent | **replaces the whole array**, never merges |
| `clear_display_condition: true` | sets SQL NULL |
| both sent | 400 |

Whole-array replacement rather than a merge, because a condition is one predicate and not a bag of
independent settings. There is no way to express "add a term" and that is deliberate: an operator
editing a condition is editing a sentence, and a partial edit of a sentence is how you get one that
says something nobody wrote.

### 2. One grammar, two implementations, and a fixture that holds them together

Terms use the **existing** `field:` term grammar, `facet.SplitFieldTerm`. **The search grammar is not
extended.** Only `field:`-class terms are admissible; other facet dimensions are refused at
configuration time.

That function is the authority and it has five properties that all matter, none of which is
guessable from looking at a term:

1. It splits on the **first** character from `=~<>`, and matches operators **longest first**, so
   `>=` is never read as `>` followed by a value beginning `=`.
2. Later operator characters stay **in the value**: `role=a=b` parses to the value `a=b`.
3. The **code is lowercased and trimmed**.
4. The **parsed value is trimmed**.
5. Nothing trims or case-folds the **stored** value, on either side of the comparison, ever.

The consequence of (4) and (5) together is the one an operator will hit, so it is stated as its own
rule: **a condition literal is trimmed and a stored value is not.** A stored value of `" Commission "`
does **not** match the condition `work_type= Commission `, because the condition's literal parses to
`Commission` and the stored value is still six characters longer. This is asymmetric on purpose.
Trimming the stored value would mean the evaluator disagreed with `=` everywhere else in the product,
including the search predicate the same grammar drives.

`=` compares the parsed literal to the stored value **exactly and case-sensitively**. `~` is a
**case-insensitive substring** match of the parsed literal against the stored value.

**The browser cannot call `SplitFieldTerm`.** It is Go. So there are two implementations of one
grammar, which is the shape that drifts, and the mitigation is the one sprint 20a used for the
emptiness predicate: **a single shared fixture corpus that both suites read**,
`web/src/lib/displayCondition.cases.json`, consumed by `app/internal/metadata/display_condition_parity_test.go`
and `web/src/lib/displayCondition.test.ts`. Adding a case to that file adds it to both planes at once,
and a change that moves one plane fails on the other plane's test rather than being noticed months
later by somebody looking at a field that will not appear.

### 3. The operator and type matrix, and why `boolean` stays out

| type | `=` | `~` |
|---|---|---|
| `text` | yes | yes |
| `longtext` | yes | yes |
| `select` | yes, against the stored slug | no |
| `tree` | yes, against the stored slug | no |
| `multi_select` | yes, as **membership** | no |
| `rich_text` | no | no |
| `boolean` | no | no |
| `number`, `date`, `datetime`, `reference` | no | no |

`>=` and `<=` are **refused for display conditions** in every pairing. They exist in the search
grammar as the two ends of an ordered range, and a range bound is a filtering question rather than a
form-composition one.

`multi_select` is membership rather than equality, and that is the only place the same operator means
two things. `role=Illustrator` on a `multi_select` is true when `Illustrator` is one of the stored
terms. Equality against the whole set would make a multi-valued field usable as a controller only when
it held exactly one value, which is not a rule anybody would write down.

`rich_text` is excluded for `regexp_filter`'s reason: what is stored is server-sanitised HTML, so a
condition would be matched against markup rather than against anything the operator can see.

**`boolean` stays excluded, and this is the exclusion most likely to be revisited by mistake.** Sprint
20a gave `boolean` a three-state control, which changed the CONTROL and not the REPRESENTATION: a
boolean is still `value_num` 1 or 0 (ADR 0012's third 2026-07-31 amendment), and the server still
handles `"number"` and `"boolean"` together on the query path. Admitting it here would require the
search engine to learn a boolean predicate. **Do not expand search semantics to widen this table.** If
`boolean` is wanted as a controller, that is a search-engine decision first and a composition decision
second.

### 4. Conditional visibility is literal, and unevaluable fails OPEN as a whole

**FALSE means the control is not rendered.** Not read-only, not collapsed, not a placeholder. There is
no replacement affordance, because a replacement affordance is a way of telling a person about a field
the operator has said is not relevant to them.

The consequences of that, stated individually because each has been implemented wrongly somewhere by
somebody:

- The **persisted value is untouched**. Hiding generates no Set, no Clear, and no empty row.
- **Reveal restores the persisted value**, byte for byte.
- A hidden dependent's **unsaved draft survives** and reappears on reveal, and is not submitted while
  hidden.
- A hidden `required` field **creates no new completeness gate** on any surface. This follows directly
  from ADR 0012's 2026-09-02 amendment: `required` is a rule about a WRITE, enforced by the four
  field-value handlers, and R1 still refuses an API clear of a required field whether or not a form
  happens to be drawing it. Asset creation still requires nothing at all.
- A controller that is itself hidden by its own condition **still contributes its in-form value**.
  Visibility is a rendering decision and does not remove a value from the model.

**Runtime cardinality:**

| N | behaviour |
|---|---|
| 0 (`NULL`) | visible exactly as today, no evaluator side effect, no submission difference |
| 1, resolvable and readable | true shows, false hides; **both arms** |
| N >= 2, all resolvable and readable | conjunctive: all true shows, any false hides |
| N >= 2, **any** term unevaluable | **whole condition fails open, the dependent is SHOWN** |

**The whole-condition rule is the part that is easy to get wrong, so here is the wrong version.** The
tempting implementation treats an unevaluable term as `true` inside the AND. That is not the same
rule, and the two differ on exactly the cases that matter: with `A = false` and `B` unevaluable,
"unknown counts as true" evaluates `false AND true` and **hides** the dependent. The rule is that once
any term cannot be evaluated, **the condition has no verdict at all** and the dependent is shown.
Nothing about the remaining terms is consulted.

A term is unevaluable when the controller's definition is **missing or unresolvable**, or when the
controller is **unreadable by this caller on this subject**.

**A readable controller with genuinely no value is a real FALSE and still hides.** Absence is an
answer. Only unevaluability fails open, and "I am allowed to look and there is nothing there" is not
unevaluability. The mirror trap is worth naming: an implementation that treats every absent value as
unknown never hides anything, and reads as green against every test that only checks the true arm.

### 5. The composition read path must derive readability on the SERVER and withhold the value

This is a security acceptance rather than an evaluator feature, and it has **two halves that are both
required**.

**(1) Trustworthy effective readability, derived server-side** for the current caller and the current
subject. The browser cannot derive it. `auth.caps` carries **globally held codes only**, so a
team-scoped grant is invisible there, and the asset edit page's own source said so before this sprint
began. A client that inferred readability from `auth.caps` would decide that a caller who holds
`meta.read.finance` scoped to the team that owns the asset **cannot** read the field, which is both
wrong and wrong in the direction that hides working fields from the people they were configured for.

**(2) Non-disclosure of stored values the caller cannot read.** The protected value must not cross the
wire at all.

Four things are explicitly **not acceptable** as implementations of this:

- sending the value alongside a `readable: false` flag,
- sending the value and dropping it in the client,
- inferring authority from `auth.caps`,
- treating the presence of a value as proof that the caller may read it.

The last is the subtle one and it is why the two halves are separate requirements. If readability were
inferred from whether a value arrived, then "withheld" and "never set" would be the same signal, and a
readable-and-unset controller (a real FALSE, which hides) would be indistinguishable from an
unreadable one (unevaluable, which shows). **That is precisely the state `GET /collections/{id}/fields`
was already in**, which is why the collection surface needed the new state as much as the asset surface
did, despite already filtering.

**Effective readability is computed as:** no `read_capability` means readable; otherwise the caller must
hold the capability globally, **or** hold it scoped to the subject's team where the subject has one. An
asset carries `team_id`. A collection does not carry a team at all, so on a collection a team-scoped
grant confers nothing and the global holding is the whole answer. That asymmetry is not new, it is the
shape `canMutatePost` and `canTransitionAssetStatus` already have, and `collections` having no
`team_id` is recorded in `collections/handler.go` as a deliberate absence.

**`/create` is different and must not be made the same.** It has no pre-existing stored subject values,
so its conditions evaluate against **local pending values** in the form. There is no server fetch of
protected existing values for create-time evaluation, because there is no subject to fetch them from
and inventing one would build the oracle this section exists to prevent.

### 6. Configuration validation is a closed refusal list, not a solver

Refused at configuration time:

- a malformed term
- an unsupported operator and type pairing
- an unknown local controller code
- a subject-kind mismatch between dependent and controller
- a **mirrored dependent** (assigning a condition to a `mirrors_column` definition)
- a **mirrored controller** (a term naming one)
- a **self-cycle**, a 2-cycle, or an N >= 3 cycle
- an **empty total N-way applicability intersection**
- **distinct `=` literals on one single-valued** `text` / `longtext` / `select` / `tree` controller
- a controller that is **already archived** at configuration time

Allowed:

- duplicate identical terms
- distinct `=` membership terms on one `multi_select`, because a multi-valued field can genuinely hold
  both
- cross-operator contradictions beyond the defined minimum set

**There is no general predicate solver and there will not be one.** The contradiction check has a
deliberately small closed shape: two `=` terms with different literals on one controller that can only
hold one value can never both be true, so the condition is inert and an operator has almost certainly
made a typing mistake. Beyond that the answer is "the field will not appear", which is a thing an
operator can see and fix. A solver would refuse configurations that are merely unusual, and it would be
a second place where the meaning of a condition is decided.

**Mirrored definitions are excluded in both directions.** `title` and `description` are views onto
columns of `assets` that carry a second human write plane (`POST /assets`, `PATCH /assets/{id}`), and
they already have first-class controls on every surface that hosts them. They are not tabbed, not
conditioned, and their write plane is unchanged.

**N-way applicability is intersected across all controllers, not checked pairwise.** Intersect the
dependent's `applies_to` with **every** controller's, treating empty (global) as universal, and refuse
when the running intersection is empty. Pairwise is insufficient and the counterexample is small enough
to state: with dependent `D {t1,t2}`, controller `A {t1}` and controller `B {t2}`, `D` plus `A` is fine
and `D` plus `B` is fine, and **`D` plus `A` plus `B` must be refused** because there is no asset type
on which all three appear together. A pairwise implementation accepts it and stores a condition that
can never be true. `applies_to` is an asset-side concept; collection fields are not scoped by it.

**Cycle detection walks the whole graph for that subject kind.** Not just the immediate edge. `A -> B`,
then `B -> C`, then `C -> A` closes the cycle only on the third write, and the first two are legitimate
configurations that must be accepted.

### 7. Controller status, and why later drift is not a rewrite

| controller status at configuration | outcome |
|---|---|
| `active` | allowed |
| `deprecated` | **allowed** |
| `archived` | **refused** |

`deprecated` is allowed because the edit surfaces deliberately render active and deprecated definitions
together (the #528 rule): a deprecated field is one an operator stopped wanting NEW values in, and
records that already hold one must keep showing it. `/create` is active-only, so a deprecated controller
is simply unresolvable there and the dependent fails open, which is the correct outcome and not a bug.

`archived` is refused because an archived definition never appears on any composition surface, so the
condition would be permanently inert from the moment it was written: stored, valid-looking, and
guaranteed to fail open forever.

**Archiving a controller AFTER a valid configuration does not rewrite anything.** The stored
`display_condition` is retained verbatim, the dependent fails open at runtime, and **if the controller is
restored to a composition-eligible status, ordinary evaluation resumes**. Configuration is a record of
what an operator decided; runtime status is a fact about today. Rewriting stored configuration because a
runtime fact drifted would destroy the operator's intent to make a transient state tidier, and it would
make un-archiving a lossy operation. For the same reason an **archived dependent keeps its
configuration** rather than being cleared.

### 8. The cycle invariant is atomic, or it is not an invariant

**Evaluating the cycle precondition and mutating `display_condition` must be atomic with respect to
competing `display_condition` writes on the same subject-kind graph.**

Without that, the check is theatre in exactly the case it exists for. Two operators, one writing
`A -> B` and one writing `B -> A`, each validate against a graph in which the other's edge is not yet
visible. Both pass. Both commit. The graph now has a 2-cycle that no single write could have created,
and every subsequent validation walks it.

The mechanism is a **transaction-scoped advisory lock keyed on the subject kind**, taken before the
graph is read and held until the write commits. It is taken only when a request actually touches
`display_condition`, so ordinary field edits are not serialised behind it, and it does not conflict with
the row locks unrelated updates take.

Advisory rather than `SELECT ... FOR UPDATE` over the subject kind, for two reasons. The invariant is a
property of the **whole graph** and not of any one row, so there is no single row whose lock expresses
it, and locking every definition of a subject kind would block unrelated field edits for the duration of
a graph walk. `LockFieldDefinitionVocabulary` uses `FOR UPDATE` for the read-modify-write of one row's
options document, which is the case where a row lock IS the right shape; this is the other case.

**The invariant is the graph invariant and nothing more.** It does not extend to `applies_to`, `status`
or `type` changing under a stored condition. Those are later drift, and later drift is handled at runtime
by failing open.

### 9. Tabs derive from DEFINITIONS, and a tab emptied by a condition keeps its chrome

`edit_tab` gets its first consumer. Tabs are **coarser than `display_group`**: a `display_group`
fieldset lives INSIDE a tab, and `display_order` remains the ordering source. ADR 0092 section 3 already
decided the property itself and is not amended.

**Effective buckets** are every distinct non-null `edit_tab` among **composition-eligible definitions**,
plus one DEFAULT bucket if any composition-eligible definition has `edit_tab = NULL`:

| configuration | buckets | strip |
|---|---|---|
| no composition-eligible fields | 0 | none, and no phantom default tab |
| all unassigned | 1 default | none |
| one named only | 1 | none |
| one named plus unassigned | 2 | yes |
| two named | 2 | yes |
| two named plus unassigned | 3 | yes |

The default bucket is selected first. **No unassigned field may disappear**, which is what the default
bucket exists to guarantee. Order is: default first, then named buckets by their **minimum member
`display_order`**, then by tab name as the tiebreak, so the strip is stable across reloads.

**Policy B, and it is mandatory: buckets derive from composition-eligible DEFINITIONS, not from
currently visible controls.** If conditions hide every field in the selected tab, **the tab remains, the
selection remains, an empty-state line appears, drafts survive, and nothing hidden is submitted.**

The alternative, deriving the strip from what is currently visible, was rejected because it makes the
navigation itself move while a person is using it. A tab would vanish from under the selection as a
side effect of typing in a different tab, and the person's own unsaved work in the vanished tab would
have nowhere to be. Policy B costs one empty-state line and buys a strip that does not move.

On `/create`, **asset type is the OUTER axis**: asset type, then edit-tab bucket, then `display_group`,
then `display_order`. **Same-named tabs from different asset types never merge.** A "Print" tab on
`image` and a "Print" tab on `document` are two operator decisions about two different kinds of thing,
and merging them would file a field under a heading its own asset type never mentioned. `/create` remains
active-only and the two-action guarantee is untouched.

### 10. Import is specified and not built

**There is no field-definition import path today.** Zero import operations, zero Go importers; the only
writers of a definition are `CreateFieldDefinition` and PATCH, and ADR 0083 records that federation
transports no metadata at all. **This sprint builds no import runtime.** What follows is specification
for whoever does.

- A **missing imported controller is preserved verbatim** and fails open at runtime. An import must not
  silently drop a term because the receiver does not have the field yet.
- **Resolvable imported terms obey the ordinary invariants.** An import whose definitions all resolve
  must not be able to create a configuration that PATCH would have refused. Import is not a back door
  around validation.
- **Mixed conditions validate the resolvable edges and preserve the unresolved ones verbatim.**
- **Deferred for an unresolved edge:** cycle participation and N-way applicability contribution. An edge
  pointing at a definition that is not here yet cannot be placed in the graph or intersected against.
- **Whoever later resolves or re-imports those edges must confront the deferred checks**, because that is
  the moment the deferred question becomes answerable and the moment a cycle could first close.
- **Bulk import inherits the atomicity requirement** of section 8. A batch that writes many conditions is
  the case the invariant was written for, not an exception to it.

`display_condition` is **classified IN** under ADR 0083: it describes the field rather than the server,
which is the same test `regexp_filter`, `required` and `show_on_card` pass. Its terms name other FIELD
CODES, which are federation-stable slugs, so the reference survives transport even when the referent does
not arrive with it. See the ADR 0083 amendment of this date.

## Consequences

**Migration `00065`** adds one nullable `jsonb` column and one shape CHECK. No default, no index (nothing
queries by it), no backfill. Every existing definition reads `NULL`, so every existing form composes
exactly as it did.

**The API grows one property and two operations.** `FieldDefinition.display_condition`,
`FieldDefinitionUpdate.display_condition` plus `clear_display_condition`, and
`GET /assets/{id}/field-composition` and `GET /collections/{id}/field-composition` returning per-field
effective readability and **no values at all**. The shape carries no value members, so non-disclosure is
structural rather than a rule somebody has to remember: there is no member for a secret to be put in.

**`GET /assets/{id}/fields` now filters by effective readability**, which it never did. This is a
narrowing and it closes the oracle described in the Context. Two of its three frontend consumers wanted
the filter already: the asset edit page rendered controls for values the caller could not read, and the
post detail host displayed them. Its sibling `GET /collections/{id}/fields` keeps filtering and switches
to the same shared helper, so the two subject kinds cannot drift on what "readable" means.

**A condition is cheap to evaluate and free when absent.** `NULL` short-circuits before any parsing, so
an install that never configures one pays nothing.

**The operator can now build a form that lies to itself.** A condition naming a controller in a tab the
person has not opened still evaluates, because the model is one form and tabs are a view of it. That is
deliberate: the alternative is a condition whose truth depends on which tab is selected, which is not a
property anybody can reason about.

**What this does not do.** It does not extend the search grammar, does not change `searchable`, does not
touch R1 or R2, does not alter the mirrored write plane, does not build a broken-reference warning UI,
and does not implement any part of federation.
