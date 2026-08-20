// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1157 — THE `field:` FILTER DIMENSION AND ITS READ GATE.
// #1165 — AND THE OPERATOR ITS VALUE GRAMMAR NOW CARRIES.
//
// The advanced search page filters on metadata field values through one
// new facet dimension, `filter=field:<code><op><value>`. Three things
// about it need holding down by a test rather than by a comment.
//
// The grammar's own parse rules — which operators exist, what an unknown
// one does, how a term round-trips through the URL — live beside the
// parser in facet/selection_test.go and need no database. What is here
// is the half that can only be checked against ROWS.
//
// # 1. It filters
//
// The value has to reach BOTH storage columns, because a vocabulary
// field's storage depends on its type: `select` and `tree` write one
// slug into `value_text`, `multi_select` writes an array into
// `value_options`. A client ticking a value does not know which, so one
// expression asks both — and a test that only covered `select` would
// pass on a dimension that silently ignored every multi-select field.
//
// # 2. It does not answer questions about a field the caller cannot read
//
// `field_definition.read_capability` names a capability without which a
// caller may not read that field. #907 settled the principle for the
// five original dimensions: a filter must not answer a question about a
// column the caller may not read, because "with a narrow enough
// selection, the filter IS the item". A caller without the capability
// who could still partition the corpus by that field's values would be
// running #902's recovery attack on the field plane, one bit at a time.
//
// The assertions are COMPARATIVE — the same filter, the same fixture,
// two callers, opposite verdicts — because a gate that refused everybody
// would satisfy a leak-only test and ship a dead feature.
//
// # 3. Terms COMBINE the way a form implies (#1165)
//
// Two values of one field mean "either"; two different fields, and the
// two bounds of a range, mean "both". The distinction is invisible with
// a single filter and is the whole correctness of a range, so the
// fixture below is built so that every wrong combining rule produces a
// different, wrong number.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	fdOwner int64 = 11571101
	// fdPhrase appears in the title of every fixture asset and nowhere
	// else in any developer's database, so a count is attributable to
	// this fixture alone.
	fdPhrase = "brindlequax"
	// fdOpenCode is readable by everyone; fdGatedCode needs a capability.
	fdOpenCode  = "ff_open_stage"
	fdGatedCode = "ff_gated_clearance"
	fdGatedCap  = "ff.clearance.read"
	// #1165 — a text field for `~` and a date field for `>=` / `<=`.
	// Both open, because what these exercise is the OPERATOR; the read
	// gate is already held down against fdGatedCode and it applies per
	// FIELD, not per operator (facet.Selection.Authorize).
	fdNotesCode = "ff_open_notes"
	fdDateCode  = "ff_open_expires"
)

