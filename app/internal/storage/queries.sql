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
-- Called when a new pin reactivates a previously GC-eligible object. A
-- pin's first statement (addPinTx): the UPDATE also takes the object
-- row's lock before the pin row is inserted.
UPDATE storage_objects
SET gc_eligible_at = NULL
WHERE hash = $1;

-- name: LockVariantWrite :exec
-- #1597: the per-(hash, variant) WRITE lock, exclusive and session-level,
-- held on one connection from before a write's begin step until its
-- record step (backend Put included). Writers of one variant are
-- serialized, so a row can only ever record the digest of the bytes
-- that were Put last. A crashed writer's session ends and the lock with
-- it. Readers that need the row and the bytes to agree (the integrity
-- sweep) take it SHARED.
SELECT pg_advisory_lock(hashtextextended('storage-variant:' || sqlc.arg(object_hash)::TEXT || '/' || sqlc.arg(variant_key)::TEXT, 0));

-- name: UnlockVariantWrite :one
SELECT pg_advisory_unlock(hashtextextended('storage-variant:' || sqlc.arg(object_hash)::TEXT || '/' || sqlc.arg(variant_key)::TEXT, 0))::BOOLEAN AS value;

-- name: LockVariantRead :exec
SELECT pg_advisory_lock_shared(hashtextextended('storage-variant:' || sqlc.arg(object_hash)::TEXT || '/' || sqlc.arg(variant_key)::TEXT, 0));

-- name: UnlockVariantRead :one
SELECT pg_advisory_unlock_shared(hashtextextended('storage-variant:' || sqlc.arg(object_hash)::TEXT || '/' || sqlc.arg(variant_key)::TEXT, 0))::BOOLEAN AS value;

-- name: BeginVariantWrite :one
-- #1597, under LockVariantWrite, COMMITTED before the backend Put: the
-- existing row (if any) stops claiming any digest and moves to a new
-- generation. If the writer dies before its record step, the row says
-- unverifiable, never the digest of bytes that may already be replaced.
-- No row: pgx.ErrNoRows, and the record step inserts generation 1.
-- updated_at is left to the record step: it means "re-rendered" (#760).
UPDATE storage_variants SET
    content_sha256     = NULL,
    content_size_bytes = NULL,
    digest_source      = NULL,
    fixity_status      = 'unverifiable',
    fixity_checked_at  = NULL,
    generation         = generation + 1
WHERE object_hash = sqlc.arg(object_hash) AND variant_key = sqlc.arg(variant_key)
RETURNING generation;

-- name: RecordVariantWrite :execrows
-- #1597, under LockVariantWrite, after the backend Put: the digest and
-- size the server computed from the bytes it streamed, for the generation
-- its begin step returned (0 when there was no row). The update is a
-- compare-and-set on that generation; 0 rows means the row moved, and
-- the caller reports it rather than recording over a newer write.
-- Only storage.Service calls this (guard test).
INSERT INTO storage_variants (
    object_hash, variant_key, size_bytes, content_type, metadata,
    content_sha256, content_size_bytes, digest_source, fixity_status, generation, fixity_checked_at
) VALUES (
    sqlc.arg(object_hash), sqlc.arg(variant_key), sqlc.arg(size_bytes), sqlc.arg(content_type), sqlc.arg(metadata),
    sqlc.arg(content_sha256), sqlc.arg(size_bytes), 'write', 'verified', 1, now()
)
ON CONFLICT (object_hash, variant_key) DO UPDATE SET
    size_bytes         = EXCLUDED.size_bytes,
    content_type       = EXCLUDED.content_type,
    metadata           = EXCLUDED.metadata,
    content_sha256     = EXCLUDED.content_sha256,
    content_size_bytes = EXCLUDED.content_size_bytes,
    digest_source      = 'write',
    fixity_status      = 'verified',
    fixity_checked_at  = now(),
    updated_at         = now()
WHERE storage_variants.generation = sqlc.arg(generation);

-- name: RecordOriginalVariant :exec
-- #1597: the ORIGINAL's row after its bytes were written. An original is
-- addressed by its own SHA-256, so its digest is object_hash, which the
-- caller computed from the very bytes it Put (UploadOriginal hashes the
-- stream; ImportVerified re-hashes the staged file). Two writers of one
-- hash can only write identical bytes, so no write lock is needed.
INSERT INTO storage_variants (
    object_hash, variant_key, size_bytes, content_type, metadata,
    content_sha256, content_size_bytes, digest_source, fixity_status, generation, fixity_checked_at
) VALUES (
    sqlc.arg(object_hash), 'original', sqlc.arg(size_bytes), sqlc.arg(content_type), '{}',
    sqlc.arg(object_hash), sqlc.arg(size_bytes), 'write', 'verified', 1, now()
)
ON CONFLICT (object_hash, variant_key) DO UPDATE SET
    size_bytes         = EXCLUDED.size_bytes,
    content_type       = EXCLUDED.content_type,
    content_sha256     = EXCLUDED.content_sha256,
    content_size_bytes = EXCLUDED.content_size_bytes,
    digest_source      = 'write',
    fixity_status      = 'verified',
    fixity_checked_at  = now(),
    generation         = storage_variants.generation + 1,
    updated_at         = now();

-- name: TouchOriginalVariant :exec
-- #1597: a re-upload that wrote NO bytes (the hash was already stored).
-- It may carry a richer content type, so that is kept, but it proves
-- nothing about the stored bytes: an existing row keeps its digest and
-- status, and a missing row is created unverifiable (column defaults).
INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, metadata)
VALUES (sqlc.arg(object_hash), 'original', sqlc.arg(size_bytes), sqlc.arg(content_type), '{}')
ON CONFLICT (object_hash, variant_key) DO UPDATE SET
    content_type = EXCLUDED.content_type,
    updated_at   = now();

