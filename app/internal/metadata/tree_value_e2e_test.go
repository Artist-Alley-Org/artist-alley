// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the `tree` field type (#778).
//
// Six call sites had drifted into three different answers about where
// a tree value is stored, and every one of them looked fine in
// isolation — the asset writer and the seeder agreed with each other,
// the collection writer and the editor agreed with each other, and the
// display agreed with nobody. Unit-testing each writer against its own
// assumption is exactly the shape of test that lets that happen.
//
// So this file drives the whole path instead: define a real tree field
// through the API, give it a value on an asset AND on a collection,
// then assert against the DATABASE COLUMNS and against the read model
// the display surface actually consumes. Nothing here trusts a
// writer's opinion of what it wrote.
package metadata_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// treeVocabulary is a three-level hierarchy with labels that differ
// from their slugs — so a test that accidentally asserts on the slug
// cannot pass by coincidence.
func treeVocabulary() map[string]any {
	return map[string]any{
		"values": []any{
			map[string]any{
				"value": "europe", "label": "Europe",
				"children": []any{
					map[string]any{
						"value": "uk", "label": "United Kingdom",
						"children": []any{
							map[string]any{"value": "london", "label": "London"},
						},
					},
				},
			},
			map[string]any{"value": "asia", "label": "Asia"},
		},
	}
}

// TestTreeValueEndToEnd is acceptance item 2: a tree field can be
// created, valued on both subject kinds, and read back correctly.
func TestTreeValueEndToEnd(t *testing.T) {
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

	// ── The field definition ────────────────────────────────────────
	// A nested options document is accepted as-is. Before #778 nothing
	// had ever created one through the API.
	assetFieldID := mustCreateField(t, router, map[string]any{
		"code":    "mtv_region",
		"label":   "Region",
		"type":    "tree",
		"options": treeVocabulary(),
	})

	assetID := mustInsertAsset(t, pool, userRef)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assetID)
	})

	// ── Asset side: write ───────────────────────────────────────────
	// The stored value is the LEAF SLUG, not the path. "london" alone
	// addresses the node because slugs are unique tree-wide.
	rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, assetFieldID),
		map[string]any{"value_text": "london"})
	if rr.Code != http.StatusOK {
		t.Fatalf("set asset tree value: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// ── Asset side: the actual columns ──────────────────────────────
	// Asserting on the response would only tell us what the handler
	// echoed back. Ask the table.
	assertAssetTreeColumns(t, pool, assetID, assetFieldID)

	// ── Asset side: read, the way the display surface reads ─────────
	values := getAssetFields(t, router, assetID)
	v := findAssetValue(t, values, assetFieldID)

	if v.ValueText == nil || *v.ValueText != "london" {
		t.Errorf("asset value_text = %v, want \"london\"", v.ValueText)
	}
	if v.ValueOptions != nil {
		t.Errorf("asset value_options = %v, want nil — a tree value is one slug, not a set", *v.ValueOptions)
	}
	if v.ValueRef != nil {
		t.Errorf("asset value_ref = %v, want nil — value_ref is for `reference`, and no writer "+
			"has ever populated it for a tree field, which is why the display rendered blank", *v.ValueRef)
	}

	// This is the assertion that would have caught the display bug:
	// the surface needs a resolved label + path, and got neither
	// because `tree` was excluded from resolution entirely.
	if v.ResolvedOptions == nil {
		t.Fatal("asset tree value shipped no resolved_options — the display surface has no field " +
			"definition and so cannot render anything but the raw slug")
	}
	opt, ok := (*v.ResolvedOptions)["london"]
	if !ok {
		t.Fatalf("resolved_options has no entry for the stored slug: %+v", *v.ResolvedOptions)
	}
	if opt.Label != "London" {
		t.Errorf("resolved label = %q, want \"London\"", opt.Label)
	}
	if opt.Path == nil {
		t.Fatal("resolved option shipped no path — the hierarchy is invisible without it")
	}
	assertStrings(t, "asset resolved path", *opt.Path, []string{"Europe", "United Kingdom", "London"})

	// ── Asset side: the wrong shape is rejected, not stored ─────────
	bad := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, assetFieldID),
		map[string]any{"value_options": []string{"europe", "uk", "london"}})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("asset tree write with value_options: status=%d want 400 body=%s",
			bad.Code, bad.Body.String())
	}

	// ── Collection side: same field type, same column ───────────────
	collFieldID := mustCreateCollectionField(t, router, "mcoltest_region", "Region", "tree")
	patched := patchJSON(t, router, "/fields/"+collFieldID, map[string]any{
		"options": treeVocabulary(),
	})
	if patched.Code != http.StatusOK {
		t.Fatalf("attach vocabulary to collection field: status=%d body=%s",
			patched.Code, patched.Body.String())
	}

	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest tree col")

	rr = putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, collFieldID),
		map[string]any{"value_text": "london"})
	if rr.Code != http.StatusOK {
		t.Fatalf("set collection tree value: status=%d body=%s", rr.Code, rr.Body.String())
	}

	assertCollectionTreeColumns(t, pool, collectionID, collFieldID)

	// The collection write path used to REQUIRE this shape and store it
	// in value_options — so before #778 this request succeeded and the
	// value landed in a different column from the asset's.
	//
	// 422, not the asset side's 400: the collection operation has only
	// ever declared 422 for value_type_mismatch. The two paths differ,
	// and openapi.yaml's prose claimed 400 for both until #778.
	badColl := putJSON(t, router,
		fmt.Sprintf("/collections/%s/fields/%s", collectionID, collFieldID),
		map[string]any{"value_options": []string{"europe", "uk", "london"}})
	if badColl.Code != http.StatusUnprocessableEntity {
		t.Errorf("collection tree write with value_options: status=%d want 422 body=%s",
			badColl.Code, badColl.Body.String())
	}

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
	if collValues[0].ValueText == nil || *collValues[0].ValueText != "london" {
		t.Errorf("collection value_text = %v, want \"london\"", collValues[0].ValueText)
	}
	if collValues[0].ValueOptions != nil {
		t.Errorf("collection value_options = %v, want nil", *collValues[0].ValueOptions)
	}

	// ── The cross-surface invariant, stated plainly ─────────────────
	// Same field type, same value, same column on both sides. This is
	// the property #778 broke, and it is worth asserting directly
	// rather than inferring from the two halves above.
	assertSameStoredColumn(t, pool, assetID, assetFieldID, collectionID, collFieldID)
}

