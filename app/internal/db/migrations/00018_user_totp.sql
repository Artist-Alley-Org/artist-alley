-- Phase 1.19.B — self-service TOTP (RFC 6238) two-factor auth.
--
-- Two tables:
--
--   user_totp           — one row per user that has enrolled. Holds
--                         the shared HMAC secret (at-rest wrapped via
--                         the atrest package). `confirmed_at` is NULL
--                         while the user is in the enroll-but-not-yet-
--                         proven window; populated when they submit a
--                         valid code on /account/security/2fa/confirm.
--                         Only confirmed rows gate the login flow.
--
--   user_totp_recovery_code  — per-user backup codes for the "phone
--                         lost" path. Each row is the sha256 hash of
--                         a randomly-generated 10-char base32 code;
--                         the plaintext is shown to the user ONCE at
--                         confirm-time and never reachable again.
--                         used_at marks single-use; unused codes can
--                         be regenerated wholesale via the
--                         /account/security/2fa/recovery/regenerate
--                         endpoint (invalidates the whole prior set).
--
-- Login flow integration lands separately (Phase 1.19.B commit 3):
-- the password handler checks for a confirmed_at row and either lets
-- login through or returns a 2fa_required error that the frontend
-- re-prompts on.

-- +goose Up

CREATE TABLE user_totp (
    user_ref       BIGINT PRIMARY KEY REFERENCES "user"(ref) ON DELETE CASCADE,
    secret_enc     BYTEA NOT NULL,
    confirmed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at   TIMESTAMPTZ
);

CREATE INDEX idx_user_totp_confirmed
    ON user_totp(user_ref)
    WHERE confirmed_at IS NOT NULL;

CREATE TABLE user_totp_recovery_code (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_ref    BIGINT NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,
    code_hash   BYTEA NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (code_hash)
);

CREATE INDEX idx_user_totp_recovery_active
    ON user_totp_recovery_code(user_ref)
    WHERE used_at IS NULL;

-- +goose Down

DROP TABLE IF EXISTS user_totp_recovery_code;
DROP TABLE IF EXISTS user_totp;
