#!/usr/bin/env bash
# scripts/dogfood/down.sh — stop studio-b, leave volumes intact.
#
# Targets only the dogfood-profile services so the dev stack
# keeps running.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

printf '\033[1;36m==>\033[0m Stopping studio-b (volumes preserved)\n'
docker compose stop nginx-b app-b postgres-b
