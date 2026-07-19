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
	if req.Body == nil || req.Body.Capability == "" {
		return openapi.RequestAssetAccess400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "capability is required"},
		}, nil
	}
	reason := ""
	if req.Body.Reason != nil {
		reason = *req.Body.Reason
	}
	row, err := h.domain.Submit(ctx, auth.RequestFromContext(ctx), SubmitInput{
		RequesterUserRef:    id.UserRef,
		TargetAssetID:       uuid.UUID(req.Id),
		RequestedCapability: req.Body.Capability,
		Reason:              reason,
	})
	if err != nil {
		if errors.Is(err, ErrUnknownCapability) {
			return openapi.RequestAssetAccess400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "unknown capability: " + req.Body.Capability,
				},
			}, nil
		}
		return nil, fmt.Errorf("requests: submit: %w", err)
	}
	return openapi.RequestAssetAccess201JSONResponse(rowToAPI(row)), nil
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
	// Coarse approver gate first; per-asset-team scoping below.
	if !id.Can(CapShareGrant) && !id.Can("system.admin") {
		return openapi.DecideAdminRequest403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapShareGrant + " capability required"},
		}, nil
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
