// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/viewkind"
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
//     [FacetType.CanonicalValue] — one function, same file, no new
//     concept on the wire.
//   - A dimension whose value NAMES ANOTHER ENTITY needs authorizing,
//     and the renderer cannot do it. Authorizing is one lookup with a
//     WHOLE-QUERY answer, so it is a separate step at the two execution
//     chokepoints — see [Selection.Authorize].
//
// Both are properties of the VALUE's type rather than of the mechanism,
// and neither needed a second query path, a bespoke parameter, or a
// change to the wire vocabulary.
//
// ⛔ THE SECOND BULLET USED TO SAY MORE THAN IT COULD KEEP, and #1251
// is where it stopped being true. It read "dimensionSQL is caller-blind
// by design … so there is nowhere to put the caller's identity without
// changing the arity for every dimension", which conflated two claims: a
// dimension may need the caller for AUTHORIZATION (a whole-query yes/no,
// which Authorize handles) or inside its PREDICATE (a per-row conjunct,
// which nothing handled). [FacetKind] is the second kind: a post matches
// through its members, and a member the caller may not read must
// contribute no kind, inside the correlated EXISTS the renderer builds.
// So dimensionSQL now takes a [RenderContext] and is no longer
// caller-blind.
//
// The arity worry turned out to be avoidable, and the way it was avoided
// is worth copying: the DERIVATION moved instead of the plumbing.
// [viewkind.KindSQL] resolves a row to its kind, so a term is
// `<expression> = $n` — still one placeholder, still one arg — where the
// obvious shape (compile the selected kinds to asset-type refs and
// extension sets, then test membership) needed three bound arrays. If a
// dimension seems to need more placeholders, ask whether it is testing a
// derived value that could be COMPUTED and compared instead.
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
		value, ok = ft.CanonicalValue(value)
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
//
// ⭐ [FacetKind] was checked against it too, and it is the first
// dimension where the two entities give DIFFERENT reasons for the same
// answer. An asset resolves to exactly one kind, so AND is the
// unsatisfiable reading extension and sensitivity already have. A POST
// is a set of members, so `kind:image AND kind:video` IS satisfiable
// there — and still wrong: it would mean "a post holding both", which
// is not what ticking two boxes on a type filter asks for, and it would
// make the same two terms mean different things on two entities.
// Non-conjunctive, deliberately, on both counts.
//
// ⭐ [FacetVisibility] was checked against it too (#1251 slice 2), and it
// is the plainest case since `extension`: a post is in exactly ONE
// sharing tier, so `visibility:public visibility:private` under AND
// returns nothing forever — a filter that looks applied and is not.
// Under OR it is the UNION, which is what the feed's own default has
// been since #1193 ("every shared tier the caller may read") and what
// this dimension has to be able to express for the feed to compose
// through it at all. Non-conjunctive, deliberately.
//
// ⛔ AND NOTE WHICH DIRECTION THE OR RUNS. Widening the tier SET widens
// the SELECTION and never the read rule, which is a separate conjunct
// ANDed on by every site that renders this — see [FacetVisibility].
func (t FacetType) conjunctive() bool { return t == FacetTag }

// orderedDomain is the VALUE DOMAIN of a comparison bound — the fact
// that decides WHICH STORAGE COLUMN an ordered term reads, and the unit
// its text is parsed in (#1173, sprint 18b).
//
// ⛔ IT IS NOT THE OPERATOR. `>=` and `<=` say which SIDE of a bound a
// row must fall on and nothing at all about what kind of quantity is
// being bounded, and until 18b this file conflated the two: dimensionSQL
// selected `value_date` on `op == FieldOpAtLeast || op == FieldOpAtMost`,
// so a bound was a date because it was a bound. That reading has exactly
// one column to offer, which is why `>=` was date-only and why a numeric
// field could not be compared at all.
type orderedDomain uint8

const (
	// domainNone is "this term carries no bound". Equality and contains
	// land here, and so does every dimension whose values are not bounds.
	domainNone orderedDomain = iota
	// domainTemporal is an instant. Canonical form is RFC3339 UTC and
	// the column is TIMESTAMPTZ — see [canonicalBound] for why the zone
	// is decided by the parser rather than by whichever server evaluates
	// the token.
	domainTemporal
	// domainNumeric is a FINITE float64. Canonical form is the shortest
	// decimal that reads back to the same float64, and the column is
	// DOUBLE PRECISION (`asset_field_value.value_num`).
	domainNumeric
	// domainBytes is an EXACT base-10 int64 count of bytes, with no
	// float64 anywhere on the path. The column is BIGINT
	// (`assets.file_size_bytes`), whose range extends past 2^53 where a
	// float64 stops being able to tell consecutive integers apart — so
	// parsing a byte count through a float would silently round a bound
	// a caller can state exactly.
	domainBytes
)

// orderedDomain classifies a DIMENSION as ordered and says which domain
// its bounds live in. domainNone means "not ordered".
//
// # ⛔ THE CLASSIFICATION IS EXPLICIT AND IT IS DELIBERATELY SHORT
//
// This is the parallel of [FacetType.conjunctive] one function above,
// and it is written the same way for the same reason. The tempting 18b
// implementation is to make [Selection.SQL] extract an operator from
// EVERY dimension's values rather than only from `field:`'s — which
// would make `extension:png` operator-aware, give `tag:>=x` a meaning
// nobody asked for, and hand every future dimension a value grammar it
// never declared. So a dimension is ordered only by appearing here.
//
// [FacetFileSize] is the only non-field dimension that qualifies in 18b.
// `field:` is NOT listed: it is a FAMILY of logical dimensions whose
// orderedness is a property of each field definition's declared type,
// answered per term against the database in [Selection.Authorize] and
// carried into the renderer as a [termShape] — see [orderedFieldTypes].
func (t FacetType) orderedDomain() orderedDomain {
	if t == FacetFileSize {
		return domainBytes
	}
	return domainNone
}

