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

-- name: FindPendingRequestForAsk :one
-- The coalesce read (#881). One ask is (requester, asset, capability);
-- a second one while the first is still pending is the same question
-- asked twice, not a new question, so Submit returns this row instead
-- of filing another. Matches the partial unique index
-- resource_request_one_pending_per_ask exactly — if these two ever
-- disagree, the INSERT's 23505 recovery path reads nothing back.
SELECT id, requester_user_ref, target_asset_id, requested_capability,
       reason, state, decided_at, decided_by_user_ref, decision_reason,
       expires_at, requested_at
FROM resource_request
WHERE requester_user_ref = $1
  AND target_asset_id = $2
  AND requested_capability = $3
  AND state = 'pending';

-- name: ListPendingRequestsForOwner :many
-- Owner-facing queue (/account/requests/incoming, #881): pending
-- requests against assets THIS user owns. Oldest first, same ordering
-- as the approver queue.
--
-- Joins assets rather than trusting a denormalised owner column on the
-- request: ownership is the assets table's fact, and #665 exists
-- because a second copy of it drifts. The Go path resolves the same
-- fact through shares.ObjectOwnerRef; both read assets.owner_user_ref.
--
-- Soft-deleted assets drop out. An owner has no decision to make about
-- access to something they deleted.
SELECT rr.id, rr.requester_user_ref, rr.target_asset_id,
       rr.requested_capability, rr.reason, rr.state, rr.decided_at,
       rr.decided_by_user_ref, rr.decision_reason, rr.expires_at,
       rr.requested_at
FROM resource_request rr
JOIN assets a ON a.id = rr.target_asset_id
WHERE rr.state = 'pending'
  AND a.owner_user_ref = $1
  AND a.deleted_at IS NULL
ORDER BY rr.requested_at ASC
LIMIT $2
OFFSET $3;

-- name: CountPendingRequestsForOwner :one
-- Badge value for the owner queue. Same predicate as
-- ListPendingRequestsForOwner; kept adjacent so they change together.
SELECT COUNT(*)::BIGINT
FROM resource_request rr
JOIN assets a ON a.id = rr.target_asset_id
WHERE rr.state = 'pending'
  AND a.owner_user_ref = $1
  AND a.deleted_at IS NULL;

-- name: ListGlobalCapabilityHolders :many
-- Notification fan-out for a newly created request (#881): every user
-- who could act on it. Resolves a capability the same way
-- UserHoldsSystemAdmin does — role grant UNION explicit grant, minus an
-- explicit revoke, approved users only — but for a SET of codes and
-- returning the holders rather than answering about one of them.
--
-- Team-scoped rows are excluded (team_id IS NULL on both sides): the
-- approver queue this feeds is the global one, and a team-scoped
-- approver has no view of a request that names no team.
--
-- The revoke check is per-code, not per-caller: a user revoked
-- share.grant but holding system.admin is still an approver, and a
-- coarser NOT EXISTS over the whole code set would silently drop them.
--
-- This is a NOTIFICATION list, never an authorisation answer. The
-- decide gate resolves the caller's own capabilities through auth;
-- appearing here confers nothing.
SELECT DISTINCT h.ref
FROM (
    SELECT ur.user_ref AS ref, rc.capability_code AS code
      FROM user_roles ur
      JOIN role_capabilities rc ON rc.role_id = ur.role_id
     WHERE ur.team_id IS NULL
       AND rc.capability_code = ANY($1::text[])
    UNION
    SELECT g.user_ref AS ref, g.capability_code AS code
      FROM user_capability_grants g
     WHERE g.team_id IS NULL
       AND g.capability_code = ANY($1::text[])
       AND (g.expires_at IS NULL OR g.expires_at > NOW())
) h
JOIN "user" u ON u.ref = h.ref AND u.approved = 1
WHERE NOT EXISTS (
    SELECT 1 FROM user_capability_revokes r
     WHERE r.user_ref = h.ref
       AND r.team_id IS NULL
       AND r.capability_code = h.code
);

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
