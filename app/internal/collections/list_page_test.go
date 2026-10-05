// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #449 — the collection browse query, converted from sqlc to hand-built
// SQL so the visibility predicate can reach it.
//
// Two kinds of test here, and both are load-bearing:
//
//   - The LEAK tests. The sqlc query this replaces applied no
//     visibility rule at all, so `GET /collections` with no filters
//     returned the whole table to anybody, anonymous included. Those
//     assertions are the fix.
//   - The PARITY test. Every narg filter and the (created_at DESC, id
//     DESC) cursor are product behaviour, not visibility, and had to
//     survive the rewrite untouched. The retained sqlc
//     ListCollectionsPage is the oracle: both implementations run
//     against the same rows and their outputs are compared, rather
//     than the new one being checked against hand-written expectations
//     that could encode the same mistake twice. #429 set this pattern.
//
// The parity comparison uses an OWNER caller over a seed set the owner
// owns entirely, which makes the predicate admit every seeded row. Any
// divergence is then a filter or ordering bug, not a visibility one —
// the two axes are separated on purpose.
//
// Skips without AA_DB_PASSWORD, same convention as the other
// integration suites.

package collections

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

func listCollPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

const (
	listCollOwner    int64 = 4490001
	listCollStranger int64 = 4490002
	listCollGrantee  int64 = 4490003
)

type collSeed struct {
	name       string
	visibility string
	featured   bool
	deleted    bool
}

// listCollSeeds spans every dimension the browse query and the
// predicate care about. All owned by listCollOwner, which is what makes
// the owner caller a clean parity oracle.
var listCollSeeds = []collSeed{
	{"449 alpha public", "public", false, false},
	{"449 bravo private", "private", false, false},
	{"449 charlie org-only", "org-only", false, false},
	{"449 delta followers", "followers", false, false},
	{"449 echo public featured", "public", true, false},
	{"449 foxtrot private featured", "private", true, false},
	{"449 golf public deleted", "public", false, true},
}

