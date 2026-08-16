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
	"github.com/mscrnt/artist-alley/app/internal/visibility"
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

// hubTab calls the real endpoint AS `callerRef` and returns the ids it
// produced.
//
// The caller is a PARAMETER as of #1121, because that issue's whole
// question is whether two DIFFERENT viewers get different answers about
// the same row — a helper hardcoding one identity can only ever assert
// a single verdict, and a single verdict passes on a tab that admits
// everyone as readily as on one that admits nobody.
func hubTab(
	t *testing.T,
	pool *pgxpool.Pool,
	tab openapi.ListCollectionsParamsTab,
	callerRef int64,
) map[uuid.UUID]bool {
	t.Helper()
	h := collections.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: callerRef, AuthMethod: "session"})

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

	got := hubTab(t, pool, openapi.ListCollectionsParamsTabPublic, hubOwner)

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

	got := hubTab(t, pool, openapi.ListCollectionsParamsTabFeatured, hubOwner)

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

// hubRail reports whether the collection is on the browse rail for
// `callerRef` (0 = anonymous).
//
// The rail is in this file because #1121 is a DISAGREEMENT between two
// surfaces, and a disagreement cannot be asserted from one of them. The
// hub tab's verdict alone is compatible with any rail behaviour at all.
func hubRail(t *testing.T, pool *pgxpool.Pool, callerRef int64, id uuid.UUID) bool {
	t.Helper()
	var caller visibility.Caller
	if callerRef == 0 {
		caller = visibility.NewCaller(nil)
	} else {
		ref := callerRef
		caller = visibility.NewCaller(&ref)
	}
	rows, err := featured.ListPublicRail(context.Background(), pool, caller,
		visibility.PostCaps{}, 500, []string{"col", "preview", "screen", "hires"})
	if err != nil {
		t.Fatalf("browse rail: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.SubjectID.Bytes) == id {
			return true
		}
	}
	return false
}

// TestHubTabs_FeaturedHasNoTierPin is #1121's acceptance pair, and it is
// a SAME-ITEM-OPPOSITE-VERDICTS pair rather than two assertions about
// two rows.
//
// ONE org-only collection, featured at scope=org, asked about by TWO
// viewers:
//
//	the owner       — may read it → present on the tab AND the rail
//	a stranger      — may not     → absent from BOTH
//
// Both halves are load-bearing and neither is sufficient. Without the
// first, a tab that returned nothing at all would pass. Without the
// second, a tab that had genuinely dropped its read gate along with its
// tier pin would pass — and that is the failure mode a "just remove the
// conjunct" change actually risks, so it is the one that has to be
// asserted rather than reasoned about.
//
// The RAIL assertions are the point of the issue. Before #1121 the
// owner's row was on the rail and off the tab: one placement, two
// surfaces, two answers. After it, each viewer gets the same answer from
// both, which is the property #1104 established for scope and this
// extends to tier.
func TestHubTabs_FeaturedHasNoTierPin(t *testing.T) {
	pool := hubPool(t)
	// The row the issue is about: NOT install-public, featured through
	// the audience POST /admin/featured writes by default.
	orgOnly := hubSeed(t, pool, "1121 hub featured org-only", "org-only", featured.ScopeOrg)
	// The control. A public collection featured the same way — it was
	// visible before this change and must still be, or the tab has been
	// broken in the other direction.
	control := hubSeed(t, pool, "1121 hub featured public", "public", featured.ScopeOrg)

	// ── Arm 1: the owner, who may read an org-only collection they own.
	owner := hubTab(t, pool, openapi.ListCollectionsParamsTabFeatured, hubOwner)
	if !owner[orgOnly] {
		t.Error("the Featured tab omitted an ORG-ONLY collection the caller owns and an admin " +
			"featured. This is #1121: the tab pinned visibility='public' on top of the read " +
			"predicate, so a tier the viewer is entitled to was filtered out anyway — and the " +
			"rail, which has no such pin, showed the very same placement.")
	}
	if !owner[control] {
		t.Error("the Featured tab omitted a PUBLIC featured collection; dropping the tier pin " +
			"has broken the tab in the other direction and the assertion above proves nothing")
	}
	if !hubRail(t, pool, hubOwner, orgOnly) {
		t.Error("the rail omitted the org-only placement for its owner — the surface the tab " +
			"is being brought into agreement WITH does not have the behaviour claimed, so " +
			"this test is measuring against the wrong baseline")
	}

	// ── Arm 2: a stranger, who may not read it. Same rows, same
	//    placements, opposite verdict — and the read gate is the ONLY
	//    thing that can produce it now.
	const stranger int64 = 11211121
	other := hubTab(t, pool, openapi.ListCollectionsParamsTabFeatured, stranger)
	if other[orgOnly] {
		t.Error("the Featured tab handed an ORG-ONLY collection to a viewer who neither owns it " +
			"nor holds an ACL on it. Removing the tier pin must not remove the READ gate: " +
			"featuring is a placement, never a grant.")
	}
	if !other[control] {
		t.Error("the Featured tab hid a PUBLIC featured collection from a signed-in stranger; " +
			"the refusal above is then just 'this tab returns nothing' and asserts nothing")
	}
	if hubRail(t, pool, stranger, orgOnly) {
		t.Error("the rail handed the org-only collection to a stranger; the two surfaces now " +
			"agree, but on the wrong answer")
	}
}
