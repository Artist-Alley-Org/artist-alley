-- name: FindObject :one
-- Used by the upload path to check whether a hash is already known
-- (dedup hit) before streaming bytes to the backend.
SELECT hash, size_bytes, content_type, backend, backend_bucket, gc_eligible_at
FROM storage_objects
WHERE hash = $1;

-- name: InsertObject :exec
-- Upsert form: dedup must be idempotent. If the row already exists we
-- leave its metadata untouched; the caller has either skipped the
-- backend Put (dedup hit) or just rewrote the same bytes (which is
-- fine — same hash means same content).
INSERT INTO storage_objects (
    hash, size_bytes, content_type, backend, backend_bucket, origin_server_id
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (hash) DO UPDATE SET
    gc_eligible_at = NULL;  -- a fresh upload cancels any pending GC

-- name: ClearGCEligible :exec
-- Called when a new pin reactivates a previously GC-eligible object.
UPDATE storage_objects
SET gc_eligible_at = NULL
WHERE hash = $1;

-- name: UpsertVariant :exec
-- One INSERT-or-update for the variant metadata. Variant bytes already
-- live in the backend by the time this runs.
--
-- updated_at moves on EVERY write, including a re-render of bytes that
-- were already there (#760). created_at deliberately does not: the pair
-- is what lets an operator tell "rendered once, long ago" from
-- "re-rendered just now", which a force-rebuild is otherwise unable to
-- demonstrate — the bytes change on disk and the database says nothing.
INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, metadata)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (object_hash, variant_key) DO UPDATE SET
    size_bytes   = EXCLUDED.size_bytes,
    content_type = EXCLUDED.content_type,
    metadata     = EXCLUDED.metadata,
    updated_at   = now();

-- name: GetVariant :one
SELECT object_hash, variant_key, size_bytes, content_type, metadata, created_at, updated_at
FROM storage_variants
WHERE object_hash = $1 AND variant_key = $2;

-- name: AddPin :exec
INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id)
VALUES ($1, $2, $3)
ON CONFLICT (object_hash, pin_subject_type, pin_subject_id) DO NOTHING;

-- name: RemovePin :exec
DELETE FROM storage_pins
WHERE object_hash = $1
  AND pin_subject_type = $2
  AND pin_subject_id = $3;

-- name: CountActivePins :one
SELECT COUNT(*)::BIGINT AS value
FROM storage_pins
WHERE object_hash = $1;

-- name: MarkGCEligibleIfOrphaned :exec
-- If a hash has zero remaining pins, schedule it for GC. The grace
-- period lives on storage_objects.gc_eligible_at; the sweeper deletes
-- the bytes + row after the grace expires.
UPDATE storage_objects
SET gc_eligible_at = NOW() + ($2::INTERVAL)
WHERE hash = $1
  AND NOT EXISTS (
      SELECT 1 FROM storage_pins WHERE object_hash = $1
  );

