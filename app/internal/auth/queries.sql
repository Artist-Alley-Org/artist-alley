-- name: FindUserByUsername :one
-- Used by /auth/login to verify credentials.
SELECT ref,
       username,
       password,
       fullname,
       email,
       usergroup,
       approved,
       account_expires,
       email_verified_at
FROM "user"
WHERE username = $1
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
       account_expires,
       email_verified_at
FROM "user"
WHERE ref = $1
LIMIT 1;

-- name: TouchUserLastActive :exec
-- Bumps last_active on the user row. Called by SessionManager.Issue
-- so downstream "active users" surfaces (admin list, leaderboards)
-- see fresh logins. Idempotent.
UPDATE "user"
SET last_active = NOW()
WHERE ref = $1;

-- ---------------------------------------------------------------------------
-- sessions table (Phase 1.5)
-- ---------------------------------------------------------------------------

-- name: InsertSession :one
-- Records a fresh login. token_hash is sha256(cookie_value); the plaintext
-- never reaches the DB. ip/user_agent are best-effort observability.
INSERT INTO sessions (user_ref, token_hash, expires_at, ip, user_agent)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, user_ref, created_at, last_used_at, expires_at;

-- name: InsertImpersonationSession :one
-- Phase 1.19.A-2. Issues a session bound to the target user but
-- attributed to the calling admin via impersonated_by_user_ref.
-- Caller passes a shorter expiry (default 30 min) so abandoned
-- impersonation sessions can't sit forever — distinct from the
-- normal login TTL.
INSERT INTO sessions (user_ref, token_hash, expires_at, ip, user_agent, impersonated_by_user_ref)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_ref, created_at, last_used_at, expires_at, impersonated_by_user_ref;

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
       s.user_agent,
       s.impersonated_by_user_ref
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

-- name: RevokeAllSessionsForUser :execrows
-- Phase 1.17.A — cascade revoke every active session for a user.
-- Fired by users.SetAdminUserStatus when a transition moves the
-- user OUT OF UserStateActive (disabled, archived, or the
-- should-never-happen active→pending). Returns rows-affected so
-- the audit log can record the cascade size.
UPDATE sessions
SET revoked_at = NOW()
WHERE user_ref = $1
  AND revoked_at IS NULL;

-- name: ListUserGrants :many
-- Per-user capability grants. Returns rows ordered by
-- (cap, team_id) so the UI displays a stable list across reloads.
-- expires_at (Phase 1.17.C) is the optional time-bound expiry —
-- NULL = permanent.
SELECT g.capability_code,
       g.team_id,
       g.granted_at,
       g.granted_by_user_ref,
       g.note,
       g.expires_at,
       t.name AS team_name
FROM user_capability_grants g
LEFT JOIN teams t ON t.id = g.team_id
WHERE g.user_ref = $1
ORDER BY g.capability_code, g.team_id NULLS FIRST;

-- name: ListUserRevokes :many
-- Per-user capability revokes (subtractive overrides). Same shape
-- as ListUserGrants — front-end renders both lists in one section.
-- expires_at (Phase 1.17.C) — NULL = permanent.
SELECT r.capability_code,
       r.team_id,
       r.revoked_at,
       r.revoked_by_user_ref,
       r.note,
       r.expires_at,
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
--
-- Phase 1.17.C — expires_at is the optional time-bound expiry
-- (NULL = permanent). The background sweeper
-- (auth/capability_sweeper.go) reaps rows past expires_at on a
-- fixed cadence.
INSERT INTO user_capability_grants (
    user_ref, capability_code, team_id, granted_by_user_ref, note, expires_at
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_ref, capability_code, team_id) DO UPDATE SET
    granted_at = NOW(),
    granted_by_user_ref = EXCLUDED.granted_by_user_ref,
    note = EXCLUDED.note,
    expires_at = EXCLUDED.expires_at;

-- name: DeleteUserGrant :execrows
-- Ownership-checked delete. The (user_ref, cap, team_id) tuple is
-- the natural key — team_id may be NULL (global grant). Returns
-- rows-affected so the handler can 404 cleanly.
DELETE FROM user_capability_grants
WHERE user_ref = $1
  AND capability_code = $2
  AND team_id IS NOT DISTINCT FROM $3;

