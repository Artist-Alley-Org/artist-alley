// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for what the tree editor writes (#779 / #825).
//
// The editor is a frontend surface, and its own invariants are unit
// tested next to it (web/src/lib/fieldOptions.test.ts). What CANNOT be
// tested there is whether the document it produces survives contact
// with the server: whether a reparent leaves stored values alone,
// whether a duplicate slug at depth is refused, and what happens when
// the editor's whole-document save races the open-vocabulary mint that
// rewrites the same column. All three are properties of the write
// path, so all three are driven through the real handlers here.
package metadata_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestTreeReparentKeepsValuesResolving — the reparent half of #779.
//
// Moving a term to a different branch is an options-document edit and
// nothing more: a tree value stores ONE slug (ADR 0012's 2026-07-31
// amendment) and slugs are unique tree-wide, so the term is a complete
// address wherever it sits. If that ever stopped being true, reparent
// would need a data migration and could not be offered as a plain
// editor gesture — so the assertion is on the untouched value ROW, not
// merely on the value resolving.
func TestTreeReparentKeepsValuesResolving(t *testing.T) {
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
		"code":    "mtv_reparent_region",
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
	storedBefore, updatedBefore := readAssetValueRow(t, pool, assetID, fieldID)

	// The move the editor performs: `uk` (carrying `london`) leaves
	// Europe and lands under Asia. Same slugs, different nesting —
	// which is exactly the document the destination picker builds.
	reparented := map[string]any{
		"values": []any{
			map[string]any{"value": "europe", "label": "Europe"},
			map[string]any{
				"value": "asia", "label": "Asia",
				"children": []any{
					map[string]any{
						"value": "uk", "label": "United Kingdom",
						"children": []any{
							map[string]any{"value": "london", "label": "London"},
						},
					},
				},
			},
		},
	}
	if rr := patchJSON(t, router, "/fields/"+fieldID,
		map[string]any{"options": reparented}); rr.Code != http.StatusOK {
		t.Fatalf("reparent: %d %s", rr.Code, rr.Body.String())
	}

	storedAfter, updatedAfter := readAssetValueRow(t, pool, assetID, fieldID)
	if storedAfter != storedBefore {
		t.Errorf("stored value changed from %q to %q — moving a term must rewrite "+
			"NOTHING in asset_field_value", storedBefore, storedAfter)
	}
	if !updatedAfter.Equal(updatedBefore) {
		t.Errorf("value row was touched (set_at %v → %v) by a move", updatedBefore, updatedAfter)
	}

	// The read model reassembles the path from the NEW position, with
	// no backfill anywhere.
	values := getAssetFields(t, router, assetID)
	v := findAssetValue(t, values, fieldID)
	if v.ResolvedOptions == nil {
		t.Fatal("no resolved_options after the move")
	}
	opt := (*v.ResolvedOptions)["london"]
	if opt.Path == nil {
		t.Fatal("no path after the move")
	}
	assertStrings(t, "path after reparent", *opt.Path,
		[]string{"Asia", "United Kingdom", "London"})

	// A branch left childless is still a term, still selectable, and
	// still the thing an asset holding `europe` resolves to.
	after := mustGetField(t, router, fieldID)
	if slugs := optionSlugs(t, after); len(slugs) != 2 || slugs[0] != "europe" {
		t.Errorf("top level after the move = %v, want europe then asia", slugs)
	}
}

// TestTreeEditorRejectsDuplicateSlugAtDepth — the failure a reparent
// bug produces, stated as the server's answer to it.
//
// A move implemented as copy-then-forget-to-remove, or a subtree
// spliced into itself, leaves one slug at two depths. No same-level
// scan sees that; NormalizeOptionsDoc does, and the message names the
// offending term so the editor can show the operator which one.
func TestTreeEditorRejectsDuplicateSlugAtDepth(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, _ := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, router, map[string]any{
		"code":    "mtv_dupdepth_region",
		"label":   "Region",
		"type":    "tree",
		"options": treeVocabulary(),
	})

	// `london` under europe/uk AND again at the top level: the shape a
	// duplicating move produces.
	forged := map[string]any{
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
			map[string]any{"value": "london", "label": "London"},
		},
	}
	rr := patchJSON(t, router, "/fields/"+fieldID, map[string]any{"options": forged})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("duplicate at depth: status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
	// The editor renders `error` from the response body verbatim, so
	// the body has to be legible on its own — a bare "bad request"
	// would leave the operator hunting for the term.
	body := rr.Body.String()
	if !strings.Contains(body, "duplicate option value") || !strings.Contains(body, "london") {
		t.Errorf("400 body %s does not name the duplicate term; the editor shows this "+
			"string to the operator unchanged", body)
	}

	// And nothing landed.
	after := mustGetField(t, router, fieldID)
	if slugs := optionSlugs(t, after); len(slugs) != 2 {
		t.Errorf("rejected write still mutated options: %v", slugs)
	}
}

