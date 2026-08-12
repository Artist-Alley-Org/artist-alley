// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package facet

import (
	"errors"
	"sort"
	"strconv"
	"strings"

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
	}
	return "", false
}

// placeholder renders $N.
func placeholder(n int) string { return "$" + strconv.Itoa(n) }
