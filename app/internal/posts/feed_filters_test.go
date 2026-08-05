// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #891 / #921 — restricted placeholders are subtracted from the browse
// feed BY DEFAULT, and `show_restricted` puts them back.
//
// #891 shipped the machinery as an opt-in (`hide_restricted`, default
// off). #921 measured that default wrong — 27 of one seeded account's 82
// posts were entirely placeholders — and inverted it. The THREE RULES
// below did not change; what changed is which branch is the common one,
// and what the nil/error seams degrade TO.
//
// Two layers, on purpose:
//
//   - TestApplyHideRestricted_Rules is a pure unit table over the
//     decision itself. It needs no database and it is where the
//     off-by-one lives: "a post with NO members" and "a post whose
//     members are all restricted" are different states and must stay
//     different.
//   - TestListPosts_ShowRestricted_* drive the real GET /posts handler
//     and assert on the SERIALIZED response, because that is the
//     contract — a grid that happens to render the right thing off a
//     wrong payload is not the same fact. They skip without
//     AA_DB_PASSWORD, like every other integration test here.
//
// The load-bearing case is the caller's OWN all-restricted post: a post
// can carry other people's restricted assets, so the "all members
// restricted → drop the post" rule would otherwise delete an author's
// own work from their own feed. That case now fires for EVERY author by
// default rather than for the handful who opted in, which is what makes
// it worth two assertions rather than one.

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
	show bool
	err  error
}

