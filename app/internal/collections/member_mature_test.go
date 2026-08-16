// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1147 — the collection MEMBER GRID shipped a mature asset's thumbhash.
//
// # Why this is a picture and not a listing
//
// `GET /collections/{id}/resources` selects `a.thumbhash`,
// `has_col_variant` and `has_full_ladder` per row. A thumbhash IS a blur
// — it is the picture in derived form, which is the whole argument #1066
// made when it withheld one — and the col-variant flag is what tells the
// client to go and fetch the full rendition. So this is a derived-picture
// surface reached through a different door from the mosaic, and it was
// the same defect: `assets.ListAssetsPageGated` filters mature
// unconditionally on the row plane, and this listing over the same
// assets did not.
//
// # ABSENT, not placeheld — and that is deliberately unlike its
// neighbours
//
// Every other withholding in this query redacts and KEEPS the row: a
// restricted member stays listed because ADR 0064 requires browse to
// show the corpus and #881's request-access flow hangs off the
// placeholder. The mature axis has no such flow — there is nothing to
// request, only a preference to change — and #921 measured what the
// placeholder alternative looks like: a grid of blurred plates nobody
// asked to be offered. `assets.ListAssetsPageGated` states the argument
// in full; this is the same list of assets, so it gets the same answer.
//
// That asymmetry is exactly what the assertion below has to pin, which
// is why it checks the row is GONE rather than that its thumbhash is
// empty. A composer that placeheld a mature member would satisfy any
// "no thumbhash" assertion and still tell a reader who opted out that
// there is adult content here and how much of it.
//
// Skips without AA_DB_PASSWORD.

package collections_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// cmReader is NOT mcfOwner, and that is the fixture's load-bearing
// detail: the mature rule exempts the OWNER (ADR 0090 §2), so a test
// that read as the asset owner would assert nothing at all.
const cmReader int64 = 11470002

// cmMembers drives the REAL endpoint as `ref`, with a resolved mature
// viewer on the context — the one piece of middleware the handler tests
// do not otherwise run, and the input this whole file is about.
func cmMembers(
	t *testing.T,
	h *collections.Handler,
	ref int64,
	colID uuid.UUID,
	v visibility.MatureViewer,
) []openapi.CollectionResource {
	t.Helper()
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: ref, AuthMethod: "session"})
	ctx = visibility.WithMatureViewer(ctx, v)
	resp, err := h.ListCollectionResources(ctx, openapi.ListCollectionResourcesRequestObject{
		Id: openapi_types.UUID(colID),
	})
	if err != nil {
		t.Fatalf("ListCollectionResources: %v", err)
	}
	ok, is200 := resp.(openapi.ListCollectionResources200JSONResponse)
	if !is200 {
		t.Fatalf("ListCollectionResources: got %T, want a 200", resp)
	}
	return ok.Items
}

func cmHas(items []openapi.CollectionResource, id uuid.UUID) *openapi.CollectionResource {
	for i := range items {
		if uuid.UUID(items[i].AssetId) == id {
			return &items[i]
		}
	}
	return nil
}

func cmMarkMature(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = TRUE WHERE id = $1`, id); err != nil {
		t.Fatalf("mark mature: %v", err)
	}
}

// TestCollectionMembers_MatureMemberIsAbsentNotPlaceheld is the pair.
//
// Both members are PUBLIC, so no sensitivity conjunct separates them,
// and both are owned by mcfOwner's stranger so the owner exemption does
// not either. The only difference between the two legs is the reader's
// opt-in.
func TestCollectionMembers_MatureMemberIsAbsentNotPlaceheld(t *testing.T) {
	pool := mcfPool(t)
	h := collections.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	plain := mcfAsset(t, pool, "cm plain", "public")
	adult := mcfAsset(t, pool, "cm adult", "public")
	cmMarkMature(t, pool, adult)
	col := mcfCollection(t, pool, plain, adult)

	out := cmMembers(t, h, cmReader, col, visibility.MatureViewer{})
	if row := cmHas(out, adult); row != nil {
		t.Errorf("the mature member is still on the grid for a viewer who never opted "+
			"in (thumbhash=%v). It is ABSENT on this axis, not redacted: there is no "+
			"request-access flow to hang a placeholder off, and a placeholder would "+
			"itself tell the reader how much adult content is here",
			row.Thumbhash != nil)
	}
	if cmHas(out, plain) == nil {
		t.Error("the non-mature member vanished too — the conjunct is filtering the " +
			"whole grid rather than the mature rows")
	}

	in := cmMembers(t, h, cmReader,
		col, visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true})
	if cmHas(in, adult) == nil || cmHas(in, plain) == nil {
		t.Errorf("qualified grid has %d members, want both — the withholding above has "+
			"to be the mature axis and not an outage", len(in))
	}
}
