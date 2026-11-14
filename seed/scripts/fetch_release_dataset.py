#!/usr/bin/env python3
"""
Download a pinned public dataset release and reconstruct a verified site root (#1514, ADR 0100).

    fetch_release_dataset.py --repo Artist-Alley-Org/<repo> --tag <immutable-tag> --site {site_a,site_b}
        --expect-manifest-sha256 <64 hex> --metadata-root <checked-out metadata-only site root>
        --output <NEW site root> [--work-dir <parent for the private download directory>]
        [--release-format {v1,v2}]

--release-format says which asset layout the pinned release uses (release_dataset.py has both):
  v2  every new edition. The tag must be an edition (vYYYY.MM.NN[-variant]: month 01-12, edition
      number 01-99) and is checked before any request. The manifest asset is `<site>-<tag>-release.json`; after its hash is checked it
      must be a v2 manifest of exactly this site and edition, so a manifest of another edition is
      refused before any payload part is requested. Parts are `<site>-<tag>-payload.tar.gz.partNNNN`.
  v1  the default, for the published unversioned editions (v2026.11.01, v2026.11.01-sm):
      `release.json` and `<site>-payload.tar.gz.partNNNN`, exactly as before. A v2 manifest served
      as `release.json` is refused.

A tag alone is not an integrity pin, so the expected release.json SHA-256 is required on every
invocation. The release.json asset is downloaded first, under a small size cap, and its SHA-256 is
checked while streaming, BEFORE it is parsed or any part is requested. It is then parsed by the
reviewed offline tool (`release_dataset.load_manifest`), and its site must equal --site. Only the
parts it names are downloaded, in order, each capped at its declared size (always < 2 GiB) and
checked for size and SHA-256 while streaming. Only when every download passes does the offline
`release_dataset.verify` run with the same manifest pin. It extracts into a new site root, and
refuses to replace an existing one. The private download directory is removed on success and on
failure.

URLs are built only as https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>, never
`latest`. Redirects must stay HTTPS on github.com or *.githubusercontent.com. No token, secret or
authorization header is sent, and no downloaded bytes or private paths are printed.

The offline tool is loaded only if its bytes hash to RELEASE_DATASET_SHA256, the reviewed version.
Exit status: 0 success, 1 check failed, 2 usage error.
"""

from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import os
import re
import shutil
import sys
import tempfile
import types
import urllib.error
import urllib.parse
import urllib.request

RELEASE_DATASET_SHA256 = "1a58d64a7831865e186566e8292cbf70851721509f7deb00d0d86fad652bebdd"
PRODUCTION_BASE = "https://github.com"
ALLOWED_OWNER = "Artist-Alley-Org"
REPO_RE = re.compile(r"\AArtist-Alley-Org/[A-Za-z0-9][A-Za-z0-9._-]{0,99}\Z")
TAG_RE = re.compile(r"\A[A-Za-z0-9][A-Za-z0-9._+-]{0,127}\Z")
SHA_RE = re.compile(r"\A[0-9a-f]{64}\Z")
MAX_MANIFEST_BYTES = 1 << 20
MAX_REDIRECTS = 5
TIMEOUT = 60
CHUNK = 1 << 20
USER_AGENT = "artist-alley-release-fetch/1"


class FetchError(Exception):
    """A download or pin check failed. Nothing was installed."""


def load_release_dataset() -> types.ModuleType:
    """Load the reviewed offline tool from the exact bytes whose hash is pinned."""
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "release_dataset.py")
    with open(path, "rb") as fh:
        src = fh.read()
    got = hashlib.sha256(src).hexdigest()
    if got != RELEASE_DATASET_SHA256:
        raise FetchError(f"release_dataset.py sha256 {got} is not the reviewed {RELEASE_DATASET_SHA256}")
    mod = types.ModuleType("release_dataset")
    mod.__file__ = path
    sys.modules["release_dataset"] = mod
    exec(compile(src, path, "exec"), mod.__dict__)
    return mod


# ---------------------------------------------------------------------------- inputs and URLs

def check_inputs(repo: str, tag: str, expect_sha: str) -> None:
    if not isinstance(repo, str) or not REPO_RE.match(repo) or ".." in repo:
        raise FetchError(f"--repo must be {ALLOWED_OWNER}/<repository>")
    if not isinstance(tag, str) or not TAG_RE.match(tag) or ".." in tag or tag.lower() == "latest":
        raise FetchError("--tag must be an explicit immutable version tag (not 'latest')")
    if not isinstance(expect_sha, str) or not SHA_RE.match(expect_sha):
        raise FetchError("--expect-manifest-sha256 must be 64 lowercase hex characters")


def asset_url(base: str, repo: str, tag: str, asset: str) -> str:
    owner, name = repo.split("/", 1)
    q = lambda s: urllib.parse.quote(s, safe="")  # noqa: E731
    return f"{base}/{q(owner)}/{q(name)}/releases/download/{q(tag)}/{q(asset)}"


