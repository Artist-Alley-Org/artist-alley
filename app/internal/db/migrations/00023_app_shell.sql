-- artist-alley migration 00023 — app-shell foundations.
--
-- Lays down the per-user preference + per-instance config columns
-- the app-shell phase needs:
--
--   1. user_profiles.language  — IETF BCP47 code; '' = use system /
--                                browser Accept-Language. Persisted
--                                here so the pref federates with the
--                                user (the profile is the federation
--                                unit) rather than living on the
--                                base user row.
--   2. user_profiles.theme     — 'light' | 'dark' | ''(=system).
--                                Today the theme is cookie-only; this
--                                column lets us migrate to "follows
--                                you across browsers when signed in"
--                                without breaking the cookie path.
--
-- Plus capability seeds for the new admin surfaces:
--
--   3. system.config.read / system.config.write — gate the GET /
--                                PATCH /admin/system/{site,smtp,...}
--                                endpoints. Admins already hold the
--                                `system.admin` wildcard which
--                                short-circuits these, but having
--                                explicit caps lets future roles
--                                (a "config viewer" who can see the
--                                running config but not change it)
--                                exist cleanly.
--   4. system.auth.write       — separate so a future "auth admin"
--                                can mutate auth/SSO config without
--                                seeing the AI provider API keys.
--   5. system.ai.write         — symmetric to system.auth.write.
--
-- All four caps are seeded to the Admin role on install. They are
-- effectively redundant for any caller who already holds
-- `system.admin` (which the wildcard logic in Identity.Can covers),
-- but become meaningful the moment a non-admin role gets one of
-- them.

-- +goose Up

ALTER TABLE user_profiles
    ADD COLUMN language TEXT NOT NULL DEFAULT '',
    ADD COLUMN theme    TEXT NOT NULL DEFAULT ''
        CHECK (theme IN ('', 'light', 'dark'));

-- New capabilities for the admin/system config surface.

INSERT INTO capabilities (code, description) VALUES
    ('system.config.read',  'View system configuration (site, SMTP, auth, AI providers).'),
    ('system.config.write', 'Modify system configuration.'),
    ('system.auth.write',   'Modify authentication / SSO configuration.'),
    ('system.ai.write',     'Modify AI provider configuration.')
ON CONFLICT (code) DO NOTHING;

-- Grant the new caps to the Admin role explicitly. Admins also hold
-- the system.admin wildcard, but explicit grants make the role
-- surface readable in the future admin UI.
WITH admin AS (SELECT id FROM roles WHERE name = 'Admin')
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'system.config.read'  FROM admin UNION ALL
SELECT id, 'system.config.write' FROM admin UNION ALL
SELECT id, 'system.auth.write'   FROM admin UNION ALL
SELECT id, 'system.ai.write'     FROM admin
ON CONFLICT DO NOTHING;

-- +goose Down

DELETE FROM role_capabilities WHERE capability_code IN (
    'system.config.read','system.config.write','system.auth.write','system.ai.write'
);
DELETE FROM capabilities WHERE code IN (
    'system.config.read','system.config.write','system.auth.write','system.ai.write'
);

ALTER TABLE user_profiles
    DROP COLUMN IF EXISTS theme,
    DROP COLUMN IF EXISTS language;
