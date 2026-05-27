-- artist-alley migration 00017 — per-resource and per-collection ACLs.
--
-- See ADR 0010 Layer 6.
--
-- ACLs are ADDITIVE exceptions on top of the role/grant model. They never
-- restrict below what the resource's visibility column allows. The
-- primary access path is:
--
--   1. Owner → always read+write
--   2. Resource visibility ('public' / 'team' / 'private') → reads
--   3. Role + team scope (Layer 5) → reads/writes per capability
--   4. ACL row → reads/writes/admin per explicit grant
--
-- A request is allowed iff ANY of 1–4 grants it. The handler-level
-- check consults ACL rows after the cap-driven checks fail. Most
-- requests never touch the ACL tables.
--
-- Shape (one row per granted permission per principal):
--   * post_id / collection_id — the resource. ON DELETE CASCADE handles
--     resource deletion (no orphans).
--   * principal_type + principal_id — polymorphic principal. We use
--     text for principal_id so user (BIGINT), role (UUID), and team
--     (UUID) all fit. The cost is losing per-table FK integrity on
--     the principal side; the sweep triggers below maintain hygiene
--     when roles or teams are deleted.
--   * permission — 'read' / 'write' / 'admin'. Three rows for a
--     full-access grant; one row for read-only. Keeps the model
--     monotonic — adding a permission is one INSERT, never an UPDATE.
--   * expires_at — optional time-boxed share ("Marketing has read for
--     7 days"). NULL = permanent. The expiry check is in the handler;
--     no background sweep needed (a stale row just stops granting).
--
-- We deliberately do NOT include a UNIQUE on (resource, principal_type,
-- principal_id) because granting read AND write needs two rows. The
-- PRIMARY KEY enforces uniqueness per (resource, principal, permission)
-- which is exactly the deduplication we want.

-- +goose Up

CREATE TABLE post_acls (
    post_id               UUID         NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    principal_type        TEXT         NOT NULL CHECK (principal_type IN ('user','role','team')),
    principal_id          TEXT         NOT NULL,
    permission            TEXT         NOT NULL CHECK (permission IN ('read','write','admin')),
    granted_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    granted_by_rs_user_id BIGINT       NULL,
    expires_at            TIMESTAMPTZ  NULL,
    PRIMARY KEY (post_id, principal_type, principal_id, permission)
);

CREATE INDEX post_acls_principal_idx
    ON post_acls (principal_type, principal_id);

CREATE INDEX post_acls_expires_idx
    ON post_acls (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE TABLE collection_acls (
    collection_id         UUID         NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    principal_type        TEXT         NOT NULL CHECK (principal_type IN ('user','role','team')),
    principal_id          TEXT         NOT NULL,
    permission            TEXT         NOT NULL CHECK (permission IN ('read','write','admin')),
    granted_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    granted_by_rs_user_id BIGINT       NULL,
    expires_at            TIMESTAMPTZ  NULL,
    PRIMARY KEY (collection_id, principal_type, principal_id, permission)
);

CREATE INDEX collection_acls_principal_idx
    ON collection_acls (principal_type, principal_id);

CREATE INDEX collection_acls_expires_idx
    ON collection_acls (expires_at)
    WHERE expires_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Principal-sweep triggers
-- ---------------------------------------------------------------------------
-- When a role or team is deleted, sweep any ACL rows that still point
-- at it. We can't use ON DELETE CASCADE because the principal_id is
-- text and Postgres can't enforce a polymorphic FK.
--
-- User deletion is NOT swept here: RS owns the "user" table and we
-- can't safely attach triggers to it from our migration namespace. The
-- handler-side permission check ignores ACL rows whose principal_id
-- doesn't resolve, so dangling user-ACLs are harmless until the
-- eventual GC story arrives.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION acl_sweep_on_role_delete() RETURNS TRIGGER AS $$
BEGIN
    DELETE FROM post_acls
     WHERE principal_type = 'role' AND principal_id = OLD.id::text;
    DELETE FROM collection_acls
     WHERE principal_type = 'role' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER acl_sweep_after_role_delete
AFTER DELETE ON roles
FOR EACH ROW EXECUTE FUNCTION acl_sweep_on_role_delete();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION acl_sweep_on_team_delete() RETURNS TRIGGER AS $$
BEGIN
    DELETE FROM post_acls
     WHERE principal_type = 'team' AND principal_id = OLD.id::text;
    DELETE FROM collection_acls
     WHERE principal_type = 'team' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER acl_sweep_after_team_delete
AFTER DELETE ON teams
FOR EACH ROW EXECUTE FUNCTION acl_sweep_on_team_delete();

-- +goose Down

DROP TRIGGER IF EXISTS acl_sweep_after_team_delete ON teams;
DROP TRIGGER IF EXISTS acl_sweep_after_role_delete ON roles;
DROP FUNCTION IF EXISTS acl_sweep_on_team_delete();
DROP FUNCTION IF EXISTS acl_sweep_on_role_delete();

DROP TABLE IF EXISTS collection_acls;
DROP TABLE IF EXISTS post_acls;
