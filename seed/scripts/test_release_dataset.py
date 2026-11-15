#!/usr/bin/env python3
"""
Synthetic tests for release_dataset.py (#1514).

Stdlib unittest only. Every corpus is generated in a temporary directory; no real dataset is read.

Run: python3 -B seed/scripts/test_release_dataset.py -v
"""

from __future__ import annotations

import gzip
import hashlib
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import release_dataset as rd  # noqa: E402


def blob(seed: str, n: int) -> bytes:
    """n deterministic pseudo-random bytes (incompressible, so parts split predictably)."""
    out, i = bytearray(), 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
        i += 1
    return bytes(out[:n])


def write_root(root: Path, files: dict[str, bytes], extra: dict[str, bytes] | None = None,
               sums_text: str | None = None) -> Path:
    """Create a dataset root with SHA256SUMS listing `files`; `extra` files are written but not listed."""
    root.mkdir(parents=True)
    for rel, data in {**files, **(extra or {})}.items():
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(data)
    if sums_text is None:
        sums_text = "".join(f"{hashlib.sha256(files[p]).hexdigest()}  {p}\n" for p in sorted(files))
    (root / "SHA256SUMS").write_text(sums_text, encoding="utf-8")
    return root


# site_b uses the same root layout as site_a (#1514): its generated metadata document is
# DATASET.md, and the curated root README.md is not listed (see the test below).
SITE_B_META = {"MANIFEST.json": b'[{"id": "x"}]\n', "posts.json": b"[]\n", "DATASET.md": b"# generated\n"}
SITE_A_META = {n: f"{n} metadata\n".encode() for n in ("ATTRIBUTIONS.md", "MANIFEST.json", "dataset-metadata.json",
                                                        "groups.csv", "kenney-hq-replacements.json",
                                                        "metadata.csv", "posts.json")}


def site_b_files(n: int, size: int = 3000) -> dict[str, bytes]:
    files = dict(SITE_B_META)
    for i in range(n):
        files[f"images/kenney/pack-{i % 3}/img_{i:03d}.png"] = blob(f"b{i}", size + i)
    return files


def site_a_files() -> dict[str, bytes]:
    files = dict(SITE_A_META)
    long_dir = "3d/" + "/".join(["very-long-directory-name-for-pax"] * 4)
    files.update({
        f"{long_dir}/model.glb": blob("glb", 5000),                 # path > 100 chars (pax header)
        "3d/kenney_kit/Models/OBJ format/wall.mtl": b"newmtl a\n",
        "audio/kenney_rpg-audio/Audio/beltHandle2.ogg": blob("ogg", 2000),
        "documents/internet/walden.epub": blob("epub", 4000),
        "fonts/Café Sans/static/Café-Bold.ttf": blob("ttf", 1500),   # non-ASCII path
        "images/internet/empty.png": b"",                            # zero-byte payload
        "images/kenney-hq/a-512.png": blob("png", 2500),
        "videos/internet/clip.webm": blob("webm", 9000),
    })
    return files


class Base(unittest.TestCase):
    def setUp(self):
        self._td = tempfile.TemporaryDirectory()
        self.t = Path(self._td.name)

    def tearDown(self):
        self._td.cleanup()

    def assertNoTemp(self, parent: Path):
        left = [p.name for p in parent.iterdir() if ".tmp-" in p.name]
        self.assertEqual(left, [], f"temporary directories left behind in {parent}")

    def assertFails(self, fn, *args, contains: str | None = None, **kw):
        with self.assertRaises(rd.ReleaseError) as cm:
            fn(*args, **kw)
        if contains:
            self.assertIn(contains, str(cm.exception))
        return cm.exception

    def build_ok(self, root, site, name="rel", **kw):
        out = self.t / name
        m = rd.build(str(root), site, str(out), **kw)
        return out, m

    def verify_ok(self, root, rel, name="site", **kw):
        out = self.t / name
        r = rd.verify(str(root), str(rel), str(out), **kw)
        return out, r

    def tree(self, root: Path) -> dict[str, bytes]:
        return {str(p.relative_to(root)): p.read_bytes() for p in sorted(root.rglob("*")) if p.is_file()}


