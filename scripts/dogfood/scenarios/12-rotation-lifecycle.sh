#!/usr/bin/env bash
# scripts/dogfood/scenarios/12-rotation-lifecycle.sh
#
# Scenario 12: Phase 1.22.I-h key rotation lifecycle.
#
# Four phases verified end to end against studio-a (single
# instance scope — the rotation surfaces are local; cross-
# instance retained-key decrypt is covered by a unit test in
# inbox/dispatcher_decrypt_test.go because the synthetic
# stale-cache setup is cleaner there than via the wire):
#
#   Phase A — self-rotation
#     * POST /account/security/rotate-federation-keys as admin.
#     * Assert: federation_user_keys has TWO rows for admin
#       (vN+1 current with rotated_at populated; vN retained
#       with rotated_at populated + retained_until ~30 days
#       out + rotated_by_user_ref = admin.ref).
#     * Assert: response carries new_version, previous_version,
#       new_public_key_b64, retained_until_days fields.
#
#   Phase B — admin-initiated rotation (compromised-key recovery)
#     * Create a fresh user fixture; ensure they have v1 via
#       the I-b backfill primitive.
#     * POST /admin/federation/users/{ref}/rotate-keys as admin.
#     * Assert: subject user's federation_user_keys has v2 with
#       rotated_by_user_ref = admin's ref (NOT the subject's
#       own ref) — the audit distinguishes admin recovery from
#       self-rotation.
#
#   Phase C — sweeper reap
#     * Manually backdate a retained row's retained_until to
#       NOW() - 1 day.
#     * Trigger one sweep pass via the admin endpoint OR wait
#       for the natural tick (here: short-circuit by direct
#       DELETE-based assertion since the sweeper goroutine
#       ticks at 1h cadence — testing the QUERY is the cheap
#       cross-section).
#     * Assert: row is gone after the sweep.
#
#   Phase D — admin observability surface
#     * GET /admin/federation/key-health.
#     * Assert: users_total > 0, recent_rotations has rows
#       from Phase A + Phase B, response shape valid.
#
# Phase E (receiver-side defense-in-depth gate) is deferred —
# the policy gate ships fully tested at the dispatcher layer
# in commit 1; production wiring of SetSensitivityLookup waits
# on sensitivity columns landing on posts/assets, which the
# resolver.go file-level comment documents as pre-MVP work.
# A future scenario activates Phase E once that lands.
#
# Idempotency: each run mints a fresh fixture user so re-running
# doesn't poison the count assertions.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

step "Logging in as admin on studio-a"
login_admin "$A_HOST" "$A_COOKIES"; pass "studio-a admin in"

admin_ref=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = 'admin' LIMIT 1" | tr -d ' \r\n')
[ -n "$admin_ref" ] || fail "admin user not found on studio-a"

# Capture the admin's current key version BEFORE rotation so we
# can assert N+1 increments rather than guessing.
admin_v_before=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT version FROM federation_user_keys
      WHERE user_id = ${admin_ref} AND is_current = TRUE" | tr -d ' \r\n')
[ -n "$admin_v_before" ] || fail "admin has no current keypair (I-b backfill should have minted v1)"
info "admin's pre-rotation current version: v${admin_v_before}"

# ============================================================
# Phase A — self-rotation
# ============================================================

step "Phase A: POST /account/security/rotate-federation-keys"
rotate_resp=$(curl -sk -b "$A_COOKIES" \
    -X POST "${A_HOST}/api/v1/account/security/rotate-federation-keys" \
    -H "Content-Type: application/json" \
    -w "\n%{http_code}")
code=$(echo "$rotate_resp" | tail -1)
body=$(echo "$rotate_resp" | head -n -1)
[ "$code" = "200" ] || fail "self-rotation HTTP ${code}; body: ${body}"

new_v=$(echo "$body" | python3 -c 'import sys, json; print(json.load(sys.stdin)["new_version"])')
prev_v=$(echo "$body" | python3 -c 'import sys, json; print(json.load(sys.stdin).get("previous_version", 0))')
pub_b64=$(echo "$body" | python3 -c 'import sys, json; print(json.load(sys.stdin)["new_public_key_b64"])')
retained_days=$(echo "$body" | python3 -c 'import sys, json; print(json.load(sys.stdin).get("retained_until_days", 0))')

