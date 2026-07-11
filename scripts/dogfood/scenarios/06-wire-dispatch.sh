#!/usr/bin/env bash
# scripts/dogfood/scenarios/06-wire-dispatch.sh
#
# Synthetic wire-dispatch test: inject an aa:Like activity row
# directly into studio-a's activities ledger + federation_outbox,
# then assert the delivery worker signed + sent it AND studio-b's
# inbox dispatcher processed it as a `Like`.
#
# Why: scenarios 01-05 from ADR 0049 cover user-facing flows.
# Several (02 share→mirror, 03 cascade, 04 restricted-refuse) need
# product code that's stubbed today (aa:Share inbound is a stub
# per app/internal/federation/inbox/handler_stub.go). Until those
# real handlers land, this scenario gives us an honest E2E test
# of the WIRE — envelope construction, HTTP-Sig signing, transport,
# inbox classification, LikeHandler dispatch, federation_inbox
# row. That's the layer the dogfood week actually exercises.
#
# Idempotency: each run mints a fresh activity_uri so a re-run
# always inserts a new row + tests fresh dispatch latency.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

step "Logging in as admin on both stacks"
login_admin "$A_HOST" "$A_COOKIES"; pass "studio-a admin in"
login_admin "$B_HOST" "$B_COOKIES"; pass "studio-b admin in"

step "Setting up fixtures"
# Mint a unique activity-uri suffix so re-runs don't collide.
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(4))')
activity_uri="urn:aa-dogfood:scenario-06:like:${suffix}"

# Create a real local post on studio-b (the target of the Like)
# so LikeHandler can resolve object_local_id → an existing post row.
upload_resp=$(curl -sk -b "$B_COOKIES" \
    -X POST "${B_HOST}/api/v1/storage/objects" \
    -H "Content-Type: application/octet-stream" \
    -H "X-Content-Type: text/plain" \
    --data-binary "scenario-06 fixture ${suffix}")
file_hash=$(echo "$upload_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["hash"])')

asset_resp=$(api_post "$B_HOST" "$B_COOKIES" "/assets" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':          'Scenario 06 fixture asset',
    'description':    'Dispatch-wire-test target.',
    'asset_type':     2,
    'file_hash':      '${file_hash}',
    'file_extension': 'txt',
}))")")
asset_id=$(echo "$asset_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["id"])')

post_resp=$(api_post "$B_HOST" "$B_COOKIES" "/posts" \
    "$(python3 -c "
import json
print(json.dumps({
    'title':       'Scenario 06 fixture post',
    'description': 'Dispatch-wire-test target post.',
    'visibility':  'explicit-share',
    'members':     [{'asset_id': '${asset_id}'}],
}))")")
post_id=$(echo "$post_resp" | python3 -c 'import sys, json; print(json.load(sys.stdin)["id"])')
pass "studio-b target post id=${post_id}"

# Studio-a's peer-id-for-studio-b — needed for the outbox row.
peer_id=$(docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT id FROM federation_peers
     WHERE instance_url = 'https://studio-b.local:9443'
     LIMIT 1" | tr -d ' \r\n')
[ -n "$peer_id" ] || fail "studio-a doesn't have studio-b as a peer (run pair.sh)"

# Studio-a admin user (any local user works as the Like actor).
admin_ref=$(docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = 'admin' LIMIT 1" | tr -d ' \r\n')
[ -n "$admin_ref" ] || fail "studio-a admin user not found"

# Studio-a federation actor URI for the admin user.
actor_uri="http://studio-a.local/users/admin"
target_uri="https://studio-b.local:9443/posts/${post_id}"
recipient_inbox="https://studio-b.local:9443/federation/inbox"

# --- inject the activity + outbox row -------------------------------------

step "Injecting Like activity row + outbox dispatch on studio-a"
# The delivery worker reads activities then signs/sends; we just
# stage the rows. The LISTEN/NOTIFY trigger on federation_outbox
# wakes the worker immediately.
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

# --- assert delivery + inbox processing -----------------------------------

step "Polling studio-b inbox for the Like (sub-1s p99 target)"
start_ts=$(date +%s%3N)
wait_for "studio-b processed Like with activity_uri=${activity_uri}" 30 "
    count=\$(docker compose exec -T postgres-b psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT count(*) FROM federation_inbox
         WHERE activity_uri = '${activity_uri}' AND status = 'processed'\" | tr -d ' \r\n')
    [ \"\$count\" -ge 1 ]
"
elapsed_ms=$(( $(date +%s%3N) - start_ts ))
pass "studio-b processed the Like (${elapsed_ms}ms wall)"

# --- assert the Like row landed on the social side -----------------------

step "Verifying the Like row was inserted on studio-b"
like_count=$(docker compose exec -T postgres-b psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT count(*) FROM likes
     WHERE target_kind = 'post'
       AND target_id = '${post_id}'
       AND peer_id IS NOT NULL
       AND actor_uri = '${actor_uri}'" | tr -d ' \r\n')
if [ "$like_count" -ge 1 ]; then
    pass "studio-b.likes has the federated row (peer_id + actor_uri set)"
else
    fail "Like row not found on studio-b after dispatch (federation_inbox processed but social-side insert missed?)"
fi

# --- assert outbox marked delivered --------------------------------------

step "Verifying studio-a outbox row marked delivered"
out_status=$(docker compose exec -T postgres psql \
    -U artist_alley -d artist_alley -tAc \
    "SELECT status FROM federation_outbox o
     JOIN activities a ON a.id = o.activity_id
     WHERE a.activity_uri = '${activity_uri}'" | tr -d ' \r\n')
if [ "$out_status" = "sent" ] || [ "$out_status" = "delivered" ]; then
    pass "studio-a outbox status = ${out_status}"
else
    warn "outbox status = '${out_status}' (expected sent|delivered)"
fi

# --- machine-readable result ---------------------------------------------

if [ -n "${DOGFOOD_REPORT_PATH:-}" ]; then
    python3 -c "
import json
print(json.dumps({
    'scenario': '06-wire-dispatch',
    'pass': True,
    'dispatch_ms': ${elapsed_ms},
    'activity_uri': '${activity_uri}',
    'post_id': '${post_id}',
    'outbox_status': '${out_status}',
}))" >> "$DOGFOOD_REPORT_PATH"
fi

printf '\n%sScenario 06 PASS%s — synthetic dispatch end-to-end in %sms.\n' \
    "$GREEN" "$RESET" "$elapsed_ms"
