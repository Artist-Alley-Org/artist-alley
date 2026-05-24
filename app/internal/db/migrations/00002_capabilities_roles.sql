-- artist-alley migration 00002 — capabilities, roles, per-user overrides.
--
-- Phase 1.3 authorization model (see docs/adr/0006-go-as-target-backend.md
-- and the chat-record design discussion):
--
--   * Capabilities are atomic permission codes ("system.admin",
--     "users.read", ...). One row per code in `capabilities`.
--   * Roles are named bundles of capabilities. A role can have one
--     `parent_id` (single-inheritance hierarchy); effective caps for a
--     role = own caps UNION parent's caps recursively.
--   * Each user has at most one role via `user_role` (1:1; the table
--     name is deliberately singular).
--   * Per-user overrides go in `user_capability_grants` (additive) and
--     `user_capability_revokes` (subtractive). Effective caps for a
--     user = role-chain caps UNION grants EXCEPT revokes.
--   * The "system.admin" capability is special: handlers treat it as
--     a wildcard that satisfies any check.
--
-- Federation prep (see ADR 0007):
--   * roles.origin_server_id allows future replication to mark
--     foreign-defined roles. NULL means "this server".
--   * UUIDs everywhere we own the PK.

-- +goose Up

CREATE TABLE capabilities (
    code        TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE roles (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id        UUID         NULL REFERENCES roles(id) ON DELETE SET NULL,
    name             TEXT         NOT NULL UNIQUE,
    description      TEXT         NOT NULL DEFAULT '',
    origin_server_id UUID         NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX roles__parent_idx ON roles (parent_id);

CREATE TABLE role_capabilities (
    role_id         UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    capability_code TEXT NOT NULL REFERENCES capabilities(code) ON DELETE CASCADE,
    PRIMARY KEY (role_id, capability_code)
);

-- One row per user, by design — single role per user. RS owns the
-- user.ref column so no FK from here.
CREATE TABLE user_role (
    rs_user_id             BIGINT       NOT NULL PRIMARY KEY,
    role_id                UUID         NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    assigned_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    assigned_by_rs_user_id BIGINT       NULL
);

CREATE INDEX user_role__role_idx ON user_role (role_id);

CREATE TABLE user_capability_grants (
    rs_user_id            BIGINT       NOT NULL,
    capability_code       TEXT         NOT NULL REFERENCES capabilities(code) ON DELETE CASCADE,
    granted_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    granted_by_rs_user_id BIGINT       NULL,
    note                  TEXT         NOT NULL DEFAULT '',
    PRIMARY KEY (rs_user_id, capability_code)
);

CREATE TABLE user_capability_revokes (
    rs_user_id            BIGINT       NOT NULL,
    capability_code       TEXT         NOT NULL REFERENCES capabilities(code) ON DELETE CASCADE,
    revoked_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    revoked_by_rs_user_id BIGINT       NULL,
    note                  TEXT         NOT NULL DEFAULT '',
    PRIMARY KEY (rs_user_id, capability_code)
);

-- --- Starter capabilities and roles ----------------------------------------
-- Tight starter set; more capabilities and roles added by future migrations
-- as features land that need them.

INSERT INTO capabilities (code, description) VALUES
    ('system.admin', 'Superpower — bypasses every capability check'),
    ('users.read',   'Read other users'' profiles and metadata'),
    ('users.write',  'Modify other users (role, capability grants/revokes)'),
    ('roles.read',   'List available roles and their capabilities'),
    ('caps.read',    'List defined capability codes')
ON CONFLICT (code) DO NOTHING;

-- Pre-defined roles (Base and Admin) so installs have something useful
-- to assign without an admin-UI step. Game-team studio installs will
-- typically add Artist / Art Director / etc. via the admin endpoints
-- later in Phase 1.3.x.
WITH base_role AS (
    INSERT INTO roles (name, description)
    VALUES ('Base', 'Minimal sign-in user; can read public catalogs')
    ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description
    RETURNING id
),
admin_role AS (
    INSERT INTO roles (name, description, parent_id)
    SELECT 'Admin', 'Full administrative access', id FROM base_role
    ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description
    RETURNING id
)
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'caps.read'   FROM base_role  UNION ALL
SELECT id, 'roles.read'  FROM base_role  UNION ALL
SELECT id, 'users.read'  FROM admin_role UNION ALL
SELECT id, 'users.write' FROM admin_role UNION ALL
SELECT id, 'system.admin' FROM admin_role
ON CONFLICT DO NOTHING;

-- +goose Down

DROP TABLE IF EXISTS user_capability_revokes;
DROP TABLE IF EXISTS user_capability_grants;
DROP TABLE IF EXISTS user_role;
DROP TABLE IF EXISTS role_capabilities;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS capabilities;