-- name: GetVariantFixity :one
-- #1597: what a variant's row says about its bytes.
SELECT object_hash, variant_key, size_bytes, content_type,
       content_sha256, content_size_bytes, digest_source, fixity_status,
       generation, fixity_checked_at
FROM storage_variants
WHERE object_hash = sqlc.arg(object_hash) AND variant_key = sqlc.arg(variant_key);

-- name: MarkVariantFixity :execrows
-- #1597: a sweep's verdict on generation `generation` of a variant. A
-- compare-and-set, so a verdict about older bytes never lands on a row a
-- newer write has since replaced.
UPDATE storage_variants SET
    fixity_status     = sqlc.arg(fixity_status),
    fixity_checked_at = now()
WHERE object_hash = sqlc.arg(object_hash) AND variant_key = sqlc.arg(variant_key)
  AND generation = sqlc.arg(generation);

-- name: RecordVariantRecheck :execrows
-- #1597: an explicit recheck of an UNVERIFIABLE row (legacy, healed, or
-- interrupted) records the digest of the bytes it read back from the
-- backend, marked digest_source 'recheck' so it is never mistaken for a
-- write-time digest. Only an unverifiable row of the same generation is
-- touched; a recorded digest is never overwritten by a recheck.
UPDATE storage_variants SET
    content_sha256     = sqlc.arg(content_sha256),
    content_size_bytes = sqlc.arg(content_size_bytes),
    digest_source      = 'recheck',
    fixity_status      = 'verified',
    fixity_checked_at  = now()
WHERE object_hash = sqlc.arg(object_hash) AND variant_key = sqlc.arg(variant_key)
  AND generation = sqlc.arg(generation) AND fixity_status = 'unverifiable'
  AND size_bytes = sqlc.arg(content_size_bytes);

-- name: ListVariantsWithoutDigest :many
-- #1597: paged walk of rows with no trustworthy digest, for the explicit
-- backfill: unverifiable rows, and rows marked missing that never had a
-- digest (so a backfill revisits them once their bytes are back). A row
-- with a recorded digest, whatever its status, is left to the sweep.
-- Ordered by the primary key so it can resume after a cursor.
SELECT object_hash, variant_key
FROM storage_variants
WHERE content_sha256 IS NULL
  AND fixity_status = ANY (ARRAY['unverifiable', 'missing'])
  AND (object_hash, variant_key) > (sqlc.arg(after_hash)::TEXT, sqlc.arg(after_variant)::TEXT)
ORDER BY object_hash, variant_key
LIMIT sqlc.arg(max_rows);

-- name: GetVariant :one
SELECT object_hash, variant_key, size_bytes, content_type, metadata, created_at, updated_at
FROM storage_variants
WHERE object_hash = $1 AND variant_key = $2;

-- name: AddPin :exec
INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id)
VALUES ($1, $2, $3)
ON CONFLICT (object_hash, pin_subject_type, pin_subject_id) DO NOTHING;

