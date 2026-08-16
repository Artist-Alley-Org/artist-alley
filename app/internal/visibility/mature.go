// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import "context"

// ---------------------------------------------------------------------------
// The mature-content qualification predicate (#1115, epic #1114, ADR 0090)
// ---------------------------------------------------------------------------
//
// # A rating is not a clearance
//
// This package already answers "who is ALLOWED" — `sensitivity`, through
// [ContentReadable] and the row predicate. Mature answers a DIFFERENT
// question: "who has OPTED IN". The two axes are independent in both
// directions and a public artwork can be mature, so this is a boolean
// beside the tier ladder and never a value inside it. ADR 0090 §1 has
// the full argument, including why "add a fifth sensitivity tier" is a
// one-line change that would quietly make every mature piece non-public.
//
// The consequence that shapes this file: the two axes are **ANDed at
// read time and never merged**. There is no combined "effective tier"
// here, because the moment one exists someone sorts by it.
//
// # This file is the PREDICATE, not the policy
//
// It has no pool, no queries and no knowledge of what a feed is. It
// takes an already-resolved viewer and answers whether they qualify;
// composing that answer into a query is each surface's job, and ADR 0090
// §3 says which plane each surface composes it on (row plane for feed
// hiding per #921, picture plane for blur per ADR 0020).
//
// Written this way for the reason the rest of the package is: a
// query-free core can be driven exhaustively by a table test, which is
// what a three-conjunct rule with an owner exemption needs.

// MatureViewer is the resolved state of one caller with respect to the
// mature axis. Three inputs, resolved once at the HTTP edge and carried,
// for the reason [ContentCaps] exists: the pieces come from three
// different places (the session, `user_preferences`, `system_config`)
// and re-reading any of them per row is how a per-row check becomes a
// per-row query.
type MatureViewer struct {
	// SignedIn is false for the anonymous sentinel. Kept explicit
	// rather than derived from a ref, so a caller cannot pass ref 0 and
	// accidentally look like user zero — the same guard
	// [Caller.IsAnonymous] exists for.
	SignedIn bool
	// OptedIn is the user's `show_mature` preference. FALSE is the
	// default and the zero value, which is the safe answer: a user with
	// no preferences row, an empty blob, or a key this build has never
	// heard of is not opted in.
	OptedIn bool
	// InstanceAllows is the operator's switch (sysconfig
	// `mature_content`). ⚠️ Note the polarity flip from the stored
	// struct: `MatureContentConfig.Disallowed` zero-values to false so
	// an unconfigured install is permissive, and callers convert with
	// `.Allowed()`. Here the field is named for what it means, because
	// this struct's zero value is deliberately the DISQUALIFIED viewer.
	InstanceAllows bool
}

// AnonymousMatureViewer is the zero value spelled out: nobody, on an
// instance that allows nothing. It is what a handler that has lost its
// identity should pass, because the zero value of this struct
// disqualifies — a gate that loses its inputs must refuse rather than
// widen.
var AnonymousMatureViewer = MatureViewer{}

// QualifiesForMature reports whether this viewer may be shown mature
// content AT ALL.
//
// Three conjuncts, all required (ADR 0090 §2):
//
//	signed in  — an anonymous viewer can never opt in: there is nowhere
//	             to store the answer and nobody to hold to it. An
//	             instance-wide "show mature to anonymous visitors" switch
//	             is a DIFFERENT product decision (the operator answering
//	             for people who have not answered) and is not this.
//	opted in   — the signed-in default is HIDDEN, per the owner.
//	instance   — the operator's answer is about the install and outranks
//	             a reader's answer about themselves. With the switch off,
//	             an opted-in user still does not qualify.
//
// This is the WHOLE of the viewer-side rule. It deliberately says
// nothing about any particular item — see [MatureItemVisible] for the
// per-item answer, which is this plus the two exemptions.
func QualifiesForMature(v MatureViewer) bool {
	return v.SignedIn && v.OptedIn && v.InstanceAllows
}

