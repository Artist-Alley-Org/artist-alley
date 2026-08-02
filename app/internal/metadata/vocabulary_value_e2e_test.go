// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the controlled-vocabulary write gate (#824).
//
// The bug: `PUT country {"value_text":"atlantis"}` returned 200. The
// slug was stored, never resolved, and rendered as the raw string on
// every read surface — a value that looks approximately right and is
// unaddressable. `select`, `multi_select` and `tree` all behaved the
// same way.
//
// These tests drive the real HTTP handlers rather than calling
// checkVocabulary directly, because the failure this closes was never
// in the membership walk — validateDefaultSlugs had one and it was
// correct. It was that the value path never CALLED one. A unit test on
// the helper passes just as happily when nothing is wired to it.
//
// The asset and collection writers are separate handlers, so every
// rule here is asserted on BOTH, and one test asserts their refusals
// are byte-identical in shape.
package metadata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Vocabularies
// ---------------------------------------------------------------------------

// vocabFlat is a flat vocabulary whose labels differ from their slugs,
// so a test cannot pass by matching the display text by coincidence.
func vocabFlat(deprecated ...string) map[string]any {
	dep := map[string]bool{}
	for _, s := range deprecated {
		dep[s] = true
	}
	entry := func(value, label string) map[string]any {
		e := map[string]any{"value": value, "label": label}
		if dep[value] {
			e["status"] = "deprecated"
		}
		return e
	}
	return map[string]any{"values": []any{
		entry("srgb", "sRGB"),
		entry("linear", "Linear"),
		entry("rec709", "Rec. 709"),
	}}
}

// vocabSet is the multi_select vocabulary.
func vocabSet(deprecated ...string) map[string]any {
	dep := map[string]bool{}
	for _, s := range deprecated {
		dep[s] = true
	}
	entry := func(value, label string) map[string]any {
		e := map[string]any{"value": value, "label": label}
		if dep[value] {
			e["status"] = "deprecated"
		}
		return e
	}
	return map[string]any{"values": []any{
		entry("alpha", "Alpha"),
		entry("beta", "Beta"),
		entry("gamma", "Gamma"),
	}}
}

// vocabTree nests three levels deep. `london` is a leaf, `europe` and
// `uk` are branches — the gate must accept all three, because the
// picker offers every term at every depth (selectableTreeOptions,
// web/src/lib/fieldOptions.ts: "a branch is a legitimate answer, not
// just a leaf").
func vocabTree() map[string]any {
	return map[string]any{"values": []any{
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
	}}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type vocabEnv struct {
	pool    *pgxpool.Pool
	router  chi.Router
	assetID string
	collID  string
	userRef int64
}

func newVocabEnv(t *testing.T) *vocabEnv {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestFields(t, pool)
	cleanCollectionTestRows(t, pool)
	t.Cleanup(func() {
		cleanTestFields(t, pool)
		cleanCollectionTestRows(t, pool)
	})

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	assetID := mustInsertAsset(t, pool, userRef)
	collID := mustInsertCollection(t, pool, userRef, "mcoltest col vocab")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assetID)
	})
	return &vocabEnv{pool: pool, router: router, assetID: assetID, collID: collID, userRef: userRef}
}

// assetField defines an asset-side field. Codes use the `mtv_` prefix
// cleanTestFields already sweeps.
func (e *vocabEnv) assetField(t *testing.T, code, ftype string, options map[string]any) string {
	t.Helper()
	return mustCreateField(t, e.router, map[string]any{
		"code": "mtv_" + code, "label": code, "type": ftype, "options": options,
	})
}

// collectionField defines the collection-side twin. Codes use the
// `mcoltest_` prefix cleanCollectionTestRows sweeps.
func (e *vocabEnv) collectionField(t *testing.T, code, ftype string, options map[string]any) string {
	t.Helper()
	return mustCreateField(t, e.router, map[string]any{
		"code": collectionTestPrefix + code, "label": code, "type": ftype,
		"subject_kind": "collection", "options": options,
	})
}

