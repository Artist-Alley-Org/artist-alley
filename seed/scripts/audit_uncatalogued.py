#!/usr/bin/env python3
"""
Classify every asset file on a site's archive that no `MANIFEST.json`
record names (#722).

"Uncatalogued" is not one thing, and treating it as one is what makes
this question keep coming back with a different answer. site_a has 461
files under `3d/` + `images/` that no record names, and they fall into
three kinds with three different correct responses:

    COMPANION  (201)  reachable from a catalogued .gltf / .obj
                      → CORRECT as-is. Registered against its parent
                        asset at seed time by Runner.applyAssetCompanions
                        (app/internal/seed/runner.go). Giving one its own
                        record would double-count the bytes and detach
                        the texture from the model that needs it.

    SUPERSEDED (260)  the `old` file of a #604 HQ replacement
                      → DEAD BYTES. Its record still exists; the record
                        was repointed at a kenney-hq render and the
                        original file was left on the share. Cataloguing
                        one would create a second record for content a
                        shipped upgrade deliberately removed.

    ORPHAN     (0)    none of the above
                      → REAL GAP. Bytes on the share that nothing knows
                        about. These are the ones that would need
                        catalogue records.

Only the third kind is a problem, and site_a currently has none.

WHY THE COMPANION HOP COUNT MATTERS
-----------------------------------
A companion chain is two hops, not one:

    model.obj ──mtllib──► model.mtl ──map_Kd──► texture.png

Stop after `mtllib` and every texture reads as an orphan. Rather than
hold a second opinion that can drift, this delegates to
`populate_archive.resolve_model_companions`, the Python twin of the Go
`format3d.ResolveCompanions` the seed runner actually registers with.

WHY SUPERSEDED FILES ARE NOT A CATALOGUING JOB
----------------------------------------------
`apply_upgrade.apply_replacements` repoints a record's `file_path` at the
HQ pool. It does not delete the file the record used to point at, and
`populate_archive.py` only removes unwanted files under `--prune`. So
every replacement leaves one stranded file behind, indistinguishable
from a never-catalogued asset unless you read the `old` column of
`kenney-hq-replacements.<site>.json`.

On site_a that is 260 files / 0.4 MB, and the set is exactly the
replacement doc's `old` column — asserted in test_dataset_upgrade.py, not
assumed. They include a 1x1 white pixel and 169 files under 64px on their
longest side. `prune` removes them, and refuses to remove anything a
record still names.

Usage
-----
    python3 audit_uncatalogued.py detect --site site_a \\
        --site-root /mnt/.../site_a

    # CI gate: exit 1 if any REAL gap exists
    python3 audit_uncatalogued.py detect --site site_a \\
        --site-root /mnt/.../site_a --fail-on-orphans

    # delete the superseded originals (dry-run by default)
    python3 audit_uncatalogued.py prune --site site_a \\
        --site-root /mnt/.../site_a --apply
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import populate_archive as pa  # noqa: E402

# Only these two trees hold bytes a record or a model could name. audio/,
# videos/, documents/ and fonts/ have no companion mechanism, so widening
# the scan would only add noise to the one number that matters.
SCAN_ROOTS = ("3d", "images")

UPGRADES = Path(__file__).resolve().parents[1] / "upgrades"


def companion_paths(site_root: Path, catalogued: set[str]) -> set[str]:
    """Site-relative paths reachable from a catalogued .gltf / .obj."""
    reachable: set[str] = set()
    for rel in sorted(catalogued):
        ext = rel.rsplit(".", 1)[-1].lower() if "." in rel else ""
        if ext not in ("gltf", "obj"):
            continue
        model = site_root / rel
        if not model.is_file():
            continue
        parent = Path(rel).parent
        for comp in pa.resolve_model_companions(model):
            reachable.add((parent / comp).as_posix())
    return reachable


def superseded_paths(upgrades: Path, site: str) -> set[str]:
    """Destination paths a #604 HQ swap left behind.

    The replacements doc is the only record that these files were ever
    catalogued — the profile entry has moved on and the file itself
    carries no marker.
    """
    doc = upgrades / f"kenney-hq-replacements.{site}.json"
    if not doc.is_file():
        return set()
    return {r["old"] for r in json.loads(doc.read_text(encoding="utf-8"))
            if r.get("old")}


def classify(site_root: Path, upgrades: Path, site: str
             ) -> tuple[list[str], list[str], list[str]]:
    """Returns (orphans, companions, superseded) as sorted relative paths.

    A file is only SUPERSEDED if no live record names it. A replacement
    that was later reverted would put the path back in the catalogue, and
    reporting it as dead on the strength of a stale doc is exactly the
    kind of confident wrong answer that gets bytes deleted.
    """
    manifest = json.loads((site_root / "MANIFEST.json").read_text(encoding="utf-8"))
    catalogued = {e["file_path"] for e in manifest if e.get("file_path")}

    on_disk: set[str] = set()
    for sub in SCAN_ROOTS:
        d = site_root / sub
        if not d.is_dir():
            continue
        for p in d.rglob("*"):
            if p.is_file():
                on_disk.add(p.relative_to(site_root).as_posix())

    uncatalogued = on_disk - catalogued
    companions = uncatalogued & companion_paths(site_root, catalogued)
    superseded = (uncatalogued - companions) & superseded_paths(upgrades, site)
    return (sorted(uncatalogued - companions - superseded),
            sorted(companions), sorted(superseded))


def _summarise(site_root: Path, orphans, companions, superseded) -> None:
    def mb(paths):
        return sum((site_root / p).stat().st_size for p in paths) / 2 ** 20
    print(f"companion  : {len(companions):5d}  ({mb(companions):.1f} MB)  "
          "reachable from a catalogued model — correct as-is",
          file=sys.stderr)
    print(f"superseded : {len(superseded):5d}  ({mb(superseded):.1f} MB)  "
          "#604 HQ replacement leftovers — dead bytes, see `prune`",
          file=sys.stderr)
    print(f"orphan     : {len(orphans):5d}  ({mb(orphans):.1f} MB)  "
          "nothing names them and no model reaches them",
          file=sys.stderr)


def cmd_detect(args: argparse.Namespace) -> int:
    orphans, companions, superseded = classify(
        args.site_root, args.upgrades, args.site)
    for rel in orphans:
        print(f"{(args.site_root / rel).stat().st_size}\t{rel}")
    _summarise(args.site_root, orphans, companions, superseded)
    if orphans and args.fail_on_orphans:
        print(f"\nFAIL: {len(orphans)} file(s) on the share are in nobody's "
              "catalogue and nothing can reach them.", file=sys.stderr)
        return 1
    return 0


def cmd_prune(args: argparse.Namespace) -> int:
    orphans, companions, superseded = classify(
        args.site_root, args.upgrades, args.site)
    _summarise(args.site_root, orphans, companions, superseded)
    if not superseded:
        print("\nnothing to prune", file=sys.stderr)
        return 0
    freed = sum((args.site_root / p).stat().st_size for p in superseded)
    if not args.apply:
        for rel in superseded[:10]:
            print(f"  would delete {rel}", file=sys.stderr)
        if len(superseded) > 10:
            print(f"  … and {len(superseded) - 10} more", file=sys.stderr)
        print(f"\n(dry run) {len(superseded)} files, {freed:,} B — "
              "pass --apply to delete", file=sys.stderr)
        return 0
    for rel in superseded:
        (args.site_root / rel).unlink()
    # A pack whose every record was replaced leaves an empty directory
    # tree behind — 22 of the 22 ui-pack-rpg-expansion files on site_a
    # are superseded. Deepest-first so parents empty out too.
    removed_dirs = 0
    for sub in SCAN_ROOTS:
        root = args.site_root / sub
        if not root.is_dir():
            continue
        for d in sorted(root.rglob("*"), key=lambda p: -len(p.parts)):
            if d.is_dir() and not any(d.iterdir()):
                d.rmdir()
                removed_dirs += 1
    print(f"\ndeleted {len(superseded)} superseded files, {freed:,} B freed, "
          f"{removed_dirs} empty directories removed", file=sys.stderr)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name, fn, helptext in (
            ("detect", cmd_detect, "classify everything no record names"),
            ("prune", cmd_prune, "delete the superseded originals")):
        p = sub.add_parser(name, help=helptext)
        p.add_argument("--site", required=True, choices=("site_a", "site_b"))
        p.add_argument("--site-root", required=True, type=Path)
        p.add_argument("--upgrades", type=Path, default=UPGRADES)
        p.set_defaults(fn=fn)
    sub.choices["detect"].add_argument(
        "--fail-on-orphans", action="store_true",
        help="exit 1 when a real gap exists (CI gate)")
    sub.choices["prune"].add_argument(
        "--apply", action="store_true",
        help="actually delete; without it this only reports")
    args = ap.parse_args()
    return args.fn(args)


if __name__ == "__main__":
    raise SystemExit(main())
