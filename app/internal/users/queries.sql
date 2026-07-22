-- User profile read + write queries. Owned by app/internal/users.
--
-- The "user" table is legacy; we don't write to its sensitive columns
-- here. user_profiles is ours (migration 00021).

-- name: GetUserPublicByRef :one
-- Returns the join of (user, user_profile) with profile defaults
-- substituted when no profile row exists. The handler computes
-- display_name precedence: profile.display_name → user.fullname →
-- user.username, picking the first non-empty.
--
-- Counts (post_count, follower_count, following_count) are returned
-- separately via dedicated queries so the cache strategy can differ
-- per metric (post counts cache for minutes; following lists can
-- change between fetches).
SELECT u.ref                                            AS user_ref,
       u.username,
       u.fullname,
       u.created                                        AS created_at,
       COALESCE(p.display_name, '')                     AS display_name,
       COALESCE(p.bio, '')                              AS bio,
       p.avatar_url,
       COALESCE(p.location, '')                         AS location,
       p.website_url,
       COALESCE(p.social_links, '{}'::jsonb)            AS social_links,
       COALESCE(p.language, '')                         AS language,
       COALESCE(p.theme, '')                            AS theme,
       COALESCE(p.hide_from_anonymous, false)           AS hide_from_anonymous,
       p.origin_server_id                               AS profile_origin_server_id
FROM "user" u
LEFT JOIN user_profiles p ON p.user_ref = u.ref
WHERE u.ref = $1;

-- name: GetUserPublicByUsername :one
-- Same as above but keyed by username. Used by /@username URL pattern.
SELECT u.ref                                            AS user_ref,
       u.username,
       u.fullname,
       u.created                                        AS created_at,
       COALESCE(p.display_name, '')                     AS display_name,
       COALESCE(p.bio, '')                              AS bio,
       p.avatar_url,
       COALESCE(p.location, '')                         AS location,
       p.website_url,
       COALESCE(p.social_links, '{}'::jsonb)            AS social_links,
       COALESCE(p.language, '')                         AS language,
       COALESCE(p.theme, '')                            AS theme,
       COALESCE(p.hide_from_anonymous, false)           AS hide_from_anonymous,
       p.origin_server_id                               AS profile_origin_server_id
FROM "user" u
LEFT JOIN user_profiles p ON p.user_ref = u.ref
WHERE u.username = $1;

-- name: CountPostsByAuthor :one
-- Total live posts authored by this user. Used as the post_count
-- field on the public profile.
SELECT COUNT(*)::BIGINT AS value
FROM posts
WHERE author_user_ref = $1 AND deleted_at IS NULL;

-- name: ListAdminUsers :many
-- Admin user list (Phase 1.17.A). Joins `user` + user_profiles + the
-- user's primary role for the display row. Filters: `status` mirrors
-- the legacy `approved` column (1=active, 0=pending, 2=disabled — see
-- Phase 1.17.B), `q` runs case-insensitive prefix-ish match against
-- username / fullname / email. Cursor pagination keys on
-- (created_at DESC, ref DESC) — newest accounts first; admins
-- typically want recent signups front-and-centre.
--
-- The "first role" projection is the alphabetically-first global
-- assignment (team_id IS NULL). Per-team role assignments don't
-- show on this list; they live on the user detail page where the
-- team picker can scope them.
SELECT u.ref                                            AS user_ref,
       u.username,
       u.fullname,
       u.email,
       u.approved,
       u.created                                        AS created_at,
       u.last_active,
       u.origin                                         AS auth_origin,
       u.account_expires,
       u.lockout_until,
       u.failed_login_count,
       COALESCE(p.display_name, '')                     AS display_name,
       p.avatar_url,
       p.origin_server_id                               AS profile_origin_server_id,
       COALESCE((
         SELECT r.name
         FROM user_roles ur
         JOIN roles r ON r.id = ur.role_id
         WHERE ur.user_ref = u.ref AND ur.team_id IS NULL
         ORDER BY r.name
         LIMIT 1
       ), '')::TEXT                                     AS primary_role
FROM "user" u
LEFT JOIN user_profiles p ON p.user_ref = u.ref
WHERE
  -- status filter: 'active' = approved=1, 'pending' = approved=0,
  -- 'disabled' = approved=2. NULL filter = any.
  (sqlc.narg('status_value')::BIGINT IS NULL OR u.approved = sqlc.narg('status_value')::BIGINT)
  AND
  -- text search: case-insensitive across username, fullname, email.
  -- Empty / NULL = no filter.
  (
    sqlc.narg('search')::TEXT IS NULL OR sqlc.narg('search')::TEXT = ''
    OR LOWER(u.username) LIKE '%' || LOWER(sqlc.narg('search')::TEXT) || '%'
    OR LOWER(COALESCE(u.fullname, '')) LIKE '%' || LOWER(sqlc.narg('search')::TEXT) || '%'
    OR LOWER(COALESCE(u.email, '')) LIKE '%' || LOWER(sqlc.narg('search')::TEXT) || '%'
  )
  AND
  -- cursor: pages after (cursor_created_at, cursor_ref) in (created DESC, ref DESC) order.
  -- Lexicographic comparison handles the tiebreak when two users share a created timestamp.
  (
    sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
    OR (u.created, u.ref) < (sqlc.narg('cursor_created_at')::TIMESTAMPTZ, sqlc.narg('cursor_ref')::BIGINT)
  )
