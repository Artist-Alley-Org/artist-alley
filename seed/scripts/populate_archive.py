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
                         because there is no LOCAL source. When such a
                         record is missing and carries a
                         `metadata.media_url`, it is downloaded instead
                         of reported missing (#602) — see RE-FETCH below.
  - source_path: relative path under that root
  - file_path:   destination path relative to <dest>

RE-FETCH (#602)
---------------
A pre-staged record used to be a dead end on a machine without the
archive share: verify-don't-copy could only report MISSING. The Pexels
videos now carry `metadata.media_url` — the direct CDN URL of the exact
bytes, size-matched against `file_size_bytes` when it was recorded — so
a missing one is downloaded rather than lost, and a from-scratch rebuild
no longer needs the share for them.

Re-fetch is attempted ONLY for records that would otherwise be reported
missing, so a normal run over a populated share does no network I/O at
all. `--no-refetch` restores the old verify-only behaviour. A download
whose length disagrees with the manifest is discarded, not written: a
wrong file staged silently is worse than a missing one reported loudly.

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
import hashlib
import io
import json
import os
import shutil
import struct
import subprocess
import sys
import urllib.request
import zipfile
from pathlib import Path

# Source roots whose bytes already sit at the destination. The copier
# verifies these instead of copying them — there is no LOCAL source to
# copy FROM. See the handling in the copy loop for what each means.
PRESTAGED_ROOTS = frozenset({"torrent_import", "site"})

UA = ("artist-alley-seed-fetcher/2.0 "
      "(+https://github.com/Artist-Alley-Org/artist-alley)")

# Downloaded pack zips, keyed by URL, for the archive-member re-fetch
# below. One pack holds hundreds of records; without this a rebuild would
# download the same 15 MB zip once per file.
_ZIP_CACHE: dict[str, "zipfile.ZipFile"] = {}


def refetch_member(url: str, member: str, expect_sha256: str,
                   dest: Path) -> tuple[bool, str]:
    """Extract one file from a remote zip into `dest` (#572).

    The Kenney half of the library has no per-file URL: the bundle it
    comes from is a paid download, and the free per-pack zips serve the
    whole pack. So `media_url`'s contract — a URL serving exactly
    `file_size_bytes` — cannot apply, and the record instead names the
    zip, the member inside it, and that member's sha256. The hash does
    the job the byte count does for a direct URL: it is what makes the
    provenance evidence rather than a plausible-looking string, and it is
    checked BEFORE the bytes are moved into place, so a pack that changed
    upstream fails loudly instead of staging different art under an
    unchanged manifest entry.
    """
    z = _ZIP_CACHE.get(url)
    if z is None:
        req = urllib.request.Request(url, headers={"User-Agent": UA})
        try:
            with urllib.request.urlopen(req, timeout=600) as resp:
                blob = resp.read()
        except Exception as e:
            return False, f"zip download failed: {e}"
        try:
            z = zipfile.ZipFile(io.BytesIO(blob))
        except zipfile.BadZipFile as e:
            return False, f"not a zip: {e}"
        _ZIP_CACHE[url] = z
    try:
        data = z.read(member)
    except KeyError:
        return False, f"member not in zip: {member}"
    got = hashlib.sha256(data).hexdigest()
    if got != expect_sha256:
        return False, (f"sha256 mismatch for {member}: recorded "
                       f"{expect_sha256[:12]}…, served {got[:12]}… — the "
                       "pack changed upstream; re-run kenney_pack_sources.py "
                       "resolve --force and re-emit")
    safe_mkdir(dest.parent)
    tmp = dest.with_suffix(dest.suffix + ".part")
    tmp.write_bytes(data)
    tmp.replace(dest)
    return True, f"{len(data):,} B from {url.rsplit('/', 1)[-1]}"


def refetch(url: str, dest: Path, expect_size: int | None) -> tuple[bool, str]:
    """Download `url` to `dest`. Returns (ok, note).

    Written to a .part file and only moved into place once the length
    agrees with the manifest. A short or substituted download that landed
    at the real path would look pre-staged on the next run and never be
    noticed — the whole point of recording a byte count alongside the URL
    is to make that impossible.
    """
    tmp = dest.with_suffix(dest.suffix + ".part")
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            safe_mkdir(dest.parent)
            with tmp.open("wb") as f:
                shutil.copyfileobj(resp, f, length=1 << 20)
    except Exception as e:
        tmp.unlink(missing_ok=True)
        return False, f"download failed: {e}"
    got = tmp.stat().st_size
    if expect_size and got != expect_size:
        tmp.unlink(missing_ok=True)
        return False, (f"size mismatch: manifest {expect_size:,} B, "
                       f"served {got:,} B — refusing to stage it")
    tmp.replace(dest)
    return True, f"{got:,} B"


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


def _glb_json_chunk(model_path: Path) -> dict | None:
    """Return the glTF JSON document inside a GLB container, or None if
    the file isn't a readable GLB (#750).

    GLB layout (glTF 2.0 §4.4, little-endian): a 12-byte header — magic
    'glTF', version, total length — then length-prefixed chunks, the
    first of which the spec requires to be the JSON chunk. Only that
    chunk is read; the BIN chunk after it is the bulk of the file. Python
    twin of format3d.ReadGLBJSONChunk."""
    try:
        with model_path.open("rb") as f:
            head = f.read(12)
            if len(head) < 12:
                return None
            magic, _version, _total = struct.unpack("<III", head)
            if magic != 0x46546C67:  # 'glTF'
                return None
            chunk = f.read(8)
            if len(chunk) < 8:
                return None
            clen, ctype = struct.unpack("<II", chunk)
            if ctype != 0x4E4F534A:  # 'JSON'
                return None
            if clen > 64 * 1024 * 1024:
                return None
            raw = f.read(clen)
            if len(raw) < clen:
                return None
        doc = json.loads(raw.decode("utf-8"))
    except (OSError, struct.error, UnicodeDecodeError, json.JSONDecodeError):
        return None
    return doc if isinstance(doc, dict) else None


def resolve_model_companions(model_path: Path) -> list[str]:
    """Return the on-disk sibling files a multi-file model declares,
    relative to the model's directory (#486). glTF and GLB → buffers[].uri
    + images[].uri; OBJ → mtllib .mtl files and, recursively, the textures
    each .mtl references. Only siblings that exist next to the model are
    returned. This is the Python twin of
    app/internal/preview/format3d.ResolveCompanions so the seed pipeline
    stages exactly what the Go runner will register + the loaders resolve.

    GLB is NOT self-contained by default (#750). It wraps the same glTF
    JSON document in a binary container, so its buffer/image URIs can name
    external files exactly as a .gltf's can. Treating it as embedded here
    is what left 363 of the catalogue's 374 GLBs staged WITHOUT the
    textures they name — Kenney ships one Textures/ dir per format
    directory, and this function returning [] meant the GLB dirs' copies
    were never copied, so the models rendered grey. FBX has the same
    unanswered question and still returns [] — see the resolver's header.
    """
    ext = model_path.suffix.lower().lstrip(".")
    base = model_path.parent
    declared: list[str] = []
    seen: set[str] = set()

    def add(rel: str | None) -> None:
        if rel and rel not in seen:
            seen.add(rel)
            declared.append(rel)

    def add_gltf_doc(doc: dict) -> None:
        for b in doc.get("buffers", []) or []:
            add(_clean_companion_uri(b.get("uri", "")))
        for im in doc.get("images", []) or []:
            add(_clean_companion_uri(im.get("uri", "")))

    if ext == "gltf":
        try:
            doc = json.loads(model_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return []
        add_gltf_doc(doc)
    elif ext == "glb":
        doc = _glb_json_chunk(model_path)
        if doc is None:
            return []
        add_gltf_doc(doc)
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
    parser.add_argument("--pack-source", type=Path, default=None,
                        help="'Kenney Game Assets All-in-1' bundle root (where "
                             "source_root='pack' resolves, #572). Optional: "
                             "records that name a metadata.source_archive are "
                             "downloaded from the pack's free CC0 zip when the "
                             "bundle is not present, so a machine without the "
                             "archive share can still build the site.")
    parser.add_argument("--profile", required=True, type=Path,
                        help="Per-studio profile JSON (studio-a.assets.json etc.)")
    parser.add_argument("--posts", type=Path, default=None,
                        help="Per-studio posts JSON (studio-a.posts.json), "
                             "staged as <dest>/posts.json — `aa seed` reads it "
                             "next to MANIFEST.json. Optional only because "
                             "earlier runs copied it by hand, which is how "
                             "site_a came to serve 584 posts against a profile "
                             "holding 859 (#572). Pass it.")
    parser.add_argument("--dest", required=True, type=Path,
                        help="Destination site directory under the archive")
    parser.add_argument("--prune", action="store_true",
                        help="Delete files at <dest> not in the profile")
    parser.add_argument("--dry-run", action="store_true",
                        help="Report what would copy without writing")
    parser.add_argument("--no-refetch", action="store_true",
                        help="Do not download pre-staged records that are "
                             "missing at <dest>, even when they carry a "
                             "metadata.media_url (#602). Restores the old "
                             "verify-only behaviour.")
    args = parser.parse_args()

    sources: dict[str, Path] = {
        "local": args.local_source,
        "internet": args.internet_source,
    }
    if args.hq_source is not None:
        sources["hq"] = args.hq_source
    if args.pack_source is not None:
        sources["pack"] = args.pack_source
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
    # The pre-staged branch needs the RECORD, not just the path — the
    # media_url and the byte count it is checked against both live there.
    by_dest = {a["file_path"]: a for a in profile if a.get("file_path")}
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
        if args.posts is not None:
            shutil.copyfile(args.posts, args.dest / "posts.json")
            print(f"copied posts   → {args.dest / 'posts.json'}", file=sys.stderr)
        elif (args.dest / "posts.json").is_file():
            # Loud, because the failure is silent: the seeder happily
            # loads a stale posts.json against a fresh MANIFEST, and the
            # only symptom is a browse wall with fewer posts than the
            # dataset says it has.
            print("warning: --posts not given; <dest>/posts.json is left as "
                  "it was and may not match the profile just written",
                  file=sys.stderr)

    print(f"copying {len(path_map):,} asset files", file=sys.stderr)
    copied = 0
    companions_copied = 0
    skipped = 0
    missing = 0
    bytes_copied = 0
    refetched = 0
    progress_every = max(1, len(path_map) // 20)
    items = sorted(path_map.items(), key=lambda x: x[1])  # sort by dest path
    preexisting = 0
    for i, ((root, source_path), dest_rel) in enumerate(items):
        # PRE-STAGED roots: the bytes already live at the destination and
        # have no LOCAL source to copy from, so verify rather than copy.
        #
        #   torrent_import — pre-copied on the destination NAS
        #                    (Synology-to-Synology from /volume1/torrents/).
        #   site           — added directly to the site (#604).
        #
        # Verify-don't-copy is also what makes re-assembly SAFE for them:
        # the alternative — treating an unreachable source as "drop the
        # record" — is exactly how the added videos would disappear.
        #
        # When one IS absent, `metadata.media_url` (#602) turns what used
        # to be a terminal MISSING into a download. That field is the
        # direct CDN URL, recorded only after a HEAD confirmed it serves
        # exactly `file_size_bytes`; the Pexels page URL in `fetched_from`
        # is an HTML document and was never something you could GET bytes
        # from, which is what left this branch a dead end before.
        if root in PRESTAGED_ROOTS:
            dest_file = args.dest / dest_rel
            if dest_file.is_file() and dest_file.stat().st_size > 0:
                preexisting += 1
                continue
            rec = by_dest.get(dest_rel) or {}
            media_url = (rec.get("metadata") or {}).get("media_url")
            if media_url and not args.no_refetch and not args.dry_run:
                ok, note = refetch(media_url, dest_file,
                                   rec.get("file_size_bytes"))
                if ok:
                    refetched += 1
                    bytes_copied += dest_file.stat().st_size
                    print(f"  REFETCHED [{root}]: {dest_rel} ({note})",
                          file=sys.stderr)
                    continue
                print(f"  REFETCH FAILED [{root}]: {dest_rel} — {note}\n"
                      f"    {media_url}", file=sys.stderr)
            missing += 1
            if missing <= 5:
                hint = ("" if media_url else
                        " — no metadata.media_url either, so there is nothing "
                        "to re-fetch from (see resolve_media_urls.py, #602)")
                print(f"  MISSING [{root}]: {dest_rel} — not pre-staged?{hint}",
                      file=sys.stderr)
            continue

        src_root = sources.get(root)
        src_file = (src_root / source_path) if src_root else None
        if src_file is None or not src_file.is_file():
            # #572 — bundle-sourced records carry the pack's free CC0 zip
            # plus the member path and its sha256, so an absent bundle is
            # a download rather than a hole. Same shape as #602's
            # media_url branch: only for records that would otherwise
            # fail, so a run over a mounted bundle does no network I/O.
            rec = by_dest.get(dest_rel) or {}
            sa = (rec.get("metadata") or {}).get("source_archive") or {}
            dest_file = args.dest / dest_rel
            if (root == "pack" and sa.get("url") and not args.no_refetch
                    and not args.dry_run):
                if dest_file.is_file() and dest_file.stat().st_size > 0:
                    skipped += 1
                    continue
                ok, note = refetch_member(sa["url"], sa["member"],
                                          sa.get("sha256", ""), dest_file)
                if ok:
                    refetched += 1
                    bytes_copied += dest_file.stat().st_size
                    print(f"  REFETCHED [{root}]: {dest_rel} ({note})",
                          file=sys.stderr)
                    continue
                print(f"  REFETCH FAILED [{root}]: {dest_rel} — {note}",
                      file=sys.stderr)
            # An absent SOURCE is not an absent ASSET. The internet cache
            # is gitignored and routinely not present on a machine that
            # already has a populated site; reporting 58 fully-staged
            # videos as MISSING and exiting 1 is the same
            # unavailable-is-not-absent confusion that makes a dropped
            # mount look like data loss. Gated on the manifest's own byte
            # count, so a genuinely short or wrong file still fails.
            want = (rec or {}).get("file_size_bytes")
            if (dest_file.is_file() and want
                    and dest_file.stat().st_size == want):
                preexisting += 1
                continue
            missing += 1
            if missing <= 5:
                hint = ("" if root != "pack" else
                        " — pass --pack-source <bundle>, or let the "
                        "metadata.source_archive re-fetch handle it")
                print(f"  MISSING [{root}]: {source_path}{hint}",
                      file=sys.stderr)
            continue
        dest_file = args.dest / dest_rel
        already = (dest_file.is_file()
                   and dest_file.stat().st_size == src_file.stat().st_size)
        if already:
            skipped += 1
        elif args.dry_run:
            copied += 1
            bytes_copied += src_file.stat().st_size
        else:
            safe_mkdir(dest_file.parent)
            shutil.copyfile(src_file, dest_file)
            copied += 1
            bytes_copied += src_file.stat().st_size

        # Companions run even when the MODEL was skipped (#572). They used
        # to sit inside the copy branch, so a model already present at the
        # destination short-circuited past its own siblings — which is
        # exactly how Sponza came to sit in site_a as a lone .gltf naming
        # a .bin and 69 textures that were never staged, and why it was
        # the only 3D asset in the instance stuck at `failed`. The model
        # matching by size proves nothing about the 70 files beside it.
        if args.dry_run and already:
            continue

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
    print(f"  preexisting: {preexisting:,} (pre-staged — bytes already in place)",
          file=sys.stderr)
    print(f"  refetched:   {refetched:,} (downloaded from metadata.media_url #602 / source_archive #572)",
          file=sys.stderr)
    print(f"  missing:     {missing:,} (not found in source)", file=sys.stderr)
    if args.prune:
        print(f"  pruned:  {pruned:,} stale files removed", file=sys.stderr)
    if missing > 5:
        print(f"  (first 5 missing logged above)", file=sys.stderr)

    return 0 if missing == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
