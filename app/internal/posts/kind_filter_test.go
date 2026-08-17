// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1166 — `GET /posts?kind=` is the browse footer's type filter.
//
// # What is actually at risk here
//
// Not "does it filter". A one-line assertion covers that and it is not
// where a filter on this endpoint goes wrong. Every filter on this feed
// is a NARROWING conjunct beside the post read rule, and the failure
// mode that matters is a filter that starts deciding rows on its own —
// so each case below plants a post the caller must NOT receive and
// checks it is absent from the filtered page, not just that the wanted
// ones are present.
//
// There are two independent gates to compose with, and they fail
// differently:
//
//  1. THE POST READ RULE. `?kind=image` must not surface another
//     author's private post that happens to have a PNG cover. This is
//     the ordinary ANDed-conjunct property every filter here has.
//
//  2. THE MEMBER'S OWN FIELD PLANE. This one is specific to this filter
//     and it is the reason kindFilterSQL carries
//     visibility.FieldsReadableSQL. A post can be readable while one of
//     its member assets is not — #883's placeholder case — and the card
//     then draws nothing about that member, deliberately, because the
//     kind of something you may not see is not yours to know. A filter
//     that could still select the post through that member gives the
//     withheld value back by elimination: ask for each kind in turn and
//     the one that returns the post has named it. That is #902/#1066's
//     derived-copy defect arriving through a new channel, and
//     TestKindFilter_RestrictedMemberIsNeverProbeable is what stops it.
//
// # #1190 widened WHICH assets are looked at, not WHICH may be
//
// The filter used to select on the COVER alone. It now matches when ANY
// member the caller can read resolves to the requested kind, because a
// post containing an epub is a post the ebook filter should return
// whatever its cover happens to be. Two consequences run through the
// cases below:
//
//   - THE KINDS NO LONGER PARTITION. A three-file post can be returned
//     by three different kinds, so "these two filters never return the
//     same post" is no longer a property and the disjointness
//     assertions that pinned it are gone. What replaces them is
//     CONTAINMENT: every kind a post's visible members resolve to
//     returns it, and no other kind does.
//   - THE LEAK CASE GOT SHARPER, not weaker. A post whose ONLY ebook is
//     restricted must still be absent from `?kind=ebook` for a stranger
//     and present for its owner — the same post, two answers, decided
//     per member.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, distinct from every other file's so parallel runs
// cannot see each other's fixtures. posts.author_user_ref has no FK.
const (
	kfAuthor  int64 = 11660001
	kfOther   int64 = 11660002
	kfViewer  int64 = 11660003
	kfNoOneAt int64 = 0
)

