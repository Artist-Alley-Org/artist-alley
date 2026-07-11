// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

// TestCollectionLifecycle covers create / get / patch / delete on the
// collection entity, plus the ownership gate that prevents one user
// from mutating another's collection.
func TestCollectionLifecycle(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = ctx
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	ownerRouter, _ := makeRouter(t, pool, 720001, /*admin=*/ false)
	intruderRouter, _ := makeRouter(t, pool, 720002, /*admin=*/ false)

	// Missing body -> 400
	bad := postJSON(t, ownerRouter, "/collections", map[string]any{})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("missing name: status=%d want 400 body=%s", bad.Code, bad.Body.String())
	}

	// Create
	rr := postJSON(t, ownerRouter, "/collections", map[string]any{
		"name":        "ct_main",
		"description": "test fixture",
		"visibility":  "private",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var c openapi.Collection
	mustDecode(t, rr.Body.Bytes(), &c)
	if c.Name != "ct_main" {
		t.Errorf("name=%q want ct_main", c.Name)
	}
	if c.Visibility != openapi.CollectionVisibilityPrivate {
		t.Errorf("visibility=%q want private", c.Visibility)
	}
	if c.OwnerUserRef != 720001 {
		t.Errorf("owner=%d want 720001", c.OwnerUserRef)
	}
	id := c.Id.String()

	// Query/hybrid membership -> 400 in 1.11.A
	deferred := postJSON(t, ownerRouter, "/collections", map[string]any{
		"name":       "ct_query_not_yet",
		"membership": "query",
	})
	if deferred.Code != http.StatusBadRequest {
		t.Errorf("query membership: status=%d want 400 body=%s", deferred.Code, deferred.Body.String())
	}

	// Get -> 200
	gRR := httptest.NewRecorder()
	ownerRouter.ServeHTTP(gRR, httptest.NewRequest(http.MethodGet, "/collections/"+id, nil))
	if gRR.Code != http.StatusOK {
		t.Fatalf("get: %d", gRR.Code)
	}

	// Patch (rename + feature)
	pRR := patchJSON(t, ownerRouter, "/collections/"+id, map[string]any{
		"name":     "ct_renamed",
		"featured": true,
	})
	if pRR.Code != http.StatusOK {
		t.Fatalf("patch: %d body=%s", pRR.Code, pRR.Body.String())
	}
	var patched openapi.Collection
	mustDecode(t, pRR.Body.Bytes(), &patched)
	if patched.Name != "ct_renamed" || !patched.Featured {
		t.Errorf("patch didn't take: name=%q featured=%v", patched.Name, patched.Featured)
	}

	// Intruder cannot patch
	intruderPatch := patchJSON(t, intruderRouter, "/collections/"+id, map[string]any{
		"name": "ct_hijacked",
	})
	if intruderPatch.Code != http.StatusForbidden {
		t.Errorf("intruder patch: status=%d want 403", intruderPatch.Code)
	}

	// Intruder cannot delete
	intruderDel := deleteReq(t, intruderRouter, "/collections/"+id)
	if intruderDel.Code != http.StatusForbidden {
		t.Errorf("intruder delete: status=%d want 403", intruderDel.Code)
	}

	// Owner deletes -> 204
	delRR := deleteReq(t, ownerRouter, "/collections/"+id)
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("delete: %d body=%s", delRR.Code, delRR.Body.String())
	}

	// Get after delete -> 404
	missRR := httptest.NewRecorder()
	ownerRouter.ServeHTTP(missRR, httptest.NewRequest(http.MethodGet, "/collections/"+id, nil))
	if missRR.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d want 404", missRR.Code)
	}
}

