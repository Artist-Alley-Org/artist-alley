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
import ci_capacity as cc  # noqa: E402
import ci_pins as cp  # noqa: E402

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


TAG2 = "v2026.11.02"
PREFIX2 = f"/{REPO}/releases/download/{TAG2}/"


class FetchV2(Base):
    """The versioned layout (#1514 follow-up): `--release-format v2`, every asset named <site>-<tag>-..."""

    def make_release_v2(self, files, edition=TAG2, site="site_b", max_part_bytes=rd.DEFAULT_MAX_PART_BYTES, name="v2"):
        src = write_root(self.t / name, files)
        rel = self.t / f"{name}-release"
        m = rd.build(str(src), site, str(rel), max_part_bytes, edition)
        meta = metadata_only_copy(src, self.t / f"{name}-meta", site)
        sha = hashlib.sha256((rel / m["manifest"]).read_bytes()).hexdigest()
        return src, rel, meta, sha, m

    def publish_at(self, rel, prefix, rename=None):
        for f in sorted(os.listdir(rel)):
            self.srv.routes[prefix + (rename or {}).get(f, f)] = {"kind": "ok", "body": (rel / f).read_bytes()}

    def fetch_v2(self, meta, sha, tag=TAG2, out=None):
        out = out or self.t / "out"
        return fe.fetch(REPO, tag, "site_b", sha, str(meta), str(out), work_dir=str(self.work),
                        release_format="v2", _base_url=self.srv.base, _test_origin_only=True)

    def test_v2_round_trip_requests_only_versioned_names(self):
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(9), max_part_bytes=12000)
        self.assertGreater(len(m["parts"]), 1)
        self.publish_at(rel, PREFIX2)
        r = self.fetch_v2(meta, sha)
        self.assertEqual((r["release_format"], r["edition"], r["manifest"]), ("v2", TAG2, "site_b-v2026.11.02-release.json"))
        self.assertEqual(self.srv.log, [PREFIX2 + m["manifest"]] + [PREFIX2 + p["name"] for p in m["parts"]])
        self.assertTrue(all(x.rsplit("/", 1)[1].startswith("site_b-v2026.11.02-") for x in self.srv.log))
        self.assertEqual(os.listdir(self.work), [])
        out = self.t / "out"                       # same output check as the v1 success test
        for line in (src / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
            h, p = line.split("  ", 1)
            self.assertEqual(hashlib.sha256((out / p).read_bytes()).hexdigest(), h)
        self.assertEqual((out / "SHA256SUMS").read_bytes(), (src / "SHA256SUMS").read_bytes())

    def test_manifest_of_another_edition_is_refused_before_any_part(self):
        # the v2026.11.02 release served under v2026.11.03 with its manifest renamed to that edition
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(3))
        p3 = f"/{REPO}/releases/download/v2026.11.03/"
        self.publish_at(rel, p3, rename={m["manifest"]: "site_b-v2026.11.03-release.json"})
        e = self.assertFetchFails("edition", meta, sha, release_format="v2", tag="v2026.11.03")
        self.assertIn("rejected", str(e))
        self.assertEqual(self.srv.log, [p3 + "site_b-v2026.11.03-release.json"])

    def test_malformed_tag_makes_no_request(self):
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(2))
        self.publish_at(rel, PREFIX2)
        for bad in ("v2026.11.2", "latest-ish", "v2026.11.02-SM", "v0.0.1-test"):
            with self.subTest(tag=bad):
                self.assertFetchFails("edition tag", meta, sha, release_format="v2", tag=bad)
        self.assertEqual(self.srv.log, [])

    def test_impossible_edition_makes_no_request(self):
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(2))
        for bad in ("v2026.13.01", "v2026.00.01", "v2026.11.00", "v0000.11.01", "v2026.13.01-sm"):
            with self.subTest(tag=bad):
                self.publish_at(rel, f"/{REPO}/releases/download/{bad}/")
                self.assertFetchFails("real calendar edition", meta, sha, release_format="v2", tag=bad)
        self.assertEqual(self.srv.log, [])

    def test_edition_number_past_the_month_length_round_trip(self):
        # NN counts editions within the month (ADR 0100), so the 30th February edition is a real one.
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(2), edition="v2026.02.30")
        prefix = f"/{REPO}/releases/download/v2026.02.30/"
        self.publish_at(rel, prefix)
        r = self.fetch_v2(meta, sha, tag="v2026.02.30")
        self.assertEqual((r["edition"], r["manifest"]), ("v2026.02.30", "site_b-v2026.02.30-release.json"))
        self.assertEqual(self.srv.log[0], prefix + "site_b-v2026.02.30-release.json")

    def test_v1_tag_parsing_is_unchanged(self):
        # The legacy v1 layout carries no edition, so its tag rule is the old one: no calendar check.
        for tag in ("v2026.11.01", "v2026.11.01-sm", "v0.0.1-test", "v2026.13.01"):
            with self.subTest(tag=tag):
                fe.check_inputs(REPO, tag, "0" * 64)

    def test_tampered_v2_manifest_is_refused_before_any_part(self):
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(3))
        mp = rel / m["manifest"]
        doc = json.loads(mp.read_text())
        doc["payload"]["count"] += 1
        mp.write_text(json.dumps(doc), encoding="utf-8")
        self.publish_at(rel, PREFIX2)
        self.assertFetchFails("does not match the pin", meta, sha, release_format="v2", tag=TAG2)
        self.assertEqual(self.srv.log, [PREFIX2 + m["manifest"]])

    def test_v1_request_against_a_v2_release_fails_closed(self):
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(2))
        self.publish_at(rel, PREFIX2)
        self.assertFetchFails("HTTP 404", meta, sha, tag=TAG2)                    # default format is v1
        self.assertEqual(self.srv.log, [PREFIX2 + "release.json"])

    def test_v2_request_against_a_v1_release_fails_closed(self):
        src, rel, meta, sha = self.make_release(site_b_files(2))
        self.publish_at(rel, PREFIX2)
        self.assertFetchFails("HTTP 404", meta, sha, release_format="v2", tag=TAG2)
        self.assertEqual(self.srv.log, [PREFIX2 + "site_b-v2026.11.02-release.json"])

    def test_v2_manifest_served_as_release_json_is_refused(self):
        # a v1 fetch whose pinned bytes are a v2 manifest: the loader refuses the name/schema pairing
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(2))
        self.publish_at(rel, PREFIX2, rename={m["manifest"]: "release.json"})
        self.assertFetchFails("does not match its recorded", meta, sha, tag=TAG2)
        self.assertEqual(self.srv.log, [PREFIX2 + "release.json"])

    def test_unknown_release_format_makes_no_request(self):
        src, rel, meta, sha, m = self.make_release_v2(site_b_files(2))
        self.publish_at(rel, PREFIX2)
        self.assertFetchFails("release-format", meta, sha, release_format="v3", tag=TAG2)
        self.assertEqual(self.srv.log, [])


