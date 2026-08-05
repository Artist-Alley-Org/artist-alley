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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
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

// CapSystemAdmin is the wildcard. Spelled here rather than reached for
// as a literal at three gate sites.
const CapSystemAdmin = "system.admin"

// CapAccessRequest is the capability the "request access" affordance
// submits, and the ONLY one an asset's owner may decide (migration
// 00035, #881).
//
// It confers nothing. No gate reads it — visibility.ContentReadable
// consults exactly system.admin and content.read.all — so granting it
// means "the owner agreed", not "and now you can see it". Per-asset
// unlocking is #912; ADR 0064's "Why the grant path is deferred" still
// holds, and the UI says so rather than implying an unlock that will
// not happen.
//
// Its narrowness is what makes the owner disjunct in http.go safe.
// requested_capability is requester-controlled input, so an owner who
// could decide ANY request could be talked into granting system.admin
// from a panel on their own work. See the migration for the full note.
const CapAccessRequest = "content.access.request"

// approverCapabilities are the codes whose global holders act on the
// request queue. Used for the create-time notification fan-out — NOT as
// an authorisation answer; the gates below ask auth directly.
var approverCapabilities = []string{CapShareGrant, CapSystemAdmin}

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

// Submit files a pending request, or returns the caller's existing one.
//
// The handler is permissive about WHO may ask — any authenticated user
// may submit a request, and the decide gate settles whether to grant —
// but the capability named must exist. It is deliberately NOT permissive
// about the string itself: this field feeds an authorisation decision,
// so it may only name a real capability (#434).
//
// # Duplicate asks (#881)
//
// created=false means the ask already existed and this call changed
// nothing. One ask is (requester, asset, capability), and the rule is:
//
//   - A second ask while the first is still PENDING coalesces onto it.
//     It is the same question asked twice, and filing it again would put
//     two rows in the approver's queue for one decision — the approver
//     would have to deny one of them, which writes a "denied" the
//     requester never earned.
//   - A DECIDED request does not block a new one. denied and expired are
//     terminal for the ROW, not for the person; state.go already says
//     re-issuing means "a new resource_request row rather than walking a
//     row backwards". Reading terminality as "denied once, never again"
//     would turn a single no into a permanent one, which no surface
//     tells the user is happening. A granted-then-expired request in
//     particular MUST be re-askable, or expiry would be a one-way door.
//
// Concurrency is settled by the storage layer, not by this read: two
// simultaneous submits both see no pending row, both INSERT, and the
// partial unique index (migration 00035) fails the loser with 23505.
// The loser re-reads the winner and returns created=false, so the
// coalesce holds under a double-click as well as a slow one.
//
// Audit fires only on a real insert. So does the notification — an
// approver should not be re-pinged because the requester refreshed.
func (h *Handler) Submit(ctx context.Context, req *http.Request, in SubmitInput) (row ResourceRequest, created bool, err error) {
	q := New(h.Pool)

	var known bool
	if err := h.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM capabilities WHERE code = $1)`,
		in.RequestedCapability,
	).Scan(&known); err != nil {
		return ResourceRequest{}, false, fmt.Errorf("requests: capability lookup: %w", err)
	}
	if !known {
		return ResourceRequest{}, false, fmt.Errorf("%w: %q", ErrUnknownCapability, in.RequestedCapability)
	}

	ask := FindPendingRequestForAskParams{
		RequesterUserRef:    in.RequesterUserRef,
		TargetAssetID:       pgtype.UUID{Bytes: in.TargetAssetID, Valid: true},
		RequestedCapability: in.RequestedCapability,
	}
	existing, err := q.FindPendingRequestForAsk(ctx, ask)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ResourceRequest{}, false, fmt.Errorf("requests: find pending: %w", err)
	}

	row, err = q.InsertResourceRequest(ctx, InsertResourceRequestParams{
		RequesterUserRef:    in.RequesterUserRef,
		TargetAssetID:       pgtype.UUID{Bytes: in.TargetAssetID, Valid: true},
		RequestedCapability: in.RequestedCapability,
		Reason:              in.Reason,
	})
	if err != nil {
		// Lost the race against a concurrent identical submit. The
		// winner's row IS the answer to this call — same requester,
		// same asset, same capability, still pending.
		// SQLSTATE 23505 = unique_violation, spelled as the literal
		// the rest of the tree spells it (assets.isPgUniqueViolation,
		// mcp_registry.Create).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			existing, findErr := q.FindPendingRequestForAsk(ctx, ask)
			if findErr == nil {
				return existing, false, nil
			}
		}
		return ResourceRequest{}, false, fmt.Errorf("requests: insert: %w", err)
	}

	if h.audit != nil {
		h.audit.RequestCreated(ctx, req,
			in.RequesterUserRef,
			uuid.UUID(row.ID.Bytes).String(),
			in.TargetAssetID.String(),
			in.RequestedCapability,
			in.Reason)
	}

	h.notifySubmitted(ctx, row)

	// Local LRU evict + broadcast in one call. cache.Cache.Invalidate
	// does both (cache.go:Invalidate); the package-level
	// InvalidatePendingCountAll is broadcast-only for cross-
	// package callers that don't hold the local cache reference.
	h.invalidateCount(ctx)

	return row, true, nil
}

// AssetOwnerRef resolves who owns the asset a request targets.
//
// Delegates to shares.ObjectOwnerRef — the single expression of "who
// owns this shareable object" (#893). A second ownership notion here is
// exactly what epic #665 exists to prevent, and #892 and #904 each spent
// a sprint undoing one. ok=false is "no resolvable owner" and every
// caller reads it as a denial, per that function's fail-closed contract.
func (h *Handler) AssetOwnerRef(ctx context.Context, assetID uuid.UUID) (int64, bool, error) {
	return shares.ObjectOwnerRef(ctx, h.Pool, federation.ShareObjectKindAsset, assetID)
}

// Get reads one request by id. ErrRequestNotFound when there is none.
func (h *Handler) Get(ctx context.Context, id uuid.UUID) (ResourceRequest, error) {
	row, err := New(h.Pool).GetResourceRequest(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ResourceRequest{}, ErrRequestNotFound
		}
		return ResourceRequest{}, fmt.Errorf("requests: get: %w", err)
	}
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

// ListPendingForOwner returns pending requests against assets the
// caller owns, oldest first. The owner-facing half of #881: an artist
// whose work was requested needs a queue they can reach without holding
// share.grant, because /admin/requests is gated on capabilities they
// have no reason to hold.
func (h *Handler) ListPendingForOwner(ctx context.Context, ownerRef int64, limit, offset int32) ([]ResourceRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return New(h.Pool).ListPendingRequestsForOwner(ctx,
		ListPendingRequestsForOwnerParams{
			OwnerUserRef: &ownerRef,
			Limit:        limit,
			Offset:       offset,
		})
}

// CountPendingForOwner is the badge value for ListPendingForOwner.
// Uncached: it is per-owner, so the single-key cache the global count
// uses cannot hold it, and a per-owner key would need its own
// invalidation edge for a number one page reads.
func (h *Handler) CountPendingForOwner(ctx context.Context, ownerRef int64) (int64, error) {
	n, err := New(h.Pool).CountPendingRequestsForOwner(ctx, &ownerRef)
	if err != nil {
		return 0, fmt.Errorf("requests: count pending for owner: %w", err)
	}
	return n, nil
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

// notifySubmitted tells the people who can act that a request arrived
// (#881).
//
// Before this, the only Notify in the package fired on the DECIDE path,
// to the requester. Creating a request notified nobody: the approver
// queue filled in silence and /admin/requests was a page you had to
// think to visit. A request nobody is told about is a request nobody
// answers, which is what made the placeholder's "ask" meaningless.
//
// Recipients: the asset's owner (the person with the strongest claim to
// decide, and since #881 the person who CAN) plus the global holders of
// the approver capabilities. Deduplicated, and the requester is never
// notified of their own ask — Writer.Notify drops self-notifications
// anyway, but relying on that would make the dedupe depend on a
// downstream implementation detail.
//
// # What the payload may say
//
// The requester may not see this asset's title, and neither the owner's
// notification nor the approvers' carries one. The keys here are exactly
// the ones the decide-path notification already uses, minus the decision
// fields — ids and a capability code, no titles, no filenames, no
// reason text. Approvers who need the detail open the queue, where the
// per-row gate applies. Keeping the two payloads the same shape also
// means one allow-list test covers both.
//
// Best-effort throughout: a failed lookup or a failed send logs at WARN
// and the request stands. The row is the source of truth; the queue is
// still correct even if nobody was pinged.
func (h *Handler) notifySubmitted(ctx context.Context, row ResourceRequest) {
	if h.notifier == nil {
		return
	}
	requestID := uuid.UUID(row.ID.Bytes)
	assetID := uuid.UUID(row.TargetAssetID.Bytes)

	recipients := make([]int64, 0, 4)
	seen := map[int64]bool{row.RequesterUserRef: true}
	add := func(ref int64) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		recipients = append(recipients, ref)
	}

	if ownerRef, ok, err := h.AssetOwnerRef(ctx, assetID); err != nil {
		h.warn(ctx, "requests.notify.owner_lookup_failed", requestID, err)
	} else if ok {
		add(ownerRef)
	}

	holders, err := New(h.Pool).ListGlobalCapabilityHolders(ctx, approverCapabilities)
	if err != nil {
		h.warn(ctx, "requests.notify.approver_lookup_failed", requestID, err)
	}
	for _, ref := range holders {
		add(ref)
	}

	payload := map[string]any{
		"request_id": requestID.String(),
		"capability": row.RequestedCapability,
		"asset_id":   assetID.String(),
	}
	actor := row.RequesterUserRef
	for _, ref := range recipients {
		if err := h.notifier.Notify(ctx, ref, &actor,
			notifications.VerbResourceRequestReceived,
			notifications.TargetKindRequest, requestID.String(),
			payload,
		); err != nil {
			h.warn(ctx, "requests.notify.failed", requestID, err)
		}
	}
}

// warn is the package's one best-effort failure log shape.
func (h *Handler) warn(ctx context.Context, msg string, requestID uuid.UUID, err error) {
	if h.Logger == nil {
		return
	}
	h.Logger.LogAttrs(ctx, slog.LevelWarn, msg,
		slog.String("request_id", requestID.String()),
		slog.String("err", err.Error()),
	)
}

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
	verb := notifications.VerbResourceRequestDenied
	if granted {
		verb = notifications.VerbResourceRequestApproved
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
		verb, notifications.TargetKindRequest, in.RequestID.String(),
		payload)
	if err != nil {
		h.warn(ctx, "requests.notify.failed", in.RequestID, err)
	}
}
