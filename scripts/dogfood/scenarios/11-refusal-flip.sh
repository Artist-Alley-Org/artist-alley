#!/usr/bin/env bash
# scripts/dogfood/scenarios/11-refusal-flip.sh
#
# Scenario 11 — Phase 1.22.I-g sender refusal flip.
#
# Validates the sender-side enforcement of the share-sensitivity
# tier's encrypt-or-refuse contract. Two halves:
#
#   Part A — refusal path. Sender refuses to dispatch a RESTRICTED
#            share when the recipient peer doesn't advertise the
#            nacl-box capability. The outbox row transitions to
#            status='refused' with reason
#            encryption_required_but_unavailable, no envelope
#            reaches studio-b, federation.emission.refused audit
#            fires.
#
#   Part B — plaintext-allowed path. Sender CONTINUES to dispatch
#            a PUBLIC share to the same legacy peer using the
#            1.22.D plaintext wire. Backwards-compat invariant:
#            I-g policy MUST NOT break public-tier federation
#            against pre-I-f peers.
#
# # Setup hack
#
# To make studio-b LOOK LIKE a legacy peer without actually
# downgrading the receiver-side binary, we overwrite studio-a's
# row for studio-b in federation_peers + flip the capabilities
# array to `[]`. The override is local-only; studio-b's
# capabilities + binary are unchanged. After the test we restore
# the captured original.
#
# # What the scenario DOES NOT exercise
#
# Receiver-side defense-in-depth (rejecting plaintext envelopes
# for restricted-share targets) is reserved for Phase 1.22.I-h.
# The sender-side enforcement at I-g is the load-bearing
# decision.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

step "Logging in as admin on both stacks"
login_admin "$A_HOST" "$A_COOKIES"; pass "studio-a admin in"
login_admin "$B_HOST" "$B_COOKIES"; pass "studio-b admin in"

step "Setting up fixtures (one RESTRICTED post + one PUBLIC post on studio-b)"
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(4))')

# Inlined per-post fixture create — the previous version used a
# bash function whose output capture suffered from a
# silent-error-envelope failure mode (curl exits 0 on 4xx, so
# `local resp=$(api_post ...)` captured an error body, and the
# downstream python parse hit KeyError on 'id' with no
# diagnostic). Inlining matches scenario 09's pattern + the
# parse_id_or_dump helper below surfaces the actual response when
# the parse fails so we get a real error message instead of just
# a traceback.

# Surface the raw response body when the json parse fails, so
# the next nightly debugger doesn't have to guess at why the
# fixture broke.
parse_id_or_dump() {
    local label="$1" body="$2"
    local id
    id=$(echo "$body" | python3 -c 'import sys, json; print(json.load(sys.stdin)["id"])' 2>/dev/null) \
        || fail "${label} response had no 'id' field; body=${body}"
    echo "$id"
}

# --- RESTRICTED post on studio-b ---
restricted_upload_resp=$(curl -sk -b "$B_COOKIES" \
    -X POST "${B_HOST}/api/v1/storage/objects" \
    -H "Content-Type: application/octet-stream" \
    -H "X-Content-Type: text/plain" \
    --data-binary "scenario-11 restricted fixture ${suffix}")
restricted_file_hash=$(echo "$restricted_upload_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["hash"])' 2>/dev/null) \
    || fail "restricted upload response had no 'hash'; body=${restricted_upload_resp}"
restricted_asset_resp=$(api_post "$B_HOST" "$B_COOKIES" "/assets" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':          'Scenario 11 restricted asset',
    'description':    'Refusal-flip restricted target.',
    'asset_type':     2,
    'file_hash':      '${restricted_file_hash}',
    'file_extension': 'txt',
}))")")
restricted_asset_id=$(parse_id_or_dump "restricted asset create" "$restricted_asset_resp")
restricted_post_resp=$(api_post "$B_HOST" "$B_COOKIES" "/posts" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':       'Scenario 11 restricted post',
    'description': 'Refusal-flip restricted target post.',
    'visibility':  'explicit-share',
    'members':     [{'asset_id': '${restricted_asset_id}'}],
}))")")
restricted_post_id=$(parse_id_or_dump "restricted post create" "$restricted_post_resp")
pass "studio-b restricted post id=${restricted_post_id}"

# --- PUBLIC post on studio-b ---
# Note: the openapi PostCreate.visibility enum is
# [private, org-only, followers, explicit-share]; "public" is
# implied by the absence of the field (visibility defaults to
# public-equivalent on the model). We omit visibility entirely
# rather than passing 'public' (which fails strict enum
# validation on post-I-g binaries).
public_upload_resp=$(curl -sk -b "$B_COOKIES" \
    -X POST "${B_HOST}/api/v1/storage/objects" \
    -H "Content-Type: application/octet-stream" \
    -H "X-Content-Type: text/plain" \
    --data-binary "scenario-11 public fixture ${suffix}")
