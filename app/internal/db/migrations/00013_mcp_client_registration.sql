-- 00013_mcp_client_registration.sql
--
-- Phase 1.53.A — Artist Alley as MCP client (foundation + ComfyUI as
-- the first integration target).
--
-- # What lands
--
--   - mcp_server_registration — one row per operator-registered MCP
--     server (e.g., "comfyui-mcp"). Holds URL + transport + auth +
--     privacy class + rate limit + health-check state.
--
--   - mcp_server_tool_grant — per-server tool whitelist with optional
--     per-tool capability gate + per-call cost estimate.
--
--   - 4 capability seeds: mcp.client.use (Base), mcp.client.admin
--     (Admin), mcp.client.images.read / mcp.client.images.write
--     (operator-assignable to roles per their privacy posture).
--
--   - 3 system_config seeds: master enable flag, default health-check
--     interval, refresh-tools-on-health-check toggle.
--
-- # Adaptations vs the brief
--
--   - `auth_kind` enum includes 'mtls' per the brief but the v1
--     code path only wires bearer/header/none. mtls reserved for a
--     follow-up; CHECK constraint already permits it so the schema
--     doesn't need a re-extension when the implementation lands.
--
--   - `additional_capability` is a single TEXT column (not a join
--     table). The brief flagged this as a signal-up if tools need
--     multiple extra caps; until that materialises, one cap per
--     tool covers operator intent. Migrating to a join table later
--     is a follow-up.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE mcp_server_registration (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                     TEXT NOT NULL UNIQUE,
    url                      TEXT NOT NULL,
    transport                TEXT NOT NULL DEFAULT 'http'
        CHECK (transport IN ('http', 'stdio')),
    auth_kind                TEXT NOT NULL DEFAULT 'none'
        CHECK (auth_kind IN ('none', 'bearer', 'header', 'mtls')),
    auth_secret_ref          TEXT,
    auth_header_name         TEXT,
    privacy_class            TEXT NOT NULL DEFAULT 'cloud'
        CHECK (privacy_class IN ('local', 'cloud')),
    enabled                  BOOLEAN NOT NULL DEFAULT FALSE,
    rate_limit_per_second    INTEGER NOT NULL DEFAULT 2
        CHECK (rate_limit_per_second > 0),
    rate_limit_per_minute    INTEGER NOT NULL DEFAULT 60
        CHECK (rate_limit_per_minute > 0),
    health_check_interval_s  INTEGER NOT NULL DEFAULT 60
        CHECK (health_check_interval_s > 0),
    last_health_check_at     TIMESTAMPTZ,
    last_health_status       TEXT
        CHECK (last_health_status IS NULL OR last_health_status IN ('healthy', 'degraded', 'unreachable')),
    last_health_error        TEXT,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    registered_by_user_ref   BIGINT
);

-- Partial index for the hot-path "which servers should the health
-- check goroutine poll" + "which are eligible candidates for the
-- dispatcher" query.
CREATE INDEX idx_mcp_server_enabled
    ON mcp_server_registration(enabled)
    WHERE enabled = TRUE;

CREATE TABLE mcp_server_tool_grant (
    server_id             UUID NOT NULL REFERENCES mcp_server_registration(id) ON DELETE CASCADE,
    tool_name             TEXT NOT NULL,
    additional_capability TEXT,
    cost_estimate_micros  BIGINT NOT NULL DEFAULT 0
        CHECK (cost_estimate_micros >= 0),
    enabled               BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (server_id, tool_name)
);

-- Capability seeds. mcp.client.use lands on Base so any signed-in
-- user can invoke a tool the operator has whitelisted; the actual
-- access gate is per-tool grants + per-server enable.
INSERT INTO capabilities (code, description) VALUES
    ('mcp.client.use',            'Invoke any whitelisted MCP tool via a registered MCP-client server'),
    ('mcp.client.admin',          'Register / configure / disable MCP-client servers'),
    ('mcp.client.images.read',    'Invoke read-only image-domain MCP tools'),
    ('mcp.client.images.write',   'Invoke image-generating MCP tools (img2img, upscale, txt2img, ...)')
ON CONFLICT (code) DO NOTHING;

INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'mcp.client.use'   FROM roles WHERE name = 'Base'
ON CONFLICT DO NOTHING;

INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'mcp.client.admin' FROM roles WHERE name = 'Admin'
ON CONFLICT DO NOTHING;

-- MCP-client config seeds.
--
--   mcp.client.enabled — master switch. Ships off; operator flips
--     when at least one server is registered + tools whitelisted.
--
--   mcp.client.default_health_interval_s — default poll cadence for
--     newly-registered servers (per-server override in the
--     registration row).
--
--   mcp.client.tool_list_refresh_on_health — when true, each health
--     check also re-pulls the server's tool list + invalidates the
--     per-server tools cache. False disables auto-refresh; operator
--     uses the "Refresh tools" button in the admin UI instead.
INSERT INTO system_config (key, value) VALUES
    ('mcp.client.enabled',                       'false'::jsonb),
    ('mcp.client.default_health_interval_s',     '60'::jsonb),
    ('mcp.client.tool_list_refresh_on_health',   'true'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM system_config WHERE key LIKE 'mcp.client.%';
DELETE FROM role_capabilities WHERE capability_code IN
    ('mcp.client.use', 'mcp.client.admin', 'mcp.client.images.read', 'mcp.client.images.write');
DELETE FROM capabilities WHERE code IN
    ('mcp.client.use', 'mcp.client.admin', 'mcp.client.images.read', 'mcp.client.images.write');
DROP TABLE IF EXISTS mcp_server_tool_grant;
DROP INDEX IF EXISTS idx_mcp_server_enabled;
DROP TABLE IF EXISTS mcp_server_registration;
-- +goose StatementEnd
