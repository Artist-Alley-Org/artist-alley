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

import hashlib
import json
import struct
import subprocess
import sys
import tempfile
import threading
import unittest
import zlib
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import apply_upgrade as up          # noqa: E402
import authored_plates as ap        # noqa: E402
import audit_uncatalogued as au     # noqa: E402
import kenney_hq as hq              # noqa: E402
import kenney_pack_sources as kps   # noqa: E402
import pexels_gameplay as px        # noqa: E402
import manifest_guard as mg         # noqa: E402
import populate_archive as pa       # noqa: E402
import resolve_media_urls as rmu    # noqa: E402
import studio_balance as sb         # noqa: E402

SCRIPTS = Path(__file__).resolve().parent
UPGRADES = SCRIPTS.parent / "upgrades"
PROFILES = SCRIPTS.parent / "profiles"


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
        processed, modified, problems = up.apply_replacements(
            self.profile, self.reps)
        self.assertEqual((processed, modified), (2, 2))
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
        n, repaired = up.merge_added(self.profile, added)
        self.assertEqual((n, repaired), (1, 0))
        rec = [e for e in self.profile if e["id"] == "vid-1"][0]
        self.assertEqual(rec["source_root"], up.SITE_SOURCE_ROOT)
        self.assertEqual(rec["source_path"], "videos/internet/x.mp4")
        self.assertEqual(up.merge_added(self.profile, added), (0, 0),
                         "not idempotent")

    def test_merge_added_backfills_media_url_onto_an_already_merged_record(self):
        """#602's own trap, tested.

        merge_added skips ids the profile already has, so a field added to
        the upgrade docs AFTER the first merge would never reach the
        profiles — leaving media_url correct for new records and absent
        from every existing one. That is worse than not having the field:
        the copier would re-fetch some and give up on others.
        """
        added = [_asset("vid-1", "videos/internet/x.mp4", asset_type="video",
                        source_path=None, source_root=None)]
        up.merge_added(self.profile, added)          # first merge, no media_url
        added[0]["metadata"]["media_url"] = "https://videos.pexels.com/video-files/1/1-hd.mp4"
        n, repaired = up.merge_added(self.profile, added)
        self.assertEqual((n, repaired), (0, 1),
                         "an already-merged record did not pick up media_url")
        rec = [e for e in self.profile if e["id"] == "vid-1"][0]
        self.assertEqual(rec["metadata"]["media_url"],
                         "https://videos.pexels.com/video-files/1/1-hd.mp4")
        # and it must not keep re-reporting the same repair
        self.assertEqual(up.merge_added(self.profile, added), (0, 0))

    def test_audit_catches_a_prestaged_record_with_no_media_url(self):
        """A pre-staged record with no direct URL is unrecoverable the
        moment the archive share is not mounted — #602 exactly."""
        added = [_asset("vid-1", "videos/internet/x.mp4", asset_type="video",
                        source_path="videos/internet/x.mp4",
                        source_root=up.SITE_SOURCE_ROOT)]
        posts = [{"id": "p", "asset_ids": ["vid-1"]}]
        problems = up.audit(self.profile + added, posts, [], added, [])
        self.assertTrue(any("cannot be re-fetched from provenance" in p
                            for p in problems), problems)

    def test_processed_and_modified_are_different_numbers(self):
        """#1295. The count `--check` needs is records CHANGED, and the
        count the progress line reports is records SEEN. They diverge on
        the second run, which is the only run that matters to a gate."""
        first = up.apply_replacements(self.profile, self.reps)
        self.assertEqual(first[:2], (2, 2), "a fresh profile modifies both")
        second = up.apply_replacements(self.profile, self.reps)
        self.assertEqual(second[:2], (2, 0),
                         "an upgraded profile is still PROCESSED in full but "
                         "must report zero modified — reporting 2/2 here is "
                         "what made the pass unusable in the drift expression")

    def test_a_stale_byte_count_alone_counts_as_modified(self):
        """The exact shape #1295 hid: 86 records already pointing at the
        right HQ file, with a `file_size_bytes` that no longer matched the
        replacements doc. `file_path` — the only field the older
        `test_profiles_are_upgraded` gate compares — was correct on every
        one of them."""
        up.apply_replacements(self.profile, self.reps)
        self.profile[0]["file_size_bytes"] = 4242
        processed, modified, problems = up.apply_replacements(
            self.profile, self.reps)
        self.assertEqual((processed, modified), (2, 1), problems)
        self.assertEqual(self.profile[0]["file_size_bytes"], 8400)

    def test_composition_cannot_stand_in_for_the_modified_count(self):
        """⛔ The tempting shortcut, refused.

        The loop already compares `_composition` before and after, so it
        looks like it already knows whether a record moved. It does not:
        `_composition` covers the fields the swap must LEAVE ALONE, so it
        is equal on a run that rewrites every record. Using it as the
        drift signal would have reproduced the bug with more code.
        """
        fresh = [_asset("id-1", "images/pack/PNG/Default/progress.png")]
        before = up._composition(fresh[0])
        processed, modified, _ = up.apply_replacements(fresh, self.reps[:1])
        self.assertEqual((processed, modified), (1, 1))
        self.assertEqual(up._composition(fresh[0]), before,
                         "composition is unchanged by a full rewrite — which "
                         "is exactly why it is not a modified-count")

    def test_a_profile_with_no_replacements_is_zero_of_both(self):
        """Zero-processed and zero-modified are distinguishable, and
        neither is an error. site_b shipped no `balance` docs for a whole
        release; a site with no replacements doc at all must report `no
        drift`, not `no data`."""
        processed, modified, problems = up.apply_replacements(self.profile, [])
        self.assertEqual((processed, modified, problems), (0, 0, []))

    def test_an_id_the_profile_lacks_is_a_problem_not_a_processed_record(self):
        """`processed` is not `len(replacements)`. A doc naming a record
        that is not there has done nothing, and counting it as done is how
        a missing record reads as a healthy one."""
        reps = self.reps + [{"id": "ghost", "old": "x", "oldSize": 1,
                             "new": "images/kenney-hq/ghost.png", "newSize": 2}]
        processed, modified, problems = up.apply_replacements(self.profile, reps)
        self.assertEqual(processed, 2)
        self.assertEqual(modified, 2)
        self.assertTrue(any("ghost" in p for p in problems), problems)

    def test_audit_rejects_replacing_the_page_url_with_the_media_url(self):
        """Swapping fetched_from for the CDN path would close the re-fetch
        gap by opening an attribution one — the page is where the licence
        and the photographer credit live."""
        added = [_asset("vid-1", "videos/internet/x.mp4", asset_type="video",
                        source_path="videos/internet/x.mp4",
                        source_root=up.SITE_SOURCE_ROOT)]
        added[0]["metadata"]["media_url"] = "https://videos.pexels.com/video-files/1/1-hd.mp4"
        posts = [{"id": "p", "asset_ids": ["vid-1"]}]
        problems = up.audit(self.profile + added, posts, [], added, [])
        self.assertTrue(any("lost fetched_from" in p for p in problems), problems)


# ---------------------------------------------------------------------------
# The pre-publish gate, driven end to end (#1295)
# ---------------------------------------------------------------------------

