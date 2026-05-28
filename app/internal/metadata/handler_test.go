package metadata_test

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
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// TestFieldDefinitionLifecycle covers create / list / get / update /
// archive on the field schema layer, plus rejection of invalid codes.
func TestFieldDefinitionLifecycle(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	router, _ := makeRouter(t, pool, /*admin=*/ true)

	// Pre-clean any leftover test fields.
	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	// Create with invalid code -> 400
	bad := postJSON(t, router, "/fields", map[string]any{
		"code": "Bad-Code", "label": "Bad", "type": "text",
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid code: status=%d want 400 body=%s", bad.Code, bad.Body.String())
	}

	// Create with valid code -> 201
	createBody := map[string]any{
		"code":  "metadata_test_priority",
		"label": "Priority",
		"type":  "select",
		"options": map[string]any{
			"values": []map[string]any{
				{"value": "low", "label": "Low"},
				{"value": "high", "label": "High"},
			},
		},
		"display_group": "test",
	}
	rr := postJSON(t, router, "/fields", createBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var created openapi.FieldDefinition
	mustDecode(t, rr.Body.Bytes(), &created)
	if created.Code != "metadata_test_priority" {
		t.Errorf("code=%q want metadata_test_priority", created.Code)
	}
	fieldID := created.Id.String()

	// Duplicate code -> 400
	dup := postJSON(t, router, "/fields", createBody)
	if dup.Code != http.StatusBadRequest {
		t.Errorf("duplicate create: status=%d want 400", dup.Code)
	}

	// List
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, httptest.NewRequest(http.MethodGet, "/fields", nil))
	if listRR.Code != http.StatusOK {
		t.Fatalf("list: %d", listRR.Code)
	}
	var defs []openapi.FieldDefinition
	mustDecode(t, listRR.Body.Bytes(), &defs)
	found := false
	for _, d := range defs {
		if d.Code == "metadata_test_priority" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("created field missing from list (%d entries)", len(defs))
	}

	// Get by id
	getRR := httptest.NewRecorder()
	router.ServeHTTP(getRR, httptest.NewRequest(http.MethodGet, "/fields/"+fieldID, nil))
	if getRR.Code != http.StatusOK {
		t.Fatalf("get: %d", getRR.Code)
	}

	// Patch
	patchBody := map[string]any{"label": "Updated Priority", "required": true}
	patchRR := patchJSON(t, router, "/fields/"+fieldID, patchBody)
	if patchRR.Code != http.StatusOK {
		t.Fatalf("patch: %d body=%s", patchRR.Code, patchRR.Body.String())
	}
	var patched openapi.FieldDefinition
	mustDecode(t, patchRR.Body.Bytes(), &patched)
	if patched.Label != "Updated Priority" || !patched.Required {
		t.Errorf("patch didn't take: label=%q required=%v", patched.Label, patched.Required)
	}

	// Archive
	delRR := httptest.NewRecorder()
	router.ServeHTTP(delRR, httptest.NewRequest(http.MethodDelete, "/fields/"+fieldID, nil))
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("archive: %d", delRR.Code)
	}

	// Status query for archived should include it
	archivedRR := httptest.NewRecorder()
	router.ServeHTTP(archivedRR, httptest.NewRequest(http.MethodGet, "/fields?status=archived", nil))
	mustDecode(t, archivedRR.Body.Bytes(), &defs)
	found = false
	for _, d := range defs {
		if d.Id.String() == fieldID {
			found = true
		}
	}
	if !found {
		t.Errorf("archived field not in status=archived listing")
	}

	_ = ctx
}

// TestNonAdminCannotCreateField verifies the capability gate works.
func TestNonAdminCannotCreateField(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	router, _ := makeRouter(t, pool, /*admin=*/ false)
	rr := postJSON(t, router, "/fields", map[string]any{
		"code": "nonadmin_field", "label": "X", "type": "text",
	})
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin create: status=%d want 403", rr.Code)
	}
}

