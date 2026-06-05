---
id: "0042"
title: Distributed catalogs — typed constants per package, no central god-file
status: accepted
date: 2026-06-04
area: process
phases: []
supersedes: []
related:
  - "0006"
  - "0011"
  - "0012"
  - "0035"
tags:
  - process
  - conventions
  - architecture
  - go
  - typing
excerpt: >-
  Named-constant catalogues (field types, status codes, log codes, event
  types, notification types, permission codes, icons) live in the package
  that owns them as typed Go constants. No central definitions god-file.
  A short meta-index in docs/catalogs.md tells contributors where each
  catalogue lives.
---
## Context

Every non-trivial system accumulates **catalogues** — closed sets of
named values that the rest of the code dispatches on. Field types,
job-status codes, audit-event types, notification channels, permission
codes, icon registry, archive states, workflow transition keys, locale
identifiers. Tens to low-hundreds of named values per catalogue;
dozens of catalogues across a mature codebase.

Two patterns compete for where they live:

1. **Central god-file.** One module / namespace / file holds every
   named constant for the whole system. Common in older languages
   (PHP `define()`s, early C `#define` headers, Python projects
   that grew without enums). Easy to grep, easy to know "where
   constants live", easy for a newcomer to spelunk.
2. **Distributed per-package.** Each package owns its own catalogue
   as typed constants in its own files. The audit package owns
   audit-event names; the jobs package owns job-status codes; the
   metadata package owns field types. Nothing imports a shared
   "constants" package.

Artist Alley landed on pattern 2 incrementally — every domain we shipped
put its enums next to the code that uses them, because Go's package +
typed-constant story makes that the path of least resistance. The
result is a tree where each catalogue lives in one place near its
owner:

- `app/internal/audit/events.go` — `EventType` constants
  (`auth.login`, `asset.upload`, `share.created`, …).
- `app/internal/userprefs/prefs.go` — `KnownEventTypes` + channel
  defaults for notification routing.
- `app/internal/jobs/` — job-status codes and priority levels as
  typed Go constants; database CHECK constraints in the matching
  migration mirror them.
- `app/internal/users/` — user-lifecycle states (`pending`,
  `approved`, `disabled`, `deleted`); migration owns the CHECK,
  Go owns the typed mirror.
- `app/internal/metadata/` — field-type registry (the small,
  closed set the validation layer dispatches on).
- `app/internal/audit/categories.go` — dotted-namespaced category
  prefixes (`auth.*`, `asset.*`, `share.*`, …).
- Permission codes — seeded into the `capabilities` table by
  migration; readable names (`users.read`, `system.admin`,
  `share.create`) replace cryptic single-letter flags.
- `web/src/lib/components/AdminIcon.svelte` — frontend icon
  registry.

This works. New contributors coming from a legacy codebase, however,
often hunt for a central catalogue first, then either:

- get confused, push back, and propose centralising into a god-file
  ("I think the constants should live somewhere shared"), or
- silently create a `pkg/constants/` or `app/internal/consts/` package
  that grows into one through accretion.

Both outcomes erode the per-package locality we landed on. This ADR
codifies the convention so it survives contributor turnover.

## Decision

**Catalogues live in the package that owns them, as typed Go
constants. No central `constants` / `definitions` / `enums` package.**

Three rules:

### 1. The owning package owns the catalogue.

If a catalogue is dispatched on by the audit subsystem, it lives in
the audit package. If only the jobs subsystem reads job-status codes,
they live in the jobs package. The owning package is the one that
*decides what's in the catalogue*; consumer packages import the
typed identifier.

When a catalogue is read by two packages but written by one, the
writer owns. When two packages legitimately co-own (rare), the
catalogue lives in whichever package's behaviour the catalogue
*describes*, not where it's checked. Example: `audit.EventType` is
owned by `audit/` even though every domain package emits values from
the catalogue — because `audit/` defines the contract of "what an
event type means".

### 2. Constants are typed, not bare strings or ints.

```go
package audit

type EventType string

const (
    EventAuthLogin       EventType = "auth.login"
    EventAuthLogout      EventType = "auth.logout"
    EventAssetUploaded   EventType = "asset.uploaded"
    EventShareCreated    EventType = "share.created"
    // ...
)
```

The named type catches drift at compile time. A function signature
that accepts `EventType` rejects a bare `"asset.upload"` typo;
`go vet` flags missing cases in `switch e := evt.(type)` against
the type's known constants when the package opts into exhaustiveness
checks.

Bare-string or bare-int constants are tolerated only when they
land in *database* enforcement (CHECK constraints, seeded reference
rows) and need to round-trip through SQL with no Go type to attach
to. The Go-side mirror is still typed.

### 3. The database CHECK is the runtime guardrail; the Go type is the compile-time guardrail.

Where a catalogue is also enforced at the storage layer (status
codes on a row, kind discriminators, archive states), the migration
ships a `CHECK (col IN (...))` constraint that pins the valid
set in the database; the Go-side typed constant set is a literal
mirror of the same values. Drift between the two is a code-review
block.

Example (jobs migration):

```sql
CHECK (status IN ('queued', 'running', 'success', 'partial', 'failed', 'cancelled'))
```

```go
// app/internal/jobs/status.go
type Status string

const (
    StatusQueued    Status = "queued"
    StatusRunning   Status = "running"
    StatusSuccess   Status = "success"
    StatusPartial   Status = "partial"
    StatusFailed    Status = "failed"
    StatusCancelled Status = "cancelled"
)
```

