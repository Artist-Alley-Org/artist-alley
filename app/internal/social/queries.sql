-- Comments + likes queries. Polymorphic across (target_kind, target_id),
-- so one set of queries serves posts, assets, and (later) collections.
-- See migration 00020.

-- ---------------------------------------------------------------------------
-- Likes
-- ---------------------------------------------------------------------------

-- name: LikeTarget :exec
-- Idempotent. The partial-UNIQUE index
-- likes_local_uniq_idx on (target_kind, target_id, user_ref)
-- WHERE user_ref IS NOT NULL means re-inserting a row the same
-- LOCAL user already liked is a no-op, and the counter trigger
-- doesn't fire a second time.
INSERT INTO likes (target_kind, target_id, user_ref)
VALUES ($1, $2, $3)
ON CONFLICT (target_kind, target_id, user_ref) WHERE user_ref IS NOT NULL DO NOTHING;

-- name: InsertRemoteLike :execrows
-- Inbound federation Like from a remote actor on a target the
-- local instance owns. Idempotent via likes_remote_uniq_idx
-- on (target_kind, target_id, peer_id, actor_uri) WHERE peer_id
-- IS NOT NULL. ON CONFLICT clause must repeat the partial-index
-- WHERE predicate so Postgres can match the right index.
INSERT INTO likes (target_kind, target_id, peer_id, actor_uri)
VALUES ($1, $2, $3, $4)
ON CONFLICT (target_kind, target_id, peer_id, actor_uri) WHERE peer_id IS NOT NULL DO NOTHING;

-- name: UnlikeTarget :execrows
-- Returns 1 if a row was removed (and the trigger decremented the
-- counter), 0 if the user didn't have a like to remove. Handler maps
-- 0 to a 404 so the client knows its optimistic update was wrong.
DELETE FROM likes
WHERE target_kind = $1 AND target_id = $2 AND user_ref = $3;

-- name: HasUserLikedTarget :one
SELECT EXISTS (
    SELECT 1 FROM likes
    WHERE target_kind = $1 AND target_id = $2 AND user_ref = $3
) AS value;

-- name: ListLikersOfTarget :many
-- Used by the post modal's "liked by X, Y, and 17 others" surface.
-- Newest likes first; the handler caps at the limit and offers an
-- offset cursor for "show more".
SELECT user_ref, liked_at
FROM likes
WHERE target_kind = $1 AND target_id = $2
ORDER BY liked_at DESC, user_ref ASC
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
          origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri;

-- name: GetComment :one
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri
FROM comments
WHERE id = $1;

