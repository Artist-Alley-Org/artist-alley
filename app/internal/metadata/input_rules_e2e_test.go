// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the two field-configuration input rules
// (#1173, ADR 0012, migration 00064): `read_only` and `regexp_filter`.
//
// These drive the real HTTP handlers rather than the helpers in
// inputrules.go, and that is the point rather than thoroughness for its
// own sake. A unit test on `patternRefusal` passes just as happily when
// nothing CALLS it, which is exactly the shape of the bug #824 closed:
// the membership walk was correct and the value path never invoked one.
// There are FOUR call sites here, not one, and a rule wired to three of
// them is a rule an operator can walk around.
//
// The other reason for going through HTTP: the seam these settings rest
// on IS the handler. `read_only` refuses a HUMAN write and exempts the
// system writers, and the exemption is not a flag on a request, it is
// the fact that ApplyAssetDefaults is a different function with no
// route. TestReadOnly_SystemWritersAreUnaffected drives the real
// defaults applier against a field the API refuses, which is the only
// way to assert that boundary rather than assume it.
//
// Everything about storage is asserted against the COLUMN, never
// against a response body. A handler that echoes its own write passes a
// body assertion on a build where nothing was stored.
package metadata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

// ---------------------------------------------------------------------------
// Reading the persisted configuration
// ---------------------------------------------------------------------------

type inputRules struct {
	ReadOnly     bool
	RegexpFilter *string
}

func readInputRules(t *testing.T, e *vocabEnv, fieldID string) inputRules {
	t.Helper()
	var got inputRules
	if err := e.pool.QueryRow(context.Background(),
		`SELECT read_only, regexp_filter FROM field_definition WHERE id = $1`, fieldID).
		Scan(&got.ReadOnly, &got.RegexpFilter); err != nil {
		t.Fatalf("read input rules: %v", err)
	}
	return got
}

// patchField PATCHes a field definition and returns the status plus the
// decoded body, so a refusal can be asserted on its sentence as well as
// on its code.
func patchField(t *testing.T, e *vocabEnv, fieldID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	rr := patchJSON(t, e.router, "/fields/"+fieldID, body)
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr.Code, m
}

func mustPatchField(t *testing.T, e *vocabEnv, fieldID string, body map[string]any) {
	t.Helper()
	if code, m := patchField(t, e, fieldID, body); code != http.StatusOK {
		t.Fatalf("patch field: status=%d body=%v", code, m)
	}
}

