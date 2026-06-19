-- 00007_self_edit_gates.sql
--
-- Phase 1.17.F — Per-field self-edit gates + profile.update_self
-- capability. Final sub-phase of the Phase 1.17 Identity & teams
-- arc; closes issue #20.
--
-- Operators occasionally need to lock down specific profile
-- fields (e.g., "users can't edit their display name themselves
-- because we map it from HR"). Before 1.17.F that required
-- editing the UpdateUserProfile handler in code. This migration
-- seeds five operator-tunable gates as system_config rows.
--
-- # Why five separate keys, not one JSON blob
--
-- Operator-readable in the admin UI; cache key per field; simpler
-- get/set. The brief locked this decision explicitly — diverges
-- from the site/smtp/auth/ai pattern (each of those is a single
-- typed struct in JSONB), but the gates are a flat boolean set
-- that doesn't benefit from struct grouping.
--
-- # Default: editable (true)
--
-- Preserves current behavior. Operators explicitly opt OUT per
-- field; no opt-in for already-working fields. Aligns with the
-- "artists want minimal input" principle — don't introduce
-- friction for the common case.
--
-- # profile.update_self capability
--
-- Adds the explicit per-user gate. UpdateUserProfile previously
-- used owner-of-session + the users.profile.edit.any escape
-- hatch (lines 367 of handler.go). The new capability formalises
-- "you may edit your OWN profile" so an operator can revoke it
-- per-user (e.g., disciplinary lock-out) without changing the
-- handler's auth model.
--
-- Seeded for the Base role (the audit confirmed "Base" is the
-- exact name) so every existing default user keeps the ability
-- by default. Audit also confirmed capabilities table is
-- (code, description, created_at, required_license_feature) —
-- no deprecated_at column.

-- +goose Up
-- +goose StatementBegin

INSERT INTO system_config (key, value) VALUES
    ('users.allow_self_edit.display_name', 'true'::jsonb),
    ('users.allow_self_edit.bio',          'true'::jsonb),
    ('users.allow_self_edit.avatar_url',   'true'::jsonb),
    ('users.allow_self_edit.location',     'true'::jsonb),
    ('users.allow_self_edit.website_url',  'true'::jsonb)
ON CONFLICT (key) DO NOTHING;

INSERT INTO capabilities (code, description) VALUES
    ('profile.update_self', 'Edit own profile (self-service)')
ON CONFLICT (code) DO NOTHING;

INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'profile.update_self'
FROM roles WHERE name = 'Base'
ON CONFLICT (role_id, capability_code) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM role_capabilities WHERE capability_code = 'profile.update_self';
DELETE FROM capabilities WHERE code = 'profile.update_self';
DELETE FROM system_config WHERE key LIKE 'users.allow_self_edit.%';

-- +goose StatementEnd
