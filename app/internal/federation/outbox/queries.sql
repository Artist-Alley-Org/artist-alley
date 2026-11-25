-- name: InsertOutboxRow :one
-- Idempotent via the partial UNIQUE indexes. ON CONFLICT DO
-- NOTHING handles both targeted (target_user_url IS NOT NULL)
-- + broadcast (NULL) flavours.
--
-- Phase 1.22.I-g: sensitivity is denormalized from the activities
-- ledger at INSERT time so the delivery Worker can consult
-- outbox.ChoosePathFor without a per-row JOIN. The dispatcher
-- always supplies a tier (unresolved when it cannot learn one);
-- a NULL (pre-I-g rows or test fixtures) is treated by the Worker
-- as unresolved, which requires encryption (#1525).
INSERT INTO federation_outbox (
    activity_id, peer_id, target_user_url, status, next_attempt_at, sensitivity
)
VALUES ($1, $2, $3, 'queued', NOW(), $4)
ON CONFLICT DO NOTHING
RETURNING id, activity_id, peer_id, target_user_url, status,
          attempts, next_attempt_at, last_attempt_at, last_error,
          sent_at, delivered_with_key_id, created_at, updated_at, was_encrypted,
          sensitivity, refused_reason;

-- name: ListDueOutbox :many
-- Hot-path read for the delivery worker. Uses
-- federation_outbox_due_idx (partial index on status='queued').
-- Sorted by next_attempt_at so the oldest-due row goes first.
-- Limited at the caller's batch size — the delivery worker
-- defaults to 100 rows/tick.
SELECT id, activity_id, peer_id, target_user_url, status,
       attempts, next_attempt_at, last_attempt_at, last_error,
       sent_at, delivered_with_key_id, created_at, updated_at, was_encrypted,
       sensitivity, refused_reason
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
       sent_at, delivered_with_key_id, created_at, updated_at, was_encrypted,
       sensitivity, refused_reason
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

-- name: MarkOutboxRefused :execrows
-- Phase 1.22.I-g terminal-refusal path. Worker flips queued →
-- refused with a catalogue reason when policy.ChoosePathFor
-- returns EmissionRefused (share sensitivity requires encryption
-- + capability or recipient key isn't available). The partial-
-- index federation_outbox_due_idx (status='queued') filters
-- refused rows out of ListDueOutbox automatically, so refusal is
-- terminal — no retries, no backoff. Operator action (re-pair →
-- caps refresh OR move share to a lower tier) is the recovery
-- path; the protocol does NOT auto-retry after capability
-- changes.
UPDATE federation_outbox
   SET status         = 'refused',
       refused_reason = $2,
       last_attempt_at = NOW(),
       updated_at     = NOW()
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
       sent_at, delivered_with_key_id, created_at, updated_at, was_encrypted,
       sensitivity, refused_reason
FROM federation_outbox
WHERE id = $1;

-- name: ListOutboxByPeer :many
-- Per-peer admin view. Most-recent first.
SELECT id, activity_id, peer_id, target_user_url, status,
       attempts, next_attempt_at, last_attempt_at, last_error,
       sent_at, delivered_with_key_id, created_at, updated_at, was_encrypted,
       sensitivity, refused_reason
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
-- The single progress row. NULL last_dispatched_activity_id means no
-- activity has been dispatched yet. It does not decide what is pending
-- (federation_dispatch_work does, #1577).
SELECT id, last_dispatched_activity_id, last_dispatched_at, updated_at
FROM federation_dispatch_state
WHERE id = 1;

-- name: AdvanceDispatchCursor :exec
-- Progress only (#1577): what is still to dispatch is the
-- federation_dispatch_work rows, never a position. The cursor moves in the
-- SAME transaction as the batch's outbox INSERTs and work acknowledgements,
-- and only FORWARD by the activity's (created_at, id): a late-committed
-- activity handled after a newer one, or a concurrent dispatcher's smaller
-- batch, leaves it where it is. A NULL cursor, or one naming an activity
-- that no longer exists, is replaced.
UPDATE federation_dispatch_state s
   SET last_dispatched_activity_id = sqlc.arg('activity_id')::uuid,
       last_dispatched_at          = NOW(),
       updated_at                  = NOW()
 WHERE s.id = 1
   AND (s.last_dispatched_activity_id IS NULL
        OR NOT EXISTS (SELECT 1 FROM activities c WHERE c.id = s.last_dispatched_activity_id)
        OR EXISTS (SELECT 1 FROM activities n, activities c
                    WHERE n.id = sqlc.arg('activity_id')::uuid AND c.id = s.last_dispatched_activity_id
                      AND (n.created_at, n.id) > (c.created_at, c.id)));

-- name: ClaimDispatchWork :many
-- The pending local activities (#1577, migration 00101): a bounded page of
-- federation_dispatch_work rows, oldest activity first, each joined to its
-- activity and locked FOR UPDATE SKIP LOCKED inside the dispatcher's
-- transaction, so two dispatchers never handle the same row. Membership
-- alone decides what is pending: a row that committed late is claimed
-- whatever its created_at. The lock lasts until the batch commits (the row
-- is then acknowledged) or rolls back (the row is pending again).
SELECT a.id, a.activity_uri, a.activity_type, a.actor_uri, a.actor_user_ref,
       a.object_uri, a.object_kind, a.object_local_id, a.target_uri,
       a.to_uris, a.cc_uris, a.bto_uris, a.bcc_uris, a.audience_uris,
       a.payload, a.signature_value, a.signature_pubkey, a.source,
       a.published_at, a.created_at
FROM federation_dispatch_work w
JOIN activities a ON a.id = w.activity_id
ORDER BY w.activity_created_at, w.activity_id
LIMIT $1
FOR UPDATE OF w SKIP LOCKED;

-- name: AckDispatchWork :execrows
-- Acknowledges the claimed rows once their activities are handled (outbox
-- rows written, or the skip decided), in the same transaction.
DELETE FROM federation_dispatch_work
 WHERE activity_id = ANY (sqlc.arg('activity_ids')::uuid[]);

-- name: MarkOutboxEncrypted :exec
-- Phase 1.22.I-e — flips the row's was_encrypted observability
-- column to TRUE. Called by the delivery worker after the
-- encryption branch produces a sealed envelope but BEFORE
-- attempting the HTTP POST, so even a transient delivery
-- failure leaves the column reflecting what we actually
-- emitted.
UPDATE federation_outbox
   SET was_encrypted = TRUE,
       updated_at = NOW()
 WHERE id = $1;
