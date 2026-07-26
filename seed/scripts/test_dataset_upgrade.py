#!/usr/bin/env python3
"""
Tests for the #604 dataset upgrade pipeline.

    python3 seed/scripts/test_dataset_upgrade.py

Stdlib unittest only, and deliberately NO dependency on the archive
share: the CI runners reach the dataset at a different mount point than
a workstation does, so a test that hardcoded /mnt/blackbox_archives
would pass here and fail there. Everything below builds a synthetic
mini-dataset in a temp dir instead.

The three rules under test are not style preferences. Each one is a bug
that already happened, and each failure mode is SILENT — the pipeline
reports success while the data is wrong, which is precisely why they
need tests rather than comments.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import apply_upgrade as up  # noqa: E402
import kenney_hq as hq      # noqa: E402

SCRIPTS = Path(__file__).resolve().parent
UPGRADES = SCRIPTS.parent / "upgrades"


# ---------------------------------------------------------------------------
# RULE 1 — names must not collide
# ---------------------------------------------------------------------------

class TestNamingCollisions(unittest.TestCase):
    """Slugging by basename silently overwrote 48 assets, then 65.

    Nothing failed at the time: the manifest still listed every entry and
    every file it named existed on disk. The assets that lost the race
    just served someone else's bytes. That is the failure mode these
    tests exist to make loud.
    """

    # Real collisions from the Kenney pack. Four separate packs ship a
    # file at this exact path shape, and the UI packs ship one widget in
    # both a Default and a Double directory.
    COLLIDING = [
        "2D assets/Axonometric Blocks/Tilesheet/tilesheet_complete_2X.png",
        "2D assets/Abstract Platformer/Tilesheet/tilesheet_complete_2X.png",
        "2D assets/Isometric Blocks/Tilesheet/tilesheet_complete_2X.png",
        "2D assets/Topdown Shooter/Tilesheet/tilesheet_complete_2X.png",
        "UI assets/UI Pack - Adventure/PNG/Default/progress_red_border.png",
        "UI assets/UI Pack - Adventure/PNG/Double/progress_red_border.png",
    ]

    def test_basename_alone_would_collide(self):
        """Guards the guard: prove these paths really do share basenames."""
        basenames = [p.rsplit("/", 1)[-1] for p in self.COLLIDING]
        self.assertLess(len(set(basenames)), len(basenames),
                        "test data no longer exercises a real collision")

    def test_path_hash_disambiguates(self):
        names = [hq.name_for(p) for p in self.COLLIDING]
        self.assertEqual(len(names), len(set(names)),
                         f"output names collided: {names}")

    def test_hash_is_stable_across_runs(self):
        # Determinism is what lets a rebuild reproduce the same pool
        # rather than a new sample with new names.
        for p in self.COLLIDING:
            self.assertEqual(hq.path_hash(p), hq.path_hash(p))
        self.assertEqual(hq.path_hash("2D assets/Cartography Pack/Textures/"
                                      "parchmentFoldedCrinkled.png"), "e0251cba")

    def test_hash_depends_on_full_path_not_basename(self):
        a = hq.path_hash("UI assets/UI Pack/PNG/Default/x.png")
        b = hq.path_hash("UI assets/UI Pack/PNG/Double/x.png")
        self.assertNotEqual(a, b)

    def test_committed_pool_manifest_has_no_collisions(self):
        entries = hq.load_pool_manifest(UPGRADES / "kenney-hq-pool.json")
        names = [e["name"] for e in entries]
        sources = [e["source"] for e in entries]
        self.assertEqual(len(names), len(set(names)), "duplicate output names")
        self.assertEqual(len(sources), len(set(sources)), "duplicate sources")

    def test_committed_pool_names_are_a_pure_function_of_source(self):
        """A hand-edited manifest, or a changed naming rule, breaks the
        link between a pool file and the source it came from."""
        entries = hq.load_pool_manifest(UPGRADES / "kenney-hq-pool.json")
        for e in entries:
            self.assertEqual(hq.name_for(e["source"], e.get("render_px")),
                             e["name"], f"name drift for {e['source']}")


# ---------------------------------------------------------------------------
# RULE 2 — quality is dimensional, never byte size
# ---------------------------------------------------------------------------

class TestQualityGate(unittest.TestCase):
    """415 upgraded assets are under 10 KB AND at least 512px.

    Flat-colour vector art compresses hard, so once the library is
    vector-rendered a byte threshold rejects exactly the assets the
    upgrade exists to produce.
    """

    def test_committed_pool_is_mostly_large(self):
        entries = hq.load_pool_manifest(UPGRADES / "kenney-hq-pool.json")
        rendered = [e for e in entries if e["kind"] == "vector"]
        self.assertTrue(all(e.get("render_px", 0) >= 512 for e in rendered),
                        "rendered vectors must target at least 512px")
        self.assertGreater(len(rendered), 0)

    def test_verifier_rejects_a_small_image_regardless_of_byte_size(self):
        with tempfile.TemporaryDirectory() as td:
            pool = Path(td)
            # A 64px PNG that is *large* in bytes. A byte-based gate would
            # happily accept it; the dimension gate must not.
            self._write_png(pool / f"x-{hq.path_hash('a/b/c.png')}.png",
                            64, 64, padding=40_000)
            self.assertEqual(hq.verify_pool(pool), 1,
                             "a 64px image must fail the pool verifier")

    def test_verifier_accepts_a_large_image_that_is_tiny_in_bytes(self):
        with tempfile.TemporaryDirectory() as td:
            pool = Path(td)
            # 512px and only a few hundred bytes — the exact shape of a
            # flat-colour vector render.
            self._write_png(pool / f"y-{hq.path_hash('a/b/d.png')}-512.png",
                            512, 512)
            self.assertEqual(hq.verify_pool(pool), 0,
                             "a 512px image must pass however small it is")

    @staticmethod
    def _write_png(path: Path, w: int, h: int, padding: int = 0) -> None:
        """Minimal valid PNG header + IHDR. Only the header is ever read
        (png_dimensions is header-only), so the image data can be junk."""
        import struct
        import zlib
        ihdr = struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0)
        chunk = (struct.pack(">I", len(ihdr)) + b"IHDR" + ihdr
                 + struct.pack(">I", zlib.crc32(b"IHDR" + ihdr) & 0xFFFFFFFF))
        path.write_bytes(b"\x89PNG\r\n\x1a\n" + chunk + b"\0" * padding)


# ---------------------------------------------------------------------------
# RULE 3 — sampling weights, explicit
# ---------------------------------------------------------------------------

class TestSamplingWeights(unittest.TestCase):
    """Input Prompts is 1,504 near-identical button glyphs — about 29% of
    every vector in the pack. Sampled evenly, browse looks like a
    settings screen."""

    def _candidates(self):
        out = []
        for i in range(1504):
            out.append(hq.Candidate(f"Icons/Input Prompts/Vector/key_{i}.svg",
                                    "Icons", "Input Prompts", "vector"))
        for i in range(200):
            out.append(hq.Candidate(f"2D assets/Brick Pack/Vector/brick_{i}.svg",
                                    "2D assets", "Brick Pack", "vector"))
        return out

    def test_damped_pack_is_under_represented_relative_to_its_size(self):
        cands = self._candidates()
        chosen = hq.select(cands, limit=200)
        prompts = sum(1 for c in chosen if c.pack == "Input Prompts")
        bricks = sum(1 for c in chosen if c.pack == "Brick Pack")
        # Input Prompts is 7.5x larger than Brick Pack as input. With a
        # 0.3x damp and a 3x boost it must NOT dominate the output.
        self.assertLess(prompts, bricks,
                        f"weights not applied: {prompts} prompts vs {bricks} bricks")

    def test_weights_are_declared_not_implicit(self):
        self.assertEqual(hq.PACK_WEIGHTS["Icons/Input Prompts"], 0.3)
        for boosted in ("2D assets/Cartography Pack", "2D assets/Brick Pack",
                        "2D assets/Fish Pack", "2D assets/Flag Pack",
                        "2D assets/Pattern Pack", "3D assets/Skybox Pack"):
            self.assertEqual(hq.PACK_WEIGHTS[boosted], 3.0, boosted)

    def test_selection_is_deterministic(self):
        cands = self._candidates()
        self.assertEqual([c.rel for c in hq.select(cands, 100)],
                         [c.rel for c in hq.select(cands, 100)])


# ---------------------------------------------------------------------------
# Titles
# ---------------------------------------------------------------------------

class TestTitles(unittest.TestCase):
    def test_vector_renders_are_marked(self):
        self.assertEqual(
            up.title_for("2d-assets-brick-pack-brick-high-1-36a68e65-512.png"),
            "Brick pack brick high 1 (vector)")

    def test_copied_bitmaps_are_not_marked(self):
        self.assertEqual(
            up.title_for("2d-assets-cartography-pack-parchmentfoldedcrinkled-e0251cba.png"),
            "Cartography pack parchmentfoldedcrinkled")

    def test_category_prefix_is_dropped(self):
        self.assertFalse(
            up.title_for("3d-assets-modular-cave-kit-preview-aabbccdd.png")
            .lower().startswith("3d assets"))


# ---------------------------------------------------------------------------
# The reconciliation itself
# ---------------------------------------------------------------------------

def _asset(aid: str, path: str, **kw) -> dict:
    base = {
        "id": aid, "asset_type": "image", "title": "Old Title",
        "file_path": path, "file_extension": "png", "file_size_bytes": 605,
        "source_root": "local", "source_path": path,
        "collection_name": "Project Echo", "team_name": "UI",
        "tags": ["ui", "buttons"], "brand_workspace": "Echo",
        "workflow_state": "approved", "owner_username": "mira.patel",
        "sensitivity_tier": "public",
        "license": "Internal", "attribution": "Someone else",
        "metadata": {"group_id": "grp-00042", "filename": path.rsplit("/", 1)[-1]},
    }
    base.update(kw)
    return base


class TestApplyUpgrade(unittest.TestCase):
    def setUp(self):
        self.profile = [
            _asset("id-1", "images/pack/PNG/Default/progress.png"),
            _asset("id-2", "images/pack/PNG/Double/progress.png"),
            _asset("id-3", "images/other/keep.png"),
        ]
        self.reps = [
            {"id": "id-1", "old": "images/pack/PNG/Default/progress.png",
             "oldSize": 605,
             "new": "images/kenney-hq/2d-assets-brick-pack-brick-high-1-36a68e65-512.png",
             "newSize": 8400},
            {"id": "id-2", "old": "images/pack/PNG/Double/progress.png",
             "oldSize": 611,
             "new": "images/kenney-hq/2d-assets-fish-pack-fish-grey-long-a-6eba36af-512.png",
             "newSize": 9100},
        ]

    def test_file_is_swapped_and_record_is_kept(self):
        """The property the whole upgrade rests on (#565 composition)."""
        before = {e["id"]: up._composition(e) for e in self.profile}
        changed, problems = up.apply_replacements(self.profile, self.reps)
        self.assertEqual(changed, 2)
        self.assertEqual(problems, [])
        for e in self.profile:
            self.assertEqual(up._composition(e), before[e["id"]],
                             f"composition moved for {e['id']}")
        e1 = self.profile[0]
        self.assertEqual(e1["file_path"], self.reps[0]["new"])
        self.assertEqual(e1["file_size_bytes"], 8400)
        self.assertEqual(e1["source_root"], "hq")
        self.assertEqual(e1["metadata"]["group_id"], "grp-00042")

    def test_licence_is_corrected_to_match_the_bytes(self):
        """Serving Kenney bytes under the old 'Internal' licence would be
        a false declaration, and site_a is published."""
        up.apply_replacements(self.profile, self.reps)
        for e in self.profile[:2]:
            self.assertEqual(e["license"], up.HQ_LICENSE)
            self.assertEqual(e["attribution"], up.HQ_ATTRIBUTION)
            self.assertEqual(e["metadata"]["license"], up.HQ_LICENSE)
        # Untouched records keep theirs.
        self.assertEqual(self.profile[2]["license"], "Internal")

    def test_original_path_is_remembered_for_the_csv(self):
        up.apply_replacements(self.profile, self.reps)
        self.assertEqual(self.profile[0]["replaced_source_path"],
                         "images/pack/PNG/Default/progress.png")

    def test_is_idempotent(self):
        up.apply_replacements(self.profile, self.reps)
        snapshot = json.dumps(self.profile, sort_keys=True)
        up.apply_replacements(self.profile, self.reps)
        self.assertEqual(json.dumps(self.profile, sort_keys=True), snapshot)

    def test_audit_catches_two_records_sharing_one_file(self):
        """The shape a name collision leaves behind in a profile."""
        up.apply_replacements(self.profile, self.reps)
        self.profile[1]["file_path"] = self.profile[0]["file_path"]
        problems = up.audit(self.profile, [], [], [], [])
        self.assertTrue(any("share a file_path" in p for p in problems),
                        problems)

    def test_audit_catches_an_asset_with_no_post(self):
        """An asset nobody posted is invisible on browse — the reason the
        videos needed solo posts at all."""
        added = [_asset("vid-1", "videos/internet/x.mp4", asset_type="video")]
        problems = up.audit(self.profile + added, [], [], added, [])
        self.assertTrue(any("unreachable on browse" in p for p in problems),
                        problems)

    def test_audit_catches_missing_copier_provenance(self):
        """populate_archive drops any record without source_path, so this
        is how 72 videos would vanish without a single error."""
        added = [_asset("vid-1", "videos/internet/x.mp4", asset_type="video",
                        source_path=None, source_root=None)]
        problems = up.audit(self.profile + added, [{"id": "p", "asset_ids": ["vid-1"]}],
                            [], added, [])
        self.assertTrue(any("no source_path" in p for p in problems), problems)

    def test_merge_added_gives_prestaged_provenance(self):
        added = [_asset("vid-1", "videos/internet/x.mp4", asset_type="video",
                        source_path=None, source_root=None)]
        n = up.merge_added(self.profile, added)
        self.assertEqual(n, 1)
        rec = [e for e in self.profile if e["id"] == "vid-1"][0]
        self.assertEqual(rec["source_root"], up.SITE_SOURCE_ROOT)
        self.assertEqual(rec["source_path"], "videos/internet/x.mp4")
        self.assertEqual(up.merge_added(self.profile, added), 0, "not idempotent")


# ---------------------------------------------------------------------------
# The regression this whole issue exists to prevent
# ---------------------------------------------------------------------------

class TestReassemblyReproducesUpgrade(unittest.TestCase):
    """End-to-end: re-running assembly must NOT restore the originals.

    Builds a synthetic source dataset + an upgraded profile, runs the
    real populate_archive.py, and asserts the two files it regenerates —
    MANIFEST.json and metadata.csv — describe the upgraded library.

    Before #604 this test fails on both counts: the manifest is a copy of
    a stale profile, and the CSV rows still name the tiny originals.
    """

    def test_regenerated_site_describes_the_upgraded_library(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            local = root / "source"
            pool = root / "hq"
            dest = root / "site"
            fetched = root / "fetched"
            for d in (local, pool, fetched):
                d.mkdir(parents=True)

            # A source dataset holding the ORIGINAL tiny file.
            tiny_rel = "images/pack/PNG/Default/progress.png"
            (local / tiny_rel).parent.mkdir(parents=True, exist_ok=True)
            (local / tiny_rel).write_bytes(b"\x89PNG\r\n\x1a\n" + b"\0" * 100)
            (local / "metadata.csv").write_text(
                "asset_id,file_path,title,kind,file_size_bytes\n"
                f"ast-1,{tiny_rel},Progress,raster,108\n"
                "ast-2,images/pack/keep.png,Keep,raster,50\n",
                encoding="utf-8")
            (local / "images/pack/keep.png").write_bytes(b"keep")

            # The HQ pool holding the replacement.
            hq_name = "2d-assets-brick-pack-brick-high-1-36a68e65-512.png"
            (pool / hq_name).write_bytes(b"\x89PNG\r\n\x1a\n" + b"\0" * 8000)

            # An upgraded profile: the record kept its identity, its file
            # changed, and it remembers where it came from.
            profile = [
                {
                    "id": "id-1", "asset_type": "image",
                    "title": "Brick pack brick high 1 (vector)",
                    "file_path": f"images/kenney-hq/{hq_name}",
                    "source_root": "hq", "source_path": hq_name,
                    "replaced_source_path": tiny_rel,
                    "file_size_bytes": 8008, "license": "CC0 1.0",
                    "metadata": {"group_id": "grp-1"},
                },
                {
                    "id": "id-2", "asset_type": "image", "title": "Keep",
                    "file_path": "images/pack/keep.png",
                    "source_root": "local", "source_path": "images/pack/keep.png",
                    "file_size_bytes": 4, "license": "CC0 1.0",
                    "metadata": {"group_id": "grp-1"},
                },
            ]
            profile_path = root / "profile.json"
            profile_path.write_text(json.dumps(profile), encoding="utf-8")

            proc = subprocess.run(
                [sys.executable, str(SCRIPTS / "populate_archive.py"),
                 "--local-source", str(local),
                 "--internet-source", str(fetched),
                 "--hq-source", str(pool),
                 "--profile", str(profile_path),
                 "--dest", str(dest)],
                capture_output=True, text=True)
            self.assertEqual(proc.returncode, 0, proc.stderr)

            # 1. The upgraded bytes were copied to the upgraded path.
            landed = dest / "images/kenney-hq" / hq_name
            self.assertTrue(landed.is_file(),
                            f"HQ file not copied.\n{proc.stderr}")
            self.assertGreater(landed.stat().st_size, 1000,
                               "the tiny original was restored")

            # 2. The manifest describes the upgrade.
            manifest = json.loads((dest / "MANIFEST.json").read_text())
            paths = {a["file_path"] for a in manifest}
            self.assertIn(f"images/kenney-hq/{hq_name}", paths)
            self.assertNotIn(tiny_rel, paths, "manifest regressed to the original")

            # 3. The shipped CSV kept the row AND repointed it — this is
            #    the half that silently vanishes if replaced_source_path
            #    is not honoured.
            import csv as _csv
            with (dest / "metadata.csv").open(newline="", encoding="utf-8") as f:
                rows = list(_csv.DictReader(f))
            by_id = {r["asset_id"]: r for r in rows}
            self.assertIn("ast-1", by_id,
                          "the replaced asset's CSV row was dropped")
            self.assertEqual(by_id["ast-1"]["file_path"],
                             f"images/kenney-hq/{hq_name}",
                             "CSV still points at the tiny original")
            self.assertEqual(len(rows), 2, "an unrelated row was lost")


class TestCommittedUpgradeData(unittest.TestCase):
    """The upgrade facts live in the repo, not only on the archive share.

    They were NAS-only until #604, which meant re-assembly on a machine
    that could not see the share silently produced the un-upgraded
    dataset.
    """

    SITES = ("site_a", "site_b")

    def test_every_site_has_its_upgrade_data_committed(self):
        for site in self.SITES:
            for kind in ("kenney-hq-replacements", "added-assets", "added-posts"):
                p = UPGRADES / f"{kind}.{site}.json"
                self.assertTrue(p.is_file(), f"missing {p}")
                self.assertGreater(len(json.loads(p.read_text())), 0, p)

    def test_replacements_all_target_the_hq_pool(self):
        pool_names = {e["name"] for e in
                      hq.load_pool_manifest(UPGRADES / "kenney-hq-pool.json")}
        for site in self.SITES:
            reps = json.loads(
                (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text())
            for r in reps:
                self.assertTrue(r["new"].startswith("images/kenney-hq/"), r)
                self.assertIn(r["new"].rsplit("/", 1)[-1], pool_names,
                              f"{site}: {r['new']} is not in the pool manifest")

    def test_byte_size_is_not_a_quality_signal(self):
        """RULE 2, demonstrated on the real mapping.

        The obvious "did this actually upgrade?" check is
        `newSize > oldSize`. It is WRONG, and the committed data proves
        it: some replacements swap a small screenshot for a 512px
        flat-colour vector render that is *fewer bytes*. Asserting on
        bytes here would fail on correct data and, worse, would teach the
        next person that bytes are the gate.

        This test exists to keep that counter-example in the suite. If it
        ever stops finding one, the data changed shape and someone should
        re-read RULE 2 before reintroducing a byte threshold.
        """
        smaller_but_upgraded = []
        for site in self.SITES:
            reps = json.loads(
                (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text())
            smaller_but_upgraded += [r for r in reps if r["newSize"] <= r["oldSize"]]
        self.assertGreater(
            len(smaller_but_upgraded), 0,
            "expected at least one replacement that is smaller in bytes yet "
            "larger in pixels — the counter-example RULE 2 rests on")
        # Every one of them still comes from the dimension-gated pool.
        pool_names = {e["name"] for e in
                      hq.load_pool_manifest(UPGRADES / "kenney-hq-pool.json")}
        for r in smaller_but_upgraded:
            self.assertIn(r["new"].rsplit("/", 1)[-1], pool_names)

    def test_replacements_target_a_dimension_gated_pool(self):
        """The real "did this upgrade?" check: every replacement points at
        a pool entry, and the pool is verified on pixels (see
        kenney_hq.verify_pool), not on bytes."""
        entries = {e["name"]: e for e in
                   hq.load_pool_manifest(UPGRADES / "kenney-hq-pool.json")}
        for site in self.SITES:
            reps = json.loads(
                (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text())
            for r in reps:
                e = entries[r["new"].rsplit("/", 1)[-1]]
                if e["kind"] == "vector":
                    self.assertGreaterEqual(e["render_px"], 512, r["new"])

    def test_added_assets_are_all_reachable(self):
        for site in self.SITES:
            assets = json.loads((UPGRADES / f"added-assets.{site}.json").read_text())
            posts = json.loads((UPGRADES / f"added-posts.{site}.json").read_text())
            referenced = {a for p in posts for a in p["asset_ids"]}
            orphans = [a["id"] for a in assets if a["id"] not in referenced]
            self.assertEqual(orphans, [],
                             f"{site}: {len(orphans)} added assets have no post")

    def test_profiles_are_upgraded(self):
        """The actual regression gate. If a future sanitize_and_assemble
        run regenerates the profiles from the source CSV without applying
        the upgrade, this fails."""
        for site, stem in (("site_a", "studio-a"), ("site_b", "studio-b")):
            profile = json.loads(
                (SCRIPTS.parent / "profiles" / f"{stem}.assets.json").read_text())
            by_id = {e["id"]: e for e in profile}
            reps = json.loads(
                (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text())
            stale = [r["id"] for r in reps
                     if by_id.get(r["id"], {}).get("file_path") != r["new"]]
            self.assertEqual(
                stale, [],
                f"{site}: {len(stale)} profile records still point at the "
                "pre-upgrade file — re-assembly would restore the originals. "
                f"Run: python3 seed/scripts/apply_upgrade.py --site {site} …")

            added = json.loads((UPGRADES / f"added-assets.{site}.json").read_text())
            missing = [a["id"] for a in added if a["id"] not in by_id]
            self.assertEqual(missing, [], f"{site}: {len(missing)} added assets "
                             "are absent from the profile")

    def test_site_a_carries_no_undeclared_licences(self):
        """site_a is published. Every licence it ships must be declared,
        and Pexels is deliberately included now (owner decision) — which
        is exactly why the aggregate licence had to stop claiming
        CC-BY-SA-4.0."""
        profile = json.loads(
            (SCRIPTS.parent / "profiles" / "studio-a.assets.json").read_text())
        licences = {e.get("license") for e in profile}
        self.assertNotIn(None, licences, "an asset ships with no licence")
        self.assertNotIn("", licences, "an asset ships with an empty licence")


if __name__ == "__main__":
    unittest.main(verbosity=2)
