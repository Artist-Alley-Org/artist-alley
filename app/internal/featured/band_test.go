// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1118 — the operator promo band.
//
// Four properties, each with a CONTROL beside it, because every one of
// them is the kind of assertion that passes on a broken implementation
// when it stands alone:
//
//   1. A band card composes the viewer's read rule. Asserting "the
//      stranger cannot see the private collection" passes on a query
//      that returns nothing at all, so every arm carries a readable
//      item the stranger MUST still see (feedback: the non-mature
//      control is not filler — the same argument, one axis over).
//   2. A band whose cards all filter away renders NOTHING rather than
//      an empty band. The two are different wire answers and the
//      difference is the whole collapse rule.
//   3. Band cards do not leak onto the featured rail, and rail
//      placements do not appear in the band. The surfaces are disjoint
//      and neither is a subset of the other.
//   4. The mature axis applies to a band card exactly as it applies
//      everywhere else — including the pre-existing gap on the rail
//      that #1118 closed, which is asserted here because the shared
//      query is what makes the two agree.
//
// Skips without AA_DB_PASSWORD, same convention as rail_test.go, and
// reuses its fixtures (railAsset, railCollection, railPool, railOwner).

package featured

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// makeBand inserts a band and registers its cleanup. The cards cascade.
func makeBand(t *testing.T, pool *pgxpool.Pool, scope string, enabled bool, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO promo_bands (id, title, blurb, enabled, after_page, scope)
		VALUES ($1,$2,'',$3,1,$4)`, id, title, enabled, scope)
	if err != nil {
		t.Fatalf("seed band: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM promo_bands WHERE id=$1`, id)
	})
	return id
}

