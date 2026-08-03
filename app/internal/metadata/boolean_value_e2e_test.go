// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the `boolean` field type (#791).
//
// The last of the field types that had never once been exercised
// end-to-end, and the last one whose writers disagreed. ADR 0012 has
// always said 0/1 in value_num; the asset writer and the seeder
// complied, the collection writer and every display surface used the
// strings "true"/"false" in value_text. So a boolean set on an asset
// rendered blank, a boolean set during upload was rejected outright,
// and a boolean set on a collection landed somewhere nothing looks.
//
// Every one of those writers passed its own unit tests. This file
// drives the real path instead: create a boolean field through the
// API, value it on an asset AND a collection, and assert against the
// DATABASE COLUMNS and against the read model the display consumes.
//
// `false` is tested as carefully as `true`. It is the value most
// likely to be dropped by a writer that treats "unset" and "false" as
// the same thing — 0 is falsy in every language on this path.
package metadata_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// TestBooleanValueEndToEnd is #791's acceptance item 2: a boolean set
// on an asset and on a collection both round-trip correctly.
func TestBooleanValueEndToEnd(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() {
		cleanTestFields(t, pool)
		cleanCollectionTestRows(t, pool)
	})

	router, userRef := makeRouter(t, pool /*admin=*/, true)

	assetFieldID := mustCreateField(t, router, map[string]any{
		"code":  "mtv_nsfw",
		"label": "Adult content",
		"type":  "boolean",
	})

	assetID := mustInsertAsset(t, pool, userRef)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assetID)
	})

	assetPath := fmt.Sprintf("/assets/%s/fields/%s", assetID, assetFieldID)

	// ── Asset side: true ────────────────────────────────────────────
	rr := putJSON(t, router, assetPath, map[string]any{"value_num": 1})
	if rr.Code != http.StatusOK {
		t.Fatalf("set asset boolean true: status=%d body=%s", rr.Code, rr.Body.String())
	}
	assertAssetBooleanColumns(t, pool, assetID, assetFieldID, 1)

	v := findAssetValue(t, getAssetFields(t, router, assetID), assetFieldID)
	if v.ValueNum == nil || *v.ValueNum != 1 {
		t.Errorf("asset value_num = %v, want 1", v.ValueNum)
	}
	// The assertion that names the bug: the display read value_text,
	// and no writer has ever populated it for a boolean field.
	if v.ValueText != nil {
		t.Errorf("asset value_text = %q, want nil — the display read this column until #791, "+
			"which is exactly why a boolean rendered blank", *v.ValueText)
	}

	// ── Asset side: false is a VALUE, not an absence ────────────────
	rr = putJSON(t, router, assetPath, map[string]any{"value_num": 0})
	if rr.Code != http.StatusOK {
		t.Fatalf("set asset boolean false: status=%d body=%s", rr.Code, rr.Body.String())
	}
	assertAssetBooleanColumns(t, pool, assetID, assetFieldID, 0)

	v = findAssetValue(t, getAssetFields(t, router, assetID), assetFieldID)
	if v.ValueNum == nil {
		t.Fatal("asset value_num is nil after setting false — 0 was dropped as if it " +
			"meant \"unset\". A stored false must be distinguishable from no value at all")
	}
	if *v.ValueNum != 0 {
		t.Errorf("asset value_num = %v, want 0", *v.ValueNum)
	}

	// ── Asset side: the drifted shape is rejected ───────────────────
	// "true" in value_text is what the upload modal's checkbox sent
	// until #791. It has always been a 400, which is why setting a
	// boolean during upload failed rather than merely rendering blank.
	bad := putJSON(t, router, assetPath, map[string]any{"value_text": "true"})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("asset boolean write with value_text: status=%d want 400 body=%s",
			bad.Code, bad.Body.String())
	}
	bad = putJSON(t, router, assetPath, map[string]any{"value_num": 2})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("asset boolean write with value_num=2: status=%d want 400 body=%s",
			bad.Code, bad.Body.String())
	}

	// ── Collection side: same type, same column, same encoding ──────
	collFieldID := mustCreateCollectionField(t, router, "mcoltest_nsfw", "Adult content", "boolean")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest bool col")
	collPath := fmt.Sprintf("/collections/%s/fields/%s", collectionID, collFieldID)

	rr = putJSON(t, router, collPath, map[string]any{"value_num": 1})
	if rr.Code != http.StatusOK {
		t.Fatalf("set collection boolean true: status=%d body=%s", rr.Code, rr.Body.String())
	}
	assertCollectionBooleanColumns(t, pool, collectionID, collFieldID, 1)

	// The shape this path used to REQUIRE. Before #791 this request
	// succeeded and put the value in value_text, a different column
	// from the same field's value on an asset.
	//
	// 422, not the asset side's 400: the collection operation has only
	// ever declared 422 for value_type_mismatch.
	badColl := putJSON(t, router, collPath, map[string]any{"value_text": "true"})
	if badColl.Code != http.StatusUnprocessableEntity {
		t.Errorf("collection boolean write with value_text: status=%d want 422 body=%s",
			badColl.Code, badColl.Body.String())
	}
	badColl = putJSON(t, router, collPath, map[string]any{"value_num": 2})
	if badColl.Code != http.StatusUnprocessableEntity {
		t.Errorf("collection boolean write with value_num=2: status=%d want 422 body=%s",
			badColl.Code, badColl.Body.String())
	}

	// The failed writes must not have disturbed the stored value.
	assertCollectionBooleanColumns(t, pool, collectionID, collFieldID, 1)

	rr = putJSON(t, router, collPath, map[string]any{"value_num": 0})
	if rr.Code != http.StatusOK {
		t.Fatalf("set collection boolean false: status=%d body=%s", rr.Code, rr.Body.String())
	}
	assertCollectionBooleanColumns(t, pool, collectionID, collFieldID, 0)

	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/collections/%s/fields", collectionID), nil))
	if listRR.Code != http.StatusOK {
		t.Fatalf("GET collection values: %d", listRR.Code)
	}
	var collValues []openapi.CollectionFieldValue
	mustDecode(t, listRR.Body.Bytes(), &collValues)
	if len(collValues) != 1 {
		t.Fatalf("got %d collection values, want 1", len(collValues))
	}
	if collValues[0].ValueNum == nil || *collValues[0].ValueNum != 0 {
		t.Errorf("collection value_num = %v, want 0", collValues[0].ValueNum)
	}
	if collValues[0].ValueText != nil {
		t.Errorf("collection value_text = %q, want nil", *collValues[0].ValueText)
	}

	// ── The cross-surface invariant, stated plainly ─────────────────
	assertSameStoredColumn(t, pool, assetID, assetFieldID, collectionID, collFieldID, "value_num")
}

