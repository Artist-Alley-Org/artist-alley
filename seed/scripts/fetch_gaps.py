#!/usr/bin/env python3
"""
Fetch Layer A (public-safe) gap-filling content from internet sources to
round out the demo seed. The existing dataset is image-heavy and light on
video / EPUB / HDR / glTF sample coverage — this script pulls
representative samples for each missing viewer kind.

All sources are CC0, CC-BY (with attribution preserved in the manifest),
or public domain. License + attribution metadata is captured in
`seed/internet-fetched/MANIFEST.json` next to the bytes.

Usage
-----
    python3 fetch_gaps.py --out /mnt/d/Projects/artist-alley/seed/internet-fetched

Idempotent: re-running skips already-downloaded files. Use --force to
re-fetch.

Network requirements
--------------------
- Direct HTTPS to blender.org, gutenberg.org, polyhaven.com,
  github.com/KhronosGroup, raw.githubusercontent.com.
- For Cloudflare-shielded sources (some Hearthstone fan resources etc.),
  see seed/README.md for the FlareSolverr + Playwright fallback.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
import urllib.request
from dataclasses import dataclass, asdict
from pathlib import Path

# -----------------------------------------------------------------------------
# Gap catalogue — every entry is Layer A (public-safe)
# -----------------------------------------------------------------------------

@dataclass
class GapAsset:
    name: str
    url: str
    target_dir: str       # subdirectory under --out
    target_filename: str  # filename to save as
    license: str
    attribution: str
    source: str
    asset_type: str       # AA asset_type
    notes: str = ""


GAPS: list[GapAsset] = [
    # --- Video coverage ---
    # Sintel — 30-second clip from Blender Foundation's open movie.
    # The full film is 1 hour; we ship a short trailer clip that
    # exercises the video pipeline without bloating the seed.
    GapAsset(
        name="Sintel — trailer clip (30s)",
        url="https://archive.org/download/Sintel/sintel_trailer-480p.mp4",
        target_dir="video",
        target_filename="sintel-trailer-480p.mp4",
        license="CC-BY 3.0",
        attribution="(c) Copyright Blender Foundation | durian.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="Demonstrates HD video viewer with subtitle track support",
    ),
    GapAsset(
        name="Big Buck Bunny — trailer clip (30s)",
        url="https://archive.org/download/BigBuckBunny_124/Content/big_buck_bunny_720p_surround.mp4",
        target_dir="video",
        target_filename="bbb-720p-surround.mp4",
        license="CC-BY 3.0",
        attribution="(c) Copyright Blender Foundation | peach.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="Exercises 5.1 surround audio path in video viewer",
    ),

    # --- EPUB coverage ---
    GapAsset(
        name="Frankenstein (Project Gutenberg)",
        url="https://www.gutenberg.org/ebooks/84.epub.images",
        target_dir="ebook",
        target_filename="frankenstein.epub",
        license="Public Domain",
        attribution="Mary Wollstonecraft Shelley | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
        notes="Demonstrates EPUB reader with embedded images",
    ),
    GapAsset(
        name="Pride and Prejudice (Project Gutenberg)",
        url="https://www.gutenberg.org/ebooks/1342.epub.images",
        target_dir="ebook",
        target_filename="pride-and-prejudice.epub",
        license="Public Domain",
        attribution="Jane Austen | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="The Adventures of Sherlock Holmes (Project Gutenberg)",
        url="https://www.gutenberg.org/ebooks/1661.epub.images",
        target_dir="ebook",
        target_filename="sherlock-holmes.epub",
        license="Public Domain",
        attribution="Arthur Conan Doyle | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),

    # --- 3D / glTF coverage ---
    # Khronos glTF Sample Models — CC-BY. Pull a handful covering the
    # viewer's feature matrix.
    GapAsset(
        name="DamagedHelmet (Khronos sample)",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/DamagedHelmet/glTF-Binary/DamagedHelmet.glb",
        target_dir="3d",
        target_filename="DamagedHelmet.glb",
        license="CC-BY 4.0",
        attribution="ctxwing | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Canonical PBR material test asset",
    ),
    GapAsset(
        name="FlightHelmet (Khronos sample)",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/FlightHelmet/glTF/FlightHelmet.gltf",
        target_dir="3d",
        target_filename="FlightHelmet.gltf",
        license="CC0 1.0",
        attribution="Microsoft | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="High-detail multi-material asset for material inspector",
    ),
    GapAsset(
        name="BoxAnimated (Khronos sample)",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/BoxAnimated/glTF-Binary/BoxAnimated.glb",
        target_dir="3d",
        target_filename="BoxAnimated.glb",
        license="CC0 1.0",
        attribution="Cesium | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Exercises 3D viewer animation timeline",
    ),

    # --- HDR / EXR coverage ---
    # Polyhaven HDR — CC0. The download URL pattern is stable; we pick
    # a small subset.
    GapAsset(
        name="Studio Small 03 HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/studio_small_03_1k.hdr",
        target_dir="hdr",
        target_filename="studio_small_03_1k.hdr",
        license="CC0 1.0",
        attribution="Greg Zaal | Polyhaven",
        source="Polyhaven",
        asset_type="image",
        notes="Studio lighting HDR for IBL preview",
    ),
    GapAsset(
        name="Kloofendal Partly Cloudy HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/kloofendal_partly_cloudy_puresky_1k.hdr",
        target_dir="hdr",
        target_filename="kloofendal_partly_cloudy_puresky_1k.hdr",
        license="CC0 1.0",
        attribution="Greg Zaal | Polyhaven",
        source="Polyhaven",
        asset_type="image",
        notes="Outdoor HDR for IBL preview",
    ),

    # --- Audiobook coverage ---
    # LibriVox short story samples — public domain readings.
    GapAsset(
        name="The Yellow Wallpaper (LibriVox)",
        url="https://archive.org/download/yellow_wallpaper_librivox/yellow_wallpaper.mp3",
        target_dir="audiobook",
        target_filename="yellow-wallpaper.mp3",
        license="Public Domain",
        attribution="Charlotte Perkins Gilman, read by LibriVox volunteer",
        source="LibriVox",
        asset_type="audio",
        notes="Short story ~30 min — fits audiobook viewer demo",
    ),

    # --- Document / code sample coverage ---
    GapAsset(
        name="README sample (Vue project structure)",
        url="https://raw.githubusercontent.com/vuejs/vue/main/README.md",
        target_dir="docs",
        target_filename="vue-README.md",
        license="MIT",
        attribution="Vue.js contributors",
        source="GitHub (vuejs/vue)",
        asset_type="document",
        notes="Exercises markdown viewer with code blocks + tables",
    ),

    # --- NASA imagery ---
    # NASA Image and Video Library is public domain. We pull a few iconic
    # high-resolution images that exercise the image viewer's pan/zoom.
    GapAsset(
        name="Hubble Pillars of Creation",
        url="https://upload.wikimedia.org/wikipedia/commons/6/68/Pillars_of_creation_2014_HST_WFC3-UVIS_full-res_denoised.jpg",
        target_dir="nasa",
        target_filename="pillars-of-creation.jpg",
        license="Public Domain",
        attribution="NASA, ESA, and the Hubble Heritage Team (STScI/AURA)",
        source="NASA",
        asset_type="image",
        notes="High-res pan/zoom demo",
    ),
    GapAsset(
        name="Earthrise from Apollo 8",
        url="https://upload.wikimedia.org/wikipedia/commons/1/1f/NASA-Apollo8-Dec24-Earthrise.jpg",
        target_dir="nasa",
        target_filename="earthrise.jpg",
        license="Public Domain",
        attribution="NASA / William Anders | Apollo 8",
        source="NASA",
        asset_type="image",
    ),
]

# -----------------------------------------------------------------------------
# Fetch + verify
# -----------------------------------------------------------------------------

def sha256_of_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch(gap: GapAsset, out_root: Path, force: bool = False) -> dict | None:
    out_dir = out_root / gap.target_dir
    out_dir.mkdir(parents=True, exist_ok=True)
    target = out_dir / gap.target_filename

    if target.exists() and not force:
        size = target.stat().st_size
        print(f"  [cached] {gap.name} ({size:,} B)", file=sys.stderr)
        return {
            "name": gap.name,
            "path": str(target.relative_to(out_root)),
            "size_bytes": size,
            "sha256": sha256_of_file(target),
            "license": gap.license,
            "attribution": gap.attribution,
            "source": gap.source,
            "asset_type": gap.asset_type,
            "notes": gap.notes,
        }

    print(f"  [fetch ] {gap.name} ← {gap.url}", file=sys.stderr)
    req = urllib.request.Request(gap.url, headers={
        "User-Agent": "artist-alley-seed-fetcher/1.0",
    })
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            data = resp.read()
    except Exception as e:
        print(f"    error: {e}", file=sys.stderr)
        return None

    target.write_bytes(data)
    size = target.stat().st_size
    print(f"    -> {size:,} B written", file=sys.stderr)
    return {
        "name": gap.name,
        "path": str(target.relative_to(out_root)),
        "size_bytes": size,
        "sha256": sha256_of_file(target),
        "license": gap.license,
        "attribution": gap.attribution,
        "source": gap.source,
        "asset_type": gap.asset_type,
        "notes": gap.notes,
        "fetched_from": gap.url,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, type=Path,
                        help="Output directory (gitignored cache)")
    parser.add_argument("--force", action="store_true",
                        help="Re-fetch even if file is already cached")
    args = parser.parse_args()

    args.out.mkdir(parents=True, exist_ok=True)

    print(f"fetching {len(GAPS)} gap assets to {args.out}", file=sys.stderr)
    manifest: list[dict] = []
    failed: list[str] = []
    for gap in GAPS:
        result = fetch(gap, args.out, force=args.force)
        if result is None:
            failed.append(gap.name)
            continue
        manifest.append(result)

    manifest_path = args.out / "MANIFEST.json"
    manifest_path.write_text(json.dumps({
        "asset_count": len(manifest),
        "total_bytes": sum(m["size_bytes"] for m in manifest),
        "assets": manifest,
        "failed": failed,
    }, indent=2), encoding="utf-8")

    print(f"\nwrote manifest to {manifest_path}", file=sys.stderr)
    print(f"  {len(manifest)} assets fetched / cached", file=sys.stderr)
    print(f"  total: {sum(m['size_bytes'] for m in manifest) / 2**20:.1f} MB", file=sys.stderr)
    if failed:
        print(f"  {len(failed)} failed:", file=sys.stderr)
        for name in failed:
            print(f"    - {name}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
