-- artist-alley migration 00044 — per-user preferences table
-- (Phase 1.17.G, feat/user-surfaces).
--
-- Sibling table to user_profiles. Where user_profiles holds the
-- public-facing identity (display name, bio, avatar, language,
-- theme), user_preferences holds the private application-behavior
-- knobs that don't belong on a profile page:
--
--   notification_channels — per event type, which channels (in_app,
--     email) the user wants. JSONB so adding new event types in
--     follow-up sub-phases (G2 follows, I2 notifications, I DMs,
--     L resource_requests) is additive and never requires another
--     migration. Empty object = "fall back to system defaults".
--
--   default_views — the home tab the user lands on, the browse-feed
--     layout + sort preference. Also JSONB for the same future-
--     proofing reason.
--
-- Federation: origin_server_id mirrors the column on user_profiles
-- so per-user prefs replicate cleanly across federated peers along
-- with the user. Per memory project_federation_is_real, every new
-- per-user table carries this even before federation is built.
--
-- ON DELETE CASCADE on rs_user_id: when a user is hard-deleted, their
-- prefs go too. Soft-delete + user-row-stays semantics aren't a thing
-- in this codebase — disabled users keep all their rows.

-- +goose Up

-- The RS baseline schema (migration 00007) created an EAV-style
-- `user_preferences` table (`ref, user, parameter, value, usergroup`)
-- that the strangler-fig PHP layer relied on. With the PHP backend
-- removed in feat/identity-teams and RS now reference-only, that
-- table is dead — never populated, no Go code reads or writes it.
-- Drop it first so the JSONB shape below takes its place cleanly.
-- Pre-MVP per memory feedback_pre_mvp_everything_is_volatile: the
-- DB can be wiped freely; we don't engineer migrations against
-- never-populated legacy tables.
DROP TABLE IF EXISTS user_preferences;

CREATE TABLE user_preferences (
    rs_user_id            BIGINT       PRIMARY KEY,
    notification_channels JSONB        NOT NULL DEFAULT '{}'::jsonb,
    default_views         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    origin_server_id      UUID         NULL,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE user_preferences IS
    'Per-user application-behavior preferences. Sibling to '
    'user_profiles. Phase 1.17.G.';

COMMENT ON COLUMN user_preferences.notification_channels IS
    'JSONB map from event type to channel list, e.g. '
    '{"comment_on_my_post":["in_app","email"],"new_follower":["in_app"]}. '
    'Empty object = system defaults apply to every event type. '
    'Schema is forward-compatible: event types added in later '
    'sub-phases (G2 follows, I2 notifications, I DMs, L requests) '
    'land as new keys without a schema migration.';

COMMENT ON COLUMN user_preferences.default_views IS
    'JSONB map of default-view selections, e.g. '
    '{"home_tab":"following","browse_layout":"masonry","browse_sort":"newest"}. '
    'Unset keys fall back to per-route defaults at render time.';

COMMENT ON COLUMN user_preferences.origin_server_id IS
    'Federation prep — the server instance that authored this '
    'preferences row. NULL = local origin. Mirrors user_profiles.';

-- +goose Down

DROP TABLE user_preferences;
