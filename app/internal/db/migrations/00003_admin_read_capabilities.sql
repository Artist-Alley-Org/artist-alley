-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00003_admin_read_capabilities.sql
--
-- Read capabilities for the admin surfaces that were gated only on
-- `system.admin` (GitHub #356, admin menu unlock Tier 4).
--
-- `system.admin` wildcards every capability check in the app, so six
-- admin surfaces were all-or-nothing: you either handed someone the
-- keys to everything or they saw nothing. There was no way to build a
-- read-only auditor role. These codes gate the GET/list handlers only —
-- every write on those surfaces still requires `system.admin` (or, for
-- request decisions, `share.grant`).
--
-- Naming follows the existing catalogue: `system.*.read` for
-- system-scoped surfaces (cf. system.audit.read, system.config.read,
-- system.tenancy.read) and `<domain>.read` for domain surfaces
-- (cf. users.read, roles.read, teams.read, caps.read).
--
-- `share.grant` is seeded here too. The requests approver gate has read
-- `Identity.Can("share.grant", InTeam(teamID))` since 1.20.C, but the
-- code was never in the catalogue — so the fallback was dead and the
-- surface was effectively system.admin-only. Seeding it makes the gate
-- the code always intended actually reachable, and keeps request
-- decisions out of reach of a read-only role (which holds only *.read).
--
-- No grants here: this seeds the catalogue only. Handing these to a role
-- is an operator/deployment concern (the demo's provision-demo.sql), and
-- resolved capability sets are cached per identity — a grant needs a
-- cache invalidation (or a restart) to take effect.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.capabilities (code, description) VALUES
    ('system.activities.read',
     'Read the admin activity feed'),
    ('featured.read',
     'Read the admin featured-content curation list'),
    ('system.license.read',
     'Read the installation license status'),
    ('system.metadata_extraction.read',
     'Read metadata-extraction failures and backfill runs'),
    ('federation.read',
     'Read federation peers, directories, shares, suggestions, and key health'),
    ('requests.read',
     'Read the admin asset-access request queue'),
    ('share.grant',
     'Approve or deny asset-access requests (scoped per team)')
ON CONFLICT (code) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Grants referencing these cascade (role_capabilities +
-- user_capability_grants both FK to capabilities.code ON DELETE CASCADE).
DELETE FROM public.capabilities WHERE code IN (
    'system.activities.read',
    'featured.read',
    'system.license.read',
    'system.metadata_extraction.read',
    'federation.read',
    'requests.read',
    'share.grant'
);
-- +goose StatementEnd