// TestCollectionResources covers add / list / remove on the membership
// side, including pagination and the cascade through DeleteCollection.
func TestCollectionResources(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	router, userRef := makeRouter(t, pool, 720010, false)

	// Create the collection
	col := mustCreate(t, router, map[string]any{
		"name":       "ct_with_members",
		"visibility": "private",
	})

	// Adding to a non-existent asset -> 404
	noAsset := postJSON(t, router, "/collections/"+col+"/resources", map[string]any{
		"asset_id": uuid.New().String(),
	})
	if noAsset.Code != http.StatusNotFound {
		t.Errorf("missing asset: status=%d want 404 body=%s", noAsset.Code, noAsset.Body.String())
	}

	// Insert two assets directly so we don't have to wire the assets
	// handler in. The cleanup hook walks them by owner_user_ref.
	asset1 := mustInsertAsset(t, pool, userRef, "asset-1")
	asset2 := mustInsertAsset(t, pool, userRef, "asset-2")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE owner_user_ref = $1`, userRef)
	})

	// Add asset1 (sort_order defaults to 0), asset2 (sort_order 10)
	r := postJSON(t, router, "/collections/"+col+"/resources", map[string]any{
		"asset_id": asset1,
	})
	if r.Code != http.StatusNoContent {
		t.Fatalf("add asset1: %d body=%s", r.Code, r.Body.String())
	}
	r = postJSON(t, router, "/collections/"+col+"/resources", map[string]any{
		"asset_id":   asset2,
		"sort_order": 10,
	})
	if r.Code != http.StatusNoContent {
		t.Fatalf("add asset2: %d body=%s", r.Code, r.Body.String())
	}

	// Re-add asset1 with new sort_order to confirm upsert
	r = postJSON(t, router, "/collections/"+col+"/resources", map[string]any{
		"asset_id":   asset1,
		"sort_order": 5,
	})
	if r.Code != http.StatusNoContent {
		t.Fatalf("re-add asset1: %d body=%s", r.Code, r.Body.String())
	}

	// List -> 2 entries in (5, 10) sort_order
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, httptest.NewRequest(http.MethodGet, "/collections/"+col+"/resources", nil))
	if listRR.Code != http.StatusOK {
		t.Fatalf("list resources: %d", listRR.Code)
	}
	var page openapi.CollectionResourceList
	mustDecode(t, listRR.Body.Bytes(), &page)
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(page.Items))
	}
	if page.Items[0].SortOrder != 5 || page.Items[1].SortOrder != 10 {
		t.Errorf("sort order: got %d, %d; want 5, 10", page.Items[0].SortOrder, page.Items[1].SortOrder)
	}

	// Page through with limit=1 to exercise the cursor
	pageRR := httptest.NewRecorder()
	router.ServeHTTP(pageRR, httptest.NewRequest(http.MethodGet, "/collections/"+col+"/resources?limit=1", nil))
	mustDecode(t, pageRR.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("first page: items=%d cursor=%v want 1 + cursor", len(page.Items), page.NextCursor)
	}
	page2RR := httptest.NewRecorder()
	router.ServeHTTP(page2RR, httptest.NewRequest(http.MethodGet,
		"/collections/"+col+"/resources?limit=1&cursor="+*page.NextCursor, nil))
	var page2 openapi.CollectionResourceList
	mustDecode(t, page2RR.Body.Bytes(), &page2)
	if len(page2.Items) != 1 {
		t.Errorf("second page: %d want 1", len(page2.Items))
	}
	if page2.NextCursor != nil {
		t.Errorf("second page should have no next_cursor, got %q", *page2.NextCursor)
	}

	// Remove asset2 -> list shrinks to 1
	rmRR := deleteReq(t, router, "/collections/"+col+"/resources/"+asset2)
	if rmRR.Code != http.StatusNoContent {
		t.Fatalf("remove: %d", rmRR.Code)
	}
	listRR2 := httptest.NewRecorder()
	router.ServeHTTP(listRR2, httptest.NewRequest(http.MethodGet, "/collections/"+col+"/resources", nil))
	var page3 openapi.CollectionResourceList
	mustDecode(t, listRR2.Body.Bytes(), &page3)
	if len(page3.Items) != 1 {
		t.Errorf("after remove: %d want 1", len(page3.Items))
	}

	// Phase 1.55.C-1b: DELETE is now soft-delete on collections.
	// The row survives (with deleted_at set); collection_resources
	// stays intact for the recovery window. The nightly softdelete.gc
	// hard-deletes past retention, at which point the FK cascade
	// fires — but that's a coordinator responsibility, not a
	// HTTP-handler assertion.
	if dr := deleteReq(t, router, "/collections/"+col); dr.Code != http.StatusNoContent {
		t.Fatalf("delete collection: %d", dr.Code)
	}
	var deletedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM collections WHERE id = $1`, col).Scan(&deletedAt); err != nil {
		t.Fatalf("post-delete row: %v", err)
	}
	if deletedAt == nil {
		t.Errorf("expected deleted_at to be set on soft-deleted collection; got NULL")
	}
	// Memberships are intentionally preserved for the recovery
	// window — the gc coordinator handles hard-delete + cascade.
	var stillPresent int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM collection_resources WHERE collection_id = $1`, col).Scan(&stillPresent); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stillPresent == 0 {
		t.Errorf("expected collection_resources to survive soft-delete; got 0 rows")
	}
}

// TestListCollectionsFilters exercises the owner_ref and featured
// filters on the list endpoint, and verifies cursor-based pagination
// across two pages.
func TestListCollectionsFilters(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	router, _ := makeRouter(t, pool, 720020, false)

	// Make three with deterministic created_at spacing so the
	// cursor pagination has stable ordering.
	for i := 0; i < 3; i++ {
		body := map[string]any{"name": fmt.Sprintf("ct_list_%d", i)}
		if i == 0 {
			body["featured"] = true
		}
		rr := postJSON(t, router, "/collections", body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %d: %d", i, rr.Code)
		}
	}

	// owner_ref filter
	mineRR := httptest.NewRecorder()
	router.ServeHTTP(mineRR, httptest.NewRequest(http.MethodGet,
		"/collections?owner_ref=720020", nil))
	var page openapi.CollectionList
	mustDecode(t, mineRR.Body.Bytes(), &page)
	if len(page.Items) < 3 {
		t.Errorf("owner_ref filter: %d want >=3", len(page.Items))
	}

	// featured=true should return only the one we set
	featRR := httptest.NewRecorder()
	router.ServeHTTP(featRR, httptest.NewRequest(http.MethodGet,
		"/collections?owner_ref=720020&featured=true", nil))
	mustDecode(t, featRR.Body.Bytes(), &page)
	if len(page.Items) != 1 {
		t.Fatalf("featured=true: got %d items", len(page.Items))
	}
	if page.Items[0].Name != "ct_list_0" {
		t.Errorf("featured one: name=%q want ct_list_0", page.Items[0].Name)
	}

	// Cursor pagination: limit=2 then a second page using next_cursor
	p1 := httptest.NewRecorder()
	router.ServeHTTP(p1, httptest.NewRequest(http.MethodGet,
		"/collections?owner_ref=720020&limit=2", nil))
	mustDecode(t, p1.Body.Bytes(), &page)
	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("page1: items=%d cursor=%v", len(page.Items), page.NextCursor)
	}
	p2 := httptest.NewRecorder()
	router.ServeHTTP(p2, httptest.NewRequest(http.MethodGet,
		"/collections?owner_ref=720020&limit=2&cursor="+*page.NextCursor, nil))
	var page2 openapi.CollectionList
	mustDecode(t, p2.Body.Bytes(), &page2)
	if len(page2.Items) < 1 {
		t.Errorf("page2: %d items want >=1", len(page2.Items))
	}
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

func makeRouter(t *testing.T, pool *pgxpool.Pool, userRef int64, admin bool) (chi.Router, int64) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// nil registry — cache integration is covered by cache_test.go;
	// these tests stick to handler logic.
	h := collections.NewHandler(pool, logger, nil)
	// Phase 1.22.B-cleanup: handlers no longer carry a legacy
	// fallback. Tests that exercise Create/Update/Delete/Add/Remove
	// MUST wire an activities.Writer — otherwise those endpoints
	// return 503. The wired writer here has no notifier / no
	// resolver because these tests don't assert on activity
	// emission; they only need the gold path to run through.
	actWriter := activities.NewWriter(pool, logger, nil)
	h.SetActivitiesWriter(actWriter, func(ctx context.Context) string { return "https://test.example" })

	caps := []string{}
	if admin {
		caps = []string{collections.CapCollectionsAdmin}
	}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{
				UserRef:      userRef,
				AuthMethod:   "session",
				Capabilities: caps,
			}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(collShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router, userRef
}

func cleanTestCollections(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM collection_resources WHERE collection_id IN (SELECT id FROM collections WHERE name LIKE 'ct_%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM collections WHERE name LIKE 'ct_%'`)
}