// card adds one placement to a band.
func card(t *testing.T, pool *pgxpool.Pool, band uuid.UUID, kind string, subject uuid.UUID, pos int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO featured_items (subject_kind, subject_id, position, band_id)
		VALUES ($1,$2,$3,$4)`, kind, subject, pos, band)
	if err != nil {
		t.Fatalf("card %s %s: %v", kind, subject, err)
	}
}

func bandTitles(t *testing.T, pool *pgxpool.Pool, band uuid.UUID, caller visibility.Caller) map[string]bool {
	t.Helper()
	rows, err := ListPlacements(context.Background(), pool, PlacementQuery{
		Caller: caller, Limit: 500, Ladder: defaultLadder, BandID: &band,
	})
	if err != nil {
		t.Fatalf("ListPlacements(band): %v", err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Title] = true
	}
	return out
}

// TestBandCardComposesTheViewersReadRule is the #902 pair, on the band.
//
// ⚠️ THE CONTROL IS LOAD-BEARING. "The stranger cannot see the private
// collection" is satisfied by a query that returns an empty list for
// every caller — by the band being broken. The readable asset in the
// same band is what separates "the rule applied" from "nothing worked".
func TestBandCardComposesTheViewersReadRule(t *testing.T) {
	pool := railPool(t)

	visible := railAsset(t, pool, "band-visible-1118", "active", "public", "ready")
	// Private: the owner sees it, a stranger does not (ADR 0063).
	withheld := railCollection(t, pool, "band-withheld-1118", "private")

	band := makeBand(t, pool, ScopePublic, true, "Winter showcase")
	card(t, pool, band, "asset", visible, 0)
	card(t, pool, band, "collection", withheld, 1)

	owner := railOwner
	ownerSees := bandTitles(t, pool, band, visibility.NewCaller(&owner))
	if !ownerSees["band-visible-1118"] {
		t.Fatal("the owner could not see the PUBLIC card in their own band — " +
			"the band read is broken, and every withholding assertion below would " +
			"pass vacuously on that. Fix this before reading the rest of this test.")
	}
	if !ownerSees["band-withheld-1118"] {
		t.Error("the owner could not see their own private collection as a band card; " +
			"the band composes the read rule, and the owner passes it")
	}

	stranger := int64(41700099)
	strangerSees := bandTitles(t, pool, band, visibility.NewCaller(&stranger))
	if !strangerSees["band-visible-1118"] {
		t.Error("the CONTROL card vanished for a stranger. Without it, the " +
			"assertion below cannot tell 'the rule applied' from 'the band " +
			"returned nothing'")
	}
	if strangerSees["band-withheld-1118"] {
		t.Error("a private collection rendered as a band card for a stranger — " +
			"a placement is a SELECTION over what the caller can already see, " +
			"never a grant (ADR 0065)")
	}

	anon := bandTitles(t, pool, band, visibility.NewCaller(nil))
	if !anon["band-visible-1118"] {
		t.Error("the CONTROL card vanished for an anonymous reader")
	}
	if anon["band-withheld-1118"] {
		t.Error("a private collection rendered as a band card for an anonymous reader")
	}
}

// TestBandCollapsesWhenEveryCardFiltersAway pins the ADR 0030 rule.
//
// Not "the band comes back with zero items" — the band does not come
// back AT ALL. A client handed an empty band renders a headline and a
// button over nothing, which is the failure the collapse exists to
// prevent, and it is a failure a length-of-items assertion cannot see.
func TestBandCollapsesWhenEveryCardFiltersAway(t *testing.T) {
	pool := railPool(t)

	withheld := railCollection(t, pool, "band-collapse-1118", "private")
	band := makeBand(t, pool, ScopePublic, true, "Nothing to see")
	card(t, pool, band, "collection", withheld, 0)

	owner := railOwner
	// CONTROL: the owner CAN see the only card, so the band renders for
	// them. Without this the assertion below passes on a band that
	// collapsed for everybody — including because it was disabled, or
	// because RenderableBand is broken.
	got, err := RenderableBand(context.Background(), pool, PlacementQuery{
		Caller: visibility.NewCaller(&owner), Limit: 500, Ladder: defaultLadder,
	})
	if err != nil {
		t.Fatalf("RenderableBand(owner): %v", err)
	}
	if got == nil || len(got.Items) != 1 {
		t.Fatalf("the owner should see a band with exactly its one card; got %+v", got)
	}

	stranger := int64(41700099)
	got, err = RenderableBand(context.Background(), pool, PlacementQuery{
		Caller: visibility.NewCaller(&stranger), Limit: 500, Ladder: defaultLadder,
	})
	if err != nil {
		t.Fatalf("RenderableBand(stranger): %v", err)
	}
	if got != nil {
		t.Errorf("a band whose every card filtered away came back as %+v. It must be "+
			"NOTHING, not an empty band: the collapse decision belongs beside the "+
			"filter that produced the emptiness, or every client has to re-implement "+
			"it and one of them will forget", got)
	}
}

// TestDisabledBandRendersNothing — the operator's switch is a filter,
// not a flag for the client to honour.
func TestDisabledBandRendersNothing(t *testing.T) {
	pool := railPool(t)

	visible := railAsset(t, pool, "band-disabled-1118", "active", "public", "ready")
	band := makeBand(t, pool, ScopePublic, false, "Switched off")
	card(t, pool, band, "asset", visible, 0)

	owner := railOwner
	got, err := RenderableBand(context.Background(), pool, PlacementQuery{
		Caller: visibility.NewCaller(&owner), Limit: 500, Ladder: defaultLadder,
	})
	if err != nil {
		t.Fatalf("RenderableBand: %v", err)
	}
	if got != nil {
		t.Errorf("a DISABLED band rendered: %+v", got)
	}

	// CONTROL: flip it on and the same band renders. Without this the
	// assertion above passes on a band nobody could ever see.
	if _, err := pool.Exec(context.Background(),
		`UPDATE promo_bands SET enabled = TRUE WHERE id = $1`, band); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, err = RenderableBand(context.Background(), pool, PlacementQuery{
		Caller: visibility.NewCaller(&owner), Limit: 500, Ladder: defaultLadder,
	})
	if err != nil {
		t.Fatalf("RenderableBand(enabled): %v", err)
	}
	if got == nil {
		t.Error("the same band did not render once enabled — the disabled " +
			"assertion above was therefore not about `enabled` at all")
	}
}

// TestBandAudienceIsTheBandsOwn: an `org` band is invisible to an
// anonymous reader and visible to a signed-in one, and the CARDS' own
// scope column plays no part in either answer.
//
// The second half is the interesting one. Band cards are written with
// the table's default scope ('org'), so a reader who consulted the card
// scope would get the right answer here by ACCIDENT. The band is
// therefore flipped to `public` while its cards keep their default: if
// the card scope were the gate, the anonymous arm would still be empty.
func TestBandAudienceIsTheBandsOwn(t *testing.T) {
	pool := railPool(t)

	visible := railAsset(t, pool, "band-audience-1118", "active", "public", "ready")
	band := makeBand(t, pool, ScopeOrg, true, "Members only")
	card(t, pool, band, "asset", visible, 0)

	anonQ := PlacementQuery{Caller: visibility.NewCaller(nil), Limit: 500, Ladder: defaultLadder}
	got, err := RenderableBand(context.Background(), pool, anonQ)
	if err != nil {
		t.Fatalf("RenderableBand(anon, org): %v", err)
	}
	if got != nil {
		t.Errorf("an `org` band rendered for an anonymous reader: %+v", got)
	}

	owner := railOwner
	got, err = RenderableBand(context.Background(), pool, PlacementQuery{
		Caller: visibility.NewCaller(&owner), Limit: 500, Ladder: defaultLadder,
	})
	if err != nil {
		t.Fatalf("RenderableBand(signed in, org): %v", err)
	}
	if got == nil {
		t.Fatal("an `org` band did not render for a signed-in reader — the " +
			"anonymous assertion above was therefore not about audience")
	}

	// Widen the BAND only. Its card keeps scope='org' (the column
	// default), so this is the arm that fails if the card's scope is
	// being read as the audience.
	if _, err := pool.Exec(context.Background(),
		`UPDATE promo_bands SET scope = 'public' WHERE id = $1`, band); err != nil {
		t.Fatalf("widen: %v", err)
	}
	got, err = RenderableBand(context.Background(), pool, anonQ)
	if err != nil {
		t.Fatalf("RenderableBand(anon, public): %v", err)
	}
	if got == nil {
		t.Error("a `public` band did not render for an anonymous reader. Its CARD " +
			"still carries scope='org' (the column default) — so this is what " +
			"fails if the card's scope is being read as the band's audience, " +
			"which is the denormalised copy migration 00053 refuses to keep")
	}
}

// TestBandCardsAndRailAreDisjoint — the two surfaces do not bleed.
//
// The rail direction is the one that mattered enough to fail loudly:
// band cards carry the table's DEFAULT scope, which the signed-in arm of
// ScopeVisibleSQL admits, so a rail query without `band_id IS NULL`
// publishes every band card on the landing page the moment a band is
// curated.
func TestBandCardsAndRailAreDisjoint(t *testing.T) {
	pool := railPool(t)

	onRail := railAsset(t, pool, "band-rail-only-1118", "active", "public", "ready")
	inBand := railAsset(t, pool, "band-band-only-1118", "active", "public", "ready")

	place(t, pool, "asset", onRail, ScopePublic, 0)
	band := makeBand(t, pool, ScopePublic, true, "Disjoint")
	card(t, pool, band, "asset", inBand, 0)

	owner := railOwner
	rail := railTitles(t, pool, visibility.NewCaller(&owner))
	if !rail["band-rail-only-1118"] {
		t.Fatal("the rail lost its own placement; nothing below is meaningful")
	}
	if rail["band-band-only-1118"] {
		t.Error("a BAND CARD appeared on the featured rail. Band rows carry the " +
			"scope column's default, which the signed-in arm of ScopeVisibleSQL " +
			"admits — so without the band_id IS NULL conjunct the rail publishes " +
			"the operator's band on the anonymous landing page")
	}

	inBandTitles := bandTitles(t, pool, band, visibility.NewCaller(&owner))
	if !inBandTitles["band-band-only-1118"] {
		t.Fatal("the band lost its own card; the assertion below is not meaningful")
	}
	if inBandTitles["band-rail-only-1118"] {
		t.Error("a RAIL placement appeared as a band card")
	}
}

// TestBandAdminListIsScopedToItsSurface — the operator's two curation
// lists are disjoint too, for the same reason and through a different
// query (sqlc's ListFeaturedItems rather than the hand-built read).
func TestBandAdminListIsScopedToItsSurface(t *testing.T) {
	pool := railPool(t)

	onRail := railAsset(t, pool, "band-admin-rail-1118", "active", "public", "ready")
	inBand := railAsset(t, pool, "band-admin-card-1118", "active", "public", "ready")
	place(t, pool, "asset", onRail, ScopeOrg, 0)
	band := makeBand(t, pool, ScopeOrg, true, "Admin scoping")
	card(t, pool, band, "asset", inBand, 0)

	q := New(pool)
	railRows, err := q.ListFeaturedItems(context.Background(), ListFeaturedItemsParams{Ladder: defaultLadder})
	if err != nil {
		t.Fatalf("ListFeaturedItems(rail): %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range railRows {
		seen[uuid.UUID(r.SubjectID.Bytes)] = true
	}
	if !seen[onRail] {
		t.Fatal("the rail curation list lost its own row")
	}
	if seen[inBand] {
		t.Error("a band card appeared in the RAIL's curation list — an operator " +
			"would be offered a remove button for a row that is not on the " +
			"surface they are looking at")
	}
}

// TestMatureCardIsWithheldFromADisqualifiedReader.
//
// ⚠️ This closes a gap that predates the band. The featured surfaces
// composed the ADR 0063 tier predicate and NOT the ADR 0090 mature
// conjunct, so an operator who featured a mature asset published it to
// every reader — including one who never opted in. The band shares the
// rail's query, so the fix lands on both, and this test drives the BAND
// arm while TestRailWithholdsMatureFromADisqualifiedReader drives the
// rail's.
//
// The non-mature control is not filler: the mature rule is a FILTER OVER
// MATURE THINGS, and a predicate that consulted the viewer's opt-in for
// every item would hide the whole library from an opted-out reader while
// passing every assertion about mature items.
func TestMatureCardIsWithheldFromADisqualifiedReader(t *testing.T) {
	pool := railPool(t)

	plain := railAsset(t, pool, "band-plain-1118", "active", "public", "ready")
	spicy := railAsset(t, pool, "band-mature-1118", "active", "public", "ready")
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = TRUE WHERE id = $1`, spicy); err != nil {
		t.Fatalf("flag mature: %v", err)
	}

	band := makeBand(t, pool, ScopePublic, true, "Mature discipline")
	card(t, pool, band, "asset", plain, 0)
	card(t, pool, band, "asset", spicy, 1)

	// A stranger, not the owner: the owner exemption would mask the
	// filter entirely (ADR 0090 checks ownership BEFORE qualification).
	stranger := int64(41700099)
	disqualified := PlacementQuery{
		Caller: visibility.NewCaller(&stranger),
		Limit:  500, Ladder: defaultLadder, BandID: &band,
		// Zero value = the disqualified viewer.
		Mature: visibility.MatureViewer{},
	}
	got := map[string]bool{}
	rows, err := ListPlacements(context.Background(), pool, disqualified)
	if err != nil {
		t.Fatalf("ListPlacements(disqualified): %v", err)
	}
	for _, r := range rows {
		got[r.Title] = true
	}
	if !got["band-plain-1118"] {
		t.Error("a NON-MATURE card was hidden from an opted-out reader. The mature " +
			"rule is a filter over MATURE things; a predicate that consults the " +
			"opt-in for every item hides the whole library and still passes every " +
			"assertion below")
	}
	if got["band-mature-1118"] {
		t.Error("a MATURE card rendered for a reader who has not opted in. " +
			"visibility.Filter answers who is ALLOWED and says nothing about who " +
			"OPTED IN — the two axes are independent (ADR 0090 §1), so composing " +
			"the tier predicate alone is not enough on any list surface")
	}

	qualified := disqualified
	qualified.Mature = visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}
	got = map[string]bool{}
	rows, err = ListPlacements(context.Background(), pool, qualified)
	if err != nil {
		t.Fatalf("ListPlacements(qualified): %v", err)
	}
	for _, r := range rows {
		got[r.Title] = true
	}
	if !got["band-mature-1118"] {
		t.Error("a QUALIFIED reader could not see the mature card — the assertion " +
			"above was therefore not about the mature axis at all. This arm also " +
			"exercises MatureFilterSQL's empty-string return, where binding the " +
			"owner placeholder unconditionally raises 42P18")
	}
	if !got["band-plain-1118"] {
		t.Error("a qualified reader lost the non-mature card")
	}
}

