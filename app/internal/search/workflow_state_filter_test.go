// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 18c — WORKFLOW-STATE FILTERING, ASSERTED AGAINST REAL ROWS.
//
// facet/workflow_state_test.go proves the grammar, the classification
// and the rendered SQL. What is here is the half that can only be
// checked against ROWS, and every assertion is a SET OF EXACT IDS
// rather than a count.
//
// ⛔ A COUNT IS NOT ENOUGH HERE AND THE FIXTURE IS BUILT AROUND THAT.
// Asking for two states must return the union. A count assertion is
// satisfied by three wrong implementations as well as by the right one:
//
//	correct OR        {A, B, C}
//	accidental AND    {}
//	first value wins  {A, B}
//	last value wins   {C}
//
// Only exact ids separate all four, which is why every population below
// is named and compared by id, and why X — a state nobody asks for —
// exists at all.
//
// ⛔ THE SEEDED CORPUS IS NOT AN ORACLE. `seed/runner.go`'s
// resolveAssetState maps both `approved` and `final` onto `published`
// and anything unknown onto `published`, so its per-state totals are
// dogfood evidence and nothing more. Every number in this file comes
// from a row this file inserted.
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
	"github.com/mscrnt/artist-alley/app/internal/workflow"
)

const (
	wsOwner int64 = 11731802

	// Each fixture gets its OWN nonsense phrase so its result set is
	// attributable to it alone and never to a neighbouring fixture. The
	// deleted-state and misfiled-domain fixtures in particular must not
	// see each other's rows: both make claims of the form "exactly
	// these ids and nothing else".
	wsPhrase      = "brindlewax"
	wsSlashPhrase = "brindlewax cleftmar"
	wsMisPhrase   = "brindlewax offdomain"
	wsDelPhrase   = "brindlewax vanishing"

	// State codes. Deliberately not real vocabulary — a test that used
	// `draft` or `published` would pass or fail depending on what the
	// seeder happened to have written.
	wsCodeS1 = "ws_s1_alpha"
	wsCodeS2 = "ws_s2_beta"
	wsCodeS3 = "ws_s3_gamma"
	// ⭐ THE PAIR THAT SEPARATES A FIRST-SLASH SPLIT FROM ANY OTHER.
	// `ws_stage` is a PREFIX of `ws_stage/final`, so an implementation
	// that split the identity on EVERY slash would look up code
	// `ws_stage` and return the WRONG asset rather than none.
	wsCodeStage      = "ws_stage"
	wsCodeStageFinal = "ws_stage/final"
	wsCodeDeleted    = "ws_doomed"
)

// wsFixture is the controlled population, by exact id.
type wsFixture struct {
	A, B string // carry S1
	C    string // carries S2
	N    string // state_id IS NULL
	X    string // carries S3, outside every requested set
	// A post and a collection carrying the phrase, so "exactly zero
	// posts" is a claim about rows that WOULD otherwise have matched.
	// Without them the mixed-arm assertion is vacuous — a mutation of
	// 18b's suite proved exactly that.
	Post, Collection string

	Domain string // the asset domain these states live in
}

func (f wsFixture) all() []string {
	out := []string{f.A, f.B, f.C, f.N, f.X}
	sort.Strings(out)
	return out
}

func wsSorted(ids ...string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

// wsAssetDomain returns the workflow domain of the asset type the
// fixtures use, through the SAME function the product uses to build it.
// A hand-written "asset:1" would be a second copy of that convention.
func wsAssetDomain(t *testing.T, pool *pgxpool.Pool) (string, int64) {
	t.Helper()
	var ref int64
	if err := pool.QueryRow(context.Background(),
		`SELECT MIN(ref) FROM asset_types`).Scan(&ref); err != nil {
		t.Fatalf("asset type ref: %v", err)
	}
	return workflow.AssetDomain(ref), ref
}

// wsState inserts one workflow state and returns its id, registering the
// cleanup that removes it.
func wsState(t *testing.T, pool *pgxpool.Pool, domain, code string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO workflow_states (id, domain, code, label, sort_order)
		VALUES ($1, $2, $3, $3, 0)`, id, domain, code); err != nil {
		t.Fatalf("seed state %s/%s: %v", domain, code, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_states WHERE id = $1`, id)
	})
	return id
}

