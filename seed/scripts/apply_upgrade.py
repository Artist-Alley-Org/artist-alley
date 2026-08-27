#!/usr/bin/env python3
"""
Apply the #604 dataset upgrade to a studio profile, so re-assembly
reproduces the upgraded library instead of regenerating the originals.

THE BUG THIS FIXES
------------------
The image upgrade and the video additions were applied by hand, directly
to the per-site `MANIFEST.json` on the archive share. That worked, and
the running sites are correct. But it left the *pipeline* describing the
old world, and the pipeline wins on the next run:

    source metadata.csv ──► sanitize_and_assemble.py ──► seed/profiles/studio-X.assets.json
                                                                  │
                                                                  ▼
                                                        populate_archive.py
                                                            │        │
                                              MANIFEST.json ◄┘        └─► metadata.csv
                                              (straight copy)             (filtered from
                                                                           source CSV, paths
                                                                           rewritten from the
                                                                           PROFILE)

`populate_archive.py` copies the profile *over* MANIFEST.json (line ~277)
and rewrites the per-site metadata.csv from the profile's path map. So a
single re-run would have restored 916 tiny images and dropped all 72
videos — regardless of the state of any metadata.csv, because the per-site
CSV is an OUTPUT of this process, not an input to it.

That is why this tool reconciles the PROFILE. Fixing the per-site
metadata.csv directly (the obvious reading) would be undone by the very
next run, because that file is regenerated from the profile every time.

WHAT IT DOES
------------
1. Replacements — for each row of `kenney-hq-replacements.<site>.json`,
   rewrite the profile entry with that id to point at the HQ file:
   file_path, source_root/source_path (so the copier can find it),
   file_size_bytes, title, and the LICENCE. Everything that defines
   composition — id, group_id, collection_name, tags, team_name, owner,
   workflow state — is left strictly alone. That "swap the file, keep the
   record" property is what let #565's group-first post composition and
   collection membership survive the upgrade, and it is asserted below
   rather than hoped for.

2. Added assets — merge the video records that were added directly to the
   manifest, giving them the source_root/source_path provenance that
   `populate_archive.py` requires. Without those two fields the copier
   silently skips a record (its path_map comprehension drops any entry
   missing source_path), so the videos would vanish on re-assembly even
   though their records existed.

3. Added posts — merge the solo posts that make those videos reachable.
   An asset with no post is invisible on browse, so the post is part of
   the deliverable, not a nicety.

LICENCE CORRECTNESS
-------------------
Replacement rewrites the licence to CC0, and that is a correctness fix,
not bookkeeping: the records being overwritten carried licences like
"Internal" from the synthetic source dataset. Keeping the old licence
string while serving Kenney bytes would be a false declaration — and
site_a is published. Per-asset `license` + `attribution` in the manifest
are authoritative for the ATTRIBUTIONS/Kaggle paperwork, so they have to
describe the bytes actually shipped.

Idempotent: running twice is a no-op. Safe to run on an already-upgraded
profile, which is what makes it usable as a pipeline stage.

Usage
-----
    python3 apply_upgrade.py --site site_a \\
        --profile seed/profiles/studio-a.assets.json \\
        --posts   seed/profiles/studio-a.posts.json \\
        [--upgrades seed/upgrades] [--check] [--dry-run]

    --check   exit non-zero if the profile is NOT already upgraded
              (for CI / a pre-publish gate; changes nothing)
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from collections import Counter
from pathlib import Path

# Every replacement asset comes from the Kenney All-in-1 pack, which is
# CC0 across the board. Uniform by construction — asserted in the tests.
HQ_LICENSE = "CC0 1.0"
HQ_ATTRIBUTION = "Kenney (kenney.nl)"

# Source root name for HQ pool files. populate_archive.py resolves this
# to the pool directory via --hq-source.
HQ_SOURCE_ROOT = "hq"

# Assets whose bytes are already staged at the destination, with no LOCAL
# source to copy from. populate_archive.py treats this as a PRE-STAGED
# root: verify presence, never drop. It is the same mechanism the
# pre-existing `torrent_import` root uses.
#
# These records used to be a dead end on a machine without the archive
# share, because their provenance was a Pexels *page* URL — an HTML
# document, not bytes. They now also carry `metadata.media_url`, the
# direct CDN URL, recorded only after a HEAD confirmed it serves exactly
# `file_size_bytes` (#602). So "pre-staged" now means "verify, and
# download if absent" rather than "verify or lose it".
SITE_SOURCE_ROOT = "site"

# Bytes copied verbatim out of the "Kenney Game Assets All-in-1" bundle
# (#572). populate_archive.py resolves this against --pack-source, and
# falls back to extracting the file from the pack's own CC0 zip when the
# bundle is not on the machine. See kenney_pack_sources.py.
PACK_SOURCE_ROOT = "pack"

# ⛔ WHO WE MAY SAY "AI" ABOUT (#1260).
#
# `ai_provenance` is a claim about HOW THE BYTES WERE MADE, attached to
# a record that also names a creator — so writing it on someone else's
# work publishes a false statement about that person. This dataset had
# four such records: `ai-declarations.site_a.json` and its site_b twin
# declared `generated` on four Kenney.nl works, in a dataset that ships
# to Kaggle with `attribution: "Kenney (kenney.nl)"` on the same row.
# They never reached the archive share, but a re-fold would have applied
# them, and nothing in this file would have said a word.
#
# So the rule is positive, not a deny-list of sources we happen to know:
# an asset may declare AI only when its own provenance says WE made it.
# Widening this is a deliberate act with a real creator on the other end
# of it — which is the point of making it a constant with a name.
#
# ⚠️ THE PREFIXES MEAN "WE MADE IT", NOT "AI MADE IT". That distinction
# only started to matter with #1290. `none` is a declaration too — it says
# no generative model was involved — and it is subject to the same rule,
# because asserting it on someone else's work is a false disclosure about
# that person just as `generated` is. But an artifact we made WITHOUT a
# model cannot honestly carry "Generated in-house (Stable Diffusion 3.5
# Large via ComfyUI)", so before #1290 there was no provenance string a
# truthful `none` could stand on. "Authored in-house" is that string:
# ours, and silent about AI. Both prefixes gate every state equally.
AI_DECLARABLE_SOURCE_PREFIXES = ("Generated in-house", "Authored in-house")

# Upgrade doc pairs merged into every profile, in order. `added` is
# #604/#602's video + internet material; `balance` is #572's per-team
# fill. Kept as separate files rather than one because they answer
# different questions and a 900-record append into the 36-record doc
# would bury the Pexels provenance work in noise.
# The upgrade documents this script merges, by stem. Each contributes
# `<stem>-assets.<site>.json` + `<stem>-posts.<site>.json`.
#
# `mature` is #1217's: twelve Met Open Access works labelled
# `mature: true`, plus the two posts that carry them. It is here rather
# than applied by hand to the archive share for the reason this file's
# docstring is about — the profile is the INPUT, and a manifest edited
# in place is undone by the next assembly.
#
# `generated` is #1260's: 45 images produced in-house with Stable
# Diffusion 3.5 Large, four per team across all eleven teams, plus the
# twelve posts that carry them. Each record declares its own
# `ai_provenance: "generated"` — `merge_added` deep-copies the whole
# record, so a NEW asset's declaration needs no separate mechanism. That
# is the difference between this doc set and `ai-declarations.*`, which
# exists only for declarations ABOUT records the source CSV already
# owns.
#
# ⛔ THE DECLARATION AND THE BYTES ARRIVE TOGETHER, ON PURPOSE. The two
# `ai-declarations.site_*.json` docs that used to sit beside these were
# DELETED in #1260 because they declared `generated` on four Kenney.nl
# works — a false statement about a named real creator, in a dataset
# that is published. An asset that declares AI must be an asset we
# actually generated, and the safest way to guarantee that is for the
# declaration to ride in on the same record as the file.
# `authored` is #1290's: the two studio plates that let the corpus declare
# the OTHER two AI states. `generated` was the only one the dataset had —
# `assisted` and `none` existed solely as soft-deleted test fixtures, so
# neither had ever been rendered for a human being, and `none` is the one
# a wrong rendering damages most.
#
# ⛔ THEY ARE NEW BYTES BECAUSE RE-LABELLING WAS NOT AVAILABLE. Every
# other asset in this corpus is a third party's work or one of the 45
# Stable Diffusion plates that already declare `generated` with their
# provenance saying so on the same row. Writing `none` onto somebody
# else's photograph is the disclosure ADR 0094 forbids, and re-declaring
# an SD plate `assisted` would contradict its own acquisition_source —
# the #1260 error, exactly. See `authored_plates.py` for how the bytes
# are made and why each label is true of the artifact it names.
DOC_SETS = ("added", "balance", "pexels", "mature", "generated", "authored")

_HASH_SUFFIX_RE = re.compile(r"-[0-9a-f]{8}(?:-\d+)?$")
_CATEGORY_PREFIX_RE = re.compile(
    r"^(?:2d-assets|3d-assets|ui-assets|icons|audio|other|goodies|archive|early-access)-"
)


def title_for(pool_filename: str) -> str:
    """Human title for an HQ pool file.

    `2d-assets-brick-pack-brick-high-1-36a68e65-512.png`
        -> `Brick pack brick high 1 (vector)`

    The category prefix is dropped (it is a directory of the source pack,
    not information about the picture) and rendered vectors are marked so
    a browsing user can tell a re-render from an original bitmap.
    """
    stem = re.sub(r"\.(png|jpg|jpeg)$", "", pool_filename, flags=re.I)
    is_vector = bool(re.search(r"-[0-9a-f]{8}-\d+$", stem))
    stem = _HASH_SUFFIX_RE.sub("", stem)
    stem = _CATEGORY_PREFIX_RE.sub("", stem)
    title = stem.replace("-", " ").strip().capitalize()
    return f"{title} (vector)" if is_vector else title


def load(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


def dump(path: Path, data) -> None:
    path.write_text(json.dumps(data, indent=1, ensure_ascii=False) + "\n",
                    encoding="utf-8")


def apply_replacements(profile: list[dict],
                       replacements: list[dict]) -> tuple[int, int, list[str]]:
    """Point replaced records at their HQ file.

    Returns (processed, modified, problems).

    ⛔ THE TWO COUNTS ARE NOT THE SAME NUMBER, AND CONFLATING THEM BLINDED
    THE PRE-PUBLISH GATE (#1295). This function used to return one count
    incremented once per record, unconditionally — so it reported
    `260/260 records repointed at the HQ pool` whether the profile was
    already upgraded or 86 byte counts out of date. `--check`'s drift
    expression therefore could not use it: adding a number that is never
    zero would have failed every run. The pass was left out of the
    expression instead, and a profile with drifted replacements passed
    the gate for as long as it took someone to notice by hand.

    So:

    - **processed** is progress — records this doc names that the profile
      actually holds. An id the profile lacks is a `problem`, not a
      processed record, which is why this is not simply `len(replacements)`.
    - **modified** is drift — records whose CONTENT this pass changed. On
      an upgraded profile it is 0, and that is what makes it usable in a
      drift expression.

    ⭐ MODIFIED IS MEASURED OVER THE WHOLE RECORD, not over the list of
    keys written below. Enumerating the keys would put the drift check and
    the writes in two places that must be kept in step by hand, and the
    next key added to this loop would go unwatched exactly the way the
    whole pass did. A record is modified if it is not byte-identical after
    the pass, whatever this function grew a write for.

    ⚠️ `_composition` is NOT that comparison and cannot stand in for it.
    It snapshots the fields the swap must LEAVE ALONE (group, collection,
    team, workflow …), so `before != after` on it is a violation of the
    pass's contract, not evidence the record moved — it is 0 on a run that
    rewrites every record and 0 on a run that rewrites none.
    """
    by_id = {e["id"]: e for e in profile}
    processed = 0
    modified = 0
    problems: list[str] = []
    for r in replacements:
        entry = by_id.get(r["id"])
        if entry is None:
            problems.append(f"replacement id {r['id']} not present in profile")
            continue
        new_name = r["new"].rsplit("/", 1)[-1]

        # Snapshot the fields that define composition. Nothing below may
        # touch them; verified immediately after.
        before = _composition(entry)
        # And the record entire, for the modified count above.
        before_record = _fingerprint(entry)

        # Remember where this record came from BEFORE repointing it.
        # `source_path` has two consumers with different needs:
        #   - the byte copier wants the HQ pool file (set below);
        #   - the per-site metadata.csv filter matches source rows by
        #     their ORIGINAL path, and silently drops any row it cannot
        #     match. Overwriting source_path alone would therefore delete
        #     916 rows from the shipped CSV — the documentation half of
        #     the dataset — while every image looked fine.
        # Only meaningful for records that came from the local dataset
        # tree; internet-sourced ones have no CSV row to preserve.
        if entry.get("source_root", "local") == "local" and entry.get("source_path"):
            entry.setdefault("replaced_source_path", entry["source_path"])

        entry["file_path"] = r["new"]
        entry["source_root"] = HQ_SOURCE_ROOT
        entry["source_path"] = new_name
        entry["file_size_bytes"] = r["newSize"]
        entry["file_extension"] = new_name.rsplit(".", 1)[-1]
        entry["title"] = title_for(new_name)
        entry["license"] = HQ_LICENSE
        entry["attribution"] = HQ_ATTRIBUTION
        meta = entry.setdefault("metadata", {})
        meta["filename"] = new_name
        meta["license"] = HQ_LICENSE
        meta["attribution"] = HQ_ATTRIBUTION

        after = _composition(entry)
        if before != after:
            problems.append(
                f"composition changed for {r['id']}: {before} -> {after}")
        processed += 1
        if _fingerprint(entry) != before_record:
            modified += 1
    return processed, modified, problems


def _fingerprint(entry: dict) -> str:
    """A stable serialisation of a whole profile record.

    Sorted keys so a re-ordered dict is not read as a change, and
    `ensure_ascii=False` so a non-ASCII title compares as itself.
    """
    return json.dumps(entry, sort_keys=True, ensure_ascii=False)


def _composition(entry: dict) -> tuple:
    """The fields that must survive a file swap untouched.

    group_id drives #565's group-first post composition; collection_name
    drives membership; team/owner/workflow drive the permission and
    review surfaces. Swapping the bytes must not move an asset between
    any of them.
    """
    return (
        entry.get("id"),
        (entry.get("metadata") or {}).get("group_id"),
        entry.get("collection_name"),
        tuple(entry.get("tags") or ()),
        entry.get("team_name"),
        entry.get("owner_username"),
        entry.get("brand_workspace"),
        entry.get("workflow_state"),
        entry.get("asset_type"),
    )


def merge_added(profile: list[dict], added: list[dict]) -> tuple[int, int]:
    """Append records absent from the profile, with copier provenance.

    Returns (appended, repaired). `repaired` counts records that were
    ALREADY in the profile and gained a `metadata.media_url` from the
    upgrade doc — see below.
    """
    by_id = {e["id"]: e for e in profile}
    n = 0
    repaired = 0
    for a in added:
        existing = by_id.get(a["id"])
        if existing is not None:
            # Already merged, so the append branch will never run again —
            # which is exactly how a field added to the upgrade docs
            # later would never reach the profiles. A provenance field
            # that is right for new rows and absent from old ones is its
            # own trap, so pull `media_url` forward on every run (#602).
            # Nothing else is touched: this is a repair, not a re-merge.
            src = (a.get("metadata") or {}).get("media_url")
            if src and not (existing.get("metadata") or {}).get("media_url"):
                existing.setdefault("metadata", {})["media_url"] = src
                repaired += 1
            continue
        rec = json.loads(json.dumps(a))  # don't mutate the committed doc
        # These records were injected straight into the manifest and have
        # no provenance, which makes populate_archive.py skip them
        # entirely. Mark them as staged-at-destination so the copier
        # verifies rather than drops them.
        if not rec.get("source_path"):
            rec["source_root"] = SITE_SOURCE_ROOT
            rec["source_path"] = rec["file_path"]
        profile.append(rec)
        n += 1
    return n, repaired


def apply_team_corrections(profile: list[dict],
                           corrections: list[dict]) -> list[tuple[str, int]]:
    """Move records the SOURCE CSV puts on the wrong team (#572).

    Idempotent by construction: a correction only fires on a record still
    sitting on the `from` team, so a second run moves nothing. Matched on
    the record's ORIGINAL source path, because `source_path` is rewritten
    to a pool filename by the #604 swap and stops identifying the pack.

    This runs BEFORE apply_replacements, so the composition snapshot that
    function takes already contains the corrected team and the "swap the
    file, keep the record" assertion still means what it says.
    """
    out: list[tuple[str, int]] = []
    for c in corrections:
        n = 0
        for e in profile:
            if e.get("team_name") != c["from"]:
                continue
            original = e.get("replaced_source_path") or e.get("source_path") or ""
            if c["match"] in original:
                e["team_name"] = c["to"]
                n += 1
        out.append((f"{c['from']} -> {c['to']} ({c['match']})", n))
    return out


def apply_ai_declarations(profile: list[dict],
                          declarations: list[dict]) -> list[tuple[str, str]]:
    """Write the maker's AI declaration onto named profile records (#1251).

    ⛔ NO SITE SHIPS ONE OF THESE DOCS ANY MORE (#1260), AND THE REASON
    IS THE WHOLE POINT OF THE FUNCTION. Both that existed — two records
    per site — declared `generated` on Kenney.nl works: `Brick pack brick
    medium slope inverted left 4`, `Animated characters retro preview`,
    `Fish pack terrain dirt top a outline`, `Planets planet01`. Every one
    of those rows also carries `attribution: "Kenney (kenney.nl)"` and
    `metadata.acquisition_source: "Kenney.nl"`, and site_a is published
    to Kaggle.

    HOW IT HAPPENED IS WORTH KEEPING, because the same trap is still
    open. The docs name records by id and describe them in prose, and
    the prose was written against the record's PRE-#604 identity —
    "Progress Blue Border", "Tile 0422 — lighting pass". `apply_replacements`
    above then swapped those records onto HQ pool files and rewrote
    `title`, `license` and `attribution` with them, exactly as it is
    meant to: swap the file, keep the record. So an id that once named a
    synthetic studio plate now names a Kenney work, the doc's prose still
    described the old one, and nothing compared the two. An id is not a
    stable description of what a record CONTAINS in a pipeline whose job
    is to change what records contain.

    The mechanism is kept because the need is real — a declaration ABOUT
    a record the source CSV already owns cannot ride in on `merge_added`,
    and `sanitize_and_assemble.py` regenerates the profiles from
    `metadata.csv`, so a hand-added key is dropped by the next assembly.
    What replaced the two docs is CONTENT WE ACTUALLY GENERATED: 45
    images made in-house with Stable Diffusion 3.5 Large, each declaring
    itself in `generated-assets.site_a.json`. `audit` now refuses any
    profile record that declares AI without in-house provenance, whichever
    route wrote it — see AI_DECLARABLE_SOURCE_PREFIXES.

    IT EXISTS so the browse footer's "Hide AI-made work" toggle has
    something to hide on a freshly seeded instance: every asset in the
    source CSV is UNDECLARED (the studio simulation has no notion of
    generative AI), so without a declared corpus the control is correct,
    wired end to end, and observably inert — the state that makes a
    reviewer conclude a feature is broken when it is working.

    WHY A DOC RATHER THAN AN EDIT TO THE PROFILE. Same reason as this
    file's docstring: `sanitize_and_assemble.py` regenerates the profiles
    from `metadata.csv`, so a hand-added key is dropped by the next
    assembly, silently, and the toggle goes inert again with nothing to
    notice it. The declaration is an input to the pipeline or it does not
    survive the pipeline.

    WHY IT MODIFIES RATHER THAN APPENDS. `merge_added` brings NEW records
    in; these are declarations ABOUT records the source CSV already owns,
    and the whole point is which existing posts they make pure. Nearest
    neighbour is `apply_team_corrections`, and this borrows its shape.

    ⚠️ THE ASSETS ARE CHOSEN, NOT PICKED. One asset row can be a member
    of MANY posts — the seeder collapses byte-identical uploads by the
    same owner, and the catalogue reuses preview plates across posts — so
    declaring a shared asset moves every post that contains it. Each
    record here names an asset that belongs to exactly ONE post, and the
    doc records which post and why, so the blast radius is checkable
    without a database.

    ⛔ IT DOES NOT TOUCH `metadata.acquisition_source`. That key is what
    the fixture sweep partitions the asset table on (ADR 0095); an asset
    the seeder wrote without it is indistinguishable from real uploaded
    content. A declared seeded asset carries both, which is why this
    writes one top-level key and nothing else.

    Idempotent by construction: it writes a value, so a second run writes
    the same value. Returns (id, outcome) per record so the caller can
    print an id that matched nothing rather than passing over it.
    """
    out: list[tuple[str, str]] = []
    by_id = {e.get("id"): e for e in profile}
    for d in declarations:
        aid = d["id"]
        want = d["ai_provenance"]
        entry = by_id.get(aid)
        if entry is None:
            out.append((aid, "MISSING from profile"))
            continue
        before = entry.get("ai_provenance")
        entry["ai_provenance"] = want
        out.append((aid, f"{before!r} -> {want!r} ({d.get('role', '?')})"))
    return out


def merge_posts(posts: list[dict], added: list[dict]) -> int:
    have = {p["id"] for p in posts}
    n = 0
    for p in added:
        if p["id"] in have:
            continue
        posts.append(json.loads(json.dumps(p)))
        n += 1
    return n


def dedupe_posts(posts: list[dict]) -> tuple[int, list[str]]:
    """Collapse posts sharing one id. Returns (removed, ids).

    ⛔ WHY THIS IS A PIPELINE PASS AND NOT A HAND EDIT. The assembler
    derives a roundup's id from `("post", "roundup", team_name,
    anchor.id)` and a sprint's from `("post", "sprint", project, label,
    anchor.id)` — neither of which is unique when the generator emits
    several roundups per team over different asset windows. Measured
    2026-08-26: eight ids in studio-a covering twelve rows, four in
    studio-b covering four, and the twins DISAGREE — one says "Props
    sprint roundup — 8 drops" with eight members, its twin says "— 10
    drops" with ten.

    A manifest cannot represent both. `aa seed` keys on the stable id, so
    it takes whichever row it reads last and the other silently never
    exists; `populate_archive.py` publishes both and pushes the coin toss
    into the dataset. Removing them here means the next assembly removes
    them too, which editing the profile by hand would not.

    WHICH ROW SURVIVES. The one with the most members, ties broken by
    first appearance. A roundup is its membership, so the richest row is
    the one that loses least, and the rule is a property of the DATA
    rather than of the order the generator happened to emit — the same
    reason `apply_replacements` keys on the record and not on position.
    ⚠️ It deliberately does NOT try to reproduce site_a/posts.json: that
    file agrees with the profile on membership for all 861 shared ids and
    disagrees on `created_at` for 840 of them, so it is a different
    assembly, not a deduplicated copy of this one.
    """
    # rank = (member count, -index): most members wins, earliest breaks
    # the tie because -index is larger for a smaller index.
    best: dict[str, tuple[int, int]] = {}
    for i, p in enumerate(posts):
        pid = p["id"]
        rank = (len(p.get("asset_ids") or ()), -i)
        if pid not in best or rank > best[pid]:
            best[pid] = rank
    keep = {pid: -rank[1] for pid, rank in best.items()}
    dup_ids = sorted(pid for pid, n in Counter(p["id"] for p in posts).items() if n > 1)
    before = len(posts)
    posts[:] = [p for i, p in enumerate(posts) if keep[p["id"]] == i]
    return before - len(posts), dup_ids


def apply_manifest_reconcile(profile: list[dict], doc: dict) -> tuple[int, int]:
    """Carry the archive share's advantage back into the profile (#1275).

    Returns (added_records, filled_values).

    ⛔ THIS PASS CAN ONLY EVER ADD. It writes a key the profile does not
    hold, or holds empty; it never replaces a value the profile already
    has. That is the safety property, and it is why the pass is
    idempotent and why re-running it cannot undo a later edit.

    ⚠️ IT IS ALSO WHY `file_size_bytes` IS NOT RECONCILED, which looks
    like an omission and is not. The share's site_a files disagree with
    the profile on 160 byte counts, and the share's number matches the
    bytes lying at the share on all 160. That is not enough to act on:
    the profile's `file_size_bytes` describes the SOURCE file that
    `populate_archive.py` copies — for 149 of the 160 that is a
    kenney-hq pool render, and `kenney-hq-replacements.site_a.json`
    records `newSize` for it — and the copier verifies the record
    against the source, not against whatever is already at the
    destination. So "the share matches its own bytes" says the share
    holds an older pool build, not that the profile is wrong; writing
    the share's number in would make the very next build refuse its own
    input. The pool is not on every machine that runs this script, so
    the disagreement cannot be adjudicated here. It is filed rather than
    guessed, and `manifest_guard.py` classifies it as CHANGED_VALUE —
    an edit, not a loss — so it never blocks a publish.
    """
    by_id = {a["id"]: a for a in profile}
    n_added = 0
    for rec in doc.get("added", ()):
        if rec["id"] in by_id:
            continue
        copied = json.loads(json.dumps(rec))
        profile.append(copied)
        by_id[rec["id"]] = copied
        n_added += 1

    filled = 0
    for entry in doc.get("fill", ()):
        target = by_id.get(entry["id"])
        if target is None:
            continue
        for key, val in entry.items():
            if key == "id":
                continue
            if isinstance(val, dict):
                sub = target.setdefault(key, {})
                if not isinstance(sub, dict):
                    continue
                for k2, v2 in val.items():
                    if k2 not in sub or _empty(sub[k2]):
                        sub[k2] = v2
                        filled += 1
                continue
            if key not in target or _empty(target[key]):
                target[key] = val
                filled += 1
    return n_added, filled


def _empty(v) -> bool:
    """`False` and `0` are values, not emptiness — losing `mature: false`
    loses a declaration."""
    return v is None or v == "" or v == [] or v == {}


def audit(profile: list[dict], posts: list[dict],
          replacements: list[dict], added_assets: list[dict],
          added_posts: list[dict]) -> list[str]:
    """Post-conditions. Every one of these has failed at least once."""
    problems: list[str] = []
    by_id = {e["id"]: e for e in profile}

    ids = [e["id"] for e in profile]
    if len(ids) != len(set(ids)):
        problems.append(f"{len(ids) - len(set(ids))} duplicate asset ids in profile")

    # RULE 1 — the collision check, at the dataset level. A pool that lost
    # assets to basename collisions produces a profile where two records
    # point at one file. Everything still "works"; the data is wrong.
    hq = [e for e in profile if (e.get("file_path") or "").startswith("images/kenney-hq/")]
    paths = [e["file_path"] for e in hq]
    if len(paths) != len(set(paths)):
        dupes = sorted({p for p in paths if paths.count(p) > 1})
        problems.append(
            f"{len(paths) - len(set(paths))} HQ records share a file_path "
            f"(e.g. {dupes[:2]}) — name collisions, see kenney_hq RULE 1")

    for r in replacements:
        e = by_id.get(r["id"])
        if e is None:
            problems.append(f"replacement {r['id']} missing from profile")
        elif e.get("file_path") != r["new"]:
            problems.append(f"replacement {r['id']} still points at "
                            f"{e.get('file_path')}")
        elif e.get("license") != HQ_LICENSE:
            problems.append(f"replacement {r['id']} has licence "
                            f"{e.get('license')!r}, expected {HQ_LICENSE!r}")

    for a in added_assets:
        e = by_id.get(a["id"])
        if e is None:
            problems.append(f"added asset {a['id']} missing from profile")
            continue
        if not e.get("source_path"):
            problems.append(
                f"added asset {a['id']} has no source_path — "
                "populate_archive.py will silently skip it")
        # A pre-staged record with no media_url is unrecoverable the
        # moment the archive share is not there: the copier can only
        # report it MISSING and re-assembly ends with a hole. That was
        # the whole of #602, and it is a post-condition now, not a note.
        meta = e.get("metadata") or {}
        if e.get("source_root") == SITE_SOURCE_ROOT and not meta.get("media_url"):
            problems.append(
                f"added asset {a['id']} ({meta.get('filename')}) is pre-staged "
                "with no metadata.media_url — its bytes cannot be re-fetched "
                "from provenance. Run: python3 seed/scripts/"
                "resolve_media_urls.py --write <profile>")
        if meta.get("media_url") and not meta.get("fetched_from"):
            problems.append(
                f"added asset {a['id']} has media_url but lost fetched_from — "
                "the source page is the attribution + licence evidence and "
                "must survive alongside the byte URL")
        # A bundle-sourced record is only reconstructible if it says WHICH
        # zip and WHICH member, and the sha256 is what turns that from a
        # plausible string into evidence — the byte count does that job
        # for a direct media_url, and an archive has no per-member count
        # to check (#572).
        if e.get("source_root") == PACK_SOURCE_ROOT:
            sa = meta.get("source_archive") or {}
            if not (sa.get("url") and sa.get("member") and sa.get("sha256")):
                problems.append(
                    f"added asset {a['id']} ({meta.get('filename')}) is "
                    "bundle-sourced with an incomplete metadata."
                    "source_archive — its bytes exist only inside a paid "
                    "bundle and cannot be re-fetched. Run: python3 "
                    "seed/scripts/studio_balance.py emit --pack <bundle>")
            if not meta.get("fetched_from"):
                problems.append(
                    f"added asset {a['id']} is bundle-sourced with no "
                    "fetched_from — the pack page is the CC0 evidence")

    # Every file_path in the profile must be unique, not just the HQ
    # ones. #572 introduced a second copied-bytes root, and two records
    # pointing at one destination is the same silent wrongness whichever
    # root produced it.
    all_paths = [e["file_path"] for e in profile if e.get("file_path")]
    if len(all_paths) != len(set(all_paths)):
        dupes = sorted({p for p in all_paths if all_paths.count(p) > 1})
        problems.append(
            f"{len(all_paths) - len(set(all_paths))} records share a "
            f"file_path (e.g. {dupes[:2]})")

    post_ids = {p["id"] for p in posts}
    for p in added_posts:
        if p["id"] not in post_ids:
            problems.append(f"added post {p['id']} missing from posts profile")

    # An asset nobody posted is invisible on browse. Every added video
    # must be reachable.
    referenced = {aid for p in posts for aid in (p.get("asset_ids") or ())}
    orphans = [a["id"] for a in added_assets if a["id"] not in referenced]
    if orphans:
        problems.append(f"{len(orphans)} added assets have no post and are "
                        f"unreachable on browse (e.g. {orphans[:2]})")

    # ⛔ NOBODY ELSE'S WORK MAY BE CALLED AI (#1260). Asserted over the
    # WHOLE profile rather than over the declaration doc, because the
    # claim can arrive by two routes — `apply_ai_declarations` writing it
    # onto an existing record, or `merge_added` carrying it in on a new
    # one — and only the finished profile sees both. See
    # AI_DECLARABLE_SOURCE_PREFIXES for what this cost the first time.
    for e in profile:
        if not e.get("ai_provenance"):
            continue
        src = (e.get("metadata") or {}).get("acquisition_source") or ""
        if not src.startswith(AI_DECLARABLE_SOURCE_PREFIXES):
            problems.append(
                f"asset {e['id']} ({e.get('title')!r}) declares "
                f"ai_provenance={e['ai_provenance']!r} but its provenance is "
                f"{src!r} — attributed to {e.get('attribution')!r}. An AI "
                "declaration on work we did not generate is a false statement "
                "about that creator, and this dataset is published. Either the "
                "declaration is wrong or the provenance is.")

    # Every copier-visible record needs a source root the copier knows.
    known_roots = {"local", "internet", "torrent_import",
                   HQ_SOURCE_ROOT, SITE_SOURCE_ROOT, PACK_SOURCE_ROOT}
    bad_roots = sorted({e.get("source_root") for e in profile
                        if e.get("source_root") not in known_roots})
    if bad_roots:
        problems.append(f"unknown source_root values: {bad_roots}")

    return problems


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--site", required=True, choices=("site_a", "site_b"))
    ap.add_argument("--profile", required=True, type=Path)
    ap.add_argument("--posts", required=True, type=Path)
    ap.add_argument("--upgrades", type=Path,
                    default=Path(__file__).resolve().parents[1] / "upgrades")
    ap.add_argument("--check", action="store_true",
                    help="verify only; non-zero exit if not already upgraded")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    reps = load(args.upgrades / f"kenney-hq-replacements.{args.site}.json")
    add_a: list[dict] = []
    add_p: list[dict] = []
    for stem in DOC_SETS:
        a = args.upgrades / f"{stem}-assets.{args.site}.json"
        p = args.upgrades / f"{stem}-posts.{args.site}.json"
        # site_b has no balance docs yet (#572 is site_a only), and a
        # missing doc must mean "nothing to merge", not a crash — the
        # site_b arm of this script runs on every assembly.
        if a.is_file():
            add_a += load(a)
        if p.is_file():
            add_p += load(p)
    corrections_doc = args.upgrades / f"team-corrections.{args.site}.json"
    corrections = load(corrections_doc) if corrections_doc.is_file() else []
    declarations_doc = args.upgrades / f"ai-declarations.{args.site}.json"
    declarations = load(declarations_doc) if declarations_doc.is_file() else []
    reconcile_doc = args.upgrades / f"manifest-reconcile.{args.site}.json"
    reconcile = load(reconcile_doc) if reconcile_doc.is_file() else {}
    profile = load(args.profile)
    posts = load(args.posts)

    before_assets, before_posts = len(profile), len(posts)

    corrected = apply_team_corrections(profile, corrections)
    # AFTER the team corrections and BEFORE the replacements, for the
    # same reason the corrections run first: apply_replacements takes a
    # composition snapshot and asserts "swap the file, keep the record".
    # A declaration is part of the record, so writing it first means that
    # assertion covers it too.
    declared = apply_ai_declarations(profile, declarations)
    n_processed, n_modified, problems = apply_replacements(profile, reps)
    n_assets, n_repaired = merge_added(profile, add_a)
    n_posts = merge_posts(posts, add_p)
    # LAST of the asset passes. The reconcile document is the archive
    # share's advantage (#1275), and the share reflects a library that
    # has already been through replacement, correction and merge — so
    # applying it before them would let a later pass overwrite the very
    # values it exists to restore.
    n_reconciled, n_filled = apply_manifest_reconcile(profile, reconcile)
    n_deduped, dup_ids = dedupe_posts(posts)
    problems += audit(profile, posts, reps, add_a, add_p)
    # A declaration naming an id this profile does not hold is a
    # PROBLEM, not a shrug. The failure it guards against is silent by
    # nature — the toggle keeps working, the wall keeps rendering, and
    # there is simply nothing to hide — so a mistyped or retired id has
    # to stop the run rather than be passed over in the log.
    problems += [f"ai declaration {aid}: {outcome}"
                 for aid, outcome in declared if outcome.startswith("MISSING")]

    print(f"site        : {args.site}", file=sys.stderr)
    print(f"replacements: {n_processed}/{len(reps)} records repointed at the "
          f"HQ pool ({n_modified} modified)", file=sys.stderr)
    for desc, n in corrected:
        print(f"correction  : {n:5d}  {desc}", file=sys.stderr)
    for aid, outcome in declared:
        print(f"ai-declare  : {aid}  {outcome}", file=sys.stderr)
    print(f"assets      : {before_assets} -> {len(profile)} "
          f"(+{n_assets} merged, +{n_reconciled} reconciled)", file=sys.stderr)
    print(f"posts       : {before_posts} -> {len(posts)} "
          f"(+{n_posts} merged, -{n_deduped} duplicate id row(s))", file=sys.stderr)
    print(f"reconcile   : {n_filled} value(s) filled from the share (#1275)",
          file=sys.stderr)
    print(f"media_url   : {n_repaired} existing record(s) backfilled (#602)",
          file=sys.stderr)
    if dup_ids:
        print(f"duplicate id(s) collapsed: {', '.join(dup_ids[:8])}"
              f"{' …' if len(dup_ids) > 8 else ''}", file=sys.stderr)

    if problems:
        print(f"\n{len(problems)} PROBLEM(S):", file=sys.stderr)
        for p in problems[:20]:
            print(f"  - {p}", file=sys.stderr)
        if len(problems) > 20:
            print(f"  … and {len(problems) - 20} more", file=sys.stderr)
        return 1

    if args.check:
        # In --check mode nothing may have needed doing. EVERY pass above
        # is a term here — a pass missing from this list is a pass the
        # pre-publish gate cannot see, which is what #1295 was.
        #
        # ⭐ AND EACH TERM CARRIES ITS OWN SENTENCE. The old message was
        # one sentence naming all seven counts, so a run where a single
        # pass had drifted still recited "would drop 0 assets and 0
        # posts, 0 record(s) are missing the media_url …" and the reader
        # had to find the non-zero number in it. A gate that reports six
        # things that did not happen alongside the one that did is
        # training people to skim it.
        drifted = [
            (n_modified,
             f"{n_modified} replacement record(s) disagree with "
             f"kenney-hq-replacements.{args.site}.json — a file_path, byte "
             "count, title or licence in the profile is stale (#1295)"),
            (n_assets,
             f"{n_assets} added asset(s) are not in the profile — "
             "re-assembly would drop them"),
            (n_posts,
             f"{n_posts} added post(s) are not in the posts profile — "
             "their assets would be unreachable on browse"),
            (n_repaired,
             f"{n_repaired} record(s) are missing the metadata.media_url "
             "that makes them re-fetchable (#602)"),
            (n_reconciled,
             f"the share's reconcile document holds {n_reconciled} asset(s) "
             "the profile does not (#1275)"),
            (n_filled,
             f"{n_filled} value(s) present at the share are absent or empty "
             "in the profile (#1275)"),
            (n_deduped,
             f"{n_deduped} post row(s) still share an id with another — "
             "one of each pair would silently never exist"),
            (sum(n for _, n in corrected),
             f"{sum(n for _, n in corrected)} record(s) still sit on the "
             "team the source CSV put them on (#572)"),
        ]
        fired = [msg for n, msg in drifted if n]
        if fired:
            print("\nFAIL: profile is not upgraded — "
                  f"{len(fired)} pass(es) would change it:", file=sys.stderr)
            for msg in fired:
                print(f"  - {msg}", file=sys.stderr)
            print("Run without --check.", file=sys.stderr)
            return 1
        print("\nOK: profile already reflects the upgrade.", file=sys.stderr)
        return 0

    if args.dry_run:
        print("\n(dry run — nothing written)", file=sys.stderr)
        return 0

    dump(args.profile, profile)
    dump(args.posts, posts)
    print(f"\nwrote {args.profile}\nwrote {args.posts}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
