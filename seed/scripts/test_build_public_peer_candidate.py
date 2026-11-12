#!/usr/bin/env python3
"""
Synthetic tests for build_public_peer_candidate.py (#1514).

Stdlib unittest only. Everything is generated in temporary directories:
a tiny fake bundle with three packs, a fake site_a, a fake contract and a
staged fixture with three base rows. No real dataset is read.

Run: python3 seed/scripts/test_build_public_peer_candidate.py -v
"""

from __future__ import annotations

import hashlib
import json
import os
import struct
import sys
import tempfile
import unittest
import uuid
import zlib
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_public_peer_candidate as b  # noqa: E402


def png(w: int, h: int, seed: int) -> bytes:
    """A valid w x h greyscale PNG whose pixel data depends on seed."""
    def chunk(t, d):
        return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    raw = b"".join(b"\x00" + bytes(((x + y + seed) % 256) for x in range(w)) for y in range(h))
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 0, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


NOTICE = b"Some Pack\n\nLicense: (Creative Commons Zero, CC0)\nhttp://creativecommons.org/publicdomain/zero/1.0/\n"


class Fixture:
    def __init__(self, td: Path, per_pack: int = 6, quotas=(3, 3, 2)):
        self.td = td
        self.bundle = td / "bundle"
        self.site_a = td / "site_a"
        self.fixture = td / "fixture"
        self.out = td / "out"
        self.packs = []
        names = ["2D assets/Alpha Pack", "2D assets/Beta Pack", "UI assets/Gamma Pack"]
        for i, (folder, q) in enumerate(zip(names, quotas)):
            base = self.bundle / folder
            (base / "PNG").mkdir(parents=True)
            (base / "License.txt").write_bytes(NOTICE + folder.encode())
            for k in range(per_pack):
                (base / "PNG" / f"tile_{k:02d}.png").write_bytes(png(64 + k, 64, 10 * i + k))
            (base / "PNG" / "tiny.png").write_bytes(png(16, 16, 99))       # too small
            (base / "PNG" / "notes.txt").write_bytes(b"not a png")          # ignored
            self.packs.append(b.Pack(folder, b.safe_segment(folder.split("/")[-1]), folder.split("/")[-1],
                                     f"https://example.invalid/{i}",
                                     hashlib.sha256(NOTICE + folder.encode()).hexdigest(), q))
        # site_a with two anchors (bytes equal to two base files) and an unrelated file
        self.site_a.mkdir()
        base_bytes = {"images/x/anchor1.png": png(70, 70, 201), "images/x/anchor2.png": png(71, 70, 202),
                      "images/x/control.png": png(72, 70, 203)}
        (self.site_a / "a1.png").write_bytes(base_bytes["images/x/anchor1.png"])
        (self.site_a / "a2.png").write_bytes(base_bytes["images/x/anchor2.png"])
        (self.site_a / "other.png").write_bytes(png(80, 80, 250))
        # staged fixture: three base rows + files
        rows = []
        for n, (rel, data) in enumerate(base_bytes.items()):
            p = self.fixture / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(data)
            rows.append({"id": str(uuid.uuid5(uuid.NAMESPACE_URL, f"base-{n}")), "file_path": rel,
                         "file_size_bytes": len(data), "metadata": {"candidate": "v1"}})
        (self.fixture / "MANIFEST.json").write_text(json.dumps(rows, indent=2) + "\n")
        (self.fixture / "posts.json").write_text("[]\n")
        self.rows = rows
        self.contract = td / "contract.json"
        self.contract.write_text(json.dumps({"fixtures": [
            {"name": "overlap-image-1", "sha256": hashlib.sha256(base_bytes["images/x/anchor1.png"]).hexdigest(),
             "assets": {"site_a": {"file_path": "a1.png"}, "site_b": {"asset_id": rows[0]["id"]}}},
            {"name": "overlap-image-2", "sha256": hashlib.sha256(base_bytes["images/x/anchor2.png"]).hexdigest(),
             "assets": {"site_a": {"file_path": "a2.png"}, "site_b": {"asset_id": rows[1]["id"]}}},
        ]}))
        total = sum(quotas)
        # most 3-asset posts that leave an even remainder for 2-asset posts
        three = next(n for n in range(total // 3, -1, -1) if (total - 3 * n) % 2 == 0)
        two = (total - 3 * three) // 2
        self.cfg = b.Config(
            packs=self.packs, fill_order=[p.folder for p in self.packs],
            base_rows={r["id"]: b.canonical_sha(r) for r in rows},
            base_files={rel: (hashlib.sha256(d).hexdigest(), len(d)) for rel, d in base_bytes.items()},
            control_id=rows[2]["id"], posts_of_three=three, posts_of_two=two,
            identity=b.Identity("u1", "Team", "Collection"))

    def build(self, out: Path | None = None):
        return b.build(self.bundle, self.site_a, self.fixture, self.contract, out or self.out, self.cfg)


class BuildTests(unittest.TestCase):
    def setUp(self):
        self._td = tempfile.TemporaryDirectory()
        self.td = Path(self._td.name)

    def tearDown(self):
        self._td.cleanup()

    def test_counts_layout_and_base_rows(self):
        f = Fixture(self.td)
        rep = f.build()
        m = json.loads((f.out / "MANIFEST.json").read_text())
        posts = json.loads((f.out / "posts.json").read_text())
        self.assertEqual(rep["new_assets"], 8)
        self.assertEqual(len(m), 11)
        self.assertEqual(m[:3], f.rows, "base rows must be carried through unchanged and first")
        new_ids = [r["id"] for r in m[3:]]
        referenced = [a for p in posts for a in p["asset_ids"]]
        self.assertEqual(sorted(referenced), sorted(new_ids), "every new asset in exactly one post")
        self.assertTrue(all(p["sensitivity_tier"] == "public" and p["workflow_state"] == "approved" for p in posts))
        self.assertEqual(len({p["created_at"] for p in posts}), len(posts), "post timestamps unique")
        self.assertEqual({len(p["asset_ids"]) for p in posts}, {3, 2} if f.cfg.posts_of_two else {3})
        for r in m[3:]:
            self.assertTrue(r["metadata"]["source_bundle"]["member"].startswith(r["metadata"]["pack_notice"]["member"].rsplit("/", 1)[0]))
            self.assertEqual(r["metadata"]["source_bundle"]["sha256"], b.sha_file(f.out / r["file_path"]))

    def test_rebuild_is_byte_identical(self):
        f = Fixture(self.td)
        f.build(self.td / "o1")
        f.build(self.td / "o2")
        self.assertEqual(b.compare_trees(self.td / "o1", self.td / "o2"), [])

    def test_sums_cover_every_file(self):
        f = Fixture(self.td)
        f.build()
        sums = (f.out / "SHA256SUMS").read_text().splitlines()
        listed = [l[66:] for l in sums]
        self.assertEqual(listed, sorted(listed))
        present = sorted(p.relative_to(f.out).as_posix() for p in f.out.rglob("*") if p.is_file() and p.name != "SHA256SUMS")
        self.assertEqual(listed, present)
        for l in sums:
            self.assertEqual(b.sha_file(f.out / l[66:]), l[:64])

    def test_excludes_site_a_and_internal_duplicates_and_small(self):
        f = Fixture(self.td)
        alpha = f.bundle / f.packs[0].folder / "PNG"
        (f.site_a / "dup_of_alpha0.png").write_bytes((alpha / "tile_00.png").read_bytes())
        (alpha / "tile_015.png").write_bytes((alpha / "tile_01.png").read_bytes())  # internal duplicate, sorts right after tile_01
        rep = f.build()
        self.assertEqual(rep["skipped"]["matches_site_a"], 1)
        self.assertEqual(rep["skipped"]["duplicate_within_candidate"], 1)
        m = json.loads((f.out / "MANIFEST.json").read_text())
        hashes = [r["metadata"]["source_bundle"]["sha256"] for r in m[3:]]
        self.assertEqual(len(hashes), len(set(hashes)))
        self.assertNotIn(hashlib.sha256((alpha / "tile_00.png").read_bytes()).hexdigest(), hashes)
        self.assertFalse(any("tiny" in r["file_path"] for r in m))

    def test_short_quota_is_filled_from_fill_order_and_reported(self):
        f = Fixture(self.td, per_pack=6, quotas=(3, 3, 2))
        beta = f.bundle / f.packs[1].folder / "PNG"
        for k in range(1, 6):
            os.remove(beta / f"tile_{k:02d}.png")  # beta keeps 1 eligible file
        rep = f.build()
        self.assertEqual(rep["quota_deviations"], [{"pack": f.packs[1].folder, "quota": 3, "taken": 1, "short": 2}])
        self.assertEqual(rep["new_assets"], 8)
        self.assertEqual(rep["per_pack"][f.packs[0].name], 5)  # alpha is first in fill order

    def test_control_in_site_a_blocks(self):
        f = Fixture(self.td)
        (f.site_a / "control_copy.png").write_bytes((f.fixture / "images/x/control.png").read_bytes())
        with self.assertRaisesRegex(b.BuildError, "BLOCKED"):
            f.build()

    def test_anchor_missing_from_site_a_fails(self):
        f = Fixture(self.td)
        os.remove(f.site_a / "a2.png")
        with self.assertRaisesRegex(b.BuildError, "anchor overlap-image-2"):
            f.build()

    def test_changed_base_row_or_file_fails(self):
        f = Fixture(self.td)
        rows = json.loads((f.fixture / "MANIFEST.json").read_text())
        rows[0]["metadata"]["candidate"] = "edited"
        (f.fixture / "MANIFEST.json").write_text(json.dumps(rows))
        with self.assertRaisesRegex(b.BuildError, "changed"):
            f.build()
        f2 = Fixture(Path(tempfile.mkdtemp(dir=self.td)))
        (f2.fixture / "images/x/anchor1.png").write_bytes(png(70, 70, 1))
        with self.assertRaisesRegex(b.BuildError, "changed"):
            f2.build()

    def test_symlinks_are_not_candidates(self):
        f = Fixture(self.td)
        alpha = f.bundle / f.packs[0].folder / "PNG"
        outside = self.td / "outside.png"
        outside.write_bytes(png(90, 90, 77))
        os.symlink(outside, alpha / "aaa_link.png")
        rep = f.build()
        self.assertEqual(rep["packs"][f.packs[0].name]["symlinks"], 1)
        m = json.loads((f.out / "MANIFEST.json").read_text())
        self.assertFalse(any("aaa-link" in r["file_path"] or "aaa_link" in r["file_path"] for r in m))

    def test_case_colliding_output_paths_are_skipped(self):
        f = Fixture(self.td)
        alpha = f.bundle / f.packs[0].folder / "PNG"
        (alpha / "Tile_00.png").write_bytes(png(95, 95, 55))  # same output name as tile_00.png
        rep = f.build()
        self.assertEqual(rep["skipped"]["path_collision"], 1)
        m = json.loads((f.out / "MANIFEST.json").read_text())
        paths = [r["file_path"].lower() for r in m]
        self.assertEqual(len(paths), len(set(paths)))

    def test_bad_notice_pin_fails(self):
        f = Fixture(self.td)
        (f.bundle / f.packs[2].folder / "License.txt").write_bytes(b"changed")
        with self.assertRaisesRegex(b.BuildError, "does not match its pin"):
            f.build()

    def test_no_absolute_paths_in_outputs(self):
        f = Fixture(self.td)
        f.build()
        for name in ("MANIFEST.json", "posts.json", "README.md", "SHA256SUMS"):
            text = (f.out / name).read_text()
            for leak in (str(self.td), str(f.bundle), str(f.site_a)):
                self.assertNotIn(leak, text, f"{name} leaks {leak}")

    def test_fixture_argument_is_required(self):
        # Dataset bytes live outside the application repository, so there is no default.
        self.assertFalse(hasattr(b, "DEFAULT_FIXTURE"))
        with self.assertRaises(SystemExit) as cm:
            b.main(["--bundle", str(self.td), "--site-a", str(self.td)])
        self.assertEqual(cm.exception.code, 2)

    def test_readme_has_no_app_relative_links(self):
        f = Fixture(self.td)
        f.build()
        text = (f.out / "README.md").read_text()
        self.assertNotIn("](../", text)
        self.assertIn("--fixture <this directory>", text)


if __name__ == "__main__":
    unittest.main()
