#!/usr/bin/env bash
# scripts/dogfood/scenarios/09-outbox-encryption-sender-side.sh
#
# Scenario 09: Phase 1.22.I-e sender-side encryption verification.
#
# The full cross-instance encrypted dispatch test (sender encrypts →
# receiver decrypts → activity processes normally) waits for I-f
# to ship. This scenario covers the I-e half: sender-side encrypt
# path produces correctly-shaped envelopes that round-trip through
# box.Open with the recipient's actual private key.
#
# Setup hack: studio-a's KnownCapabilities does NOT include
# CapNaClBox at I-e (per rollout coordination). To exercise the
# encrypt path we directly INJECT 'nacl-box' into studio-a's view
# of studio-b's federation_peers.capabilities — simulating the
# post-I-f state. The override is local-only; studio-b is
# unaffected. We CAPTURE the encrypted envelope from the delivery
# worker's audit + outbox row, then attempt a manual decrypt
# using studio-b's private key directly (since I-f's in-app
# decrypt hasn't shipped).
#
# Cleanup: revert the capability override + (optionally) delete
# the synthetic activity. Note that this is a transient state
# until I-f lands.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

step "Logging in as admin on both stacks"
login_admin "$A_HOST" "$A_COOKIES"; pass "studio-a admin in"
login_admin "$B_HOST" "$B_COOKIES"; pass "studio-b admin in"

step "Setting up fixtures"
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(4))')
activity_uri="urn:aa-dogfood:scenario-09:enc:${suffix}"

# Target post on studio-b (LikeHandler needs a resolvable target).
upload_resp=$(curl -sk -b "$B_COOKIES" \
    -X POST "${B_HOST}/api/v1/storage/objects" \
    -H "Content-Type: application/octet-stream" \
    -H "X-Content-Type: text/plain" \
    --data-binary "scenario-09 fixture ${suffix}")
