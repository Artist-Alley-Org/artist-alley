// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Selection is a set of facet-value constraints — the thing a caller
// sends back after reading the counts (#907).
//
// It lives in THIS package rather than in `search` because a selection
// and a count are two halves of one contract: a bucket says "42 rows
// carry this value", and ticking it must return those 42 rows. Both
// halves are rendered from this type, against the same population, so
// the number and the result set cannot drift apart by being written
// twice. The `search` Engine and the aggregators below are its two
// consumers.
//
// EXTENSIBILITY IS THE POINT. #910 (search inside a collection) is this
// mechanism with a `collection` dimension, and the prior art is
// decisive: Immich exposes container membership as an ordinary filter
// predicate (`albumIds`), while ResourceSpace spends a parser on a
// `!collection<id>` special to reach the same place. We already have
// the better half — a typed field:value grammar with a parse-time
// whitelist — so a new dimension is:
//
//  1. one FacetType const + one [ParseFacetType] case;
//  2. one case in [Selection.dimensionSQL] per entity that supports it;
//  3. optionally one [Aggregator] if it should also carry counts.
//
// No new wire parameter, no new handler branch, no `!bang` syntax.
//
// #910 SHIPPED AND THE CLAIM MOSTLY HELD — with two exceptions, recorded
// here because the next dimension will hit whichever of them applies to
// it, and a prediction is only useful if its misses are written down:
//
//   - A dimension whose value is a TYPE needs validating. Steps 1-3 all
//     assume the value is opaque text, which is true for a tag and an
//     extension and false for a UUID: an unparseable one reaches a
//     `::UUID` cast and raises a Postgres error mid-query. See
//     [FacetType.canonicalValue] — one function, same file, no new
//     concept on the wire.
//   - A dimension whose value NAMES ANOTHER ENTITY needs authorizing,
//     and the renderer cannot do it. dimensionSQL is caller-blind by
//     design (it takes an entity, a dimension, an alias and one
//     placeholder index) and every term renders exactly one placeholder,
//     so there is nowhere to put the caller's identity without changing
//     the arity for every dimension. It is therefore a separate step at
//     the two execution chokepoints — see [Selection.Authorize].
//
// Both are properties of the VALUE's type rather than of the mechanism,
// and neither needed a second query path, a bespoke parameter, or a
// change to the wire vocabulary.
type Selection struct {
	// terms is kept in insertion order and deduplicated by
	// (Type, Value). Small by construction — a rail with more than a
	// handful of ticks is a saved search, not a click.
	terms []Term
}

// Term is one facet-value constraint: "this dimension has this value".
type Term struct {
	Type  FacetType
	Value string
}

// ErrBadFilter is returned by [ParseSelection] for a malformed
// `filter=` parameter. The handler maps it to 400.
var ErrBadFilter = errors.New("facet: filter must be <dimension>:<value>")

// ParseSelection reads the repeated `filter=<dimension>:<value>` query
// parameter into a Selection.
//
// THE WIRE SHAPE, and why it is this one. Three candidates were on the
// table (#907):
//
//   - Reuse `dsl=`. The DSL already owns a field whitelist, so the
//     dimensions would come for free — but the frontend would have to
//     splice UI state into a hand-written query string, re-quoting
//     values that contain a space or a colon, and `dsl=` currently
//     REPLACES the free-text query rather than composing with it. A
//     tick on a rail is not an edit to the user's typed query and
//     should not overwrite it.
//   - One parameter per dimension (`?tag=&extension=`). Adding
//     `collection:` then means a new parameter on two endpoints, a new
//     branch in two handlers, and a new field on the frontend's state —
//     the opposite of the one-line test the issue sets.
//   - ONE repeated parameter carrying `dimension:value`. Orthogonal to
//     `q=` and `dsl=` (all three compose), values are URL-encoded so
//     there is no quoting grammar to get wrong, the token is exactly
//     the (facet key, bucket value) pair the counts endpoint just
//     handed the client, and a new dimension adds nothing here at all.
//
// The third. Unknown dimensions are REJECTED rather than ignored: a
// silently dropped filter renders a result set that looks narrowed and
// is not, which is the failure this whole issue is about.
func ParseSelection(raw []string) (Selection, error) {
	var s Selection
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		dim, value, found := strings.Cut(r, ":")
		if !found {
			return Selection{}, ErrBadFilter
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return Selection{}, ErrBadFilter
		}
		ft, ok := ParseFacetType(strings.TrimSpace(strings.ToLower(dim)))
		if !ok {
			return Selection{}, ErrBadFilter
		}
		value, ok = ft.canonicalValue(value)
		if !ok {
			return Selection{}, ErrBadFilter
		}
		s = s.With(ft, value)
	}
	return s, nil
}

// With returns a copy of s carrying one more term. Duplicate
// (type, value) pairs collapse so a double-tick can't square the work.
func (s Selection) With(t FacetType, value string) Selection {
	for _, existing := range s.terms {
		if existing.Type == t && existing.Value == value {
			return s
		}
	}
	out := Selection{terms: make([]Term, len(s.terms), len(s.terms)+1)}
	copy(out.terms, s.terms)
	out.terms = append(out.terms, Term{Type: t, Value: value})
	return out
}

// Empty reports whether the selection constrains nothing.
func (s Selection) Empty() bool { return len(s.terms) == 0 }

// Terms returns the constraints in insertion order. The slice is a copy;
// callers may not mutate the Selection through it.
func (s Selection) Terms() []Term {
	out := make([]Term, len(s.terms))
	copy(out, s.terms)
	return out
}

