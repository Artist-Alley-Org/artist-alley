-- #794 — operator overrides of shipped UI strings (ADR 0081 §1).
--
-- Three queries, one row per (key, language). There is deliberately no
-- "get one override" query: the resolver reads the whole map, caches it
-- under a single key and rebuilds it wholesale on invalidation, so a
-- per-key read would only ever serve a cache the caller already holds.

-- name: ListSiteText :many
-- The whole override map, in a stable order so the admin list and the
-- cached payload agree without a second sort.
SELECT key, language, value, updated_at, updated_by_user_ref
FROM site_text
ORDER BY key, language;

-- name: UpsertSiteText :one
-- Per-row upsert. This is the reason site_text is a table rather than a
-- jsonb document in system_config: two operators editing two different
-- strings touch two different primary keys and cannot lose each other's
-- write (cf. #737's whole-document race).
INSERT INTO site_text (key, language, value, updated_by_user_ref)
VALUES ($1, $2, $3, sqlc.narg('updated_by_user_ref')::bigint)
ON CONFLICT (key, language) DO UPDATE
    SET value               = EXCLUDED.value,
        updated_at          = now(),
        updated_by_user_ref = EXCLUDED.updated_by_user_ref
RETURNING key, language, value, updated_at, updated_by_user_ref;

-- name: DeleteSiteText :execrows
-- Revert to the shipped string. Returns rows-affected so the handler
-- can map 0 → 404 rather than reporting a delete that removed nothing.
DELETE FROM site_text WHERE key = $1 AND language = $2;
