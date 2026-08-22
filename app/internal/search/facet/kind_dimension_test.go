// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1251 — THE `kind` DIMENSION'S PARSER AND ITS RENDER CONTRACT.
//
// No database, for the reason selection_test.go's header gives: these
// are properties of the grammar, and a grammar bug that reached SQL
// would only be caught on a machine with a stack up and only for the
// kinds someone remembered to seed. These run everywhere, every time.
//
// The two that matter most are the two directions a filter can fail:
//
//   - [TestKindDimension_UnknownValueRejected] — a name outside the
//     vocabulary must be REFUSED, never carried through to a predicate.
//   - [TestKindDimension_PostArmNeedsACaller] — the post arm must be
//     UNSATISFIABLE without a caller, because rendering it with a zero
//     visibility.Caller would read as "user zero", which is wider than
//     anonymous. A forgotten render context has to lose rows, not gain
//     them.

package facet

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/viewkind"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

func kindRC() RenderContext {
	return RenderContext{
		Caller:    visibility.NewCaller(nil),
		CallerArg: "$1",
	}
}

// TestKindDimension_UnknownValueRejected pins the fail-closed reading of
// the value grammar. `filter=kind:junk` is a 400 out of ParseSelection,
// exactly as `filter=ai:maybe` is, because there is one value in the
// token and nothing to narrow to if it is dropped.
//
// ⚠️ This is deliberately NOT the same answer the feed's `?kind=` gives.
// That parameter is a COMMA LIST, so a junk term beside a real one must
// still narrow to the real one — see viewkind.ParseList and
// posts.kindSelection. Both refuse to widen; they differ only in what
// there is left to do after refusing.
func TestKindDimension_UnknownValueRejected(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"kind:junk", "kind:", "kind:images", "kind:IMAGE_"} {
		if _, err := ParseSelection([]string{bad}); err == nil {
			t.Errorf("ParseSelection(%q) was accepted; an unknown kind must be a 400", bad)
		}
	}
	// Every name in the vocabulary parses, including the ones no asset
	// can resolve to — `sequence` is a real kind and a real question,
	// and its answer is an empty page rather than an error.
	for _, k := range viewkind.All() {
		sel, err := ParseSelection([]string{"kind:" + string(k)})
		if err != nil {
			t.Errorf("ParseSelection(kind:%s): %v", k, err)
			continue
		}
		if got := sel.Params(); len(got) != 1 || got[0] != "kind:"+string(k) {
			t.Errorf("kind:%s round-tripped as %v", k, got)
		}
	}
	// Case and whitespace fold, so a hand-typed token and a rail's token
	// share one cache key.
	sel, err := ParseSelection([]string{"kind: Image "})
	if err != nil {
		t.Fatalf("ParseSelection(kind: Image ): %v", err)
	}
	if got := sel.Params(); len(got) != 1 || got[0] != "kind:image" {
		t.Errorf("kind: Image  canonicalised to %v, want [kind:image]", got)
	}
}

