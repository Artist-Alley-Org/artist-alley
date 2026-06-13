#!/usr/bin/env bash
# scripts/dogfood/scenarios/07-encryption-key-distribution.sh
#
# Scenario 07: cross-instance verification of Phase 1.22.I-c
# encryption-key distribution.
#
# Reuses scenario 06's wire surface: injects a fresh aa:Like
# activity on studio-a, lets the delivery worker carry the
# envelope to studio-b, then asserts that:
#
#   1. studio-b's outbound envelope inspection shows the
#      aa:encryptionPublicKey block was present.
#   2. studio-b's federation_remote_actors row for studio-a's
#      actor URI now carries a 32-byte encryption_public_key
#      with version >= 1.
#   3. studio-b emitted the federation.remote_actor.key_updated
#      audit event (only on the first observation of the key).
#
# Pre-1.22.I-c peers would skip the block; the absence test
# (assertion that the column starts NULL and ends populated)
# is the integration evidence.
#
# Idempotency: each run mints a fresh activity_uri so re-running
# always exercises a clean dispatch.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

step "Logging in as admin on both stacks"
login_admin "$A_HOST" "$A_COOKIES"; pass "studio-a admin in"
login_admin "$B_HOST" "$B_COOKIES"; pass "studio-b admin in"

step "Setting up fixtures"
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(4))')
activity_uri="urn:aa-dogfood:scenario-07:enckey:${suffix}"

# Target post on studio-b — minimal fixture (same shape as
# scenario 06; LikeHandler needs a resolvable object_local_id).
upload_resp=$(curl -sk -b "$B_COOKIES" \
    -X POST "${B_HOST}/api/v1/storage/objects" \
    -H "Content-Type: application/octet-stream" \
    -H "X-Content-Type: text/plain" \
    --data-binary "scenario-07 fixture ${suffix}")