// wsAsset inserts one searchable asset, optionally carrying a state.
func wsAsset(t *testing.T, pool *pgxpool.Pool, title string, state *uuid.UUID) string {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, state_id)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready',$4)`,
		id, title, wsOwner, state); err != nil {
		t.Fatalf("seed asset %q: %v", title, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id.String()
}

// wsSeed plants the controlled fixture and PROVES ITS OWN PREMISES.
//
// ⛔ A state can be DEFINED and never POPULATED — `deleted` is exactly
// that in the shipped vocabulary today — so "the query returned nothing"
// and "the fixture was never built" look identical. Both halves are
// asserted here rather than assumed downstream.
func wsSeed(t *testing.T, pool *pgxpool.Pool) wsFixture {
	t.Helper()
	ctx := context.Background()

	domain, _ := wsAssetDomain(t, pool)
	s1 := wsState(t, pool, domain, wsCodeS1)
	s2 := wsState(t, pool, domain, wsCodeS2)
	s3 := wsState(t, pool, domain, wsCodeS3)

	fx := wsFixture{Domain: domain}
	fx.A = wsAsset(t, pool, wsPhrase+" alpha one", &s1)
	fx.B = wsAsset(t, pool, wsPhrase+" alpha two", &s1)
	fx.C = wsAsset(t, pool, wsPhrase+" beta", &s2)
	fx.N = wsAsset(t, pool, wsPhrase+" stateless", nil)
	fx.X = wsAsset(t, pool, wsPhrase+" gamma", &s3)

	postID, collID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`, postID, wsOwner, wsPhrase+" post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`, collID, wsOwner, wsPhrase+" collection"); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, collID)
	})
	fx.Post, fx.Collection = postID.String(), collID.String()

	// ── PREMISE 1: the states exist, under the identity the filter
	// spells them with.
	for _, code := range []string{wsCodeS1, wsCodeS2, wsCodeS3} {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM workflow_states WHERE domain = $1 AND code = $2`,
			domain, code).Scan(&n); err != nil {
			t.Fatalf("premise query: %v", err)
		}
		if n != 1 {
			t.Fatalf("premise failed: %s/%s has %d rows in workflow_states, want 1 — "+
				"every zero-result assertion below would pass on an unbuilt fixture",
				domain, code, n)
		}
	}
	// ── PREMISE 2: the assets actually CARRY them. A defined-but-unused
	// state makes every "returns zero" assertion vacuous.
	for _, c := range []struct {
		id   string
		want uuid.UUID
		name string
	}{
		{fx.A, s1, "A"}, {fx.B, s1, "B"}, {fx.C, s2, "C"}, {fx.X, s3, "X"},
	} {
		var got *uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT state_id FROM assets WHERE id = $1`,
			uuid.MustParse(c.id)).Scan(&got); err != nil {
			t.Fatalf("premise read %s: %v", c.name, err)
		}
		if got == nil || *got != c.want {
			t.Fatalf("premise failed: asset %s carries state %v, want %v", c.name, got, c.want)
		}
	}
	var nState *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT state_id FROM assets WHERE id = $1`,
		uuid.MustParse(fx.N)).Scan(&nState); err != nil {
		t.Fatalf("premise read N: %v", err)
	}
	if nState != nil {
		t.Fatalf("premise failed: asset N carries state %v, want NULL", *nState)
	}
	return fx
}

// wsTerm spells one workflow-state filter the way a URL carries it.
func wsTerm(v string) string { return "workflow_state:" + v }

func wsIdentity(domain, code string) string { return domain + "/" + code }

// wsRun executes one asset-scoped search over a fixture phrase and
// returns the sorted hit ids.
func wsRun(t *testing.T, pool *pgxpool.Pool, phrase string, filters ...string) []string {
	t.Helper()
	sel, err := facet.ParseSelection(filters)
	if err != nil {
		t.Fatalf("ParseSelection(%v): %v — these are well-formed filters; a rejection\n"+
			"  here is the PURE validity class firing, which is a different contract",
			filters, err)
	}
	return wsRunSelection(t, pool, phrase, sel)
}

func wsRunSelection(t *testing.T, pool *pgxpool.Pool, phrase string, sel facet.Selection) []string {
	t.Helper()
	ref := wsOwner
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Text:          phrase,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		Filters:       sel,
		CallerUserRef: &ref,
	})
	if err != nil {
		t.Fatalf("Run(%v): %v", sel.Params(), err)
	}
	if res.TotalCount != len(res.Hits) {
		t.Errorf("total_count %d but %d hits for %v — the count is a derived copy of the\n"+
			"  result set, so an operator that narrowed one and not the other turns the\n"+
			"  count into an oracle the hits are not", res.TotalCount, len(res.Hits), sel.Params())
	}
	return hitIDs(res)
}

// wsAssertSet compares sorted id slices in both directions, so a
// widening and a narrowing are distinguishable in the failure.
func wsAssertSet(t *testing.T, what string, got, want []string, why string) {
	t.Helper()
	if sameIDs(got, want) {
		return
	}
	inGot, inWant := map[string]bool{}, map[string]bool{}
	for _, g := range got {
		inGot[g] = true
	}
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

// TestWorkflowStateFilter_ValuesOr is the deterministic table, and the
// S1+S2 row is the discriminator the whole fixture exists for.
func TestWorkflowStateFilter_ValuesOr(t *testing.T) {
	pool := coPool(t)
	fx := wsSeed(t, pool)

	s1 := wsTerm(wsIdentity(fx.Domain, wsCodeS1))
	s2 := wsTerm(wsIdentity(fx.Domain, wsCodeS2))
	none := wsTerm(facet.WorkflowStateNone)

	// N=0. Identical to today, and the premise for everything below.
	wsAssertSet(t, "the unfiltered fixture", wsRun(t, pool, wsPhrase), fx.all(),
		"if this is wrong the fixture is not isolated and nothing below means anything")

	wsAssertSet(t, "S1", wsRun(t, pool, wsPhrase, s1), wsSorted(fx.A, fx.B),
		"two assets carry S1; X carries a state nobody asked for and must never appear")
	wsAssertSet(t, "S2", wsRun(t, pool, wsPhrase, s2), wsSorted(fx.C),
		"one asset carries S2")

	// ⛔ THE ONE THAT MATTERS.
	wsAssertSet(t, "S1 + S2", wsRun(t, pool, wsPhrase, s1, s2), wsSorted(fx.A, fx.B, fx.C),
		"an asset holds exactly ONE state, so two values are a VALUE LIST and OR.\n"+
			"  Accidental AND returns nothing; first-value-wins returns {A,B};\n"+
			"  last-value-wins returns {C}. Only exact ids separate all three from the\n"+
			"  correct union, which is why this is not a count")

	wsAssertSet(t, "none", wsRun(t, pool, wsPhrase, none), wsSorted(fx.N),
		"the reserved literal selects assets whose state_id IS NULL, and nothing else")
	wsAssertSet(t, "S1 + none", wsRun(t, pool, wsPhrase, s1, none),
		wsSorted(fx.A, fx.B, fx.N),
		"a concrete identity beside `none` is the same OR — an asset either carries\n"+
			"  that state or carries none")

	// The state nobody asked for, asked for. A counterweight: an
	// implementation that returned everything would satisfy the unions
	// above but not this.
	wsAssertSet(t, "S3", wsRun(t, pool, wsPhrase, wsTerm(wsIdentity(fx.Domain, wsCodeS3))),
		wsSorted(fx.X), "X exists and is reachable; it is simply outside every set above")
}

// TestWorkflowStateFilter_NonAssetDomainMatchesTheMisfiledAsset.
//
// ⛔ THE ASSERTION IS A MATCH, NOT A ZERO, and that is the whole point.
// `assets`' create path does not check that the state it writes belongs
// to the `asset:<asset_type>` domain — it defers that to Transition() —
// so an asset genuinely can carry a `post` state. This fixture builds
// exactly that row and requires the filter to FIND it by id.
//
// ⭐ It distinguishes the accepted implementation from four wrong ones:
// validating `domain LIKE 'asset:%'`; rejecting `post/published`
// outright; accepting it and forcing zero; or otherwise hiding the
// corrupted row. A count-only or "normally zero" assertion passes on all
// four.
func TestWorkflowStateFilter_NonAssetDomainMatchesTheMisfiledAsset(t *testing.T) {
	pool := coPool(t)
	ctx := context.Background()
	domain, _ := wsAssetDomain(t, pool)

	// PREMISE: a REAL non-asset state. `post`/`published` is created by
	// migration 00059 and is the post domain's entry point; this test
	// reads it rather than inventing one, so the identity it asks for is
	// one the product actually holds.
	var postPublished uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM workflow_states WHERE domain = 'post' AND code = 'published'`,
	).Scan(&postPublished); err != nil {
		t.Fatalf("premise failed: no post/published state (%v) — migration 00059 creates "+
			"it, so its absence means this database is not the one the product runs on", err)
	}

	assetState := wsState(t, pool, domain, wsCodeS1)
	// ⛔ THE CORRUPTED ROW: an ASSET carrying a POST state's uuid.
	misfiled := wsAsset(t, pool, wsMisPhrase+" misfiled", &postPublished)
	control := wsAsset(t, pool, wsMisPhrase+" ordinary", &assetState)

	// PREMISE: the write actually landed. Without this, "the filter
	// found nothing" and "the row was never misfiled" are the same
	// observation.
	var got uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT state_id FROM assets WHERE id = $1`,
		uuid.MustParse(misfiled)).Scan(&got); err != nil || got != postPublished {
		t.Fatalf("premise failed: the misfiled asset carries %v (err %v), want %v",
			got, err, postPublished)
	}

	wsAssertSet(t, "workflow_state:post/published", wsRun(t, pool, wsMisPhrase,
		wsTerm("post/published")), wsSorted(misfiled),
		"⛔ THE MISFILED ASSET MUST BE FOUND BY ID. A domain whitelist, an outright\n"+
			"  rejection, or an accept-then-force-zero all return the empty set here and\n"+
			"  hide a corrupted row that the filter exists to surface")

	// The counterweight: the ordinary asset-domain identity still works,
	// so the assertion above is not satisfied by a filter that returns
	// everything or by one that returns whatever it is asked for.
	wsAssertSet(t, "the asset-domain control", wsRun(t, pool, wsMisPhrase,
		wsTerm(wsIdentity(domain, wsCodeS1))), wsSorted(control),
		"the ordinary identity selects the ordinary asset and NOT the misfiled one")
}

// TestWorkflowStateFilter_UnknownIdentityIsAcceptedAndEmpty.
//
// ⛔ A DIFFERENT CONTRACT FROM THE TEST ABOVE, and the two must not be
// collapsed. There, a well-formed identity that EXISTS matches its row.
// Here, a well-formed identity that exists NOWHERE is still ACCEPTED —
// not 400, not a DSL error — and matches zero.
func TestWorkflowStateFilter_UnknownIdentityIsAcceptedAndEmpty(t *testing.T) {
	pool := coPool(t)
	fx := wsSeed(t, pool)

	const unknown = "asset:99999/ws_never_defined"
	// PREMISE: it really does not exist.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM workflow_states WHERE domain || '/' || code = $1`,
		unknown).Scan(&n); err != nil {
		t.Fatalf("premise query: %v", err)
	}
	if n != 0 {
		t.Fatalf("premise failed: %q exists (%d rows); pick an identity that does not", unknown, n)
	}
	// PREMISE: the corpus is non-empty, so "zero" is a decision rather
	// than an empty database.
	wsAssertSet(t, "the unfiltered fixture", wsRun(t, pool, wsPhrase), fx.all(),
		"the rows the unknown identity has to decline")

	// ACCEPTED on the wire path.
	if _, err := facet.ParseSelection([]string{wsTerm(unknown)}); err != nil {
		t.Fatalf("ParseSelection refused %q (%v) — a well-formed identity naming no row\n"+
			"  must be accepted. CanonicalValue is pure and cannot check existence, and\n"+
			"  #897 lets an operator delete a state: rejection would make a stored query\n"+
			"  stop parsing the moment somebody renamed one", unknown, err)
	}
	// ACCEPTED on the DSL path too — the same value must survive a save.
	in, err := facet.ParseSelection([]string{wsTerm(unknown)})
	if err != nil {
		t.Fatal(err)
	}
	out, text := throughDSL(t, in)
	if in.CacheKey() != out.CacheKey() {
		t.Errorf("an unknown identity did not survive the DSL round trip via %q:\n"+
			"  in  %v\n  out %v", text, in.Params(), out.Params())
	}
	// And matches nothing.
	wsAssertSet(t, wsTerm(unknown), wsRun(t, pool, wsPhrase, wsTerm(unknown)), []string{},
		"an identity naming no row matches zero — applied, and satisfied by nothing")
}

