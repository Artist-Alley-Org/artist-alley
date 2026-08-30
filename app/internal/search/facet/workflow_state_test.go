// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18c — the `workflow_state` GRAMMAR, proven without a
// database.
//
// What is here is every claim that is a property of the value and of the
// rendered SQL. The claims that need ROWS — which assets a filter
// returns, that a non-asset domain actually matches a misfiled asset,
// that a deleted state's identity returns zero and not `none` — are in
// search/workflow_state_filter_test.go, because a claim about a
// POPULATION cannot be made against a string.
//
// ⛔ The portable fail-before proof is deliberately NOT in this file.
// Every case below names [FacetWorkflowState], a symbol that does not
// exist on `dev`, so none of them could be compiled against the old
// behaviour even in principle. See workflow_state_failbefore_test.go,
// which names no new symbol at all and therefore runs verbatim on both.
package facet

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// TestWorkflowState_Registered — the dimension parses, under its name
// and under nothing else.
func TestWorkflowState_Registered(t *testing.T) {
	got, ok := ParseFacetType("workflow_state")
	if !ok || got != FacetWorkflowState {
		t.Errorf("ParseFacetType(\"workflow_state\") = (%q, %v), want (workflow_state, true)", got, ok)
	}
	// ⚠️ No unannounced aliases. `state` is deliberately NOT one: a POST
	// has a state_id too and it means publication, so a short name would
	// invite a filter that cannot do what its name suggests.
	for _, alias := range []string{"state", "workflow", "workflowstate", "workflow_states"} {
		if _, ok := ParseFacetType(alias); ok {
			t.Errorf("ParseFacetType accepted %q — an unannounced alias", alias)
		}
	}
}

// TestWorkflowState_ValueValidity is the value-validity table, covered
// directly.
//
// ⛔ THE THREE OUTCOMES ARE DIFFERENT AND THE DIFFERENCE IS THE
// CONTRACT. Malformed is a request error (pure, knowable without a row).
// Well-formed is accepted whether or not the row exists, because
// existence needs a database and #897 lets an operator delete a state.
// A test that collapsed "unknown" into "invalid" would look correct and
// assert the opposite of the decision.
func TestWorkflowState_ValueValidity(t *testing.T) {
	for _, c := range []struct {
		name  string
		in    string
		want  string
		valid bool
		why   string
	}{
		{
			"the reserved literal", "none", "none", true,
			"selects assets with no state at all",
		},
		{
			"an ordinary identity", "asset:1/published", "asset:1/published", true,
			"the natural key of workflow_states, UNIQUE (domain, code)",
		},
		{
			"a code containing a further slash", "asset:1/stage/final", "asset:1/stage/final", true,
			"the split is at the FIRST slash, so the whole code survives. A code is\n" +
				"  operator-defined free text under #897 and may contain one",
		},
		{
			"an unknown but well-formed identity", "asset:9/never_defined", "asset:9/never_defined", true,
			"⛔ ACCEPTED, and matches zero. CanonicalValue is PURE and cannot check\n" +
				"  existence; rejecting would make a saved query unparseable the moment an\n" +
				"  operator deleted a state, and matching zero is correct — exactly as\n" +
				"  extension:zzz matches zero",
		},
		{
			"a non-asset domain", "post/published", "post/published", true,
			"a real row. Asset create does not validate the domain, so an asset can\n" +
				"  carry it, and accepting the identity is what surfaces that",
		},
		{
			"no slash", "published", "", false,
			"a concrete identity is <domain>/<code>; without a separator there is no\n" +
				"  domain, and guessing one would be a filter that looks applied and is not",
		},
		{
			"an empty domain", "/published", "", false,
			"the domain half is empty",
		},
		{
			"an empty code", "asset:1/", "", false,
			"the code half is empty",
		},
		{
			"a bare slash", "/", "", false,
			"both halves empty",
		},
		{
			// ⚠️ Not a special case: `NONE` has no slash, so it is
			// malformed by the ordinary rule. The reserved literal is
			// matched EXACTLY because nothing in this dimension folds
			// case — see the preservation test below.
			"the reserved literal in caps", "NONE", "", false,
			"the literal is matched exactly; `NONE` is simply a value with no slash",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := FacetWorkflowState.CanonicalValue(c.in)
			if ok != c.valid {
				t.Fatalf("CanonicalValue(%q) ok = %v, want %v — %s", c.in, ok, c.valid, c.why)
			}
			if ok && got != c.want {
				t.Errorf("CanonicalValue(%q) = %q, want %q — %s", c.in, got, c.want, c.why)
			}
		})
	}
}

