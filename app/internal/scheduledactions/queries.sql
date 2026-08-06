-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- Scheduled-action engine queries (#40 sprint 1, ADR 0020).

-- name: CreateScheduledAction :one
-- Schedules one action. created_by is nullable — a system-scheduled
-- action (e.g. trash retention) has no user behind it.
INSERT INTO scheduled_actions (action, target_kind, target_id, params, scheduled_for, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, action, target_kind, target_id, params, scheduled_for,
          state, error, created_by, created_at, executed_at;

-- name: GetScheduledAction :one
SELECT id, action, target_kind, target_id, params, scheduled_for,
       state, error, created_by, created_at, executed_at
FROM scheduled_actions
WHERE id = $1;

-- name: ListScheduledActions :many
-- Admin surface. Optional state filter (NULL = all states), newest
-- first, with a cursor on created_at for pagination.
SELECT id, action, target_kind, target_id, params, scheduled_for,
       state, error, created_by, created_at, executed_at
FROM scheduled_actions
WHERE (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state')::text)
  AND (sqlc.narg('cursor_created_at')::timestamptz IS NULL
       OR created_at < sqlc.narg('cursor_created_at')::timestamptz)
ORDER BY created_at DESC
LIMIT sqlc.arg('row_limit')::integer;

-- name: CancelScheduledAction :execrows
-- Only a PENDING action can be cancelled — a done/failed/cancelled one
-- is terminal. Guarding on state='pending' makes cancel idempotent and
-- lets the handler map rows-affected=0 to "not cancellable" rather than
-- silently succeeding on an already-fired action.
UPDATE scheduled_actions
SET state = 'cancelled'
WHERE id = $1 AND state = 'pending';

-- name: ClaimDueAction :one
-- The reaper's claim. One pending action whose time has come, oldest
-- first, row-locked so overlapping reaper runs never grab the same one.
-- SKIP LOCKED means a second reaper moves on to the next due action
-- instead of blocking. The caller executes + marks within the SAME
-- transaction that holds this lock, so no intermediate "executing"
-- state is needed.
SELECT id, action, target_kind, target_id, params, scheduled_for,
       state, error, created_by, created_at, executed_at
FROM scheduled_actions
WHERE state = 'pending' AND scheduled_for <= NOW()
ORDER BY scheduled_for ASC
FOR UPDATE SKIP LOCKED
LIMIT 1;

-- name: MarkActionDone :exec
UPDATE scheduled_actions
SET state = 'done', executed_at = NOW(), error = NULL
WHERE id = $1;

-- name: MarkActionFailed :exec
UPDATE scheduled_actions
SET state = 'failed', executed_at = NOW(), error = $2
WHERE id = $1;

-- name: CountDueActions :one
-- Cheap "is there work" probe for the reaper's log line + tests.
SELECT COUNT(*)::bigint AS value
FROM scheduled_actions
WHERE state = 'pending' AND scheduled_for <= NOW();

-- ---------------------------------------------------------------------------
-- Executor domain writes. These live in the executor's own package on
-- purpose: the scheduled-action engine owns the small, audited mutations
-- it performs, and each is a single guarded statement.
-- ---------------------------------------------------------------------------

-- name: ExecAssetChangeSensitivity :one
-- Flips an asset's sensitivity tier and returns the PREVIOUS value so
-- the executor can record old->new in the audit trail. The CTE reads +
-- locks the row before the UPDATE so the returned old value is the real
-- prior tier, not the just-written one. Zero rows = asset missing or
-- soft-deleted; the executor treats that as a failure, not a silent
-- no-op.
WITH prev AS (
    SELECT assets.id AS pid, assets.sensitivity AS old_sensitivity
    FROM assets
    WHERE assets.id = $1 AND assets.deleted_at IS NULL
    FOR UPDATE
)
UPDATE assets a
SET sensitivity = $2, updated_at = NOW()
FROM prev
WHERE a.id = prev.pid
RETURNING prev.old_sensitivity::text AS old_sensitivity;

-- name: ExecAssetSoftDelete :execrows
-- Soft-delete (set deleted_at); the nightly GC hard-deletes later. Only
-- acts on a live row, so re-running against an already-deleted asset is
-- a no-op the executor reports as rows-affected=0.
--
-- deleted_by_user_ref is the scheduled action's `created_by`, which is
-- itself nullable — a system-scheduled retention delete has no human
-- behind it. NULL there means "nobody may self-restore this", which is
-- the correct fail-closed answer rather than a gap (#931).
UPDATE assets
SET deleted_at = NOW(), deleted_reason = $2, deleted_by_user_ref = $3, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ExecAssetChangeState :one
-- Moves an asset to a workflow state, returning the previous state_id
-- for the audit trail. Same CTE-lock pattern as sensitivity.
WITH prev AS (
    SELECT assets.id AS pid, assets.state_id AS old_state_id
    FROM assets
    WHERE assets.id = $1 AND assets.deleted_at IS NULL
    FOR UPDATE
)
UPDATE assets a
SET state_id = $2, updated_at = NOW()
FROM prev
WHERE a.id = prev.pid
RETURNING prev.old_state_id AS old_state_id;

-- name: ResolveWorkflowStateByCode :one
-- change_state accepts a state CODE (per ADR 0020's "to_state":
-- "archived") and resolves it to the id within a domain. Returns the id
-- so the executor can call ExecAssetChangeState.
SELECT id FROM workflow_states
WHERE domain = $1 AND code = $2;

-- name: GetAssetSensitivityDomain :one
-- The executor needs the asset's workflow domain to resolve a state
-- code, and its owner for a notify subject. One read covers both.
SELECT owner_user_ref, state_id FROM assets WHERE id = $1 AND deleted_at IS NULL;
