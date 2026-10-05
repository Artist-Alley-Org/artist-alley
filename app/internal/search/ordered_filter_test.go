// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18b — ORDERED FILTERING, ASSERTED AGAINST REAL ROWS.
//
// facet/ordered_test.go proves the grammar, the canonical identity and
// the rendered SQL. What is here is the half that can only be checked
// against ROWS, and it is three claims:
//
// # 1. TWO BOUNDS ARE AN INTERSECTION, PROVEN BY SET EQUALITY
//
// ⛔ A COUNT ASSERTION THAT PASSES ON A UNION PASSES ON THE BUG, so the
// fixture below builds FOUR controlled populations and compares hit ID
// SETS:
//
//	L  size > B              satisfies `>=A` only
//	U  size < A              satisfies `<=B` only
//	X  A <= size <= B        satisfies both        ← what the range returns
//	N  file_size_bytes NULL  satisfies neither
//
// ⭐ N EXISTS ONLY BECAUSE OF NULL. A real number is always below,
// within or above a range, so "satisfies neither bound" is UNREACHABLE
// for a sized row — which makes N simultaneously the null-handling proof
// and the only way this fixture can distinguish "matched nothing" from
// "was not asked".
//
// Under the OR bug the range returns X ∪ L ∪ U, which is strictly LARGER
// than either single bound — so the assertion fails against the bug in
// the loud direction as well as against any accidental widening.
//
// # 2. THE TWO VALIDITY CLASSES HAVE DIFFERENT OUTCOMES
//
// A malformed bound is pure and is a 400 (asserted in facet/, no rows
// needed). Whether a FIELD may be compared this way needs
// `field_definition.type`, so it is refused in Selection.Authorize and
// the outcome is an EMPTY RESULT SET, never an error. The fixture plants
// a `date` field, a `number` field and a `text` field so the refusal can
// be driven in every direction it has, WITH a counterweight — a gate that
// refused everybody would satisfy the refusal assertions and ship a dead
// feature.
//
// # 3. A SIZE FILTER EXCLUDES THE ARMS THAT HAVE NO FILE
//
// Asserted as EXACTLY ZERO posts and collections beside a non-zero asset
// count, on one request naming all three types.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"sort"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	obOwner int64 = 11731801
	// obPhrase appears in the title of every fixture row and nowhere
	// else in any developer's database, so a result set is attributable
	// to this fixture alone. #1173's own corpus, deliberately NOT shared
	// with field_filter_test.go's: these assets carry sizes and that
	// fixture's do not, and a size filter over a shared corpus would
	// make both files' counts depend on each other.
	obPhrase = "quiverstok"

	// The bounds. A <= B, and the populations are placed relative to
	// them rather than to absolute numbers, so the arithmetic is visible.
	obLower int64 = 5_000
	obUpper int64 = 9_000

	// Field codes. Three types, so the ordered-type refusal can be
	// driven in every direction.
	obDateCode   = "ob_captured_on"
	obNumberCode = "ob_pixel_width"
	obTextCode   = "ob_studio_notes"
	// ⭐ THE ONE THAT MAKES THE Authorize SEAM LOAD-BEARING. Every other
	// incompatible type stores its value somewhere the bound's column is
	// NULL, so the SQL alone already answers "no rows" and a test built
	// on those types passes whether or not the type check exists — which
	// is exactly what a mutation of this suite proved before this field
	// was added. `boolean` is ADR 0012's 1/0 in value_num, the SAME
	// column a numeric bound reads, so `>=0` on one MATCHES unless
	// something refuses it.
	obBoolCode = "ob_is_approved"
)

// obFixture is the four populations plus the typed fields, and the ids
// of each population so the assertions can be set comparisons rather
// than counts.
type obFixture struct {
	L, U, X, N []string // asset ids, sorted
	// ⭐ A POST AND A COLLECTION CARRYING THE SAME PHRASE, so "a size
	// filter returns exactly zero posts" is a claim about rows that
	// WOULD otherwise have matched. Without them the assertion is
	// vacuous — the search returns no posts because there are none, and
	// an arm that treated a size filter as no constraint would still
	// pass. A mutation of this suite proved exactly that before they
	// were added.
	Post, Collection string
}

