#!/usr/bin/env python3
"""
Sanitize the existing artist-alley_dataset metadata.csv into AA-schema-shaped
profile JSONs, split between studio-a (Mirror Studios) and studio-b
(Adventureworks) for federation dogfood, plus a unified `dev` profile for
solo-developer re-seeds.

Inputs
------
- metadata.csv (12,871 rows, 41 cols) from the source dataset directory
- groups.csv (asset sibling linkage)
- The on-disk asset tree (used only to verify path existence + size)

Outputs (under seed/profiles/)
------------------------------
- dataset.MANIFEST.json      catalogue index: per-asset row trimmed to AA shape
- dataset.users.json         30 fictional artists + approvers as user records
- dataset.teams.json         11 teams
- dataset.collections.json   13 projects mapped to collections
- dataset.brand_workspaces.json  Echo + Mirror (the 2 promoted franchises)
- dataset.field_definitions.json 12 custom field definitions
- dataset.workflow.json      5 workflow states + standard transitions
- studio-a.assets.json       Mirror Studios' asset subset (~5 GB)
- studio-b.assets.json       Adventureworks' asset subset (~5 GB)
- dev.assets.json            Unified set (full 10 GB) for solo dev re-seed
- demo.assets.json           Public-safe subset (Layer A only, ~2 GB) for the
                             Phase 1.48 demo sandbox seed pack

Schema mapping (CSV column → AA target)
---------------------------------------
See seed/README.md §"Schema mapping" for the full table. Summary:
- asset_id (ast-XX)      → assets.id (UUID gen) + metadata.external_id
- group_id (grp-NNNNN)   → asset_companions (siblings) + post.id
- kind                   → assets.asset_type (raster/vector→image, ...)
- team                   → teams.name + team_memberships
- project                → collections.name (1 collection per project)
- franchise              → Echo/Mirror = brand_workspace; rest = tags
- artist/approver        → users.username + ownership/review
- status                 → workflow_states (Approved/Final/InReview/Draft/Archived)
- confidentiality        → assets.sensitivity_tier (Public/Internal/Restricted)
- pipeline_stage, version, revision_count, rating, polycount,
  texture_resolution, color_space, loop_seconds, runtime_seconds,
  engine_compatibility, target_platforms, naming_compliant, external_id
                         → asset_field_value via custom field_definitions

Studio split (by project)
-------------------------
- Studio A (Mirror Studios):
    Project Mirror, Project Echo, Project Citylight, Project Compass,
    Engine Core (Mirror subset), Art Research, Snapdex Pokemon (subset)
    Owns Mirror + Echo brand workspaces.
- Studio B (Adventureworks):
    Project Heroes, Project Zoo, Project Adventure, Project Jumpstart,
    Project Toybox, Engine Core (Adventureworks subset),
    Hearthstone, MTG (subset), Overwatch Heroes (subset)
- Intentional overlap on Engine Core with different subsets per studio
  to exercise CAS content-hash dedup across federation.
- Studio Library (personal media + reference) is split based on
  filename keyword heuristics — distributed roughly evenly.

10 GB cap is enforced by per-pack sampling (see TRIMS section below).

Usage
-----
    python3 sanitize_and_assemble.py \\
        --source /mnt/d/Projects/unraid_management/artist-alley_dataset \\
        --out    /mnt/d/Projects/artist-alley/seed/profiles \\
        [--dry-run]

Idempotent. Re-running with the same inputs produces the same outputs.
"""

from __future__ import annotations

import argparse
import csv
import dataclasses
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
from collections import defaultdict
from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

# -----------------------------------------------------------------------------
# Studio split — by project
# -----------------------------------------------------------------------------

# site_a is dual-purpose: (1) Studio A in federation dogfood, (2) source for
# the Phase 1.48 public demo sandboxes. That second purpose forces site_a
# to be Layer A only (CC0 / CC-BY / OFL / public-domain) — no game-rip
# references, no TCG IPs, no personal photos, no proprietary fonts.
#
# So Studio A's projects are limited to ORIGINAL work that's safe to ship
# publicly. Reference material (Art Research, Snapdex) moves to Studio B.
STUDIO_A_PROJECTS = {
    "Project Mirror",
    "Project Echo",
    "Project Citylight",
    "Project Compass",
}

# Studio B is local-only: dogfood peer + your dev re-seed source. Carries
# the IP-referenced and personal content (game rips, TCG cards, comics,
# personal photos). Never shipped publicly.
STUDIO_B_PROJECTS = {
    "Project Heroes",
    "Project Zoo",
    "Project Adventure",
    "Project Jumpstart",
    "Project Toybox",
    "Hearthstone Archive",
    "MTG Archive",
    "Heroes Archive",
    "Art Research",     # moved from A — game-rip references are Layer B
    "Snapdex",          # moved from A — Pokemon IP, Layer B
}

# Engine Core is split — each studio takes a subset. Studio Library is split
# by filename heuristic in `_split_engine_or_library`.
SPLIT_PROJECTS = {"Engine Core", "Studio Library"}

# Shared pack patterns — assets whose file_path matches any of these flow
# to BOTH site_a and site_b as byte-identical duplicates. This is what
# exercises CAS content-hash dedup across federation: same SHA-256 on both
# instances, recognized as the same content when shared.
#
# CRITICAL: every pattern here must be Layer A (CC0 / CC-BY / OFL /
# public domain). Shared content lands on site_a, which is the public
# demo source — proprietary fonts ("Licensed from type foundry") and
# unknown-license assets MUST stay out of this set.
#
# Removed from a prior iteration of this set:
#   BOOKmanOpti-Bold/             "Licensed from type foundry" — proprietary
#   cheltenham-condensed-bold_*/  license unclear
#   Ultrawide/                    license unclear
SHARED_PACK_PATTERNS: list[str] = [
    "unpacked/kenney_prototype-textures",   # Kenney CC0
    "unpacked/kenney_development-essentials",  # Kenney CC0
    "unpacked/kenney_kenney-fonts",         # Kenney CC0
    "unpacked/kenney_rpg-audio",            # Kenney CC0
    "epub/",                                # Project Gutenberg PD
    "Playwrite_DE_Grund/",                  # Google Fonts OFL
    "Sono/",                                # Google Fonts OFL
    "Wellfleet/",                           # Google Fonts OFL
]

# Brand workspaces: only Echo + Mirror become full workspaces (per ADR 0025);
# the rest are tags only.
BRAND_WORKSPACE_FRANCHISES = {"Echo", "Mirror"}

# Layer A — public-safe sources that can ship in the Phase 1.48 demo sandbox
# seed pack. Everything else is Layer B (local-only: dogfood + dev re-seed).
LAYER_A_SOURCES = {
    "Kenney.nl",
    "PixelSpaces",
    "Google Fonts",
    "Open Font License",
    "Project Gutenberg",
    "NASA",
    "Blender Foundation",
    "Polyhaven",
    "Khronos Sample Models",
    "UISketch",                   # ML dataset; CC0
}

# Sampling caps applied to reach the ~10 GB target. Tune these per-pack.
TRIMS: dict[str, int] = {
    # source-pack-name → max rows kept
    "Snapdex (TCG)": 50,
    "hearthstone": 100,           # group cap (× ~3-4 size variants each)
    "mtg_img_archive": 150,
    "heroes": 30,                 # 3 heroes × 10 portraits
    "UISketch": 200,
    "Dresden Files Comics": 6,    # comic-viewer coverage; ~600 MB total
}

# Hard drops by path substring — files that blow the budget single-handedly
# without adding format-coverage value. Order doesn't matter; any match drops.
HARD_DROP_PATH_PATTERNS: list[str] = [
    "Ramen Cooking Class",        # ~4.55 GB single video; Sintel + BBB clips
                                  # (fetched from internet) cover the video
                                  # pipeline at <100 MB combined
    ".LRV",                       # GoPro low-res variants; we keep the high-res
]

# Per-file size cap. Any single file larger than this is dropped unless its
# path matches a whitelist entry. 500 MB keeps the GoPro / Castle Rock /
# MariCart / FarmerWasReplaced clips that legitimately exercise the video
# + animation pipelines without inheriting the 4.5 GB Ramen outlier.
MAX_FILE_SIZE_BYTES = 500 * 1024 * 1024  # 500 MB

# Whitelist for files that legitimately need to be larger than the cap.
LARGE_FILE_WHITELIST: list[str] = [
    # Add when we have specific large files we want to keep.
]

# Per-asset-type budget caps — applied AFTER pack trims. Enforces format-
# coverage balance: keeps a representative sample of each viewer kind without
# letting image-heavy Kenney packs dominate the demo experience.
#
# Caps are global (across all studios). Shared assets count toward each
# studio's profile, so the per-site count = unique-to-site + shared.
ASSET_TYPE_CAPS: dict[str, int] = {
    "image": 1200,
    "audio": 350,
    "3d": 400,
    "video": 25,
    "comic": 10,
    "document": 100,
    "font": 60,
}

# Asset bytes are reorganized by type on the destination archive. This map
# turns the AA `asset_type` into the destination top-level folder. Original
# pack structure is preserved under the type folder so attribution + source
# context isn't lost.
#
#   unpacked/kenney_animal-pack-remastered/PNG/Pancakes/lion-walk1.png
#                    ↓
#   images/kenney_animal-pack-remastered/PNG/Pancakes/lion-walk1.png
TYPE_FOLDER_MAP: dict[str, str] = {
    "image": "images",
    "audio": "audio",
    "3d": "3d",
    "video": "videos",       # plural — dodges a stale SMB cache entry on
                             # /mnt/blackbox_archives/.../site_b/video that
                             # showed up as a ghost dir after a failed run
    "document": "documents",
    "font": "fonts",
    "comic": "comics",
}

# When the source path lives under "unpacked/" (Kenney packs), strip that
# prefix on reorganization — it's noise from how the source dataset was
# organized, not meaningful attribution.
STRIPPABLE_PREFIXES: list[str] = [
    "unpacked/",
]


def reorganize_path(asset_type: str, original_path: str) -> str:
    """Compute the destination path under the typed-folder layout.

    Drops any STRIPPABLE_PREFIXES from the start of the original path, then
    prepends the asset_type's destination folder. Internet-fetched content
    follows the same shape with its own subdirectories.
    """
    cleaned = original_path
    for prefix in STRIPPABLE_PREFIXES:
        if cleaned.startswith(prefix):
            cleaned = cleaned[len(prefix):]
            break
    type_folder = TYPE_FOLDER_MAP.get(asset_type, asset_type)
    return f"{type_folder}/{cleaned}"

# Project-level row caps applied AFTER per-pack TRIMS. Keeps Studio Library
# (personal photos + reference docs + comics) from dominating the seed.
PROJECT_ROW_CAPS: dict[str, int] = {
    "Studio Library": 60,         # was 175; keep a representative sample of
                                  # personal media + comics + reference docs
}

# -----------------------------------------------------------------------------
# Schema mapping — CSV column shapes → AA targets
# -----------------------------------------------------------------------------

# CSV `kind` → AA asset_type. Vector → image (we have one image viewer that
# handles raster + SVG); design-source folds into image too. 3D / audio /
# video / font / document / comic map 1:1.
KIND_TO_ASSET_TYPE = {
    "raster": "image",
    "vector": "image",
    "design-source": "image",
    "3d": "3d",
    "audio": "audio",
    "video": "video",
    "font": "font",
    "document": "document",
    "comic": "comic",
}