class RoundTrip(Base):
    def test_site_b_one_payload_round_trip(self):
        files = site_b_files(1)
        root = write_root(self.t / "meta_src", files)
        rel, m = self.build_ok(root, "site_b")
        self.assertEqual(sorted(os.listdir(rel)), ["release.json", "site_b-payload.tar.gz.part0000"])
        self.assertEqual(m["payload"]["count"], 1)
        self.assertIn("not rights clearance", m["notice"])
        # a metadata-only clone: same root without the images
        meta = write_root(self.t / "clone", SITE_B_META,
                          sums_text=(root / "SHA256SUMS").read_text(encoding="utf-8"))
        out, r = self.verify_ok(meta, rel)
        self.assertEqual(r["payload_count"], 1)
        got = self.tree(out)
        self.assertEqual(got.pop("SHA256SUMS"), (root / "SHA256SUMS").read_bytes())
        self.assertEqual(got, files)
        self.assertNoTemp(self.t)

    def test_site_b_repo_root_layout_with_curated_readme(self):
        """#1514: site_b's site root is its repository root, like site_a. The curated README.md
        and .gitignore sit there UNLISTED; they are not payload, not release metadata and do not
        block a build. The generated DATASET.md is listed metadata."""
        files = site_b_files(2)
        root = write_root(self.t / "repo", files,
                          extra={"README.md": b"# curated\n", ".gitignore": b"/images/\n"})
        rel, m = self.build_ok(root, "site_b")
        self.assertEqual([e["path"] for e in m["metadata"]], ["DATASET.md", "MANIFEST.json", "posts.json"])
        meta = write_root(self.t / "clone", SITE_B_META,
                          extra={"README.md": b"# curated\n", ".gitignore": b"/images/\n"},
                          sums_text=(root / "SHA256SUMS").read_text(encoding="utf-8"))
        out, r = self.verify_ok(meta, rel)
        self.assertEqual(r["payload_count"], 2)
        got = self.tree(out)
        self.assertNotIn("README.md", got)
        self.assertNotIn(".gitignore", got)

    def test_zero_payload_files(self):
        root = write_root(self.t / "src", dict(SITE_B_META))
        rel, m = self.build_ok(root, "site_b")
        self.assertEqual((m["payload"]["count"], m["payload"]["bytes"], len(m["parts"])), (0, 0, 1))
        out, r = self.verify_ok(root, rel)
        self.assertEqual(r["payload_count"], 0)
        self.assertEqual(sorted(self.tree(out)), sorted(list(SITE_B_META) + ["SHA256SUMS"]))

    def test_many_files_forced_split_and_part_limit(self):
        files = site_b_files(60)
        root = write_root(self.t / "src", files)
        rel, m = self.build_ok(root, "site_b", max_part_bytes=16384)
        self.assertGreater(len(m["parts"]), 5)
        names = [p["name"] for p in m["parts"]]
        self.assertEqual(names, [rd.part_name("site_b", i) for i in range(len(names))])
        stream = b""
        for p in m["parts"]:
            data = (rel / p["name"]).read_bytes()
            self.assertLess(len(data), 16384)
            self.assertGreater(len(data), 0)
            self.assertEqual(hashlib.sha256(data).hexdigest(), p["sha256"])
            stream += data
        self.assertEqual(len(stream), m["stream"]["bytes"])
        self.assertEqual(hashlib.sha256(stream).hexdigest(), m["stream"]["sha256"])
        # the concatenation is one ordinary tar.gz with exactly the payload, sorted, no metadata
        with tarfile.open(fileobj=io.BytesIO(stream), mode="r:gz") as tf:
            members = tf.getmembers()
        payload = sorted((p for p in files if p.startswith("images/")), key=lambda s: s.encode())
        self.assertEqual([x.name for x in members], payload)
        self.assertTrue(all(x.mtime == 0 and x.uid == 0 and x.mode == 0o644 and x.isreg() for x in members))
        out, _ = self.verify_ok(root, rel)
        self.assertEqual({k: v for k, v in self.tree(out).items() if k != "SHA256SUMS"}, files)

    def test_site_a_layout_long_and_unicode_paths(self):
        files = site_a_files()
        extra = {"README.md": b"repo readme\n", ".gitignore": b"/3d/\n", ".git/config": b"[core]\n",
                 "MANIFEST.json.pre-1.bak": b"old\n"}
        root = write_root(self.t / "src", files, extra)
        rel, m = self.build_ok(root, "site_a", max_part_bytes=8192)
        self.assertEqual(m["payload"]["count"], 8)
        self.assertEqual(len(m["metadata"]), 7)
        self.assertEqual(set(m["payload"]["by_prefix"]), set(rd.SITE_PREFIXES["site_a"]))
        out, r = self.verify_ok(root, rel)
        self.assertEqual(r["metadata_files"], 7)
        got = self.tree(out)
        got.pop("SHA256SUMS")
        self.assertEqual(got, files)          # unlisted README/.gitignore/.git/.bak are NOT carried

    def test_deterministic(self):
        root = write_root(self.t / "src", site_b_files(25))
        r1, _ = self.build_ok(root, "site_b", name="r1", max_part_bytes=20000)
        r2, _ = self.build_ok(root, "site_b", name="r2", max_part_bytes=20000)
        self.assertEqual(self.tree(r1), self.tree(r2))

    def test_expect_manifest_sha256(self):
        root = write_root(self.t / "src", site_b_files(3))
        rel, _ = self.build_ok(root, "site_b")
        good = hashlib.sha256((rel / "release.json").read_bytes()).hexdigest()
        self.verify_ok(root, rel, expect_manifest_sha256=good)
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "x"), "0" * 64, contains="expected")
        self.assertFalse((self.t / "x").exists())


