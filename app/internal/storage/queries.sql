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
INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, metadata)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (object_hash, variant_key) DO UPDATE SET
    size_bytes   = EXCLUDED.size_bytes,
    content_type = EXCLUDED.content_type,
    metadata     = EXCLUDED.metadata;

-- name: GetVariant :one
SELECT object_hash, variant_key, size_bytes, content_type, metadata, created_at
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
