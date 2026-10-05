// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1242 / ADR 0094 fourth amendment — the SECOND derived AI fact:
// is this post PURELY AI?
//
// # Why a second fact needs a second test file
//
// Because the two facts are deliberately DIFFERENT ANSWERS about the
// same post, and a test that only checked one of them would let the
// other be keyed off it. `posts.ai_provenance` propagates a positive
// claim on ANY contributor, so all four of
//
//	{generated, generated}  {generated, none}
//	{generated, undeclared} {generated, assisted}
//
// read `generated`. A "hide AI work" filter keyed on that column would
// therefore exclude the three MIXED posts along with the pure one — and
// the owner's ruling is exactly that it must not, because excluding a
// post for one member's declaration punishes the honest declaration the
// whole design depends on.
//
// So the assertions below are written as PAIRS wherever the two facts
// diverge: same post, both columns, opposite answers. An implementation
// that made `ai_pure` a synonym for `ai_provenance = 'generated'` — the
// tempting one, since it is one expression instead of a second
// derivation — fails on every mixed case here.
//
// # This is entirely a PLURAL rule, which is where this codebase's bugs
// live
//
// Purity is a claim about a WHOLE member set, and the recurring defect
// shape in this tree is "right for one, wrong for two": ADR 0094's own
// first derivation rule was correct for a single member and undefined
// over {none, undeclared}, and #907's filter grouping was correct for
// one term and ORed two. So every two-member combination is asserted
// explicitly rather than by sampling, the empty set is asserted, and the
// single-member case is asserted separately so a rule that only works
// for n=1 cannot hide behind one that only works for n=2.
//
// Every case is exercised through a REAL membership or cover change, so
// what is under test is the maintained COLUMN and the triggers that
// write it — not the function, which a correct trigger-less
// implementation would also pass.
//
// Skips without AA_DB_PASSWORD.

package assets

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// aipPure returns the post's stored purity flag. The COLUMN, for the
// reason aipRead gives: a trigger that never fired leaves a correct
// function beside a stale column, and the column is what the filter
// reads.
func aipPure(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(context.Background(),
		`SELECT ai_pure FROM posts WHERE id = $1`, postID).Scan(&v); err != nil {
		t.Fatalf("read post: %v", err)
	}
	return v
}

func aipWantPure(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID, want bool, why string) {
	t.Helper()
	if got := aipPure(t, pool, postID); got != want {
		t.Errorf("post ai_pure = %v, want %v — %s", got, want, why)
	}
}

// TestAIPurity_EveryTwoMemberCombination is the plural rule, stated as a
// table because the cases are the specification.
//
// ⭐ Read the `provenance` column of this table beside the `pure` one.
// Four of the six rows carry `generated` and are NOT pure; that
// divergence IS the feature, and it is what a filter keyed on the
// labelling fact would get wrong.
func TestAIPurity_EveryTwoMemberCombination(t *testing.T) {
	pool := aipPool(t)

	for _, c := range []struct {
		name       string
		members    []string
		provenance string
		pure       bool
		why        string
	}{
		{
			name: "generated + generated", members: []string{"generated", "generated"},
			provenance: "generated", pure: true,
			why: "every contributor declares generated over a non-empty set — this is " +
				"the ONLY shape a hide-AI filter may exclude",
		},
		{
			name: "generated + none", members: []string{"generated", "none"},
			provenance: "generated", pure: false,
			why: "⭐ THE OWNER'S RULING. AI in an ideation phase and a final piece made " +
				"by hand is human work; excluding this post would punish the honest " +
				"declaration on the first member",
		},
		{
			name: "generated + undeclared", members: []string{"generated", ""},
			provenance: "generated", pure: false,
			why: "an undeclared member is a member nobody was asked about, and " +
				"not-knowing must never hide an artist's work (ADR 0094 §3)",
		},
		{
			name: "generated + assisted", members: []string{"generated", "assisted"},
			provenance: "generated", pure: false,
			why: "an assisted member is human work made with AI help, so the post is " +
				"not purely AI however strong the other member's claim is",
		},
		{
			name: "assisted + assisted", members: []string{"assisted", "assisted"},
			provenance: "assisted", pure: false,
			why: "assisted NEVER contributes to purity — an all-assisted post is " +
				"exactly the work the ruling protects",
		},
		{
			name: "none + none", members: []string{"none", "none"},
			provenance: "none", pure: false,
			why: "wholly disclaimed work is the furthest thing from pure AI",
		},
		{
			name: "undeclared + undeclared", members: []string{"", ""},
			provenance: "", pure: false,
			why: "nobody was asked, so nothing may be concluded against them",
		},
		{
			name: "no members at all", members: nil,
			provenance: "", pure: false,
			why: "there is nothing to be pure of; the empty set must not satisfy a " +
				"unanimity test, which is the classic vacuous-truth bug",
		},
		{
			name: "one generated member alone", members: []string{"generated"},
			provenance: "generated", pure: true,
			why: "the singular case, asserted separately so a rule that is only " +
				"correct for two members cannot hide behind one that is only " +
				"correct for one",
		},
		{
			name: "one undeclared member alone", members: []string{""},
			provenance: "", pure: false,
			why: "the singular case of the fails-toward-showing direction",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ids := make([]uuid.UUID, 0, len(c.members))
			for _, m := range c.members {
				ids = append(ids, aipAsset(t, pool, m))
			}
			p := aipPost(t, pool, ids...)
			aipWantPure(t, pool, p, c.pure, c.why)
			// The PAIR. Asserted on every row, so the divergence between
			// the labelling fact and the filtering fact is held down
			// case by case rather than at one hand-picked point.
			aipWant(t, pool, p, c.provenance,
				"the labelling fact must be unchanged by #1242")
		})
	}
}

