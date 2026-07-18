-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00006_storage_read_capability.sql
--
-- Read capability for the admin storage surface (#402, v0.4.0 Sprint 2).
--
-- Operators had no view of what storage actually holds: total bytes on
-- disk, how much of it is derivatives vs originals, and which variant
-- families (turntable frames, HLS segments, previews) dominate. All of
-- it was psql-only. `system.storage.read` gates a read-only view of
-- those aggregates so an auditor role can answer "what is using the
-- disk" without the `system.admin` wildcard.
--
-- Read-only by construction: no write path is exposed under this cap.
-- The mutating storage tools (orphan sweep, checksum re-verify,
-- reimport) stay `system.admin` and land in later sprints.
--
-- Mirrors 00003_admin_read_capabilities.sql / 00005_jobs_read_capability.sql:
-- this migration only DEFINES the capability. Granting it to a role is a
-- provisioning step, not schema — resolved capability sets are cached
-- per identity, so a grant needs a cache invalidation or restart anyway.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.capabilities (code, description) VALUES
    ('system.storage.read',
     'Read storage usage: object/variant counts, bytes on disk, per-family and per-content-type breakdowns')
ON CONFLICT (code) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Grants referencing this cascade (role_capabilities +
-- user_capability_grants both FK to capabilities.code ON DELETE CASCADE).
DELETE FROM public.capabilities WHERE code = 'system.storage.read';
-- +goose StatementEnd
