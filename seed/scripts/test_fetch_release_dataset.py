#!/usr/bin/env python3
"""
Synthetic tests for fetch_release_dataset.py (#1514).

Stdlib unittest only. Tiny synthetic corpora are packaged with the reviewed offline tool and served
from a local 127.0.0.1 HTTP server. A socket guard refuses every non-loopback connection, so no test
can reach GitHub or any other network host.

Run: python3 -B seed/scripts/test_fetch_release_dataset.py -v
"""

from __future__ import annotations

import hashlib
import http.server
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import fetch_release_dataset as fe  # noqa: E402

rd = fe.load_release_dataset()
REPO, TAG = "Artist-Alley-Org/test-dataset", "v0.0.1-test"
PREFIX = f"/{REPO}/releases/download/{TAG}/"

_real_create_connection = socket.create_connection


def _loopback_only(address, *a, **kw):
    host = address[0]
    if host not in ("127.0.0.1", "localhost", "::1"):
        raise AssertionError(f"test tried to reach a non-loopback host: {host!r}")
    return _real_create_connection(address, *a, **kw)


def setUpModule():
    socket.create_connection = _loopback_only


def tearDownModule():
    socket.create_connection = _real_create_connection


def blob(seed: str, n: int) -> bytes:
    out, i = bytearray(), 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
        i += 1
    return bytes(out[:n])


def write_root(root: Path, files: dict[str, bytes]) -> Path:
    root.mkdir(parents=True)
    for rel, data in files.items():
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(data)
    (root / "SHA256SUMS").write_text(
        "".join(f"{hashlib.sha256(files[p]).hexdigest()}  {p}\n" for p in sorted(files)), encoding="utf-8")
    return root


