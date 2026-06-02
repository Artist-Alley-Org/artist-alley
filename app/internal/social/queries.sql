-- Comments + likes queries. Polymorphic across (target_kind, target_id),
-- so one set of queries serves posts, assets, and (later) collections.
-- See migration 00020.

-- ---------------------------------------------------------------------------
-- Likes
-- ---------------------------------------------------------------------------

-- name: LikeTarget :exec
-- Idempotent. The PRIMARY KEY (target_kind, target_id, rs_user_id)
-- means re-inserting a row the same user already liked is a no-op,
-- and the counter trigger doesn't fire a second time.
INSERT INTO likes (target_kind, target_id, rs_user_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: UnlikeTarget :execrows
-- Returns 1 if a row was removed (and the trigger decremented the
-- counter), 0 if the user didn't have a like to remove. Handler maps
-- 0 to a 404 so the client knows its optimistic update was wrong.
DELETE FROM likes
WHERE target_kind = $1 AND target_id = $2 AND rs_user_id = $3;

-- name: HasUserLikedTarget :one
SELECT EXISTS (
    SELECT 1 FROM likes
    WHERE target_kind = $1 AND target_id = $2 AND rs_user_id = $3
) AS value;

-- name: ListLikersOfTarget :many
-- Used by the post modal's "liked by X, Y, and 17 others" surface.
-- Newest likes first; the handler caps at the limit and offers an
-- offset cursor for "show more".
SELECT rs_user_id, liked_at
FROM likes
WHERE target_kind = $1 AND target_id = $2
ORDER BY liked_at DESC, rs_user_id ASC
LIMIT $3 OFFSET $4;

-- ---------------------------------------------------------------------------
-- Comments
-- ---------------------------------------------------------------------------

-- name: CreateComment :one
-- The Go caller generates the id up front so top-level comments can
-- have root_id = id at INSERT time (the column is NOT NULL, so we
-- can't post-set it). For replies the caller passes the parent's
-- root_id; depth comes from parent.depth + 1.
INSERT INTO comments (
    id, target_kind, target_id, parent_id, root_id, depth,
    author_user_ref, body, body_html, annotation_type, annotation_data
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id, target_kind, target_id, parent_id, root_id, depth,
          author_user_ref, body, body_html,
          annotation_type, annotation_data,
          like_count, edited_at, deleted_at,
          origin_server_id, created_at, updated_at;

-- name: GetComment :one
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at
FROM comments
WHERE id = $1;

-- name: UpdateComment :one
-- PATCH-style. body update sets edited_at; body_html is recomputed
-- by the caller. Empty annotation_type clears the annotation.
UPDATE comments SET
    body            = COALESCE(sqlc.narg('body'),            body),
    body_html       = COALESCE(sqlc.narg('body_html'),       body_html),
    annotation_type = sqlc.narg('annotation_type'),
    annotation_data = sqlc.narg('annotation_data'),
    edited_at       = CASE WHEN sqlc.narg('body') IS NOT NULL THEN NOW() ELSE edited_at END,
    updated_at      = NOW()
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING id, target_kind, target_id, parent_id, root_id, depth,
          author_user_ref, body, body_html,
          annotation_type, annotation_data,
          like_count, edited_at, deleted_at,
          origin_server_id, created_at, updated_at;

-- name: SoftDeleteComment :execrows
-- Sets deleted_at + clears body so a soft-deleted comment doesn't
-- leak its content in admin views. The trigger decrements the
-- target's comment_count.
UPDATE comments
   SET deleted_at = NOW(),
       body       = '',
       body_html  = '',
       updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListThreadForTarget :many
-- Returns every LIVE comment on (kind, id), sorted by root then
-- depth-first within root (so replies appear under their parent).
-- The cursor pagination is on root_id alone — we always return a
-- whole thread together, so the cursor advances by root.
--
-- Ordering: root_created_at DESC (newest threads first), then within
-- a thread by (depth ASC, created_at ASC) so the root is first and
-- replies follow in chronological order.
WITH thread_roots AS (
    SELECT cr.id, cr.created_at
    FROM comments cr
    WHERE cr.target_kind = $1 AND cr.target_id = $2
      AND cr.parent_id IS NULL
      AND cr.deleted_at IS NULL
      AND (sqlc.narg('cursor_root_created_at')::timestamptz IS NULL
           OR cr.created_at < sqlc.narg('cursor_root_created_at')::timestamptz)
    ORDER BY cr.created_at DESC, cr.id ASC
    LIMIT sqlc.arg('thread_limit')::int
)
SELECT c.id, c.target_kind, c.target_id, c.parent_id, c.root_id, c.depth,
       c.author_user_ref, c.body, c.body_html,
       c.annotation_type, c.annotation_data,
       c.like_count, c.edited_at, c.deleted_at,
       c.origin_server_id, c.created_at, c.updated_at
FROM comments c
JOIN thread_roots tr ON tr.id = c.root_id
WHERE c.deleted_at IS NULL
ORDER BY tr.created_at DESC, tr.id ASC, c.depth ASC, c.created_at ASC;

-- name: ListCommentsByAuthor :many
-- "Things X commented on" — used by the user profile page later.
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at
FROM comments
WHERE author_user_ref = $1
  AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListAnnotationsForAsset :many
-- Future review-mode UI: fetch every annotation comment on an asset
-- for overlay rendering. Only comments with annotation_type set are
-- returned — the same table, but the partial index makes this
-- cheap.
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at
FROM comments
WHERE target_kind = 'asset'
  AND target_id = $1
  AND annotation_type IS NOT NULL
  AND deleted_at IS NULL
ORDER BY created_at ASC;

-- name: ListTextAnnotationsForAsset :many
-- Doc-viewer review panel: every text-range annotation on an asset,
-- newest first. Backed by the comments_text_annotations_idx partial
-- index (migration 00036). Replies to an annotation thread under it
-- via parent_id and surface through the normal thread query; this
-- list returns only the top-level anchors.
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at
FROM comments
WHERE target_kind = 'asset'
  AND target_id = $1
  AND annotation_type = 'text-range'
  AND parent_id IS NULL
  AND deleted_at IS NULL
ORDER BY created_at ASC;

-- name: UpdateAnnotationData :one
-- Narrow update for the doc-viewer's "change color / change style /
-- toggle resolved / edit comment body" actions. Replaces the full
-- annotation_data blob (the panel sends the merged shape it wants)
-- and optionally updates the body text. Caller must guarantee the
-- comment is an annotation; we don't enforce annotation_type here so
-- the same query serves whiteboard PATCH too if needed later.
UPDATE comments SET
    body            = COALESCE(sqlc.narg('body'), body),
    annotation_data = sqlc.arg('annotation_data'),
    edited_at       = CASE WHEN sqlc.narg('body') IS NOT NULL THEN NOW() ELSE edited_at END,
    updated_at      = NOW()
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING id, target_kind, target_id, parent_id, root_id, depth,
          author_user_ref, body, body_html,
          annotation_type, annotation_data,
          like_count, edited_at, deleted_at,
          origin_server_id, created_at, updated_at;

-- name: ListWhiteboardsForPost :many
-- Sidebar "Whiteboards" surface — every whiteboard sketch on a post,
-- newest first. Whiteboards are top-level comments
-- (parent_id IS NULL) with annotation_type='whiteboard'; reply
-- comments to a whiteboard show up via the existing thread query, not
-- here. The comments_whiteboards_idx partial index (migration 00029)
-- covers this exactly: (target_kind, target_id, created_at DESC)
-- WHERE annotation_type='whiteboard' AND deleted_at IS NULL.
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at
FROM comments
WHERE target_kind = 'post'
  AND target_id = $1
  AND annotation_type = 'whiteboard'
  AND parent_id IS NULL
  AND deleted_at IS NULL
ORDER BY created_at DESC;
