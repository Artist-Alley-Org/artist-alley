// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The CLOSED REFUSAL LIST as pure graph algebra (#1173, #1119,
// ADR 0099 §6).
//
// The end-to-end suite drives the same rules through PATCH and is what
// proves they are actually wired to the endpoint. This file is what makes
// the BOUNDARY cases affordable: a three-edge cycle takes three HTTP
// round trips and four field definitions to build over the wire, and the
// N-way applicability counterexample needs a dependent plus two
// controllers with disjoint `applies_to`. Building those as graph
// literals is what lets every case be stated rather than the two or three
// somebody had the patience for.
//
// No database and no router here, so no skip.
package metadata_test

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

type gnode = metadata.ConditionGraphNodeForTest

func strptr(s string) *string { return &s }

// validate is the shorthand every case below uses: an ASSET dependent of
// type `text` with no applies_to restriction and no mirror, unless the
// case says otherwise.
func validate(t *testing.T, proposed []string, graph map[string]gnode) string {
	t.Helper()
	return metadata.ValidateDisplayConditionConfigForTest(
		"dep", "text", "asset", nil, nil, proposed, graph)
}

func mustRefuse(t *testing.T, msg, contains string) {
	t.Helper()
	if msg == "" {
		t.Fatalf("expected a refusal mentioning %q, got acceptance", contains)
	}
	if !strings.Contains(msg, contains) {
		t.Fatalf("refusal %q does not mention %q; an operator has to be able to tell WHICH rule they broke", msg, contains)
	}
}

func mustAccept(t *testing.T, msg string) {
	t.Helper()
	if msg != "" {
		t.Fatalf("expected acceptance, got refusal: %s", msg)
	}
}

// ---------------------------------------------------------------------------
// The ordinary acceptance, so every refusal below is a real discriminator
// ---------------------------------------------------------------------------

func TestDisplayConditionConfig_AcceptsAnOrdinaryCondition(t *testing.T) {
	mustAccept(t, validate(t, []string{"ctrl=Commission"}, map[string]gnode{
		"ctrl": {Type: "text"},
	}))
}

// ---------------------------------------------------------------------------
// Malformed, unknown, subject kind, operator matrix
// ---------------------------------------------------------------------------

func TestDisplayConditionConfig_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		proposed []string
		graph    map[string]gnode
		contains string
	}{
		{
			name:     "a term with no operator",
			proposed: []string{"ctrl"},
			graph:    map[string]gnode{"ctrl": {Type: "text"}},
			contains: "not a valid",
		},
		{
			name:     "a term with an empty value",
			proposed: []string{"ctrl="},
			graph:    map[string]gnode{"ctrl": {Type: "text"}},
			contains: "not a valid",
		},
		{
			name:     "a field: prefix is a facet dimension, not a bare term",
			proposed: []string{"field:ctrl=x"},
			graph:    map[string]gnode{"ctrl": {Type: "text"}},
			contains: "not a valid",
		},
		{
			name:     "a controller this server does not have",
			proposed: []string{"nosuch=x"},
			graph:    map[string]gnode{"ctrl": {Type: "text"}},
			contains: "does not have",
		},
		{
			name:     "a controller describing the other subject kind",
			proposed: []string{"ctrl=x"},
			graph:    map[string]gnode{"ctrl": {Type: "text", Subject: "collection"}},
			contains: "describes a collection",
		},
		{
			name:     "~ on a select",
			proposed: []string{"ctrl~x"},
			graph:    map[string]gnode{"ctrl": {Type: "select"}},
			contains: "accepts only =",
		},
		{
			name:     "~ on a multi_select",
			proposed: []string{"ctrl~x"},
			graph:    map[string]gnode{"ctrl": {Type: "multi_select"}},
			contains: "accepts only =",
		},
		{
			name:     "= on a boolean, which stays excluded despite 20a's tri-state control",
			proposed: []string{"ctrl=1"},
			graph:    map[string]gnode{"ctrl": {Type: "boolean"}},
			contains: "cannot be used as a condition",
		},
		{
			name:     "= on a rich_text, whose stored value is sanitised markup",
			proposed: []string{"ctrl=x"},
			graph:    map[string]gnode{"ctrl": {Type: "rich_text"}},
			contains: "cannot be used as a condition",
		},
		{
			name:     "= on a number",
			proposed: []string{"ctrl=1"},
			graph:    map[string]gnode{"ctrl": {Type: "number"}},
			contains: "cannot be used as a condition",
		},
		{
			name:     "= on a reference",
			proposed: []string{"ctrl=x"},
			graph:    map[string]gnode{"ctrl": {Type: "reference"}},
			contains: "cannot be used as a condition",
		},
		{
			name:     ">= is a range bound and is refused even on a text field",
			proposed: []string{"ctrl>=x"},
			graph:    map[string]gnode{"ctrl": {Type: "text"}},
			contains: "accepts only",
		},
		{
			name:     "<= likewise",
			proposed: []string{"ctrl<=x"},
			graph:    map[string]gnode{"ctrl": {Type: "longtext"}},
			contains: "accepts only",
		},
		{
			name:     "a MIRRORED controller",
			proposed: []string{"ctrl=x"},
			graph:    map[string]gnode{"ctrl": {Type: "text", MirrorsColumn: strptr("title")}},
			contains: "mirrors the \"title\" column",
		},
		{
			name:     "an ALREADY ARCHIVED controller",
			proposed: []string{"ctrl=x"},
			graph:    map[string]gnode{"ctrl": {Type: "text", Status: "archived"}},
			contains: "is archived",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustRefuse(t, validate(t, tc.proposed, tc.graph), tc.contains)
		})
	}
}