def metadata_only_copy(src: Path, dst: Path, site: str) -> Path:
    dst.mkdir()
    for line in (src / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
        _, p = line.split("  ", 1)
        if not p.startswith(rd.SITE_PREFIXES[site]):
            (dst / p).write_bytes((src / p).read_bytes())
    (dst / "SHA256SUMS").write_bytes((src / "SHA256SUMS").read_bytes())
    return dst


class Server:
    """Serves routes[path] -> dict(kind=..., ...); records every requested path."""

    def __init__(self):
        self.routes: dict[str, dict] = {}
        self.log: list[str] = []
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"

            def log_message(self, *a):
                pass

            def do_GET(self):
                outer.log.append(self.path)
                r = outer.routes.get(self.path)
                if r is None:
                    self.send_error(404)
                    return
                kind, body = r.get("kind", "ok"), r.get("body", b"")
                if kind == "redirect":
                    self.send_response(302)
                    self.send_header("Location", r["to"])
                    self.end_headers()
                    return
                if kind == "status":
                    self.send_error(r["code"])
                    return
                self.send_response(200)
                if kind == "ok":
                    self.send_header("Content-Length", str(len(body)))
                elif kind == "truncated":
                    self.send_header("Content-Length", str(len(body)))
                    body = body[: max(0, len(body) - 7)]
                elif kind == "no_length":
                    pass
                self.end_headers()
                self.wfile.write(body)

        class Quiet(http.server.ThreadingHTTPServer):
            def handle_error(self, request, client_address):
                if not isinstance(sys.exc_info()[1], (ConnectionResetError, BrokenPipeError)):
                    super().handle_error(request, client_address)   # a client hanging up early is expected

        self.httpd = Quiet(("127.0.0.1", 0), H)
        self.base = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        self.t = threading.Thread(target=self.httpd.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True)
        self.t.start()

    def publish(self, release_dir: Path):
        for f in sorted(os.listdir(release_dir)):
            self.routes[PREFIX + f] = {"kind": "ok", "body": (release_dir / f).read_bytes()}

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class Base(unittest.TestCase):
    site = "site_b"

    def setUp(self):
        self._td = tempfile.TemporaryDirectory()
        self.t = Path(self._td.name)
        self.work = self.t / "work"
        self.work.mkdir()
        self.srv = Server()

    def tearDown(self):
        self.srv.close()
        self._td.cleanup()

    def make_release(self, files, site="site_b", max_part_bytes=rd.DEFAULT_MAX_PART_BYTES, name="src"):
        src = write_root(self.t / name, files)
        rel = self.t / f"{name}-release"
        rd.build(str(src), site, str(rel), max_part_bytes)
        meta = metadata_only_copy(src, self.t / f"{name}-meta", site)
        sha = hashlib.sha256((rel / "release.json").read_bytes()).hexdigest()
        return src, rel, meta, sha

    def run_fetch(self, meta, sha, out=None, site=None, repo=REPO, tag=TAG, **kw):
        out = out or self.t / "out"
        return fe.fetch(repo, tag, site or self.site, sha, str(meta), str(out), work_dir=str(self.work),
                        _base_url=self.srv.base, _test_origin_only=True, **kw)

    def assertClean(self):
        self.assertEqual(os.listdir(self.work), [], "download directory left behind")
        self.assertFalse((self.t / "out").exists(), "output was installed despite a failure")
        self.assertEqual([p for p in os.listdir(self.t) if ".tmp-" in p], [], "verifier temp left behind")

    def assertFetchFails(self, contains, meta, sha, **kw):
        with self.assertRaises(fe.FetchError) as cm:
            self.run_fetch(meta, sha, **kw)
        self.assertIn(contains, str(cm.exception))
        self.assertClean()
        return cm.exception


def site_b_files(n):
    f = {"MANIFEST.json": b"[]\n", "posts.json": b"[]\n", "README.md": b"# generated\n"}
    for i in range(n):
        f[f"images/kenney/p{i % 2}/img_{i:02d}.png"] = blob(f"i{i}", 2000 + 37 * i)
    return f


class Success(Base):
    def test_one_part_site_b(self):
        src, rel, meta, sha = self.make_release(site_b_files(3))
        self.srv.publish(rel)
        r = self.run_fetch(meta, sha)
        self.assertEqual((r["site"], r["parts"], r["payload_count"], r["release_json_sha256"]), ("site_b", 1, 3, sha))
        self.assertEqual(self.srv.log, [PREFIX + "release.json", PREFIX + rd.part_name("site_b", 0)])
        out = self.t / "out"
        for line in (src / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
            h, p = line.split("  ", 1)
            self.assertEqual(hashlib.sha256((out / p).read_bytes()).hexdigest(), h)
        self.assertEqual((out / "SHA256SUMS").read_bytes(), (src / "SHA256SUMS").read_bytes())
        self.assertEqual(os.listdir(self.work), [])

    def test_many_parts_site_a_in_order(self):
        files = {n: f"{n}\n".encode() for n in ("ATTRIBUTIONS.md", "MANIFEST.json", "posts.json")}
        for i, pre in enumerate(rd.SITE_PREFIXES["site_a"]):
            files[f"{pre}deep/dir/file_{i}.bin"] = blob(pre, 6000)
        _, rel, meta, sha = self.make_release(files, site="site_a", max_part_bytes=4096)
        m = json.loads((rel / "release.json").read_text(encoding="utf-8"))
        self.assertGreater(len(m["parts"]), 5)
        self.srv.publish(rel)
        r = self.run_fetch(meta, sha, site="site_a")
        self.assertEqual(r["parts"], len(m["parts"]))
        self.assertEqual(self.srv.log, [PREFIX + "release.json"] + [PREFIX + p["name"] for p in m["parts"]])
        self.assertEqual(os.listdir(self.work), [])

    def test_same_origin_redirect_is_followed(self):
        _, rel, meta, sha = self.make_release(site_b_files(2))
        self.srv.publish(rel)
        pn = PREFIX + rd.part_name("site_b", 0)
        self.srv.routes["/cdn/part0"] = self.srv.routes[pn]
        self.srv.routes[pn] = {"kind": "redirect", "to": self.srv.base + "/cdn/part0"}
        self.run_fetch(meta, sha)
        self.assertIn("/cdn/part0", self.srv.log)


class ManifestPin(Base):
    def test_wrong_pin_fetches_no_parts(self):
        _, rel, meta, sha = self.make_release(site_b_files(2))
        self.srv.publish(rel)
        self.assertFetchFails("does not match the pin", meta, "0" * 64)
        self.assertEqual(self.srv.log, [PREFIX + "release.json"])

    def test_modified_manifest_with_old_pin(self):
        _, rel, meta, sha = self.make_release(site_b_files(2))
        self.srv.publish(rel)
        body = self.srv.routes[PREFIX + "release.json"]["body"]
        self.srv.routes[PREFIX + "release.json"]["body"] = body.replace(b'"site_b"', b'"site_b" ', 1)
        self.assertFetchFails("does not match the pin", meta, sha)
        self.assertEqual(self.srv.log, [PREFIX + "release.json"])

    def test_oversized_manifest(self):
        _, rel, meta, sha = self.make_release(site_b_files(1))
        self.srv.publish(rel)
        self.srv.routes[PREFIX + "release.json"] = {"kind": "no_length", "body": b"x" * (fe.MAX_MANIFEST_BYTES + 10)}
        self.assertFetchFails("larger than expected", meta, sha)
        self.srv.routes[PREFIX + "release.json"] = {"kind": "ok", "body": b"x" * (fe.MAX_MANIFEST_BYTES + 10)}
        self.assertFetchFails("Content-Length", meta, sha)

    def test_wrong_site_fetches_no_parts(self):
        _, rel, meta, sha = self.make_release(site_b_files(2))
        self.srv.publish(rel)
        self.assertFetchFails("not 'site_a'", meta, sha, site="site_a")
        self.assertEqual(self.srv.log, [PREFIX + "release.json"])

    def test_http_error_on_manifest(self):
        _, rel, meta, sha = self.make_release(site_b_files(1))
        self.srv.routes[PREFIX + "release.json"] = {"kind": "status", "code": 500}
        self.assertFetchFails("HTTP 500", meta, sha)


class Parts(Base):
    def setUp(self):
        super().setUp()
        self.src, self.rel, self.meta, self.sha = self.make_release(site_b_files(12), max_part_bytes=8192)
        self.m = json.loads((self.rel / "release.json").read_text(encoding="utf-8"))
        self.assertGreater(len(self.m["parts"]), 2)
        self.srv.publish(self.rel)
        self.p1 = PREFIX + self.m["parts"][1]["name"]

    def test_missing_part(self):
        del self.srv.routes[self.p1]
        self.assertFetchFails("HTTP 404", self.meta, self.sha)

    def test_modified_part(self):
        b = bytearray(self.srv.routes[self.p1]["body"])
        b[5] ^= 0x55
        self.srv.routes[self.p1]["body"] = bytes(b)
        self.assertFetchFails("SHA-256 does not match", self.meta, self.sha)
        self.assertNotIn(PREFIX + self.m["parts"][2]["name"], self.srv.log)

    def test_oversized_part(self):
        self.srv.routes[self.p1]["body"] += b"extra"
        self.assertFetchFails("Content-Length", self.meta, self.sha)
        self.srv.routes[self.p1]["kind"] = "no_length"
        self.assertFetchFails("larger than expected", self.meta, self.sha)

    def test_truncated_part(self):
        self.srv.routes[self.p1]["kind"] = "truncated"
        self.assertFetchFails("truncated", self.meta, self.sha)
        body = self.srv.routes[self.p1]["body"]
        self.srv.routes[self.p1] = {"kind": "no_length", "body": body[:-3]}
        self.assertFetchFails("truncated", self.meta, self.sha)

    def test_http_error_on_part(self):
        self.srv.routes[self.p1] = {"kind": "status", "code": 503}
        self.assertFetchFails("HTTP 503", self.meta, self.sha)

    def test_redirect_to_other_origin_refused(self):
        other = http.server.ThreadingHTTPServer(("127.0.0.1", 0), http.server.BaseHTTPRequestHandler)
        try:
            self.srv.routes[self.p1] = {"kind": "redirect",
                                        "to": f"http://127.0.0.1:{other.server_address[1]}/x"}
            self.assertFetchFails("redirect", self.meta, self.sha)
        finally:
            other.server_close()

    def test_offline_verifier_refusal(self):
        (self.meta / "README.md").write_bytes(b"different metadata\n")
        e = self.assertFetchFails("offline verification failed", self.meta, self.sha)
        self.assertIn("does not match SHA256SUMS", str(e))
        self.assertEqual(len(self.srv.log), 1 + len(self.m["parts"]))   # every part was fetched first

    def test_existing_output_preserved_no_requests(self):
        out = self.t / "existing"
        out.mkdir()
        (out / "keep").write_bytes(b"keep")
        with self.assertRaises(fe.FetchError) as cm:
            self.run_fetch(self.meta, self.sha, out=out)
        self.assertIn("already exists", str(cm.exception))
        self.assertEqual(os.listdir(out), ["keep"])
        self.assertEqual(self.srv.log, [])
        self.assertEqual(os.listdir(self.work), [])


class Inputs(Base):
    def test_malformed_repo_tag_sha_make_no_requests(self):
        _, rel, meta, sha = self.make_release(site_b_files(1))
        self.srv.publish(rel)
        bad = [
            dict(repo="Other-Org/test-dataset"), dict(repo="Artist-Alley-Org/../x"), dict(repo="Artist-Alley-Org/a/b"),
            dict(repo="Artist-Alley-Org/"), dict(tag="latest"), dict(tag="LATEST"), dict(tag="../v1"),
            dict(tag="v1/x"), dict(tag=""), dict(tag="v1..2"),
        ]
        for kw in bad:
            with self.subTest(**kw), self.assertRaises(fe.FetchError):
                self.run_fetch(meta, sha, **kw)
        for s in ("A" * 64, "0" * 63, "", "g" * 64):
            with self.subTest(sha=s), self.assertRaises(fe.FetchError):
                self.run_fetch(meta, s)
        self.assertEqual(self.srv.log, [])
        self.assertEqual(os.listdir(self.work), [])

    def test_non_production_base_requires_test_mode(self):
        _, rel, meta, sha = self.make_release(site_b_files(1))
        # a loopback base, and an HTTPS base the production redirect policy would otherwise accept
        for base in (self.srv.base, "https://objects.githubusercontent.com"):
            with self.subTest(base=base), self.assertRaises(fe.FetchError) as cm:
                fe.fetch(REPO, TAG, "site_b", sha, str(meta), str(self.t / "out"), work_dir=str(self.work),
                         _base_url=base)
            self.assertIn("only allowed in test mode", str(cm.exception))
        self.assertEqual(self.srv.log, [])

    def test_work_dir_inside_metadata_root_refused(self):
        _, rel, meta, sha = self.make_release(site_b_files(1))
        inner = meta / "dl"
        inner.mkdir()
        with self.assertRaises(fe.FetchError):
            fe.fetch(REPO, TAG, "site_b", sha, str(meta), str(self.t / "out"), work_dir=str(inner),
                     _base_url=self.srv.base, _test_origin_only=True)
        self.assertEqual(self.srv.log, [])

    def test_production_url_shape_and_policy(self):
        u = fe.asset_url(fe.PRODUCTION_BASE, REPO, TAG, "release.json")
        self.assertEqual(u, "https://github.com/Artist-Alley-Org/test-dataset/releases/download/v0.0.1-test/release.json")
        self.assertNotIn("latest", u)
        ok = ["https://github.com/a", "https://objects.githubusercontent.com/x",
              "https://release-assets.githubusercontent.com/y"]
        bad = ["http://github.com/a", "http://objects.githubusercontent.com/x", "https://evil.example/x",
               "https://github.com.evil.example/x", "https://user:pw@github.com/a", "ftp://github.com/a",
               "https://githubusercontent.com.evil/x"]
        for x in ok:
            self.assertTrue(fe.production_url_allowed(x), x)
        for x in bad:
            self.assertFalse(fe.production_url_allowed(x), x)

    def test_packager_hash_pin(self):
        real = fe.RELEASE_DATASET_SHA256
        fe.RELEASE_DATASET_SHA256 = "0" * 64
        try:
            with self.assertRaises(fe.FetchError):
                fe.load_release_dataset()
        finally:
            fe.RELEASE_DATASET_SHA256 = real
        self.assertEqual(hashlib.sha256((HERE / "release_dataset.py").read_bytes()).hexdigest(), real)

    def test_cli_usage_and_failure_without_network(self):
        script = str(HERE / "fetch_release_dataset.py")
        base = [sys.executable, "-I", "-B", script, "--repo", REPO, "--tag", TAG, "--site", "site_b",
                "--metadata-root", str(self.t), "--output", str(self.t / "out")]
        r = subprocess.run(base, capture_output=True, text=True)
        self.assertEqual(r.returncode, 2)                      # --expect-manifest-sha256 is required
        self.assertIn("expect-manifest-sha256", r.stderr)
        r = subprocess.run(base[:7] + ["latest"] + base[8:] + ["--expect-manifest-sha256", "0" * 64],
                           capture_output=True, text=True)
        self.assertEqual(r.returncode, 1)
        self.assertIn("FAILED", r.stderr)


if __name__ == "__main__":
    unittest.main()