-- name: LockObjectForPinChange :exec
-- #1596: a pin removal locks the object row BEFORE it changes a pin row,
-- the order every pin writer uses (AddPin, casfetch, migration 00084).
-- A concurrent pinner already holding the lock has committed by the time
-- this returns, so the GC mark that follows, a separate statement with a
-- fresh snapshot, sees that pin.
SELECT 1 FROM storage_objects WHERE hash = $1 FOR UPDATE;

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
-- One finding per (run, object, variant, finding) (#1598, migration
-- 00083): a batch retried after an interruption records nothing twice.
INSERT INTO storage_sweep_findings (id, run_id, finding, object_hash, variant_key, detail)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (run_id, object_hash, variant_key, finding) DO NOTHING;

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

-- name: LockImportHashSharedSession :exec
-- #72: the per-hash import lock, SHARED and SESSION-level, held by an
-- importer on one connection from before its intent is recorded until
-- its commit has finished (Put included). A crashed importer's session
-- ends and the lock goes with it, so "lock held" means "import alive".
SELECT pg_advisory_lock_shared(hashtextextended('storage-import:' || sqlc.arg(object_hash)::TEXT, 0));

-- name: UnlockImportHashSharedSession :one
SELECT pg_advisory_unlock_shared(hashtextextended('storage-import:' || sqlc.arg(object_hash)::TEXT, 0))::BOOLEAN AS value;

-- name: TryLockImportHashExclusive :one
-- #72: the reconciler's side, EXCLUSIVE, transaction-level, never
-- waiting: false while any import of the hash is alive.
SELECT pg_try_advisory_xact_lock(hashtextextended('storage-import:' || sqlc.arg(object_hash)::TEXT, 0))::BOOLEAN AS value;

-- name: InsertImportIntent :exec
-- #72: written before the backend Put of verified bytes.
INSERT INTO storage_import_intents (lease_id, object_hash) VALUES ($1, $2);

-- name: DeleteImportIntent :execrows
-- #72: removed in the transaction that records object, variant and pin.
-- Zero rows means the reconciler already treated the import as
-- abandoned, and the caller must not record the object.
DELETE FROM storage_import_intents WHERE lease_id = $1;

-- name: ListStaleImportIntentHashes :many
-- #72: hashes with at least one intent older than the cutoff.
SELECT DISTINCT object_hash
FROM storage_import_intents
WHERE created_at < now() - sqlc.arg(stale_after)::INTERVAL
ORDER BY object_hash
LIMIT sqlc.arg(max_rows);

-- name: DeleteImportIntentsForHash :execrows
-- #72: every intent of a hash. Only called by the reconciler while it
-- holds the exclusive import lock, when no import of the hash is alive.
DELETE FROM storage_import_intents WHERE object_hash = sqlc.arg(object_hash);

-- name: ObjectRowExists :one
SELECT EXISTS (SELECT 1 FROM storage_objects WHERE hash = $1)::BOOLEAN AS value;

