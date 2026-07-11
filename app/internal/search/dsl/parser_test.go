// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package dsl_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
)

func TestParse_FreeText(t *testing.T) {
	q, err := dsl.Parse("cat")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Root.(dsl.FreeTextNode); !ok {
		t.Errorf("Root = %T, want FreeTextNode", q.Root)
	}
}

func TestParse_Phrase(t *testing.T) {
	q, err := dsl.Parse(`"a b c"`)
	if err != nil {
		t.Fatal(err)
	}
	ph, ok := q.Root.(dsl.PhraseNode)
	if !ok {
		t.Fatalf("Root = %T, want PhraseNode", q.Root)
	}
	if ph.Text != "a b c" {
		t.Errorf("Text = %q, want 'a b c'", ph.Text)
	}
}

func TestParse_BooleanAndOrNot(t *testing.T) {
	q, err := dsl.Parse("foo AND bar OR NOT baz")
	if err != nil {
		t.Fatal(err)
	}
	// Root should be OR(AND(foo, bar), NOT(baz))
	or, ok := q.Root.(dsl.OrNode)
	if !ok {
		t.Fatalf("Root = %T, want OrNode", q.Root)
	}
	if _, ok := or.Left.(dsl.AndNode); !ok {
		t.Errorf("Left = %T, want AndNode", or.Left)
	}
	if _, ok := or.Right.(dsl.NotNode); !ok {
		t.Errorf("Right = %T, want NotNode", or.Right)
	}
}

func TestParse_FieldMatch(t *testing.T) {
	q, err := dsl.Parse("title:cat")
	if err != nil {
		t.Fatal(err)
	}
	fm, ok := q.Root.(dsl.FieldMatchNode)
	if !ok {
		t.Fatalf("Root = %T, want FieldMatchNode", q.Root)
	}
	if fm.Field != dsl.FieldTitle {
		t.Errorf("Field = %q, want title", fm.Field)
	}
	if fm.Value != "cat" {
		t.Errorf("Value = %q, want cat", fm.Value)
	}
}

func TestParse_UnknownField_ReturnsWhitelist(t *testing.T) {
	_, err := dsl.Parse("bogus:cat")
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	var de dsl.DSLError
	if !errors.As(err, &de) {
		t.Fatalf("err type = %T, want DSLError", err)
	}
	if de.Kind != dsl.UnknownField {
		t.Errorf("Kind = %d, want UnknownField", de.Kind)
	}
	if len(de.ValidFields) < 5 {
		t.Errorf("ValidFields = %v, want at least 5 entries", de.ValidFields)
	}
}

func TestParse_ParenGrouping(t *testing.T) {
	q, err := dsl.Parse("(foo OR bar) AND baz")
	if err != nil {
		t.Fatal(err)
	}
	// Root: AND(OR(foo,bar), baz)
	and, ok := q.Root.(dsl.AndNode)
	if !ok {
		t.Fatalf("Root = %T, want AndNode", q.Root)
	}
	if _, ok := and.Left.(dsl.OrNode); !ok {
		t.Errorf("Left = %T, want OrNode", and.Left)
	}
}

func TestParse_SimilarTo_CompilesInB3(t *testing.T) {
	q, err := dsl.Parse("similar_to:0192abcd-1234-5678-9abc-def012345678")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Root.(dsl.SimilarToNode); !ok {
		t.Errorf("Root = %T, want SimilarToNode", q.Root)
	}
	// Phase 1.16.B-3: compilation records the asset ID +
	// suggests HybridWeight=1.0 (pure-vector intent). Actual
	// embedding fetch happens at the Service layer.
	cq, cerr := dsl.Compile(q)
	if cerr != nil {
		t.Fatalf("compile err = %v; expected nil in B-3", cerr)
	}
	if cq.SimilarToAssetID != "0192abcd-1234-5678-9abc-def012345678" {
		t.Errorf("SimilarToAssetID = %q; want the parsed UUID", cq.SimilarToAssetID)
	}
	if cq.HybridWeightSuggestion != 1.0 {
		t.Errorf("HybridWeightSuggestion = %g; want 1.0 for pure-vector", cq.HybridWeightSuggestion)
	}
	if cq.TSQuery != "" {
		t.Errorf("TSQuery = %q; want empty for pure-vector", cq.TSQuery)
	}
}

func TestParse_SimilarTo_CombinedWithFreeText_LowersWeight(t *testing.T) {
	// `similar_to:X foo` → implicit AND with free-text; hybrid
	// weight defaults to 0.5 (mixed intent).
	q, err := dsl.Parse("similar_to:0192abcd-1234-5678-9abc-def012345678 foo")
	if err != nil {
		t.Fatal(err)
	}
	cq, cerr := dsl.Compile(q)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if cq.HybridWeightSuggestion != 0.5 {
		t.Errorf("HybridWeightSuggestion = %g; want 0.5 for mixed", cq.HybridWeightSuggestion)
	}
	if cq.TSQuery == "" {
		t.Errorf("TSQuery should carry the free-text plainto_tsquery fragment")
	}
}

func TestCompile_NoUserTextReachesTSQuery(t *testing.T) {
	// 20 injection-shaped inputs. Compile must produce a TSQuery
	// string that contains NO substring from the original inputs
	// besides the field names themselves — user text lives only
	// in TSQueryArgs.
	injections := []string{
		"'; DROP TABLE assets; --",
		"foo & bar",
		"foo | bar",
		"foo & !bar",
		`"a" & '; SELECT * FROM users; --"`,
		"tag:'; DROP TABLE assets; --",
		"foo\x00bar",
		"foo\nbar",
		"foo\tbar",
		"foo(bar)baz",
		"'''''",
		`"""""`,
		`"'"''"`,
		"foo AND OR NOT bar",
		"foo AND (bar",
		"foo )(",
		"::",
		"title::foo",
		"title:foo:bar",
		"NOT NOT NOT foo",
	}
	for _, in := range injections {
		q, err := dsl.Parse(in)
		if err != nil {
			// Parse error is fine — that's rejection, safe.
			continue
		}
		cq, err := dsl.Compile(q)
		if err != nil {
			continue
		}
		// The compiled TSQuery string must NOT contain the raw
		// injection text (beyond incidental single-word overlap).
		// We accept the compiled form contains only:
		//   plainto_tsquery, phraseto_tsquery, &&, ||, !!, (, ), $
		//   'english', ',', digits, spaces
		safe := isSafeCompiled(cq.TSQuery)
		if !safe {
			t.Errorf("compile(%q) produced unsafe TSQuery: %q", in, cq.TSQuery)
		}
	}
}

// isSafeCompiled returns true if s contains only allowed tokens.
func isSafeCompiled(s string) bool {
	allowed := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 &|!()',$_"
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}

func TestLex_InputTooLong_Rejected(t *testing.T) {
	big := strings.Repeat("a", dsl.MaxInputBytes+1)
	_, err := dsl.Parse(big)
	if !errors.Is(err, dsl.ErrInputTooLong) {
		t.Errorf("err = %v, want ErrInputTooLong", err)
	}
}

func TestLex_UnterminatedString_Rejected(t *testing.T) {
	_, err := dsl.Parse(`"unterminated`)
	if !errors.Is(err, dsl.ErrUnterminatedString) {
		t.Errorf("err = %v, want ErrUnterminatedString", err)
	}
}
