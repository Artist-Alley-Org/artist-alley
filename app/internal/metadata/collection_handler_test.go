// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.9.B — collection field-value integration tests.
//
// These ride the same router scaffolding handler_test.go uses for
// the asset-side path so the assertions go through the full strict-
// server stack (JSON in, JSON out, real DB). The tests deliberately
// don't share fixtures with the asset tests — the cleanup query
// runs at start AND end so a flaky prior run can't pollute.
package metadata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const collectionTestPrefix = "mcoltest_"

// TestCollectionField_Upsert_NewValue_Inserts covers the happy path:
// admin defines a collection field; setting a value via PUT returns
// 200 and the row reads back via GET.
func TestCollectionField_Upsert_NewValue_Inserts(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_client", "Client", "text")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col 1")

	rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), map[string]any{
		"value_text": "Acme",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT value: status=%d body=%s", rr.Code, rr.Body.String())
	}

	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/collections/%s/fields", collectionID), nil))
	if listRR.Code != http.StatusOK {
		t.Fatalf("GET values: %d", listRR.Code)
	}
	var values []openapi.CollectionFieldValue
	mustDecode(t, listRR.Body.Bytes(), &values)
	if len(values) != 1 {
		t.Fatalf("got %d values, want 1", len(values))
	}
	if values[0].ValueText == nil || *values[0].ValueText != "Acme" {
		t.Errorf("value_text = %v, want \"Acme\"", values[0].ValueText)
	}
}

