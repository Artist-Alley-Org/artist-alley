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
    by_url: dict[str, list[dict]] = {}
    for r in records:
        sa = (r.get("metadata") or {}).get("source_archive") or {}
        if sa.get("url"):
            by_url.setdefault(sa["url"], []).append(r)
    ok = bad = absent = 0
    offenders: list[str] = []
    for url, recs in sorted(by_url.items()):
        try:
            blob = _get(url, timeout=600)
            z = zipfile.ZipFile(io.BytesIO(blob))
        except Exception as exc:
            print(f"  FAIL {url}: {exc}", file=sys.stderr)
            bad += len(recs)
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
                bad += 1
                offenders.append(member)
        print(f"  {url.rsplit('/', 1)[-1]:<44} {len(recs):4d} record(s)",
              file=sys.stderr)
    print(f"\n{ok} record(s) reproduce from their pack zip, {bad} hash "
          f"mismatch, {absent} member absent", file=sys.stderr)
    for o in offenders[:25]:
        print(f"  NOT REPRODUCIBLE: {o}", file=sys.stderr)
    if len(offenders) > 25:
        print(f"  … and {len(offenders) - 25} more", file=sys.stderr)
    return 1 if offenders else 0


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
