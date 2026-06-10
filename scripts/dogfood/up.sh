#!/usr/bin/env bash
# scripts/dogfood/up.sh — bring up the dogfood stack.
#
# Two modes:
#   default          studio-a (dev) + studio-b (dogfood profile).
#                    Full federation playground.
#   --standalone     studio-a only. Skips mkcert + /etc/hosts +
#                    the docker compose `dogfood` profile. Use this
#                    for the standalone Playwright runs / PR-fast-
#                    feedback workflow; cuts ~30s off the cycle.
#
# Profile-based: the dev stack at the repo root plays studio-a;
# the default mode adds studio-b alongside it via the `dogfood`
# compose profile. Idempotent — safe to re-run.
#
# After the default mode returns, studio-b is reachable at
# https://studio-b.local:9443 with a trusted cert.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# --- arg parsing ----------------------------------------------------------

mode="full"
while [ $# -gt 0 ]; do
    case "$1" in
        --standalone) mode="standalone"; shift ;;
        --full)       mode="full"; shift ;;
        -h|--help)
            sed -n '2,18p' "$0"
            exit 0
            ;;
        *) printf 'Unknown arg: %s\n' "$1" >&2; exit 2 ;;
    esac
done

CERT_DIR="infra/docker/dogfood/certs"
HOSTS_MARKER="# artist-alley dogfood — managed by scripts/dogfood/up.sh"
HOSTS_BEGIN="${HOSTS_MARKER} BEGIN"
HOSTS_END="${HOSTS_MARKER} END"

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# --- 0. preflight -----------------------------------------------------------

if [ "$mode" = "full" ]; then
    if ! command -v mkcert >/dev/null 2>&1; then
        fail "mkcert is required but not found on PATH. Install per
       https://github.com/FiloSottile/mkcert#installation
       then re-run this script."
    fi
fi

if [ ! -f .env ]; then
    fail ".env not found. Run ./scripts/bootstrap.sh first — the dogfood
       profile reuses the same AA_MASTER_KEY + AA_SCRAMBLE_KEY."
fi

# --- standalone short-circuit ---------------------------------------------
# In standalone mode we skip the mkcert / /etc/hosts / dogfood-profile
# work entirely. Just `docker compose up -d` the dev services and
# wait for them.

if [ "$mode" = "standalone" ]; then
    step "Bringing up the dev stack only (standalone mode)"
    docker compose up -d --build
    step "Waiting for dev app to report healthy"
    for i in $(seq 1 30); do
        if docker compose exec -T app curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
            echo "  ready after ${i} attempts"
            break
        fi
        sleep 2
        if [ "$i" -eq 30 ]; then
            fail "dev app did not become healthy in 60s"
        fi
    done
    printf '\n\033[1;32mstandalone up.\033[0m\n\n'
    printf 'Studio-a UI:  http://localhost:5173/\n'
    printf 'Run tests:    ./scripts/dogfood/run-ui.sh standalone\n\n'
    exit 0
fi

# --- 1. mkcert CA -----------------------------------------------------------

step "Provisioning mkcert root CA (idempotent)"
mkcert -install

# --- 2. studio-b cert + mkcert root CA -------------------------------------

step "Issuing studio-b.local cert into ${CERT_DIR}/"
mkdir -p "$CERT_DIR"
if [ ! -f "${CERT_DIR}/studio-b.local.pem" ] || \
   [ ! -f "${CERT_DIR}/studio-b.local-key.pem" ]; then
    ( cd "$CERT_DIR" && mkcert studio-b.local )
else
    echo "  cert already present; skipping issuance"
fi

# Copy the mkcert root CA into the certs dir so the dogfood compose
# override can bind-mount it into the app + app-b containers. Without
# this, Go's TLS verify on outbound federation HTTPS calls fails with
# `x509: certificate signed by unknown authority`. The path matches
# infra/docker/dogfood/docker-compose.override.yml.
mkcert_caroot="$(mkcert -CAROOT)"
cp -f "${mkcert_caroot}/rootCA.pem" "${CERT_DIR}/mkcert-rootCA.pem"
echo "  mkcert root CA copied into ${CERT_DIR}/mkcert-rootCA.pem"

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
# The override file adds the mkcert CA bind-mount + SSL_CERT_DIR
# env to app + app-b so outbound HTTPS federation calls trust
# studio-b's mkcert-issued cert. Without it, the delivery worker
# silently retries forever with "x509: certificate signed by
# unknown authority" in last_error. See infra/docker/dogfood/
# docker-compose.override.yml.
docker compose \
    -f docker-compose.yml \
    -f infra/docker/dogfood/docker-compose.override.yml \
    --profile dogfood up -d --build \
    --force-recreate app  # pick up the new bind-mount if app was already running

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
