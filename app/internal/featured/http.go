// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// GitHub #341 — HTTP surface for the featured-content curation list.
//
// Four endpoints, all system.admin-gated:
//
//   GET    /admin/featured        — list in display order
//   POST   /admin/featured        — add an asset/collection
//   DELETE /admin/featured/{id}   — remove one entry
//   PUT    /admin/featured/order  — reorder the whole list
//
// The gate is coarse (system.admin), matching the rest of the content
// admin section. Kept separate from Handler so non-HTTP consumers
// don't drag the openapi import.

package featured

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// HTTPHandler adapts featured.Handler to the openapi strict-server
// contract.
type HTTPHandler struct {
	domain *Handler
	logger *slog.Logger
}

// NewHTTPHandler builds the adapter.
func NewHTTPHandler(h *Handler, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{domain: h, logger: logger}
}

// ---------------------------------------------------------------------------
// GET /admin/featured
// ---------------------------------------------------------------------------

func (h *HTTPHandler) ListFeaturedItems(
	ctx context.Context,
	_ openapi.ListFeaturedItemsRequestObject,
) (openapi.ListFeaturedItemsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListFeaturedItems401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapSystemAdmin) {
		return openapi.ListFeaturedItems403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapSystemAdmin + " capability required"},
		}, nil
	}
	rows, err := h.domain.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("featured: list: %w", err)
	}
	items := make([]openapi.FeaturedItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, listRowToAPI(r))
	}
	return openapi.ListFeaturedItems200JSONResponse{Items: items}, nil
}

// ---------------------------------------------------------------------------
// POST /admin/featured
// ---------------------------------------------------------------------------

func (h *HTTPHandler) AddFeaturedItem(
	ctx context.Context,
	req openapi.AddFeaturedItemRequestObject,
) (openapi.AddFeaturedItemResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.AddFeaturedItem401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapSystemAdmin) {
		return openapi.AddFeaturedItem403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapSystemAdmin + " capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddFeaturedItem400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	kind := string(req.Body.SubjectKind)
	if kind != "asset" && kind != "collection" {
		return openapi.AddFeaturedItem400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "subject_kind must be asset or collection"},
		}, nil
	}
	in := AddInput{
		SubjectKind: kind,
		SubjectID:   uuid.UUID(req.Body.SubjectId),
		CreatedBy:   &id.UserRef,
	}
	if req.Body.Position != nil {
		p := int32(*req.Body.Position)
		in.Position = &p
	}
	row, err := h.domain.Add(ctx, in)
	if err != nil {
		if errors.Is(err, ErrAlreadyFeatured) {
			return openapi.AddFeaturedItem409JSONResponse{Error: "subject already featured"}, nil
		}
		return nil, fmt.Errorf("featured: add: %w", err)
	}
	return openapi.AddFeaturedItem201JSONResponse(insertRowToAPI(row)), nil
}

// ---------------------------------------------------------------------------
// PUT /admin/featured/order
// ---------------------------------------------------------------------------

func (h *HTTPHandler) ReorderFeaturedItems(
	ctx context.Context,
	req openapi.ReorderFeaturedItemsRequestObject,
) (openapi.ReorderFeaturedItemsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ReorderFeaturedItems401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapSystemAdmin) {
		return openapi.ReorderFeaturedItems403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapSystemAdmin + " capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.ReorderFeaturedItems400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	ids := make([]uuid.UUID, 0, len(req.Body.Ids))
	for _, x := range req.Body.Ids {
		ids = append(ids, uuid.UUID(x))
	}
	if err := h.domain.Reorder(ctx, ids); err != nil {
		return nil, fmt.Errorf("featured: reorder: %w", err)
	}
	return openapi.ReorderFeaturedItems204Response{}, nil
}

// ---------------------------------------------------------------------------
// DELETE /admin/featured/{id}
// ---------------------------------------------------------------------------

func (h *HTTPHandler) RemoveFeaturedItem(
	ctx context.Context,
	req openapi.RemoveFeaturedItemRequestObject,
) (openapi.RemoveFeaturedItemResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RemoveFeaturedItem401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapSystemAdmin) {
		return openapi.RemoveFeaturedItem403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapSystemAdmin + " capability required"},
		}, nil
	}
	if err := h.domain.Remove(ctx, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return openapi.RemoveFeaturedItem404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "featured item not found"},
			}, nil
		}
		return nil, fmt.Errorf("featured: remove: %w", err)
	}
	return openapi.RemoveFeaturedItem204Response{}, nil
}

// ---------------------------------------------------------------------------
// row → API
// ---------------------------------------------------------------------------

func listRowToAPI(r ListFeaturedItemsRow) openapi.FeaturedItem {
	out := openapi.FeaturedItem{
		Id:          uuid.UUID(r.ID.Bytes),
		SubjectKind: openapi.FeaturedItemSubjectKind(r.SubjectKind),
		SubjectId:   uuid.UUID(r.SubjectID.Bytes),
		Position:    int(r.Position),
		Title:       r.Title,
		CreatedAt:   r.CreatedAt.Time,
	}
	if r.SubjectKind == "asset" {
		out.AssetFileHash = r.AssetFileHash
		hasImg := r.AssetHasImage
		out.AssetHasImage = &hasImg
	}
	return out
}

// insertRowToAPI maps the RETURNING row from an add. The base row
// carries no resolved title/thumb (the frontend re-fetches the list),
// so those stay zero.
func insertRowToAPI(r FeaturedItem) openapi.FeaturedItem {
	return openapi.FeaturedItem{
		Id:          uuid.UUID(r.ID.Bytes),
		SubjectKind: openapi.FeaturedItemSubjectKind(r.SubjectKind),
		SubjectId:   uuid.UUID(r.SubjectID.Bytes),
		Position:    int(r.Position),
		CreatedAt:   r.CreatedAt.Time,
	}
}
