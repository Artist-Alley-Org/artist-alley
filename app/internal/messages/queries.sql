-- name: InsertDirectMessage :one
-- Writer entry: send a DM. Caller enforces permission gates
-- (HasBlockBetween + capability) BEFORE calling. body is trusted
-- to be non-empty + trimmed at the handler layer.
INSERT INTO direct_messages (
    sender_user_ref,
    recipient_user_ref,
    body
)
VALUES ($1, $2, $3)
RETURNING id,
          sender_user_ref,
          recipient_user_ref,
          body,
          sent_at,
          read_at,
          origin_server_id;

-- name: ListThreadWithPeer :many
-- Thread view: every message between the caller and peer X,
-- newest-first cursor-paginated. The OR-ed WHERE clause hits one
-- of the two recipient/sender indexes per direction; planner picks.
SELECT id,
       sender_user_ref,
       recipient_user_ref,
       body,
       sent_at,
       read_at,
       origin_server_id
FROM direct_messages
WHERE ((sender_user_ref = $1 AND recipient_user_ref = $2)
    OR (sender_user_ref = $2 AND recipient_user_ref = $1))
  AND (sqlc.narg('cursor_sent_at')::TIMESTAMPTZ IS NULL
       OR sent_at < sqlc.narg('cursor_sent_at')::TIMESTAMPTZ
       OR (sent_at = sqlc.narg('cursor_sent_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY sent_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: ListMyThreads :many
-- Inbox view: list of peers I've exchanged DMs with + the most
-- recent message in each thread + unread count for the thread.
-- Uses a window function over the (peer, time DESC) partition to
-- pick the latest message per peer in one pass; cheaper than a
-- self-join + DISTINCT ON.
WITH dms AS (
    SELECT
        (CASE WHEN sender_user_ref = $1 THEN recipient_user_ref
             ELSE sender_user_ref END)::BIGINT AS peer_user_ref,
        id,
        sender_user_ref,
        recipient_user_ref,
        body,
        sent_at,
        read_at,
        ROW_NUMBER() OVER (
            PARTITION BY CASE WHEN sender_user_ref = $1 THEN recipient_user_ref
                              ELSE sender_user_ref END
            ORDER BY sent_at DESC, id DESC
        ) AS rn
    FROM direct_messages
    WHERE sender_user_ref = $1 OR recipient_user_ref = $1
)
SELECT
    d.peer_user_ref,
    d.id            AS last_message_id,
    d.sender_user_ref AS last_sender_user_ref,
    d.body          AS last_body,
    d.sent_at       AS last_sent_at,
    d.read_at       AS last_read_at,
    u.username      AS peer_username,
    up.display_name AS peer_display_name,
    up.avatar_url   AS peer_avatar_url,
    (
        SELECT COUNT(*)::BIGINT
        FROM direct_messages dm
        WHERE dm.recipient_user_ref = $1
          AND dm.sender_user_ref = d.peer_user_ref
          AND dm.read_at IS NULL
    ) AS unread_count
FROM dms d
JOIN "user" u             ON u.ref = d.peer_user_ref
LEFT JOIN user_profiles up ON up.rs_user_id = d.peer_user_ref
WHERE d.rn = 1
ORDER BY d.sent_at DESC
LIMIT $2;

-- name: CountMyUnreadDirectMessages :one
-- Envelope badge query — hits idx_dm_unread (partial). Cache-backed
-- in the handler via cache.Registry NOTIFY.
SELECT COUNT(*)::BIGINT AS count
FROM direct_messages
WHERE recipient_user_ref = $1
  AND read_at IS NULL;

-- name: MarkThreadRead :execrows
-- "Mark every unread DM from peer X as read." Called when the user
-- opens a thread. Returns rows-affected so the handler knows
-- whether to invalidate the unread cache.
UPDATE direct_messages
SET read_at = NOW()
WHERE recipient_user_ref = $1
  AND sender_user_ref = $2
  AND read_at IS NULL;
