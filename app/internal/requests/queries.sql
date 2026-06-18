-- Phase 1.17.E — resource_request queries.
--
-- Naming convention: InsertX / GetX / ListX / MarkX
-- mirrors the existing auth + users packages.

-- name: InsertResourceRequest :one
-- Creates a fresh pending request. The state column defaults to
-- 'pending' via the schema; we don't pass it explicitly so the
-- INSERT can't accidentally bypass the default.
INSERT INTO resource_request
    (requester_user_ref, target_asset_id, requested_capability, reason)
VALUES ($1, $2, $3, $4)
RETURNING id, requester_user_ref, target_asset_id, requested_capability,
          reason, state, decided_at, decided_by_user_ref, decision_reason,
          expires_at, requested_at;

-- name: GetResourceRequest :one
-- Single-row read by id. Used by the decide handler to load the
-- row's `state` before transitioning + by the admin viewer's
-- detail page.
SELECT id, requester_user_ref, target_asset_id, requested_capability,
       reason, state, decided_at, decided_by_user_ref, decision_reason,
       expires_at, requested_at
FROM resource_request
WHERE id = $1;

-- name: MarkRequestGranted :one
-- Atomic CAS: only transitions pending → granted. Returns the
-- updated row so the handler can build the audit changeset
-- (1.17.D RecordChange) + the notification payload from a single
-- query. Returns no rows when the request was already decided —
-- the handler maps that to a 409.
UPDATE resource_request
   SET state = 'granted',
       decided_at = NOW(),
       decided_by_user_ref = $2,
       decision_reason = $3,
       expires_at = $4
 WHERE id = $1 AND state = 'pending'
 RETURNING id, requester_user_ref, target_asset_id, requested_capability,
           reason, state, decided_at, decided_by_user_ref, decision_reason,
           expires_at, requested_at;

-- name: MarkRequestDenied :one
-- Symmetric to MarkRequestGranted.
UPDATE resource_request
   SET state = 'denied',
       decided_at = NOW(),
       decided_by_user_ref = $2,
       decision_reason = $3
 WHERE id = $1 AND state = 'pending'
 RETURNING id, requester_user_ref, target_asset_id, requested_capability,
           reason, state, decided_at, decided_by_user_ref, decision_reason,
           expires_at, requested_at;

-- name: MarkRequestExpired :execrows
-- Called from the CapabilitySweeper request-cascade callback when
-- a granted-with-request_ref user_capability_grants row reaps.
-- Returns rows-affected so the sweeper logs "we expired N
-- requests" — and gracefully handles the race where a sweep
-- competes with a manual operator action.
UPDATE resource_request
   SET state = 'expired'
 WHERE id = $1 AND state = 'granted';

-- name: ListRequestsForRequester :many
-- Requester-facing list (/account/requests). Most-recent first.
-- limit is hard-capped at the handler layer (50 default; max 200).
SELECT id, requester_user_ref, target_asset_id, requested_capability,
       reason, state, decided_at, decided_by_user_ref, decision_reason,
       expires_at, requested_at
FROM resource_request
WHERE requester_user_ref = $1
ORDER BY requested_at DESC
LIMIT $2;

-- name: ListPendingRequests :many
-- Approver-facing list (/admin/requests). Oldest pending first so
-- nothing rots at the bottom of the queue. The handler filters
-- the results by the approver's capability scope before returning
-- — this query intentionally surfaces ALL pending so the per-row
-- gate is the authoritative filter.
SELECT id, requester_user_ref, target_asset_id, requested_capability,
       reason, state, decided_at, decided_by_user_ref, decision_reason,
       expires_at, requested_at
FROM resource_request
WHERE state = 'pending'
ORDER BY requested_at ASC
LIMIT $1
OFFSET $2;

-- name: CountPendingRequests :one
-- Total count of pending rows (no capability filter — the badge
-- callers post-filter or accept the unfiltered count).
SELECT COUNT(*)::BIGINT FROM resource_request WHERE state = 'pending';

-- name: GetGrantsByRequestRef :many
-- Used by the CapabilitySweeper request-cascade callback hook.
-- Mirrors the existing user_capability_grants query shape.
SELECT user_ref, capability_code, granted_at, granted_by_user_ref,
       note, team_id, expires_at, request_ref
FROM user_capability_grants
WHERE request_ref = $1;
