-- name: ListPeers :many
-- Admin audit/list surface. All peers, newest-handshake-first.
-- Bounded LIMIT — admins with thousands of peers would page; v1
-- expects dozens at most.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, created_at, updated_at
FROM federation_peers
ORDER BY handshake_at DESC, id DESC
LIMIT $1;

-- name: GetPeerByID :one
-- Admin detail view + the auth path that resolves "which peer
-- is this inbound activity from?" once we have a UUID.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, created_at, updated_at
FROM federation_peers
WHERE id = $1;

-- name: GetPeerByInstanceURL :one
-- Hot read on the inbound federation path: every signed request
-- from a peer arrives addressed-by-URL; we look up the row to
-- (a) authenticate the request via instance_public_key, (b) check
-- the enabled flag, (c) update last_seen_at. Cache-fronted at
-- the package layer.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, created_at, updated_at
FROM federation_peers
WHERE instance_url = $1;

-- name: ListEnabledPeers :many
-- Hot read on the outbound delivery path: the federation outbox
-- worker (Phase 1.22.D) iterates this set to know who can
-- receive activities. Partial index federation_peers_enabled_idx
-- means this is sub-ms even with hundreds of peers.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, created_at, updated_at
FROM federation_peers
WHERE enabled = TRUE
ORDER BY instance_url;

-- name: InsertPeer :one
-- Manual admin entry (1.22.B-a) + handshake landing (1.22.B-b)
-- both insert here. UNIQUE on instance_url forces the operator
-- to think before re-pairing — the upsert variant
-- (UpdatePeerAfterHandshake) handles re-keying explicitly.
INSERT INTO federation_peers (
    instance_url,
    display_name,
    instance_public_key,
    trust_tier,
    encryption_policy,
    enabled,
    handshake_by_user_ref,
    notes
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, instance_url, display_name, instance_public_key,
          trust_tier, encryption_policy, enabled,
          handshake_at, handshake_by_user_ref, last_seen_at,
          notes, created_at, updated_at;

-- name: UpdatePeer :one
-- Admin edit: change display_name / trust_tier / encryption_policy
-- / enabled / notes. Does NOT change instance_url or
-- instance_public_key — those require defederate+repair (the
-- public_key change in particular signals re-handshake).
UPDATE federation_peers
SET display_name     = COALESCE(sqlc.narg('display_name')::TEXT,     display_name),
    trust_tier       = COALESCE(sqlc.narg('trust_tier')::TEXT,       trust_tier),
    encryption_policy = COALESCE(sqlc.narg('encryption_policy')::TEXT, encryption_policy),
    enabled          = COALESCE(sqlc.narg('enabled')::BOOLEAN,        enabled),
    notes            = COALESCE(sqlc.narg('notes')::TEXT,             notes),
    updated_at       = NOW()
WHERE id = $1
RETURNING id, instance_url, display_name, instance_public_key,
          trust_tier, encryption_policy, enabled,
          handshake_at, handshake_by_user_ref, last_seen_at,
          notes, created_at, updated_at;

-- name: DeletePeer :exec
-- Defederation. Per ADR 0043 §"Trust model" the receiving side
-- of this should cascade-delete federation_shares rows targeting
-- the peer (FK ON DELETE CASCADE on that table, Phase 1.22.C);
-- the audit emission + final aa:Unshare delivery to the peer is
-- the dispatcher's job (Phase 1.22.D).
DELETE FROM federation_peers WHERE id = $1;

-- name: TouchPeerLastSeen :exec
-- Phase 1.22.D inbox path: best-effort bump of last_seen_at when
-- a valid signed request arrives. No row returned; failure
-- (e.g. peer disabled between auth + this write) is logged-and-
-- ignored.
UPDATE federation_peers
SET last_seen_at = NOW()
WHERE instance_url = $1;
