// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1161 / ADR 0091 decision 5 — an asset knows where it appears.
//
// This is the one place in the product where somebody learns something
// about a post they may not read, and ADR 0091's first amendment flags
// it as the part of the model with NO PRIOR ART: on art platforms a
// post's files are always the author's own, so nothing validates a
// wider shape than the one the decision names.
//
// The assertions are therefore about ABSENCE as much as presence. A
// test that only checked `withheld_count == 2` would pass on a payload
// that also carried the two withheld posts' titles, and that payload is
// the failure this endpoint exists to avoid.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	auOwner    int64 = 6620001 // owns the asset
	auStranger int64 = 6620002 // owns two posts the owner cannot read
	auOutsider int64 = 6620003 // neither; must learn nothing at all
)

func auSeedAsset(t *testing.T, pool *pgxpool.Pool, owner int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO assets (id, owner_user_ref, title, asset_type, status, processing_status, sensitivity)
		 VALUES ($1, $2, 'au shared asset', (SELECT MIN(ref) FROM asset_types), 'active', 'ready', 'public')`,
		id, owner); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

// auSeedPostWith plants a post at one tier with the asset as a member.
func auSeedPostWith(t *testing.T, pool *pgxpool.Pool, author int64, vis, title string, asset uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility)
		 VALUES ($1,$2,$3,'au body',$4)`, id, author, title, vis); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,0)`,
		id, asset); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

func auCall(t *testing.T, h *Handler, caller *auth.Identity, asset uuid.UUID) openapi.ListAssetPostsResponseObject {
	t.Helper()
	ctx := t.Context()
	if caller != nil {
		ctx = auth.WithIdentity(ctx, caller)
	}
	resp, err := h.ListAssetPosts(ctx, openapi.ListAssetPostsRequestObject{
		Id: openapi_types.UUID(asset),
	})
	if err != nil {
		t.Fatalf("ListAssetPosts: %v", err)
	}
	return resp
}

func auID(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: ref, AuthMethod: "session", Capabilities: caps}
}

// TestAssetPosts_DisclosesExistenceAndCountOnly is the load-bearing
// test. The owner's asset sits in three posts: one they may read and
// two they may not.
func TestAssetPosts_DisclosesExistenceAndCountOnly(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	asset := auSeedAsset(t, pool, auOwner)
	mine := auSeedPostWith(t, pool, auOwner, "public", "au my own post", asset)
	// Two the owner cannot read: a stranger's `private` post and a
	// stranger's `followers` post they do not follow.
	auSeedPostWith(t, pool, auStranger, "private", "au SECRET private title", asset)
	auSeedPostWith(t, pool, auStranger, "followers", "au SECRET followers title", asset)

	resp := auCall(t, h, auID(auOwner), asset)
	ok, is := resp.(openapi.ListAssetPosts200JSONResponse)
	if !is {
		t.Fatalf("owner got %T, want 200", resp)
	}

	if len(ok.Items) != 1 || uuid.UUID(ok.Items[0].Id) != mine {
		t.Fatalf("items = %d posts, want exactly the owner's own", len(ok.Items))
	}
	if ok.WithheldCount != 2 {
		t.Errorf("withheld_count = %d, want 2", ok.WithheldCount)
	}

	// ABSENCE, asserted over the whole serialised payload rather than
	// over the fields this test happens to know about. A count is easy
	// to get right beside a leak; the leak is whatever else came along.
	raw, err := json.Marshal(openapi.AssetPostUsage(ok))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, forbidden := range []string{
		"SECRET private title",
		"SECRET followers title",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the payload carries a withheld post's TITLE (%q)", forbidden)
		}
	}
	// The withheld posts' AUTHOR must not appear either. The owner's
	// own ref legitimately does (it is on their own post), so this
	// looks for the other one specifically.
	if strings.Contains(body, "6620002") {
		t.Error("the payload carries the withheld posts' author ref")
	}
	// And no handle: the only ids in the body belong to the readable
	// post and its members.
	var probe struct {
		Items []struct {
			Id string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(probe.Items) != 1 {
		t.Errorf("items carries %d entries, want 1", len(probe.Items))
	}
}

// TestAssetPosts_IsTheOwnersQuestion — a caller who does not own the
// asset learns nothing, and learns it in a way that does not confirm
// the asset exists.
func TestAssetPosts_IsTheOwnersQuestion(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	asset := auSeedAsset(t, pool, auOwner)
	auSeedPostWith(t, pool, auStranger, "private", "au outsider probe", asset)

	got := auCall(t, h, auID(auOutsider), asset)
	if _, is := got.(openapi.ListAssetPosts404JSONResponse); !is {
		t.Fatalf("an outsider got %T, want 404", got)
	}

	// Byte-identical to the answer for an asset that does not exist —
	// otherwise the endpoint is an asset-existence probe.
	absent := auCall(t, h, auID(auOutsider), uuid.New())
	gotJSON, _ := json.Marshal(got)
	absentJSON, _ := json.Marshal(absent)
	if string(gotJSON) != string(absentJSON) {
		t.Errorf("refusals differ:\n  someone else's asset: %s\n  nonexistent asset:    %s",
			gotJSON, absentJSON)
	}

	// assets.admin is the documented exception.
	if _, is := auCall(t, h, auID(auOutsider, visibility.AssetsAdmin), asset).(openapi.ListAssetPosts200JSONResponse); !is {
		t.Error("assets.admin was refused; the ADR names it as a holder of this question")
	}
}

// TestAssetPosts_CountsNothingItShouldNot — soft-deleted posts are not
// somewhere an asset appears, and an asset in nobody's post reports
// zero rather than omitting the field.
func TestAssetPosts_CountsNothingItShouldNot(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	asset := auSeedAsset(t, pool, auOwner)

	resp := auCall(t, h, auID(auOwner), asset)
	ok, is := resp.(openapi.ListAssetPosts200JSONResponse)
	if !is {
		t.Fatalf("got %T, want 200", resp)
	}
	if len(ok.Items) != 0 || ok.WithheldCount != 0 {
		t.Errorf("an unused asset reported %d items / %d withheld, want 0 / 0",
			len(ok.Items), ok.WithheldCount)
	}

	dead := auSeedPostWith(t, pool, auStranger, "private", "au deleted", asset)
	if _, err := pool.Exec(t.Context(),
		`UPDATE posts SET deleted_at = NOW() WHERE id=$1`, dead); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	resp = auCall(t, h, auID(auOwner), asset)
	ok = resp.(openapi.ListAssetPosts200JSONResponse)
	if ok.WithheldCount != 0 {
		t.Errorf("a soft-deleted post was counted: withheld_count = %d, want 0", ok.WithheldCount)
	}
}

// TestAssetPosts_AnonymousIsRefused — the endpoint is authenticated,
// and an anonymous caller owns nothing.
func TestAssetPosts_AnonymousIsRefused(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	asset := auSeedAsset(t, pool, auOwner)

	resp, err := h.ListAssetPosts(t.Context(), openapi.ListAssetPostsRequestObject{
		Id: openapi_types.UUID(asset),
	})
	if err != nil {
		t.Fatalf("ListAssetPosts: %v", err)
	}
	if _, is := resp.(openapi.ListAssetPosts401JSONResponse); !is {
		t.Errorf("anonymous got %T, want 401", resp)
	}
}