// TestDisplayConditionConfig_MirroredDependent is the OTHER direction of
// the mirror rule, and it has to be tested separately because it is a
// property of the dependent rather than of any term.
func TestDisplayConditionConfig_MirroredDependent(t *testing.T) {
	msg := metadata.ValidateDisplayConditionConfigForTest(
		"title", "text", "asset", nil, strptr("title"),
		[]string{"ctrl=x"},
		map[string]gnode{"ctrl": {Type: "text"}},
	)
	mustRefuse(t, msg, "cannot carry a display condition")
}

// ---------------------------------------------------------------------------
// Controller STATUS: the discriminator, not just the refusal
// ---------------------------------------------------------------------------

// TestDisplayConditionConfig_ControllerStatusDiscriminator is A-13's
// point and the reason a bare "archived is refused" test is not enough:
// the interesting claim is that DEPRECATED is ACCEPTED. Edit surfaces
// deliberately render active and deprecated definitions together (#528),
// so a deprecated controller is a live thing on the surface that
// evaluates conditions.
func TestDisplayConditionConfig_ControllerStatusDiscriminator(t *testing.T) {
	for _, tc := range []struct {
		status string
		accept bool
	}{
		{"active", true},
		{"deprecated", true},
		{"archived", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			msg := validate(t, []string{"ctrl=x"}, map[string]gnode{
				"ctrl": {Type: "text", Status: tc.status},
			})
			if tc.accept {
				mustAccept(t, msg)
			} else {
				mustRefuse(t, msg, "is archived")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cycles, walked across the WHOLE graph
// ---------------------------------------------------------------------------

// TestDisplayConditionConfig_SelfCycle is the degenerate case.
func TestDisplayConditionConfig_SelfCycle(t *testing.T) {
	mustRefuse(t, validate(t, []string{"dep=x"}, map[string]gnode{
		"dep": {Type: "text"},
	}), "cannot depend on itself")
}

// TestDisplayConditionConfig_TwoCycle: B already depends on A, so A
// depending on B closes the ring.
func TestDisplayConditionConfig_TwoCycle(t *testing.T) {
	msg := metadata.ValidateDisplayConditionConfigForTest(
		"a", "text", "asset", nil, nil,
		[]string{"b=x"},
		map[string]gnode{
			"a": {Type: "text"},
			"b": {Type: "text", Condition: []string{"a=x"}},
		},
	)
	mustRefuse(t, msg, "would create a loop")
}

// TestDisplayConditionConfig_ThreeCycleClosesOnTheThirdWrite is the case
// a validator that only looks at the IMMEDIATE edge gets wrong.
//
// `A -> B` and `B -> C` are both legitimate and must be ACCEPTED. Only
// `C -> A` closes the ring, and nothing about C's own direct controllers
// says so: the answer is two hops away.
func TestDisplayConditionConfig_ThreeCycleClosesOnTheThirdWrite(t *testing.T) {
	// Write 1: A -> B, on an empty graph. Accepted.
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"a", "text", "asset", nil, nil,
		[]string{"b=x"},
		map[string]gnode{"a": {Type: "text"}, "b": {Type: "text"}, "c": {Type: "text"}},
	))
	// Write 2: B -> C, with A -> B already stored. Accepted.
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"b", "text", "asset", nil, nil,
		[]string{"c=x"},
		map[string]gnode{
			"a": {Type: "text", Condition: []string{"b=x"}},
			"b": {Type: "text"},
			"c": {Type: "text"},
		},
	))
	// Write 3: C -> A. THIS is the one that closes it, and only a walk of
	// the whole graph can see it.
	msg := metadata.ValidateDisplayConditionConfigForTest(
		"c", "text", "asset", nil, nil,
		[]string{"a=x"},
		map[string]gnode{
			"a": {Type: "text", Condition: []string{"b=x"}},
			"b": {Type: "text", Condition: []string{"c=x"}},
			"c": {Type: "text"},
		},
	)
	mustRefuse(t, msg, "would create a loop")
	// The refusal names the actual ring, so an operator can see which
	// three fields are involved rather than being told one exists.
	for _, code := range []string{"a", "b", "c"} {
		if !strings.Contains(msg, code) {
			t.Errorf("refusal %q does not name %q; the loop has to be readable", msg, code)
		}
	}
}

