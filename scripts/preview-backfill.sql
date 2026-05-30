-- scripts/preview-backfill.sql
--
-- One-shot backfill: enqueue preview.raster jobs for every existing
-- image asset that doesn't already have its variants generated.
--
-- Safe to re-run — the worker is idempotent on (hash, variant_key)
-- so reprocessing rows whose variants already exist is nearly free.
--
-- Usage:
--   cat scripts/preview-backfill.sql | \
--     docker compose exec -T postgres psql -U artist_alley -d artist_alley

BEGIN;

-- Flip in-flight rows back to pending so the worker picks them up.
-- (Most should already be 'ready' or 'pending' from before this phase.)
UPDATE assets
   SET processing_status = 'pending',
       processing_error  = NULL,
       processing_started_at = NULL,
       processing_finished_at = NULL
 WHERE deleted_at IS NULL
   AND file_hash IS NOT NULL
   AND file_extension IS NOT NULL
   AND lower(file_extension) IN ('jpg','jpeg','png','gif','webp','bmp','tif','tiff');

-- Enqueue one preview.raster job per asset. priority=500 (PriorityBackfill)
-- so user-triggered uploads still cut to the front of the queue.
-- ON CONFLICT is irrelevant — jobs has no unique constraint we'd hit;
-- if a row is re-enqueued the worker will just process it again and
-- the variant-exists check makes the second pass a no-op.
INSERT INTO jobs (type, payload, priority, max_attempts)
SELECT
    'preview.raster',
    jsonb_build_object(
      'asset_id',       a.id::text,
      'file_hash',      a.file_hash,
      'file_extension', a.file_extension
    ),
    500,
    3
  FROM assets a
 WHERE a.deleted_at IS NULL
   AND a.file_hash IS NOT NULL
   AND a.file_extension IS NOT NULL
   AND lower(a.file_extension) IN ('jpg','jpeg','png','gif','webp','bmp','tif','tiff');

COMMIT;

-- Summary
SELECT 'pending_assets'   AS metric, COUNT(*) FROM assets WHERE processing_status = 'pending';
SELECT 'queued_jobs'      AS metric, COUNT(*) FROM jobs   WHERE status = 'pending' AND type = 'preview.raster';
