// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #922 — the post member gate: you may only attach an asset you can
// actually read.
//
// # What was wrong
//
// CreatePost's members loop handled exactly one failure — a foreign-key
// violation on asset_id became a 404 — and had no readability check at
// all. Any authenticated caller could name ANY asset UUID as a member of
// their own post. POST /posts/{id}/assets, the update path, had the same
// hole: it gated the POST (canMutatePost) and never the ASSET.
//
// The severity is precise and should not be overstated. This never
// leaked content: ADR 0064's member conjunction still runs per-caller at
// render time, so a viewer who is not independently entitled sees a
// placeholder carrying the real owner's name. What it permitted is
// unwanted ASSOCIATION — attaching someone's restricted work to your
// post so everyone who IS entitled meets it framed by you.
//
// # What makes this file worth reading
//
// The gate is trivially easy to build so that it gates NOTHING. Per ADR
// 0064 sensitivity lives on the CONTENT plane, so EntityAsset's
// authenticated ROW predicate is `deleted_at IS NULL` and nothing more
// (visibility/predicate.go). A gate built on visibility.CanSee alone
// therefore admits every restricted asset in the instance while looking,
// in review, exactly like a working gate. So the load-bearing case is
// "restricted asset owned by a stranger": row-visible, not
// content-readable.
//
// Every other case exists to stop the cheap ways of passing that one:
//
//   - "public asset owned by a stranger" must still SUCCEED. A
//     deny-everything gate passes every refusal case and collapses the
//     feature into "own assets only" — this is the case that proves the
//     gate is a READ gate and not an ownership gate.
//   - "own restricted asset" — the owner short-circuit, unchanged.
//   - "soft-deleted public asset" — the ROW conjunct on its own account.
//     ContentReadable never reads deleted_at.
//   - a refused member and a nonexistent UUID are asserted to produce
//     the SAME response template, because a distinguishable refusal is a
//     UUID-existence oracle.
//
// Every refusal also asserts that NO post_assets row landed, and for
// CreatePost that no `posts` row landed either. A status-only assertion
// passes just as happily on a handler that rejects AFTER inserting.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	mgAuthor   int64 = 9220001 // does the posting
	mgStranger int64 = 9220002 // owns the assets; never calls anything
)

// mgSoftDelete marks an asset deleted the way the row predicate reads
// it, so the ROW conjunct has something to refuse.
func mgSoftDelete(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = now() WHERE id = $1`, assetID); err != nil {
		t.Fatalf("soft-delete asset: %v", err)
	}
}

// mgMemberCount asks the DATABASE, not the handler: "the write did not
// happen" must not be answered by the same code path that might have
// let it happen.
func mgMemberCount(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM post_assets WHERE asset_id = $1`, assetID).Scan(&n); err != nil {
		t.Fatalf("count post_assets: %v", err)
	}
	return n
}

