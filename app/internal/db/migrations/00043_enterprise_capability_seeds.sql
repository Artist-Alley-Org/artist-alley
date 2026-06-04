-- artist-alley migration 00043 — enterprise capability seeds
-- (Phase 1.17.P-foundation).
--
-- Seeds the capability codes that gate enterprise admin surfaces,
-- each pinned to its license feature via the
-- required_license_feature column added in migration 00042. The
-- auth.Identity.Can() license-bridge consults this column BEFORE the
-- SuperAdmin shortcut, so even a SuperAdmin on a Community install
-- cannot reach these endpoints — they're install-level features the
-- customer hasn't purchased.
--
-- Cap → feature mapping:
--   system.sso.ldap.write    → sso_ldap        (LDAP/AD provider config)
--   system.sso.ldap.read     → sso_ldap        (read-only LDAP config view)
--   system.sso.saml.write    → sso_saml        (SAML IdP trust + metadata)
--   system.sso.saml.read     → sso_saml        (read-only SAML config view)
--   system.tenancy.write     → multi_tenant    (tenant CRUD, quotas)
--   system.tenancy.read      → multi_tenant    (tenant list / details)
--
-- Why split read vs write per feature: future enterprise deployments
-- often have a "compliance auditor" role that needs visibility into
-- the SSO + tenant config without being able to mutate it. Splitting
-- the cap pair up front keeps that door open without a future
-- migration that has to wedge a read variant in.
--
-- Federation is explicitly NOT here — that's free even at community
-- tier (per user direction; small communities can self-organize).

-- +goose Up

INSERT INTO capabilities (code, description, required_license_feature) VALUES
    ('system.sso.ldap.read',  'View LDAP/AD identity-provider configuration', 'sso_ldap'),
    ('system.sso.ldap.write', 'Configure LDAP/AD identity-provider connections', 'sso_ldap'),
    ('system.sso.saml.read',  'View SAML 2.0 IdP trust configuration', 'sso_saml'),
    ('system.sso.saml.write', 'Configure SAML 2.0 IdP trust + service-provider metadata', 'sso_saml'),
    ('system.tenancy.read',   'View multi-tenant deployment configuration', 'multi_tenant'),
    ('system.tenancy.write',  'Manage tenants, quotas, and per-tenant administration', 'multi_tenant')
ON CONFLICT (code) DO UPDATE
    SET description              = EXCLUDED.description,
        required_license_feature = EXCLUDED.required_license_feature;

-- +goose Down

DELETE FROM capabilities
WHERE code IN (
    'system.sso.ldap.read',
    'system.sso.ldap.write',
    'system.sso.saml.read',
    'system.sso.saml.write',
    'system.tenancy.read',
    'system.tenancy.write'
);
