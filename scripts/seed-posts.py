#!/usr/bin/env python3
# scripts/seed-posts.py
#
# Bulk-seed varied Posts from a local image dir, for browse-page testing.
#
# Produces a mix of:
#   - single-asset posts (most common — one finished piece per post)
#   - multi-asset posts (carousels — testing the grouped-asset case)
#   - posts in named collections (manual collections + "featured")
#   - tags derived from filename tokens (set code prefix, "pokemon")
#
# Per-file flow uses the existing two-step upload chain plus the Post
# create: POST /storage/objects -> POST /assets -> (collect ids) ->
# POST /posts with members[].asset_id.
#
# Usage:
#   AA_SESSION=$(...) python3 scripts/seed-posts.py \
#     /mnt/d/Projects/Snapdex/datasets/cards 200
#
# The first positional arg is the source dir; second is the target
# post count (default 200). Auth via AA_SESSION env or --session.

import argparse
import json
import os
import random
import re
import sys
import time
import urllib.request
import urllib.error
from pathlib import Path

IMAGE_EXTS = {".jpg", ".jpeg", ".png", ".gif", ".webp"}
DEFAULT_BASE = "http://localhost:8088"

# Distribution: 200 posts → 150 single + 40 grouped (2–5 members each)
# + 10 special (varied visibility + custom titles). Total card usage
# averages ~150 + 40*3 + 10 ≈ 280 cards.
SINGLE_RATIO   = 0.75
GROUPED_RATIO  = 0.20
SPECIAL_RATIO  = 0.05
GROUP_SIZE_MIN = 2
GROUP_SIZE_MAX = 5

# Collections we'll create + populate.
COLLECTIONS = [
    {"name": "Featured",       "description": "Hand-picked highlights",                "visibility": "public",  "post_ratio": 0.10},
    {"name": "Recent uploads", "description": "Everything from the last seeding run",  "visibility": "public",  "post_ratio": 0.20},
    {"name": "Carousels",      "description": "Multi-asset posts",                     "visibility": "public",  "post_ratio": 1.00},
]


def parse_args():
    p = argparse.ArgumentParser(description="Seed varied posts from a local image dir.")
    p.add_argument("src", help="source directory of images")
    p.add_argument("count", nargs="?", type=int, default=200, help="target post count")
    p.add_argument("--session", default=os.environ.get("AA_SESSION", ""),
                   help="auth session (or set AA_SESSION env)")
    p.add_argument("--base-url", default=DEFAULT_BASE)
    p.add_argument("--seed", type=int, default=42, help="rng seed for reproducible variety")
    return p.parse_args()


def http_request(method, url, headers=None, data=None, ct=None):
    """Bare-bones request via stdlib to avoid the requests dependency."""
    h = headers.copy() if headers else {}
    if ct:
        h["Content-Type"] = ct
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    try:
        with urllib.request.urlopen(req) as resp:
            body = resp.read()
            return resp.status, body
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def content_type_for(filename):
    e = filename.lower().rsplit(".", 1)[-1]
    return {
        "jpg": "image/jpeg", "jpeg": "image/jpeg",
        "png": "image/png", "gif": "image/gif", "webp": "image/webp",
    }.get(e, "application/octet-stream")


def title_from_filename(name):
    # sv3-5_en_001_std → "Sv3-5 En 001 Std"  (rough, friendly)
    stem = Path(name).stem
    parts = re.split(r"[_\-]", stem)
    return " ".join(p.capitalize() for p in parts if p)


def set_code_tag(name):
    # First token before the first underscore is the set code in this
    # dataset (sv3-5, base1, neo2, …). Useful as a filter tag.
    stem = Path(name).stem
    m = re.match(r"([A-Za-z0-9-]+)", stem)
    return m.group(1).lower() if m else None