// TestAIPurity_TracksMembershipChanges — the column is MAINTAINED.
//
// Both directions, and through both write paths (a membership change and
// a declaration change), because a derivation that can only ever turn
// purity ON is a different bug from one that never fires: a post that
// became pure and then gained a hand-made member would stay hidden from
// a viewer filtering AI out, forever, with nothing to say why.
func TestAIPurity_TracksMembershipChanges(t *testing.T) {
	pool := aipPool(t)
	ctx := context.Background()

	first := aipAsset(t, pool, "generated")
	p := aipPost(t, pool, first)
	aipWantPure(t, pool, p, true, "baseline: one generated member")

	// A hand-made member joins. The post is now mixed.
	joiner := aipAsset(t, pool, "none")
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, 9)`,
		p, joiner); err != nil {
		t.Fatalf("add member: %v", err)
	}
	aipWantPure(t, pool, p, false,
		"adding hand-made work to an AI post makes it mixed — post_assets_ai_provenance_sync_trg "+
			"must be able to turn purity OFF")

	// …and leaves again.
	if _, err := pool.Exec(ctx,
		`DELETE FROM post_assets WHERE post_id = $1 AND asset_id = $2`, p, joiner); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	aipWantPure(t, pool, p, true, "…and back ON when the last non-AI member leaves")

	// The remaining member's own declaration changes. No membership row
	// moves at all, so this is the assets trigger rather than the
	// post_assets one.
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET ai_provenance = 'assisted' WHERE id = $1`, first); err != nil {
		t.Fatalf("update declaration: %v", err)
	}
	aipWantPure(t, pool, p, false,
		"a maker downgrading their own declaration must re-derive every post holding the asset")

	// Clearing the declaration entirely — undeclared is not pure.
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET ai_provenance = 'generated' WHERE id = $1`, first); err != nil {
		t.Fatalf("restore declaration: %v", err)
	}
	aipWantPure(t, pool, p, true, "…and back")
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET ai_provenance = NULL WHERE id = $1`, first); err != nil {
		t.Fatalf("clear declaration: %v", err)
	}
	aipWantPure(t, pool, p, false, "withdrawing a declaration leaves nothing to be pure of")
}

