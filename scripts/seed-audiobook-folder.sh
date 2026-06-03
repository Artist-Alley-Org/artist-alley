#!/usr/bin/env bash
# scripts/seed-audiobook-folder.sh
#
# Ingest a folder of MP3 audiobook chapters (Dark Tower style:
# one .mp3 per disk + an album.nfo) into a single multi-member
# audiobook post. Each MP3 becomes an audio asset (asset_type=11);
# the .nfo is attached as a companion to EVERY member so the
# preview pipeline's nfo-fold runs against each member and stamps
# the album-level metadata.
#
# Usage:
#   ./scripts/seed-audiobook-folder.sh "<folder>" [--session TOKEN]
#                                                [--base-url URL]
#
# Example:
#   ./scripts/seed-audiobook-folder.sh \
#     "/mnt/d/Projects/artist-alley/Book 5 - Wolves of the Calla"
#
# Auth: --session OR AA_SESSION env. If neither set, the script
# bails (mirrors seed-assets.sh).

set -uo pipefail

SRC=""
SESSION="${AA_SESSION:-}"
BASE_URL="http://localhost:8088"
while [ $# -gt 0 ]; do
    case "$1" in
        --session)  SESSION="$2"; shift 2 ;;
        --base-url) BASE_URL="${2%/}"; shift 2 ;;
        -h|--help)  sed -n '2,/^# Auth:/p' "$0"; exit 0 ;;
        *)
            if [ -z "$SRC" ]; then SRC="$1"; shift
            else echo "unknown arg: $1" >&2; exit 2; fi ;;
    esac
done

if [ -z "$SRC" ]; then
    echo "usage: $(basename "$0") <folder> [--session TOKEN]" >&2
    exit 2
fi
if [ ! -d "$SRC" ]; then
    echo "error: not a directory: $SRC" >&2
    exit 2
fi
if [ -z "$SESSION" ]; then
    echo "error: --session or AA_SESSION env required" >&2
    exit 2
fi

COOKIE_ARGS=(-H "Cookie: user=${SESSION}")

echo "==> scanning $SRC"
mapfile -t MP3S < <(find "$SRC" -maxdepth 1 -type f -iname '*.mp3' | sort)
NFO_PATH=$(find "$SRC" -maxdepth 1 -type f -iname 'album.nfo' | head -1)

if [ "${#MP3S[@]}" -eq 0 ]; then
    echo "error: no mp3 files in $SRC" >&2
    exit 1
fi
echo "==> ${#MP3S[@]} tracks found"
if [ -n "$NFO_PATH" ]; then
    echo "==> using album.nfo metadata: $NFO_PATH"
fi

# Pull album-level title + artist from the .nfo with a tiny python
# one-liner (XML stdlib parser). Fall back to folder name + empty
# artist when no .nfo is present.
ALBUM_TITLE=""
ALBUM_ARTIST=""
if [ -n "$NFO_PATH" ]; then
    ALBUM_TITLE=$(python3 - <<EOF
import xml.etree.ElementTree as ET, sys
t = ET.parse("$NFO_PATH").getroot()
e = t.find("title")
print((e.text or "").strip() if e is not None else "")
EOF
    )
    ALBUM_ARTIST=$(python3 - <<EOF
import xml.etree.ElementTree as ET
t = ET.parse("$NFO_PATH").getroot()
for tag in ("albumartist", "artist"):
    e = t.find(tag)
    if e is not None and (e.text or "").strip():
        print((e.text or "").strip()); break
EOF
    )
fi
if [ -z "$ALBUM_TITLE" ]; then
    ALBUM_TITLE=$(basename "$SRC")
fi

echo "==> album title: $ALBUM_TITLE"
echo "==> album artist: ${ALBUM_ARTIST:-(unknown)}"

# Upload the album.nfo bytes ONCE so every member references the
# same content-addressed object. Storage dedups by hash so this is
# cheap regardless.
NFO_HASH=""
NFO_PATH_REL="album.nfo"
if [ -n "$NFO_PATH" ]; then
    UP=$(curl -s "${COOKIE_ARGS[@]}" -X POST \
        -H "X-Content-Type: application/xml" \
        -H "Content-Type: application/octet-stream" \
        --data-binary "@${NFO_PATH}" \
        "${BASE_URL}/api/v1/storage/objects")
    NFO_HASH=$(echo "$UP" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("hash",""))')
    if [ -z "$NFO_HASH" ]; then
        echo "warn: .nfo upload failed: $UP" >&2
        NFO_PATH="" # don't try to attach below
    else
        echo "==> .nfo bytes uploaded (hash=${NFO_HASH:0:12}…)"
    fi