class TestCheckSeesReplacementDrift(unittest.TestCase):
    """⛔ A GATE THAT HAS ONLY EVER BEEN SEEN TO PASS IS UNTESTED.

    That is ADR 0095's 2026-08-26 amendment and ADR 0097's second
    consequence, and it is the whole of #1295: `--check` reported `OK:
    profile already reflects the upgrade` while studio-b carried 86 stale
    `file_size_bytes`, and nobody could tell, because nobody had ever seen
    the sentence it prints when replacements drift — there wasn't one.

    So these drive the real script, on a real drift, and assert the
    FAILURE. The repaired-and-passing half is asserted too, because a gate
    that fails on everything is no better.
    """

    HQ = "images/kenney-hq/2d-assets-brick-pack-brick-high-1-36a68e65-512.png"
    HQ2 = "images/kenney-hq/2d-assets-fish-pack-fish-grey-long-a-6eba36af-512.png"

    def _site(self, td: Path, *, stale_bytes=False, duplicate_post=False):
        """A minimal but REAL site: an upgrades dir, a profile, posts."""
        upgrades = td / "upgrades"
        upgrades.mkdir(exist_ok=True)
        reps = [
            {"id": "id-1", "old": "images/pack/a.png", "oldSize": 605,
             "new": self.HQ, "newSize": 8400},
            {"id": "id-2", "old": "images/pack/b.png", "oldSize": 611,
             "new": self.HQ2, "newSize": 9100},
        ]
        (upgrades / "kenney-hq-replacements.site_a.json").write_text(
            json.dumps(reps), encoding="utf-8")

        profile = [_asset("id-1", "images/pack/a.png"),
                   _asset("id-2", "images/pack/b.png")]
        # Bring it to the upgraded state the committed profiles are in,
        # so the only difference below is the one being constructed.
        up.apply_replacements(profile, reps)
        if stale_bytes:
            # The #1295 shape exactly: the file_path is RIGHT, so every
            # pre-existing gate is satisfied; only the byte count is stale.
            profile[0]["file_size_bytes"] = 8401

        posts = [{"id": "post-1", "asset_ids": ["id-1", "id-2"]}]
        if duplicate_post:
            posts.append({"id": "post-1", "asset_ids": ["id-1"]})

        (td / "assets.json").write_text(json.dumps(profile), encoding="utf-8")
        (td / "posts.json").write_text(json.dumps(posts), encoding="utf-8")
        return upgrades

    def _check(self, td: Path, upgrades: Path):
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
             "--site", "site_a", "--upgrades", str(upgrades),
             "--profile", str(td / "assets.json"),
             "--posts", str(td / "posts.json"), "--check"],
            capture_output=True, text=True)

    def test_an_upgraded_profile_passes(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            r = self._check(td, self._site(td))
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("OK: profile already reflects the upgrade", r.stderr)

    def test_a_stale_byte_count_fails_the_gate_and_is_named(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            r = self._check(td, self._site(td, stale_bytes=True))
            self.assertEqual(r.returncode, 1,
                             "a profile with drifted replacements passed the "
                             f"pre-publish gate:\n{r.stderr}")
            self.assertIn("1 replacement record(s) disagree", r.stderr)
            self.assertIn("kenney-hq-replacements.site_a.json", r.stderr)

    def test_the_progress_line_distinguishes_processed_from_modified(self):
        """`260/260 records repointed` was true on every run and told the
        reader nothing. The modified count is the half that moves."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            clean = self._check(td, self._site(td))
            self.assertIn("2/2 records repointed at the HQ pool (0 modified)",
                          clean.stderr)
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            drifted = self._check(td, self._site(td, stale_bytes=True))
            self.assertIn("2/2 records repointed at the HQ pool (1 modified)",
                          drifted.stderr)

    def test_the_failure_names_only_the_passes_that_actually_drifted(self):
        """⭐ One boolean over seven counters printed all seven regardless
        of which fired, so the reader had to hunt for the non-zero number
        in a sentence of zeroes. Two drifting passes name two; one names
        one; and the other six stay out of it."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            one = self._check(td, self._site(td, stale_bytes=True))
            verdict = one.stderr.split("FAIL:", 1)[1]
            self.assertIn("1 pass(es) would change it", verdict)
            # The progress block above still reports every pass, as it
            # should. It is the VERDICT that must name only what fired.
            self.assertNotIn("media_url", verdict)
            self.assertNotIn("share an id with another", verdict)
            self.assertEqual(verdict.count("\n  - "), 1, verdict)
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            two = self._check(td, self._site(td, stale_bytes=True,
                                             duplicate_post=True))
            self.assertEqual(two.returncode, 1, two.stderr)
            verdict = two.stderr.split("FAIL:", 1)[1]
            self.assertIn("2 pass(es) would change it", verdict)
            self.assertIn("replacement record(s) disagree", verdict)
            self.assertIn("share an id with another", verdict)
            self.assertEqual(verdict.count("\n  - "), 2, verdict)

    def test_the_gate_passes_again_once_the_profile_is_repaired(self):
        """The other half of "seen to refuse": running WITHOUT --check
        must reach a fixed point. Sprint 14's backed-out attempt at #1294
        failed exactly here — it rewrote 149 values on every run."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, stale_bytes=True)
            self.assertEqual(self._check(td, upgrades).returncode, 1)
            repair = subprocess.run(
                [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
                 "--site", "site_a", "--upgrades", str(upgrades),
                 "--profile", str(td / "assets.json"),
                 "--posts", str(td / "posts.json")],
                capture_output=True, text=True)
            self.assertEqual(repair.returncode, 0, repair.stderr)
            self.assertEqual(self._check(td, upgrades).returncode, 0)

    def test_the_committed_profiles_pass_the_widened_gate(self):
        """⚠️ The gate got STRICTER, and this is the assertion that the
        committed data already satisfies it — i.e. that #1294 is settled
        and stays settled. Before sprint 14 repaired them, studio-b's 86
        stale byte counts would have reddened this."""
        for site, stem in (("site_a", "studio-a"), ("site_b", "studio-b")):
            r = subprocess.run(
                [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
                 "--site", site, "--upgrades", str(UPGRADES),
                 "--profile", str(PROFILES / f"{stem}.assets.json"),
                 "--posts", str(PROFILES / f"{stem}.posts.json"), "--check"],
                capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, f"{site}:\n{r.stderr}")
            self.assertIn("(0 modified)", r.stderr, site)


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

    def test_no_record_still_claims_pexels_is_site_b_only(self):
        """#607 corrected this string in studio-a's profile — but left it
        in studio-b's AND in both upgrade docs, which merge_added would
        have pushed straight back into a regenerated profile. A fix that
        only lands in the output and not in the input is not a fix."""
        docs = [SCRIPTS.parent / "profiles" / f"{stem}.assets.json"
                for _, stem in (("site_a", "studio-a"), ("site_b", "studio-b"))]
        docs += [UPGRADES / f"added-assets.{s}.json" for s in ("site_a", "site_b")]
        for p in docs:
            stale = [e["id"] for e in json.loads(p.read_text())
                     if "site_b only" in (e.get("description") or "")]
            self.assertEqual(stale, [], f"{p.name}: {len(stale)} record(s) still "
                             "describe Pexels content as site_b only (#607)")


# ---------------------------------------------------------------------------
# RULE 4 — a record must be re-fetchable from what we wrote down (#602)
# ---------------------------------------------------------------------------

class TestProvenanceIsRefetchable(unittest.TestCase):
    """The failure this guards is quiet and only appears on a machine you
    do not have: with the archive share mounted, a pre-staged record
    verifies fine forever. Without it, `metadata.fetched_from` for the 30
    Pexels videos is `https://www.pexels.com/video/…/` — an HTML page
    behind Cloudflare — so there is nothing to GET and re-assembly ends
    with a hole it reports as MISSING.

    Offline by construction. Nothing here touches the network or the
    archive share; the committed profiles are the artefact under test.
    """

    SITES = (("site_a", "studio-a"), ("site_b", "studio-b"))

    def profiles(self):
        for site, stem in self.SITES:
            yield site, json.loads(
                (SCRIPTS.parent / "profiles" / f"{stem}.assets.json").read_text())

    def test_every_prestaged_record_can_be_refetched(self):
        """`site`-root records have no local source. If they also have no
        media_url, the copier's only option is to give up."""
        for site, profile in self.profiles():
            orphans = [
                (e["id"], (e.get("metadata") or {}).get("filename"))
                for e in profile
                if e.get("source_root") == up.SITE_SOURCE_ROOT
                and not (e.get("metadata") or {}).get("media_url")
            ]
            self.assertEqual(
                orphans, [],
                f"{site}: {len(orphans)} pre-staged record(s) carry no "
                "metadata.media_url, so a rebuild without the archive share "
                "cannot reconstruct them. Run: python3 seed/scripts/"
                f"resolve_media_urls.py --write seed/profiles/*.assets.json")

    def test_the_page_url_survives_alongside_the_media_url(self):
        """Both, not either. The page is the attribution + licence
        evidence (ATTRIBUTIONS.md and the Kaggle paperwork point at it);
        the CDN path is the bytes. Replacing one with the other trades
        one gap for another."""
        for site, profile in self.profiles():
            for e in profile:
                meta = e.get("metadata") or {}
                if not meta.get("media_url"):
                    continue
                self.assertTrue(
                    meta.get("fetched_from"),
                    f"{site}: {e['id']} has a media_url but no fetched_from")

    def test_pexels_records_point_at_a_file_host_not_a_page(self):
        """The actual #602 bug, pinned: a www.pexels.com/video/… URL is a
        document. Recording it as the media URL is what made these
        records unrecoverable."""
        for site, profile in self.profiles():
            px = [e for e in profile
                  if (e.get("metadata") or {}).get("acquisition_source") == "Pexels"]
            self.assertGreater(len(px), 0, f"{site}: no Pexels records to check")
            for e in px:
                meta = e["metadata"]
                self.assertTrue(
                    meta["fetched_from"].startswith("https://www.pexels.com/video/"),
                    f"{site}: {e['id']} fetched_from is no longer the Pexels page")
                self.assertTrue(
                    meta["media_url"].startswith(
                        "https://videos.pexels.com/video-files/"),
                    f"{site}: {e['id']} media_url {meta['media_url']!r} is not a "
                    "direct Pexels media path")
                self.assertNotEqual(meta["media_url"], meta["fetched_from"])
                self.assertTrue(
                    meta["media_url"].endswith(".mp4"),
                    f"{site}: {e['id']} media_url does not name a file")

    def test_media_url_hosts_are_declared(self):
        """A new source silently introducing a page URL again is the way
        this regresses. The host allow-list is the tripwire."""
        from urllib.parse import urlparse
        for site, profile in self.profiles():
            for e in profile:
                mu = (e.get("metadata") or {}).get("media_url")
                if not mu:
                    continue
                self.assertIn(urlparse(mu).netloc, rmu.DIRECT_MEDIA_HOSTS,
                              f"{site}: {e['id']} media_url host is undeclared — "
                              "add it to resolve_media_urls.DIRECT_MEDIA_HOSTS "
                              "deliberately, or it is a page URL again")

    def test_both_sites_agree_on_the_url_for_the_same_file(self):
        """site_a and site_b ship the same bytes for these records. Two
        different URLs would mean one of them was resolved against the
        wrong clip."""
        by_path = {}
        for site, profile in self.profiles():
            for e in profile:
                mu = (e.get("metadata") or {}).get("media_url")
                if not mu:
                    continue
                prev = by_path.setdefault(e["file_path"], (site, mu))
                self.assertEqual(prev[1], mu,
                                 f"{e['file_path']}: {prev[0]} says {prev[1]}, "
                                 f"{site} says {mu}")


class TestResolverPureFunctions(unittest.TestCase):
    """resolve_media_urls' URL derivation, with no network at all.

    The resolver never trusts these candidates — it accepts one only when
    a HEAD returns the recorded byte count — but a wrong candidate list
    means the pattern path silently degrades to always scraping the page,
    which needs a Cloudflare solver. Worth pinning.
    """

    def test_video_id_comes_off_the_page_url(self):
        self.assertEqual(
            rmu.pexels_video_id(
                "https://www.pexels.com/video/time-lapse-of-sky-at-sunset-10161903/"),
            "10161903")
        # slug full of digits and hyphens — the id is the LAST run
        self.assertEqual(
            rmu.pexels_video_id(
                "https://www.pexels.com/video/xonotic-0-8-2-gameplay-853996/"),
            "853996")
        self.assertIsNone(rmu.pexels_video_id("https://www.pexels.com/video/nope/"))

    def test_dimensions_come_from_the_record_itself(self):
        self.assertEqual(
            rmu.recorded_dimensions({"description": "1280x720 12s landscape — x"}),
            ("1280", "720"))
        self.assertEqual(
            rmu.recorded_dimensions({"description": "1440x2560 9s portrait"}),
            ("1440", "2560"))
        self.assertEqual(rmu.recorded_dimensions({"description": "no dims here"}),
                         (None, None))

    def test_candidates_are_bounded_and_well_formed(self):
        cands = rmu.pexels_pattern_candidates("10161903", "1280", "720")
        self.assertEqual(len(cands),
                         len(rmu.PEXELS_QUALITIES) * len(rmu.PEXELS_FPS))
        self.assertIn(
            "https://videos.pexels.com/video-files/10161903/"
            "10161903-hd_1280_720_60fps.mp4", cands)
        for c in cands:
            self.assertTrue(c.startswith("https://videos.pexels.com/video-files/"))
        # no dimensions recorded -> no guessing
        self.assertEqual(rmu.pexels_pattern_candidates("1", None, None), [])

    def test_page_candidates_are_filtered_to_this_video(self):
        """A Pexels page also embeds related clips. Without the id filter
        a byte-count collision could record someone else's file."""
        html = (
            'x https://videos.pexels.com/video-files/10161903/10161903-hd_1280_720_60fps.mp4 y'
            ' https://videos.pexels.com/video-files/99999999/99999999-hd_1280_720_60fps.mp4')
        # exercise the filter without the fetch
        import re as _re
        found = [u for u in rmu._PEXELS_MEDIA_RE.findall(html)
                 if u.startswith("https://videos.pexels.com/video-files/10161903/")]
        self.assertEqual(len(found), 1)
        self.assertIn("10161903-hd", found[0])

    def test_check_mode_fails_a_record_with_no_media_url(self):
        with tempfile.TemporaryDirectory() as d:
            doc = Path(d) / "p.json"
            rec = {"id": "a", "file_path": "videos/internet/x.mp4",
                   "file_size_bytes": 10,
                   "metadata": {"filename": "x.mp4",
                                "fetched_from": "https://www.pexels.com/video/a-1/"}}
            doc.write_text(json.dumps([rec]))
            self.assertEqual(rmu.cmd_check([doc]), 1)
            rec["metadata"]["media_url"] = \
                "https://videos.pexels.com/video-files/1/1-hd_1280_720_30fps.mp4"
            doc.write_text(json.dumps([rec]))
            self.assertEqual(rmu.cmd_check([doc]), 0)

    def test_check_mode_rejects_an_undeclared_host(self):
        with tempfile.TemporaryDirectory() as d:
            doc = Path(d) / "p.json"
            doc.write_text(json.dumps([{
                "id": "a", "file_path": "videos/internet/x.mp4",
                "file_size_bytes": 10,
                "metadata": {"filename": "x.mp4",
                             "fetched_from": "https://www.pexels.com/video/a-1/",
                             "media_url": "https://www.pexels.com/video/a-1/"},
            }]))
            self.assertEqual(rmu.cmd_check([doc]), 1,
                             "a page URL smuggled in as media_url must fail")


class TestPopulateArchiveRefetch(unittest.TestCase):
    """The round trip, end to end, against a LOCAL server.

    This is the half that cannot be argued about: a pre-staged record
    whose bytes are absent at the destination is reconstructed from
    `metadata.media_url` alone. The server is `http.server` on 127.0.0.1
    — stdlib, no network, no NAS, per the fixture rule at the top of this
    file. What it proves is the WIRING; the real Pexels round trip is
    `resolve_media_urls.py --refetch --against <site>`, which is a
    network operation and therefore not a unit test.
    """

    @staticmethod
    def _serve(directory):
        handler = partial(SimpleHTTPRequestHandler, directory=str(directory))
        httpd = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        t = threading.Thread(target=httpd.serve_forever, daemon=True)
        t.start()
        return httpd, f"http://127.0.0.1:{httpd.server_address[1]}"

    def _run(self, tmp, media_url, size, extra_args=()):
        """Build a minimal dataset + profile and run populate_archive."""
        tmp = Path(tmp)
        local = tmp / "local"
        (local).mkdir()
        (local / "metadata.csv").write_text("file_path,title\n", encoding="utf-8")
        internet = tmp / "internet"
        internet.mkdir()
        dest = tmp / "dest"
        profile = tmp / "profile.json"
        rec = {
            "id": "vid-1", "asset_type": "video",
            "file_path": "videos/internet/clip.mp4",
            "source_root": "site", "source_path": "videos/internet/clip.mp4",
            "file_extension": "mp4", "file_size_bytes": size,
            "metadata": {
                "filename": "clip.mp4",
                "fetched_from": "https://www.pexels.com/video/a-clip-1/",
                **({"media_url": media_url} if media_url else {}),
            },
        }
        profile.write_text(json.dumps([rec]), encoding="utf-8")
        proc = subprocess.run(
            [sys.executable, str(SCRIPTS / "populate_archive.py"),
             "--local-source", str(local), "--internet-source", str(internet),
             "--profile", str(profile), "--dest", str(dest), *extra_args],
            capture_output=True, text=True)
        return proc, dest / "videos/internet/clip.mp4"

    def test_a_missing_prestaged_record_is_refetched(self):
        payload = b"\x00\x01fake-mp4-bytes" * 64
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            (served / "clip.mp4").write_bytes(payload)
            httpd, base = self._serve(served)
            try:
                proc, out = self._run(d, f"{base}/clip.mp4", len(payload))
            finally:
                httpd.shutdown()
                httpd.server_close()
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertTrue(out.is_file(),
                            f"nothing was staged:\n{proc.stderr}")
            self.assertEqual(out.read_bytes(), payload,
                             "re-fetched bytes differ from the source")
            self.assertIn("REFETCHED", proc.stderr)

    def test_a_short_download_is_refused_not_staged(self):
        """Staging a wrong file would look pre-staged forever after. The
        recorded byte count is what makes that impossible."""
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            (served / "clip.mp4").write_bytes(b"truncated")
            httpd, base = self._serve(served)
            try:
                proc, out = self._run(d, f"{base}/clip.mp4", 999999)
            finally:
                httpd.shutdown()
                httpd.server_close()
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertFalse(out.exists(), "a short download was staged anyway")
            self.assertFalse(out.with_suffix(".mp4.part").exists(),
                             "the .part file was left behind")
            self.assertIn("size mismatch", proc.stderr)

    def test_no_refetch_restores_verify_only(self):
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            (served / "clip.mp4").write_bytes(b"x" * 32)
            httpd, base = self._serve(served)
            try:
                proc, out = self._run(d, f"{base}/clip.mp4", 32,
                                      extra_args=("--no-refetch",))
            finally:
                httpd.shutdown()
                httpd.server_close()
            self.assertEqual(proc.returncode, 1)
            self.assertFalse(out.exists())
            self.assertIn("MISSING", proc.stderr)

    def test_a_record_with_no_media_url_says_so(self):
        """The message has to name the fix, because the person hitting it
        is on a machine that has never seen the archive share."""
        with tempfile.TemporaryDirectory() as d:
            proc, out = self._run(d, None, 32)
            self.assertEqual(proc.returncode, 1)
            self.assertIn("no metadata.media_url", proc.stderr)


# ---------------------------------------------------------------------------
# #572 — per-team balance
# ---------------------------------------------------------------------------

class TestStudioBalanceShape(unittest.TestCase):
    """The committed profile must keep the shape #572 gave it.

    These read the committed data, so they catch a regenerated profile
    that lost the fill, a recipe edit that starved a team, and a future
    addition that quietly re-concentrates the library — none of which
    break anything loudly.
    """

    @classmethod
    def setUpClass(cls):
        cls.profile = json.loads(
            (SCRIPTS.parent / "profiles" / "studio-a.assets.json")
            .read_text(encoding="utf-8"))
        cls.counts = sb.distribution(cls.profile)
        cls.total = sum(cls.counts.values())

    def test_no_team_is_empty_or_a_stub(self):
        below = {t: n for t, n in self.counts.items() if n < sb.FLOOR}
        self.assertEqual(below, {},
                         f"teams below the floor of {sb.FLOOR}: {below}")

    def test_no_team_owns_the_dataset(self):
        over = {t: round(100 * n / self.total, 1)
                for t, n in self.counts.items()
                if n / self.total > sb.MAX_TEAM_SHARE}
        self.assertEqual(over, {},
                         f"teams above {100 * sb.MAX_TEAM_SHARE:.0f}%: {over}")

    def test_every_team_in_the_catalogue_has_assets(self):
        """A team row with no assets is the original #572 symptom."""
        teams = {t["name"] for t in json.loads(
            (SCRIPTS.parent / "profiles" / "dataset.teams.json")
            .read_text(encoding="utf-8"))}
        self.assertEqual(teams - set(self.counts), set())

    def test_destination_paths_are_unique(self):
        """Two records on one file_path is RULE 1's collision arriving
        from the record side: everything validates, the counts are right,
        and one of the two assets serves the other's bytes."""
        paths = [e["file_path"] for e in self.profile if e.get("file_path")]
        dupes = sorted({p for p in paths if paths.count(p) > 1})
        self.assertEqual(dupes, [])


class TestBalanceProvenance(unittest.TestCase):
    """Every bundle-sourced record must be reconstructible from the
    internet alone — the #602 standard, applied to the Kenney half of the
    library, which had no internet provenance at all before #572."""

    @classmethod
    def setUpClass(cls):
        cls.records = json.loads(
            (UPGRADES / "balance-assets.site_a.json").read_text(encoding="utf-8"))
        cls.posts = json.loads(
            (UPGRADES / "balance-posts.site_a.json").read_text(encoding="utf-8"))

    def test_records_exist(self):
        self.assertGreater(len(self.records), 500)

    def test_every_record_names_a_page_and_an_archive_member(self):
        for r in self.records:
            m = r.get("metadata") or {}
            with self.subTest(id=r["id"]):
                self.assertTrue(
                    (m.get("fetched_from") or "").startswith("https://kenney.nl/assets/"),
                    "fetched_from must be the pack page — it is the CC0 evidence")
                sa = m.get("source_archive") or {}
                self.assertTrue(sa.get("url", "").startswith("https://kenney.nl/"))
                self.assertTrue(sa.get("member"))
                self.assertRegex(sa.get("sha256", ""), r"^[0-9a-f]{64}$")

    def test_no_record_claims_a_media_url(self):
        """A zip cannot serve `file_size_bytes`, so a media_url here would
        be a string that looks checkable and is not — the exact failure
        #602 existed to remove."""
        offenders = [r["id"] for r in self.records
                     if (r.get("metadata") or {}).get("media_url")]
        self.assertEqual(offenders, [])

    def test_every_record_is_reachable_on_browse(self):
        posted = {a for p in self.posts for a in p["asset_ids"]}
        orphans = [r["id"] for r in self.records if r["id"] not in posted]
        self.assertEqual(orphans, [])

    def test_the_pack_provenance_doc_covers_every_recipe_pack(self):
        recorded = {e["pack"] for e in json.loads(
            (UPGRADES / "kenney-pack-sources.json").read_text(encoding="utf-8"))}
        wanted = {r["pack"] for rules in sb.TEAM_RECIPES.values() for r in rules}
        self.assertEqual(wanted - recorded, set())

    def test_recipes_never_name_a_pack_with_no_public_download(self):
        """A record whose bytes exist only inside a paid bundle cannot be
        re-fetched, which is the whole hole this shape closes."""
        named = {r["pack"] for rules in sb.TEAM_RECIPES.values() for r in rules}
        self.assertEqual(named & kps.NOT_PUBLISHED_STANDALONE, set())

    def test_excluded_sources_never_appear_in_the_output(self):
        used = {r.get("balance_source") for r in self.records}
        self.assertEqual(used & set(sb.SOURCE_EXCLUSIONS), set())


class TestTeamCorrections(unittest.TestCase):
    """Moving a mis-teamed record is a data fix, so it has to be exactly
    as reversible and as idempotent as the rest of the upgrade."""

    def _profile(self):
        return [
            {"id": "a", "team_name": "Environment",
             "replaced_source_path": "unpacked/kenney_minimap-pack/x.png"},
            {"id": "b", "team_name": "Environment",
             "source_path": "unpacked/kenney_retro-fantasy-kit/y.obj"},
            {"id": "c", "team_name": "UI",
             "replaced_source_path": "unpacked/kenney_minimap-pack/z.png"},
        ]

    def test_only_matching_records_move(self):
        prof = self._profile()
        up.apply_team_corrections(prof, sb.TEAM_CORRECTIONS)
        self.assertEqual([e["team_name"] for e in prof],
                         ["UI", "Environment", "UI"])

    def test_running_twice_moves_nothing_extra(self):
        prof = self._profile()
        up.apply_team_corrections(prof, sb.TEAM_CORRECTIONS)
        second = up.apply_team_corrections(prof, sb.TEAM_CORRECTIONS)
        self.assertEqual([n for _, n in second], [0, 0, 0])

    def test_matching_uses_the_original_path_not_the_swapped_one(self):
        """After #604 `source_path` is a pool filename, so a correction
        keyed on it would silently match nothing."""
        prof = [{"id": "a", "team_name": "Environment",
                 "source_path": "2d-assets-minimap-pack-x-deadbeef-512.png",
                 "replaced_source_path": "unpacked/kenney_minimap-pack/x.png"}]
        up.apply_team_corrections(prof, sb.TEAM_CORRECTIONS)
        self.assertEqual(prof[0]["team_name"], "UI")


class TestPackRootAudit(unittest.TestCase):
    """apply_upgrade must refuse a bundle record it cannot reconstruct."""

    def _record(self, metadata):
        return {
            "id": "p1", "file_path": "3d/kenney-allin1/x.glb",
            "source_root": up.PACK_SOURCE_ROOT, "source_path": "Pack/x.glb",
            "metadata": metadata,
        }

    def _audit(self, rec):
        posts = [{"id": "post-1", "asset_ids": [rec["id"]]}]
        return up.audit([rec], posts, [], [rec], posts)

    def test_a_complete_record_passes(self):
        rec = self._record({
            "filename": "x.glb",
            "fetched_from": "https://kenney.nl/assets/pack",
            "source_archive": {"url": "https://kenney.nl/a.zip",
                               "member": "x.glb", "sha256": "0" * 64},
        })
        self.assertEqual(self._audit(rec), [])

    def test_a_missing_source_archive_is_a_problem(self):
        rec = self._record({"filename": "x.glb",
                            "fetched_from": "https://kenney.nl/assets/pack"})
        problems = self._audit(rec)
        self.assertTrue(any("source_archive" in p for p in problems), problems)

    def test_a_missing_page_url_is_a_problem(self):
        rec = self._record({
            "filename": "x.glb",
            "source_archive": {"url": "https://kenney.nl/a.zip",
                               "member": "x.glb", "sha256": "0" * 64},
        })
        problems = self._audit(rec)
        self.assertTrue(any("fetched_from" in p for p in problems), problems)

    def test_two_records_on_one_destination_is_a_problem(self):
        rec = self._record({
            "filename": "x.glb",
            "fetched_from": "https://kenney.nl/assets/pack",
            "source_archive": {"url": "https://kenney.nl/a.zip",
                               "member": "x.glb", "sha256": "0" * 64},
        })
        other = json.loads(json.dumps(rec))
        other["id"] = "p2"
        posts = [{"id": "post-1", "asset_ids": ["p1", "p2"]}]
        problems = up.audit([rec, other], posts, [], [rec, other], posts)
        self.assertTrue(any("share a file_path" in p for p in problems), problems)


class TestArchiveMemberRefetch(unittest.TestCase):
    """Extracting one member of a remote zip, with the hash as the gate.

    Served from `http.server` on 127.0.0.1 over a zip built here — no
    network, no NAS, per the fixture rule at the top of this file.
    """

    @staticmethod
    def _zip(path: Path, members: dict[str, bytes]) -> None:
        import zipfile as zf
        with zf.ZipFile(path, "w") as z:
            for name, data in members.items():
                z.writestr(name, data)

    def _serve(self, directory):
        handler = partial(SimpleHTTPRequestHandler, directory=str(directory))
        httpd = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        return httpd, f"http://127.0.0.1:{httpd.server_address[1]}"

    def test_the_member_is_extracted_when_the_hash_agrees(self):
        payload = b"glb-bytes" * 32
        digest = hashlib.sha256(payload).hexdigest()
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            self._zip(served / "pack.zip", {"Models/x.glb": payload,
                                            "readme.txt": b"hi"})
            httpd, base = self._serve(served)
            try:
                pa._ZIP_CACHE.clear()
                out = Path(d) / "out" / "x.glb"
                ok, note = pa.refetch_member(f"{base}/pack.zip", "Models/x.glb",
                                             digest, out)
                got = out.read_bytes() if out.is_file() else None
            finally:
                httpd.shutdown()
                httpd.server_close()
        self.assertTrue(ok, note)
        self.assertEqual(got, payload)

    def test_a_changed_pack_fails_loudly_and_stages_nothing(self):
        """The upstream pack moving under us must not silently swap the
        art. The hash is the byte count's stand-in for an archive."""
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            self._zip(served / "pack.zip", {"Models/x.glb": b"different"})
            httpd, base = self._serve(served)
            try:
                pa._ZIP_CACHE.clear()
                out = Path(d) / "out" / "x.glb"
                ok, note = pa.refetch_member(f"{base}/pack.zip", "Models/x.glb",
                                             "0" * 64, out)
                staged = out.exists()
            finally:
                httpd.shutdown()
                httpd.server_close()
        self.assertFalse(ok)
        self.assertIn("sha256 mismatch", note)
        self.assertFalse(staged, "a mismatched member was staged anyway")

    def test_an_absent_member_is_named(self):
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            self._zip(served / "pack.zip", {"other.glb": b"x"})
            httpd, base = self._serve(served)
            try:
                pa._ZIP_CACHE.clear()
                ok, note = pa.refetch_member(f"{base}/pack.zip", "Models/x.glb",
                                             "0" * 64, Path(d) / "out.glb")
            finally:
                httpd.shutdown()
                httpd.server_close()
        self.assertFalse(ok)
        self.assertIn("member not in zip", note)


class TestCompanionsSurviveASkippedModel(unittest.TestCase):
    """Sponza's actual bug: companions were staged only on the COPY path.

    A model already present at the destination short-circuited past its
    own siblings, so `3d/internet/Sponza.gltf` sat there naming a .bin
    and 69 textures that were never copied — and it was the only 3D asset
    in the instance stuck at `failed`. Matching the model's size proves
    nothing about the 70 files beside it.
    """

    def test_a_preexisting_model_still_gets_its_siblings(self):
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            local = d / "local"
            (local / "3d").mkdir(parents=True)
            (local / "metadata.csv").write_text("file_path,title\n",
                                                encoding="utf-8")
            model = local / "3d" / "scene.gltf"
            model.write_text(json.dumps({
                "buffers": [{"uri": "scene.bin"}],
                "images": [{"uri": "tex.png"}],
            }), encoding="utf-8")
            (local / "3d" / "scene.bin").write_bytes(b"buffer-bytes")
            (local / "3d" / "tex.png").write_bytes(b"texture-bytes")

            dest = d / "dest"
            (dest / "3d/internet").mkdir(parents=True)
            # The model — and ONLY the model — is already staged.
            (dest / "3d/internet/scene.gltf").write_bytes(model.read_bytes())

            profile = d / "profile.json"
            profile.write_text(json.dumps([{
                "id": "m1", "asset_type": "3d",
                "file_path": "3d/internet/scene.gltf",
                "source_root": "local", "source_path": "3d/scene.gltf",
                "file_extension": "gltf",
                "file_size_bytes": model.stat().st_size,
                "metadata": {"filename": "scene.gltf"},
            }]), encoding="utf-8")

            proc = subprocess.run(
                [sys.executable, str(SCRIPTS / "populate_archive.py"),
                 "--local-source", str(local),
                 "--internet-source", str(d / "internet"),
                 "--profile", str(profile), "--dest", str(dest)],
                capture_output=True, text=True)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertTrue((dest / "3d/internet/scene.bin").is_file(),
                            f"buffer not staged:\n{proc.stderr}")
            self.assertTrue((dest / "3d/internet/tex.png").is_file(),
                            f"texture not staged:\n{proc.stderr}")


class TestAbsentSourceIsNotAbsentAsset(unittest.TestCase):
    """The internet cache is gitignored and usually not on a machine that
    already has a populated site. Reporting 58 fully-staged videos as
    MISSING and exiting 1 is unavailable-is-not-absent again."""

    def _run(self, d, staged_bytes, manifest_size):
        d = Path(d)
        local = d / "local"
        local.mkdir()
        (local / "metadata.csv").write_text("file_path,title\n", encoding="utf-8")
        dest = d / "dest"
        (dest / "videos/internet").mkdir(parents=True)
        if staged_bytes is not None:
            (dest / "videos/internet/clip.mp4").write_bytes(staged_bytes)
        profile = d / "profile.json"
        profile.write_text(json.dumps([{
            "id": "v1", "asset_type": "video",
            "file_path": "videos/internet/clip.mp4",
            "source_root": "internet", "source_path": "videos/clip.mp4",
            "file_extension": "mp4", "file_size_bytes": manifest_size,
            "metadata": {"filename": "clip.mp4"},
        }]), encoding="utf-8")
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "populate_archive.py"),
             "--local-source", str(local),
             "--internet-source", str(d / "internet-cache"),
             "--profile", str(profile), "--dest", str(dest)],
            capture_output=True, text=True)

    def test_a_staged_file_with_no_source_is_not_missing(self):
        with tempfile.TemporaryDirectory() as d:
            proc = self._run(d, b"x" * 100, 100)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertNotIn("MISSING", proc.stderr)

    def test_a_short_staged_file_is_still_missing(self):
        """The manifest's byte count is what keeps this from becoming a
        blanket excuse."""
        with tempfile.TemporaryDirectory() as d:
            proc = self._run(d, b"x" * 3, 100)
        self.assertEqual(proc.returncode, 1)
        self.assertIn("MISSING", proc.stderr)