// TestDisplayConditionConfig_DiamondIsNotACycle guards the other
// direction: a graph where two paths reach one node is perfectly legal,
// and a walk that marked nodes visited too eagerly, or that mistook a
// re-visit for a ring, would refuse it.
func TestDisplayConditionConfig_DiamondIsNotACycle(t *testing.T) {
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "asset", nil, nil,
		[]string{"b=x", "c=x"},
		map[string]gnode{
			"a": {Type: "text"},
			"b": {Type: "text", Condition: []string{"a=x"}},
			"c": {Type: "text", Condition: []string{"a=x"}},
			"d": {Type: "text"},
		},
	))
}

// ---------------------------------------------------------------------------
// N-WAY applicability
// ---------------------------------------------------------------------------

// TestDisplayConditionConfig_NWayApplicability is the counterexample a
// PAIRWISE implementation fails.
//
// D applies to {1,2}. A applies to {1}. B applies to {2}. D with A alone
// is fine, D with B alone is fine, and D with BOTH must be REFUSED,
// because there is no asset type on which all three appear together and
// the condition could therefore never be true.
//
// A pairwise check passes the first two AND the third, because each
// individual pair intersects.
func TestDisplayConditionConfig_NWayApplicability(t *testing.T) {
	graph := map[string]gnode{
		"a": {Type: "text", AppliesTo: []int64{1}},
		"b": {Type: "text", AppliesTo: []int64{2}},
	}
	dep := []int64{1, 2}

	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "asset", dep, nil, []string{"a=x"}, graph))
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "asset", dep, nil, []string{"b=x"}, graph))

	msg := metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "asset", dep, nil, []string{"a=x", "b=x"}, graph)
	mustRefuse(t, msg, "never appear on the same asset type")
}

