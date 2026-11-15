#!/usr/bin/env python3
"""
Build the PROVISIONAL public `site_b` peer candidate (#1514, v0.11.1) into the ROOT of a SEPARATE
site_b dataset repository, named explicitly with --fixture. Dataset bytes do not live in this
application repository.

REPOSITORY LAYOUT (same as site_a)
----------------------------------
The site root IS the repository root. The generator owns exactly GENERATED_TOP
(MANIFEST.json, posts.json, DATASET.md, SHA256SUMS) and everything under images/. The
repository's curated README.md and .gitignore are NOT generated: --check ignores them and a
write never touches them. The .git directory is skipped. Any other file at the root is
reported as unexpected (for example a stale copy of the old site_b/ layout).

WHAT IT BUILDS
--------------
A site root of 503 image assets and 200 posts:

  * the 3 rows already staged (two cross-peer overlap anchors and one
    non-overlap control), carried through UNCHANGED: same ids, paths,
    bytes and row values, verified against pins below;
  * 500 ADDITIONAL distinct PNGs (each at least 64x64) copied byte-for-byte
    from five named packs of a local Kenney Game Assets All-in-1 3.6.0
    bundle, each pack pinned by its pack-local License.txt SHA-256;
  * 200 public, approved posts in one team and collection: 100 with three
    new assets and 100 with two, each new asset in exactly one post. The
    3 original assets stay standalone (the accepted overlap shape).

It is a CANDIDATE, not a release: a pack-local CC0 notice and a current
first-party page are pack-level evidence, not a per-file rights review,
and nothing here changes a rights ledger.

PAYLOAD DOCUMENTS
-----------------
The images do not travel alone. For each of the seven packs the candidate draws on (the five
above plus the two packs of the staged rows), the pack-local License.txt is copied unmodified
from the bundle to images/kenney/<pack>/License.txt, after its pin is checked. One provenance
file, images/kenney/PROVENANCE.json, binds each pack to its notice SHA-256, first-party page,
the bundle version and pinned Readme SHA-256, the selected-file scope and the caveats. Both are
listed in SHA256SUMS, so they ship inside the release payload with the images. The full
All-in-1 bundle is never copied: only the selected PNGs and these eight small documents.

DETERMINISM
-----------
Members are visited in sorted path order, quotas are fixed, a pack that
cannot fill its quota is topped up from the next pack in FILL_ORDER, ids
are UUIDv5, timestamps are fixed. Running it twice over the same bundle
and site_a gives byte-identical files; `--check` proves that by building
into a temporary directory and comparing every file.

CROSS-PEER CHECK
----------------
Every regular file under the site_a root is indexed by size; files whose
size equals a candidate's are hashed. A new file must not byte-match any
site_a file. The two declared anchors must match their contract file; the
control must match no site_a file (otherwise the build stops: the
control would no longer be a non-overlap fixture).

No machine path is written to any generated file: the bundle is recorded
by name and version, members by their path inside the bundle.

Usage:
  build_public_peer_candidate.py --bundle <All-in-1 3.6.0 dir> --site-a <site_a root> \\
      --fixture <site_b repository root> [--contract <peer-overlap json>] [--check]
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import stat
import struct
import sys
import tempfile
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
DEFAULT_CONTRACT = REPO / "seed" / "contracts" / "peer-overlap.v1.json"

BUNDLE_NAME = "Kenney Game Assets All-in-1"
BUNDLE_VERSION = "3.6.0"
CANDIDATE_MARK = ("provisional public site_b peer candidate (#1514, 503-asset scale stage); "
                  "not release-cleared")
ID_PREFIX = "artist-alley:v0.11.1:public-site-b"


@dataclass(frozen=True)
class Pack:
    folder: str        # path inside the bundle
    slug: str          # path-safe output segment
    name: str          # human name
    page: str          # current first-party page (pack-level evidence)
    notice_sha256: str  # pinned pack-local License.txt
    quota: int


@dataclass(frozen=True)
class Identity:
    owner: str
    team: str
    collection: str


@dataclass
class Config:
    packs: list[Pack]
    fill_order: list[str]                      # pack folders, used to top up a short quota
    base_rows: dict[str, str]                  # asset id -> sha256 of its canonical row JSON
    base_files: dict[str, tuple[str, int]]     # file_path -> (sha256, bytes)
    control_id: str
    posts_of_three: int
    posts_of_two: int
    identity: Identity
    min_side: int = 64
    epoch: str = "2026-11-10T00:00:00Z"
    extra: dict = field(default_factory=dict)
    base_packs: list = field(default_factory=list)  # NoticePack: the packs of the staged base rows
    readme_sha256: str = ""                        # pinned bundle Readme.html; empty = refuse


@dataclass(frozen=True)
class NoticePack:
    folder: str
    slug: str
    name: str
    page: str
    notice_sha256: str


PACKS = [
    Pack("2D assets/Isometric Tiles Buildings", "isometric-tiles-buildings", "Isometric Tiles Buildings",
         "https://kenney.nl/assets/isometric-tiles-buildings",
         "78dd057891c8a78b039260a553200044cc336a00827cbc9535d973021c0b75cc", 75),
    Pack("2D assets/Isometric Tiles Vehicles", "isometric-tiles-vehicles", "Isometric Tiles Vehicles",
         "https://kenney.nl/assets/isometric-tiles-vehicles",
         "1c2094b8f5ef716d83fe0a3fca6ba0f4daad2cc124e67bad329ba300a6225611", 9),
    Pack("UI assets/Mobile Controls", "mobile-controls", "Mobile Controls",
         "https://kenney.nl/assets/mobile-controls",
         "4c4c9d0d74f9b98ae42cb8413ded43a2a657b555f31ada2f6214d138b23e6c25", 140),
    Pack("2D assets/New Platformer Pack", "new-platformer-pack", "New Platformer Pack",
         "https://kenney.nl/assets/new-platformer-pack",
         "f0bc5a99a4710f75a2d507e2ab153025af203d9a6b5a698df737a47684316077", 140),
    Pack("2D assets/Toon Characters", "toon-characters", "Toon Characters",
         "https://kenney.nl/assets/toon-characters",
         "5f404b883e7e1acb43f36a71d8698f9156e6e8bc6e842aadefb2f030fb2d0dd7", 136),
]
SUPPORT_PAGE = "https://kenney.nl/support"
BASE_PACKS = [
    NoticePack("2D assets/Monster Builder Pack", "monster-builder-pack", "Monster Builder Pack",
               "https://kenney.nl/assets/monster-builder-pack",
               "b8c772c6ae1ae3d13fa3e9be9569b35c6b7671845e1d33ff9e2845d3e3ef3080"),
    NoticePack("2D assets/Planets", "planets", "Planets",
               "https://kenney.nl/assets/planets",
               "f9151e3adda789dc96df0d826b7381c6587fd28c16e423cb7141778ab587d384"),
]
README_SHA256 = "b40161bba62e54f94a3dbbd818c0b24b5865fdb6555d5d6806b2dc623037a868"
README_STATEMENTS = ("All content in this package is Creative Commons Zero (CC0) licensed",
                     "Please do not distribute this bundle directly")
NOTICE_NAME = "License.txt"
PROVENANCE_PATH = "images/kenney/PROVENANCE.json"
# The generated metadata document. It is not README.md, because the repository root holds a
# curated README.md (the same human-facing layout as site_a).
GENERATED_DOC = "DATASET.md"
GENERATED_TOP = frozenset({"MANIFEST.json", "posts.json", GENERATED_DOC, "SHA256SUMS"})
PAYLOAD_ROOT = "images/"
CURATED_TOP = frozenset({"README.md", ".gitignore"})
CAVEATS = [
    "The files were copied byte-for-byte from a local copy of Kenney Game Assets All-in-1 3.6.0. That this local copy "
    "is identical to the bundle Kenney distributes was not independently proven.",
    "Kenney's current first-party pages describe these packs as CC0; they do not by themselves prove these exact historical bytes.",
    "Scope: selected individual files from seven asset packs, grouped by pack folder, each with its pack-local License.txt. "
    "This is NOT the All-in-1 bundle, whose Readme asks that the bundle not be distributed directly.",
    "CC0 1.0 waives copyright and related rights. It does not grant trademark or publicity rights; do not imply endorsement by Kenney.",
    "Not legal advice.",
]

DEFAULT_CONFIG = Config(
    packs=PACKS,
    fill_order=["UI assets/Mobile Controls", "2D assets/New Platformer Pack", "2D assets/Toon Characters",
                "2D assets/Isometric Tiles Buildings", "2D assets/Isometric Tiles Vehicles"],
    base_rows={
        # canonical JSON (sort_keys, compact separators) of the three staged rows
        "40aa74bc-da53-5ace-885e-521a292c1201": "66739e55f00efc7bb1467401e131f277fd56100526f89e7077d9ea9d5b471d52",
        "57f9eac8-25ca-5580-b0c5-b4e58cccffa1": "998555620f728b5fb3746a3e4d43eb3acba1a85806dac9cd9b42918ff945583a",
        "c0082a12-3ebb-5997-90bf-0d18b745bbf3": "7eaf6bc8b7b4505a6a0bb73a735ffea8e6e034d70c16379fe333c1cb63ac91bd",
    },
    base_files={
        "images/kenney/monster-builder-pack/spritesheet/spritesheet_default.png":
            ("52ac0ca27297ac18457727ed79edc19de1c1b896a465559cdd9260ec2a4740c1", 146573),
        "images/kenney/planets/parts/sphere1.png":
            ("b1d6e3414762406a9724549876b4286c41a5b2b60007ccbfb5305001d94c9fb7", 170097),
        "images/kenney/planets/parts/light0.png":
            ("94fc9669a38a2b406ec136117a3cd50c14b92e6732fbbb87087d3470b934b9e2", 42570),
    },
    control_id="57f9eac8-25ca-5580-b0c5-b4e58cccffa1",
    posts_of_three=100,
    posts_of_two=100,
    # The existing candidate rows follow "first username (sorted) whose primary_team is the
    # row's team"; Environment -> anya.volkov. Studio Library is a public collection.
    identity=Identity(owner="anya.volkov", team="Environment", collection="Studio Library"),
    base_packs=BASE_PACKS,
    readme_sha256=README_SHA256,
)


class BuildError(Exception):
    pass


# --- helpers ---------------------------------------------------------------

def sha_file(p: Path) -> str:
    h = hashlib.sha256()
    with open(p, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def canonical_sha(row: dict) -> str:
    return hashlib.sha256(json.dumps(row, sort_keys=True, separators=(",", ":"),
                                     ensure_ascii=False).encode()).hexdigest()


def png_size(p: Path) -> tuple[int, int] | None:
    with open(p, "rb") as fh:
        head = fh.read(24)
    if len(head) < 24 or head[:8] != b"\x89PNG\r\n\x1a\n" or head[12:16] != b"IHDR":
        return None
    return struct.unpack(">II", head[16:24])


_SAFE = re.compile(r"[^a-z0-9._-]+")


def safe_segment(s: str) -> str:
    out = _SAFE.sub("-", s.lower()).strip("-.")
    out = re.sub(r"-{2,}", "-", out)
    if not out or out in (".", ".."):
        raise BuildError(f"unsafe path segment from {s!r}")
    return out


def human(stem: str) -> str:
    words = re.sub(r"[_\-]+", " ", stem).strip()
    words = re.sub(r"([a-z])([A-Z])", r"\1 \2", words)
    words = re.sub(r"\s+", " ", words)
    return words[:1].upper() + words[1:]


def stamp(epoch: str, seconds: int) -> str:
    base = datetime.strptime(epoch, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    return (base + timedelta(seconds=seconds)).strftime("%Y-%m-%dT%H:%M:%SZ")


def bundle_file(bundle: Path, rel: str) -> Path:
    """bundle/rel as a regular file, refusing unsafe paths and a symlink in ANY component."""
    parts = rel.split("/")
    if not rel or rel.startswith("/") or any(c in ("", ".", "..") for c in parts) or "\\" in rel:
        raise BuildError(f"unsafe bundle path {rel!r}")
    cur = bundle
    if cur.is_symlink() or not cur.is_dir():
        raise BuildError("bundle root is missing or a symlink")
    for i, c in enumerate(parts):
        cur = cur / c
        try:
            st = os.lstat(cur)
        except FileNotFoundError:
            raise BuildError(f"bundle path {rel!r} is missing") from None
        if stat.S_ISLNK(st.st_mode):
            raise BuildError(f"bundle path {rel!r} has a symlink component")
        if i < len(parts) - 1 and not stat.S_ISDIR(st.st_mode):
            raise BuildError(f"bundle path {rel!r} has a non-directory component")
    if not stat.S_ISREG(os.lstat(cur).st_mode):
        raise BuildError(f"bundle path {rel!r} is not a regular file")
    return cur


def read_notice(bundle: Path, folder: str, pin: str) -> tuple[bytes, str]:
    """The pinned pack-local License.txt and its CC0 statement, or BuildError."""
    try:
        p = bundle_file(bundle, folder + "/" + NOTICE_NAME)
    except BuildError as e:
        raise BuildError(f"pack notice {folder}/{NOTICE_NAME} is missing or not a regular file ({e})") from None
    raw = p.read_bytes()
    if hashlib.sha256(raw).hexdigest() != pin:
        raise BuildError(f"pack notice {folder}/{NOTICE_NAME} does not match its pin")
    statement = notice_statement(raw)
    if not statement:
        raise BuildError(f"pack notice {folder}/{NOTICE_NAME} has no CC0 statement")
    return raw, statement


def notice_statement(raw: bytes) -> str | None:
    for line in raw.decode("utf-8", "replace").splitlines():
        if "creative commons" in line.lower() and "cc0" in line.lower():
            return line.strip()
    return None


# --- inputs -----------------------------------------------------------------

def load_base(fixture: Path, cfg: Config) -> tuple[list[dict], dict[str, bytes]]:
    """The three staged rows (by id) and their file bytes, verified against the pins."""
    rows = json.loads((fixture / "MANIFEST.json").read_text(encoding="utf-8"))
    by_id = {r["id"]: r for r in rows}
    base = []
    for aid, want in cfg.base_rows.items():
        r = by_id.get(aid)
        if r is None:
            raise BuildError(f"staged base row {aid} is missing from the fixture manifest")
        if canonical_sha(r) != want:
            raise BuildError(f"staged base row {aid} changed (canonical sha {canonical_sha(r)}, pinned {want})")
        base.append(r)
    files = {}
    for rel, (want_sha, want_len) in cfg.base_files.items():
        p = fixture / rel
        if p.is_symlink() or not p.is_file():
            raise BuildError(f"staged base file {rel} is missing or not a regular file")
        b = p.read_bytes()
        if hashlib.sha256(b).hexdigest() != want_sha or len(b) != want_len:
            raise BuildError(f"staged base file {rel} changed")
        files[rel] = b
    if {r["file_path"] for r in base} != set(cfg.base_files):
        raise BuildError("staged base rows and base files disagree")
    return base, files


def site_a_index(site_a: Path, sizes: set[int]) -> tuple[dict[str, list[str]], dict]:
    """sha256 -> site_a-relative paths, for every regular file whose size is in `sizes`."""
    idx: dict[str, list[str]] = {}
    stats = {"regular_files": 0, "symlinks": 0, "hashed": 0}
    for dp, dns, fns in os.walk(site_a, followlinks=False):
        for n in dns + fns:
            if os.path.islink(os.path.join(dp, n)):
                stats["symlinks"] += 1
        for n in fns:
            full = Path(dp, n)
            st = os.lstat(full)
            if not stat.S_ISREG(st.st_mode):
                continue
            stats["regular_files"] += 1
            if st.st_size in sizes:
                stats["hashed"] += 1
                idx.setdefault(sha_file(full), []).append(full.relative_to(site_a).as_posix())
    for v in idx.values():
        v.sort()
    return idx, stats


def scan_pack(bundle: Path, pack: Pack, cfg: Config) -> tuple[list[dict], dict]:
    base = bundle / pack.folder
    if base.is_symlink() or not base.is_dir():
        raise BuildError(f"pack folder {pack.folder} is missing or a symlink")
    raw, statement = read_notice(bundle, pack.folder, pack.notice_sha256)
    out, info = [], {"symlinks": 0, "png": 0, "too_small": 0, "not_png": 0}
    for dp, dns, fns in os.walk(base, followlinks=False):
        dns.sort()
        for n in dns + fns:
            if os.path.islink(os.path.join(dp, n)):
                info["symlinks"] += 1
        for n in sorted(fns):
            if not n.lower().endswith(".png"):
                continue
            full = Path(dp, n)
            st = os.lstat(full)
            if not stat.S_ISREG(st.st_mode):
                continue
            info["png"] += 1
            wh = png_size(full)
            if wh is None:
                info["not_png"] += 1
                continue
            if wh[0] < cfg.min_side or wh[1] < cfg.min_side:
                info["too_small"] += 1
                continue
            member = full.relative_to(base).as_posix()
            out.append({"pack": pack, "member": member, "full": full, "bytes": st.st_size, "w": wh[0], "h": wh[1]})
    out.sort(key=lambda c: c["member"])
    info.update(statement=statement, eligible=len(out))
    return out, info


# --- build ------------------------------------------------------------------

def build(bundle: Path, site_a: Path, fixture: Path, contract_path: Path, out: Path, cfg: Config) -> dict:
    base_rows, base_files = load_base(fixture, cfg)
    contract = json.loads(contract_path.read_text(encoding="utf-8"))
    if not cfg.readme_sha256:
        raise BuildError("no bundle Readme pin configured")
    readme_raw = bundle_file(bundle, "Readme.html").read_bytes()
    if hashlib.sha256(readme_raw).hexdigest() != cfg.readme_sha256:
        raise BuildError("bundle Readme.html does not match its pin")
    readme_text = readme_raw.decode("utf-8", "replace")
    if not all(st in readme_text for st in README_STATEMENTS):
        raise BuildError("bundle Readme.html lacks an expected licence statement")
    base_notices = {}
    for bp in cfg.base_packs:
        base_notices[bp.folder] = read_notice(bundle, bp.folder, bp.notice_sha256)
    for rel in cfg.base_files:
        if not any(rel.startswith(f"images/kenney/{bp.slug}/") for bp in cfg.base_packs):
            raise BuildError(f"staged base file {rel} is not under a declared base pack folder")

    packs = {p.folder: p for p in cfg.packs}
    cands, pack_info = {}, {}
    for p in cfg.packs:
        cands[p.folder], pack_info[p.folder] = scan_pack(bundle, p, cfg)

    sizes = {c["bytes"] for cl in cands.values() for c in cl} | {v[1] for v in cfg.base_files.values()}
    idx, site_stats = site_a_index(site_a, sizes)

    # anchors and control against the full site_a corpus
    anchors = {}
    for fx in contract["fixtures"]:
        sb = fx["assets"]["site_b"]["asset_id"]
        sa_path = fx["assets"]["site_a"]["file_path"]
        hits = idx.get(fx["sha256"], [])
        if sa_path not in hits:
            raise BuildError(f"anchor {fx['name']}: the contract's site_a file does not byte-match")
        anchors[sb] = {"fixture": fx["name"], "site_a_matches": len(hits)}
    control = next(r for r in base_rows if r["id"] == cfg.control_id)
    ctrl_sha = cfg.base_files[control["file_path"]][0]
    if idx.get(ctrl_sha):
        raise BuildError("BLOCKED: the non-overlap control byte-matches a site_a file")
    if set(anchors) != {r["id"] for r in base_rows} - {cfg.control_id}:
        raise BuildError("contract anchors are not exactly the two staged non-control rows")

    excluded_hashes = {v[0] for v in cfg.base_files.values()}
    taken: set[str] = set()
    used_paths = {p.lower() for p in cfg.base_files}
    skipped = {"duplicate_within_candidate": 0, "matches_site_a": 0, "matches_base": 0, "path_collision": 0}
    chosen: list[dict] = []

    def take(c) -> bool:
        h = sha_file(c["full"])
        if h in excluded_hashes:
            skipped["matches_base"] += 1
            return False
        if h in taken:
            skipped["duplicate_within_candidate"] += 1
            return False
        if idx.get(h):
            skipped["matches_site_a"] += 1
            return False
        rel = "images/kenney/" + c["pack"].slug + "/" + "/".join(safe_segment(s) for s in c["member"].split("/"))
        if rel.lower() in used_paths or ".." in rel.split("/"):
            skipped["path_collision"] += 1
            return False
        c.update(sha256=h, file_path=rel)
        taken.add(h)
        used_paths.add(rel.lower())
        chosen.append(c)
        return True

    cursor = {f: 0 for f in cands}
    per_pack, deviations = {}, []
    for p in cfg.packs:
        n = 0
        cl = cands[p.folder]
        while n < p.quota and cursor[p.folder] < len(cl):
            if take(cl[cursor[p.folder]]):
                n += 1
            cursor[p.folder] += 1
        per_pack[p.folder] = n
        if n < p.quota:
            deviations.append({"pack": p.folder, "quota": p.quota, "taken": n, "short": p.quota - n})
    target = sum(p.quota for p in cfg.packs)
    for folder in cfg.fill_order:
        cl = cands[folder]
        while len(chosen) < target and cursor[folder] < len(cl):
            if take(cl[cursor[folder]]):
                per_pack[folder] += 1
            cursor[folder] += 1
    if len(chosen) != target:
        raise BuildError(f"only {len(chosen)} eligible distinct files for a target of {target}")
    if 3 * cfg.posts_of_three + 2 * cfg.posts_of_two != target:
        raise BuildError("post layout does not cover every new asset exactly once")

    # final order: pack order, then member path
    order = {p.folder: i for i, p in enumerate(cfg.packs)}
    chosen.sort(key=lambda c: (order[c["pack"].folder], c["member"]))

    ident = cfg.identity
    new_rows = []
    for i, c in enumerate(chosen):
        p = c["pack"]
        bundle_member = p.folder + "/" + c["member"]
        aid = str(uuid.uuid5(uuid.NAMESPACE_URL, f"{ID_PREFIX}:asset:kenney-all-in-1-{BUNDLE_VERSION}:{bundle_member}"))
        stem = human(Path(c["member"]).stem)
        t = stamp(cfg.epoch, i)
        new_rows.append({
            "id": aid, "asset_type": "image",
            "title": f"{p.name}: {stem}"[:200],
            "description": (f"Kenney (kenney.nl) CC0 pack image from {p.name}, copied unmodified from "
                            f"{BUNDLE_NAME} {BUNDLE_VERSION} for the provisional public site_b peer candidate "
                            f"(#1514). {c['w']}x{c['h']} PNG."),
            "file_path": c["file_path"], "file_extension": "png", "file_size_bytes": c["bytes"],
            "sensitivity_tier": "public", "mature": False, "archive_state": "active",
            "owner_username": ident.owner, "collection_name": ident.collection, "team_name": ident.team,
            "tags": ["kenney", "cc0", p.slug], "workflow_state": "approved",
            "metadata": {
                "acquisition_source": "Kenney.nl",
                "source_page": p.page,
                "source_bundle": {"name": BUNDLE_NAME, "version": BUNDLE_VERSION, "member": bundle_member,
                                  "sha256": c["sha256"], "bytes": c["bytes"]},
                "pack_notice": {"member": p.folder + "/License.txt", "sha256": p.notice_sha256,
                                "statement": pack_info[p.folder]["statement"]},
                "license": "CC0 1.0", "attribution": "Kenney (kenney.nl)",
                "candidate": CANDIDATE_MARK,
            },
            "field_values": {}, "review_notes": "", "reviewer_username": "",
            "created_at": t, "updated_at": t, "license": "CC0 1.0", "attribution": "Kenney (kenney.nl)",
        })
    ids = [r["id"] for r in base_rows + new_rows]
    if len(set(ids)) != len(ids):
        raise BuildError("asset id collision")

    posts, k = [], 0
    layout = [3] * cfg.posts_of_three + [2] * cfg.posts_of_two
    for n, size in enumerate(layout):
        members = new_rows[k:k + size]
        k += size
        names = []
        for r in members:
            nm = next(p.name for p in cfg.packs if r["tags"][2] == p.slug)
            if nm not in names:
                names.append(nm)
        pid = str(uuid.uuid5(uuid.NAMESPACE_URL, f"{ID_PREFIX}:post:{n + 1:03d}"))
        t = stamp(cfg.epoch, 86400 + 600 * n)
        posts.append({
            "id": pid,
            "title": f"{' + '.join(names)} set {n + 1:03d}",
            "description": (f"{size} Kenney CC0 images from {', '.join(names)} "
                            f"(provisional public site_b peer candidate, #1514)."),
            "asset_ids": [r["id"] for r in members],
            "asset_types_in_post": ["image"], "is_mixed_type": False,
            "post_kind": "asset_group",
            "author_username": ident.owner, "collection_name": ident.collection, "team_name": ident.team,
            "sensitivity_tier": "public", "workflow_state": "approved",
            "tags": ["kenney", "cc0"] + sorted({r["tags"][2] for r in members}),
            "created_at": t, "updated_at": t,
        })
    if k != len(new_rows):
        raise BuildError("posts do not reference every new asset exactly once")

    # payload documents: seven pack-local notices and one provenance file
    all_packs = [(p.folder, p.slug, p.name, p.page, p.notice_sha256) for p in cfg.packs] + \
                [(bp.folder, bp.slug, bp.name, bp.page, bp.notice_sha256) for bp in cfg.base_packs]
    notices, prov_packs = {}, []
    occupied = {r["file_path"].lower() for r in base_rows + new_rows}
    for folder, slug, name, page, pin in sorted(all_packs, key=lambda t: t[1]):
        if safe_segment(slug) != slug:
            raise BuildError(f"pack slug {slug!r} is not path-safe")
        rel = f"images/kenney/{slug}/{NOTICE_NAME}"
        if rel.lower() in occupied:
            raise BuildError(f"pack notice path {rel} collides with another payload file")
        occupied.add(rel.lower())
        raw, statement = base_notices[folder] if folder in base_notices else read_notice(bundle, folder, pin)
        notices[rel] = raw
        n_files = sum(1 for r in base_rows + new_rows if r["file_path"].startswith(f"images/kenney/{slug}/"))
        prov_packs.append({"slug": slug, "name": name, "bundle_folder": folder, "files": n_files,
                           "notice": rel, "notice_sha256": pin, "notice_statement": statement,
                           "first_party_page": page})
    if PROVENANCE_PATH.lower() in occupied:
        raise BuildError(f"{PROVENANCE_PATH} collides with another payload file")
    if sum(p["files"] for p in prov_packs) != len(base_rows) + len(new_rows):
        raise BuildError("not every image is under one of the declared pack folders")
    provenance = {
        "schema": "artist-alley.site-b-kenney-provenance/v1",
        "scope": ("Selected individual PNG files from the asset packs below, grouped by pack folder, each folder "
                  "with its pack-local License.txt. This is NOT the Kenney Game Assets All-in-1 bundle and must not "
                  "be used to redistribute it."),
        "source_bundle": {"name": BUNDLE_NAME, "version": BUNDLE_VERSION, "readme_sha256": cfg.readme_sha256},
        "license": "CC0-1.0",
        "attribution": "Not required. Optional credit: Kenney (kenney.nl)",
        "support_page": SUPPORT_PAGE,
        "packs": prov_packs,
        "caveats": CAVEATS,
    }

    # write
    out.mkdir(parents=True, exist_ok=True)
    manifest = base_rows + new_rows
    (out / "MANIFEST.json").write_text(json.dumps(manifest, indent=2, ensure_ascii=True) + "\n", encoding="utf-8")
    (out / "posts.json").write_text(json.dumps(posts, indent=2, ensure_ascii=True) + "\n", encoding="utf-8")
    for rel, b in base_files.items():
        dst = out / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        if not dst.exists() or dst.read_bytes() != b:
            dst.write_bytes(b)
    for c in chosen:
        dst = out / c["file_path"]
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(c["full"], dst)
        if sha_file(dst) != c["sha256"]:
            raise BuildError(f"copy of {c['file_path']} does not match its source")
    for rel, raw in notices.items():
        dst = out / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        dst.write_bytes(raw)
    (out / PROVENANCE_PATH).write_text(json.dumps(provenance, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
                                       encoding="utf-8")

    report = {
        "assets": len(manifest), "new_assets": len(new_rows), "posts": len(posts),
        "posts_layout": {"three": cfg.posts_of_three, "two": cfg.posts_of_two},
        "per_pack": {p.name: per_pack[p.folder] for p in cfg.packs},
        "quota_deviations": deviations,
        "skipped": skipped,
        "packs": {p.name: {k2: v for k2, v in pack_info[p.folder].items()} for p in cfg.packs},
        "site_a": site_stats,
        "anchors": anchors, "control_site_a_matches": 0,
        "new_bytes": sum(c["bytes"] for c in chosen),
        "payload_docs": {"notices": sorted(notices), "provenance": PROVENANCE_PATH},
        "provenance_packs": [(p["slug"], p["files"]) for p in prov_packs],
    }
    (out / GENERATED_DOC).write_text(readme(cfg, report), encoding="utf-8")
    write_sums(out)
    return report


def write_sums(out: Path) -> None:
    files = sorted(p.relative_to(out).as_posix() for p in out.rglob("*")
                   if p.is_file() and p.name != "SHA256SUMS")
    lines = [f"{sha_file(out / f)}  {f}\n" for f in files]
    (out / "SHA256SUMS").write_text("".join(lines), encoding="utf-8")


def readme(cfg: Config, rep: dict) -> str:
    pack_rows = "\n".join(
        f"| {p.name} | `{p.folder}` | {rep['per_pack'][p.name]} | `{p.notice_sha256}` | <{p.page}> |"
        for p in cfg.packs)
    notice_rows = "\n".join(
        f"| `{slug}` | {n} | `images/kenney/{slug}/License.txt` |" for slug, n in rep["provenance_packs"])
    dev = ("None: every pack filled its quota." if not rep["quota_deviations"] else
           "; ".join(f"{d['pack']} short by {d['short']} (filled from FILL_ORDER)" for d in rep["quota_deviations"]))
    return f"""# Public peer candidate (provisional)

