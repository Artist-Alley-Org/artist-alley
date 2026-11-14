#!/usr/bin/env python3
"""
Local release-payload builder and offline verifier for metadata-only dataset roots (#1514).

The public dataset repositories hold metadata only. The binary payload ships as a versioned
GitHub Release made of sequential parts of one deterministic tar.gz stream. This tool:

  inspect  check a dataset root (SHA256SUMS, payload listing) without writing anything
  build    package the listed payload files of a root into release parts + a release manifest
  verify   check a release against a metadata-only root and reconstruct a NEW site root

Two release formats (both verified offline and fetched online):
  v2 (every new build)  `build --tag vYYYY.MM.NN[-variant]`: year, month 01-12 and edition number
                        01-99 within the month (checked before any output is created). Every asset basename carries the site
                        AND the immutable edition tag, so two editions downloaded into one folder
                        can never overwrite each other:
                          <site>-<tag>-release.json
                          <site>-<tag>-payload.tar.gz.part0000, ...0001, ...
                        The manifest (schema artist-alley.dataset-release/v2) records `edition` and
                        its own `manifest` name; both must agree with the file names and, when
                        given, with the expected tag.
  v1 (legacy)           `release.json` + `<site>-payload.tar.gz.partNNNN`, schema .../v1. The
                        published v2026.11.01 and v2026.11.01-sm editions use it. It stays fully
                        verifiable, and `build --legacy-v1` reproduces it byte for byte; it is not
                        for new editions.

It is stdlib-only, makes no network access and knows no URLs. Packaging or verifying a payload
checks BYTES only: it is not rights clearance, not release approval and not permission to publish.

Safety rules (all fail closed):
  * every path is a clean relative POSIX path; files are opened through a directory-fd walk with
    O_NOFOLLOW on every component and must be regular files (no symlinks anywhere);
  * SHA256SUMS lines are exactly "<64 hex>  <path>"; duplicates, unsafe paths and a self-entry
    are rejected; payload files under the site's prefixes must be listed, and listed files must
    exist with the listed hash;
  * outputs are built in a private temporary sibling and installed with renameat2(RENAME_NOREPLACE),
    so a pre-existing output is never replaced; on any error the temporary directory is removed.

Usage:
  release_dataset.py inspect --root ROOT --site {site_a,site_b}
  release_dataset.py build   --root ROOT --site {site_a,site_b} --output NEW_RELEASE_DIR
                             (--tag EDITION | --legacy-v1) [--max-part-bytes N]
  release_dataset.py verify  --root METADATA_ROOT --release RELEASE_DIR --output NEW_SITE_ROOT
                             [--expect-manifest-sha256 HEX] [--tag EDITION]
Exit status: 0 success, 1 check failed, 2 usage error.
"""

from __future__ import annotations

import argparse
import ctypes
import gzip
import hashlib
import io
import json
import os
import posixpath
import re
import shutil
import stat
import sys
import tarfile
import tempfile
import zlib

SCHEMA = "artist-alley.dataset-release/v1"          # legacy, unversioned asset names
SCHEMA_V2 = "artist-alley.dataset-release/v2"       # every asset name carries site + edition
TOOL_VERSION = 1
MANIFEST_NAME = "release.json"                      # the v1 manifest's (only) name
SUMS_NAME = "SHA256SUMS"
SITE_PREFIXES = {
    "site_a": ("3d/", "audio/", "documents/", "fonts/", "images/", "videos/"),
    "site_b": ("images/",),
}
DEFAULT_MAX_PART_BYTES = 1_900_000_000
HARD_MAX_PART_BYTES = 2 * 1024 ** 3      # parts are strictly smaller, so always < 2 GiB
MIN_MAX_PART_BYTES = 2
MAX_PARTS = 9999
GZIP_LEVEL = 6
CHUNK = 1 << 20
NOTICE = ("CANDIDATE PAYLOAD ONLY. Packaging and verification check bytes, not rights: this is not "
          "rights clearance, not release approval and not permission to publish.")
HEX64 = re.compile(r"\A[0-9a-f]{64}\Z")
# An immutable edition tag: calendar edition vYYYY.MM.NN (year, month, edition number within the month; see
# check_edition), optional lowercase variant (v2026.11.02, v2026.11.02-sm).
# Deliberately narrow: no `/`, no `..`, no upper case, nothing that could change a path or a URL.
EDITION_PATTERN = r"v[0-9]{4}\.[0-9]{2}\.[0-9]{2}(?:-[a-z][a-z0-9]{0,15})?"
EDITION_RE = re.compile(r"\A" + EDITION_PATTERN + r"\Z")
V2_MANIFEST_RE = re.compile(r"\A(site_a|site_b)-(" + EDITION_PATTERN + r")-release\.json\Z")


