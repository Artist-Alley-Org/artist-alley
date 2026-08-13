// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1048 — `?feed=following` is the UNION of the two follow graphs.
//
// # The case that was broken is the case with NO user follows
//
// The filter had one arm, against user_follows, and `team_follows` was
// never consulted. So an account that follows four studios and no
// people clicked "Following" and got an empty grid while several
// hundred readable posts by those studios sat one tab away. That shape
// is not exotic: following a *user* is a write a read-only account is
// refused, while following a team is not, so on the public demo it was
// the ONLY shape a visitor could produce.
//
// Every fixture here therefore has the follower follow a team and NO
// user, except the one test whose subject is the overlap. A fixture
// that followed both would have passed against the bug — which is
// exactly why the gap survived: the surface was only ever exercised by
// accounts that followed somebody.
//
// # What must NOT change
//
// list_page.go's header states the contract: every filter NARROWS, and
// the read rule is ANDed onto the result rather than ORed into it. A
// team follow is a bookmark (teams/follows_handler.go), not a grant, so
// widening the SELECTION from one graph to two must not move a single
// row of what the caller may read. TestFollowingFeed_ReadRuleStillNarrows
// is the positive control for that, and it asks with an explicit
// `?visibility=` so the answer comes from the read rule and not from
// the handler's org-only display default quietly hiding the row.

package posts

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// folAuthor is the studio's poster: somebody the follower does NOT
// follow, so any row of theirs that appears got there through the team
// arm. posts.author_user_ref carries no FK to "user", so no user row is
// needed for an author.
const folAuthor int64 = 10480001

// folOtherAuthor is a second unfollowed author, used for decoys.
const folOtherAuthor int64 = 10480002

// folFollower creates the caller. Unlike an author, this one needs a real
// `user` row: team_follows.user_ref carries an FK to "user"(ref), which
// is the structural difference between the two follow tables and the
// reason a synthetic ref cannot stand in here.
func folFollower(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var ref int64
	name := "fol_follower_" + uuid.New().String()[:12]
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $1, 1) RETURNING ref`,
		name).Scan(&ref); err != nil {
		t.Fatalf("seed follower: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// folTeam creates a real team row — team_id has an FK, so it must exist.
func folTeam(t *testing.T, pool *pgxpool.Pool, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	slug := "fol_" + id.String()[:8] + "_" + label
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`, id, slug, label); err != nil {
		t.Fatalf("seed team %s: %v", label, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

func folFollowTeam(t *testing.T, pool *pgxpool.Pool, ref int64, team uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO team_follows (user_ref, team_id) VALUES ($1, $2)`, ref, team); err != nil {
		t.Fatalf("follow team: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM team_follows WHERE user_ref = $1 AND team_id = $2`, ref, team)
	})
}

func folFollowUser(t *testing.T, pool *pgxpool.Pool, follower, followee int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, follower, followee); err != nil {
		t.Fatalf("follow user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref = $1 AND followee_user_ref = $2`,
			follower, followee)
	})
}