class Seeder:
    def __init__(self, base_url, session):
        self.base = base_url.rstrip("/")
        self.session = session
        self.cookie = {"Cookie": f"user={session}"}
        self.json_h = {**self.cookie, "Content-Type": "application/json"}
        self.stats = {"uploads": 0, "assets": 0, "posts": 0,
                      "collections": 0, "errors": 0}

    # ---- low-level ----
    def upload(self, path):
        with open(path, "rb") as f:
            data = f.read()
        ct = content_type_for(path.name)
        headers = {**self.cookie, "X-Content-Type": ct}
        status, body = http_request(
            "POST", f"{self.base}/api/v1/storage/objects",
            headers=headers, data=data, ct="application/octet-stream",
        )
        if status != 201:
            raise RuntimeError(f"upload {path.name}: HTTP {status}: {body[:120]!r}")
        self.stats["uploads"] += 1
        return json.loads(body)["hash"]

    def create_asset(self, title, file_hash, ext, tags=None):
        body = {
            "title": title,
            "asset_type": 1,  # Photo
            "status": "active",
            "file_hash": file_hash,
            "file_extension": ext,
            "tags": tags or [],
        }
        status, resp = http_request(
            "POST", f"{self.base}/api/v1/assets",
            headers=self.json_h, data=json.dumps(body).encode(),
            ct="application/json",
        )
        if status != 201:
            raise RuntimeError(f"asset create: HTTP {status}: {resp[:120]!r}")
        self.stats["assets"] += 1
        return json.loads(resp)["id"]

    def create_post(self, title, description, asset_ids, tags=None,
                    visibility="public", collection_id=None):
        members = [{"asset_id": aid, "sort_order": i} for i, aid in enumerate(asset_ids)]
        body = {
            "title": title,
            "description": description,
            "visibility": visibility,
            "members": members,
            "tags": tags or [],
        }
        if collection_id:
            body["collection_id"] = collection_id
        status, resp = http_request(
            "POST", f"{self.base}/api/v1/posts",
            headers=self.json_h, data=json.dumps(body).encode(),
            ct="application/json",
        )
        if status != 201:
            raise RuntimeError(f"post create: HTTP {status}: {resp[:160]!r}")
        self.stats["posts"] += 1
        return json.loads(resp)["id"]

    def create_collection(self, name, description, visibility):
        body = {"name": name, "description": description, "visibility": visibility}
        status, resp = http_request(
            "POST", f"{self.base}/api/v1/collections",
            headers=self.json_h, data=json.dumps(body).encode(),
            ct="application/json",
        )
        if status != 201:
            raise RuntimeError(f"collection create: HTTP {status}: {resp[:120]!r}")
        self.stats["collections"] += 1
        return json.loads(resp)["id"]

    def add_post_to_collection(self, collection_id, post_id):
        # collection_posts isn't yet exposed via API — use collection_id
        # passed at post-create instead. Fallback DB path would go here.
        raise NotImplementedError("set collection_id at post-create time")

    # ---- high-level: upload a file, create asset, return id+ctx ----
    def upload_card(self, path):
        h = self.upload(path)
        title = title_from_filename(path.name)
        ext = path.suffix.lstrip(".")
        set_tag = set_code_tag(path.name)
        tags = ["pokemon"] + ([set_tag] if set_tag else [])
        aid = self.create_asset(title, h, ext, tags=tags)
        return {"id": aid, "title": title, "set_tag": set_tag, "filename": path.name}