**Status: PROVISIONAL candidate for issue #1514 (v0.11.1).**
This is not a release, not a CI cutover and not permission to publish.
No configured CI job or seed profile currently points to this candidate (a local,
disposable seed has read it for testing), and it does not replace the existing peer corpus.

This document and the dataset files at the repository root are GENERATED by
`seed/scripts/build_public_peer_candidate.py` in the artist-alley application repository:
`MANIFEST.json`, `posts.json`, this `{GENERATED_DOC}`, `SHA256SUMS` and everything under
`images/`. Do not edit them by hand. The curated `README.md` and `.gitignore` are not generated.

## Counts

- {rep['assets']} assets: the 3 original overlap rows plus {rep['new_assets']} new images.
- {rep['posts']} posts: {rep['posts_layout']['three']} with three new assets and
  {rep['posts_layout']['two']} with two. Every new asset is in exactly one post. The 3
  original assets stay standalone.
- All {rep['posts']} posts are public and approved, in one team
  (`{cfg.identity.team}`) and one collection (`{cfg.identity.collection}`), authored by
  `{cfg.identity.owner}`, so one designated feed has {rep['posts']} posts: more than the
  73 needed to reach a third 36-item UI page. This is a manifest count; the visible count
  still has to be measured on a seeded instance.
- {rep['new_bytes']} bytes of new images.
- {len(rep['payload_docs']['notices'])} pack-local `License.txt` notices and one provenance file
  (`{rep['payload_docs']['provenance']}`) under `images/kenney/`. They are payload documents, not
  assets: they have no manifest rows, and they ship with the images in the release payload.

