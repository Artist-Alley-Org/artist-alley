-- name: InsertAuditEvent :exec
-- Append-only write of a single audit event. Callers build the event
-- through audit.Recorder, which knows the per-event-type metadata shape.
INSERT INTO audit_events (
    event_type,
    subject_user_ref,
    actor_user_ref,
    ip,
    user_agent,
    metadata
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: RecentAuditEventsForUser :many
-- Powers "your recent activity" surfaces. The partial index on
-- (subject_user_ref, occurred_at DESC) keeps this fast.
--
-- `ip` is deliberately NOT selected (#458). Actor IP is personal data
-- gated behind system.audit.pii.read (#425), and this query has no
-- caller today — so leaving ip in the SELECT was a latent trap: a
-- future caller would render it un-gated. When per-user activity does
-- need IP, it wires through the same includeIP gate as the export path
-- (audit/export.go) rather than pulling the raw column here.
SELECT id,
       event_type,
       occurred_at,
       subject_user_ref,
       actor_user_ref,
       user_agent,
       metadata
FROM audit_events
WHERE subject_user_ref = $1
ORDER BY occurred_at DESC
LIMIT $2;

-- name: ListAuditEvents :many
-- Admin audit viewer (Phase 1.17.K). Supports keyset pagination on
-- (occurred_at DESC, id DESC) — cursor params @cursor_at + @cursor_id
-- are the last row of the previous page. NULL cursor values fetch the
-- newest page.
--
-- Filters (each NULL = no constraint):
--   @event_type        — exact match on event_type
--   @actor_user_ref    — actor (admin who did it)
--   @subject_user_ref  — subject (user it happened to)
--   @since / @until    — occurred_at window
SELECT id,
       event_type,
       occurred_at,
       subject_user_ref,
       actor_user_ref,
       ip,
       user_agent,
       metadata
FROM audit_events
WHERE ( sqlc.narg(event_type)::text         IS NULL OR event_type       = sqlc.narg(event_type)::text )
  AND ( sqlc.narg(actor_user_ref)::bigint   IS NULL OR actor_user_ref   = sqlc.narg(actor_user_ref)::bigint )
  AND ( sqlc.narg(subject_user_ref)::bigint IS NULL OR subject_user_ref = sqlc.narg(subject_user_ref)::bigint )
  AND ( sqlc.narg(since)::timestamptz       IS NULL OR occurred_at      >= sqlc.narg(since)::timestamptz )
  AND ( sqlc.narg(until)::timestamptz       IS NULL OR occurred_at      <= sqlc.narg(until)::timestamptz )
  AND ( sqlc.narg(cursor_at)::timestamptz   IS NULL
        OR occurred_at < sqlc.narg(cursor_at)::timestamptz
        OR (occurred_at = sqlc.narg(cursor_at)::timestamptz AND id < sqlc.narg(cursor_id)::uuid) )
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg(lim)::int;

-- name: CountAuditEvents :one
-- Total rows matching the filter — cursor-independent. Drives the
-- "X events" counter in the viewer header. Same filter clauses as
-- ListAuditEvents but no cursor / pagination.
SELECT COUNT(*)::bigint AS total
FROM audit_events
WHERE ( sqlc.narg(event_type)::text         IS NULL OR event_type       = sqlc.narg(event_type)::text )
  AND ( sqlc.narg(actor_user_ref)::bigint   IS NULL OR actor_user_ref   = sqlc.narg(actor_user_ref)::bigint )
  AND ( sqlc.narg(subject_user_ref)::bigint IS NULL OR subject_user_ref = sqlc.narg(subject_user_ref)::bigint )
  AND ( sqlc.narg(since)::timestamptz       IS NULL OR occurred_at      >= sqlc.narg(since)::timestamptz )
  AND ( sqlc.narg(until)::timestamptz       IS NULL OR occurred_at      <= sqlc.narg(until)::timestamptz );

-- name: ListAuditEventTypes :many
-- Distinct event_type values present in the log. Powers the type-
-- filter dropdown in the admin viewer — we don't hard-code the list
-- because new event types land all the time.
SELECT DISTINCT event_type
FROM audit_events
ORDER BY event_type ASC;

-- ---------------------------------------------------------------------------
-- Retention + export (#467, ADR 0032).
-- ---------------------------------------------------------------------------

-- name: ListRetentionPolicies :many
-- Per-category retention durations. Categories absent here fall to the
-- code default (7 years).
SELECT category, retention, updated_by, updated_at
FROM audit_retention_policy
ORDER BY category;

-- name: DistinctAuditCategories :many
-- Every category currently present in the log, so the purge covers
-- categories that have no explicit policy row (they get the default).
SELECT DISTINCT category FROM audit_events WHERE category <> '';

-- name: PurgeAuditCategoryBatch :execrows
-- Deletes up to @lim over-age, non-held rows of one category. Batched
-- so a large backlog can't wedge one giant transaction; the caller
-- loops until a batch comes back short. legal_hold rows are exempt.
DELETE FROM audit_events
WHERE id IN (
    SELECT id FROM audit_events
    WHERE category = @category::text
      AND occurred_at < @cutoff::timestamptz
      AND legal_hold = false
    LIMIT @lim::int
);

-- name: TombstoneActor :execrows
-- GDPR DSAR (ADR 0024): a user is being deleted. Their audit rows are
-- PRESERVED — this only anonymizes the actor identity: the numeric ref
-- is cleared and a `deleted-user-{ref}` pseudonym is recorded in
-- metadata so the trail stays continuous without pointing at a now-gone
-- user row. Rows survive; identity does not.
UPDATE audit_events
SET actor_user_ref = NULL,
    metadata = jsonb_set(metadata, '{actor_tombstone}',
                         to_jsonb('deleted-user-' || @user_ref::text))
WHERE actor_user_ref = @user_ref::bigint;

-- name: ExportAuditEventsPage :many
-- One keyset page for the streaming export. Ascending (occurred_at, id)
-- so the cursor advances forward through the whole range; the caller
-- pulls page after page and never holds more than one in memory.
SELECT id, event_type, occurred_at, subject_user_ref, actor_user_ref,
       ip, user_agent, metadata, legal_hold
FROM audit_events
WHERE ( sqlc.narg(since)::timestamptz IS NULL OR occurred_at >= sqlc.narg(since)::timestamptz )
  AND ( sqlc.narg(until)::timestamptz IS NULL OR occurred_at <= sqlc.narg(until)::timestamptz )
  AND ( sqlc.narg(cursor_at)::timestamptz IS NULL
        OR occurred_at > sqlc.narg(cursor_at)::timestamptz
        OR (occurred_at = sqlc.narg(cursor_at)::timestamptz AND id > sqlc.narg(cursor_id)::uuid) )
ORDER BY occurred_at ASC, id ASC
LIMIT sqlc.arg(lim)::int;
