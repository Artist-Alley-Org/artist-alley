#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Kenneth Blossom
"""
scripts/ci/scale_expected.py: the expected sets for the full-corpus scale check (#1514, ui-scale.yml).

Run once, after `aa seed` and BEFORE the dataset copy is released, against the job's OWN disposable
postgres. It writes one JSON file that tests/scale/*.spec.ts read through SCALE_EXPECTED, and it fails
closed (exit 1, with the numbers) when the seeded instance is not the one the specs are about to judge.

What it records, and why each piece is independent of the API under test:

  manifest      row counts read from the verified site root's own MANIFEST.json and posts.json
  seed_report   the seeder's last stdout line, `seed complete: users=.. assets=.. posts=..`
  db            counts read here, in a READ ONLY transaction, straight from the tables
  admin         every live asset id and every live published post id, in the API's keyset order
                (assets: created_at DESC, id DESC; posts: posted_at DESC, id DESC). The bootstrap admin
                holds system.admin, so the read rules admit every one of these rows (see the checks below
                for the assumptions that makes, each verified rather than assumed)
  principals    per named seeded user: its ref, its live assets minus the mature rows it does not own
                (the only listing exclusion besides deletion), and the published posts it authored

Checks (any failure exits 1 and writes nothing):

  - exactly one `seed complete:` line, and no `NOT a clean reseed` line
  - seed_report assets/posts == the live counts in the database
  - live assets == MANIFEST.json rows (no row missing, none deduplicated away)
  - live posts >= posts.json rows (the collection-post backfill may add posts, never remove one), and
    every live post is published (the default listing hides unpublished ones)
  - every live post's visibility is 'public' or 'org-only' (the seeder writes only those two; the admin
    expected set is only the admin's visible set while that holds)
  - zero per-asset preview jobs (the seed ran with --previews=false); the server's one boot-time
    thumbhash backfill job is reported, not counted
  - every principal exists and is approved: seeded users come from the app catalogue
    (seed/profiles/dataset.users.json), not from the site's MANIFEST, so presence is checked here

It never writes to the database, never prints a credential (it handles none) and reads no private path.
Stdlib only; run with `python3 -I`.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

SCHEMA = "artist-alley.ui-scale-expected/v1"
DEFAULT_PSQL = ("docker compose exec -T postgres "
                "psql -U artist_alley -d artist_alley -X -q -At -v ON_ERROR_STOP=1")
USERNAME_RE = re.compile(r"\A[a-z0-9][a-z0-9._-]{0,49}\Z")
SEED_LINE_RE = re.compile(
    r"^seed complete: users=(\d+) teams=(\d+) collections=(\d+) assets=(\d+) posts=(\d+) comments=(\d+)\s*$")
PUBLISHED = ("(SELECT id FROM workflow_states WHERE domain = 'post' AND code = 'published')")
ALLOWED_VISIBILITY = {"public", "org-only"}
THUMBHASH_BACKFILL = "preview.thumbhash_backfill"


class Refused(Exception):
    pass


def psql_cmd() -> list[str]:
    return shlex.split(os.environ.get("AA_SCALE_PSQL", DEFAULT_PSQL))


def query(sql: str, **variables: str) -> list[list[str]]:
    """One read-only transaction per query, through psql on stdin so `:'name'` variables are quoted by psql."""
    cmd = psql_cmd() + ["-F", "\t"]
    for k, v in variables.items():
        cmd += ["-v", f"{k}={v}"]
    script = f"BEGIN TRANSACTION READ ONLY;\n{sql};\nCOMMIT;\n"
    r = subprocess.run(cmd, input=script, capture_output=True, text=True, check=False)
    if r.returncode != 0:
        raise Refused(f"psql failed ({r.returncode}): {r.stderr.strip()[:500]}")
    return [ln.split("\t") for ln in r.stdout.splitlines() if ln != ""]


def one_int(sql: str, **v: str) -> int:
    rows = query(sql, **v)
    if len(rows) != 1 or len(rows[0]) != 1:
        raise Refused(f"expected one value from: {sql[:120]}")
    return int(rows[0][0])


def ids(sql: str, **v: str) -> list[str]:
    out = [r[0] for r in query(sql, **v)]
    if len(set(out)) != len(out):
        raise Refused(f"duplicate ids from: {sql[:120]}")
    return out


def rows_of(doc) -> list:
    """MANIFEST.json / posts.json are either a bare list or an object holding one list."""
    if isinstance(doc, list):
        return doc
    if isinstance(doc, dict):
        for k in ("assets", "posts", "items"):
            if isinstance(doc.get(k), list):
                return doc[k]
    raise Refused("unrecognised MANIFEST.json / posts.json shape")


def read_seed_report(path: Path) -> dict:
    text = path.read_text(encoding="utf-8", errors="replace")
    if "NOT a clean reseed" in text:
        raise Refused("the seed log says NOT a clean reseed; a fresh stack must seed cleanly")
    hits = [m for m in (SEED_LINE_RE.match(ln) for ln in text.splitlines()) if m]
    if len(hits) != 1:
        raise Refused(f"expected exactly one 'seed complete:' line in {path}, found {len(hits)}")
    keys = ("users", "teams", "collections", "assets", "posts", "comments")
    return dict(zip(keys, (int(x) for x in hits[0].groups())))


def build(site: str, site_root: Path, seed_log: Path, principals: list[str]) -> dict:
    manifest = {
        "assets": len(rows_of(json.loads((site_root / "MANIFEST.json").read_text(encoding="utf-8")))),
        "posts": len(rows_of(json.loads((site_root / "posts.json").read_text(encoding="utf-8")))),
    }
    seed = read_seed_report(seed_log)

    db = {
        "assets_live": one_int("SELECT count(*) FROM assets WHERE deleted_at IS NULL"),
        "posts_live": one_int("SELECT count(*) FROM posts WHERE deleted_at IS NULL"),
        "posts_live_unpublished": one_int(
            f"SELECT count(*) FROM posts WHERE deleted_at IS NULL AND state_id IS DISTINCT FROM {PUBLISHED}"),
        "assets_mature": one_int("SELECT count(*) FROM assets WHERE deleted_at IS NULL AND mature"),
        "posts_mature": one_int("SELECT count(*) FROM posts WHERE deleted_at IS NULL AND mature"),
        "users": one_int('SELECT count(*) FROM "user"'),
        "posts_by_visibility": {r[0]: int(r[1]) for r in query(
            "SELECT visibility, count(*) FROM posts WHERE deleted_at IS NULL GROUP BY 1 ORDER BY 1")},
        "jobs_by_type": {r[0]: int(r[1]) for r in query("SELECT type, count(*) FROM jobs GROUP BY 1 ORDER BY 1")},
    }
    db["per_asset_preview_jobs"] = sum(n for t, n in db["jobs_by_type"].items()
                                       if t.startswith("preview.") and t != THUMBHASH_BACKFILL)

    problems = []
    if seed["assets"] != db["assets_live"]:
        problems.append(f"seed report assets={seed['assets']} but the database holds {db['assets_live']} live assets")
    if seed["posts"] != db["posts_live"]:
        problems.append(f"seed report posts={seed['posts']} but the database holds {db['posts_live']} live posts")
    if db["assets_live"] != manifest["assets"]:
        problems.append(f"{db['assets_live']} live assets, but MANIFEST.json lists {manifest['assets']} rows")
    if db["posts_live"] < manifest["posts"]:
        problems.append(f"{db['posts_live']} live posts, fewer than the {manifest['posts']} rows in posts.json")
    if db["posts_live_unpublished"]:
        problems.append(f"{db['posts_live_unpublished']} live post(s) are not published")
    odd = sorted(set(db["posts_by_visibility"]) - ALLOWED_VISIBILITY)
    if odd:
        problems.append(f"post visibility outside {sorted(ALLOWED_VISIBILITY)}: {odd}")
    if db["per_asset_preview_jobs"]:
        problems.append(f"{db['per_asset_preview_jobs']} per-asset preview job(s) exist; the seed must run "
                        f"with --previews=false")

    admin = {
        "assets": ids("SELECT id FROM assets WHERE deleted_at IS NULL ORDER BY created_at DESC, id DESC"),
        "posts": ids(f"SELECT id FROM posts WHERE deleted_at IS NULL AND state_id = {PUBLISHED} "
                     f"ORDER BY posted_at DESC, id DESC"),
    }

    people = {}
    for name in principals:
        if not USERNAME_RE.match(name):
            raise Refused(f"principal {name!r} is not a plain seeded username")
        found = query('SELECT ref, approved FROM "user" WHERE username = :\'uname\'', uname=name)
        if len(found) != 1:
            problems.append(f"principal {name!r} is not a seeded user on this instance ({len(found)} rows); "
                            f"seeded users come from seed/profiles/dataset.users.json")
            continue
        ref, approved = int(found[0][0]), int(found[0][1])
        if approved != 1:
            problems.append(f"principal {name!r} is not approved (approved={approved})")
        r = str(ref)
        people[name] = {
            "ref": ref,
            "assets": ids("SELECT id FROM assets WHERE deleted_at IS NULL "
                          "AND (NOT mature OR owner_user_ref = :'ref'::BIGINT) "
                          "ORDER BY created_at DESC, id DESC", ref=r),
            "owned_assets": one_int("SELECT count(*) FROM assets WHERE deleted_at IS NULL "
                                    "AND owner_user_ref = :'ref'::BIGINT", ref=r),
            "authored_posts": ids(f"SELECT id FROM posts WHERE deleted_at IS NULL AND state_id = {PUBLISHED} "
                                  f"AND author_user_ref = :'ref'::BIGINT ORDER BY posted_at DESC, id DESC", ref=r),
        }

    if problems:
        raise Refused("; ".join(problems))
    return {"schema": SCHEMA, "site": site, "manifest": manifest, "seed_report": seed, "db": db,
            "admin": admin, "principals": people}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--site", required=True, choices=["site_a", "site_b"])
    ap.add_argument("--site-root", required=True, type=Path)
    ap.add_argument("--seed-log", required=True, type=Path)
    ap.add_argument("--principal", action="append", default=[], required=True)
    ap.add_argument("--out", required=True, type=Path)
    a = ap.parse_args(argv)
    try:
        if a.out.exists():
            raise Refused(f"{a.out} already exists; refusing to overwrite an earlier record")
        doc = build(a.site, a.site_root, a.seed_log, a.principal)
    except (Refused, OSError, ValueError) as e:
        print(f"::error title=scale expected set::{e}", file=sys.stderr)
        return 1
    a.out.write_text(json.dumps(doc, indent=1) + "\n", encoding="utf-8")
    d, s = doc["db"], doc["seed_report"]
    print(f"scale_expected: {a.site}: manifest {doc['manifest']['assets']} assets / {doc['manifest']['posts']} posts; "
          f"seed report {s['assets']} / {s['posts']}; database {d['assets_live']} live assets "
          f"({d['assets_mature']} mature), {d['posts_live']} live posts ({d['posts_mature']} mature) "
          f"{d['posts_by_visibility']}; per-asset preview jobs {d['per_asset_preview_jobs']}")
    for name, p in doc["principals"].items():
        print(f"scale_expected:   {name} ref {p['ref']}: {len(p['assets'])} listable assets "
              f"({p['owned_assets']} owned), {len(p['authored_posts'])} authored posts")
    print(f"scale_expected: wrote {a.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