// MatureItemVisible reports whether this viewer may be shown a
// particular item, given whether that item is flagged mature.
//
// A NON-MATURE ITEM IS ALWAYS VISIBLE ON THIS AXIS. That is the sentence
// this function exists to make impossible to get wrong: the mature rule
// is a filter over mature things, and a gate that consulted the viewer's
// opt-in for every item would hide the entire library from an opted-out
// reader. It is stated as the first branch rather than left implicit in
// a boolean expression, because the implicit version is a plausible
// typo away from exactly that outage.
//
// The two EXEMPTIONS, which are asymmetries on purpose (ADR 0090 §2):
//
//   - the OWNER. An artist must be able to see their own work. If
//     switching the instance off made an artist's own uploads invisible
//     to them, an operator would have destroyed access to content the
//     artist owns by flipping a display switch.
//   - SYSTEM ADMIN, who has to be able to moderate what the switch hid.
//
// Both are checked BEFORE the qualification, so they survive the
// instance switch being off — which is the case that matters, and the
// one a "qualified OR owner" spelling gets right by luck rather than by
// construction.
//
// ⚠️ THE OWNER EXEMPTION REQUIRES A SIGNED-IN VIEWER, and that guard is
// load-bearing rather than defensive. `isOwner` is computed by the
// caller from a comparison against the caller's user_ref, and an
// anonymous caller carries the sentinel ref 0 — so a row whose owner is
// ref 0 would compare EQUAL and hand an anonymous visitor an exemption
// meant for the artist. It is the same hazard [ContentReadable]
// documents for its own ownership comparison, and no user has ref 0
// today only as a matter of data rather than structure.
//
// It is also what keeps this in step with [MatureFilterSQL], whose
// `NULLIF(…, 0)` refuses the identical case. TestMatureFilterSQL_MatchesGo
// found the disagreement: without this line the Go form said VISIBLE
// where the SQL said hidden, so an anonymous reader's per-item check and
// their feed query would have answered differently about the same asset.
//
// ⚠️ This is the mature axis ONLY. It composes with — never replaces —
// the row predicate and [ContentReadable]. A caller ANDs it in; it can
// only ever narrow.
func MatureItemVisible(v MatureViewer, itemIsMature, isOwner, isSystemAdmin bool) bool {
	if !itemIsMature {
		return true
	}
	if (isOwner && v.SignedIn) || isSystemAdmin {
		return true
	}
	return QualifiesForMature(v)
}

// MatureFilterSQL is the SQL transcription of the per-item rule, as a
// WHERE-clause conjunct (it starts with " AND "). `alias` is the table
// alias ("" for none) and the column is assumed to be `mature` on it —
// true for both `assets` and `posts`, which is why the derived post
// column is named the same as the asset one.
//
// It exists for the LIST surfaces, which reduce many rows to a page and
// so have no per-row Go step to decide in. Everything else calls
// [MatureItemVisible]; this is not the preferred form.
//
// Two expressions of one rule is the defect ADR 0063 exists to prevent,
// so this one is held to the rule by TestMatureFilterSQL_MatchesGo,
// which drives every (mature × signed-in × opted-in × instance × owner ×
// admin) combination through both and fails on the first disagreement.
// If you edit [MatureItemVisible], that test tells you to edit this.
//
// `ownerCol` is the row's owner column, and it is a PARAMETER because
// the two tables disagree: an asset's is `owner_user_ref` and a post's
// is `author_user_ref`. Hardcoding either would have produced a
// conjunct that silently referenced a column the other table does not
// have — a runtime error on one surface, found by whoever wired it
// second. `ownerArg` is the placeholder holding the caller's user_ref,
// so the exemption is evaluated per row rather than resolved in Go. It
// takes the same `NULLIF(..., 0)` guard the content rule uses: an
// anonymous caller carries ref 0, and without it a row owned by ref 0
// would match an anonymous caller AS ITS OWNER.
//
// A QUALIFIED viewer, and an admin, get the empty string — no conjunct
// at all rather than a constant TRUE, so Postgres never sees a filter it
// has to reason about on the common path.
func MatureFilterSQL(alias, ownerCol, ownerArg string, v MatureViewer, isSystemAdmin bool) string {
	if isSystemAdmin || QualifiesForMature(v) {
		return ""
	}
	p := columnPrefix(alias)
	return ` AND (NOT ` + p + `mature OR ` + p + ownerCol + ` = NULLIF(` + ownerArg + `::BIGINT, 0))`
}

