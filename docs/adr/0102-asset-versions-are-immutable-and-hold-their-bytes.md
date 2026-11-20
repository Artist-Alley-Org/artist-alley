---
id: "0102"
title: Asset versions - an immutable, numbered history whose bytes stay pinned while a version exists
status: accepted
date: 2026-11-19
area: storage
phases:
  - "1.22"
supersedes: []
related:
  - "0008"
  - "0011"
  - "0043"
  - "0101"
tags:
  - federation
  - preservation
  - versions
  - retention
  - garbage-collection
excerpt: >-
  Every local asset with a file has an immutable, numbered,
  predecessor-linked version history in the database (migration 00084,
  #1596). A version's bytes are pinned for as long as the version
  exists, whatever SQL created or removed it, and are released only when
  the version goes with its asset. A version starts as a draft; sealing
  fixes its file set, and in this first slice the file set may hold only
  the original. Nothing is announced to peers yet.
---

## Status (2026-11-19)

Accepted.
- **Slice 1:** the version model and its retention rules.
- **Slice 2:** access-copy promotion and the selection policy, then
  reconciliation, which seals versions with verified access copies and
  adds rendition successors (both below).
- **Still to come:** nothing is announced to peers yet. Replacement, the
  versions read endpoint, the sender and the access-only transfer
  profile are later work.

## Context

Federation spec v1 §17.2.1 says a version is immutable once announced,
and that changed bytes always make a new version linked to the one
before it. §17.2.2 lists a file only when the sender holds a verified
SHA-256 for exactly those bytes, with at least one `original` per
version. Until now an asset had a single `file_hash` that any SQL could
overwrite. Nothing recorded which bytes an asset had held before, and
nothing kept those bytes from garbage collection once the asset's own
pin went. Soft delete, for example, removes the asset pin.

Bytes are content-addressed and shared (ADR 0008): retention is a pin
per `(object, subject)`, and an object with no pin is scheduled for GC
after a grace period. Assets are written by several paths: the API
create, the aiedit job's own `INSERT`, seed inserts, test fixtures and
the seed reset, which uses `TRUNCATE ... CASCADE`. A rule enforced only
by a Go wrapper would hold on some of these paths and silently fail on
the rest.

## Decision

### Version state lives in the database

`asset_versions` holds one row per version of a local asset: number,
predecessor, origin, the original's hash, size, type and base name, and
seal and announce state. `asset_version_files` holds a sealed version's
file set. `assets.current_version_id` names the version whose original
is the asset's file.

- The chain is linear and gap-free: numbers start at 1 and go up by one,
  each predecessor is the same asset's latest version, and a version
  has at most one successor.
- Remote assets are versioned by their sender (#1574) and get no rows
  here. An asset with no bytes has no version.
- `file_hash` changes only by moving to a later version whose original
  it is. A direct change is refused, and the current version never
  moves backwards.

### Immutability and sealing

- A version's defining columns never change. A version is deleted only
  together with its asset.
- A version is created as a draft. `asset_version_seal()` claims it for
  the current transaction (by transaction id, not time), writes the
  file rows, then sets `sealed_at`. After the seal the file set cannot
  change, and `announced_at` can be set once, only on a sealed version.
- **A file set holds the original and at most one `preview` and one
  `access` copy.** Migration 00085 enforces this in the database:
  - **the original:** no version is sealed unless the original's stored
    bytes are verified for exactly its hash and size;
  - **a copy:** it is accepted only if all four hold:
    - it is derived from the version's own original under a named
      variant key;
    - it is a verified object of its own;
    - its digest and size are in the immutable digest history of exactly
      that variant of that original (provenance);
    - it is held, either by a live promotion pin or because it is the
      same object in the same role of an earlier sealed version of the
      asset (carried forward unchanged).
- **Provenance.** `storage_variant_digest_history` keeps every verified
  digest recorded for a variant the policy can choose, by object, key and
  generation:
  - it is insert-only, and each row must mirror a verified variant row;
  - a regeneration adds a row and removes none, so a copy promoted
    before a regeneration still seals;
  - an unrelated object cannot be presented as an asset's rendition.
- **Role vocabulary.** `preview` and `access` are the local byte classes
  of spec §7.5.7 (a display rendition, and the largest rendition served
  for viewing), which share decisions need to tell apart. Both are wire
  role `access` in §17.2.2; the sender maps them. `preservation` is not
  used.

### Pin lifetime and GC

- Every version pins its original, and every file row its object, with
  `(hash, 'asset_version', <version id>)`.
  - **Writing the pin:** a trigger on the row itself writes it, so a raw
    `INSERT` pins just as the helper functions do. It takes the storage
    object's row lock and clears GC first, the same order as the
    existing pin paths.
  - **The commit check:** the rule that an existing version's bytes are
    pinned to it is re-checked when the transaction commits. It runs
    after any version insert or update, any file row insert, and any
    change to or removal of a version pin.
  - **Refused:** removing a live version's pin, moving it to another
    subject, or `TRUNCATE storage_pins` while versions exist.
- **Every pin writer uses one lock order.** The storage service's
  `AddPin`, `RemovePin`, upload and import, casfetch, and the version
  functions all lock the storage object's row before changing a pin row.
  - **Adding:** a pin clears any pending GC in the same transaction.
  - **Removing:** a removal decides "was that the last pin?" in a
    separate statement after taking the lock, so it sees a pin that a
    concurrent transaction has just committed. A single
    `UPDATE ... WHERE NOT EXISTS (pins)` that waits on the lock re-checks
    with its old snapshot, and was measured scheduling a just-pinned
    object.
  - **Result:** an object that is pinned when the last of two racing
    transactions commits is never left GC-eligible.
- Soft delete keeps version pins, so a soft-deleted asset's bytes
  survive until the hard purge.
- When a version goes with its asset (a hard purge, a `DELETE`, or a
  `TRUNCATE` reset), the same statement releases its pins. Every object
  left with no pin is scheduled for GC after the grace period:
  `aa.storage_gc_grace` when the caller sets it, which the purge does
  from the storage service, else the 24-hour default.
- The hard purge deletes only rows that are still past retention when its
  `DELETE` reaches them. An asset restored after the purge picked it is
  kept, with its versions and pins, and gets no purge audit event. A
  restore that reaches the row after the purge has deleted it waits, then
  reports that the asset no longer exists.

### Backfill provenance

The migration gives every existing local asset with bytes, soft-deleted
ones included, a draft version 1:

- origin `backfill_current`;
- `created_at` set to the migration time, not the asset's creation time.

It records what was present when the migration ran. It does not claim
that this is what was ingested, and it adds no preservation events. A
local asset that points at a missing storage object stops the migration
with a count; the migration never invents or skips one.

### Access copies are promoted, then chosen by one policy

A derived variant lives under its parent's hash and is overwritten by
each regeneration, so a sealed file set cannot point at it.

**Promotion** (`storage.Service.PromoteVariant`) copies a variant into
its own content-addressed object, addressed by the variant's SHA-256:

- **Only verified bytes:** only a variant whose #1597 row says verified,
  and whose stored bytes, read under the variant's shared lock, still
  hash to the recorded digest and size. Anything else fails closed and
  changes nothing.
- **Never overwrites:** an object that already has the digest is only
  pinned, never written again or relabelled. A new one goes through the
  verified import path, which takes the per-hash import lock exclusively
  and checks again under it. Every other writer of an original, peer
  imports and ordinary uploads, takes the same lock shared around its
  check, Put and record. So an original another writer recorded
  meanwhile is only pinned, and an upload arriving during a promotion
  waits, then deduplicates. Because an upload now holds a database
  connection across its backend Put, it first takes one of the bounded
  put slots it shares with variant writes, keeping the smallest pool's
  connection budget.
- **Checked by reading:** before it is returned, the result is confirmed
  by its recorded size and verified original row, and by reading and
  hashing the stored bytes. A row that still says verified over changed
  bytes is refused.
- **The transient pin:** the copy is held by its own pin,
  `version_promote`, from the moment it is recorded. The caller releases
  it only after a sealed version pins the copy, so a published copy is
  never unpinned. A failure releases the pin. A copy nothing sealed
  becomes GC-eligible once released, and `ReleaseStalePins` clears pins
  left by a crash.

**Selection** (`assets.SelectAccessVariants`) follows §7.5.7:

- **Candidates:** only the configured still ladder's `contain` rungs
  whose key is a plain token (lowercase letters, digits, `-`, `_`), so
  every dotted or path-like key is excluded: HLS, turntable frames,
  views, sprites, subtitles. The bare names other writers use for
  non-ladder bytes (`original`, `poster`, `cover`, `embedded-preview`,
  `iso`) are reserved even when an operator configures them as a rung.
  Cropped `cover` rungs are excluded by their fit.
- **Preview:** the smallest available rung.
- **Access derivative:** the largest rung, only for images, the one
  kind whose viewer shows a derived still in place of the original.
  Other kinds get a preview only.
- **Determinism:** the choice depends only on the ladder and the keys
  present.

### Reconciliation seals versions and adds rendition successors

One `asset.version_reconcile` job per object hash
(`assets.VersionReconciler`).

**Who requests it:**
- the database on every terminal processing event (an asset set to
  ready or failed, which covers every preview handler);
- the database when an asset that needs no processing is inserted;
- the statement that records a verified digest for a key the policy can
  choose, unless a preview job for those bytes is in flight (so a
  re-render never seals a half-written ladder). The request and the
  record commit together or not at all, so a verified change is never
  left unrequested. HLS segments, frames, sprites and covers never
  request one;
- the operator command.

Requests coalesce per hash. One that arrives while a job runs is queued
after that job, so no request is lost. The type is capped at two
concurrent jobs.

**What one run does, for every local asset whose current version's
original is the hash:**
- **Skips** an asset still processing, and everything when the original
  is unverified.
- **Promotes** each chosen variant once. An unverified, missing or
  changed variant is omitted, never published.
- **Seals** a draft with the original and the copies.
- **Leaves alone** a sealed version whose copies already match, so a
  rerun is a no-op.
- **Adds exactly one successor** when a chosen copy changed or is new:
  origin `rendition`, the same original, sealed with the new copies and
  every unchanged one carried forward. The predecessor keeps its file
  set, pins and bytes. A copy that cannot be promoted now is not a
  change.

**Transient pins:** every promotion's pin is released after the
transactions that pin the copies to their versions have finished, on
success and on failure. Stale promotion pins are released in a bounded
batch at the start of each run.

**Operator path:** `aa reconcile-asset-versions [-apply]` requests
reconciliation for drafts the 00084 backfill made, but only for hashes
whose original is verified; the rest wait for
`aa backfill-variant-digests`.

## Consequences

### Positive

- Old bytes cannot be lost to GC while their version exists, whichever
  code path wrote or removed rows.
- A sealed file set is fixed, which is what §17.2.1 needs before
  anything is announced.
- Raw-SQL test cleanups and the seed reset keep working without changes:
  removing an asset releases its version pins on its own.

### Negative

- Soft-deleted assets' bytes are no longer scheduled for GC until the
  hard purge. Storage held by the trash grows accordingly. No sweeper
  deletes bytes yet, so today only the GC marks change.
- Every insert, update or delete of a version, file row or version pin
  does extra work under a row lock, and is checked again at commit.
- Every pin add or removal is now its own short transaction holding the
  object's row lock, so concurrent pin changes on one shared object
  queue behind each other.
- Changing an asset's bytes now requires the version machinery. Code
  that wrote `file_hash` directly must create a version instead, and
  three test fixtures were rewritten this way.
- View-only peers cannot yet be sent anything. The federation original
  grant (`allow_original`, §7.5.7) withholds the original, and no
  verified access copy can be sealed yet. **An access-only transfer
  profile, built on verified access copies, is required before #1596 and
  #1574 close.**

## Alternatives considered

- **Release version pins in Go and refuse any other delete.** Rejected:
  more than a hundred test files delete assets with raw SQL, and the
  seed reset truncates. Each would have needed its own release step,
  and a missed one would leak pins. Releasing in the delete, with a
  commit check as a backstop, keeps every path correct.
- **Seal by time, or by a session setting.** Rejected: neither proves
  that the transaction writing the file rows is the one sealing. The
  transaction id does.
- **Accept access copies at seal time if the object exists with the
  right size.** Rejected: existence and size do not show that the bytes
  are a verified derivative of the original. Access copies wait for
  verified promotion.
- **Back-date backfilled versions to the asset's creation.** Rejected:
  the version did not exist then, and the bytes may have changed since
  the asset was ingested.

## References

- Federation spec v1 §17.2.1 (versions), §17.2.2 (files), §7.5.7 (byte
  classes and original files), §8.5 (CAS).
- Migration `app/internal/db/migrations/00084_asset_versions.sql`.
- Issues #1596 (versions), #1597 (variant fixity), #1574 (receiving
  side).