class TestPexelsAdditions(unittest.TestCase):
    """The #675 regression — a licence claim scoped to one site, fixed in
    the output and left in the inputs — must not come back in new data."""

    @classmethod
    def setUpClass(cls):
        cls.records = json.loads(
            (UPGRADES / "pexels-assets.site_a.json").read_text(encoding="utf-8"))

    def test_both_provenance_keys_are_present(self):
        for r in self.records:
            m = r["metadata"]
            with self.subTest(id=r["id"]):
                self.assertTrue(m["fetched_from"].startswith("https://www.pexels.com/"))
                self.assertTrue(m["media_url"].startswith("https://videos.pexels.com/"))
                self.assertGreater(r["file_size_bytes"], 0)

    def test_no_record_scopes_the_licence_to_one_site(self):
        offenders = [r["id"] for r in self.records if "site_b only" in json.dumps(r)]
        self.assertEqual(offenders, [])

    def test_the_search_that_produced_each_record_is_recorded(self):
        """The query list is the editorial decision; a record that does
        not say which search found it cannot be re-derived or argued
        with."""
        queries = {q["q"] for q in px.QUERIES}
        for r in self.records:
            self.assertIn(r["metadata"].get("search_query"), queries)

    def test_the_videos_land_across_teams(self):
        teams = {r["team_name"] for r in self.records}
        self.assertGreaterEqual(len(teams), 3, f"all in {teams}")


