// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #433 — the binary-plane sensitivity contract (ADR 0064).
//
// This is a contract test on CanReadContent, which is the single rule
// all six byte-streaming handlers call. Testing it once at the point of
// definition is the same argument as ADR 0063's predicate matrix: six
// copies of a security check would be six places to drift, so there is
// one implementation and one test of it.
//
// Skips without AA_DB_PASSWORD.

package visibility

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func contentPool(t *testing.T) *pgxpool.Pool {
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

const (
	ctOwner    int64 = 4330001
	ctStranger int64 = 4330002
	ctMember   int64 = 4330003
)

func notAdmin(string) bool { return false }
func isAdmin(string) bool  { return true }

// seedContentAsset inserts one asset at a tier, optionally with a team
// and optionally ownerless.
func seedContentAsset(t *testing.T, pool *pgxpool.Pool, sensitivity string, teamID *uuid.UUID, ownerless bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	var owner any = ctOwner
	if ownerless {
		owner = nil
	}
	var team any
	if teamID != nil {
		team = *teamID
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, sensitivity, team_id)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),$4,$5)`,
		id, "ct-"+sensitivity, owner, sensitivity, team)
	if err != nil {
		t.Fatalf("seed %s: %v", sensitivity, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

func seedTeamWithMember(t *testing.T, pool *pgxpool.Pool, member int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	teamID := uuid.New()
	slug := "ct-team-" + teamID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, name, slug) VALUES ($1,$2,$3)`, teamID, slug, slug); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1,$2)`, teamID, member); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM team_memberships WHERE team_id=$1`, teamID)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id=$1`, teamID)
	})
	return teamID
}

func can(t *testing.T, pool *pgxpool.Pool, ref int64, caps CapabilityChecker, id uuid.UUID) bool {
	t.Helper()
	r := ref
	ok, err := CanReadContent(context.Background(), pool, NewCaller(&r), caps, id)
	if err != nil {
		return false // fail closed, which is the contract
	}
	return ok
}

// TestCanReadContent_Tiers is the core matrix: for each sensitivity
// tier, who may receive the bytes.
func TestCanReadContent_Tiers(t *testing.T) {
	pool := contentPool(t)

	pub := seedContentAsset(t, pool, "public", nil, false)
	restricted := seedContentAsset(t, pool, "restricted", nil, false)
	embargo := seedContentAsset(t, pool, "embargo", nil, false)

	cases := []struct {
		name          string
		asset         uuid.UUID
		strangerAllow bool
	}{
		{"public is readable by any authenticated caller", pub, true},
		{"restricted denies a non-owner", restricted, false},
		{"embargo denies a non-owner", embargo, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := can(t, pool, ctStranger, notAdmin, c.asset); got != c.strangerAllow {
				t.Errorf("stranger allowed=%v, want %v", got, c.strangerAllow)
			}
			// Owner and admin always pass, at every tier.
			if !can(t, pool, ctOwner, notAdmin, c.asset) {
				t.Errorf("owner was denied their own asset's bytes")
			}
			if !can(t, pool, ctStranger, isAdmin, c.asset) {
				t.Errorf("system.admin was denied")
			}
		})
	}

	// The specific regression the grant deferral could invite: a caller
	// who would pass `restricted` if grants existed must still be denied
	// `embargo`. Both deny today, and embargo must never become the
	// weaker of the two.
	t.Run("embargo is never weaker than restricted", func(t *testing.T) {
		if can(t, pool, ctStranger, notAdmin, embargo) {
			t.Error("embargo admitted a non-owner")
		}
	})
}

// TestCanReadContent_TeamTier covers the tier with a membership lookup,
// including the NULL-team trap.
func TestCanReadContent_TeamTier(t *testing.T) {
	pool := contentPool(t)

	teamID := seedTeamWithMember(t, pool, ctMember)
	teamAsset := seedContentAsset(t, pool, "team", &teamID, false)
	orphanTeamAsset := seedContentAsset(t, pool, "team", nil, false) // team_id IS NULL

	if !can(t, pool, ctMember, notAdmin, teamAsset) {
		t.Error("team member was denied a team-tier asset")
	}
	if can(t, pool, ctStranger, notAdmin, teamAsset) {
		t.Error("non-member received a team-tier asset's bytes")
	}
	// The NULL trap: a team asset with no team has no members, so even
	// somebody who is a member of SOME team must be refused.
	if can(t, pool, ctMember, notAdmin, orphanTeamAsset) {
		t.Error("team asset with team_id IS NULL admitted a caller; NULL must deny")
	}
}

// TestCanReadContent_FailsClosed pins the fail-closed posture on the
// paths that are easy to get wrong.
func TestCanReadContent_FailsClosed(t *testing.T) {
	pool := contentPool(t)

	t.Run("missing asset denies", func(t *testing.T) {
		if can(t, pool, ctStranger, notAdmin, uuid.New()) {
			t.Error("a nonexistent asset id was admitted")
		}
	})

	t.Run("anonymous caller denies even at public", func(t *testing.T) {
		pub := seedContentAsset(t, pool, "public", nil, false)
		ok, err := CanReadContent(context.Background(), pool, NewCaller(nil), notAdmin, pub)
		if err != nil || ok {
			t.Errorf("anonymous admitted (ok=%v err=%v); binary plane is authenticated-only", ok, err)
		}
	})

	t.Run("ownerless restricted asset is admin-only", func(t *testing.T) {
		orphan := seedContentAsset(t, pool, "restricted", nil, true)
		if can(t, pool, ctOwner, notAdmin, orphan) {
			t.Error("NULL owner_user_ref matched a caller; NULL must never match")
		}
		if !can(t, pool, ctStranger, isAdmin, orphan) {
			t.Error("admin denied on an ownerless asset")
		}
	})

	t.Run("unknown sensitivity tier denies", func(t *testing.T) {
		// Bypass the CHECK to simulate a tier added by a future
		// migration that nobody taught this function about.
		ctx := context.Background()
		id := seedContentAsset(t, pool, "public", nil, false)
		if _, err := pool.Exec(ctx,
			`ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_sensitivity_check`); err != nil {
			t.Skipf("cannot relax the constraint here: %v", err)
		}
		defer func() {
			_, _ = pool.Exec(ctx, `UPDATE assets SET sensitivity='public' WHERE id=$1`, id)
			_, _ = pool.Exec(ctx, `ALTER TABLE assets ADD CONSTRAINT assets_sensitivity_check
				CHECK (sensitivity = ANY (ARRAY['public'::text,'team'::text,'restricted'::text,'embargo'::text]))`)
		}()
		if _, err := pool.Exec(ctx, `UPDATE assets SET sensitivity='some_future_tier' WHERE id=$1`, id); err != nil {
			t.Fatalf("set unknown tier: %v", err)
		}
		if can(t, pool, ctStranger, notAdmin, id) {
			t.Error("an unrecognised sensitivity tier defaulted to allow; it must deny")
		}
	})
}
