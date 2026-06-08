#!/usr/bin/env python3
"""
verify.py — standalone post-seed validation against a running AA instance.

Re-fetches counts via the API and compares against the MANIFEST.json at
a populated site root. Reports any divergence. Useful for:
- Post-restore validation (snapshot restored; does it match the seed?)
- Drift detection (months after seeding, are the seeded entities still present?)
- Apply re-run smoke test (after apply.py finishes, run verify.py
  independently to confirm)

Shares the AAClient + Catalogues machinery with apply.py.

Usage
-----
    python3 verify.py \\
        --site /mnt/blackbox_archives/datasets/artist_alley/site_a \\
        --api http://localhost:8080 \\
        [--admin-pass-env AA_ADMIN_PASSWORD] \\
        [--tolerance 0.1] \\
        [--catalogue path/to/profiles]

Exit codes:
  0 — all checks within tolerance
  1 — divergence beyond tolerance on any entity
  2 — config / network / auth error
"""

from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path
from urllib.parse import urlparse

# Re-use apply.py's machinery
sys.path.insert(0, str(Path(__file__).resolve().parent))
from apply import (  # noqa: E402
    AAClient, APIError, Catalogues, DEFAULT_API,
    DEFAULT_ADMIN_USER, resolve_admin_password,
    site_key_from_path,
)

LOG = logging.getLogger("verify")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--site", required=True, type=Path)
    parser.add_argument("--catalogue", type=Path,
                        default=Path(__file__).resolve().parent.parent / "profiles")
    parser.add_argument("--api", default=DEFAULT_API)
    parser.add_argument("--admin-user", default=DEFAULT_ADMIN_USER)
    g = parser.add_mutually_exclusive_group()
    g.add_argument("--admin-pass")
    g.add_argument("--admin-pass-env", default="AA_ADMIN_PASSWORD")
    g.add_argument("--admin-pass-file", type=Path)
    parser.add_argument("--tolerance", type=float, default=0.1,
                        help="Fractional tolerance for count divergence (default 0.10 = 10%%)")
    parser.add_argument("--verbose", "-v", action="store_true")
    return parser.parse_args()


def count_endpoint(client: AAClient, path: str) -> int | None:
    """Fetch a count from a listing endpoint. Returns None on error."""
    try:
        resp = client.get(path, params={"limit": 1})
    except APIError as e:
        LOG.warning("count fetch failed for %s: %s", path, e)
        return None
    if isinstance(resp, dict):
        return resp.get("total") or resp.get("count") or len(resp.get("items", []))
    if isinstance(resp, list):
        return len(resp)
    return None


def check(label: str, expected: int, actual: int | None, tolerance: float) -> bool:
    if actual is None:
        LOG.warning("  %-15s expected %d, actual UNKNOWN (endpoint error)", label, expected)
        return False
    if expected == 0:
        ok = actual == 0
    else:
        ratio = actual / expected
        ok = ratio >= (1 - tolerance)
    mark = "✓" if ok else "✗"
    LOG.info("  %s %-15s expected %5d  actual %5d", mark, label, expected, actual)
    return ok


def main() -> int:
    args = parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(message)s",
    )

    if not args.site.is_dir():
        LOG.error("--site %s is not a directory", args.site)
        return 2

    site_key = site_key_from_path(args.site)
    try:
        cat = Catalogues.load(args.catalogue, args.site, site_key)
    except Exception as e:
        LOG.error("catalogue load failed: %s", e)
        return 2

    client = AAClient(args.api, dry_run=False)
    LOG.info("authenticating as %s @ %s", args.admin_user, args.api)
    try:
        client.login(args.admin_user, resolve_admin_password(args))
    except APIError as e:
        LOG.error("login failed: %s", e)
        return 2

    LOG.info("verifying site %s against %s (tolerance %.0f%%)",
             args.site.name, args.api, args.tolerance * 100)

    results = [
        check("users",       len(cat.users),       count_endpoint(client, "/admin/users"),  args.tolerance),
        check("teams",       len(cat.teams),       count_endpoint(client, "/teams"),         args.tolerance),
        check("collections", len(cat.collections), count_endpoint(client, "/collections"),   args.tolerance),
        check("fields",      len(cat.fields),      count_endpoint(client, "/fields"),        args.tolerance),
        check("assets",      len(cat.assets),      count_endpoint(client, "/assets"),        args.tolerance),
        check("posts",       len(cat.posts),       count_endpoint(client, "/posts"),         args.tolerance),
    ]

    passed = sum(1 for r in results if r)
    LOG.info("")
    LOG.info("verify: %d/%d checks within tolerance", passed, len(results))
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