REPO_ROOT = HERE.parents[1]
PINS = REPO_ROOT / "seed" / "contracts" / "ci-datasets.v1.json"
HELPER = REPO_ROOT / "scripts" / "ci" / "public-datasets.sh"
WORKFLOWS = {
    "ui-pr": (REPO_ROOT / ".github" / "workflows" / "ui-pr.yml", ["site_a_sm"]),
    "ui-nightly": (REPO_ROOT / ".github" / "workflows" / "ui-nightly.yml", ["site_b", "site_a"]),
}


def code_lines(path: Path) -> str:
    """The file without its comment lines, so prose that NAMES a retired mechanism is not a hit."""
    return "\n".join(ln for ln in path.read_text(encoding="utf-8").splitlines() if not ln.lstrip().startswith("#"))


class CIDatasetPins(unittest.TestCase):
    """The pins file the hosted UI workflows consume (#1514). Text checks only: stdlib, no network."""

    def setUp(self):
        self.pins = json.loads(PINS.read_text(encoding="utf-8"))

    def test_schema_and_sites(self):
        self.assertEqual(self.pins["schema"], "artist-alley.ci-datasets/v1")
        self.assertEqual(sorted(self.pins["sites"]), ["site_a", "site_a_sm", "site_b"])

    def test_every_pin_is_exact(self):
        import re
        for key, s in self.pins["sites"].items():
            with self.subTest(key=key):
                site = cp.value(self.pins, key, "site")
                self.assertRegex(s["repo"], r"\AArtist-Alley-Org/artist-alley-dataset-site-[ab]\Z")
                self.assertTrue(s["repo"].endswith(site.replace("_", "-")))
                want_tag = r"\Av[0-9]{4}\.[0-9]{2}\.[0-9]{2}-sm\Z" if key == "site_a_sm" else r"\Av[0-9]{4}\.[0-9]{2}\.[0-9]{2}\Z"
                self.assertRegex(s["tag"], want_tag)
                self.assertNotEqual(s["tag"].lower(), "latest")
                for field in ("repo", "tag", "commit", "release_json_sha256", "site", "need_bytes"):
                    cp.value(self.pins, key, field)
                self.assertRegex(s["commit"], r"\A[0-9a-f]{40}\Z")
                self.assertRegex(s["release_json_sha256"], r"\A[0-9a-f]{64}\Z")
                fe.check_inputs(s["repo"], s["tag"], s["release_json_sha256"])
                for k in ("download_bytes", "extracted_bytes"):
                    self.assertIsInstance(s["capacity"][k], int)
                    self.assertGreater(s["capacity"][k], 0)
                self.assertIsNone(re.search(r"https?://", json.dumps(s)))

    def test_pinned_values_are_the_published_v2026_11_01(self):
        a, b = self.pins["sites"]["site_a"], self.pins["sites"]["site_b"]
        self.assertEqual((a["tag"], a["commit"], a["release_json_sha256"]),
                         ("v2026.11.01", "62ebfa6a642f96ceda829ffdad42601d4d9fe525",
                          "84996745cda978476dacfe7cf8ae1030d0d0d21faa7f1593e3aa713e0c092da5"))
        self.assertEqual((b["tag"], b["commit"], b["release_json_sha256"]),
                         ("v2026.11.01", "9fa8fb76cf208d2b3e6a40d13ed6a44a715ef36e",
                          "a4b1fa20ddefab64c161fdd27bf393abf85ea78ec240d9cfb9c3e4c9d8451093"))

    def test_pinned_values_of_the_site_a_ci_subset_edition(self):
        sm, a = self.pins["sites"]["site_a_sm"], self.pins["sites"]["site_a"]
        self.assertEqual((sm["site"], sm["repo"], sm["tag"], sm["commit"], sm["release_json_sha256"]),
                         ("site_a", a["repo"], a["tag"] + "-sm", "3f078b1500ce066d5df04adeba11d38fc8ad7277",
                          "c6838e5020b1bc56f048c0602c2021c1c840e3e3ddf236827eb7358b521a503f"))
        self.assertEqual(sm["capacity"], {"download_bytes": 658160717, "extracted_bytes": 666231365})

    def test_tag_shape_is_exact_per_key(self):
        def with_tag(key, tag, **extra):
            d = json.loads(json.dumps(self.pins))
            d["sites"][key]["tag"] = tag
            d["sites"][key].update(extra)
            return d
        good = {"site_a": "v2026.11.01", "site_b": "v2026.12.03", "site_a_sm": "v2026.11.01-sm"}
        for key, tag in good.items():
            self.assertEqual(cp.value(with_tag(key, tag), key, "tag"), tag)
        bad = {"site_a": ["v2026.11.01-sm", "latest", "v2026.11.1", "main", "v2026.11.01 "],
               "site_b": ["v2026.11.01-sm"],
               "site_a_sm": ["v2026.11.01", "v2026.11.01-xs", "v2026.11.01-sm-2", "v2026.11.01-SM", "latest"]}
        for key, tags in bad.items():
            for tag in tags:
                with self.subTest(key=key, tag=tag), self.assertRaises(cp.PinError):
                    cp.value(with_tag(key, tag), key, "tag")
        with self.assertRaises(cp.PinError):
            cp.value(with_tag("site_a_sm", "v2026.11.01-sm", site="site_b"), "site_a_sm", "site")
        with self.assertRaises(cp.PinError):
            cp.value(self.pins, "site_c", "tag")
        import contextlib
        import io
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(cp.main([str(PINS), "site_a_sm", "site"]), 0)
            self.assertEqual(cp.main([str(PINS), "site_a", "nope"]), 1)
        self.assertEqual(out.getvalue(), "site_a\n")

    def with_pin(self, key, **fields):
        d = json.loads(json.dumps(self.pins))
        for k, v in fields.items():
            if v is None:
                d["sites"][key].pop(k, None)
            else:
                d["sites"][key][k] = v
        return d

    def test_release_format_defaults_to_v1_for_the_current_pins(self):
        self.assertEqual(cp.RELEASE_FORMATS, fe.RELEASE_FORMATS)
        for key, s in self.pins["sites"].items():
            with self.subTest(key=key):
                self.assertNotIn("release_format", s)              # the published pins stay byte-identical
                self.assertEqual(cp.value(self.pins, key, "release_format"), "v1")
                self.assertEqual(cp.value(self.pins, key, "tag"), s["tag"])
        d = self.with_pin("site_b", release_format="v1")           # an explicit v1 is the same pin
        self.assertEqual(cp.value(d, "site_b", "release_format"), "v1")

    def test_release_format_v2_future_pin(self):
        future = {"site_a": "v2026.12.01", "site_b": "v2026.11.02", "site_a_sm": "v2026.12.01-sm"}
        for key, tag in future.items():
            with self.subTest(key=key):
                d = self.with_pin(key, release_format="v2", tag=tag)
                self.assertEqual(cp.value(d, key, "release_format"), "v2")
                self.assertEqual(cp.value(d, key, "tag"), tag)
                for field in ("repo", "commit", "release_json_sha256", "site", "need_bytes"):
                    cp.value(d, key, field)

    def test_release_format_invalid_values_fail(self):
        for bad in ("v3", "V2", "v2 ", "", "2", "release.json", 2, ["v2"], {"v": 2}, True):
            with self.subTest(release_format=bad):
                d = self.with_pin("site_b", release_format=bad)
                with self.assertRaises(cp.PinError):
                    cp.value(d, "site_b", "release_format")
                with self.assertRaises(cp.PinError):             # the tag is not usable without a valid format
                    cp.value(d, "site_b", "tag")
        d = json.loads(json.dumps(self.pins))
        d["sites"]["site_b"]["release_format"] = None             # JSON null is not "absent"
        with self.assertRaises(cp.PinError):
            cp.value(d, "site_b", "release_format")

    def test_v2_pin_needs_a_real_calendar_edition(self):
        for key, tag in (("site_b", "v2026.13.01"), ("site_b", "v2026.00.01"), ("site_a", "v2026.11.00"),
                         ("site_a_sm", "v2026.13.01-sm")):
            with self.subTest(key=key, tag=tag):
                with self.assertRaises(cp.PinError) as cm:
                    cp.value(self.with_pin(key, release_format="v2", tag=tag), key, "tag")
                self.assertIn("real calendar edition", str(cm.exception))
                # The legacy v1 pin tag shape is unchanged: the same string is still a v1-shaped tag.
                self.assertEqual(cp.value(self.with_pin(key, tag=tag), key, "tag"), tag)

    def test_helper_uses_the_pins_and_the_reviewed_tool(self):
        src = code_lines(HELPER)
        self.assertIn("seed/contracts/ci-datasets.v1.json", src)
        self.assertIn("seed/scripts/fetch_release_dataset.py", src)
        self.assertIn("seed/scripts/ci_pins.py", src)
        self.assertIn('--site "$dsite"', src)
        self.assertIn('fmt="$(pin "$site" release_format)"', src)
        self.assertIn('--release-format "$fmt"', src)
        self.assertLess(src.index('pin "$site" release_format'), src.index("ls-remote"))   # before any network
        self.assertIn("--expect-manifest-sha256", src)
        self.assertIn("sha256sum --quiet --strict -c SHA256SUMS", src)
        self.assertNotIn("latest", src)
        self.assertNotIn("TOKEN", src)

    def test_workflows_are_hosted_and_secret_free_for_data(self):
        for wf, (path, sites) in WORKFLOWS.items():
            src = code_lines(path)
            with self.subTest(workflow=wf):
                # assertTrue/False with a short message: assertIn would print the whole workflow.
                for retired in ("self-hosted", "SEED_SITE", "runner-tls", "docker network"):
                    self.assertFalse(retired in src, f"{wf}: still uses {retired!r}")
                wanted = ["runs-on: ubuntu-24.04", "infra/docker/ci/docker-compose.hosted.yml",
                          "public-datasets.sh prepare",
                          "${AA_CI_DATASET_ROOT:?the dataset prepare step did not run}/sites/site_a"]
                wanted += [f"public-datasets.sh fetch {site}" for site in sites]
                for w in wanted:
                    self.assertTrue(w in src, f"{wf}: missing {w!r}")

    def test_check_names_preserved(self):
        pr = WORKFLOWS["ui-pr"][0].read_text(encoding="utf-8")
        self.assertIn("name: UI dogfood (standalone)", pr)
        self.assertIn("name: Playwright standalone", pr)
        nightly = WORKFLOWS["ui-nightly"][0].read_text(encoding="utf-8")
        self.assertIn("name: Playwright federation + shell scenarios", nightly)