// TestAIPurity_CoverPicturesAreContributors — the #1147 arm, on the new
// fact.
//
// A COVER IS A CONTRIBUTOR AND IS NOT A MEMBER. `posts.mature` shipped
// deriving from `post_assets` alone and #1147 was the bill: a post can
// point at a cover it does not hold, and a card shows the cover FIRST.
// 00060 carried the correction into the labelling fact from its first
// migration; 00061 inherits it by construction, because both facts now
// read one `post_ai_contributors` population — and this test is what
// proves the inheritance rather than assuming it.
//
// Cover-disagrees-with-member is the case that matters, so every
// assertion here is one.
func TestAIPurity_CoverPicturesAreContributors(t *testing.T) {
	pool := aipPool(t)
	ctx := context.Background()

	member := aipAsset(t, pool, "generated")
	p := aipPost(t, pool, member)
	aipWantPure(t, pool, p, true, "baseline: members only, all generated")

	// A hand-made COVER over AI members. The post shows human work
	// first, so it is not purely AI.
	handCover := aipAsset(t, pool, "none")
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_asset_id = $2 WHERE id = $1`, p, handCover); err != nil {
		t.Fatalf("set cover: %v", err)
	}
	aipWantPure(t, pool, p, false,
		"a hand-made cover is hand-made work the post shows first — a covers-blind "+
			"derivation returns true here and hides the artist's own picture")

	// An UNDECLARED cover is the fails-toward-showing case, one level up.
	undeclaredCover := aipAsset(t, pool, "")
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_asset_id = $2 WHERE id = $1`, p, undeclaredCover); err != nil {
		t.Fatalf("swap cover: %v", err)
	}
	aipWantPure(t, pool, p, false,
		"a post cannot be called purely AI over a picture whose maker was never asked")

	// Removing the cover re-derives from the members alone.
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_asset_id = NULL WHERE id = $1`, p); err != nil {
		t.Fatalf("clear cover: %v", err)
	}
	aipWantPure(t, pool, p, true, "removing the cover re-derives from the members alone")

	// `cover_thumbnail_asset_id` is the other half of the same hole —
	// a standalone non-member picture, and 00054 covers both.
	thumb := aipAsset(t, pool, "assisted")
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_thumbnail_asset_id = $2 WHERE id = $1`, p, thumb); err != nil {
		t.Fatalf("set cover thumbnail: %v", err)
	}
	aipWantPure(t, pool, p, false,
		"cover_thumbnail_asset_id is a contributor too, and a derivation that reads "+
			"only cover_asset_id passes every other assertion in this file")

	// ⭐ The opposite direction: an AI cover over hand-made members must
	// NOT make the post pure. A rule spelled "any contributor is
	// generated" — the labelling rule — returns true here.
	if _, err := pool.Exec(ctx, `
		UPDATE posts SET cover_thumbnail_asset_id = NULL, cover_asset_id = NULL
		 WHERE id = $1`, p); err != nil {
		t.Fatalf("clear covers: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET ai_provenance = 'none' WHERE id = $1`, member); err != nil {
		t.Fatalf("downgrade member: %v", err)
	}
	aiCover := aipAsset(t, pool, "generated")
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_asset_id = $2 WHERE id = $1`, p, aiCover); err != nil {
		t.Fatalf("set ai cover: %v", err)
	}
	aipWantPure(t, pool, p, false,
		"an AI cover over hand-made members is a MIXED post — the labelling fact says "+
			"`generated` here and the filter must still show it")
	aipWant(t, pool, p, "generated",
		"…and the labelling fact does say generated, which is the whole reason the "+
			"filter cannot be keyed on it")
}

// TestAIPurity_SoftDeletedMemberIsNotAContributor — a deleted member is
// not a member, on both facts, following 00052/00054/00060.
//
// The direction asserted here is the one that could leak a surprise: a
// mixed post whose only hand-made member is deleted BECOMES pure, and a
// viewer filtering AI out stops seeing it. That is correct — the post
// now shows nothing but AI work — but it is a state change nobody
// writes a membership row for, so it is exactly the path a delta-based
// recompute would miss.
func TestAIPurity_SoftDeletedMemberIsNotAContributor(t *testing.T) {
	pool := aipPool(t)
	ctx := context.Background()

	ai := aipAsset(t, pool, "generated")
	human := aipAsset(t, pool, "none")
	p := aipPost(t, pool, ai, human)
	aipWantPure(t, pool, p, false, "baseline: mixed, both members live")

	if _, err := pool.Exec(ctx,
		`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, human); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	aipWantPure(t, pool, p, true,
		"with the hand-made member gone the survivors are unanimous — no post_assets "+
			"row changed, so only the assets trigger can have fired")

	if _, err := pool.Exec(ctx,
		`UPDATE assets SET deleted_at = NULL WHERE id = $1`, human); err != nil {
		t.Fatalf("restore: %v", err)
	}
	aipWantPure(t, pool, p, false, "restoring it makes the post mixed again")
}

// TestAIPurity_DuplicateContributorDoesNotBreakUnanimity pins the one
// property the `UNION ALL` in `post_ai_contributors` depends on.
//
// An asset that is BOTH a member and the cover appears twice in the
// population. Both counts double, so the ratio the unanimity arm
// compares is unchanged — but that is an argument, and an argument is
// what a test is for. Swapping the UNION ALL for a UNION would also pass
// here; what would NOT pass is a rule spelled `count(generated) = 1`.
func TestAIPurity_DuplicateContributorDoesNotBreakUnanimity(t *testing.T) {
	pool := aipPool(t)
	ctx := context.Background()

	only := aipAsset(t, pool, "generated")
	p := aipPost(t, pool, only)
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_asset_id = $2 WHERE id = $1`, p, only); err != nil {
		t.Fatalf("set cover to the member: %v", err)
	}
	aipWantPure(t, pool, p, true,
		"the post's only asset is both its member and its cover; counting it twice "+
			"must not change a unanimity verdict")

	// And the same asset, both roles, NOT generated.
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET ai_provenance = 'assisted' WHERE id = $1`, only); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	aipWantPure(t, pool, p, false, "…in both directions")
}
