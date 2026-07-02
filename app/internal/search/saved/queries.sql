-- Phase 1.16.B-4 — sqlc queries for the saved_search table.

-- name: CreateSavedSearch :one
-- Owner + name uniqueness enforced by the composite UNIQUE index;
-- handler catches 23505 and maps to 409 Conflict.
INSERT INTO saved_search (
    owner_user_ref, name, dsl, notify_channel, notify_interval_minutes
) VALUES ($1, $2, $3, $4, $5)
RETURNING id, owner_user_ref, name, dsl, notify_channel,
          notify_interval_minutes, enabled,
          last_result_hash, last_result_ids,
          last_run_at, last_notified_at,
          origin_server_id, created_at, updated_at;

-- name: GetSavedSearch :one
SELECT id, owner_user_ref, name, dsl, notify_channel,
       notify_interval_minutes, enabled,
       last_result_hash, last_result_ids,
       last_run_at, last_notified_at,
       origin_server_id, created_at, updated_at
FROM saved_search
WHERE id = $1;

-- name: ListSavedSearchesForOwner :many
-- Owner-scoped listing for the /account/saved-searches page.
-- Keyset pagination on (created_at DESC, id DESC) mirrors the
-- convention every other user-owned table uses in the project.
SELECT id, owner_user_ref, name, dsl, notify_channel,
       notify_interval_minutes, enabled,
       last_result_hash, last_result_ids,
       last_run_at, last_notified_at,
       origin_server_id, created_at, updated_at
FROM saved_search
WHERE owner_user_ref = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: CountEnabledSavedSearchesForOwner :one
-- Rate-limit gate: Create refuses when the caller already owns >=
-- sysconfig.search.saved_search.max_per_user ENABLED rows.
-- Disabled rows don't count so operators can keep history without
-- burning their cap.
SELECT COUNT(*)::BIGINT
FROM saved_search
WHERE owner_user_ref = $1 AND enabled = TRUE;

-- name: UpdateSavedSearch :one
-- Partial update via COALESCE on nullable narg() args. Ownership
-- gate is enforced at the HTTP layer before this query runs.
UPDATE saved_search SET
    name                    = COALESCE(sqlc.narg('name'), name),
    dsl                     = COALESCE(sqlc.narg('dsl'), dsl),
    notify_channel          = COALESCE(sqlc.narg('notify_channel'), notify_channel),
    notify_interval_minutes = COALESCE(sqlc.narg('notify_interval_minutes'), notify_interval_minutes),
    enabled                 = COALESCE(sqlc.narg('enabled'), enabled),
    updated_at              = NOW()
WHERE id = sqlc.arg('id')
RETURNING id, owner_user_ref, name, dsl, notify_channel,
          notify_interval_minutes, enabled,
          last_result_hash, last_result_ids,
          last_run_at, last_notified_at,
          origin_server_id, created_at, updated_at;

-- name: DeleteSavedSearch :exec
DELETE FROM saved_search WHERE id = $1;

-- name: RecordSavedSearchRun :one
-- Called at the end of every notify_run. Writes fresh delta state
-- + timestamps in one statement. Returned row is the fresh
-- snapshot; the caller uses it for observability + digest email
-- rendering.
UPDATE saved_search SET
    last_result_hash = $2,
    last_result_ids  = $3,
    last_run_at      = NOW(),
    last_notified_at = CASE WHEN $4::BOOLEAN THEN NOW() ELSE last_notified_at END,
    updated_at       = NOW()
WHERE id = $1
RETURNING id, owner_user_ref, name, dsl, notify_channel,
          notify_interval_minutes, enabled,
          last_result_hash, last_result_ids,
          last_run_at, last_notified_at,
          origin_server_id, created_at, updated_at;

-- name: ListDueSavedSearches :many
-- Coordinator walk: enabled rows whose next-run threshold is past.
-- The partial index (saved_search_due_idx) covers the sort.
-- Batch size caps a single coordinator tick so a large table
-- doesn't wedge one goroutine.
SELECT id, owner_user_ref, name, dsl, notify_channel,
       notify_interval_minutes, enabled,
       last_result_hash, last_result_ids,
       last_run_at, last_notified_at,
       origin_server_id, created_at, updated_at
FROM saved_search
WHERE enabled = TRUE
  AND (last_run_at IS NULL
       OR last_run_at + (notify_interval_minutes * INTERVAL '1 minute') <= NOW())
ORDER BY last_run_at NULLS FIRST
LIMIT $1;

-- name: CountActiveSavedSearches :one
-- Gauge query for /admin/search/health.
SELECT COUNT(*)::BIGINT FROM saved_search WHERE enabled = TRUE;