// TestCollectionField_Upsert_Replace_OverridesAndWritesHistory covers
// the conflict path — second write replaces and the history table
// records both values.
func TestCollectionField_Upsert_Replace_OverridesAndWritesHistory(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_priority", "Priority", "text")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col 2")

	if rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), map[string]any{
		"value_text": "low",
	}); rr.Code != http.StatusOK {
		t.Fatalf("first PUT: %d", rr.Code)
	}
	if rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), map[string]any{
		"value_text": "high",
	}); rr.Code != http.StatusOK {
		t.Fatalf("second PUT: %d", rr.Code)
	}

	// Value row reflects the latest.
	var got string
	if err := pool.QueryRow(context.Background(),
		`SELECT value_text FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&got); err != nil {
		t.Fatalf("read value row: %v", err)
	}
	if got != "high" {
		t.Errorf("value_text = %q, want \"high\"", got)
	}

	// History has both rows.
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collection_field_value_history
		 WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&count); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if count != 2 {
		t.Errorf("history rows = %d, want 2", count)
	}
}

// TestCollectionField_AssetSubjectRejected_422 covers the
// discriminator gate — putting a value on a field whose
// subject_kind is 'asset' returns 422 with field_not_for_collection.
func TestCollectionField_AssetSubjectRejected_422(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	// Asset-scoped field (the default subject_kind when none supplied).
	assetFieldID := mustCreateField(t, router, map[string]any{
		"code":  "mcoltest_asset_only",
		"label": "Asset Only",
		"type":  "text",
	})
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col 3")

	rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, assetFieldID), map[string]any{
		"value_text": "should fail",
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != "field_not_for_collection" {
		t.Errorf("reason=%v want field_not_for_collection", body["reason"])
	}
}

// TestCollectionField_TypeMismatch_422 — number field, text payload.
func TestCollectionField_TypeMismatch_422(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_amount", "Amount", "number")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col 4")

	rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), map[string]any{
		"value_text": "not a number",
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != "value_type_mismatch" {
		t.Errorf("reason=%v want value_type_mismatch", body["reason"])
	}
}

// TestCollectionField_Delete_RemovesRowAndWritesHistory.
func TestCollectionField_Delete_RemovesRowAndWritesHistory(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_notes", "Notes", "text")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col 5")

	if rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), map[string]any{
		"value_text": "to be deleted",
	}); rr.Code != http.StatusOK {
		t.Fatalf("seed PUT: %d", rr.Code)
	}

	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Row is gone.
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collection_field_value
		 WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("value row count = %d, want 0 after delete", count)
	}

	// History has 2 rows: the upsert + the delete.
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collection_field_value_history
		 WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&count); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if count != 2 {
		t.Errorf("history count = %d, want 2", count)
	}
}

// TestCollectionField_HistoryEndpoint_ReturnsNewestFirst.
func TestCollectionField_HistoryEndpoint_ReturnsNewestFirst(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_status", "Status", "text")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col 6")

	for _, v := range []string{"draft", "review", "final"} {
		if rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID), map[string]any{
			"value_text": v,
		}); rr.Code != http.StatusOK {
			t.Fatalf("PUT %q: %d", v, rr.Code)
		}
	}

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/collections/%s/fields/%s/history", collectionID, fieldID), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET history: %d body=%s", rr.Code, rr.Body.String())
	}

	var entries []openapi.CollectionFieldValueHistoryEntry
	mustDecode(t, rr.Body.Bytes(), &entries)
	if len(entries) != 3 {
		t.Fatalf("history len = %d, want 3", len(entries))
	}
	// Newest first: last write was "final"; sqlc ORDER BY DESC.
	if got, _ := entries[0].NewValue, error(nil); got == nil {
		t.Fatal("entries[0].NewValue is nil")
	}
	// Read the value from the JSONB shape — valueRowToJSON writes
	// {"type": "text", "value": "final"} for the text type.
	first := (*entries[0].NewValue)["value"]
	if first != "final" {
		t.Errorf("newest entry value = %v, want \"final\"", first)
	}
}

// TestCollectionField_FilterListBySubjectKind verifies
// GET /fields?subject_kind=collection only returns collection-scoped
// definitions.
func TestCollectionField_FilterListBySubjectKind(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	cleanTestFields(t, pool)
	t.Cleanup(func() {
		cleanCollectionTestRows(t, pool)
		cleanTestFields(t, pool)
	})

	router, _ := makeRouter(t, pool, true)
	// Asset-scoped (default).
	_ = mustCreateField(t, router, map[string]any{
		"code":  "metadata_test_asset",
		"label": "Asset only",
		"type":  "text",
	})
	// Collection-scoped.
	collectionFieldID := mustCreateCollectionField(t, router, "mcoltest_only", "Collection only", "text")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/fields?subject_kind=collection", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d", rr.Code)
	}
	var defs []openapi.FieldDefinition
	mustDecode(t, rr.Body.Bytes(), &defs)

	sawCollection := false
	for _, d := range defs {
		if d.SubjectKind != openapi.FieldDefinitionSubjectKindCollection {
			t.Errorf("filter returned %q field %s with subject_kind=%s", d.Code, d.Id, d.SubjectKind)
		}
		if d.Id.String() == collectionFieldID {
			sawCollection = true
		}
	}
	if !sawCollection {
		t.Error("created collection field missing from filtered list")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustCreateCollectionField(t *testing.T, router interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, code, label, fieldType string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"code":         code,
		"label":        label,
		"type":         fieldType,
		"subject_kind": "collection",
	})
	req := httptest.NewRequest(http.MethodPost, "/fields", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection field: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var def openapi.FieldDefinition
	mustDecode(t, rr.Body.Bytes(), &def)
	if def.SubjectKind != openapi.FieldDefinitionSubjectKindCollection {
		t.Fatalf("created field subject_kind=%s want collection", def.SubjectKind)
	}
	return def.Id.String()
}

func mustInsertCollection(t *testing.T, pool *pgxpool.Pool, ownerRef int64, name string) string {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO collections (owner_user_ref, name) VALUES ($1, $2) RETURNING id`,
		ownerRef, name).Scan(&id); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	return id.String()
}

func cleanCollectionTestRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Order: history → values → field defs → collections. Cascades
	// would handle some of these but explicit order keeps test
	// failures readable.
	_, _ = pool.Exec(ctx, `DELETE FROM collection_field_value_history
		WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE $1)`,
		collectionTestPrefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM collection_field_value
		WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE $1)`,
		collectionTestPrefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM field_definition WHERE code LIKE $1`,
		collectionTestPrefix+"%")
	_, _ = pool.Exec(ctx, `DELETE FROM collections WHERE name LIKE 'mcoltest col %'`)
}