// The owner columns the two mature-bearing tables use. Named constants
// rather than string literals at the call sites, because "which column
// holds the owner" is exactly the kind of fact that gets transcribed
// wrong once and then copied.
const (
	MatureOwnerColAsset = "owner_user_ref"
	MatureOwnerColPost  = "author_user_ref"
)

// ---------------------------------------------------------------------------
// Resolving a viewer (#1116/#1117)
// ---------------------------------------------------------------------------

// MatureResolver turns a caller into the three-conjunct answer above.
//
// # Why this is an INTERFACE here rather than a struct somewhere
//
// [MatureViewer]'s three fields come from three different places — the
// session, `user_preferences`, and `system_config` — and only the first
// is already in scope wherever a predicate is composed. Something has
// to read the other two, and that something needs a pool and a config
// store, which this package deliberately does not have.
//
// So the CONTRACT lives here, beside the rule it feeds, and the
// implementation lives at the HTTP edge where both stores already are
// (see `http.matureResolverAdapter`). Every consuming package holds
// this one type instead of declaring its own, which is the difference
// between one spelling of "who qualifies" and six.
//
// # It is called ONCE PER REQUEST, never per row
//
// The whole reason [MatureViewer] is a struct of resolved booleans
// rather than a closure is that the answer is stable for the life of a
// request and has to reach a SQL fragment and a cache key. A resolver
// consulted per row would be a preferences lookup per row.
type MatureResolver interface {
	// ResolveMature answers for this caller. Implementations must NOT
	// return a widened viewer on error — see [ResolveMatureOr].
	ResolveMature(ctx context.Context, caller Caller) (MatureViewer, error)
}

// ResolveMatureOr is the seam every call site should use, and it exists
// so that "what happens when the lookup fails" is answered once.
//
// ⚠️ IT FAILS CLOSED, and that is the opposite of the sibling decision
// in `posts.showRestricted` — deliberately, because the two gates
// protect different things.
//
// `showRestricted` degrades to the build's DEFAULT FEED, and the
// argument there is explicitly that "it leaks nothing either way: the
// redaction that protects the content already happened". That argument
// does not transfer. This gate IS the redaction. A viewer resolved
// permissively on a failed preferences read is a viewer who is shown
// mature content they never opted into — the exact outcome the whole
// axis exists to prevent — and unlike a shortened feed, it cannot be
// taken back once it has been rendered.
//
// A nil resolver resolves the same way, and that case is real rather
// than defensive: it is the boot-order and test-construction path, and
// a handler wired without its resolver must refuse to widen rather than
// silently serve every reader the unfiltered library.
//
// The cost of failing closed is bounded and symmetric: an opted-in
// reader briefly sees the same library an opted-out reader sees. The
// cost of failing open is unbounded.
func ResolveMatureOr(ctx context.Context, r MatureResolver, caller Caller) MatureViewer {
	if r == nil {
		return AnonymousMatureViewer
	}
	v, err := r.ResolveMature(ctx, caller)
	if err != nil {
		return AnonymousMatureViewer
	}
	return v
}

// CacheKey is the component every cache keyed on a request's inputs
// must include (#1117).
//
// `search.keyForQuery` states the rule this satisfies: "every input the
// Engine READS is a component of the key". Without this, an opted-in
// reader's cached result page is served verbatim to an opted-out one —
// a leak that is invisible in every single-caller test, because it
// needs two callers and a warm cache to appear at all.
//
// Three characters, one per conjunct, so the string is fixed-width and
// a missing conjunct cannot alias with a present one.
func (v MatureViewer) CacheKey() string {
	b := []byte("---")
	if v.SignedIn {
		b[0] = 'i'
	}
	if v.OptedIn {
		b[1] = 'o'
	}
	if v.InstanceAllows {
		b[2] = 'a'
	}
	return string(b)
}
