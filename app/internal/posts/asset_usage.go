// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// GET /assets/{id}/posts — an asset knows where it appears (ADR 0091 §5)
// ---------------------------------------------------------------------------
//
// An asset is personal storage until it is posted. Its owner therefore
// has a question nothing in the product could answer: where has my file
// ended up — including in posts written by other people, in a shared
// team library, which the DAM half of this product makes ordinary.
//
// # Why this is not GetPostsByAsset with a different gate
//
// `GET /posts/by-asset/{id}` asks the READER's question: which posts
// featuring this asset may I read. A post the caller may not read is
// ABSENT, which is the correct answer to a reader and the wrong one to
// an owner — it tells them their file is nowhere.
//
// So this operation returns the same readable posts AND a count of the
// rest. The two halves are the disclosure rule made structural:
// everything the caller is entitled to arrives whole, and everything
// they are not is compressed into an integer that carries no handle
// back to what it counted.
//
// # Why it is the most conservative shape the decision allows
//
// ADR 0091's first amendment flags decision 5 as the part of the model
// with NO PRIOR ART: on art platforms a post's files are always the
// author's own, so "my asset is in someone else's post" never arises
// there. Nothing was borrowed, so nothing validates a wider shape.
//
// What that means concretely, and each of these is a deliberate
// omission rather than a thing not got to yet:
//
//   - no ids for the withheld posts. An id is a handle, and a handle is
//     probeable against every other post endpoint.
//   - no titles, authors, tiers or timestamps. The ADR names title and
//     author specifically; the others are the same class of fact.
//   - no cursor over the withheld set, and no filter that moves the
//     count selectively. Either would let a caller binary-search the
//     integer back into the posts behind it — which is exactly how the
//     count-leak defect works on the search side (#902).
//
// Asking twice returns the same integer. That is the whole surface.
//
// # Who may ask
//
// The asset's OWNER, plus assets.admin / system.admin. A stranger gets
// the SAME 404 a nonexistent asset gets — not a 403 — because "this
// asset appears in 3 posts you cannot see" is the owner's information
// about their own file, and answering it for anyone would turn the
// endpoint into a usage oracle over the whole library.

// ListAssetPosts implements GET /assets/{id}/posts.
func (h *Handler) ListAssetPosts(
	ctx context.Context,
	req openapi.ListAssetPostsRequestObject,
) (openapi.ListAssetPostsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() {
		return openapi.ListAssetPosts401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	assetID := uuid.UUID(req.Id)
	pgAsset := pgtype.UUID{Bytes: assetID, Valid: true}

	// The ownership gate. `notFound` is used for BOTH "no such asset"
	// and "not yours", deliberately and in that order, so the endpoint
	// cannot be walked to discover which asset UUIDs exist.
	notFound := openapi.ListAssetPosts404JSONResponse{
		NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
	}
	owner, err := New(h.Pool).GetAssetOwnerRef(ctx, pgAsset)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound, nil
		}
		return nil, fmt.Errorf("posts: asset usage: owner lookup: %w", err)
	}
	isOwner := owner != nil && *owner != 0 && *owner == caller.UserRef
	if !isOwner && !caller.Can(visibility.AssetsAdmin) && !caller.Can(CapSystemAdmin) {
		return notFound, nil
	}

	// The readable half — the SAME call `/posts/by-asset` makes, so the
	// two can never disagree about which posts this caller may read.
	// A second query here would be a second expression of the post read
	// rule, which is the defect epic #665 exists to remove.
	ids, err := h.ListPostsByAssetGated(ctx, caller, assetID,
		h.resolveMature(ctx, caller), caller.Can(CapSystemAdmin))
	if err != nil {
		return nil, err
	}
	items := make([]openapi.Post, 0, len(ids))
	for _, id := range ids {
		full, err := h.fetchFullPost(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // raced deletion between the list and the fetch
			}
			return nil, err
		}
		items = append(items, *full)
	}
	ptrs := make([]*openapi.Post, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	if err := h.enrichForCaller(ctx, ptrs...); err != nil {
		return nil, err
	}

	// The withheld half. Total minus readable, floored at zero.
	//
	// The floor is not defensive padding: the two queries run outside a
	// single snapshot, so a post deleted between them would make the
	// subtraction negative, and a negative "how many places is my file
	// used" is worse than a stale zero. It cannot go the other way into
	// a leak — a race can only ever make the count too SMALL, and a
	// count too small discloses less than the truth.
	//
	// ⚠️ The mature axis is deliberately NOT applied to the total. A
	// reader who opted out of adult content still gets an accurate
	// count of where their own file is used: `mature` answers who has
	// opted in to being SHOWN something, and this shows nothing. Making
	// the number move with a display preference would tell the owner
	// their file was in fewer places than it is (ADR 0090 §1 — the two
	// axes are independent).
	total, err := New(h.Pool).CountLivePostsForAsset(ctx, pgAsset)
	if err != nil {
		return nil, fmt.Errorf("posts: asset usage: count: %w", err)
	}
	withheld := total - int64(len(items))
	if withheld < 0 {
		withheld = 0
	}

	return openapi.ListAssetPosts200JSONResponse(openapi.AssetPostUsage{
		Items:         items,
		WithheldCount: withheld,
	}), nil
}
