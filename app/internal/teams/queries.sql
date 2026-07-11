-- Team CRUD + DAG membership queries. Owned by app/internal/teams.
-- See migration 00015 and ADR 0010 Layer 4.

-- name: CreateTeam :one
INSERT INTO teams (slug, name, description)
VALUES ($1, $2, $3)
RETURNING id, slug, name, description, origin_server_id, created_at, updated_at, deleted_at;

-- name: GetTeam :one
SELECT id, slug, name, description, origin_server_id, created_at, updated_at, deleted_at
FROM teams
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateTeam :one
-- PATCH-style: only non-NULL params apply. COALESCE keeps existing
-- values when the caller omits a field.
UPDATE teams
   SET name        = COALESCE(sqlc.narg('name'),        name),
       description = COALESCE(sqlc.narg('description'), description),
       updated_at  = NOW()
 WHERE id = sqlc.arg('id') AND deleted_at IS NULL
 RETURNING id, slug, name, description, origin_server_id, created_at, updated_at, deleted_at;

-- name: SoftDeleteTeam :execrows
UPDATE teams
   SET deleted_at = NOW(), updated_at = NOW()
 WHERE id = $1 AND deleted_at IS NULL;

-- name: ListTeams :many
-- Paginated by (name ASC, id ASC). When cursor_name/cursor_id are
-- supplied, returns rows strictly after the cursor.
SELECT id, slug, name, description, origin_server_id, created_at, updated_at, deleted_at
FROM teams
WHERE deleted_at IS NULL
  AND (sqlc.narg('cursor_name')::text IS NULL OR (name, id) > (sqlc.narg('cursor_name')::text, sqlc.narg('cursor_id')::uuid))
ORDER BY name ASC, id ASC
LIMIT sqlc.arg('row_limit')::int;

-- name: ListTeamsUnderAncestor :many
-- All teams in the closure of an ancestor (including the ancestor
-- itself via the depth-0 self-row). Used by the upload-modal team
-- picker when scoping to "anywhere under team X".
SELECT t.id, t.slug, t.name, t.description, t.origin_server_id,
       t.created_at, t.updated_at, t.deleted_at
FROM team_closure c
JOIN teams t ON t.id = c.descendant_id
WHERE c.ancestor_id = $1
  AND t.deleted_at IS NULL
ORDER BY t.name ASC, t.id ASC
LIMIT $2;

-- name: ListTeamParents :many
-- Direct parents of a team (single hop, no closure walk).
SELECT t.id, t.slug, t.name, t.description, t.origin_server_id,
       t.created_at, t.updated_at, t.deleted_at
FROM team_parents tp
JOIN teams t ON t.id = tp.parent_id
WHERE tp.child_id = $1 AND t.deleted_at IS NULL
ORDER BY t.name ASC;

-- name: AddTeamParent :exec
-- Inserts the edge; the BEFORE-INSERT cycle-rejection trigger raises
-- check_violation if this would close a cycle, and the AFTER-INSERT
-- propagation trigger materialises new closure rows.
INSERT INTO team_parents (child_id, parent_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: RemoveTeamParent :execrows
DELETE FROM team_parents
WHERE child_id = $1 AND parent_id = $2;

-- name: ListTeamMembers :many
SELECT team_id, user_ref, added_at, added_by_user_ref
FROM team_memberships
WHERE team_id = $1
ORDER BY added_at DESC, user_ref ASC;

-- name: AddTeamMember :exec
INSERT INTO team_memberships (team_id, user_ref, added_by_user_ref)
VALUES ($1, $2, $3)
ON CONFLICT (team_id, user_ref) DO NOTHING;

-- name: RemoveTeamMember :execrows
DELETE FROM team_memberships
WHERE team_id = $1 AND user_ref = $2;

-- name: ListUserTeams :many
-- Direct team memberships for the caller's user_ref. Used by
-- /auth/me/teams to render the upload modal's team picker.
SELECT t.id, t.slug, t.name, t.description, t.origin_server_id,
       t.created_at, t.updated_at, t.deleted_at
FROM team_memberships tm
JOIN teams t ON t.id = tm.team_id
WHERE tm.user_ref = $1 AND t.deleted_at IS NULL
ORDER BY t.name ASC, t.id ASC;
