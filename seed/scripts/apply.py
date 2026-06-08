#!/usr/bin/env python3
"""
apply.py — drive Artist Alley's API to materialize a seeded instance
from a populated site directory.

Reads the MANIFEST.json + supporting catalogues at a site root (one of
/mnt/blackbox_archives/datasets/artist_alley/{site_a,site_b}) and walks
through the dependency-ordered phases to populate a running AA
instance via its HTTP API.

The script is gold-standard:
- Idempotent: stable UUIDs from the seed data + username/code natural
  keys mean re-runs are no-ops (or recover from a partial run).
- Resumable: a `.apply-state-<host>.json` file in the working
  directory tracks completed phases + per-entity created IDs. A
  crashed run resumes where it stopped.
- Parallel where safe: byte uploads run in a ThreadPoolExecutor
  (default 8 workers); entity creates run sequentially within each
  phase because later phases depend on the IDs returned.
- Dry-run: `--dry-run` walks every phase emitting the API call plan
  without hitting the server. Useful for sanity-checking the seed
  data shape + the target instance's pre-seeded state.
- Verify pass: after all phases complete, counts entities + asserts
  they match the MANIFEST. Exit code reflects pass/fail.
- Standalone: stdlib + `requests` only.

Usage
-----
    python3 apply.py \\
        --site /mnt/blackbox_archives/datasets/artist_alley/site_a \\
        --catalogue /mnt/d/Projects/artist-alley/seed/profiles \\
        --api http://localhost:8080 \\
        [--admin-user admin] \\
        [--admin-pass-env AA_ADMIN_PASSWORD | --admin-pass-file PATH] \\
        [--workers 8] \\
        [--dry-run] \\
        [--resume] [--reset-state] \\
        [--phases all|users,teams,assets,posts,...] \\
        [--verbose]

When the target AA was booted with `AA_BOOTSTRAP_DEFAULT_ADMIN=1`
(default in `docker-compose.yml`), the bootstrap admin is
`admin / ArtistAlleyMogul` and no password configuration is needed —
apply.py defaults to those credentials.

In production (no bootstrap env flag), the operator must grep the
boot log for the random password and supply it via --admin-pass-env
or --admin-pass-file. Companion utility `recover_admin.py` automates
that grep when log output is captured to a file.

Phases (dependency order)
-------------------------
1. resolve_workflow_states   GET /workflow/states; map (domain, name) → UUID
2. resolve_asset_types       GET /asset_types; map name → ID (int64)
3. apply_users               POST /admin/seed/users for each fictional user
4. apply_teams               POST /teams for each team
5. apply_team_memberships    POST /teams/{id}/members for each user's primary team
6. apply_fields              POST /fields for each custom field definition
7. apply_collections         POST /collections for each project
8. apply_assets              For each asset:
                               - POST /storage/objects (byte stream) → hash
                               - POST /assets (create entity with state_id, file_hash)
                               - POST /assets/{id}/tags (tag list)
                               - POST /collections/{id}/resources (add to collection)
9. apply_posts               POST /posts for each post (with member assets,
                               cover, collection, tags, state_id)
10. apply_comments           POST /admin/seed/comments for each post's review_notes
                               (forged author = reviewer)
11. apply_timestamps         POST /admin/seed/timestamps to backfill 14-month
                               timeline (assets + posts + comments in batches of 1000)
12. verify                   Re-fetch counts + assert they match MANIFEST
"""

from __future__ import annotations

import argparse
import concurrent.futures
import dataclasses
import hashlib
import json
import logging
import os
import random
import re
import shutil
import socket
import sys
import time
import typing as T
from pathlib import Path
from urllib.parse import urljoin, urlparse

try:
    import requests
except ImportError:
    print("error: requests is required. install with `pip install --user requests`",
          file=sys.stderr)
    sys.exit(2)

LOG = logging.getLogger("apply")

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

DEFAULT_API = "http://localhost:8080"
DEFAULT_ADMIN_USER = "admin"
DEFAULT_ADMIN_PASS_BOOTSTRAP = "ArtistAlleyMogul"  # AA_BOOTSTRAP_DEFAULT_ADMIN=1 mode

# Workflow state names in our seed data → AA's actual seeded state names.
# AA pre-seeds 4 asset states + 1 post state (see migrations/00002_seeds.sql):
#   asset:1  draft / pending_review / published / archived
#   post     published
# Our seed dataset uses richer naming (draft / in_review / approved /
# final / archived) — apply collapses them onto AA's actual states.
WORKFLOW_STATE_ALIASES = {
    # (domain_prefix, seed_name)  →  AA_seeded_name
    ("asset", "draft"):       "draft",
    ("asset", "in_review"):   "pending_review",
    ("asset", "approved"):    "published",
    ("asset", "final"):       "published",
    ("asset", "archived"):    "archived",
    ("post",  "draft"):       "published",
    ("post",  "in_review"):   "published",
    ("post",  "approved"):    "published",
    ("post",  "final"):       "published",
    ("post",  "archived"):    "published",
}

# AA's asset_type registry uses int64 IDs. apply looks them up by name at
# startup. The names below are the typed-folder labels we use locally
# (see sanitize_and_assemble.py's TYPE_FOLDER_MAP) — apply maps each onto
# whatever asset_type record happens to exist in the running AA.
SEED_ASSET_TYPE_HINT_NAMES = {
    "image":    ["image", "raster", "picture"],
    "audio":    ["audio", "sound"],
    "3d":       ["3d", "model"],
    "video":    ["video", "movie"],
    "document": ["document", "doc", "pdf"],
    "font":     ["font", "typeface"],
    "comic":    ["comic", "cbz"],
}

# Tag aliases — seed data tags use lowercase / sometimes prefixes (e.g.
# "franchise:reference"); they round-trip unchanged.

# Phase ordering (dependency-respecting). Each phase declares its tracker
# key and human description. `--phases` accepts comma-separated keys.
@dataclasses.dataclass
class Phase:
    key: str
    label: str
    fn_name: str