class TestAliasProfilesTrackTheirSource(unittest.TestCase):
    """`dev` and `demo` are aliases, and were written BEFORE the upgrade
    pass — so every upgrade since #604 landed on studio-{a,b} and missed
    its own aliases. demo shipped 971 records against studio-a's 1,007:
    a demo re-seed would have dropped all 36 added videos silently."""

    def test_demo_matches_studio_a(self):
        p = SCRIPTS.parent / "profiles"
        self.assertEqual(
            json.loads((p / "demo.assets.json").read_text(encoding="utf-8")),
            json.loads((p / "studio-a.assets.json").read_text(encoding="utf-8")))

    def test_dev_matches_studio_b(self):
        p = SCRIPTS.parent / "profiles"
        self.assertEqual(
            json.loads((p / "dev.assets.json").read_text(encoding="utf-8")),
            json.loads((p / "studio-b.assets.json").read_text(encoding="utf-8")))


class TestAIProvenance(unittest.TestCase):
    """⛔ NOBODY ELSE'S WORK MAY BE CALLED AI (#1260).

    `ai_provenance` is a claim about how the bytes were made, written on
    a record that also names a creator — so putting it on someone else's
    work publishes a false statement about that person, and site_a is
    published to Kaggle.

    That is not hypothetical. Two upgrade documents
    (`ai-declarations.site_a.json` and its site_b twin) declared
    `generated` on FOUR Kenney.nl works, and the four committed profiles
    carried the key. `apply_upgrade --check` reported "OK: profile
    already reflects the upgrade" on every one of them, because being
    already-applied was the only thing it asked.

    The tests below are the two halves that were missing. The first is
    the invariant over the finished profile, which is where BOTH routes
    to the claim converge — `apply_ai_declarations` writing it onto an
    existing record, and `merge_added` carrying it in on a new one. The
    second is the pair the toggle needs, asserted where it now lives: on
    the content we actually generated.
    """

    PROFILES = (("site_a", "studio-a.assets.json", "studio-a.posts.json"),
                ("site_b", "studio-b.assets.json", "studio-b.posts.json"),
                ("demo",   "demo.assets.json",     "studio-a.posts.json"),
                ("dev",    "dev.assets.json",      "studio-b.posts.json"))

    def _profile(self, name):
        p = SCRIPTS.parent / "profiles" / name
        return json.loads(p.read_text(encoding="utf-8"))

    def test_no_third_party_work_is_declared_ai(self):
        for site, prof, _ in self.PROFILES:
            for e in self._profile(prof):
                if not e.get("ai_provenance"):
                    continue
                src = (e.get("metadata") or {}).get("acquisition_source") or ""
                self.assertTrue(
                    src.startswith(up.AI_DECLARABLE_SOURCE_PREFIXES),
                    f"{site}: {e['id']} ({e.get('title')!r}) declares "
                    f"ai_provenance={e['ai_provenance']!r} but came from {src!r} and "
                    f"is attributed to {e.get('attribution')!r}. That is a false "
                    f"statement about a named creator in a published dataset.")

    def test_the_audit_refuses_a_third_party_declaration(self):
        """The guard, not just its current result — the profiles are
        clean today and would stay clean if the rule were deleted."""
        prof = [{"id": "k", "title": "Brick pack", "file_path": "images/k.png",
                 "attribution": "Kenney (kenney.nl)", "ai_provenance": "generated",
                 "source_root": "hq",
                 "metadata": {"acquisition_source": "Kenney.nl"}}]
        problems = up.audit(prof, [], [], [], [])
        self.assertTrue(
            any("false statement" in p for p in problems),
            f"a third-party AI declaration must stop the run; got {problems}")

    def test_an_in_house_declaration_passes_the_audit(self):
        prof = [{"id": "g", "title": "Ref plate", "file_path": "images/g.png",
                 "attribution": "Aurora R&D — AI-generated", "ai_provenance": "generated",
                 "source_root": "local", "source_path": "g.png",
                 "metadata": {"acquisition_source":
                              "Generated in-house (Stable Diffusion 3.5 Large via ComfyUI)"}}]
        problems = up.audit(prof, [], [], [], [])
        self.assertEqual(
            [p for p in problems if "false statement" in p], [],
            "work we generated ourselves must be declarable")

    def test_the_declared_corpus_is_exactly_the_work_we_made(self):
        """A declaration that arrived by any other route is the bug.

        Two docs now, not one: #1260's 45 Stable Diffusion plates, all
        `generated`, and #1290's two authored plates carrying `assisted`
        and `none`. The invariant is unchanged — a declaration may only
        ride in on a record we made — but it is no longer a synonym for
        "the AI-generated images", because `none` is a declaration about
        work with no model in it at all."""
        expected = set()
        for doc in ("generated-assets.site_a.json", "authored-assets.site_a.json"):
            expected |= {a["id"] for a in
                         json.loads((UPGRADES / doc).read_text(encoding="utf-8"))}
        self.assertEqual(len(expected), 47)
        for site, prof, _ in (self.PROFILES[0], self.PROFILES[2]):
            got = {e["id"] for e in self._profile(prof) if e.get("ai_provenance")}
            self.assertEqual(got, expected, f"{site}: declared set drifted")
        for site, prof, _ in (self.PROFILES[1], self.PROFILES[3]):
            got = {e["id"] for e in self._profile(prof) if e.get("ai_provenance")}
            self.assertEqual(got, set(), f"{site}: site_b has no generated content")

    def test_declaring_leaves_the_acquisition_stamp_alone(self):
        """⛔ ADR 0095. The fixture sweep partitions the asset table on
        `metadata.acquisition_source` alone; a seeded asset without it is
        indistinguishable from real uploaded content and becomes
        sweep-bait. Declaring AI must not cost an asset that stamp."""
        for site, prof, _ in self.PROFILES:
            for e in self._profile(prof):
                if not e.get("ai_provenance"):
                    continue
                self.assertIn("acquisition_source", e.get("metadata") or {},
                              f"{site}: declared asset {e['id']} lost its seed stamp")

    def test_the_toggle_has_a_pure_post_and_a_mixed_one(self):
        """⭐ THE PAIR IS THE FIXTURE (ADR 0094 fourth amendment).

        `ai_pure` is unanimity over `generated` across the members UNION
        the covers, and the seeder makes members[0] the cover — so a post
        whose every member is declared is pure. One post alone cannot show
        the ruling: a filter keyed on the LABELLING column hides the mixed
        post too and passes every test that only reads the pure one."""
        posts = json.loads((UPGRADES / "generated-posts.site_a.json")
                           .read_text(encoding="utf-8"))
        declared = {a["id"] for a in json.loads(
            (UPGRADES / "generated-assets.site_a.json").read_text(encoding="utf-8"))}
        pure = [p for p in posts if set(p["asset_ids"]) <= declared]
        mixed = [p for p in posts if set(p["asset_ids"]) & declared
                 and not set(p["asset_ids"]) <= declared]
        self.assertTrue(pure, "no post has an all-declared membership — nothing is ai_pure")
        self.assertEqual(
            len(mixed), 1,
            "exactly one post must mix a declared member with an undeclared one; "
            f"got {[p['title'] for p in mixed]}")
        for p in mixed:
            others = [a for a in p["asset_ids"] if a not in declared]
            profile = {e["id"] for e in self._profile("studio-a.assets.json")}
            for a in others:
                self.assertIn(a, profile,
                              "the mixed post's undeclared member must exist in the profile")

    def test_every_generated_asset_is_reachable_on_browse(self):
        """An asset with no post is invisible, and a declared one that
        nobody can see makes the toggle demonstrate nothing."""
        add = json.loads((UPGRADES / "generated-assets.site_a.json")
                         .read_text(encoding="utf-8"))
        posts = json.loads((UPGRADES / "generated-posts.site_a.json")
                           .read_text(encoding="utf-8"))
        referenced = {a for p in posts for a in p["asset_ids"]}
        orphans = [a["id"] for a in add if a["id"] not in referenced]
        self.assertEqual(orphans, [])

    def test_the_deleted_declaration_docs_stay_deleted(self):
        """The two docs are gone and the profiles no longer carry what
        they wrote. Re-adding one is how the false claim comes back."""
        for site in ("site_a", "site_b"):
            self.assertFalse(
                (UPGRADES / f"ai-declarations.{site}.json").exists(),
                f"ai-declarations.{site}.json is back — every id it named was a "
                f"Kenney.nl work. A declaration belongs on the record that "
                f"introduces content we made (generated-assets.*).")

    def test_the_committed_profiles_are_already_upgraded(self):
        """Applying the docs to the committed profile is a NO-OP, which
        is what "already upgraded" means for every other doc here."""
        p = SCRIPTS.parent / "profiles"
        entries = json.loads((p / "studio-a.assets.json").read_text(encoding="utf-8"))
        posts = json.loads((p / "studio-a.posts.json").read_text(encoding="utf-8"))
        add = json.loads((UPGRADES / "generated-assets.site_a.json")
                         .read_text(encoding="utf-8"))
        add_p = json.loads((UPGRADES / "generated-posts.site_a.json")
                           .read_text(encoding="utf-8"))
        self.assertEqual(up.merge_added(entries, add), (0, 0))
        self.assertEqual(up.merge_posts(posts, add_p), 0)

    def test_an_unknown_id_is_reported_rather_than_ignored(self):
        prof = [{"id": "a"}]
        out = up.apply_ai_declarations(
            prof, [{"id": "nope", "ai_provenance": "generated", "role": "pure"}])
        self.assertTrue(any(o.startswith("MISSING") for _, o in out),
                        "a declaration naming an id the profile lacks must be reported; "
                        "silence here is a toggle that quietly hides nothing")

    def test_applying_twice_is_idempotent(self):
        prof = [{"id": "a"}]
        doc = [{"id": "a", "ai_provenance": "generated", "role": "pure"}]
        up.apply_ai_declarations(prof, doc)
        up.apply_ai_declarations(prof, doc)
        self.assertEqual(prof, [{"id": "a", "ai_provenance": "generated"}])