-- ---------------------------------------------------------------------
-- Admin read surface (#402, v0.4.0 Sprint 2). All aggregates, all
-- read-only, gated on system.storage.read at the HTTP layer.
--
-- Byte accounting note: storage_variants ALREADY contains one row per
-- object under variant_key='original' whose size_bytes equals
-- storage_objects.size_bytes (verified on dev: both sum to exactly
-- 2684754681). So SUM(storage_variants.size_bytes) is the complete
-- deduplicated on-disk total, and adding storage_objects to it would
-- double-count every original. storage_objects is used here only for
-- the distinct-object count and backend grouping, never for bytes.
-- ---------------------------------------------------------------------

-- name: AdminStorageTotals :one
-- One-row rollup for the usage tile: distinct objects, variant rows,
-- total bytes on disk, and the originals/derivatives split.
SELECT
    COUNT(DISTINCT object_hash)::BIGINT                                          AS object_count,
    COUNT(*)::BIGINT                                                             AS variant_count,
    COALESCE(SUM(size_bytes), 0)::BIGINT                                         AS total_bytes,
    COALESCE(SUM(size_bytes) FILTER (WHERE variant_key = 'original'), 0)::BIGINT AS original_bytes,
    COALESCE(SUM(size_bytes) FILTER (WHERE variant_key <> 'original'), 0)::BIGINT AS derivative_bytes
FROM storage_variants;

-- name: AdminStorageByFamily :many
-- Per-family rollup for the variants tile. variant_key is
-- high-cardinality (2090 distinct on dev — one key per HLS segment and
-- per turntable frame), so a raw per-key listing is unusable. The
-- family is the segment before the first '/' ('turntable/0028.png' ->
-- 'turntable'); keys without a '/' are their own family ('original',
-- 'hires'). That collapses to ~12 rows, which is the useful grain.
SELECT
    split_part(variant_key, '/', 1)::TEXT   AS family,
    COUNT(*)::BIGINT                        AS variant_count,
    COUNT(DISTINCT variant_key)::BIGINT     AS distinct_keys,
    COUNT(DISTINCT object_hash)::BIGINT     AS object_count,
    COALESCE(SUM(size_bytes), 0)::BIGINT    AS total_bytes,
    MAX(created_at)::TIMESTAMPTZ            AS newest_at
FROM storage_variants
GROUP BY 1
ORDER BY total_bytes DESC;

-- name: AdminStorageByContentType :many
-- Content-type breakdown for the usage tile (22 distinct on dev, so no
-- paging needed).
SELECT
    content_type::TEXT                      AS content_type,
    COUNT(*)::BIGINT                        AS variant_count,
    COALESCE(SUM(size_bytes), 0)::BIGINT    AS total_bytes
FROM storage_variants
GROUP BY 1
ORDER BY total_bytes DESC;

-- name: AdminStorageByBackend :many
-- Object counts per storage backend, from storage_objects (the object
-- registry). Bytes deliberately come from the variants rollup above,
-- not from here — see the accounting note.
SELECT
    backend::TEXT       AS backend,
    COUNT(*)::BIGINT    AS object_count
FROM storage_objects
GROUP BY 1
ORDER BY object_count DESC;

-- ---------------------------------------------------------------------
-- Integrity sweeps (#403). Runs + findings; see 00007 for the shape.
-- ---------------------------------------------------------------------

-- name: CreateSweepRun :one
INSERT INTO storage_sweep_runs (id, kind, triggered_by_user_ref)
VALUES ($1, $2, $3)
RETURNING id, kind, status, cursor, objects_scanned, findings_count,
          started_at, finished_at, error, triggered_by_user_ref;

-- name: AdvanceSweepRun :exec
-- Batch checkpoint: record the resume cursor + running totals without
-- closing the run, so a sweep survives a restart mid-scan.
UPDATE storage_sweep_runs
SET cursor = $2,
    objects_scanned = objects_scanned + $3,
    findings_count = findings_count + $4
WHERE id = $1;

-- name: FinishSweepRun :exec
UPDATE storage_sweep_runs
SET status = $2, error = $3, cursor = NULL, finished_at = NOW()
WHERE id = $1;

-- name: RecordSweepFinding :exec
INSERT INTO storage_sweep_findings (id, run_id, finding, object_hash, variant_key, detail)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: LatestSweepRun :one
SELECT id, kind, status, cursor, objects_scanned, findings_count,
       started_at, finished_at, error, triggered_by_user_ref
FROM storage_sweep_runs
WHERE kind = $1
ORDER BY started_at DESC
LIMIT 1;

-- name: ListSweepRuns :many
SELECT id, kind, status, cursor, objects_scanned, findings_count,
       started_at, finished_at, error, triggered_by_user_ref
FROM storage_sweep_runs
WHERE (sqlc.narg('kind')::TEXT IS NULL OR kind = sqlc.narg('kind')::TEXT)
ORDER BY started_at DESC
LIMIT $1 OFFSET $2;

-- name: ListSweepFindings :many
-- Unresolved findings for a run, newest first.
SELECT id, run_id, finding, object_hash, variant_key, detail, detected_at, resolved_at
FROM storage_sweep_findings
WHERE run_id = $1
  AND (sqlc.narg('finding')::TEXT IS NULL OR finding = sqlc.narg('finding')::TEXT)
ORDER BY detected_at DESC, id
LIMIT $2 OFFSET $3;

-- name: CountSweepFindings :one
SELECT COUNT(*)::BIGINT
FROM storage_sweep_findings
WHERE run_id = $1
  AND (sqlc.narg('finding')::TEXT IS NULL OR finding = sqlc.narg('finding')::TEXT);

-- name: ListVariantsForVerify :many
-- Paged walk of the DB side, ordered so a sweep can resume after a
-- (hash, variant) cursor.
SELECT object_hash, variant_key, size_bytes
FROM storage_variants
WHERE (object_hash, variant_key) > ($1::TEXT, $2::TEXT)
ORDER BY object_hash, variant_key
LIMIT $3;

-- name: VariantExists :one
-- Re-verification probe: is this (hash, variant) still referenced?
-- Used at ACTION time, never trusting a scan-time finding.
SELECT EXISTS (
    SELECT 1 FROM storage_variants
    WHERE object_hash = $1 AND variant_key = $2
)::BOOLEAN;

-- name: InsertVariantIfMissing :execrows
-- Heals the metadata plane's record of bytes that are demonstrably on
-- the backend (#827).
--
-- DO NOTHING, not DO UPDATE, and deliberately NOT a reuse of
-- UpsertVariant: this runs on the SKIP path of every preview job, where
-- the row is normally already correct. UpsertVariant moves updated_at on
-- every write, and updated_at is precisely what tells an operator
-- "re-rendered just now" from "rendered once, long ago" — a reconcile
-- that touched it would erase that signal on every requeue, for nothing.
--
-- The affected-row count says whether this call actually inserted, so
-- the caller can distinguish a healed split-brain row from a no-op.
INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, metadata)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (object_hash, variant_key) DO NOTHING;
