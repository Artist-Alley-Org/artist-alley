#!/usr/bin/env bash
# artist-alley bootstrap.
# One-shot setup for a fresh dev environment.
#
# Usage: ./scripts/bootstrap.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

step "Checking prerequisites"
command -v docker >/dev/null || fail "docker is not installed"
docker compose version >/dev/null 2>&1 || fail "docker compose v2 is not available"

if [ ! -f .env ]; then
    step "Creating .env from .env.example"
    cp .env.example .env

    # Substitute current UID/GID so bind-mounted files are owned correctly.
    sed -i.bak "s/^UID=.*/UID=$(id -u)/" .env && rm .env.bak
    sed -i.bak "s/^GID=.*/GID=$(id -g)/" .env && rm .env.bak

    # Secrets. openssl is REQUIRED rather than optional (#996): two of
    # the values below are hard requirements of docker-compose.yml
    # (`${AA_MASTER_KEY:?...}`), and that error message points here. A
    # bootstrap that silently skipped them left a .env the stack cannot
    # start from and a compose error naming a script that had not
    # generated the key.
    command -v openssl >/dev/null \
        || fail "openssl is required to generate the install's secrets"

    PG_PW=$(openssl rand -hex 16)
    # Per-install pepper mixed into bcrypt hashes. hex 32, matching
    # docs/install/README.md.
    SCRAMBLE=$(openssl rand -hex 32)
    # At-rest encryption master key. base64-encoded 32 bytes, matching
    # docs/install/README.md — the app rejects any other shape.
    MASTER=$(openssl rand -base64 32)

    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${PG_PW}|"   .env && rm .env.bak
    sed -i.bak "s|^AA_SCRAMBLE_KEY=.*|AA_SCRAMBLE_KEY=${SCRAMBLE}|"    .env && rm .env.bak
    # `|` is in neither base64 alphabet, so it is a safe sed delimiter
    # for a value that can legitimately contain `/`, `+` and `=`.
    sed -i.bak "s|^AA_MASTER_KEY=.*|AA_MASTER_KEY=${MASTER}|"          .env && rm .env.bak
    printf '   Generated the Postgres password, AA_SCRAMBLE_KEY and AA_MASTER_KEY.\n'
    printf '   Edit .env if you want to override defaults.\n'

    # Fail here rather than in `docker compose up` if .env.example ever
    # loses one of the placeholder lines the seds above rewrite — a sed
    # that matches nothing succeeds silently.
    for required in POSTGRES_PASSWORD AA_SCRAMBLE_KEY AA_MASTER_KEY; do
        grep -q "^${required}=." .env \
            || fail "${required} is missing from the generated .env — check .env.example"
    done
else
    step ".env already exists \u2014 leaving it alone"
fi

step "Building and starting containers"
docker compose up --build -d

step "Waiting for services to report healthy"
attempts=0
while true; do
    unhealthy=$(docker compose ps --format json 2>/dev/null \
        | grep -c '"Health":"starting"' || true)
    if [ "$unhealthy" -eq 0 ]; then break; fi
    attempts=$((attempts + 1))
    if [ "$attempts" -gt 30 ]; then
        printf '   Services still starting after 5 min; check `docker compose ps`.\n'
        break
    fi
    sleep 10
done

NGINX_PORT=$(grep '^NGINX_HTTP_PORT=' .env | cut -d= -f2)
NGINX_PORT=${NGINX_PORT:-8080}

step "Stack is up"
printf '\n'
printf '   Open http://localhost:%s to reach the SPA.\n' "$NGINX_PORT"
printf '   Bootstrap admin lands automatically when AA_BOOTSTRAP_DEFAULT_ADMIN=1.\n'
printf '   See README.md for the post-bootstrap walkthrough.\n'
printf '\n'
printf '   Logs:  docker compose logs -f\n'
printf '   Stop:  docker compose down\n'
printf '\n'
