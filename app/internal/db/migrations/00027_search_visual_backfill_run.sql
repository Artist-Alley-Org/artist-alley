-- Phase 1.16.B-3-followup-4 — CLIP visual-embedding backfill run history.
--
-- One row per operator-triggered visual-embedding backfill sweep.
-- Mirrors the 1.16.B-5 search_reindex_run shape so the admin UI +
-- coordinator patterns match: scope stored as JSONB, cancellation
-- via cancelled_at, per-batch progress via processed/succeeded/
-- failed counters.
--
-- Divergence from search_reindex_run: no `target` column. Visual
-- backfill has a single mode (fetch image bytes → sidecar embed →
-- UpsertAssetVisualEmbedding). Consolidation into a shared
-- BackfillRun table is tracked by #186 (AdminJobBackfillPage).
--
-- Only ONE row can be IN_PROGRESS at a time (partial unique index
-- on the "in-progress" filter — coordinator refuses to start a
-- second run while one is active).
--
-- Never federates: visual embeddings are per-instance (no
-- origin_server_id column, mirroring asset_visual_embedding).

-- +goose Up

-- +goose StatementBegin
CREATE TABLE search_visual_backfill_run (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    scope               JSONB NOT NULL DEFAULT '{}'::jsonb,
    total_estimated     BIGINT,
    processed           BIGINT NOT NULL DEFAULT 0,
    succeeded           BIGINT NOT NULL DEFAULT 0,
    failed              BIGINT NOT NULL DEFAULT 0,
    started_by_user_ref BIGINT REFERENCES "user"(ref),
    last_error          TEXT
);

-- Recent-first listing for the admin history table.
CREATE INDEX search_visual_backfill_run_started_idx
    ON search_visual_backfill_run (started_at DESC);

-- Only one in-progress run at a time. In-progress = completed_at
-- IS NULL AND cancelled_at IS NULL. Second admin trigger runs
-- into 23505 uniqueness violation → handler maps to 409.
CREATE UNIQUE INDEX search_visual_backfill_run_active_uniq
    ON search_visual_backfill_run ((TRUE))
    WHERE completed_at IS NULL AND cancelled_at IS NULL;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS search_visual_backfill_run_active_uniq;
DROP INDEX IF EXISTS search_visual_backfill_run_started_idx;
DROP TABLE IF EXISTS search_visual_backfill_run;
-- +goose StatementEnd
