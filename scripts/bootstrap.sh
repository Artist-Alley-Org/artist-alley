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

    # Replace the placeholder passwords with something locally unique.
    # Caller is expected to edit .env if they want their own values.
    if command -v openssl >/dev/null; then
        ROOT_PW=$(openssl rand -hex 16)
        RS_PW=$(openssl rand -hex 16)
        PG_PW=$(openssl rand -hex 16)
        sed -i.bak "s|^MYSQL_ROOT_PASSWORD=.*|MYSQL_ROOT_PASSWORD=${ROOT_PW}|" .env && rm .env.bak
        sed -i.bak "s|^MYSQL_PASSWORD=.*|MYSQL_PASSWORD=${RS_PW}|"           .env && rm .env.bak
        sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${PG_PW}|"     .env && rm .env.bak
        printf '   Generated random passwords for mysql + postgres.\n'
    fi
    printf '   Edit .env if you want to override defaults.\n'
else
    step ".env already exists \u2014 leaving it alone"
fi

step "Building and starting containers"
docker compose up --build -d

if [ ! -f vendor/autoload.php ]; then
    step "Installing PHP dependencies (vendor/ missing)"
    # COMPOSER_PROCESS_TIMEOUT bumped because google/apiclient-services is huge
    # and routinely exceeds the default 300s unzip window.
    docker compose exec -T \
        -e COMPOSER_PROCESS_TIMEOUT=1800 \
        php composer install --no-interaction --prefer-dist
fi

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
