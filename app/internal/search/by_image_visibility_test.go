// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #210 / #1066 — the by_image floor.
//
// #210: the anonymous floor delegates to the shared predicate (ADR
// 0063). The load-bearing assertion there is the TIGHTENING: the old
// inline SQL admitted any non-deleted public asset, so a
// public-but-DRAFT or public-but-still-PROCESSING asset used to surface
// in anonymous reverse-image results.
//
// #1066: the AUTHENTICATED floor is the content plane, not "everything".
// Reverse-image search ranks the catalogue against a picture the caller
// supplied, so a restricted asset coming back with a similarity score
// discloses what that asset looks like — the thing the withheld
// thumbhash and the withheld bytes exist to protect. The tests below
// state that as two callers reaching OPPOSITE verdicts on ONE row: a
// test where both get the same answer proves nothing.
//
// This is an in-package test (package search) because
// filterVisibleAssetIDs is unexported, and it seeds real rows because
// the point is which assets the query actually returns.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

func byImagePool(t *testing.T) *pgxpool.Pool {
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

// TestByImageAnonymousFloor_MatchesFullPredicate seeds one asset per
// dimension the predicate cares about and asserts the anonymous filter
// keeps only the fully-visible one.
func TestByImageAnonymousFloor_MatchesFullPredicate(t *testing.T) {
	pool := byImagePool(t)
	ctx := context.Background()

	type seed struct {
		name        string
		status      string
		sensitivity string
		processing  string
		deleted     bool
		wantVisible bool
	}
	seeds := []seed{
		{"active public ready", "active", "public", "ready", false, true},
		{"draft public ready", "draft", "public", "ready", false, false},             // tightening
		{"active public processing", "active", "public", "processing", false, false}, // tightening
		{"active team ready", "active", "team", "ready", false, false},
		{"active public ready deleted", "active", "public", "ready", true, false},
	}

	ids := make([]uuid.UUID, len(seeds))
	want := map[uuid.UUID]bool{}
	for i, s := range seeds {
		id := uuid.New()
		ids[i] = id
		want[id] = s.wantVisible
		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status, deleted_at)
			VALUES ($1,$2,4210001,(SELECT MIN(ref) FROM asset_types),$3,$4,$5,`+del+`)`,
			id, "byimg-"+s.name, s.status, s.sensitivity, s.processing)
		if err != nil {
			t.Fatalf("seed %q: %v", s.name, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
	})

	// The anonymous caller under test.
	got, err := filterVisibleAssetIDs(ctx, pool,
		visibility.NewCaller(nil), visibility.ContentCaps{}, ids)
	if err != nil {
		t.Fatalf("filterVisibleAssetIDs: %v", err)
	}
	for i, s := range seeds {
		_, visible := got[ids[i]]
		if visible != s.wantVisible {
			t.Errorf("%q: visible=%v, want %v — anonymous reverse-image results must match the "+
				"full predicate (active + public + ready + not-deleted)", s.name, visible, s.wantVisible)
		}
	}
}

// TestByImageAuthenticatedFloor_ContentPlane is the #1066 regression
// test, and it is written as a POSITIVE CONTROL: one restricted asset,
// three callers, opposite verdicts.
//
// The assertion is on the RESULT SET, not on any field. The fields of a
// restricted asset are withheld from the payload either way (#899), so a
// field assertion passes on the bug — what leaks is that the asset RANKED
// at all, which is a property of the picture the caller supplied.
//
// It fails on the pre-fix build, where the authenticated branch returned
// every candidate id it was handed.
func TestByImageAuthenticatedFloor_ContentPlane(t *testing.T) {
	pool := byImagePool(t)
	ctx := context.Background()

	const owner int64 = 4210002
	const stranger int64 = 4210003

	// A restricted asset, and a public one beside it. The public asset is
	// the non-vacuity control: if the stranger sees NEITHER, the filter
	// is simply returning nothing and the restricted assertion is empty.
	restricted, public := uuid.New(), uuid.New()
	for _, s := range []struct {
		id          uuid.UUID
		title       string
		sensitivity string
	}{
		{restricted, "byimg-restricted", "restricted"},
		{public, "byimg-public", "public"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready')`,
			s.id, s.title, owner, s.sensitivity); err != nil {
			t.Fatalf("seed %s: %v", s.title, err)
		}
	}
	ids := []uuid.UUID{restricted, public}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
	})

	ask := func(t *testing.T, caller visibility.Caller, caps visibility.ContentCaps) map[uuid.UUID]struct{} {
		t.Helper()
		got, err := filterVisibleAssetIDs(ctx, pool, caller, caps, ids)
		if err != nil {
			t.Fatalf("filterVisibleAssetIDs: %v", err)
		}
		return got
	}

	ownerRef, strangerRef := owner, stranger
	for _, tc := range []struct {
		name           string
		caller         visibility.Caller
		caps           visibility.ContentCaps
		wantRestricted bool
	}{
		// The verdict #1066 is about.
		{"authenticated stranger", visibility.NewCaller(&strangerRef), visibility.ContentCaps{}, false},
		// The counterweights. Without these, "return nothing" passes.
		{"owner", visibility.NewCaller(&ownerRef), visibility.ContentCaps{}, true},
		{"content.read.all", visibility.NewCaller(&strangerRef), visibility.ContentCaps{ContentReadAll: true}, true},
		{"system.admin", visibility.NewCaller(&strangerRef), visibility.ContentCaps{SystemAdmin: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ask(t, tc.caller, tc.caps)
			if _, ok := got[public]; !ok {
				t.Fatal("the PUBLIC asset was filtered out — the floor over-narrowed and every " +
					"assertion below would pass vacuously")
			}
			if _, ok := got[restricted]; ok != tc.wantRestricted {
				if tc.wantRestricted {
					t.Errorf("restricted asset absent: %s must still rank it — gating reverse-image "+
						"search must not take the catalogue away from a caller entitled to it", tc.name)
				} else {
					t.Errorf("restricted asset RANKED for %s: an asset whose picture this caller may "+
						"not read scored against a picture they supplied, which discloses what it "+
						"looks like (#1066)", tc.name)
				}
			}
		})
	}
}

// TestByImageAnonymousFloor_Unchanged_ByContentPlane pins acceptance 4:
// composing the content plane for EVERY caller rather than branching must
// not move the anonymous answer. For a caller with no ref and no caps the
// content plane reduces to sensitivity='public', which the predicate's
// anonymous branch already demanded.
func TestByImageAnonymousFloor_Unchanged_ByContentPlane(t *testing.T) {
	pool := byImagePool(t)
	ctx := context.Background()

	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1,'byimg-anon-public',4210004,(SELECT MIN(ref) FROM asset_types),'active','public','ready')`,
		id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id) })

	got, err := filterVisibleAssetIDs(ctx, pool,
		visibility.NewCaller(nil), visibility.ContentCaps{}, []uuid.UUID{id})
	if err != nil {
		t.Fatalf("filterVisibleAssetIDs: %v", err)
	}
	if _, ok := got[id]; !ok {
		t.Error("anonymous caller lost a public/active/ready asset — the #1066 conjunct narrowed " +
			"the anonymous path, which it must not")
	}
}
