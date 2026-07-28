#!/usr/bin/env python3
"""
Rebalance site_a's per-team library so every team has a browsable
catalogue and no single team owns the dataset (#572, closes #562).

THE SHAPE THAT WAS WRONG
------------------------
site_a shipped 1,007 assets across 11 teams like this:

    Environment 476 (47.3%)   UI            59
    Audio       182           Props         40
    VFX         115           Tech Art      20
    Reference   104           Textures       8
                              Marketing Art  3
                              Animation      0
                              Characters     0

Two teams empty, two in single digits, one holding nearly half. Clicking
into Animation showed nothing; clicking into Environment showed the
dataset. Both readings are wrong, and the second is the harder one — a
floor alone leaves Environment at 37%.

THE CONSTRAINT THE ISSUE ASSUMED, AND WHY IT DOES NOT HOLD
----------------------------------------------------------
#572 said this "needs a cross-studio data rewrite, not post composition"
because Animation had 2 assets globally. That was true of the MANIFEST,
not of the source. site_a's 1,007 records are drawn from a 78,441-file
CC0 bundle; ~1.3% of it is in use. The material for every starved team
is already on the share:

    2D assets/New Platformer Pack                    134 animation-frame vectors
    2D assets/Fish Pack                              126 swim/rest frame vectors
    3D assets/{Blocky,Mini} Characters, Cube Pets     68 character models
    3D assets/Animated Characters {Protagonists,…}     9 .fbx animation clips
    UI assets/UI Pack{,- Sci-fi,- Adventure}         933 UI vectors
    2D assets/{Pattern,Brick,Prototype} …            360 texture sources

So this is an ADDITIVE pass, not a rewrite.

THE TWO LEVERS, AND WHY BOTH
----------------------------
1. CORRECTION (caps Environment). 55 of Environment's 476 are mis-teamed
   in the source CSV and say so in their own tags: 34 minimap-pack icons
   tagged `ui`/`icon`/`minimap`, 18 fantasy TEXTURE plates tagged
   `texture`/`tiling`, 3 voiceover clips tagged `voiceover`. Moving them
   to UI / Textures / Audio is a correctness fix that happens to shave
   11.6% off the dominant team and lift two starved ones. See
   TEAM_CORRECTIONS.

2. ADDITION (raises everyone else). Pack-sourced records for the teams
   below their floor, which drops Environment's SHARE without deleting a
   single record. Deleting was considered and rejected: posts,
   collections and group siblings all reference those ids, and #604's
   whole "swap the file, keep the record" rule exists because removing
   records from this dataset breaks composition silently.

THE FLOOR: 60
-------------
Chosen from the product, not from roundness. `/search` returns 25 per
page and the profile/browse rails render 24 tiles, so a team whose
entire library fits in one response has nothing to scroll and nothing to
filter — it reads as a stub even though it is technically non-empty.
60 is two and a half pages: the first screen fills, pagination exists,
and narrowing by type or tag still leaves a grid rather than a single
row. Every team lands well above it — the smallest ends at 116 once the
#572 video pull lands on top, and Environment drops from 47.3% of the
library to 21.6%.

TWO SOURCE ROOTS, FOR TWO REASONS
---------------------------------
  `vector` -> the kenney-hq pool (source_root "hq"). SVGs have to be
              RENDERED, and the renderer is the pipeline's (#679). New
              vectors are appended to kenney-hq-pool.json so
              `kenney_hq.py build` reproduces them like any other pool
              file, through the current rasteriser.
  `file`   -> copied verbatim from the bundle (source_root "pack", new).
              3D models, audio and already-large PNGs have no render
              step; routing them through an image pool would only add a
              naming layer. Destination paths keep the bundle's own
              directory structure, which is unique by construction —
              the collision RULE 1 in kenney_hq.py exists to prevent.

PROVENANCE
----------
Every added record carries `metadata.fetched_from` (the kenney.nl pack
page — licence + attribution evidence) and `metadata.source_archive`
{url, member, sha256}: the free per-pack CC0 zip, the path inside it,
and the sha256 of that member's bytes. Kenney's per-pack zips are
byte-identical to the All-in-1 bundle (asserted by
`kenney_pack_sources.py verify`), so a machine with no archive share can
reconstruct these records from the internet alone —
`populate_archive.py` does exactly that when a `pack` file is absent.
See kenney_pack_sources.py for why this is NOT `media_url`.

Usage
-----
    # what would be added, per team, without writing anything
    python3 studio_balance.py plan --pack "<All-in-1 root>"

    # write the upgrade docs (pool entries + assets + posts + corrections)
    python3 studio_balance.py emit --pack "<All-in-1 root>"

    # offline gate: committed docs agree with the recipes
    python3 studio_balance.py check
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import struct
import sys
from pathlib import Path

SEED_DIR = Path(__file__).resolve().parents[1]
UPGRADES = SEED_DIR / "upgrades"
PROFILES = SEED_DIR / "profiles"

# Same namespace as sanitize_and_assemble.stable_uuid, so ids generated
# here can never collide with ids generated there for a different file.
_NAMESPACE_SEED = "artist-alley.seed.v1"

# Render size for vectors — the pool's own constant. Imported rather than
# re-declared where possible; duplicated here only as the default.
DEFAULT_RENDER_PX = 512

# RULE 2 (kenney_hq.py): quality is dimensional, never byte size. A
# bitmap copied verbatim has to clear the size the pool RENDERS vectors
# at, otherwise the additions reintroduce exactly the sprite-tile wall
# #604 removed. 512, not 1024: 1024 is the bar for a CURATED bitmap
# standing in for artwork, while these are texture plates and skin sheets
# whose native size is 512-1024 and which are legitimately that size.
MIN_BITMAP_EDGE = 512

HQ_SOURCE_ROOT = "hq"
PACK_SOURCE_ROOT = "pack"

LICENSE = "CC0 1.0"
ATTRIBUTION = "Kenney (kenney.nl)"
ACQUISITION_SOURCE = "Kenney.nl"

EXT_TYPE = {
    "png": "image", "jpg": "image", "jpeg": "image",
    "glb": "3d", "gltf": "3d", "fbx": "3d", "obj": "3d",
    "ogg": "audio", "mp3": "audio", "wav": "audio",
    "ttf": "font", "otf": "font",
}

TYPE_DIR = {"image": "images", "3d": "3d", "audio": "audio",
            "font": "fonts", "video": "videos", "document": "documents"}


# --------------------------------------------------------------------------
# 0. DE-DUPLICATION — packs that appear in both source trees
# --------------------------------------------------------------------------
# Bundle pack -> the directory the SAME pack is unpacked under in the
# local source tree. Only the packs that appear in both; the layout below
# the pack root is identical, which is what makes the tail comparable, and
# without this the bundle's copy of an image site_a already ships would be
# added a second time — counts up, library unchanged.
LOCAL_PACK_ALIASES: dict[str, str] = {
    "2D assets/Light Masks": "kenney_light-masks-1.0",
    "2D assets/Prototype Textures": "kenney_prototype-textures",
}

# Bundle files a recipe would otherwise pick that must not ship, with
# the measurement that condemned them. All three are the LEGITIMATE
# padded-canvas case #679 identified — the source declares a large square
# around a small drawing, so the frame is the artist's composition and
# trimming it would silently recompose the art. That makes them fine
# files and bad tiles: `detect_oversized_canvas.mjs` reports 4-8% content,
# and a card that is 92% transparent reads as a broken thumbnail whoever
# is at fault. Excluded at SELECTION rather than trimmed at render, so
# the renderer keeps its one rule and the sampler simply picks something
# else from the same pack.
SOURCE_EXCLUSIONS: dict[str, str] = {
    "2D assets/Fish Pack/Vector/bubble_a.svg":
        "4.0% content in a 512x512 frame (declared 64x64 around a small "
        "bubble) — the #679 do-not-trim example, by name",
    "2D assets/Fish Pack/Vector/bubble_b.svg":
        "4.0% content, same shape as bubble_a",
    "2D assets/Fish Pack/Vector/hud_dot.svg":
        "7.9% content — a HUD dot centred in a square canvas",
    # The other direction: sources whose artwork runs OUTSIDE the viewBox
    # they declare, so any faithful render clips them. Measured with
    # probe_render_loss.mjs (re-render at +12% viewBox, count solid pixels
    # in the added ring); the bar is 1%, two orders of magnitude above the
    # 0.08% anti-aliased fringe a tight render normally leaves.
    #
    # Not "fixed" by padding the frame: the viewBox is the artist's
    # composition and widening it is the same silent recomposition #679
    # rejected. A source we cannot render faithfully simply does not ship.
    "2D assets/Robot Pack/Vector/vector_robotsSide.svg":
        "13.6% of the ring outside its declared viewBox",
    "2D assets/Platformer Characters 1/Vector/adventurer_vector.svg":
        "2.5% of the ring outside its declared viewBox",
    "2D assets/Platformer Characters 1/Vector/female_vector.svg":
        "2.3% of the ring outside its declared viewBox",
    "2D assets/Platformer Characters 1/Vector/player_vector.svg":
        "2.3% of the ring outside its declared viewBox",
    "2D assets/Platformer Characters 1/Vector/soldier_vector.svg":
        "2.4% of the ring outside its declared viewBox",
    "2D assets/Platformer Characters 1/Vector/zombie_vector.svg":
        "2.4% of the ring outside its declared viewBox",
}

# --------------------------------------------------------------------------
# 1. CORRECTIONS — records the source CSV puts on the wrong team
# --------------------------------------------------------------------------
# Matched on the record's ORIGINAL source path (`replaced_source_path`
# falls back to `source_path`), because that is what survives the #604
# file swap. Each entry names what the records already say about
# themselves in their own tags, so this is a data fix with evidence
# rather than a thumb on the scale.
TEAM_CORRECTIONS: list[dict] = [
    {
        "match": "kenney_minimap-pack/",
        "from": "Environment",
        "to": "UI",
        "why": "minimap iconography — every one of these records is "
               "already tagged icon/minimap/ui",
    },
    {
        "match": "kenney_retro-textures-fantasy/",
        "from": "Environment",
        "to": "Textures",
        "why": "tiling texture plates, tagged texture/tiling",
    },
    {
        "match": "kenney_voiceover-pack/",
        "from": "Environment",
        "to": "Audio",
        "why": "voiceover clips, tagged voiceover/vocal — audio has "
               "never been an Environment deliverable",
    },
]


# --------------------------------------------------------------------------
# 2. RECIPES — which bundle material each team gets
# --------------------------------------------------------------------------
# Data, in the spirit of kenney_hq.PACK_WEIGHTS: edit the table, re-run
# `plan`, look at the numbers. `take` is a CAP per rule, not a quota —
# rules are consumed in order until the team's target is met, so a rule
# that runs dry is covered by the next one instead of failing the build.
#
# `match` is a regex over the bundle-relative path INSIDE the pack dir.
# GLB is preferred over OBJ everywhere: it is self-contained, so a model
# cannot arrive without its material (#486 companions still work, there
# is just nothing to resolve).

TEAM_RECIPES: dict[str, list[dict]] = {
    "UI": [
        {"pack": "UI assets/UI Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 45,
         "collection": "Project Echo", "tags": ["ui", "widget", "vector"]},
        {"pack": "UI assets/UI Pack - Sci-fi", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 32,
         "collection": "Project Mirror", "tags": ["ui", "widget", "scifi"]},
        {"pack": "UI assets/UI Pack - Adventure", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 20,
         "collection": "Project Compass", "tags": ["ui", "widget", "adventure"]},
        {"pack": "UI assets/UI Pack", "kind": "file",
         "match": r"^Sounds/.*\.ogg$", "take": 6,
         "collection": "Project Echo", "tags": ["ui", "sfx", "interface"]},
        {"pack": "UI assets/Cursor Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 20,
         "collection": "Project Compass", "tags": ["ui", "cursor", "vector"]},
    ],
    "Props": [
        {"pack": "3D assets/Food Kit", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 70,
         "collection": "Project Echo", "tags": ["prop", "lowpoly", "3d"]},
        # Holiday Kit, not Furniture Kit: Furniture ships DAE/GLTF/STL and
        # no GLB, and a multi-file GLTF that arrives without its .bin is
        # the exact failure Sponza is sitting in. Self-contained formats
        # only for the bulk fill.
        {"pack": "3D assets/Holiday Kit", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 55,
         "collection": "Project Citylight", "tags": ["prop", "interior", "3d"]},
        {"pack": "3D assets/Survival Kit", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 30,
         "collection": "Project Echo", "tags": ["prop", "survival", "3d"]},
        {"pack": "3D assets/Blaster Kit", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 20,
         "collection": "Project Mirror", "tags": ["prop", "weapon", "3d"]},
    ],
    "VFX": [
        {"pack": "2D assets/Splat Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 20,
         "collection": "Project Mirror", "tags": ["vfx", "decal", "vector"]},
        {"pack": "2D assets/Light Masks", "kind": "file",
         "match": r"^(Default|Transparent)/.*\.png$", "take": 25,
         "collection": "Project Mirror", "tags": ["vfx", "light", "mask"]},
        {"pack": "2D assets/Particle Pack", "kind": "file",
         "match": r"^PNG.*/.*\.png$", "take": 20,
         "collection": "Project Mirror", "tags": ["vfx", "particle"]},
    ],
    "Characters": [
        {"pack": "2D assets/New Platformer Pack", "kind": "vector",
         "match": r"^Vector/Characters/.*\.svg$", "take": 45,
         "collection": "Project Echo", "tags": ["character", "sprite", "vector"]},
        {"pack": "3D assets/Mini Characters", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 26,
         "collection": "Project Echo", "tags": ["character", "lowpoly", "3d"]},
        {"pack": "3D assets/Cube Pets", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 24,
         "collection": "Project Citylight", "tags": ["character", "creature", "3d"]},
        {"pack": "3D assets/Blocky Characters", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 18,
         "collection": "Project Echo", "tags": ["character", "lowpoly", "3d"]},
        {"pack": "2D assets/Animal Pack Remastered", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 8,
         "collection": "Project Echo", "tags": ["character", "creature", "vector"]},
        {"pack": "2D assets/Animal Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 8,
         "collection": "Project Echo", "tags": ["character", "creature", "vector"]},
        {"pack": "2D assets/Toon Characters", "kind": "vector",
         "match": r"^[^/]+/Vector/.*\.svg$", "take": 6,
         "collection": "Project Echo", "tags": ["character", "modular", "vector"]},
        {"pack": "2D assets/Toon Characters", "kind": "file",
         "match": r"^[^/]+/Tilesheet/.*HD\.png$", "take": 6,
         "collection": "Project Echo", "tags": ["character", "spritesheet", "2d"]},
        {"pack": "2D assets/Platformer Characters 1", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 5,
         "collection": "Project Echo", "tags": ["character", "sprite", "vector"]},
        {"pack": "3D assets/Animated Characters Protagonists", "kind": "file",
         "match": r"^(Skins/[^/]+\.png|Model/[^/]+\.fbx)$", "take": 5,
         "collection": "Project Mirror", "tags": ["character", "skin", "cinematic"]},
        {"pack": "3D assets/Animated Characters Survivors", "kind": "file",
         "match": r"^(Skins/[^/]+\.png|Model/[^/]+\.fbx)$", "take": 5,
         "collection": "Project Mirror", "tags": ["character", "skin", "cinematic"]},
        {"pack": "2D assets/Monster Builder Pack", "kind": "file",
         "match": r"^Spritesheet/.*\.png$", "take": 2,
         "collection": "Project Echo", "tags": ["character", "creature", "spritesheet"]},
        {"pack": "2D assets/Robot Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 2,
         "collection": "Project Mirror", "tags": ["character", "robot", "vector"]},
        {"pack": "2D assets/Shape Characters", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 1,
         "collection": "Project Echo", "tags": ["character", "abstract", "vector"]},
    ],
    "Textures": [
        {"pack": "2D assets/Brick Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 50,
         "collection": "Project Citylight", "tags": ["texture", "brick", "tiling"]},
        {"pack": "2D assets/Pattern Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 44,
         "collection": "Project Echo", "tags": ["texture", "pattern", "tiling"]},
        {"pack": "2D assets/Pattern Pack Lines", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 30,
         "collection": "Project Echo", "tags": ["texture", "pattern", "tiling"]},
    ],
    "Marketing Art": [
        {"pack": "2D assets/Flag Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 80,
         "collection": "Project Echo", "tags": ["marketing", "flag", "banner"]},
        {"pack": "2D assets/Planets", "kind": "file",
         "match": r"^Parts/.*\.png$", "take": 34,
         "collection": "Project Mirror", "tags": ["marketing", "keyart", "space"]},
        {"pack": "2D assets/Skyboxes", "kind": "file",
         "match": r"^Skyboxes/.*\.png$", "take": 5,
         "collection": "Project Mirror", "tags": ["marketing", "keyart", "sky"]},
        {"pack": "2D assets/Ranks Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 4,
         "collection": "Project Echo", "tags": ["marketing", "badge", "rank"]},
        {"pack": "2D assets/Generic Items", "kind": "file",
         "match": r"^Spritesheet/.*\.png$", "take": 2,
         "collection": "Project Echo", "tags": ["marketing", "sheet", "promo"]},
    ],
    "Animation": [
        {"pack": "2D assets/New Platformer Pack", "kind": "vector",
         "match": r"^Vector/Enemies/.*\.svg$", "take": 60,
         "collection": "Project Echo", "tags": ["animation", "frame", "2d"]},
        {"pack": "2D assets/Fish Pack", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 70,
         "collection": "Project Echo", "tags": ["animation", "frame", "2d"]},
        {"pack": "3D assets/Animated Characters Protagonists", "kind": "file",
         "match": r"^Animations/[^/]+\.fbx$", "take": 3,
         "collection": "Project Mirror", "tags": ["animation", "rig", "clip"]},
        {"pack": "3D assets/Animated Characters Survivors", "kind": "file",
         "match": r"^Animations/[^/]+\.fbx$", "take": 3,
         "collection": "Project Mirror", "tags": ["animation", "rig", "clip"]},
        {"pack": "3D assets/Animated Characters Retro", "kind": "file",
         "match": r"^Animations/[^/]+\.fbx$", "take": 3,
         "collection": "Project Mirror", "tags": ["animation", "rig", "clip"]},
    ],
    "Tech Art": [
        {"pack": "3D assets/Prototype Kit", "kind": "file",
         "match": r"^Models/GLB format/.*\.glb$", "take": 60,
         "collection": "Engine Core", "tags": ["techart", "prototype", "greybox"]},
        {"pack": "2D assets/Prototype Textures", "kind": "file",
         "match": r"^PNG/.*\.png$", "take": 32,
         "collection": "Engine Core", "tags": ["techart", "texture", "greybox"]},
        {"pack": "2D assets/Prototype Textures", "kind": "vector",
         "match": r"^Vector/.*\.svg$", "take": 13,
         "collection": "Engine Core", "tags": ["techart", "prototype", "vector"]},
    ],
}

# Per-team target size AFTER the corrections above. Environment, Audio
# and Reference are untouched — they are already browsable, and inflating
# them would only re-create the imbalance in a new place. Every number is
# bounded by what kenney.nl actually publishes standalone: the "Animated
# Characters Bundle" would have carried Characters to 200 on its own and
# is excluded because its bytes are not re-fetchable (see
# kenney_pack_sources.NOT_PUBLISHED_STANDALONE). Re-fetchability beat
# roundness.
TEAM_TARGETS: dict[str, int] = {
    "UI": 200,
    "Props": 170,
    "VFX": 170,
    "Characters": 150,
    "Textures": 150,
    "Marketing Art": 120,
    "Animation": 120,
    "Tech Art": 120,
}

FLOOR = 60

# The share above which one team stops looking like emphasis and starts
# looking like the dataset. 11 teams, so an even split is 9.1%; an
# open-world shop legitimately leans on Environment, and ~2.5x the even
# share reads as a studio with a specialism. Above 25% it reads as one
# team plus some others.
MAX_TEAM_SHARE = 0.25

# Reviewers per team come from dataset.users.json; the owner is the
# team's own artist. Filled in at emit time from the committed catalogue
# so a changed roster cannot leave these dangling.


# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------

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


def slugify(text: str) -> str:
    return re.sub(r"-+", "-", re.sub(r"[^a-z0-9]+", "-", text.lower())).strip("-")


def png_dimensions(path: Path) -> tuple[int, int] | None:
    try:
        with path.open("rb") as f:
            head = f.read(26)
        if head[:8] != b"\x89PNG\r\n\x1a\n":
            return None
        return struct.unpack(">II", head[16:24])
    except (OSError, struct.error):
        return None


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


def dump(path: Path, data) -> None:
    path.write_text(json.dumps(data, indent=1, ensure_ascii=False) + "\n",
                    encoding="utf-8")


def title_from(rel: str) -> str:
    stem = rel.rsplit("/", 1)[-1].rsplit(".", 1)[0]
    stem = re.sub(r"(?<=[a-z0-9])(?=[A-Z])", " ", stem)
    stem = re.sub(r"[-_]+", " ", stem).strip()
    return stem[:1].upper() + stem[1:] if stem else "Untitled"


# --------------------------------------------------------------------------
# Selection
# --------------------------------------------------------------------------

def pack_files(pack_root: Path, pack: str) -> list[str]:
    """Every file under a bundle pack, pack-relative, in a stable order."""
    base = pack_root / pack
    out: list[str] = []
    for dirpath, dirnames, filenames in os.walk(base):
        dirnames.sort()
        for fn in sorted(filenames):
            out.append((Path(dirpath) / fn).relative_to(base).as_posix())
    return out


def select_for_rule(pack_root: Path, rule: dict, want: int,
                    taken: set[str]) -> list[str]:
    """Pack-relative paths a rule contributes, capped by rule['take'].

    Ordered by a hash of the path so the choice is stable and independent
    of directory iteration order — the same determinism rule the pool
    sampler follows (kenney_hq.select).
    """
    rx = re.compile(rule["match"])
    base = pack_root / rule["pack"]
    cands = []
    for rel in pack_files(pack_root, rule["pack"]):
        if not rx.match(rel):
            continue
        key = f"{rule['pack']}/{rel}"
        if key in taken or key in SOURCE_EXCLUSIONS:
            continue
        ext = rel.rsplit(".", 1)[-1].lower()
        if rule["kind"] == "file" and ext in ("png", "jpg", "jpeg"):
            # RULE 2 — dimensional gate, never bytes.
            dims = png_dimensions(base / rel)
            if not dims or max(dims) < MIN_BITMAP_EDGE:
                continue
        cands.append(rel)
    cands.sort(key=lambda r: hashlib.sha1(
        f"{rule['pack']}/{r}".encode()).hexdigest())
    return cands[:min(want, rule.get("take", want))]


# --------------------------------------------------------------------------
# Record construction
# --------------------------------------------------------------------------

PIPELINE_STAGES = ["Greybox", "Pass 1", "Polish", "Final", "Locked"]
WORKFLOW_STATES = ["draft", "in_review", "approved", "final"]
PLATFORM_SETS = [["PC"], ["PC", "Console"], ["Mobile"], ["PC", "Console", "Mobile"]]
ENGINES = ["Unreal 5", "Unity 2022", "Godot 4", "All"]


def field_values_for(asset_type: str, key: str, dims: tuple[int, int] | None,
                     size: int) -> dict:
    fv: dict = {
        "pipeline_stage": PIPELINE_STAGES[stable_int(5, "stage", key)],
        "version": f"v{1 + stable_int(3, 'ver', key)}",
        "revision_count": 1 + stable_int(6, "rev", key),
        "rating": 3 + stable_int(3, "rating", key),
        "engine_compatibility": ENGINES[stable_int(4, "engine", key)],
        "target_platforms": PLATFORM_SETS[stable_int(4, "plat", key)],
        "naming_compliant": stable_int(10, "naming", key) > 1,
    }
    if asset_type == "image":
        edge = max(dims) if dims else 512
        bucket = min([256, 512, 1024, 2048, 4096], key=lambda b: abs(b - edge))
        fv["texture_resolution"] = f"{bucket}x{bucket}"
        fv["color_space"] = "sRGB"
    elif asset_type == "3d":
        fv["polycount"] = 200 + stable_int(9800, "poly", key)
        fv["color_space"] = "Linear"
    elif asset_type == "audio":
        fv["loop_seconds"] = round(0.4 + stable_int(60, "loop", key) / 10, 1)
        fv["color_space"] = "N/A"
    return fv


def iso(day_seed: str) -> str:
    """A plausible timestamp inside the dataset's own window (2025-2026)."""
    day = stable_int(600, "day", day_seed)
    y, doy = (2025, day) if day < 300 else (2026, day - 300)
    month = 1 + doy // 26
    month = min(month, 12)
    dom = 1 + doy % 26
    hh = stable_int(24, "hh", day_seed)
    mm = stable_int(60, "mm", day_seed)
    return f"{y}-{month:02d}-{dom:02d}T{hh:02d}:{mm:02d}:00Z"


