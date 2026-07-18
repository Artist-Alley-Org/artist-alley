-- ---------------------------------------------------------------------------
-- jobs queue — generic background work
-- ---------------------------------------------------------------------------
--
-- Workers claim with FOR UPDATE SKIP LOCKED so any number of workers
-- (in-process, external farm, federated peer) can drain the queue
-- without stepping on each other. A lease (`lease_expires_at`) keeps
-- a dead worker's job from being lost — a watchdog requeues stuck
-- rows in code.

-- name: EnqueueJob :one
-- Insert a fresh job. `payload` is handler-defined JSONB. `priority`
-- defaults to 100; lower numbers run sooner. `idempotency_key` is
-- optional — when set, the partial UNIQUE INDEX on
-- (type, idempotency_key) WHERE status IN ('pending','running')
-- (migration 00009) causes a 23505 unique_violation if the same
-- (type, key) work is already in-flight. Handler layer catches
-- that violation and looks the existing job up via
-- GetJobByIdempotencyKey, returning the existing id.
INSERT INTO jobs (
    type, payload, priority, max_attempts, scheduled_for, origin_server_id,
    idempotency_key
) VALUES (
    $1, $2,
    COALESCE(sqlc.narg('priority')::INTEGER, 100),
    COALESCE(sqlc.narg('max_attempts')::INTEGER, 3),
    sqlc.narg('scheduled_for')::TIMESTAMPTZ,
    sqlc.narg('origin_server_id')::UUID,
    sqlc.narg('idempotency_key')::TEXT
)
RETURNING id, type, payload, status, priority, attempts, max_attempts,
          claimed_by, claimed_at, lease_expires_at, last_error, result,
          origin_server_id, scheduled_for,
          enqueued_at, started_at, finished_at,
          idempotency_key;

-- name: GetJobByIdempotencyKey :one
-- Resolves the existing in-flight job's id when an Enqueue hits the
-- partial UNIQUE INDEX. Looks at pending + running statuses only —
-- a completed/failed job with the same key should not block a fresh
-- re-enqueue (the new call is genuinely new work, the prior result
-- is historical).
SELECT id, type, payload, status, priority, attempts, max_attempts,
       claimed_by, claimed_at, lease_expires_at, last_error, result,
       origin_server_id, scheduled_for,
       enqueued_at, started_at, finished_at,
       idempotency_key
FROM jobs
WHERE type = $1
  AND idempotency_key = $2
  AND status IN ('pending', 'running');

-- name: ClaimNextJob :one
-- Atomically pick the highest-priority pending job whose type is in
-- the given list, mark it as running, and return its current state.
-- FOR UPDATE SKIP LOCKED makes this race-free across N workers.
--
-- `types` empty = any type. `lease_seconds` is how long the claim
-- is valid before a watchdog can requeue it.
WITH picked AS (
    SELECT id
      FROM jobs
     WHERE status = 'pending'
       AND (sqlc.narg('scope_types')::TEXT[] IS NULL
            OR cardinality(sqlc.narg('scope_types')::TEXT[]) = 0
            OR type = ANY(sqlc.narg('scope_types')::TEXT[]))
       AND (scheduled_for IS NULL OR scheduled_for <= NOW())
       AND attempts < max_attempts
     ORDER BY priority ASC, enqueued_at ASC
     LIMIT 1
     FOR UPDATE SKIP LOCKED
)
UPDATE jobs j SET
    status           = 'running',
    attempts         = attempts + 1,
    claimed_by       = sqlc.arg('claimed_by')::TEXT,
    claimed_at       = NOW(),
    lease_expires_at = NOW() + (sqlc.arg('lease_seconds')::INTEGER * INTERVAL '1 second'),
    started_at       = COALESCE(j.started_at, NOW())
FROM picked
WHERE j.id = picked.id
RETURNING j.id, j.type, j.payload, j.status, j.priority, j.attempts, j.max_attempts,
          j.claimed_by, j.claimed_at, j.lease_expires_at, j.last_error, j.result,
          j.origin_server_id, j.scheduled_for,
          j.enqueued_at, j.started_at, j.finished_at,
          j.idempotency_key;

-- name: ClaimJobBatch :many
-- Like ClaimNextJob but returns up to `limit` rows for batched
-- external-worker pulls. Same FOR UPDATE SKIP LOCKED semantics.
WITH picked AS (
    SELECT id
      FROM jobs
     WHERE status = 'pending'
       AND (sqlc.narg('scope_types')::TEXT[] IS NULL
            OR cardinality(sqlc.narg('scope_types')::TEXT[]) = 0
            OR type = ANY(sqlc.narg('scope_types')::TEXT[]))
       AND (scheduled_for IS NULL OR scheduled_for <= NOW())
       AND attempts < max_attempts
     ORDER BY priority ASC, enqueued_at ASC
     LIMIT sqlc.arg('row_limit')::INTEGER
     FOR UPDATE SKIP LOCKED
)
UPDATE jobs j SET
    status           = 'running',
    attempts         = attempts + 1,
    claimed_by       = sqlc.arg('claimed_by')::TEXT,
    claimed_at       = NOW(),
    lease_expires_at = NOW() + (sqlc.arg('lease_seconds')::INTEGER * INTERVAL '1 second'),
    started_at       = COALESCE(j.started_at, NOW())
FROM picked
WHERE j.id = picked.id
RETURNING j.id, j.type, j.payload, j.status, j.priority, j.attempts, j.max_attempts,
          j.claimed_by, j.claimed_at, j.lease_expires_at, j.last_error, j.result,
          j.origin_server_id, j.scheduled_for,
          j.enqueued_at, j.started_at, j.finished_at,
          j.idempotency_key;

