-- #795 — operator-authored email templates (ADR 0081 §2 as amended).
--
-- Three queries, one row per (template_name, part). There is
-- deliberately no "get one override" query: the resolver reads every
-- override, caches it under a single key and rebuilds it wholesale on
-- invalidation, so a per-row read would only ever serve a cache the
-- caller already holds (the site_text posture).

-- name: ListEmailTemplateOverrides :many
-- Every override, in a stable order so the admin surface and the
-- cached payload agree without a second sort.
SELECT template_name, part, body, updated_at, updated_by_user_ref
FROM email_template
ORDER BY template_name, part;

-- name: UpsertEmailTemplateOverride :one
-- Per-row upsert. This is the reason email_template is a table rather
-- than a jsonb document: two operators editing two different parts
-- touch two different primary keys and cannot lose each other's write
-- (cf. #737's whole-document race).
INSERT INTO email_template (template_name, part, body, updated_by_user_ref)
VALUES ($1, $2, $3, sqlc.narg('updated_by_user_ref')::bigint)
ON CONFLICT (template_name, part) DO UPDATE
    SET body                = EXCLUDED.body,
        updated_at          = now(),
        updated_by_user_ref = EXCLUDED.updated_by_user_ref
RETURNING template_name, part, body, updated_at, updated_by_user_ref;

-- name: DeleteEmailTemplateOverride :execrows
-- Revert to the shipped template. Returns rows-affected so the handler
-- can map 0 → 404 rather than reporting a delete that removed nothing.
DELETE FROM email_template WHERE template_name = $1 AND part = $2;