// TestWorkflowStateFilter_SplitsAtTheFirstSlash.
//
// ⭐ THE FIXTURE IS THE DISCRIMINATOR. `ws_stage` and `ws_stage/final`
// are two real states, and `ws_stage` is a PREFIX of the other. An
// implementation that split the identity on every slash — the obvious
// `split_part(v, '/', 2)` — would resolve `…/ws_stage/final` to code
// `ws_stage` and return the WRONG asset rather than none, which a
// "returns something" assertion would not catch.
func TestWorkflowStateFilter_SplitsAtTheFirstSlash(t *testing.T) {
	pool := coPool(t)
	domain, _ := wsAssetDomain(t, pool)

	stage := wsState(t, pool, domain, wsCodeStage)
	final := wsState(t, pool, domain, wsCodeStageFinal)
	stageAsset := wsAsset(t, pool, wsSlashPhrase+" stage", &stage)
	finalAsset := wsAsset(t, pool, wsSlashPhrase+" final", &final)

	wsAssertSet(t, wsTerm(wsIdentity(domain, wsCodeStageFinal)),
		wsRun(t, pool, wsSlashPhrase, wsTerm(wsIdentity(domain, wsCodeStageFinal))),
		wsSorted(finalAsset),
		"⛔ the code is EVERYTHING after the first slash. Splitting on every slash\n"+
			"  truncates it to `ws_stage` and returns the other asset instead")
	wsAssertSet(t, wsTerm(wsIdentity(domain, wsCodeStage)),
		wsRun(t, pool, wsSlashPhrase, wsTerm(wsIdentity(domain, wsCodeStage))),
		wsSorted(stageAsset),
		"and the prefix state still resolves to its own asset alone — the two are\n"+
			"  different identities and neither may bleed into the other")
	wsAssertSet(t, "both codes",
		wsRun(t, pool, wsSlashPhrase,
			wsTerm(wsIdentity(domain, wsCodeStage)),
			wsTerm(wsIdentity(domain, wsCodeStageFinal))),
		wsSorted(stageAsset, finalAsset),
		"and they OR like any other pair")
}