// TestKindDimension_PostArmNeedsACaller is the fail-closed guard on
// [RenderContext].
//
// The post arm's predicate carries visibility.FieldsReadableSQL for each
// candidate MEMBER, and that fragment takes a visibility.Caller. Go's
// zero value for one is `{UserRef: 0, IsAnonymous: false}` — "user
// zero" — which skips the anonymous-only status conjuncts and is
// therefore WIDER than an anonymous reader. A dimension that rendered a
// forgotten context would fail OPEN, which is the one direction none of
// this may move.
//
// So: no CallerArg, no predicate, and no rows. The ASSET arm is
// unaffected, because there the row being filtered is the row the
// execution site's own field-plane conjunct already covers.
func TestKindDimension_PostArmNeedsACaller(t *testing.T) {
	t.Parallel()

	sel := (Selection{}).With(FacetKind, "image")

	if _, _, ok := sel.SQL(visibility.EntityPost, "posts", 0, RenderContext{}); ok {
		t.Error("the post arm rendered without a caller; a forgotten render context " +
			"must lose rows, never gain them")
	}
	frag, args, ok := sel.SQL(visibility.EntityPost, "posts", 0, kindRC())
	if !ok {
		t.Fatal("the post arm was unsatisfiable WITH a caller")
	}
	if len(args) != 1 || args[0] != "image" {
		t.Errorf("one term bound %d args (%v), want exactly the kind name", len(args), args)
	}
	// The member gate is inside the EXISTS, beside the match — not
	// hoisted to the post, which is the implementation
	// posts.TestKindFilter_RestrictedMemberIsNeverProbeable exists to
	// fail. Its `owner_user_ref` disjunct is the cheapest proof that
	// FieldsReadableSQL rendered at all, and its position relative to
	// the closing paren is the proof it rendered INSIDE.
	if !strings.Contains(frag, "post_assets fkp") {
		t.Fatalf("the post arm does not range over the membership:\n%s", frag)
	}
	if !strings.Contains(frag, "fkm.owner_user_ref") {
		t.Errorf("the post arm carries no per-member field gate — a restricted "+
			"member's kind would be recoverable by elimination:\n%s", frag)
	}
	if strings.Index(frag, "fkm.owner_user_ref") < strings.Index(frag, "post_assets fkp") {
		t.Errorf("the member gate is rendered OUTSIDE the membership EXISTS:\n%s", frag)
	}

	// The asset arm needs nothing: an asset IS the row.
	if _, _, ok := (Selection{}).With(FacetKind, "image").
		SQL(visibility.EntityAsset, "assets", 0, RenderContext{}); !ok {
		t.Error("the asset arm was unsatisfiable without a caller; it reads no member")
	}
	// A collection has no members that resolve to a badge kind, so it
	// drops out of a kind-filtered page entirely — zero hits AND zero
	// count, never "no constraint".
	if _, _, ok := (Selection{}).With(FacetKind, "image").
		SQL(visibility.EntityCollection, "c", 0, kindRC()); ok {
		t.Error("a collection claimed to satisfy `kind:`; it has no badge kind")
	}
}

// TestKindDimension_ValuesCombineWithOR is the N≥2 rule, asserted rather
// than assumed — ADR 0093's amendment asks for exactly this whenever a
// dimension can appear more than once, because "singular right, plural
// wrong" is invisible to any test that uses one of the thing.
//
// The rule for `kind` is OR: the control is a multi-select and
// `?kind=image,video` has meant the union since #1166. The rendered
// shape is what proves it — one group, joined by OR — and the row-level
// arithmetic is asserted against a real corpus by
// posts.TestKindFilter_MultiSelectIsTheUnion.
func TestKindDimension_ValuesCombineWithOR(t *testing.T) {
	t.Parallel()

	frag, args, ok := (Selection{}).With(FacetKind, "image").With(FacetKind, "video").
		SQL(visibility.EntityAsset, "assets", 0, kindRC())
	if !ok {
		t.Fatal("two kind terms were unsatisfiable")
	}
	if len(args) != 2 {
		t.Fatalf("two terms bound %d args, want 2", len(args))
	}
	if strings.Count(frag, " OR ") == 0 {
		t.Errorf("two kind terms did not OR:\n%s", frag)
	}
	// AND would be the bug in both directions at once: unsatisfiable for
	// an asset (one row, one kind) and satisfiable-but-wrong for a post
	// ("holds both"), so the same two terms would mean different things
	// on two entities.
	if strings.Contains(frag, " AND (") && strings.Count(frag, "AND (") > 1 {
		t.Errorf("two kind terms rendered as separate ANDed groups:\n%s", frag)
	}
	// A term whose kind no asset can resolve to renders a predicate that
	// simply never matches — no "empty selection" special case, which is
	// the concept viewkind.KindSQL removed.
	if _, _, ok := (Selection{}).With(FacetKind, "sequence").
		SQL(visibility.EntityAsset, "assets", 0, kindRC()); !ok {
		t.Error("kind:sequence was reported UNSATISFIABLE; it must render a " +
			"never-matching predicate so `kind:image,sequence` still narrows to image")
	}
}
