package assets_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// TestAssetLifecycle_HappyPath exercises the whole asset entity flow:
// upload bytes -> create asset -> get -> update -> tag ops -> file
// download -> list -> delete. Pin transitions are verified directly
// against storage_pins to make sure the asset really takes ownership
// of the bytes from the uploading user.
func TestAssetLifecycle_HappyPath(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()

	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	svc.GCGracePeriod = time.Second

	const userRef int64 = 424343
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	storageH := storage.NewHandler(svc, logger)
	assetsH := assets.NewHandler(pool, svc, logger, nil, nil)

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session"}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{storage: storageH, assets: assetsH}, nil), router)

	// Random payload so reruns don't dedup against earlier state.
	payload := make([]byte, 8*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(want[:])

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_tag WHERE asset_id IN (SELECT id FROM assets WHERE owner_user_ref=$1)`, userRef)
		_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE pin_subject_type='asset' AND pin_subject_id IN (SELECT id::text FROM assets WHERE owner_user_ref=$1)`, userRef)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE owner_user_ref=$1`, userRef)
		_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE pin_subject_type='user' AND pin_subject_id=$1`, "424343")
		_, _ = pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash=$1`, hashHex)
		_, _ = pool.Exec(c, `DELETE FROM storage_objects WHERE hash=$1`, hashHex)
	})

	// --- 1. upload raw bytes ---
	upReq := httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader(payload))
	upReq.Header.Set("X-Content-Type", "image/png")
	upRR := httptest.NewRecorder()
	router.ServeHTTP(upRR, upReq)
	if upRR.Code != http.StatusCreated {
		t.Fatalf("upload bytes: %d body=%s", upRR.Code, upRR.Body.String())
	}
	var upResult openapi.UploadResult
	mustDecode(t, upRR.Body.Bytes(), &upResult)
	if upResult.Hash != hashHex {
		t.Fatalf("upload hash mismatch: got %s want %s", upResult.Hash, hashHex)
	}

	// Sanity: the user pin should exist now.
	if pinCount(t, pool, "user", "424343", hashHex) != 1 {
		t.Errorf("expected user pin after upload")
	}

	// --- 2. create asset linked to the uploaded hash ---
	createBody := map[string]any{
		"title":         "Test Asset",
		"description":   "Round-trip integration test",
		"asset_type": int64(1),
		"file_hash":     hashHex,
		"tags":          []string{"smoke", "test"},
		"metadata":      map[string]any{"width": 4096, "format": "png"},
	}
	createRR := postJSON(t, router, "/assets", createBody)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("create asset: %d body=%s", createRR.Code, createRR.Body.String())
	}
	var created openapi.Asset
	mustDecode(t, createRR.Body.Bytes(), &created)
	if created.Title != "Test Asset" {
		t.Errorf("title=%q want Test Asset", created.Title)
	}
	if created.FileHash == nil || *created.FileHash != hashHex {
		t.Errorf("file_hash not propagated: %+v", created.FileHash)
	}
	if len(created.Tags) != 2 {
		t.Errorf("tags=%v want 2 entries", created.Tags)
	}
	assetID := created.Id.String()

	// Pin transition: asset pin added, user pin removed.
	if pinCount(t, pool, "asset", assetID, hashHex) != 1 {
		t.Errorf("expected asset pin after create")
	}
	if pinCount(t, pool, "user", "424343", hashHex) != 0 {
		t.Errorf("expected user pin removed after create")
	}

	// --- 3. fetch ---
	getRR := httptest.NewRecorder()
	router.ServeHTTP(getRR, httptest.NewRequest(http.MethodGet, "/assets/"+assetID, nil))
	if getRR.Code != http.StatusOK {
		t.Fatalf("get asset: %d body=%s", getRR.Code, getRR.Body.String())
	}
	var fetched openapi.Asset
	mustDecode(t, getRR.Body.Bytes(), &fetched)
	if fetched.Id != created.Id {
		t.Errorf("get returned wrong asset")
	}

	// --- 4. PATCH title + replace tags ---
	patchRR := patchJSON(t, router, "/assets/"+assetID, map[string]any{
		"title": "Updated Title",
		"tags":  []string{"only", "two"},
	})
	if patchRR.Code != http.StatusOK {
		t.Fatalf("patch asset: %d body=%s", patchRR.Code, patchRR.Body.String())
	}
	var patched openapi.Asset
	mustDecode(t, patchRR.Body.Bytes(), &patched)
	if patched.Title != "Updated Title" {
		t.Errorf("title not updated: %q", patched.Title)
	}
	if len(patched.Tags) != 2 || patched.Tags[0] == "smoke" {
		t.Errorf("tags not replaced: %v", patched.Tags)
	}

	// --- 5. add tags (additive) ---
	addRR := postJSON(t, router, "/assets/"+assetID+"/tags", map[string]any{
		"tags": []string{"extra", "two"}, // "two" already present, should be idempotent
	})
	if addRR.Code != http.StatusNoContent {
		t.Fatalf("add tags: %d body=%s", addRR.Code, addRR.Body.String())
	}
	getRR2 := httptest.NewRecorder()
	router.ServeHTTP(getRR2, httptest.NewRequest(http.MethodGet, "/assets/"+assetID, nil))
	var afterAdd openapi.Asset
	mustDecode(t, getRR2.Body.Bytes(), &afterAdd)
	if len(afterAdd.Tags) != 3 {
		t.Errorf("after add tags=%v want 3", afterAdd.Tags)
	}

	// --- 6. remove tag ---
	delTagRR := httptest.NewRecorder()
	router.ServeHTTP(delTagRR, httptest.NewRequest(http.MethodDelete, "/assets/"+assetID+"/tags/extra", nil))
	if delTagRR.Code != http.StatusNoContent {
		t.Fatalf("remove tag: %d", delTagRR.Code)
	}

	// --- 7. download file (proxy to storage) ---
	fileRR := httptest.NewRecorder()
	router.ServeHTTP(fileRR, httptest.NewRequest(http.MethodGet, "/assets/"+assetID+"/file", nil))
	if fileRR.Code != http.StatusOK {
		t.Fatalf("download file: %d body=%s", fileRR.Code, fileRR.Body.String())
	}
	if !bytes.Equal(fileRR.Body.Bytes(), payload) {
		t.Errorf("downloaded body mismatch (%d vs %d bytes)", fileRR.Body.Len(), len(payload))
	}

	// --- 8. variant 404 (no variant generator yet) ---
	varRR := httptest.NewRecorder()
	router.ServeHTTP(varRR, httptest.NewRequest(http.MethodGet, "/assets/"+assetID+"/variants/preview_2048", nil))
	if varRR.Code != http.StatusNotFound {
		t.Errorf("variant: status=%d want 404", varRR.Code)
	}

	// --- 9. list ---
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, httptest.NewRequest(http.MethodGet, "/assets?owner_ref=424343&limit=50", nil))
	if listRR.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", listRR.Code, listRR.Body.String())
	}
	var page openapi.AssetList
	mustDecode(t, listRR.Body.Bytes(), &page)
	found := false
	for _, a := range page.Items {
		if a.Id.String() == assetID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("created asset not in list: items=%d", len(page.Items))
	}

	// --- 10. tag-filtered list ---
	tagListRR := httptest.NewRecorder()
	router.ServeHTTP(tagListRR, httptest.NewRequest(http.MethodGet, "/assets?owner_ref=424343&tag=only", nil))
	if tagListRR.Code != http.StatusOK {
		t.Fatalf("tag-filter list: %d body=%s", tagListRR.Code, tagListRR.Body.String())
	}
	var tagPage openapi.AssetList
	mustDecode(t, tagListRR.Body.Bytes(), &tagPage)
	if len(tagPage.Items) != 1 {
		t.Errorf("tag-filtered list len=%d want 1", len(tagPage.Items))
	}

	// --- 11. soft-delete ---
	delRR := httptest.NewRecorder()
	router.ServeHTTP(delRR, httptest.NewRequest(http.MethodDelete, "/assets/"+assetID, nil))
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", delRR.Code)
	}
	get404 := httptest.NewRecorder()
	router.ServeHTTP(get404, httptest.NewRequest(http.MethodGet, "/assets/"+assetID, nil))
	if get404.Code != http.StatusNotFound {
		t.Errorf("after delete: status=%d want 404", get404.Code)
	}
	// Asset pin gone.
	if pinCount(t, pool, "asset", assetID, hashHex) != 0 {
		t.Errorf("asset pin should be gone after delete")
	}

	// --- 12. anonymous create -> 401 ---
	bareRouter := chi.NewRouter()
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{storage: storageH, assets: assetsH}, nil), bareRouter)
	anonRR := postJSON(t, bareRouter, "/assets", map[string]any{
		"title": "Anon", "asset_type": int64(1),
	})
	if anonRR.Code != http.StatusUnauthorized {
		t.Errorf("anonymous create: status=%d want 401", anonRR.Code)
	}

	_ = ctx
}

// TestCreateAssetWithoutFile creates a draft-shaped asset (no
// file_hash) and confirms the storage layer is not touched.
func TestCreateAssetWithoutFile(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()

	const userRef int64 = 424344
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	storageH := storage.NewHandler(svc, logger)
	assetsH := assets.NewHandler(pool, svc, logger, nil, nil)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session"}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{storage: storageH, assets: assetsH}, nil), router)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE owner_user_ref=$1`, userRef)
	})

	rr := postJSON(t, router, "/assets", map[string]any{
		"title":         "Draft without file",
		"asset_type": int64(1),
		"status":        "draft",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rr.Code, rr.Body.String())
	}
	var a openapi.Asset
	mustDecode(t, rr.Body.Bytes(), &a)
	if a.FileHash != nil {
		t.Errorf("file_hash should be nil: %v", a.FileHash)
	}
	if a.Status != "draft" {
		t.Errorf("status=%q want draft", a.Status)
	}

	// file download -> 404 since no hash
	fileRR := httptest.NewRecorder()
	router.ServeHTTP(fileRR, httptest.NewRequest(http.MethodGet, "/assets/"+a.Id.String()+"/file", nil))
	if fileRR.Code != http.StatusNotFound {
		t.Errorf("file download with no hash: status=%d want 404", fileRR.Code)
	}
}

