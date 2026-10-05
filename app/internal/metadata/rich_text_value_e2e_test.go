// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the `rich_text` sanitisation boundary (#816).
//
// rich_text is the one stored value a client renders as markup rather
// than as text, so it is the one value where "what is in the column"
// and "what runs in a reader's browser" are the same question. The
// policy lives in internal/richtext and is unit-tested there; this
// file pins that it is actually WIRED, at both boundaries, through the
// real router and the real database.
//
// The two directions are deliberately separate tests, because they
// fail for different reasons and only one of them is obvious:
//
//   - the write side is what keeps a live payload out of the column;
//   - the read side is what covers every writer that is not a handler
//     — the seed's direct INSERT, an import, a hand-edit, a peer.
//
// Every test here is named so that deleting a sanitiser call makes the
// failure say which one went. If you are reading this because one of
// them is red, the sanitiser is not being called; do not "fix" the
// expectation.
package metadata_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// dirtyRichText is the canary payload: a script element, an inline
// event handler, and a javascript: href — the three shapes an XSS in
// a metadata panel actually takes. `window.__xss` is the same canary
// the Playwright test installs, so the string is greppable across both
// halves of the proof.
const dirtyRichText = `<p>Cleared for <strong>internal</strong> use.</p>` +
	`<script>window.__xss && window.__xss('script')</script>` +
	`<img src=x onerror="window.__xss && window.__xss('onerror')">` +
	`<a href="javascript:window.__xss && window.__xss('href')">terms</a>` +
	`<a href="https://example.test/licence">licence</a>`

// assertNeutralised is the shared verdict. It is intentionally about
// what must NOT be there plus what must survive: a sanitiser that
// returned the empty string would pass a half-written check.
func assertNeutralised(t *testing.T, where, got string) {
	t.Helper()
	for _, forbidden := range []string{"__xss", "<script", "onerror", "javascript:"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(forbidden)) {
			t.Errorf("%s still contains %q — the sanitiser is not on this path:\n%s",
				where, forbidden, got)
		}
	}
	// The formatting the field exists for has to survive the scrub.
	for _, want := range []string{"<p>", "<strong>internal</strong>", "Cleared for"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s lost %q — sanitising is not supposed to be deleting:\n%s",
				where, want, got)
		}
	}
	// The one good link keeps its href and gains the rel.
	if !strings.Contains(got, `href="https://example.test/licence"`) {
		t.Errorf("%s dropped the https link:\n%s", where, got)
	}
	if !strings.Contains(got, `rel="noopener noreferrer"`) {
		t.Errorf("%s did not force rel=\"noopener noreferrer\" on the surviving link:\n%s",
			where, got)
	}
}

// TestRichTextWriteSideStoresSanitisedHTML is the write-side proof.
//
// It reads the row back RAW, straight off the pool, rather than
// through the API — going back out through the read path would prove
// only that one of the two hooks exists, and the whole point of having
// two is that neither is load-bearing on its own.
func TestRichTextWriteSideStoresSanitisedHTML(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, router, map[string]any{
		"code":  "metadata_test_usage_rights",
		"label": "Usage rights",
		"type":  "rich_text",
	})
	assetID := mustInsertAsset(t, pool, userRef)
	cleanupAssets(t, pool, assetID)

	rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, fieldID),
		map[string]any{"value_text": dirtyRichText})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT rich_text value: status=%d body=%s", rr.Code, rr.Body.String())
	}

	stored := readRawValueText(t, pool, assetID, fieldID)
	assertNeutralised(t, "the stored row", stored)

	// The PUT response is a read of the value it just wrote, and it
	// goes through the same DTO builder the list path does (#775). If
	// the two ever disagree about rich_text it will be here.
	var put openapi.AssetFieldValue
	mustDecode(t, rr.Body.Bytes(), &put)
	if put.ValueText == nil {
		t.Fatal("PUT response has no value_text")
	}
	assertNeutralised(t, "the PUT response", *put.ValueText)
	if *put.ValueText != stored {
		t.Errorf("PUT response and stored row disagree:\n stored %q\n wire   %q", stored, *put.ValueText)
	}
}

