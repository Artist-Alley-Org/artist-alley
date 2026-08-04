// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #883 — the IIIF collection manifest's member gate.
//
// #661 fixed the ANONYMOUS half of this: the member list carries the
// EntityAsset predicate, so an anonymous manifest cannot list an asset
// whose own manifest that caller would be refused. The half it did not
// cover is the AUTHENTICATED one, and it was a live leak: the
// per-member check in BuildCollectionManifest ran only `if isAnonymous`,
// so a signed-in caller received the TITLE of every restricted member as
// its `label`, in a collection they had nothing to do with.
//
// The gate is now visibility.FieldsReadable for every caller, decided in
// the loader and carried on EntityRef.MemberReadable — the same function
// the post and collection JSON APIs use, so the three surfaces cannot
// drift on what "may not see" means.
//
// WHY IIIF OMITS RATHER THAN PLACEHOLDERS. Everywhere else #883 renders
// a VISIBLE placeholder; here the member is dropped. A Collection's
// `items` are dereferenceable manifest references: a placeholder entry
// would point at /iiif/3/asset/{id}/manifest.json, every conforming
// viewer would follow it, and it would fail. IIIF has no request-access
// affordance to make a broken reference worth anything. The tests below
// pin the omission so that divergence is deliberate rather than
// forgotten.
//
// Skips without AA_DB_PASSWORD.

package presentation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// mgStranger is a signed-in caller who owns nothing in the fixture.
const mgStranger int64 = 8834001

// TestIIIFCollectionManifest_AuthenticatedMemberGate is the regression
// test for the leak this issue found.
func TestIIIFCollectionManifest_AuthenticatedMemberGate(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)

	r := iiifRouter(t, pool, &auth.Identity{UserRef: mgStranger, AuthMethod: "session"})
	code, body := getManifest(t, r, collectionManifestPath(f.collections["coll-public"]))
	if code != http.StatusOK {
		t.Fatalf("authenticated manifest for a PUBLIC collection: status=%d, want 200", code)
	}

	// The restricted member's title is seeded as "#661 iiif restricted".
	// Its presence anywhere in the manifest is the leak.
	items, _ := body["items"].([]any)
	restrictedURL := "/iiif/3/asset/" + f.assets["restricted"].String() + "/manifest.json"
	for _, it := range items {
		m, _ := it.(map[string]any)
		id, _ := m["id"].(string)
		if strings.HasSuffix(id, restrictedURL) {
			t.Errorf("a RESTRICTED member is listed in a signed-in stranger's collection "+
				"manifest, carrying label %v — the per-member gate used to run only for "+
				"anonymous callers", m["label"])
		}
	}
	if len(items) == 0 {
		t.Fatal("no members at all — the test would pass vacuously")
	}
}

// TestIIIFCollectionMembers_MemberReadableMatchesTheJSONAPI pins that
// the manifest's answer comes from the SAME function the JSON APIs use.
// If the loader ever grows its own copy of the rule, the two surfaces
// can disagree about one asset and only one of them will be tested.
func TestIIIFCollectionMembers_MemberReadableMatchesTheJSONAPI(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)

	ref := mgStranger
	caller := visibility.NewCaller(&ref)
	members, err := NewLoader(pool).LoadCollectionMembers(
		context.Background(), f.collections["coll-public"], caller, nil, 200)
	if err != nil {
		t.Fatalf("LoadCollectionMembers: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("no members returned — vacuous")
	}

	for _, m := range members {
		var row visibility.FieldsRow
		if err := pool.QueryRow(context.Background(), `
			SELECT a.sensitivity, a.status, a.processing_status, a.owner_user_ref, FALSE
			  FROM assets a WHERE a.id = $1`, m.ID,
		).Scan(&row.Sensitivity, &row.Status, &row.ProcessingStatus, &row.OwnerUserRef, &row.IsTeamMember); err != nil {
			t.Fatalf("read row %v: %v", m.ID, err)
		}
		want := visibility.FieldsReadable(row, caller, nil)
		if m.MemberReadable != want {
			t.Errorf("member %v (%q): loader says MemberReadable=%v, visibility.FieldsReadable says %v",
				m.ID, m.Title, m.MemberReadable, want)
		}
	}
}

// TestIIIFCollectionManifest_EveryListedMemberIsFetchable restates epic
// #665's invariant for the AUTHENTICATED caller — the direction #661
// only checked for anonymous. Every member the manifest advertises must
// itself resolve for the same caller, or the collection is publishing
// broken references.
func TestIIIFCollectionManifest_EveryListedMemberIsFetchable(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)

	r := iiifRouter(t, pool, &auth.Identity{UserRef: mgStranger, AuthMethod: "session"})
	code, body := getManifest(t, r, collectionManifestPath(f.collections["coll-public"]))
	if code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Fatal("no members — vacuous")
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		raw, _ := m["id"].(string)
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("member id %q is not a URL: %v", raw, err)
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, u.Path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("collection listed member %s but its own manifest returns %d for the same caller",
				raw, rr.Code)
		}
	}
}

// TestIIIFCollectionManifest_OwnerStillSeesEverything is the control:
// tightening the gate must not empty the owner's own manifest.
func TestIIIFCollectionManifest_OwnerStillSeesEverything(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)

	r := iiifRouter(t, pool, &auth.Identity{UserRef: iiifOwner, AuthMethod: "session"})
	code, body := getManifest(t, r, collectionManifestPath(f.collections["coll-public"]))
	if code != http.StatusOK {
		t.Fatalf("status=%d, want 200", code)
	}
	items, _ := body["items"].([]any)
	seen := map[uuid.UUID]bool{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		raw, _ := m["id"].(string)
		for label, id := range f.assets {
			_ = label
			if strings.Contains(raw, id.String()) {
				seen[id] = true
			}
		}
	}
	// The owner should see every seeded asset except the soft-deleted
	// one, which the row predicate removes for everybody.
	for _, s := range iiifAssetSeeds {
		id := f.assets[s.label]
		want := !s.deleted
		if seen[id] != want {
			t.Errorf("owner: member %q listed=%v, want %v", s.label, seen[id], want)
		}
	}
}