class BuildRejects(Base):
    def _expect_build_fail(self, root, contains, site="site_b", **kw):
        self.assertFails(rd.build, str(root), site, str(self.t / "out"), contains=contains, **kw)
        self.assertFalse((self.t / "out").exists())
        self.assertNoTemp(self.t)

    def test_hash_mismatch(self):
        files = site_b_files(2)
        root = write_root(self.t / "src", files)
        (root / "images/kenney/pack-0/img_000.png").write_bytes(b"tampered")
        self._expect_build_fail(root, "SHA-256")

    def test_unlisted_payload(self):
        root = write_root(self.t / "src", site_b_files(2), extra={"images/stray.png": b"x"})
        self._expect_build_fail(root, "unlisted")

    def test_missing_listed_file(self):
        files = site_b_files(2)
        root = write_root(self.t / "src", files)
        (root / "images/kenney/pack-1/img_001.png").unlink()
        self._expect_build_fail(root, "img_001.png")

    def test_symlinked_file_and_dir(self):
        files = site_b_files(1)
        root = write_root(self.t / "src", files)
        target = self.t / "outside.png"
        target.write_bytes(b"secret")
        os.symlink(target, root / "images/link.png")
        self._expect_build_fail(root, "symlink")
        (root / "images/link.png").unlink()
        os.symlink(self.t, root / "images/linkdir")
        self._expect_build_fail(root, "symlink")

    def test_symlinked_prefix_dir(self):
        real = self.t / "real_images"
        real.mkdir()
        root = write_root(self.t / "src", dict(SITE_B_META))
        os.symlink(real, root / "images")
        self._expect_build_fail(root, "not a real directory")

    def test_unsafe_and_malformed_sums(self):
        h = "a" * 64
        cases = {
            f"{h}  ../escape.png\n": "unsafe path",
            f"{h}  /abs/x.png\n": "unsafe path",
            f"{h}  images/./x.png\n": "unsafe path",
            f"{h}  images//x.png\n": "unsafe path",
            f"{h}  images/x.png\n{h}  images/x.png\n": "duplicate",
            f"{'A' * 64}  images/x.png\n": "expected",
            f"{h} images/x.png\n": "expected",
            f"{h}  images/x.png\r\n": "carriage return",
            f"{h}  SHA256SUMS\n": "lists itself",
            f"{h}  docs/notes.txt\n": "neither under",
            f"{h}  images/x.png": "newline",
        }
        for i, (text, needle) in enumerate(cases.items()):
            root = write_root(self.t / f"src{i}", {}, sums_text=text)
            with self.subTest(text=text):
                self._expect_build_fail(root, needle)

    def test_output_inside_source_and_preexisting_output(self):
        root = write_root(self.t / "src", site_b_files(1))
        self.assertFails(rd.build, str(root), "site_b", str(root / "rel"), contains="overlaps")
        self.assertFalse((root / "rel").exists())
        empty = self.t / "existing_empty"
        empty.mkdir()
        self.assertFails(rd.build, str(root), "site_b", str(empty), contains="already exists")
        self.assertEqual(list(empty.iterdir()), [])
        full = self.t / "existing_full"
        full.mkdir()
        (full / "keep.txt").write_bytes(b"keep")
        self.assertFails(rd.build, str(root), "site_b", str(full), contains="already exists")
        self.assertEqual((full / "keep.txt").read_bytes(), b"keep")
        self.assertNoTemp(self.t)

    def test_install_race_never_replaces(self):
        root = write_root(self.t / "src", site_b_files(1))
        out = self.t / "race"
        real = rd.install_noreplace

        def racing(tmp, dst):
            os.mkdir(dst)                    # someone creates the output after the pre-check
            (Path(dst) / "theirs.txt").write_bytes(b"theirs")
            return real(tmp, dst)
        rd.install_noreplace = racing
        try:
            self.assertFails(rd.build, str(root), "site_b", str(out), contains="install")
        finally:
            rd.install_noreplace = real
        self.assertEqual(sorted(os.listdir(out)), ["theirs.txt"])
        self.assertNoTemp(self.t)

    def test_part_size_bounds(self):
        root = write_root(self.t / "src", site_b_files(1))
        self.assertLessEqual(rd.DEFAULT_MAX_PART_BYTES, 1_900_000_000)
        self.assertLess(rd.HARD_MAX_PART_BYTES - 1, 2 * 1024 ** 3)
        for bad in (1, 0, rd.HARD_MAX_PART_BYTES + 1):
            self._expect_build_fail(root, "max-part-bytes", max_part_bytes=bad)


