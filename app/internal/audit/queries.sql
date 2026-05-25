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
SELECT id,
       event_type,
       occurred_at,
       subject_user_ref,
       actor_user_ref,
       ip,
       user_agent,
       metadata
FROM audit_events
WHERE subject_user_ref = $1
ORDER BY occurred_at DESC
LIMIT $2;