-- name: InsertRemoteComment :one
-- Inbound federation Create(Note) — the remote actor posted a
-- comment on a target the local instance owns. Idempotent via
-- comments_activity_uri_uniq_idx UNIQUE (activity_uri) WHERE
-- NOT NULL — a retried dispatch lands as a constraint-violation
-- we map to "already saved" rather than a new row.
--
-- author_user_ref is NULL for remote rows per the origin CHECK;
-- peer_id + actor_uri + activity_uri together identify the
-- remote authorship.
INSERT INTO comments (
    id, target_kind, target_id, parent_id, root_id, depth,
    body, body_html,
    peer_id, actor_uri, activity_uri
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id, target_kind, target_id, parent_id, root_id, depth,
          author_user_ref, body, body_html,
          annotation_type, annotation_data,
          like_count, edited_at, deleted_at,
          origin_server_id, peer_id, actor_uri, activity_uri,
          created_at, updated_at;

-- name: GetCommentByActivityURI :one
-- Used by the inbox dispatcher to detect a retried delivery —
-- if a row already exists for this activity_uri, the dispatcher
-- treats it as "already processed" without re-firing the
-- notification.
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       peer_id, actor_uri, activity_uri,
       created_at, updated_at
FROM comments
WHERE activity_uri = $1;

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
          origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri;

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
--
-- ## Caching note (post-1.22.D audit, 2026-06-09)
--
-- The LEFT JOIN against federation_remote_actors below denormalises
-- the remote actor's display_name into each comment row in one
-- query instead of N+1 row-level GetRemoteActor lookups. This is
-- the correct pattern when the joined table is:
--   - small (one row per ever-seen remote actor),
--   - indexed (federation_remote_actors uses actor_uri as PK),
--   - low-churn (display fields refresh on inbound activity only),
--   - hot enough to live in Postgres shared_buffers.
--
-- A row-level cache on GetRemoteActor would add invalidation
-- surface without saving query work — the planner collapses the
-- JOIN into a hash lookup against the indexed actor_uri. The
-- GetRemoteActor query in federation/remote/queries.sql is
-- currently unused; future row-level callers should re-check this
-- comment before adding a cache.
--
-- If the comments-list endpoint becomes a perf bottleneck during
-- dogfood, the right cache then is the COMMENTS-BY-POST page
-- result (one entry per target_id + cursor), not the per-actor
-- row. Track via cache.Registry following the cacheDomainPostByID
-- pattern in posts/handler.go.
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
       c.origin_server_id, c.created_at, c.updated_at,
       c.peer_id, c.actor_uri, c.activity_uri,
       fra.display_name::text AS remote_display_name
FROM comments c
JOIN thread_roots tr ON tr.id = c.root_id
LEFT JOIN federation_remote_actors fra ON fra.actor_uri = c.actor_uri
WHERE c.deleted_at IS NULL
ORDER BY tr.created_at DESC, tr.id ASC, c.depth ASC, c.created_at ASC;

-- name: ListCommentsByAuthor :many
-- "Things X commented on" — used by the user profile page later.
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri
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
       origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri
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
       origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri
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
          origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri;

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
       origin_server_id, created_at, updated_at, peer_id, actor_uri, activity_uri
FROM comments
WHERE target_kind = 'post'
  AND target_id = $1
  AND annotation_type = 'whiteboard'
  AND parent_id IS NULL
  AND deleted_at IS NULL
ORDER BY created_at DESC;

-- ---------------------------------------------------------------------------
-- Social graph: follows + blocks (Phase 1.17.G2)
-- ---------------------------------------------------------------------------

-- name: FollowUser :exec
-- Idempotent — re-following someone you already follow is a no-op
-- rather than a primary-key violation, so the handler can return 204
-- without distinguishing first-follow from already-following.
INSERT INTO user_follows (follower_user_ref, followee_user_ref)
VALUES ($1, $2)
ON CONFLICT (follower_user_ref, followee_user_ref) DO NOTHING;

-- name: UnfollowUser :execrows
-- Rows-affected so the handler can 404 cleanly when the caller
-- wasn't following the target. The handler maps "0 rows deleted"
-- to 204 anyway (idempotent unfollow) but having the count makes
-- audit + metrics writers happy.
DELETE FROM user_follows
WHERE follower_user_ref = $1
  AND followee_user_ref = $2;

-- name: IsFollowing :one
-- The load-bearing check for visibility='followers' posts (wires
-- the long-parked posts.handler.go:877 TODO). Single-row EXISTS
-- against the PK so it's a sub-ms query.
SELECT EXISTS (
    SELECT 1 FROM user_follows
    WHERE follower_user_ref = $1
      AND followee_user_ref = $2
) AS following;

-- name: CountFollowers :one
-- "X followers" badge on a profile page. The followee-indexed lookup
-- (idx_user_follows_followee) makes this a small index-only scan.
SELECT COUNT(*)::BIGINT AS count
FROM user_follows
WHERE followee_user_ref = $1;

-- name: CountFollowing :one
-- "Following Y" badge on a profile page.
SELECT COUNT(*)::BIGINT AS count
FROM user_follows
WHERE follower_user_ref = $1;

-- name: ListFollowers :many
-- Paginated reverse lookup — who follows the given user. Joined
-- against "user" + user_profiles so the response carries enough to
-- render the user card without a second round-trip. Cursor uses
-- created_at as a stable sort key with PK breakage on ties.
SELECT u.ref,
       u.username,
       up.display_name,
       up.avatar_url,
       f.created_at
FROM user_follows f
JOIN "user" u             ON u.ref = f.follower_user_ref
LEFT JOIN user_profiles up ON up.user_ref = u.ref
WHERE f.followee_user_ref = $1
ORDER BY f.created_at DESC, f.follower_user_ref DESC
LIMIT $2;

-- name: ListFollowing :many
-- Symmetric to ListFollowers — who the given user follows.
SELECT u.ref,
       u.username,
       up.display_name,
       up.avatar_url,
       f.created_at
FROM user_follows f
JOIN "user" u             ON u.ref = f.followee_user_ref
LEFT JOIN user_profiles up ON up.user_ref = u.ref
WHERE f.follower_user_ref = $1
ORDER BY f.created_at DESC, f.followee_user_ref DESC
LIMIT $2;

-- name: BlockUser :exec
-- Upserts the block edge with an optional reason. The handler also
-- runs UnfollowUser in both directions before BlockUser — blocking
-- and continuing to follow is a contradictory state, and modern
-- social platforms all auto-unfollow on block.
INSERT INTO user_blocks (blocker_user_ref, blocked_user_ref, reason)
VALUES ($1, $2, $3)
ON CONFLICT (blocker_user_ref, blocked_user_ref) DO UPDATE
    SET reason = EXCLUDED.reason;

-- name: UnblockUser :execrows
DELETE FROM user_blocks
WHERE blocker_user_ref = $1
  AND blocked_user_ref = $2;

-- name: HasBlockBetween :one
-- The "are A and B mutually visible?" gate. Returns true if EITHER
-- direction of the block edge exists. Consumed by visibility-aware
-- writers (notification dispatcher, DM delivery in 1.17.I, mention
-- resolution) to short-circuit before doing work.
SELECT EXISTS (
    SELECT 1 FROM user_blocks
    WHERE (blocker_user_ref = $1 AND blocked_user_ref = $2)
       OR (blocker_user_ref = $2 AND blocked_user_ref = $1)
) AS blocked;

-- name: ListBlocked :many
-- "Users I've blocked" management page (/account/blocked). Only the
-- blocker's perspective — legacy-style "show me who blocked me" is
-- deliberately NOT exposed (most platforms hide this for the
-- blocker's privacy).
SELECT u.ref,
       u.username,
       up.display_name,
       up.avatar_url,
       b.reason,
       b.created_at
FROM user_blocks b
JOIN "user" u             ON u.ref = b.blocked_user_ref
LEFT JOIN user_profiles up ON up.user_ref = u.ref
WHERE b.blocker_user_ref = $1
ORDER BY b.created_at DESC, b.blocked_user_ref DESC
LIMIT $2;

-- name: IsBlocking :one
-- Single-direction block check, consumed by GetUserRelationship to
-- populate the per-direction is_blocked_by_me / is_blocked_by_them
-- booleans. HasBlockBetween is the bidirectional visibility gate;
-- IsBlocking is for "did THIS user block that one?" reporting.
SELECT EXISTS (
    SELECT 1 FROM user_blocks
    WHERE blocker_user_ref = $1
      AND blocked_user_ref = $2
) AS blocking;

-- name: UserExists :one
-- Cheap existence check used by the follow / block handlers to map
-- "no such user" to 404 BEFORE the downstream write would surface a
-- less intelligible error. The PK lookup is sub-ms.
SELECT ref FROM "user" WHERE ref = $1 LIMIT 1;

-- name: GetPostAuthorAndTitle :one
-- Tiny lookup the notification emitter uses to know who to notify
-- + populate the inbox card with the post title. Single-row index
-- hit on posts.id PK; sub-ms.
SELECT author_user_ref, title
FROM posts
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetCommentAuthorAndContext :one
-- Tiny lookup the comment reply path needs — pulls the comment's
-- author + parent_id so we can fire "reply_to_my_comment" against
-- the parent comment's author (and skip when there isn't one).
SELECT author_user_ref, parent_id, target_kind, target_id
FROM comments
WHERE id = $1 AND deleted_at IS NULL;