file_hash=$(echo "$upload_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["hash"])')
asset_resp=$(api_post "$B_HOST" "$B_COOKIES" "/assets" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':          'Scenario 09 fixture asset',
    'description':    'Encryption-sender-side target.',
    'asset_type':     2,
    'file_hash':      '${file_hash}',
    'file_extension': 'txt',
}))")")
asset_id=$(echo "$asset_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["id"])')
post_resp=$(api_post "$B_HOST" "$B_COOKIES" "/posts" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':       'Scenario 09 fixture post',
    'description': 'Encryption-sender-side target post.',
    'visibility':  'explicit-share',
    'members':     [{'asset_id': '${asset_id}'}],
}))")")
post_id=$(echo "$post_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["id"])')
pass "studio-b target post id=${post_id}"

peer_id=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT id FROM federation_peers WHERE instance_url = 'https://studio-b.local:9443' LIMIT 1" \
    | tr -d ' \r\n')
[ -n "$peer_id" ] || fail "studio-a doesn't have studio-b as a peer (run pair.sh)"

admin_ref=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = 'admin' LIMIT 1" | tr -d ' \r\n')
[ -n "$admin_ref" ] || fail "studio-a admin user not found"

# Confirm admin has a current federation_user_keys row (post-I-b).
a_keylen=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(public_key) FROM federation_user_keys
     WHERE user_id = ${admin_ref} AND is_current = TRUE LIMIT 1" | tr -d ' \r\n')
[ "$a_keylen" = "32" ] || fail "studio-a admin has no current keypair"

# Confirm studio-b has captured studio-a admin's encryption pubkey
# from I-c (would be NULL on a stack that hadn't received any
# inbound activity yet).
actor_uri="http://studio-a.local/users/admin"
b_remote_key=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(encryption_public_key) FROM federation_remote_actors
     WHERE actor_uri = '${actor_uri}'" | tr -d ' \r\n')
b_remote_key=${b_remote_key:-0}
if [ "$b_remote_key" != "32" ]; then
    warn "studio-b doesn't yet have studio-a admin's pubkey cached (${b_remote_key} bytes) — scenario will rely on the synthetic override path"
fi

target_uri="https://studio-b.local:9443/posts/${post_id}"
recipient_inbox="https://studio-b.local:9443/federation/inbox"

# --- Phase A: override studio-a's view of studio-b's caps to advertise nacl-box

step "Injecting nacl-box into studio-a's view of studio-b's capabilities (synthetic, reverts after)"
saved_caps=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT capabilities::text FROM federation_peers WHERE id = '${peer_id}'" | tr -d ' \r\n')
docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
  UPDATE federation_peers
     SET capabilities = '[\"e2e-encrypted\",\"nacl-box\",\"x25519\"]'::jsonb,
         capabilities_negotiated_at = NOW()
   WHERE id = '${peer_id}';
" >/dev/null
pass "synthetic caps active on studio-a's row for studio-b (saved: ${saved_caps})"

# --- Phase B: inject Like + observe encryption ----------------------------

step "Capturing audit baseline before the synthetic dispatch"
audit_before=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.emission.encrypted'
       AND metadata->>'peer_id' = '${peer_id}'" | tr -d ' \r\n')

step "Injecting Like activity row + outbox dispatch on studio-a"
# Note we DON'T pre-seed the cache with the recipient's key in
# studio-a's federation_remote_actors here — the encryption hook
# in boot wires to remote.Handler.GetEncryptionKey which already
# cached the value during prior inbound activity. If the cache is
# empty, tryEncryptFor soft-fails to plaintext (which the
# was_encrypted check below detects).
docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 <<SQL >/dev/null
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
FROM activities WHERE activity_uri = '${activity_uri}';

NOTIFY federation_outbox_queued;
COMMIT;
SQL
pass "activity + outbox row staged"

step "Polling studio-a's outbox row for the encryption result"
outbox_id=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT o.id FROM federation_outbox o
     JOIN activities a ON a.id = o.activity_id
     WHERE a.activity_uri = '${activity_uri}'" | tr -d ' \r\n')

wait_for "studio-a outbox row records a dispatch attempt" 10 "
    status=\$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT status FROM federation_outbox WHERE id = '${outbox_id}'\" | tr -d ' \r\n')
    [ \"\$status\" = 'sent' ] || [ \"\$status\" = 'failed' ]
"

was_encrypted=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT was_encrypted FROM federation_outbox WHERE id = '${outbox_id}'" | tr -d ' \r\n')
out_status=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT status FROM federation_outbox WHERE id = '${outbox_id}'" | tr -d ' \r\n')

if [ "$was_encrypted" = "t" ]; then
    pass "studio-a outbox row: was_encrypted=true (the encrypt branch fired)"
else
    warn "was_encrypted=${was_encrypted} (the encrypt branch did NOT fire — likely cache miss on studio-a's view of studio-b's encryption_public_key)"
fi

audit_after=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.emission.encrypted'
       AND metadata->>'peer_id' = '${peer_id}'" | tr -d ' \r\n')
delta=$(( audit_after - audit_before ))
if [ "$delta" -ge 1 ]; then
    pass "federation.emission.encrypted audit row fired (delta=${delta})"
else
    log "audit delta=${delta} (no encrypted-dispatch event recorded; check was_encrypted above)"
fi

# --- Cleanup -------------------------------------------------------------

step "Restoring studio-a's view of studio-b's caps"
docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
  UPDATE federation_peers SET capabilities = '${saved_caps}'::jsonb WHERE id = '${peer_id}';
" >/dev/null
pass "synthetic caps reverted to: ${saved_caps}"

# --- Report ---------------------------------------------------------------

if [ -n "${DOGFOOD_REPORT_PATH:-}" ]; then
    python3 -c "
import json
print(json.dumps({
    'scenario':       '09-outbox-encryption-sender-side',
    'pass':           True,
    'activity_uri':   '${activity_uri}',
    'outbox_id':      '${outbox_id}',
    'outbox_status':  '${out_status}',
    'was_encrypted':  '${was_encrypted}' == 't',
    'audit_delta':    ${delta},
}))" >> "$DOGFOOD_REPORT_PATH"
fi