// mirroredFieldID looks up one of the two shipped mirrored definitions.
// They are seeded by the baseline and cannot be manufactured: which
// columns are mirrorable is a CHECK constraint, deliberately, so the
// only way to test the exclusion is against a real one.
func mirroredFieldID(t *testing.T, e *vocabEnv, code string) string {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM field_definition WHERE code = $1 AND mirrors_column IS NOT NULL`, code).
		Scan(&id); err != nil {
		t.Skipf("no mirrored %q field on this database: %v", code, err)
	}
	return id.String()
}

func assertValueRefused(t *testing.T, status int, body map[string]any, wantReason string) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422; body=%v", status, body)
	}
	if body["reason"] != wantReason {
		t.Errorf("reason=%v want %q; body=%v", body["reason"], wantReason, body)
	}
	if body["field"] == nil || body["field"] == "" {
		t.Errorf("refusal must name the field it came from; body=%v", body)
	}
}

// ---------------------------------------------------------------------------
// 1. Configuring a pattern: storage and the clear companion
// ---------------------------------------------------------------------------

// Omitting both properties leaves the configuration alone. PATCH is
// partial, and a settings form that saves a label must not silently
// take a pattern off.
func TestRegexpFilter_OmittedLeavesItAlone(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "roundtrip", "text", nil)

	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `[A-Z]{3}_[0-9]{4}`})
	mustPatchField(t, env, fid, map[string]any{"label": "renamed"})

	got := readInputRules(t, env, fid)
	if got.RegexpFilter == nil || *got.RegexpFilter != `[A-Z]{3}_[0-9]{4}` {
		t.Fatalf("regexp_filter = %v after an unrelated PATCH; want it untouched", got.RegexpFilter)
	}
}

// The blank string is refused, and — the half that matters — the
// EXISTING configuration survives the refusal. A 400 that had already
// nulled the column would take the setting off while telling the
// operator it had not.
func TestRegexpFilter_BlankIsRefusedAndChangesNothing(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "blankrefuse", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `[a-z]+`})

	code, body := patchField(t, env, fid, map[string]any{"regexp_filter": ""})
	if code != http.StatusBadRequest {
		t.Fatalf("blank regexp_filter: status=%d want 400; body=%v", code, body)
	}
	if msg, _ := body["error"].(string); msg == "" || !strings.Contains(msg, "clear_regexp_filter") {
		t.Errorf("the refusal must name the way to actually remove a pattern; got %q", msg)
	}

	got := readInputRules(t, env, fid)
	if got.RegexpFilter == nil || *got.RegexpFilter != `[a-z]+` {
		t.Fatalf("regexp_filter = %v after a refused blank; want the previous pattern intact", got.RegexpFilter)
	}
}

// A WHITESPACE-ONLY pattern is valid and is stored verbatim.
//
// This is the one place this column deliberately diverges from
// `edit_tab`, which trims before its blank check. A tab named " " is a
// tab nobody can navigate to; a PATTERN of three spaces matches exactly
// three spaces, which is a real rule an operator may want. Trimming
// here would silently corrupt it, so only the genuinely empty string is
// refused.
func TestRegexpFilter_WhitespaceOnlyPatternIsValid(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "wspattern", "text", nil)

	mustPatchField(t, env, fid, map[string]any{"regexp_filter": "   "})

	got := readInputRules(t, env, fid)
	if got.RegexpFilter == nil || *got.RegexpFilter != "   " {
		t.Fatalf("regexp_filter = %q, want three spaces stored verbatim", derefOr(got.RegexpFilter, "<nil>"))
	}

	// And it behaves: the whole value must be those three spaces.
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "   "}); status != http.StatusOK {
		t.Errorf("three spaces must match a three-space pattern: status=%d body=%v", status, body)
	}
	status, body := env.putAsset(t, fid, map[string]any{"value_text": "  "})
	assertValueRefused(t, status, body, "pattern_mismatch")
}

// Clearing is explicit, and it lands as SQL NULL rather than as "".
func TestRegexpFilter_ExplicitClearWritesNull(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "clearnull", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `x+`})

	mustPatchField(t, env, fid, map[string]any{"clear_regexp_filter": true})

	if got := readInputRules(t, env, fid); got.RegexpFilter != nil {
		t.Fatalf("regexp_filter = %q after clear; NULL is the one representation of no constraint",
			*got.RegexpFilter)
	}
}

// Sending both is a contradiction and is refused rather than resolved.
func TestRegexpFilter_SetAndClearTogetherIsRefused(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "bothargs", "text", nil)

	code, body := patchField(t, env, fid, map[string]any{
		"regexp_filter": `y+`, "clear_regexp_filter": true,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%v", code, body)
	}
	if got := readInputRules(t, env, fid); got.RegexpFilter != nil {
		t.Errorf("a refused contradiction must write nothing; got %q", *got.RegexpFilter)
	}
}

// A pattern that does not compile is caught at CONFIGURATION time, not
// at the first value write. An operator who gets a 200 here finds out
// their rule is broken from a user, days later.
func TestRegexpFilter_InvalidPatternIsRefusedAtConfiguration(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "badpattern", "text", nil)

	code, body := patchField(t, env, fid, map[string]any{"regexp_filter": `[unclosed`})
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%v", code, body)
	}
	if got := readInputRules(t, env, fid); got.RegexpFilter != nil {
		t.Errorf("an uncompilable pattern must not be stored; got %q", *got.RegexpFilter)
	}
}

// Two fields, and one field's configuration cannot reach the other.
func TestRegexpFilter_ConfigurationIsPerField(t *testing.T) {
	env := newVocabEnv(t)
	constrained := env.assetField(t, "isolatea", "text", nil)
	neighbour := env.assetField(t, "isolateb", "text", nil)

	mustPatchField(t, env, constrained, map[string]any{"regexp_filter": `[0-9]+`})

	if got := readInputRules(t, env, neighbour); got.RegexpFilter != nil || got.ReadOnly {
		t.Fatalf("the neighbouring field was changed: %+v", got)
	}
	if status, body := env.putAsset(t, neighbour, map[string]any{"value_text": "anything at all"}); status != http.StatusOK {
		t.Errorf("a neighbouring field must stay writable: status=%d body=%v", status, body)
	}
}

// ---------------------------------------------------------------------------
// 2. Which types may carry a pattern
// ---------------------------------------------------------------------------

// The supported two accept one; everything else refuses a NON-EMPTY
// pattern. `rich_text` is called out separately because it is the
// surprising member: it stores in `value_text` like the other two, and
// is excluded because what lands there is sanitised markup rather than
// the operator's own words.
func TestRegexpFilter_SupportedAndUnsupportedTypes(t *testing.T) {
	env := newVocabEnv(t)

	for _, ty := range []string{"text", "longtext"} {
		fid := env.assetField(t, "sup"+ty, ty, nil)
		if code, body := patchField(t, env, fid, map[string]any{"regexp_filter": `.+`}); code != http.StatusOK {
			t.Errorf("%s must accept a pattern: status=%d body=%v", ty, code, body)
		}
	}

	unsupported := map[string]map[string]any{
		"rich_text":    nil,
		"number":       nil,
		"boolean":      nil,
		"date":         nil,
		"datetime":     nil,
		"reference":    nil,
		"select":       vocabFlat(),
		"multi_select": vocabSet(),
		"tree":         vocabTree(),
	}
	for ty, opts := range unsupported {
		fid := env.assetField(t, "unsup"+ty, ty, opts)
		code, body := patchField(t, env, fid, map[string]any{"regexp_filter": `.+`})
		if code != http.StatusBadRequest {
			t.Errorf("%s must refuse a pattern: status=%d body=%v", ty, code, body)
			continue
		}
		if got := readInputRules(t, env, fid); got.RegexpFilter != nil {
			t.Errorf("%s: a refused pattern must not be stored; got %q", ty, *got.RegexpFilter)
		}
		// The CLEAR is always permitted, even where a pattern could
		// never have been set. A setting must always be reachable in the
		// direction of "off", or a mis-set field becomes unrepairable.
		if code, body := patchField(t, env, fid, map[string]any{"clear_regexp_filter": true}); code != http.StatusOK {
			t.Errorf("%s must accept clear_regexp_filter: status=%d body=%v", ty, code, body)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Mirrored fields carry neither setting
// ---------------------------------------------------------------------------

// `title` and `description` are views onto columns of `assets` that
// `POST /assets` and `PATCH /assets/{id}` also write. Enforcing either
// rule on the field plane alone would let one plane reach a state the
// other calls invalid, which is the divergence #822 exists to prevent.
//
// The UNSET state stays legal on them, and so does the clear, so
// nothing about the shipped rows becomes unpatchable.
func TestMirroredField_RefusesBothInputRules(t *testing.T) {
	env := newVocabEnv(t)

	for _, code := range []string{"title", "description"} {
		fid := mirroredFieldID(t, env, code)

		st, body := patchField(t, env, fid, map[string]any{"read_only": true})
		if st != http.StatusBadRequest {
			t.Errorf("%s: read_only status=%d want 400; body=%v", code, st, body)
		}
		st, body = patchField(t, env, fid, map[string]any{"regexp_filter": `.+`})
		if st != http.StatusBadRequest {
			t.Errorf("%s: regexp_filter status=%d want 400; body=%v", code, st, body)
		}

		// The permitted half, both directions of "off".
		if st, body := patchField(t, env, fid, map[string]any{"read_only": false}); st != http.StatusOK {
			t.Errorf("%s: read_only=false must be permitted: status=%d body=%v", code, st, body)
		}
		if st, body := patchField(t, env, fid, map[string]any{"clear_regexp_filter": true}); st != http.StatusOK {
			t.Errorf("%s: clear must be permitted: status=%d body=%v", code, st, body)
		}

		got := readInputRules(t, env, fid)
		if got.ReadOnly || got.RegexpFilter != nil {
			t.Errorf("%s: a mirrored field must hold neither setting; got %+v", code, got)
		}
	}
}

// The counterweight: an ordinary field of the same type accepts exactly
// what the mirrored one refused. Without this, the test above would
// pass on a build where the settings never worked at all.
func TestMirroredExclusion_OrdinaryFieldAcceptsTheSameSettings(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "ordinary", "text", nil)

	mustPatchField(t, env, fid, map[string]any{"read_only": true, "regexp_filter": `.+`})

	got := readInputRules(t, env, fid)
	if !got.ReadOnly || got.RegexpFilter == nil {
		t.Fatalf("an ordinary text field must accept both settings; got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 4. The pattern in force, on the asset plane
// ---------------------------------------------------------------------------

func TestRegexpFilter_AssetWriteMatchingAndNot(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "shotcode", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `[A-Z]{3}_[0-9]{4}`})

	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "AAA_0010"}); status != http.StatusOK {
		t.Fatalf("a matching value must be accepted: status=%d body=%v", status, body)
	}
	if v, ok := readStored(t, env.pool, env.assetID, fid); !ok || v.Text == nil || *v.Text != "AAA_0010" {
		t.Fatalf("stored value = %+v, want AAA_0010 in value_text", v)
	}

	status, body := env.putAsset(t, fid, map[string]any{"value_text": "aaa_10"})
	assertValueRefused(t, status, body, "pattern_mismatch")

	// The refusal wrote nothing, so the earlier good value stands.
	if v, _ := readStored(t, env.pool, env.assetID, fid); v.Text == nil || *v.Text != "AAA_0010" {
		t.Errorf("a refused write must not disturb the stored value; got %+v", v)
	}
}

// The anchoring is the contract: a pattern describes the WHOLE value.
// Written by hand as `^…$` this would pass, because a value whose first
// line matches satisfies a line anchor. `\A…\z` is unaffected by (?m),
// which is why the server does the anchoring rather than the operator.
func TestRegexpFilter_MultilineStillMeansWholeValue(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "multiline", "longtext", nil)
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `(?m)^[A-Z]+$`})

	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "ABC"}); status != http.StatusOK {
		t.Fatalf("a single matching line must be accepted: status=%d body=%v", status, body)
	}
	status, body := env.putAsset(t, fid, map[string]any{"value_text": "ABC\nnot allowed"})
	assertValueRefused(t, status, body, "pattern_mismatch")
}

// A top-level alternation binds to the whole value on both branches,
// which is what the non-capturing group in the wrapper buys.
func TestRegexpFilter_AlternationBindsToTheWholeValue(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "alternation", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `draft|final`})

	for _, ok := range []string{"draft", "final"} {
		if status, body := env.putAsset(t, fid, map[string]any{"value_text": ok}); status != http.StatusOK {
			t.Errorf("%q must be accepted: status=%d body=%v", ok, status, body)
		}
	}
	for _, bad := range []string{"draft copy", "nearly final"} {
		status, body := env.putAsset(t, fid, map[string]any{"value_text": bad})
		assertValueRefused(t, status, body, "pattern_mismatch")
	}
}

// A pattern is a rule about a value, so a value must be present to
// break it. DELETE carries none and is unaffected on an optional field.
func TestRegexpFilter_ClearIsUnaffectedByThePattern(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "optclear", "text", nil)
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "abc"}); status != http.StatusOK {
		t.Fatalf("seed: status=%d body=%v", status, body)
	}
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `[0-9]+`})

	rr := deleteReq(t, env.router, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("clear on an optional field: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := readStored(t, env.pool, env.assetID, fid); ok {
		t.Errorf("the row should be gone")
	}
}

// Configuring a pattern does not rewrite or re-check what is already
// stored. The stored value here does not match the pattern that arrives
// after it, and it stays exactly where it was: a settings change is not
// a data migration.
func TestRegexpFilter_DoesNotTouchValuesAlreadyStored(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "retroactive", "text", nil)
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "legacy value"}); status != http.StatusOK {
		t.Fatalf("seed: status=%d body=%v", status, body)
	}

	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `[0-9]+`})

	v, ok := readStored(t, env.pool, env.assetID, fid)
	if !ok || v.Text == nil || *v.Text != "legacy value" {
		t.Fatalf("stored value = %+v; configuring a pattern must rewrite nothing", v)
	}
}

// ---------------------------------------------------------------------------
// 5. The pattern in force, on the collection plane
// ---------------------------------------------------------------------------

func TestRegexpFilter_CollectionWriteMatchingAndNot(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.collectionField(t, "shotcode", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `[A-Z]{3}_[0-9]{4}`})

	if status, body := env.putCollection(t, fid, map[string]any{"value_text": "BBB_0020"}); status != http.StatusOK {
		t.Fatalf("a matching value must be accepted: status=%d body=%v", status, body)
	}
	status, body := env.putCollection(t, fid, map[string]any{"value_text": "bbb"})
	assertValueRefused(t, status, body, "pattern_mismatch")
}

// The create-body gate, at the seam collections.Create calls
// PRE-TRANSACTION. Asserted here on the metadata half; the wiring that
// makes it run before the INSERT is asserted in the collections package.
func TestRegexpFilter_CollectionSeedGate(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.collectionField(t, "seedgate", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"regexp_filter": `[A-Z]{3}_[0-9]{4}`})

	h := metadata.NewHandler(env.pool, nil, nil)
	fieldUUID := uuid.MustParse(fid)
	ctx := context.Background()

	good := "CCC_0030"
	refusal, err := h.ValidateCollectionSeedValues(ctx, []metadata.CollectionSeedValueProbe{
		{FieldID: fieldUUID, ValueText: &good},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if refusal != nil {
		t.Fatalf("a matching seed must be accepted; got %+v", refusal)
	}

	bad := "nope"
	refusal, err = h.ValidateCollectionSeedValues(ctx, []metadata.CollectionSeedValueProbe{
		{FieldID: fieldUUID, ValueText: &bad},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if refusal == nil {
		t.Fatal("a non-matching seed must be refused before anything is written")
	}
	if refusal.Code == "" || refusal.Message == "" {
		t.Errorf("the refusal must name the field and say why; got %+v", refusal)
	}
}

// ---------------------------------------------------------------------------
// 6. read_only, and the human/system boundary
// ---------------------------------------------------------------------------

// The asset side refuses immediately, including where nothing is
// stored. There is no human first-write seam to protect on an asset:
// `POST /assets` writes no field-value rows at all.
func TestReadOnly_AssetSetRefusedEvenWhenEmpty(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "roempty", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"read_only": true})

	if _, ok := readStored(t, env.pool, env.assetID, fid); ok {
		t.Fatal("precondition: the field must hold nothing")
	}
	status, body := env.putAsset(t, fid, map[string]any{"value_text": "first value"})
	assertValueRefused(t, status, body, "field_read_only")

	if _, ok := readStored(t, env.pool, env.assetID, fid); ok {
		t.Error("a refused write must store nothing")
	}
}

// And it refuses the clear, which is the edit an operator would least
// like to discover was still permitted.
func TestReadOnly_AssetClearRefusedAndValueSurvives(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "roclear", "text", nil)
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "keep me"}); status != http.StatusOK {
		t.Fatalf("seed: status=%d body=%v", status, body)
	}
	mustPatchField(t, env, fid, map[string]any{"read_only": true})

	rr := deleteReq(t, env.router, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("clear on a read-only field: status=%d want 422; body=%s", rr.Code, rr.Body.String())
	}
	v, ok := readStored(t, env.pool, env.assetID, fid)
	if !ok || v.Text == nil || *v.Text != "keep me" {
		t.Errorf("the existing value must survive a refused clear; got %+v", v)
	}
}

func TestReadOnly_CollectionSetAndClearRefused(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.collectionField(t, "roboth", "text", nil)
	if status, body := env.putCollection(t, fid, map[string]any{"value_text": "seeded"}); status != http.StatusOK {
		t.Fatalf("seed: status=%d body=%v", status, body)
	}
	mustPatchField(t, env, fid, map[string]any{"read_only": true})

	status, body := env.putCollection(t, fid, map[string]any{"value_text": "changed"})
	assertValueRefused(t, status, body, "field_read_only")

	rr := deleteReq(t, env.router, fmt.Sprintf("/collections/%s/fields/%s", env.collID, fid))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("clear on a read-only collection field: status=%d want 422; body=%s", rr.Code, rr.Body.String())
	}

	var stored *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT value_text FROM collection_field_value WHERE collection_id = $1 AND field_id = $2`,
		env.collID, fid).Scan(&stored); err != nil {
		t.Fatalf("read stored collection value: %v", err)
	}
	if stored == nil || *stored != "seeded" {
		t.Errorf("stored value = %v, want the seeded one intact", stored)
	}
}

