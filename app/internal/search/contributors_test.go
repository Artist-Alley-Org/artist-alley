// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18d — THE CONTRIBUTOR LOOKUP, ASSERTED AGAINST REAL ROWS.
//
// Every claim below needs a database, because every one of them is about
// which rows enter a candidate set and in what order. The three that
// cannot be checked any other way:
//
//  1. ⭐⭐ THE NAMELESS CONTRIBUTOR. A user whose three stored identity
//     columns are all empty renders as `user <ref>` through ADR 0070's
//     rung 4, which is INVENTED rather than stored. No prefix over the
//     stored columns can reach them, so a REQUIRED prefix would make
//     them permanently unreachable and continuation could not repair it
//     — the row would never enter the candidate set at all. The proof
//     that the prefix is genuinely optional is that empty-prefix
//     browsing REACHES this row, and that it is ABSENT from a
//     non-matching prefix.
//
//  2. ⭐ THE PAGE BOUNDARY. Cursor completeness is a claim about two
//     responses, so it needs more contributors than fit in one page and
//     an assertion on the CONCATENATION: no duplicate, no skip, and a
//     terminal condition distinguishable from "more available".
//
//  3. THE THREE COLUMNS MATCH INDEPENDENTLY. `display_name`, `fullname`
//     and `username` are ORed, not laddered. A fixture where each column
//     carries a DIFFERENT distinctive token is the only shape that can
//     tell an independent OR from a reconstructed
//     `COALESCE(display_name, fullname, username)` — under the ladder,
//     the fullname and username tokens of a user who HAS a display_name
//     would match nothing.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/users"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	// One nonsense phrase, so the population under test is exactly the
	// rows this file inserted and never a neighbouring fixture's.
	cbPhrase = "quillombra"

	// Refs well outside anything a seeder allocates. Ordered so the
	// `ref ASC` tiebreak is observable: LADDER < NAMELESS < OTHER.
	cbLadderRef   int64 = 11731851
	cbNamelessRef int64 = 11731852
	cbOtherRef    int64 = 11731853

	// ⭐ THREE DIFFERENT TOKENS, ONE PER COLUMN. This is what separates
	// an independent OR from a precedence ladder: `cbLadderRef` carries
	// all three, and a ladder would only ever expose the first.
	cbDisplayToken  = "narwhalx"
	cbFullnameToken = "quokkax"
	cbUsernameToken = "zebux"

	// A prefix that matches NONE of the three, used to prove the
	// nameless contributor is absent from a non-matching search rather
	// than merely rare.
	cbMissToken = "wolverinx"
)

// cbUser inserts one user with the exact identity columns given. A nil
// pointer writes SQL NULL, which is the state `username`, `fullname`
// and the whole profile row legitimately hold — none of the three
// carries a NOT NULL or a CHECK.
func cbUser(t *testing.T, pool *pgxpool.Pool, ref int64, username, fullname, displayName *string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "user" (ref, username, fullname, approved)
		VALUES ($1, $2, $3, 1)`, ref, username, fullname); err != nil {
		t.Fatalf("seed user %d: %v", ref, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	if displayName == nil {
		return
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_profiles (user_ref, display_name) VALUES ($1, $2)`,
		ref, *displayName); err != nil {
		t.Fatalf("seed profile %d: %v", ref, err)
	}
}

// cbAsset inserts one searchable, publicly visible asset owned by ref.
func cbAsset(t *testing.T, pool *pgxpool.Pool, title string, owner int64, sensitivity string) string {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready')`,
		id, title, owner, sensitivity); err != nil {
		t.Fatalf("seed asset %q: %v", title, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id.String()
}

func strp(s string) *string { return &s }

// cbSeed plants the controlled population and PROVES ITS OWN PREMISES.
//
// ⛔ The nameless user is the premise most worth checking. If a NOT NULL
// or a default ever appears on one of those columns, every assertion
// about rung 4 would silently start testing a named user instead, and
// they would all still pass.
func cbSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	cbUser(t, pool, cbLadderRef,
		strp(cbUsernameToken+"_handle"),
		strp(cbFullnameToken+" Fullname"),
		strp(cbDisplayToken+" Display"))
	// ⭐ THE NAMELESS ONE. All three columns absent, and no profile row.
	cbUser(t, pool, cbNamelessRef, nil, nil, nil)
	cbUser(t, pool, cbOtherRef, strp("cb_other_handle"), nil, nil)

	// The LADDER user owns two, so the `n DESC` order is observable and
	// the ties are not universal.
	cbAsset(t, pool, cbPhrase+" ladder one", cbLadderRef, "public")
	cbAsset(t, pool, cbPhrase+" ladder two", cbLadderRef, "public")
	cbAsset(t, pool, cbPhrase+" nameless", cbNamelessRef, "public")
	cbAsset(t, pool, cbPhrase+" other", cbOtherRef, "public")

	// ── PREMISE: the nameless user really has no stored name.
	var uname, fname, dname *string
	if err := pool.QueryRow(ctx, `
		SELECT u.username, u.fullname, p.display_name
		  FROM "user" u LEFT JOIN user_profiles p ON p.user_ref = u.ref
		 WHERE u.ref = $1`, cbNamelessRef).Scan(&uname, &fname, &dname); err != nil {
		t.Fatalf("premise read nameless user: %v", err)
	}
	if uname != nil || fname != nil || dname != nil {
		t.Fatalf("premise failed: user %d carries username=%v fullname=%v display_name=%v; "+
			"every rung-4 assertion below would silently test a NAMED user and still pass",
			cbNamelessRef, uname, fname, dname)
	}
	// ── PREMISE: rung 4 is what the product would render for it.
	if got := users.ResolveDisplayName("", nil, nil, cbNamelessRef, false); got != "user 11731852" {
		t.Fatalf("premise failed: ResolveDisplayName rung 4 rendered %q", got)
	}
}

