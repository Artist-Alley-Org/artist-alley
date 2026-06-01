#!/usr/bin/env bash
# scripts/sprite-batch-smoke.sh
#
# End-to-end smoke for the sprite-tool batch API. Verifies that
# every operation the SpriteToolPanel exposes is reachable over
# HTTP — so an external CLI / cron / federation peer can drive the
# same flows without needing the frontend.
#
# What's covered:
#   * Login (cookie session)
#   * GET  /assets/{id}/companions             (read sprite frames)
#   * POST /assets/{id}/companions             (persist edited frames)
#   * DELETE /assets/{id}/companions/{cid}     (detach companion)
#   * GET  /assets/{id}/alternates             (list variants)
#   * POST /assets/{id}/alternates             (palette swap output)
#   * GET  /assets/{id}/alternates/{aid}       (download variant)
#   * DELETE /assets/{id}/alternates/{aid}     (remove variant)
#
# What's NOT covered (deliberately): the export encoders (GIF /
# packed sheet / PNG zip). Those run entirely in the browser. A
# headless CLI replicates them with any Node / Go encoder; the
# source PNG + companion JSON + alternates this script exercises
# are everything an encoder needs.
#
# Usage:
#   ./scripts/sprite-batch-smoke.sh [base_url] [asset_id]
#
# Defaults: base_url=http://localhost:8088  asset_id=<first sprite
# returned by GET /assets>.

set -euo pipefail

BASE="${1:-http://localhost:8088}"
ASSET="${2:-}"
USER_NAME="${AA_SMOKE_USER:-admin}"
PASSWORD="${AA_SMOKE_PASSWORD:-P@ssw0rd}"
COOKIES="$(mktemp -t aa-sprite-smoke-XXXXXX)"
trap 'rm -f "$COOKIES"' EXIT

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; exit 1; }

step "logging in as $USER_NAME"
http_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    -c "$COOKIES" -b "$COOKIES" \
    -X POST "$BASE/api/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$USER_NAME\",\"password\":\"$PASSWORD\"}")
[ "$http_code" = "200" ] || fail "login HTTP $http_code"

if [ -z "$ASSET" ]; then
    step "finding a sprite asset"
    ASSET=$(curl -sS -b "$COOKIES" "$BASE/api/v1/assets?limit=1" \
        | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)
    [ -n "$ASSET" ] || fail "no assets in the database"
fi
echo "    asset_id = $ASSET"

step "list companions"
curl -sS -b "$COOKIES" "$BASE/api/v1/assets/$ASSET/companions" | head -c 200; echo

step "post a smoke companion (sprite frame stub)"
companion_body='{"frames":{"smoke.png":{"frame":{"x":0,"y":0,"w":8,"h":8}}},"meta":{"app":"sprite-batch-smoke"}}'
companion_id=$(curl -sS -b "$COOKIES" \
    -X POST "$BASE/api/v1/assets/$ASSET/companions" \
    -H 'X-Companion-Path: sprite-smoke.json' \
    -H 'X-Content-Type: application/json' \
    -H 'Content-Type: application/octet-stream' \
    --data-binary "$companion_body" \
    | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)
[ -n "$companion_id" ] || fail "companion id missing in response"
echo "    companion_id = $companion_id"

step "list alternates (initial)"
curl -sS -b "$COOKIES" "$BASE/api/v1/assets/$ASSET/alternates" | head -c 200; echo

step "post a smoke alternate (palette swap stub)"
alt_id=$(curl -sS -b "$COOKIES" \
    -X POST "$BASE/api/v1/assets/$ASSET/alternates" \
    -H 'X-Alternate-Label: sprite-batch-smoke-output' \
    -H 'X-Alternate-Kind: palette_swap' \
    -H 'X-Alternate-Metadata: {"remap":[{"from":"#ff0000","to":"#00ff00"}]}' \
    -H 'Content-Type: application/octet-stream' \
    --data-binary 'fake-png-bytes' \
    | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)
[ -n "$alt_id" ] || fail "alternate id missing in response"
echo "    alternate_id = $alt_id"

step "download alternate"
body=$(curl -sS -b "$COOKIES" "$BASE/api/v1/assets/$ASSET/alternates/$alt_id")
[ "$body" = "fake-png-bytes" ] || fail "download bytes mismatch: '$body'"

step "cleanup — delete smoke alternate"
http_code=$(curl -sS -o /dev/null -w '%{http_code}' -b "$COOKIES" \
    -X DELETE "$BASE/api/v1/assets/$ASSET/alternates/$alt_id")
[ "$http_code" = "204" ] || fail "delete alternate HTTP $http_code"

step "cleanup — delete smoke companion"
http_code=$(curl -sS -o /dev/null -w '%{http_code}' -b "$COOKIES" \
    -X DELETE "$BASE/api/v1/assets/$ASSET/companions/$companion_id")
[ "$http_code" = "204" ] || fail "delete companion HTTP $http_code"

printf '\n\033[1;32mOK\033[0m sprite batch API surface verified.\n'
