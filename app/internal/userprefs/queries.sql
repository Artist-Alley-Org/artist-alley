-- name: GetUserPreferences :one
-- Returns the per-user preferences row for the given user ref. The
-- handler treats pgx.ErrNoRows as "user has never saved prefs" and
-- responds with the zero-value Preferences struct (system defaults
-- everywhere) rather than 404 — every authenticated caller has
-- "preferences," even if the row hasn't materialized yet.
SELECT user_ref,
       notification_channels,
       default_views,
       origin_server_id,
       created_at,
       updated_at
FROM user_preferences
WHERE user_ref = $1
LIMIT 1;

-- name: UpsertUserPreferences :exec
-- Idempotent persistence — first save creates the row, subsequent
-- saves replace both JSONB blobs and bump updated_at. The handler
-- always sends the full prefs object (PATCH semantics are applied
-- service-side via merge before this call), so the JSONB blobs here
-- are authoritative replacements.
INSERT INTO user_preferences (
    user_ref,
    notification_channels,
    default_views
)
VALUES ($1, $2, $3)
ON CONFLICT (user_ref) DO UPDATE
SET notification_channels = EXCLUDED.notification_channels,
    default_views         = EXCLUDED.default_views,
    updated_at            = NOW();
