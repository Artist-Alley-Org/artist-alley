#!/usr/bin/env python3
"""
Build the "kenney-hq" high-quality image pool from the Kenney All-in-1
source pack, and (optionally) choose which existing dataset assets it
should replace.

Background (#604)
-----------------
The seeded image library was dominated by tiny sprite-sheet tiles — 183
site_a images under 1 KB, 71% of a 150-asset sample under 100px on the
long edge. A wall of 32px tiles makes every card treatment look broken
and tells you nothing about whether the viewer works. This tool rebuilds
that library from the CC0 Kenney pack, which ships both large finished
PNGs (skyboxes, tilesheets, texture plates) and ~5,200 per-item VECTORS
that can be rendered at whatever size we want.

Two hard-won rules are encoded here rather than left to the next person.
Both were found the expensive way; see the module tests.

RULE 1 — NEVER name by basename. Use the path hash.
    The pack ships identical filenames in sibling variant directories:
    four separate packs contain `Tilesheet/tilesheet_complete_2X.png`,
    and UI packs ship the same widget in PNG/Default/ AND PNG/Double/.
    Slugging output by basename silently OVERWRITES those collisions —
    48 assets on the first attempt, 65 on the second. The damage is
    invisible: the manifest still lists N entries, every file exists,
    and the seed "succeeds" while serving the wrong bytes for the
    overwritten ids. Output names therefore carry an 8-char SHA-1 of
    the source path (`name_for`), which is collision-free across all
    88,346 files in the pack.

RULE 2 — GATE ON DIMENSIONS, NEVER ON BYTES.
    Byte size stops being a quality proxy the moment assets are vector-
    rendered: 415 of the upgraded assets are under 10 KB *and* at least
    512px, because flat-colour vector art compresses hard. A byte
    threshold would reject exactly the assets this tool exists to
    produce. Selection reads real pixel dimensions (`png_dimensions`)
    and never looks at file size for a quality decision.

A third rule is about taste rather than correctness:

RULE 3 — WEIGHT THE SAMPLE, EXPLICITLY.
    Pack sizes are wildly uneven and do not track visual interest.
    `Icons/Input Prompts` alone holds 1,504 near-identical keyboard and
    gamepad button glyphs — a third of every vector in the pack. Sampled
    evenly it floods the library with UI chrome and the browse wall
    looks like a settings screen. PACK_WEIGHTS damps it to 0.3x and
    boosts the packs that actually read as artwork at tile size. The
    weights are data, not cleverness: edit the table, re-run, look.

Determinism
-----------
Same pack + same weights + same --limit produces the same pool, byte for
byte. The walk is sorted, the sampler is seeded off the asset path (not
a global RNG), and output names are pure functions of the source path.
That is what lets re-assembly reproduce the upgraded dataset instead of
regenerating the tiny originals.

Rasterisation
-------------
SVG -> PNG goes through `rasterize_svg.mjs` (sharp / libvips). sharp is
a plain `npm install sharp` with no sudo and no system libvips needed —
it ships prebuilt binaries. The Kenney launcher AppImage is an Electron
wrapper around the same sharp + ffmpeg and offers no CLI, so there is
nothing to drive but the libraries themselves.

Usage
-----
    # build the pool (dry run first — it prints the selection breakdown)
    python3 kenney_hq.py build \\
        --pack "/mnt/.../Kenney Game Assets All-in-1 3.6.0" \\
        --out  /tmp/kenney-hq --limit 700 --dry-run

    # verify an existing pool matches what this tool would produce
    python3 kenney_hq.py verify \\
        --pack "/mnt/.../Kenney Game Assets All-in-1 3.6.0" \\
        --pool /mnt/.../site_b/images/kenney-hq

    # re-measure the replacement docs' newSize against a built pool (#1294)
    python3 kenney_hq.py sizes --pool /tmp/kenney-hq \\
        --replacements seed/upgrades/kenney-hq-replacements.site_a.json \\
        --replacements seed/upgrades/kenney-hq-replacements.site_b.json
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import struct
import subprocess
import sys
from pathlib import Path

# --------------------------------------------------------------------------
# Naming — RULE 1
# --------------------------------------------------------------------------

# Rendered vectors carry a trailing -<px>; copied-as-is bitmaps do not.
# Both carry the 8-char path hash immediately before it.
POOL_NAME_RE = re.compile(
    r"^(?P<slug>.+)-(?P<hash>[0-9a-f]{8})(?:-(?P<size>\d+))?\.(?P<ext>png|jpg|jpeg)$"
)

HASH_LEN = 8


def path_hash(rel_path: str) -> str:
    """8-char SHA-1 of the pack-relative POSIX path.

    The *path* is hashed, not the bytes: two variant directories can
    hold byte-identical art (Default vs Double is often a 2x re-export
    of the same source), and we need them to stay distinguishable so a
    manifest entry keeps pointing at the file it was built from.
    """
    return hashlib.sha1(rel_path.encode("utf-8")).hexdigest()[:HASH_LEN]


def slugify(text: str) -> str:
    return re.sub(r"-+", "-", re.sub(r"[^a-z0-9]+", "-", text.lower())).strip("-")


def name_for(rel_path: str, render_px: int | None = None, ext: str = "png") -> str:
    """Output filename for a pack-relative source path.

    `<category>-<pack>-<item>-<hash>[-<px>].<ext>`

    Category and pack come from the first two path segments and the item
    from the basename, so the name stays readable in a file listing
    while the hash guarantees uniqueness. Intermediate directories
    (Tilesheet/, PNG/Double/, Vector/) are deliberately NOT in the slug:
    they are what collides, and the hash already separates them.
    """
    parts = rel_path.rsplit(".", 1)[0].split("/")
    category = parts[0] if parts else ""
    pack = parts[1] if len(parts) > 1 else ""
    item = parts[-1]
    slug = slugify("-".join(p for p in (category, pack, item) if p))
    suffix = f"-{render_px}" if render_px else ""
    return f"{slug}-{path_hash(rel_path)}{suffix}.{ext}"


# --------------------------------------------------------------------------
# Dimensions — RULE 2
# --------------------------------------------------------------------------

def png_dimensions(path: Path) -> tuple[int, int] | None:
    """(width, height) from a PNG's IHDR, or None if not a readable PNG.

    Deliberately header-only: the pack is 1.2 GB and a selection pass
    that decoded every image would take minutes for two integers.
    """
    try:
        with path.open("rb") as f:
            head = f.read(26)
        if head[:8] != b"\x89PNG\r\n\x1a\n":
            return None
        return struct.unpack(">II", head[16:24])
    except (OSError, struct.error):
        return None


# Minimum long edge for a bitmap to be worth copying as-is. Below this
# it is a sprite tile, which is what we are replacing.
#
# This is a FLOOR, not the selection rule. 707 files in the pack clear
# 1024px, and copying all of them would fill the pool with prototype
# textures and character turnaround sheets before a single vector got
# rendered. Which large bitmaps are actually worth having is a curatorial
# call (BITMAP_PACKS below), not something a threshold can decide.
LARGE_PNG_MIN_EDGE = 1024

# Packs whose large bitmaps earn their place: finished, self-contained
# artwork that reads at tile size. Skyboxes and planets are complete
# images; the tilesheets and kit previews are dense, colourful plates
# that look like something. Everything else in the pack is either a
# sprite atlas (which is what we are replacing) or a flat prototype
# texture (grey grids — technically 2048px, visually nothing).
BITMAP_PACKS: frozenset[str] = frozenset({
    "2D assets/Planets",
    "2D assets/Cartography Pack",
    "2D assets/Skyboxes",
    "2D assets/Toon Characters",
    "2D assets/Isometric Blocks",
    "2D assets/Letter Tiles",
    "3D assets/City Kit - Industrial",
    "3D assets/Modular Cave Kit",
    "3D assets/Modular Dungeon Kit",
    "3D assets/Modular Space Kit",
})

# Render size for vectors. 512 is the smallest size that still looks
# sharp on the largest tile the grid produces (measured ~333px at the
# default rung, ~1360px in masonry at 4K — masonry upscales, accepted).
DEFAULT_RENDER_PX = 512


# --------------------------------------------------------------------------
# Sampling weights — RULE 3
# --------------------------------------------------------------------------

# Multiplier on a pack's share of the sample. 1.0 is neutral.
# Keys match "<Category>/<Pack>" exactly.
PACK_WEIGHTS: dict[str, float] = {
    # Damped — enormous packs of near-identical UI chrome. Input Prompts
    # is 1,504 button glyphs, ~29% of every vector in the pack; left
    # neutral it drowns the library in keycaps.
    "Icons/Input Prompts": 0.3,
    "UI assets/Mobile Controls": 0.3,
    "UI assets/Cursor Pack": 0.3,
    "Icons/Board Game Info": 0.5,
    "Icons/Board Game Icons": 0.5,
    # Boosted — packs that read as actual artwork at tile size, which is
    # what makes a browse wall worth looking at.
    "2D assets/Cartography Pack": 3.0,
    "2D assets/Brick Pack": 3.0,
    "2D assets/Fish Pack": 3.0,
    "2D assets/Flag Pack": 3.0,
    "2D assets/Pattern Pack": 3.0,
    "2D assets/New Platformer Pack": 3.0,
    "3D assets/Skybox Pack": 3.0,
}

DEFAULT_WEIGHT = 1.0


def pack_weight(category: str, pack: str, weights: dict[str, float]) -> float:
    return weights.get(f"{category}/{pack}", DEFAULT_WEIGHT)


# --------------------------------------------------------------------------
# Discovery
# --------------------------------------------------------------------------

class Candidate:
    __slots__ = ("rel", "category", "pack", "kind", "width", "height")

    def __init__(self, rel: str, category: str, pack: str, kind: str,
                 width: int = 0, height: int = 0):
        self.rel = rel
        self.category = category
        self.pack = pack
        self.kind = kind          # "vector" | "bitmap"
        self.width = width
        self.height = height

    @property
    def out_name(self) -> str:
        return name_for(self.rel,
                        DEFAULT_RENDER_PX if self.kind == "vector" else None)


def discover(pack_root: Path) -> list[Candidate]:
    """Every eligible source file in the pack, in a stable order.

    Sorted at every level so two runs on the same pack enumerate
    identically — the sampler's determinism depends on it.
    """
    out: list[Candidate] = []
    for category in sorted(_subdirs(pack_root)):
        cat_dir = pack_root / category
        for pack in sorted(_subdirs(cat_dir)):
            pack_dir = cat_dir / pack
            for dirpath, dirnames, filenames in os.walk(pack_dir):
                dirnames.sort()
                for fn in sorted(filenames):
                    src = Path(dirpath) / fn
                    rel = src.relative_to(pack_root).as_posix()
                    ext = fn.rsplit(".", 1)[-1].lower() if "." in fn else ""
                    if ext == "svg":
                        out.append(Candidate(rel, category, pack, "vector"))
                    elif ext == "png" and f"{category}/{pack}" in BITMAP_PACKS:
                        dims = png_dimensions(src)
                        if dims and max(dims) >= LARGE_PNG_MIN_EDGE:
                            out.append(Candidate(rel, category, pack, "bitmap",
                                                 dims[0], dims[1]))
    return out


def _subdirs(root: Path) -> list[str]:
    if not root.is_dir():
        return []
    return [d.name for d in root.iterdir() if d.is_dir()]


def select(candidates: list[Candidate], limit: int,
           weights: dict[str, float] | None = None) -> list[Candidate]:
    """Pick `limit` candidates, weighted per pack (RULE 3).

    Every bitmap is kept unconditionally — they are the large finished
    art and there are few of them. Vectors are sampled: each gets a
    score derived from a hash of its own path (so the choice is stable
    and independent of iteration order) divided by its pack weight, and
    the lowest scores win. Dividing by the weight means a 3x pack's
    items score a third as high and therefore survive the cut more
    often, without any per-pack quota arithmetic.
    """
    weights = PACK_WEIGHTS if weights is None else weights
    bitmaps = [c for c in candidates if c.kind == "bitmap"]
    vectors = [c for c in candidates if c.kind == "vector"]

    def score(c: Candidate) -> tuple[float, str]:
        # Stable per-item pseudo-random in [0,1) — seeded by the path so
        # it does not depend on how many items came before it.
        raw = int(hashlib.sha1(c.rel.encode("utf-8")).hexdigest()[:12], 16)
        unit = raw / float(1 << 48)
        w = pack_weight(c.category, c.pack, weights)
        return (unit / w if w > 0 else float("inf"), c.rel)

    room = max(0, limit - len(bitmaps))
    chosen = sorted(vectors, key=score)[:room]
    return sorted(bitmaps + chosen, key=lambda c: c.rel)


# --------------------------------------------------------------------------
# Build
# --------------------------------------------------------------------------

def rasterize(jobs: list[dict], node_script: Path) -> None:
    """Render SVG jobs to PNG via sharp. Jobs are [{src,dst,px}, ...]."""
    if not jobs:
        return
    proc = subprocess.run(
        ["node", str(node_script)],
        input=json.dumps(jobs), text=True, capture_output=True,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"rasterize_svg.mjs failed ({proc.returncode}).\n"
            f"stderr:\n{proc.stderr.strip()}\n\n"
            "If this is a missing module, install the renderer:\n"
            "    cd seed/scripts && npm install sharp"
        )
    if proc.stderr.strip():
        print(proc.stderr.strip(), file=sys.stderr)


def load_pool_manifest(path: Path) -> list[dict]:
    doc = json.loads(path.read_text(encoding="utf-8"))
    return doc["entries"] if isinstance(doc, dict) else doc


def build(pack_root: Path, out_dir: Path, manifest: Path, dry_run: bool,
          rerender: list[str] | None = None) -> int:
    """Rebuild the pool EXACTLY as described by the committed manifest.

    Selection is data, not a re-derived heuristic, and that is the whole
    point (#604). Which 656 files belong in the library was a curatorial
    decision made once, by eye; a sampler re-run would produce a
    different-but-plausible set every time the pack version or the
    weights moved, and the dataset would drift out from under the
    manifests that reference it. `select` proposes; this reproduces.
    """
    entries = load_pool_manifest(manifest)

    # RULE 1, checked before a single byte is written. If the naming
    # scheme ever regresses toward basenames this trips here rather than
    # silently overwriting assets that then seed with the wrong bytes.
    names = [e["name"] for e in entries]
    if len(names) != len(set(names)):
        dupes = sorted({n for n in names if names.count(n) > 1})
        raise SystemExit(
            f"FATAL: {len(names) - len(set(names))} output-name collisions in "
            f"{manifest} (e.g. {dupes[:3]}). See RULE 1.")

    # Every name must still be the pure function of its source path. This
    # catches a hand-edited manifest, and it catches the naming rule
    # being changed without regenerating the pool.
    drift = [e for e in entries
             if name_for(e["source"], e.get("render_px")) != e["name"]]
    if drift:
        raise SystemExit(
            f"FATAL: {len(drift)} manifest entries whose name is not "
            f"name_for(source) — the naming rule and the pool disagree. "
            f"e.g. {drift[0]['source']} -> {drift[0]['name']}")

    missing_src = [e for e in entries if not (pack_root / e["source"]).is_file()]
    if missing_src:
        raise SystemExit(
            f"FATAL: {len(missing_src)} source files named by the manifest are "
            f"not in the pack at {pack_root} (e.g. {missing_src[0]['source']}). "
            "Wrong pack version, or a dropped mount.")

    n_vec = sum(1 for e in entries if e["kind"] == "vector")
    print(f"pack       : {pack_root}", file=sys.stderr)
    print(f"manifest   : {manifest} ({len(entries):,} entries)", file=sys.stderr)
    print(f"plan       : {n_vec:,} to render, {len(entries) - n_vec:,} to copy",
          file=sys.stderr)
    if dry_run:
        print("(dry run — nothing written)", file=sys.stderr)
        return 0

    out_dir.mkdir(parents=True, exist_ok=True)
    # A build normally skips anything already on disk, which is what makes
    # it cheap to re-run. The cost is that a RASTERISER fix can never reach
    # a pool that already exists — #672's and #685's fixes both had to be
    # applied by hand because of it. `--rerender` re-renders the named
    # entries (output name, or a substring of the source path) whether or
    # not the PNG is there, so a repair is a command rather than a
    # procedure someone has to remember.
    def wanted(e: dict) -> bool:
        if not rerender:
            return False
        return any(pat == e["name"] or pat in e["source"] for pat in rerender)

    jobs, copied, forced = [], 0, 0
    for e in entries:
        src, dst = pack_root / e["source"], out_dir / e["name"]
        if e["kind"] == "vector":
            if wanted(e):
                forced += 1
            if not dst.exists() or wanted(e):
                jobs.append({"src": str(src), "dst": str(dst),
                             "px": e.get("render_px") or DEFAULT_RENDER_PX})
        else:
            if not dst.exists() or dst.stat().st_size != src.stat().st_size:
                shutil.copyfile(src, dst)
            copied += 1
    if rerender and not forced:
        raise SystemExit(
            f"FATAL: --rerender {rerender} matched no manifest entry. A repair "
            "that silently repairs nothing is how a known-bad file ships twice.")
    print(f"copied {copied:,} bitmaps; rendering {len(jobs):,} vectors"
          + (f" ({forced:,} forced by --rerender)" if forced else "") + "…",
          file=sys.stderr)
    rasterize(jobs, Path(__file__).with_name("rasterize_svg.mjs"))
    return verify_pool(out_dir, expected_names={e["name"] for e in entries})


def cmd_select(pack_root: Path, limit: int, out: Path | None) -> int:
    """Propose a weighted selection and print it as a pool manifest.

    This is how the pool GROWS. It never overwrites the committed
    manifest implicitly — you run it, read the breakdown, and merge what
    you want. The weights (RULE 3) are what make the proposal usable
    rather than 1,504 keycaps.
    """
    candidates = discover(pack_root)
    chosen = select(candidates, limit)
    n_vec = sum(1 for c in chosen if c.kind == "vector")
    print(f"candidates : {len(candidates):,} "
          f"({sum(1 for c in candidates if c.kind == 'vector'):,} vector, "
          f"{sum(1 for c in candidates if c.kind == 'bitmap'):,} curated bitmap)",
          file=sys.stderr)
    print(f"proposed   : {len(chosen):,} ({n_vec:,} vector, "
          f"{len(chosen) - n_vec:,} bitmap)", file=sys.stderr)
    by_pack: dict[str, int] = {}
    for c in chosen:
        key = f"{c.category}/{c.pack}"
        by_pack[key] = by_pack.get(key, 0) + 1
    print("top packs  :", file=sys.stderr)
    for name, n in sorted(by_pack.items(), key=lambda kv: -kv[1])[:12]:
        cat, pk = name.split("/", 1)
        print(f"   {n:5d}  {name}  (weight {pack_weight(cat, pk, PACK_WEIGHTS)}x)",
              file=sys.stderr)
    doc = {
        "pack": pack_root.name,
        "render_px": DEFAULT_RENDER_PX,
        "entries": [
            {"source": c.rel, "name": c.out_name, "kind": c.kind,
             "render_px": DEFAULT_RENDER_PX if c.kind == "vector" else None}
            for c in chosen
        ],
    }
    text = json.dumps(doc, indent=1, ensure_ascii=False) + "\n"
    if out:
        out.write_text(text, encoding="utf-8")
        print(f"wrote {out}", file=sys.stderr)
    else:
        sys.stdout.write(text)
    return 0


def verify_pool(pool_dir: Path, expected_names: set[str] | None = None) -> int:
    """Assert the pool is internally consistent: entries == unique == on-disk.

    This is the check that would have caught the silent-overwrite bug.
    A pool that lost 65 assets to name collisions still LOOKS fine —
    every file it references exists. What it fails is the count.
    """
    on_disk = sorted(p.name for p in pool_dir.iterdir() if p.is_file())
    problems: list[str] = []

    if len(on_disk) != len(set(on_disk)):
        problems.append("duplicate filenames on disk (should be impossible)")

    unparsed = [n for n in on_disk if not POOL_NAME_RE.match(n)]
    if unparsed:
        problems.append(f"{len(unparsed)} files do not match the pool naming "
                        f"scheme (e.g. {unparsed[:3]})")

    hashes = [m.group("hash") for n in on_disk
              if (m := POOL_NAME_RE.match(n))]
    if len(hashes) != len(set(hashes)):
        problems.append(f"{len(hashes) - len(set(hashes))} path-hash collisions "
                        "— RULE 1 has regressed")

    # RULE 2: quality is dimensional. Report on pixels, never on bytes.
    small = []
    for n in on_disk:
        dims = png_dimensions(pool_dir / n)
        if dims and max(dims) < 256:
            small.append(n)
    if small:
        problems.append(f"{len(small)} pool images have a long edge < 256px "
                        f"(e.g. {small[:3]})")

    if expected_names is not None:
        missing = sorted(expected_names - set(on_disk))
        if missing:
            problems.append(f"{len(missing)} selected assets missing from the "
                            f"pool (e.g. {missing[:3]})")

    print(f"\npool       : {pool_dir}", file=sys.stderr)
    print(f"  files    : {len(on_disk):,}", file=sys.stderr)
    print(f"  unique   : {len(set(on_disk)):,}", file=sys.stderr)
    print(f"  hashes   : {len(set(hashes)):,} distinct", file=sys.stderr)
    if problems:
        for p in problems:
            print(f"  FAIL     : {p}", file=sys.stderr)
        return 1
    print("  OK       : entries == unique == on-disk, no collisions, "
          "dimensions pass", file=sys.stderr)
    return 0


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def rows_needing_a_hash(profiles: list[Path]) -> set[str]:
    """Record ids that carry a `metadata.sha256` (#1302).

    A replacement repoints a record at a pool file but used to leave
    `metadata.sha256` describing the file it USED to be — so two records
    per site published the hash of a screenshot they no longer contain.
    The fix is not to give all 916 replacement rows a hash to serve two
    of them, and it is not to drop the key either: the published
    manifest HAS it, so dropping it reads as MISSING_KEY and refuses the
    next publish (`manifest_guard.py:34-43`).

    So the doc carries `newSha256` exactly where the record it repoints
    already carries a hash — two rows per site today, and automatically
    the right rows if a record ever gains or loses one.
    """
    ids = set()
    for p in profiles:
        for rec in json.loads(p.read_text(encoding="utf-8")):
            if (rec.get("metadata") or {}).get("sha256"):
                ids.add(rec["id"])
    return ids


def _measure_balance(pool_dir: Path, doc: Path, write: bool) -> int:
    """Re-measure a balance document's hq byte counts against the pool.

    ⛔ WHY THESE NEED THEIR OWN GATE (#1303). `balance-assets.site_a.json`
    carries 517 hq records with a `file_size_bytes` each, and NO PASS CAN
    REACH THEM. `apply_upgrade.merge_added` appends only records ABSENT
    from the profile, and all 517 ids are already in it — the pass
    touches an existing record solely to add `metadata.media_url`. So the
    numbers sat outside every gate: `kenney_hq.py sizes` covered only the
    replacements documents, and nothing covered these.

    They agree with the pool today, which is an accident of when they
    were emitted (after the rasteriser fixes) rather than anything
    keeping them there. A rasteriser change is exactly what invalidated
    150 of site_a's 260 `newSize` values in #1294, and it would have
    invalidated these in the same stroke with nothing to say so.
    """
    rows = json.loads(doc.read_text(encoding="utf-8"))
    drifted, absent = [], []
    for r in rows:
        if r.get("source_root") != HQ_SOURCE_ROOT_NAME:
            continue
        name = str(r.get("file_path", "")).rsplit("/", 1)[-1]
        f = pool_dir / name
        if not f.is_file():
            absent.append(name)
            continue
        size = f.stat().st_size
        if size != r.get("file_size_bytes"):
            drifted.append((name, r.get("file_size_bytes"), size))
            if write:
                r["file_size_bytes"] = size
    n_hq = sum(1 for r in rows if r.get("source_root") == HQ_SOURCE_ROOT_NAME)
    print(f"\n{doc.name}", file=sys.stderr)
    print(f"  hq rows  : {n_hq:,} of {len(rows):,}", file=sys.stderr)
    print(f"  drifted  : {len(drifted):,}", file=sys.stderr)
    print(f"  absent   : {len(absent):,} row(s) name a file the pool does "
          "not hold", file=sys.stderr)
    for name, was, now in drifted[:5]:
        print(f"     {was!s:>9} -> {now:<9} {name}", file=sys.stderr)
    if absent:
        print("  FAIL     : a row naming a file the pool cannot produce "
              "cannot be re-measured.", file=sys.stderr)
        return -1
    if write and drifted:
        doc.write_text(
            json.dumps(rows, indent=1, ensure_ascii=False) + "\n",
            encoding="utf-8")
        print(f"  wrote {doc}", file=sys.stderr)
    return len(drifted)


# The balance documents spell the root out as a plain string; keep the
# name in one place so a rename cannot silently empty this gate.
HQ_SOURCE_ROOT_NAME = "hq"


def cmd_sizes(pool_dir: Path, docs: list[Path], write: bool,
              profiles: list[Path] | None = None,
              balance: list[Path] | None = None) -> int:
    """Re-measure `newSize` in a replacements document against a built pool.

    ⛔ WHY THIS IS A COMMAND AND NOT A ONE-OFF SCRIPT (#1294).

    `newSize` is the byte count of a pool file, and a pool file is a
    RENDER — its size moves whenever the rasteriser changes. #630 and
    #685 both changed what frame a vector is rendered into, and #685
    alone took `vector_backgrounds` from an 8.8%-of-the-artwork crop to
    the whole drawing. Every such fix silently invalidates a number
    committed in this document, and nothing re-derived it: the value was
    measured once, by hand, on whatever pool existed that day.

    Measured 2026-08-26, against a pool rebuilt from the committed
    manifest and the pack: **150 of site_a's 260 rows and 472 of
    site_b's 656** disagreed with the file they name. site_a's published
    share agreed with the REBUILT pool on 776 of 777 records, so the
    stale side was the repository's, not the share's.

    ⚠️ THE DIRECTION IS NOT AN OPINION AND IT IS NOT ADR 0097's SUBJECT.
    0097 governs CONTENT — which records exist and what values they
    carry — and there the profile is the source of truth. A byte count
    is not content: it is a MEASUREMENT of a file the profile names, and
    the pipeline can only ever produce one answer. The profile must
    describe what `build` makes; anything else describes an artifact
    that no longer exists.

    Report-only by default, and non-zero when anything drifted, so it
    can stand as a gate. `--write` re-measures in place.
    """
    total_drift = 0
    hashed_ids = rows_needing_a_hash(profiles or [])
    for doc in docs:
        rows = json.loads(doc.read_text(encoding="utf-8"))
        drifted, absent, hash_drift = [], [], []
        for r in rows:
            name = r["new"].rsplit("/", 1)[-1]
            f = pool_dir / name
            # A row naming a file the pool cannot produce is a DIFFERENT
            # problem from a row whose number is stale, and collapsing
            # the two would let a deleted pool entry read as a size fix.
            if not f.is_file():
                absent.append(name)
                continue
            size = f.stat().st_size
            if size != r.get("newSize"):
                drifted.append((r["id"], name, r.get("newSize"), size))
                if write:
                    r["newSize"] = size
            # The hash follows the same rule as the size and for the same
            # reason: it is a MEASUREMENT of a file the pipeline produces,
            # so the pool is the only thing entitled to state it.
            wants_hash = r["id"] in hashed_ids or "newSha256" in r
            if wants_hash:
                digest = sha256_of(f)
                if digest != r.get("newSha256"):
                    hash_drift.append((name, r.get("newSha256"), digest))
                    if write:
                        r["newSha256"] = digest
            elif profiles and "newSha256" in r:
                # The record stopped carrying a hash, so the row's is
                # now describing nothing.
                hash_drift.append((name, r["newSha256"], None))
                if write:
                    del r["newSha256"]
        print(f"\n{doc.name}", file=sys.stderr)
        print(f"  rows     : {len(rows):,}", file=sys.stderr)
        print(f"  drifted  : {len(drifted):,}", file=sys.stderr)
        print(f"  absent   : {len(absent):,} row(s) name a file the pool "
              "does not hold", file=sys.stderr)
        for name in absent[:5]:
            print(f"     MISSING {name}", file=sys.stderr)
        for _, name, was, now in drifted[:5]:
            print(f"     {was!s:>9} -> {now:<9} {name}", file=sys.stderr)
        if len(drifted) > 5:
            print(f"     … and {len(drifted) - 5:,} more", file=sys.stderr)
        if profiles is not None:
            print(f"  hashes   : {len(hash_drift):,} drifted of "
                  f"{sum(1 for r in rows if r['id'] in hashed_ids):,} row(s) whose "
                  "record carries one (#1302)", file=sys.stderr)
            for name, was, now in hash_drift[:5]:
                print(f"     {str(was)[:12]:>12} -> {str(now)[:12]:<12} {name}",
                      file=sys.stderr)
        if absent:
            print("  FAIL     : a row naming a file the pool cannot produce "
                  "cannot be re-measured. Re-run `build`, or drop the row.",
                  file=sys.stderr)
            return 1
        if write and (drifted or hash_drift):
            doc.write_text(
                json.dumps(rows, indent=1, sort_keys=True, ensure_ascii=False)
                + "\n", encoding="utf-8")
            print(f"  wrote {doc}", file=sys.stderr)
        total_drift += len(drifted) + len(hash_drift)

    for doc in (balance or []):
        n = _measure_balance(pool_dir, doc, write)
        if n < 0:
            return 1
        total_drift += n

    if total_drift and not write:
        print(f"\nFAIL: {total_drift:,} measurement(s) disagree with the "
              "pool. Re-run with --write, then apply_upgrade.py to carry "
              "them into the profiles.", file=sys.stderr)
        return 1
    print(f"\nOK: every measurement matches the pool ({total_drift:,} "
          "rewritten)." if write else
          "\nOK: every measurement matches the pool.", file=sys.stderr)
    return 0


def cmd_verify(pack_root: Path | None, pool_dir: Path) -> int:
    """Verify a pool, and if a pack is given, that every file traces back
    to a real source path through its hash."""
    rc = verify_pool(pool_dir)
    if pack_root is None:
        return rc
    index = {}
    for dirpath, dirnames, filenames in os.walk(pack_root):
        dirnames.sort()
        for fn in sorted(filenames):
            rel = (Path(dirpath) / fn).relative_to(pack_root).as_posix()
            index[path_hash(rel)] = rel
    unresolved = []
    for p in sorted(pool_dir.iterdir()):
        if not p.is_file():
            continue
        m = POOL_NAME_RE.match(p.name)
        if not m or m.group("hash") not in index:
            unresolved.append(p.name)
    print(f"  traced   : {sum(1 for _ in pool_dir.iterdir()) - len(unresolved):,} "
          f"files resolve to a source path in the pack", file=sys.stderr)
    if unresolved:
        print(f"  FAIL     : {len(unresolved)} pool files do not trace back "
              f"(e.g. {unresolved[:3]})", file=sys.stderr)
        return 1
    return rc


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    default_manifest = (Path(__file__).resolve().parents[1]
                        / "upgrades" / "kenney-hq-pool.json")

    b = sub.add_parser("build", help="rebuild the pool from the committed manifest")
    b.add_argument("--pack", required=True, type=Path,
                   help="Kenney All-in-1 pack root (read-only)")
    b.add_argument("--out", required=True, type=Path, help="pool output dir")
    b.add_argument("--manifest", type=Path, default=default_manifest,
                   help="pool manifest (default: seed/upgrades/kenney-hq-pool.json)")
    b.add_argument("--dry-run", action="store_true")
    b.add_argument("--rerender", action="append", metavar="NAME_OR_SOURCE_SUBSTRING",
                   help="re-render these entries even though the PNG exists "
                        "(repeatable). Matches an output name exactly, or any "
                        "source path containing the string. Use after a "
                        "rasterize_svg.mjs fix — a plain build would skip them.")

    s = sub.add_parser("select", help="propose a weighted selection (prints a manifest)")
    s.add_argument("--pack", required=True, type=Path)
    s.add_argument("--limit", type=int, default=700)
    s.add_argument("--out", type=Path, default=None)

    v = sub.add_parser("verify", help="verify an existing pool")
    v.add_argument("--pool", required=True, type=Path)
    v.add_argument("--pack", type=Path, default=None,
                   help="if given, also trace every pool file to a source path")

    z = sub.add_parser(
        "sizes",
        help="re-measure a replacements doc's newSize against a built pool")
    z.add_argument("--pool", required=True, type=Path,
                   help="pool directory produced by `build`")
    z.add_argument("--replacements", required=True, type=Path, action="append",
                   metavar="DOC", help="kenney-hq-replacements.<site>.json "
                                       "(repeatable)")
    z.add_argument("--write", action="store_true",
                   help="re-measure in place (default: report and exit "
                        "non-zero if anything drifted)")
    z.add_argument("--balance", type=Path, action="append", default=[],
                   metavar="DOC", help="balance-assets.<site>.json "
                                       "(repeatable). Its hq records carry a "
                                       "file_size_bytes no other pass can "
                                       "reach (#1303).")
    z.add_argument("--profile", type=Path, action="append", default=[],
                   metavar="PROFILE", help="asset profile(s) the documents "
                                           "repoint (repeatable). Given, the "
                                           "pool's sha256 is measured for "
                                           "every row whose record carries "
                                           "one (#1302).")

    args = ap.parse_args()
    if args.cmd in ("build", "select") and not args.pack.is_dir():
        print(f"error: --pack not a directory: {args.pack}\n"
              "If this path is on the archive share, the mount may have "
              "dropped — that reads as 'No such file or directory'. Check "
              "`mountpoint` and remount before assuming data is missing.",
              file=sys.stderr)
        return 2
    if args.cmd == "build":
        return build(args.pack, args.out, args.manifest, args.dry_run,
                     args.rerender)
    if args.cmd == "select":
        return cmd_select(args.pack, args.limit, args.out)
    if args.cmd == "sizes":
        if not args.pool.is_dir():
            print(f"error: --pool not a directory: {args.pool}\n"
                  "The pool is BUILT, not shipped — there is none on the "
                  "archive share. Run `kenney_hq.py build --pack <pack> "
                  "--out <dir>` first.", file=sys.stderr)
            return 2
        return cmd_sizes(args.pool, args.replacements, args.write,
                         args.profile, args.balance)
    return cmd_verify(args.pack, args.pool)


if __name__ == "__main__":
    raise SystemExit(main())
