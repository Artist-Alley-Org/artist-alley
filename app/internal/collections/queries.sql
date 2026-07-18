-- ---------------------------------------------------------------------------
-- collections (the entity)
-- ---------------------------------------------------------------------------

-- Column ORDER in the explicit lists below is load-bearing, not style.
-- It matches the physical column order a migrated database actually has
-- (search_text/smart_query were added before the soft-delete columns).
-- When a query's column list matches table order, sqlc reuses the
-- Collection model; when it doesn't, sqlc emits a bespoke per-query Row
-- struct and every caller that assigns to Collection stops compiling.
-- See #420 -- app/schema.sql previously described an order migrations
-- never produce, which is what hid this.

-- name: CreateCollection :one
INSERT INTO collections (
    owner_user_ref, name, description, visibility, membership,
    expires_at, featured, purpose, origin_server_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, owner_user_ref, name, description, visibility, membership,
          expires_at, featured, purpose, origin_server_id,
          created_at, updated_at, search_text, smart_query,
          deleted_at, deleted_reason;

-- name: GetCollection :one
-- Filters soft-deleted rows by default. Admin surfaces reading
-- deleted rows use GetCollectionIncludingDeleted below.
SELECT id, owner_user_ref, name, description, visibility, membership,
       expires_at, featured, purpose, origin_server_id,
       created_at, updated_at, search_text, smart_query,
       deleted_at, deleted_reason
FROM collections
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetCollectionIncludingDeleted :one
-- Same as GetCollection but returns soft-deleted rows too. Used by
-- the Restore path (which needs to Get the row to fire the audit
-- event even after deleted_at IS NOT NULL).
SELECT id, owner_user_ref, name, description, visibility, membership,
       expires_at, featured, purpose, origin_server_id,
       created_at, updated_at, search_text, smart_query,
       deleted_at, deleted_reason
FROM collections
WHERE id = $1;

-- name: UpdateCollection :one
-- Partial update via COALESCE — NULL args keep current values.
UPDATE collections SET
    name        = COALESCE(sqlc.narg('name'),        name),
    description = COALESCE(sqlc.narg('description'), description),
    visibility  = COALESCE(sqlc.narg('visibility'),  visibility),
    membership  = COALESCE(sqlc.narg('membership'),  membership),
    featured    = COALESCE(sqlc.narg('featured'),    featured),
    purpose     = COALESCE(sqlc.narg('purpose'),     purpose),
    expires_at  = COALESCE(sqlc.narg('expires_at'),  expires_at),
    updated_at  = NOW()
WHERE id = sqlc.arg('id')
RETURNING id, owner_user_ref, name, description, visibility, membership,
          expires_at, featured, purpose, origin_server_id,
          created_at, updated_at, search_text, smart_query,
          deleted_at, deleted_reason;

-- name: ClearCollectionExpiresAt :exec
-- Separate query because COALESCE can't express "explicitly set to NULL".
-- Callers use this when the admin removes a TTL.
UPDATE collections SET expires_at = NULL, updated_at = NOW() WHERE id = $1;

-- name: DeleteCollection :exec
-- Phase 1.55.C-1b: soft-delete. Sets deleted_at + deleted_reason on
-- the row rather than hard-DELETE; the nightly softdelete.gc
-- coordinator hard-deletes past sysconfig.CollectionRetentionDays,
-- at which point collection_resources / collection_posts /
-- collection_acls cascade via their existing FK ON DELETE CASCADE.
UPDATE collections
SET deleted_at = NOW(), deleted_reason = $2, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListCollectionsPage :many
-- Cursor pagination on (created_at DESC, id DESC). Filters are
-- nullable narg() so a single query covers every combo.
--
-- `q_name` is an optional case-insensitive substring match on the
-- collection's display name; the handler ILIKE-escapes the input.
--
-- `shared_with_user` powers the "Shared" hub tab: collections the
-- caller has an ACL grant on but doesn't own. The handler also passes
-- the caller's user_ref into `exclude_owner` to drop owned rows.
SELECT id, owner_user_ref, name, description, visibility, membership,
       expires_at, featured, purpose, origin_server_id,
       created_at, updated_at, search_text, smart_query,
       deleted_at, deleted_reason
FROM collections c
WHERE (sqlc.narg('include_deleted')::BOOLEAN IS TRUE OR deleted_at IS NULL)
  AND (sqlc.narg('owner_user_ref')::BIGINT  IS NULL OR owner_user_ref = sqlc.narg('owner_user_ref')::BIGINT)
  AND (sqlc.narg('exclude_owner')::BIGINT   IS NULL OR owner_user_ref <> sqlc.narg('exclude_owner')::BIGINT)
  AND (sqlc.narg('visibility')::TEXT        IS NULL OR visibility     = sqlc.narg('visibility')::TEXT)
  AND (sqlc.narg('featured')::BOOLEAN       IS NULL OR featured       = sqlc.narg('featured')::BOOLEAN)
  AND (sqlc.narg('q_name')::TEXT            IS NULL OR name ILIKE '%' || sqlc.narg('q_name')::TEXT || '%')
  AND (sqlc.narg('shared_with_user')::BIGINT IS NULL OR EXISTS (
         SELECT 1 FROM collection_acls a
          WHERE a.collection_id = c.id
            AND a.principal_type = 'user'
            AND a.principal_id   = sqlc.narg('shared_with_user')::BIGINT::TEXT
            AND (a.expires_at IS NULL OR a.expires_at > NOW())
       ))
  AND (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
       OR created_at < sqlc.narg('cursor_created_at')::TIMESTAMPTZ
       OR (created_at = sqlc.narg('cursor_created_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: CountCollectionResources :one
SELECT COUNT(*)::BIGINT AS value
FROM collection_resources
WHERE collection_id = $1 AND pinned = TRUE
  AND (expires_at IS NULL OR expires_at > NOW());

-- ---------------------------------------------------------------------------
-- collection_resources (manual membership)
-- ---------------------------------------------------------------------------

-- name: AddCollectionResource :exec
-- Idempotent on the PK (collection_id, asset_id). A re-add updates
-- sort_order + expires_at + pinned but keeps added_at fixed.
INSERT INTO collection_resources (
    collection_id, asset_id, sort_order, pinned, expires_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (collection_id, asset_id) DO UPDATE SET
    sort_order = EXCLUDED.sort_order,
    pinned     = EXCLUDED.pinned,
    expires_at = EXCLUDED.expires_at;

-- name: RemoveCollectionResource :exec
DELETE FROM collection_resources WHERE collection_id = $1 AND asset_id = $2;

-- name: ListCollectionResourcesPage :many
-- Returns pinned members, sorted by sort_order then added_at. Excludes
-- expired-membership rows. Joined onto assets so the list can carry
-- the title/thumb/type the front-end needs without an N+1.
SELECT cr.collection_id, cr.asset_id, cr.sort_order, cr.pinned,
       cr.expires_at, cr.added_at,
       a.title, a.asset_type, a.status, a.file_hash, a.created_at AS asset_created_at
FROM collection_resources cr
JOIN assets a ON a.id = cr.asset_id
WHERE cr.collection_id = $1
  AND cr.pinned = TRUE
  AND (cr.expires_at IS NULL OR cr.expires_at > NOW())
  AND a.deleted_at IS NULL
  AND (sqlc.narg('cursor_sort_order')::INTEGER IS NULL
       OR cr.sort_order > sqlc.narg('cursor_sort_order')::INTEGER
       OR (cr.sort_order = sqlc.narg('cursor_sort_order')::INTEGER
           AND cr.added_at > sqlc.narg('cursor_added_at')::TIMESTAMPTZ))
ORDER BY cr.sort_order ASC, cr.added_at ASC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- ---------------------------------------------------------------------------
-- ACLs (Phase 1.7.B-7c)
-- ---------------------------------------------------------------------------

-- name: ListCollectionAcls :many
SELECT collection_id, principal_type, principal_id, permission,
       granted_at, granted_by_user_ref, expires_at
FROM collection_acls
WHERE collection_id = $1
ORDER BY granted_at DESC, principal_type, principal_id, permission;

-- name: AddCollectionAcl :exec
INSERT INTO collection_acls (collection_id, principal_type, principal_id, permission,
                             granted_by_user_ref, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (collection_id, principal_type, principal_id, permission) DO UPDATE SET
    granted_at            = NOW(),
    granted_by_user_ref = EXCLUDED.granted_by_user_ref,
    expires_at            = EXCLUDED.expires_at;

-- name: RemoveCollectionAcl :execrows
DELETE FROM collection_acls
WHERE collection_id = $1
  AND principal_type = $2
  AND principal_id   = $3
  AND permission     = $4;