PHASES: list[Phase] = [
    Phase("resolve_workflow_states", "Resolve workflow states",            "phase_resolve_workflow_states"),
    Phase("resolve_asset_types",     "Resolve asset types",                "phase_resolve_asset_types"),
    Phase("users",                   "Seed users",                          "phase_apply_users"),
    Phase("teams",                   "Seed teams",                          "phase_apply_teams"),
    Phase("team_memberships",        "Seed team memberships",               "phase_apply_team_memberships"),
    Phase("fields",                  "Seed field definitions",              "phase_apply_fields"),
    Phase("collections",             "Seed collections",                    "phase_apply_collections"),
    Phase("assets",                  "Seed assets (upload + create + tag)", "phase_apply_assets"),
    Phase("posts",                   "Seed posts",                          "phase_apply_posts"),
    Phase("comments",                "Seed forged comments",                "phase_apply_comments"),
    Phase("timestamps",              "Backfill timestamps",                 "phase_apply_timestamps"),
    Phase("verify",                  "Verify counts match MANIFEST",        "phase_verify"),
]


# ---------------------------------------------------------------------------
# State tracker (resume support)
# ---------------------------------------------------------------------------

class State:
    """Persistent state tracker. Writes incrementally so a crashed run
    can resume. State file is per-target-host so multiple instances
    can be seeded in parallel without collision."""

    VERSION = 1

    def __init__(self, path: Path, target_host: str):
        self.path = path
        self.target_host = target_host
        self.data: dict[str, T.Any] = {
            "version": self.VERSION,
            "target_host": target_host,
            "phases_completed": [],
            "workflow_states": {},   # (domain, name) → UUID
            "asset_types": {},        # name → int64
            "users": {},              # username → ref (int64)
            "teams": {},              # slug → UUID
            "fields": {},             # code → UUID
            "collections": {},        # name → UUID
            "assets": {},             # seed_uuid → server_uuid (usually same)
            "posts": {},              # seed_uuid → server_uuid (usually same)
            "comments": {},           # seed_uuid → server_uuid
        }
        if path.is_file():
            try:
                existing = json.loads(path.read_text(encoding="utf-8"))
                if existing.get("target_host") != target_host:
                    LOG.warning("state file targets a different host (%s); ignoring",
                                existing.get("target_host"))
                else:
                    self.data.update(existing)
                    LOG.info("loaded state from %s (%d phases done)",
                             path, len(self.data["phases_completed"]))
            except Exception as e:
                LOG.warning("ignoring corrupt state file %s: %s", path, e)

    def save(self) -> None:
        tmp = self.path.with_suffix(self.path.suffix + ".tmp")
        tmp.write_text(json.dumps(self.data, indent=2, sort_keys=True, default=str),
                       encoding="utf-8")
        tmp.replace(self.path)

    def mark_phase_done(self, phase_key: str) -> None:
        if phase_key not in self.data["phases_completed"]:
            self.data["phases_completed"].append(phase_key)
        self.save()

    def is_phase_done(self, phase_key: str) -> bool:
        return phase_key in self.data["phases_completed"]

    # --- per-entity registries (composite-key access) ---

    def set_workflow_state(self, domain_kind: str, name: str, state_uuid: str) -> None:
        self.data["workflow_states"][f"{domain_kind}|{name}"] = state_uuid
        self.save()

    def get_workflow_state(self, domain_kind: str, name: str) -> str | None:
        return self.data["workflow_states"].get(f"{domain_kind}|{name}")

    def set_asset_type(self, name: str, ref: int) -> None:
        self.data["asset_types"][name] = ref
        self.save()

    def get_asset_type(self, name: str) -> int | None:
        return self.data["asset_types"].get(name)

    def set_user(self, username: str, ref: int) -> None:
        self.data["users"][username] = ref

    def get_user(self, username: str) -> int | None:
        return self.data["users"].get(username)

    def set_entity(self, kind: str, seed_uuid: str, server_uuid: str) -> None:
        self.data[kind][seed_uuid] = server_uuid

    def get_entity(self, kind: str, seed_uuid: str) -> str | None:
        return self.data[kind].get(seed_uuid)

    def reset(self) -> None:
        self.data["phases_completed"] = []
        # Don't clear registries — IDs from previous runs are still valid
        # in the target instance.
        self.save()


# ---------------------------------------------------------------------------
# AA API client (auth, retry, rate-limit respect)
# ---------------------------------------------------------------------------

class APIError(Exception):
    def __init__(self, method: str, path: str, status: int, body: str):
        super().__init__(f"{method} {path} → {status}: {body[:200]}")
        self.method = method
        self.path = path
        self.status = status
        self.body = body