def build_record(team: str, rule: dict, rel: str, pack_root: Path,
                 source: dict, owners: list[str], reviewers: list[str],
                 render_px: int | None) -> tuple[dict, dict | None]:
    """One asset record, plus the pool entry when it needs rendering."""
    pack = rule["pack"]
    src = pack_root / pack / rel
    key = f"{pack}/{rel}"
    aid = stable_uuid("asset", "kenney-allin1", key)
    member_sha = sha256_file(src)

    if rule["kind"] == "vector":
        # kenney_hq naming: <category>-<pack>-<item>-<hash>-<px>.png
        from kenney_hq import name_for  # local import; same directory
        bundle_rel = f"{pack}/{rel}"
        pool_name = name_for(bundle_rel, render_px)
        file_path = f"images/kenney-hq/{pool_name}"
        source_root, source_path = HQ_SOURCE_ROOT, pool_name
        ext = "png"
        asset_type = "image"
        dims = (render_px, render_px)
        # Size is unknown until the pool is built; populate_archive
        # compares source vs dest size and copies on mismatch, and the
        # seeder reads the real size off disk. Recorded as 0 would look
        # like a truncated file, so use the SVG's own size as a
        # placeholder only if the render is absent.
        size = 0
        pool_entry = {"source": bundle_rel, "name": pool_name,
                      "kind": "vector", "render_px": render_px}
    else:
        ext = rel.rsplit(".", 1)[-1].lower()
        asset_type = EXT_TYPE.get(ext, "image")
        tdir = TYPE_DIR.get(asset_type, "images")
        dest_rel = "/".join(slugify(p) if i < len(rel.split("/")) - 1 else p
                            for i, p in enumerate(rel.split("/")))
        file_path = f"{tdir}/kenney-allin1/{slugify(pack)}/{dest_rel}"
        source_root, source_path = PACK_SOURCE_ROOT, f"{pack}/{rel}"
        dims = png_dimensions(src) if asset_type == "image" else None
        size = src.stat().st_size
        pool_entry = None

    owner = owners[stable_int(len(owners), "owner", key)] if owners else "seed.bot"
    reviewer = (reviewers[stable_int(len(reviewers), "rev", key)]
                if reviewers else None)
    created = iso(key)
    state = WORKFLOW_STATES[stable_int(4, "wf", key)]
    tier = ["team", "team", "public"][stable_int(3, "tier", key)]

    kind_word = {"image": "Raster", "3d": "Model", "audio": "Audio",
                 "font": "Font"}.get(asset_type, "Asset")
    title = title_from(rel) + (" (vector)" if rule["kind"] == "vector" else "")
    desc = (f"{kind_word} asset for {rule['collection']}, "
            f"{team.lower()} library. Source: Kenney '{pack.split('/')[-1]}' "
            f"(CC0).")

    rec = {
        "id": aid,
        "asset_type": asset_type,
        "title": title,
        "description": desc,
        "file_path": file_path,
        "source_root": source_root,
        "source_path": source_path,
        "file_extension": ext,
        "file_size_bytes": size,
        "sensitivity_tier": tier,
        "archive_state": "active" if state in ("approved", "final") else "draft",
        "owner_username": owner,
        "collection_name": rule["collection"],
        "team_name": team,
        "brand_workspace": ("Echo" if rule["collection"] == "Project Echo"
                            else "Mirror" if rule["collection"] in
                            ("Project Mirror", "Project Citylight",
                             "Project Compass") else None),
        "tags": list(rule["tags"]),
        "workflow_state": state,
        "metadata": {
            "filename": file_path.rsplit("/", 1)[-1],
            "kind": "vector" if rule["kind"] == "vector" else asset_type,
            "license": LICENSE,
            "usage_rights": "All Use",
            "acquisition_source": ACQUISITION_SOURCE,
            "attribution": ATTRIBUTION,
            "group_id": f"grp-bal-{aid[:8]}",
            # Provenance (#602 shape, archive-member variant — see the
            # module docstring and kenney_pack_sources.py).
            "fetched_from": source["page"],
            "source_archive": {
                "url": source["zip_url"],
                "member": rel,
                "sha256": member_sha,
            },
        },
        "field_values": field_values_for(asset_type, key, dims, size),
        "external_id": f"P4-{stable_int(90000, 'p4', key) + 10000}",
        "review_notes": None,
        "reviewer_username": reviewer,
        "created_at": created,
        "updated_at": created,
        "last_reviewed_at": None,
        "license": LICENSE,
        "attribution": ATTRIBUTION,
        "layer": "A",
        "studio": "a",
        "balance_source": key,
    }
    if rule["kind"] == "vector":
        rec["metadata"]["render"] = {"px": render_px,
                                     "tool": "seed/scripts/rasterize_svg.mjs"}
    return rec, pool_entry


