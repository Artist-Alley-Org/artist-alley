// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #941 — the post COVER gate.
//
// # What was wrong
//
// #922 gated post MEMBERS on visibility.CanAttachAsset. It did not gate
// the cover. An explicitly-supplied `cover_asset_id` /
// `cover_thumbnail_asset_id` was copied straight into the INSERT, and
// PATCH /posts/{id} did the same for `cover_asset_id` — so the asset
// the members loop had just refused walked in through the cover field
// on the same request.
//
// The implicit cover was safe by accident: with `cover_asset_id`
// omitted the handler copies members[0], which IS gated.
//
// # The severity, stated honestly
//
// No ASSOCIATION renders. The API hands out only a UUID for the cover,
// and both resolution paths are content-gated server-side, so a viewer
// who cannot read the asset never sees it. This is not #922 wearing a
// different hat.
//
// What it did produce, and what these tests pin:
//
//   - a 500 where a 404 belongs. A cover naming an absent asset trips
//     posts_cover_asset_id_fkey, SQLSTATE 23503, which neither handler
//     matched — it fell through to the wrapped-error branch and the
//     caller got a server fault for a bad input.
//   - a weak existence oracle: "500" and "201" separate a real UUID
//     from a fake one.
//   - a silent lie in the data — posts.cover_asset_id pointing at an
//     asset the author was never entitled to read, waiting for the
//     first surface that trusts the column.
//
// # What makes this file worth reading
//
// TWO call sites. A create-only gate is not a gate; that is exactly
// what #922 turned out to be when POST /posts/{id}/assets was found to
// need it as badly as CreatePost did. So the update path is tested on
// its own account, with a caller canMutatePost already admits — only an
// ASSET gate can refuse that request.
//
// The negative controls carry the weight, same as in member_gate_test:
// a stranger's PUBLIC asset must still be a valid cover (read gate, not
// owner gate) and the implicit cover must still default to members[0].
// A deny-everything cover gate passes every refusal case here.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	cgAuthor   int64 = 9410001 // does the posting
	cgStranger int64 = 9410002 // owns the assets; never calls anything
)

// cgCreate drives the real POST /posts with one readable member and
// whatever covers the case under test wants. The member is always
// readable so that a refusal can only have come from a COVER — if the
// member were the unreadable one, #922's gate would refuse first and
// this file would be testing nothing.
func cgCreate(
	t *testing.T,
	h *Handler,
	member uuid.UUID,
	cover, thumb *uuid.UUID,
) openapi.CreatePostResponseObject {
	t.Helper()
	title := "cg post"
	body := &openapi.PostCreate{
		Title:   &title,
		Members: []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(member)}},
	}
	if cover != nil {
		c := openapi_types.UUID(*cover)
		body.CoverAssetId = &c
	}
	if thumb != nil {
		c := openapi_types.UUID(*thumb)
		body.CoverThumbnailAssetId = &c
	}
	resp, err := h.CreatePost(ctxAs(cgAuthor), openapi.CreatePostRequestObject{Body: body})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	return resp
}

// cgPostCount asks the DATABASE how many posts this author has. The
// cover is written by the INSERT itself, so a gate placed after it
// leaves a fully-formed post behind on every refusal and a status-only
// assertion would never notice.
func cgPostCount(t *testing.T, pool *pgxpool.Pool, author int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM posts WHERE author_user_ref = $1`, author).Scan(&n); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	return n
}

// cgCoverOf reads posts.cover_asset_id straight out of the table. The
// column is the thing #941 is about — "the API doesn't render it" is
// not a reason to let a lie sit in it.
func cgCoverOf(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) uuid.UUID {
	t.Helper()
	var got *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT cover_asset_id FROM posts WHERE id = $1`, postID).Scan(&got); err != nil {
		t.Fatalf("read cover_asset_id: %v", err)
	}
	if got == nil {
		return uuid.Nil
	}
	return *got
}

func cgCleanupPost(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
	})
}