The DB rejects an unknown value at write time; the type rejects an
unknown value at build time. Either is sufficient defence in depth;
both is policy.

### 4. There is one meta-index.

A short, hand-maintained document at [`docs/catalogs.md`](../catalogs.md)
lists every catalogue in the system with its owning package and a
one-line summary. The meta-index is *navigation*, not the catalogue
itself. It does not contain the values; it tells you which package
to open.

The meta-index is the answer to "where do constants live?" without
violating the distributed locality. New catalogues add a single
line to the index when they land.

### 5. What is *not* a catalogue (and doesn't need this treatment).

- Per-instance configuration (SMTP server, site name, theme tokens)
  — lives in `system_config` rows or environment variables, not
  in code.
- Tunable thresholds (cache sizes, retry counts, rate limits) —
  literal numbers in configuration, not named constants, unless
  the value is referenced from multiple call sites.
- Strings that look enum-shaped but are really *identifiers* (user
  IDs, asset hashes, session tokens) — these are values, not
  catalogue members.

When in doubt: a catalogue is a closed set of named values the
program dispatches on. If the program doesn't dispatch on it,
it's not a catalogue.

## Consequences

### Positive

- **Compile-time safety.** Typed constants catch typos at build,
  not at runtime. A catalogue change in one package is a build
  break in every consumer that hasn't updated.
- **Per-package locality.** Changing the user-lifecycle state set
  touches `users/` and the matching migration. Nothing else
  rebuilds; nothing else conflicts during merge.
- **Cross-package imports stay narrow.** A consumer that only
  needs job-status constants imports `app/internal/jobs/`, not a
  god-package that drags in every other catalogue's transitive
  surface.
- **Catalogues are documented next to the code that owns them.**
  A reader looking at audit emission sees the EventType type and
  its constants in the same package; no jumping to a faraway
  file.
- **The meta-index gives newcomers the single landing page they
  want** without forcing the project to actually centralise.
- **Database + Go drift is a code-review block, not a runtime
  surprise.** The CHECK constraint and the typed-constant set are
  written in the same change; reviewers compare them on the spot.

### Negative

- **No single grep target for "all the constants".** A contributor
  asking "what status values exist anywhere?" has to consult the
  meta-index and visit each package. The meta-index fixes the
  navigation problem but doesn't make the search global.
- **Catalogue duplication risk.** Two packages could independently
  invent overlapping catalogues if the meta-index isn't read.
  Mitigation: the meta-index is updated alongside every new
  catalogue, and new ones get called out in code review.
- **Cross-package shared catalogues (rare) need an owner decision.**
  When a catalogue is genuinely co-owned, picking the owning
  package needs a judgement call. The rule of thumb in §1 helps
  but doesn't eliminate ambiguity.
- **Migrations and Go drift is a real failure mode.** If a CHECK
  constraint adds `'archived'` but the Go type doesn't add the
  matching constant, reads work but writes from Go silently break.
  Mitigation: the CHECK + typed-constant pair is written in the
  same PR; reviewer checks both halves.

## Alternatives considered

- **Central `constants` / `definitions` package.** The legacy
  pattern. Rejected because (a) every change becomes a merge
  hotspot; (b) imports widen unnecessarily; (c) typed constants
  in Go give us per-package locality essentially for free, so
  the centralisation has no upside.

- **Per-domain `enums/` sub-package.** Pull all catalogues *within
  a domain* into a `domain/enums/` sub-package. Rejected as
  needless layering — Go's package-level constants already give
  the same isolation without an extra import path.

- **Database-only, no Go-side typed mirror.** Let the migration's
  CHECK constraint be the only source of truth; Go-side code
  uses bare strings. Rejected because compile-time safety on
  catalogue membership is genuinely valuable and the typed
  mirror cost is one declaration per value.

- **Code-generate the typed Go constants from the migration's
  CHECK clauses.** Considered. Rejected at this scale because
  (a) the generator is more code than the typed constants it
  produces; (b) it forces every catalogue through one parser's
  failure modes; (c) hand-maintained typed mirrors are <50 lines
  per catalogue and the drift is caught at review.

- **Code-generate the migration's CHECK from the Go typed
  constants.** Same considered / rejected for the same reasons,
  inverted direction. Plus migrations are immutable once shipped;
  generating them after the fact is wrong.

## Implementation

This ADR codifies what is already the working convention. No
existing code changes are required. Two concrete deliverables:

1. **[`docs/catalogs.md`](../catalogs.md) — the meta-index.**
   Single-page listing of every catalogue, its owning package,
   its one-line purpose, and where its database mirror lives
   (migration number or "n/a — Go-only"). Updated alongside any
   new catalogue.

2. **Code-review checklist update.** When a PR introduces a new
   catalogue or extends an existing one, reviewers verify:
   - The catalogue lives in the owning package (per §1).
   - Constants are typed (per §2).
   - Database CHECK and Go-type are written in the same PR (per §3).
   - The meta-index has a one-line entry (per §4).

A future contributor proposing a `pkg/constants/` god-package is
referred to this ADR and the meta-index. The conversation goes:
"we already solved this; here's the convention."

## References

- ADR 0006 — Go as target backend. Sets the language baseline this
  convention is shaped around.
- ADR 0011 — Asset entity. The asset's archive-state / status
  catalogues are the canonical example.
- ADR 0012 — Metadata model. Field types live in
  `app/internal/metadata/`; the catalogue convention is the same.
- ADR 0035 — ADR conventions. The procedural template this ADR follows.
- [`docs/catalogs.md`](../catalogs.md) — the meta-index this ADR
  formalises.