class TestArchiveRecordsAreOutOfMediaUrlScope(unittest.TestCase):
    """resolve_media_urls' gate must not fail 895 records for lacking a
    field that cannot exist for them — nor stop checking the ones it
    can."""

    def test_a_source_archive_record_is_skipped(self):
        doc = [{"id": "a", "metadata": {
            "fetched_from": "https://kenney.nl/assets/ui-pack",
            "source_archive": {"url": "https://kenney.nl/a.zip",
                               "member": "x.png", "sha256": "0" * 64}}}]
        self.assertEqual(rmu.internet_records(doc), [])

    def test_a_plain_internet_record_is_still_checked(self):
        doc = [{"id": "b", "metadata": {
            "fetched_from": "https://www.pexels.com/video/x-1/"}}]
        self.assertEqual(len(rmu.internet_records(doc)), 1)


class TestUncataloguedClassification(unittest.TestCase):
    """"Uncatalogued" is three different things (#722).

    The bug this guards is a wrong ANSWER, not a crash. Ask "what is
    uncatalogued?" with a one-hop companion walk and you over-count by
    every OBJ texture; ask it without reading the replacements doc and
    you get 260 files that look like never-catalogued assets but are the
    superseded halves of a shipped upgrade. Cataloguing either kind
    creates duplicate records for bytes that are already accounted for.

    Fixture is a synthetic site in a temp dir — no archive share, per the
    rule at the top of this file.
    """

    def _site(self, root: Path) -> Path:
        site = root / "site"
        (site / "images/pack").mkdir(parents=True)
        (site / "3d/model").mkdir(parents=True)

        # A catalogued OBJ whose texture is TWO hops away.
        (site / "3d/model/thing.obj").write_text(
            "mtllib thing.mtl\nv 0 0 0\n", encoding="utf-8")
        (site / "3d/model/thing.mtl").write_text(
            "newmtl m\nmap_Kd thing_diffuse.png\n", encoding="utf-8")
        (site / "3d/model/thing_diffuse.png").write_bytes(b"tex")

        # A catalogued image, a superseded original, a real orphan.
        (site / "images/pack/kept.png").write_bytes(b"kept")
        (site / "images/pack/old.png").write_bytes(b"old")
        (site / "images/pack/nobody.png").write_bytes(b"nobody")

        (site / "MANIFEST.json").write_text(json.dumps([
            {"id": "a", "file_path": "3d/model/thing.obj"},
            {"id": "b", "file_path": "images/pack/kept.png"},
            # The record that USED to point at old.png; it moved on.
            {"id": "c", "file_path": "images/kenney-hq/new.png"},
        ]), encoding="utf-8")

        upgrades = root / "upgrades"
        upgrades.mkdir()
        (upgrades / "kenney-hq-replacements.site_a.json").write_text(
            json.dumps([{"id": "c", "old": "images/pack/old.png",
                         "new": "images/kenney-hq/new.png", "newSize": 9}]),
            encoding="utf-8")
        return site

    def test_the_three_kinds_are_told_apart(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            site = self._site(root)
            orphans, companions, superseded = au.classify(
                site, root / "upgrades", "site_a")
            self.assertEqual(orphans, ["images/pack/nobody.png"])
            self.assertEqual(superseded, ["images/pack/old.png"])
            self.assertEqual(
                sorted(companions),
                ["3d/model/thing.mtl", "3d/model/thing_diffuse.png"])

    def test_the_second_companion_hop_is_walked(self):
        """One hop reaches the .mtl and stops, which reports the texture
        as an orphan. That miscount is what made the gap look 200 files
        bigger than it is."""
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            site = self._site(root)
            orphans, companions, _ = au.classify(
                site, root / "upgrades", "site_a")
            self.assertIn("3d/model/thing_diffuse.png", companions)
            self.assertNotIn("3d/model/thing_diffuse.png", orphans)

    def test_a_reverted_replacement_is_not_reported_as_dead(self):
        """`prune` deletes what this call returns. A path the catalogue
        still names must never appear in it, however stale the
        replacements doc gets."""
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            site = self._site(root)
            manifest = json.loads((site / "MANIFEST.json").read_text())
            manifest.append({"id": "d", "file_path": "images/pack/old.png"})
            (site / "MANIFEST.json").write_text(json.dumps(manifest),
                                                encoding="utf-8")
            orphans, _, superseded = au.classify(
                site, root / "upgrades", "site_a")
            self.assertEqual(superseded, [])
            self.assertNotIn("images/pack/old.png", orphans)

    def test_no_replacements_doc_means_nothing_is_superseded(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            site = self._site(root)
            (root / "upgrades/kenney-hq-replacements.site_a.json").unlink()
            orphans, _, superseded = au.classify(
                site, root / "upgrades", "site_a")
            self.assertEqual(superseded, [])
            self.assertIn("images/pack/old.png", orphans)


class TestSiteAHasNoUncataloguedAssets(unittest.TestCase):
    """The finding behind #722, asserted against the committed docs.

    #722 was filed as "260 assets were never catalogued". They were: the
    260 paths are exactly the `old` column of the site_a replacements
    doc, so each one HAS a record — the record was repointed at a
    kenney-hq render and the file was left on the share. Adding records
    for them would give 260 pieces of content two entries each.

    This runs off the committed upgrade docs alone, so it holds on a
    machine that has never seen the archive.
    """

    def test_the_reported_orphans_are_all_replacement_leftovers(self):
        reps = json.loads(
            (UPGRADES / "kenney-hq-replacements.site_a.json").read_text())
        old = {r["old"] for r in reps}
        self.assertEqual(len(old), len(reps),
                         "two replacements claim the same old file")
        # Every superseded original is a path the profile no longer names.
        profile = json.loads(
            (SCRIPTS.parent / "profiles" / "studio-a.assets.json").read_text())
        live = {e.get("file_path") for e in profile}
        self.assertEqual(sorted(old & live), [],
                         "a replaced record still points at its old file — "
                         "the upgrade did not apply")

    def test_every_replacement_names_the_file_it_supersedes(self):
        """`prune` is driven entirely off this column. A replacement with
        no `old` leaves bytes on a published share that nothing can ever
        identify as dead."""
        for site in ("site_a", "site_b"):
            reps = json.loads(
                (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text())
            missing = [r["id"] for r in reps if not r.get("old")]
            self.assertEqual(missing, [], f"{site}: {len(missing)} "
                             "replacement(s) do not say what they replaced")


class TestPoolSizesAgreeAcrossDocuments(unittest.TestCase):
    """⭐ #1294, CATCHABLE WITHOUT THE POOL AND WITHOUT THE SHARE.

    `newSize` in a replacements doc and `file_size_bytes` on a
    balance-doc record are both the byte count of ONE file in the pool.
    Where two committed documents name the same pool filename they are
    making the same claim about the same bytes, and they cannot both be
    right when they differ.

    Measured on `dev` at b3374cba, before the repair: **115 pool
    filenames carried two different sizes**, because `balance-assets.*`
    was emitted after #630/#685 changed what frame a vector renders into
    and `kenney-hq-replacements.*` was not. That contradiction sat in the
    repository for months, needing neither the archive share nor a built
    pool to see — which is the point of this test. #1294 was filed as
    "someone with the pool mounted should compare"; the repo was
    disagreeing with itself the whole time.
    """

    def _claims(self):
        claims: dict[str, set] = {}
        for site in ("site_a", "site_b"):
            for r in json.loads(
                    (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text()):
                claims.setdefault(r["new"].rsplit("/", 1)[-1], set()).add(
                    (f"kenney-hq-replacements.{site}.json", r["newSize"]))
            bal = UPGRADES / f"balance-assets.{site}.json"
            if not bal.is_file():
                continue
            for e in json.loads(bal.read_text()):
                if e.get("source_root") == "hq":
                    claims.setdefault(e["source_path"], set()).add(
                        (f"balance-assets.{site}.json", e["file_size_bytes"]))
        return claims

    def test_one_pool_file_has_one_size(self):
        claims = self._claims()
        disagree = {k: sorted(v) for k, v in claims.items()
                    if len({size for _, size in v}) > 1}
        self.assertEqual(
            disagree, {},
            f"{len(disagree)} pool file(s) are given two different byte "
            "counts by two committed documents. Rebuild the pool and re-run: "
            "python3 seed/scripts/kenney_hq.py sizes --pool <dir> "
            "--replacements seed/upgrades/kenney-hq-replacements.site_a.json "
            "--replacements seed/upgrades/kenney-hq-replacements.site_b.json "
            "--write")

    def test_the_check_is_not_vacuous(self):
        """⚠️ It only means something while the documents actually overlap.
        If a later selection pass stopped sharing pool files between the
        replacement and balance docs, the assertion above would pass by
        describing nothing."""
        claims = self._claims()
        overlapping = [k for k, v in claims.items()
                       if len({src.split(".")[0] for src, _ in v}) > 1]
        self.assertGreater(
            len(overlapping), 50,
            "the replacement and balance documents no longer describe the "
            "same pool files, so cross-checking them proves nothing")


class TestPoolSizeReMeasurement(unittest.TestCase):
    """`kenney_hq.py sizes` — the command that keeps #1294 from recurring.

    A `newSize` is a measurement of a RENDER, and a rasteriser fix moves
    it. Nothing re-derived these values: they were measured once by hand,
    so #630 and #685 each silently invalidated a slice of them. The
    command makes re-measurement a command instead of a procedure.
    """

    def _pool(self, td: Path, sizes: dict[str, int]) -> Path:
        pool = td / "pool"
        pool.mkdir()
        for name, n in sizes.items():
            (pool / name).write_bytes(b"\0" * n)
        return pool

    def _doc(self, td: Path, rows) -> Path:
        doc = td / "kenney-hq-replacements.site_a.json"
        doc.write_text(json.dumps(rows), encoding="utf-8")
        return doc

    def test_a_matching_doc_passes_and_is_left_alone(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            pool = self._pool(td, {"a-11111111-512.png": 500})
            doc = self._doc(td, [{"id": "x", "old": "o", "oldSize": 9,
                                  "new": "images/kenney-hq/a-11111111-512.png",
                                  "newSize": 500}])
            before = doc.read_text()
            self.assertEqual(hq.cmd_sizes(pool, [doc], write=False), 0)
            self.assertEqual(doc.read_text(), before)

    def test_a_stale_size_is_reported_and_the_command_refuses(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            pool = self._pool(td, {"a-11111111-512.png": 500})
            doc = self._doc(td, [{"id": "x", "old": "o", "oldSize": 9,
                                  "new": "images/kenney-hq/a-11111111-512.png",
                                  "newSize": 250}])
            self.assertEqual(hq.cmd_sizes(pool, [doc], write=False), 1,
                             "report-only mode must exit non-zero so it can "
                             "stand as a gate")
            self.assertEqual(json.loads(doc.read_text())[0]["newSize"], 250,
                             "report-only mode wrote to the document")

    def test_write_re_measures_and_then_reaches_a_fixed_point(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            pool = self._pool(td, {"a-11111111-512.png": 500})
            doc = self._doc(td, [{"id": "x", "old": "o", "oldSize": 9,
                                  "new": "images/kenney-hq/a-11111111-512.png",
                                  "newSize": 250}])
            self.assertEqual(hq.cmd_sizes(pool, [doc], write=True), 0)
            self.assertEqual(json.loads(doc.read_text())[0]["newSize"], 500)
            self.assertEqual(hq.cmd_sizes(pool, [doc], write=False), 0)

    def test_an_absent_pool_file_is_a_different_problem_from_a_stale_size(self):
        """⛔ The empty case. A row naming a file the pool cannot produce
        has nothing to measure, and silently 'fixing' it — or counting it
        among the stale sizes — would let a dropped pool entry read as a
        successful repair."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            pool = self._pool(td, {"a-11111111-512.png": 500})
            doc = self._doc(td, [{"id": "x", "old": "o", "oldSize": 9,
                                  "new": "images/kenney-hq/gone-22222222-512.png",
                                  "newSize": 250}])
            self.assertEqual(hq.cmd_sizes(pool, [doc], write=True), 1)
            self.assertEqual(json.loads(doc.read_text())[0]["newSize"], 250,
                             "a row with no pool file must not be rewritten")

    def test_write_preserves_the_committed_serialisation(self):
        """The docs are diffed by humans. A re-measure that reflowed the
        whole file would bury 622 changed integers in 4,000 moved lines."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            pool = self._pool(td, {"a-11111111-512.png": 500})
            rows = [{"id": "x", "new": "images/kenney-hq/a-11111111-512.png",
                     "newSize": 250, "old": "o", "oldSize": 9}]
            doc = td / "kenney-hq-replacements.site_a.json"
            doc.write_text(json.dumps(rows, indent=1, sort_keys=True,
                                      ensure_ascii=False) + "\n",
                           encoding="utf-8")
            hq.cmd_sizes(pool, [doc], write=True)
            rows[0]["newSize"] = 500
            self.assertEqual(
                doc.read_text(),
                json.dumps(rows, indent=1, sort_keys=True,
                           ensure_ascii=False) + "\n")


class TestPackPageSlugs(unittest.TestCase):
    def test_slug_comes_off_the_pack_directory_name(self):
        self.assertEqual(kps.page_for("UI assets/UI Pack"),
                         "https://kenney.nl/assets/ui-pack")

    def test_overrides_win(self):
        self.assertEqual(kps.page_for("2D assets/Platformer Characters 1"),
                         "https://kenney.nl/assets/platformer-characters")


class TestManifestGuard(unittest.TestCase):
    """#1275 — publishing must not be able to make the destination poorer.

    The bug was silent by construction: `populate_archive.py` copies the
    profile over the site's MANIFEST.json, so a profile that had fallen
    behind the published dataset deleted the difference without a word.
    """

    @staticmethod
    def _rec(rid, **kw):
        base = {"id": rid, "title": f"asset {rid}", "field_values": {}}
        base.update(kw)
        return base

    def test_identical_sides_lose_nothing(self):
        recs = [self._rec("a", field_values={"k": "v"}), self._rec("b")]
        cmp = mg.compare(recs, json.loads(json.dumps(recs)), "assets")
        self.assertEqual(cmp.losses, [])
        self.assertTrue(cmp.ok)

    def test_a_record_only_at_the_destination_is_a_loss(self):
        cmp = mg.compare([self._rec("a")], [self._rec("a"), self._rec("b")], "assets")
        self.assertEqual([x.kind for x in cmp.losses], [mg.MISSING_RECORD])
        self.assertEqual(cmp.losses[0].record_id, "b")
        self.assertFalse(cmp.ok)

    def test_the_source_being_ahead_is_not_a_loss(self):
        """The normal direction. A profile with MORE than the site is what
        publishing is FOR, and the guard must not stand in its way."""
        cmp = mg.compare([self._rec("a"), self._rec("b")], [self._rec("a")], "assets")
        self.assertEqual(cmp.losses, [])
        self.assertEqual(cmp.added, ["b"])
        self.assertTrue(cmp.ok)

    def test_missing_key_emptied_value_and_changed_value_are_three_cases(self):
        src = [self._rec("a", field_values={"kept": "x", "emptied": ""},
                         license="CC0 1.0")]
        dst = [self._rec("a", field_values={"kept": "y", "emptied": "was here",
                                            "gone": "also here"},
                         license="CC-BY 4.0")]
        cmp = mg.compare(src, dst, "assets")
        kinds = sorted((x.kind, x.key) for x in cmp.losses)
        self.assertEqual(kinds, [(mg.EMPTIED_VALUE, "field_values.emptied"),
                                 (mg.MISSING_KEY, "field_values.gone")])
        # A different non-empty value on both sides is an EDIT. Refusing
        # it would make the profile unable to correct anything it has
        # already published, which is the opposite of the point.
        self.assertEqual(sorted(x.key for x in cmp.changes),
                         ["field_values.kept", "license"])
        self.assertFalse(cmp.ok)

    def test_false_is_a_value_not_an_absence(self):
        """`mature: false` is a declaration. 1,947 of them were about to
        be dropped, and an `if not value` check would have called that
        nothing."""
        cmp = mg.compare([self._rec("a")], [self._rec("a", mature=False)], "assets")
        self.assertEqual([x.key for x in cmp.losses], ["mature"])
        self.assertFalse(mg.is_empty(False))
        self.assertFalse(mg.is_empty(0))
        self.assertTrue(mg.is_empty(""))
        self.assertTrue(mg.is_empty({}))

    def test_duplicate_ids_in_the_source_are_refused(self):
        cmp = mg.compare([self._rec("a"), self._rec("a")], [self._rec("a")], "posts")
        self.assertEqual(cmp.duplicates, {"a": 2})
        self.assertFalse(cmp.ok)

    def test_a_destination_that_does_not_exist_yet_is_a_first_publish(self):
        cmp = mg.compare([self._rec("a")], None, "assets")
        self.assertEqual(cmp.losses, [])
        self.assertTrue(cmp.ok)

    def test_an_unreadable_destination_raises_rather_than_reading_empty(self):
        """⛔ "Unreadable" must never collapse to "empty". That mistake
        turns the guard into a rubber stamp on exactly the run that most
        needs stopping."""
        with tempfile.TemporaryDirectory() as td:
            p = Path(td) / "MANIFEST.json"
            p.write_text('[{"id": "a"},')
            with self.assertRaises(ValueError) as cm:
                mg.load_json_list(p)
            self.assertIn("MANIFEST.json", str(cm.exception))
            self.assertIsNone(mg.load_json_list(Path(td) / "absent.json"))


class TestManifestReconcile(unittest.TestCase):
    """#1275 — the repair that lets the guard ever pass."""

    def test_the_pass_can_only_add_never_replace(self):
        """The safety property, stated as a test. A repair tool aimed at
        published data must not be able to overwrite a later edit."""
        profile = [_asset("a", "images/a.png", field_values={"keep": "mine"},
                          license="CC-BY 4.0")]
        doc = {"fill": [{"id": "a", "field_values": {"keep": "theirs",
                                                     "add": "new"},
                         "license": "CC0 1.0", "mature": False}]}
        added, filled = up.apply_manifest_reconcile(profile, doc)
        self.assertEqual((added, filled), (0, 2))
        self.assertEqual(profile[0]["field_values"], {"keep": "mine", "add": "new"})
        self.assertEqual(profile[0]["license"], "CC-BY 4.0")
        self.assertIs(profile[0]["mature"], False)

    def test_it_is_idempotent(self):
        profile = [_asset("a", "images/a.png")]
        doc = {"added": [_asset("b", "images/b.png")],
               "fill": [{"id": "a", "field_values": {"k": "v"}}]}
        first = up.apply_manifest_reconcile(profile, doc)
        snapshot = json.loads(json.dumps(profile))
        second = up.apply_manifest_reconcile(profile, doc)
        self.assertEqual(first, (1, 1))
        self.assertEqual(second, (0, 0))
        self.assertEqual(profile, snapshot)

    def test_an_empty_value_counts_as_absent(self):
        profile = [_asset("a", "images/a.png", field_values={"blank": ""},
                          description="")]
        doc = {"fill": [{"id": "a", "field_values": {"blank": "filled"},
                         "description": "written"}]}
        _, filled = up.apply_manifest_reconcile(profile, doc)
        self.assertEqual(filled, 2)
        self.assertEqual(profile[0]["field_values"]["blank"], "filled")

    def test_a_fill_naming_an_unknown_id_is_skipped_not_invented(self):
        profile = [_asset("a", "images/a.png")]
        _, filled = up.apply_manifest_reconcile(
            profile, {"fill": [{"id": "ghost", "field_values": {"k": "v"}}]})
        self.assertEqual((len(profile), filled), (1, 0))


class TestPostDeduplication(unittest.TestCase):
    """#1275 — two posts under one id is a coin toss, not a duplicate.

    The assembler derives a roundup's id from (team, anchor asset), which
    is not unique across the several roundups it emits per team. `aa seed`
    keys on the stable id, so one of the two silently never exists.
    """

    def test_the_richest_row_survives(self):
        posts = [{"id": "x", "title": "8 drops", "asset_ids": [1, 2, 3]},
                 {"id": "x", "title": "10 drops", "asset_ids": [1, 2, 3, 4]},
                 {"id": "y", "title": "other", "asset_ids": []}]
        removed, ids = up.dedupe_posts(posts)
        self.assertEqual((removed, ids), (1, ["x"]))
        self.assertEqual([p["title"] for p in posts], ["10 drops", "other"])

    def test_ties_break_on_first_appearance(self):
        posts = [{"id": "x", "title": "first", "asset_ids": [1]},
                 {"id": "x", "title": "second", "asset_ids": [1]}]
        up.dedupe_posts(posts)
        self.assertEqual([p["title"] for p in posts], ["first"])

    def test_a_clean_list_is_untouched(self):
        posts = [{"id": "a", "asset_ids": []}, {"id": "b", "asset_ids": []}]
        self.assertEqual(up.dedupe_posts(posts), (0, []))
        self.assertEqual([p["id"] for p in posts], ["a", "b"])

    def test_the_committed_post_profiles_hold_no_duplicate_ids(self):
        """On the real data, because that is where they were. Twelve rows
        in studio-a and four in studio-b shared an id with a row that
        disagreed with them."""
        for name in ("studio-a", "studio-b"):
            posts = json.loads((PROFILES / f"{name}.posts.json").read_text())
            ids = [p["id"] for p in posts]
            dupes = {i for i in ids if ids.count(i) > 1} if len(ids) != len(set(ids)) else set()
            self.assertEqual(dupes, set(), f"{name}.posts.json")


class TestSiteAProfileIsNotBehindItsPublishedSite(unittest.TestCase):
    """#1275, on the committed data and WITHOUT the archive share.

    The share is not reachable from CI, so the reconcile document is what
    carries the published site's content into the repo. These assert the
    outcome the guard needs in order to ever let a publish through.
    """

    def test_the_reconcile_document_is_committed(self):
        doc = json.loads((UPGRADES / "manifest-reconcile.site_a.json").read_text())
        self.assertTrue(doc.get("_why"), "the document must say why it exists")
        self.assertEqual(len(doc["added"]), 1)
        self.assertEqual(doc["added"][0]["id"],
                         "0407bb0c-1d4d-58f9-a4a3-e9b0174956c7")
        self.assertGreater(len(doc["fill"]), 1900)

    def test_the_reconcile_document_never_replaces_a_committed_value(self):
        """The add-only property, checked against the profile it targets
        rather than against a fixture — a document that had drifted into
        overwriting real values would pass a synthetic test."""
        profile = {a["id"]: a
                   for a in json.loads((PROFILES / "studio-a.assets.json").read_text())}
        doc = json.loads((UPGRADES / "manifest-reconcile.site_a.json").read_text())
        for entry in doc["fill"]:
            target = profile.get(entry["id"])
            self.assertIsNotNone(target, entry["id"])
            for key, val in entry.items():
                if key == "id":
                    continue
                if isinstance(val, dict):
                    for k2, v2 in val.items():
                        have = (target.get(key) or {}).get(k2)
                        self.assertTrue(have == v2 or up._empty(have),
                                        f"{entry['id']}.{key}.{k2} would be replaced")
                else:
                    have = target.get(key)
                    self.assertTrue(have == val or up._empty(have),
                                    f"{entry['id']}.{key} would be replaced")

    def test_every_site_a_asset_carries_field_values(self):
        """100 records had none at all, and a publish would have taken the
        other 1,847's partial sets down with them."""
        assets = json.loads((PROFILES / "studio-a.assets.json").read_text())
        bare = [a["id"] for a in assets if not a.get("field_values")]
        self.assertEqual(bare, [], f"{len(bare)} asset(s) carry no field values")
        # 2,005 reconciled from the share (#1275) + 2 authored plates (#1290)
        self.assertEqual(len(assets), 2007)

    def test_the_extra_published_asset_is_in_the_profile(self):
        assets = {a["id"] for a in
                  json.loads((PROFILES / "studio-a.assets.json").read_text())}
        self.assertIn("0407bb0c-1d4d-58f9-a4a3-e9b0174956c7", assets)


class TestEveryAIStateIsInTheCorpus(unittest.TestCase):
    """#1290 — a corpus that cannot produce a state cannot show it.

    `generated` was the only declaration the dataset carried. `assisted`
    and `none` existed only as soft-deleted test fixtures, so neither had
    ever been rendered for a human being — and `none` is the state a wrong
    rendering damages most, because it must never become a "no AI" claim.
    """

    def setUp(self):
        self.assets = json.loads(
            (PROFILES / "studio-a.assets.json").read_text(encoding="utf-8"))
        self.posts = json.loads(
            (PROFILES / "studio-a.posts.json").read_text(encoding="utf-8"))
        self.by_id = {a["id"]: a for a in self.assets}

    def test_the_profile_declares_every_state_at_least_once(self):
        declared = {a.get("ai_provenance") for a in self.assets}
        for state in ("generated", "assisted", "none"):
            self.assertIn(state, declared, f"no seeded asset declares {state!r}")
        # Undeclared must stay the overwhelming majority: the corpus
        # models a real library, and a real library does not disclose.
        self.assertGreater(sum(1 for a in self.assets if not a.get("ai_provenance")),
                           1000)

    def test_a_post_mixes_generated_with_assisted(self):
        """⭐ Two DIFFERENT declared states in one post. A post that mixes
        `generated` with UNDECLARED already existed; it cannot exercise
        what a second declaration does, because there is only one label to
        derive from."""
        mixed = []
        for p in self.posts:
            states = {self.by_id[a].get("ai_provenance")
                      for a in (p.get("asset_ids") or ()) if a in self.by_id}
            if {"generated", "assisted"} <= states:
                mixed.append(p["id"])
        self.assertTrue(mixed, "no post mixes generated with assisted")

    def test_every_declaring_asset_is_our_own_work(self):
        """The #1260 rule, restated over all four states rather than over
        `generated` alone — `none` asserted on somebody else's photograph
        is a false disclosure about that person too."""
        for a in self.assets:
            if not a.get("ai_provenance"):
                continue
            src = (a.get("metadata") or {}).get("acquisition_source") or ""
            self.assertTrue(src.startswith(up.AI_DECLARABLE_SOURCE_PREFIXES),
                            f"{a['id']} ({a.get('title')!r}) declares "
                            f"{a['ai_provenance']!r} from {src!r}")

    def test_none_was_authored_for_a_new_asset_not_swept_over_old_ones(self):
        """⛔ ADR 0094. The failure this guards is a BULK one — a backfill
        that wrote `none` across the undeclared corpus would leave hundreds
        of rows claiming a disclosure nobody made."""
        none_assets = [a for a in self.assets if a.get("ai_provenance") == "none"]
        self.assertEqual(len(none_assets), 1)
        authored = {a["id"] for a in json.loads(
            (UPGRADES / "authored-assets.site_a.json").read_text(encoding="utf-8"))}
        self.assertIn(none_assets[0]["id"], authored,
                      "the `none` declaration must come from the authored doc, "
                      "not from a sweep across records that already existed")

    def test_the_declaring_plates_are_reachable_on_browse(self):
        """An asset in no post is invisible, so "the corpus has one" would
        be true and useless."""
        posted = {a for p in self.posts for a in (p.get("asset_ids") or ())}
        for a in self.assets:
            if a.get("ai_provenance") in ("assisted", "none"):
                self.assertIn(a["id"], posted, f"{a['title']!r} is in no post")

    def test_the_plate_recipe_is_committed_and_names_its_source(self):
        """The repo carries the recipe, not the bytes — same as
        `kenney_hq.py build`. A record pointing at bytes no one can
        reproduce is a dead end on any machine without the share."""
        self.assertTrue((SCRIPTS / "authored_plates.py").is_file())
        import authored_plates as ap
        self.assertEqual(len(ap.PLATES), 2)
        for a in json.loads((UPGRADES / "authored-assets.site_a.json").read_text()):
            self.assertIn(a["file_path"].rsplit("/", 1)[-1], ap.PLATES)
            self.assertEqual(a["source_root"], "local")

    def test_the_assisted_plate_really_does_carry_ai_content(self):
        """`assisted` is only true if a model actually contributed. The
        mood board samples one of the #1260 plates, so the claim is
        checkable rather than decorative."""
        import authored_plates as ap
        gen = {a["file_path"] for a in self.assets
               if a.get("ai_provenance") == "generated"}
        self.assertIn(f"images/aurora-generated/{ap.MOOD_BOARD_SOURCE}", gen)


class TestAuthoredPlatesAreDeterministic(unittest.TestCase):
    """A plate generator that drifted would churn `file_size_bytes` in the
    profile on every rebuild, and the guard would read that as an edit."""

    def test_the_colour_chart_is_byte_identical_across_runs(self):
        import authored_plates as ap
        with tempfile.TemporaryDirectory() as td:
            a, b = Path(td) / "a.png", Path(td) / "b.png"
            ap.build_colour_chart(a)
            ap.build_colour_chart(b)
            self.assertEqual(a.read_bytes(), b.read_bytes())

    def test_the_committed_record_matches_the_generator(self):
        """The record's byte count and hash describe bytes this repo can
        still produce. If they ever disagree, the profile is describing a
        file nobody can rebuild."""
        import authored_plates as ap
        with tempfile.TemporaryDirectory() as td:
            out = Path(td) / ap.PLATES[0]
            size = ap.build_colour_chart(out)
            digest = hashlib.sha256(out.read_bytes()).hexdigest()
        rec = next(a for a in json.loads(
            (UPGRADES / "authored-assets.site_a.json").read_text())
            if a["file_path"].endswith(ap.PLATES[0]))
        self.assertEqual(rec["file_size_bytes"], size)
        self.assertEqual(rec["metadata"]["sha256"], digest)
        self.assertEqual(rec["ai_provenance"], "none")

    def test_the_reader_handles_all_five_row_filters(self):
        """The reader is the half that runs on somebody else's bytes. The
        writer only ever emits filter 0, so a round-trip through it would
        exercise one branch out of five and prove nothing about the
        Paeth predictor."""
        import authored_plates as ap
        w, h = 4, 5
        pix = bytearray()
        for y in range(h):
            for x in range(w):
                pix += bytes(((x * 37 + y) % 256, (y * 91 + x) % 256, (x * y * 13) % 256))
        stride = w * 3

        def encode(filter_type: int) -> bytes:
            raw = bytearray()
            prev = bytearray(stride)
            for y in range(h):
                line = pix[y * stride:(y + 1) * stride]
                enc = bytearray()
                for i in range(stride):
                    a = line[i - 3] if i >= 3 else 0
                    b = prev[i]
                    c = prev[i - 3] if i >= 3 else 0
                    if filter_type == 0:
                        v = line[i]
                    elif filter_type == 1:
                        v = line[i] - a
                    elif filter_type == 2:
                        v = line[i] - b
                    elif filter_type == 3:
                        v = line[i] - ((a + b) >> 1)
                    else:
                        p = a + b - c
                        pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                        pred = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                        v = line[i] - pred
                    enc.append(v & 0xFF)
                raw.append(filter_type)
                raw += enc
                prev = line
            body = zlib.compress(bytes(raw), 6)

            def chunk(tag, data):
                return (struct.pack(">I", len(data)) + tag + data
                        + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))
            return (b"\x89PNG\r\n\x1a\n"
                    + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
                    + chunk(b"IDAT", body) + chunk(b"IEND", b""))

        with tempfile.TemporaryDirectory() as td:
            for ft in range(5):
                p = Path(td) / f"f{ft}.png"
                p.write_bytes(encode(ft))
                gw, gh, got = ap.png_read_rgb(p)
                self.assertEqual((gw, gh), (w, h), f"filter {ft}")
                self.assertEqual(bytes(got), bytes(pix), f"filter {ft} decoded wrong")

    def test_a_non_rgb_png_is_refused_rather_than_misread(self):
        import authored_plates as ap
        with tempfile.TemporaryDirectory() as td:
            p = Path(td) / "grey.png"

            def chunk(tag, data):
                return (struct.pack(">I", len(data)) + tag + data
                        + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))
            p.write_bytes(b"\x89PNG\r\n\x1a\n"
                          + chunk(b"IHDR", struct.pack(">IIBBBBB", 2, 2, 8, 0, 0, 0, 0))
                          + chunk(b"IDAT", zlib.compress(b"\x00\x01\x02\x00\x03\x04"))
                          + chunk(b"IEND", b""))
            with self.assertRaises(ValueError):
                ap.png_read_rgb(p)


if __name__ == "__main__":
    unittest.main(verbosity=2)
