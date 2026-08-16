// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #625 — the admin curation list resolves collection covers, and it
// does so with DELIBERATELY weaker gates than the public rail.
//
// The rail's cover lateral (#559) splices the caller's visibility
// predicate and requires ca.sensitivity = 'public', because it serves
// anonymous visitors and featuring must never widen access. The admin
// endpoint is system.admin-gated and serves operators who read every
// tier, so those two caller-scoped gates are dropped there — and the
// central test below pins the DIFFERENCE, not just the admin behaviour,
// because copying the rail verbatim would have silently hidden
// team-tier covers from operators entitled to see them and every
// admin-only assertion would still have passed on public-tier fixtures.

package featured

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// adminRowFor finds one subject's row in the admin curation list.
func adminRowFor(t *testing.T, pool *pgxpool.Pool, subject uuid.UUID) (ListFeaturedItemsRow, bool) {
	t.Helper()
	rows, err := New(pool).ListFeaturedItems(context.Background(), ListFeaturedItemsParams{Ladder: defaultLadder})
	if err != nil {
		t.Fatalf("ListFeaturedItems: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.SubjectID.Bytes) == subject {
			return r, true
		}
	}
	return ListFeaturedItemsRow{}, false
}

// TestAdminList_TeamTierCoverResolvesWhereRailWouldNot is THE test for
// this change: the same seeded collection, asked about twice.
//
// Its only post's cover is a team-tier asset with a servable col. The
// admin list must resolve it — an operator curating the featured list
// needs to see what the tile IS, whatever its tier. The public rail,
// asked as an anonymous caller, must NOT — that is #559's
// never-widens-access invariant, unchanged. If the two answers ever
// converge, one of two bugs has happened: the admin query grew the
// rail's caller-scoped gates back (operators go blind on non-public
// covers), or the rail lost them (featuring leaks tiers to anonymous).
func TestAdminList_TeamTierCoverResolvesWhereRailWouldNot(t *testing.T) {
	pool := railPool(t)

	cover := railStoredAsset(t, pool, "admin-cover team-tier", "team", true)
	coll := railCollection(t, pool, "admin-cover collection", "public")
	railPostInCollection(t, pool, coll, cover, time.Hour)
	place(t, pool, "collection", coll, "public", 9200)

	t.Run("admin list resolves the team-tier cover", func(t *testing.T) {
		row, ok := adminRowFor(t, pool, coll)
		if !ok {
			t.Fatal("collection placement missing from the admin list")
		}
		if !row.CoverAssetID.Valid {
			t.Fatal("admin list did not resolve a cover for a collection whose " +
				"only post has a team-tier cover with a servable col — the " +
				"caller-scoped gates are back (#625)")
		}
		if got := uuid.UUID(row.CoverAssetID.Bytes); got != cover {
			t.Errorf("cover_asset_id = %s, want the post cover %s", got, cover)
		}
		if row.AssetFileHash == "" {
			t.Error("cover resolved but asset_file_hash is empty; the client " +
				"gates thumbUrl on BOTH fields and would render a placeholder")
		}
		if !row.AssetPreviewAvailable {
			t.Error("cover resolved but preview_available is false")
		}
	})

	t.Run("the public rail still refuses it to an anonymous caller", func(t *testing.T) {
		// Guards the guard: if the rail ALSO resolved this cover, the
		// admin assertion above proves nothing about the dropped gates —
		// a verbatim copy of the rail's lateral would pass it too.
		row, ok := railRowFor(t, pool, visibility.NewCaller(nil), coll)
		if !ok {
			t.Skip("collection not visible on the anonymous rail at all; " +
				"the divergence is covered by the admin half above")
		}
		if row.CoverAssetID.Valid {
			t.Error("the ANONYMOUS rail resolved a team-tier cover — #559's " +
				"never-widens-access invariant has regressed; this test is " +
				"about the admin/rail DIFFERENCE and the difference is gone")
		}
	})
}

// TestAdminList_NoEligibleCoverStaysEmpty — the fix must not become
// permissive about servability. A cover without a col variant cannot be
// rendered by anyone, operator or not, and surfacing it would have the
// client build a byte URL that 404s (#471).
func TestAdminList_NoEligibleCoverStaysEmpty(t *testing.T) {
	pool := railPool(t)

	cover := railStoredAsset(t, pool, "admin-cover no-col", "public", false /* no col */)
	coll := railCollection(t, pool, "admin-cover no-col collection", "public")
	railPostInCollection(t, pool, coll, cover, time.Hour)
	place(t, pool, "collection", coll, "public", 9201)

	row, ok := adminRowFor(t, pool, coll)
	if !ok {
		t.Fatal("collection placement missing from the admin list")
	}
	if row.CoverAssetID.Valid {
		t.Error("admin list resolved a cover whose col variant does not exist; " +
			"the thumbnail URL built from it would 404")
	}
	if row.AssetFileHash != "" {
		t.Errorf("asset_file_hash = %q for a collection with no servable cover", row.AssetFileHash)
	}
	if row.AssetPreviewAvailable {
		t.Error("preview_available = true with nothing servable")
	}
}

// TestAdminList_DeletedCoverIsNotResolved pins the gate that the rail
// got from its spliced predicate and the admin lateral must now carry
// EXPLICITLY: soft-delete. Dropping the caller-scoped splice is the
// point of #625; dropping the soft-delete assertion that rode along
// inside it would let a deleted asset render as a cover for operators.
func TestAdminList_DeletedCoverIsNotResolved(t *testing.T) {
	pool := railPool(t)
	ctx := context.Background()

	cover := railStoredAsset(t, pool, "admin-cover deleted", "public", true)
	coll := railCollection(t, pool, "admin-cover deleted collection", "public")
	railPostInCollection(t, pool, coll, cover, time.Hour)
	place(t, pool, "collection", coll, "public", 9202)

	if _, err := pool.Exec(ctx,
		`UPDATE assets SET deleted_at = now() WHERE id = $1`, cover); err != nil {
		t.Fatalf("soft-delete cover: %v", err)
	}

	row, ok := adminRowFor(t, pool, coll)
	if !ok {
		t.Fatal("collection placement missing from the admin list")
	}
	if row.CoverAssetID.Valid {
		t.Error("a soft-deleted asset resolved as a collection cover — the " +
			"admin lateral dropped the soft-delete gate along with the " +
			"caller-scoped ones (#625)")
	}
}

// TestAdminList_AssetSubjectCarriesCoverAssetID — #619 must not regress,
// and the client now keys thumbUrl on cover_asset_id for BOTH kinds, so
// an asset subject must carry its own id there.
func TestAdminList_AssetSubjectCarriesCoverAssetID(t *testing.T) {
	pool := railPool(t)

	asset := railStoredAsset(t, pool, "admin-cover asset-subject", "team", true)
	place(t, pool, "asset", asset, "public", 9203)

	row, ok := adminRowFor(t, pool, asset)
	if !ok {
		t.Fatal("asset placement missing from the admin list")
	}
	if !row.CoverAssetID.Valid || uuid.UUID(row.CoverAssetID.Bytes) != asset {
		t.Fatalf("asset subject's cover_asset_id = %v, want its own id — the "+
			"client keys thumbUrl on this for both kinds now", row.CoverAssetID)
	}
	// Team-tier on purpose: the admin asset branch must not carry the
	// rail's sensitivity wrapper either.
	if row.AssetFileHash == "" || !row.AssetPreviewAvailable {
		t.Error("team-tier asset subject lost its thumbnail hints in the " +
			"admin list; the sensitivity='public' wrapper is back (#625)")
	}
}