func mustCreate(t *testing.T, r chi.Router, body map[string]any) string {
	t.Helper()
	rr := postJSON(t, r, "/collections", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rr.Code, rr.Body.String())
	}
	var c openapi.Collection
	mustDecode(t, rr.Body.Bytes(), &c)
	return c.Id.String()
}

func mustInsertAsset(t *testing.T, pool *pgxpool.Pool, userRef int64, title string) string {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO assets (title, asset_type, owner_user_ref) VALUES ($1, 1, $2) RETURNING id`,
		title, userRef).Scan(&id); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	return id.String()
}

func postJSON(t *testing.T, r chi.Router, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func patchJSON(t *testing.T, r chi.Router, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func deleteReq(t *testing.T, r chi.Router, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func mustDecode(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, data)
	}
}

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// shim — overrides only what the collections tests exercise; every
// other StrictServerInterface method panics via the embedded
// *strictservershim.PanicShim.
// ---------------------------------------------------------------------------

type collShim struct {
	*strictservershim.PanicShim
	h *collections.Handler
}

func (s collShim) ListCollections(ctx context.Context, req openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	return s.h.ListCollections(ctx, req)
}
func (s collShim) CreateCollection(ctx context.Context, req openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	return s.h.CreateCollection(ctx, req)
}
func (s collShim) GetCollection(ctx context.Context, req openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	return s.h.GetCollection(ctx, req)
}
func (s collShim) UpdateCollection(ctx context.Context, req openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	return s.h.UpdateCollection(ctx, req)
}
func (s collShim) DeleteCollection(ctx context.Context, req openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	return s.h.DeleteCollection(ctx, req)
}
func (s collShim) ListCollectionResources(ctx context.Context, req openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	return s.h.ListCollectionResources(ctx, req)
}
func (s collShim) AddCollectionResource(ctx context.Context, req openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	return s.h.AddCollectionResource(ctx, req)
}
func (s collShim) RemoveCollectionResource(ctx context.Context, req openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	return s.h.RemoveCollectionResource(ctx, req)
}