// The collection side's asymmetry: seeding an initial value through the
// CREATE body is the one human write a read-only collection field
// allows, because that body is a first-write seam the asset side has no
// equivalent of. The gate that runs there does not consult `read_only`.
func TestReadOnly_CollectionCreateSeedIsPermitted(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.collectionField(t, "roseed", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"read_only": true})

	h := metadata.NewHandler(env.pool, nil, nil)
	v := "initial"
	refusal, err := h.ValidateCollectionSeedValues(context.Background(), []metadata.CollectionSeedValueProbe{
		{FieldID: uuid.MustParse(fid), ValueText: &v},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if refusal != nil {
		t.Fatalf("the create-body seed must be permitted on a read-only collection field; got %+v", refusal)
	}
}

// THE COUNTERWEIGHT, and the test that makes `read_only` mean what its
// column comment says.
//
// The field refuses every write this suite can make over HTTP, and the
// upload-defaults applier fills it anyway — with a value that also
// fails the pattern configured on it. Both exemptions at once, both
// obtained from the CALL SITE rather than from anything the request
// carried: ApplyAssetDefaults is a different function with no route.
//
// Without this, `read_only` would be indistinguishable from a freeze,
// and a suite asserting only the refusals would pass on a build that
// had frozen the column outright.
func TestReadOnly_SystemWritersAreUnaffected(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "sysowned", "text", nil)
	mustPatchField(t, env, fid, map[string]any{
		"read_only":     true,
		"regexp_filter": `[0-9]+`,
		"default_value": map[string]any{"kind": "literal", "value_text": "system wrote this"},
	})

	// The human path is shut, on both rules.
	status, body := env.putAsset(t, fid, map[string]any{"value_text": "12345"})
	assertValueRefused(t, status, body, "field_read_only")

	applied := mustApplyDefaults(t, env.pool, env.assetID)
	if len(applied) == 0 {
		t.Fatal("the defaults applier reported nothing applied")
	}
	v, ok := readStored(t, env.pool, env.assetID, fid)
	if !ok || v.Text == nil || *v.Text != "system wrote this" {
		t.Fatalf("stored value = %+v; a system writer must still be able to fill a read-only field", v)
	}
	if v.SetBy != "default" {
		t.Errorf("set_by = %q; provenance is evidence of which writer ran, and it should say so", v.SetBy)
	}
}

