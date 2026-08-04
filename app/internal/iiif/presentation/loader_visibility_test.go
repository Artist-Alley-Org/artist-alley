// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #661 — the IIIF Presentation manifest route had no ROW plane.
//
// sensitivity_test.go next door is a pure unit test on the builder's
// CONTENT plane (ADR 0064). It passed throughout, and it could not
// have caught any of this: the defect was in what the loader SELECTs,
// and the builder never sees a row the loader refused to return.
//
// Three concrete failures, all reproduced below against a live DB:
//
//  1. LoadAsset read `WHERE id = $1 AND deleted_at IS NULL`. The
//     anonymous EntityAsset predicate also requires `status='active'`
//     and `processing_status='ready'` (visibility/predicate.go), so a
//     DRAFT asset with public sensitivity served a full anonymous
//     manifest — title, description, custom-field metadata, a canvas —
//     while GET /assets/{same id} returned 404.
//
//  2. LoadCollectionMembers had the same omission, so an anonymous
//     collection manifest LISTED draft members whose own manifest the
//     same caller could not fetch. A list wider than the item read is
//     the invariant epic #665 names.
//
//  3. LoadCollection had NO filter at all — not the visibility
//     disjunction, not `deleted_at`. Anonymous callers were saved only
//     by a default-deny switch in http.go; AUTHENTICATED callers were
//     not saved by anything, so any signed-in user could read the
//     manifest of anyone else's PRIVATE collection, member list
//     included. That is the #660 shape on a different route, and it is
//     the most serious finding in this file.
//
// The tests drive the mounted chi handler, not the loader, because the
// question is what the ROUTE serves. Skips without AA_DB_PASSWORD.

package presentation

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	iiifOwner    int64 = 4290662
	iiifStranger int64 = 4290663
)

func iiifPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type iiifAssetSeed struct {
	label       string
	status      string
	sensitivity string
	processing  string
	deleted     bool
	// anonVisible mirrors the anonymous EntityAsset predicate.
	anonVisible bool
}

var iiifAssetSeeds = []iiifAssetSeed{
	{label: "public", status: "active", sensitivity: "public", processing: "ready", anonVisible: true},
	{label: "draft", status: "draft", sensitivity: "public", processing: "ready"},
	{label: "archived", status: "archived", sensitivity: "public", processing: "ready"},
	{label: "processing", status: "active", sensitivity: "public", processing: "processing"},
	{label: "restricted", status: "active", sensitivity: "restricted", processing: "ready"},
	{label: "soft-deleted", status: "active", sensitivity: "public", processing: "ready", deleted: true},
}

type iiifCollSeed struct {
	label      string
	visibility string
	deleted    bool
}

var iiifCollSeeds = []iiifCollSeed{
	{label: "coll-public", visibility: "public"},
	{label: "coll-private", visibility: "private"},
	{label: "coll-org", visibility: "org-only"},
	{label: "coll-public-deleted", visibility: "public", deleted: true},
}

type iiifFixture struct {
	assets      map[string]uuid.UUID
	collections map[string]uuid.UUID
}