def main():
    args = parse_args()
    if not args.session:
        print("error: AA_SESSION env or --session required", file=sys.stderr)
        return 2

    src = Path(args.src)
    if not src.is_dir():
        print(f"error: source dir not found: {src}", file=sys.stderr)
        return 2

    files = sorted([p for p in src.rglob("*") if p.is_file() and p.suffix.lower() in IMAGE_EXTS])
    if not files:
        print(f"no images found under {src}", file=sys.stderr)
        return 2

    rng = random.Random(args.seed)
    rng.shuffle(files)

    target = args.count
    n_special = max(1, int(target * SPECIAL_RATIO))
    n_grouped = max(1, int(target * GROUPED_RATIO))
    n_single  = target - n_grouped - n_special

    # Plan group sizes upfront so we know how many files we need.
    group_sizes = [rng.randint(GROUP_SIZE_MIN, GROUP_SIZE_MAX) for _ in range(n_grouped)]
    files_needed = n_single + sum(group_sizes) + n_special
    if files_needed > len(files):
        print(f"need {files_needed} files but only {len(files)} available — capping", file=sys.stderr)
        scale = len(files) / files_needed
        n_single  = int(n_single * scale)
        group_sizes = [max(2, int(s * scale)) for s in group_sizes[:max(1, int(n_grouped * scale))]]
        n_grouped = len(group_sizes)
        n_special = int(n_special * scale)
        files_needed = n_single + sum(group_sizes) + n_special

    files = files[:files_needed]
    print(f"==> plan: {n_single} single + {n_grouped} grouped + {n_special} special = {n_single + n_grouped + n_special} posts ({files_needed} cards)")

    s = Seeder(args.base_url, args.session)

    # Create collections first.
    print("==> creating collections")
    coll_ids = {}
    for c in COLLECTIONS:
        coll_ids[c["name"]] = s.create_collection(c["name"], c["description"], c["visibility"])
        print(f"   - {c['name']}: {coll_ids[c['name']][:8]}")

    # Upload all the cards and create assets up-front. Each asset can
    # then be sliced into single / grouped posts however we want.
    print(f"==> uploading {files_needed} card images")
    cards = []
    t0 = time.time()
    for i, p in enumerate(files, start=1):
        try:
            cards.append(s.upload_card(p))
        except Exception as e:
            s.stats["errors"] += 1
            print(f"  err {p.name}: {e}", file=sys.stderr)
            continue
        if i % 25 == 0 or i == files_needed:
            elapsed = time.time() - t0
            rate = i / elapsed if elapsed > 0 else 0
            print(f"  [{i:4d}/{files_needed}] rate={rate:.1f}/s  ok={s.stats['assets']} err={s.stats['errors']}")

    cards_iter = iter(cards)
    def take(n):
        out = []
        for _ in range(n):
            try:
                out.append(next(cards_iter))
            except StopIteration:
                break
        return out

    # Featured collection gets a slice of all posts.
    featured_target = int((n_single + n_grouped + n_special) * 0.10)
    recent_target   = int((n_single + n_grouped + n_special) * 0.20)
    posts_for_featured = 0
    posts_for_recent = 0
    total_posts = 0

    def pick_collection_for_post(is_grouped):
        """Distribute new posts into collections."""
        nonlocal posts_for_featured, posts_for_recent
        if is_grouped:
            return coll_ids["Carousels"]  # every grouped post → Carousels
        # Mix of Featured + Recent + neither.
        r = rng.random()
        if posts_for_featured < featured_target and r < 0.10:
            posts_for_featured += 1
            return coll_ids["Featured"]
        if posts_for_recent < recent_target and r < 0.30:
            posts_for_recent += 1
            return coll_ids["Recent uploads"]
        return None

    # 1. single-asset posts
    print(f"==> creating {n_single} single-asset posts")
    for i in range(n_single):
        batch = take(1)
        if not batch:
            break
        c = batch[0]
        collection_id = pick_collection_for_post(False)
        title = c["title"]
        # Sprinkle some private posts so visibility is exercised.
        visibility = "private" if rng.random() < 0.03 else "public"
        try:
            s.create_post(
                title=title,
                description=f"Single card upload — {c['filename']}",
                asset_ids=[c["id"]],
                tags=["pokemon"] + ([c["set_tag"]] if c["set_tag"] else []),
                visibility=visibility,
                collection_id=collection_id,
            )
            total_posts += 1
        except Exception as e:
            s.stats["errors"] += 1
            print(f"  err post: {e}", file=sys.stderr)

    # 2. grouped posts
    print(f"==> creating {n_grouped} grouped posts (carousel)")
    set_themes = ["Carousel", "Set highlights", "Card lineup", "Display", "Selection"]
    for i, size in enumerate(group_sizes):
        batch = take(size)
        if not batch:
            break
        title = f"{rng.choice(set_themes)} {i+1}"
        tags = list({"pokemon"} | {c["set_tag"] for c in batch if c["set_tag"]})
        try:
            s.create_post(
                title=title,
                description=f"{len(batch)}-card grouping",
                asset_ids=[c["id"] for c in batch],
                tags=tags,
                visibility="public",
                collection_id=coll_ids["Carousels"],
            )
            total_posts += 1
        except Exception as e:
            s.stats["errors"] += 1
            print(f"  err grouped post: {e}", file=sys.stderr)

    # 3. special posts — mix of visibilities, one without title, one
    # with a long description.
    print(f"==> creating {n_special} special posts")
    specials = [
        {"title": "",                   "description": "untitled post — falls back to placeholder title in UI"},
        {"title": "Long-form description test", "description": "  ".join(["Pokemon TCG card showcase."] * 5)},
        {"title": "Private draft",      "description": "owner-only post", "visibility": "private"},
        {"title": "Followers-only",     "description": "scoped to followers (treated as public until 1.13.I)", "visibility": "followers"},
        {"title": "Tagged heavily",     "description": "five tags", "tags_extra": ["fire", "water", "grass", "psychic", "fighting"]},
        {"title": "No tags",            "description": "explicitly tagless post", "tags_extra": []},
        {"title": "Untitled - Featured","description": "shows in Featured", "collection": "Featured"},
        {"title": "Untitled - Recent",  "description": "shows in Recent",   "collection": "Recent uploads"},
        {"title": "Cover override demo","description": "cover_asset_id will default to first member",},
        {"title": "Mixed-set lineup",   "description": "deliberately scrambled across sets"},
    ]
    for spec in specials[:n_special]:
        batch = take(1)
        if not batch:
            break
        c = batch[0]
        tags = (spec.get("tags_extra")
                if "tags_extra" in spec
                else (["pokemon"] + ([c["set_tag"]] if c["set_tag"] else [])))
        try:
            s.create_post(
                title=spec.get("title", c["title"]),
                description=spec.get("description", ""),
                asset_ids=[c["id"]],
                tags=tags,
                visibility=spec.get("visibility", "public"),
                collection_id=coll_ids.get(spec.get("collection", "")),
            )
            total_posts += 1
        except Exception as e:
            s.stats["errors"] += 1
            print(f"  err special: {e}", file=sys.stderr)

    elapsed = time.time() - t0
    print(f"\n==> done in {elapsed:.1f}s")
    print(f"   uploads:     {s.stats['uploads']}")
    print(f"   assets:      {s.stats['assets']}")
    print(f"   posts:       {s.stats['posts']} ({total_posts} planned)")
    print(f"   collections: {s.stats['collections']}")
    print(f"   errors:      {s.stats['errors']}")
    return 0 if s.stats["errors"] == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
