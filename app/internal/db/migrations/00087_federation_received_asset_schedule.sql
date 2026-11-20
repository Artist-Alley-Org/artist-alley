-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00087_federation_received_asset_schedule.sql
--
-- #1574 (inbox route and worker): a pending received asset record is the
-- durable import task. The inbox dispatcher binds an aa:AssetVersion to
-- its record in one transaction and transfers nothing; the receiver's
-- worker (assetingest.Worker) runs pending records, a bounded number at a
-- time. No separate queue row exists, so a task is written atomically
-- with its record and is bounded by the record's own uniqueness (one per
-- peer and activity, one live per peer and asset).
--
-- next_attempt_at is when a pending record may be started:
--   - now() when it is bound;
--   - a lease ahead while an attempt runs (the worker's claim, under FOR
--     UPDATE SKIP LOCKED, and its heartbeat): no other worker starts it;
--     a process that stops mid-attempt leaves it due once the lease lapses;
--   - a capped backoff ahead after a transient failure.
-- It means nothing once a record is complete or rejected; the 00086
-- update guard still refuses any change to those.

-- +goose Up

ALTER TABLE public.federation_received_assets
    ADD COLUMN next_attempt_at timestamp with time zone DEFAULT now() NOT NULL;

CREATE INDEX federation_received_assets_due_idx
    ON public.federation_received_assets USING btree (next_attempt_at, id)
    WHERE (state = 'pending'::text);

COMMENT ON COLUMN public.federation_received_assets.next_attempt_at IS 'When a pending record may be started (#1574): now() when bound, a lease ahead while an attempt runs, a capped backoff ahead after a transient failure. assetingest.Worker claims due pending records with FOR UPDATE SKIP LOCKED.';

-- +goose Down

DROP INDEX IF EXISTS public.federation_received_assets_due_idx;
ALTER TABLE public.federation_received_assets DROP COLUMN IF EXISTS next_attempt_at;