-- name: InsertUserRevoke :exec
-- Phase 1.17.C — expires_at is the optional time-bound expiry on
-- the REVOKE side (same NULL-is-permanent convention as grants).
INSERT INTO user_capability_revokes (
    user_ref, capability_code, team_id, revoked_by_user_ref, note, expires_at
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_ref, capability_code, team_id) DO UPDATE SET
    revoked_at = NOW(),
    revoked_by_user_ref = EXCLUDED.revoked_by_user_ref,
    note = EXCLUDED.note,
    expires_at = EXCLUDED.expires_at;

-- name: DeleteUserRevoke :execrows
DELETE FROM user_capability_revokes
WHERE user_ref = $1
  AND capability_code = $2
  AND team_id IS NOT DISTINCT FROM $3;

-- name: SweepExpiredGrants :many
-- Phase 1.17.C — used by capability_sweeper.go for non-protected
-- grants only. Deletes every non-system.admin grant past its
-- expires_at and returns the reaped rows so the sweeper can emit
-- a per-row audit event + invalidate the affected user's
-- capability cache.
--
-- system.admin global grants are EXCLUDED from this bulk sweep
-- and handled separately via ListExpiredAdminGrants +
-- DeleteUserGrant per-row so the sweeper can enforce the
-- last-admin invariant before each reap. Team-scoped
-- system.admin grants (team_id IS NOT NULL) don't affect the
-- global admin count, so they bulk-sweep here.
DELETE FROM user_capability_grants
WHERE expires_at IS NOT NULL AND expires_at < NOW()
  AND NOT (capability_code = 'system.admin' AND team_id IS NULL)
RETURNING user_ref, capability_code, team_id, expires_at, request_ref;

