// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #478 slice-2 — post-by-asset lookup visibility.
//
// An asset can be a member of several posts at different visibility
// tiers. GetPostsByAsset must return only what the caller may see: an
// anonymous viewer gets the public posts featuring the asset and nothing
// members-only; an authenticated viewer additionally sees the
// walled-garden tier — the same gating as the feed, no new plane.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func TestGetPostsByAsset_AnonymousSeesOnlyPublic(t *testing.T) {
	pool := previewPool(t)
	ctx := context.Background()

	asset := seedPreviewAsset(t, pool, "public", true)
	publicPost := seedPreviewPost(t, pool, asset) // visibility 'public'

	// A second post featuring the SAME asset, but members-only.
	orgPost := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,'org post','org-only')`,
		orgPost, pePostOwner); err != nil {
		t.Fatalf("seed org post: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,0)`, orgPost, asset); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, orgPost)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, orgPost)
	})

	h := peHandler(pool)
	req := openapi.GetPostsByAssetRequestObject{Id: asset}

	// --- anonymous: only the public post ---
	anonResp, err := h.GetPostsByAsset(ctx, req)
	if err != nil {
		t.Fatalf("anon: %v", err)
	}
	anon := postIDSet(t, anonResp)
	if !anon[publicPost] {
		t.Errorf("anonymous should see the public post %s; got %v", publicPost, anon)
	}
	if anon[orgPost] {
		t.Errorf("anonymous must NOT see the members-only post %s", orgPost)
	}

	// --- authenticated: both tiers ---
	authed := auth.WithIdentity(ctx, &auth.Identity{UserRef: peStranger, Username: "viewer"})
	authResp, err := h.GetPostsByAsset(authed, req)
	if err != nil {
		t.Fatalf("authed: %v", err)
	}
	got := postIDSet(t, authResp)
	if !got[publicPost] || !got[orgPost] {
		t.Errorf("authenticated viewer should see both posts; got %v", got)
	}
}

func postIDSet(t *testing.T, resp openapi.GetPostsByAssetResponseObject) map[uuid.UUID]bool {
	t.Helper()
	ok, is := resp.(openapi.GetPostsByAsset200JSONResponse)
	if !is {
		t.Fatalf("expected 200, got %T", resp)
	}
	out := map[uuid.UUID]bool{}
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}
