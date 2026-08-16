// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

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
	case FacetField:
		// #1157 — `<code>=<value>`. The SECOND dimension with a value
		// grammar, and the first whose value is compound.
		//
		// `=` rather than `:` because [ParseSelection] cuts the wire
		// token at the FIRST colon, so a nested colon would be
		// ambiguous with the dimension separator the moment a field
		// value contained one. `=` appears in no field CODE (codes are
		// slugs), and a `=` inside the VALUE is harmless because only
		// the first one separates.
		code, value, found := strings.Cut(v, "=")
		if !found {
			return "", false
		}
		code = strings.ToLower(strings.TrimSpace(code))
		value = strings.TrimSpace(value)
		if code == "" || value == "" || !validFieldCode(code) {
			return "", false
		}
		return code + "=" + value, true
	}
	return v, true
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

// SplitFieldTerm splits a canonical [FacetField] value into its code
// and value halves. Exported because [Selection.Authorize] needs the
// code to look the field up, and the frontend's chip labels need both.
func SplitFieldTerm(v string) (code, value string, ok bool) {
	code, value, ok = strings.Cut(v, "=")
	return code, value, ok
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
			code, _, ok := SplitFieldTerm(t.Value)
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
		parts := make([]string, 0, len(values))
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
			idx++
			expr, ok := dimensionSQL(e, dim, a, idx)
			if !ok {
				return "", nil, false
			}
			parts = append(parts, expr)
			args = append(args, v)
		}
		b.WriteString(" AND (" + strings.Join(parts, joiner) + ")")
	}
	return b.String(), args, true
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
func dimensionSQL(e visibility.EntityType, dim FacetType, a string, idx int) (string, bool) {
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
	case FacetField:
		// #1157 — one metadata field's value, as an ordinary predicate.
		//
		// ONE PLACEHOLDER, carrying `<code>=<value>`, and the split
		// happens in SQL. That is deliberate rather than lazy: this
		// function's contract is that every term renders exactly one
		// placeholder, and [Selection.SQL] appends exactly one arg per
		// term. Taking two here would change the arity for every
		// dimension — the same objection [Selection.Authorize]'s doc
		// records against threading the caller through. `split_part`
		// takes the code, and `substr(… position('=' …))` takes
		// everything after the FIRST `=`, so a value containing `=`
		// survives intact.
		//
		// The value is matched against BOTH storage columns because a
		// vocabulary field's storage depends on its type
		// (web/src/lib/fieldOptions.ts VALUE_COLUMN): `select` and
		// `tree` write one slug into `value_text`, `multi_select`
		// writes an array into `value_options`. A caller ticking
		// "steel" does not know or care which, and asking both is one
		// expression rather than a type lookup per term.
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
			return `EXISTS (SELECT 1 FROM asset_field_value ffv
			                 JOIN field_definition ffd ON ffd.id = ffv.field_id
			                WHERE ffv.asset_id = ` + a + `id
			                  AND ffd.code = split_part(` + p + `::TEXT, '=', 1)
			                  AND ffd.searchable = TRUE
			                  AND ffd.status = 'active'
			                  AND (ffv.value_text = substr(` + p + `::TEXT, position('=' IN ` + p + `::TEXT) + 1)
			                       OR substr(` + p + `::TEXT, position('=' IN ` + p + `::TEXT) + 1)
			                          = ANY(ffv.value_options)))`, true
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

// placeholder renders $N.
func placeholder(n int) string { return "$" + strconv.Itoa(n) }
