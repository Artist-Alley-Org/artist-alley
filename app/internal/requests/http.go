// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.E HTTP surface for the resource-request workflow.
//
// Six endpoints, two workflows, one table:
//
//	POST /assets/{id}/request-access                  — ask to SEE it (#881)
//	POST /account/trash/{kind}/{id}/request-restore   — ask for it BACK (#931)
//	GET  /account/requests                            — requester's own
//	GET  /account/requests/incoming                   — the queue you can
//	                                                    reach with no
//	                                                    capability
//	GET  /admin/requests                              — approver-facing list
//	POST /admin/requests/{id}/decide                  — the decision, for
//	                                                    either workflow
//
// # Two workflows behind one decide endpoint
//
// The two submit endpoints file the same row type and the same decide
// endpoint closes both — but `requested_capability` selects which gate
// applies and what granting DOES, and the two answers share nothing:
//
//	content.access.request  → share.grant | system.admin | the asset's
//	                          OWNER. Granting writes a capability grant.
//	content.restore.request → auth.CanRestoreDeleted against the
//	                          TARGET's deleter. NOT share.grant, NOT the
//	                          owner. Granting performs a restore and
//	                          writes no grant at all.
//
// They are one endpoint because they are one state machine (pending →
// granted/denied, CAS, 409 on a race) with one queue and one
// notification shape. They are two gates because the capability is
// requester-controlled input, and #881's lesson is that a gate must
// name exactly one payload — see DecideAdminRequest.
//
// The access approver gate is Identity.Can("share.grant", InTeam(teamID))
// where teamID comes from the target asset's owning team. Phase
// 1.17.E ships with the global-OR-team-scoped check; per-row
// asset-team gating is in the decide path. For the list endpoint
// we use a coarser gate (global share.grant OR system.admin) +
// the per-row asset-team gate fires at decide time anyway.

package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// HTTPHandler adapts requests.Handler to the openapi strict-server
// shim contract. Kept separate from Handler so the non-HTTP
// consumers (sweeper cascade) don't drag the openapi import.
type HTTPHandler struct {
	domain *Handler
	logger *slog.Logger
}

// NewHTTPHandler builds the adapter.
func NewHTTPHandler(h *Handler, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{domain: h, logger: logger}
}

// ---------------------------------------------------------------------------
// POST /assets/{id}/request-access
// ---------------------------------------------------------------------------

func (h *HTTPHandler) RequestAssetAccess(
	ctx context.Context,
	req openapi.RequestAssetAccessRequestObject,
) (openapi.RequestAssetAccessResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RequestAssetAccess401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.RequestAssetAccess400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	// `capability` is OPTIONAL and defaults to CapAccessRequest (#881).
	//
	// The UI never sends one, and that is the point. ADR 0064: "the field
	// is requester-controlled input … the only thing between that and the
	// file is an administrator clicking grant on a free-text value the
	// requester chose." A button on a placeholder must not be a way to
	// name your own capability, and the default is the inert marker code
	// that owners are allowed to decide.
	//
	// An explicit code is still accepted — the endpoint predates the
	// button and admin tooling asks for specific capabilities — but such
	// a request is only decidable by a real approver, never by the asset's
	// owner. See DecideAdminRequest.
	capability := req.Body.Capability
	if capability == nil || *capability == "" {
		capability = ptr(CapAccessRequest)
	}
	reason := ""
	if req.Body.Reason != nil {
		reason = *req.Body.Reason
	}
	if *capability == CapRestoreRequest {
		// The appeal marker is not askable here. This endpoint's guards
		// are about an ASSET's readability; an appeal's guards are about
		// a soft-deleted row's ownership and its deleter, and none of
		// them run on this path. Accepting the code would file a row the
		// restore gate would then honour, having never checked that the
		// requester owns the item or that it is even deleted.
		return openapi.RequestAssetAccess400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "use POST /account/trash/{kind}/{id}/request-restore to appeal a delete",
			},
		}, nil
	}
	row, created, err := h.domain.Submit(ctx, auth.RequestFromContext(ctx), SubmitInput{
		RequesterUserRef:    id.UserRef,
		TargetKind:          TargetKindAsset,
		TargetID:            uuid.UUID(req.Id),
		RequestedCapability: *capability,
		Reason:              reason,
	})
	if err != nil {
		if errors.Is(err, ErrUnknownCapability) {
			return openapi.RequestAssetAccess400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "unknown capability: " + *capability,
				},
			}, nil
		}
		return nil, fmt.Errorf("requests: submit: %w", err)
	}
	if !created {
		// The caller already had this ask pending. 200, not 201 — a
		// 201 would claim a row was created and hand the client a
		// "requested" id it would then file a duplicate against on the
		// next render. See Handler.Submit for the coalescing rule.
		return openapi.RequestAssetAccess200JSONResponse(rowToAPI(row)), nil
	}
	return openapi.RequestAssetAccess201JSONResponse(rowToAPI(row)), nil
}

