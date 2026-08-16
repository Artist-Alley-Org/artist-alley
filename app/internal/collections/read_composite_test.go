// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// #1059 — "an instance admin may read any collection" had two homes.
//
// visibility.Filter(EntityCollection) has no admin disjunct, so the rule
// was applied OUTSIDE it: once in GetCollection, and again in
// facet.Selection.Authorize (#910) because that author copied the first.
// Two copies of one rule is a drift hazard whose symptom is not a leak
// but a feature that looks broken — an admin opens a foreign private
// collection's page perfectly well and then gets an empty result from
// the "Search in this collection" button on that same page.
//
// These tests put BOTH surfaces on the SAME collection in one test body,
// because that pairing is the whole claim. Either one alone would pass
// on the duplicated implementation.

// rcRouterWithCaps is makeRouter with an arbitrary capability set —
// makeRouter's bool only ever grants collections.admin, which is exactly
// the capability the negative control below needs to hold WITHOUT
// system.admin.
func rcRouterWithCaps(t *testing.T, pool *pgxpool.Pool, userRef int64, caps []string) chi.Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := collections.NewHandler(pool, logger, nil)
	h.SetActivitiesWriter(activities.NewWriter(pool, logger, nil),
		func(ctx context.Context) string { return "https://test.example" })

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(
		openapi.NewStrictHandler(collShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

// rcGetStatus is the collection-page surface.
func rcGetStatus(t *testing.T, r chi.Router, colID string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/collections/"+colID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr.Code
}

// rcMaySearchInside is the "Search in this collection" surface, called at
// the same seam the search Engine and the facet aggregator both call it:
// Selection.Authorize is the single parent gate for a container-scoped
// query.
func rcMaySearchInside(t *testing.T, pool *pgxpool.Pool, colID string, userRef int64, caps []string) bool {
	t.Helper()
	sel, err := facet.ParseSelection([]string{"collection:" + colID})
	if err != nil {
		t.Fatalf("parse selection: %v", err)
	}
	ref := userRef
	ok, err := sel.Authorize(context.Background(), pool,
		visibility.NewCaller(&ref),
		visibility.CapabilityChecker(func(code string) bool {
			for _, c := range caps {
				if c == code {
					return true
				}
			}
			return false
		}),
	)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return ok
}

// TestCollectionRead_AdminGetsOneVerdictOnBothSurfaces is the issue.
//
// A system.admin who is neither the owner nor a sharee of a private
// collection must be admitted by BOTH surfaces, and a stranger holding
// nothing must be refused by BOTH. Same collection, opposite verdicts,
// ONE rule — which is only true once both surfaces call
// visibility.CanReadCollection.
func TestCollectionRead_AdminGetsOneVerdictOnBothSurfaces(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_read_composite", "visibility": "private",
	})

	const rcAdmin int64 = 1059001
	adminCaps := []string{collections.CapSystemAdmin}

	// The admin is a foreigner to this collection: not the owner, no ACL.
	// Without the bypass the row plane refuses them, which is the whole
	// reason the bypass exists.
	if visible := rcRowPlaneSees(t, pool, colID, rcAdmin); visible {
		t.Fatalf("precondition: the row plane already admits user %d to a private "+
			"collection it does not own — this test would pass with the admin "+
			"disjunct deleted", rcAdmin)
	}

	adminRouter := rcRouterWithCaps(t, pool, rcAdmin, adminCaps)
	if got := rcGetStatus(t, adminRouter, colID); got != http.StatusOK {
		t.Errorf("GET /collections/%s as system.admin = %d, want 200", colID, got)
	}
	if !rcMaySearchInside(t, pool, colID, rcAdmin, adminCaps) {
		t.Errorf("system.admin refused by the collection-scoped search gate while " +
			"GET admitted them — an admin who can open the page and gets an empty " +
			"\"Search in this collection\" is #1059 exactly")
	}

	// And the same pair, inverted, for a caller holding nothing.
	const rcStranger int64 = 1059002
	strangerRouter := rcRouterWithCaps(t, pool, rcStranger, nil)
	if got := rcGetStatus(t, strangerRouter, colID); got != http.StatusNotFound {
		t.Errorf("GET /collections/%s as a stranger = %d, want 404", colID, got)
	}
	if rcMaySearchInside(t, pool, colID, rcStranger, nil) {
		t.Errorf("a stranger was allowed to scope a search to a private collection " +
			"they cannot open — the parent gate #910 added")
	}
}