// seedIIIF plants assets, collections, and pins EVERY asset into every
// collection so the member list exercises the row predicate.
func seedIIIF(t *testing.T, pool *pgxpool.Pool) iiifFixture {
	t.Helper()
	ctx := context.Background()
	f := iiifFixture{
		assets:      make(map[string]uuid.UUID, len(iiifAssetSeeds)),
		collections: make(map[string]uuid.UUID, len(iiifCollSeeds)),
	}

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM collection_resources WHERE collection_id IN (SELECT id FROM collections WHERE owner_user_ref=$1)`, iiifOwner)
		_, _ = pool.Exec(bg, `DELETE FROM collections WHERE owner_user_ref=$1`, iiifOwner)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE owner_user_ref=$1`, iiifOwner)
	})

	for i, s := range iiifAssetSeeds {
		id := uuid.New()
		f.assets[s.label] = id
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
			                    processing_status, created_at, deleted_at)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),$4,$5,$6,
			        NOW() - ($7::int * INTERVAL '1 minute'), `+del+`)`,
			id, "#661 iiif "+s.label, iiifOwner, s.status, s.sensitivity, s.processing, i); err != nil {
			t.Fatalf("seed asset %s: %v", s.label, err)
		}
	}

	for i, c := range iiifCollSeeds {
		id := uuid.New()
		f.collections[c.label] = id
		del := "NULL"
		if c.deleted {
			del = "NOW()"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO collections (id, name, description, owner_user_ref, visibility, membership,
			                         created_at, deleted_at)
			VALUES ($1,$2,$3,$4,$5,'manual', NOW() - ($6::int * INTERVAL '1 minute'), `+del+`)`,
			id, "#661 "+c.label, "secret collection description", iiifOwner, c.visibility, i); err != nil {
			t.Fatalf("seed collection %s: %v", c.label, err)
		}
		for j, s := range iiifAssetSeeds {
			if _, err := pool.Exec(ctx, `
				INSERT INTO collection_resources (collection_id, asset_id, sort_order, pinned)
				VALUES ($1,$2,$3,TRUE) ON CONFLICT DO NOTHING`,
				id, f.assets[s.label], j); err != nil {
				t.Fatalf("pin %s into %s: %v", s.label, c.label, err)
			}
		}
	}
	return f
}

// iiifRouter mounts the real Presentation handler for one caller. A nil
// identity is anonymous. The cache is left nil so each request re-reads
// — a cached body would mask which caller the loader answered for.
func iiifRouter(t *testing.T, pool *pgxpool.Pool, id *auth.Identity) chi.Router {
	t.Helper()
	h := &Handler{
		Loader: NewLoader(pool),
		Builder: NewBuilder(BuilderConfig{
			SiteBaseURL: "https://art.example.test",
			Provider:    Provider{Label: EN("Test"), Type: "Agent"},
		}),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	r := chi.NewRouter()
	if id != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
			})
		})
	}
	h.Mount(r)
	return r
}

func getManifest(t *testing.T, r chi.Router, path string) (int, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body
}

func assetManifestPath(id uuid.UUID) string {
	return "/iiif/3/asset/" + id.String() + "/manifest.json"
}

func collectionManifestPath(id uuid.UUID) string {
	return "/iiif/3/collection/" + id.String() + "/manifest.json"
}

// TestIIIFAssetManifest_AnonymousRowPlane is the #661 regression test
// for site 2. It fails on the pre-fix loader, which served draft,
// archived and still-processing assets to anonymous callers because it
// filtered on `deleted_at IS NULL` alone.
func TestIIIFAssetManifest_AnonymousRowPlane(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)
	anon := iiifRouter(t, pool, nil)

	// Non-vacuity: the one asset an anonymous caller may see must still
	// render, or every refusal below is trivially satisfied.
	if code, body := getManifest(t, anon, assetManifestPath(f.assets["public"])); code != http.StatusOK {
		t.Fatalf("anonymous manifest for a PUBLIC asset: status=%d body=%v — the gate over-narrowed", code, body)
	}

	for _, s := range iiifAssetSeeds {
		if s.anonVisible {
			continue
		}
		code, body := getManifest(t, anon, assetManifestPath(f.assets[s.label]))
		if code != http.StatusNotFound {
			t.Errorf("anonymous IIIF manifest for %q asset: status=%d, want 404 (label=%v) — "+
				"the row plane is missing the status/processing_status conjuncts",
				s.label, code, body["label"])
		}
	}
}