// retire rewrites a field's vocabulary, which is how a term becomes
// deprecated in real life — the operator edits the definition long
// after records started carrying the term.
func (e *vocabEnv) retire(t *testing.T, fieldID string, options map[string]any) {
	t.Helper()
	rr := patchJSON(t, e.router, "/fields/"+fieldID, map[string]any{"options": options})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch field options: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func (e *vocabEnv) putAsset(t *testing.T, fieldID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	rr := putJSON(t, e.router, fmt.Sprintf("/assets/%s/fields/%s", e.assetID, fieldID), body)
	return rr.Code, decodeBody(t, rr.Body.Bytes())
}

func (e *vocabEnv) putCollection(t *testing.T, fieldID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	rr := putJSON(t, e.router, fmt.Sprintf("/collections/%s/fields/%s", e.collID, fieldID), body)
	return rr.Code, decodeBody(t, rr.Body.Bytes())
}

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, b)
	}
	return m
}

// assertRejected checks the 422 contract: the status, the machine
// reason, and that the body names BOTH the field code and the
// offending slug. The last part is the difference between an error an
// operator can act on and one that just says no.
func assertRejected(t *testing.T, status int, body map[string]any, wantReason, wantField, wantOption string) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422; body=%v", status, body)
	}
	if got := body["reason"]; got != wantReason {
		t.Errorf("reason=%v want %q", got, wantReason)
	}
	if got := body["field"]; got != wantField {
		t.Errorf("field=%v want %q — the body must name which field refused", got, wantField)
	}
	if got := body["option"]; got != wantOption {
		t.Errorf("option=%v want %q — the body must name the offending slug", got, wantOption)
	}
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Error("error message empty")
	}
}

// ---------------------------------------------------------------------------
// Unknown slugs
// ---------------------------------------------------------------------------

// TestVocabularyGate_UnknownSlugRejected is the issue's reproduction,
// on all three vocabulary types and both subject kinds. Every one of
// these returned 200 before #824.
func TestVocabularyGate_UnknownSlugRejected(t *testing.T) {
	env := newVocabEnv(t)

	cases := []struct {
		name    string
		ftype   string
		options map[string]any
		write   map[string]any
		option  string
	}{
		{"select", "select", vocabFlat(), map[string]any{"value_text": "atlantis"}, "atlantis"},
		{"tree", "tree", vocabTree(), map[string]any{"value_text": "atlantis"}, "atlantis"},
		{"multi_select_all_bogus", "multi_select", vocabSet(),
			map[string]any{"value_options": []string{"not-a-term"}}, "not-a-term"},
		// The subtle one: a set where only ONE element is bogus. A
		// check that looked at the set as a whole, or only at its first
		// element, would let this through.
		{"multi_select_one_bogus", "multi_select", vocabSet(),
			map[string]any{"value_options": []string{"alpha", "not-a-term", "beta"}}, "not-a-term"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			afid := env.assetField(t, "unk_"+c.name, c.ftype, c.options)
			status, body := env.putAsset(t, afid, c.write)
			assertRejected(t, status, body, "unknown_option", "mtv_unk_"+c.name, c.option)

			cfid := env.collectionField(t, "unk_"+c.name, c.ftype, c.options)
			status, body = env.putCollection(t, cfid, c.write)
			assertRejected(t, status, body, "unknown_option", collectionTestPrefix+"unk_"+c.name, c.option)
		})
	}
}

// TestVocabularyGate_NothingWasStored proves the refusal is a refusal
// and not a 422 alongside a successful write — the transaction must
// roll back, leaving no row.
func TestVocabularyGate_NothingWasStored(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "rollback", "select", vocabFlat())

	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "atlantis"}); status != 422 {
		t.Fatalf("status=%d want 422 body=%v", status, body)
	}
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		env.assetID, fid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("asset_field_value rows = %d, want 0 — the write was refused but stored anyway", n)
	}
}

// ---------------------------------------------------------------------------
// Valid slugs still save
// ---------------------------------------------------------------------------