class ReleaseError(Exception):
    """A check failed. Nothing was installed."""


# ---------------------------------------------------------------------------- paths and files

def check_rel_path(p: str) -> str:
    if not isinstance(p, str) or not p:
        raise ReleaseError(f"unsafe path {p!r}: empty")
    if "\0" in p or "\\" in p or "\n" in p or "\r" in p:
        raise ReleaseError(f"unsafe path {p!r}: forbidden character")
    if p.startswith("/"):
        raise ReleaseError(f"unsafe path {p!r}: absolute")
    parts = p.split("/")
    if any(c in ("", ".", "..") for c in parts) or posixpath.normpath(p) != p:
        raise ReleaseError(f"unsafe path {p!r}: not a clean relative path")
    return p


def _open_dir_nofollow(root: str) -> int:
    try:
        return os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    except OSError as e:
        raise ReleaseError(f"cannot open directory {root!r} without following links: {e}") from None


def open_regular(root: str, rel: str) -> int:
    """fd of root/rel, refusing symlinks in every component and non-regular files."""
    check_rel_path(rel)
    parts = rel.split("/")
    fd = _open_dir_nofollow(root)
    try:
        for c in parts[:-1]:
            try:
                nfd = os.open(c, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            except OSError as e:
                raise ReleaseError(f"{rel}: cannot open directory component {c!r}: {e}") from None
            os.close(fd)
            fd = nfd
        try:
            ffd = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW, dir_fd=fd)
        except OSError as e:
            raise ReleaseError(f"{rel}: cannot open without following links: {e}") from None
    finally:
        os.close(fd)
    if not stat.S_ISREG(os.fstat(ffd).st_mode):
        os.close(ffd)
        raise ReleaseError(f"{rel}: not a regular file")
    return ffd


def read_regular(root: str, rel: str) -> bytes:
    fd = open_regular(root, rel)
    try:
        chunks = []
        while True:
            b = os.read(fd, CHUNK)
            if not b:
                return b"".join(chunks)
            chunks.append(b)
    finally:
        os.close(fd)


def hash_fd(fd: int) -> tuple[str, int]:
    h, n = hashlib.sha256(), 0
    while True:
        b = os.read(fd, CHUNK)
        if not b:
            return h.hexdigest(), n
        h.update(b)
        n += len(b)