## What the original three rows exercise

A three-row site root for the public `site_b` side of the cross-peer
overlap contract `seed/contracts/peer-overlap.v1.json` in the artist-alley application
repository (status `proposed`):

| Role | Asset id | File | Bytes | SHA-256 |
|---|---|---|---|---|
| overlap anchor `overlap-image-1` | `40aa74bc-da53-5ace-885e-521a292c1201` | `images/kenney/monster-builder-pack/spritesheet/spritesheet_default.png` | 146573 | `52ac0ca27297ac18457727ed79edc19de1c1b896a465559cdd9260ec2a4740c1` |
| overlap anchor `overlap-image-2` | `c0082a12-3ebb-5997-90bf-0d18b745bbf3` | `images/kenney/planets/parts/sphere1.png` | 170097 | `b1d6e3414762406a9724549876b4286c41a5b2b60007ccbfb5305001d94c9fb7` |
| non-overlap control | `57f9eac8-25ca-5580-b0c5-b4e58cccffa1` | `images/kenney/planets/parts/light0.png` | 42570 | `94fc9669a38a2b406ec136117a3cd50c14b92e6732fbbb87087d3470b934b9e2` |

- **Two anchors:** each is byte-identical to the `site_a` file the
  contract names, but has its own asset id. The ids are UUIDv5 in the URL
  namespace, with names `artist-alley:v0.11.1:public-site-b:overlap-image-<n>`.
  They are the cross-peer duplicate-detection fixtures.
