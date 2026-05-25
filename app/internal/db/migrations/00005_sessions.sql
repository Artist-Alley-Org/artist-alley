-- artist-alley migration 00005 — first-class session table.
--
-- Replaces RS's single-session-per-user model (the user.session column,
-- which we keep writing to for PHP coexistence) with a real sessions
-- table. One row per active login; users can have many concurrent
-- sessions across devices and revoke any one of them without nuking
-- the others.
--
-- token_hash is sha256(plaintext_cookie). The plaintext only ever lives
-- in the cookie on the client, so a leaked DB snapshot cannot be replayed.
--
-- Federation prep (ADR 0007): origin_server_id is for the eventual
-- multi-site model — a peer-issued session can replicate without
-- colliding with locally-issued ones.

-- +goose Up

CREATE TABLE sessions (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_ref         BIGINT       NOT NULL,                   -- "user".ref (RS users table is not FK-friendly, no constraint)
    token_hash       BYTEA        NOT NULL UNIQUE,            -- sha256 of cookie value
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_used_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ  NULL,                       -- NULL = session expires only on idle/timeout
    revoked_at       TIMESTAMPTZ  NULL,
    ip               INET         NULL,
    user_agent       TEXT         NULL,
    origin_server_id UUID         NULL
);

CREATE INDEX sessions__user_ref_idx
    ON sessions (user_ref)
    WHERE revoked_at IS NULL;

CREATE INDEX sessions__last_used_idx
    ON sessions (last_used_at)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE  sessions IS
    'Active login sessions. One row per device/browser. token_hash is sha256(cookie_value).';
COMMENT ON COLUMN sessions.token_hash IS
    'sha256 of the plaintext cookie value. Lookups always hash first, never compare plaintext.';
COMMENT ON COLUMN sessions.expires_at IS
    'Hard cap on session lifetime. NULL means subject only to idle timeout (last_used_at).';

-- +goose Down

DROP TABLE IF EXISTS sessions;