public_file_hash=$(echo "$public_upload_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["hash"])' 2>/dev/null) \
    || fail "public upload response had no 'hash'; body=${public_upload_resp}"
public_asset_resp=$(api_post "$B_HOST" "$B_COOKIES" "/assets" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':          'Scenario 11 public asset',
    'description':    'Refusal-flip public target.',
    'asset_type':     2,
    'file_hash':      '${public_file_hash}',
    'file_extension': 'txt',
}))")")
public_asset_id=$(parse_id_or_dump "public asset create" "$public_asset_resp")
public_post_resp=$(api_post "$B_HOST" "$B_COOKIES" "/posts" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':       'Scenario 11 public post',
    'description': 'Refusal-flip public target post.',
    'members':     [{'asset_id': '${public_asset_id}'}],
}))")")
public_post_id=$(parse_id_or_dump "public post create" "$public_post_resp")
pass "studio-b public post id=${public_post_id}"

peer_id=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT id FROM federation_peers WHERE instance_url = 'https://studio-b.local:9443' LIMIT 1" \
    | tr -d ' \r\n')
[ -n "$peer_id" ] || fail "studio-a doesn't have studio-b as a peer (run pair.sh)"

admin_ref=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = 'admin' LIMIT 1" | tr -d ' \r\n')
[ -n "$admin_ref" ] || fail "studio-a admin user not found"

actor_uri="http://studio-a.local/users/admin"

# --- Part A: refusal path -------------------------------------------------

step "Part A — RESTRICTED share to legacy peer (expect refusal)"
step "Capturing studio-a's current view of studio-b's capabilities (will restore later)"
saved_caps=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT capabilities::text FROM federation_peers WHERE id = '${peer_id}'" | tr -d ' \r\n')
info "saved: ${saved_caps}"

step "Synthetically marking studio-b as a legacy peer (capabilities = [])"
docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
  UPDATE federation_peers
     SET capabilities = '[]'::jsonb,
         capabilities_negotiated_at = NOW()
   WHERE id = '${peer_id}';
" >/dev/null
pass "synthetic legacy caps active on studio-a's row for studio-b"

step "Capturing audit baseline before the restricted dispatch"
audit_before_refused=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.emission.refused'
       AND metadata->>'peer_id' = '${peer_id}'" | tr -d ' \r\n')
audit_before_refused=${audit_before_refused:-0}
info "before refused count: ${audit_before_refused}"

step "Staging RESTRICTED Like activity"
restricted_activity_uri="urn:aa-dogfood:scenario-11:restricted:${suffix}"
restricted_target_uri="https://studio-b.local:9443/posts/${restricted_post_id}"
recipient_inbox="https://studio-b.local:9443/federation/inbox"

docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 <<SQL >/dev/null
BEGIN;

INSERT INTO activities (
    id, activity_uri, activity_type, actor_uri, actor_user_ref,
    object_uri, object_kind, object_local_id, to_uris,
    payload, source
) VALUES (
    gen_random_uuid(),
    '${restricted_activity_uri}',
    'Like',
    '${actor_uri}',
    ${admin_ref},
    '${restricted_target_uri}',
    'post',
    '${restricted_post_id}',
    '["${recipient_inbox}"]'::jsonb,
    jsonb_build_object(
        '@context', 'aa-fed/v1',
        'type',     'Like',
        'id',       '${restricted_activity_uri}',
        'actor',    '${actor_uri}',
        'object',   '${restricted_target_uri}'
    ),
    'local'
);

-- For the dogfood scenario we don't have a real share row yet so
-- we inject the outbox row directly with sensitivity=restricted —
-- emulates what the resolver dispatcher would write once shares
-- carry sensitivity metadata. The Worker's policy gate runs on
-- the sensitivity column regardless of how the row landed.
INSERT INTO federation_outbox (
    activity_id, peer_id, target_user_url, status, sensitivity
)
SELECT id, '${peer_id}'::uuid, '${recipient_inbox}', 'queued', 'restricted'
FROM activities WHERE activity_uri = '${restricted_activity_uri}';

NOTIFY federation_outbox_queued;
COMMIT;
SQL
pass "restricted activity + outbox row staged"

step "Polling studio-a's outbox row for the refusal terminal state"
restricted_outbox_id=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT o.id FROM federation_outbox o
     JOIN activities a ON a.id = o.activity_id
     WHERE a.activity_uri = '${restricted_activity_uri}'" | tr -d ' \r\n')

wait_for "studio-a restricted outbox row transitions to refused" 15 "
    status=\$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT status FROM federation_outbox WHERE id = '${restricted_outbox_id}'\" | tr -d ' \r\n')
    [ \"\$status\" = 'refused' ]
"

refused_reason=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT refused_reason FROM federation_outbox WHERE id = '${restricted_outbox_id}'" | tr -d ' \r\n')
[ "$refused_reason" = "encryption_required_but_unavailable" ] \
    || fail "refused_reason: got '${refused_reason}' want 'encryption_required_but_unavailable'"
pass "studio-a outbox row: status=refused, reason=encryption_required_but_unavailable"

audit_after_refused=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.emission.refused'
       AND metadata->>'peer_id' = '${peer_id}'" | tr -d ' \r\n')
audit_after_refused=${audit_after_refused:-0}
refused_delta=$(( audit_after_refused - audit_before_refused ))
[ "$refused_delta" -ge 1 ] \
    || fail "federation.emission.refused audit did not fire (delta=${refused_delta})"
pass "studio-a fired federation.emission.refused (delta=${refused_delta})"

step "Confirming studio-b's inbox saw NO envelope for the refused activity"
b_inbox_count=$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \
    "SELECT COUNT(*) FROM federation_inbox WHERE activity_uri = '${restricted_activity_uri}'" | tr -d ' \r\n')
