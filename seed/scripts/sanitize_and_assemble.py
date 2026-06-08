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
import hashlib
import json
import os
import re
import sys
from collections import defaultdict
from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

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


def derive_posts(assets: list[AssetRecord], target_per_site: int = 600) -> list[dict[str, Any]]:
    """Generate posts across multiple shapes to give each site ~500+ posts
    after per-site filtering. Four pass strategy:

      1. Multi-asset posts — anchor + 1-4 related companions of different
         asset_types (the "real" mixed-asset post UI exercise).
      2. Single-asset showcase posts — one post per remaining asset,
         framed as a solo drop. Boosts count significantly.
      3. Team roundup posts — periodic "this sprint's <team> output"
         summaries that link 5-10 assets owned by the same team.
      4. Project sprint posts — milestone posts that link 5-10 assets
         from a single project across teams.

    Posts inherit author, collection, brand_workspace, and timestamps
    from their anchor (or are generated for roundups). Sort order is
    by created_at across all post types so the feed feels organic.
    """
    import random
    rng = random.Random("artist-alley.posts.v2")

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

    # ---------------------------------------------------------------------
    # Pass 1: Multi-asset posts (the original logic, expanded)
    # ---------------------------------------------------------------------
    used_in_multi: set[str] = set()
    multi_candidates = [
        a for a in assets
        if len(by_team_project[(a.team_name, a.collection_name)]) >= 3
    ]
    rng.shuffle(multi_candidates)

    multi_target = 250
    for anchor in multi_candidates:
        if anchor.id in used_in_multi:
            continue
        siblings = by_team_project[(anchor.team_name, anchor.collection_name)]
        eligible = [s for s in siblings if s.id != anchor.id and s.id not in used_in_multi]
        eligible.sort(key=lambda x: (x.asset_type == anchor.asset_type, x.id))
        n_companions = rng.randint(1, 4)
        chosen = eligible[:n_companions]
        if not chosen:
            continue

        post_assets = [anchor] + chosen
        used_in_multi.update(a.id for a in post_assets)

        types_in_post = sorted({a.asset_type for a in post_assets})
        theme_parts = [theme_by_type.get(t, t) for t in types_in_post]
        title = f"{anchor.collection_name}: {anchor.team_name} {' + '.join(theme_parts)}"

        if len(types_in_post) > 1:
            desc = (f"{anchor.team_name} working pass for {anchor.collection_name}. "
                    f"Bundles {len(post_assets)} assets across "
                    f"{', '.join(types_in_post)} — concept, references, and supporting media in one post.")
        else:
            desc = (f"{anchor.team_name} drop for {anchor.collection_name}. "
                    f"{len(post_assets)} {types_in_post[0]} assets pulled together.")

        all_tags = sorted({t for a in post_assets for t in (a.tags or [])})
        last_updated = max((a.updated_at for a in post_assets), default=anchor.updated_at)

        posts.append({
            "id": stable_uuid("post", "multi", anchor.id),
            "title": title,
            "description": desc,
            "author_username": anchor.owner_username,
            "collection_name": anchor.collection_name,
            "team_name": anchor.team_name,
            "brand_workspace": anchor.brand_workspace,
            "tags": all_tags,
            "asset_ids": [a.id for a in post_assets],
            "asset_types_in_post": types_in_post,
            "is_mixed_type": len(types_in_post) > 1,
            "post_kind": "multi_asset",
            "workflow_state": anchor.workflow_state,
            "sensitivity_tier": anchor.sensitivity_tier,
            "created_at": anchor.created_at,
            "updated_at": last_updated,
            "studio": anchor.studio,
            "layer": "A" if all(a.layer == "A" for a in post_assets) else "B",
        })
        if len(posts) >= multi_target:
            break

    # ---------------------------------------------------------------------
    # Pass 2: Single-asset showcase posts
    # ---------------------------------------------------------------------
    # For assets not yet anchoring a multi-post, create a solo showcase
    # post. Caps at target_per_site / 2 so we don't drown out other types.
    showcase_solo_titles = {
        "image": ["new render", "color study", "lighting pass", "reference plate", "concept sketch"],
        "3d": ["model drop", "topology pass", "PBR test", "WIP turntable", "asset ship"],
        "audio": ["audio drop", "sfx test", "ambient bed", "score sketch", "VO take"],
        "video": ["cinematic test", "edit pass", "previs sweep", "playblast", "shot reference"],
        "document": ["briefing", "style guide", "pipeline note", "research doc", "spec draft"],
        "font": ["type sample", "kerning check", "char set test", "weight comparison"],
        "comic": ["storyboard panel", "page rough", "layout pass"],
    }

    solo_candidates = [a for a in assets if a.id not in used_in_multi]
    rng.shuffle(solo_candidates)
    solo_target = 600

    for asset in solo_candidates:
        if len(posts) - multi_target >= solo_target:
            break

        title_options = showcase_solo_titles.get(asset.asset_type, ["asset drop"])
        flavor = rng.choice(title_options)
        title = f"{asset.title} — {flavor}"

        # Keep description short for solo posts; lean on the asset's
        # own description rather than re-narrating
        desc_lead = asset.description or f"{asset.title} from {asset.collection_name}."
        if len(desc_lead) > 140:
            desc_lead = desc_lead[:137] + "..."

        posts.append({
            "id": stable_uuid("post", "solo", asset.id),
            "title": title,
            "description": desc_lead,
            "author_username": asset.owner_username,
            "collection_name": asset.collection_name,
            "team_name": asset.team_name,
            "brand_workspace": asset.brand_workspace,
            "tags": list(asset.tags or []),
            "asset_ids": [asset.id],
            "asset_types_in_post": [asset.asset_type],
            "is_mixed_type": False,
            "post_kind": "solo_showcase",
            "workflow_state": asset.workflow_state,
            "sensitivity_tier": asset.sensitivity_tier,
            "created_at": asset.created_at,
            "updated_at": asset.updated_at,
            "studio": asset.studio,
            "layer": asset.layer,
        })

    # Pass 2b: Revision-stage posts — for a subset of high-rated assets,
    # generate additional posts framing them at different lifecycle stages
    # (draft, review pass, final ship). Same asset, different "moments."
    revision_stages = [
        ("draft", "Draft drop — {title}",
         "First-pass draft of {title}. Open for early feedback."),
        ("review", "Review pass — {title}",
         "Review pass on {title}. Notes from {owner} attached in the thread."),
        ("ship", "Ship gate — {title}",
         "Locking in {title}. Sign-off from approver: {reviewer}."),
    ]
    high_rated = [a for a in assets if a.field_values.get("rating", 0) >= 4]
    rng.shuffle(high_rated)
    revision_target = 350
    revision_count = 0
    for asset in high_rated:
        if revision_count >= revision_target:
            break
        for stage_key, title_tmpl, desc_tmpl in revision_stages:
            owner = asset.owner_username or "unknown"
            reviewer = asset.reviewer_username or owner
            posts.append({
                "id": stable_uuid("post", "revision", stage_key, asset.id),
                "title": title_tmpl.format(title=asset.title),
                "description": desc_tmpl.format(title=asset.title, owner=owner, reviewer=reviewer),
                "author_username": owner if stage_key != "ship" else reviewer,
                "collection_name": asset.collection_name,
                "team_name": asset.team_name,
                "brand_workspace": asset.brand_workspace,
                "tags": list(asset.tags or []) + [f"stage:{stage_key}"],
                "asset_ids": [asset.id],
                "asset_types_in_post": [asset.asset_type],
                "is_mixed_type": False,
                "post_kind": f"revision_{stage_key}",
                "workflow_state": asset.workflow_state,
                "sensitivity_tier": asset.sensitivity_tier,
                "created_at": asset.created_at,
                "updated_at": asset.updated_at,
                "studio": asset.studio,
                "layer": asset.layer,
            })
            revision_count += 1
            if revision_count >= revision_target:
                break

    # ---------------------------------------------------------------------
    # Pass 3: Team roundup posts (5-10 assets, same team, varied projects)
    # ---------------------------------------------------------------------
    # Periodic "this sprint's <team> output" posts. ~3-4 per team per
    # studio. Pulls assets across projects within the team.
    roundup_target = 60
    teams_pool = list(by_team.keys())
    rng.shuffle(teams_pool)
    for team_name in teams_pool * 5:  # multiple sweeps for variety
        if len([p for p in posts if p.get("post_kind") == "team_roundup"]) >= roundup_target:
            break
        team_assets = [a for a in by_team[team_name] if a.id]
        if len(team_assets) < 5:
            continue
        sample_size = rng.randint(5, min(10, len(team_assets)))
        sample = rng.sample(team_assets, sample_size)
        # Anchor = the most-recent asset for the team
        anchor = max(sample, key=lambda x: x.updated_at)
        types_in_post = sorted({a.asset_type for a in sample})
        projects_in_post = sorted({a.collection_name for a in sample})

        title = f"{team_name} sprint roundup — {len(sample)} drops"
        desc = (f"{team_name} team output across {len(projects_in_post)} project(s): "
                f"{', '.join(projects_in_post[:3])}"
                f"{'...' if len(projects_in_post) > 3 else ''}. "
                f"Mix of {', '.join(types_in_post)}.")

        posts.append({
            "id": stable_uuid("post", "roundup", team_name, anchor.id),
            "title": title,
            "description": desc,
            "author_username": anchor.owner_username,
            "collection_name": None,  # team-scoped, not collection-scoped
            "team_name": team_name,
            "brand_workspace": None,
            "tags": sorted({t for a in sample for t in (a.tags or [])}),
            "asset_ids": [a.id for a in sample],
            "asset_types_in_post": types_in_post,
            "is_mixed_type": len(types_in_post) > 1,
            "post_kind": "team_roundup",
            "workflow_state": anchor.workflow_state,
            "sensitivity_tier": "team",
            "created_at": anchor.created_at,
            "updated_at": max(a.updated_at for a in sample),
            "studio": anchor.studio,
            "layer": "A" if all(a.layer == "A" for a in sample) else "B",
        })

    # ---------------------------------------------------------------------
    # Pass 4: Project sprint / milestone posts (5-10 assets, same project,
    #         varied teams)
    # ---------------------------------------------------------------------
    project_target = 80
    projects_pool = list(by_project.keys())
    rng.shuffle(projects_pool)
    sprint_labels = ["sprint 12", "sprint 13", "sprint 14", "milestone alpha",
                     "milestone beta", "review session", "lock-in pass",
                     "polish week", "final review", "ship gate"]
    sprint_idx = 0
    for project_name in projects_pool * 6:
        if len([p for p in posts if p.get("post_kind") == "project_sprint"]) >= project_target:
            break
        proj_assets = by_project[project_name]
        if len(proj_assets) < 5:
            continue
        sample_size = rng.randint(5, min(10, len(proj_assets)))
        sample = rng.sample(proj_assets, sample_size)
        anchor = max(sample, key=lambda x: x.updated_at)
        types_in_post = sorted({a.asset_type for a in sample})
        teams_in_post = sorted({a.team_name for a in sample})

        label = sprint_labels[sprint_idx % len(sprint_labels)]
        sprint_idx += 1
        title = f"{project_name} {label} — {len(sample)} assets across {len(teams_in_post)} team(s)"
        desc = (f"{label.title()} for {project_name}. "
                f"Pulls work from {', '.join(teams_in_post[:3])}"
                f"{'...' if len(teams_in_post) > 3 else ''} — "
                f"{', '.join(types_in_post)}.")

        posts.append({
            "id": stable_uuid("post", "sprint", project_name, label, anchor.id),
            "title": title,
            "description": desc,
            "author_username": anchor.owner_username,
            "collection_name": project_name,
            "team_name": None,
            "brand_workspace": anchor.brand_workspace,
            "tags": sorted({t for a in sample for t in (a.tags or [])}),
            "asset_ids": [a.id for a in sample],
            "asset_types_in_post": types_in_post,
            "is_mixed_type": len(types_in_post) > 1,
            "post_kind": "project_sprint",
            "workflow_state": "in_review",
            "sensitivity_tier": "team",
            "created_at": anchor.created_at,
            "updated_at": max(a.updated_at for a in sample),
            "studio": anchor.studio,
            "layer": "A" if all(a.layer == "A" for a in sample) else "B",
        })

    # ---------------------------------------------------------------------
    # Pass 5: Video boost — videos are scarce in the dataset, so generate
    # extra video-anchored posts so they're not buried in the feed.
    # Each video gets 3-5 separate post framings + a "cinematics showreel"
    # post that bundles multiple videos with related references.
    # ---------------------------------------------------------------------
    videos = [a for a in assets if a.asset_type == "video"]
    video_post_templates = [
        ("dailies",      "Dailies — {title}",
         "Today's video pass on {project}. Reviewing pacing, color, and continuity."),
        ("cinematic_cut", "Cinematic cut — {title}",
         "Latest cinematic cut for {project}. Compositing and grade approaching final."),
        ("preview_share", "Preview share — {title}",
         "Sharing the latest preview for {project} for cross-team feedback."),
        ("playblast",    "Playblast — {title}",
         "Raw playblast from {project}. Animation timing pass."),
        ("reference",    "Reference reel — {title}",
         "Reference reel pulled for {project}. Annotating motion and staging cues."),
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
        for reel_idx, reel_label in enumerate(["Q3 reel", "Q4 reel"]):
            sample_size = min(len(studio_videos), rng.randint(3, 5))
            sample = rng.sample(studio_videos, sample_size)
            anchor = sample[0]
            posts.append({
                "id": stable_uuid("post", "showreel", studio_key, reel_label, anchor.id),
                "title": f"Cinematics {reel_label} — {sample_size} cuts",
                "description": (f"Studio cinematics roundup. {sample_size} pieces bundled for "
                               f"the {reel_label.lower()} screening — see for pacing references "
                               f"and stylistic consistency across active projects."),
                "author_username": anchor.owner_username,
                "collection_name": None,
                "team_name": "Marketing Art",
                "brand_workspace": None,
                "tags": sorted({t for v in sample for t in (v.tags or [])}) + ["cinematic", "showreel"],
                "asset_ids": [v.id for v in sample],
                "asset_types_in_post": ["video"],
                "is_mixed_type": False,
                "post_kind": "cinematics_showreel",
                "workflow_state": "approved",
                "sensitivity_tier": "team",
                "created_at": anchor.created_at,
                "updated_at": max(v.updated_at for v in sample),
                "studio": studio_key,
                "layer": "A" if all(v.layer == "A" for v in sample) else "B",
            })

    # Sort by created_at for interlaced feed appearance
    posts.sort(key=lambda p: p["created_at"])
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

def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path,
                        help="Path to artist-alley_dataset directory containing metadata.csv")
    parser.add_argument("--out", required=True, type=Path,
                        help="Output directory for profile JSONs")
    parser.add_argument("--internet", type=Path,
                        default=Path("seed/internet-fetched"),
                        help="Directory containing internet-fetched MANIFEST.json")
    parser.add_argument("--dry-run", action="store_true",
                        help="Print summary stats; don't write files")
    args = parser.parse_args()

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
    posts = derive_posts(assets, target_per_site=600)
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

    print(f"\nwrote {len(list(out.glob('*.json')))} files to {out}", file=sys.stderr)
    return 0


def write_json(path: Path, obj: Any) -> None:
    path.write_text(json.dumps(obj, indent=2, ensure_ascii=False, default=str), encoding="utf-8")


if __name__ == "__main__":
    sys.exit(main())
