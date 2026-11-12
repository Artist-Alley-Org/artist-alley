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


SITE_B_META = {"MANIFEST.json": b'[{"id": "x"}]\n', "posts.json": b"[]\n", "README.md": b"# generated\n"}
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
        (self.root / "README.md").write_bytes(b"changed\n")
        self._expect_verify_fail("does not match SHA256SUMS")
        (self.root / "README.md").write_bytes(SITE_B_META["README.md"])
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
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel"))
        self.assertEqual(r.returncode, 0, r.stderr)
        r = run("verify", "--root", str(root), "--release", str(self.t / "rel"), "--output", str(self.t / "out"))
        self.assertEqual(r.returncode, 0, r.stderr)
        r = run("build", "--root", str(root), "--site", "site_b", "--output", str(self.t / "rel"))
        self.assertEqual(r.returncode, 1)
        self.assertIn("FAILED", r.stderr)
        r = run("build", "--root", str(root))
        self.assertEqual(r.returncode, 2)


if __name__ == "__main__":
    unittest.main()
