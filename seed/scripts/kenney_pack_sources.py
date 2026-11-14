#!/usr/bin/env python3
"""
Resolve, record and verify the DOWNLOADABLE provenance of the Kenney
packs the seed dataset draws from (#572).

WHY THIS EXISTS
---------------
Every image in the kenney-hq pool traces back to a file inside the
"Kenney Game Assets All-in-1 3.6.0" bundle sitting on the archive share.
That is fine while the share is mounted and useless the moment it is
not: the bundle is an itch.io purchase, not something a rebuild can GET.
So the ~90% of the library that is Kenney-sourced had, until now, no
internet provenance at all — the same hole #602 closed for the 30 Pexels
videos, just larger and quieter, because verify-don't-copy never fails
while the mount is up.

Kenney also publishes every pack INDIVIDUALLY as a free CC0 zip. Those
zips are byte-identical to the All-in-1 copies (asserted by `verify`),
which makes them a real re-fetch path:

    kenney.nl/assets/<slug>            the page   -> metadata.fetched_from
    kenney.nl/media/.../<pack>.zip     the bytes  -> metadata.source_archive.url
    <path inside the zip>                         -> metadata.source_archive.member
    sha256 of that member's bytes                 -> metadata.source_archive.sha256

WHY NOT `media_url`
-------------------
#602's `media_url` means "a URL that serves EXACTLY file_size_bytes of
the record's own bytes" — populate_archive.py checks the served length
against the manifest and refuses a mismatch. A zip serves the whole
pack, so putting a zip URL in `media_url` would make every re-fetch fail
that check, or worse, disable it. A member of an archive is a different
shape of provenance and gets a different key, with the per-member sha256
doing the job the byte count does for a direct URL. Same principle:
record evidence, not a plausible-looking string.

The zip URL carries a content hash Kenney regenerates when a pack is
updated (`.../f651646eab-1718203990/kenney_ui-pack.zip`), so it is
resolved from the page rather than constructed, and re-resolvable when
it moves. The recorded `zip_sha256` is what says whether a re-resolve
got the same pack or a newer one.

Usage
-----
    # resolve pages + zip URLs, verify sizes, write the doc (needs network)
    python3 kenney_pack_sources.py resolve --packs-from-recipes

    # offline gate: every pack a recipe names is in the doc
    python3 kenney_pack_sources.py check

    # prove the zips reproduce the bundle byte for byte (network + share)
    python3 kenney_pack_sources.py verify --pack-root "<All-in-1 root>" \\
        [--sample 40] [--only "UI assets/UI Pack"]
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

DOC = Path(__file__).resolve().parents[1] / "upgrades" / "kenney-pack-sources.json"

UA = ("artist-alley-seed-fetcher/2.0 "
      "(+https://github.com/Artist-Alley-Org/artist-alley)")

PAGE_BASE = "https://kenney.nl/assets/"

# Pack directory name inside the All-in-1 bundle -> kenney.nl page slug,
# for the cases where slugifying the directory name does not land on the
# right page. Data, not cleverness: when `resolve` 404s, add a line.
SLUG_OVERRIDES: dict[str, str] = {
    "UI Pack - Sci-fi": "ui-pack-sci-fi",
    "UI Pack - Adventure": "ui-pack-adventure",
    "Platformer Characters 1": "platformer-characters",
}

# Bundle directories that kenney.nl does NOT publish as a standalone pack
# — "Animated Characters Bundle", "Character Pack" and "Weapon Pack" are
# All-in-1-only compilations. They are excluded from the recipes on
# purpose: a record whose bytes exist only inside a paid bundle cannot be
# re-fetched from provenance, which is the hole this whole shape exists
# to close. If a pack is not downloadable, it does not go in the dataset.
NOT_PUBLISHED_STANDALONE = frozenset({
    "3D assets/Animated Characters Bundle",
    "2D assets/Character Pack",
    "3D assets/Weapon Pack",
})

_ZIP_RE = re.compile(r"href='(https://kenney\.nl/media/pages/assets/[^']+\.zip)'")


def slugify(name: str) -> str:
    return re.sub(r"-+", "-", re.sub(r"[^a-z0-9]+", "-", name.lower())).strip("-")


def page_for(pack_dir: str) -> str:
    """kenney.nl page URL for an All-in-1 pack path ("UI assets/UI Pack")."""
    leaf = pack_dir.rsplit("/", 1)[-1]
    return PAGE_BASE + SLUG_OVERRIDES.get(leaf, slugify(leaf))


def _get(url: str, timeout: int = 60) -> bytes:
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read()


def _head_length(url: str, timeout: int = 60) -> int | None:
    req = urllib.request.Request(url, headers={"User-Agent": UA}, method="HEAD")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            n = resp.headers.get("Content-Length")
            return int(n) if n else None
    except Exception:
        return None


# ---------------------------------------------------------------------------
# #1514: the signed download-link transport.
#
# Most records name a direct pack zip (`kenney.nl/media/pages/assets/...zip`)
# and carry no `transport` key; that path is unchanged. A record whose pack
# is served only through Kenney's download button instead records
#
#     source_archive.url            https://kenney.nl/download-link/<hex>
#     source_archive.transport      "kenney-download-link-json"
#     source_archive.archive_bytes  exact length of the zip it resolves to
#     source_archive.archive_sha256 sha256 of that whole zip
#     source_archive.member/sha256  as for every other record
#
# The endpoint is the stable locator. It does NOT serve the zip: a GET
# returns JSON `{"url": <short-lived signed zip URL>}`. The signed URL
# expires, so it is never recorded, logged or put in an error message; the
# archive hash is what proves a fresh link reached the same pack, and it is
# checked on the whole zip BEFORE anything is extracted from it.
# ---------------------------------------------------------------------------

TRANSPORT_SIGNED_LINK = "kenney-download-link-json"
_DOWNLOAD_LINK_RE = re.compile(r"\Ahttps://kenney\.nl/download-link/([0-9a-f]{2,256})\Z")
# The endpoint's hex is the ASCII asset path it belongs to ("assets/<slug>").
# Kenney answers the endpoint only with that asset page as Referer (HTTP 403
# without it, observed 2026-11-13), so the Referer is DERIVED from the hex,
# never taken from anywhere else, and only a plain asset slug is accepted.
_ASSET_PATH_RE = re.compile(r"\Aassets/([a-z0-9]+(?:-[a-z0-9]+)*)\Z")
# Hosts a signed zip URL (and any redirect on the way to it) may name. Only
# Kenney's own host: anything else fails closed and needs a deliberate edit.
SIGNED_ZIP_HOSTS = frozenset({"kenney.nl"})
LINK_RESPONSE_MAX_BYTES = 64 * 1024
ARCHIVE_MAX_BYTES = 512 * 1024 * 1024
_HEX64 = re.compile(r"\A[0-9a-f]{64}\Z")


class ArchiveError(Exception):
    """A source archive that cannot be fetched or authenticated. The message
    never contains a signed URL."""


def _host_ok(url: str, hosts: frozenset[str]) -> bool:
    from urllib.parse import urlsplit
    try:
        p = urlsplit(url)
        port = p.port
    except ValueError:
        return False
    return (p.scheme == "https" and p.hostname in hosts and port in (None, 443)
            and p.username is None and p.password is None)


class _HostLockedRedirect(urllib.request.HTTPRedirectHandler):
    """Follow a redirect only to an allowed https host, so a link cannot walk
    the download off Kenney before the final-URL check would see it."""

    def __init__(self, hosts: frozenset[str]):
        super().__init__()
        self.hosts = hosts

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if not _host_ok(newurl, self.hosts):
            raise ArchiveError(f"refused a redirect (HTTP {code}) to a host "
                               f"outside {sorted(self.hosts)}")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def _default_open(req, timeout):
    opener = urllib.request.build_opener(_HostLockedRedirect(SIGNED_ZIP_HOSTS))
    return opener.open(req, timeout=timeout)


def _read_bounded(resp, limit: int, what: str) -> bytes:
    declared = resp.headers.get("Content-Length") if resp.headers else None
    if declared is not None:
        try:
            if int(declared) > limit:
                raise ArchiveError(f"{what} declares {int(declared):,} B, over the "
                                   f"{limit:,} B bound")
        except ValueError:
            raise ArchiveError(f"{what} has a malformed Content-Length") from None
    blob = resp.read(limit + 1)
    if len(blob) > limit:
        raise ArchiveError(f"{what} is over the {limit:,} B bound")
    return blob


def asset_page_for_download_link(url: str) -> str:
    """The kenney.nl asset page a download-link endpoint belongs to, decoded
    strictly from its hex path. Raises ArchiveError for anything else."""
    m = _DOWNLOAD_LINK_RE.match(url or "")
    if not m or len(m.group(1)) % 2:
        raise ArchiveError("not a https://kenney.nl/download-link/<hex> endpoint")
    try:
        path = bytes.fromhex(m.group(1)).decode("ascii")
    except (ValueError, UnicodeDecodeError):
        raise ArchiveError("download-link hex does not decode to an ASCII path") from None
    slug = _ASSET_PATH_RE.match(path)
    if not slug:
        raise ArchiveError("download-link hex does not decode to assets/<slug>")
    return PAGE_BASE + slug.group(1)


def _fetch(url: str, what: str, limit: int, timeout: int, opener, accept: str,
           referer: str | None = None) -> bytes:
    headers = {"User-Agent": UA, "Accept": accept}
    if referer is not None:
        if not (_host_ok(referer, SIGNED_ZIP_HOSTS) and referer.startswith(PAGE_BASE)):
            raise ArchiveError("refused a Referer that is not a kenney.nl asset page")
        headers["Referer"] = referer
    req = urllib.request.Request(url, headers=headers)
    try:
        with opener(req, timeout) as resp:
            if not _host_ok(resp.geturl(), SIGNED_ZIP_HOSTS):
                raise ArchiveError(f"{what} ended on a host outside {sorted(SIGNED_ZIP_HOSTS)}")
            return _read_bounded(resp, limit, what)
    except ArchiveError:
        raise
    except urllib.error.HTTPError as e:
        hint = " (the signed link may have expired)" if e.code in (401, 403, 404, 410) \
            and what == "signed zip" else ""
        raise ArchiveError(f"{what} request failed: HTTP {e.code}{hint}") from None
    except Exception as e:  # URLError, timeout, TLS: name the kind, never the URL
        raise ArchiveError(f"{what} request failed: {type(e).__name__}") from None


def validate_signed_archive(sa: dict) -> None:
    """Offline shape check for a signed-link `source_archive`. Raises."""
    if sa.get("transport") != TRANSPORT_SIGNED_LINK:
        raise ArchiveError(f"transport is {sa.get('transport')!r}, not {TRANSPORT_SIGNED_LINK!r}")
    if not isinstance(sa.get("url"), str):
        raise ArchiveError("url is not a https://kenney.nl/download-link/<hex> endpoint")
    asset_page_for_download_link(sa["url"])
    n = sa.get("archive_bytes")
    if not isinstance(n, int) or isinstance(n, bool) or not 0 < n <= ARCHIVE_MAX_BYTES:
        raise ArchiveError("archive_bytes must be a positive byte count within the bound")
    for k in ("archive_sha256", "sha256"):
        if not isinstance(sa.get(k), str) or not _HEX64.match(sa[k]):
            raise ArchiveError(f"{k} is not a sha256")
    if not isinstance(sa.get("member"), str) or not sa["member"]:
        raise ArchiveError("member is missing")


def resolve_download_link(url: str, *, page: str | None = None, timeout: int = 60,
                          opener=None) -> str:
    """GET the endpoint and return the fresh signed zip URL. Raises.

    The request carries the endpoint's own asset page as Referer (derived from
    the hex). `page`, when given (a record's `fetched_from`), must be that same
    page: an endpoint and a page that disagree are refused before any request."""
    derived = asset_page_for_download_link(url)
    if page is not None and page != derived:
        raise ArchiveError("the record's asset page does not match the download-link endpoint")
    body = _fetch(url, "download-link endpoint", LINK_RESPONSE_MAX_BYTES, timeout,
                  opener or _default_open, "application/json", referer=derived)
    try:
        doc = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        raise ArchiveError("download-link response is not JSON") from None
    signed = doc.get("url") if isinstance(doc, dict) else None
    if not isinstance(signed, str) or not signed:
        raise ArchiveError("download-link response has no `url` string")
    if not _host_ok(signed, SIGNED_ZIP_HOSTS):
        raise ArchiveError(f"download-link returned a URL that is not https on "
                           f"{sorted(SIGNED_ZIP_HOSTS)}")
    return signed


def fetch_signed_archive(sa: dict, *, page: str | None = None, timeout: int = 600,
                         opener=None) -> bytes:
    """The whole zip a signed-link `source_archive` names, authenticated by
    length and sha256 before it is returned. Raises ArchiveError. `page` is
    the record's `fetched_from` where the caller has it; see
    `resolve_download_link`. The signed zip GET sends no Referer."""
    validate_signed_archive(sa)
    signed = resolve_download_link(sa["url"], page=page, opener=opener)
    blob = _fetch(signed, "signed zip", sa["archive_bytes"], timeout,
                  opener or _default_open, "application/zip")
    if len(blob) != sa["archive_bytes"]:
        raise ArchiveError(f"signed zip is {len(blob):,} B, recorded "
                           f"{sa['archive_bytes']:,} B; the pack changed upstream")
    got = hashlib.sha256(blob).hexdigest()
    if got != sa["archive_sha256"]:
        raise ArchiveError(f"archive sha256 mismatch: recorded {sa['archive_sha256'][:12]}..., "
                           f"served {got[:12]}...; the pack changed upstream")
    return blob


def resolve_one(pack_dir: str) -> dict:
    """Page + zip URL + served byte count for one pack. Raises on failure."""
    page = page_for(pack_dir)
    html = _get(page).decode("utf-8", "replace")
    m = _ZIP_RE.search(html)
    if not m:
        raise RuntimeError(f"no .zip link on {page}")
    zip_url = m.group(1)
    length = _head_length(zip_url)
    if not length:
        raise RuntimeError(f"no Content-Length for {zip_url}")
    return {"pack": pack_dir, "page": page, "zip_url": zip_url,
            "zip_bytes": length, "license": "CC0 1.0",
            "attribution": "Kenney (kenney.nl)"}


def load_doc() -> dict[str, dict]:
    if not DOC.is_file():
        return {}
    return {e["pack"]: e for e in json.loads(DOC.read_text(encoding="utf-8"))}


def save_doc(entries: dict[str, dict]) -> None:
    DOC.write_text(
        json.dumps([entries[k] for k in sorted(entries)], indent=1,
                   ensure_ascii=False) + "\n", encoding="utf-8")


def recipe_packs() -> list[str]:
    """Every pack directory named by the balance recipes."""
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from studio_balance import TEAM_RECIPES  # noqa: E402
    packs: list[str] = []
    for rules in TEAM_RECIPES.values():
        for r in rules:
            if r["pack"] in NOT_PUBLISHED_STANDALONE:
                raise SystemExit(
                    f"FATAL: recipe names {r['pack']!r}, which kenney.nl does "
                    "not publish standalone — its bytes would be "
                    "unreconstructable without the paid bundle.")
            if r["pack"] not in packs:
                packs.append(r["pack"])
    return packs


def cmd_resolve(args: argparse.Namespace) -> int:
    packs = args.pack or (recipe_packs() if args.packs_from_recipes else [])
    if not packs:
        print("error: give --pack <dir> or --packs-from-recipes", file=sys.stderr)
        return 2
    entries = load_doc()
    ok = fail = skip = 0
    for p in packs:
        if p in entries and not args.force:
            skip += 1
            continue
        try:
            e = resolve_one(p)
        except Exception as exc:
            print(f"  FAIL {p}: {exc}", file=sys.stderr)
            fail += 1
            continue
        entries[p] = e
        ok += 1
        print(f"  ok   {p} -> {e['zip_url']} ({e['zip_bytes']:,} B)",
              file=sys.stderr)
    if not args.dry_run:
        save_doc(entries)
        print(f"wrote {DOC} ({len(entries)} packs)", file=sys.stderr)
    print(f"resolved {ok}, skipped {skip}, failed {fail}", file=sys.stderr)
    return 1 if fail else 0


def cmd_check(args: argparse.Namespace) -> int:
    """Offline gate — no network, no share."""
    entries = load_doc()
    missing = [p for p in recipe_packs() if p not in entries]
    bad = [e["pack"] for e in entries.values()
           if not e.get("zip_url", "").startswith("https://kenney.nl/")
           or not e.get("zip_bytes")]
    for p in missing:
        print(f"  MISSING provenance for pack: {p}", file=sys.stderr)
    for p in bad:
        print(f"  MALFORMED entry: {p}", file=sys.stderr)
    print(f"{len(entries)} packs recorded, {len(missing)} missing, "
          f"{len(bad)} malformed", file=sys.stderr)
    return 1 if (missing or bad) else 0


def cmd_verify(args: argparse.Namespace) -> int:
    """Download each zip and prove its members match the All-in-1 bundle.

    This is the claim the whole provenance shape rests on: that the free
    per-pack zip and the paid bundle ship the same bytes. Asserted, not
    assumed.
    """
    entries = load_doc()
    if args.only:
        entries = {k: v for k, v in entries.items() if k in args.only}
    root = args.pack_root
    total_checked = total_match = total_absent = total_differ = 0
    for pack, e in sorted(entries.items()):
        pack_dir = root / pack
        if not pack_dir.is_dir():
            print(f"  SKIP {pack}: not in the bundle at {root}", file=sys.stderr)
            continue
        try:
            blob = _get(e["zip_url"], timeout=600)
        except Exception as exc:
            print(f"  FAIL {pack}: download: {exc}", file=sys.stderr)
            total_differ += 1
            continue
        if len(blob) != e["zip_bytes"]:
            print(f"  FAIL {pack}: served {len(blob):,} B, doc says "
                  f"{e['zip_bytes']:,} B — pack was updated upstream",
                  file=sys.stderr)
            total_differ += 1
            continue
        import io
        z = zipfile.ZipFile(io.BytesIO(blob))
        members = [n for n in z.namelist() if not n.endswith("/")]
        members.sort()
        if args.sample and len(members) > args.sample:
            step = max(1, len(members) // args.sample)
            members = members[::step][:args.sample]
        checked = match = absent = differ = 0
        for n in members:
            p = pack_dir / n
            if not p.is_file():
                absent += 1
                continue
            checked += 1
            if hashlib.sha256(z.read(n)).hexdigest() == \
               hashlib.sha256(p.read_bytes()).hexdigest():
                match += 1
            else:
                differ += 1
                if differ <= 2:
                    print(f"    DIFFERS: {pack}/{n}", file=sys.stderr)
        print(f"  {pack}: {match}/{checked} members byte-identical"
              f"{f', {absent} not in bundle' if absent else ''}", file=sys.stderr)
        total_checked += checked
        total_match += match
        total_absent += absent
        total_differ += differ
    print(f"\n{total_match}/{total_checked} members byte-identical, "
          f"{total_differ} differ, {total_absent} absent from the bundle",
          file=sys.stderr)
    return 0 if total_differ == 0 and total_checked else 1


def cmd_verify_records(args: argparse.Namespace) -> int:
    """Every emitted record's member must exist in the zip with its hash.

    `verify` samples a pack and answers "does the free zip ship the same
    art as the paid bundle?". The answer turned out to be *mostly*: seven
    packs have drifted upstream since All-in-1 3.6.0 was cut, so a
    sampled member can match while a member we actually SHIP does not.
    This checks the exact set of files the dataset references, which is
    the only set whose re-fetchability is a claim we make.

    A mismatch here is not a bug to paper over — it means those bytes
    cannot be reconstructed from the recorded provenance, and the record
    should be dropped from the recipes (studio_balance.SOURCE_EXCLUSIONS)
    rather than shipped with provenance that fails on use.
    """
    import io
    entries = load_doc()
    records = json.loads(args.records.read_text(encoding="utf-8"))
    # ⛔ ANY FAILURE IS NONZERO (#1514). This used to set its exit code from
    # member mismatches alone, so a zip that failed to download or parse
    # printed FAIL and still exited 0, and a provenance gate went green on a
    # pack nobody verified. Every way a record can go unverified is counted
    # below and every one of them fails the run, as does verifying nothing.
    by_url: dict[str, list[dict]] = {}
    malformed: list[str] = []
    skipped = 0
    for i, r in enumerate(records if isinstance(records, list) else ()):
        # A row that is not an object, or whose metadata is present but not
        # an object, is MALFORMED, never "unchecked": counting it as skipped
        # let a broken row ride along with a valid group to exit 0. Only a
        # well-formed row with no source_archive is legitimately skipped.
        if not isinstance(r, dict):
            malformed.append(f"record[{i}] (not a JSON object: {type(r).__name__})")
            continue
        rid = str(r["id"])[:80] if "id" in r else f"record[{i}]"
        meta = r.get("metadata")
        if meta is None:
            skipped += 1
            continue
        if not isinstance(meta, dict):
            malformed.append(f"{rid} (metadata is not a JSON object: {type(meta).__name__})")
            continue
        sa = meta.get("source_archive")
        if sa is None:
            skipped += 1
            continue
        if not (isinstance(sa, dict) and isinstance(sa.get("url"), str) and sa["url"]
                and isinstance(sa.get("member"), str) and sa["member"]
                and isinstance(sa.get("sha256"), str) and _HEX64.match(sa["sha256"])):
            malformed.append(f"{rid} (source_archive needs url, member and a sha256)")
            continue
        if sa.get("transport") is not None and not isinstance(sa["transport"], str):
            malformed.append(f"{rid} (source_archive.transport must be a string, got "
                             f"{type(sa['transport']).__name__})")
            continue
        by_url.setdefault(sa["url"], []).append(r)
    if not isinstance(records, list):
        malformed.append(f"{args.records}: expected a JSON list of records")
    ok = mismatch = absent = unfetched = 0
    offenders: list[str] = []
    failed_archives: list[str] = []
    for url, recs in sorted(by_url.items()):
        sa0 = recs[0]["metadata"]["source_archive"]
        transports = {r["metadata"]["source_archive"].get("transport") for r in recs}
        try:
            if len(transports) != 1:
                raise ArchiveError(f"records disagree on the transport ({sorted(map(str, transports))})")
            if sa0.get("transport") is None:
                blob = _get(url, timeout=600)
            elif sa0["transport"] == TRANSPORT_SIGNED_LINK:
                blob = fetch_signed_archive(
                    sa0, page=(recs[0]["metadata"].get("fetched_from") or None))
            else:
                raise ArchiveError(f"unknown transport {sa0['transport']!r}")
            z = zipfile.ZipFile(io.BytesIO(blob))
        except Exception as exc:
            # `url` is the recorded locator (a direct zip or the stable
            # download-link endpoint), never a signed URL, and ArchiveError
            # text never carries one either.
            why = str(exc) if isinstance(exc, (ArchiveError, zipfile.BadZipFile)) \
                else f"{type(exc).__name__}: {exc}"
            print(f"  FAIL {url}: {why}", file=sys.stderr)
            unfetched += len(recs)
            failed_archives.append(url)
            continue
        names = set(z.namelist())
        for r in recs:
            sa = r["metadata"]["source_archive"]
            member = sa["member"]
            if member not in names:
                absent += 1
                offenders.append(f"{r['metadata'].get('pack','')}{member} (absent)")
                continue
            got = hashlib.sha256(z.read(member)).hexdigest()
            if got == sa["sha256"]:
                ok += 1
            else:
                mismatch += 1
                offenders.append(member)
        print(f"  {url.rsplit('/', 1)[-1]:<44} {len(recs):4d} record(s)",
              file=sys.stderr)
    print(f"\n{ok} record(s) reproduce from their pack zip, {mismatch} hash "
          f"mismatch, {absent} member absent, {unfetched} in {len(failed_archives)} "
          f"archive(s) that could not be fetched or opened, {len(malformed)} malformed, "
          f"{skipped} without a source_archive (not checked)", file=sys.stderr)
    for o in malformed[:25]:
        print(f"  MALFORMED: {o}", file=sys.stderr)
    for o in offenders[:25]:
        print(f"  NOT REPRODUCIBLE: {o}", file=sys.stderr)
    if len(offenders) > 25:
        print(f"  … and {len(offenders) - 25} more", file=sys.stderr)
    failed = bool(offenders or failed_archives or malformed)
    if not failed and ok == 0:
        print("FAIL: no record was verified; an empty selection proves nothing",
              file=sys.stderr)
        return 1
    if failed:
        print("FAIL: not every record reproduces from its recorded provenance",
              file=sys.stderr)
        return 1
    print(f"OK: all {ok} record(s) verified", file=sys.stderr)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    r = sub.add_parser("resolve", help="resolve pages + zip URLs (network)")
    r.add_argument("--pack", action="append", default=None)
    r.add_argument("--packs-from-recipes", action="store_true")
    r.add_argument("--force", action="store_true",
                   help="re-resolve packs already in the doc")
    r.add_argument("--dry-run", action="store_true")

    sub.add_parser("check", help="offline: recipes vs recorded provenance")

    v = sub.add_parser("verify", help="zips reproduce the bundle byte for byte")
    v.add_argument("--pack-root", required=True, type=Path)
    v.add_argument("--sample", type=int, default=40,
                   help="members per pack to hash (0 = all)")
    v.add_argument("--only", action="append", default=None)

    vr = sub.add_parser("verify-records",
                        help="every emitted record's member reproduces from its zip")
    vr.add_argument("--records", required=True, type=Path)

    args = ap.parse_args()
    if args.cmd == "resolve":
        return cmd_resolve(args)
    if args.cmd == "check":
        return cmd_check(args)
    if args.cmd == "verify-records":
        return cmd_verify_records(args)
    return cmd_verify(args)


if __name__ == "__main__":
    raise SystemExit(main())
