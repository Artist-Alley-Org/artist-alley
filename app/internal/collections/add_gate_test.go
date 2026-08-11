// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #882 — the add gate on POST /collections/{id}/resources: you may only
// collect what you can actually see.
//
// # What makes this file worth reading
//
// The gate is trivially easy to build so that it gates NOTHING. Per ADR
// 0064 sensitivity lives on the CONTENT plane, so EntityAsset's
// authenticated ROW predicate is `deleted_at IS NULL` and nothing more
// (visibility/predicate.go). An implementation that checks only
// visibility.CanSee(EntityAsset) therefore admits every restricted
// asset in the instance while looking, in review, exactly like a
// working gate.
//
// So the load-bearing case in here is
// TestAddCollectionResource_Gate/restricted_asset_owned_by_a_stranger:
// an asset that IS row-visible to the caller and IS NOT content-
// readable. That single case is what a CanSee-only gate fails and a
// real gate passes. Every other case exists to stop the cheap ways of
// passing it:
//
//   - "public asset owned by a stranger" — a deny-everything gate
//     passes the restricted case and breaks the feature. This is the
//     half that proves the endpoint still does its job.
//   - "own restricted asset" — the owner short-circuit; existing
//     behaviour must be unchanged.
//   - "nonexistent uuid" vs the restricted case — asserted BYTE FOR
//     BYTE identical, because a distinguishable refusal is the
//     enumeration oracle the decision exists to remove.
//   - "soft-deleted public asset" — the ROW conjunct on its own
//     account. ContentReadable never looks at deleted_at, so a
//     content-plane-only gate admits a deleted public asset and creates
//     a member row the contents query then drops in SQL.
//
// Every refusal also asserts that no `collection_resources` row landed
// — a 404 whose write went through anyway is not a refusal.
//
// Skips without AA_DB_PASSWORD.

package collections_test

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agCollectionOwner int64 = 882001 // owns the collection; does the adding
	agStranger        int64 = 882002 // owns the assets; never calls anything
)

// agSoftDelete marks an asset deleted the way the row predicate reads
// it, so the ROW conjunct has something to refuse.
func agSoftDelete(t *testing.T, pool *pgxpool.Pool, assetID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = now() WHERE id = $1`, assetID); err != nil {
		t.Fatalf("soft-delete asset: %v", err)
	}
}

// agMembershipCount reports how many collection_resources rows exist
// for the pair. A refusal that still wrote the row is not a refusal.
func agMembershipCount(t *testing.T, pool *pgxpool.Pool, colID, assetID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM collection_resources WHERE collection_id = $1 AND asset_id = $2`,
		colID, assetID).Scan(&n); err != nil {
		t.Fatalf("count membership: %v", err)
	}
	return n
}