// fdSeed plants four searchable field definitions and three assets: one
// carrying the open vocabulary field via value_text, one carrying it via
// value_options (the multi_select path), and one carrying the gated
// field. All three also carry a text value and a date value, so the
// #1165 operators have rows to match. Returns nothing but its cleanup.
func fdSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	openField, gatedField := uuid.New(), uuid.New()
	// #1165 — the two field types the operator grammar exists for. They
	// hang on the SAME three assets as the vocabulary field above, so a
	// filter mixing a vocabulary value with a text or date constraint is
	// expressible against one fixture and the AND/OR grouping rule can be
	// asserted on real rows rather than only on rendered SQL.
	notesField, dateField := uuid.New(), uuid.New()
	for _, f := range []struct {
		id      uuid.UUID
		code    string
		typ     string
		readCap *string
	}{
		{openField, fdOpenCode, "select", nil},
		{gatedField, fdGatedCode, "select", fdStrPtr(fdGatedCap)},
		{notesField, fdNotesCode, "text", nil},
		{dateField, fdDateCode, "date", nil},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO field_definition
			    (id, code, label, type, subject_kind, searchable, status, read_capability)
			VALUES ($1, $2, $2, $3, 'asset', TRUE, 'active', $4)`,
			f.id, f.code, f.typ, f.readCap); err != nil {
			t.Fatalf("seed field %s: %v", f.code, err)
		}
	}

	textAsset, optsAsset, gatedAsset := uuid.New(), uuid.New(), uuid.New()
	for _, a := range []struct {
		id    uuid.UUID
		title string
	}{
		{textAsset, fdPhrase + " one"},
		{optsAsset, fdPhrase + " two"},
		{gatedAsset, fdPhrase + " three"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready')`,
			a.id, a.title, fdOwner); err != nil {
			t.Fatalf("seed asset %s: %v", a.title, err)
		}
	}

	// value_text (the select/tree path).
	if _, err := pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_text)
		VALUES ($1, $2, 'Locked')`, textAsset, openField); err != nil {
		t.Fatalf("seed value_text: %v", err)
	}
	// value_options (the multi_select path) — SAME field code, so the
	// two storage columns are exercised by one filter and a dimension
	// that read only one of them fails here rather than in production.
	if _, err := pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_options)
		VALUES ($1, $2, ARRAY['Locked','Other'])`, optsAsset, openField); err != nil {
		t.Fatalf("seed value_options: %v", err)
	}
	// The gated field.
	if _, err := pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_text)
		VALUES ($1, $2, 'Secret')`, gatedAsset, gatedField); err != nil {
		t.Fatalf("seed gated value: %v", err)
	}

	// #1165 — notes and dates on all three assets.
	//
	// The VALUES are chosen so that each operator's expected count is
	// distinct AND so that a combination discriminates AND from OR.
	// `Pending REVIEW` is deliberately mixed-case: `~` promises
	// case-insensitivity, and a fixture spelled the same way as the
	// query would pass on an operator that had silently become
	// case-sensitive.
	//
	// Dates: only optsAsset falls inside [2026-04-01, 2026-08-01], while
	// each bound ALONE admits two rows. So a range that ORs its bounds
	// returns 3, one that ANDs them returns 1, and no arithmetic on a
	// single count can confuse the two.
	for _, v := range []struct {
		asset uuid.UUID
		notes string
		date  string
	}{
		{textAsset, "Approved for print", "2026-03-01"},
		{optsAsset, "Pending REVIEW", "2026-06-15"},
		{gatedAsset, "rejected outright", "2026-09-30"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_field_value (asset_id, field_id, value_text)
			VALUES ($1, $2, $3)`, v.asset, notesField, v.notes); err != nil {
			t.Fatalf("seed notes for %s: %v", v.asset, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_field_value (asset_id, field_id, value_date)
			VALUES ($1, $2, $3::TIMESTAMPTZ)`, v.asset, dateField, v.date); err != nil {
			t.Fatalf("seed date for %s: %v", v.asset, err)
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		ids := []uuid.UUID{textAsset, optsAsset, gatedAsset}
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM field_definition WHERE id = ANY($1::uuid[])`,
			[]uuid.UUID{openField, gatedField, notesField, dateField})
	})
}

func fdStrPtr(s string) *string { return &s }

// fdRun executes one search with one or more field filters for one
// caller.
//
// Variadic since #1165: a range is TWO terms, and the rule that they
// narrow each other rather than widening cannot be stated with one.
func fdRun(t *testing.T, pool *pgxpool.Pool, caps visibility.CapabilityChecker, filters ...string) int {
	t.Helper()
	return fdRunText(t, pool, fdPhrase, caps, filters...)
}