# --------------------------------------------------------------------------
# Posts
# --------------------------------------------------------------------------

def build_posts(records: list[dict]) -> list[dict]:
    """Working-set posts over the new records.

    An asset nobody posted is invisible on browse (apply_upgrade asserts
    this), so posts are part of the deliverable, not a nicety. Clustered
    by (team, collection, asset_type) in the same shape
    sanitize_and_assemble's loose-asset pass produces.
    """
    clusters: dict[tuple, list[dict]] = {}
    for r in records:
        clusters.setdefault(
            (r["team_name"], r["collection_name"], r["asset_type"]), []
        ).append(r)

    posts: list[dict] = []
    for key in sorted(clusters):
        team, collection, atype = key
        members = sorted(clusters[key], key=lambda r: r["id"])
        i = 0
        n = 0
        while i < len(members):
            size = 3 + stable_int(3, "chunk", team, collection, atype, str(n))
            chunk = members[i:i + size]
            i += size
            n += 1
            anchor = chunk[0]
            pid = stable_uuid("post", "balance", anchor["id"])
            label = {"image": "art drop", "3d": "model drop",
                     "audio": "audio drop"}.get(atype, "drop")
            posts.append({
                "id": pid,
                "asset_ids": [c["id"] for c in chunk],
                "asset_types_in_post": [atype],
                "author_username": anchor["owner_username"],
                "brand_workspace": anchor["brand_workspace"],
                "collection_name": collection,
                "created_at": anchor["created_at"],
                "description": (
                    f"{team} working set for {collection}: {len(chunk)} "
                    f"{atype} assets from the CC0 Kenney library."),
                "is_mixed_type": False,
                "layer": "A",
                "post_kind": "asset_group",
                "sensitivity_tier": anchor["sensitivity_tier"],
                "studio": "a",
                "tags": sorted({t for c in chunk for t in c["tags"]}),
                "team_name": team,
                "title": f"{collection}: {team} {label} — {len(chunk)} assets",
                "updated_at": anchor["updated_at"],
                "workflow_state": anchor["workflow_state"],
            })
    return posts


