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
DOC_SETS = ("added", "balance", "pexels", "mature")

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


def apply_replacements(profile: list[dict], replacements: list[dict]) -> tuple[int, list[str]]:
    """Point replaced records at their HQ file. Returns (changed, problems)."""
    by_id = {e["id"]: e for e in profile}
    changed = 0
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
        changed += 1
    return changed, problems


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


def merge_posts(posts: list[dict], added: list[dict]) -> int:
    have = {p["id"] for p in posts}
    n = 0
    for p in added:
        if p["id"] in have:
            continue
        posts.append(json.loads(json.dumps(p)))
        n += 1
    return n


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
    profile = load(args.profile)
    posts = load(args.posts)

    before_assets, before_posts = len(profile), len(posts)

    corrected = apply_team_corrections(profile, corrections)
    changed, problems = apply_replacements(profile, reps)
    n_assets, n_repaired = merge_added(profile, add_a)
    n_posts = merge_posts(posts, add_p)
    problems += audit(profile, posts, reps, add_a, add_p)

    print(f"site        : {args.site}", file=sys.stderr)
    print(f"replacements: {changed}/{len(reps)} records repointed at the HQ pool",
          file=sys.stderr)
    for desc, n in corrected:
        print(f"correction  : {n:5d}  {desc}", file=sys.stderr)
    print(f"assets      : {before_assets} -> {len(profile)} (+{n_assets})",
          file=sys.stderr)
    print(f"posts       : {before_posts} -> {len(posts)} (+{n_posts})",
          file=sys.stderr)
    print(f"media_url   : {n_repaired} existing record(s) backfilled (#602)",
          file=sys.stderr)

    if problems:
        print(f"\n{len(problems)} PROBLEM(S):", file=sys.stderr)
        for p in problems[:20]:
            print(f"  - {p}", file=sys.stderr)
        if len(problems) > 20:
            print(f"  … and {len(problems) - 20} more", file=sys.stderr)
        return 1

    if args.check:
        # In --check mode nothing may have needed doing.
        drift = n_assets or n_posts or n_repaired or any(n for _, n in corrected)
        if drift:
            print("\nFAIL: profile is not upgraded — re-assembly would drop "
                  f"{n_assets} assets and {n_posts} posts, and {n_repaired} "
                  "record(s) are missing the media_url that makes them "
                  "re-fetchable. Run without --check.", file=sys.stderr)
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
