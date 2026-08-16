// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1115 — the post-level mature derivation (migration 00052, ADR 0090 §4).
//
// # Why this is asserted against the DATABASE
//
// `posts.mature` is a DERIVED COPY, and #902's rule is that a derived
// copy must be maintained at write time rather than recomputed per
// reader. That decision is only worth anything if the maintenance
// actually fires, and the only thing that can tell us it fired is the
// stored row — a handler's echo of its own input passes on a trigger
// that does nothing (#946).
//
// So every assertion here re-SELECTs the column after the write. None
// of them go through Go code at all: the point is that the DATABASE
// keeps the value true, no matter which call site did the writing,
// including call sites that do not exist yet. That is the whole reason
// the maintenance is a trigger and not a write-path hook.
//
// # The paths, and why each is here
//
// A trigger on `post_assets` alone would be a plausible and incomplete
// implementation: an asset's OWN flag can change, and an asset can be
// soft-deleted, and neither touches a membership row. Both of those are
// separate arms below, and both fail against the `post_assets`-only
// version.
//
// Skips without AA_DB_PASSWORD, like every other integration test here.

package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const mdOwner int64 = 11150001

func mdPool(t *testing.T) *pgxpool.Pool {
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
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
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

// mdSeed plants a post and an asset, unlinked, and returns both ids.
func mdSeed(t *testing.T, pool *pgxpool.Pool, assetMature bool) (postID, assetID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	postID, assetID = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'md', 'public')`,
		postID, mdOwner); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, mature)
		 VALUES ($1, 'md asset', 1, $2, $3)`,
		assetID, mdOwner, assetMature); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, assetID)
	})
	return postID, assetID
}

// mdPostMature reads the PERSISTED value. Not the handler's answer, not
// a recomputation — the column.
func mdPostMature(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) bool {
	t.Helper()
	var m bool
	if err := pool.QueryRow(context.Background(),
		`SELECT mature FROM posts WHERE id = $1`, postID).Scan(&m); err != nil {
		t.Fatalf("read posts.mature: %v", err)
	}
	return m
}

func mdLink(t *testing.T, pool *pgxpool.Pool, postID, assetID uuid.UUID, order int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, $3)`,
		postID, assetID, order); err != nil {
		t.Fatalf("link asset: %v", err)
	}
}

// TestMatureDerivation_MemberAddAndRemove is the arm the issue names:
// the derived value asserted from the DB after a member is added and
// after one is removed.
func TestMatureDerivation_MemberAddAndRemove(t *testing.T) {
	pool := mdPool(t)
	postID, matureAsset := mdSeed(t, pool, true)

	if mdPostMature(t, pool, postID) {
		t.Fatal("a post with no members is mature — the DEFAULT is wrong, not the trigger")
	}

	mdLink(t, pool, postID, matureAsset, 0)
	if !mdPostMature(t, pool, postID) {
		t.Error("adding a mature member did not make the post mature — the post_assets " +
			"trigger did not fire, or the derivation is inverted")
	}

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM post_assets WHERE post_id = $1 AND asset_id = $2`, postID, matureAsset); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if mdPostMature(t, pool, postID) {
		t.Error("removing the only mature member left the post mature — the DELETE arm of " +
			"the trigger is missing, which is the half a set-on-insert implementation forgets")
	}
}

// TestMatureDerivation_AnyMemberNotAll pins the ANY rule.
//
// A bundle containing ONE mature piece is a bundle a disqualified viewer
// must not be handed, so a post with one mature member and three clean
// ones is mature. An `ALL` implementation passes the test above and
// fails here.
func TestMatureDerivation_AnyMemberNotAll(t *testing.T) {
	pool := mdPool(t)
	postID, matureAsset := mdSeed(t, pool, true)

	// Three clean members alongside the mature one.
	for i := 0; i < 3; i++ {
		_, clean := mdSeed(t, pool, false)
		mdLink(t, pool, postID, clean, i)
	}
	if mdPostMature(t, pool, postID) {
		t.Fatal("three clean members made the post mature")
	}

	mdLink(t, pool, postID, matureAsset, 9)
	if !mdPostMature(t, pool, postID) {
		t.Error("one mature member among four did not make the post mature — the rule is " +
			"ALL rather than ANY, and a bundle would leak its one adult piece")
	}
}