# CSV `confidentiality` → AA sensitivity_tier per ADR 0020.
CONFIDENTIALITY_TO_TIER = {
    "Public": "public",
    "Internal": "team",
    "Restricted": "restricted",
}

# CSV `status` → workflow_state. The 5 states match the seeded workflow.
STATUS_TO_WORKFLOW_STATE = {
    "Draft": "draft",
    "In Review": "in_review",
    "Approved": "approved",
    "Final": "final",
    "Archived": "archived",
}

# Standard 5-state workflow seeded per-deployment.
WORKFLOW_STATES = [
    {"name": "draft", "label": "Draft", "color": "#6b7280", "order": 0},
    {"name": "in_review", "label": "In Review", "color": "#f59e0b", "order": 1},
    {"name": "approved", "label": "Approved", "color": "#10b981", "order": 2},
    {"name": "final", "label": "Final", "color": "#2563eb", "order": 3},
    {"name": "archived", "label": "Archived", "color": "#9ca3af", "order": 4},
]

WORKFLOW_TRANSITIONS = [
    ("draft", "in_review"),
    ("in_review", "approved"),
    ("in_review", "draft"),
    ("approved", "final"),
    ("final", "archived"),
    ("approved", "archived"),
]

# Custom metadata field definitions per ADR 0012.
FIELD_DEFINITIONS = [
    {"name": "pipeline_stage", "label": "Pipeline stage", "type": "select",
     "options": ["Greybox", "Pass 1", "Polish", "Final", "Locked"]},
    {"name": "version", "label": "Version", "type": "text"},
    {"name": "revision_count", "label": "Revisions", "type": "number"},
    {"name": "rating", "label": "Rating", "type": "number"},
    {"name": "polycount", "label": "Polycount", "type": "number"},
    {"name": "texture_resolution", "label": "Texture resolution", "type": "select",
     "options": ["256x256", "512x512", "1024x1024", "2048x2048", "4096x4096"]},
    {"name": "color_space", "label": "Color space", "type": "select",
     "options": ["sRGB", "Linear", "Raw", "N/A"]},
    {"name": "loop_seconds", "label": "Loop length (s)", "type": "number"},
    {"name": "runtime_seconds", "label": "Runtime (s)", "type": "number"},
    {"name": "engine_compatibility", "label": "Engine", "type": "select",
     "options": ["Unreal 5", "Unity 2022", "Godot 4", "All", "N/A"]},
    {"name": "target_platforms", "label": "Target platforms", "type": "multi_select",
     "options": ["PC", "Console", "Mobile", "All"]},
    {"name": "naming_compliant", "label": "Naming compliant", "type": "boolean"},
]

# -----------------------------------------------------------------------------
# Deterministic UUIDs — same input → same UUID, so re-running is stable
# -----------------------------------------------------------------------------

_NAMESPACE_SEED = "artist-alley.seed.v1"

def stable_uuid(*parts: str) -> str:
    """UUID-shaped string derived from the seed namespace + parts. Stable
    across runs so the seeded instance has consistent IDs."""
    h = hashlib.sha256()
    h.update(_NAMESPACE_SEED.encode())
    for p in parts:
        h.update(b"\x00")
        h.update(str(p).encode())
    d = h.hexdigest()
    return f"{d[0:8]}-{d[8:12]}-{d[12:16]}-{d[16:20]}-{d[20:32]}"


# -----------------------------------------------------------------------------
# Post identity (#1293)
# -----------------------------------------------------------------------------
#
# ADR 0098: "A derived id must be a function of what distinguishes the
# thing it names. An input that merely ACCOMPANIES the thing — an anchor,
# a sample's extremum, a dominant value — does not identify it and must
# not be the whole key."
#
# The three narrative passes built their id from the sample's ANCHOR: the
# most-recent member. A team's genuinely most-recent asset lands in many
# samples, so several roundups — different membership, different titles —
# derived the same id:
#
#     studio-a.posts.json   873 rows under 861 ids
#     studio-b.posts.json   771 rows under 767 ids
#
# and every colliding pair disagreed: "— 7 drops" beside "— 5 drops"
# beside "— 8 drops" under one id.
#
# ⛔ The consequence is a DISAPPEARANCE, not a duplicate. `aa seed` keys
# on the stable id, so of n colliding rows exactly one survives and the
# rest can never be seeded.
#
# What distinguishes one roundup from another is its MEMBERSHIP, so
# membership is the key. `label` and `reel_label` stay in front of it
# because they are not redundant: a project holding exactly five assets
# yields the same five-member sample on every sweep, and only the label
# tells those posts apart.
#
# ⭐ These live at module level so `migrate_post_ids.py` calls the SAME
# function the assembler does. A migration that reimplements the key is a
# second definition of identity, and the two drift.

def roundup_post_id(team_name: str, asset_ids: Iterable[str]) -> str:
    return stable_uuid("post", "roundup", team_name, *sorted(asset_ids))


def sprint_post_id(project_name: str, label: str, asset_ids: Iterable[str]) -> str:
    return stable_uuid("post", "sprint", project_name, label, *sorted(asset_ids))


def showreel_post_id(studio_key: str, reel_label: str,
                     asset_ids: Iterable[str]) -> str:
    return stable_uuid("post", "showreel", studio_key, reel_label,
                       *sorted(asset_ids))


SPRINT_LABELS = ("sprint 12", "sprint 13", "sprint 14", "milestone alpha",
                 "milestone beta", "review session", "lock-in pass",
                 "polish week", "final review", "ship gate")
REEL_LABELS = ("Q3 reel", "Q4 reel")


def stable_int(n: int, *parts: str) -> int:
    """Deterministic int in [0, n) from the same namespace as stable_uuid.
    Used for per-item flavour picks so composition never depends on RNG
    call ORDER — a seeded Random() re-sequences every downstream draw when
    an earlier pass changes, which silently rewrites unrelated posts."""
    if n <= 0:
        return 0
    h = hashlib.sha256()
    h.update(_NAMESPACE_SEED.encode())
    for p in parts:
        h.update(b"\x00")
        h.update(str(p).encode())
    return int.from_bytes(h.digest()[:8], "big") % n


# -----------------------------------------------------------------------------
# Post titles (#1306)
# -----------------------------------------------------------------------------
#
# ⛔ NO EM DASHES, AND NO GENERATED COUNTS.
#
# Measured on the shipped `studio-a.posts.json`: 863 posts, 781 of them
# (90%) containing an em dash and 580 (67%) ending in a count the
# generator produced — "— 4 assets", "— 6 drops", "— 2-part set". Nobody
# writes a title that way, and the count is already on the card:
# `PostCard.svelte` renders `CardKindBadge count={memberCount}` right
# beside it, so the suffix repeated chrome sitting an inch away.
#
# ⛔ THE COUNT WAS ALSO LOAD-BEARING, WHICH IS WHY THIS IS NOT A DELETION.
# Removing it outright collapses 128 studio-a titles into 44 groups and
# 111 studio-b titles into 44 — three separate "Project Echo: Audio audio
# bundle" posts, three "VFX sprint roundup" posts, and so on, because the
# number was the only thing telling one chunk from the next. Each of
# those templates therefore gains the LEAD ASSET instead, which is both
# what a person would actually name the post after and a stronger
# discriminator than a count: chunks are disjoint, so their leads differ
# even when their sizes collide.
#
# Every function here is shared by the generator and by the in-place
# retitle pass, so there is one definition of each title rather than a
# template and a migration that can drift apart.

# ⚠️ NEITHER OF THESE CLAIMS COMPLETENESS, deliberately. "…, the full
# set" reads as a contradiction the moment `disambiguate_titles` has to
# append "part 2" to it, and it did: two site_b groups share an anchor
# title. The generator's own descriptions already call these "variants
# and siblings kept together" and "assets across N types, delivered
# together", so the titles say that instead.
def title_group_set(anchor_title: str) -> str:
    return f"{anchor_title} and variants"


def title_group_bundle(anchor_title: str) -> str:
    return f"{anchor_title} and siblings"


def title_collection_chunk(collection: str, team: str, theme: str) -> str:
    return f"{collection}: {team} {theme}"


def title_solo(flavor: str, asset_title: str) -> str:
    return f"{flavor[:1].upper()}{flavor[1:]}: {asset_title}"


def title_team_roundup(team_name: str) -> str:
    return f"{team_name} sprint roundup"


def title_project_sprint(project_name: str, label: str) -> str:
    return f"{project_name} {label}"


def title_showreel(reel_label: str) -> str:
    return f"Cinematics {reel_label}"


# ⛔ THE INVERSES BELONG BESIDE THE FORMATTERS (#1306).
#
# `migrate_post_ids.derived_id` recovers a sprint `label` and a
# `reel_label` FROM THE TITLE, because neither is stored anywhere else on
# the post, and feeds them straight into `sprint_post_id` /
# `showreel_post_id`. So a title format change is an identity concern for
# exactly two post kinds, and #1306 broke the parse the moment it
# retired the em dash — caught by `migrate_post_ids.py --check`, which
# the guard suite runs.
#
# ⚠️ The ids do NOT move: the label recovered is the same label, so the
# derivation's inputs are unchanged. What moved was the parser's ability
# to find it. Keeping the inverse next to the formatter is what stops the
# next title edit from being a silent id migration instead of a loud
# parse failure.

SOLO_FLAVORS = {
    "image": ["new render", "color study", "lighting pass", "reference plate", "concept sketch"],
    "3d": ["model drop", "topology pass", "PBR test", "WIP turntable", "asset ship"],
    "audio": ["audio drop", "sfx test", "ambient bed", "score sketch", "VO take"],
    "video": ["cinematic test", "edit pass", "previs sweep", "playblast", "shot reference"],
    "document": ["briefing", "style guide", "pipeline note", "research doc", "spec draft"],
    "font": ["type sample", "kerning check", "char set test", "weight comparison"],
    "comic": ["storyboard panel", "page rough", "layout pass"],
}

# Flat, for the inverse: a solo title is "{asset} \u2014 {flavor}" and the
# asset title can hold a dash of its own, so the only safe way to find
# the split is to know what the flavours ARE.
ALL_SOLO_FLAVORS = frozenset(
    f for flavors in SOLO_FLAVORS.values() for f in flavors) | {"asset drop"}

# The generated suffixes every retired template ended in. A closed
# vocabulary: anything outside it is REPORTED rather than guessed at.
_SUFFIX_RE = re.compile(
    r" \u2014 \d+[- ](?P<what>part set|asset bundle|assets|drops|cuts)$")
_SPRINT_SUFFIX_RE = re.compile(r" \u2014 \d+ assets across \d+ team\(s\)$")

_PART_SUFFIX = re.compile(r", part \d+$")


def strip_part_suffix(title: str) -> str:
    """Remove the `, part N` a title may have gained from
    `disambiguate_titles`. Idempotent, and a no-op on a title that never
    collided."""
    return _PART_SUFFIX.sub("", title)


def sprint_label_from_title(project_name: str, title: str) -> str | None:
    """Inverse of `title_project_sprint`. None when it does not match."""
    head = strip_part_suffix(title)
    prefix = f"{project_name} "
    if not project_name or not head.startswith(prefix):
        return None
    return head[len(prefix):]


def reel_label_from_title(title: str) -> str | None:
    """Inverse of `title_showreel`. None when it does not match."""
    head = strip_part_suffix(title)
    prefix = "Cinematics "
    if not head.startswith(prefix):
        return None
    return head[len(prefix):]


