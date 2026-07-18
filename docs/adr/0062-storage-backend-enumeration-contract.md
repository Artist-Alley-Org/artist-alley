---
id: "0062"
title: Storage backend enumeration contract — ordered, cursor-resumable List
status: accepted
date: 2026-07-18
area: storage
phases:
  - "1.19"
supersedes: []
related:
  - "0057"
tags:
  - storage
  - integrity
  - jobs
  - contract
excerpt: >-
  Storage backends must expose enumeration that is globally lexicographic
  by object key and resumable from a cursor. Filesystem depth-first walk
  order does not satisfy this, so the fs backend prunes and sorts to honour
  the contract; a shared contract test enforces it for every backend.
---

## Context

Storage integrity work needs to answer two questions: *does every object the
database references still exist on the backend*, and *does the backend hold
objects nothing references*. The first is answerable with per-row `Stat`.
The second requires **enumerating the backend**, and the `Backend` interface
had no enumeration primitive at all — only `Name`, `Put`, `Get`, `GetRange`,
`Delete`, `Stat`, `PresignGet`, `PresignPut`.

Without enumeration, orphan detection can only ever check the direction that
finds broken references, never the direction that finds wasted space — which
is the direction with real operator value.

The sweeps run as background jobs over potentially very large object sets, so
they must be **batched and resumable**: a sweep persists a cursor and
re-enqueues itself rather than holding one long transaction. That makes the
*ordering* of enumeration a correctness property, not an implementation
detail — a cursor is only meaningful against a stable, total order.

## Decision

Add enumeration to the `Backend` interface:

```go
List(ctx context.Context, cursor string, limit int) (refs []ObjectRef, next string, err error)
```

The contract every backend must satisfy:

1. **Globally lexicographic by object key.** The key is
   `ObjectPath(hash, variant)`; `ParseObjectPath` is its inverse, so a key
   round-trips to the `(hash, variant)` pair it names.
2. **Cursor-resumable.** Passing back `next` continues exactly where the
   previous page stopped — no key emitted twice, none skipped.
3. **Unrecognised paths are skipped**, not errors: an operator's stray file
   in the object store must not break a sweep.

**The subtle part, recorded because a naive implementation looks correct:**
filesystem depth-first traversal is *not* globally lexicographic over this
key space. For variants `a.b` and `a/b`, `.` (0x2E) sorts below `/` (0x2F)
as a byte, so `a.b` precedes `a/b` lexicographically — but `filepath.WalkDir`
descends into directory `a` first and emits `a/b` earlier. A cursor derived
from walk order therefore **duplicates some keys and skips others**. The fs
backend must prune subtrees against the cursor and sort within a page to
honour the contract. S3 gets the ordering for free: `ListObjectsV2` returns
keys in lexicographic order with continuation tokens.

Because the property is easy to get wrong and invisible in a single-page
test, it is enforced by a **shared contract test** (`storagetest`) that every
backend implementation runs, not by per-backend tests.

## Consequences

- Orphan detection covers both directions; the first production-shaped run
  surfaced three genuine pre-existing orphans across ~21,200 objects.
- Any new backend must implement ordered enumeration to pass the shared
  contract test — the cost of adding a backend goes up slightly, and that is
  the right trade for sweeps that can be trusted.
- Sweeps are resumable and observable: they run as job kinds, so they appear
  in the jobs admin queue like any other work.
- Enumeration ordering is a prerequisite for a future **delete** path, but it
  is not sufficient for one. Sweep findings are **advisory and record scan
  time** — the schema deliberately carries `resolved_at` and no "confirmed"
  flag. Anything that deletes must **re-verify at delete time** that the
  object is still unreferenced, because the database can change between scan
  and delete.
- `checksum_verify` reads every byte of every object by design (content
  addressing means the key *is* the expected checksum), so it is inherently
  I/O-bound and belongs in an off-peak window on a large install. The admin
  surface warns before triggering it; the cheap `orphan_scan` deliberately
  carries no warning, so the warning keeps its meaning.

## References

- Content-addressed storage layout and the baseline schema shape (ADR 0057).
- Storage integrity sweeps: orphan scan + checksum verification.
- The advisory-findings invariant carried into the cleanup work.
