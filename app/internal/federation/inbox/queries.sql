-- name: InsertInbox :one
-- Persist a received envelope at pipeline stage 10. UNIQUE on
-- activity_uri makes replays surface as a constraint violation
-- (caller maps to 200 OK + no-op).
INSERT INTO federation_inbox (
    activity_uri,
    peer_id,
    actor_uri,
    activity_type,
    object_kind,
    object_id,
    envelope_json,
    http_sig_key
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, activity_uri, peer_id, actor_uri, activity_type,
          object_kind, object_id, envelope_json, http_sig_key,
          received_at, status, reject_reason, dispatch_attempts,
          last_attempt_at, last_error, processed_at,
          correlation_activity_id, created_at, updated_at;

-- name: GetInboxByID :one
SELECT id, activity_uri, peer_id, actor_uri, activity_type,
       object_kind, object_id, envelope_json, http_sig_key,
       received_at, status, reject_reason, dispatch_attempts,
       last_attempt_at, last_error, processed_at,
       correlation_activity_id, created_at, updated_at
FROM federation_inbox
WHERE id = $1;

-- name: GetInboxByActivityURI :one
SELECT id, activity_uri, peer_id, actor_uri, activity_type,
       object_kind, object_id, envelope_json, http_sig_key,
       received_at, status, reject_reason, dispatch_attempts,
       last_attempt_at, last_error, processed_at,
       correlation_activity_id, created_at, updated_at
FROM federation_inbox
WHERE activity_uri = $1;

-- name: ListPendingInbox :many
-- Background worker fetches the next batch ordered by receipt
-- time (FIFO). Caller passes a chunk size; partial index
-- federation_inbox_pending_idx keeps this O(log N).
SELECT id, activity_uri, peer_id, actor_uri, activity_type,
       object_kind, object_id, envelope_json, http_sig_key,
       received_at, status, reject_reason, dispatch_attempts,
       last_attempt_at, last_error, processed_at,
       correlation_activity_id, created_at, updated_at
FROM federation_inbox
WHERE status = 'pending'
ORDER BY received_at
LIMIT $1;

-- name: MarkInboxProcessed :one
-- Worker stage 13: dispatch succeeded.
UPDATE federation_inbox
SET status                  = 'processed',
    processed_at            = NOW(),
    last_attempt_at         = NOW(),
    dispatch_attempts       = dispatch_attempts + 1,
    correlation_activity_id = $2,
    last_error              = '',
    updated_at              = NOW()
WHERE id = $1
RETURNING id, status, processed_at, correlation_activity_id;

-- name: MarkInboxRejected :one
-- Worker stage 12: gate rejected or domain handler said "permanent
-- failure, do not retry." reason MUST be a closed-catalogue value
-- per docs/spec/federation/v1.md §12.1.
UPDATE federation_inbox
SET status            = 'rejected',
    reject_reason     = $2,
    last_attempt_at   = NOW(),
    dispatch_attempts = dispatch_attempts + 1,
    last_error        = $3,
    updated_at        = NOW()
WHERE id = $1
RETURNING id, status, reject_reason;

-- name: MarkInboxAttemptFailed :one
-- Worker stage 13 transient-failure path. Increments dispatch_
-- attempts + records the error, but keeps status='pending' so
-- the worker picks it up again on the next tick. Distinct from
-- MarkInboxFailedTerminal which moves to status='failed' after
-- the attempts cap is reached or the error is non-retryable.
UPDATE federation_inbox
SET last_attempt_at   = NOW(),
    dispatch_attempts = dispatch_attempts + 1,
    last_error        = $2,
    updated_at        = NOW()
WHERE id = $1
RETURNING id, status, dispatch_attempts;

-- name: MarkInboxFailedTerminal :one
-- Worker stage 13 terminal-failure path. The row will NOT be
-- retried automatically; an operator must trigger the re-queue
-- button (1.22.D-c). Use when:
--   - dispatch_attempts has hit the configured cap, OR
--   - the domain handler returned a non-retryable error.
UPDATE federation_inbox
SET status            = 'failed',
    last_attempt_at   = NOW(),
    dispatch_attempts = dispatch_attempts + 1,
    last_error        = $2,
    updated_at        = NOW()
WHERE id = $1
RETURNING id, status, dispatch_attempts;

-- name: RequeueFailedInbox :one
-- Operator action (1.22.D-c admin UI re-queue button) — moves a
-- failed row back to pending so the worker picks it up again on
-- the next tick. Resets dispatch_attempts to 0 — the gold-
-- standard semantic is "treat the next attempt as a fresh start
-- against the cap." Operator-driven recovery flow shouldn't
-- inherit the prior failure history's attempt counter (which
-- would terminal-fail on attempt 1 if it was already at the cap).
-- The dispatch_attempts history is preserved in last_error +
-- the per-attempt audit events.
UPDATE federation_inbox
SET status            = 'pending',
    dispatch_attempts = 0,
    last_error        = '',
    updated_at        = NOW()
WHERE id = $1 AND status = 'failed'
RETURNING id, status, dispatch_attempts;

-- name: ListInboxByPeer :many
-- Admin per-peer view. Returns most recent N regardless of
-- status so the page shows pending + processed + rejected + failed
-- in one feed.
SELECT id, activity_uri, peer_id, actor_uri, activity_type,
       object_kind, object_id, envelope_json, http_sig_key,
       received_at, status, reject_reason, dispatch_attempts,
       last_attempt_at, last_error, processed_at,
       correlation_activity_id, created_at, updated_at
FROM federation_inbox
WHERE peer_id = $1
ORDER BY received_at DESC
LIMIT $2;

-- name: CountInboxByStatusByPeer :many
-- Admin dashboard rollup: per-peer counts grouped by status. Cheap
-- via federation_inbox_by_peer_idx + the worker keeps the partial
-- index's working set small.
SELECT status, COUNT(*)::BIGINT AS n
FROM federation_inbox
WHERE peer_id = $1
GROUP BY status;