# Keyed by the post_kind the generator writes, so the retitle pass and
# the generator cannot disagree about which wording belongs to which post.
VIDEO_TITLES = {
    "dailies": "Dailies on {title}",
    "cinematic_cut": "Cinematic cut of {title}",
}

REVISION_TITLES = {
    "draft": "First draft of {title}",
    "review": "Review pass on {title}",
    "ship": "Signing off on {title}",
}


def clean_dashes(text: str) -> str:
    """Drop the em dash from a title without dropping what it separated.

    43 ASSET titles carry one — "Moby Dick — Herman Melville",
    "DamagedHelmet — Khronos PBR test" — and they flow straight into the
    solo, revision and video post titles, so a post-template fix alone
    still leaves 37 site_a titles with a dash in them. In every one of
    the 43 the dash separates a name from a qualifier, which is what a
    comma does in a catalogue: the meaning survives and the tell does not.
    """
    # ⚠️ THE SURROUNDING SPACES COME OFF FIRST. Replacing the character
    # before the spaces leaves "Big Buck Bunny , 720p surround", which is
    # a worse tell than the dash was.
    return (text.replace(" \u2014 ", ", ")
                .replace("\u2014 ", ", ")
                .replace(" \u2014", ", ")
                .replace("\u2014", ", "))


def disambiguate_titles(posts: list[dict]) -> int:
    """Number the posts that would otherwise share a title.

    ⛔ THIS IS WHAT MAKES DROPPING THE COUNTS SAFE. The generated count
    was load-bearing: 128 studio-a titles and 111 studio-b titles
    collapse into 44 groups each without it, because a chunk's size was
    the only thing telling it from the next chunk of the same collection
    and team.

    So the count is replaced rather than deleted, by the thing a person
    reaches for instead: "part two". It is shorter than a byte count, it
    does not repeat the `CardKindBadge count={memberCount}` sitting
    beside it on the card, and it is applied to the FINISHED document —
    the same function on the generator's output and on the committed one,
    so the two cannot disagree about which post is part two.

    Ordering is by post id, which is derived from membership and is
    therefore stable across a re-assembly; iteration order is not.
    """
    by_title: dict[str, list[dict]] = {}
    for p in posts:
        by_title.setdefault(p.get("title") or "", []).append(p)
    renamed = 0
    for title, group in by_title.items():
        if len(group) < 2:
            continue
        # ⚠️ EVERY member is numbered, including the first. Leaving one
        # bare reads as an oversight beside fourteen numbered siblings,
        # and site_a really does have a fifteen-chunk family.
        for n, post in enumerate(sorted(group, key=lambda x: x["id"]), start=1):
            post["title"] = f"{title}, part {n}"
            renamed += 1
    return renamed


def retitle_posts(posts: list[dict]) -> tuple[int, list[str]]:
    """Re-title an ALREADY COMPOSED posts document in place (#1306).

    Returns (changed, problems).

    ⚠️ WHY THIS EXISTS RATHER THAN A RECOMPOSE. The shipped
    `studio-a.posts.json` is not this module's output: a recompose emits
    1,103 posts where the committed file holds 863, and `apply_upgrade`
    already records that the two disagree on `created_at` for 840 of 861
    shared ids. That divergence is its own issue (#1309); resolving it
    here would hide a 240-post change inside a copy edit. So the
    generator's wording is fixed above and applied to the existing
    composition here, leaving membership, ids and timestamps untouched.

    Every wording comes from the same helper the generator calls, so the
    two cannot drift.
    """
    changed = 0
    problems: list[str] = []
    for post in posts:
        kind = post.get("post_kind") or ""
        old = post.get("title") or ""
        coll = post.get("collection_name") or ""
        team = post.get("team_name") or ""
        new = None

        # ⛔ DISPATCH ON THE TITLE'S OWN SHAPE, NOT ON `post_kind`.
        # The two disagree in the committed data: 228 posts carry
        # `post_kind: "asset_group"` while their title was written by the
        # collection-chunk template ("Project Echo: Animation art drop
        # — 4 assets"). Trusting the kind turned those into "..., the
        # full set, part 15", which is both wrong about the post and
        # nonsense to read. The suffix is what the generator actually
        # produced, so it is what the inverse reads.
        m = _SUFFIX_RE.search(old)
        sprint = _SPRINT_SUFFIX_RE.search(old)
        if m:
            head = old[:m.start()]
            what = m.group("what")
            if what == "part set":
                new = title_group_set(head)
            elif what == "asset bundle":
                new = title_group_bundle(head)
            elif what == "drops":
                new = title_team_roundup(
                    head[:-len(" sprint roundup")]
                    if head.endswith(" sprint roundup") else head)
            elif what == "cuts":
                label = reel_label_from_title(head)
                new = title_showreel(label) if label is not None else head
            elif what == "assets":
                prefix = f"{coll}: {team} "
                if coll and team and head.startswith(prefix):
                    new = title_collection_chunk(coll, team,
                                                 head[len(prefix):])
                else:
                    new = head
        elif sprint:
            head = old[:sprint.start()]
            label = sprint_label_from_title(coll, head)
            new = (title_project_sprint(coll, label)
                   if label is not None else head)
        elif kind == "solo_showcase":
            # The flavour is a SUFFIX, so this splits on the LAST dash:
            # an asset title carrying a dash of its own would otherwise
            # be cut in half. Matched against the known flavours rather
            # than "whatever follows the dash", so an asset title that
            # merely looks like one cannot be mistaken for it.
            head, sep, tail = old.rpartition(" — ")
            if sep and tail in ALL_SOLO_FLAVORS:
                new = title_solo(tail, head)
        else:
            # The prefix templates: their fixed wording comes FIRST, so
            # they split on the first dash rather than the last.
            pre_head, pre_sep, pre_tail = old.partition(" — ")
            if pre_sep and kind.startswith("video_"):
                tmpl = VIDEO_TITLES.get(kind[len("video_"):])
                if tmpl:
                    new = tmpl.format(title=pre_tail)
            elif pre_sep and kind.startswith("revision_"):
                tmpl = REVISION_TITLES.get(kind[len("revision_"):])
                if tmpl:
                    new = tmpl.format(title=pre_tail)

        if new is None:
            # ⛔ A post this pass cannot name is REPORTED, never left
            # to be noticed later on the wall. Silently keeping the old
            # title is how "zero em dashes" becomes a claim about the
            # posts the pass happened to understand.
            if "—" in old:
                problems.append(
                    f"{kind or '(no kind)'}: no template matched {old!r}; "
                    "it falls through to the dash clean below, which is "
                    "correct for a hand-written title but would hide a "
                    "template this pass has stopped recognising")
            continue
        if new != old:
            post["title"] = new
            changed += 1

    # The asset titles the templates embed carried 43 em dashes of their
    # own, so a template-only fix still leaves 37 site_a post titles with
    # one. Applied after the per-kind pass, and to EVERY title, so a post
    # this pass could not name still comes out clean.
    for post in posts:
        cleaned = clean_dashes(post.get("title") or "")
        if cleaned != post.get("title"):
            post["title"] = cleaned
            changed += 1

    # LAST, on the finished document (see disambiguate_titles).
    disambiguate_titles(posts)
    return changed, problems


# -----------------------------------------------------------------------------
# Row transform — CSV → AA-shaped dict
# -----------------------------------------------------------------------------

@dataclass
class AssetRecord:
    id: str
    asset_type: str
    title: str
    description: str
    file_path: str        # destination path under the typed-folder layout
    source_path: str      # where the bytes live in the source dataset
                          # (for populate_archive to copy from). For
                          # internet-fetched, points at the local cache.
    source_root: str      # 'local' or 'internet' — selects which root
                          # populate_archive resolves source_path against
    file_extension: str
    file_size_bytes: int
    sensitivity_tier: str
    archive_state: str
    owner_username: str
    collection_name: str
    team_name: str
    brand_workspace: str | None
    tags: list[str]
    workflow_state: str
    metadata: dict[str, Any]
    field_values: dict[str, Any]
    external_id: str
    review_notes: str | None
    reviewer_username: str | None
    created_at: str
    updated_at: str
    last_reviewed_at: str | None
    license: str
    attribution: str
    layer: str  # 'A' (public-safe) or 'B' (local-only)
    studio: str  # 'a', 'b', or 'shared'
    # The ADR 0090 content RATING, and a SECOND AXIS beside
    # `sensitivity_tier` above rather than a value inside it (#1217): a
    # public work can be mature and a restricted one need not be.
    #
    # Always False here, and defaulted rather than derived, because
    # nothing the source CSV carries decides it — the studio simulation
    # has no notion of the rating. The twelve labelled works arrive
    # through the `mature` upgrade doc, exactly as the added videos do.
    #
    # It is EMITTED rather than left absent because the manifest shape is
    # what documents the axis to whoever reads the published dataset: a
    # key that appeared only on the twelve would read as "these rows are
    # special", when the truth is that every asset carries a label and
    # almost all of them are false.
    mature: bool = False


# Keys that downstream tooling ANNOTATES onto an already-assembled
# profile. They are not part of the assembly schema — nothing in
# `derive_posts` reads them — but they are part of the file on disk, so
# reading a profile back has to know they are legitimate:
#
#   replaced_source_path  apply_upgrade.py, on every HQ replacement
#   ai_provenance         apply_upgrade.py, on the twelve declared rows
#   balance_source        studio_balance.py, on every rebalanced record
#
# Before this list existed, `AssetRecord(**entry)` raised
#
#     TypeError: AssetRecord.__init__() got an unexpected keyword
#                argument 'replaced_source_path'
#
# on the FIRST record carrying one, which meant `--recompose-posts`
# could not read the very profiles this script writes. The assembler had
# been unable to re-derive posts from its own output for as long as the
# annotations have existed.
#
# ⛔ The fix is a named allow-list, not `{k: v for k, v in entry.items()
# if k in FIELDS}`. Silently dropping unknown keys would swallow a typo
# and a genuinely new field alike; naming them means the next tool that
# adds one gets told to come here and say so.
POST_ASSEMBLY_KEYS = frozenset({
    "replaced_source_path",
    "ai_provenance",
    "balance_source",
})


def asset_records_from_profile(raw: list[dict[str, Any]],
                               source: Path | str) -> list[AssetRecord]:
    """Rebuild AssetRecords from a serialised profile.

    Raises ValueError naming the offending key if the profile carries a
    field that is neither part of AssetRecord nor a known post-assembly
    annotation — a profile shape nothing understands is a bug to report,
    not a record to guess at.
    """
    known = {f.name for f in dataclasses.fields(AssetRecord)}
    unknown: dict[str, int] = defaultdict(int)
    records: list[AssetRecord] = []
    for entry in raw:
        for key in entry:
            if key not in known and key not in POST_ASSEMBLY_KEYS:
                unknown[key] += 1
        records.append(AssetRecord(**{k: v for k, v in entry.items() if k in known}))
    if unknown:
        listed = ", ".join(f"{k} ({n} record(s))" for k, n in sorted(unknown.items()))
        raise ValueError(
            f"{source}: profile carries key(s) the assembler does not know: "
            f"{listed}. Add the field to AssetRecord if assembly should read "
            f"it, or to POST_ASSEMBLY_KEYS if it is an annotation applied "
            f"after assembly."
        )
    return records


