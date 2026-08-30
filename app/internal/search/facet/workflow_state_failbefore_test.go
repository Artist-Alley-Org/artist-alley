// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18c — THE PORTABLE FAIL-BEFORE.
//
// ⛔ THIS FILE NAMES NO SYMBOL THAT 18C INTRODUCES. It uses
// [ParseSelection], [Selection.Terms] and [Term], all of which predate
// this sprint, and it drives the dimension by its WIRE SPELLING — a
// string literal — rather than by the constant.
//
// That is the whole reason it is a separate file. A test that referenced
// FacetWorkflowState could not be compiled against the previous commit,
// so running it there would prove nothing about the old behaviour; this
// one compiles and runs unchanged on both sides and FAILS on the old
// one, where `workflow_state` is not a registered dimension and
// ParseSelection answers ErrBadFilter (which the search handler renders
// as 400 `invalid_filter`).
//
// It asserts the REQUEST-SHAPE half only. Which rows come back is a
// claim about a population and lives in
// search/workflow_state_filter_test.go.
package facet

import "testing"

// TestWorkflowStateFilter_WireTokenIsAccepted drives the existing public
// value-parsing surface with the exact token a caller puts in the URL.
//
// `filter=workflow_state:asset:1/published` — [ParseSelection] cuts at
// the FIRST colon, so the dimension is `workflow_state` and the domain's
// own colon survives inside the value. No new wire rule was needed and
// this asserts that too.
func TestWorkflowStateFilter_WireTokenIsAccepted(t *testing.T) {
	for _, c := range []struct {
		token string
		value string
	}{
		{"workflow_state:asset:1/published", "asset:1/published"},
		{"workflow_state:asset:1/pending_review", "asset:1/pending_review"},
		{"workflow_state:none", "none"},
		{"workflow_state:post/published", "post/published"},
		{"workflow_state:asset:1/stage/final", "asset:1/stage/final"},
	} {
		t.Run(c.token, func(t *testing.T) {
			s, err := ParseSelection([]string{c.token})
			if err != nil {
				t.Fatalf("ParseSelection(%q) = %v, want it accepted.\n"+
					"  ⛔ THIS IS THE FAIL-BEFORE: on the previous commit `workflow_state`\n"+
					"  is not a registered dimension, so this is ErrBadFilter and the\n"+
					"  search handler answers 400 invalid_filter.", c.token, err)
			}
			terms := s.Terms()
			if len(terms) != 1 {
				t.Fatalf("%q produced %d terms, want 1", c.token, len(terms))
			}
			if string(terms[0].Type) != "workflow_state" {
				t.Errorf("%q parsed as dimension %q, want workflow_state", c.token, terms[0].Type)
			}
			if terms[0].Value != c.value {
				t.Errorf("%q carried value %q, want %q — only the FIRST colon separates,\n"+
					"  so the domain's own colon stays in the value", c.token, terms[0].Value, c.value)
			}
		})
	}
}

// TestWorkflowStateFilter_MalformedWireTokenIsRefused is the other half
// of the same public surface, and it passes on BOTH commits — for
// different reasons, which is exactly why it cannot stand alone as the
// fail-before.
//
// Before: the dimension is unknown. After: the dimension is known and
// the VALUE is malformed. Either way the caller gets 400 rather than a
// filter that looks applied and is not.
func TestWorkflowStateFilter_MalformedWireTokenIsRefused(t *testing.T) {
	for _, token := range []string{
		"workflow_state:published",
		"workflow_state:/published",
		"workflow_state:asset:1/",
	} {
		if _, err := ParseSelection([]string{token}); err == nil {
			t.Errorf("ParseSelection(%q) succeeded, want a rejection — a concrete "+
				"identity is <domain>/<code> with both halves non-empty", token)
		}
	}
}