// Params renders the selection back to its wire form, one string per
// `filter=` parameter. Used by save-as-collection and by tests that
// round-trip a selection through the HTTP surface.
func (s Selection) Params() []string {
	out := make([]string, 0, len(s.terms))
	for _, t := range s.terms {
		out = append(out, string(t.Type)+":"+t.Value)
	}
	return out
}

// CacheKey is the stable string a per-caller cache folds in beside the
// query text (#907).
//
// A cache key is a claim that two requests deserve the same bytes.
// Filters were absent from search's key only because the Engine ignored
// them; the moment they are real, two different selections on the same
// `q` from the same caller are two different result sets, and leaving
// them out serves one to the other for the rest of the TTL. Unlike the
// capability components beside it this is not a revoke-direction
// argument — it is plain correctness, and it fails in BOTH directions.
//
// Sorted, so a rail rendered in a different order is still a cache hit,
// and length-prefixed per term so ("tag","a:b") and ("tag:a","b") — both
// reachable, since a bucket value may contain a colon — cannot collide.
func (s Selection) CacheKey() string {
	if len(s.terms) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.terms))
	for _, t := range s.terms {
		parts = append(parts, string(t.Type)+"\x1f"+t.Value)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1e")
}

// conjunctive reports whether repeated values in this dimension mean
// AND rather than OR.
//
// Only `tag` is conjunctive, and it is the only dimension where AND is
// satisfiable: an asset has exactly one extension, one sensitivity, one
// type and one owner, so requiring two of them returns nothing forever.
// A tag is the one multi-valued dimension, and the DSL already fixes its
// meaning — `tag:a tag:b` documents "carries EVERY tag" — so the rail
// and the typed query say the same thing.
//
// ⭐ [FacetAI] was checked against that argument rather than inheriting
// the default, because it is the first dimension added since the rule
// was written and #1242 is entirely a plural rule. A post has exactly
// ONE purity state, so it lands on the same side as extension and
// sensitivity: `ai:pure ai:not_pure` under AND returns nothing forever,
// which is a filter that looks applied and is not — the failure this
// whole file exists to prevent. Under OR it returns everything, which is
// the honest reading of "show me pure work or non-pure work" and is
// exactly what the caller asked for, since the two values PARTITION the
// corpus. Non-conjunctive, deliberately.
func (t FacetType) conjunctive() bool { return t == FacetTag }

// canonicalValue validates a value for dimension t and returns the form
// the rest of the pipeline should carry.
//
// FIVE OF THE SIX DIMENSIONS HAVE NO USE FOR THIS, and that is worth
// saying plainly: `extension:!!!` is a well-formed filter that matches
// nothing, because the expression it renders is a TEXT comparison and
// every string is a legal input to it. #910's `collection` is the first
// dimension whose value is a TYPE — a UUID, spliced into a `::UUID`
// cast — so a malformed one is not "matches nothing", it is a Postgres
// 22P02 raised mid-query, i.e. a 500 on a caller mistake.
//
// So the rule that made unknown DIMENSIONS a 400 now extends one level
// down to values, for the dimensions that have a value grammar at all.
// The reason is the same one [ParseSelection] gives: the alternative is
// a filter that looks applied and is not.
//
// Canonicalising (rather than merely accepting) matters because
// google/uuid takes several spellings — braced, hyphenless, urn: — and
// Postgres takes a different subset of them. Normalising here means one
// spelling reaches the SQL, and `{X}` and `X` share a [CacheKey] instead
// of paying for the same query twice.
func (t FacetType) canonicalValue(v string) (string, bool) {
	switch t {
	case FacetCollection:
		id, err := uuid.Parse(v)
		if err != nil {
			return "", false
		}
		return id.String(), true
	case FacetAI:
		// #1242 — the THIRD dimension with a value grammar, and the
		// first whose vocabulary is a CLOSED SET of two.
		//
		// Rejecting an unknown value matters more here than for the
		// opaque-text dimensions, and for a different reason than
		// `collection:`. There is no `::UUID` cast to raise a 22P02;
		// what a tolerated `ai:generated` or `ai:false` would do is
		// render a predicate matching nothing, and a caller who asked to
		// hide AI work would get an EMPTY page rather than an unfiltered
		// one. Failing at the parser turns that into a 400 the client
		// can see.
		switch v = strings.ToLower(strings.TrimSpace(v)); v {
		case AIPure, AINotPure:
			return v, true
		}
		return "", false
	case FacetField:
		// #1157/#1165 — `<code><op><value>`. The SECOND dimension with a
		// value grammar, the first whose value is compound, and now the
		// first that carries an OPERATOR.
		//
		// The separator characters are drawn from OUTSIDE the field-code
		// slug alphabet ([validFieldCode]) on purpose, which is what lets
		// the code and the operator be found by scanning rather than by a
		// second delimiter: no code can contain `=`, `~`, `>` or `<`, so
		// the first occurrence of one of them is always the operator and
		// never part of the code. A value that contains one is harmless
		// for the same reason the original `=` split was — only the FIRST
		// occurrence separates.
		//
		// `:` remains unusable as an operator character because
		// [ParseSelection] cuts the wire token at the first colon, so a
		// nested colon would be ambiguous with the dimension separator.
		// That is why a date value keeps its colons intact (they fall in
		// the value half, after the operator) but no operator may use one.
		code, op, value, ok := SplitFieldTerm(v)
		if !ok {
			return "", false
		}
		// The bound operators name a value whose TYPE is a timestamp, and
		// an unvalidated one reaches a `::TIMESTAMPTZ` cast mid-query —
		// the same 22P02-shaped 500 [FacetCollection]'s UUID validation
		// exists to prevent, and the reason this switch is where a
		// dimension's value grammar lives at all.
		if op == FieldOpAtLeast || op == FieldOpAtMost {
			bound, ok := canonicalBound(value, op)
			if !ok {
				return "", false
			}
			value = bound
		}
		return code + string(op) + value, true
	}
	return v, true
}