// TestCollectionField_DanglingReferenceRejected_422 is the collection
// half of #842: a reference write on a collection is gated exactly like
// the asset path — a UUID naming no asset is refused 422
// dangling_reference and stores nothing, a real target writes 200.
func TestCollectionField_DanglingReferenceRejected_422(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_ref", "Ref", "reference")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col ref")
	target := mustInsertTitledAsset(t, pool, userRef, "Coll Ref Target", "active")
	cleanupAssets(t, pool, target)

	orphan := uuid.New().String()
	rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID),
		map[string]any{"value_ref": orphan})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dangling collection reference write: status=%d want 422 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != "dangling_reference" {
		t.Errorf("reason=%v want dangling_reference", body["reason"])
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&count); err != nil {
		t.Fatalf("count value rows: %v", err)
	}
	if count != 0 {
		t.Errorf("a rejected dangling write left %d collection value row(s); it must store nothing", count)
	}

	if ok := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID),
		map[string]any{"value_ref": target}); ok.Code != http.StatusOK {
		t.Fatalf("valid collection reference write: status=%d want 200 body=%s", ok.Code, ok.Body.String())
	}
}

// TestCollectionField_ResolvesOptionsAndReference is the behavioural
// core of #840: a collection carrying a select value and a reference
// value renders the LABEL and the linked TITLE, not the raw slug and a
// bare UUID — the same resolution the asset path has always done. It
// also pins the #839 degradation: once the reference target is
// soft-deleted, resolved_reference goes absent and the bare id remains.
func TestCollectionField_ResolvesOptionsAndReference(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	selectField := mustCreateField(t, router, map[string]any{
		"code": "mcoltest_status", "label": "Status", "type": "select",
		"subject_kind": "collection",
		"options": map[string]any{
			"values": []any{map[string]any{"value": "in_review", "label": "In Review"}},
		},
	})
	refField := mustCreateCollectionField(t, router, "mcoltest_hero", "Hero", "reference")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col resolve")
	target := mustInsertTitledAsset(t, pool, userRef, "Hero Asset", "active")
	cleanupAssets(t, pool, target)

	if rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, selectField),
		map[string]any{"value_text": "in_review"}); rr.Code != http.StatusOK {
		t.Fatalf("set select: %d %s", rr.Code, rr.Body.String())
	}
	if rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, refField),
		map[string]any{"value_ref": target}); rr.Code != http.StatusOK {
		t.Fatalf("set reference: %d %s", rr.Code, rr.Body.String())
	}

	sel := findCollectionValue(t, getCollectionFields(t, router, collectionID), selectField)
	if sel.ResolvedOptions == nil {
		t.Fatal("resolved_options is nil on a collection select value — #840 did not resolve it")
	}
	if opt, ok := (*sel.ResolvedOptions)["in_review"]; !ok || opt.Label != "In Review" {
		t.Errorf("resolved_options[in_review] = %+v, want label \"In Review\"", opt)
	}

	ref := findCollectionValue(t, getCollectionFields(t, router, collectionID), refField)
	if ref.ResolvedReference == nil {
		t.Fatal("resolved_reference is nil for a live collection reference target — the #840 bug")
	}
	if ref.ResolvedReference.Title != "Hero Asset" {
		t.Errorf("resolved_reference.title = %q, want \"Hero Asset\"", ref.ResolvedReference.Title)
	}

	// #839 degradation: soft-delete the target, resolved_reference goes
	// absent, the bare id stays. No disclosure that the row was removed.
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, target); err != nil {
		t.Fatalf("soft-delete target: %v", err)
	}
	after := findCollectionValue(t, getCollectionFields(t, router, collectionID), refField)
	if after.ResolvedReference != nil {
		t.Errorf("resolved_reference = %+v for a soft-deleted target, want absent", *after.ResolvedReference)
	}
	if after.ValueRef == nil || after.ValueRef.String() != target {
		t.Errorf("value_ref = %v, want %s — the id must remain for the client to degrade to", after.ValueRef, target)
	}
}

// getCollectionFields GETs the value list for a collection.
func getCollectionFields(t *testing.T, router interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, collectionID string) []openapi.CollectionFieldValue {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/collections/%s/fields", collectionID), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET collection fields: %d %s", rr.Code, rr.Body.String())
	}
	var values []openapi.CollectionFieldValue
	mustDecode(t, rr.Body.Bytes(), &values)
	return values
}

func findCollectionValue(t *testing.T, values []openapi.CollectionFieldValue, fieldID string) openapi.CollectionFieldValue {
	t.Helper()
	for _, v := range values {
		if v.FieldId.String() == fieldID {
			return v
		}
	}
	t.Fatalf("field %s not found among %d collection values", fieldID, len(values))
	return openapi.CollectionFieldValue{}
}
