// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the accept-and-create write path (#830).
//
// #824 closed the write path: a term the field does not offer is
// refused, always. That is right for `country` and wrong for
// `keywords`, whose vocabulary is supposed to grow from the material.
// `open_vocabulary` is the sanctioned way past the gate — a term that
// matches nothing is CREATED rather than refused.
//
// These drive the real HTTP handlers for the same reason
// vocabulary_value_e2e_test.go does: the interesting failures are all
// about whether the rule is CALLED, and about the transaction it is
// called inside. A unit test on the resolver would pass on a handler
// that never reached it, and could not see the row lock at all.
package metadata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// openVocabSet is the starting vocabulary: three terms whose labels
// differ from their slugs in casing and punctuation, so a test cannot
// pass by matching display text against a slug by coincidence.
func openVocabSet() map[string]any {
	return map[string]any{"values": []any{
		map[string]any{"value": "abstract", "label": "Abstract"},
		map[string]any{"value": "black-and-white", "label": "Black and White"},
		map[string]any{"value": "long-exposure", "label": "Long Exposure"},
	}}
}

// openField defines an OPEN multi_select on the asset side. Codes use
// the `mtv_` prefix cleanTestFields already sweeps.
func (e *vocabEnv) openField(t *testing.T, code string, options map[string]any) string {
	t.Helper()
	return mustCreateField(t, e.router, map[string]any{
		"code": "mtv_" + code, "label": code, "type": "multi_select",
		"options": options, "open_vocabulary": true,
	})
}

// fieldOptions reads a field definition's vocabulary back out of the
// DATABASE rather than out of the write's response body. The response
// echoes the value, not the definition, and a term that was minted into
// a cached copy but never persisted would look identical there.
func (e *vocabEnv) fieldOptions(t *testing.T, fieldID string) []map[string]any {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(),
		`SELECT options FROM field_definition WHERE id = $1`, fieldID).Scan(&raw); err != nil {
		t.Fatalf("read options: %v", err)
	}
	var doc struct {
		Values []json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode options %s: %v", raw, err)
	}
	out := make([]map[string]any, 0, len(doc.Values))
	for _, v := range doc.Values {
		var obj map[string]any
		if err := json.Unmarshal(v, &obj); err == nil {
			out = append(out, obj)
			continue
		}
		// Bare-slug entry: the slug IS the display text.
		var bare string
		if err := json.Unmarshal(v, &bare); err != nil {
			t.Fatalf("option entry is neither object nor string: %s", v)
		}
		out = append(out, map[string]any{"value": bare, "label": bare})
	}
	return out
}

// storedOptions reads the value row's value_options straight from the
// column, for the same reason.
func (e *vocabEnv) storedOptions(t *testing.T, fieldID string) []string {
	t.Helper()
	var got []string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT value_options FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		e.assetID, fieldID).Scan(&got); err != nil {
		t.Fatalf("read value_options: %v", err)
	}
	return got
}

