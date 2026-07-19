// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #414 P0a — the entity × caller visibility contract.
//
// This is a CONTRACT test on visibility.ToSQL, not a per-endpoint test,
// and that is deliberate: the predicate is spliced into ~11 hand-built
// read paths (search, facets, suggest, saved-search execution, vector
// kNN), so one branch here governs all of them. Testing it once, at the
// point of definition, is what makes that blast radius safe rather than
// frightening. See ADR 0063.
//
// The fragments are executed against real Postgres rows rather than
// string-matched, because what matters is which rows a caller can
// actually see — a fragment that looks right and selects the wrong rows
// is the failure mode this exists to catch.
//
// Skips without AA_DB_PASSWORD, same convention as the other
// integration suites.

package visibility

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func matrixPool(t *testing.T) *pgxpool.Pool {
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
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

// visibleIDs runs the predicate against one table and returns which of
// the seeded ids the caller can see.
func visibleIDs(t *testing.T, pool *pgxpool.Pool, entity EntityType, caller Caller, table string, ids []uuid.UUID) map[uuid.UUID]bool {
	t.Helper()
	pred, err := Filter(context.Background(), entity, caller)
	if err != nil {
		t.Fatalf("Filter(%v): %v", entity, err)
	}
	// $1 = the id set; the predicate's own args start at $2.
	frag, args := pred.ToSQL("", 1)
	sql := fmt.Sprintf(`SELECT id FROM %s WHERE id = ANY($1::uuid[])%s`, table, frag)
	all := append([]any{ids}, args...)

	rows, err := pool.Query(context.Background(), sql, all...)
	if err != nil {
		t.Fatalf("query %s: %v\nSQL: %s", table, err, sql)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func anonCaller() Caller { return NewCaller(nil) }
func userCaller(ref int64) Caller {
	return NewCaller(&ref)
}

// --- assets ----------------------------------------------------------

// TestMatrix_Assets pins the asset rule in both directions: anonymous
// callers see ONLY published-public-ready assets, and authenticated
// callers see exactly what they saw before this change (soft-delete
// only) — including assets of non-public sensitivity, which is the
// deliberately-deferred gap.
func TestMatrix_Assets(t *testing.T) {
	pool := matrixPool(t)
	ctx := context.Background()
	const owner int64 = 4140001
	const other int64 = 4140002

	type seed struct {
		name        string
		status      string
		sensitivity string
		processing  string
		deleted     bool
		anonSees    bool // the whole point of the matrix
	}
	seeds := []seed{
		{"public+active+ready", "active", "public", "ready", false, true},
		{"draft", "draft", "public", "ready", false, false},
		{"archived", "archived", "public", "ready", false, false},
		{"sensitivity=team", "active", "team", "ready", false, false},
		{"sensitivity=restricted", "active", "restricted", "ready", false, false},
		{"sensitivity=embargo", "active", "embargo", "ready", false, false},
		{"still processing", "active", "public", "processing", false, false},
		{"soft-deleted", "active", "public", "ready", true, false},
	}

	ids := make([]uuid.UUID, len(seeds))
	for i, s := range seeds {
		id := uuid.New()
		ids[i] = id
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		_, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status, deleted_at)
			VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), $4, $5, $6, %s)`, del),
			id, "vis-matrix-"+s.name, owner, s.status, s.sensitivity, s.processing)
		if err != nil {
			t.Fatalf("seed %q: %v", s.name, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
	})

	t.Run("anonymous sees only published public ready assets", func(t *testing.T) {
		got := visibleIDs(t, pool, EntityAsset, anonCaller(), "assets", ids)
		for i, s := range seeds {
			if got[ids[i]] != s.anonSees {
				verb := "must NOT be"
				if s.anonSees {
					verb = "MUST be"
				}
				t.Errorf("anonymous: %q %s visible (got visible=%v)", s.name, verb, got[ids[i]])
			}
		}
	})

	// The deferred-gap assertions. If someone tightens the
	// authenticated asset branch without deciding the sensitivity rule,
	// these fail loudly — that change would silently move ~11 call
	// sites and break existing callers.
	for _, c := range []struct {
		name   string
		caller Caller
	}{
		{"owner", userCaller(owner)},
		{"authenticated non-owner", userCaller(other)},
	} {
		t.Run(c.name+" sees every non-deleted asset (unchanged behaviour)", func(t *testing.T) {
			got := visibleIDs(t, pool, EntityAsset, c.caller, "assets", ids)
			for i, s := range seeds {
				want := !s.deleted // soft-delete gate ONLY
				if got[ids[i]] != want {
					t.Errorf("%s: %q visible=%v, want %v — authenticated asset visibility "+
						"must stay soft-delete-only until the sensitivity rule is decided (ADR 0063)",
						c.name, s.name, got[ids[i]], want)
				}
			}
		})
	}
}

// --- collections -----------------------------------------------------

func TestMatrix_Collections(t *testing.T) {
	pool := matrixPool(t)
	ctx := context.Background()
	const owner int64 = 4140011
	const other int64 = 4140012

	type seed struct {
		name       string
		visibility string
		deleted    bool
		anonSees   bool
	}
	seeds := []seed{
		{"public", "public", false, true},
		{"private", "private", false, false},
		{"org-only", "org-only", false, false},
		{"followers", "followers", false, false},
		{"public but deleted", "public", true, false},
	}

	ids := make([]uuid.UUID, len(seeds))
	for i, s := range seeds {
		id := uuid.New()
		ids[i] = id
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		_, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO collections (id, name, owner_user_ref, visibility, membership, deleted_at)
			VALUES ($1, $2, $3, $4, 'manual', %s)`, del),
			id, "vis-matrix-"+s.name, owner, s.visibility)
		if err != nil {
			t.Fatalf("seed %q: %v (does migration 00008 allow 'public'?)", s.name, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = ANY($1::uuid[])`, ids)
	})

	t.Run("anonymous sees only public non-deleted collections", func(t *testing.T) {
		got := visibleIDs(t, pool, EntityCollection, anonCaller(), "collections", ids)
		for i, s := range seeds {
			if got[ids[i]] != s.anonSees {
				t.Errorf("anonymous: %q visible=%v, want %v", s.name, got[ids[i]], s.anonSees)
			}
		}
	})

	t.Run("owner sees own collections regardless of visibility (unchanged)", func(t *testing.T) {
		got := visibleIDs(t, pool, EntityCollection, userCaller(owner), "collections", ids)
		for i, s := range seeds {
			if !got[ids[i]] {
				t.Errorf("owner: %q not visible; the owner branch must be unchanged", s.name)
			}
		}
	})

	t.Run("authenticated non-owner sees none of them (no ACL grant)", func(t *testing.T) {
		got := visibleIDs(t, pool, EntityCollection, userCaller(other), "collections", ids)
		for i, s := range seeds {
			if got[ids[i]] {
				t.Errorf("non-owner: %q visible without an ACL grant", s.name)
			}
		}
	})
}

// --- posts -----------------------------------------------------------

func TestMatrix_Posts(t *testing.T) {
	pool := matrixPool(t)
	ctx := context.Background()
	const author int64 = 4140021
	const other int64 = 4140022

	type seed struct {
		name       string
		visibility string
		deleted    bool
		anonSees   bool
	}
	seeds := []seed{
		{"public", "public", false, true},
		{"private", "private", false, false},
		{"org-only", "org-only", false, false},
		{"public but deleted", "public", true, false},
	}

	ids := make([]uuid.UUID, len(seeds))
	for i, s := range seeds {
		id := uuid.New()
		ids[i] = id
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		_, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO posts (id, title, author_user_ref, visibility, deleted_at)
			VALUES ($1, $2, $3, $4, %s)`, del),
			id, "vis-matrix-"+s.name, author, s.visibility)
		if err != nil {
			t.Fatalf("seed %q: %v (does migration 00008 allow 'public'?)", s.name, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = ANY($1::uuid[])`, ids)
	})

	t.Run("anonymous sees only public non-deleted posts", func(t *testing.T) {
		got := visibleIDs(t, pool, EntityPost, anonCaller(), "posts", ids)
		for i, s := range seeds {
			if got[ids[i]] != s.anonSees {
				t.Errorf("anonymous: %q visible=%v, want %v", s.name, got[ids[i]], s.anonSees)
			}
		}
	})

	t.Run("author sees own non-deleted posts (unchanged)", func(t *testing.T) {
		got := visibleIDs(t, pool, EntityPost, userCaller(author), "posts", ids)
		for i, s := range seeds {
			want := !s.deleted
			if got[ids[i]] != want {
				t.Errorf("author: %q visible=%v, want %v", s.name, got[ids[i]], want)
			}
		}
	})

	t.Run("authenticated non-author sees only public (unchanged)", func(t *testing.T) {
		got := visibleIDs(t, pool, EntityPost, userCaller(other), "posts", ids)
		for i, s := range seeds {
			want := s.visibility == "public" && !s.deleted
			if got[ids[i]] != want {
				t.Errorf("non-author: %q visible=%v, want %v", s.name, got[ids[i]], want)
			}
		}
	})
}

// TestAnonymousBranchesBindNoArgs pins the composition property every
// splice site depends on: the anonymous fragments introduce no
// placeholders, so appending their (empty) args last cannot shift any
// caller's numbering.
func TestAnonymousBranchesBindNoArgs(t *testing.T) {
	for _, e := range []EntityType{EntityAsset, EntityCollection, EntityPost} {
		pred, err := Filter(context.Background(), e, anonCaller())
		if err != nil {
			t.Fatalf("Filter(%v): %v", e, err)
		}
		frag, args := pred.ToSQL("a", 7)
		if len(args) != 0 {
			t.Errorf("%v anonymous: binds %d args, want 0 — a non-zero count shifts placeholders at every splice site", e, len(args))
		}
		if containsPlaceholder(frag) {
			t.Errorf("%v anonymous fragment contains a $-placeholder but binds no args: %s", e, frag)
		}
	}
}

func containsPlaceholder(s string) bool {
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '$' && s[i+1] >= '0' && s[i+1] <= '9' {
			return true
		}
	}
	return false
}