// TestMatureDerivation_AssetFlagFlip is the arm a `post_assets`-only
// trigger fails.
//
// Nothing about the MEMBERSHIP changes here: the asset's own flag does,
// which is exactly what the operator override and the artist's own edit
// do. A post that stayed non-mature through this would serve an adult
// asset to a disqualified viewer.
func TestMatureDerivation_AssetFlagFlip(t *testing.T) {
	pool := mdPool(t)
	postID, assetID := mdSeed(t, pool, false)
	mdLink(t, pool, postID, assetID, 0)

	if mdPostMature(t, pool, postID) {
		t.Fatal("a post whose only member is clean is mature")
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = true WHERE id = $1`, assetID); err != nil {
		t.Fatalf("flip flag: %v", err)
	}
	if !mdPostMature(t, pool, postID) {
		t.Error("flagging the member asset did not propagate to the post — the assets " +
			"trigger is missing, and a `post_assets`-only implementation passes every " +
			"membership test while leaking here")
	}

	// …and back, because a one-way propagation is its own bug: an
	// operator who clears a mis-flag must actually clear it.
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = false WHERE id = $1`, assetID); err != nil {
		t.Fatalf("unflip: %v", err)
	}
	if mdPostMature(t, pool, postID) {
		t.Error("clearing the member's flag left the post mature — the propagation is one-way")
	}
}

// TestMatureDerivation_SoftDeletedMemberDropsOut is the second arm no
// membership trigger can see: a soft-delete writes no `post_assets` row
// at all.
//
// A deleted member is not a member — the same rule every contents
// listing applies — so a post whose only mature piece has been deleted
// is no longer mature, and restoring it brings the flag back.
func TestMatureDerivation_SoftDeletedMemberDropsOut(t *testing.T) {
	pool := mdPool(t)
	postID, assetID := mdSeed(t, pool, true)
	mdLink(t, pool, postID, assetID, 0)

	if !mdPostMature(t, pool, postID) {
		t.Fatal("linking a mature asset did not make the post mature")
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = now() WHERE id = $1`, assetID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if mdPostMature(t, pool, postID) {
		t.Error("soft-deleting the only mature member left the post mature — the derivation " +
			"is not excluding deleted members, or the assets trigger does not watch deleted_at")
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = NULL WHERE id = $1`, assetID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !mdPostMature(t, pool, postID) {
		t.Error("restoring the member did not restore the post's flag — a restore has to " +
			"re-derive, or a restored adult asset comes back inside a post marked clean")
	}
}

// TestMatureDerivation_MultiplePostsSharingAnAsset pins the loop in the
// assets trigger. One asset can be a member of many posts, and a
// `SELECT post_id … LIMIT 1` would update exactly one of them.
func TestMatureDerivation_MultiplePostsSharingAnAsset(t *testing.T) {
	pool := mdPool(t)
	postA, assetID := mdSeed(t, pool, false)
	postB, _ := mdSeed(t, pool, false)

	mdLink(t, pool, postA, assetID, 0)
	mdLink(t, pool, postB, assetID, 0)

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = true WHERE id = $1`, assetID); err != nil {
		t.Fatalf("flip flag: %v", err)
	}
	if !mdPostMature(t, pool, postA) || !mdPostMature(t, pool, postB) {
		t.Errorf("flagging a shared asset updated postA=%v postB=%v — both hold it, so both "+
			"are mature; the trigger is updating one post rather than every post that "+
			"references the asset",
			mdPostMature(t, pool, postA), mdPostMature(t, pool, postB))
	}
}
