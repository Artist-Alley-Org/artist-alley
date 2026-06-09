#!/usr/bin/env bash
# scripts/dogfood/pair.sh — pair studio-a (dev) ↔ studio-b
# via direct registration. Idempotent.
#
# Asymmetry note (dogfood-specific):
#
#   studio-a → studio-b   https://studio-b.local:9443
#     Goes through the admin API normally. The peer registry
#     validator accepts https URLs only — that's a production
#     safety; nothing to bypass.
#
#   studio-b → studio-a   http://nginx (the docker service name)
#     Dev's nginx doesn't terminate TLS, so we register this
#     direction by inserting into federation_peers directly via
#     psql. The runtime federation code is transport-agnostic
#     (HTTP-Sig signs payload, not transport) so this works
#     identically to the API path; we're only sidestepping the
#     "must be https" validator for the local-only case.
#
# When 1.22.I-b+ requires real TLS on both ends, dev will get
# its own mkcert cert + an HTTPS listener and both directions
# go through the API. Until then this is the simplest path.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }
pass() { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

ADMIN_USER="${AA_DOGFOOD_ADMIN_USER:-admin}"
ADMIN_PASS="${AA_DOGFOOD_ADMIN_PASS:-ArtistAlleyMogul}"

A_HOST="${STUDIO_A_HOST:-http://localhost:5173}"
B_HOST="${STUDIO_B_HOST:-https://studio-b.local:9443}"

# URLs the OTHER peer uses to dial this one.
B_URL_FROM_A="https://studio-b.local:9443"
A_URL_FROM_B="http://nginx"  # dev's nginx service name on the bridge

a_cookies=$(mktemp)
trap 'rm -f "$a_cookies"' EXIT

# --- 1. log into studio-a (admin API needs the session) -------------------

step "Logging in as admin on studio-a"
code=$(curl -sk -o /dev/null -w "%{http_code}" \
    -c "$a_cookies" \
    -X POST "${A_HOST}/api/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"${ADMIN_USER}\",\"password\":\"${ADMIN_PASS}\"}")
[ "$code" = "200" ] || fail "studio-a login: HTTP ${code}"
pass "studio-a admin in"

# --- 2. fetch each peer's instance doc (public, unauth) ------------------

step "Fetching federation instance docs"
A_INSTANCE=$(curl -sk "${A_HOST}/api/v1/federation/instance")
B_INSTANCE=$(curl -sk "${B_HOST}/api/v1/federation/instance")
A_PUBKEY=$(echo "$A_INSTANCE" | python3 -c 'import sys, json; print(json.load(sys.stdin)["public_key_pem"])')
B_PUBKEY=$(echo "$B_INSTANCE" | python3 -c 'import sys, json; print(json.load(sys.stdin)["public_key_pem"])')
A_FINGERPRINT=$(echo "$A_INSTANCE" | python3 -c 'import sys, json; print(json.load(sys.stdin)["fingerprint"])')
B_FINGERPRINT=$(echo "$B_INSTANCE" | python3 -c 'import sys, json; print(json.load(sys.stdin)["fingerprint"])')
pass "studio-a fingerprint ${A_FINGERPRINT:0:16}…"
pass "studio-b fingerprint ${B_FINGERPRINT:0:16}…"

# --- 3. register studio-b on studio-a (admin API) -------------------------

step "Registering studio-b as a peer on studio-a (API)"
body=$(python3 -c "
import json
print(json.dumps({
    'instance_url': '${B_URL_FROM_A}',
    'display_name': 'Studio B (dogfood)',
    'instance_public_key': '''${B_PUBKEY}''',
    'trust_tier': 'connected',
    'encryption_policy': 'plaintext',
}))")
out=$(mktemp)
code=$(curl -sk -b "$a_cookies" -o "$out" -w "%{http_code}" \
    -X POST "${A_HOST}/api/v1/admin/federation/peers" \
    -H "Content-Type: application/json" \
    -d "$body")
case "$code" in
    200|201) pass "studio-b registered on studio-a (HTTP ${code})" ;;
    409)     pass "studio-b already registered on studio-a (idempotent)" ;;
    *)
        cat "$out" >&2
        fail "studio-b registration on studio-a failed (HTTP ${code})"
        ;;
esac
rm -f "$out"

# --- 4. register studio-a on studio-b (direct DB) -------------------------

step "Registering studio-a as a peer on studio-b (direct INSERT, http://)"
# Idempotent via ON CONFLICT (instance_url). handshake_by_user_ref is
# whatever the bootstrap admin's ref is on studio-b — usually 1.
admin_ref=$(docker compose exec -T postgres-b psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = '${ADMIN_USER}' LIMIT 1" \
    | tr -d ' \r')
[ -n "$admin_ref" ] || fail "couldn't find admin user ref on studio-b"

# Escape the PEM newlines for SQL literal. psql -c needs the
# string on one line; PEM newlines stay as \\n inside the literal.
A_PUBKEY_SQL=$(printf '%s' "$A_PUBKEY" | python3 -c 'import sys; print(sys.stdin.read().replace("\n", "\\n").replace("'\''", "'\'\''"))')

docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
INSERT INTO federation_peers (
    instance_url, display_name, instance_public_key,
    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref
) VALUES (
    '${A_URL_FROM_B}',
    'Studio A (dev)',
    E'${A_PUBKEY_SQL}',
    'connected',
    'plaintext',
    TRUE,
    'connected',
    ${admin_ref}
)
ON CONFLICT (instance_url) DO UPDATE
   SET instance_public_key = EXCLUDED.instance_public_key,
       display_name        = EXCLUDED.display_name,
       enabled             = TRUE,
       status              = 'connected'
RETURNING id, instance_url, status;
" 2>&1 | tail -3
pass "studio-a registered on studio-b"

# --- 5. done --------------------------------------------------------------

cat <<EOF

$(printf '\033[1;32mpaired.\033[0m')

Verify in the admin UI:
  studio-a:  ${A_HOST}/admin/federation/peers
  studio-b:  ${B_HOST}/admin/federation/peers

Next:
  ./scripts/dogfood/scenarios/01-like-cross-instance.sh
EOF
