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

	// Delete collection cascades to memberships
	if dr := deleteReq(t, router, "/collections/"+col); dr.Code != http.StatusNoContent {
		t.Fatalf("delete collection: %d", dr.Code)
	}
	var leftover int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM collection_resources WHERE collection_id = $1`, col).Scan(&leftover); err != nil {
		t.Fatalf("count: %v", err)
	}
	if leftover != 0 {
		t.Errorf("collection_resources still has %d rows after cascade", leftover)
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
	openapi.HandlerFromMux(openapi.NewStrictHandler(collShim{h: h}, nil), router)
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
// shim — implements every other StrictServerInterface method as a panic.
// ---------------------------------------------------------------------------

type collShim struct{ h *collections.Handler }

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

func (collShim) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from collections test shim")
}
func (collShim) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from collections test shim")
}
func (collShim) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from collections test shim")
}
func (collShim) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from collections test shim")
}
func (collShim) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from collections test shim")
}
func (collShim) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from collections test shim")
}
func (collShim) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from collections test shim")
}
func (collShim) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from collections test shim")
}
func (collShim) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from collections test shim")
}
func (collShim) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from collections test shim")
}
func (collShim) ListAssetTypes(context.Context, openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	panic("ListAssetTypes called from collections test shim")
}
func (collShim) UploadStorageObject(context.Context, openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	panic("UploadStorageObject called from collections test shim")
}
func (collShim) DownloadStorageObject(context.Context, openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	panic("DownloadStorageObject called from collections test shim")
}
func (collShim) DownloadStorageObjectVariant(context.Context, openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	panic("DownloadStorageObjectVariant called from collections test shim")
}
func (collShim) CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	panic("CreateAsset called from collections test shim")
}
func (collShim) ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	panic("ListAssets called from collections test shim")
}
func (collShim) GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	panic("GetAsset called from collections test shim")
}
func (collShim) UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	panic("UpdateAsset called from collections test shim")
}
func (collShim) DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	panic("DeleteAsset called from collections test shim")
}
func (collShim) DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	panic("DownloadAssetFile called from collections test shim")
}
func (collShim) DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	panic("DownloadAssetVariant called from collections test shim")
}
func (collShim) AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	panic("AddAssetTags called from collections test shim")
}
func (collShim) RecreateAssetPreview(context.Context, openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	panic("RecreateAssetPreview called from collections test shim")
}
func (collShim) RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	panic("RemoveAssetTag called from collections test shim")
}
func (collShim) ListAssetCompanions(context.Context, openapi.ListAssetCompanionsRequestObject) (openapi.ListAssetCompanionsResponseObject, error) {
	panic("ListAssetCompanions called from collections test shim")
}
func (collShim) AddAssetCompanion(context.Context, openapi.AddAssetCompanionRequestObject) (openapi.AddAssetCompanionResponseObject, error) {
	panic("AddAssetCompanion called from collections test shim")
}
func (collShim) DownloadAssetCompanion(context.Context, openapi.DownloadAssetCompanionRequestObject) (openapi.DownloadAssetCompanionResponseObject, error) {
	panic("DownloadAssetCompanion called from collections test shim")
}
func (collShim) RemoveAssetCompanion(context.Context, openapi.RemoveAssetCompanionRequestObject) (openapi.RemoveAssetCompanionResponseObject, error) {
	panic("RemoveAssetCompanion called from collections test shim")
}
func (collShim) ListAssetAlternates(context.Context, openapi.ListAssetAlternatesRequestObject) (openapi.ListAssetAlternatesResponseObject, error) {
	panic("ListAssetAlternates called from collections test shim")
}
func (collShim) AddAssetAlternate(context.Context, openapi.AddAssetAlternateRequestObject) (openapi.AddAssetAlternateResponseObject, error) {
	panic("AddAssetAlternate called from collections test shim")
}
func (collShim) DownloadAssetAlternate(context.Context, openapi.DownloadAssetAlternateRequestObject) (openapi.DownloadAssetAlternateResponseObject, error) {
	panic("DownloadAssetAlternate called from collections test shim")
}
func (collShim) RemoveAssetAlternate(context.Context, openapi.RemoveAssetAlternateRequestObject) (openapi.RemoveAssetAlternateResponseObject, error) {
	panic("RemoveAssetAlternate called from collections test shim")
}
func (collShim) GetEpubSpine(context.Context, openapi.GetEpubSpineRequestObject) (openapi.GetEpubSpineResponseObject, error) {
	panic("GetEpubSpine called from collections test shim")
}
func (collShim) GetEpubChapter(context.Context, openapi.GetEpubChapterRequestObject) (openapi.GetEpubChapterResponseObject, error) {
	panic("GetEpubChapter called from collections test shim")
}
func (collShim) GetEpubResource(context.Context, openapi.GetEpubResourceRequestObject) (openapi.GetEpubResourceResponseObject, error) {
	panic("GetEpubResource called from collections test shim")
}

func (collShim) SearchEpub(context.Context, openapi.SearchEpubRequestObject) (openapi.SearchEpubResponseObject, error) {
	panic("SearchEpub called from collections test shim")
}
func (collShim) GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from collections test shim")
}
func (collShim) CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from collections test shim")
}
func (collShim) ListWorkflowStates(context.Context, openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from collections test shim")
}
func (collShim) ListFields(context.Context, openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	panic("ListFields called from collections test shim")
}
func (collShim) CreateField(context.Context, openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	panic("CreateField called from collections test shim")
}
func (collShim) GetField(context.Context, openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	panic("GetField called from collections test shim")
}
func (collShim) UpdateField(context.Context, openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	panic("UpdateField called from collections test shim")
}
func (collShim) ArchiveField(context.Context, openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	panic("ArchiveField called from collections test shim")
}
func (collShim) GetAssetFields(context.Context, openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	panic("GetAssetFields called from collections test shim")
}
func (collShim) SetAssetFieldValue(context.Context, openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	panic("SetAssetFieldValue called from collections test shim")
}
func (collShim) ClearAssetFieldValue(context.Context, openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	panic("ClearAssetFieldValue called from collections test shim")
}
func (collShim) GetAssetFieldValueHistory(context.Context, openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	panic("GetAssetFieldValueHistory called from collections test shim")
}
func (collShim) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from collections test shim")
}
func (collShim) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from collections test shim")
}
func (collShim) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from collections test shim")
}
func (collShim) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from collections test shim")
}
func (collShim) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from collections test shim")
}
func (collShim) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from collections test shim")
}
func (collShim) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from collections test shim")
}
func (collShim) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from collections_test test shim")
}
func (collShim) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from collections_test test shim")
}
func (collShim) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from collections_test test shim")
}
func (collShim) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from collections_test test shim")
}
func (collShim) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from collections_test test shim")
}
func (collShim) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from collections_test test shim")
}
func (collShim) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from collections_test test shim")
}
func (collShim) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from collections_test test shim")
}
func (collShim) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from collections_test test shim")
}
func (collShim) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from collections_test test shim")
}
func (collShim) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from collections_test test shim")
}
func (collShim) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from collections_test test shim")
}
func (collShim) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from collections_test test shim")
}
func (collShim) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from collections_test test shim")
}
func (collShim) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from collections_test test shim")
}
func (collShim) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from collections_test test shim")
}
func (collShim) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from collections_test test shim")
}
func (collShim) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from collections_test test shim")
}
func (collShim) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from collections_test test shim")
}
func (collShim) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from collections_test test shim")
}
func (collShim) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from collections_test test shim")
}
func (collShim) ListAdminUsers(context.Context, openapi.ListAdminUsersRequestObject) (openapi.ListAdminUsersResponseObject, error) {
	panic("ListAdminUsers called from collections test shim")
}
func (collShim) SetAdminUserStatus(context.Context, openapi.SetAdminUserStatusRequestObject) (openapi.SetAdminUserStatusResponseObject, error) {
	panic("SetAdminUserStatus called from collections test shim")
}
func (collShim) ListMySessions(context.Context, openapi.ListMySessionsRequestObject) (openapi.ListMySessionsResponseObject, error) {
	panic("ListMySessions called from collections test shim")
}
func (collShim) RevokeMySession(context.Context, openapi.RevokeMySessionRequestObject) (openapi.RevokeMySessionResponseObject, error) {
	panic("RevokeMySession called from collections test shim")
}
func (collShim) ListAdminUserSessions(context.Context, openapi.ListAdminUserSessionsRequestObject) (openapi.ListAdminUserSessionsResponseObject, error) {
	panic("ListAdminUserSessions called from collections test shim")
}
func (collShim) RevokeAdminUserSession(context.Context, openapi.RevokeAdminUserSessionRequestObject) (openapi.RevokeAdminUserSessionResponseObject, error) {
	panic("RevokeAdminUserSession called from collections test shim")
}
func (collShim) ChangeMyPassword(context.Context, openapi.ChangeMyPasswordRequestObject) (openapi.ChangeMyPasswordResponseObject, error) {
	panic("ChangeMyPassword called from collections test shim")
}
func (collShim) AdminResetUserPassword(context.Context, openapi.AdminResetUserPasswordRequestObject) (openapi.AdminResetUserPasswordResponseObject, error) {
	panic("AdminResetUserPassword called from collections test shim")
}
func (collShim) ListAdminUserCapabilities(context.Context, openapi.ListAdminUserCapabilitiesRequestObject) (openapi.ListAdminUserCapabilitiesResponseObject, error) {
	panic("ListAdminUserCapabilities called from collections test shim")
}
func (collShim) AddAdminUserGrant(context.Context, openapi.AddAdminUserGrantRequestObject) (openapi.AddAdminUserGrantResponseObject, error) {
	panic("AddAdminUserGrant called from collections test shim")
}
func (collShim) RemoveAdminUserGrant(context.Context, openapi.RemoveAdminUserGrantRequestObject) (openapi.RemoveAdminUserGrantResponseObject, error) {
	panic("RemoveAdminUserGrant called from collections test shim")
}
func (collShim) AddAdminUserRevoke(context.Context, openapi.AddAdminUserRevokeRequestObject) (openapi.AddAdminUserRevokeResponseObject, error) {
	panic("AddAdminUserRevoke called from collections test shim")
}
func (collShim) RemoveAdminUserRevoke(context.Context, openapi.RemoveAdminUserRevokeRequestObject) (openapi.RemoveAdminUserRevokeResponseObject, error) {
	panic("RemoveAdminUserRevoke called from collections test shim")
}
func (collShim) ListAssetTypeAcls(context.Context, openapi.ListAssetTypeAclsRequestObject) (openapi.ListAssetTypeAclsResponseObject, error) {
	panic("ListAssetTypeAcls called from collections test shim")
}
func (collShim) AddAssetTypeAcl(context.Context, openapi.AddAssetTypeAclRequestObject) (openapi.AddAssetTypeAclResponseObject, error) {
	panic("AddAssetTypeAcl called from collections test shim")
}
func (collShim) RemoveAssetTypeAcl(context.Context, openapi.RemoveAssetTypeAclRequestObject) (openapi.RemoveAssetTypeAclResponseObject, error) {
	panic("RemoveAssetTypeAcl called from collections test shim")
}
func (collShim) ListAdminAuditEvents(context.Context, openapi.ListAdminAuditEventsRequestObject) (openapi.ListAdminAuditEventsResponseObject, error) {
	panic("ListAdminAuditEvents called from collections test shim")
}
func (collShim) ListAdminAuditEventTypes(context.Context, openapi.ListAdminAuditEventTypesRequestObject) (openapi.ListAdminAuditEventTypesResponseObject, error) {
	panic("ListAdminAuditEventTypes called from collections test shim")
}
func (collShim) GetAdminLicenseStatus(context.Context, openapi.GetAdminLicenseStatusRequestObject) (openapi.GetAdminLicenseStatusResponseObject, error) {
	panic("GetAdminLicenseStatus called from collections test shim")
}
func (collShim) ValidateAdminLicense(context.Context, openapi.ValidateAdminLicenseRequestObject) (openapi.ValidateAdminLicenseResponseObject, error) {
	panic("ValidateAdminLicense called from collections test shim")
}
func (collShim) UploadAdminLicense(context.Context, openapi.UploadAdminLicenseRequestObject) (openapi.UploadAdminLicenseResponseObject, error) {
	panic("UploadAdminLicense called from collections test shim")
}
func (collShim) ListIdentityProviders(context.Context, openapi.ListIdentityProvidersRequestObject) (openapi.ListIdentityProvidersResponseObject, error) {
	panic("ListIdentityProviders called from collections test shim")
}
func (collShim) GetAccountPreferences(context.Context, openapi.GetAccountPreferencesRequestObject) (openapi.GetAccountPreferencesResponseObject, error) {
	panic("GetAccountPreferences called from collections test shim")
}
func (collShim) PatchAccountPreferences(context.Context, openapi.PatchAccountPreferencesRequestObject) (openapi.PatchAccountPreferencesResponseObject, error) {
	panic("PatchAccountPreferences called from collections test shim")
}
func (collShim) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from collections_test test shim")
}
func (collShim) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from collections_test test shim")
}
func (collShim) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from collections_test test shim")
}
func (collShim) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from collections_test test shim")
}
func (collShim) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from collections_test test shim")
}
func (collShim) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from collections_test test shim")
}

func (collShim) GetSiteConfig(context.Context, openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	panic("GetSiteConfig called from collections test shim")
}
func (collShim) UpdateSiteConfig(context.Context, openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from collections test shim")
}
func (collShim) GetSMTPConfig(context.Context, openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from collections test shim")
}
func (collShim) UpdateSMTPConfig(context.Context, openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from collections test shim")
}
func (collShim) GetAuthConfig(context.Context, openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from collections test shim")
}
func (collShim) UpdateAuthConfig(context.Context, openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from collections test shim")
}
func (collShim) GetAIConfig(context.Context, openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from collections test shim")
}
func (collShim) UpdateAIConfig(context.Context, openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from collections test shim")
}
func (collShim) ListLocales(context.Context, openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from collections test shim")
}

func (collShim) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from collections test shim")
}
func (collShim) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from collections test shim")
}
func (collShim) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from collections test shim")
}

// --- jobs stubs (Phase 1.18.A) -------------------------------------------
func (collShim) ClaimJobs(context.Context, openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	panic("ClaimJobs called from test shim")
}
func (collShim) GetJob(context.Context, openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	panic("GetJob called from test shim")
}
func (collShim) HeartbeatJob(context.Context, openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	panic("HeartbeatJob called from test shim")
}
func (collShim) CompleteJob(context.Context, openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	panic("CompleteJob called from test shim")
}
func (collShim) FailJob(context.Context, openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	panic("FailJob called from test shim")
}

func (collShim) ListPostWhiteboards(context.Context, openapi.ListPostWhiteboardsRequestObject) (openapi.ListPostWhiteboardsResponseObject, error) {
	panic("ListPostWhiteboards called from collections_test test shim")
}

func (collShim) CreatePostWhiteboard(context.Context, openapi.CreatePostWhiteboardRequestObject) (openapi.CreatePostWhiteboardResponseObject, error) {
	panic("CreatePostWhiteboard called from collections_test test shim")
}

// --- brush packs stubs (Phase 1.21c) -------------------------------------
func (collShim) ListBrushPacks(context.Context, openapi.ListBrushPacksRequestObject) (openapi.ListBrushPacksResponseObject, error) {
	panic("ListBrushPacks called from collShim test shim")
}
func (collShim) ImportBrushPack(context.Context, openapi.ImportBrushPackRequestObject) (openapi.ImportBrushPackResponseObject, error) {
	panic("ImportBrushPack called from collShim test shim")
}
func (collShim) GetBrushPack(context.Context, openapi.GetBrushPackRequestObject) (openapi.GetBrushPackResponseObject, error) {
	panic("GetBrushPack called from collShim test shim")
}
func (collShim) DeleteBrushPack(context.Context, openapi.DeleteBrushPackRequestObject) (openapi.DeleteBrushPackResponseObject, error) {
	panic("DeleteBrushPack called from collShim test shim")
}
func (collShim) GetBrushPackStamp(context.Context, openapi.GetBrushPackStampRequestObject) (openapi.GetBrushPackStampResponseObject, error) {
	panic("GetBrushPackStamp called from collShim test shim")
}
func (collShim)ListAssetTextAnnotations(context.Context, openapi.ListAssetTextAnnotationsRequestObject) (openapi.ListAssetTextAnnotationsResponseObject, error) {
	panic("ListAssetTextAnnotations called from collections_test test shim")
}
func (collShim)CreateAssetTextAnnotation(context.Context, openapi.CreateAssetTextAnnotationRequestObject) (openapi.CreateAssetTextAnnotationResponseObject, error) {
	panic("CreateAssetTextAnnotation called from collections_test test shim")
}
func (collShim)UpdateTextAnnotation(context.Context, openapi.UpdateTextAnnotationRequestObject) (openapi.UpdateTextAnnotationResponseObject, error) {
	panic("UpdateTextAnnotation called from collections_test test shim")
}
func (collShim)LintAsset(context.Context, openapi.LintAssetRequestObject) (openapi.LintAssetResponseObject, error) {
	panic("LintAsset called from collections_test test shim")
}
func (collShim) FollowUser(context.Context, openapi.FollowUserRequestObject) (openapi.FollowUserResponseObject, error) {
	panic("FollowUser called from collections test shim")
}
func (collShim) UnfollowUser(context.Context, openapi.UnfollowUserRequestObject) (openapi.UnfollowUserResponseObject, error) {
	panic("UnfollowUser called from collections test shim")
}
func (collShim) ListUserFollowers(context.Context, openapi.ListUserFollowersRequestObject) (openapi.ListUserFollowersResponseObject, error) {
	panic("ListUserFollowers called from collections test shim")
}
func (collShim) ListUserFollowing(context.Context, openapi.ListUserFollowingRequestObject) (openapi.ListUserFollowingResponseObject, error) {
	panic("ListUserFollowing called from collections test shim")
}
func (collShim) GetUserRelationship(context.Context, openapi.GetUserRelationshipRequestObject) (openapi.GetUserRelationshipResponseObject, error) {
	panic("GetUserRelationship called from collections test shim")
}
func (collShim) BlockUser(context.Context, openapi.BlockUserRequestObject) (openapi.BlockUserResponseObject, error) {
	panic("BlockUser called from collections test shim")
}
func (collShim) UnblockUser(context.Context, openapi.UnblockUserRequestObject) (openapi.UnblockUserResponseObject, error) {
	panic("UnblockUser called from collections test shim")
}
func (collShim) ListMyBlocked(context.Context, openapi.ListMyBlockedRequestObject) (openapi.ListMyBlockedResponseObject, error) {
	panic("ListMyBlocked called from collections test shim")
}
func (collShim) ListMyNotifications(context.Context, openapi.ListMyNotificationsRequestObject) (openapi.ListMyNotificationsResponseObject, error) {
	panic("ListMyNotifications called from collections test shim")
}
func (collShim) GetMyUnreadNotificationCount(context.Context, openapi.GetMyUnreadNotificationCountRequestObject) (openapi.GetMyUnreadNotificationCountResponseObject, error) {
	panic("GetMyUnreadNotificationCount called from collections test shim")
}
func (collShim) MarkNotificationRead(context.Context, openapi.MarkNotificationReadRequestObject) (openapi.MarkNotificationReadResponseObject, error) {
	panic("MarkNotificationRead called from collections test shim")
}
func (collShim) MarkAllMyNotificationsRead(context.Context, openapi.MarkAllMyNotificationsReadRequestObject) (openapi.MarkAllMyNotificationsReadResponseObject, error) {
	panic("MarkAllMyNotificationsRead called from collections test shim")
}
func (collShim) ListMyDirectMessageThreads(context.Context, openapi.ListMyDirectMessageThreadsRequestObject) (openapi.ListMyDirectMessageThreadsResponseObject, error) {
	panic("ListMyDirectMessageThreads called from collections test shim")
}
func (collShim) GetMyUnreadDirectMessageCount(context.Context, openapi.GetMyUnreadDirectMessageCountRequestObject) (openapi.GetMyUnreadDirectMessageCountResponseObject, error) {
	panic("GetMyUnreadDirectMessageCount called from collections test shim")
}
func (collShim) ListDirectMessageThread(context.Context, openapi.ListDirectMessageThreadRequestObject) (openapi.ListDirectMessageThreadResponseObject, error) {
	panic("ListDirectMessageThread called from collections test shim")
}
func (collShim) SendDirectMessage(context.Context, openapi.SendDirectMessageRequestObject) (openapi.SendDirectMessageResponseObject, error) {
	panic("SendDirectMessage called from collections test shim")
}
func (collShim) MarkDirectMessageThreadRead(context.Context, openapi.MarkDirectMessageThreadReadRequestObject) (openapi.MarkDirectMessageThreadReadResponseObject, error) {
	panic("MarkDirectMessageThreadRead called from collections test shim")
}
func (collShim) ListAdminActivities(context.Context, openapi.ListAdminActivitiesRequestObject) (openapi.ListAdminActivitiesResponseObject, error) {
	panic("ListAdminActivities called from collections test shim")
}
func (collShim) ListFederationPeers(context.Context, openapi.ListFederationPeersRequestObject) (openapi.ListFederationPeersResponseObject, error) {
	panic("ListFederationPeers called from collections test shim")
}
func (collShim) GetFederationPeer(context.Context, openapi.GetFederationPeerRequestObject) (openapi.GetFederationPeerResponseObject, error) {
	panic("GetFederationPeer called from collections test shim")
}
func (collShim) CreateFederationPeer(context.Context, openapi.CreateFederationPeerRequestObject) (openapi.CreateFederationPeerResponseObject, error) {
	panic("CreateFederationPeer called from collections test shim")
}
func (collShim) UpdateFederationPeer(context.Context, openapi.UpdateFederationPeerRequestObject) (openapi.UpdateFederationPeerResponseObject, error) {
	panic("UpdateFederationPeer called from collections test shim")
}
func (collShim) DeleteFederationPeer(context.Context, openapi.DeleteFederationPeerRequestObject) (openapi.DeleteFederationPeerResponseObject, error) {
	panic("DeleteFederationPeer called from collections test shim")
}
func (collShim) GetFederationInstance(context.Context, openapi.GetFederationInstanceRequestObject) (openapi.GetFederationInstanceResponseObject, error) {
	panic("GetFederationInstance called from collections test shim")
}
func (collShim) PostFederationHandshake(context.Context, openapi.PostFederationHandshakeRequestObject) (openapi.PostFederationHandshakeResponseObject, error) {
	panic("PostFederationHandshake called from collections test shim")
}
func (collShim) InitiateFederationHandshake(context.Context, openapi.InitiateFederationHandshakeRequestObject) (openapi.InitiateFederationHandshakeResponseObject, error) {
	panic("InitiateFederationHandshake called from collections test shim")
}
func (collShim) ListFederationPendingInbound(context.Context, openapi.ListFederationPendingInboundRequestObject) (openapi.ListFederationPendingInboundResponseObject, error) {
	panic("ListFederationPendingInbound called from collections test shim")
}
func (collShim) AcceptFederationPeer(context.Context, openapi.AcceptFederationPeerRequestObject) (openapi.AcceptFederationPeerResponseObject, error) {
	panic("AcceptFederationPeer called from collections test shim")
}
func (collShim) ListFederationDirectories(context.Context, openapi.ListFederationDirectoriesRequestObject) (openapi.ListFederationDirectoriesResponseObject, error) {
	panic("ListFederationDirectories called from collections test shim")
}
func (collShim) SubscribeFederationDirectory(context.Context, openapi.SubscribeFederationDirectoryRequestObject) (openapi.SubscribeFederationDirectoryResponseObject, error) {
	panic("SubscribeFederationDirectory called from collections test shim")
}
func (collShim) UnsubscribeFederationDirectory(context.Context, openapi.UnsubscribeFederationDirectoryRequestObject) (openapi.UnsubscribeFederationDirectoryResponseObject, error) {
	panic("UnsubscribeFederationDirectory called from collections test shim")
}
func (collShim) PollFederationDirectory(context.Context, openapi.PollFederationDirectoryRequestObject) (openapi.PollFederationDirectoryResponseObject, error) {
	panic("PollFederationDirectory called from collections test shim")
}
func (collShim) ListFederationDirectoryEntries(context.Context, openapi.ListFederationDirectoryEntriesRequestObject) (openapi.ListFederationDirectoryEntriesResponseObject, error) {
	panic("ListFederationDirectoryEntries called from collections test shim")
}
func (collShim) RequestFederationDirectoryPublishChallenge(context.Context, openapi.RequestFederationDirectoryPublishChallengeRequestObject) (openapi.RequestFederationDirectoryPublishChallengeResponseObject, error) {
	panic("RequestFederationDirectoryPublishChallenge called from collections test shim")
}
func (collShim) RegisterFederationDirectoryPublishListing(context.Context, openapi.RegisterFederationDirectoryPublishListingRequestObject) (openapi.RegisterFederationDirectoryPublishListingResponseObject, error) {
	panic("RegisterFederationDirectoryPublishListing called from collections test shim")
}
func (collShim) GetFederationPeersVisible(context.Context, openapi.GetFederationPeersVisibleRequestObject) (openapi.GetFederationPeersVisibleResponseObject, error) {
	panic("GetFederationPeersVisible called from collections test shim")
}
func (collShim) ListFederationPeerSuggestions(context.Context, openapi.ListFederationPeerSuggestionsRequestObject) (openapi.ListFederationPeerSuggestionsResponseObject, error) {
	panic("ListFederationPeerSuggestions called from collections test shim")
}
func (collShim) RefreshFederationPeerSuggestions(context.Context, openapi.RefreshFederationPeerSuggestionsRequestObject) (openapi.RefreshFederationPeerSuggestionsResponseObject, error) {
	panic("RefreshFederationPeerSuggestions called from collections test shim")
}
func (collShim) ListFederationShares(context.Context, openapi.ListFederationSharesRequestObject) (openapi.ListFederationSharesResponseObject, error) {
	panic("ListFederationShares called from collections test shim")
}
func (collShim) GrantFederationShare(context.Context, openapi.GrantFederationShareRequestObject) (openapi.GrantFederationShareResponseObject, error) {
	panic("GrantFederationShare called from collections test shim")
}
func (collShim) RevokeFederationShare(context.Context, openapi.RevokeFederationShareRequestObject) (openapi.RevokeFederationShareResponseObject, error) {
	panic("RevokeFederationShare called from collections test shim")
}
func (collShim) PreviewFederationPeerDefederation(context.Context, openapi.PreviewFederationPeerDefederationRequestObject) (openapi.PreviewFederationPeerDefederationResponseObject, error) {
	panic("PreviewFederationPeerDefederation called from collections test shim")
}