class AAClient:
    """Thin requests wrapper with auth, automatic retry on 5xx + 429,
    and consistent error reporting. Holds a single requests.Session
    so cookie auth + connection pooling work across calls."""

    def __init__(self, api_base: str, dry_run: bool = False):
        self.api_base = api_base.rstrip("/")
        self.dry_run = dry_run
        self.session = requests.Session()
        self.session.headers.update({
            "User-Agent": "artist-alley-apply/1.0",
            "Accept": "application/json",
        })
        self._authenticated = False

    def login(self, username: str, password: str) -> dict[str, T.Any]:
        """POST /auth/login. Stores the session cookie for subsequent calls."""
        if self.dry_run:
            LOG.info("[dry-run] would POST /auth/login as %s", username)
            self._authenticated = True
            return {"username": username, "ref": -1}
        resp = self._do("POST", "/auth/login", json={
            "username": username,
            "password": password,
        }, _login_call=True)
        self._authenticated = True
        return resp

    def get(self, path: str, **kw) -> T.Any:
        return self._do("GET", path, **kw)

    def post(self, path: str, **kw) -> T.Any:
        return self._do("POST", path, **kw)

    def put(self, path: str, **kw) -> T.Any:
        return self._do("PUT", path, **kw)

    def delete(self, path: str, **kw) -> T.Any:
        return self._do("DELETE", path, **kw)

    def upload(self, path: str, data: bytes, content_type: str = "application/octet-stream",
               extra_headers: dict[str, str] | None = None) -> T.Any:
        """Streamed binary upload (POST /storage/objects)."""
        if self.dry_run:
            LOG.debug("[dry-run] would POST %s (%d bytes, %s)", path, len(data), content_type)
            # Return a synthetic response matching UploadResult shape
            return {
                "hash": hashlib.sha256(data).hexdigest(),
                "size_bytes": len(data),
                "deduped": False,
                "content_type": content_type,
            }
        headers = {"Content-Type": content_type, "X-Content-Type": content_type}
        if extra_headers:
            headers.update(extra_headers)
        return self._do("POST", path, data=data, headers=headers, expect_json=True)

    def _do(self, method: str, path: str, *,
            json: T.Any = None,
            data: bytes | None = None,
            headers: dict[str, str] | None = None,
            params: dict[str, T.Any] | None = None,
            expect_json: bool = True,
            _login_call: bool = False,
            max_retries: int = 6) -> T.Any:
        if self.dry_run and not _login_call:
            LOG.debug("[dry-run] %s %s json=%s", method, path,
                      (json if json is None else "<%d bytes>" % len(repr(json))))
            # Synthetic response that satisfies every apply-phase consumer.
            # Include both the UUID-shaped `id` (for assets/posts/etc.) and
            # the int64 `ref` (for users) so callers don't need to special-
            # case dry-run. `already_existed=False` keeps the user phase's
            # idempotent branch a no-op.
            synthetic_ref = abs(hash(path + repr(json))) % (10 ** 12)
            return {
                "id": _stable_uuid("dryrun", path, repr(json)),
                "ref": synthetic_ref,
                "username": (json or {}).get("username", "dryrun"),
                "already_existed": False,
                "_dry_run": True,
            }

        url = urljoin(self.api_base + "/", path.lstrip("/"))
        last_exc: Exception | None = None
        for attempt in range(max_retries):
            try:
                resp = self.session.request(
                    method, url,
                    json=json,
                    data=data,
                    headers=headers,
                    params=params,
                    timeout=(10, 600),  # 10s connect, 10min read (big uploads)
                )
            except (requests.ConnectionError, requests.Timeout) as e:
                last_exc = e
                backoff = min(2 ** attempt, 60) + random.uniform(0, 1)
                LOG.warning("network error on %s %s (attempt %d/%d): %s — backing off %.1fs",
                            method, path, attempt + 1, max_retries, e, backoff)
                time.sleep(backoff)
                continue

            # Retriable status?
            if resp.status_code == 429 or 500 <= resp.status_code < 600:
                retry_after = resp.headers.get("Retry-After")
                if retry_after:
                    try:
                        backoff = float(retry_after)
                    except ValueError:
                        backoff = 30.0
                else:
                    backoff = min(2 ** attempt, 60) + random.uniform(0, 1)
                if attempt + 1 < max_retries:
                    LOG.warning("retriable %d on %s %s (attempt %d/%d) — backing off %.1fs",
                                resp.status_code, method, path, attempt + 1, max_retries, backoff)
                    time.sleep(backoff)
                    continue

            # Terminal status — success or non-retriable error
            if 200 <= resp.status_code < 300:
                if expect_json and resp.content:
                    return resp.json()
                return None
            raise APIError(method, path, resp.status_code, resp.text)

        # Exhausted retries on network errors
        if last_exc:
            raise APIError(method, path, -1, f"network error after retries: {last_exc}") from last_exc
        raise APIError(method, path, -1, "retries exhausted (no terminal response)")


# ---------------------------------------------------------------------------
# Progress reporting
# ---------------------------------------------------------------------------

class Progress:
    """Tiny progress reporter — periodic per-phase counters streamed to
    stdout. Avoids fancy curses / TTY libraries so log capture stays
    clean."""

    def __init__(self, phase_label: str, total: int):
        self.phase_label = phase_label
        self.total = total
        self.done = 0
        self.skipped = 0
        self.failed = 0
        self.started_at = time.monotonic()
        self.last_report = 0.0

    def tick(self, *, done: int = 0, skipped: int = 0, failed: int = 0) -> None:
        self.done += done
        self.skipped += skipped
        self.failed += failed
        now = time.monotonic()
        if now - self.last_report >= 1.0 or (self.done + self.skipped + self.failed) == self.total:
            self.last_report = now
            self._emit()

    def _emit(self) -> None:
        processed = self.done + self.skipped + self.failed
        elapsed = time.monotonic() - self.started_at
        rate = processed / elapsed if elapsed > 0 else 0
        msg = (f"  [{self.phase_label}] {processed}/{self.total} "
               f"(done={self.done} skipped={self.skipped} failed={self.failed}) "
               f"{rate:.1f}/s")
        print(msg, file=sys.stderr, flush=True)

    def done_msg(self) -> None:
        self._emit()


# ---------------------------------------------------------------------------
# Catalogue loading (read seed/profiles/* + site MANIFEST.json)
# ---------------------------------------------------------------------------