# --------------------------------------------------------------------------
# Distribution
# --------------------------------------------------------------------------

def original_path(entry: dict) -> str:
    return entry.get("replaced_source_path") or entry.get("source_path") or ""


def apply_corrections(profile: list[dict]) -> dict[str, int]:
    """Move mis-teamed records. Returns {rule_index_desc: n}."""
    moved: dict[str, int] = {}
    for c in TEAM_CORRECTIONS:
        n = 0
        for e in profile:
            if e.get("team_name") != c["from"]:
                continue
            if c["match"] in original_path(e):
                e["team_name"] = c["to"]
                n += 1
        moved[f"{c['from']} -> {c['to']} ({c['match']})"] = n
    return moved


def distribution(profile: list[dict]) -> dict[str, int]:
    out: dict[str, int] = {}
    for e in profile:
        t = e.get("team_name") or "(none)"
        out[t] = out.get(t, 0) + 1
    return out


def print_table(before: dict[str, int], after: dict[str, int]) -> None:
    teams = sorted(set(before) | set(after),
                   key=lambda t: -after.get(t, 0))
    tb, ta = sum(before.values()), sum(after.values())
    print(f"{'team':<16}{'before':>8}{'%':>8}{'after':>8}{'%':>8}{'delta':>8}",
          file=sys.stderr)
    for t in teams:
        b, a = before.get(t, 0), after.get(t, 0)
        print(f"{t:<16}{b:>8}{100*b/tb:>7.1f}%{a:>8}{100*a/ta:>7.1f}%"
              f"{a-b:>+8}", file=sys.stderr)
    print(f"{'TOTAL':<16}{tb:>8}{'':>8}{ta:>8}", file=sys.stderr)