-- name: ListExpiredAdminGrants :many
-- Phase 1.17.C — used by capability_sweeper.go for the last-
-- admin-protected sweep. Returns expired GLOBAL system.admin
-- grant candidates so the sweeper can per-row check
-- CountActiveAdminsIfRowRemoved and skip rows whose reap would
-- leave the system with zero active admins (logging a "stuck
-- open" WARN so the operator notices).
SELECT user_ref, capability_code, team_id, expires_at, request_ref
FROM user_capability_grants
WHERE expires_at IS NOT NULL AND expires_at < NOW()
  AND capability_code = 'system.admin'
  AND team_id IS NULL;

-- name: SweepExpiredRevokes :many
-- Same as SweepExpiredGrants but for revokes — same audit +
-- cache contract.
DELETE FROM user_capability_revokes
WHERE expires_at IS NOT NULL AND expires_at < NOW()
RETURNING user_ref, capability_code, team_id, expires_at;

-- name: CountActiveAdminsIfRowRemoved :one
-- Phase 1.17.C sweeper-time guard. Returns the system.admin
-- holder count AFTER speculatively removing the row identified by
-- (userRef, 'system.admin', team_id IS NULL). Used by the
-- sweeper to refuse to reap a system.admin grant whose expiry
-- would leave the system with zero active admins — the sweeper
-- logs the "stuck open" grant and leaves the row in place so the
-- operator can extend or replace it.
--
-- Mirrors CountSystemAdmins's logic but excludes the candidate
-- row from the union.
WITH admin_candidates AS (
    SELECT u.ref
    FROM user_roles ur
    JOIN role_capabilities rc ON rc.role_id = ur.role_id
    JOIN "user" u             ON u.ref     = ur.user_ref
    WHERE rc.capability_code = 'system.admin'
      AND ur.team_id IS NULL
      AND u.approved = 1
    UNION
    SELECT u.ref
    FROM user_capability_grants g
    JOIN "user" u ON u.ref = g.user_ref
    WHERE g.capability_code = 'system.admin'
      AND g.team_id IS NULL
      AND u.approved = 1
      AND NOT (g.user_ref = $1)
)
SELECT COUNT(*)::BIGINT AS value
FROM admin_candidates c
WHERE NOT EXISTS (
    SELECT 1 FROM user_capability_revokes r
    WHERE r.user_ref = c.ref
      AND r.capability_code = 'system.admin'
      AND r.team_id IS NULL
);

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
-- because the legacy-style hashing has a per-call HMAC step (the candidate
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
-- Returns the number of APPROVED users who currently hold the
-- system.admin capability, via EITHER a global role assignment
-- whose role grants it OR an explicit grant — minus anyone with
-- an explicit revoke (which nullifies role-derived + grant-
-- derived powers per the user_capability_revokes contract).
--
-- The DISTINCT + UNION + LEFT JOIN shape is the full identity
-- resolution mirror — same logic the Identity.Can check uses.
-- Used by the bootstrap package (to skip on re-runs) AND by the
-- last-admin invariant guard in the admin user-mutation paths.
WITH admin_candidates AS (
    SELECT u.ref
    FROM user_roles ur
    JOIN role_capabilities rc ON rc.role_id = ur.role_id
    JOIN "user" u             ON u.ref     = ur.user_ref
    WHERE rc.capability_code = 'system.admin'
      AND ur.team_id IS NULL
      AND u.approved = 1
    UNION
    SELECT u.ref
    FROM user_capability_grants g
    JOIN "user" u ON u.ref = g.user_ref
    WHERE g.capability_code = 'system.admin'
      AND g.team_id IS NULL
      AND u.approved = 1
)
SELECT COUNT(*)::BIGINT AS value
FROM admin_candidates c
WHERE NOT EXISTS (
    SELECT 1 FROM user_capability_revokes r
    WHERE r.user_ref = c.ref
      AND r.capability_code = 'system.admin'
      AND r.team_id IS NULL
);

-- name: RoleGrantsSystemAdmin :one
-- Returns 1 when the role's capability set includes
-- system.admin (directly OR via parent inheritance is NOT
-- considered — system.admin is always direct in v1).
SELECT COUNT(*)::BIGINT AS value
FROM role_capabilities
WHERE role_id = $1 AND capability_code = 'system.admin';

-- name: UserHoldsSystemAdmin :one
-- Returns 1 when the supplied user currently holds system.admin
-- per the full resolution above, 0 otherwise. Used by the last-
-- admin invariant guard: combined with CountSystemAdmins == 1
-- it tells the guard "this user IS the last admin" so the
-- guarded operation can refuse with a clear error.
SELECT COUNT(*)::BIGINT AS value
FROM (
    SELECT u.ref
    FROM user_roles ur
    JOIN role_capabilities rc ON rc.role_id = ur.role_id
    JOIN "user" u             ON u.ref     = ur.user_ref
    WHERE rc.capability_code = 'system.admin'
      AND ur.team_id IS NULL
      AND u.approved = 1
      AND u.ref = $1
    UNION
    SELECT u.ref
    FROM user_capability_grants g
    JOIN "user" u ON u.ref = g.user_ref
    WHERE g.capability_code = 'system.admin'
      AND g.team_id IS NULL
      AND u.approved = 1
      AND u.ref = $1
) AS holders
WHERE NOT EXISTS (
    SELECT 1 FROM user_capability_revokes r
    WHERE r.user_ref = $1
      AND r.capability_code = 'system.admin'
      AND r.team_id IS NULL
);

-- name: FindRoleByName :one
-- Used by setup to look up the seeded "Admin" role without hardcoding
-- a UUID.
SELECT id, parent_id, name, description, origin_server_id, created_at, updated_at
FROM roles
WHERE name = $1
LIMIT 1;

-- name: CreateUser :one
-- Used by setup (initial admin) and later by the admin user-management
-- endpoints. usergroup is the legacy-side permission group: while the Go
-- side authorises via roles+capabilities, legacy-rendered pages still gate
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

-- ---------------------------------------------------------------------------
-- Phase 1.19.B — self-service TOTP (RFC 6238 2FA)
-- ---------------------------------------------------------------------------

-- name: GetUserTOTP :one
-- Returns the user's TOTP row (or pgx.ErrNoRows when not enrolled).
-- Caller decrypts secret_enc via atrest before passing to totp.Verify.
SELECT user_ref, secret_enc, confirmed_at, created_at, last_used_at
FROM user_totp
WHERE user_ref = $1;

-- name: UpsertUserTOTP :exec
-- Used on enroll AND re-enroll. confirmed_at intentionally reset to
-- NULL so a re-enroll re-proves the secret before it gates login.
INSERT INTO user_totp (user_ref, secret_enc, confirmed_at, created_at)
VALUES ($1, $2, NULL, NOW())
ON CONFLICT (user_ref) DO UPDATE
   SET secret_enc   = EXCLUDED.secret_enc,
       confirmed_at = NULL,
       created_at   = NOW(),
       last_used_at = NULL;

-- name: ConfirmUserTOTP :exec
-- Flips confirmed_at to NOW on the first valid verify. Once set, the
-- login flow refuses password-only authentication for this user.
UPDATE user_totp
   SET confirmed_at = NOW()
 WHERE user_ref = $1
   AND confirmed_at IS NULL;

-- name: TouchUserTOTP :exec
-- Records last successful TOTP verification — surfaced on the
-- /account/security page so the user sees their authenticator is
-- still in use.
UPDATE user_totp
   SET last_used_at = NOW()
 WHERE user_ref = $1;

-- name: DeleteUserTOTP :exec
-- Disable 2FA wholesale. Also cascades the recovery codes via the
-- FK ON DELETE CASCADE — see migration 00018.
DELETE FROM user_totp WHERE user_ref = $1;

-- name: InsertRecoveryCode :exec
-- One row per backup code. code_hash is sha256 of the normalized
-- plaintext; the plaintext is shown to the user once and never
-- reachable again.
INSERT INTO user_totp_recovery_code (user_ref, code_hash) VALUES ($1, $2);

-- name: DeleteRecoveryCodesForUser :exec
-- Wipes the prior batch — called by the regenerate path before
-- inserting fresh codes so the user sees exactly N unused codes.
DELETE FROM user_totp_recovery_code WHERE user_ref = $1;

-- name: FindUnusedRecoveryCodeByHash :one
-- Returns the row id when a hash matches an unused recovery code
-- for the user. Caller then marks it used.
SELECT id FROM user_totp_recovery_code
 WHERE user_ref = $1
   AND code_hash = $2
   AND used_at IS NULL
 LIMIT 1;

-- name: MarkRecoveryCodeUsed :exec
UPDATE user_totp_recovery_code
   SET used_at = NOW()
 WHERE id = $1
   AND used_at IS NULL;

-- name: CountUnusedRecoveryCodes :one
SELECT COUNT(*) FROM user_totp_recovery_code
 WHERE user_ref = $1 AND used_at IS NULL;

-- ---------------------------------------------------------------------------
-- Phase 1.19.C — self-service registration + email verification
-- ---------------------------------------------------------------------------

-- name: InsertEmailVerificationToken :exec
-- Persists a token-hash for a freshly-generated verification link.
-- The plaintext token only exists in the email body; the server
-- only ever sees its sha256 hash.
INSERT INTO email_verification_token (user_ref, token_hash, purpose, expires_at)
VALUES ($1, $2, $3, $4);

-- name: FindActiveEmailVerificationToken :one
-- Verifies one incoming link click. Returns the row id + user
-- when the hash matches an unconsumed, unexpired row. Caller
-- marks consumed_at + flips user.email_verified_at in one tx.
SELECT id, user_ref, purpose
  FROM email_verification_token
 WHERE token_hash = $1
   AND consumed_at IS NULL
   AND expires_at > NOW()
 LIMIT 1;

-- name: ConsumeEmailVerificationToken :exec
UPDATE email_verification_token
   SET consumed_at = NOW()
 WHERE id = $1
   AND consumed_at IS NULL;

-- name: MarkUserEmailVerified :exec
-- Idempotent — re-applying is a no-op.
UPDATE "user"
   SET email_verified_at = NOW()
 WHERE ref = $1
   AND email_verified_at IS NULL;

-- name: DeleteExpiredEmailVerificationTokens :execrows
-- Sweeper-friendly cleanup of expired/consumed rows. The active-
-- index partial-WHERE-clause already keeps the hot path narrow;
-- this just bounds total table size.
DELETE FROM email_verification_token
 WHERE consumed_at IS NOT NULL OR expires_at < NOW() - INTERVAL '7 days';

-- name: FindUserByEmail :one
-- Used by the resend-verification path so a logged-out caller
-- can re-request their link by email instead of having to
-- remember which exact username they signed up with.
SELECT ref, username, email, email_verified_at, password
  FROM "user"
 WHERE LOWER(email) = LOWER($1)
 LIMIT 1;

-- name: CreateUserForRegistration :one
-- Inserts a freshly-registered user with the standard "approved=1
-- + email_verified_at IS NULL" shape Phase 1.19.C expects. Done
-- as a typed query (not the catch-all CreateUser) so the column
-- defaults are explicit + the audit row in commit 2 captures
-- "self_registered" intent.
INSERT INTO "user" (username, password, email, fullname, approved, email_verified_at)
VALUES ($1, $2, $3, $4, 1, NULL)
RETURNING ref, username, email;
