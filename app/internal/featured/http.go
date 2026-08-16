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
	"github.com/mscrnt/artist-alley/app/internal/users"
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
	// nil = the RAIL's own placements. Band cards are a different
	// surface and are listed by GetAdminPromoBand (#1118); mixing them
	// in here would offer an operator a "remove" button on a row that is
	// not on the list they are looking at.
	rows, err := h.domain.List(ctx, nil)
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
	// Audience (#1104). An omitted scope stays org — the OpenAPI
	// `default` is documentation for the client, not a server-side
	// fill, so the empty string arriving here means "not supplied" and
	// Handler.Add resolves it. Both admissible values are gated on
	// system.admin, already checked above; see AddInput.Scope for why
	// there is no narrower gate to reach for and what should happen if
	// one is ever added.
	if req.Body.Scope != nil {
		in.Scope = string(*req.Body.Scope)
	}
	row, err := h.domain.Add(ctx, in)
	if err != nil {
		if errors.Is(err, ErrAlreadyFeatured) {
			return openapi.AddFeaturedItem409JSONResponse{Error: "subject already featured"}, nil
		}
		if errors.Is(err, ErrScopeNotWritable) {
			return openapi.AddFeaturedItem400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "scope must be org or public",
				},
			}, nil
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
	//
	// postCaps rides along for the subtitle's member count (#1110) and
	// is resolved from the SAME identity, at the same moment, rather
	// than inside the query builder: visibility.PostCaps is a resolved
	// two-state value precisely so a closure over the identity cannot
	// outlive the request that produced it (see ADR 0063's note on
	// ContentCaps). Anonymous leaves it at its zero value, which admits
	// nothing.
	q := h.placementQuery(ctx, limit)
	rows, err := ListPlacements(ctx, h.domain.Pool, q)
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
		Subtitle:    r.Subtitle,
	}
	// nil stays nil: an asset subject has no membership, and emitting 0
	// there would have the card print "0 items" under an asset's name.
	if r.ItemCount != nil {
		n := int(*r.ItemCount)
		out.ItemCount = &n
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

// ---------------------------------------------------------------------------
// The operator promo band (#1118)
// ---------------------------------------------------------------------------

// placementQuery resolves everything [ListPlacements] needs from the
// request, ONCE.
//
// Every input is read at the HTTP edge and carried, never re-derived
// inside the query builder: the caller, the post capabilities the member
// count needs, and the mature axis. That is the same discipline
// visibility.MatureViewer's own doc states and the reason
// visibility.PostCaps is a resolved two-state value — a closure over the
// identity must not outlive the request that produced it.
//
// An anonymous caller leaves PostCaps and Mature at their zero values,
// both of which admit nothing. A handler that loses its inputs must
// refuse rather than widen.
func (h *HTTPHandler) placementQuery(ctx context.Context, limit int32) PlacementQuery {
	q := PlacementQuery{
		Caller: visibility.NewCaller(nil),
		// ⚠️ visibility.MatureFromContext returns the DISQUALIFIED viewer
		// when the middleware never ran, not an error. See its doc: the
		// visible failure (an opted-in reader still cannot see it) is the
		// one deliberately chosen over the invisible one.
		Mature: visibility.MatureFromContext(ctx),
		Limit:  limit,
		Ladder: h.domain.Ladder(ctx),
	}
	if id := auth.IdentityFromContext(ctx); id != nil {
		q.Caller = visibility.NewCaller(&id.UserRef)
		q.PostCaps = visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) })
		// ADR 0090 checks the admin exemption BEFORE qualification, so
		// it survives the instance switch being off — which is the case
		// that matters, because an admin has to be able to moderate what
		// the switch hid.
		q.MatureAdmin = id.Can(CapSystemAdmin)
	}
	return q
}

// GetPromoBand serves the browse feed's promo band, or nothing.
//
// No capability check and no identity requirement, for the same reason
// GetPublicFeaturedRail has none: what a caller may see is decided by
// the predicate inside the query rather than by a gate here, and the
// install-wide public-mode toggle rejects an anonymous request before it
// arrives when the operator has not opened the install (#445). That
// toggle already covers this route — auth.PublicSurfaceRoutes names
// "/featured" as a PREFIX — so the band and the rail cannot end up on
// opposite sides of it.
//
// ⚠️ THE EMPTY ANSWER IS AN ABSENT BAND, not an empty one. See
// RenderableBand: the collapse decision lives beside the filter that
// produced the emptiness, so no client can render a headline over a row
// of nothing by forgetting a length check.
func (h *HTTPHandler) GetPromoBand(
	ctx context.Context,
	_ openapi.GetPromoBandRequestObject,
) (openapi.GetPromoBandResponseObject, error) {
	// The band is a strip of a handful of cards, not a scrollable rail;
	// the cap exists so a mis-curated band cannot become a page of its
	// own, and is generous enough that it is never the reason a card is
	// missing.
	rendered, err := RenderableBand(ctx, h.domain.Pool, h.placementQuery(ctx, promoBandCardLimit))
	if err != nil {
		return nil, fmt.Errorf("featured: promo band: %w", err)
	}
	if rendered == nil {
		return openapi.GetPromoBand200JSONResponse{}, nil
	}
	authors, err := h.resolveAuthors(ctx, rendered.Items)
	if err != nil {
		return nil, err
	}
	band := bandToAPI(rendered.Band)
	band.Items = make([]openapi.FeaturedItem, 0, len(rendered.Items))
	for _, r := range rendered.Items {
		item := railRowToAPI(r)
		// An omission from the map is a withheld or absent identity —
		// see users.LookupAuthors. The card renders with no author chip
		// rather than with a placeholder, exactly as a post card does.
		if r.OwnerUserRef != nil {
			if a, ok := authors[*r.OwnerUserRef]; ok {
				item.Author = &a
			}
		}
		band.Items = append(band.Items, item)
	}
	return openapi.GetPromoBand200JSONResponse{Band: &band}, nil
}

