-- artist-alley migration 00024 — generic jobs table + preview pipeline.
--
-- Phase 1.18.A foundation. Three concerns in one file because they
-- co-exist as one schema unit:
--
--   1. A generic `jobs` queue table. Not preview-specific. Any
--      background workload (preview gen, checksum, reindex,
--      federation sync, etc.) lands here, keyed by `type` and with a
--      `payload` JSONB that handlers parse.
--
--   2. The first job type — preview.raster — and its sibling
--      preview.svg / .video / .audio / .pdf / .font / .3d (handlers
--      ship across follow-up phases). We don't model job types in
--      SQL; they're just strings the handler registry dispatches on.
--
--   3. A handful of `assets.processing_*` bookkeeping columns the
--      worker writes back to once variants are produced. The
--      `processing_status` column was added in migration 00022 — we
--      loosen its CHECK to admit the 'processing' state.
--
-- Worker model: rows are claimed with FOR UPDATE SKIP LOCKED so any
-- number of workers (in-process, external farm, federated peer) can
-- drain the queue without coordination. A lease (`lease_expires_at`)
-- protects against worker death — a sweeper requeues stuck rows.
--
-- Federation prep: every row carries `origin_server_id` so a future
-- federation router can decide whether to handle a job locally or
-- forward it.

-- +goose Up

CREATE TABLE jobs (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    type                TEXT         NOT NULL,
    payload             JSONB        NOT NULL DEFAULT '{}',
    -- pending  = waiting in queue
    -- running  = claimed by some worker; updates expected via heartbeat
    -- done     = finished successfully (result optional)
    -- failed   = exceeded max_attempts or fatal error
    -- cancelled= admin / federation withdrew the job
    status              TEXT         NOT NULL DEFAULT 'pending'
                                     CHECK (status IN ('pending','running','done','failed','cancelled')),
    -- Lower = sooner. Default 100; preview.raster typically 50,
    -- federation backfill 200, etc. Handlers pick their own.
    priority            INTEGER      NOT NULL DEFAULT 100,
    attempts            INTEGER      NOT NULL DEFAULT 0,
    max_attempts        INTEGER      NOT NULL DEFAULT 3,
    -- Stable identity of the worker that holds the current lease.
    -- For in-process workers we use `aa://{instance_id}/{worker_id}`.
    -- External farms supply their own (logged on claim).
    claimed_by          TEXT         NULL,
    claimed_at          TIMESTAMPTZ  NULL,
    lease_expires_at    TIMESTAMPTZ  NULL,
    -- Last failure message, kept across attempts for diagnosis.
    last_error          TEXT         NULL,
    -- Handler-defined result JSON. preview.raster writes
    -- {variants: ["col","preview","screen"], skipped: []}.
    result              JSONB        NULL,
    -- Federation: NULL = locally originated; set when a peer
    -- forwarded the job to us OR when we forwarded ours to a peer.
    origin_server_id    UUID         NULL,
    -- Optional defer point. NULL = run as soon as possible.
    scheduled_for       TIMESTAMPTZ  NULL,
    enqueued_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    started_at          TIMESTAMPTZ  NULL,
    finished_at         TIMESTAMPTZ  NULL
);

COMMENT ON TABLE jobs IS
    'Generic background-job queue. Workers claim rows via FOR UPDATE SKIP LOCKED. Handlers dispatch on `type`; `payload` shape is handler-defined.';

-- Worker pickup: covers `WHERE status='pending' AND (scheduled_for IS NULL OR scheduled_for <= NOW())`
-- ordered by priority + enqueued_at. Partial so only the live queue
-- rows are indexed.
CREATE INDEX jobs_pending_idx
    ON jobs (priority ASC, enqueued_at ASC)
    WHERE status = 'pending';

-- Sweep index: finds stuck `running` rows whose lease has expired so
-- a watchdog can requeue them.
CREATE INDEX jobs_lease_expiry_idx
    ON jobs (lease_expires_at)
    WHERE status = 'running';

-- Filter by type for handler-specific worker pools and for the
-- (future) `/jobs/claim?types=` API surface.
CREATE INDEX jobs_type_status_idx
    ON jobs (type, status);

-- ---------------------------------------------------------------------------
-- assets bookkeeping
-- ---------------------------------------------------------------------------

-- Loosen the CHECK to admit the in-flight 'processing' state. The
-- worker flips pending → processing → (ready|failed).
ALTER TABLE assets
    DROP CONSTRAINT IF EXISTS assets_processing_status_check;
ALTER TABLE assets
    ADD CONSTRAINT assets_processing_status_check
        CHECK (processing_status IN ('pending', 'processing', 'ready', 'failed'));

ALTER TABLE assets
    ADD COLUMN processing_attempts    INTEGER     NOT NULL DEFAULT 0,
    ADD COLUMN processing_error       TEXT        NULL,
    ADD COLUMN processing_started_at  TIMESTAMPTZ NULL,
    ADD COLUMN processing_finished_at TIMESTAMPTZ NULL;

COMMENT ON COLUMN assets.processing_attempts IS
    'Number of times a worker has tried to generate variants for this asset. Capped in code; rows past the cap go to status=failed.';

-- +goose Down

ALTER TABLE assets
    DROP COLUMN IF EXISTS processing_finished_at,
    DROP COLUMN IF EXISTS processing_started_at,
    DROP COLUMN IF EXISTS processing_error,
    DROP COLUMN IF EXISTS processing_attempts;

ALTER TABLE assets
    DROP CONSTRAINT IF EXISTS assets_processing_status_check;
ALTER TABLE assets
    ADD CONSTRAINT assets_processing_status_check
        CHECK (processing_status IN ('pending','ready','failed'));

DROP INDEX IF EXISTS jobs_type_status_idx;
DROP INDEX IF EXISTS jobs_lease_expiry_idx;
DROP INDEX IF EXISTS jobs_pending_idx;
DROP TABLE IF EXISTS jobs;
