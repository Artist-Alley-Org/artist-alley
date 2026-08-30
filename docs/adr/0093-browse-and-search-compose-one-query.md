---
id: "0093"
title: Browse and search compose one query — a filter is a filter wherever it appears
status: accepted
date: 2026-08-19
area: architecture
phases: []
supersedes: []
related:
  - "0092"
  - "0064"
tags:
  - search
  - browse
  - visibility
  - filters
excerpt: >-
  A filter chosen on the feed and the same filter chosen on the advanced page
  produce the same query through the same engine and the same gates — so a new
  filter is written once, and no surface can grow a second, weaker copy of a
  visibility rule.
---

## Context

Browsing and searching are the same question asked two ways: browse is a query
with nothing specified, advanced search is a query built deliberately. The
codebase half-knows this — search runs through `search.Engine` with one query
representation, while the browse feed is its own endpoint with its own
parameters.

That fork was harmless while browse had no filters. It stopped being harmless
in v0.10.1, when the feed gained a type filter (#1166): the filter had to be
implemented on the feed endpoint, with its own visibility conjunct, because the
feed does not go through the engine. It worked — and it also meant the rule
"a withheld cover matches no kind" had to be written into a second place, where
it could have been forgotten.

The remaining filter arms (tag, date range, and the operator-defined metadata
fields of ADR 0092) would each repeat that: two implementations, two visibility
compositions, two chances to diverge. Every gate this project has had to fix
twice — the derived-copy family, the display-name opt-out, the mature axis —
was a rule that existed in more than one expression.

## Decision

**1. A filtered feed is a search.** When the browse feed carries any filter
beyond its scoping parameters, it composes the query through the search engine.
Unfiltered browse may keep its direct path for cost reasons; filtered browse
does not get a parallel filter implementation.

**2. Scope and filter are different things.** Which *collection*, *team*, or
*follow set* you are looking at is scope — it selects the corpus. What you then
narrow it by is a filter — it belongs to the query. Scope stays on the feed;
filters compose.

**3. A filter is defined once.** Adding a filterable dimension means adding it
to the query grammar. Surfaces render controls for it; they do not implement it.

**4. Visibility composes inside the query, never around it.** Every filter
conjunct sits inside the engine's gated composition, so a value the reader
cannot see can never be used to select — the property v0.10.1 proved matters
when a withheld cover would otherwise have been recoverable by asking for each
kind in turn.

## Consequences

- The feed endpoint's filter parameters become a thin translation into the
  query grammar rather than a second query builder. `?kind=` shipped as the
  standalone version; it converges here.
- **Caching gets one story.** The engine's result cache already folds caller
  identity, capabilities and the mature axis into its key, and correctly
  refuses to cache what it cannot key (capability-gated field filters). A
  filtered feed inherits that instead of needing its own answer.
- The performance question is real and bounded: unfiltered browse is the hot
  path and keeps its direct query; the engine's cost applies only when someone
  actually filters.
- Federation benefits by construction — a query that travels is one grammar,
  not "the feed's parameters plus the search's".
- ⚠️ **This is a refactor with a wide blast radius**, so it lands filter-arm by
  filter-arm rather than as one flag day: each new arm is written against the
  engine, and the standalone `kind` implementation converges when the second arm
  arrives, with the badge-agreement and no-probe assertions carried over.

## Alternatives considered

**Leave browse and search separate and duplicate each filter.** Rejected: it is
the status quo, and its cost is exactly the class of security bug this project
has spent three releases eliminating — a rule with two expressions eventually
has two behaviours.

**Route unfiltered browse through the engine too.** Rejected for now: it adds
engine cost to the most-hit path in the product for no behavioural gain. The
door is left open — if the engine's unfiltered path ever measures equal, the
distinction in decision 1 can go away.

**Make the feed's parameters the canonical grammar and teach search to speak
it.** Rejected: the feed's parameters are positional and scope-shaped; the
search grammar already expresses conditions, negation and composition, and is
the one federation and smart collections (ADR 0009, #1194) both need.

## Amendment — 2026-08-20, after #1165 (PR #1244)

Two things this ADR did not say, one of which was quietly false for as long as it has existed.

**1. The grammar now carries OPERATORS, not only equality.** Decision 3 says adding a filterable
dimension means adding it to the query grammar; #1165 extends that to the *value* grammar. A field
term is now `code<op>value` — contains and date-range join equality — with the separator characters
drawn from outside the field-code alphabet so the operator is found by scanning rather than by a
second delimiter. It lives in the shared grammar, so the rail, the DSL, a saved query and the URL
all mean the same thing by it. An unknown or malformed operator fails **closed** at parse time; it
never degrades to equality, because a filter that quietly matches everything is worse than one that
errors.

**2. ⛔ HOW MULTIPLE TERMS COMBINE — the rule this ADR never wrote down, and the code got wrong.**

Decision 4 calls every filter a **conjunct**. For the `field:` dimension that was **false**.
`Selection.SQL` grouped terms by `FacetType`, and `field:` is a *single* `FacetType` holding a whole
family of dimensions — so two terms naming **different fields ORed**, and adding a filter made the
result set **larger**. Measured on the real corpus before and after the fix:

| | `color_space=sRGB` | `version=v2` | both |
|---|---|---|---|
| before | 907 | 596 | **1191** — exactly the union |
| after | 907 | 596 | **312** — the intersection |

`907 + 596 − 312 = 1191`, so the pre-fix query was returning precisely the union.

It stayed invisible because every single-filter test and every manual check passes either way. It
became load-bearing the moment #1165 shipped: **a date range is two terms on one field**, so under
the old grouping a June range returned 74 rows instead of 6.

**The rule, stated so it cannot drift again:**

> - Terms on the **same field with the same operator** combine with **OR** — they are a value list.
> - Terms on **different fields** combine with **AND**.
> - Terms in **different dimensions** combine with **AND**.
>
> Implemented as a sub-group key of **(code, operator)** within a dimension. Every dimension other
> than `field:` has exactly one sub-group, which is the shape this loop had before #1165.

**Second arm, 2026-08-21 (#1242, PR #1250).** The `ai` dimension is the first filter added to the
grammar since this rule was written, and it exercised it: two `ai:` terms combine with **OR**
(a post has one purity state, so AND returns nothing forever), and because `pure`/`not_pure`
partition the corpus, both terms together are equivalent to no filter — asserted rather than
assumed. Implementation also found that `query.go:954` **discarded** the rendered fragment for
collections (`if _, _, ok :=`), which was inert only while no dimension was satisfiable there.
⚠️ **Decision 3 says a filter is defined once; that is not the same as it being APPLIED
everywhere it is defined.** Check both when adding an arm.

**The lesson worth carrying past this ADR:** a composition rule that is never written down is not
a decision, it is whatever the code happened to do — and *singular right, plural wrong* is invisible
to any test that uses one of the thing. When a dimension can appear more than once, state its
combination rule here and assert the N≥2 case with the arithmetic written out (`both < min(a, b)`),
never `both > 0`, because a count assertion that passes on a union passes on the bug.

## Amendment — 2026-08-22, after implementing the first arm (#1251 slice 1, PR #1252)

**Decision 1 is unimplementable as literally worded, and this amendment fixes the wording rather
than the decision.** It says a filtered feed *"composes the query through the search engine"*.
Read as "executes via `Engine.Run`", that destroys browse:

- `search/query.go:766` orders by `score DESC, id DESC`, where score is
  `ts_rank_cd(search_text, plainto_tsquery('english', $1))`. **Text-less, every post scores 0**, so
  the ordering collapses to `id DESC` — the wall would be sorted **by UUID**.
- The feed orders by a `(posted_at, id)` **keyset** (`posts/list_page.go:184`), where the `ORDER BY`
  and the cursor predicate are deliberately *"the SAME fact stated"* — chronological browse with
  stable pagination.

**What crosses is the GRAMMAR, not the executor.** That is what decision 3 already says (*"a filter
is defined once… surfaces render controls, they do not implement it"*) and what the Consequences
describe (*"a thin translation into the query grammar"*). Decision 1 should be read as: **a filtered
feed composes its filters through the shared filter grammar, and keeps its own ranking, cursor and
scope.** Ranking and pagination are properties of the surface; a filter is not.

**⭐ The first arm found the real seam, and it was not the one anyone predicted.** The obstacle was
never scope — the feed still binds its scope parameters unchanged, exactly as decision 2 requires.
It was the **caller**. `kind` is the first dimension whose *predicate* needs one, because the read
rule is a conjunct on each candidate **member**, inside the correlated `EXISTS` the renderer builds;
hoisting it up to the post is precisely the implementation the no-probe leak test exists to fail.

This file had recorded that as impossible (`facet/selection.go:56-61`): *"dimensionSQL is
caller-blind by design … there is nowhere to put the caller's identity without changing the arity
for every dimension."* That argument is sound for a whole-query authorization (`Selection.Authorize`)
and cannot hold for a per-member conjunct. `Selection.SQL`/`dimensionSQL` now take a
`facet.RenderContext`, and the claim is corrected at the source.

⚠️ **The safety property that makes that widening acceptable, and it must survive any future
change:** `visibility.Caller`'s zero value is `{UserRef: 0, IsAnonymous: false}` — **"user zero",
which is WIDER than anonymous**, because the anonymous branch of the field plane adds status
conjuncts this one skips. A dimension that read a forgotten `RenderContext` would therefore fail
**OPEN**. So a dimension needing a caller requires `RenderContext.CallerArg` to be non-empty and
returns `ok=false` without it (`selection.go:1136-1141`), which `Selection.SQL` turns into "this
entity matches nothing" — the fail-closed direction the rest of the file takes.

**Corollary for the remaining arms.** `Tag` and `Visibility` (slice ii) are caller-independent
predicates, so they do not need a `RenderContext` — but any *future* dimension that does must add
the same refusal, and the no-probe assertion must be carried onto **every** surface the dimension
reaches, not just the one that motivated it. Slice 1 added `search/kind_filter_test.go` for exactly
that reason.

## Amendment, 2026-08-29, after #1368 (sprint 18a)

Decision 3 says a filter is defined once. This amendment says what "once" means when the
query has to be **written down and read back**, because a saved search is exactly that, and
until now it could not be.

**The defect it was written for.** A reader narrowed a search on the rail, saved it, and the
saved search replayed **wider**: every facet filter was silently dropped, so the digest they
were emailed contained hits their own search excludes. The bridge that folds a compiled DSL
into a `facet.Selection` had been built and documented for this exact case; the save path
never fed it, because the query expression was the only thing that travelled.

### 1. The canonical serialization, and the contract it establishes

A `facet.Selection` now has a canonical DSL spelling: every term written **exactly once**,
sorted by (field, value) so a set has one string, values quoted by a rule **derived from the
lexer** rather than from a list of characters. That last part is decision 3 applied to the
serializer itself. A hand-listed set of delimiters is a second copy of the word-run grammar,
so the serializer instead lexes its own candidate token and keeps it bare only when one
`TokWord` carrying the original bytes comes back. Whatever the lexer starts treating specially
next is covered on the day it changes.

> **Every constraint present in a savable interactive search is represented exactly once in
> canonical DSL and reconstructs the same effective `facet.Selection` when replayed.**

⛔ **The guarantee is `Selection -> DSL -> Selection`, and NOT arbitrary boolean-DSL
equivalence.** The compiler flattens filter terms into one set regardless of the `AND` / `OR`
they were written in, so `extension:png OR extension:jpg` and the same pair under `AND` compile
identically. That is not a defect being papered over: it is structurally the shape a Selection
holds, where combination is a property of the **dimension** (the 2026-08-20 amendment above)
rather than of the syntax. A hand-written boolean expression over filters is not something the
round trip promises to preserve, and nothing should be built as though it were.

### 2. The per-dimension classification, stated because the code had it wrong

The 2026-08-20 amendment wrote down how multiple terms combine. It did not write down which
dimensions can *carry* multiple terms, and four of them could not: `owner`, `sensitivity`,
`asset_type` and `extension` were plain assignments in the compiler, so `extension:png AND
extension:jpg` compiled to `jpg` and lost the first term with no error and no log line. The rail
lets a reader tick both and the facet layer ORs them, so *singular right, plural wrong* was
reachable from a click.

| dimension | multiplicity | combination |
| --- | --- | --- |
| `tag` | many | **AND**: the entity carries every tag asked for |
| `extension`, `sensitivity`, `asset_type`, `owner` | many | **OR**: one value per entity, so AND is unsatisfiable |
| `field` | many, a **family** of logical dimensions | (code, operator) sub-groups: same code + same op OR, otherwise AND |

`field:` values stay **opaque `code<op>value` tokens** through the DSL. `SplitFieldTerm` and
`CanonicalValue` remain the single authority for what a code and an operator mean, for the same
reason `facet/selection.go` gives for re-splitting rather than threading: one parse function with
one set of rules keeps grouping and rendering from disagreeing. The consequence is that a future
operator needs **no DSL change at all**.

⛔ **A dimension the DSL cannot spell is a refusal, never a drop.** `ai`, `kind`, `collection`
and `visibility` are registered and have no savable producer; if one reaches the serializer the
save fails loudly. Silently omitting a ticked term would persist a query wider than the page it
came from, which is this issue again with a different dimension.

### 3. The composition rule: the expression is ONE parenthesised operand

`AND` binds tighter than `OR` in this grammar, so conjuncting a filter onto a saved expression
by appending it re-associates every top-level disjunction:

```
saved:  cat OR dog        + extension:png
naive:  cat OR dog AND extension:png   parses as  cat OR (dog AND extension:png)   WRONG, wider
here:   (cat OR dog) AND extension:png                                             correct
```

The expression is carried through **opaquely** and wrapped. Nothing parses it and nothing needs
to; the only claim is that adding the selection cannot change what the expression already meant.

### 4. ⛔ Two things the analysis missed, and both were load-bearing

**The text half of the query is reconstructed from the AST, not from the stored string.**
Nothing executes the compiler's `TSQuery`; its only readers test it for emptiness. What runs is
`plainto_tsquery` over `Query.Text`, and both DSL callers set that to the **whole DSL string**.
That was survivable while a saved query was a bare phrase, since English stop-wording eats `and`,
`or` and `not` and Postgres eats the punctuation, so `cat OR dog` and `cat dog` produce the same
lexemes. It stops being survivable the instant a saved query carries its filters:
`(cat) AND extension:png` reaches Postgres as `'cat' & 'extens' & 'png'`, so the **filter term
becomes a text requirement** and the replay returns near-nothing. Reconstructing the Selection
from the canonical DSL is only half the job; the text has to be reconstructed from it too.

**The wire form of a save is not the same question as its stored form.** The stored query is one
canonical DSL string: no second persisted representation, no merge rule, no precedence answer.
What the browser POSTs is the `dimension:value` token list it already holds, which is the form
the save-as-collection button beside it has posted since #907, and the server composes the
canonical string. Building that string in the browser would put a second implementation of the
quoting grammar in a language that cannot derive it from the lexer. `facet.ParseSelection` had
already recorded the argument, when it rejected `dsl=` as the rail's wire shape precisely
because *"the frontend would have to splice UI state into a hand-written query string, re-quoting
values that contain a space or a colon"*.

### 5. Consequences

- The DSL's quoted-string grammar gained `\\` as the escape for a literal backslash. Without it
  **no valid spelling existed** for a value ending in one, so the canonical form could not write
  down every value it has to carry. ⚠️ This changes the reading of existing input containing a
  doubled backslash (two literal backslashes before, one after); verified before the change that
  no test, fixture or stored row contained one.
- `field` joins the DSL's field whitelist, so the parser stops answering `unknown field "field"`
  and the whitelist rendered in error responses stays accurate.
- The DSL path now **canonicalises values** the way the `filter=` path always has. That is not
  tidiness: an unvalidated `field:` date bound reaches a `::TIMESTAMPTZ` cast and raises a
  Postgres 22P02 mid-query, which is the fail-closed hole `CanonicalValue` exists to close on the
  other path.
- Typed ordered comparison, `file_size`, `workflow_state` and ordered non-field grouping are
  **sprint 18b** and get their own amendment. Nothing here anticipates them.

## Amendment, 2026-08-29, after #1173 (sprint 18b)

The 2026-08-20 amendment gave the grammar operators. It gave them exactly one meaning, and that
meaning was wrong in a way nothing measured: a bound was a **date** because it was a bound.

`facet.dimensionSQL` chose the storage column from the OPERATOR (`op == FieldOpAtLeast || op ==
FieldOpAtMost` selected `value_date`), and `canonicalBound` accepted RFC3339 or `2006-01-02` and
nothing else. So `filter=field:pixel_width>=1920` answered **400** against a `number` field that
stores a perfectly orderable value in `value_num`, and no non-field dimension could be bounded at
all. This amendment covers 18b only.

⛔ **`workflow_state` is sprint 18c and gets its own amendment.** The 18a amendment's closing line
lists it beside these; the roadmap was re-cut afterwards (`98b5d9f8`), and nothing here anticipates
it. Resource and media UI is 18d.

### 1. A bound has a DOMAIN, and the domain picks the column

An operator says which **side** of a bound a row must fall on. It says nothing about what kind of
quantity is being bounded. Those are two facts and the code carried one.

| domain | canonical form | column | cast |
|---|---|---|---|
| temporal | RFC3339 UTC | `asset_field_value.value_date` | `TIMESTAMPTZ` |
| numeric | shortest decimal that reads back to the same `float64` | `asset_field_value.value_num` | `DOUBLE PRECISION` |
| bytes | exact base-10 `int64` | `assets.file_size_bytes` | `BIGINT` |

Temporal behaviour is unchanged, **including** a date-only `<=` canonicalising to the last
microsecond of that day.

**The numeric canonical form is stated as a property, not as a notation:** two spellings that denote
the same `float64` produce the same canonical string, and that string reads back to that same
`float64`. So `1920`, `1920.0` and `1.92e3` are one term and one `CacheKey`.

**Non-finite values are refused because they are out of domain, not because they would be inert.**
`strconv.ParseFloat` accepts `NaN`, `Inf` and `Infinity`, and an ordered filter over this project's
numeric metadata is defined over **finite** values only.

⛔ The tempting justification is that such a bound would match nothing, and **that is false for
PostgreSQL.** Postgres does not evaluate a NaN comparison as unknown: it deliberately makes NaN
**equal to NaN** and **greater than every non-NaN float**, so NaNs sort deterministically and can
live in btree indexes. Measured rather than assumed:

| expression | result |
|---|---|
| `'NaN'::float8 >= 'NaN'::float8` | `t` |
| `'NaN'::float8 > 1e300::float8` | `t` |
| `'NaN'::float8 >= 'Infinity'::float8` | `t` |
| `1e300::float8 >= 'NaN'::float8` | `f` |

So `value_num >= 'NaN'` matches exactly the rows storing NaN, and `>= '-Infinity'` matches every row
that has a value at all. Accepting either would surface an exceptional ordering rule through a
control that promises an ordinary numeric bound. Rejecting during pure value-domain validation keeps
that semantics off the wire entirely, which is a stronger reason than inertness would have been.

**Bytes are `int64` with no `float64` anywhere on the path.** `file_size_bytes` is BIGINT and
reaches past 2^53, where a `float64` stops being able to tell consecutive integers apart. A byte
count parsed through one comes back as a different number than the caller wrote, silently, and only
for large files, which is exactly where a size filter gets used.

### 2. ⭐ THE TWO SPELLINGS ARE DISJOINT, WHICH IS WHY NO SCHEMA IS NEEDED TO CANONICALISE

`FacetType.CanonicalValue` is **pure**, and it is where canonical identity (hence the cache key) is
fixed. 18b makes the domain a function of the value alone, and that only works because no string is
both a date and a number: RFC3339 and `2006-01-02` require the full punctuated layout, so `2026` is
the number 2026 and never a year, and `ParseFloat` rejects every date spelling because a date
carries `-` in the middle of its digits. Asserted, not assumed.

### 3. ⛔ TWO VALIDITY CLASSES, DELIBERATELY DIFFERENT OUTCOMES

This asymmetry is load-bearing, and a test asserting the wrong one looks correct and proves nothing.

**Lexical and value-domain validity is PURE and is a request error.** A malformed bound, a
non-finite numeric, a fractional byte, a unit-bearing value such as `1MB`, an integral overflow: all
knowable without a schema, all refused in `CanonicalValue`, all **400** on the `filter=` path and a
`DSLError` on the DSL path.

**Type compatibility needs a ROW and fails CLOSED to an empty result set.** Whether a *field* may be
compared this way is `field_definition.type`, so it is answered in `Selection.Authorize`, beside the
`read_capability` lookup already there and on the same row, and a refusal is an empty page rather
than an error. A 400 would tell a caller "that code names a text field" about a code they supplied,
which is the existence oracle `validFieldCode` refuses one level up.

| `field_definition.type` | ordered | column |
|---|---|---|
| `date`, `datetime` | yes | `value_date` |
| `number` | yes | `value_num` |
| everything else | **no, refused** | n/a |

⚠️ **`boolean` is deliberately absent and it is the case that proves the seam does anything.** ADR
0012 encodes a boolean as 1 or 0 in `value_num`, the same column a numeric bound reads, so
`field:<bool>>=0` **matches every row** unless something refuses it. Every other incompatible type
is refused twice over, by the check and by a NULL column, so a test built on those types passes
whether or not the check exists. Found by mutating the implementation and re-running the suite, not
by reading it.

There is no integer field type in the CHECK constraint, which is why exact integral comparison
exists only for a resource dimension and never for `field:`.

### 4. `file_size`, and the value shape a non-field ordered dimension has

`filter=file_size:>=12345`, backed by `assets.file_size_bytes`.

The value is a **bare bound with the operator leading**, unlike `field:`'s compound
`code<op>value`, because `file_size` names exactly one column and has nothing to disambiguate. That
is a new value shape for `CanonicalValue` and a second split function beside `SplitFieldTerm`.

⛔ **`filter=file_size>=12345` carries no colon, so it names no dimension, and it stays malformed
forever.** `ParseSelection` cuts the wire token at the first colon; that is a property of the wire
form rather than of this dimension, and it is why it can never be used as a fail-before.

It needs **no lexer change**: `>` and `=` are not delimiters in the DSL's word run, so `>=12345`
lexes as one `TokWord`, `parseFieldMatch` already accepts a word as a value, and `Serialize`
therefore emits it unquoted. `:` remains unusable as an operator character for the reason #1165
gave.

**Only an asset has a file.** A post is a set of members and a collection is a container, so both
fall through to `ok=false`, which `Selection.SQL` returns as `satisfiable=false` and all four call
sites already honour by returning nothing for that entity. This is `FacetExtension`'s shape since
#907 and it needs no second exclusion mechanism. It is the positive-narrowing direction `ai:`'s
collection arm established as the test: a caller asking for files over 10MB is asking about **files**,
so an entity with no file leaving the page is the answer rather than a loss. An arm that treated an
active narrowing filter as no constraint would return every post and every collection beside the
qualifying assets, which is a filter that made the result set **larger**.

### 5. Ordered grouping: a classification, not a widening

The 2026-08-20 amendment fixed how terms combine and the 18a amendment wrote down which dimensions
can carry several. Neither covered a dimension whose values are **bounds**.

> **A FacetType is ordered when its values are bounds carrying a comparison operator.** Among
> non-field dimensions only `file_size` qualifies.
>
> - Ordered, **same** operator: **OR**. A value list, and the looser bound wins, which is what "at
>   least A or at least B" means.
> - Ordered, **different** operators: separate sub-groups, therefore **AND**. The intersection.
> - `field:`: unchanged, already grouped by `(code, operator)`.
> - Every other dimension: unchanged, one sub-group, existing OR.

⛔ **The failure mode to avoid is generalising.** The operator extraction was gated on
`dim == FacetField`; widening it to *all* dimensions would make `extension:png` operator-aware and
hand `tag:` a value grammar it never declared. So a dimension is ordered only by appearing in
`FacetType.orderedDomain`, which is the parallel of `FacetType.conjunctive` and is written the same
way for the same reason. `field:` is deliberately **not** in it: its orderedness is a property of
each field definition's declared type, answered per term against the database.

`file_size:>=A` beside `file_size:<=B` ORed reads "bigger than A or smaller than B", which is every
asset that has a size at all. That is the range that looks like it worked, and it is #1165's date
range one dimension over.

### 6. ⭐ The regression that proves it, and why it needed four populations

⛔ **A count assertion that passes on a union passes on the bug.** The fixture builds four
populations against bounds A <= B and compares hit ID **sets**:

| population | condition | satisfies |
|---|---|---|
| L | size > B | `>=A` only |
| U | size < A | `<=B` only |
| X | A <= size <= B | both |
| N | `file_size_bytes IS NULL` | **neither** |

⭐ **N exists only because of NULL.** A real number is always below, within or above a range, so
"satisfies neither bound" is unreachable for a sized row. That makes N simultaneously the
null-handling proof and the only way the fixture can tell "matched nothing" from "was not asked".

`>=A AND <=B` returns X exactly. Measured against the OR behaviour it returns 6 where 2 is correct,
which is X ∪ L ∪ U, strictly **larger** than either single bound.

### 7. Consequences

- `file_size` joins the DSL whitelist and `dslFieldForFacet`, so it round-trips through a saved
  query with no new mechanism. Both bounds have to survive as two terms, because the grouping rule
  that makes them an intersection is applied downstream and has nothing to work with if the compiler
  collapsed them.
- Cache identity gains ordered terms, which is `Selection`'s own contract. **ADR 0056 is unchanged
  and this was re-verified rather than asserted:** no cursor payload change, no pagination change,
  no `total_count` contract change. `search/cursor.go` and the keyset path are untouched by the
  diff, and the count and the result set narrow together, which every DB-backed assertion in this
  arc checks on each run.
- `fieldReadable` became `fieldGate` and returns the declared type on the same row. One lookup, two
  facts; asking twice for two columns of one row would double the per-term cost of a filter for no
  gain.
- ⚠️ One existing assertion moved rather than being deleted. `field:expires<=42` was on the
  "unknown or malformed operator" list because a bound could only be a date; it is now lexically
  valid, and the question it was really asking, may a field DECLARED as a date be compared to 42,
  moved to the seam that can answer it. The value-domain rejections replaced it in place.
- ⛔ **Five mutations were run against the finished suite, and two of its assertions were vacuous
  until that found them.** Removing the `Authorize` type check left every refusal test green, and
  the "exactly zero posts" cross-arm assertion passed with a deliberately broken arm because the
  fixture had no post carrying the phrase. Green is a claim about the tests that ran; both were
  fixed and both now fail against the mutation.