// TestWorkflowStateFilter_DeletedStateReturnsZeroNeverNone.
//
// ⛔ THE THIRD ASSERTION IS THE LOAD-BEARING ONE. `assets_state_id_fkey`
// is ON DELETE SET NULL, so deleting a state nulls every asset holding
// it. A saved query naming that state must keep naming it and return
// nothing. If it degraded to `none`, deleting one state would silently
// WIDEN every stored query that referenced it into rows its author never
// asked for — #1368's defect arriving through the schema instead of
// through the serializer.
func TestWorkflowStateFilter_DeletedStateReturnsZeroNeverNone(t *testing.T) {
	pool := coPool(t)
	ctx := context.Background()
	domain, _ := wsAssetDomain(t, pool)

	doomed := wsState(t, pool, domain, wsCodeDeleted)
	d1 := wsAsset(t, pool, wsDelPhrase+" one", &doomed)
	d2 := wsAsset(t, pool, wsDelPhrase+" two", &doomed)
	// Always NULL, so `none` has an inhabitant BEFORE the deletion and
	// the widening is visible as a change rather than as a bare set.
	dn := wsAsset(t, pool, wsDelPhrase+" stateless", nil)

	stale := wsTerm(wsIdentity(domain, wsCodeDeleted))
	none := wsTerm(facet.WorkflowStateNone)

	// BEFORE — the premise. Without it, "zero after" is indistinguishable
	// from a fixture that never worked.
	wsAssertSet(t, "the concrete identity BEFORE the deletion",
		wsRun(t, pool, wsDelPhrase, stale), wsSorted(d1, d2),
		"the two assets carry the state; if this is empty the fixture is broken and\n"+
			"  every assertion below is vacuous")
	wsAssertSet(t, "`none` BEFORE the deletion", wsRun(t, pool, wsDelPhrase, none),
		wsSorted(dn), "only the deliberately stateless asset")

	if _, err := pool.Exec(ctx, `DELETE FROM workflow_states WHERE id = $1`, doomed); err != nil {
		t.Fatalf("delete state: %v", err)
	}
	// PREMISE: the FK actually nulled them.
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM assets WHERE id = ANY($1::uuid[]) AND state_id IS NOT NULL`,
		[]uuid.UUID{uuid.MustParse(d1), uuid.MustParse(d2)}).Scan(&remaining); err != nil {
		t.Fatalf("premise query: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("premise failed: %d of the 2 assets still carry a state after the "+
			"delete — ON DELETE SET NULL did not fire, so the scenario is not the one "+
			"this test is about", remaining)
	}

	// 1. The stale concrete query returns EXACTLY zero.
	after := wsRun(t, pool, wsDelPhrase, stale)
	wsAssertSet(t, "the stale concrete identity AFTER the deletion", after, []string{},
		"the identity still names the state it always named; that state no longer\n"+
			"  exists, so nothing satisfies it")

	// 2. `none` now sees the nulled assets.
	wsAssertSet(t, "`none` AFTER the deletion", wsRun(t, pool, wsDelPhrase, none),
		wsSorted(d1, d2, dn),
		"the FK nulled them, so they genuinely have no state now and `none` is\n"+
			"  their honest answer")

	// 3. ⛔ AND THE TWO ARE NOT THE SAME QUERY.
	if sameIDs(after, wsSorted(d1, d2, dn)) {
		t.Errorf("the stale concrete identity behaved as `none` — deleting one state " +
			"would then silently WIDEN every saved query that named it into rows its " +
			"author never asked for")
	}
	for _, id := range []string{d1, d2, dn} {
		for _, got := range after {
			if got == id {
				t.Errorf("the stale concrete identity returned %s, a row it can only "+
					"have reached by degrading into `none`", id)
			}
		}
	}
}

// TestWorkflowStateFilter_ExcludesPostsAndCollections is the mixed-arm
// contract, asserted as EXACTLY ZERO against a NON-VACUOUS control.
//
// ⛔ The unfiltered control asserts the post and the collection by id
// FIRST. Without that, "zero posts" is satisfied by a corpus that has
// none, and an arm that ignored the constraint entirely would still
// pass — a mutation of 18b's suite proved exactly that.
func TestWorkflowStateFilter_ExcludesPostsAndCollections(t *testing.T) {
	pool := coPool(t)
	fx := wsSeed(t, pool)

	ref := wsOwner
	threeTypes := func(filters ...string) map[HitType][]string {
		t.Helper()
		s, err := facet.ParseSelection(filters)
		if err != nil {
			t.Fatalf("parse %v: %v", filters, err)
		}
		res, err := NewEngine(pool).Run(context.Background(), Query{
			Text:          wsPhrase,
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

	unfiltered := threeTypes()
	wsAssertSet(t, "the unfiltered three-type search's POSTS",
		unfiltered[HitTypePost], wsSorted(fx.Post),
		"the fixture post is public, published and carries the phrase, so it is\n"+
			"  eligible for ordinary search visibility. If it is missing here the\n"+
			"  exclusion assertion below proves nothing")
	wsAssertSet(t, "the unfiltered three-type search's COLLECTIONS",
		unfiltered[HitTypeCollection], wsSorted(fx.Collection), "same for the collection")
	wsAssertSet(t, "the unfiltered three-type search's ASSETS",
		unfiltered[HitTypeAsset], fx.all(), "and all five assets")

	filtered := threeTypes(wsTerm(wsIdentity(fx.Domain, wsCodeS1)))
	if n := len(filtered[HitTypePost]); n != 0 {
		t.Errorf("a workflow-state-filtered search returned %d POSTS, want exactly 0 — a\n"+
			"  draft appears on no shared surface including search, so `post/wip` is\n"+
			"  unreachable there and `post/published` is tautological. The arm must fall\n"+
			"  out through satisfiable=false rather than render with no constraint: %v",
			n, filtered[HitTypePost])
	}
	if n := len(filtered[HitTypeCollection]); n != 0 {
		t.Errorf("a workflow-state-filtered search returned %d COLLECTIONS, want exactly "+
			"0 — a collection has no state_id column at all: %v", n, filtered[HitTypeCollection])
	}
	// ⭐ The counterweight. Excluding every arm would satisfy both
	// assertions above and ship a filter that returns nothing.
	wsAssertSet(t, "the asset arm of a three-type filtered search",
		filtered[HitTypeAsset], wsSorted(fx.A, fx.B),
		"assets DO carry a state and must still be returned")
}

// TestWorkflowStateFilter_RoundTripsThroughDSL is the saved-query half,
// asserted on the POPULATION rather than only on the representation.
//
// The same request twice: once with the selection the rail produced,
// once with the selection recovered from its canonical DSL.
func TestWorkflowStateFilter_RoundTripsThroughDSL(t *testing.T) {
	pool := coPool(t)
	fx := wsSeed(t, pool)

	s1 := wsTerm(wsIdentity(fx.Domain, wsCodeS1))
	s2 := wsTerm(wsIdentity(fx.Domain, wsCodeS2))

	for _, c := range []struct {
		name    string
		filters []string
		want    []string
	}{
		{"one concrete state", []string{s1}, wsSorted(fx.A, fx.B)},
		{"two concrete states", []string{s1, s2}, wsSorted(fx.A, fx.B, fx.C)},
		{"a concrete state and none", []string{s1, wsTerm(facet.WorkflowStateNone)},
			wsSorted(fx.A, fx.B, fx.N)},
		{"none alone", []string{wsTerm(facet.WorkflowStateNone)}, wsSorted(fx.N)},
		{"an unknown identity", []string{wsTerm("asset:99999/ws_never_defined")}, []string{}},
		{"beside another dimension", []string{s1, "sensitivity:public"}, wsSorted(fx.A, fx.B)},
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
			wsAssertSet(t, "the direct selection", wsRunSelection(t, pool, wsPhrase, in),
				c.want, "the fixture's expected set")
			wsAssertSet(t, "the selection replayed from "+text,
				wsRunSelection(t, pool, wsPhrase, out), c.want,
				"a saved search must replay exactly the page it was saved from")
		})
	}
}