ORDER BY u.created DESC NULLS LAST, u.ref DESC
LIMIT sqlc.arg('limit_n')::BIGINT;

-- name: CountAdminUsers :one
-- Total matching rows (no cursor). Returned alongside the page so
-- the UI can render an "N users" badge without fetching every row.
SELECT COUNT(*)::BIGINT AS value
FROM "user" u
WHERE
  (sqlc.narg('status_value')::BIGINT IS NULL OR u.approved = sqlc.narg('status_value')::BIGINT)
  AND (
    sqlc.narg('search')::TEXT IS NULL OR sqlc.narg('search')::TEXT = ''
    OR LOWER(u.username) LIKE '%' || LOWER(sqlc.narg('search')::TEXT) || '%'
    OR LOWER(COALESCE(u.fullname, '')) LIKE '%' || LOWER(sqlc.narg('search')::TEXT) || '%'
    OR LOWER(COALESCE(u.email, '')) LIKE '%' || LOWER(sqlc.narg('search')::TEXT) || '%'
  );

-- name: UpdateUserStatus :one
-- Lifecycle mutation (Phase 1.17.B). Atomically swaps the user's
-- approved column to $1 and returns the prior value alongside the
-- new one + the user's username so the audit row can carry the
-- before/after pair without a second SELECT.
--
-- Returns no rows when the user doesn't exist; handler turns that
-- into a 404 rather than silently no-opping.
WITH prior AS (
  SELECT ref, username, approved FROM "user" WHERE ref = sqlc.arg('user_ref')::BIGINT
),
updated AS (
  UPDATE "user"
     SET approved = sqlc.arg('new_status')::BIGINT
   WHERE ref = sqlc.arg('user_ref')::BIGINT
     AND (SELECT approved FROM prior) <> sqlc.arg('new_status')::BIGINT
  RETURNING ref
)
SELECT prior.ref       AS user_ref,
       prior.username,
       prior.approved  AS prev_status,
       sqlc.arg('new_status')::BIGINT AS new_status,
       (SELECT COUNT(*) FROM updated)::BIGINT > 0 AS changed
FROM prior;

-- name: GetUserStatusByRef :one
-- Lightweight status-only read used by the handler's pre-write
-- short-circuit + the per-user cache invalidation. Doesn't touch
-- user_profiles.
SELECT ref AS user_ref, username, approved
FROM "user"
WHERE ref = $1;

-- name: UpsertUserProfile :one
-- Caller's own profile edit. Idempotent — overwrites existing fields.
-- The handler picks whether COALESCE-style PATCH or full overwrite
-- semantics apply; the query accepts the values to write.
INSERT INTO user_profiles (
    user_ref, display_name, bio, avatar_url, location, website_url,
    social_links, language, theme, hide_from_anonymous
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (user_ref) DO UPDATE SET
    display_name        = EXCLUDED.display_name,
    bio                 = EXCLUDED.bio,
    avatar_url          = EXCLUDED.avatar_url,
    location            = EXCLUDED.location,
    website_url         = EXCLUDED.website_url,
    social_links        = EXCLUDED.social_links,
    language            = EXCLUDED.language,
    theme               = EXCLUDED.theme,
    hide_from_anonymous = EXCLUDED.hide_from_anonymous,
    updated_at          = NOW()
RETURNING user_ref, display_name, bio, avatar_url, location,
          website_url, social_links, language, theme,
          hide_from_anonymous, origin_server_id, created_at, updated_at;

-- name: GetActorKeyMaterial :one
-- Phase 1.22.A — federation actor keypair fetch. Returns the
-- five columns added by migration 00048. Private-key columns
-- come back as their AES-256-GCM ciphertexts; the caller decrypts
-- via app/internal/atrest.Decrypt. Plain bytes never appear in
-- the SQL result row.
SELECT actor_uri,
       signing_public_key_pem,
       signing_private_key_enc,
       encryption_public_key,
       encryption_private_key_enc
FROM "user"
WHERE ref = $1;

-- name: SetActorKeyMaterial :exec
-- Phase 1.22.A — federation actor keypair install. Called once
-- per user on first federation event involving them (lazy
-- generation). Caller supplies the freshly-generated keys with
-- private keys already wrapped by atrest.Encrypt.
UPDATE "user"
SET actor_uri                  = $2,
    signing_public_key_pem     = $3,
    signing_private_key_enc    = $4,
    encryption_public_key      = $5,
    encryption_private_key_enc = $6
WHERE ref = $1;
