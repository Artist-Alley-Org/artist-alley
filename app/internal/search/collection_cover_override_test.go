// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1027 — the curator's chosen collection cover, seen from /search.
//
// The override is implemented once, inside collections.ComposeCovers,
// precisely so its two production callers cannot disagree. This file is
// what makes that claim testable rather than merely stated: search
// reaches ComposeCovers from an engine holding no *auth.Identity, having
// resolved the caller triple at the HTTP edge, so it is the caller most
// likely to be left behind by a change made "in the handler".
//
// Skips without AA_DB_PASSWORD, matching the rest of this package.

package search

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ccoToken = "quimbolate1027"

const (
	ccoOwner    = int64(10270001)
	ccoStranger = int64(10270002)
)

// ccoRenderableAsset plants an asset that can actually paint a tile:
// storage object, `col` variant, active + ready. `sensitivity` is the
// only knob, so flipping it changes exactly one thing.
func ccoRenderableAsset(t *testing.T, pool *pgxpool.Pool, owner int64, sensitivity string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	raw := uuid.New()
	hash := hex.EncodeToString(raw[:]) + hex.EncodeToString(raw[:])
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 1, 'fs')`,
		hash); err != nil {
		t.Fatalf("seed storage object: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 1)`,
		hash); err != nil {
		t.Fatalf("seed col variant: %v", err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, file_hash,
		                    status, sensitivity, processing_status)
		VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), $4, 'active', $5, 'ready')`,
		id, ccoToken+" asset", owner, hash, sensitivity); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

// ccoCoversOf runs the real engine as `caller` and returns the cover
// asset ids on the collection hit, read out of the served `extra` bag
// rather than from a query — the bag is what a client actually paints.
func ccoCoversOf(t *testing.T, pool *pgxpool.Pool, caller int64, collID uuid.UUID) []string {
	t.Helper()
	ref := caller
	res, err := (&Engine{Pool: pool}).Run(context.Background(), Query{
		Text:          ccoToken,
		CallerUserRef: &ref,
		Types:         []HitType{HitTypeCollection},
		Limit:         25,
	})
	if err != nil {
		t.Fatalf("collection search as %d: %v", caller, err)
	}
	for _, h := range res.Hits {
		if h.ID != collID {
			continue
		}
		if len(h.ExtraJSON) == 0 {
			return nil
		}
		var extra struct {
			Covers []struct {
				AssetID string `json:"asset_id"`
			} `json:"covers"`
		}
		if err := json.Unmarshal(h.ExtraJSON, &extra); err != nil {
			t.Fatalf("collection hit extras are not an object: %v", err)
		}
		out := make([]string, 0, len(extra.Covers))
		for _, c := range extra.Covers {
			out = append(out, c.AssetID)
		}
		return out
	}
	t.Fatalf("collection %s absent from a search as %d — the fixture is wrong and every "+
		"assertion below would be meaningless", collID, caller)
	return nil
}

// ccoSeed plants a PUBLIC collection (so both callers can reach it),
// one member both may picture, and returns the pieces.
func ccoSeed(t *testing.T, pool *pgxpool.Pool) (collID, member uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	collID = uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility)
		VALUES ($1, $2, $3, '', 'public')`,
		collID, ccoOwner, ccoToken+" collection"); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, collID)
	})
	member = ccoRenderableAsset(t, pool, ccoOwner, "public")
	if _, err := pool.Exec(ctx, `
		INSERT INTO collection_resources (collection_id, asset_id, sort_order, pinned)
		VALUES ($1, $2, 0, TRUE)`, collID, member); err != nil {
		t.Fatalf("pin member: %v", err)
	}
	return collID, member
}

// TestSearchCollectionHit_HonoursTheChosenCover — acceptance 2 on the
// third surface. The mosaic and the override are composed in the same
// function, but search binds its own caller triple and caches on it, so
// "it works on /collections" is not evidence it works here.
func TestSearchCollectionHit_HonoursTheChosenCover(t *testing.T) {
	pool := byImagePool(t)
	collID, member := ccoSeed(t, pool)

	if got := ccoCoversOf(t, pool, ccoOwner, collID); len(got) != 1 || got[0] != member.String() {
		t.Fatalf("precondition: derived mosaic on /search = %v, want [%s]", got, member)
	}

	// Not a member — the free pointer is the design, and a member would
	// not distinguish it from the mosaic.
	chosen := ccoRenderableAsset(t, pool, ccoOwner, "public")
	if _, err := pool.Exec(context.Background(),
		`UPDATE collections SET cover_asset_id = $1 WHERE id = $2`, chosen, collID); err != nil {
		t.Fatalf("set cover: %v", err)
	}
	if got := ccoCoversOf(t, pool, ccoOwner, collID); len(got) != 1 || got[0] != chosen.String() {
		t.Errorf("/search covers = %v, want the chosen cover [%s] as the sole entry", got, chosen)
	}

	// And clearing it puts the mosaic back on this surface too.
	if _, err := pool.Exec(context.Background(),
		`UPDATE collections SET cover_asset_id = NULL WHERE id = $1`, collID); err != nil {
		t.Fatalf("clear cover: %v", err)
	}
	if got := ccoCoversOf(t, pool, ccoOwner, collID); len(got) != 1 || got[0] != member.String() {
		t.Errorf("/search covers after clearing = %v, want the derived mosaic [%s]", got, member)
	}
}

// TestSearchCollectionHit_WithheldCoverFallsBackToTheMosaic is the
// #1027 case that would silently not work, asserted on the surface with
// its own caller plumbing and its own result cache.
//
// A cache keyed on the caller triple is what keeps this honest; a cache
// that keyed on the collection alone would serve the curator's view of
// the cover to the next stranger who searched, which is the failure
// this assertion catches.
func TestSearchCollectionHit_WithheldCoverFallsBackToTheMosaic(t *testing.T) {
	pool := byImagePool(t)
	collID, member := ccoSeed(t, pool)

	secret := ccoRenderableAsset(t, pool, ccoOwner, "restricted")
	if _, err := pool.Exec(context.Background(),
		`UPDATE collections SET cover_asset_id = $1 WHERE id = $2`, secret, collID); err != nil {
		t.Fatalf("set cover: %v", err)
	}

	if got := ccoCoversOf(t, pool, ccoOwner, collID); len(got) != 1 || got[0] != secret.String() {
		t.Fatalf("the curator's own /search view = %v, want their chosen cover [%s] — "+
			"without this leg, an engine that dropped every override would pass below",
			got, secret)
	}

	got := ccoCoversOf(t, pool, ccoStranger, collID)
	for _, g := range got {
		if g == secret.String() {
			t.Fatalf("the chosen cover %s reached a /search caller who may not picture it "+
				"(covers=%v)", secret, got)
		}
	}
	if len(got) != 1 || got[0] != member.String() {
		t.Errorf("stranger's /search covers = %v, want the derived mosaic [%s]. An empty "+
			"array is a blank tile — a withheld cover must FALL BACK", got, member)
	}
}