// TestIIIFAssetManifest_AuthenticatedNotNarrowed is the counterweight:
// the authenticated EntityAsset predicate is soft-delete only, so a
// signed-in caller still reaches a draft or restricted asset's
// manifest. Tightening the anonymous path must not tighten theirs.
func TestIIIFAssetManifest_AuthenticatedNotNarrowed(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)
	r := iiifRouter(t, pool, &auth.Identity{UserRef: iiifStranger, AuthMethod: "session"})

	for _, label := range []string{"public", "draft", "archived", "processing", "restricted"} {
		if code, _ := getManifest(t, r, assetManifestPath(f.assets[label])); code != http.StatusOK {
			t.Errorf("authenticated IIIF manifest for %q: status=%d, want 200 — "+
				"signing in must never remove access (#451)", label, code)
		}
	}
	if code, _ := getManifest(t, r, assetManifestPath(f.assets["soft-deleted"])); code != http.StatusNotFound {
		t.Errorf("authenticated IIIF manifest for a soft-deleted asset: status=%d, want 404", code)
	}
}

// TestIIIFCollectionManifest_AuthenticatedStrangerRefused is the #661
// regression test for site 3, and the most serious of the three. On the
// pre-fix loader this returned 200 with the collection's name,
// description and full member list for a caller who owns nothing and
// holds no grant.
func TestIIIFCollectionManifest_AuthenticatedStrangerRefused(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)
	stranger := iiifRouter(t, pool, &auth.Identity{UserRef: iiifStranger, AuthMethod: "session"})
	owner := iiifRouter(t, pool, &auth.Identity{UserRef: iiifOwner, AuthMethod: "session"})

	// Non-vacuity: the owner still gets their own private collection.
	if code, _ := getManifest(t, owner, collectionManifestPath(f.collections["coll-private"])); code != http.StatusOK {
		t.Fatalf("OWNER manifest for their own private collection: status=%d, want 200 — over-narrowed", code)
	}
	// And everybody gets the public one.
	if code, _ := getManifest(t, stranger, collectionManifestPath(f.collections["coll-public"])); code != http.StatusOK {
		t.Fatalf("authenticated manifest for a PUBLIC collection: status=%d, want 200 — over-narrowed", code)
	}

	for _, label := range []string{"coll-private", "coll-org"} {
		code, body := getManifest(t, stranger, collectionManifestPath(f.collections[label]))
		if code != http.StatusNotFound {
			t.Errorf("authenticated STRANGER read %q: status=%d label=%v items=%v — "+
				"any signed-in user could read anyone's private collection manifest",
				label, code, body["label"], body["items"])
		}
	}
}

// TestIIIFCollectionManifest_AclGranteeAdmitted proves the third
// disjunct of the EntityCollection predicate reaches this route, and is
// the reason the authenticated collection manifest is not cached: two
// signed-in callers legitimately get different answers for one id, so a
// cache keyed on "authenticated" alone would hand the grantee's
// manifest to the stranger. It also pins that the unqualified `id` in
// the predicate's ACL sub-select resolves to collections.id — the FROM
// here is un-aliased and collection_acls has no `id` column of its own.
func TestIIIFCollectionManifest_AclGranteeAdmitted(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)
	privID := f.collections["coll-private"]

	grantee := int64(4290664)
	// principal_id is TEXT (polymorphic principal), so bind the ref as a
	// string — pgx has no encode plan from int64 to OID 25.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO collection_acls (collection_id, principal_type, principal_id, permission)
		VALUES ($1,'user',$2,'read') ON CONFLICT DO NOTHING`,
		privID, strconv.FormatInt(grantee, 10)); err != nil {
		t.Fatalf("grant acl: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM collection_acls WHERE collection_id=$1`, privID)
	})

	granteeR := iiifRouter(t, pool, &auth.Identity{UserRef: grantee, AuthMethod: "session"})
	if code, _ := getManifest(t, granteeR, collectionManifestPath(privID)); code != http.StatusOK {
		t.Errorf("ACL grantee read of a private collection manifest: status=%d, want 200", code)
	}
	strangerR := iiifRouter(t, pool, &auth.Identity{UserRef: iiifStranger, AuthMethod: "session"})
	if code, _ := getManifest(t, strangerR, collectionManifestPath(privID)); code != http.StatusNotFound {
		t.Errorf("non-grantee read of the same private collection: status=%d, want 404", code)
	}
}