# --------------------------------------------------------------------------
# Commands
# --------------------------------------------------------------------------

def compute(pack_root: Path, profile: list[dict], render_px: int,
            users: list[dict]) -> tuple[list[dict], list[dict], dict]:
    """Corrections + additions. Returns (records, pool_entries, moved)."""
    moved = apply_corrections(profile)
    have = distribution(profile)

    by_team_owner: dict[str, list[str]] = {}
    by_team_rev: dict[str, list[str]] = {}
    for u in users:
        (by_team_rev if u["role"] == "reviewer" else by_team_owner) \
            .setdefault(u["primary_team"], []).append(u["username"])

    sources = {e["pack"]: e for e in load(UPGRADES / "kenney-pack-sources.json")}

    # Bundle files THIS SITE already shows are off limits. The pool names
    # a file by a hash of its source path, so re-selecting one produces a
    # second record pointing at the same `images/kenney-hq/<name>.png` —
    # two ids, one file. That is kenney_hq RULE 1's silent collision
    # arriving from the other direction: it validates cleanly right up
    # until someone counts.
    #
    # Scoped to THIS profile, not to the whole pool: the pool is shared
    # between the two sites, and a pool file site_b uses and site_a does
    # not is a perfectly good source for a site_a record — the studios
    # deliberately overlap (see the Engine Core split in seed/README.md).
    # Blocking the whole pool instead costs ~180 assets for no benefit.
    pool_by_name = {e["name"]: e["source"]
                    for e in load(UPGRADES / "kenney-hq-pool.json")["entries"]}
    taken: set[str] = set()
    for e in profile:
        fp = e.get("file_path") or ""
        if fp.startswith("images/kenney-hq/"):
            src = pool_by_name.get(fp.rsplit("/", 1)[-1])
            if src:
                taken.add(src)
    # Same for anything the LOCAL dataset tree already contributes. Two
    # of the recipe packs are also unpacked under `unpacked/kenney_*` in
    # the source tree, with the same layout below the pack root, so the
    # bundle copy of `Default/circle_a.png` is the identical picture to a
    # record site_a already ships. Adding it again would grow the counts
    # without growing the library — padding, which is the failure mode a
    # rebalance is most likely to reach for.
    for e in profile:
        op = original_path(e)
        for bundle_pack, local_dir in LOCAL_PACK_ALIASES.items():
            prefix = f"unpacked/{local_dir}/"
            if op.startswith(prefix):
                taken.add(f"{bundle_pack}/{op[len(prefix):]}")

    records: list[dict] = []
    pool_entries: list[dict] = []
    for team in sorted(TEAM_RECIPES, key=lambda t: -TEAM_TARGETS.get(t, 0)):
        target = TEAM_TARGETS.get(team, FLOOR)
        need = max(0, target - have.get(team, 0))
        for rule in TEAM_RECIPES[team]:
            if need <= 0:
                break
            src = sources.get(rule["pack"])
            if src is None:
                raise SystemExit(
                    f"FATAL: no recorded provenance for pack "
                    f"{rule['pack']!r}. Run:\n"
                    f"  python3 seed/scripts/kenney_pack_sources.py resolve "
                    f"--packs-from-recipes")
            chosen = select_for_rule(pack_root, rule, need, taken)
            for rel in chosen:
                taken.add(f"{rule['pack']}/{rel}")
                rec, pool = build_record(
                    team, rule, rel, pack_root, src,
                    by_team_owner.get(team) or by_team_owner.get("Reference", []),
                    by_team_rev.get(team, []), render_px)
                records.append(rec)
                if pool:
                    pool_entries.append(pool)
            need -= len(chosen)
        if need > 0:
            print(f"  WARNING: {team} short by {need} — recipes ran dry",
                  file=sys.stderr)
    return records, pool_entries, moved