// ordered reports whether this dimension's values are BOUNDS carrying a
// comparison operator, rather than plain values.
//
// Derived from [FacetType.orderedDomain] rather than being a second list,
// so the classification and the column choice cannot come to disagree.
func (t FacetType) ordered() bool { return t.orderedDomain() != domainNone }

// CanonicalValue validates a value for dimension t and returns the form
// the rest of the pipeline should carry.
//
// ⭐ EXPORTED BY #1251 SLICE 3, and the reason is worth stating because
// it decides where the NEXT wire parameter validates. A surface that
// takes one dimension as a query parameter of its own — `?ai=` on the
// browse feed, and whatever follows it — has to reject an out-of-
// vocabulary value BEFORE it reaches [Selection.With], which is
// error-free by design. Doing that with a local `switch` over the
// exported value constants would be a SECOND copy of the value grammar,
// which is the shape ADR 0093 decision 3 exists to refuse: two
// implementations that agree today with nothing asserting they must
// keep agreeing. So the feed calls this, and `filter=` calls this, and
// there is one answer to "is this a legal value for this dimension".
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
func (t FacetType) CanonicalValue(v string) (string, bool) {
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
	case FacetKind:
		// #1251 — the FOURTH dimension with a value grammar, and like
		// [FacetAI] its vocabulary is a CLOSED SET: package viewkind's
		// [viewkind.All], which a parity test holds to the frontend's
		// ViewKind union.
		//
		// Rejecting an unknown name here is what makes `filter=kind:junk`
		// a 400 out of [ParseSelection] rather than a predicate that
		// matches nothing. ⚠️ The FEED's `?kind=` parameter answers the
		// same mistake differently — `viewkind.ParseList` DROPS an
		// unrecognised name and reports that a filter was asked for, so
		// `?kind=nonsense` yields an empty page — and the two are not in
		// conflict: `?kind=` is a comma list where one junk term beside a
		// real one must still narrow to the real one, while `filter=` is
		// one dimension and one value, where there is nothing left to
		// narrow to. Both fail CLOSED; neither can widen.
		v = strings.ToLower(strings.TrimSpace(v))
		if !viewkind.Valid(v) {
			return "", false
		}
		return v, true
	case FacetVisibility:
		// #1251 slice 2 — the FIFTH dimension with a value grammar, and
		// like [FacetAI] and [FacetKind] its vocabulary is a CLOSED SET:
		// the five tiers of [VisibilityTiers], which are exactly the
		// `posts_visibility_check` constraint's values.
		//
		// Rejecting an unknown tier is what makes `filter=visibility:junk`
		// a 400 rather than an empty page under a label promising one
		// tier. It is ALSO what keeps a malformed value out of a
		// comparison against a column the read rule reads: this dimension
		// selects among tiers, and the set of tiers it may name is the
		// set the database defines, never caller text that merely looks
		// like one.
		//
		// ⚠️ Case-folded and trimmed, unlike [FacetTag] one arm below.
		// The tiers are an enum this repository authored — `Public` and
		// `public` are the same tier by construction — whereas a tag is
		// user text whose exact bytes ARE the identity (migration 00050).
		// Two dimensions, two answers, both deliberate.
		v = strings.ToLower(strings.TrimSpace(v))
		for _, tier := range VisibilityTiers() {
			if v == tier {
				return v, true
			}
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
			bound, _, ok := canonicalBound(value, op)
			if !ok {
				return "", false
			}
			value = bound
		}
		return code + string(op) + value, true
	case FacetFileSize:
		// #1173 sprint 18b — the SIXTH dimension with a value grammar,
		// and the FIRST whose whole value is a BOUND.
		//
		// ⚠️ THE VALUE SHAPE IS NEW AND IT IS NOT `field:`'s. A field
		// term is compound — `code<op>value` — because `field:` is a
		// family and the term has to say WHICH field. `file_size` names
		// exactly one column, so there is nothing to disambiguate and the
		// value is a BARE BOUND WITH THE OPERATOR LEADING: `>=12345`.
		//
		// The wire spelling is therefore `filter=file_size:>=12345`.
		// [ParseSelection] cuts at the FIRST colon, so the dimension is
		// `file_size` and `>=12345` is the whole value; `file_size>=12345`
		// carries no colon at all and stays malformed, which is a property
		// of the wire form rather than of this dimension.
		//
		// It is rejected here rather than tolerated for [FacetAI]'s
		// reason and [FacetCollection]'s reason at once: a malformed
		// bound would render a predicate that matches nothing (an empty
		// page under a label promising a narrowing) and a non-integral
		// one would reach a `::BIGINT` cast and raise a 22P02 mid-query.
		return canonicalByteBound(v)
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
// [FieldOpAtMost] value, and reports which [orderedDomain] it landed in.
//
// # Why it canonicalises rather than merely accepting
//
// Two reasons, and the second is the load-bearing one.
//
//  1. A [CacheKey] is text, so `2026-01-31` and `2026-01-31T00:00:00Z`
//     would pay for the same query twice — and after 18b so would
//     `1920`, `1920.0` and `1.92e3`.
//  2. FEDERATION. `value_date` is TIMESTAMPTZ, so a bare `2026-01-31`
//     has no meaning until something supplies a zone — and the thing
//     that would supply it is whichever server happens to evaluate the
//     token. A filter that returns different rows on a peer than on its
//     origin is not one grammar. Normalising to an explicit `Z` here
//     means the instant is decided once, by the parser, and travels with
//     the token.
//
// # ⭐ THE TWO SPELLINGS ARE DISJOINT, AND THAT IS WHY NO SCHEMA IS NEEDED
//
// 18b makes a bound either temporal or numeric, decided WITHOUT knowing
// the field's declared type — this function is pure, and it is what
// [FacetType.CanonicalValue] calls, which is what fixes canonical
// identity for the cache key. That only works because no string is both:
//
//   - RFC3339 and `2006-01-02` both require the full punctuated layout,
//     so `2026` is not a year here, it is the number 2026;
//   - `strconv.ParseFloat` rejects every date spelling, because a date
//     carries `-` in the middle of its digits.
//
// So the domain is a function of the VALUE alone. What the schema then
// decides is a different question — whether the FIELD may be compared
// that way at all — and it is answered in [Selection.Authorize], which
// fails CLOSED to an empty result set rather than to a 400. See
// [orderedFieldTypes].
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
//
// ⛔ There is NO equivalent widening for the numeric arm, deliberately.
// `1920` denotes one value of a DOUBLE PRECISION column, not a half-open
// interval of them, so `<=1920` is the literal bound and nothing is
// added to it.
//
// # The numeric canonical form, stated as a property rather than a notation
//
//	Two spellings that denote the same float64 produce the SAME canonical
//	string, and that string reads back to that same float64.
//
// `strconv.FormatFloat(n, 'g', -1, 64)` is the shortest decimal with that
// round-trip property, so the property is what is being relied on and the
// notation is only how it is obtained. ⚠️ NON-FINITE VALUES ARE REJECTED:
// ParseFloat accepts `NaN`, `Inf` and `Infinity` (and returns ±Inf with
// ErrRange for a decimal too large to represent), none of which is a
// bound — `value_num >= 'NaN'` is false for every row including the ones
// that hold NaN, which is a filter that looks applied and is not.
func canonicalBound(v string, op FieldOp) (string, orderedDomain, bool) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC().Format(time.RFC3339Nano), domainTemporal, true
	}
	if d, err := time.Parse("2006-01-02", v); err == nil {
		if op == FieldOpAtMost {
			d = d.Add(24*time.Hour - time.Microsecond)
		}
		return d.UTC().Format(time.RFC3339Nano), domainTemporal, true
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return "", domainNone, false
		}
		return strconv.FormatFloat(n, 'g', -1, 64), domainNumeric, true
	}
	return "", domainNone, false
}