def create_file(root_fd: int, rel: str) -> int:
    """Create root/rel exclusively (parents as needed) inside a directory we own; no links."""
    check_rel_path(rel)
    parts = rel.split("/")
    fd = os.dup(root_fd)
    try:
        for c in parts[:-1]:
            try:
                os.mkdir(c, 0o755, dir_fd=fd)
            except FileExistsError:
                pass
            nfd = os.open(c, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = nfd
        return os.open(parts[-1], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o644, dir_fd=fd)
    finally:
        os.close(fd)


def write_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    while view:
        n = os.write(fd, view)
        view = view[n:]


def real_inside(child: str, parent: str) -> bool:
    c, p = os.path.realpath(child), os.path.realpath(parent)
    return c == p or c.startswith(p.rstrip(os.sep) + os.sep)


def check_new_output(out: str, *forbidden_roots: str) -> str:
    out = os.path.abspath(out)
    if os.path.lexists(out):
        raise ReleaseError(f"output {out!r} already exists; refusing to replace it")
    parent = os.path.dirname(out)
    if not os.path.isdir(parent) or os.path.islink(parent):
        raise ReleaseError(f"output parent {parent!r} is not an existing real directory")
    for r in forbidden_roots:
        if real_inside(parent, r) or real_inside(r, out):
            raise ReleaseError(f"output {out!r} overlaps input {r!r}")
    return out


_RENAME_NOREPLACE = 1
_AT_FDCWD = -100


def install_noreplace(tmp: str, out: str) -> None:
    """Atomically rename tmp to out, failing if out exists (Linux renameat2)."""
    libc = ctypes.CDLL(None, use_errno=True)
    fn = getattr(libc, "renameat2", None)
    if fn is None:
        raise ReleaseError("renameat2 is unavailable; refusing a non-atomic install")
    fn.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    if fn(_AT_FDCWD, os.fsencode(tmp), _AT_FDCWD, os.fsencode(out), _RENAME_NOREPLACE) != 0:
        err = ctypes.get_errno()
        raise ReleaseError(f"install of {out!r} failed: {os.strerror(err)}")
    pfd = os.open(os.path.dirname(out), os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(pfd)
    finally:
        os.close(pfd)


def make_private_sibling(out: str) -> str:
    return tempfile.mkdtemp(prefix="." + os.path.basename(out) + ".tmp-", dir=os.path.dirname(out))


# ---------------------------------------------------------------------------- SHA256SUMS / roots

def parse_sums(data: bytes) -> dict[str, str]:
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        raise ReleaseError("SHA256SUMS is not UTF-8") from None
    if text and not text.endswith("\n"):
        raise ReleaseError("SHA256SUMS does not end with a newline")
    if "\r" in text:
        raise ReleaseError("SHA256SUMS contains a carriage return")
    out: dict[str, str] = {}
    for i, line in enumerate(text.split("\n")[:-1], 1):
        h, sep, p = line.partition("  ")
        if not sep or not HEX64.match(h):
            raise ReleaseError(f"SHA256SUMS line {i}: expected '<64 lowercase hex>  <path>'")
        check_rel_path(p)
        if p == SUMS_NAME:
            raise ReleaseError("SHA256SUMS lists itself")
        if p in out:
            raise ReleaseError(f"SHA256SUMS line {i}: duplicate path {p!r}")
        out[p] = h
    return out


def classify(site: str, sums: dict[str, str]) -> tuple[list[str], list[str]]:
    prefixes = SITE_PREFIXES[site]
    payload, metadata = [], []
    for p in sums:
        if p.startswith(prefixes):
            payload.append(p)
        elif "/" not in p:
            metadata.append(p)
        else:
            raise ReleaseError(f"listed path {p!r} is neither under {list(prefixes)} nor top-level metadata")
    key = lambda s: s.encode("utf-8")  # noqa: E731
    return sorted(payload, key=key), sorted(metadata, key=key)


def walk_payload_tree(root: str, prefixes: tuple[str, ...]) -> list[str]:
    """Every entry under the payload prefixes; symlinks or non-regular files are errors."""
    found = []
    for pre in prefixes:
        top = os.path.join(root, pre.rstrip("/"))
        if not os.path.lexists(top):
            continue
        st = os.lstat(top)
        if stat.S_ISLNK(st.st_mode) or not stat.S_ISDIR(st.st_mode):
            raise ReleaseError(f"payload prefix {pre!r} is not a real directory")
        for dp, dns, fns in os.walk(top, followlinks=False):
            for d in dns:
                if os.path.islink(os.path.join(dp, d)):
                    raise ReleaseError(f"symlink in payload tree: {os.path.relpath(os.path.join(dp, d), root)}")
            for f in fns:
                full = os.path.join(dp, f)
                rel = os.path.relpath(full, root).replace(os.sep, "/")
                m = os.lstat(full).st_mode
                if stat.S_ISLNK(m):
                    raise ReleaseError(f"symlink in payload tree: {rel}")
                if not stat.S_ISREG(m):
                    raise ReleaseError(f"non-regular file in payload tree: {rel}")
                found.append(rel)
    return found


def payload_list_sha256(entries: list[tuple[str, str, int]]) -> str:
    """Digest of the sorted payload list: '<sha256>  <bytes>  <path>' lines."""
    h = hashlib.sha256()
    for p, s, n in sorted(entries, key=lambda e: e[0].encode("utf-8")):
        h.update(f"{s}  {n}  {p}\n".encode("utf-8"))
    return h.hexdigest()


def inspect_root(root: str, site: str) -> dict:
    """Verify a dataset root read-only. Returns the facts the builder and verifier need."""
    if site not in SITE_PREFIXES:
        raise ReleaseError(f"unknown site {site!r}")
    root = os.path.abspath(root)
    sums_bytes = read_regular(root, SUMS_NAME)
    sums = parse_sums(sums_bytes)
    payload, metadata = classify(site, sums)
    on_disk = walk_payload_tree(root, SITE_PREFIXES[site])
    unlisted = sorted(set(on_disk) - set(payload))
    if unlisted:
        raise ReleaseError(f"{len(unlisted)} unlisted payload file(s), e.g. {unlisted[:3]}")
    facts = {"payload": [], "metadata": []}
    for kind, paths in (("metadata", metadata), ("payload", payload)):
        for p in paths:
            fd = open_regular(root, p)
            try:
                got, n = hash_fd(fd)
            finally:
                os.close(fd)
            if got != sums[p]:
                raise ReleaseError(f"{p}: SHA-256 {got} != SHA256SUMS {sums[p]}")
            facts[kind].append((p, got, n))
    by_prefix = {}
    for pre in SITE_PREFIXES[site]:
        sel = [e for e in facts["payload"] if e[0].startswith(pre)]
        by_prefix[pre] = {"count": len(sel), "bytes": sum(e[2] for e in sel)}
    facts.update({
        "root": root, "site": site, "sums_bytes": sums_bytes, "sums": sums,
        "sums_sha256": hashlib.sha256(sums_bytes).hexdigest(),
        "payload_count": len(facts["payload"]), "payload_bytes": sum(e[2] for e in facts["payload"]),
        "payload_list_sha256": payload_list_sha256(facts["payload"]), "by_prefix": by_prefix,
    })
    return facts


# ---------------------------------------------------------------------------- build

class PartWriter(io.RawIOBase):
    """Splits one byte stream into sequential part files, each strictly smaller than max_bytes."""

    def __init__(self, dir_fd: int, site: str, max_bytes: int, edition: str | None = None):
        super().__init__()
        self.dir_fd, self.site, self.max, self.edition = dir_fd, site, max_bytes, edition
        self.parts: list[dict] = []
        self.fd = None
        self.cur = 0
        self.part_hash = None
        self.stream_hash = hashlib.sha256()
        self.stream_bytes = 0

    def writable(self):
        return True

    def _rotate(self):
        self._close_part()
        if len(self.parts) >= MAX_PARTS:
            raise ReleaseError(f"more than {MAX_PARTS} parts; raise --max-part-bytes")
        name = part_name(self.site, len(self.parts), self.edition)
        self.fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o644, dir_fd=self.dir_fd)
        self.parts.append({"name": name, "bytes": 0, "sha256": None})
        self.cur = 0
        self.part_hash = hashlib.sha256()

    def _close_part(self):
        if self.fd is not None:
            os.fsync(self.fd)
            os.close(self.fd)
            self.parts[-1]["bytes"] = self.cur
            self.parts[-1]["sha256"] = self.part_hash.hexdigest()
            self.fd = None

    def write(self, b):
        data = bytes(b)
        n = len(data)
        while data:
            if self.fd is None or self.cur >= self.max - 1:
                self._rotate()
            room = self.max - 1 - self.cur
            chunk, data = data[:room], data[room:]
            write_all(self.fd, chunk)
            self.part_hash.update(chunk)
            self.stream_hash.update(chunk)
            self.cur += len(chunk)
            self.stream_bytes += len(chunk)
        return n

    def finish(self):
        self._close_part()


def check_edition(tag) -> str:
    """The edition tag of a v2 release, refused unless it is exactly the narrow pattern and names a real
    calendar edition (ADR 0100): year 0001 or later, month 01-12, then NN, the edition number 01-99 within
    that month. NN counts editions, it is not a day of the month, so v2026.02.30 (the 30th February
    edition) is valid while v2026.13.01, v2026.00.01 and v2026.11.00 are not."""
    if not isinstance(tag, str) or not EDITION_RE.match(tag):
        raise ReleaseError(f"edition tag {tag!r} is not vYYYY.MM.NN with an optional lowercase -variant")
    year, month, number = (int(x) for x in tag[1:11].split("."))
    if year < 1 or not 1 <= month <= 12 or number < 1:
        raise ReleaseError(f"edition tag {tag!r} does not name a real calendar edition "
                           "(year 0001 or later, month 01-12, edition number 01-99)")
    return tag


def manifest_name(site: str, edition: str | None = None) -> str:
    """The manifest's asset name: `release.json` for v1, `<site>-<edition>-release.json` for v2."""
    return MANIFEST_NAME if edition is None else f"{site}-{check_edition(edition)}-release.json"


def part_name(site: str, i: int, edition: str | None = None) -> str:
    if edition is None:
        return f"{site}-payload.tar.gz.part{i:04d}"
    return f"{site}-{check_edition(edition)}-payload.tar.gz.part{i:04d}"


def manifest_file(m: dict) -> str:
    """The asset name a loaded manifest must be stored under."""
    return MANIFEST_NAME if m.get("schema") == SCHEMA else m["manifest"]


def find_manifest(release: str) -> str:
    """The one manifest in a release directory: `release.json` (v1) or `<site>-<edition>-release.json`
    (v2). None or more than one is refused, so a folder mixing editions cannot be verified by accident."""
    found = sorted(n for n in os.listdir(release) if n == MANIFEST_NAME or V2_MANIFEST_RE.match(n))
    if len(found) != 1:
        raise ReleaseError(f"release directory must hold exactly one manifest, found {found[:4]}")
    return found[0]


class HashingReader(io.RawIOBase):
    def __init__(self, fd: int):
        super().__init__()
        self.fd, self.h, self.n = fd, hashlib.sha256(), 0

    def readable(self):
        return True

    def readinto(self, b):
        data = os.read(self.fd, len(b))
        b[:len(data)] = data
        self.h.update(data)
        self.n += len(data)
        return len(data)


def _tarinfo(path: str, size: int) -> tarfile.TarInfo:
    ti = tarfile.TarInfo(path)
    ti.size, ti.mtime, ti.mode, ti.type = size, 0, 0o644, tarfile.REGTYPE
    ti.uid = ti.gid = 0
    ti.uname = ti.gname = ""
    return ti


def build(root: str, site: str, output: str, max_part_bytes: int = DEFAULT_MAX_PART_BYTES,
          edition: str | None = None) -> dict:
    """Build a release. With `edition` (every new edition) the result is v2, every asset named
    `<site>-<edition>-...`; without it, the legacy v1 layout, byte-identical to earlier builds."""
    if edition is not None:
        check_edition(edition)
    if not (MIN_MAX_PART_BYTES <= max_part_bytes <= HARD_MAX_PART_BYTES):
        raise ReleaseError(f"--max-part-bytes must be in [{MIN_MAX_PART_BYTES}, {HARD_MAX_PART_BYTES}]")
    output = check_new_output(output, root)
    facts = inspect_root(root, site)
    tmp = make_private_sibling(output)
    try:
        dfd = _open_dir_nofollow(tmp)
        try:
            sink = PartWriter(dfd, site, max_part_bytes, edition)
            gz = gzip.GzipFile(filename="", mode="wb", compresslevel=GZIP_LEVEL, fileobj=sink, mtime=0)
            tf = tarfile.open(fileobj=gz, mode="w|", format=tarfile.PAX_FORMAT, encoding="utf-8")
            for p, want, n in facts["payload"]:
                fd = open_regular(facts["root"], p)
                try:
                    if os.fstat(fd).st_size != n:
                        raise ReleaseError(f"{p}: size changed since inspection")
                    rd = HashingReader(fd)
                    tf.addfile(_tarinfo(p, n), io.BufferedReader(rd, CHUNK))
                finally:
                    os.close(fd)
                if rd.n != n or rd.h.hexdigest() != want:
                    raise ReleaseError(f"{p}: archived bytes do not match SHA256SUMS")
            tf.close()
            gz.close()
            sink.finish()
            manifest = {
                "schema": SCHEMA, "tool": "seed/scripts/release_dataset.py", "tool_version": TOOL_VERSION,
                "notice": NOTICE, "site": site, "payload_prefixes": list(SITE_PREFIXES[site]),
                "sha256sums": {"sha256": facts["sums_sha256"], "bytes": len(facts["sums_bytes"]),
                               "entries": len(facts["sums"])},
                "metadata": [{"path": p, "sha256": s, "bytes": b} for p, s, b in facts["metadata"]],
                "payload": {"count": facts["payload_count"], "bytes": facts["payload_bytes"],
                            "list_sha256": facts["payload_list_sha256"],
                            "list_format": "sha256 of sorted '<sha256>  <bytes>  <path>\\n' lines",
                            "by_prefix": facts["by_prefix"]},
                "archive": {"format": "tar (POSIX pax) in gzip, split into sequential parts",
                            "member_order": "UTF-8 byte order of path; payload files only, no directories",
                            "tar_member": {"mtime": 0, "mode": "0644", "uid": 0, "gid": 0, "uname": "", "gname": ""},
                            "gzip": {"mtime": 0, "filename": "", "compresslevel": GZIP_LEVEL},
                            "toolchain": {"python": sys.version.split()[0], "zlib": zlib.ZLIB_RUNTIME_VERSION}},
                "max_part_bytes": max_part_bytes,
                "parts": sink.parts,
                "stream": {"bytes": sink.stream_bytes, "sha256": sink.stream_hash.hexdigest()},
            }
            if edition is not None:
                manifest.update({"schema": SCHEMA_V2, "tool_version": 2, "edition": edition,
                                 "manifest": manifest_name(site, edition)})
            blob = (json.dumps(manifest, indent=1, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")
            mfd = os.open(manifest_name(site, edition), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                          0o644, dir_fd=dfd)
            try:
                write_all(mfd, blob)
                os.fsync(mfd)
            finally:
                os.close(mfd)
            os.fsync(dfd)
        finally:
            os.close(dfd)
        os.chmod(tmp, 0o755)
        install_noreplace(tmp, output)
    except BaseException:
        shutil.rmtree(tmp, ignore_errors=True)
        raise
    return manifest


# ---------------------------------------------------------------------------- verify

class PartReader(io.RawIOBase):
    """Reads the parts in order, re-hashing each part and the whole stream as it goes."""

    def __init__(self, release: str, parts: list[dict]):
        super().__init__()
        self.release, self.parts = release, parts
        self.i, self.fd, self.ph = -1, None, None
        self.part_seen: list[tuple[str, int]] = []
        self.sh, self.total, self.pn = hashlib.sha256(), 0, 0

    def readable(self):
        return True

    def _next(self):
        if self.fd is not None:
            os.close(self.fd)
            self.part_seen.append((self.ph.hexdigest(), self.pn))
            self.fd = None
        self.i += 1
        if self.i < len(self.parts):
            self.fd = open_regular(self.release, self.parts[self.i]["name"])
            self.ph, self.pn = hashlib.sha256(), 0

    def readinto(self, b):
        while True:
            if self.fd is None:
                if self.i >= len(self.parts):
                    return 0
                self._next()
                if self.fd is None:
                    return 0
            data = os.read(self.fd, len(b))
            if data:
                b[:len(data)] = data
                self.ph.update(data)
                self.sh.update(data)
                self.pn += len(data)
                self.total += len(data)
                return len(data)
            self._next()

    def drain(self):
        """Read to the end of the last part so every part's hash is final."""
        buf = bytearray(CHUNK)
        while self.readinto(buf):
            pass


def _req(d, key, typ):
    if key not in d or not isinstance(d[key], typ) or isinstance(d[key], bool) and typ is int:
        raise ReleaseError(f"release.json: missing or bad {key!r}")
    return d[key]


def load_manifest(release: str, expect_sha: str | None, edition: str | None = None) -> tuple[dict, str]:
    """Load and check the release directory's one manifest (see find_manifest).

    The pinned SHA-256 is checked on the raw bytes before parsing. A v1 manifest must be named
    `release.json` and carry no edition. A v2 manifest must be named `<site>-<edition>-release.json`,
    record that same name and edition, and name every part `<site>-<edition>-payload...`. With
    `edition` (the expected tag) the manifest MUST be v2 and of exactly that edition."""
    if edition is not None:
        check_edition(edition)
    name = find_manifest(release)
    blob = read_regular(release, name)
    got = hashlib.sha256(blob).hexdigest()
    if expect_sha is not None and got != expect_sha:
        raise ReleaseError(f"{name} SHA-256 {got} != expected {expect_sha}")
    try:
        m = json.loads(blob.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as e:
        raise ReleaseError(f"{name} is not valid JSON: {e}") from None
    if not isinstance(m, dict) or m.get("schema") not in (SCHEMA, SCHEMA_V2):
        raise ReleaseError(f"{name} schema is not {SCHEMA!r} or {SCHEMA_V2!r}")
    site = _req(m, "site", str)
    if site not in SITE_PREFIXES or m.get("payload_prefixes") != list(SITE_PREFIXES[site]):
        raise ReleaseError("release.json site / payload_prefixes do not match this tool")
    if m["schema"] == SCHEMA:
        if name != MANIFEST_NAME or "edition" in m or "manifest" in m:
            raise ReleaseError(f"a v1 manifest must be {MANIFEST_NAME!r} with no edition (found {name!r})")
        if edition is not None:
            raise ReleaseError(f"expected edition {edition!r}, but this is an unversioned v1 release")
        ed = None
    else:
        ed = check_edition(_req(m, "edition", str))
        if m.get("manifest") != manifest_name(site, ed) or name != manifest_name(site, ed):
            raise ReleaseError(f"manifest {name!r} does not match its recorded site/edition {site!r}/{ed!r}")
        if edition is not None and ed != edition:
            raise ReleaseError(f"release edition {ed!r} != expected tag {edition!r}")
    mx = _req(m, "max_part_bytes", int)
    if not (MIN_MAX_PART_BYTES <= mx <= HARD_MAX_PART_BYTES):
        raise ReleaseError("release.json max_part_bytes out of range")
    parts = _req(m, "parts", list)
    if not parts or len(parts) > MAX_PARTS:
        raise ReleaseError("release.json has no parts or too many")
    total = 0
    for i, p in enumerate(parts):
        if not isinstance(p, dict) or p.get("name") != part_name(site, i, ed):
            raise ReleaseError(f"release.json part {i} is missing, renamed or out of order")
        n, s = _req(p, "bytes", int), _req(p, "sha256", str)
        if not HEX64.match(s) or not (0 < n < mx):
            raise ReleaseError(f"release.json part {p['name']} has a bad size or hash")
        total += n
    st = _req(m, "stream", dict)
    if _req(st, "bytes", int) != total or not HEX64.match(_req(st, "sha256", str)):
        raise ReleaseError("release.json stream size/hash inconsistent with parts")
    pl = _req(m, "payload", dict)
    for k in ("count", "bytes"):
        _req(pl, k, int)
    if not HEX64.match(_req(pl, "list_sha256", str)):
        raise ReleaseError("release.json payload.list_sha256 malformed")
    ss = _req(m, "sha256sums", dict)
    if not HEX64.match(_req(ss, "sha256", str)):
        raise ReleaseError("release.json sha256sums.sha256 malformed")
    _req(m, "metadata", list)
    return m, got


def verify(root: str, release: str, output: str, expect_manifest_sha256: str | None = None,
           edition: str | None = None) -> dict:
    root, release = os.path.abspath(root), os.path.abspath(release)
    output = check_new_output(output, root, release)
    m, msha = load_manifest(release, expect_manifest_sha256, edition)
    site = m["site"]
    # metadata-only root: pinned SHA256SUMS and metadata files
    sums_bytes = read_regular(root, SUMS_NAME)
    if hashlib.sha256(sums_bytes).hexdigest() != m["sha256sums"]["sha256"]:
        raise ReleaseError("metadata root SHA256SUMS does not match the release pin")
    sums = parse_sums(sums_bytes)
    payload, metadata = classify(site, sums)
    if m["sha256sums"].get("entries") != len(sums) or m["payload"]["count"] != len(payload):
        raise ReleaseError("release.json counts do not match SHA256SUMS")
    want_meta = [(p, sums[p]) for p in metadata]
    got_meta = [(e.get("path"), e.get("sha256")) for e in m["metadata"] if isinstance(e, dict)]
    if want_meta != got_meta:
        raise ReleaseError("release.json metadata list does not match SHA256SUMS")
    meta_bytes = {}
    for p in metadata:
        b = read_regular(root, p)
        if hashlib.sha256(b).hexdigest() != sums[p]:
            raise ReleaseError(f"metadata root {p}: SHA-256 does not match SHA256SUMS")
        meta_bytes[p] = b
    # release directory: exactly release.json + the listed parts, all regular files
    names = {e.name: e for e in os.scandir(release)}
    want_names = {manifest_file(m)} | {p["name"] for p in m["parts"]}
    if set(names) != want_names:
        extra, missing = sorted(set(names) - want_names), sorted(want_names - set(names))
        raise ReleaseError(f"release directory mismatch: extra {extra[:3]}, missing {missing[:3]}")
    # pre-pass: every part and the whole stream against the pins, before any extraction
    sh = hashlib.sha256()
    for p in m["parts"]:
        fd = open_regular(release, p["name"])
        try:
            h, n = hashlib.sha256(), 0
            while True:
                b = os.read(fd, CHUNK)
                if not b:
                    break
                h.update(b)
                sh.update(b)
                n += len(b)
        finally:
            os.close(fd)
        if n != p["bytes"] or h.hexdigest() != p["sha256"]:
            raise ReleaseError(f"part {p['name']} does not match release.json")
    if sh.hexdigest() != m["stream"]["sha256"]:
        raise ReleaseError("concatenated stream does not match release.json")
    # extraction into a private sibling
    expected = {p: sums[p] for p in payload}
    tmp = make_private_sibling(output)
    try:
        rfd = _open_dir_nofollow(tmp)
        try:
            reader = PartReader(release, m["parts"])
            gz = gzip.GzipFile(fileobj=io.BufferedReader(reader, CHUNK), mode="rb")
            seen: dict[str, tuple[str, int]] = {}
            try:
                tf = tarfile.open(fileobj=gz, mode="r|", encoding="utf-8")
                for ti in tf:
                    name = ti.name
                    if not ti.isreg() or ti.issparse():
                        raise ReleaseError(f"tar member {name!r} is not a regular file")
                    check_rel_path(name)
                    if name not in expected:
                        raise ReleaseError(f"tar member {name!r} is not a listed payload file")
                    if name in seen:
                        raise ReleaseError(f"tar member {name!r} appears twice")
                    src = tf.extractfile(ti)
                    ofd = create_file(rfd, name)
                    h, n = hashlib.sha256(), 0
                    try:
                        while True:
                            b = src.read(CHUNK)
                            if not b:
                                break
                            h.update(b)
                            n += len(b)
                            write_all(ofd, b)
                    finally:
                        os.close(ofd)
                    if n != ti.size or h.hexdigest() != expected[name]:
                        raise ReleaseError(f"tar member {name!r} does not match SHA256SUMS")
                    seen[name] = (h.hexdigest(), n)
                tail = gz.read()
                if tail.strip(b"\0"):
                    raise ReleaseError("non-zero data after the end of the tar archive")
            except (tarfile.TarError, EOFError, OSError, zlib.error) as e:
                if isinstance(e, ReleaseError):
                    raise
                raise ReleaseError(f"payload stream is not a valid tar.gz: {e}") from None
            reader.drain()
            for i, (h, n) in enumerate(reader.part_seen):
                if h != m["parts"][i]["sha256"] or n != m["parts"][i]["bytes"]:
                    raise ReleaseError(f"part {m['parts'][i]['name']} changed during extraction")
            if len(reader.part_seen) != len(m["parts"]) or reader.sh.hexdigest() != m["stream"]["sha256"]:
                raise ReleaseError("stream changed during extraction")
            missing = sorted(set(expected) - set(seen))
            if missing:
                raise ReleaseError(f"{len(missing)} payload file(s) missing from the archive, e.g. {missing[:3]}")
            entries = [(p, s, n) for p, (s, n) in seen.items()]
            if (payload_list_sha256(entries) != m["payload"]["list_sha256"]
                    or sum(e[2] for e in entries) != m["payload"]["bytes"]):
                raise ReleaseError("extracted payload list does not match release.json")
            for p, b in meta_bytes.items():
                fd = create_file(rfd, p)
                try:
                    write_all(fd, b)
                finally:
                    os.close(fd)
            fd = create_file(rfd, SUMS_NAME)
            try:
                write_all(fd, sums_bytes)
            finally:
                os.close(fd)
        finally:
            os.close(rfd)
        # final check: the reconstructed root passes its own SHA256SUMS and has nothing else
        final = inspect_root(tmp, site)
        present = set()
        for dp, dns, fns in os.walk(tmp, followlinks=False):
            for f in fns:
                present.add(os.path.relpath(os.path.join(dp, f), tmp).replace(os.sep, "/"))
        if present != set(sums) | {SUMS_NAME}:
            raise ReleaseError("reconstructed root has unexpected or missing files")
        os.chmod(tmp, 0o755)
        install_noreplace(tmp, output)
    except BaseException:
        shutil.rmtree(tmp, ignore_errors=True)
        raise
    return {"site": site, "edition": m.get("edition"), "manifest": manifest_file(m),
            "release_json_sha256": msha, "payload_count": final["payload_count"],
            "payload_bytes": final["payload_bytes"], "metadata_files": len(metadata), "parts": len(m["parts"]),
            "stream_sha256": m["stream"]["sha256"], "output": output}


# ---------------------------------------------------------------------------- CLI

def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    pi = sub.add_parser("inspect")
    pi.add_argument("--root", required=True)
    pi.add_argument("--site", required=True, choices=sorted(SITE_PREFIXES))
    pb = sub.add_parser("build")
    pb.add_argument("--root", required=True)
    pb.add_argument("--site", required=True, choices=sorted(SITE_PREFIXES))
    pb.add_argument("--output", required=True)
    pb.add_argument("--max-part-bytes", type=int, default=DEFAULT_MAX_PART_BYTES)
    fmt = pb.add_mutually_exclusive_group(required=True)
    fmt.add_argument("--tag", help="immutable edition tag; builds the v2 layout (every new edition)")
    fmt.add_argument("--legacy-v1", action="store_true",
                     help="build the unversioned v1 layout, only to reproduce an existing v1 edition")
    pv = sub.add_parser("verify")
    pv.add_argument("--root", required=True)
    pv.add_argument("--release", required=True)
    pv.add_argument("--output", required=True)
    pv.add_argument("--expect-manifest-sha256")
    pv.add_argument("--tag", help="expected edition; requires a v2 release of exactly this edition")
    a = ap.parse_args(argv)
    try:
        if a.cmd == "inspect":
            f = inspect_root(a.root, a.site)
            out = {"site": f["site"], "sha256sums_sha256": f["sums_sha256"], "entries": len(f["sums"]),
                   "metadata_files": len(f["metadata"]), "payload_count": f["payload_count"],
                   "payload_bytes": f["payload_bytes"], "payload_list_sha256": f["payload_list_sha256"],
                   "by_prefix": f["by_prefix"]}
        elif a.cmd == "build":
            m = build(a.root, a.site, a.output, a.max_part_bytes, None if a.legacy_v1 else a.tag)
            out = {"site": m["site"], "edition": m.get("edition"), "manifest": manifest_file(m),
                   "parts": [(p["name"], p["bytes"]) for p in m["parts"]],
                   "stream": m["stream"], "payload": {k: m["payload"][k] for k in ("count", "bytes", "list_sha256")}}
        else:
            out = verify(a.root, a.release, a.output, a.expect_manifest_sha256, a.tag)
    except (ReleaseError, OSError, tarfile.TarError, EOFError, zlib.error) as e:
        print(f"release_dataset: FAILED: {type(e).__name__}: {e}", file=sys.stderr)
        return 1
    print(json.dumps(out, indent=1, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