def cmd_plan(args: argparse.Namespace) -> int:
    profile = load(args.profile)
    before = distribution(profile)
    users = load(PROFILES / "dataset.users.json")
    records, pool_entries, moved = compute(args.pack, profile, args.render_px,
                                           users)
    after = dict(distribution(profile))
    for r in records:
        after[r["team_name"]] = after.get(r["team_name"], 0) + 1

    print("\ncorrections (mis-teamed at source):", file=sys.stderr)
    for k, v in moved.items():
        print(f"  {v:5d}  {k}", file=sys.stderr)
    print(f"\nadditions: {len(records)} records "
          f"({len(pool_entries)} rendered vectors, "
          f"{len(records) - len(pool_entries)} copied from the bundle)",
          file=sys.stderr)
    by_type: dict[str, int] = {}
    for r in records:
        by_type[r["asset_type"]] = by_type.get(r["asset_type"], 0) + 1
    print(f"  by type: {by_type}", file=sys.stderr)
    print(file=sys.stderr)
    print_table(before, after)

    total = sum(after.values())
    problems = []
    for t, n in after.items():
        if n < FLOOR:
            problems.append(f"{t} at {n}, below the floor of {FLOOR}")
        if n / total > MAX_TEAM_SHARE:
            problems.append(f"{t} holds {100*n/total:.1f}% "
                            f"(> {100*MAX_TEAM_SHARE:.0f}%)")
    for p in problems:
        print(f"  PROBLEM: {p}", file=sys.stderr)
    return 1 if problems else 0