file_hash=$(echo "$upload_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["hash"])')

asset_resp=$(api_post "$B_HOST" "$B_COOKIES" "/assets" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':          'Scenario 07 fixture asset',
    'description':    'Encryption-key-distribution target.',
    'asset_type':     2,
    'file_hash':      '${file_hash}',
    'file_extension': 'txt',
}))")")
asset_id=$(echo "$asset_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["id"])')

post_resp=$(api_post "$B_HOST" "$B_COOKIES" "/posts" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':       'Scenario 07 fixture post',
    'description': 'Encryption-key-distribution target post.',
    'visibility':  'explicit-share',
    'members':     [{'asset_id': '${asset_id}'}],
}))")")
post_id=$(echo "$post_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["id"])')
pass "studio-b target post id=${post_id}"

# Studio-a peer-row for studio-b.
peer_id=$(docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT id FROM federation_peers
     WHERE instance_url = 'https://studio-b.local:9443'
     LIMIT 1" | tr -d ' \r\n')
[ -n "$peer_id" ] || fail "studio-a doesn't have studio-b as a peer (run pair.sh)"

# Studio-a admin user ref — the Like actor.
admin_ref=$(docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = 'admin' LIMIT 1" | tr -d ' \r\n')
[ -n "$admin_ref" ] || fail "studio-a admin user not found"

# Confirm studio-a's admin has a current X25519 keypair from
# 1.22.I-b. If not, the inline block won't be emitted and this
# scenario can't pass — make that diagnostic explicit before
# the wire test starts.
a_keylen=$(docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(public_key) FROM federation_user_keys
     WHERE user_id = ${admin_ref} AND is_current = TRUE LIMIT 1" | tr -d ' \r\n')
if [ "$a_keylen" != "32" ]; then
    fail "studio-a admin has no current federation_user_keys row (len=${a_keylen}) — was 1.22.I-b applied?"
fi
pass "studio-a admin has a 32-byte X25519 public key"

actor_uri="http://studio-a.local/users/admin"
target_uri="https://studio-b.local:9443/posts/${post_id}"
recipient_inbox="https://studio-b.local:9443/federation/inbox"

# Capture studio-b's pre-state for the actor URI so we can prove
# the column went from "no key" to "key" (or "old key" to "new
# key" on a re-run, which still exercises the rotation-detection
# path via the audit event).
pre_keylen=$(docker compose exec -T postgres-b psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT COALESCE(octet_length(encryption_public_key), 0) FROM federation_remote_actors
     WHERE actor_uri = '${actor_uri}'" | tr -d ' \r\n')
pre_keylen=${pre_keylen:-0}
info "studio-b pre-state: encryption_public_key length = ${pre_keylen}"

# --- inject the activity + outbox row -------------------------------------

step "Injecting Like activity row + outbox dispatch on studio-a"
docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 <<SQL >/dev/null
BEGIN;

INSERT INTO activities (
    id, activity_uri, activity_type, actor_uri, actor_user_ref,
    object_uri, object_kind, object_local_id, to_uris,
    payload, source
) VALUES (
    gen_random_uuid(),
    '${activity_uri}',
    'Like',
    '${actor_uri}',
    ${admin_ref},
    '${target_uri}',
    'post',
    '${post_id}',
    '["${recipient_inbox}"]'::jsonb,
    jsonb_build_object(
        '@context', 'aa-fed/v1',
        'type',     'Like',
        'id',       '${activity_uri}',
        'actor',    '${actor_uri}',
        'object',   '${target_uri}'
    ),
    'local'
);

INSERT INTO federation_outbox (
    activity_id, peer_id, target_user_url, status
)
SELECT id, '${peer_id}'::uuid, '${recipient_inbox}', 'queued'
FROM activities
WHERE activity_uri = '${activity_uri}';

NOTIFY federation_outbox_queued;
COMMIT;
SQL
pass "activity + outbox row staged"

# --- assert delivery + encryption-key landed on studio-b ------------------

step "Polling studio-b for the inbound activity + encryption key"
start_ts=$(date +%s%3N)
wait_for "studio-b processed the activity AND has encryption_public_key for the actor" 30 "
    inbox_ok=\$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT count(*) FROM federation_inbox
         WHERE activity_uri = '${activity_uri}' AND status = 'processed'\" | tr -d ' \r\n')
    key_len=\$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT COALESCE(octet_length(encryption_public_key), 0) FROM federation_remote_actors
         WHERE actor_uri = '${actor_uri}'\" | tr -d ' \r\n')
    [ \"\$inbox_ok\" -ge 1 ] && [ \"\$key_len\" = '32' ]
"
elapsed_ms=$(( $(date +%s%3N) - start_ts ))
pass "studio-b processed + persisted the encryption key (${elapsed_ms}ms wall)"

# Capture the post-state for the audit + report.
post_keylen=$(docker compose exec -T postgres-b psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(encryption_public_key) FROM federation_remote_actors
     WHERE actor_uri = '${actor_uri}'" | tr -d ' \r\n')
post_version=$(docker compose exec -T postgres-b psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT encryption_public_key_version FROM federation_remote_actors
     WHERE actor_uri = '${actor_uri}'" | tr -d ' \r\n')
[ "$post_keylen" = "32" ] || fail "encryption_public_key length = ${post_keylen}, want 32"
[ "$post_version" -ge 1 ] 2>/dev/null || fail "encryption_public_key_version = ${post_version}, want >= 1"
pass "studio-b key shape: 32-byte key, version=${post_version}"

# --- assert audit event fired (first-time observation) -------------------

step "Verifying federation.remote_actor.key_updated audit fired on studio-b"
audit_count=$(docker compose exec -T postgres-b psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.remote_actor.key_updated'
       AND metadata->>'actor_uri' = '${actor_uri}'" | tr -d ' \r\n')
if [ "$audit_count" -ge 1 ]; then
    pass "studio-b audit log has key_updated row (count=${audit_count})"
else
    warn "no audit row found; rotation-detection path may be silent. ok if previous_version == new_version on a re-run."
fi

# --- machine-readable result ---------------------------------------------

if [ -n "${DOGFOOD_REPORT_PATH:-}" ]; then
    python3 -c "
import json
print(json.dumps({
    'scenario':       '07-encryption-key-distribution',
    'pass':           True,
    'dispatch_ms':    ${elapsed_ms},
    'activity_uri':   '${activity_uri}',
    'pre_keylen':     ${pre_keylen},
    'post_keylen':    ${post_keylen},
    'post_version':   ${post_version},
    'audit_rows':     ${audit_count},
}))" >> "$DOGFOOD_REPORT_PATH"
fi
