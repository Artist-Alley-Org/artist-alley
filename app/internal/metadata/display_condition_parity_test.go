// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// CROSS-LANGUAGE PARITY for the display-condition grammar (#1173,
// #1119, ADR 0099 §2).
//
// # Why this file is not just "parser tests"
//
// `display_condition` is parsed and evaluated in TWO LANGUAGES. The
// authority is Go — `facet.SplitFieldTerm`, the same function the search
// `filter=field:...` predicate uses, reused verbatim so a condition and a
// filter cannot disagree about what a term means. THE BROWSER CANNOT CALL
// IT, so `web/src/lib/displayCondition.ts` reimplements the same grammar,
// and two implementations of one rule drift.
//
// This test reads `web/src/lib/displayCondition.cases.json`, and so does
// `web/src/lib/displayCondition.test.ts`. Neither suite carries its own
// copy of the case list, so a change that moves one plane fails on the
// OTHER plane's test rather than being discovered months later by an
// operator whose field will not appear. That is 20a's
// `fieldEmptiness.cases.json` precedent applied to a second rule.
//
// ⛔ DO NOT ADD A CASE HERE. Add it to the JSON; both planes pick it up.
//
// These are unit tests over pure functions: no database, no router, and
// therefore no skip. They run everywhere.
package metadata_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

const displayConditionCasesPath = "../../../web/src/lib/displayCondition.cases.json"