// TestWorkflowState_IdentityIsNotRewritten.
//
// ⛔ THE COLUMNS ARE FREE TEXT AND THEIR EXACT BYTES ARE THE IDENTITY.
// `workflow_states.domain` and `.code` are `text` with no CHECK
// constraint, and #897 lets an operator define a code by typing it. This
// is [FacetTag]'s answer, not [FacetVisibility]'s: folding `Final` to
// `final` would ask for a state that does not exist while looking like
// it asked for the one that does.
func TestWorkflowState_IdentityIsNotRewritten(t *testing.T) {
	for _, v := range []string{
		"asset:1/Published",
		"asset:1/PENDING_REVIEW",
		"asset:1/awaiting art director",
		`asset:1/say "when"`,
		`asset:1/C:\art\ref`,
		`asset:1/trailing\`,
		"asset:1/stage/final",
		"Asset:1/published",
	} {
		got, ok := FacetWorkflowState.CanonicalValue(v)
		if !ok {
			t.Errorf("CanonicalValue(%q) refused a well-formed identity", v)
			continue
		}
		if got != v {
			t.Errorf("CanonicalValue rewrote the identity:\n  in  %q\n  out %q\n"+
				"  ⛔ nothing may lowercase, trim or normalise it — the bytes ARE the key", v, got)
		}
	}
}

// TestWorkflowState_IsNotOrderedAndNotConjunctive — the two
// classifications, asserted rather than inherited.
//
// ⛔ [TestOrderedDimension_ClassificationIsShort] holds the ordered set
// to exactly {file_size}; this states the same fact from the other side
// and says WHY, so a future reader does not have to infer it from an
// absence.
func TestWorkflowState_IsNotOrderedAndNotConjunctive(t *testing.T) {
	if FacetWorkflowState.ordered() {
		t.Error("workflow_state is classified as ORDERED — its values are identities, " +
			"not bounds, and it carries no comparison operator")
	}
	if FacetWorkflowState.orderedDomain() != domainNone {
		t.Errorf("workflow_state's ordered domain is %v, want none", FacetWorkflowState.orderedDomain())
	}
	if FacetWorkflowState.conjunctive() {
		t.Error("workflow_state is conjunctive — an asset holds exactly ONE state, so " +
			"AND returns nothing forever, which is a filter that looks applied and is not")
	}
	// A value that merely LOOKS like a bound is still an opaque
	// identity in a dimension that is not ordered.
	if _, ok := ParseSelection([]string{"workflow_state:asset:1/>=5"}); ok != nil {
		t.Errorf("`asset:1/>=5` was rejected (%v) — `>=5` is a legal operator-defined code", ok)
	}
}

// TestWorkflowState_ValuesShareOneSubGroupAndOr is the N>=2 grouping
// rule at the SQL level.
//
// ⛔ Two identities must land in ONE sub-group joined by OR. Separate
// sub-groups would AND them, which returns nothing forever.
func TestWorkflowState_ValuesShareOneSubGroupAndOr(t *testing.T) {
	s := Selection{}.
		With(FacetWorkflowState, "asset:1/draft").
		With(FacetWorkflowState, "asset:1/published")
	frag, args, ok := s.SQL(visibility.EntityAsset, "a", 0, RenderContext{})
	if !ok {
		t.Fatal("two workflow states were unsatisfiable for an asset")
	}
	if got := topLevelGroups(t, frag); got != 1 {
		t.Errorf("two workflow states rendered %d sub-groups, want 1 — an unordered "+
			"dimension has exactly one.\nSQL: %s", got, frag)
	}
	if !strings.Contains(frag, " OR ") {
		t.Errorf("two workflow states were not ORed.\nSQL: %s", frag)
	}
	if len(args) != 2 {
		t.Errorf("two terms bound %d args, want 2 — one placeholder per term", len(args))
	}
	// A concrete identity beside `none` is the SAME one sub-group: an
	// asset either carries that state or carries none.
	mixed := Selection{}.
		With(FacetWorkflowState, "asset:1/draft").
		With(FacetWorkflowState, WorkflowStateNone)
	mixedFrag, _, ok := mixed.SQL(visibility.EntityAsset, "a", 0, RenderContext{})
	if !ok {
		t.Fatal("a concrete state beside `none` was unsatisfiable")
	}
	if got := topLevelGroups(t, mixedFrag); got != 1 || !strings.Contains(mixedFrag, " OR ") {
		t.Errorf("a concrete state beside `none` rendered %d sub-groups (want 1) and "+
			"OR=%v.\nSQL: %s", got, strings.Contains(mixedFrag, " OR "), mixedFrag)
	}
}

// TestWorkflowState_SplitsAtTheFirstSlashInSQL.
//
// ⛔ `split_part(v, '/', 2)` is the obvious spelling and it is WRONG: it
// stops at the SECOND slash and silently truncates an operator-defined
// code that contains one. The rendered predicate must derive the code
// positionally from the FIRST separator.
func TestWorkflowState_SplitsAtTheFirstSlashInSQL(t *testing.T) {
	frag, _, ok := Selection{}.With(FacetWorkflowState, "asset:1/stage/final").
		SQL(visibility.EntityAsset, "a", 0, RenderContext{})
	if !ok {
		t.Fatal("a slash-bearing code was unsatisfiable")
	}
	if strings.Contains(frag, "split_part") {
		t.Errorf("the predicate uses split_part, which truncates a code at its own "+
			"slash.\nSQL: %s", frag)
	}
	for _, want := range []string{"strpos(", "substr(", "workflow_states"} {
		if !strings.Contains(frag, want) {
			t.Errorf("the predicate does not contain %q.\nSQL: %s", want, frag)
		}
	}
	// ⛔ No domain whitelist. Restricting the lookup to `asset:%` would
	// hide the misfiled row this dimension exists to surface.
	if strings.Contains(frag, "asset:%") || strings.Contains(frag, "LIKE") {
		t.Errorf("the predicate restricts the domain — a misfiled asset carrying a "+
			"`post` state must still be findable.\nSQL: %s", frag)
	}
}

// TestWorkflowState_NoneRendersIsNull — the reserved literal reaches a
// NULL test rather than a lookup for a state named "none".
func TestWorkflowState_NoneRendersIsNull(t *testing.T) {
	frag, args, ok := Selection{}.With(FacetWorkflowState, WorkflowStateNone).
		SQL(visibility.EntityAsset, "a", 0, RenderContext{})
	if !ok {
		t.Fatal("`none` was unsatisfiable for an asset")
	}
	if !strings.Contains(frag, "state_id IS NULL") {
		t.Errorf("`none` does not render a NULL test.\nSQL: %s", frag)
	}
	if len(args) != 1 || args[0] != WorkflowStateNone {
		t.Errorf("args = %v, want exactly [%q] — the value stays in the placeholder",
			args, WorkflowStateNone)
	}
}

// TestWorkflowState_OnlyAssetsSatisfyIt is the entity-arm contract.
//
// ⛔ EXACTLY ok=false, which [Selection.SQL] turns into
// satisfiable=false and every call site honours by skipping the entity.
// An arm that treated an active narrowing filter as no constraint would
// return every post and every collection beside the qualifying assets —
// a filter that made the result set LARGER.
func TestWorkflowState_OnlyAssetsSatisfyIt(t *testing.T) {
	for _, v := range []string{"asset:1/published", "post/published", WorkflowStateNone} {
		s := Selection{}.With(FacetWorkflowState, v)
		if _, _, ok := s.SQL(visibility.EntityAsset, "a", 0, RenderContext{}); !ok {
			t.Errorf("%q made the ASSET arm unsatisfiable — the column is assets.state_id", v)
		}
		if _, _, ok := s.SQL(visibility.EntityPost, "p", 0, RenderContext{}); ok {
			t.Errorf("%q left the POST arm satisfiable. A draft appears on no shared "+
				"surface including search, so `post/wip` is unreachable there and "+
				"`post/published` is tautological", v)
		}
		if _, _, ok := s.SQL(visibility.EntityCollection, "c", 0, RenderContext{}); ok {
			t.Errorf("%q left the COLLECTION arm satisfiable — a collection has no "+
				"state_id column at all", v)
		}
	}
}

// TestWorkflowState_SQLRefusesAValueTheParserWouldHaveRejected is the
// second gate [Selection.With] makes necessary: it is exported, takes no
// error, and a programmatic caller can seed a term the parser never saw.
func TestWorkflowState_SQLRefusesAValueTheParserWouldHaveRejected(t *testing.T) {
	for _, bad := range []string{"published", "/published", "asset:1/", ""} {
		s := Selection{}.With(FacetWorkflowState, bad)
		if _, _, ok := s.SQL(visibility.EntityAsset, "a", 0, RenderContext{}); ok {
			t.Errorf("SQL accepted the malformed value %q — it must fail closed", bad)
		}
	}
}

// TestWorkflowState_CacheKeySeparatesTheValues — two different
// selections are two different result sets and must not share bytes.
func TestWorkflowState_CacheKeySeparatesTheValues(t *testing.T) {
	none := Selection{}.With(FacetWorkflowState, WorkflowStateNone)
	one := Selection{}.With(FacetWorkflowState, "asset:1/published")
	two := one.With(FacetWorkflowState, "asset:1/draft")
	keys := map[string]string{
		"none": none.CacheKey(), "one": one.CacheKey(), "two": two.CacheKey(),
	}
	seen := map[string]string{}
	for name, k := range keys {
		if prev, dup := seen[k]; dup {
			t.Errorf("%q and %q share a cache key (%q) — one would be served the "+
				"other's bytes for the rest of the TTL", name, prev, k)
		}
		seen[k] = name
	}
	// Tick order is not part of the query.
	reversed := Selection{}.With(FacetWorkflowState, "asset:1/draft").
		With(FacetWorkflowState, "asset:1/published")
	if reversed.CacheKey() != two.CacheKey() {
		t.Errorf("tick order changed the cache key:\n  %q\n  %q", two.CacheKey(), reversed.CacheKey())
	}
}
