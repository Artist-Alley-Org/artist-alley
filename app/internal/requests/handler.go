// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.E — resource_request lifecycle handler.
//
// The Handler is the package's primary surface. Three transition
// methods (Submit / Grant / Deny) + one reaper hook (MarkExpired)
// + a list/count read pair. Each transition method:
//
//   1. Validates the proposed transition via state.ValidateTransition
//   2. Wraps the write in a pgx tx so the resource_request CAS, the
//      user_capability_grants insert (Grant only), and the audit
//      emit (1.17.D-style structured changeset where applicable)
//      either all commit or all roll back
//   3. Best-effort notification to the requester via the existing
//      notifications.Writer.Notify(ctx, Input) path
//   4. Best-effort cache invalidation for the per-approver
//      pending-count badge
//
// Best-effort notification + cache failures log at WARN and never
// fail the calling transition — same convention as the audit
// recorder. The DB state is the source of truth.
//
// # Why a thin "notifier" interface, not the concrete *notifications.Writer
//
// The notifications package depends on cache.Registry + jobs +
// prefs; the requests package needs none of those. The thin
// interface lets the api.go composition wire a tiny adapter
// (mirroring socialNotifyAdapter at api.go:1019-1032) without
// pulling the full notifications surface into the request lifecycle.

package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// CapShareGrant is the capability code an approver needs to decide
// a request. Held globally OR on the asset's owning team — the
// approver-gate uses Identity.Can("share.grant", InTeam(teamID))
// at the api layer.
//
// Seeded into the catalogue by migration 00003 (#356). Before that it
// was referenced here but had no row in `capabilities`, so nothing
// could ever hold it and the OR-fallback was dead code — the surface
// was effectively system.admin-only. Operators grant it explicitly via
// /admin/users/{ref}/grants; no role-seed default.
const CapShareGrant = "share.grant"

// CapRequestsRead gates reading the admin request queue. An approver
// (share.grant) still reads the queue they act on; this cap lets a
// read-only auditor role view it without being able to decide (#356).
const CapRequestsRead = "requests.read"

// SubmitInput is the parameter list for Submit. Kept as a struct
// so future fields (priority, team_scope_request, etc.) don't
// require a positional-arg signature churn across every caller.
type SubmitInput struct {
	RequesterUserRef    int64
	TargetAssetID       uuid.UUID
	RequestedCapability string
	Reason              string // free-text justification; may be empty
}

// DecideInput is the shared input shape for Grant + Deny. expiresAt
// is consumed only by Grant; Deny ignores it. The handler validates
// per-decision; a zero ExpiresAt on Grant means "no auto-expiry"
// (permanent grant).
type DecideInput struct {
	RequestID      uuid.UUID
	ApproverRef    int64
	DecisionReason string
	ExpiresAt      time.Time // zero = permanent (Grant only)
}

// auditRecorder is the slice of *audit.Recorder this package needs.
// Interface so tests can substitute a fake. Production wires the
// concrete pool-bound Recorder.
type auditRecorder interface {
	RequestCreated(ctx context.Context, req *http.Request, requesterRef int64, requestID, assetID, capability, reason string)
	RequestGranted(ctx context.Context, req *http.Request, approverRef, requesterRef int64, requestID, assetID, capability, decisionReason string, expiresAt time.Time)
	RequestDenied(ctx context.Context, req *http.Request, approverRef, requesterRef int64, requestID, assetID, capability, decisionReason string)
	RequestExpired(ctx context.Context, requesterRef int64, requestID, capability string, expiredAt time.Time)
}

// notifier is the thin adapter the api.go composition wraps around
// notifications.Writer.Notify. Pulling the full notifications API
// into this package would create a heavy dep edge (cache + jobs +
// prefs); the adapter pattern matches socialNotifyAdapter precedent.
type notifier interface {
	Notify(ctx context.Context, recipientRef int64, actorRef *int64, verb, targetKind, targetID string, payload map[string]any) error
}

// Handler is the public surface. Construct via NewHandler at boot;
// SetAuditRecorder + SetNotifier are post-construction setters
// matching users.Handler / sysconfig.Handler convention.
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	registry *cache.Registry
	counts   *pendingCountCache

	audit    auditRecorder
	notifier notifier
}

// NewHandler builds the Handler. registry may be nil (tests
// without LISTEN/NOTIFY); the count cache then degrades to direct
// PG reads. audit + notifier are nil until SetAuditRecorder /
// SetNotifier are called at boot.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	return &Handler{
		Pool:     pool,
		Logger:   logger,
		registry: registry,
		counts:   newPendingCountCache(registry, logger),
	}
}

