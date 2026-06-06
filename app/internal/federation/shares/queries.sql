-- name: InsertShare :one
-- Grant a new share. Called inside WithEmission so the activity
-- row + audit row commit atomically with this insert.
-- granted_activity_id is the just-recorded aa:Share activity's
-- UUID; the FK is RESTRICT so we can never lose the audit chain.
INSERT INTO federation_shares (
    grantor_user_ref,
    object_kind,
    object_id,
    peer_id,
    target_user_url,
    scope,
    expires_at,
    notes,
    granted_activity_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, grantor_user_ref, object_kind, object_id,
          peer_id, target_user_url, scope, expires_at, notes,
          granted_activity_id, granted_at, revoked_at,
          revoked_activity_id, created_at, updated_at;

-- name: GetShareByID :one
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE id = $1;

-- name: RevokeShare :one
-- Mark a share revoked + capture the aa:Unshare activity UUID.
-- Idempotent: re-revoking a revoked row is a no-op (returns the
-- existing revoked row unchanged via the WHERE filter).
UPDATE federation_shares
SET revoked_at          = NOW(),
    revoked_activity_id = $2,
    updated_at          = NOW()
WHERE id = $1 AND revoked_at IS NULL
RETURNING id, grantor_user_ref, object_kind, object_id,
          peer_id, target_user_url, scope, expires_at, notes,
          granted_activity_id, granted_at, revoked_at,
          revoked_activity_id, created_at, updated_at;

-- name: ListSharesByObject :many
-- "Who has access to this object?" — admin per-object view.
-- Only ACTIVE shares; revoked rows are visible via a separate
-- ListSharesByObjectAll query when the admin wants full history.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE object_kind = $1 AND object_id = $2 AND revoked_at IS NULL
ORDER BY granted_at DESC, id DESC;

-- name: ListSharesByPeer :many
-- "What am I sharing with this peer?" — admin per-peer outbound
-- view + the defederation cascade preview source.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE peer_id = $1 AND revoked_at IS NULL
ORDER BY object_kind, granted_at DESC, id DESC
LIMIT $2;

-- name: CountSharesByPeer :one
-- Used by the defederation cascade-preview modal: how many
-- shares is the operator about to revoke?
SELECT COUNT(*)::BIGINT AS total
FROM federation_shares
WHERE peer_id = $1 AND revoked_at IS NULL;

-- name: ListSharesByGrantor :many
-- "What am I sharing?" — the user's own outbound shares list.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE grantor_user_ref = $1 AND revoked_at IS NULL
ORDER BY granted_at DESC, id DESC
LIMIT $2;

-- name: FindActiveShare :one
-- Inbox-filter direct check: is there an ACTIVE share for this
-- (object, peer, optional target_user)? Used per inbound activity.
--
-- target_user_url match: NULL on the share means "any user on
-- the peer" so it always matches; a non-null share value must
-- match the requesting user's actor URL exactly.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE object_kind = $1
  AND object_id   = $2
  AND peer_id     = $3
  AND (target_user_url IS NULL OR target_user_url = $4)
  AND revoked_at  IS NULL
  AND (expires_at IS NULL OR expires_at > NOW())
ORDER BY
    -- Prefer the SPECIFIC user grant over the broadcast grant
    -- (if both exist somehow); higher scope wins next.
    CASE WHEN target_user_url IS NOT NULL THEN 0 ELSE 1 END,
    CASE scope
        WHEN 'remix'    THEN 0
        WHEN 'annotate' THEN 1
        WHEN 'comment'  THEN 2
        WHEN 'view'     THEN 3
        ELSE 4
    END
LIMIT 1;

-- name: ListActiveSharesByObject :many
-- Returns ALL active shares for one object — the per-object
-- snapshot the cache stores + the decision function iterates in
-- memory to find a peer+user+scope match. Bounded by the
-- federation_shares_lookup_idx partial index.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE object_kind = $1 AND object_id = $2 AND revoked_at IS NULL
ORDER BY id;

-- name: FindContainingCollections :many
-- Container fallback for assets: which collections contain this
-- asset? Used by the inbox-filter when a direct asset share lookup
-- misses — we check if any container collection has a share that
-- covers the requesting peer/user.
SELECT collection_id::UUID AS collection_id
FROM collection_resources
WHERE asset_id = $1;

-- name: GetShareCountsByPeer :many
-- Defederation cascade-preview source. Groups active shares by
-- object_kind so the admin UI can render "12 posts, 23
-- collections, 8 assets, 4 brand kits" in the cascade modal.
-- Bounded by federation_shares_by_peer_idx (partial active).
SELECT object_kind, COUNT(*)::BIGINT AS share_count
FROM federation_shares
WHERE peer_id = $1 AND revoked_at IS NULL
GROUP BY object_kind
ORDER BY object_kind;

-- name: ListExpiringShares :many
-- Expiry sweeper (Phase 1.22.C-d job) input. Returns active
-- shares whose expires_at has passed in the last `lookback`
-- window — bounded so the sweeper can chunk its work without
-- pulling a million-row dataset on first run.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE revoked_at IS NULL
  AND expires_at IS NOT NULL
  AND expires_at <= NOW()
ORDER BY expires_at
LIMIT $1;

-- name: ListActiveSharesByPeerChunk :many
-- Defederation cascade chunk fetch (Phase 1.22.C-d job). Pulls
-- N active shares per call so the worker can process them
-- in bounded transactions. Job idempotent — re-runs filter on
-- revoked_at IS NULL so already-processed rows drop out.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at
FROM federation_shares
WHERE peer_id = $1 AND revoked_at IS NULL
ORDER BY id
LIMIT $2;