-- name: ListStalePinsOfType :many
-- #72: pins of one subject type older than the cutoff. Used to release
-- federation_ingest pins that ingest (#1574) never converted into an
-- asset pin.
SELECT object_hash, pin_subject_id
FROM storage_pins
WHERE pin_subject_type = sqlc.arg(pin_subject_type)
  AND created_at < now() - sqlc.arg(stale_after)::INTERVAL
ORDER BY created_at
LIMIT sqlc.arg(max_rows);

-- name: InsertUploadReceipt :one
-- #1595: one receipt per successful upload, bound to the uploading user,
-- the exact hash and the base name the request carried (NULL with source
-- 'unknown' when it carried none). POST /assets consumes it.
INSERT INTO upload_receipts (user_ref, file_hash, original_basename, original_basename_source, expires_at)
VALUES (sqlc.arg(user_ref), sqlc.arg(file_hash), sqlc.narg(original_basename), sqlc.arg(original_basename_source),
        now() + sqlc.arg(ttl)::INTERVAL)
RETURNING id;

-- name: SweepExpiredUploadReceipts :execrows
-- #1595: delete at most max_rows expired receipts. Run on every upload,
-- so expired rows are removed faster than uploads can add them and the
-- table stays bounded without a background job. SKIP LOCKED keeps two
-- concurrent uploads from waiting on each other's sweep.
DELETE FROM upload_receipts
WHERE id IN (
    SELECT r.id FROM upload_receipts r
    WHERE r.expires_at <= now()
    ORDER BY r.expires_at
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
);

-- name: ListAssetsReferencingObject :many
-- #1598: every asset whose preservation history an observation of this
-- stored object belongs to: its primary file, an alternate, or a
-- companion. DISTINCT, so an asset that references the bytes twice gets
-- one event, and two assets sharing deduplicated bytes get one each. A
-- storage pin alone is not proof that an asset owns the bytes.
SELECT id FROM assets WHERE file_hash = sqlc.arg(object_hash)::TEXT
UNION
SELECT asset_id FROM asset_alternates WHERE object_hash = sqlc.arg(object_hash)::TEXT
UNION
SELECT asset_id FROM asset_companions WHERE object_hash = sqlc.arg(object_hash)::TEXT
ORDER BY 1;

-- name: InsertFixityEvent :execrows
-- #1598: one local fixity-check event for one asset. Idempotent on
-- (run, asset, object, variant, generation, reason): a retried batch
-- inserts nothing; a new run, a new generation or a different result is
-- a new event. The run is written as both its immutable snapshot
-- (sweep_run_id) and its live reference (sweep_run_ref).
--
-- The asset is read FOR KEY SHARE in the same statement: an asset
-- deleted since the reference lookup (committed, or committing while
-- this waits on its row) yields no row and no event, instead of a
-- foreign-key error that would fail the whole sweep. A surviving asset
-- is held until the transaction ends, so its event is always valid.
INSERT INTO asset_preservation_events (
    asset_id, source, event_type, occurred_at, outcome, outcome_reason, algorithm,
    object_hash, variant_key, observed_generation, observed_sha256, observed_size_bytes,
    recorded_sha256, fixity_status, sweep_run_id, sweep_run_ref, agent_name, agent_version
)
SELECT a.id, 'local', 'fixity-check', sqlc.arg(occurred_at)::timestamptz, sqlc.arg(outcome)::text, sqlc.arg(outcome_reason)::text, sqlc.narg(algorithm)::text,
       sqlc.arg(object_hash)::text, sqlc.arg(variant_key)::text, sqlc.arg(observed_generation)::bigint, sqlc.narg(observed_sha256)::text, sqlc.narg(observed_size_bytes)::bigint,
       sqlc.narg(recorded_sha256)::text, sqlc.arg(fixity_status)::text, sqlc.arg(sweep_run_id)::uuid, sqlc.arg(sweep_run_id)::uuid, sqlc.arg(agent_name)::text, sqlc.narg(agent_version)::text
FROM assets a
WHERE a.id = sqlc.arg(asset_id)::uuid
FOR KEY SHARE OF a
ON CONFLICT (sweep_run_id, asset_id, object_hash, variant_key, observed_generation, outcome_reason) WHERE sweep_run_id IS NOT NULL
DO NOTHING;

-- name: ListAssetPreservationEvents :many
-- #1598: one page of an asset's preservation history, newest first.
-- Keyset on (occurred_at, id), both descending: rows with equal
-- timestamps still have one fixed order, and a page never shifts or
-- repeats when newer events are written while an operator pages back.
-- before_at/before_id are the last row of the previous page; NULL
-- fetches the newest page.
SELECT id, asset_id, source, event_type, occurred_at, outcome, outcome_reason, algorithm,
       object_hash, variant_key, observed_generation, observed_sha256, observed_size_bytes,
       recorded_sha256, fixity_status, sweep_run_id, sweep_run_ref, agent_name, agent_version
FROM asset_preservation_events
WHERE asset_id = sqlc.arg(asset_id)::uuid
  AND (sqlc.narg(before_at)::timestamptz IS NULL
       OR (occurred_at, id) < (sqlc.narg(before_at)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: CountAssetPreservationEvents :one
-- #1598: how many events an asset's history holds (cursor-independent).
SELECT count(*) FROM asset_preservation_events WHERE asset_id = sqlc.arg(asset_id)::uuid;

-- name: AssetRowExists :one
-- #1598: whether an asset row exists at all. A soft-deleted asset still
-- has a row, and its preservation history stays readable to operators.
SELECT EXISTS (SELECT 1 FROM assets WHERE id = sqlc.arg(asset_id)::uuid);
