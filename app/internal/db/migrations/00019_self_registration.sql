-- Phase 1.19.C — self-service registration + email verification.
--
-- Two pieces:
--
--   1. email_verification_token — one row per outstanding token.
--      sha256 hash at-rest (plaintext only exists in the link the
--      user receives via email). Single-use; consumed by
--      /auth/verify-email. FK to "user" so cascading delete on
--      user-archive cleans up. expires_at caps the window (1h
--      default; long enough that "open the email later" works,
--      short enough that a leaked link is briefly useful).
--
--   2. user.email_verified_at — null on freshly-registered users;
--      populated by the verify endpoint. The login gate refuses
--      sessions for users with email_verified_at IS NULL ONLY
--      when sysconfig auth.self_registration.require_email_
--      verification is on; admin-created users bypass via the
--      seed path setting email_verified_at=NOW().

-- +goose Up

ALTER TABLE "user"
    ADD COLUMN email_verified_at TIMESTAMPTZ;

-- Backfill existing accounts so the new login gate doesn't
-- retroactively lock everyone out. Pre-migration users are
-- treated as verified — they got their accounts the old way
-- (admin-created or bootstrap), the new gate only applies to
-- the new /auth/register path going forward.
UPDATE "user" SET email_verified_at = NOW() WHERE email IS NOT NULL;

CREATE TABLE email_verification_token (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_ref     BIGINT NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,
    token_hash   BYTEA NOT NULL,
    purpose      TEXT NOT NULL DEFAULT 'register'
        CHECK (purpose IN ('register', 'email_change')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    UNIQUE (token_hash)
);

-- Partial index on outstanding tokens for the "list this user's
-- pending verification links" admin surface (future).
CREATE INDEX idx_email_verification_token_active
    ON email_verification_token(user_ref, expires_at)
    WHERE consumed_at IS NULL;

-- Capability gate for the admin "force-verify a user" path.
-- Seeded to Admin only — useful for the "user lost the email,
-- the relay is down, just verify them" support flow.
INSERT INTO capabilities (code, description) VALUES
    ('auth.email_verify_force',
     'Force-mark a user''s email as verified without the link click. ' ||
     'Bypass for the operator-helps-a-stuck-user path.')
ON CONFLICT (code) DO NOTHING;

INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'auth.email_verify_force' FROM roles WHERE name = 'Admin'
ON CONFLICT DO NOTHING;

-- +goose Down

DELETE FROM role_capabilities WHERE capability_code = 'auth.email_verify_force';
DELETE FROM capabilities WHERE code = 'auth.email_verify_force';
DROP TABLE IF EXISTS email_verification_token;
ALTER TABLE "user" DROP COLUMN IF EXISTS email_verified_at;