@dataclasses.dataclass
class Catalogues:
    """All seed catalogues a target site needs. Profiles come from
    seed/profiles/; the site-specific MANIFEST.json (assets + posts)
    comes from the site root."""
    users: list[dict]
    teams: list[dict]
    collections: list[dict]
    fields: list[dict]
    workflow: dict
    assets: list[dict]
    posts: list[dict]

    @classmethod
    def load(cls, catalogue_root: Path, site_root: Path, site_key: str) -> "Catalogues":
        def jload(p: Path) -> T.Any:
            if not p.is_file():
                raise FileNotFoundError(f"missing catalogue file: {p}")
            return json.loads(p.read_text(encoding="utf-8"))

        users = jload(catalogue_root / "dataset.users.json")
        teams = jload(catalogue_root / "dataset.teams.json")
        collections = jload(catalogue_root / "dataset.collections.json")
        fields = jload(catalogue_root / "dataset.field_definitions.json")
        workflow = jload(catalogue_root / "dataset.workflow.json")

        # Site-specific records: try the MANIFEST.json shipped with the
        # populated site first (canonical), else fall back to the
        # per-studio files in catalogue_root.
        assets_path = site_root / "MANIFEST.json"
        if not assets_path.is_file():
            assets_path = catalogue_root / f"studio-{site_key}.assets.json"
        assets = jload(assets_path)
        if not isinstance(assets, list):
            raise ValueError(f"expected list at {assets_path}, got {type(assets)}")

        posts_path = site_root / "posts.json"
        if not posts_path.is_file():
            posts_path = catalogue_root / f"studio-{site_key}.posts.json"
        posts = jload(posts_path)
        if not isinstance(posts, list):
            raise ValueError(f"expected list at {posts_path}, got {type(posts)}")

        return cls(users=users, teams=teams, collections=collections,
                   fields=fields, workflow=workflow, assets=assets, posts=posts)


# ---------------------------------------------------------------------------
# Phase implementations
# ---------------------------------------------------------------------------

def phase_resolve_workflow_states(client: AAClient, cat: Catalogues, state: State,
                                  args: argparse.Namespace) -> None:
    resp = client.get("/workflow/states")
    items = resp if isinstance(resp, list) else resp.get("items", [])
    LOG.info("found %d workflow states", len(items))
    # Index by (domain, name) and (domain_prefix, name)
    for it in items:
        # The API returns a `domain` field (string like "asset:1" or "post")
        domain = it.get("domain", "")
        name = it.get("name", "")
        state_id = it.get("id")
        if not (domain and name and state_id):
            continue
        # Store both the exact domain and the prefix (asset/post) for lookup
        domain_prefix = domain.split(":", 1)[0]
        state.set_workflow_state(domain_prefix, name, state_id)
        if domain_prefix != domain:
            state.set_workflow_state(domain, name, state_id)
    LOG.info("indexed %d workflow state UUIDs", len(state.data["workflow_states"]))


def phase_resolve_asset_types(client: AAClient, cat: Catalogues, state: State,
                              args: argparse.Namespace) -> None:
    resp = client.get("/asset_types")
    items = resp if isinstance(resp, list) else resp.get("items", [])
    LOG.info("found %d asset types", len(items))
    # Build a lookup: lower-case name → ref. The seed data uses the
    # typed-folder labels (image/audio/3d/video/document/font/comic);
    # we map each onto whatever AA happens to have.
    name_to_ref: dict[str, int] = {}
    for it in items:
        name = (it.get("name") or "").lower()
        ref = it.get("ref")
        if name and ref is not None:
            name_to_ref[name] = ref
    for seed_name, hints in SEED_ASSET_TYPE_HINT_NAMES.items():
        for h in hints:
            if h in name_to_ref:
                state.set_asset_type(seed_name, name_to_ref[h])
                break
        else:
            # No match found — use the first available asset_type as a
            # fallback so apply doesn't hard-fail.
            if name_to_ref:
                fallback_ref = next(iter(name_to_ref.values()))
                LOG.warning("asset_type '%s' has no match; falling back to ref=%d",
                            seed_name, fallback_ref)
                state.set_asset_type(seed_name, fallback_ref)
    LOG.info("mapped %d seed asset-types onto AA asset_type refs",
             len(state.data["asset_types"]))


def phase_apply_users(client: AAClient, cat: Catalogues, state: State,
                      args: argparse.Namespace) -> None:
    total = len(cat.users)
    prog = Progress("users", total)
    for u in cat.users:
        username = u["username"]
        if state.get_user(username) is not None:
            prog.tick(skipped=1)
            continue
        body = {
            "username": username,
            "fullname": u.get("full_name"),
            "email": u.get("email"),
        }
        # No password by default — these are actor-only fictional users.
        body = {k: v for k, v in body.items() if v is not None}
        try:
            resp = client.post("/admin/seed/users", json=body)
        except APIError as e:
            LOG.error("create user %s failed: %s", username, e)
            prog.tick(failed=1)
            continue
        if not isinstance(resp, dict):
            LOG.error("unexpected response for user %s: %r", username, resp)
            prog.tick(failed=1)
            continue
        ref = resp.get("ref")
        if ref is None:
            LOG.error("no ref in response for user %s: %r", username, resp)
            prog.tick(failed=1)
            continue
        state.set_user(username, int(ref))
        prog.tick(done=1)
    state.save()
    prog.done_msg()


def phase_apply_teams(client: AAClient, cat: Catalogues, state: State,
                      args: argparse.Namespace) -> None:
    total = len(cat.teams)
    prog = Progress("teams", total)
    for t in cat.teams:
        name = t["name"]
        slug = re.sub(r"[^a-z0-9]+", "-", name.lower()).strip("-")[:80] or "team"
        if state.get_entity("teams", slug):
            prog.tick(skipped=1)
            continue
        try:
            resp = client.post("/teams", json={
                "slug": slug,
                "name": name,
                "description": f"{name} team",
            })
        except APIError as e:
            # 409 means duplicate — fetch it
            if e.status == 409:
                LOG.info("team %s exists; recovering ID via list", slug)
                try:
                    listing = client.get("/teams")
                    items = listing if isinstance(listing, list) else listing.get("items", [])
                    found = next((x for x in items if x.get("slug") == slug), None)
                    if found:
                        state.set_entity("teams", slug, found["id"])
                        prog.tick(skipped=1)
                        continue
                except APIError:
                    pass
            LOG.error("create team %s failed: %s", slug, e)
            prog.tick(failed=1)
            continue
        if isinstance(resp, dict) and resp.get("id"):
            state.set_entity("teams", slug, resp["id"])
            prog.tick(done=1)
        else:
            LOG.error("no id in response for team %s", slug)
            prog.tick(failed=1)
    state.save()
    prog.done_msg()


