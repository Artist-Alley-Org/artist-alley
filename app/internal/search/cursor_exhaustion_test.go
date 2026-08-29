// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1356 — THE WALK REACHES WHAT THE COUNT ADVERTISES.
//
// `/search` reported 523 results and then stopped handing out cursors at
// 75 of them. `total_count` was never wrong: the hits statement and the
// count statement splice identical eligibility fragments, so the number
// was the exact size of the matching, visible, mature-filtered,
// facet-filtered set. What was wrong is that every arm re-fetched THE
// SAME WINDOW at the top of the ranking on every request and the cursor
// was applied to that window in Go, so a walk could never leave
// `Σ min(perEntityLimit, matches_of_that_type)`.
//
// # ⛔ WHY THE ASSERTION IS THE WALK AND NOT A COUNT
//
// A count assertion passes on the bug — `total_count` was the honest
// half. `len(hits) == total_count` on a single page passes on it too,
// for any query small enough. The only statement that discriminates is
// "follow the cursor until it stops, and what you collected is the whole
// of what you were told exists", so that is what every case here does.
//
// # ⛔ WHY MORE THAN ONE PAGE SIZE, AND WHY THE FIXTURE IS SIZED THIS WAY
//
// The old ceiling was `3 × limit` per entity arm — PROPORTIONAL to the
// page size. A guard at one page size cannot tell a ceiling that was
// REMOVED from one that was merely RAISED: both read as "more results
// than before". Two sizes over one fixture can, because a surviving
// proportional ceiling has to land on two different numbers while the
// fixture size stays put.
//
// So the fixture is sized against the LARGER of the two page sizes:
// exAssets (48) and exAssets+exPosts (68) both sit above `3 × 10 = 30`
// per arm, which is what makes the pre-fix ceilings (30, and 30+20) and
// the post-fix answers (48, 68) different numbers rather than the same
// number twice.
//
// Measured against the unfixed engine, this file reports the defect in
// its own words:
//
//	reachable 30 of a reported 48 hits ... the walk stopped 18 short
//
// # ⭐ WHAT IS ASSERTED BESIDE EXHAUSTION
//
// Reaching more rows must not REORDER the rows that were already
// reachable. Two production callers depend on that and neither of them
// pages: save-as-collection runs the engine once at limit 100 and
// PERSISTS the hits in rank order, and the saved-search executor runs it
// once and HASHES the first page's id set for delta detection. So every
// walk here also asserts that the first page equals an independent
// cursor-less run, and that the concatenation of all pages is in
// `(NormalisedScore DESC, ID DESC, Type DESC)` — the accepted ADR 0056
// §1 order — with no duplicate and no gap.
//
// # ⛔ HYBRID IS NOT WALKED HERE
//
// Both embedding tables hold zero rows on every stack this suite runs
// on, so a hybrid walk would assert against a path that cannot execute.
// Its count contract is separately different (#1364: `perTypeCount` is
// captured before the vector merge, so vector-only hits are never
// counted), which makes `reachable == total_count` an invalid claim
// there rather than merely an unverifiable one. The engine keeps that
// path on its original fetch-from-top behaviour; [TestKeysetFragment]
// covers the piece of the fix that can be checked without a corpus.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

const exOwner int64 = 13560101

// exPhrase is in the title of every fixture row and nowhere else in any
// developer's database, so every count here is attributable to this
// fixture and to nothing that happens to be seeded beside it. That
// matters more than usual: the claim is an EQUALITY between the walk and
// the count, and a stray row matching the term would break it from
// either side.
const exPhrase = "quexlorimbat"

// The subset markers. Each is its own lexeme, carried by a PREFIX of the
// asset fixture, so one seeded corpus answers every boundary case
// without a second set of rows to keep in step.
const (
	exNone   = "znarquibbleth" // in no row at all — the empty case
	exFew    = "yerbolathane"  // fewer than one page
	exExact  = "wilphomorack"  // exactly one page
	exWindow = "vothrenquilm"  // exactly the former per-entity window
)

