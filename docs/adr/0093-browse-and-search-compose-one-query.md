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

**The lesson worth carrying past this ADR:** a composition rule that is never written down is not
a decision, it is whatever the code happened to do — and *singular right, plural wrong* is invisible
to any test that uses one of the thing. When a dimension can appear more than once, state its
combination rule here and assert the N≥2 case with the arithmetic written out (`both < min(a, b)`),
never `both > 0`, because a count assertion that passes on a union passes on the bug.