STUB_GIT = r"""#!/usr/bin/env bash
# Records every call; answers ls-remote / clone / rev-parse / ls-files for the pinned commit. No network.
printf '%s\n' "$*" >> "$STUB_GIT_LOG"
case " $* " in
  *" ls-remote "*) printf '%s\trefs/tags/x\n' "$STUB_COMMIT" ;;
  *" clone "*) mkdir -p "${@: -1}" ;;
  *" rev-parse HEAD"*) echo "$STUB_COMMIT" ;;
  *" ls-files"*) : ;;
  *) exit 97 ;;
esac
"""

STUB_FETCH = r"""import json, os, sys
if __name__ == "__main__":
    # Run by the helper: record the argv it was handed, then fail so the helper stops.
    with open(os.environ["STUB_FETCH_LOG"], "w", encoding="utf-8") as fh:
        json.dump(sys.argv[1:], fh)
    sys.exit(3)
# Imported by ci_pins.py for the v2 tag check: behave as the real reviewed fetcher.
__file__ = os.environ["STUB_REAL_FETCH"]
exec(compile(open(__file__, encoding="utf-8").read(), __file__, "exec"))
"""


class CIHelperReleaseFormat(unittest.TestCase):
    """scripts/ci/public-datasets.sh fetch hands the pin's validated release_format to the fetcher, and a bad
    format or impossible v2 date stops it before any git call. Run on a copy of the helper + ci_pins.py with a
    stub git on PATH and a stub fetcher that only records its argv: nothing reaches the network."""

    def setUp(self):
        self.t = Path(tempfile.mkdtemp(prefix="aa-helper-fmt-"))
        self.addCleanup(subprocess.run, ["rm", "-rf", "--", str(self.t)])
        tree = self.t / "tree"
        (tree / "scripts" / "ci").mkdir(parents=True)
        (tree / "seed" / "scripts").mkdir(parents=True)
        (tree / "seed" / "contracts").mkdir(parents=True)
        (tree / "scripts" / "ci" / "public-datasets.sh").write_bytes(HELPER.read_bytes())
        (tree / "seed" / "scripts" / "ci_pins.py").write_bytes((HERE / "ci_pins.py").read_bytes())
        (tree / "seed" / "scripts" / "fetch_release_dataset.py").write_text(STUB_FETCH, encoding="utf-8")
        bin_ = self.t / "bin"
        bin_.mkdir()
        (bin_ / "git").write_text(STUB_GIT, encoding="utf-8")
        os.chmod(bin_ / "git", 0o755)
        self.tree, self.bin = tree, bin_
        self.git_log, self.fetch_log = self.t / "git.log", self.t / "fetch.json"

    def run_helper(self, pins: dict, key: str):
        (self.tree / "seed" / "contracts" / "ci-datasets.v1.json").write_text(json.dumps(pins), encoding="utf-8")
        root = self.t / "run" / "aa-ci-datasets"
        subprocess.run(["rm", "-rf", "--", str(self.t / "run")], check=True)
        for d in ("meta", "dl", "sites"):
            (root / d).mkdir(parents=True)
        for f in (self.git_log, self.fetch_log):
            if f.exists():
                f.unlink()
        env = {"PATH": f"{self.bin}:{os.environ['PATH']}", "HOME": str(self.t), "AA_CI_DATASET_ROOT": str(root),
               "STUB_GIT_LOG": str(self.git_log), "STUB_FETCH_LOG": str(self.fetch_log),
               "STUB_COMMIT": pins["sites"][key]["commit"],
               "STUB_REAL_FETCH": str(HERE / "fetch_release_dataset.py")}
        return subprocess.run(["bash", str(self.tree / "scripts" / "ci" / "public-datasets.sh"), "fetch", key],
                              env=env, capture_output=True, text=True, timeout=60)

    def pins_with(self, key, **fields):
        d = json.loads(PINS.read_text(encoding="utf-8"))
        d["sites"][key].update(fields)
        return d

    def fetch_argv(self):
        return json.loads(self.fetch_log.read_text(encoding="utf-8"))

    def test_current_pin_without_the_field_fetches_v1(self):
        r = self.run_helper(json.loads(PINS.read_text(encoding="utf-8")), "site_b")
        self.assertEqual(r.returncode, 1, r.stderr)                 # the stub fetcher exits 3 after recording
        a = self.fetch_argv()
        self.assertEqual(a[a.index("--release-format") + 1], "v1")
        self.assertEqual(a[a.index("--tag") + 1], "v2026.11.01")
        self.assertIn("ls-remote", self.git_log.read_text())

    def test_v2_future_pin_fetches_v2(self):
        r = self.run_helper(self.pins_with("site_a_sm", release_format="v2", tag="v2026.12.01-sm"), "site_a_sm")
        self.assertEqual(r.returncode, 1, r.stderr)
        a = self.fetch_argv()
        self.assertEqual(a[a.index("--release-format") + 1], "v2")
        self.assertEqual((a[a.index("--tag") + 1], a[a.index("--site") + 1]), ("v2026.12.01-sm", "site_a"))
        self.assertIn("release format v2", r.stdout)
        self.assertIn("ls-remote", self.git_log.read_text())

    def test_bad_format_or_date_stops_before_any_git_call(self):
        cases = [("site_b", dict(release_format="v3")), ("site_b", dict(release_format="")),
                 ("site_a", dict(release_format=["v2"])),
                 ("site_b", dict(release_format="v2", tag="v2026.00.01")),
                 ("site_a_sm", dict(release_format="v2", tag="v2026.13.01-sm"))]
        for key, fields in cases:
            with self.subTest(key=key, **{k: str(v) for k, v in fields.items()}):
                r = self.run_helper(self.pins_with(key, **fields), key)
                self.assertNotEqual(r.returncode, 0)
                self.assertIn("ci_pins:", r.stderr)
                self.assertIn("real calendar edition" if "tag" in fields else "release_format", r.stderr)
                self.assertFalse(self.git_log.exists(), "git ran despite a malformed pin")
                self.assertFalse(self.fetch_log.exists(), "the fetcher ran despite a malformed pin")