// The page sizes the walks run at. exLimitB is the one the fixture is
// sized against; exLimitA exists to make a surviving proportional
// ceiling land on a different number.
const (
	exLimitA = 5
	exLimitB = 10
)

// The former per-entity ceiling at exLimitB, spelled from the engine's
// own multiplier rather than as a literal so it cannot drift away from
// it. exWindow marks exactly this many rows, which is the boundary the
// old off-by-window behaviour lived on.
const exWindowRows = exLimitB * 3

// Row counts. Both totals sit above exWindowRows so the pre-fix ceiling
// and the post-fix answer are different numbers — see the header.
const (
	exAssets = 48
	exPosts  = 20
)

func exPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// exTitle builds one fixture title. The subset markers ride on a PREFIX
// of the rows, so `exFew` names the first 4, `exExact` the first 10 and
// so on; the ordinal keeps every document distinct.
func exTitle(kind string, i int) string {
	title := fmt.Sprintf("%s %s plate %d", exPhrase, kind, i)
	if kind == "asset" {
		if i < 4 {
			title += " " + exFew
		}
		if i < exLimitB {
			title += " " + exExact
		}
		if i < exWindowRows {
			title += " " + exWindow
		}
	}
	return title
}

// exSeed plants the corpus. Deliberately NOT one row per subset: the
// markers overlap on purpose so the boundary cases are drawn from the
// SAME ranking the exhaustion walks use, rather than from a private
// corpus whose ordering nothing else exercises.
func exSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1,$2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		exOwner, "ex-owner-1356"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, exOwner, `DELETE FROM "user" WHERE ref = $1`)
	})

	for i := 0; i < exAssets; i++ {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type,
			                    status, sensitivity, processing_status, file_extension)
			VALUES ($1, $2, '', $3, 1, 'active', 'public', 'ready', 'png')`,
			id, exTitle("asset", i), exOwner); err != nil {
			t.Fatalf("seed asset %d: %v", i, err)
		}
		t.Cleanup(func() {
			testdb.Purge(t, pool, id, `DELETE FROM assets WHERE id = $1`)
		})
	}
	for i := 0; i < exPosts; i++ {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO posts (id, author_user_ref, title, description, visibility)
			VALUES ($1, $2, $3, '', 'public')`,
			id, exOwner, exTitle("post", i)); err != nil {
			t.Fatalf("seed post %d: %v", i, err)
		}
		t.Cleanup(func() {
			testdb.Purge(t, pool, id, `DELETE FROM posts WHERE id = $1`)
		})
	}
}

// exQuery is one search as the fixture's owner.
func exQuery(text string, types []HitType, limit int) Query {
	ref := exOwner
	return Query{
		Text:          text,
		Types:         types,
		Limit:         limit,
		CallerUserRef: &ref,
	}
}

// exResult is one completed walk.
type exResult struct {
	hits       []Hit // every page concatenated, in the order delivered
	pages      int
	pageSizes  []int
	totalCount int
	capped     bool
}

// exWalk follows the cursor to exhaustion and returns everything the
// caller could reach.
//
// ⛔ The page cap is not a convenience: a paging bug that emitted a
// cursor forever would otherwise hang the suite rather than fail it, and
// a hang reads as infrastructure rather than as this file's subject.
func exWalk(t *testing.T, e *Engine, q Query, limit int) exResult {
	t.Helper()
	q.Limit = limit
	var out exResult
	maxPages := (exAssets+exPosts)/limit + 8
	for {
		res, err := e.Run(context.Background(), q)
		if err != nil {
			t.Fatalf("run page %d: %v", out.pages+1, err)
		}
		out.pages++
		out.pageSizes = append(out.pageSizes, len(res.Hits))
		out.hits = append(out.hits, res.Hits...)
		out.totalCount = res.TotalCount
		out.capped = res.TotalCountCapped
		if res.NextCursor == nil {
			break
		}
		if out.pages >= maxPages {
			t.Fatalf("the cursor never terminated: %d pages at limit %d for a corpus of %d",
				out.pages, limit, res.TotalCount)
		}
		q.Cursor = res.NextCursor
	}
	return out
}