func TestAddCollectionResource_Gate(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestCollections(t, pool)
	t.Cleanup(func() {
		cleanTestCollections(t, pool)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE owner_user_ref IN ($1, $2)`, agCollectionOwner, agStranger)
	})

	router, _ := makeRouter(t, pool, agCollectionOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{
		"name": "ct_add_gate", "visibility": "private",
	})

	// Assets owned by SOMEONE ELSE. All active + ready, so the only
	// thing that can vary between them is the plane under test.
	strangerPublic := mustInsertAsset(t, pool, agStranger, "ct_gate_stranger_public")
	strangerRestricted := mustInsertAsset(t, pool, agStranger, "ct_gate_stranger_restricted")
	strangerTeam := mustInsertAsset(t, pool, agStranger, "ct_gate_stranger_team")
	strangerDeleted := mustInsertAsset(t, pool, agStranger, "ct_gate_stranger_deleted")
	setAssetTier(t, pool, strangerPublic, "active", "public")
	setAssetTier(t, pool, strangerRestricted, "active", "restricted")
	setAssetTier(t, pool, strangerTeam, "active", "team")
	setAssetTier(t, pool, strangerDeleted, "active", "public")
	agSoftDelete(t, pool, strangerDeleted)

	// The caller's OWN restricted asset — the owner short-circuit.
	ownRestricted := mustInsertAsset(t, pool, agCollectionOwner, "ct_gate_own_restricted")
	setAssetTier(t, pool, ownRestricted, "active", "restricted")

	missing := uuid.New().String()

	cases := []struct {
		name    string
		assetID string
		want    int
		why     string
	}{
		{
			name:    "restricted asset owned by a stranger",
			assetID: strangerRestricted,
			want:    http.StatusNotFound,
			why: "THE discriminating case. This asset is ROW-visible to the caller — " +
				"EntityAsset's authenticated predicate is deleted_at IS NULL and nothing " +
				"more (ADR 0064) — and is NOT content-readable. A gate built on " +
				"visibility.CanSee(EntityAsset) admits it while gating nothing",
		},
		{
			name:    "team asset owned by a stranger, caller not in the team",
			assetID: strangerTeam,
			want:    http.StatusNotFound,
			why:     "the team tier resolves through membership; a non-member is refused",
		},
		{
			name:    "soft-deleted public asset owned by a stranger",
			assetID: strangerDeleted,
			want:    http.StatusNotFound,
			why: "the ROW conjunct on its own account: ContentReadable never reads " +
				"deleted_at, so a content-plane-only gate would pin a deleted asset and " +
				"create a member row the contents query drops in SQL",
		},
		{
			name:    "nonexistent uuid",
			assetID: missing,
			want:    http.StatusNotFound,
			why:     "unchanged; the body is compared against the restricted case below",
		},
		{
			name:    "public asset owned by a stranger",
			assetID: strangerPublic,
			want:    http.StatusNoContent,
			why: "the feature half. Collecting someone else's PUBLIC work is the whole " +
				"point of #882 — a deny-everything gate passes every case above and " +
				"fails only this one",
		},
		{
			name:    "caller's own restricted asset",
			assetID: ownRestricted,
			want:    http.StatusNoContent,
			why:     "the owner reaches their own asset at any tier; existing behaviour unchanged",
		},
	}

	bodies := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := postJSON(t, router, "/collections/"+colID+"/resources",
				map[string]any{"asset_id": tc.assetID})
			if rr.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s\nwhy this case exists: %s",
					rr.Code, tc.want, rr.Body.String(), tc.why)
			}
			bodies[tc.name] = rr.Body.String()

			got := agMembershipCount(t, pool, colID, tc.assetID)
			want := 0
			if tc.want == http.StatusNoContent {
				want = 1
			}
			if got != want {
				t.Errorf("collection_resources rows for this pair = %d, want %d — a %d "+
					"that still wrote the membership is not a refusal", got, want, rr.Code)
			}
		})
	}

	// The enumeration assertion. These two must be indistinguishable to
	// a caller: same status (asserted above) AND same bytes. If the
	// restricted case ever answers 403, or "forbidden", or anything the
	// missing case does not, POST becomes a UUID-existence probe.
	restricted := bodies["restricted asset owned by a stranger"]
	nonexistent := bodies["nonexistent uuid"]
	if restricted != nonexistent {
		t.Errorf("an unreadable asset and a nonexistent one must be BYTE-IDENTICAL:\n"+
			"  unreadable:  %q\n  nonexistent: %q\n"+
			"any difference is an enumeration oracle — the caller learns the UUID is real",
			restricted, nonexistent)
	}
}

// TestRemoveCollectionResource_NeedsNoAssetGate pins the deliberate
// asymmetry (#882 scope): the gate is on ADD only.
//
// Removing is not a read — it un-pins a row from a collection the
// caller already had to prove they own (canMutateCollection), affects
// nobody else, and leaks nothing: DELETE answers 204 whether or not the
// membership existed, so it is not an existence probe either. Adding a
// readability check here would instead STRAND members — an asset whose
// sensitivity was raised after it was pinned could never be removed by
// the collection's owner.
//
// This test is the executable form of that reasoning: if someone later
// "fixes" remove by adding the add gate to it, this fails.
func TestRemoveCollectionResource_NeedsNoAssetGate(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestCollections(t, pool)
	t.Cleanup(func() {
		cleanTestCollections(t, pool)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE owner_user_ref IN ($1, $2)`, agCollectionOwner, agStranger)
	})

	router, _ := makeRouter(t, pool, agCollectionOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{
		"name": "ct_add_gate_remove", "visibility": "private",
	})

	// Pin it while it is public, then raise its sensitivity out from
	// under the collection — the exact state that would strand a member
	// if removal were gated on readability.
	asset := mustInsertAsset(t, pool, agStranger, "ct_gate_raised")
	setAssetTier(t, pool, asset, "active", "public")
	if rr := postJSON(t, router, "/collections/"+colID+"/resources",
		map[string]any{"asset_id": asset}); rr.Code != http.StatusNoContent {
		t.Fatalf("pin while public: status=%d body=%s", rr.Code, rr.Body.String())
	}
	setAssetTier(t, pool, asset, "active", "restricted")

	if rr := deleteReq(t, router, "/collections/"+colID+"/resources/"+asset); rr.Code != http.StatusNoContent {
		t.Fatalf("remove after the tier was raised: status=%d body=%s — removal is "+
			"deliberately NOT gated on readability; gating it strands members",
			rr.Code, rr.Body.String())
	}
	if n := agMembershipCount(t, pool, colID, asset); n != 0 {
		t.Errorf("membership rows after remove = %d, want 0", n)
	}
}
