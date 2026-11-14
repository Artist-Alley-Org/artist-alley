#!/usr/bin/env python3
"""
Pin validation for the CI dataset editions (#1514, ADR 0100).

    ci_pins.py <ci-datasets.v1.json> <key> <field>

Prints one format-checked value from seed/contracts/ci-datasets.v1.json, or exits non-zero naming what is
malformed. scripts/ci/public-datasets.sh reads every pin through this, so a malformed pin fails before any
network request. Fields: repo, tag, commit, release_json_sha256, site (the dataset site the release
holds, passed to fetch_release_dataset.py --site) and need_bytes (download + extracted capacity estimate).

Each key has an exact tag shape. Full editions are calendar tags `vYYYY.MM.NN`; the PR CI subset of site_a
(`site_a_sm`) is the same shape with the `-sm` suffix and nothing else. No other suffix, `latest` or
branch name is accepted, and a key cannot borrow another key's shape.
"""

from __future__ import annotations

import json
import re
import sys

SCHEMA = "artist-alley.ci-datasets/v1"
CALVER = r"v[0-9]{4}\.[0-9]{2}\.[0-9]{2}"
KEYS = {
    # key: (dataset site the release holds, exact tag pattern)
    "site_a": ("site_a", CALVER),
    "site_b": ("site_b", CALVER),
    "site_a_sm": ("site_a", CALVER + r"-sm"),
}
FIELDS = {
    "repo": r"Artist-Alley-Org/[A-Za-z0-9][A-Za-z0-9._-]{0,99}",
    "commit": r"[0-9a-f]{40}",
    "release_json_sha256": r"[0-9a-f]{64}",
}


class PinError(Exception):
    """A pin is missing or malformed."""


def entry(pins: dict, key: str) -> dict:
    if pins.get("schema") != SCHEMA:
        raise PinError("unexpected pins schema")
    if key not in KEYS:
        raise PinError(f"unknown dataset key {key!r} (want one of {sorted(KEYS)})")
    s = pins.get("sites", {}).get(key)
    if not isinstance(s, dict):
        raise PinError(f"no pin for {key}")
    return s


def value(pins: dict, key: str, field: str) -> str:
    s = entry(pins, key)
    site, tag_re = KEYS[key]
    if field == "tag":
        v, pat = s.get("tag"), tag_re
    elif field == "site":
        v = s.get("site", key)
        if v != site:
            raise PinError(f"pin {key}.site must be {site!r}")
        return v
    elif field == "need_bytes":
        cap = s.get("capacity", {})
        parts = [cap.get(k) for k in ("download_bytes", "extracted_bytes")] if isinstance(cap, dict) else [None]
        if not all(type(x) is int and x > 0 for x in parts):
            raise PinError(f"pin {key}.capacity is malformed")
        return str(sum(parts))
    elif field in FIELDS:
        v, pat = s.get(field), FIELDS[field]
    else:
        raise PinError(f"unknown field {field!r}")
    if not isinstance(v, str) or not re.fullmatch(pat, v):
        raise PinError(f"pin {key}.{field} is malformed")
    return v


def main(argv=None) -> int:
    a = sys.argv[1:] if argv is None else argv
    if len(a) != 3:
        print("usage: ci_pins.py <ci-datasets.v1.json> <key> <field>", file=sys.stderr)
        return 2
    try:
        with open(a[0], encoding="utf-8") as fh:
            pins = json.load(fh)
        print(value(pins, a[1], a[2]))
    except (OSError, ValueError, PinError) as e:
        print(f"ci_pins: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
