// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #684 — `GET /posts?team_id=` is the team page's feed.
//
// # Why this is a pagination test and not a filter test
//
// "Does the filter work" is one page-one assertion and it is not where
// the risk is. The risk is that #868 gave this feed a DIRECTION, and
// the keyset predicate and the ORDER BY now move together through
// feedOrder — but nothing had ever exercised that machinery with a
// WHERE clause also in play.
//
// A filter interacts with a keyset in a specific, quiet way: the query
// fetches `limit + 1` rows to decide `next_cursor`, and the cursor is
// taken from the last row of the PAGE. Get the interaction wrong and
// the symptom is not an error — it is a feed that silently drops posts
// somewhere around page two, in one direction only, which is invisible
// to anyone who looks at page one.
//
// So this walks the filtered feed to exhaustion in BOTH directions and
// asserts no skips, no repeats, correct order, and that asc is the
// exact mirror of desc — the #868 discipline, now with a filter.
//
// # And it proves the filter NARROWS
//
// The fixture plants decoys: posts by the SAME author in a different
// team, and posts by that author in no team at all. Every walk asserts
// they never appear. Without the decoys a filter that was silently
// ignored would pass every ordering assertion in this file.
//
// Reuses fdWalk's harness (fdSeed's sort, fdAssertExactlyOnce,
// fdAssertOrder) from feed_direction_test.go — same package, and the
// point is that the filtered feed obeys the SAME contract, so it should
// be checked by the same assertions.

package posts

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// tfAuthor is a synthetic author, distinct from fdAuthor so the two
// files' fixtures cannot see each other even when run in parallel.
// posts.author_user_ref has no FK to "user", so no user row is needed.
const tfAuthor int64 = 6840001

// tfTeam creates a real team row. Unlike the author, team_id DOES carry
// an FK, so the row has to exist.
func tfTeam(t *testing.T, pool *pgxpool.Pool, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	slug := "tf_feed_" + id.String()[:8] + "_" + label
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`, id, slug, label); err != nil {
		t.Fatalf("seed team %s: %v", label, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

// tfSeedPosts plants n posts for tfAuthor in the given team (nil for
// none) and returns their ids in oldest-first (posted_at ASC, id ASC)
// order — the same key the query pages on, computed rather than assumed
// from insert order.
//
// `sameInstant` collapses every posted_at onto one timestamp, which is
// the only shape in which the `id` half of the keyset does any work. It
// is not a contrived case: a bulk import, a seed run and a backfill all
// produce it, and the seeded database's team posts share timestamps.
func tfSeedPosts(t *testing.T, pool *pgxpool.Pool, team *uuid.UUID, n int, sameInstant bool) []uuid.UUID {
	t.Helper()
	base := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

	rows := make([]fdSeededPost, 0, n)
	for i := 0; i < n; i++ {
		at := base
		if !sameInstant {
			at = base.Add(time.Duration(i) * time.Minute)
		}
		rows = append(rows, fdSeededPost{id: uuid.New(), at: at})
	}
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	for _, r := range rows {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO posts (id, author_user_ref, title, description, visibility, posted_at, team_id)
			 VALUES ($1, $2, 'tf post', '', 'org-only', $3, $4)`,
			r.id, tfAuthor, r.at, teamArg); err != nil {
			t.Fatalf("seed post: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, r := range rows {
			_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, r.id)
		}
	})

	want := make([]uuid.UUID, len(rows))
	for i := range rows {
		want[i] = rows[i].id
	}
	sortUUIDsByKey(want, rows)
	return want
}

// tfWalk pages the TEAM-FILTERED feed to exhaustion in one direction,
// following next_cursor exactly as the browser does.
//
// AuthorRef is pinned to the caller's own ref for the same reason
// fdWalk does it: it isolates the fixture from everything else in the
// database, and it drops the handler's `org-only` visibility default
// (an explicit self-author filter means "all of my tiers"), so the
// direction and the team filter are the only variables.
//
// The page cap turns the classic half-flipped-keyset symptom — a cursor
// that never advances — into a bounded failure instead of a hung suite.
func tfWalk(t *testing.T, h *Handler, team uuid.UUID, ascending bool, pageSize int) []uuid.UUID {
	t.Helper()
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{UserRef: tfAuthor, AuthMethod: "session"})

	ref := tfAuthor
	limit := pageSize
	teamID := openapi_types.UUID(team)
	var out []uuid.UUID
	var cursor *string

	for page := 0; ; page++ {
		if page > 20 {
			t.Fatalf("team feed dir=%v: cursor never terminated after 20 pages "+
				"(%d ids so far) — a keyset that does not advance", ascending, len(out))
		}
		params := openapi.ListPostsParams{
			AuthorRef: &ref,
			TeamId:    &teamID,
			Limit:     &limit,
			Cursor:    cursor,
		}
		if ascending {
			d := openapi.Asc
			params.Dir = &d
		}
		resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: params})
		if err != nil {
			t.Fatalf("ListPosts: %v", err)
		}
		ok, is := resp.(openapi.ListPosts200JSONResponse)
		if !is {
			t.Fatalf("ListPosts returned %T, want 200", resp)
		}
		for _, p := range ok.Items {
			out = append(out, uuid.UUID(p.Id))
		}
		if ok.NextCursor == nil {
			return out
		}
		cursor = ok.NextCursor
	}
}

// ---------------------------------------------------------------------------
// ⭐ Keyset pagination holds on the filtered feed, both directions
// ---------------------------------------------------------------------------

