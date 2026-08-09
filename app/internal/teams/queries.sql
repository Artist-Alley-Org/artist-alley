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
-- Joined against "user" + user_profiles so the response carries a NAME
-- (#684), the same shape social.ListFollowers uses.
--
-- Before the team page this query returned bare refs, because its only
-- consumer was an admin table that renders them as ids. A member strip
-- built on that could say nothing but "#19", and resolving the names
-- client-side would be one /users/by-ref request per member on every
-- team page — a dozen round trips for a line of text a JOIN already
-- had in hand.
--
-- LEFT JOIN on user_profiles: a profile row is optional, and a member
-- without one must still appear in the strip under their username
-- rather than vanish from their own team.
SELECT tm.team_id,
       tm.user_ref,
       tm.added_at,
       tm.added_by_user_ref,
       u.username,
       up.display_name
FROM team_memberships tm
JOIN "user" u              ON u.ref = tm.user_ref
LEFT JOIN user_profiles up ON up.user_ref = tm.user_ref
WHERE tm.team_id = $1
ORDER BY tm.added_at DESC, tm.user_ref ASC;

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

-- name: FollowTeam :exec
-- Bookmark a team into the caller's channels rail (#577).
--
-- ON CONFLICT DO NOTHING makes follow IDEMPOTENT: a double-tapped
-- button, a retried request and a genuine re-follow are one request
-- with one outcome. The alternative — letting the PK raise 23505 and
-- mapping it to a 409 — would make the client's correctness depend on
-- it knowing a state the server already knows.
--
-- LIVENESS IS NOT CHECKED HERE. The team_id FK cannot see
-- teams.deleted_at, so this statement will happily bookmark a
-- tombstoned team. The handler probes for that BEFORE calling this,
-- the same discipline visibility.CanAssignToTeam carries (#955). Do
-- not call this without the probe.
INSERT INTO team_follows (user_ref, team_id)
VALUES ($1, $2)
ON CONFLICT (user_ref, team_id) DO NOTHING;

-- name: UnfollowTeam :execrows
-- Drop the caller's bookmark. :execrows rather than :exec so the
-- handler can log the no-op case, but the response is 204 either way —
-- unfollowing something you do not follow has already achieved what
-- the caller asked for.
--
-- Deliberately NOT joined against teams: a follow of a since-deleted
-- team is exactly the row a user most wants to be able to remove, and
-- a liveness filter here would strand it in their rail permanently.
DELETE FROM team_follows
WHERE user_ref = $1 AND team_id = $2;

-- name: ListFollowedTeams :many
-- The caller's channels rail (#577). Same projection and ordering as
-- ListUserTeams so the two lists render through one code path, but a
-- DIFFERENT table: this is what the user bookmarked, that is what the
-- user belongs to. They are not the same question and neither implies
-- the other.
--
-- Soft-deleted teams are filtered out rather than deleted, so a studio
-- that is tombstoned simply leaves the rail and comes back if it is
-- ever restored.
SELECT t.id, t.slug, t.name, t.description, t.origin_server_id,
       t.created_at, t.updated_at, t.deleted_at
FROM team_follows tf
JOIN teams t ON t.id = tf.team_id
WHERE tf.user_ref = $1 AND t.deleted_at IS NULL
ORDER BY t.name ASC, t.id ASC;

-- name: IsTeamLive :one
-- The liveness half of the follow gate. Separate from any existence
-- check ON PURPOSE: a nonexistent team and a soft-deleted team both
-- answer false, so the handler cannot accidentally tell them apart and
-- the endpoint cannot become a team-existence oracle. See
-- visibility.CanAssignToTeam for the full argument.
SELECT EXISTS (
    SELECT 1 FROM teams WHERE id = $1 AND deleted_at IS NULL
) AS live;

-- name: TeamDirectoryStats :many
-- Member and content counts for one PAGE of the /teams directory
-- (#684) — the "10 members · 173 works" line on each card.
--
-- Batched over the page's ids rather than issued per row: the
-- directory renders up to 100 teams and three round trips beat 200.
--
-- ## Why these are computed, not stored
--
-- Neither number has a column. A denormalised count is a second source
-- of truth needing maintenance on every membership change, upload,
-- delete, restore and team merge, and both of these run against an
-- index (team_memberships PK; assets_team_idx). Add the column when a
-- measurement says to.
--
-- ## Why the asset count is not visibility-filtered
--
-- It is a raw count and it deliberately includes assets whose fields
-- this caller may not read. That discloses nothing new: these
-- endpoints are teams.read-gated, so the caller is signed in, and the
-- authenticated asset predicate already returns restricted rows to
-- them as placeholders — they can reach the same number today by
-- paging /assets?team_id=X and counting. Filtering here would make the
-- card disagree with the page it links to, for no gain.
--
-- Soft-deleted assets ARE excluded, because those are not reachable by
-- that route and the count would then be one nobody can verify.
SELECT t.id AS team_id,
       (SELECT COUNT(*)::BIGINT FROM team_memberships tm WHERE tm.team_id = t.id) AS member_count,
       (SELECT COUNT(*)::BIGINT FROM assets a
         WHERE a.team_id = t.id AND a.deleted_at IS NULL) AS asset_count
FROM teams t
WHERE t.id = ANY(sqlc.arg('team_ids')::uuid[]);
