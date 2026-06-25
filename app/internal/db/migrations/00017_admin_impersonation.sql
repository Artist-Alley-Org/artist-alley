-- Phase 1.19.A-2 — admin impersonation.
--
-- Adds a way for admins holding `auth.impersonate` to issue
-- themselves a session bound to a target user's identity. The
-- session row remembers WHO triggered the impersonation so:
--
--   1. Audit attribution stays honest: every action under the
--      impersonation session is attributable to BOTH the
--      admin (via impersonated_by_user_ref) and the target
--      (the session's user_ref). The audit recorder picks both up.
--
--   2. UI can render a persistent "you are acting as @target"
--      banner with a one-click "end impersonation" button.
--
--   3. Defense-in-depth: server-side rejects dangerous mutations
--      while impersonating (password change, capability grant,
--      another impersonation) so the trail can't be muddied.
--
-- Capability seed: `auth.impersonate` is admin-only and CANNOT
-- be granted to any other seeded role (that's the contract; ad-
-- hoc per-user grants go through the existing user_capability_
-- grants surface — operators decide case-by-case).

-- +goose Up

ALTER TABLE sessions
    ADD COLUMN impersonated_by_user_ref BIGINT
        REFERENCES "user"(ref) ON DELETE SET NULL;

-- Partial index on impersonation sessions only — admins
-- listing "who is currently being impersonated" hits this. The
-- vast majority of session rows have impersonated_by_user_ref
-- NULL and are excluded.
CREATE INDEX idx_sessions_impersonated_by_active
    ON sessions(impersonated_by_user_ref, created_at DESC)
    WHERE impersonated_by_user_ref IS NOT NULL
      AND revoked_at IS NULL;

INSERT INTO capabilities (code, description) VALUES
    ('auth.impersonate',
     'Issue a session as another user for support / debugging. ' ||
     'Every action under impersonation is audit-attributed to BOTH ' ||
     'the acting admin and the target. Admin-only by seed; ad-hoc ' ||
     'per-user grants go through user_capability_grants.')
ON CONFLICT (code) DO NOTHING;

-- Admin-only seed. NO other role gets `auth.impersonate` by
-- default — the cap memory says "cannot be granted to any
-- seeded role except admin".
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'auth.impersonate' FROM roles WHERE name = 'Admin'
ON CONFLICT DO NOTHING;

-- +goose Down

DROP INDEX IF EXISTS idx_sessions_impersonated_by_active;
ALTER TABLE sessions DROP COLUMN IF EXISTS impersonated_by_user_ref;
DELETE FROM role_capabilities WHERE capability_code = 'auth.impersonate';
DELETE FROM capabilities WHERE code = 'auth.impersonate';
