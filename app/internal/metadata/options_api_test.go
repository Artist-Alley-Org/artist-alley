// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// TestUpdateFieldConflictDetection covers the ADR 0012 amendment's
// second gap: two admins editing one field's options must not clobber
// each other silently.
//
// The important half is the CONTENT assertion — asserting only the
// status code passes on a 409 that overwrote anyway.
func TestUpdateFieldConflictDetection(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	router, _ := makeRouter(t, pool /*admin=*/, true)
	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	fieldID := mustCreateField(t, router, map[string]any{
		"code":  "metadata_test_conflict",
		"label": "Conflict",
		"type":  "select",
		"options": map[string]any{
			"values": []string{"alpha", "beta"},
		},
	})

	// Both editors load the same baseline.
	baseline := mustGetField(t, router, fieldID)
	stale := baseline.UpdatedAt

	// Editor A saves first — succeeds.
	first := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"if_unchanged_since": stale,
		"options":            map[string]any{"values": []string{"alpha", "beta", "gamma"}},
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first write: status=%d body=%s", first.Code, first.Body.String())
	}

	// Editor B saves with the SAME now-stale baseline — must 409.
	second := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"if_unchanged_since": stale,
		"options":            map[string]any{"values": []string{"only-b-wrote-this"}},
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("stale write: status=%d want 409 body=%s", second.Code, second.Body.String())
	}

	// The conflict body must carry the server-authoritative
	// updated_at so the client can re-baseline rather than guess.
	var conflict openapi.EditConflict
	mustDecode(t, second.Body.Bytes(), &conflict)
	if conflict.Error == "" {
		t.Error("409 body has no error message")
	}
	if conflict.UpdatedAt.Equal(stale) {
		t.Error("409 reported the stale updated_at; client cannot re-baseline from it")
	}

	// THE POINT: editor B's content must not have landed.
	after := mustGetField(t, router, fieldID)
	got := optionSlugs(t, after)
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("after conflict options=%v want %v (B's write leaked through)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("after conflict options=%v want %v (B's write leaked through)", got, want)
		}
	}

	// Re-baselining on the conflict's updated_at lets B retry.
	retry := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"if_unchanged_since": conflict.UpdatedAt,
		"options":            map[string]any{"values": []string{"alpha", "beta", "gamma", "delta"}},
	})
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after re-baseline: status=%d body=%s", retry.Code, retry.Body.String())
	}

	// Omitting the guard entirely stays last-write-wins, so existing
	// single-field callers are unaffected.
	legacy := patchJSON(t, router, "/fields/"+fieldID, map[string]any{"label": "Renamed"})
	if legacy.Code != http.StatusOK {
		t.Fatalf("unguarded write: status=%d body=%s", legacy.Code, legacy.Body.String())
	}
}

// TestUpdateFieldRejectsDanglingReplacement — a replaced_by that names
// nothing is the orphan class ADR 0012 rejects hard deletion for, and
// it must not reach storage.
func TestUpdateFieldRejectsDanglingReplacement(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	router, _ := makeRouter(t, pool /*admin=*/, true)
	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	fieldID := mustCreateField(t, router, map[string]any{
		"code":    "metadata_test_dangling",
		"label":   "Dangling",
		"type":    "select",
		"options": map[string]any{"values": []string{"alpha", "beta"}},
	})

	bad := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"options": map[string]any{"values": []any{
			map[string]any{"value": "alpha", "status": "deprecated", "replaced_by": "ghost"},
			"beta",
		}},
	})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("dangling replaced_by: status=%d want 400 body=%s", bad.Code, bad.Body.String())
	}

	// And nothing was written.
	after := mustGetField(t, router, fieldID)
	if slugs := optionSlugs(t, after); len(slugs) != 2 {
		t.Errorf("rejected write still mutated options: %v", slugs)
	}

	unknownStatus := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"options": map[string]any{"values": []any{
			map[string]any{"value": "alpha", "status": "retired"},
		}},
	})
	if unknownStatus.Code != http.StatusBadRequest {
		t.Errorf("unknown status: status=%d want 400 body=%s",
			unknownStatus.Code, unknownStatus.Body.String())
	}

	// Creating a field with a dangling pointer is rejected too.
	badCreate := postJSON(t, router, "/fields", map[string]any{
		"code":  "metadata_test_dangling_create",
		"label": "Dangling create",
		"type":  "select",
		"options": map[string]any{"values": []any{
			map[string]any{"value": "alpha", "replaced_by": "ghost"},
		}},
	})
	if badCreate.Code != http.StatusBadRequest {
		t.Errorf("dangling replaced_by on create: status=%d want 400 body=%s",
			badCreate.Code, badCreate.Body.String())
	}
}