// FieldOp is the comparison a [FacetField] term applies between a field
// and its value (#1165).
//
// # Why an operator lives in the SHARED grammar rather than on the page
//
// The advanced page needs "title contains foo" and "licence expires
// between two dates", and neither is expressible by equality. The
// alternative to putting them here was a page-side widget that compiles
// down to something else — which is the second query language ADR 0093
// and #1067 forbid, because the rail, the DSL, a saved search and a
// federated peer would then each have to learn the translation
// separately and could disagree about what the user asked for.
//
// So an operator is one more piece of the VALUE grammar, exactly as the
// field code is. Nothing new appears on the wire: `filter=field:…` still
// takes one repeated parameter carrying one dimension and one value, it
// still round-trips through [Selection.Params], it still folds into
// [Selection.CacheKey] as opaque text, and a peer that echoes the token
// back gets the same predicate we would have run.
//
// # The set is CLOSED, and unknown operators fail closed
//
// [SplitFieldTerm] matches against this list and rejects anything else,
// which makes [ParseSelection] answer 400 rather than guessing. That
// direction is deliberate and is the property #1165 asks for by name: a
// filter that silently degraded to equality — or worse, dropped its
// predicate and matched everything — renders a result set that LOOKS
// narrowed and is not, which is the whole defect the `filter=` parameter
// was introduced to fix.
type FieldOp string

const (
	// FieldOpEq is equality, the original and still the only operator a
	// vocabulary field uses. Matches value_text OR a member of
	// value_options — see [dimensionSQL].
	FieldOpEq FieldOp = "="
	// FieldOpContains is a case-insensitive substring match, for `text`,
	// `longtext` and `rich_text` fields.
	FieldOpContains FieldOp = "~"
	// FieldOpAtLeast and FieldOpAtMost are the inclusive bounds of a
	// date range, against value_date. Two terms rather than one
	// two-ended token, so an open-ended range ("expiring after March",
	// with no upper bound) needs no separate spelling — see
	// [Selection.SQL] for why two bounds on one field AND together
	// where two values of one vocabulary field OR.
	FieldOpAtLeast FieldOp = ">="
	FieldOpAtMost  FieldOp = "<="
)

// fieldOps is the match order for [SplitFieldTerm]: LONGEST FIRST, so
// `>=` is never mistaken for a bare `>` followed by a value beginning
// `=`. A bare `>` or `<` matches nothing here and is therefore rejected,
// which is the intended reading — we define inclusive bounds only, and
// silently treating `>` as `>=` would be an off-by-one the caller cannot
// see.
var fieldOps = []FieldOp{FieldOpAtLeast, FieldOpAtMost, FieldOpContains, FieldOpEq}

// fieldOpChars is every character that may START an operator. None of
// them is legal in a field code ([validFieldCode]), which is the
// invariant that makes scanning for the first one a correct split.
const fieldOpChars = "=~<>"

// canonicalBound validates and normalises a [FieldOpAtLeast] /
// [FieldOpAtMost] value to RFC3339 UTC.
//
// # Why it canonicalises rather than merely accepting
//
// Two reasons, and the second is the load-bearing one.
//
//  1. A [CacheKey] is text, so `2026-01-31` and `2026-01-31T00:00:00Z`
//     would pay for the same query twice.
//  2. FEDERATION. `value_date` is TIMESTAMPTZ, so a bare `2026-01-31`
//     has no meaning until something supplies a zone — and the thing
//     that would supply it is whichever server happens to evaluate the
//     token. A filter that returns different rows on a peer than on its
//     origin is not one grammar. Normalising to an explicit `Z` here
//     means the instant is decided once, by the parser, and travels with
//     the token.
//
// # The upper bound of a date-only value is the END of that day
//
// A caller who writes `<=2026-01-31` means "through the 31st", not
// "through the first instant of the 31st". Reading it literally would
// silently drop 23 hours and 59 minutes of matches — an off-by-one that
// looks like missing data rather than like a bug. So a date-only upper
// bound canonicalises to the last microsecond of that day, and the
// canonical form is what the URL then carries, so what will run is
// visible rather than implied.
//
// Microseconds, not nanoseconds: TIMESTAMPTZ resolves to microseconds,
// and a `.999999999` bound would round UP to the next second on the way
// into Postgres — re-introducing on the storage side exactly the
// off-by-one this is here to remove.
func canonicalBound(v string, op FieldOp) (string, bool) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC().Format(time.RFC3339Nano), true
	}
	if d, err := time.Parse("2006-01-02", v); err == nil {
		if op == FieldOpAtMost {
			d = d.Add(24*time.Hour - time.Microsecond)
		}
		return d.UTC().Format(time.RFC3339Nano), true
	}
	return "", false
}

// validFieldCode reports whether s is a well-formed field-definition
// code: the slug shape `[a-z0-9_-]+`.
//
// It is a SHAPE check, not an existence check, and the difference
// matters. Rejecting an unknown code here would turn this dimension
// into an existence oracle for field definitions — the caller supplies
// the string, and a 400 would separate "no such field" from "a field
// you may not read". A well-formed code for a field that does not
// exist (or that this caller may not read) matches no rows, which is
// the same answer for both and the fail-closed direction the rest of
// the file takes.
func validFieldCode(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return s != ""
}

