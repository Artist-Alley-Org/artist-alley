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
-- deleted_by_user_ref: see the note on assets.SoftDeleteAsset. The
-- restore gate reads it, so every soft-delete path has to write it.
UPDATE posts SET deleted_at = NOW(), deleted_reason = $2, deleted_by_user_ref = $3, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetPostDeletedBy :one
-- Who soft-deleted this post. pgx.ErrNoRows when the row is live or
-- absent — the two cases the restore path already conflates.
SELECT deleted_by_user_ref
  FROM posts
 WHERE id = $1 AND deleted_at IS NOT NULL;

-- The two post LIST queries (ListPostsPage, ListPostsByAsset) are NOT
-- here. They live in list_page.go as hand-built SQL, because the read
-- rule they must apply is a runtime fragment (readRuleSQL) and a sqlc
-- query is a static string — the same reason every splice site of
-- visibility.Predicate is hand-built.
--
-- They were deleted from this file rather than left in place unused
-- (#660). The version that lived here took a caller-supplied
-- `visibility` and applied it as a bare equality, documented as "NULL
-- for whatever the caller is authorised to see (handler enforces)" —
-- and the handler did not enforce, so `?visibility=private` returned
-- every author's private posts to any signed-in caller. A second,
-- ungated expression of "which posts does this list return" is exactly
-- what produced that bug; leaving one here for the next caller to pick
-- up would rebuild it.

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

-- name: PostIDsForAsset :many
-- Every post that lists this asset as a member. Drives cache
-- invalidation when the ASSET changes in a way that changes what
-- ListPostAssets returns — soft-delete and restore, which flip the
-- `a.deleted_at IS NULL` half of that join without touching any post
-- row (#920). No visibility filter: this answers "whose cached copy is
-- now wrong", which is every holder, not just the ones a given reader
-- may see.
SELECT post_id FROM post_assets WHERE asset_id = $1;

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

-- ListCollectionPostsPage was DELETED here (#661, epic #665). It listed
-- a collection's pinned posts with `p.deleted_at IS NULL` as its only
-- post-side condition — no visibility rule at all — and nothing in the
-- tree called it: no handler, no test, and its generated row/param
-- types were referenced nowhere. An unused query that would leak every
-- private post in a collection the day somebody wired it up is not a
-- head start, it is a trap; deleting it is strictly better than
-- auditing it. A future collection-posts listing must go through
-- the post read rule (posts.readRuleSQL) the way ListPostsByAssetGated does.

-- ---------------------------------------------------------------------------
-- ACLs (Phase 1.7.B-7b)
-- ---------------------------------------------------------------------------

-- name: ListPostAcls :many
-- All ACL rows on a post, newest first. The handler filters expired
-- rows out of effective-access checks; this endpoint shows them so
-- admins can see "X had read access until last week".
SELECT post_id, principal_type, principal_id, permission,
       granted_at, granted_by_user_ref, expires_at
FROM post_acls
WHERE post_id = $1
ORDER BY granted_at DESC, principal_type, principal_id, permission;

-- name: AddPostAcl :exec
-- Idempotent on the full (post, principal, permission) PK. Adding the
-- same row twice is a no-op (returns 204 the second time).
INSERT INTO post_acls (post_id, principal_type, principal_id, permission,
                       granted_by_user_ref, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (post_id, principal_type, principal_id, permission) DO UPDATE SET
    granted_at            = NOW(),
    granted_by_user_ref = EXCLUDED.granted_by_user_ref,
    expires_at            = EXCLUDED.expires_at;

-- name: RemovePostAcl :execrows
DELETE FROM post_acls
WHERE post_id = $1
  AND principal_type = $2
  AND principal_id   = $3
  AND permission     = $4;

-- name: GetWorkflowStateIDByCode :one
-- Resolve one workflow state's id from its stable (domain, code) key
-- — for posts, ('post','published') and ('post','wip') (ADR 0091
-- decision 7). UNIQUE (domain, code), so this is one index probe.
--
-- This package reads the row rather than caching the UUID because a
-- cached id is silently wrong the first time an install reseeds its
-- state machine, and "silently wrong" here means every post looks like
-- a draft. It is asked once per create and once per cache MISS on the
-- read path, never per row.
SELECT id FROM workflow_states WHERE domain = $1 AND code = $2;

-- name: GetPostInitialStateID :one
-- The domain's entry-point state (`is_initial`), used for a post that
-- is created already published. Asked by name rather than assumed to be
-- 'published' so an install that moved its entry point is obeyed; a
-- partial unique index guarantees at most one row per domain.
SELECT id FROM workflow_states WHERE domain = $1 AND is_initial = TRUE LIMIT 1;

-- name: GetAssetOwnerRef :one
-- The asset's owner, for the ownership gate on GET /assets/{id}/posts
-- (ADR 0091 decision 5). Soft-deleted assets answer no rows: a deleted
-- file has no "where does it appear" to report.
SELECT owner_user_ref FROM assets WHERE id = $1 AND deleted_at IS NULL;

-- name: CountLivePostsForAsset :one
-- How many live posts contain this asset, with NO read rule applied.
--
-- The raw total is half of decision 5's disclosure: the handler
-- subtracts the posts the caller may actually read and reports the
-- remainder as `withheld_count`. It is deliberately a COUNT and not a
-- list — see the operation's description for why an id, a title or a
-- cursor over the same set would undo the whole point.
SELECT COUNT(*)::BIGINT AS value
FROM posts p
WHERE p.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM post_assets pa
                WHERE pa.post_id = p.id AND pa.asset_id = $1);