class VerifyRejects(Base):
    def setUp(self):
        super().setUp()
        self.files = site_b_files(30)
        self.root = write_root(self.t / "src", self.files)
        self.rel, self.m = self.build_ok(self.root, "site_b", max_part_bytes=16384)
        self.assertGreater(len(self.m["parts"]), 3)

    def _expect_verify_fail(self, contains, root=None, rel=None):
        out = self.t / "site_out"
        self.assertFails(rd.verify, str(root or self.root), str(rel or self.rel), str(out), contains=contains)
        self.assertFalse(out.exists())
        self.assertNoTemp(self.t)

    def _manifest(self):
        return json.loads((self.rel / "release.json").read_text(encoding="utf-8"))

    def _write_manifest(self, m):
        (self.rel / "release.json").write_text(json.dumps(m), encoding="utf-8")

    def test_tampered_part(self):
        p = self.rel / self.m["parts"][1]["name"]
        data = bytearray(p.read_bytes())
        data[10] ^= 0xFF
        p.write_bytes(bytes(data))
        self._expect_verify_fail("does not match release.json")

    def test_tampered_manifest_hash_and_reorder(self):
        m = self._manifest()
        m["parts"][0]["sha256"] = "0" * 64
        self._write_manifest(m)
        self._expect_verify_fail("does not match release.json")
        m = self._manifest()
        m["parts"][0], m["parts"][1] = m["parts"][1], m["parts"][0]
        self._write_manifest(m)
        self._expect_verify_fail("out of order")

    def test_swapped_part_contents(self):
        a, b = (self.rel / self.m["parts"][i]["name"] for i in (0, 1))
        da, db = a.read_bytes(), b.read_bytes()
        a.write_bytes(db)
        b.write_bytes(da)
        self._expect_verify_fail("does not match release.json")

    def test_missing_and_extra_parts(self):
        last = self.rel / self.m["parts"][-1]["name"]
        data = last.read_bytes()
        last.unlink()
        self._expect_verify_fail("missing")
        last.write_bytes(data)
        (self.rel / rd.part_name("site_b", len(self.m["parts"]))).write_bytes(b"extra")
        self._expect_verify_fail("extra")

    def test_symlinked_part(self):
        p = self.rel / self.m["parts"][0]["name"]
        moved = self.t / "moved_part"
        p.rename(moved)
        os.symlink(moved, p)
        self._expect_verify_fail("without following links")

    def test_tampered_metadata_root(self):
        (self.root / "DATASET.md").write_bytes(b"changed\n")
        self._expect_verify_fail("does not match SHA256SUMS")
        (self.root / "DATASET.md").write_bytes(SITE_B_META["DATASET.md"])
        sums = (self.root / "SHA256SUMS").read_text(encoding="utf-8")
        (self.root / "SHA256SUMS").write_text(sums + f"{'b' * 64}  images/new.png\n", encoding="utf-8")
        self._expect_verify_fail("release pin")

    def test_preexisting_output_untouched(self):
        out = self.t / "already"
        out.mkdir()
        (out / "keep").write_bytes(b"keep")
        self.assertFails(rd.verify, str(self.root), str(self.rel), str(out), contains="already exists")
        self.assertEqual(os.listdir(out), ["keep"])
        self.assertFails(rd.verify, str(self.root), str(self.rel), str(self.rel / "x"), contains="overlaps")


