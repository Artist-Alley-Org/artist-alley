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

import collections
import contextlib
import dataclasses
import hashlib
import io
import json
import os
import re
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import unittest
import unittest.mock
import zlib
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import apply_upgrade as up          # noqa: E402
import asset_collapse as ac         # noqa: E402
import authored_plates as ap        # noqa: E402
import audit_uncatalogued as au     # noqa: E402
import kenney_hq as hq              # noqa: E402
import kenney_pack_sources as kps   # noqa: E402
import pexels_gameplay as px        # noqa: E402
import manifest_guard as mg         # noqa: E402
import measure_staged as ms        # noqa: E402
import migrate_post_ids as mpi      # noqa: E402
import populate_archive as pa       # noqa: E402
import resolve_media_urls as rmu    # noqa: E402
import sanitize_and_assemble as sa  # noqa: E402
import studio_balance as sb         # noqa: E402
import preserved_archive as pres    # noqa: E402
import verify_site as vs            # noqa: E402

SCRIPTS = Path(__file__).resolve().parent
UPGRADES = SCRIPTS.parent / "upgrades"
PROFILES = SCRIPTS.parent / "profiles"

# The 13 site_b survivors the deterministic rule selects (#1319): the member
# earliest in profile (JSON) order, which is the row `applyAssets` reaches
# first (app/internal/seed/runner.go:762), qualified by the readable-bytes
# rule (:763-768). Pinned as LITERALS because the 22 losing records are no
# longer in the corrected profile, so the choice cannot be re-derived from it,
# and re-deriving it from the document would only prove the document agrees
# with itself.
SITE_B_SURVIVORS = (
    ("chen.wei", "027d8a6a-3684-eb98-a058-a9a7e6ffd463"),
    ("imani.williams", "543704bb-1c1f-2418-13df-625bb4d5aa3e"),
    ("imani.williams", "d5fbcd12-ddec-76aa-eed6-747c9721ef47"),
    ("imani.williams", "e943dc5d-4f93-f145-9d87-fb0f758cffb2"),
    ("layla.hassan", "249308ea-a6b9-5477-4e96-45967cb085f6"),
    ("layla.hassan", "997af954-cc46-ef5f-7e60-64b4f5af3e4c"),
    ("layla.hassan", "dbb70039-9d4f-83cc-98b4-97a61f3556c2"),
    ("maya.okonkwo", "034df3b9-671d-c292-32da-54f55d3d09ec"),
    ("maya.okonkwo", "89bc0b62-7b2c-ee5f-75bf-712c7e7f20dd"),
    ("maya.okonkwo", "e223b561-02f7-e6b4-ff4c-c1cd641dd1de"),
    ("maya.okonkwo", "ecae6d74-5143-85de-56dc-a121be8a8db4"),
    ("yuki.sato", "0b9bdf8c-1f74-fb4d-8b27-9306a48a4c10"),
    ("yuki.sato", "4de4817a-80d6-c999-dbf5-7a400bdc7c88"),
)


def _documented_retired_ids(profile_stem: str) -> frozenset[str]:
    """Ids a committed asset-collapse document retires for one profile.

    Empty when there is no document, which is the normal case: absence is
    no evidence, so nothing is exempt from anything.
    """
    # An alias profile is a byte copy of its source, so it inherits its
    # source's retirements. Resolving it here keeps the callers from
    # each growing their own copy of the mapping.
    for stem, alias in up.PROFILE_ALIASES:
        if profile_stem == alias:
            profile_stem = stem
            break
    path = UPGRADES / f"asset-collapse.{profile_stem}.json"
    if not path.is_file():
        return frozenset()
    return ac.load_collapse_document(
        path, profile_name=f"{profile_stem}.assets.json").retired_ids




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
        n, repaired, _ = up.merge_added(self.profile, added)
        self.assertEqual((n, repaired), (1, 0))
        rec = [e for e in self.profile if e["id"] == "vid-1"][0]
        self.assertEqual(rec["source_root"], up.SITE_SOURCE_ROOT)
        self.assertEqual(rec["source_path"], "videos/internet/x.mp4")
        self.assertEqual(up.merge_added(self.profile, added), (0, 0, 0),
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
        n, repaired, _ = up.merge_added(self.profile, added)
        self.assertEqual((n, repaired), (0, 1),
                         "an already-merged record did not pick up media_url")
        rec = [e for e in self.profile if e["id"] == "vid-1"][0]
        self.assertEqual(rec["metadata"]["media_url"],
                         "https://videos.pexels.com/video-files/1/1-hd.mp4")
        # and it must not keep re-reporting the same repair
        self.assertEqual(up.merge_added(self.profile, added), (0, 0, 0))

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

    # -- #1301: the manifest describes what it SHIPS -----------------

    PROFILE_FOR = {"site_a": ("studio-a", "demo"),
                   "site_b": ("studio-b", "dev")}

    # The four cut records that DO carry a direct URL, and so are the
    # four a re-fetch can actually reach. Named rather than derived: the
    # point of the assertion is that this set does not shrink, and
    # deriving it from the file under test would make it unable to fail.
    HAD_A_MEDIA_URL = frozenset({
        "videos/internet/red-eclipse-gameplay.webm",
        "videos/internet/sintel-2010-1080p.mkv",
        "videos/internet/tears-of-steel-720p.mov",
        "videos/internet/xonotic-gameplay.webm",
    })

    def _staged(self, site):
        doc = UPGRADES / f"staged-measurements.{site}.json"
        self.assertTrue(doc.is_file(), f"missing {doc} — re-emit it with "
                                       "measure_staged.py from a mounted share")
        return {e["id"]: e for e in json.loads(doc.read_text())}

    def test_no_record_describes_bytes_it_does_not_ship(self):
        """⛔ THIS ASSERTION FAILED ON THE DATA IT SHIPPED WITH.

        Before #1301, eleven video records per profile carried the
        ORIGIN's byte count — `videos/internet/sintel-2010-1080p.mkv`
        claimed 1,172,428,172 B for the 179,941,478 B cut the dataset
        actually publishes — and 2,690,105,638 B of overstatement passed
        every gate in this suite.

        The oracle is `staged-measurements.<site>.json`, which is what a
        mounted share measured. That indirection is deliberate: the
        runners cannot see /mnt/blackbox_archives, and a test that can
        only run where the share is mounted is a test CI cannot run.
        `measure_staged.py verify` is the direct form for a machine that
        does have it.
        """
        for site, profiles in self.PROFILE_FOR.items():
            staged = self._staged(site)
            for name in profiles:
                recs = json.loads(
                    (PROFILES / f"{name}.assets.json").read_text())
                for r in recs:
                    e = staged.get(r["id"])
                    if e is None:
                        continue
                    self.assertEqual(
                        r["file_size_bytes"], e["bytes"],
                        f"{name}: {r['file_path']} says "
                        f"{r['file_size_bytes']:,} B, {site} ships "
                        f"{e['bytes']:,} B")

    def test_a_prestaged_record_carries_the_hash_of_what_it_ships(self):
        """Without it the re-fetch has only a length to go on, and a
        length cannot tell two different cuts apart — site_a and site_b
        hold byte-identical, hash-different copies of three of these."""
        for site, profiles in self.PROFILE_FOR.items():
            staged = self._staged(site)
            for name in profiles:
                recs = json.loads(
                    (PROFILES / f"{name}.assets.json").read_text())
                for r in recs:
                    e = staged.get(r["id"])
                    if e is None or "sha256" not in e:
                        continue
                    self.assertEqual(
                        (r.get("metadata") or {}).get("sha256"), e["sha256"],
                        f"{name}: {r['file_path']} does not carry the hash "
                        f"of the bytes {site} ships")

    def test_the_origin_survives_every_correction(self):
        """⭐ ATTRIBUTION IS NOT OPTIONAL. Re-pointing `file_size_bytes`
        at the cut must not cost the record its provenance: the URL stays,
        and what that URL serves is preserved beside it so the two never
        have to be guessed apart again."""
        for site, profiles in self.PROFILE_FOR.items():
            staged = self._staged(site)
            cut = {i: e for i, e in staged.items() if "origin_bytes" in e}
            self.assertEqual(len(cut), 11,
                             f"{site}: expected the 11 known cut records")
            for name in profiles:
                recs = {r["id"]: r for r in json.loads(
                    (PROFILES / f"{name}.assets.json").read_text())}
                for rid, e in cut.items():
                    meta = recs[rid].get("metadata") or {}
                    self.assertEqual(meta.get("origin_bytes"),
                                     e["origin_bytes"],
                                     f"{name}: {e['file_path']} lost the "
                                     "origin's byte count")
                    # ⚠️ NOT "every record has a URL". Seven of the
                    # eleven never had one — they carry
                    # `acquisition_source` + `attribution` and nothing
                    # machine-readable, which is a pre-existing #602 gap
                    # and not this change's to invent. What must hold is
                    # that the correction COSTS a record nothing: the
                    # attribution stays, and a URL that was there is
                    # still there.
                    self.assertTrue(
                        meta.get("attribution"),
                        f"{name}: {e['file_path']} lost its attribution")
                    if e["file_path"] in self.HAD_A_MEDIA_URL:
                        self.assertTrue(
                            meta.get("media_url"),
                            f"{name}: {e['file_path']} lost the origin URL "
                            "it shipped with")

    def test_an_internet_records_hash_is_never_re_measured(self):
        """⛔ `metadata.sha256` ON AN `internet` RECORD IS IDENTITY.

        `sanitize_and_assemble.py` mints the id from it —
        `stable_uuid("asset", "internet", sha256)` — and spreads three
        timestamps off the same value. Re-pointing it at a staged cut
        would move asset ids on the next assembly, which is why
        `measure_staged.py` records a hash for the PRE-STAGED roots only.
        This test is the tripwire on that rule.
        """
        for site in self.SITES:
            staged = self._staged(site)
            for name in self.PROFILE_FOR[site]:
                recs = {r["id"]: r for r in json.loads(
                    (PROFILES / f"{name}.assets.json").read_text())}
                for rid, e in staged.items():
                    if recs[rid].get("source_root") != "internet":
                        continue
                    self.assertNotIn(
                        "sha256", e,
                        f"{name}: {e['file_path']} is an internet record and "
                        "the staged document carries a hash for it — that "
                        "would move its id on re-assembly")

    # -- #1302: the hash moves with the file -------------------------

    def test_a_repointed_record_carries_the_hash_of_its_pool_file(self):
        """⛔ FAILED ON THE DATA IT SHIPPED WITH.

        `apply_replacements` swapped every other field that describes the
        bytes — path, size, extension, title, licence, filename — and left
        `metadata.sha256` describing the file the record used to be. Two
        records per site therefore published the hash of a screenshot they
        no longer contain.

        The oracle is `newSha256`, measured off a built pool by
        `kenney_hq.py sizes --profile`. It exists on exactly the rows
        whose record carries a hash, so the document did not have to grow
        916 values to serve four.
        """
        for site, profiles in self.PROFILE_FOR.items():
            reps = {r["id"]: r for r in json.loads(
                (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text())}
            hashed = {i: r for i, r in reps.items() if "newSha256" in r}
            self.assertEqual(len(hashed), 2,
                             f"{site}: expected the two known repointed "
                             "records that carry a hash")
            for name in profiles:
                recs = {r["id"]: r for r in json.loads(
                    (PROFILES / f"{name}.assets.json").read_text())}
                for rid, row in hashed.items():
                    rec = recs[rid]
                    self.assertEqual(rec["file_path"], row["new"])
                    self.assertEqual(
                        (rec.get("metadata") or {}).get("sha256"),
                        row["newSha256"],
                        f"{name}: {row['new']} carries the hash of the file "
                        "it used to be")

    def test_the_hash_is_corrected_and_never_dropped(self):
        """Dropping the key would be honest about no longer knowing the
        value, and would also refuse the next publish: the destination
        HAS it, so its absence is MISSING_KEY, which manifest_guard
        classifies as a LOSS."""
        for site, profiles in self.PROFILE_FOR.items():
            reps = [r for r in json.loads(
                (UPGRADES / f"kenney-hq-replacements.{site}.json").read_text())
                if "newSha256" in r]
            for name in profiles:
                recs = {r["id"]: r for r in json.loads(
                    (PROFILES / f"{name}.assets.json").read_text())}
                for row in reps:
                    self.assertIn(
                        "sha256", recs[row["id"]].get("metadata") or {},
                        f"{name}: {row['new']} lost metadata.sha256 — "
                        "publishing would be refused as a loss")

    # -- #1303: the values no pass can reach -------------------------

    def test_the_balance_docs_hq_sizes_match_the_profile(self):
        """⛔ 517 BYTE COUNTS THAT NO PASS COULD REACH.

        `merge_added` appends only records ABSENT from the profile, and
        all 517 of `balance-assets.site_a.json`'s hq ids are already in
        it — the pass touches an existing record solely to add
        `metadata.media_url`. So the document's byte counts could drift
        away from the profile's without one line of output.

        The direct gate is `kenney_hq.py sizes --balance`, which measures
        them against a built pool. That needs the pack, which CI does not
        have. This is the half that runs anywhere: the two in-repo
        statements of the same measurement must agree, so a drift in
        either one fails here even when no pool is reachable.

        ⚠️ NOT the same check as `TestPoolSizesAgreeAcrossDocuments`.
        That one compares a balance row against a REPLACEMENTS row where
        both name one pool file, and 517 of these rows are named by no
        replacement at all. This compares the balance row against the
        PROFILE RECORD it belongs to, which is the pairing #1303 is
        about and the one nothing covered.
        """
        checked = 0
        skipped = 0
        for site, profiles in self.PROFILE_FOR.items():
            doc = UPGRADES / f"balance-assets.{site}.json"
            if not doc.is_file():
                continue
            rows = [r for r in json.loads(doc.read_text())
                    if r.get("source_root") == "hq"]
            self.assertGreater(len(rows), 0, doc)
            for name in profiles:
                recs = {r["id"]: r for r in json.loads(
                    (PROFILES / f"{name}.assets.json").read_text())}
                retired = _documented_retired_ids(name)
                for row in rows:
                    # ⛔ RETIREMENT-AWARE, NOT RETIREMENT-BLIND (#1319).
                    # The balance document is NOT rewritten when a record
                    # is retired: it is the record of what the pass
                    # emitted, and editing it to make this test pass
                    # would turn evidence into bookkeeping. So the row is
                    # skipped, and ONLY when a collapse document names
                    # it, which is what stops the exemption growing.
                    if row["id"] in retired:
                        skipped += 1
                        continue
                    rec = recs.get(row["id"])
                    self.assertIsNotNone(
                        rec, f"{name}: balance row {row['id']} is in no "
                             f"profile and no asset-collapse document retires "
                             f"it, so nothing can ever check it")
                    self.assertEqual(
                        rec["file_size_bytes"], row["file_size_bytes"],
                        f"{name}: {row['file_path']} — the balance doc says "
                        f"{row['file_size_bytes']:,} B and the profile says "
                        f"{rec['file_size_bytes']:,} B")
                    checked += 1
        self.assertGreater(checked, 0, "this test checked nothing")
        # The skipped rows are named by a document, so the exemption is
        # bounded and visible rather than a hole the next edit widens.
        #
        # ⛔ THE EXPECTATION COUNTS (profile, BALANCE ROW) PAIRS, NOT
        # RETIREMENTS. A retirement can only be skipped by the loop above if
        # the balance document actually HOLDS that row, and site_b has no
        # balance document at all (#572 is site_a only). Summing every
        # profile's retirement count instead made the expectation a claim
        # about documents this test never opens: it read 1 + 1 + 0 + 0 == 2
        # while site_b retired nothing, and the first site_b retirement
        # turned it into 46 against an unchanged 2. The loop's own key is
        # the honest one, so the two cannot drift again.
        expected_skips = 0
        for site, profiles in self.PROFILE_FOR.items():
            doc = UPGRADES / f"balance-assets.{site}.json"
            if not doc.is_file():
                continue
            row_ids = {r["id"] for r in json.loads(doc.read_text())
                       if r.get("source_root") == "hq"}
            for name in profiles:
                expected_skips += len(row_ids & _documented_retired_ids(name))
        self.assertEqual(skipped, expected_skips,
                         "a row was skipped that no collapse document retires")

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

    def _run(self, tmp, media_url, size, extra_args=(), meta=None,
             prestage=None):
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
                **(meta or {}),
            },
        }
        profile.write_text(json.dumps([rec]), encoding="utf-8")
        if prestage is not None:
            staged = dest / "videos/internet/clip.mp4"
            staged.parent.mkdir(parents=True, exist_ok=True)
            staged.write_bytes(prestage)
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

    # -- #1301 -------------------------------------------------------
    #
    # ⛔ EVERY TEST BELOW ASSERTS A REFUSAL. The hazard was never that
    # the re-fetch failed; it was that it SUCCEEDED, printed REFETCHED,
    # and staged a 1.1 GB original over a two-minute cut because the
    # length it checked was the origin's own. A validator that has only
    # ever been observed permitting is untested, so these drive the
    # decline.

    def test_the_origin_is_refused_when_the_dataset_ships_a_cut(self):
        """THE BUG (#1301). The URL serves the ORIGINAL; the dataset
        publishes a cut of it. The origin must not be staged."""
        origin = b"ORIGINAL-" * 4096          # what media_url serves
        ships = len(b"CUT-" * 64)             # what the manifest publishes
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            (served / "clip.mp4").write_bytes(origin)
            httpd, base = self._serve(served)
            try:
                proc, out = self._run(
                    d, f"{base}/clip.mp4", ships,
                    meta={"origin_bytes": len(origin)})
            finally:
                httpd.shutdown()
                httpd.server_close()
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertFalse(out.exists(),
                             "the ORIGIN was staged over a record that "
                             "publishes a cut of it")
            self.assertNotIn("REFETCHED", proc.stderr)
            self.assertIn("size mismatch", proc.stderr)
            # The message has to name both numbers, because the reader's
            # first question is "which of these is my dataset?".
            self.assertIn(f"ships {ships:,} B", proc.stderr)
            self.assertIn(f"served {len(origin):,} B", proc.stderr)

    def test_the_origin_is_refused_even_when_it_measures_the_same(self):
        """A LENGTH IS A WEAK ORACLE. Three of our video records have
        byte-identical staged copies in site_a and site_b with different
        hashes, so "same size" genuinely does not mean "same bytes".
        The recorded hash is what closes that gap."""
        ships = b"THE-CUT-" * 512
        impostor = b"NOT-CUT-" * 512          # same length, other bytes
        self.assertEqual(len(ships), len(impostor))
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            (served / "clip.mp4").write_bytes(impostor)
            httpd, base = self._serve(served)
            try:
                proc, out = self._run(
                    d, f"{base}/clip.mp4", len(ships),
                    meta={"sha256": hashlib.sha256(ships).hexdigest()})
            finally:
                httpd.shutdown()
                httpd.server_close()
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertFalse(out.exists(),
                             "a same-length impostor was staged")
            self.assertIn("sha256 mismatch", proc.stderr)

    def test_the_real_cut_is_still_staged(self):
        """The refusal must not be a blanket one: when the URL serves
        exactly what the manifest publishes, the re-fetch still works.
        A gate that refuses everything is as useless as one that refuses
        nothing, and only this test tells the two apart."""
        ships = b"THE-CUT-" * 512
        with tempfile.TemporaryDirectory() as d:
            served = Path(d) / "served"
            served.mkdir()
            (served / "clip.mp4").write_bytes(ships)
            httpd, base = self._serve(served)
            try:
                proc, out = self._run(
                    d, f"{base}/clip.mp4", len(ships),
                    meta={"sha256": hashlib.sha256(ships).hexdigest()})
            finally:
                httpd.shutdown()
                httpd.server_close()
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertEqual(out.read_bytes(), ships)
            self.assertIn("REFETCHED", proc.stderr)

    def test_a_prestaged_file_of_the_wrong_size_fails_the_run(self):
        """`st_size > 0` was the entire pre-staged check, which is how a
        two-minute cut sat under a 1.1 GB claim for a whole release
        without one line of output about it."""
        with tempfile.TemporaryDirectory() as d:
            proc, out = self._run(d, None, 999999, prestage=b"much shorter")
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertIn("WRONG SIZE", proc.stderr)
            self.assertIn("wrong size:  1", proc.stderr)
            # ⚠️ And it must NOT delete or re-fetch over it. The bytes on
            # the share are the only copy; a guard that "repairs" them is
            # the data loss it was meant to prevent.
            self.assertEqual(out.read_bytes(), b"much shorter")

    def test_a_correct_prestaged_file_is_still_silent(self):
        with tempfile.TemporaryDirectory() as d:
            body = b"exactly this"
            proc, out = self._run(d, None, len(body), prestage=body)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertNotIn("WRONG SIZE", proc.stderr)
            self.assertIn("preexisting: 1", proc.stderr)

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

class TestSeededTitlesReadAsWritten(unittest.TestCase):
    """#1306 — the shipped titles, and the functions that produce them.

    Measured on the file this replaces: 863 studio-a posts, 781 of them
    (90%) containing an em dash, 580 (67%) ending in a count the
    generator produced, 35 containing the literal "team(s)".
    """

    POSTS = ("studio-a", "studio-b", "dataset")

    def _titles(self, name):
        return [p["title"] for p in json.loads(
            (PROFILES / f"{name}.posts.json").read_text())]

    def test_no_shipped_title_contains_an_em_dash(self):
        for name in self.POSTS:
            bad = [t for t in self._titles(name) if "\u2014" in t]
            self.assertEqual(bad, [], f"{name}.posts.json: {len(bad)} title(s)")

    def test_no_shipped_asset_title_contains_an_em_dash(self):
        """⛔ THE TEMPLATES WERE NOT THE ONLY SOURCE. 43 ASSET titles
        carried one, and the solo, revision and video templates embed an
        asset title verbatim — so fixing the templates alone still left
        37 site_a post titles with a dash arriving from the other end."""
        for name in ("studio-a", "studio-b", "demo", "dev"):
            bad = [a["title"] for a in json.loads(
                (PROFILES / f"{name}.assets.json").read_text())
                if "\u2014" in (a.get("title") or "")]
            self.assertEqual(bad, [], f"{name}.assets.json: {len(bad)}")

    def test_no_shipped_title_says_team_s(self):
        for name in self.POSTS:
            bad = [t for t in self._titles(name) if "team(s)" in t]
            self.assertEqual(bad, [], f"{name}: {bad[:3]}")

    def test_no_shipped_title_ends_in_a_generated_count(self):
        """The count is already on the card — `PostCard.svelte` renders
        `CardKindBadge count={memberCount}` beside the title — so the
        suffix repeated chrome an inch away."""
        pat = re.compile(r"\d+[- ](?:part set|asset bundle|assets|drops|cuts)$")
        for name in self.POSTS:
            bad = [t for t in self._titles(name) if pat.search(t)]
            self.assertEqual(bad, [], f"{name}: {bad[:3]}")

    def test_the_titles_are_all_distinct(self):
        """⛔ THE COUNT WAS LOAD-BEARING, which is why it is replaced and
        not deleted. Dropping it outright collapses 128 studio-a titles
        into 44 groups. The shipped file was worse than that even WITH
        the counts: 313 studio-a titles were duplicates of another."""
        for name in self.POSTS:
            titles = self._titles(name)
            dupes = [t for t, n in collections.Counter(titles).items() if n > 1]
            self.assertEqual(dupes, [], f"{name}: {len(dupes)} repeated")

    def test_titles_did_not_run_away_in_length(self):
        """The shipped studio-a median was 39 and its p90 51. A title
        that fixes the voice by becoming a sentence has not fixed it."""
        for name in self.POSTS:
            lengths = sorted(len(t) for t in self._titles(name))
            median = lengths[len(lengths) // 2]
            p90 = lengths[int(len(lengths) * 0.9)]
            self.assertLessEqual(median, 45, f"{name}: median {median}")
            self.assertLessEqual(p90, 70, f"{name}: p90 {p90}")

    # -- the functions, not just the data ----------------------------

    def test_the_dash_clean_takes_the_spaces_with_it(self):
        """⚠️ Replacing the character and leaving the spaces gives
        "Big Buck Bunny , 720p surround", which is a worse tell than the
        dash was. This caught exactly that. Since #1319 the dash becomes
        an ASCII hyphen, never a comma."""
        self.assertEqual(sa.clean_dashes("Big Buck Bunny \u2014 720p surround"),
                         "Big Buck Bunny - 720p surround")
        self.assertEqual(sa.clean_dashes("Ana (Overwatch \u2014 Support)"),
                         "Ana (Overwatch - Support)")
        self.assertEqual(sa.clean_dashes("a \u2014 b \u2014 c"), "a - b - c")
        self.assertNotIn("  ", sa.clean_dashes("a \u2014 b \u2014 c"))

    def test_disambiguation_numbers_every_member_of_a_family(self):
        posts = [{"id": "c", "title": "Same"}, {"id": "a", "title": "Same"},
                 {"id": "b", "title": "Same"}, {"id": "d", "title": "Alone"}]
        sa.disambiguate_titles(posts)
        got = {p["id"]: p["title"] for p in posts}
        self.assertEqual(got["a"], "Same - part 1")
        self.assertEqual(got["b"], "Same - part 2")
        self.assertEqual(got["c"], "Same - part 3")
        self.assertEqual(got["d"], "Alone",
                         "a title nothing collides with must be left alone")

    def test_disambiguation_orders_by_id_not_by_position(self):
        """Iteration order is not stable across a re-assembly; the post
        id is derived from membership and is."""
        a = [{"id": "z", "title": "T"}, {"id": "y", "title": "T"}]
        b = [{"id": "y", "title": "T"}, {"id": "z", "title": "T"}]
        sa.disambiguate_titles(a)
        sa.disambiguate_titles(b)
        self.assertEqual({p["id"]: p["title"] for p in a},
                         {p["id"]: p["title"] for p in b})

    def test_a_solo_title_splits_on_the_LAST_dash(self):
        """The flavour is a suffix, so an asset title carrying a dash of
        its own must not be cut in half — that produced
        "720p surround, cinematic test: Big Buck Bunny"."""
        posts = [{"id": "1", "post_kind": "solo_showcase",
                  "title": "Big Buck Bunny \u2014 720p surround "
                           "\u2014 cinematic test",
                  "asset_ids": []}]
        sa.retitle_posts(posts)
        self.assertEqual(posts[0]["title"],
                         "Cinematic test: Big Buck Bunny - 720p surround")

    def test_a_video_title_splits_on_the_FIRST_dash(self):
        """Its fixed wording is a PREFIX, so the opposite rule applies."""
        posts = [{"id": "1", "post_kind": "video_dailies",
                  "title": "Dailies \u2014 Sintel \u2014 480p trailer",
                  "asset_ids": []}]
        sa.retitle_posts(posts)
        self.assertEqual(posts[0]["title"], "Dailies on Sintel - 480p trailer")


class TestTitleParsersAreTheFormattersInverse(unittest.TestCase):
    """⛔ TWO POST KINDS DERIVE THEIR ID THROUGH THE TITLE (#1306/#1293).

    `migrate_post_ids.derived_id` recovers a sprint `label` and a
    `reel_label` from the title — neither is stored anywhere else on the
    post — and feeds them into `sprint_post_id` / `showreel_post_id`. So
    the title format is an identity concern for those two kinds, and
    retiring the em dash broke the parse the moment it landed.

    The ids did not move: the recovered label is the same label. But a
    parse that HALF matched would have moved them silently, which is why
    the inverse now lives beside the formatter and why this pins them
    together.
    """

    def test_sprint_label_round_trips(self):
        for project, label in (("Project Mirror", "milestone alpha"),
                               ("Project Echo", "sprint 14"),
                               ("Art Research", "ship gate")):
            title = sa.title_project_sprint(project, label)
            self.assertEqual(sa.sprint_label_from_title(project, title), label)

    def test_sprint_label_round_trips_through_disambiguation(self):
        """A colliding title gains ` - part N` (#1319), and the parse has
        to see through it. This is exactly what went red under #1306."""
        title = sa.title_project_sprint("Project Mirror", "milestone alpha")
        posts = [{"id": "a", "title": title}, {"id": "b", "title": title}]
        sa.disambiguate_titles(posts)
        for post in posts:
            self.assertIn(" - part ", post["title"])
            self.assertEqual(
                sa.sprint_label_from_title("Project Mirror", post["title"]),
                "milestone alpha")

    def test_reel_label_round_trips(self):
        for label in sa.REEL_LABELS:
            title = sa.title_showreel(label)
            self.assertEqual(sa.reel_label_from_title(title), label)
            posts = [{"id": "a", "title": title}, {"id": "b", "title": title}]
            sa.disambiguate_titles(posts)
            for post in posts:
                self.assertEqual(sa.reel_label_from_title(post["title"]), label)

    def test_a_title_that_does_not_match_returns_None_not_a_guess(self):
        self.assertIsNone(
            sa.sprint_label_from_title("Project Mirror", "Something else"))
        self.assertIsNone(sa.reel_label_from_title("Not a showreel"))
        self.assertIsNone(sa.sprint_label_from_title("", "anything"))

    def test_stripping_the_part_suffix_is_idempotent(self):
        self.assertEqual(sa.strip_part_suffix("A title - part 12"), "A title")
        self.assertEqual(
            sa.strip_part_suffix(sa.strip_part_suffix("A title - part 12")),
            "A title")
        self.assertEqual(sa.strip_part_suffix("A title"), "A title")
        # ⚠️ Not a blanket "drop everything after the last hyphen".
        self.assertEqual(sa.strip_part_suffix("Moby Dick - Herman Melville"),
                         "Moby Dick - Herman Melville")
        # ⛔ And no fallback for the retired comma form: stale data has
        # to fail the label parse, not be quietly accepted.
        self.assertEqual(sa.strip_part_suffix("A title, part 12"),
                         "A title, part 12")


# ---------------------------------------------------------------------------
# #1319: titles lose their comma and em-dash separators (repository slice)
# ---------------------------------------------------------------------------

EM = "\u2014"
_LEGACY_PART = re.compile(r", part \d+$")


def _profile(name):
    return json.loads((PROFILES / f"{name}.json").read_text(encoding="utf-8"))


class TestTitlePunctuationRule(unittest.TestCase):
    """The rule itself (#1319): a comma or em dash touching whitespace on
    either side becomes " - ", one touching none becomes "-"."""

    def test_the_four_em_dash_spacings_are_each_pinned(self):
        self.assertEqual(sa.normalize_title(f"a {EM} b"), "a - b")
        self.assertEqual(sa.normalize_title(f"a {EM}b"), "a - b")
        self.assertEqual(sa.normalize_title(f"a{EM} b"), "a - b")
        self.assertEqual(sa.normalize_title(f"a{EM}b"), "a-b")

    def test_the_comma_spacings_are_each_pinned(self):
        self.assertEqual(sa.normalize_title("a, b"), "a - b")
        self.assertEqual(sa.normalize_title("a , b"), "a - b")
        self.assertEqual(sa.normalize_title("a ,b"), "a - b")
        self.assertEqual(sa.normalize_title("Sono Variablefont Mono,wght (font)"),
                         "Sono Variablefont Mono-wght (font)")

    def test_the_two_comma_sintel_title(self):
        self.assertEqual(
            sa.normalize_title("Sintel , full film (512kb stereo, ~13 min)"),
            "Sintel - full film (512kb stereo - ~13 min)")
        self.assertEqual(
            sa.normalize_title(f"Sintel {EM} full film (512kb stereo, ~13 min)"),
            "Sintel - full film (512kb stereo - ~13 min)")

    def test_each_unspaced_repeat_is_its_own_separator(self):
        """The rule applies to EACH comma and EACH em dash: two unspaced
        separators are two hyphens, never collapsed into one."""
        self.assertEqual(sa.normalize_title("a,,b"), "a--b")
        self.assertEqual(sa.normalize_title(f"a{EM}{EM}b"), "a--b")

    def test_it_is_idempotent_and_never_doubles_a_space(self):
        samples = ["Sintel , full film (512kb stereo, ~13 min)",
                   f"a {EM} b {EM} c", "a  ,  b", f"x{EM}y, z", "plain",
                   "Moby Dick - Herman Melville", ""]
        for t in samples:
            once = sa.normalize_title(t)
            self.assertEqual(sa.normalize_title(once), once, t)
            self.assertFalse(sa.has_title_separator(once), once)
            if "  " not in t:
                self.assertNotIn("  ", once, t)

    def test_the_post_path_is_the_same_rule_restricted_to_the_em_dash(self):
        """⛔ `clean_dashes` runs over every post title, and a comma an
        author wrote there is legal, so it must never rewrite one."""
        self.assertEqual(sa.clean_dashes("Notes, sketches and studies"),
                         "Notes, sketches and studies")
        self.assertEqual(sa.clean_dashes(f"Notes, sketches {EM} studies"),
                         "Notes, sketches - studies")
        for t in (f"a {EM} b", f"a{EM}b", f"a {EM}b"):
            self.assertEqual(sa.clean_dashes(t), sa.normalize_title(t))


class TestCommittedTitlePunctuation(unittest.TestCase):
    """The committed profiles after the #1319 correction."""

    ASSETS = ("studio-a", "studio-b", "demo", "dev")
    POSTS = ("studio-a", "studio-b", "dataset")

    def test_no_asset_title_holds_a_comma_or_an_em_dash(self):
        bad = {}
        for name in self.ASSETS:
            n = sum(1 for a in _profile(f"{name}.assets")
                    if "," in (a.get("title") or "")
                    or EM in (a.get("title") or ""))
            if n:
                bad[name] = n
        self.assertEqual(bad, {}, "asset titles holding a comma or em dash")

    def test_no_post_title_ends_in_the_retired_comma_part_suffix(self):
        bad = {}
        for name in self.POSTS:
            n = sum(1 for p in _profile(f"{name}.posts")
                    if _LEGACY_PART.search(p["title"]))
            if n:
                bad[name] = n
        self.assertEqual(bad, {}, "titles still ending in ', part N'")

    def test_every_part_family_is_numbered_one_to_k(self):
        """Every family reads " - part N" and counts 1..k with no gap and
        no repeat. Not in id order: `disambiguate_titles` numbered by the
        id each post held THEN, and later id migrations (#1310) moved
        ids without renumbering, which is right, because a title must not
        move with an id. The family counts are the denominator; a corpus
        with no hyphen family at all must not pass by being empty."""
        suffix = re.compile(r"^(?P<base>.*) - part (?P<n>\d+)$")
        expected = {"studio-a": 45, "studio-b": 47, "dataset": 63}
        got = {}
        for name in self.POSTS:
            posts = _profile(f"{name}.posts")
            families = {}
            for p in posts:
                m = suffix.match(p["title"])
                if m:
                    families.setdefault(m["base"], []).append(
                        (p["id"], int(m["n"])))
            bare = {p["title"] for p in posts if not suffix.match(p["title"])}
            for base, members in families.items():
                ns = sorted(n for _, n in members)
                self.assertEqual(ns, list(range(1, len(ns) + 1)),
                                 f"{name}: {base!r}")
                self.assertGreaterEqual(len(ns), 2, f"{name}: {base!r}")
                self.assertNotIn(base, bare, f"{name}: {base!r}")
            got[name] = len(families)
        self.assertEqual(got, expected)

    def _em_dash_sources(self):
        out = {}
        for stem in ("generated", "mature", "authored"):
            for p in json.loads((UPGRADES / f"{stem}-posts.site_a.json")
                                .read_text(encoding="utf-8")):
                if f" {EM} " in p["title"]:
                    out[p["id"]] = p["title"]
        return out

    def test_a_mechanical_separator_reads_as_the_rules_hyphen(self):
        """R3a. A post whose committed source document titles it with an
        em dash between two halves reads that title with the dash turned
        into the rule's hyphen. The balance document's retired count
        suffix is not one of these sources: those posts use the chunk
        form (see TestBalanceTemplateMatchesCommitted)."""
        sources = self._em_dash_sources()
        bad, seen = {}, 0
        for name in self.POSTS:
            n = 0
            for p in _profile(f"{name}.posts"):
                src = sources.get(p["id"])
                if src is None:
                    continue
                seen += 1
                want = src.replace(f" {EM} ", " - ")
                if sa.strip_part_suffix(p["title"]) != want:
                    n += 1
            if n:
                bad[name] = n
        self.assertEqual(seen, 15, "the denominator moved")
        self.assertEqual(bad, {})

    def test_every_embedded_member_title_is_the_corrected_one(self):
        """R3b. Failing condition: rule(post) contains rule(member) while
        the post title itself does not. So an embedded member title must
        appear in its corrected form, and a comma the post's author wrote
        anywhere else is never the subject."""
        a = _profile("studio-a.assets")
        b = _profile("studio-b.assets")
        by_profile = {"studio-a": [a], "studio-b": [b], "dataset": [a, b]}
        bad, embedded = {}, 0
        for name in self.POSTS:
            titles = {}
            for records in by_profile[name]:
                for r in records:
                    titles.setdefault(r["id"], set()).add(r["title"])
            n = 0
            for p in _profile(f"{name}.posts"):
                norm_post = sa.normalize_title(p["title"])
                for aid in p["asset_ids"]:
                    for t in titles.get(aid, ()):
                        fixed = sa.normalize_title(t)
                        if not t or fixed not in norm_post:
                            continue
                        if fixed in p["title"]:
                            embedded += 1
                        else:
                            n += 1
            if n:
                bad[name] = n
        self.assertGreater(embedded, 0, "no embedding found at all")
        self.assertEqual(bad, {})

    PRE_HQ_SOURCE_TITLES = {
        "f72d6f4d-68f2-47b4-aa45-c98e927d81df":
            "Card sketch (digital - stylus) and variants",
        "db63a1d1-5835-3e00-c068-d00f109a9bd0":
            "Color study: Switch Disabled sketch (digital - stylus)",
        "5345de23-e2ff-b39e-f709-01b9fc75113a":
            "Data Table sketch (digital - stylus) and variants - part 1",
        "bf158ba2-c849-a10f-977d-823d3adb89f6":
            "Data Table sketch (digital - stylus) and variants - part 2",
        "f94121ab-9f60-8645-9644-0771d0567448":
            "First draft of Radio Button Checked sketch (digital - stylus)",
        "a6c9b0ae-555b-ac0b-4739-dea0623460e2":
            "Lighting pass: Switch Disabled sketch (paper - pencil)",
        "c7b99888-e9cc-44f0-7273-067d8b848f1d":
            "Lighting pass: Switch Enabled sketch (digital - stylus)",
        "dade7d7d-5730-6153-3bc6-8ae74c39722a":
            "Review pass on Radio Button Checked sketch (digital - stylus)",
        "f8dc5708-07a4-3c93-43b7-78aba8291531":
            "Signing off on Radio Button Checked sketch (digital - stylus)",
        "df829d74-b213-ccad-08f5-07c13ada36b5":
            "Slider sketch (digital - stylus) and variants",
    }

    def test_titles_embedding_a_pre_hq_source_title_are_pinned(self):
        """R3c. These ten embed a member's title from BEFORE the HQ rename,
        and after the correction nothing in the repo records where their
        comma came from, so the corrected values are pinned. A pin of
        values, not an allowlist."""
        for name in ("studio-b", "dataset"):
            got = {p["id"]: p["title"] for p in _profile(f"{name}.posts")
                   if p["id"] in self.PRE_HQ_SOURCE_TITLES}
            self.assertEqual(got, self.PRE_HQ_SOURCE_TITLES, name)

    def test_pinned_exact_results(self):
        """R4."""
        assets = {
            ("studio-a", "a74f9231-347d-ee8a-e80b-5c966c5265f6"):
                "Sono Variablefont Mono-wght (font)",
            ("studio-a", "083f9159-1903-73c9-f0c1-52eef9884c24"):
                "Sintel - full film (512kb stereo - ~13 min)",
            ("studio-b", "8175e587-77c7-8db9-1f5e-c138f15ea19d"):
                "Embark Collections Manager User's Guide - Part 1 (document)",
            ("studio-b", "0dfff156-bb0a-6958-69ae-c2c861954391"):
                "Card sketch (paper - pencil)",
            ("studio-b", "f61ccbb9-bf6f-4e1f-a786-60b4829d47a1"):
                "Ana (Overwatch - Support)",
        }
        posts = {
            ("studio-a", "35985ec0-5421-9970-a429-a82f8051e8cb"):
                "Dailies on Sintel - full film (512kb stereo - ~13 min)",
            ("studio-a", "d52da9c9-2574-5eac-a92d-28a386bcf16d"):
                "Animation - AI reference set",
            ("studio-b", "5345de23-e2ff-b39e-f709-01b9fc75113a"):
                "Data Table sketch (digital - stylus) and variants - part 1",
            ("studio-b", "df829d74-b213-ccad-08f5-07c13ada36b5"):
                "Slider sketch (digital - stylus) and variants",
            ("studio-b", "f4ee67b0-a134-00b4-d7d9-3b21c9494f6c"):
                "MTG Archive lock-in pass - part 2",
            ("dataset", "f4ee67b0-a134-00b4-d7d9-3b21c9494f6c"):
                "MTG Archive lock-in pass - part 2",
            ("dataset", "8bc25946-966e-2bef-c329-420281485242"):
                "Cinematics Q4 reel - part 2",
        }
        for kind, pins in (("assets", assets), ("posts", posts)):
            cache = {}
            for (name, rid), want in pins.items():
                if name not in cache:
                    cache[name] = {r["id"]: r["title"]
                                   for r in _profile(f"{name}.{kind}")}
                self.assertEqual(cache[name].get(rid), want, f"{name} {rid}")


class TestPartSuffixPair(unittest.TestCase):
    """R6. The formatter and its parse, including N >= 2 families."""

    def test_no_suffix_recovers_the_label(self):
        for label in sa.SPRINT_LABELS:
            title = sa.title_project_sprint("MTG Archive", label)
            self.assertEqual(sa.sprint_label_from_title("MTG Archive", title),
                             label)

    def test_families_of_two_and_three_number_every_member(self):
        for size in (2, 3):
            for label in ("lock-in pass", "milestone alpha"):
                title = sa.title_project_sprint("MTG Archive", label)
                posts = [{"id": f"id-{k}", "title": title}
                         for k in range(size)]
                sa.disambiguate_titles(posts)
                got = sorted(p["title"] for p in posts)
                self.assertEqual(
                    got, [f"{title} - part {k}" for k in range(1, size + 1)])
                for p in posts:
                    self.assertEqual(
                        sa.sprint_label_from_title("MTG Archive", p["title"]),
                        label)
            reel = sa.title_showreel("Q4 reel")
            posts = [{"id": f"id-{k}", "title": reel} for k in range(size)]
            sa.disambiguate_titles(posts)
            for p in posts:
                self.assertRegex(p["title"], r" - part \d+$")
                self.assertEqual(sa.reel_label_from_title(p["title"]), "Q4 reel")

    def test_only_the_trailing_suffix_is_stripped(self):
        self.assertEqual(sa.strip_part_suffix("X - Y - part 2"), "X - Y")
        self.assertEqual(sa.strip_part_suffix("Moby Dick - Herman Melville"),
                         "Moby Dick - Herman Melville")

    def _sprint(self, title):
        return {"id": "p", "post_kind": "project_sprint",
                "collection_name": "MTG Archive", "title": title,
                "asset_ids": ["a1", "a2"], "studio": "b"}

    def test_a_stale_comma_suffix_makes_derived_id_raise(self):
        """⛔ No compatibility: stale data fails loudly."""
        with self.assertRaises(mpi.Unresolvable):
            mpi.derived_id(self._sprint("MTG Archive lock-in pass, part 1"))

    def test_the_hyphen_suffix_derives_the_same_id_as_no_suffix(self):
        bare = mpi.derived_id(self._sprint("MTG Archive lock-in pass"))
        self.assertEqual(
            mpi.derived_id(self._sprint("MTG Archive lock-in pass - part 1")),
            bare)


class TestAssetTitleWriters(unittest.TestCase):
    """R8 and R9. Every writer that stores an asset title applies the
    rule to the title it stores, and only after the id is derived."""

    def test_the_local_csv_writer(self):
        row = {"asset_id": "ast-test", "kind": "raster",
               "file_size_bytes": "10", "file_path": "images/x/a.png",
               "title": f"Card sketch (paper, pencil) {EM} v2",
               "project": "Project Mirror", "team": "Art"}
        rec = sa.transform_row(row)
        self.assertEqual(rec.title, "Card sketch (paper - pencil) - v2")
        self.assertEqual(rec.id, sa.stable_uuid("asset", "ast-test"))

    def test_the_internet_writer(self):
        name = f"Moby Dick {EM} Herman Melville, 1851"
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / "MANIFEST.json").write_text(json.dumps({"assets": [
                {"name": name, "path": "x/moby.txt", "size_bytes": 5,
                 "asset_type": "document", "sha256": "ab" * 32}]}))
            rec = sa.load_internet_assets(Path(d))[0]
        self.assertEqual(rec.title, "Moby Dick - Herman Melville - 1851")
        self.assertEqual(rec.id, sa.stable_uuid("asset", "internet", "ab" * 32))
        self.assertEqual(rec.description,
                         f"{name} {EM} public-safe reference content.")

    def test_the_torrent_writer_on_a_synthetic_entry(self):
        name = f"Foo {EM} bar, baz"
        with tempfile.TemporaryDirectory() as d:
            doc = Path(d) / "t.json"
            doc.write_text(json.dumps({"assets": [
                {"name": name, "file_path": "videos/torrent/x.mp4",
                 "file_size_bytes": 5}]}))
            rec = sa.load_torrent_imports(doc)[0]
        self.assertEqual(rec.title, "Foo - bar - baz")
        seed = hashlib.sha256(f"{name}|5".encode()).hexdigest()
        self.assertEqual(rec.id, sa.stable_uuid("asset", "torrent", seed))
        self.assertEqual(rec.description,
                         f"{name} {EM} Blender Foundation open content.")

    def test_the_committed_torrent_ids_derive_from_the_raw_name(self):
        """R9. ⛔ No torrent entry stores a `sha_seed`, so the hash of the
        manifest's own name IS the id. Normalising before the hash would
        move all three, and this is the test that sees it."""
        records = sa.load_torrent_imports(SCRIPTS / "torrent_imports.json")
        self.assertEqual(len(records), 3)
        raw = json.loads((SCRIPTS / "torrent_imports.json")
                         .read_text(encoding="utf-8"))["assets"]
        for name in ("studio-a", "studio-b"):
            by_id = {a["id"]: a for a in _profile(f"{name}.assets")}
            for rec, entry in zip(records, raw):
                self.assertIn(rec.id, by_id, f"{name}: {entry['name']!r}")
                committed = by_id[rec.id]
                self.assertEqual(rec.title, committed["title"])
                self.assertFalse(sa.has_title_separator(rec.title))
                self.assertEqual(rec.description, committed["description"])
                self.assertEqual(rec.description, entry["notes"])
                moved = sa.stable_uuid("asset", "torrent", hashlib.sha256(
                    f"{rec.title}|{entry['file_size_bytes']}".encode()
                ).hexdigest())
                self.assertNotEqual(moved, rec.id,
                                    "the probe cannot tell the orders apart")

    def test_the_pexels_writer(self):
        video = {"id": 123, "user": {"name": "Doe, Jane"},
                 "url": "https://www.pexels.com/video/123/", "duration": 5}
        vf = {"width": 1280, "height": 720,
              "link": "https://videos.pexels.com/x.mp4"}
        spec = {"collection": "C", "team": "VFX", "tags": ["a"],
                "why": "w", "q": "q"}
        rec = px.build_record(video, vf, spec, 10)
        self.assertEqual(rec["title"], "Pexels 123 Doe - Jane")
        self.assertEqual(rec["id"], px.stable_uuid("asset", "pexels:123"))
        self.assertEqual(px.build_post(rec)["title"], "Pexels 123 Doe - Jane")

    def test_the_studio_balance_writer(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            rel = "Audio/impact, heavy.ogg"
            (root / "Pack" / "Audio").mkdir(parents=True)
            (root / "Pack" / rel).write_bytes(b"OggS" + b"\0" * 32)
            rule = {"pack": "Pack", "kind": "bitmap", "collection": "C",
                    "tags": ["t"]}
            source = {"page": "https://kenney.nl/assets/x",
                      "zip_url": "https://kenney.nl/x.zip"}
            rec, _ = sb.build_record("Audio", rule, rel, root, source,
                                     ["o"], ["r"], None)
        self.assertEqual(rec["title"], "Impact - heavy")
        self.assertEqual(rec["id"],
                         sb.stable_uuid("asset", "kenney-allin1", f"Pack/{rel}"))

    def test_the_hq_replacement_writer(self):
        self.assertEqual(
            up.title_for("2d-assets-brick-pack-brick,high-1-36a68e65-512.png"),
            "Brick pack brick-high 1 (vector)")
        profile = [_asset("id-1", "images/pack/a.png")]
        up.apply_replacements(profile, [{
            "id": "id-1", "old": "images/pack/a.png", "oldSize": 1,
            "new": "images/kenney-hq/2d-assets-a, b-36a68e65-512.png",
            "newSize": 2}])
        self.assertEqual(profile[0]["title"], "A - b (vector)")
        self.assertEqual(profile[0]["id"], "id-1")


class TestMergeAddedRefusesUnnormalizedTitles(unittest.TestCase):
    """R10. An upgrade document is historical evidence: a NEW record whose
    title breaks the rule is refused, never normalised."""

    def _site(self, td: Path, title: str):
        upgrades = td / "upgrades"
        upgrades.mkdir()
        (upgrades / "kenney-hq-replacements.site_a.json").write_text("[]")
        rec = _asset("new-1", "videos/internet/new.mp4")
        rec["title"] = title
        rec["metadata"] = {"media_url": "https://example.test/new.mp4",
                           "fetched_from": "https://example.test/new"}
        (upgrades / "added-assets.site_a.json").write_text(json.dumps([rec]))
        (upgrades / "added-posts.site_a.json").write_text(json.dumps(
            [{"id": "post-new", "asset_ids": ["new-1"], "title": "New"}]))
        (td / "assets.json").write_text(json.dumps(
            [_asset("id-1", "images/pack/a.png")]))
        (td / "posts.json").write_text(json.dumps(
            [{"id": "post-1", "asset_ids": ["id-1"], "title": "Old"}]))
        return upgrades

    def _run(self, td: Path, upgrades: Path):
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
             "--site", "site_a", "--upgrades", str(upgrades),
             "--profile", str(td / "assets.json"),
             "--posts", str(td / "posts.json")],
            capture_output=True, text=True)

    def _assert_refused(self, title):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, title)
            before = {f: (td / f).read_bytes()
                      for f in ("assets.json", "posts.json")}
            r = self._run(td, upgrades)
            self.assertNotEqual(r.returncode, 0,
                                f"merged silently:\n{r.stderr}")
            self.assertIn("new-1", r.stderr)
            self.assertIn("added-assets.site_a.json", r.stderr)
            for f, data in before.items():
                self.assertEqual((td / f).read_bytes(), data,
                                 f"{f} was written")

    def test_a_new_record_titled_with_a_comma_is_refused(self):
        self._assert_refused("Reference, new")

    def test_a_new_record_titled_with_an_em_dash_is_refused(self):
        self._assert_refused(f"Reference {EM} new")

    def test_the_same_document_with_a_hyphen_merges(self):
        """The control: the harness can succeed, so the refusals above
        are about the title and nothing else."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            r = self._run(td, self._site(td, "Reference - new"))
            self.assertEqual(r.returncode, 0, r.stderr)
            ids = {a["id"] for a in json.loads((td / "assets.json").read_text())}
            self.assertIn("new-1", ids)

    def test_a_record_the_profile_already_holds_is_not_refused(self):
        """The repair branch is unaffected: a record already merged is not
        new, whatever its title."""
        existing = _asset("id-1", "images/pack/a.png")
        existing["title"] = "Legacy, title"
        profile = [existing]
        doc = [{**_asset("id-1", "images/pack/a.png"), "title": "Legacy, title",
                "metadata": {"media_url": "https://example.test/a.png"}}]
        self.assertEqual(up.merge_added(profile, doc), (0, 1, 0))
        self.assertEqual(profile[0]["title"], "Legacy, title")

    def test_the_function_refuses_before_appending_anything(self):
        profile = [_asset("id-1", "images/pack/a.png")]
        good = {**_asset("ok-1", "images/x/ok.png"), "title": "Fine"}
        bad = {**_asset("bad-1", "images/x/bad.png"), "title": "Not, fine"}
        with self.assertRaises(up.UnnormalizedTitle) as cm:
            up.merge_added(profile, [good, bad],
                           sources={"bad-1": "balance-assets.site_a.json"})
        self.assertIn("bad-1", str(cm.exception))
        self.assertIn("balance-assets.site_a.json", str(cm.exception))
        self.assertEqual([e["id"] for e in profile], ["id-1"])

    def test_no_committed_upgrade_document_carries_a_refused_title(self):
        """Every committed asset document keeps merging exactly as before."""
        seen = 0
        for site in ("site_a", "site_b"):
            for stem in up.DOC_SETS:
                path = UPGRADES / f"{stem}-assets.{site}.json"
                if not path.is_file():
                    continue
                for rec in json.loads(path.read_text(encoding="utf-8")):
                    seen += 1
                    self.assertFalse("," in rec["title"] or EM in rec["title"],
                                     f"{path.name}: {rec['id']}")
        self.assertGreater(seen, 0)


class TestNaturalPostCommaSurvives(unittest.TestCase):
    """R11, preservation. These pass on the commit before #1319 as well;
    they are here so the post path can never grow into a blind replace."""

    TITLE = "Notes, sketches and studies"

    def test_the_title_passes_are_no_ops_on_a_natural_comma(self):
        self.assertEqual(sa.clean_dashes(self.TITLE), self.TITLE)
        posts = [{"id": "1", "post_kind": "multi_asset", "title": self.TITLE,
                  "asset_ids": []}]
        sa.retitle_posts(posts)
        self.assertEqual(posts[0]["title"], self.TITLE)
        sa.disambiguate_titles(posts)
        self.assertEqual(posts[0]["title"], self.TITLE)

    def test_the_data_correction_leaves_a_natural_comma(self):
        posts = [{"id": "1", "post_kind": "multi_asset", "title": self.TITLE,
                  "asset_ids": ["a1"]}]
        n = sa.repunctuate_posts(
            posts, embedded_by_asset={"a1": ["Sintel , 480p trailer"]},
            em_dash_sources={"1": f"Notes {EM} other studies"})
        self.assertEqual(n, 0)
        self.assertEqual(posts[0]["title"], self.TITLE)

    def test_the_data_correction_fixes_only_what_has_an_origin(self):
        """A natural comma beside an inherited one: only the inherited one
        changes, which the asset rule applied to the whole title would
        get wrong."""
        title = "Notes, sketches on Sintel, 480p trailer, part 2"
        got = sa.repunctuate_post_title(
            title, embedded=["Sintel , 480p trailer"])
        self.assertEqual(got, "Notes, sketches on Sintel - 480p trailer - part 2")
        self.assertNotEqual(got, sa.normalize_title(title))
        self.assertEqual(sa.repunctuate_post_title(got,
                         embedded=["Sintel , 480p trailer"]), got)
        self.assertEqual(
            sa.repunctuate_post_title(
                "Animation, AI reference set",
                em_dash_source=f"Animation {EM} AI reference set"),
            "Animation - AI reference set")


class TestBalanceTemplateMatchesCommitted(unittest.TestCase):
    """R13. The balance generator emits the chunk form the committed
    profile carries, for every one of its 230 posts."""

    def test_every_balance_post_title_is_the_committed_one(self):
        records = json.loads((UPGRADES / "balance-assets.site_a.json")
                             .read_text(encoding="utf-8"))
        committed = {p["id"]: p["title"] for p in _profile("studio-a.posts")}
        doc_ids = {p["id"] for p in json.loads(
            (UPGRADES / "balance-posts.site_a.json").read_text(encoding="utf-8"))}
        generated = sb.build_posts(records)
        self.assertEqual({p["id"] for p in generated}, doc_ids)
        self.assertEqual(len(generated), 230)
        bad = [(p["title"], committed.get(p["id"])) for p in generated
               if sa.strip_part_suffix(committed.get(p["id"]) or "")
               != p["title"]]
        self.assertEqual(bad, [], f"{len(bad)} of 230 disagree")


class TestMeasureStagedRootPolicy(unittest.TestCase):
    """⛔ WHICH ROOTS MAY BE MEASURED FROM THE SHARE, AND WHY IT MATTERS.

    `apply_manifest_reconcile` refuses to take `file_size_bytes` from the
    archive share, and its reasoning is right: for a root copied from a
    reproducible source the share can simply be OLDER than the source.
    Measured 2026-08-27, 392 of site_b's 656 staged pool files disagree
    with a freshly built pool while the profile agrees with that pool on
    all 656 — so adopting the share's number there would have "corrected"
    a correct profile into one the next build refuses.

    A pre-staged root has no source to be stale against: the destination
    IS the artifact. That distinction is the whole licence for #1301's
    pass, so it gets a test rather than a comment.
    """

    def _site(self, tmp, rel, body):
        p = Path(tmp) / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(body)

    def test_a_source_backed_root_is_never_measured(self):
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "images/kenney-hq/a-512.png", b"stale pool render")
            profile = [{"id": "hq-1", "source_root": "hq",
                        "file_path": "images/kenney-hq/a-512.png",
                        "file_size_bytes": 999999}]
            entries, notes = ms.measure(profile, Path(d))
            self.assertEqual(entries, [],
                             "the share was allowed to overrule a rebuildable "
                             "source")
            # ⛔ `local` IS NO LONGER ONE OF THESE (#1319). Its source dataset
            # has been permanently retired, so there is nothing for the share
            # to be stale AGAINST; see the companion test below. `pack` still
            # rebuilds from an attested archive member.
            for root in sorted(ms.SOURCE_BACKED_ROOTS):
                profile[0]["source_root"] = root
                self.assertEqual(ms.measure(profile, Path(d))[0], [], root)
            self.assertNotIn("local", ms.SOURCE_BACKED_ROOTS)

    def test_a_preserved_local_root_is_measured_and_hashed(self):
        """⛔ THE OTHER HALF OF THE #1319 RECLASSIFICATION. `local` bytes are
        the archive's own: of 696 site_a and 552 site_b `local` records, 0
        carry a `metadata.media_url` and 0 carry a `metadata.source_archive`,
        and the source dataset they were copied from no longer exists. So the
        destination is the only description of them there is, which is the
        definition of a pre-staged root, and its hash is the attestation the
        preserved model needs."""
        body = b"the only copy of these bytes"
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "images/aurora-authored/plate.png", body)
            profile = [{"id": "l-1", "source_root": "local",
                        "file_path": "images/aurora-authored/plate.png",
                        "file_size_bytes": 999999}]
            entries, _ = ms.measure(profile, Path(d))
            self.assertEqual(len(entries), 1)
            self.assertEqual(entries[0]["bytes"], len(body))
            self.assertEqual(entries[0]["sha256"], hashlib.sha256(body).hexdigest())
            self.assertIn("local", ms.PRESTAGED_ROOTS)

    def test_verify_fails_on_a_preserved_local_mismatch(self):
        """R4. A `local` size disagreement was a permitted stale-share
        report; it is a refusal now. And the `hq` case MUST stay permitted:
        1 site_a and 392 site_b records disagree today, every one of them
        `hq`, so over-reaching here refuses every publish."""
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "images/aurora-authored/plate.png", b"short")
            prof = Path(d) / "p.json"
            rec = {"id": "l-1", "source_root": "local",
                   "file_path": "images/aurora-authored/plate.png",
                   "file_size_bytes": 999999}
            prof.write_text(json.dumps([rec]), encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(ms.cmd_verify(prof, Path(d)), 1)
            self.assertIn("MISMATCH [local]", err.getvalue())
            rec["source_root"] = "hq"
            prof.write_text(json.dumps([rec]), encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(ms.cmd_verify(prof, Path(d)), 0,
                                 "an hq disagreement must stay permitted")
            self.assertIn("source-backed record(s) differ", err.getvalue())

    def test_a_prestaged_root_is_measured_and_hashed(self):
        body = b"the two-minute cut"
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "videos/torrent/clip.avi", body)
            profile = [{"id": "v-1", "source_root": "torrent_import",
                        "file_path": "videos/torrent/clip.avi",
                        "file_size_bytes": 999999}]
            entries, _ = ms.measure(profile, Path(d))
            self.assertEqual(len(entries), 1)
            self.assertEqual(entries[0]["bytes"], len(body))
            self.assertEqual(entries[0]["origin_bytes"], 999999)
            self.assertEqual(entries[0]["sha256"],
                             hashlib.sha256(body).hexdigest())

    def test_an_internet_root_is_measured_but_never_hashed(self):
        """The hash is that record's IDENTITY (#1301). Recording a staged
        one would move its id on the next assembly."""
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "videos/internet/clip.webm", b"cut")
            profile = [{"id": "i-1", "source_root": "internet",
                        "file_path": "videos/internet/clip.webm",
                        "file_size_bytes": 4242}]
            entries, _ = ms.measure(profile, Path(d))
            self.assertEqual(len(entries), 1)
            self.assertEqual(entries[0]["bytes"], 3)
            self.assertNotIn("sha256", entries[0])

    def test_an_absent_file_is_a_note_and_never_a_zero(self):
        """⚠️ Unavailable is not absent and neither is zero. Emitting
        `bytes: 0` for a dropped mount would publish a manifest that
        describes nothing, and the re-fetch would then happily accept an
        empty file as correct."""
        with tempfile.TemporaryDirectory() as d:
            profile = [{"id": "v-1", "source_root": "site",
                        "file_path": "videos/internet/gone.webm",
                        "file_size_bytes": 10}]
            entries, notes = ms.measure(profile, Path(d))
            self.assertEqual(entries, [])
            self.assertEqual(len(notes), 1)
            self.assertIn("ABSENT", notes[0])

    def test_a_zero_byte_staged_file_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "videos/internet/empty.webm", b"")
            profile = [{"id": "v-1", "source_root": "site",
                        "file_path": "videos/internet/empty.webm",
                        "file_size_bytes": 10}]
            entries, notes = ms.measure(profile, Path(d))
            self.assertEqual(entries, [])
            self.assertIn("EMPTY", notes[0])

    def test_verify_reports_a_stale_share_without_failing(self):
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "images/kenney-hq/a-512.png", b"older render")
            prof = Path(d) / "p.json"
            prof.write_text(json.dumps([{
                "id": "hq-1", "source_root": "hq",
                "file_path": "images/kenney-hq/a-512.png",
                "file_size_bytes": 999999}]), encoding="utf-8")
            self.assertEqual(ms.cmd_verify(prof, Path(d)), 0)

    def test_verify_fails_on_a_measurable_mismatch(self):
        with tempfile.TemporaryDirectory() as d:
            self._site(d, "videos/torrent/clip.avi", b"short")
            prof = Path(d) / "p.json"
            prof.write_text(json.dumps([{
                "id": "v-1", "source_root": "torrent_import",
                "file_path": "videos/torrent/clip.avi",
                "file_size_bytes": 999999}]), encoding="utf-8")
            self.assertEqual(ms.cmd_verify(prof, Path(d)), 1)


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
        """⛔ RETIREMENT-AWARE (#1319). A documented retired record is
        deliberately in no post: it can never materialize beside its
        survivor, and a post naming it would silently lose a member. The
        exemption is exactly the documented set, asserted below, so it
        cannot grow by accident."""
        retired = _documented_retired_ids("studio-a")
        posted = {a for p in self.posts for a in p["asset_ids"]}
        orphans = [r["id"] for r in self.records if r["id"] not in posted]
        # ⚠️ NOT red-then-green, and deliberately not asserted as an
        # equality. Both documents here are HISTORICAL and neither is
        # rewritten by a retirement, so today the retired record is still
        # named by the historical balance post and the exemption is
        # unused. The assertion is a subset rule so that the first time
        # it IS used, only a documented id may use it.
        self.assertEqual(sorted(set(orphans) - retired), [],
                         "a balance record is unreachable on browse and no "
                         "asset-collapse document retires it")

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

    def test_the_alias_is_refreshed_by_the_pass_that_invalidates_it(self):
        """⛔ The re-copy used to live only in the assembler's
        full-assembly path, so running `apply_upgrade.py` on its own —
        which is what its own usage block tells you to do — left the
        alias holding the pre-upgrade profile. Nothing but the two tests
        above stood between that and a demo re-seed shipping the wrong
        records, and they only fail AFTER the drift is committed."""
        with tempfile.TemporaryDirectory() as td:
            d = Path(td)
            (d / "studio-a.assets.json").write_text('[{"id": "new"}]')
            (d / "demo.assets.json").write_text('[{"id": "old"}]')
            self.assertEqual(up.refresh_profile_alias(d / "studio-a.assets.json"),
                             "demo.assets.json")
            self.assertEqual((d / "demo.assets.json").read_text(), '[{"id": "new"}]')
            # A profile with no alias, and an alias file that is not there.
            self.assertIsNone(up.refresh_profile_alias(d / "studio-b.assets.json"))
            self.assertIsNone(up.refresh_profile_alias(d / "unrelated.assets.json"))

    def test_the_mapping_has_exactly_one_definition(self):
        """Spelling it out in both modules is how the two drift, which is
        the #572 bug the re-copy exists to prevent."""
        self.assertIs(sa.apply_upgrade.PROFILE_ALIASES, up.PROFILE_ALIASES)

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
        self.assertEqual(up.merge_added(entries, add), (0, 0, 0))
        self.assertEqual(up.merge_posts(posts, add_p), (0, 0))

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


class TestGuardMeasurementVsEdit(unittest.TestCase):
    """#1312 — the guard could not see a corrupted measurement.

    ADR 0097 splits authority: the profile owns CONTENT, the produced
    artifact owns MEASUREMENTS. The guard implemented neither half, so a
    profile carrying a stale byte count published over a destination
    carrying a true one and the report said "an edit, not a loss".
    Sprint 14 shipped 86 wrongly-"corrected" byte counts that way.

    ⭐ BOTH DIRECTIONS ARE TESTED HERE. A gate proven only to refuse is
    as broken as one proven only to permit: refusing every changed value
    would make the profile unable to correct anything it has published.
    """

    @staticmethod
    def _rec(rid, root, **kw):
        base = {"id": rid, "source_root": root, "title": f"asset {rid}",
                "file_size_bytes": 100, "metadata": {}, "field_values": {}}
        base.update(kw)
        return base

    # -- direction 1: a corrupted measurement must REFUSE -----------------

    def test_a_stale_byte_count_on_a_staged_root_is_refused(self):
        """The destination staged the bytes and is the only thing that
        weighed them. A source that disagrees is stale, not editing."""
        for root in sorted(mg.MEASURABLE_ROOTS):
            with self.subTest(root=root):
                cmp = mg.compare([self._rec("a", root, file_size_bytes=100)],
                                 [self._rec("a", root, file_size_bytes=250)],
                                 "assets")
                self.assertEqual([x.kind for x in cmp.losses],
                                 [mg.CORRUPTED_MEASUREMENT])
                self.assertEqual(cmp.losses[0].key, "file_size_bytes")
                self.assertEqual(cmp.losses[0].dest_value, 250)
                self.assertEqual(cmp.changes, [])
                self.assertFalse(cmp.ok)

    def test_origin_bytes_is_measured_too(self):
        cmp = mg.compare([self._rec("a", "site", metadata={"origin_bytes": 1})],
                         [self._rec("a", "site", metadata={"origin_bytes": 2})],
                         "assets")
        self.assertEqual([(x.kind, x.key) for x in cmp.losses],
                         [(mg.CORRUPTED_MEASUREMENT, "metadata.origin_bytes")])

    def test_the_refusal_reaches_the_report_and_names_its_own_remedy(self):
        """A corrupted measurement is not "stripped of a value" — the fix
        is to RE-MEASURE, not to carry the old number back."""
        cmp = mg.compare([self._rec("a", "site", file_size_bytes=100)],
                         [self._rec("a", "site", file_size_bytes=250)],
                         "assets")
        report = mg.format_report(cmp)
        self.assertIn("WOULD OVERWRITE A MEASUREMENT", report)
        self.assertIn("measure_staged.py", report)
        # It must NOT be counted as a stripped value: that number drives
        # the "carry it back into the profile" repair, which is wrong here.
        self.assertEqual(cmp.records_degraded, 0)
        self.assertEqual(cmp.stripped, [])
        self.assertNotIn("stripped of", report)

    # -- direction 2: a legitimate edit must still PASS -------------------

    def test_a_content_edit_still_passes(self):
        """⛔ THE TRAP. Promoting every CHANGED_VALUE to a loss refuses
        every legitimate edit and makes the guard unusable."""
        cmp = mg.compare([self._rec("a", "site", title="corrected title",
                                    field_values={"credit": "new"})],
                         [self._rec("a", "site", title="old title",
                                    field_values={"credit": "old"})],
                         "assets")
        self.assertEqual(cmp.losses, [])
        self.assertEqual(sorted(x.kind for x in cmp.changes),
                         [mg.CHANGED_VALUE, mg.CHANGED_VALUE])
        self.assertTrue(cmp.ok)

    def test_a_source_backed_root_may_disagree_and_still_publish(self):
        """⛔ MEASURED, NOT ASSUMED. On 2026-08-27 a freshly built
        kenney-hq pool matched studio-b's PROFILE on all 656 `hq` records
        and site_b's published manifest on only 264. The share held an
        older pool build. Refusing those 392 would have "corrected" a
        correct profile into a broken one, and the very next build would
        then have refused its own input."""
        for root in sorted(mg.SOURCE_BACKED_ROOTS):
            with self.subTest(root=root):
                cmp = mg.compare([self._rec("a", root, file_size_bytes=35433)],
                                 [self._rec("a", root, file_size_bytes=17313)],
                                 "assets")
                self.assertEqual(cmp.losses, [])
                self.assertEqual([x.kind for x in cmp.changes],
                                 [mg.CHANGED_VALUE])
                self.assertTrue(cmp.ok)

    # -- the rule of two -------------------------------------------------

    def test_the_same_field_is_a_measurement_or_identity_by_RECORD_CLASS(self):
        """⛔⛔ `metadata.sha256` is a measurement on an `hq`/`site`
        record and IDENTITY on an `internet` one — the asset id is
        `stable_uuid("asset", "internet", sha256)`. A single rule over the
        field NAME gets one of the two wrong, and the wrong one moves
        published asset ids."""
        sha_src, sha_dst = "a" * 64, "b" * 64
        internet = mg.compare(
            [self._rec("a", "internet", metadata={"sha256": sha_src})],
            [self._rec("a", "internet", metadata={"sha256": sha_dst})], "assets")
        self.assertEqual(internet.losses, [])
        self.assertEqual([x.kind for x in internet.changes], [mg.CHANGED_VALUE])
        self.assertTrue(internet.ok)

        staged = mg.compare(
            [self._rec("a", "site", metadata={"sha256": sha_src})],
            [self._rec("a", "site", metadata={"sha256": sha_dst})], "assets")
        self.assertEqual([x.kind for x in staged.losses],
                         [mg.CORRUPTED_MEASUREMENT])
        self.assertFalse(staged.ok)

        # …and the byte count of that same `internet` record IS measured.
        # Only the hash is exempt, because only the hash is identity.
        cmp = mg.compare([self._rec("a", "internet", file_size_bytes=100)],
                         [self._rec("a", "internet", file_size_bytes=250)],
                         "assets")
        self.assertEqual([x.kind for x in cmp.losses],
                         [mg.CORRUPTED_MEASUREMENT])

    def test_one_record_can_carry_a_corruption_AND_an_edit(self):
        """⭐ The verdict is per FIELD. All-or-nothing per record would
        either publish the stale number or refuse the real edit."""
        cmp = mg.compare(
            [self._rec("a", "site", file_size_bytes=100, title="corrected")],
            [self._rec("a", "site", file_size_bytes=250, title="old")],
            "assets")
        self.assertEqual([(x.kind, x.key) for x in cmp.losses],
                         [(mg.CORRUPTED_MEASUREMENT, "file_size_bytes")])
        self.assertEqual([(x.kind, x.key) for x in cmp.changes],
                         [(mg.CHANGED_VALUE, "title")])

    def test_a_disputed_root_refuses_rather_than_permits(self):
        """`source_root` is content, so the two sides can disagree about
        it. The union is taken: a disagreement can add a refusal but can
        never hide one, and a refusal is the recoverable mistake."""
        cmp = mg.compare([self._rec("a", "hq", file_size_bytes=100)],
                         [self._rec("a", "site", file_size_bytes=250)],
                         "assets")
        self.assertEqual([x.kind for x in cmp.losses],
                         [mg.CORRUPTED_MEASUREMENT])

    def test_a_record_with_no_source_root_is_treated_as_local(self):
        """Posts carry no `source_root`, and neither did the profiles
        before #572. Defaulting to a measurable root would refuse every
        post edit."""
        cmp = mg.compare([{"id": "a", "title": "new"}],
                         [{"id": "a", "title": "old"}], "posts")
        self.assertEqual(cmp.losses, [])
        self.assertTrue(cmp.ok)


class TestKindMatchesItsTitleTemplate(unittest.TestCase):
    """#1314 — `post_kind` is claimed to disagree with the title template
    that produced the post, on 228 site_a posts.

    ⛔ THE FIGURE DID NOT REPRODUCE, AND THE PREMISE BEHIND IT IS WRONG.
    Re-running each kind's OWN formatter over each post's own fields, a
    FRESH ASSEMBLY of site_a (1,103 posts, 11 kinds) disagrees on ZERO.
    The generator is self-consistent, so "the assembler assigns
    `post_kind` inconsistently with the template it then applies" is
    refuted, and no id is being fed by a wrong kind.

    The committed `studio-a.posts.json` does disagree, on 374 rather than
    228, and none of the three causes is a generator defect:

      244 `asset_group` posts carry a collection-chunk title (230 of them
          exactly `{collection}: {team} {theme}`). The group pass emits
          only `title_group_set` / `title_group_bundle`, so these are
          from an older generator.
       83 `solo_showcase` and `multi_asset` posts carry titles AUTHORED
          BY UPGRADE DOCUMENTS (`added-posts`, `mature-posts`), which no
          template produced and none should.
       47 `revision_*` and `video_*` posts embed a member asset's title
          as it was BEFORE the HQ replacement renamed the asset.

    All three are the same underlying fact: the committed posts profile
    is a frozen historical composition of 863 posts where the assembler
    now emits 1,103, which `retitle_posts` already records. This test
    pins the half that is checkable and permanent.
    """

    KINDS = 11

    @classmethod
    def setUpClass(cls):
        raw = json.loads((PROFILES / "studio-a.assets.json")
                         .read_text(encoding="utf-8"))
        cls.assets = {a["id"]: a for a in raw}
        records = sa.asset_records_from_profile(raw, PROFILES / "studio-a.assets.json")
        cls.posts = sa.derive_posts(records)
        cls.flavours = {f[:1].upper() + f[1:] for f in sa.ALL_SOLO_FLAVORS}

    def _agrees(self, post):
        """True when the post's title is what its kind's formatter makes.

        ⚠️ Returns None for a kind this test does not know, so a NEW kind
        fails `test_every_kind_is_covered` instead of passing silently.
        """
        kind = post.get("post_kind")
        title = sa.strip_part_suffix(post.get("title") or "")
        members = [self.assets[i] for i in (post.get("asset_ids") or ())
                   if i in self.assets]
        if kind == "asset_group":
            anchors = {sa.title_group_set(m["title"]) for m in members}
            anchors |= {sa.title_group_bundle(m["title"]) for m in members}
            return title in {sa.clean_dashes(a) for a in anchors}
        if kind == "multi_asset":
            return title.startswith(
                f"{post.get('collection_name')}: {post.get('team_name')} ")
        if kind == "solo_showcase":
            return ": " in title and title.split(": ", 1)[0] in self.flavours
        if kind == "team_roundup":
            return title == sa.title_team_roundup(post.get("team_name") or "")
        if kind == "project_sprint":
            return sa.sprint_label_from_title(
                post.get("collection_name") or "", title) in sa.SPRINT_LABELS
        if kind == "cinematics_showreel":
            return sa.reel_label_from_title(title) in sa.REEL_LABELS
        for prefix, table in (("revision_", sa.REVISION_TITLES),
                              ("video_", sa.VIDEO_TITLES)):
            if kind and kind.startswith(prefix):
                tmpl = table.get(kind[len(prefix):])
                return bool(tmpl) and any(
                    title == sa.clean_dashes(tmpl.format(title=m["title"]))
                    for m in members)
        return None

    def test_a_fresh_assembly_has_ZERO_disagreements(self):
        """⭐ #1314's acceptance, as a permanent invariant rather than a
        one-off audit. It passes today; what it is here for is the next
        title edit, which is exactly how #1306 broke the sprint-label
        parse."""
        bad = [(p["post_kind"], p["title"]) for p in self.posts
               if self._agrees(p) is False]
        self.assertEqual(bad, [], f"{len(bad)} post(s) disagree")

    def test_every_kind_is_covered(self):
        """⛔ A kind this test does not recognise reads as "no
        disagreement", which is the shape of a gate that cannot fail. The
        count is asserted so a new kind has to come here first."""
        kinds = {p["post_kind"] for p in self.posts}
        self.assertEqual(len(kinds), self.KINDS, sorted(kinds))
        unknown = {p["post_kind"] for p in self.posts
                   if self._agrees(p) is None}
        self.assertEqual(unknown, set())

    def test_the_check_can_actually_fail(self):
        """The denominator. A test that only ever sees agreement proves
        nothing about its own ability to see disagreement."""
        good = dict(self.posts[0])
        self.assertIsNot(self._agrees(good), False)
        self.assertFalse(self._agrees({**good, "title": "not a template"}))


class TestBundleIdentity(unittest.TestCase):
    """#1310 — a bundle's id named ONE MEMBER, not the bundle.

    ⛔ NOT THE COLLISION #1293 WAS. Measured on the committed profile,
    863 rows under 863 distinct ids and zero collisions, because the
    bundle loop partitions its cluster into DISJOINT chunks so two
    bundles cannot share an anchor. The defect is that the key names an
    accompanying value rather than an identifying one (ADR 0098).
    """

    def test_the_key_is_the_membership_and_nothing_else(self):
        self.assertEqual(sa.bundle_post_id(["a", "b", "c"]),
                         sa.stable_uuid("post", "bundle", "a", "b", "c"))

    def test_member_ORDER_does_not_change_the_id(self):
        """A bundle is a set of assets. Reordering them is a
        presentation change, and the curation reorders 383 posts."""
        self.assertEqual(sa.bundle_post_id(["c", "a", "b"]),
                         sa.bundle_post_id(["a", "b", "c"]))

    def test_a_different_membership_is_a_different_bundle(self):
        self.assertNotEqual(sa.bundle_post_id(["a", "b"]),
                            sa.bundle_post_id(["a", "b", "c"]))

    def test_it_does_not_depend_on_anything_outside_the_membership(self):
        """⭐ THE POINT, AND IT IS MEASURABLE. The old key was the
        chunk's most-recent member by `updated_at`, while the cluster
        sorts by `created_at` — two independent fields. So editing one
        asset's `updated_at`, which changes no membership at all, moved a
        bundle id: measured on studio-a, 1 of 340 bundle ids moved under
        the anchor key and 0 under this one."""
        ids = ["a", "b", "c"]
        self.assertEqual(sa.bundle_post_id(ids), sa.bundle_post_id(list(ids)))
        # A generator is accepted, and consumed once.
        self.assertEqual(sa.bundle_post_id(i for i in ids),
                         sa.bundle_post_id(ids))

    def test_multi_asset_is_a_migrated_kind_and_needs_no_title_parse(self):
        """The other three kinds recover a label from the title, so a
        wording change can move an id. A bundle's key is its membership,
        so there is nothing to parse and nothing to break."""
        self.assertIn("multi_asset", mpi.MIGRATED_KINDS)
        post = {"id": "x", "post_kind": "multi_asset",
                "asset_ids": ["a", "b"], "title": "anything at all"}
        self.assertEqual(mpi.derived_id(post), sa.bundle_post_id(["a", "b"]))
        post["title"] = "something completely different"
        self.assertEqual(mpi.derived_id(post), sa.bundle_post_id(["a", "b"]))


class TestMigrationDocumentAccumulates(unittest.TestCase):
    """#1310 — the mapping document is how a publish tells a MIGRATION
    from a LOSS (ADR 0097), and a second migration replaced it."""

    def test_a_second_migration_keeps_the_first_ones_moves(self):
        prior = [{"old_id": "A", "new_id": "B", "post_kind": "team_roundup",
                  "title": "t", "members": 2}]
        fresh = [{"old_id": "X", "new_id": "Y", "post_kind": "multi_asset",
                  "title": "u", "members": 3}]
        out = mpi.accumulate_moves(prior, fresh)
        self.assertEqual(sorted((m["old_id"], m["new_id"]) for m in out),
                         [("A", "B"), ("X", "Y")])

    def test_a_chained_move_is_COMPOSED_to_its_destination(self):
        """⭐ The published site holds A. Recording A->B and B->C leaves
        the reader to compose them; what it can look up is A->C."""
        prior = [{"old_id": "A", "new_id": "B", "post_kind": "team_roundup",
                  "title": "t", "members": 2}]
        fresh = [{"old_id": "B", "new_id": "C", "post_kind": "team_roundup",
                  "title": "t", "members": 2}]
        out = mpi.accumulate_moves(prior, fresh)
        self.assertEqual([(m["old_id"], m["new_id"]) for m in out], [("A", "C")])

    def test_the_shipped_document_still_holds_the_1293_moves(self):
        doc = json.loads((UPGRADES / "post-id-migration.studio-a.json")
                         .read_text(encoding="utf-8"))
        kinds = collections.Counter(m["post_kind"] for m in doc["moves"])
        self.assertEqual(kinds["team_roundup"], 29)
        self.assertEqual(kinds["project_sprint"], 35)
        self.assertEqual(kinds["cinematics_showreel"], 4)
        self.assertEqual(kinds["multi_asset"], 107)

    def test_no_upgrade_document_holds_a_stale_post_id(self):
        """⛔ `mature-posts.site_a.json` held two `multi_asset` ids under
        the old key. `merge_posts` keys on the id, so the profile no
        longer having them meant the next run MERGED THEM BACK IN and
        site_a went 863 -> 865 with two duplicated posts."""
        for path in sorted(UPGRADES.glob("*-posts.site_*.json")):
            rows = json.loads(path.read_text(encoding="utf-8"))
            for row in rows:
                if row.get("post_kind") not in mpi.MIGRATED_KINDS:
                    continue
                self.assertEqual(row["id"], mpi.derived_id(row),
                                 f"{path.name}: {row['id']} does not derive")


class TestPostCuration(unittest.TestCase):
    """#1309 — the hand-made feed ordering, reproduced by the build."""

    @staticmethod
    def _post(pid, **kw):
        base = {"id": pid, "title": f"post {pid}", "post_kind": "team_roundup",
                "asset_ids": ["a1", "a2"], "created_at": "2025-01-01T00:00:00Z",
                "updated_at": "2025-01-01T00:00:00Z"}
        base.update(kw)
        return base

    def test_merge_posts_SKIPS_an_existing_post(self):
        """⛔ THE TRAP, STATED AS A TEST. `apply_upgrade` discovers
        `{stem}-posts.{site}.json` documents and hands them to
        `merge_posts`, so filing the curation under that convention is
        the obvious move. Every curated post already exists, so all of
        them would be skipped and the run would report success.

        This test exists so that if anyone ever routes curation through
        `merge_posts`, something goes red instead of quiet."""
        posts = [self._post("p1", created_at="2025-01-01T00:00:00Z")]
        n, _ = up.merge_posts(posts, [self._post("p1", created_at="2026-06-06T00:00:00Z")])
        self.assertEqual(n, 0)
        self.assertEqual(posts[0]["created_at"], "2025-01-01T00:00:00Z")

    def test_curation_amends_a_post_that_already_exists(self):
        posts = [self._post("p1")]
        n_posts, n_vals, missing, advisories = up.apply_post_curation(
            posts, {"curate": [
                {"id": "p1", "created_at": "2026-06-06T00:00:00Z",
                 "asset_ids": ["a2", "a1"]}]})
        self.assertEqual((n_posts, n_vals, missing, advisories), (1, 2, [], []))
        self.assertEqual(posts[0]["created_at"], "2026-06-06T00:00:00Z")
        self.assertEqual(posts[0]["asset_ids"], ["a2", "a1"])

    def test_member_ORDER_is_the_thing_being_reproduced(self):
        """383 of the 388 curated memberships hold the same assets in a
        different order. Comparing them as SETS would call the entire
        hero placement a no-op."""
        posts = [self._post("p1", asset_ids=["a1", "a2", "a3"])]
        up.apply_post_curation(posts, {"curate": [
            {"id": "p1", "asset_ids": ["a3", "a1", "a2"]}]})
        self.assertEqual(posts[0]["asset_ids"], ["a3", "a1", "a2"])

    def test_only_the_fields_NAMED_in_an_entry_are_written(self):
        posts = [self._post("p1", title="kept", description="kept too")]
        up.apply_post_curation(posts, {"curate": [
            {"id": "p1", "created_at": "2026-06-06T00:00:00Z"}]})
        self.assertEqual(posts[0]["title"], "kept")
        self.assertEqual(posts[0]["description"], "kept too")

    def test_a_key_outside_the_closed_list_is_not_written(self):
        """The document is hand-recovered from backups. A typo'd key that
        silently created a new field on 841 posts would be invisible."""
        posts = [self._post("p1")]
        up.apply_post_curation(posts, {"curate": [
            {"id": "p1", "titel": "typo", "post_kind": "solo_showcase"}]})
        self.assertNotIn("titel", posts[0])
        self.assertEqual(posts[0]["post_kind"], "team_roundup")

    def test_a_curated_post_that_no_longer_exists_is_reported_not_created(self):
        """⛔ The document carries values, not membership or kind. It
        cannot build a post, so it must not pretend to."""
        posts = [self._post("p1")]
        n_posts, n_vals, missing, advisories = up.apply_post_curation(
            posts, {"curate": [{"id": "gone",
                                "created_at": "2026-06-06T00:00:00Z"}]})
        self.assertEqual((n_posts, n_vals), (0, 0))
        self.assertEqual(len(posts), 1)
        self.assertEqual(len(missing), 1)
        self.assertIn("gone", missing[0])
        # ⛔ #1324. The loss must NOT arrive on the advisory channel, or
        # a caller that refuses on losses has to refuse on advisories
        # too, which makes a documented "look here" fatal.
        self.assertEqual(advisories, [])

    def test_it_is_idempotent(self):
        posts = [self._post("p1")]
        doc = {"curate": [{"id": "p1", "created_at": "2026-06-06T00:00:00Z"}]}
        first = up.apply_post_curation(posts, doc)
        snapshot = json.loads(json.dumps(posts))
        second = up.apply_post_curation(posts, doc)
        self.assertEqual(first[:2], (1, 1))
        self.assertEqual(second[:2], (0, 0))
        self.assertEqual((first[2], first[3], second[2], second[3]),
                         ([], [], [], []))
        self.assertEqual(posts, snapshot)

    def test_it_never_creates_deletes_or_reorders_posts(self):
        posts = [self._post("p1"), self._post("p2"), self._post("p3")]
        up.apply_post_curation(posts, {"curate": [
            {"id": "p3", "created_at": "2026-06-06T00:00:00Z"},
            {"id": "p1", "created_at": "2026-06-06T00:00:00Z"}]})
        self.assertEqual([p["id"] for p in posts], ["p1", "p2", "p3"])

    def test_membership_drift_is_reported_rather_than_silently_applied(self):
        """⚠️ The document holds values, not reasoning. If the assembler
        later changes a post's membership, the curated order and date go
        on being applied to something else and nothing would notice.
        `pipeline_members` is what makes that visible."""
        posts = [self._post("p1", asset_ids=["a1", "a9"])]
        doc = {"curate": [{"id": "p1", "created_at": "2026-06-06T00:00:00Z",
                           "pipeline_members": up._members_digest(["a1", "a2"])}]}
        n_posts, n_vals, missing, advisories = up.apply_post_curation(posts, doc)
        self.assertEqual(len(advisories), 1)
        self.assertIn("membership has moved", advisories[0])
        # ⛔ #1324. And the advisory must NOT arrive on the loss channel.
        self.assertEqual(missing, [])
        # It still applies. The curation is the owner's decision; this is
        # a report, not a veto.
        self.assertEqual((n_posts, n_vals), (1, 1))
        self.assertEqual(posts[0]["created_at"], "2026-06-06T00:00:00Z")

    def test_a_matching_membership_digest_is_silent(self):
        posts = [self._post("p1", asset_ids=["a2", "a1"])]
        doc = {"curate": [{"id": "p1", "created_at": "2026-06-06T00:00:00Z",
                           "pipeline_members": up._members_digest(["a1", "a2"])}]}
        _, _, missing, advisories = up.apply_post_curation(posts, doc)
        self.assertEqual((missing, advisories), ([], []))

    def test_the_shipped_document_carries_no_title(self):
        """⛔⛔ THE MEASUREMENT THAT DECIDED THIS DOCUMENT'S CONTENT.
        The published posts.json disagrees with the pipeline on 780
        titles and NOT ONE is a hand edit: 774 are #1306's rewrite and 6
        are the #1293 collision dedupe. A plain published-vs-pipeline
        diff would have codified all 780 and reverted #1306 on 774 posts,
        and it would have looked like it worked."""
        doc = json.loads((UPGRADES / "post-curation.site_a.json")
                         .read_text(encoding="utf-8"))
        keys = {k for e in doc["curate"] for k in e}
        self.assertEqual(keys - {"id", "pipeline_members"},
                         set(up.CURATABLE_FIELDS))
        self.assertNotIn("title", keys)

    def test_the_shipped_document_never_changes_a_MEMBERSHIP(self):
        """⛔⛔ Curating `asset_ids` on a `team_roundup` or
        `project_sprint` changes what its id should be, because those ids
        derive from membership (ADR 0098). The curation is hero
        PLACEMENT: every one of its 383 memberships is a reordering of
        the assets the assembler already put there.

        The five that changed the SET were excluded. They are not edits:
        they go 10 members to 8, 8 to 6, 8 to 7, 9 to 6 and 10 to 5, and
        they are the losing rows of the #1293 collision, which the
        published feed kept under the winner's id."""
        doc = json.loads((UPGRADES / "post-curation.site_a.json")
                         .read_text(encoding="utf-8"))
        posts = {p["id"]: p for p in json.loads(
            (PROFILES / "studio-a.posts.json").read_text(encoding="utf-8"))}
        for e in doc["curate"]:
            if "asset_ids" not in e:
                continue
            self.assertEqual(
                sorted(e["asset_ids"]),
                sorted(posts[e["id"]].get("asset_ids") or ()),
                f"{e['id']} curates a different membership, not an order")

    def test_the_shipped_document_curates_only_posts_the_profile_holds(self):
        doc = json.loads((UPGRADES / "post-curation.site_a.json")
                         .read_text(encoding="utf-8"))
        have = {p["id"] for p in json.loads(
            (PROFILES / "studio-a.posts.json").read_text(encoding="utf-8"))}
        missing = [e["id"] for e in doc["curate"] if e["id"] not in have]
        self.assertEqual(missing, [])


class TestCurationLossStopsTheRun(unittest.TestCase):
    """#1324. The pipeline drops hand curation and exits 0.

    ⛔ A PASS THAT CANNOT DO ITS JOB MUST NOT REPORT SUCCESS. Measured
    on 2026-08-27: a fresh assembly emits no `asset_group` posts (#1322),
    so 200 of the 841 curated posts are not in the document the curation
    is applied to. Their values are discarded. `apply_upgrade` printed
    "1135 hand-made value(s) reapplied to 641 post(s)", listed eight of
    the 200 losses under "… and 192 more", wrote all three profiles and
    exited 0. The curation document is the ONLY copy of that work.

    ⚠️ AND THE FIX MUST NOT OVERSHOOT. The pass reports two different
    things and one of them is deliberate: a curated post whose
    membership has moved is applied and flagged, because nothing can
    say whether the curation went bad. Making the whole warning list
    fatal would turn that documented advisory into a failure, so the
    two are separated at the source and only losses reach `problems`.

    Both halves are driven through the real script, end to end.
    """

    HQ = "images/kenney-hq/2d-assets-brick-pack-brick-high-1-36a68e65-512.png"

    def _site(self, td: Path, *, curate: list[dict]):
        upgrades = td / "upgrades"
        upgrades.mkdir(exist_ok=True)
        reps = [{"id": "id-1", "old": "images/pack/a.png", "oldSize": 605,
                 "new": self.HQ, "newSize": 8400}]
        (upgrades / "kenney-hq-replacements.site_a.json").write_text(
            json.dumps(reps), encoding="utf-8")
        (upgrades / "post-curation.site_a.json").write_text(
            json.dumps({"curate": curate}), encoding="utf-8")

        profile = [_asset("id-1", "images/pack/a.png")]
        up.apply_replacements(profile, reps)
        posts = [{"id": "post-1", "asset_ids": ["id-1"],
                  "created_at": "2025-01-01T00:00:00Z",
                  "updated_at": "2025-01-01T00:00:00Z"}]
        (td / "assets.json").write_text(json.dumps(profile), encoding="utf-8")
        (td / "posts.json").write_text(json.dumps(posts), encoding="utf-8")
        return upgrades

    def _run(self, td: Path, upgrades: Path, *extra: str):
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
             "--site", "site_a", "--upgrades", str(upgrades),
             "--profile", str(td / "assets.json"),
             "--posts", str(td / "posts.json"), *extra],
            capture_output=True, text=True)

    # -- the loss half -------------------------------------------------

    def test_a_curated_post_absent_from_the_profile_fails_the_run(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": "post-1", "created_at": "2026-06-06T00:00:00Z"},
                {"id": "post-gone", "created_at": "2026-06-06T00:00:00Z"},
            ])
            r = self._run(td, upgrades)
            self.assertEqual(
                r.returncode, 1,
                "a run that discarded hand curation reported success:\n"
                + r.stderr)
            self.assertIn("PROBLEM(S)", r.stderr)
            self.assertIn("post-gone", r.stderr)

    def test_the_loss_count_is_on_a_summary_line_not_only_in_the_tail(self):
        """⭐ The warning tail is capped at eight and ends in "… and N
        more", which is a shape a reader skims and a caller cannot see.
        The count belongs beside the count of what worked."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": f"post-gone-{i}", "created_at": "2026-06-06T00:00:00Z"}
                for i in range(12)])
            r = self._run(td, upgrades)
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertIn("curation    : 12 curated post(s) are ABSENT",
                          r.stderr)

    def test_the_profiles_are_NOT_written_when_curation_is_lost(self):
        """The refusal has to happen before the write, or the run has
        already published the reverted feed by the time it complains."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": "post-1", "created_at": "2026-06-06T00:00:00Z"},
                {"id": "post-gone", "created_at": "2026-06-06T00:00:00Z"}])
            before = (td / "posts.json").read_text(encoding="utf-8")
            self.assertEqual(self._run(td, upgrades).returncode, 1)
            self.assertEqual((td / "posts.json").read_text(encoding="utf-8"),
                             before)

    def test_a_lossless_curation_still_applies_and_exits_zero(self):
        """The other half of "seen to refuse": a gate that fails on
        everything is no better than one that never fails."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": "post-1", "created_at": "2026-06-06T00:00:00Z"}])
            r = self._run(td, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("1 hand-made value(s) reapplied to 1 post(s)",
                          r.stderr)
            self.assertNotIn("ABSENT", r.stderr)
            written = json.loads((td / "posts.json").read_text(encoding="utf-8"))
            self.assertEqual(written[0]["created_at"], "2026-06-06T00:00:00Z")

    # -- the advisory half ---------------------------------------------

    def test_a_MOVED_membership_is_advisory_and_the_run_still_succeeds(self):
        """⚠️ THE BRANCH REAL DATA DOES NOT CURRENTLY EXERCISE. Measured
        2026-08-27, the shipped document produces 200 MISSING and ZERO
        MOVED, so a fix that made the whole warning list fatal would
        have looked correct and stayed latent until the assembler next
        moved a membership. This fixture produces the MOVED case
        directly."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": "post-1", "created_at": "2026-06-06T00:00:00Z",
                 "pipeline_members": up._members_digest(["id-1", "id-9"])}])
            r = self._run(td, upgrades)
            self.assertEqual(
                r.returncode, 0,
                "a documented advisory was made fatal:\n" + r.stderr)
            self.assertIn("membership has moved", r.stderr)
            self.assertNotIn("PROBLEM(S)", r.stderr)
            # It still applied. The curation is the owner's decision;
            # the advisory is a report, not a veto.
            written = json.loads((td / "posts.json").read_text(encoding="utf-8"))
            self.assertEqual(written[0]["created_at"], "2026-06-06T00:00:00Z")

    def test_both_kinds_at_once_refuse_on_the_loss_and_report_the_move(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": "post-1", "created_at": "2026-06-06T00:00:00Z",
                 "pipeline_members": up._members_digest(["id-1", "id-9"])},
                {"id": "post-gone", "created_at": "2026-06-06T00:00:00Z"}])
            r = self._run(td, upgrades)
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertIn("membership has moved", r.stderr)
            self.assertIn("post-gone", r.stderr)
            # Exactly one PROBLEM: the loss. The advisory is not one.
            verdict = r.stderr.split("PROBLEM(S):", 1)[1]
            self.assertEqual(verdict.count("\n  - "), 1, verdict)

    # -- the pre-publish gate ------------------------------------------

    def test_check_mode_sees_curation_that_has_not_been_applied(self):
        """⚠️ "A pass missing from this list is a pass the pre-publish
        gate cannot see, which is what #1295 was." Curation was missing
        from it, so a rebuild that reverted the owner's feed ordering to
        the assembler's derived dates passed the gate."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": "post-1", "created_at": "2026-06-06T00:00:00Z"}])
            r = self._run(td, upgrades, "--check")
            self.assertEqual(
                r.returncode, 1,
                "the gate passed a profile the curation would change:\n"
                + r.stderr)
            verdict = r.stderr.split("FAIL:", 1)[1]
            self.assertIn("hand curation", verdict)
            self.assertIn("post-curation.site_a.json", verdict)

    def test_check_mode_passes_once_the_curation_has_been_applied(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, curate=[
                {"id": "post-1", "created_at": "2026-06-06T00:00:00Z"}])
            self.assertEqual(self._run(td, upgrades, "--check").returncode, 1)
            self.assertEqual(self._run(td, upgrades).returncode, 0)
            r = self._run(td, upgrades, "--check")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("OK: profile already reflects the upgrade", r.stderr)


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
        added, filled, _, unknown, _ = up.apply_manifest_reconcile(profile, doc)
        self.assertEqual((added, filled, unknown), (0, 2, []))
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
        self.assertEqual(first, (1, 1, 0, [], []))
        self.assertEqual(second, (0, 0, 0, [], []))
        self.assertEqual(profile, snapshot)

    def test_an_empty_value_counts_as_absent(self):
        profile = [_asset("a", "images/a.png", field_values={"blank": ""},
                          description="")]
        doc = {"fill": [{"id": "a", "field_values": {"blank": "filled"},
                         "description": "written"}]}
        _, filled, _, _, _ = up.apply_manifest_reconcile(profile, doc)
        self.assertEqual(filled, 2)
        self.assertEqual(profile[0]["field_values"]["blank"], "filled")

    def test_a_fill_naming_an_unknown_id_is_skipped_not_invented(self):
        """Skipped, and since #1328 also NAMED: the id comes back on its
        own channel so the caller can report it. Inventing a record from
        a fill entry would still be wrong; the pass only ever adds from
        `added`."""
        profile = [_asset("a", "images/a.png")]
        _, filled, _, unknown, _ = up.apply_manifest_reconcile(
            profile, {"fill": [{"id": "ghost", "field_values": {"k": "v"}}]})
        self.assertEqual((len(profile), filled, unknown), (1, 0, ["ghost"]))


class TestUnknownIdsAreNotSkippedInSilence(unittest.TestCase):
    """#1328. Two upgrade passes stepped over a document entry naming a
    record the profile does not hold, and said nothing.

    `apply_manifest_reconcile` and `apply_staged_measurements` both did
    `target = by_id.get(entry["id"]); if target is None: continue`. A
    mistyped or retired id produced a run that printed its counts, wrote
    the profiles, exited 0, and left `--check` saying "OK: profile
    already reflects the upgrade." Nothing about it was visible.

    The two passes are NOT treated alike, and the tests below assert the
    difference rather than a uniform refusal:

      reconcile   the pass can only add, so a skipped entry cannot make
                  the profile worse, and the record it names is what
                  `manifest_guard.py` reports as MISSING_RECORD at
                  publish. A normal run REPORTS the id on its own summary
                  line and still writes. `--check` is NOT clean over it,
                  and must not claim the pass "would change" the profile,
                  because running without --check would skip it again.
      staged      the document is the authority for the bytes the site
                  ships and is emitted from the very profile it must
                  match, so an id the profile lacks is a wrong input. It
                  is a PROBLEM: exit 1, nothing written, in both modes.

    Every case is driven through the real script as a subprocess so the
    exit code and the stderr a caller would see are what is asserted.
    """

    HQ = "images/kenney-hq/2d-assets-brick-pack-brick-high-1-36a68e65-512.png"

    def _site(self, td: Path, *, reconcile: dict | None = None,
              staged: list[dict] | None = None) -> Path:
        upgrades = td / "upgrades"
        upgrades.mkdir(exist_ok=True)
        reps = [{"id": "id-1", "old": "images/pack/a.png", "oldSize": 605,
                 "new": self.HQ, "newSize": 8400}]
        (upgrades / "kenney-hq-replacements.site_a.json").write_text(
            json.dumps(reps), encoding="utf-8")
        if reconcile is not None:
            (upgrades / "manifest-reconcile.site_a.json").write_text(
                json.dumps(reconcile), encoding="utf-8")
        if staged is not None:
            (upgrades / "staged-measurements.site_a.json").write_text(
                json.dumps(staged), encoding="utf-8")

        profile = [_asset("id-1", "images/pack/a.png"),
                   _asset("id-2", "images/pack/b.png",
                          field_values={"have": "yes"})]
        up.apply_replacements(profile, reps)
        posts = [{"id": "post-1", "asset_ids": ["id-1", "id-2"],
                  "created_at": "2025-01-01T00:00:00Z",
                  "updated_at": "2025-01-01T00:00:00Z"}]
        # Written the way the script writes, so a run that changes
        # nothing rewrites byte-identical files and the tests can say
        # "unchanged" by comparing bytes.
        up.dump(td / "assets.json", profile)
        up.dump(td / "posts.json", posts)
        return upgrades

    def _run(self, td: Path, upgrades: Path, *extra: str):
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
             "--site", "site_a", "--upgrades", str(upgrades),
             "--profile", str(td / "assets.json"),
             "--posts", str(td / "posts.json"), *extra],
            capture_output=True, text=True)

    @staticmethod
    def _bytes(td: Path) -> tuple[bytes, bytes]:
        return ((td / "assets.json").read_bytes(),
                (td / "posts.json").read_bytes())

    @staticmethod
    def _profile(td: Path) -> dict[str, dict]:
        return {a["id"]: a for a in json.loads(
            (td / "assets.json").read_text(encoding="utf-8"))}

    KNOWN_FILL_NEW = {"id": "id-2", "field_values": {"added": "from-share"}}
    KNOWN_FILL_NOOP = {"id": "id-2", "field_values": {"have": "yes"}}
    GHOST_FILL = {"id": "ghost", "field_values": {"k": "v"}}
    KNOWN_STAGED_NEW = {"id": "id-2", "file_path": "images/pack/b.png",
                        "bytes": 700, "origin_bytes": 605}
    KNOWN_STAGED_NOOP = {"id": "id-2", "file_path": "images/pack/b.png",
                         "bytes": 605}
    GHOST_STAGED = {"id": "ghost", "file_path": "videos/x.webm", "bytes": 1}

    # -- reconcile: report, keep going --------------------------------

    def test_reconcile_names_an_unknown_id_and_the_run_still_writes(self):
        """One unknown among known. The known entry applies exactly as
        before, the run exits 0 and writes, and the unknown id is on a
        summary line beside the reconcile count."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, reconcile={
                "fill": [self.KNOWN_FILL_NEW, self.GHOST_FILL]})
            r = self._run(td, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("1 value(s) filled from the share", r.stderr)
            self.assertIn("ghost", r.stderr,
                          "a fill entry for a record the profile does not "
                          f"hold was skipped without a word:\n{r.stderr}")
            self.assertIn("1 fill entry(ies) name an id the profile does "
                          "not hold", r.stderr)
            self.assertNotIn("PROBLEM", r.stderr)
            self.assertEqual(self._profile(td)["id-2"]["field_values"],
                             {"have": "yes", "added": "from-share"})

    def test_reconcile_check_is_not_clean_over_an_unknown_id(self):
        """Everything else is applied, so every "would change" term is
        zero. The check must still fail, name the id, and NOT tell the
        reader to run without --check, because that would not clear it."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, reconcile={
                "fill": [self.KNOWN_FILL_NOOP, self.GHOST_FILL]})
            r = self._run(td, upgrades, "--check")
            self.assertEqual(r.returncode, 1,
                             "--check called a profile clean while the "
                             "reconcile document names a record it does "
                             f"not hold:\n{r.stderr}")
            self.assertNotIn("OK: profile already reflects the upgrade",
                             r.stderr)
            self.assertIn("ghost", r.stderr)
            self.assertNotIn("would change it", r.stderr)
            self.assertNotIn("Run without --check.", r.stderr)

    # -- staged: refuse -----------------------------------------------

    def test_staged_an_unknown_id_is_a_problem_and_nothing_is_written(self):
        """One unknown among known. The known entry WOULD correct a byte
        count, which is exactly why the write has to be refused: the
        document was measured against some other profile."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, staged=[self.KNOWN_STAGED_NEW,
                                              self.GHOST_STAGED])
            before = self._bytes(td)
            r = self._run(td, upgrades)
            self.assertEqual(r.returncode, 1,
                             "a staged measurement for a record the profile "
                             "does not hold was applied around in silence:\n"
                             + r.stderr)
            self.assertIn("PROBLEM(S)", r.stderr)
            self.assertIn("staged measurement ghost", r.stderr)
            self.assertIn("measure_staged.py emit", r.stderr)
            self.assertEqual(self._bytes(td), before,
                             "the profiles were written despite the refusal")

    def test_staged_check_fails_and_names_the_id(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, staged=[self.KNOWN_STAGED_NOOP,
                                              self.GHOST_STAGED])
            r = self._run(td, upgrades, "--check")
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertIn("ghost", r.stderr)
            self.assertNotIn("OK: profile already reflects the upgrade",
                             r.stderr)

    # -- every entry unknown --------------------------------------------

    def test_all_entries_unknown(self):
        """The degenerate document. Reconcile applies nothing and names
        every id; staged refuses and lists every id. Both modes."""
        ghosts_fill = [{"id": f"ghost-{i}", "field_values": {"k": "v"}}
                       for i in range(3)]
        ghosts_staged = [{"id": f"ghost-{i}", "file_path": f"v/{i}.webm",
                          "bytes": 1} for i in range(3)]
        names = [g["id"] for g in ghosts_fill]

        with self.subTest("reconcile, normal run"), \
                tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, reconcile={"fill": ghosts_fill})
            r = self._run(td, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("0 value(s) filled from the share", r.stderr)
            self.assertIn("3 fill entry(ies) name an id the profile does "
                          "not hold", r.stderr)
            for n in names:
                self.assertIn(n, r.stderr)
            self.assertEqual(self._profile(td)["id-2"]["field_values"],
                             {"have": "yes"})

        with self.subTest("reconcile, --check"), \
                tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, reconcile={"fill": ghosts_fill})
            r = self._run(td, upgrades, "--check")
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertNotIn("OK: profile already reflects", r.stderr)
            for n in names:
                self.assertIn(n, r.stderr)

        with self.subTest("staged, normal run"), \
                tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, staged=ghosts_staged)
            before = self._bytes(td)
            r = self._run(td, upgrades)
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertIn("3 PROBLEM(S)", r.stderr)
            for n in names:
                self.assertIn(f"staged measurement {n}", r.stderr)
            self.assertEqual(self._bytes(td), before)

        with self.subTest("staged, --check"), \
                tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self._site(td, staged=ghosts_staged)
            r = self._run(td, upgrades, "--check")
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertNotIn("OK: profile already reflects", r.stderr)
            for n in names:
                self.assertIn(n, r.stderr)

    # -- helper level: the channel itself -----------------------------

    def test_the_staged_pass_returns_unknown_ids_beside_its_counts(self):
        profile = [_asset("a", "images/a.png")]
        corrected, hashed, unknown = up.apply_staged_measurements(
            profile, [{"id": "a", "file_path": "images/a.png", "bytes": 9},
                      {"id": "ghost", "file_path": "x", "bytes": 1}])
        self.assertEqual((corrected, hashed, unknown), (1, 0, ["ghost"]))
        self.assertEqual(profile[0]["file_size_bytes"], 9)


class TestUnknownIdReportingLeavesTheCleanPathAlone(unittest.TestCase):
    """#1328, the other half of "seen to refuse": documents that name
    only records the profile holds behave exactly as before. Zero
    entries print no line about unknowns; a known id with nothing to do
    is a byte-identical no-op; several known entries all apply."""

    def setUp(self):
        self.base = TestUnknownIdsAreNotSkippedInSilence()

    def test_zero_entry_documents_print_no_unknown_line(self):
        for extra in ((), ("--check",)):
            with self.subTest(extra=extra), \
                    tempfile.TemporaryDirectory() as t:
                td = Path(t)
                upgrades = self.base._site(td, reconcile={"fill": []},
                                           staged=[])
                r = self.base._run(td, upgrades, *extra)
                self.assertEqual(r.returncode, 0, r.stderr)
                self.assertNotIn("does not hold", r.stderr)
                self.assertNotIn("#1328", r.stderr)
                if extra:
                    self.assertIn("OK: profile already reflects", r.stderr)

    def test_known_ids_with_nothing_to_do_are_a_no_op(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self.base._site(
                td, reconcile={"fill": [self.base.KNOWN_FILL_NOOP]},
                staged=[self.base.KNOWN_STAGED_NOOP])
            before = self.base._bytes(td)
            r = self.base._run(td, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("0 value(s) filled from the share", r.stderr)
            self.assertIn("0 record(s) re-pointed", r.stderr)
            self.assertNotIn("does not hold", r.stderr)
            self.assertEqual(self.base._bytes(td), before)
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self.base._site(
                td, reconcile={"fill": [self.base.KNOWN_FILL_NOOP]},
                staged=[self.base.KNOWN_STAGED_NOOP])
            r = self.base._run(td, upgrades, "--check")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("OK: profile already reflects", r.stderr)

    def test_every_known_entry_applies_when_none_is_unknown(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            upgrades = self.base._site(
                td,
                reconcile={"fill": [
                    {"id": "id-1", "description": "from the share"},
                    self.base.KNOWN_FILL_NEW]},
                staged=[self.base.KNOWN_STAGED_NEW,
                        {"id": "id-1", "file_path": self.base.HQ,
                         "bytes": 8400, "sha256": "ab" * 32}])
            r = self.base._run(td, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("2 value(s) filled from the share", r.stderr)
            self.assertIn("1 record(s) re-pointed at the bytes the site "
                          "ships, 1 hash(es) recorded", r.stderr)
            self.assertNotIn("does not hold", r.stderr)
            written = self.base._profile(td)
            self.assertEqual(written["id-1"]["description"], "from the share")
            self.assertEqual(written["id-1"]["metadata"]["sha256"], "ab" * 32)
            self.assertEqual(written["id-2"]["file_size_bytes"], 700)
            self.assertEqual(written["id-2"]["field_values"]["added"],
                             "from-share")


class TestShippedUpgradeDocumentsNameOnlyHeldRecords(unittest.TestCase):
    """#1328, the corpus guard. Measured 2026-09-15 there are zero
    unknown ids in any of the four documents, and these tests keep it
    that way: they are not evidence the old code was broken, they stop
    a latent condition from becoming permanent noise once it can be
    seen. Shape follows the curation guard above."""

    SITES = (("site_a", "studio-a"), ("site_b", "studio-b"))

    def _held(self, studio: str) -> set[str]:
        return {a["id"] for a in json.loads(
            (PROFILES / f"{studio}.assets.json").read_text(encoding="utf-8"))}

    def test_every_reconcile_fill_id_is_held_by_the_committed_profile(self):
        for site, studio in self.SITES:
            with self.subTest(site=site):
                doc = json.loads((UPGRADES / f"manifest-reconcile.{site}.json")
                                 .read_text(encoding="utf-8"))
                have = self._held(studio)
                # ⛔ RETIREMENT-AWARE (#1319). The reconcile document is
                # historical and is not rewritten when a record retires,
                # so its entry for a retired id outlives the record. The
                # exemption is exactly the documented set: a mistyped id
                # still fails here, which is what #1328 is about.
                retired = _documented_retired_ids(studio)
                missing = [e["id"] for e in doc.get("fill", ())
                           if e["id"] not in have and e["id"] not in retired]
                self.assertEqual(missing, [])

    def test_every_staged_measurement_id_is_held_by_the_committed_profile(self):
        for site, studio in self.SITES:
            with self.subTest(site=site):
                doc = json.loads(
                    (UPGRADES / f"staged-measurements.{site}.json")
                    .read_text(encoding="utf-8"))
                have = self._held(studio)
                missing = [e["id"] for e in doc if e["id"] not in have]
                self.assertEqual(missing, [])


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
        retired = _documented_retired_ids("studio-a")
        for entry in doc["fill"]:
            target = profile.get(entry["id"])
            if target is None and entry["id"] in retired:
                # The document is historical and keeps its entry for a
                # record an asset-collapse document retired (#1319). It
                # fills nothing, so the add-only property is trivially
                # held; an id that is NOT documented still fails below.
                continue
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
        # 2,005 reconciled from the share (#1275) + 2 authored plates
        # (#1290), less the 1 record retired onto its survivor because it
        # could never materialize beside it (#1319, ADR 0097).
        self.assertEqual(len(assets), 2006)
        self.assertEqual(len(_documented_retired_ids("studio-a")), 1)

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


class TestAssemblyDeterminism(unittest.TestCase):
    """#1296 — assembly must be a pure function of its inputs.

    ADR 0098: "Re-running assembly over unchanged inputs produces an
    unchanged profile." No test asserted that, which is why 840 posts
    reached a published dataset carrying timestamps the profile does not
    agree with.

    ⛔ Read the four tests as a ladder. The first one passes on the buggy
    code too: `derive_posts` seeded its RNG, so it always reproduced over
    a byte-identical pool in a byte-identical ORDER. The bug lived one
    rung up — the seeded stream made every draw depend on the position of
    every draw before it, so re-ordering the pool, or changing one asset
    in it, re-sequenced posts that had nothing to do with the change.
    `test_input_order_does_not_change_the_output` and
    `test_a_local_change_stays_local` are the two that fail on `dev`.
    """

    @staticmethod
    def _asset(aid, *, team="Characters", project="Aurora",
               atype="image", created="2025-06-01T00:00:00Z",
               updated="2025-06-02T00:00:00Z", group=None, studio="a"):
        return sa.AssetRecord(
            id=aid, asset_type=atype, title=f"Asset {aid}",
            description=f"Description for {aid}",
            file_path=f"{atype}s/{aid}.bin", source_path=f"src/{aid}.bin",
            source_root="local", file_extension="bin", file_size_bytes=1024,
            sensitivity_tier="team", archive_state="active",
            owner_username=f"owner-{team.lower().replace(' ', '-')}",
            collection_name=project, team_name=team, brand_workspace=None,
            tags=[atype, team.lower()], workflow_state="approved",
            metadata=({"group_id": group} if group else {}),
            field_values={"rating": 3}, external_id=f"ext-{aid}",
            review_notes=None, reviewer_username=None,
            created_at=created, updated_at=updated, last_reviewed_at=None,
            license="CC0 1.0", attribution="synthetic", layer="A",
            studio=studio,
        )

    def _pool(self):
        """Three teams across two projects, big enough for every pass to
        fire: group sets, loose bundles, solos, roundups and sprints."""
        pool = []
        n = 0
        for team in ("Characters", "Environments", "Tech Art"):
            for project in ("Aurora", "Borealis"):
                for i in range(9):
                    n += 1
                    pool.append(self._asset(
                        f"a{n:04d}", team=team, project=project,
                        atype=("image" if i % 2 else "3d"),
                        created=f"2025-0{1 + (i % 8)}-1{i % 9}T0{i % 9}:00:00Z",
                        updated=f"2025-09-1{i % 9}T0{i % 9}:00:00Z",
                        # Two of every nine are siblings, so pass 1 fires.
                        group=(f"grp-{team}-{project}" if i < 2 else None),
                    ))
        return pool

    @staticmethod
    def _dump(posts):
        return json.dumps(posts, sort_keys=True, ensure_ascii=False)

    def test_two_assemblies_of_one_input_are_byte_identical(self):
        """The rung that already passed. Kept because it is the property
        being claimed, and because it is what fails first if someone
        reaches for `datetime.now()` or an unseeded `random`."""
        pool = self._pool()
        self.assertEqual(self._dump(sa.derive_posts(pool)),
                         self._dump(sa.derive_posts(pool)))

    def test_assembly_does_not_mutate_its_input(self):
        """A pass that sorted the caller's list in place, or edited a
        record, would make the SECOND run see a different pool — the
        first run's output would then be unreproducible from the file it
        came from, which is the shape of a Heisenbug nobody enjoys."""
        pool = self._pool()
        before = [dataclasses.asdict(a) for a in pool]
        sa.derive_posts(pool)
        self.assertEqual([dataclasses.asdict(a) for a in pool], before)

    def test_input_order_does_not_change_the_output(self):
        """⛔ THE ONE THAT FAILS ON dev.

        `by_team` / `by_project` are insertion-ordered by the asset list,
        and the old code fed those orders into `rng.shuffle` and
        `rng.sample`. So the same assets, listed in a different order,
        produced different posts — a profile whose content depended on
        the order rows happened to sit in on disk.
        """
        pool = self._pool()
        reordered = list(reversed(pool))
        self.assertEqual(self._dump(sa.derive_posts(pool)),
                         self._dump(sa.derive_posts(reordered)))

    def test_a_local_change_stays_local(self):
        """⛔ THE OTHER ONE THAT FAILS ON dev, and the real defect.

        Drop one asset from one team. Every post that neither contained
        it nor belongs to that team must come through untouched — id,
        membership and timestamp.

        Measured on site_a before the fix, dropping a single audio asset
        moved 65 post ids of which ONE contained the dropped asset: 26
        team roundups, 28 project sprints and 4 showreels belonging to
        other teams and other projects moved because a seeded RNG had
        re-sequenced.
        """
        pool = self._pool()
        victim = next(a for a in pool if a.team_name == "Tech Art")
        reduced = [a for a in pool if a.id != victim.id]

        before = {p["id"]: p for p in sa.derive_posts(pool)}
        after = {p["id"]: p for p in sa.derive_posts(reduced)}

        def untouchable(post):
            return (victim.id not in post["asset_ids"]
                    and post["team_name"] != victim.team_name
                    and post["collection_name"] != victim.collection_name)

        for pid, post in before.items():
            if not untouchable(post):
                continue
            self.assertIn(pid, after,
                          f"{post['post_kind']} {post['title']!r} vanished, "
                          f"and it has nothing to do with the dropped asset")
            self.assertEqual(post, after[pid],
                             f"{post['post_kind']} {post['title']!r} changed, "
                             f"and it has nothing to do with the dropped asset")

    def test_a_team_with_fewer_than_five_assets_gets_no_roundup(self):
        """The empty case. Whatever replaces the sampling must keep
        refusing to build a roundup out of four assets."""
        pool = [a for a in self._pool() if a.team_name != "Tech Art"]
        pool += [self._asset(f"tiny{i}", team="Tiny", project="Aurora")
                 for i in range(4)]
        posts = sa.derive_posts(pool)
        self.assertEqual(
            [p for p in posts
             if p["post_kind"] == "team_roundup" and p["team_name"] == "Tiny"],
            [], "a four-asset team produced a roundup")

    def test_every_post_carries_the_canonical_key_set(self):
        """`_post`'s docstring claims every post is assembled in one
        place with exactly these keys. Three passes used to build their
        dict inline and bypass it, so the claim was only true of the
        passes that happened to call it."""
        posts = sa.derive_posts(self._pool())
        self.assertTrue(posts)
        expected = set(posts[0])
        self.assertEqual(len(expected), 18, sorted(expected))
        for p in posts:
            self.assertEqual(set(p), expected, p.get("post_kind"))

    def test_member_lists_are_in_a_stable_order(self):
        """`rng.sample` returned DRAW order. 388 of the 861 posts shared
        between the repo profile and the published site differ in
        `asset_ids` while only 5 differ in membership as a set — that gap
        is pure ordering churn."""
        for p in sa.derive_posts(self._pool()):
            if p["post_kind"] in ("team_roundup", "project_sprint",
                                  "cinematics_showreel"):
                self.assertEqual(p["asset_ids"], sorted(p["asset_ids"]),
                                 p["title"])


class TestProfileReadBack(unittest.TestCase):
    """#1296 — the assembler could not read the profiles it writes.

    `--recompose-posts` is the only path that re-derives posts without
    the 12,871-row source CSV, and it did this on every current profile:

        TypeError: AssetRecord.__init__() got an unexpected keyword
                   argument 'replaced_source_path'

    because `apply_upgrade` and `studio_balance` annotate the profile
    after assembly. So post composition could not be regenerated at all,
    and every repair since had to be made to the DATA by hand.
    """

    def test_the_committed_profiles_load(self):
        for name in ("studio-a", "studio-b"):
            raw = json.loads((PROFILES / f"{name}.assets.json").read_text())
            records = sa.asset_records_from_profile(raw, f"{name}.assets.json")
            self.assertEqual(len(records), len(raw), name)

    def test_a_post_assembly_annotation_is_accepted(self):
        raw = [{**dataclasses.asdict(TestAssemblyDeterminism._asset("a1")),
                "replaced_source_path": "old/path.png",
                "ai_provenance": "none",
                "balance_source": "kenney"}]
        records = sa.asset_records_from_profile(raw, "synthetic")
        self.assertEqual(records[0].id, "a1")

    def test_a_key_nobody_declared_is_refused_by_name(self):
        """Not silently dropped. A filter would swallow a typo — and a
        genuinely new field — with the same shrug."""
        raw = [{**dataclasses.asdict(TestAssemblyDeterminism._asset("a1")),
                "raplaced_source_path": "typo"}]
        with self.assertRaises(ValueError) as ctx:
            sa.asset_records_from_profile(raw, "synthetic")
        self.assertIn("raplaced_source_path", str(ctx.exception))

    def test_the_annotations_named_in_the_allow_list_are_the_ones_on_disk(self):
        """Guards the guard: if a tool starts writing a fourth
        annotation, this is where it gets noticed."""
        known = {f.name for f in dataclasses.fields(sa.AssetRecord)}
        seen = set()
        for name in ("studio-a", "studio-b"):
            for entry in json.loads((PROFILES / f"{name}.assets.json").read_text()):
                seen |= set(entry) - known
        self.assertEqual(seen, set(sa.POST_ASSEMBLY_KEYS) & seen)
        self.assertTrue(seen, "the profiles carry no annotations at all")


class TestRecomposeRefusesToOverwriteTheCorpus(unittest.TestCase):
    """#1322. `--recompose-posts` replaced the authoritative corpus.

    ADR 0098's ruling: `seed/profiles/*.posts.json` is the corpus the
    project ships and is not reproducible from the asset profiles, since
    `group_id` never left the source catalogue. Yet the command wrote
    straight over `studio-a.posts.json`, `studio-b.posts.json`,
    `dataset.posts.json` and every `--site` root's `posts.json`, with no
    existence check: 1,103 posts for 863, 336 ids kept, 200 curated posts
    left without a row. And its writes were interleaved per studio, so a
    refusal decided mid-loop would have rewritten one studio anyway.

    The guard is all-or-nothing and decided before the first write. Every
    case here drives `main()` with argv against temporary copies of the
    committed profiles; nothing touches `seed/profiles` or a site root.
    """

    POSTS = ("studio-a.posts.json", "studio-b.posts.json",
             "dataset.posts.json")

    @staticmethod
    def _sha(path: Path) -> str:
        return hashlib.sha256(path.read_bytes()).hexdigest()

    def _out(self, td: Path, *posts: str) -> Path:
        out = td / "profiles"
        out.mkdir()
        for name in ("studio-a.assets.json", "studio-b.assets.json", *posts):
            (out / name).write_bytes((PROFILES / name).read_bytes())
        return out

    def _run(self, *argv: str) -> tuple[int, str]:
        err = io.StringIO()
        with unittest.mock.patch.object(
                sys, "argv", ["sanitize_and_assemble.py", *argv]), \
                contextlib.redirect_stderr(err):
            rc = sa.main()
        return rc, err.getvalue()

    def test_the_committed_layout_is_refused_and_every_profile_named(self):
        with tempfile.TemporaryDirectory() as t:
            out = self._out(Path(t), *self.POSTS)
            before = {n: self._sha(out / n) for n in self.POSTS}
            rc, err = self._run("--recompose-posts", "--out", str(out))
            self.assertNotEqual(
                rc, 0, "a recompose over the committed layout exited 0")
            for n in self.POSTS:
                self.assertIn(str(out / n), err,
                              f"the refusal did not name {n}:\n{err}")
                self.assertEqual(self._sha(out / n), before[n],
                                 f"{n} was rewritten")

    def test_one_existing_target_alone_refuses_the_whole_run(self):
        """Any one of the three, on its own. The other two must still be
        absent afterwards: a refusal that has already created them is
        a partial run, not a refusal."""
        for present in self.POSTS:
            with self.subTest(present=present), \
                    tempfile.TemporaryDirectory() as t:
                out = self._out(Path(t), present)
                before = self._sha(out / present)
                rc, err = self._run("--recompose-posts", "--out", str(out))
                self.assertEqual(rc, 2, err)
                self.assertIn(str(out / present), err)
                self.assertEqual(self._sha(out / present), before,
                                 f"{present} was rewritten")
                for other in self.POSTS:
                    if other != present:
                        self.assertFalse((out / other).exists(),
                                         f"{other} was created")

    def test_a_site_root_posts_json_alone_refuses_and_creates_no_profile(self):
        """The fourth protected target is not under --out at all: it is
        `posts.json` in a --site root whose basename names a studio, the
        copy the Go seeder actually reads."""
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            out = self._out(td)
            site = td / "site_a"
            site.mkdir()
            target = site / "posts.json"
            target.write_bytes((PROFILES / "studio-a.posts.json").read_bytes())
            before = self._sha(target)
            rc, err = self._run("--recompose-posts", "--out", str(out),
                                "--site", str(site))
            self.assertEqual(rc, 2, err)
            self.assertIn(str(target), err)
            self.assertEqual(self._sha(target), before,
                             "the site root's posts.json was overwritten")
            for n in self.POSTS:
                self.assertFalse((out / n).exists(), f"{n} was created")

    def test_dry_run_reports_the_refusal_and_exits_the_same_way(self):
        """populate_archive.py's rule, applied here: a dry run that passes
        while a real run would be refused is worse than no dry run."""
        with tempfile.TemporaryDirectory() as t:
            out = self._out(Path(t), *self.POSTS)
            before = {n: self._sha(out / n) for n in self.POSTS}
            rc, err = self._run("--recompose-posts", "--out", str(out),
                                "--dry-run")
            self.assertEqual(rc, 2,
                             "the dry run exited differently from the "
                             f"refused real run:\n{err}")
            for n in self.POSTS:
                self.assertIn(str(out / n), err)
                self.assertEqual(self._sha(out / n), before[n])


class TestRecomposeIntoAnEmptyDirectoryStillWorks(unittest.TestCase):
    """#1322, the legitimate use. A directory holding only the asset
    profiles is what a rebuild is for, and the guard must not touch it:
    the three posts profiles are written, and so is `posts.json` in a
    --site root that names a studio, while a root that names no studio
    gets nothing."""

    def test_an_empty_output_directory_is_composed_and_written(self):
        with tempfile.TemporaryDirectory() as t:
            td = Path(t)
            out = td / "profiles"
            out.mkdir()
            for name in ("studio-a.assets.json", "studio-b.assets.json"):
                (out / name).write_bytes((PROFILES / name).read_bytes())
            site_a = td / "site_a"
            site_a.mkdir()
            other = td / "not-a-studio"
            other.mkdir()
            err = io.StringIO()
            with unittest.mock.patch.object(
                    sys, "argv", ["sanitize_and_assemble.py",
                                  "--recompose-posts", "--out", str(out),
                                  "--site", str(site_a),
                                  "--site", str(other)]), \
                    contextlib.redirect_stderr(err):
                rc = sa.main()
            self.assertEqual(rc, 0, err.getvalue())
            for n in TestRecomposeRefusesToOverwriteTheCorpus.POSTS:
                self.assertTrue((out / n).is_file(), f"{n} was not written")
            self.assertTrue((site_a / "posts.json").is_file())
            self.assertFalse((other / "posts.json").exists())
            site_posts = json.loads(
                (site_a / "posts.json").read_text(encoding="utf-8"))
            studio_posts = json.loads(
                (out / "studio-a.posts.json").read_text(encoding="utf-8"))
            self.assertEqual(site_posts, studio_posts)


class TestPostIdentity(unittest.TestCase):
    """#1293 — a derived id is a function of what identifies the post.

    team_roundup, project_sprint and cinematics_showreel keyed their id
    on the sample's ANCHOR: the most-recent member. An anchor accompanies
    a post, it does not identify one — a team's genuinely most-recent
    asset lands in many samples, so several roundups with different
    membership derived the same id.

    ⛔ And a collision here is a DISAPPEARANCE, not a duplicate. `aa seed`
    keys on the stable id, so of n colliding rows exactly one is ever
    seeded: sixteen roundup posts existed in the catalogue and could
    never reach a database.
    """

    def test_two_member_sets_sharing_an_anchor_get_different_ids(self):
        """The exact case that collided. Two roundups for one team,
        overlapping membership, the same most-recent asset in both."""
        anchor = "a-newest"
        first = [anchor, "a-002", "a-003", "a-004", "a-005"]
        second = [anchor, "a-002", "a-003", "a-004", "a-005", "a-006"]
        self.assertNotEqual(sa.roundup_post_id("Tech Art", first),
                            sa.roundup_post_id("Tech Art", second),
                            "two roundups with different membership share an id")

    def test_the_same_membership_gives_the_same_id(self):
        """The other half of identity: order must not matter, because
        the same post read back from disk is the same post."""
        members = ["a-003", "a-001", "a-002"]
        self.assertEqual(sa.roundup_post_id("Tech Art", members),
                         sa.roundup_post_id("Tech Art", reversed(members)))

    def test_two_teams_with_one_membership_get_different_ids(self):
        members = ["a-001", "a-002"]
        self.assertNotEqual(sa.roundup_post_id("Tech Art", members),
                            sa.roundup_post_id("VFX", members))

    def test_the_sprint_label_still_separates_identical_samples(self):
        """A project holding exactly five assets yields the same sample
        on every sweep, so the label is the only thing left telling
        those posts apart. Dropping it from the key would reintroduce
        the collision in the one case it is guaranteed to happen."""
        members = ["a-001", "a-002", "a-003", "a-004", "a-005"]
        self.assertNotEqual(sa.sprint_post_id("Aurora", "sprint 12", members),
                            sa.sprint_post_id("Aurora", "ship gate", members))

    def test_a_real_assembly_emits_no_colliding_ids(self):
        """End to end, on a pool big enough for several roundups per
        team. Before the fix this assembly collides."""
        pool = TestAssemblyDeterminism()._pool()
        ids = [p["id"] for p in sa.derive_posts(pool)]
        dupes = sorted({i for i in ids if ids.count(i) > 1})
        self.assertEqual(dupes, [], f"{len(dupes)} colliding id(s)")

    def test_the_committed_profiles_hold_only_derived_ids(self):
        """⭐ The permanent invariant, on the real data. `plan()` returns
        every row whose id does not equal the id its own content derives;
        after the migration that list is empty, and it stays empty unless
        someone edits a post's membership without re-deriving its id."""
        for name in ("studio-a", "studio-b", "dataset"):
            posts = json.loads((PROFILES / f"{name}.posts.json").read_text())
            moves = mpi.plan(posts)
            self.assertEqual(
                [(m["post_kind"], m["title"]) for m in moves], [],
                f"{name}.posts.json holds {len(moves)} id(s) that do not "
                f"derive from the post they name — run "
                f"seed/scripts/migrate_post_ids.py")

    def test_the_migration_documents_match_the_profiles(self):
        """The reconcile artifact. Every new id it names is in the
        profile and every old id is gone, so a publish that finds the
        destination holding ids the source no longer has can tell a
        migration from a loss (ADR 0097)."""
        for name in ("studio-a", "studio-b", "dataset"):
            doc = json.loads((UPGRADES / f"post-id-migration.{name}.json").read_text())
            self.assertTrue(doc.get("_why"), f"{name}: the document must say why")
            self.assertEqual(doc["profile"], f"{name}.posts.json")
            posts = json.loads((PROFILES / f"{name}.posts.json").read_text())
            live = {p["id"] for p in posts}
            moves = doc["moves"]
            self.assertTrue(moves, f"{name}: an empty mapping documents nothing")
            self.assertEqual(len({m["old_id"] for m in moves}), len(moves),
                             f"{name}: an old id is listed twice")
            self.assertEqual([m for m in moves if m["new_id"] not in live], [],
                             f"{name}: the document names a new id the profile "
                             f"does not hold")
            self.assertEqual([m for m in moves if m["old_id"] in live], [],
                             f"{name}: the document names an old id that is "
                             f"still in the profile")

    def test_an_unrecoverable_label_is_refused_rather_than_guessed(self):
        """A row whose key cannot be recovered stops the migration. A
        tool that skipped what it did not understand would leave the
        profile in two id schemes at once."""
        post = {"id": "x", "post_kind": "project_sprint", "asset_ids": ["a1"],
                "collection_name": "Aurora",
                "title": "Aurora quarterly wash-up — 1 assets across 1 team(s)"}
        with self.assertRaises(mpi.Unresolvable):
            mpi.derived_id(post)

    def test_a_migration_that_would_still_collide_is_refused(self):
        """`check_safe` is what stands between a migration and swapping
        one collision for another."""
        posts = [
            {"id": "old-1", "post_kind": "team_roundup", "team_name": "T",
             "asset_ids": ["a", "b"], "title": "T sprint roundup — 2 drops"},
            {"id": "old-2", "post_kind": "team_roundup", "team_name": "T",
             "asset_ids": ["b", "a"], "title": "T sprint roundup — 2 drops"},
        ]
        moves = mpi.plan(posts)
        self.assertEqual(len(moves), 2)
        self.assertTrue(mpi.check_safe(posts, moves),
                        "two rows deriving one id must be refused")


# ---------------------------------------------------------------------------
# #1319: a committed identity migration is not a deletion (ADR 0097)
# ---------------------------------------------------------------------------

def _post(pid, **kw):
    """A posts.json row with the keys the guard descends into."""
    # The title is NOT derived from the id: a moved record must be
    # identical apart from its identity, or the fixture would carry an
    # edit of its own.
    base = {"id": pid, "title": "a post", "post_kind": "asset_group",
            "asset_ids": ["a1"], "tags": ["t"], "field_values": {}, "metadata": {}}
    base.update(kw)
    return base


def _moves(*pairs):
    return [{"old_id": o, "new_id": n, "post_kind": "asset_group",
             "title": f"post {o}", "members": 1} for o, n in pairs]


def _publish_dry_run(tmp, source_posts, dest_posts, document, *, stem="fixture",
                     extra_args=(), profile_field=None, write_document=True):
    """Lay a posts profile and its migration document out the way the
    repository holds them (`<root>/profiles/<stem>.posts.json` beside
    `<root>/upgrades/post-id-migration.<stem>.json`) and run
    populate_archive.py --dry-run over an empty assets profile, so the
    only thing under test is the posts.json guard.

    Only arguments that exist on `dev` before #1319 are used, which is
    what lets the class-A tests below run UNCHANGED against the old code
    and fail there for the old reason.
    """
    tmp = Path(tmp)
    local = tmp / "local"
    local.mkdir(exist_ok=True)
    (local / "metadata.csv").write_text("file_path,title\n", encoding="utf-8")
    (tmp / "internet").mkdir(exist_ok=True)
    profiles = tmp / "profiles"
    profiles.mkdir(exist_ok=True)
    upgrades = tmp / "upgrades"
    upgrades.mkdir(exist_ok=True)
    dest = tmp / "dest"
    dest.mkdir(exist_ok=True)
    assets = profiles / f"{stem}.assets.json"
    assets.write_text("[]", encoding="utf-8")
    (dest / "MANIFEST.json").write_text("[]", encoding="utf-8")
    posts = profiles / f"{stem}.posts.json"
    posts.write_text(json.dumps(source_posts), encoding="utf-8")
    (dest / "posts.json").write_text(json.dumps(dest_posts), encoding="utf-8")
    if write_document and document is not None:
        doc = {"_why": ["fixture"],
               "profile": profile_field if profile_field is not None else posts.name,
               "moves": document}
        (upgrades / f"post-id-migration.{stem}.json").write_text(
            json.dumps(doc), encoding="utf-8")
    proc = subprocess.run(
        [sys.executable, str(SCRIPTS / "populate_archive.py"),
         "--local-source", str(local), "--internet-source", str(tmp / "internet"),
         "--profile", str(assets), "--posts", str(posts), "--dest", str(dest),
         "--dry-run", *extra_args],
        capture_output=True, text=True)
    return proc


class TestMigrationAwareGuardCLI(unittest.TestCase):
    """Class A, fail-before-fix (#1319). Run UNCHANGED against `dev` at
    02739f6b these refuse with "WOULD LOSE: N record(s) deleted" because
    the old guard reads every moved id as a deleted record; on the fixed
    code the same invocation passes and reports N migrations.

    The document is laid out where the fixed lookup expects it and the
    invocation is the production one, `--posts <profile> --dry-run`, with
    no new argument.
    """

    def test_an_identity_only_move_is_a_migration_not_a_loss(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(d, [_post("A2")], [_post("A")], _moves(("A", "A2")))
        self.assertEqual(proc.returncode, 0,
                         "an id the migration document moved was refused as a "
                         f"deleted record:\n{proc.stderr}")
        self.assertIn("migrated: 1 record(s)", proc.stderr)
        self.assertNotIn("WOULD LOSE", proc.stderr)
        self.assertNotIn("would add", proc.stderr,
                         "the new id is the old record under a new name, not an addition")
        self.assertNotIn("would change", proc.stderr,
                         "the id transition must not be reported as an edit")
        self.assertIn("nothing at the destination would be lost", proc.stderr)

    def test_two_moves_beside_an_unmoved_record(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(
                d, [_post("A2"), _post("B2"), _post("C")],
                [_post("A"), _post("B"), _post("C")],
                _moves(("A", "A2"), ("B", "B2")))
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("migrated: 2 record(s)", proc.stderr)
        self.assertNotIn("would add", proc.stderr)
        self.assertNotIn("would change", proc.stderr)

    def test_an_unrelated_missing_record_is_still_a_loss(self):
        """D is not in the document, so D is deleted. Refused on both the
        old and the new code; the fixed report additionally accounts for
        the two moves instead of listing them as losses."""
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(
                d, [_post("A2"), _post("B2"), _post("C")],
                [_post("A"), _post("B"), _post("C"), _post("D")],
                _moves(("A", "A2"), ("B", "B2")))
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("MISSING_RECORD D", proc.stderr)
        self.assertIn("migrated: 2 record(s)", proc.stderr,
                      "the two recorded moves must be migrations, not losses")
        self.assertIn("WOULD LOSE: 1 record(s) deleted", proc.stderr)

    def test_content_lost_across_a_move_still_refuses(self):
        """The move is recorded, but the new record lacks a value the old
        one carried. A migration excuses the id and nothing else."""
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(
                d, [_post("A2")], [_post("A", field_values={"k": "v"})],
                _moves(("A", "A2")))
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("MISSING_KEY A .field_values.k", proc.stderr,
                      "the loss must be named as the dropped key, not as a "
                      f"deleted record:\n{proc.stderr}")
        self.assertIn("migrated: 1 record(s)", proc.stderr)
        self.assertNotIn("MISSING_RECORD", proc.stderr)

    def test_a_value_emptied_across_a_move_still_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(
                d, [_post("A2", field_values={"k": ""})],
                [_post("A", field_values={"k": "v"})],
                _moves(("A", "A2")))
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("EMPTIED_VALUE A .field_values.k", proc.stderr)
        self.assertIn("migrated: 1 record(s)", proc.stderr)

    def test_an_ordinary_edit_across_a_move_is_reported_not_refused(self):
        """Title changed, id moved: one CHANGED_VALUE (the title) and no
        change for the id. A count of 2 would mean the id transition was
        reported as an edit."""
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(
                d, [_post("A2", title="new")], [_post("A", title="old")],
                _moves(("A", "A2")))
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("migrated: 1 record(s)", proc.stderr)
        self.assertIn("would change: 1 value(s)", proc.stderr)


class TestMigrationWitnessFromCommittedInputs(unittest.TestCase):
    """Class A, the real-data witness, built at test time from committed
    inputs and no archive share: source = the committed posts profile,
    destination = the same rows with every new id rewritten back to its
    old id per the committed document (and, for studio-b, the four ids
    the published site holds twice appended a second time). On `dev` the
    guard reports 175 / 336 deleted records; fixed, 0 losses and 175 /
    336 migrations, and no identity-key change.
    """

    # The four ids site_b's published posts.json carries twice. Supplied
    # here as the fixture's shape, not read from the share.
    SITE_B_TWICE = (
        "81f57645-32f0-c0d0-e2a1-4eb5af1bfce7",
        "7e6e18ee-4e2b-3109-9334-9e4107576ac1",
        "a08d3d10-4100-a973-74c9-cc09ed94185a",
        "3e74d6e4-50cb-b1ba-952a-c21d5ecb7dd4",
    )

    def _witness(self, stem, expected_moves, twice=()):
        source = json.loads((PROFILES / f"{stem}.posts.json").read_text(encoding="utf-8"))
        doc = json.loads((UPGRADES / f"post-id-migration.{stem}.json").read_text(encoding="utf-8"))
        moves = doc["moves"]
        self.assertEqual(len(moves), expected_moves)
        back = {m["new_id"]: m["old_id"] for m in moves}
        dest = []
        for row in source:
            row = json.loads(json.dumps(row))
            row["id"] = back.get(row["id"], row["id"])
            dest.append(row)
        by_id = {r["id"]: r for r in dest}
        for rid in twice:
            self.assertIn(rid, by_id, f"{rid} is not a {stem} post")
            dest.append(json.loads(json.dumps(by_id[rid])))
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(d, source, dest, moves, stem=stem)
        self.assertEqual(proc.returncode, 0,
                         f"{stem}: {expected_moves} recorded moves were refused as "
                         f"deleted records:\n{proc.stderr[-2000:]}")
        self.assertIn(f"migrated: {expected_moves} record(s)", proc.stderr)
        self.assertNotIn("WOULD LOSE", proc.stderr)
        self.assertNotIn("WOULD OVERWRITE A MEASUREMENT", proc.stderr)
        self.assertNotIn("would add", proc.stderr,
                         "every source id is either shared or a migration target")
        self.assertNotIn("would change", proc.stderr,
                         "rows differ only in id; an identity-key change would show here")
        self.assertIn(f"destination {len(dest):,}", proc.stderr)

    def test_studio_a_175_moves(self):
        self._witness("studio-a", 175)

    def test_studio_b_336_moves_with_the_four_twice_published_ids(self):
        self._witness("studio-b", 336, twice=self.SITE_B_TWICE)

    def test_manifest_verdicts_are_unchanged(self):
        """Class B. The MANIFEST comparison takes no migration and must
        judge exactly as before: one deleted record, one edit."""
        profile = json.loads((PROFILES / "studio-a.assets.json").read_text(encoding="utf-8"))
        dest = json.loads(json.dumps(profile))
        dest[0]["title"] = dest[0]["title"] + " (published)"
        dest.append({**dest[1], "id": "only-at-the-destination"})
        cmp = mg.compare(profile, dest, "MANIFEST.json")
        self.assertEqual([(x.kind, x.record_id) for x in cmp.losses],
                         [(mg.MISSING_RECORD, "only-at-the-destination")])
        self.assertEqual([(x.kind, x.key) for x in cmp.changes], [(mg.CHANGED_VALUE, "title")])
        self.assertFalse(cmp.ok)


class TestMigrationDocumentValidation(unittest.TestCase):
    """Class C, branch-only: the document is validated one-to-one before
    any comparison, and every defect refuses rather than half-applies."""

    def _doc(self, moves, profile="x.posts.json"):
        return {"_why": ["t"], "profile": profile, "moves": moves}

    def test_many_to_one_is_refused_not_two_migrations(self):
        with self.assertRaises(mg.MigrationError) as cm:
            mg.parse_migration_document(self._doc(_moves(("A", "X"), ("B", "X"))),
                                        source="doc", profile_name="x.posts.json")
        msg = str(cm.exception)
        self.assertIn("doc", msg)
        for rid in ("A", "B", "X"):
            self.assertIn(rid, msg)

    def test_conflicting_targets_for_one_old_id_are_refused(self):
        with self.assertRaises(mg.MigrationError) as cm:
            mg.parse_migration_document(self._doc(_moves(("A", "X"), ("A", "Y"))), source="doc")
        self.assertIn("two different targets", str(cm.exception))

    def test_an_old_id_recorded_twice_is_refused(self):
        with self.assertRaises(mg.MigrationError) as cm:
            mg.parse_migration_document(self._doc(_moves(("A", "X"), ("A", "X"))), source="doc")
        self.assertIn("recorded twice", str(cm.exception))

    def test_an_uncomposed_chain_is_refused(self):
        with self.assertRaises(mg.MigrationError) as cm:
            mg.parse_migration_document(self._doc(_moves(("A", "B"), ("B", "C"))), source="doc")
        self.assertIn("uncomposed chain", str(cm.exception))
        self.assertIn("B", str(cm.exception))

    def test_a_self_move_and_a_malformed_move_are_refused(self):
        with self.assertRaises(mg.MigrationError):
            mg.parse_migration_document(self._doc(_moves(("A", "A"))), source="doc")
        with self.assertRaises(mg.MigrationError):
            mg.parse_migration_document(self._doc([{"old_id": "A"}]), source="doc")
        with self.assertRaises(mg.MigrationError):
            mg.parse_migration_document(self._doc([{"old_id": "", "new_id": "B"}]), source="doc")
        with self.assertRaises(mg.MigrationError):
            mg.parse_migration_document({"profile": "x.posts.json", "moves": {}}, source="doc")
        with self.assertRaises(mg.MigrationError):
            mg.parse_migration_document({"profile": "x.posts.json"}, source="doc")
        with self.assertRaises(mg.MigrationError):
            mg.parse_migration_document([], source="doc")

    def test_a_profile_naming_another_file_is_refused(self):
        with self.assertRaises(mg.MigrationError) as cm:
            mg.parse_migration_document(self._doc(_moves(("A", "B")), profile="studio-b.posts.json"),
                                        source="doc", profile_name="studio-a.posts.json")
        self.assertIn("studio-b.posts.json", str(cm.exception))
        self.assertIn("studio-a.posts.json", str(cm.exception))
        with self.assertRaises(mg.MigrationError):
            mg.parse_migration_document({"moves": []}, source="doc")

    def test_a_mapped_new_id_absent_from_the_source_is_refused(self):
        m = mg.parse_migration_document(self._doc(_moves(("A", "Z"))), source="doc")
        with self.assertRaises(mg.MigrationError) as cm:
            mg.compare([_post("B")], [_post("A")], "posts", migration=m)
        self.assertIn("Z", str(cm.exception))
        self.assertIn("absent from the source", str(cm.exception))

    def test_an_old_id_still_in_the_source_is_refused(self):
        m = mg.parse_migration_document(self._doc(_moves(("A", "A2"))), source="doc")
        with self.assertRaises(mg.MigrationError) as cm:
            mg.compare([_post("A"), _post("A2")], [_post("A")], "posts", migration=m)
        self.assertIn("still present in the source", str(cm.exception))

    def test_malformed_json_refuses_naming_the_document(self):
        with tempfile.TemporaryDirectory() as td:
            p = Path(td) / "post-id-migration.x.json"
            p.write_text('{"profile": "x.posts.json", "moves": [', encoding="utf-8")
            with self.assertRaises(mg.MigrationError) as cm:
                mg.load_migration_document(p, profile_name="x.posts.json")
            self.assertIn(p.name, str(cm.exception))

    def test_the_lookup_is_derived_from_the_posts_path(self):
        self.assertEqual(
            mg.migration_document_path(Path("/r/profiles/studio-a.posts.json")),
            Path("/r/upgrades/post-id-migration.studio-a.json"))
        self.assertIsNone(mg.migration_document_path(Path("/r/profiles/posts.json")))
        self.assertIsNone(mg.migration_document_path(Path("/r/profiles/.posts.json")))
        for stem in ("studio-a", "studio-b", "dataset"):
            doc = mg.load_migration_document(
                UPGRADES / f"post-id-migration.{stem}.json", profile_name=f"{stem}.posts.json")
            posts = json.loads((PROFILES / f"{stem}.posts.json").read_text(encoding="utf-8"))
            mg.validate_migration_against_source(doc.moves, (p["id"] for p in posts))

    def test_a_migration_target_is_not_an_addition(self):
        m = mg.parse_migration_document(self._doc(_moves(("A", "A2"))), source="doc")
        cmp = mg.compare([_post("A2"), _post("N")], [_post("A")], "posts", migration=m)
        self.assertEqual(cmp.migrated, [("A", "A2")])
        self.assertEqual(cmp.records_migrated, 1)
        self.assertEqual(cmp.added, ["N"])
        self.assertEqual(cmp.losses, [])
        self.assertTrue(cmp.ok)
        report = mg.format_report(cmp)
        self.assertIn("migrated: 1 record(s)", report)
        self.assertIn("would add: 1 record(s)", report)

    def test_a_plain_mapping_is_accepted_and_a_moved_measurement_keeps_its_class(self):
        """The comparison across a move is the ordinary one: a corrupted
        measurement on a staged root still refuses, an edit still passes."""
        cmp = mg.compare([_post("A2", source_root="site", file_size_bytes=1)],
                         [_post("A", source_root="site", file_size_bytes=2)],
                         "posts", migration={"A": "A2"})
        self.assertEqual([x.kind for x in cmp.losses], [mg.CORRUPTED_MEASUREMENT])
        self.assertEqual(cmp.migrated, [("A", "A2")])

    def test_duplicate_source_ids_stay_refused_with_a_migration(self):
        m = mg.parse_migration_document(self._doc(_moves(("A", "A2"))), source="doc")
        cmp = mg.compare([_post("A2"), _post("A2")], [_post("A")], "posts", migration=m)
        self.assertEqual(cmp.duplicates, {"A2": 2})
        self.assertFalse(cmp.ok)


class TestMigrationAwareGuardCLIRefusals(unittest.TestCase):
    """Class C, branch-only: the CLI wiring. A document that cannot be
    used refuses non-overridably; an absent one is simply no evidence."""

    def test_many_to_one_refuses_through_the_cli_and_no_flag_overrides_it(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(d, [_post("X")], [_post("A"), _post("B")],
                                    _moves(("A", "X"), ("B", "X")),
                                    extra_args=("--allow-regression",))
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("many-to-one", proc.stderr)
        self.assertNotIn("migrated:", proc.stderr)
        self.assertNotIn("publishing anyway", proc.stderr)

    def test_a_profile_mismatch_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(d, [_post("A2")], [_post("A")], _moves(("A", "A2")),
                                    profile_field="studio-b.posts.json")
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("studio-b.posts.json", proc.stderr)
        self.assertIn("Refusing", proc.stderr)

    def test_a_malformed_document_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / "upgrades").mkdir()
            (Path(d) / "upgrades" / "post-id-migration.fixture.json").write_text(
                "{not json", encoding="utf-8")
            proc = _publish_dry_run(d, [_post("A2")], [_post("A")], None)
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("unreadable", proc.stderr)
        self.assertIn("post-id-migration.fixture.json", proc.stderr)

    def test_a_mapped_new_id_absent_from_the_source_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(d, [_post("B")], [_post("A")], _moves(("A", "Z")),
                                    extra_args=("--allow-regression",))
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("absent from the source", proc.stderr)

    def test_an_absent_document_is_no_evidence_and_the_old_id_is_a_loss(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(d, [_post("A2")], [_post("A")], _moves(("A", "A2")),
                                    write_document=False)
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("no migration document at", proc.stderr)
        self.assertIn("MISSING_RECORD A", proc.stderr)

    def test_a_posts_file_not_named_stem_posts_json_gets_no_lookup(self):
        with tempfile.TemporaryDirectory() as d:
            tmp = Path(d)
            (tmp / "local").mkdir()
            (tmp / "local" / "metadata.csv").write_text("file_path,title\n", encoding="utf-8")
            (tmp / "internet").mkdir()
            (tmp / "dest").mkdir()
            (tmp / "dest" / "MANIFEST.json").write_text("[]", encoding="utf-8")
            (tmp / "dest" / "posts.json").write_text(json.dumps([_post("A")]), encoding="utf-8")
            (tmp / "assets.json").write_text("[]", encoding="utf-8")
            (tmp / "posts.json").write_text(json.dumps([_post("A2")]), encoding="utf-8")
            proc = subprocess.run(
                [sys.executable, str(SCRIPTS / "populate_archive.py"),
                 "--local-source", str(tmp / "local"), "--internet-source", str(tmp / "internet"),
                 "--profile", str(tmp / "assets.json"), "--posts", str(tmp / "posts.json"),
                 "--dest", str(tmp / "dest"), "--dry-run"],
                capture_output=True, text=True)
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("no migration document lookup", proc.stderr)

    def test_the_override_reads_a_document_elsewhere_and_still_checks_profile(self):
        """The optional override is for fixtures; it never relaxes the
        `profile` check, and it is not what the class-A witnesses use."""
        with tempfile.TemporaryDirectory() as d:
            doc = Path(d) / "elsewhere.json"
            doc.write_text(json.dumps({"profile": "fixture.posts.json",
                                       "moves": _moves(("A", "A2"))}), encoding="utf-8")
            proc = _publish_dry_run(d, [_post("A2")], [_post("A")], None,
                                    extra_args=("--migration-document", str(doc)))
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertIn("migrated: 1 record(s)", proc.stderr)
            doc.write_text(json.dumps({"profile": "other.posts.json",
                                       "moves": _moves(("A", "A2"))}), encoding="utf-8")
            proc = _publish_dry_run(d, [_post("A2")], [_post("A")], None,
                                    extra_args=("--migration-document", str(doc)))
            self.assertEqual(proc.returncode, 2, proc.stderr)
            proc = _publish_dry_run(d, [_post("A2")], [_post("A")], None,
                                    extra_args=("--migration-document", str(Path(d) / "absent.json")))
            self.assertEqual(proc.returncode, 2, proc.stderr)
            self.assertIn("not a file", proc.stderr)

    def test_duplicate_source_records_stay_non_overridable(self):
        with tempfile.TemporaryDirectory() as d:
            proc = _publish_dry_run(d, [_post("A2"), _post("A2")], [_post("A")],
                                    _moves(("A", "A2")), extra_args=("--allow-regression",))
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertIn("DUPLICATE IDS", proc.stderr)
        self.assertIn("not overridable", proc.stderr)


# ---------------------------------------------------------------------------
# #1319: the read-only site verifier
# ---------------------------------------------------------------------------

def _site_fixture(root, *, stem="fixture"):
    """A profile pair, a site built exactly from it, and the preserved
    files a publish must leave alone. Returns (profile, posts, site)."""
    root = Path(root)
    profiles = root / "profiles"
    profiles.mkdir()
    (root / "upgrades").mkdir()
    site = root / "site"
    (site / "images").mkdir(parents=True)
    records = []
    for i, size in enumerate((11404, 20)):
        rel = f"images/plate-{i}.png"
        (site / rel).write_bytes(b"\x89PNG" + bytes(size - 4))
        records.append({"id": f"asset-{i}", "title": f"plate {i}", "file_path": rel,
                        "file_size_bytes": size, "source_root": "local",
                        "field_values": {"k": "v"}, "metadata": {}})
    posts = [_post("p1"), _post("p2")]
    profile_path = profiles / f"{stem}.assets.json"
    posts_path = profiles / f"{stem}.posts.json"
    profile_path.write_text(json.dumps(records), encoding="utf-8")
    posts_path.write_text(json.dumps(posts), encoding="utf-8")
    (site / "MANIFEST.json").write_text(json.dumps(records), encoding="utf-8")
    (site / "posts.json").write_text(json.dumps(posts), encoding="utf-8")
    (site / "posts.json.pre-1.bak").write_bytes(b"old posts")
    (site / "images" / "MANIFEST.json.pre-2.bak").write_bytes(b"old manifest")
    (site / "dataset-metadata.json").write_bytes(b'{"kaggle": true}')
    (site / "kenney-hq-replacements.json").write_bytes(b"[]")
    (site / "ATTRIBUTIONS.md").write_bytes(vs.REPO_ATTRIBUTIONS.read_bytes())
    return profile_path, posts_path, site


def _tree_digest(root):
    out = {}
    for p in sorted(Path(root).rglob("*")):
        if p.is_file():
            out[p.relative_to(root).as_posix()] = hashlib.sha256(p.read_bytes()).hexdigest()
    return out


def _verdict(rep, name):
    for v in rep.verdicts:
        if v.name == name:
            return v
    raise AssertionError(f"no verdict named {name!r}; have {[v.name for v in rep.verdicts]}")


class TestVerifySite(unittest.TestCase):
    """Class C: every file-level assertion, positive and negative, on a
    temp-directory site; a supplied expectation and a preservation
    baseline fail on one altered byte; and "not compared" is said out
    loud when there is nothing to compare against."""

    def test_a_site_built_from_the_profile_verifies_and_says_what_it_did_not_compare(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            before = _tree_digest(site)
            rep = vs.verify(profile, posts, site)
            self.assertTrue(rep.ok, "\n".join(str(v) for v in rep.failed))
            self.assertEqual(sorted(v.name for v in rep.not_compared),
                             ["expectations", "preserved files"])
            self.assertIn("NOT COMPARED", str(_verdict(rep, "preserved files")))
            self.assertEqual(_verdict(rep, "ATTRIBUTIONS.md equals the repository copy").status, vs.PASS)
            self.assertIn("not compared", rep.summary())
            self.assertEqual(_tree_digest(site), before, "the verifier wrote into the site")

    def test_counts_files_and_sizes_are_profile_derived(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            rows = json.loads((site / "posts.json").read_text())
            (site / "posts.json").write_text(json.dumps(rows[:1]), encoding="utf-8")
            rep = vs.verify(profile, posts, site)
            self.assertEqual(_verdict(rep, "posts.json rows").status, vs.FAIL)
            self.assertEqual(_verdict(rep, "posts.json distinct ids").status, vs.FAIL)
            self.assertEqual(_verdict(rep, "posts.json guard: no loss").status, vs.PASS,
                             "the site being behind the profile is not a loss")

            (site / "images" / "plate-1.png").write_bytes(b"x" * 21)
            (site / "images" / "plate-0.png").unlink()
            rep = vs.verify(profile, posts, site)
            v = _verdict(rep, "profile files present at the site")
            self.assertEqual(v.status, vs.FAIL)
            self.assertIn("plate-0.png", v.detail)
            v = _verdict(rep, "recorded file_size_bytes match the bytes on disk")
            self.assertEqual(v.status, vs.FAIL)
            self.assertIn("plate-1.png", v.detail)
            self.assertIn("on disk 21", v.detail)
            self.assertFalse(rep.ok)

    def test_the_guard_runs_profile_versus_site_with_the_migration_document(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            # The site still carries the old id; the document records the move.
            (site / "posts.json").write_text(json.dumps([_post("p0"), _post("p2")]), encoding="utf-8")
            rep = vs.verify(profile, posts, site)
            self.assertEqual(_verdict(rep, "posts.json guard: no loss").status, vs.FAIL)
            self.assertIn("MISSING_RECORD 1", _verdict(rep, "posts.json guard: no loss").detail)

            doc = Path(d) / "upgrades" / "post-id-migration.fixture.json"
            doc.write_text(json.dumps({"profile": "fixture.posts.json",
                                       "moves": _moves(("p0", "p1"))}), encoding="utf-8")
            rep = vs.verify(profile, posts, site)
            self.assertEqual(_verdict(rep, "posts.json guard: no loss").status, vs.PASS)
            self.assertEqual(_verdict(rep, "posts.json guard: migrated records").detail, "1")
            self.assertIn("1 recorded move", _verdict(rep, "migration document").detail)

            rep = vs.verify(profile, posts, site, expectations={"migrations": 1})
            self.assertEqual(_verdict(rep, "migrations").status, vs.PASS)
            rep = vs.verify(profile, posts, site, expectations={"migrations": 0})
            self.assertEqual(_verdict(rep, "migrations").status, vs.FAIL)

            doc.write_text("{bad", encoding="utf-8")
            rep = vs.verify(profile, posts, site)
            self.assertEqual(_verdict(rep, "migration document").status, vs.FAIL)
            self.assertFalse(rep.ok)

            # A corrupted measurement and a duplicate source id are their
            # own verdicts.
            records = json.loads(profile.read_text())
            site_records = json.loads(json.dumps(records))
            site_records[0]["source_root"] = "site"
            site_records[0]["file_size_bytes"] = 11405
            records[0]["source_root"] = "site"
            profile.write_text(json.dumps(records + [records[1]]), encoding="utf-8")
            (site / "MANIFEST.json").write_text(json.dumps(site_records), encoding="utf-8")
            rep = vs.verify(profile, posts, site)
            self.assertEqual(_verdict(rep, "MANIFEST.json guard: no corrupted measurement").status, vs.FAIL)
            self.assertEqual(_verdict(rep, "MANIFEST.json guard: no duplicate source ids").status, vs.FAIL)

    def test_site_specific_expectations_are_supplied_never_hard_wired(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            good = {"manifest_rows": 2, "manifest_ids": 2, "posts_rows": 2, "posts_ids": 2,
                    "once": ["p1", "p2"]}
            rep = vs.verify(profile, posts, site, expectations=good)
            self.assertTrue(rep.ok, "\n".join(str(v) for v in rep.failed))
            self.assertNotIn("expectations", [v.name for v in rep.not_compared])

            rows = json.loads((site / "posts.json").read_text())
            (site / "posts.json").write_text(json.dumps(rows + [rows[0]]), encoding="utf-8")
            rep = vs.verify(profile, posts, site, expectations=good)
            self.assertEqual(_verdict(rep, "posts_rows").status, vs.FAIL)
            self.assertEqual(_verdict(rep, "posts_ids").status, vs.PASS)
            v = _verdict(rep, "post p1 appears exactly once")
            self.assertEqual(v.status, vs.FAIL)
            self.assertIn("2 row(s)", v.detail)
            self.assertEqual(_verdict(rep, "post p2 appears exactly once").status, vs.PASS)

            with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
                json.dump({"manifest_rowz": 2}, f)
            with self.assertRaises(ValueError):
                vs.load_expectations(Path(f.name))
            Path(f.name).unlink()

    def test_the_preservation_baseline_fails_on_one_altered_byte(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            baseline = vs.record_baseline(site)
            self.assertEqual(sorted(baseline["files"]),
                             ["ATTRIBUTIONS.md", "dataset-metadata.json",
                              "images/MANIFEST.json.pre-2.bak", "kenney-hq-replacements.json",
                              "posts.json.pre-1.bak"])
            rep = vs.verify(profile, posts, site, baseline=baseline)
            v = _verdict(rep, "preserved files byte-equal to baseline")
            self.assertEqual(v.status, vs.PASS, v.detail)
            self.assertEqual(rep.not_compared and [x.name for x in rep.not_compared], ["expectations"])

            (site / "posts.json.pre-1.bak").write_bytes(b"old postS")
            (site / "kenney-hq-replacements.json").unlink()
            (site / "new.bak").write_bytes(b"made by the publish")
            rep = vs.verify(profile, posts, site, baseline=baseline)
            v = _verdict(rep, "preserved files byte-equal to baseline")
            self.assertEqual(v.status, vs.FAIL)
            self.assertIn("posts.json.pre-1.bak", v.detail)
            self.assertIn("kenney-hq-replacements.json", v.detail)
            self.assertIn("new.bak", _verdict(rep, "preserved files not in the baseline").detail)
            self.assertFalse(rep.ok)

    def test_a_reference_directory_works_like_a_baseline(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            ref = Path(d) / "reference"
            shutil.copytree(site, ref)
            rep = vs.verify(profile, posts, site, reference=ref)
            self.assertEqual(_verdict(rep, f"preserved files byte-equal to reference {ref}").status, vs.PASS)
            (site / "dataset-metadata.json").write_bytes(b'{"kaggle": false}')
            rep = vs.verify(profile, posts, site, reference=ref)
            self.assertEqual(_verdict(rep, f"preserved files byte-equal to reference {ref}").status, vs.FAIL)

    def test_attributions_must_equal_the_repository_copy(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            name = "ATTRIBUTIONS.md equals the repository copy"
            (site / "ATTRIBUTIONS.md").write_bytes(vs.REPO_ATTRIBUTIONS.read_bytes() + b"\n")
            rep = vs.verify(profile, posts, site)
            self.assertEqual(_verdict(rep, name).status, vs.FAIL)
            (site / "ATTRIBUTIONS.md").unlink()
            rep = vs.verify(profile, posts, site)
            self.assertEqual(_verdict(rep, name).status, vs.NOT_COMPARED)
            rep = vs.verify(profile, posts, site, expectations={"require_attributions": True})
            self.assertEqual(_verdict(rep, "ATTRIBUTIONS.md present at the site").status, vs.FAIL)

    def test_the_verifier_refuses_a_partially_staged_copy(self):
        """The staging drill's other half: the copy a failed publish left
        behind does not verify, and the verdict names the file."""
        with tempfile.TemporaryDirectory() as d:
            local, profile, posts, _live, stage = _staging_world(d)
            proc = _staging_publish(local, profile, posts, stage)
            self.assertEqual(proc.returncode, 1, proc.stderr)
            rep = vs.verify(profile, posts, stage)
            self.assertFalse(rep.ok)
            self.assertIn("zz-missing.png", _verdict(rep, "profile files present at the site").detail)
            self.assertEqual(_verdict(rep, "MANIFEST.json rows").status, vs.PASS,
                             "the root files were already replaced before the copy failed")

    def test_the_cli_exits_nonzero_on_failure_and_refuses_a_baseline_inside_the_site(self):
        with tempfile.TemporaryDirectory() as d:
            profile, posts, site = _site_fixture(d)
            out = Path(d) / "baseline.json"
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(vs.main(["baseline", "--site", str(site),
                                          "--out", str(site / "b.json")]), 2)
                self.assertEqual(vs.main(["baseline", "--site", str(site), "--out", str(out)]), 0)
            self.assertFalse((site / "b.json").exists())
            argv = ["check", "--profile", str(profile), "--posts", str(posts),
                    "--site", str(site), "--baseline", str(out)]
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                self.assertEqual(vs.main(argv), 0)
            self.assertIn("RESULT: VERIFIED", buf.getvalue())
            self.assertIn("NOT COMPARED", buf.getvalue())
            (site / "images" / "plate-0.png").write_bytes(b"short")
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                self.assertEqual(vs.main(argv), 1)
            self.assertIn("RESULT: FAILED", buf.getvalue())
            self.assertIn("FAIL", buf.getvalue())


def _staging_world(root):
    """A 'live' site with old root files, asset bytes and a `.bak`; a
    sibling copy of it; and a profile whose sorted copy order ends in a
    record whose source is missing. Shared by the characterization drill
    and the verifier test. Returns (local, profile, posts, live, stage)."""
    root = Path(root)
    local = root / "local"
    (local / "src").mkdir(parents=True)
    (local / "metadata.csv").write_text("file_path,title\n", encoding="utf-8")
    (root / "internet").mkdir()
    (root / "profiles").mkdir()
    (root / "upgrades").mkdir()
    live = root / "live"
    (live / "images").mkdir(parents=True)

    def rec(i, size):
        (local / "src" / f"p{i}.png").write_bytes(b"\x89PNG" + bytes(size - 4))
        return {"id": f"asset-{i}", "title": f"plate {i}",
                "file_path": f"images/p{i}.png", "source_path": f"src/p{i}.png",
                "source_root": "local", "file_size_bytes": size,
                "field_values": {}, "metadata": {}}

    old = [rec(0, 16), rec(1, 24)]
    for a in old:
        (live / a["file_path"]).write_bytes((local / a["source_path"]).read_bytes())
    # ⛔ THE PUBLISHED MANIFEST ALREADY RECORDS PLATE 0's NEW BYTE COUNT
    # WHILE THE FILE ITSELF STILL LAGS, which IS the partially-staged state
    # this fixture is about: the root files are replaced before the copy loop
    # runs, so a run that dies mid-copy leaves exactly this.
    #
    # It used to record the OLD count, and that stopped being expressible in
    # #1319: `local` is a MEASURABLE root now (its source dataset is gone and
    # the archive is the maintained copy), so a profile and a destination
    # disagreeing on a `local` file_size_bytes is a CORRUPTED_MEASUREMENT and
    # the guard refuses before the copy loop is reached. The fixture's subject
    # is the WRITE ORDER, not the root policy, so it must not smuggle in a
    # measurement disagreement to get there.
    published = json.loads(json.dumps(old))
    published[0]["file_size_bytes"] = 32
    (live / "MANIFEST.json").write_text(json.dumps(published), encoding="utf-8")
    (live / "posts.json").write_text(json.dumps([_post("p1")]), encoding="utf-8")
    (live / "posts.json.pre.bak").write_bytes(b"kept")
    # The new profile: a changed plate 0, a new plate 2, and a last
    # record (sorted by destination path) whose source is missing.
    new = [rec(0, 32), old[1], rec(2, 8),
           {"id": "asset-9", "title": "gone", "file_path": "images/zz-missing.png",
            "source_path": "src/zz-missing.png", "source_root": "local",
            "file_size_bytes": 5, "field_values": {}, "metadata": {}}]
    profile = root / "profiles" / "fixture.assets.json"
    posts = root / "profiles" / "fixture.posts.json"
    profile.write_text(json.dumps(new), encoding="utf-8")
    posts.write_text(json.dumps([_post("p1")]), encoding="utf-8")
    stage = root / "stage"
    shutil.copytree(live, stage)
    return local, profile, posts, live, stage


def _staging_publish(local, profile, posts, dest):
    return subprocess.run(
        [sys.executable, str(SCRIPTS / "populate_archive.py"),
         "--local-source", str(local), "--internet-source", str(local.parent / "internet"),
         "--profile", str(profile), "--posts", str(posts), "--dest", str(dest)],
        capture_output=True, text=True)


class TestStagingCharacterization(unittest.TestCase):
    """Characterization, not a #1319 regression: what a publish does to
    a SIBLING copy versus the live tree when its last copy fails. Passes
    on `dev` and on the branch; populate_archive.py's in-place write
    order is not changed by #1319.

    Against a sibling copy the failure is contained: the run exits 1,
    the assets before the failing one are already recopied in the copy,
    and the live tree is byte-identical to before (the verifier refusing
    that copy is asserted in TestVerifySite, which is branch-only).
    Against the live tree the same failure leaves it partially mutated
    (root files already replaced), which is why the sibling staging
    exists.
    """

    def test_a_failed_publish_against_a_sibling_copy_leaves_the_live_tree_untouched(self):
        with tempfile.TemporaryDirectory() as d:
            local, profile, posts, live, stage = _staging_world(d)
            live_before = _tree_digest(live)
            proc = _staging_publish(local, profile, posts, stage)
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertIn("MISSING", proc.stderr)
            # Earlier assets were recopied into the COPY.
            self.assertEqual((stage / "images" / "p0.png").stat().st_size, 32)
            self.assertTrue((stage / "images" / "p2.png").is_file())
            self.assertEqual(json.loads((stage / "MANIFEST.json").read_text())[0]["file_size_bytes"], 32)
            # The live tree did not move.
            self.assertEqual(_tree_digest(live), live_before)

    def test_the_same_failure_against_the_live_tree_mutates_it_partially(self):
        with tempfile.TemporaryDirectory() as d:
            local, profile, posts, live, _stage = _staging_world(d)
            live_before = _tree_digest(live)
            proc = _staging_publish(local, profile, posts, live)
            self.assertEqual(proc.returncode, 1, proc.stderr)
            after = _tree_digest(live)
            self.assertNotEqual(after, live_before, "the live tree was mutated by a failed run")
            self.assertEqual(json.loads((live / "MANIFEST.json").read_text())[0]["file_size_bytes"], 32,
                             "root files are replaced before the copy loop runs")
            self.assertEqual(after["posts.json.pre.bak"], live_before["posts.json.pre.bak"])


# ---------------------------------------------------------------------------
# #1319: a record that can never materialize is retired by document
# ---------------------------------------------------------------------------
#
# The defect: two catalogue records describing the same produced bytes
# owned by one user. `(owner_user_ref, file_hash)` is the app's identity
# and no DedupBehavior value relaxes it, so one of them gets no row, no
# field values, and the post naming it silently loses a member.
#
# Everything below builds synthetic fixtures. Nothing here needs the
# pack, the pool, the archive share or the network, because the required
# guard suite runs on a runner that has none of them.

SRC_SHA = "a" * 64
SRC_SHA_2 = "b" * 64


def _collapse_rec(rid, owner="priya.sharma", *, name=None, root="hq",
                  src_sha=SRC_SHA, px=512, **over):
    name = name or rid[:8]
    rec = {
        "id": rid,
        "owner_username": owner,
        "source_root": root,
        "source_path": f"{name}.png",
        "file_path": f"images/kenney-hq/{name}.png",
        "file_size_bytes": 11,
        "title": "Slide horizontal grey section wide (vector)",
        "license": "CC0 1.0",
        "archive_state": "active",
        "field_values": {"rating": 4, "country": "ng"},
        "metadata": {
            "filename": f"{name}.png",
            "render": {"px": px, "tool": "seed/scripts/rasterize_svg.mjs"},
            "source_archive": {"member": f"Vector/{name}.svg", "sha256": src_sha},
        },
    }
    rec.update(over)
    return rec


def _collapse_entry(retired, survivor, *, post_subs=(), mat=None, losses=None,
                    **over):
    entry = {
        "retired_id": retired["id"],
        "survivor_id": survivor["id"],
        "owner_username": retired["owner_username"],
        "source_root": retired["source_root"],
        "retired_file_path": retired["file_path"],
        "evidence": {
            # `kind` is REQUIRED and has no default (#1319). The fixtures
            # describe `hq` renders, so the default here is the
            # produced-source claim; `_preserved_entry` below builds the
            # other kind.
            "kind": ac.KIND_PRODUCED,
            "source_sha256": retired["metadata"]["source_archive"]["sha256"],
            "member": retired["metadata"]["source_archive"]["member"],
            "render_px": retired["metadata"]["render"]["px"],
            "materialized_sha256": mat or ("c" * 64),
            "materialized_tool": "seed/scripts/rasterize_svg.mjs (sharp 0.35.4)",
        },
        "retired_record": json.loads(json.dumps(retired)),
        "acknowledged_losses": (ac.recompute_losses(retired, survivor)
                                if losses is None else losses),
        "post_substitutions": list(post_subs),
    }
    entry.update(over)
    return entry


def _collapse_doc(entries, profile="studio-a.assets.json", **over):
    doc = {
        "_why": ["fixture"],
        "profile": profile,
        "pool_of_record": {
            "pack": "Kenney Game Assets All-in-1",
            "render_px": 512,
            "rasteriser": "seed/scripts/rasterize_svg.mjs",
            "sharp": "0.35.4",
            "node": "v22.23.1",
        },
        "collapse": list(entries),
    }
    doc.update(over)
    return doc


def _sub(post_id, old, new, **over):
    s = {"post_id": post_id, "old_members": list(old), "new_members": list(new)}
    s.update(over)
    return s


RETIRED_ID = "5b26546f-9647-5175-24e3-a3a55532f310"
SURVIVOR_ID = "05e1977d-cd90-e2ab-507a-4bb7322926c9"
OTHER_ID = "4bb8fbc3-a67d-dcfa-b8b7-a1ff2d39ddc9"
POST_ID = "a5b03a15-dd4a-445c-0ed4-6d27438806fa"
POST_ID_2 = "9c8813f6-6e12-0f3c-bf30-6358efe73d14"


class TestCollapseDocumentStructure(unittest.TestCase):
    """LAYER A1: document-internal, before the document reaches any pass.

    Every check is a refusal and none is overridable, for the same reason
    the migration document's are: a document that cannot be validated is
    not evidence, and "unusable" must never quietly become "nothing to
    retire". That reading would let a run write a profile the document
    was supposed to govern.
    """

    def setUp(self):
        self.retired = _collapse_rec(RETIRED_ID)
        self.survivor = _collapse_rec(SURVIVOR_ID, archive_state="draft")
        self.doc = _collapse_doc([_collapse_entry(self.retired, self.survivor)])

    def _parse(self, doc=None, **kw):
        return ac.parse_collapse_document(doc or self.doc, source="fixture", **kw)

    def _refuses(self, doc, fragment):
        with self.assertRaises(ac.CollapseError) as cm:
            ac.parse_collapse_document(doc, source="fixture")
        self.assertIn(fragment, str(cm.exception))

    def test_a_valid_document_parses(self):
        doc = self._parse()
        self.assertEqual(len(doc.entries), 1)
        self.assertEqual(doc.retired_ids, frozenset({RETIRED_ID}))
        self.assertEqual(doc.survivor_ids, frozenset({SURVIVOR_ID}))

    def test_an_empty_collapse_list_is_a_legal_no_op(self):
        """N=0. Absence of a retirement is never a failure, and it is
        never permission either: nothing is retired."""
        doc = self._parse(_collapse_doc([]))
        self.assertEqual(doc.entries, ())
        self.assertEqual(doc.retired_ids, frozenset())

    def test_a_document_for_another_profile_is_refused(self):
        with self.assertRaises(ac.CollapseError) as cm:
            self._parse(profile_name="studio-b.assets.json")
        self.assertIn("refusing to apply one profile's retirements", str(cm.exception))

    def test_a_missing_pool_of_record_is_refused(self):
        doc = _collapse_doc([_collapse_entry(self.retired, self.survivor)])
        doc.pop("pool_of_record")
        self._refuses(doc, "pool_of_record")

    def test_a_pool_of_record_without_the_locked_toolchain_is_refused(self):
        """A produced hash is only reproducible against the toolchain
        that produced it, so a document that does not name one records a
        number nobody can re-derive."""
        for key in ("sharp", "node", "rasteriser"):
            doc = _collapse_doc([_collapse_entry(self.retired, self.survivor)])
            doc["pool_of_record"].pop(key)
            with self.subTest(key=key):
                self._refuses(doc, f"pool_of_record.{key}")

    def test_the_two_hashes_may_not_be_the_same_value(self):
        e = _collapse_entry(self.retired, self.survivor, mat=SRC_SHA)
        self._refuses(_collapse_doc([e]), "both the source hash and the produced hash")

    def test_a_malformed_hash_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor, mat="not-a-hash")
        self._refuses(_collapse_doc([e]), "materialized_sha256 is not a sha256")

    def test_a_retired_record_that_disagrees_with_the_evidence_is_refused(self):
        rec = _collapse_rec(RETIRED_ID, src_sha=SRC_SHA_2)
        e = _collapse_entry(rec, self.survivor)
        e["evidence"]["source_sha256"] = SRC_SHA
        self._refuses(_collapse_doc([e]), "source_archive.sha256")

    def test_a_render_px_that_disagrees_with_the_record_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor)
        e["evidence"]["render_px"] = 1024
        self._refuses(_collapse_doc([e]), "render.px")

    def test_an_unauthenticable_source_root_is_refused(self):
        """A pre-staged root has no source to re-derive produced bytes
        from, so a retirement on one could never reach Layer B and would
        be a Layer-A claim wearing a publish tick."""
        rec = _collapse_rec(RETIRED_ID, root="site")
        self._refuses(_collapse_doc([_collapse_entry(rec, self.survivor)]),
                      "has no reproducible source")

    def test_retiring_a_record_onto_itself_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor)
        e["survivor_id"] = e["retired_id"]
        self._refuses(_collapse_doc([e]), "onto itself")

    def test_one_id_retired_twice_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor)
        self._refuses(_collapse_doc([e, json.loads(json.dumps(e))]), "retired twice")

    def test_an_id_that_is_both_retired_and_a_survivor_is_refused(self):
        third = _collapse_rec(OTHER_ID)
        e1 = _collapse_entry(self.retired, self.survivor)
        e2 = _collapse_entry(self.survivor, third)
        self._refuses(_collapse_doc([e1, e2]),
                      "both a retired id and a survivor")

    def test_unsorted_acknowledged_losses_are_refused(self):
        losses = ac.recompute_losses(self.retired, self.survivor)
        e = _collapse_entry(self.retired, self.survivor,
                            losses=list(reversed(losses)))
        self._refuses(_collapse_doc([e]), "not sorted by path")

    def test_a_substitution_that_changes_nothing_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor,
                            post_subs=[_sub(POST_ID, [RETIRED_ID], [RETIRED_ID])])
        self._refuses(_collapse_doc([e]), "changes nothing")

    def test_a_substitution_that_keeps_the_retired_id_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor,
                            post_subs=[_sub(POST_ID, [OTHER_ID, RETIRED_ID],
                                            [OTHER_ID, RETIRED_ID, SURVIVOR_ID])])
        self._refuses(_collapse_doc([e]), "still contains the retired id")

    def test_a_substitution_that_drops_the_survivor_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor,
                            post_subs=[_sub(POST_ID, [OTHER_ID, RETIRED_ID],
                                            [OTHER_ID])])
        self._refuses(_collapse_doc([e]), "does not contain the survivor")

    def test_two_entries_may_not_substitute_one_post(self):
        """A chained substitution cannot be authenticated: every object is
        judged before any is applied, so the second entry's old_members
        would describe a state the stage never sees. A post losing several
        retired members is ONE substitution naming the final membership."""
        third = _collapse_rec(OTHER_ID, src_sha=SRC_SHA_2)
        survivor2 = _collapse_rec("77777777-8888-9999-aaaa-bbbbbbbbbbbb",
                                  src_sha=SRC_SHA_2)
        e1 = _collapse_entry(self.retired, self.survivor,
                             post_subs=[_sub(POST_ID, [OTHER_ID, RETIRED_ID],
                                             [OTHER_ID, SURVIVOR_ID])])
        e2 = _collapse_entry(third, survivor2,
                             post_subs=[_sub(POST_ID, [OTHER_ID, SURVIVOR_ID],
                                             [survivor2["id"], SURVIVOR_ID])])
        self._refuses(_collapse_doc([e1, e2]), "substituted by two entries")

    def test_an_unknown_key_is_refused(self):
        e = _collapse_entry(self.retired, self.survivor)
        e["also_delete"] = ["something"]
        self._refuses(_collapse_doc([e]), "unknown key(s)")

    def test_the_document_path_derives_from_the_profile(self):
        got = ac.collapse_document_path(PROFILES / "studio-a.assets.json")
        self.assertEqual(got, UPGRADES / "asset-collapse.studio-a.json")
        self.assertIsNone(ac.collapse_document_path(Path("/x/posts.json")))

    def test_an_unreadable_document_raises_rather_than_reading_as_empty(self):
        with tempfile.TemporaryDirectory() as d:
            bad = Path(d) / "asset-collapse.studio-a.json"
            bad.write_text("{not json", encoding="utf-8")
            with self.assertRaises(ac.CollapseError):
                ac.load_collapse_document(bad)


class TestCollapseLossesAreEnumerated(unittest.TestCase):
    """The guard must not diff the retired record against the survivor and
    call the result acceptable. The document carries the enumeration; both
    layers recompute it and compare for EQUALITY."""

    def test_a_conflicting_value_is_a_loss_and_the_survivor_wins(self):
        retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        survivor = _collapse_rec(SURVIVOR_ID, archive_state="active")
        losses = ac.recompute_losses(retired, survivor)
        by_path = {x["path"]: x for x in losses}
        self.assertEqual(by_path["archive_state"]["retired_value"], "draft")
        self.assertEqual(by_path["archive_state"]["survivor_value"], "active")

    def test_a_key_only_the_retired_record_has_is_dropped_and_enumerated(self):
        retired = _collapse_rec(RETIRED_ID)
        retired["field_values"]["production_notes"] = "kept nowhere"
        survivor = _collapse_rec(SURVIVOR_ID)
        by_path = {x["path"]: x for x in ac.recompute_losses(retired, survivor)}
        self.assertIn("field_values.production_notes", by_path)
        self.assertNotIn("survivor_value", by_path["field_values.production_notes"])

    def test_a_key_only_the_survivor_has_is_kept_and_is_not_a_loss(self):
        retired = _collapse_rec(RETIRED_ID)
        survivor = _collapse_rec(SURVIVOR_ID)
        survivor["field_values"]["copyright"] = "survivor only"
        paths = {x["path"] for x in ac.recompute_losses(retired, survivor)}
        self.assertNotIn("field_values.copyright", paths)

    def test_identical_values_are_no_ops(self):
        rec = _collapse_rec(RETIRED_ID)
        self.assertEqual(ac.recompute_losses(rec, json.loads(json.dumps(rec))), [])

    def test_losses_are_found_below_the_two_nested_levels_the_guard_descends(self):
        retired = _collapse_rec(RETIRED_ID)
        survivor = _collapse_rec(SURVIVOR_ID)
        survivor["metadata"]["source_archive"]["member"] = "Vector/Other.svg"
        paths = {x["path"] for x in ac.recompute_losses(retired, survivor)}
        self.assertIn("metadata.source_archive.member", paths)

    def test_a_retired_only_path_whose_value_is_FALSY_is_still_a_loss(self):
        """⛔ THE DANGEROUS CASE, AND NOTHING COVERED IT.

        `acknowledged_losses` must hold every retired leaf that does not
        survive, including one whose value is `""`, `[]`, `{}`, `false`, `0`
        or `null`. An enumeration built behind `if retired_value:` would drop
        exactly these and still equal a recomputation built the same way, so
        the two would agree and the loss would be invisible. Each falsy value
        is asserted on its own, because a single `""` case would not catch a
        guard that special-cased only the empty string.
        """
        for falsy in ("", [], {}, False, 0, None):
            retired = _collapse_rec(RETIRED_ID)
            retired["field_values"]["production_notes"] = falsy
            survivor = _collapse_rec(SURVIVOR_ID)
            survivor["field_values"].pop("production_notes", None)
            losses = ac.recompute_losses(retired, survivor)
            by_path = {x["path"]: x for x in losses}
            with self.subTest(retired_value=repr(falsy)):
                self.assertIn("field_values.production_notes", by_path,
                              "a retired-only leaf was dropped because its value "
                              "is falsy")
                loss = by_path["field_values.production_notes"]
                self.assertNotIn("survivor_value", loss)
                self.assertEqual(loss["retired_value"], falsy)
                self.assertIs(type(loss["retired_value"]), type(falsy))

    def test_a_differing_path_whose_retired_value_is_FALSY_is_still_a_loss(self):
        """The same hole one step over: the retired value is falsy and the
        survivor holds a real one, so the row differs rather than being
        retired-only. `null` against a string is the case the committed
        site_b document actually carries."""
        for falsy in ("", [], {}, False, 0, None):
            retired = _collapse_rec(RETIRED_ID)
            retired["review_notes"] = falsy
            survivor = _collapse_rec(SURVIVOR_ID)
            survivor["review_notes"] = "the survivor's note"
            by_path = {x["path"]: x
                       for x in ac.recompute_losses(retired, survivor)}
            with self.subTest(retired_value=repr(falsy)):
                self.assertIn("review_notes", by_path)
                self.assertEqual(by_path["review_notes"]["retired_value"], falsy)
                self.assertEqual(by_path["review_notes"]["survivor_value"],
                                 "the survivor's note")

    def test_a_falsy_value_that_is_UNCHANGED_is_still_not_a_loss(self):
        """The other half of the same boundary. Falsy must not become a
        synonym for "lost" either: an equal value is a no-op whatever it is,
        and `0 == False` in Python, so the pair is checked by type too."""
        for falsy in ("", [], {}, False, 0, None):
            retired = _collapse_rec(RETIRED_ID)
            retired["review_notes"] = falsy
            survivor = _collapse_rec(SURVIVOR_ID)
            survivor["review_notes"] = falsy
            paths = {x["path"] for x in ac.recompute_losses(retired, survivor)}
            with self.subTest(retired_value=repr(falsy)):
                self.assertNotIn("review_notes", paths)

    def test_the_committed_document_enumerates_exactly_what_it_would_lose(self):
        """The real one, against the real survivor. This is the check that
        fails the moment either record moves under the document."""
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-a.json",
                                        profile_name="studio-a.assets.json")
        by_id = {a["id"]: a for a in json.loads(
            (PROFILES / "studio-a.assets.json").read_text(encoding="utf-8"))}
        for e in doc.entries:
            with self.subTest(retired=e.retired_id):
                self.assertIn(e.survivor_id, by_id)
                self.assertTrue(ac.losses_equal(
                    e.acknowledged_losses,
                    ac.recompute_losses(e.retired_record, by_id[e.survivor_id])))


class TestCollapseState(unittest.TestCase):
    """LAYER A2: exactly Pending or Applied, per object. A third state is a
    hard failure and never a normalisation."""

    def setUp(self):
        self.retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        self.survivor = _collapse_rec(SURVIVOR_ID)
        self.other = _collapse_rec(OTHER_ID)
        self.sub = _sub(POST_ID, [OTHER_ID, RETIRED_ID], [OTHER_ID, SURVIVOR_ID])
        self.doc = ac.parse_collapse_document(
            _collapse_doc([_collapse_entry(self.retired, self.survivor,
                                           post_subs=[self.sub])]),
            source="fixture")

    def _pending(self):
        return ([json.loads(json.dumps(r)) for r in
                 (self.other, self.survivor, self.retired)],
                [{"id": POST_ID, "asset_ids": [OTHER_ID, RETIRED_ID]}])

    def _applied(self):
        return ([json.loads(json.dumps(r)) for r in (self.other, self.survivor)],
                [{"id": POST_ID, "asset_ids": [OTHER_ID, SURVIVOR_ID]}])

    def test_pending_is_recognised_and_converted(self):
        profile, posts = self._pending()
        res = ac.apply_collapse(profile, posts, self.doc)
        self.assertEqual(res.states[0].asset_state, ac.PENDING)
        self.assertEqual(res.records_removed, 1)
        self.assertEqual(posts[0]["asset_ids"], [OTHER_ID, SURVIVOR_ID])
        self.assertNotIn(RETIRED_ID, {a["id"] for a in profile})

    def test_applied_is_an_idempotent_no_op(self):
        profile, posts = self._applied()
        before = json.dumps([profile, posts], sort_keys=True)
        res = ac.apply_collapse(profile, posts, self.doc)
        self.assertEqual(res.states[0].asset_state, ac.APPLIED)
        self.assertEqual(res.records_removed, 0)
        self.assertEqual(json.dumps([profile, posts], sort_keys=True), before)

    def test_running_the_stage_twice_changes_nothing_the_second_time(self):
        profile, posts = self._pending()
        ac.apply_collapse(profile, posts, self.doc)
        after_first = json.dumps([profile, posts], sort_keys=True)
        ac.apply_collapse(profile, posts, self.doc)
        self.assertEqual(json.dumps([profile, posts], sort_keys=True), after_first)

    def test_a_changed_retired_record_fails_before_any_deletion(self):
        profile, posts = self._pending()
        profile[2]["title"] = "somebody edited this"
        with self.assertRaises(ac.CollapseError) as cm:
            ac.apply_collapse(profile, posts, self.doc)
        self.assertIn("not the retired_record this document authorises",
                      str(cm.exception))
        self.assertIn(RETIRED_ID, {a["id"] for a in profile})

    def test_an_absent_survivor_fails(self):
        profile, posts = self._pending()
        profile[:] = [a for a in profile if a["id"] != SURVIVOR_ID]
        with self.assertRaises(ac.CollapseError) as cm:
            ac.apply_collapse(profile, posts, self.doc)
        self.assertIn("is not in the profile", str(cm.exception))
        self.assertIn(RETIRED_ID, {a["id"] for a in profile})

    def test_an_acknowledged_loss_list_that_disagrees_fails(self):
        doc = ac.parse_collapse_document(
            _collapse_doc([_collapse_entry(
                self.retired, self.survivor, post_subs=[self.sub],
                losses=ac.recompute_losses(self.retired, self.survivor)[:-1])]),
            source="fixture")
        profile, posts = self._pending()
        with self.assertRaises(ac.CollapseError) as cm:
            ac.apply_collapse(profile, posts, doc)
        self.assertIn("acknowledged_losses", str(cm.exception))

    def test_an_order_only_permutation_is_a_third_state(self):
        """The membership comparisons are exact, order included. The
        curation digest sorts, so it cannot stand in for this."""
        profile, posts = self._pending()
        posts[0]["asset_ids"] = [RETIRED_ID, OTHER_ID]
        with self.assertRaises(ac.CollapseError) as cm:
            ac.apply_collapse(profile, posts, self.doc)
        self.assertIn("neither exactly the documented old_members", str(cm.exception))
        self.assertIn(RETIRED_ID, {a["id"] for a in profile})

    def test_an_extra_member_is_a_third_state(self):
        profile, posts = self._pending()
        posts[0]["asset_ids"] = [OTHER_ID, RETIRED_ID, SURVIVOR_ID]
        with self.assertRaises(ac.CollapseError):
            ac.apply_collapse(profile, posts, self.doc)

    def test_an_absent_post_is_a_third_state(self):
        profile, posts = self._pending()
        posts.clear()
        with self.assertRaises(ac.CollapseError):
            ac.apply_collapse(profile, posts, self.doc)

    def test_the_asset_may_be_applied_while_the_post_is_pending(self):
        profile, posts = self._applied()
        posts[0]["asset_ids"] = [OTHER_ID, RETIRED_ID]
        res = ac.apply_collapse(profile, posts, self.doc)
        self.assertEqual(res.states[0].asset_state, ac.APPLIED)
        self.assertEqual(res.states[0].post_states[POST_ID], ac.PENDING)
        self.assertEqual(posts[0]["asset_ids"], [OTHER_ID, SURVIVOR_ID])

    def test_the_post_may_be_applied_while_the_asset_is_pending(self):
        profile, posts = self._pending()
        posts[0]["asset_ids"] = [OTHER_ID, SURVIVOR_ID]
        res = ac.apply_collapse(profile, posts, self.doc)
        self.assertEqual(res.states[0].asset_state, ac.PENDING)
        self.assertEqual(res.states[0].post_states[POST_ID], ac.APPLIED)
        self.assertEqual(res.records_removed, 1)

    def test_a_later_third_state_prevents_an_earlier_deletion(self):
        """Every entry is judged before any is applied. A stale document
        whose second entry no longer matches must not delete the record
        its first entry names."""
        r2 = _collapse_rec("11111111-2222-3333-4444-555555555555", src_sha=SRC_SHA_2)
        s2 = _collapse_rec("66666666-7777-8888-9999-aaaaaaaaaaaa", src_sha=SRC_SHA_2)
        doc = ac.parse_collapse_document(
            _collapse_doc([_collapse_entry(self.retired, self.survivor,
                                           post_subs=[self.sub]),
                           _collapse_entry(r2, s2)]),
            source="fixture")
        profile, posts = self._pending()
        profile += [json.loads(json.dumps(s2)),
                    dict(json.loads(json.dumps(r2)), title="edited")]
        with self.assertRaises(ac.CollapseError):
            ac.apply_collapse(profile, posts, doc)
        self.assertIn(RETIRED_ID, {a["id"] for a in profile})

    def test_n_of_five_collapses_to_one_survivor(self):
        """N>=2 within one group. The survivor rule holds at every size,
        and a post naming all of them keeps exactly the survivor."""
        ids = ["1111aaaa-1111-1111-1111-111111111111",
               "2222aaaa-2222-2222-2222-222222222222",
               "3333aaaa-3333-3333-3333-333333333333",
               "4444aaaa-4444-4444-4444-444444444444"]
        survivor = _collapse_rec(SURVIVOR_ID)
        retired = [_collapse_rec(i, archive_state="draft") for i in ids]
        subs = []
        entries = []
        for r in retired:
            entries.append(_collapse_entry(r, survivor, post_subs=[]))
        doc_entries = entries
        doc_entries[0]["post_substitutions"] = [
            _sub(POST_ID, ids + [SURVIVOR_ID], [SURVIVOR_ID])]
        for e in doc_entries[1:]:
            e["post_substitutions"] = []
        # Only one substitution may name a post, so the other three
        # entries retire without touching it.
        doc = ac.parse_collapse_document(_collapse_doc(doc_entries), source="fixture")
        profile = [json.loads(json.dumps(survivor))] + \
                  [json.loads(json.dumps(r)) for r in retired]
        posts = [{"id": POST_ID, "asset_ids": ids + [SURVIVOR_ID]}]
        res = ac.apply_collapse(profile, posts, doc)
        self.assertEqual(res.records_removed, 4)
        self.assertEqual([a["id"] for a in profile], [SURVIVOR_ID])
        self.assertEqual(posts[0]["asset_ids"], [SURVIVOR_ID])
        self.assertEqual(len(subs), 0)

    def test_a_post_holding_both_survivor_and_retired_dedupes_to_one(self):
        doc = ac.parse_collapse_document(
            _collapse_doc([_collapse_entry(
                self.retired, self.survivor,
                post_subs=[_sub(POST_ID, [SURVIVOR_ID, RETIRED_ID], [SURVIVOR_ID])])]),
            source="fixture")
        profile, _ = self._pending()
        posts = [{"id": POST_ID, "asset_ids": [SURVIVOR_ID, RETIRED_ID]}]
        ac.apply_collapse(profile, posts, doc)
        self.assertEqual(posts[0]["asset_ids"], [SURVIVOR_ID])

    def test_a_substitution_can_move_the_post_id(self):
        """A kind that DOES derive its id from membership records a
        new_post_id; `asset_group` does not and omits it."""
        moved = _sub(POST_ID, [OTHER_ID, RETIRED_ID], [OTHER_ID, SURVIVOR_ID],
                     new_post_id=POST_ID_2)
        doc = ac.parse_collapse_document(
            _collapse_doc([_collapse_entry(self.retired, self.survivor,
                                           post_subs=[moved])]),
            source="fixture")
        profile, posts = self._pending()
        res = ac.apply_collapse(profile, posts, doc)
        self.assertEqual(res.posts_renamed, 1)
        self.assertEqual(posts[0]["id"], POST_ID_2)
        self.assertEqual(doc.moved_post_ids, frozenset({POST_ID}))

    def test_a_substitution_that_keeps_the_id_moves_nothing(self):
        self.assertEqual(self.doc.moved_post_ids, frozenset())

    def test_cross_owner_identical_bytes_are_never_a_collision(self):
        """The CAS dedup fixture. Identity is per OWNER, so two records
        with the same input and different owners are legal."""
        a = _collapse_rec(RETIRED_ID, owner="priya.sharma")
        b = _collapse_rec(SURVIVOR_ID, owner="chen.wei")
        self.assertEqual(ac.proxy_collisions([a, b]), {})


class TestProxyCollisionKeyIsPartial(unittest.TestCase):
    """The key is NECESSARY but not SUFFICIENT for the real invariant,
    which is `(owner, produced_byte_sha256)`, and it is documented that
    way everywhere it is used."""

    def test_records_with_no_source_archive_hash_are_not_keyed(self):
        rec = _collapse_rec(RETIRED_ID)
        rec["metadata"].pop("source_archive")
        self.assertIsNone(ac.proxy_key(rec))
        self.assertEqual(ac.proxy_collisions([rec, _collapse_rec(SURVIVOR_ID)]), {})

    def test_the_same_input_at_a_different_render_size_is_not_a_collision(self):
        a = _collapse_rec(RETIRED_ID, px=512)
        b = _collapse_rec(SURVIVOR_ID, px=1024)
        self.assertEqual(ac.proxy_collisions([a, b]), {})

    def test_one_owner_one_input_one_size_is_a_collision(self):
        a = _collapse_rec(RETIRED_ID)
        b = _collapse_rec(SURVIVOR_ID)
        groups = ac.proxy_collisions([a, b])
        self.assertEqual(len(groups), 1)
        self.assertEqual(sorted(next(iter(groups.values()))), sorted([RETIRED_ID, SURVIVOR_ID]))


class TestCommittedProfilesHoldNoSameOwnerDuplicate(unittest.TestCase):
    """W1. No two records in a committed assets profile may share
    `(owner_username, metadata.source_archive.sha256, metadata.render.px)`.

    ⚠️ A PARTIAL PROXY, and the docstring says so on purpose. The app's
    live key is `(owner_user_ref, file_hash)` over the PRODUCED bytes.
    This one needs neither pack nor pool, so it runs on a CI runner that
    has neither; two records sharing it certainly share an input and a
    render size, which is how the one live collision was found, and two
    records can still produce identical bytes from different inputs
    without this saying a word. The sufficient check is the publish
    guard, which hashes the produced files under the source roots.
    """

    PROFILES_UNDER_TEST = ("studio-a", "studio-b", "demo", "dev")

    def test_no_committed_profile_holds_a_same_owner_input_duplicate(self):
        for stem in self.PROFILES_UNDER_TEST:
            profile = json.loads((PROFILES / f"{stem}.assets.json")
                                 .read_text(encoding="utf-8"))
            groups = ac.proxy_collisions(profile)
            with self.subTest(profile=stem):
                self.assertEqual(
                    groups, {},
                    f"{stem}.assets.json holds {len(groups)} group(s) of records "
                    f"owned by one user that share a source archive hash and a "
                    f"render size. The app can hold only one of each group: "
                    f"retire the losers with an asset-collapse document.")

    def test_the_check_is_looking_at_something(self):
        """A guard nothing reaches is a guard that cannot fail. 895 of
        studio-a's records carry the key this test is about."""
        profile = json.loads((PROFILES / "studio-a.assets.json")
                             .read_text(encoding="utf-8"))
        keyed = [a for a in profile if ac.proxy_key(a) is not None]
        self.assertGreater(len(keyed), 500, "the proxy key reached almost nothing")

    def test_every_documented_survivor_is_in_the_profile(self):
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-a.json",
                                        profile_name="studio-a.assets.json")
        ids = {a["id"] for a in json.loads(
            (PROFILES / "studio-a.assets.json").read_text(encoding="utf-8"))}
        self.assertEqual(doc.survivor_ids - ids, set())
        self.assertEqual(doc.retired_ids & ids, set())

    def test_no_committed_post_names_a_retired_id(self):
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-a.json",
                                        profile_name="studio-a.assets.json")
        for stem in ("studio-a", "dataset"):
            posts = json.loads((PROFILES / f"{stem}.posts.json")
                               .read_text(encoding="utf-8"))
            named = [p["id"] for p in posts
                     if set(p.get("asset_ids") or ()) & doc.retired_ids]
            with self.subTest(posts=stem):
                self.assertEqual(named, [])

    def test_site_b_has_a_preserved_only_collapse_document(self):
        """⭐ THE PREMISE OF THE TEST THIS REPLACES IS GONE (#1319).

        It used to assert site_b had NO document, because the 13 same-owner
        byte groups were OBSERVATIONS about a destination and ADR 0097 made
        the destination an output. The owner has since retired the old
        `$DATASET_SRC`, and ADR 0097's 2026-09-24 amendment makes the
        archive the MAINTAINED DATASET for `local`. So the evidence now
        exists, under the WEAKER `preserved_archive` kind, and the document
        is required rather than forbidden.
        """
        path = UPGRADES / "asset-collapse.studio-b.json"
        self.assertTrue(path.is_file(), "the site_b collapse document is absent")
        doc = ac.load_collapse_document(path, profile_name="studio-b.assets.json")
        self.assertTrue(doc.is_preserved_only)
        self.assertFalse(doc.has_produced_source)
        # ⛔ A preserved-only document must carry NO pool_of_record: it
        # produced nothing, so a pool would describe a toolchain with no part
        # in the claim. The parser refuses one; this pins the committed file.
        self.assertEqual(doc.pool_of_record, {})
        raw = json.loads(path.read_text(encoding="utf-8"))
        self.assertNotIn("pool_of_record", raw)
        self.assertEqual({e.kind for e in doc.entries}, {ac.KIND_PRESERVED})
        self.assertEqual({e.source_root for e in doc.entries}, {"local"})
        for e in doc.entries:
            with self.subTest(retired=e.retired_id):
                # No fabricated produced-source provenance, anywhere.
                for absent in ("source_sha256", "materialized_sha256",
                               "source_member", "render_px",
                               "materialized_tool"):
                    self.assertIsNone(getattr(e, absent), absent)
                self.assertEqual(e.retired_sha256, e.survivor_sha256)
                self.assertNotIn("source_archive", e.retired_record["metadata"])
                self.assertNotIn("media_url", e.retired_record["metadata"])
                self.assertNotIn("sha256", e.retired_record["metadata"])

    def test_site_b_retires_exactly_the_twenty_two_that_cannot_materialize(self):
        """13 groups over 35 records can hold 13 rows, so 22 records retire.

        The counts are LITERAL. Deriving them from the document would make
        the test unable to fail, which is the whole point of pinning a
        reviewed corpus correction.
        """
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        self.assertEqual(len(doc.entries), 22)
        self.assertEqual(len(doc.retired_ids), 22)
        self.assertEqual(len(doc.survivor_ids), 13)
        self.assertEqual(doc.retired_ids & doc.survivor_ids, set())
        # Group sizes: 7 pairs, 4 triples, one 4 and one 5 -> 22 victims.
        from collections import Counter
        per_survivor = Counter(e.survivor_id for e in doc.entries)
        self.assertEqual(sorted(Counter(per_survivor.values()).items()),
                         [(1, 7), (2, 4), (3, 1), (4, 1)])
        self.assertEqual(sum(per_survivor.values()), 22)

    def test_site_b_survivor_choices_are_the_recorded_deterministic_ones(self):
        """⛔ A SURVIVOR IS CHOSEN BY A RULE, NOT BY WHICHEVER MATERIALIZED.

        The rule is the member earliest in profile (JSON) order, which is the
        row `applyAssets` reaches first (app/internal/seed/runner.go:762),
        qualified by the readable-bytes rule (:763-768). The 13 resulting
        (owner, survivor) pairs are pinned as LITERALS: the losing records are
        no longer in the profile, so the choice cannot be re-derived from the
        corrected corpus, and a test that re-derived it from the document
        would only prove the document agrees with itself.
        """
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        recorded = sorted({(e.owner_username, e.survivor_id) for e in doc.entries})
        self.assertEqual(recorded, sorted(SITE_B_SURVIVORS))
        self.assertEqual(len(recorded), 13)
        # One survivor per group, and no group split across two of them.
        by_owner_sha = {}
        for e in doc.entries:
            by_owner_sha.setdefault((e.owner_username, e.retired_sha256),
                                    set()).add(e.survivor_id)
        for key, survs in by_owner_sha.items():
            with self.subTest(group=key[0]):
                self.assertEqual(len(survs), 1,
                                 "one group of identical bytes was retired onto "
                                 "two different survivors")
        self.assertEqual(len(by_owner_sha), 13)
        ids = {a["id"] for a in json.loads(
            (PROFILES / "studio-b.assets.json").read_text(encoding="utf-8"))}
        self.assertEqual(doc.survivor_ids - ids, set(),
                         "a survivor is not in the profile; a retirement with no "
                         "survivor is a deletion")
        self.assertEqual(doc.retired_ids & ids, set(),
                         "a retired record is still in the profile")

    def test_site_b_profile_holds_the_corrected_count_and_dev_is_the_same_bytes(self):
        """1306 records, 22 of which could never hold a row, so 1284.

        `dev.assets.json` is a byte alias of `studio-b.assets.json`, so the
        correction has to reach it too or the dev corpus keeps seeding the
        records the app cannot hold.
        """
        b = (PROFILES / "studio-b.assets.json").read_bytes()
        d = (PROFILES / "dev.assets.json").read_bytes()
        self.assertEqual(len(json.loads(b.decode("utf-8"))), 1284)
        self.assertEqual(b, d, "dev.assets.json is not the byte alias it claims")

    def test_no_committed_site_b_post_names_a_retired_id(self):
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        for stem in ("studio-b", "dataset"):
            posts = json.loads((PROFILES / f"{stem}.posts.json")
                               .read_text(encoding="utf-8"))
            named = [p["id"] for p in posts
                     if set(p.get("asset_ids") or ()) & doc.retired_ids]
            with self.subTest(posts=stem):
                self.assertEqual(named, [])

    def test_site_b_substitutions_are_the_committed_membership(self):
        """25 posts, 132 members to 108, none emptied, no duplicate member.

        ⛔ A POST IS NEVER LEFT WITH A MEMBER IT DOES NOT HAVE, and the
        substitution is checked against the COMMITTED posts rather than
        against the document's own arithmetic.
        """
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        posts = {p["id"]: p for p in json.loads(
            (PROFILES / "studio-b.posts.json").read_text(encoding="utf-8"))}
        subs = [s for e in doc.entries for s in e.post_substitutions]
        self.assertEqual(len(subs), 25)
        self.assertEqual(len({s.post_id for s in subs}), 25,
                         "a post is substituted twice")
        self.assertEqual(sum(len(s.old_members) for s in subs), 132)
        self.assertEqual(sum(len(s.new_members) for s in subs), 108)
        ids = {a["id"] for a in json.loads(
            (PROFILES / "studio-b.assets.json").read_text(encoding="utf-8"))}
        for s in subs:
            with self.subTest(post=s.post_id):
                self.assertTrue(s.new_members, "a post was emptied")
                self.assertEqual(len(set(s.new_members)), len(s.new_members),
                                 "the substitution created a duplicate member")
                # The post is APPLIED: it sits at its applied id holding
                # exactly the documented new membership, order included.
                post = posts.get(s.applied_id)
                self.assertIsNotNone(post, f"{s.applied_id} is not a committed post")
                self.assertEqual(tuple(post["asset_ids"]), s.new_members)
                if s.new_post_id:
                    self.assertNotIn(s.post_id, posts,
                                     "the pre-migration id is still a post")
                # Every member resolves to a live record.
                self.assertEqual(set(s.new_members) - ids, set())
                # Unrelated members keep their relative order.
                kept = [m for m in s.old_members if m in set(s.new_members)]
                self.assertEqual(kept, [m for m in s.new_members if m in kept])

    def test_site_b_membership_derived_post_ids_moved_and_chained(self):
        """12 ids move and 13 stay, because the kinds differ.

        `asset_group` derives its id from the source group_id, so a member
        swap does not move it. `project_sprint` and `team_roundup` derive
        theirs from MEMBERSHIP, so they do. ⛔ Each move is COMPOSED onto the
        existing chain rather than appended: the published site holds the id
        from the earlier migration, so a new row would make that earlier hop
        look like a deleted record.
        """
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        posts = {p["id"]: p for p in json.loads(
            (PROFILES / "studio-b.posts.json").read_text(encoding="utf-8"))}
        subs = [s for e in doc.entries for s in e.post_substitutions]
        moved = [s for s in subs if s.new_post_id]
        stayed = [s for s in subs if not s.new_post_id]
        self.assertEqual(len(moved), 12)
        self.assertEqual(len(stayed), 13)
        self.assertEqual({posts[s.post_id]["post_kind"] for s in stayed},
                         {"asset_group"})
        from collections import Counter
        self.assertEqual(
            Counter(posts[s.applied_id]["post_kind"] for s in moved),
            Counter({"project_sprint": 7, "team_roundup": 5}))
        self.assertEqual(doc.moved_post_ids, {s.post_id for s in moved})
        # Each moved id is the one its own content derives, and the move is
        # recorded in the accumulating migration document as a COMPOSED hop.
        mig = json.loads((UPGRADES / "post-id-migration.studio-b.json")
                         .read_text(encoding="utf-8"))
        self.assertEqual(len(mig["moves"]), 336)
        final = {m["new_id"] for m in mig["moves"]}
        intermediate = {m["old_id"] for m in mig["moves"]}
        for s in moved:
            with self.subTest(post=s.applied_id):
                self.assertEqual(mpi.derived_id(posts[s.applied_id]), s.applied_id)
                self.assertIn(s.applied_id, final,
                              "the post-collapse id is in no migration row, so a "
                              "publish cannot tell the move from a deletion")
                self.assertNotIn(s.post_id, intermediate,
                                 "the pre-collapse id was left as a standalone "
                                 "hop instead of being composed away")
        # Every row still points at a post that exists.
        self.assertEqual(final - set(posts), set())

    def test_site_b_layer_a2_verdict_is_APPLIED_for_every_object(self):
        """LAYER A2 on the real committed site_b data: every asset and every
        post is Applied, so a re-run converts nothing and a third state has
        already raised.

        ⛔ `evaluate` RAISES on a third state, so this also proves the
        corrected profile and posts are not in some half-applied shape that a
        count comparison would read as settled.
        """
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        profile = json.loads(
            (PROFILES / "studio-b.assets.json").read_text(encoding="utf-8"))
        posts = json.loads(
            (PROFILES / "studio-b.posts.json").read_text(encoding="utf-8"))
        result = ac.evaluate(profile, posts, doc)
        self.assertEqual(len(result.states), 22)
        self.assertEqual(result.pending, 0)
        self.assertEqual(result.pending_assets, 0)
        self.assertEqual(result.pending_posts, 0)
        for st in result.states:
            with self.subTest(retired=st.entry.retired_id):
                self.assertEqual(st.asset_state, ac.APPLIED)
                self.assertEqual(set(st.post_states.values()) or {ac.APPLIED},
                                 {ac.APPLIED})
        # Applied is idempotent: applying again removes and rewrites nothing.
        again = ac.apply_collapse(profile, posts, doc)
        self.assertEqual(
            (again.records_removed, again.posts_rewritten, again.posts_renamed),
            (0, 0, 0))
        self.assertEqual(len(profile), 1284)
        self.assertEqual(len(posts), 767)

    def test_site_b_a_changed_retired_record_refuses_before_any_deletion(self):
        """⛔ A STALE DOCUMENT MUST NEVER DELETE DATA THAT MOVED. Put one
        retired record back with a single field changed and the document must
        refuse it as a third state rather than retire it again."""
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        profile = json.loads(
            (PROFILES / "studio-b.assets.json").read_text(encoding="utf-8"))
        posts = json.loads(
            (PROFILES / "studio-b.posts.json").read_text(encoding="utf-8"))
        entry = doc.entries[0]
        revived = json.loads(json.dumps(entry.retired_record))
        revived["title"] = "changed under the document"
        profile.append(revived)
        with self.assertRaises(ac.CollapseError) as cm:
            ac.evaluate(profile, posts, doc)
        self.assertIn("not the retired_record this document authorises",
                      str(cm.exception))
        # Nothing was removed: the refusal happens before any mutation.
        self.assertEqual(len(profile), 1285)

    def test_site_b_losses_are_the_reviewed_enumeration(self):
        """207 losses over 22 entries: 32 retired-only and 175 differing,
        over 19 distinct paths, and every one equal to a recomputation."""
        doc = ac.load_collapse_document(UPGRADES / "asset-collapse.studio-b.json",
                                        profile_name="studio-b.assets.json")
        by_id = {a["id"]: a for a in json.loads(
            (PROFILES / "studio-b.assets.json").read_text(encoding="utf-8"))}
        total = retired_only = differing = 0
        paths = set()
        for e in doc.entries:
            with self.subTest(retired=e.retired_id):
                self.assertIn(e.survivor_id, by_id)
                self.assertTrue(ac.losses_equal(
                    e.acknowledged_losses,
                    ac.recompute_losses(e.retired_record, by_id[e.survivor_id])))
            for loss in e.acknowledged_losses:
                total += 1
                paths.add(loss["path"])
                if "survivor_value" in loss:
                    differing += 1
                else:
                    retired_only += 1
        self.assertEqual(total, 207)
        self.assertEqual(retired_only, 32)
        self.assertEqual(differing, 175)
        self.assertEqual(len(paths), 19)
        # ⛔ AT LEAST ONE REAL EMPTY-VALUED LOSS. A guard that dropped falsy
        # retired values would still report 206 of these and read green.
        empties = [loss for e in doc.entries for loss in e.acknowledged_losses
                   if loss["retired_value"] in ("", [], {}, None)
                   or loss["retired_value"] is False]
        self.assertGreaterEqual(len(empties), 1)


class TestRetirementAwareBalanceDocuments(unittest.TestCase):
    """The historical documents are NOT rewritten when a record retires.

    `balance-assets.site_a.json` is the record of what the balance pass
    emitted, and editing it to make a check pass would turn evidence into
    bookkeeping. The two tests that read it become retirement-aware
    instead, and each asserts that every id it skipped is DOCUMENTED, so
    the exemption cannot quietly grow.
    """

    def _doc(self):
        return ac.load_collapse_document(UPGRADES / "asset-collapse.studio-a.json",
                                         profile_name="studio-a.assets.json")

    def test_the_balance_document_still_holds_the_retired_row(self):
        rows = {r["id"] for r in json.loads(
            (UPGRADES / "balance-assets.site_a.json").read_text(encoding="utf-8"))}
        self.assertEqual(self._doc().retired_ids - rows, set(),
                         "history was rewritten; the balance document is the "
                         "record of what the pass emitted")

    def test_the_reconcile_document_still_holds_the_retired_fill(self):
        doc = json.loads((UPGRADES / "manifest-reconcile.site_a.json")
                         .read_text(encoding="utf-8"))
        fills = {e["id"] for e in doc.get("fill", ())}
        self.assertEqual(self._doc().retired_ids - fills, set())

    def test_a_retired_fill_is_classed_retired_and_not_unknown(self):
        retired = _collapse_rec(RETIRED_ID)
        profile = [_collapse_rec(SURVIVOR_ID)]
        doc = {"fill": [{"id": RETIRED_ID, "mature": False},
                        {"id": "99999999-9999-9999-9999-999999999999",
                         "mature": False}]}
        _, _, _, unknown, retired_ids = up.apply_manifest_reconcile(
            profile, doc, frozenset({RETIRED_ID}))
        self.assertEqual(retired_ids, [RETIRED_ID])
        self.assertEqual(unknown, ["99999999-9999-9999-9999-999999999999"])

    def test_a_merged_retired_row_is_counted_apart_from_ordinary_drift(self):
        profile = [_collapse_rec(SURVIVOR_ID)]
        added = [_collapse_rec(RETIRED_ID), _collapse_rec(OTHER_ID)]
        n, _, n_retired = up.merge_added(profile, added, frozenset({RETIRED_ID}))
        self.assertEqual((n, n_retired), (2, 1))
        self.assertIn(RETIRED_ID, {a["id"] for a in profile},
                      "the record must still ENTER, so the collapse stage can "
                      "authenticate it before removing it")

    def test_a_moved_post_id_is_not_resurrected_by_the_historical_document(self):
        posts = [{"id": POST_ID_2, "asset_ids": [SURVIVOR_ID]}]
        added = [{"id": POST_ID, "asset_ids": [RETIRED_ID]},
                 {"id": "deadbeef-0000-0000-0000-000000000000", "asset_ids": []}]
        n, n_moved = up.merge_posts(posts, added, frozenset({POST_ID}))
        self.assertEqual((n, n_moved), (1, 1))
        self.assertNotIn(POST_ID, {p["id"] for p in posts})


class TestApplyUpgradeCollapseStage(unittest.TestCase):
    """W5b and W5c, driven through the real CLI.

    The fixtures reproduce the shape the live corpus is in: historical
    balance documents that would re-add the retired record and re-add the
    affected post, a reconcile fill naming it, and a curation entry whose
    `pipeline_members` digest describes the CORRECTED membership.
    """

    RETIRED = _collapse_rec(RETIRED_ID, archive_state="draft")
    SURVIVOR = _collapse_rec(SURVIVOR_ID)
    OTHER = _collapse_rec(OTHER_ID, src_sha=SRC_SHA_2)

    def _world(self, d: Path, *, applied: bool, doc=None, curation_digest=None,
               extra_post=None, retired_record=None):
        profiles = d / "profiles"
        upgrades = d / "upgrades"
        profiles.mkdir(parents=True, exist_ok=True)
        upgrades.mkdir(parents=True, exist_ok=True)

        old_members = [OTHER_ID, RETIRED_ID]
        new_members = [OTHER_ID, SURVIVOR_ID]
        sub = _sub(POST_ID, old_members, new_members, curation_pipeline_members={
            "old": up._members_digest(old_members),
            "new": up._members_digest(new_members)})
        # ⚠️ THE DOCUMENT'S COPY IS THE RECORD AS THE WHOLE PIPELINE
        # PRODUCES IT, reconcile fills included. That is what the live
        # document holds, and it is what makes the fixture faithful: the
        # balance row below carries no `mature`, the reconcile fill adds
        # it, and the stage compares against the post-fill record.
        post_fill = dict(json.loads(json.dumps(self.RETIRED)), mature=False)
        entry = _collapse_entry(retired_record or post_fill, self.SURVIVOR,
                                post_subs=[sub])
        if retired_record is not None:
            entry["acknowledged_losses"] = ac.recompute_losses(
                retired_record, self.SURVIVOR)
        (upgrades / "asset-collapse.studio-a.json").write_text(
            json.dumps(doc if doc is not None else _collapse_doc([entry])),
            encoding="utf-8")

        profile = [json.loads(json.dumps(r)) for r in (self.OTHER, self.SURVIVOR)]
        # The curated date is already on the post, so the curation pass
        # is a no-op and the only drift term this fixture can fire is the
        # one under test.
        posts = [{"id": POST_ID, "asset_ids": list(new_members),
                  "created_at": "2026-06-01T01:07:00Z"}]
        if not applied:
            profile.append(json.loads(json.dumps(self.RETIRED)))
            posts[0]["asset_ids"] = list(old_members)
        if extra_post is not None:
            posts.append(extra_post)
        # Written through the tool's own serialiser, so a byte-for-byte
        # assertion below measures CONTENT and not indentation.
        up.dump(profiles / "studio-a.assets.json", profile)
        up.dump(profiles / "studio-a.posts.json", posts)

        # The historical documents, exactly as the live corpus keeps them.
        (upgrades / "kenney-hq-replacements.site_a.json").write_text("[]",
                                                                     encoding="utf-8")
        (upgrades / "balance-assets.site_a.json").write_text(
            json.dumps([json.loads(json.dumps(self.RETIRED))]), encoding="utf-8")
        (upgrades / "balance-posts.site_a.json").write_text(
            json.dumps([{"id": POST_ID, "asset_ids": list(old_members)}]),
            encoding="utf-8")
        (upgrades / "manifest-reconcile.site_a.json").write_text(
            json.dumps({"added": [], "fill": [{"id": RETIRED_ID, "mature": False}]}),
            encoding="utf-8")
        curate = [{"id": POST_ID,
                   "pipeline_members": curation_digest or up._members_digest(new_members),
                   "created_at": "2026-06-01T01:07:00Z"}]
        if extra_post is not None:
            curate.append({"id": extra_post["id"],
                           "pipeline_members": up._members_digest([OTHER_ID]),
                           "created_at": extra_post.get("created_at")})
        (upgrades / "post-curation.site_a.json").write_text(
            json.dumps({"curate": curate}), encoding="utf-8")
        return profiles, upgrades

    def _run(self, profiles: Path, upgrades: Path, *extra):
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
             "--site", "site_a", "--upgrades", str(upgrades),
             "--profile", str(profiles / "studio-a.assets.json"),
             "--posts", str(profiles / "studio-a.posts.json"), *extra],
            capture_output=True, text=True)

    @staticmethod
    def _hashes(profiles: Path):
        return {p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                for p in sorted(profiles.iterdir())}

    ADVISORY = "membership has moved since the curation was recorded"

    def test_a_fresh_pending_assembly_is_corrected_with_no_curation_advisory(self):
        """W5c.1. The stage runs BEFORE the curation, so the digest is
        compared against the corrected membership and this deliberate
        substitution does not print the advisory that exists to flag the
        UNdocumented kind."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False)
            r = self._run(profiles, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            profile = json.loads((profiles / "studio-a.assets.json").read_text())
            posts = json.loads((profiles / "studio-a.posts.json").read_text())
            self.assertNotIn(RETIRED_ID, {a["id"] for a in profile})
            self.assertEqual(posts[0]["asset_ids"], [OTHER_ID, SURVIVOR_ID])
            self.assertNotIn(f"{POST_ID}: {self.ADVISORY}", r.stderr)

    def test_an_applied_profile_is_a_no_op_with_no_curation_advisory(self):
        """W5c.2."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=True)
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(self._hashes(profiles), before)
            self.assertNotIn(f"{POST_ID}: {self.ADVISORY}", r.stderr)

    def test_a_changed_retired_record_fails_before_any_deletion(self):
        """W5c.3."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False)
            profile = json.loads((profiles / "studio-a.assets.json").read_text())
            for a in profile:
                if a["id"] == RETIRED_ID:
                    a["title"] = "somebody edited this"
            up.dump(profiles / "studio-a.assets.json", profile)
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("not the retired_record this document authorises", r.stderr)
            self.assertEqual(self._hashes(profiles), before)
            self.assertIn(RETIRED_ID, {a["id"] for a in json.loads(
                (profiles / "studio-a.assets.json").read_text())})

    def test_an_order_only_permutation_fails_and_writes_nothing(self):
        """W5c.4, the order half."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False)
            posts = json.loads((profiles / "studio-a.posts.json").read_text())
            posts[0]["asset_ids"] = [RETIRED_ID, OTHER_ID]
            up.dump(profiles / "studio-a.posts.json", posts)
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("order included", r.stderr)
            self.assertEqual(self._hashes(profiles), before)

    def test_an_extra_member_fails_and_writes_nothing(self):
        """W5c.4, the extra-member half."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False)
            posts = json.loads((profiles / "studio-a.posts.json").read_text())
            posts[0]["asset_ids"] = [OTHER_ID, RETIRED_ID, SURVIVOR_ID]
            up.dump(profiles / "studio-a.posts.json", posts)
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertNotEqual(r.returncode, 0)
            self.assertEqual(self._hashes(profiles), before)

    def test_two_consecutive_checks_on_the_applied_state_are_clean(self):
        """W5c.5 and W5: the historical balance document still holds the
        retired row, and the gate still comes clean twice."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=True)
            for _ in range(2):
                r = self._run(profiles, upgrades, "--check")
                self.assertEqual(r.returncode, 0, r.stderr)
                self.assertIn("OK: profile already reflects the upgrade", r.stderr)
            rows = json.loads((upgrades / "balance-assets.site_a.json").read_text())
            self.assertEqual([x["id"] for x in rows], [RETIRED_ID])

    def test_unrelated_membership_drift_still_produces_the_advisory(self):
        """W5c.6. The fix is not a broad waiver: a different curated post
        whose membership moved for reasons the document does not name
        still gets the existing advisory."""
        other_post = {"id": POST_ID_2, "asset_ids": [SURVIVOR_ID],
                      "created_at": "2026-06-02T01:07:00Z"}
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False,
                                             extra_post=other_post)
            r = self._run(profiles, upgrades)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn(f"{POST_ID_2}: {self.ADVISORY}", r.stderr)
            self.assertNotIn(f"{POST_ID}: {self.ADVISORY}", r.stderr)

    def test_a_malformed_document_stops_the_write(self):
        """W5b. On dev the document is unknown, the run completes and
        exits 0 with the retired record written. Here it refuses before
        any mutation and leaves every file byte-identical."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False)
            doc = json.loads((upgrades / "asset-collapse.studio-a.json").read_text())
            doc["collapse"][0]["acknowledged_losses"] = \
                doc["collapse"][0]["acknowledged_losses"][:-1]
            (upgrades / "asset-collapse.studio-a.json").write_text(json.dumps(doc))
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("acknowledged_losses", r.stderr)
            self.assertEqual(self._hashes(profiles), before)

    def test_an_unparseable_document_stops_the_write(self):
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False)
            (upgrades / "asset-collapse.studio-a.json").write_text("{nope")
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("unreadable", r.stderr)
            self.assertEqual(self._hashes(profiles), before)

    def test_a_document_for_another_profile_stops_the_write(self):
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=False)
            doc = json.loads((upgrades / "asset-collapse.studio-a.json").read_text())
            doc["profile"] = "studio-b.assets.json"
            (upgrades / "asset-collapse.studio-a.json").write_text(json.dumps(doc))
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertNotEqual(r.returncode, 0)
            self.assertEqual(self._hashes(profiles), before)

    def test_a_curation_entry_that_re_adds_a_retired_id_is_a_problem(self):
        """Curation CAN write membership, and it runs after the stage.
        The post-condition is what turns "no entry does today" into a
        property of the pipeline."""
        with tempfile.TemporaryDirectory() as t:
            profiles, upgrades = self._world(Path(t), applied=True)
            doc = json.loads((upgrades / "post-curation.site_a.json").read_text())
            doc["curate"][0]["asset_ids"] = [OTHER_ID, RETIRED_ID]
            (upgrades / "post-curation.site_a.json").write_text(json.dumps(doc))
            before = self._hashes(profiles)
            r = self._run(profiles, upgrades)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("names retired asset", r.stderr)
            self.assertEqual(self._hashes(profiles), before)

    def test_the_committed_profiles_pass_the_retirement_gate_twice(self):
        """W5, on the real corpus: two consecutive --check runs, both
        sites, with every historical document untouched."""
        for site, stem in (("site_a", "studio-a"), ("site_b", "studio-b")):
            for run in (1, 2):
                r = subprocess.run(
                    [sys.executable, str(SCRIPTS / "apply_upgrade.py"),
                     "--site", site, "--upgrades", str(UPGRADES),
                     "--profile", str(PROFILES / f"{stem}.assets.json"),
                     "--posts", str(PROFILES / f"{stem}.posts.json"), "--check"],
                    capture_output=True, text=True)
                with self.subTest(site=site, run=run):
                    self.assertEqual(r.returncode, 0, r.stderr)
                    self.assertIn("OK: profile already reflects the upgrade", r.stderr)


class TestPublishAuthenticatesRetirement(unittest.TestCase):
    """LAYER B (W4, W7): the produced files are hashed under the source
    roots, and only then may the guard report COLLAPSED_RECORD.

    The fixture's "produced files" are small synthetic blobs, so this runs
    on a machine with no pack and no pool. What it exercises is the RULE:
    the two files must hash identically to each other in this build and
    equal the document's recorded value.
    """

    BYTES = b"produced-bytes"

    def _world(self, d: Path, *, retired_bytes=None, mat=None, src_profile=None,
               dest_records=None, stage_retired=True, doc=None):
        root = Path(d)
        for sub in ("profiles", "upgrades", "local", "internet", "hq", "dest"):
            (root / sub).mkdir(parents=True, exist_ok=True)
        (root / "local" / "metadata.csv").write_text("file_path\n", encoding="utf-8")

        retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        survivor = _collapse_rec(SURVIVOR_ID)
        mat_hash = mat or hashlib.sha256(self.BYTES).hexdigest()
        entry = _collapse_entry(retired, survivor, mat=mat_hash)
        (root / "upgrades" / "asset-collapse.studio-a.json").write_text(
            json.dumps(doc if doc is not None else _collapse_doc([entry])),
            encoding="utf-8")

        profile = src_profile if src_profile is not None else [survivor]
        (root / "profiles" / "studio-a.assets.json").write_text(
            json.dumps(profile), encoding="utf-8")
        (root / "profiles" / "studio-a.posts.json").write_text(
            json.dumps([{"id": POST_ID, "asset_ids": [SURVIVOR_ID]}]),
            encoding="utf-8")

        (root / "hq" / survivor["source_path"]).write_bytes(self.BYTES)
        (root / "hq" / retired["source_path"]).write_bytes(
            self.BYTES if retired_bytes is None else retired_bytes)

        dest = root / "dest"
        (dest / "MANIFEST.json").write_text(
            json.dumps(dest_records if dest_records is not None
                       else [survivor, retired]), encoding="utf-8")
        (dest / "posts.json").write_text(
            json.dumps([{"id": POST_ID, "asset_ids": [SURVIVOR_ID]}]),
            encoding="utf-8")
        for rec, body in ((survivor, self.BYTES), (retired, self.BYTES)):
            if rec is retired and not stage_retired:
                continue
            p = dest / rec["file_path"]
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(body)
        return root

    def _run(self, root: Path, *extra):
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "populate_archive.py"),
             "--local-source", str(root / "local"),
             "--internet-source", str(root / "internet"),
             "--hq-source", str(root / "hq"),
             "--profile", str(root / "profiles" / "studio-a.assets.json"),
             "--posts", str(root / "profiles" / "studio-a.posts.json"),
             "--dest", str(root / "dest"), *extra],
            capture_output=True, text=True)

    @staticmethod
    def _tree(root: Path):
        return {p.relative_to(root).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                for p in sorted(root.rglob("*")) if p.is_file()}

    def test_an_authenticated_retirement_reports_collapsed_and_loses_nothing(self):
        """W4 green: exactly one COLLAPSED_RECORD, 0 losses."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn(f"COLLAPSED_RECORD {RETIRED_ID} -> {SURVIVOR_ID}", r.stderr)
            self.assertNotIn("MISSING_RECORD", r.stderr)
            self.assertIn("nothing at the destination would be lost", r.stderr)

    def test_no_document_leaves_the_destination_only_id_a_loss(self):
        """Absence is never permission."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            (root / "upgrades" / "asset-collapse.studio-a.json").unlink()
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn(f"MISSING_RECORD {RETIRED_ID}", r.stderr)

    def test_produced_files_that_differ_refuse(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d), retired_bytes=b"different artwork")
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn("NOT identical in this build", r.stderr)
            self.assertIn("diagnostic", r.stderr.lower())

    def test_a_produced_hash_that_disagrees_with_the_document_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d), mat="d" * 64)
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn("materialized_sha256", r.stderr)
            self.assertIn("re-measure", r.stderr)

    def test_a_survivor_absent_from_the_source_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d), src_profile=[])
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn("is not in the source profile", r.stderr)

    def test_a_retired_id_still_in_the_source_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d), src_profile=[
                _collapse_rec(SURVIVOR_ID),
                _collapse_rec(RETIRED_ID, archive_state="draft")])
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn("was never applied to it", r.stderr)

    def test_a_stale_destination_predecessor_is_a_missing_record(self):
        """The document describes a record the destination no longer
        holds, so it is not evidence about the record it does hold."""
        with tempfile.TemporaryDirectory() as d:
            stale = _collapse_rec(RETIRED_ID, archive_state="draft")
            stale["title"] = "an edit nobody enumerated"
            root = self._world(Path(d), dest_records=[_collapse_rec(SURVIVOR_ID), stale])
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn(f"MISSING_RECORD {RETIRED_ID}", r.stderr)

    def test_an_unusable_document_refuses_rather_than_reading_as_absent(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            (root / "upgrades" / "asset-collapse.studio-a.json").write_text("{nope")
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn("not overridable by --allow-regression", r.stderr)

    def test_a_dry_run_reports_the_retired_path_and_deletes_nothing(self):
        """W7, the dry-run half."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            before = self._tree(root / "dest")
            r = self._run(root, "--dry-run")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("would remove retired path", r.stderr)
            self.assertEqual(self._tree(root / "dest"), before)

    def test_a_real_run_removes_exactly_the_retired_path(self):
        """W7, the real half."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            retired_rel = _collapse_rec(RETIRED_ID)["file_path"]
            before = set(self._tree(root / "dest"))
            r = self._run(root)
            self.assertEqual(r.returncode, 0, r.stderr)
            after = set(self._tree(root / "dest"))
            self.assertEqual(before - after, {retired_rel})

    def test_a_retired_path_with_the_wrong_bytes_refuses_and_deletes_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            retired_rel = _collapse_rec(RETIRED_ID)["file_path"]
            (root / "dest" / retired_rel).write_bytes(b"not what the document says")
            r = self._run(root)
            self.assertEqual(r.returncode, 1)
            self.assertIn("is not the one this document describes", r.stderr)
            self.assertTrue((root / "dest" / retired_rel).is_file())

    def test_a_retired_path_a_current_record_still_wants_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            retired_rel = _collapse_rec(RETIRED_ID)["file_path"]
            claimant = _collapse_rec("77777777-8888-9999-aaaa-bbbbbbbbbbbb",
                                     src_sha=SRC_SHA_2)
            claimant["file_path"] = retired_rel
            claimant["source_path"] = Path(retired_rel).name
            (root / "hq" / claimant["source_path"]).write_bytes(self.BYTES)
            (root / "profiles" / "studio-a.assets.json").write_text(
                json.dumps([_collapse_rec(SURVIVOR_ID), claimant]), encoding="utf-8")
            r = self._run(root)
            self.assertEqual(r.returncode, 1)
            self.assertIn("still the destination path of a record", r.stderr)
            self.assertTrue((root / "dest" / retired_rel).is_file())

    def test_the_override_locates_a_fixture_document(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            moved = root / "elsewhere.json"
            (root / "upgrades" / "asset-collapse.studio-a.json").rename(moved)
            r = self._run(root, "--dry-run", "--collapse-document", str(moved))
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("COLLAPSED_RECORD", r.stderr)

    def test_the_override_naming_no_file_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(Path(d))
            r = self._run(root, "--dry-run", "--collapse-document",
                          str(root / "nope.json"))
            self.assertEqual(r.returncode, 2)
            self.assertIn("not a file", r.stderr)


class TestManifestGuardCollapsedRecord(unittest.TestCase):
    """`COLLAPSED_RECORD` is produced ONLY from a mapping the caller has
    already source-authenticated, and it is never folded into another
    number."""

    def setUp(self):
        self.retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        self.survivor = _collapse_rec(SURVIVOR_ID)
        self.collapses = {RETIRED_ID: {"survivor_id": SURVIVOR_ID,
                                       "retired_record": self.retired}}

    def test_an_authenticated_retirement_is_not_a_loss(self):
        cmp = mg.compare([self.survivor], [self.survivor, self.retired],
                         "MANIFEST.json", collapses=self.collapses)
        self.assertEqual(cmp.losses, [])
        self.assertEqual(cmp.collapsed, [(RETIRED_ID, SURVIVOR_ID)])
        self.assertTrue(cmp.ok)

    def test_it_is_not_counted_as_an_addition_a_change_or_a_migration(self):
        cmp = mg.compare([self.survivor], [self.survivor, self.retired],
                         "MANIFEST.json", collapses=self.collapses)
        self.assertEqual(cmp.added, [])
        self.assertEqual(cmp.changes, [])
        self.assertEqual(cmp.migrated, [])
        self.assertEqual(cmp.records_collapsed, 1)

    def test_it_gets_its_own_report_line(self):
        cmp = mg.compare([self.survivor], [self.survivor, self.retired],
                         "MANIFEST.json", collapses=self.collapses,
                         collapse_source="asset-collapse.studio-a.json")
        report = mg.format_report(cmp)
        self.assertIn("collapsed: 1 record(s) retired onto a named survivor", report)
        self.assertIn(f"{mg.COLLAPSED_RECORD} {RETIRED_ID} -> {SURVIVOR_ID}", report)

    def test_a_destination_record_that_differs_falls_back_to_missing_record(self):
        stale = json.loads(json.dumps(self.retired))
        stale["field_values"]["rating"] = 5
        cmp = mg.compare([self.survivor], [self.survivor, stale],
                         "MANIFEST.json", collapses=self.collapses)
        self.assertEqual([x.kind for x in cmp.losses], [mg.MISSING_RECORD])
        self.assertEqual(cmp.collapsed, [])

    def test_without_the_mapping_the_record_is_a_loss(self):
        cmp = mg.compare([self.survivor], [self.survivor, self.retired],
                         "MANIFEST.json")
        self.assertEqual([x.kind for x in cmp.losses], [mg.MISSING_RECORD])

    def test_an_unrelated_missing_record_is_still_a_loss(self):
        other = _collapse_rec(OTHER_ID, src_sha=SRC_SHA_2)
        cmp = mg.compare([self.survivor], [self.survivor, self.retired, other],
                         "MANIFEST.json", collapses=self.collapses)
        self.assertEqual([x.record_id for x in cmp.losses], [OTHER_ID])


class TestVerifySiteRetirementExpectations(unittest.TestCase):
    """`collapses`, `retired_paths_absent` and `same_owner_same_bytes`."""

    def _site(self, d: Path, *, stage_retired=True):
        root = Path(d)
        (root / "profiles").mkdir(parents=True, exist_ok=True)
        (root / "upgrades").mkdir(parents=True, exist_ok=True)
        site = root / "site"
        retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        survivor = _collapse_rec(SURVIVOR_ID)
        (root / "upgrades" / "asset-collapse.studio-a.json").write_text(
            json.dumps(_collapse_doc([_collapse_entry(retired, survivor)])),
            encoding="utf-8")
        (root / "profiles" / "studio-a.assets.json").write_text(
            json.dumps([survivor]), encoding="utf-8")
        posts = [{"id": POST_ID, "asset_ids": [SURVIVOR_ID]}]
        (root / "profiles" / "studio-a.posts.json").write_text(
            json.dumps(posts), encoding="utf-8")
        site.mkdir(parents=True, exist_ok=True)
        (site / "MANIFEST.json").write_text(json.dumps([survivor]), encoding="utf-8")
        (site / "posts.json").write_text(json.dumps(posts), encoding="utf-8")
        for rec in ((survivor, retired) if stage_retired else (survivor,)):
            p = site / rec["file_path"]
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(b"produced-byte")
        return root

    def _verify(self, root: Path, expectations):
        return vs.verify(root / "profiles" / "studio-a.assets.json",
                         root / "profiles" / "studio-a.posts.json",
                         root / "site", expectations=expectations,
                         attributions=root / "nope.md")

    def test_the_collapse_count_is_checked(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._site(Path(d), stage_retired=False)
            rep = self._verify(root, {"collapses": 1})
            self.assertEqual([v.status for v in rep.verdicts if v.name == "collapses"],
                             [vs.PASS])
            rep = self._verify(root, {"collapses": 2})
            self.assertEqual([v.status for v in rep.verdicts if v.name == "collapses"],
                             [vs.FAIL])

    def test_a_retired_path_left_in_staging_fails(self):
        """The Kaggle-tree rule: the uploader hands the whole directory
        over, so a retired file still in staging is published."""
        with tempfile.TemporaryDirectory() as d:
            root = self._site(Path(d), stage_retired=True)
            rep = self._verify(root, {"retired_paths_absent": True})
            v = [x for x in rep.verdicts if x.name.startswith("retired paths")]
            self.assertEqual([x.status for x in v], [vs.FAIL])

    def test_a_clean_staging_tree_passes_the_retired_path_rule(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._site(Path(d), stage_retired=False)
            rep = self._verify(root, {"retired_paths_absent": True})
            v = [x for x in rep.verdicts if x.name.startswith("retired paths")]
            self.assertEqual([x.status for x in v], [vs.PASS])

    def test_same_owner_same_bytes_is_labelled_a_staged_state_diagnostic(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._site(Path(d), stage_retired=False)
            rep = self._verify(root, {"same_owner_same_bytes": 0})
            v = [x for x in rep.verdicts if x.name == "same_owner_same_bytes"][0]
            self.assertEqual(v.status, vs.PASS)
            self.assertIn("STAGED-STATE DIAGNOSTIC", v.detail)
            self.assertIn("not source authority", v.detail)

    def test_same_owner_same_bytes_counts_a_real_staged_group(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._site(Path(d), stage_retired=True)
            site = root / "site"
            retired = _collapse_rec(RETIRED_ID, archive_state="draft")
            manifest = json.loads((site / "MANIFEST.json").read_text())
            manifest.append(retired)
            (site / "MANIFEST.json").write_text(json.dumps(manifest), encoding="utf-8")
            groups = vs.staged_same_owner_same_bytes(manifest, site)
            self.assertEqual(len(groups), 1)
            self.assertEqual(sorted(next(iter(groups.values()))),
                             sorted([RETIRED_ID, SURVIVOR_ID]))

    def test_cross_owner_identical_bytes_are_not_grouped(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._site(Path(d), stage_retired=True)
            site = root / "site"
            other = _collapse_rec(RETIRED_ID, owner="chen.wei", archive_state="draft")
            manifest = json.loads((site / "MANIFEST.json").read_text()) + [other]
            self.assertEqual(vs.staged_same_owner_same_bytes(manifest, site), {})

    def test_an_unusable_document_fails_the_verifier(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._site(Path(d), stage_retired=False)
            (root / "upgrades" / "asset-collapse.studio-a.json").write_text("{nope")
            rep = self._verify(root, {"collapses": 1})
            v = [x for x in rep.verdicts if x.name == "collapse document"][0]
            self.assertEqual(v.status, vs.FAIL)

    def test_an_unknown_expectation_key_is_still_refused(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "e.json"
            p.write_text(json.dumps({"collapsed": 1}), encoding="utf-8")
            with self.assertRaises(ValueError):
                vs.load_expectations(p)


# ---------------------------------------------------------------------------
# #1319: the archive is the MAINTAINED DATASET for preserved roots
# ---------------------------------------------------------------------------
#
# The source dataset the profiles were built against
# (`/mnt/d/Projects/unraid_management/artist-alley_dataset`) has been
# permanently retired, and the maintained datasets are the published trees.
# For `local` the archive is the ONLY copy: measured on the committed
# profiles, 0 of 696 site_a and 0 of 552 site_b `local` records carry a
# `metadata.media_url` and 0 carry a `metadata.source_archive`.
#
# Everything below is synthetic. Nothing needs the archive share, the pack,
# the pool or the network, because the required guard suite runs on a runner that
# has none of them. The fixtures REPRODUCE the published files' awkward
# shape rather than importing them: CRLF terminators and bare LFs inside
# quoted fields, both measured on the real site_b `metadata.csv` (1,216 LF
# bytes against 1,206 logical rows).

# The 41 columns the published CSVs actually carry, in order, copied
# VERBATIM from site_a/metadata.csv on 2026-09-24. Held here so a
# fixture is the same shape as the artifact rather than a plausible
# imitation of it; only `file_path` is load-bearing for the transform.
CSV_FIELDS = (
    "asset_id,group_id,file_path,filename,title,description,team,project,"
    "franchise,source,attribution,artist,approver,status,pipeline_stage,"
    "version,revision_count,file_format,kind,file_size_bytes,polycount,"
    "texture_resolution,color_space,loop_seconds,runtime_seconds,"
    "engine_compatibility,target_platforms,license,usage_rights,"
    "confidentiality,naming_compliant,external_id,is_published,"
    "archived_reason,tags,group_size,rating,review_notes,created_at,"
    "updated_at,last_reviewed_at"
).split(",")


def _csv_bytes(paths, *, embed_newline_every=0, fields=CSV_FIELDS):
    """A metadata.csv in the published shape: CRLF terminators and, where
    asked, a BARE LF inside a quoted description, which is what site_b
    actually ships and what a parse-and-rewrite would silently normalise."""
    out = bytearray()
    out += (",".join(fields) + "\r\n").encode()
    for i, fp in enumerate(paths):
        row = [""] * len(fields)
        row[fields.index("asset_id")] = f"a{i:05d}"
        row[fields.index("file_path")] = fp
        row[fields.index("filename")] = fp.rsplit("/", 1)[-1]
        row[fields.index("title")] = f"plate {i}"
        desc = f"row {i}"
        if embed_newline_every and i % embed_newline_every == 0:
            desc = f"row {i},\nsecond line"
        row[fields.index("description")] = desc
        cells = []
        for c in row:
            cells.append(f'"{c}"' if ("," in c or "\n" in c or '"' in c) else c)
        out += (",".join(cells) + "\r\n").encode()
    return bytes(out)


def _paths(n, prefix="images/local"):
    return [f"{prefix}/p{i:05d}.png" for i in range(n)]


class TestAliasRefusal(unittest.TestCase):
    """R2. A source root that IS the destination makes the destination its
    own evidence, which is the exact failure ADR 0097 exists to prevent.

    ⛔ EQUALITY IS NOT THE ONLY WAY TO ALIAS. A "source" that CONTAINS the
    destination, or sits inside it, reads bytes the run is about to write.
    And the comparison is on RESOLVED paths: `..`, a symlink and a trailing
    slash all defeat a string prefix, and `/a/bc` starts with `/a/b` as text
    while being nowhere near it.
    """

    def test_equal_paths_are_refused(self):
        bad = pres.alias_refusals({"--local-source": "/data/site_a",
                                   "--dest": "/data/site_a"})
        self.assertEqual(len(bad), 1)
        self.assertIn("same path", bad[0])

    def test_a_source_containing_the_destination_is_refused(self):
        bad = pres.alias_refusals({"--local-source": "/data",
                                   "--dest": "/data/site_a"})
        self.assertEqual(len(bad), 1)
        self.assertIn("CONTAINS", bad[0])

    def test_a_destination_containing_a_source_is_refused(self):
        bad = pres.alias_refusals({"--pack-source": "/data/site_a/packs",
                                   "--dest": "/data/site_a"})
        self.assertEqual(len(bad), 1)
        self.assertIn("CONTAINS", bad[0])

    def test_dot_dot_and_a_trailing_slash_do_not_defeat_it(self):
        bad = pres.alias_refusals({"--local-source": "/data/x/../site_a/",
                                   "--dest": "/data/site_a"})
        self.assertEqual(len(bad), 1)

    def test_a_symlink_does_not_defeat_it(self):
        with tempfile.TemporaryDirectory() as d:
            real = Path(d) / "site_a"
            real.mkdir()
            link = Path(d) / "published"
            link.symlink_to(real)
            self.assertEqual(len(pres.alias_refusals(
                {"--local-source": link, "--dest": real})), 1)

    def test_a_shared_prefix_that_is_not_containment_is_allowed(self):
        """⛔ THE TRAP. `/a/bc` starts with `/a/b` as text. A prefix match
        would refuse a perfectly separate tree, and a guard that refuses
        correct configurations gets switched off."""
        self.assertEqual(pres.alias_refusals({"--local-source": "/a/bc",
                                              "--dest": "/a/b"}), [])

    def test_the_snapshot_the_manifest_and_the_documents_are_all_covered(self):
        for label in ("--frozen-snapshot", "--snapshot-manifest",
                      "--csv-transform", "--internet-source", "--hq-source"):
            with self.subTest(label=label):
                bad = pres.alias_refusals({label: "/data/site_a/x",
                                           "--dest": "/data/site_a"})
                self.assertEqual(len(bad), 1, label)

    def test_two_read_only_source_roots_may_be_one_tree(self):
        """The only exemption, and it is read-vs-read. One dataset root
        serving two roots writes nothing; nothing is ever exempted from the
        comparison against the tree the run WRITES."""
        group = [{"--local-source", "--internet-source"}]
        self.assertEqual(pres.alias_refusals(
            {"--local-source": "/data/src", "--internet-source": "/data/src"},
            equal_ok_within=group), [])
        self.assertEqual(len(pres.alias_refusals(
            {"--local-source": "/data/src", "--internet-source": "/data/src",
             "--dest": "/data/src"}, equal_ok_within=group)), 2)

    def test_nesting_is_refused_even_inside_the_exempt_group(self):
        group = [{"--local-source", "--internet-source"}]
        self.assertEqual(len(pres.alias_refusals(
            {"--local-source": "/data/src", "--internet-source": "/data/src/i"},
            equal_ok_within=group)), 1)

    def test_a_none_path_is_simply_absent(self):
        self.assertEqual(pres.alias_refusals({"--hq-source": None,
                                              "--dest": "/data/site_a"}), [])


class TestMetadataCsvTransform(unittest.TestCase):
    """C1-C9. `metadata.csv` is preservation-owned but NOT exact-bytes.

    ⛔ NEITHER A WAIVER NOR A BASELINE. A retirement legitimately removes its
    row, so exact bytes would refuse the one correct change; a waiver would
    have permitted the HEADER-ONLY file the old regeneration actually wrote
    (0 of 907 site_a rows and 0 of 1,206 site_b rows matched its source-path
    map, because the published column already holds DESTINATION paths).
    """

    PROFILE = "studio-a.assets.json"

    def _binding(self, removals):
        """A synthetic collapse-document identity for a fixture transform.

        The real one comes from `preserved_archive.collapse_binding`; these
        tests exercise the CSV machinery, so they state an equivalent identity
        rather than building a whole collapse document for each case."""
        ids = sorted({rid for _p, rid in removals})
        return {"profile": self.PROFILE, "sha256": "b" * 64,
                "entries": len(ids), "retired_ids": ids}

    def _doc(self, blob, removals=(), *, collapse=True):
        removals = list(removals)
        with tempfile.TemporaryDirectory() as d:
            csvp = Path(d) / "metadata.csv"
            csvp.write_bytes(blob)
            return pres.build_csv_transform(
                csvp, removals=removals, profile=self.PROFILE,
                collapse=self._binding(removals) if collapse else None)

    # -- the byte-level filter -------------------------------------------

    def test_the_record_split_round_trips_a_published_shaped_file(self):
        """⚠️ CRLF AND BARE LFs INSIDE QUOTED FIELDS, both measured on the
        real site_b metadata.csv. A `csv`-module round trip re-quotes and
        re-terminates them, so "the retained rows are unchanged" would be
        false the first time it ran."""
        blob = _csv_bytes(_paths(1206), embed_newline_every=134)
        header, rows = pres.split_csv_records(blob)
        self.assertEqual(len(rows), 1206)
        self.assertEqual(header + b"".join(rows), blob)
        self.assertGreater(blob.count(b"\n"), len(rows) + 1,
                           "the fixture must actually embed bare newlines")
        self.assertTrue(blob.endswith(b"\r\n"))

    def test_a_truncated_quoted_field_refuses_rather_than_guessing(self):
        with self.assertRaises(pres.PreservedError):
            pres.split_csv_records(b'file_path\r\n"unclosed\r\n')

    def test_a_duplicate_file_path_refuses_before_anything_is_keyed_on_it(self):
        """⛔ CARDINALITY BEFORE KEYING. A removal is keyed on `file_path`,
        so a duplicate makes the row a retirement names ambiguous and could
        drop one nobody enumerated."""
        blob = _csv_bytes(["images/a.png", "images/a.png"])
        with self.assertRaises(pres.PreservedError) as cm:
            self._doc(blob)
        self.assertIn("more than one row", str(cm.exception))

    # -- C2: the documented transform -------------------------------------

    def test_the_documented_22_row_transform_passes(self):
        """C2. 1,206 -> 1,184, the site_b row count with 22 removals."""
        paths = _paths(1206)
        blob = _csv_bytes(paths, embed_newline_every=134)
        gone = paths[100:122]
        doc = self._doc(blob, [(p, f"rid-{i}") for i, p in enumerate(gone)])
        self.assertEqual(doc["original"]["data_rows"], 1206)
        self.assertEqual(doc["expected"]["data_rows"], 1184)
        self.assertEqual(len(doc["removals"]), 22)
        after, refusals = pres.transform_csv(blob, doc)
        self.assertEqual(refusals, [])
        self.assertEqual(pres.verify_csv_transform(doc, after), [])

    def test_c9_every_unrelated_row_survives_byte_identically_and_the_header_too(self):
        """C9, a PRESERVATION assertion rather than a regression: the retained
        bytes are the SAME bytes, not a re-rendering of the same values."""
        paths = _paths(200, prefix="images/local")
        blob = _csv_bytes(paths, embed_newline_every=7)
        header, rows = pres.split_csv_records(blob)
        gone = {paths[3], paths[7], paths[14]}
        doc = self._doc(blob, [(p, f"rid-{i}") for i, p in enumerate(sorted(gone))])
        after, _ = pres.transform_csv(blob, doc)
        a_header, a_rows = pres.split_csv_records(after)
        self.assertEqual(a_header, header)
        kept = [r for p, r in zip(paths, rows) if p not in gone]
        self.assertEqual(a_rows, kept)
        self.assertEqual(after, header + b"".join(kept))

    # -- C3-C8: every way verification must refuse -------------------------

    def test_c3_one_additional_row_removed_fails(self):
        paths = _paths(60)
        blob = _csv_bytes(paths)
        doc = self._doc(blob, [(paths[5], "rid-5")])
        after, _ = pres.transform_csv(blob, doc)
        h, rows = pres.split_csv_records(after)
        tampered = h + b"".join(rows[:10] + rows[11:])
        refusals = pres.verify_csv_transform(doc, tampered)
        self.assertTrue(refusals)
        self.assertTrue(any("data row(s)" in r for r in refusals))

    def test_c4_a_changed_retained_row_fails(self):
        paths = _paths(60)
        blob = _csv_bytes(paths)
        doc = self._doc(blob, [(paths[5], "rid-5")])
        after, _ = pres.transform_csv(blob, doc)
        h, rows = pres.split_csv_records(after)
        rows[20] = rows[20].replace(b"plate", b"PLATE")
        refusals = pres.verify_csv_transform(doc, h + b"".join(rows))
        self.assertTrue(any("BYTE-IDENTICALLY" in r for r in refusals), refusals)

    def test_c5_a_reordered_retained_row_fails_via_the_ordered_digest(self):
        """⛔ ORDER IS PART OF THE CLAIM. A digest over a SET would accept a
        permuted file, and a permuted metadata.csv is a changed artifact."""
        paths = _paths(60)
        blob = _csv_bytes(paths)
        doc = self._doc(blob, [(paths[5], "rid-5")])
        after, _ = pres.transform_csv(blob, doc)
        h, rows = pres.split_csv_records(after)
        rows[10], rows[11] = rows[11], rows[10]
        refusals = pres.verify_csv_transform(doc, h + b"".join(rows))
        self.assertTrue(any("REORDERED" in r for r in refusals), refusals)
        self.assertTrue(any("ordered digest" in r for r in refusals), refusals)

    def test_a_changed_header_fails(self):
        paths = _paths(20)
        blob = _csv_bytes(paths)
        doc = self._doc(blob)
        h, rows = pres.split_csv_records(blob)
        refusals = pres.verify_csv_transform(
            doc, h.replace(b"asset_id", b"assetid") + b"".join(rows))
        self.assertTrue(any("header" in r for r in refusals), refusals)

    def test_c8_a_documented_removal_that_never_happened_fails(self):
        paths = _paths(60)
        blob = _csv_bytes(paths)
        doc = self._doc(blob, [(paths[5], "rid-5")])
        refusals = pres.verify_csv_transform(doc, blob)
        self.assertTrue(any("still in the file" in r for r in refusals), refusals)
        self.assertTrue(any("PRE-OPERATION" in r for r in refusals), refusals)

    def test_a_wrong_whole_file_hash_fails_on_its_own(self):
        paths = _paths(20)
        blob = _csv_bytes(paths)
        doc = self._doc(blob)
        refusals = pres.verify_csv_transform(doc, blob + b"trailing junk\r\n")
        self.assertTrue(any("whole file hashes" in r for r in refusals), refusals)

    def test_r5_emptying_the_file_fails_where_a_baseline_could_not_see_it(self):
        """R5. On `dev` an emptied metadata.csv passed the preservation
        baseline, because the baseline never recorded the file at all."""
        paths = _paths(907)
        blob = _csv_bytes(paths)
        doc = self._doc(blob)
        header, _ = pres.split_csv_records(blob)
        refusals = pres.verify_csv_transform(doc, header)
        self.assertTrue(refusals)
        self.assertTrue(any("0 data row(s)" in r for r in refusals), refusals)

    # -- N boundaries ------------------------------------------------------

    def test_n0_a_zero_removal_transform_still_enforces_byte_equality(self):
        """N=0. ⛔ ABSENCE IS NOT PERMISSION. site_a's real transform is
        this one: 907 rows to 907, 0 removals, and its metadata.csv must
        stay byte-identical, an expectation to enforce rather than a case to
        skip."""
        blob = _csv_bytes(_paths(907))
        doc = self._doc(blob)
        self.assertEqual(doc["removals"], [])
        self.assertEqual(doc["expected"]["sha256"], doc["original"]["sha256"])
        self.assertEqual(pres.verify_csv_transform(doc, blob), [])
        h, rows = pres.split_csv_records(blob)
        self.assertTrue(pres.verify_csv_transform(doc, h + b"".join(rows[:-1])))

    def test_n1_one_documented_retirement(self):
        paths = _paths(10)
        blob = _csv_bytes(paths)
        doc = self._doc(blob, [(paths[4], "rid-4")])
        after, _ = pres.transform_csv(blob, doc)
        self.assertEqual(doc["expected"]["data_rows"], 9)
        self.assertEqual(pres.verify_csv_transform(doc, after), [])

    def test_a_retirement_whose_path_has_no_row_removes_nothing(self):
        """The ordinary `hq` case, and the reason site_a's real transform is
        a zero-removal one: the retired render has no CSV row at all."""
        blob = _csv_bytes(_paths(10))
        doc = self._doc(blob, [("images/kenney-hq/not-in-the-csv.png", "rid-x")])
        self.assertEqual(doc["removals"], [])
        self.assertEqual(doc["expected"]["data_rows"], 10)

    def test_two_retirements_may_not_name_one_row(self):
        paths = _paths(10)
        with self.assertRaises(pres.PreservedError) as cm:
            self._doc(_csv_bytes(paths), [(paths[1], "r1"), (paths[1], "r2")])
        self.assertIn("same row", str(cm.exception))

    def test_cross_owner_identical_bytes_stay_two_rows(self):
        """Legal, and never grouped: identity is per OWNER. Two rows with
        the same content under different paths are two rows before and two
        rows after."""
        blob = _csv_bytes(["images/a/plate.png", "images/b/plate.png"])
        doc = self._doc(blob)
        self.assertEqual(doc["expected"]["data_rows"], 2)
        self.assertEqual(pres.verify_csv_transform(doc, blob), [])

    # -- the document itself ----------------------------------------------

    def test_a_document_that_disagrees_with_itself_is_refused(self):
        doc = self._doc(_csv_bytes(_paths(10)))
        doc["expected"]["ordered_digest"] = "f" * 64
        with self.assertRaises(pres.PreservedError) as cm:
            pres.parse_csv_transform(doc, source="fixture")
        self.assertIn("disagrees with itself", str(cm.exception))

    def test_arithmetic_that_does_not_close_is_refused(self):
        doc = self._doc(_csv_bytes(_paths(10)))
        # the id has to be one the binding names, or the stray check fires
        # first; this case is about the row arithmetic and nothing else.
        doc["removals"] = [{"file_path": "x", "retired_id": "r-1"}]
        doc[pres.COLLAPSE_BINDING_KEY] = {"profile": self.PROFILE, "sha256": "b" * 64,
                                          "entries": 1, "retired_ids": ["r-1"]}
        with self.assertRaises(pres.PreservedError) as cm:
            pres.parse_csv_transform(doc, source="fixture")
        self.assertIn("arithmetic does not close", str(cm.exception))

    def test_a_removal_the_bound_document_does_not_hold_is_refused(self):
        """⛔ THE STRUCTURAL HALF OF THE BINDING. A removal naming a
        retired_id the bound document does not state cannot be a row that
        document authorised, whatever the transform's own arithmetic says."""
        paths = _paths(10)
        doc = self._doc(_csv_bytes(paths), [(paths[2], "r-2")])
        doc["removals"] = [{"file_path": paths[2], "retired_id": "someone-elses-id"}]
        with self.assertRaises(pres.PreservedError) as cm:
            pres.parse_csv_transform(doc, source="fixture")
        self.assertIn("retired_id the bound collapse document does not hold",
                      str(cm.exception))

    def test_a_transform_with_no_profile_or_binding_key_is_refused(self):
        """Absence is not an explicit null: a transform that says nothing
        about its authority is refused rather than read as unbound."""
        doc = self._doc(_csv_bytes(_paths(5)))
        for key, fragment in (("profile", "missing `profile`"),
                              (pres.COLLAPSE_BINDING_KEY,
                               f"missing `{pres.COLLAPSE_BINDING_KEY}`")):
            bad = json.loads(json.dumps(doc))
            bad.pop(key)
            with self.subTest(key=key):
                with self.assertRaises(pres.PreservedError) as cm:
                    pres.parse_csv_transform(bad, source="fixture")
                self.assertIn(fragment, str(cm.exception))

    def test_an_unsorted_or_duplicated_retirement_set_is_refused(self):
        doc = self._doc(_csv_bytes(_paths(5)))
        for ids, fragment in ((["b-2", "a-1"], "is not sorted"),
                              (["a-1", "a-1"], "names an id twice")):
            bad = json.loads(json.dumps(doc))
            bad[pres.COLLAPSE_BINDING_KEY] = {
                "profile": self.PROFILE, "sha256": "b" * 64,
                "entries": len(ids), "retired_ids": ids}
            with self.subTest(ids=ids):
                with self.assertRaises(pres.PreservedError) as cm:
                    pres.parse_csv_transform(bad, source="fixture")
                self.assertIn(fragment, str(cm.exception))

    def test_an_unreadable_document_raises_rather_than_reading_as_empty(self):
        with tempfile.TemporaryDirectory() as d:
            bad = Path(d) / "t.json"
            bad.write_text("{not json", encoding="utf-8")
            with self.assertRaises(pres.PreservedError):
                pres.load_csv_transform(bad)

    def test_a_document_built_from_other_bytes_refuses_rather_than_adapting(self):
        blob = _csv_bytes(_paths(10))
        doc = self._doc(blob)
        other = _csv_bytes(_paths(11))
        after, refusals = pres.transform_csv(other, doc)
        self.assertEqual(after, b"")
        self.assertTrue(any("not evidence about these bytes" in r
                            for r in refusals), refusals)

    def test_the_cli_refuses_an_out_inside_the_site_and_verifies_read_only(self):
        with tempfile.TemporaryDirectory() as d:
            site = Path(d) / "site"
            site.mkdir()
            (site / "metadata.csv").write_bytes(_csv_bytes(_paths(5)))
            before = hashlib.sha256((site / "metadata.csv").read_bytes()).hexdigest()
            trees = ["--snapshot", str(site), "--live-site", str(Path(d) / "live"),
                     "--staging", str(Path(d) / "staging")]
            with contextlib.redirect_stderr(io.StringIO()):
                # evidence inside the tree it describes is still refused
                self.assertEqual(pres.main(
                    ["csv-transform", *trees, "--no-collapse-document",
                     "--profile", self.PROFILE, "--out", str(site / "t.json")]), 2)
                out = Path(d) / "t.json"
                self.assertEqual(pres.main(
                    ["csv-transform", *trees, "--no-collapse-document",
                     "--profile", self.PROFILE, "--out", str(out)]), 0)
                self.assertEqual(pres.main(["verify-csv", "--transform", str(out),
                                            "--csv", str(site / "metadata.csv")]), 0)
            self.assertEqual(
                hashlib.sha256((site / "metadata.csv").read_bytes()).hexdigest(),
                before, "the emit and verify commands must not write under --site")


class TestGroupsCsvIsPreservationOwned(unittest.TestCase):
    """C6. `groups.csv` needs no transformation, so it gets the strictest
    rule: the bytes do not change.

    ⛔ `asset_count` IS NOT REINTERPRETED. It is an ORIGINAL-DATASET fact
    that already disagrees with the shipped subset. Measured on site_b,
    `grp-00219` states 8 and ships 3, `grp-00215` states 8 and ships 2, and
    262 of 1,047 rows disagree. "Correcting" it would overwrite a fact with
    a derivation. And no group loses its last shipped member: 0 of 1,047
    ship nothing, so a retirement never empties one.
    """

    def test_groups_csv_is_in_the_exact_preservation_set(self):
        self.assertIn(pres.GROUPS_NAME, vs.PRESERVED_NAMES)
        self.assertNotIn(pres.CSV_NAME, vs.PRESERVED_NAMES)

    def test_a_changed_groups_csv_fails_the_baseline(self):
        with tempfile.TemporaryDirectory() as d:
            site = Path(d) / "site"
            (site / "images").mkdir(parents=True)
            rec = {"id": "a1", "source_root": "local", "file_path": "images/a.png",
                   "file_size_bytes": 3, "field_values": {}, "metadata": {}}
            (site / "images" / "a.png").write_bytes(b"abc")
            (site / "MANIFEST.json").write_text(json.dumps([rec]), encoding="utf-8")
            (site / "posts.json").write_text("[]", encoding="utf-8")
            (site / "groups.csv").write_bytes(b"group_id,asset_count\r\ng1,8\r\n")
            prof = Path(d) / "p.json"
            prof.write_text(json.dumps([rec]), encoding="utf-8")
            posts = Path(d) / "q.json"
            posts.write_text("[]", encoding="utf-8")
            baseline = vs.record_baseline(site)
            self.assertIn("groups.csv", baseline["files"])
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, site, baseline=baseline)
            name = "preserved files byte-equal to baseline"
            self.assertEqual(_verdict(rep, name).status, vs.PASS)
            # ⛔ Rewriting asset_count to the shipped count is exactly the
            # "correction" this rule exists to refuse.
            (site / "groups.csv").write_bytes(b"group_id,asset_count\r\ng1,1\r\n")
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, site, baseline=baseline)
            self.assertEqual(_verdict(rep, name).status, vs.FAIL)
            self.assertIn("groups.csv", _verdict(rep, name).detail)


class TestVerifierChecksTheCsvTransform(unittest.TestCase):
    """C7. The verifier's metadata.csv verdict, including the site_a shape:
    a ZERO-REMOVAL transform, so any change at all fails.

    ⛔ AND IT NEVER REPORTS PASS FOR A COMPARISON IT DID NOT MAKE. Without
    the document the verdict is NOT COMPARED, because a green tick over an unmade
    check is the shape of every silent failure `verify_site` exists for, and
    #1319 measured what it looks like.
    """

    def _world(self, d, rows=907):
        site = Path(d) / "site"
        (site / "images").mkdir(parents=True)
        rec = {"id": "a1", "source_root": "local", "file_path": "images/a.png",
               "file_size_bytes": 3, "field_values": {}, "metadata": {}}
        (site / "images" / "a.png").write_bytes(b"abc")
        (site / "MANIFEST.json").write_text(json.dumps([rec]), encoding="utf-8")
        (site / "posts.json").write_text("[]", encoding="utf-8")
        (site / "metadata.csv").write_bytes(_csv_bytes(_paths(rows)))
        prof = Path(d) / "p.json"
        prof.write_text(json.dumps([rec]), encoding="utf-8")
        posts = Path(d) / "q.json"
        posts.write_text("[]", encoding="utf-8")
        out = Path(d) / "t.json"
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(pres.main([
                "csv-transform", "--snapshot", str(site),
                "--live-site", str(Path(d) / "live"),
                "--staging", str(Path(d) / "staging"),
                "--no-collapse-document", "--profile", prof.name,
                "--out", str(out)]), 0)
        return prof, posts, site, out

    NAME = "metadata.csv is the documented transform"

    def test_the_zero_removal_transform_passes_then_fails_on_any_change(self):
        with tempfile.TemporaryDirectory() as d:
            prof, posts, site, doc = self._world(d)
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, site, csv_transform=doc)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.PASS)
            blob = (site / "metadata.csv").read_bytes()
            h, rows = pres.split_csv_records(blob)
            (site / "metadata.csv").write_bytes(h + b"".join(rows[:-1]))
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, site, csv_transform=doc)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.FAIL)
            self.assertFalse(rep.ok)

    def test_an_emptied_metadata_csv_fails(self):
        with tempfile.TemporaryDirectory() as d:
            prof, posts, site, doc = self._world(d)
            h, _ = pres.split_csv_records((site / "metadata.csv").read_bytes())
            (site / "metadata.csv").write_bytes(h)
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, site, csv_transform=doc)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.FAIL)

    def test_without_the_document_the_verdict_is_not_compared_never_pass(self):
        with tempfile.TemporaryDirectory() as d:
            prof, posts, site, _doc = self._world(d)
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, site)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.NOT_COMPARED)

    def test_an_unusable_document_fails_rather_than_reading_as_absent(self):
        with tempfile.TemporaryDirectory() as d:
            prof, posts, site, doc = self._world(d)
            doc.write_text("{nope", encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, site, csv_transform=doc)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.FAIL)

    def test_the_cli_takes_the_flag(self):
        with tempfile.TemporaryDirectory() as d:
            prof, posts, site, doc = self._world(d, rows=20)
            with contextlib.redirect_stdout(io.StringIO()), \
                    contextlib.redirect_stderr(io.StringIO()):
                rc = vs.main(["check", "--profile", str(prof), "--posts", str(posts),
                              "--site", str(site), "--csv-transform", str(doc)])
            self.assertEqual(rc, 0)


# --- preserved_archive evidence fixtures ------------------------------------
#
# A `local` record carries NO source_archive and NO render block, because the
# real ones do not: 0 of 696 site_a and 0 of 552 site_b `local` records carry
# one. That is the whole reason the evidence schema is discriminated: the
# produced-source shape could only be satisfied here by inventing provenance.

PRESERVED_BYTES = b"the only copy of these bytes"
PRESERVED_SHA = hashlib.sha256(PRESERVED_BYTES).hexdigest()


def _pres_rec(rid, owner="priya.sharma", *, name=None, **over):
    name = name or rid[:8]
    rec = {
        "id": rid,
        "owner_username": owner,
        "source_root": "local",
        "source_path": f"aurora-authored/{name}.png",
        "file_path": f"images/aurora-authored/{name}.png",
        "file_size_bytes": len(PRESERVED_BYTES),
        "title": "Studio plate",
        "license": "CC0 1.0",
        "archive_state": "active",
        "field_values": {"rating": 4},
        "metadata": {"filename": f"{name}.png"},
    }
    rec.update(over)
    return rec


def _pres_entry(retired, survivor, *, retired_sha=None, survivor_sha=None,
                losses=None, post_subs=(), **over):
    entry = {
        "retired_id": retired["id"],
        "survivor_id": survivor["id"],
        "owner_username": retired["owner_username"],
        "source_root": retired["source_root"],
        "retired_file_path": retired["file_path"],
        "evidence": {
            "kind": ac.KIND_PRESERVED,
            "retired_sha256": retired_sha or PRESERVED_SHA,
            "survivor_sha256": survivor_sha or retired_sha or PRESERVED_SHA,
        },
        "retired_record": json.loads(json.dumps(retired)),
        "acknowledged_losses": (ac.recompute_losses(retired, survivor)
                                if losses is None else losses),
        "post_substitutions": list(post_subs),
    }
    entry.update(over)
    return entry


def _pres_doc(entries, profile="studio-a.assets.json", **over):
    """⛔ NO `pool_of_record`. A preserved-only document produces nothing, so
    a pool would describe a toolchain that had no part in the claim."""
    doc = {"_why": ["fixture"], "profile": profile, "collapse": list(entries)}
    doc.update(over)
    return doc


class TestDiscriminatedCollapseEvidence(unittest.TestCase):
    """E1-E7. `evidence.kind` is REQUIRED and has NO default.

    ⛔ A DEFAULT WOULD HAVE TO CHOOSE, and choosing `produced_source` would
    hand the stronger authority to an entry nobody labelled. That is the
    permissive arm, and the permissive arm is the dangerous one.
    """

    def setUp(self):
        self.retired = _pres_rec(RETIRED_ID, archive_state="draft")
        self.survivor = _pres_rec(SURVIVOR_ID)

    def _parse(self, doc, **kw):
        return ac.parse_collapse_document(doc, source="fixture", **kw)

    def _refuses(self, doc, fragment):
        with self.assertRaises(ac.CollapseError) as cm:
            ac.parse_collapse_document(doc, source="fixture")
        self.assertIn(fragment, str(cm.exception))
        return str(cm.exception)

    # -- E1: a local entry becomes expressible -----------------------------

    def test_e1_a_preserved_entry_validates_with_no_source_render_or_tool(self):
        """E1. On `dev` this could not be written at all: the schema demanded
        a source hash, a member, a render size and a rasteriser, and a
        `local` record has none of them."""
        doc = self._parse(_pres_doc([_pres_entry(self.retired, self.survivor)]))
        self.assertEqual(len(doc.entries), 1)
        e = doc.entries[0]
        self.assertEqual(e.kind, ac.KIND_PRESERVED)
        self.assertTrue(e.is_preserved)
        self.assertEqual(e.retired_sha256, PRESERVED_SHA)
        self.assertEqual(e.survivor_sha256, PRESERVED_SHA)
        for absent in ("source_sha256", "source_member", "render_px",
                       "materialized_sha256", "materialized_tool"):
            self.assertIsNone(getattr(e, absent), absent)
        self.assertEqual(e.retired_bytes_sha256, PRESERVED_SHA)

    def test_the_local_cross_checks_that_do_apply_are_still_enforced(self):
        """`id`, `owner_username`, `source_root`, `file_path` and a non-empty
        `source_path`. Only the source_archive and render cross-checks are
        dropped, and only because there is nothing to check them against."""
        for key, value in (("id", OTHER_ID), ("owner_username", "someone.else"),
                           ("source_root", "hq"), ("file_path", "images/other.png")):
            with self.subTest(key=key):
                rec = _pres_rec(RETIRED_ID, archive_state="draft")
                rec[key] = value
                e = _pres_entry(rec, self.survivor)
                e["retired_file_path"] = self.retired["file_path"]
                e["source_root"] = "local"
                e["owner_username"] = "priya.sharma"
                e["retired_id"] = RETIRED_ID
                self._refuses(_pres_doc([e]), "retired_record")
        rec = _pres_rec(RETIRED_ID, archive_state="draft")
        rec["source_path"] = ""
        self._refuses(_pres_doc([_pres_entry(rec, self.survivor)]),
                      "has no source_path")

    def test_a_preserved_entry_must_not_declare_a_source_archive_cross_check(self):
        """⛔ THE CROSS-CHECKS THAT DO NOT APPLY ARE NOT FAKED. A record that
        does carry a source_archive is an `hq`/`pack` record, and the root
        rule already refuses the weaker claim for it."""
        rec = _pres_rec(RETIRED_ID, archive_state="draft", source_root="hq")
        e = _pres_entry(rec, self.survivor)
        e["source_root"] = "hq"
        self._refuses(_pres_doc([e]), "not archive-authoritative")

    # -- E3: the kind itself ----------------------------------------------

    def test_e3_a_missing_kind_refuses(self):
        e = _pres_entry(self.retired, self.survivor)
        e["evidence"].pop("kind")
        self._refuses(_pres_doc([e]), "There is NO default")

    def test_e3_an_unknown_kind_refuses(self):
        for bad in ("produced", "PRESERVED_ARCHIVE", "", None, 7):
            with self.subTest(kind=bad):
                e = _pres_entry(self.retired, self.survivor)
                e["evidence"]["kind"] = bad
                self._refuses(_pres_doc([e]), "evidence.kind is")

    # -- E4/E5: the two field sets are disjoint ----------------------------

    def test_e4_a_preserved_entry_carrying_produced_fields_is_refused(self):
        """⛔ REFUSED, NOT IGNORED. An ignored field looks like it was
        honoured: a reader would believe a produced hash had been checked
        when nothing checked it."""
        for key, value in (("materialized_sha256", "c" * 64),
                           ("source_sha256", "a" * 64),
                           ("member", "Vector/x.svg"),
                           ("render_px", 512),
                           ("materialized_tool", "rasterize_svg.mjs")):
            with self.subTest(key=key):
                e = _pres_entry(self.retired, self.survivor)
                e["evidence"][key] = value
                msg = self._refuses(_pres_doc([e]), "belong to the other kind")
                self.assertIn(key, msg)

    def test_a_produced_entry_carrying_preserved_fields_is_refused(self):
        retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        survivor = _collapse_rec(SURVIVOR_ID)
        for key in ("retired_sha256", "survivor_sha256"):
            with self.subTest(key=key):
                e = _collapse_entry(retired, survivor)
                e["evidence"][key] = "d" * 64
                self._refuses(_collapse_doc([e]), "belong to the other kind")

    def test_an_unknown_evidence_key_is_refused_for_either_kind(self):
        e = _pres_entry(self.retired, self.survivor)
        e["evidence"]["also_trust_me"] = True
        self._refuses(_pres_doc([e]), "unknown key(s)")

    def test_e5_produced_source_keeps_every_requirement_it_had(self):
        retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        survivor = _collapse_rec(SURVIVOR_ID)
        for key, fragment in (("source_sha256", "source_sha256 is not a sha256"),
                              ("materialized_sha256", "materialized_sha256 is not"),
                              ("member", "member is empty"),
                              ("render_px", "render_px must be a positive int"),
                              ("materialized_tool", "materialized_tool is empty")):
            with self.subTest(key=key):
                e = _collapse_entry(retired, survivor)
                e["evidence"].pop(key)
                self._refuses(_collapse_doc([e]), fragment)

    def test_e5_produced_source_keeps_both_retired_record_cross_checks(self):
        retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        survivor = _collapse_rec(SURVIVOR_ID)
        e = _collapse_entry(retired, survivor)
        e["evidence"]["source_sha256"] = SRC_SHA_2
        self._refuses(_collapse_doc([e]), "source_archive.sha256")
        e = _collapse_entry(retired, survivor)
        e["evidence"]["render_px"] = 1024
        self._refuses(_collapse_doc([e]), "render.px")

    def test_e5_produced_source_still_requires_its_two_hashes_to_DIFFER(self):
        """⚠️ THE OPPOSITE RULE FROM THE PRESERVED ONE, ON PURPOSE. Here the
        two values describe DIFFERENT bytes, a pack member before a render
        and then the render, so conflating them is the mistake. There the two
        values describe the SAME bytes, which is the collapse. Different
        fields, opposite rules, never merged."""
        retired = _collapse_rec(RETIRED_ID, archive_state="draft")
        survivor = _collapse_rec(SURVIVOR_ID)
        e = _collapse_entry(retired, survivor, mat=SRC_SHA)
        self._refuses(_collapse_doc([e]),
                      "both the source hash and the produced hash")

    # -- E6: the preserved pair must be EQUAL ------------------------------

    def test_e6_unequal_preserved_hashes_are_refused(self):
        e = _pres_entry(self.retired, self.survivor, survivor_sha="e" * 64)
        self._refuses(_pres_doc([e]), "which are DIFFERENT bytes")

    def test_e6_malformed_preserved_hashes_refuse_closed(self):
        for key in ("retired_sha256", "survivor_sha256"):
            for bad in ("not-a-hash", "", None, "A" * 64, "d" * 63):
                with self.subTest(key=key, value=bad):
                    e = _pres_entry(self.retired, self.survivor)
                    e["evidence"][key] = bad
                    self._refuses(_pres_doc([e]), f"{key} is not a sha256")

    # -- E2/E7: pool_of_record --------------------------------------------

    def test_e2_a_preserved_only_document_needs_no_pool_of_record(self):
        doc = self._parse(_pres_doc([_pres_entry(self.retired, self.survivor)]))
        self.assertEqual(doc.pool_of_record, {})
        self.assertTrue(doc.is_preserved_only)
        self.assertFalse(doc.has_produced_source)

    def test_e2_a_pool_on_a_preserved_only_document_is_refused_as_meaningless(self):
        doc = _pres_doc([_pres_entry(self.retired, self.survivor)])
        doc["pool_of_record"] = {"pack": "p", "render_px": 512, "rasteriser": "r",
                                 "sharp": "0.35.4", "node": "v22"}
        self._refuses(doc, "describes nothing")

    def test_e7_a_mixed_document_requires_the_pool_and_validates_each_kind(self):
        """E7. Deterministic: one produced-source entry makes the pool
        load-bearing, and each entry is validated only under its own kind:
        neither kind's fields ever satisfy the other's rules."""
        hq_ret = _collapse_rec(OTHER_ID, archive_state="draft")
        hq_sur = _collapse_rec("77777777-8888-9999-aaaa-bbbbbbbbbbbb")
        mixed = [_collapse_entry(hq_ret, hq_sur),
                 _pres_entry(self.retired, self.survivor)]
        doc = self._parse(_collapse_doc(mixed))
        self.assertEqual([e.kind for e in doc.entries],
                         [ac.KIND_PRODUCED, ac.KIND_PRESERVED])
        self.assertTrue(doc.has_produced_source)
        self.assertFalse(doc.is_preserved_only)
        self.assertEqual(doc.entries[0].retired_bytes_sha256, "c" * 64)
        self.assertEqual(doc.entries[1].retired_bytes_sha256, PRESERVED_SHA)
        # the pool is required, because a produced-source entry is present
        no_pool = _collapse_doc(mixed)
        no_pool.pop("pool_of_record")
        self._refuses(no_pool, "missing `pool_of_record`")
        # and the preserved entry is still judged as preserved
        bad = json.loads(json.dumps(mixed))
        bad[1]["evidence"]["survivor_sha256"] = "e" * 64
        self._refuses(_collapse_doc(bad), "DIFFERENT bytes")

    def test_the_root_sets_do_not_drift_from_the_guard_or_the_contract(self):
        """⛔ ONE BOUNDARY, THREE MODULES. If these ever disagree, a root
        could be source-backed to the guard and preserved to the schema, and
        the pair would then agree with each other while disagreeing with the
        dataset."""
        self.assertEqual(ac.PRODUCED_SOURCE_ROOTS, mg.SOURCE_BACKED_ROOTS)
        self.assertEqual(ac.PRESERVED_ARCHIVE_ROOTS, pres.PRESERVED_ROOTS)
        self.assertLessEqual(ac.PRESERVED_ARCHIVE_ROOTS, mg.MEASURABLE_ROOTS)
        self.assertEqual(ac.PRODUCED_SOURCE_ROOTS | ac.PRESERVED_ARCHIVE_ROOTS,
                         ac.AUTHENTICABLE_ROOTS)

    def test_n0_an_empty_collapse_list_is_still_a_legal_no_op(self):
        """N=0. Neither rule fires on a document with no entries, and an
        empty list stays what it was: nothing retired, and never
        permission."""
        doc = self._parse(_pres_doc([]))
        self.assertEqual(doc.entries, ())
        self.assertFalse(doc.has_produced_source)
        self.assertFalse(doc.is_preserved_only)

    def test_n2_several_preserved_entries_in_one_document(self):
        third = _pres_rec(OTHER_ID, archive_state="draft", name="third")
        fourth = _pres_rec("77777777-8888-9999-aaaa-bbbbbbbbbbbb", name="fourth")
        doc = self._parse(_pres_doc([
            _pres_entry(self.retired, self.survivor),
            _pres_entry(third, fourth, retired_sha="d" * 64)]))
        self.assertEqual(len(doc.entries), 2)
        self.assertEqual(doc.retired_ids, frozenset({RETIRED_ID, OTHER_ID}))

    def test_cross_owner_identical_bytes_are_never_grouped_away(self):
        """Two records with identical bytes and DIFFERENT owners are legal
        and are not a collision: identity is `(owner, file_hash)`.

        ⛔ A1/A2 cannot refuse this on their own: they never see the owner
        of the profile's survivor, only the document's claim about it. What
        they do guarantee is that the change is ENUMERATED: `owner_username`
        appears in the recomputed losses, so nothing is hidden. The refusal
        belongs to Layer B, which compares the document against the
        survivor actually in the profile (asserted in
        TestPreservedArchiveLayerB)."""
        other_owner = _pres_rec(SURVIVOR_ID, owner="diego.martinez")
        e = _pres_entry(self.retired, other_owner)
        doc = self._parse(_pres_doc([e]))
        self.assertEqual(doc.entries[0].owner_username, "priya.sharma")
        self.assertIn("owner_username",
                      {x["path"] for x in doc.entries[0].acknowledged_losses})
        # A2 is satisfied, and that is the honest boundary: the enumeration
        # matches, and the ownership question is Layer B's.
        state = ac.evaluate([other_owner], [], doc)
        self.assertEqual(state.pending_assets, 0)


class TestPreservedRootModeIsExplicit(unittest.TestCase):
    """R3/R8. The mode has to be asked for by name.

    ⛔ A FALLBACK CANNOT TELL A DECISION FROM A TYPO. On `dev`
    `--local-source` was `required=True`, so the mode did not exist; making
    it optional without an explicit flag would have made every forgotten
    argument a silent archive-authoritative publish.
    """

    def _world(self, d, *, rows=40, local_bytes=PRESERVED_BYTES):
        root = Path(d)
        for sub in ("profiles", "upgrades", "internet", "dest", "evidence"):
            (root / sub).mkdir(parents=True, exist_ok=True)
        rec = _pres_rec(SURVIVOR_ID)
        rec["file_size_bytes"] = len(local_bytes)
        (root / "profiles" / "studio-a.assets.json").write_text(
            json.dumps([rec]), encoding="utf-8")
        (root / "profiles" / "studio-a.posts.json").write_text("[]", encoding="utf-8")
        dest = root / "dest"
        (dest / "MANIFEST.json").write_text(json.dumps([rec]), encoding="utf-8")
        (dest / "posts.json").write_text("[]", encoding="utf-8")
        (dest / pres.CSV_NAME).write_bytes(
            _csv_bytes(_paths(rows) + [rec["file_path"]]))
        (dest / pres.GROUPS_NAME).write_bytes(b"group_id,asset_count\r\ng1,8\r\n")
        p = dest / rec["file_path"]
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(local_bytes)
        # THREE TREES. `live` is a separate sibling of the staging tree the
        # publish writes, and the snapshot is a third frozen copy: the whole
        # point of the boundary is that these are not interchangeable.
        live = root / "live"
        shutil.copytree(dest, live)
        snapshot = root / "snapshot"
        shutil.copytree(dest, snapshot)
        out = root / "evidence" / "site_a.csv-transform.json"
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(pres.main([
                "csv-transform", "--snapshot", str(snapshot),
                "--live-site", str(live), "--staging", str(dest),
                "--no-collapse-document",
                "--profile", "studio-a.assets.json", "--out", str(out)]), 0)
        return root, out

    def _run(self, root, *extra, live=True):
        args = ["--internet-source", str(root / "internet"),
                "--profile", str(root / "profiles" / "studio-a.assets.json"),
                "--posts", str(root / "profiles" / "studio-a.posts.json"),
                "--dest", str(root / "dest")]
        if live:
            args += ["--live-site", str(root / "live")]
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "populate_archive.py"), *args, *extra],
            capture_output=True, text=True)

    @staticmethod
    def _tree(root):
        return {p.relative_to(root).as_posix():
                hashlib.sha256(p.read_bytes()).hexdigest()
                for p in sorted(root.rglob("*")) if p.is_file()}

    def test_r3_omitting_local_source_without_the_flag_is_an_error(self):
        with tempfile.TemporaryDirectory() as d:
            root, _out = self._world(d)
            r = self._run(root)
            self.assertEqual(r.returncode, 2)
            self.assertIn("--local-source is required", r.stderr)
            self.assertIn("--preserved-roots", r.stderr)
            self.assertIn("cannot tell a decision from a typo", r.stderr)

    def test_r3_the_flag_states_the_mode_and_names_the_weaker_claim(self):
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            r = self._run(root, "--preserved-roots", "--csv-transform", str(out),
                          "--dry-run")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("PRESERVED-ROOT MODE", r.stderr)
            self.assertIn("ARCHIVE-AUTHORITATIVE", r.stderr)
            self.assertIn("WEAKER claim", r.stderr)
            self.assertIn("'local'", r.stderr)

    def test_a_local_source_with_the_flag_is_refused_as_meaningless(self):
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            (root / "elsewhere").mkdir()
            r = self._run(root, "--preserved-roots", "--csv-transform", str(out),
                          "--local-source", str(root / "elsewhere"))
            self.assertEqual(r.returncode, 2)
            self.assertIn("meaningless with --preserved-roots", r.stderr)

    def test_the_mode_requires_the_csv_transform_document(self):
        with tempfile.TemporaryDirectory() as d:
            root, _out = self._world(d)
            r = self._run(root, "--preserved-roots")
            self.assertEqual(r.returncode, 2)
            self.assertIn("requires --csv-transform", r.stderr)
            self.assertIn("HEADER-ONLY", r.stderr)

    def test_r2_a_source_that_is_the_destination_is_refused(self):
        """R2. Accepted on `dev`: the destination became its own evidence."""
        with tempfile.TemporaryDirectory() as d:
            root, _out = self._world(d)
            r = self._run(root, "--local-source", str(root / "dest"))
            self.assertEqual(r.returncode, 2)
            self.assertIn("aliasing refusal", r.stderr)
            self.assertIn("its own evidence", r.stderr)

    def test_r2_a_source_containing_the_destination_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root, _out = self._world(d)
            r = self._run(root, "--local-source", str(root))
            self.assertEqual(r.returncode, 2)
            self.assertIn("CONTAINS", r.stderr)

    def test_r2_an_evidence_document_inside_the_destination_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            inside = root / "dest" / "t.json"
            shutil.copyfile(out, inside)
            r = self._run(root, "--preserved-roots", "--csv-transform", str(inside))
            self.assertEqual(r.returncode, 2)
            self.assertIn("CONTAINS", r.stderr)

    def test_the_documented_transform_is_applied_and_groups_csv_is_untouched(self):
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            groups_before = (root / "dest" / pres.GROUPS_NAME).read_bytes()
            csv_before = (root / "dest" / pres.CSV_NAME).read_bytes()
            r = self._run(root, "--preserved-roots", "--csv-transform", str(out))
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("ZERO-REMOVAL transform", r.stderr)
            self.assertIn("nothing to write", r.stderr)
            self.assertIn("preservation-owned, left untouched", r.stderr)
            self.assertEqual((root / "dest" / pres.CSV_NAME).read_bytes(),
                             csv_before)
            self.assertEqual((root / "dest" / pres.GROUPS_NAME).read_bytes(),
                             groups_before)

    def test_a_destination_csv_the_document_does_not_describe_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            csvp = root / "dest" / pres.CSV_NAME
            csvp.write_bytes(csvp.read_bytes() + b"a99999,,images/x.png\r\n")
            before = self._tree(root / "dest")
            r = self._run(root, "--preserved-roots", "--csv-transform", str(out))
            self.assertEqual(r.returncode, 2)
            self.assertIn("not evidence about these bytes", r.stderr)
            self.assertEqual(self._tree(root / "dest"), before,
                             "a refusal must write nothing")

    def test_r8_the_dry_run_reports_the_same_actions_and_writes_nothing(self):
        """R8. ⛔ A dry run that passes while the real run would destroy data
        is worse than no dry run at all."""
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            before = self._tree(root / "dest")
            dry = self._run(root, "--preserved-roots", "--csv-transform", str(out),
                            "--dry-run")
            self.assertEqual(dry.returncode, 0, dry.stderr)
            self.assertEqual(self._tree(root / "dest"), before)
            real = self._run(root, "--preserved-roots", "--csv-transform", str(out))
            self.assertEqual(real.returncode, 0, real.stderr)

            def actions(text):
                return [l.strip() for l in text.split("\n")
                        if l.startswith(("metadata.csv:", "groups.csv:"))
                        or "preexisting:" in l or "missing:" in l
                        or "wrong size:" in l or "copied:" in l]
            self.assertEqual(actions(dry.stderr), actions(real.stderr))

    def test_r4_a_preserved_local_byte_count_disagreement_refuses(self):
        """R4. `local` is a MEASURABLE root now: a profile that disagrees
        with the archive about a `local` byte count is a corrupted
        measurement, where on `dev` it was a permitted stale-share edit."""
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            prof = root / "profiles" / "studio-a.assets.json"
            recs = json.loads(prof.read_text())
            recs[0]["file_size_bytes"] = 999999
            prof.write_text(json.dumps(recs), encoding="utf-8")
            r = self._run(root, "--preserved-roots", "--csv-transform", str(out),
                          "--dry-run")
            self.assertEqual(r.returncode, 2)
            self.assertIn("CORRUPTED_MEASUREMENT", r.stderr)

    def test_r9_a_preserved_local_file_absent_at_the_destination_still_fails(self):
        """R9-adjacent: the N-boundary behaviour of the copy loop is
        untouched. A preserved record has no source to fall back to, so an
        absent file is a hole and the run must fail, exactly as a pre-staged
        one does."""
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            (root / "dest" / _pres_rec(SURVIVOR_ID)["file_path"]).unlink()
            r = self._run(root, "--preserved-roots", "--csv-transform", str(out))
            self.assertEqual(r.returncode, 1)
            self.assertIn("MISSING [local]", r.stderr)

    def test_same_length_different_bytes_is_caught_by_the_declared_hash(self):
        """⛔ A SIZE MATCH IS NOT A BYTE MATCH, and for a preserved root the
        recorded hash is the only attestation there is. Measured before this
        was added: 96 site_a and 39 site_b records declare a sha256 on a
        pre-staged root and all 135 match, so it refuses nothing that
        exists."""
        with tempfile.TemporaryDirectory() as d:
            root, out = self._world(d)
            prof = root / "profiles" / "studio-a.assets.json"
            recs = json.loads(prof.read_text())
            recs[0]["metadata"]["sha256"] = PRESERVED_SHA
            prof.write_text(json.dumps(recs), encoding="utf-8")
            (root / "dest" / "MANIFEST.json").write_text(json.dumps(recs),
                                                         encoding="utf-8")
            ok = self._run(root, "--preserved-roots", "--csv-transform", str(out))
            self.assertEqual(ok.returncode, 0, ok.stderr)
            target = root / "dest" / recs[0]["file_path"]
            target.write_bytes(b"X" * len(PRESERVED_BYTES))
            bad = self._run(root, "--preserved-roots", "--csv-transform", str(out))
            self.assertEqual(bad.returncode, 1)
            self.assertIn("WRONG BYTES [local]", bad.stderr)


class TestPreservedArchiveLayerB(unittest.TestCase):
    """R6/R6b. A `preserved_archive` retirement is authenticated against a
    FROZEN PRE-OPERATION SNAPSHOT, attested by an external manifest.

    ⛔ THE SNAPSHOT IS NOT TRUSTED FOR BEING A SNAPSHOT. It is a copy we
    took; the manifest is what we said it was at the moment we took it. The
    relevant hashes are recomputed against the manifest immediately before
    the comparison, EVERY RUN. Without that, the check is the archive
    agreeing with itself, which a stale copy does perfectly.

    ⛔ AND NEVER FROM THE TREE BEING PUBLISHED. Live or staging, a tree this
    run writes to cannot be the evidence for what it writes.
    """

    def _world(self, d, *, snapshot_bytes=None, doc=None, entry_over=None,
               stage_retired=True):
        root = Path(d)
        for sub in ("profiles", "upgrades", "internet", "dest", "snapshot",
                    "live", "evidence"):
            (root / sub).mkdir(parents=True, exist_ok=True)
        retired = _pres_rec(RETIRED_ID, archive_state="draft")
        survivor = _pres_rec(SURVIVOR_ID)
        entry = _pres_entry(retired, survivor, **(entry_over or {}))
        (root / "upgrades" / "asset-collapse.studio-a.json").write_text(
            json.dumps(doc if doc is not None else _pres_doc([entry])),
            encoding="utf-8")
        (root / "profiles" / "studio-a.assets.json").write_text(
            json.dumps([survivor]), encoding="utf-8")
        (root / "profiles" / "studio-a.posts.json").write_text(
            "[]", encoding="utf-8")

        dest = root / "dest"
        (dest / "MANIFEST.json").write_text(json.dumps([survivor, retired]),
                                            encoding="utf-8")
        (dest / "posts.json").write_text("[]", encoding="utf-8")
        (dest / pres.GROUPS_NAME).write_bytes(b"group_id,asset_count\r\ng1,8\r\n")
        rows = _paths(20) + [survivor["file_path"], retired["file_path"]]
        (dest / pres.CSV_NAME).write_bytes(_csv_bytes(rows))
        for rec in (survivor, retired):
            if rec is retired and not stage_retired:
                continue
            p = dest / rec["file_path"]
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(PRESERVED_BYTES)

        # ⛔ THREE DISTINCT TREES. `live` is the published site, `dest` is the
        # staging copy this run writes, and `snapshot` is the frozen pre-op
        # copy the retirement is authenticated against. A boundary checked
        # only against `dest` would accept `live` as the snapshot, which is
        # the hole these fixtures exist to keep closed.
        live = root / "live"
        snap = root / "snapshot"
        for tree in (live, snap):
            for rec in (survivor, retired):
                p = tree / rec["file_path"]
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_bytes(PRESERVED_BYTES if (snapshot_bytes is None
                                                  or tree is live)
                              else snapshot_bytes)
        shutil.copyfile(dest / pres.CSV_NAME, snap / pres.CSV_NAME)
        shutil.copyfile(dest / pres.CSV_NAME, live / pres.CSV_NAME)
        manifest = root / "evidence" / "site_a.snapshot-manifest.json"
        cdoc = root / "upgrades" / "asset-collapse.studio-a.json"
        with contextlib.redirect_stderr(io.StringIO()):
            pres.main(["snapshot-manifest", "--snapshot", str(snap),
                       "--live-site", str(live), "--staging", str(dest),
                       "--out", str(manifest)])
        transform = root / "evidence" / "site_a.csv-transform.json"
        with contextlib.redirect_stderr(io.StringIO()):
            pres.main(["csv-transform", "--snapshot", str(snap),
                       "--live-site", str(live), "--staging", str(dest),
                       "--collapse-document", str(cdoc),
                       "--out", str(transform)])
        return root, transform, manifest

    def _run(self, root, transform, *extra, live=True):
        args = ["--preserved-roots",
                "--internet-source", str(root / "internet"),
                "--profile", str(root / "profiles" / "studio-a.assets.json"),
                "--posts", str(root / "profiles" / "studio-a.posts.json"),
                "--csv-transform", str(transform),
                "--dest", str(root / "dest")]
        if live:
            args += ["--live-site", str(root / "live")]
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "populate_archive.py"), *args, *extra],
            capture_output=True, text=True)

    def _full(self, root, transform, manifest, *extra):
        return self._run(root, transform,
                         "--frozen-snapshot", str(root / "snapshot"),
                         "--snapshot-manifest", str(manifest), *extra)

    @staticmethod
    def _tree(root):
        return {p.relative_to(root).as_posix():
                hashlib.sha256(p.read_bytes()).hexdigest()
                for p in sorted(root.rglob("*")) if p.is_file()}

    def test_an_attested_preserved_retirement_reports_collapsed(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            r = self._full(root, transform, manifest, "--dry-run")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn(f"COLLAPSED_RECORD {RETIRED_ID} -> {SURVIVOR_ID}",
                          r.stderr)
            self.assertNotIn("MISSING_RECORD", r.stderr)
            self.assertIn(ac.KIND_PRESERVED, r.stderr)
            self.assertIn("WEAKER CLAIM", r.stderr)

    def test_the_csv_row_of_the_retired_record_is_the_documented_removal(self):
        """N=1 on the real shape: the retirement removes its CSV row and
        nothing else, and the transform document says so in advance."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            doc = pres.load_csv_transform(transform)
            self.assertEqual(len(doc["removals"]), 1)
            self.assertEqual(doc["removals"][0]["retired_id"], RETIRED_ID)
            self.assertEqual(doc["original"]["data_rows"], 22)
            self.assertEqual(doc["expected"]["data_rows"], 21)
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(
                pres.verify_csv_transform(
                    doc, (root / "dest" / pres.CSV_NAME).read_bytes()), [])
            self.assertFalse(
                (root / "dest" / _pres_rec(RETIRED_ID)["file_path"]).is_file())

    def test_r6_without_the_snapshot_and_manifest_it_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            for extra in ((), ("--frozen-snapshot", str(root / "snapshot")),
                          ("--snapshot-manifest", str(manifest))):
                with self.subTest(extra=extra):
                    r = self._run(root, transform, *extra, "--dry-run")
                    self.assertEqual(r.returncode, 2)
                    self.assertIn("needs both --frozen-snapshot and "
                                  "--snapshot-manifest", r.stderr)

    def test_r6_authentication_from_the_staging_tree_refuses(self):
        """⛔ THE TREE BEING PUBLISHED CANNOT BE THE EVIDENCE."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            before = self._tree(root / "dest")
            r = self._run(root, transform,
                          "--frozen-snapshot", str(root / "dest"),
                          "--snapshot-manifest", str(manifest))
            self.assertEqual(r.returncode, 2)
            self.assertIn("preserved-operation path refusal", r.stderr)
            self.assertIn("the tree this run WRITES", r.stderr)
            self.assertEqual(self._tree(root / "dest"), before)

    def test_r6_authentication_from_the_LIVE_tree_refuses(self):
        """⛔⛔ THE HOLE A DESTINATION-ONLY CHECK LEAVES OPEN. A preserved
        operation has THREE trees, and live is neither the snapshot nor the
        staging tree. Comparing the snapshot against `--dest` alone proves
        only that it is not the tree being written: measured against the
        first version of this work, passing the LIVE site as
        `--frozen-snapshot` while `--dest` pointed at staging was ACCEPTED,
        the retirement authenticated, and the retired file deleted. Live can
        change under the run; it was never frozen."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            live, dest = root / "live", root / "dest"
            self.assertNotEqual(live.resolve(), dest.resolve(),
                                "the fixture must have live and staging as "
                                "genuinely distinct sibling trees")
            before = self._tree(dest)
            r = self._run(root, transform, "--frozen-snapshot", str(live),
                          "--snapshot-manifest", str(manifest))
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("preserved-operation path refusal", r.stderr)
            self.assertIn("FROZEN", r.stderr)
            self.assertIn("not frozen", r.stderr)
            self.assertEqual(self._tree(dest), before,
                             "a refusal must write nothing and delete nothing")

    def test_all_six_snapshot_shapes_against_live_and_staging_refuse(self):
        """The full boundary, as paths rather than as prose: equality and
        containment in both directions, against BOTH other trees."""
        live, staging, snap = Path("/a/live"), Path("/a/stage"), Path("/a/frozen")
        cases = [
            ("snapshot == live", live, staging, live),
            ("snapshot under live", live, staging, live / "frozen"),
            ("live under snapshot", snap / "live", staging, snap),
            ("snapshot == staging", live, staging, staging),
            ("snapshot under staging", live, staging, staging / "frozen"),
            ("staging under snapshot", live, snap / "stage", snap),
        ]
        for label, lv, st, sn in cases:
            with self.subTest(case=label):
                bad = pres.operation_refusals(live=lv, staging=st, snapshot=sn)
                self.assertTrue(bad, label)
        # …and the clean three-tree case, plus a direct publish where live
        # and staging are legitimately the same tree, both pass.
        self.assertEqual(pres.operation_refusals(
            live=live, staging=staging, snapshot=snap), [])
        self.assertEqual(pres.operation_refusals(
            live=live, staging=live, snapshot=snap), [])
        # a staging tree NESTED in live is still refused
        self.assertTrue(pres.operation_refusals(
            live=live, staging=live / "stage", snapshot=snap))

    def test_the_boundary_resists_dot_dot_symlinks_and_trailing_slashes(self):
        """⛔ IDENTITY IS THE RESOLVED PATH, NEVER A NAME. `..`, a symlink and a
        trailing slash all make two spellings of one tree look different, and a
        boundary defeated by a spelling is not a boundary. And no check
        anywhere looks for `frozen`, `.preop` or any other substring: a name is
        a label an operator chooses and a typo silently disables."""
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            live, staging = root / "live", root / "staging"
            for x in (live, staging):
                x.mkdir()
            # the same tree, spelled three ways
            for spelling in (root / "x" / ".." / "live",
                             Path(str(live) + "/"),
                             root / "linked"):
                if spelling.name == "linked":
                    spelling.symlink_to(live)
                with self.subTest(spelling=str(spelling)):
                    bad = pres.operation_refusals(
                        live=live, staging=staging, snapshot=spelling)
                    self.assertTrue(bad, f"{spelling} is the live tree")
            # a NAME that looks frozen is not evidence of anything
            decoy = root / "live" / "site_a.preop.frozen.snapshot"
            decoy.mkdir(parents=True)
            self.assertTrue(pres.operation_refusals(
                live=live, staging=staging, snapshot=decoy),
                "a directory named like a snapshot, inside live, is still inside live")

    def test_a_missing_live_site_refuses_rather_than_defaulting(self):
        """⛔ A FALLBACK CANNOT TELL A DECISION FROM A TYPO. Defaulting live
        to `--dest` would silently restore the hole for every operator who
        forgot the flag in the staging workflow."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            r = self._run(root, transform, "--frozen-snapshot",
                          str(root / "snapshot"), "--snapshot-manifest",
                          str(manifest), live=False)
            self.assertEqual(r.returncode, 2)
            self.assertIn("requires --live-site", r.stderr)
            self.assertIn("THREE distinct trees", r.stderr)

    def test_evidence_inside_any_of_the_three_trees_refuses(self):
        """D/E. The manifest and the transform are evidence ABOUT the
        operation; stored inside a tree it writes or attests, they are
        evidence the operation can rewrite."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            for tree in ("live", "dest", "snapshot"):
                for label, src in (("--snapshot-manifest", manifest),
                                   ("--csv-transform", transform)):
                    with self.subTest(tree=tree, evidence=label):
                        inside = root / tree / "evidence.json"
                        shutil.copyfile(src, inside)
                        extra = ["--frozen-snapshot", str(root / "snapshot"),
                                 "--snapshot-manifest", str(manifest)]
                        if label == "--snapshot-manifest":
                            extra[-1] = str(inside)
                            r = self._run(root, transform, *extra)
                        else:
                            r = self._run(root, inside, *extra)
                        self.assertEqual(r.returncode, 2, r.stderr)
                        self.assertIn("preserved-operation path refusal", r.stderr)
                        inside.unlink()

    def test_a_snapshot_inside_the_destination_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            inside = root / "dest" / "frozen"
            shutil.copytree(root / "snapshot", inside)
            r = self._run(root, transform, "--frozen-snapshot", str(inside),
                          "--snapshot-manifest", str(manifest))
            self.assertEqual(r.returncode, 2)
            self.assertIn("CONTAINS", r.stderr)

    def test_r6b_a_snapshot_changed_after_attestation_refuses(self):
        """⛔⛔ R6b, AND THE WHOLE REASON THIS EVIDENCE KIND IS EVIDENCE. The
        mtime is restored to exactly what it was, because permissions and
        mtimes are NOT integrity proof, because a CIFS tree can change under both.
        Only the bytes say so."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            victim = root / "snapshot" / _pres_rec(RETIRED_ID)["file_path"]
            st = victim.stat()
            victim.write_bytes(b"Y" * len(PRESERVED_BYTES))
            os.utime(victim, (st.st_atime, st.st_mtime))
            os.chmod(victim, stat.S_IMODE(st.st_mode))
            after = victim.stat()
            self.assertEqual(after.st_mtime, st.st_mtime)
            self.assertEqual(after.st_size, st.st_size)
            before = self._tree(root / "dest")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("CHANGED after it was attested", r.stderr)
            self.assertIn("mtimes say nothing about this", r.stderr)
            self.assertEqual(self._tree(root / "dest"), before,
                             "a refusal deletes nothing")

    def test_a_path_missing_from_the_manifest_is_unattested_and_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            doc = json.loads(manifest.read_text())
            doc["files"].pop(_pres_rec(RETIRED_ID)["file_path"])
            manifest.write_text(json.dumps(doc), encoding="utf-8")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("not in the snapshot manifest", r.stderr)

    def test_an_unusable_manifest_refuses_rather_than_reading_as_absent(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            manifest.write_text("{nope", encoding="utf-8")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("authenticates nothing", r.stderr)

    def test_a_manifest_of_the_wrong_kind_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            manifest.write_text(json.dumps({"files": {"a": "b" * 64}}),
                                encoding="utf-8")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("kind is", r.stderr)

    def test_a_snapshot_whose_two_files_differ_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            victim = root / "snapshot" / _pres_rec(RETIRED_ID)["file_path"]
            victim.write_bytes(b"different artwork entirely")
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(pres.main([
                    "snapshot-manifest", "--snapshot", str(root / "snapshot"),
                    "--live-site", str(root / "live"),
                    "--staging", str(root / "dest"),
                    "--out", str(manifest)]), 0)
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("DIFFERENT bytes", r.stderr)

    def test_a_recorded_hash_the_snapshot_disagrees_with_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(
                d, entry_over={"retired_sha": "d" * 64})
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("Re-measure against the snapshot", r.stderr)

    def test_a_survivor_owned_by_someone_else_refuses(self):
        """The Layer B half of the cross-owner rule: identity is per OWNER,
        and two records with identical bytes and different owners are not a
        collision.

        ⚠️ THE LOSS ENUMERATION CATCHES IT FIRST, and that ordering is the
        stronger one: changing the survivor's owner changes what retiring
        onto it would LOSE, so the recomputation disagrees with the document
        before the owner cross-check is even reached. The cross-check
        remains the backstop for a document that got the enumeration right
        and the owner wrong."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            prof = root / "profiles" / "studio-a.assets.json"
            recs = json.loads(prof.read_text())
            recs[0]["owner_username"] = "diego.martinez"
            prof.write_text(json.dumps(recs), encoding="utf-8")
            before = self._tree(root / "dest")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("acknowledged_losses", r.stderr)
            self.assertIn("could not be source-authenticated", r.stderr)
            self.assertEqual(self._tree(root / "dest"), before)

    def test_a_dry_run_removes_nothing_and_a_real_run_removes_only_that_path(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            before = set(self._tree(root / "dest"))
            dry = self._full(root, transform, manifest, "--dry-run")
            self.assertEqual(dry.returncode, 0, dry.stderr)
            self.assertIn("would remove retired path", dry.stderr)
            self.assertEqual(set(self._tree(root / "dest")), before)
            real = self._full(root, transform, manifest)
            self.assertEqual(real.returncode, 0, real.stderr)
            gone = before - set(self._tree(root / "dest"))
            self.assertEqual(gone, {_pres_rec(RETIRED_ID)["file_path"]})

    def test_the_retired_path_is_hashed_against_the_preserved_hash_before_removal(self):
        """`retired_bytes_sha256` is the kind-aware field: `retired_sha256`
        here, `materialized_sha256` for a produced-source entry. A file that
        is not the one the document describes is never deleted."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            target = root / "dest" / _pres_rec(RETIRED_ID)["file_path"]
            target.write_bytes(b"not what the document says")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 1)
            self.assertIn("is not the one this document describes", r.stderr)
            self.assertIn(ac.KIND_PRESERVED, r.stderr)
            self.assertTrue(target.is_file())

    def test_a_produced_source_entry_without_its_root_still_refuses(self):
        """R6's other half, and proof the preserved path did not widen the
        produced one: an `hq` entry with no `--hq-source` cannot be
        authenticated and the run refuses."""
        with tempfile.TemporaryDirectory() as d:
            hq_ret = _collapse_rec(RETIRED_ID, archive_state="draft")
            hq_sur = _collapse_rec(SURVIVOR_ID)
            root, transform, manifest = self._world(
                d, doc=_collapse_doc([_collapse_entry(hq_ret, hq_sur)]))
            (root / "profiles" / "studio-a.assets.json").write_text(
                json.dumps([hq_sur]), encoding="utf-8")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 2)
            self.assertIn("--hq-source was not given", r.stderr)

    def test_a_preserved_only_document_asks_for_no_pool_at_publish(self):
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            doc = json.loads((root / "upgrades" /
                              "asset-collapse.studio-a.json").read_text())
            self.assertNotIn("pool_of_record", doc)
            r = self._full(root, transform, manifest, "--dry-run")
            self.assertEqual(r.returncode, 0, r.stderr)

    def test_a_retired_path_already_absent_is_a_clean_no_op(self):
        """The missing-earlier-file boundary. A retirement whose produced
        file is already gone from the destination has nothing left to
        remove, and that is Applied rather than an error: the pass is
        idempotent, so a second run over the same tree changes nothing."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d, stage_retired=False)
            before = self._tree(root / "dest")
            r = self._full(root, transform, manifest)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("retired path already absent", r.stderr)
            self.assertIn(f"COLLAPSED_RECORD {RETIRED_ID}", r.stderr)
            after = self._tree(root / "dest")
            # only metadata.csv moves, because the CSV row is still there
            self.assertEqual(set(before) - set(after), set())
            self.assertEqual(
                pres.verify_csv_transform(
                    pres.load_csv_transform(transform),
                    (root / "dest" / pres.CSV_NAME).read_bytes()), [])

    def test_a_preserved_entry_without_the_mode_refuses(self):
        """⛔ TWO AUTHORITY MODELS IN ONE RUN IS NOT A CONFIGURATION. A
        preserved retirement is a claim about ARCHIVE-HELD bytes; authenticating
        one while the same run copies `local` from a source root would let the
        weaker claim do the deciding."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            (root / "localsrc").mkdir()
            (root / "localsrc" / pres.CSV_NAME).write_bytes(b"file_path\r\n")
            r = subprocess.run(
                [sys.executable, str(SCRIPTS / "populate_archive.py"),
                 "--local-source", str(root / "localsrc"),
                 "--internet-source", str(root / "internet"),
                 "--profile", str(root / "profiles" / "studio-a.assets.json"),
                 "--posts", str(root / "profiles" / "studio-a.posts.json"),
                 "--frozen-snapshot", str(root / "snapshot"),
                 "--snapshot-manifest", str(manifest),
                 "--dest", str(root / "dest"), "--dry-run"],
                capture_output=True, text=True)
            self.assertEqual(r.returncode, 2)
            self.assertIn("only meaningful when `local` is", r.stderr)
            self.assertIn("--preserved-roots", r.stderr)

    def test_p8r_the_dry_run_and_the_evidence_work_never_touch_the_snapshot(self):
        """P8r, a PRESERVATION assertion. The snapshot is the evidence; a
        run that could write to it would be grading its own homework."""
        with tempfile.TemporaryDirectory() as d:
            root, transform, manifest = self._world(d)
            snap_before = self._tree(root / "snapshot")
            man_before = manifest.read_bytes()
            r = self._full(root, transform, manifest, "--dry-run")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(self._tree(root / "snapshot"), snap_before)
            self.assertEqual(manifest.read_bytes(), man_before)
            self._full(root, transform, manifest)
            self.assertEqual(self._tree(root / "snapshot"), snap_before,
                             "a real run must not write to the snapshot either")


class TestCommittedCollapseDocumentIsMigrated(unittest.TestCase):
    """R7, a PRESERVATION assertion. The one committed document gains
    `evidence.kind: produced_source` and NOTHING ELSE changes.

    ⛔ AND THERE IS NO LEGACY NO-KIND FALLBACK. A default branch that granted
    the stronger authority to an unlabelled entry is exactly the permissive
    arm this discrimination exists to close, so the committed document is
    MIGRATED rather than grandfathered.
    """

    PATH = UPGRADES / "asset-collapse.studio-a.json"

    def test_the_committed_document_declares_produced_source(self):
        raw = json.loads(self.PATH.read_text(encoding="utf-8"))
        self.assertEqual([e["evidence"]["kind"] for e in raw["collapse"]],
                         [ac.KIND_PRODUCED])

    def test_it_still_loads_and_authenticates_through_the_same_path(self):
        doc = ac.load_collapse_document(self.PATH,
                                        profile_name="studio-a.assets.json")
        self.assertEqual(len(doc.entries), 1)
        e = doc.entries[0]
        self.assertEqual(e.kind, ac.KIND_PRODUCED)
        self.assertEqual(e.source_root, "hq")
        self.assertFalse(e.is_preserved)
        # every produced-source field is still in hand and unchanged
        self.assertEqual(e.source_sha256,
                         e.retired_record["metadata"]["source_archive"]["sha256"])
        self.assertEqual(e.render_px, e.retired_record["metadata"]["render"]["px"])
        self.assertEqual(e.retired_bytes_sha256, e.materialized_sha256)
        self.assertIsNone(e.retired_sha256)
        # and it still requires its pool, because it produces bytes
        self.assertTrue(doc.has_produced_source)
        self.assertFalse(doc.is_preserved_only)
        self.assertEqual(doc.pool_of_record["sharp"], "0.35.4")

    def test_the_verdict_against_the_committed_profile_is_unchanged(self):
        doc = ac.load_collapse_document(self.PATH,
                                        profile_name="studio-a.assets.json")
        profile = json.loads(
            (PROFILES / "studio-a.assets.json").read_text(encoding="utf-8"))
        posts = json.loads(
            (PROFILES / "studio-a.posts.json").read_text(encoding="utf-8"))
        result = ac.evaluate(profile, posts, doc)
        self.assertEqual(len(result.states), 1)
        self.assertEqual(result.states[0].asset_state, ac.APPLIED)
        by_id = {a["id"]: a for a in profile}
        for e in doc.entries:
            self.assertIn(e.survivor_id, by_id)
            self.assertNotIn(e.retired_id, by_id)
            self.assertTrue(ac.losses_equal(
                e.acknowledged_losses,
                ac.recompute_losses(e.retired_record, by_id[e.survivor_id])))

    def test_only_the_kind_line_was_added(self):
        """The migration is one key. Everything the document said before, it
        still says, byte for byte, in the same order."""
        raw = json.loads(self.PATH.read_text(encoding="utf-8"))
        ev = raw["collapse"][0]["evidence"]
        self.assertEqual(list(ev)[0], "kind")
        self.assertEqual(list(ev)[1:], ["source_sha256", "member", "render_px",
                                        "materialized_sha256",
                                        "materialized_tool", "_why"])

    def test_a_document_stripped_of_its_kind_refuses_rather_than_defaulting(self):
        raw = json.loads(self.PATH.read_text(encoding="utf-8"))
        raw["collapse"][0]["evidence"].pop("kind")
        with self.assertRaises(ac.CollapseError) as cm:
            ac.parse_collapse_document(raw, source="the committed document")
        self.assertIn("There is NO default", str(cm.exception))


class TestAuthoredPlateInstallIsChecked(unittest.TestCase):
    """P1r-P7r. The plates are built EXTERNALLY, checked, then installed.

    ⛔ `populate_archive.py` NEVER SYNTHESIZES THEM. A publish that can
    manufacture the bytes it is about to attest has no independent evidence
    left, so the recipe reads an ATTESTED snapshot's `images/aurora-generated`
    and writes to a scratch directory outside every site tree.

    The expectation is the PROFILE's, not this module's: a constant here
    would be a second opinion that could drift from the one the guard and
    the seeder use, and the pair would then agree with each other while
    disagreeing with the dataset.
    """

    SIZES = {"studio-colour-chart.png": 11404,
             "reference-mood-board.png": 1290128}
    HASHES = {
        "studio-colour-chart.png":
            "1495db50a28a55ba6238f85616b84918114069c33757774bb39a1b41d37108a0",
        "reference-mood-board.png":
            "6fa8e7e7790b76fcbab82b93e8ac6630ca14dd26732586c56fc48450f12ff65c",
    }

    def test_the_committed_profile_states_both_plates(self):
        want = ap.expected_plates(PROFILES / "studio-a.assets.json")
        self.assertEqual(set(want), set(ap.PLATES))
        for name, (size, sha) in want.items():
            self.assertEqual(size, self.SIZES[name])
            self.assertEqual(sha, self.HASHES[name])

    def test_p1r_a_staging_tree_copied_from_a_site_without_them_lacks_both(self):
        """P1r, a PRESERVATION assertion. `images/aurora-authored` does not
        exist in the published site_a, so the two records the profile carries
        describe bytes that have never been staged."""
        with tempfile.TemporaryDirectory() as d:
            live = Path(d) / "live"
            (live / "images" / "aurora-generated").mkdir(parents=True)
            stage = Path(d) / "stage"
            shutil.copytree(live, stage)
            self.assertFalse((stage / ap.INSTALL_DIR).exists())
            want = ap.expected_plates(PROFILES / "studio-a.assets.json")
            refusals = ap.verify_install(stage / ap.INSTALL_DIR, want)
            self.assertTrue(refusals)
            self.assertIn("not a directory", refusals[0])

    def _installed(self, d, want):
        """An install that is exactly right, built from the declared bytes."""
        target = Path(d) / "site" / ap.INSTALL_DIR
        target.mkdir(parents=True)
        for name, (size, _sha) in want.items():
            (target / name).write_bytes(b"\x00" * size)
        return target

    def test_p2r_p3r_an_install_adds_exactly_two_files_and_they_are_checked(self):
        """P2r/P3r. Exactly two, and each one checked against the size AND
        the hash the profile declares. A size match is not a byte match."""
        want = ap.expected_plates(PROFILES / "studio-a.assets.json")
        with tempfile.TemporaryDirectory() as d:
            target = self._installed(d, want)
            self.assertEqual(sorted(p.name for p in target.iterdir()),
                             sorted(ap.PLATES))
            refusals = ap.verify_install(target, want)
            # right sizes, wrong bytes: the hash is what catches it
            self.assertEqual(len(refusals), 2)
            self.assertTrue(all("different pixels" in r for r in refusals))

    def test_a_correct_install_passes(self):
        with tempfile.TemporaryDirectory() as d:
            target = Path(d) / ap.INSTALL_DIR
            target.mkdir(parents=True)
            bodies = {"a.png": b"alpha", "b.png": b"beta beta"}
            want = {n: (len(b), hashlib.sha256(b).hexdigest())
                    for n, b in bodies.items()}
            for n, b in bodies.items():
                (target / n).write_bytes(b)
            self.assertEqual(ap.verify_install(target, want), [])

    def test_p5r_a_wrong_hash_at_the_right_size_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            target = Path(d) / "plates"
            target.mkdir()
            (target / "a.png").write_bytes(b"wrong")
            want = {"a.png": (5, hashlib.sha256(b"right").hexdigest())}
            refusals = ap.verify_install(target, want)
            self.assertEqual(len(refusals), 1)
            self.assertIn("different pixels", refusals[0])

    def test_p6r_a_missing_plate_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            target = Path(d) / "plates"
            target.mkdir()
            (target / "a.png").write_bytes(b"ok")
            want = {"a.png": (2, hashlib.sha256(b"ok").hexdigest()),
                    "b.png": (2, hashlib.sha256(b"ok").hexdigest())}
            refusals = ap.verify_install(target, want)
            self.assertEqual(refusals, ["b.png is absent"])

    def test_p7r_an_unexpected_third_output_refuses(self):
        """⛔ The install hands a directory to the site and the uploader
        hands the site to the world, so a third output nobody enumerated
        would be published as studio work."""
        with tempfile.TemporaryDirectory() as d:
            target = Path(d) / "plates"
            target.mkdir()
            (target / "a.png").write_bytes(b"ok")
            (target / "scratch-draft.png").write_bytes(b"ok")
            want = {"a.png": (2, hashlib.sha256(b"ok").hexdigest())}
            refusals = ap.verify_install(target, want)
            self.assertEqual(len(refusals), 1)
            self.assertIn("nobody enumerated", refusals[0])

    def test_a_wrong_size_refuses_before_the_hash_is_even_read(self):
        with tempfile.TemporaryDirectory() as d:
            target = Path(d) / "plates"
            target.mkdir()
            (target / "a.png").write_bytes(b"too long")
            want = {"a.png": (2, hashlib.sha256(b"ok").hexdigest())}
            refusals = ap.verify_install(target, want)
            self.assertEqual(len(refusals), 1)
            self.assertIn("the profile declares", refusals[0])

    def test_a_record_with_no_declared_hash_is_not_installable(self):
        """An unattested plate cannot be checked, so it cannot be installed,
        rather than installed unchecked."""
        with tempfile.TemporaryDirectory() as d:
            prof = Path(d) / "p.json"
            prof.write_text(json.dumps([
                {"id": "x", "file_path": f"{ap.INSTALL_DIR}/a.png",
                 "file_size_bytes": 5, "metadata": {}}]), encoding="utf-8")
            with self.assertRaises(SystemExit) as cm:
                ap.expected_plates(prof)
            self.assertIn("not installable", str(cm.exception))

    def test_the_build_command_refuses_to_write_into_what_it_samples(self):
        """⛔ ALIAS REFUSAL. Writing the plates into the directory they
        sample would make the next build's input depend on the last build's
        output, and the recorded hashes would describe a tree that produced
        itself."""
        with tempfile.TemporaryDirectory() as d:
            snap = Path(d) / "snapshot"
            gen = snap / "images" / "aurora-generated"
            gen.mkdir(parents=True)
            argv = ["build", "--generated-source", str(gen),
                    "--out", str(gen / "out"), "--snapshot", str(snap),
                    "--live-site", str(Path(d) / "live"),
                    "--staging", str(Path(d) / "staging")]
            with unittest.mock.patch.object(sys, "argv", ["authored_plates.py"] + argv), \
                    contextlib.redirect_stderr(io.StringIO()) as err:
                rc = ap.main()
            self.assertEqual(rc, 2)
            self.assertIn("CONTAINS", err.getvalue())

    def test_the_cli_verify_subcommand_reads_the_profile(self):
        with tempfile.TemporaryDirectory() as d:
            want = ap.expected_plates(PROFILES / "studio-a.assets.json")
            target = self._installed(d, want)
            argv = ["verify", "--dir", str(target),
                    "--profile", str(PROFILES / "studio-a.assets.json")]
            with unittest.mock.patch.object(
                    sys, "argv", ["authored_plates.py"] + argv), \
                    contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(ap.main(), 1)
            self.assertIn("different pixels", err.getvalue())

    def test_p4r_a_publish_reports_zero_missing_for_an_installed_pair(self):
        """P4r. Once installed, the two records are ordinary preserved
        records: verified where they sit, 0 missing."""
        bodies = {"studio-colour-chart.png": b"chart bytes",
                  "reference-mood-board.png": b"board bytes here"}
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            for sub in ("profiles", "internet", "dest", "snapshot", "evidence"):
                (root / sub).mkdir(parents=True)
            recs = []
            for i, (name, body) in enumerate(sorted(bodies.items())):
                recs.append({
                    "id": f"0000000{i}-1111-2222-3333-44444444444{i}",
                    "owner_username": "aurora", "source_root": "local",
                    "source_path": f"aurora-authored/{name}",
                    "file_path": f"{ap.INSTALL_DIR}/{name}",
                    "file_size_bytes": len(body),
                    "field_values": {},
                    "metadata": {"filename": name,
                                 "sha256": hashlib.sha256(body).hexdigest()}})
            (root / "profiles" / "studio-a.assets.json").write_text(
                json.dumps(recs), encoding="utf-8")
            (root / "profiles" / "studio-a.posts.json").write_text(
                "[]", encoding="utf-8")
            dest = root / "dest"
            (dest / "MANIFEST.json").write_text(json.dumps(recs), encoding="utf-8")
            (dest / "posts.json").write_text("[]", encoding="utf-8")
            (dest / pres.CSV_NAME).write_bytes(
                _csv_bytes([r["file_path"] for r in recs]))
            for r, body in zip(recs, [bodies[Path(r["file_path"]).name]
                                      for r in recs]):
                p = dest / r["file_path"]
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_bytes(body)
            live = root / "live"
            shutil.copytree(dest, live)
            shutil.copyfile(dest / pres.CSV_NAME,
                            root / "snapshot" / pres.CSV_NAME)
            out = root / "evidence" / "t.json"
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(pres.main([
                    "csv-transform", "--snapshot", str(root / "snapshot"),
                    "--live-site", str(live), "--staging", str(dest),
                    "--no-collapse-document", "--profile",
                    "studio-a.assets.json", "--out", str(out)]), 0)
            r = subprocess.run(
                [sys.executable, str(SCRIPTS / "populate_archive.py"),
                 "--preserved-roots", "--internet-source", str(root / "internet"),
                 "--profile", str(root / "profiles" / "studio-a.assets.json"),
                 "--posts", str(root / "profiles" / "studio-a.posts.json"),
                 "--csv-transform", str(out), "--live-site", str(live),
                 "--dest", str(dest)],
                capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn("missing:     0", r.stderr)
            self.assertIn("wrong size:  0", r.stderr)
            self.assertIn("preexisting: 2", r.stderr)


class TestTransformAuthorityIsBoundToTheCurrentDocument(unittest.TestCase):
    """⛔ INTERNAL SELF-CONSISTENCY IS NOT AUTHORITY.

    A transform states its own before and after hashes, so a forged one that
    drops an extra row and recomputes its own expectations is perfectly
    self-consistent. Measured against the first version of this work: such a
    transform removed a row no collapse document mentioned, the publish wrote
    it, and the run exited 0. Two independent bindings close it, because they
    fail on different things: the DIGEST catches a document that changed at
    all, the RECOMPUTATION catches removals that disagree with it in either
    direction.
    """

    def _world(self, d, *, doc=None):
        """`doc` overrides the whole collapse document, because the wrapper
        differs per kind: a preserved-only document carries NO pool_of_record
        and a produced_source one requires it."""
        root = Path(d)
        for sub in ("profiles", "upgrades", "internet", "dest", "snapshot",
                    "live", "evidence"):
            (root / sub).mkdir(parents=True, exist_ok=True)
        retired = _pres_rec(RETIRED_ID, archive_state="draft")
        survivor = _pres_rec(SURVIVOR_ID)
        if doc is None:
            doc = _pres_doc([_pres_entry(retired, survivor)])
        self.cdoc = root / "upgrades" / "asset-collapse.studio-a.json"
        self.cdoc.write_text(json.dumps(doc, indent=1), encoding="utf-8")
        (root / "profiles" / "studio-a.assets.json").write_text(
            json.dumps([survivor]), encoding="utf-8")
        (root / "profiles" / "studio-a.posts.json").write_text("[]", encoding="utf-8")

        rows = _paths(20) + [survivor["file_path"], retired["file_path"]]
        dest = root / "dest"
        (dest / "MANIFEST.json").write_text(json.dumps([survivor, retired]),
                                            encoding="utf-8")
        (dest / "posts.json").write_text("[]", encoding="utf-8")
        (dest / pres.CSV_NAME).write_bytes(_csv_bytes(rows))
        for rec in (survivor, retired):
            p = dest / rec["file_path"]
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(PRESERVED_BYTES)
        for tree in ("live", "snapshot"):
            for rec in (survivor, retired):
                p = root / tree / rec["file_path"]
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_bytes(PRESERVED_BYTES)
            shutil.copyfile(dest / pres.CSV_NAME, root / tree / pres.CSV_NAME)
        self.manifest = root / "evidence" / "m.json"
        self.transform = root / "evidence" / "t.json"
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(pres.main([
                "snapshot-manifest", "--snapshot", str(root / "snapshot"),
                "--live-site", str(root / "live"), "--staging", str(dest),
                "--out", str(self.manifest)]), 0)
            self.assertEqual(pres.main([
                "csv-transform", "--snapshot", str(root / "snapshot"),
                "--live-site", str(root / "live"), "--staging", str(dest),
                "--collapse-document", str(self.cdoc),
                "--out", str(self.transform)]), 0)
        self.good = json.loads(self.transform.read_text())
        return root

    def _run(self, root, *extra):
        return subprocess.run(
            [sys.executable, str(SCRIPTS / "populate_archive.py"),
             "--preserved-roots", "--internet-source", str(root / "internet"),
             "--profile", str(root / "profiles" / "studio-a.assets.json"),
             "--posts", str(root / "profiles" / "studio-a.posts.json"),
             "--csv-transform", str(self.transform),
             "--live-site", str(root / "live"),
             "--frozen-snapshot", str(root / "snapshot"),
             "--snapshot-manifest", str(self.manifest),
             "--dest", str(root / "dest"), *extra],
            capture_output=True, text=True)

    def _reexpect(self, doc, root):
        """Re-derive the transform's own arithmetic so the forgery is
        INTERNALLY CONSISTENT: that is the whole point of these cases."""
        blob = (root / "snapshot" / pres.CSV_NAME).read_bytes()
        header, rows = pres.split_csv_records(blob)
        idx = pres.record_fields(header).index("file_path")
        gone = {r["file_path"] for r in doc["removals"]}
        kept = [r for r in rows if pres.record_fields(r)[idx] not in gone]
        digs = [pres._sha(r) for r in kept]
        after = header + b"".join(kept)
        doc["expected"] = {"sha256": pres._sha(after), "bytes": len(after),
                           "data_rows": len(kept),
                           "ordered_digest": pres.ordered_digest(digs),
                           "row_digests": digs}
        self.transform.write_text(json.dumps(doc, indent=1), encoding="utf-8")
        return doc

    @staticmethod
    def _csv(root):
        return (root / "dest" / pres.CSV_NAME).read_bytes()

    # -- A: the honest case ------------------------------------------------

    def test_a_valid_transform_and_the_matching_document_pass(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            self.assertEqual(len(self.good["removals"]), 1)
            self.assertEqual(self.good["profile"], "studio-a.assets.json")
            self.assertEqual(self.good[pres.COLLAPSE_BINDING_KEY]["retired_ids"],
                             [RETIRED_ID])
            r = self._run(root)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(pres.verify_csv_transform(self.good, self._csv(root)), [])

    def test_the_binding_does_not_break_idempotency(self):
        """⛔ THE RECOMPUTATION NEEDS THE PRE-OPERATION ROWS. Caught by running
        the publish twice: on the second run the documented rows are already
        gone, so intersecting the document's retirements with the rows present
        comes back EMPTY and a naive recomputation refuses a correct no-op.
        The identity half still holds there and the bytes are separately proved
        to be the documented result, so the write path never loses the full
        check."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            first = self._run(root)
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertIn("wrote", first.stderr)
            after_first = self._csv(root)
            second = self._run(root)
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertIn("already exactly the documented transform",
                          second.stderr)
            self.assertNotIn("not authorised", second.stderr)
            self.assertEqual(self._csv(root), after_first,
                             "the second run must write nothing")
            third = self._run(root)
            self.assertEqual(third.returncode, 0, third.stderr)
            self.assertEqual(self._csv(root), after_first)

    # -- B: an extra removal nobody authorised -----------------------------

    def test_b_an_extra_removal_with_an_unknown_id_refuses_before_any_write(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            before = self._csv(root)
            doc = json.loads(json.dumps(self.good))
            doc["removals"] = sorted(
                doc["removals"] + [{"file_path": _paths(1)[0],
                                    "retired_id": OTHER_ID}],
                key=lambda r: r["file_path"])
            self._reexpect(doc, root)
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("retired_id the bound collapse document does not hold",
                          r.stderr)
            self.assertEqual(self._csv(root), before)

    def test_b_a_real_id_pointed_at_the_wrong_row_refuses_before_any_write(self):
        """⛔ THE 23rd ROW. The id is one the document really holds, so the
        structural check passes and only the RECOMPUTATION against the
        pre-operation CSV catches it."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            before = self._csv(root)
            doc = json.loads(json.dumps(self.good))
            doc["removals"] = [{"file_path": _paths(1)[0], "retired_id": RETIRED_ID}]
            self._reexpect(doc, root)
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("not exactly the ones the current collapse document "
                          "authorises", r.stderr)
            self.assertIn("does NOT retire", r.stderr)
            self.assertIn("Refusing before writing anything", r.stderr)
            self.assertEqual(self._csv(root), before)

    # -- C: a documented removal quietly left in place ---------------------

    def test_c_a_transform_omitting_a_documented_removal_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            before = self._csv(root)
            doc = json.loads(json.dumps(self.good))
            doc["removals"] = []
            self._reexpect(doc, root)
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("DOES retire that this transform leaves in place",
                          r.stderr)
            self.assertEqual(self._csv(root), before)

    # -- D: a different document or profile --------------------------------

    def test_d_a_transform_built_for_another_profile_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            doc = json.loads(json.dumps(self.good))
            doc["profile"] = "studio-b.assets.json"
            doc[pres.COLLAPSE_BINDING_KEY]["profile"] = "studio-b.assets.json"
            self.transform.write_text(json.dumps(doc, indent=1), encoding="utf-8")
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("authority about one profile's retirements", r.stderr)

    def test_d_a_transform_built_from_another_document_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            doc = json.loads(json.dumps(self.good))
            doc[pres.COLLAPSE_BINDING_KEY]["sha256"] = "e" * 64
            self.transform.write_text(json.dumps(doc, indent=1), encoding="utf-8")
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("It is STALE", r.stderr)

    def test_d_a_retirement_set_that_disagrees_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            doc = json.loads(json.dumps(self.good))
            doc[pres.COLLAPSE_BINDING_KEY]["retired_ids"] = sorted(
                [RETIRED_ID, OTHER_ID])
            doc[pres.COLLAPSE_BINDING_KEY]["entries"] = 2
            self.transform.write_text(json.dumps(doc, indent=1), encoding="utf-8")
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("retirement set disagrees with the current document",
                          r.stderr)

    # -- E: a stale transform ---------------------------------------------

    def test_e_a_stale_transform_after_the_document_changes_refuses(self):
        """⛔ THE DIGEST IS OVER THE DOCUMENT BYTES ON PURPOSE. A comment edit
        invalidates the transform and the operator re-emits it, which costs
        one command; the alternative is a transform that keeps claiming
        authority from a document it has not seen."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            before = self._csv(root)
            doc = json.loads(self.cdoc.read_text())
            doc["_why"].append("a later edit nobody re-emitted the transform for")
            self.cdoc.write_text(json.dumps(doc, indent=1), encoding="utf-8")
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("It is STALE", r.stderr)
            self.assertIn("re-emit it from the current document", r.stderr)
            self.assertEqual(self._csv(root), before)

    def test_a_transform_claiming_no_document_where_one_exists_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            doc = json.loads(json.dumps(self.good))
            doc[pres.COLLAPSE_BINDING_KEY] = None
            doc["removals"] = []
            self._reexpect(doc, root)
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("Absence is not permission", r.stderr)

    def test_a_transform_claiming_a_document_where_none_exists_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            self.cdoc.unlink()
            r = self._run(root)
            self.assertEqual(r.returncode, 2, r.stderr)
            self.assertIn("authorised by nothing that is in hand", r.stderr)

    # -- F: the site_a shape ----------------------------------------------

    def test_f_the_zero_removal_case_remains_valid_and_bound(self):
        """F. site_a: one `hq` retirement whose produced render has NO CSV
        row, so the authorised removal set is EMPTY and the file must stay
        byte-identical. The binding still has to hold; a zero-removal
        transform is an expectation, not an exemption."""
        with tempfile.TemporaryDirectory() as d:
            hq_ret = _collapse_rec(RETIRED_ID, archive_state="draft")
            hq_sur = _collapse_rec(SURVIVOR_ID)
            root = self._world(d, doc=_collapse_doc([
                _collapse_entry(hq_ret, hq_sur)]))
            self.assertEqual(self.good["removals"], [])
            self.assertEqual(self.good["expected"]["sha256"],
                             self.good["original"]["sha256"])
            self.assertEqual(self.good[pres.COLLAPSE_BINDING_KEY]["retired_ids"],
                             [RETIRED_ID])
            before = self._csv(root)
            # `hq` needs its pack root, which this world has not got; the CSV
            # binding is proved BEFORE Layer B, so the refusal is the pack
            # one and the CSV is untouched either way.
            r = self._run(root, "--dry-run")
            self.assertNotIn("not authorised by the collapse document", r.stderr)
            self.assertEqual(self._csv(root), before)
            # and the transform itself verifies against the unchanged bytes
            self.assertEqual(pres.verify_csv_transform(self.good, before), [])

    def test_a_zero_removal_transform_still_refuses_a_changed_file(self):
        with tempfile.TemporaryDirectory() as d:
            hq_ret = _collapse_rec(RETIRED_ID, archive_state="draft")
            hq_sur = _collapse_rec(SURVIVOR_ID)
            root = self._world(d, doc=_collapse_doc([
                _collapse_entry(hq_ret, hq_sur)]))
            blob = self._csv(root)
            h, rows = pres.split_csv_records(blob)
            self.assertTrue(pres.verify_csv_transform(
                self.good, h + b"".join(rows[:-1])))

    # -- the site_b shape, at its real scale -------------------------------

    def test_the_site_b_shape_proves_exactly_its_22_retirements(self):
        """The shape PR #2 will need, exercised through a REAL collapse
        document rather than a synthetic binding: 22 retirements, all 22 paths
        present in the CSV, 1,206 rows to 1,184.

        Three things are proved at that scale, because an off-by-one in the
        intersection would be invisible at N=1: exactly those 22 rows are the
        authorised removals, no 23rd row is authorised, and no documented
        retirement is omitted.
        """
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            for sub in ("profiles", "upgrades", "internet", "dest", "snapshot",
                        "live", "evidence"):
                (root / sub).mkdir(parents=True)
            # 22 retirements, each retiring one record onto a survivor
            rows = _paths(1206)
            entries, survivors = [], []
            for i in range(22):
                rid = f"{i:08d}-1111-2222-3333-444444444444"
                sid = f"{i:08d}-5555-6666-7777-888888888888"
                ret = _pres_rec(rid, name=f"retired{i}")
                sur = _pres_rec(sid, name=f"survivor{i}")
                # the retired record's produced file IS one of the CSV rows
                ret["file_path"] = rows[i * 7]
                sha = hashlib.sha256(f"bytes-{i}".encode()).hexdigest()
                entries.append(_pres_entry(ret, sur, retired_sha=sha))
                survivors.append(sur)
            doc = _pres_doc(entries)
            cdoc = root / "upgrades" / "asset-collapse.studio-a.json"
            cdoc.write_text(json.dumps(doc, indent=1), encoding="utf-8")
            (root / "profiles" / "studio-a.assets.json").write_text(
                json.dumps(survivors), encoding="utf-8")
            (root / "dest" / pres.CSV_NAME).write_bytes(
                _csv_bytes(rows, embed_newline_every=134))
            shutil.copyfile(root / "dest" / pres.CSV_NAME,
                            root / "snapshot" / pres.CSV_NAME)
            out = root / "evidence" / "t.json"
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(pres.main([
                    "csv-transform", "--snapshot", str(root / "snapshot"),
                    "--live-site", str(root / "live"),
                    "--staging", str(root / "dest"),
                    "--collapse-document", str(cdoc), "--out", str(out)]), 0)
            tdoc = pres.load_csv_transform(out)
            self.assertEqual(tdoc["original"]["data_rows"], 1206)
            self.assertEqual(tdoc["expected"]["data_rows"], 1184)
            self.assertEqual(len(tdoc["removals"]), 22)

            loaded = ac.load_collapse_document(cdoc)
            blob = (root / "snapshot" / pres.CSV_NAME).read_bytes()
            raw = cdoc.read_bytes()
            # exactly those 22, against the real document
            self.assertEqual(pres.binding_refusals(
                tdoc, profile_name="studio-a.assets.json", collapse_doc=loaded,
                collapse_raw=raw, csv_blob=blob), [])
            self.assertEqual(
                [r["file_path"] for r in tdoc["removals"]],
                sorted(rows[i * 7] for i in range(22)))

            # a 23rd row is not authorised
            extra = json.loads(json.dumps(tdoc))
            extra["removals"] = sorted(
                extra["removals"] + [{"file_path": rows[3],
                                      "retired_id": entries[0]["retired_id"]}],
                key=lambda r: r["file_path"])
            bad = pres.binding_refusals(
                extra, profile_name="studio-a.assets.json", collapse_doc=loaded,
                collapse_raw=raw, csv_blob=blob)
            self.assertTrue(any("does NOT retire" in x for x in bad), bad)

            # and none of the 22 may be omitted
            for drop in (0, 11, 21):
                short = json.loads(json.dumps(tdoc))
                del short["removals"][drop]
                bad = pres.binding_refusals(
                    short, profile_name="studio-a.assets.json",
                    collapse_doc=loaded, collapse_raw=raw, csv_blob=blob)
                with self.subTest(dropped=drop):
                    self.assertTrue(any("DOES retire" in x for x in bad), bad)

            # the transform itself still applies byte-exactly, bare LFs included
            after, refusals = pres.transform_csv(blob, tdoc)
            self.assertEqual(refusals, [])
            self.assertEqual(pres.verify_csv_transform(tdoc, after), [])
            header, kept = pres.split_csv_records(after)
            self.assertEqual(len(kept), 1184)

    # -- G: the verifier ---------------------------------------------------

    NAME = "metadata.csv transform is authorised by the collapse document"

    def test_g_the_verifier_refuses_a_transform_whose_authority_moved(self):
        """G. After the write the recomputation is impossible (the site holds
        the POST-operation CSV), so the verifier proves the identity half. A
        document that has moved since the transform was emitted fails here,
        which is what stops a stale transform being blessed after the fact."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            r = self._run(root)
            self.assertEqual(r.returncode, 0, r.stderr)
            prof = root / "profiles" / "studio-a.assets.json"
            posts = root / "profiles" / "studio-a.posts.json"
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, root / "dest",
                                collapse_path=self.cdoc,
                                csv_transform=self.transform)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.PASS)
            # the document moves; the same site and the same transform now fail
            doc = json.loads(self.cdoc.read_text())
            doc["_why"].append("moved after the publish")
            self.cdoc.write_text(json.dumps(doc, indent=1), encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(prof, posts, root / "dest",
                                collapse_path=self.cdoc,
                                csv_transform=self.transform)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.FAIL)
            self.assertFalse(rep.ok)

    def test_g_an_unusable_document_is_not_read_as_an_absent_one(self):
        """⛔ UNUSABLE IS NOT ABSENT. Without the distinction the verdict would
        say "no collapse document exists", which is a softer and different
        claim, and the transform's removals would look unauthorised-but-fine
        rather than unauthorised-and-refused."""
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            self.cdoc.write_text("{nope", encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(root / "profiles" / "studio-a.assets.json",
                                root / "profiles" / "studio-a.posts.json",
                                root / "dest", collapse_path=self.cdoc,
                                csv_transform=self.transform)
            v = _verdict(rep, self.NAME)
            self.assertEqual(v.status, vs.FAIL)
            self.assertIn("Unusable is not absent", v.detail)

    def test_g_the_verifier_refuses_a_transform_for_another_profile(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            doc = json.loads(json.dumps(self.good))
            doc["profile"] = "studio-b.assets.json"
            self.transform.write_text(json.dumps(doc, indent=1), encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()):
                rep = vs.verify(root / "profiles" / "studio-a.assets.json",
                                root / "profiles" / "studio-a.posts.json",
                                root / "dest", collapse_path=self.cdoc,
                                csv_transform=self.transform)
            self.assertEqual(_verdict(rep, self.NAME).status, vs.FAIL)

    # -- the emitter -------------------------------------------------------

    def test_the_emitter_demands_an_explicit_document_or_an_explicit_absence(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            trees = ["--snapshot", str(root / "snapshot"),
                     "--live-site", str(root / "live"),
                     "--staging", str(root / "dest")]
            out = root / "evidence" / "x.json"
            with contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(pres.main(
                    ["csv-transform", *trees, "--out", str(out)]), 2)
            self.assertIn("exactly one of --collapse-document", err.getvalue())
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(pres.main(
                    ["csv-transform", *trees, "--collapse-document", str(self.cdoc),
                     "--no-collapse-document", "--out", str(out)]), 2)

    def test_the_emitter_refuses_an_unusable_collapse_document(self):
        with tempfile.TemporaryDirectory() as d:
            root = self._world(d)
            self.cdoc.write_text("{nope", encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(pres.main([
                    "csv-transform", "--snapshot", str(root / "snapshot"),
                    "--live-site", str(root / "live"),
                    "--staging", str(root / "dest"),
                    "--collapse-document", str(self.cdoc),
                    "--out", str(root / "evidence" / "x.json")]), 2)
            self.assertIn("would not be either", err.getvalue())


class TestAuthoredPlateScratchBoundary(unittest.TestCase):
    """F/G of correction 1. The plates are built OUTSIDE every operation tree.

    ⛔ A scratch directory inside live or staging would be published and
    pruned as if it were content; one inside the snapshot would be attested as
    if it were part of what the snapshot froze; one inside the evidence would
    sit in the very document set it is checked against.
    """

    def _world(self, d):
        root = Path(d)
        for sub in ("live", "staging", "snapshot", "evidence", "scratch"):
            (root / sub).mkdir(parents=True)
        gen = root / "snapshot" / "images" / "aurora-generated"
        gen.mkdir(parents=True)
        return root, gen

    def _build(self, root, gen, out, *extra):
        argv = ["build", "--generated-source", str(gen), "--out", str(out),
                "--snapshot", str(root / "snapshot"),
                "--live-site", str(root / "live"),
                "--staging", str(root / "staging"), *extra]
        with unittest.mock.patch.object(sys, "argv", ["authored_plates.py"] + argv), \
                contextlib.redirect_stderr(io.StringIO()) as err:
            try:
                rc = ap.main()
            except SystemExit as e:
                # `build_mood_board` raises SystemExit for a missing source
                # plate. That is PAST every boundary check, which is what the
                # accept-the-clean-case test is asserting.
                return 0, err.getvalue() + str(e)
        return rc, err.getvalue()

    def test_f_scratch_under_live_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, gen = self._world(d)
            rc, err = self._build(root, gen, root / "live" / "aurora-authored")
            self.assertEqual(rc, 2)
            self.assertIn("preserved-operation path refusal", err)

    def test_g_scratch_under_staging_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, gen = self._world(d)
            rc, err = self._build(root, gen, root / "staging" / "aurora-authored")
            self.assertEqual(rc, 2)
            self.assertIn("preserved-operation path refusal", err)

    def test_scratch_under_the_snapshot_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, gen = self._world(d)
            rc, err = self._build(root, gen, root / "snapshot" / "scratch")
            self.assertEqual(rc, 2)
            self.assertIn("preserved-operation path refusal", err)

    def test_scratch_under_the_evidence_refuses(self):
        with tempfile.TemporaryDirectory() as d:
            root, gen = self._world(d)
            rc, err = self._build(root, gen, root / "evidence" / "scratch",
                                  "--evidence", str(root / "evidence"))
            self.assertEqual(rc, 2)
            self.assertIn("preserved-operation path refusal", err)

    def test_a_missing_tree_refuses_rather_than_skipping_the_check(self):
        with tempfile.TemporaryDirectory() as d:
            root, gen = self._world(d)
            argv = ["build", "--generated-source", str(gen),
                    "--out", str(root / "scratch")]
            with unittest.mock.patch.object(
                    sys, "argv", ["authored_plates.py"] + argv), \
                    contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(ap.main(), 2)
            self.assertIn("--snapshot, --live-site, --staging", err.getvalue())
            self.assertIn("unprovable boundary", err.getvalue())

    def test_a_generated_source_outside_the_snapshot_refuses(self):
        """The sampled pixels are an INPUT to a hash the profile records, so
        they must come from the attested tree."""
        with tempfile.TemporaryDirectory() as d:
            root, _gen = self._world(d)
            loose = root / "elsewhere" / "aurora-generated"
            loose.mkdir(parents=True)
            rc, err = self._build(root, loose, root / "scratch")
            self.assertEqual(rc, 2)
            self.assertIn("is not inside --snapshot", err)

    def test_a_clean_scratch_directory_is_accepted_and_the_build_runs(self):
        """The boundary must not refuse the correct configuration: it gets
        past every check and fails only on the missing source plate."""
        with tempfile.TemporaryDirectory() as d:
            root, gen = self._world(d)
            _rc, err = self._build(root, gen, root / "scratch" / "aurora-authored")
            self.assertNotIn("preserved-operation path refusal", err)
            self.assertNotIn("is not inside --snapshot", err)
            self.assertNotIn("build needs", err)
            # it got as far as reading the source plate, which is past every
            # boundary check: the colour chart was even written.
            self.assertIn(ap.MOOD_BOARD_SOURCE, err)
            self.assertTrue((root / "scratch" / "aurora-authored"
                             / ap.PLATES[0]).is_file())

if __name__ == "__main__":
    unittest.main(verbosity=2)
