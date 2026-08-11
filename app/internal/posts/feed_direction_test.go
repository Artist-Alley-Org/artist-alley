// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #868 — the feed's Newest/Oldest toggle, and the half of it that bites.
//
// The reported bug is small: the browse page sent `?dir=asc`, no such
// parameter was declared, and "Oldest" rendered newest. Declaring it and
// flipping the ORDER BY closes that in one line.
//
// The half worth testing is the CURSOR. `ORDER BY posted_at ASC` with
// the original `posted_at < $cursor` predicate is not a partial fix, it
// is a worse bug: the scan walks forward while the predicate asks for
// rows behind it, so page 2 re-serves page 1's window and everything
// past it is unreachable. That failure is invisible on page one, which
// is the only page a manual check tends to look at.
//
// So the assertions here are about SEQUENCES ACROSS PAGES, in both
// directions:
//
//   - every page walked end to end yields each seeded post EXACTLY ONCE
//     (no skips, no repeats),
//   - the concatenation is sorted in the requested direction,
//   - asc is the exact reverse of desc over the same rows,
//   - and the pagination holds when every post shares one posted_at, so
//     the `id` tiebreak carries the whole ordering on its own.
//
// That last case is the one that catches a half-flipped keyset: with
// distinct timestamps the timestamp comparison alone can look correct
// while the id comparison still points the other way.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

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

// One synthetic author, so the fixture can be selected with an
// author_ref filter and cannot see or be seen by anything else in the
// database. As in list_visibility_test.go, posts.author_user_ref has no
// FK to "user", so no user row is needed.
const fdAuthor int64 = 6610001

// fdSeededPost is one planted row plus the key it was planted at, so the
// expected order is COMPUTED from the seed rather than assumed from
// insert order.
type fdSeededPost struct {
	id uuid.UUID
	at time.Time
}

// fdSeed plants n posts for fdAuthor and returns their ids in
// oldest-first order.
//
// `sameInstant` collapses every posted_at onto one timestamp. That is
// not an edge case invented for the test: a bulk import, a seed run and
// a migration backfill all produce it, and it is the ONLY shape in which
// the `id` half of the keyset does any work.
func fdSeed(t *testing.T, pool *pgxpool.Pool, n int, sameInstant bool) []uuid.UUID {
	t.Helper()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	rows := make([]fdSeededPost, 0, n)
	for i := 0; i < n; i++ {
		at := base
		if !sameInstant {
			at = base.Add(time.Duration(i) * time.Minute)
		}
		rows = append(rows, fdSeededPost{id: uuid.New(), at: at})
	}

	for _, r := range rows {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO posts (id, author_user_ref, title, description, visibility, posted_at)
			 VALUES ($1, $2, $3, '', 'org-only', $4)`,
			r.id, fdAuthor, "fd post", r.at); err != nil {
			t.Fatalf("seed post: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE author_user_ref=$1`, fdAuthor)
	})

	// Oldest-first is (posted_at ASC, id ASC) — the same key the query
	// pages on, so the expectation is computed by the test rather than
	// assumed from insert order. With sameInstant the timestamps tie and
	// the uuid decides, which is exactly the ordering under test.
	want := make([]uuid.UUID, len(rows))
	for i := range rows {
		want[i] = rows[i].id
	}
	sortUUIDsByKey(want, rows)
	return want
}

// sortUUIDsByKey sorts ids by (posted_at ASC, id ASC) using an
// insertion sort — n is tiny and this keeps the comparator visible.
func sortUUIDsByKey(ids []uuid.UUID, rows []fdSeededPost) {
	at := map[uuid.UUID]time.Time{}
	for _, r := range rows {
		at[r.id] = r.at
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0; j-- {
			a, b := ids[j-1], ids[j]
			less := at[b].Before(at[a]) || (at[b].Equal(at[a]) && b.String() < a.String())
			if !less {
				break
			}
			ids[j-1], ids[j] = b, a
		}
	}
}

