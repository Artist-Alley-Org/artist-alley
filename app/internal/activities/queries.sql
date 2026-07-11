-- name: InsertActivity :one
-- Idempotent activity insert. ON CONFLICT DO NOTHING means
-- re-recording the same activity_uri (job retry, replay tool,
-- peer redelivery) is a no-op. The returning row is the row that
-- now exists in the table — either the freshly-inserted one or
-- the pre-existing one (because the second SELECT picks it up).
--
-- This is the single point where activities enter the ledger.
-- Every social action handler calls this in the same transaction
-- as its domain write per ADR 0044.
WITH ins AS (
    INSERT INTO activities (
        activity_uri,
        activity_type,
        actor_uri,
        actor_user_ref,
        object_uri,
        object_kind,
        object_local_id,
        target_uri,
        to_uris,
        cc_uris,
        bto_uris,
        bcc_uris,
        audience_uris,
        payload,
        source,
        published_at
    )
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
    ON CONFLICT (activity_uri) DO NOTHING
    RETURNING id, activity_uri, activity_type, actor_uri, actor_user_ref,
              object_uri, object_kind, object_local_id, target_uri,
              to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
              payload, signature_value, signature_pubkey, source,
              published_at, created_at
)
SELECT id, activity_uri, activity_type, actor_uri, actor_user_ref,
       object_uri, object_kind, object_local_id, target_uri,
       to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
       payload, signature_value, signature_pubkey, source,
       published_at, created_at
FROM ins
UNION ALL
SELECT id, activity_uri, activity_type, actor_uri, actor_user_ref,
       object_uri, object_kind, object_local_id, target_uri,
       to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
       payload, signature_value, signature_pubkey, source,
       published_at, created_at
FROM activities
WHERE activity_uri = $1
LIMIT 1;

-- name: GetActivityByURI :one
-- Look up an activity by its AP URI. Powers cross-package
-- consumers (federation outbox dispatcher, admin audit UI).
SELECT id, activity_uri, activity_type, actor_uri, actor_user_ref,
       object_uri, object_kind, object_local_id, target_uri,
       to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
       payload, signature_value, signature_pubkey, source,
       published_at, created_at
FROM activities
WHERE activity_uri = $1;

-- name: ListActorOutbox :many
-- Per-actor outbox feed, newest-first cursor-paginated. The
-- federation outbox worker (Phase 1.22.D) calls this filtered to
-- a window of "what hasn't been delivered yet"; the admin audit
-- UI calls it unfiltered.
SELECT id, activity_uri, activity_type, actor_uri, actor_user_ref,
       object_uri, object_kind, object_local_id, target_uri,
       to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
       payload, signature_value, signature_pubkey, source,
       published_at, created_at
FROM activities
WHERE actor_user_ref = $1
  AND source = 'local'
  AND (sqlc.narg('cursor_published_at')::TIMESTAMPTZ IS NULL
       OR published_at < sqlc.narg('cursor_published_at')::TIMESTAMPTZ
       OR (published_at = sqlc.narg('cursor_published_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY published_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: ListObjectActivities :many
-- Per-object timeline. Powers the admin audit "show me everything
-- that happened to this post" drill-down + future "object history"
-- frontend surfaces.
SELECT id, activity_uri, activity_type, actor_uri, actor_user_ref,
       object_uri, object_kind, object_local_id, target_uri,
       to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
       payload, signature_value, signature_pubkey, source,
       published_at, created_at
FROM activities
WHERE object_kind = $1
  AND object_local_id = $2
ORDER BY published_at DESC, id DESC
LIMIT $3;

-- name: SetActivitySignature :exec
-- Phase 1.22.D will populate the signature columns after the
-- outbox worker signs an envelope for delivery. Separate update
-- so signing doesn't have to rewrite the payload JSONB.
UPDATE activities
SET signature_value  = $2,
    signature_pubkey = $3
WHERE id = $1;

-- name: LookupMostRecentLocalActivity :one
-- Phase 1.22.A-bis-3a — supports Undo emission. Returns the
-- activity_uri of the most recent LOCAL-emitted activity by an
-- actor about a specific object, of a specific type.
--
-- Use case: when a user unlikes a post, the resulting Undo
-- activity per AP §6.10 must reference the original Like
-- activity's URI. This query finds it.
--
-- We don't filter "has this been undone already?" — the domain
-- table (likes / user_follows / user_blocks / comments) is the
-- authority on whether the edge is currently active. If the
-- handler is calling this query to emit an Undo, the edge WAS
-- active (the handler just deleted it). The most-recent matching
-- activity is by definition the one we're undoing.
--
-- Returns pgx.ErrNoRows when no prior activity exists (e.g. the
-- like predates ADR-0044 wiring and was never recorded). Callers
-- treat this as "skip the Undo emission" and continue — the
-- domain mutation still happens; the activity ledger is just
-- incomplete for that pre-existing edge.
SELECT activity_uri
FROM activities
WHERE actor_user_ref = $1
  AND activity_type  = $2
  AND object_kind    = $3
  AND object_local_id = $4
  AND source = 'local'
ORDER BY published_at DESC, id DESC
LIMIT 1;

-- name: ListActivitiesAdmin :many
-- Phase 1.22.A-bis-3b — admin audit view. Cursor-paginated by
-- (published_at DESC, id DESC). All filter args are optional;
-- pass NULL via sqlc.narg to skip a filter.
--
-- This query is system.admin-gated at the handler layer per ADR
-- 0044 — surfaces full payloads + addressing for the operator
-- to audit federation activity. Future enhancement (per ADR 0043
-- §"Audit + observability"): a federation.admin capability split
-- so non-system admins can view but not mutate the federation
-- surface.
SELECT id, activity_uri, activity_type, actor_uri, actor_user_ref,
       object_uri, object_kind, object_local_id, target_uri,
       to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
       payload, signature_value, signature_pubkey, source,
       published_at, created_at
FROM activities
WHERE
    (sqlc.narg('activity_type')::TEXT IS NULL
        OR activity_type = sqlc.narg('activity_type')::TEXT)
AND (sqlc.narg('source')::TEXT IS NULL
        OR source = sqlc.narg('source')::TEXT)
AND (sqlc.narg('actor_user_ref')::BIGINT IS NULL
        OR actor_user_ref = sqlc.narg('actor_user_ref')::BIGINT)
AND (sqlc.narg('object_kind')::TEXT IS NULL
        OR object_kind = sqlc.narg('object_kind')::TEXT)
AND (sqlc.narg('since')::TIMESTAMPTZ IS NULL
        OR published_at >= sqlc.narg('since')::TIMESTAMPTZ)
AND (sqlc.narg('cursor_published_at')::TIMESTAMPTZ IS NULL
        OR published_at < sqlc.narg('cursor_published_at')::TIMESTAMPTZ
        OR (published_at = sqlc.narg('cursor_published_at')::TIMESTAMPTZ
            AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY published_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;
