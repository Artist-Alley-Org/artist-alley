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

-- name: FindUserByRef :one
-- Used by the registry-dispatched login flow after a provider has
-- resolved credentials to a local user ref. Same shape as the
-- username + session lookups so the handler's downstream code
-- (approval gate, session minting) is provider-agnostic.
SELECT ref,
       username,
       fullname,
       email,
       usergroup,
       approved,
       account_expires
FROM "user"
WHERE ref = $1
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

-- ---------------------------------------------------------------------------
-- sessions table (Phase 1.5)
-- ---------------------------------------------------------------------------

-- name: InsertSession :one
-- Records a fresh login. token_hash is sha256(cookie_value); the plaintext
-- never reaches the DB. ip/user_agent are best-effort observability.
INSERT INTO sessions (user_ref, token_hash, expires_at, ip, user_agent)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, user_ref, created_at, last_used_at, expires_at;

-- name: FindActiveSession :one
-- Resolves an incoming cookie. Only returns rows that haven't been revoked
-- and haven't passed their hard expiry. The idle-timeout check lives in
-- code so the cutoff is configurable per request.
SELECT s.id,
       s.user_ref,
       s.created_at,
       s.last_used_at,
       s.expires_at,
       s.ip,
       s.user_agent
FROM sessions s
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL
  AND (s.expires_at IS NULL OR s.expires_at > NOW())
LIMIT 1;

-- name: TouchSession :exec
-- Updates last_used_at on each authenticated request. Cheap and safe
-- to call on every hit; the index on last_used_at is partial-on-active.
UPDATE sessions
SET last_used_at = NOW()
WHERE id = $1;

-- name: RevokeSession :exec
-- Soft-deletes a session by id. Idempotent.
UPDATE sessions
SET revoked_at = NOW()
WHERE id = $1
  AND revoked_at IS NULL;

-- name: RevokeSessionForUser :execrows
-- Ownership-checked soft-delete. Same as RevokeSession but the
-- WHERE includes user_ref so a caller can't revoke someone else's
-- session by ID-guessing. Used by the self-service
-- DELETE /account/sessions/{id} endpoint. Returns rows-affected so
-- the handler can distinguish "revoked" (1) from "not yours / not
-- found / already revoked" (0).
UPDATE sessions
SET revoked_at = NOW()
WHERE id = $1
  AND user_ref = $2
  AND revoked_at IS NULL;

-- name: RevokeSessionByToken :exec
-- Revoke by cookie hash. Used by /auth/logout when we have the cookie
-- but no session id loaded.
UPDATE sessions
SET revoked_at = NOW()
WHERE token_hash = $1
  AND revoked_at IS NULL;

-- name: ListUserGrants :many
-- Per-user capability grants (Phase 1.17.F). Returns rows ordered
-- by (cap, team_id) so the UI displays a stable list across reloads.
SELECT g.capability_code,
       g.team_id,
       g.granted_at,
       g.granted_by_user_ref,
       g.note,
       t.name AS team_name
FROM user_capability_grants g
LEFT JOIN teams t ON t.id = g.team_id
WHERE g.user_ref = $1
ORDER BY g.capability_code, g.team_id NULLS FIRST;

-- name: ListUserRevokes :many
-- Per-user capability revokes (subtractive overrides). Same shape
-- as ListUserGrants — front-end renders both lists in one section.
SELECT r.capability_code,
       r.team_id,
       r.revoked_at,
       r.revoked_by_user_ref,
       r.note,
       t.name AS team_name
FROM user_capability_revokes r
LEFT JOIN teams t ON t.id = r.team_id
WHERE r.user_ref = $1
ORDER BY r.capability_code, r.team_id NULLS FIRST;

-- name: InsertUserGrant :exec
-- Upsert a grant. The UNIQUE NULLS NOT DISTINCT (user_ref, cap,
-- team_id) means re-granting the same (cap, team_id) is a no-op
-- update of granted_at + note + granter — useful when an admin
-- refreshes a stale grant.
INSERT INTO user_capability_grants (
    user_ref, capability_code, team_id, granted_by_user_ref, note
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_ref, capability_code, team_id) DO UPDATE SET
    granted_at = NOW(),
    granted_by_user_ref = EXCLUDED.granted_by_user_ref,
    note = EXCLUDED.note;