// TestDisplayConditionConfig_EmptyAppliesToIsUniversal pins the trap in
// the set algebra: `applies_to = '{}'` means "every asset type", so
// mapping it to the empty SET would make the intersection empty
// immediately and refuse every condition touching an unrestricted field.
func TestDisplayConditionConfig_EmptyAppliesToIsUniversal(t *testing.T) {
	// Dependent restricted, controller global.
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "asset", []int64{7}, nil, []string{"a=x"},
		map[string]gnode{"a": {Type: "text"}}))
	// Dependent global, controller restricted.
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "asset", nil, nil, []string{"a=x"},
		map[string]gnode{"a": {Type: "text", AppliesTo: []int64{7}}}))
	// Both restricted and disjoint: this is the real refusal.
	mustRefuse(t, metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "asset", []int64{7}, nil, []string{"a=x"},
		map[string]gnode{"a": {Type: "text", AppliesTo: []int64{8}}}),
		"never appear on the same asset type")
}

// TestDisplayConditionConfig_CollectionsSkipApplicability: collection
// fields are not applies_to-scoped, so the intersection must not run
// there at all. Running it would refuse a perfectly ordinary collection
// condition on the strength of a column collections do not use.
func TestDisplayConditionConfig_CollectionsSkipApplicability(t *testing.T) {
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"d", "text", "collection", []int64{7}, nil, []string{"a=x"},
		map[string]gnode{"a": {Type: "text", Subject: "collection", AppliesTo: []int64{8}}}))
}

// ---------------------------------------------------------------------------
// The contradiction MINIMUM SET, and its boundary
// ---------------------------------------------------------------------------

// TestDisplayConditionConfig_ContradictionMinimumSet covers both halves,
// and the second half is the important one: this is NOT a general
// predicate solver and must not become one.
func TestDisplayConditionConfig_ContradictionMinimumSet(t *testing.T) {
	t.Run("two distinct = literals on one single-valued controller", func(t *testing.T) {
		for _, ftype := range []string{"text", "longtext", "select", "tree"} {
			msg := validate(t, []string{"ctrl=a", "ctrl=b"}, map[string]gnode{
				"ctrl": {Type: ftype},
			})
			mustRefuse(t, msg, "two different values at once")
		}
	})

	t.Run("DUPLICATE IDENTICAL terms are allowed", func(t *testing.T) {
		mustAccept(t, validate(t, []string{"ctrl=a", "ctrl=a"}, map[string]gnode{
			"ctrl": {Type: "text"},
		}))
	})

	t.Run("distinct = MEMBERSHIP terms on one multi_select are allowed", func(t *testing.T) {
		mustAccept(t, validate(t, []string{"ctrl=a", "ctrl=b"}, map[string]gnode{
			"ctrl": {Type: "multi_select"},
		}))
	})

	t.Run("a cross-operator contradiction is NOT refused: no solver", func(t *testing.T) {
		// `ctrl=a` and `ctrl~zzz` cannot both be true, and that is
		// deliberately accepted. Beyond the minimum set the answer is
		// "the field will not appear", which an operator can see and fix,
		// and a solver would start refusing configurations that are
		// merely unusual.
		mustAccept(t, validate(t, []string{"ctrl=a", "ctrl~zzz"}, map[string]gnode{
			"ctrl": {Type: "text"},
		}))
	})

	t.Run("two = literals on DIFFERENT controllers are ordinary", func(t *testing.T) {
		mustAccept(t, validate(t, []string{"one=a", "two=b"}, map[string]gnode{
			"one": {Type: "text"},
			"two": {Type: "text"},
		}))
	})
}

// TestDisplayConditionConfig_EmptyProposalIsAccepted: a clear removes
// edges and can never create a cycle or empty an intersection, so it is
// accepted unconditionally. That is what makes a setting always
// removable, even from a field it could no longer be applied to.
func TestDisplayConditionConfig_EmptyProposalIsAccepted(t *testing.T) {
	mustAccept(t, metadata.ValidateDisplayConditionConfigForTest(
		"title", "text", "asset", nil, strptr("title"), nil, map[string]gnode{}))
}