def parse_int(s: str) -> int | None:
    if s is None or s == "":
        return None
    try:
        return int(s)
    except (TypeError, ValueError):
        return None


def parse_float(s: str) -> float | None:
    if s is None or s == "":
        return None
    try:
        return float(s)
    except (TypeError, ValueError):
        return None


def parse_bool(s: str) -> bool | None:
    if s is None or s == "":
        return None
    return s.strip().lower() in ("yes", "true", "1")


def split_tags(raw: str) -> list[str]:
    if not raw:
        return []
    return [t.strip() for t in raw.split(",") if t.strip()]


def transform_row(row: dict[str, str]) -> AssetRecord | None:
    """Map a CSV row to an AssetRecord. Returns None if the row should be
    dropped (unknown kind, unmapped status, etc.)."""
    kind = (row.get("kind") or "").strip()
    asset_type = KIND_TO_ASSET_TYPE.get(kind)
    if asset_type is None:
        return None

    confidentiality = (row.get("confidentiality") or "").strip()
    sensitivity = CONFIDENTIALITY_TO_TIER.get(confidentiality, "public")

    status = (row.get("status") or "").strip()
    workflow_state = STATUS_TO_WORKFLOW_STATE.get(status, "draft")
    archive_state = "archived" if status == "Archived" else (
        "active" if (row.get("is_published") or "").strip().lower() == "yes" else "draft"
    )

    franchise = (row.get("franchise") or "").strip()
    brand_workspace = franchise if franchise in BRAND_WORKSPACE_FRANCHISES else None

    tags = split_tags(row.get("tags") or "")
    # Add the non-brand franchises as tags.
    if franchise and franchise not in BRAND_WORKSPACE_FRANCHISES:
        tags.append(f"franchise:{franchise.lower()}")

    source = (row.get("source") or "").strip()
    layer = "A" if any(src.lower() in source.lower() for src in LAYER_A_SOURCES) else "B"
    if confidentiality == "Public" and source:
        layer = "A"  # Public confidentiality overrides

    studio = assign_studio(row)

    field_values: dict[str, Any] = {}
    for fd in FIELD_DEFINITIONS:
        col = fd["name"]
        raw = row.get(col)
        if raw is None or raw == "":
            continue
        ftype = fd["type"]
        if ftype == "number":
            v = parse_float(raw) if "." in str(raw) else parse_int(raw)
            if v is not None:
                field_values[col] = v
        elif ftype == "boolean":
            v = parse_bool(raw)
            if v is not None:
                field_values[col] = v
        elif ftype == "multi_select":
            field_values[col] = [s.strip() for s in str(raw).split(",")]
        else:
            field_values[col] = str(raw)

    file_size = parse_int(row.get("file_size_bytes") or "") or 0
    if file_size == 0:
        return None  # Skip zero-byte rows

    source_path = row["file_path"]
    return AssetRecord(
        id=stable_uuid("asset", row["asset_id"]),
        asset_type=asset_type,
        title=(row.get("title") or "").strip() or row.get("filename", "untitled"),
        description=(row.get("description") or "").strip(),
        file_path=reorganize_path(asset_type, source_path),
        source_path=source_path,
        source_root="local",
        file_extension=(row.get("file_format") or "").strip(),
        file_size_bytes=file_size,
        sensitivity_tier=sensitivity,
        archive_state=archive_state,
        owner_username=username_slug(row.get("artist") or "unknown"),
        collection_name=(row.get("project") or "Untriaged").strip(),
        team_name=(row.get("team") or "Reference").strip(),
        brand_workspace=brand_workspace,
        tags=tags,
        workflow_state=workflow_state,
        metadata={
            "filename": row.get("filename", ""),
            "kind": kind,
            "license": (row.get("license") or "").strip(),
            "usage_rights": (row.get("usage_rights") or "").strip(),
            "acquisition_source": source,
            "attribution": (row.get("attribution") or "").strip(),
            "group_id": row.get("group_id", ""),
        },
        field_values=field_values,
        external_id=(row.get("external_id") or "").strip(),
        review_notes=(row.get("review_notes") or "").strip() or None,
        reviewer_username=username_slug(row.get("approver") or "") or None,
        created_at=normalize_iso(row.get("created_at")),
        updated_at=normalize_iso(row.get("updated_at")),
        last_reviewed_at=normalize_iso(row.get("last_reviewed_at")) if row.get("last_reviewed_at") else None,
        license=(row.get("license") or "").strip(),
        attribution=(row.get("attribution") or "").strip(),
        layer=layer,
        studio=studio,
    )


def assign_studio(row: dict[str, str]) -> str:
    # Shared-pack patterns take precedence: these assets flow to BOTH sites
    # to exercise CAS dedup. Patterns are Layer A only — site_a can only
    # hold Layer A content.
    path = row.get("file_path", "")
    if any(p in path for p in SHARED_PACK_PATTERNS):
        return "shared"

    project = (row.get("project") or "").strip()
    if project in STUDIO_A_PROJECTS:
        return "a"
    if project in STUDIO_B_PROJECTS:
        return "b"
    # SPLIT_PROJECTS (Engine Core, Studio Library) and any unmapped project
    # land on site_b. Engine Core has a mix of CC0 Kenney (already caught
    # by SHARED_PACK_PATTERNS above) and proprietary fonts (BOOKmanOpti etc.
    # — Layer B). Studio Library has personal photos + Dresden Files
    # comics + non-PD reference docs (Layer B). All of that belongs on
    # site_b only.
    return "b"


def _split_engine_or_library(row: dict[str, str]) -> str:
    """For Engine Core + Studio Library, split rows ~50/50 between studios
    using a deterministic hash of the file path. Different subsets land on
    each side; CAS dedup catches the few that match by content."""
    path = row.get("file_path", "")
    digest = hashlib.sha256(path.encode()).hexdigest()
    return "a" if int(digest[0:2], 16) < 128 else "b"


def username_slug(name: str) -> str:
    """Turn 'Sofia Hernandez' into 'sofia.hernandez'. Idempotent."""
    if not name:
        return ""
    s = re.sub(r"[^a-zA-Z0-9 ]+", "", name).strip().lower()
    return s.replace(" ", ".")


def normalize_iso(s: str | None) -> str:
    if not s:
        return ""
    try:
        # The CSV uses 'Z' suffix; Python's fromisoformat accepts +00:00 only.
        return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    except Exception:
        return s or ""


# -----------------------------------------------------------------------------
# Per-pack sampling (10 GB cap enforcement)
# -----------------------------------------------------------------------------

def apply_trims(rows: list[dict[str, str]]) -> list[dict[str, str]]:
    """Apply hard drops, per-file-size caps, per-pack sampling caps, and
    per-project row caps. Deterministic — same input order → same selection.
    Drops surface a stat at the end so we can see what was excluded."""
    keep_counts: dict[str, int] = defaultdict(int)
    project_counts: dict[str, int] = defaultdict(int)
    kept: list[dict[str, str]] = []
    drops = {
        "hard_drop": 0,
        "oversize": 0,
        "pack_trim": 0,
        "project_cap": 0,
    }

    def pack_key(row: dict[str, str]) -> str | None:
        source = (row.get("source") or "").strip()
        for k in TRIMS:
            if k.lower() in source.lower():
                return k
        path = row.get("file_path", "")
        for k in TRIMS:
            if k.lower() in path.lower():
                return k
        return None

    def is_hard_drop(row: dict[str, str]) -> bool:
        path = row.get("file_path", "")
        return any(p in path for p in HARD_DROP_PATH_PATTERNS)

    def is_whitelisted_large(row: dict[str, str]) -> bool:
        path = row.get("file_path", "")
        return any(p in path for p in LARGE_FILE_WHITELIST)

    for row in rows:
        # 1. Hard-drop outliers
        if is_hard_drop(row):
            drops["hard_drop"] += 1
            continue

        # 2. Per-file-size cap (skip oversized unless whitelisted)
        size = int(row.get("file_size_bytes") or 0)
        if size > MAX_FILE_SIZE_BYTES and not is_whitelisted_large(row):
            drops["oversize"] += 1
            continue

        # 3. Per-pack sampling cap
        pk = pack_key(row)
        if pk and keep_counts[pk] >= TRIMS[pk]:
            drops["pack_trim"] += 1
            continue

        # 4. Per-project row cap
        project = (row.get("project") or "").strip()
        cap = PROJECT_ROW_CAPS.get(project)
        if cap is not None and project_counts[project] >= cap:
            drops["project_cap"] += 1
            continue

        if pk:
            keep_counts[pk] += 1
        project_counts[project] += 1
        kept.append(row)

    print(f"  drops: hard={drops['hard_drop']:,} oversize={drops['oversize']:,} "
          f"pack_trim={drops['pack_trim']:,} project_cap={drops['project_cap']:,}",
          file=sys.stderr)
    return kept


# -----------------------------------------------------------------------------
# Aggregations — users, teams, collections, brand workspaces
# -----------------------------------------------------------------------------

def derive_users(rows: list[AssetRecord]) -> list[dict[str, Any]]:
    seen: dict[str, dict[str, Any]] = {}
    for r in rows:
        for name_field in ("owner_username", "reviewer_username"):
            uname = getattr(r, name_field)
            if not uname or uname in seen:
                continue
            full = uname.replace(".", " ").title()
            seen[uname] = {
                "id": stable_uuid("user", uname),
                "username": uname,
                "full_name": full,
                "email": f"{uname}@studio.local",
                "role": "artist" if name_field == "owner_username" else "reviewer",
                "primary_team": r.team_name,
            }
    return sorted(seen.values(), key=lambda u: u["username"])


def derive_teams(rows: list[AssetRecord]) -> list[dict[str, Any]]:
    names = sorted({r.team_name for r in rows if r.team_name})
    return [{"id": stable_uuid("team", n), "name": n} for n in names]


def derive_collections(rows: list[AssetRecord]) -> list[dict[str, Any]]:
    by_name: dict[str, set[str]] = defaultdict(set)
    for r in rows:
        if r.collection_name:
            by_name[r.collection_name].add(r.studio)
    return [
        {
            "id": stable_uuid("collection", name),
            "name": name,
            "studio_membership": sorted(studios),
        }
        for name, studios in sorted(by_name.items())
    ]


def derive_brand_workspaces(rows: list[AssetRecord]) -> list[dict[str, Any]]:
    by_name: dict[str, set[str]] = defaultdict(set)
    for r in rows:
        if r.brand_workspace:
            by_name[r.brand_workspace].add(r.studio)
    return [
        {
            "id": stable_uuid("brand_workspace", name),
            "name": name,
            "owner_studio": "a",  # Mirror Studios owns both Echo + Mirror
            "studio_membership": sorted(studios),
        }
        for name, studios in sorted(by_name.items())
    ]