// ptr is the one-line address-of helper the openapi optional fields
// need. Package-local; the codebase has no shared generic for it.
func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// POST /account/trash/{kind}/{id}/request-restore
// ---------------------------------------------------------------------------

// RequestRestore files a restoration appeal (#931).
//
// # One operation over three kinds
//
// Not three endpoints, following the trash listing's precedent: this is
// one PROJECTION of a question — "who owns this, who deleted it, is it
// still deleted" — that reads identically for assets, posts and
// collections. The three DELETE paths diverge substantially (storage
// unpin, federation Tombstone, cache fan-out) and so restore stays
// three endpoints; none of that divergence is visible in the three
// facts this gate reads.
//
// # No capability field, at all
//
// /assets/{id}/request-access accepts an optional `capability` for
// admin tooling that predates the button. This endpoint accepts none.
// #881's lesson is not "validate the requester's capability string"
// but "do not take one" — an appeal has exactly one thing it can ask
// for, so a field to say which would be a field with no legitimate
// second value and one dangerous one.
//
// # The guard order is the disclosure order
//
// Existence and soft-deletedness are checked before ownership, and both
// failures render the SAME 404 as "not yours". A caller learns nothing
// about rows they do not own — not that an id exists, not that it is
// deleted. The one status that is not 404 is 409, and it is only
// reachable by someone who has already demonstrated ownership.
func (h *HTTPHandler) RequestRestore(
	ctx context.Context,
	req openapi.RequestRestoreRequestObject,
) (openapi.RequestRestoreResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() || id.UserRef == 0 {
		return openapi.RequestRestore401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	kind := TargetKind(req.Kind)
	if !kind.Valid() {
		return openapi.RequestRestore400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "unknown kind"},
		}, nil
	}
	targetID := uuid.UUID(req.Id)

	// notFound is the single answer to every "you may not ask about
	// this" case, built once so no branch can accidentally say more.
	notFound := openapi.RequestRestore404JSONResponse{Error: "no such deleted item"}

	st, err := h.domain.TargetDeleteState(ctx, kind, targetID)
	if err != nil {
		return nil, fmt.Errorf("requests: target delete state: %w", err)
	}
	if !st.Exists || !st.SoftDeleted {
		return notFound, nil
	}

	ownerRef, ok, err := h.domain.TargetOwnerRef(ctx, kind, targetID)
	if err != nil {
		return nil, fmt.Errorf("requests: target owner: %w", err)
	}
	// ok=false covers a NULL owner (an orphaned asset) as well as a
	// missing row. Nobody may appeal on behalf of an item with no owner:
	// the whole premise of this endpoint is that the person asking is
	// the one who lost something.
	if !ok || ownerRef != id.UserRef {
		return notFound, nil
	}

	// Someone who can already restore it does not need to ask. 409
	// rather than filing a request nobody needs to answer — an appeal
	// from a person holding the undo is a queue item whose only correct
	// resolution is "you could have done that yourself".
	if auth.CanRestoreDeleted(id, st.DeletedBy) {
		return openapi.RequestRestore409JSONResponse{
			Error: "you can restore this yourself",
		}, nil
	}

	reason := ""
	if req.Body != nil && req.Body.Reason != nil {
		reason = *req.Body.Reason
	}

	row, created, err := h.domain.Submit(ctx, auth.RequestFromContext(ctx), SubmitInput{
		RequesterUserRef: id.UserRef,
		TargetKind:       kind,
		TargetID:         targetID,
		// Forced, never read from the request. See the doc comment.
		RequestedCapability: CapRestoreRequest,
		Reason:              reason,
	})
	if err != nil {
		return nil, fmt.Errorf("requests: submit restore: %w", err)
	}
	if !created {
		// Coalesced onto the caller's existing pending appeal. 200 for
		// the same reason RequestAssetAccess answers 200: a 201 would
		// claim a row was created.
		return openapi.RequestRestore200JSONResponse(rowToAPI(row)), nil
	}
	return openapi.RequestRestore201JSONResponse(rowToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// GET /account/requests
// ---------------------------------------------------------------------------

func (h *HTTPHandler) ListOwnRequests(
	ctx context.Context,
	req openapi.ListOwnRequestsRequestObject,
) (openapi.ListOwnRequestsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListOwnRequests401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	limit := int32(50)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	rows, err := h.domain.ListForRequester(ctx, id.UserRef, limit)
	if err != nil {
		return nil, fmt.Errorf("requests: list for requester: %w", err)
	}
	items := make([]openapi.ResourceRequest, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToAPI(r))
	}
	resp := openapi.ListOwnRequests200JSONResponse{}
	resp.Items = items
	return resp, nil
}