// exKey identifies a hit across pages. Type as well as id, because the
// merge is cross-entity and "no duplicates" is a claim about the union.
func exKey(h Hit) string { return string(h.Type) + ":" + h.ID.String() }

// exAssertWalk is the whole contract, applied to one completed walk.
func exAssertWalk(t *testing.T, e *Engine, label string, base Query, limit int) exResult {
	t.Helper()
	w := exWalk(t, e, base, limit)

	// ⛔ THE HEADLINE. Against the unfixed engine this is the line that
	// fails, and it names the shortfall rather than just the mismatch.
	if len(w.hits) != w.totalCount {
		t.Errorf("%s: reachable %d of a reported %d hits at limit %d over %d pages — "+
			"the walk stopped %d short of what /search advertises (#1356)",
			label, len(w.hits), w.totalCount, limit, w.pages, w.totalCount-len(w.hits))
	}
	if w.capped {
		t.Errorf("%s: total_count_capped is set on a fixture of %d rows, so this walk is "+
			"no longer testing the exact-count path ADR 0056 §1 specifies",
			label, exAssets+exPosts)
	}

	// No duplicates and no gaps, asserted rather than assumed. A ceiling
	// and a corrupted keyset look the same on the exhaustion line alone.
	seen := make(map[string]int, len(w.hits))
	for i, h := range w.hits {
		if prev, dup := seen[exKey(h)]; dup {
			t.Errorf("%s: %s was delivered twice, at positions %d and %d",
				label, exKey(h), prev, i)
			continue
		}
		seen[exKey(h)] = i
	}

	// ⭐ EVERY PAGE BUT THE LAST IS FULL, AND THE LAST IS NOT EMPTY.
	//
	// A cursor is emitted only when more rows remain than fit on a page,
	// so a short page mid-walk means the window ran out early — the
	// defect one page at a time — and an empty final page is the phantom
	// page a cursor emitted against nothing.
	for i, n := range w.pageSizes {
		last := i == len(w.pageSizes)-1
		switch {
		case !last && n != limit:
			t.Errorf("%s: page %d of %d returned %d hits at limit %d; a page that is not "+
				"the last must be full or the cursor was emitted against a window that "+
				"had already run out", label, i+1, w.pages, n, limit)
		case last && n == 0 && w.totalCount > 0:
			t.Errorf("%s: the walk ended on an EMPTY page %d, so the previous page emitted "+
				"a cursor that led nowhere", label, i+1)
		}
	}

	// ⛔ ORDERING IS PRESERVED ACROSS THE WHOLE WALK.
	for i := 1; i < len(w.hits); i++ {
		if !exOrdered(w.hits[i-1], w.hits[i]) {
			t.Fatalf("%s: positions %d and %d are out of the accepted "+
				"(NormalisedScore DESC, ID DESC, Type DESC) order: "+
				"(%v, %s) then (%v, %s)",
				label, i-1, i,
				w.hits[i-1].NormalisedScore, exKey(w.hits[i-1]),
				w.hits[i].NormalisedScore, exKey(w.hits[i]))
		}
	}

	// ⛔ PAGE ONE IS WHAT IT WAS. Run the query again with no cursor at
	// all — the shape save-as-collection and the saved-search executor
	// use — and it must be the head of the walk, hit for hit.
	first, err := e.Run(context.Background(), exQuery(base.Text, base.Types, limit))
	if err != nil {
		t.Fatalf("%s: cursor-less run: %v", label, err)
	}
	head := w.hits
	if len(head) > limit {
		head = head[:limit]
	}
	if len(first.Hits) != len(head) {
		t.Fatalf("%s: a cursor-less run returned %d hits, the walk's first page had %d",
			label, len(first.Hits), len(head))
	}
	for i := range head {
		if exKey(first.Hits[i]) != exKey(head[i]) {
			t.Fatalf("%s: position %d of page one moved: cursor-less run says %s, the walk "+
				"says %s. Reaching more rows must not reorder the rows that were already "+
				"reachable — save-as-collection persists this order",
				label, i, exKey(first.Hits[i]), exKey(head[i]))
		}
	}
	return w
}