// cbRequest is the population the lookup runs against: one phrase, one
// authenticated caller with no special capabilities.
func cbRequest(caller int64, filters ...string) (facet.Request, error) {
	sel, err := facet.ParseSelection(filters)
	if err != nil {
		return facet.Request{}, err
	}
	return facet.Request{
		QueryText: cbPhrase,
		Selection: sel,
		Caller:    visibility.NewCaller(&caller),
	}, nil
}

// cbPage runs one page and returns it.
func cbPage(t *testing.T, pool *pgxpool.Pool, q facet.ContributorQuery) facet.ContributorPage {
	t.Helper()
	page, err := facet.Contributors(context.Background(), pool, q)
	if err != nil {
		t.Fatalf("Contributors: %v", err)
	}
	return page
}

func cbRefs(rows []facet.ContributorRow) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Ref)
	}
	return out
}

func cbSortedRefs(rows []facet.ContributorRow) []int64 {
	out := cbRefs(rows)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func cbEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────

func TestContributors_EmptyPrefixBrowsesTheWholePopulation(t *testing.T) {
	pool := coPool(t)
	cbSeed(t, pool)

	req, err := cbRequest(cbLadderRef)
	if err != nil {
		t.Fatal(err)
	}
	page := cbPage(t, pool, facet.ContributorQuery{Request: req, Limit: 50})

	want := []int64{cbLadderRef, cbNamelessRef, cbOtherRef}
	if got := cbSortedRefs(page.Rows); !cbEqual(got, want) {
		t.Fatalf("empty prefix returned %v, want %v — an empty prefix is a VALID query "+
			"meaning no identity predicate at all, not a missing argument", got, want)
	}
	if page.Next != nil {
		t.Fatalf("a page holding the whole population must be TERMINAL, got cursor %+v", *page.Next)
	}
	// The busiest contributor leads: `n DESC` first, `ref ASC` to break
	// the tie between the two single-asset owners.
	if got := cbRefs(page.Rows); !cbEqual(got, []int64{cbLadderRef, cbNamelessRef, cbOtherRef}) {
		t.Fatalf("order was %v, want the two-asset owner first then ref ascending", got)
	}
}

// ⭐⭐ THE NAMELESS-USER BOUNDARY. A distinct regression from the page
// boundary: this one is about the CANDIDATE SET, not about continuation.
func TestContributors_NamelessContributorIsReachableAndLabelledByRungFour(t *testing.T) {
	pool := coPool(t)
	cbSeed(t, pool)

	req, err := cbRequest(cbLadderRef)
	if err != nil {
		t.Fatal(err)
	}

	// 1. ABSENT from a non-matching, non-empty prefix. Stated separately
	//    from "present under an empty prefix" so a lookup that returned
	//    everything regardless of the prefix cannot pass both.
	miss := cbPage(t, pool, facet.ContributorQuery{Request: req, Prefix: cbMissToken, Limit: 50})
	if len(miss.Rows) != 0 {
		t.Fatalf("prefix %q matched %v; it matches none of the three stored columns",
			cbMissToken, cbRefs(miss.Rows))
	}

	// 2. REACHED by empty-prefix browsing, ONE ROW AT A TIME, so the
	//    continuation is what does the reaching rather than a page large
	//    enough to hide the question.
	var seen []facet.ContributorRow
	var cursor *facet.ContributorCursor
	for i := 0; i < 10; i++ {
		page := cbPage(t, pool, facet.ContributorQuery{Request: req, Limit: 1, After: cursor})
		seen = append(seen, page.Rows...)
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	var nameless *facet.ContributorRow
	for i := range seen {
		if seen[i].Ref == cbNamelessRef {
			nameless = &seen[i]
		}
	}
	if nameless == nil {
		t.Fatalf("continuation over %v never reached the nameless contributor %d. "+
			"A required prefix could not have reached it either: rung 4 is INVENTED, "+
			"so the row never enters the candidate set", cbRefs(seen), cbNamelessRef)
	}

	// 3. The label is EXACTLY ADR 0070's rung-4 fallback, resolved
	//    through the one expression rather than rebuilt here.
	label := users.ResolveDisplayName(
		nameless.ProfileDisplayName, nameless.Fullname, nameless.Username, nameless.Ref, false)
	if label != "user 11731852" {
		t.Fatalf("nameless contributor rendered %q, want %q", label, "user 11731852")
	}

	// 5. Its owned asset is returned by the search the selection builds.
	//    (4 — that ticking it emits `owner:<ref>` — is the UI's half and
	//    is asserted through the real control in the dogfood suite.)
	hits := cbRunOwner(t, pool, cbNamelessRef)
	if len(hits) != 1 {
		t.Fatalf("owner:%d returned %d hits, want the one asset it owns", cbNamelessRef, len(hits))
	}
}

// cbRunOwner executes the search an `owner:` selection produces and
// returns the hit ids.
func cbRunOwner(t *testing.T, pool *pgxpool.Pool, ref int64) []string {
	t.Helper()
	sel, err := facet.ParseSelection([]string{"owner:" + itoa64(ref)})
	if err != nil {
		t.Fatalf("ParseSelection: %v", err)
	}
	caller := cbLadderRef
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          cbPhrase,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &caller,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.ID.String())
	}
	return out
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ⭐ THE THREE COLUMNS ARE ORed, NOT LADDERED.
func TestContributors_PrefixMatchesTheThreeStoredColumnsIndependently(t *testing.T) {
	pool := coPool(t)
	cbSeed(t, pool)

	req, err := cbRequest(cbLadderRef)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, prefix string
	}{
		// The winning rung. A ladder would find this one too, which is
		// why it is the CONTROL rather than the evidence.
		{"display_name (rung 1, the control)", cbDisplayToken},
		// ⛔ THE TWO THAT SEPARATE THE IMPLEMENTATIONS. This user HAS a
		// display_name, so under `COALESCE(display_name, fullname,
		// username)` the fullname and username tokens would match
		// nothing at all.
		{"fullname (rung 2, shadowed by rung 1)", cbFullnameToken},
		{"username (rung 3, shadowed by rungs 1 and 2)", cbUsernameToken},
	} {
		page := cbPage(t, pool, facet.ContributorQuery{Request: req, Prefix: c.prefix, Limit: 50})
		if got := cbRefs(page.Rows); !cbEqual(got, []int64{cbLadderRef}) {
			t.Fatalf("%s: prefix %q returned %v, want [%d]. Precedence decides WHICH NAME "+
				"TO SHOW; it does not decide whether a row MATCHES, and matching the "+
				"resolved label would rebuild ADR 0070's ladder in a fourth place",
				c.name, c.prefix, got, cbLadderRef)
		}
	}

	// Case folding, in both directions.
	upper := cbPage(t, pool, facet.ContributorQuery{
		Request: req, Prefix: "NARWHALX", Limit: 50,
	})
	if got := cbRefs(upper.Rows); !cbEqual(got, []int64{cbLadderRef}) {
		t.Fatalf("an upper-cased prefix returned %v; a person typing a name is not typing "+
			"its capitalisation", got)
	}

	// A LIKE wildcard is a literal character, not a pattern. `%` would
	// otherwise match every contributor.
	wild := cbPage(t, pool, facet.ContributorQuery{Request: req, Prefix: "%", Limit: 50})
	if len(wild.Rows) != 0 {
		t.Fatalf("prefix %q matched %v; `%%` is a character a person can type, not a wildcard "+
			"they can inject", "%", cbRefs(wild.Rows))
	}
}

