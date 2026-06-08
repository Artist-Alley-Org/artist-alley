-- name: InsertOutboxRow :one
-- Idempotent via the partial UNIQUE indexes. ON CONFLICT DO
-- NOTHING handles both targeted (target_user_url IS NOT NULL)
-- + broadcast (NULL) flavours.
INSERT INTO federation_outbox (
    activity_id, peer_id, target_user_url, status, next_attempt_at
)
VALUES ($1, $2, $3, 'queued', NOW())
ON CONFLICT DO NOTHING
RETURNING id, activity_id, peer_id, target_user_url, status,
          attempts, next_attempt_at, last_attempt_at, last_error,
          sent_at, delivered_with_key_id, created_at, updated_at;

-- name: ListDueOutbox :many
-- Hot-path read for the delivery worker. Uses
-- federation_outbox_due_idx (partial index on status='queued').
-- Sorted by next_attempt_at so the oldest-due row goes first.
-- Limited at the caller's batch size — the delivery worker
-- defaults to 100 rows/tick.
SELECT id, activity_id, peer_id, target_user_url, status,
       attempts, next_attempt_at, last_attempt_at, last_error,
       sent_at, delivered_with_key_id, created_at, updated_at
FROM federation_outbox
WHERE status = 'queued'
  AND next_attempt_at <= NOW()
ORDER BY next_attempt_at
LIMIT $1;

-- name: ListDueOutboxByPeer :many
-- Per-peer batching for the delivery worker. Pulls due rows
-- for a SINGLE peer so the worker can fold them into one
-- batched POST per the §3.10 batched-delivery optimisation.
SELECT id, activity_id, peer_id, target_user_url, status,
       attempts, next_attempt_at, last_attempt_at, last_error,
       sent_at, delivered_with_key_id, created_at, updated_at
FROM federation_outbox
WHERE status = 'queued'
  AND next_attempt_at <= NOW()
  AND peer_id = $1
ORDER BY next_attempt_at
LIMIT $2;

-- name: MarkOutboxSent :execrows
-- Transition queued → sent on a successful delivery.
UPDATE federation_outbox
   SET status     = 'sent',
       sent_at    = NOW(),
       last_attempt_at = NOW(),
       updated_at = NOW(),
       delivered_with_key_id = $2
 WHERE id = $1 AND status = 'queued';

-- name: MarkOutboxAttemptFailed :execrows
-- Transient failure: bump attempts, schedule next_attempt_at
-- per the §3.4 backoff schedule. Caller passes the computed
-- next_attempt_at + the new error string.
UPDATE federation_outbox
   SET attempts        = attempts + 1,
       next_attempt_at = $2,
       last_attempt_at = NOW(),
       last_error      = $3,
       updated_at      = NOW()
 WHERE id = $1 AND status = 'queued';

