// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.E HTTP surface for the resource-request workflow.
//
// Four endpoints:
//
//   POST /assets/{id}/request-access  — requester submits
//   GET  /account/requests            — requester's own
//   GET  /admin/requests              — approver-facing pending list
//   POST /admin/requests/{id}/decide  — approver decision
//
// The approver gate is Identity.Can("share.grant", InTeam(teamID))
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
	row, created, err := h.domain.Submit(ctx, auth.RequestFromContext(ctx), SubmitInput{
		RequesterUserRef:    id.UserRef,
		TargetAssetID:       uuid.UUID(req.Id),
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
	items := make([]openapi.ResourceRequest, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToAPI(r))
	}
	resp := openapi.ListIncomingRequests200JSONResponse{}
	resp.Items = items
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
	items := make([]openapi.ResourceRequest, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToAPI(r))
	}
	resp := openapi.ListAdminRequests200JSONResponse{}
	resp.Items = items
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
	if !id.Can(CapShareGrant) && !id.Can(CapSystemAdmin) {
		if err := h.ownerMayDecide(ctx, id.UserRef, uuid.UUID(req.Id)); err != nil {
			return openapi.DecideAdminRequest403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
					Error: CapShareGrant + " capability or ownership of the requested asset required",
				},
			}, nil
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
	if row.RequestedCapability != CapAccessRequest {
		return errNotOwner
	}
	ownerRef, ok, err := h.domain.AssetOwnerRef(ctx, uuid.UUID(row.TargetAssetID.Bytes))
	if err != nil || !ok || ownerRef != callerRef {
		return errNotOwner
	}
	return nil
}

// ---------------------------------------------------------------------------
// row → API
// ---------------------------------------------------------------------------

func rowToAPI(r ResourceRequest) openapi.ResourceRequest {
	out := openapi.ResourceRequest{
		Id:                  uuid.UUID(r.ID.Bytes),
		RequesterUserRef:    r.RequesterUserRef,
		TargetAssetId:       uuid.UUID(r.TargetAssetID.Bytes),
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
