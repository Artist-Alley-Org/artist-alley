// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// `display_condition` OVER THE WIRE (#1173, #1119, ADR 0099 §1 and §6).
//
// The graph algebra is tested as pure functions in
// display_condition_config_test.go, where a three-edge cycle costs one
// map literal instead of four field definitions and three round trips.
// THIS file proves the other half, which that one cannot: that the column
// exists, that the API carries it, that the PATCH semantics are the ones
// the schema promises, that the refusals are actually WIRED to the
// endpoint rather than merely implemented, and that a stored condition
// survives the drift the runtime is supposed to absorb.
package metadata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

// metadataControllerState and evaluateDrift are thin aliases so the drift
// test can express "what a form would see" without importing the
// evaluator's types into every assertion.
type metadataControllerState = metadata.ControllerState

func evaluateDrift(cond []string, resolve func(string) (metadata.ControllerState, bool)) bool {
	return metadata.EvaluateDisplayCondition(cond, resolve)
}

type dcEnv struct {
	pool   *pgxpool.Pool
	router chi.Router
}

func newDCEnv(t *testing.T) *dcEnv {
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
	router, _ := makeRouter(t, pool /*admin=*/, true)
	return &dcEnv{pool: pool, router: router}
}

func (e *dcEnv) field(t *testing.T, code, ftype string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"code": "metadata_test_" + code, "label": code, "type": ftype}
	for k, v := range extra {
		body[k] = v
	}
	return mustCreateField(t, e.router, body)
}

// storedCondition reads the column DIRECTLY, so the round-trip assertion
// is about what is PERSISTED rather than about what the handler echoed
// back. A handler that echoed its own request body would satisfy a
// response-only assertion while writing nothing.
func (e *dcEnv) storedCondition(t *testing.T, fieldID string) (raw []byte, isNull bool) {
	t.Helper()
	var out []byte
	if err := e.pool.QueryRow(context.Background(),
		`SELECT display_condition FROM field_definition WHERE id = $1`, fieldID).Scan(&out); err != nil {
		t.Fatalf("read stored condition: %v", err)
	}
	return out, out == nil
}