// ⭐ THE PAGE BOUNDARY, on the CONCATENATION rather than on one page.
func TestContributors_CursorPagesWithoutDuplicateOrSkip(t *testing.T) {
	pool := coPool(t)
	cbSeed(t, pool)

	req, err := cbRequest(cbLadderRef)
	if err != nil {
		t.Fatal(err)
	}

	whole := cbPage(t, pool, facet.ContributorQuery{Request: req, Limit: 50})
	if len(whole.Rows) != 3 {
		t.Fatalf("fixture premise: expected 3 contributors, got %d", len(whole.Rows))
	}

	var walked []int64
	var cursor *facet.ContributorCursor
	pages := 0
	for {
		page := cbPage(t, pool, facet.ContributorQuery{Request: req, Limit: 1, After: cursor})
		pages++
		walked = append(walked, cbRefs(page.Rows)...)
		if page.Next == nil {
			break
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatalf("continuation did not terminate after %d pages", pages)
		}
	}

	if !cbEqual(walked, cbRefs(whole.Rows)) {
		t.Fatalf("paging one row at a time walked %v; one page holding everything is %v. "+
			"They must be the SAME SEQUENCE: a different order means a duplicate, a skip, "+
			"or both, and a set comparison would hide the first two", walked, cbRefs(whole.Rows))
	}
	// ⛔ TERMINAL IS OBSERVED, NOT INFERRED. A full final page is
	// indistinguishable from a full non-final page, so `len == limit`
	// would have reported "more available" here.
	if pages != 3 {
		t.Fatalf("walked %d pages over 3 contributors at one per page; a fourth page means "+
			"the terminal condition was inferred from a full page rather than observed", pages)
	}
	exact := cbPage(t, pool, facet.ContributorQuery{Request: req, Limit: 3})
	if exact.Next != nil {
		t.Fatalf("a page holding EXACTLY the whole population reported more available "+
			"(cursor %+v) — this is the `len == limit` inference", *exact.Next)
	}
}

