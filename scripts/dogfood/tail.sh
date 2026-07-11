#!/usr/bin/env bash
# scripts/dogfood/tail.sh — combined log tail, color-coded per
# instance so federation traffic is easy to read.
#
# studio-a (dev) is GREEN, studio-b is BLUE.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

GREEN=$'\033[0;32m'
BLUE=$'\033[0;34m'
RESET=$'\033[0m'

stamp() {
    local color="$1" tag="$2"
    while IFS= read -r line; do
        printf '%s[%s]%s %s\n' "$color" "$tag" "$RESET" "$line"
    done
}

# Start both follows in background, route to stamp filters.
docker compose logs -f app | stamp "$GREEN" "studio-a" &
A_PID=$!
docker compose logs -f app-b | stamp "$BLUE" "studio-b" &
B_PID=$!

trap 'kill $A_PID $B_PID 2>/dev/null || true' EXIT INT TERM

wait