func seedBrowseCollections(t *testing.T, pool *pgxpool.Pool) []uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := make([]uuid.UUID, 0, len(listCollSeeds))
	for i, s := range listCollSeeds {
		id := uuid.New()
		ids = append(ids, id)
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		// Distinct created_at so ordering + cursor paging are
		// deterministic rather than tie-broken by chance.
		_, err := pool.Exec(ctx, `
			INSERT INTO collections (id, name, owner_user_ref, visibility, membership,
			                         created_at, deleted_at)
			VALUES ($1,$2,$3,$4,'manual',
			        NOW() - ($5::int * INTERVAL '1 minute'), `+del+`)`,
			id, s.name, listCollOwner, s.visibility, i)
		if err != nil {
			t.Fatalf("seed %q: %v", s.name, err)
		}
		// #449 seeded `featured` as a column; ADR 0065 made it a
		// placement, so the ?featured= filter now resolves through
		// featured_items at scope='org'. Same meaning, different home.
		if s.featured {
			if _, err := pool.Exec(ctx, `
				INSERT INTO featured_items (subject_kind, subject_id, position, scope)
				VALUES ('collection',$1,$2,'org') ON CONFLICT DO NOTHING`, id, i); err != nil {
				t.Fatalf("seed org placement for %q: %v", s.name, err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(),
					`DELETE FROM featured_items WHERE subject_id=$1`, id)
			})
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = ANY($1::uuid[])`, ids)
	})
	return ids
}

func collOwnerPtr() *int64 { r := listCollOwner; return &r }

// seen reduces a result set to the seeded ids present in it, so
// assertions don't depend on whatever else lives in the test database.
func seen(rows []Collection, ids []uuid.UUID) map[uuid.UUID]bool {
	want := map[uuid.UUID]bool{}
	for _, id := range ids {
		want[id] = false
	}
	for _, r := range rows {
		id := uuid.UUID(r.ID.Bytes)
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	return want
}

// TestListCollectionsPage_AnonymousSeesOnlyPublic is the leak this PR
// closes, asserted at the query rather than through the handler so it
// cannot be satisfied by an HTTP-layer check somebody later removes.
//
// Before #449 this returned every row in the table: the WHERE clause
// was `deleted_at IS NULL` plus optional filters, and the default call
// sets none of them.
func TestListCollectionsPage_AnonymousSeesOnlyPublic(t *testing.T) {
	pool := listCollPool(t)
	ids := seedBrowseCollections(t, pool)
	ctx := context.Background()

	rows, err := ListCollectionsPageGated(ctx, pool, visibility.NewCaller(nil),
		ListCollectionsPageGatedParams{RowLimit: 500})
	if err != nil {
		t.Fatalf("gated: %v", err)
	}
	got := seen(rows, ids)
	for i, s := range listCollSeeds {
		want := s.visibility == "public" && !s.deleted
		if got[ids[i]] != want {
			t.Errorf("anonymous: %q (visibility=%q deleted=%v) visible=%v, want %v — "+
				"an anonymous caller must never enumerate a private collection",
				s.name, s.visibility, s.deleted, got[ids[i]], want)
		}
	}
}

// TestListCollectionsPage_NoFiltersIsNotAnEscapeHatch pins the specific
// shape of the bug: it was the ABSENCE of filters that leaked, because
// every condition in the old query was optional and none of them were
// about authorization. A future filter added without a predicate would
// reintroduce it, so this asserts the empty-parameter call directly.
func TestListCollectionsPage_NoFiltersIsNotAnEscapeHatch(t *testing.T) {
	pool := listCollPool(t)
	seedBrowseCollections(t, pool)
	ctx := context.Background()

	rows, err := ListCollectionsPageGated(ctx, pool, visibility.NewCaller(nil),
		ListCollectionsPageGatedParams{RowLimit: 500})
	if err != nil {
		t.Fatalf("gated: %v", err)
	}
	for _, r := range rows {
		if r.Visibility != "public" {
			t.Fatalf("anonymous caller with NO filters received a %q collection (%q); "+
				"the visibility rule must not be reachable only via a filter",
				r.Visibility, r.Name)
		}
	}
}

// TestListCollectionsPage_OwnerAndStranger covers the two authenticated
// ends: the owner sees their own collections at every visibility, and a
// stranger with no ACL grant does not.
//
// NOTE ON THE PUBLIC ROW: whether a stranger sees the owner's PUBLIC
// collections depends on the authenticated branch of the collection
// predicate, which #448 fixes. This test asserts only what is true
// regardless of that fix — the stranger never sees a PRIVATE row — so
// it does not encode either side of that change. The public case is
// covered by the predicate's own contract test, which is where that
// rule lives.
func TestListCollectionsPage_OwnerAndStranger(t *testing.T) {
	pool := listCollPool(t)
	ids := seedBrowseCollections(t, pool)
	ctx := context.Background()

	t.Run("owner sees own collections at every visibility", func(t *testing.T) {
		rows, err := ListCollectionsPageGated(ctx, pool, visibility.NewCaller(collOwnerPtr()),
			ListCollectionsPageGatedParams{RowLimit: 500})
		if err != nil {
			t.Fatalf("gated: %v", err)
		}
		got := seen(rows, ids)
		for i, s := range listCollSeeds {
			if s.deleted {
				continue // soft-delete is the predicate's separate axis
			}
			if !got[ids[i]] {
				t.Errorf("owner: %q not visible; the owner path must be unchanged", s.name)
			}
		}
	})

	t.Run("stranger never sees a private collection", func(t *testing.T) {
		rows, err := ListCollectionsPageGated(ctx, pool, visibility.NewCaller(refPtr(listCollStranger)),
			ListCollectionsPageGatedParams{RowLimit: 500})
		if err != nil {
			t.Fatalf("gated: %v", err)
		}
		got := seen(rows, ids)
		for i, s := range listCollSeeds {
			if s.visibility == "public" {
				continue // governed by #448, asserted in the predicate contract test
			}
			if got[ids[i]] {
				t.Errorf("stranger: %q (visibility=%q) visible without an ACL grant",
					s.name, s.visibility)
			}
		}
	})

	t.Run("an ACL grant admits a non-owner to one private collection", func(t *testing.T) {
		// Grant on the private row, and assert it becomes visible while
		// the OTHER private rows stay hidden — a grant must widen by
		// exactly one row, not switch the rule off.
		target := ids[1] // "449 bravo private"
		_, err := pool.Exec(ctx, `
			INSERT INTO collection_acls (collection_id, principal_type, principal_id, permission)
			VALUES ($1,'user',$2,'read')
			ON CONFLICT DO NOTHING`, target, strconv.FormatInt(listCollGrantee, 10))
		if err != nil {
			// Deliberately fatal rather than skipped. A skipped subtest
			// here would silently stop asserting that a grant widens by
			// exactly one row, which is the half of the ACL rule most
			// likely to break.
			t.Fatalf("seed an ACL grant: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM collection_acls WHERE collection_id=$1 AND principal_id=$2`,
				target, strconv.FormatInt(listCollGrantee, 10))
		})

		rows, err := ListCollectionsPageGated(ctx, pool, visibility.NewCaller(refPtr(listCollGrantee)),
			ListCollectionsPageGatedParams{RowLimit: 500})
		if err != nil {
			t.Fatalf("gated: %v", err)
		}
		got := seen(rows, ids)
		if !got[target] {
			t.Error("grantee cannot see the collection they hold an ACL grant on")
		}
		for i, s := range listCollSeeds {
			if ids[i] == target || s.visibility == "public" {
				continue
			}
			if got[ids[i]] {
				t.Errorf("grantee: %q (visibility=%q) visible; one grant must admit one row",
					s.name, s.visibility)
			}
		}
	})
}

func refPtr(r int64) *int64 { return &r }

