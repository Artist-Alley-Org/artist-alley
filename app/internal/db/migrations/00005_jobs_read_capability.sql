-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00005_jobs_read_capability.sql
--
-- Read capability for the admin jobs surface (#400, v0.4.0 Sprint 0).
--
-- The whole async pipeline (derivatives, previews, AI tagging,
-- federation outbox) runs on the job queue, but operators had zero
-- admin visibility — #278/#279/#292 were all diagnosed by hand against
-- psql. `system.jobs.read` gates a read-only view of the queue (jobs by
-- status/type, active workers + lease, live status counts) so an
-- auditor role can watch the pipeline without the `system.admin`
-- wildcard.
--
-- Read-only by construction: no write path is exposed under this cap.
-- Requeue / cancel / concurrency-edit stay `system.admin` and land in
-- Sprint 1 (#401).
--
-- Mirrors 00003_admin_read_capabilities.sql: this migration only DEFINES
-- the capability. Granting it to a role (demo-viewer on the demo box) is
-- a provisioning step, not schema — resolved capability sets are cached
-- per identity, so a grant needs a cache invalidation or restart anyway.

-- +goose Up
-- +goose StatementBegin
INSERT INTO public.capabilities (code, description) VALUES
    ('system.jobs.read',
     'Read the job queue: jobs by status/type, active workers, live counts')
ON CONFLICT (code) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Grants referencing this cascade (role_capabilities +
-- user_capability_grants both FK to capabilities.code ON DELETE CASCADE).
DELETE FROM public.capabilities WHERE code = 'system.jobs.read';
-- +goose StatementEnd