def derive_posts(assets: list[AssetRecord]) -> list[dict[str, Any]]:
    """Compose posts from the asset pool, sibling sets FIRST (#565).

    Pass order matters — each pass consumes what the previous one left:

      1. asset_group  — every group_id with 2+ shipped members becomes
         exactly ONE post holding ALL of them. This is the authoritative
         pass: group_id is the catalogue's own record of what belongs
         together (turnarounds, variant sets, kits).
      2. multi_asset  — the loose remainder, clustered by
         (collection, team, type) and bundled into 3-5 asset working sets.
      3. solo_showcase / revision_* — what is genuinely standalone, plus a
         capped number of lifecycle framings.
      4. team_roundup / project_sprint / video_* / cinematics_showreel —
         narrative posts that re-surface assets in a second context.

    Composition used to run (2) FIRST using (team, collection) proximity,
    with no notion of group_id at all, which scattered real sibling sets
    into single-asset posts — 73% of site_a's feed. There is no
    post-count target any more: the shape follows the data.

    ⭐ Deterministic, and LOCALLY so (#1296). Every choice is derived
    from stable_uuid / stable_int over asset ids, so composition depends
    on neither iteration order nor RNG call order.

    Passes 3-5 used to draw from a shared `random.Random("artist-alley.
    posts.v2")`. Seeding it made the function reproducible over an
    IDENTICAL pool, which is what a naive determinism test would check
    and pass. It did not make it stable under a local edit, because a
    seeded stream is a SEQUENCE: change how many draws an earlier
    iteration takes — by adding one asset to one team — and every draw
    after it re-sequences. Measured on site_a before this change,
    dropping a single audio asset:

        65 post ids disappeared and 67 appeared.
        ONE of the 65 actually contained the dropped asset.
        The other 64 were collateral: unrelated teams, unrelated
        projects, plus 38 surviving posts whose membership changed and
        19 whose titles changed.

    `stable_int`'s own docstring had already named this ("a seeded
    Random() re-sequences every downstream draw when an earlier pass
    changes, which silently rewrites unrelated posts") and passes 1-2
    were converted for exactly that reason; passes 3-5 were not. They
    are now, so a change confined to one team moves that team's posts
    and nothing else.

    ⛔ Do not reintroduce a shared RNG here. If a pass needs a pick,
    derive it with `stable_int` from the identity of what is being
    picked FOR — team name, project name, sweep index — never from a
    stream whose position depends on everything that ran before it.
    """
    # Group assets by (team, project) for related-asset lookup
    by_team_project: dict[tuple[str, str], list[AssetRecord]] = defaultdict(list)
    by_team: dict[str, list[AssetRecord]] = defaultdict(list)
    by_project: dict[str, list[AssetRecord]] = defaultdict(list)
    by_id: dict[str, AssetRecord] = {a.id: a for a in assets}
    for a in assets:
        by_team_project[(a.team_name, a.collection_name)].append(a)
        by_team[a.team_name].append(a)
        by_project[a.collection_name].append(a)

    theme_by_type = {
        "image": "reference",
        "3d": "model pass",
        "audio": "audio bundle",
        "video": "cinematic pass",
        "document": "briefing",
        "font": "type kit",
        "comic": "storyboards",
    }

    posts: list[dict[str, Any]] = []

    def _post(**kw: Any) -> dict[str, Any]:
        """Build a post with the canonical key set. Every post in
        posts.json must carry exactly these 18 keys (the Go seeder reads a
        subset via manifestPost; downstream tooling assumes key
        invariance), so they are assembled in one place."""
        members: list[AssetRecord] = kw.pop("members")
        types_in_post = sorted({a.asset_type for a in members})
        return {
            "id": kw.pop("id"),
            "title": kw.pop("title"),
            "description": kw.pop("description"),
            "author_username": kw.pop("author_username"),
            "collection_name": kw.pop("collection_name"),
            "team_name": kw.pop("team_name"),
            "brand_workspace": kw.pop("brand_workspace"),
            "tags": kw.pop("tags"),
            "asset_ids": [a.id for a in members],
            "asset_types_in_post": types_in_post,
            "is_mixed_type": len(types_in_post) > 1,
            "post_kind": kw.pop("post_kind"),
            "workflow_state": kw.pop("workflow_state"),
            "sensitivity_tier": kw.pop("sensitivity_tier"),
            "created_at": kw.pop("created_at"),
            "updated_at": max((a.updated_at for a in members), default=""),
            "studio": kw.pop("studio"),
            "layer": "A" if all(a.layer == "A" for a in members) else "B",
        }

    def _dominant(members: list[AssetRecord], attr: str) -> Any:
        """Most common non-empty value of `attr` across members, ties
        broken by name so the pick is deterministic."""
        counts: dict[Any, int] = defaultdict(int)
        for a in members:
            v = getattr(a, attr)
            if v:
                counts[v] += 1
        if not counts:
            return None
        return sorted(counts.items(), key=lambda kv: (-kv[1], str(kv[0])))[0][0]

    def _studio_of(members: list[AssetRecord]) -> str:
        studios = {a.studio for a in members}
        if len(studios) == 1:
            return next(iter(studios))
        return "shared"

    def _pick(pool: list[AssetRecord], lo: int, hi: int,
              *key: str) -> list[AssetRecord]:
        """A subset of `pool` of size in [lo, hi], chosen without an RNG.

        Replaces `rng.sample(pool, rng.randint(lo, hi))`. Both the size
        and the membership come from `stable_int` over `key` plus each
        candidate's own id, so the result is a pure function of (this
        pool, this key) — nothing about what else the caller assembled
        before or after can move it.

        Returned in id order, so `asset_ids` is stable too: the old
        `rng.sample` returned draw order, which is why 388 posts in the
        published dataset differ from the profile in `asset_ids` while
        only 5 differ in membership as a SET.
        """
        hi = min(hi, len(pool))
        lo = min(lo, hi)
        size = lo + stable_int(hi - lo + 1, "pick-size", *key)
        ranked = sorted(pool, key=lambda a: (stable_int(1 << 32, "pick", *key, a.id), a.id))
        return sorted(ranked[:size], key=lambda a: a.id)

    def _anchor(members: list[AssetRecord]) -> AssetRecord:
        """The member a post is dated and attributed by: the most recent,
        ties broken by id.

        The `, x.id` is not decoration. `max(members, key=updated_at)`
        alone resolves a tie by position in the list, so two members
        sharing a timestamp made the post's `created_at`, author and
        workflow state depend on member ORDER — and `created_at` sets
        `posted_at` and is read by the fixture sweep's deletion
        predicate (ADR 0098).
        """
        return max(members, key=lambda x: (x.updated_at, x.id))

    # ---------------------------------------------------------------------
    # Pass 1: group_id sibling sets — the authoritative composition (#565)
    # ---------------------------------------------------------------------
    # Every asset carries the group_id it was catalogued under: turnaround
    # sets, colour/size variants, multi-part kits. A group is a REAL
    # sibling set, so it becomes exactly ONE post holding ALL of its
    # shipped members.
    #
    # This pass used to pick "related" assets by (team, collection)
    # proximity instead, which scattered genuine sibling sets across
    # solo_showcase / revision_* singles and left the feed a wall of
    # one-asset posts (73% single on site_a).
    #
    # Only groups with 2+ SHIPPED members qualify. groups.csv's
    # asset_count describes the ORIGINAL source; sampling (apply_trims plus
    # the per-type balance cap) drops members, so a group claiming six
    # assets may ship two. What is on disk is what composes — a singleton
    # group is just a loose asset.
    by_group: dict[str, list[AssetRecord]] = defaultdict(list)
    loose: list[AssetRecord] = []
    for a in sorted(assets, key=lambda x: x.id):
        gid = (a.metadata or {}).get("group_id") or ""
        if gid:
            by_group[gid].append(a)
        else:
            loose.append(a)

    for gid in sorted(by_group):
        members = sorted(by_group[gid], key=lambda x: x.id)
        if len(members) < 2:
            loose.extend(members)
            continue
        anchor = _anchor(members)
        types_in_post = sorted({a.asset_type for a in members})
        collection = _dominant(members, "collection_name")
        team = _dominant(members, "team_name")
        if len(types_in_post) == 1:
            title = title_group_set(anchor.title)
            desc = (f"{len(members)}-piece {types_in_post[0]} set from "
                    f"{collection}, owned by {team}. Variants and siblings "
                    f"kept together as one delivery.")
        else:
            title = title_group_bundle(anchor.title)
            desc = (f"{team} bundle for {collection}: {len(members)} assets "
                    f"across {', '.join(types_in_post)}, delivered together.")
        posts.append(_post(
            members=members,
            id=stable_uuid("post", "group", gid),
            title=title,
            description=desc,
            author_username=anchor.owner_username,
            collection_name=collection,
            team_name=team,
            brand_workspace=anchor.brand_workspace,
            tags=sorted({t for a in members for t in (a.tags or [])}),
            post_kind="asset_group",
            workflow_state=anchor.workflow_state,
            sensitivity_tier=anchor.sensitivity_tier,
            created_at=anchor.created_at,
            studio=_studio_of(members),
        ))

    # ---------------------------------------------------------------------
    # Pass 2: working-set bundles from the loose assets
    # ---------------------------------------------------------------------
    # Assets with no group (or a group that shipped only one member) still
    # mostly belong to a body of work. Cluster them by
    # (collection, team, asset_type) and bundle a share of each cluster
    # into 3-5 asset "working set" posts; the remainder falls through to
    # solo posts below.
    #
    # LOOSE_BUNDLE_SHARE is the dial between "multi-asset dominant" and
    # "enough posts to fill a feed". Bundling a cluster WHOLESALE is not
    # the answer: (collection, team, type) is coarse — site_a's 481 loose
    # assets fall into just 19 clusters — so consuming all of them would
    # collapse the feed to ~284 posts and leave 1% singles, which is as
    # unrepresentative as the wall of singles it replaces.
    LOOSE_BUNDLE_SHARE = 0.88
    BUNDLE_SIZES = (3, 4, 5)

    loose_clusters: dict[tuple[str, str, str], list[AssetRecord]] = defaultdict(list)
    for a in loose:
        loose_clusters[(a.collection_name, a.team_name, a.asset_type)].append(a)

    solo_pool: list[AssetRecord] = []
    for key in sorted(loose_clusters):
        cluster = sorted(loose_clusters[key], key=lambda x: (x.created_at, x.id))
        n_bundle = int(len(cluster) * LOOSE_BUNDLE_SHARE)
        to_bundle, rest = cluster[:n_bundle], cluster[n_bundle:]
        solo_pool.extend(rest)

        # The 3/4/5 size cycle restarts for every cluster. It used to be
        # a counter shared across ALL clusters, which coupled them: one
        # asset added to the first cluster shifted its chunk count by
        # one, and every later cluster then re-chunked at different
        # boundaries. Per-cluster, a change stays inside the cluster it
        # was made in (#1296).
        size_idx = 0
        i = 0
        while i < len(to_bundle):
            size = BUNDLE_SIZES[size_idx % len(BUNDLE_SIZES)]
            size_idx += 1
            chunk = to_bundle[i:i + size]
            i += size
            if len(chunk) < 2:
                # A trailing single is a solo post, not a "bundle of one".
                solo_pool.extend(chunk)
                continue
            anchor = _anchor(chunk)
            collection, team, atype = key
            types_in_post = sorted({a.asset_type for a in chunk})
            theme = theme_by_type.get(atype, atype)
            title = title_collection_chunk(collection, team, theme)
            desc = (f"{team} working set for {collection}. "
                    f"{len(chunk)} {atype} assets pulled together for review.")
            posts.append(_post(
                members=chunk,
                id=stable_uuid("post", "bundle", anchor.id),
                title=title,
                description=desc,
                author_username=anchor.owner_username,
                collection_name=collection,
                team_name=team,
                brand_workspace=anchor.brand_workspace,
                tags=sorted({t for a in chunk for t in (a.tags or [])}),
                post_kind="multi_asset",
                workflow_state=anchor.workflow_state,
                sensitivity_tier=anchor.sensitivity_tier,
                created_at=anchor.created_at,
                studio=_studio_of(chunk),
            ))

    # ---------------------------------------------------------------------
    # Pass 3: solo showcase posts for the genuinely standalone remainder
    # ---------------------------------------------------------------------
    showcase_solo_titles = SOLO_FLAVORS

    for asset in sorted(solo_pool, key=lambda x: x.id):
        title_options = showcase_solo_titles.get(asset.asset_type, ["asset drop"])
        flavor = title_options[stable_int(len(title_options), "solo", asset.id)]
        desc_lead = asset.description or f"{asset.title} from {asset.collection_name}."
        if len(desc_lead) > 140:
            desc_lead = desc_lead[:137] + "..."
        posts.append(_post(
            members=[asset],
            id=stable_uuid("post", "solo", asset.id),
            title=title_solo(flavor, asset.title),
            description=desc_lead,
            author_username=asset.owner_username,
            collection_name=asset.collection_name,
            team_name=asset.team_name,
            brand_workspace=asset.brand_workspace,
            tags=list(asset.tags or []),
            post_kind="solo_showcase",
            workflow_state=asset.workflow_state,
            sensitivity_tier=asset.sensitivity_tier,
            created_at=asset.created_at,
            studio=asset.studio,
        ))

    # Pass 3b: Revision-stage posts — a few high-rated assets framed at
    # three lifecycle moments (draft, review, ship). Deliberately CAPPED:
    # each one is the same single asset posted a third time, so a large
    # target (this used to be 350) manufactures single-asset posts and is
    # exactly what buried the real multi-asset work.
    revision_stages = [
        ("draft", REVISION_TITLES["draft"],
         "First-pass draft of {title}. Open for early feedback."),
        ("review", REVISION_TITLES["review"],
         "Review pass on {title}. Notes from {owner} attached in the thread."),
        ("ship", REVISION_TITLES["ship"],
         "Locking in {title}. Sign-off from approver: {reviewer}."),
    ]
    high_rated = sorted(
        (a for a in assets if a.field_values.get("rating", 0) >= 4),
        key=lambda x: x.id,
    )
    revision_target = 90
    revision_count = 0
    for asset in high_rated:
        if revision_count >= revision_target:
            break
        for stage_key, title_tmpl, desc_tmpl in revision_stages:
            owner = asset.owner_username or "unknown"
            reviewer = asset.reviewer_username or owner
            posts.append(_post(
                members=[asset],
                id=stable_uuid("post", "revision", stage_key, asset.id),
                title=title_tmpl.format(title=asset.title),
                description=desc_tmpl.format(title=asset.title, owner=owner, reviewer=reviewer),
                author_username=owner if stage_key != "ship" else reviewer,
                collection_name=asset.collection_name,
                team_name=asset.team_name,
                brand_workspace=asset.brand_workspace,
                tags=list(asset.tags or []) + [f"stage:{stage_key}"],
                post_kind=f"revision_{stage_key}",
                workflow_state=asset.workflow_state,
                sensitivity_tier=asset.sensitivity_tier,
                created_at=asset.created_at,
                studio=asset.studio,
            ))
            revision_count += 1
            if revision_count >= revision_target:
                break

    # ---------------------------------------------------------------------
    # Pass 3: Team roundup posts (5-10 assets, same team, varied projects)
    # ---------------------------------------------------------------------
    # Periodic "this sprint's <team> output" posts. ~3-4 per team per
    # studio. Pulls assets across projects within the team.
    roundup_target = 60
    ROUNDUP_SWEEPS = 5
    # Sorted, not shuffled. The shuffle drew from the shared RNG and its
    # result depended on `by_team`'s insertion order, which is the order
    # of the asset list — so the pool order, and with it every roundup,
    # moved whenever the caller's list did. Sorted team names are a
    # property of the teams, not of how the assets happened to arrive.
    teams_pool = sorted(by_team)
    # Flat sweep order, so the target check below still breaks out of the
    # whole pass the way it did when the pool was `teams_pool * 5`.
    for sweep, team_name in ((s, t) for s in range(ROUNDUP_SWEEPS) for t in teams_pool):
        if len([p for p in posts if p.get("post_kind") == "team_roundup"]) >= roundup_target:
            break
        team_assets = sorted((a for a in by_team[team_name] if a.id),
                             key=lambda x: x.id)
        if len(team_assets) < 5:
            continue
        sample = _pick(team_assets, 5, 10, "roundup", team_name, str(sweep))
        anchor = _anchor(sample)
        types_in_post = sorted({a.asset_type for a in sample})
        projects_in_post = sorted({a.collection_name for a in sample})

        title = title_team_roundup(team_name)
        desc = (f"{team_name} team output across {len(projects_in_post)} project(s): "
                f"{', '.join(projects_in_post[:3])}"
                f"{'...' if len(projects_in_post) > 3 else ''}. "
                f"Mix of {', '.join(types_in_post)}.")

        posts.append(_post(
            members=sample,
            id=roundup_post_id(team_name, (a.id for a in sample)),
            title=title,
            description=desc,
            author_username=anchor.owner_username,
            # A roundup spans projects, but leaving collection_name NULL
            # meant the post landed in no collection at all and made the
            # collection pages look emptier than the data is (#565). File
            # it under the project it draws from most.
            collection_name=_dominant(sample, "collection_name"),
            team_name=team_name,
            brand_workspace=None,
            tags=sorted({t for a in sample for t in (a.tags or [])}),
            post_kind="team_roundup",
            workflow_state=anchor.workflow_state,
            sensitivity_tier="team",
            created_at=anchor.created_at,
            studio=anchor.studio,
        ))

    # ---------------------------------------------------------------------
    # Pass 4: Project sprint / milestone posts (5-10 assets, same project,
    #         varied teams)
    # ---------------------------------------------------------------------
    project_target = 80
    PROJECT_SWEEPS = 6
    projects_pool = sorted(by_project)
    sprint_labels = SPRINT_LABELS
    for sweep, project_name in ((s, p) for s in range(PROJECT_SWEEPS) for p in projects_pool):
        if len([p for p in posts if p.get("post_kind") == "project_sprint"]) >= project_target:
            break
        proj_assets = sorted(by_project[project_name], key=lambda x: x.id)
        if len(proj_assets) < 5:
            continue
        sample = _pick(proj_assets, 5, 10, "sprint", project_name, str(sweep))
        anchor = _anchor(sample)
        types_in_post = sorted({a.asset_type for a in sample})
        teams_in_post = sorted({a.team_name for a in sample})

        # The label used to come from a counter incremented once per
        # EMITTED post, shared across every project — so a project that
        # stopped qualifying re-labelled every sprint post after it. It
        # is now a function of the project and the sweep: each project
        # starts the label list at its own offset (so the ten labels are
        # all used across the dataset rather than only the first six)
        # and walks it one step per sweep.
        label = sprint_labels[
            (stable_int(len(sprint_labels), "sprint-label", project_name) + sweep)
            % len(sprint_labels)
        ]
        title = title_project_sprint(project_name, label)
        desc = (f"{label.title()} for {project_name}. "
                f"Pulls work from {', '.join(teams_in_post[:3])}"
                f"{'...' if len(teams_in_post) > 3 else ''} — "
                f"{', '.join(types_in_post)}.")

        posts.append(_post(
            members=sample,
            id=sprint_post_id(project_name, label, (a.id for a in sample)),
            title=title,
            description=desc,
            author_username=anchor.owner_username,
            collection_name=project_name,
            # A sprint spans teams, but a NULL team_name left these posts
            # unattributable in team views (#565 — 24 such posts on
            # site_a). Credit the team that contributed most of the sample.
            team_name=_dominant(sample, "team_name"),
            brand_workspace=anchor.brand_workspace,
            tags=sorted({t for a in sample for t in (a.tags or [])}),
            post_kind="project_sprint",
            workflow_state="in_review",
            sensitivity_tier="team",
            created_at=anchor.created_at,
            studio=anchor.studio,
        ))

    # ---------------------------------------------------------------------
    # Pass 5: Video boost — videos are scarce in the dataset, so give each
    # one a couple of framings + a "cinematics showreel" post that bundles
    # several together.
    #
    # This used to emit FIVE single-asset posts per video. With only 8
    # videos on site_a that is 40 manufactured singles — the same
    # padding-with-singles that buried the real multi-asset work, so it is
    # cut to two framings (#565).
    # ---------------------------------------------------------------------
    videos = sorted((a for a in assets if a.asset_type == "video"),
                    key=lambda x: x.id)
    video_post_templates = [
        ("dailies",      VIDEO_TITLES["dailies"],
         "Today's video pass on {project}. Reviewing pacing, color, and continuity."),
        ("cinematic_cut", VIDEO_TITLES["cinematic_cut"],
         "Latest cinematic cut for {project}. Compositing and grade approaching final."),
    ]
    for video in videos:
        for template_key, title_tmpl, desc_tmpl in video_post_templates:
            posts.append({
                "id": stable_uuid("post", "video", template_key, video.id),
                "title": title_tmpl.format(title=video.title, project=video.collection_name),
                "description": desc_tmpl.format(project=video.collection_name, title=video.title),
                "author_username": video.owner_username,
                "collection_name": video.collection_name,
                "team_name": video.team_name,
                "brand_workspace": video.brand_workspace,
                "tags": list(video.tags or []) + ["video", "cinematic"],
                "asset_ids": [video.id],
                "asset_types_in_post": ["video"],
                "is_mixed_type": False,
                "post_kind": f"video_{template_key}",
                "workflow_state": video.workflow_state,
                "sensitivity_tier": video.sensitivity_tier,
                "created_at": video.created_at,
                "updated_at": video.updated_at,
                "studio": video.studio,
                "layer": video.layer,
            })

    # Cinematics showreel posts — bundle 2-5 videos together with optional
    # related image/3D references. One per studio that has 3+ videos.
    for studio_key in ("a", "b"):
        # Pick videos available on that studio (studio = key OR shared)
        studio_videos = [v for v in videos if v.studio in (studio_key, "shared")]
        if len(studio_videos) < 2:
            continue
        # Two reels per studio (sample variations) so feeds feel populated
        for reel_label in REEL_LABELS:
            sample = _pick(studio_videos, 3, 5, "showreel", studio_key, reel_label)
            # Was `sample[0]` — the first DRAW, which is only meaningful
            # while the sample comes out of an RNG in draw order. The
            # post is dated by its most recent member like every other
            # multi-asset post.
            anchor = _anchor(sample)
            posts.append(_post(
                members=sample,
                id=showreel_post_id(studio_key, reel_label, (v.id for v in sample)),
                title=title_showreel(reel_label),
                description=(f"Studio cinematics roundup. {len(sample)} pieces bundled for "
                             f"the {reel_label.lower()} screening — see for pacing references "
                             f"and stylistic consistency across active projects."),
                author_username=anchor.owner_username,
                collection_name=_dominant(sample, "collection_name"),
                team_name="Marketing Art",
                brand_workspace=None,
                tags=sorted({t for v in sample for t in (v.tags or [])}) + ["cinematic", "showreel"],
                post_kind="cinematics_showreel",
                workflow_state="approved",
                sensitivity_tier="team",
                created_at=anchor.created_at,
                studio=studio_key,
            ))

    # Sort by created_at for interlaced feed appearance. `, p["id"]` makes
    # it a TOTAL order: `created_at` alone leaves ties resolved by the
    # order the passes happened to append in, so two posts sharing a
    # timestamp could swap places between runs without a single input
    # changing.
    posts.sort(key=lambda p: (p["created_at"], p["id"]))

    # ⭐ THE SAME TWO PASSES THE COMMITTED DOCUMENT GETS (#1306), on the
    # finished list rather than inside each template — which is the only
    # way "part two" can mean the same post in both. `clean_dashes`
    # catches the em dashes that arrive inside an embedded ASSET title
    # rather than from a template here.
    for post in posts:
        post["title"] = clean_dashes(post["title"])
    disambiguate_titles(posts)
    return posts