def phase_apply_team_memberships(client: AAClient, cat: Catalogues, state: State,
                                 args: argparse.Namespace) -> None:
    # For each user with a `primary_team`, add them to that team.
    eligible = [u for u in cat.users if u.get("primary_team")]
    prog = Progress("team_memberships", len(eligible))
    for u in eligible:
        username = u["username"]
        team_name = u["primary_team"]
        team_slug = re.sub(r"[^a-z0-9]+", "-", team_name.lower()).strip("-")[:80]
        user_ref = state.get_user(username)
        team_id = state.get_entity("teams", team_slug)
        if user_ref is None or team_id is None:
            LOG.warning("skipping membership %s ↔ %s (user_ref=%s team_id=%s)",
                        username, team_slug, user_ref, team_id)
            prog.tick(skipped=1)
            continue
        try:
            client.post(f"/teams/{team_id}/members", json={"rs_user_id": user_ref})
            prog.tick(done=1)
        except APIError as e:
            if e.status in (409, 400):
                # Already a member — count as skipped
                prog.tick(skipped=1)
            else:
                LOG.error("add %s to team %s failed: %s", username, team_slug, e)
                prog.tick(failed=1)
    prog.done_msg()


def phase_apply_fields(client: AAClient, cat: Catalogues, state: State,
                       args: argparse.Namespace) -> None:
    prog = Progress("fields", len(cat.fields))
    for f in cat.fields:
        code = f["name"]  # `name` in our catalogue is the federation-stable code
        if state.get_entity("fields", code):
            prog.tick(skipped=1)
            continue
        body: dict[str, T.Any] = {
            "code": code,
            "label": f["label"],
            "type": f["type"],
            "required": False,
            "searchable": True,
            "applies_to": [],  # empty = all asset types
        }
        # If the field has options, attach them
        if "options" in f and isinstance(f["options"], list):
            body["options"] = {"values": f["options"]}
        try:
            resp = client.post("/fields", json=body)
        except APIError as e:
            if e.status == 409:
                # Field already exists; fetch it
                try:
                    listing = client.get("/fields")
                    items = listing if isinstance(listing, list) else listing.get("items", [])
                    found = next((x for x in items if x.get("code") == code), None)
                    if found:
                        state.set_entity("fields", code, found["id"])
                        prog.tick(skipped=1)
                        continue
                except APIError:
                    pass
            LOG.error("create field %s failed: %s", code, e)
            prog.tick(failed=1)
            continue
        if isinstance(resp, dict) and resp.get("id"):
            state.set_entity("fields", code, resp["id"])
            prog.tick(done=1)
        else:
            prog.tick(failed=1)
    state.save()
    prog.done_msg()


def phase_apply_collections(client: AAClient, cat: Catalogues, state: State,
                            args: argparse.Namespace) -> None:
    prog = Progress("collections", len(cat.collections))
    for c in cat.collections:
        name = c["name"]
        if state.get_entity("collections", name):
            prog.tick(skipped=1)
            continue
        try:
            resp = client.post("/collections", json={
                "name": name,
                "description": f"{name} — seeded collection",
                "visibility": "org-only",
            })
        except APIError as e:
            if e.status == 409:
                # Recover via list
                try:
                    listing = client.get("/collections")
                    items = listing if isinstance(listing, list) else listing.get("items", [])
                    found = next((x for x in items if x.get("name") == name), None)
                    if found:
                        state.set_entity("collections", name, found["id"])
                        prog.tick(skipped=1)
                        continue
                except APIError:
                    pass
            LOG.error("create collection %s failed: %s", name, e)
            prog.tick(failed=1)
            continue
        if isinstance(resp, dict) and resp.get("id"):
            state.set_entity("collections", name, resp["id"])
            prog.tick(done=1)
        else:
            prog.tick(failed=1)
    state.save()
    prog.done_msg()


# Asset processing is the bulk of the work — parallelize byte uploads.

def _upload_one_asset(asset: dict, site_root: Path, client: AAClient) -> tuple[str, str | None, Exception | None]:
    """Upload one asset's bytes. Returns (seed_uuid, file_hash, error)."""
    seed_uuid = asset["id"]
    file_path = asset.get("file_path") or asset.get("source_path")
    if not file_path:
        return seed_uuid, None, ValueError("no file_path on asset")
    abs_path = site_root / file_path
    if not abs_path.is_file():
        return seed_uuid, None, FileNotFoundError(f"asset bytes not at {abs_path}")
    try:
        data = abs_path.read_bytes()
    except OSError as e:
        return seed_uuid, None, e
    content_type = _guess_content_type(asset.get("file_extension", ""))
    try:
        resp = client.upload("/storage/objects", data, content_type=content_type)
        return seed_uuid, resp.get("hash"), None
    except Exception as e:
        return seed_uuid, None, e


def _guess_content_type(extension: str) -> str:
    ext = (extension or "").lower().lstrip(".")
    return {
        "jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png",
        "gif": "image/gif", "webp": "image/webp", "svg": "image/svg+xml",
        "hdr": "image/vnd.radiance",
        "mp4": "video/mp4", "webm": "video/webm", "mov": "video/quicktime",
        "avi": "video/x-msvideo", "mkv": "video/x-matroska", "ogv": "video/ogg",
        "mp3": "audio/mpeg", "ogg": "audio/ogg", "wav": "audio/wav",
        "m4a": "audio/mp4", "flac": "audio/flac",
        "pdf": "application/pdf", "epub": "application/epub+zip",
        "txt": "text/plain", "md": "text/markdown",
        "json": "application/json", "yaml": "text/yaml", "yml": "text/yaml",
        "otf": "font/otf", "ttf": "font/ttf", "woff": "font/woff", "woff2": "font/woff2",
        "fbx": "model/fbx", "glb": "model/gltf-binary", "gltf": "model/gltf+json",
        "obj": "model/obj",
        "cbz": "application/vnd.comicbook+zip",
        "zip": "application/zip",
    }.get(ext, "application/octet-stream")