// TestPreLifecycleOptionsSurviveAnEdit — the five option-carrying
// fields on a live instance hold bare slug strings and no status key.
// Saving through the new editor must leave an untouched vocabulary
// byte-identical, and must keep a deprecated term RESOLVING even
// though it stops being SELECTABLE.
func TestPreLifecycleOptionsSurviveAnEdit(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	router, userRef := makeRouter(t, pool /*admin=*/, true)
	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	// Seed-shaped document: bare strings, no status anywhere.
	fieldID := mustCreateField(t, router, map[string]any{
		"code":    "metadata_test_legacy_opts",
		"label":   "Color space",
		"type":    "select",
		"options": map[string]any{"values": []string{"sRGB", "Linear", "Raw", "N/A"}},
	})

	stored := storedOptions(t, pool, fieldID)
	if stored != `{"values": ["sRGB", "Linear", "Raw", "N/A"]}` &&
		stored != `{"values":["sRGB","Linear","Raw","N/A"]}` {
		t.Logf("stored form: %s", stored)
	}

	// An asset already carries the value we are about to deprecate.
	assetID := mustInsertAsset(t, pool, userRef)
	setRR := putJSON(t, router, "/assets/"+assetID+"/fields/"+fieldID,
		map[string]any{"value_text": "Raw"})
	if setRR.Code != http.StatusOK && setRR.Code != http.StatusCreated {
		t.Fatalf("set asset value: status=%d body=%s", setRR.Code, setRR.Body.String())
	}

	// A pure round-trip — save the document back unchanged.
	before := mustGetField(t, router, fieldID)
	rt := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"if_unchanged_since": before.UpdatedAt,
		"options":            before.Options,
	})
	if rt.Code != http.StatusOK {
		t.Fatalf("round-trip save: status=%d body=%s", rt.Code, rt.Body.String())
	}
	if got, want := storedOptions(t, pool, fieldID), stored; got != want {
		t.Errorf("round-trip changed the stored document\n got: %s\nwant: %s", got, want)
	}

	// Now deprecate "Raw" with a successor, relabel "N/A", and leave
	// the rest alone.
	cur := mustGetField(t, router, fieldID)
	edit := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"if_unchanged_since": cur.UpdatedAt,
		"options": map[string]any{"values": []any{
			"sRGB",
			"Linear",
			map[string]any{"value": "Raw", "status": "deprecated", "replaced_by": "Linear"},
			map[string]any{"value": "N/A", "label": "Not applicable"},
		}},
	})
	if edit.Code != http.StatusOK {
		t.Fatalf("deprecate: status=%d body=%s", edit.Code, edit.Body.String())
	}

	// Untouched entries stay bare strings — the vocabulary does not
	// bloat just because one term grew a lifecycle.
	after := mustGetField(t, router, fieldID)
	raw, _ := json.Marshal((*after.Options)["values"])
	var entries []any
	mustDecode(t, raw, &entries)
	if s, ok := entries[0].(string); !ok || s != "sRGB" {
		t.Errorf("untouched entry was rewritten as %#v", entries[0])
	}
	if m, ok := entries[2].(map[string]any); !ok || m["status"] != "deprecated" || m["replaced_by"] != "Linear" {
		t.Errorf("deprecated entry round-tripped as %#v", entries[2])
	}
	if m, ok := entries[3].(map[string]any); !ok || m["label"] != "Not applicable" {
		t.Errorf("relabelled entry round-tripped as %#v", entries[3])
	}

	// THE OTHER HALF: the asset that already carries "Raw" still
	// resolves and displays it. Deprecating stops a term spreading; it
	// must not break the assets already holding it.
	valsRR := httptest.NewRecorder()
	router.ServeHTTP(valsRR, httptest.NewRequest(http.MethodGet, "/assets/"+assetID+"/fields", nil))
	if valsRR.Code != http.StatusOK {
		t.Fatalf("list asset fields: %d", valsRR.Code)
	}
	var vals []openapi.AssetFieldValue
	mustDecode(t, valsRR.Body.Bytes(), &vals)
	found := false
	for _, v := range vals {
		if v.FieldCode == "metadata_test_legacy_opts" {
			found = true
			if v.ValueText == nil || *v.ValueText != "Raw" {
				t.Errorf("deprecated value no longer resolves: %+v", v.ValueText)
			}
		}
	}
	if !found {
		t.Error("the asset's value vanished after its option was deprecated")
	}
}

// --- local helpers ---------------------------------------------------------

func mustGetField(t *testing.T, r chi.Router, id string) openapi.FieldDefinition {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/fields/"+id, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("get field: %d body=%s", rr.Code, rr.Body.String())
	}
	var def openapi.FieldDefinition
	mustDecode(t, rr.Body.Bytes(), &def)
	return def
}

// optionSlugs flattens the values array to its slugs, tolerating both
// the bare-string and object entry shapes.
func optionSlugs(t *testing.T, def openapi.FieldDefinition) []string {
	t.Helper()
	if def.Options == nil {
		return nil
	}
	raw, err := json.Marshal((*def.Options)["values"])
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		var s string
		if err := json.Unmarshal(e, &s); err == nil {
			out = append(out, s)
			continue
		}
		var o struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(e, &o); err == nil {
			out = append(out, o.Value)
		}
	}
	return out
}

func storedOptions(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT options::text FROM field_definition WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read stored options: %v", err)
	}
	return s
}