# -----------------------------------------------------------------------------
# Internet-fetched ingestion
# -----------------------------------------------------------------------------

def _spread_timestamp(seed_hex: str, base_iso: str = "2025-04-01T00:00:00Z",
                      window_days: int = 420) -> str:
    """Deterministic timestamp in [base, base + window_days). Internet
    assets need realistic per-asset timestamps spread over the same
    ~14-month window as the local content, so the seeded feed looks
    interlaced rather than all bunched at one fetch date."""
    from datetime import datetime, timedelta, timezone
    base = datetime.fromisoformat(base_iso.replace("Z", "+00:00"))
    offset_seconds = int(seed_hex[:12], 16) % (window_days * 86400)
    return (base + timedelta(seconds=offset_seconds)).astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def load_torrent_imports(json_path: Path) -> list[AssetRecord]:
    """Read seed/scripts/torrent_imports.json and emit AssetRecord
    entries for torrent-imported content. These files are pre-copied
    on the Synology directly into site_a/site_b at the indicated
    file_path — populate_archive treats source_root='torrent_import'
    as preexisting (skips copy attempt; just verifies the file
    exists at the dest path)."""
    if not json_path.is_file():
        return []
    data = json.loads(json_path.read_text(encoding="utf-8"))
    records: list[AssetRecord] = []
    for entry in data.get("assets", []):
        asset_type = entry.get("asset_type", "video")
        file_path = entry["file_path"]
        size = int(entry["file_size_bytes"])
        title = entry["name"]
        ext = Path(file_path).suffix.lstrip(".") or "bin"
        sha_seed = entry.get("sha_seed") or hashlib.sha256(f"{title}|{size}".encode()).hexdigest()
        records.append(AssetRecord(
            id=stable_uuid("asset", "torrent", sha_seed),
            asset_type=asset_type,
            title=title,
            description=entry.get("notes", f"{title} — Blender Foundation open content."),
            file_path=file_path,
            source_path=file_path,
            source_root="torrent_import",
            file_extension=ext,
            file_size_bytes=size,
            sensitivity_tier="public",
            archive_state="active",
            owner_username="seed.bot",
            collection_name="Cinematics Reference",
            team_name="Marketing Art",
            brand_workspace=None,
            tags=["reference", "public-domain", "cinematic", "torrent-imported",
                  f"source:{entry.get('source', 'unknown').lower().replace(' ', '-')}"],
            workflow_state="approved",
            metadata={
                "filename": Path(file_path).name,
                "kind": asset_type,
                "license": entry.get("license", ""),
                "usage_rights": "All Use",
                "acquisition_source": entry.get("source", ""),
                "attribution": entry.get("attribution", ""),
                "group_id": "",
                "import_source": "torrent",
            },
            field_values={},
            external_id="",
            review_notes=None,
            reviewer_username=None,
            created_at=_spread_timestamp(sha_seed),
            updated_at=_spread_timestamp(sha_seed),
            last_reviewed_at=_spread_timestamp(sha_seed),
            license=entry.get("license", ""),
            attribution=entry.get("attribution", ""),
            layer="A",
            studio="shared",
        ))
    return records


