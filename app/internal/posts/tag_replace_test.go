// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119: ReplacePostTags, and the tag that vanished when it was re-sent.
//
// # The bug, and why nothing caught it for this long
//
// The query was a data-modifying CTE:
//
//	WITH wipe AS (DELETE FROM post_tags WHERE post_id = $1)
//	INSERT INTO post_tags (post_id, tag)
//	SELECT $1, unnest($2::TEXT[])
//	ON CONFLICT (post_id, tag) DO NOTHING;
//
// Sub-statements in a WITH clause all run against one snapshot and cannot
// see one another's effects (PostgreSQL manual, 7.8.2). So for a tag the
// post ALREADY had, the INSERT's conflict check saw the row the CTE was
// deleting, treated it as a duplicate and skipped it; the delete then
// removed it. Re-sending a tag DELETED it.
//
// ⛔ AND IT READ AS CORRECT, which is the part worth writing down. Adding
// a NEW tag worked, because a new tag has nothing to conflict with.
// `PATCH /posts/{id}` had accepted `tags` since it was written and NO
// SHIPPED CLIENT EVER SENT IT, so the only requests that reached this
// query were hand-written ones that added. The post editor is its first
// real caller, and the first thing it does is re-send the set it read.
//
// ⛔ WHY THE ASSERTION IS ON `post_tags` AND NOT ON A RESPONSE. This is a
// write that returns nothing to be echoed, and the handler above it
// re-reads the post afterwards, so the only oracle that can be wrong in
// the right way is the table.
//
// Skips without AA_DB_PASSWORD, same convention as the other integration
// suites in this package.

package posts

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const trTagOwner int64 = 11190101

func trSeedPost(t *testing.T, pool *pgxpool.Pool, tags []string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, description, visibility)
		 VALUES ($1, $2, 'tr_1119 post', '', 'org-only')`, id, trTagOwner); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_tags WHERE post_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	for _, tag := range tags {
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_tags (post_id, tag) VALUES ($1, $2)
			 ON CONFLICT DO NOTHING`, id, tag); err != nil {
			t.Fatalf("seed tag %q: %v", tag, err)
		}
	}
	return id
}

func trTags(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT tag FROM post_tags WHERE post_id = $1`, id)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			t.Fatalf("scan tag: %v", err)
		}
		out = append(out, tag)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

func trReplace(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, tags []string) {
	t.Helper()
	if err := New(pool).ReplacePostTags(context.Background(), ReplacePostTagsParams{
		PostID:  pgtype.UUID{Bytes: id, Valid: true},
		Column2: tags,
	}); err != nil {
		t.Fatalf("ReplacePostTags(%v): %v", tags, err)
	}
}

// ⛔ THE REGRESSION. Fails on the CTE version with `["added"]`.
func TestReplacePostTags_ResendingATagKeepsIt(t *testing.T) {
	pool := previewPool(t)
	id := trSeedPost(t, pool, []string{"kept"})

	trReplace(t, pool, id, []string{"kept", "added"})

	got := trTags(t, pool, id)
	want := []string{"added", "kept"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("tags = %v, want %v. Re-sending a tag the post already had must "+
			"KEEP it. The CTE version dropped it: the INSERT's conflict check saw "+
			"the row the CTE was deleting, skipped the insert, and the delete then "+
			"removed it.", got, want)
	}
}

// The whole set re-sent unchanged is a no-op, which is the shape the
// editor produces every time an author saves without touching the chips.
func TestReplacePostTags_ResendingTheWholeSetChangesNothing(t *testing.T) {
	pool := previewPool(t)
	id := trSeedPost(t, pool, []string{"alpha", "bravo", "charlie"})

	trReplace(t, pool, id, []string{"alpha", "bravo", "charlie"})

	got := trTags(t, pool, id)
	if len(got) != 3 {
		t.Fatalf("tags = %v, want all three. A save that touched no tag must "+
			"leave all three in place. The CTE version emptied the set entirely, "+
			"because EVERY tag conflicted.", got)
	}
}

// The REMOVE half, which the bug's shape made accidentally correct and
// which the fix must not break: a tag left out of the array goes.
func TestReplacePostTags_OmittedTagsAreRemoved(t *testing.T) {
	pool := previewPool(t)
	id := trSeedPost(t, pool, []string{"stays", "goes"})

	trReplace(t, pool, id, []string{"stays"})

	got := trTags(t, pool, id)
	if len(got) != 1 || got[0] != "stays" {
		t.Fatalf("tags = %v, want [stays]: a tag omitted from the array is a "+
			"tag the author removed", got)
	}
}

// The EMPTY array clears the set. `tag <> ALL('{}')` is true for every
// row and `unnest('{}')` yields none, so both halves do the right thing
// on a value that looks like a degenerate case.
func TestReplacePostTags_EmptyArrayClearsTheSet(t *testing.T) {
	pool := previewPool(t)
	id := trSeedPost(t, pool, []string{"one", "two"})

	trReplace(t, pool, id, []string{})

	if got := trTags(t, pool, id); len(got) != 0 {
		t.Fatalf("tags = %v, want none: clearing every chip must clear the set", got)
	}
}

// Replacing a set with a DISJOINT one: nothing survives and nothing
// lingers. The case where the two sub-statements have the most to
// disagree about, and the one a `<> ALL` restriction could get wrong by
// deleting too little.
func TestReplacePostTags_DisjointReplacement(t *testing.T) {
	pool := previewPool(t)
	id := trSeedPost(t, pool, []string{"old1", "old2"})

	trReplace(t, pool, id, []string{"new1", "new2"})

	got := trTags(t, pool, id)
	want := []string{"new1", "new2"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("tags = %v, want %v", got, want)
	}
}

// A post with NO tags gaining its first ones, which is the path that
// always worked and therefore the one that hid the bug.
func TestReplacePostTags_FirstTagsOnAnUntaggedPost(t *testing.T) {
	pool := previewPool(t)
	id := trSeedPost(t, pool, nil)

	trReplace(t, pool, id, []string{"first", "second"})

	if got := trTags(t, pool, id); len(got) != 2 {
		t.Fatalf("tags = %v, want two", got)
	}
}