// assertAssetBooleanColumns asks the table, not the handler. The
// value AND the emptiness of every other column both matter: a
// writer that populates value_text as well as value_num has left a
// stale answer behind for the next reader to find.
func assertAssetBooleanColumns(t *testing.T, pool *pgxpool.Pool, assetID, fieldID string, want float64) {
	t.Helper()
	var text *string
	var num *float64
	var opts []string
	var ref *string
	err := pool.QueryRow(context.Background(), `
		SELECT value_text, value_num, value_options, value_ref::text
		  FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		assetID, fieldID).Scan(&text, &num, &opts, &ref)
	if err != nil {
		t.Fatalf("read asset_field_value row: %v", err)
	}
	if num == nil || *num != want {
		t.Errorf("asset_field_value.value_num = %v, want %v", num, want)
	}
	if text != nil {
		t.Errorf("asset_field_value.value_text = %q, want NULL — a boolean is 0/1 in "+
			"value_num, never the strings \"true\"/\"false\" (ADR 0012, #791)", *text)
	}
	if opts != nil {
		t.Errorf("asset_field_value.value_options = %v, want NULL", opts)
	}
	if ref != nil {
		t.Errorf("asset_field_value.value_ref = %v, want NULL", *ref)
	}
}

func assertCollectionBooleanColumns(t *testing.T, pool *pgxpool.Pool, collectionID, fieldID string, want float64) {
	t.Helper()
	var text *string
	var num *float64
	var opts []string
	var ref *string
	err := pool.QueryRow(context.Background(), `
		SELECT value_text, value_num, value_options, value_ref::text
		  FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&text, &num, &opts, &ref)
	if err != nil {
		t.Fatalf("read collection_field_value row: %v", err)
	}
	if num == nil || *num != want {
		t.Errorf("collection_field_value.value_num = %v, want %v — this path wrote "+
			"\"true\"/\"false\" into value_text until #791, putting the same field's "+
			"value in a different column from the asset side", num, want)
	}
	if text != nil {
		t.Errorf("collection_field_value.value_text = %q, want NULL", *text)
	}
	if opts != nil {
		t.Errorf("collection_field_value.value_options = %v, want NULL", opts)
	}
	if ref != nil {
		t.Errorf("collection_field_value.value_ref = %v, want NULL", *ref)
	}
}
