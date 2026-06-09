#!/usr/bin/env bash
# scripts/dogfood/up.sh — bring up the dogfood stack.
#
# Profile-based: the dev stack at the repo root plays studio-a;
# this script adds studio-b alongside it. Idempotent — safe to
# re-run.
#
# What it does (in order):
#   1. mkcert root CA install (one-time, host trust store).
#   2. Issue studio-b.local cert into infra/docker/dogfood/certs/.
#   3. Idempotently write `studio-b.local 127.0.0.1` into
#      /etc/hosts under a marker block.
#   4. `docker compose --profile dogfood up -d` so the studio-b
#      services join the dev stack's network.
#   5. Wait for studio-b's app to report healthy.
#
# After this script returns, studio-b is reachable at
# https://studio-b.local:9443 with a trusted cert.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

CERT_DIR="infra/docker/dogfood/certs"
HOSTS_MARKER="# artist-alley dogfood — managed by scripts/dogfood/up.sh"
HOSTS_BEGIN="${HOSTS_MARKER} BEGIN"
HOSTS_END="${HOSTS_MARKER} END"

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# --- 0. preflight -----------------------------------------------------------

if ! command -v mkcert >/dev/null 2>&1; then
    fail "mkcert is required but not found on PATH. Install per
       https://github.com/FiloSottile/mkcert#installation
       then re-run this script."
fi

if [ ! -f .env ]; then
    fail ".env not found. Run ./scripts/bootstrap.sh first — the dogfood
       profile reuses the same AA_MASTER_KEY + AA_SCRAMBLE_KEY."
fi

# --- 1. mkcert CA -----------------------------------------------------------

step "Provisioning mkcert root CA (idempotent)"
mkcert -install

# --- 2. studio-b cert -------------------------------------------------------

step "Issuing studio-b.local cert into ${CERT_DIR}/"
mkdir -p "$CERT_DIR"
if [ ! -f "${CERT_DIR}/studio-b.local.pem" ] || \
   [ ! -f "${CERT_DIR}/studio-b.local-key.pem" ]; then
    ( cd "$CERT_DIR" && mkcert studio-b.local )
else
    echo "  cert already present; skipping issuance"
fi

# --- 3. /etc/hosts ----------------------------------------------------------

step "Idempotently writing studio-b.local to /etc/hosts"
if grep -q "$HOSTS_BEGIN" /etc/hosts; then
    echo "  marker block already present; leaving it alone"
else
    if ! sudo -n true 2>/dev/null; then
        warn "sudo required to edit /etc/hosts — you may be prompted"
    fi
    sudo bash -c "cat >> /etc/hosts <<EOF

${HOSTS_BEGIN}
127.0.0.1 studio-b.local
${HOSTS_END}
EOF"
    echo "  added"
fi

# --- 4. docker compose ------------------------------------------------------

step "Bringing up the dogfood profile (studio-b alongside dev)"
docker compose --profile dogfood up -d --build

# --- 5. wait for studio-b ready --------------------------------------------

step "Waiting for studio-b app to report healthy"
for i in $(seq 1 30); do
    if docker compose exec -T app-b curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
        echo "  ready after ${i} attempts"
        break
    fi
    sleep 2
    if [ "$i" = "30" ]; then
        fail "studio-b never became healthy. Check: docker compose logs app-b"
    fi
done

cat <<EOF

\033[1;32mdogfood up.\033[0m

  Dev (= studio-a):  http://localhost:5173    (Vite)
                     http://localhost:8080    (nginx prod-shape)
                     internal alias: studio-a.local (no TLS)

  Studio B:          https://studio-b.local:9443

  Login:             admin / ArtistAlleyMogul  (both stacks)

Next:
  ./scripts/dogfood/seed.sh --site <path>   # seed studio-b
  ./scripts/dogfood/pair.sh                  # pair the two stacks
  ./scripts/dogfood/scenarios/01-like-cross-instance.sh
EOF