// all returns the union, sorted — the unfiltered fixture population.
func (f obFixture) all() []string { return obUnion(f.L, f.U, f.X, f.N) }

func obUnion(sets ...[]string) []string {
	out := []string{}
	for _, s := range sets {
		out = append(out, s...)
	}
	sort.Strings(out)
	return out
}

// obSeed plants the fixture. Every population has N>=2 members, because
// a one-member population cannot distinguish "the set" from "a member of
// it" and the whole point of the grouping rule is a claim about SETS.
func obSeed(t *testing.T, pool *pgxpool.Pool) obFixture {
	t.Helper()
	ctx := context.Background()

	dateField, numberField := uuid.New(), uuid.New()
	textField, boolField := uuid.New(), uuid.New()
	for _, f := range []struct {
		id   uuid.UUID
		code string
		typ  string
	}{
		{dateField, obDateCode, "date"},
		{numberField, obNumberCode, "number"},
		{textField, obTextCode, "text"},
		{boolField, obBoolCode, "boolean"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO field_definition
			    (id, code, label, type, subject_kind, searchable, status)
			VALUES ($1, $2, $2, $3, 'asset', TRUE, 'active')`,
			f.id, f.code, f.typ); err != nil {
			t.Fatalf("seed field %s: %v", f.code, err)
		}
	}

	var fx obFixture
	// sizes: nil means the file_size_bytes column stays NULL.
	plant := func(label string, size *int64, dst *[]string) {
		for i := 0; i < 2; i++ {
			id := uuid.New()
			if _, err := pool.Exec(ctx, `
				INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
				                    sensitivity, processing_status, file_size_bytes)
				VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready',$4)`,
				id, obPhrase+" "+label, obOwner, size); err != nil {
				t.Fatalf("seed asset %s: %v", label, err)
			}
			*dst = append(*dst, id.String())
		}
	}
	sz := func(n int64) *int64 { return &n }
	// L is ABOVE the range, so it satisfies `>=A` and not `<=B`.
	plant("large", sz(obUpper+1_000), &fx.L)
	// U is BELOW, so it satisfies `<=B` and not `>=A`.
	plant("small", sz(obLower-1_000), &fx.U)
	// X is INSIDE, inclusive on both edges — one member sits exactly ON
	// each bound, so an operator that had become exclusive returns a
	// SMALLER X rather than the same one.
	{
		for _, v := range []int64{obLower, obUpper} {
			id := uuid.New()
			if _, err := pool.Exec(ctx, `
				INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
				                    sensitivity, processing_status, file_size_bytes)
				VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready',$4)`,
				id, obPhrase+" inrange", obOwner, v); err != nil {
				t.Fatalf("seed asset inrange: %v", err)
			}
			fx.X = append(fx.X, id.String())
		}
	}
	// ⭐ N — the only population that satisfies NEITHER bound, and the
	// only way to build one.
	plant("unsized", nil, &fx.N)

	// The post and the collection. Both carry obPhrase in their text, so
	// an unfiltered three-type search finds them and a size-filtered one
	// has something to exclude.
	postID, collID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`,
		postID, obOwner, obPhrase+" post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`,
		collID, obOwner, obPhrase+" collection"); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	fx.Post, fx.Collection = postID.String(), collID.String()

	// The typed field values hang on the X assets, so a field bound and
	// a size bound are expressible over one fixture.
	for _, id := range fx.X {
		aid := uuid.MustParse(id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_field_value (asset_id, field_id, value_date)
			VALUES ($1, $2, '2026-06-15'::TIMESTAMPTZ)`, aid, dateField); err != nil {
			t.Fatalf("seed date value: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_field_value (asset_id, field_id, value_num)
			VALUES ($1, $2, 1920)`, aid, numberField); err != nil {
			t.Fatalf("seed num value: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_field_value (asset_id, field_id, value_text)
			VALUES ($1, $2, 'approved')`, aid, textField); err != nil {
			t.Fatalf("seed text value: %v", err)
		}
		// ⭐ 1, in value_num — ADR 0012's boolean encoding, and the same
		// column a numeric bound reads.
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_field_value (asset_id, field_id, value_num)
			VALUES ($1, $2, 1)`, aid, boolField); err != nil {
			t.Fatalf("seed bool value: %v", err)
		}
	}

	sort.Strings(fx.L)
	sort.Strings(fx.U)
	sort.Strings(fx.X)
	sort.Strings(fx.N)
	// ⛔ Every population non-empty, asserted rather than assumed: an
	// empty L or U makes the intersection assertion pass on the OR bug.
	for _, p := range []struct {
		name string
		ids  []string
	}{{"L", fx.L}, {"U", fx.U}, {"X", fx.X}, {"N", fx.N}} {
		if len(p.ids) < 2 {
			t.Fatalf("population %s has %d members, want at least 2 — a fixture with an\n"+
				"  empty or singleton population cannot distinguish the intersection\n"+
				"  from the union.", p.name, len(p.ids))
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		ids := make([]uuid.UUID, 0, len(fx.all()))
		for _, s := range fx.all() {
			ids = append(ids, uuid.MustParse(s))
		}
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM field_definition WHERE id = ANY($1::uuid[])`,
			[]uuid.UUID{dateField, numberField, textField, boolField})
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, collID)
	})
	return fx
}

// obRun executes one filtered search over the fixture and returns the
// sorted hit ids.
func obRun(t *testing.T, pool *pgxpool.Pool, filters ...string) []string {
	t.Helper()
	return obRunTypes(t, pool, []HitType{HitTypeAsset}, nil, filters...)
}

func obRunTypes(
	t *testing.T, pool *pgxpool.Pool, types []HitType,
	caps visibility.CapabilityChecker, filters ...string,
) []string {
	t.Helper()
	sel, err := facet.ParseSelection(filters)
	if err != nil {
		t.Fatalf("ParseSelection(%v): %v — these are all well-formed filters; a rejection\n"+
			"  here is the PURE validity class failing, which is a different bug from\n"+
			"  the one this test is about", filters, err)
	}
	ref := obOwner
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          obPhrase,
		Types:         types,
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &ref,
		CapChecker:    caps,
	})
	if err != nil {
		t.Fatalf("Run(%v): %v", filters, err)
	}
	if res.TotalCount != len(res.Hits) {
		t.Errorf("total_count %d but %d hits for %v — the count is a derived copy of the\n"+
			"  result set, so an operator that narrowed one and not the other turns the\n"+
			"  count into an oracle the hits are not", res.TotalCount, len(res.Hits), filters)
	}
	return hitIDs(res)
}

// obAssertSet compares two sorted id slices, reporting the difference in
// both directions so a widening and a narrowing are distinguishable.
func obAssertSet(t *testing.T, what string, got, want []string, why string) {
	t.Helper()
	if sameIDs(got, want) {
		return
	}
	inGot := map[string]bool{}
	for _, g := range got {
		inGot[g] = true
	}
	inWant := map[string]bool{}
	for _, w := range want {
		inWant[w] = true
	}
	var extra, missing []string
	for _, g := range got {
		if !inWant[g] {
			extra = append(extra, g)
		}
	}
	for _, w := range want {
		if !inGot[w] {
			missing = append(missing, w)
		}
	}
	t.Errorf("%s returned %d ids, want %d — %s\n  UNEXPECTED (%d): %v\n  MISSING (%d): %v",
		what, len(got), len(want), why, len(extra), extra, len(missing), missing)
}

func obBound(op string, n int64) string {
	return "file_size:" + op + strconv.FormatInt(n, 10)
}

// TestFileSizeFilter_TwoBoundsAreTheIntersection is the load-bearing
// regression, and it is a SET comparison for the reason the header gives.
func TestFileSizeFilter_TwoBoundsAreTheIntersection(t *testing.T) {
	pool := coPool(t)
	fx := obSeed(t, pool)

	// The premise: unfiltered, the fixture is all four populations.
	obAssertSet(t, "the unfiltered fixture", obRun(t, pool), fx.all(),
		"if this is wrong the fixture is not isolated and nothing below means anything")

	lower := obBound(">=", obLower)
	upper := obBound("<=", obUpper)

	obAssertSet(t, lower, obRun(t, pool, lower), obUnion(fx.X, fx.L),
		"a lower bound admits the in-range rows AND the large ones, and no NULL-sized row")
	obAssertSet(t, upper, obRun(t, pool, upper), obUnion(fx.X, fx.U),
		"an upper bound admits the in-range rows AND the small ones, and no NULL-sized row")

	// ⛔ THE ONE THAT MATTERS.
	obAssertSet(t, lower+" AND "+upper, obRun(t, pool, lower, upper), fx.X,
		"two bounds with DIFFERENT operators are separate sub-groups and therefore AND.\n"+
			"  Under the OR bug this returns X ∪ L ∪ U — strictly LARGER than either\n"+
			"  single bound, which is a filter that made the result set bigger.\n"+
			"  A count assertion would pass on that; set equality does not")

	// An empty range is empty, not everything.
	obAssertSet(t, "an inverted range", obRun(t, pool, obBound(">=", obUpper+50_000), upper),
		[]string{}, "an unsatisfiable range returns nothing, never the union")
}

// TestFileSizeFilter_SameOperatorOrs is the other half of the
// classification: bounds sharing an operator are a VALUE LIST.
//
// N=0, N=1 and N>=2 in one place, because the rule is only visible at
// N>=2 and the first two are what every manual check exercises.
func TestFileSizeFilter_SameOperatorOrs(t *testing.T) {
	pool := coPool(t)
	fx := obSeed(t, pool)

	// N=0 — no size filter at all.
	obAssertSet(t, "N=0", obRun(t, pool), fx.all(), "no filter constrains nothing")

	// N=1 — one bound, the shape a manual check uses.
	obAssertSet(t, "N=1", obRun(t, pool, obBound(">=", obLower)), obUnion(fx.X, fx.L),
		"one lower bound")

	// N=2, SAME operator. `>=A OR >=A'` is the LOOSER of the two, so it
	// equals the result of the smaller bound alone. An implementation
	// that ANDed them would return the tighter one (L alone) instead.
	got := obRun(t, pool, obBound(">=", obLower), obBound(">=", obUpper+1))
	obAssertSet(t, "two lower bounds", got, obUnion(fx.X, fx.L),
		"two bounds with the SAME operator share a sub-group and OR, so the union is\n"+
			"  the looser bound's answer. Getting L alone here means they ANDed")

	// The same, downward.
	got = obRun(t, pool, obBound("<=", obUpper), obBound("<=", obLower-1))
	obAssertSet(t, "two upper bounds", got, obUnion(fx.X, fx.U),
		"two upper bounds OR to the looser one")

	// N=3: two lower bounds ORed, then intersected with an upper bound.
	got = obRun(t, pool, obBound(">=", obLower), obBound(">=", obUpper+1), obBound("<=", obUpper))
	obAssertSet(t, "two lower bounds AND one upper", got, fx.X,
		"(>=A OR >=B) AND <=U — the OR group narrows to nothing extra and the upper\n"+
			"  bound removes L")
}

// TestFileSizeFilter_ExcludesTheArmsWithNoFile is the cross-arm
// behaviour, asserted as EXACTLY ZERO rather than as "fewer".
//
// An arm that treated an active narrowing filter as no constraint would
// return every post and every collection beside the qualifying assets —
// a filter that made the result set LARGER.
func TestFileSizeFilter_ExcludesTheArmsWithNoFile(t *testing.T) {
	pool := coPool(t)
	fx := obSeed(t, pool)

	ref := obOwner
	threeTypes := func(filters ...string) map[HitType][]string {
		t.Helper()
		s, err := facet.ParseSelection(filters)
		if err != nil {
			t.Fatalf("parse %v: %v", filters, err)
		}
		res, err := NewEngine(pool).Run(context.Background(), Query{
			Text:          obPhrase,
			Types:         []HitType{HitTypeAsset, HitTypePost, HitTypeCollection},
			Limit:         50,
			Filters:       s,
			CallerUserRef: &ref,
		})
		if err != nil {
			t.Fatalf("run %v: %v", filters, err)
		}
		out := map[HitType][]string{}
		for _, h := range res.Hits {
			out[h.Type] = append(out[h.Type], h.ID.String())
		}
		for k := range out {
			sort.Strings(out[k])
		}
		return out
	}

	// ⛔ THE PREMISE. Without a size filter the SAME request returns the
	// post and the collection, so "exactly zero" below is a claim about
	// rows that would otherwise have matched rather than about an empty
	// corpus.
	unfiltered := threeTypes()
	obAssertSet(t, "the unfiltered three-type search's POSTS",
		unfiltered[HitTypePost], []string{fx.Post},
		"the fixture post carries the phrase; if it is missing here the exclusion\n"+
			"  assertion below proves nothing")
	obAssertSet(t, "the unfiltered three-type search's COLLECTIONS",
		unfiltered[HitTypeCollection], []string{fx.Collection},
		"same for the collection")

	filtered := threeTypes(obBound(">=", obLower))
	byType := map[HitType]int{}
	for k, v := range filtered {
		byType[k] = len(v)
	}
	if byType[HitTypePost] != 0 {
		t.Errorf("a size-filtered search returned %d POSTS, want exactly 0 — a post is a\n"+
			"  set of members and has no byte count, so it must fall out through the\n"+
			"  satisfiable=false path rather than render with no constraint",
			byType[HitTypePost])
	}
	if byType[HitTypeCollection] != 0 {
		t.Errorf("a size-filtered search returned %d COLLECTIONS, want exactly 0 — same\n"+
			"  reason: a container has no file", byType[HitTypeCollection])
	}
	// ⭐ The counterweight. Excluding every arm would satisfy both
	// assertions above and ship a filter that returns nothing.
	obAssertSet(t, "the asset arm of a three-type size-filtered search",
		filtered[HitTypeAsset], obUnion(fx.X, fx.L),
		"assets DO have a file and must still be returned")
}

// TestOrderedField_TypeCompatibilityFailsClosed is the SCHEMA-AWARE
// validity class, and its outcome is deliberately NOT the pure class's.
//
// ⛔ A malformed bound is a 400 out of ParseSelection. An INCOMPATIBLE
// one — a well-formed bound against a field whose declared type has no
// ordering, or has a different one — is refused in Selection.Authorize
// and comes back as an EMPTY RESULT SET, so "that field is a text field"
// and "no such field" are indistinguishable on a code the caller
// supplied. A test asserting an error here would look correct and prove
// the opposite of the decision.
func TestOrderedField_TypeCompatibilityFailsClosed(t *testing.T) {
	pool := coPool(t)
	fx := obSeed(t, pool)

	// ⭐ THE COUNTERWEIGHTS FIRST. A gate that refused everybody would
	// satisfy every refusal below and ship a dead feature.
	obAssertSet(t, "a temporal bound on a DATE field",
		obRun(t, pool, "field:"+obDateCode+">=2026-01-01"), fx.X,
		"the date field's values are 2026-06-15 and hang on the X assets")
	obAssertSet(t, "a numeric bound on a NUMBER field",
		obRun(t, pool, "field:"+obNumberCode+">=1000"), fx.X,
		"⛔ THIS IS THE FIX. Before 18b `>=` was date-only and this was a 400")
	obAssertSet(t, "a numeric range on a NUMBER field",
		obRun(t, pool, "field:"+obNumberCode+">=1000", "field:"+obNumberCode+"<=2000"), fx.X,
		"the (code, operator) sub-group rule already made this an AND; the bounds\n"+
			"  just had nowhere to be evaluated before")
	obAssertSet(t, "a numeric bound that excludes",
		obRun(t, pool, "field:"+obNumberCode+">=2000"), []string{},
		"1920 < 2000, so the bound must actually compare rather than match on presence")

	// The refusals. Every one is an EMPTY SET, never an error.
	for _, c := range []struct {
		name   string
		filter string
		why    string
	}{
		{
			"a numeric bound on a DATE field", "field:" + obDateCode + ">=1920",
			"a date field stores value_date; comparing it to 1920 is a question its\n" +
				"  column cannot answer, and answering it against value_num anyway would\n" +
				"  compare a different field's storage",
		},
		{
			"a temporal bound on a NUMBER field", "field:" + obNumberCode + ">=2026-01-01",
			"the mismatch is symmetric — neither direction may fall back to the other\n" +
				"  column",
		},
		{
			"a bound on a TEXT field", "field:" + obTextCode + ">=2026-01-01",
			"a range over prose is meaningless rather than merely narrow. ⚠️ This was\n" +
				"  ALREADY zero before 18b, because value_date is NULL on those rows —\n" +
				"  the behaviour is PRESERVED, and it is now refused for the stated\n" +
				"  reason instead of by accident",
		},
		{
			"a numeric bound on a TEXT field", "field:" + obTextCode + ">=1920",
			"and the arm 18b creates: value_num is NULL there too, so the refusal and\n" +
				"  the SQL agree",
		},
		{
			// ⛔⛔ THE ONE THAT PROVES THE SEAM DOES ANYTHING.
			"a numeric bound on a BOOLEAN field", "field:" + obBoolCode + ">=0",
			"a boolean is stored as 1 or 0 in value_num (ADR 0012) — the SAME column a\n" +
				"  numeric bound reads. So `>=0` MATCHES every fixture row unless something\n" +
				"  refuses it, and that something is the declared-type check in\n" +
				"  Selection.Authorize. Every other incompatible type is refused twice over\n" +
				"  (the check, and a NULL column), which makes this the only case that can\n" +
				"  tell the two apart. `at least true` is not a question",
		},
		{
			"an upper bound on a BOOLEAN field", "field:" + obBoolCode + "<=1",
			"and the other direction, which would match every row for the same reason",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			obAssertSet(t, c.filter, obRun(t, pool, c.filter), []string{}, c.why)
		})
	}
}

// TestOrderedField_RefusalDoesNotLeakThroughAnotherTerm pins the
// direction of the Authorize refusal.
//
// Authorize answers for the WHOLE query, so an incompatible term empties
// the whole result. That is only correct because every filter term is a
// CONJUNCT — dimensions AND, field codes AND — so a term that matches
// nothing already empties the result. Asserted rather than assumed,
// because an implementation that refused per TERM and dropped it would
// return the other term's rows and look like it worked.
func TestOrderedField_RefusalDoesNotLeakThroughAnotherTerm(t *testing.T) {
	pool := coPool(t)
	obSeed(t, pool)

	obAssertSet(t, "an incompatible bound beside a matching size filter",
		obRun(t, pool, "field:"+obTextCode+">=1920", obBound(">=", obLower)),
		[]string{},
		"the size filter alone matches X ∪ L. If those rows come back, the\n"+
			"  incompatible term was DROPPED rather than refused, which is a filter\n"+
			"  that looks applied and is not")
}

// TestOrderedFilter_RoundTripsThroughDSL is the saved-query half,
// asserted on the POPULATION rather than only on the representation.
//
// The same request twice: once with the selection the rail produced, once
// with the selection recovered from its canonical DSL. Set equality on
// ids, for the reason canonical_population_test.go gives.
func TestOrderedFilter_RoundTripsThroughDSL(t *testing.T) {
	pool := coPool(t)
	fx := obSeed(t, pool)

	for _, c := range []struct {
		name    string
		filters []string
		want    []string
	}{
		{"a size range", []string{obBound(">=", obLower), obBound("<=", obUpper)}, fx.X},
		{"one size bound", []string{obBound(">=", obLower)}, obUnion(fx.X, fx.L)},
		{"a numeric field bound", []string{"field:" + obNumberCode + ">=1000"}, fx.X},
		{
			"a size range beside a numeric field bound",
			[]string{obBound(">=", obLower), obBound("<=", obUpper), "field:" + obNumberCode + "<=2000"},
			fx.X,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			in, err := facet.ParseSelection(c.filters)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			out, text := throughDSL(t, in)
			if in.CacheKey() != out.CacheKey() {
				t.Errorf("the selection changed across the round trip via %q:\n  in  %v\n  out %v",
					text, in.Params(), out.Params())
			}
			direct := obRunSelection(t, pool, in)
			replayed := obRunSelection(t, pool, out)
			obAssertSet(t, "the direct selection", direct, c.want, "the fixture's expected set")
			obAssertSet(t, "the selection replayed from "+text, replayed, c.want,
				"a saved search must replay exactly the page it was saved from")
		})
	}
}