func (e *dcEnv) apiCondition(t *testing.T, fieldID string) []string {
	t.Helper()
	rr := getJSON(t, e.router, "/fields/"+fieldID)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET field: %d %s", rr.Code, rr.Body.String())
	}
	var def struct {
		DisplayCondition []string `json:"display_condition"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &def); err != nil {
		t.Fatalf("decode field: %v", err)
	}
	return def.DisplayCondition
}

// ---------------------------------------------------------------------------
// A-1 — the PATCH and storage contract
// ---------------------------------------------------------------------------

// TestDisplayCondition_PatchRoundTrip covers the four PATCH states in one
// sequence, because they are only meaningful in relation to each other:
// "omitted leaves unchanged" says nothing unless something was set first,
// and "present replaces the whole array" is indistinguishable from a
// merge until the second write is SHORTER than the first.
func TestDisplayCondition_PatchRoundTrip(t *testing.T) {
	env := newDCEnv(t)
	env.field(t, "ctrl_a", "text", nil)
	env.field(t, "ctrl_b", "text", nil)
	dep := env.field(t, "dep", "text", nil)

	// Floor: a new field has NO condition, and the member is absent
	// rather than an empty array.
	if _, isNull := env.storedCondition(t, dep); !isNull {
		t.Fatal("a newly created field must store SQL NULL, the canonical unset")
	}
	if got := env.apiCondition(t, dep); got != nil {
		t.Fatalf("a field with no condition must not send the member; got %#v", got)
	}

	// SET two terms.
	want := []string{"metadata_test_ctrl_a=Commission", "metadata_test_ctrl_b~urgent"}
	rr := patchJSON(t, env.router, "/fields/"+dep, map[string]any{"display_condition": want})
	if rr.Code != http.StatusOK {
		t.Fatalf("set condition: %d %s", rr.Code, rr.Body.String())
	}
	if got := env.apiCondition(t, dep); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("stored condition = %#v, want %#v", got, want)
	}

	// OMITTED leaves it unchanged. An unrelated property moves.
	if rr := patchJSON(t, env.router, "/fields/"+dep, map[string]any{"label": "renamed"}); rr.Code != http.StatusOK {
		t.Fatalf("unrelated patch: %d %s", rr.Code, rr.Body.String())
	}
	if got := env.apiCondition(t, dep); len(got) != 2 {
		t.Fatalf("an omitted display_condition must leave the stored one alone; got %#v", got)
	}

	// PRESENT REPLACES THE WHOLE ARRAY. The replacement is SHORTER, which
	// is what makes this distinguishable from a merge: a merge would
	// leave two terms.
	one := []string{"metadata_test_ctrl_b~urgent"}
	if rr := patchJSON(t, env.router, "/fields/"+dep, map[string]any{"display_condition": one}); rr.Code != http.StatusOK {
		t.Fatalf("replace condition: %d %s", rr.Code, rr.Body.String())
	}
	got := env.apiCondition(t, dep)
	if len(got) != 1 || got[0] != one[0] {
		t.Fatalf("display_condition must REPLACE the whole array, not merge; got %#v", got)
	}

	// CLEAR sets SQL NULL.
	if rr := patchJSON(t, env.router, "/fields/"+dep, map[string]any{"clear_display_condition": true}); rr.Code != http.StatusOK {
		t.Fatalf("clear condition: %d %s", rr.Code, rr.Body.String())
	}
	if _, isNull := env.storedCondition(t, dep); !isNull {
		t.Fatal("clear_display_condition must write SQL NULL, not an empty array")
	}
	if got := env.apiCondition(t, dep); got != nil {
		t.Fatalf("a cleared condition must not send the member; got %#v", got)
	}

	// BOTH SENT is a 400.
	rr = patchJSON(t, env.router, "/fields/"+dep, map[string]any{
		"display_condition":       one,
		"clear_display_condition": true,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("sending both members: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestDisplayCondition_EmptyArrayIsRefusedRatherThanTreatedAsAClear.
//
// `[]` and "remove the condition" being the same request is precisely the
// second spelling of unset the CHECK exists to prevent, so the API
// refuses it with a sentence pointing at the member that does mean
// removal.
func TestDisplayCondition_EmptyArrayIsRefused(t *testing.T) {
	env := newDCEnv(t)
	env.field(t, "ctrl", "text", nil)
	dep := env.field(t, "dep", "text", nil)
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"display_condition": []string{"metadata_test_ctrl=x"}}); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	rr := patchJSON(t, env.router, "/fields/"+dep, map[string]any{"display_condition": []string{}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty array: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "clear_display_condition") {
		t.Errorf("the refusal should point at the member that DOES remove a condition; body=%s", rr.Body.String())
	}
	// And it did not silently clear.
	if got := env.apiCondition(t, dep); len(got) != 1 {
		t.Fatalf("a refused empty array must leave the stored condition alone; got %#v", got)
	}
}

// TestDisplayCondition_CheckConstraintRefusesEverySecondSpellingOfUnset
// goes UNDER the API deliberately.
//
// The handler refuses `[]` and a blank term with a sentence, so the CHECK
// is never reached through HTTP. That is the right division of labour and
// it is also why the constraint needs its own test: it is the backstop
// for every writer that is not this handler — a migration, a seed, a
// future import runtime, an operator at psql.
func TestDisplayCondition_CheckConstraintRefusesEverySecondSpellingOfUnset(t *testing.T) {
	env := newDCEnv(t)
	fid := env.field(t, "chk", "text", nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		json string
	}{
		{"empty array", `[]`},
		{"object", `{}`},
		{"bare empty string", `""`},
		{"JSON null", `null`},
		{"array holding an empty string", `[""]`},
		{"array holding a whitespace-only string", `["   "]`},
		{"array holding a number", `[1]`},
		{"array holding JSON null", `["a=b", null]`},
		{"array holding an object", `[{"code":"a"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.pool.Exec(ctx,
				`UPDATE field_definition SET display_condition = $2::jsonb WHERE id = $1`, fid, tc.json)
			if err == nil {
				t.Fatalf("the CHECK accepted %s; NULL must be the only representation of unset", tc.json)
			}
			if !strings.Contains(err.Error(), "display_condition_shape_check") {
				t.Fatalf("refused by the wrong constraint: %v", err)
			}
		})
	}

	// The counterweight: the shapes that ARE legal go in. Without this a
	// constraint that refused everything would pass every case above.
	for _, ok := range []string{`null`, `["a=b"]`, `["a=b","c~d"]`} {
		var stmt string
		if ok == `null` {
			stmt = `UPDATE field_definition SET display_condition = NULL WHERE id = $1`
			if _, err := env.pool.Exec(ctx, stmt, fid); err != nil {
				t.Fatalf("SQL NULL must be accepted: %v", err)
			}
			continue
		}
		if _, err := env.pool.Exec(ctx,
			`UPDATE field_definition SET display_condition = $2::jsonb WHERE id = $1`, fid, ok); err != nil {
			t.Fatalf("the CHECK refused a legal shape %s: %v", ok, err)
		}
	}
}

