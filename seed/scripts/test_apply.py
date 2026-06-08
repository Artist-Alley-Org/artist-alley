#!/usr/bin/env python3
"""
Tests for apply.py — focused on the load-bearing logic that doesn't
require a live AA instance:
  - State tracker persistence + resume semantics
  - AAClient retry + backoff
  - Stable UUID matches sanitize_and_assemble's algorithm
  - Content-type mapping
  - Catalogue loader
  - Password recovery (recover_admin.py)

Integration tests against a real instance live in
test_apply_integration.py and are gated on AA_INTEGRATION_TEST=1.

Run:
    python3 -m pytest test_apply.py -v
"""

from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path
from unittest import mock

# Add scripts/ to path so apply.py imports work
sys.path.insert(0, str(Path(__file__).resolve().parent))
from apply import (
    State, AAClient, APIError, Catalogues,
    _stable_uuid, _guess_content_type,
    WORKFLOW_STATE_ALIASES, site_key_from_path,
)
import recover_admin


class TestStableUUID(unittest.TestCase):
    """The stable UUID must match sanitize_and_assemble.py's algorithm
    so apply can derive expected IDs without coordination."""

    def test_deterministic(self):
        a = _stable_uuid("asset", "ast-91f47307b0")
        b = _stable_uuid("asset", "ast-91f47307b0")
        self.assertEqual(a, b)

    def test_uuid_shape(self):
        u = _stable_uuid("comment", "post-id", "asset-id")
        # UUID format: 8-4-4-4-12 hex chars
        self.assertRegex(u, r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

    def test_different_parts_different_uuid(self):
        a = _stable_uuid("asset", "x")
        b = _stable_uuid("post", "x")
        self.assertNotEqual(a, b)


class TestContentType(unittest.TestCase):
    def test_image_extensions(self):
        self.assertEqual(_guess_content_type("png"), "image/png")
        self.assertEqual(_guess_content_type("jpg"), "image/jpeg")
        self.assertEqual(_guess_content_type("webp"), "image/webp")

    def test_video_extensions(self):
        self.assertEqual(_guess_content_type("mp4"), "video/mp4")
        self.assertEqual(_guess_content_type("webm"), "video/webm")
        self.assertEqual(_guess_content_type("mkv"), "video/x-matroska")

    def test_3d_extensions(self):
        self.assertEqual(_guess_content_type("glb"), "model/gltf-binary")
        self.assertEqual(_guess_content_type("gltf"), "model/gltf+json")
        self.assertEqual(_guess_content_type("fbx"), "model/fbx")

    def test_document_extensions(self):
        self.assertEqual(_guess_content_type("pdf"), "application/pdf")
        self.assertEqual(_guess_content_type("epub"), "application/epub+zip")

    def test_unknown_extension(self):
        self.assertEqual(_guess_content_type("xyz"), "application/octet-stream")

    def test_case_and_leading_dot(self):
        self.assertEqual(_guess_content_type(".PNG"), "image/png")
        self.assertEqual(_guess_content_type("PDF"), "application/pdf")


class TestState(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.tmp = tempfile.mkdtemp(prefix="apply-test-")
        self.path = Path(self.tmp) / "state.json"

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_fresh_state_empty(self):
        s = State(self.path, target_host="h:a")
        self.assertEqual(s.data["phases_completed"], [])
        self.assertEqual(s.data["target_host"], "h:a")

    def test_phase_completion_persists(self):
        s = State(self.path, target_host="h:a")
        s.mark_phase_done("users")
        # Re-load and check
        s2 = State(self.path, target_host="h:a")
        self.assertIn("users", s2.data["phases_completed"])
        self.assertTrue(s2.is_phase_done("users"))

    def test_idempotent_mark(self):
        s = State(self.path, target_host="h:a")
        s.mark_phase_done("users")
        s.mark_phase_done("users")
        self.assertEqual(s.data["phases_completed"].count("users"), 1)

    def test_user_registry(self):
        s = State(self.path, target_host="h:a")
        s.set_user("alice", 42)
        self.assertEqual(s.get_user("alice"), 42)
        self.assertIsNone(s.get_user("bob"))

    def test_entity_registry(self):
        s = State(self.path, target_host="h:a")
        s.set_entity("assets", "seed-uuid", "server-uuid")
        self.assertEqual(s.get_entity("assets", "seed-uuid"), "server-uuid")
        self.assertIsNone(s.get_entity("assets", "missing"))

    def test_workflow_state_composite_key(self):
        s = State(self.path, target_host="h:a")
        s.set_workflow_state("asset", "draft", "uuid-1")
        s.set_workflow_state("asset:1", "draft", "uuid-2")
        self.assertEqual(s.get_workflow_state("asset", "draft"), "uuid-1")
        self.assertEqual(s.get_workflow_state("asset:1", "draft"), "uuid-2")

    def test_reset_preserves_registries(self):
        """Reset clears phase completions but keeps the ID maps —
        re-running apply against the same instance shouldn't have
        to look up entities that were already created."""
        s = State(self.path, target_host="h:a")
        s.set_user("alice", 1)
        s.mark_phase_done("users")
        s.reset()
        s2 = State(self.path, target_host="h:a")
        self.assertEqual(s2.data["phases_completed"], [])
        self.assertEqual(s2.get_user("alice"), 1)

    def test_target_host_mismatch_ignored(self):
        """Loading a state file for a different host doesn't merge —
        avoids cross-instance contamination."""
        s = State(self.path, target_host="h:a")
        s.set_user("alice", 1)
        # Different host should ignore the file
        s2 = State(self.path, target_host="other:b")
        self.assertEqual(s2.get_user("alice"), None)


class TestAAClient(unittest.TestCase):
    def setUp(self):
        self.client = AAClient("http://example.test", dry_run=False)

    def test_dry_run_skips_network(self):
        c = AAClient("http://example.test", dry_run=True)
        # Should return synthetic response without hitting network
        result = c.post("/assets", json={"title": "x"})
        self.assertIn("_dry_run", result)

    def test_upload_dry_run_returns_synthetic_hash(self):
        c = AAClient("http://example.test", dry_run=True)
        result = c.upload("/storage/objects", b"hello world",
                          content_type="text/plain")
        import hashlib as _h
        self.assertEqual(result["hash"], _h.sha256(b"hello world").hexdigest())
        self.assertEqual(result["size_bytes"], 11)

    @mock.patch("apply.requests.Session.request")
    def test_retries_on_500(self, mock_req):
        # First two calls return 500, third returns 200
        def side_effect(*args, **kwargs):
            resp = mock.Mock()
            resp.headers = {}
            resp.content = b'{"ok": true}'
            if mock_req.call_count <= 2:
                resp.status_code = 500
                resp.text = "internal error"
            else:
                resp.status_code = 200
                resp.json = lambda: {"ok": True}
            return resp
        mock_req.side_effect = side_effect
        # Patch sleep so the test is fast
        with mock.patch("apply.time.sleep"):
            result = self.client.get("/foo")
        self.assertEqual(result, {"ok": True})
        self.assertEqual(mock_req.call_count, 3)

    @mock.patch("apply.requests.Session.request")
    def test_terminal_400_raises(self, mock_req):
        resp = mock.Mock()
        resp.status_code = 400
        resp.headers = {}
        resp.content = b"bad"
        resp.text = "bad input"
        mock_req.return_value = resp
        with self.assertRaises(APIError) as cm:
            self.client.post("/assets", json={})
        self.assertEqual(cm.exception.status, 400)
        self.assertEqual(mock_req.call_count, 1)  # No retry on 4xx

    @mock.patch("apply.requests.Session.request")
    def test_respects_retry_after_header(self, mock_req):
        # 429 with Retry-After: 0 (instant); then 200
        calls = [0]
        def side_effect(*args, **kwargs):
            calls[0] += 1
            resp = mock.Mock()
            resp.content = b'{"ok": true}'
            if calls[0] == 1:
                resp.status_code = 429
                resp.headers = {"Retry-After": "0"}
                resp.text = "rate limited"
            else:
                resp.status_code = 200
                resp.headers = {}
                resp.json = lambda: {"ok": True}
            return resp
        mock_req.side_effect = side_effect
        with mock.patch("apply.time.sleep") as mock_sleep:
            self.client.get("/foo")
            # Retry-After of "0" should produce a sleep of 0.0
            mock_sleep.assert_called_once_with(0.0)


class TestSiteKey(unittest.TestCase):
    def test_explicit_a(self):
        self.assertEqual(site_key_from_path(Path("/x/site_a")), "a")
    def test_explicit_b(self):
        self.assertEqual(site_key_from_path(Path("/x/site_b")), "b")
    def test_short_a(self):
        self.assertEqual(site_key_from_path(Path("/x/a")), "a")
    def test_unknown(self):
        self.assertEqual(site_key_from_path(Path("/x/other")), "other")


class TestWorkflowAliases(unittest.TestCase):
    """The seed dataset uses richer state names than AA's pre-seeded
    states; apply collapses them via WORKFLOW_STATE_ALIASES. This test
    pins the mapping so changes are deliberate."""

    def test_asset_aliases_cover_all_seed_names(self):
        seed_names = {"draft", "in_review", "approved", "final", "archived"}
        covered = {name for (domain, name) in WORKFLOW_STATE_ALIASES if domain == "asset"}
        self.assertSetEqual(seed_names, covered)

    def test_post_aliases_cover_all_seed_names(self):
        seed_names = {"draft", "in_review", "approved", "final", "archived"}
        covered = {name for (domain, name) in WORKFLOW_STATE_ALIASES if domain == "post"}
        self.assertSetEqual(seed_names, covered)

    def test_approved_and_final_collapse_to_published(self):
        self.assertEqual(WORKFLOW_STATE_ALIASES[("asset", "approved")], "published")
        self.assertEqual(WORKFLOW_STATE_ALIASES[("asset", "final")], "published")


class TestCatalogues(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.tmp = Path(tempfile.mkdtemp(prefix="apply-test-cat-"))
        self.cat_root = self.tmp / "profiles"
        self.cat_root.mkdir()
        self.site_root = self.tmp / "site_a"
        self.site_root.mkdir()
        for name, content in {
            "dataset.users.json": [{"username": "alice", "full_name": "Alice"}],
            "dataset.teams.json": [{"name": "Characters"}],
            "dataset.collections.json": [{"name": "Project Echo"}],
            "dataset.field_definitions.json": [{"name": "pipeline_stage", "label": "Stage", "type": "select"}],
            "dataset.workflow.json": {"states": [], "transitions": []},
        }.items():
            (self.cat_root / name).write_text(json.dumps(content))
        (self.site_root / "MANIFEST.json").write_text(json.dumps([
            {"id": "uuid-a", "title": "A", "asset_type": "image"},
        ]))
        (self.site_root / "posts.json").write_text(json.dumps([
            {"id": "p1", "title": "Post 1", "asset_ids": ["uuid-a"]},
        ]))

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_load_reads_all_required_files(self):
        cat = Catalogues.load(self.cat_root, self.site_root, "a")
        self.assertEqual(len(cat.users), 1)
        self.assertEqual(len(cat.teams), 1)
        self.assertEqual(len(cat.collections), 1)
        self.assertEqual(len(cat.fields), 1)
        self.assertEqual(len(cat.assets), 1)
        self.assertEqual(len(cat.posts), 1)

    def test_load_prefers_site_manifest_over_studio_file(self):
        # Create a studio-a file with a different shape
        (self.cat_root / "studio-a.assets.json").write_text(json.dumps([
            {"id": "different", "title": "Different"},
        ]))
        cat = Catalogues.load(self.cat_root, self.site_root, "a")
        # Site MANIFEST.json should win
        self.assertEqual(cat.assets[0]["id"], "uuid-a")

    def test_load_missing_catalogue_raises(self):
        (self.cat_root / "dataset.users.json").unlink()
        with self.assertRaises(FileNotFoundError):
            Catalogues.load(self.cat_root, self.site_root, "a")


class TestRecoverAdmin(unittest.TestCase):
    def test_extract_dev_banner_pattern(self):
        text = """
INFO server starting
WARN: ================================================================
WARN: AA_BOOTSTRAP_DEFAULT_ADMIN=1 — DEVELOPMENT MODE
WARN:   username: admin   email: admin@localhost   password: ArtistAlleyMogul
WARN: ================================================================
INFO server ready
"""
        pw = recover_admin.extract_from_text(text)
        self.assertEqual(pw, "ArtistAlleyMogul")

    def test_extract_prod_log_pattern(self):
        text = "INFO bootstrap admin password: x7K9pQmR4vN8sT2hY6jL3bF5cZ1wU0aE"
        pw = recover_admin.extract_from_text(text)
        self.assertEqual(pw, "x7K9pQmR4vN8sT2hY6jL3bF5cZ1wU0aE")

    def test_no_match_returns_none(self):
        text = "INFO server starting\nINFO server ready"
        self.assertIsNone(recover_admin.extract_from_text(text))

    def test_filters_redacted_marker(self):
        text = "password: <redacted>"
        self.assertIsNone(recover_admin.extract_from_text(text))


if __name__ == "__main__":
    unittest.main(verbosity=2)
