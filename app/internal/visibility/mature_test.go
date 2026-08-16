// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1115 — the mature qualification predicate (ADR 0090).
//
// # Why every case is a PAIR
//
// A three-conjunct rule with two exemptions has a lot of ways to be
// uniformly wrong, and every one of them passes a single-caller
// assertion. "The opted-in user sees the mature post" passes on a gate
// that admits everybody. "The anonymous visitor does not" passes on a
// gate that admits nobody, which would hide the entire library from
// every opted-out reader — a far worse outage than the leak, and one
// with the same test output.
//
// So the table below varies ONE conjunct at a time against a fixed
// remainder, and asserts the pair. The row that separates "the rule
// consults the instance switch" from "the rule consults the user" is
// the one where those two disagree.
//
// # The non-mature control is not filler
//
// `TestMatureItemVisible_NonMatureIsAlwaysVisible` is the assertion
// that would catch the catastrophic version of this bug: a predicate
// that asks about the viewer's opt-in for EVERY item rather than for
// mature ones. Every other test in this file passes on that
// implementation.
//
// No database: the predicate is query-free by design, so the whole
// input space is driven exhaustively rather than sampled.

package visibility

import (
	"fmt"
	"testing"
)

// qualified is the viewer every negative case is derived from by
// turning exactly one thing off — which is what makes each case a pair
// rather than an assertion about a constant.
var qualified = MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}

