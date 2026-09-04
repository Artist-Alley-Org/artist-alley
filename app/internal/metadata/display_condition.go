// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

// `display_condition` — when a field should be OFFERED at all (#1173,
// #1119, ADR 0099).
//
// # What this file is, and the one sentence that keeps it honest
//
// A CONDITION IS A FORM HINT AND NEVER AUTHORIZATION. Everything here
// decides whether a CONTROL is drawn. Nothing here decides whether a
// value may be read or written: a hidden field keeps its values, keeps
// its read_capability and write_capability, and is still writable
// through PUT /assets/{id}/fields/{field_id}. If a rule in this file ever
// looks like it is protecting something, that is the bug.
//
// The file holds four things, in order:
//
//  1. the TERM GRAMMAR, which is not a new grammar — it is
//     facet.SplitFieldTerm, reused verbatim, because a second parser is a
//     second grammar free to disagree with the first;
//  2. the OPERATOR/TYPE MATRIX, which is a closed table;
//  3. EVALUATION, which the server needs for tests and which the browser
//     reimplements against a shared fixture corpus;
//  4. CONFIGURATION VALIDATION, the closed refusal list, including the
//     whole-graph cycle walk and the N-way applicability intersection.
//
// # ⛔ THE ONE THING MOST LIKELY TO BE IMPLEMENTED WRONGLY
//
// Evaluation is CONJUNCTIVE with a WHOLE-CONDITION FAIL-OPEN. If any term
// cannot be evaluated, the condition has NO VERDICT and the dependent is
// SHOWN — the remaining terms are not consulted at all.
//
// The tempting wrong version treats an unevaluable term as `true` inside
// the AND. It is not the same rule, and it differs on exactly the cases
// that matter: with term A FALSE and term B unevaluable, "unknown counts
// as true" evaluates `false AND true` and HIDES the field. See
// [EvaluateDisplayCondition] where the two passes are separated for this
// reason.
//
// The mirror trap is the other half: a READABLE controller with genuinely
// no value is a REAL FALSE and still hides. Absence is an answer.
// Treating every absent value as unknown produces an evaluator that never
// hides anything and reads green against every test that only checks the
// true arm.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

// ---------------------------------------------------------------------------
// 1. The term grammar
// ---------------------------------------------------------------------------

// DisplayConditionTerm is one parsed `<code><op><value>` entry.
//
// `Code` is lowercased and trimmed and `Value` is trimmed, because that
// is what [facet.SplitFieldTerm] does and this type is its output rather
// than a re-derivation of it.
type DisplayConditionTerm struct {
	// Raw is the entry exactly as it was stored, kept so a refusal can
	// quote what the operator actually wrote rather than a normalised
	// version of it.
	Raw   string
	Code  string
	Op    facet.FieldOp
	Value string
}

// ParseDisplayConditionTerm splits one stored entry.
//
// ⛔ IT DELEGATES TO [facet.SplitFieldTerm] AND MUST CONTINUE TO. That
// function is the authority on five properties that are not guessable
// from looking at a term, and all five are load-bearing here:
//
//  1. it splits on the FIRST character from `=~<>`;
//  2. it matches operators LONGEST FIRST, so `>=` is never read as `>`
//     followed by a value beginning `=`;
//  3. later operator characters stay IN the value, so `role=a=b` has the
//     value `a=b`;
//  4. the CODE is lowercased and trimmed;
//  5. the parsed VALUE is trimmed.
//
// Nothing trims or case-folds the STORED value, on either side, ever. The
// asymmetry between (5) and that is deliberate and is the thing an
// operator will hit: a stored `" Commission "` does NOT match a condition
// `work_type= Commission `, because the literal parses to `Commission`
// and the stored value is six characters longer. Trimming the stored
// value would make this evaluator disagree with `=` everywhere else in
// the product, including the search predicate the same grammar drives.
//
// A bare code with no operator, an empty parsed value, and a code
// carrying anything outside `[a-z0-9_-]` all fail here rather than being
// guessed at.
func ParseDisplayConditionTerm(raw string) (DisplayConditionTerm, bool) {
	code, op, value, ok := facet.SplitFieldTerm(raw)
	if !ok {
		return DisplayConditionTerm{Raw: raw}, false
	}
	return DisplayConditionTerm{Raw: raw, Code: code, Op: op, Value: value}, true
}