// ⛔ SELF-EXCLUSION, through the EXISTING Selection.ForFacet.
func TestContributors_SelectingOneOwnerLeavesTheOthersSelectable(t *testing.T) {
	pool := coPool(t)
	cbSeed(t, pool)

	req, err := cbRequest(cbLadderRef, "owner:"+itoa64(cbLadderRef))
	if err != nil {
		t.Fatal(err)
	}
	page := cbPage(t, pool, facet.ContributorQuery{Request: req, Limit: 50})

	want := []int64{cbLadderRef, cbNamelessRef, cbOtherRef}
	if got := cbSortedRefs(page.Rows); !cbEqual(got, want) {
		t.Fatalf("with owner:%d already selected the lookup returned %v, want %v. "+
			"Selection.ForFacet drops an OR dimension's own terms 'so an OR dimension does "+
			"not filter itself out of existence'; without it, ticking one contributor makes "+
			"every other one unreachable and there is no way back except clearing",
			cbLadderRef, got, want)
	}

	// A filter in ANOTHER dimension is NOT self-excluded — it still
	// narrows. Without this the test above would also pass on an
	// implementation that ignored the selection entirely.
	other, err := cbRequest(cbLadderRef, "extension:zzz_no_such_extension")
	if err != nil {
		t.Fatal(err)
	}
	narrowed := cbPage(t, pool, facet.ContributorQuery{Request: other, Limit: 50})
	if len(narrowed.Rows) != 0 {
		t.Fatalf("a filter in another dimension returned %v; only the lookup's OWN dimension "+
			"is dropped, everything else still narrows the population",
			cbRefs(narrowed.Rows))
	}
}

// Visibility scoping: an owner whose only asset the caller cannot see is
// not a contributor as far as that caller is concerned.
func TestContributors_ScopedToWhatTheCallerCanSee(t *testing.T) {
	pool := coPool(t)
	cbSeed(t, pool)

	// A fourth contributor whose single asset is RESTRICTED and owned by
	// somebody else, so an ordinary caller cannot read it.
	const hiddenRef int64 = 11731854
	cbUser(t, pool, hiddenRef, strp("cb_hidden_handle"), nil, nil)
	cbAsset(t, pool, cbPhrase+" hidden", hiddenRef, "restricted")

	req, err := cbRequest(cbLadderRef)
	if err != nil {
		t.Fatal(err)
	}
	page := cbPage(t, pool, facet.ContributorQuery{Request: req, Limit: 50})
	for _, r := range page.Rows {
		if r.Ref == hiddenRef {
			t.Fatalf("contributor %d was disclosed to a caller who cannot read their only "+
				"asset. The lookup must name nobody the caller could not have reached by "+
				"paging their own results", hiddenRef)
		}
	}

	// The owner CAN see their own restricted asset, so they are a
	// contributor from their own seat — which is what proves the
	// absence above is the visibility gate rather than a broken fixture.
	own, err := cbRequest(hiddenRef)
	if err != nil {
		t.Fatal(err)
	}
	mine := cbPage(t, pool, facet.ContributorQuery{Request: own, Limit: 50})
	found := false
	for _, r := range mine.Rows {
		if r.Ref == hiddenRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("the owner of the restricted asset did not appear in their own lookup "+
			"(%v) — the previous assertion would then be vacuous", cbRefs(mine.Rows))
	}
}

func TestContributors_LimitIsBounded(t *testing.T) {
	pool := coPool(t)
	req, err := cbRequest(cbLadderRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{-1, facet.MaxContributorLimit + 1} {
		if _, err := facet.Contributors(context.Background(), pool,
			facet.ContributorQuery{Request: req, Limit: n}); err == nil {
			t.Fatalf("limit %d was accepted; a caller asking for the whole population in one "+
				"response is asking for the continuation to be optional", n)
		}
	}
}