class CICapacityModel(unittest.TestCase):
    """The disk phase model the hosted UI workflows gate on (#1514). rev134 summed every phase and
    could never pass on the advertised runner; these pin the model, its arithmetic and the order the
    workflows must keep for the model to be true."""

    def setUp(self):
        self.pins = json.loads(PINS.read_text(encoding="utf-8"))
        self.advertised = self.pins["capacity_model"]["advertised_runner"]["storage_bytes"]
        self.m = {p: cc.load_profile(self.pins, p) for p in ("ui-pr", "ui-nightly")}

    def test_every_profile_fits_the_advertised_standard_runner(self):
        self.assertEqual(self.advertised, 14_000_000_000)
        for p, m in self.m.items():
            with self.subTest(profile=p):
                self.assertLessEqual(cc.preflight_need(m), self.advertised)
                self.assertLessEqual(cc.prepare_need(m), self.advertised)

    def test_rev134_summed_floor_is_what_this_replaces(self):
        # rev134: download + extracted of every site + 1 GiB, plus an 8 / 10 GiB docker floor on one disk.
        floors = {"ui-pr": 8, "ui-nightly": 10}
        for p, m in self.m.items():
            rev134 = m["download"] + m["extracted"] + cc.GIB + floors[p] * cc.GIB
            with self.subTest(profile=p):
                self.assertGreater(rev134, cc.preflight_need(m))
        nightly = self.m["ui-nightly"]
        self.assertGreater(nightly["download"] + nightly["extracted"] + cc.GIB + 10 * cc.GIB, self.advertised)

    def test_phase_arithmetic(self):
        m = {"download": 10, "extracted": 20, "build_peak": 100, "build_kept": 40, "seed": 3,
             "runtime": 5, "late": 7}
        self.assertEqual(cc.data_phases(m), {"fetch": 30, "seed": 31, "late": 18})
        self.assertEqual(cc.plan_phases(m), {"build": 107, "fetch": 77, "seed": 78, "late": 58})
        self.assertEqual(cc.prepare_need(m), 31 + cc.GIB)
        self.assertEqual(cc.preflight_need(m), 107 + cc.GIB)
        big = dict(m, build_peak=20 * cc.GIB)
        self.assertEqual(cc.preflight_need(big), 20 * cc.GIB + 7 + int((20 * cc.GIB + 7) * 0.10))
        self.assertEqual(cc.prepare_need_split(m), (30 + cc.GIB, 18 + cc.GIB))

    def test_cli_fails_closed_on_measured_shortfall(self):
        import contextlib
        import io
        quiet = contextlib.ExitStack()
        quiet.enter_context(contextlib.redirect_stdout(io.StringIO()))
        quiet.enter_context(contextlib.redirect_stderr(io.StringIO()))
        self.addCleanup(quiet.close)
        need = cc.preflight_need(self.m["ui-nightly"])
        args = ["preflight", "ui-nightly", "--pins", str(PINS)]
        self.assertEqual(cc.main(args + ["--free", str(need)]), 0)
        self.assertEqual(cc.main(args + ["--free", str(need - 1)]), 1)
        pneed = cc.prepare_need(self.m["ui-pr"])
        pargs = ["prepare", "ui-pr", "--pins", str(PINS), "--same-fs"]
        self.assertEqual(cc.main(pargs + ["--dataset-free", str(pneed), "--docker-free", "0"]), 0)
        self.assertEqual(cc.main(pargs + ["--dataset-free", str(pneed - 1), "--docker-free", "0"]), 1)
        ds, dk = cc.prepare_need_split(self.m["ui-pr"])
        split = ["prepare", "ui-pr", "--pins", str(PINS)]
        self.assertEqual(cc.main(split + ["--dataset-free", str(ds), "--docker-free", str(dk)]), 0)
        self.assertEqual(cc.main(split + ["--dataset-free", str(ds), "--docker-free", str(dk - 1)]), 1)

    def test_malformed_model_is_refused(self):
        bad = json.loads(json.dumps(self.pins))
        bad["capacity_model"]["profiles"]["ui-pr"]["build_kept_bytes"] = 10 ** 12
        with self.assertRaises(cc.CapacityError):
            cc.load_profile(bad, "ui-pr")
        bad = json.loads(json.dumps(self.pins))
        bad["capacity_model"]["profiles"]["ui-pr"]["seed_bytes"] = "800000000"
        with self.assertRaises(cc.CapacityError):
            cc.load_profile(bad, "ui-pr")
        with self.assertRaises(cc.CapacityError):
            cc.load_profile(self.pins, "ui-everything")

    def test_profiles_match_the_workflows_and_their_phase_order(self):
        order = {
            "ui-pr": ["public-datasets.sh preflight ui-pr", "docker compose up -d --build app postgres",
                      "docker builder prune -af", "public-datasets.sh prepare ui-pr",
                      "public-datasets.sh fetch site_a_sm", "app seed --site /seed/site",
                      "public-datasets.sh release"],
            "ui-nightly": ["public-datasets.sh preflight ui-nightly", "./scripts/dogfood/up.sh",
                           "docker builder prune -af", "public-datasets.sh prepare ui-nightly",
                           "public-datasets.sh fetch site_b", "public-datasets.sh fetch site_a",
                           "app seed --profile ci", "public-datasets.sh release"],
        }
        for p, marks in order.items():
            src = code_lines(WORKFLOWS[p][0])
            with self.subTest(workflow=p):
                at = [src.find(x) for x in marks]
                self.assertNotIn(-1, at, f"{p}: missing one of {marks}")
                self.assertEqual(at, sorted(at), f"{p}: steps out of the modelled phase order")
                self.assertEqual(self.m[p]["sites"], WORKFLOWS[p][1])


if __name__ == "__main__":
    unittest.main()
