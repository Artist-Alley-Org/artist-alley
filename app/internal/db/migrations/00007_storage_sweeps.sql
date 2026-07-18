-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00007_storage_sweeps.sql
--
-- Storage integrity sweeps (#403, v0.4.0 Sprint 3).
--
-- Nothing could answer "is what the DB thinks we have actually on
-- disk, and is anything on disk not in the DB?". Both directions
-- matter: a missing object is a broken asset, an unreferenced object
-- is silent waste. These two tables record what each sweep run found
-- so the admin surface can show "last sweep, N findings" and drill in.
--
-- A run is resumable: sweeps walk potentially large object sets, so
-- they persist `cursor` between batches rather than holding one
-- transaction open across the whole scan.
--
-- Findings are advisory, never authoritative. A finding records what
-- was true at scan time; the DB may have changed since. Anything that
-- acts on a finding (the orphan cleanup, landing separately) MUST
-- re-verify at action time — which is why `resolved_at` exists but no
-- "confirmed" flag does. A stale finding must never be sufficient
-- grounds to delete a live object.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS public.storage_sweep_runs (
    id              uuid PRIMARY KEY,
    kind            text NOT NULL,
    status          text NOT NULL DEFAULT 'running',
    -- Resume point: the backend key (orphan scan) or object hash
    -- (checksum verify) the next batch continues after. NULL once the
    -- run finishes.
    cursor          text,
    objects_scanned bigint NOT NULL DEFAULT 0,
    findings_count  bigint NOT NULL DEFAULT 0,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    -- Populated when status = 'failed'.
    error           text,
    triggered_by_user_ref bigint,
    CONSTRAINT storage_sweep_runs_kind_check
        CHECK (kind = ANY (ARRAY['orphan_scan'::text, 'checksum_verify'::text])),
    CONSTRAINT storage_sweep_runs_status_check
        CHECK (status = ANY (ARRAY['running'::text, 'completed'::text, 'failed'::text])),
    CONSTRAINT storage_sweep_runs_counts_check
        CHECK (objects_scanned >= 0 AND findings_count >= 0)
);
-- The admin surface's primary read is "latest run of this kind".
CREATE INDEX IF NOT EXISTS storage_sweep_runs_kind_started_idx
    ON public.storage_sweep_runs (kind, started_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS public.storage_sweep_findings (
    id           uuid PRIMARY KEY,
    run_id       uuid NOT NULL REFERENCES public.storage_sweep_runs (id) ON DELETE CASCADE,
    -- missing_object    — a storage_variants row whose bytes are gone
    -- orphan_object     — bytes on disk with no storage_variants row
    -- checksum_mismatch — bytes present but re-hash != object_hash
    -- size_mismatch     — bytes present but length != size_bytes
    finding      text NOT NULL,
    object_hash  text NOT NULL,
    variant_key  text NOT NULL,
    -- Human-readable specifics (expected vs actual), for the drill-in.
    detail       text NOT NULL DEFAULT '',
    detected_at  timestamptz NOT NULL DEFAULT now(),
    -- Set when a later action (cleanup, re-upload) settled this
    -- finding. Findings are never deleted, so the history of what was
    -- wrong and when survives.
    resolved_at  timestamptz,
    CONSTRAINT storage_sweep_findings_finding_check
        CHECK (finding = ANY (ARRAY[
            'missing_object'::text,
            'orphan_object'::text,
            'checksum_mismatch'::text,
            'size_mismatch'::text
        ]))
);
CREATE INDEX IF NOT EXISTS storage_sweep_findings_run_idx
    ON public.storage_sweep_findings (run_id, detected_at DESC);
-- Cleanup looks up "is this object still a live finding?" by subject.
CREATE INDEX IF NOT EXISTS storage_sweep_findings_subject_idx
    ON public.storage_sweep_findings (object_hash, variant_key)
    WHERE resolved_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS public.storage_sweep_findings;
DROP TABLE IF EXISTS public.storage_sweep_runs;
-- +goose StatementEnd