// mgPostCount counts posts by this author. CreatePost inserts the post
// BEFORE its members, so a gate placed after the insert would leave an
// orphan post behind on every refusal.
func mgPostCount(t *testing.T, pool *pgxpool.Pool, author int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM posts WHERE author_user_ref = $1`, author).Scan(&n); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	return n
}

// mgCreate drives the real POST /posts with one member.
func mgCreate(t *testing.T, h *Handler, author int64, assetID uuid.UUID) openapi.CreatePostResponseObject {
	t.Helper()
	title := "mg post"
	resp, err := h.CreatePost(ctxAs(author), openapi.CreatePostRequestObject{
		Body: &openapi.PostCreate{
			Title:   &title,
			Members: []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(assetID)}},
		},
	})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	return resp
}

// mgRefusalTemplate strips the echoed UUID out of a refusal body so two
// refusals naming different assets can be compared for sameness. The
// UUID is the caller's own input; everything else is what the endpoint
// discloses.
func mgRefusalTemplate(t *testing.T, resp openapi.CreatePostResponseObject, assetID uuid.UUID) string {
	t.Helper()
	nf, is := resp.(openapi.CreatePost404JSONResponse)
	if !is {
		t.Fatalf("response is %T, want CreatePost404JSONResponse", resp)
	}
	return strings.ReplaceAll(nf.Error, assetID.String(), "<uuid>")
}

// TestCreatePost_MemberGate is #922 on the create path.
func TestCreatePost_MemberGate(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	// Assets owned by SOMEONE ELSE. All active + ready, so the only
	// thing that varies between them is the plane under test.
	strangerPublic := seedPreviewAssetOwned(t, pool, "public", false, mgStranger)
	strangerRestricted := seedPreviewAssetOwned(t, pool, "restricted", false, mgStranger)
	strangerTeam := seedPreviewAssetOwned(t, pool, "team", false, mgStranger)
	strangerDeleted := seedPreviewAssetOwned(t, pool, "public", false, mgStranger)
	mgSoftDelete(t, pool, strangerDeleted)

	// The caller's OWN restricted asset — the owner short-circuit.
	ownRestricted := seedPreviewAssetOwned(t, pool, "restricted", false, mgAuthor)

	missing := uuid.New()

	cases := []struct {
		name    string
		assetID uuid.UUID
		wantOK  bool
		why     string
	}{
		{
			name:    "restricted asset owned by a stranger",
			assetID: strangerRestricted,
			wantOK:  false,
			why: "THE discriminating case. Row-visible to the caller — EntityAsset's " +
				"authenticated predicate is deleted_at IS NULL and nothing more (ADR 0064) " +
				"— and NOT content-readable. A gate built on visibility.CanSee alone admits " +
				"it while gating nothing. Before #922 this returned 201",
		},
		{
			name:    "team asset owned by a stranger, caller not in the team",
			assetID: strangerTeam,
			wantOK:  false,
			why:     "the team tier resolves through membership; a non-member is refused",
		},
		{
			name:    "soft-deleted public asset owned by a stranger",
			assetID: strangerDeleted,
			wantOK:  false,
			why: "the ROW conjunct on its own account: ContentReadable never reads " +
				"deleted_at, so a content-plane-only gate would attach a deleted asset and " +
				"create a member row the read query then drops in SQL",
		},
		{
			name:    "nonexistent uuid",
			assetID: missing,
			wantOK:  false,
			why:     "pre-existing behaviour; the body is compared against the restricted case below",
		},
		{
			name:    "public asset owned by a stranger",
			assetID: strangerPublic,
			wantOK:  true,
			why: "the case that proves this is a READ gate and not an ownership gate. " +
				"Posting someone else's PUBLIC work still works — a deny-everything gate " +
				"passes every case above and fails only this one",
		},
		{
			name:    "caller's own restricted asset",
			assetID: ownRestricted,
			wantOK:  true,
			why:     "the owner reaches their own asset at any tier; existing behaviour unchanged",
		},
	}

	refusals := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			postsBefore := mgPostCount(t, pool, mgAuthor)
			resp := mgCreate(t, h, mgAuthor, tc.assetID)

			if tc.wantOK {
				created, is := resp.(openapi.CreatePost201JSONResponse)
				if !is {
					t.Fatalf("response is %T, want 201\nwhy this case exists: %s", resp, tc.why)
				}
				newID := uuid.UUID(created.Id)
				t.Cleanup(func() {
					c := context.Background()
					_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, newID)
					_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, newID)
				})
				if n := mgMemberCount(t, pool, tc.assetID); n != 1 {
					t.Errorf("post_assets rows for this asset = %d, want 1", n)
				}
				return
			}

			refusals[tc.name] = mgRefusalTemplate(t, resp, tc.assetID)

			if n := mgMemberCount(t, pool, tc.assetID); n != 0 {
				t.Errorf("post_assets rows for this asset = %d, want 0 — a 404 that still "+
					"wrote the membership is not a refusal\nwhy this case exists: %s", n, tc.why)
			}
			if n := mgPostCount(t, pool, mgAuthor); n != postsBefore {
				t.Errorf("posts by the author = %d, want %d — the gate must run before the "+
					"transaction opens, or a refusal leaves an orphan post behind", n, postsBefore)
			}
		})
	}

	// The enumeration assertion. A refused member and a nonexistent UUID
	// must be indistinguishable once the caller's own echoed input is
	// removed. Any difference makes POST /posts a UUID-existence probe.
	restricted := refusals["restricted asset owned by a stranger"]
	nonexistent := refusals["nonexistent uuid"]
	if restricted == "" || nonexistent == "" {
		t.Fatalf("both refusal bodies must have been captured; got %q and %q", restricted, nonexistent)
	}
	if restricted != nonexistent {
		t.Errorf("an unreadable asset and a nonexistent one must produce the SAME body:\n"+
			"  unreadable:  %q\n  nonexistent: %q\n"+
			"any difference is an enumeration oracle — the caller learns the UUID is real",
			restricted, nonexistent)
	}
}

// TestAddPostAsset_MemberGate is the same gate on the UPDATE path.
//
// This test is the reason #922 is not "one gate in CreatePost". Members
// can be added after creation through POST /posts/{id}/assets, which
// gated the POST (canMutatePost) and never the ASSET — so a gate on
// create alone would not have been a gate at all. The caller here is the
// post's own author, i.e. someone canMutatePost already admits.
func TestAddPostAsset_MemberGate(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	postID := seedTierPost(t, pool, mgAuthor, "public")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM post_assets WHERE post_id = $1`, postID)
	})

	strangerRestricted := seedPreviewAssetOwned(t, pool, "restricted", false, mgStranger)
	strangerPublic := seedPreviewAssetOwned(t, pool, "public", false, mgStranger)
	missing := uuid.New()

	add := func(assetID uuid.UUID) openapi.AddPostAssetResponseObject {
		t.Helper()
		resp, err := h.AddPostAsset(ctxAs(mgAuthor), openapi.AddPostAssetRequestObject{
			Id:   openapi_types.UUID(postID),
			Body: &openapi.PostAssetWrite{AssetId: openapi_types.UUID(assetID)},
		})
		if err != nil {
			t.Fatalf("AddPostAsset: %v", err)
		}
		return resp
	}

	// The discriminating case: the author may mutate this post, and must
	// still not be able to reach a stranger's restricted asset.
	resp := add(strangerRestricted)
	refused, is := resp.(openapi.AddPostAsset404JSONResponse)
	if !is {
		t.Fatalf("attaching a stranger's restricted asset returned %T, want 404 — the post "+
			"author passes canMutatePost, so only an ASSET gate can refuse this", resp)
	}
	if n := mgMemberCount(t, pool, strangerRestricted); n != 0 {
		t.Errorf("post_assets rows = %d, want 0 — a 404 that still wrote the membership "+
			"is not a refusal", n)
	}

	// Indistinguishable from a nonexistent UUID.
	missingResp, is := add(missing).(openapi.AddPostAsset404JSONResponse)
	if !is {
		t.Fatalf("nonexistent uuid: want 404")
	}
	if refused.Error != missingResp.Error {
		t.Errorf("an unreadable asset and a nonexistent one must be BYTE-IDENTICAL:\n"+
			"  unreadable:  %q\n  nonexistent: %q", refused.Error, missingResp.Error)
	}

	// The feature half: a stranger's PUBLIC asset still attaches.
	if got := add(strangerPublic); !isAddPostAsset204(got) {
		t.Fatalf("attaching a stranger's PUBLIC asset returned %T, want 204 — the gate must "+
			"be a read gate, not an ownership gate", got)
	}
	if n := mgMemberCount(t, pool, strangerPublic); n != 1 {
		t.Errorf("post_assets rows = %d, want 1", n)
	}
}

func isAddPostAsset204(resp openapi.AddPostAssetResponseObject) bool {
	_, is := resp.(openapi.AddPostAsset204Response)
	return is
}
