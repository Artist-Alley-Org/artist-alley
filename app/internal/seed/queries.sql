-- Admin-only seed/loader queries — backfill timestamps + forge
-- comments. NOT for general operator use.

-- name: BackfillAssetTimestamps :execrows
-- Bulk UPDATE assets.created_at / updated_at from a JSONB
-- array of {id, created_at, updated_at} objects. JSONB +
-- jsonb_to_recordset keeps params simple (one arg) + lets
-- Postgres + sqlc infer types cleanly vs the multi-arg
-- unnest pattern.
UPDATE assets a
   SET created_at = u.created_at,
       updated_at = u.updated_at
  FROM jsonb_to_recordset(sqlc.arg('payload')::jsonb)
       AS u(id uuid,
            created_at timestamptz,
            updated_at timestamptz)
 WHERE a.id = u.id;

-- name: BackfillPostTimestamps :execrows
-- Same shape minus last_reviewed_at. Also touches posted_at so
-- the public feed ordering matches the seeded timeline (posts
-- order by posted_at, not created_at).
UPDATE posts p
   SET created_at = u.created_at,
       updated_at = u.updated_at,
       posted_at  = u.created_at
  FROM jsonb_to_recordset(sqlc.arg('payload')::jsonb)
       AS u(id uuid,
            created_at timestamptz,
            updated_at timestamptz)
 WHERE p.id = u.id;

-- name: BackfillCommentTimestamps :execrows
UPDATE comments c
   SET created_at = u.created_at,
       updated_at = u.updated_at
  FROM jsonb_to_recordset(sqlc.arg('payload')::jsonb)
       AS u(id uuid,
            created_at timestamptz,
            updated_at timestamptz)
 WHERE c.id = u.id;

-- name: SeedInsertUser :one
-- Forged username + optional password (NULL when omitted —
-- user can't log in but can be referenced as an actor on
-- posts/comments) + optional created_at. ON CONFLICT
-- (username) DO NOTHING + caller re-fetches via
-- SeedGetUserByUsername when 0 rows returned — that's how
-- idempotent re-runs surface the existing row to the client.
INSERT INTO "user" (
    username, password, fullname, email, usergroup, approved, created
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (username) DO NOTHING
RETURNING ref, username, fullname, email, usergroup, approved, created;

-- name: SeedGetUserByUsername :one
-- Re-fetch path for idempotent SeedInsertUser conflicts.
SELECT ref, username, fullname, email, usergroup, approved, created
FROM "user"
WHERE username = $1;

-- name: SeedAuthorExists :one
SELECT 1 AS ok FROM "user" WHERE ref = $1;

-- name: SeedPostExists :one
SELECT 1 AS ok FROM posts WHERE id = $1 AND deleted_at IS NULL;

-- name: SeedAssetExists :one
SELECT 1 AS ok FROM assets WHERE id = $1;

-- name: SeedCollectionExists :one
SELECT 1 AS ok FROM collections WHERE id = $1;

-- name: SeedGetCommentByID :one
SELECT id, target_kind, target_id, parent_id, root_id, depth,
       author_user_ref, body, body_html,
       annotation_type, annotation_data,
       like_count, edited_at, deleted_at,
       origin_server_id, created_at, updated_at,
       peer_id, actor_uri, activity_uri
FROM comments
WHERE id = $1;

-- name: SeedGetCommentParentInfo :one
-- Fetches root_id + depth from a parent comment so the new row's
-- thread placement matches the public CreateComment path's
-- semantics.
SELECT root_id, depth
FROM comments
WHERE id = $1 AND deleted_at IS NULL;

-- name: SeedInsertComment :one
-- Forged-author + optional created_at + optional stable id.
-- ON CONFLICT (id) DO NOTHING + the caller re-fetches via
-- SeedGetCommentByID when 0 rows returned — that's how
-- idempotent re-runs surface the existing row to the client.
INSERT INTO comments (
    id, target_kind, target_id, parent_id, root_id, depth,
    author_user_ref, body, body_html,
    annotation_type, annotation_data,
    created_at, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
ON CONFLICT (id) DO NOTHING
RETURNING id, target_kind, target_id, parent_id, root_id, depth,
          author_user_ref, body, body_html,
          annotation_type, annotation_data,
          like_count, edited_at, deleted_at,
          origin_server_id, created_at, updated_at,
          peer_id, actor_uri, activity_uri;
