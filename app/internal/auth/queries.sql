-- name: FindUserByUsername :one
-- Used by /auth/login to verify credentials.
SELECT ref,
       username,
       password,
       fullname,
       email,
       usergroup,
       approved,
       account_expires
FROM "user"
WHERE username = $1
LIMIT 1;

-- name: FindUserBySession :one
-- Used by the session-cookie middleware to resolve rs_session -> user.
SELECT ref,
       username,
       fullname,
       email,
       usergroup,
       approved,
       account_expires
FROM "user"
WHERE session = $1
LIMIT 1;

-- name: SetUserSession :exec
-- Writes a freshly minted session token to the user's row. Also
-- bumps last_active so RS-side "active users" lists notice. Used at
-- the end of /auth/login.
UPDATE "user"
SET session     = $1,
    last_active = NOW(),
    logged_in   = 1
WHERE ref = $2;

-- name: ClearUserSession :exec
-- Used by /auth/logout. Idempotent: clearing an already-NULL session
-- is a no-op.
UPDATE "user"
SET session   = NULL,
    logged_in = 0
WHERE ref = $1;

-- name: ClearUserSessionByToken :exec
-- Same as ClearUserSession but matches on the cookie value rather than
-- the user id — used when we know the cookie but haven't resolved the
-- user (e.g., logout from an already-expired session).
UPDATE "user"
SET session   = NULL,
    logged_in = 0
WHERE session = $1;

-- name: CreateApiToken :one
INSERT INTO api_tokens (rs_user_id, name, token_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, rs_user_id, name, scopes, expires_at, last_used_at, created_at;

-- name: FindActiveApiToken :one
-- Returns the matching token only if it is not revoked and not expired.
-- Used by the bearer-token middleware on every authenticated request.
SELECT id,
       rs_user_id,
       name,
       scopes,
       expires_at,
       last_used_at,
       created_at
FROM api_tokens
WHERE token_hash = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > NOW())
LIMIT 1;

-- name: TouchApiToken :exec
-- Updates last_used_at. Best-effort; we don't block the request if this
-- fails. Driven by the bearer-token middleware on every authenticated
-- request.
UPDATE api_tokens
SET last_used_at = NOW()
WHERE id = $1;

-- name: ListApiTokensForUser :many
-- Lists the caller's tokens. Excludes revoked ones; expired ones are
-- still shown so the user can see why an old token stopped working.
SELECT id,
       rs_user_id,
       name,
       scopes,
       expires_at,
       last_used_at,
       created_at
FROM api_tokens
WHERE rs_user_id = $1
  AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: RevokeApiToken :execrows
-- Returns the number of rows updated so the handler can tell 404 from
-- success without a separate SELECT.
UPDATE api_tokens
SET revoked_at = NOW()
WHERE id = $1
  AND rs_user_id = $2
  AND revoked_at IS NULL;

-- name: EffectiveCapabilitiesForUser :many
-- Returns the union of capabilities a user can exercise after all role
-- inheritance and per-user overrides resolve. The recursive CTE walks
-- the role hierarchy from the user's assigned role up to the chain
-- root, then we union in explicit grants and remove explicit revokes.
WITH RECURSIVE role_chain AS (
    SELECT r.id, r.parent_id, 0 AS depth
    FROM roles r
    JOIN user_role ur ON ur.role_id = r.id
    WHERE ur.rs_user_id = $1

    UNION ALL

    SELECT r.id, r.parent_id, rc.depth + 1
    FROM roles r
    JOIN role_chain rc ON r.id = rc.parent_id
    WHERE rc.depth < 32 -- belt + braces against accidental cycles
),
role_caps AS (
    SELECT DISTINCT rc.capability_code AS code
    FROM role_chain rch
    JOIN role_capabilities rc ON rc.role_id = rch.id
),
all_caps AS (
    SELECT code FROM role_caps
    UNION
    SELECT g.capability_code AS code
    FROM user_capability_grants g
    WHERE g.rs_user_id = $1
)
SELECT ac.code
FROM all_caps ac
WHERE ac.code NOT IN (
    SELECT v.capability_code
    FROM user_capability_revokes v
    WHERE v.rs_user_id = $1
)
ORDER BY ac.code;

-- name: AssignedRoleForUser :one
-- Returns the user's currently assigned role (id + name) or no rows.
SELECT r.id, r.name, r.description, r.parent_id
FROM roles r
JOIN user_role ur ON ur.role_id = r.id
WHERE ur.rs_user_id = $1;

-- name: SetUserRole :exec
-- Idempotent assign-or-overwrite. Used by the admin endpoint.
INSERT INTO user_role (rs_user_id, role_id, assigned_by_rs_user_id)
VALUES ($1, $2, $3)
ON CONFLICT (rs_user_id) DO UPDATE SET
    role_id                = EXCLUDED.role_id,
    assigned_at            = NOW(),
    assigned_by_rs_user_id = EXCLUDED.assigned_by_rs_user_id;

-- name: ListCapabilities :many
SELECT code, description FROM capabilities ORDER BY code;

-- name: ListRoles :many
-- Returns every role with its parent_id; the handler enriches each
-- with the list of role-attached capabilities via ListRoleCapabilities.
SELECT id, name, description, parent_id
FROM roles
ORDER BY name;

-- name: ListRoleCapabilities :many
-- Capabilities directly attached to a role (not including inherited).
SELECT capability_code FROM role_capabilities
WHERE role_id = $1
ORDER BY capability_code;

-- name: GetRole :one
SELECT id, name, description, parent_id FROM roles WHERE id = $1;
