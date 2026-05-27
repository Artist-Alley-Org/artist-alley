-- Workflow state machine queries. Owned by app/internal/workflow.
-- See migration 00018 and docs/adr/0010-permissions-teams-workflow.md.

-- name: GetState :one
SELECT id, domain, code, label, sort_order, is_initial, is_terminal, visible_by_default
FROM workflow_states
WHERE id = $1;

-- name: GetStateByCode :one
SELECT id, domain, code, label, sort_order, is_initial, is_terminal, visible_by_default
FROM workflow_states
WHERE domain = $1 AND code = $2;

-- name: GetInitialState :one
-- The single is_initial row for the domain. Used at resource creation
-- time to populate state_id with the correct entry point. Partial
-- unique index guarantees at most one row.
SELECT id, domain, code, label, sort_order, is_initial, is_terminal, visible_by_default
FROM workflow_states
WHERE domain = $1 AND is_initial = TRUE
LIMIT 1;

-- name: ListStatesForDomain :many
SELECT id, domain, code, label, sort_order, is_initial, is_terminal, visible_by_default
FROM workflow_states
WHERE domain = $1
ORDER BY sort_order, code;

-- name: ListTransitionsForDomain :many
-- Returns every transition where the from-state OR to-state is in the
-- given domain. Includes the NULL-from "initial entry" rows for the
-- domain's initial state. Used for the workflow editor UI.
SELECT t.id, t.from_state_id, t.to_state_id, t.required_capability, t.requires_team_scope
FROM workflow_transitions t
LEFT JOIN workflow_states fs ON fs.id = t.from_state_id
JOIN workflow_states ts ON ts.id = t.to_state_id
WHERE ts.domain = $1 AND (t.from_state_id IS NULL OR fs.domain = $1);

-- name: FindTransition :one
-- Looks up the exact transition row for a (from, to) pair. Both
-- params are pgtype.UUID; pass an invalid (Valid=false) UUID for
-- from when checking the "initial entry" transition (from is NULL
-- in the row).
--
-- NB: $1 IS NULL OR f.from_state_id = $1 covers both shapes because
-- the param's NULL-ness flips the first clause; sqlc-pgx renders
-- pgtype.UUID with Valid=false as NULL on the wire.
SELECT id, from_state_id, to_state_id, required_capability, requires_team_scope
FROM workflow_transitions
WHERE from_state_id IS NOT DISTINCT FROM $1
  AND to_state_id = $2;

-- name: GetPostState :one
-- Returns the post's current state_id and team_id together (both nullable).
-- One round-trip for the Transition() pre-check.
SELECT state_id, team_id
FROM posts
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetAssetState :one
SELECT state_id, team_id
FROM assets
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdatePostState :execrows
-- Sets the post's state_id. Returns the row count so callers can
-- distinguish "no such post / already deleted" from a real update.
UPDATE posts
   SET state_id = $2, updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateAssetState :execrows
UPDATE assets
   SET state_id = $2, updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL;

-- name: InsertWorkflowAudit :exec
INSERT INTO workflow_audit (
    resource_kind, resource_id, from_state_id, to_state_id, actor_rs_user_id, note
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListWorkflowAudit :many
-- Audit history for a single resource, newest first.
SELECT a.id, a.resource_kind, a.resource_id,
       a.from_state_id, a.to_state_id,
       a.actor_rs_user_id, a.note, a.transitioned_at,
       fs.code AS from_code, ts.code AS to_code
FROM workflow_audit a
LEFT JOIN workflow_states fs ON fs.id = a.from_state_id
JOIN workflow_states ts ON ts.id = a.to_state_id
WHERE a.resource_kind = $1 AND a.resource_id = $2
ORDER BY a.transitioned_at DESC
LIMIT $3 OFFSET $4;