def production_url_allowed(url: str) -> bool:
    u = urllib.parse.urlsplit(url)
    host = (u.hostname or "").lower()
    return (u.scheme == "https" and not u.username and not u.password
            and (host == "github.com" or host.endswith(".githubusercontent.com")))


class _Policy:
    """Which URLs (first request and every redirect) may be fetched."""

    def __init__(self, base: str, test_origin_only: bool):
        self.test_origin_only = test_origin_only
        b = urllib.parse.urlsplit(base)
        self.origin = (b.scheme, b.hostname, b.port)

    def allowed(self, url: str) -> bool:
        if self.test_origin_only:
            u = urllib.parse.urlsplit(url)
            return (u.scheme, u.hostname, u.port) == self.origin and not u.username
        return production_url_allowed(url)


class _Redirects(urllib.request.HTTPRedirectHandler):
    max_redirections = MAX_REDIRECTS

    def __init__(self, policy: _Policy):
        super().__init__()
        self.policy = policy

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        newurl = urllib.parse.urljoin(req.full_url, newurl)
        if not self.policy.allowed(newurl):
            raise FetchError(f"refusing redirect to a disallowed URL (HTTP {code})")
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None:
            for h in list(new.headers):
                if h.lower() in ("authorization", "cookie"):
                    del new.headers[h]
        return new


def make_opener(policy: _Policy) -> urllib.request.OpenerDirector:
    return urllib.request.build_opener(_Redirects(policy), urllib.request.HTTPSHandler())


# ---------------------------------------------------------------------------- streaming download

def download(opener, policy: _Policy, url: str, dir_fd: int, name: str, *, cap: int,
             exact: int | None, want_sha: str | None) -> tuple[str, int]:
    """Stream url into dir/name. At most `cap` bytes; if `exact`, exactly that many; hash checked."""
    if not policy.allowed(url):
        raise FetchError(f"refusing to fetch a disallowed URL for {name}")
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, "Accept": "application/octet-stream"})
    h, n = hashlib.sha256(), 0
    try:
        resp = opener.open(req, timeout=TIMEOUT)
    except urllib.error.HTTPError as e:
        code = e.code
        e.close()
        raise FetchError(f"{name}: HTTP {code}") from None
    except urllib.error.URLError as e:
        if isinstance(e.reason, FetchError):
            raise e.reason from None
        raise FetchError(f"{name}: request failed: {type(e.reason).__name__}") from None
    try:
        if resp.status != 200:
            raise FetchError(f"{name}: HTTP {resp.status}")
        if not policy.allowed(resp.geturl()):
            raise FetchError(f"{name}: final URL is not allowed")
        cl = resp.headers.get("Content-Length")
        if cl is not None:
            if not cl.isdigit():
                raise FetchError(f"{name}: malformed Content-Length")
            if int(cl) > cap or (exact is not None and int(cl) != exact):
                raise FetchError(f"{name}: Content-Length {cl} does not match the expected size")
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=dir_fd)
        try:
            while True:
                try:
                    b = resp.read(min(CHUNK, cap + 1 - n))
                except http.client.IncompleteRead:
                    raise FetchError(f"{name}: truncated response") from None
                if not b:
                    break
                n += len(b)
                if n > cap or (exact is not None and n > exact):
                    raise FetchError(f"{name}: response larger than expected")
                h.update(b)
                view = memoryview(b)
                while view:
                    view = view[os.write(fd, view):]
            os.fsync(fd)
        finally:
            os.close(fd)
    except (http.client.HTTPException, OSError) as e:
        if isinstance(e, FetchError):
            raise
        raise FetchError(f"{name}: download failed: {type(e).__name__}") from None
    finally:
        resp.close()
    if exact is not None and n != exact:
        raise FetchError(f"{name}: truncated ({n} of {exact} bytes)")
    got = h.hexdigest()
    if want_sha is not None and got != want_sha:
        raise FetchError(f"{name}: SHA-256 does not match the pin")
    return got, n


# ---------------------------------------------------------------------------- main flow

RELEASE_FORMATS = ("v1", "v2")


def fetch(repo: str, tag: str, site: str, expect_manifest_sha256: str, metadata_root: str, output: str,
          *, work_dir: str | None = None, release_format: str = "v1", _base_url: str = PRODUCTION_BASE,
          _test_origin_only: bool = False, _opener=None) -> dict:
    """Download, check and reconstruct. `_base_url`, `_test_origin_only`, `_opener` are for tests only."""
    rd = load_release_dataset()
    try:
        return _fetch(rd, repo, tag, site, expect_manifest_sha256, metadata_root, output, work_dir,
                      release_format, _base_url, _test_origin_only, _opener)
    except rd.ReleaseError as e:
        raise FetchError(str(e)) from None