// promoBandCardLimit caps the cards one band may render.
//
// The reference design shows five. The cap is higher so that a band with
// six is not silently truncated by a number nobody chose, and low enough
// that a band cannot become an infinite scroll of its own.
const promoBandCardLimit = int32(24)

// resolveAuthors turns the band's owner refs into renderable identities,
// in ONE query.
//
// ⛔ The name inside comes from users.ResolveDisplayName, through
// LookupAuthors. Do NOT re-derive it here from username/fullname/
// display_name: that ladder has an authenticated rung and an anonymous
// arm, and #1023 exists because it had been transcribed four times with
// three of the copies wrong. This function's whole content is "hand the
// refs to the one home and index the result".
func (h *HTTPHandler) resolveAuthors(
	ctx context.Context,
	rows []RailRow,
) (map[int64]openapi.PostAuthor, error) {
	refs := make([]int64, 0, len(rows))
	seen := make(map[int64]struct{}, len(rows))
	for _, r := range rows {
		if r.OwnerUserRef == nil {
			continue
		}
		if _, ok := seen[*r.OwnerUserRef]; ok {
			continue
		}
		seen[*r.OwnerUserRef] = struct{}{}
		refs = append(refs, *r.OwnerUserRef)
	}
	anonymous := auth.IdentityFromContext(ctx) == nil
	authors, err := users.LookupAuthors(ctx, h.domain.Pool, refs, anonymous)
	if err != nil {
		return nil, fmt.Errorf("featured: band authors: %w", err)
	}
	return authors, nil
}

// GetAdminPromoBand serves the band an operator edits — no audience
// gate, no collapse, disabled bands included. See the operation
// description for why those three differences from the public read are
// the point rather than an oversight.
func (h *HTTPHandler) GetAdminPromoBand(
	ctx context.Context,
	_ openapi.GetAdminPromoBandRequestObject,
) (openapi.GetAdminPromoBandResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetAdminPromoBand401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapFeaturedRead) && !id.Can(CapSystemAdmin) {
		return openapi.GetAdminPromoBand403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapFeaturedRead + " capability required"},
		}, nil
	}
	band, items, err := h.adminBand(ctx)
	if errors.Is(err, ErrNoBand) {
		return openapi.GetAdminPromoBand404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "no promo band configured"},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	band.Items = items
	return openapi.GetAdminPromoBand200JSONResponse(band), nil
}

// adminBand reads the band plus its cards for an operator surface.
func (h *HTTPHandler) adminBand(ctx context.Context) (openapi.PromoBand, []openapi.FeaturedItem, error) {
	row, err := h.domain.Band(ctx)
	if err != nil {
		return openapi.PromoBand{}, nil, err
	}
	bandID := uuid.UUID(row.ID.Bytes)
	cards, err := h.domain.List(ctx, &bandID)
	if err != nil {
		return openapi.PromoBand{}, nil, fmt.Errorf("featured: band cards: %w", err)
	}
	items := make([]openapi.FeaturedItem, 0, len(cards))
	for _, c := range cards {
		items = append(items, listRowToAPI(c))
	}
	return bandToAPI(row), items, nil
}