// NamesFieldDimension reports whether the selection carries a
// [FacetField] term — i.e. whether its result depends on a capability
// from an OPEN set that no cache key can enumerate (#1157).
//
// Every other dimension's access story is fully described by the
// components already folded into the search cache key: the caller ref,
// the three resolved capability value types, the mature axis and the
// selection itself. `field:` is the first one that is not, because
// `field_definition.read_capability` holds a code an operator typed at
// runtime.
//
// The search cache is consulted BEFORE Engine.Run, so [Selection.Authorize]
// cannot defend a cache hit: a caller who ran a gated field filter while
// holding its capability would keep being served that narrowed page for
// the rest of the TTL after the capability was revoked — the exact
// revoke-direction failure [Selection.CacheKey]'s doc warns about, and
// the one that matters, since it serves MORE than the caller is owed.
func (s Selection) NamesFieldDimension() bool {
	for _, t := range s.terms {
		if t.Type == FacetField {
			return true
		}
	}
	return false
}

// SplitFieldTerm splits a [FacetField] value into its code, its
// operator and its value (#1157, operator added #1165).
//
// # The split is a SCAN, not a cut on a fixed delimiter
//
// It was `strings.Cut(v, "=")` while equality was the only operator.
// With four, the delimiter is no longer known in advance, so this finds
// the first character from [fieldOpChars] — none of which
// [validFieldCode] admits into a code — and matches the operator at that
// position, longest first. Everything before is the code; everything
// after the operator is the value, including any further operator
// characters it happens to contain.
//
// # It rejects rather than falling back, and that is the point
//
// #1165 asks for an unknown or malformed operator to fail CLOSED at
// parse time. Every failure path here returns ok=false, which
// [FacetType.canonicalValue] turns into a 400 out of [ParseSelection]
// and, for the programmatic callers that bypass it, into
// "this entity matches nothing" out of [Selection.SQL]. Neither path can
// reach a query. Degrading to equality — the tempting alternative, since
// equality is what every existing term uses — would answer a DIFFERENT
// question than the caller asked and look like it had answered theirs.
//
// Exported because [Selection.Authorize] needs the code to look the
// field up and [Selection.SQL] needs the operator to pick a predicate.
//
// ⚠️ Its doc used to claim the frontend's chip labels were a caller too.
// They are not and never were — the web client builds these tokens but
// has never parsed one back (#1165 verified: this function's only caller
// in the tree is Authorize). Kept exported for Authorize alone.
func SplitFieldTerm(v string) (code string, op FieldOp, value string, ok bool) {
	i := strings.IndexAny(v, fieldOpChars)
	if i < 0 {
		return "", "", "", false
	}
	code = strings.ToLower(strings.TrimSpace(v[:i]))
	rest := v[i:]
	for _, candidate := range fieldOps {
		if !strings.HasPrefix(rest, string(candidate)) {
			continue
		}
		op = candidate
		value = strings.TrimSpace(rest[len(candidate):])
		break
	}
	if op == "" || value == "" || !validFieldCode(code) {
		return "", "", "", false
	}
	return code, op, value, true
}

// ForFacet returns the subset of the selection that should be applied
// when COUNTING dimension t.
//
// This is the standard faceted-search rule, and it falls out of the
// semantics above rather than being a separate policy: apply every
// predicate that would still apply if the user were about to add
// another value in t.
//
//   - For an OR dimension that means dropping t's own terms. Otherwise
//     picking `extension: png` collapses the extension facet to a single
//     bucket and there is no way back to `jpg` except clearing.
//   - For the conjunctive dimension (tag) it means KEEPING them, because
//     the next tick narrows further: the tag facet under `tag:a` should
//     show what co-occurs with a, with the counts a second tick will
//     actually return.
//
// With nothing else selected, ForFacet is empty for every dimension, so
// a bucket's count is exactly what ticking that bucket returns — the
// invariant #907 exists to establish.
func (s Selection) ForFacet(t FacetType) Selection {
	if t.conjunctive() {
		return s
	}
	out := Selection{}
	for _, term := range s.terms {
		if term.Type == t {
			continue
		}
		out.terms = append(out.terms, term)
	}
	return out
}

