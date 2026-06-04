-- artist-alley migration 00040 — per-asset-type ACLs (Phase 1.17.F-bis).
--
-- The existing post_acls / collection_acls (migration 00017) gate per-
-- INSTANCE access. This migration adds the per-TYPE counterpart: which
-- users / roles / teams can interact with an entire asset_type.
--
-- Access model: ABSENCE of any rows for an asset_type = open to every
-- caller (today's behaviour). PRESENCE of one or more rows flips the
-- type to "restricted" — only callers with a matching ACL row may
-- exercise the gated operation. Admins (system.admin) bypass.
--
-- Permission alphabet — same enum as post_acls so the principal
-- editor UI is reusable, but the semantics map to type-level
-- operations:
--
--   read   — see the type in pickers / list its assets
--   write  — upload new assets of this type
--   admin  — edit the type definition itself (name, allowed_extensions,
--            colour, icon, etc.) and manage its ACL rows
--
-- The handler-side filter for ListAssetTypes is wired in this same
-- branch. Enforcement on the asset upload + list paths is opt-in
-- via a helper exported from internal/assettype; assets / upload
-- handlers will adopt it in a follow-up commit.
--
-- Sweep triggers mirror migration 00017: role/team deletion clears
-- the corresponding ACL rows. User-principal sweep is again skipped
-- (no FK; dangling rows tolerated by the cap-check).

-- +goose Up

CREATE TABLE asset_type_acls (
    asset_type_ref        BIGINT       NOT NULL REFERENCES asset_types(ref) ON DELETE CASCADE,
    principal_type        TEXT         NOT NULL CHECK (principal_type IN ('user','role','team')),
    principal_id          TEXT         NOT NULL,
    permission            TEXT         NOT NULL CHECK (permission IN ('read','write','admin')),
    granted_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    granted_by_rs_user_id BIGINT       NULL,
    expires_at            TIMESTAMPTZ  NULL,
    PRIMARY KEY (asset_type_ref, principal_type, principal_id, permission)
);

CREATE INDEX asset_type_acls_principal_idx
    ON asset_type_acls (principal_type, principal_id);

CREATE INDEX asset_type_acls_expires_idx
    ON asset_type_acls (expires_at)
    WHERE expires_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Principal-sweep triggers — same pattern as migration 00017.
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION asset_type_acl_sweep_on_role_delete() RETURNS TRIGGER AS $$
BEGIN
    DELETE FROM asset_type_acls
     WHERE principal_type = 'role' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER asset_type_acl_sweep_after_role_delete
AFTER DELETE ON roles
FOR EACH ROW EXECUTE FUNCTION asset_type_acl_sweep_on_role_delete();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION asset_type_acl_sweep_on_team_delete() RETURNS TRIGGER AS $$
BEGIN
    DELETE FROM asset_type_acls
     WHERE principal_type = 'team' AND principal_id = OLD.id::text;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER asset_type_acl_sweep_after_team_delete
AFTER DELETE ON teams
FOR EACH ROW EXECUTE FUNCTION asset_type_acl_sweep_on_team_delete();

-- ---------------------------------------------------------------------------
-- Capabilities seed.
-- ---------------------------------------------------------------------------
-- system.asset_types.admin gates the ACL management endpoints (separate
-- from system.admin so a future "content admin" role can curate types
-- without holding the master cap). Admins (system.admin wildcard) get
-- it via the existing Identity.Can short-circuit.
INSERT INTO capabilities (code, description) VALUES
    ('system.asset_types.admin', 'Edit asset_type definitions and manage their per-type ACLs')
ON CONFLICT (code) DO NOTHING;

-- +goose Down

DROP TRIGGER IF EXISTS asset_type_acl_sweep_after_team_delete ON teams;
DROP TRIGGER IF EXISTS asset_type_acl_sweep_after_role_delete ON roles;
DROP FUNCTION IF EXISTS asset_type_acl_sweep_on_team_delete();
DROP FUNCTION IF EXISTS asset_type_acl_sweep_on_role_delete();

DROP TABLE IF EXISTS asset_type_acls;

DELETE FROM capabilities WHERE code = 'system.asset_types.admin';
