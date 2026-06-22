-- 00015_metadata_extraction.sql
--
-- Phase 1.18.A-2 — upload-time image metadata extraction
-- (EXIF + ICC + orientation).
--
-- # What lands
--
--   - field_definition.extraction_source + .extraction_mode —
--     per-field operator config that wires a field to be
--     populated from extracted file metadata (EXIF tag name)
--     with a write mode (skip_if_set | replace | append | prepend).
--   - extraction_failure — admin review queue. One row per
--     (asset, field) that failed validation or extraction, with
--     the raw value + a human-readable error for the admin to
--     act on.
--   - metadata_backfill_run — progress + audit of operator-
--     triggered backfill runs (scope, total, processed,
--     succeeded, failed, started_at, completed_at, who started).
--   - system_config seeds for upload.dedup_scope +
--     upload.dedup_behavior so the admin UI has somewhere to
--     read/write the dedup posture. Actual dedup enforcement
--     lands in migration 00016 (commit 7) when the unique
--     constraint goes on the asset table.
--
-- # Append-only intent (ADR 0046)
--
-- Pre-v1 the schema can still evolve, but write this as if
-- append-only-already. No destructive edits; new columns default
-- so existing rows stay valid.

-- +goose Up
-- +goose StatementBegin

-- Per-field extraction config. Existing field_definition rows
-- get extraction_source='' (operator-managed; no extraction)
-- which preserves current behaviour.
ALTER TABLE field_definition
    ADD COLUMN extraction_source TEXT NOT NULL DEFAULT '',
    ADD COLUMN extraction_mode   TEXT NOT NULL DEFAULT 'skip_if_set'
        CHECK (extraction_mode IN ('skip_if_set', 'replace', 'append', 'prepend'));

-- Index for the "list all extraction-wired fields" query the
-- ExtractionConfig cache loads at startup + on field-def writes.
CREATE INDEX idx_field_definition_extraction_source
    ON field_definition(extraction_source)
    WHERE extraction_source != '';

CREATE TABLE extraction_failure (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id      UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    format        TEXT NOT NULL,
    error_kind    TEXT NOT NULL
        CHECK (error_kind IN ('unsupported_format', 'malformed_file',
                              'library_panic', 'validation', 'no_metadata')),
    message       TEXT NOT NULL,
    field_key     TEXT NOT NULL DEFAULT '',
    raw_value     JSONB NOT NULL DEFAULT '{}',
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    dismissed_at  TIMESTAMPTZ
);

CREATE INDEX idx_extraction_failure_pending
    ON extraction_failure(occurred_at DESC)
    WHERE dismissed_at IS NULL;

CREATE INDEX idx_extraction_failure_asset
    ON extraction_failure(asset_id);

CREATE TABLE metadata_backfill_run (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope                 JSONB NOT NULL DEFAULT '{}',
    total                 BIGINT NOT NULL DEFAULT 0,
    processed             BIGINT NOT NULL DEFAULT 0,
    succeeded             BIGINT NOT NULL DEFAULT 0,
    failed                BIGINT NOT NULL DEFAULT 0,
    started_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at          TIMESTAMPTZ,
    cancelled_at          TIMESTAMPTZ,
    started_by_user_ref   BIGINT REFERENCES "user"(ref) ON DELETE SET NULL
);

CREATE INDEX idx_metadata_backfill_run_active
    ON metadata_backfill_run(started_at DESC)
    WHERE completed_at IS NULL AND cancelled_at IS NULL;

-- system_config seeds — these are READ by the upload handler
-- (commit 7) + WRITTEN by the admin UI (commit 9). Defaults are
-- "per_user / warn" — the safest pair (operators get visibility
-- into duplicate uploads; existing behaviour preserved for
-- multi-user uploads).
INSERT INTO system_config (key, value) VALUES
    ('upload.dedup_scope',    '"per_user"'::jsonb),
    ('upload.dedup_behavior', '"warn"'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM system_config WHERE key IN ('upload.dedup_scope', 'upload.dedup_behavior');
DROP TABLE IF EXISTS metadata_backfill_run;
DROP TABLE IF EXISTS extraction_failure;
DROP INDEX IF EXISTS idx_field_definition_extraction_source;
ALTER TABLE field_definition
    DROP COLUMN IF EXISTS extraction_mode,
    DROP COLUMN IF EXISTS extraction_source;

-- +goose StatementEnd
