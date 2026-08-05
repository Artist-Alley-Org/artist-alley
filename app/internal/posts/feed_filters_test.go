// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #891 — the "hide restricted members" browse preference.
//
// Two layers, on purpose:
//
//   - TestApplyHideRestricted_Rules is a pure unit table over the
//     decision itself. It needs no database and it is where the
//     off-by-one lives: "a post with NO members" and "a post whose
//     members are all restricted" are different states and must stay
//     different.
//   - TestListPosts_HideRestricted_* drive the real GET /posts handler
//     and assert on the SERIALIZED response, because that is the
//     contract — a grid that happens to render the right thing off a
//     wrong payload is not the same fact. They skip without
//     AA_DB_PASSWORD, like every other integration test here.
//
// The load-bearing case is TestListPosts_HideRestricted_NeverHidesOwn:
// a post can carry other people's restricted assets, so the "all
// members restricted → drop the post" rule would otherwise delete an
// author's own work from their own feed over a display preference.

package posts

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

const (
	ffAuthor int64 = 4910001 // owns the assets + most of the posts
	ffCaller int64 = 4910002 // the reader whose preference is under test
)

// ffStub is the feedFilterReader seam under test control.
type ffStub struct {
	hide bool
	err  error
}

func (s ffStub) HideRestrictedFeedMembers(context.Context, int64) (bool, error) {
	return s.hide, s.err
}

// ---------------------------------------------------------------------------
// Unit: the decision
// ---------------------------------------------------------------------------

func member(restricted bool) openapi.PostMember {
	return openapi.PostMember{AssetId: openapi_types.UUID(uuid.New()), Restricted: restricted}
}

func post(author int64, members ...openapi.PostMember) openapi.Post {
	return openapi.Post{
		Id:            openapi_types.UUID(uuid.New()),
		AuthorUserRef: author,
		Members:       members,
	}
}

func TestApplyHideRestricted_Rules(t *testing.T) {
	cases := []struct {
		name          string
		in            openapi.Post
		wantKept      bool
		wantMemberN   int
		wantRationale string
	}{
		{
			name:          "some members restricted",
			in:            post(ffAuthor, member(false), member(true), member(false)),
			wantKept:      true,
			wantMemberN:   2,
			wantRationale: "the post still shows something; only the placeholders go",
		},
		{
			name:          "every member restricted",
			in:            post(ffAuthor, member(true), member(true)),
			wantKept:      false,
			wantRationale: "nothing left to render — an empty card is worse than a placeholder",
		},
		{
			name:          "no members at all",
			in:            post(ffAuthor),
			wantKept:      true,
			wantMemberN:   0,
			wantRationale: "an article (ADR 0073) was never showing the caller anything withheld",
		},
		{
			name:          "every member restricted but the caller wrote it",
			in:            post(ffCaller, member(true), member(true)),
			wantKept:      true,
			wantMemberN:   0,
			wantRationale: "your own work does not vanish from your own feed over a display preference",
		},
		{
			name:          "no members and the caller wrote it",
			in:            post(ffCaller),
			wantKept:      true,
			wantMemberN:   0,
			wantRationale: "same as any other memberless post",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyHideRestricted([]openapi.Post{tc.in}, ffCaller)
			if tc.wantKept && len(got) != 1 {
				t.Fatalf("post was dropped, want kept — %s", tc.wantRationale)
			}
			if !tc.wantKept {
				if len(got) != 0 {
					t.Fatalf("post was kept, want dropped — %s", tc.wantRationale)
				}
				return
			}
			if n := len(got[0].Members); n != tc.wantMemberN {
				t.Fatalf("members = %d, want %d", n, tc.wantMemberN)
			}
			for _, m := range got[0].Members {
				if m.Restricted {
					t.Fatal("a restricted member survived the filter")
				}
			}
		})
	}
}

