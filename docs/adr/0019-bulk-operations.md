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
