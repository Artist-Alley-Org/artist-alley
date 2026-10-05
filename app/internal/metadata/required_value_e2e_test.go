// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// R1 — what `required` MEANS on the ordinary field-value write paths
// (#1389), on BOTH subject kinds.
//
// The defect these cover: `SetAssetFieldValue`, `ClearAssetFieldValue`,
// `SetCollectionFieldValue` and `ClearCollectionFieldValue` contained
// ZERO `fieldRow.Required` checks between them. The only two in the
// package were inside the mirrored helpers, so the flag reached `title`
// and `description` and nothing else — an operator could mark any other
// field required and empty it, or delete it outright, through the
// ordinary API, on an asset or on a collection.
//
// These drive the real HTTP handlers, and every assertion about what
// was stored reads the COLUMN. A response-body assertion passes on a
// build where the handler echoed its own request and wrote nothing.
//
// The counterweights at the bottom are the other half of the sprint and
// are not optional: R1 is a rule about LATER HUMAN writes, and a fix
// that also caught the system writers, the collection-create seed, or
// collection create's own R2 presence rule would be a regression
// wearing a passing test.
package metadata_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// requiredAssetField defines a NON-MIRRORED required asset field.
//
// Non-mirrored is the whole point. `title` already had the check, which
// is exactly why the hole stayed invisible for so long: a reproduction
// written against a mirrored field passes on the broken build.
func requiredAssetField(t *testing.T, e *vocabEnv, code, ftype string, options map[string]any) string {
	t.Helper()
	body := map[string]any{
		"code": "mtv_" + code, "label": code, "type": ftype, "required": true,
	}
	if options != nil {
		body["options"] = options
	}
	return mustCreateField(t, e.router, body)
}

func requiredCollectionField(t *testing.T, e *vocabEnv, code, ftype string, options map[string]any) string {
	t.Helper()
	body := map[string]any{
		"code": collectionTestPrefix + code, "label": code, "type": ftype,
		"subject_kind": "collection", "required": true,
	}
	if options != nil {
		body["options"] = options
	}
	return mustCreateField(t, e.router, body)
}