// The filter must not disturb the ORDER of a page, or the cursor the
// caller resumes from stops matching what they were shown.
func TestApplyHideRestricted_PreservesOrder(t *testing.T) {
	a := post(ffAuthor, member(false))
	drop := post(ffAuthor, member(true))
	b := post(ffAuthor, member(false))
	got := applyHideRestricted([]openapi.Post{a, drop, b}, ffCaller)
	if len(got) != 2 || got[0].Id != a.Id || got[1].Id != b.Id {
		t.Fatalf("order changed: got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Integration: the serialized feed
// ---------------------------------------------------------------------------

// ffSeedAsset plants an asset owned by ffAuthor at the given
// sensitivity. `restricted` is unreadable to anyone but its owner.
func ffSeedAsset(t *testing.T, pool *pgxpool.Pool, sensitivity string) uuid.UUID {
	t.Helper()
	return seedPreviewAssetOwned(t, pool, sensitivity, true, ffAuthor)
}

// ffSeedPost plants an org-only post — the tier ListPosts defaults to
// when no `?visibility=` is sent, which is what every frontend surface
// does.
func ffSeedPost(t *testing.T, pool *pgxpool.Pool, author int64, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,$3,'org-only')`,
		postID, author, "ff post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`, postID, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, postID)
	})
	return postID
}