// orderedBoundOps is the operator set an ORDERED DIMENSION's bare value
// may lead with, longest-first for the same reason [fieldOps] is.
//
// It is the two inclusive bounds and nothing else. A bare `>` or `<`
// matches nothing here and is therefore rejected, which is the intended
// reading — we define inclusive bounds only, and silently treating `>`
// as `>=` would be an off-by-one the caller cannot see. Equality and
// contains are absent because they are not orderings: `file_size:=1234`
// is a question nobody asks of a byte count, and admitting it would put
// a fourth spelling of "equals" on the wire.
var orderedBoundOps = []FieldOp{FieldOpAtLeast, FieldOpAtMost}

// SplitOrderedBound splits an ORDERED DIMENSION's value into its leading
// operator and the rest (#1173).
//
// This is the non-field twin of [SplitFieldTerm], and the difference in
// shape is the difference in the dimensions. A `field:` term has to name
// WHICH field, so its operator sits in the middle of a compound value and
// is found by scanning. An ordered dimension names exactly one column, so
// there is nothing before the operator and the split is a prefix match.
//
// Every failure path returns ok=false, which [FacetType.CanonicalValue]
// turns into a 400 out of [ParseSelection] and [Selection.SQL] turns into
// "this entity matches nothing" for the programmatic callers that bypass
// it. Neither can reach a query. Exported for the same reason
// [SplitFieldTerm] is: the grouping pass and the renderer must read the
// operator with ONE function or they will eventually disagree about
// where it ends.
func SplitOrderedBound(v string) (op FieldOp, value string, ok bool) {
	v = strings.TrimSpace(v)
	for _, candidate := range orderedBoundOps {
		if !strings.HasPrefix(v, string(candidate)) {
			continue
		}
		value = strings.TrimSpace(v[len(candidate):])
		if value == "" {
			return "", "", false
		}
		return candidate, value, true
	}
	return "", "", false
}

// canonicalByteBound validates and normalises a [FacetFileSize] value —
// `<op><int64>`, an EXACT count of bytes (#1173).
//
// # ⛔ int64, AND NOT A float64 ANYWHERE ON THE PATH
//
// `assets.file_size_bytes` is BIGINT. Beyond 2^53 a float64 cannot tell
// consecutive integers apart, so a byte count parsed through one would
// come back as a DIFFERENT number than the caller wrote — silently, and
// only for large files, which is precisely where a size filter is used.
// `strconv.ParseInt(_, 10, 64)` is exact over the whole column range and
// reports ErrRange rather than saturating past it.
//
// It also gives the value-domain rejections for free, and each one is a
// caller mistake that would otherwise look like a working filter:
//
//   - `12.5` — a fractional byte does not exist. ParseInt rejects it;
//     a float parse would have silently truncated or rounded it.
//   - `1MB` — units are NOT part of this grammar. There is no unit
//     vocabulary on the wire, so `1MB` can only ever be a typo, and
//     accepting the digits before the letters would run a filter three
//     orders of magnitude away from the one that was asked for.
//   - `9223372036854775808` — past int64. ErrRange.
//   - `0x2000`, `1_000`, `1e3` — base 10 means base 10. ParseInt with an
//     explicit base rejects prefixes, separators and exponents, so one
//     spelling reaches the column.
//
// Canonicalising (rather than merely accepting) is [canonicalBound]'s
// argument applied to integers: `+12345` and `12345` denote one bound and
// must share one [CacheKey] and one stored DSL spelling.
func canonicalByteBound(v string) (string, bool) {
	op, rest, ok := SplitOrderedBound(v)
	if !ok {
		return "", false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", false
	}
	return string(op) + strconv.FormatInt(n, 10), true
}

