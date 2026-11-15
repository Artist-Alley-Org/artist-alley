#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Kenneth Blossom
"""
Unit tests for scripts/ci/scale_expected.py (#1514). Stdlib only, no database, no Docker.

psql is replaced through AA_SCALE_PSQL by a fake that answers each statement from canned data and
records what it was sent, so these tests pin: the read-only transaction wrapper, the -F/-v argument
shape, the tab-separated parsing, and every fail-closed check against the seed log, the manifest and
the database counts.

Run: python3 -I scripts/ci/test_scale_expected.py
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("scale_expected", HERE / "scale_expected.py")
se = importlib.util.module_from_spec(spec)
spec.loader.exec_module(se)

FAKE_PSQL = r'''
import json, os, sys
args = sys.argv[1:]
variables = {}
for i, a in enumerate(args):
    if a == "-v" and i + 1 < len(args):
        k, _, v = args[i + 1].partition("=")
        variables[k] = v
assert "-F" in args and args[args.index("-F") + 1] == "\t", args
sql = sys.stdin.read()
with open(os.environ["FAKE_PSQL_LOG"], "a", encoding="utf-8") as fh:
    fh.write(json.dumps({"sql": sql, "vars": variables}) + "\n")
lines = sql.strip().splitlines()
assert lines[0] == "BEGIN TRANSACTION READ ONLY;", lines[0]
assert lines[-1] == "COMMIT;", lines[-1]
q = "\n".join(lines[1:-1])
d = json.load(open(os.environ["FAKE_PSQL_DATA"], encoding="utf-8"))
if d.get("fail"):
    print("ERROR:  relation does not exist", file=sys.stderr); sys.exit(3)
def out(rows):
    for r in rows:
        print("\t".join(str(c) for c in r))
    sys.exit(0)
if "GROUP BY 1" in q and "visibility" in q: out(d["vis"])
if "FROM jobs" in q: out(d["jobs"])
if 'FROM "user" WHERE username' in q:
    assert "uname" in variables
    out(d["users_by_name"].get(variables["uname"], []))
if 'count(*) FROM "user"' in q: out([[d["users"]]])
if "SELECT id FROM assets" in q and "owner_user_ref" in q: out([[x] for x in d["p_assets"][variables["ref"]]])
if "SELECT id FROM assets" in q: out([[x] for x in d["admin_assets"]])
if "SELECT id FROM posts" in q and "author_user_ref" in q: out([[x] for x in d["authored"][variables["ref"]]])
if "SELECT id FROM posts" in q: out([[x] for x in d["admin_posts"]])
if "count(*) FROM assets" in q and "owner_user_ref" in q: out([[d["owned"][variables["ref"]]]])
if "count(*) FROM assets" in q and "mature" in q: out([[d["assets_mature"]]])
if "count(*) FROM assets" in q: out([[len(d["admin_assets"])]])
if "count(*) FROM posts" in q and "IS DISTINCT FROM" in q: out([[d["unpublished"]]])
if "count(*) FROM posts" in q and "mature" in q: out([[d["posts_mature"]]])
if "count(*) FROM posts" in q: out([[d.get("posts_live", len(d["admin_posts"]))]])
print("unhandled: " + q, file=sys.stderr); sys.exit(9)
'''


def seed_line(assets: int, posts: int) -> str:
    return f"seed complete: users=32 teams=11 collections=9 assets={assets} posts={posts} comments=1\n"


class ScaleExpected(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="scale-expected-test-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(self.tmp, ignore_errors=True))
        (self.tmp / "fake_psql.py").write_text(FAKE_PSQL, encoding="utf-8")
        self.site = self.tmp / "site"
        self.site.mkdir()
        (self.site / "MANIFEST.json").write_text(json.dumps([{"id": f"a{i}"} for i in range(4)]), encoding="utf-8")
        (self.site / "posts.json").write_text(json.dumps({"posts": [{"id": "p1"}, {"id": "p2"}]}), encoding="utf-8")
        self.log = self.tmp / "seed.log"
        self.log.write_text('{"msg":"seed.assets"}\n' + seed_line(4, 3), encoding="utf-8")
        self.data = {
            "vis": [["org-only", 2], ["public", 1]],
            "jobs": [["preview.thumbhash_backfill", 1], ["digest.daily", 1]],
            "users_by_name": {"chen.wei": [[12, 1]], "anya.volkov": [[7, 1]], "pending.user": [[9, 0]]},
            "users": 32,
            "admin_assets": ["a3", "a2", "a1", "a0"],
            "admin_posts": ["p3", "p2", "p1"],
            "p_assets": {"12": ["a3", "a2", "a0"], "7": ["a3", "a2", "a1", "a0"], "9": []},
            "authored": {"12": ["p2"], "7": [], "9": []},
            "owned": {"12": 0, "7": 4, "9": 0},
            "assets_mature": 1,
            "posts_mature": 0,
            "unpublished": 0,
        }
        self.psql_log = self.tmp / "psql.log"
        self.env = {"AA_SCALE_PSQL": f"{sys.executable} -I {self.tmp / 'fake_psql.py'}",
                    "FAKE_PSQL_DATA": str(self.tmp / "data.json"), "FAKE_PSQL_LOG": str(self.psql_log)}
        self.out = self.tmp / "expected.json"

    def run_main(self, principals=("chen.wei",), **override):
        d = dict(self.data, **override)
        (self.tmp / "data.json").write_text(json.dumps(d), encoding="utf-8")
        args = ["--site", "site_a", "--site-root", str(self.site), "--seed-log", str(self.log), "--out", str(self.out)]
        for p in principals:
            args += ["--principal", p]
        old = {k: os.environ.get(k) for k in self.env}
        os.environ.update(self.env)
        err = io.StringIO()
        try:
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
                rc = se.main(args)
        finally:
            for k, v in old.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v
        return rc, err.getvalue()

    def test_success_records_ordered_sets_and_counts(self):
        rc, err = self.run_main(principals=("chen.wei", "anya.volkov"))
        self.assertEqual(rc, 0, err)
        doc = json.loads(self.out.read_text(encoding="utf-8"))
        self.assertEqual(doc["schema"], "artist-alley.ui-scale-expected/v1")
        self.assertEqual(doc["manifest"], {"assets": 4, "posts": 2})
        self.assertEqual(doc["seed_report"]["assets"], 4)
        self.assertEqual(doc["admin"], {"assets": ["a3", "a2", "a1", "a0"], "posts": ["p3", "p2", "p1"]})
        self.assertEqual(doc["db"]["per_asset_preview_jobs"], 0)
        self.assertEqual(doc["db"]["posts_by_visibility"], {"org-only": 2, "public": 1})
        self.assertEqual(doc["principals"]["chen.wei"],
                         {"ref": 12, "assets": ["a3", "a2", "a0"], "owned_assets": 0, "authored_posts": ["p2"]})
        self.assertEqual(doc["principals"]["anya.volkov"]["owned_assets"], 4)

    def test_every_statement_is_a_read_only_transaction_and_names_go_through_psql_variables(self):
        rc, err = self.run_main()
        self.assertEqual(rc, 0, err)
        sent = [json.loads(ln) for ln in self.psql_log.read_text(encoding="utf-8").splitlines()]
        self.assertTrue(sent)
        for s in sent:
            self.assertTrue(s["sql"].startswith("BEGIN TRANSACTION READ ONLY;\n"))
            self.assertNotIn("chen.wei", s["sql"])               # the name is a psql variable, never spliced
        self.assertIn({"uname": "chen.wei"}, [s["vars"] for s in sent])
        import re
        for word in ("INSERT", "UPDATE", "DELETE", "TRUNCATE", "ALTER", "DROP"):
            pat = re.compile(rf"\b{word}\b")                     # `deleted_at` is not DELETE
            self.assertFalse(any(pat.search(s["sql"].upper()) for s in sent), word)

    def test_default_psql_targets_the_compose_postgres_quietly(self):
        self.assertTrue(se.DEFAULT_PSQL.startswith("docker compose exec -T postgres psql"))
        for flag in ("-X", "-q", "-At", "ON_ERROR_STOP=1"):
            self.assertIn(flag, se.DEFAULT_PSQL)

    def assertRefused(self, needle, principals=("chen.wei",), **override):
        rc, err = self.run_main(principals=principals, **override)
        self.assertEqual(rc, 1, err)
        self.assertIn(needle, err)
        self.assertFalse(self.out.exists(), "a refused run must write nothing")

    def test_seed_report_must_equal_the_database(self):
        self.log.write_text(seed_line(5, 3), encoding="utf-8")
        self.assertRefused("seed report assets=5")

    def test_exactly_one_seed_line_and_no_drift(self):
        self.log.write_text(seed_line(4, 3) + seed_line(4, 3), encoding="utf-8")
        self.assertRefused("exactly one 'seed complete:' line")
        self.log.write_text("no summary here\n", encoding="utf-8")
        self.assertRefused("found 0")
        self.log.write_text(seed_line(4, 3) + "  NOT a clean reseed: 2 post(s) still disagree\n", encoding="utf-8")
        self.assertRefused("NOT a clean reseed")

    def test_assets_must_equal_the_manifest(self):
        self.log.write_text(seed_line(3, 3), encoding="utf-8")
        self.assertRefused("MANIFEST.json lists 4 rows", admin_assets=["a3", "a2", "a1"])

    def test_posts_cannot_be_fewer_than_posts_json(self):
        self.log.write_text(seed_line(4, 1), encoding="utf-8")
        self.assertRefused("fewer than the 2 rows", posts_live=1)

    def test_unpublished_or_unexpected_visibility_is_refused(self):
        self.assertRefused("not published", unpublished=1)
        self.assertRefused("post visibility outside", vis=[["followers", 1], ["public", 2]])

    def test_per_asset_preview_jobs_are_refused_but_the_boot_backfill_is_not(self):
        self.assertRefused("per-asset preview job", jobs=[["preview.raster", 3], ["preview.thumbhash_backfill", 1]])

    def test_principals_must_exist_be_approved_and_be_plain_names(self):
        self.assertRefused("is not a seeded user", principals=("nobody",))
        self.assertRefused("is not approved", principals=("pending.user",))
        self.assertRefused("not a plain seeded username", principals=("x'; select 1; --",))

    def test_psql_failure_and_an_existing_output_are_refused(self):
        self.assertRefused("psql failed", fail=True)
        self.out.write_text("{}", encoding="utf-8")
        rc, err = self.run_main()
        self.assertEqual(rc, 1)
        self.assertIn("already exists", err)

    def test_seed_line_regex_matches_the_seeder_format(self):
        # app/cmd/aa/main.go prints exactly this shape.
        src = (HERE.parents[1] / "app" / "cmd" / "aa" / "main.go").read_text(encoding="utf-8")
        self.assertIn('"seed complete: users=%d teams=%d collections=%d assets=%d posts=%d comments=%d\\n"', src)
        self.assertTrue(se.SEED_LINE_RE.match(seed_line(2005, 900).rstrip("\n")))


if __name__ == "__main__":
    unittest.main()
