// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// The browse feed's FILTER parameters, translated into the shared filter
// grammar (#1251, ADR 0093 decisions 1 and 3).
//
// # What used to be here, and why it is not
//
// A `kindFilterSQL` that built the kind conjunct itself, and — one file
// over, in list_page.go — two more hand-built conjuncts: `visibility =
// ANY($3)` and an EXISTS over `post_tags` at `$5`. The tag one was a
// byte-for-byte second copy of facet.dimensionSQL's [facet.FacetTag]
// post arm, which is exactly the shape ADR 0093 exists to stop: two
// implementations that agree today with nothing asserting they must keep
// agreeing.
//
// The predicates now live in facet.dimensionSQL, where /search reads the
// identical ones. What is left here is the TRANSLATION — decision 1's
// "thin translation into the query grammar rather than a second query
// builder" — and it is four functions with no SQL in them.
//
// ⚠️ WHAT DID NOT CROSS THE SEAM, and why. Every SCOPING parameter —
// author, team, follow set, liked-by, the draft switch — is still bound
// and applied in list_page.go, because scope selects the CORPUS rather
// than narrowing within it (decision 2), and so are the feed's ordering
// and its keyset cursor.

// feedFilters folds the feed's three filter parameters into ONE
// [facet.Selection] — the whole of what this query narrows by, in one
// value, rendered by one call.
//
// ⛔ IT IS NOT REACHED WITH AN AMBIGUOUS EMPTY. Two of the three
// parameters have a "requested but naming nothing" state that must
// select NOTHING while their absent state selects EVERYTHING, and those
// two readings are one line apart and differ by the whole feed. A
// [facet.Selection] cannot carry the distinction — an empty selection is
// "no constraint" by definition — so the branch stays at the call site
// in ListPostsPageGated, which returns an empty page before ever calling
// this. See [ListPostsPageParams.KindsRequested] and
// [ListPostsPageParams.Visibility].
//
// The dimensions compose as the grammar composes them, and the two
// answers differ:
//
//   - `visibility` and `kind` are NON-CONJUNCTIVE, so two tiers or two
//     kinds are the UNION. That is what the feed's default tier set has
//     meant since #1193 and what the footer's multi-select has meant
//     since #1166.
//   - `tag` is CONJUNCTIVE — the only dimension that is — so two tags
//     mean "carries EVERY tag", matching what `tag:a tag:b` has
//     documented in the DSL since it shipped.
//
// ⚠️ THAT LAST ONE IS NEW TO THIS SURFACE. The feed's parameter was a
// `*string` and could not express two tags at all, so composing through
// the grammar hands it a plural case it never had. It is decided here
// rather than inherited, and it is asserted with arithmetic rather than
// membership — see TestTagFilter_TwoTagsIntersect, which requires
// `both < min(a, b)` strictly, because `both > 0` passes on the union
// that #1165 and #1242 each found shipped.
//
// Across dimensions the fragment ANDs, which is what makes the browse
// page's controls narrow each other rather than compete.
func feedFilters(p ListPostsPageParams) facet.Selection {
	var sel facet.Selection
	for _, tier := range p.Visibility {
		sel = sel.With(facet.FacetVisibility, tier)
	}
	for _, tag := range p.Tags {
		sel = sel.With(facet.FacetTag, tag)
	}
	if p.KindsRequested {
		for _, k := range p.Kinds {
			sel = sel.With(facet.FacetKind, string(k))
		}
	}
	return sel
}

// feedFiltersSelectNothing reports that the caller ASKED for a filter and
// named nothing it can be — the state that must answer an empty page
// rather than the whole feed.
//
// It is a separate function from [feedFilters] because the two readings
// of "no terms" are opposite and a single return value cannot carry both.
// `?kind=nonsense` parses to a present-but-empty kind selection; an
// absent `?kind=` parses to the same emptiness with `KindsRequested`
// false. Collapsing them turns a typo into "show me everything", which is
// the one direction a narrowing filter may never move.
//
// `Visibility` has the identical shape and its own note in
// [ListPostsPageParams]: NIL means no tier filter, an EMPTY NON-NIL slice
// means a caller named a set with nothing in it. The hand-built SQL this
// replaced gave that slice `= ANY('{}')`, which matches no row, so the
// degradation was already to an empty page — and preserving it is not a
// formality, because the grammar's own default for zero terms is the
// opposite.
//
// `Tags` has NO such state and is deliberately not checked here. The
// handler drops blank values before they reach the query, so an empty
// slice means "no tag filter" and nothing else; there is no spelling of
// `?tag=` that asks for a tag and names none.
func feedFiltersSelectNothing(p ListPostsPageParams) bool {
	if p.Visibility != nil && len(p.Visibility) == 0 {
		return true
	}
	if p.KindsRequested && len(p.Kinds) == 0 {
		return true
	}
	return false
}

// kindSelection turns a parsed `?kind=` list into the grammar's terms.
//
// Retained beside [feedFilters] for the tests that drive the kind
// dimension alone; the feed itself goes through feedFilters, which folds
// this in with the other two.
//
// The wire vocabularies of `?kind=` and `filter=kind:` differ on purpose
// and both fail closed. `viewkind.ParseList` DROPS an unrecognised name
// from a comma list, so `?kind=image,nonsense` still narrows to image;
// [facet.ParseSelection] REJECTS one outright with a 400, because there
// is only ever one value in a `filter=` token and nothing left to narrow
// to. Neither can widen, which is the property that matters — see
// facet.FacetType.canonicalValue's FacetKind arm.
func kindSelection(kinds []viewkind.Kind) facet.Selection {
	var sel facet.Selection
	for _, k := range kinds {
		sel = sel.With(facet.FacetKind, string(k))
	}
	return sel
}

// feedRenderContext resolves this caller down to the inputs
// [facet.RenderContext] needs, so the kind dimension's per-member field
// gate composes the same rule enrichPreview applies in Go when it decides
// `PostMember.Restricted`.
//
// `callerArg` names the placeholder the feed query has ALREADY bound with
// the caller's ref. It is bound whether or not the gate references it —
// see the note on $10 in ListPostsPageGated — so this function never has
// to know which callers the gate folds away for.
//
// ⚠️ NEITHER `tag` NOR `visibility` NEEDS THIS, and it is worth saying
// which of them is the surprising one. A tag predicate reads `post_tags`
// and a tier predicate reads one column; both are caller-INDEPENDENT, so
// the fail-open hazard below cannot arise for them and this context is
// threaded for `kind` alone. It is passed for the whole selection because
// [facet.Selection.SQL] takes one context per call, not one per term.
//
// Anonymous is `visibility.NewCaller(nil)` and not the zero Caller: the
// zero value reads as "user zero", which skips the anonymous-only status
// conjuncts and is therefore WIDER. Building it here rather than
// defaulting it in the grammar is what keeps that decision at the place
// that knows the identity.
func feedRenderContext(id *auth.Identity, callerArg string) facet.RenderContext {
	rc := facet.RenderContext{
		Caller:    visibility.NewCaller(nil),
		CallerArg: callerArg,
	}
	if id != nil {
		check := func(code string) bool { return id.Can(code) }
		rc.Caller = visibility.NewCaller(&id.UserRef)
		rc.Caps = visibility.ResolveContentCaps(check)
		rc.MutationCaps = visibility.ResolveAssetMutationCaps(
			check, id.ScopedTeams(visibility.AssetsAdmin))
	}
	return rc
}