// TestTeamFeed_PaginatesBothWays walks the team-scoped feed end to end
// in each direction over a row count that does not divide evenly into
// the page size, so the short final page is exercised too.
//
// The decoys are the point of the fixture: the same author's posts in
// ANOTHER team, and in no team at all. They outnumber nothing and prove
// everything — a filter that was dropped on the floor would return them
// and fail here, while passing every ordering assertion.
func TestTeamFeed_PaginatesBothWays(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	teamA := tfTeam(t, pool, "a")
	teamB := tfTeam(t, pool, "b")

	const n, pageSize = 11, 3 // 4 pages: 3 + 3 + 3 + 2
	oldestFirst := tfSeedPosts(t, pool, &teamA, n, false)
	decoyOtherTeam := tfSeedPosts(t, pool, &teamB, 4, false)
	decoyNoTeam := tfSeedPosts(t, pool, nil, 4, false)

	desc := tfWalk(t, h, teamA, false, pageSize)
	fdAssertExactlyOnce(t, "team feed dir=desc", desc, oldestFirst)
	fdAssertOrder(t, "team feed dir=desc", desc, oldestFirst, false)

	asc := tfWalk(t, h, teamA, true, pageSize)
	fdAssertExactlyOnce(t, "team feed dir=asc", asc, oldestFirst)
	fdAssertOrder(t, "team feed dir=asc", asc, oldestFirst, true)

	// The two walks are exact mirrors — the property a user asserts by
	// clicking the toggle: the same posts, reversed.
	if len(asc) != len(desc) {
		t.Fatalf("asc returned %d posts, desc returned %d", len(asc), len(desc))
	}
	for i := range asc {
		if asc[i] != desc[len(desc)-1-i] {
			t.Fatalf("asc is not the reverse of desc at position %d: %s vs %s",
				i, asc[i], desc[len(desc)-1-i])
		}
	}

	// ── The filter NARROWS ───────────────────────────────────────────
	//
	// fdAssertExactlyOnce already flags anything unexpected, but it
	// reports it as "unexpected post <uuid>", which does not say WHY the
	// row is wrong. Naming the two decoy classes makes a dropped filter
	// read as a dropped filter in the failure output.
	for _, walk := range [][]uuid.UUID{desc, asc} {
		for _, id := range decoyOtherTeam {
			if _, found := indexOf(walk, id); found {
				t.Errorf("post %s belongs to ANOTHER team but appeared on team %s's feed — "+
					"the team filter is not being applied", id, teamA)
			}
		}
		for _, id := range decoyNoTeam {
			if _, found := indexOf(walk, id); found {
				t.Errorf("post %s has no team but appeared on team %s's feed — "+
					"a NULL team_id must not match a team filter", id, teamA)
			}
		}
	}
}

// TestTeamFeed_TiebreakCarriesTheOrder is the same walk with every post
// sharing one posted_at, so the timestamp comparison can never advance
// the scan and pagination rests entirely on the uuid tiebreak moving
// with the direction.
//
// This is the case a half-flipped keyset survives: flip `posted_at <` to
// `>` and leave `id <` alone and the test above still passes, while this
// one returns page one forever or an empty page two depending on where
// the uuids fall. Worth re-running under the filter because the filtered
// page is a different row set, so the `limit + 1` lookahead lands on
// different boundaries.
func TestTeamFeed_TiebreakCarriesTheOrder(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	team := tfTeam(t, pool, "tie")
	other := tfTeam(t, pool, "tie-other")

	const n, pageSize = 9, 2 // 5 pages: 2 + 2 + 2 + 2 + 1
	oldestFirst := tfSeedPosts(t, pool, &team, n, true)
	// Decoys at the SAME instant, so an ignored filter does not just add
	// rows — it interleaves them into the tiebreak and corrupts the
	// order rather than merely lengthening it.
	tfSeedPosts(t, pool, &other, 5, true)

	for _, tc := range []struct {
		name      string
		ascending bool
	}{
		{"desc", false},
		{"asc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tfWalk(t, h, team, tc.ascending, pageSize)
			fdAssertExactlyOnce(t, "team tiebreak dir="+tc.name, got, oldestFirst)
			fdAssertOrder(t, "team tiebreak dir="+tc.name, got, oldestFirst, tc.ascending)
		})
	}
}

// TestTeamFeed_UnknownTeamIsAnEmptyPage — not a 404. An unknown UUID, a
// soft-deleted team and a team with no readable posts are one answer, so
// the endpoint cannot be used to enumerate the studios on an instance.
func TestTeamFeed_UnknownTeamIsAnEmptyPage(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	team := tfTeam(t, pool, "real")
	tfSeedPosts(t, pool, &team, 3, false)

	// REACHABLE FIRST: the real team's feed is non-empty, so the empty
	// results below are about the team and not about a broken fixture.
	if got := tfWalk(t, h, team, false, 10); len(got) != 3 {
		t.Fatalf("REACHABILITY: the real team's feed returned %d posts, want 3", len(got))
	}

	if got := tfWalk(t, h, uuid.New(), false, 10); len(got) != 0 {
		t.Errorf("a nonexistent team returned %d posts, want an empty page", len(got))
	}

	if _, err := pool.Exec(t.Context(),
		`UPDATE teams SET deleted_at = now() WHERE id = $1`, team); err != nil {
		t.Fatalf("soft-delete team: %v", err)
	}
	// The posts outlive the tombstone (a soft delete cascades nothing)
	// and the filter is an exact column match that does not join
	// `teams`, so they are still served. What matters for the oracle
	// argument is that both calls answered 200 with a page — neither
	// raised a 404, so neither reveals whether the UUID names a team.
	if got := tfWalk(t, h, team, false, 10); len(got) != 3 {
		t.Errorf("a soft-deleted team's posts returned %d, want 3 — the rows are still "+
			"live and still reachable through unfiltered browse, so the team filter "+
			"must not be narrower than the predicate", len(got))
	}
}