// TestTreeAncestorRenameDoesNotRewriteValues is acceptance item 4,
// end-to-end: renaming an ancestor is an options-document edit and
// must not touch a single stored value.
//
// Had the value been the "europe/uk/london" path string ADR 0012
// originally specified, this rename would have required finding and
// rewriting every descendant's row — the cascade the prior art pays
// and the slug indirection exists to avoid.
func TestTreeAncestorRenameDoesNotRewriteValues(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, router, map[string]any{
		"code":    "mtv_rename_region",
		"label":   "Region",
		"type":    "tree",
		"options": treeVocabulary(),
	})

	assetID := mustInsertAsset(t, pool, userRef)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assetID)
	})

	if rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, fieldID),
		map[string]any{"value_text": "london"}); rr.Code != http.StatusOK {
		t.Fatalf("set value: %d %s", rr.Code, rr.Body.String())
	}

	// Snapshot the raw stored value and the row's mtime so we can prove
	// the rename did not touch the row at all.
	storedBefore, updatedBefore := readAssetValueRow(t, pool, assetID, fieldID)

	// Rename an ANCESTOR's label. Nothing about the leaf changes.
	renamed := map[string]any{
		"values": []any{
			map[string]any{
				"value": "europe", "label": "Europe (EU)",
				"children": []any{
					map[string]any{
						"value": "uk", "label": "Great Britain",
						"children": []any{
							map[string]any{"value": "london", "label": "London"},
						},
					},
				},
			},
			map[string]any{"value": "asia", "label": "Asia"},
		},
	}
	if rr := patchJSON(t, router, "/fields/"+fieldID,
		map[string]any{"options": renamed}); rr.Code != http.StatusOK {
		t.Fatalf("rename ancestors: %d %s", rr.Code, rr.Body.String())
	}

	storedAfter, updatedAfter := readAssetValueRow(t, pool, assetID, fieldID)
	if storedAfter != storedBefore {
		t.Errorf("stored value changed from %q to %q — renaming an ancestor must rewrite "+
			"NOTHING in asset_field_value", storedBefore, storedAfter)
	}
	if !updatedAfter.Equal(updatedBefore) {
		t.Errorf("value row was touched (set_at %v → %v) by an options-only edit",
			updatedBefore, updatedAfter)
	}

	// And the read surface reflects the new labels immediately, with no
	// backfill, because the path is derived rather than stored.
	values := getAssetFields(t, router, assetID)
	v := findAssetValue(t, values, fieldID)
	if v.ResolvedOptions == nil {
		t.Fatal("no resolved_options after rename")
	}
	opt := (*v.ResolvedOptions)["london"]
	if opt.Path == nil {
		t.Fatal("no path after rename")
	}
	assertStrings(t, "path after ancestor rename", *opt.Path,
		[]string{"Europe (EU)", "Great Britain", "London"})
}

