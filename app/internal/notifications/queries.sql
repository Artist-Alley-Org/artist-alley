-- name: InsertNotification :one
-- Single source of insert for the notifications writer. delivered_at
-- defaults to NOW() at the column level — in-app delivery IS the
-- row landing. read_at + email_sent_at stay NULL.
INSERT INTO notifications (
    recipient_user_ref,
    actor_user_ref,
    verb,
    target_kind,
    target_id,
    payload
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id,
          recipient_user_ref,
          actor_user_ref,
          verb,
          target_kind,
          target_id,
          payload,
          read_at,
          delivered_at,
          email_sent_at,
          origin_server_id,
          created_at;

-- name: ListMyNotifications :many
-- Cursor-paginated by (created_at DESC, id DESC). The optional
-- `only_unread` flag is the inbox's "Unread" tab. The
-- idx_notifications_recipient_recent covering index handles the
-- general page; the partial idx_notifications_unread takes over
-- when only_unread is true.
SELECT id,
       recipient_user_ref,
       actor_user_ref,
       verb,
       target_kind,
       target_id,
       payload,
       read_at,
       delivered_at,
       email_sent_at,
       origin_server_id,
       created_at
FROM notifications
WHERE recipient_user_ref = $1
  AND (sqlc.narg('only_unread')::BOOLEAN IS NOT TRUE OR read_at IS NULL)
  AND (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
       OR created_at < sqlc.narg('cursor_created_at')::TIMESTAMPTZ
       OR (created_at = sqlc.narg('cursor_created_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: CountMyUnreadNotifications :one
-- The bell-badge query — runs on every authenticated page render.
-- Partial index makes this an index-only scan over the unread tail
-- regardless of how large the lifetime notifications table grows.
SELECT COUNT(*)::BIGINT AS count
FROM notifications
WHERE recipient_user_ref = $1
  AND read_at IS NULL;

-- name: MarkNotificationRead :execrows
-- Idempotent: setting read_at on an already-read row keeps the
-- original read_at (uses NOT NULL guard in the SET). Returns
-- rows-affected so the handler can 404 cleanly when the row isn't
-- mine or doesn't exist (the WHERE clause filters on recipient
-- specifically — no separate "is this mine?" check needed).
UPDATE notifications
SET read_at = NOW()
WHERE id = $1
  AND recipient_user_ref = $2
  AND read_at IS NULL;

-- name: MarkAllMyNotificationsRead :execrows
-- "Mark all read" — the inbox header button. Returns count of rows
-- newly-flipped to read so the UI can show "Marked 12 as read."
UPDATE notifications
SET read_at = NOW()
WHERE recipient_user_ref = $1
  AND read_at IS NULL;

-- name: InsertDigestQueue :exec
-- Phase 1.55.Y — queue a non-immediate notification email for the
-- digest coordinator to batch. topic is the notification verb; cadence
-- is hourly|daily|weekly (immediate never queues). notification_id FKs
-- the row the digest renders from.
INSERT INTO digest_queue (user_ref, topic, cadence, notification_id)
VALUES ($1, $2, $3, $4);

-- name: ListPendingDigest :many
-- Phase 1.55.Y — the coordinator's per-tick read: every unsent digest
-- row whose cadence is due this tick, joined to its notification for
-- rendering. Ordered by user so the coordinator can group in one pass.
SELECT dq.id,
       dq.user_ref,
       dq.topic,
       dq.cadence,
       dq.notification_id,
       dq.queued_at,
       n.verb,
       n.actor_user_ref,
       n.target_kind,
       n.target_id,
       n.payload,
       n.created_at
FROM digest_queue dq
JOIN notifications n ON n.id = dq.notification_id
WHERE dq.sent_at IS NULL
  AND dq.cadence = ANY(@cadences::text[])
ORDER BY dq.user_ref, dq.queued_at;

-- name: MarkDigestSent :exec
-- Phase 1.55.Y — mark consumed rows sent after a digest email goes out.
-- Idempotent: a re-run over already-sent ids is a no-op (WHERE guards).
UPDATE digest_queue
SET sent_at = NOW()
WHERE id = ANY(@ids::uuid[])
  AND sent_at IS NULL;

-- name: DigestRecipientEmail :one
-- Phase 1.55.Y — email + display name for a digest recipient. Mirrors
-- the notification.email job's lookup (fullname → username → NULL).
SELECT email, fullname, username
FROM "user"
WHERE ref = $1;
