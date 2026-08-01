-- Phase 1.18.A-2 — metadata extraction failure + backfill-run queries.
-- The actual extraction config (per-field-definition) lives on the
-- field_definition table via the .extraction_source +
-- .extraction_mode columns added by migration 00015; reads of that
-- column set go through metadata/queries.sql (the operator's
-- custom-field package). This file owns the extraction-failure
-- queue + backfill-run progress.

-- name: InsertExtractionFailure :one
-- Records a single failure for the admin review queue. Caller
-- builds the message + raw_value per the FailureRecord shape.
INSERT INTO extraction_failure (
    asset_id, format, error_kind, message, field_key, raw_value
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, asset_id, format, error_kind, message, field_key,
          raw_value, occurred_at, dismissed_at;

-- name: ListPendingExtractionFailures :many
-- Admin-review queue: newest pending failures first. Pagination
-- via LIMIT/OFFSET — the admin badge counts (via FailureCountCache)
-- give the operator an upper-bound estimate for navigation.
SELECT id, asset_id, format, error_kind, message, field_key,
       raw_value, occurred_at, dismissed_at
  FROM extraction_failure
 WHERE dismissed_at IS NULL
   AND (sqlc.narg('error_kind')::TEXT IS NULL OR error_kind = sqlc.narg('error_kind')::TEXT)
   AND (sqlc.narg('format')::TEXT IS NULL OR format = sqlc.narg('format')::TEXT)
 ORDER BY occurred_at DESC
 LIMIT $1 OFFSET $2;

-- name: CountPendingExtractionFailures :one
-- Powers the admin nav badge + the FailureCountCache value.
SELECT COUNT(*) FROM extraction_failure WHERE dismissed_at IS NULL;

-- name: DismissExtractionFailure :exec
-- Soft-dismiss — keeps the row for audit. Called from the admin
-- bulk-dismiss action + the "re-extract succeeded" path.
UPDATE extraction_failure
   SET dismissed_at = NOW()
 WHERE id = $1
   AND dismissed_at IS NULL;

-- name: DismissExtractionFailuresForAsset :exec
-- Used by the re-extract path: if the new extract succeeds for a
-- given asset, dismiss every prior failure on that asset so the
-- queue doesn't carry stale rows.
UPDATE extraction_failure
   SET dismissed_at = NOW()
 WHERE asset_id = $1
   AND dismissed_at IS NULL;

-- name: InsertMetadataBackfillRun :one
INSERT INTO metadata_backfill_run (
    scope, total, started_by_user_ref
) VALUES ($1, $2, $3)
RETURNING id, scope, total, processed, succeeded, failed,
          started_at, completed_at, cancelled_at, started_by_user_ref;

-- name: UpdateMetadataBackfillRunProgress :exec
-- Called once per batch by the backfill job worker.
UPDATE metadata_backfill_run
   SET processed = sqlc.arg('processed')::BIGINT,
       succeeded = sqlc.arg('succeeded')::BIGINT,
       failed    = sqlc.arg('failed')::BIGINT
 WHERE id = sqlc.arg('id');

-- name: CompleteMetadataBackfillRun :exec
UPDATE metadata_backfill_run
   SET completed_at = NOW(),
       processed    = sqlc.arg('processed')::BIGINT,
       succeeded    = sqlc.arg('succeeded')::BIGINT,
       failed       = sqlc.arg('failed')::BIGINT
 WHERE id = sqlc.arg('id');

-- name: CancelMetadataBackfillRun :exec
UPDATE metadata_backfill_run
   SET cancelled_at = NOW()
 WHERE id = $1
   AND completed_at IS NULL
   AND cancelled_at IS NULL;

-- name: GetMetadataBackfillRun :one
SELECT id, scope, total, processed, succeeded, failed,
       started_at, completed_at, cancelled_at, started_by_user_ref
  FROM metadata_backfill_run
 WHERE id = $1;

-- name: ListRecentMetadataBackfillRuns :many
SELECT id, scope, total, processed, succeeded, failed,
       started_at, completed_at, cancelled_at, started_by_user_ref
  FROM metadata_backfill_run
 ORDER BY started_at DESC
 LIMIT $1;

-- name: ListExtractionEnabledFieldDefinitions :many
-- Powers the ExtractionConfig cache. Returns every field
-- definition that's wired to an extraction source. Read on every
-- extract job (via the cache); invalidated on field-def writes.
--
-- type + options are part of the config, not decoration: the applier
-- refuses a type it has no column for (multi_select) and resolves a
-- select / tree value against options before writing the slug.
SELECT id, extraction_source, extraction_mode, type, options
  FROM field_definition
 WHERE extraction_source != '';
