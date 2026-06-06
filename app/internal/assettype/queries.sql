-- name: List :many
-- List every asset type, sorted by display order then by ID. Used
-- by GET /api/v1/asset_types and by anything that needs the lookup
-- table in memory.
SELECT ref,
       name,
       allowed_extensions,
       order_by,
       icon,
       colour,
       tab
FROM asset_types
ORDER BY COALESCE(order_by, 0), ref;

-- name: Get :one
-- Fetch a single asset type by primary key. Returns sql.ErrNoRows
-- when missing.
SELECT ref,
       name,
       allowed_extensions,
       order_by,
       icon,
       colour,
       tab
FROM asset_types
WHERE ref = $1;

-- ---------------------------------------------------------------------------
-- Per-type ACLs (Phase 1.17.F-bis)
-- ---------------------------------------------------------------------------

-- name: ListAcls :many
-- Every ACL row attached to one asset_type, deterministic ordering so
-- the editor UI shows a stable list across reloads.
SELECT asset_type_ref,
       principal_type,
       principal_id,
       permission,
       granted_at,
       granted_by_user_ref,
       expires_at
FROM asset_type_acls
WHERE asset_type_ref = $1
ORDER BY principal_type, principal_id, permission;

-- name: InsertAcl :exec
-- Upsert an ACL row. The PRIMARY KEY (ref, principal_type, principal_id,
-- permission) makes repeats idempotent — re-granting refreshes
-- granted_at + granter + expires_at, mirroring user_capability_grants.
INSERT INTO asset_type_acls (
    asset_type_ref, principal_type, principal_id, permission,
    granted_by_user_ref, expires_at
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (asset_type_ref, principal_type, principal_id, permission) DO UPDATE SET
    granted_at            = NOW(),
    granted_by_user_ref = EXCLUDED.granted_by_user_ref,
    expires_at            = EXCLUDED.expires_at;

-- name: DeleteAcl :execrows
-- Composite-key delete. The four-tuple is the PK — no surrogate ID
-- exists. Returns rows-affected so the handler can 404 cleanly when
-- the caller asks to remove a grant that isn't there.
DELETE FROM asset_type_acls
WHERE asset_type_ref = $1
  AND principal_type = $2
  AND principal_id   = $3
  AND permission     = $4;

-- name: ListUnauthorisedTypeRefsForUser :many
-- Returns the set of asset_type_refs the given user does NOT have
-- read access to. Used by ListAssetTypes to filter the catalogue for
-- non-admin callers — admins (system.admin) skip this query entirely.
--
-- A type appears in the result iff it has at least one ACL row
-- (= "restricted") AND no non-expired ACL row matches the user. The
-- match walks the user's direct user_role assignments and direct
-- team memberships; any of read/write/admin counts as read (higher
-- permissions implicitly include lower).
--
-- For anonymous callers (no user_ref) pass 0 — the join still works
-- because no user_role or team_membership row has user_ref=0, so
-- every restricted type ends up in the unauthorised set.
WITH user_role_ids AS (
    SELECT ur.role_id::text AS rid
      FROM user_roles ur
     WHERE ur.user_ref = sqlc.arg(user_ref)::bigint
),
user_team_ids AS (
    SELECT tm.team_id::text AS tid
      FROM team_memberships tm
     WHERE tm.user_ref = sqlc.arg(user_ref)::bigint
),
restricted_types AS (
    SELECT DISTINCT asset_type_ref FROM asset_type_acls
),
allowed_types AS (
    SELECT DISTINCT a.asset_type_ref
    FROM asset_type_acls a
    WHERE (a.expires_at IS NULL OR a.expires_at > NOW())
      AND (
            (a.principal_type = 'user' AND a.principal_id = sqlc.arg(user_ref)::bigint::text)
         OR (a.principal_type = 'role' AND a.principal_id IN (SELECT rid FROM user_role_ids))
         OR (a.principal_type = 'team' AND a.principal_id IN (SELECT tid FROM user_team_ids))
      )
)
SELECT asset_type_ref FROM restricted_types
WHERE asset_type_ref NOT IN (SELECT asset_type_ref FROM allowed_types);

-- name: HasAssetTypeAccess :one
-- Returns true when the given user holds at least the requested
-- permission on the asset_type. 'admin' implies 'write' and 'read';
-- 'write' implies 'read'. The caller is responsible for short-circuiting
-- the system.admin cap before invoking this — admins bypass the ACL.
--
-- Used by the upload + asset-list handlers (follow-up commit) to gate
-- per-type operations.
--
-- Params: @user_ref, @asset_type_ref, @permission.
WITH user_role_ids AS (
    SELECT ur.role_id::text AS rid
      FROM user_roles ur
     WHERE ur.user_ref = sqlc.arg(user_ref)::bigint
),
user_team_ids AS (
    SELECT tm.team_id::text AS tid
      FROM team_memberships tm
     WHERE tm.user_ref = sqlc.arg(user_ref)::bigint
)
SELECT EXISTS (
    SELECT 1
    FROM asset_type_acls a
    WHERE a.asset_type_ref = sqlc.arg(asset_type_ref)::bigint
      AND (a.expires_at IS NULL OR a.expires_at > NOW())
      AND (
          a.permission = sqlc.arg(permission)::text
          OR (a.permission = 'admin')
          OR (a.permission = 'write' AND sqlc.arg(permission)::text = 'read')
      )
      AND (
          (a.principal_type = 'user' AND a.principal_id = sqlc.arg(user_ref)::bigint::text)
       OR (a.principal_type = 'role' AND a.principal_id IN (SELECT rid FROM user_role_ids))
       OR (a.principal_type = 'team' AND a.principal_id IN (SELECT tid FROM user_team_ids))
      )
) AS allowed;
