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

# --- synthetic actor URIs (#1526) ------------------------------------------
#
# Every envelope is signed by its actor. studio-a's outbox refuses to
# sign an activity whose actor_uri is not "<its site.base_url>/users/
# <username>", and studio-b's inbox accepts an envelope only from
# "<the instance URL it registered studio-a under>/users/<name>"
# (unknown_actor otherwise). pair.sh makes the two agree (studio-a's
# base_url = A_URL_FROM_B). Scenarios that inject activities must use
# studio_a_actor_uri, never a hard-coded host, and call
# assert_actor_bound_to_peer before staging any row, so fixture drift
# fails here instead of as a silent refusal after dispatch.

# studio_a_actor_uri <username>: the actor URI studio-a signs as.
studio_a_actor_uri() {
    local base
    base=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
        "SELECT value->>'base_url' FROM system_config WHERE key = 'site'" | tr -d ' \r\n')
    [ -n "$base" ] || fail "studio-a site.base_url is empty (run pair.sh)"
    printf '%s/users/%s' "${base%/}" "$1"
}

# assert_actor_bound_to_peer <actor_uri>: fail before dispatch unless
# the URI is "<origin>/users/<one segment>", that origin is studio-a's
# site.base_url, and studio-b has a connected, enabled peer registered
# at exactly that origin.
assert_actor_bound_to_peer() {
    local actor="$1" origin name base n
    origin="${actor%/users/*}"
    name="${actor##*/users/}"
    if [ "$origin" = "$actor" ] || [ -z "$name" ] || [ "${name#*/}" != "$name" ]; then
        fail "actor URI ${actor} is not <origin>/users/<name> (#1526)"
    fi
    base=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
        "SELECT value->>'base_url' FROM system_config WHERE key = 'site'" | tr -d ' \r\n')
    [ "$origin" = "${base%/}" ] \
        || fail "actor URI ${actor} is not under studio-a's site.base_url ${base}: studio-a would refuse to sign it (#1526)"
    n=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
        "SELECT count(*) FROM federation_peers
         WHERE instance_url = '${origin}' AND enabled AND status = 'connected'" | tr -d ' \r\n')
    [ "$n" = "1" ] \
        || fail "studio-b has no connected peer at ${origin}: ${actor} would be refused as unknown_actor (#1526)"
    pass "actor ${actor} is bound to studio-b's peer row for studio-a"
}

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
