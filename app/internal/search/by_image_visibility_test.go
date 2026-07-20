// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #210 — the by_image anonymous floor delegates to the shared
// predicate (ADR 0063).
//
// This is an in-package test (package search) because
// filterVisibleAssetIDs is unexported, and it seeds real rows because
// the point is which assets the query actually returns. The load-
// bearing assertion is the TIGHTENING: the old inline SQL admitted any
// non-deleted public asset, so a public-but-DRAFT or public-but-still-
// PROCESSING asset used to surface in anonymous reverse-image results.
// Routing through visibility.Filter drops them, matching every other
// anonymous read path.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
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

	// identity == nil is the anonymous branch under test.
	got, err := filterVisibleAssetIDs(ctx, pool, nil, ids)
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

// TestByImageAuthenticatedFloor_Unchanged pins acceptance 3: the
// authenticated branch still returns every candidate id (row checks
// happen downstream). Delegating the anonymous branch must not touch
// this.
func TestByImageAuthenticatedFloor_Unchanged(t *testing.T) {
	pool := byImagePool(t)
	ctx := context.Background()

	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1,'byimg-auth',4210002,(SELECT MIN(ref) FROM asset_types),'draft','team','processing')`,
		id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id) })

	ref := int64(4210002)
	identity := &auth.Identity{UserRef: ref, AuthMethod: "session"}
	got, err := filterVisibleAssetIDs(ctx, pool, identity, []uuid.UUID{id})
	if err != nil {
		t.Fatalf("filterVisibleAssetIDs: %v", err)
	}
	// A draft/team/processing asset that the anonymous floor would drop
	// must STILL be returned to an authenticated caller — the branch
	// returns all ids and defers to downstream row checks.
	if _, ok := got[id]; !ok {
		t.Error("authenticated caller: candidate id was filtered out; the authenticated branch " +
			"must return all ids and let the asset-detail lookup gate them")
	}
}