// TestVocabularyGate_ValidSlugsAccepted is the regression guard on the
// gate itself: the point is to refuse bogus slugs, not to refuse
// writes.
//
// The `tree` rows encode the branch-slug answer. A tree accepts a
// value at ANY depth — leaf, mid-branch or root — because that is what
// the picker offers: selectableTreeOptions flattens the whole tree and
// filters only on lifecycle, on the stated grounds that "Europe" is a
// legitimate answer when the operator does not know the city. Matching
// leaves-only here would have made the UI offer values the API
// refuses.
func TestVocabularyGate_ValidSlugsAccepted(t *testing.T) {
	env := newVocabEnv(t)

	cases := []struct {
		name    string
		ftype   string
		options map[string]any
		write   map[string]any
	}{
		{"select", "select", vocabFlat(), map[string]any{"value_text": "srgb"}},
		{"tree_leaf", "tree", vocabTree(), map[string]any{"value_text": "london"}},
		{"tree_mid_branch", "tree", vocabTree(), map[string]any{"value_text": "uk"}},
		{"tree_root_branch", "tree", vocabTree(), map[string]any{"value_text": "europe"}},
		{"multi_select", "multi_select", vocabSet(),
			map[string]any{"value_options": []string{"alpha", "gamma"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			afid := env.assetField(t, "ok_"+c.name, c.ftype, c.options)
			if status, body := env.putAsset(t, afid, c.write); status != http.StatusOK {
				t.Fatalf("asset status=%d want 200; body=%v", status, body)
			}
			cfid := env.collectionField(t, "ok_"+c.name, c.ftype, c.options)
			if status, body := env.putCollection(t, cfid, c.write); status != http.StatusOK {
				t.Fatalf("collection status=%d want 200; body=%v", status, body)
			}
		})
	}
}

// TestVocabularyGate_NonVocabularyTypesUntouched guards the blast
// radius. A `text` field holds whatever the operator types; the gate
// must not have opinions about it.
func TestVocabularyGate_NonVocabularyTypesUntouched(t *testing.T) {
	env := newVocabEnv(t)

	cases := []struct {
		name  string
		ftype string
		write map[string]any
	}{
		{"text", "text", map[string]any{"value_text": "atlantis"}},
		{"longtext", "longtext", map[string]any{"value_text": "atlantis"}},
		{"rich_text", "rich_text", map[string]any{"value_text": "atlantis"}},
		{"number", "number", map[string]any{"value_num": 42}},
		{"boolean", "boolean", map[string]any{"value_num": 1}},
		{"date", "date", map[string]any{"value_date": "2026-08-01T00:00:00Z"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Deliberately WITH an options document, to prove the gate
			// dispatches on the field's TYPE and not on the mere
			// presence of a vocabulary.
			afid := env.assetField(t, "free_"+c.name, c.ftype, vocabFlat())
			if status, body := env.putAsset(t, afid, c.write); status != http.StatusOK {
				t.Fatalf("asset status=%d want 200; body=%v", status, body)
			}
			cfid := env.collectionField(t, "free_"+c.name, c.ftype, vocabFlat())
			if status, body := env.putCollection(t, cfid, c.write); status != http.StatusOK {
				t.Fatalf("collection status=%d want 200; body=%v", status, body)
			}
		})
	}
}

// TestVocabularyGate_ResolvedOptionsUnchanged checks the gate did not
// disturb the read model it sits next to: an accepted value still
// comes back with its resolved label and ancestor path.
func TestVocabularyGate_ResolvedOptionsUnchanged(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "resolve", "tree", vocabTree())

	status, body := env.putAsset(t, fid, map[string]any{"value_text": "london"})
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%v", status, body)
	}
	resolved, ok := body["resolved_options"].(map[string]any)
	if !ok {
		t.Fatalf("no resolved_options on the write response; body=%v", body)
	}
	opt, ok := resolved["london"].(map[string]any)
	if !ok {
		t.Fatalf("london did not resolve; resolved_options=%v", resolved)
	}
	if opt["label"] != "London" {
		t.Errorf("label=%v want London", opt["label"])
	}
	path, _ := opt["path"].([]any)
	if len(path) != 3 {
		t.Errorf("path=%v want 3 elements (Europe / United Kingdom / London)", path)
	}
}

// ---------------------------------------------------------------------------
// The lifecycle rule — grandfathering
// ---------------------------------------------------------------------------

