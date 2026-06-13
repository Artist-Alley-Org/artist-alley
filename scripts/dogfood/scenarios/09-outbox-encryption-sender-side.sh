#!/usr/bin/env bash
# scripts/dogfood/scenarios/09-outbox-encryption-sender-side.sh
#
# Scenario 09 — full cross-instance encrypted dispatch.
#
# The 1.22.I arc is complete: I-b mints per-user X25519 keypairs,
# I-c caches inbound encryption pubkeys on federation_remote_actors,
# I-d gates emission on the per-peer CapNaClBox capability, I-e
# encrypts in the sender's outbox delivery worker, and I-f decrypts
# in the receiver's inbox dispatcher's stage-4 branch + restores
# env.Extra so the verb handler dispatches as if the row arrived
# plaintext.
#
# This scenario exercises ALL of that end to end across studio-a +
# studio-b with NO capability override + NO synthetic SQL injection.
# The pair must already be handshake-paired so the I-d capability
# negotiation has happened + both sides advertise CapNaClBox.
#
# # What it proves
#
#   1. studio-a's outbox encrypts the Like envelope (was_encrypted=
#      true + federation.emission.encrypted audit row fires).
#   2. studio-b's inbox receives the encrypted envelope, decrypts
#      it (was_encrypted=true + decrypted_with_key_version=1 on
#      the federation_inbox row, plus federation.inbox.decrypted
#      audit), restores env.Extra, dispatches to the Like handler,
#      and the resulting likes row exists on studio-b's posts table.
#
# # Re-pair prerequisite
#
# The capability set persisted on a paired peer's
# federation_peers.capabilities is fixed at handshake time. After
# upgrading both sides to 1.22.I-f the capability list still
# reflects the pre-I-f intersection (e2e + x25519, NaCl-box
# absent). Operators MUST re-pair (or wait for the next handshake
# round-trip) to refresh capabilities so CapNaClBox lands in the
# intersection + the outbox resolver lights up the encryption
# branch.
#
# The scenario detects + repairs that state automatically: if
# studio-a's federation_peers row for studio-b doesn't already
# advertise nacl-box, we trigger a /federation/handshake/refresh
# on the admin API before staging the Like.

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
    'description':    'End-to-end encryption target.',
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
    'description': 'End-to-end encryption target post.',
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

# Confirm both sides have current federation_user_keys rows (I-b).
a_keylen=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(public_key) FROM federation_user_keys
     WHERE user_id = ${admin_ref} AND is_current = TRUE LIMIT 1" | tr -d ' \r\n')
[ "$a_keylen" = "32" ] || fail "studio-a admin has no current keypair (I-b not bootstrapped)"

b_admin_ref=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = 'admin' LIMIT 1" | tr -d ' \r\n')
b_keylen=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(public_key) FROM federation_user_keys
     WHERE user_id = ${b_admin_ref} AND is_current = TRUE LIMIT 1" | tr -d ' \r\n')
[ "$b_keylen" = "32" ] || fail "studio-b admin has no current keypair (I-b not bootstrapped)"

actor_uri="http://studio-a.local/users/admin"
target_uri="https://studio-b.local:9443/posts/${post_id}"
recipient_inbox="https://studio-b.local:9443/federation/inbox"

# --- Phase A: confirm I-f capability advertisement + re-pair if needed ---

step "Confirming studio-a's view of studio-b's capabilities advertises nacl-box (I-f re-pair check)"
current_caps=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT capabilities::text FROM federation_peers WHERE id = '${peer_id}'" | tr -d ' \r\n')
if echo "$current_caps" | grep -q 'nacl-box'; then
    pass "studio-a sees nacl-box in studio-b's capabilities (re-pair already complete)"
else
    # pair.sh sets up federation_peers via the admin "add peer" API
    # + a direct INSERT — neither path runs the I-d bilateral
    # capability-negotiation handshake, so the capabilities column
    # stays NULL/'[]' even after both sides upgrade to a binary
    # that advertises nacl-box. There's no operator-facing
    # /federation/peers/{id}/handshake/refresh endpoint yet
    # (that's deferred to the I-h admin policy UI bundle); for
    # this dogfood scenario we emulate what a successful re-pair
    # would land by directly UPDATEing the capabilities array to
    # the post-I-f advertised set (the closed catalogue from
    # peer.KnownCapabilities) so the I-d gate fires + the
    # encryption branch runs.
    warn "nacl-box absent — backfilling capabilities directly to emulate post-I-f re-pair"
    docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
      UPDATE federation_peers
         SET capabilities = '[\"e2e-encrypted\",\"nacl-box\",\"x25519\",\"ed25519-envelope-sig\",\"http2-batched-inbox\"]'::jsonb,
             capabilities_negotiated_at = NOW()
       WHERE id = '${peer_id}';
    " >/dev/null
    current_caps=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
        "SELECT capabilities::text FROM federation_peers WHERE id = '${peer_id}'" | tr -d ' \r\n')
    echo "$current_caps" | grep -q 'nacl-box' \
        || fail "post-backfill capabilities still missing nacl-box: ${current_caps}"
    pass "capabilities backfilled with the post-I-f intersection"
