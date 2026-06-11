-- name: ListPeers :many
-- Admin audit/list surface. All peers, newest-handshake-first.
-- Bounded LIMIT — admins with thousands of peers would page; v1
-- expects dozens at most.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled, status,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at
FROM federation_peers
ORDER BY handshake_at DESC, id DESC
LIMIT $1;

-- name: ListPendingInboundPeers :many
-- Admin "requests awaiting your approval" feed (1.22.B-b).
-- Backed by the federation_peers_pending_inbound_idx partial
-- index so this stays cheap regardless of total table size.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled, status,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at
FROM federation_peers
WHERE status = 'pending_inbound'
ORDER BY handshake_at DESC, id DESC;

-- name: GetPeerByID :one
-- Admin detail view + the auth path that resolves "which peer
-- is this inbound activity from?" once we have a UUID.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled, status,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at
FROM federation_peers
WHERE id = $1;

-- name: GetPeerByInstanceURL :one
-- Hot read on the inbound federation path: every signed request
-- from a peer arrives addressed-by-URL; we look up the row to
-- (a) authenticate the request via instance_public_key, (b) check
-- the enabled flag + status, (c) update last_seen_at. Cache-
-- fronted at the package layer.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled, status,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at
FROM federation_peers
WHERE instance_url = $1;

-- name: ListEnabledPeers :many
-- Hot read on the outbound delivery path: the federation outbox
-- worker (Phase 1.22.D) iterates this set to know who can
-- receive activities. Partial index federation_peers_enabled_idx
-- (revised in migration 00052 to gate on status='connected') means
-- this is sub-ms even with hundreds of peers — and pending
-- peers are excluded so we never deliver to a half-paired peer.
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled, status,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at
FROM federation_peers
WHERE enabled = TRUE AND status = 'connected'
ORDER BY instance_url;

-- name: InsertPeer :one
-- Manual admin entry (1.22.B-a) + handshake landing (1.22.B-b)
-- both insert here. UNIQUE on instance_url forces the operator
-- to think before re-pairing — the upsert variant
-- (UpdatePeerAfterHandshake) handles re-keying explicitly.
--
-- Status param defaults to 'connected' (manual entry) but the
-- handshake flow passes 'pending_outbound' or 'pending_inbound'
-- per v1.md §11 state machine.
INSERT INTO federation_peers (
    instance_url,
    display_name,
    instance_public_key,
    trust_tier,
    encryption_policy,
    enabled,
    status,
    handshake_by_user_ref,
    notes
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, instance_url, display_name, instance_public_key,
          trust_tier, encryption_policy, enabled, status,
          handshake_at, handshake_by_user_ref, last_seen_at,
          notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at;

-- name: UpdatePeer :one
-- Admin edit: change display_name / trust_tier / encryption_policy
-- / enabled / notes. Does NOT change instance_url or
-- instance_public_key — those require defederate+repair (the
-- public_key change in particular signals re-handshake).
UPDATE federation_peers
SET display_name          = COALESCE(sqlc.narg('display_name')::TEXT,       display_name),
    trust_tier            = COALESCE(sqlc.narg('trust_tier')::TEXT,         trust_tier),
    encryption_policy     = COALESCE(sqlc.narg('encryption_policy')::TEXT,  encryption_policy),
    enabled               = COALESCE(sqlc.narg('enabled')::BOOLEAN,          enabled),
    notes                 = COALESCE(sqlc.narg('notes')::TEXT,               notes),
    share_in_visible_list = COALESCE(sqlc.narg('share_in_visible_list')::BOOLEAN, share_in_visible_list),
    updated_at            = NOW()
WHERE id = $1
RETURNING id, instance_url, display_name, instance_public_key,
          trust_tier, encryption_policy, enabled, status,
          handshake_at, handshake_by_user_ref, last_seen_at,
          notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at;

-- name: CompleteOutboundHandshake :one
-- Atomically replaces the pending_outbound placeholder pubkey
-- with the peer's real key (delivered in the confirm envelope)
-- + flips status to connected. The atomic write is important —
-- a separate UPDATE-pubkey-then-UPDATE-status sequence could
-- leave us with the new key but still pending_outbound on a
-- mid-flight crash.
UPDATE federation_peers
SET instance_public_key = $2,
    status              = 'connected',
    updated_at          = NOW()
WHERE id = $1 AND status = 'pending_outbound'
RETURNING id, instance_url, display_name, instance_public_key,
          trust_tier, encryption_policy, enabled, status,
          handshake_at, handshake_by_user_ref, last_seen_at,
          notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at;

-- name: SetPeerStatus :one
-- Internal handshake state transition: pending_outbound →
-- connected, pending_inbound → connected, etc. Bypasses
-- UpdatePeer because the public PATCH endpoint doesn't expose
-- status changes — those are protocol-driven, not admin-driven.
UPDATE federation_peers
SET status     = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING id, instance_url, display_name, instance_public_key,
          trust_tier, encryption_policy, enabled, status,
          handshake_at, handshake_by_user_ref, last_seen_at,
          notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at;

-- name: AppendPeerNote :one
-- Internal: append a timestamped line to the notes column so
-- the admin UI surfaces transient handshake failures (offer POST
-- timed out, peer 5xx, etc.). $2 is the pre-stamped line.
UPDATE federation_peers
SET notes      = CASE WHEN notes = '' THEN $2 ELSE notes || E'\n' || $2 END,
    updated_at = NOW()
WHERE id = $1
RETURNING id, instance_url, display_name, instance_public_key,
          trust_tier, encryption_policy, enabled, status,
          handshake_at, handshake_by_user_ref, last_seen_at,
          notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at;

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

-- name: ListVisiblePeers :many
-- The set returned by GET /federation/peers/visible — peers we've
-- opted to expose to anyone who asks. Backed by the partial index
-- federation_peers_visible_idx (enabled AND status=connected AND
-- share_in_visible_list).
SELECT id, instance_url, display_name, instance_public_key,
       trust_tier, encryption_policy, enabled, status,
       handshake_at, handshake_by_user_ref, last_seen_at,
       notes, share_in_visible_list, created_at, updated_at, capabilities, capabilities_negotiated_at
FROM federation_peers
WHERE enabled = TRUE AND status = 'connected' AND share_in_visible_list = TRUE
ORDER BY instance_url;

-- --- Phase 1.22.I-d — capability negotiation ------------------------

-- name: SetPeerCapabilities :exec
-- Writes the bilateral intersection produced by the handshake
-- engine. The intersection — NOT either side's raw advertised set
-- — is what we record, so the I-e/I-g dispatch gates can rely on
-- "this peer supports X" without re-doing the intersection at
-- every check site. capabilities_negotiated_at moves to NOW() so
-- ListPeersMissingCapabilities stops surfacing this peer.
UPDATE federation_peers
   SET capabilities = $2,
       capabilities_negotiated_at = NOW()
 WHERE id = $1;

-- name: ListPeersMissingCapabilities :many
-- Operator observability: peers paired before I-d that haven't
-- been re-negotiated. Surfaced on the admin federation page so
-- the operator can trigger re-pairing. Backed by the
-- federation_peers_unnegotiated_idx partial index from migration
-- 00009 so this query stays cheap regardless of total peer count.
SELECT id, instance_url, display_name
  FROM federation_peers
 WHERE capabilities_negotiated_at IS NULL
 ORDER BY instance_url;