// Authorize checks the dimensions whose value NAMES ANOTHER ENTITY
// against that entity's own read rule. Returns false when the caller may
// not see one of them; the caller then returns an EMPTY result set.
//
// # Why a filter needs an authorization step at all (#910)
//
// The other five dimensions describe the ROW: `extension:png` is a
// property of the asset, and the asset's own predicate is the whole
// access-control story. `collection:<id>` is not — it names a
// collection, which has an owner, a visibility tier and an ACL table of
// its own. Scoping a search to a collection is a read of that
// collection's membership by another door, and the contents endpoint
// this borrows its membership condition from is explicit that the door
// needs two locks: "The parent gate and the member gate answer
// different questions and both are required"
// (collections/resources_page.go). Without this, a caller holding the
// id of a collection they cannot open — a revoked share, a link that
// outlived its grant — could still enumerate which of the assets they
// can read are curated into it. The member gate would not catch it,
// because every disclosed row IS one they may read; the leak is the
// MEMBERSHIP, which is exactly the thing ADR 0009 §3 keeps separate
// from readability in both directions.
//
// # Why an empty result set and not a 403 or a 404
//
// Because an error would be the oracle the rest of the arc removes: it
// would separate "this collection exists and you may not see it" from
// "no such collection", on an id the caller supplied. An empty page is
// indistinguishable from a real collection the caller may see that
// happens to contain nothing matching, and it is the same fail-closed
// direction [Selection.SQL] takes for an unsatisfiable dimension.
//
// # Which rule, and where it lives
//
// [visibility.CanReadCollection] — the whole read rule, row plane OR
// system.admin, carrying the reasoning for the admin disjunct and for
// what that disjunct assumes about its callers. #910 open-coded the
// disjunct here because GetCollection had open-coded it there; #1059
// moved the one rule beside the plane it composes, so this endpoint and
// the collection page it is rendered on cannot drift into giving an
// admin opposite answers about the same collection.
//
// `caps` is that admin arm. Nil (or any other caller) gets the row
// plane alone.
//
// # Cost
//
// One EXISTS per collection term, NONE at all for a selection that
// names no collection (the loop body never runs, so an ordinary faceted
// search pays nothing for this), and none for a system.admin either —
// CanReadCollection checks the capability before it queries.
func (s Selection) Authorize(
	ctx context.Context,
	pool visibility.Pool,
	caller visibility.Caller,
	caps visibility.CapabilityChecker,
) (bool, error) {
	for _, t := range s.terms {
		switch t.Type {
		case FacetCollection:
			id, err := uuid.Parse(t.Value)
			if err != nil {
				// Cannot arrive via ParseSelection; fail closed for the
				// same reason SQL() does.
				return false, nil
			}
			ok, err := visibility.CanReadCollection(ctx, pool, caller, caps, id)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		case FacetField:
			// #1157 — the FIELD's own read gate.
			//
			// Unlike `collection:`, what needs authorizing here is the
			// DIMENSION rather than the value: `field_definition.
			// read_capability` names a capability without which a caller
			// may not read that field at all, and #907 established that a
			// filter must not answer a question about a column the caller
			// cannot read ("with a narrow enough selection, the filter IS
			// the item"). A `material=steel` filter on a capability-gated
			// field would let a caller without the capability partition
			// the corpus by a value they are refused — one bit at a time,
			// which is the #902 recovery shape on the field plane.
			//
			// Same fail-closed direction and same no-oracle reasoning as
			// the collection arm: refusing yields an EMPTY result set, not
			// a 403, so "this field is gated" and "no such field" are
			// indistinguishable on a code the caller supplied.
			// The OPERATOR is deliberately discarded here. What
			// `read_capability` gates is the FIELD — whether this caller
			// may learn anything about that column at all — and a
			// substring probe or a date bound is exactly as capable of
			// partitioning the corpus one bit at a time as an equality
			// is. Authorizing per operator would be a gate scoped to the
			// principal rather than to the payload.
			code, _, _, ok := SplitFieldTerm(t.Value)
			if !ok {
				return false, nil
			}
			allowed, err := fieldReadable(ctx, pool, caps, code)
			if err != nil {
				return false, err
			}
			if !allowed {
				return false, nil
			}
		}
	}
	return true, nil
}

// fieldReadable answers "may this caller read the field definition with
// this code" — the same question metadata's collection handler asks per
// row (`canReadField`), asked once per selected dimension here.
//
// A field with a NULL read_capability is readable by everyone, which is
// the overwhelmingly common case, so the lookup is one indexed read on
// a small table and only for a selection that names a field at all.
//
// An unknown code returns (false, nil): it cannot be read because it
// does not exist, and reporting that distinctly is the oracle
// [Selection.Authorize]'s doc refuses.
func fieldReadable(
	ctx context.Context, pool visibility.Pool,
	caps visibility.CapabilityChecker, code string,
) (bool, error) {
	var readCap *string
	err := pool.QueryRow(ctx, `
		SELECT read_capability FROM field_definition
		 WHERE code = $1 AND status = 'active' AND searchable = TRUE`,
		code).Scan(&readCap)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if readCap == nil || *readCap == "" {
		return true, nil
	}
	return caps != nil && caps(*readCap), nil
}

// SQL renders the selection as a WHERE-clause suffix for entity e.
//
// Follows the same contract as [visibility.Predicate.ToSQL]: the
// fragment starts with " AND ", placeholders are numbered from
// argOffset+1, and the caller appends the returned args in order. alias
// is the table alias ("" for an un-aliased FROM).
//
// satisfiable is FALSE when this entity has no column for one of the
// selected dimensions — a collection has no file extension, a post has
// no sensitivity tier. The caller must then skip the entity entirely
// rather than render the fragment: "unsupported" means zero rows and
// zero count, not "no constraint". Getting that backwards would widen a
// filtered search to the rows it was asked to exclude.
func (s Selection) SQL(e visibility.EntityType, alias string, argOffset int) (fragment string, args []any, satisfiable bool) {
	if len(s.terms) == 0 {
		return "", nil, true
	}
	a := strings.TrimSpace(alias)
	if a != "" {
		a += "."
	}
	// Group by dimension, preserving first-seen dimension order so the
	// rendered SQL is deterministic for a given selection.
	order := make([]FacetType, 0, len(s.terms))
	byDim := make(map[FacetType][]string, len(s.terms))
	for _, t := range s.terms {
		if _, seen := byDim[t.Type]; !seen {
			order = append(order, t.Type)
		}
		byDim[t.Type] = append(byDim[t.Type], t.Value)
	}

	var b strings.Builder
	idx := argOffset
	for _, dim := range order {
		values := byDim[dim]
		joiner := " OR "
		if dim.conjunctive() {
			joiner = " AND "
		}
		// Sub-groups WITHIN a dimension, in first-seen order. Every
		// dimension but `field:` has exactly one, which is the shape this
		// loop had before #1165 — see subGroupKey.
		subOrder := make([]string, 0, 1)
		bySub := make(map[string][]string, 1)
		for _, v := range values {
			// Second gate on the value grammar. ParseSelection already
			// rejected a malformed one with a 400, but [Selection.With]
			// is exported and takes no error, so a programmatic caller
			// (SelectionFromDSL, a future saved query) can put anything
			// in. Fail CLOSED here rather than letting a bad UUID reach
			// a ::UUID cast: "this entity matches nothing" is a wrong
			// answer a caller can act on, a 22P02 is a 500.
			if _, ok := dim.canonicalValue(v); !ok {
				return "", nil, false
			}
			key, ok := subGroupKey(dim, v)
			if !ok {
				return "", nil, false
			}
			if _, seen := bySub[key]; !seen {
				subOrder = append(subOrder, key)
			}
			bySub[key] = append(bySub[key], v)
		}

		groups := make([]string, 0, len(subOrder))
		for _, key := range subOrder {
			parts := make([]string, 0, len(bySub[key]))
			for _, v := range bySub[key] {
				var op FieldOp
				if dim == FacetField {
					// Re-split rather than threading it out of the
					// grouping pass: canonicalValue has already proven it
					// parses, so this cannot fail, and one parse function
					// with one set of rules is what keeps the grouping
					// and the rendering from disagreeing about where the
					// operator ends.
					_, op, _, _ = SplitFieldTerm(v)
				}
				idx++
				expr, ok := dimensionSQL(e, dim, a, idx, op)
				if !ok {
					return "", nil, false
				}
				parts = append(parts, expr)
				args = append(args, v)
			}
			groups = append(groups, "("+strings.Join(parts, joiner)+")")
		}
		b.WriteString(" AND (" + strings.Join(groups, " AND ") + ")")
	}
	return b.String(), args, true
}

