-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00014_content_read_all_capability.sql
--
-- A content-only read capability for the binary plane (#474).
--
-- The public demo's catalogue is ~85% team/restricted sensitivity. The
-- demo-viewer role can see the rows (blurred, listed — ADR 0020) but not
-- the BYTES, so GET /assets/{id}/variants/col 404s and every tile renders
-- "Preview unavailable". The derivatives are fine; it is
-- visibility.CanReadContent (ADR 0064) gating the bytes on sensitivity.
--
-- `content.read.all` grants read access to asset content at EVERY
-- sensitivity tier — and nothing else. It is honoured at exactly one
-- place, visibility.CanReadContent, next to the system.admin wildcard.
--
-- WHY A DEDICATED CAPABILITY RATHER THAN system.admin:
-- system.admin is a wildcard that satisfies every capability check, so
-- granting it to a published demo login would expose every admin surface
-- — system config, SMTP creds, federation keys, user PII. The demo needs
-- to READ CONTENT, not administer the system. Those are different jobs;
-- this capability is the first without the second. Same reasoning as
-- system.audit.pii.read (00011): carve read access by area rather than
-- conflating it with administration.
--
-- Mirrors 00003 / 00005 / 00006 / 00011: this migration only DEFINES the
-- capability. Granting it to a role is a provisioning step, not schema —
-- resolved capability sets are cached per identity, so a grant needs a
-- cache invalidation or restart anyway. The demo-viewer grant lives in
-- the demo deploy bundle (artist-alley-demo, ADR 0060), not here.
--
-- Deliberately NOT granted to anything here, including system.admin,
-- which does not need it: system.admin is a wildcard in Identity.Can and
-- satisfies every capability check without holding a row.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.capabilities (code, description) VALUES
    ('content.read.all',
     'Read the BYTES of any asset at every sensitivity tier (public, team, restricted, embargo). Content-only: grants no admin surface and no write.')
ON CONFLICT (code) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Grants referencing this cascade (role_capabilities +
-- user_capability_grants both FK to capabilities.code ON DELETE CASCADE).
DELETE FROM public.capabilities WHERE code = 'content.read.all';
-- +goose StatementEnd