// TestDisplayCondition_NotOnCreate: the property is UPDATE-ONLY, so a
// create body carrying it must not store one. A create cannot reference a
// graph that does not exist yet.
func TestDisplayCondition_NotOnCreate(t *testing.T) {
	env := newDCEnv(t)
	env.field(t, "ctrl", "text", nil)
	rr := postJSON(t, env.router, "/fields", map[string]any{
		"code": "metadata_test_createcond", "label": "c", "type": "text",
		"display_condition": []string{"metadata_test_ctrl=x"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var def struct {
		Id string `json:"id"`
	}
	mustDecode(t, rr.Body.Bytes(), &def)
	if _, isNull := env.storedCondition(t, def.Id); !isNull {
		t.Fatal("FieldDefinitionCreate does not carry display_condition; a create body naming one must store NULL")
	}
}

// ---------------------------------------------------------------------------
// The refusals are WIRED, not merely implemented
// ---------------------------------------------------------------------------

// TestDisplayCondition_RefusalsReachTheEndpoint drives one case per rule
// through PATCH. The exhaustive matrix lives in the pure-function suite;
// what this proves is that the validator is actually called, on the right
// graph, with the right dependent.
func TestDisplayCondition_RefusalsReachTheEndpoint(t *testing.T) {
	env := newDCEnv(t)
	env.field(t, "text_ctrl", "text", nil)
	env.field(t, "bool_ctrl", "boolean", nil)
	env.field(t, "t1_only", "text", map[string]any{"applies_to": []int64{1}})
	env.field(t, "t2_only", "text", map[string]any{"applies_to": []int64{2}})
	archived := env.field(t, "arch_ctrl", "text", nil)
	if rr := patchJSON(t, env.router, "/fields/"+archived, map[string]any{"status": "archived"}); rr.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}

	dep := env.field(t, "dep", "text", nil)
	both := env.field(t, "dep_both", "text", map[string]any{"applies_to": []int64{1, 2}})

	// The mirrored controller and the mirrored dependent both need a real
	// mirrored definition, which is `title` — created by migration 00044
	// and not by this test, so it is looked up rather than made.
	var titleID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id::text FROM field_definition WHERE mirrors_column = 'title' AND subject_kind = 'asset' LIMIT 1`).Scan(&titleID); err != nil {
		t.Fatalf("find the mirrored title definition: %v", err)
	}

	for _, tc := range []struct {
		name     string
		field    string
		cond     []string
		contains string
	}{
		{"malformed term", dep, []string{"no operator here"}, "not a valid"},
		{"unknown controller", dep, []string{"metadata_test_nosuch=x"}, "does not have"},
		{"unsupported pairing", dep, []string{"metadata_test_bool_ctrl=1"}, "cannot be used as a condition"},
		{"range bound", dep, []string{"metadata_test_text_ctrl>=x"}, "accepts only"},
		{"self reference", dep, []string{"metadata_test_dep=x"}, "cannot depend on itself"},
		{"archived controller", dep, []string{"metadata_test_arch_ctrl=x"}, "is archived"},
		{"contradiction", dep, []string{"metadata_test_text_ctrl=a", "metadata_test_text_ctrl=b"}, "two different values at once"},
		{"mirrored controller", dep, []string{"title=x"}, "mirrors the"},
		{
			"N-WAY applicability, which a pairwise check would accept",
			both,
			[]string{"metadata_test_t1_only=x", "metadata_test_t2_only=x"},
			"never appear on the same asset type",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := patchJSON(t, env.router, "/fields/"+tc.field, map[string]any{"display_condition": tc.cond})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.contains) {
				t.Fatalf("refusal does not say %q; body=%s", tc.contains, rr.Body.String())
			}
			if _, isNull := env.storedCondition(t, tc.field); !isNull {
				t.Fatal("a refused configuration must write nothing")
			}
		})
	}

	// The N-way case's own counterweight: EITHER controller alone is
	// accepted. Without this the refusal above could be caused by
	// anything, including a rule that refuses two terms.
	for _, one := range []string{"metadata_test_t1_only=x", "metadata_test_t2_only=x"} {
		rr := patchJSON(t, env.router, "/fields/"+both, map[string]any{"display_condition": []string{one}})
		if rr.Code != http.StatusOK {
			t.Fatalf("one controller alone must be accepted (%s): %d %s", one, rr.Code, rr.Body.String())
		}
	}
}

// TestDisplayCondition_MirroredDependentIsRefused is the other direction,
// on the real `title` definition.
func TestDisplayCondition_MirroredDependentIsRefused(t *testing.T) {
	env := newDCEnv(t)
	env.field(t, "ctrl", "text", nil)
	var titleID string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id::text FROM field_definition WHERE mirrors_column = 'title' AND subject_kind = 'asset' LIMIT 1`).Scan(&titleID); err != nil {
		t.Fatalf("find the mirrored title definition: %v", err)
	}
	rr := patchJSON(t, env.router, "/fields/"+titleID,
		map[string]any{"display_condition": []string{"metadata_test_ctrl=x"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "cannot carry a display condition") {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if _, isNull := env.storedCondition(t, titleID); !isNull {
		t.Fatal("nothing may have been written to the mirrored definition")
	}
}

// TestDisplayCondition_CycleAcrossThreeSeparateWrites is the cycle rule
// as an operator meets it: three PATCHes, the first two accepted.
//
// A validator that only looked at the new edge's direct controllers would
// accept all three, so the first two acceptances are as load-bearing as
// the refusal.
func TestDisplayCondition_CycleAcrossThreeSeparateWrites(t *testing.T) {
	env := newDCEnv(t)
	a := env.field(t, "cyc_a", "text", nil)
	b := env.field(t, "cyc_b", "text", nil)
	c := env.field(t, "cyc_c", "text", nil)

	if rr := patchJSON(t, env.router, "/fields/"+a,
		map[string]any{"display_condition": []string{"metadata_test_cyc_b=x"}}); rr.Code != http.StatusOK {
		t.Fatalf("A -> B must be accepted: %d %s", rr.Code, rr.Body.String())
	}
	if rr := patchJSON(t, env.router, "/fields/"+b,
		map[string]any{"display_condition": []string{"metadata_test_cyc_c=x"}}); rr.Code != http.StatusOK {
		t.Fatalf("B -> C must be accepted: %d %s", rr.Code, rr.Body.String())
	}
	rr := patchJSON(t, env.router, "/fields/"+c,
		map[string]any{"display_condition": []string{"metadata_test_cyc_a=x"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("C -> A closes the ring and must be refused: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "would create a loop") {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if _, isNull := env.storedCondition(t, c); !isNull {
		t.Fatal("the refused edge must not have been written")
	}
}

// TestDisplayCondition_SubjectKindsCannotCross, over the wire.
func TestDisplayCondition_SubjectKindsCannotCross(t *testing.T) {
	env := newDCEnv(t)
	env.field(t, "assetside", "text", nil)
	collDep := mustCreateField(t, env.router, map[string]any{
		"code": collectionTestPrefix + "dep", "label": "d", "type": "text",
		"subject_kind": "collection",
	})
	rr := patchJSON(t, env.router, "/fields/"+collDep,
		map[string]any{"display_condition": []string{"metadata_test_assetside=x"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "describes a asset") {
		t.Fatalf("body=%s", rr.Body.String())
	}

	// And a COLLECTION controller on a collection dependent is fine, so
	// the refusal above is about the kind and not about collections.
	mustCreateField(t, env.router, map[string]any{
		"code": collectionTestPrefix + "ctrl", "label": "c", "type": "text",
		"subject_kind": "collection",
	})
	if rr := patchJSON(t, env.router, "/fields/"+collDep,
		map[string]any{"display_condition": []string{collectionTestPrefix + "ctrl=x"}}); rr.Code != http.StatusOK {
		t.Fatalf("a collection controller on a collection dependent must be accepted: %d %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// A-19 — LATER-ARCHIVE DRIFT
// ---------------------------------------------------------------------------

// TestDisplayCondition_LaterArchiveDriftPreservesConfiguration walks the
// seven steps of the drift rule, on a controller that was genuinely
// ACTIVE and genuinely evaluable first.
//
// Starting from an already-archived or never-existing controller would
// make the whole test vacuous: fail-open would be observed for a
// condition that had never worked, which proves nothing about drift.
func TestDisplayCondition_LaterArchiveDriftPreservesConfiguration(t *testing.T) {
	env := newDCEnv(t)
	ctrl := env.field(t, "drift_ctrl", "text", nil)
	dep := env.field(t, "drift_dep", "text", nil)
	cond := []string{"metadata_test_drift_ctrl=Commission"}

	// 1. Configure a VALID condition against an ACTIVE controller.
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"display_condition": cond}); rr.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rr.Code, rr.Body.String())
	}

	// 2. It EVALUATES NORMALLY, both arms. The evaluator is fed the same
	// state a form would build: the definition resolves, is readable, and
	// holds a value.
	resolve := func(text string) func(string) (metadataControllerState, bool) {
		return func(code string) (metadataControllerState, bool) {
			if code == "metadata_test_drift_ctrl" {
				return metadataControllerState{Type: "text", Readable: true, Text: text}, true
			}
			return metadataControllerState{}, false
		}
	}
	if !evaluateDrift(cond, resolve("Commission")) {
		t.Fatal("step 2 (true arm): a matching controller value must SHOW the dependent")
	}
	if evaluateDrift(cond, resolve("Personal")) {
		t.Fatal("step 2 (false arm): a non-matching controller value must HIDE the dependent")
	}

	// 3. ARCHIVE the controller.
	if rr := patchJSON(t, env.router, "/fields/"+ctrl,
		map[string]any{"status": "archived"}); rr.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}

	// 4. The stored condition was NOT rewritten or cleared.
	if got := env.apiCondition(t, dep); len(got) != 1 || got[0] != cond[0] {
		t.Fatalf("archiving a controller must not rewrite the dependent's stored condition; got %#v", got)
	}
	raw, isNull := env.storedCondition(t, dep)
	if isNull {
		t.Fatal("archiving a controller must not CLEAR the dependent's stored condition")
	}
	if !strings.Contains(string(raw), "metadata_test_drift_ctrl") {
		t.Fatalf("stored condition was rewritten: %s", raw)
	}

	// 5. The dependent is now SHOWN, via whole-condition fail-open,
	// because an archived definition is not in the composition set — so
	// the controller does not resolve at all.
	unresolvable := func(string) (metadataControllerState, bool) {
		return metadataControllerState{}, false
	}
	if !evaluateDrift(cond, unresolvable) {
		t.Fatal("step 5: with the controller gone from the composition set the whole condition is unevaluable and the dependent must be SHOWN")
	}

	// 6. RESTORE the controller to a composition-eligible status. Note
	// DEPRECATED rather than active: the rule is about composition
	// eligibility, and deprecated definitions are rendered on edit
	// surfaces, so this is the stricter restoration to prove.
	if rr := patchJSON(t, env.router, "/fields/"+ctrl,
		map[string]any{"status": "deprecated"}); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}

	// 7. ORDINARY EVALUATION RESUMES, both arms again.
	if !evaluateDrift(cond, resolve("Commission")) {
		t.Fatal("step 7 (true arm): evaluation must resume once the controller is composition-eligible again")
	}
	if evaluateDrift(cond, resolve("Personal")) {
		t.Fatal("step 7 (false arm): evaluation must resume as a real conjunction, not as a permanent fail-open")
	}
}

// TestDisplayCondition_ArchivedDependentKeepsItsConfiguration is the
// other side of the same rule: an archived DEPENDENT is not forbidden its
// configuration either.
func TestDisplayCondition_ArchivedDependentKeepsItsConfiguration(t *testing.T) {
	env := newDCEnv(t)
	env.field(t, "keep_ctrl", "text", nil)
	dep := env.field(t, "keep_dep", "text", nil)
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"display_condition": []string{"metadata_test_keep_ctrl=x"}}); rr.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rr.Code, rr.Body.String())
	}
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"status": "archived"}); rr.Code != http.StatusOK {
		t.Fatalf("archive dependent: %d %s", rr.Code, rr.Body.String())
	}
	if _, isNull := env.storedCondition(t, dep); isNull {
		t.Fatal("archiving a DEPENDENT must not clear its stored condition either")
	}
}