func TestQualifiesForMature_OneConjunctAtATime(t *testing.T) {
	cases := []struct {
		name string
		v    MatureViewer
		want bool
	}{
		{"all three", qualified, true},
		// Each of the three, turned off alone. The `want` differs from
		// the row above by exactly one input, which is the property that
		// makes a uniformly-wrong gate fail here.
		{"not signed in", MatureViewer{SignedIn: false, OptedIn: true, InstanceAllows: true}, false},
		{"not opted in", MatureViewer{SignedIn: true, OptedIn: false, InstanceAllows: true}, false},
		{"instance disallows", MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: false}, false},
		// The zero value must disqualify: a handler that has lost its
		// inputs has to refuse rather than widen.
		{"zero value", MatureViewer{}, false},
		{"AnonymousMatureViewer", AnonymousMatureViewer, false},
		// The case the owner named explicitly: an opted-in user on an
		// instance that has switched the feature off. The operator's
		// answer is about the install and outranks the reader's about
		// themselves.
		{"opted in but instance off", MatureViewer{SignedIn: true, OptedIn: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QualifiesForMature(tc.v); got != tc.want {
				t.Errorf("QualifiesForMature(%+v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// TestMatureItemVisible_NonMatureIsAlwaysVisible is the control arm,
// and it is the most important test in this file.
//
// The mature rule is a filter over MATURE things. A predicate that
// consulted the opt-in for every item would hide the whole library from
// every reader who has not opted in — and it would pass every other
// assertion here, because every other assertion is about mature items.
func TestMatureItemVisible_NonMatureIsAlwaysVisible(t *testing.T) {
	for _, v := range []MatureViewer{
		qualified,
		{}, // anonymous, disqualified in every way
		{SignedIn: true},
		{SignedIn: true, InstanceAllows: true},
	} {
		for _, owner := range []bool{true, false} {
			for _, admin := range []bool{true, false} {
				if !MatureItemVisible(v, false, owner, admin) {
					t.Fatalf("a NON-mature item was hidden from %+v (owner=%v admin=%v) — "+
						"the rule is filtering on the viewer rather than on the item, which "+
						"hides the entire library from everyone who has not opted in",
						v, owner, admin)
				}
			}
		}
	}
}

// TestMatureItemVisible_SameItemOppositeVerdicts is the pair the issue
// asks for, per arm: ONE mature item, two viewers, opposite answers.
func TestMatureItemVisible_SameItemOppositeVerdicts(t *testing.T) {
	arms := []struct {
		name         string
		admits       MatureViewer
		refuses      MatureViewer
		what         string
	}{
		{
			name:    "anonymous vs opted-in",
			admits:  qualified,
			refuses: MatureViewer{},
			what:    "signing in",
		},
		{
			name:    "opted-out vs opted-in",
			admits:  qualified,
			refuses: MatureViewer{SignedIn: true, OptedIn: false, InstanceAllows: true},
			what:    "the reader's own opt-in",
		},
		{
			name:    "instance off vs instance on",
			admits:  qualified,
			refuses: MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: false},
			what:    "the operator's switch",
		},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			// Neither viewer is the owner and neither is an admin, so the
			// only thing that can differ is the conjunct under test.
			if !MatureItemVisible(arm.admits, true, false, false) {
				t.Errorf("the admitted viewer was refused — %s is not being consulted, or the "+
					"gate refuses everyone", arm.what)
			}
			if MatureItemVisible(arm.refuses, true, false, false) {
				t.Errorf("the refused viewer was admitted — %s is not being consulted, or the "+
					"gate admits everyone", arm.what)
			}
		})
	}
}

// TestMatureItemVisible_ExemptionsSurviveTheInstanceSwitch is the arm
// that a "qualified OR owner" spelling gets right only by luck.
//
// ADR 0090 §2: the owner and the admin are checked BEFORE the
// qualification, so they survive the operator switching the feature off.
// That is the case that matters — if flipping a display switch made an
// artist's own uploads invisible to them, the operator would have
// destroyed access to content the artist owns.
func TestMatureItemVisible_ExemptionsSurviveTheInstanceSwitch(t *testing.T) {
	// The most hostile viewer state there is: not signed in, not opted
	// in, on an instance that disallows the feature entirely.
	nobody := MatureViewer{}

	// The owner is SIGNED IN, and that is not incidental: an anonymous
	// caller carries the sentinel ref 0, so a row owned by ref 0 would
	// compare equal and take the artist's exemption. See the guard in
	// MatureItemVisible and its twin `NULLIF` in the SQL.
	signedInOwner := MatureViewer{SignedIn: true}
	if !MatureItemVisible(signedInOwner, true, true, false) {
		t.Error("the OWNER could not see their own mature asset on a disallowing instance — " +
			"an operator would have destroyed access to content the artist owns by flipping " +
			"a display switch")
	}
	// …and an ANONYMOUS caller claiming ownership is refused, which is
	// the sentinel-ref hazard, not a hypothetical: this is the exact
	// case TestMatureFilterSQL_MatchesGo found the two forms disagreeing
	// on.
	if MatureItemVisible(nobody, true, true, false) {
		t.Error("an ANONYMOUS caller was granted the OWNER exemption — user_ref 0 is the " +
			"anonymous sentinel, so a row owned by ref 0 would hand a stranger the artist's " +
			"own exemption")
	}
	if !MatureItemVisible(nobody, true, false, true) {
		t.Error("a system admin could not see a mature asset on a disallowing instance — " +
			"they have to be able to moderate what the switch hid")
	}
	// And the control: strip both exemptions and the same viewer is
	// refused, so the two assertions above are the exemptions and not a
	// gate that admits everyone.
	if MatureItemVisible(nobody, true, false, false) {
		t.Error("a disqualified non-owner non-admin was admitted — the exemptions are not " +
			"exemptions, the gate is open")
	}
}

// TestMatureFilterSQL_MatchesGo is the twin every SQL transcription in
// this package carries (ADR 0063's rule: two expressions of one rule is
// the defect).
//
// It drives EVERY combination of the six inputs through both forms and
// fails on the first disagreement. The SQL is evaluated by rendering it
// against known column values rather than by running Postgres, which is
// the trade this test makes deliberately: it cannot catch a syntax
// error (the integration suite does that by executing the queries), and
// it CAN catch the thing that actually drifts, which is a changed
// verdict.
//
// ⚠️ It is a consistency check, and the #907 lesson applies: two
// consistently-wrong expressions pass it. That is why the tests above
// exist beside it, asserting the RULE against stated expectations
// rather than against the other implementation.
func TestMatureFilterSQL_MatchesGo(t *testing.T) {
	const callerRef = int64(4242)

	for _, isMature := range []bool{true, false} {
		for _, signedIn := range []bool{true, false} {
			for _, optedIn := range []bool{true, false} {
				for _, allows := range []bool{true, false} {
					for _, isOwner := range []bool{true, false} {
						for _, admin := range []bool{true, false} {
							v := MatureViewer{SignedIn: signedIn, OptedIn: optedIn, InstanceAllows: allows}
							name := fmt.Sprintf("mature=%v/in=%v/opt=%v/allow=%v/own=%v/admin=%v",
								isMature, signedIn, optedIn, allows, isOwner, admin)

							goAnswer := MatureItemVisible(v, isMature, isOwner, admin)

							frag := MatureFilterSQL("a", MatureOwnerColAsset, "$1", v, admin)
							sqlAnswer := evalMatureFragment(frag, isMature, isOwner, signedIn, callerRef)

							if goAnswer != sqlAnswer {
								t.Errorf("%s: Go says %v, SQL fragment %q says %v",
									name, goAnswer, frag, sqlAnswer)
							}
						}
					}
				}
			}
		}
	}
}

// evalMatureFragment interprets what [MatureFilterSQL] emits, for the
// one row described by its arguments.
//
// The fragment has exactly two shapes — empty, or the single disjunction
// — so this is a reading of the emitted string rather than a second
// implementation of the rule. It asserts the shape it expects and fails
// loudly on anything else, which is what stops it silently agreeing with
// a fragment it does not understand.
func evalMatureFragment(frag string, isMature, isOwner, signedIn bool, callerRef int64) bool {
	if frag == "" {
		// No conjunct: every row passes.
		return true
	}
	want := ` AND (NOT a.mature OR a.owner_user_ref = NULLIF($1::BIGINT, 0))`
	if frag != want {
		panic("MatureFilterSQL emitted an unrecognised fragment: " + frag +
			" — this evaluator must be taught the new shape, not deleted")
	}
	// NOT a.mature
	if !isMature {
		return true
	}
	// a.owner_user_ref = NULLIF($1::BIGINT, 0) — the NULLIF is what
	// stops an anonymous caller (ref 0) matching a row owned by ref 0.
	if !signedIn || callerRef == 0 {
		return false
	}
	return isOwner
}

// TestMatureFilterSQL_OwnerColumnIsAParameter pins the reason the
// column is not hardcoded: the two tables that carry `mature` disagree
// about which column holds the owner, and a fragment naming the wrong
// one is a runtime error on whichever surface is wired second.
func TestMatureFilterSQL_OwnerColumnIsAParameter(t *testing.T) {
	v := MatureViewer{SignedIn: true} // disqualified, so a fragment is emitted
	asset := MatureFilterSQL("a", MatureOwnerColAsset, "$1", v, false)
	post := MatureFilterSQL("p", MatureOwnerColPost, "$1", v, false)

	if asset == post {
		t.Fatal("the asset and post fragments are identical — the owner column is not being used")
	}
	if want := "a.owner_user_ref"; !contains(asset, want) {
		t.Errorf("asset fragment %q does not name %s", asset, want)
	}
	if want := "p.author_user_ref"; !contains(post, want) {
		t.Errorf("post fragment %q does not name %s", post, want)
	}
}

// TestMatureFilterSQL_QualifiedViewerGetsNoConjunct pins the fast path.
// A qualified viewer and an admin must get the EMPTY string, not a
// constant TRUE — Postgres should never see a filter it has to reason
// about on the common path.
func TestMatureFilterSQL_QualifiedViewerGetsNoConjunct(t *testing.T) {
	if got := MatureFilterSQL("a", MatureOwnerColAsset, "$1", qualified, false); got != "" {
		t.Errorf("a qualified viewer got the conjunct %q, want none", got)
	}
	if got := MatureFilterSQL("a", MatureOwnerColAsset, "$1", MatureViewer{}, true); got != "" {
		t.Errorf("a system admin got the conjunct %q, want none", got)
	}
	if got := MatureFilterSQL("a", MatureOwnerColAsset, "$1", MatureViewer{}, false); got == "" {
		t.Error("a disqualified viewer got NO conjunct — the filter is not being applied")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
