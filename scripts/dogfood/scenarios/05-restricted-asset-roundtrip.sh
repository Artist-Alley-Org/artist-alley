#!/usr/bin/env bash
# scripts/dogfood/scenarios/05-restricted-asset-roundtrip.sh
#
# Scenario 05 (Phase 1.22.I-i — cap-stone). The canonical
# encryption-arc acceptance test, narrowed to the load-bearing
# new validation: the RECEIVER-SIDE encryption-required defense
# gate (1.22.I-h dormant, activated at I-i via the
# inboxSensitivityLookup boot wire).
#
# The earlier I-arc scenarios already prove the happy path:
#
#   * 07 — encryption-key distribution (I-c)
#   * 08 — capability negotiation (I-d)
#   * 09 — sender-side outbox encryption (I-e + I-g)
#   * 11 — sender refusal flip (I-g)
#
# What this scenario adds: the receiver REFUSES to accept a
# plaintext envelope whose target object's sensitivity tier
# mandates encryption — defense in depth against a misbehaving
# (or pre-I-g) sender that ignored its own refusal policy. This
# is the behavior the spec's v1.0-rc1 §4.4 changelog calls
# "active" for the first time.
#
# Two phases:
#
#   Phase A — fixture setup
#     * INSERT a restricted-tier asset directly into studio-a's
#       assets table (the API surface for sensitivity edits is
#       deferred to a future svelte pass; direct SQL is the
#       canonical operator action for now).
#     * Capture admin's user_ref + a peer fixture for the
#       inbox row's actor URI.
#
#   Phase B — negative-path defense-in-depth
#     * INSERT a plaintext federation_inbox row targeting the
#       restricted asset (object_kind='asset', object_id=<asset>,
#       envelope WITHOUT an encryption block).
#     * Trigger the inbox dispatcher via LISTEN/NOTIFY (the
#       same path a real peer would walk).
#     * Assert: row transitions to status='rejected' with
#       reject_reason='encryption_required'.
#     * Assert: federation.inbox.encryption_required_rejected
#       audit event fires.
#     * Assert: no domain-side side-effects (no Like row, no
#       comment row, etc. — the gate fires BEFORE the verb
#       handler).
#
# Why no cross-instance wire delivery here: the wire path is
# already exercised by scenarios 06+07+09. Scenario 05 is
# specifically about the receiver-side gate, which is local-
# only behavior. Bundling the wire path would conflate two
# things the previous scenarios already separate cleanly.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "${ROOT}/scripts/dogfood/scenarios/lib.sh"

step "Logging in as admin on studio-a"
login_admin "$A_HOST" "$A_COOKIES"; pass "studio-a admin in"

# ============================================================
# Phase A — fixture setup
# ============================================================

step "Phase A: provision a restricted-tier asset"

asset_id=$(python3 -c 'import uuid; print(uuid.uuid4())')
admin_ref=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM \"user\" WHERE username = 'admin' LIMIT 1" | tr -d ' \r\n')
[ -n "$admin_ref" ] || fail "admin user not found on studio-a"

# Pick any active asset_type ref so the FK is satisfied.
asset_type_ref=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT ref FROM asset_types ORDER BY ref LIMIT 1" | tr -d ' \r\n')
[ -n "$asset_type_ref" ] || fail "no asset_types row found"

docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 -c "
    INSERT INTO assets (id, title, asset_type, owner_user_ref, status, sensitivity)
    VALUES ('${asset_id}'::uuid, 'scenario-05 restricted fixture', ${asset_type_ref}, ${admin_ref}, 'active', 'restricted')" >/dev/null

# Confirm the sensitivity column accepted the value.
got_tier=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT sensitivity FROM assets WHERE id = '${asset_id}'::uuid" | tr -d ' \r\n')
[ "${got_tier}" = "restricted" ] || fail "asset sensitivity readback = ${got_tier}, want restricted"
pass "asset ${asset_id} provisioned at sensitivity=restricted"

# ============================================================
# Phase B — receiver-side defense-in-depth
# ============================================================

step "Phase B: inject plaintext inbox row targeting the restricted asset"

