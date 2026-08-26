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