// TestVocabularyGate_DeprecatedGrandfathered_Asset is the pair the
// brief calls the subtle case, on the single-slug types.
//
// Options are never hard-deleted, so a record can legitimately hold a
// term that has since been retired. Re-saving that record must not
// fail — otherwise deprecating a term silently freezes every record
// holding it, which is the exact harm ADR 0012's no-hard-delete rule
// exists to prevent. CHANGING to a retired term is a different act and
// is refused.
func TestVocabularyGate_DeprecatedGrandfathered_Asset(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "grand_asset", "select", vocabFlat())

	// The term is active, and the asset acquires it honestly.
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "linear"}); status != http.StatusOK {
		t.Fatalf("initial write status=%d body=%v", status, body)
	}

	// Time passes; the operator retires `linear` AND `rec709`.
	env.retire(t, fid, vocabFlat("linear", "rec709"))

	// Unchanged save of the value the asset already holds → 200.
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "linear"}); status != http.StatusOK {
		t.Fatalf("unchanged save of a held-but-deprecated term status=%d want 200; body=%v", status, body)
	}

	// Changing to a DIFFERENT deprecated term → 422. The asset does not
	// hold rec709, so nothing grandfathers it.
	status, body := env.putAsset(t, fid, map[string]any{"value_text": "rec709"})
	assertRejected(t, status, body, "option_not_offerable", "mtv_grand_asset", "rec709")

	// Moving OFF the deprecated term onto an active one → 200. The gate
	// must never trap a record on a retired value.
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "srgb"}); status != http.StatusOK {
		t.Fatalf("escape to an active term status=%d want 200; body=%v", status, body)
	}

	// And now that the asset holds `srgb`, `linear` is no longer
	// grandfathered — the grandfather follows the STORED value, not the
	// history.
	status, body = env.putAsset(t, fid, map[string]any{"value_text": "linear"})
	assertRejected(t, status, body, "option_not_offerable", "mtv_grand_asset", "linear")
}

// TestVocabularyGate_DeprecatedGrandfathered_Collection is the same
// pair on the other handler. Separate handlers, so separate proof.
func TestVocabularyGate_DeprecatedGrandfathered_Collection(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.collectionField(t, "grand_coll", "select", vocabFlat())

	if status, body := env.putCollection(t, fid, map[string]any{"value_text": "linear"}); status != http.StatusOK {
		t.Fatalf("initial write status=%d body=%v", status, body)
	}
	env.retire(t, fid, vocabFlat("linear", "rec709"))

	if status, body := env.putCollection(t, fid, map[string]any{"value_text": "linear"}); status != http.StatusOK {
		t.Fatalf("unchanged save status=%d want 200; body=%v", status, body)
	}
	status, body := env.putCollection(t, fid, map[string]any{"value_text": "rec709"})
	assertRejected(t, status, body, "option_not_offerable", collectionTestPrefix+"grand_coll", "rec709")
}

// TestVocabularyGate_MultiSelectGrandfatherIsPerElement pins the
// comparison used for a SET, which is the case with more than one
// defensible answer.
//
// "Unchanged" is per-element MEMBERSHIP in the stored set — not
// equality of the sets. An operator removing other keywords from a set
// that happens to also contain a retired one is changing the value,
// but is not choosing the retired term, and refusing that save would
// make a deprecated keyword impossible to edit around: the only way to
// touch the field would be to drop the grandfathered term in the same
// write, which the UI does not offer.
//
// Set equality is the tempting alternative and this test is what
// distinguishes them — under set equality the middle case below is a
// 422.
func TestVocabularyGate_MultiSelectGrandfatherIsPerElement(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "grand_multi", "multi_select", vocabSet())

	// Acquire all three while every term is active.
	if status, body := env.putAsset(t, fid,
		map[string]any{"value_options": []string{"alpha", "beta", "gamma"}}); status != http.StatusOK {
		t.Fatalf("initial write status=%d body=%v", status, body)
	}

	// `gamma` is retired under the asset.
	env.retire(t, fid, vocabSet("gamma"))

	// THE assertion. The set changes (beta is dropped) but gamma was
	// already a member, so it is grandfathered and the save stands.
	// Set-equality grandfathering fails here.
	if status, body := env.putAsset(t, fid,
		map[string]any{"value_options": []string{"alpha", "gamma"}}); status != http.StatusOK {
		t.Fatalf("removing an unrelated element from a set holding a grandfathered term "+
			"status=%d want 200 — the grandfather is per-element, not set equality; body=%v", status, body)
	}

	// Now retire `beta` too. The stored set is {alpha, gamma}, so beta
	// is NOT grandfathered: adding it back is choosing a retired term.
	env.retire(t, fid, vocabSet("gamma", "beta"))
	status, body := env.putAsset(t, fid,
		map[string]any{"value_options": []string{"alpha", "gamma", "beta"}})
	assertRejected(t, status, body, "option_not_offerable", "mtv_grand_multi", "beta")

	// Dropping the grandfathered term entirely is always allowed.
	if status, body := env.putAsset(t, fid,
		map[string]any{"value_options": []string{"alpha"}}); status != http.StatusOK {
		t.Fatalf("dropping the grandfathered term status=%d want 200; body=%v", status, body)
	}
}