func obRunSelection(t *testing.T, pool *pgxpool.Pool, sel facet.Selection) []string {
	t.Helper()
	ref := obOwner
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          obPhrase,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &ref,
	})
	if err != nil {
		t.Fatalf("run %v: %v", sel.Params(), err)
	}
	return hitIDs(res)
}

// TestFileSizeFilter_ExactBeyondFloat53 drives an int64 bound past the
// point a float64 stops being exact, against a real BIGINT column.
//
// facet/ordered_test.go asserts the PARSE stays exact. This asserts the
// comparison does: a value round-tripped through a float64 anywhere on
// the path — the parse, the canonical form, the placeholder, the cast —
// would compare against a neighbouring integer instead.
func TestFileSizeFilter_ExactBeyondFloat53(t *testing.T) {
	pool := coPool(t)
	ctx := context.Background()

	// 2^53 = 9007199254740992. 2^53+1 is not representable as a float64;
	// it rounds DOWN to 2^53. So an asset of exactly 2^53+1 bytes must
	// satisfy `>=2^53+1`, and must NOT satisfy `>=2^53+2`.
	const big int64 = 9007199254740993
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, file_size_bytes)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready',$4)`,
		id, obPhrase+"big huge", obOwner, big); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})

	run := func(filter string) []string {
		sel, err := facet.ParseSelection([]string{filter})
		if err != nil {
			t.Fatalf("parse %q: %v", filter, err)
		}
		ref := obOwner
		res, err := NewEngine(pool).Run(ctx, Query{
			Text: obPhrase + "big huge", Types: []HitType{HitTypeAsset},
			Limit: 50, Filters: sel, CallerUserRef: &ref,
		})
		if err != nil {
			t.Fatalf("run %q: %v", filter, err)
		}
		return hitIDs(res)
	}

	obAssertSet(t, "file_size:>=9007199254740993", run("file_size:>=9007199254740993"),
		[]string{id.String()},
		"the bound equals the stored size EXACTLY. A float64 anywhere on the path\n"+
			"  rounds both to 2^53 and this still passes, so the next assertion is the\n"+
			"  one that discriminates")
	obAssertSet(t, "file_size:>=9007199254740994", run("file_size:>=9007199254740994"),
		[]string{},
		"⛔ THE DISCRIMINATOR. 2^53+1 < 2^53+2, so the row must NOT match. Through a\n"+
			"  float64 both bounds and the stored value collapse onto 2^53 and the row\n"+
			"  matches — a filter that admits a file smaller than the caller asked for")
	obAssertSet(t, "file_size:<=9007199254740992", run("file_size:<=9007199254740992"),
		[]string{},
		"and the same one bound downward: 2^53+1 > 2^53")
	// ⛔ THE SECOND DISCRIMINATOR, and the one that catches a float on the
	// path in the direction the two above do not. 2^53+1 is not
	// representable as a float64 and rounds DOWN to 2^53, so a bound
	// parsed through one becomes `<=2^53` and the row — which is EQUAL to
	// the bound the caller wrote — stops matching. A caller who asked for
	// "at most exactly this size" would not get the file that is exactly
	// that size.
	obAssertSet(t, "file_size:<=9007199254740993", run("file_size:<=9007199254740993"),
		[]string{id.String()},
		"the bound EQUALS the stored size and both bounds are inclusive. Through a\n"+
			"  float64 the bound rounds down to 2^53 and this returns nothing")
}
