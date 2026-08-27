#!/usr/bin/env python3
"""
Record what a published site ACTUALLY SHIPS, so the manifest stops
describing a file the dataset does not contain (#1301).

THE BUG
-------
Eleven video records in each profile carried the ORIGIN's byte count
while the dataset shipped a deliberate two-minute cut. Measured
2026-08-27 against the share, the overstatement totalled 2,690,105,638
bytes — `videos/internet/sintel-2010-1080p.mkv` alone claimed
1,172,428,172 B for a 179,941,478 B file.

That is not only a documentation defect. Two consumers read the number:

  * `populate_archive.py`'s re-fetch (#602) validates a download against
    it, so a missing cut was replaced by the 1.1 GB ORIGINAL and the
    length check PASSED. A wrong file staged silently, which is exactly
    what that check exists to prevent.
  * the Go seeder's coverage selection (`app/internal/seed/coverage.go`)
    costs every asset by it to decide what a shallow seed can afford —
    `catalogues.go:96` calls the field advisory because the seeder writes
    the size it measures off disk, but coverage still SELECTS on it. So
    the seeder was budgeting 2.7 GB it never spends.

WHAT THE FIELDS MEAN NOW
------------------------
    file_size_bytes         the SHIPPED bytes. One meaning, every record.
    metadata.origin_bytes   what `metadata.media_url` serves, when that
                            is not the same thing. Absent when they agree.
    metadata.sha256         the hash of the record's SOURCE_ROOT file.
                            For a pre-staged root the destination IS the
                            source, so it is the shipped hash.

⛔ `metadata.sha256` IS NEVER RE-MEASURED FOR AN `internet` RECORD.
It is the hash `fetch_gaps.py:1084` took of the download, and
`sanitize_and_assemble.py:1517` derives the record's whole IDENTITY from
it — `stable_uuid("asset", "internet", sha256)`, plus the three
timestamps at `:1558-1560`. Re-measuring it against a staged cut would
move asset ids on the next assembly. The origin hash is correct where it
is; it simply describes the download rather than the ship.

WHY A DOCUMENT RATHER THAN A LIVE READ
--------------------------------------
The share is not on every machine that runs the assembly, so a pass that
can only run where it is mounted is a pass CI cannot check. The
measurement is therefore EMITTED once from a mounted share into
`seed/upgrades/staged-measurements.<site>.json` and applied from there,
the same shape `kenney-hq-replacements.<site>.json` and
`manifest-reconcile.<site>.json` already use.

⛔ ONLY PRE-STAGED AND `internet` ROOTS ARE MEASURABLE THIS WAY.
An `hq`, `local` or `pack` record is copied FROM a reproducible source,
and the share can be STALE relative to it: measured 2026-08-27, 392 of
site_b's 656 staged pool files disagree with a freshly built pool while
the profile agrees with that pool on all 656. Adopting the share's
number for those would make the very next build refuse its own input —
the reasoning `apply_upgrade.apply_manifest_reconcile` already records.
For a pre-staged root there is no source to be stale against: the
destination IS the artifact.

Usage
-----
    # emit, on a machine with the share mounted (READ ONLY)
    python3 measure_staged.py emit \\
        --profile seed/profiles/studio-a.assets.json \\
        --site    /mnt/blackbox_archives/datasets/artist_alley/site_a \\
        --out     seed/upgrades/staged-measurements.site_a.json

    # verify a profile against a mounted share (no writes)
    python3 measure_staged.py verify \\
        --profile seed/profiles/studio-a.assets.json \\
        --site    /mnt/blackbox_archives/datasets/artist_alley/site_a
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

# Roots whose bytes are staged AT the destination with no local source to
# copy from, so the destination is the only description of them there is.
# Kept in step with populate_archive.PRESTAGED_ROOTS.
PRESTAGED_ROOTS = frozenset({"torrent_import", "site"})

# `internet` has a source — the gitignored fetch cache — but what SHIPS
# is a cut of it, and the shipped bytes are what the manifest must
# describe. Its `metadata.sha256` still belongs to the download.
MEASURABLE_ROOTS = PRESTAGED_ROOTS | {"internet"}

# Roots copied from a reproducible source. The share may lag them, so a
# disagreement means "the share is stale", never "the record is wrong".
SOURCE_BACKED_ROOTS = frozenset({"local", "hq", "pack"})


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(8 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _formatting_of(path: Path) -> tuple[int, str, bool]:
    """(indent, trailing, ensure_ascii) as the file on disk already uses.

    The profiles are not written by one tool and normalising one would
    bury a 30-line edit in a 111,000-line diff. Same helper as
    `resolve_media_urls._formatting_of`, kept local so this module has no
    import-time dependency on it.
    """
    text = path.read_text(encoding="utf-8")
    indent = 1
    lines = text.split("\n", 2)
    if len(lines) > 1:
        stripped = lines[1].lstrip(" ")
        indent = len(lines[1]) - len(stripped) or 1
    return indent, ("\n" if text.endswith("\n") else ""), text.isascii()


def dump_like(path: Path, data, fmt: tuple[int, str, bool]) -> None:
    indent, trailing, ensure_ascii = fmt
    path.write_text(
        json.dumps(data, indent=indent, ensure_ascii=ensure_ascii) + trailing,
        encoding="utf-8")


def measure(profile: list[dict], site: Path,
            want_hashes: bool = True) -> tuple[list[dict], list[str]]:
    """Measure every measurable record against the bytes at `site`.

    Returns (entries, notes). An entry is emitted ONLY when the staged
    bytes disagree with the record, so the document stays a list of
    corrections rather than a second copy of the manifest.

    ⚠️ A record whose staged file is ABSENT produces no entry and a note.
    Absent is not zero: emitting `bytes: 0` for an unmounted or
    part-copied share would publish a manifest describing nothing, and
    the re-fetch would then accept an empty file. Unavailable is not the
    same as absent, and neither is the same as measured.
    """
    entries: list[dict] = []
    notes: list[str] = []
    for rec in profile:
        root = rec.get("source_root", "local")
        fp = rec.get("file_path")
        if not fp or root not in MEASURABLE_ROOTS:
            continue
        staged = site / fp
        if not staged.is_file():
            notes.append(f"ABSENT {root}: {fp} — not staged at {site}")
            continue
        size = staged.stat().st_size
        if size == 0:
            notes.append(f"EMPTY {root}: {fp} — zero bytes, refusing to record it")
            continue
        meta = rec.get("metadata") or {}
        origin = meta.get("origin_bytes", rec.get("file_size_bytes"))
        if size == origin and (
                root not in PRESTAGED_ROOTS or meta.get("sha256")):
            continue
        entry = {"id": rec["id"], "file_path": fp, "bytes": size}
        if origin is not None and origin != size:
            entry["origin_bytes"] = origin
        # The hash is the thing that makes a re-fetch REFUSE an origin
        # rather than merely notice a different length, so it is recorded
        # for every pre-staged record — including the ones whose byte
        # count already agrees but which carry no hash at all.
        if want_hashes and root in PRESTAGED_ROOTS:
            entry["sha256"] = sha256_of(staged)
        entries.append(entry)
    entries.sort(key=lambda e: e["file_path"])
    return entries, notes


def cmd_emit(profile_path: Path, site: Path, out: Path) -> int:
    profile = json.loads(profile_path.read_text(encoding="utf-8"))
    entries, notes = measure(profile, site)
    for n in notes:
        print(f"  {n}", file=sys.stderr)
    understated = sum(e["origin_bytes"] - e["bytes"]
                      for e in entries if "origin_bytes" in e)
    print(f"measured {len(entries)} record(s) whose shipped bytes were not "
          f"described; {understated:,} B of overstatement", file=sys.stderr)
    fmt = _formatting_of(out) if out.is_file() else (1, "\n", False)
    dump_like(out, entries, fmt)
    print(f"wrote {out}", file=sys.stderr)
    return 0


def cmd_verify(profile_path: Path, site: Path) -> int:
    """Fail when the profile disagrees with the bytes at `site`.

    ⛔ Only the measurable roots are judged. A source-backed root that
    disagrees is REPORTED and does not fail: the share lagging a rebuilt
    pool is a stale publish, not a wrong record, and conflating the two
    is how a correct profile gets "fixed" into a broken one.
    """
    profile = json.loads(profile_path.read_text(encoding="utf-8"))
    bad = stale = 0
    for rec in profile:
        fp = rec.get("file_path")
        root = rec.get("source_root", "local")
        if not fp:
            continue
        staged = site / fp
        if not staged.is_file():
            continue
        size = staged.stat().st_size
        want = rec.get("file_size_bytes")
        if size == want:
            continue
        if root in SOURCE_BACKED_ROOTS:
            stale += 1
            continue
        bad += 1
        print(f"  MISMATCH [{root}] {fp}: profile {want:,} B, staged "
              f"{size:,} B", file=sys.stderr)
    if stale:
        print(f"  {stale} source-backed record(s) differ — the share holds an "
              f"older build of a reproducible source, not a wrong record",
              file=sys.stderr)
    if bad:
        print(f"\n⛔ {bad} record(s) describe bytes this site does not ship.",
              file=sys.stderr)
        return 1
    print("  every measurable record matches the bytes staged for it",
          file=sys.stderr)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    e = sub.add_parser("emit", help="write a staged-measurements document")
    e.add_argument("--profile", required=True, type=Path)
    e.add_argument("--site", required=True, type=Path)
    e.add_argument("--out", required=True, type=Path)

    v = sub.add_parser("verify", help="check a profile against a site (read only)")
    v.add_argument("--profile", required=True, type=Path)
    v.add_argument("--site", required=True, type=Path)

    args = ap.parse_args()
    if not args.site.is_dir():
        print(f"error: --site not a directory: {args.site}\n"
              "  If this is the archive share, the mount may have dropped — "
              "that reads as 'No such file or directory'. Check `mountpoint` "
              "before assuming data is missing.", file=sys.stderr)
        return 2
    if args.cmd == "emit":
        return cmd_emit(args.profile, args.site, args.out)
    return cmd_verify(args.profile, args.site)


if __name__ == "__main__":
    raise SystemExit(main())
