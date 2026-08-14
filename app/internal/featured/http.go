// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// GitHub #341 — HTTP surface for the featured-content curation list.
//
// Four endpoints:
//
//   GET    /admin/featured        — list in display order  (featured.read)
//   POST   /admin/featured        — add an asset/collection (system.admin)
//   DELETE /admin/featured/{id}   — remove one entry        (system.admin)
//   PUT    /admin/featured/order  — reorder the whole list  (system.admin)
//
// The read takes a dedicated cap so a read-only auditor role can view
// the curation list (#356); curation itself stays system.admin. Kept
// separate from Handler so non-HTTP consumers don't drag the openapi
// import.

package featured

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
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
	if !id.Can(CapFeaturedRead) && !id.Can(CapSystemAdmin) {
		return openapi.ListFeaturedItems403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapFeaturedRead + " capability required"},
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
	// ADDING A SUBJECT KIND TOUCHES SIX PLACES, not two. #1084 was
	// scoped as four and the last two were found by adding a `team` and
	// looking at the result:
	//
	//   1. featured_items_subject_kind_check   (migration 00048)
	//   2. this check
	//   3. this check's error string
	//   4. FeaturedItemInput.subject_kind enum — the REQUEST schema
	//   5. FeaturedItem.subject_kind enum — the RESPONSE schema, a
	//      separate object. Miss it and the server keeps serialising the
	//      new kind while the generated client's type narrows it away.
	//   6. ListFeaturedItems' title resolution + the /admin/content/
	//      featured page, both of which switch on the kind. Miss those
	//      and the operator's own curation list shows the new kind as an
	//      untitled Collection with a dead link — on the very page they
	//      would use to remove it.
	//
	// This check is not redundant with the database's. Without it an
	// unknown kind reaches Postgres, raises 23514 and surfaces as a 500 —
	// the database covering for the handler rather than the two agreeing.
	// The constraint is the backstop; this is the contract.
	kind := string(req.Body.SubjectKind)
	if kind != "asset" && kind != "collection" && kind != "team" {
		return openapi.AddFeaturedItem400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "subject_kind must be asset, collection or team",
			},
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
	// Thumbnail hints are populated for BOTH subject kinds (#625). This
	// used to be gated behind `if r.SubjectKind == "asset"`, which meant
	// the query below could resolve a collection cover perfectly and the
	// mapper would throw it away — the admin list rendered a "C"
	// placeholder for every collection while the public rail showed real
	// covers (#559). The query yields NULL/'' hints when nothing is
	// servable, so passing them through unconditionally adds no claim.
	if r.CoverAssetID.Valid {
		id := uuid.UUID(r.CoverAssetID.Bytes)
		out.CoverAssetId = &id
	}
	if r.AssetFileHash != "" {
		h := r.AssetFileHash
		out.AssetFileHash = &h
	}
	out.PreviewAvailable = r.AssetPreviewAvailable
	out.LadderAvailable = r.AssetLadderAvailable
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

// ---------------------------------------------------------------------------
// GET /featured — the PUBLIC rail (#417, ADR 0065)
// ---------------------------------------------------------------------------

// GetPublicFeaturedRail serves the anonymous-readable rail.
//
// No capability check and no identity requirement, deliberately: this
// is the public surface, and what a caller may see is decided by the
// visibility predicate inside ListPublicRail rather than by a gate
// here. Adding a second check at this layer would be the fourth
// expression of a rule that already has one home (ADR 0063) — the
// defect class that cost #210, #212, #432 and #449.
//
// The install-wide public-mode toggle still applies: an anonymous
// request to this route is rejected by auth/middleware.go before it
// arrives when the operator has not opened the install (#445).
func (h *HTTPHandler) GetPublicFeaturedRail(
	ctx context.Context,
	req openapi.GetPublicFeaturedRailRequestObject,
) (openapi.GetPublicFeaturedRailResponseObject, error) {
	limit := int32(24)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}

	// Anonymous callers resolve to the anonymous predicate; there is no
	// nil-identity special case beyond building the caller.
	caller := visibility.NewCaller(nil)
	if id := auth.IdentityFromContext(ctx); id != nil {
		caller = visibility.NewCaller(&id.UserRef)
	}

	rows, err := ListPublicRail(ctx, h.domain.Pool, caller, limit, h.domain.Ladder(ctx))
	if err != nil {
		return nil, fmt.Errorf("featured: public rail: %w", err)
	}
	items := make([]openapi.FeaturedItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, railRowToAPI(r))
	}
	return openapi.GetPublicFeaturedRail200JSONResponse{Items: items}, nil
}

// railRowToAPI mirrors listRowToAPI. The thumbnail hints are already
// suppressed in SQL for non-public assets (ADR 0020), so this copies
// what it is given rather than re-deciding.
//
// Copied for BOTH subject kinds (#559). This used to be gated on
// `== "asset"`, which discarded a collection's cover even once the
// query produced one — the hints are populated per subject kind in SQL,
// and re-deciding here is exactly the duplicated rule the comment above
// warns against. A collection with no eligible cover arrives with a nil
// hash and false flags, so the gate bought nothing.
func railRowToAPI(r RailRow) openapi.FeaturedItem {
	out := openapi.FeaturedItem{
		Id:          uuid.UUID(r.ID.Bytes),
		SubjectKind: openapi.FeaturedItemSubjectKind(r.SubjectKind),
		SubjectId:   uuid.UUID(r.SubjectID.Bytes),
		Position:    int(r.Position),
		Title:       r.Title,
	}
	if r.CoverAssetID.Valid {
		id := uuid.UUID(r.CoverAssetID.Bytes)
		out.CoverAssetId = &id
	}
	out.AssetFileHash = r.AssetFileHash
	out.PreviewAvailable = r.AssetPreviewAvailable
	out.LadderAvailable = r.AssetLadderAvailable
	return out
}