class MaliciousStream(Base):
    """An attacker-built release whose parts match its own release.json; SHA256SUMS still rules."""

    def _forge(self, root: Path, members: list[tuple[tarfile.TarInfo, bytes | None]], name="forged") -> Path:
        good, _ = self.build_ok(root, "site_b", name=name + "_base")
        m = json.loads((good / "release.json").read_text(encoding="utf-8"))
        raw = io.BytesIO()
        with gzip.GzipFile(fileobj=raw, mode="wb", mtime=0, filename="") as gz:
            with tarfile.open(fileobj=gz, mode="w|", format=tarfile.PAX_FORMAT) as tf:
                for ti, data in members:
                    tf.addfile(ti, io.BytesIO(data) if data is not None else None)
        stream = raw.getvalue()
        rel = self.t / name
        rel.mkdir()
        pn = rd.part_name("site_b", 0)
        (rel / pn).write_bytes(stream)
        h = hashlib.sha256(stream).hexdigest()
        m["parts"] = [{"name": pn, "bytes": len(stream), "sha256": h}]
        m["stream"] = {"bytes": len(stream), "sha256": h}
        (rel / "release.json").write_text(json.dumps(m), encoding="utf-8")
        return rel

    def _ti(self, name, size, typ=tarfile.REGTYPE, link=""):
        ti = tarfile.TarInfo(name)
        ti.size, ti.type, ti.linkname = size, typ, link
        return ti

    def setUp(self):
        super().setUp()
        self.files = site_b_files(2)
        self.root = write_root(self.t / "src", self.files)
        self.payload = sorted(p for p in self.files if p.startswith("images/"))

    def _expect(self, rel, contains):
        out = self.t / "site_out"
        self.assertFails(rd.verify, str(self.root), str(rel), str(out), contains=contains)
        self.assertFalse(out.exists())
        self.assertFalse((self.t / "evil").exists())
        self.assertNoTemp(self.t)

    def test_traversal_member(self):
        rel = self._forge(self.root, [(self._ti("../evil", 4), b"evil")])
        self._expect(rel, "unsafe path")

    def test_absolute_member(self):
        rel = self._forge(self.root, [(self._ti(str(self.t / "evil"), 4), b"evil")])
        self._expect(rel, "unsafe path")

    def test_symlink_member(self):
        rel = self._forge(self.root, [(self._ti(self.payload[0], 0, tarfile.SYMTYPE, "/etc/passwd"), None)])
        self._expect(rel, "not a regular file")

    def test_unlisted_and_duplicate_members(self):
        rel = self._forge(self.root, [(self._ti("images/other.png", 1), b"x")], name="f1")
        self._expect(rel, "not a listed payload file")
        d = self.files[self.payload[0]]
        rel = self._forge(self.root, [(self._ti(self.payload[0], len(d)), d)] * 2, name="f2")
        self._expect(rel, "appears twice")

    def test_wrong_bytes_member_cleans_up_partial_extraction(self):
        d0, d1 = self.files[self.payload[0]], self.files[self.payload[1]]
        rel = self._forge(self.root, [(self._ti(self.payload[0], len(d0)), d0),
                                      (self._ti(self.payload[1], len(d1)), d1[::-1])])
        self._expect(rel, "does not match SHA256SUMS")

    def test_missing_member(self):
        d0 = self.files[self.payload[0]]
        rel = self._forge(self.root, [(self._ti(self.payload[0], len(d0)), d0)])
        self._expect(rel, "missing from the archive")

    def test_not_gzip(self):
        rel = self._forge(self.root, [])
        pn = rel / rd.part_name("site_b", 0)
        junk = b"this is not gzip data at all"
        pn.write_bytes(junk)
        m = json.loads((rel / "release.json").read_text(encoding="utf-8"))
        h = hashlib.sha256(junk).hexdigest()
        m["parts"][0].update(bytes=len(junk), sha256=h)
        m["stream"] = {"bytes": len(junk), "sha256": h}
        (rel / "release.json").write_text(json.dumps(m), encoding="utf-8")
        self._expect(rel, "not a valid tar.gz")