// subGroupKey returns the key that decides which terms of one dimension
// combine with OR and which with AND (#1165).
//
// # Why `field:` needs this and the other dimensions do not
//
// Every other FacetType is ONE dimension: `extension` names one column,
// so two values of it are two answers to one question and OR is the only
// reading that makes sense. `field:` is not one dimension — it is a
// FAMILY of them, one per field definition, collapsed into a single
// FacetType because field definitions are data and cannot be enum
// members ([FacetField]'s doc explains why). Grouping it by FacetType
// alone therefore ORed terms that name DIFFERENT fields, so ticking
// `pipeline_stage=lookdev` on top of `color_space=srgb` WIDENED the
// result set instead of narrowing it.
//
// That was already wrong before #1165 — it is #1157's bug, found while
// adding the operator — but the operator is what makes it unshippable
// rather than merely surprising: the two bounds of a date range are two
// terms on ONE field, and ORed they read "on or after March OR on or
// before June", which is every row that has a date at all. A range would
// have looked like it worked and quietly matched everything, which is
// the exact failure mode #1165 asks the parser to make impossible.
//
// So the key is (code, operator):
//
//   - Same field, same operator → OR. Two values of one vocabulary field
//     is "material is steel or brass", which is what a multi-select row
//     on the advanced page means and what #1157 documented.
//   - Same field, different operator → AND. `>=March` and `<=June` is a
//     range; `~draft` beside `>=March` is "contains draft AND is recent".
//   - Different field → AND. Two rows of a form narrow each other.
//
// Non-field dimensions all return the same constant key, which
// reproduces their previous single-group rendering exactly.
func subGroupKey(dim FacetType, v string) (string, bool) {
	if dim != FacetField {
		return "", true
	}
	code, op, _, ok := SplitFieldTerm(v)
	if !ok {
		return "", false
	}
	return code + "\x1f" + string(op), true
}

