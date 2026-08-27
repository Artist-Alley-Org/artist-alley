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