// TestCreatePost_CoverGate is #941 on the create path.
func TestCreatePost_CoverGate(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	// The member every case uses. Readable, so it never explains a
	// refusal.
	ownMember := seedPreviewAssetOwned(t, pool, "public", false, cgAuthor)

	strangerRestricted := seedPreviewAssetOwned(t, pool, "restricted", false, cgStranger)
	strangerTeam := seedPreviewAssetOwned(t, pool, "team", false, cgStranger)
	strangerPublic := seedPreviewAssetOwned(t, pool, "public", false, cgStranger)
	strangerDeleted := seedPreviewAssetOwned(t, pool, "public", false, cgStranger)
	mgSoftDelete(t, pool, strangerDeleted)
	ownRestricted := seedPreviewAssetOwned(t, pool, "restricted", false, cgAuthor)
	missing := uuid.New()

	cases := []struct {
		name   string
		cover  uuid.UUID
		thumb  bool // put it on cover_thumbnail_asset_id instead
		wantOK bool
		why    string
	}{
		{
			name:   "cover: stranger's restricted asset",
			cover:  strangerRestricted,
			wantOK: false,
			why: "THE discriminating case. Row-visible to the caller (EntityAsset's " +
				"authenticated predicate is deleted_at IS NULL and nothing more, ADR 0064) " +
				"and NOT content-readable. Before #941 this returned 201 and wrote the " +
				"stranger's UUID into posts.cover_asset_id",
		},
		{
			name:   "cover: stranger's team asset, caller not in the team",
			cover:  strangerTeam,
			wantOK: false,
			why:    "the team tier resolves through membership; a non-member is refused",
		},
		{
			name:   "cover: soft-deleted public asset",
			cover:  strangerDeleted,
			wantOK: false,
			why: "the ROW conjunct on its own account — ContentReadable never reads " +
				"deleted_at, so a content-plane-only gate would pin a deleted asset as cover",
		},
		{
			name:   "cover: nonexistent uuid",
			cover:  missing,
			wantOK: false,
			why: "before #941 this was an unhandled SQLSTATE 23503 on " +
				"posts_cover_asset_id_fkey and answered 500. Its body is compared against " +
				"the restricted case below",
		},
		{
			name:   "cover_thumbnail: stranger's restricted asset",
			cover:  strangerRestricted,
			thumb:  true,
			wantOK: false,
			why: "the second cover column. It is NOT a post member and has its own FK, so " +
				"a gate that only covered cover_asset_id would leave this one wide open",
		},
		{
			name:   "cover_thumbnail: nonexistent uuid",
			cover:  missing,
			thumb:  true,
			wantOK: false,
			why:    "same 23503-was-a-500 story on posts_cover_thumbnail_asset_id_fkey",
		},
		{
			name:   "cover: stranger's PUBLIC asset",
			cover:  strangerPublic,
			wantOK: true,
			why: "the case that proves this is a READ gate and not an ownership gate. " +
				"A deny-everything cover gate passes every case above and fails only here",
		},
		{
			name:   "cover: caller's own restricted asset",
			cover:  ownRestricted,
			wantOK: true,
			why:    "the owner reaches their own asset at any tier",
		},
	}

	refusals := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := cgPostCount(t, pool, cgAuthor)

			var cover, thumb *uuid.UUID
			if tc.thumb {
				thumb = &tc.cover
			} else {
				cover = &tc.cover
			}
			resp := cgCreate(t, h, ownMember, cover, thumb)

			if tc.wantOK {
				created, is := resp.(openapi.CreatePost201JSONResponse)
				if !is {
					t.Fatalf("response is %T, want 201\nwhy this case exists: %s", resp, tc.why)
				}
				newID := uuid.UUID(created.Id)
				cgCleanupPost(t, pool, newID)
				if got := cgCoverOf(t, pool, newID); got != tc.cover {
					t.Errorf("posts.cover_asset_id = %v, want %v", got, tc.cover)
				}
				return
			}

			if _, is := resp.(openapi.CreatePost404JSONResponse); !is {
				t.Fatalf("response is %T, want CreatePost404JSONResponse — a 500 for a bad "+
					"cover is the FK violation leaking\nwhy this case exists: %s", resp, tc.why)
			}
			refusals[tc.name] = mgRefusalTemplate(t, resp, tc.cover)

			if n := cgPostCount(t, pool, cgAuthor); n != before {
				t.Errorf("posts by the author = %d, want %d — a 404 that still wrote the "+
					"post is not a refusal, and the row it left carries exactly the cover "+
					"the gate rejected\nwhy this case exists: %s", n, before, tc.why)
			}
		})
	}

	// The enumeration assertions. An unreadable cover and a nonexistent
	// one must be indistinguishable once the caller's own echoed input
	// is removed, on BOTH columns.
	for _, pair := range [][2]string{
		{"cover: stranger's restricted asset", "cover: nonexistent uuid"},
		{"cover_thumbnail: stranger's restricted asset", "cover_thumbnail: nonexistent uuid"},
	} {
		unreadable, nonexistent := refusals[pair[0]], refusals[pair[1]]
		if unreadable == "" || nonexistent == "" {
			t.Fatalf("both refusal bodies must have been captured for %v; got %q and %q",
				pair, unreadable, nonexistent)
		}
		if unreadable != nonexistent {
			t.Errorf("%s vs %s: an unreadable cover and a nonexistent one must produce the "+
				"SAME body:\n  unreadable:  %q\n  nonexistent: %q\n"+
				"any difference is an enumeration oracle", pair[0], pair[1],
				unreadable, nonexistent)
		}
	}
}