type dcParseCase struct {
	Why   string `json:"why"`
	Input string `json:"input"`
	OK    bool   `json:"ok"`
	Code  string `json:"code"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

type dcMatrixCase struct {
	Type    string `json:"type"`
	Op      string `json:"op"`
	Allowed bool   `json:"allowed"`
}

type dcController struct {
	Type     string   `json:"type"`
	Readable bool     `json:"readable"`
	Text     *string  `json:"text"`
	Options  []string `json:"options"`
}

type dcEvaluateCase struct {
	Why        string       `json:"why"`
	Term       string       `json:"term"`
	Controller dcController `json:"controller"`
	Match      bool         `json:"match"`
}

type dcConditionCase struct {
	Why         string                  `json:"why"`
	Condition   []string                `json:"condition"`
	Controllers map[string]dcController `json:"controllers"`
	Shown       bool                    `json:"shown"`
}

type dcCases struct {
	Parse     []dcParseCase     `json:"parse"`
	Matrix    []dcMatrixCase    `json:"matrix"`
	Evaluate  []dcEvaluateCase  `json:"evaluate"`
	Condition []dcConditionCase `json:"condition"`
}

func loadDisplayConditionCases(t *testing.T) dcCases {
	t.Helper()
	raw, err := os.ReadFile(displayConditionCasesPath)
	if err != nil {
		t.Fatalf("read shared corpus %s: %v", displayConditionCasesPath, err)
	}
	var c dcCases
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode shared corpus: %v", err)
	}
	// ANTI-VACUITY. A corpus that silently became empty — a rename, a bad
	// merge, a JSON key typo — would make every loop below iterate zero
	// times and report green while proving nothing. These floors are the
	// denominators the sprint reports.
	if len(c.Parse) < 12 {
		t.Fatalf("parse corpus has %d cases; the grammar needs at least 12 to cover its five properties", len(c.Parse))
	}
	if len(c.Matrix) < 20 {
		t.Fatalf("matrix corpus has %d cases; the operator/type table is bigger than that", len(c.Matrix))
	}
	if len(c.Evaluate) < 10 {
		t.Fatalf("evaluate corpus has %d cases", len(c.Evaluate))
	}
	if len(c.Condition) < 12 {
		t.Fatalf("condition corpus has %d cases; the cardinality matrix alone is six", len(c.Condition))
	}
	return c
}

func toController(c dcController) metadata.ControllerState {
	s := metadata.ControllerState{
		Type:     c.Type,
		Readable: c.Readable,
		Options:  c.Options,
	}
	if c.Text != nil {
		s.Text = *c.Text
		s.HasText = true
	}
	return s
}

// TestDisplayConditionParse_SharedCorpus pins the five parse properties
// against the same list the TypeScript suite reads.
func TestDisplayConditionParse_SharedCorpus(t *testing.T) {
	cases := loadDisplayConditionCases(t)
	for _, c := range cases.Parse {
		t.Run(c.Input, func(t *testing.T) {
			got, ok := metadata.ParseDisplayConditionTerm(c.Input)
			if ok != c.OK {
				t.Fatalf("ok = %v, want %v (%s)", ok, c.OK, c.Why)
			}
			if !c.OK {
				return
			}
			if got.Code != c.Code {
				t.Errorf("code = %q, want %q (%s)", got.Code, c.Code, c.Why)
			}
			if string(got.Op) != c.Op {
				t.Errorf("op = %q, want %q (%s)", got.Op, c.Op, c.Why)
			}
			if got.Value != c.Value {
				t.Errorf("value = %q, want %q (%s)", got.Value, c.Value, c.Why)
			}
		})
	}
}

// TestDisplayConditionMatrix_SharedCorpus pins the closed operator/type
// table, including the entries that are absent on purpose: `boolean`
// (20a changed its CONTROL, not its representation) and `>=` / `<=` (they
// are range bounds, which is a filtering question).
func TestDisplayConditionMatrix_SharedCorpus(t *testing.T) {
	cases := loadDisplayConditionCases(t)
	for _, c := range cases.Matrix {
		t.Run(c.Type+c.Op, func(t *testing.T) {
			got := metadata.DisplayConditionOpAllowedForTest(c.Type, facet.FieldOp(c.Op))
			if got != c.Allowed {
				t.Fatalf("allowed(%q, %q) = %v, want %v", c.Type, c.Op, got, c.Allowed)
			}
		})
	}
}

// TestDisplayConditionTermMatch_SharedCorpus pins the COMPARISON rules,
// and in particular the asymmetry that will bite an operator: the
// condition literal is trimmed and the stored value never is.
func TestDisplayConditionTermMatch_SharedCorpus(t *testing.T) {
	cases := loadDisplayConditionCases(t)
	for _, c := range cases.Evaluate {
		t.Run(c.Term+"/"+c.Controller.Type, func(t *testing.T) {
			term, ok := metadata.ParseDisplayConditionTerm(c.Term)
			if !ok {
				t.Fatalf("corpus term %q does not parse; an evaluate case must use a valid term", c.Term)
			}
			got := metadata.DisplayTermMatchesForTest(term, toController(c.Controller))
			if got != c.Match {
				t.Fatalf("match = %v, want %v (%s)", got, c.Match, c.Why)
			}
		})
	}
}

// TestDisplayConditionEvaluate_SharedCorpus is the whole-condition rule,
// including the six cardinality discriminators.
//
// Cases 4 and 5 are the ones a naive implementation fails: with one term
// FALSE and another unevaluable, "unknown counts as true inside the AND"
// evaluates `false AND true` and HIDES the field. Case 6 is the mirror
// trap, where a readable controller holding nothing must be a REAL FALSE.
func TestDisplayConditionEvaluate_SharedCorpus(t *testing.T) {
	cases := loadDisplayConditionCases(t)
	for _, c := range cases.Condition {
		t.Run(c.Why, func(t *testing.T) {
			resolve := func(code string) (metadata.ControllerState, bool) {
				ctrl, ok := c.Controllers[code]
				if !ok {
					return metadata.ControllerState{}, false
				}
				return toController(ctrl), true
			}
			got := metadata.EvaluateDisplayCondition(c.Condition, resolve)
			if got != c.Shown {
				t.Fatalf("shown = %v, want %v (%s)", got, c.Shown, c.Why)
			}
		})
	}
}

// TestDisplayConditionEvaluate_FailOpenIsNotUnknownIsTrue states the
// distinction the corpus encodes, directly and without the corpus, so
// that deleting a case from the JSON cannot quietly remove the rule.
//
// The two implementations differ on exactly one input shape, and this is
// it: one term FALSE, one term unevaluable. An AND that substituted
// `true` for the unknown would answer HIDDEN.
func TestDisplayConditionEvaluate_FailOpenIsNotUnknownIsTrue(t *testing.T) {
	resolve := func(code string) (metadata.ControllerState, bool) {
		if code == "known" {
			return metadata.ControllerState{Type: "text", Readable: true, Text: "no"}, true
		}
		return metadata.ControllerState{}, false
	}
	// `known=yes` is FALSE. `missing=x` is unevaluable.
	if !metadata.EvaluateDisplayCondition([]string{"known=yes", "missing=x"}, resolve) {
		t.Fatal("whole-condition fail-open was not applied: a false term plus an unevaluable term must SHOW the field, " +
			"and hiding it is precisely the 'unknown counts as true inside the AND' bug")
	}
	// Sanity: with both terms evaluable it really does hide, so the test
	// above is not passing because the evaluator shows everything.
	resolveBoth := func(code string) (metadata.ControllerState, bool) {
		return metadata.ControllerState{Type: "text", Readable: true, Text: "no"}, true
	}
	if metadata.EvaluateDisplayCondition([]string{"known=yes", "other=x"}, resolveBoth) {
		t.Fatal("evaluator shows a field whose terms are all false; the fail-open assertion above proves nothing")
	}
}

// TestDisplayConditionDecode covers the storage boundary the corpus
// cannot: SQL NULL is N=0, and a decoded empty array is normalised to the
// same nil rather than surviving as a second spelling of unset.
func TestDisplayConditionDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want int
		err  bool
	}{
		{"sql null", nil, 0, false},
		{"empty bytes", []byte{}, 0, false},
		{"empty array normalises to unset", []byte(`[]`), 0, false},
		{"one term", []byte(`["a=b"]`), 1, false},
		{"two terms", []byte(`["a=b","c~d"]`), 2, false},
		{"corrupt", []byte(`{"a":1}`), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := metadata.DecodeDisplayCondition(tc.in)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want error %v", err, tc.err)
			}
			if len(got) != tc.want {
				t.Fatalf("len = %d, want %d", len(got), tc.want)
			}
		})
	}
}
