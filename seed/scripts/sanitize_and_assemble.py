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

STUDIO_A_PROJECTS = {
    "Project Mirror",
    "Project Echo",
    "Project Citylight",
    "Project Compass",
    "Art Research",
    "Snapdex",
}

STUDIO_B_PROJECTS = {
    "Project Heroes",
    "Project Zoo",
    "Project Adventure",
    "Project Jumpstart",
    "Project Toybox",
    "Hearthstone Archive",
    "MTG Archive",
    "Heroes Archive",
}

# Engine Core is split — each studio takes a subset. Studio Library is split
# by filename heuristic in `_split_engine_or_library`.
SPLIT_PROJECTS = {"Engine Core", "Studio Library"}

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
ASSET_TYPE_CAPS: dict[str, int] = {
    "image": 800,        # was ~5,000; ~200 from each major pack source
    "audio": 200,        # was ~412
    "3d": 200,           # was ~392
    "video": 15,
    "comic": 6,
    "document": 50,
    "font": 30,
}

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
    file_path: str
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

    return AssetRecord(
        id=stable_uuid("asset", row["asset_id"]),
        asset_type=asset_type,
        title=(row.get("title") or "").strip() or row.get("filename", "untitled"),
        description=(row.get("description") or "").strip(),
        file_path=row["file_path"],
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
    project = (row.get("project") or "").strip()
    if project in STUDIO_A_PROJECTS:
        return "a"
    if project in STUDIO_B_PROJECTS:
        return "b"
    if project in SPLIT_PROJECTS:
        return _split_engine_or_library(row)
    return "shared"


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


# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path,
                        help="Path to artist-alley_dataset directory containing metadata.csv")
    parser.add_argument("--out", required=True, type=Path,
                        help="Output directory for profile JSONs")
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

    # Re-sort by external_id (CSV asset_id) so the output is stable across
    # runs even though we shuffled.
    balanced.sort(key=lambda a: a.external_id or a.id)
    assets = balanced

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

    # Write per-profile files
    studio_a_assets = [a for a in assets if a.studio in ("a", "shared")]
    studio_b_assets = [a for a in assets if a.studio in ("b", "shared")]
    dev_assets = assets
    demo_assets = [a for a in assets if a.layer == "A"]

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

    print(f"\nwrote {len(list(out.glob('*.json')))} files to {out}", file=sys.stderr)
    return 0


def write_json(path: Path, obj: Any) -> None:
    path.write_text(json.dumps(obj, indent=2, ensure_ascii=False, default=str), encoding="utf-8")


if __name__ == "__main__":
    sys.exit(main())