-- name: MarkOutboxFailedTerminal :execrows
-- Permanent failure: status → failed. Caller invokes when the
-- attempts cap is hit OR when the error is non-retryable
-- (4xx that isn't 429).
UPDATE federation_outbox
   SET status     = 'failed',
       last_attempt_at = NOW(),
       last_error = $2,
       updated_at = NOW()
 WHERE id = $1 AND status = 'queued';

-- name: CancelOutboxByPeer :execrows
-- Defederation cascade: cancel every queued row for a peer.
-- Returns the count for the audit row.
UPDATE federation_outbox
   SET status     = 'cancelled',
       last_error = 'peer defederation cascade',
       updated_at = NOW()
 WHERE peer_id = $1 AND status = 'queued';

-- name: RequeueFailedOutbox :execrows
-- Admin re-queue button (1.22.D-c). Resets the attempt count
-- so the operator-driven retry starts fresh against the cap.
UPDATE federation_outbox
   SET status          = 'queued',
       attempts        = 0,
       next_attempt_at = NOW(),
       last_attempt_at = NULL,
       updated_at      = NOW()
 WHERE id = $1 AND status = 'failed';

-- name: GetOutboxByID :one
SELECT id, activity_id, peer_id, target_user_url, status,
       attempts, next_attempt_at, last_attempt_at, last_error,
       sent_at, delivered_with_key_id, created_at, updated_at
FROM federation_outbox
WHERE id = $1;

-- name: ListOutboxByPeer :many
-- Per-peer admin view. Most-recent first.
SELECT id, activity_id, peer_id, target_user_url, status,
       attempts, next_attempt_at, last_attempt_at, last_error,
       sent_at, delivered_with_key_id, created_at, updated_at
FROM federation_outbox
WHERE peer_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: CountOutboxByStatusByPeer :many
-- Admin queue-summary metrics: how many queued / sent /
-- failed / cancelled per peer.
SELECT peer_id, status, COUNT(*) AS n
FROM federation_outbox
GROUP BY peer_id, status
ORDER BY peer_id, status;

-- name: ListOutboxForAdmin :many
-- Filtered + paginated admin list for /admin/federation/outbox
-- (Phase 1.22.D-c). All filters are optional (NULL = skip).
-- Pagination uses (created_at, id) tuple via the cursor params
-- — opaque to the client but deterministic so the next page
-- doesn't skip or duplicate rows under concurrent inserts.
SELECT o.id, o.activity_id, o.peer_id, o.target_user_url,
       o.status, o.attempts, o.next_attempt_at, o.last_attempt_at,
       o.last_error, o.sent_at, o.delivered_with_key_id,
       o.created_at, o.updated_at,
       a.activity_type AS activity_type
FROM federation_outbox o
JOIN activities a ON a.id = o.activity_id
WHERE (sqlc.narg('peer_id')::uuid IS NULL
       OR o.peer_id = sqlc.narg('peer_id')::uuid)
  AND (sqlc.narg('status')::text IS NULL
       OR o.status = sqlc.narg('status')::text)
  AND (sqlc.narg('activity_type')::text IS NULL
       OR a.activity_type = sqlc.narg('activity_type')::text)
  AND (sqlc.narg('since')::timestamptz IS NULL
       OR o.created_at >= sqlc.narg('since')::timestamptz)
  AND (sqlc.narg('cursor_created_at')::timestamptz IS NULL
       OR (o.created_at, o.id) < (
           sqlc.narg('cursor_created_at')::timestamptz,
           sqlc.narg('cursor_id')::uuid
       ))
ORDER BY o.created_at DESC, o.id DESC
LIMIT sqlc.arg('limit_n')::int;

-- --- dispatch_state ---------------------------------------------------

-- name: GetDispatchState :one
-- The single cursor row. NULL last_dispatched_activity_id means
-- "no activities dispatched yet" — first tick processes
-- everything from the beginning.
SELECT id, last_dispatched_activity_id, last_dispatched_at, updated_at
FROM federation_dispatch_state
WHERE id = 1;

-- name: AdvanceDispatchCursor :exec
-- Cursor advance happens in the SAME transaction as the
-- outbox INSERTs from the same batch (per design §3.7).
UPDATE federation_dispatch_state
   SET last_dispatched_activity_id = $1,
       last_dispatched_at          = NOW(),
       updated_at                  = NOW()
 WHERE id = 1;

-- name: ListUndispatchedActivities :many
-- Activities published after the cursor. The cursor itself is
-- compared via (created_at, id) so multi-row-same-timestamp
-- orderings stay correct. When the cursor's last_id is NULL
-- (first run) we return EVERYTHING ordered by (created_at, id).
SELECT id, activity_uri, activity_type, actor_uri, actor_user_ref,
       object_uri, object_kind, object_local_id, target_uri,
       to_uris, cc_uris, bto_uris, bcc_uris, audience_uris,
       payload, signature_value, signature_pubkey, source,
       published_at, created_at
FROM activities
WHERE source = 'local'
  AND (
    sqlc.narg('cursor_id')::uuid IS NULL
    OR (created_at, id) > (
      (SELECT created_at FROM activities WHERE id = sqlc.narg('cursor_id')::uuid),
      sqlc.narg('cursor_id')::uuid
    )
  )
ORDER BY created_at, id
LIMIT $1;