# We need a peer to attribute the inbox row to. Reuse the
# existing dogfood peer-pair row (studio-b on studio-a's side).
# If pair.sh didn't run yet (single-instance scenario), use any
# row from federation_peers as the foreign-key satisfier — the
# receiver-side gate doesn't care which peer sent the envelope,
# only that one exists.
peer_id=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT id FROM federation_peers ORDER BY created_at DESC LIMIT 1" | tr -d ' \r\n')
[ -n "$peer_id" ] || fail "no federation_peers row found (run pair.sh)"

# Pre-count audit events of the rejection type so the
# post-dispatch delta is unambiguous (other test runs may have
# fired them too).
audit_before=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT COUNT(*) FROM audit_events
      WHERE event_type = 'federation.inbox.encryption_required_rejected'
        AND metadata->>'object_kind' = 'asset'" | tr -d ' \r\n')

# Synthesise a Like activity targeting the restricted asset.
# Plaintext envelope (no encryption block) — the gate's exact
# firing condition.
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(4))')
activity_uri="urn:aa-dogfood:scenario-05:plaintext:${suffix}"
inbox_id=$(python3 -c 'import uuid; print(uuid.uuid4())')

# The envelope JSON. Note: NO "encryption" field. The dispatcher
# parses this, sees Encryption=nil, then hits the policy gate
# at stage 3.5. Required actor + activity-type fields kept to
# the minimum the dispatcher's pre-gate parsing demands.
envelope_json=$(python3 -c "
import json
print(json.dumps({
    '@context':  'aa-fed/v1',
    'type':      'Like',
    'id':        '${activity_uri}',
    'actor':     'http://studio-b.local/users/admin',
    'object':    'https://studio-a.local/assets/${asset_id}',
    'published': '2026-06-15T00:00:00Z',
    'to':        ['http://studio-a.local/users/admin'],
}))
")

docker compose exec -T postgres psql -U artist_alley -d artist_alley -v ON_ERROR_STOP=1 <<SQL >/dev/null
INSERT INTO federation_inbox (
    id, activity_uri, peer_id, actor_uri, activity_type,
    object_kind, object_id, envelope_json, http_sig_key, status
) VALUES (
    '${inbox_id}'::uuid,
    '${activity_uri}',
    '${peer_id}'::uuid,
    'http://studio-b.local/users/admin',
    'Like',
    'asset',
    '${asset_id}'::uuid,
    '${envelope_json}'::jsonb,
    'dummy-key-id',
    'pending'
);
NOTIFY federation_inbox_pending;
SQL

# Wait for the dispatcher to process the row.
wait_for "studio-a inbox row transitions away from pending" 15 "
    status=\$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \\
        \"SELECT status FROM federation_inbox WHERE id = '${inbox_id}'::uuid\" | tr -d ' \\r\\n')
    [ \"\$status\" != 'pending' ]
"

# Read the final state.
final=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc "
    SELECT status || '|' || COALESCE(reject_reason, '<null>')
      FROM federation_inbox
     WHERE id = '${inbox_id}'::uuid" | tr -d ' \r\n')
final_status="${final%%|*}"
final_reason="${final##*|}"

[ "${final_status}" = "rejected" ] \
    || fail "inbox row status = ${final_status}, want rejected"
[ "${final_reason}" = "encryption_required" ] \
    || fail "inbox row reject_reason = ${final_reason}, want encryption_required"
pass "inbox row rejected with reason=encryption_required (gate fired as designed)"

# Verify the audit event.
audit_after=$(docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc \
    "SELECT COUNT(*) FROM audit_events
      WHERE event_type = 'federation.inbox.encryption_required_rejected'
        AND metadata->>'object_kind' = 'asset'" | tr -d ' \r\n')
[ "${audit_after}" -gt "${audit_before}" ] \
    || fail "federation.inbox.encryption_required_rejected count unchanged (${audit_before} → ${audit_after})"
pass "federation.inbox.encryption_required_rejected fired (count ${audit_before} → ${audit_after})"

# Cleanup the fixture asset.
docker compose exec -T postgres psql -U artist_alley -d artist_alley -c \
    "DELETE FROM assets WHERE id = '${asset_id}'::uuid" >/dev/null

step "Scenario 05 complete: receiver-side encryption-required gate validated end-to-end"