// ---------------------------------------------------------------------------
// Column-level assertions — the point of this file
// ---------------------------------------------------------------------------

func assertAssetTreeColumns(t *testing.T, pool *pgxpool.Pool, assetID, fieldID string) {
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
	if text == nil || *text != "london" {
		t.Errorf("asset_field_value.value_text = %v, want \"london\"", text)
	}
	if opts != nil {
		t.Errorf("asset_field_value.value_options = %v, want NULL", opts)
	}
	if ref != nil {
		t.Errorf("asset_field_value.value_ref = %v, want NULL", *ref)
	}
	if num != nil {
		t.Errorf("asset_field_value.value_num = %v, want NULL", *num)
	}
}

func assertCollectionTreeColumns(t *testing.T, pool *pgxpool.Pool, collectionID, fieldID string) {
	t.Helper()
	var text *string
	var opts []string
	var ref *string
	err := pool.QueryRow(context.Background(), `
		SELECT value_text, value_options, value_ref::text
		  FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&text, &opts, &ref)
	if err != nil {
		t.Fatalf("read collection_field_value row: %v", err)
	}
	if text == nil || *text != "london" {
		t.Errorf("collection_field_value.value_text = %v, want \"london\" — "+
			"this path used to write value_options, putting the same field's value "+
			"in a different column from the asset side", text)
	}
	if opts != nil {
		t.Errorf("collection_field_value.value_options = %v, want NULL", opts)
	}
	if ref != nil {
		t.Errorf("collection_field_value.value_ref = %v, want NULL", *ref)
	}
}

// assertSameStoredColumn proves the invariant directly: ask each table
// which of its value_* columns is non-NULL and require the same answer.
func assertSameStoredColumn(t *testing.T, pool *pgxpool.Pool, assetID, assetFieldID, collectionID, collFieldID string) {
	t.Helper()
	const colExpr = `CASE
		WHEN value_text    IS NOT NULL THEN 'value_text'
		WHEN value_num     IS NOT NULL THEN 'value_num'
		WHEN value_date    IS NOT NULL THEN 'value_date'
		WHEN value_options IS NOT NULL THEN 'value_options'
		WHEN value_ref     IS NOT NULL THEN 'value_ref'
		ELSE 'none' END`

	var assetCol, collCol string
	if err := pool.QueryRow(context.Background(),
		`SELECT `+colExpr+` FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		assetID, assetFieldID).Scan(&assetCol); err != nil {
		t.Fatalf("asset column probe: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT `+colExpr+` FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		collectionID, collFieldID).Scan(&collCol); err != nil {
		t.Fatalf("collection column probe: %v", err)
	}
	if assetCol != collCol {
		t.Errorf("a tree value is in %s on an asset but %s on a collection — "+
			"that is bug #778: the same field type stored two different ways, so a value "+
			"written through one surface is invisible to anything reading the other",
			assetCol, collCol)
	}
	if assetCol != "value_text" {
		t.Errorf("tree value stored in %s, want value_text", assetCol)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func readAssetValueRow(t *testing.T, pool *pgxpool.Pool, assetID, fieldID string) (string, time.Time) {
	t.Helper()
	var text *string
	var setAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT value_text, set_at FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		assetID, fieldID).Scan(&text, &setAt); err != nil {
		t.Fatalf("read value row: %v", err)
	}
	if text == nil {
		t.Fatal("value_text is NULL")
	}
	return *text, setAt
}

func getAssetFields(t *testing.T, router interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, assetID string) []openapi.AssetFieldValue {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/assets/%s/fields", assetID), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET asset fields: %d %s", rr.Code, rr.Body.String())
	}
	var values []openapi.AssetFieldValue
	mustDecode(t, rr.Body.Bytes(), &values)
	return values
}

func findAssetValue(t *testing.T, values []openapi.AssetFieldValue, fieldID string) openapi.AssetFieldValue {
	t.Helper()
	for _, v := range values {
		if v.FieldId.String() == fieldID {
			return v
		}
	}
	t.Fatalf("field %s not present in %d returned values", fieldID, len(values))
	return openapi.AssetFieldValue{}
}

func assertStrings(t *testing.T, where string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", where, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", where, got, want)
			return
		}
	}
}