// ---------------------------------------------------------------------------
// GET /account/requests/incoming
// ---------------------------------------------------------------------------

// ListIncomingRequests is the OWNER's queue: pending requests against
// assets this caller owns (#881).
//
// No capability gate, and none is possible — the gate IS the ownership
// join in the query. The caller sees requests on their own work and
// nothing else, which is why this lives under /account rather than
// /admin: an artist has no reason to hold requests.read, and before
// this the only queue was one they could not open.
func (h *HTTPHandler) ListIncomingRequests(
	ctx context.Context,
	req openapi.ListIncomingRequestsRequestObject,
) (openapi.ListIncomingRequestsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListIncomingRequests401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	limit := int32(50)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	offset := int32(0)
	if req.Params.Offset != nil {
		offset = int32(*req.Params.Offset)
	}
	rows, err := h.domain.ListPendingForOwner(ctx, id.UserRef, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("requests: list pending for owner: %w", err)
	}
	total, err := h.domain.CountPendingForOwner(ctx, id.UserRef)
	if err != nil {
		return nil, fmt.Errorf("requests: count pending for owner: %w", err)
	}
	resp := openapi.ListIncomingRequests200JSONResponse{}
	resp.Items = h.withDecidable(ctx, id, rows)
	resp.Total = total
	return resp, nil
}

// ---------------------------------------------------------------------------
// GET /admin/requests
// ---------------------------------------------------------------------------

func (h *HTTPHandler) ListAdminRequests(
	ctx context.Context,
	req openapi.ListAdminRequestsRequestObject,
) (openapi.ListAdminRequestsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListAdminRequests401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	// Read gate: a read-only auditor (requests.read), an approver who
	// acts on this queue (share.grant, held globally), or system.admin.
	// Per-row asset-team scoping happens at decide time.
	if !id.Can(CapRequestsRead) && !id.Can(CapShareGrant) && !id.Can("system.admin") {
		return openapi.ListAdminRequests403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapRequestsRead + " capability required"},
		}, nil
	}
	limit := int32(50)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	offset := int32(0)
	if req.Params.Offset != nil {
		offset = int32(*req.Params.Offset)
	}
	rows, err := h.domain.ListPending(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("requests: list pending: %w", err)
	}
	total, err := h.domain.CountPending(ctx, id.UserRef)
	if err != nil {
		return nil, fmt.Errorf("requests: count pending: %w", err)
	}
	resp := openapi.ListAdminRequests200JSONResponse{}
	resp.Items = h.withDecidable(ctx, id, rows)
	resp.Total = total
	return resp, nil
}

