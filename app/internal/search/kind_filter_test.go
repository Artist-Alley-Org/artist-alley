// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1251 — THE `kind:` DIMENSION ON /search.
//
// The browse feed's `?kind=` converged onto the shared grammar, which
// means the dimension is now DEFINED for assets and posts. ADR 0093's
// #1242 amendment records why that is only half of what has to be true:
//
//	⚠️ Decision 3 says a filter is defined once; that is not the same as
//	it being APPLIED everywhere it is defined. Check both when adding an
//	arm.
//
// That amendment exists because `runCollections` had DISCARDED the
// rendered fragment for three releases (`if _, _, ok :=`), inert only
// while no dimension was satisfiable there. So this file checks the
// application, on the surface the feed's tests cannot see.
//
// Two things are at stake and only one of them is convenience:
//
//  1. The filter narrows /search at all, for both entities it is defined
//     for, and the count travels with the array.
//
//  2. ⛔ THE PER-MEMBER GATE HOLDS HERE TOO. posts.kind_filter_test.go's
//     no-probe assertion protects the FEED. A dimension shared between
//     two surfaces is exactly the shape where a rule gets carried on one
//     of them, so the same recovery-by-elimination attack is run against
//     the engine: a public post holding a restricted PNG and a public
//     MP4 must answer `kind:video` and no other kind, for a stranger.
//     If that only worked on /posts, the leak would simply have moved.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
)

const (
	kdOwner    int64 = 12510101
	kdStranger int64 = 12510102
)

// kdPhrase is in the title of every fixture row and nowhere else in any
// developer's database, so every count here is attributable to this
// fixture alone.
const kdPhrase = "thrimblesnatch"

func kdPool(t *testing.T) *pgxpool.Pool {
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

type kdFixture struct {
	imageAsset, videoAsset, secretImage uuid.UUID
	imagePost, videoPost, mixedPost     uuid.UUID
	collection                          uuid.UUID
}

// kdAsset plants one asset. `assetType` is a NON-OVERRIDING ref
// throughout this fixture, so every kind here is decided by the
// EXTENSION — which is the half a filter keyed on `asset_type` would get
// wrong, and the half the badge actually draws from.
func kdAsset(t *testing.T, pool *pgxpool.Pool, label, ext, sensitivity string, assetType, owner int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type,
		                    status, sensitivity, processing_status, file_extension)
		VALUES ($1, $2, '', $3, $4, 'active', $5, 'ready', $6)`,
		id, kdPhrase+" "+label, owner, assetType, sensitivity, ext); err != nil {
		t.Fatalf("seed asset %s: %v", label, err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, id, `DELETE FROM assets WHERE id = $1`)
	})
	return id
}

func kdPost(t *testing.T, pool *pgxpool.Pool, label string, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`, id, kdOwner, kdPhrase+" "+label); err != nil {
		t.Fatalf("seed post %s: %v", label, err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, id, `DELETE FROM posts WHERE id = $1`)
	})
	for i, m := range members {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`,
			id, m, i); err != nil {
			t.Fatalf("seed membership %s/%d: %v", label, i, err)
		}
	}
	return id
}

func kdSeed(t *testing.T, pool *pgxpool.Pool) kdFixture {
	t.Helper()
	for _, u := range []struct {
		ref  int64
		name string
	}{{kdOwner, "kd-owner-1251"}, {kdStranger, "kd-stranger-1251"}} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO "user" (ref, username) VALUES ($1,$2)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
			u.ref, u.name); err != nil {
			t.Fatalf("seed user %d: %v", u.ref, err)
		}
		ref := u.ref
		t.Cleanup(func() {
			testdb.Purge(t, pool, ref, `DELETE FROM "user" WHERE ref = $1`)
		})
	}

	f := kdFixture{
		imageAsset:  kdAsset(t, pool, "image asset", "png", "public", 1, kdOwner),
		videoAsset:  kdAsset(t, pool, "video asset", "mp4", "public", 3, kdOwner),
		secretImage: kdAsset(t, pool, "secret image", "png", "restricted", 1, kdOwner),
	}
	f.imagePost = kdPost(t, pool, "image post", f.imageAsset)
	f.videoPost = kdPost(t, pool, "video post", f.videoAsset)
	// ⭐ The no-probe fixture, rebuilt on this surface: a PUBLIC post
	// holding a restricted PNG owned by somebody else plus a public MP4.
	// To a stranger the card shows the video and nothing at all about the
	// PNG.
	f.mixedPost = kdPost(t, pool, "mixed post", f.secretImage, f.videoAsset)

	f.collection = uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility)
		VALUES ($1,$2,$3,$3,'public')`,
		f.collection, kdOwner, kdPhrase+" collection"); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, f.collection, `DELETE FROM collections WHERE id = $1`)
	})
	return f
}

// kdRun executes one search over all three entity types as `caller`.
func kdRun(t *testing.T, pool *pgxpool.Pool, caller int64, filters ...string) []Hit {
	t.Helper()
	sel, err := facet.ParseSelection(filters)
	if err != nil {
		t.Fatalf("parse %v: %v", filters, err)
	}
	q := Query{
		Text:    kdPhrase,
		Types:   AllHitTypes(),
		Limit:   50,
		Filters: sel,
	}
	if caller != 0 {
		ref := caller
		q.CallerUserRef = &ref
	}
	res, err := NewEngine(pool).Run(context.Background(), q)
	if err != nil {
		t.Fatalf("run %v as %d: %v", filters, caller, err)
	}
	if res.TotalCount != len(res.Hits) {
		t.Errorf("total_count %d but %d hits for %v — a filter that narrows one and "+
			"not the other turns the count into an oracle the hits are not",
			res.TotalCount, len(res.Hits), filters)
	}
	return res.Hits
}