// orderedFieldTypes maps a `field_definition.type` to the domain its
// ORDERED comparisons live in. A type absent from this table supports NO
// ordered comparison at all (#1173).
//
// # ⛔ THE TWO VALIDITY CLASSES ARE DIFFERENT QUESTIONS WITH DIFFERENT ANSWERS
//
// A malformed bound is knowable without a schema, so it is rejected in
// [FacetType.CanonicalValue] — pure, never reaches execution, and surfaces
// as a 400 on the `filter=` path and a DSLError on the DSL path.
//
// Whether a FIELD may be compared this way is not knowable without the
// schema: it needs `field_definition.type`, which is a row. So it is
// answered in [Selection.Authorize] beside the read-capability lookup
// that is already there, and a refusal is an EMPTY RESULT SET rather than
// an error — the same no-oracle direction the collection and field
// arms take, for the same reason. A 400 would separate "that field is a
// text field" from "no such field" on a code the caller supplied.
//
// # Why these three types and no others
//
// The enum is CHECK-constrained on `field_definition.type` to eleven
// values (migration 00001). The storage column follows the type
// (web/src/lib/fieldOptions.ts VALUE_COLUMN), and only two columns hold
// an ORDERED quantity:
//
//	date, datetime  -> value_date  (TIMESTAMPTZ)
//	number          -> value_num   (DOUBLE PRECISION)
//
// ⚠️ `boolean` also writes `value_num` — ADR 0012 encodes it as 1 or 0 —
// and it is deliberately ABSENT. "At least true" is not a question, and
// admitting it would let a bound act as a clumsy equality on a column
// whose two values already have one.
//
// `text`, `longtext` and `rich_text` are absent because a range over
// prose is meaningless rather than merely narrow; `select`,
// `multi_select` and `tree` because a vocabulary has no order; and
// `reference` because a UUID has no magnitude. There is no INTEGER field
// type at all, which is why exact integral comparison exists only for a
// resource dimension ([FacetFileSize]) and never for `field:`.
var orderedFieldTypes = map[string]orderedDomain{
	"date":     domainTemporal,
	"datetime": domainTemporal,
	"number":   domainNumeric,
}

// fieldTypeAdmitsBound reports whether a field of declared type ftype may
// be compared with op against value.
//
// Non-bound operators are admitted unchanged: `=` and `~` are text
// questions that every field type can be asked, and #1165's answer for a
// type that stores nothing matching is already "no rows".
//
// For a bound, the value's own domain has to be the one that type stores.
// A temporal bound on a `number` field and a numeric bound on a `date`
// field are BOTH refusals — the mismatch is symmetric, and treating
// either as "compare against the other column anyway" would answer a
// different question than the caller asked.
func fieldTypeAdmitsBound(ftype string, op FieldOp, value string) bool {
	if op != FieldOpAtLeast && op != FieldOpAtMost {
		return true
	}
	_, dom, ok := canonicalBound(value, op)
	if !ok {
		return false
	}
	return dom != domainNone && orderedFieldTypes[ftype] == dom
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
// [FacetType.CanonicalValue] turns into a 400 out of [ParseSelection]
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
			//
			// #1173 sprint 18b — AND THE FIELD'S DECLARED TYPE, which is
			// the SECOND thing about a `field:` term that cannot be
			// decided without a row.
			//
			// `>=` and `<=` name a comparison, and whether a field
			// supports one at all is `field_definition.type` — the same
			// lookup already being made here for `read_capability`, so
			// the check costs nothing extra. A refusal is the SAME empty
			// result set for the SAME no-oracle reason: 400 would tell a
			// caller "that code names a text field" about a code they
			// supplied, which is the existence oracle [validFieldCode]
			// refuses one level up. See [orderedFieldTypes].
			code, op, value, ok := SplitFieldTerm(t.Value)
			if !ok {
				return false, nil
			}
			allowed, ftype, err := fieldGate(ctx, pool, caps, code)
			if err != nil {
				return false, err
			}
			if !allowed {
				return false, nil
			}
			if !fieldTypeAdmitsBound(ftype, op, value) {
				return false, nil
			}
		}
	}
	return true, nil
}

// fieldGate reads the two facts about a field definition that only the
// DATABASE knows and that a `field:` term's admissibility depends on:
// may this caller read it, and what TYPE is it declared as.
//
// The readability half is the same question metadata's collection handler
// asks per row (`canReadField`), asked once per selected dimension here.
// A field with a NULL read_capability is readable by everyone, which is
// the overwhelmingly common case, so the lookup is one indexed read on
// a small table and only for a selection that names a field at all.
//
// The TYPE half arrives on the same row rather than in a second query
// (#1173): 18b needs it to decide whether an ordered comparison is
// admissible at all, and asking twice for two columns of one row would
// double the per-term cost of a filter for no gain. ftype is the empty
// string whenever readable is false, so a refusal cannot be mistaken for
// a field of some type.
//
// An unknown code returns (false, "", nil): it cannot be read because it
// does not exist, and reporting that distinctly is the oracle
// [Selection.Authorize]'s doc refuses.
func fieldGate(
	ctx context.Context, pool visibility.Pool,
	caps visibility.CapabilityChecker, code string,
) (readable bool, ftype string, err error) {
	var readCap *string
	err = pool.QueryRow(ctx, `
		SELECT read_capability, type FROM field_definition
		 WHERE code = $1 AND status = 'active' AND searchable = TRUE`,
		code).Scan(&readCap, &ftype)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil
		}
		return false, "", err
	}
	if readCap == nil || *readCap == "" {
		return true, ftype, nil
	}
	if caps != nil && caps(*readCap) {
		return true, ftype, nil
	}
	return false, "", nil
}