// ---------------------------------------------------------------------------
// POST /admin/requests/{id}/decide
// ---------------------------------------------------------------------------

func (h *HTTPHandler) DecideAdminRequest(
	ctx context.Context,
	req openapi.DecideAdminRequestRequestObject,
) (openapi.DecideAdminRequestResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DecideAdminRequest401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.DecideAdminRequest400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	// Approver gate — three disjuncts (#881).
	//
	//   share.grant | system.admin   → decide anything
	//   the asset's OWNER            → decide access requests on it
	//
	// The owner disjunct exists because the person with the strongest
	// claim to decide was the one person who could not: an artist whose
	// work was requested had no route unless an operator handed them a
	// global grant capability, so every request routed through an
	// administrator who knows nothing about the work.
	//
	// It is NARROWER than the other two, deliberately.
	// requested_capability is requester-controlled input, so "the owner
	// may decide requests on their asset" without a capability
	// restriction would let anyone submit `system.admin` against a
	// stranger's asset and talk them into approving it from a panel that
	// looks like it is about a picture. An owner therefore decides only
	// CapAccessRequest — the marker code that confers nothing (migration
	// 00035) — and everything else still needs a real approver.
	//
	// A restoration appeal (#931) takes NEITHER of those routes. Its
	// gate is auth.CanRestoreDeleted against the TARGET's deleter, and
	// it is evaluated INSTEAD of the two above rather than alongside
	// them — an if/else, not an extra disjunct. Both other routes must
	// be closed to it:
	//
	//   share.grant  — sharing authority is not moderation authority.
	//                  A share approver who could reverse a moderator's
	//                  delete would make the delete gate decorative.
	//   ownership    — the owner IS the requester on an appeal. An
	//                  owner disjunct here would let anyone whose work
	//                  was removed approve its return, which is the
	//                  exact asymmetry auth.CanRestoreDeleted exists to
	//                  preserve.
	//
	// ownerMayDecide already refuses a non-CapAccessRequest row, so
	// ownership was never a live route to an appeal. This branch makes
	// that structural instead of incidental: the appeal never reaches a
	// gate that could say yes for the wrong reason.
	forbidden := func(msg string) openapi.DecideAdminRequest403JSONResponse {
		return openapi.DecideAdminRequest403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
		}
	}
	// Pre-load to learn WHICH gate applies. A failed load is not a 404
	// here: for a non-approver it must stay indistinguishable from "not
	// yours", so the flow falls through to the access branch, whose
	// ownerMayDecide answers 403 for a missing row exactly as before.
	// An approver reaching Grant/Deny with a bad id still gets the 404
	// those return.
	pre, preErr := h.domain.Get(ctx, uuid.UUID(req.Id))
	isAppeal := preErr == nil && pre.RequestedCapability == CapRestoreRequest

	if isAppeal {
		st, err := h.domain.TargetDeleteState(ctx, TargetKind(pre.TargetKind), uuid.UUID(pre.TargetID.Bytes))
		if err != nil {
			return nil, fmt.Errorf("requests: target delete state: %w", err)
		}
		// st.DeletedBy survives a restore, so an appeal someone else
		// already satisfied stays decidable by the person it was
		// addressed to. A target that has been hard-deleted reports nil
		// and fails closed to system.admin.
		if !auth.CanRestoreDeleted(id, st.DeletedBy) {
			return forbidden("only the person who deleted this item, or an administrator, can decide a restoration request"), nil
		}
	} else if !id.Can(CapShareGrant) && !id.Can(CapSystemAdmin) {
		if err := h.ownerMayDecide(ctx, id.UserRef, uuid.UUID(req.Id)); err != nil {
			return forbidden(CapShareGrant + " capability or ownership of the requested asset required"), nil
		}
	}

	reason := ""
	if req.Body.Reason != nil {
		reason = *req.Body.Reason
	}
	in := DecideInput{
		RequestID:      uuid.UUID(req.Id),
		ApproverRef:    id.UserRef,
		DecisionReason: reason,
	}
	if req.Body.ExpiresAt != nil {
		if req.Body.ExpiresAt.Before(time.Now()) {
			return openapi.DecideAdminRequest400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "expires_at must be in the future"},
			}, nil
		}
		in.ExpiresAt = *req.Body.ExpiresAt
	}

	var row ResourceRequest
	var err error
	switch req.Body.Decision {
	case openapi.Granted:
		row, err = h.domain.Grant(ctx, auth.RequestFromContext(ctx), in)
	case openapi.Denied:
		row, err = h.domain.Deny(ctx, auth.RequestFromContext(ctx), in)
	default:
		return openapi.DecideAdminRequest400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "decision must be granted or denied"},
		}, nil
	}
	if err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			return openapi.DecideAdminRequest404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "request not found"},
			}, nil
		}
		if errors.Is(err, ErrRequestAlreadyDecided) {
			return openapi.DecideAdminRequest409JSONResponse{Error: "request already decided"}, nil
		}
		if errors.Is(err, ErrExpiryOnRestore) {
			// 400 rather than silently dropping the field. A restore
			// that "expires" would have to re-delete the item, and
			// nothing does; an operator who set a date and saw a 200
			// would believe otherwise.
			return openapi.DecideAdminRequest400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "a restored item cannot expire; omit expires_at",
				},
			}, nil
		}
		if errors.Is(err, ErrTargetGone) {
			// The retention GC hard-deleted the item while the appeal
			// sat pending. Not a 500 — the request is well-formed and
			// the decider did nothing wrong — and not a success, because
			// there is nothing to come back.
			return openapi.DecideAdminRequest409JSONResponse{
				Error: "the item has been permanently deleted and cannot be restored",
			}, nil
		}
		return nil, fmt.Errorf("requests: decide: %w", err)
	}
	return openapi.DecideAdminRequest200JSONResponse(rowToAPI(row)), nil
}

