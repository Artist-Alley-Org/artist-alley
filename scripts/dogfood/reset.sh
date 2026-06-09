#!/usr/bin/env bash
# scripts/dogfood/reset.sh — destructive: nuke studio-b's
# volumes so the next up.sh starts from a fresh DB + storage.
#
# Does NOT touch the dev stack's volumes. Requires the dogfood
# services to be stopped first (down.sh).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }

read -r -p "This will delete ALL studio-b data. Continue? (yes/no) " ack
if [ "$ack" != "yes" ]; then
    echo "aborted."
    exit 1
fi

step "Stopping studio-b if running"
docker compose stop nginx-b app-b postgres-b 2>/dev/null || true

step "Removing studio-b containers"
docker compose rm -f nginx-b app-b postgres-b 2>/dev/null || true

step "Removing studio-b volumes"
docker volume rm artist-alley_postgres-b-data 2>/dev/null || true
docker volume rm artist-alley_aa-storage-b 2>/dev/null || true

step "Done. Next: ./scripts/dogfood/up.sh"
