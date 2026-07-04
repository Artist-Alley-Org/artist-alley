-- Phase 1.19.D — per-username account lockout.
--
-- Adds persistent state on the "user" row so an account can be locked
-- after N consecutive failed login attempts. Composes with the
-- existing in-process LoginLimiter (app/internal/auth/ratelimit.go)
-- rather than replacing it: rate limiter is memory-only + short-term
-- (survives ~a minute of burst); lockout is DB-backed + durable
-- across process restarts + IP rotation.
--
-- Anti-enumeration is the whole point of this layer. Locked users
-- receive the same 401 response shape as wrong-password callers so
-- attackers rotating IPs against a probed username set can't tell
-- "account exists + is locked" from "wrong credentials". The
-- LockoutLimiter middleware in Go enforces the response shape;
-- this migration just captures the state.
--
-- Auto-clear is READ-TIME (lockout_until > NOW() at query time),
-- not sweeper-driven. Stale lockout_until values stay in the row
-- until overwritten by the next failed login OR admin unlock.
-- Cheaper than a sweeper AND avoids sweeper-vs-auth race.

-- +goose Up

ALTER TABLE "user"
    ADD COLUMN failed_login_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN lockout_until TIMESTAMPTZ NULL;

-- Partial index only on rows currently under active lockout —
-- the /admin/iiif/health gauge "N users currently locked" hits
-- this index; the vast majority of user rows have lockout_until
-- NULL so a full index is wasted space.
CREATE INDEX idx_users_lockout_active
    ON "user"(lockout_until)
    WHERE lockout_until IS NOT NULL;

-- Phase 1.19.D — auth.unlock capability. Held by admins for the
-- manual-unlock path when a legitimate user gets stuck (typo
-- cascades, forgot password + retried too many times). Same
-- admin-only seed pattern as auth.impersonate (migration 00017).
INSERT INTO capabilities (code, description) VALUES
    ('auth.unlock',
     'Manually clear a user''s failed_login_count + lockout_until. ' ||
     'Every unlock is audited with actor=admin.user_ref + subject=' ||
     'target.user_ref. Admin-only by seed; ad-hoc per-user grants go ' ||
     'through user_capability_grants.')
ON CONFLICT (code) DO NOTHING;

-- Admin-only seed. NO other role gets auth.unlock by default.
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'auth.unlock' FROM roles WHERE name = 'Admin'
ON CONFLICT DO NOTHING;

-- +goose Down

DELETE FROM role_capabilities WHERE capability_code = 'auth.unlock';
DELETE FROM capabilities WHERE code = 'auth.unlock';
DROP INDEX IF EXISTS idx_users_lockout_active;
ALTER TABLE "user"
    DROP COLUMN IF EXISTS lockout_until,
    DROP COLUMN IF EXISTS failed_login_count;
