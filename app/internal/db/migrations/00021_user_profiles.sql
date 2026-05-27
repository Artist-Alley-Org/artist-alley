-- artist-alley migration 00021 — user_profiles (display layer over RS user).
--
-- The RS "user" table is wide and password-bearing; we don't want to
-- pollute it with display-layer fields. user_profiles sits alongside,
-- keyed by rs_user_id, holding only the data the frontend renders on
-- user-facing surfaces (post sidebar, profile page, /@username, etc.).
--
-- Why a separate table:
--   * RS owns the user table; we don't want migrations fighting with
--     RS's CheckDBStruct().
--   * Federation: the profile is what gets mirrored to peer sites
--     (display_name, bio, avatar_url). The password hash, session
--     token, etc. never leave the home server.
--   * Editable independently — user can update their bio without
--     touching anything that affects auth.
--
-- All columns are nullable / defaulted. A user without a profile row
-- still renders fine — the GET /users/{ref} handler returns the join,
-- substituting defaults when the row is absent.

-- +goose Up

CREATE TABLE user_profiles (
    rs_user_id       BIGINT       PRIMARY KEY,
    -- Optional display name. Falls back to user.fullname, then user.username
    -- in the GET handler's resolution. Lets people set "kthx" without
    -- changing their legal name on the user row.
    display_name     TEXT         NULL,
    bio              TEXT         NOT NULL DEFAULT '',
    -- Local URL into our own static-image storage (storage_objects hash
    -- with /assets/{id}/file shape), or a federated remote URL. The
    -- frontend renders a generated default (colored initials disc) when
    -- this is NULL.
    avatar_url       TEXT         NULL,
    location         TEXT         NOT NULL DEFAULT '',
    website_url      TEXT         NULL,
    -- Free-form social links keyed by platform: {"twitter":"@x", ...}.
    -- Frontend renders each as an icon link; unknown keys are dropped
    -- on render.
    social_links     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    -- Federation prep (ADR 0007).
    origin_server_id UUID         NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX user_profiles_origin_idx
    ON user_profiles (origin_server_id)
    WHERE origin_server_id IS NOT NULL;

-- A capability for editing OTHER users' profiles (moderator edit, e.g.
-- to remove inappropriate bio content). Editing your own profile is
-- always allowed — the handler checks ownership rather than a cap.
INSERT INTO capabilities (code, description) VALUES
    ('users.profile.edit.any', 'Edit any user''s profile (moderator)')
ON CONFLICT (code) DO NOTHING;

WITH admin AS (SELECT id FROM roles WHERE name = 'Admin')
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'users.profile.edit.any' FROM admin
ON CONFLICT DO NOTHING;

-- +goose Down

DELETE FROM role_capabilities WHERE capability_code = 'users.profile.edit.any';
DELETE FROM capabilities      WHERE code            = 'users.profile.edit.any';

DROP TABLE IF EXISTS user_profiles;
