// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

// TestFieldDefinitionLifecycle covers create / list / get / update /
// archive on the field schema layer, plus rejection of invalid codes.
func TestFieldDefinitionLifecycle(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx := t.Context()

	pool := openPool(t, pwd)
	defer pool.Close()

	router, _ := makeRouter(t, pool /*admin=*/, true)

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

	// #528 — the DEFAULT listing (no status param) must exclude archived
	// fields. They're tombstones; editors like the collection edit modal
	// call GET /fields without a status and must never render them.
	defaultRR := httptest.NewRecorder()
	router.ServeHTTP(defaultRR, httptest.NewRequest(http.MethodGet, "/fields", nil))
	mustDecode(t, defaultRR.Body.Bytes(), &defs)
	for _, d := range defs {
		if d.Id.String() == fieldID {
			t.Errorf("archived field %s leaked into the default (no-status) listing", fieldID)
		}
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

// TestSetFieldExtraction covers the PR-B PUT /fields/{id}/extraction
// surface: wire on, normalise empty mode → skip_if_set, reject bad
// mode, clear, capability gate, 404 on unknown id.
func TestSetFieldExtraction(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	adminRouter, _ := makeRouter(t, pool, true)
	fieldID := mustCreateField(t, adminRouter, map[string]any{
		"code":  "metadata_test_extract_target",
		"label": "Capture",
		"type":  "datetime",
	})

	// Wire source with no explicit mode → server normalises to skip_if_set.
	rr := putJSON(t, adminRouter, "/fields/"+fieldID+"/extraction", map[string]any{
		"source": "capture_datetime",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("set extraction: %d body=%s", rr.Code, rr.Body.String())
	}
	var got openapi.FieldDefinition
	mustDecode(t, rr.Body.Bytes(), &got)
	if got.ExtractionSource == nil || *got.ExtractionSource != "capture_datetime" {
		t.Errorf("ExtractionSource = %v, want capture_datetime", got.ExtractionSource)
	}
	if got.ExtractionMode == nil || *got.ExtractionMode != openapi.FieldDefinitionExtractionModeSkipIfSet {
		t.Errorf("ExtractionMode = %v, want skip_if_set", got.ExtractionMode)
	}

	// Explicit mode replace.
	rr = putJSON(t, adminRouter, "/fields/"+fieldID+"/extraction", map[string]any{
		"source": "capture_datetime",
		"mode":   "replace",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("set replace: %d", rr.Code)
	}
	mustDecode(t, rr.Body.Bytes(), &got)
	if got.ExtractionMode == nil || *got.ExtractionMode != openapi.FieldDefinitionExtractionModeReplace {
		t.Errorf("ExtractionMode = %v, want replace", got.ExtractionMode)
	}

	// Bogus mode → 400.
	rr = putJSON(t, adminRouter, "/fields/"+fieldID+"/extraction", map[string]any{
		"source": "capture_datetime",
		"mode":   "DELETE_THE_DATABASE",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bogus mode: %d want 400", rr.Code)
	}

	// Clear extraction (empty source).
	rr = putJSON(t, adminRouter, "/fields/"+fieldID+"/extraction", map[string]any{
		"source": "",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear extraction: %d", rr.Code)
	}
	mustDecode(t, rr.Body.Bytes(), &got)
	if got.ExtractionSource == nil || *got.ExtractionSource != "" {
		t.Errorf("ExtractionSource after clear = %v, want empty", got.ExtractionSource)
	}

	// Unknown id → 404.
	rr = putJSON(t, adminRouter, "/fields/00000000-0000-0000-0000-000000000000/extraction", map[string]any{
		"source": "capture_datetime",
	})
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d want 404", rr.Code)
	}

	// Non-admin → 403.
	nonAdminRouter, _ := makeRouter(t, pool, false)
	rr = putJSON(t, nonAdminRouter, "/fields/"+fieldID+"/extraction", map[string]any{
		"source": "capture_datetime",
	})
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin: %d want 403", rr.Code)
	}
}

// TestNonAdminCannotCreateField verifies the capability gate works.
func TestNonAdminCannotCreateField(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	router, _ := makeRouter(t, pool /*admin=*/, false)
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

	router, userRef := makeRouter(t, pool /*admin=*/, true)
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
	// The vocabulary is not decoration. This fixture defined a
	// multi_select with NO options and then wrote "alpha"/"beta" to it,
	// which the write path accepted because it never checked membership
	// (#824). It is also a state production cannot reach: a
	// multi_select with an empty vocabulary renders as an empty picker,
	// so no operator can put a value in it. Give the field the terms a
	// real one would have, and the test exercises the real path.
	multiFieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_tags", "label": "Tags", "type": "multi_select",
		"options": map[string]any{"values": []any{"alpha", "beta", "gamma"}},
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
	openapi.HandlerFromMux(openapi.NewStrictHandler(metaShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
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
		`INSERT INTO assets (title, asset_type, owner_user_ref) VALUES ('test asset', 1, $1) RETURNING id`,
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
	ctx := t.Context()

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
// shim — overrides only the metadata methods this test exercises;
// every other StrictServerInterface method panics via the
// embedded *strictservershim.PanicShim.
// ---------------------------------------------------------------------------

type metaShim struct {
	*strictservershim.PanicShim
	h *metadata.Handler
}

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
func (s metaShim) SetFieldExtraction(ctx context.Context, req openapi.SetFieldExtractionRequestObject) (openapi.SetFieldExtractionResponseObject, error) {
	return s.h.SetFieldExtraction(ctx, req)
}
func (s metaShim) ListFieldDefaultOverrides(ctx context.Context, req openapi.ListFieldDefaultOverridesRequestObject) (openapi.ListFieldDefaultOverridesResponseObject, error) {
	return s.h.ListFieldDefaultOverrides(ctx, req)
}
func (s metaShim) SetFieldDefaultOverride(ctx context.Context, req openapi.SetFieldDefaultOverrideRequestObject) (openapi.SetFieldDefaultOverrideResponseObject, error) {
	return s.h.SetFieldDefaultOverride(ctx, req)
}
func (s metaShim) DeleteFieldDefaultOverride(ctx context.Context, req openapi.DeleteFieldDefaultOverrideRequestObject) (openapi.DeleteFieldDefaultOverrideResponseObject, error) {
	return s.h.DeleteFieldDefaultOverride(ctx, req)
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
func (s metaShim) GetCollectionFields(ctx context.Context, req openapi.GetCollectionFieldsRequestObject) (openapi.GetCollectionFieldsResponseObject, error) {
	return s.h.GetCollectionFields(ctx, req)
}
func (s metaShim) SetCollectionFieldValue(ctx context.Context, req openapi.SetCollectionFieldValueRequestObject) (openapi.SetCollectionFieldValueResponseObject, error) {
	return s.h.SetCollectionFieldValue(ctx, req)
}
func (s metaShim) ClearCollectionFieldValue(ctx context.Context, req openapi.ClearCollectionFieldValueRequestObject) (openapi.ClearCollectionFieldValueResponseObject, error) {
	return s.h.ClearCollectionFieldValue(ctx, req)
}
func (s metaShim) GetCollectionFieldValueHistory(ctx context.Context, req openapi.GetCollectionFieldValueHistoryRequestObject) (openapi.GetCollectionFieldValueHistoryResponseObject, error) {
	return s.h.GetCollectionFieldValueHistory(ctx, req)
}
