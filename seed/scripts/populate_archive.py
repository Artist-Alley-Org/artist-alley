#!/usr/bin/env python3
"""
Populate a per-site directory in the unraid archive with ONLY the assets
referenced by a studio's profile JSON, organized by asset type under
typed-folder layout. Filtered + path-rewritten metadata.csv ships alongside.

Layout produced under <dest>:

    <dest>/
    ├── images/<original-pack-path>/...
    ├── audio/<original-pack-path>/...
    ├── 3d/<original-pack-path>/...
    ├── video/<original-pack-path>/...
    ├── documents/<original-pack-path>/...
    ├── fonts/<original-pack-path>/...
    ├── comics/<original-pack-path>/...
    ├── <type>/internet/<filename>    (internet-fetched content)
    ├── metadata.csv                  filtered + file_path rewritten
    ├── groups.csv                    filtered to groups touching site rows
    └── MANIFEST.json                 copy of studio-X.assets.json

Each profile record carries:
  - source_root: which source root to copy from —
        'local'          the source dataset tree (--local-source)
        'internet'       the fetched cache (--internet-source)
        'hq'             the kenney-hq pool (--hq-source), built by
                         kenney_hq.py from the CC0 Kenney pack (#604)
        'torrent_import' / 'site'   pre-staged AT the destination; the
                         copier verifies presence instead of copying,
                         because there is no reachable source
  - source_path: relative path under that root
  - file_path:   destination path relative to <dest>

Usage
-----
    python3 populate_archive.py \\
        --local-source /mnt/d/Projects/unraid_management/artist-alley_dataset \\
        --internet-source seed/internet-fetched \\
        --profile seed/profiles/studio-a.assets.json \\
        --dest /mnt/blackbox_archives/datasets/artist_alley/site_a

Idempotent: files already present at the destination with matching size
are skipped. Use --prune to delete files at the destination that aren't
in the profile (useful when regenerating).
"""

from __future__ import annotations

import argparse
import csv
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

# Source roots whose bytes already sit at the destination. The copier
# verifies these instead of copying them — there is no reachable source
# to copy FROM. See the handling in the copy loop for what each means.
PRESTAGED_ROOTS = frozenset({"torrent_import", "site"})


def safe_mkdir(path: Path, max_retries: int = 3) -> None:
    """Robust mkdir for SMB / network mounts. Python's pathlib.mkdir
    sometimes raises FileExistsError with exist_ok=True when the mount
    returns weird errnos. Tries multiple strategies + a brief retry
    loop to let SMB caches settle. Raises RuntimeError if the directory
    still isn't usable after all attempts."""
    import time
    for attempt in range(max_retries):
        # Strategy 1: pathlib mkdir
        try:
            path.mkdir(parents=True, exist_ok=True)
        except (FileExistsError, OSError):
            pass
        # Strategy 2: shell mkdir -p (more forgiving on SMB)
        try:
            subprocess.run(["mkdir", "-p", str(path)], check=False,
                           timeout=10, capture_output=True)
        except Exception:
            pass
        # Probe: is the directory actually usable now?
        if path.is_dir():
            return
        # Wait briefly for SMB cache to sync, then retry
        time.sleep(0.5 + attempt * 0.5)
    # Final failure
    raise RuntimeError(f"could not create directory after {max_retries} attempts: {path}")


def _clean_companion_uri(uri: str) -> str | None:
    """Normalise a declared URI to a safe relative companion path, or
    None when it needs no companion (embedded data:, remote, absolute)
    or would escape the model directory. Mirrors the Go
    format3d.cleanCompanionURI used by the seed runner (#486)."""
    from urllib.parse import unquote

    uri = (uri or "").strip()
    if not uri:
        return None
    low = uri.lower()
    if low.startswith("data:") or "://" in uri:
        return None
    uri = unquote(uri).replace("\\", "/")
    if uri.startswith("/"):
        return None
    cleaned = os.path.normpath(uri).replace("\\", "/")
    if cleaned in (".", "..") or cleaned.startswith("../"):
        return None
    return cleaned