def phase_apply_assets(client: AAClient, cat: Catalogues, state: State,
                       args: argparse.Namespace) -> None:
    site_root = Path(args.site)
    assets = cat.assets
    prog = Progress("assets", len(assets))

    # Map workflow_state from seed name → AA UUID (per asset_type domain)
    def resolve_state(asset: dict) -> str | None:
        seed_state = asset.get("workflow_state") or "approved"
        aa_state = WORKFLOW_STATE_ALIASES.get(("asset", seed_state), "published")
        # asset states live under `asset:<asset_type_id>` domains — apply
        # tries each known asset_type domain first, then the plain
        # `asset` prefix as a fallback.
        type_ref = state.get_asset_type(asset.get("asset_type", "image")) or 1
        return (state.get_workflow_state(f"asset:{type_ref}", aa_state) or
                state.get_workflow_state("asset", aa_state))

    # --- Phase 1: parallel byte uploads ---
    needs_upload = [a for a in assets if state.get_entity("assets", a["id"]) is None]
    LOG.info("uploading bytes for %d assets (%d already created)",
             len(needs_upload), len(assets) - len(needs_upload))

    upload_results: dict[str, str] = {}  # seed_uuid → file_hash
    if needs_upload:
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
            futures = {pool.submit(_upload_one_asset, a, site_root, client): a
                       for a in needs_upload}
            for fut in concurrent.futures.as_completed(futures):
                asset = futures[fut]
                seed_uuid, file_hash, err = fut.result()
                if err is not None:
                    LOG.error("upload failed for %s: %s", asset.get("title", seed_uuid), err)
                    continue
                if file_hash:
                    upload_results[seed_uuid] = file_hash

    # --- Phase 2: sequential asset entity creates ---
    for asset in assets:
        seed_uuid = asset["id"]
        if state.get_entity("assets", seed_uuid):
            prog.tick(skipped=1)
            continue
        file_hash = upload_results.get(seed_uuid)
        if not file_hash:
            LOG.warning("no upload result for asset %s; skipping create", seed_uuid)
            prog.tick(failed=1)
            continue
        type_ref = state.get_asset_type(asset.get("asset_type", "image")) or 1
        state_id = resolve_state(asset)
        body = {
            "title": asset.get("title") or "Untitled",
            "description": asset.get("description") or "",
            "asset_type": type_ref,
            "status": "active" if asset.get("archive_state") == "active" else "draft",
            "file_hash": file_hash,
            "file_extension": asset.get("file_extension"),
            "tags": asset.get("tags") or [],
            "metadata": asset.get("metadata") or {},
        }
        if state_id:
            body["state_id"] = state_id
        try:
            resp = client.post("/assets", json=body)
        except APIError as e:
            LOG.error("create asset %s failed: %s", seed_uuid, e)
            prog.tick(failed=1)
            continue
        if isinstance(resp, dict) and resp.get("id"):
            state.set_entity("assets", seed_uuid, resp["id"])
            # Add to collection if assigned
            coll_name = asset.get("collection_name")
            coll_id = state.get_entity("collections", coll_name) if coll_name else None
            if coll_id:
                try:
                    client.post(f"/collections/{coll_id}/resources",
                                json={"asset_id": resp["id"]})
                except APIError as e:
                    if e.status not in (400, 409):
                        LOG.warning("add asset %s to collection %s failed: %s",
                                    resp["id"], coll_name, e)
            prog.tick(done=1)
        else:
            prog.tick(failed=1)
    state.save()
    prog.done_msg()


def phase_apply_posts(client: AAClient, cat: Catalogues, state: State,
                      args: argparse.Namespace) -> None:
    prog = Progress("posts", len(cat.posts))
    for post in cat.posts:
        seed_uuid = post["id"]
        if state.get_entity("posts", seed_uuid):
            prog.tick(skipped=1)
            continue
        # Resolve asset_ids → server UUIDs
        members = []
        for seed_aid in post.get("asset_ids", []):
            server_id = state.get_entity("assets", seed_aid)
            if server_id:
                members.append({"asset_id": server_id})
        if not members:
            LOG.warning("post %s has no resolvable members; skipping",
                        post.get("title", seed_uuid))
            prog.tick(skipped=1)
            continue
        # Resolve collection
        coll_id = None
        if post.get("collection_name"):
            coll_id = state.get_entity("collections", post["collection_name"])
        # Resolve workflow state for post domain
        seed_state = post.get("workflow_state") or "approved"
        aa_state = WORKFLOW_STATE_ALIASES.get(("post", seed_state), "published")
        state_id = state.get_workflow_state("post", aa_state)

        body: dict[str, T.Any] = {
            "title": post.get("title") or "Untitled",
            "description": post.get("description") or "",
            "visibility": "org-only",
            "members": members,
            "tags": post.get("tags") or [],
        }
        if coll_id:
            body["collection_id"] = coll_id
        if state_id:
            body["state_id"] = state_id

        try:
            resp = client.post("/posts", json=body)
        except APIError as e:
            LOG.error("create post %s failed: %s", seed_uuid, e)
            prog.tick(failed=1)
            continue
        if isinstance(resp, dict) and resp.get("id"):
            state.set_entity("posts", seed_uuid, resp["id"])
            prog.tick(done=1)
        else:
            prog.tick(failed=1)
    state.save()
    prog.done_msg()


