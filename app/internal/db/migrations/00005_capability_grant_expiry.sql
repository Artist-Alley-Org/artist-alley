-- 00005_capability_grant_expiry.sql
--
-- Phase 1.17.C — Time-bound capability grants + revokes.
--
-- Adds expires_at to both user_capability_grants and
-- user_capability_revokes. NULL = permanent (the existing
-- behavior, which every existing row keeps). A non-NULL value
-- means the row gets reaped by the background sweeper
-- (auth/capability_sweeper.go) once NOW() passes it.
--
-- # Why both grants AND revokes
--
-- The two surfaces are symmetric — they're the additive + sub-
-- tractive overlays on a user's effective capability set. A
-- time-bound REVOKE is just as operationally meaningful as a
-- time-bound GRANT ("revoke posts.write from user X for the
-- next 24h while we investigate the incident").
--
-- # Indexes are partial
--
-- The vast majority of rows will be permanent (expires_at IS
-- NULL). Partial indexes keep the sweeper's scan cheap without
-- bloating the index for the common-case write path.
--
-- # No sysconfig seed
--
-- The userkeys sweeper (Phase 1.22.I-h precedent) hardcodes its
-- tick interval — we match that pattern for symmetry. A sysconfig
-- key for the tick interval is easy to add later if operators
-- actually want to tune it.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE user_capability_grants
    ADD COLUMN expires_at TIMESTAMPTZ;

ALTER TABLE user_capability_revokes
    ADD COLUMN expires_at TIMESTAMPTZ;

CREATE INDEX idx_user_capability_grants_expires_at
    ON user_capability_grants(expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX idx_user_capability_revokes_expires_at
    ON user_capability_revokes(expires_at)
    WHERE expires_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_user_capability_revokes_expires_at;
DROP INDEX IF EXISTS idx_user_capability_grants_expires_at;
ALTER TABLE user_capability_revokes DROP COLUMN expires_at;
ALTER TABLE user_capability_grants DROP COLUMN expires_at;

-- +goose StatementEnd