// TestListCollectionsPage_FilterParity is why this rewrite is safe: for
// a caller whose predicate admits every seeded row, the hand-built
// query must return exactly what the sqlc query returned, in the same
// order, across every filter combination. Any divergence is a filter or
// cursor regression rather than a visibility change.
func TestListCollectionsPage_FilterParity(t *testing.T) {
	pool := listCollPool(t)
	ids := seedBrowseCollections(t, pool)
	ctx := context.Background()
	q := New(pool)
	caller := visibility.NewCaller(collOwnerPtr())

	visPublic := "public"
	featTrue := true
	qName := "449 "

	cases := []struct {
		name string
		p    ListCollectionsPageGatedParams
	}{
		{"owner only", ListCollectionsPageGatedParams{OwnerUserRef: collOwnerPtr(), RowLimit: 50}},
		{"owner + visibility", ListCollectionsPageGatedParams{OwnerUserRef: collOwnerPtr(), Visibility: &visPublic, RowLimit: 50}},
		{"owner + featured", ListCollectionsPageGatedParams{OwnerUserRef: collOwnerPtr(), Featured: &featTrue, RowLimit: 50}},
		{"owner + name substring", ListCollectionsPageGatedParams{OwnerUserRef: collOwnerPtr(), QName: &qName, RowLimit: 50}},
		{"small limit (paging boundary)", ListCollectionsPageGatedParams{OwnerUserRef: collOwnerPtr(), RowLimit: 3}},
		{"no filters at all", ListCollectionsPageGatedParams{RowLimit: 500}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ListCollectionsPageGated(ctx, pool, caller, c.p)
			if err != nil {
				t.Fatalf("gated: %v", err)
			}
			want, err := q.ListCollectionsPage(ctx, ListCollectionsPageParams{
				IncludeDeleted:  c.p.IncludeDeleted,
				OwnerUserRef:    c.p.OwnerUserRef,
				ExcludeOwner:    c.p.ExcludeOwner,
				Visibility:      c.p.Visibility,
				Featured:        c.p.Featured,
				QName:           c.p.QName,
				SharedWithUser:  c.p.SharedWithUser,
				CursorCreatedAt: c.p.CursorCreatedAt,
				CursorID:        c.p.CursorID,
				RowLimit:        c.p.RowLimit,
			})
			if err != nil {
				t.Fatalf("sqlc oracle: %v", err)
			}
			assertSameCollections(t, filterToSeeded(want, ids), filterToSeeded(got, ids))
		})
	}

	// Cursor paging: walk the set two rows at a time through both
	// implementations and require the same sequence. Paging is where an
	// ordering or tie-break mistake surfaces.
	t.Run("cursor paging walks identically", func(t *testing.T) {
		var gotCursor, wantCursor struct {
			ts pgtype.Timestamptz
			id pgtype.UUID
		}
		for page := 0; page < 4; page++ {
			got, err := ListCollectionsPageGated(ctx, pool, caller, ListCollectionsPageGatedParams{
				OwnerUserRef: collOwnerPtr(), RowLimit: 2,
				CursorCreatedAt: gotCursor.ts, CursorID: gotCursor.id,
			})
			if err != nil {
				t.Fatalf("gated page %d: %v", page, err)
			}
			want, err := q.ListCollectionsPage(ctx, ListCollectionsPageParams{
				OwnerUserRef: collOwnerPtr(), RowLimit: 2,
				CursorCreatedAt: wantCursor.ts, CursorID: wantCursor.id,
			})
			if err != nil {
				t.Fatalf("sqlc page %d: %v", page, err)
			}
			assertSameCollections(t, want, got)
			if len(got) == 0 {
				break
			}
			gotCursor.ts = got[len(got)-1].CreatedAt
			gotCursor.id = got[len(got)-1].ID
			wantCursor.ts = want[len(want)-1].CreatedAt
			wantCursor.id = want[len(want)-1].ID
		}
	})
}

// filterToSeeded drops rows the test didn't seed, so the "no filters"
// parity case compares the two implementations over a set this test
// controls rather than over whatever else the shared database holds.
func filterToSeeded(rows []Collection, ids []uuid.UUID) []Collection {
	keep := map[uuid.UUID]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	out := make([]Collection, 0, len(rows))
	for _, r := range rows {
		if keep[uuid.UUID(r.ID.Bytes)] {
			out = append(out, r)
		}
	}
	return out
}

func assertSameCollections(t *testing.T, want, got []Collection) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("row count: sqlc=%d gated=%d", len(want), len(got))
	}
	for i := range want {
		if want[i].ID != got[i].ID {
			t.Fatalf("row %d: sqlc id=%v gated id=%v — ordering or filtering diverged",
				i, uuid.UUID(want[i].ID.Bytes), uuid.UUID(got[i].ID.Bytes))
		}
		if want[i].Name != got[i].Name || want[i].Visibility != got[i].Visibility ||
			want[i].OwnerUserRef != got[i].OwnerUserRef {
			t.Errorf("row %d (%v): column mismatch between sqlc and gated results",
				i, uuid.UUID(want[i].ID.Bytes))
		}
	}
}