// dimensionSQL renders ONE (dimension, entity) pair as a boolean
// expression reading the value from placeholder $idx.
//
// This function is the whole per-entity surface of the filter path: the
// switch below is the "one WHERE clause" a new dimension has to add.
// Falling through returns ok=false, which the caller turns into "this
// entity matches nothing" — the fail-closed direction.
//
// Values arrive as TEXT because that is what a bucket carries on the
// wire, and the opaque-value dimensions (asset_type, owner) accept
// EITHER the bucket's raw value or its human label. The rail sends back
// the ref it was given; a human writing `type:Image` or `owner:alice`
// in the DSL sends the name. One expression serves both so the two
// entry points cannot disagree about what `type:3` means.
// op is the [FieldOp] of a [FacetField] term and is EMPTY for every
// other dimension. It is a parameter rather than something re-derived
// here because [Selection.SQL] has already parsed the value to group by
// it, and parsing the same string twice in two places is how the
// grouping and the predicate would come to disagree.
//
// ⚠️ It does NOT widen the one-placeholder contract this function's
// callers rely on. Each operator renders a DIFFERENT EXPRESSION over the
// SAME single placeholder, still carrying `<code><op><value>`; the split
// still happens in SQL. That is what kept #1165 from having to change
// the arity for every dimension — the objection [Selection.Authorize]'s
// doc records against threading the caller through applies here too.
func dimensionSQL(e visibility.EntityType, dim FacetType, a string, idx int, op FieldOp) (string, bool) {
	p := placeholder(idx)
	switch dim {
	case FacetTag:
		switch e {
		case visibility.EntityAsset:
			return `EXISTS (SELECT 1 FROM asset_tag ft
			                 WHERE ft.asset_id = ` + a + `id AND ft.tag = ` + p + `::TEXT)`, true
		case visibility.EntityPost:
			return `EXISTS (SELECT 1 FROM post_tags ft
			                 WHERE ft.post_id = ` + a + `id AND ft.tag = ` + p + `::TEXT)`, true
		}
	case FacetAssetType:
		if e == visibility.EntityAsset {
			return `(` + a + `asset_type::TEXT = ` + p + `::TEXT
			          OR EXISTS (SELECT 1 FROM asset_types fat
			                      WHERE fat.ref = ` + a + `asset_type
			                        AND LOWER(fat.name) = LOWER(` + p + `::TEXT)))`, true
		}
	case FacetSensitivity:
		if e == visibility.EntityAsset {
			return `LOWER(` + a + `sensitivity) = LOWER(` + p + `::TEXT)`, true
		}
	case FacetOwner:
		if e == visibility.EntityAsset {
			return `(` + a + `owner_user_ref::TEXT = ` + p + `::TEXT
			          OR EXISTS (SELECT 1 FROM "user" fu
			                      WHERE fu.ref = ` + a + `owner_user_ref
			                        AND LOWER(fu.username) = LOWER(` + p + `::TEXT)))`, true
		}
	case FacetExtension:
		if e == visibility.EntityAsset {
			return `LOWER(` + a + `file_extension) = LOWER(` + p + `::TEXT)`, true
		}
	case FacetAI:
		// #1242 — "hide AI work", as an ordinary predicate.
		//
		// Every arm is spelled as `<is this row pure?> = <did the caller
		// ask for pure?>`, and both halves are TOTAL booleans. That is
		// not a stylistic choice: it is what keeps the fails-toward-
		// showing direction out of three-valued logic. The obvious
		// spelling of the asset arm — `ai_provenance <> 'generated'` —
		// evaluates to NULL for an UNDECLARED asset, and a NULL conjunct
		// drops the row, so every work nobody was asked about would
		// vanish from a search that asked to see non-AI work. That is
		// the exact error ADR 0094 §3 and both amendments forbid,
		// arriving through SQL's NULL semantics instead of through a
		// derivation. `IS NOT DISTINCT FROM` and a NOT NULL column are
		// the two ways it is prevented here.
		//
		// The literals below are spliced from the Go constants
		// [AIPure] / [AINotPure], never from caller text; the caller's
		// bytes stay in the placeholder, where [FacetType.canonicalValue]
		// has already confirmed they are one of the two.
		switch e {
		case visibility.EntityPost:
			// The maintained derived fact (migration 00061). NOT NULL,
			// so `= (…)` is total.
			//
			// ⚠️ NOT `ai_provenance`. That column answers "does this post
			// CONTAIN AI?" and reads `generated` for {generated, none},
			// {generated, undeclared} and {generated, assisted} alike —
			// keying the filter on it would exclude precisely the mixed
			// posts the owner's ruling protects.
			return `(` + a + `ai_pure = (` + p + `::TEXT = '` + AIPure + `'))`, true
		case visibility.EntityAsset:
			// The SAME rule over a one-element contributor set, which is
			// what an asset is: it is pure exactly when its own maker
			// declared `generated`. No second column, because there is
			// no set to reduce and therefore nothing a stored value
			// could disagree with.
			return `((` + a + `ai_provenance IS NOT DISTINCT FROM 'generated')
			          = (` + p + `::TEXT = '` + AIPure + `'))`, true
		case visibility.EntityCollection:
			// ⭐ A collection is SATISFIABLE here, and it is the only
			// dimension for which that is true — see runCollections,
			// which had to start applying the rendered fragment for this
			// arm to mean anything.
			//
			// The reason is the direction of the question. Every other
			// dimension is a POSITIVE narrowing: a caller who asks for
			// `extension:png` is asking about files, and a collection
			// dropping out of that page is the answer, not a loss. This
			// one is an EXCLUSION wearing a value's clothes — the caller
			// is saying "not that" — and letting an exclusion silently
			// remove every collection from the page would hide curated
			// human work from someone who asked to see less AI work.
			// That is the fails-toward-showing rule, and it applies to
			// the mechanism as much as to the derivation.
			//
			// So: a collection is never a pure-AI WORK — it is a
			// container, and we derive no purity for it — which makes it
			// a member of `not_pure` and never of `pure`. The
			// placeholder is read (rather than a bare TRUE/FALSE)
			// because every term appends exactly one arg and pgx rejects
			// a statement that does not name it.
			return `(` + p + `::TEXT = '` + AINotPure + `')`, true
		}
	case FacetField:
		// #1157 — one metadata field's value, as an ordinary predicate.
		// #1165 — under one of four operators.
		//
		// ONE PLACEHOLDER, carrying `<code><op><value>`, and the split
		// happens in SQL. That is deliberate rather than lazy: this
		// function's contract is that every term renders exactly one
		// placeholder, and [Selection.SQL] appends exactly one arg per
		// term. Taking two here would change the arity for every
		// dimension — the same objection [Selection.Authorize]'s doc
		// records against threading the caller through. `split_part`
		// takes the code, and `substr(… position(<op> …))` takes
		// everything after the FIRST operator occurrence, so a value
		// containing the operator's own characters survives intact.
		//
		// The operator's spelling is spliced from a CLOSED Go constant
		// ([fieldOps]), never from caller text: `op` reaches here only
		// after [SplitFieldTerm] matched it against that list, so the
		// literal below is one of four strings this file wrote. The
		// caller's bytes stay in the placeholder, where they have always
		// been.
		//
		// `searchable` and `status='active'` are conjuncts, not
		// conveniences: `searchable` is the operator's statement that a
		// field participates in search at all, and the advanced page
		// renders its rows from exactly that set, so the backend has to
		// agree or the page would offer a filter the engine ignores.
		//
		// ⛔ read_capability is NOT here. It is caller-dependent and
		// this function is caller-blind by design — see
		// [Selection.Authorize], which refuses the whole search when a
		// term names a field this caller may not read.
		if e == visibility.EntityAsset {
			match, ok := fieldValueSQL(op, p)
			if !ok {
				return "", false
			}
			return `EXISTS (SELECT 1 FROM asset_field_value ffv
			                 JOIN field_definition ffd ON ffd.id = ffv.field_id
			                WHERE ffv.asset_id = ` + a + `id
			                  AND ffd.code = split_part(` + p + `::TEXT, '` + string(op) + `', 1)
			                  AND ffd.searchable = TRUE
			                  AND ffd.status = 'active'
			                  AND ` + match + `)`, true
		}
		// Posts and collections carry no asset_field_value rows, so a
		// field filter makes them unsatisfiable — zero hits AND zero
		// count, the same fall-through FacetCollection relies on. That
		// is correct rather than lossy: "assets whose material is
		// steel" is a question about assets.
	case FacetCollection:
		// #910 — "search inside this collection", as an ordinary
		// predicate rather than a second query path.
		//
		// The membership condition is COPIED, not invented: it is the
		// one the collection-contents page already runs
		// (collections/resources_page.go, and the sqlc query beside it)
		// — pinned rows whose membership has not expired. An asset the
		// contents page stopped listing an hour ago must not still be
		// reachable by searching inside the collection, and the only way
		// to guarantee that is for both to ask the same question.
		//
		// `pinned` is DEFAULT TRUE on both tables, so this is not a
		// narrowing in practice; it is there because the contents query
		// has it and a divergence between the two would be silent.
		//
		// The placeholder is cast to UUID, not TEXT: `collection_id` is
		// the leading column of each table's primary key and casting the
		// COLUMN instead would give up that index for a sequential scan
		// of every membership row in the install. See
		// [FacetType.canonicalValue] for why the cast is safe.
		//
		// Collections themselves are absent on purpose. A collection has
		// no membership in another collection, so EntityCollection falls
		// through to ok=false and drops out of a scoped search entirely
		// — which is the right answer: "inside collection X" is a
		// question about items, and returning X itself (or its
		// siblings) beside its own contents would be noise.
		switch e {
		case visibility.EntityAsset:
			return `EXISTS (SELECT 1 FROM collection_resources fcr
			                 WHERE fcr.asset_id = ` + a + `id
			                   AND fcr.collection_id = ` + p + `::UUID
			                   AND fcr.pinned = TRUE
			                   AND (fcr.expires_at IS NULL OR fcr.expires_at > NOW()))`, true
		case visibility.EntityPost:
			return `EXISTS (SELECT 1 FROM collection_posts fcp
			                 WHERE fcp.post_id = ` + a + `id
			                   AND fcp.collection_id = ` + p + `::UUID
			                   AND fcp.pinned = TRUE
			                   AND (fcp.expires_at IS NULL OR fcp.expires_at > NOW()))`, true
		}
	}
	return "", false
}

