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

# Capability-shaped check, not environment-shaped. The script doesn't
# care WHERE it's running (bare metal, CI runner, container, etc.) —
# only what it needs to bring up studio-b's HTTPS surface:
#
#   path A:  certs already provisioned at $CERT_DIR.
#            (CI flow: the runner template bind-mounts pre-generated
#             certs from the host into the workspace before this
#             script runs; nothing for up.sh to do.)
#
#   path B:  mkcert on PATH. We'll issue + install fresh certs
#            ourselves.
#            (Local-dev flow.)
#
# This is intentionally NOT `if [ -n "$CI" ]`. Every divergence
# between "what local dev does" and "what CI does" is a future
# heisenbug; branching on capability instead keeps both code paths
# walking the same script.

if [ "$mode" = "full" ]; then
    if [ -f "${CERT_DIR}/studio-b.local.pem" ] && \
       [ -f "${CERT_DIR}/studio-b.local-key.pem" ] && \
       [ -f "${CERT_DIR}/mkcert-rootCA.pem" ]; then
        tls_source="pre-existing"
    elif command -v mkcert >/dev/null 2>&1; then
        tls_source="mkcert"
    else
        fail "No TLS prereqs available. Either:
       (a) Drop studio-b.local.pem + studio-b.local-key.pem +
           mkcert-rootCA.pem into ${CERT_DIR}/, or
       (b) Install mkcert
           (https://github.com/FiloSottile/mkcert#installation)
       and re-run this script."
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

# --- 1. TLS certs -----------------------------------------------------------

mkdir -p "$CERT_DIR"
if [ "$tls_source" = "mkcert" ]; then
    step "Provisioning mkcert root CA (idempotent)"
    mkcert -install

    step "Issuing studio-b.local cert into ${CERT_DIR}/"
    if [ ! -f "${CERT_DIR}/studio-b.local.pem" ] || \
       [ ! -f "${CERT_DIR}/studio-b.local-key.pem" ]; then
        ( cd "$CERT_DIR" && mkcert studio-b.local )
    else
        echo "  cert already present; skipping issuance"
    fi

    # Copy the mkcert root CA into the certs dir so the dogfood
    # compose override can bind-mount it into the app + app-b
    # containers. Without this, Go's TLS verify on outbound
    # federation HTTPS calls fails with `x509: certificate signed by
    # unknown authority`. The path matches infra/docker/dogfood/
    # docker-compose.override.yml.
    mkcert_caroot="$(mkcert -CAROOT)"
    cp -f "${mkcert_caroot}/rootCA.pem" "${CERT_DIR}/mkcert-rootCA.pem"
    echo "  mkcert root CA copied into ${CERT_DIR}/mkcert-rootCA.pem"
else
    step "Using pre-existing TLS prereqs at ${CERT_DIR}/"
    echo "  studio-b.local cert + key + mkcert root CA all present"
fi

# --- 2. studio-b.local resolution -------------------------------------------

step "Checking studio-b.local resolution"
if getent hosts studio-b.local >/dev/null 2>&1; then
    echo "  studio-b.local resolves — nothing to do"
else
    # `studio-b.local` is a docker network alias on nginx-b (see
    # docker-compose.yml), so anything attached to
    # artist-alley_default resolves it via docker DNS automatically
    # — that includes both stacks' own containers AND any extra
    # container that joins the network (e.g. the CI runner via
    # `docker network connect`).
    #
    # The host SHELL running this script is NOT on that network;
    # `studio-b.local` doesn't resolve from outside docker. We
    # used to edit /etc/hosts here to paper that over, but that
    # broke under the containerised CI runner — `127.0.0.1` from
    # inside the runner container is the runner's own loopback,
    # NOT the host's, so the entry pointed at nothing.
    #
    # New contract: /etc/hosts is HOST state, not script state.
    # If you want to reach https://studio-b.local:9443 from the
    # host's browser, add this once to your /etc/hosts:
    #
    #     127.0.0.1 studio-b.local
    #
    # Documented in infra/runner/README.md (CI case) and in the
    # dogfood docs (local-dev case).
    warn "studio-b.local doesn't resolve from this shell.
       Containers on the compose network reach it via docker DNS automatically;
       the host browser needs a one-time '/etc/hosts' edit:
         echo '127.0.0.1 studio-b.local' | sudo tee -a /etc/hosts"
fi

# --- 4. docker compose ------------------------------------------------------

step "Bringing up the dogfood profile (studio-b alongside dev)"
# The override file adds the mkcert CA bind-mount + SSL_CERT_DIR
# env to app + app-b so outbound HTTPS federation calls trust
# studio-b's mkcert-issued cert. Without it, the delivery worker
# silently retries forever with "x509: certificate signed by
# unknown authority" in last_error. See infra/docker/dogfood/
# docker-compose.override.yml.
#
# Two compose calls: the first brings every service in the dogfood
# profile up (creates whatever's missing); the second
# force-recreates `app` only, so a re-run picks up a freshly-mounted
# CA without bouncing the whole stack. Passing `app` as a positional
# arg to compose `up` SCOPES the command to that one service + its
# deps — without the bare `up` first, postgres-b / app-b / nginx-b
# would never start on a fresh runner.
docker compose \
    -f docker-compose.yml \
    -f infra/docker/dogfood/docker-compose.override.yml \
    --profile dogfood up -d --build
docker compose \
    -f docker-compose.yml \
    -f infra/docker/dogfood/docker-compose.override.yml \
    --profile dogfood up -d --force-recreate app

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