// TestFieldEditConflictsWithOpenVocabularyMint — the mint race, pinned
// at the API level.
//
// #737 records a residual last-write-wins gap between the admin
// editor's whole-document PATCH and EnsureOpenVocabularyTerms, which
// rewrites the same column under a row lock the editor does not take.
// For an editor that sends `if_unchanged_since` — which is every save
// the shipped editor makes — that residual is NOT a silent clobber: the
// mint bumps `updated_at` (SetFieldDefinitionOptions), so the editor's
// baseline is stale and the write comes back 409 with the minted term
// intact. That is a detected conflict, and the operator is offered the
// reload.
//
// Pinned on `multi_select` rather than `tree` deliberately:
// openVocabularyApplies honours the flag on multi_select ALONE, so a
// tree-field variant of this race is not constructible today. If tree
// is ever opened, this test is the one to widen.
func TestFieldEditConflictsWithOpenVocabularyMint(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "mintrace", openVocabSet())

	// The admin opens the editor: this is the baseline it holds.
	baseline := mustGetField(t, env.router, fid).UpdatedAt

	// Meanwhile an ordinary value save mints a term. Nobody thinks of
	// this as editing the field, which is precisely why it is
	// dangerous.
	status, _ := env.putAsset(t, fid, map[string]any{
		"value_options": []string{"Minted While Editing"},
	})
	if status != http.StatusOK {
		t.Fatalf("mint write: status=%d", status)
	}
	if findOption(env.fieldOptions(t, fid), "minted-while-editing") == nil {
		t.Fatalf("the term was not minted; nothing to race with")
	}

	// The admin now saves the document they loaded BEFORE the mint —
	// three terms, no `minted-while-editing`. Unguarded, this is the
	// clobber. Guarded, it is a 409.
	stale := patchJSON(t, env.router, "/fields/"+fid, map[string]any{
		"if_unchanged_since": baseline,
		"options": map[string]any{"values": []any{
			map[string]any{"value": "abstract", "label": "Abstract Renamed"},
			map[string]any{"value": "black-and-white", "label": "Black and White"},
			map[string]any{"value": "long-exposure", "label": "Long Exposure"},
		}},
	})
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale save after a mint: status=%d want 409 body=%s",
			stale.Code, stale.Body.String())
	}

	// THE POINT: the minted term survived. Asserting only the status
	// code would pass on a 409 that overwrote anyway.
	if findOption(env.fieldOptions(t, fid), "minted-while-editing") == nil {
		t.Fatalf("the minted term was clobbered by a save that reported a conflict; "+
			"options = %v", env.fieldOptions(t, fid))
	}
	if opt := findOption(env.fieldOptions(t, fid), "abstract"); opt != nil &&
		opt["label"] == "Abstract Renamed" {
		t.Error("the refused edit's content landed anyway")
	}

	// And the operator's escape hatch works: re-baseline on the
	// conflict's updated_at and the deliberate overwrite goes through.
	var conflict struct {
		UpdatedAt string `json:"updated_at"`
	}
	mustDecode(t, stale.Body.Bytes(), &conflict)
	if conflict.UpdatedAt == "" {
		t.Fatal("409 carried no updated_at; the client cannot re-baseline")
	}
	retry := patchJSON(t, env.router, "/fields/"+fid, map[string]any{
		"if_unchanged_since": conflict.UpdatedAt,
		"options": map[string]any{"values": []any{
			map[string]any{"value": "abstract", "label": "Abstract"},
			map[string]any{"value": "black-and-white", "label": "Black and White"},
			map[string]any{"value": "long-exposure", "label": "Long Exposure"},
			map[string]any{"value": "minted-while-editing", "label": "Minted While Editing"},
		}},
	})
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after re-baseline: status=%d body=%s", retry.Code, retry.Body.String())
	}
}