-- name: HeartbeatJob :execrows
-- Extend the lease on a running job. Worker must call this faster
-- than `lease_seconds` to keep ownership; otherwise the watchdog
-- requeues. Returns the row count so the worker knows if its lease
-- was already stolen.
UPDATE jobs SET
    lease_expires_at = NOW() + (sqlc.arg('lease_seconds')::INTEGER * INTERVAL '1 second')
WHERE id = $1
  AND status = 'running'
  AND claimed_by = sqlc.arg('claimed_by')::TEXT;

-- name: CompleteJob :execrows
-- Mark a running job as done. `result` is handler-defined JSONB.
UPDATE jobs SET
    status      = 'done',
    finished_at = NOW(),
    result      = sqlc.narg('result')::JSONB,
    last_error  = NULL
WHERE id = $1
  AND status = 'running'
  AND claimed_by = sqlc.arg('claimed_by')::TEXT;

-- name: FailJob :execrows
-- Record a failure on a running job. If attempts >= max_attempts the
-- row goes to status='failed'; otherwise it returns to 'pending' so a
-- future claim picks it up. The caller can force-fail by passing
-- terminal=TRUE (handler-level "this will never succeed").
UPDATE jobs SET
    status =
        CASE
            WHEN sqlc.arg('terminal')::BOOLEAN THEN 'failed'
            WHEN attempts >= max_attempts      THEN 'failed'
            ELSE 'pending'
        END,
    finished_at =
        CASE
            WHEN sqlc.arg('terminal')::BOOLEAN OR attempts >= max_attempts THEN NOW()
            ELSE NULL
        END,
    claimed_by       = NULL,
    claimed_at       = NULL,
    lease_expires_at = NULL,
    last_error       = sqlc.arg('error_message')::TEXT
WHERE id = $1
  AND status = 'running'
  AND claimed_by = sqlc.arg('claimed_by')::TEXT;

-- name: RequeueStuckJobs :execrows
-- Watchdog. Returns any job whose lease has expired to the pending
-- pool so another worker can pick it up. The attempts counter has
-- already been incremented at claim time, so retries are capped.
UPDATE jobs SET
    status           = 'pending',
    claimed_by       = NULL,
    claimed_at       = NULL,
    lease_expires_at = NULL,
    last_error       = COALESCE(last_error, '') || ' [lease expired]'
WHERE status = 'running'
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at < NOW();

-- name: GetJob :one
SELECT id, type, payload, status, priority, attempts, max_attempts,
       claimed_by, claimed_at, lease_expires_at, last_error, result,
       origin_server_id, scheduled_for,
       enqueued_at, started_at, finished_at,
       idempotency_key
FROM jobs
WHERE id = $1;

-- name: CountJobsByStatus :many
-- Cheap summary for the admin queue UI.
SELECT type, status, COUNT(*)::BIGINT AS count
FROM jobs
GROUP BY type, status
ORDER BY type, status;

-- name: PurgeOldDoneJobs :execrows
-- Periodic cleanup. Keeps `done` rows for `keep_days` so the admin
-- view still has a recent history, then deletes.
DELETE FROM jobs
WHERE status = 'done'
  AND finished_at < NOW() - (sqlc.arg('keep_days')::INTEGER * INTERVAL '1 day');

-- name: AdminListJobs :many
-- Read-only admin queue view (#400). Optional status + type filters
-- (NULL = all). Ordered priority ASC, enqueued_at ASC — the same order
-- the pending index (priority, enqueued_at) and ClaimNextJob use, so
-- the operator sees jobs in roughly the order they'll run. `age_seconds`
-- is derived server-side (NOW() - enqueued_at) so the UI needn't trust a
-- client clock. LIMIT/OFFSET paging mirrors the metadata-extraction
-- admin list.
SELECT id, type, status, priority, attempts, max_attempts,
       claimed_by, claimed_at, lease_expires_at, last_error,
       origin_server_id, scheduled_for, enqueued_at, started_at, finished_at,
       EXTRACT(EPOCH FROM (NOW() - enqueued_at))::BIGINT AS age_seconds
  FROM jobs
 WHERE (sqlc.narg('status')::TEXT IS NULL OR status = sqlc.narg('status')::TEXT)
   AND (sqlc.narg('type')::TEXT   IS NULL OR type   = sqlc.narg('type')::TEXT)
 ORDER BY priority ASC, enqueued_at ASC
 LIMIT $1 OFFSET $2;

-- name: AdminCountJobs :one
-- Total under the same status + type filter (ignores limit/offset), so
-- the queue UI can page + show a total.
SELECT COUNT(*)::BIGINT
  FROM jobs
 WHERE (sqlc.narg('status')::TEXT IS NULL OR status = sqlc.narg('status')::TEXT)
   AND (sqlc.narg('type')::TEXT   IS NULL OR type   = sqlc.narg('type')::TEXT);

-- name: AdminListActiveWorkers :many
-- One row per running job = one busy worker holding that job (#400).
-- `claimed_by` is the worker id; lease_expires_at is when the lease
-- lapses (RequeueStuckJobs reclaims a job whose lease expired). Ordered
-- by worker then claim time so a worker's held work groups together.
-- Bounded by the running-job count (workerPoolSize is NumCPU/2 ≤ 8), so
-- no LIMIT is needed. lease_stale is a convenience flag the UI colours.
SELECT claimed_by, id AS job_id, type, priority, attempts,
       claimed_at, lease_expires_at,
       (lease_expires_at < NOW()) AS lease_stale
  FROM jobs
 WHERE status = 'running'
   AND claimed_by IS NOT NULL
 ORDER BY claimed_by ASC, claimed_at ASC;