func (s ffStub) ShowRestrictedFeedMembers(context.Context, int64) (bool, error) {
	return s.show, s.err
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

// ffAssertDefaultFeed reads the five discriminating facts about a page
// served under the #921 DEFAULT. Shared by the stored-preference case
// and the nil-seam case, because "the seam is missing" and "the seam
// said false" must be the same feed and asserting that twice by hand is
// how the two drift.
func ffAssertDefaultFeed(t *testing.T, page openapi.PostList, fx ffFixture) {
	t.Helper()

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
	//    alone would delete it from its author's feed — and since #921
	//    that would happen to every author by default, not to the few who
	//    opted in.
	mine := ffFind(page, fx.mineRestricted)
	if mine == nil {
		t.Fatal("the caller's OWN post was hidden from their own feed")
	}
	if len(mine.Members) != 0 {
		t.Errorf("own post kept %d members, want 0 — the filter applies to members regardless of authorship", len(mine.Members))
	}
}

// THE #921 HEADLINE: a caller who has never touched preferences gets no
// placeholders. Asserted on the serialized page, because "the feed hides
// them by default" is a statement about bytes on the wire.
func TestListPosts_ShowRestricted_DefaultHides(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	h := peHandler(pool)
	h.SetFeedFilters(ffStub{show: false}) // what a row-less account reads as
	page, _ := ffFeed(t, h, ffCaller)

	ffAssertDefaultFeed(t, page, fx)
}

// The nil seam — a test that never wired it, or a boot-order slip —
// must produce the DEFAULT feed and not a third behaviour. Byte-for-byte
// against the stored default, because the two are meant to be
// indistinguishable and a field-level compare would let a new key drift
// between them unnoticed.
//
// This inverted at #921 and the literal did not move: `false` used to
// mean "show everything", it now means "hide the placeholders", and both
// seams answer `false` because both fail to whatever the build's default
// is. See posts.showRestricted.
func TestListPosts_ShowRestricted_NilSeamIsTheDefault(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	unwired := peHandler(pool) // no seam at all
	page, nilSeam := ffFeed(t, unwired, ffCaller)

	stored := peHandler(pool)
	stored.SetFeedFilters(ffStub{show: false})
	_, defaulted := ffFeed(t, stored, ffCaller)

	if string(nilSeam) != string(defaulted) {
		t.Fatal("an unwired seam served a different feed from the stored default; the two must be identical")
	}
	ffAssertDefaultFeed(t, page, fx)
}

// Preference ON restores the pre-#921 feed exactly: every post back,
// every placeholder back, and #913's Request access with them. This is
// the regression pin in the other direction — the opt-in has to be worth
// opting into.
func TestListPosts_ShowRestricted_OnIsTheUnfilteredFeed(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	h := peHandler(pool)
	h.SetFeedFilters(ffStub{show: true})
	page, _ := ffFeed(t, h, ffCaller)

	for _, id := range []uuid.UUID{fx.mixed, fx.allRestricted, fx.noMembers, fx.mineRestricted} {
		if ffFind(page, id) == nil {
			t.Errorf("post %v missing from the unfiltered feed", id)
		}
	}

	// The placeholders themselves, not just the rows that carry them.
	mixed := ffFind(page, fx.mixed)
	if mixed == nil {
		t.Fatal("the mixed post is missing from the unfiltered feed")
	}
	if n := len(mixed.Members); n != 2 {
		t.Fatalf("unfiltered mixed post has %d members, want 2 (one of them a placeholder)", n)
	}
	if !hasRestrictedMember(*mixed) {
		t.Error("the mixed post came back with no placeholder; turning the setting on restored nothing")
	}
	allRest := ffFind(page, fx.allRestricted)
	if allRest == nil {
		t.Fatal("the all-restricted post is missing from the unfiltered feed")
	}
	if n := len(allRest.Members); n != 2 || !hasRestrictedMember(*allRest) {
		t.Errorf("all-restricted post has %d members and restricted=%v, want 2 placeholders",
			n, hasRestrictedMember(*allRest))
	}
}

// THE BOUNDARY GUARD. A post asked for BY NAME is untouched — it keeps
// its placeholders, and therefore keeps #913's "Request access" button.
//
// This is a deliberate boundary, not an omission. The post-level rule
// exists BECAUSE an all-restricted post rendering as an empty card is
// worse than the placeholder it replaced; filtering members here would
// rebuild that empty card on the one surface that avoided it. Extending
// the filter to GetPost was tried during #891 and reverted.
//
// #921 raises the stakes rather than changing the assertion: the posts
// covered here are now the ones the feed drops from EVERY default
// caller's page, so the detail path is where they can still be reached
// at all. Both settings are exercised — the boundary is about the
// SURFACE, not about what the reader asked for. If anyone ever filters
// the detail path, this must fail.
func TestGetPost_HideRestricted_IsUntouched(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	for _, seam := range []struct {
		name string
		show bool
	}{
		{"default (feed hides these)", false},
		{"opted into placeholders", true},
	} {
		t.Run(seam.name, func(t *testing.T) {
			h := peHandler(pool)
			h.SetFeedFilters(ffStub{show: seam.show})

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

// A failed preference lookup serves THE DEFAULT FEED — not the reader's
// stored setting, and not "everything".
//
// This inverted at #921 and it is the subtle half of the sprint. Under
// #891 the seam degraded toward showing everything, on the reasoning
// that a display preference can leak nothing (still true: enrichPreview
// already did the redaction, and a restricted member carries no `asset`
// at all). But "show everything" is now the rejected experience, so a
// prefs blip would have repainted every affected reader's feed as the
// wall of locked doors #921 exists to remove — a loud, instance-wide
// surprise triggered by a component unrelated to what they are looking
// at. Degrading to the default shortens the feed of the minority who
// opted in, and shortens it to exactly what everyone else sees.
//
// The stub says `show: true` on purpose: the error must WIN over the
// stored value, or this test would pass on a seam that simply ignored
// the error.
func TestListPosts_ShowRestricted_ErrorServesTheDefault(t *testing.T) {
	pool := previewPool(t)
	fx := ffPlant(t, pool)

	h := peHandler(pool)
	h.SetFeedFilters(ffStub{show: true, err: context.DeadlineExceeded})
	page, _ := ffFeed(t, h, ffCaller)

	if ffFind(page, fx.allRestricted) != nil {
		t.Error("a failed preference lookup served the stored value; it must fall back to the default feed")
	}
	ffAssertDefaultFeed(t, page, fx)
}