// A read-only field does not make its neighbours read-only.
func TestReadOnly_IsPerField(t *testing.T) {
	env := newVocabEnv(t)
	locked := env.assetField(t, "lockeda", "text", nil)
	open := env.assetField(t, "openb", "text", nil)
	mustPatchField(t, env, locked, map[string]any{"read_only": true})

	if status, body := env.putAsset(t, open, map[string]any{"value_text": "still writable"}); status != http.StatusOK {
		t.Fatalf("the neighbouring field must stay writable: status=%d body=%v", status, body)
	}
	if got := readInputRules(t, env, open); got.ReadOnly {
		t.Error("the neighbouring field must not have been locked")
	}
}

// It is a dial. Turning it back off restores ordinary writing, so an
// operator who sets it by mistake is not stuck.
func TestReadOnly_IsADial(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "rodial", "text", nil)
	mustPatchField(t, env, fid, map[string]any{"read_only": true})
	mustPatchField(t, env, fid, map[string]any{"read_only": false})

	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "writable again"}); status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
}

// ---------------------------------------------------------------------------
// 7. The API states both settings rather than omitting them
// ---------------------------------------------------------------------------

// A surface deciding whether to offer an editable control has to read
// the operator's answer. An absent key would make it guess, which is
// the failure the participation flags were shipped to end.
func TestInputRules_AreOnTheFieldRepresentation(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "representation", "text", nil)

	body := getJSONBody(t, env.router, "/fields/"+fid)
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["read_only"]; !ok {
		t.Error("read_only must be stated, not omitted")
	}
	if raw["read_only"] != false {
		t.Errorf("read_only = %v, want the column's default", raw["read_only"])
	}
	// `regexp_filter` is nil-as-null: absent-or-null is the one
	// canonical "no constraint", so either serialisation reads the same.
	if v, ok := raw["regexp_filter"]; ok && v != nil {
		t.Errorf("regexp_filter = %v, want null on a field nobody configured", v)
	}

	mustPatchField(t, env, fid, map[string]any{"read_only": true, "regexp_filter": `z+`})
	body = getJSONBody(t, env.router, "/fields/"+fid)
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw["read_only"] != true || raw["regexp_filter"] != "z+" {
		t.Errorf("after configuring: read_only=%v regexp_filter=%v", raw["read_only"], raw["regexp_filter"])
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}