// TestRichTextReadSideSanitisesValuesWrittenAroundTheHandler is the
// read-side proof, and it is the one that matters most.
//
// The INSERT here is deliberately the shape the seed runner uses —
// straight at asset_field_value, no handler, no gate — because that
// path is real (SeedInsertAssetFieldValue) and because an import or a
// federated peer will look exactly the same. If this test is the only
// one failing, the write hook is fine and a value nobody sanitised on
// the way in is going out live.
func TestRichTextReadSideSanitisesValuesWrittenAroundTheHandler(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, router, map[string]any{
		"code":  "metadata_test_usage_rights",
		"label": "Usage rights",
		"type":  "rich_text",
	})
	assetID := mustInsertAsset(t, pool, userRef)
	cleanupAssets(t, pool, assetID)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by)
		 VALUES ($1, $2, $3, 'manual')`,
		assetID, fieldID, dirtyRichText); err != nil {
		t.Fatalf("direct insert (the seed's shape): %v", err)
	}
	// Sanity: the column really does hold the payload, so a pass below
	// is the read hook working and not the INSERT having failed
	// quietly.
	if raw := readRawValueText(t, pool, assetID, fieldID); !strings.Contains(raw, "__xss") {
		t.Fatalf("fixture did not land dirty — this test would pass for the wrong reason: %q", raw)
	}

	v := findAssetValue(t, getAssetFields(t, router, assetID), fieldID)
	if v.ValueText == nil {
		t.Fatal("value_text is nil on the read path")
	}
	assertNeutralised(t, "the list response", *v.ValueText)
}

// TestRichTextCollectionValueIsSanitisedOnBothSides is the same pair
// of claims for the collection subject. It is a separate handler with
// its own params builder and its own DTO builder, and the reason both
// call one package rather than each owning a policy is that a divided
// policy is a policy one of them will get wrong.
func TestRichTextCollectionValueIsSanitisedOnBothSides(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() { cleanCollectionTestRows(t, pool) })

	router, userRef := makeRouter(t, pool, true)
	fieldID := mustCreateCollectionField(t, router, "mcoltest_usage_rights", "Usage rights", "rich_text")
	collectionID := mustInsertCollection(t, pool, userRef, "mcoltest col rich")

	rr := putJSON(t, router, fmt.Sprintf("/collections/%s/fields/%s", collectionID, fieldID),
		map[string]any{"value_text": dirtyRichText})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT collection rich_text: status=%d body=%s", rr.Code, rr.Body.String())
	}

	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT value_text FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&stored); err != nil {
		t.Fatalf("read stored collection value: %v", err)
	}
	assertNeutralised(t, "the stored collection row", stored)

	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/collections/%s/fields", collectionID), nil))
	if listRR.Code != http.StatusOK {
		t.Fatalf("GET collection values: %d %s", listRR.Code, listRR.Body.String())
	}
	var values []openapi.CollectionFieldValue
	mustDecode(t, listRR.Body.Bytes(), &values)
	if len(values) != 1 || values[0].ValueText == nil {
		t.Fatalf("got %d collection values, want 1 with value_text", len(values))
	}
	assertNeutralised(t, "the collection list response", *values[0].ValueText)
}

// TestRichTextSanitiserLeavesOtherTextTypesAlone is the blast-radius
// pin. The sanitiser gates on the FIELD TYPE, and a `text` or
// `longtext` value that came back HTML-escaped would be this sprint
// breaking three field types to fix one.
func TestRichTextSanitiserLeavesOtherTextTypesAlone(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	assetID := mustInsertAsset(t, pool, userRef)
	cleanupAssets(t, pool, assetID)

	// A value that a sanitiser would visibly change if it ran: angle
	// brackets, an ampersand, and a tag that is not on the allowlist.
	const literal = `a < b && <div>kept verbatim</div>`

	for _, ft := range []string{"text", "longtext"} {
		fieldID := mustCreateField(t, router, map[string]any{
			"code":  "metadata_test_plain_" + ft,
			"label": "Plain " + ft,
			"type":  ft,
		})
		rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, fieldID),
			map[string]any{"value_text": literal})
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT %s: status=%d body=%s", ft, rr.Code, rr.Body.String())
		}
		if stored := readRawValueText(t, pool, assetID, fieldID); stored != literal {
			t.Errorf("%s was rewritten on write:\n got  %q\n want %q", ft, stored, literal)
		}
		v := findAssetValue(t, getAssetFields(t, router, assetID), fieldID)
		if v.ValueText == nil || *v.ValueText != literal {
			t.Errorf("%s was rewritten on read: %v, want %q", ft, v.ValueText, literal)
		}
	}
}

func readRawValueText(t *testing.T, pool *pgxpool.Pool, assetID, fieldID string) string {
	t.Helper()
	var text *string
	if err := pool.QueryRow(context.Background(),
		`SELECT value_text FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		assetID, fieldID).Scan(&text); err != nil {
		t.Fatalf("read raw value_text: %v", err)
	}
	if text == nil {
		t.Fatal("value_text is NULL")
	}
	return *text
}
