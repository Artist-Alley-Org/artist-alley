// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1104 — the collections hub's tab mapping, at the HANDLER.
//
// The scope split (featured_items.scope) was the cause the issue found
// and is tested at the query in featured_scope_test.go. It was not the
// only one. `GET /collections?tab=featured` still returned nothing
// afterwards, because the tab mapping ALSO pins
// `visibility = 'org-only'` — a value that meant "the top tier" when it
// was written at v0.1.0 and stopped meaning that when migration 00008
// added `public` above it.
//
// ⚠️ THE LESSON THIS FILE EXISTS TO ENCODE: featured_scope_test.go calls
// ListCollectionsPageGated directly with Featured=true, so it passes on
// a tree where the endpoint above it returns an empty page. A test one
// layer below the bug is not a test of the bug. These go through
// Handler.ListCollections, which is what the browser calls.
//
// Skips without AA_DB_PASSWORD.

package collections_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/featured"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const hubOwner int64 = 11041101

func hubPool(t *testing.T) *pgxpool.Pool {
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
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// hubSeed plants a collection at `vis` and, when `scope` is non-empty,
// features it at that audience.
func hubSeed(t *testing.T, pool *pgxpool.Pool, name, vis, scope string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, name, description, owner_user_ref, visibility, membership)
		VALUES ($1,$2,'',$3,$4,'manual')`, id, name, hubOwner, vis); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id)
	})
	if scope != "" {
		if _, err := pool.Exec(ctx, `
			INSERT INTO featured_items (subject_kind, subject_id, position, scope)
			VALUES ('collection',$1,700,$2)`, id, scope); err != nil {
			t.Fatalf("feature %q at %s: %v", name, scope, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM featured_items WHERE subject_id = $1`, id)
		})
	}
	return id
}

// hubTab calls the real endpoint and returns the ids it produced.
func hubTab(t *testing.T, pool *pgxpool.Pool, tab openapi.ListCollectionsParamsTab) map[uuid.UUID]bool {
	t.Helper()
	h := collections.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: hubOwner, AuthMethod: "session"})

	limit := 200
	resp, err := h.ListCollections(ctx, openapi.ListCollectionsRequestObject{
		Params: openapi.ListCollectionsParams{Tab: &tab, Limit: &limit},
	})
	if err != nil {
		t.Fatalf("ListCollections(tab=%s): %v", tab, err)
	}
	ok, isOK := resp.(openapi.ListCollections200JSONResponse)
	if !isOK {
		t.Fatalf("ListCollections(tab=%s) returned %T, want 200", tab, resp)
	}
	out := map[uuid.UUID]bool{}
	for _, c := range ok.Items {
		out[uuid.UUID(c.Id)] = true
	}
	return out
}

// TestHubTabs_PublicTierIsPublicNotOrgOnly is the second cause of the
// owner's report.
//
// The Public tab's contract is "every install-public collection". It
// returned exactly the collections that are NOT install-public, because
// it pinned the tier name that was top-of-ladder before migration 00008
// introduced one above it.
func TestHubTabs_PublicTierIsPublicNotOrgOnly(t *testing.T) {
	pool := hubPool(t)
	pub := hubSeed(t, pool, "1104 hub public", "public", "")
	org := hubSeed(t, pool, "1104 hub org-only", "org-only", "")
	priv := hubSeed(t, pool, "1104 hub private", "private", "")

	got := hubTab(t, pool, openapi.ListCollectionsParamsTabPublic)

	if !got[pub] {
		t.Error("the Public tab omitted a visibility='public' collection. Its description is " +
			"\"every install-public collection\"; `public` is the install-public tier that " +
			"migration 00008 added, and `org-only` — which this tab pinned from v0.1.0 until " +
			"#1104 — is the tier BELOW it.")
	}
	if got[org] {
		t.Error("the Public tab returned an org-only collection; that tier is not install-public")
	}
	if got[priv] {
		t.Error("the Public tab returned a private collection")
	}
}

// TestHubTabs_FeaturedShowsWhatTheAdminFeatured is #1104's acceptance
// criterion 4, driven through the endpoint the browser calls: feature a
// collection through the API's default audience and it appears on this
// tab, with no database surgery.
func TestHubTabs_FeaturedShowsWhatTheAdminFeatured(t *testing.T) {
	pool := hubPool(t)
	// What POST /admin/featured writes by default.
	orgFeatured := hubSeed(t, pool, "1104 hub featured org", "public", featured.ScopeOrg)
	// What the seed writes — the shape the owner actually had.
	pubFeatured := hubSeed(t, pool, "1104 hub featured public", "public", featured.ScopePublic)
	// Public tier, not featured: the negative control that proves the
	// tab still filters on featuring at all.
	notFeatured := hubSeed(t, pool, "1104 hub not featured", "public", "")

	got := hubTab(t, pool, openapi.ListCollectionsParamsTabFeatured)

	if !got[orgFeatured] {
		t.Error("the Featured tab omitted a collection featured at scope=org — which is what " +
			"POST /admin/featured writes by default. This is the owner's reproduction.")
	}
	if !got[pubFeatured] {
		t.Error("the Featured tab omitted a collection featured at scope=public — which is what " +
			"the seed writes, and the shape the owner's install was in.")
	}
	if got[notFeatured] {
		t.Error("the Featured tab returned a collection that is not featured at all; the tab has " +
			"stopped filtering on featuring and the two assertions above are vacuous")
	}
}