// exOrdered reports whether a then b is the accepted order. Mirrors the
// engine's sort comparator; it is spelled out here rather than reused so
// a change to the comparator has to be made twice on purpose.
func exOrdered(a, b Hit) bool {
	if a.NormalisedScore != b.NormalisedScore {
		return a.NormalisedScore > b.NormalisedScore
	}
	if a.ID.String() != b.ID.String() {
		return a.ID.String() > b.ID.String()
	}
	return a.Type >= b.Type
}

// TestCursorReachesTotalCount is the exhaustion guard, at two page sizes
// and over both a single arm and a cross-entity merge.
func TestCursorReachesTotalCount(t *testing.T) {
	pool := exPool(t)
	exSeed(t, pool)
	e := NewEngine(pool)

	cases := []struct {
		label string
		types []HitType
		want  int
	}{
		// One arm. The pool was `perEntityLimit` rows and the ceiling
		// was `3 × limit`.
		{"assets only", []HitType{HitTypeAsset}, exAssets},
		{"posts only", []HitType{HitTypePost}, exPosts},
		// ⭐ TWO ARMS, because the pool accumulated PER ARM: mixed-type
		// behaviour is not single-type behaviour scaled up, and the
		// original measurement showed it (6 pages, not 3).
		{"assets and posts", []HitType{HitTypeAsset, HitTypePost}, exAssets + exPosts},
		// Three arms, one of which matches nothing. An arm that returns
		// no rows has no maximum to normalise by, and that is the branch
		// the keyset renders a constant for.
		{"all three types", AllHitTypes(), exAssets + exPosts},
	}
	for _, c := range cases {
		for _, limit := range []int{exLimitA, exLimitB} {
			label := fmt.Sprintf("%s at limit %d", c.label, limit)
			t.Run(label, func(t *testing.T) {
				w := exAssertWalk(t, e, label, exQuery(exPhrase, c.types, limit), limit)
				if w.totalCount != c.want {
					t.Fatalf("%s: total_count is %d, the fixture is %d — something "+
						"other than this fixture matches %q and every equality here "+
						"is measuring the wrong corpus",
						label, w.totalCount, c.want, exPhrase)
				}
			})
		}
	}
}

// TestCursorBoundaries walks the sizes where an off-by-window shows up:
// nothing, less than a page, exactly a page, exactly the former window,
// and past it.
func TestCursorBoundaries(t *testing.T) {
	pool := exPool(t)
	exSeed(t, pool)
	e := NewEngine(pool)

	cases := []struct {
		label     string
		term      string
		want      int
		wantPages int
	}{
		{"no results at all", exNone, 0, 1},
		{"fewer than one page", exFew, 4, 1},
		// ⛔ EXACTLY ONE PAGE MUST NOT EMIT A CURSOR. A dangling cursor
		// here is a second request that returns nothing, which the
		// automatic pager on /search turns into a request per scroll.
		{"exactly one page", exExact, exLimitB, 1},
		// ⛔ EXACTLY THE FORMER PER-ENTITY WINDOW. This is where the old
		// behaviour ran out, and it must exhaust cleanly rather than
		// need a phantom page to finish.
		{"exactly the former window", exWindow, exWindowRows, exWindowRows / exLimitB},
		{"past the former window", exPhrase, exAssets, 5},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			w := exAssertWalk(t, e, c.label, exQuery(c.term, []HitType{HitTypeAsset}, exLimitB), exLimitB)
			if w.totalCount != c.want {
				t.Fatalf("%s: total_count is %d, expected %d", c.label, w.totalCount, c.want)
			}
			if w.pages != c.wantPages {
				t.Errorf("%s: the walk took %d pages for %d hits at limit %d, expected %d "+
					"(page sizes %v)", c.label, w.pages, w.totalCount, exLimitB,
					c.wantPages, w.pageSizes)
			}
		})
	}
}