def cmd_emit(args: argparse.Namespace) -> int:
    profile = load(args.profile)
    users = load(PROFILES / "dataset.users.json")
    records, pool_entries, moved = compute(args.pack, profile, args.render_px,
                                           users)
    posts = build_posts(records)

    # Pool: append, never rewrite. The committed selection is a curatorial
    # decision (#604) and re-deriving it would drift the whole dataset.
    pool_doc = load(UPGRADES / "kenney-hq-pool.json")
    existing = {e["name"] for e in pool_doc["entries"]}
    added_pool = [e for e in pool_entries if e["name"] not in existing]
    pool_doc["entries"] = pool_doc["entries"] + added_pool
    names = [e["name"] for e in pool_doc["entries"]]
    if len(names) != len(set(names)):
        raise SystemExit("FATAL: pool name collision — see kenney_hq RULE 1")

    dests = [r["file_path"] for r in records]
    if len(dests) != len(set(dests)):
        dupes = sorted({d for d in dests if dests.count(d) > 1})
        raise SystemExit(f"FATAL: {len(dests)-len(set(dests))} added records "
                         f"share a file_path (e.g. {dupes[:2]}) — RULE 1")

    if args.dry_run:
        print(f"(dry run) {len(records)} assets, {len(posts)} posts, "
              f"{len(added_pool)} new pool entries", file=sys.stderr)
        return 0

    dump(UPGRADES / "kenney-hq-pool.json", pool_doc)
    dump(UPGRADES / f"balance-assets.{args.site}.json", records)
    dump(UPGRADES / f"balance-posts.{args.site}.json", posts)
    dump(UPGRADES / f"team-corrections.{args.site}.json", TEAM_CORRECTIONS)
    print(f"wrote {len(records)} assets, {len(posts)} posts, "
          f"{len(added_pool)} pool entries, "
          f"{len(TEAM_CORRECTIONS)} corrections", file=sys.stderr)
    for k, v in moved.items():
        print(f"  correction {v:5d}  {k}", file=sys.stderr)
    return 0