// folPost plants one post and returns its id. `team` may be nil for a
// post that belongs to no team — the row shape that must never match a
// team follow, since `NULL = tf.team_id` is NULL, not true.
func folPost(
	t *testing.T,
	pool *pgxpool.Pool,
	author int64,
	team *uuid.UUID,
	visibility string,
	minutesOld int,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesOld) * time.Minute)
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility, posted_at, team_id)
		 VALUES ($1, $2, 'fol post', '', $3, $4, $5)`,
		id, author, visibility, at, teamArg); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

// folFeed runs `?feed=following` as `ref` and returns the ids on the
// page, in order. `vis` is the explicit `?visibility=` filter, or "" to
// take the handler's org-only default.
//
// No AuthorRef pin here, unlike the team-feed harness: pinning the
// author is the one thing that would make a following feed prove
// nothing. Isolation comes from the fixture instead — the follower is a
// fresh user who follows exactly one freshly-created team, so no row
// outside the fixture can satisfy the filter.
func folFeed(t *testing.T, h *Handler, ref int64, vis string) []uuid.UUID {
	t.Helper()
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{UserRef: ref, AuthMethod: "session"})

	feed := openapi.Following
	limit := 100
	params := openapi.ListPostsParams{Feed: &feed, Limit: &limit}
	if vis != "" {
		v := openapi.ListPostsParamsVisibility(vis)
		params.Visibility = &v
	}
	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: params})
	if err != nil {
		t.Fatalf("ListPosts(feed=following): %v", err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts(feed=following) returned %T, want 200", resp)
	}
	out := make([]uuid.UUID, 0, len(ok.Items))
	for _, p := range ok.Items {
		out = append(out, uuid.UUID(p.Id))
	}
	return out
}

// ---------------------------------------------------------------------------
// ⭐ The exact broken case: a team follow and no user follows
// ---------------------------------------------------------------------------

// TestFollowingFeed_TeamFollowAloneReturnsTeamPosts is #1048 itself. The
// follower follows ONE team and zero users; the team's posts are by an
// author they do not follow. Before the fix this page was empty.
//
// The decoys prove the arm that was added still NARROWS: an identical
// post by the same author in ANOTHER team, and one in no team at all.
// A `team_follows` join written as an outer join, or a NULL team_id
// compared with IS NOT DISTINCT FROM, would return them.
func TestFollowingFeed_TeamFollowAloneReturnsTeamPosts(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	follower := folFollower(t, pool)
	followed := folTeam(t, pool, "followed")
	other := folTeam(t, pool, "other")
	folFollowTeam(t, pool, follower, followed)

	wantA := folPost(t, pool, folAuthor, &followed, "org-only", 1)
	wantB := folPost(t, pool, folOtherAuthor, &followed, "org-only", 2)
	decoyOtherTeam := folPost(t, pool, folAuthor, &other, "org-only", 3)
	decoyNoTeam := folPost(t, pool, folAuthor, nil, "org-only", 4)

	got := folFeed(t, h, follower, "")

	for _, want := range []struct {
		id  uuid.UUID
		why string
	}{
		{wantA, "a post in the followed team"},
		{wantB, "a post in the followed team by a second author"},
	} {
		if _, found := indexOf(got, want.id); !found {
			t.Errorf("%s (%s) is missing from feed=following — the team follow graph "+
				"is not being consulted; the follower follows this team and NO users, "+
				"which is the whole case #1048 is about", want.why, want.id)
		}
	}

	if _, found := indexOf(got, decoyOtherTeam); found {
		t.Errorf("post %s belongs to a team the caller does NOT follow but appeared on "+
			"feed=following — the team arm is not restricted to followed teams", decoyOtherTeam)
	}
	if _, found := indexOf(got, decoyNoTeam); found {
		t.Errorf("post %s has no team at all but appeared on feed=following — a NULL "+
			"team_id must not match a team follow", decoyNoTeam)
	}
}

// TestFollowingFeed_UserFollowStillWorks is the arm that was already
// there, re-asserted beside the new one so a future edit cannot trade
// one graph for the other. The author's post is in NO team, so only the
// user arm can return it.
func TestFollowingFeed_UserFollowStillWorks(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	follower := folFollower(t, pool)
	folFollowUser(t, pool, follower, folAuthor)

	want := folPost(t, pool, folAuthor, nil, "org-only", 1)
	decoy := folPost(t, pool, folOtherAuthor, nil, "org-only", 2)

	got := folFeed(t, h, follower, "")
	if _, found := indexOf(got, want); !found {
		t.Errorf("post %s by a followed author is missing from feed=following — the "+
			"user follow graph stopped being consulted", want)
	}
	if _, found := indexOf(got, decoy); found {
		t.Errorf("post %s by an UNfollowed author appeared on feed=following — the "+
			"filter is not narrowing", decoy)
	}
}

// TestFollowingFeed_UnionDoesNotDoubleCount covers the overlap: a post
// whose author the caller follows AND whose team the caller follows
// satisfies both arms. Two EXISTS ORed inside one conjunct can only
// match a row once; two JOINs would emit it twice, and the symptom
// would be a duplicated card mid-feed plus a page that quietly returns
// fewer distinct posts than its limit.
func TestFollowingFeed_UnionDoesNotDoubleCount(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	follower := folFollower(t, pool)
	team := folTeam(t, pool, "both")
	folFollowTeam(t, pool, follower, team)
	folFollowUser(t, pool, follower, folAuthor)

	both := folPost(t, pool, folAuthor, &team, "org-only", 1)
	teamOnly := folPost(t, pool, folOtherAuthor, &team, "org-only", 2)
	authorOnly := folPost(t, pool, folAuthor, nil, "org-only", 3)

	got := folFeed(t, h, follower, "")

	count := 0
	for _, id := range got {
		if id == both {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the post matching BOTH follow graphs appeared %d times on "+
			"feed=following, want exactly 1 — the union must not multiply rows", count)
	}
	for _, id := range []uuid.UUID{teamOnly, authorOnly} {
		if _, found := indexOf(got, id); !found {
			t.Errorf("post %s satisfies one arm of the union and is missing", id)
		}
	}
}

// ---------------------------------------------------------------------------
// ⛔ The read rule is ANDed, never widened
// ---------------------------------------------------------------------------

// TestFollowingFeed_ReadRuleStillNarrows is the positive control. The
// caller follows a team that holds posts they may not read: one
// `private` (author-or-moderator only) and one `followers` by an author
// they do NOT follow. Following the TEAM must not open either.
//
// Each tier is asked for with an explicit `?visibility=`, so the
// handler's org-only display default is not what produces the absence.
// Without that, this test would pass on a build where team_follows HAD
// been spliced into the read rule.
func TestFollowingFeed_ReadRuleStillNarrows(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	follower := folFollower(t, pool)
	team := folTeam(t, pool, "gated")
	folFollowTeam(t, pool, follower, team)

	readable := folPost(t, pool, folAuthor, &team, "org-only", 1)
	private := folPost(t, pool, folAuthor, &team, "private", 2)
	followersOnly := folPost(t, pool, folAuthor, &team, "followers", 3)

	// REACHABLE FIRST: the team arm returns something at all, so the
	// two absences below are about the read rule and not about a
	// fixture that never reached the query.
	if got := folFeed(t, h, follower, ""); len(got) == 0 {
		t.Fatalf("REACHABILITY: the followed team's readable post did not appear — " +
			"this fixture proves nothing about the read rule")
	} else if _, found := indexOf(got, readable); !found {
		t.Fatalf("REACHABILITY: post %s is readable and in the followed team but "+
			"absent — fix that before reading the assertions below", readable)
	}

	if got := folFeed(t, h, follower, "private"); len(got) != 0 {
		t.Errorf("feed=following&visibility=private returned %d posts for a caller who "+
			"only FOLLOWS the team — following a studio is a bookmark, not a grant, and "+
			"must never open a tier the read rule closes (got %v, private post is %s)",
			len(got), got, private)
	}
	if got := folFeed(t, h, follower, "followers"); len(got) != 0 {
		t.Errorf("feed=following&visibility=followers returned %d posts — the followers "+
			"tier resolves through user_follows in the read rule, and a TEAM follow is "+
			"not a user follow (got %v, followers-only post is %s)",
			len(got), got, followersOnly)
	}
}

// TestFollowingFeed_FollowingNothingIsAnEmptyPage — an account that
// follows neither people nor studios gets the empty state, not a 4xx and
// not an error. Same contract the handler comment has always promised;
// worth an assertion now that two subqueries have to agree on it.
func TestFollowingFeed_FollowingNothingIsAnEmptyPage(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	follower := folFollower(t, pool)
	team := folTeam(t, pool, "unfollowed")
	folPost(t, pool, folAuthor, &team, "org-only", 1)
	folPost(t, pool, folAuthor, nil, "org-only", 2)

	if got := folFeed(t, h, follower, ""); len(got) != 0 {
		t.Errorf("a caller following nothing got %d posts on feed=following, want an "+
			"empty page: %v", len(got), got)
	}
}
