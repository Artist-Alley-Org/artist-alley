#!/usr/bin/env bash
# scripts/seed-assets.sh
#
# Bulk-upload local image files as assets, for browse-page testing.
#
# Two-step per file:
#   1. POST /api/v1/storage/objects with the raw bytes + X-Content-Type.
#      The server dedups by sha256 and returns { hash, deduped, ... }.
#   2. POST /api/v1/assets with { title, asset_type, file_hash }
#      to create the asset row that re-pins the bytes under the
#      asset's UUID.
#
# The field is `asset_type` and the flag is `--asset-type` (#966). Both
# were `resource_type` — the pre-fork name — long after `AssetCreate`
# stopped accepting it, so the create step sent a body with no
# `asset_type` in it at all, the column defaulted to 0, and the row hit
# a foreign key. Anyone following this script got a 500 per file.
#
# Usage:
#   ./scripts/seed-assets.sh <source-dir> [--limit N] [--session TOKEN]
#                            [--asset-type N] [--base-url URL]
#                            [--shuffle]
#
# Examples:
#   ./scripts/seed-assets.sh /mnt/d/Projects/Snapdex/datasets/cards --limit 200
#   AA_SESSION=$(psql ... -tA -c "SELECT session FROM user WHERE ref=1") \
#     ./scripts/seed-assets.sh ./photos --limit 50
#
# Notes:
#   - Picks images by extension (jpg/jpeg/png/gif/webp). Other files
#     are skipped silently.
#   - Title is the filename minus extension.
#   - Asset type defaults to 1 (Photo, per the seeded asset_types
#     table).
#   - Auth: session token from --session OR AA_SESSION env. If
#     neither is set, the script prompts.
#   - Failures are reported but don't abort the run; a summary tallies
#     successes / dedupes / errors at the end.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- args ----
SRC=""
LIMIT=0           # 0 = no cap
SESSION="${AA_SESSION:-}"
ASSET_TYPE=1
BASE_URL="http://localhost:8088"
SHUFFLE="no"

while [ $# -gt 0 ]; do
    case "$1" in
        --limit)         LIMIT="$2"; shift 2 ;;
        --session)       SESSION="$2"; shift 2 ;;
        --asset-type)    ASSET_TYPE="$2"; shift 2 ;;
        --base-url)      BASE_URL="${2%/}"; shift 2 ;;
        --shuffle)       SHUFFLE="yes"; shift ;;
        -h|--help)
            sed -n '2,/^# Notes:/p' "$0"; exit 0 ;;
        *)
            if [ -z "$SRC" ]; then SRC="$1"; shift
            else echo "unknown arg: $1" >&2; exit 2; fi ;;
    esac
done

if [ -z "$SRC" ]; then
    echo "usage: $(basename "$0") <source-dir> [--limit N] [--session TOKEN]" >&2
    exit 2
fi
if [ ! -d "$SRC" ]; then
    echo "error: source dir not found: $SRC" >&2
    exit 2
fi
if [ -z "$SESSION" ]; then
    echo "error: no session token (--session or AA_SESSION env). Sign in via the UI then read user.session from postgres." >&2
    exit 2
fi

# --- file discovery ----
echo "==> scanning $SRC for images"
mapfile -t FILES < <(
    find "$SRC" -type f \
        \( -iname "*.jpg" -o -iname "*.jpeg" -o -iname "*.png" \
           -o -iname "*.gif" -o -iname "*.webp" \) \
    | { [ "$SHUFFLE" = "yes" ] && shuf || cat; }
)
TOTAL=${#FILES[@]}
if [ "$LIMIT" -gt 0 ] && [ "$LIMIT" -lt "$TOTAL" ]; then
    FILES=("${FILES[@]:0:$LIMIT}")
fi
PLANNED=${#FILES[@]}
echo "==> $TOTAL files found, $PLANNED queued for upload"

if [ "$PLANNED" -eq 0 ]; then
    exit 0
fi

# --- content-type inference ----
content_type_for() {
    case "${1,,}" in
        *.jpg|*.jpeg) echo "image/jpeg" ;;
        *.png)        echo "image/png" ;;
        *.gif)        echo "image/gif" ;;
        *.webp)       echo "image/webp" ;;
        *)            echo "application/octet-stream" ;;
    esac
}

OK=0
DEDUPED=0
ERR=0
START=$SECONDS

i=0
for file in "${FILES[@]}"; do
    i=$((i + 1))
    name=$(basename "$file")
    stem="${name%.*}"
    ct=$(content_type_for "$file")

    # 1) upload bytes
    upload=$(curl -s -X POST "${BASE_URL}/api/v1/storage/objects" \
        -H "Cookie: user=${SESSION}" \
        -H "X-Content-Type: ${ct}" \
        -H "Content-Type: application/octet-stream" \
        --data-binary "@${file}")
    hash=$(printf '%s' "$upload" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("hash",""))' 2>/dev/null)
    deduped=$(printf '%s' "$upload" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("deduped",False))' 2>/dev/null)

    if [ -z "$hash" ]; then
        ERR=$((ERR + 1))
        printf '\r[%4d/%d] err  %s — upload failed: %s\n' "$i" "$PLANNED" "$name" "${upload:0:100}"
        continue
    fi

    # 2) create asset
    body=$(python3 -c '
import json, sys
print(json.dumps({
    "title": sys.argv[1],
    "asset_type": int(sys.argv[2]),
    "file_hash": sys.argv[3],
    "file_extension": sys.argv[4],
    "status": "active",
}))
' "$stem" "$ASSET_TYPE" "$hash" "${name##*.}")

    create=$(curl -s -o /tmp/aa-seed-create.json -w '%{http_code}' -X POST "${BASE_URL}/api/v1/assets" \
        -H "Cookie: user=${SESSION}" \
        -H "Content-Type: application/json" \
        --data "$body")

    if [ "$create" != "201" ]; then
        ERR=$((ERR + 1))
        printf '\r[%4d/%d] err  %s — asset create HTTP %s: %s\n' "$i" "$PLANNED" "$name" "$create" "$(cat /tmp/aa-seed-create.json | head -c 100)"
        continue
    fi

    OK=$((OK + 1))
    [ "$deduped" = "True" ] && DEDUPED=$((DEDUPED + 1))

    # Lightweight progress — every 10 items or at end.
    if [ $((i % 10)) -eq 0 ] || [ "$i" -eq "$PLANNED" ]; then
        elapsed=$((SECONDS - START))
        # The zero-elapsed guard has to be in BASH, not in awk. Both
        # `$i` and `$elapsed` are interpolated by the shell before awk
        # ever sees them, so a fast run handed gawk the literal
        # `if (0 > 0) printf "%.1f", 6 / 0`, and gawk folds that
        # constant division at PARSE time — the guard it sits behind
        # never runs. Every short run ended with "awk: division by zero
        # attempted" on stderr next to a successful summary.
        if [ "$elapsed" -gt 0 ]; then
            rate=$(awk "BEGIN { printf \"%.1f\", $i / $elapsed }")
        else
            rate="-"
        fi
        printf '\r[%4d/%d] ok  rate %s/s  ok=%d deduped=%d err=%d   ' "$i" "$PLANNED" "$rate" "$OK" "$DEDUPED" "$ERR"
    fi
done

echo ""
echo "==> done. ok=$OK deduped=$DEDUPED err=$ERR  elapsed=$((SECONDS - START))s"
[ "$ERR" -eq 0 ]