func kdHas(hits []Hit, id uuid.UUID) bool {
	for _, h := range hits {
		if h.ID == id {
			return true
		}
	}
	return false
}

// TestKindFilter_SearchAppliesTheDimension is the "defined is not
// applied" check. It runs the same `kind:` token the feed now composes
// against /search and asserts it narrows BOTH entities the dimension is
// defined for — and that a collection, which has no badge kind, drops
// out rather than surviving as an unconstrained row.
func TestKindFilter_SearchAppliesTheDimension(t *testing.T) {
	pool := kdPool(t)
	f := kdSeed(t, pool)

	// Unfiltered: everything the phrase matches is on the page, so the
	// numbers below are narrowings of a known superset rather than
	// coincidences.
	all := kdRun(t, pool, kdOwner)
	for _, id := range []uuid.UUID{f.imageAsset, f.videoAsset, f.imagePost, f.videoPost, f.mixedPost, f.collection} {
		if !kdHas(all, id) {
			t.Fatalf("the unfiltered search is missing %v; the fixture is not what "+
				"this test assumes", id)
		}
	}

	images := kdRun(t, pool, kdOwner, "kind:image")
	if !kdHas(images, f.imageAsset) {
		t.Error("kind:image did not return the PNG asset — the dimension is defined " +
			"for assets and must be applied there")
	}
	if !kdHas(images, f.imagePost) {
		t.Error("kind:image did not return the post holding the PNG")
	}
	if kdHas(images, f.videoAsset) || kdHas(images, f.videoPost) {
		t.Error("kind:image returned video rows — the fragment is rendered and not applied")
	}
	// A collection has no badge kind, so it is UNSATISFIABLE for this
	// dimension and leaves the page entirely — zero hits and zero count,
	// which is the positive-narrowing direction facet.FacetKind's doc
	// distinguishes from facet.FacetAI's exclusion.
	if kdHas(images, f.collection) {
		t.Error("kind:image returned a collection; a collection resolves to no badge kind")
	}

	videos := kdRun(t, pool, kdOwner, "kind:video")
	if !kdHas(videos, f.videoAsset) || !kdHas(videos, f.videoPost) {
		t.Error("kind:video did not return the MP4 asset and its post")
	}
	if kdHas(videos, f.imageAsset) || kdHas(videos, f.imagePost) {
		t.Error("kind:video returned image rows")
	}

	// A kind nothing resolves to is an empty page, not an error and not
	// the whole corpus.
	if got := kdRun(t, pool, kdOwner, "kind:sequence"); len(got) != 0 {
		t.Errorf("kind:sequence returned %d hits; no asset resolves to it", len(got))
	}
}

// TestKindFilter_SearchRestrictedMemberIsNeverProbeable is
// posts.TestKindFilter_RestrictedMemberIsNeverProbeable's assertion,
// re-run against the engine.
//
// The attack is the same and so is the reason it works: a restricted
// member's card shows no kind and no extension, so a filter that could
// still select its post lets a reader recover that member's kind by
// asking for each kind in turn. Sharing the dimension between two
// surfaces is precisely the shape where the rule gets carried on one of
// them, so the property is asserted on both.
//
// The video arm is a POSITIVE assertion in the same loop. Without it an
// implementation that matched nothing at all would pass — which is how
// the feed's own version of this test passed for a weaker reason than it
// looked, under the cover-only rule #1190 replaced.
func TestKindFilter_SearchRestrictedMemberIsNeverProbeable(t *testing.T) {
	pool := kdPool(t)
	f := kdSeed(t, pool)

	// The post itself is public, so a stranger has it unfiltered.
	if !kdHas(kdRun(t, pool, kdStranger), f.mixedPost) {
		t.Fatal("the stranger cannot see the public post at all; the fixture is wrong")
	}

	for _, k := range viewkind.All() {
		got := kdRun(t, pool, kdStranger, "kind:"+string(k))
		switch k {
		case viewkind.KindVideo:
			if !kdHas(got, f.mixedPost) {
				t.Error("kind:video did not return the post through the member this " +
					"caller CAN read — a filter that matches nothing passes the leak " +
					"half of this test for the wrong reason")
			}
		default:
			if kdHas(got, f.mixedPost) {
				t.Errorf("kind:%s returned a post whose only member of that kind this "+
					"caller may not read — the withheld kind is recoverable by "+
					"elimination (#902/#1066), on /search", k)
			}
		}
		// The ASSET plane's own half of the same rule (#907): a filter
		// asks a question about a column, so a restricted asset must not
		// answer one. The stranger must never see the secret PNG as a hit
		// of its own either.
		if kdHas(got, f.secretImage) {
			t.Errorf("kind:%s returned the restricted asset itself to a stranger", k)
		}
	}

	// The control, on both planes: to the OWNER the PNG is readable, so
	// `kind:image` returns both the asset and the post.
	owned := kdRun(t, pool, kdOwner, "kind:image")
	if !kdHas(owned, f.mixedPost) {
		t.Error("the owner's kind:image did not return their own post — the gate is " +
			"refusing the holder of the right, not the stranger")
	}
	if !kdHas(owned, f.secretImage) {
		t.Error("the owner's kind:image did not return their own restricted asset")
	}
}
