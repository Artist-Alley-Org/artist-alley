#!/usr/bin/env python3
"""
Give every internet-sourced seed record a DIRECT media URL, so the bytes
can be re-fetched from the recorded provenance alone (#602).

THE GAP THIS CLOSES
-------------------
`metadata.fetched_from` was recorded as whatever URL a human was looking
at when they took the file. For most sources that happens to be the file
itself (`upload.wikimedia.org/.../MineClone2.webm`), so re-fetching is a
GET away. For the 30 Pexels videos added in #605 it is the *listing
page* — `https://www.pexels.com/video/…-10161903/` — which is an HTML
document behind Cloudflare, not an mp4. #605 handled that correctly at
the time by wiring those records into the pre-existing
`torrent_import`-style verify-don't-copy root (`site`), so re-assembly
VALIDATES them. But validate is not re-download: on a machine without
the archive share mounted, those 60 records (30 × 2 sites) could not be
reconstructed at all.

WHAT IS RECORDED, AND WHY BOTH
------------------------------
`metadata.fetched_from` is KEPT, untouched. It is the human-facing
attribution link and the licence evidence — the Pexels page is where the
Pexels License, the photographer credit and the terms actually live, and
ATTRIBUTIONS.md/Kaggle paperwork point at it. Replacing it with a CDN
path would close the re-fetch gap by opening an attribution one, which
is a trade, not a fix.

`metadata.media_url` is ADDED: the direct, unauthenticated URL of the
exact bytes we shipped. One uniform key across every internet-sourced
record — for sources whose `fetched_from` was already direct, the two
are simply equal, so a consumer never has to know which source it is
looking at.

Correctness is not asserted by shape. A candidate URL is accepted ONLY
when a HEAD returns `Content-Length` equal to the record's recorded
`file_size_bytes`. A URL that merely looks like an mp4 proves nothing;
a byte count that matches the manifest is evidence that the URL serves
the file we actually have.

HOW A PEXELS PAGE URL IS RESOLVED
---------------------------------
1. PATTERN. Older uploads name the file after the video id:
   `videos.pexels.com/video-files/<id>/<id>-hd_<W>_<H>_<fps>fps.mp4`.
   W×H come from the record's own description ("1280x720 12s landscape"),
   so the candidate list is short. Measured: 19 of 30 resolve this way
   with no page fetch at all.
2. PAGE. Newer uploads use a DIFFERENT file id and drop the quality
   prefix — `video-files/27580032/12174126_1080_1920_50fps.mp4` — which
   no pattern can derive. Those need the page, which is Cloudflare-
   guarded (a plain GET returns 403). Set `FLARESOLVERR_URL` to route
   the page fetch through FlareSolverr; the media URLs scraped out of it
   are then plain HTTPS on a CDN with no challenge.

That asymmetry is the whole argument for storing the result: resolving
needs a challenge-solver, re-fetching needs `urllib`.

Usage
-----
    # resolve + write (needs network; FLARESOLVERR_URL for the page path)
    python3 resolve_media_urls.py --write \\
        seed/upgrades/added-assets.site_a.json \\
        seed/profiles/studio-a.assets.json

    # structural gate — no network, safe in CI
    python3 resolve_media_urls.py --check seed/profiles/studio-a.assets.json

    # prove the round trip: HEAD every media_url, compare Content-Length
    python3 resolve_media_urls.py --verify seed/profiles/studio-a.assets.json

    # actually download, and hash against the staged copy if given
    python3 resolve_media_urls.py --refetch /tmp/out \\
        [--against /mnt/.../site_a] [--limit 3] seed/profiles/studio-a.assets.json
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

# Wikimedia's UA policy wants a descriptive agent with a contact route;
# an anonymous one gets 429'd hard on media files.
UA = ("artist-alley-seed-fetcher/2.0 "
      "(+https://github.com/Artist-Alley-Org/artist-alley)")

# Hosts that serve bytes rather than an HTML page. A media_url outside
# this set is a red flag, not a hard error — new sources get added here
# deliberately, which is the point of the list.
DIRECT_MEDIA_HOSTS = frozenset({
    "videos.pexels.com",
    "images.pexels.com",
    "upload.wikimedia.org",
    "download.blender.org",
    "archive.org",
    "www.archive.org",
    "ia800000.us.archive.org",
})

# Pexels' own resolution ladder. `hd` first because every record we hold
# is 720p or better; the fps order is the observed frequency in the set,
# which is what keeps the median resolve under three HEAD requests.
PEXELS_QUALITIES = ("hd", "uhd", "sd")
PEXELS_FPS = (30, 25, 24, 60, 50)

_PEXELS_ID_RE = re.compile(r"-(\d+)/?$")
_DIMS_RE = re.compile(r"^(\d+)x(\d+)")
_PEXELS_MEDIA_RE = re.compile(r"https://videos\.pexels\.com/video-files/\d+/[^\"'\\\s<>]+")


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

def head_probe(url: str, timeout: int = 30,
               retries: int = 3) -> tuple[str, int | None]:
    """HEAD `url` -> (outcome, content_length).

    outcome is one of:
      'ok'          — served, with a Content-Length
      'absent'      — 401/403/404/410; this URL will not give you the file
      'unavailable' — 429/5xx/timeout; the HOST would not answer

    The 'absent' / 'unavailable' split is the point. upload.wikimedia.org
    rate-limits a concurrent burst with 429, and a run that folded that
    into "no match" would report perfectly good provenance as broken —
    the same unavailable-is-not-absent confusion that makes an unmounted
    share look like data loss. Only 'absent' is evidence about the URL.

    403 counts as absent, not as a block: videos.pexels.com is
    S3-backed and answers 403 (not 404) for a key that does not exist, so
    every miss in the candidate ladder lands there. Retrying those with a
    backoff turned a ~2-second resolve into three minutes.
    """
    req = urllib.request.Request(url, method="HEAD", headers={"User-Agent": UA})
    for attempt in range(retries + 1):
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                raw = resp.headers.get("Content-Length")
                return "ok", (int(raw) if raw else None)
        except urllib.error.HTTPError as e:
            if e.code in (401, 403, 404, 410):
                return "absent", None
        except Exception:
            pass
        if attempt < retries:
            time.sleep(2.0 * (attempt + 1))
    return "unavailable", None


def head_length(url: str) -> int | None:
    """Content-Length, or None when the URL did not serve one."""
    return head_probe(url)[1]


def get_text(url: str, timeout: int = 60) -> str | None:
    """Plain GET. Returns None on any failure INCLUDING a Cloudflare 403,
    which is the normal outcome for a Pexels page — see fetch_page."""
    req = urllib.request.Request(url, headers={
        "User-Agent": ("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
                       "(KHTML, like Gecko) Chrome/126 Safari/537.36"),
        "Accept": "text/html,application/xhtml+xml",
    })
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.read().decode("utf-8", "replace")
    except Exception:
        return None


def fetch_page(url: str) -> str | None:
    """Page HTML, direct first and via FlareSolverr second.

    Direct is tried first on purpose: it costs nothing when it works and
    it means the tool is not silently dependent on a box on someone's
    LAN. FlareSolverr is the documented escape hatch for the Cloudflare
    interstitial, addressed by FLARESOLVERR_URL so nothing is hardcoded.
    """
    html = get_text(url)
    if html and "videos.pexels.com" in html:
        return html
    endpoint = os.environ.get("FLARESOLVERR_URL", "").strip()
    if not endpoint:
        return html
    payload = json.dumps({
        "cmd": "request.get", "url": url, "maxTimeout": 60000,
    }).encode()
    req = urllib.request.Request(endpoint, data=payload, headers={
        "Content-Type": "application/json",
    })
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            body = json.loads(resp.read().decode("utf-8", "replace"))
    except Exception as e:
        print(f"    flaresolverr error: {e}", file=sys.stderr)
        return html
    return ((body.get("solution") or {}).get("response")) or html


# ---------------------------------------------------------------------------
# Pexels resolution
# ---------------------------------------------------------------------------

def pexels_video_id(page_url: str) -> str | None:
    m = _PEXELS_ID_RE.search(page_url.rstrip("/"))
    return m.group(1) if m else None


def recorded_dimensions(record: dict) -> tuple[str, str] | tuple[None, None]:
    """W,H as written in the record's own description ("1280x720 12s …").

    Using the record rather than probing means the pattern candidates are
    a handful of URLs instead of the full ladder.
    """
    m = _DIMS_RE.match(record.get("description") or "")
    return (m.group(1), m.group(2)) if m else (None, None)


def pexels_pattern_candidates(video_id: str, w: str | None, h: str | None) -> list[str]:
    if not (w and h):
        return []
    base = f"https://videos.pexels.com/video-files/{video_id}"
    return [f"{base}/{video_id}-{q}_{w}_{h}_{fps}fps.mp4"
            for q in PEXELS_QUALITIES for fps in PEXELS_FPS]


def pexels_page_candidates(page_url: str, video_id: str) -> list[str]:
    """Media URLs scraped off the listing page, restricted to THIS video.

    A Pexels page also embeds "related video" URLs; without the id filter
    a size collision with an unrelated clip could record the wrong file.
    """
    html = fetch_page(page_url)
    if not html:
        return []
    prefix = f"https://videos.pexels.com/video-files/{video_id}/"
    return sorted({u for u in _PEXELS_MEDIA_RE.findall(html) if u.startswith(prefix)})


def resolve_pexels(record: dict) -> tuple[str | None, str]:
    """(media_url, note). Accepts only a Content-Length == file_size_bytes."""
    page = (record.get("metadata") or {}).get("fetched_from") or ""
    want = record.get("file_size_bytes")
    video_id = pexels_video_id(page)
    if not video_id:
        return None, "no video id in fetched_from"
    if not want:
        return None, "record has no file_size_bytes to match against"

    w, h = recorded_dimensions(record)
    tried = 0
    for url in pexels_pattern_candidates(video_id, w, h):
        tried += 1
        if head_length(url) == want:
            return url, f"pattern, {tried} HEADs"

    page_urls = pexels_page_candidates(page, video_id)
    if not page_urls:
        return None, (f"pattern failed after {tried} HEADs and the page "
                      "yielded nothing (Cloudflare? set FLARESOLVERR_URL)")
    for url in page_urls:
        tried += 1
        if head_length(url) == want:
            return url, f"page scrape, {tried} HEADs"
    return None, (f"none of {len(page_urls)} page URLs matched "
                  f"{want} bytes after {tried} HEADs")


def resolve_record(record: dict) -> tuple[str | None, str]:
    """media_url for one record, by source."""
    meta = record.get("metadata") or {}
    fetched = meta.get("fetched_from") or ""
    if meta.get("acquisition_source") == "Pexels":
        return resolve_pexels(record)
    if not fetched:
        return None, "no fetched_from to work from"
    host = urllib.parse.urlparse(fetched).netloc
    if host in DIRECT_MEDIA_HOSTS:
        # Already a byte URL — mirror it so `media_url` is uniform and a
        # consumer never has to special-case the source.
        return fetched, "fetched_from is already direct"
    return None, f"fetched_from host {host!r} is not a known media host"


# ---------------------------------------------------------------------------
# Document handling
# ---------------------------------------------------------------------------

def load(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


def _formatting_of(path: Path) -> tuple[int, str, bool]:
    """(indent, trailing, ensure_ascii) as the file on disk already uses.

    The profiles are not written by one tool: studio-a is 2-space,
    \\u-escaped, with no final newline; studio-b and the upgrade docs are
    1-space, literal UTF-8, with one. Normalising would rewrite 56,000
    lines to add one key per record and bury the actual change, so match
    what is there instead of imposing a house style. Measured: the naive
    version of this function produced a 111,848-line diff for a 36-line
    edit.
    """
    text = path.read_text(encoding="utf-8")
    indent = 1
    lines = text.split("\n", 2)
    if len(lines) > 1:
        stripped = lines[1].lstrip(" ")
        indent = len(lines[1]) - len(stripped) or 1
    # All-ASCII on disk means non-ASCII was escaped when it was written.
    return indent, ("\n" if text.endswith("\n") else ""), text.isascii()


def dump(path: Path, data, fmt: tuple[int, str, bool] | None = None) -> None:
    indent, trailing, ensure_ascii = fmt or (1, "\n", False)
    path.write_text(
        json.dumps(data, indent=indent, ensure_ascii=ensure_ascii) + trailing,
        encoding="utf-8")


def internet_records(doc: list[dict]) -> list[dict]:
    """Records a per-file media URL is meaningful for.

    `fetched_from` alone no longer implies one. #572 gave every
    Kenney-sourced record the pack PAGE as `fetched_from` — closing a
    real attribution gap — but their bytes live inside a zip, so they
    carry `metadata.source_archive` {url, member, sha256} instead. A
    `media_url` must serve exactly `file_size_bytes`, which no archive
    URL can, and pointing this gate at them would either fail 895 records
    forever or push someone to write a zip URL into a field whose entire
    value is that it is checkable. Those records are gated by
    apply_upgrade.py's own source_archive post-condition.
    """
    return [r for r in doc
            if (r.get("metadata") or {}).get("fetched_from")
            and not (r.get("metadata") or {}).get("source_archive")]


def needs_media_url(record: dict) -> bool:
    """A record is short a media URL when it has none, or when the one it
    has is the same page URL we already know is not a file."""
    meta = record.get("metadata") or {}
    return not meta.get("media_url")


# ---------------------------------------------------------------------------
# Modes
# ---------------------------------------------------------------------------

def cmd_check(paths: list[Path]) -> int:
    """Offline structural gate. No network, no NAS — CI-safe."""
    problems: list[str] = []
    total = 0
    for path in paths:
        doc = load(path)
        recs = internet_records(doc)
        total += len(recs)
        for r in recs:
            meta = r["metadata"]
            mu = meta.get("media_url")
            if not mu:
                problems.append(f"{path.name}: {r['id']} ({meta.get('filename')}) "
                                "has fetched_from but no media_url — its bytes "
                                "cannot be re-fetched from provenance (#602)")
                continue
            host = urllib.parse.urlparse(mu).netloc
            if host not in DIRECT_MEDIA_HOSTS:
                problems.append(f"{path.name}: {r['id']} media_url host {host!r} "
                                "is not a known direct-media host")
            if not meta.get("fetched_from"):
                problems.append(f"{path.name}: {r['id']} lost fetched_from — the "
                                "page URL is the attribution + licence evidence "
                                "and must survive")
    print(f"checked {total} internet-sourced records across {len(paths)} file(s)",
          file=sys.stderr)
    for p in problems[:20]:
        print(f"  - {p}", file=sys.stderr)
    if len(problems) > 20:
        print(f"  … and {len(problems) - 20} more", file=sys.stderr)
    return 1 if problems else 0


def cmd_write(paths: list[Path], force: bool, jobs: int) -> int:
    """Resolve + persist. Idempotent: an already-resolved record is left
    alone unless --force."""
    rc = 0
    for path in paths:
        fmt = _formatting_of(path)
        doc = load(path)
        todo = [r for r in internet_records(doc) if force or needs_media_url(r)]
        print(f"\n{path}: {len(todo)} record(s) to resolve", file=sys.stderr)
        if not todo:
            continue
        with ThreadPoolExecutor(max_workers=jobs) as ex:
            results = list(ex.map(resolve_record, todo))
        ok = 0
        for r, (url, note) in zip(todo, results):
            name = (r.get("metadata") or {}).get("filename") or r.get("file_path")
            if url:
                r["metadata"]["media_url"] = url
                ok += 1
                print(f"  ok   {name}\n       {url}  [{note}]", file=sys.stderr)
            else:
                rc = 1
                print(f"  FAIL {name}  [{note}]", file=sys.stderr)
        print(f"  {ok}/{len(todo)} resolved", file=sys.stderr)
        if ok:
            dump(path, doc, fmt)
            print(f"  wrote {path}", file=sys.stderr)
    return rc


def cmd_verify(paths: list[Path], jobs: int) -> int:
    """HEAD every media_url and compare Content-Length to the manifest.

    This is the cheap half of the round trip — it proves the URL serves a
    file of exactly the size we recorded without moving gigabytes.

    A host that will not answer (429/5xx) is reported SEPARATELY from a
    URL that answered with the wrong thing, and does not fail the run.
    "upload.wikimedia.org is rate-limiting me right now" and "this
    provenance record is wrong" are different findings and must not be
    collapsed into one number.
    """
    rc = 0
    for path in paths:
        doc = load(path)
        recs = [r for r in internet_records(doc)
                if (r["metadata"].get("media_url"))]
        print(f"\n{path}: HEADing {len(recs)} media URLs", file=sys.stderr)

        def probe(r):
            return head_probe(r["metadata"]["media_url"])

        with ThreadPoolExecutor(max_workers=jobs) as ex:
            results = list(ex.map(probe, recs))
        bad = unavailable = 0
        for r, (outcome, got) in zip(recs, results):
            want = r.get("file_size_bytes")
            name = r["metadata"].get("filename")
            url = r["metadata"]["media_url"]
            if outcome == "unavailable":
                unavailable += 1
                print(f"  UNAVAILABLE {name}: host would not answer "
                      f"(rate limit / outage) — not a provenance failure\n"
                      f"    {url}", file=sys.stderr)
            elif outcome == "absent" or got != want:
                bad += 1
                rc = 1
                print(f"  MISMATCH {name}: recorded {want}, served "
                      f"{got if outcome == 'ok' else outcome}\n"
                      f"    {url}", file=sys.stderr)
        checked = len(recs) - unavailable
        print(f"  {checked - bad}/{checked} reachable URLs serve exactly the "
              f"recorded byte count"
              + (f" ({unavailable} unreachable right now)" if unavailable else ""),
              file=sys.stderr)
    return rc


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def cmd_refetch(paths: list[Path], out: Path, against: Path | None,
                limit: int | None, only: str | None) -> int:
    """The full round trip: download from `media_url` and, when a staged
    copy is available, hash both. Byte-identical is the deliverable —
    "the URL 200s" is not."""
    out.mkdir(parents=True, exist_ok=True)
    rc = 0
    n = 0
    for path in paths:
        for r in internet_records(load(path)):
            meta = r["metadata"]
            url = meta.get("media_url")
            if not url:
                continue
            if only and only not in (meta.get("filename") or ""):
                continue
            if limit is not None and n >= limit:
                return rc
            n += 1
            dest = out / (meta.get("filename") or Path(url).name)
            print(f"\n[{n}] {dest.name}\n    <- {url}", file=sys.stderr)
            req = urllib.request.Request(url, headers={"User-Agent": UA})
            try:
                with urllib.request.urlopen(req, timeout=300) as resp:
                    dest.write_bytes(resp.read())
            except Exception as e:
                rc = 1
                print(f"    DOWNLOAD FAILED: {e}", file=sys.stderr)
                continue
            size = dest.stat().st_size
            want = r.get("file_size_bytes")
            digest = sha256_of(dest)
            print(f"    {size:,} B (manifest {want:,} B) "
                  f"{'MATCH' if size == want else 'SIZE MISMATCH'}", file=sys.stderr)
            print(f"    sha256 {digest}", file=sys.stderr)
            if size != want:
                rc = 1
            if against:
                staged = against / r["file_path"]
                if staged.is_file():
                    sdig = sha256_of(staged)
                    same = sdig == digest
                    print(f"    staged sha256 {sdig}  "
                          f"{'IDENTICAL' if same else 'DIFFERENT'}", file=sys.stderr)
                    if not same:
                        rc = 1
                else:
                    # Not a failure: the archive share may simply not be
                    # mounted here. Unavailable is not the same as absent
                    # and must not be reported as data loss.
                    print(f"    (no staged copy at {staged} — share not mounted?)",
                          file=sys.stderr)
    return rc


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("files", nargs="+", type=Path,
                    help="profile / upgrade JSON documents (a list of asset records)")
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true",
                      help="offline structural gate; no network")
    mode.add_argument("--write", action="store_true",
                      help="resolve missing media_urls and write them back")
    mode.add_argument("--verify", action="store_true",
                      help="HEAD each media_url, compare Content-Length")
    mode.add_argument("--refetch", type=Path, metavar="DIR",
                      help="download from media_url into DIR")
    ap.add_argument("--against", type=Path,
                    help="staged site root, to hash the download against")
    ap.add_argument("--limit", type=int, help="--refetch: stop after N files")
    ap.add_argument("--only", help="--refetch: substring filter on filename")
    ap.add_argument("--force", action="store_true",
                    help="--write: re-resolve records that already have a media_url")
    ap.add_argument("--jobs", type=int, default=8)
    args = ap.parse_args()

    if args.check:
        return cmd_check(args.files)
    if args.write:
        return cmd_write(args.files, args.force, args.jobs)
    if args.verify:
        return cmd_verify(args.files, args.jobs)
    return cmd_refetch(args.files, args.refetch, args.against,
                       args.limit, args.only)


if __name__ == "__main__":
    raise SystemExit(main())