// TestAssetFieldValueLifecycle exercises the value set/get/clear/
// history path for several field types, verifies typed-column
// population, history append, and rejection of mismatched type.
func TestAssetFieldValueLifecycle(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	router, userRef := makeRouter(t, pool, /*admin=*/ true)
	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	// Create test fields of various types so we can write to each.
	textFieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_text", "label": "Text Field", "type": "text",
	})
	numFieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_score", "label": "Score", "type": "number",
	})
	dateFieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_due", "label": "Due", "type": "datetime",
	})
	multiFieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_tags", "label": "Tags", "type": "multi_select",
	})

	// Insert a throwaway asset (we don't go through /assets to keep
	// this test focused). The user must exist for owner_user_ref FK
	// purposes — but assets.owner_user_ref is nullable so we can
	// skip user setup and pass NULL.
	assetID := mustInsertAsset(t, pool, userRef)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM asset_field_value_history WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM assets WHERE id = $1`, assetID)
	})

	// Set text value
	r := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, textFieldID),
		map[string]any{"value_text": "hello world"})
	if r.Code != http.StatusOK {
		t.Fatalf("set text: %d body=%s", r.Code, r.Body.String())
	}
	var v openapi.AssetFieldValue
	mustDecode(t, r.Body.Bytes(), &v)
	if v.ValueText == nil || *v.ValueText != "hello world" {
		t.Errorf("text value mismatch: %+v", v.ValueText)
	}

	// Set number value
	r = putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, numFieldID),
		map[string]any{"value_num": 4.5})
	if r.Code != http.StatusOK {
		t.Fatalf("set num: %d body=%s", r.Code, r.Body.String())
	}

	// Set datetime value
	when := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	r = putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, dateFieldID),
		map[string]any{"value_date": when})
	if r.Code != http.StatusOK {
		t.Fatalf("set date: %d body=%s", r.Code, r.Body.String())
	}

	// Set multi_select value
	r = putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, multiFieldID),
		map[string]any{"value_options": []string{"alpha", "beta"}})
	if r.Code != http.StatusOK {
		t.Fatalf("set options: %d body=%s", r.Code, r.Body.String())
	}

	// Mismatched type → 400
	bad := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, textFieldID),
		map[string]any{"value_num": 7})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("mismatched type: status=%d want 400 body=%s", bad.Code, bad.Body.String())
	}

	// GET all values
	gAll := httptest.NewRecorder()
	router.ServeHTTP(gAll, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/assets/%s/fields", assetID), nil))
	if gAll.Code != http.StatusOK {
		t.Fatalf("get fields: %d", gAll.Code)
	}
	var values []openapi.AssetFieldValue
	mustDecode(t, gAll.Body.Bytes(), &values)
	if len(values) < 4 {
		t.Errorf("expected at least 4 values, got %d", len(values))
	}

	// Update text and verify history gets a new row
	r = putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, textFieldID),
		map[string]any{"value_text": "second value"})
	if r.Code != http.StatusOK {
		t.Fatalf("update text: %d", r.Code)
	}

	histRR := httptest.NewRecorder()
	router.ServeHTTP(histRR, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/assets/%s/fields/%s/history", assetID, textFieldID), nil))
	if histRR.Code != http.StatusOK {
		t.Fatalf("history: %d", histRR.Code)
	}
	var hist []openapi.FieldValueHistoryEntry
	mustDecode(t, histRR.Body.Bytes(), &hist)
	if len(hist) != 2 {
		t.Errorf("expected 2 history rows after 1 insert + 1 update, got %d", len(hist))
	}

	// Clear: 204, then GET returns empty for that field, and history
	// gets a row with new_value=null.
	clrRR := httptest.NewRecorder()
	router.ServeHTTP(clrRR, httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/assets/%s/fields/%s", assetID, textFieldID), nil))
	if clrRR.Code != http.StatusNoContent {
		t.Errorf("clear: %d", clrRR.Code)
	}

	histRR2 := httptest.NewRecorder()
	router.ServeHTTP(histRR2, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/assets/%s/fields/%s/history", assetID, textFieldID), nil))
	// Fresh slice — json.Unmarshal into a reused slice keeps fields
	// the new payload omits (omitempty), so we'd see stale NewValue
	// pointers from the earlier decode.
	var hist2 []openapi.FieldValueHistoryEntry
	mustDecode(t, histRR2.Body.Bytes(), &hist2)
	if len(hist2) != 3 {
		t.Errorf("expected 3 history rows after clear, got %d", len(hist2))
	}
	gotClear := false
	for _, h := range hist2 {
		if h.NewValue == nil {
			gotClear = true
			break
		}
	}
	if !gotClear {
		t.Errorf("no clear-event history row (new_value=nil) found among %d entries", len(hist2))
	}

	// Verify search_text on the asset is populated (trigger fired).
	var searchText *string
	if err := pool.QueryRow(context.Background(),
		`SELECT search_text::text FROM assets WHERE id = $1`, assetID).Scan(&searchText); err != nil {
		t.Fatalf("read search_text: %v", err)
	}
	if searchText == nil || !strings.Contains(*searchText, "alpha") {
		t.Errorf("search_text should mention 'alpha' from the tags field; got %v", searchText)
	}
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// makeRouter builds a router that injects a synthetic admin (or
// non-admin) identity. Returns the router and the user ref it
// claims so we can clean up DB rows that reference it.
func makeRouter(t *testing.T, pool *pgxpool.Pool, admin bool) (chi.Router, int64) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// nil registry is intentional — these tests exercise the handler
	// in isolation without spinning up the LISTEN goroutine. The cache
	// integration is covered by internal/cache/cache_test.go.
	h := metadata.NewHandler(pool, logger, nil)

	userRef := int64(420000)
	caps := []string{}
	if admin {
		caps = []string{metadata.CapFieldsAdmin}
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
	openapi.HandlerFromMux(openapi.NewStrictHandler(metaShim{h: h}, nil), router)
	return router, userRef
}

func cleanTestFields(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE 'metadata_test_%' OR code LIKE 'mtv_%' OR code LIKE 'nonadmin_%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE 'metadata_test_%' OR code LIKE 'mtv_%' OR code LIKE 'nonadmin_%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM field_definition WHERE code LIKE 'metadata_test_%' OR code LIKE 'mtv_%' OR code LIKE 'nonadmin_%'`)
}