def cmd_sizes(args: argparse.Namespace) -> int:
    """Fill in file_size_bytes for the rendered records, from a built pool.

    A vector record's bytes do not exist until the pool is built, and the
    pool is built FROM the manifest this script writes — so the size can
    only be learned on a second pass. It is recorded rather than left at
    0 because the manifest is what ships as the site's MANIFEST.json, and
    a catalogue that reports every rendered image as 0 bytes is wrong in
    a way nothing else notices: the seeder takes the size off the real
    upload, so the DB stays right while the published manifest lies.
    """
    doc = UPGRADES / f"balance-assets.{args.site}.json"
    records = load(doc)
    n = miss = 0
    for r in records:
        if r.get("source_root") != HQ_SOURCE_ROOT:
            continue
        p = args.pool / r["source_path"]
        if not p.is_file():
            miss += 1
            continue
        size = p.stat().st_size
        if r.get("file_size_bytes") != size:
            r["file_size_bytes"] = size
            n += 1
    print(f"sizes: {n} record(s) updated, {miss} pool file(s) absent",
          file=sys.stderr)
    if miss:
        print("  build the pool first: python3 seed/scripts/kenney_hq.py "
              "build --pack <bundle> --out <pool>", file=sys.stderr)
        return 1
    dump(doc, records)
    print(f"wrote {doc}", file=sys.stderr)
    return 0


def cmd_check(args: argparse.Namespace) -> int:
    """Offline gate — committed docs vs the committed profile."""
    problems: list[str] = []
    assets_doc = UPGRADES / f"balance-assets.{args.site}.json"
    posts_doc = UPGRADES / f"balance-posts.{args.site}.json"
    if not assets_doc.is_file():
        print(f"missing {assets_doc}", file=sys.stderr)
        return 1
    records = load(assets_doc)
    posts = load(posts_doc)
    profile = load(args.profile)

    dist = distribution(profile)
    total = sum(dist.values())
    for t, n in sorted(dist.items()):
        if n < FLOOR:
            problems.append(f"{t} at {n}, below the floor of {FLOOR}")
        if n / total > MAX_TEAM_SHARE:
            problems.append(f"{t} holds {100*n/total:.1f}% of {total}")

    ids = {e["id"] for e in profile}
    absent = [r["id"] for r in records if r["id"] not in ids]
    if absent:
        problems.append(f"{len(absent)} balance records are not in the "
                        f"profile — run apply_upgrade.py")
    posted = {a for p in posts for a in p["asset_ids"]}
    orphans = [r["id"] for r in records if r["id"] not in posted]
    if orphans:
        problems.append(f"{len(orphans)} balance records have no post")

    for r in records:
        m = r.get("metadata") or {}
        sa = m.get("source_archive") or {}
        if not m.get("fetched_from"):
            problems.append(f"{r['id']}: no fetched_from")
            break
        if not (sa.get("url") and sa.get("member") and sa.get("sha256")):
            problems.append(f"{r['id']}: incomplete source_archive")
            break

    for p in problems:
        print(f"  PROBLEM: {p}", file=sys.stderr)
    print(f"{len(records)} balance records, {len(posts)} posts, "
          f"{len(problems)} problem(s)", file=sys.stderr)
    return 1 if problems else 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("plan", "emit", "sizes", "check"):
        p = sub.add_parser(name)
        p.add_argument("--site", default="site_a", choices=("site_a", "site_b"))
        p.add_argument("--profile", type=Path,
                       default=PROFILES / "studio-a.assets.json")
        if name in ("plan", "emit"):
            p.add_argument("--pack", required=True, type=Path,
                           help="Kenney All-in-1 bundle root (read-only)")
            p.add_argument("--render-px", type=int, default=DEFAULT_RENDER_PX)
        if name == "emit":
            p.add_argument("--dry-run", action="store_true")
        if name == "sizes":
            p.add_argument("--pool", required=True, type=Path,
                           help="built kenney-hq pool directory")
    args = ap.parse_args()

    if getattr(args, "pack", None) is not None and not args.pack.is_dir():
        print(f"error: --pack not a directory: {args.pack}\n"
              "If this is on the archive share, the mount may have dropped — "
              "that reads as 'No such file or directory'. Check `mountpoint` "
              "and remount before assuming the data is gone.", file=sys.stderr)
        return 2

    if args.cmd == "plan":
        return cmd_plan(args)
    if args.cmd == "emit":
        return cmd_emit(args)
    if args.cmd == "sizes":
        return cmd_sizes(args)
    return cmd_check(args)


if __name__ == "__main__":
    raise SystemExit(main())