class Cli(Base):
    def test_cli_exit_codes(self):
        root = write_root(self.t / "src", site_b_files(2))
        script = str(HERE / "release_dataset.py")
        run = lambda *a: subprocess.run([sys.executable, "-I", "-B", script, *a],  # noqa: E731
                                        capture_output=True, text=True)
        r = run("inspect", "--root", str(root), "--site", "site_b")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout)["payload_count"], 2)
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel"), "--legacy-v1")
        self.assertEqual(r.returncode, 0, r.stderr)
        r = run("verify", "--root", str(root), "--release", str(self.t / "rel"), "--output", str(self.t / "out"))
        self.assertEqual(r.returncode, 0, r.stderr)
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel"), "--legacy-v1")
        self.assertEqual(r.returncode, 1)
        self.assertIn("FAILED", r.stderr)
        r = run("build", "--root", str(root))
        self.assertEqual(r.returncode, 2)
        # A new build must say which layout: no --tag and no --legacy-v1 is a usage error.
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel3"))
        self.assertEqual(r.returncode, 2)
        self.assertIn("--tag", r.stderr)
        # v2 through the CLI, and verify with the expected tag.
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel2"),
                "--tag", "v2026.12.01")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout)["manifest"], "site_b-v2026.12.01-release.json")
        r = run("verify", "--root", str(root), "--release", str(self.t / "rel2"), "--output", str(self.t / "out2"),
                "--tag", "v2026.12.01")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(json.loads(r.stdout)["edition"], "v2026.12.01")
        r = run("verify", "--root", str(root), "--release", str(self.t / "rel2"), "--output", str(self.t / "out3"),
                "--tag", "v2026.12.02")
        self.assertEqual(r.returncode, 1)
        self.assertIn("expected tag", r.stderr)
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel4"),
                "--tag", "latest")
        self.assertEqual(r.returncode, 1)
        self.assertFalse((self.t / "rel4").exists())
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel5"),
                "--tag", "v2026.13.01")
        self.assertEqual(r.returncode, 1)
        self.assertIn("real calendar edition", r.stderr)
        self.assertFalse((self.t / "rel5").exists())


