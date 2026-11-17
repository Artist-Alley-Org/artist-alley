#!/usr/bin/env bash
# scripts/dogfood/check-scenario-actor-origins.sh
#
# Static guard for #1526 fixture drift; needs no running stack.
#
# Every federation envelope is now signed by its actor, and both ends
# check the actor URI's origin: studio-a only signs for
# "<site.base_url>/users/<name>", and studio-b only accepts an actor of
# the instance URL it registered studio-a under. pair.sh makes those one
# value (A_URL_FROM_B). A scenario that injects activities with a
# hard-coded actor host fails silently after dispatch, so this check
# fails first:
#
#   1. pair.sh still sets studio-a's site.base_url to A_URL_FROM_B, the
#      URL studio-b registers it under.
#   2. no scenario assigns a literal http(s) actor_uri; actor URIs come
#      from studio_a_actor_uri (lib.sh).
#   3. every scenario that injects activities calls
#      assert_actor_bound_to_peer before staging them.
#
# Usage: scripts/dogfood/check-scenario-actor-origins.sh   (exit 1 on drift)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
D="$ROOT/scripts/dogfood"
bad=0
complain() { printf 'actor-origin check: %s\n' "$*" >&2; bad=1; }

grep -q 'patch_site "studio-a" "$A_HOST" "$a_cookies" "$A_URL_FROM_B"' "$D/pair.sh" \
    || complain "pair.sh no longer sets studio-a's site.base_url to A_URL_FROM_B"

for f in "$D"/scenarios/*.sh; do
    name=$(basename "$f")
    if grep -nE '^[[:space:]]*actor_uri="https?://' "$f" >/dev/null; then
        complain "$name hard-codes actor_uri; use \$(studio_a_actor_uri <user>)"
    fi
    if grep -q 'INSERT INTO activities' "$f"; then
        inject=$(grep -n 'INSERT INTO activities' "$f" | head -1 | cut -d: -f1 || true)
        guard=$(grep -n 'assert_actor_bound_to_peer "' "$f" | head -1 | cut -d: -f1 || true)
        if [ -z "$guard" ] || [ "$guard" -gt "$inject" ]; then
            complain "$name injects activities without calling assert_actor_bound_to_peer first"
        fi
    fi
done

if [ "$bad" -ne 0 ]; then
    exit 1
fi
echo "actor-origin check: OK"