// TestCollectionRead_CollectionsAdminIsNotSystemAdmin is the negative
// control that proves the cleanup widened nothing.
//
// `collections.admin` is a MUTATION capability (canMutateCollection
// admits it alongside system.admin). It has never granted a read past
// the row plane, and folding the read bypass into a helper must not
// start. If this test goes green on a build where the previous test also
// passes, the composite is admitting the wrong capability.
func TestCollectionRead_CollectionsAdminIsNotSystemAdmin(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_read_collections_admin", "visibility": "private",
	})

	const rcCurator int64 = 1059003
	caps := []string{collections.CapCollectionsAdmin}

	if got := rcGetStatus(t, rcRouterWithCaps(t, pool, rcCurator, caps), colID); got != http.StatusNotFound {
		t.Errorf("GET /collections/%s as collections.admin (no system.admin) = %d, "+
			"want 404 — collections.admin is a write capability and must not have "+
			"acquired a read bypass", colID, got)
	}
	if rcMaySearchInside(t, pool, colID, rcCurator, caps) {
		t.Errorf("collections.admin was allowed to scope a search to a foreign " +
			"private collection — the composite is checking the wrong capability")
	}
}

// TestCollectionRead_SoftDeletedStaysInvisibleToNonAdmins guards the
// OTHER system.admin check in this handler, the one that must NOT be
// absorbed into the read composite: "an admin also sees soft-deleted
// rows, so the Restore button has something to render" is a different
// question about a different set of rows. The owner of a soft-deleted
// collection still gets a 404 — merging that branch into the read rule
// would have changed this.
func TestCollectionRead_SoftDeletedStaysInvisibleToNonAdmins(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_read_softdeleted", "visibility": "private",
	})
	if got := rcGetStatus(t, ownerRouter, colID); got != http.StatusOK {
		t.Fatalf("precondition: owner cannot read their own collection (%d)", got)
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE collections SET deleted_at = NOW(), deleted_reason = 'test' WHERE id = $1`,
		colID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	if got := rcGetStatus(t, ownerRouter, colID); got != http.StatusNotFound {
		t.Errorf("GET a soft-deleted collection as its OWNER = %d, want 404", got)
	}
	// The admin still sees it — via the ErrNoRows branch, which the read
	// composite has to leave reachable. An admin passes the read gate on
	// the capability without the row plane agreeing, exactly so this
	// branch is still entered.
	adminRouter := rcRouterWithCaps(t, pool, 1059004, []string{collections.CapSystemAdmin})
	if got := rcGetStatus(t, adminRouter, colID); got != http.StatusOK {
		t.Errorf("GET a soft-deleted collection as system.admin = %d, want 200 — "+
			"the Restore button on /collections/{id} has nothing to render without it", got)
	}
}

// rcRowPlaneSees asks the row plane alone — no capabilities — so a test
// can assert its own precondition rather than assuming it.
func rcRowPlaneSees(t *testing.T, pool *pgxpool.Pool, colID string, userRef int64) bool {
	t.Helper()
	id, err := uuid.Parse(colID)
	if err != nil {
		t.Fatalf("parse collection id: %v", err)
	}
	ref := userRef
	ok, err := visibility.CanSee(context.Background(), pool,
		visibility.EntityCollection, visibility.NewCaller(&ref), id)
	if err != nil {
		t.Fatalf("CanSee: %v", err)
	}
	return ok
}
