#!/usr/bin/env python3
"""
Pull game-adjacent video reference from the Pexels API into the seed
dataset (#572).

WHY VIDEO, AND WHY THESE SEARCHES
---------------------------------
site_a shipped 47 videos against 1,007 assets, and most of them were
generic stock: sunsets, traffic, coffee. A studio's reference library is
not a stock reel — it is the footage the team actually looks at while
working. So the query list is deliberately narrow and is the deliverable
as much as the count:

    arcade / neon / retro gaming     what the art direction references
    pixel / glitch / crt             screen-space and post effects
    particles / smoke / sparks       VFX plates the VFX team matches to
    controller / joystick / keyboard capture reference for input UI
    abstract loop / motion graphics  looping backgrounds for menus

Each query is attributed to the team that would plausibly have saved it,
so the videos land across Reference / VFX / Marketing Art rather than
piling into one bucket — the same imbalance #572 exists to fix.

PROVENANCE (#602 / #675)
------------------------
Two keys, both required, and the byte URL is written only after a HEAD
confirms it serves exactly the size recorded:

    metadata.fetched_from   the pexels.com PAGE — where the licence and
                            the videographer credit live
    metadata.media_url      videos.pexels.com CDN link — the exact bytes

The API hands both back in one response, so unlike #602's retrofit
nothing here needs a Cloudflare-guarded page fetch. The HEAD still runs:
an API field that agrees with itself is not evidence, and the whole point
of recording a byte count next to a URL is that a rebuild can check it.

LICENCE
-------
Pexels content is **not** CC0. The Pexels License permits free use with
no attribution required, and this dataset credits the videographer
anyway because ATTRIBUTIONS.md is what makes the redistribution
defensible. It is approved for BOTH sites (owner decision 2026-07-25);
#675 found a stale "site_b only" string that had been fixed in one
profile and left in three other files, so that phrase must never be
reintroduced here.

Usage
-----
    # search + stage + write the upgrade doc (needs PEXELS_API_KEY)
    python3 pexels_gameplay.py fetch --dest <site_a dir> [--per-query 4]

    # offline: are the committed records well-formed?
    python3 pexels_gameplay.py check

The key is read from the environment, or from a `.env` given with
--env-file. It is never printed and never written anywhere.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

SEED_DIR = Path(__file__).resolve().parents[1]
UPGRADES = SEED_DIR / "upgrades"
PROFILES = SEED_DIR / "profiles"

_NAMESPACE_SEED = "artist-alley.seed.v1"

UA = ("artist-alley-seed-fetcher/2.0 "
      "(+https://github.com/Artist-Alley-Org/artist-alley)")

LICENSE = "Pexels License"
ACQUISITION_SOURCE = "Pexels"

# query -> (team, collection, tags, why). Ordered; the fetcher walks the
# list and takes --per-query results from each, so the earlier entries
# are the ones that survive a small budget.
QUERIES: list[dict] = [
    {"q": "arcade machine", "team": "Reference", "collection": "Internet Reference",
     "tags": ["video", "reference", "arcade", "gaming"],
     "why": "cabinet + CRT reference for the retro art direction"},
    {"q": "video game controller", "team": "UI", "collection": "Internet Reference",
     "tags": ["video", "reference", "input", "controller"],
     "why": "input-prompt reference for the UI team's controller glyphs"},
    {"q": "neon lights abstract", "team": "Marketing Art", "collection": "Project Mirror",
     "tags": ["video", "keyart", "neon", "loop"],
     "why": "menu / key-art backplates"},
    {"q": "particles floating", "team": "VFX", "collection": "Project Mirror",
     "tags": ["video", "vfx", "particles", "plate"],
     "why": "particle plates the VFX team matches against"},
    {"q": "smoke effect black background", "team": "VFX", "collection": "Project Mirror",
     "tags": ["video", "vfx", "smoke", "plate"],
     "why": "smoke plates, alpha-friendly on black"},
    {"q": "glitch effect screen", "team": "VFX", "collection": "Project Mirror",
     "tags": ["video", "vfx", "glitch", "screenspace"],
     "why": "screen-space corruption reference"},
    {"q": "pixel art animation", "team": "Animation", "collection": "Project Echo",
     "tags": ["video", "animation", "pixel", "reference"],
     "why": "2D animation timing reference for the pixel work"},
    {"q": "keyboard gaming rgb", "team": "UI", "collection": "Internet Reference",
     "tags": ["video", "reference", "input", "hardware"],
     "why": "peripheral reference to sit next to the controller capture"},
    {"q": "abstract motion background loop", "team": "Marketing Art",
     "collection": "Project Mirror",
     "tags": ["video", "keyart", "loop", "motion"],
     "why": "looping menu backgrounds"},
    {"q": "sparks fire slow motion", "team": "VFX", "collection": "Project Mirror",
     "tags": ["video", "vfx", "sparks", "plate"],
     "why": "ember + spark plates"},
    {"q": "esports tournament", "team": "Reference", "collection": "Internet Reference",
     "tags": ["video", "reference", "esports", "gaming"],
     "why": "audience + stage reference for the tournament mode mock-ups"},
    {"q": "retro gaming console", "team": "Reference", "collection": "Internet Reference",
     "tags": ["video", "reference", "retro", "hardware"],
     "why": "hardware reference for the retro fantasy kit's framing"},
]

# HD, landscape, and big enough to be worth a viewer — but not the 4K
# files, which run 80-200 MB each and would dominate the dataset's byte
# budget for no extra information (#604's MAX_FILE_SIZE_BYTES logic, same
# reasoning). Preference order, first match wins.
QUALITY_ORDER = ("hd", "sd")
MAX_WIDTH = 1920
MAX_BYTES = 60 * 1024 * 1024


def stable_uuid(*parts: str) -> str:
    h = hashlib.sha256()
    h.update(_NAMESPACE_SEED.encode())
    for p in parts:
        h.update(b"\x00")
        h.update(str(p).encode())
    d = h.hexdigest()
    return f"{d[0:8]}-{d[8:12]}-{d[12:16]}-{d[16:20]}-{d[20:32]}"


def stable_int(n: int, *parts: str) -> int:
    if n <= 0:
        return 0
    h = hashlib.sha256()
    h.update(_NAMESPACE_SEED.encode())
    for p in parts:
        h.update(b"\x00")
        h.update(str(p).encode())
    return int.from_bytes(h.digest()[:8], "big") % n


def api_key(env_file: Path | None) -> str:
    """PEXELS_API_KEY from the environment, or from a .env we only READ.

    The repo `.env` is the operator's file; this parses it and never
    writes it. The value is not printed by anything in this module.
    """
    key = os.environ.get("PEXELS_API_KEY", "").strip()
    if key:
        return key
    if env_file and env_file.is_file():
        for line in env_file.read_text(encoding="utf-8").splitlines():
            if line.startswith("PEXELS_API_KEY="):
                return line.split("=", 1)[1].strip().strip('"').strip("'")
    raise SystemExit(
        "error: PEXELS_API_KEY not set and not found in --env-file.\n"
        "  export PEXELS_API_KEY=... , or pass --env-file <path to .env>")


def api_get(path: str, params: dict, key: str) -> dict:
    url = f"https://api.pexels.com/{path}?{urllib.parse.urlencode(params)}"
    req = urllib.request.Request(url, headers={
        "Authorization": key, "User-Agent": UA})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.loads(resp.read().decode("utf-8"))


def head_length(url: str) -> int | None:
    req = urllib.request.Request(url, headers={"User-Agent": UA}, method="HEAD")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            n = resp.headers.get("Content-Length")
            return int(n) if n else None
    except Exception:
        return None


def pick_file(video: dict) -> dict | None:
    """The best video_file for the dataset, or None if nothing fits."""
    files = [f for f in video.get("video_files", [])
             if (f.get("file_type") or "").endswith("mp4")
             and (f.get("width") or 0) <= MAX_WIDTH]
    for quality in QUALITY_ORDER:
        cands = [f for f in files if f.get("quality") == quality]
        if cands:
            return max(cands, key=lambda f: (f.get("width") or 0))
    return None


def slug(text: str) -> str:
    return re.sub(r"-+", "-", re.sub(r"[^a-z0-9]+", "-", text.lower())).strip("-")


def build_record(video: dict, vf: dict, spec: dict, size: int) -> dict:
    vid = str(video["id"])
    author = video.get("user", {}).get("name") or "Unknown"
    name = f"pexels-{vid}-{slug(author)}.mp4"
    key = f"pexels:{vid}"
    created = "2026-07-27T12:00:00Z"
    return {
        "archive_state": "active",
        "asset_type": "video",
        "attribution": f"{author} via Pexels",
        "brand_workspace": None,
        "collection_name": spec["collection"],
        "created_at": created,
        "description": (
            f"{vf.get('width')}x{vf.get('height')} {video.get('duration')}s — "
            f"{spec['why']} — Pexels License — free to use, not CC0/PD "
            f"(see ATTRIBUTIONS.md)"),
        "external_id": "",
        "field_values": {"runtime_seconds": video.get("duration")},
        "file_extension": "mp4",
        "file_path": f"videos/internet/{name}",
        "file_size_bytes": size,
        "id": stable_uuid("asset", key),
        "last_reviewed_at": created,
        "layer": "A",
        "license": LICENSE,
        "metadata": {
            "acquisition_source": ACQUISITION_SOURCE,
            "attribution": f"{author} via Pexels",
            "fetched_from": video["url"],
            "filename": name,
            "kind": "video",
            "license": LICENSE,
            "media_url": vf["link"],
            "search_query": spec["q"],
        },
        "owner_username": "seed.bot",
        "review_notes": None,
        "reviewer_username": None,
        "sensitivity_tier": "public",
        "studio": "shared",
        "tags": list(spec["tags"]),
        "team_name": spec["team"],
        "title": f"Pexels {vid} {author}",
        "updated_at": created,
        "workflow_state": "approved",
    }


def build_post(rec: dict) -> dict:
    return {
        "asset_ids": [rec["id"]],
        "asset_types_in_post": ["video"],
        "author_username": "seed.bot",
        "brand_workspace": rec["brand_workspace"],
        "collection_name": rec["collection_name"],
        "created_at": rec["created_at"],
        "description": rec["description"],
        "id": stable_uuid("post", "pexels-gameplay", rec["id"]),
        "is_mixed_type": False,
        "layer": "A",
        "post_kind": "solo_showcase",
        "sensitivity_tier": "public",
        "studio": "shared",
        "tags": list(rec["tags"]),
        "team_name": rec["team_name"],
        "title": rec["title"],
        "updated_at": rec["updated_at"],
        "workflow_state": "approved",
    }


def download(url: str, dest: Path, expect: int) -> tuple[bool, str]:
    import shutil
    tmp = dest.with_suffix(dest.suffix + ".part")
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    try:
        with urllib.request.urlopen(req, timeout=600) as resp:
            dest.parent.mkdir(parents=True, exist_ok=True)
            with tmp.open("wb") as f:
                shutil.copyfileobj(resp, f, length=1 << 20)
    except Exception as e:
        tmp.unlink(missing_ok=True)
        return False, f"download failed: {e}"
    got = tmp.stat().st_size
    if got != expect:
        tmp.unlink(missing_ok=True)
        return False, f"size mismatch: expected {expect:,}, got {got:,}"
    tmp.replace(dest)
    return True, f"{got:,} B"


def cmd_fetch(args: argparse.Namespace) -> int:
    key = api_key(args.env_file)
    existing_ids: set[str] = set()
    for doc in ("added-assets.site_a.json", "pexels-assets.site_a.json"):
        p = UPGRADES / doc
        if p.is_file():
            for r in json.loads(p.read_text(encoding="utf-8")):
                ff = (r.get("metadata") or {}).get("fetched_from") or ""
                m = re.search(r"-(\d+)/?$", ff.rstrip("/"))
                if m:
                    existing_ids.add(m.group(1))
    print(f"{len(existing_ids)} Pexels video(s) already in the dataset",
          file=sys.stderr)

    records: list[dict] = []
    seen: set[str] = set(existing_ids)
    for spec in QUERIES:
        try:
            page = api_get("videos/search", {
                "query": spec["q"], "per_page": 20,
                "orientation": "landscape", "size": "medium",
            }, key)
        except Exception as e:
            print(f"  FAIL query {spec['q']!r}: {e}", file=sys.stderr)
            continue
        taken = 0
        for video in page.get("videos", []):
            if taken >= args.per_query:
                break
            vid = str(video["id"])
            if vid in seen:
                continue
            vf = pick_file(video)
            if vf is None:
                continue
            # The HEAD is the whole contract: a media_url is written only
            # after the CDN confirms what it serves (#602). An API field
            # that agrees with itself proves nothing.
            length = head_length(vf["link"])
            if not length or length > MAX_BYTES:
                continue
            rec = build_record(video, vf, spec, length)
            records.append(rec)
            seen.add(vid)
            taken += 1
        print(f"  {spec['q']:<32} +{taken}", file=sys.stderr)

    print(f"\nselected {len(records)} new videos", file=sys.stderr)
    if args.dry_run:
        return 0

    staged = failed = 0
    for r in records[:]:
        dest = args.dest / r["file_path"]
        if dest.is_file() and dest.stat().st_size == r["file_size_bytes"]:
            staged += 1
            continue
        ok, note = download(r["metadata"]["media_url"], dest,
                            r["file_size_bytes"])
        if ok:
            staged += 1
        else:
            print(f"  DROP {r['metadata']['filename']}: {note}", file=sys.stderr)
            records.remove(r)
            failed += 1

    # Pre-staged at the destination, exactly like #604's videos: there is
    # no LOCAL source tree for them. `media_url` is what turns that from a
    # dead end into a download (#602).
    for r in records:
        r["source_root"] = "site"
        r["source_path"] = r["file_path"]

    posts = [build_post(r) for r in records]
    (UPGRADES / "pexels-assets.site_a.json").write_text(
        json.dumps(records, indent=1, ensure_ascii=False) + "\n",
        encoding="utf-8")
    (UPGRADES / "pexels-posts.site_a.json").write_text(
        json.dumps(posts, indent=1, ensure_ascii=False) + "\n",
        encoding="utf-8")
    print(f"staged {staged}, dropped {failed}; wrote "
          f"{len(records)} assets + {len(posts)} posts", file=sys.stderr)
    by_team: dict[str, int] = {}
    for r in records:
        by_team[r["team_name"]] = by_team.get(r["team_name"], 0) + 1
    print(f"  by team: {by_team}", file=sys.stderr)
    return 0


def cmd_check(args: argparse.Namespace) -> int:
    p = UPGRADES / "pexels-assets.site_a.json"
    if not p.is_file():
        print(f"missing {p}", file=sys.stderr)
        return 1
    records = json.loads(p.read_text(encoding="utf-8"))
    problems: list[str] = []
    for r in records:
        m = r.get("metadata") or {}
        if not m.get("fetched_from", "").startswith("https://www.pexels.com/"):
            problems.append(f"{r['id']}: fetched_from is not a Pexels page")
        if not m.get("media_url", "").startswith("https://videos.pexels.com/"):
            problems.append(f"{r['id']}: media_url is not a Pexels CDN link")
        if not r.get("file_size_bytes"):
            problems.append(f"{r['id']}: no file_size_bytes to check against")
        # #675's regression, kept from coming back: the licence claim must
        # not scope Pexels content to one site.
        if "site_b only" in json.dumps(r):
            problems.append(f"{r['id']}: stale 'site_b only' licence claim")
    for x in problems:
        print(f"  PROBLEM: {x}", file=sys.stderr)
    print(f"{len(records)} Pexels records, {len(problems)} problem(s)",
          file=sys.stderr)
    return 1 if problems else 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    f = sub.add_parser("fetch")
    f.add_argument("--dest", required=True, type=Path,
                   help="site directory the bytes are staged into")
    f.add_argument("--per-query", type=int, default=4)
    f.add_argument("--env-file", type=Path, default=None,
                   help="read PEXELS_API_KEY from this .env (read-only)")
    f.add_argument("--dry-run", action="store_true")
    sub.add_parser("check")
    args = ap.parse_args()
    return cmd_fetch(args) if args.cmd == "fetch" else cmd_check(args)


if __name__ == "__main__":
    raise SystemExit(main())