// TestDisplayCondition_ClearIsAlwaysAvailable: a clear is unguarded by
// the refusal list, so a setting can be taken back off a field it could
// no longer be put on. Here the controller is archived AFTER the
// configuration, which would make a re-SET refused.
func TestDisplayCondition_ClearIsAlwaysAvailable(t *testing.T) {
	env := newDCEnv(t)
	ctrl := env.field(t, "gone_ctrl", "text", nil)
	dep := env.field(t, "gone_dep", "text", nil)
	cond := []string{"metadata_test_gone_ctrl=x"}
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"display_condition": cond}); rr.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rr.Code, rr.Body.String())
	}
	if rr := patchJSON(t, env.router, "/fields/"+ctrl,
		map[string]any{"status": "archived"}); rr.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}
	// Re-SETTING the same condition is now refused...
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"display_condition": cond}); rr.Code != http.StatusBadRequest {
		t.Fatalf("re-setting against an archived controller should be refused: %d %s", rr.Code, rr.Body.String())
	}
	// ...and CLEARING it is still allowed.
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"clear_display_condition": true}); rr.Code != http.StatusOK {
		t.Fatalf("a clear must always be available: %d %s", rr.Code, rr.Body.String())
	}
	if _, isNull := env.storedCondition(t, dep); !isNull {
		t.Fatal("the clear did not write NULL")
	}
}

