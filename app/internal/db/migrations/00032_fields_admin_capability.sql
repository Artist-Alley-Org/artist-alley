-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00032_fields_admin_capability.sql
--
-- Make the `fields.admin` capability real and grantable (#804).
--
-- The metadata field-admin surface has always gated on
-- `id.Can("fields.admin") || id.Can("system.admin")`
-- (internal/metadata/handler.go), and the code constant CapFieldsAdmin
-- already spells it "fields.admin". But there was NO `fields.admin` row
-- in the `capabilities` table. `role_capabilities.capability_code` and
-- the per-user grant table both FK to `capabilities(code)`, so any
-- attempt to grant `fields.admin` was rejected by that FK — the
-- Can(CapFieldsAdmin) branch was permanently dead and field admin was
-- effectively `system.admin`-only, with no way to delegate it.
--
-- This adds the missing capability row (matching the baseline pattern,
-- required_license_feature NULL — no license gate) and grants it to the
-- built-in Admin role, which already carries system.config.read and the
-- rest of the operator set. After this, field admin is reachable through
-- a narrower capability than system.admin, so an operator can delegate
-- it to a non-superuser role.

-- +goose Up

INSERT INTO public.capabilities (code, description, created_at, required_license_feature)
VALUES ('fields.admin', 'Create, edit, and delete metadata field definitions and their options.', now(), NULL);

INSERT INTO public.role_capabilities (role_id, capability_code)
VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'fields.admin');

-- +goose Down

DELETE FROM public.role_capabilities
 WHERE role_id = 'aa6b632d-5bef-4924-93d4-aba070dfe503'
   AND capability_code = 'fields.admin';

DELETE FROM public.capabilities WHERE code = 'fields.admin';