func mustCreateField(t *testing.T, router chi.Router, body map[string]any) string {
	t.Helper()
	rr := postJSON(t, router, "/fields", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create field: %d body=%s", rr.Code, rr.Body.String())
	}
	var def openapi.FieldDefinition
	mustDecode(t, rr.Body.Bytes(), &def)
	return def.Id.String()
}

func mustInsertAsset(t *testing.T, pool *pgxpool.Pool, userRef int64) string {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO assets (title, resource_type, owner_user_ref) VALUES ('test asset', 1, $1) RETURNING id`,
		userRef).Scan(&id); err != nil {
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

func putJSON(t *testing.T, r chi.Router, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(b))
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

type metaShim struct{ h *metadata.Handler }

func (s metaShim) ListFields(ctx context.Context, req openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	return s.h.ListFields(ctx, req)
}
func (s metaShim) CreateField(ctx context.Context, req openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	return s.h.CreateField(ctx, req)
}
func (s metaShim) GetField(ctx context.Context, req openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	return s.h.GetField(ctx, req)
}
func (s metaShim) UpdateField(ctx context.Context, req openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	return s.h.UpdateField(ctx, req)
}
func (s metaShim) ArchiveField(ctx context.Context, req openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	return s.h.ArchiveField(ctx, req)
}
func (s metaShim) GetAssetFields(ctx context.Context, req openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	return s.h.GetAssetFields(ctx, req)
}
func (s metaShim) SetAssetFieldValue(ctx context.Context, req openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	return s.h.SetAssetFieldValue(ctx, req)
}
func (s metaShim) ClearAssetFieldValue(ctx context.Context, req openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	return s.h.ClearAssetFieldValue(ctx, req)
}
func (s metaShim) GetAssetFieldValueHistory(ctx context.Context, req openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	return s.h.GetAssetFieldValueHistory(ctx, req)
}

func (metaShim) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from metadata test shim")
}
func (metaShim) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from metadata test shim")
}
func (metaShim) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from metadata test shim")
}
func (metaShim) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from metadata test shim")
}
func (metaShim) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from metadata test shim")
}
func (metaShim) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from metadata test shim")
}
func (metaShim) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from metadata test shim")
}
func (metaShim) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from metadata test shim")
}
func (metaShim) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from metadata test shim")
}
func (metaShim) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from metadata test shim")
}
func (metaShim) ListResourceTypes(context.Context, openapi.ListResourceTypesRequestObject) (openapi.ListResourceTypesResponseObject, error) {
	panic("ListResourceTypes called from metadata test shim")
}
func (metaShim) UploadStorageObject(context.Context, openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	panic("UploadStorageObject called from metadata test shim")
}
func (metaShim) DownloadStorageObject(context.Context, openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	panic("DownloadStorageObject called from metadata test shim")
}
func (metaShim) DownloadStorageObjectVariant(context.Context, openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	panic("DownloadStorageObjectVariant called from metadata test shim")
}
func (metaShim) CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	panic("CreateAsset called from metadata test shim")
}
func (metaShim) ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	panic("ListAssets called from metadata test shim")
}
func (metaShim) GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	panic("GetAsset called from metadata test shim")
}
func (metaShim) UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	panic("UpdateAsset called from metadata test shim")
}
func (metaShim) DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	panic("DeleteAsset called from metadata test shim")
}
func (metaShim) DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	panic("DownloadAssetFile called from metadata test shim")
}
func (metaShim) DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	panic("DownloadAssetVariant called from metadata test shim")
}
func (metaShim) AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	panic("AddAssetTags called from metadata test shim")
}
func (metaShim) RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	panic("RemoveAssetTag called from metadata test shim")
}
func (metaShim) GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from metadata test shim")
}
func (metaShim) CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from metadata test shim")
}
func (metaShim) ListWorkflowStates(context.Context, openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from metadata test shim")
}
func (metaShim) ListCollections(context.Context, openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	panic("ListCollections called from metadata test shim")
}
func (metaShim) CreateCollection(context.Context, openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	panic("CreateCollection called from metadata test shim")
}
func (metaShim) GetCollection(context.Context, openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	panic("GetCollection called from metadata test shim")
}
func (metaShim) UpdateCollection(context.Context, openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	panic("UpdateCollection called from metadata test shim")
}
func (metaShim) DeleteCollection(context.Context, openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	panic("DeleteCollection called from metadata test shim")
}
func (metaShim) ListCollectionResources(context.Context, openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	panic("ListCollectionResources called from metadata test shim")
}
func (metaShim) AddCollectionResource(context.Context, openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	panic("AddCollectionResource called from metadata test shim")
}
func (metaShim) RemoveCollectionResource(context.Context, openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	panic("RemoveCollectionResource called from metadata test shim")
}
func (metaShim) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from metadata test shim")
}
func (metaShim) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from metadata test shim")
}
func (metaShim) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from metadata test shim")
}
func (metaShim) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from metadata test shim")
}
func (metaShim) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from metadata test shim")
}
func (metaShim) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from metadata test shim")
}
func (metaShim) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from metadata test shim")
}
func (metaShim) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from metadata_test test shim")
}
func (metaShim) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from metadata_test test shim")
}
func (metaShim) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from metadata_test test shim")
}
func (metaShim) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from metadata_test test shim")
}
func (metaShim) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from metadata_test test shim")
}
func (metaShim) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from metadata_test test shim")
}
func (metaShim) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from metadata_test test shim")
}
func (metaShim) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from metadata_test test shim")
}
func (metaShim) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from metadata_test test shim")
}
func (metaShim) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from metadata_test test shim")
}
func (metaShim) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from metadata_test test shim")
}
func (metaShim) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from metadata_test test shim")
}
func (metaShim) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from metadata_test test shim")
}
func (metaShim) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from metadata_test test shim")
}
func (metaShim) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from metadata_test test shim")
}
func (metaShim) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from metadata_test test shim")
}
func (metaShim) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from metadata_test test shim")
}
func (metaShim) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from metadata_test test shim")
}
func (metaShim) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from metadata_test test shim")
}
func (metaShim) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from metadata_test test shim")
}
func (metaShim) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from metadata_test test shim")
}
func (metaShim) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from metadata_test test shim")
}
func (metaShim) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from metadata_test test shim")
}
func (metaShim) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from metadata_test test shim")
}
func (metaShim) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from metadata_test test shim")
}
func (metaShim) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from metadata_test test shim")
}
func (metaShim) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from metadata_test test shim")
}

func (metaShim) GetSiteConfig(context.Context, openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	panic("GetSiteConfig called from metadata test shim")
}
func (metaShim) UpdateSiteConfig(context.Context, openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from metadata test shim")
}
func (metaShim) GetSMTPConfig(context.Context, openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from metadata test shim")
}
func (metaShim) UpdateSMTPConfig(context.Context, openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from metadata test shim")
}
func (metaShim) GetAuthConfig(context.Context, openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from metadata test shim")
}
func (metaShim) UpdateAuthConfig(context.Context, openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from metadata test shim")
}
func (metaShim) GetAIConfig(context.Context, openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from metadata test shim")
}
func (metaShim) UpdateAIConfig(context.Context, openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from metadata test shim")
}
func (metaShim) ListLocales(context.Context, openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from metadata test shim")
}

func (metaShim) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from metadata test shim")
}
func (metaShim) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from metadata test shim")
}
func (metaShim) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from metadata test shim")
}

// --- jobs stubs (Phase 1.18.A) -------------------------------------------
func (metaShim) ClaimJobs(context.Context, openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	panic("ClaimJobs called from test shim")
}
func (metaShim) GetJob(context.Context, openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	panic("GetJob called from test shim")
}
func (metaShim) HeartbeatJob(context.Context, openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	panic("HeartbeatJob called from test shim")
}
func (metaShim) CompleteJob(context.Context, openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	panic("CompleteJob called from test shim")
}
func (metaShim) FailJob(context.Context, openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	panic("FailJob called from test shim")
}