def load_internet_assets(internet_dir: Path) -> list[AssetRecord]:
    """Read seed/internet-fetched/MANIFEST.json (produced by fetch_gaps.py)
    and turn each entry into an AssetRecord marked studio='shared' + layer='A'.

    Returns empty list if the manifest doesn't exist yet (fetch_gaps not run)."""
    manifest_path = internet_dir / "MANIFEST.json"
    if not manifest_path.is_file():
        print(f"  no internet manifest at {manifest_path}; skipping", file=sys.stderr)
        return []

    data = json.loads(manifest_path.read_text(encoding="utf-8"))
    entries = data.get("assets", [])
    records: list[AssetRecord] = []

    for entry in entries:
        asset_type = entry.get("asset_type", "image")
        local_path = entry["path"]
        size = int(entry["size_bytes"])
        title = entry["name"]
        ext = Path(local_path).suffix.lstrip(".") or "bin"
        # All internet content goes under <type>/internet/<original-filename>
        # so it's easy to identify the provenance at a glance.
        dest_path = f"{TYPE_FOLDER_MAP.get(asset_type, asset_type)}/internet/{Path(local_path).name}"

        records.append(AssetRecord(
            id=stable_uuid("asset", "internet", entry.get("sha256", local_path)),
            asset_type=asset_type,
            title=title,
            description=entry.get("notes", "") or f"{title} — public-safe reference content.",
            file_path=dest_path,
            source_path=local_path,
            source_root="internet",
            file_extension=ext,
            file_size_bytes=size,
            sensitivity_tier="public",
            archive_state="active",
            owner_username="seed.bot",
            collection_name="Internet Reference",
            team_name="Reference",
            brand_workspace=None,
            tags=["reference", "public-domain", f"source:{entry.get('source', 'unknown').lower().replace(' ', '-')}"],
            workflow_state="approved",
            metadata={
                "filename": Path(local_path).name,
                "kind": asset_type,
                "license": entry.get("license", ""),
                "usage_rights": "All Use",
                "acquisition_source": entry.get("source", ""),
                "attribution": entry.get("attribution", ""),
                "group_id": "",
                "sha256": entry.get("sha256", ""),
                # fetch_gaps.py already writes the URL it pulled the file
                # from into the internet manifest, and this function used
                # to drop it on the floor — every internet record reached
                # the profile with no way back to its source. Carry both
                # keys so a record's provenance survives assembly (#602):
                # fetched_from is where a human looks, media_url is what a
                # machine GETs. For these sources they are the same URL;
                # for Pexels they are not, which is the whole point.
                "fetched_from": entry.get("fetched_from", ""),
                "media_url": entry.get("fetched_from", ""),
            },
            field_values={},
            external_id="",
            review_notes=None,
            reviewer_username=None,
            created_at=_spread_timestamp(entry.get("sha256", local_path)),
            updated_at=_spread_timestamp(entry.get("sha256", local_path)),
            last_reviewed_at=_spread_timestamp(entry.get("sha256", local_path)),
            license=entry.get("license", ""),
            attribution=entry.get("attribution", ""),
            layer="A",
            studio="shared",
        ))

    return records


# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

