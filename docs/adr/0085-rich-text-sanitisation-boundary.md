---
id: "0085"
title: Rich text is sanitised at both boundaries through one policy
status: accepted
date: 2026-08-02
area: security
phases: []
supersedes: []
related:
  - "0012"
  - "0081"
tags:
  - metadata
  - sanitisation
  - xss
excerpt: >
  A rich_text value is the only stored string a client renders as markup, so it is
  sanitised on write AND re-sanitised on read, both through the single policy in
  internal/richtext. The API returns pre-sanitised HTML; clients render it verbatim
  and carry no sanitiser of their own.
---

# Rich text is sanitised at both boundaries through one policy

## Context

`rich_text` was the last field type without a working display: values rendered as escaped
source, pinned deliberately (#839) until the sanitisation boundary was decided, because
rendering operator- and user-supplied markup is a stored-XSS surface. At decision time the
frontend contained **zero** `{@html}` uses — this is the application's first HTML-rendering
surface, so the choice sets precedent, not just behaviour.

Three boundaries were on the table: sanitise on write (cheap reads, but seed, import, and any
future federation inbox write **around** the HTTP handlers — verified: `SeedInsertAssetFieldValue`
passes no handler gate); sanitise on render in the client (every render surface must remember,
the classic two-implementations trap, plus a frontend dependency); or both.

## Decision

**Both boundaries, one implementation.** `app/internal/richtext` (wrapping bluemonday) is the
single policy — allowed tags `p br strong em ul ol li blockquote h3 h4` and `a[href]`
restricted to http/https/mailto with `rel="noopener noreferrer"` forced; everything else
stripped, not escaped. `Sanitize` is idempotent (tested), which is what makes double
application at two boundaries free.

- **Write:** `SanitizeValueText` is called in `buildUpsertParams` / `buildCollectionUpsertParams`
  — a placement that also covers `ApplyAssetDefaults` (defaults funnel through the same
  function), both seed writers, and the extraction writer adapter.
- **Read:** called again in `buildAssetValue` and `buildCollectionValue` — the shared DTO
  helpers behind BOTH the list and mutation-response paths — so a stored value is never
  trusted regardless of how it arrived.
- **The API contract is "rich_text HTML arrives pre-sanitised."** Clients render it verbatim
  (`{@html}`) and carry no sanitiser dependency; every client of the API inherits the guarantee.

## Consequences

- The one enforcement point follows the codebase's established doctrine
  (`NormalizeOptionsDoc`, `checkVocabulary`): a rule lives in exactly one place, and every
  path calls it. Changing the allowed-tag set is a one-package change.
- Values written before this decision (or around the handlers in future) are safe at render
  time by construction — the read boundary, not operator discipline, is the guarantee. No
  backfill exists or is needed (pre-release, and read-side covers it).
- Removing either hook fails a named test (`TestRichTextWriteSideStoresSanitisedHTML`,
  `TestRichTextReadSideSanitisesValuesWrittenAroundTheHandler`); the browser canary test
  includes an `innerHTML` control proving the canary can fire, so its silence is evidence.
- bluemonday joins the supply-chain fork-audit list.
- A future federation inbox that writes field values MUST route rich_text through the same
  package on ingest; the read boundary protects rendering either way, but stored-clean is
  the contract peers should be able to assume.