// TestDisplayCondition_DeprecatedControllerIsAccepted over the wire, so
// the status discriminator is proven at the endpoint and not only in the
// pure validator.
func TestDisplayCondition_DeprecatedControllerIsAccepted(t *testing.T) {
	env := newDCEnv(t)
	ctrl := env.field(t, "depr_ctrl", "text", nil)
	dep := env.field(t, "depr_dep", "text", nil)
	if rr := patchJSON(t, env.router, "/fields/"+ctrl,
		map[string]any{"status": "deprecated"}); rr.Code != http.StatusOK {
		t.Fatalf("deprecate: %d %s", rr.Code, rr.Body.String())
	}
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"display_condition": []string{"metadata_test_depr_ctrl=x"}}); rr.Code != http.StatusOK {
		t.Fatalf("a DEPRECATED controller must be accepted: %d %s", rr.Code, rr.Body.String())
	}
}

// TestDisplayCondition_HidingIsNotAWriteGate is the ADR 0012 amendment's
// promise, stated where it can be checked: a field with a condition is
// still writable through the ordinary value endpoint, whatever the
// condition currently says. A condition is composition, never
// authorization.
func TestDisplayCondition_HidingIsNotAWriteGate(t *testing.T) {
	env := newDCEnv(t)
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	assetID := mustInsertAsset(t, env.pool, 420000)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = env.pool.Exec(c, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, assetID)
		_, _ = env.pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
		_, _ = env.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, assetID)
	})

	env.field(t, "gate_ctrl", "text", nil)
	dep := env.field(t, "gate_dep", "text", nil)
	if rr := patchJSON(t, env.router, "/fields/"+dep,
		map[string]any{"display_condition": []string{"metadata_test_gate_ctrl=never"}}); rr.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rr.Code, rr.Body.String())
	}
	// The controller holds nothing, so the condition is FALSE and a form
	// would draw no control. The value endpoint does not care.
	rr := putJSON(t, env.router, fmt.Sprintf("/assets/%s/fields/%s", assetID, dep),
		map[string]any{"value_text": "written anyway", "if_absent": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("a conditioned field must stay writable through the value API: %d %s", rr.Code, rr.Body.String())
	}
}
