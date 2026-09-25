#!/usr/bin/env python3
"""
Populate a per-site directory in the unraid archive with ONLY the assets
referenced by a studio's profile JSON, organized by asset type under
typed-folder layout. Filtered + path-rewritten metadata.csv ships alongside.

Layout produced under <dest>:

    <dest>/
    ├── images/<original-pack-path>/...
    ├── audio/<original-pack-path>/...
    ├── 3d/<original-pack-path>/...
    ├── video/<original-pack-path>/...
    ├── documents/<original-pack-path>/...
    ├── fonts/<original-pack-path>/...
    ├── comics/<original-pack-path>/...
    ├── <type>/internet/<filename>    (internet-fetched content)
    ├── metadata.csv                  filtered + file_path rewritten
    ├── groups.csv                    filtered to groups touching site rows
    └── MANIFEST.json                 copy of studio-X.assets.json

Each profile record carries:
  - source_root: which source root to copy from —
        'local'          the source dataset tree (--local-source)
        'internet'       the fetched cache (--internet-source)
        'hq'             the kenney-hq pool (--hq-source), built by
                         kenney_hq.py from the CC0 Kenney pack (#604)
        'torrent_import' / 'site'   pre-staged AT the destination; the
                         copier verifies presence instead of copying,
                         because there is no LOCAL source. When such a
                         record is missing and carries a
                         `metadata.media_url`, it is downloaded instead
                         of reported missing (#602) — see RE-FETCH below.
  - source_path: relative path under that root
  - file_path:   destination path relative to <dest>

RE-FETCH (#602)
---------------
A pre-staged record used to be a dead end on a machine without the
archive share: verify-don't-copy could only report MISSING. The Pexels
videos now carry `metadata.media_url` — the direct CDN URL of the exact
bytes, size-matched against `file_size_bytes` when it was recorded — so
a missing one is downloaded rather than lost, and a from-scratch rebuild
no longer needs the share for them.

Re-fetch is attempted ONLY for records that would otherwise be reported
missing, so a normal run over a populated share does no network I/O at
all. `--no-refetch` restores the old verify-only behaviour. A download
whose length disagrees with the manifest is discarded, not written: a
wrong file staged silently is worse than a missing one reported loudly.

PRESERVED-ROOT MODE (#1319)
---------------------------
⛔ THE SOURCE DATASET IS GONE. `/mnt/d/Projects/unraid_management/
artist-alley_dataset` has been permanently retired, and the maintained
datasets are the published trees under `/mnt/blackbox_archives/datasets/
artist_alley`. For `local` the archive is the ONLY copy: 0 of 696 site_a
and 0 of 552 site_b `local` records carry a `metadata.media_url` and 0
carry a `metadata.source_archive`, so there is nothing to re-derive them
from. `--preserved-roots` says so explicitly, and then:

  * `local` joins the pre-staged roots: its bytes are VERIFIED where they
    sit and never copied from a source. `--local-source` is refused as
    meaningless.
  * `metadata.csv` is NOT regenerated. The old regeneration kept a row
    only when its `file_path` was in the profile's SOURCE-path map, and
    the published column already holds DESTINATION paths: measured, it
    matched 0 of 907 site_a rows and 0 of 1,206 site_b rows and wrote a
    HEADER-ONLY file. It now changes only the way the `--csv-transform`
    document, written BEFORE the run, says it may.
  * `groups.csv` is left untouched and is preservation-owned.
  * A `preserved_archive` retirement is authenticated against
    `--frozen-snapshot`, whose hashes are RECOMPUTED against
    `--snapshot-manifest` immediately beforehand. ⛔ Never the live tree
    and never the staging copy. Those are THREE distinct trees, so
    `--live-site` is REQUIRED in preserved mode: comparing the snapshot
    against `--dest` alone proves only that it is not the tree being written,
    and leaves the live site acceptable as the snapshot while `--dest` points
    at staging. Live is not frozen.
  * The `--csv-transform` document's removals are proved, before any write,
    to be EXACTLY the current collapse document's retirements intersected
    with the rows the frozen pre-operation CSV holds. A transform's own
    before-and-after arithmetic is self-consistent by construction and is
    not authority for which rows may go.

⛔ THE MODE IS NEVER INFERRED. Omitting `--local-source` without
`--preserved-roots` is still an error, because a fallback cannot tell a decision
from a typo.

Usage
-----
    # non-preserved, against a real source dataset
    python3 populate_archive.py \\
        --local-source <source-dataset-root> \\
        --internet-source seed/internet-fetched \\
        --profile seed/profiles/studio-a.assets.json \\
        --dest /mnt/blackbox_archives/datasets/artist_alley/site_a

    # preserved: the archive is the maintained dataset for `local`
    python3 populate_archive.py --preserved-roots \\
        --internet-source seed/internet-fetched \\
        --hq-source <pool> --pack-source <kenney-bundle> \\
        --profile seed/profiles/studio-a.assets.json \\
        --posts   seed/profiles/studio-a.posts.json \\
        --csv-transform $EVIDENCE/site_a.csv-transform.json \\
        --live-site $LIVE/site_a \\
        --frozen-snapshot $FROZEN/site_a \\
        --snapshot-manifest $EVIDENCE/site_a.snapshot-manifest.json \\
        --dest $STAGING/site_a

Idempotent: files already present at the destination with matching size
are skipped. Use --prune to delete files at the destination that aren't
in the profile (useful when regenerating).
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import io
import json
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import urllib.request
import zipfile
from pathlib import Path

import asset_collapse as ac
import manifest_guard as mg
import preserved_archive as pa

# Source roots whose bytes already sit at the destination. The copier
# verifies these instead of copying them — there is no LOCAL source to
# copy FROM. See the handling in the copy loop for what each means.
PRESTAGED_ROOTS = frozenset({"torrent_import", "site"})

# ⛔ PRESERVED-ROOT MODE IS EXPLICIT AND IS NEVER AN IMPLICIT FALLBACK
# (#1319). `local` joins the pre-staged roots ONLY when `--preserved-roots`
# is passed. A missing `--local-source` without the flag is still an error:
# the difference between "the operator has decided the archive is the
# maintained copy" and "the operator forgot an argument" is exactly the
# difference between a governed publish and an accident, and a fallback
# cannot tell them apart.
PRESERVED_ROOTS = pa.PRESERVED_ROOTS

UA = ("artist-alley-seed-fetcher/2.0 "
      "(+https://github.com/Artist-Alley-Org/artist-alley)")

# Downloaded pack zips, keyed by URL, for the archive-member re-fetch
# below. One pack holds hundreds of records; without this a rebuild would
# download the same 15 MB zip once per file.
_ZIP_CACHE: dict[str, "zipfile.ZipFile"] = {}


def refetch_member(url: str, member: str, expect_sha256: str,
                   dest: Path) -> tuple[bool, str]:
    """Extract one file from a remote zip into `dest` (#572).

    The Kenney half of the library has no per-file URL: the bundle it
    comes from is a paid download, and the free per-pack zips serve the
    whole pack. So `media_url`'s contract — a URL serving exactly
    `file_size_bytes` — cannot apply, and the record instead names the
    zip, the member inside it, and that member's sha256. The hash does
    the job the byte count does for a direct URL: it is what makes the
    provenance evidence rather than a plausible-looking string, and it is
    checked BEFORE the bytes are moved into place, so a pack that changed
    upstream fails loudly instead of staging different art under an
    unchanged manifest entry.
    """
    z = _ZIP_CACHE.get(url)
    if z is None:
        req = urllib.request.Request(url, headers={"User-Agent": UA})
        try:
            with urllib.request.urlopen(req, timeout=600) as resp:
                blob = resp.read()
        except Exception as e:
            return False, f"zip download failed: {e}"
        try:
            z = zipfile.ZipFile(io.BytesIO(blob))
        except zipfile.BadZipFile as e:
            return False, f"not a zip: {e}"
        _ZIP_CACHE[url] = z
    try:
        data = z.read(member)
    except KeyError:
        return False, f"member not in zip: {member}"
    got = hashlib.sha256(data).hexdigest()
    if got != expect_sha256:
        return False, (f"sha256 mismatch for {member}: recorded "
                       f"{expect_sha256[:12]}…, served {got[:12]}… — the "
                       "pack changed upstream; re-run kenney_pack_sources.py "
                       "resolve --force and re-emit")
    safe_mkdir(dest.parent)
    tmp = dest.with_suffix(dest.suffix + ".part")
    tmp.write_bytes(data)
    tmp.replace(dest)
    return True, f"{len(data):,} B from {url.rsplit('/', 1)[-1]}"


def refetch(url: str, dest: Path, expect_size: int | None,
            expect_sha256: str | None = None) -> tuple[bool, str]:
    """Download `url` to `dest`. Returns (ok, note).

    Written to a .part file and only moved into place once it agrees with
    the manifest. A short or substituted download that landed at the real
    path would look pre-staged on the next run and never be noticed — the
    whole point of recording a byte count alongside the URL is to make
    that impossible.

    ⛔ `expect_size` IS THE SHIPPED BYTE COUNT, NOT THE ORIGIN'S (#1301).
    This is the check that let the hazard through. Eleven video records
    carried the origin's length while the dataset shipped a two-minute
    cut, so a re-fetch downloaded the 1.1 GB ORIGINAL, compared it
    against the origin's own number, agreed with itself and staged it.
    The dataset would have silently gained a file it does not publish and
    the run would have printed REFETCHED. `file_size_bytes` now means the
    shipped bytes everywhere (`measure_staged.py`), which turns this same
    comparison from a rubber stamp into the refusal it was written to be.

    `expect_sha256` is the stronger form and is used whenever the record
    carries one. A length is a weak oracle — two different cuts of the
    same film can share a byte count, and three of ours do across the two
    sites — so a hash is what actually distinguishes the bytes we publish
    from bytes that merely measure the same.
    """
    tmp = dest.with_suffix(dest.suffix + ".part")
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            safe_mkdir(dest.parent)
            with tmp.open("wb") as f:
                shutil.copyfileobj(resp, f, length=1 << 20)
    except Exception as e:
        tmp.unlink(missing_ok=True)
        return False, f"download failed: {e}"
    got = tmp.stat().st_size
    if expect_size and got != expect_size:
        tmp.unlink(missing_ok=True)
        return False, (f"size mismatch: manifest ships {expect_size:,} B, "
                       f"the URL served {got:,} B — refusing to stage it")
    if expect_sha256:
        digest = sha256_of(tmp)
        if digest != expect_sha256:
            tmp.unlink(missing_ok=True)
            return False, (f"sha256 mismatch: manifest ships "
                           f"{expect_sha256[:12]}…, the URL served "
                           f"{digest[:12]}… — refusing to stage it")
    tmp.replace(dest)
    return True, f"{got:,} B"


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(8 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def safe_mkdir(path: Path, max_retries: int = 3) -> None:
    """Robust mkdir for SMB / network mounts. Python's pathlib.mkdir
    sometimes raises FileExistsError with exist_ok=True when the mount
    returns weird errnos. Tries multiple strategies + a brief retry
    loop to let SMB caches settle. Raises RuntimeError if the directory
    still isn't usable after all attempts."""
    import time
    for attempt in range(max_retries):
        # Strategy 1: pathlib mkdir
        try:
            path.mkdir(parents=True, exist_ok=True)
        except (FileExistsError, OSError):
            pass
        # Strategy 2: shell mkdir -p (more forgiving on SMB)
        try:
            subprocess.run(["mkdir", "-p", str(path)], check=False,
                           timeout=10, capture_output=True)
        except Exception:
            pass
        # Probe: is the directory actually usable now?
        if path.is_dir():
            return
        # Wait briefly for SMB cache to sync, then retry
        time.sleep(0.5 + attempt * 0.5)
    # Final failure
    raise RuntimeError(f"could not create directory after {max_retries} attempts: {path}")


def _clean_companion_uri(uri: str) -> str | None:
    """Normalise a declared URI to a safe relative companion path, or
    None when it needs no companion (embedded data:, remote, absolute)
    or would escape the model directory. Mirrors the Go
    format3d.cleanCompanionURI used by the seed runner (#486)."""
    from urllib.parse import unquote

    uri = (uri or "").strip()
    if not uri:
        return None
    low = uri.lower()
    if low.startswith("data:") or "://" in uri:
        return None
    uri = unquote(uri).replace("\\", "/")
    if uri.startswith("/"):
        return None
    # Windows drive-absolute (`C:\Users\...\tex.png`, which FBX FileName
    # properties routinely carry) names a file on the exporter's machine,
    # not a sibling of the model — and after the separator swap above it
    # would otherwise pass as an ordinary relative path (#753).
    if len(uri) >= 2 and uri[1] == ":" and uri[0].isascii() and uri[0].isalpha():
        return None
    cleaned = os.path.normpath(uri).replace("\\", "/")
    if cleaned in (".", "..") or cleaned.startswith("../"):
        return None
    return cleaned


def _glb_json_chunk(model_path: Path) -> dict | None:
    """Return the glTF JSON document inside a GLB container, or None if
    the file isn't a readable GLB (#750).

    GLB layout (glTF 2.0 §4.4, little-endian): a 12-byte header — magic
    'glTF', version, total length — then length-prefixed chunks, the
    first of which the spec requires to be the JSON chunk. Only that
    chunk is read; the BIN chunk after it is the bulk of the file. Python
    twin of format3d.ReadGLBJSONChunk."""
    try:
        with model_path.open("rb") as f:
            head = f.read(12)
            if len(head) < 12:
                return None
            magic, _version, _total = struct.unpack("<III", head)
            if magic != 0x46546C67:  # 'glTF'
                return None
            chunk = f.read(8)
            if len(chunk) < 8:
                return None
            clen, ctype = struct.unpack("<II", chunk)
            if ctype != 0x4E4F534A:  # 'JSON'
                return None
            if clen > 64 * 1024 * 1024:
                return None
            raw = f.read(clen)
            if len(raw) < clen:
                return None
        doc = json.loads(raw.decode("utf-8"))
    except (OSError, struct.error, UnicodeDecodeError, json.JSONDecodeError):
        return None
    return doc if isinstance(doc, dict) else None


# ---------------------------------------------------------------- FBX (#753)
#
# Python twin of app/internal/preview/format3d/fbx.go. An FBX is a tree of
# node records; the media a model uses is named in Video / Texture nodes'
# RelativeFilename + FileName string properties, unless the Video carries
# the bytes inline in a Content property. Both container encodings appear
# in the wild and both are read here — binary record offsets are uint32
# below version 7500 and uint64 from 7500 on, and the catalogue has both.
#
# Not a filename regex, for the reason the Go side isn't: a regex cannot
# tell a texture reference from the Creator string or a bone named
# "hand.L.png".

_FBX_MAGIC = b"Kaydara FBX Binary  \x00"
_FBX_HEADER_LEN = 27
_FBX_WIDE_VERSION = 7500
_FBX_MAX_DEPTH = 32
_FBX_ARRAY_ELEM = {b"f": 4, b"d": 8, b"l": 8, b"i": 4, b"b": 1}
_FBX_SCALAR = {b"C": 1, b"Y": 2, b"I": 4, b"F": 4, b"D": 8, b"L": 8}


class _FbxError(Exception):
    """The container could not be read. Distinct from 'declares nothing'."""


class _FbxMedia:
    __slots__ = ("rel", "abs", "embedded")

    def __init__(self) -> None:
        self.rel = ""
        self.abs = ""
        self.embedded = False


def _fbx_read_prop(buf: memoryview, off: int, want_value: bool):
    """Read one property record. Returns (offset_after, code, str_value,
    payload_len)."""
    code = bytes(buf[off:off + 1])
    off += 1
    if code in _FBX_SCALAR:
        return off + _FBX_SCALAR[code], code, "", 0
    if code in _FBX_ARRAY_ELEM:
        alen, enc, clen = struct.unpack_from("<III", buf, off)
        off += 12
        return off + (clen if enc else alen * _FBX_ARRAY_ELEM[code]), code, "", 0
    if code in (b"S", b"R"):
        (n,) = struct.unpack_from("<I", buf, off)
        off += 4
        value = ""
        if code == b"S" and want_value and n <= 64 * 1024:
            value = bytes(buf[off:off + n]).decode("latin-1")
        return off + n, code, value, n
    raise _FbxError(f"unknown property type {code!r} at {off - 1}")


def _fbx_walk(buf: memoryview, off: int, end: int, wide: bool, depth: int,
              cur: "_FbxMedia | None", acc: list) -> int:
    if depth > _FBX_MAX_DEPTH:
        raise _FbxError("record nesting too deep")
    num_fmt = "<QQQ" if wide else "<III"
    hdr = 25 if wide else 13
    while off + hdr <= end:
        if off + hdr > len(buf):
            raise _FbxError("truncated record header")
        end_off, num_props, prop_len = struct.unpack_from(num_fmt, buf, off)
        name_len = buf[off + hdr - 1]
        p = off + hdr
        if end_off == 0 and num_props == 0 and prop_len == 0 and name_len == 0:
            return off + hdr
        if end_off <= off or end_off > end or end_off > len(buf):
            raise _FbxError(f"record end offset {end_off} out of range at {off}")
        name = bytes(buf[p:p + name_len]).decode("latin-1").lower()
        p += name_len

        props_end = p + prop_len
        if props_end > end_off:
            raise _FbxError(f"property list of {name!r} overruns its record")
        if cur is not None and num_props > 0 and name in (
                "relativefilename", "filename", "content"):
            _, code, value, length = _fbx_read_prop(buf, p, name != "content")
            if name == "relativefilename" and code == b"S":
                cur.rel = value
            elif name == "filename" and code == b"S":
                cur.abs = value
            elif name == "content" and length > 0:
                cur.embedded = True
        p = props_end

        child = cur
        if name in ("video", "texture"):
            child = _FbxMedia()
            acc.append(child)
        if p < end_off:
            _fbx_walk(buf, p, end_off, wide, depth + 1, child, acc)
        off = end_off
    return off


def _fbx_parse_binary(blob: bytes) -> list:
    (version,) = struct.unpack_from("<I", blob, 23)
    acc: list = []
    _fbx_walk(memoryview(blob), _FBX_HEADER_LEN, len(blob),
              version >= _FBX_WIDE_VERSION, 0, None, acc)
    return acc


def _fbx_parse_ascii(text: str) -> list:
    """Brace-depth walk of the text encoding. `FileName` means something
    only inside a Video or Texture block, so the stack matters."""
    stack: list = []          # [(name, media|None)]
    acc: list = []
    saw_marker = False
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith(";"):
            continue
        if not saw_marker and ("FBXHeaderExtension" in line or "FBXVersion" in line):
            saw_marker = True
        while line.startswith("}"):
            if stack:
                stack.pop()
            line = line[1:].strip()
        if not line:
            continue

        key = ""
        colon = line.find(":")
        if colon > 0:
            k = line[:colon].strip()
            if not any(c in k for c in '"{}'):
                key = k.lower()

        if line.endswith("{"):
            media = None
            if key in ("video", "texture"):
                media = _FbxMedia()
                acc.append(media)
            if len(stack) >= _FBX_MAX_DEPTH:
                raise _FbxError("ascii nesting too deep")
            stack.append((key, media))
            continue

        if key and stack and stack[-1][1] is not None:
            cur = stack[-1][1]
            first, last = line.find('"'), line.rfind('"')
            value = line[first + 1:last] if 0 <= first < last else ""
            if key == "relativefilename":
                cur.rel = value
            elif key == "filename":
                cur.abs = value
            elif key == "content":
                rest = line[colon + 1:].strip().strip(", \t")
                if rest and rest != '""':
                    cur.embedded = True

        while line.endswith("}"):
            if stack:
                stack.pop()
            line = line[:-1].strip()

    if not saw_marker:
        raise _FbxError("not an fbx (no binary magic, no FBXHeaderExtension)")
    return acc


def _fbx_companions(model_path: Path) -> list[str]:
    """Relative paths an FBX declares, cleaned and de-duplicated. Raises
    _FbxError when the container cannot be read — which is NOT the same
    answer as 'declares nothing'."""
    try:
        blob = model_path.read_bytes()
    except OSError as e:
        raise _FbxError(str(e)) from e
    try:
        if blob.startswith(_FBX_MAGIC):
            media = _fbx_parse_binary(blob)
        else:
            media = _fbx_parse_ascii(blob.decode("latin-1"))
    except (struct.error, IndexError, UnicodeDecodeError) as e:
        raise _FbxError(str(e)) from e

    # A Video that embeds its bytes and the Texture applying it name the
    # same file; only the Video carries the Content, so the embedded names
    # are excluded everywhere.
    embedded = set()
    for m in media:
        if m.embedded:
            for raw in (m.rel, m.abs):
                if raw:
                    embedded.add(raw.replace("\\", "/").rsplit("/", 1)[-1].lower())

    out: list[str] = []
    seen: set[str] = set()
    for m in media:
        if m.embedded:
            continue
        # Relative first, absolute only as a fallback — what three.js's
        # FBXLoader does (`RelativeFilename || Filename`).
        rel = _clean_companion_uri(m.rel) or _clean_companion_uri(m.abs)
        if not rel:
            continue
        if rel.rsplit("/", 1)[-1].lower() in embedded or rel in seen:
            continue
        seen.add(rel)
        out.append(rel)
    return out


def resolve_model_companions(model_path: Path) -> list[str]:
    """Return the on-disk sibling files a multi-file model declares,
    relative to the model's directory (#486). glTF and GLB → buffers[].uri
    + images[].uri; OBJ → mtllib .mtl files and, recursively, the textures
    each .mtl references. Only siblings that exist next to the model are
    returned. This is the Python twin of
    app/internal/preview/format3d.ResolveCompanions so the seed pipeline
    stages exactly what the Go runner will register + the loaders resolve.

    GLB is NOT self-contained by default (#750). It wraps the same glTF
    JSON document in a binary container, so its buffer/image URIs can name
    external files exactly as a .gltf's can. Treating it as embedded here
    is what left 363 of the catalogue's 374 GLBs staged WITHOUT the
    textures they name — Kenney ships one Textures/ dir per format
    directory, and this function returning [] meant the GLB dirs' copies
    were never copied, so the models rendered grey.

    FBX was the same bug one format over (#753) and is now read the same
    way: a Video node either embeds its image in a Content property or
    names it in RelativeFilename / FileName. 127 of the catalogue's 131
    name a file and 126 resolve to a sibling path (the odd one out names
    only an authoring-machine `C:\\...` path); none embed. This function
    returning [] is why 0 of those 246 references had their file staged.
    Reading them stages all 246, verified against both shares.
    """
    ext = model_path.suffix.lower().lstrip(".")
    base = model_path.parent
    declared: list[str] = []
    seen: set[str] = set()

    def add(rel: str | None) -> None:
        if rel and rel not in seen:
            seen.add(rel)
            declared.append(rel)

    def add_gltf_doc(doc: dict) -> None:
        for b in doc.get("buffers", []) or []:
            add(_clean_companion_uri(b.get("uri", "")))
        for im in doc.get("images", []) or []:
            add(_clean_companion_uri(im.get("uri", "")))

    if ext == "gltf":
        try:
            doc = json.loads(model_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return []
        add_gltf_doc(doc)
    elif ext == "glb":
        doc = _glb_json_chunk(model_path)
        if doc is None:
            return []
        add_gltf_doc(doc)
    elif ext == "fbx":
        try:
            for rel in _fbx_companions(model_path):
                add(rel)
        except _FbxError as e:
            # Soft-fail, matching the Go runner: an unreadable container
            # stages nothing and says so, rather than passing silently as
            # a model with no references.
            print(f"  ! unreadable fbx {model_path.name}: {e}", file=sys.stderr)
            return []
    elif ext == "obj":
        try:
            text = model_path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return []
        map_kw = {
            "map_ka", "map_kd", "map_ks", "map_ke", "map_ns", "map_d",
            "map_bump", "bump", "disp", "decal", "refl", "norm",
            "map_pr", "map_pm", "map_ps",
        }
        for line in text.splitlines():
            parts = line.split()
            if len(parts) >= 2 and parts[0].lower() == "mtllib":
                for lib in parts[1:]:
                    rel = _clean_companion_uri(lib)
                    add(rel)
                    if not rel:
                        continue
                    mtl_path = base / rel
                    try:
                        mtl_text = mtl_path.read_text(encoding="utf-8", errors="replace")
                    except OSError:
                        continue
                    mtl_dir = os.path.dirname(rel)
                    for mline in mtl_text.splitlines():
                        mparts = mline.split()
                        if len(mparts) >= 2 and mparts[0].lower() in map_kw:
                            tex = _clean_companion_uri(mparts[-1])
                            if tex:
                                add(os.path.join(mtl_dir, tex).replace("\\", "/") if mtl_dir else tex)
    else:
        return []

    return [rel for rel in declared if (base / rel).is_file()]


def load_posts_migration(args):
    """The committed migration document for `--posts`, validated (#1319).

    Returns a `manifest_guard.Migration`, None when there is no document
    (no evidence, so the plain comparison runs and every old id is a
    loss), or False after printing a refusal when the document exists
    and cannot be used. False is a refusal, not an absence: an
    unusable document must never quietly become "no migrations".

    The document is located from the posts file alone, so the existing
    `--posts <profile> --dry-run` invocation is sufficient:
    `seed/profiles/<stem>.posts.json` maps to
    `seed/upgrades/post-id-migration.<stem>.json`, and the document's
    own `profile` field must name the posts file. `--migration-document`
    overrides the location (for fixtures laid out elsewhere), never the
    `profile` check.
    """
    override = getattr(args, "migration_document", None)
    doc_path = override if override is not None else mg.migration_document_path(args.posts)
    if doc_path is None:
        print(f"  posts.json: no migration document lookup for {args.posts.name} "
              f"(not named <stem>{mg.POSTS_PROFILE_SUFFIX}); every destination-only "
              f"id counts as a loss", file=sys.stderr)
        return None
    if not doc_path.is_file():
        if override is not None:
            print(f"error: --migration-document {doc_path}: not a file", file=sys.stderr)
            return False
        print(f"  posts.json: no migration document at {doc_path}; every "
              f"destination-only id counts as a loss", file=sys.stderr)
        return None
    try:
        migration = mg.load_migration_document(doc_path, profile_name=args.posts.name)
    except mg.MigrationError as e:
        print(f"error: {e}\n"
              "  Refusing: a migration document that cannot be validated is not "
              "evidence, and \"unusable\" must not be read as \"no migrations\". "
              "This is not overridable by --allow-regression.", file=sys.stderr)
        return False
    print(f"  posts.json: migration document {doc_path} ({len(migration.moves)} "
          f"recorded move(s) for {migration.profile})", file=sys.stderr)
    return migration


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _produced_file(root_name: str, source_path: str, sources: dict[str, Path],
                   rec: dict, args, tmp: Path) -> tuple[Path | None, str]:
    """Locate the PRODUCED file for one record under its own source root.

    Returns (path, note) or (None, reason). The file is the one the
    copier would stage: a kenney-hq pool render, a local file, a bundle
    member. For `pack` the bundle is optional on a given machine, so the
    member is extracted from the pack's own free CC0 zip and verified
    against `metadata.source_archive.sha256` before it is used, which is
    the same evidence rule the copier applies.
    """
    root = sources.get(root_name)
    if root is not None:
        candidate = root / source_path
        if candidate.is_file():
            return candidate, f"{root_name}:{source_path}"
    if root_name != "pack":
        return None, (f"no file at --{root_name}-source/{source_path}"
                      if root is not None else
                      f"--{root_name}-source was not given")
    sa = (rec.get("metadata") or {}).get("source_archive") or {}
    if not sa.get("url") or args.no_refetch:
        return None, ("not under --pack-source and no re-fetchable "
                      "metadata.source_archive")
    dest = tmp / f"{abs(hash(source_path))}-{Path(source_path).name}"
    ok, note = refetch_member(sa["url"], sa["member"], sa.get("sha256", ""), dest)
    if not ok:
        return None, f"pack member could not be authenticated: {note}"
    return dest, f"pack zip member {sa['member']}"


def _preserved_snapshot(args):
    """(snapshot root, manifest) for preserved authentication, or False.

    ⛔ THE SNAPSHOT IS NOT TRUSTED FOR BEING A SNAPSHOT. It is a copy we
    took; the manifest is what we said it was at the moment we took it, and
    it lives outside it. Recomputation happens per authentication, below.
    ⛔ NOT mtime, NOT mode, NOT size: a CIFS tree can change under all
    three, and a copy that agrees with its own metadata is the oldest form
    of this mistake.
    """
    if args.frozen_snapshot is None or args.snapshot_manifest is None:
        print(f"error: a {ac.KIND_PRESERVED} retirement needs both "
              f"--frozen-snapshot and --snapshot-manifest.\n"
              f"  This evidence kind has no external source to re-derive bytes "
              f"from, so the ONLY thing that can authenticate it is a frozen "
              f"pre-operation snapshot whose hashes were recorded immediately "
              f"after it was taken:\n"
              f"    python3 seed/scripts/preserved_archive.py snapshot-manifest "
              f"--snapshot <frozen> --out <outside-it>/<site>.snapshot-manifest.json\n"
              f"  ⛔ The live tree and the staging copy are BOTH refused: a tree "
              f"this run writes to cannot be the evidence for what it writes.",
              file=sys.stderr)
        return False
    snap = args.frozen_snapshot
    if not snap.is_dir():
        print(f"error: --frozen-snapshot not a directory: {snap}", file=sys.stderr)
        return False
    # Belt and braces over main()'s boundary check: this is the one check
    # whose failure would authenticate a retirement against a tree nobody
    # froze, so it is made again at the point of use rather than trusted
    # from earlier. ⛔ AGAINST LIVE AS WELL AS AGAINST --dest: the live site
    # and the staging tree are different trees, and the snapshot must be
    # neither.
    if not pa.refuse_operation(
            live=args.live_site, staging=args.dest, snapshot=snap,
            evidence={"--snapshot-manifest": args.snapshot_manifest,
                      "--csv-transform": args.csv_transform}):
        print("  A preserved retirement authenticated from the live site or "
              "from the tree being published proves nothing at all.",
              file=sys.stderr)
        return False
    try:
        manifest = pa.load_snapshot_manifest(args.snapshot_manifest)
    except pa.PreservedError as e:
        print(f"error: {e}\n"
              "  Refusing: a snapshot manifest that cannot be validated is not "
              "an attestation, and an unattested snapshot authenticates nothing.",
              file=sys.stderr)
        return False
    return snap, manifest


def load_collapse(args):
    """Resolve, read and validate the collapse document ONCE per run.

    Returns `(path, document, raw_bytes)`, `None` when there is no document
    (absence is not permission: every destination-only id then counts as a
    loss), or `False` after printing a refusal.

    ⛔ ONCE, AND SHARED. The CSV transform's authority is bound to a specific
    document, and Layer B authenticates a document; if those were two
    independent reads of the same path, nothing would prove they were the
    same bytes. `raw_bytes` is what the binding digest is taken over, so the
    document the transform claims and the document that authenticates are the
    same object in memory.
    """
    override = getattr(args, "collapse_document", None)
    doc_path = override if override is not None else ac.collapse_document_path(args.profile)
    if doc_path is None:
        print(f"  MANIFEST.json: no collapse document lookup for "
              f"{args.profile.name} (not named <stem>{ac.ASSETS_PROFILE_SUFFIX}); "
              f"every destination-only id counts as a loss", file=sys.stderr)
        return None
    if not doc_path.is_file():
        if override is not None:
            print(f"error: --collapse-document {doc_path}: not a file", file=sys.stderr)
            return False
        print(f"  MANIFEST.json: no collapse document at {doc_path}; every "
              f"destination-only id counts as a loss", file=sys.stderr)
        return None
    try:
        raw = doc_path.read_bytes()
        doc = ac.load_collapse_document(doc_path, profile_name=args.profile.name)
    except (OSError, ac.CollapseError) as e:
        print(f"error: {e}\n"
              "  Refusing: an asset-collapse document that cannot be validated "
              "is not evidence, and \"unusable\" must not be read as \"nothing "
              "was retired\". This is not overridable by --allow-regression.",
              file=sys.stderr)
        return False
    return doc_path, doc, raw


def authenticate_collapses(args, sources: dict[str, Path],
                           profile: list[dict], *, preserved: bool = False,
                           collapse_loaded=None):
    """LAYER B. Source-authenticate every documented retirement, or refuse.

    Returns a mapping retired_id -> {"survivor_id", "retired_record",
    "entry"} for the entries that passed, `{}` when there is no document,
    or False after printing a refusal.

    ⛔ THIS IS THE ONLY PLACE A RETIREMENT IS AUTHENTICATED, AND IT IS
    NOT THE SAME CLAIM LAYER A MAKES. `apply_upgrade.py` proves the
    document is structurally valid and that the repository is in one of
    the two states it describes; it runs where there is no pack, no pool
    and no dataset source, so it has never seen a byte. Here the produced
    files exist, so here is where the load-bearing fact is established:

      * the survivor is in the SOURCE profile and the retired id is not;
      * `acknowledged_losses` recomputes to exactly the recorded list
        against the source's survivor record;
      * both produced files are located under the root their own
        `source_root` names, and they hash IDENTICALLY TO EACH OTHER
        within this one build;
      * that hash equals the document's `materialized_sha256`.

    ⚠️ VERSION DRIFT REFUSES RATHER THAN PASSES. A png is byte
    reproducible only within one sharp build (`kenney_hq.py` documents 24
    site_a files differing in exactly 8 pHYs bytes across versions). The
    equality between the two files is what proves the collision; the
    absolute value is recorded so a toolchain change is visible, and a
    mismatch refuses and names re-measurement. The IDAT-only reading
    appears in the refusal as a diagnostic so an operator can tell a
    metadata-chunk difference from different artwork. It is never an
    acceptance path.

    ⛔ AND IT NEVER THREADS THESE ROOTS BACK INTO `apply_upgrade.py`. The
    assembler calls that script with no source roots and the required
    guard suite runs on a runner that has none, so a byte check there
    would either fail CI or be skipped, and a skipped validation is how a
    waiver appears.
    """
    loaded = collapse_loaded
    if loaded is False:
        return False
    if loaded is None:
        return {}
    doc_path, doc, _raw = loaded

    by_id = {a.get("id"): a for a in profile}
    problems: list[str] = []
    authenticated: dict[str, dict] = {}

    # ⛔ THE SNAPSHOT IS DEMANDED ONLY BY THE ENTRIES THAT NEED IT, AND
    # NEVER OPTIONAL FOR THOSE. A document holding no preserved entry (the
    # committed studio-a one, for instance) authenticates exactly as it did
    # before and asks for nothing new.
    snapshot = manifest = None
    if any(e.is_preserved for e in doc.entries):
        # ⛔ AND THE MODE MUST SAY SO. A preserved retirement is a claim about
        # ARCHIVE-HELD bytes; authenticating one while the same run copies
        # `local` from a source root would be two different authority models
        # in one publish, and the weaker one would be doing the deciding.
        if not preserved:
            print(f"error: this document holds {ac.KIND_PRESERVED} "
                  f"retirement(s), which are only meaningful when `local` is "
                  f"being treated as archive-authoritative. Pass "
                  f"--preserved-roots, or use a document whose entries are all "
                  f"{ac.KIND_PRODUCED}.", file=sys.stderr)
            return False
        got = _preserved_snapshot(args)
        if got is False:
            return False
        snapshot, manifest = got

    tmp = Path(tempfile.mkdtemp(prefix="aa-collapse-"))
    try:
        for e in doc.entries:
            survivor = by_id.get(e.survivor_id)
            if survivor is None:
                problems.append(f"{e.retired_id}: survivor {e.survivor_id} is not "
                                f"in the source profile")
                continue
            if e.retired_id in by_id:
                problems.append(f"{e.retired_id}: still present in the source "
                                f"profile, so the retirement this document "
                                f"records was never applied to it")
                continue
            recomputed = ac.recompute_losses(e.retired_record, survivor)
            if not ac.losses_equal(e.acknowledged_losses, recomputed):
                problems.append(
                    f"{e.retired_id}: acknowledged_losses ({len(e.acknowledged_losses)}) "
                    f"disagrees with the recomputation against the source's "
                    f"survivor ({len(recomputed)})")
                continue
            if survivor.get("source_root") != e.source_root:
                problems.append(
                    f"{e.retired_id}: the survivor's source_root is "
                    f"{survivor.get('source_root')!r}, the document says "
                    f"{e.source_root!r}; the two files would be looked for under "
                    f"different roots and their equality would prove nothing")
                continue
            if survivor.get("owner_username") != e.owner_username:
                problems.append(
                    f"{e.retired_id}: the survivor is owned by "
                    f"{survivor.get('owner_username')!r}, the document says "
                    f"{e.owner_username!r}. Identity is per OWNER: two records "
                    f"with identical bytes and different owners are legal and "
                    f"are not a collision")
                continue
            if e.is_preserved:
                # ── preserved_archive ─────────────────────────────────────
                # ⛔ RECOMPUTE THE SNAPSHOT AGAINST ITS MANIFEST IMMEDIATELY
                # BEFORE USING IT, every run, for exactly the paths this
                # authentication is about to read. Without this the check
                # would be the archive agreeing with itself, and a stale or
                # silently-changed copy does that perfectly. A CIFS tree can
                # change under its own mtimes, so bytes are the only proof.
                sur_rel = str(survivor.get("file_path") or "")
                if not sur_rel:
                    problems.append(f"{e.retired_id}: the survivor record has no "
                                    f"file_path, so there is nothing to locate in "
                                    f"the snapshot")
                    continue
                stale = pa.recompute_snapshot(snapshot, manifest,
                                              (e.retired_file_path, sur_rel))
                if stale:
                    problems.append(
                        f"{e.retired_id}: the frozen snapshot does not match its "
                        f"manifest, so it is not frozen: " + "; ".join(stale))
                    continue
                s_hash = pa.sha256_file(snapshot / sur_rel)
                r_hash = pa.sha256_file(snapshot / e.retired_file_path)
                if s_hash != r_hash:
                    problems.append(
                        f"{e.retired_id}: in the attested snapshot the survivor "
                        f"({sur_rel}, {s_hash[:12]}…) and the retired file "
                        f"({e.retired_file_path}, {r_hash[:12]}…) are DIFFERENT "
                        f"bytes. The whole claim is that they collapse to one row; "
                        f"two different files are two records.")
                    continue
                if r_hash != e.retired_sha256:
                    problems.append(
                        f"{e.retired_id}: the attested snapshot holds {r_hash[:12]}… "
                        f"where the document records retired_sha256 "
                        f"{e.retired_sha256[:12]}…. Re-measure against the snapshot "
                        f"and re-record rather than widening the check.")
                    continue
                authenticated[e.retired_id] = {
                    "survivor_id": e.survivor_id,
                    "retired_record": e.retired_record,
                    "entry": e,
                }
                continue

            # ── produced_source, unchanged ────────────────────────────────
            paths = []
            for label, rec, spath in (
                    ("survivor", survivor, survivor.get("source_path") or ""),
                    ("retired", e.retired_record, e.retired_source_path)):
                p, note = _produced_file(e.source_root, spath, sources, rec, args, tmp)
                if p is None:
                    problems.append(f"{e.retired_id}: the {label} record's produced "
                                    f"file could not be located ({note})")
                    paths = []
                    break
                paths.append((label, p, note))
            if not paths:
                continue
            (_, sp, snote), (_, rp, rnote) = paths
            s_hash, r_hash = sha256_file(sp), sha256_file(rp)
            if s_hash != r_hash:
                problems.append(
                    f"{e.retired_id}: the two produced files are NOT identical in "
                    f"this build (survivor {snote} {s_hash[:12]}…, retired {rnote} "
                    f"{r_hash[:12]}…). The whole claim is that they materialize to "
                    f"one row; without that there is nothing to retire. "
                    f"{_idat_note(sp, rp)}")
                continue
            if s_hash != e.materialized_sha256:
                problems.append(
                    f"{e.retired_id}: the produced files agree with each other "
                    f"({s_hash[:12]}…) but not with the document's "
                    f"materialized_sha256 ({e.materialized_sha256[:12]}…). A png is "
                    f"byte reproducible only within one sharp build, so re-measure "
                    f"and re-record rather than widening the check. "
                    f"{_idat_note(sp, rp)}")
                continue
            authenticated[e.retired_id] = {
                "survivor_id": e.survivor_id,
                "retired_record": e.retired_record,
                "entry": e,
            }
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    if problems:
        print(f"error: {doc_path}: {len(problems)} documented retirement(s) could "
              f"not be source-authenticated:", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        print("  Refusing. Each of these falls back to MISSING_RECORD, which is "
              "what an unauthenticated retirement is: a record the destination "
              "holds and this run would delete.", file=sys.stderr)
        return False

    if doc.entries:
        n_prod = sum(1 for e in doc.entries if not e.is_preserved)
        n_pres = sum(1 for e in doc.entries if e.is_preserved)
        how = []
        if n_prod:
            how.append(f"{n_prod} {ac.KIND_PRODUCED} (produced bytes re-derived "
                       f"under {doc.pool_of_record.get('rasteriser')}, sharp "
                       f"{doc.pool_of_record.get('sharp')})")
        if n_pres:
            # ⛔ NAMED AS THE WEAKER CLAIM IN THE REPORT TOO. A line that
            # said "authenticated" without saying against what would be the
            # Layer-A-wearing-a-publish-tick shape all over again.
            how.append(f"{n_pres} {ac.KIND_PRESERVED} (WEAKER CLAIM: bytes "
                       f"compared against the frozen snapshot "
                       f"{args.frozen_snapshot}, recomputed against "
                       f"{args.snapshot_manifest})")
        print(f"  MANIFEST.json: collapse document {doc_path} "
              f"({len(authenticated)} authenticated retirement(s) for "
              f"{doc.profile}; " + "; ".join(how) + ")", file=sys.stderr)
    return authenticated


def _idat_note(a: Path, b: Path) -> str:
    """A DIAGNOSTIC for a refusal message, never an acceptance path.

    Two pngs whose image data agrees and whose whole-file hashes do not
    differ in a metadata chunk (a pHYs written by another libvips, for
    instance), which is a different problem from two different pictures.
    Saying which one an operator is looking at saves a bisect. It does
    not make the run pass: the caller has already decided to refuse by
    the time this is called.
    """
    try:
        ia, ib = _idat_sha256(a), _idat_sha256(b)
    except (OSError, ValueError) as e:
        return f"(IDAT diagnostic unavailable: {e})"
    if ia is None or ib is None:
        return "(IDAT diagnostic unavailable: not a png)"
    if ia == ib:
        return ("DIAGNOSTIC ONLY: the IDAT streams are identical, so the "
                "difference is in a metadata chunk, not in the picture. This is "
                "not an acceptance path; re-render both under one toolchain and "
                "re-record materialized_sha256.")
    return "DIAGNOSTIC ONLY: the IDAT streams differ too, so these are different images."


def _idat_sha256(path: Path) -> str | None:
    data = path.read_bytes()
    if not data.startswith(b"\x89PNG\r\n\x1a\n"):
        return None
    h = hashlib.sha256()
    i = 8
    while i + 8 <= len(data):
        length = struct.unpack(">I", data[i:i + 4])[0]
        kind = data[i + 4:i + 8]
        if kind == b"IDAT":
            h.update(data[i + 8:i + 8 + length])
        i += 12 + length
        if kind == b"IEND":
            break
    return h.hexdigest()


def remove_retired_paths(args, collapses, wanted_dest_paths: set[str]) -> tuple[int, bool]:
    """Delete exactly the produced files of source-authenticated retirements.

    Returns (removed, ok). `ok` is False after a refusal, and a refusal
    deletes nothing at all: the pass is all or nothing so an operator
    never has to work out which half ran.

    Three conditions, every one of them required, per path:

      1. Layer B authenticated THAT entry. An unauthenticated retirement
         has already refused the run long before this point.
      2. The path belongs to no current record's destination path. A file
         something still wants is not stale, whatever a document says.
      3. The bytes at the path hash to the document's
         `materialized_sha256`. If they do not, the file is not the one
         the document describes and deleting it would destroy something
         nobody enumerated.

    ⛔ THIS IS NOT `--prune`, AND IT DELIBERATELY SHARES NO MACHINERY WITH
    IT. `--prune` walks the whole destination and spares a keep-list of
    exactly `metadata.csv`, `groups.csv` and `MANIFEST.json`, so it would
    also delete `posts.json`, every `.bak`, `ATTRIBUTIONS.md` and
    `dataset-metadata.json`. This pass never walks the tree: it visits
    one named path per authenticated entry and nothing else, so there is
    no breadth for it to be invoked with.
    """
    removed = 0
    refusals: list[str] = []
    planned: list[tuple[Path, str]] = []
    for rid, info in sorted(collapses.items()):
        entry = info["entry"]
        rel = entry.retired_file_path
        if rel in wanted_dest_paths:
            refusals.append(f"{rid}: {rel} is still the destination path of a "
                            f"record in this profile; refusing to remove a file "
                            f"the run itself wants")
            continue
        target = args.dest / rel
        if not target.is_file():
            print(f"  retired path already absent: {rel}", file=sys.stderr)
            continue
        got = sha256_file(target)
        # `retired_bytes_sha256` is `materialized_sha256` for a
        # produced_source entry (both produced files hash to it) and
        # `retired_sha256` for a preserved_archive one. Same rule, and the
        # entry says which field established it rather than this pass
        # guessing.
        if got != entry.retired_bytes_sha256:
            refusals.append(
                f"{rid}: {rel} hashes {got[:12]}…, the document records "
                f"{entry.retired_bytes_sha256[:12]}… ({entry.kind}). The file at "
                f"the retired path "
                f"is not the one this document describes, so removing it would "
                f"destroy bytes nobody enumerated. Re-measure rather than widen "
                f"the rule.")
            continue
        planned.append((target, rel))

    if refusals:
        print(f"error: {len(refusals)} retired path(s) could not be removed:",
              file=sys.stderr)
        for r in refusals:
            print(f"  - {r}", file=sys.stderr)
        print("  Refusing, and nothing was deleted.", file=sys.stderr)
        return 0, False

    for target, rel in planned:
        if args.dry_run:
            print(f"  would remove retired path: {rel}", file=sys.stderr)
        else:
            target.unlink()
            print(f"  removed retired path: {rel}", file=sys.stderr)
        removed += 1
    return removed, True


def check_destination(args, profile: list[dict], collapses=None) -> bool:
    """Compare the profile against what it is about to overwrite (#1275).

    Returns True when the run may proceed. Prints the comparison either
    way — the issue asks for the diff to be REPORTED before any write,
    not only when it refuses, because "nothing would be lost" is the
    fact an operator most needs and least often gets.

    ⚠️ Both files are checked, not just the manifest. `--posts` is copied
    over `<dest>/posts.json` by the same run, and a post that disappears
    from the wall is exactly as published and exactly as silent as an
    asset that disappears from the manifest.
    """
    print("\nchecking destination against the profile (#1275)", file=sys.stderr)
    # (label, source records, destination path, migration document).
    # Only posts carry a migration: asset ids have never moved, and the
    # MANIFEST comparison is byte-for-byte what it was before #1319.
    pairs = [("MANIFEST.json", profile, args.dest / "MANIFEST.json", None)]
    # Only the MANIFEST pair carries retirements: `asset-collapse` is an
    # ASSET document, and the post whose membership it corrects is an
    # ordinary CHANGED_VALUE on the posts side.
    collapse_for = {"MANIFEST.json": collapses or {}}
    if args.posts is not None:
        try:
            posts = json.loads(args.posts.read_text(encoding="utf-8"))
        except (OSError, ValueError) as e:
            print(f"error: --posts {args.posts}: {e}", file=sys.stderr)
            return False
        migration = load_posts_migration(args)
        if migration is False:
            return False
        pairs.append(("posts.json", posts, args.dest / "posts.json", migration))

    verdicts = []
    for label, source, dest_path, migration in pairs:
        try:
            dest = mg.load_json_list(dest_path)
        except ValueError as e:
            # Unreadable is NOT empty. Treating a truncated destination
            # as "nothing to lose" would wave through the one run that
            # most needs stopping.
            print(f"error: {e}\n"
                  "  Refusing: a destination that cannot be read cannot be "
                  "compared, and an uncomparable destination is not a safe "
                  "one to overwrite.", file=sys.stderr)
            return False
        try:
            if dest is None:
                print(f"  {label}: destination does not exist yet: first publish, "
                      "nothing to lose", file=sys.stderr)
                # Still worth reporting duplicates: a first publish can ship
                # a coin toss just as easily as a re-publish can.
                cmp = mg.compare(source, None, label, migration=migration,
                                 collapses=collapse_for.get(label),
                                 collapse_source=str(
                                     getattr(args, "collapse_document", None)
                                     or ac.collapse_document_path(args.profile) or ""))
            else:
                cmp = mg.compare(source, dest, label, migration=migration,
                                 collapses=collapse_for.get(label),
                                 collapse_source=str(
                                     getattr(args, "collapse_document", None)
                                     or ac.collapse_document_path(args.profile) or ""))
                print(mg.format_report(cmp), file=sys.stderr)
        except mg.MigrationError as e:
            # The document disagrees with the profile it claims to
            # describe. Not overridable: half a mapping is a guess, and
            # a guess over published ids is how a wall gets duplicated.
            print(f"error: {e}\n"
                  "  Refusing: the migration document is not evidence for this "
                  "profile. Fix the document (seed/scripts/migrate_post_ids.py "
                  "writes it) rather than the destination.", file=sys.stderr)
            return False
        verdicts.append(cmp)

    dup = [c for c in verdicts if c.duplicates]
    if dup:
        for c in dup:
            print(f"\nerror: {c.label} holds {len(c.duplicates)} id(s) used by more "
                  f"than one record.", file=sys.stderr)
            print(mg.format_report(c), file=sys.stderr)
        print("\n⛔ REFUSING. A manifest cannot represent two records under one "
              "id: `aa seed` keys on the stable id and silently takes whichever "
              "it reads last. This is not overridable by --allow-regression, "
              "because there is no version of it that is the intended change.\n"
              "  Fix: python3 seed/scripts/apply_upgrade.py --site <site> "
              "--profile <profile> --posts <posts>", file=sys.stderr)
        return False

    lost = [c for c in verdicts if c.losses]
    if not lost:
        return True
    if args.allow_regression:
        print("\n⚠️  --allow-regression given: publishing anyway, and the loss "
              "above is real and unrecoverable.", file=sys.stderr)
        return True
    print("\n⛔ REFUSING: the destination holds content the profile does not, "
          "and this run would overwrite it.\n"
          "  The destination is an OUTPUT of this pipeline, so the fix is to "
          "make the PROFILE correct — editing the destination is undone by the "
          "next run (see apply_upgrade.py).\n"
          "    python3 seed/scripts/apply_upgrade.py --site <site> "
          "--profile <profile> --posts <posts>\n"
          "  If the removal really is the intended change, say so with "
          "--allow-regression.", file=sys.stderr)
    return False


def preserved_csv_plan(args, collapse_loaded):
    """The ONE change `metadata.csv` may undergo in preserved mode.

    Returns (note, after_bytes) where `after_bytes` is None for an
    already-applied no-op, or False after printing a refusal.

    ⛔ THIS RUNS BEFORE ANY WRITE, AND IN --dry-run TOO. The document was
    produced from the FROZEN PRE-OPERATION CSV, so the first thing checked
    is that the destination's CSV is still those bytes: a document built
    against one file is not evidence about another, and the published
    archive has no backup.

    ⛔ IT IS NOT A WAIVER AND NOT AN EXACT BASELINE. A retirement
    legitimately removes its row, so exact bytes would refuse the one
    correct change; a waiver would have permitted the header-only file that
    the old regeneration actually wrote (0 of 907 site_a rows matched its
    source-path map, because the published column already holds DESTINATION
    paths). The document is narrower than either: exactly the enumerated
    rows leave, every other row survives byte-identically and IN ORDER, and
    the whole-file hash afterwards was stated in advance.
    """
    dest_csv = args.dest / pa.CSV_NAME
    try:
        doc = pa.load_csv_transform(args.csv_transform)
    except pa.PreservedError as e:
        print(f"error: {e}\n"
              "  Refusing: a transform document that cannot be validated is not "
              "evidence, and \"unusable\" must not be read as \"no expectations\". "
              "This is not overridable by --allow-regression.", file=sys.stderr)
        return False
    if not dest_csv.is_file():
        print(f"error: {dest_csv} is absent. In preserved mode the destination IS "
              f"the maintained dataset, so its {pa.CSV_NAME} is the input to the "
              f"documented transform and there is nothing to regenerate it from.",
              file=sys.stderr)
        return False
    blob = dest_csv.read_bytes()

    # ⛔ THE BINDING, BEFORE ANY WRITE AND BEFORE THE "already applied"
    # SHORTCUT. A transform states its own before and after hashes, so a
    # forged one that drops an extra row and recomputes its own expectations
    # is perfectly self-consistent. Measured against the first version of this
    # file: such a transform removed a row no collapse document mentioned and
    # the run exited 0. Internal arithmetic is not authority; the current
    # validated document is.
    #
    # The recomputation is against the PRE-OPERATION bytes, which is what
    # `blob` holds at this point: the authorised set is every retirement whose
    # produced file has a row in that CSV, and nothing else.
    collapse_doc = collapse_raw = None
    if collapse_loaded not in (None, False):
        _path, collapse_doc, collapse_raw = collapse_loaded
    # ⚠️ THE RECOMPUTATION NEEDS THE PRE-OPERATION ROWS, so it runs only when
    # the CSV in hand still IS the pre-operation CSV. On an idempotent re-run
    # the documented rows are already gone, so intersecting the document's
    # retirements with the rows present would come back empty and refuse a
    # correct no-op. The identity half still holds there, and the bytes are
    # separately proved to be exactly the documented result; that run writes
    # nothing either way, so the write path always carries the full check.
    pre_op = pa.sha256_bytes(blob) == doc["original"]["sha256"]
    bound = pa.binding_refusals(doc, profile_name=args.profile.name,
                                collapse_doc=collapse_doc,
                                collapse_raw=collapse_raw,
                                csv_blob=blob if pre_op else None)
    if bound:
        print(f"error: {args.csv_transform}: its removals are not authorised by "
              f"the collapse document this run uses:", file=sys.stderr)
        for r in bound:
            print(f"  - {r}", file=sys.stderr)
        print("  Refusing before writing anything. Re-emit the transform from "
              "the CURRENT document:\n"
              "    python3 seed/scripts/preserved_archive.py csv-transform "
              "--snapshot <frozen> --live-site <live> --staging <dest> \\\n"
              "        --collapse-document <asset-collapse.<stem>.json> --out "
              "<evidence>/<site>.csv-transform.json", file=sys.stderr)
        return False

    got = pa.sha256_file(dest_csv)
    if got == doc["expected"]["sha256"] and got != doc["original"]["sha256"]:
        print(f"  {pa.CSV_NAME}: already exactly the documented transform "
              f"({doc['expected']['data_rows']:,} row(s)); nothing to do. The "
              f"removal recomputation needs the pre-operation rows and they are "
              f"already gone, so the collapse-document binding was proved by "
              f"identity here; this run writes nothing.", file=sys.stderr)
        return ("already applied", None)
    after, refusals = pa.transform_csv(blob, doc)
    if refusals:
        print(f"error: {args.csv_transform}: {pa.CSV_NAME} at the destination is "
              f"not the file this document describes:", file=sys.stderr)
        for r in refusals:
            print(f"  - {r}", file=sys.stderr)
        return False
    check = pa.verify_csv_transform(doc, after)
    if check:
        print(f"error: the transform this run would apply does not match the "
              f"document's own expectations. Refusing before writing anything:",
              file=sys.stderr)
        for r in check:
            print(f"  - {r}", file=sys.stderr)
        return False
    note = (f"{doc['original']['data_rows']:,} row(s) -> "
            f"{doc['expected']['data_rows']:,}, "
            f"{len(doc['removals'])} documented removal(s)")
    if not doc["removals"]:
        note += " (ZERO-REMOVAL transform: the bytes must not change at all)"
    if after == blob:
        # ⛔ NOTHING TO CHANGE MEANS NOTHING IS WRITTEN. Re-writing identical
        # bytes would move the mtime of a preservation-owned file on a share
        # with no backup, for no gain. This is the site_a case: 907 rows to
        # 907, 0 removals.
        return (note + "; already exactly that, nothing to write", None)
    return (note, after)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--local-source", type=Path, default=None,
                        help="Local dataset root (where source_root='local' "
                             "resolves). Required UNLESS --preserved-roots is "
                             "given: the source dataset has been retired, and the "
                             "archive-authoritative mode has to be asked for by "
                             "name rather than inferred from a missing argument.")
    parser.add_argument("--preserved-roots", action="store_true",
                        help="Treat `local` as ARCHIVE-AUTHORITATIVE / PRESERVED "
                             "(#1319): its bytes are verified at <dest> like a "
                             "pre-staged root, never copied from a source, and "
                             "metadata.csv changes only through --csv-transform. "
                             "⛔ EXPLICIT ONLY. Omitting --local-source without "
                             "this flag is still an error.")
    parser.add_argument("--csv-transform", type=Path, default=None,
                        help="metadata.csv expected-transform document, produced "
                             "BEFORE this run by `preserved_archive.py "
                             "csv-transform` from the frozen pre-op CSV and the "
                             "committed collapse document. REQUIRED in preserved "
                             "mode: the published CSV is no longer regenerated "
                             "from the profile's source-path map, which matched 0 "
                             "of 907 site_a rows and wrote a header-only file.")
    parser.add_argument("--live-site", type=Path, default=None,
                        help="The LIVE published site. REQUIRED in preserved "
                             "mode: a preserved operation has three distinct "
                             "trees (live, the staging tree this run writes, and "
                             "the frozen pre-operation snapshot), and without "
                             "knowing where live is the run cannot prove the "
                             "snapshot is not it. For a direct publish pass the "
                             "same path as --dest.")
    parser.add_argument("--frozen-snapshot", type=Path, default=None,
                        help="A frozen pre-operation snapshot of the site. The "
                             "ONLY tree a preserved_archive retirement may be "
                             "authenticated against. Never the live tree and "
                             "never the staging copy.")
    parser.add_argument("--snapshot-manifest", type=Path, default=None,
                        help="External path->sha256 manifest of --frozen-snapshot, "
                             "recorded immediately after the snapshot was taken "
                             "(`preserved_archive.py snapshot-manifest`). The "
                             "relevant hashes are RECOMPUTED against it "
                             "immediately before authentication; a mismatch "
                             "refuses. Permissions and mtimes are not accepted as "
                             "integrity proof.")
    parser.add_argument("--internet-source", required=True, type=Path,
                        help="Internet-fetched cache root (where source_root='internet' resolves)")
    parser.add_argument("--hq-source", type=Path, default=None,
                        help="kenney-hq pool root (where source_root='hq' resolves). "
                             "Build it with kenney_hq.py build. Required only if "
                             "the profile references HQ assets (#604).")
    parser.add_argument("--pack-source", type=Path, default=None,
                        help="'Kenney Game Assets All-in-1' bundle root (where "
                             "source_root='pack' resolves, #572). Optional: "
                             "records that name a metadata.source_archive are "
                             "downloaded from the pack's free CC0 zip when the "
                             "bundle is not present, so a machine without the "
                             "archive share can still build the site.")
    parser.add_argument("--profile", required=True, type=Path,
                        help="Per-studio profile JSON (studio-a.assets.json etc.)")
    parser.add_argument("--posts", type=Path, default=None,
                        help="Per-studio posts JSON (studio-a.posts.json), "
                             "staged as <dest>/posts.json — `aa seed` reads it "
                             "next to MANIFEST.json. Optional only because "
                             "earlier runs copied it by hand, which is how "
                             "site_a came to serve 584 posts against a profile "
                             "holding 859 (#572). Pass it.")
    parser.add_argument("--dest", required=True, type=Path,
                        help="Destination site directory under the archive")
    parser.add_argument("--migration-document", type=Path, default=None,
                        help="Override where the post-id migration document is "
                             "read from (#1319). Normally NOT needed: it is "
                             "located from --posts as "
                             "seed/upgrades/post-id-migration.<stem>.json. The "
                             "document's `profile` field must still name the "
                             "--posts file. For fixtures laid out elsewhere.")
    parser.add_argument("--collapse-document", type=Path, default=None,
                        help="Override where the asset-collapse document is "
                             "read from (#1319). Normally NOT needed: it is "
                             "located from --profile as "
                             "seed/upgrades/asset-collapse.<stem>.json. The "
                             "document's `profile` field must still name the "
                             "--profile file. For fixtures laid out elsewhere.")
    parser.add_argument("--prune", action="store_true",
                        help="Delete files at <dest> not in the profile")
    parser.add_argument("--dry-run", action="store_true",
                        help="Report what would copy without writing")
    parser.add_argument("--no-refetch", action="store_true",
                        help="Do not download pre-staged records that are "
                             "missing at <dest>, even when they carry a "
                             "metadata.media_url (#602). Restores the old "
                             "verify-only behaviour.")
    parser.add_argument("--allow-regression", action="store_true",
                        help="Publish even though <dest> holds records or "
                             "values the profile does not (#1275). The loss "
                             "is still printed in full. Use this ONLY when "
                             "the removal is the intended change — the "
                             "default refusal exists because the destination "
                             "is the PUBLISHED dataset and the loss is "
                             "silent and unrecoverable.")
    args = parser.parse_args()

    # ── THE MODE, DECIDED BEFORE ANYTHING ELSE ────────────────────────────
    preserved = bool(args.preserved_roots)
    prestaged = PRESTAGED_ROOTS | PRESERVED_ROOTS if preserved else PRESTAGED_ROOTS
    if preserved:
        # ⛔ A SOURCE ROOT FOR A PRESERVED ROOT IS MEANINGLESS AND IS REFUSED.
        # In this mode `local` bytes are the archive's own and are verified
        # where they sit; a `--local-source` would either be the destination
        # (the destination as its own evidence) or a copy of it, and a copy
        # of the published tree carries DESTINATION paths in its
        # metadata.csv, which is what made the CSV regeneration write a
        # header-only file.
        if args.local_source is not None:
            print("error: --local-source is meaningless with --preserved-roots: "
                  f"`local` is treated as archive-authoritative, so its bytes are "
                  f"verified at --dest and never copied from a source.\n"
                  f"  Drop --local-source, or drop --preserved-roots and name a "
                  f"real source dataset.", file=sys.stderr)
            return 2
        if args.csv_transform is None:
            print("error: --preserved-roots requires --csv-transform.\n"
                  "  metadata.csv is no longer regenerated from the profile's "
                  "source-path map. Measured against the real published trees "
                  "that matched 0 of 907 site_a rows and 0 of 1,206 site_b rows, "
                  "and wrote a HEADER-ONLY file. It now changes only in the way a "
                  "document written BEFORE this run says it may:\n"
                  "    python3 seed/scripts/preserved_archive.py csv-transform "
                  "--snapshot <frozen> \\\n"
                  "        --live-site <live> --staging <dest> "
                  "--collapse-document <asset-collapse.<stem>.json> \\\n"
                  "        --out <outside-all-three>/<site>.csv-transform.json",
                  file=sys.stderr)
            return 2
        if args.live_site is None:
            print("error: --preserved-roots requires --live-site.\n"
                  "  A preserved operation has THREE distinct trees: the live "
                  "published site, the staging tree this run writes (--dest), "
                  "and the frozen pre-operation snapshot a preserved retirement "
                  "is authenticated against. Comparing the snapshot against "
                  "--dest alone proves only that it is not the tree being "
                  "written: it does NOT stop the LIVE site being passed as the "
                  "snapshot while --dest points at staging, and that run "
                  "authenticates against a tree nobody froze.\n"
                  "  For a direct publish, pass the same path as --dest.",
                  file=sys.stderr)
            return 2
        # ⛔ THE WHOLE BOUNDARY, BEFORE ANYTHING ELSE. Six shapes on the
        # snapshot (equal to or nested with live, equal to or nested with
        # staging), nesting between live and staging, and every evidence
        # document outside all three.
        # `require_snapshot=False` here ONLY: a preserved publish whose
        # collapse document holds no `preserved_archive` entry authenticates
        # nothing against a snapshot, so demanding one would refuse a correct
        # run. A snapshot that IS supplied is checked in full, and
        # `_preserved_snapshot` demands one unconditionally for the entries
        # that need it.
        if not pa.refuse_operation(
                live=args.live_site, staging=args.dest,
                snapshot=args.frozen_snapshot, require_snapshot=False,
                evidence={"--snapshot-manifest": args.snapshot_manifest,
                          "--csv-transform": args.csv_transform}):
            return 2
        print("PRESERVED-ROOT MODE (#1319): `local` is treated as "
              "ARCHIVE-AUTHORITATIVE / PRESERVED.", file=sys.stderr)
        print(f"  roots verified at the destination rather than copied: "
              f"{sorted(prestaged)}", file=sys.stderr)
        print("  metadata.csv changes ONLY through the expected-transform "
              "document; it is never regenerated from the profile.",
              file=sys.stderr)
        print("  ⚠️  `preserved_archive` evidence is a WEAKER claim than "
              "`produced_source`. It rests on the frozen snapshot manifest "
              "recomputation, never on the archive agreeing with itself.",
              file=sys.stderr)
    elif args.local_source is None:
        print("error: --local-source is required.\n"
              "  If `local` should be treated as ARCHIVE-AUTHORITATIVE because "
              "the source dataset is gone, say so explicitly with "
              "--preserved-roots (#1319). That mode is never inferred from a "
              "missing argument: a fallback cannot tell a decision from a typo.",
              file=sys.stderr)
        return 2

    sources: dict[str, Path] = {"internet": args.internet_source}
    if args.local_source is not None:
        sources["local"] = args.local_source
    if args.hq_source is not None:
        sources["hq"] = args.hq_source
    if args.pack_source is not None:
        sources["pack"] = args.pack_source

    # ⛔ ALIAS REFUSAL, BEFORE ANY MUTATION AND BEFORE ANY EVIDENCE IS USED.
    # Equality is not the only way for a source to be the destination: a
    # "source" that CONTAINS <dest>, or sits inside it, reads bytes this run
    # is about to write. Resolved paths only, so `..`, a symlink or a
    # trailing slash cannot defeat it. Every path this run knows about is
    # named here; a pair missing from this map is a pair nobody checked.
    if not pa.refuse_aliasing({
            **{f"--{k}-source": v for k, v in sources.items()},
            "--dest": args.dest,
            "--frozen-snapshot": args.frozen_snapshot,
            "--snapshot-manifest": args.snapshot_manifest,
            "--csv-transform": args.csv_transform,
            "--profile": args.profile,
            "--posts": args.posts,
            "--collapse-document": args.collapse_document,
            "--migration-document": args.migration_document},
            equal_ok_within=[{f"--{k}-source" for k in
                              ("local", "internet", "hq", "pack")}]):
        return 2

    src_csv = src_groups = None
    if not preserved:
        if not sources["local"].is_dir():
            print(f"error: --local-source not a directory: {sources['local']}",
                  file=sys.stderr)
            return 2
        src_csv = sources["local"] / pa.CSV_NAME
        src_groups = sources["local"] / pa.GROUPS_NAME
        if not src_csv.is_file():
            print(f"error: {pa.CSV_NAME} not found at {src_csv}", file=sys.stderr)
            return 2
    # ⛔ ONE LOAD, SHARED BY THE TRANSFORM BINDING AND BY LAYER B. Two
    # independent reads of the same path would prove nothing about them being
    # the same bytes, which is exactly the gap the binding exists to close.
    collapse_loaded = load_collapse(args)
    if collapse_loaded is False:
        return 2

    csv_plan = None
    if preserved:
        csv_plan = preserved_csv_plan(args, collapse_loaded)
        if csv_plan is False:
            return 2

    print(f"loading profile {args.profile}", file=sys.stderr)
    profile = json.loads(args.profile.read_text(encoding="utf-8"))
    if not isinstance(profile, list):
        print(f"error: profile root must be a list of assets", file=sys.stderr)
        return 2

    # ⛔ THE GUARD (#1275). Before anything is written — and in --dry-run
    # too, because a dry run that passes while a real run would destroy
    # data is worse than no dry run at all.
    #
    # This step copies the profile straight over <dest>/MANIFEST.json and
    # <dest>/posts.json, and regenerates metadata.csv from the profile's
    # path map. So whatever the profile says wins, INCLUDING when the
    # profile says less. Measured 2026-08-26, the committed profile was
    # behind the published site_a by one whole asset and 12,096 values
    # across 1,947 records, and running this script would have deleted
    # them without a word. `apply_upgrade.py` reconciles the profile;
    # this refuses the run when it has not been.
    # ⛔ LAYER B, BEFORE THE GUARD THAT CONSUMES IT. A retirement is
    # source-authenticated here, with the source roots this script
    # already takes: both produced files located, hashed, required equal
    # to each other in this build and equal to the document's recorded
    # value. Only then may the guard report COLLAPSED_RECORD instead of
    # MISSING_RECORD. An unauthenticated retirement is a refusal, and a
    # refusal is what the destination-holds-what-the-source-does-not case
    # has always been.
    collapses = authenticate_collapses(args, sources, profile,
                                       preserved=preserved,
                                       collapse_loaded=collapse_loaded)
    if collapses is False:
        return 2

    if not check_destination(args, profile, collapses):
        return 2

    # Fail loudly and early rather than per-file. A profile that
    # references the HQ pool without --hq-source would otherwise print a
    # few MISSING lines, skip 916 assets, and still exit 0 — leaving a
    # site whose manifest names files that were never copied. That is the
    # silent-success shape this whole issue is about (#604).
    hq_wanted = sum(1 for a in profile if a.get("source_root") == "hq")
    if hq_wanted and "hq" not in sources:
        print(f"error: profile references {hq_wanted} kenney-hq assets but "
              "--hq-source was not given.\n"
              "  Build the pool first:\n"
              "    python3 seed/scripts/kenney_hq.py build "
              "--pack <kenney-pack> --out <pool>", file=sys.stderr)
        return 2
    if hq_wanted and not sources["hq"].is_dir():
        print(f"error: --hq-source not a directory: {sources['hq']}\n"
              "  If it is on the archive share, the mount may have dropped — "
              "that reads as 'No such file or directory'.", file=sys.stderr)
        return 2

    # Build the (source_root, source_path) → file_path mapping
    path_map = {
        (a.get("source_root", "local"), a["source_path"]): a["file_path"]
        for a in profile if a.get("file_path") and a.get("source_path")
    }
    wanted_dest_paths = set(path_map.values())
    # The pre-staged branch needs the RECORD, not just the path — the
    # media_url and the byte count it is checked against both live there.
    by_dest = {a["file_path"]: a for a in profile if a.get("file_path")}
    wanted_group_ids = {a.get("metadata", {}).get("group_id") for a in profile}
    wanted_group_ids.discard(None)
    wanted_group_ids.discard("")

    by_root: dict[str, int] = {"local": 0, "internet": 0}
    for (root, _), _ in path_map.items():
        by_root[root] = by_root.get(root, 0) + 1
    print(f"  {len(path_map):,} assets ({by_root.get('local', 0):,} local, "
          f"{by_root.get('internet', 0):,} internet)", file=sys.stderr)
    print(f"  {len(wanted_group_ids):,} group_ids", file=sys.stderr)

    if not args.dry_run:
        safe_mkdir(args.dest)
        # Pre-create every typed-folder root we'll write into. The SMB
        # mount sometimes returns stale "directory exists" info after a
        # half-completed run; doing this upfront with a fresh stat avoids
        # the per-file mkdir race that bit us earlier.
        type_roots = {dp.split("/", 1)[0] for dp in wanted_dest_paths if "/" in dp}
        for tr in sorted(type_roots):
            safe_mkdir(args.dest / tr)
        # Also pre-create the per-pack subdirs (one level deeper) for the
        # same reason — gets all the directory creation out of the way
        # before any file copies start.
        pack_dirs = {str(Path(dp).parent) for dp in wanted_dest_paths if "/" in dp}
        for pd in sorted(pack_dirs):
            safe_mkdir(args.dest / pd)

    if preserved:
        # ⛔ NO REGENERATION AND NO FILTER. `metadata.csv` moves only the
        # way the expected-transform document says, and `groups.csv` does
        # not move at all: its `asset_count` is an original-dataset fact
        # that already disagrees with the shipped subset (262 of site_b's
        # 1,047 rows), no group loses its last shipped member, and it is
        # preservation-owned in `verify_site.PRESERVED_NAMES`, so any byte
        # change fails the ordinary preservation check.
        note, after = csv_plan
        print(f"{pa.CSV_NAME}: applying the documented transform ({note})",
              file=sys.stderr)
        if after is None:
            pass
        elif args.dry_run:
            print(f"  would write {len(after):,} B to "
                  f"{args.dest / pa.CSV_NAME}", file=sys.stderr)
        else:
            (args.dest / pa.CSV_NAME).write_bytes(after)
            print(f"  wrote {len(after):,} B", file=sys.stderr)
        print(f"{pa.GROUPS_NAME}: preservation-owned, left untouched",
              file=sys.stderr)
    else:
        # Filter + rewrite metadata.csv — keep rows whose original file_path
        # belongs to this site; rewrite the file_path column to the new layout
        # so the seeded instance can resolve it under <dest>.
        print(f"filtering + rewriting {pa.CSV_NAME} → "
              f"{args.dest / pa.CSV_NAME}", file=sys.stderr)
        # Rows are matched by the source path they ORIGINALLY had. An asset
        # whose bytes were swapped for a kenney-hq render (#604) keeps its
        # CSV row and gets the new path written into it — the record survives
        # the file swap, which is the same "swap the file, keep the record"
        # rule the upgrade itself follows. `replaced_source_path` is what
        # remembers the original; without it these rows match nothing and
        # drop out of the shipped CSV entirely.
        local_path_to_dest = {
            src_path: dest_path
            for (root, src_path), dest_path in path_map.items()
            if root == "local"
        }
        for a in profile:
            original = a.get("replaced_source_path")
            if original and a.get("file_path"):
                local_path_to_dest[original] = a["file_path"]
        kept_rows = 0
        with src_csv.open(newline="", encoding="utf-8") as f:
            reader = csv.DictReader(f)
            fieldnames = reader.fieldnames
            out_rows: list[dict] = []
            total_rows = 0
            for r in reader:
                total_rows += 1
                if r["file_path"] in local_path_to_dest:
                    r["file_path"] = local_path_to_dest[r["file_path"]]
                    out_rows.append(r)
            kept_rows = len(out_rows)
            # ⛔ A RUN THAT WOULD KEEP NOTHING REFUSES RATHER THAN WRITING A
            # HEADER-ONLY FILE. Measured: pointed at the PUBLISHED archive
            # as --local-source, this filter matches 0 of 907 site_a rows
            # and 0 of 1,206 site_b rows, because the published `file_path`
            # column already holds DESTINATION paths while the map is keyed
            # by SOURCE paths. It then wrote a header and stopped, and
            # nothing downstream noticed. Dropping every row is never the
            # intended change, so it is not overridable.
            if total_rows and not kept_rows:
                print(f"error: the {pa.CSV_NAME} filter matched 0 of "
                      f"{total_rows:,} rows, so this run would publish a "
                      f"HEADER-ONLY {pa.CSV_NAME}.\n"
                      f"  --local-source {sources['local']} does not look like a "
                      f"SOURCE dataset: its `file_path` column has to hold source "
                      f"paths, and a PUBLISHED archive holds destination paths.\n"
                      f"  If that tree is the maintained dataset, say so: "
                      f"--preserved-roots with --csv-transform (#1319). Dropping "
                      f"every row is never the intended change, so this is not "
                      f"overridable by --allow-regression.", file=sys.stderr)
                return 2
            if not args.dry_run:
                with (args.dest / pa.CSV_NAME).open(
                        "w", newline="", encoding="utf-8") as out:
                    writer = csv.DictWriter(out, fieldnames=fieldnames)
                    writer.writeheader()
                    writer.writerows(out_rows)
        print(f"  kept {kept_rows:,} rows", file=sys.stderr)

        if src_groups.is_file():
            print(f"filtering {pa.GROUPS_NAME} → {args.dest / pa.GROUPS_NAME}",
                  file=sys.stderr)
            kept_groups = 0
            with src_groups.open(newline="", encoding="utf-8") as f:
                reader = csv.DictReader(f)
                fieldnames = reader.fieldnames
                rows = [r for r in reader if r["group_id"] in wanted_group_ids]
                kept_groups = len(rows)
                if not args.dry_run:
                    with (args.dest / pa.GROUPS_NAME).open(
                            "w", newline="", encoding="utf-8") as out:
                        writer = csv.DictWriter(out, fieldnames=fieldnames)
                        writer.writeheader()
                        writer.writerows(rows)
            print(f"  kept {kept_groups:,} rows", file=sys.stderr)

    if not args.dry_run:
        shutil.copyfile(args.profile, args.dest / "MANIFEST.json")
        print(f"copied profile → {args.dest / 'MANIFEST.json'}", file=sys.stderr)
        if args.posts is not None:
            shutil.copyfile(args.posts, args.dest / "posts.json")
            print(f"copied posts   → {args.dest / 'posts.json'}", file=sys.stderr)
        elif (args.dest / "posts.json").is_file():
            # Loud, because the failure is silent: the seeder happily
            # loads a stale posts.json against a fresh MANIFEST, and the
            # only symptom is a browse wall with fewer posts than the
            # dataset says it has.
            print("warning: --posts not given; <dest>/posts.json is left as "
                  "it was and may not match the profile just written",
                  file=sys.stderr)

    print(f"copying {len(path_map):,} asset files", file=sys.stderr)
    copied = 0
    companions_copied = 0
    skipped = 0
    missing = 0
    bytes_copied = 0
    refetched = 0
    progress_every = max(1, len(path_map) // 20)
    items = sorted(path_map.items(), key=lambda x: x[1])  # sort by dest path
    preexisting = 0
    wrong = 0
    for i, ((root, source_path), dest_rel) in enumerate(items):
        # PRE-STAGED roots: the bytes already live at the destination and
        # have no LOCAL source to copy from, so verify rather than copy.
        #
        #   torrent_import — pre-copied on the destination NAS
        #                    (Synology-to-Synology from /volume1/torrents/).
        #   site           — added directly to the site (#604).
        #
        # Verify-don't-copy is also what makes re-assembly SAFE for them:
        # the alternative — treating an unreachable source as "drop the
        # record" — is exactly how the added videos would disappear.
        #
        # When one IS absent, `metadata.media_url` (#602) turns what used
        # to be a terminal MISSING into a download. That field is the
        # direct CDN URL, recorded only after a HEAD confirmed it serves
        # exactly `file_size_bytes`; the Pexels page URL in `fetched_from`
        # is an HTML document and was never something you could GET bytes
        # from, which is what left this branch a dead end before.
        # `prestaged` is PRESTAGED_ROOTS, plus PRESERVED_ROOTS when
        # --preserved-roots was asked for: a preserved `local` record's
        # bytes are the archive's own, so they are VERIFIED where they sit
        # and never copied from a source. Its manifest byte count is the
        # oracle, and a disagreement is now a refusal rather than a stale
        # publish (manifest_guard.MEASURABLE_ROOTS).
        if root in prestaged:
            dest_file = args.dest / dest_rel
            rec = by_dest.get(dest_rel) or {}
            want = rec.get("file_size_bytes")
            if dest_file.is_file() and dest_file.stat().st_size > 0:
                # ⛔ `> 0` USED TO BE THE WHOLE CHECK (#1301), and it is
                # the reason nothing ever noticed that eleven records
                # described a 1.1 GB original while a two-minute cut sat
                # here. A pre-staged root has no source to compare
                # against, so this is the ONLY place the claim can be
                # tested — and it was testing that the file was not
                # empty. `file_size_bytes` now means the shipped bytes,
                # so the manifest's own number is the oracle.
                got = dest_file.stat().st_size
                want_sha = (rec.get("metadata") or {}).get("sha256")
                if want and got != want:
                    wrong += 1
                    if wrong <= 5:
                        print(f"  WRONG SIZE [{root}]: {dest_rel} — manifest "
                              f"says {want:,} B, staged file is {got:,} B. "
                              "Re-emit seed/upgrades/staged-measurements."
                              "<site>.json (measure_staged.py) rather than "
                              "editing either by hand.", file=sys.stderr)
                # ⛔ WHERE THE RECORD DECLARES A HASH, THE BYTES ARE HASHED.
                # A size match is not a byte match, and for a pre-staged or
                # PRESERVED root the recorded hash is the only attestation
                # of the shipped bytes there is: nothing else in the run
                # has an independent copy to compare against. The two
                # authored plates (#1290) are exactly this case: same shape,
                # different pixels would be invisible to a length check.
                #
                # ⚠️ MEASURED BEFORE IT WAS ADDED, on the real published
                # trees: 96 site_a and 39 site_b records declare a sha256 on
                # a pre-staged root and ALL 135 match, so this refuses
                # nothing that exists today. `internet` is deliberately not
                # a pre-staged root and never reaches here: its sha256 is
                # the hash of the DOWNLOAD and is that record's identity,
                # not a description of the shipped cut.
                elif want_sha and sha256_file(dest_file) != want_sha:
                    wrong += 1
                    if wrong <= 5:
                        print(f"  WRONG BYTES [{root}]: {dest_rel}: the "
                              f"staged file is {got:,} B as the manifest "
                              f"says, but hashes "
                              f"{sha256_file(dest_file)[:12]}… where the "
                              f"record declares {want_sha[:12]}…. Same "
                              f"length, different bytes.", file=sys.stderr)
                else:
                    preexisting += 1
                continue
            media_url = (rec.get("metadata") or {}).get("media_url")
            if media_url and not args.no_refetch and not args.dry_run:
                # BOTH oracles, and both describe the SHIPPED file. The
                # URL points at the origin, which for these records is
                # not what the dataset publishes, so this is the call
                # that has to decline it.
                ok, note = refetch(media_url, dest_file, want,
                                   (rec.get("metadata") or {}).get("sha256"))
                if ok:
                    refetched += 1
                    bytes_copied += dest_file.stat().st_size
                    print(f"  REFETCHED [{root}]: {dest_rel} ({note})",
                          file=sys.stderr)
                    continue
                print(f"  REFETCH FAILED [{root}]: {dest_rel} — {note}\n"
                      f"    {media_url}", file=sys.stderr)
            missing += 1
            if missing <= 5:
                hint = ("" if media_url else
                        " — no metadata.media_url either, so there is nothing "
                        "to re-fetch from (see resolve_media_urls.py, #602)")
                print(f"  MISSING [{root}]: {dest_rel} — not pre-staged?{hint}",
                      file=sys.stderr)
            continue

        src_root = sources.get(root)
        src_file = (src_root / source_path) if src_root else None
        if src_file is None or not src_file.is_file():
            # #572 — bundle-sourced records carry the pack's free CC0 zip
            # plus the member path and its sha256, so an absent bundle is
            # a download rather than a hole. Same shape as #602's
            # media_url branch: only for records that would otherwise
            # fail, so a run over a mounted bundle does no network I/O.
            rec = by_dest.get(dest_rel) or {}
            sa = (rec.get("metadata") or {}).get("source_archive") or {}
            dest_file = args.dest / dest_rel
            if (root == "pack" and sa.get("url") and not args.no_refetch
                    and not args.dry_run):
                if dest_file.is_file() and dest_file.stat().st_size > 0:
                    skipped += 1
                    continue
                ok, note = refetch_member(sa["url"], sa["member"],
                                          sa.get("sha256", ""), dest_file)
                if ok:
                    refetched += 1
                    bytes_copied += dest_file.stat().st_size
                    print(f"  REFETCHED [{root}]: {dest_rel} ({note})",
                          file=sys.stderr)
                    continue
                print(f"  REFETCH FAILED [{root}]: {dest_rel} — {note}",
                      file=sys.stderr)
            # An absent SOURCE is not an absent ASSET. The internet cache
            # is gitignored and routinely not present on a machine that
            # already has a populated site; reporting 58 fully-staged
            # videos as MISSING and exiting 1 is the same
            # unavailable-is-not-absent confusion that makes a dropped
            # mount look like data loss. Gated on the manifest's own byte
            # count, so a genuinely short or wrong file still fails.
            want = (rec or {}).get("file_size_bytes")
            if (dest_file.is_file() and want
                    and dest_file.stat().st_size == want):
                preexisting += 1
                continue
            missing += 1
            if missing <= 5:
                hint = ("" if root != "pack" else
                        " — pass --pack-source <bundle>, or let the "
                        "metadata.source_archive re-fetch handle it")
                print(f"  MISSING [{root}]: {source_path}{hint}",
                      file=sys.stderr)
            continue
        dest_file = args.dest / dest_rel
        already = (dest_file.is_file()
                   and dest_file.stat().st_size == src_file.stat().st_size)
        if already:
            skipped += 1
        elif args.dry_run:
            copied += 1
            bytes_copied += src_file.stat().st_size
        else:
            safe_mkdir(dest_file.parent)
            shutil.copyfile(src_file, dest_file)
            copied += 1
            bytes_copied += src_file.stat().st_size

        # Companions run even when the MODEL was skipped (#572). They used
        # to sit inside the copy branch, so a model already present at the
        # destination short-circuited past its own siblings — which is
        # exactly how Sponza came to sit in site_a as a lone .gltf naming
        # a .bin and 69 textures that were never staged, and why it was
        # the only 3D asset in the instance stuck at `failed`. The model
        # matching by size proves nothing about the 70 files beside it.
        # NOTE: this used to `continue` here when `args.dry_run and
        # already`, which made --dry-run blind to exactly the case #750 and
        # #753 are about: every model in a populated site is `already`, so
        # a dry run reported "companions: 0" whether the resolver had
        # learned a new format or not, and the only way to see what a fix
        # would stage was to mutate the share. A dry run has to report the
        # work the real run would do.

        # Multi-file models (#486): copy the .gltf/.obj siblings the model
        # declares (buffer, textures, .mtl) next to the destination so the
        # asset resolves at render + view time. The Go seed runner then
        # auto-registers whatever landed next to the model as companions.
        for rel in resolve_model_companions(src_file):
            comp_src = src_file.parent / rel
            comp_dest = dest_file.parent / rel
            comp_rel = (Path(dest_rel).parent / rel).as_posix()
            wanted_dest_paths.add(comp_rel)  # survive --prune
            if comp_dest.is_file() and comp_dest.stat().st_size == comp_src.stat().st_size:
                continue
            if args.dry_run:
                companions_copied += 1
                bytes_copied += comp_src.stat().st_size
                continue
            safe_mkdir(comp_dest.parent)
            shutil.copyfile(comp_src, comp_dest)
            companions_copied += 1
            bytes_copied += comp_src.stat().st_size

        if (i + 1) % progress_every == 0:
            print(f"  ... {i+1:,}/{len(path_map):,} "
                  f"({bytes_copied / 2**20:.1f} MB)", file=sys.stderr)

    retired_removed = 0
    retired_ok = True
    if collapses:
        print(f"removing {len(collapses)} retired path(s) (#1319)", file=sys.stderr)
        retired_removed, retired_ok = remove_retired_paths(
            args, collapses, wanted_dest_paths)

    pruned = 0
    if args.prune and args.dest.is_dir():
        print(f"pruning files not in profile", file=sys.stderr)
        for f in args.dest.rglob("*"):
            if not f.is_file():
                continue
            rel = f.relative_to(args.dest).as_posix()
            if rel in ("metadata.csv", "groups.csv", "MANIFEST.json"):
                continue
            if rel not in wanted_dest_paths:
                if args.dry_run:
                    pruned += 1
                else:
                    f.unlink()
                    pruned += 1
        # Clean empty dirs
        if not args.dry_run:
            for d in sorted(args.dest.rglob("*"), key=lambda p: -len(str(p))):
                if d.is_dir() and not any(d.iterdir()):
                    d.rmdir()
        print(f"  pruned {pruned:,} stale files", file=sys.stderr)

    print(f"\n=== Summary ===", file=sys.stderr)
    print(f"  copied:      {copied:,} files ({bytes_copied / 2**30:.2f} GB)", file=sys.stderr)
    print(f"  companions:  {companions_copied:,} multi-file model siblings (#486)", file=sys.stderr)
    print(f"  skipped:     {skipped:,} (already present, same size)", file=sys.stderr)
    print(f"  preexisting: {preexisting:,} (pre-staged — bytes already in place)",
          file=sys.stderr)
    print(f"  refetched:   {refetched:,} (downloaded from metadata.media_url #602 / source_archive #572)",
          file=sys.stderr)
    print(f"  missing:     {missing:,} (not found in source)", file=sys.stderr)
    print(f"  wrong size:  {wrong:,} (pre-staged bytes disagree with the "
          f"manifest #1301)", file=sys.stderr)
    if collapses:
        print(f"  retired:     {retired_removed:,} produced file(s) of "
              f"source-authenticated retirements "
              f"{'would be ' if args.dry_run else ''}removed (#1319)",
              file=sys.stderr)
    if args.prune:
        print(f"  pruned:  {pruned:,} stale files removed", file=sys.stderr)
    if missing > 5:
        print(f"  (first 5 missing logged above)", file=sys.stderr)
    if wrong > 5:
        print(f"  (first 5 wrong-size logged above)", file=sys.stderr)

    # ⛔ A WRONG SIZE FAILS THE RUN, exactly as a missing file does. The
    # two are the same defect seen from different ends: the dataset does
    # not hold what its manifest says it holds. Exiting 0 on "the file is
    # there, it is simply not the one we describe" is the silent-success
    # shape this script has been bitten by twice (#604, #1301).
    return 0 if (missing == 0 and wrong == 0 and retired_ok) else 1


if __name__ == "__main__":
    sys.exit(main())