// fdWalk pages the feed to exhaustion in one direction and returns every
// id it handed out, in the order it handed them out.
//
// It follows next_cursor exactly as the browser does, and caps the walk
// so a cursor that fails to advance (the classic half-flipped-keyset
// symptom) fails as a bounded assertion rather than hanging the suite.
func fdWalk(t *testing.T, h *Handler, ascending bool, pageSize int) []uuid.UUID {
	t.Helper()
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{UserRef: fdAuthor, AuthMethod: "session"})

	ref := fdAuthor
	limit := pageSize
	var out []uuid.UUID
	var cursor *string

	for page := 0; ; page++ {
		if page > 20 {
			t.Fatalf("dir=%v: cursor never terminated after 20 pages (%d ids so far) — "+
				"a keyset that does not advance", ascending, len(out))
		}
		params := openapi.ListPostsParams{
			AuthorRef: &ref,
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

// fdAssertExactlyOnce is the no-skips / no-repeats assertion, reported
// as the two distinct defects rather than as one count mismatch — a
// page that both drops one row and repeats another has the right length.
func fdAssertExactlyOnce(t *testing.T, label string, got, want []uuid.UUID) {
	t.Helper()
	seen := map[uuid.UUID]int{}
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		switch seen[id] {
		case 1:
		case 0:
			t.Errorf("%s: post %s was SKIPPED — never returned by any page", label, id)
		default:
			t.Errorf("%s: post %s was REPEATED across pages (%d times)", label, id, seen[id])
		}
	}
	for id, n := range seen {
		if _, expected := indexOf(want, id); !expected {
			t.Errorf("%s: unexpected post %s in results (%d times)", label, id, n)
		}
	}
}

func indexOf(xs []uuid.UUID, want uuid.UUID) (int, bool) {
	for i, x := range xs {
		if x == want {
			return i, true
		}
	}
	return 0, false
}

func fdAssertOrder(t *testing.T, label string, got, wantOldestFirst []uuid.UUID, ascending bool) {
	t.Helper()
	want := make([]uuid.UUID, len(wantOldestFirst))
	copy(want, wantOldestFirst)
	if !ascending {
		for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
			want[i], want[j] = want[j], want[i]
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d posts, want %d", label, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: position %d is %s, want %s\n got: %v\nwant: %v",
				label, i, got[i], want[i], got, want)
		}
	}
}

// TestFeedDirection_PaginatesBothWays is the core of #868: four pages
// each way over eleven posts, with the pages deliberately not dividing
// evenly into the row count so the final short page is exercised too.
func TestFeedDirection_PaginatesBothWays(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	const n, pageSize = 11, 3 // 4 pages: 3 + 3 + 3 + 2
	oldestFirst := fdSeed(t, pool, n, false)

	desc := fdWalk(t, h, false, pageSize)
	fdAssertExactlyOnce(t, "dir=desc", desc, oldestFirst)
	fdAssertOrder(t, "dir=desc", desc, oldestFirst, false)

	asc := fdWalk(t, h, true, pageSize)
	fdAssertExactlyOnce(t, "dir=asc", asc, oldestFirst)
	fdAssertOrder(t, "dir=asc", asc, oldestFirst, true)

	// The two walks must be exact mirrors. Stated separately from the
	// per-direction order checks because it is the property a user
	// actually asserts by clicking the toggle: the same posts, reversed.
	if len(asc) != len(desc) {
		t.Fatalf("asc returned %d posts, desc returned %d", len(asc), len(desc))
	}
	for i := range asc {
		if asc[i] != desc[len(desc)-1-i] {
			t.Fatalf("asc is not the reverse of desc at position %d: %s vs %s",
				i, asc[i], desc[len(desc)-1-i])
		}
	}
}

// TestFeedDirection_TiebreakCarriesTheOrder pins the `id` half of the
// keyset. Every post shares one posted_at, so the timestamp comparison
// can never advance the scan and the whole of pagination rests on the
// uuid tiebreak moving with the direction.
//
// A keyset that flipped `posted_at <` to `>` but left `id <` alone
// passes the test above and fails here — it returns page 1 forever, or
// an empty page 2, depending on where the uuids happen to fall.
func TestFeedDirection_TiebreakCarriesTheOrder(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	const n, pageSize = 9, 2 // 5 pages: 2 + 2 + 2 + 2 + 1
	oldestFirst := fdSeed(t, pool, n, true)

	for _, tc := range []struct {
		name      string
		ascending bool
	}{
		{"desc", false},
		{"asc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fdWalk(t, h, tc.ascending, pageSize)
			fdAssertExactlyOnce(t, "tiebreak dir="+tc.name, got, oldestFirst)
			fdAssertOrder(t, "tiebreak dir="+tc.name, got, oldestFirst, tc.ascending)
		})
	}
}

// TestFeedDirection_DefaultsToNewestFirst records that omitting `dir`
// and sending `dir=desc` are the same request, and that an unrecognised
// value lands on that documented default rather than on an arbitrary
// order. Nothing validates the enum at bind time (see the handler's
// note), so this is the only place that behaviour is stated.
func TestFeedDirection_DefaultsToNewestFirst(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	const n = 5
	oldestFirst := fdSeed(t, pool, n, false)
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{UserRef: fdAuthor, AuthMethod: "session"})

	list := func(dir *openapi.ListPostsParamsDir) []uuid.UUID {
		ref := fdAuthor
		limit := 50
		resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{
			Params: openapi.ListPostsParams{AuthorRef: &ref, Limit: &limit, Dir: dir},
		})
		if err != nil {
			t.Fatalf("ListPosts: %v", err)
		}
		ok := resp.(openapi.ListPosts200JSONResponse)
		out := make([]uuid.UUID, 0, len(ok.Items))
		for _, p := range ok.Items {
			out = append(out, uuid.UUID(p.Id))
		}
		return out
	}

	descExplicit := openapi.Desc
	junk := openapi.ListPostsParamsDir("sideways")

	fdAssertOrder(t, "dir omitted", list(nil), oldestFirst, false)
	fdAssertOrder(t, "dir=desc", list(&descExplicit), oldestFirst, false)
	fdAssertOrder(t, "dir=sideways", list(&junk), oldestFirst, false)
}