// findOption returns the option carrying slug, or nil.
func findOption(opts []map[string]any, slug string) map[string]any {
	for _, o := range opts {
		if o["value"] == slug {
			return o
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Creating a term
// ---------------------------------------------------------------------------

// The headline: a term the field has never heard of is accepted, and
// what lands is a real option — slugified value, the operator's own
// text as the label — with the ROW storing the slug and not the text.
func TestOpenVocabulary_NovelTermIsCreated(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "novel", openVocabSet())

	status, body := env.putAsset(t, fid, map[string]any{
		"value_options": []string{"abstract", "Sunset Over Water"},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%v", status, body)
	}

	opt := findOption(env.fieldOptions(t, fid), "sunset-over-water")
	if opt == nil {
		t.Fatalf("the term was not created; options = %v", env.fieldOptions(t, fid))
	}
	if opt["label"] != "Sunset Over Water" {
		t.Errorf("label = %v, want %q — the operator's own text, not the slug",
			opt["label"], "Sunset Over Water")
	}

	got := env.storedOptions(t, fid)
	want := []string{"abstract", "sunset-over-water"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("value_options = %v, want %v — the row stores slugs, never raw input", got, want)
	}

	// The response must resolve the term it just created. The handler
	// loaded the field definition BEFORE the write, so building the body
	// from that copy returns every term except the new one — and a
	// client rendering straight from the response shows the raw slug
	// where the label belongs until something forces a refetch.
	resolved, _ := body["resolved_options"].(map[string]any)
	entry, _ := resolved["sunset-over-water"].(map[string]any)
	if entry == nil {
		t.Fatalf("the write that created the term did not resolve it in its own "+
			"response; resolved_options = %v", resolved)
	}
	if entry["label"] != "Sunset Over Water" {
		t.Errorf("resolved label = %v, want %q", entry["label"], "Sunset Over Water")
	}
}

// Casing and stray whitespace are not new terms. Three spellings of one
// word must converge on one option and one stored slug — otherwise an
// open vocabulary fills up with `sunset`, `Sunset` and ` sunset ` and
// stops being a vocabulary.
func TestOpenVocabulary_CasingAndSpacingConverge(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "converge", openVocabSet())

	// Within ONE request first: the dedupe has to happen before the
	// insert, not only across requests.
	if status, body := env.putAsset(t, fid, map[string]any{
		"value_options": []string{"Sunset", "sunset", " sunset "},
	}); status != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%v", status, body)
	}
	if got := env.storedOptions(t, fid); len(got) != 1 || got[0] != "sunset" {
		t.Fatalf("value_options = %v, want exactly [sunset]", got)
	}

	// And again across requests, which is the path that would mint a
	// second option if the second write resolved against a stale copy.
	if status, body := env.putAsset(t, fid, map[string]any{
		"value_options": []string{"SUNSET", "abstract"},
	}); status != http.StatusOK {
		t.Fatalf("second write status=%d; body=%v", status, body)
	}

	opts := env.fieldOptions(t, fid)
	n := 0
	for _, o := range opts {
		if o["value"] == "sunset" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("`sunset` appears %d times in the vocabulary, want 1; options = %v", n, opts)
	}
	// The label is the FIRST spelling seen. Later spellings of the same
	// term match it and do not relabel it — an operator's label is not
	// something a later value save gets to overwrite.
	if opt := findOption(opts, "sunset"); opt == nil || opt["label"] != "Sunset" {
		t.Errorf("label = %v, want %q (the spelling that created the term)", opt["label"], "Sunset")
	}
}

// A term that matches an existing option BY LABEL stores that option's
// slug and creates nothing. This is the "Character = character" rule,
// and it is what stops an open field duplicating its own vocabulary
// every time someone types the display text.
func TestOpenVocabulary_LabelMatchCreatesNothing(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "labelmatch", openVocabSet())
	before := len(env.fieldOptions(t, fid))

	if status, body := env.putAsset(t, fid, map[string]any{
		// Display text, wrong case, extra spaces — all one existing term.
		"value_options": []string{"Abstract", "  black and white  "},
	}); status != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%v", status, body)
	}

	if after := len(env.fieldOptions(t, fid)); after != before {
		t.Errorf("vocabulary grew from %d to %d on a pure label match", before, after)
	}
	got := env.storedOptions(t, fid)
	want := []string{"abstract", "black-and-white"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("value_options = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// What stays closed
// ---------------------------------------------------------------------------

// The flag is opt-in per field. A multi_select without it behaves
// exactly as #824 left it — this is the regression guard for every
// curated vocabulary in the install.
func TestOpenVocabulary_ClosedFieldStillRefuses(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "closed", "multi_select", openVocabSet())

	status, body := env.putAsset(t, fid, map[string]any{
		"value_options": []string{"abstract", "Sunset Over Water"},
	})
	assertRejected(t, status, body, "unknown_option", "mtv_closed", "Sunset Over Water")

	if opt := findOption(env.fieldOptions(t, fid), "sunset-over-water"); opt != nil {
		t.Error("a closed field created a term")
	}
}

// Openness is about terms the field does not HAVE. It says nothing
// about terms it has RETIRED, so a deprecated term is still refused as
// a fresh choice — the same answer a closed field gives.
func TestOpenVocabulary_DeprecatedTermStillRefused(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "deprecated", openVocabSet())
	env.retire(t, fid, map[string]any{"values": []any{
		map[string]any{"value": "abstract", "label": "Abstract", "status": "deprecated"},
		map[string]any{"value": "black-and-white", "label": "Black and White"},
	}})

	status, body := env.putAsset(t, fid, map[string]any{"value_options": []string{"abstract"}})
	assertRejected(t, status, body, "option_not_offerable", "mtv_deprecated", "abstract")
}

// An archived term's LABEL must not resurrect it, and must not mint a
// second option whose slug collides with it either. Refusing is the
// only answer that leaves the catalogue with one meaning per slug:
// reviving ignores a decision the operator made, and minting
// `abstract-2` gives them two terms for one idea.
func TestOpenVocabulary_ArchivedSlugCollisionRefused(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "archived", openVocabSet())
	env.retire(t, fid, map[string]any{"values": []any{
		map[string]any{"value": "abstract", "label": "Abstract", "status": "archived"},
		map[string]any{"value": "black-and-white", "label": "Black and White"},
	}})

	// Typed as the label, which no longer matches — an archived term is
	// not matchable — and whose slug is the archived one.
	status, body := env.putAsset(t, fid, map[string]any{"value_options": []string{"Abstract"}})
	assertRejected(t, status, body, "option_not_offerable", "mtv_archived", "abstract")

	if n := len(env.fieldOptions(t, fid)); n != 2 {
		t.Errorf("vocabulary has %d terms, want 2 — nothing should have been minted", n)
	}
}

