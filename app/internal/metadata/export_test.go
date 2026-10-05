// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

// Test-only windows onto the display-condition internals (#1173, #1119).
//
// The metadata suite lives in the EXTERNAL `metadata_test` package, which
// is what keeps it honest: it drives the handlers through a router and
// sees only what a caller sees. Two pieces of ADR 0099 are pure functions
// with no HTTP surface of their own, and they are exactly the pieces the
// SHARED CROSS-LANGUAGE CORPUS has to compare against their TypeScript
// twins, one case at a time.
//
// This file is compiled only for tests, so nothing here widens the
// package's real API. The alternative — exporting the operator matrix and
// the term comparison for good — would put two internal decisions in the
// package's public surface purely so a test could reach them.
//
// ⛔ NOTHING NON-TEST MAY CALL THESE. If production code needs one of
// them, move the function itself rather than promoting its test alias.

import "github.com/mscrnt/artist-alley/app/internal/search/facet"

// DisplayConditionOpAllowedForTest exposes the closed operator/type table
// so the shared corpus can assert it entry by entry, including the
// entries that are absent on purpose (`boolean`, `>=`, `<=`).
func DisplayConditionOpAllowedForTest(fieldType string, op facet.FieldOp) bool {
	return displayConditionOpAllowed(fieldType, op)
}

// DisplayTermMatchesForTest exposes the single-term comparison, which is
// where the trimming and case-sensitivity asymmetry lives: the condition
// literal is trimmed by the parser and the stored value never is.
func DisplayTermMatchesForTest(t DisplayConditionTerm, c ControllerState) bool {
	return displayTermMatches(t, c)
}

// ValidateDisplayConditionConfigForTest exposes the closed refusal list
// so the cycle walk and the N-way applicability intersection can be
// tested as PURE GRAPH ALGEBRA, on graphs it would take a dozen HTTP
// round trips to build. The end-to-end suite drives the same rules
// through PATCH; this is what makes the boundary cases affordable.
func ValidateDisplayConditionConfigForTest(
	dependentCode, dependentType, dependentSubject string,
	dependentAppliesTo []int64,
	dependentMirrors *string,
	proposed []string,
	graph map[string]ConditionGraphNodeForTest,
) string {
	nodes := make(map[string]conditionGraphNode, len(graph)+1)
	for code, n := range graph {
		nodes[code] = n.toNode(code)
	}
	dep := conditionGraphNode{
		Code:          dependentCode,
		Type:          dependentType,
		Status:        "active",
		subject:       dependentSubject,
		AppliesTo:     dependentAppliesTo,
		MirrorsColumn: dependentMirrors,
	}
	// The dependent is part of its own graph, exactly as it is at
	// runtime: the cycle walk starts from it and a self-reference has to
	// resolve to something.
	if _, ok := nodes[dependentCode]; !ok {
		nodes[dependentCode] = dep
	} else {
		existing := nodes[dependentCode]
		dep.Condition = existing.Condition
		nodes[dependentCode] = dep
	}
	return validateDisplayConditionConfig(dep, proposed, nodes)
}

// ConditionGraphNodeForTest is the external suite's way of describing one
// node without the unexported subject field.
type ConditionGraphNodeForTest struct {
	Type          string
	Status        string
	Subject       string
	AppliesTo     []int64
	MirrorsColumn *string
	Condition     []string
}

func (n ConditionGraphNodeForTest) toNode(code string) conditionGraphNode {
	status := n.Status
	if status == "" {
		status = "active"
	}
	subject := n.Subject
	if subject == "" {
		subject = string(SubjectAsset)
	}
	return conditionGraphNode{
		Code:          code,
		Type:          n.Type,
		Status:        status,
		subject:       subject,
		AppliesTo:     n.AppliesTo,
		MirrorsColumn: n.MirrorsColumn,
		Condition:     n.Condition,
	}
}