-- name: DeleteUserGrant :execrows
-- Ownership-checked delete. The (user_ref, cap, team_id) tuple is
-- the natural key — team_id may be NULL (global grant). Returns
-- rows-affected so the handler can 404 cleanly.
DELETE FROM user_capability_grants
WHERE user_ref = $1
  AND capability_code = $2
  AND team_id IS NOT DISTINCT FROM $3;

-- name: InsertUserRevoke :exec
INSERT INTO user_capability_revokes (
    user_ref, capability_code, team_id, revoked_by_user_ref, note
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_ref, capability_code, team_id) DO UPDATE SET
    revoked_at = NOW(),
    revoked_by_user_ref = EXCLUDED.revoked_by_user_ref,
    note = EXCLUDED.note;

-- name: DeleteUserRevoke :execrows
DELETE FROM user_capability_revokes
WHERE user_ref = $1
  AND capability_code = $2
  AND team_id IS NOT DISTINCT FROM $3;

-- name: RevokeOtherSessionsForUser :execrows
-- Revokes every session belonging to a user EXCEPT the one passed
-- as $2. Used by the self-service password-change endpoint when the
-- caller opts to "sign out everywhere else" — defensive default for
-- "I think someone got my password" recovery flows. Returns count
-- so the audit row + UI can report how many sessions ended.
UPDATE sessions
SET revoked_at = NOW()
WHERE user_ref = $1
  AND id <> $2
  AND revoked_at IS NULL;

-- name: GetUserPasswordHashByRef :one
-- Returns just the username + stored hash for a user. Used by the
-- self-service password change endpoint to verify the caller knows
-- their CURRENT password before accepting a new one.
SELECT ref AS user_ref, username, password
FROM "user"
WHERE ref = $1;

-- name: UpdateUserPassword :exec
-- Atomic password change: sets user.password + bumps
-- password_last_change. The handler is responsible for the policy
-- check + the history-reuse check + the history insert; this query
-- is the leaf write.
UPDATE "user"
SET password = $2,
    password_last_change = NOW(),
    password_reset_hash = NULL
WHERE ref = $1;

-- name: InsertPasswordHistory :exec
-- Append-only history row. The handler calls this after every
-- successful UpdateUserPassword (whether self-service or admin
-- reset) so the reuse-prevention check has the data it needs.
INSERT INTO user_password_history (user_ref, password_hash)
VALUES ($1, $2);

-- name: ListRecentPasswordHashes :many
-- Most recent N hashes for reuse-prevention. The handler iterates +
-- VerifyPasswords against each — we can't WHERE on the hash directly
-- because RS-style hashing has a per-call HMAC step (the candidate
-- plaintext needs to be re-hashed and compared in code).
SELECT password_hash
FROM user_password_history
WHERE user_ref = $1
ORDER BY changed_at DESC
LIMIT $2;

-- name: ListSessionsForUser :many
-- Powers /auth/me/sessions. Returns active sessions ordered most recently
-- used first.
SELECT id,
       user_ref,
       created_at,
       last_used_at,
       expires_at,
       ip,
       user_agent
FROM sessions
WHERE user_ref = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > NOW())
ORDER BY last_used_at DESC;

-- ---------------------------------------------------------------------------
-- setup wizard (Phase 1.6.A)
-- ---------------------------------------------------------------------------

-- name: CountSystemAdmins :one
-- Returns the number of real (still-existing) users whose assigned role
-- grants system.admin. The join against "user" filters out dangling
-- user_roles rows left over from deleted users — the user table doesn't
-- cascade. Counts only global role assignments (team_id IS NULL);
-- team-scoped system.admin would be a misconfiguration anyway since
-- system.admin is a global wildcard.
SELECT COUNT(DISTINCT ur.user_ref)::BIGINT AS value
FROM user_roles ur
JOIN role_capabilities rc ON rc.role_id = ur.role_id
JOIN "user" u             ON u.ref     = ur.user_ref
WHERE rc.capability_code = 'system.admin'
  AND ur.team_id IS NULL;

-- name: FindRoleByName :one
-- Used by setup to look up the seeded "Admin" role without hardcoding
-- a UUID.
SELECT id, parent_id, name, description, origin_server_id, created_at, updated_at
FROM roles
WHERE name = $1
LIMIT 1;