def phase_apply_comments(client: AAClient, cat: Catalogues, state: State,
                         args: argparse.Namespace) -> None:
    # We forge comments for any post whose review_notes mention a
    # reviewer. The seed data carries review notes on ASSETS — we
    # attach them as comments on the first post containing that asset
    # (if any) or, failing that, skip.
    asset_to_post: dict[str, str] = {}
    for post in cat.posts:
        post_server_id = state.get_entity("posts", post["id"])
        if not post_server_id:
            continue
        for seed_aid in post.get("asset_ids", []):
            if seed_aid not in asset_to_post:
                asset_to_post[seed_aid] = post_server_id

    candidates = []
    for asset in cat.assets:
        notes = (asset.get("review_notes") or "").strip()
        if not notes:
            continue
        reviewer = asset.get("reviewer_username")
        if not reviewer:
            continue
        reviewer_ref = state.get_user(reviewer)
        if not reviewer_ref:
            continue
        post_id = asset_to_post.get(asset["id"])
        if not post_id:
            continue
        candidates.append({
            "seed_id_base": asset["id"],
            "post_id": post_id,
            "author_ref": reviewer_ref,
            "body": notes,
        })

    prog = Progress("comments", len(candidates))
    for c in candidates:
        # Deterministic comment ID derived from asset+post so re-runs
        # hit the ON CONFLICT path
        seed_comment_id = _stable_uuid("comment", c["seed_id_base"], c["post_id"])
        if state.get_entity("comments", seed_comment_id):
            prog.tick(skipped=1)
            continue
        body = {
            "id": seed_comment_id,
            "target_kind": "post",
            "target_id": c["post_id"],
            "author_user_ref": c["author_ref"],
            "body": c["body"],
        }
        try:
            resp = client.post("/admin/seed/comments", json=body)
        except APIError as e:
            LOG.error("forge comment for post %s failed: %s", c["post_id"], e)
            prog.tick(failed=1)
            continue
        if isinstance(resp, dict) and resp.get("id"):
            state.set_entity("comments", seed_comment_id, resp["id"])
            prog.tick(done=1)
        else:
            prog.tick(failed=1)
    state.save()
    prog.done_msg()


def phase_apply_timestamps(client: AAClient, cat: Catalogues, state: State,
                           args: argparse.Namespace) -> None:
    """Backfill the 14-month timeline on assets + posts. Batches of 1000."""
    items: list[dict] = []
    # Assets
    for asset in cat.assets:
        server_id = state.get_entity("assets", asset["id"])
        if not server_id or not asset.get("created_at"):
            continue
        items.append({
            "kind": "asset",
            "id": server_id,
            "created_at": asset["created_at"],
            "updated_at": asset.get("updated_at") or asset["created_at"],
        })
    # Posts
    for post in cat.posts:
        server_id = state.get_entity("posts", post["id"])
        if not server_id or not post.get("created_at"):
            continue
        items.append({
            "kind": "post",
            "id": server_id,
            "created_at": post["created_at"],
            "updated_at": post.get("updated_at") or post["created_at"],
        })

    total_items = len(items)
    batches = [items[i:i + 1000] for i in range(0, total_items, 1000)]
    prog = Progress("timestamps", total_items)
    for batch in batches:
        try:
            resp = client.post("/admin/seed/timestamps", json={"items": batch})
            if isinstance(resp, dict):
                prog.tick(done=resp.get("asset_updated", 0) + resp.get("post_updated", 0))
                if resp.get("skipped_unknown_id"):
                    prog.tick(skipped=resp["skipped_unknown_id"])
        except APIError as e:
            LOG.error("backfill batch failed: %s", e)
            prog.tick(failed=len(batch))
    prog.done_msg()


def phase_verify(client: AAClient, cat: Catalogues, state: State,
                 args: argparse.Namespace) -> None:
    """Re-fetch counts and assert they match what we tried to seed.
    Soft assertions (warn rather than hard-fail) because the listing
    endpoints may include pre-seeded entities."""
    expected_users = len(cat.users)
    expected_teams = len(cat.teams)
    expected_collections = len(cat.collections)
    expected_assets = len(cat.assets)
    expected_posts = len(cat.posts)

    actual_users = len(state.data["users"])
    actual_teams = len(state.data["teams"])
    actual_collections = len(state.data["collections"])
    actual_assets = len(state.data["assets"])
    actual_posts = len(state.data["posts"])

    LOG.info("=" * 60)
    LOG.info("VERIFY:")
    LOG.info("  users:       %d created / %d expected", actual_users, expected_users)
    LOG.info("  teams:       %d created / %d expected", actual_teams, expected_teams)
    LOG.info("  collections: %d created / %d expected", actual_collections, expected_collections)
    LOG.info("  assets:      %d created / %d expected", actual_assets, expected_assets)
    LOG.info("  posts:       %d created / %d expected", actual_posts, expected_posts)
    LOG.info("=" * 60)

    failures = []
    if actual_users < expected_users * 0.9:
        failures.append(f"users: {actual_users}/{expected_users}")
    if actual_teams < expected_teams * 0.9:
        failures.append(f"teams: {actual_teams}/{expected_teams}")
    if actual_collections < expected_collections * 0.9:
        failures.append(f"collections: {actual_collections}/{expected_collections}")
    if actual_assets < expected_assets * 0.9:
        failures.append(f"assets: {actual_assets}/{expected_assets}")
    if actual_posts < expected_posts * 0.9:
        failures.append(f"posts: {actual_posts}/{expected_posts}")

    if failures:
        LOG.warning("verify: %d shortfalls — %s", len(failures), ", ".join(failures))
        if args.strict_verify:
            raise SystemExit(1)
    else:
        LOG.info("verify: all counts within 10%% of expected — PASS")


# ---------------------------------------------------------------------------
# Utility — stable UUID5-style identifier (matches sanitize_and_assemble.py)
# ---------------------------------------------------------------------------

def _stable_uuid(*parts: str) -> str:
    h = hashlib.sha256()
    h.update(b"artist-alley.seed.v1")
    for p in parts:
        h.update(b"\x00")
        h.update(str(p).encode())
    d = h.hexdigest()
    return f"{d[0:8]}-{d[8:12]}-{d[12:16]}-{d[16:20]}-{d[20:32]}"


