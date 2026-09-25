---
id: "0097"
title: The published archive is an output, and the build refuses to overwrite content it cannot reproduce
status: accepted
date: 2026-08-26
area: ops
phases: []
supersedes: []
related:
  - "0095"
  - "0080"
tags:
  - seed
  - dataset
  - data-safety
excerpt: >-
  The dataset build copies the repository's profile over the published
  archive's manifest, so the archive is an output and the profile is the
  source of truth. That direction had never been enforced, and the archive
  had drifted 12,097 values ahead of the profile — a single ordinary build
  would have destroyed them. The build now compares before it writes and
  refuses when the destination holds content the source does not.
---

# The published archive is an output, and the build refuses to overwrite content it cannot reproduce

## Context

`seed/scripts/populate_archive.py` ends by copying the repository profile **over** the published
archive's `MANIFEST.json` (`:736`, a literal `shutil.copyfile`). The per-site `metadata.csv` is
regenerated from the profile's path map, so it is an *output* of the same process and cannot
preserve anything the copy removes. `apply_upgrade.py`'s header already recorded the consequence:
*"a single re-run would have restored 916 tiny images and dropped all 72 videos — regardless of the
state of any metadata.csv."*

**So the direction of truth was already decided by the code, and never enforced.** Measured
2026-08-26, before this change:

| | repo profile | published archive |
|---|---|---|
| assets | 2004 | **2005** |
| with `field_values` | 1904 | **2005** |

One ordinary build would have **deleted one published asset** and **stripped 12,097 values** —
`field_values` from 1,947 assets (100 losing all of them, 1,847 losing 3–11 each) plus `mature` on
the same set. Zero assets were richer in the repo, so the drift was strictly one-directional: the
archive had been edited, or enriched by tooling that wrote only to it, and the profile never caught
up.

⚠️ Nothing compared the two before writing, and nothing ever had. The drift survived because the
build is not run often, and the one person who might have noticed was the one who would have run it.

## Decision

**The profile is the source of truth. The published archive is an output. The build enforces that
by refusing to produce a run it cannot justify.**

1. **Before writing, compare.** If the destination holds content the source does not, the build
   **refuses and exits non-zero**, naming what would be lost.
2. **The remedy is to fix the SOURCE, never the destination.** Editing the archive is undone by the
   next build by construction, so the error points at `apply_upgrade.py` — reconcile the profile,
   then publish.
3. ⭐ **Duplicate ids are refused NON-overridably.** A manifest holding two records under one id has
   no correct interpretation — `aa seed` keys on the stable id and silently takes whichever it reads
   last — so unlike a loss, there is no version of it that is somebody's intended change. No flag
   forces it through.
4. **A deliberate removal remains possible**, via an explicit `--allow-regression`, which states in
   its output that the loss is real and unrecoverable. The gate is against *silent* destruction, not
   against intent.
5. **The guard runs in `--dry-run` too.** A dry run that passes while a real run would destroy data
   is worse than no dry run at all.

## Consequences

- The 12,097 drifted values were carried back into the profile before the guard shipped. A guard in
  front of an unreconciled source would simply have blocked the tool forever, so **repair is part of
  the decision, not a follow-up.**
- ⚠️ **The guard proves nothing until it has been seen to REFUSE.** It was verified in both
  directions — against the pre-repair profile it reports 12,097 losses and exits non-zero; against
  the repaired one, zero. A guard only ever observed permitting a run is untested, which is the
  failure ADR 0095's 2026-08-26 amendment records at length.
