# scripts/dogfood/scenarios/lib.sh — shared scenario helpers.
#
# Source this from every scenario script. Sets:
#
#   A_HOST            studio-a (dev) base URL
#   B_HOST            studio-b base URL
#   A_COOKIES         cookie jar for admin@studio-a
#   B_COOKIES         cookie jar for admin@studio-b
#
# Functions:
#
#   login_admin <host> <cookies>       cookie-jar admin login
#   api_post   <host> <cookies> <path> <json>
#   api_get    <host> <cookies> <path>
#   step       <message>                cyan header
#   pass       <message>                green ✓
#   fail       <message>                red ✗ + exit 1
#   info       <message>                plain
#
# Tear-down: scripts that mint fixture users/posts should
# t-cleanup-style remove them at the end. Use `trap` against
# the function `cleanup_fixtures` you define per-scenario.

set -euo pipefail

A_HOST="${STUDIO_A_HOST:-http://localhost:5173}"
B_HOST="${STUDIO_B_HOST:-https://studio-b.local:9443}"

ADMIN_USER="${AA_DOGFOOD_ADMIN_USER:-admin}"
ADMIN_PASS="${AA_DOGFOOD_ADMIN_PASS:-ArtistAlleyMogul}"

A_COOKIES=$(mktemp)
B_COOKIES=$(mktemp)
trap 'rm -f "$A_COOKIES" "$B_COOKIES"' EXIT

CYAN=$'\033[1;36m'
GREEN=$'\033[1;32m'
RED=$'\033[1;31m'
YELLOW=$'\033[1;33m'
RESET=$'\033[0m'

step() { printf '\n%s==>%s %s\n' "$CYAN" "$RESET" "$*"; }
pass() { printf '%s ✓%s %s\n'     "$GREEN" "$RESET" "$*"; }
fail() { printf '%s ✗%s %s\n'     "$RED"   "$RESET" "$*" >&2; exit 1; }
info() { printf '   %s\n'         "$*"; }
warn() { printf '%sWARN:%s %s\n'  "$YELLOW" "$RESET" "$*" >&2; }

login_admin() {
    local host="$1" cookies="$2"
    local code
    code=$(curl -sk -o /dev/null -w "%{http_code}" \
        -c "$cookies" \
        -X POST "${host}/api/v1/auth/login" \
        -H "Content-Type: application/json" \
        -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}")
    if [ "$code" != "200" ]; then
        fail "${host} login: HTTP ${code}"
    fi
}

api_post() {
    local host="$1" cookies="$2" path="$3" json="$4"
    curl -sk -b "$cookies" \
        -X POST "${host}/api/v1${path}" \
        -H "Content-Type: application/json" \
        -d "$json"
}

api_get() {
    local host="$1" cookies="$2" path="$3"
    curl -sk -b "$cookies" "${host}/api/v1${path}"
}

api_delete() {
    local host="$1" cookies="$2" path="$3"
    curl -sk -b "$cookies" \
        -X DELETE "${host}/api/v1${path}" \
        -o /dev/null -w "%{http_code}"
}

# Sleep + retry pattern: poll a predicate until it returns 0
# (success) or attempts run out. The predicate is a shell
# expression evaluated each tick.
#
#   wait_for "studio-b shows the Like" 30 "$(...)"
wait_for() {
    local desc="$1" attempts="$2" predicate="$3"
    local i
    for i in $(seq 1 "$attempts"); do
        if eval "$predicate"; then
            return 0
        fi
        sleep 1
    done
    fail "timed out waiting for: ${desc}"
}
