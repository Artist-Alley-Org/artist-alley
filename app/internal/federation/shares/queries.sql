-- name: InsertShare :one
-- Grant a new share. Called inside WithEmission so the activity
-- row + audit row commit atomically with this insert.
-- granted_activity_id is the just-recorded aa:Share activity's
-- UUID; the FK is RESTRICT so we can never lose the audit chain.
--
-- id is optional (#1586): the admin grant path mints the share id BEFORE
-- the transaction, because the aa:Share envelope and the
-- federation.share.granted audit both carry it, and it must be the id
-- the row is stored under. NULL keeps the column default.
INSERT INTO federation_shares (
    id,
    grantor_user_ref,
    object_kind,
    object_id,
    peer_id,
    target_user_url,
    scope,
    expires_at,
    notes,
    granted_activity_id,
    allow_original,
    allow_access_copies
)
VALUES (
    COALESCE(sqlc.narg(id)::UUID, gen_random_uuid()),
    sqlc.arg(grantor_user_ref),
    sqlc.arg(object_kind),
    sqlc.arg(object_id),
    sqlc.arg(peer_id),
    sqlc.narg(target_user_url),
    sqlc.arg(scope),
    sqlc.narg(expires_at),
    sqlc.arg(notes),
    sqlc.arg(granted_activity_id),
    sqlc.arg(allow_original),
    sqlc.arg(allow_access_copies)
)
RETURNING id, grantor_user_ref, object_kind, object_id,
          peer_id, target_user_url, scope, expires_at, notes,
          granted_activity_id, granted_at, revoked_at,
          revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies;

-- name: GetUnrevokedShareForTuple :one
-- #1586: the row that holds one grant tuple's slot in
-- federation_shares_active_uniq_idx — (grantor, object_kind, object_id,
-- peer, recipient-or-empty) WHERE revoked_at IS NULL — live
-- OR expired-but-not-yet-revoked. The index admits at most one, so
-- this is exactly the row a new grant for the tuple collides with.
-- `expired` is evaluated against the database clock, the same clock
-- ListExpiringShares and every expiry predicate use. Pass the empty
-- string as target_key to mean "no specific recipient" (NULL), as
-- the index does.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies,
       (expires_at IS NOT NULL AND expires_at <= NOW())::BOOLEAN AS expired
FROM federation_shares
WHERE grantor_user_ref = sqlc.arg(grantor_user_ref)
  AND object_kind      = sqlc.arg(object_kind)
  AND object_id        = sqlc.arg(object_id)
  AND peer_id          = sqlc.arg(peer_id)
  AND COALESCE(target_user_url, '') = sqlc.arg(target_key)::TEXT
  AND revoked_at IS NULL;

-- name: GetShareByID :one
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
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
          revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies;

-- name: RevokeExpiredShare :one
-- #1586: RevokeShare, but only while the row is STILL expired. The
-- expiry retirement (the sweeper and the regrant path) reads a row as
-- expired and closes it in a later statement; nothing locks the row in
-- between. If its expiry was extended in that window the row is live
-- again, and this UPDATE matches nothing (pgx.ErrNoRows), so the caller
-- rolls back its aa:Unshare and leaves the live grant untouched. Same
-- shape and RETURNING as RevokeShare.
UPDATE federation_shares
SET revoked_at          = NOW(),
    revoked_activity_id = $2,
    updated_at          = NOW()
WHERE id = $1
  AND revoked_at IS NULL
  AND expires_at IS NOT NULL
  AND expires_at <= NOW()
RETURNING id, grantor_user_ref, object_kind, object_id,
          peer_id, target_user_url, scope, expires_at, notes,
          granted_activity_id, granted_at, revoked_at,
          revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies;

-- name: ListSharesByObject :many
-- "Who has access to this object?" — admin per-object view.
-- Only ACTIVE shares; revoked rows are visible via a separate
-- ListSharesByObjectAll query when the admin wants full history.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
FROM federation_shares
WHERE object_kind = $1 AND object_id = $2 AND revoked_at IS NULL
ORDER BY granted_at DESC, id DESC;

-- name: ListSharesByPeer :many
-- "What am I sharing with this peer?" — admin per-peer outbound
-- view + the defederation cascade preview source.
SELECT id, grantor_user_ref, object_kind, object_id,
       peer_id, target_user_url, scope, expires_at, notes,
       granted_activity_id, granted_at, revoked_at,
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
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
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
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
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
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
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
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
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
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
       revoked_activity_id, created_at, updated_at, allow_original, allow_access_copies
FROM federation_shares
WHERE peer_id = $1 AND revoked_at IS NULL
ORDER BY id
LIMIT $2;

-- name: GetPeerForOriginalFetch :one
-- #1573: the authoritative peer state for an original-byte fetch
-- decision, read from the row, never from caller-supplied flags.
-- capabilities is the stored negotiated intersection.
SELECT id, instance_url, enabled, status, capabilities
FROM federation_peers
WHERE id = $1;

-- name: GetAssetForOriginalFetch :one
-- #1573: the asset facts an original-byte fetch decision binds to:
-- owner (the grantor-owner rule), the stored original's hash and its
-- size as stored (storage_objects, not the asset's copy of it), the
-- sensitivity tier, whether it is deleted, and whether this instance is
-- its origin (a copy received from a peer carries origin_server_id).
SELECT a.id, a.owner_user_ref, a.file_hash, so.size_bytes AS stored_size,
       a.sensitivity,
       (a.deleted_at IS NOT NULL)::BOOLEAN AS deleted,
       (a.origin_server_id IS NOT NULL)::BOOLEAN AS from_peer
FROM assets a
LEFT JOIN storage_objects so ON so.hash = a.file_hash
WHERE a.id = $1;

-- name: GetAssetForAccessCopyFetch :one
-- #1597: the asset facts an access-copy decision binds to: owner (the
-- grantor-owner rule), tier, deletion, and whether this instance is its
-- origin. Deliberately not the asset's current file_hash: an access copy
-- is authorised by its sealed version's own file, never by the asset's
-- current original.
SELECT a.id, a.owner_user_ref, a.sensitivity,
       (a.deleted_at IS NOT NULL)::BOOLEAN AS deleted,
       (a.origin_server_id IS NOT NULL)::BOOLEAN AS from_peer
FROM assets a
WHERE a.id = $1;

-- name: GetAccessCopyFacts :one
-- #1597: whether version_number of asset_id is sealed and lists an access
-- copy with exactly object_hash and size_bytes; whether that copy is an
-- immutable object of its own whose recorded fixity is verified NOW, at
-- that SHA-256 and size (migration 00085 binds a sealed copy to such an
-- object and to its original's recorded variant digest); and whether this
-- origin recorded, before sending, an announcement of that version to
-- peer_id that lists that file as wire role access (migration 00091). Both
-- local derivative roles are wire role access (assetsend.versionFiles).
-- Each answer is read from rows, never from a mutable rendition key.
SELECT
    EXISTS (SELECT 1 FROM asset_versions v
             WHERE v.asset_id = sqlc.arg(asset_id) AND v.number = sqlc.arg(version_number)
               AND v.sealed_at IS NOT NULL)::BOOLEAN AS version_sealed,
    EXISTS (SELECT 1 FROM asset_versions v
              JOIN asset_version_files f ON f.version_id = v.id
             WHERE v.asset_id = sqlc.arg(asset_id) AND v.number = sqlc.arg(version_number)
               AND v.sealed_at IS NOT NULL
               AND f.role IN ('preview', 'access')
               AND f.object_hash = sqlc.arg(object_hash)::TEXT AND f.size_bytes = sqlc.arg(size_bytes)::BIGINT)::BOOLEAN AS copy_listed,
    EXISTS (SELECT 1 FROM asset_versions v
              JOIN asset_version_files f ON f.version_id = v.id
              JOIN storage_objects so ON so.hash = f.object_hash AND so.size_bytes = f.size_bytes
              JOIN storage_variants sv ON sv.object_hash = so.hash AND sv.variant_key = 'original'
             WHERE v.asset_id = sqlc.arg(asset_id) AND v.number = sqlc.arg(version_number)
               AND v.sealed_at IS NOT NULL
               AND f.role IN ('preview', 'access')
               AND f.object_hash = sqlc.arg(object_hash)::TEXT AND f.size_bytes = sqlc.arg(size_bytes)::BIGINT
               AND sv.fixity_status = 'verified'
               AND sv.content_sha256 = sqlc.arg(object_hash)::TEXT
               AND sv.content_size_bytes = sqlc.arg(size_bytes)::BIGINT)::BOOLEAN AS copy_verified,
    EXISTS (SELECT 1 FROM federation_asset_announcement_claims c
             WHERE c.peer_id = sqlc.arg(peer_id) AND c.asset_id = sqlc.arg(asset_id)
               AND c.version_number = sqlc.arg(version_number)
               AND c.files @> jsonb_build_array(jsonb_build_object('role', 'access', 'sha256', sqlc.arg(object_hash)::TEXT)))::BOOLEAN AS announced;

-- name: GetAssetGrantContext :one
-- #1573: what the grant form shows next to the original-download
-- toggle: the asset's title and tier. Deleted assets are not offered.
SELECT id, title, sensitivity
FROM assets
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListAssetRightsFields :many
-- #1573: the asset's local copyright and credit fields (free text, the
-- shipped rights fields), shown in the grant form as context only. They
-- are not an ArchivePub rights URI or requiredStatement and never grant
-- or block anything. read_capability is returned so the handler can
-- omit a field the caller may not read.
SELECT fd.code, afv.value_text, COALESCE(fd.read_capability, '')::TEXT AS read_capability
FROM asset_field_value afv
JOIN field_definition fd ON fd.id = afv.field_id
WHERE afv.asset_id = $1
  AND fd.code IN ('copyright', 'credit')
  AND afv.value_text IS NOT NULL
  AND afv.value_text <> ''
ORDER BY fd.code;