def _fetch(rd, repo, tag, site, expect_manifest_sha256, metadata_root, output, work_dir,
           release_format, _base_url, _test_origin_only, _opener) -> dict:
    check_inputs(repo, tag, expect_manifest_sha256)
    if site not in rd.SITE_PREFIXES:
        raise FetchError(f"--site must be one of {sorted(rd.SITE_PREFIXES)}")
    if release_format not in RELEASE_FORMATS:
        raise FetchError(f"--release-format must be one of {list(RELEASE_FORMATS)}")
    # v2: the tag IS the edition, so it must pass the edition rule before anything is requested;
    # v1 releases carry no edition (the tag is only the download path).
    edition = rd.check_edition(tag) if release_format == "v2" else None
    manifest = rd.manifest_name(site, edition)
    if not _test_origin_only and _base_url != PRODUCTION_BASE:
        raise FetchError("a non-production base URL is only allowed in test mode")
    metadata_root = os.path.abspath(metadata_root)
    if not os.path.isdir(metadata_root) or os.path.islink(metadata_root):
        raise FetchError("--metadata-root is not a real directory")
    output = rd.check_new_output(output, metadata_root)
    parent = os.path.abspath(work_dir) if work_dir else tempfile.gettempdir()
    if not os.path.isdir(parent) or os.path.islink(parent):
        raise FetchError("--work-dir is not a real directory")
    if rd.real_inside(parent, metadata_root):
        raise FetchError("the download directory must be outside the metadata root")
    if rd.real_inside(parent, output):
        raise FetchError("the download directory must be outside the output")
    policy = _Policy(_base_url, _test_origin_only)
    opener = _opener or make_opener(policy)
    dl = tempfile.mkdtemp(prefix="aa-release-download-", dir=parent)
    try:
        dfd = os.open(dl, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            # 1. the manifest: capped, hash-checked while streaming, BEFORE parsing or any part request
            download(opener, policy, asset_url(_base_url, repo, tag, manifest), dfd, manifest,
                     cap=MAX_MANIFEST_BYTES, exact=None, want_sha=expect_manifest_sha256)
            # 2. parse with the reviewed offline loader (re-checks the same pin; for v2 also that the
            #    manifest is of exactly this edition and every part name carries it)
            try:
                m, _ = rd.load_manifest(dl, expect_manifest_sha256, edition)
            except rd.ReleaseError as e:
                raise FetchError(f"{manifest} rejected: {e}") from None
            if m["site"] != site:
                raise FetchError(f"{manifest} is for {m['site']!r}, not {site!r}")
            if rd.manifest_file(m) != manifest:
                raise FetchError(f"{manifest} is not a {release_format} manifest")
            names = [p["name"] for p in m["parts"]]
            if len(set(names)) != len(names) or manifest in names:
                raise FetchError(f"{manifest} names a part twice")
            # 3. only the named parts, in order, each capped at its declared size
            for p in m["parts"]:
                rd.check_rel_path(p["name"])
                if "/" in p["name"]:
                    raise FetchError(f"unsafe part name {p['name']!r}")
                download(opener, policy, asset_url(_base_url, repo, tag, p["name"]), dfd, p["name"],
                         cap=p["bytes"], exact=p["bytes"], want_sha=p["sha256"])
            if sorted(os.listdir(dl)) != sorted([manifest] + names):
                raise FetchError("unexpected local entries in the download directory")
        finally:
            os.close(dfd)
        # 4. offline verification and extraction into a NEW site root
        try:
            result = rd.verify(metadata_root, dl, output, expect_manifest_sha256, edition)
        except (rd.ReleaseError, OSError) as e:
            raise FetchError(f"offline verification failed: {e}") from None
    finally:
        shutil.rmtree(dl, ignore_errors=True)
    return {"repo": repo, "tag": tag, "site": result["site"], "release_format": release_format,
            "edition": result["edition"], "manifest": result["manifest"],
            "release_json_sha256": result["release_json_sha256"],
            "parts": result["parts"], "payload_count": result["payload_count"],
            "payload_bytes": result["payload_bytes"], "stream_sha256": result["stream_sha256"]}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Download and verify a pinned public dataset release (ADR 0100).")
    ap.add_argument("--repo", required=True)
    ap.add_argument("--tag", required=True)
    ap.add_argument("--site", required=True, choices=["site_a", "site_b"])
    ap.add_argument("--expect-manifest-sha256", required=True)
    ap.add_argument("--metadata-root", required=True)
    ap.add_argument("--output", required=True)
    ap.add_argument("--work-dir")
    ap.add_argument("--release-format", choices=list(RELEASE_FORMATS), default="v1",
                    help="v2 for every new edition (site + tag in every asset name); v1 for the published "
                         "unversioned editions")
    a = ap.parse_args(argv)
    try:
        out = fetch(a.repo, a.tag, a.site, a.expect_manifest_sha256, a.metadata_root, a.output,
                    work_dir=a.work_dir, release_format=a.release_format)
    except FetchError as e:
        print(f"fetch_release_dataset: FAILED: {e}", file=sys.stderr)
        return 1
    print(json.dumps(out, indent=1, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
