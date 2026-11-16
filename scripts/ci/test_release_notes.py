#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Kenneth Blossom
"""
Unit tests for scripts/ci/release_notes.py, and static checks that release.yml and ci.yml use it.
Stdlib only, no network, no GitHub.

They pin: the exact file chosen for an exact tag, rejection of a missing, blank, heading-less or
wrong-version file, the package.json and tag agreement (a stable tag equals the version; a
prerelease tag equals it or extends its base, and needs its own file), and that the release
workflow publishes the file with --notes-file after the check and never falls back to generated
notes.

Run: python3 -I scripts/ci/test_release_notes.py
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import re
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent

spec = importlib.util.spec_from_file_location("release_notes", HERE / "release_notes.py")
rn = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rn)

GOOD = "## Artist Alley {tag}: A theme\n\nWhat changed, in prose.\n"


class Repo:
    """A throwaway repository root with docs/release-notes/ and web/package.json."""

    def __init__(self, version: str | None = "0.11.1"):
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        (self.root / "docs/release-notes").mkdir(parents=True)
        (self.root / "web").mkdir()
        if version is not None:
            (self.root / "web/package.json").write_text(json.dumps({"name": "x", "version": version}))

    def notes(self, tag: str, text: str | bytes) -> Path:
        p = self.root / "docs/release-notes" / f"{tag}.md"
        p.write_bytes(text if isinstance(text, bytes) else text.encode("utf-8"))
        return p

    def run(self, *args: str) -> tuple[int, str]:
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                code = rn.main([*args, "--root", str(self.root)])
            except SystemExit as e:  # argparse errors
                code = e.code
        return code, out.getvalue() + err.getvalue()

    def close(self):
        self._tmp.cleanup()


class CheckNotes(unittest.TestCase):
    def setUp(self):
        self.repo = Repo()

    def tearDown(self):
        self.repo.close()

    def test_exact_tag_selects_exactly_its_file(self):
        self.repo.notes("v0.11.10", GOOD.format(tag="v0.11.10"))
        self.repo.notes("v0.11.1", GOOD.format(tag="v0.11.1"))
        self.assertEqual(rn.check_notes("v0.11.1", self.repo.root), self.repo.root / "docs/release-notes/v0.11.1.md")
        self.assertEqual(rn.check_notes("v0.11.10", self.repo.root), self.repo.root / "docs/release-notes/v0.11.10.md")

    def test_prerelease_tag_uses_its_own_file(self):
        self.repo.notes("v0.12.0", GOOD.format(tag="v0.12.0"))
        with self.assertRaisesRegex(rn.NotesError, r"v0\.12\.0-rc\.1\.md does not exist"):
            rn.check_notes("v0.12.0-rc.1", self.repo.root)
        self.repo.notes("v0.12.0-rc.1", GOOD.format(tag="v0.12.0-rc.1"))
        self.assertTrue(rn.check_notes("v0.12.0-rc.1", self.repo.root).name == "v0.12.0-rc.1.md")

    def test_missing_file_is_rejected(self):
        with self.assertRaisesRegex(rn.NotesError, r"v0\.11\.1\.md does not exist.*RELEASING\.md"):
            rn.check_notes("v0.11.1", self.repo.root)

    def test_blank_and_whitespace_only_files_are_rejected(self):
        for text in ("", "\n\n", "   \n\t\n"):
            self.repo.notes("v0.11.1", text)
            with self.assertRaisesRegex(rn.NotesError, r"is blank"):
                rn.check_notes("v0.11.1", self.repo.root)

    def test_heading_only_file_is_rejected(self):
        self.repo.notes("v0.11.1", "## Artist Alley v0.11.1: A theme\n\n")
        with self.assertRaisesRegex(rn.NotesError, r"heading but no notes"):
            rn.check_notes("v0.11.1", self.repo.root)

    def test_first_line_must_be_a_heading(self):
        self.repo.notes("v0.11.1", "Artist Alley v0.11.1\n\nProse.\n")
        with self.assertRaisesRegex(rn.NotesError, r"must start with a Markdown heading"):
            rn.check_notes("v0.11.1", self.repo.root)
        self.repo.notes("v0.11.1", "##No space v0.11.1\n\nProse.\n")
        with self.assertRaisesRegex(rn.NotesError, r"must start with a Markdown heading"):
            rn.check_notes("v0.11.1", self.repo.root)

    def test_wrong_version_heading_is_rejected(self):
        for heading in (
            "## Artist Alley v0.11.0: copied from the last release",
            "## Artist Alley v0.11.10: a later patch",
            "## Artist Alley v0.11.1-rc.1: the release candidate",
            "## Artist Alley 0.11.1 without the v",
            "## Artist Alley xv0.11.1",
        ):
            self.repo.notes("v0.11.1", f"{heading}\n\nProse.\n")
            with self.assertRaisesRegex(rn.NotesError, r"does not name v0\.11\.1", msg=heading):
                rn.check_notes("v0.11.1", self.repo.root)

    def test_tag_followed_by_punctuation_is_accepted(self):
        for heading in ("## Artist Alley v0.11.1: theme", "# v0.11.1", "## Release v0.11.1.", "## (v0.11.1)"):
            self.repo.notes("v0.11.1", f"{heading}\n\nProse.\n")
            self.assertEqual(rn.check_notes("v0.11.1", self.repo.root).name, "v0.11.1.md", heading)

    def test_non_utf8_file_is_rejected(self):
        self.repo.notes("v0.11.1", b"## Artist Alley v0.11.1\n\n\xff\xfe broken\n")
        with self.assertRaisesRegex(rn.NotesError, r"not UTF-8"):
            rn.check_notes("v0.11.1", self.repo.root)

    def test_malformed_tags_are_rejected(self):
        for tag in ("0.11.1", "v0.11", "v0.11.1/../x", "latest", "v0.11.1 "):
            with self.assertRaisesRegex(rn.NotesError, r"is not a release tag", msg=tag):
                rn.notes_path(tag, self.repo.root)


class Cli(unittest.TestCase):
    def setUp(self):
        self.repo = Repo("0.11.1")

    def tearDown(self):
        self.repo.close()

    def test_package_json_version_names_the_file(self):
        self.repo.notes("v0.11.1", GOOD.format(tag="v0.11.1"))
        code, out = self.repo.run("--package-json", "web/package.json")
        self.assertEqual(code, 0, out)
        self.assertIn("ok: docs/release-notes/v0.11.1.md for v0.11.1", out)

    def test_version_bump_without_notes_fails(self):
        self.repo.notes("v0.11.1", GOOD.format(tag="v0.11.1"))
        (self.repo.root / "web/package.json").write_text(json.dumps({"version": "0.11.2"}))
        code, out = self.repo.run("--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("v0.11.2.md does not exist", out)

    def test_tag_must_match_package_json(self):
        self.repo.notes("v0.11.2", GOOD.format(tag="v0.11.2"))
        code, out = self.repo.run("--tag", "v0.11.2", "--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("does not match web/package.json version 0.11.1", out)

    def test_tag_and_package_json_agree(self):
        self.repo.notes("v0.11.1", GOOD.format(tag="v0.11.1"))
        code, out = self.repo.run("--tag", "v0.11.1", "--package-json", "web/package.json")
        self.assertEqual(code, 0, out)

    def test_unreadable_package_json_fails(self):
        (self.repo.root / "web/package.json").write_text("{not json")
        code, out = self.repo.run("--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("cannot read the version", out)

    def test_no_arguments_is_a_usage_error(self):
        code, _ = self.repo.run()
        self.assertEqual(code, 2)


class TagMatchesPackage(unittest.TestCase):
    def test_stable_tag_needs_the_exact_version(self):
        self.assertTrue(rn.tag_matches_package("v0.11.2", "0.11.2"))
        for version in ("0.11.1", "0.11.3", "0.12.0", "1.11.2", "0.11.2-rc.1"):
            self.assertFalse(rn.tag_matches_package("v0.11.2", version), version)

    def test_prerelease_tag_accepts_its_base_or_its_exact_version(self):
        for tag in ("v0.11.2-rc.1", "v0.11.2-beta.2"):
            self.assertTrue(rn.tag_matches_package(tag, "0.11.2"), tag)
            self.assertTrue(rn.tag_matches_package(tag, tag[1:]), tag)

    def test_prerelease_of_another_version_is_rejected(self):
        for version in ("0.11.1", "0.11.3", "0.12.0", "1.11.2", "0.11.2-rc.2", "0.11.2-beta.1"):
            self.assertFalse(rn.tag_matches_package("v0.11.2-rc.1", version), version)


class Prerelease(unittest.TestCase):
    """Prerelease tags keep release.yml's existing --prerelease path, with their own notes file."""

    def setUp(self):
        self.repo = Repo("0.11.2")
        self.repo.notes("v0.11.2", GOOD.format(tag="v0.11.2"))

    def tearDown(self):
        self.repo.close()

    def test_rc_and_beta_tags_on_the_base_version_pass_with_their_own_file(self):
        for tag in ("v0.11.2-rc.1", "v0.11.2-beta.2"):
            self.repo.notes(tag, GOOD.format(tag=tag))
            code, out = self.repo.run("--tag", tag, "--package-json", "web/package.json")
            self.assertEqual(code, 0, out)
            self.assertIn(f"ok: docs/release-notes/{tag}.md for {tag}", out)

    def test_prerelease_tag_needs_its_own_file_not_the_stable_one(self):
        code, out = self.repo.run("--tag", "v0.11.2-rc.1", "--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("v0.11.2-rc.1.md does not exist", out)

    def test_prerelease_notes_must_name_the_full_tag(self):
        self.repo.notes("v0.11.2-rc.1", GOOD.format(tag="v0.11.2"))
        code, out = self.repo.run("--tag", "v0.11.2-rc.1", "--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("does not name v0.11.2-rc.1", out)

    def test_exact_prerelease_package_version_passes(self):
        (self.repo.root / "web/package.json").write_text(json.dumps({"version": "0.11.2-rc.1"}))
        self.repo.notes("v0.11.2-rc.1", GOOD.format(tag="v0.11.2-rc.1"))
        code, out = self.repo.run("--tag", "v0.11.2-rc.1", "--package-json", "web/package.json")
        self.assertEqual(code, 0, out)

    def test_prerelease_of_an_unrelated_version_fails(self):
        for tag in ("v0.11.3-rc.1", "v0.12.0-rc.1", "v1.11.2-beta.1"):
            self.repo.notes(tag, GOOD.format(tag=tag))
            code, out = self.repo.run("--tag", tag, "--package-json", "web/package.json")
            self.assertEqual(code, 1, tag)
            self.assertIn(f"tag {tag} does not match web/package.json version 0.11.2", out)

    def test_stable_tag_against_a_prerelease_package_fails(self):
        (self.repo.root / "web/package.json").write_text(json.dumps({"version": "0.11.2-rc.1"}))
        code, out = self.repo.run("--tag", "v0.11.2", "--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("a stable tag must equal it", out)

    def test_other_prerelease_against_a_prerelease_package_fails(self):
        (self.repo.root / "web/package.json").write_text(json.dumps({"version": "0.11.2-rc.1"}))
        self.repo.notes("v0.11.2-rc.2", GOOD.format(tag="v0.11.2-rc.2"))
        code, out = self.repo.run("--tag", "v0.11.2-rc.2", "--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("tag v0.11.2-rc.2 does not match web/package.json version 0.11.2-rc.1", out)

    def test_malformed_tag_is_reported_before_the_version_comparison(self):
        code, out = self.repo.run("--tag", "0.11.2-rc.1", "--package-json", "web/package.json")
        self.assertEqual(code, 1)
        self.assertIn("is not a release tag", out)


class RealRepository(unittest.TestCase):
    """The checked-in state: the notes file for the current version exists and passes."""

    def test_current_version_has_valid_notes(self):
        version = json.loads((REPO / "web/package.json").read_text())["version"]
        path = rn.check_notes(f"v{version}", REPO)
        self.assertEqual(path, REPO / "docs/release-notes" / f"v{version}.md")

    def test_every_checked_in_notes_file_is_valid_for_its_own_tag(self):
        files = sorted((REPO / "docs/release-notes").glob("v*.md"))
        self.assertTrue(files, "no release notes files checked in")
        for f in files:
            self.assertEqual(rn.check_notes(f.stem, REPO), f)


class Workflows(unittest.TestCase):
    """Static checks: the release workflow cannot publish generated notes or skip the check."""

    @classmethod
    def setUpClass(cls):
        cls.release = (REPO / ".github/workflows/release.yml").read_text()
        cls.ci = (REPO / ".github/workflows/ci.yml").read_text()

    def _commands(self, text: str) -> str:
        """The workflow without comment lines, so a comment cannot satisfy or trip a check."""
        return "\n".join(ln for ln in text.splitlines() if not ln.lstrip().startswith("#"))

    def test_release_never_generates_notes(self):
        cmds = self._commands(self.release)
        self.assertNotIn("--generate-notes", cmds)
        self.assertNotIn("--notes-start-tag", cmds)
        self.assertNotRegex(cmds, r"gh release create[^\n]*\\\n(?:[^\n]*\\\n)*[^\n]*--notes \"")

    def test_release_publishes_the_exact_tag_file(self):
        cmds = self._commands(self.release)
        self.assertEqual(cmds.count("gh release create"), 1)
        self.assertIn('--notes-file "docs/release-notes/$TAG.md"', cmds)

    def test_check_runs_before_the_release_is_created(self):
        cmds = self._commands(self.release)
        check = cmds.find('scripts/ci/release_notes.py --tag "$TAG" --package-json web/package.json')
        create = cmds.find("gh release create")
        self.assertGreater(check, -1, "release.yml does not run the release-notes check")
        self.assertLess(check, create, "the check must run before gh release create")
        job = cmds[cmds.find("  release-notes:"):cmds.find("  docker:")]
        self.assertIn("release_notes.py --tag", job, "the check must be in the release-notes job")
        self.assertIn("gh release create", job)

    def test_release_keeps_title_prerelease_and_docker_dependency(self):
        cmds = self._commands(self.release)
        self.assertIn('--title "artist-alley $TAG"', cmds)
        self.assertRegex(cmds, r'\*-\*\) extra_flags="--prerelease"')
        self.assertRegex(cmds, r"\n  docker:\n(?:[^\n]*\n){0,3}\s+needs: release-notes\n")

    def test_ci_runs_the_preflight_and_its_tests(self):
        cmds = self._commands(self.ci)
        job = re.search(r"\n  release-notes:\n(.*?)(?=\n  [a-z][a-z0-9_-]*:\n|\Z)", cmds, re.S)
        self.assertIsNotNone(job, "ci.yml has no release-notes job")
        body = job.group(1)
        self.assertIn("python3 -I scripts/ci/test_release_notes.py", body)
        self.assertIn("python3 -I scripts/ci/release_notes.py --package-json web/package.json", body)
        self.assertNotIn("needs:", body, "the preflight must not be skipped by the docs-only gate")


if __name__ == "__main__":
    unittest.main(verbosity=2)
