#!/usr/bin/env python3
"""
Seed Pokemon-card metadata onto already-loaded assets.

Consumes the pre-built `catalog.csv` from /mnt/d/Projects/Snapdex/build/,
which has every Pokemon TCG row enriched with `image_path` pointing at
the matched file under datasets/cards/. Skips the brittle filename-
parsing problem entirely — the catalog has already done the heavy
lifting (16,424 of 17,172 rows matched per match_report.json).

Flow:

  1. Load catalog.csv into memory.
  2. Load every asset's (file_hash, id) from the DB.
  3. For each catalog row that has an image_path:
       - sha256 the file bytes
       - find the asset by hash
       - PUT field values via /api/v1/assets/{id}/fields/{field_id}

Field definitions are created via POST /fields on first run (idempotent
via the existing-code skip path).

Usage:
  ./scripts/seed-pokemon-metadata.py \
      --catalog /mnt/d/Projects/Snapdex/build/catalog.csv \
      --cards-base /mnt/d/Projects/Snapdex \
      [--base-url http://localhost:8088] \
      [--username admin] [--password P@ssw0rd] \
      [--limit N] [--dry-run]

The script is stdlib-only on the HTTP side (urllib) and uses psycopg
or psycopg2 if available for the file_hash → asset_id lookup; falls
back to a docker-exec psql call otherwise.
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import http.cookiejar
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Iterable

# ---------------------------------------------------------------------------
# Field definitions
# ---------------------------------------------------------------------------

# (code, label, type, options-dict-or-None)
FIELDS = [
    ("pokemon_name",         "Pokemon Name",   "text",         {}),
    ("pokemon_hp",           "HP",             "number",       {}),
    ("pokemon_types",        "Types",          "multi_select", {}),
    ("pokemon_subtypes",     "Subtypes",       "multi_select", {}),
    ("pokemon_rarity",       "Rarity",         "text",         {}),
    ("pokemon_set",          "Set",            "text",         {}),
    ("pokemon_series",       "Series",         "text",         {}),
    ("pokemon_artist",       "Artist",         "text",         {}),
    ("pokemon_release_date", "Release Date",   "datetime",     {}),
    ("pokemon_flavor_text",  "Flavor Text",    "longtext",     {}),
]

# Resource type 1 = Photo (set by 00007 baseline seed). Cards are
# uploaded as photos.
PHOTO_RESOURCE_TYPE = 1

# ---------------------------------------------------------------------------
# Filename parsing
# ---------------------------------------------------------------------------

# Underscore pattern: <set>_<lang>(_<region>)?_<num>(_<variant>)?.jpg
# Examples:
#   sv5_en_018_std.jpg            -> id = sv5-18
#   sv3-5_en_001_std.jpg          -> id = sv3-5-1
#   en_us_swsh7_209_glaceon_vmax.jpg -> id = swsh7-209
_UNDERSCORE_RE = re.compile(r"^[a-z0-9_\-]+\.(jpg|jpeg|png|webp)$", re.IGNORECASE)

# Dash pattern: <name>(-<more>)*-<set-code>-<num>.jpg
#   aipom-aquapolis-aq-67.jpg     -> id = aq-67
#   yanma-skyridge-sk-116.jpg     -> id = sk-116
_NUMERIC_RE = re.compile(r"^\d+$")


def derive_csv_id(filename: str) -> str | None:
    """Best-effort parse: take the file basename, strip extension,
    try the two patterns, return CSV id or None.

    The CSV id is `<set-code>-<num-as-int>` (no leading zeros).
    """
    stem = Path(filename).stem.lower()

    # Some files have multiple-word names like
    #   "en_us_swsh11_076_hisuian_zoroark.jpg".
    # The set-code is the first token that follows the language tokens
    # ("en", "en_us") OR is itself the first token if no language.
    # Strategy: split on `_`, find the first token that looks like a
    # set code (contains a digit), take the next pure-numeric token as
    # the card number.

    if "_" in stem:
        toks = stem.split("_")
        # Strip known language prefixes from the front.
        while toks and toks[0] in ("en", "us", "ja", "fr", "de", "es", "it", "ko", "pt", "zh"):
            toks.pop(0)
        if not toks:
            return None
        # Skip leading language combos that snuck through (e.g. "en_us"
        # collapsed already above). Now toks[0] should be the set code.
        set_code = toks[0]
        # The number is the next numeric token. There may be a hyphen
        # in the set code (e.g. "sv3-5") so we don't split further.
        for t in toks[1:]:
            if _NUMERIC_RE.match(t):
                try:
                    num = int(t)
                except ValueError:
                    continue
                return f"{set_code}-{num}"
        return None

    if "-" in stem:
        toks = stem.split("-")
        # Last token = number, second-to-last = set letters.
        if len(toks) >= 2 and _NUMERIC_RE.match(toks[-1]):
            try:
                num = int(toks[-1])
            except ValueError:
                return None
            set_code = toks[-2]
            return f"{set_code}-{num}"

    return None


# ---------------------------------------------------------------------------
# HTTP client (stdlib)
# ---------------------------------------------------------------------------


class APIClient:
    def __init__(self, base_url: str):
        self.base_url = base_url.rstrip("/")
        self.cookie_jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.cookie_jar),
        )

    def request(self, method: str, path: str, body: dict | None = None) -> tuple[int, bytes]:
        url = self.base_url + path
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, method=method, headers=headers)
        try:
            with self.opener.open(req, timeout=15) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def login(self, username: str, password: str) -> None:
        status, body = self.request("POST", "/api/v1/auth/login", {
            "username": username,
            "password": password,
        })
        if status != 200:
            raise RuntimeError(f"login failed: {status} {body.decode(errors='replace')}")

    def post_json(self, path: str, body: dict) -> tuple[int, dict | None]:
        status, raw = self.request("POST", path, body)
        try:
            return status, json.loads(raw.decode("utf-8")) if raw else None
        except json.JSONDecodeError:
            return status, None

    def put_json(self, path: str, body: dict) -> tuple[int, dict | None]:
        status, raw = self.request("PUT", path, body)
        try:
            return status, json.loads(raw.decode("utf-8")) if raw else None
        except json.JSONDecodeError:
            return status, None

    def get_json(self, path: str) -> tuple[int, dict | list | None]:
        status, raw = self.request("GET", path, None)
        try:
            return status, json.loads(raw.decode("utf-8")) if raw else None
        except json.JSONDecodeError:
            return status, None


# ---------------------------------------------------------------------------
# Field setup
# ---------------------------------------------------------------------------


def ensure_fields(client: APIClient) -> dict[str, str]:
    """Create (idempotent) the Pokemon field definitions.
    Returns a map code → field id (UUID)."""
    status, listed = client.get_json("/api/v1/fields?status=active")
    if status != 200 or not isinstance(listed, list):
        raise RuntimeError(f"list fields: {status}")
    existing = {f["code"]: f["id"] for f in listed if "code" in f}

    out: dict[str, str] = {}
    for code, label, ftype, options in FIELDS:
        if code in existing:
            out[code] = existing[code]
            continue
        body = {
            "code": code,
            "label": label,
            "type": ftype,
            "applies_to": [PHOTO_RESOURCE_TYPE],
            "display_group": "Pokemon",
            "options": options or {},
        }
        status, data = client.post_json("/api/v1/fields", body)
        if status not in (200, 201):
            raise RuntimeError(f"create field {code}: {status} {data}")
        if data and "id" in data:
            out[code] = data["id"]
        print(f"  + created field: {code}")
    return out


# ---------------------------------------------------------------------------
# DB helper (psycopg-direct file_hash lookup)
# ---------------------------------------------------------------------------


def load_assets_by_hash() -> dict[str, str]:
    """Return file_hash → asset_id for every live asset.

    Uses psycopg if available; falls back to walking /api/v1/assets
    pages if not. Walking is slower but stdlib-only."""
    try:
        import psycopg  # type: ignore
        dsn = os.environ.get("AA_DSN") or (
            f"host={os.environ.get('AA_DB_HOST', 'localhost')} "
            f"port={os.environ.get('AA_DB_PORT', '5433')} "
            f"user={os.environ.get('AA_DB_USER', 'artist_alley')} "
            f"password={os.environ.get('AA_DB_PASSWORD', '')} "
            f"dbname={os.environ.get('AA_DB_NAME', 'artist_alley')}"
        )
        with psycopg.connect(dsn) as conn, conn.cursor() as cur:
            cur.execute(
                "SELECT file_hash, id FROM assets WHERE file_hash IS NOT NULL AND deleted_at IS NULL"
            )
            return {h: str(i) for h, i in cur.fetchall()}
    except ImportError:
        pass

    # psycopg unavailable — fall back to docker exec of psql. Cheaper
    # than a 440-page API walk.
    import subprocess
    pwd = os.environ.get("AA_DB_PASSWORD") or os.environ.get("POSTGRES_PASSWORD")
    if not pwd:
        # Try .env
        try:
            with open(".env") as f:
                for line in f:
                    if line.startswith("POSTGRES_PASSWORD="):
                        pwd = line.split("=", 1)[1].strip()
                        break
        except FileNotFoundError:
            pass
    if not pwd:
        raise RuntimeError("AA_DB_PASSWORD / POSTGRES_PASSWORD not set; cannot run DB lookup")
    cmd = [
        "docker", "compose", "exec", "-T",
        "-e", f"PGPASSWORD={pwd}",
        "postgres", "psql",
        "-U", "artist_alley", "-d", "artist_alley",
        "-t", "-A", "-F", "|",
        "-c", "SELECT file_hash, id FROM assets WHERE file_hash IS NOT NULL AND deleted_at IS NULL",
    ]
    out = subprocess.run(cmd, capture_output=True, text=True, check=True)
    result: dict[str, str] = {}
    for line in out.stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        if "|" not in line:
            continue
        h, aid = line.split("|", 1)
        result[h] = aid
    return result


# ---------------------------------------------------------------------------
# CSV loader
# ---------------------------------------------------------------------------


def load_csv(path: Path) -> dict[str, dict[str, str]]:
    """Parse the Pokemon TCG CSV, return id → row map.
    Skips rows without an id."""
    rows: dict[str, dict[str, str]] = {}
    with path.open(encoding="utf-8") as f:
        rd = csv.DictReader(f)
        for r in rd:
            rid = (r.get("id") or "").strip().lower()
            if rid:
                rows[rid] = r
    return rows


# ---------------------------------------------------------------------------
# Walker / matcher
# ---------------------------------------------------------------------------


def walk_card_files(root: Path) -> Iterable[Path]:
    """Recursively yield all .jpg/.jpeg/.png/.webp files under root."""
    exts = {".jpg", ".jpeg", ".png", ".webp"}
    for p in root.rglob("*"):
        if p.is_file() and p.suffix.lower() in exts:
            yield p


def sha256_hex(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


# ---------------------------------------------------------------------------
# Value translation
# ---------------------------------------------------------------------------


def parse_list_cell(s: str) -> list[str]:
    """The CSV stores Python-like list literals: ['Psychic'], ['Stage 2'].
    Naive parser since the strings are well-formed."""
    if not s:
        return []
    s = s.strip()
    if not (s.startswith("[") and s.endswith("]")):
        return []
    inner = s[1:-1]
    out: list[str] = []
    for chunk in inner.split(","):
        chunk = chunk.strip()
        if (chunk.startswith("'") and chunk.endswith("'")) or (
            chunk.startswith('"') and chunk.endswith('"')
        ):
            chunk = chunk[1:-1]
        if chunk:
            out.append(chunk)
    return out


def normalize_date(s: str) -> str | None:
    """CSV release_date is "M/D/YYYY". Return ISO 8601 datetime."""
    if not s:
        return None
    try:
        m, d, y = s.split("/")
        return f"{int(y):04d}-{int(m):02d}-{int(d):02d}T00:00:00Z"
    except ValueError:
        return None


def value_payload(field_type: str, csv_value: str) -> dict | None:
    """Build the AssetFieldValueWrite payload for a given field type."""
    if csv_value is None:
        return None
    v = csv_value.strip()
    if not v:
        return None
    if field_type == "text":
        return {"value_text": v, "set_by": "import"}
    if field_type == "longtext":
        return {"value_text": v, "set_by": "import"}
    if field_type == "number":
        try:
            return {"value_num": float(v), "set_by": "import"}
        except ValueError:
            return None
    if field_type == "datetime":
        iso = normalize_date(v)
        if not iso:
            return None
        return {"value_date": iso, "set_by": "import"}
    if field_type == "multi_select":
        opts = parse_list_cell(v)
        if not opts:
            return None
        return {"value_options": opts, "set_by": "import"}
    return None


# Map field code → (csv column, field type, transform?)
FIELD_MAP = {
    "pokemon_name":         ("name",         "text"),
    "pokemon_hp":           ("hp",           "number"),
    "pokemon_types":        ("types",        "multi_select"),
    "pokemon_subtypes":     ("subtypes",     "multi_select"),
    "pokemon_rarity":       ("rarity",       "text"),
    "pokemon_set":          ("set",          "text"),
    "pokemon_series":       ("series",       "text"),
    "pokemon_artist":       ("artist",       "text"),
    "pokemon_release_date": ("release_date", "datetime"),
    "pokemon_flavor_text":  ("flavorText",   "longtext"),
}


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--catalog", default="/mnt/d/Projects/Snapdex/build/catalog.csv")
    ap.add_argument("--cards-base", default="/mnt/d/Projects/Snapdex/datasets",
                    help="image_path column is relative to this directory")
    ap.add_argument("--base-url", default="http://localhost:8088")
    ap.add_argument("--username", default="admin")
    ap.add_argument("--password", default="P@ssw0rd")
    ap.add_argument("--limit", type=int, default=0, help="Process at most N rows (0 = all)")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    catalog_path = Path(args.catalog)
    cards_base = Path(args.cards_base)
    if not catalog_path.is_file():
        print(f"catalog csv not found: {catalog_path}", file=sys.stderr)
        return 2
    if not cards_base.is_dir():
        print(f"cards base dir not found: {cards_base}", file=sys.stderr)
        return 2

    print(f"Loading catalog: {catalog_path}")
    catalog_rows = load_csv(catalog_path)
    rows_with_image = [r for r in catalog_rows.values() if r.get("image_path")]
    print(f"  {len(catalog_rows)} rows; {len(rows_with_image)} have image_path")

    print("Loading existing assets...")
    hash_to_asset = load_assets_by_hash()
    print(f"  {len(hash_to_asset)} assets with file_hash")

    client = APIClient(args.base_url)
    print(f"Logging in as {args.username}...")
    client.login(args.username, args.password)

    print("Ensuring field definitions...")
    field_ids = ensure_fields(client)
    print(f"  {len(field_ids)} fields ready")

    print(f"Walking catalog ...")
    matched = 0
    unmatched_no_asset = 0
    file_not_found = 0
    set_calls = 0
    errors = 0
    seen_assets: set[str] = set()

    for i, row in enumerate(rows_with_image):
        if args.limit and i >= args.limit:
            break
        rel = (row.get("image_path") or "").strip()
        if not rel:
            continue
        path = cards_base / rel
        if not path.is_file():
            file_not_found += 1
            continue
        h = sha256_hex(path)
        asset_id = hash_to_asset.get(h)
        if not asset_id:
            unmatched_no_asset += 1
            continue
        # Skip duplicate work — same hash can land on many catalog
        # rows if duplicates exist on disk.
        if asset_id in seen_assets:
            continue
        seen_assets.add(asset_id)

        matched += 1
        if args.dry_run:
            continue

        for code, (csv_col, ftype) in FIELD_MAP.items():
            fid = field_ids.get(code)
            if not fid:
                continue
            payload = value_payload(ftype, row.get(csv_col, ""))
            if payload is None:
                continue
            status, body = client.put_json(
                f"/api/v1/assets/{asset_id}/fields/{fid}", payload,
            )
            if status not in (200, 201):
                errors += 1
                if errors < 10:
                    print(f"  ! {rel} → {code}: {status} {body}")
                continue
            set_calls += 1

        if matched % 25 == 0:
            print(f"  ... {matched} matched, {set_calls} field values set")

    print("\nSummary:")
    print(f"  Matched and seeded:  {matched}")
    print(f"  Field values set:    {set_calls}")
    print(f"  No asset (file hash not in DB): {unmatched_no_asset}")
    print(f"  Image file missing on disk:    {file_not_found}")
    print(f"  Errors during set:   {errors}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