// SetAuditRecorder wires the audit pipeline post-construction.
// Mirrors users.Handler.SetAuditRecorder.
func (h *Handler) SetAuditRecorder(rec auditRecorder) { h.audit = rec }

// SetNotifier wires the notification adapter post-construction.
func (h *Handler) SetNotifier(n notifier) { h.notifier = n }

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrRequestAlreadyDecided is returned by Grant + Deny when the
// CAS update finds the row in a non-pending state. The api layer
// maps to HTTP 409 with a "already decided" payload.
var ErrRequestAlreadyDecided = errors.New("requests: already decided")

// ErrRequestNotFound is returned when GetResourceRequest finds no
// row matching the id. Mapped to HTTP 404.
var ErrRequestNotFound = errors.New("requests: not found")

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

// ErrUnknownCapability is returned by Submit when requested_capability
// names something that is not in the capabilities registry (#434). The
// DB enforces this too (FK, migration 00009); checking here turns a
// constraint violation into a clean 400 that names the problem.
var ErrUnknownCapability = errors.New("requests: unknown capability")

// Submit creates a fresh pending request. The handler is permissive
// about WHO may ask — any authenticated user may submit a request, and
// the approver gate decides whether to grant — but the capability named
// must exist. It is deliberately NOT permissive about the string
// itself: this field feeds an authorisation decision, so it may only
// name a real capability (#434).
//
// Note what this does not settle: a real capability is not necessarily
// a REQUESTABLE one. Nothing stops a request naming system.admin, and
// that rule belongs to the grant path (ADR 0064). Audit fires alongside the row
// insert; the per-admin pending-count cache is wildcard-evicted
// so every approver's badge picks up the new pending entry on
// their next read.
func (h *Handler) Submit(ctx context.Context, req *http.Request, in SubmitInput) (ResourceRequest, error) {
	q := New(h.Pool)

	var known bool
	if err := h.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM capabilities WHERE code = $1)`,
		in.RequestedCapability,
	).Scan(&known); err != nil {
		return ResourceRequest{}, fmt.Errorf("requests: capability lookup: %w", err)
	}
	if !known {
		return ResourceRequest{}, fmt.Errorf("%w: %q", ErrUnknownCapability, in.RequestedCapability)
	}

	row, err := q.InsertResourceRequest(ctx, InsertResourceRequestParams{
		RequesterUserRef:    in.RequesterUserRef,
		TargetAssetID:       pgtype.UUID{Bytes: in.TargetAssetID, Valid: true},
		RequestedCapability: in.RequestedCapability,
		Reason:              in.Reason,
	})
	if err != nil {
		return ResourceRequest{}, fmt.Errorf("requests: insert: %w", err)
	}

	if h.audit != nil {
		h.audit.RequestCreated(ctx, req,
			in.RequesterUserRef,
			uuid.UUID(row.ID.Bytes).String(),
			in.TargetAssetID.String(),
			in.RequestedCapability,
			in.Reason)
	}

	// Local LRU evict + broadcast in one call. cache.Cache.Invalidate
	// does both (cache.go:Invalidate); the package-level
	// InvalidatePendingCountAll is broadcast-only for cross-
	// package callers that don't hold the local cache reference.
	h.invalidateCount(ctx)

	return row, nil
}

// ---------------------------------------------------------------------------
// Grant
// ---------------------------------------------------------------------------

// Grant transitions a pending request to granted. The
// user_capability_grants row insert happens in the SAME tx as the
// resource_request CAS update so the audit + cache invariants
// can't observe a half-committed state. expiresAt zero means
// permanent.
//
// Returns ErrRequestAlreadyDecided when the row isn't pending
// (race against another approver). Returns ErrRequestNotFound when
// the id matches no row. Other errors bubble up as 500.
func (h *Handler) Grant(ctx context.Context, req *http.Request, in DecideInput) (ResourceRequest, error) {
	// Pre-load the request so we can build the audit + notification
	// payloads even if the CAS races us. Also gives us the
	// requester ref + asset id for the grant insert.
	q := New(h.Pool)
	pre, err := q.GetResourceRequest(ctx, pgtype.UUID{Bytes: in.RequestID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResourceRequest{}, ErrRequestNotFound
		}
		return ResourceRequest{}, fmt.Errorf("requests: pre-load: %w", err)
	}
	if pre.State != string(RequestStatePending) {
		return ResourceRequest{}, ErrRequestAlreadyDecided
	}
	if err := ValidateTransition(RequestState(pre.State), RequestStateGranted); err != nil {
		return ResourceRequest{}, err
	}

	var row ResourceRequest
	err = pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		txq := New(tx)
		expires := pgtype.Timestamptz{}
		if !in.ExpiresAt.IsZero() {
			expires = pgtype.Timestamptz{Time: in.ExpiresAt, Valid: true}
		}
		updated, err := txq.MarkRequestGranted(ctx, MarkRequestGrantedParams{
			ID:               pgtype.UUID{Bytes: in.RequestID, Valid: true},
			DecidedByUserRef: &in.ApproverRef,
			DecisionReason:   in.DecisionReason,
			ExpiresAt:        expires,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRequestAlreadyDecided
			}
			return fmt.Errorf("mark granted: %w", err)
		}

		// Insert the consequent user_capability_grants row in the
		// same tx. team_id is NULL (global grant) — the api layer
		// can swap to team-scoped via a future enhancement; for
		// MVP, the grant matches what the requester asked for
		// without further scoping. request_ref ties the two for
		// the sweeper-cascade.
		_, err = tx.Exec(ctx,
			`INSERT INTO user_capability_grants
			    (user_ref, capability_code, granted_by_user_ref, note,
			     team_id, expires_at, request_ref)
			 VALUES ($1, $2, $3, $4, NULL, $5, $6)
			 ON CONFLICT (user_ref, capability_code, team_id) DO UPDATE SET
			    granted_at = NOW(),
			    granted_by_user_ref = EXCLUDED.granted_by_user_ref,
			    note = EXCLUDED.note,
			    expires_at = EXCLUDED.expires_at,
			    request_ref = EXCLUDED.request_ref`,
			pre.RequesterUserRef,
			pre.RequestedCapability,
			&in.ApproverRef,
			"granted via request "+in.RequestID.String(),
			expires,
			pgtype.UUID{Bytes: in.RequestID, Valid: true},
		)
		if err != nil {
			return fmt.Errorf("insert grant: %w", err)
		}
		row = updated
		return nil
	})
	if err != nil {
		return ResourceRequest{}, err
	}

	// Post-commit best-effort side effects. Audit + notification
	// + cache eviction — any of these failing logs at WARN but
	// doesn't undo the decision.
	if h.audit != nil {
		h.audit.RequestGranted(ctx, req,
			in.ApproverRef,
			pre.RequesterUserRef,
			in.RequestID.String(),
			uuid.UUID(pre.TargetAssetID.Bytes).String(),
			pre.RequestedCapability,
			in.DecisionReason,
			in.ExpiresAt)
	}
	h.notifyDecision(ctx, pre, in, true /* granted */)
	h.invalidateCount(ctx)

	return row, nil
}

// ---------------------------------------------------------------------------
// Deny
// ---------------------------------------------------------------------------

// Deny transitions a pending request to denied. Symmetric to
// Grant but no user_capability_grants side-effect.
func (h *Handler) Deny(ctx context.Context, req *http.Request, in DecideInput) (ResourceRequest, error) {
	q := New(h.Pool)
	pre, err := q.GetResourceRequest(ctx, pgtype.UUID{Bytes: in.RequestID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResourceRequest{}, ErrRequestNotFound
		}
		return ResourceRequest{}, fmt.Errorf("requests: pre-load: %w", err)
	}
	if pre.State != string(RequestStatePending) {
		return ResourceRequest{}, ErrRequestAlreadyDecided
	}
	if err := ValidateTransition(RequestState(pre.State), RequestStateDenied); err != nil {
		return ResourceRequest{}, err
	}

	row, err := q.MarkRequestDenied(ctx, MarkRequestDeniedParams{
		ID:               pgtype.UUID{Bytes: in.RequestID, Valid: true},
		DecidedByUserRef: &in.ApproverRef,
		DecisionReason:   in.DecisionReason,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResourceRequest{}, ErrRequestAlreadyDecided
		}
		return ResourceRequest{}, fmt.Errorf("requests: mark denied: %w", err)
	}

	if h.audit != nil {
		h.audit.RequestDenied(ctx, req,
			in.ApproverRef,
			pre.RequesterUserRef,
			in.RequestID.String(),
			uuid.UUID(pre.TargetAssetID.Bytes).String(),
			pre.RequestedCapability,
			in.DecisionReason)
	}
	h.notifyDecision(ctx, pre, in, false /* denied */)
	h.invalidateCount(ctx)

	return row, nil
}

// invalidateCount evicts the local LRU entry + broadcasts via the
// registry. cache.Cache.Invalidate does both in one call — same
// pattern users.Handler uses for the byRef cache.
func (h *Handler) invalidateCount(ctx context.Context) {
	if h.counts == nil {
		return
	}
	_ = h.counts.c.Invalidate(ctx, countCacheKey)
}

// ---------------------------------------------------------------------------
// MarkExpired — called from the CapabilitySweeper cascade
// ---------------------------------------------------------------------------

// MarkExpired transitions a granted request to expired. Called
// from the auth.CapabilitySweeper's request-cascade callback when
// the linked grant reaps. Best-effort by contract — failure here
// logs at WARN but does NOT undo the grant's expiry (the grant is
// already gone by the time this fires; the request would just
// stay stuck at granted which the operator can clean up by hand).
//
// expiredAt is the timestamp on the reaped grant; passed back to
// the audit emit so the lifecycle reconstruction has the
// expires_at the grant actually used.
func (h *Handler) MarkExpired(ctx context.Context, requestID uuid.UUID, expiredAt time.Time) error {
	q := New(h.Pool)
	n, err := q.MarkRequestExpired(ctx, pgtype.UUID{Bytes: requestID, Valid: true})
	if err != nil {
		return fmt.Errorf("requests: mark expired: %w", err)
	}
	if n == 0 {
		// Already-decided race — the operator denied the request
		// between the grant insert and the sweeper-time reap, OR
		// another sweeper tick raced us. Either way: no state
		// change; we don't audit a phantom transition.
		return nil
	}

	// We need the requester + capability to attribute the audit.
	// One additional read; cheap because this only fires on the
	// rare reap path.
	pre, getErr := q.GetResourceRequest(ctx, pgtype.UUID{Bytes: requestID, Valid: true})
	if getErr == nil && h.audit != nil {
		h.audit.RequestExpired(ctx,
			pre.RequesterUserRef,
			requestID.String(),
			pre.RequestedCapability,
			expiredAt)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ListForRequester returns the requester's own requests, newest
// first. limit defaults to 50 + caps at 200; the api layer
// enforces those bounds.
func (h *Handler) ListForRequester(ctx context.Context, requesterRef int64, limit int32) ([]ResourceRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return New(h.Pool).ListRequestsForRequester(ctx,
		ListRequestsForRequesterParams{
			RequesterUserRef: requesterRef,
			Limit:            limit,
		})
}

// ListPending returns all pending requests, oldest first. limit
// defaults to 50 + caps at 200. The approver-side capability
// filter happens at the api layer per-row, not in this query.
func (h *Handler) ListPending(ctx context.Context, limit, offset int32) ([]ResourceRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return New(h.Pool).ListPendingRequests(ctx,
		ListPendingRequestsParams{Limit: limit, Offset: offset})
}

// CountPending returns the total pending count. Cache-fronted
// under the single key "all" because at MVP every approver sees
// the same unfiltered count; the per-approver capability filter
// is a polish-phase follow-up. The cache.Registry-wide
// NOTIFY/LISTEN broadcasts evict this single key on every
// transition, so the badge never serves stale.
//
// approverRef is kept on the signature so the call sites + the
// cache contract don't churn when per-approver filtering ships.
func (h *Handler) CountPending(ctx context.Context, approverRef int64) (int64, error) {
	const key = countCacheKey
	if h.counts != nil {
		if v, ok := h.counts.c.Get(key); ok {
			return v, nil
		}
	}
	n, err := New(h.Pool).CountPendingRequests(ctx)
	if err != nil {
		return 0, fmt.Errorf("requests: count pending: %w", err)
	}
	if h.counts != nil {
		h.counts.c.Add(key, n)
	}
	_ = approverRef // reserved for per-approver filtering follow-up
	return n, nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// notifyDecision pushes a "your request was decided" notification
// to the requester via the existing notifications.Writer.Notify
// path. Best-effort — failure logs at WARN; the decision stands.
//
// The verbs are the pre-seeded ones in notifications/events.go
// (1.17.E was anticipated by the notifications package as
// "VerbResourceRequestApproved" / "VerbResourceRequestDenied").
func (h *Handler) notifyDecision(ctx context.Context, pre ResourceRequest, in DecideInput, granted bool) {
	if h.notifier == nil {
		return
	}
	verb := "resource_request_denied"
	if granted {
		verb = "resource_request_approved"
	}
	payload := map[string]any{
		"request_id":      in.RequestID.String(),
		"capability":      pre.RequestedCapability,
		"asset_id":        uuid.UUID(pre.TargetAssetID.Bytes).String(),
		"decision_reason": in.DecisionReason,
	}
	if granted && !in.ExpiresAt.IsZero() {
		payload["expires_at"] = in.ExpiresAt.UTC().Format(time.RFC3339)
	}
	actor := in.ApproverRef
	err := h.notifier.Notify(ctx,
		pre.RequesterUserRef, &actor,
		verb, "request", in.RequestID.String(),
		payload)
	if err != nil && h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn,
			"requests.notify.failed",
			slog.String("request_id", in.RequestID.String()),
			slog.String("err", err.Error()),
		)
	}
}