-- name: CreateUser :one
-- Used by setup (initial admin) and later by the admin user-management
-- endpoints. usergroup is the RS-side permission group: while the Go
-- side authorises via roles+capabilities, RS-rendered pages still gate
-- on the `permissions` string of the assigned usergroup. Pass NULL to
-- omit (Go-only user); pass 3 for the seeded Super Admin row.
-- approved defaults to 1.
INSERT INTO "user" (username, password, fullname, email, usergroup, approved, lang)
VALUES ($1, $2, $3, $4, $5, 1, $6)
RETURNING ref, username, fullname, email, usergroup, created;

-- name: CreateApiToken :one
INSERT INTO api_tokens (user_ref, name, token_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, user_ref, name, scopes, expires_at, last_used_at, created_at;

-- name: FindActiveApiToken :one
-- Returns the matching token only if it is not revoked and not expired.
-- Used by the bearer-token middleware on every authenticated request.
SELECT id,
       user_ref,
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
       user_ref,
       name,
       scopes,
       expires_at,
       last_used_at,
       created_at
FROM api_tokens
WHERE user_ref = $1
  AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: RevokeApiToken :execrows
-- Returns the number of rows updated so the handler can tell 404 from
-- success without a separate SELECT.
UPDATE api_tokens
SET revoked_at = NOW()
WHERE id = $1
  AND user_ref = $2
  AND revoked_at IS NULL;

-- name: EffectiveCapabilitiesForUser :many
-- Returns the union of capabilities a user can exercise after all role
-- inheritance and per-user overrides resolve, for the *global* scope
-- (team_id IS NULL). The recursive CTE walks the role hierarchy from
-- every globally-assigned role, then unions in global grants and
-- removes global revokes.
--
-- Phase 1.7.B-3 adds a scoped resolver that consults team-scoped
-- assignments. This query intentionally ignores team_id so existing
-- handlers that only ask for global caps continue to get the right
-- answer with no behavioural change.
WITH RECURSIVE role_chain AS (
    SELECT r.id, r.parent_id, 0 AS depth
    FROM roles r
    JOIN user_roles ur ON ur.role_id = r.id
    WHERE ur.user_ref = $1
      AND ur.team_id IS NULL

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
    WHERE g.user_ref = $1
      AND g.team_id IS NULL
)
SELECT ac.code
FROM all_caps ac
WHERE ac.code NOT IN (
    SELECT v.capability_code
    FROM user_capability_revokes v
    WHERE v.user_ref = $1
      AND v.team_id IS NULL
)
ORDER BY ac.code;

-- name: EffectiveScopedCapabilitiesForUser :many
-- Returns (capability_code, team_id) tuples for every capability the
-- user can exercise. Used by the auth resolver to populate Identity
-- on every request.
--
-- The query handles both axes of ADR 0010 in one shot:
--   * Role inheritance — recursive CTE walks each role's parent chain
--     up to a depth cap; team_id is propagated through the chain so a
--     team-scoped role assignment scopes every inherited capability.
--   * Team scope expansion — scoped grants are pre-expanded via
--     team_closure (one row per descendant team). The resolver does
--     pure-memory flat lookups after this; no closure walk in Go.
--
-- team_id semantics:
--   * NULL row → global capability (allows any scope including
--     unscoped Can() checks).
--   * non-NULL row → capability is effective on the specific team
--     in that row (post-closure-expansion this includes all
--     descendant teams).
--
-- Revokes are matched with NULLs-not-distinct: a revoke at the same
-- (code, team_id) pair removes that effective row exactly. Revokes
-- DON'T expand through closure — they target the specific scope.
WITH RECURSIVE role_chain(role_id, team_id, depth) AS (
    -- Seed: every role assignment, preserving its team scope.
    SELECT ur.role_id, ur.team_id, 0
    FROM user_roles ur
    WHERE ur.user_ref = $1

    UNION ALL

    -- Walk: ancestor role inherits the seed assignment's team scope.
    SELECT r.parent_id, rc.team_id, rc.depth + 1
    FROM roles r
    JOIN role_chain rc ON r.id = rc.role_id
    WHERE r.parent_id IS NOT NULL AND rc.depth < 32
),
role_caps AS (
    SELECT rcap.capability_code AS code, rch.team_id AS team_id
    FROM role_chain rch
    JOIN role_capabilities rcap ON rcap.role_id = rch.role_id
),
grant_caps AS (
    SELECT g.capability_code AS code, g.team_id AS team_id
    FROM user_capability_grants g
    WHERE g.user_ref = $1
),
all_caps AS (
    SELECT code, team_id FROM role_caps
    UNION
    SELECT code, team_id FROM grant_caps
),
non_revoked AS (
    SELECT a.code, a.team_id
    FROM all_caps a
    WHERE NOT EXISTS (
        SELECT 1 FROM user_capability_revokes v
        WHERE v.user_ref = $1
          AND v.capability_code = a.code
          AND v.team_id IS NOT DISTINCT FROM a.team_id
    )
),
expanded AS (
    -- Global rows pass through unchanged.
    SELECT code, NULL::uuid AS team_id FROM non_revoked WHERE team_id IS NULL
    UNION
    -- Scoped rows fan out via team_closure (includes the team itself
    -- via the depth-0 self-row).
    SELECT nr.code, tc.descendant_id
    FROM non_revoked nr
    JOIN team_closure tc ON tc.ancestor_id = nr.team_id
    WHERE nr.team_id IS NOT NULL
)
SELECT code, team_id
FROM expanded
ORDER BY code, team_id NULLS FIRST;

-- name: EffectiveCapabilitiesForRoleName :many
-- Closure-walked capabilities for the named role, including everything
-- inherited from its parent chain. Used to populate the synthetic
-- Anonymous Identity that the middleware injects on unauthenticated
-- requests. No team scope (the Anonymous role isn't team-scoped by
-- design); the returned set is flat global codes.
WITH RECURSIVE role_chain AS (
    SELECT r.id, r.parent_id, 0 AS depth
    FROM roles r
    WHERE r.name = $1

    UNION ALL

    SELECT r.id, r.parent_id, rc.depth + 1
    FROM roles r
    JOIN role_chain rc ON r.id = rc.parent_id
    WHERE rc.depth < 32
)
SELECT DISTINCT rcap.capability_code AS code
FROM role_chain rch
JOIN role_capabilities rcap ON rcap.role_id = rch.id
ORDER BY code;

-- name: AssignedRolesForUser :many
-- Returns every role the user has been assigned, with the optional
-- team scope. NULL team_id = global assignment. Replaces the old
-- AssignedRoleForUser (:one) — multi-role per user is supported as of
-- migration 00016.
SELECT r.id, r.name, r.description, r.parent_id, ur.team_id
FROM roles r
JOIN user_roles ur ON ur.role_id = r.id
WHERE ur.user_ref = $1
ORDER BY ur.team_id NULLS FIRST, r.name;

-- name: SetUserGlobalRole :exec
-- Replaces the user's *global* role assignment(s) with the supplied
-- role. Preserves the "exactly one global role per user" admin UX
-- from the singular SetUserRole, while leaving any team-scoped
-- assignments untouched.
--
-- The DELETE-then-INSERT in a single statement-with-CTE is atomic at
-- the statement level (a single SQL statement runs in one snapshot),
-- so there's no window where the user has zero roles.
WITH _del AS (
    DELETE FROM user_roles
     WHERE user_ref = $1 AND team_id IS NULL
)
INSERT INTO user_roles (user_ref, role_id, assigned_by_user_ref)
VALUES ($1, $2, $3)
ON CONFLICT ON CONSTRAINT user_roles_unique DO UPDATE SET
    assigned_at            = NOW(),
    assigned_by_user_ref = EXCLUDED.assigned_by_user_ref;

-- name: ListCapabilities :many
SELECT code, description FROM capabilities ORDER BY code;

-- name: LoadCapabilityLicenseFeatures :many
-- Returns the (code, required_license_feature) pairs for every cap that
-- has a license-feature gate. Rows where the column is NULL are skipped
-- entirely — they don't need to live in the in-process map. The result
-- feeds auth.SetCapLicenseFeatures at boot, so Identity.Can() can check
-- the install's license without a per-call DB hit.
SELECT code, required_license_feature
FROM capabilities
WHERE required_license_feature IS NOT NULL
ORDER BY code;

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