// fdRunText is fdRun with the free-text term under the caller's control,
// so the filter-only path ("" text) can be driven through the same door.
func fdRunText(
	t *testing.T, pool *pgxpool.Pool, text string,
	caps visibility.CapabilityChecker, filters ...string,
) int {
	t.Helper()
	sel, err := facet.ParseSelection(filters)
	if err != nil {
		t.Fatalf("parse %q: %v", filters, err)
	}
	ref := fdOwner
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          text,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &ref,
		CapChecker:    caps,
	})
	if err != nil {
		t.Fatalf("run %q: %v", filters, err)
	}
	// #1165 acceptance: the count and the array are the SAME number.
	// A search's total_count is a derived copy of its result set, so an
	// operator that narrowed one and not the other would turn the count
	// into an oracle the hits are not. Asserted on EVERY run through this
	// helper rather than in a test of its own, so no future operator can
	// be added without it being checked.
	if res.TotalCount != len(res.Hits) {
		t.Errorf("total_count %d but %d hits for %q — the count and the result "+
			"set must narrow together or the count leaks",
			res.TotalCount, len(res.Hits), filters)
	}
	return len(res.Hits)
}

// TestFieldFilter_MatchesBothStorageColumns is the "it filters" half.
func TestFieldFilter_MatchesBothStorageColumns(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	// Unfiltered: all three fixture assets.
	if got := fdRun(t, pool, nil, "field:"+fdOpenCode+"=Locked"); got != 2 {
		t.Errorf("field:%s=Locked returned %d hits, want 2 — one stored in\n"+
			"  value_text and one in value_options. A dimension reading only\n"+
			"  one column returns 1 here and silently ignores every\n"+
			"  multi_select field in production.", fdOpenCode, got)
	}
	// A value nothing carries.
	if got := fdRun(t, pool, nil, "field:"+fdOpenCode+"=Nonexistent"); got != 0 {
		t.Errorf("an unmatched value returned %d hits, want 0", got)
	}
	// A well-formed code for a field that does not exist: zero, not an
	// error — the shape check is deliberately not an existence oracle.
	if got := fdRun(t, pool, nil, "field:ff_no_such_field=Locked"); got != 0 {
		t.Errorf("an unknown field code returned %d hits, want 0", got)
	}
}

// TestFieldFilter_GatedFieldRefusesWithoutTheCapability is the gate,
// with its counterweight.
func TestFieldFilter_GatedFieldRefusesWithoutTheCapability(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	filter := "field:" + fdGatedCode + "=Secret"
	holder := visibility.CapabilityChecker(func(code string) bool { return code == fdGatedCap })
	stranger := visibility.CapabilityChecker(func(string) bool { return false })

	if got := fdRun(t, pool, stranger, filter); got != 0 {
		t.Errorf("a caller WITHOUT %s got %d hits from a filter on that field, want 0.\n"+
			"  A filter must not answer a question about a column the caller\n"+
			"  may not read (#907) — with a narrow enough selection the filter\n"+
			"  IS the item.", fdGatedCap, got)
	}
	// ⭐ The counterweight. A gate that refused everyone would pass the
	// assertion above and ship a filter nobody can use.
	if got := fdRun(t, pool, holder, filter); got != 1 {
		t.Errorf("a caller WITH %s got %d hits, want 1 — this is a gate, not a\n"+
			"  removal of the feature.", fdGatedCap, got)
	}
	// And the open field is unaffected for the same stranger, so the
	// refusal is scoped to the gated field rather than to `field:`.
	if got := fdRun(t, pool, stranger, "field:"+fdOpenCode+"=Locked"); got != 2 {
		t.Errorf("the ungated field returned %d hits for the stranger, want 2 —\n"+
			"  the gate keys on the FIELD, not on the dimension.", got)
	}
}

