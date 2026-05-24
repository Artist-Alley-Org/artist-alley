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