// SavePromoBand upserts the band definition.
func (h *HTTPHandler) SavePromoBand(
	ctx context.Context,
	req openapi.SavePromoBandRequestObject,
) (openapi.SavePromoBandResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SavePromoBand401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapSystemAdmin) {
		return openapi.SavePromoBand403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapSystemAdmin + " capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SavePromoBand400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	in := BandInput{Title: req.Body.Title, AfterPage: 1, CreatedBy: &id.UserRef}
	if req.Body.Blurb != nil {
		in.Blurb = *req.Body.Blurb
	}
	if req.Body.CtaLabel != nil {
		in.CTALabel = *req.Body.CtaLabel
	}
	if req.Body.CtaUrl != nil {
		in.CTAURL = *req.Body.CtaUrl
	}
	if req.Body.Enabled != nil {
		in.Enabled = *req.Body.Enabled
	}
	// The OpenAPI `default` is documentation for the client, not a
	// server-side fill — same reading AddFeaturedItem takes of its own
	// scope default — so the zero here is the field's real default and
	// not a value the client sent.
	if req.Body.AfterPage != nil {
		in.AfterPage = int32(*req.Body.AfterPage)
	}
	if req.Body.Scope != nil {
		in.Scope = string(*req.Body.Scope)
	}
	if _, err := h.domain.SaveBand(ctx, in); err != nil {
		if errors.Is(err, ErrBadCTA) || errors.Is(err, ErrBadAfterPage) || errors.Is(err, ErrBandScopeNotWritable) {
			return openapi.SavePromoBand400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		}
		return nil, err
	}
	// Re-read rather than mapping SaveBand's RETURNING row: the response
	// carries the band WITH ITS CARDS, so the admin page repaints from
	// one round trip and cannot repaint from a stale copy of them.
	band, items, err := h.adminBand(ctx)
	if err != nil {
		return nil, err
	}
	band.Items = items
	return openapi.SavePromoBand200JSONResponse(band), nil
}

// DeletePromoBand removes the band and, by cascade, its cards.
func (h *HTTPHandler) DeletePromoBand(
	ctx context.Context,
	_ openapi.DeletePromoBandRequestObject,
) (openapi.DeletePromoBandResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DeletePromoBand401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapSystemAdmin) {
		return openapi.DeletePromoBand403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapSystemAdmin + " capability required"},
		}, nil
	}
	band, err := h.domain.Band(ctx)
	if errors.Is(err, ErrNoBand) {
		return openapi.DeletePromoBand404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "no promo band configured"},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := h.domain.RemoveBand(ctx, uuid.UUID(band.ID.Bytes)); err != nil {
		if errors.Is(err, ErrNoBand) {
			return openapi.DeletePromoBand404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "no promo band configured"},
			}, nil
		}
		return nil, err
	}
	return openapi.DeletePromoBand204Response{}, nil
}

// AddPromoBandItem appends one card to the band.
//
// Removal and reordering deliberately have no band-specific twin:
// DELETE /admin/featured/{id} takes any placement id and
// PUT /admin/featured/order assigns positions by id, both of which are
// already correct for a band card. Minting copies of them would be two
// more places for the next change to miss.
func (h *HTTPHandler) AddPromoBandItem(
	ctx context.Context,
	req openapi.AddPromoBandItemRequestObject,
) (openapi.AddPromoBandItemResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.AddPromoBandItem401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapSystemAdmin) {
		return openapi.AddPromoBandItem403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapSystemAdmin + " capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.AddPromoBandItem400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	// Two kinds, not three. A band renders a COVER, and the only
	// admissible route to a team's picture is the render-time hero
	// re-check (#982) that the teams rail owns — see PromoBandItemInput.
	// This check is not redundant with the schema enum: the enum stops a
	// generated client sending it, and this stops everything else.
	kind := string(req.Body.SubjectKind)
	if kind != "asset" && kind != "collection" {
		return openapi.AddPromoBandItem400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "subject_kind must be asset or collection",
			},
		}, nil
	}
	band, err := h.domain.Band(ctx)
	if errors.Is(err, ErrNoBand) {
		return openapi.AddPromoBandItem404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "no promo band configured"},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	bandID := uuid.UUID(band.ID.Bytes)
	row, err := h.domain.Add(ctx, AddInput{
		SubjectKind: kind,
		SubjectID:   uuid.UUID(req.Body.SubjectId),
		CreatedBy:   &id.UserRef,
		BandID:      &bandID,
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyFeatured) {
			return openapi.AddPromoBandItem409JSONResponse{Error: "subject already in this band"}, nil
		}
		return nil, fmt.Errorf("featured: band add: %w", err)
	}
	return openapi.AddPromoBandItem201JSONResponse(insertRowToAPI(row)), nil
}

// bandToAPI maps the stored definition. `items` is filled by the caller,
// which is what keeps the two reads — public (filtered, collapsing) and
// admin (unfiltered, complete) — from having to share a resolution they
// deliberately do not share.
func bandToAPI(b PromoBand) openapi.PromoBand {
	out := openapi.PromoBand{
		Id:        uuid.UUID(b.ID.Bytes),
		Title:     b.Title,
		Blurb:     b.Blurb,
		CtaLabel:  b.CtaLabel,
		CtaUrl:    b.CtaUrl,
		Enabled:   b.Enabled,
		AfterPage: int(b.AfterPage),
		Scope:     openapi.PromoBandScope(b.Scope),
		Items:     []openapi.FeaturedItem{},
	}
	if b.CreatedAt.Valid {
		t := b.CreatedAt.Time
		out.CreatedAt = &t
	}
	if b.UpdatedAt.Valid {
		t := b.UpdatedAt.Time
		out.UpdatedAt = &t
	}
	return out
}