// A refusal must roll the whole transaction back, including any term an
// earlier element of the same set already created. Otherwise a rejected
// write leaves half a vocabulary behind.
func TestOpenVocabulary_RefusalRollsBackCreatedTerms(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "rollback", openVocabSet())
	env.retire(t, fid, map[string]any{"values": []any{
		map[string]any{"value": "abstract", "label": "Abstract", "status": "deprecated"},
	}})

	// First element is novel and would be created; second is the
	// deprecated term, which the gate refuses.
	status, _ := env.putAsset(t, fid, map[string]any{
		"value_options": []string{"Brand New Term", "abstract"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422", status)
	}
	if opt := findOption(env.fieldOptions(t, fid), "brand-new-term"); opt != nil {
		t.Error("a term created before the refusal survived the rollback")
	}
}

// ---------------------------------------------------------------------------
// Concurrency — the reason the read takes a row lock
// ---------------------------------------------------------------------------

// Adding a term rewrites the WHOLE options document. Two value writes
// minting different terms at the same moment must both keep theirs; a
// plain read-modify-write loses one, silently, and the loser's asset is
// left holding a slug for a term that does not exist.
//
// Real goroutines against a real database because that is the only
// place the lock exists. Serialised code passes this test with the lock
// removed.
func TestOpenVocabulary_ConcurrentCreatesBothSurvive(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "concurrent", openVocabSet())

	// Distinct assets so the two writes contend on the FIELD row rather
	// than on the same value row — the field document is what the race
	// is about.
	assets := []string{env.assetID, mustInsertAsset(t, env.pool, env.userRef)}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = env.pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, assets[1])
		_, _ = env.pool.Exec(ctx, `DELETE FROM asset_field_value WHERE asset_id = $1`, assets[1])
		_, _ = env.pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, assets[1])
	})

	terms := []string{"Aurora Borealis", "Deep Sea Trench"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, len(terms))
	bodies := make([]string, len(terms))
	for i := range terms {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rr := putJSON(t, env.router,
				fmt.Sprintf("/assets/%s/fields/%s", assets[i], fid),
				map[string]any{"value_options": []string{terms[i]}})
			statuses[i] = rr.Code
			bodies[i] = rr.Body.String()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("write %d status=%d body=%s", i, s, bodies[i])
		}
	}

	opts := env.fieldOptions(t, fid)
	for _, want := range []string{"aurora-borealis", "deep-sea-trench"} {
		if findOption(opts, want) == nil {
			t.Errorf("%q was lost to the concurrent write; options = %v", want, opts)
		}
	}
}

// ---------------------------------------------------------------------------
// The collection twin
// ---------------------------------------------------------------------------

// The two writers are separate handlers, which is exactly the shape
// that gets a rule copied into one of them. Everything above is
// asserted once more through the collection path.
func TestOpenVocabulary_CollectionPathMatches(t *testing.T) {
	env := newVocabEnv(t)
	fid := mustCreateField(t, env.router, map[string]any{
		"code": collectionTestPrefix + "open", "label": "open", "type": "multi_select",
		"subject_kind": "collection", "options": openVocabSet(), "open_vocabulary": true,
	})

	status, body := env.putCollection(t, fid, map[string]any{
		"value_options": []string{"Abstract", "Sunset Over Water"},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%v", status, body)
	}
	if opt := findOption(env.fieldOptions(t, fid), "sunset-over-water"); opt == nil {
		t.Fatal("the collection path did not create the term")
	}

	var got []string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT value_options FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		env.collID, fid).Scan(&got); err != nil {
		t.Fatalf("read collection value_options: %v", err)
	}
	want := []string{"abstract", "sunset-over-water"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("value_options = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Defaults stay strict
// ---------------------------------------------------------------------------

// A default names the value EVERY new asset gets. Minting vocabulary as
// a side effect of typing in the default box would be a change nobody
// asked for, in the one place where the options editor is two controls
// away. So the defaults validator keeps the closed rule even here.
func TestOpenVocabulary_DefaultsStayClosed(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "defaults", openVocabSet())

	rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{
		"default_value": map[string]any{
			"kind": "literal", "value_options": []string{"Never Seen Before"},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", rr.Code, rr.Body.String())
	}
	if opt := findOption(env.fieldOptions(t, fid), "never-seen-before"); opt != nil {
		t.Error("the defaults editor minted a vocabulary term")
	}
}