// TestCreatePost_ImplicitCoverStillDefaults is the negative control for
// the shape of the fix. The gate list must not accidentally start
// requiring an explicit cover, and the omitted-cover path must still
// resolve to members[0] without a second gate query for an asset that
// was already gated as a member.
func TestCreatePost_ImplicitCoverStillDefaults(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	member := seedPreviewAssetOwned(t, pool, "public", false, cgAuthor)

	resp := cgCreate(t, h, member, nil, nil)
	created, is := resp.(openapi.CreatePost201JSONResponse)
	if !is {
		t.Fatalf("response is %T, want 201 — omitting the cover must still work", resp)
	}
	newID := uuid.UUID(created.Id)
	cgCleanupPost(t, pool, newID)

	if got := cgCoverOf(t, pool, newID); got != member {
		t.Errorf("posts.cover_asset_id = %v, want members[0] = %v", got, member)
	}
}

// TestUpdatePost_CoverGate is the same gate on the UPDATE path.
//
// This test is the reason #941 is not "one gate in CreatePost". The
// caller is the post's own AUTHOR — canMutatePost admits them outright,
// so nothing but an asset gate can refuse these requests. #922 learned
// this lesson once already, on POST /posts/{id}/assets.
func TestUpdatePost_CoverGate(t *testing.T) {
	h := wireWriteHandler(t)
	pool := h.Pool

	strangerRestricted := seedPreviewAssetOwned(t, pool, "restricted", false, cgStranger)
	strangerPublic := seedPreviewAssetOwned(t, pool, "public", false, cgStranger)
	missing := uuid.New()

	patch := func(t *testing.T, postID, cover uuid.UUID) openapi.UpdatePostResponseObject {
		t.Helper()
		c := openapi_types.UUID(cover)
		resp, err := h.UpdatePost(ctxAs(cgAuthor), openapi.UpdatePostRequestObject{
			Id:   openapi_types.UUID(postID),
			Body: &openapi.PostUpdate{CoverAssetId: &c},
		})
		if err != nil {
			t.Fatalf("UpdatePost: %v", err)
		}
		return resp
	}

	// The discriminating case, plus the proof that nothing was written.
	t.Run("stranger's restricted asset is refused and writes nothing", func(t *testing.T) {
		postID := seedTierPost(t, pool, cgAuthor, "public")
		resp := patch(t, postID, strangerRestricted)
		if _, is := resp.(openapi.UpdatePost404JSONResponse); !is {
			t.Fatalf("response is %T, want UpdatePost404JSONResponse — the author passes "+
				"canMutatePost, so only an ASSET gate can refuse this. Before #941 this "+
				"returned 200 and pinned the stranger's asset as the cover", resp)
		}
		if got := cgCoverOf(t, pool, postID); got != uuid.Nil {
			t.Errorf("posts.cover_asset_id = %v, want unset — a 404 that still wrote the "+
				"cover is not a refusal", got)
		}
	})

	// Indistinguishable from a nonexistent UUID, and neither is a 500.
	t.Run("unreadable and nonexistent are indistinguishable", func(t *testing.T) {
		postID := seedTierPost(t, pool, cgAuthor, "public")

		unreadable, is := patch(t, postID, strangerRestricted).(openapi.UpdatePost404JSONResponse)
		if !is {
			t.Fatalf("unreadable cover: want 404")
		}
		nonexistent, is := patch(t, postID, missing).(openapi.UpdatePost404JSONResponse)
		if !is {
			t.Fatalf("nonexistent cover: want 404 — before #941 the FK violation on " +
				"posts_cover_asset_id_fkey fell through as a 500")
		}
		wantUnreadable := "asset not found: " + strangerRestricted.String()
		wantMissing := "asset not found: " + missing.String()
		if unreadable.Error != wantUnreadable || nonexistent.Error != wantMissing {
			t.Fatalf("refusal bodies differ in more than the echoed UUID:\n"+
				"  unreadable:  %q (want %q)\n  nonexistent: %q (want %q)",
				unreadable.Error, wantUnreadable, nonexistent.Error, wantMissing)
		}
	})

	// The feature half: a stranger's PUBLIC asset is still a valid
	// cover. Read gate, not owner gate.
	t.Run("stranger's public asset is still a valid cover", func(t *testing.T) {
		postID := seedTierPost(t, pool, cgAuthor, "public")
		resp := patch(t, postID, strangerPublic)
		if _, is := resp.(openapi.UpdatePost200JSONResponse); !is {
			t.Fatalf("response is %T, want 200 — pinning someone else's PUBLIC work as a "+
				"cover must keep working, or the gate is an ownership gate", resp)
		}
		if got := cgCoverOf(t, pool, postID); got != strangerPublic {
			t.Errorf("posts.cover_asset_id = %v, want %v", got, strangerPublic)
		}
	})
}