// readCollectionStored is readStored's collection twin.
func readCollectionStored(t *testing.T, pool *pgxpool.Pool, collectionID, fieldID string) (storedValue, bool) {
	t.Helper()
	var v storedValue
	err := pool.QueryRow(context.Background(), `
		SELECT value_text, value_num, value_date, value_options, value_ref, set_by
		  FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		collectionID, fieldID).Scan(&v.Text, &v.Num, &v.Date, &v.Options, &v.Ref, &v.SetBy)
	if err != nil {
		return storedValue{}, false
	}
	return v, true
}

func (e *vocabEnv) clearAsset(t *testing.T, fieldID string) (int, map[string]any) {
	t.Helper()
	rr := deleteReq(t, e.router, fmt.Sprintf("/assets/%s/fields/%s", e.assetID, fieldID))
	var m map[string]any
	if rr.Body.Len() > 0 {
		m = decodeBody(t, rr.Body.Bytes())
	}
	return rr.Code, m
}

func (e *vocabEnv) clearCollection(t *testing.T, fieldID string) (int, map[string]any) {
	t.Helper()
	rr := deleteReq(t, e.router, fmt.Sprintf("/collections/%s/fields/%s", e.collID, fieldID))
	var m map[string]any
	if rr.Body.Len() > 0 {
		m = decodeBody(t, rr.Body.Bytes())
	}
	return rr.Code, m
}

// assertRequiredRefusal checks the 422 contract for R1: the status, the
// machine reason, and that the sentence names the field.
func assertRequiredRefusal(t *testing.T, status int, body map[string]any) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 field_required; body=%v", status, body)
	}
	if body["reason"] != "field_required" {
		t.Errorf("reason=%v want field_required; body=%v", body["reason"], body)
	}
	if body["field"] == nil || body["field"] == "" {
		t.Errorf("the refusal must name the field it came from; body=%v", body)
	}
}

// ---------------------------------------------------------------------------
// R1-a / R1-b — the asset half of the reproduction
// ---------------------------------------------------------------------------

// R1-a. Every one of these returned 200 and stored an empty value
// before #1389.
func TestRequired_AssetEmptySetRefused(t *testing.T) {
	env := newVocabEnv(t)

	cases := []struct {
		name  string
		ftype string
		seed  map[string]any
		empty map[string]any
	}{
		{"text", "text", map[string]any{"value_text": "AAA"}, map[string]any{"value_text": ""}},
		{"longtext", "longtext", map[string]any{"value_text": "prose"}, map[string]any{"value_text": ""}},
		{"text_whitespace", "text", map[string]any{"value_text": "AAA"}, map[string]any{"value_text": "   \t "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fid := requiredAssetField(t, env, "req_"+tc.name, tc.ftype, nil)
			if code, body := env.putAsset(t, fid, tc.seed); code != http.StatusOK {
				t.Fatalf("seed a real value: status=%d body=%v", code, body)
			}
			before, ok := readStored(t, env.pool, env.assetID, fid)
			if !ok {
				t.Fatal("seed did not store a row")
			}

			code, body := env.putAsset(t, fid, tc.empty)
			assertRequiredRefusal(t, code, body)

			// R1-g's single-field half: a refused write leaves what was
			// there byte-identical.
			after, ok := readStored(t, env.pool, env.assetID, fid)
			if !ok {
				t.Fatal("the refused write removed the row")
			}
			if before.Text == nil || after.Text == nil || *before.Text != *after.Text {
				t.Errorf("stored value changed on a refused write: %v -> %v", before.Text, after.Text)
			}
		})
	}
}

// R1-b. `DELETE /assets/{id}/fields/{field_id}` answered 204 and removed
// the row before #1389.
func TestRequired_AssetClearRefused(t *testing.T) {
	env := newVocabEnv(t)
	fid := requiredAssetField(t, env, "req_clear", "text", nil)
	if code, body := env.putAsset(t, fid, map[string]any{"value_text": "keep me"}); code != http.StatusOK {
		t.Fatalf("seed: status=%d body=%v", code, body)
	}

	code, body := env.clearAsset(t, fid)
	assertRequiredRefusal(t, code, body)

	got, ok := readStored(t, env.pool, env.assetID, fid)
	if !ok {
		t.Fatal("the refused clear removed the row anyway")
	}
	if got.Text == nil || *got.Text != "keep me" {
		t.Errorf("value after a refused clear = %v, want %q", got.Text, "keep me")
	}
}

// ---------------------------------------------------------------------------
// R1-c / R1-d — the collection half, which is a SEPARATE handler
// ---------------------------------------------------------------------------

func TestRequired_CollectionEmptySetRefused(t *testing.T) {
	env := newVocabEnv(t)

	cases := []struct {
		name  string
		ftype string
		seed  map[string]any
		empty map[string]any
	}{
		{"text", "text", map[string]any{"value_text": "AAA"}, map[string]any{"value_text": ""}},
		{"text_whitespace", "text", map[string]any{"value_text": "AAA"}, map[string]any{"value_text": "  "}},
		{"longtext", "longtext", map[string]any{"value_text": "prose"}, map[string]any{"value_text": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fid := requiredCollectionField(t, env, "req_"+tc.name, tc.ftype, nil)
			if code, body := env.putCollection(t, fid, tc.seed); code != http.StatusOK {
				t.Fatalf("seed: status=%d body=%v", code, body)
			}
			code, body := env.putCollection(t, fid, tc.empty)
			assertRequiredRefusal(t, code, body)

			got, ok := readCollectionStored(t, env.pool, env.collID, fid)
			if !ok || got.Text == nil || *got.Text != tc.seed["value_text"] {
				t.Errorf("stored value changed on a refused write: %v", got.Text)
			}
		})
	}
}

func TestRequired_CollectionClearRefused(t *testing.T) {
	env := newVocabEnv(t)
	fid := requiredCollectionField(t, env, "req_cclear", "text", nil)
	if code, body := env.putCollection(t, fid, map[string]any{"value_text": "keep me"}); code != http.StatusOK {
		t.Fatalf("seed: status=%d body=%v", code, body)
	}

	code, body := env.clearCollection(t, fid)
	assertRequiredRefusal(t, code, body)

	got, ok := readCollectionStored(t, env.pool, env.collID, fid)
	if !ok || got.Text == nil || *got.Text != "keep me" {
		t.Errorf("value after a refused clear = %v", got.Text)
	}
}

// ---------------------------------------------------------------------------
// RT — rich_text is SEMANTIC, and this is the case a TrimSpace passes
// ---------------------------------------------------------------------------

// RT-b + RT-c. The sanitiser removes what a value may not CONTAIN and
// strips no empty elements, so `<p></p>`, `<p><br></p>` and
// `<p>&nbsp;</p>` all survive it unchanged (bar the entity decoding to a
// literal U+00A0) and all trim to themselves. An implementation of R1
// written as `strings.TrimSpace(value_text) == ""` ACCEPTS every one of
// them, and the field then renders blank while the server considers it
// filled.
func TestRequired_RichTextSemanticEmptiness(t *testing.T) {
	env := newVocabEnv(t)

	refused := []string{
		"",
		"   ",
		"<p></p>",
		"<p><br></p>",
		"<p>   </p>",
		"<br>",
		"<p>&nbsp;</p>",
		"<ul><li></li></ul>",
		"<blockquote></blockquote>",
		"<p></p><p></p>",
		// Stripped entirely by the sanitiser, so what reaches storage is "".
		"<div></div>",
		"<script>alert(1)</script>",
	}
	accepted := []string{
		"<p>real</p>",
		"<p><strong>hi</strong></p>",
		"<p> a </p>",
		"<ul><li>one</li></ul>",
	}

	for _, kind := range []string{"asset", "collection"} {
		t.Run(kind, func(t *testing.T) {
			var fid string
			put := env.putAsset
			read := func() (storedValue, bool) { return readStored(t, env.pool, env.assetID, fid) }
			if kind == "collection" {
				fid = requiredCollectionField(t, env, "req_rt", "rich_text", nil)
				put = env.putCollection
				read = func() (storedValue, bool) { return readCollectionStored(t, env.pool, env.collID, fid) }
			} else {
				fid = requiredAssetField(t, env, "req_rt", "rich_text", nil)
			}

			// RT-d first, so there is a real value for the refusals to
			// fail to overwrite.
			for _, in := range accepted {
				if code, body := put(t, fid, map[string]any{"value_text": in}); code != http.StatusOK {
					t.Fatalf("formatted content %q must be accepted: status=%d body=%v", in, code, body)
				}
			}
			last, ok := read()
			if !ok {
				t.Fatal("no row after the accepted writes")
			}

			for _, in := range refused {
				code, body := put(t, fid, map[string]any{"value_text": in})
				if code != http.StatusUnprocessableEntity {
					t.Errorf("visually empty rich_text %q must be refused, got status=%d body=%v", in, code, body)
					continue
				}
				if body["reason"] != "field_required" {
					t.Errorf("%q refused with reason=%v, want field_required", in, body["reason"])
				}
			}

			after, ok := read()
			if !ok || after.Text == nil || last.Text == nil || *after.Text != *last.Text {
				t.Errorf("a refused rich_text write changed the stored value: %v -> %v", last.Text, after.Text)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MS-a — required multi_select
// ---------------------------------------------------------------------------

// The `[]` case is the collection-side hole: validateCollectionValueType
// only required `value_options` to be non-nil, so an empty array stored
// an empty set on a required field and answered 200.
func TestRequired_MultiSelectEmptySet(t *testing.T) {
	env := newVocabEnv(t)

	t.Run("asset", func(t *testing.T) {
		fid := requiredAssetField(t, env, "req_ms", "multi_select", vocabSet())
		if code, body := env.putAsset(t, fid, map[string]any{"value_options": []string{"alpha"}}); code != http.StatusOK {
			t.Fatalf("one member must be accepted: status=%d body=%v", code, body)
		}
		// nil and [] are both empty. The asset path refuses both before
		// R1 even sees them (buildUpsertParams), which is fine: the
		// requirement is that neither is stored, not which gate says so.
		for _, empty := range []map[string]any{{"value_options": nil}, {"value_options": []string{}}} {
			if code, body := env.putAsset(t, fid, empty); code == http.StatusOK {
				t.Errorf("empty value_options accepted on a required field: body=%v", body)
			}
		}
		got, ok := readStored(t, env.pool, env.assetID, fid)
		if !ok || len(got.Options) != 1 || got.Options[0] != "alpha" {
			t.Errorf("stored options after the refusals = %v, want [alpha]", got.Options)
		}
	})

	t.Run("collection", func(t *testing.T) {
		fid := requiredCollectionField(t, env, "req_ms", "multi_select", vocabSet())
		if code, body := env.putCollection(t, fid, map[string]any{"value_options": []string{"alpha"}}); code != http.StatusOK {
			t.Fatalf("one member must be accepted: status=%d body=%v", code, body)
		}
		code, body := env.putCollection(t, fid, map[string]any{"value_options": []string{}})
		assertRequiredRefusal(t, code, body)
		if code, body := env.putCollection(t, fid, map[string]any{"value_options": nil}); code == http.StatusOK {
			t.Errorf("null value_options accepted on a required field: body=%v", body)
		}
		got, ok := readCollectionStored(t, env.pool, env.collID, fid)
		if !ok || len(got.Options) != 1 || got.Options[0] != "alpha" {
			t.Errorf("stored options after the refusals = %v, want [alpha]", got.Options)
		}
	})
}

// ---------------------------------------------------------------------------
// required + boolean: FALSE IS A REAL VALUE
// ---------------------------------------------------------------------------

// The rule that would delete an operator's data: a truthiness test.
// `value_num = 0` is a deliberate "no" and must save on a required
// boolean; only a NULL value_num is empty.
func TestRequired_BooleanFalseIsAValue(t *testing.T) {
	env := newVocabEnv(t)

	t.Run("asset", func(t *testing.T) {
		fid := requiredAssetField(t, env, "req_bool", "boolean", nil)
		if code, body := env.putAsset(t, fid, map[string]any{"value_num": 0}); code != http.StatusOK {
			t.Fatalf("false must be storable on a required boolean: status=%d body=%v", code, body)
		}
		got, ok := readStored(t, env.pool, env.assetID, fid)
		if !ok || got.Num == nil || *got.Num != 0 {
			t.Fatalf("value_num after storing false = %v, want 0", got.Num)
		}
		// And the clear of it is still refused, because the field is required.
		code, body := env.clearAsset(t, fid)
		assertRequiredRefusal(t, code, body)
	})

	t.Run("collection", func(t *testing.T) {
		fid := requiredCollectionField(t, env, "req_bool", "boolean", nil)
		if code, body := env.putCollection(t, fid, map[string]any{"value_num": 0}); code != http.StatusOK {
			t.Fatalf("false must be storable: status=%d body=%v", code, body)
		}
		got, ok := readCollectionStored(t, env.pool, env.collID, fid)
		if !ok || got.Num == nil || *got.Num != 0 {
			t.Fatalf("value_num after storing false = %v, want 0", got.Num)
		}
		code, body := env.clearCollection(t, fid)
		assertRequiredRefusal(t, code, body)
	})
}

// read_only is checked FIRST, so a field carrying both settings answers
// with the more specific sentence (the boundary matrix's
// required+read_only cell). No configuration-time refusal is added for
// the combination — every combination stays legal, per sprint 18d's
// ruling.
func TestRequired_ReadOnlyWinsTheSentence(t *testing.T) {
	env := newVocabEnv(t)
	fid := requiredAssetField(t, env, "req_ro", "text", nil)
	if code, body := env.putAsset(t, fid, map[string]any{"value_text": "seeded"}); code != http.StatusOK {
		t.Fatalf("seed: status=%d body=%v", code, body)
	}
	mustPatchField(t, env, fid, map[string]any{"read_only": true})

	code, body := env.putAsset(t, fid, map[string]any{"value_text": ""})
	if code != http.StatusUnprocessableEntity || body["reason"] != "field_read_only" {
		t.Errorf("required+read_only set: status=%d reason=%v, want 422 field_read_only", code, body["reason"])
	}
	code, body = env.clearAsset(t, fid)
	if code != http.StatusUnprocessableEntity || body["reason"] != "field_read_only" {
		t.Errorf("required+read_only clear: status=%d reason=%v, want 422 field_read_only", code, body["reason"])
	}
}

// ---------------------------------------------------------------------------
// R1-g — neighbour preservation, N>=2, both subject kinds
// ---------------------------------------------------------------------------

func TestRequired_NeighbourPreservation(t *testing.T) {
	env := newVocabEnv(t)

	t.Run("asset", func(t *testing.T) {
		req := requiredAssetField(t, env, "nb_req", "text", nil)
		opt := env.assetField(t, "nb_opt", "text", nil)
		if code, _ := env.putAsset(t, req, map[string]any{"value_text": "required value"}); code != http.StatusOK {
			t.Fatal("seed required")
		}
		if code, _ := env.putAsset(t, opt, map[string]any{"value_text": "neighbour"}); code != http.StatusOK {
			t.Fatal("seed neighbour")
		}

		// A refused write leaves the NEIGHBOUR byte-identical.
		if code, body := env.putAsset(t, req, map[string]any{"value_text": " "}); code != http.StatusUnprocessableEntity {
			t.Fatalf("expected refusal, got %d %v", code, body)
		}
		n, ok := readStored(t, env.pool, env.assetID, opt)
		if !ok || n.Text == nil || *n.Text != "neighbour" {
			t.Errorf("neighbour changed by a refused write: %v", n.Text)
		}

		// A SUCCESSFUL neighbour write leaves the required field
		// byte-identical, including its clear.
		if code, _ := env.putAsset(t, opt, map[string]any{"value_text": "moved on"}); code != http.StatusOK {
			t.Fatal("neighbour write")
		}
		if code, _ := env.clearAsset(t, opt); code != http.StatusNoContent {
			t.Fatal("neighbour clear must succeed — it is not required")
		}
		r, ok := readStored(t, env.pool, env.assetID, req)
		if !ok || r.Text == nil || *r.Text != "required value" {
			t.Errorf("required field changed by neighbour activity: %v", r.Text)
		}
	})

	t.Run("collection", func(t *testing.T) {
		req := requiredCollectionField(t, env, "nb_req", "text", nil)
		opt := env.collectionField(t, "nb_opt", "text", nil)
		if code, _ := env.putCollection(t, req, map[string]any{"value_text": "required value"}); code != http.StatusOK {
			t.Fatal("seed required")
		}
		if code, _ := env.putCollection(t, opt, map[string]any{"value_text": "neighbour"}); code != http.StatusOK {
			t.Fatal("seed neighbour")
		}
		if code, _ := env.putCollection(t, req, map[string]any{"value_text": ""}); code != http.StatusUnprocessableEntity {
			t.Fatal("expected refusal")
		}
		n, ok := readCollectionStored(t, env.pool, env.collID, opt)
		if !ok || n.Text == nil || *n.Text != "neighbour" {
			t.Errorf("neighbour changed by a refused write: %v", n.Text)
		}
		if code, _ := env.clearCollection(t, opt); code != http.StatusNoContent {
			t.Fatal("optional neighbour clear must succeed")
		}
		r, ok := readCollectionStored(t, env.pool, env.collID, req)
		if !ok || r.Text == nil || *r.Text != "required value" {
			t.Errorf("required field changed by neighbour activity: %v", r.Text)
		}
	})
}

// ---------------------------------------------------------------------------
// The optional counterweight — Clear WORKS when the field is optional
// ---------------------------------------------------------------------------

// Without this half, "required refuses Clear" is indistinguishable from
// "Clear is broken". Every type an edit surface can empty is covered,
// on both subject kinds.
func TestOptionalClearSucceedsAcrossTypes(t *testing.T) {
	env := newVocabEnv(t)

	cases := []struct {
		name    string
		ftype   string
		options map[string]any
		value   map[string]any
	}{
		{"text", "text", nil, map[string]any{"value_text": "gone soon"}},
		{"longtext", "longtext", nil, map[string]any{"value_text": "prose"}},
		{"rich_text", "rich_text", nil, map[string]any{"value_text": "<p>prose</p>"}},
		{"number", "number", nil, map[string]any{"value_num": 42}},
		{"boolean_true", "boolean", nil, map[string]any{"value_num": 1}},
		{"boolean_false", "boolean", nil, map[string]any{"value_num": 0}},
		{"date", "date", nil, map[string]any{"value_date": "2026-01-01T00:00:00Z"}},
		{"select", "select", vocabFlat(), map[string]any{"value_text": "srgb"}},
		{"multi_select", "multi_select", vocabSet(), map[string]any{"value_options": []string{"alpha", "beta"}}},
		{"tree", "tree", vocabTree(), map[string]any{"value_text": "london"}},
	}

	for _, tc := range cases {
		t.Run("asset/"+tc.name, func(t *testing.T) {
			fid := env.assetField(t, "opt_"+tc.name, tc.ftype, tc.options)
			if code, body := env.putAsset(t, fid, tc.value); code != http.StatusOK {
				t.Fatalf("seed: status=%d body=%v", code, body)
			}
			if code, body := env.clearAsset(t, fid); code != http.StatusNoContent {
				t.Fatalf("optional clear: status=%d body=%v", code, body)
			}
			if _, ok := readStored(t, env.pool, env.assetID, fid); ok {
				t.Error("the row survived an optional clear")
			}
		})
		t.Run("collection/"+tc.name, func(t *testing.T) {
			fid := env.collectionField(t, "opt_"+tc.name, tc.ftype, tc.options)
			if code, body := env.putCollection(t, fid, tc.value); code != http.StatusOK {
				t.Fatalf("seed: status=%d body=%v", code, body)
			}
			if code, body := env.clearCollection(t, fid); code != http.StatusNoContent {
				t.Fatalf("optional clear: status=%d body=%v", code, body)
			}
			if _, ok := readCollectionStored(t, env.pool, env.collID, fid); ok {
				t.Error("the row survived an optional clear")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CW-5 — the mirrored refusals are BYTE-IDENTICAL
// ---------------------------------------------------------------------------

// `title` is required and mirrored, and its refusals are the asset
// plane's rule rather than R1's: a 400 with a specific sentence, from
// setMirroredFieldValue / clearMirroredFieldValue. R1 must not have
// reached them, or the two planes start describing one refusal
// differently — the divergence ADR 0012's mirrored amendment exists to
// prevent.
func TestRequired_MirroredRefusalsUnchanged(t *testing.T) {
	env := newVocabEnv(t)
	fid := mirroredFieldID(t, env, "title")

	code, body := env.putAsset(t, fid, map[string]any{"value_text": "   "})
	if code != http.StatusBadRequest {
		t.Fatalf("mirrored empty set: status=%d want 400; body=%v", code, body)
	}
	if got, want := body["error"], "title is required and cannot be empty"; got != want {
		t.Errorf("mirrored set refusal = %q, want %q", got, want)
	}

	code, body = env.clearAsset(t, fid)
	if code != http.StatusBadRequest {
		t.Fatalf("mirrored clear: status=%d want 400; body=%v", code, body)
	}
	if got, want := body["error"], "title is required and cannot be cleared"; got != want {
		t.Errorf("mirrored clear refusal = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// CW-1 — R1 constrains HUMAN writes, and must not convert the system
// writers into human ones
// ---------------------------------------------------------------------------

// The exemption is STRUCTURAL, exactly as `read_only`'s is: those call
// sites are different Go functions with no route, so there is nothing a
// client could send to claim it and nothing here to configure. What
// this drives is the REAL defaults applier against a field whose human
// path R1 has just shut, which is the only way to assert the boundary
// rather than assume it.
func TestRequired_SystemWritersAreUnaffected(t *testing.T) {
	env := newVocabEnv(t)
	fid := requiredAssetField(t, env, "sysreq", "text", nil)
	mustPatchField(t, env, fid, map[string]any{
		"default_value": map[string]any{"kind": "literal", "value_text": "system wrote this"},
	})

	// The human path refuses an empty write into it.
	status, body := env.putAsset(t, fid, map[string]any{"value_text": "   "})
	assertRequiredRefusal(t, status, body)

	applied := mustApplyDefaults(t, env.pool, env.assetID)
	if len(applied) == 0 {
		t.Fatal("the defaults applier reported nothing applied")
	}
	v, ok := readStored(t, env.pool, env.assetID, fid)
	if !ok || v.Text == nil || *v.Text != "system wrote this" {
		t.Fatalf("stored = %+v; a system writer must still fill a required field", v)
	}
	if v.SetBy != "default" {
		t.Errorf("set_by = %q; provenance is evidence of which writer ran", v.SetBy)
	}

	// And the human path is open again the moment there is a real value
	// to write: `required` refuses EMPTINESS, not editing.
	if code, b := env.putAsset(t, fid, map[string]any{"value_text": "a person's value"}); code != http.StatusOK {
		t.Fatalf("a non-empty human write must be accepted: status=%d body=%v", code, b)
	}
}

// CW-2 — the collection-create SEED is a HUMAN write and is still
// EXEMPT, because the exemption comes from the R1/R2 BOUNDARY rather
// than from provenance: it is the create body, which is R2's business,
// and R2 already demands the field be present.
//
// This drives the REAL seeder rather than the collections package's
// fake gate, because the thing being asserted is that R1's later-write
// predicate was not wired into this function. It pins the boundary in
// the direction a fix would break by accident, so widening R1 into the
// create path becomes a deliberate decision with a failing test in
// front of it rather than a side effect.
//
// R2's own refusals (create omitting a required field, create seeding
// it) are asserted where they live, in
// internal/collections/required_field_gate_test.go, against the real
// handler. Nothing here changes them.
func TestRequired_CollectionSeedIsNotSubjectToR1(t *testing.T) {
	env := newVocabEnv(t)
	fid := requiredCollectionField(t, env, "seedreq", "text", nil)

	h := metadata.NewHandler(env.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx := context.Background()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	blank := ""
	if err := h.SeedCollectionFieldValueInTx(ctx, tx,
		uuid.MustParse(env.collID), uuid.MustParse(fid),
		&blank, nil, nil, nil, nil, env.userRef,
	); err != nil {
		t.Fatalf("the create-body seed must not be subject to R1: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, ok := readCollectionStored(t, env.pool, env.collID, fid)
	if !ok || got.Text == nil || *got.Text != "" {
		t.Fatalf("the seeded value did not land: %+v", got)
	}

	// And the LATER write through the ordinary handler is refused, which
	// is the whole point of the boundary: same value, same field, and the
	// difference is which writer ran.
	code, body := env.putCollection(t, fid, map[string]any{"value_text": ""})
	assertRequiredRefusal(t, code, body)
}