- Content-level drift is a **different question from presence** and is only partly addressed here: a
  missing key, an empty value and a *differing* value are three cases. Presence is enforced;
  divergence in value is tracked separately (#1294, #1295).
- ⛔ **This does not make the archive backed up.** It makes one specific destruction impossible. The
  published dataset still has no backup, and every other path that writes to that share is still
  unguarded.

## Amendment, 2026-08-26 (#1294, #1295): a MEASUREMENT is not content, and this ADR does not govern it

The `CHANGED_VALUE` case above was left "tracked separately", and the separate tracking found that
the two issues were not the same kind of question at all.

**#1294 — 160 site_a `file_size_bytes` where the profile and the share disagreed.** The instinct
this ADR creates is "the profile is the source of truth, so the share is wrong." That instinct is
**not applicable**, and following it was how sprint 14 nearly shipped a profile that would have made
the next build refuse its own input.

> ⭐ **A byte count is a MEASUREMENT of a file the pipeline produces, not a value the profile is
> free to assert.** There is exactly one right answer — what `kenney_hq.py build` makes from the
> committed manifest and the pack — and the profile's job is to *describe* it. This ADR governs
> which records exist and what values they carry. It has nothing to say about arithmetic.

Measured against a rebuilt pool: **150 of site_a's 260 replacement rows and 472 of site_b's 656**
named a size the file does not have, and site_a's published share agreed with the *rebuilt* pool on
776 of 777 records. The repository was the stale side. `newSize` is the size of a **render**, #630
and #685 both changed what frame a vector is drawn into, and nothing ever re-derived the numbers —
they were measured once, by hand. Re-measurement is now a command (`kenney_hq.py sizes`), report-only
and non-zero on drift by default so it can stand as a gate.

⭐ **And it was visible without the share or the pool.** `balance-assets.site_a.json` was emitted
after those fixes and had been contradicting the replacements docs about **115 pool files** inside
the repository the whole time. Two committed documents naming one pool file must agree about its
size; that is a test now.

**#1295 — the gate could not see the pass.** `apply_upgrade.py --check` had a term for every pass
except `apply_replacements`, because that pass returned records *processed*, not records *modified*
— `260/260` on every run, upgraded or not. A number that is never zero cannot be a drift signal, so
the pass was left out rather than fixed, and a profile with drifted replacements passed the
pre-publish gate for as long as it took someone to notice by hand.

⚠️ This is the second consequence above, arriving from the other direction: **a gate only ever
observed permitting a run is untested.** The refusal now has a constructed-drift test that drives
the real script, watches it fail, repairs the profile and watches it pass.

**What a future reader should take from this.** When the profile and the archive disagree, ask first
what *kind* of value it is:

| the value is… | who is authoritative | example |
|---|---|---|
| content — a record, a field value, a flag | the **profile** (this ADR) | `field_values`, `mature`, which assets exist |
| a measurement of a file the pipeline produces | the **artifact** the pipeline makes | `file_size_bytes` on a pool render |
| a claim about bytes staged from elsewhere | **neither, until the bytes are checked** | the 11 video records — see below |

⛔ **The third row is unresolved and is not a byte count.** Eleven site_a video records claim a size
their staged file does not have, and probing the origins each record names returns *exactly the
profile's number* — while four of them carry a `metadata.sha256` that matches the smaller staged
file. Those records describe **two different artifacts at once**, and no rule in this ADR picks
between them: it is a decision about what the published dataset ships. `populate_archive.py`'s
pre-staged branch only checks `size > 0`, so nothing will surface it on its own.

---

## Amendment, 2026-08-27 (#1311, #1312): the guard cannot see a corrupted measurement

The amendment above split **content** from **measurement** and said the artifact is authoritative
for the second. `manifest_guard` does not implement that split, and sprint 14d found the gap.

`manifest_guard.py:34-43` refuses `MISSING_RECORD`, `MISSING_KEY` and `EMPTIED_VALUE`, and reports
`CHANGED_VALUE` as *"NOT a loss ... Reported, never refused"* on the reasoning that an edit is what
a change looks like and the profile is the source of truth for edits.

⛔ **That reasoning is correct for content and wrong for measurements.** Measured on `dev` before
PR #1311: **twelve records** where the profile and the published manifest disagreed on
`file_size_bytes`, and in **all twelve** the manifest matched the bytes on disk while the profile
claimed larger, totalling **2,690,105,638 bytes**. `populate_archive.py:841` copies the profile over
`MANIFEST.json`, so the next publish would have replaced correct measurements with wrong ones, and
the guard would have reported it and proceeded.

⭐ **The same property that makes a correction safe makes a corruption invisible.** The sprint-14d
brief cited `CHANGED_VALUE` approvingly as proof that re-measuring four hashes was safe. That was
true, and its inverse was equally true and unstated.

⚠️ **And the split is per record class, not global.** `metadata.sha256` is a measurement for hq
records and **identity** for `internet`-root ones: `sanitize_and_assemble.py:1517` mints the asset
id from it and `:1558-1560` derive three timestamps from it. A brief that ruled "re-measure the
four hashes" and closed the question would have moved ids on the next assembly, and only the
implementing agent's refusal to follow a closed instruction stopped it.

**So this ADR's category is a property of the FIELD IN A RECORD CLASS, not of the field.** Deciding
"is this content, a measurement, or identity" has to be asked per class, and a rule that answers it
once for a field name is wrong.

Tracked as #1312. Not fixed here: naively promoting `CHANGED_VALUE` to a loss would refuse every
legitimate edit and make the guard unusable, which is the failure mode of over-correcting a gate.

---

## Amendment, 2026-08-27 (#1318, #1312, #1313): the split is implemented, and its axis is `source_root`

The amendment above named the gap and left it open, because promoting `CHANGED_VALUE` to a loss
would refuse every legitimate edit. Sprint 14e closed it without that cost.

`CHANGED_VALUE` is unchanged. A new `CORRUPTED_MEASUREMENT` verdict sits beside it, and the axis
that selects between them is the record's **`source_root`**, never the field name:

| `source_root` | why | verdict on a disagreement |
|---|---|---|
| `site`, `torrent_import`, `internet` | the bytes are staged at the destination with no reproducible source, so the destination **is** the artifact | **refused** |
| `local`, `hq`, `pack` | copied from a source the profile is built against, so the share can lag it | reported, permitted |

This is the previous amendment's "per record class, not per field" rule given a mechanism. It is
measured rather than asserted: against a kenney-hq pool built fresh on 2026-08-27 (945 vectors
rendered, 86 bitmaps copied), all **656** of studio-b's `hq` records match the profile and only
**264** match the published manifest.

### ⛔ The direction error this ADR's own author then made

That 656-versus-264 measurement exists because the sprint brief asked for the opposite of the
correct thing. Site_b's published manifest disagreed with the profile on 392 `file_size_bytes`; the
manifest matched the bytes on disk on all 392 and the profile on none; the brief concluded the
profile was stale and asked for it to be reconciled to its files. **Doing so would have overwritten
392 correct values with stale ones, and the next build would then have refused its own input.**

⭐ **Matching its own bytes proves only that a copy is SELF-CONSISTENT.** A stale copy agrees with
itself perfectly. All 392 records are `hq`, copied *from* the pool, so the authority is the pool.

**So this ADR's rule needs its sharper form: the artifact is what the pipeline PRODUCES, not where
the pipeline WRITES.** A destination is downstream of the artifact and inherits its staleness
silently. The record's `source_root` is what points at the artifact, which is why the verdict above
keys on it.

⚠️ Worth recording that this ADR's measurement-versus-content rule was written one day earlier by
the same author who then applied it to the wrong noun. A rule stated at the level of "which side is
authoritative" is not usable until it also says **how to find the side**.

### The larger site_b defect, which no issue had named

While the 392 were being disputed, **6,806 `field_values` across all 1,306 site_b records** existed
at the share and not in the profile, in the same eleven keys as #1275's. Since
`populate_archive.py:736` copies the profile over `MANIFEST.json`, an ordinary run would have
stripped every one. The new guard reported 6,806 losses; `manifest-reconcile.site_b.json` carries
them back (6,806 filled, 0 overwritten, 1,306 ids unchanged in both directions) and the guard now
reports 0.

⚠️ **The generalisable miss: a sibling artifact's known defect was not tested for.** Site_a's defect
was missing `field_values`; site_b was measured for `file_size_bytes` instead, one field was
checked, and the finding was generalised from it. When two artifacts come off one pipeline, test the
second for the **first one's** defect before reporting whatever the first probe happened to find.

---

## Amendment, 2026-09-21 (#1319, ADR 0098): a committed identity migration is not a deletion

`manifest_guard.compare` keyed both sides by `id` and filed every destination id absent from the
source as `MISSING_RECORD`. Two migrations (#1293, #1310; ADR 0098) had moved 511 post ids onto
values derived from each post's own content, the published wall still carried the old ids, and so
the guard read the pipeline's own work as a deletion. Measured read-only against the live share:
`posts.json` reported **175** `MISSING_RECORD` on site_a and **336** on site_b, every one an
`old_id` in `seed/upgrades/post-id-migration.studio-a.json` or `.studio-b.json` whose `new_id` the
current profile holds, and **0** uncovered; `MANIFEST.json` reported 0 losses on both sites.
Decision 1 refused a publish that would have lost nothing, and `--allow-regression` (Decision 4)
was the only way through, which is the wrong tool: it waves through every loss, not the one thing
that is not a loss.

**A recorded identity migration is not a deletion, and the evidence is the committed document.**

1. A destination record whose id is a recorded `old_id`, and whose `new_id` is present in the
   source, is `MIGRATED_RECORD`: not a loss, not an addition, reported on its own line of the
   report.
2. The evidence is the pipeline's reconciliation document, `seed/upgrades/post-id-migration.<stem>.json`,
   which `migrate_post_ids.py` writes for exactly this reader. It is located from the posts
   profile alone (`<stem>.posts.json`), and its `profile` field must name the file being guarded.
   Nothing is inferred: not from a title, not from a member set, not from a resemblance. An id
   the document does not record stays `MISSING_RECORD`.
3. The consumer validates the document one-to-one before it compares anything, and refuses
   non-overridably when it cannot: unparseable; a missing or malformed move; a `profile` naming
   another file; one `old_id` recorded twice; two `old_id` values landing on one `new_id`; an id
   on both sides (an uncomposed chain); a mapped `new_id` the source does not hold; an `old_id`
   the source still holds. An absent or invalid mapping is a loss, never a migration.
4. Migration excuses nothing carried across the move. The moved record is compared against its
   new self with **only the identity key excluded**: a `MISSING_KEY` or `EMPTIED_VALUE` across the
   move refuses exactly as on an unmoved record, `CHANGED_VALUE` stays report-only, and the id
   transition itself never surfaces as a change.
5. Duplicate ambiguity still refuses, non-overridably, on either side: two source records under
   one id (Decision 3), or two `old_id` values claiming one `new_id` in the document.

Decisions 1 to 5 and the two measurement amendments above are untouched, and `MANIFEST.json`
comparisons produce the verdicts they produced before. Witness, with the document consumed:
site_a reports 175 migrations and 0 losses, site_b 336 and 0, with 0 identity-key changes on
either, and the same `MANIFEST.json` verdicts as before (0 losses; 42 and 440 reported changes).

## Amendment, 2026-09-22 (#1319): a source-authenticated retirement is not a deletion either

Two catalogue records can describe the same produced bytes owned by the same user. The app cannot
hold both: asset identity is `(owner_user_ref, file_hash)`, enforced by
`idx_assets_owner_hash_unique`, and no `DedupBehavior` value relaxes it (ADR 0011). One of the two
therefore never materializes at all: no id, no declaration, no size, no field values, and the post
naming it silently ships with one member fewer. On the committed site_a corpus there was exactly
one such pair, both owned by `priya.sharma`, both rendered at 512 px from the same 918-byte Kenney
vector, both 9,379 bytes.

Removing the loser is a deletion at the destination, so Decision 1 refuses it and
`--allow-regression` (Decision 4) is the only way through. That is the wrong tool for the same
reason it was the wrong tool for a migrated id: it waves through every loss rather than the one
thing that is not one.

**A retirement that is authenticated against the SOURCE is not a deletion.**

1. A destination record a validated collapse document retires onto a survivor the source holds is
   `COLLAPSED_RECORD`: not a loss, not an addition, not a change, not a migration, on its own line
   of the report.
2. The evidence is the committed document, `seed/upgrades/asset-collapse.<stem>.json`, located
   from the assets profile alone and naming it in its own `profile` field. It records one retired
   id, one survivor, the owner, the retired file path, the source hash and render size, the
   PRODUCED hash and the toolchain that produced it, the retired record verbatim, and every value
   the retirement costs, enumerated rather than inferred.
3. **Evidence is the produced artifact and never the destination.** The destination is an output,
   so a reading taken from it can describe what is currently staged and can authorise nothing. At
   publish, both records' produced files are located under the root their own `source_root` names,
   hashed, and required to be identical TO EACH OTHER within one build and equal to the recorded
   `materialized_sha256`. A png is byte-reproducible only within one sharp build, so the equality
   between the two files is the load-bearing claim and the recorded absolute value is re-derived:
   a mismatch refuses and names re-measurement. An IDAT-only comparison may appear in a refusal as
   a diagnostic and is never an acceptance path.
4. **Three hashes, never conflated.** `source_sha256` is the archive member before any render;
   `materialized_sha256` is the file the pipeline ships; `assets.file_hash` is the uploaded
   produced file and the app's live key. `metadata.source_archive.sha256` is not the app, content
   or storage hash, and a staged png is never compared against an SVG member hash. The invariant
   that matters is `(owner, produced_byte_sha256)`;
   `(owner, source_archive.sha256, render.px)` is a cheap repository-only PROXY of it, necessary
   but not sufficient, and is never described as complete.
5. **Two layers, proving different things, named apart.** Layer A is repository-local and runs
   where there is no pack, no pool and no dataset source: it validates the document structurally
   and checks that each object is in one of exactly two states, Pending or Applied. A third state
   is a hard failure, never a normalisation, and it fails before any deletion, so a stale document
   can never remove data that changed underneath it. Layer B is the publish-time source
   authentication in §3. **Layer A never authorises a publish**, and every report line says which
   of the two it is speaking for.
6. This is strictly narrower than Decision 4. `--allow-regression` accepts any loss at an
   operator's word; a `COLLAPSED_RECORD` names one record, names its survivor, enumerates what is
   lost, and re-derives the bytes. An unauthenticated retirement is `MISSING_RECORD` and refuses,
   which is what it has always been. Absence of a document is not permission: every
   destination-only id stays a loss.
7. Removing the retired file from the destination is separate and narrow: only for an entry Layer
   B authenticated, only when the path belongs to no current record, and only when the bytes at
   that path hash to `materialized_sha256`. It is not `--prune`, shares none of its machinery, and
   a hash mismatch refuses rather than deletes.

Decisions 1 to 5 and every amendment above are untouched.

## Amendment, 2026-09-24 (#1319): the archive is the MAINTAINED DATASET for preserved roots

⛔ THE PREMISE OF THIS ADR'S TITLE NO LONGER HOLDS FOR EVERY ROOT, and nothing above is
retracted: the reasoning was correct for the pipeline that existed when it was written. That
pipeline had two ends. A source dataset at `/mnt/d/Projects/unraid_management/artist-alley_dataset`
was what the repository was built against, the published trees under
`/mnt/blackbox_archives/datasets/artist_alley` were purely OUTPUTS, and every rule followed from
that shape: the destination is never an input, a disagreement means the profile is stale, and the
fix is always to correct the profile.

The owner has permanently retired that source dataset. It no longer exists, and the maintained
datasets are the published trees. Authority is therefore per ROOT from here on.

### 1. Per-root authority, and the measurements that decide it

| root | authority | why |
|---|---|---|
| `hq`, `pack` | source-backed, UNCHANGED | still externally reproducible |
| `local`, and generated bytes held only in the archive | ARCHIVE-AUTHORITATIVE / preserved | nothing left to re-derive them from |
| `site`, `internet`, `torrent_import` | unchanged | pre-staged, as before |

`local` has no other copy, measured on the committed profiles: site_a 696 records and site_b 552,
of which **0** carry `metadata.media_url`, **0** carry `metadata.source_archive`, and
`metadata.sha256` appears on only **2** (the unpublished authored plates). A `local` byte count is
therefore a MEASUREMENT of the archive's own bytes, and a profile that disagrees with it is a
`CORRUPTED_MEASUREMENT` rather than a stale publish. Cost today: **0** disagreements.

⛔ `hq` AND `pack` ARE NOT RECLASSIFIED WITH IT, and the reason is measured rather than preferred.
`hq` rebuilds from the Kenney pack through the committed `seed/upgrades/kenney-hq-pool.json`, and
`pack` copies or extracts members verified against `metadata.source_archive.sha256`, which 378 of
378 site_a `pack` records carry. The published trees already disagree with the profiles on
`file_size_bytes` for **1** record (site_a) and **392** (site_b). Every single one of them is `hq`,
because the share holds an older pool build. Reclassifying `hq` would turn all 393 into
`CORRUPTED_MEASUREMENT` and refuse every publish, so the measurement is the argument.

### 2. `preserved_archive` is a WEAKER claim than `produced_source`, and is named as one

The collapse evidence schema is now discriminated by a REQUIRED `evidence.kind`:

- `produced_source` keeps every requirement it had: a source hash and a materialized hash that must
  DIFFER, a member, a render size, a tool, and both `retired_record` cross-checks. Layer B locates
  both produced files under the root their `source_root` names and requires them equal to each
  other in one build.
- `preserved_archive` carries a `retired_sha256` and a `survivor_sha256` under distinct field names,
  and those two MUST be EQUAL: that equality IS the documented byte collapse. It fabricates no
  member, no render size, no tool and no `source_archive` claim, because a `local` record has none.
  The cross-checks that apply are the ones that exist for `local`: `id`, `owner_username`,
  `source_root`, `file_path` and a non-empty `source_path`.

⛔ `kind` HAS NO DEFAULT. An unlabelled entry is refused, because a default would have to choose,
and choosing `produced_source` would hand the stronger authority to an entry nobody labelled. The
one committed document (`asset-collapse.studio-a.json`) is MIGRATED rather than grandfathered.
⛔ A preserved entry carrying produced-source-only fields is REFUSED, not ignored: an ignored field
reads as one that was honoured.

### 3. A preserved claim rests on a frozen snapshot, never on the archive agreeing with itself

⛔ A STALE COPY AGREES WITH ITSELF PERFECTLY. So `preserved_archive` authentication takes a FROZEN
PRE-OPERATION SNAPSHOT plus an EXTERNAL path-to-sha256 manifest recorded immediately after that
snapshot was taken, and the relevant snapshot hashes are RECOMPUTED against the manifest
immediately before the comparison, every run. A mismatch refuses. Authentication from the live tree
refuses and authentication from the staging copy refuses, because a tree the run writes to cannot be
the evidence for what it writes. ⛔ Permissions and mtimes are NOT integrity proof: a CIFS tree can
change under both, so only bytes count.

### 4. Alias refusal, before any mutation and before any evidence is used

Equality is not the only way for a source to be the destination. A source that CONTAINS the
destination, or sits inside it, reads bytes the run is about to write. Every path a run knows about
is checked in all three shapes (every `--*-source`, the frozen snapshot, the snapshot manifest,
the transform document, the collapse and migration documents, the authored-plate scratch directory,
and the destination) on RESOLVED paths, so `..`, a symlink or a trailing slash cannot defeat it.
Two read-only source roots may legitimately be one tree; nothing is ever exempt from the comparison
against the tree the run WRITES.

### 5. `groups.csv` is preservation-owned, exactly

Any byte change fails the ordinary preservation check. It needs no transformation: `asset_count` is
an ORIGINAL-DATASET fact that already disagrees with the shipped subset. On site_b `grp-00219`
states 8 and ships 3, `grp-00215` states 8 and ships 2, and 262 of 1,047 rows disagree. And no
group loses its last shipped member (0 of 1,047 ship nothing). ⛔ DO NOT REINTERPRET OR REWRITE
`asset_count`: it describes the dataset the rows came from, not the cut this site ships, and
"correcting" it would overwrite a fact with a derivation.

### 6. `metadata.csv` changes only by an EXPECTED-TRANSFORM document

The regeneration this ADR's original Decision took for granted is removed from preserved mode, and
the reason is measured. It kept a row only when its `file_path` was in the profile's SOURCE-path
map and rewrote that column to the destination path; the published column ALREADY holds destination
paths, so pointed at the archive it matched **0 of 907** site_a rows and **0 of 1,206** site_b rows
and wrote a HEADER-ONLY file. Nothing noticed, because `metadata.csv` was not preservation-owned.

⛔ AND IT DOES NOT BECOME AN ORDINARY EXACT-BASELINE FILE. A retirement legitimately removes its
row, so exact bytes would refuse the one change that is correct, and a waiver would have permitted
the header-only file. The document is narrower than either. Produced BEFORE the operation from the
frozen pre-op CSV and the committed collapse document, kept outside every site tree, it records the
original sha256 and row count, the exact 41-field header and its hash, each removed `file_path` tied
to its `retired_id`, the expected transformed sha256 and row count, and an ordered digest of the
retained rows. Verification refuses an extra removal, a missing documented removal, a changed
retained row, a REORDERED retained row, a changed header, a wrong row count and a wrong whole-file
hash. Site_a's real transform is the ZERO-REMOVAL one: 907 rows to 907, byte-identical, which is an
expectation to enforce rather than a case to skip.

⚠️ The retained rows are filtered BY BYTE, not parsed and rewritten. Measured on the real published
files: both use CRLF terminators and site_b's carries 9 BARE LFs inside quoted fields (1,216 LF
bytes against 1,206 logical rows). A `csv`-module round trip would re-quote and re-terminate those,
so "the retained rows are unchanged" would have been false the first time it ran.

### 7. Authored outputs are built externally and installed explicitly

The two studio plates are reproducible outputs built from attested archive-held
`images/aurora-generated`, into a scratch directory outside every site tree, and then installed as a
separate step checked against the sizes and hashes the profile already records. ⛔ The publish NEVER
synthesizes them: a run that can manufacture the bytes it is about to attest has no independent
evidence left. An install that adds an unexpected third output is refused, because the uploader
hands the whole site to the world.

### What is untouched

Decisions 1 to 5 and every amendment above stand as written. The profile remains authoritative for
CONTENT; the destination-holds-what-the-source-does-not refusal, the migration-aware and
retirement-aware readings, `--allow-regression`'s narrowness, and the entire `produced_source`
Layer B are unchanged. This amendment adds a second authority class and the evidence that class
requires; it removes no guard.
