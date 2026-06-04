-- artist-alley migration 00042 — capability ↔ license feature bridge.
--
-- Phase 1.17.O-2. Adds the column auth.Identity.Can() consults to
-- license-gate per-user RBAC checks:
--
--   - capabilities.required_license_feature (TEXT NULL) names the
--     license feature an install must hold for the cap to grant.
--   - NULL = no license dependency (every install can use the cap
--     if the user has it via their roles + grants).
--   - Non-null = Identity.Can() returns false UNLESS the active
--     license includes the named feature, regardless of RBAC.
--     This applies even to system.admin: SuperAdmin is about USER
--     authorisation; license features are about INSTALL
--     authorisation. A SuperAdmin on a Pro install cannot invoke
--     enterprise-only caps (SSO config, multi-tenant, audit
--     export, etc.) — those are sold features the install hasn't
--     paid for.
--
-- We deliberately do NOT seed any rows yet. The values land when
-- the actual enterprise caps are introduced (LDAP setup → sso_ldap,
-- SAML setup → sso_saml, multi-tenant teams → multi_tenant, etc.).
-- This migration just locks in the column shape so adding those
-- caps later is additive, not migration-breaking.

-- +goose Up

ALTER TABLE capabilities
    ADD COLUMN required_license_feature TEXT NULL;

COMMENT ON COLUMN capabilities.required_license_feature IS
    'License feature flag (e.g. sso_ldap, multi_tenant) the install '
    'must hold for Identity.Can() to grant this cap. NULL = no '
    'license dependency.';

-- +goose Down

ALTER TABLE capabilities
    DROP COLUMN required_license_feature;
