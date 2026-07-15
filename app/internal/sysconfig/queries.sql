-- name: GetSystemConfig :one
-- Returns the raw JSONB blob stored under key. Callers deserialize
-- into the typed struct that owns the key.
SELECT key, value, updated_at
FROM system_config
WHERE key = $1;

-- name: UpsertSystemConfig :exec
-- Idempotent write. updated_at refreshes on every write so audit and
-- monitoring can see when settings drift.
INSERT INTO system_config (key, value, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (key) DO UPDATE SET
    value      = EXCLUDED.value,
    updated_at = NOW();

-- name: DeleteSystemConfig :exec
DELETE FROM system_config WHERE key = $1;

-- name: ListSystemConfigByPrefix :many
-- Every config row whose key starts with the given literal prefix, in
-- key order. Used to load the scalar `jobs.type_concurrency.<type>`
-- caps, which are stored one-row-per-type rather than as a single JSON
-- blob. starts_with matches the prefix literally (no LIKE wildcards).
SELECT key, value
FROM system_config
WHERE starts_with(key, sqlc.arg('prefix')::TEXT)
ORDER BY key;