// ffFeed calls the real ListPosts and returns the page, decoded back out
// of its JSON so the assertions run against the bytes a client gets and
// not against an in-memory struct the marshaller might not agree with.
func ffFeed(t *testing.T, h *Handler, callerRef int64) (openapi.PostList, []byte) {
	t.Helper()
	limit := 200
	resp, err := h.ListPosts(ctxAs(callerRef), openapi.ListPostsRequestObject{
		Params: openapi.ListPostsParams{Limit: &limit},
	})
	if err != nil {
		t.Fatalf("list posts: %v", err)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var page openapi.PostList
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return page, body
}

func ffFind(page openapi.PostList, id uuid.UUID) *openapi.Post {
	for i := range page.Items {
		if uuid.UUID(page.Items[i].Id) == id {
			return &page.Items[i]
		}
	}
	return nil
}

// ffFixture plants the four discriminating posts and returns their ids.
type ffFixture struct {
	mixed          uuid.UUID // one readable member, one restricted
	allRestricted  uuid.UUID // every member restricted, someone else's post
	noMembers      uuid.UUID // an article — never had members
	mineRestricted uuid.UUID // every member restricted, the CALLER's post
	readableAsset  uuid.UUID
}

func ffPlant(t *testing.T, pool *pgxpool.Pool) ffFixture {
	t.Helper()
	pub := ffSeedAsset(t, pool, "public")
	restA := ffSeedAsset(t, pool, "restricted")
	restB := ffSeedAsset(t, pool, "restricted")
	return ffFixture{
		mixed:          ffSeedPost(t, pool, ffAuthor, pub, restA),
		allRestricted:  ffSeedPost(t, pool, ffAuthor, restA, restB),
		noMembers:      ffSeedPost(t, pool, ffAuthor),
		mineRestricted: ffSeedPost(t, pool, ffCaller, restA, restB),
		readableAsset:  pub,
	}
}

// Preference OFF must be byte-for-byte the pre-#891 feed. This is the
// regression pin: everything else in this file describes a change, and
// this one describes the change NOT happening.
func TestListPosts_HideRestricted_OffIsUnchanged(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	unwired := peHandler(pool) // no seam at all — the pre-#891 handler
	_, before := ffFeed(t, unwired, ffCaller)

	off := peHandler(pool)
	off.SetFeedFilters(ffStub{hide: false})
	_, after := ffFeed(t, off, ffCaller)

	if string(before) != string(after) {
		t.Fatal("preference off changed the feed payload; it must be identical to the unwired handler")
	}

	page, _ := ffFeed(t, off, ffCaller)
	for _, id := range []uuid.UUID{fx.mixed, fx.allRestricted, fx.noMembers, fx.mineRestricted} {
		if ffFind(page, id) == nil {
			t.Errorf("post %v missing from the unfiltered feed", id)
		}
	}
	// And the placeholders are still there to be hidden later.
	mixed := ffFind(page, fx.mixed)
	if n := len(mixed.Members); n != 2 {
		t.Fatalf("unfiltered mixed post has %d members, want 2 (one of them a placeholder)", n)
	}
}

// Preference ON: the three subtractions and the two things it must not
// touch, all read off one page.
func TestListPosts_HideRestricted_On(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	h := peHandler(pool)
	h.SetFeedFilters(ffStub{hide: true})
	page, _ := ffFeed(t, h, ffCaller)

	// 1. A post with SOME restricted members stays; those members go.
	mixed := ffFind(page, fx.mixed)
	if mixed == nil {
		t.Fatal("a post with a readable member must survive the filter")
	}
	if len(mixed.Members) != 1 {
		t.Fatalf("mixed post kept %d members, want 1", len(mixed.Members))
	}
	if uuid.UUID(mixed.Members[0].AssetId) != fx.readableAsset {
		t.Error("the surviving member is not the readable one")
	}
	for _, m := range mixed.Members {
		if m.Restricted {
			t.Error("a restricted placeholder survived in the serialized feed")
		}
	}

	// 2. A post whose members are ALL restricted drops out.
	if ffFind(page, fx.allRestricted) != nil {
		t.Error("a post with no visible members must not occupy a slot in the page")
	}

	// 3. A post with no members at all is untouched — "had members, none
	//    visible" is not the same state as "had none".
	if ffFind(page, fx.noMembers) == nil {
		t.Error("a memberless post (an article) was hidden; it withholds nothing")
	}

	// 4. THE BOUNDARY: the caller's OWN post survives with zero visible
	//    members. It carries somebody else's restricted assets, so rule 2
	//    alone would delete it from its author's feed.
	mine := ffFind(page, fx.mineRestricted)
	if mine == nil {
		t.Fatal("the caller's OWN post was hidden from their own feed")
	}
	if len(mine.Members) != 0 {
		t.Errorf("own post kept %d members, want 0 — the filter applies to members regardless of authorship", len(mine.Members))
	}
}

// A post asked for BY NAME is untouched by the preference — it keeps
// its placeholders, and therefore keeps #913's "Request access" button.
//
// This is the deliberate boundary, not an omission. The post-level rule
// exists BECAUSE an all-restricted post rendering as an empty card is
// worse than the placeholder it replaced; filtering members here would
// rebuild that empty card on the one surface that avoided it.
func TestGetPost_HideRestricted_IsUntouched(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	h := peHandler(pool)
	h.SetFeedFilters(ffStub{hide: true})

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want int
	}{
		{"all members restricted", fx.allRestricted, 2},
		{"some members restricted", fx.mixed, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.GetPost(ctxAs(ffCaller), openapi.GetPostRequestObject{Id: openapi_types.UUID(tc.id)})
			if err != nil {
				t.Fatalf("get post: %v", err)
			}
			ok, isOK := resp.(openapi.GetPost200JSONResponse)
			if !isOK {
				t.Fatalf("post was refused: %T — a display preference must never gate a read", resp)
			}
			p := openapi.Post(ok)
			if len(p.Members) != tc.want {
				t.Fatalf("members = %d, want %d — GET by id is outside the filter", len(p.Members), tc.want)
			}
			if !hasRestrictedMember(p) {
				t.Error("the placeholder is gone, and #913's Request access button with it")
			}
		})
	}
}

func hasRestrictedMember(p openapi.Post) bool {
	for _, m := range p.Members {
		if m.Restricted {
			return true
		}
	}
	return false
}

// The seam failing must not silently shorten anyone's feed. A display
// preference degrades toward showing everything, which can leak nothing:
// the redaction that protects the content already ran in enrichPreview.
func TestListPosts_HideRestricted_ErrorShowsEverything(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	h := peHandler(pool)
	h.SetFeedFilters(ffStub{hide: true, err: context.DeadlineExceeded})
	page, _ := ffFeed(t, h, ffCaller)

	if ffFind(page, fx.allRestricted) == nil {
		t.Error("a failed preference lookup hid a post; it must fall back to unfiltered")
	}
}