class V2Format(Base):
    """The versioned layout (#1514 follow-up): site + edition in every asset name."""

    ED = "v2026.11.02"

    def v2(self, root, site="site_b", ed=ED, name=None, **kw):
        return self.build_ok(root, site, name=name or f"rel-{ed}", edition=ed, **kw)

    def test_round_trip_names_every_asset_with_site_and_edition(self):
        root = write_root(self.t / "src", site_b_files(6))
        rel, m = self.v2(root, max_part_bytes=6000)
        names = sorted(os.listdir(rel))
        self.assertGreater(len(m["parts"]), 1)
        self.assertEqual(names, sorted(["site_b-v2026.11.02-release.json"]
                                       + [f"site_b-v2026.11.02-payload.tar.gz.part{i:04d}" for i in range(len(m["parts"]))]))
        self.assertTrue(all(n.startswith("site_b-v2026.11.02-") for n in names))
        self.assertEqual((m["schema"], m["edition"], m["manifest"], m["tool_version"]),
                         (rd.SCHEMA_V2, self.ED, "site_b-v2026.11.02-release.json", 2))
        sha = hashlib.sha256((rel / m["manifest"]).read_bytes()).hexdigest()
        out, r = self.verify_ok(root, rel, expect_manifest_sha256=sha, edition=self.ED)
        self.assertEqual((r["edition"], r["manifest"], r["release_json_sha256"]), (self.ED, m["manifest"], sha))
        src = self.tree(root)
        self.assertEqual(self.tree(out), src)
        out2, r2 = self.verify_ok(root, rel, name="site2")          # without an expected tag: still checked
        self.assertEqual(r2["edition"], self.ED)
        self.assertEqual(self.tree(out2), src)

    def test_two_editions_of_one_site_never_collide(self):
        root = write_root(self.t / "src", site_b_files(6))
        a, ma = self.v2(root, ed="v2026.11.02", max_part_bytes=6000)
        b, mb = self.v2(root, ed="v2026.12.01-sm", max_part_bytes=6000)
        na, nb = set(os.listdir(a)), set(os.listdir(b))
        self.assertEqual(na & nb, set(), "two editions share an asset name")
        # side by side in ONE download folder, each still verifies on its own names only
        both = self.t / "downloads"
        both.mkdir()
        for d in (a, b):
            for f in os.listdir(d):
                (both / f).write_bytes((d / f).read_bytes())
        self.assertEqual(len(os.listdir(both)), len(na) + len(nb))
        self.assertFails(rd.verify, str(root), str(both), str(self.t / "mixed"), contains="exactly one manifest")
        # ...and the legacy names would have collided: v1 always uses release.json + <site>-payload...
        l1, _ = self.build_ok(root, "site_b", name="v1-a", max_part_bytes=6000)
        l2, _ = self.build_ok(root, "site_b", name="v1-b", max_part_bytes=6000)
        self.assertEqual(set(os.listdir(l1)), set(os.listdir(l2)))

    def test_legacy_v1_build_is_unchanged(self):
        root = write_root(self.t / "src", site_b_files(4))
        rel, m = self.build_ok(root, "site_b", max_part_bytes=6000)
        self.assertEqual(m["schema"], rd.SCHEMA)
        self.assertEqual(m["tool_version"], 1)
        self.assertNotIn("edition", m)
        self.assertNotIn("manifest", m)
        self.assertEqual(sorted(os.listdir(rel)),
                         sorted(["release.json"] + [rd.part_name("site_b", i) for i in range(len(m["parts"]))]))
        _, r = self.verify_ok(root, rel)
        self.assertIsNone(r["edition"])
        self.assertEqual(r["manifest"], "release.json")
        # v1 cannot be verified as an edition: the expected tag has nothing to match
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "x"), edition=self.ED, contains="unversioned v1")

    def test_malformed_tags_are_refused_before_writing(self):
        root = write_root(self.t / "src", site_b_files(2))
        for bad in ("latest", "v2026.11.2", "2026.11.02", "V2026.11.02", "v2026.11.02-", "v2026.11.02-SM",
                    "v2026.11.02-sm.1", "v2026.11.02/../x", "v2026.11.02-sm/x", "v2026.11.02 ", "",
                    "v2026.11.02-" + "a" * 17):
            with self.subTest(tag=bad):
                out = self.t / f"bad-{abs(hash(bad))}"
                self.assertFails(rd.build, str(root), "site_b", str(out), edition=bad, contains="edition tag")
                self.assertFalse(out.exists())
        self.assertNoTemp(self.t)

    # vYYYY.MM.NN is year, month and edition number within the month (ADR 0100), not a day of the month.
    IMPOSSIBLE_EDITIONS = ("v2026.13.01", "v2026.00.01", "v2026.99.01", "v2026.11.00", "v0000.11.01",
                           "v2026.13.01-sm", "v2026.00.01-sm", "v2026.11.00-sm")
    REAL_EDITIONS = ("v2026.11.01", "v2026.12.99", "v2026.02.30", "v2026.02.29", "v2026.04.31-sm",
                     "v0001.01.01", "v9999.12.99-sm")

    def test_impossible_editions_are_refused_before_writing(self):
        root = write_root(self.t / "src", site_b_files(2))
        for bad in self.IMPOSSIBLE_EDITIONS:
            with self.subTest(tag=bad):
                out = self.t / f"bad-{bad}"
                self.assertFails(rd.build, str(root), "site_b", str(out), edition=bad,
                                 contains="real calendar edition")
                self.assertFalse(out.exists())
                with self.assertRaises(rd.ReleaseError):
                    rd.manifest_name("site_b", bad)
        rel, m = self.v2(root)                                     # verify --tag refuses one too, installs nothing
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "out"), edition="v2026.13.01",
                         contains="real calendar edition")
        self.assertFalse((self.t / "out").exists())
        self.assertNoTemp(self.t)

    def test_real_editions_are_accepted_and_nn_is_not_a_day(self):
        for good in self.REAL_EDITIONS:
            with self.subTest(tag=good):
                self.assertEqual(rd.check_edition(good), good)
        root = write_root(self.t / "src", site_b_files(2))
        rel, m = self.v2(root, ed="v2026.02.30")                   # the 30th February edition
        self.assertEqual(m["manifest"], "site_b-v2026.02.30-release.json")
        self.verify_ok(root, rel, edition="v2026.02.30")

    def test_tag_mismatch_is_refused_and_installs_nothing(self):
        root = write_root(self.t / "src", site_b_files(3))
        rel, m = self.v2(root)
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "out"), edition="v2026.11.03",
                         contains="expected tag")
        self.assertFalse((self.t / "out").exists())
        self.assertNoTemp(self.t)

    def test_renamed_manifest_is_refused(self):
        root = write_root(self.t / "src", site_b_files(3))
        rel, m = self.v2(root)
        os.rename(rel / m["manifest"], rel / "site_b-v2026.11.03-release.json")    # claims another edition
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o1"), contains="does not match its recorded")
        os.rename(rel / "site_b-v2026.11.03-release.json", rel / "site_a-v2026.11.02-release.json")  # another site
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o2"), contains="does not match its recorded")
        os.rename(rel / "site_a-v2026.11.02-release.json", rel / "release.json")   # passed off as v1
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o3"), contains="does not match its recorded")
        self.assertFalse(any((self.t / o).exists() for o in ("o1", "o2", "o3")))

    def test_edited_edition_field_is_refused_with_or_without_the_pin(self):
        root = write_root(self.t / "src", site_b_files(3))
        rel, m = self.v2(root)
        mp = rel / m["manifest"]
        sha = hashlib.sha256(mp.read_bytes()).hexdigest()
        doc = json.loads(mp.read_text())
        doc["edition"] = "v2026.11.03"
        mp.write_text(json.dumps(doc), encoding="utf-8")
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o1"), expect_manifest_sha256=sha,
                         contains="SHA-256")                                     # the pin catches it first
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o2"), contains="does not match its recorded")

    def test_renamed_or_extra_part_is_refused(self):
        root = write_root(self.t / "src", site_b_files(6))
        rel, m = self.v2(root, max_part_bytes=6000)
        first = rel / m["parts"][0]["name"]
        os.rename(first, rel / rd.part_name("site_b", 0))                         # back to a v1 part name
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o1"), contains="release directory mismatch")
        os.rename(rel / rd.part_name("site_b", 0), first)
        (rel / rd.part_name("site_b", len(m["parts"]), self.ED)).write_bytes(b"extra")
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o2"), contains="release directory mismatch")
        self.assertFalse((self.t / "o1").exists() or (self.t / "o2").exists())

    def test_part_list_naming_another_edition_is_refused(self):
        root = write_root(self.t / "src", site_b_files(3))
        rel, m = self.v2(root)
        mp = rel / m["manifest"]
        doc = json.loads(mp.read_text())
        old = doc["parts"][0]["name"]
        doc["parts"][0]["name"] = rd.part_name("site_b", 0, "v2026.11.03")
        mp.write_text(json.dumps(doc), encoding="utf-8")
        os.rename(rel / old, rel / doc["parts"][0]["name"])
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o"), contains="renamed or out of order")

    def test_tampered_part_bytes_are_refused_before_extraction(self):
        root = write_root(self.t / "src", site_b_files(3))
        rel, m = self.v2(root)
        pn = rel / m["parts"][0]["name"]
        b = bytearray(pn.read_bytes())
        b[len(b) // 2] ^= 0xFF
        pn.write_bytes(bytes(b))
        self.assertFails(rd.verify, str(root), str(rel), str(self.t / "o"), contains="does not match release.json")
        self.assertFalse((self.t / "o").exists())
        self.assertNoTemp(self.t)


if __name__ == "__main__":
    unittest.main()