// TestRailWithholdsMatureFromADisqualifiedReader — the same rule, the
// same query, the other surface. Kept as its own test rather than a
// table arm because the two surfaces reach ListPlacements by different
// routes (a band id versus a scope predicate) and a shared helper would
// hide which one regressed.
func TestRailWithholdsMatureFromADisqualifiedReader(t *testing.T) {
	pool := railPool(t)

	plain := railAsset(t, pool, "rail-plain-1118", "active", "public", "ready")
	spicy := railAsset(t, pool, "rail-mature-1118", "active", "public", "ready")
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = TRUE WHERE id = $1`, spicy); err != nil {
		t.Fatalf("flag mature: %v", err)
	}
	place(t, pool, "asset", plain, ScopePublic, 0)
	place(t, pool, "asset", spicy, ScopePublic, 1)

	stranger := int64(41700099)
	rows, err := ListPlacements(context.Background(), pool, PlacementQuery{
		Caller: visibility.NewCaller(&stranger), Limit: 500, Ladder: defaultLadder,
	})
	if err != nil {
		t.Fatalf("ListPlacements: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Title] = true
	}
	if !got["rail-plain-1118"] {
		t.Error("a non-mature placement was hidden from an opted-out reader")
	}
	if got["rail-mature-1118"] {
		t.Error("a MATURE asset rendered on the featured rail for a reader who has " +
			"not opted in (#1118 closed this; it predates the band)")
	}
}

// TestBandCTAValidation — the write path refuses what the CHECK
// constraint would refuse, with a named error rather than a 23514.
//
// The `javascript:` case is the one that matters: the value becomes an
// href on the browse page of every reader, and Svelte does not sanitise
// hrefs.
func TestBandCTAValidation(t *testing.T) {
	cases := []struct {
		name  string
		in    BandInput
		wantE error
	}{
		{"javascript url", BandInput{Title: "x", AfterPage: 1, CTALabel: "Go", CTAURL: "javascript:alert(1)"}, ErrBadCTA},
		{"data url", BandInput{Title: "x", AfterPage: 1, CTALabel: "Go", CTAURL: "data:text/html,<script>"}, ErrBadCTA},
		{"scheme relative", BandInput{Title: "x", AfterPage: 1, CTALabel: "Go", CTAURL: "//evil.example/x"}, ErrBadCTA},
		{"label without url", BandInput{Title: "x", AfterPage: 1, CTALabel: "Go"}, ErrBadCTA},
		{"url without label", BandInput{Title: "x", AfterPage: 1, CTAURL: "https://example.org"}, ErrBadCTA},
		{"page zero", BandInput{Title: "x", AfterPage: 0}, ErrBadAfterPage},
		{"team scope", BandInput{Title: "x", AfterPage: 1, Scope: ScopeTeam}, ErrBandScopeNotWritable},
		{"https ok", BandInput{Title: "x", AfterPage: 1, CTALabel: "Go", CTAURL: "https://example.org/x"}, nil},
		{"site relative ok", BandInput{Title: "x", AfterPage: 1, CTALabel: "Go", CTAURL: "/collections/abc"}, nil},
		{"no cta ok", BandInput{Title: "x", AfterPage: 1}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			err := validateBand(&in)
			if c.wantE == nil && err != nil {
				t.Fatalf("wanted accepted, got %v", err)
			}
			if c.wantE != nil && err == nil {
				t.Fatalf("wanted %v, got accepted — an operator-supplied href reaching "+
					"the browse page unchecked is stored XSS", c.wantE)
			}
		})
	}
}