[ "$b_inbox_count" = "0" ] \
    || fail "studio-b unexpectedly received the refused envelope (count=${b_inbox_count})"
pass "studio-b inbox: 0 rows for the refused activity (no envelope dispatched)"

# --- Part B: plaintext-allowed path -------------------------------------

step "Part B — PUBLIC share to legacy peer (expect plaintext dispatch)"
step "Staging PUBLIC Like activity against the same legacy peer caps"
public_activity_uri="urn:aa-dogfood:scenario-11:public:${suffix}"
public_target_uri="https://studio-b.local:9443/posts/${public_post_id}"

docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 <<SQL >/dev/null
BEGIN;

INSERT INTO activities (
    id, activity_uri, activity_type, actor_uri, actor_user_ref,
    object_uri, object_kind, object_local_id, to_uris,
    payload, source
) VALUES (
    gen_random_uuid(),
    '${public_activity_uri}',
    'Like',
    '${actor_uri}',
    ${admin_ref},
    '${public_target_uri}',
    'post',
    '${public_post_id}',
    '["${recipient_inbox}"]'::jsonb,
    jsonb_build_object(
        '@context', 'aa-fed/v1',
        'type',     'Like',
        'id',       '${public_activity_uri}',
        'actor',    '${actor_uri}',
        'object',   '${public_target_uri}'
    ),
    'local'
);

INSERT INTO federation_outbox (
    activity_id, peer_id, target_user_url, status, sensitivity
)
SELECT id, '${peer_id}'::uuid, '${recipient_inbox}', 'queued', 'public'
FROM activities WHERE activity_uri = '${public_activity_uri}';

NOTIFY federation_outbox_queued;
COMMIT;
SQL
pass "public activity + outbox row staged"

step "Polling studio-a's outbox row for the plaintext-sent terminal state"
public_outbox_id=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT o.id FROM federation_outbox o
     JOIN activities a ON a.id = o.activity_id
     WHERE a.activity_uri = '${public_activity_uri}'" | tr -d ' \r\n')

wait_for "studio-a public outbox row transitions to sent" 15 "
    status=\$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT status FROM federation_outbox WHERE id = '${public_outbox_id}'\" | tr -d ' \r\n')
    [ \"\$status\" = 'sent' ]
"

public_was_encrypted=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT was_encrypted FROM federation_outbox WHERE id = '${public_outbox_id}'" | tr -d ' \r\n')
[ "$public_was_encrypted" = "f" ] \
    || fail "public was_encrypted: got '${public_was_encrypted}' want 'f' (plaintext expected)"
pass "studio-a public outbox row: sent, was_encrypted=false (plaintext path)"

step "Confirming studio-b RECEIVED the public envelope"
wait_for "studio-b inbox row exists for the public activity" 10 "
    n=\$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT COUNT(*) FROM federation_inbox WHERE activity_uri = '${public_activity_uri}'\" | tr -d ' \r\n')
    [ \"\$n\" -ge 1 ]
"
pass "studio-b inbox: row landed for the public activity"

step "Confirming NO additional refused audit fired during Part B"
audit_after_part_b=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM audit_events
     WHERE event_type = 'federation.emission.refused'
       AND metadata->>'peer_id' = '${peer_id}'" | tr -d ' \r\n')
audit_after_part_b=${audit_after_part_b:-0}
[ "$audit_after_part_b" = "$audit_after_refused" ] \
    || fail "refused audit grew during Part B (before=${audit_after_refused} after=${audit_after_part_b}); public tier MUST NOT refuse"
pass "no additional refused audit during Part B (public tier delivered plaintext)"

# --- Cleanup -------------------------------------------------------------

step "Restoring studio-a's view of studio-b's caps"
docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
  UPDATE federation_peers SET capabilities = '${saved_caps}'::jsonb WHERE id = '${peer_id}';
" >/dev/null
pass "synthetic caps reverted to: ${saved_caps}"

# --- Report --------------------------------------------------------------

if [ -n "${DOGFOOD_REPORT_PATH:-}" ]; then
    python3 -c "
import json
print(json.dumps({
    'scenario':                  '11-refusal-flip',
    'pass':                      True,
    'restricted_activity_uri':   '${restricted_activity_uri}',
    'restricted_outbox_id':      '${restricted_outbox_id}',
    'restricted_refused_reason': '${refused_reason}',
    'public_activity_uri':       '${public_activity_uri}',
    'public_outbox_id':          '${public_outbox_id}',
    'public_was_encrypted':      False,
    'refused_audit_delta_partA': ${refused_delta},
    'refused_audit_delta_partB': 0,
}))" >> "$DOGFOOD_REPORT_PATH"
fi