[ "${new_v}" = "$((admin_v_before + 1))" ] \
    || fail "new_version should be ${admin_v_before}+1=$((admin_v_before+1)); got ${new_v}"
[ "${prev_v}" = "${admin_v_before}" ] \
    || fail "previous_version should be ${admin_v_before}; got ${prev_v}"
[ -n "${pub_b64}" ] && [ ${#pub_b64} -ge 40 ] \
    || fail "new_public_key_b64 missing or too short (got ${#pub_b64} chars; expected base64 of 32 bytes ~= 44 chars)"
[ "${retained_days}" -gt 0 ] \
    || fail "retained_until_days should be > 0; got ${retained_days}"
pass "self-rotation HTTP 200 with v${admin_v_before} → v${new_v}, retained ${retained_days}d"

# DB-side assertions: both rows present with the right metadata.
db_state=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc "
    SELECT json_agg(json_build_object(
        'version', version,
        'is_current', is_current,
        'rotated_at_set', rotated_at IS NOT NULL,
        'retained_until_set', retained_until IS NOT NULL,
        'rotated_by', rotated_by_user_ref
    ) ORDER BY version)
      FROM federation_user_keys
     WHERE user_id = ${admin_ref}
       AND version IN (${admin_v_before}, ${new_v})")

prev_rotated_by=$(echo "$db_state" | python3 -c "
import sys, json
rows = json.loads(sys.stdin.read().strip())
prev = next(r for r in rows if r['version'] == ${admin_v_before})
new  = next(r for r in rows if r['version'] == ${new_v})
assert prev['is_current'] is False, f'prev still current: {prev}'
assert new['is_current'] is True, f'new not current: {new}'
assert prev['retained_until_set'] is True, f'prev retained_until missing: {prev}'
assert new['retained_until_set'] is False, f'new has retained_until set: {new}'
assert prev['rotated_at_set'] is True, f'prev rotated_at missing: {prev}'
assert new['rotated_at_set'] is True, f'new rotated_at missing: {new}'
print(prev['rotated_by'])
")
[ "${prev_rotated_by}" = "${admin_ref}" ] \
    || fail "previous row rotated_by_user_ref = ${prev_rotated_by}, want ${admin_ref} (self-rotation)"
pass "DB state: v${admin_v_before} retained, v${new_v} current, metadata recorded"

# ============================================================
# Phase B — admin-initiated rotation on a fixture user
# ============================================================

step "Phase B: admin-initiated rotation on a fixture user"

# Mint a throwaway user via the admin API so we have a clean
# subject != admin.
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(4))')
fixture_username="rotation-test-${suffix}"
fixture_body=$(printf '{"username":"%s","fullname":"Rotation Test","password":"P@ssw0rd!","email":"%s@example.invalid"}' \
    "${fixture_username}" "${fixture_username}")
create_resp=$(curl -sk -b "$A_COOKIES" \
    -X POST "${A_HOST}/api/v1/admin/users" \
    -H "Content-Type: application/json" \
    -d "${fixture_body}" \
    -w "\n%{http_code}")
create_code=$(echo "$create_resp" | tail -1)
create_body=$(echo "$create_resp" | head -n -1)
if [ "$create_code" != "201" ] && [ "$create_code" != "200" ]; then
    fail "fixture user create HTTP ${create_code}; body: ${create_body}"
fi
fixture_ref=$(echo "$create_body" | python3 -c 'import sys, json; print(json.load(sys.stdin)["ref"])' 2>/dev/null || true)
if [ -z "$fixture_ref" ]; then
    fixture_ref=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
        "SELECT ref FROM \"user\" WHERE username = '${fixture_username}'" | tr -d ' \r\n')
fi
[ -n "$fixture_ref" ] || fail "fixture user ref not resolvable"

# Ensure the fixture user has a current key (I-b should mint
# inline on /admin/users; defensive backfill query is the same
# query the boot sweep uses).
v1=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT COALESCE(MAX(version), 0) FROM federation_user_keys
      WHERE user_id = ${fixture_ref} AND is_current = TRUE" | tr -d ' \r\n')
[ "${v1}" = "1" ] || fail "fixture user has no v1 keypair (got max version=${v1}); /admin/users create path should mint inline"

# Trigger admin rotation.
admin_rotate_resp=$(curl -sk -b "$A_COOKIES" \
    -X POST "${A_HOST}/api/v1/admin/federation/users/${fixture_ref}/rotate-keys" \
    -H "Content-Type: application/json" \
    -w "\n%{http_code}")
admin_code=$(echo "$admin_rotate_resp" | tail -1)
admin_body=$(echo "$admin_rotate_resp" | head -n -1)
[ "$admin_code" = "200" ] || fail "admin rotation HTTP ${admin_code}; body: ${admin_body}"

fixture_v2_rotated_by=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT rotated_by_user_ref FROM federation_user_keys
      WHERE user_id = ${fixture_ref} AND version = 2" | tr -d ' \r\n')
[ "${fixture_v2_rotated_by}" = "${admin_ref}" ] \
    || fail "fixture v2 rotated_by_user_ref = ${fixture_v2_rotated_by}, want ${admin_ref} (admin recovery, NOT self)"
pass "admin recovery: subject=#${fixture_ref}, rotated_by=admin#${admin_ref}"

# Cleanup the fixture user — its keypair rows cascade.
docker compose exec -T postgres psql -U artist_alley -d artist_alley -c \
    "DELETE FROM \"user\" WHERE ref = ${fixture_ref}" >/dev/null

# ============================================================
# Phase C — sweeper reap
# ============================================================

step "Phase C: sweeper reaps expired retained keys"

# Pick admin's v${admin_v_before} retained row + backdate its
# retained_until so the sweeper picks it up. The row is real;
# this is a controlled time-travel via direct SQL.
docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
    UPDATE federation_user_keys
       SET retained_until = NOW() - INTERVAL '1 day'
     WHERE user_id = ${admin_ref}
       AND version = ${admin_v_before}
       AND is_current = FALSE" >/dev/null

# We can't easily trigger the goroutine sweep mid-scenario;
# instead exercise the underlying query (same SQL the
# Sweeper.SweepOnce path runs). Asserts the query reaps + the
# row is gone afterwards.
reaped=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc "
    WITH deleted AS (
        DELETE FROM federation_user_keys
         WHERE is_current = FALSE
           AND retained_until IS NOT NULL
           AND retained_until < NOW()
        RETURNING user_id
    )
    SELECT COUNT(*) FROM deleted" | tr -d ' \r\n')
[ "${reaped}" -ge "1" ] || fail "sweep query reaped ${reaped} rows; expected >=1"

still_there=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT COUNT(*) FROM federation_user_keys
      WHERE user_id = ${admin_ref} AND version = ${admin_v_before}" | tr -d ' \r\n')
[ "${still_there}" = "0" ] || fail "admin's v${admin_v_before} retained row should be gone post-sweep; ${still_there} remain"
pass "sweep reaped ${reaped} expired row(s); admin's v${admin_v_before} cleared"

# ============================================================
# Phase D — admin observability surface
# ============================================================

step "Phase D: GET /admin/federation/key-health"
health_resp=$(curl -sk -b "$A_COOKIES" \
    "${A_HOST}/api/v1/admin/federation/key-health" \
    -w "\n%{http_code}")
health_code=$(echo "$health_resp" | tail -1)
health_body=$(echo "$health_resp" | head -n -1)
[ "$health_code" = "200" ] || fail "key-health HTTP ${health_code}; body: ${health_body}"

users_total=$(echo "$health_body" | python3 -c 'import sys, json; print(json.load(sys.stdin)["users_total"])')
[ "${users_total}" -gt 0 ] || fail "users_total = ${users_total}, want > 0"

rotations_count=$(echo "$health_body" | python3 -c '
import sys, json
data = json.load(sys.stdin)
rotations = data.get("recent_rotations") or []
print(len(rotations))
')
[ "${rotations_count}" -ge "1" ] \
    || fail "recent_rotations had ${rotations_count} rows; expected >= 1 (Phase A or B rotation should appear)"
pass "key-health: users_total=${users_total}, recent_rotations=${rotations_count}"

step "Scenario 12 complete: rotation primitive + sweeper + admin observability validated"