// fieldValueSQL renders the VALUE half of a [FacetField] predicate for
// operator op, reading the term from placeholder p (#1165).
//
// The value is extracted in SQL as everything after the first occurrence
// of the operator, so `v` below is the caller's text and nothing else.
// An unrecognised operator returns ok=false, which [dimensionSQL] turns
// into "this entity matches nothing" — the same fail-closed direction
// [SplitFieldTerm] takes at parse time, repeated here because
// [Selection.With] is exported and can seed a term the parser never saw.
//
// # Which storage column each operator reads, and why it is not all of
// them
//
// A field's storage depends on its type (web/src/lib/fieldOptions.ts
// VALUE_COLUMN): `select` and `tree` write one slug into `value_text`,
// `multi_select` writes an array into `value_options`, and the date
// types write `value_date`.
//
//   - Equality and contains ask BOTH text columns, because a caller
//     ticking "steel" does not know or care which one holds it, and
//     asking both is one expression rather than a type lookup per term.
//   - The bounds ask value_date ALONE. A range against a text field is
//     not a narrower question, it is a meaningless one, and `value_date`
//     is NULL on those rows so it matches nothing — which is the right
//     answer and the fail-closed one. It also keeps the partial index
//     `asset_field_value_date_idx (field_id, value_date)` usable, which
//     an OR across columns would have given up.
//
// # `strpos`, not ILIKE
//
// A substring match spelled `ILIKE '%' || v || '%'` requires escaping
// `%` and `_` out of the caller's text, and an escape that is forgotten
// (or that a later edit drops) turns a search for "50_percent" into a
// wildcard the caller did not ask for. `strpos` has no metacharacters,
// so there is nothing to escape and nothing to forget. Both sides are
// lowered for the case-insensitivity the operator promises.
//
// `value_options` is unnested rather than joined into one string:
// flattening the array would let a match straddle two elements and
// report a hit for text no single value contains.
func fieldValueSQL(op FieldOp, p string) (string, bool) {
	// Everything after the FIRST occurrence of the operator.
	v := `substr(` + p + `::TEXT, position('` + string(op) + `' IN ` + p +
		`::TEXT) + ` + strconv.Itoa(len(op)) + `)`
	switch op {
	case FieldOpEq:
		return `(ffv.value_text = ` + v + ` OR ` + v + ` = ANY(ffv.value_options))`, true
	case FieldOpContains:
		return `(strpos(LOWER(ffv.value_text), LOWER(` + v + `)) > 0
		         OR EXISTS (SELECT 1 FROM unnest(COALESCE(ffv.value_options, '{}'::TEXT[])) fo
		                     WHERE strpos(LOWER(fo), LOWER(` + v + `)) > 0))`, true
	case FieldOpAtLeast:
		return `ffv.value_date >= ` + v + `::TIMESTAMPTZ`, true
	case FieldOpAtMost:
		return `ffv.value_date <= ` + v + `::TIMESTAMPTZ`, true
	}
	return "", false
}

// placeholder renders $N.
func placeholder(n int) string { return "$" + strconv.Itoa(n) }