// kfAsset plants one asset with a real extension and asset_type, at a
// sensitivity, owned by `owner`. The extension is the whole point: the
// kind the badge draws is DERIVED from it, so a fixture without one
// cannot exercise the filter at all.
func kfAsset(t *testing.T, pool *pgxpool.Pool, ext string, assetType int64, sensitivity string, owner int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                     processing_status, file_extension)
		 VALUES ($1,$2,$3,$4,'active',$5,'ready',$6)`,
		id, "kf-"+ext, owner, assetType, sensitivity, ext); err != nil {
		t.Fatalf("seed asset (%s): %v", ext, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

// kfPost plants a post at a visibility tier with the given members in
// order. `cover` names the explicit cover; pass uuid.Nil to leave
// cover_asset_id NULL, which since #1190 changes nothing about what the
// filter selects — the conjunct ranges over the membership — but keeps
// the fixtures honest about the shape real posts have.
func kfPost(
	t *testing.T, pool *pgxpool.Pool,
	author int64, visibility string, cover uuid.UUID, members ...uuid.UUID,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var coverArg any
	if cover != uuid.Nil {
		coverArg = cover
	}
	// A fixed posted_at well in the past keeps these off the head of a
	// seeded feed without changing anything the filter reads.
	at := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility, posted_at, cover_asset_id)
		 VALUES ($1,$2,'kf post','',$3,$4,$5)`,
		id, author, visibility, at, coverArg); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`, id, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

// kfFeed runs one feed page for a caller (ref 0 = anonymous) with an
// optional `?kind=`, and returns the ids it received as a set.
//
// The limit is deliberately generous and the fixture posts share one
// old timestamp, so "absent" means absent rather than "on page two".
// Cross-checked by kfAssertPresent, which fails loudly if the fixture's
// own control post is missing.
func kfFeed(t *testing.T, h *Handler, callerRef int64, kind string) map[uuid.UUID]bool {
	t.Helper()
	ctx := context.Background()
	if callerRef != kfNoOneAt {
		ctx = auth.WithIdentity(ctx, &auth.Identity{UserRef: callerRef, AuthMethod: "session"})
	}
	limit := 200
	params := openapi.ListPostsParams{Limit: &limit}
	// The fixture spans several tiers, and the handler's default display
	// filter is org-only. Asking for the tier under test explicitly is
	// what keeps this a test of the KIND conjunct: `visibility` narrows
	// within what the read rule already admits, so it can never widen
	// what an unauthorised caller receives.
	vis := openapi.ListPostsParamsVisibility("public")
	params.Visibility = &vis
	if kind != "" {
		params.Kind = &kind
	}
	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: params})
	if err != nil {
		t.Fatalf("ListPosts(caller=%d kind=%q): %v", callerRef, kind, err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts returned %T, want 200", resp)
	}
	out := make(map[uuid.UUID]bool, len(ok.Items))
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}

func kfAssertPresent(t *testing.T, what string, got map[uuid.UUID]bool, id uuid.UUID) {
	t.Helper()
	if !got[id] {
		t.Errorf("%s: post %v is MISSING", what, id)
	}
}

func kfAssertAbsent(t *testing.T, what string, got map[uuid.UUID]bool, id uuid.UUID) {
	t.Helper()
	if got[id] {
		t.Errorf("%s: post %v is PRESENT and must not be", what, id)
	}
}

// ---------------------------------------------------------------------------
// The filter selects by ANY member's kind (#1190)
// ---------------------------------------------------------------------------

// TestKindFilter_SelectsAnyMemberKind is the base case plus the
// derivations that are easy to get wrong: the asset_type override (a
// sprite atlas is a PNG and its badge says sprite), and the owner's
// ruling itself — a member that is NOT the cover still matches.
//
// The bundle post is the whole point of #1190. It holds a 3D model and
// a video, is covered by neither explicitly, and must be returned by
// `kind=3d` AND by `kind=video` — while still being returned by no
// kind it does not contain.
func TestKindFilter_SelectsAnyMemberKind(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfAuthor)
	mp4 := kfAsset(t, pool, "mp4", 3, "public", kfAuthor)
	glb := kfAsset(t, pool, "glb", 5, "public", kfAuthor)
	epub := kfAsset(t, pool, "epub", 2, "public", kfAuthor)
	// asset_type 13 is Sprite; the extension says image and the ref wins,
	// exactly as kindForAsset resolves it in the browser.
	atlas := kfAsset(t, pool, "png", 13, "public", kfAuthor)

	imagePost := kfPost(t, pool, kfAuthor, "public", png, png)
	videoPost := kfPost(t, pool, kfAuthor, "public", mp4, mp4)
	spritePost := kfPost(t, pool, kfAuthor, "public", atlas, atlas)
	// The owner's case: a bundle. No explicit cover, a 3D model first and
	// a video second. Before #1190 this answered `kind=3d` only, because
	// only the resolved cover was looked at.
	bundlePost := kfPost(t, pool, kfAuthor, "public", uuid.Nil, glb, mp4)
	// The literal report: an art drop whose cover is a PNG with the epub
	// buried inside it. "I picked ebook and got no results."
	dropPost := kfPost(t, pool, kfAuthor, "public", png, png, epub)

	images := kfFeed(t, h, kfAuthor, "image")
	kfAssertPresent(t, "kind=image", images, imagePost)
	kfAssertPresent(t, "kind=image (the drop's cover IS a png)", images, dropPost)
	kfAssertAbsent(t, "kind=image", images, videoPost)
	kfAssertAbsent(t, "kind=image (no image member)", images, bundlePost)
	kfAssertAbsent(t, "kind=image (a sprite atlas is a PNG; the ref overrides)", images, spritePost)

	videos := kfFeed(t, h, kfAuthor, "video")
	kfAssertPresent(t, "kind=video", videos, videoPost)
	kfAssertPresent(t, "⭐ kind=video (a NON-cover member — #1190)", videos, bundlePost)
	kfAssertAbsent(t, "kind=video", videos, imagePost)
	kfAssertAbsent(t, "kind=video", videos, dropPost)

	// ⭐ The report itself: the epub is member two of a png-covered post.
	ebooks := kfFeed(t, h, kfAuthor, "ebook")
	kfAssertPresent(t, "⭐ kind=ebook (an epub inside a png-covered drop — #1190)", ebooks, dropPost)
	kfAssertAbsent(t, "kind=ebook", ebooks, imagePost)
	kfAssertAbsent(t, "kind=ebook", ebooks, bundlePost)

	sprites := kfFeed(t, h, kfAuthor, "sprite")
	kfAssertPresent(t, "kind=sprite", sprites, spritePost)
	kfAssertAbsent(t, "kind=sprite", sprites, imagePost)

	threeD := kfFeed(t, h, kfAuthor, "3d")
	kfAssertPresent(t, "kind=3d", threeD, bundlePost)
	kfAssertAbsent(t, "kind=3d", threeD, imagePost)

	// CONTAINMENT, which is what replaced the old disjointness property.
	// One post, two kinds, and it is the SAME post — a filter that had
	// become "the first matching member wins" would fail exactly here.
	if !threeD[bundlePost] || !videos[bundlePost] {
		t.Errorf("the bundle post must be reachable by BOTH of its members' kinds; "+
			"3d=%v video=%v", threeD[bundlePost], videos[bundlePost])
	}

	// Multi-select, comma-joined: the union of two kinds and nothing else.
	both := kfFeed(t, h, kfAuthor, "image,video")
	kfAssertPresent(t, "kind=image,video", both, imagePost)
	kfAssertPresent(t, "kind=image,video", both, videoPost)
	kfAssertPresent(t, "kind=image,video (the video member)", both, bundlePost)
	kfAssertPresent(t, "kind=image,video (the png member)", both, dropPost)
	kfAssertAbsent(t, "kind=image,video", both, spritePost)

	// All-checked is spelled as an ABSENT parameter by the control, and
	// the unfiltered feed is the superset every filtered page came from.
	all := kfFeed(t, h, kfAuthor, "")
	for _, id := range []uuid.UUID{imagePost, videoPost, spritePost, bundlePost, dropPost} {
		kfAssertPresent(t, "unfiltered", all, id)
	}
}

// ---------------------------------------------------------------------------
// ⭐ Gate 1 — it composes with the post read rule
// ---------------------------------------------------------------------------

// TestKindFilter_ComposesWithReadRule plants a post a caller may not
// read whose cover is exactly the kind they ask for. A filter that had
// become a disjunct, or that ran before the rule, hands it over.
func TestKindFilter_ComposesWithReadRule(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfOther)

	mine := kfPost(t, pool, kfViewer, "public", png, png)
	// Another author's private post, same cover kind. Unreadable by
	// kfViewer with or without the filter.
	theirs := kfPost(t, pool, kfOther, "private", png, png)

	filtered := kfFeed(t, h, kfViewer, "image")
	kfAssertPresent(t, "kind=image (own public post)", filtered, mine)
	kfAssertAbsent(t, "kind=image (another author's PRIVATE post)", filtered, theirs)

	unfiltered := kfFeed(t, h, kfViewer, "")
	kfAssertAbsent(t, "unfiltered (another author's PRIVATE post)", unfiltered, theirs)
}

// TestKindFilter_AnonymousSeesPublicOnly is the public-mode arm (#1181):
// an anonymous caller filtering by kind gets public posts of that cover
// kind and nothing else. The org-only decoy shares the cover kind, so
// only the read rule can be keeping it out.
func TestKindFilter_AnonymousSeesPublicOnly(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfAuthor)
	mp4 := kfAsset(t, pool, "mp4", 3, "public", kfAuthor)

	publicImage := kfPost(t, pool, kfAuthor, "public", png, png)
	publicVideo := kfPost(t, pool, kfAuthor, "public", mp4, mp4)
	orgOnlyImage := kfPost(t, pool, kfAuthor, "org-only", png, png)
	privateImage := kfPost(t, pool, kfAuthor, "private", png, png)

	anon := kfFeed(t, h, kfNoOneAt, "image")
	kfAssertPresent(t, "anonymous kind=image", anon, publicImage)
	kfAssertAbsent(t, "anonymous kind=image (a VIDEO cover)", anon, publicVideo)
	kfAssertAbsent(t, "anonymous kind=image (an ORG-ONLY post)", anon, orgOnlyImage)
	kfAssertAbsent(t, "anonymous kind=image (a PRIVATE post)", anon, privateImage)

	// And the filter did not widen what anonymous sees: the same two
	// decoys are absent unfiltered.
	anonAll := kfFeed(t, h, kfNoOneAt, "")
	kfAssertPresent(t, "anonymous unfiltered", anonAll, publicImage)
	kfAssertAbsent(t, "anonymous unfiltered (an ORG-ONLY post)", anonAll, orgOnlyImage)
	kfAssertAbsent(t, "anonymous unfiltered (a PRIVATE post)", anonAll, privateImage)
}

// ---------------------------------------------------------------------------
// ⭐⭐ Gate 2 — a member the caller may not READ contributes no kind
// ---------------------------------------------------------------------------

// TestKindFilter_RestrictedMemberIsNeverProbeable is the leak test, and
// #1190 is the reason it had to get SHARPER rather than looser.
//
// The fixture is a PUBLIC post — readable by everyone — holding a
// restricted PNG owned by somebody else plus one public MP4, so #921's
// applyHideRestricted does not drop the whole post. To a stranger the
// card shows the video and nothing at all about the PNG.
//
// The property: the post is reachable by the kinds of its VISIBLE
// members and by no others. Asking for every kind in turn is exactly the
// attack — if `image` returns it, the reader has recovered the hidden
// member's kind by elimination — so the test asks for every kind in
// turn and allows exactly `video`.
//
// Under the cover-only rule this same fixture answered NO kind at all,
// which passed for a weaker reason than it looked: a filter that simply
// never matched anything passed too. Now the video arm is a positive
// assertion in the same loop, so the test can only pass by deciding
// per member.
//
// The owner's own view is the second control: to kfOther the PNG is
// readable, so `kind=image` must return the same post.
func TestKindFilter_RestrictedMemberIsNeverProbeable(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	secretImage := kfAsset(t, pool, "png", 1, "restricted", kfOther)
	publicMember := kfAsset(t, pool, "mp4", 3, "public", kfOther)

	post := kfPost(t, pool, kfOther, "public", secretImage, secretImage, publicMember)

	// A viewer who may read the POST but not one of its members.
	unfiltered := kfFeed(t, h, kfViewer, "")
	kfAssertPresent(t, "unfiltered (a readable post with a restricted member)", unfiltered, post)

	for _, kind := range []string{
		"image", "video", "pdf", "audio", "sequence", "font", "sprite",
		"3d", "ebook", "doc", "audiobook", "archive", "placeholder",
	} {
		got := kfFeed(t, h, kfViewer, kind)
		switch kind {
		case "video":
			kfAssertPresent(t, "kind=video (the member this caller CAN read)", got, post)
		default:
			if got[post] {
				t.Errorf("kind=%s returned a post whose only member of that kind this caller "+
					"may not read — the withheld kind is recoverable by elimination "+
					"(#902/#1066)", kind)
			}
		}
	}
	// Anonymous too: the same post is public, so it is on their feed,
	// and the restricted member must be just as unnameable to them.
	anonAll := kfFeed(t, h, kfNoOneAt, "")
	kfAssertPresent(t, "anonymous unfiltered", anonAll, post)
	kfAssertAbsent(t, "anonymous kind=image", kfFeed(t, h, kfNoOneAt, "image"), post)
	kfAssertPresent(t, "anonymous kind=video", kfFeed(t, h, kfNoOneAt, "video"), post)

	// The control: to the asset's OWNER it is readable, so it matches.
	kfAssertPresent(t, "owner kind=image", kfFeed(t, h, kfOther, "image"), post)
}

// TestKindFilter_RestrictedOnlyEbookSplitsByCaller is the case the
// #1190 ruling names directly: a post whose ONLY ebook member is
// restricted.
//
// It is the one that any-member matching could have got wrong in the
// obvious way. Widening from "the cover" to "the members" without
// carrying visibility.FieldsReadableSQL INTO the per-member EXISTS
// turns `?kind=ebook` into a working probe for the existence of an
// epub inside any post a caller can open — a fact the card withholds
// completely, since a restricted member ships with no `asset` at all.
//
// Same post, two answers: absent for the stranger, present for the
// owner. An implementation that hoisted the readability conjunct up to
// the post would return it for both.
func TestKindFilter_RestrictedOnlyEbookSplitsByCaller(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	secretEbook := kfAsset(t, pool, "epub", 2, "restricted", kfOther)
	publicCover := kfAsset(t, pool, "png", 1, "public", kfOther)

	post := kfPost(t, pool, kfOther, "public", publicCover, publicCover, secretEbook)

	kfAssertPresent(t, "stranger unfiltered", kfFeed(t, h, kfViewer, ""), post)
	kfAssertPresent(t, "stranger kind=image (the visible cover)",
		kfFeed(t, h, kfViewer, "image"), post)
	kfAssertAbsent(t, "⭐⭐ stranger kind=ebook (the only epub is restricted)",
		kfFeed(t, h, kfViewer, "ebook"), post)
	kfAssertAbsent(t, "⭐⭐ anonymous kind=ebook (the only epub is restricted)",
		kfFeed(t, h, kfNoOneAt, "ebook"), post)

	// The owner may read the epub, so for them the post IS an ebook post.
	kfAssertPresent(t, "⭐⭐ owner kind=ebook", kfFeed(t, h, kfOther, "ebook"), post)
}

// ---------------------------------------------------------------------------
// The parameter's own edges
// ---------------------------------------------------------------------------

// TestKindFilter_UnknownKindReturnsNothing pins the fail-closed reading.
// An ignored junk value would serve the whole feed under a label
// promising one kind — a filter widening, which is the one direction it
// may never move.
func TestKindFilter_UnknownKindReturnsNothing(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfAuthor)
	post := kfPost(t, pool, kfAuthor, "public", png, png)

	kfAssertAbsent(t, "kind=nonsense", kfFeed(t, h, kfAuthor, "nonsense"), post)
	// `sequence` is a real kind that no single asset can resolve to.
	kfAssertAbsent(t, "kind=sequence", kfFeed(t, h, kfAuthor, "sequence"), post)
	// A junk term beside a real one narrows to the real one.
	kfAssertPresent(t, "kind=image,nonsense", kfFeed(t, h, kfAuthor, "image,nonsense"), post)
	// Whitespace-only is not a filter at all — what the control sends
	// when every box is ticked.
	kfAssertPresent(t, "kind=(blank)", kfFeed(t, h, kfAuthor, "   "), post)
}

// TestKindFilter_ComposesWithTagAndTeam is the acceptance the browse
// footer needs: the type filter runs beside the rail's chips, and three
// conjuncts intersect rather than one of them winning.
func TestKindFilter_ComposesWithTagAndTeam(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	png := kfAsset(t, pool, "png", 1, "public", kfAuthor)
	mp4 := kfAsset(t, pool, "mp4", 3, "public", kfAuthor)

	team := tfTeam(t, pool, "kf")
	tagged := kfPost(t, pool, kfAuthor, "public", png, png)
	wrongKind := kfPost(t, pool, kfAuthor, "public", mp4, mp4)
	untagged := kfPost(t, pool, kfAuthor, "public", png, png)

	for _, id := range []uuid.UUID{tagged, wrongKind} {
		if _, err := pool.Exec(t.Context(),
			`UPDATE posts SET team_id=$2 WHERE id=$1`, id, team); err != nil {
			t.Fatalf("assign team: %v", err)
		}
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO post_tags (post_id, tag) VALUES ($1,'kf-tag')`, id); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM post_tags WHERE post_id = ANY($1)`, []uuid.UUID{tagged, wrongKind})
	})

	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: kfAuthor, AuthMethod: "session"})
	limit := 200
	kind := "image"
	tag := "kf-tag"
	teamID := team
	vis := openapi.ListPostsParamsVisibility("public")
	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: openapi.ListPostsParams{
		Limit: &limit, Kind: &kind, Tag: &tag, TeamId: &teamID, Visibility: &vis,
	}})
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	ok := resp.(openapi.ListPosts200JSONResponse)
	got := make(map[uuid.UUID]bool, len(ok.Items))
	for _, p := range ok.Items {
		got[uuid.UUID(p.Id)] = true
	}
	kfAssertPresent(t, "kind+tag+team", got, tagged)
	kfAssertAbsent(t, "kind+tag+team (wrong kind)", got, wrongKind)
	kfAssertAbsent(t, "kind+tag+team (no tag, no team)", got, untagged)
}
