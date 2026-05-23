#!/usr/bin/env bash
# rs-diff.sh
#
# Compare the artist-alley fork to a given ResourceSpace version (trunk or
# a release tag). Use this to inspect upstream changes for potential
# cherry-pick into our fork.
#
# Usage:
#   ./scripts/rs-diff.sh                       # interactive
#   ./scripts/rs-diff.sh trunk                 # vs current trunk
#   ./scripts/rs-diff.sh 10.7                  # vs releases/10.7
#   ./scripts/rs-diff.sh trunk -r 28830        # vs trunk at specific rev
#   ./scripts/rs-diff.sh 10.7 --html out.html  # generate side-by-side HTML
#
# Notes:
#   - Requires `svn`. Install with `sudo apt-get install subversion`.
#   - Excludes our additive directories (services/, web/, infra/, etc.)
#     and gitignored paths so the diff is RS-vs-RS only.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SVN_BASE="http://svn.resourcespace.com/svn/rs"
SCRATCH="${TMPDIR:-/tmp}/artist-alley-rs-diff"

# --- Arg parsing ---
TARGET="${1:-}"
REV=""
HTML_OUT=""

if [ -z "$TARGET" ]; then
    read -rp "Compare against (trunk | 10.7 | 10.6 | ...): " TARGET
fi
shift || true
while [ $# -gt 0 ]; do
    case "$1" in
        -r|--rev)   REV="$2"; shift 2 ;;
        --html)     HTML_OUT="$2"; shift 2 ;;
        *)          echo "Unknown arg: $1" >&2; exit 2 ;;
    esac
done

command -v svn >/dev/null || { echo "svn is not installed" >&2; exit 1; }

# --- URL resolution ---
case "$TARGET" in
    trunk)          URL="$SVN_BASE/trunk" ;;
    [0-9]*.[0-9]*)  URL="$SVN_BASE/releases/$TARGET" ;;
    *)              echo "Unrecognized target: $TARGET" >&2; exit 2 ;;
esac
if [ -n "$REV" ]; then URL="${URL}@${REV}"; fi

mkdir -p "$SCRATCH"
EXPORT_DIR="$SCRATCH/$(echo "$TARGET" | tr / _)${REV:+_r$REV}"

if [ ! -d "$EXPORT_DIR" ]; then
    echo "==> Exporting $URL to $EXPORT_DIR"
    svn export "$URL" "$EXPORT_DIR" --quiet
else
    echo "==> Using cached export at $EXPORT_DIR"
fi

# --- Diff ---
# Exclude our additive dirs and common gitignore patterns. Compare RS-only
# territory; that's the surface where we'd consider cherry-picking.
EXCLUDES=(
    --exclude=.git
    --exclude=.svn
    --exclude=services
    --exclude=web
    --exclude=infra
    --exclude=scripts
    --exclude=docs
    --exclude=_docker_reference
    --exclude=node_modules
    --exclude=vendor
    --exclude=filestore
    --exclude=.env
    --exclude=.env.example
    --exclude=docker-compose.yml
    --exclude=LICENSE
    --exclude=README.md
    --exclude='*.zip'
)

if [ -n "$HTML_OUT" ]; then
    command -v diff2html >/dev/null \
        || { echo "diff2html-cli not found. npm install -g diff2html-cli" >&2; exit 1; }
    PATCH="$SCRATCH/diff_$(date +%s).patch"
    diff -ruN "${EXCLUDES[@]}" "$EXPORT_DIR" "$ROOT" > "$PATCH" || true
    diff2html -i file -s side -F "$HTML_OUT" -- "$PATCH"
    echo "==> HTML report: $HTML_OUT"
else
    diff -ruN "${EXCLUDES[@]}" "$EXPORT_DIR" "$ROOT" | head -200
    echo
    echo "==> Truncated to first 200 lines. Pipe through \`less\` or use --html."
fi
