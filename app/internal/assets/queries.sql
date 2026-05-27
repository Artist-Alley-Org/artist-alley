-- name: CreateAsset :one
INSERT INTO assets (
    title, description, resource_type, owner_user_ref, status,
    file_hash, file_extension, file_size_bytes, metadata, origin_server_id,
    state_id, processing_status, thumbhash
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
    $11, $12, $13
)
RETURNING id, title, description, resource_type, owner_user_ref, status,
          file_hash, file_extension, file_size_bytes, metadata,
          origin_server_id, state_id, processing_status, thumbhash,
          created_at, updated_at;

-- name: GetAsset :one
SELECT id, title, description, resource_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at
FROM assets
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateAsset :one
-- Partial update via COALESCE: any field passed as NULL keeps its
-- current value. Tag changes go through a separate set of queries.
UPDATE assets SET
    title       = COALESCE(sqlc.narg('title'),       title),
    description = COALESCE(sqlc.narg('description'), description),
    status      = COALESCE(sqlc.narg('status'),      status),
    metadata    = COALESCE(sqlc.narg('metadata'),    metadata),
    updated_at  = NOW()
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING id, title, description, resource_type, owner_user_ref, status,
          file_hash, file_extension, file_size_bytes, metadata,
          origin_server_id, state_id, processing_status, thumbhash,
          created_at, updated_at;

-- name: SoftDeleteAsset :exec
UPDATE assets
SET deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListAssetsPage :many
-- Cursor pagination: rows newer than the cursor timestamp, plus tie-
-- breaker on id. Filters are OR'd with NULL-checks so a single query
-- covers all the optional-filter combinations.
--
-- `q` is an optional plain-text search query that runs against the
-- search_text TSVECTOR column (maintained by the Phase 1.9 metadata
-- trigger across asset title/description/tags + field values). Backed
-- by the assets_search_text_gin index. Phase 1.12 will replace this
-- with the proper search DSL (ADR 0010), but for the browse page MVP
-- a plain tsquery match is enough.
SELECT id, title, description, resource_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at
FROM assets
WHERE deleted_at IS NULL
  AND (sqlc.narg('owner_user_ref')::BIGINT IS NULL OR owner_user_ref = sqlc.narg('owner_user_ref')::BIGINT)
  AND (sqlc.narg('resource_type')::BIGINT  IS NULL OR resource_type  = sqlc.narg('resource_type')::BIGINT)
  AND (sqlc.narg('status')::TEXT           IS NULL OR status          = sqlc.narg('status')::TEXT)
  AND (sqlc.narg('q')::TEXT                IS NULL
       OR search_text @@ plainto_tsquery('english', sqlc.narg('q')::TEXT))
  AND (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
       OR created_at < sqlc.narg('cursor_created_at')::TIMESTAMPTZ
       OR (created_at = sqlc.narg('cursor_created_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: ListAssetsByTagPage :many
-- Same paginated list but constrained to a single tag. Separate
-- query because the join breaks the COALESCE pattern.
SELECT a.id, a.title, a.description, a.resource_type, a.owner_user_ref, a.status,
       a.file_hash, a.file_extension, a.file_size_bytes, a.metadata,
       a.origin_server_id, a.state_id, a.processing_status, a.thumbhash,
       a.created_at, a.updated_at
FROM assets a
JOIN asset_tag t ON t.asset_id = a.id
WHERE a.deleted_at IS NULL
  AND t.tag = sqlc.arg('tag')::TEXT
  AND (sqlc.narg('owner_user_ref')::BIGINT IS NULL OR a.owner_user_ref = sqlc.narg('owner_user_ref')::BIGINT)
  AND (sqlc.narg('resource_type')::BIGINT  IS NULL OR a.resource_type  = sqlc.narg('resource_type')::BIGINT)
  AND (sqlc.narg('status')::TEXT           IS NULL OR a.status          = sqlc.narg('status')::TEXT)
  AND (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
       OR a.created_at < sqlc.narg('cursor_created_at')::TIMESTAMPTZ
       OR (a.created_at = sqlc.narg('cursor_created_at')::TIMESTAMPTZ
           AND a.id < sqlc.narg('cursor_id')::UUID))
ORDER BY a.created_at DESC, a.id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: ListAssetTags :many
SELECT tag FROM asset_tag WHERE asset_id = $1 ORDER BY tag;

-- name: AddAssetTag :exec
INSERT INTO asset_tag (asset_id, tag)
VALUES ($1, $2)
ON CONFLICT (asset_id, tag) DO NOTHING;

-- name: RemoveAssetTag :exec
DELETE FROM asset_tag WHERE asset_id = $1 AND tag = $2;

-- name: ReplaceAssetTags :exec
-- Wipes and refills the tag set in one transaction. Called by
-- AssetUpdate when the request body sends a `tags` array.
WITH wipe AS (
    DELETE FROM asset_tag WHERE asset_id = $1
)
INSERT INTO asset_tag (asset_id, tag)
SELECT $1, unnest($2::TEXT[])
ON CONFLICT (asset_id, tag) DO NOTHING;
