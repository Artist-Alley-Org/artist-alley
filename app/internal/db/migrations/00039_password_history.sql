-- artist-alley migration 00039 — password history + reset cap.
--
-- Phase 1.17.D — self-service password change + admin force-reset.
--
-- `user_password_history` records every hash the user has ever held so
-- the policy can reject reuse of the last N passwords. RS doesn't ship
-- this; we add it because reuse-prevention is table stakes for modern
-- security auditors and we want it from day one rather than retro-
-- fitting later.
--
-- Federation: origin_server_id NULL = home install; a federated user's
-- history is owned by their origin server (we won't replicate password
-- bytes — only metadata).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS user_password_history (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rs_user_id       BIGINT NOT NULL,
    password_hash    VARCHAR(255) NOT NULL,
    changed_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    origin_server_id UUID NULL
);

-- Fast lookup: "show me this user's last N hashes" for reuse-check.
CREATE INDEX IF NOT EXISTS user_password_history_user_changed_idx
    ON user_password_history (rs_user_id, changed_at DESC);

-- Separate cap so a "Helpdesk" role can issue temp passwords without
-- inheriting users.write (role assignments, grant/revoke). Admin
-- already has system.admin wildcard.
INSERT INTO capabilities (code, description)
VALUES ('users.password.reset', 'Issue a one-shot password reset for any user (admin helpdesk action)')
ON CONFLICT (code) DO NOTHING;

INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'users.password.reset' FROM roles WHERE name = 'admin'
ON CONFLICT DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM role_capabilities WHERE capability_code = 'users.password.reset';
DELETE FROM capabilities      WHERE code            = 'users.password.reset';
DROP INDEX IF EXISTS user_password_history_user_changed_idx;
DROP TABLE IF EXISTS user_password_history;
-- +goose StatementEnd
