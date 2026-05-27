-- User profile read + write queries. Owned by app/internal/users.
--
-- The "user" table is RS's; we don't write to its sensitive columns
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
SELECT u.ref                                            AS rs_user_id,
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
       p.origin_server_id                               AS profile_origin_server_id
FROM "user" u
LEFT JOIN user_profiles p ON p.rs_user_id = u.ref
WHERE u.ref = $1;

-- name: GetUserPublicByUsername :one
-- Same as above but keyed by username. Used by /@username URL pattern.
SELECT u.ref                                            AS rs_user_id,
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
       p.origin_server_id                               AS profile_origin_server_id
FROM "user" u
LEFT JOIN user_profiles p ON p.rs_user_id = u.ref
WHERE u.username = $1;

-- name: CountPostsByAuthor :one
-- Total live posts authored by this user. Used as the post_count
-- field on the public profile.
SELECT COUNT(*)::BIGINT AS value
FROM posts
WHERE author_user_ref = $1 AND deleted_at IS NULL;

-- name: UpsertUserProfile :one
-- Caller's own profile edit. Idempotent — overwrites existing fields.
-- The handler picks whether COALESCE-style PATCH or full overwrite
-- semantics apply; the query accepts the values to write.
INSERT INTO user_profiles (
    rs_user_id, display_name, bio, avatar_url, location, website_url,
    social_links, language, theme
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (rs_user_id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    bio          = EXCLUDED.bio,
    avatar_url   = EXCLUDED.avatar_url,
    location     = EXCLUDED.location,
    website_url  = EXCLUDED.website_url,
    social_links = EXCLUDED.social_links,
    language     = EXCLUDED.language,
    theme        = EXCLUDED.theme,
    updated_at   = NOW()
RETURNING rs_user_id, display_name, bio, avatar_url, location,
          website_url, social_links, language, theme,
          origin_server_id, created_at, updated_at;