// RenderContext carries the caller-dependent inputs a dimension needs
// when its predicate has to decide, IN SQL, whether this caller may see
// the value it is selecting on (#1251).
//
// # Why it exists, when [Selection.Authorize] already handles the caller
//
// Authorize answers questions with a WHOLE-QUERY answer: may you read
// this collection, may you read this field. One lookup, one boolean, and
// a refusal means an empty result set — so it can sit at the execution
// chokepoint and the renderer can stay caller-blind, which is what this
// file used to say it would always be.
//
// [FacetKind] is the first dimension that cannot be served that way. A
// post matches a kind through its MEMBERS, and a member the caller may
// not read must contribute no kind — otherwise asking for each kind in
// turn recovers the withheld one by elimination (#902/#1066, and
// posts.TestKindFilter_RestrictedMemberIsNeverProbeable is the assertion
// that pins it). That rule is a conjunct on each candidate member, INSIDE
// the correlated EXISTS the renderer builds, and there is nowhere else to
// put it: hoisting it up to the post is precisely the implementation the
// leak test exists to fail.
//
// # ⚠️ THE ZERO VALUE MUST NOT BE USABLE, and it is not
//
// [visibility.Caller]'s zero value is `{UserRef: 0, IsAnonymous: false}`
// — "user zero", which is WIDER than anonymous, because the anonymous
// branch of the field plane adds status conjuncts that this one skips. A
// dimension that read a forgotten RenderContext would therefore fail
// OPEN. So a dimension that needs one requires [RenderContext.CallerArg]
// to be non-empty and returns ok=false without it, which
// [Selection.SQL] turns into "this entity matches nothing" — the
// fail-closed direction the rest of this file takes.
type RenderContext struct {
	// Caller is the reader whose readability is being composed.
	Caller visibility.Caller
	// Caps and MutationCaps are that caller's resolved content-plane and
	// assets.admin capabilities — the same two values every other splice
	// site of [visibility.FieldsReadableSQL] passes.
	Caps         visibility.ContentCaps
	MutationCaps visibility.AssetMutationCaps
	// CallerArg is an ALREADY-BOUND placeholder ("$3") holding the
	// caller's user_ref, with 0 for anonymous — the same contract
	// FieldsReadableSQL's own `callerArg` parameter has, and the reason
	// this is a placeholder rather than an inlined literal is
	// posts.ListPostsPageGated: it is the hottest query in the app and a
	// per-user query TEXT would defeat statement caching for a value that
	// changes nothing about the plan. (The facet aggregators inline it;
	// they run on a colder path.)
	//
	// EMPTY MEANS "no caller was supplied", which is not the same as
	// anonymous and is treated as a programming error — see the type doc.
	CallerArg string
}