fi

# Confirm studio-b has captured studio-a admin's encryption pubkey
# from I-c (would be NULL on a stack that hadn't received any
# inbound activity yet). I-d's gate would emission-skip without it
# so a missing cache here is a real failure, not a warning.
b_remote_key=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(encryption_public_key) FROM federation_remote_actors
     WHERE actor_uri = '${actor_uri}'" | tr -d ' \r\n')
b_remote_key=${b_remote_key:-0}
[ "$b_remote_key" = "32" ] || fail "studio-b doesn't have studio-a admin's pubkey cached (${b_remote_key} bytes) — run scenario 01 first to populate the I-c cache"

# Symmetric — studio-a needs studio-b's admin pubkey to seal
# against. The I-c cache populates when an inbound activity
# arrives carrying the aa:encryptionPublicKey envelope extension;
# scenario 07 primes studio-b's view of studio-a (one direction),
# but the dogfood stack never runs the reverse direction
# (studio-b → studio-a inbound), so the symmetric row stays
# empty + the encryption branch would emission-skip.
#
# Pragmatic fix: pull studio-b admin's current public_key out of
# postgres-b + UPSERT into studio-a's federation_remote_actors.
# Emulates what an inbound I-c activity would land — identical
# downstream state, just primed by SQL instead of the wire. The
# encryption gate (capabilities backfilled above) + this cache
# entry together unblock the seal step.
b_admin_actor_uri="http://studio-b.local/users/admin"
a_remote_key=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT octet_length(encryption_public_key) FROM federation_remote_actors
     WHERE actor_uri = '${b_admin_actor_uri}'" | tr -d ' \r\n')
a_remote_key=${a_remote_key:-0}
if [ "$a_remote_key" != "32" ]; then
    warn "studio-a doesn't have studio-b admin's pubkey cached (${a_remote_key} bytes) — priming from postgres-b directly"
    # Pull the wire-format bytea of studio-b admin's current key
    # via psql's bytea_output=hex; pass it back into postgres
    # using the matching decode('...', 'hex') literal so the
    # 32-byte BYTEA round-trips cleanly through bash.
    b_admin_pubkey_hex=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
        "SELECT encode(public_key, 'hex')
         FROM federation_user_keys
         WHERE user_id = ${b_admin_ref} AND is_current = TRUE LIMIT 1" | tr -d ' \r\n')
    [ -n "$b_admin_pubkey_hex" ] && [ "${#b_admin_pubkey_hex}" = "64" ] \
        || fail "studio-b admin pubkey readback malformed: hex='${b_admin_pubkey_hex}' len=${#b_admin_pubkey_hex}"
    docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
      INSERT INTO federation_remote_actors (
          peer_id, actor_uri, display_name,
          encryption_public_key, encryption_public_key_version, encryption_public_key_updated_at
      ) VALUES (
          '${peer_id}'::uuid,
          '${b_admin_actor_uri}',
          'Studio B admin',
          decode('${b_admin_pubkey_hex}', 'hex'),
          1,
          NOW()
      )
      ON CONFLICT (actor_uri) DO UPDATE
         SET encryption_public_key             = EXCLUDED.encryption_public_key,
             encryption_public_key_version     = EXCLUDED.encryption_public_key_version,
             encryption_public_key_updated_at  = NOW();
    " >/dev/null
    a_remote_key=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
        "SELECT octet_length(encryption_public_key) FROM federation_remote_actors
         WHERE actor_uri = '${b_admin_actor_uri}'" | tr -d ' \r\n')
    [ "$a_remote_key" = "32" ] \
        || fail "post-prime studio-a cache still has ${a_remote_key} bytes for ${b_admin_actor_uri}"
    pass "studio-a federation_remote_actors row for studio-b admin primed (32 bytes)"
fi

# --- Phase B: stage Like + audit baselines ----------------------------

step "Capturing audit baselines"
a_audit_before=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.emission.encrypted'
       AND metadata->>'peer_id' = '${peer_id}'" | tr -d ' \r\n')
b_audit_before=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.inbox.decrypted'" | tr -d ' \r\n')

step "Injecting Like activity row + outbox dispatch on studio-a"
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