- **Control:** intended as a non-overlap fixture, a file that should not
  occur on `site_a`. The generator checked it against every regular file
  of the local `site_a` corpus by exact SHA-256 (files of equal size were
  hashed) and found no match. The same check must pass against the
  released `site_a` before any release.
- Every original manifest row keeps `metadata.candidate` ("provisional public
  site_b peer candidate v1 (#1514); not release-cleared").

## The new images

Copied byte-for-byte from **{BUNDLE_NAME} {BUNDLE_VERSION}**. Each row records
`metadata.source_bundle` (bundle name, version, member path, SHA-256, bytes),
`metadata.pack_notice` (the pack-local `License.txt` member, its SHA-256 and its
statement) and `metadata.source_page` (the pack's current first-party page). Each row
also keeps `metadata.candidate` ("{CANDIDATE_MARK}").

| Pack | Bundle folder | Files | Pack `License.txt` SHA-256 | First-party page |
|---|---|---|---|---|
{pack_rows}

- Every pack notice states `License: (Creative Commons Zero, CC0)` and was checked
  against the pin above. <{SUPPORT_PAGE}> explains Kenney's asset licensing.
- Selection: sorted member order within each pack, PNG at least {cfg.min_side}x{cfg.min_side},
  regular file (no symlink), quotas {', '.join(f'{p.name} {p.quota}' for p in cfg.packs)}.
  Quota deviations: {dev}
- Excluded: any file byte-identical to another new file, to one of the 3 original files,
  or to ANY file of the local `site_a` corpus. So the only cross-peer byte overlaps
  are the two declared anchors.
- Output names are lower-case, path-safe and checked for case-insensitive collisions.
- The bytes match the local bundle. Whether they also equal what kenney.nl serves today
  was NOT checked.

## Pack notices and provenance (payload documents)

The images travel with their notices. For every pack folder under `images/kenney/`, the
pack-local `License.txt` is copied unmodified from the bundle, after its SHA-256 is checked
against the pin in the table above (the two staged-row packs, Monster Builder Pack and
Planets, have their own pins in the generator):

| Pack folder | Images | Notice |
|---|---|---|
{notice_rows}

`{rep['payload_docs']['provenance']}` binds each pack folder to its notice SHA-256, its
first-party page, the bundle name and version, and the pinned SHA-256 of the bundle's
`Readme.html`. It also states the scope (selected files only, not the All-in-1 bundle, which
its Readme asks not to be distributed directly) and the caveats: the local bundle copy's
identity with Kenney's distribution is not proven, the first-party pages do not prove the
exact bytes, and CC0 grants no trademark or publicity rights. It holds no machine paths.

## Rights and visibility limits

- **Not a rights clearance.** A pack-local CC0 notice plus a current first-party page is
  pack-level evidence. A PRIVATE, conditional per-file review record covers the exact 503
  selected PNGs only. It is not legal clearance and not public-release authorization, and it
  does not establish the exact upstream origin of the local bundle copy. The seven licence
  notices and the provenance file accompany the payload. No rights ledger changed.
- **Not measured.** The {rep['posts']}-post feed and the {rep['assets']}-asset listing are manifest counts. The
  counts visible to a signed-in user on a seeded instance still have to be measured.
- **Not shipped.** Nothing has been released or uploaded. Any release archive built from these
  files is a private local candidate only.

## Rebuild and verify

From the artist-alley application repository root, with read-only access to a local
{BUNDLE_NAME} {BUNDLE_VERSION} directory and the local `site_a` root:

```
python3 seed/scripts/build_public_peer_candidate.py --bundle <bundle dir> --site-a <site_a root> \\
    --fixture <site_b repository root> --check
```

`--check` rebuilds into a temporary directory and fails unless every generated file is
byte-identical to the one here and no unexpected file is present. Without `--check` it
rewrites the generated files; it never touches `README.md` or `.gitignore`.

`SHA256SUMS` lists every generated file except itself, sorted by path (`LC_ALL=C`): the
metadata files, every image, and the payload documents above. It does not list the curated
`README.md` or `.gitignore`. To verify, from the repository root: `sha256sum -c SHA256SUMS`.
"""


def compare_trees(a: Path, b: Path) -> list[str]:
    fa = {p.relative_to(a).as_posix() for p in a.rglob("*") if p.is_file()}
    fb = {p.relative_to(b).as_posix() for p in b.rglob("*") if p.is_file()}
    diffs = sorted(fa ^ fb)
    diffs += sorted(f for f in fa & fb if (a / f).read_bytes() != (b / f).read_bytes())
    return diffs


def fixture_view(root: Path) -> tuple[set[str], list[str]]:
    """(generated-owned files present under `root`, unexpected files).

    Generated-owned: GENERATED_TOP at the root and every file under PAYLOAD_ROOT. The curated
    root files and the .git directory are skipped. Anything else is unexpected."""
    owned: set[str] = set()
    unexpected: list[str] = []
    for p in root.rglob("*"):
        rel = p.relative_to(root).as_posix()
        if rel == ".git" or rel.startswith(".git/") or not p.is_file():
            continue
        if rel in GENERATED_TOP or rel.startswith(PAYLOAD_ROOT):
            owned.add(rel)
        elif rel not in CURATED_TOP:
            unexpected.append(rel)
    return owned, sorted(unexpected)


def compare_to_fixture(generated: Path, fixture: Path) -> list[str]:
    """Differences between a fresh build and the generated-owned part of the fixture, plus any
    unexpected fixture file. The curated README.md and .gitignore are never compared."""
    gen = {p.relative_to(generated).as_posix() for p in generated.rglob("*") if p.is_file()}
    owned, unexpected = fixture_view(fixture)
    diffs = sorted(gen ^ owned)
    diffs += sorted(f for f in gen & owned if (generated / f).read_bytes() != (fixture / f).read_bytes())
    return diffs + [f"unexpected: {f}" for f in unexpected]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bundle", type=Path, required=True, help=f"local {BUNDLE_NAME} {BUNDLE_VERSION} directory")
    ap.add_argument("--site-a", type=Path, required=True, help="local site_a root (read only)")
    ap.add_argument("--fixture", type=Path, required=True,
                    help="the site_b dataset directory (outside this application repository)")
    ap.add_argument("--contract", type=Path, default=DEFAULT_CONTRACT)
    ap.add_argument("--check", action="store_true", help="rebuild into a temp dir and require byte identity")
    a = ap.parse_args(argv)
    try:
        if a.check:
            with tempfile.TemporaryDirectory() as td:
                rep = build(a.bundle, a.site_a, a.fixture, a.contract, Path(td), DEFAULT_CONFIG)
                diffs = compare_to_fixture(Path(td), a.fixture)
            if diffs:
                print(f"CHECK FAILED: {len(diffs)} file(s) differ, first: {diffs[:5]}", file=sys.stderr)
                return 1
            print(f"CHECK OK: rebuild is byte-identical ({rep['assets']} assets, {rep['posts']} posts)")
        else:
            with tempfile.TemporaryDirectory() as td:
                rep = build(a.bundle, a.site_a, a.fixture, a.contract, Path(td), DEFAULT_CONFIG)
                for f in sorted(p.relative_to(Path(td)) for p in Path(td).rglob("*") if p.is_file()):
                    dst = a.fixture / f
                    dst.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(Path(td) / f, dst)
            stale = compare_trees_extra(a.fixture, rep)
            print(json.dumps(rep, indent=1, default=str))
            if stale:
                print(f"WARNING: files in the fixture that the build did not produce: {stale}", file=sys.stderr)
                return 1
    except BuildError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    return 0


def compare_trees_extra(fixture: Path, rep: dict) -> list[str]:
    sums = (fixture / "SHA256SUMS").read_text(encoding="utf-8").splitlines()
    listed = {l[66:] for l in sums} | {"SHA256SUMS"}
    owned, unexpected = fixture_view(fixture)
    return sorted((owned - listed) | set(unexpected))


if __name__ == "__main__":
    sys.exit(main())
