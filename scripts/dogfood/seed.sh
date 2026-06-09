#!/usr/bin/env bash
# scripts/dogfood/seed.sh — populate studio-b via seed/scripts/apply.py.
#
# studio-b ships without a baked-in dataset. The operator picks
# whatever site/ directory they want to seed it with (different
# from the dev stack's data is recommended so federation
# scenarios produce visible cross-stack flow).
#
# Usage:
#   ./scripts/dogfood/seed.sh --site /path/to/site_dir
#
# Notes:
#   - studio-b's API is reachable from the host at
#     https://studio-b.local:9443/api/v1.
#   - apply.py defaults to admin / ArtistAlleyMogul auth — works
#     out of the box because the dogfood profile sets
#     AA_BOOTSTRAP_DEFAULT_ADMIN=1.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

site=""
while [ $# -gt 0 ]; do
    case "$1" in
        --site) site="$2"; shift 2 ;;
        --help|-h)
            sed -n '2,18p' "$0"
            exit 0
            ;;
        *)
            printf 'unknown arg: %s\n' "$1" >&2
            exit 2
            ;;
    esac
done

if [ -z "$site" ]; then
    printf '\033[1;31mERROR:\033[0m --site is required.\n' >&2
    printf 'Pick a site/ directory that contains a MANIFEST.json (the\n' >&2
    printf 'shape apply.py expects — see seed/scripts/apply.py).\n' >&2
    exit 2
fi

if [ ! -f "${site}/MANIFEST.json" ]; then
    printf '\033[1;31mERROR:\033[0m %s/MANIFEST.json not found.\n' "$site" >&2
    exit 2
fi

printf '\033[1;36m==>\033[0m Seeding studio-b from %s\n' "$site"
exec python3 seed/scripts/apply.py \
    --site "$site" \
    --api https://studio-b.local:9443/api/v1
