-- Phase 1.53.A — MCP server registration + tool grants CRUD.
--
-- Operator surface: add a server, whitelist its tools (with per-tool
-- capability + cost estimate), flip the server on. Health-check
-- goroutine + dispatcher consume the read queries; the admin UI
-- consumes the CRUD set.

-- name: ListServers :many
-- All servers, ordered by name for stable admin-UI rendering.
SELECT id, name, url, transport, auth_kind, auth_secret_ref,
       auth_header_name, privacy_class, enabled,
       rate_limit_per_second, rate_limit_per_minute,
       health_check_interval_s, last_health_check_at,
       last_health_status, last_health_error,
       created_at, updated_at, registered_by_user_ref
FROM mcp_server_registration
ORDER BY name ASC;

-- name: ListEnabledServers :many
-- Hot-path query for the dispatcher + the health-check pool's
-- enumeration. Backed by the partial index idx_mcp_server_enabled.
SELECT id, name, url, transport, auth_kind, auth_secret_ref,
       auth_header_name, privacy_class, enabled,
       rate_limit_per_second, rate_limit_per_minute,
       health_check_interval_s, last_health_check_at,
       last_health_status, last_health_error,
       created_at, updated_at, registered_by_user_ref
FROM mcp_server_registration
WHERE enabled = TRUE
ORDER BY name ASC;

-- name: GetServerByID :one
SELECT id, name, url, transport, auth_kind, auth_secret_ref,
       auth_header_name, privacy_class, enabled,
       rate_limit_per_second, rate_limit_per_minute,
       health_check_interval_s, last_health_check_at,
       last_health_status, last_health_error,
       created_at, updated_at, registered_by_user_ref
FROM mcp_server_registration
WHERE id = $1;

-- name: GetServerByName :one
SELECT id, name, url, transport, auth_kind, auth_secret_ref,
       auth_header_name, privacy_class, enabled,
       rate_limit_per_second, rate_limit_per_minute,
       health_check_interval_s, last_health_check_at,
       last_health_status, last_health_error,
       created_at, updated_at, registered_by_user_ref
FROM mcp_server_registration
WHERE name = $1;

-- name: InsertServer :one
-- Create a new registration. UNIQUE(name) returns 23505 on duplicate;
-- the registry layer maps that to ErrDuplicateName.
INSERT INTO mcp_server_registration (
    name, url, transport, auth_kind, auth_secret_ref, auth_header_name,
    privacy_class, enabled, rate_limit_per_second, rate_limit_per_minute,
    health_check_interval_s, registered_by_user_ref
) VALUES (
    $1, $2, $3, $4,
    sqlc.narg('auth_secret_ref')::TEXT,
    sqlc.narg('auth_header_name')::TEXT,
    $5, $6, $7, $8, $9,
    sqlc.narg('registered_by_user_ref')::BIGINT
)
RETURNING id, name, url, transport, auth_kind, auth_secret_ref,
          auth_header_name, privacy_class, enabled,
          rate_limit_per_second, rate_limit_per_minute,
          health_check_interval_s, last_health_check_at,
          last_health_status, last_health_error,
          created_at, updated_at, registered_by_user_ref;

-- name: UpdateServer :one
-- Partial update via COALESCE on every editable field. NULL inputs
-- keep the existing value. Health-check fields are NOT editable here
-- (they're owned by the health-check goroutine via UpdateHealthStatus
-- below).
UPDATE mcp_server_registration SET
    url                     = COALESCE(sqlc.narg('url'),                    url),
    transport               = COALESCE(sqlc.narg('transport'),              transport),
    auth_kind               = COALESCE(sqlc.narg('auth_kind'),              auth_kind),
    auth_secret_ref         = COALESCE(sqlc.narg('auth_secret_ref'),        auth_secret_ref),
    auth_header_name        = COALESCE(sqlc.narg('auth_header_name'),       auth_header_name),
    privacy_class           = COALESCE(sqlc.narg('privacy_class'),          privacy_class),
    enabled                 = COALESCE(sqlc.narg('enabled'),                enabled),
    rate_limit_per_second   = COALESCE(sqlc.narg('rate_limit_per_second'),  rate_limit_per_second),
    rate_limit_per_minute   = COALESCE(sqlc.narg('rate_limit_per_minute'),  rate_limit_per_minute),
    health_check_interval_s = COALESCE(sqlc.narg('health_check_interval_s'),health_check_interval_s),
    updated_at              = NOW()
WHERE id = sqlc.arg('id')
RETURNING id, name, url, transport, auth_kind, auth_secret_ref,
          auth_header_name, privacy_class, enabled,
          rate_limit_per_second, rate_limit_per_minute,
          health_check_interval_s, last_health_check_at,
          last_health_status, last_health_error,
          created_at, updated_at, registered_by_user_ref;

-- name: DeleteServer :exec
-- Cascades to mcp_server_tool_grant via the FK ON DELETE CASCADE.
DELETE FROM mcp_server_registration WHERE id = $1;

-- name: UpdateHealthStatus :exec
-- Called by the health-check goroutine after each poll. Last-error
-- is set when status != 'healthy'; otherwise cleared so the admin UI
-- doesn't show a stale failure after recovery.
UPDATE mcp_server_registration
SET last_health_check_at = NOW(),
    last_health_status   = $2,
    last_health_error    = sqlc.narg('last_health_error')::TEXT,
    updated_at           = NOW()
WHERE id = $1;

-- ---------------------------------------------------------------------------
-- Tool grants
-- ---------------------------------------------------------------------------

-- name: ListToolGrants :many
-- All grants for a given server (the dispatcher reads this on every
-- invoke; the cache layer in cache.go absorbs the repeat reads).
SELECT server_id, tool_name, additional_capability,
       cost_estimate_micros, enabled
FROM mcp_server_tool_grant
WHERE server_id = $1
ORDER BY tool_name ASC;

-- name: GetToolGrant :one
SELECT server_id, tool_name, additional_capability,
       cost_estimate_micros, enabled
FROM mcp_server_tool_grant
WHERE server_id = $1 AND tool_name = $2;

-- name: UpsertToolGrant :one
-- Insert or replace a single tool grant. Used by the admin UI's
-- per-tool editor.
INSERT INTO mcp_server_tool_grant
    (server_id, tool_name, additional_capability, cost_estimate_micros, enabled)
VALUES
    ($1, $2,
     sqlc.narg('additional_capability')::TEXT,
     $3, $4)
ON CONFLICT (server_id, tool_name) DO UPDATE
    SET additional_capability = EXCLUDED.additional_capability,
        cost_estimate_micros  = EXCLUDED.cost_estimate_micros,
        enabled               = EXCLUDED.enabled
RETURNING server_id, tool_name, additional_capability,
          cost_estimate_micros, enabled;

-- name: DeleteToolGrant :exec
DELETE FROM mcp_server_tool_grant
WHERE server_id = $1 AND tool_name = $2;