def recompose_posts(profiles: Path, sites: list[Path], dry_run: bool = False) -> int:
    """Regenerate posts IN PLACE from the already-assembled asset
    profiles, without the 12,871-row source CSV.

    seed/profiles/studio-{a,b}.assets.json are serialised AssetRecords —
    the exact asset set each site ships (verified id-for-id against each
    site's MANIFEST.json) — so post composition can be re-derived from the
    repo alone. That matters because the source dataset is a ~10 GB
    archive that is not checked in and not present on most machines.

    Posts are derived PER SITE rather than once over the combined pool.
    The combined path then filters posts down to those whose assets all
    live on one site, which silently DROPS any post composed across the
    studio split — including group posts, whose members are exactly the
    siblings most likely to span it. Deriving per site means every post
    that is generated is a post that lands.
    """
    out_names = {"studio-a.assets.json": "studio-a.posts.json",
                 "studio-b.assets.json": "studio-b.posts.json"}
    site_by_profile = {"studio-a.assets.json": "site_a",
                       "studio-b.assets.json": "site_b"}
    combined: dict[str, dict[str, Any]] = {}

    for assets_name, posts_name in out_names.items():
        src = profiles / assets_name
        if not src.is_file():
            print(f"error: {src} not found", file=sys.stderr)
            return 2
        raw = json.loads(src.read_text(encoding="utf-8"))
        try:
            assets = asset_records_from_profile(raw, src)
        except ValueError as exc:
            print(f"error: {exc}", file=sys.stderr)
            return 2
        posts = derive_posts(assets)
        combined.update({p["id"]: p for p in posts})

        n_multi = sum(1 for p in posts if len(p["asset_ids"]) > 1)
        share = 100.0 * (len(posts) - n_multi) / len(posts) if posts else 0.0
        print(f"{site_by_profile[assets_name]}: {len(assets):,} assets -> "
              f"{len(posts):,} posts ({n_multi:,} multi-asset, "
              f"{share:.0f}% single)", file=sys.stderr)

        if dry_run:
            continue
        write_json(profiles / posts_name, posts)
        # The Go seeder reads posts.json from the SITE root, not from the
        # catalogue dir (see catalogues.go loadCatalogues), so the site
        # copies are the ones a reseed actually consumes.
        for site_root in sites:
            if site_root.name == site_by_profile[assets_name]:
                write_json(site_root / "posts.json", posts)
                print(f"  wrote {site_root / 'posts.json'}", file=sys.stderr)

    if not dry_run:
        write_json(profiles / "dataset.posts.json",
                   sorted(combined.values(), key=lambda p: (p["created_at"], p["id"])))
    print(f"combined dataset.posts.json: {len(combined):,} posts", file=sys.stderr)
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path,
                        help="Path to artist-alley_dataset directory containing metadata.csv")
    parser.add_argument("--out", required=True, type=Path,
                        help="Output directory for profile JSONs")
    parser.add_argument("--internet", type=Path,
                        default=Path("seed/internet-fetched"),
                        help="Directory containing internet-fetched MANIFEST.json")
    parser.add_argument("--dry-run", action="store_true",
                        help="Print summary stats; don't write files")
    parser.add_argument("--recompose-posts", action="store_true",
                        help="Regenerate posts only, in place from --out's "
                             "studio-*.assets.json (no source CSV needed)")
    parser.add_argument("--site", type=Path, action="append", default=[],
                        help="Site root to also write posts.json into "
                             "(repeatable; used with --recompose-posts)")
    parser.add_argument("--skip-upgrade", action="store_true",
                        help="Do NOT apply the #604 dataset upgrade to the "
                             "generated profiles. Produces the pre-upgrade "
                             "library (916 sub-kilobyte images, no added "
                             "videos) — for reproducing the original "
                             "assembly only, never for a real site build.")
    args = parser.parse_args()

    if args.recompose_posts:
        return recompose_posts(args.out, args.site, args.dry_run)

    if args.source is None:
        parser.error("--source is required unless --recompose-posts is given")

    csv_path = args.source / "metadata.csv"
    if not csv_path.is_file():
        print(f"error: {csv_path} not found", file=sys.stderr)
        return 2

    args.out.mkdir(parents=True, exist_ok=True)

    print(f"reading {csv_path}", file=sys.stderr)
    with csv_path.open(newline="", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        raw_rows = list(reader)
    print(f"  read {len(raw_rows):,} rows", file=sys.stderr)

    print("applying per-pack trims to reach ~10 GB target", file=sys.stderr)
    trimmed = apply_trims(raw_rows)
    print(f"  kept {len(trimmed):,} rows ({len(raw_rows) - len(trimmed):,} dropped)", file=sys.stderr)

    print("transforming rows to AA schema", file=sys.stderr)
    assets: list[AssetRecord] = []
    skipped = 0
    for row in trimmed:
        a = transform_row(row)
        if a is None:
            skipped += 1
            continue
        assets.append(a)
    print(f"  produced {len(assets):,} assets ({skipped:,} skipped)", file=sys.stderr)

    # Enforce asset_type balance — applied after transform since asset_type
    # depends on the kind→type mapping. Without this, Kenney sprite floods
    # would drown out video / comic / document coverage.
    #
    # Deterministic shuffle BEFORE the cap so the selected sample is drawn
    # from across all packs/projects, not just the first N rows in CSV order.
    # CSV is sorted by group_id which clusters early packs (Kenney) at the
    # top — a naive cap would starve later packs (Snapdex / TCG / Adventure).
    print("balancing asset types", file=sys.stderr)
    import random as _r
    rng = _r.Random("artist-alley.seed.balance.v1")
    shuffled = assets[:]
    rng.shuffle(shuffled)

    type_counts: dict[str, int] = defaultdict(int)
    type_dropped: dict[str, int] = defaultdict(int)
    balanced: list[AssetRecord] = []
    for a in shuffled:
        cap = ASSET_TYPE_CAPS.get(a.asset_type)
        if cap is not None and type_counts[a.asset_type] >= cap:
            type_dropped[a.asset_type] += 1
            continue
        type_counts[a.asset_type] += 1
        balanced.append(a)
    if type_dropped:
        print("  asset-type drops: " + ", ".join(
            f"{t}={n:,}" for t, n in sorted(type_dropped.items())
        ), file=sys.stderr)
    print(f"  kept {len(balanced):,} after balance", file=sys.stderr)

    # Sort by created_at so when apply.sh ingests these in order, AA's
    # "recent uploads" feed shows interlaced variety (not all 3D first,
    # all audio second, etc.). The source CSV has realistic per-asset
    # timestamps spread over ~14 months — using them as the seed order
    # makes the demo look like an organic migration rather than a
    # type-by-type bulk import.
    balanced.sort(key=lambda a: (a.created_at, a.external_id or a.id))
    assets = balanced

    # Append internet-fetched content (all studio="shared", layer="A")
    print(f"loading internet-fetched assets from {args.internet}", file=sys.stderr)
    internet_assets = load_internet_assets(args.internet)
    print(f"  appending {len(internet_assets)} internet asset records", file=sys.stderr)
    assets.extend(internet_assets)

    # Append torrent-imported content (pre-copied on Synology — populate
    # won't re-copy these; it'll just verify the file is present at the
    # destination file_path)
    torrent_manifest = Path(__file__).resolve().parent / "torrent_imports.json"
    torrent_assets = load_torrent_imports(torrent_manifest)
    if torrent_assets:
        print(f"  appending {len(torrent_assets)} torrent-imported asset records",
              file=sys.stderr)
        assets.extend(torrent_assets)

    # Compute summary stats
    total_bytes = sum(a.file_size_bytes for a in assets)
    by_studio: dict[str, int] = defaultdict(int)
    by_studio_bytes: dict[str, int] = defaultdict(int)
    by_layer: dict[str, int] = defaultdict(int)
    by_layer_bytes: dict[str, int] = defaultdict(int)
    by_type: dict[str, int] = defaultdict(int)
    by_tier: dict[str, int] = defaultdict(int)

    for a in assets:
        by_studio[a.studio] += 1
        by_studio_bytes[a.studio] += a.file_size_bytes
        by_layer[a.layer] += 1
        by_layer_bytes[a.layer] += a.file_size_bytes
        by_type[a.asset_type] += 1
        by_tier[a.sensitivity_tier] += 1

    def gb(n: int) -> str:
        return f"{n / (1024 ** 3):.2f} GB"

    print("\n=== Summary ===", file=sys.stderr)
    print(f"Total: {len(assets):,} assets, {gb(total_bytes)}", file=sys.stderr)
    print("\nBy studio:", file=sys.stderr)
    for s in ("a", "b", "shared"):
        print(f"  studio-{s}: {by_studio[s]:>5,} assets, {gb(by_studio_bytes[s])}", file=sys.stderr)
    print("\nBy layer (A=public-safe, B=local-only):", file=sys.stderr)
    for lyr in ("A", "B"):
        print(f"  Layer {lyr}: {by_layer[lyr]:>5,} assets, {gb(by_layer_bytes[lyr])}", file=sys.stderr)
    print("\nBy asset_type:", file=sys.stderr)
    for t, n in sorted(by_type.items()):
        print(f"  {t:<10}: {n:>5,}", file=sys.stderr)
    print("\nBy sensitivity_tier:", file=sys.stderr)
    for t, n in sorted(by_tier.items()):
        print(f"  {t:<10}: {n:>5,}", file=sys.stderr)

    if args.dry_run:
        print("\n(dry-run; no files written)", file=sys.stderr)
        return 0

    # Derive aggregates
    users = derive_users(assets)
    teams = derive_teams(assets)
    collections = derive_collections(assets)
    brand_workspaces = derive_brand_workspaces(assets)
    posts = derive_posts(assets)
    mixed_type_posts = [p for p in posts if p["is_mixed_type"]]
    print(f"\nDerived {len(posts)} posts ({len(mixed_type_posts)} mixed-type)",
          file=sys.stderr)

    # Write per-profile files
    # site_a is the demo source — Layer A only (CC0 / CC-BY / OFL / PD).
    # site_b is everything (dogfood peer + local dev re-seed source).
    # Shared assets are Layer A by construction (see SHARED_PACK_PATTERNS),
    # so they all land on site_a too.
    studio_a_assets = [
        a for a in assets
        if a.studio in ("a", "shared") and a.layer == "A"
    ]
    studio_b_assets = [a for a in assets if a.studio in ("b", "shared")]
    # `dev` and `demo` profiles are aliases for site_b and site_a
    # respectively — same content, just named for their purpose.
    dev_assets = studio_b_assets
    demo_assets = studio_a_assets

    out = args.out
    write_json(out / "dataset.MANIFEST.json", {
        "generated_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "asset_count": len(assets),
        "total_bytes": total_bytes,
        "by_studio": dict(by_studio),
        "by_layer": dict(by_layer),
        "by_type": dict(by_type),
        "by_tier": dict(by_tier),
        "studio_a": {"asset_count": len(studio_a_assets), "bytes": sum(a.file_size_bytes for a in studio_a_assets)},
        "studio_b": {"asset_count": len(studio_b_assets), "bytes": sum(a.file_size_bytes for a in studio_b_assets)},
        "demo": {"asset_count": len(demo_assets), "bytes": sum(a.file_size_bytes for a in demo_assets)},
    })
    write_json(out / "dataset.users.json", users)
    write_json(out / "dataset.teams.json", teams)
    write_json(out / "dataset.collections.json", collections)
    write_json(out / "dataset.brand_workspaces.json", brand_workspaces)
    write_json(out / "dataset.field_definitions.json", FIELD_DEFINITIONS)
    write_json(out / "dataset.workflow.json", {
        "states": WORKFLOW_STATES,
        "transitions": [{"from": f, "to": t} for f, t in WORKFLOW_TRANSITIONS],
    })
    write_json(out / "studio-a.assets.json", [asdict(a) for a in studio_a_assets])
    write_json(out / "studio-b.assets.json", [asdict(a) for a in studio_b_assets])
    write_json(out / "dev.assets.json", [asdict(a) for a in dev_assets])
    write_json(out / "demo.assets.json", [asdict(a) for a in demo_assets])

    # Per-site post filtering: a post belongs to a site if ALL its assets
    # are in that site (otherwise the post would reference unseeded
    # records). For site_a (Layer A only), additionally require post.layer
    # == "A".
    asset_ids_a = {a.id for a in studio_a_assets}
    asset_ids_b = {a.id for a in studio_b_assets}
    site_a_posts = [
        p for p in posts
        if all(aid in asset_ids_a for aid in p["asset_ids"]) and p["layer"] == "A"
    ]
    site_b_posts = [
        p for p in posts
        if all(aid in asset_ids_b for aid in p["asset_ids"])
    ]
    write_json(out / "dataset.posts.json", posts)
    write_json(out / "studio-a.posts.json", site_a_posts)
    write_json(out / "studio-b.posts.json", site_b_posts)
    print(f"posts per site: site_a={len(site_a_posts)}, site_b={len(site_b_posts)}",
          file=sys.stderr)

    # The upgrade pass (#604). Everything above rebuilds the profiles from
    # the SOURCE metadata.csv, which still describes the pre-upgrade
    # library — 916 sub-kilobyte sprite tiles and no added videos. Without
    # this step a re-run silently reverts the dataset: populate_archive
    # copies the profile straight over MANIFEST.json, so the site would
    # come back with the tiny images restored and the videos gone.
    #
    # Applied here rather than left as a manual follow-up because "you
    # must remember to run a second script or you corrupt the dataset" is
    # not a pipeline, it is a trap. The step is idempotent and asserts its
    # own post-conditions, so a re-run that cannot satisfy them fails
    # loudly instead of writing a half-upgraded profile.
    if not args.skip_upgrade:
        rc = apply_dataset_upgrade(out)
        if rc != 0:
            return rc

    print(f"\nwrote {len(list(out.glob('*.json')))} files to {out}", file=sys.stderr)
    return 0


def apply_dataset_upgrade(out: Path) -> int:
    """Re-apply the #604 upgrade to the freshly-generated profiles."""
    upgrades = Path(__file__).resolve().parents[1] / "upgrades"
    if not upgrades.is_dir():
        print(f"warning: {upgrades} not found — profiles left un-upgraded",
              file=sys.stderr)
        return 0
    print("\napplying dataset upgrade (#604)", file=sys.stderr)
    for site, stem in (("site_a", "studio-a"), ("site_b", "studio-b")):
        proc = subprocess.run(
            [sys.executable, str(Path(__file__).with_name("apply_upgrade.py")),
             "--site", site,
             "--profile", str(out / f"{stem}.assets.json"),
             "--posts", str(out / f"{stem}.posts.json"),
             "--upgrades", str(upgrades)],
            capture_output=True, text=True)
        sys.stderr.write(proc.stdout)
        sys.stderr.write(proc.stderr)
        if proc.returncode != 0:
            print(f"error: upgrade pass failed for {site}", file=sys.stderr)
            return proc.returncode

    # `dev` and `demo` are aliases for site_b and site_a, and they were
    # written ABOVE — before the upgrade ran. So every upgrade since #604
    # has landed on studio-{a,b} and silently missed its own aliases:
    # demo.assets.json shipped 971 records where studio-a had 1,007,
    # i.e. a demo re-seed would have quietly dropped all 36 added videos
    # and none of the checks would have noticed, because nothing compares
    # an alias to its source. Re-copy after the upgrade so an alias is an
    # alias (#572).
    for stem, alias in (("studio-a", "demo"), ("studio-b", "dev")):
        src, dst = out / f"{stem}.assets.json", out / f"{alias}.assets.json"
        if src.is_file():
            shutil.copyfile(src, dst)
            print(f"alias       : {alias}.assets.json <- {stem}.assets.json",
                  file=sys.stderr)
    return 0


def write_json(path: Path, obj: Any) -> None:
    path.write_text(json.dumps(obj, indent=2, ensure_ascii=False, default=str), encoding="utf-8")


if __name__ == "__main__":
    sys.exit(main())