fi

# Per-track upload → create asset → attach .nfo companion.
ASSET_IDS=()
i=0
for mp3 in "${MP3S[@]}"; do
    i=$((i + 1))
    name=$(basename "$mp3")
    stem="${name%.*}"
    printf '[%2d/%d] %s — ' "$i" "${#MP3S[@]}" "$stem"

    UP=$(curl -s "${COOKIE_ARGS[@]}" -X POST \
        -H "X-Content-Type: audio/mpeg" \
        -H "Content-Type: application/octet-stream" \
        --data-binary "@${mp3}" \
        "${BASE_URL}/api/v1/storage/objects")
    HASH=$(echo "$UP" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("hash",""))' 2>/dev/null)
    if [ -z "$HASH" ]; then
        echo "FAIL upload: ${UP:0:120}"
        continue
    fi

    BODY=$(python3 -c "import json; print(json.dumps({'title':'$stem','asset_type':11,'file_hash':'$HASH','file_extension':'mp3','status':'active'}))")
    CR=$(curl -s "${COOKIE_ARGS[@]}" -X POST -H 'Content-Type: application/json' -d "$BODY" -o /tmp/aa-ab-asset.json -w '%{http_code}' "${BASE_URL}/api/v1/assets")
    AID=$(cat /tmp/aa-ab-asset.json | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("id",""))' 2>/dev/null)
    if [ "$CR" != "201" ] || [ -z "$AID" ]; then
        echo "FAIL create: HTTP $CR $(head -c 120 /tmp/aa-ab-asset.json)"
        continue
    fi
    ASSET_IDS+=("$AID")
    echo "asset ${AID:0:8}…"

    # Attach the .nfo companion. The companion API takes raw bytes
    # + headers (X-Companion-Path + X-Content-Type), not JSON — the
    # bytes are content-addressed in storage so re-uploading the
    # same .nfo for every member dedups to one object.
    if [ -n "$NFO_PATH" ]; then
        CC=$(curl -s "${COOKIE_ARGS[@]}" -X POST \
            -H "X-Companion-Path: $NFO_PATH_REL" \
            -H "X-Content-Type: application/xml" \
            -H "Content-Type: application/octet-stream" \
            --data-binary "@${NFO_PATH}" \
            -o /tmp/aa-ab-comp.json -w '%{http_code}' \
            "${BASE_URL}/api/v1/assets/$AID/companions")
        if [ "$CC" != "201" ]; then
            echo "    warn: companion attach HTTP $CC: $(head -c 120 /tmp/aa-ab-comp.json)" >&2
        fi
    fi
done

if [ "${#ASSET_IDS[@]}" -eq 0 ]; then
    echo "no assets uploaded; bailing" >&2
    exit 1
fi

# Build the post — members ordered by upload sequence so the
# AudiobookTool's track list matches disk-01 .. disk-NN. Pass the
# id list via env to avoid bash array expansion bleeding newlines
# into the python heredoc.
DESCRIPTION="Audiobook · ${ALBUM_ARTIST:-unknown} · ${#ASSET_IDS[@]} disks"
POST_BODY=$(AA_IDS="${ASSET_IDS[*]}" AA_TITLE="$ALBUM_TITLE" AA_DESC="$DESCRIPTION" python3 -c "
import json, os
ids = os.environ['AA_IDS'].split()
members = [{'asset_id': a, 'sort_order': i} for i, a in enumerate(ids)]
print(json.dumps({
    'title': os.environ['AA_TITLE'].strip(),
    'description': os.environ['AA_DESC'],
    'visibility': 'public',
    'members': members,
}))")
PR=$(curl -s "${COOKIE_ARGS[@]}" -X POST -H 'Content-Type: application/json' -d "$POST_BODY" -o /tmp/aa-ab-post.json -w '%{http_code}' "${BASE_URL}/api/v1/posts")
POST_ID=$(cat /tmp/aa-ab-post.json | python3 -c 'import json,sys; print(json.loads(sys.stdin.read()).get("id",""))' 2>/dev/null)
if [ "$PR" != "201" ] || [ -z "$POST_ID" ]; then
    echo "post create FAILED: HTTP $PR $(head -c 200 /tmp/aa-ab-post.json)" >&2
    exit 1
fi

echo ""
echo "==> ok"
echo "    post:   $POST_ID"
echo "    assets: ${#ASSET_IDS[@]}"
echo "    open:   ${BASE_URL/8088/5173}/?post=${POST_ID}"
