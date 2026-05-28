-- 00024_appearance.sql — install-wide brand / typography settings.
--
-- Adds a single capability that gates writes to the appearance
-- settings under sysconfig key "appearance" (see
-- app/internal/sysconfig/appearance.go). Reads of the public
-- appearance endpoint are unauthenticated by design so the frontend
-- can pick fonts at boot.
--
-- The settings shape itself is a JSON blob in system_config rows;
-- no new table is needed.

-- +goose Up

INSERT INTO capabilities (code, description) VALUES
    ('system.appearance.write', 'Modify the per-install brand and typography settings.')
ON CONFLICT (code) DO NOTHING;

WITH admin AS (SELECT id FROM roles WHERE name = 'Admin')
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'system.appearance.write' FROM admin
ON CONFLICT DO NOTHING;

-- +goose Down

DELETE FROM role_capabilities WHERE capability_code = 'system.appearance.write';
DELETE FROM capabilities WHERE code = 'system.appearance.write';