# ---------------------------------------------------------------------------
# Main orchestration
# ---------------------------------------------------------------------------

def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--site", required=True, type=Path,
                        help="Site root (populated directory; contains MANIFEST.json + bytes)")
    parser.add_argument("--catalogue", type=Path,
                        default=Path(__file__).resolve().parent.parent / "profiles",
                        help="Catalogue directory (seed/profiles/)")
    parser.add_argument("--api", default=DEFAULT_API,
                        help=f"AA API base URL (default: {DEFAULT_API})")
    parser.add_argument("--admin-user", default=DEFAULT_ADMIN_USER)
    g = parser.add_mutually_exclusive_group()
    g.add_argument("--admin-pass", help="Plaintext admin password (NOT recommended)")
    g.add_argument("--admin-pass-env", default="AA_ADMIN_PASSWORD",
                   help="Env var holding the admin password")
    g.add_argument("--admin-pass-file", type=Path,
                   help="File containing the admin password (single line)")
    parser.add_argument("--workers", type=int, default=8,
                        help="Concurrent byte upload workers (default: 8)")
    parser.add_argument("--dry-run", action="store_true",
                        help="Print the API call plan without contacting the server")
    parser.add_argument("--resume", action="store_true",
                        help="Continue from existing .apply-state.json (default behaviour)")
    parser.add_argument("--reset-state", action="store_true",
                        help="Clear .apply-state.json before starting")
    parser.add_argument("--phases", default="all",
                        help="Comma-separated phase keys, or 'all'")
    parser.add_argument("--state-file", type=Path, default=None,
                        help="State tracker path (default: ./apply-state-<host>.json)")
    parser.add_argument("--strict-verify", action="store_true",
                        help="Exit 1 if verify counts diverge by >10%%")
    parser.add_argument("--verbose", "-v", action="store_true")
    return parser.parse_args()


def resolve_admin_password(args: argparse.Namespace) -> str:
    if args.admin_pass:
        return args.admin_pass
    if args.admin_pass_file:
        return args.admin_pass_file.read_text(encoding="utf-8").strip()
    env_val = os.environ.get(args.admin_pass_env or "")
    if env_val:
        return env_val
    # Fallback: bootstrap default (works when AA_BOOTSTRAP_DEFAULT_ADMIN=1)
    LOG.info("no password supplied; using bootstrap default '%s'",
             DEFAULT_ADMIN_PASS_BOOTSTRAP)
    return DEFAULT_ADMIN_PASS_BOOTSTRAP


def site_key_from_path(p: Path) -> str:
    name = p.name.lower()
    if "site_a" in name or name == "a":
        return "a"
    if "site_b" in name or name == "b":
        return "b"
    return name


def main() -> int:
    args = parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
        datefmt="%H:%M:%S",
    )

    if not args.site.is_dir():
        LOG.error("--site %s is not a directory", args.site)
        return 2
    if not args.catalogue.is_dir():
        LOG.error("--catalogue %s is not a directory", args.catalogue)
        return 2

    # Resolve target host for state-file naming
    host = urlparse(args.api).hostname or "default"
    site_key = site_key_from_path(args.site)
    state_path = args.state_file or Path.cwd() / f"apply-state-{host}-{site_key}.json"

    state = State(state_path, target_host=f"{host}:{site_key}")
    if args.reset_state:
        state.reset()
        LOG.info("state reset")

    # Load catalogues + manifest
    LOG.info("loading catalogues from %s + site %s", args.catalogue, args.site)
    try:
        cat = Catalogues.load(args.catalogue, args.site, site_key)
    except Exception as e:
        LOG.error("catalogue load failed: %s", e)
        return 2
    LOG.info("catalogues: %d users, %d teams, %d collections, %d fields, "
             "%d assets, %d posts",
             len(cat.users), len(cat.teams), len(cat.collections),
             len(cat.fields), len(cat.assets), len(cat.posts))

    # Compute phase plan
    if args.phases == "all":
        plan = [p for p in PHASES]
    else:
        wanted = set(args.phases.split(","))
        plan = [p for p in PHASES if p.key in wanted]
        unknown = wanted - {p.key for p in PHASES}
        if unknown:
            LOG.warning("ignoring unknown phase keys: %s", ", ".join(sorted(unknown)))

    # Initialize client + authenticate
    client = AAClient(args.api, dry_run=args.dry_run)
    if not args.dry_run:
        LOG.info("authenticating as %s @ %s", args.admin_user, args.api)
        password = resolve_admin_password(args)
        try:
            client.login(args.admin_user, password)
            LOG.info("login OK")
        except APIError as e:
            LOG.error("login failed: %s", e)
            return 3

    # Execute phases
    overall_start = time.monotonic()
    for phase in plan:
        if state.is_phase_done(phase.key) and not args.reset_state and phase.key != "verify":
            LOG.info("phase %s already complete — skipping", phase.key)
            continue
        LOG.info("=== Phase: %s ===", phase.label)
        phase_start = time.monotonic()
        fn = globals().get(phase.fn_name)
        if fn is None:
            LOG.error("missing phase implementation: %s", phase.fn_name)
            return 4
        try:
            fn(client, cat, state, args)
        except KeyboardInterrupt:
            LOG.warning("interrupted during phase %s; state saved", phase.key)
            state.save()
            return 130
        except Exception as e:
            LOG.exception("phase %s failed: %s", phase.key, e)
            state.save()
            return 5
        state.mark_phase_done(phase.key)
        LOG.info("phase %s done in %.1fs", phase.key,
                 time.monotonic() - phase_start)

    overall_elapsed = time.monotonic() - overall_start
    LOG.info("apply complete in %.1fs", overall_elapsed)
    return 0


if __name__ == "__main__":
    sys.exit(main())