// TestVocabularyGate_ArchivedRefusedLikeDeprecated — archived is the
// harder retire, and gets the same treatment on the write path: a
// record holding one keeps saving, nothing may change to one.
func TestVocabularyGate_ArchivedRefusedLikeDeprecated(t *testing.T) {
	env := newVocabEnv(t)
	archived := map[string]any{"values": []any{
		map[string]any{"value": "srgb", "label": "sRGB"},
		map[string]any{"value": "linear", "label": "Linear", "status": "archived"},
	}}
	fid := env.assetField(t, "arch", "select", archived)

	status, body := env.putAsset(t, fid, map[string]any{"value_text": "linear"})
	assertRejected(t, status, body, "option_not_offerable", "mtv_arch", "linear")
}

// ---------------------------------------------------------------------------
// One helper, two handlers
// ---------------------------------------------------------------------------

// TestVocabularyGate_BothPathsShareTheRule is the anti-drift
// assertion. The two writers are separate handlers with separate type
// checks; this rule lives in ONE helper and both call it, so the same
// bad write must produce the same refusal on both — same status, same
// reason, same body keys, same message template.
//
// The cheap version of this test asserts each path returns "a 422".
// That passes while the two paths disagree about everything else,
// which is the #665 expressed-vs-obtained class this is here to catch.
func TestVocabularyGate_BothPathsShareTheRule(t *testing.T) {
	env := newVocabEnv(t)

	afid := env.assetField(t, "shared", "multi_select", vocabSet())
	cfid := env.collectionField(t, "shared", "multi_select", vocabSet())
	write := map[string]any{"value_options": []string{"alpha", "atlantis"}}

	aStatus, aBody := env.putAsset(t, afid, write)
	cStatus, cBody := env.putCollection(t, cfid, write)

	if aStatus != cStatus {
		t.Fatalf("asset status=%d collection status=%d — the two paths disagree", aStatus, cStatus)
	}
	if aStatus != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422; asset body=%v", aStatus, aBody)
	}

	// Same keys.
	aKeys, cKeys := bodyKeys(aBody), bodyKeys(cBody)
	if len(aKeys) != len(cKeys) {
		t.Errorf("body keys differ: asset=%v collection=%v", aKeys, cKeys)
	}
	for k := range aBody {
		if _, ok := cBody[k]; !ok {
			t.Errorf("asset body has key %q, collection body does not", k)
		}
	}

	// Same machine-readable reason and the same offending slug.
	if aBody["reason"] != cBody["reason"] {
		t.Errorf("reason: asset=%v collection=%v", aBody["reason"], cBody["reason"])
	}
	if aBody["option"] != cBody["option"] {
		t.Errorf("option: asset=%v collection=%v", aBody["option"], cBody["option"])
	}

	// Same message TEMPLATE — the only licensed difference is the field
	// code each path names.
	wantAsset := fmt.Sprintf("%s: %q is not one of this field's options", "mtv_shared", "atlantis")
	wantColl := fmt.Sprintf("%s: %q is not one of this field's options", collectionTestPrefix+"shared", "atlantis")
	if aBody["error"] != wantAsset {
		t.Errorf("asset error=%q want %q", aBody["error"], wantAsset)
	}
	if cBody["error"] != wantColl {
		t.Errorf("collection error=%q want %q", cBody["error"], wantColl)
	}
}

func bodyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
