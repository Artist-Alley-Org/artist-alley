-- ---------------------------------------------------------------------------
-- posts (the entity)
-- ---------------------------------------------------------------------------

-- name: CreatePost :one
-- posted_at defaults to NOW() via the column default. team_id is
-- optional (NULL = un-scoped post; scoped post visibility is gated by
-- the post's team_id and the caller's scoped caps — see ADR 0010 L5).
-- cover_thumbnail_asset_id is an optional standalone thumbnail (not
-- a post member) used by the upload modal's "use a different image
-- as the cover" UX. state_id is the workflow state in the 'post'
-- domain — NULL means no workflow tracking.
INSERT INTO posts (
    author_user_ref, title, description, visibility, cover_asset_id,
    cover_thumbnail_asset_id, team_id, state_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, author_user_ref, title, description, visibility, cover_asset_id,
          cover_thumbnail_asset_id, posted_at, like_count, comment_count,
          origin_server_id, team_id, state_id, created_at, updated_at;

-- name: GetPost :one
SELECT id, author_user_ref, title, description, visibility, cover_asset_id,
       cover_thumbnail_asset_id, posted_at, like_count, comment_count,
       origin_server_id, team_id, state_id, created_at, updated_at
FROM posts
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdatePost :one
-- COALESCE-based partial update — NULL args keep current values.
UPDATE posts SET
    title                    = COALESCE(sqlc.narg('title'),                    title),
    description              = COALESCE(sqlc.narg('description'),              description),
    visibility               = COALESCE(sqlc.narg('visibility'),               visibility),
    cover_asset_id           = COALESCE(sqlc.narg('cover_asset_id'),           cover_asset_id),
    cover_thumbnail_asset_id = COALESCE(sqlc.narg('cover_thumbnail_asset_id'), cover_thumbnail_asset_id),
    state_id                 = COALESCE(sqlc.narg('state_id'),                 state_id),
    updated_at               = NOW()
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING id, author_user_ref, title, description, visibility, cover_asset_id,
          cover_thumbnail_asset_id, posted_at, like_count, comment_count,
          origin_server_id, team_id, state_id, created_at, updated_at;

-- name: SoftDeletePost :exec
UPDATE posts SET deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListPostsPage :many
-- Cursor pagination on (posted_at DESC, id DESC). Filters:
--   - author_user_ref: limit to posts by a given user
--   - visibility: 'public' for the public feed; NULL for whatever the
--     caller is authorised to see (handler enforces)
--   - q: plain-text TSVECTOR search across post search_text
--   - tag: single-tag filter (intersects with q if both given)
--   - feed_follower_ref (Phase 1.17.G2): when non-NULL, restrict the
--     feed to posts authored by users the given ref follows. EXISTS
--     subquery hits the user_follows PK (follower, followee) so it's
--     an index-only scan per candidate row — no nested loop.
SELECT id, author_user_ref, title, description, visibility, cover_asset_id,
       cover_thumbnail_asset_id, posted_at, like_count, comment_count,
       origin_server_id, team_id, state_id, created_at, updated_at
FROM posts
WHERE deleted_at IS NULL
  AND (sqlc.narg('author_user_ref')::BIGINT IS NULL
       OR author_user_ref = sqlc.narg('author_user_ref')::BIGINT)
  AND (sqlc.narg('visibility')::TEXT IS NULL
       OR visibility = sqlc.narg('visibility')::TEXT)
  AND (sqlc.narg('q')::TEXT IS NULL
       OR search_text @@ plainto_tsquery('english', sqlc.narg('q')::TEXT))
  AND (sqlc.narg('tag')::TEXT IS NULL
       OR EXISTS (SELECT 1 FROM post_tags pt
                    WHERE pt.post_id = posts.id
                      AND pt.tag = sqlc.narg('tag')::TEXT))
  AND (sqlc.narg('feed_follower_ref')::BIGINT IS NULL
       OR EXISTS (SELECT 1 FROM user_follows f
                    WHERE f.follower_user_ref = sqlc.narg('feed_follower_ref')::BIGINT
                      AND f.followee_user_ref = posts.author_user_ref))
  AND (sqlc.narg('cursor_posted_at')::TIMESTAMPTZ IS NULL
       OR posted_at < sqlc.narg('cursor_posted_at')::TIMESTAMPTZ
       OR (posted_at = sqlc.narg('cursor_posted_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY posted_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- ---------------------------------------------------------------------------
-- post_assets (members)
-- ---------------------------------------------------------------------------

-- name: AddPostAsset :exec
-- Idempotent on PK. Re-adding bumps sort_order; added_at stays fixed.
INSERT INTO post_assets (post_id, asset_id, sort_order)
VALUES ($1, $2, $3)
ON CONFLICT (post_id, asset_id) DO UPDATE SET
    sort_order = EXCLUDED.sort_order;

-- name: RemovePostAsset :exec
DELETE FROM post_assets WHERE post_id = $1 AND asset_id = $2;

-- name: ListPostAssets :many
-- Members of a post, in display order, joined onto the asset row so
-- the API can return the full member shape in one call (no N+1).
SELECT pa.post_id, pa.asset_id, pa.sort_order, pa.added_at,
       a.title, a.description, a.asset_type, a.owner_user_ref,
       a.status, a.file_hash, a.file_extension, a.file_size_bytes,
       a.metadata, a.created_at AS asset_created_at,
       a.updated_at AS asset_updated_at
FROM post_assets pa
JOIN assets a ON a.id = pa.asset_id
WHERE pa.post_id = $1 AND a.deleted_at IS NULL
ORDER BY pa.sort_order ASC, pa.added_at ASC;

-- ---------------------------------------------------------------------------
-- post_tags
-- ---------------------------------------------------------------------------

-- name: ListPostTags :many
SELECT tag FROM post_tags WHERE post_id = $1 ORDER BY tag;

-- name: AddPostTag :exec
INSERT INTO post_tags (post_id, tag)
VALUES ($1, $2)
ON CONFLICT (post_id, tag) DO NOTHING;

-- name: RemovePostTag :exec
DELETE FROM post_tags WHERE post_id = $1 AND tag = $2;

-- name: ReplacePostTags :exec
-- Wipes and refills the tag set in one transaction. Called by
-- UpdatePost when the body sends a `tags` array.
WITH wipe AS (
    DELETE FROM post_tags WHERE post_id = $1
)
INSERT INTO post_tags (post_id, tag)
SELECT $1, unnest($2::TEXT[])
ON CONFLICT (post_id, tag) DO NOTHING;

-- ---------------------------------------------------------------------------
-- collection_posts
-- ---------------------------------------------------------------------------

-- name: AddCollectionPost :exec
-- Idempotent on (collection_id, post_id). Re-adding updates
-- sort_order/pinned/expires_at; added_at stays fixed.
INSERT INTO collection_posts (
    collection_id, post_id, sort_order, pinned, expires_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (collection_id, post_id) DO UPDATE SET
    sort_order = EXCLUDED.sort_order,
    pinned     = EXCLUDED.pinned,
    expires_at = EXCLUDED.expires_at;

-- name: RemoveCollectionPost :exec
DELETE FROM collection_posts WHERE collection_id = $1 AND post_id = $2;

-- name: ListCollectionPostsPage :many
-- Pinned posts in a collection, sort_order then added_at. Excludes
-- expired memberships and soft-deleted posts. Returns the post row
-- joined with its cover_asset for the grid render.
SELECT cp.collection_id, cp.post_id, cp.sort_order, cp.pinned,
       cp.expires_at, cp.added_at,
       p.author_user_ref, p.title, p.description, p.visibility,
       p.cover_asset_id, p.posted_at, p.like_count, p.comment_count,
       p.created_at AS post_created_at,
       p.updated_at AS post_updated_at
FROM collection_posts cp
JOIN posts p ON p.id = cp.post_id
WHERE cp.collection_id = $1
  AND cp.pinned = TRUE
  AND (cp.expires_at IS NULL OR cp.expires_at > NOW())
  AND p.deleted_at IS NULL
  AND (sqlc.narg('cursor_sort_order')::INTEGER IS NULL
       OR cp.sort_order > sqlc.narg('cursor_sort_order')::INTEGER
       OR (cp.sort_order = sqlc.narg('cursor_sort_order')::INTEGER
           AND cp.added_at > sqlc.narg('cursor_added_at')::TIMESTAMPTZ))
ORDER BY cp.sort_order ASC, cp.added_at ASC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- ---------------------------------------------------------------------------
-- ACLs (Phase 1.7.B-7b)
-- ---------------------------------------------------------------------------

-- name: ListPostAcls :many
-- All ACL rows on a post, newest first. The handler filters expired
-- rows out of effective-access checks; this endpoint shows them so
-- admins can see "X had read access until last week".
SELECT post_id, principal_type, principal_id, permission,
       granted_at, granted_by_rs_user_id, expires_at
FROM post_acls
WHERE post_id = $1
ORDER BY granted_at DESC, principal_type, principal_id, permission;

-- name: AddPostAcl :exec
-- Idempotent on the full (post, principal, permission) PK. Adding the
-- same row twice is a no-op (returns 204 the second time).
INSERT INTO post_acls (post_id, principal_type, principal_id, permission,
                       granted_by_rs_user_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (post_id, principal_type, principal_id, permission) DO UPDATE SET
    granted_at            = NOW(),
    granted_by_rs_user_id = EXCLUDED.granted_by_rs_user_id,
    expires_at            = EXCLUDED.expires_at;

-- name: RemovePostAcl :execrows
DELETE FROM post_acls
WHERE post_id = $1
  AND principal_type = $2
  AND principal_id   = $3
  AND permission     = $4;