// SQL renders the selection as a WHERE-clause suffix for entity e.
//
// Follows the same contract as [visibility.Predicate.ToSQL]: the
// fragment starts with " AND ", placeholders are numbered from
// argOffset+1, and the caller appends the returned args in order. alias
// is the table alias ("" for an un-aliased FROM).
//
// rc carries the caller for the dimensions whose predicate needs one —
// today only [FacetKind], and see [RenderContext] for why that could not
// be handled at the execution chokepoint the way [Selection.Authorize]
// is. Every other dimension ignores it, so a caller-blind site (the
// facet aggregators counting a dimension none of whose values name a
// kind) may pass the zero value and get exactly its previous rendering.
//
// satisfiable is FALSE when this entity has no column for one of the
// selected dimensions — a collection has no file extension, a post has
// no sensitivity tier. The caller must then skip the entity entirely
// rather than render the fragment: "unsupported" means zero rows and
// zero count, not "no constraint". Getting that backwards would widen a
// filtered search to the rows it was asked to exclude.
func (s Selection) SQL(e visibility.EntityType, alias string, argOffset int, rc RenderContext) (fragment string, args []any, satisfiable bool) {
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
			if _, ok := dim.CanonicalValue(v); !ok {
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
				// Re-derive rather than threading it out of the grouping
				// pass: CanonicalValue has already proven the value
				// parses, and one parse function with one set of rules
				// is what keeps the grouping and the rendering from
				// disagreeing about where the operator ends.
				sh, ok := shapeOf(dim, v)
				if !ok {
					return "", nil, false
				}
				idx++
				expr, ok := dimensionSQL(e, dim, a, idx, sh, rc)
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
// combine with OR and which with AND (#1165, extended #1173).
//
// # Why `field:` needs this and most other dimensions do not
//
// Most FacetTypes are ONE dimension with one kind of value: `extension`
// names one column, so two values of it are two answers to one question
// and OR is the only reading that makes sense. `field:` is not one
// dimension — it is a FAMILY of them, one per field definition, collapsed
// into a single FacetType because field definitions are data and cannot
// be enum members ([FacetField]'s doc explains why). Grouping it by
// FacetType alone therefore ORed terms that name DIFFERENT fields, so
// ticking `pipeline_stage=lookdev` on top of `color_space=srgb` WIDENED
// the result set instead of narrowing it.
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
// # ⭐ AN ORDERED DIMENSION NEEDS IT FOR EXACTLY ONE HALF OF THAT REASON
//
// [FacetFileSize] is one dimension over one column, so there is no code
// to key on — but its values are BOUNDS, and the range argument above is
// about the OPERATOR rather than about the family. `file_size:>=A` beside
// `file_size:<=B` ORed reads "bigger than A or smaller than B", which is
// every asset that has a size at all: the range that looks like it worked.
// So the key is the operator alone, which makes two bounds AND (an
// intersection) and two of the SAME bound OR (a value list, the looser
// of the two winning, which is what "at least A or at least B" means).
//
// ⛔ THE CLASSIFICATION IS EXPLICIT, NOT UNIVERSAL. Extracting an operator
// from every dimension's values would make `extension:png` operator-aware
// and give `tag:` a value grammar nobody declared. A dimension is ordered
// only by [FacetType.orderedDomain] naming it.
//
// Every other dimension returns the same constant key, which reproduces
// its previous single-group rendering exactly.
func subGroupKey(dim FacetType, v string) (string, bool) {
	switch {
	case dim == FacetField:
		code, op, _, ok := SplitFieldTerm(v)
		if !ok {
			return "", false
		}
		return code + "\x1f" + string(op), true
	case dim.ordered():
		op, _, ok := SplitOrderedBound(v)
		if !ok {
			return "", false
		}
		return string(op), true
	}
	return "", true
}

// termShape is HOW one term compares: its operator, and — for an ordered
// term — the [orderedDomain] the bound was written in (#1173).
//
// # ⛔ THE DOMAIN IS THE HALF THAT USED TO BE MISSING, AND ITS ABSENCE WAS THE BUG
//
// Before 18b [dimensionSQL] chose the storage column from the OPERATOR:
// `op == FieldOpAtLeast || op == FieldOpAtMost` selected `value_date`.
// That is only correct while dates are the only thing anyone can bound,
// and it is what made `>=` date-only. The column follows the QUANTITY,
// so the quantity has to travel with the term.
//
// It stays a value carried alongside the term rather than a second
// placeholder: [dimensionSQL]'s contract is one placeholder per term, and
// [Selection.SQL] appends exactly one arg. See [dimensionSQL]'s doc.
type termShape struct {
	// op is the comparison. EMPTY for a dimension whose values carry no
	// operator, which is every dimension but `field:` and the ordered
	// ones.
	op FieldOp
	// domain is the bound's value domain, and [domainNone] whenever op
	// is not a bound.
	domain orderedDomain
}

// shapeOf derives the [termShape] of one already-canonical term.
//
// ⚠️ It reads the VALUE, not the schema, and that is exactly the split
// [canonicalBound] documents: which domain a bound was written in is
// decided by its spelling and is knowable here, while whether the FIELD
// admits that domain is a row and is decided in [Selection.Authorize].
// A term that reaches here having failed the schema check renders SQL
// that matches no row — the column it names is NULL on every row of a
// field stored elsewhere — so the two gates fail in the same direction.
//
// Returns ok=false for a value the grammar cannot read, which
// [Selection.SQL] turns into "this entity matches nothing". That is the
// second gate [Selection.With] makes necessary: it is exported and takes
// no error, so a programmatic caller can seed a term the parser never saw.
func shapeOf(dim FacetType, v string) (termShape, bool) {
	switch {
	case dim == FacetField:
		_, op, value, ok := SplitFieldTerm(v)
		if !ok {
			return termShape{}, false
		}
		if op != FieldOpAtLeast && op != FieldOpAtMost {
			return termShape{op: op}, true
		}
		_, dom, ok := canonicalBound(value, op)
		if !ok {
			return termShape{}, false
		}
		return termShape{op: op, domain: dom}, true
	case dim.ordered():
		op, _, ok := SplitOrderedBound(v)
		if !ok {
			return termShape{}, false
		}
		return termShape{op: op, domain: dim.orderedDomain()}, true
	}
	return termShape{}, true
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
//
// rc is the caller context [FacetKind]'s post arm needs and every other
// dimension ignores; see [RenderContext].
//
// sh is the [termShape] of this term — its operator, and the domain its
// bound was written in. Both are the ZERO VALUE for a dimension whose
// values carry no operator, which is every dimension but `field:` and the
// ordered ones. They are parameters rather than something re-derived here
// because [Selection.SQL] has already parsed the value to group by it,
// and parsing the same string twice in two places is how the grouping and
// the predicate would come to disagree.
//
// ⚠️ It does NOT widen the one-placeholder contract this function's
// callers rely on. Each operator renders a DIFFERENT EXPRESSION over the
// SAME single placeholder, still carrying the whole term; the split still
// happens in SQL. That is what kept #1165 from having to change the arity
// for every dimension, #1251 held the same line for [FacetKind] by moving
// the DERIVATION into SQL rather than binding the compiled sets — see
// [Selection]'s doc — and #1173's ordered dimensions hold it again by
// carrying the DOMAIN, which is one enum value per term rather than a
// second bound argument. `rc` is not a counter-example either: it is one
// value for the whole selection, not a per-term placeholder.
func dimensionSQL(e visibility.EntityType, dim FacetType, a string, idx int, sh termShape, rc RenderContext) (string, bool) {
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
	case FacetVisibility:
		// #1251 slice 2 — the sharing tier, as an ordinary predicate.
		//
		// ⛔ THIS IS THE ONE DIMENSION WHOSE COLUMN A READ RULE ALSO
		// READS, and the expression below is deliberately the whole of
		// what it does. It compares a tier to a bound value. It grants
		// nothing, waives nothing and consults no relationship table,
		// because every caller of [Selection.SQL] ANDs the entity's read
		// rule on after this fragment — so the set this narrows is
		// already the set the caller may read, and a tier named here can
		// only ever remove rows from it. See [FacetVisibility] for why
		// that ordering is the safety argument rather than a detail.
		//
		// A plain `=` and no LOWER(), unlike [FacetSensitivity] beside
		// it: [FacetType.CanonicalValue] has already folded the value to
		// one of five literals this package wrote, so lowering the COLUMN
		// would buy nothing and would give up
		// `collections_visibility_idx` on the collection arm.
		switch e {
		case visibility.EntityPost:
			return a + `visibility = ` + p + `::TEXT`, true
		case visibility.EntityCollection:
			// A collection carries the SAME five-tier column with the
			// same CHECK constraint, so it answers this question
			// honestly and stays on a tier-filtered page. Its own rule
			// (visibility.CollectionReadableSQL, spliced by
			// search.runCollections) is what decides whether the row was
			// readable in the first place — this only narrows within it.
			return a + `visibility = ` + p + `::TEXT`, true
		}
		// An ASSET falls through to ok=false and drops out of a
		// tier-filtered page entirely. It has no `visibility` column: its
		// axis is `sensitivity`, whose four values are a DIFFERENT
		// vocabulary served by [FacetSensitivity], and answering a tier
		// question with a sensitivity answer would make one token mean
		// two things.
		//
		// ⚠️ This is the FIRST dimension an asset cannot satisfy, which
		// makes [buildAssetPopulationSQL]'s unsatisfiable branch — and
		// the drop of tagAgg's asset half beneath it — reachable for the
		// first time. Both were already written to honour it.
		//
		// The direction is the safe one, by [FacetAI]'s own check: a tier
		// filter is a POSITIVE narrowing, so an entity that cannot answer
		// leaving the page is the answer. `ai:` had to be satisfiable for
		// a collection precisely because it is the opposite — an
		// EXCLUSION wearing a value's clothes.
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
		// bytes stay in the placeholder, where [FacetType.CanonicalValue]
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
	case FacetKind:
		// #1166/#1190, converged onto the grammar by #1251 — the badge
		// kind, as an ordinary predicate.
		//
		// ONE PLACEHOLDER, holding the kind NAME, compared against
		// [viewkind.KindSQL] — the SQL twin of the same resolver the card
		// draws its glyph from. The derivation is transcribed once, in
		// package viewkind beside the Go form and its parity tests, so
		// this file states WHICH ROWS a kind selects and never what a kind
		// IS.
		switch e {
		case visibility.EntityAsset:
			// An asset IS the row, so the question is direct. No
			// readability conjunct here: the asset arm's field plane is
			// applied by the execution site to the whole selection
			// (runAssets / enrichAssetHits both append
			// FieldsReadableSQL under an active filter, for #907's
			// reason), and the row this predicate reads is the row that
			// gate covers.
			return viewkind.KindSQL(strings.TrimSuffix(a, ".")) + ` = ` + p + `::TEXT`, true
		case visibility.EntityPost:
			// ⭐⭐ A POST MATCHES THROUGH ITS MEMBERS, AND THE READABILITY
			// CONJUNCT LIVES PER MEMBER. Both halves are load-bearing and
			// they are the whole reason [RenderContext] exists.
			//
			// ANY MEMBER, NOT THE COVER (#1190). The owner's ruling: "a
			// post containing an ebook matches the ebook filter, cover or
			// not". #1166 shipped the cover-only reading and it answered
			// the wrong question — a five-file art drop whose first image
			// happens to be the cover was unreachable by every kind it
			// actually contains.
			//
			// The membership is `post_assets` and the EXPLICIT COVER IS
			// NOT ADDED BESIDE IT, even though `posts.cover_asset_id` can
			// name a non-member. PostCard resolves its cover as
			// `cover_asset_id ?? members[0]` and then LOOKS IT UP IN
			// `post.members`, so a cover that is not a member yields no
			// coverAsset, no kind badge and no extension band. Selecting a
			// post by a fact its card cannot draw is the disagreement this
			// filter's whole test suite is built to catch.
			//
			// ⛔ AND THE GATE SITS INSIDE THE EXISTS, BESIDE THE MATCH.
			// visibility.FieldsReadableSQL is the SQL twin of the exact Go
			// call posts.enrichPreview makes to decide
			// `PostMember.Restricted`, so "this member matched" and "this
			// member's kind is drawable" are one decision. Dropping it —
			// or hoisting it out to the post — turns the filter into an
			// oracle for a value the card deliberately withholds: a
			// restricted member shows no kind and no extension anywhere on
			// the card, and a filter that could still select the post lets
			// a reader recover that member's kind by asking for each kind
			// in turn. That is the derived-copy defect class of
			// #902/#1066 arriving through a new channel.
			//
			// It cannot widen: it is a conjunct INSIDE an EXISTS that is
			// itself a conjunct, and the whole fragment only ever removes
			// rows.
			//
			// `deleted_at IS NULL` is ListPostAssets' own filter, so the
			// assets considered here are exactly the ones that reach
			// `post.members` — a soft-deleted asset is not a member of
			// anything the reader can see.
			if rc.CallerArg == "" {
				// No caller was supplied. Fail closed rather than render
				// the zero Caller, which reads as "user zero" and is wider
				// than anonymous — see [RenderContext].
				return "", false
			}
			readable := visibility.FieldsReadableSQL(
				"fkm", rc.CallerArg, rc.Caller, rc.Caps, rc.MutationCaps)
			return `EXISTS (SELECT 1 FROM post_assets fkp
			                 JOIN assets fkm ON fkm.id = fkp.asset_id
			                WHERE fkp.post_id = ` + a + `id
			                  AND fkm.deleted_at IS NULL
			                  AND ` + viewkind.KindSQL("fkm") + ` = ` + p + `::TEXT` +
				readable + `)`, true
		}
		// A collection has no members that resolve to a badge kind — its
		// mosaic is composed from the assets INSIDE it, which are reached
		// by `collection:` on the asset entity rather than by this
		// dimension. So it falls through to ok=false and drops out of a
		// kind-filtered page entirely.
		//
		// ⚠️ That is the POSITIVE-NARROWING direction, and the check
		// [FacetAI]'s collection arm records is why it is safe here: a
		// caller asking for `kind:image` is asking about files, so a
		// collection leaving the page is the answer rather than a loss.
		// `ai:` had to be satisfiable precisely because it is an
		// EXCLUSION wearing a value's clothes.
	case FacetField:
		// #1157 — one metadata field's value, as an ordinary predicate.
		// #1165 — under one of four operators.
		//
		// ONE PLACEHOLDER, carrying `<code><op><value>`, and the split
		// happens in SQL. That is deliberate rather than lazy: this
		// function's contract is that every term renders exactly one
		// placeholder, and [Selection.SQL] appends exactly one arg per
		// term. Taking two here would change the arity for every
		// dimension, which is the line [FacetKind] also had to hold and
		// held by computing its value in SQL. `split_part`
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
		// ⛔ read_capability is NOT here, even though this function does
		// now receive a [RenderContext] (#1251). What that context
		// carries is a caller's RESOLVED, closed-set capabilities, and
		// `read_capability` is an open set an operator types at runtime —
		// the same reason [Query.CapChecker] cannot be a cache-key
		// component. It stays with [Selection.Authorize], which refuses
		// the whole search when a term names a field this caller may not
		// read.
		if e == visibility.EntityAsset {
			match, ok := fieldValueSQL(sh, p)
			if !ok {
				return "", false
			}
			return `EXISTS (SELECT 1 FROM asset_field_value ffv
			                 JOIN field_definition ffd ON ffd.id = ffv.field_id
			                WHERE ffv.asset_id = ` + a + `id
			                  AND ffd.code = split_part(` + p + `::TEXT, '` + string(sh.op) + `', 1)
			                  AND ffd.searchable = TRUE
			                  AND ffd.status = 'active'
			                  AND ` + match + `)`, true
		}
		// Posts and collections carry no asset_field_value rows, so a
		// field filter makes them unsatisfiable — zero hits AND zero
		// count, the same fall-through FacetCollection relies on. That
		// is correct rather than lossy: "assets whose material is
		// steel" is a question about assets.
	case FacetFileSize:
		// #1173 sprint 18b — the stored byte count of a file, as an
		// ordinary predicate under an ordered operator.
		//
		// ONE PLACEHOLDER, holding the whole bare bound `<op><digits>`,
		// and the split happens in SQL — the same contract [FacetField]
		// holds one case above, for the same reason: every term appends
		// exactly one arg, and taking two here would change the arity for
		// every dimension. The operator is a FIXED-WIDTH prefix here
		// rather than something to search for, so the value is everything
		// from byte len(op)+1 onward. `substr` is 1-indexed.
		//
		// The operator's spelling is spliced from a CLOSED Go constant
		// ([orderedBoundOps]), never from caller text: `sh.op` reaches
		// here only after [SplitOrderedBound] matched it against that
		// list, and the guard below is the same fail-closed refusal
		// [fieldValueSQL] makes for an operator it does not recognise.
		// The caller's bytes stay in the placeholder, where
		// [FacetType.CanonicalValue] has already proven they are an
		// int64 in base 10.
		//
		// # ⛔ ONLY AN ASSET HAS A FILE, AND THE OTHER TWO ARMS ARE THE POINT
		//
		// A post is a set of members and a collection is a container;
		// neither carries a byte count of its own. Both therefore fall
		// through to ok=false, which [Selection.SQL] returns as
		// satisfiable=false and every one of its call sites honours by
		// skipping the entity entirely — see [FacetExtension], which has
		// had exactly this shape since #907.
		//
		// That is the POSITIVE-NARROWING direction [FacetAI]'s collection
		// arm records as the test: a caller asking for files over 10MB is
		// asking about FILES, so an entity with no file leaving the page
		// is the answer rather than a loss. Treating an active narrowing
		// filter as "no constraint" on those arms would return every post
		// and every collection beside the qualifying assets, which is a
		// result set the filter made LARGER.
		//
		// ⚠️ NULL is not zero. `file_size_bytes` is nullable — an asset
		// whose file was never measured has no size — and a comparison
		// against NULL is NULL, so such a row satisfies NEITHER bound and
		// drops out. That is the fail-closed reading and it is asserted
		// rather than assumed: it is the only way a row can satisfy
		// neither `>=A` nor `<=B` for A <= B, since a real number is
		// always below, within or above the range.
		if sh.op != FieldOpAtLeast && sh.op != FieldOpAtMost {
			return "", false
		}
		if e == visibility.EntityAsset {
			return a + `file_size_bytes ` + string(sh.op) + ` substr(` + p +
				`::TEXT, ` + strconv.Itoa(len(sh.op)+1) + `)::BIGINT`, true
		}
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
		// [FacetType.CanonicalValue] for why the cast is safe.
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
// term shape sh, reading the term from placeholder p (#1165, typed #1173).
//
// The value is extracted in SQL as everything after the first occurrence
// of the operator, so `v` below is the caller's text and nothing else.
// An unrecognised operator — or a bound whose domain this function has no
// column for — returns ok=false, which [dimensionSQL] turns into "this
// entity matches nothing": the same fail-closed direction
// [SplitFieldTerm] takes at parse time, repeated here because
// [Selection.With] is exported and can seed a term the parser never saw.
//
// # Which storage column each operator reads, and why it is not all of
// them
//
// A field's storage depends on its type (web/src/lib/fieldOptions.ts
// VALUE_COLUMN): `select` and `tree` write one slug into `value_text`,
// `multi_select` writes an array into `value_options`, `number` writes
// `value_num`, and the date types write `value_date`.
//
//   - Equality and contains ask BOTH text columns, because a caller
//     ticking "steel" does not know or care which one holds it, and
//     asking both is one expression rather than a type lookup per term.
//   - A BOUND asks the ONE column its domain lives in. It keeps the
//     partial indexes `asset_field_value_date_idx (field_id, value_date)`
//     and `asset_field_value_num_idx (field_id, value_num)` usable, which
//     an OR across columns would have given up.
//
// # ⛔ THE COLUMN COMES FROM THE DOMAIN, NOT FROM THE OPERATOR
//
// It used to come from the operator — `>=` meant `value_date`, full stop
// — which is why a numeric field could not be compared at all and why
// `field:pixel_width>=1920` was a 400. The operator says which SIDE of a
// bound a row must fall on; only the quantity says which column holds it.
//
// ⚠️ AND THE DOMAIN IS NOT A GUESS ABOUT THE FIELD. It is read off the
// caller's own bound ([shapeOf]), and whether the FIELD may be compared
// that way is a separate, schema-aware refusal in [Selection.Authorize]
// (see [orderedFieldTypes]). The two agree by construction, because the
// domain -> column mapping here and the type -> domain mapping there are
// the same three rows read from two directions. If one ever slipped past
// the other the predicate would still match no row, because a field
// stored in `value_text` has NULL in both of these columns.
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
func fieldValueSQL(sh termShape, p string) (string, bool) {
	// Everything after the FIRST occurrence of the operator.
	v := `substr(` + p + `::TEXT, position('` + string(sh.op) + `' IN ` + p +
		`::TEXT) + ` + strconv.Itoa(len(sh.op)) + `)`
	switch sh.op {
	case FieldOpEq:
		return `(ffv.value_text = ` + v + ` OR ` + v + ` = ANY(ffv.value_options))`, true
	case FieldOpContains:
		return `(strpos(LOWER(ffv.value_text), LOWER(` + v + `)) > 0
		         OR EXISTS (SELECT 1 FROM unnest(COALESCE(ffv.value_options, '{}'::TEXT[])) fo
		                     WHERE strpos(LOWER(fo), LOWER(` + v + `)) > 0))`, true
	case FieldOpAtLeast, FieldOpAtMost:
		switch sh.domain {
		case domainTemporal:
			return `ffv.value_date ` + string(sh.op) + ` ` + v + `::TIMESTAMPTZ`, true
		case domainNumeric:
			return `ffv.value_num ` + string(sh.op) + ` ` + v + `::DOUBLE PRECISION`, true
		}
		return "", false
	}
	return "", false
}

// placeholder renders $N.
func placeholder(n int) string { return "$" + strconv.Itoa(n) }