// TestCreateAssetInputValidation covers the 400 paths.
func TestCreateAssetInputValidation(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()
	backend, _ := storagefs.New(t.TempDir())
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	storageH := storage.NewHandler(svc, logger)
	assetsH := assets.NewHandler(pool, svc, logger, nil, nil)
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: 424345, AuthMethod: "session"}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{storage: storageH, assets: assetsH}, nil), router)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "empty title",
			body: map[string]any{"title": "  ", "asset_type": int64(1)},
			want: "title",
		},
		{
			name: "bad status",
			body: map[string]any{"title": "x", "asset_type": int64(1), "status": "weird"},
			want: "status",
		},
		{
			name: "bad file_hash",
			body: map[string]any{"title": "x", "asset_type": int64(1), "file_hash": "not-a-sha"},
			want: "file_hash",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := postJSON(t, router, "/assets", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: status=%d body=%s", tc.name, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.want) {
				t.Errorf("error body %q lacks %q", rr.Body.String(), tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

func mustDecode(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, data)
	}
}

func pinCount(t *testing.T, pool *pgxpool.Pool, subjType, subjID, hash string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM storage_pins WHERE pin_subject_type=$1 AND pin_subject_id=$2 AND object_hash=$3`,
		subjType, subjID, hash).Scan(&n); err != nil {
		t.Fatalf("pin count: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// shim
// ---------------------------------------------------------------------------

type shimImpl struct {
	storage *storage.Handler
	assets  *assets.Handler
}

func (s shimImpl) UploadStorageObject(ctx context.Context, req openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	return s.storage.UploadStorageObject(ctx, req)
}
func (s shimImpl) DownloadStorageObject(ctx context.Context, req openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	return s.storage.DownloadStorageObject(ctx, req)
}
func (s shimImpl) DownloadStorageObjectVariant(ctx context.Context, req openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	return s.storage.DownloadStorageObjectVariant(ctx, req)
}
func (s shimImpl) CreateAsset(ctx context.Context, req openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	return s.assets.CreateAsset(ctx, req)
}
func (s shimImpl) ListAssets(ctx context.Context, req openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	return s.assets.ListAssets(ctx, req)
}
func (s shimImpl) GetAsset(ctx context.Context, req openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	return s.assets.GetAsset(ctx, req)
}
func (s shimImpl) UpdateAsset(ctx context.Context, req openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	return s.assets.UpdateAsset(ctx, req)
}
func (s shimImpl) DeleteAsset(ctx context.Context, req openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	return s.assets.DeleteAsset(ctx, req)
}
func (s shimImpl) DownloadAssetFile(ctx context.Context, req openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	return s.assets.DownloadAssetFile(ctx, req)
}
func (s shimImpl) DownloadAssetVariant(ctx context.Context, req openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	return s.assets.DownloadAssetVariant(ctx, req)
}
func (s shimImpl) AddAssetTags(ctx context.Context, req openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	return s.assets.AddAssetTags(ctx, req)
}
func (s shimImpl) RecreateAssetPreview(ctx context.Context, req openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	panic("RecreateAssetPreview called from assets test shim")
}
func (s shimImpl) RemoveAssetTag(ctx context.Context, req openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	return s.assets.RemoveAssetTag(ctx, req)
}
func (s shimImpl) ListAssetCompanions(ctx context.Context, req openapi.ListAssetCompanionsRequestObject) (openapi.ListAssetCompanionsResponseObject, error) {
	return s.assets.ListAssetCompanions(ctx, req)
}
func (s shimImpl) AddAssetCompanion(ctx context.Context, req openapi.AddAssetCompanionRequestObject) (openapi.AddAssetCompanionResponseObject, error) {
	return s.assets.AddAssetCompanion(ctx, req)
}
func (s shimImpl) DownloadAssetCompanion(ctx context.Context, req openapi.DownloadAssetCompanionRequestObject) (openapi.DownloadAssetCompanionResponseObject, error) {
	return s.assets.DownloadAssetCompanion(ctx, req)
}
func (s shimImpl) RemoveAssetCompanion(ctx context.Context, req openapi.RemoveAssetCompanionRequestObject) (openapi.RemoveAssetCompanionResponseObject, error) {
	return s.assets.RemoveAssetCompanion(ctx, req)
}
func (s shimImpl) ListAssetAlternates(ctx context.Context, req openapi.ListAssetAlternatesRequestObject) (openapi.ListAssetAlternatesResponseObject, error) {
	return s.assets.ListAssetAlternates(ctx, req)
}
func (s shimImpl) AddAssetAlternate(ctx context.Context, req openapi.AddAssetAlternateRequestObject) (openapi.AddAssetAlternateResponseObject, error) {
	return s.assets.AddAssetAlternate(ctx, req)
}
func (s shimImpl) DownloadAssetAlternate(ctx context.Context, req openapi.DownloadAssetAlternateRequestObject) (openapi.DownloadAssetAlternateResponseObject, error) {
	return s.assets.DownloadAssetAlternate(ctx, req)
}
func (s shimImpl) RemoveAssetAlternate(ctx context.Context, req openapi.RemoveAssetAlternateRequestObject) (openapi.RemoveAssetAlternateResponseObject, error) {
	return s.assets.RemoveAssetAlternate(ctx, req)
}
func (s shimImpl) GetEpubSpine(ctx context.Context, req openapi.GetEpubSpineRequestObject) (openapi.GetEpubSpineResponseObject, error) {
	return s.assets.GetEpubSpine(ctx, req)
}
func (s shimImpl) GetEpubChapter(ctx context.Context, req openapi.GetEpubChapterRequestObject) (openapi.GetEpubChapterResponseObject, error) {
	return s.assets.GetEpubChapter(ctx, req)
}
func (s shimImpl) GetEpubResource(ctx context.Context, req openapi.GetEpubResourceRequestObject) (openapi.GetEpubResourceResponseObject, error) {
	return s.assets.GetEpubResource(ctx, req)
}
func (s shimImpl) SearchEpub(ctx context.Context, req openapi.SearchEpubRequestObject) (openapi.SearchEpubResponseObject, error) {
	return s.assets.SearchEpub(ctx, req)
}

func (shimImpl) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from assets test shim")
}
func (shimImpl) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from assets test shim")
}
func (shimImpl) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from assets test shim")
}
func (shimImpl) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from assets test shim")
}
func (shimImpl) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from assets test shim")
}
func (shimImpl) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from assets test shim")
}
func (shimImpl) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from assets test shim")
}
func (shimImpl) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from assets test shim")
}
func (shimImpl) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from assets test shim")
}
func (shimImpl) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from assets test shim")
}
func (shimImpl) ListAssetTypes(context.Context, openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	panic("ListAssetTypes called from assets test shim")
}
func (shimImpl) GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from assets test shim")
}
func (shimImpl) CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from assets test shim")
}
func (shimImpl) ListWorkflowStates(context.Context, openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from assets test shim")
}
func (shimImpl) ListFields(context.Context, openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	panic("ListFields called from assets test shim")
}
func (shimImpl) CreateField(context.Context, openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	panic("CreateField called from assets test shim")
}
func (shimImpl) GetField(context.Context, openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	panic("GetField called from assets test shim")
}
func (shimImpl) UpdateField(context.Context, openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	panic("UpdateField called from assets test shim")
}
func (shimImpl) ArchiveField(context.Context, openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	panic("ArchiveField called from assets test shim")
}
func (shimImpl) GetAssetFields(context.Context, openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	panic("GetAssetFields called from assets test shim")
}
func (shimImpl) SetAssetFieldValue(context.Context, openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	panic("SetAssetFieldValue called from assets test shim")
}
func (shimImpl) ClearAssetFieldValue(context.Context, openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	panic("ClearAssetFieldValue called from assets test shim")
}
func (shimImpl) GetAssetFieldValueHistory(context.Context, openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	panic("GetAssetFieldValueHistory called from assets test shim")
}
func (shimImpl) ListCollections(context.Context, openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	panic("ListCollections called from assets test shim")
}
func (shimImpl) CreateCollection(context.Context, openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	panic("CreateCollection called from assets test shim")
}
func (shimImpl) GetCollection(context.Context, openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	panic("GetCollection called from assets test shim")
}
func (shimImpl) UpdateCollection(context.Context, openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	panic("UpdateCollection called from assets test shim")
}
func (shimImpl) DeleteCollection(context.Context, openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	panic("DeleteCollection called from assets test shim")
}
func (shimImpl) ListCollectionResources(context.Context, openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	panic("ListCollectionResources called from assets test shim")
}
func (shimImpl) AddCollectionResource(context.Context, openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	panic("AddCollectionResource called from assets test shim")
}
func (shimImpl) RemoveCollectionResource(context.Context, openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	panic("RemoveCollectionResource called from assets test shim")
}
func (shimImpl) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from assets test shim")
}
func (shimImpl) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from assets test shim")
}
func (shimImpl) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from assets test shim")
}
func (shimImpl) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from assets test shim")
}
func (shimImpl) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from assets test shim")
}
func (shimImpl) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from assets test shim")
}
func (shimImpl) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from assets test shim")
}
func (shimImpl) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from assets_test test shim")
}
func (shimImpl) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from assets_test test shim")
}
func (shimImpl) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from assets_test test shim")
}
func (shimImpl) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from assets_test test shim")
}
func (shimImpl) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from assets_test test shim")
}
func (shimImpl) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from assets_test test shim")
}
func (shimImpl) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from assets_test test shim")
}
func (shimImpl) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from assets_test test shim")
}
func (shimImpl) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from assets_test test shim")
}
func (shimImpl) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from assets_test test shim")
}
func (shimImpl) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from assets_test test shim")
}
func (shimImpl) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from assets_test test shim")
}
func (shimImpl) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from assets_test test shim")
}
func (shimImpl) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from assets_test test shim")
}
func (shimImpl) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from assets_test test shim")
}
func (shimImpl) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from assets_test test shim")
}
func (shimImpl) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from assets_test test shim")
}
func (shimImpl) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from assets_test test shim")
}
func (shimImpl) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from assets_test test shim")
}
func (shimImpl) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from assets_test test shim")
}
func (shimImpl) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from assets_test test shim")
}
func (shimImpl) ListAdminUsers(context.Context, openapi.ListAdminUsersRequestObject) (openapi.ListAdminUsersResponseObject, error) {
	panic("ListAdminUsers called from assets test shim")
}
func (shimImpl) SetAdminUserStatus(context.Context, openapi.SetAdminUserStatusRequestObject) (openapi.SetAdminUserStatusResponseObject, error) {
	panic("SetAdminUserStatus called from assets test shim")
}
func (shimImpl) ListMySessions(context.Context, openapi.ListMySessionsRequestObject) (openapi.ListMySessionsResponseObject, error) {
	panic("ListMySessions called from assets test shim")
}
func (shimImpl) RevokeMySession(context.Context, openapi.RevokeMySessionRequestObject) (openapi.RevokeMySessionResponseObject, error) {
	panic("RevokeMySession called from assets test shim")
}
func (shimImpl) ListAdminUserSessions(context.Context, openapi.ListAdminUserSessionsRequestObject) (openapi.ListAdminUserSessionsResponseObject, error) {
	panic("ListAdminUserSessions called from assets test shim")
}
func (shimImpl) RevokeAdminUserSession(context.Context, openapi.RevokeAdminUserSessionRequestObject) (openapi.RevokeAdminUserSessionResponseObject, error) {
	panic("RevokeAdminUserSession called from assets test shim")
}
func (shimImpl) ChangeMyPassword(context.Context, openapi.ChangeMyPasswordRequestObject) (openapi.ChangeMyPasswordResponseObject, error) {
	panic("ChangeMyPassword called from assets test shim")
}
func (shimImpl) AdminResetUserPassword(context.Context, openapi.AdminResetUserPasswordRequestObject) (openapi.AdminResetUserPasswordResponseObject, error) {
	panic("AdminResetUserPassword called from assets test shim")
}
func (shimImpl) ListAdminUserCapabilities(context.Context, openapi.ListAdminUserCapabilitiesRequestObject) (openapi.ListAdminUserCapabilitiesResponseObject, error) {
	panic("ListAdminUserCapabilities called from assets test shim")
}
func (shimImpl) AddAdminUserGrant(context.Context, openapi.AddAdminUserGrantRequestObject) (openapi.AddAdminUserGrantResponseObject, error) {
	panic("AddAdminUserGrant called from assets test shim")
}
func (shimImpl) RemoveAdminUserGrant(context.Context, openapi.RemoveAdminUserGrantRequestObject) (openapi.RemoveAdminUserGrantResponseObject, error) {
	panic("RemoveAdminUserGrant called from assets test shim")
}
func (shimImpl) AddAdminUserRevoke(context.Context, openapi.AddAdminUserRevokeRequestObject) (openapi.AddAdminUserRevokeResponseObject, error) {
	panic("AddAdminUserRevoke called from assets test shim")
}
func (shimImpl) RemoveAdminUserRevoke(context.Context, openapi.RemoveAdminUserRevokeRequestObject) (openapi.RemoveAdminUserRevokeResponseObject, error) {
	panic("RemoveAdminUserRevoke called from assets test shim")
}
func (shimImpl) ListAssetTypeAcls(context.Context, openapi.ListAssetTypeAclsRequestObject) (openapi.ListAssetTypeAclsResponseObject, error) {
	panic("ListAssetTypeAcls called from assets test shim")
}
func (shimImpl) AddAssetTypeAcl(context.Context, openapi.AddAssetTypeAclRequestObject) (openapi.AddAssetTypeAclResponseObject, error) {
	panic("AddAssetTypeAcl called from assets test shim")
}
func (shimImpl) RemoveAssetTypeAcl(context.Context, openapi.RemoveAssetTypeAclRequestObject) (openapi.RemoveAssetTypeAclResponseObject, error) {
	panic("RemoveAssetTypeAcl called from assets test shim")
}
func (shimImpl) ListAdminAuditEvents(context.Context, openapi.ListAdminAuditEventsRequestObject) (openapi.ListAdminAuditEventsResponseObject, error) {
	panic("ListAdminAuditEvents called from assets test shim")
}
func (shimImpl) ListAdminAuditEventTypes(context.Context, openapi.ListAdminAuditEventTypesRequestObject) (openapi.ListAdminAuditEventTypesResponseObject, error) {
	panic("ListAdminAuditEventTypes called from assets test shim")
}
func (shimImpl) GetAdminLicenseStatus(context.Context, openapi.GetAdminLicenseStatusRequestObject) (openapi.GetAdminLicenseStatusResponseObject, error) {
	panic("GetAdminLicenseStatus called from assets test shim")
}
func (shimImpl) ValidateAdminLicense(context.Context, openapi.ValidateAdminLicenseRequestObject) (openapi.ValidateAdminLicenseResponseObject, error) {
	panic("ValidateAdminLicense called from assets test shim")
}
func (shimImpl) UploadAdminLicense(context.Context, openapi.UploadAdminLicenseRequestObject) (openapi.UploadAdminLicenseResponseObject, error) {
	panic("UploadAdminLicense called from assets test shim")
}
func (shimImpl) ListIdentityProviders(context.Context, openapi.ListIdentityProvidersRequestObject) (openapi.ListIdentityProvidersResponseObject, error) {
	panic("ListIdentityProviders called from assets test shim")
}
func (shimImpl) GetAccountPreferences(context.Context, openapi.GetAccountPreferencesRequestObject) (openapi.GetAccountPreferencesResponseObject, error) {
	panic("GetAccountPreferences called from assets test shim")
}
func (shimImpl) PatchAccountPreferences(context.Context, openapi.PatchAccountPreferencesRequestObject) (openapi.PatchAccountPreferencesResponseObject, error) {
	panic("PatchAccountPreferences called from assets test shim")
}
func (shimImpl) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from assets_test test shim")
}
func (shimImpl) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from assets_test test shim")
}
func (shimImpl) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from assets_test test shim")
}
func (shimImpl) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from assets_test test shim")
}
func (shimImpl) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from assets_test test shim")
}
func (shimImpl) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from assets_test test shim")
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

func (shimImpl) GetSiteConfig(context.Context, openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	panic("GetSiteConfig called from assets test shim")
}
func (shimImpl) UpdateSiteConfig(context.Context, openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from assets test shim")
}
func (shimImpl) GetSMTPConfig(context.Context, openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from assets test shim")
}
func (shimImpl) UpdateSMTPConfig(context.Context, openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from assets test shim")
}
func (shimImpl) GetAuthConfig(context.Context, openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from assets test shim")
}
func (shimImpl) UpdateAuthConfig(context.Context, openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from assets test shim")
}
func (shimImpl) GetAIConfig(context.Context, openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from assets test shim")
}
func (shimImpl) UpdateAIConfig(context.Context, openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from assets test shim")
}
func (shimImpl) ListLocales(context.Context, openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from assets test shim")
}

func (shimImpl) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from assets test shim")
}
func (shimImpl) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from assets test shim")
}
func (shimImpl) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from assets test shim")
}

// --- jobs stubs (Phase 1.18.A) -------------------------------------------
func (shimImpl) ClaimJobs(context.Context, openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	panic("ClaimJobs called from test shim")
}
func (shimImpl) GetJob(context.Context, openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	panic("GetJob called from test shim")
}
func (shimImpl) HeartbeatJob(context.Context, openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	panic("HeartbeatJob called from test shim")
}
func (shimImpl) CompleteJob(context.Context, openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	panic("CompleteJob called from test shim")
}
func (shimImpl) FailJob(context.Context, openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	panic("FailJob called from test shim")
}

func (shimImpl) ListPostWhiteboards(context.Context, openapi.ListPostWhiteboardsRequestObject) (openapi.ListPostWhiteboardsResponseObject, error) {
	panic("ListPostWhiteboards called from assets_test test shim")
}

func (shimImpl) CreatePostWhiteboard(context.Context, openapi.CreatePostWhiteboardRequestObject) (openapi.CreatePostWhiteboardResponseObject, error) {
	panic("CreatePostWhiteboard called from assets_test test shim")
}

// --- brush packs stubs (Phase 1.21c) -------------------------------------
func (shimImpl) ListBrushPacks(context.Context, openapi.ListBrushPacksRequestObject) (openapi.ListBrushPacksResponseObject, error) {
	panic("ListBrushPacks called from shimImpl test shim")
}
func (shimImpl) ImportBrushPack(context.Context, openapi.ImportBrushPackRequestObject) (openapi.ImportBrushPackResponseObject, error) {
	panic("ImportBrushPack called from shimImpl test shim")
}
func (shimImpl) GetBrushPack(context.Context, openapi.GetBrushPackRequestObject) (openapi.GetBrushPackResponseObject, error) {
	panic("GetBrushPack called from shimImpl test shim")
}
func (shimImpl) DeleteBrushPack(context.Context, openapi.DeleteBrushPackRequestObject) (openapi.DeleteBrushPackResponseObject, error) {
	panic("DeleteBrushPack called from shimImpl test shim")
}
func (shimImpl) GetBrushPackStamp(context.Context, openapi.GetBrushPackStampRequestObject) (openapi.GetBrushPackStampResponseObject, error) {
	panic("GetBrushPackStamp called from shimImpl test shim")
}
func (shimImpl) ListAssetTextAnnotations(context.Context, openapi.ListAssetTextAnnotationsRequestObject) (openapi.ListAssetTextAnnotationsResponseObject, error) {
	panic("ListAssetTextAnnotations called from assets_test test shim")
}
func (shimImpl) CreateAssetTextAnnotation(context.Context, openapi.CreateAssetTextAnnotationRequestObject) (openapi.CreateAssetTextAnnotationResponseObject, error) {
	panic("CreateAssetTextAnnotation called from assets_test test shim")
}
func (shimImpl) UpdateTextAnnotation(context.Context, openapi.UpdateTextAnnotationRequestObject) (openapi.UpdateTextAnnotationResponseObject, error) {
	panic("UpdateTextAnnotation called from assets_test test shim")
}
func (s shimImpl) LintAsset(ctx context.Context, req openapi.LintAssetRequestObject) (openapi.LintAssetResponseObject, error) {
	return s.assets.LintAsset(ctx, req)
}
func (shimImpl) FollowUser(context.Context, openapi.FollowUserRequestObject) (openapi.FollowUserResponseObject, error) {
	panic("FollowUser called from assets test shim")
}
func (shimImpl) UnfollowUser(context.Context, openapi.UnfollowUserRequestObject) (openapi.UnfollowUserResponseObject, error) {
	panic("UnfollowUser called from assets test shim")
}
func (shimImpl) ListUserFollowers(context.Context, openapi.ListUserFollowersRequestObject) (openapi.ListUserFollowersResponseObject, error) {
	panic("ListUserFollowers called from assets test shim")
}
func (shimImpl) ListUserFollowing(context.Context, openapi.ListUserFollowingRequestObject) (openapi.ListUserFollowingResponseObject, error) {
	panic("ListUserFollowing called from assets test shim")
}
func (shimImpl) GetUserRelationship(context.Context, openapi.GetUserRelationshipRequestObject) (openapi.GetUserRelationshipResponseObject, error) {
	panic("GetUserRelationship called from assets test shim")
}
func (shimImpl) BlockUser(context.Context, openapi.BlockUserRequestObject) (openapi.BlockUserResponseObject, error) {
	panic("BlockUser called from assets test shim")
}
func (shimImpl) UnblockUser(context.Context, openapi.UnblockUserRequestObject) (openapi.UnblockUserResponseObject, error) {
	panic("UnblockUser called from assets test shim")
}
func (shimImpl) ListMyBlocked(context.Context, openapi.ListMyBlockedRequestObject) (openapi.ListMyBlockedResponseObject, error) {
	panic("ListMyBlocked called from assets test shim")
}
func (shimImpl) ListMyNotifications(context.Context, openapi.ListMyNotificationsRequestObject) (openapi.ListMyNotificationsResponseObject, error) {
	panic("ListMyNotifications called from assets test shim")
}
func (shimImpl) GetMyUnreadNotificationCount(context.Context, openapi.GetMyUnreadNotificationCountRequestObject) (openapi.GetMyUnreadNotificationCountResponseObject, error) {
	panic("GetMyUnreadNotificationCount called from assets test shim")
}
func (shimImpl) MarkNotificationRead(context.Context, openapi.MarkNotificationReadRequestObject) (openapi.MarkNotificationReadResponseObject, error) {
	panic("MarkNotificationRead called from assets test shim")
}
func (shimImpl) MarkAllMyNotificationsRead(context.Context, openapi.MarkAllMyNotificationsReadRequestObject) (openapi.MarkAllMyNotificationsReadResponseObject, error) {
	panic("MarkAllMyNotificationsRead called from assets test shim")
}
func (shimImpl) ListMyDirectMessageThreads(context.Context, openapi.ListMyDirectMessageThreadsRequestObject) (openapi.ListMyDirectMessageThreadsResponseObject, error) {
	panic("ListMyDirectMessageThreads called from assets test shim")
}
func (shimImpl) GetMyUnreadDirectMessageCount(context.Context, openapi.GetMyUnreadDirectMessageCountRequestObject) (openapi.GetMyUnreadDirectMessageCountResponseObject, error) {
	panic("GetMyUnreadDirectMessageCount called from assets test shim")
}
func (shimImpl) ListDirectMessageThread(context.Context, openapi.ListDirectMessageThreadRequestObject) (openapi.ListDirectMessageThreadResponseObject, error) {
	panic("ListDirectMessageThread called from assets test shim")
}
func (shimImpl) SendDirectMessage(context.Context, openapi.SendDirectMessageRequestObject) (openapi.SendDirectMessageResponseObject, error) {
	panic("SendDirectMessage called from assets test shim")
}
func (shimImpl) MarkDirectMessageThreadRead(context.Context, openapi.MarkDirectMessageThreadReadRequestObject) (openapi.MarkDirectMessageThreadReadResponseObject, error) {
	panic("MarkDirectMessageThreadRead called from assets test shim")
}