// ParseDisplayCondition parses a whole stored array.
//
// Returns the terms and the RAW ENTRY of the first one that did not
// parse. A caller that gets a non-empty `bad` has an UNEVALUABLE
// condition, not a partially evaluable one.
func ParseDisplayCondition(raw []string) (terms []DisplayConditionTerm, bad string, ok bool) {
	terms = make([]DisplayConditionTerm, 0, len(raw))
	for _, entry := range raw {
		t, good := ParseDisplayConditionTerm(entry)
		if !good {
			return nil, entry, false
		}
		terms = append(terms, t)
	}
	return terms, "", true
}

// DecodeDisplayCondition reads the stored jsonb column.
//
// A NULL column (nil or empty bytes) is N=0 and returns nil with no
// error, which is the canonical unset. Migration 00065's CHECK means the
// only other stored shape is a non-empty array of non-empty strings, so a
// decode failure here is a corrupted row rather than an ordinary state,
// and the caller treats it as unevaluable rather than as absent.
func DecodeDisplayCondition(b []byte) ([]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("metadata: display_condition decode: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 2. The operator / type matrix
// ---------------------------------------------------------------------------

// displayConditionOps is the CLOSED table of which operator a controller
// of each type accepts (ADR 0099 §3).
//
// `>=` and `<=` appear nowhere. They are the two ends of an ordered
// range, which is a FILTERING question; a form composition rule that
// asked "is this date after that one" would be a predicate language, and
// this is deliberately not one.
//
// ⛔ `boolean` IS ABSENT ON PURPOSE AND IS THE ENTRY MOST LIKELY TO BE
// ADDED BY MISTAKE. Sprint 20a gave `boolean` a three-state CONTROL,
// which changed the control and not the REPRESENTATION: a boolean is
// still 1 or 0 in `value_num` (ADR 0012's third 2026-07-31 amendment) and
// the server still handles "number" and "boolean" together on the query
// path. Admitting it here would require the SEARCH ENGINE to learn a
// boolean predicate. Do not widen this table by widening search
// semantics.
//
// `rich_text` is absent for `regexp_filter`'s reason: what is stored is
// server-sanitised HTML, so a condition would be matched against markup
// rather than against anything the operator can see.
var displayConditionOps = map[string][]facet.FieldOp{
	"text":     {facet.FieldOpEq, facet.FieldOpContains},
	"longtext": {facet.FieldOpEq, facet.FieldOpContains},
	// The stored SLUG, not the label. ADR 0012 keeps only the slug on the
	// record, so that is the only thing there is to compare.
	"select": {facet.FieldOpEq},
	"tree":   {facet.FieldOpEq},
	// `=` here means MEMBERSHIP, and it is the one place the operator
	// means two things. See [displayTermMatches].
	"multi_select": {facet.FieldOpEq},
}

// displayConditionOpAllowed reports whether a controller of `fieldType`
// accepts `op` in a display condition.
func displayConditionOpAllowed(fieldType string, op facet.FieldOp) bool {
	for _, allowed := range displayConditionOps[fieldType] {
		if allowed == op {
			return true
		}
	}
	return false
}

// displayConditionSingleValued reports whether a controller of this type
// can hold at most ONE value.
//
// Used by the contradiction check only. `multi_select` is the one
// admissible type that is not single-valued, which is why two distinct
// `=` terms on one are legitimate there and a contradiction anywhere
// else.
func displayConditionSingleValued(fieldType string) bool {
	switch fieldType {
	case "text", "longtext", "select", "tree":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 3. Evaluation
// ---------------------------------------------------------------------------

// ControllerState is one controller field as the evaluator sees it.
//
// The three states it can express are the three the rules distinguish,
// and conflating any two of them is a bug the fixture corpus is written
// to catch:
//
//	Missing (no entry at all)        -> UNEVALUABLE, fails open
//	Present with Readable == false   -> UNEVALUABLE, fails open
//	Present, readable, no value      -> REAL FALSE, hides
//
// ⚠️ `Readable` is a SERVER-DERIVED fact about this caller and this
// subject, never an inference from whether a value arrived. Inferring it
// from value presence collapses the second and third states into one,
// which is exactly the defect ADR 0099 §5 exists to close.
type ControllerState struct {
	Type     string
	Readable bool
	// Text is the stored value_text, verbatim and untrimmed.
	Text string
	// HasText distinguishes a stored empty string from no row at all.
	// Both are FALSE for every operator, so this exists for clarity at
	// the call sites rather than for the verdict.
	HasText bool
	// Options is the stored value_options for a multi_select, verbatim.
	Options []string
}

// DisplayConditionResolver answers "what is the state of the controller
// with this code". Returning ok == false means the definition is MISSING
// or UNRESOLVABLE, which fails the whole condition open.
type DisplayConditionResolver func(code string) (ControllerState, bool)

// displayTermMatches evaluates ONE term against a readable controller.
//
// Reached only for a controller that resolved AND is readable AND whose
// type accepts the operator; the unevaluable cases are decided by the
// caller, before any of this runs.
//
// ⛔ NOTHING HERE TRIMS OR CASE-FOLDS THE STORED VALUE. `=` is exact and
// case-sensitive. `~` lowercases BOTH sides for the comparison and stores
// neither, which is a matching rule rather than a normalisation of what
// is on the record.
func displayTermMatches(t DisplayConditionTerm, c ControllerState) bool {
	switch t.Op {
	case facet.FieldOpEq:
		if c.Type == "multi_select" {
			// MEMBERSHIP. Equality against the whole set would make a
			// multi-valued field usable as a controller only while it
			// held exactly one value, which is not a rule anybody would
			// write down.
			for _, opt := range c.Options {
				if opt == t.Value {
					return true
				}
			}
			return false
		}
		return c.Text == t.Value
	case facet.FieldOpContains:
		return strings.Contains(strings.ToLower(c.Text), strings.ToLower(t.Value))
	}
	// An operator outside the matrix is unreachable through configuration
	// and is refused rather than guessed at. Answering `false` here would
	// HIDE a field because of a rule nobody could see.
	return false
}

// EvaluateDisplayCondition decides whether a dependent field is SHOWN.
//
// Returns true for SHOWN and false for HIDDEN, which is the direction
// that makes the fail-open default readable at the call sites: every
// early return in this function returns `true`.
//
//	N == 0                     -> shown, and no term is examined
//	any term unparseable       -> shown  (unevaluable)
//	any controller missing     -> shown  (unevaluable)
//	any controller unreadable  -> shown  (unevaluable)
//	any operator/type mismatch -> shown  (unevaluable)
//	otherwise                  -> AND of the terms
//
// ⛔ THE TWO PASSES ARE SEPARATE AND MUST STAY SEPARATE. The first pass
// asks only "can every term be evaluated"; the second computes the
// conjunction. Folding them into one loop that ANDs as it goes is the
// "unknown counts as true" bug, which hides a field whenever one term is
// false and another is unknown.
func EvaluateDisplayCondition(raw []string, resolve DisplayConditionResolver) bool {
	if len(raw) == 0 {
		return true
	}
	terms, _, ok := ParseDisplayCondition(raw)
	if !ok {
		return true
	}

	// PASS 1 — evaluability. Nothing is decided here.
	states := make([]ControllerState, len(terms))
	for i, t := range terms {
		c, found := resolve(t.Code)
		if !found || !c.Readable {
			return true
		}
		if !displayConditionOpAllowed(c.Type, t.Op) {
			return true
		}
		states[i] = c
	}

	// PASS 2 — the conjunction. Every term is known evaluable, so a
	// `false` here is a REAL false, including the one produced by a
	// readable controller holding nothing.
	for i, t := range terms {
		if !displayTermMatches(t, states[i]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 4. Configuration validation
// ---------------------------------------------------------------------------

// conditionGraphNode is one definition as the validator sees it. Built
// from ListFieldDefinitionsForConditionGraph, which reads the WHOLE
// subject kind inside the advisory lock.
type conditionGraphNode struct {
	Code   string
	Type   string
	Status string
	// subject is the row's subject_kind. Unexported and read through
	// [conditionGraphNode.subjectKind] so an empty value defaults to
	// `asset` rather than producing a node that matches nothing.
	subject       string
	AppliesTo     []int64
	MirrorsColumn *string
	// Condition is the node's own stored terms, already decoded. An entry
	// that does not parse is kept as-is; the walk ignores it, because an
	// unparseable term names no node and therefore forms no edge.
	Condition []string
}

// validateDisplayConditionConfig is the CLOSED REFUSAL LIST of ADR 0099
// §6, applied to a proposed condition on one dependent.
//
// Returns an operator-facing sentence, or "" when the configuration is
// acceptable. Every refusal names what is wrong and, where there is one,
// what to do instead: an operator staring at a 400 that says "invalid"
// has to guess which of ten rules they broke.
//
// `graph` is every definition of the dependent's subject kind, keyed by
// code, INCLUDING archived ones — an archived dependent keeps its stored
// configuration, so its edges still exist and a cycle through it is still
// a cycle. The archived rule is about the CONTROLLER side and is applied
// per term below.
//
// ⚠️ This function is pure. It does not read the database and it does not
// write. Its atomicity is the CALLER's job (ADR 0099 §8): the caller
// takes a transaction-scoped advisory lock on the subject kind, reads the
// graph inside it, calls this, writes, and commits. Calling this against
// a graph read outside that lock makes the cycle check theatre, because
// two operators writing `A -> B` and `B -> A` each validate against a
// graph in which the other's edge is not yet visible.
func validateDisplayConditionConfig(
	dependent conditionGraphNode,
	proposed []string,
	graph map[string]conditionGraphNode,
) string {
	if len(proposed) == 0 {
		return ""
	}

	// The DEPENDENT side of the mirrored exclusion. `title` and
	// `description` are views onto columns of `assets` that carry a
	// second human write plane, and every surface that hosts them already
	// draws a first-class control. Conditioning one would mean the
	// condition governed the field plane and not the column plane, and
	// only one of the two would obey it.
	if dependent.MirrorsColumn != nil && *dependent.MirrorsColumn != "" {
		return fmt.Sprintf(
			"field %q mirrors the %q column of the asset and cannot carry a display condition: that column has its own control and its own write plane, and only one of the two would obey it",
			dependent.Code, *dependent.MirrorsColumn)
	}

	terms, bad, ok := ParseDisplayCondition(proposed)
	if !ok {
		return fmt.Sprintf(
			"display condition term %q is not a valid <code><operator><value> term; the operators are = and ~", bad)
	}

	// applies_to intersection, N-WAY rather than pairwise (ADR 0099 §6).
	// Starts from the dependent and narrows once per controller, so the
	// refusal is on the TOTAL intersection.
	//
	// ⛔ A PAIRWISE IMPLEMENTATION PASSES THE OBVIOUS CASES AND FAILS THE
	// REAL ONE: dependent {t1,t2} with controllers {t1} and {t2} is
	// acceptable with either controller alone and must be REFUSED with
	// both, because there is no asset type on which all three appear.
	//
	// nil means "universal so far" — an empty applies_to is global, and
	// treating it as the empty SET would refuse every condition involving
	// an unrestricted field.
	var narrowed []int64
	if dependent.SubjectKindIsAsset() {
		narrowed = appliesToSet(dependent.AppliesTo)
	}

	// Per-controller `=` literals, for the contradiction minimum set.
	eqLiterals := map[string]map[string]struct{}{}

	for _, t := range terms {
		ctrl, found := graph[t.Code]
		if !found {
			return fmt.Sprintf(
				"display condition names field %q, which this server does not have", t.Code)
		}
		if t.Code == dependent.Code {
			return fmt.Sprintf(
				"field %q cannot depend on itself", dependent.Code)
		}
		if ctrl.MirrorsColumn != nil && *ctrl.MirrorsColumn != "" {
			return fmt.Sprintf(
				"display condition names field %q, which mirrors the %q column of the asset; a mirrored field has a second write plane and cannot be used as a controller",
				t.Code, *ctrl.MirrorsColumn)
		}
		// The subject-kind gate. A collection field's condition cannot
		// name an asset field: the two live on different records and
		// there is no subject on which both have a value.
		if !ctrl.SameSubjectKindAs(dependent) {
			return fmt.Sprintf(
				"display condition names field %q, which describes a %s; a %s field's condition can only name other %s fields",
				t.Code, ctrl.subjectKind(), dependent.subjectKind(), dependent.subjectKind())
		}
		// ALREADY ARCHIVED is refused; `deprecated` is ACCEPTED.
		//
		// Deprecated is accepted because edit surfaces deliberately render
		// active and deprecated definitions together (#528): a deprecated
		// field is one an operator stopped wanting NEW values in, and
		// records that already hold one must keep showing it. On /create,
		// which is active-only, a deprecated controller is simply
		// unresolvable and the dependent fails open, which is correct.
		//
		// Archived is refused because an archived definition never appears
		// on ANY composition surface, so the condition would be
		// permanently inert from the moment it was written: stored,
		// valid-looking, and guaranteed to fail open forever. That is a
		// setting that silently does nothing, which is the failure mode
		// 00045's card/capability rule was also written to prevent.
		if ctrl.Status == "archived" {
			return fmt.Sprintf(
				"display condition names field %q, which is archived; an archived field never appears on a composition surface, so the condition could never be evaluated",
				t.Code)
		}
		if !displayConditionOpAllowed(ctrl.Type, t.Op) {
			return displayConditionOpRefusal(t, ctrl.Type)
		}

		if dependent.SubjectKindIsAsset() {
			narrowed = intersectAppliesTo(narrowed, appliesToSet(ctrl.AppliesTo))
			if narrowed != nil && len(narrowed) == 0 {
				return fmt.Sprintf(
					"display condition cannot be satisfied: field %q and the fields it names never appear on the same asset type, so the condition would have nothing to evaluate against",
					dependent.Code)
			}
		}

		// The contradiction MINIMUM SET, and it is deliberately minimal.
		// Two `=` terms with different literals on one controller that can
		// hold at most one value can never both be true, so the condition
		// is inert and the operator has almost certainly mistyped. That is
		// the whole check.
		//
		// ⛔ NOT A GENERAL PREDICATE SOLVER, and one must not grow here. A
		// solver would refuse configurations that are merely unusual, and
		// it would become a second place where the meaning of a condition
		// is decided. Duplicate IDENTICAL terms are fine, and so are
		// distinct `=` MEMBERSHIP terms on one multi_select, which can
		// genuinely hold both.
		if t.Op == facet.FieldOpEq && displayConditionSingleValued(ctrl.Type) {
			seen, ok := eqLiterals[t.Code]
			if !ok {
				seen = map[string]struct{}{}
				eqLiterals[t.Code] = seen
			}
			seen[t.Value] = struct{}{}
			if len(seen) > 1 {
				return fmt.Sprintf(
					"display condition requires field %q to equal two different values at once, which can never be true; %q holds one value at a time",
					t.Code, t.Code)
			}
		}
	}

	// The CYCLE WALK, over the whole subject-kind graph.
	//
	// ⛔ WALKING ONLY THE IMMEDIATE EDGE IS NOT ENOUGH: `A -> B`, then
	// `B -> C`, then `C -> A` closes the cycle on the THIRD write, and the
	// first two are legitimate configurations that must be accepted. A
	// validator that only compared the new edge against its direct
	// controllers accepts all three.
	//
	// Depth-first from the DEPENDENT with the proposed edges substituted
	// in. Any new cycle must pass through the dependent, because the graph
	// was acyclic before this write by induction, so reaching the
	// dependent again is the whole test.
	if cyc := findConditionCycle(dependent.Code, proposed, graph); len(cyc) > 0 {
		return fmt.Sprintf(
			"display condition would create a loop: %s. A field cannot depend, directly or through other fields, on itself",
			strings.Join(cyc, " -> "))
	}
	return ""
}

// displayConditionOpRefusal writes the sentence for an operator/type
// mismatch, naming what the type DOES accept so the operator is not left
// guessing at a closed table they cannot see.
func displayConditionOpRefusal(t DisplayConditionTerm, fieldType string) string {
	allowed := displayConditionOps[fieldType]
	if len(allowed) == 0 {
		return fmt.Sprintf(
			"display condition uses field %q, whose type %q cannot be used as a condition; conditions read text, longtext, select, tree and multi_select fields",
			t.Code, fieldType)
	}
	names := make([]string, 0, len(allowed))
	for _, op := range allowed {
		names = append(names, string(op))
	}
	return fmt.Sprintf(
		"display condition uses %q on field %q, whose type %q accepts only %s",
		string(t.Op), t.Code, fieldType, strings.Join(names, " and "))
}

// findConditionCycle returns the cycle path, or nil.
//
// `proposed` REPLACES the dependent's stored edges for the walk, because
// the question is about the graph this write would LAND ON and not the
// one it starts from. Terms that do not parse, and terms naming a code
// the graph does not hold, form no edge: they are refused separately
// above, and treating them as edges here would report a cycle through a
// node that does not exist.
func findConditionCycle(start string, proposed []string, graph map[string]conditionGraphNode) []string {
	edges := func(code string) []string {
		var raw []string
		if code == start {
			raw = proposed
		} else {
			n, ok := graph[code]
			if !ok {
				return nil
			}
			raw = n.Condition
		}
		out := make([]string, 0, len(raw))
		for _, entry := range raw {
			t, ok := ParseDisplayConditionTerm(entry)
			if !ok {
				continue
			}
			if _, ok := graph[t.Code]; !ok {
				continue
			}
			out = append(out, t.Code)
		}
		return out
	}

	// Iterative DFS carrying its own path, so the refusal can quote the
	// actual loop rather than merely asserting that one exists.
	var path []string
	onPath := map[string]bool{}
	visited := map[string]bool{}

	var walk func(code string) []string
	walk = func(code string) []string {
		if onPath[code] {
			// Found it. Cut the path down to the cycle itself and close
			// the ring so it reads as one.
			for i, c := range path {
				if c == code {
					return append(append([]string{}, path[i:]...), code)
				}
			}
			return []string{code, code}
		}
		if visited[code] {
			return nil
		}
		visited[code] = true
		onPath[code] = true
		path = append(path, code)
		for _, next := range edges(code) {
			if cyc := walk(next); len(cyc) > 0 {
				return cyc
			}
		}
		path = path[:len(path)-1]
		onPath[code] = false
		return nil
	}
	return walk(start)
}

// ---------------------------------------------------------------------------
// applies_to set algebra
// ---------------------------------------------------------------------------

// appliesToSet normalises a stored applies_to into the validator's
// representation.
//
// ⚠️ nil means UNIVERSAL, not empty. `applies_to = '{}'` is "this field
// applies to every asset type" (the column's default), so mapping it to
// the empty SET would make the intersection empty immediately and refuse
// every condition that touched an unrestricted field.
func appliesToSet(v []int64) []int64 {
	if len(v) == 0 {
		return nil
	}
	out := append([]int64{}, v...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// intersectAppliesTo narrows `a` by `b`, with nil meaning universal on
// either side.
func intersectAppliesTo(a, b []int64) []int64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	in := map[int64]struct{}{}
	for _, v := range b {
		in[v] = struct{}{}
	}
	out := make([]int64, 0, len(a))
	for _, v := range a {
		if _, ok := in[v]; ok {
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// conditionGraphNode helpers
// ---------------------------------------------------------------------------

// subjectKindOf is carried on the node so the validator's messages can
// name it. The graph is read one subject kind at a time, so in practice
// every node in one map shares it; the accessor exists so a future caller
// that mixes them cannot do so silently.
func (n conditionGraphNode) subjectKind() string {
	if n.subject == "" {
		return string(SubjectAsset)
	}
	return n.subject
}

// SubjectKindIsAsset reports whether applies_to means anything for this
// node. Collections are not applies_to-scoped, so the N-way intersection
// is skipped entirely on that side rather than run against a column that
// is always empty.
func (n conditionGraphNode) SubjectKindIsAsset() bool {
	return n.subjectKind() == string(SubjectAsset)
}

// SameSubjectKindAs is the discriminator gate for a term.
func (n conditionGraphNode) SameSubjectKindAs(other conditionGraphNode) bool {
	return n.subjectKind() == other.subjectKind()
}