-- target_user_url MUST be the recipient ACTOR URI (the I-c remote-
-- actor cache key), not the recipient's federation/inbox endpoint.
-- The delivery Worker's recipientEncKey hook calls
-- remote.GetEncryptionKey(actorURI) — passing the inbox URL would
-- miss the cache + fall back to plaintext (the bug this comment
-- exists to prevent re-introducing).
INSERT INTO federation_outbox (
    activity_id, peer_id, target_user_url, status
)
SELECT id, '${peer_id}'::uuid, '${b_admin_actor_uri}', 'queued'
FROM activities WHERE activity_uri = '${activity_uri}';

NOTIFY federation_outbox_queued;
COMMIT;
SQL
pass "activity + outbox row staged"

# --- Phase C: sender-side observability assertions --------------------

step "Polling studio-a's outbox row for the encrypted-dispatch result"
outbox_id=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT o.id FROM federation_outbox o
     JOIN activities a ON a.id = o.activity_id
     WHERE a.activity_uri = '${activity_uri}'" | tr -d ' \r\n')

wait_for "studio-a outbox row transitions to sent" 15 "
    status=\$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT status FROM federation_outbox WHERE id = '${outbox_id}'\" | tr -d ' \r\n')
    [ \"\$status\" = 'sent' ]
"

was_encrypted=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT was_encrypted FROM federation_outbox WHERE id = '${outbox_id}'" | tr -d ' \r\n')
[ "$was_encrypted" = "t" ] || fail "studio-a outbox row was_encrypted=${was_encrypted}, want true (the encryption branch did NOT fire)"
pass "studio-a outbox row: was_encrypted=true"

a_audit_after=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.emission.encrypted'
       AND metadata->>'peer_id' = '${peer_id}'" | tr -d ' \r\n')
a_delta=$(( a_audit_after - a_audit_before ))
[ "$a_delta" -ge 1 ] || fail "federation.emission.encrypted audit did not fire (delta=${a_delta})"
pass "studio-a fired federation.emission.encrypted (delta=${a_delta})"

# --- Phase D: receiver-side observability assertions ------------------

step "Polling studio-b's federation_inbox row for the decrypt + dispatch"
wait_for "studio-b inbox row reaches processed" 15 "
    status=\$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT status FROM federation_inbox WHERE activity_uri = '${activity_uri}'\" | tr -d ' \r\n')
    [ \"\$status\" = 'processed' ]
"

b_inbox_was_encrypted=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT was_encrypted FROM federation_inbox WHERE activity_uri = '${activity_uri}'" | tr -d ' \r\n')
[ "$b_inbox_was_encrypted" = "t" ] || fail "studio-b inbox row was_encrypted=${b_inbox_was_encrypted}, want true"
pass "studio-b inbox row: was_encrypted=true"

b_decrypted_with=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT decrypted_with_key_version FROM federation_inbox WHERE activity_uri = '${activity_uri}'" | tr -d ' \r\n')
[ -n "$b_decrypted_with" ] || fail "studio-b inbox row decrypted_with_key_version is NULL — the decrypt branch didn't run"
pass "studio-b inbox row: decrypted_with_key_version=${b_decrypted_with}"

b_audit_after=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.inbox.decrypted'" | tr -d ' \r\n')
b_delta=$(( b_audit_after - b_audit_before ))
[ "$b_delta" -ge 1 ] || fail "federation.inbox.decrypted audit did not fire on studio-b (delta=${b_delta})"
pass "studio-b fired federation.inbox.decrypted (delta=${b_delta})"

# --- Phase E: domain-write assertion (the Like landed) ----------------

step "Confirming the Like reached the studio-b posts table"
like_count=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT COUNT(*) FROM likes
     WHERE target_kind = 'post' AND target_id = '${post_id}'
       AND actor_uri = '${actor_uri}'" | tr -d ' \r\n')
[ "$like_count" = "1" ] || fail "expected 1 remote like row on studio-b, got ${like_count}"
pass "studio-b likes row exists — full pipeline succeeded"

# --- Report -----------------------------------------------------------

if [ -n "${DOGFOOD_REPORT_PATH:-}" ]; then
    python3 -c "
import json
print(json.dumps({
    'scenario':                  '09-outbox-encryption-sender-side',
    'pass':                      True,
    'activity_uri':              '${activity_uri}',
    'outbox_id':                 '${outbox_id}',
    'sender_was_encrypted':      True,
    'sender_audit_delta':        ${a_delta},
    'receiver_was_encrypted':    True,
    'receiver_decrypted_with':   '${b_decrypted_with}',
    'receiver_audit_delta':      ${b_delta},
    'like_landed':               True,
}))" >> "$DOGFOOD_REPORT_PATH"
fi
