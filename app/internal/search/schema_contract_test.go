// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #650 — the unified search engine's SQL is hand-written strings, not
// sqlc, so nothing checks it against the schema at build time. When
// #456 (ADR 0065) dropped `collections.featured`, runCollections kept
// selecting it and EVERY authenticated search 500'd with SQLSTATE
// 42703 for weeks. The suite stayed green because no test ever ran
// Engine.Run against a real database — the failure lives entirely in
// the gap between a Go string and a Postgres catalog.
//
// This file closes that gap: it seeds one row per hit type and runs
// the real engine against the real schema, so any column the engine
// names but the database lacks fails here rather than in production.
// It is deliberately assertion-light about ranking — the point is that
// every branch EXECUTES, and that each still returns its own kind.
//
// Skips without AA_DB_PASSWORD, matching the other integration tests
// in this package.

package search

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaProbeToken is a nonsense word so the seeded rows are the only
// possible matches regardless of what else lives in the dev database.
const schemaProbeToken = "zorbulon650"

// schemaProbeOwner is a user_ref outside any seeded range.
const schemaProbeOwner = int64(6500650)

// seedSchemaProbeRows inserts one asset, one collection and one post
// matching schemaProbeToken and registers their cleanup.
//
// The collection is PRIVATE on purpose: it is visible to its owner and
// invisible to an anonymous caller, which is what lets the same fixture
// pin acceptance 5 (no widening) alongside the fix.
func seedSchemaProbeRows(t *testing.T, pool *pgxpool.Pool) (assetID, collID, postID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	assetID, collID, postID = uuid.New(), uuid.New(), uuid.New()

	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), 'active', 'public', 'ready')`,
		assetID, schemaProbeToken+" asset", schemaProbeOwner); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility)
		VALUES ($1, $2, $3, '', 'private')`,
		collID, schemaProbeOwner, schemaProbeToken+" collection"); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, visibility)
		VALUES ($1, $2, $3, 'public')`,
		postID, schemaProbeOwner, schemaProbeToken+" post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(bg, `DELETE FROM collections WHERE id = $1`, collID)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE id = $1`, assetID)
	})
	return assetID, collID, postID
}

// TestEngineRun_AuthenticatedAgainstRealSchema is the regression test
// for #650. On the pre-fix code it fails at the first assertion with
//
//	search: run collection: ERROR: column c.featured does not exist (SQLSTATE 42703)
//
// which is byte-for-byte the production 500.
func TestEngineRun_AuthenticatedAgainstRealSchema(t *testing.T) {
	pool := byImagePool(t)
	ctx := context.Background()
	assetID, collID, postID := seedSchemaProbeRows(t, pool)

	caller := schemaProbeOwner
	res, err := (&Engine{Pool: pool}).Run(ctx, Query{
		Text:          schemaProbeToken,
		CallerUserRef: &caller,
		Limit:         25,
	})
	if err != nil {
		t.Fatalf("authenticated search errored: %v\n"+
			"Every branch of the engine's hand-written SQL must run against the live schema. "+
			"A column named here but absent from the database 500s every search (#650).", err)
	}

	// Each entity branch must have produced its seeded row. Asserting
	// per-id (not just "some hits") is what makes a silently-dropped
	// branch fail rather than pass on the other two.
	want := map[uuid.UUID]HitType{
		assetID: HitTypeAsset,
		collID:  HitTypeCollection,
		postID:  HitTypePost,
	}
	got := map[uuid.UUID]HitType{}
	for _, h := range res.Hits {
		got[h.ID] = h.Type
	}
	for id, typ := range want {
		if gotType, ok := got[id]; !ok {
			t.Errorf("%s hit %s missing from results — the %s branch did not surface its row", typ, id, typ)
		} else if gotType != typ {
			t.Errorf("hit %s: type=%q want %q", id, gotType, typ)
		}
	}
}

// TestEngineRun_CollectionHitCarriesNoFeaturedFlag pins the #650
// decision: featuring is a placement in featured_items with an
// audience scope (ADR 0065), so a collection hit does not carry a
// boolean `featured` extra. A single bool cannot say WHICH audience a
// placement is for; reintroducing one — even derived via EXISTS —
// rebuilds the column-shaped concept ADR 0065 removed.
func TestEngineRun_CollectionHitCarriesNoFeaturedFlag(t *testing.T) {
	pool := byImagePool(t)
	ctx := context.Background()
	_, collID, _ := seedSchemaProbeRows(t, pool)

	caller := schemaProbeOwner
	res, err := (&Engine{Pool: pool}).Run(ctx, Query{
		Text:          schemaProbeToken,
		CallerUserRef: &caller,
		Types:         []HitType{HitTypeCollection},
		Limit:         25,
	})
	if err != nil {
		t.Fatalf("collection search: %v", err)
	}
	var found bool
	for _, h := range res.Hits {
		if h.ID != collID {
			continue
		}
		found = true
		if len(h.ExtraJSON) == 0 {
			continue
		}
		var extra map[string]any
		if err := json.Unmarshal(h.ExtraJSON, &extra); err != nil {
			t.Fatalf("extras are not an object: %v", err)
		}
		if _, ok := extra["featured"]; ok {
			t.Error("collection hit carries a `featured` extra; featuring is a scoped placement " +
				"in featured_items (ADR 0065), not a boolean on the collection")
		}
	}
	if !found {
		t.Fatalf("seeded collection %s absent from a collection-only search", collID)
	}
}

// TestEngineRun_AnonymousNotWidened pins acceptance 5. Fixing the SQL
// must not change who sees what: an anonymous caller still gets the
// public asset and post but NOT the private collection.
//
// (Whether an anonymous HTTP request reaches the engine at all is a
// separate gate — /search is on the public-mode allowlist, so it 401s
// only while public mode is off. This asserts the layer that decides
// row visibility either way.)
func TestEngineRun_AnonymousNotWidened(t *testing.T) {
	pool := byImagePool(t)
	ctx := context.Background()
	assetID, collID, postID := seedSchemaProbeRows(t, pool)

	res, err := (&Engine{Pool: pool}).Run(ctx, Query{
		Text:  schemaProbeToken,
		Limit: 25,
	})
	if err != nil {
		t.Fatalf("anonymous search errored: %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, h := range res.Hits {
		seen[h.ID] = true
	}
	if seen[collID] {
		t.Error("anonymous caller received a PRIVATE collection — the #650 fix must not widen visibility")
	}
	if !seen[assetID] {
		t.Error("anonymous caller lost the public asset; the fix must not narrow visibility either")
	}
	if !seen[postID] {
		t.Error("anonymous caller lost the public post; the fix must not narrow visibility either")
	}
}