def resolve_model_companions(model_path: Path) -> list[str]:
    """Return the on-disk sibling files a multi-file model declares,
    relative to the model's directory (#486). glTF → buffers[].uri +
    images[].uri; OBJ → mtllib .mtl files and, recursively, the textures
    each .mtl references. GLB/FBX are self-contained → []. Only siblings
    that exist next to the model are returned. This is the Python twin of
    app/internal/preview/format3d.ResolveCompanions so the seed pipeline
    stages exactly what the Go runner will register + the loaders resolve."""
    ext = model_path.suffix.lower().lstrip(".")
    base = model_path.parent
    declared: list[str] = []
    seen: set[str] = set()

    def add(rel: str | None) -> None:
        if rel and rel not in seen:
            seen.add(rel)
            declared.append(rel)

    if ext == "gltf":
        try:
            doc = json.loads(model_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return []
        for b in doc.get("buffers", []):
            add(_clean_companion_uri(b.get("uri", "")))
        for im in doc.get("images", []):
            add(_clean_companion_uri(im.get("uri", "")))
    elif ext == "obj":
        try:
            text = model_path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return []
        map_kw = {
            "map_ka", "map_kd", "map_ks", "map_ke", "map_ns", "map_d",
            "map_bump", "bump", "disp", "decal", "refl", "norm",
            "map_pr", "map_pm", "map_ps",
        }
        for line in text.splitlines():
            parts = line.split()
            if len(parts) >= 2 and parts[0].lower() == "mtllib":
                for lib in parts[1:]:
                    rel = _clean_companion_uri(lib)
                    add(rel)
                    if not rel:
                        continue
                    mtl_path = base / rel
                    try:
                        mtl_text = mtl_path.read_text(encoding="utf-8", errors="replace")
                    except OSError:
                        continue
                    mtl_dir = os.path.dirname(rel)
                    for mline in mtl_text.splitlines():
                        mparts = mline.split()
                        if len(mparts) >= 2 and mparts[0].lower() in map_kw:
                            tex = _clean_companion_uri(mparts[-1])
                            if tex:
                                add(os.path.join(mtl_dir, tex).replace("\\", "/") if mtl_dir else tex)
    else:
        return []

    return [rel for rel in declared if (base / rel).is_file()]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--local-source", required=True, type=Path,
                        help="Local dataset root (where source_root='local' resolves)")
    parser.add_argument("--internet-source", required=True, type=Path,
                        help="Internet-fetched cache root (where source_root='internet' resolves)")
    parser.add_argument("--hq-source", type=Path, default=None,
                        help="kenney-hq pool root (where source_root='hq' resolves). "
                             "Build it with kenney_hq.py build. Required only if "
                             "the profile references HQ assets (#604).")
    parser.add_argument("--profile", required=True, type=Path,
                        help="Per-studio profile JSON (studio-a.assets.json etc.)")
    parser.add_argument("--dest", required=True, type=Path,
                        help="Destination site directory under the archive")
    parser.add_argument("--prune", action="store_true",
                        help="Delete files at <dest> not in the profile")
    parser.add_argument("--dry-run", action="store_true",
                        help="Report what would copy without writing")
    args = parser.parse_args()

    sources: dict[str, Path] = {
        "local": args.local_source,
        "internet": args.internet_source,
    }
    if args.hq_source is not None:
        sources["hq"] = args.hq_source
    if not sources["local"].is_dir():
        print(f"error: --local-source not a directory: {sources['local']}", file=sys.stderr)
        return 2

    src_csv = sources["local"] / "metadata.csv"
    src_groups = sources["local"] / "groups.csv"
    if not src_csv.is_file():
        print(f"error: metadata.csv not found at {src_csv}", file=sys.stderr)
        return 2

    print(f"loading profile {args.profile}", file=sys.stderr)
    profile = json.loads(args.profile.read_text(encoding="utf-8"))
    if not isinstance(profile, list):
        print(f"error: profile root must be a list of assets", file=sys.stderr)
        return 2

    # Fail loudly and early rather than per-file. A profile that
    # references the HQ pool without --hq-source would otherwise print a
    # few MISSING lines, skip 916 assets, and still exit 0 — leaving a
    # site whose manifest names files that were never copied. That is the
    # silent-success shape this whole issue is about (#604).
    hq_wanted = sum(1 for a in profile if a.get("source_root") == "hq")
    if hq_wanted and "hq" not in sources:
        print(f"error: profile references {hq_wanted} kenney-hq assets but "
              "--hq-source was not given.\n"
              "  Build the pool first:\n"
              "    python3 seed/scripts/kenney_hq.py build "
              "--pack <kenney-pack> --out <pool>", file=sys.stderr)
        return 2
    if hq_wanted and not sources["hq"].is_dir():
        print(f"error: --hq-source not a directory: {sources['hq']}\n"
              "  If it is on the archive share, the mount may have dropped — "
              "that reads as 'No such file or directory'.", file=sys.stderr)
        return 2

    # Build the (source_root, source_path) → file_path mapping
    path_map = {
        (a.get("source_root", "local"), a["source_path"]): a["file_path"]
        for a in profile if a.get("file_path") and a.get("source_path")
    }
    wanted_dest_paths = set(path_map.values())
    wanted_group_ids = {a.get("metadata", {}).get("group_id") for a in profile}
    wanted_group_ids.discard(None)
    wanted_group_ids.discard("")

    by_root: dict[str, int] = {"local": 0, "internet": 0}
    for (root, _), _ in path_map.items():
        by_root[root] = by_root.get(root, 0) + 1
    print(f"  {len(path_map):,} assets ({by_root.get('local', 0):,} local, "
          f"{by_root.get('internet', 0):,} internet)", file=sys.stderr)
    print(f"  {len(wanted_group_ids):,} group_ids", file=sys.stderr)

    if not args.dry_run:
        safe_mkdir(args.dest)
        # Pre-create every typed-folder root we'll write into. The SMB
        # mount sometimes returns stale "directory exists" info after a
        # half-completed run; doing this upfront with a fresh stat avoids
        # the per-file mkdir race that bit us earlier.
        type_roots = {dp.split("/", 1)[0] for dp in wanted_dest_paths if "/" in dp}
        for tr in sorted(type_roots):
            safe_mkdir(args.dest / tr)
        # Also pre-create the per-pack subdirs (one level deeper) for the
        # same reason — gets all the directory creation out of the way
        # before any file copies start.
        pack_dirs = {str(Path(dp).parent) for dp in wanted_dest_paths if "/" in dp}
        for pd in sorted(pack_dirs):
            safe_mkdir(args.dest / pd)

    # Filter + rewrite metadata.csv — keep rows whose original file_path
    # belongs to this site; rewrite the file_path column to the new layout
    # so the seeded instance can resolve it under <dest>.
    print(f"filtering + rewriting metadata.csv → {args.dest / 'metadata.csv'}",
          file=sys.stderr)
    # Rows are matched by the source path they ORIGINALLY had. An asset
    # whose bytes were swapped for a kenney-hq render (#604) keeps its
    # CSV row and gets the new path written into it — the record survives
    # the file swap, which is the same "swap the file, keep the record"
    # rule the upgrade itself follows. `replaced_source_path` is what
    # remembers the original; without it these rows match nothing and
    # drop out of the shipped CSV entirely.
    local_path_to_dest = {
        src_path: dest_path
        for (root, src_path), dest_path in path_map.items()
        if root == "local"
    }
    for a in profile:
        original = a.get("replaced_source_path")
        if original and a.get("file_path"):
            local_path_to_dest[original] = a["file_path"]
    kept_rows = 0
    with src_csv.open(newline="", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        fieldnames = reader.fieldnames
        out_rows: list[dict] = []
        for r in reader:
            if r["file_path"] in local_path_to_dest:
                r["file_path"] = local_path_to_dest[r["file_path"]]
                out_rows.append(r)
        kept_rows = len(out_rows)
        if not args.dry_run:
            with (args.dest / "metadata.csv").open("w", newline="", encoding="utf-8") as out:
                writer = csv.DictWriter(out, fieldnames=fieldnames)
                writer.writeheader()
                writer.writerows(out_rows)
    print(f"  kept {kept_rows:,} rows", file=sys.stderr)

    if src_groups.is_file():
        print(f"filtering groups.csv → {args.dest / 'groups.csv'}", file=sys.stderr)
        kept_groups = 0
        with src_groups.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f)
            fieldnames = reader.fieldnames
            rows = [r for r in reader if r["group_id"] in wanted_group_ids]
            kept_groups = len(rows)
            if not args.dry_run:
                with (args.dest / "groups.csv").open("w", newline="", encoding="utf-8") as out:
                    writer = csv.DictWriter(out, fieldnames=fieldnames)
                    writer.writeheader()
                    writer.writerows(rows)
        print(f"  kept {kept_groups:,} rows", file=sys.stderr)

    if not args.dry_run:
        shutil.copyfile(args.profile, args.dest / "MANIFEST.json")
        print(f"copied profile → {args.dest / 'MANIFEST.json'}", file=sys.stderr)

    print(f"copying {len(path_map):,} asset files", file=sys.stderr)
    copied = 0
    companions_copied = 0
    skipped = 0
    missing = 0
    bytes_copied = 0
    progress_every = max(1, len(path_map) // 20)
    items = sorted(path_map.items(), key=lambda x: x[1])  # sort by dest path
    preexisting = 0
    for i, ((root, source_path), dest_rel) in enumerate(items):
        # PRE-STAGED roots: the bytes already live at the destination and
        # cannot be re-copied from this side, so verify rather than copy.
        #
        #   torrent_import — pre-copied on the destination NAS
        #                    (Synology-to-Synology from /volume1/torrents/).
        #   site           — added directly to the site and not re-fetchable
        #                    from the recorded provenance (#604). The Pexels
        #                    videos record a page URL, not a direct media
        #                    URL, so there is nothing to GET.
        #
        # Verify-don't-copy is also what makes re-assembly SAFE for them:
        # the alternative — treating an unreachable source as "drop the
        # record" — is exactly how the added videos would disappear.
        if root in PRESTAGED_ROOTS:
            dest_file = args.dest / dest_rel
            if dest_file.is_file() and dest_file.stat().st_size > 0:
                preexisting += 1
            else:
                missing += 1
                if missing <= 5:
                    print(f"  MISSING [{root}]: {dest_rel} — not pre-staged?",
                          file=sys.stderr)
            continue

        src_file = sources[root] / source_path
        if not src_file.is_file():
            missing += 1
            if missing <= 5:
                print(f"  MISSING [{root}]: {source_path}", file=sys.stderr)
            continue
        dest_file = args.dest / dest_rel
        if dest_file.is_file() and dest_file.stat().st_size == src_file.stat().st_size:
            skipped += 1
            continue
        if args.dry_run:
            copied += 1
            bytes_copied += src_file.stat().st_size
            continue
        safe_mkdir(dest_file.parent)
        shutil.copyfile(src_file, dest_file)
        copied += 1
        bytes_copied += src_file.stat().st_size

        # Multi-file models (#486): copy the .gltf/.obj siblings the model
        # declares (buffer, textures, .mtl) next to the destination so the
        # asset resolves at render + view time. The Go seed runner then
        # auto-registers whatever landed next to the model as companions.
        for rel in resolve_model_companions(src_file):
            comp_src = src_file.parent / rel
            comp_dest = dest_file.parent / rel
            comp_rel = (Path(dest_rel).parent / rel).as_posix()
            wanted_dest_paths.add(comp_rel)  # survive --prune
            if comp_dest.is_file() and comp_dest.stat().st_size == comp_src.stat().st_size:
                continue
            if args.dry_run:
                companions_copied += 1
                bytes_copied += comp_src.stat().st_size
                continue
            safe_mkdir(comp_dest.parent)
            shutil.copyfile(comp_src, comp_dest)
            companions_copied += 1
            bytes_copied += comp_src.stat().st_size

        if (i + 1) % progress_every == 0:
            print(f"  ... {i+1:,}/{len(path_map):,} "
                  f"({bytes_copied / 2**20:.1f} MB)", file=sys.stderr)

    pruned = 0
    if args.prune and args.dest.is_dir():
        print(f"pruning files not in profile", file=sys.stderr)
        for f in args.dest.rglob("*"):
            if not f.is_file():
                continue
            rel = f.relative_to(args.dest).as_posix()
            if rel in ("metadata.csv", "groups.csv", "MANIFEST.json"):
                continue
            if rel not in wanted_dest_paths:
                if args.dry_run:
                    pruned += 1
                else:
                    f.unlink()
                    pruned += 1
        # Clean empty dirs
        if not args.dry_run:
            for d in sorted(args.dest.rglob("*"), key=lambda p: -len(str(p))):
                if d.is_dir() and not any(d.iterdir()):
                    d.rmdir()
        print(f"  pruned {pruned:,} stale files", file=sys.stderr)

    print(f"\n=== Summary ===", file=sys.stderr)
    print(f"  copied:      {copied:,} files ({bytes_copied / 2**30:.2f} GB)", file=sys.stderr)
    print(f"  companions:  {companions_copied:,} multi-file model siblings (#486)", file=sys.stderr)
    print(f"  skipped:     {skipped:,} (already present, same size)", file=sys.stderr)
    print(f"  preexisting: {preexisting:,} (torrent_import — bytes already in place)",
          file=sys.stderr)
    print(f"  missing:     {missing:,} (not found in source)", file=sys.stderr)
    if args.prune:
        print(f"  pruned:  {pruned:,} stale files removed", file=sys.stderr)
    if missing > 5:
        print(f"  (first 5 missing logged above)", file=sys.stderr)

    return 0 if missing == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