// errNotOwner is the single failure value of ownerMayDecide. The caller
// renders one 403 for every reason it can fail, so the distinctions are
// deliberately not surfaced.
var errNotOwner = errors.New("requests: caller is not the owner of the requested asset")

// ownerMayDecide answers the owner disjunct of the decide gate: may
// this NON-approver decide this request?
//
// Yes only when both hold:
//
//  1. The request names CapAccessRequest. See the gate comment — the
//     capability is requester-controlled, so an owner's authority is
//     scoped to the one code that grants nothing.
//  2. The caller owns the asset the request targets, per
//     shares.ObjectOwnerRef — the single expression of object ownership
//     (#893/#665), which fails closed on a missing row, a NULL owner, or
//     a kind with no owner column.
//
// Every failure is the same error, and the caller renders 403 rather
// than 404 for all of them. A caller who is not an approver must not be
// able to tell a request id that exists from one that does not, or which
// capability an existing one names; a 404 here would be an oracle for
// both.
func (h *HTTPHandler) ownerMayDecide(ctx context.Context, callerRef int64, requestID uuid.UUID) error {
	row, err := h.domain.Get(ctx, requestID)
	if err != nil {
		return errNotOwner
	}
	return h.ownerMayDecideRow(ctx, callerRef, row)
}

