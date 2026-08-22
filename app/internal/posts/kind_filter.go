// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// The browse feed's `?kind=` parameter, translated into the shared
// filter grammar (#1251, ADR 0093 decision 1).
//
// # What used to be here, and why it is not
//
// A `kindFilterSQL` that built the conjunct itself: the compiled
// asset-type refs and extension sets as three bound arrays, the
// per-member EXISTS, and the field-plane readability rule spliced inside
// it. It was correct and it was a SECOND implementation of a filter —
// the shape ADR 0093 exists to stop, because the next arm (`tag`,
// `visibility`, ADR 0092's operator-defined fields) would each have
// repeated it, and every gate this project has had to fix twice was a
// rule that existed in more than one expression.
//
// The predicate now lives in facet.dimensionSQL's [facet.FacetKind] arm,
// where /search reads the identical one. What is left here is the
// TRANSLATION — which is all decision 1's "thin translation into the
// query grammar rather than a second query builder" asks for, and it is
// two functions with no SQL in them.

// kindSelection turns the parsed `?kind=` list into the grammar's terms.
//
// ⚠️ IT IS ONLY HALF THE ANSWER, and the other half is the caller's
// [ListPostsPageParams.KindsRequested] flag, because an empty result
// here has two opposite meanings that this function cannot tell apart:
// nobody asked for a kind filter, and somebody asked for one and named
// nothing that exists. The first must return the whole feed and the
// second must return nothing, so the branch stays at the call site where
// `KindsRequested` is in scope.
//
// The wire vocabularies of `?kind=` and `filter=kind:` differ on
// purpose and both fail closed. `viewkind.ParseList` DROPS an
// unrecognised name from a comma list, so `?kind=image,nonsense` still
// narrows to image; [facet.ParseSelection] REJECTS one outright with a
// 400, because there is only ever one value in a `filter=` token and
// nothing left to narrow to. Neither can widen, which is the property
// that matters — see facet.FacetType.canonicalValue's FacetKind arm.
func kindSelection(kinds []viewkind.Kind) facet.Selection {
	var sel facet.Selection
	for _, k := range kinds {
		sel = sel.With(facet.FacetKind, string(k))
	}
	return sel
}

// kindRenderContext resolves this caller down to the inputs
// [facet.RenderContext] needs, so the kind dimension's per-member field
// gate composes the same rule enrichPreview applies in Go when it
// decides `PostMember.Restricted`.
//
// `callerArg` names the placeholder the feed query has ALREADY bound
// with the caller's ref. It is bound whether or not the gate references
// it — see the note on $12 in ListPostsPageGated — so this function
// never has to know which callers the gate folds away for.
//
// Anonymous is `visibility.NewCaller(nil)` and not the zero Caller: the
// zero value reads as "user zero", which skips the anonymous-only status
// conjuncts and is therefore WIDER. Building it here rather than
// defaulting it in the grammar is what keeps that decision at the place
// that knows the identity.
func kindRenderContext(id *auth.Identity, callerArg string) facet.RenderContext {
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