// TestFieldFilter_SelectionIsUncacheable pins the cache decision, which
// is otherwise invisible and would rot silently.
//
// A `field:` result cannot be keyed: `read_capability` names a
// capability from an open set that keyForQuery cannot enumerate, and the
// cache is consulted BEFORE Engine.Run, where the gate lives. So a
// caller whose capability was revoked would keep being served the
// narrowed page for the rest of the TTL — the revoke direction, which
// serves MORE than the caller is owed.
func TestFieldFilter_SelectionIsUncacheable(t *testing.T) {
	for _, c := range []struct {
		name   string
		filter string
		want   bool
	}{
		{"a field selection", "field:x=y", true},
		{"an ordinary selection", "extension:png", false},
		{"a collection selection", "collection:" + uuid.New().String(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sel, err := facet.ParseSelection([]string{c.filter})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := sel.NamesFieldDimension(); got != c.want {
				t.Errorf("NamesFieldDimension() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFieldFilter_FilterOnlySearchRuns is the #1157 review finding: a
// selection with NO free text is a runnable search.
//
// It was not, for ANY dimension. `/search?filter=extension:png` with no
// `q` answered 400 `query_required` — the HTTP handler rejected on empty
// text before `filter=` was parsed at all, so this predates the `field:`
// dimension and had been dead since the rail shipped in #907. "Everything
// at pipeline stage Final" is the primary question an advanced search
// page exists to ask.
//
// Two halves, and the second is the one that could go wrong quietly:
// the text predicate has to go (`plainto_tsquery('english',”)` is the
// empty tsquery and matches no row, so keeping it would return nothing
// however the caller filtered) while the FIELD PLANE has to stay
// (AssetSearchMatchSQL is `search_text @@ q AND FieldsReadableSQL`, and
// only the first half is about text).
func TestFieldFilter_FilterOnlySearchRuns(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	// The fixture's own field, no text at all.
	if got := fdRunText(t, pool, "", nil, "field:"+fdOpenCode+"=Locked"); got != 2 {
		t.Errorf("a filter-only search returned %d hits, want 2.\n"+
			"  Zero here means the empty tsquery is still being ANDed in;\n"+
			"  an error means the request never reached the Engine.", got)
	}
	// And it is still a SEARCH, not "everything": a value nothing carries
	// returns nothing rather than the whole table.
	if got := fdRunText(t, pool, "", nil, "field:"+fdOpenCode+"=Nonexistent"); got != 0 {
		t.Errorf("a filter-only search on an unmatched value returned %d hits, want 0 —\n"+
			"  dropping the text predicate must not drop the FILTER with it.", got)
	}
}

// TestFieldFilter_FilterOnlyKeepsTheFieldPlane is the security half,
// asserted on the new path specifically.
//
// ⚠️ Without this, the natural implementation — skip the whole match
// fragment when the text is empty — silently sheds FieldsReadableSQL,
// because that conjunct lives INSIDE AssetSearchMatchSQL. The gate would
// still pass every existing test, all of which run with text present.
func TestFieldFilter_FilterOnlyKeepsTheFieldPlane(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	filter := "field:" + fdGatedCode + "=Secret"
	holder := visibility.CapabilityChecker(func(code string) bool { return code == fdGatedCap })
	stranger := visibility.CapabilityChecker(func(string) bool { return false })

	if got := fdRunText(t, pool, "", stranger, filter); got != 0 {
		t.Errorf("a filter-only search by a caller WITHOUT %s returned %d hits, want 0.\n"+
			"  The read gate must not depend on there being text to match.", fdGatedCap, got)
	}
	if got := fdRunText(t, pool, "", holder, filter); got != 1 {
		t.Errorf("a filter-only search by a caller WITH %s returned %d hits, want 1.", fdGatedCap, got)
	}
}

// TestEmptyRequestIsStillRejected pins the other side of the widened
// rule: text, similarity hint and selection all absent is still 400.
func TestEmptyRequestIsStillRejected(t *testing.T) {
	pool := coPool(t)
	_, err := NewEngine(pool).Run(context.Background(), Query{
		Types: []HitType{HitTypeAsset},
		Limit: 10,
	})
	if !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("a request with no text, no hint and no filters returned err=%v,\n"+
			"  want ErrEmptyQuery — widening what counts as a search must not\n"+
			"  make the genuinely empty request run.", err)
	}
}

// TestFieldOperator_Contains is `~`: a case-insensitive substring over
// whichever text column the field's type uses.
//
// The two arms are not redundant. `value_text` is the `text`/`select`
// path and `value_options` is the `multi_select` path, and the second is
// an ARRAY — an implementation that flattened it to a string would match
// text no single element contains, and one that forgot it entirely would
// pass a value_text-only test while ignoring every multi-select field.
func TestFieldOperator_Contains(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	// Case-insensitive: the fixture spells it `Pending REVIEW`.
	if got := fdRun(t, pool, nil, "field:"+fdNotesCode+"~review"); got != 1 {
		t.Errorf("%s~review returned %d hits, want 1 — the fixture's only match is\n"+
			"  spelled `Pending REVIEW`, so a case-SENSITIVE operator returns 0 here.",
			fdNotesCode, got)
	}
	// A substring that is not a whole word, and not at the start: this is
	// what separates `~` from equality and from a prefix match.
	if got := fdRun(t, pool, nil, "field:"+fdNotesCode+"~pprove"); got != 1 {
		t.Errorf("%s~pprove returned %d hits, want 1 (inside `Approved for print`)",
			fdNotesCode, got)
	}
	// Present in all three, so `~` is not accidentally an equality.
	if got := fdRun(t, pool, nil, "field:"+fdNotesCode+"~e"); got != 3 {
		t.Errorf("%s~e returned %d hits, want 3 — every fixture value contains an `e`",
			fdNotesCode, got)
	}
	if got := fdRun(t, pool, nil, "field:"+fdNotesCode+"~nosuchtext"); got != 0 {
		t.Errorf("%s~nosuchtext returned %d hits, want 0", fdNotesCode, got)
	}
	// The value_options arm: `Locked` lives in value_text on one asset
	// and inside an ARRAY on another, and `ock` is a substring of neither
	// column's whole contents on the array side unless it is unnested.
	if got := fdRun(t, pool, nil, "field:"+fdOpenCode+"~ock"); got != 2 {
		t.Errorf("%s~ock returned %d hits, want 2 — one via value_text and one via\n"+
			"  value_options, which only matches if the array is unnested.",
			fdOpenCode, got)
	}
	// `%` and `_` are LIKE metacharacters and must be literal here. If
	// this ever returns non-zero the operator has grown a wildcard the
	// caller did not ask for.
	if got := fdRun(t, pool, nil, "field:"+fdNotesCode+"~%"); got != 0 {
		t.Errorf("%s~%% returned %d hits, want 0 — `%%` must be literal text, not a\n"+
			"  wildcard. A LIKE-based implementation with a missing escape returns 3.",
			fdNotesCode, got)
	}
	if got := fdRun(t, pool, nil, "field:"+fdNotesCode+"~_"); got != 0 {
		t.Errorf("%s~_ returned %d hits, want 0 — `_` must be literal, not "+
			"match-any-character.", fdNotesCode, got)
	}
}

// TestFieldOperator_DateRange is `>=` / `<=`, and THE RANGE IS THE POINT.
//
// Each bound alone admits two of the three fixture rows, and only one
// row satisfies both. So the two-bound assertion discriminates every
// wrong combining rule: ORed bounds return 3, a dropped second bound
// returns 2, and an off-by-one on the inclusive edge returns 0 or 2.
func TestFieldOperator_DateRange(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)
	f := func(op, v string) string { return "field:" + fdDateCode + op + v }

	if got := fdRun(t, pool, nil, f(">=", "2026-04-01")); got != 2 {
		t.Errorf("%s>=2026-04-01 returned %d hits, want 2 (June + September)", fdDateCode, got)
	}
	if got := fdRun(t, pool, nil, f("<=", "2026-08-01")); got != 2 {
		t.Errorf("%s<=2026-08-01 returned %d hits, want 2 (March + June)", fdDateCode, got)
	}
	// ⛔ The one that matters.
	if got := fdRun(t, pool, nil, f(">=", "2026-04-01"), f("<=", "2026-08-01")); got != 1 {
		t.Errorf("the range [2026-04-01, 2026-08-01] returned %d hits, want 1 (June).\n"+
			"  3 means the two bounds ORed — a range that matches every row with a\n"+
			"  date at all, which is the failure #1165 exists to make impossible.\n"+
			"  2 means the second bound was dropped.", got)
	}
	// Both bounds are INCLUSIVE, asserted on the exact fixture instants
	// rather than near them.
	if got := fdRun(t, pool, nil, f(">=", "2026-06-15"), f("<=", "2026-06-15")); got != 1 {
		t.Errorf("the closed range [2026-06-15, 2026-06-15] returned %d hits, want 1 —\n"+
			"  a date-only UPPER bound must cover the whole of that day, not just\n"+
			"  its first instant. 0 means it was read as midnight.", got)
	}
	// An empty range is empty, not everything.
	if got := fdRun(t, pool, nil, f(">=", "2026-08-01"), f("<=", "2026-04-01")); got != 0 {
		t.Errorf("an inverted range returned %d hits, want 0", got)
	}
	// A bound against a TEXT field matches nothing: value_date is NULL on
	// those rows, which is the fail-closed answer rather than a cast error.
	if got := fdRun(t, pool, nil, "field:"+fdNotesCode+">=2026-01-01"); got != 0 {
		t.Errorf("a date bound on a text field returned %d hits, want 0", got)
	}
}

// TestFieldOperator_DifferentFieldsNarrow is the grouping rule on real
// rows (#1165), and it is a bug fix as much as a new feature.
//
// Before #1165 every `field:` term ORed with every other regardless of
// which field it named, so adding a second filter to the advanced page
// WIDENED the result set. That is invisible with one filter and obvious
// with two, which is why this needs a fixture where the two filters have
// different, overlapping answers.
func TestFieldOperator_DifferentFieldsNarrow(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	stage := "field:" + fdOpenCode + "=Locked"  // 2 hits
	notes := "field:" + fdNotesCode + "~review" // 1 hit, and it is one of the 2
	if got := fdRun(t, pool, nil, stage); got != 2 {
		t.Fatalf("precondition: %s returned %d, want 2", stage, got)
	}
	if got := fdRun(t, pool, nil, notes); got != 1 {
		t.Fatalf("precondition: %s returned %d, want 1", notes, got)
	}
	if got := fdRun(t, pool, nil, stage, notes); got != 1 {
		t.Errorf("two filters naming DIFFERENT fields returned %d hits, want 1.\n"+
			"  2 means they ORed — adding a filter widened the result set, which is\n"+
			"  the #1157 grouping bug #1165 fixes.", got)
	}
	// Two values of ONE field still OR: that is what a multi-select row
	// on the advanced page means, and the fix must not have flattened it.
	both := []string{"field:" + fdOpenCode + "=Locked", "field:" + fdOpenCode + "=Other"}
	if got := fdRun(t, pool, nil, both...); got != 2 {
		t.Errorf("two values of ONE field returned %d hits, want 2 — same field, same\n"+
			"  operator still means `either`, and ANDing them would return 1.", got)
	}
}

// TestFieldOperator_UnknownIsRejectedAtTheEdge is the acceptance #1165
// names: an unknown operator ERRORS rather than matching everything.
//
// Asserted at ParseSelection — the edge the HTTP handler maps to 400 —
// because that is where the decision has to be made. By the time a term
// reaches the Engine the only honest answers left are "empty" or "wrong".
func TestFieldOperator_UnknownIsRejectedAtTheEdge(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)

	for _, bad := range []string{
		"field:" + fdDateCode + ">2026-04-01", // exclusive bound: not defined
		"field:" + fdNotesCode + "!=review",   // not-equal: not defined
		"field:" + fdDateCode + ">=whenever",  // unparseable bound
		"field:" + fdNotesCode,                // no operator at all
	} {
		if _, err := facet.ParseSelection([]string{bad}); err == nil {
			t.Errorf("ParseSelection(%q) accepted it — an unknown or malformed\n"+
				"  operator must be refused so the handler answers 400. Accepting it\n"+
				"  would run SOME query, and whichever one it ran would not be the\n"+
				"  one the caller asked for.", bad)
		}
	}
	// And the corpus is untouched by the refusal: a well-formed filter
	// beside it still returns exactly its own rows.
	if got := fdRun(t, pool, nil, "field:"+fdOpenCode+"=Locked"); got != 2 {
		t.Errorf("after the refusals above, a valid filter returned %d hits, want 2", got)
	}
}

// TestFieldFilter_CountEqualsResultsAcrossPlanes is the acceptance the
// advanced page's LIVE RESULT COUNT rests on (#1173).
//
// # Why a count needs its own test at all
//
// The page previews "N results" as the form changes, and it gets that
// number by running the very query the Search button navigates to and
// reading `total_count`. That is safe only because the engine narrows
// the count and the hits with ONE predicate string
// (`matchFrag + visFrag + matureFrag + selFrag`, spliced identically
// into both statements in runAssetQuery). This test is what stops that
// from silently ceasing to be true.
//
// A count is a DERIVED COPY of a result set. If a row excluded from the
// results is still counted, the number becomes an oracle the list is
// not: a caller narrows a filter until the count moves and has recovered
// a withheld value one bit at a time. That is #902 on the text plane and
// #1066 on the similarity plane, and both were a second path computing
// "the same" thing under a slightly different rule.
//
// # The assertion is EQUALITY, and it is checked on BOTH planes
//
// "The count is non-zero" would pass on the bug. What is asserted is
// that the count equals the number of rows actually returned, for a
// caller who may see the restricted row and for one who may not — and,
// separately, that those two callers get DIFFERENT answers, because a
// gate that hid the row from everybody would satisfy an equality-only
// test while shipping a dead filter.
func TestFieldFilter_CountEqualsResultsAcrossPlanes(t *testing.T) {
	pool := coPool(t)
	fdSeed(t, pool)
	ctx := context.Background()

	// A fourth asset, RESTRICTED, carrying the same open field value as
	// the two public ones. Nothing else about the fixture changes.
	restricted := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','restricted','ready')`,
		restricted, fdPhrase+" restricted", fdOwner); err != nil {
		t.Fatalf("seed restricted asset: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO asset_field_value (asset_id, field_id, value_text)
		SELECT $1, id, 'Locked' FROM field_definition WHERE code = $2`,
		restricted, fdOpenCode); err != nil {
		t.Fatalf("seed restricted value: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, restricted)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, restricted)
	})

	sel, err := facet.ParseSelection([]string{"field:" + fdOpenCode + "=Locked"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	run := func(caller *int64) (total, hits int) {
		t.Helper()
		res, err := NewEngine(pool).Run(ctx, Query{
			Text:          fdPhrase,
			Types:         []HitType{HitTypeAsset},
			Limit:         50, // > the fixture, so the array is never truncated
			Filters:       sel,
			CallerUserRef: caller,
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res.TotalCount, len(res.Hits)
	}

	owner := fdOwner
	for _, c := range []struct {
		name   string
		caller *int64
	}{
		{"anonymous", nil},
		{"the owner, who may see the restricted row", &owner},
	} {
		total, hits := run(c.caller)
		if total != hits {
			t.Errorf("%s: total_count %d but %d hits returned.\n"+
				"  The advanced page shows total_count as its live result count, so a\n"+
				"  count that outran the result set would tell that caller how many\n"+
				"  rows they are NOT allowed to see.", c.name, total, hits)
		}
	}

	anonTotal, _ := run(nil)
	ownerTotal, _ := run(&owner)
	if anonTotal >= ownerTotal {
		t.Errorf("anonymous counted %d and the owner counted %d — the restricted row\n"+
			"  must raise the owner's count and not the stranger's. Equal counts mean\n"+
			"  either the row leaks into the anonymous count or the fixture is not\n"+
			"  actually restricted, and an equality-only assertion cannot tell those\n"+
			"  apart from a working gate.", anonTotal, ownerTotal)
	}
}