// ownerMayDecideRow is the same rule against a row the caller already
// holds. Split out for the listing path, which has every row in hand
// and would otherwise re-read each one by id — a per-row round trip to
// answer a question the row already contains.
//
// Both the gate and the listing go through THIS function, so the two
// cannot drift into disagreeing about who owns what.
func (h *HTTPHandler) ownerMayDecideRow(ctx context.Context, callerRef int64, row ResourceRequest) error {
	if row.RequestedCapability != CapAccessRequest {
		return errNotOwner
	}
	ownerRef, ok, err := h.domain.TargetOwnerRef(ctx, TargetKind(row.TargetKind), uuid.UUID(row.TargetID.Bytes))
	if err != nil || !ok || ownerRef != callerRef {
		return errNotOwner
	}
	return nil
}

// decidableByCaller answers, for one queue row, the same question the
// decide gate above will answer — so a listing never renders a control
// that 403s.
//
// It exists because the two queues have different reach. /admin/requests
// is readable by requests.read and share.grant holders, and since #931
// it contains rows NEITHER of them may decide. Row presence therefore
// stopped being a usable proxy for "you can act on this", and the
// alternative to computing it here is a client that guesses — the exact
// failure the trash listing's `restorable_by_caller` was introduced to
// avoid (#937).
//
// Kept structurally parallel to DecideAdminRequest's gate: appeal rows
// take the CanRestoreDeleted branch and nothing else, access rows take
// the capability-or-owner branch. A divergence between the two is a bug
// in this function, not a policy.
//
// The extra read only happens for appeal rows, which are rare — an
// access-request row is answered from the identity alone.
func (h *HTTPHandler) decidableByCaller(ctx context.Context, id *auth.Identity, row ResourceRequest) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	if row.RequestedCapability == CapRestoreRequest {
		st, err := h.domain.TargetDeleteState(ctx, TargetKind(row.TargetKind), uuid.UUID(row.TargetID.Bytes))
		if err != nil {
			// Fail closed. A flag that guessed "yes" on a failed read
			// would offer the button this function exists to withhold.
			return false
		}
		return auth.CanRestoreDeleted(id, st.DeletedBy)
	}
	if id.Can(CapShareGrant) || id.Can(CapSystemAdmin) {
		return true
	}
	return h.ownerMayDecide(ctx, id.UserRef, uuid.UUID(row.ID.Bytes)) == nil
}

// withDecidable renders a queue page, annotating each row with the
// caller's own answer. Used by both queue endpoints so the two cannot
// drift.
func (h *HTTPHandler) withDecidable(ctx context.Context, id *auth.Identity, rows []ResourceRequest) []openapi.ResourceRequest {
	items := make([]openapi.ResourceRequest, 0, len(rows))
	for _, r := range rows {
		item := rowToAPI(r)
		item.DecidableByCaller = ptr(h.decidableByCaller(ctx, id, r))
		items = append(items, item)
	}
	return items
}

// ---------------------------------------------------------------------------
// row → API
// ---------------------------------------------------------------------------

func rowToAPI(r ResourceRequest) openapi.ResourceRequest {
	out := openapi.ResourceRequest{
		Id:                  uuid.UUID(r.ID.Bytes),
		RequesterUserRef:    r.RequesterUserRef,
		TargetKind:          openapi.ResourceRequestTargetKind(r.TargetKind),
		TargetId:            uuid.UUID(r.TargetID.Bytes),
		RequestedCapability: r.RequestedCapability,
		State:               openapi.ResourceRequestState(r.State),
		RequestedAt:         r.RequestedAt.Time,
	}
	if r.Reason != "" {
		out.Reason = &r.Reason
	}
	if r.DecisionReason != "" {
		out.DecisionReason = &r.DecisionReason
	}
	if r.DecidedAt.Valid {
		t := r.DecidedAt.Time
		out.DecidedAt = &t
	}
	if r.DecidedByUserRef != nil {
		out.DecidedByUserRef = r.DecidedByUserRef
	}
	if r.ExpiresAt.Valid {
		t := r.ExpiresAt.Time
		out.ExpiresAt = &t
	}
	return out
}

// silence unused imports on builds where the cascade test is the
// only file pulling them in.
var (
	_ = pgx.ErrNoRows
	_ = (*pgxpool.Pool)(nil)
)