// TestIIIFCollectionManifest_SoftDeleted covers the specific omission
// the issue names: LoadCollection had no `deleted_at` filter, and the
// only thing standing between an anonymous caller and a soft-deleted
// collection was a default-deny switch in a different file.
func TestIIIFCollectionManifest_SoftDeleted(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)

	for _, tc := range []struct {
		name string
		id   *auth.Identity
	}{
		{"anonymous", nil},
		{"owner", &auth.Identity{UserRef: iiifOwner, AuthMethod: "session"}},
		{"stranger", &auth.Identity{UserRef: iiifStranger, AuthMethod: "session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := iiifRouter(t, pool, tc.id)
			code, body := getManifest(t, r, collectionManifestPath(f.collections["coll-public-deleted"]))
			if code != http.StatusNotFound {
				t.Errorf("%s read a SOFT-DELETED collection manifest: status=%d label=%v", tc.name, code, body["label"])
			}
		})
	}
}

// TestIIIFCollectionManifest_AnonymousPublicIsServed pins the other
// half of the drift. `public` was missing from LoadCollection's
// visibility→sensitivity switch and fell through to the fail-closed
// default, so a genuinely public collection 404'd anonymously. The
// switch's comment still claimed no such value existed — it was written
// before migration 00008 added the tier (#414).
func TestIIIFCollectionManifest_AnonymousPublicIsServed(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)
	anon := iiifRouter(t, pool, nil)

	code, body := getManifest(t, anon, collectionManifestPath(f.collections["coll-public"]))
	if code != http.StatusOK {
		t.Fatalf("anonymous manifest for a PUBLIC collection: status=%d, want 200", code)
	}

	// And the member list is row-gated: only the anonymously-visible
	// asset survives, even though every seeded asset is pinned.
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Errorf("anonymous public collection listed %d members; want 1 (only the public/active/ready one) — "+
			"the member list must not be wider than the per-asset manifest read", len(items))
	}

	// Nail it to the specific ids: every member listed must itself be
	// fetchable by the same caller. This is epic #665's invariant.
	for _, it := range items {
		m, _ := it.(map[string]any)
		raw, _ := m["id"].(string)
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("member id %q is not a URL: %v", raw, err)
		}
		rr := httptest.NewRecorder()
		anon.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, u.Path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("collection listed member %s but its own manifest returns %d for the same caller", raw, rr.Code)
		}
	}
}

// TestIIIFCollectionMembers_AnonymousRowPlane states the member-list
// invariant directly, independent of the count above so a fixture
// change cannot make it vacuous.
func TestIIIFCollectionMembers_AnonymousRowPlane(t *testing.T) {
	pool := iiifPool(t)
	f := seedIIIF(t, pool)

	members, err := NewLoader(pool).LoadCollectionMembers(
		context.Background(), f.collections["coll-public"], visibility.NewCaller(nil), nil, 200)
	if err != nil {
		t.Fatalf("LoadCollectionMembers: %v", err)
	}
	allowed := map[uuid.UUID]bool{f.assets["public"]: true}
	if len(members) == 0 {
		t.Fatal("no members returned — vacuous")
	}
	for _, m := range members {
		if !allowed[m.ID] {
			t.Errorf("anonymous member list included %v (%q), which the anonymous predicate withholds", m.ID, m.Title)
		}
		// #883 — and every row it DOES return must be marked readable,
		// or the manifest builder would silently drop it.
		if !m.MemberReadable {
			t.Errorf("member %v survived the predicate but MemberReadable is false", m.ID)
		}
	}
}
