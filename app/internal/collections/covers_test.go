// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1026 — the composed cover mosaic on GET /collections{,/{id}}.
//
// # What makes this file worth reading
//
// The reported bug ("my collection with two posts and one asset has no
// thumbnail") is the easy half: TestCollectionCovers_PostOnly fails
// against the old composer and passes against any implementation that
// looks at `collection_posts` at all.
//
// The half that would silently not work is
// TestCollectionCovers_WithheldMembersDoNotCrowdTheMosaic. The composer
// this replaces did not LEAK restricted members — it rendered each one
// as a blank tile that still consumed a slot, so four restricted members
// at the head of a collection produced four blank quarters and every
// renderable member behind them never got a chance. An implementation
// that filters withheld members out of the RESULT but not out of the
// CANDIDATE SET (fetch four, drop the bad ones) passes every other test
// in this file and returns an empty mosaic here.
//
// And the direction that must not regress while fixing it:
// TestCollectionCovers_WithheldMemberContributesNothing asserts on the
// SERVED PAYLOAD — the withheld asset's id must not appear at all. Row
// counts would not catch a composer that shipped the id with a false
// availability flag, which is precisely the shape the old client card
// consumed.
//
// Every withheld fixture is given a `col` rendition, so it IS a
// candidate on every axis except the caller's right to see it. Without
// that, these tests would pass on an implementation with no visibility
// rule whatsoever.
//
// Skips without AA_DB_PASSWORD.

package collections_test

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	ccOwner    int64 = 1026001 // owns the collection and does the reading
	ccStranger int64 = 1026002 // owns the withheld members
)

// ccRenderableAsset plants an asset that CAN paint a tile: a real
// storage object, a `col` variant, active + ready.
//
// `sensitivity` is the only knob, so a test that flips it from "public"
// to "restricted" changes exactly one thing about the row.
func ccRenderableAsset(t *testing.T, pool *pgxpool.Pool, owner int64, title, sensitivity string) string {
	t.Helper()
	ctx := context.Background()
	// storage_objects.hash is CHECKed against ^[0-9a-f]{64}$; a UUID's
	// 32 hex digits doubled is exactly that and is unique per call.
	raw := uuid.New()
	hash := hex.EncodeToString(raw[:]) + hex.EncodeToString(raw[:])
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 1, 'fs')`,
		hash); err != nil {
		t.Fatalf("seed storage object: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 1)`,
		hash); err != nil {
		t.Fatalf("seed col variant: %v", err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO assets (title, asset_type, owner_user_ref, file_hash,
		                     sensitivity, status, processing_status)
		 VALUES ($1, 1, $2, $3, $4, 'active', 'ready') RETURNING id`,
		title, owner, hash, sensitivity).Scan(&id); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	return id.String()
}

// ccPost plants a post whose feed cover is `coverAssetID`. `thumbID` is
// the standalone cover_thumbnail_asset_id, "" for none.
func ccPost(t *testing.T, pool *pgxpool.Pool, author int64, vis, coverAssetID, thumbID string) string {
	t.Helper()
	var thumb any
	if thumbID != "" {
		thumb = thumbID
	}
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO posts (author_user_ref, title, visibility,
		                    cover_asset_id, cover_thumbnail_asset_id)
		 VALUES ($1, 'ct_cover_post', $2, $3, $4) RETURNING id`,
		author, vis, coverAssetID, thumb).Scan(&id); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id)
	})
	return id.String()
}

// ccPinAssetViaPost / ccPinPost make a membership row at an EXPLICIT
// added_at, which is the axis the mosaic orders on. Written directly
// rather than through the add endpoint because the ordering is a
// property under test and `now()` cannot express it.
//
// ccPinAssetViaPost replaced ccPinAsset with #1236. The old helper wrote
// a `collection_resources` row — the direct route, which no longer
// paints a tile. Every assertion those fixtures made is about the ASSET
// PICTURE PLANE (restricted, draft, mature, caps) and survives the
// change unaltered, because the mosaic gates a post's cover asset with
// the identical `previewFrag` + mature conjunct it used to splice on the
// direct half: `renderable` joins `assets` whichever half fed it.
//
// The wrapper post is PUBLIC and authored by the reader, so the post
// plane admits it unconditionally and the asset plane stays the only
// variable — which is what each caller wants. A test that needs the POST
// half withheld builds its own post through ccPost.
func ccPinAssetViaPost(t *testing.T, pool *pgxpool.Pool, colID, assetID string, at time.Time) {
	t.Helper()
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", assetID, ""), at)
}

// ccPinBareAsset writes the RETIRED direct membership row —
// `collection_resources` — which no rendered surface draws any more
// (#1236). It exists so the tests above can prove the mosaic ignores
// such a row, which is a fact about rows that must be PRESENT.
//
// The table itself is internal by decision, not gone; see the note in
// handler.go for the writers and readers that keep it alive.
func ccPinBareAsset(t *testing.T, pool *pgxpool.Pool, colID, assetID string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_resources (collection_id, asset_id, sort_order, pinned, added_at)
		 VALUES ($1, $2, 0, TRUE, $3)`, colID, assetID, at); err != nil {
		t.Fatalf("pin bare asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM collection_resources WHERE collection_id = $1 AND asset_id = $2`,
			colID, assetID)
	})
}

func ccPinPost(t *testing.T, pool *pgxpool.Pool, colID, postID string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_posts (collection_id, post_id, sort_order, pinned, added_at)
		 VALUES ($1, $2, 0, TRUE, $3)`, colID, postID, at); err != nil {
		t.Fatalf("pin post: %v", err)
	}
}

// ccCovers reads GET /collections/{id} and returns the cover asset ids
// in served order. It asserts on the WIRE payload — the whole point is
// what a client receives, not what a query returned.
func ccCovers(t *testing.T, r interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, colID string) []string {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/collections/"+colID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /collections/%s: status=%d body=%s", colID, rr.Code, rr.Body.String())
	}
	var c openapi.Collection
	mustDecode(t, rr.Body.Bytes(), &c)
	if c.Covers == nil {
		t.Fatalf("GET /collections/%s: `covers` ABSENT — the detail surface composes covers, "+
			"so absence here is the field never being stamped, not an empty collection",
			colID)
	}
	out := make([]string, 0, len(*c.Covers))
	for _, cv := range *c.Covers {
		out = append(out, cv.AssetId.String())
	}
	return out
}

// ccSetup opens the pool and cleans every table these tests touch.
func ccSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM collection_posts WHERE collection_id IN
			(SELECT id FROM collections WHERE name LIKE 'ct_%')`)
		cleanTestCollections(t, pool)
		_, _ = pool.Exec(ctx, `DELETE FROM posts WHERE author_user_ref IN ($1,$2)`, ccOwner, ccStranger)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE owner_user_ref IN ($1,$2)`, ccOwner, ccStranger)
	}
	clean()
	t.Cleanup(clean)
	return pool
}

// TestCollectionCovers_PostOnly is #1026 as reported: a collection whose
// members are all POSTS had no cover source at all, because the composer
// only ever read collection_resources. #882 made this an ordinary thing
// to own — "save someone else's post" produces exactly this collection.
func TestCollectionCovers_PostOnly(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_post_only", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	coverA := ccRenderableAsset(t, pool, ccOwner, "ct_cover_a", "public")
	coverB := ccRenderableAsset(t, pool, ccOwner, "ct_cover_b", "public")
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", coverA, ""), base)
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", coverB, ""), base.Add(time.Minute))

	got := ccCovers(t, router, colID)
	if len(got) == 0 {
		t.Fatal("a post-only collection returned NO covers — that is #1026: the composer " +
			"never learned about collection_posts, so a collection of saved posts renders " +
			"as an empty folder")
	}
	want := []string{coverA, coverB}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("covers = %v, want %v (each post contributes its feed cover, in added_at order)", got, want)
	}
}

// TestCollectionCovers_PrefersPostThumbnail pins the contribution rule.
// It is not invented here: the Post schema already specifies that a feed
// card prefers cover_thumbnail_asset_id over cover_asset_id, and a tile
// showing a different picture from the card for the same post would be a
// second rule for one question.
func TestCollectionCovers_PrefersPostThumbnail(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_thumb_pref", "visibility": "private"})

	member := ccRenderableAsset(t, pool, ccOwner, "ct_cover_member", "public")
	standalone := ccRenderableAsset(t, pool, ccOwner, "ct_cover_standalone", "public")
	ccPinPost(t, pool, colID,
		ccPost(t, pool, ccOwner, "public", member, standalone), time.Now().Add(-time.Hour))

	got := ccCovers(t, router, colID)
	if len(got) != 1 || got[0] != standalone {
		t.Errorf("covers = %v, want [%s] — cover_thumbnail_asset_id wins over cover_asset_id, "+
			"the same preference the feed card uses", got, standalone)
	}
}

// TestCollectionCovers_OrdersByAddedAt pins the ordering decision:
// added_at ascending, which is the arrangement the curator built. A
// composer that ordered by sort_order or by post id returns the same SET
// and the wrong arrangement, and the arrangement is the curation.
//
// This used to be TestCollectionCovers_InterleavesBothKinds, asserting
// the interleave ACROSS the two membership tables. #1236 left one table,
// so there is nothing to interleave — but added_at is still the lead
// key, and a rank that lost it would still be wrong.
func TestCollectionCovers_OrdersByAddedAt(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_order", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	p1 := ccRenderableAsset(t, pool, ccOwner, "ct_il_p1", "public")
	p2 := ccRenderableAsset(t, pool, ccOwner, "ct_il_p2", "public")
	p3 := ccRenderableAsset(t, pool, ccOwner, "ct_il_p3", "public")

	// Pinned OUT of added_at order, so a composer that simply returned
	// insertion order would fail.
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", p2, ""), base.Add(2*time.Minute))
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", p1, ""), base)
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", p3, ""), base.Add(4*time.Minute))

	got := strings.Join(ccCovers(t, router, colID), ",")
	want := strings.Join([]string{p1, p2, p3}, ",")
	if got != want {
		t.Errorf("covers = %v\nwant   %v\nadded_at ascending is the curator's arrangement", got, want)
	}
}

// TestCollectionCovers_BareAssetMembersPaintNothing is #1236's central
// assertion, on a collection carrying BOTH halves.
//
// A `collection_resources` row is not a member of anything a reader can
// open: #1161 retired the write path and #1185 took the asset section
// off the collection page. This composer was the last surface still
// drawing them, so the tile summarised content the collection does not
// contain — and on the seeded corpus the bare half outnumbered the post
// half roughly two to one, so it was most of the tile.
//
// The bare assets here are PUBLIC and carry a `col` rendition, so they
// are candidates on every axis except the one that now matters: which
// table they arrived through. An implementation that kept the half
// returns four covers; the fixture is built so the wrong answer is not
// merely a different order but a different SET.
func TestCollectionCovers_BareAssetMembersPaintNothing(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_bare", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	bare1 := ccRenderableAsset(t, pool, ccOwner, "ct_bare_1", "public")
	bare2 := ccRenderableAsset(t, pool, ccOwner, "ct_bare_2", "public")
	viaPost := ccRenderableAsset(t, pool, ccOwner, "ct_bare_via_post", "public")

	// The bare rows go FIRST, so a surviving half would take the lead
	// slots and the assertion cannot pass by accident on ordering.
	ccPinBareAsset(t, pool, colID, bare1, base)
	ccPinBareAsset(t, pool, colID, bare2, base.Add(time.Minute))
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", viaPost, ""), base.Add(2*time.Minute))

	got := ccCovers(t, router, colID)
	if strings.Join(got, ",") != viaPost {
		t.Errorf("covers = %v, want [%s] — a bare `collection_resources` row is not a "+
			"member of anything the collection page shows, so it must not paint the "+
			"tile that stands for the collection (#1236)", got, viaPost)
	}
}

// TestCollectionCovers_BareAssetOnlyCollectionHasNoCover is the visible
// cost of the change, asserted rather than discovered.
//
// A collection holding ONLY bare assets used to paint a full mosaic and
// open to an empty page. It now yields no covers at all — which is the
// honest answer, because the collection IS empty. 13 collections on the
// seeded corpus are in exactly this state (leftover ui-13 / UI-30
// fixtures).
//
// ⚠️ This is NOT a return of #1026. That defect was a collection with
// RENDERABLE MEMBERS drawing nothing, and before it, withheld members
// crowding renderable ones out of the slots. An empty array here is the
// same answer an empty collection gets, and CollectionCard's deliberate
// empty state renders it — see the header note in covers.go.
func TestCollectionCovers_BareAssetOnlyCollectionHasNoCover(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_bare_only", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	for i, title := range []string{"ct_bare_only_1", "ct_bare_only_2", "ct_bare_only_3"} {
		ccPinBareAsset(t, pool, colID,
			ccRenderableAsset(t, pool, ccOwner, title, "public"),
			base.Add(time.Duration(i)*time.Minute))
	}

	if got := ccCovers(t, router, colID); len(got) != 0 {
		t.Errorf("covers = %v, want [] — a collection whose only members are bare assets "+
			"contains nothing a reader can open, so the tile has nothing to summarise", got)
	}
}

// TestCollectionCovers_DeduplicatesOneAssetReachedTwice — one asset can
// be a cover candidate several times over: two posts in a collection
// sharing a cover, the same file posted twice, a repost. Seed data has
// plenty.
//
// (Before #1236 there was a third route — pinned directly AND carried as
// a post's cover — and it is gone with the direct half. The DISTINCT ON
// is not: two posts sharing a cover is the ordinary case and always was.)
//
// Two things go wrong without the DISTINCT ON. The mosaic paints the
// same picture two to four times, which summarises nothing; and
// CollectionCard's keyed {#each covers as a (a.asset_id)} throws
// `each_key_duplicate` and takes the WHOLE HUB PAGE down — no cards at
// all, not just a wrong one. (That is how this was found: in the
// browser, on real seeded posts, after every unit test here was green.)
//
// The duplicate must also not EAT A SLOT, which is why it is collapsed
// before the rank rather than after — hence the fourth member and the
// exact-length assertion.
func TestCollectionCovers_DeduplicatesOneAssetReachedTwice(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_dedupe", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	shared := ccRenderableAsset(t, pool, ccOwner, "ct_dedupe_shared", "public")
	other := ccRenderableAsset(t, pool, ccOwner, "ct_dedupe_other", "public")

	// The same asset three ways, then one distinct member behind them.
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", shared, ""), base)
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", shared, ""), base.Add(time.Minute))
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", shared, ""), base.Add(2*time.Minute))
	ccPinAssetViaPost(t, pool, colID, other, base.Add(3*time.Minute))

	got := ccCovers(t, router, colID)
	want := []string{shared, other}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("covers = %v, want %v — one asset is one tile, at its earliest "+
			"position, however many membership rows point at it", got, want)
	}
}

// TestCollectionCovers_WithheldMembersDoNotCrowdTheMosaic is the
// crowding fix, and the one that would silently not work.
//
// The FIRST FOUR members are all withheld from this caller — a
// restricted asset owned by a stranger, a private post owned by a
// stranger, and two more of each. Every one of them has a `col`
// rendition, so the only thing keeping them out is the caller's right to
// see them. Behind them sit four renderable members, and those are the
// mosaic.
//
// An implementation that fetches `limit: 4` and then filters returns
// NOTHING here. One that slots the withheld members as placeholders
// returns four blanks. Both are the shipped bug.
func TestCollectionCovers_WithheldMembersDoNotCrowdTheMosaic(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_crowding", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	// Four withheld members at the head, alternating the PLANE that
	// withholds them so a bug in one gate cannot hide behind the other:
	// a stranger's restricted asset carried by a post the caller may
	// read (the asset plane refuses the picture), then a public asset
	// carried by a stranger's PRIVATE post (the post plane refuses the
	// row). Before #1236 the alternation was across the two membership
	// tables; the two planes are the pair that is left, and they are the
	// pair that matters.
	hidden := make([]string, 0, 4)
	for i := 0; i < 2; i++ {
		a := ccRenderableAsset(t, pool, ccStranger, "ct_crowd_hidden_asset", "restricted")
		ccPinAssetViaPost(t, pool, colID, a, at(2*i))
		hidden = append(hidden, a)

		pc := ccRenderableAsset(t, pool, ccStranger, "ct_crowd_hidden_post_cover", "public")
		ccPinPost(t, pool, colID, ccPost(t, pool, ccStranger, "private", pc, ""), at(2*i+1))
		hidden = append(hidden, pc)
	}

	// Four renderable members behind them.
	want := make([]string, 0, 4)
	for i := 0; i < 2; i++ {
		a := ccRenderableAsset(t, pool, ccOwner, "ct_crowd_ok_asset", "public")
		ccPinAssetViaPost(t, pool, colID, a, at(4+2*i))
		want = append(want, a)

		pc := ccRenderableAsset(t, pool, ccOwner, "ct_crowd_ok_post_cover", "public")
		ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", pc, ""), at(4+2*i+1))
		want = append(want, pc)
	}

	got := ccCovers(t, router, colID)
	if len(got) == 0 {
		t.Fatal("four withheld members at the head of the collection crowded out every " +
			"renderable member behind them — the mosaic came back EMPTY. That is #1026's " +
			"crowding half: a withheld member must be SKIPPED, not slotted")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("covers = %v\nwant   %v", got, want)
	}
	// And the non-leak, restated on this fixture: nothing withheld may
	// have reached the payload while we were fixing the crowding.
	for _, h := range hidden {
		for _, g := range got {
			if g == h {
				t.Errorf("withheld member %s appears in the mosaic — the crowding fix "+
					"widened the picture plane", h)
			}
		}
	}
}

// TestCollectionCovers_WithheldMemberContributesNothing is the direction
// that must not regress. It asserts on the SERVED PAYLOAD rather than on
// a row count: a composer that shipped the withheld id with a false
// availability flag would satisfy any count-based assertion, and that is
// exactly the shape the deleted client card consumed.
//
// Both legs matter. The stranger sees only the public member; the OWNER
// of the withheld members sees all three. Without the second leg a
// composer that denies everybody passes.
func TestCollectionCovers_WithheldMemberContributesNothing(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	strangerRouter, _ := makeRouter(t, pool, ccStranger /*admin=*/, false)

	// PUBLIC-tier collection so both callers can read the parent; the
	// member gate is the only thing under test.
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_withheld", "visibility": "public",
	})

	base := time.Now().Add(-time.Hour)
	open := ccRenderableAsset(t, pool, ccOwner, "ct_wh_open", "public")
	secretAsset := ccRenderableAsset(t, pool, ccStranger, "ct_wh_secret_asset", "restricted")
	secretPostCover := ccRenderableAsset(t, pool, ccStranger, "ct_wh_secret_post_cover", "public")

	ccPinAssetViaPost(t, pool, colID, open, base)
	ccPinAssetViaPost(t, pool, colID, secretAsset, base.Add(time.Minute))
	ccPinPost(t, pool, colID,
		ccPost(t, pool, ccStranger, "private", secretPostCover, ""), base.Add(2*time.Minute))

	got := ccCovers(t, ownerRouter, colID)
	for _, forbidden := range []string{secretAsset, secretPostCover} {
		for _, g := range got {
			if g == forbidden {
				t.Errorf("asset %s reached the mosaic of a caller who may not picture it "+
					"(covers=%v)", forbidden, got)
			}
		}
	}
	if len(got) != 1 || got[0] != open {
		t.Errorf("covers = %v, want [%s] — the one member this caller may picture", got, open)
	}

	// The converse: the stranger owns both withheld members, so all
	// three are theirs to see. A deny-everything composer fails here.
	ownGot := ccCovers(t, strangerRouter, colID)
	if len(ownGot) != 3 {
		t.Errorf("the owner of the withheld members got %v (%d covers), want all 3 — "+
			"a composer that denied everybody would pass the leak assertion above",
			ownGot, len(ownGot))
	}
}

// TestCollectionCovers_AnonymousCaller pins the anonymous branch of the
// picture plane, which is the ONE arm with extra conjuncts (status +
// processing_status) and therefore the arm a splice site gets wrong.
func TestCollectionCovers_AnonymousCaller(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	anon := anonRouter(t, pool)

	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_anon", "visibility": "public",
	})

	base := time.Now().Add(-time.Hour)
	open := ccRenderableAsset(t, pool, ccOwner, "ct_anon_open", "public")
	draft := ccRenderableAsset(t, pool, ccOwner, "ct_anon_draft", "public")
	setAssetTier(t, pool, draft, "draft", "public")
	restricted := ccRenderableAsset(t, pool, ccOwner, "ct_anon_restricted", "restricted")
	ccPinAssetViaPost(t, pool, colID, draft, base)
	ccPinAssetViaPost(t, pool, colID, restricted, base.Add(time.Minute))
	ccPinAssetViaPost(t, pool, colID, open, base.Add(2*time.Minute))

	got := ccCovers(t, anon, colID)
	if len(got) != 1 || got[0] != open {
		t.Errorf("anonymous covers = %v, want [%s] — an anonymous caller gets neither a "+
			"draft nor a restricted-tier picture, and the two ahead of it must not "+
			"crowd the one it may see", got, open)
	}
}

// ccRouterWithCaps is makeRouter with an arbitrary capability set.
// makeRouter only offers `collections.admin`, and the case below needs
// a CONTENT-plane capability.
func ccRouterWithCaps(t *testing.T, pool *pgxpool.Pool, userRef int64, caps ...string) chi.Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := collections.NewHandler(pool, logger, nil)
	h.SetActivitiesWriter(activities.NewWriter(pool, logger, nil),
		func(ctx context.Context) string { return "https://test.example" })
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(
		collShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

// TestCollectionCovers_CapsHoldingCallerSeesEverything drives the arm
// where the picture-plane fragment is EMPTY.
//
// `content.read.all` and `system.admin` short-circuit
// visibility.PreviewReadableSQL, so the composed statement for such a
// caller contains no readability conjunct at all. That is the arm where
// a splice site that BOUND the caller ref as a placeholder blows up —
// Postgres refuses a statement with a parameter nothing references — and
// it is invisible to every other test here, all of which run as ordinary
// users whose fragment does name the ref. It is also the arm where a
// composer that failed OPEN would be caught, since the demo-viewer role
// this capability exists for renders a mostly-restricted catalogue.
func TestCollectionCovers_CapsHoldingCallerSeesEverything(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_caps", "visibility": "public",
	})

	base := time.Now().Add(-time.Hour)
	open := ccRenderableAsset(t, pool, ccOwner, "ct_caps_open", "public")
	secret := ccRenderableAsset(t, pool, ccStranger, "ct_caps_secret", "restricted")
	ccPinAssetViaPost(t, pool, colID, secret, base)
	ccPinAssetViaPost(t, pool, colID, open, base.Add(time.Minute))

	for _, cap := range []string{"content.read.all", "system.admin"} {
		t.Run(cap, func(t *testing.T) {
			viewer := ccRouterWithCaps(t, pool, 1026003, cap)
			got := ccCovers(t, viewer, colID)
			if len(got) != 2 {
				t.Errorf("%s holder got covers %v, want both members — this capability "+
					"admits the picture at every tier (visibility.PreviewReadable)", cap, got)
			}
		})
	}
}

// TestCollectionCovers_EmptyCollectionIsAnEmptyArray pins the
// absent-vs-empty contract the schema states: ABSENT means "this surface
// did not compose covers", so a surface that DOES compose them must send
// [] rather than omitting the key. A client cannot otherwise tell "no
// members" from "not computed".
func TestCollectionCovers_EmptyCollectionIsAnEmptyArray(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_empty", "visibility": "private"})

	if got := ccCovers(t, router, colID); len(got) != 0 {
		t.Errorf("an empty collection returned covers %v", got)
	}
}

// TestCollectionCovers_OnTheListPath — the hub is the surface the issue
// was filed from, and it reads GET /collections, not GET
// /collections/{id}. The list path is a query rather than the by-id
// cache, so it is a genuinely different code path and gets its own
// assertion rather than being assumed from the detail one.
func TestCollectionCovers_OnTheListPath(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_list", "visibility": "private"})

	cover := ccRenderableAsset(t, pool, ccOwner, "ct_list_cover", "public")
	ccPinPost(t, pool, colID,
		ccPost(t, pool, ccOwner, "public", cover, ""), time.Now().Add(-time.Hour))

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/collections?tab=mine&limit=50", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /collections: %d body=%s", rr.Code, rr.Body.String())
	}
	var list openapi.CollectionList
	mustDecode(t, rr.Body.Bytes(), &list)

	for _, c := range list.Items {
		if c.Id.String() != colID {
			continue
		}
		if c.Covers == nil {
			t.Fatal("`covers` ABSENT on the list path — the hub card has nothing to paint")
		}
		if len(*c.Covers) != 1 || (*c.Covers)[0].AssetId.String() != cover {
			t.Errorf("list covers = %v, want [%s]", *c.Covers, cover)
		}
		return
	}
	t.Fatalf("collection %s not in its owner's `mine` listing", colID)
}

// ---------------------------------------------------------------------------
// #1027 — the curator's CHOSEN cover
// ---------------------------------------------------------------------------
//
// The override is a pointer at any asset the curator may PICTURE, stored
// on the collection, and it replaces the derived mosaic above.
//
// The case that would silently not work is
// TestCollectionCover_WithheldOverrideFallsBackToMosaic. An
// implementation that returns the chosen asset unconditionally LEAKS a
// picture; one that returns "the override, gated" without a fallback
// renders a BLANK tile, which is the crowding defect #1026 just fixed
// arriving through a new door. Both are wrong and only the second is
// invisible to a leak test, so the assertion below pins the served
// payload on BOTH counts: the withheld id is absent AND the mosaic is
// what came back instead.

// ccSetCover points a collection at an asset and fails on any non-200,
// so a test asserting on covers cannot pass because the write quietly
// 400'd and left the collection as it was.
func ccSetCover(t *testing.T, r chi.Router, colID, assetID string) {
	t.Helper()
	rr := patchJSON(t, r, "/collections/"+colID, map[string]any{"cover_asset_id": assetID})
	if rr.Code != http.StatusOK {
		t.Fatalf("set cover %s on %s: status=%d body=%s", assetID, colID, rr.Code, rr.Body.String())
	}
}

// TestCollectionCover_OverrideIsTheSoleEntry — a chosen cover wins over
// the mosaic and is the ONLY tile, on the detail path AND the list path.
// The list is a different code path (a query, not the by-id cache), and
// #1026 already found the two can disagree.
//
// The collection is given three renderable members the caller may
// picture, so a composer that ignored the override entirely would return
// three covers and a composer that merely PREPENDED it would return
// four. Only replacement returns one.
func TestCollectionCover_OverrideIsTheSoleEntry(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_cover_sole", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	m1 := ccRenderableAsset(t, pool, ccOwner, "ct_sole_m1", "public")
	m2 := ccRenderableAsset(t, pool, ccOwner, "ct_sole_m2", "public")
	m3 := ccRenderableAsset(t, pool, ccOwner, "ct_sole_m3", "public")
	ccPinAssetViaPost(t, pool, colID, m1, base)
	ccPinAssetViaPost(t, pool, colID, m2, base.Add(time.Minute))
	ccPinAssetViaPost(t, pool, colID, m3, base.Add(2*time.Minute))

	// NOT a member — the whole point of the free pointer is that it need
	// not be one, and a test using a member could not tell the two
	// designs apart.
	chosen := ccRenderableAsset(t, pool, ccOwner, "ct_sole_chosen", "public")
	ccSetCover(t, router, colID, chosen)

	if got := ccCovers(t, router, colID); len(got) != 1 || got[0] != chosen {
		t.Errorf("detail covers = %v, want [%s] — the chosen cover REPLACES the mosaic "+
			"rather than joining it", got, chosen)
	}

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/collections?tab=mine&limit=50", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /collections: %d body=%s", rr.Code, rr.Body.String())
	}
	var list openapi.CollectionList
	mustDecode(t, rr.Body.Bytes(), &list)
	for _, c := range list.Items {
		if c.Id.String() != colID {
			continue
		}
		if c.Covers == nil || len(*c.Covers) != 1 || (*c.Covers)[0].AssetId.String() != chosen {
			t.Errorf("list covers = %v, want [%s] — the list path composes covers "+
				"separately from the detail path and must honour the override too",
				c.Covers, chosen)
		}
		// The curator's SETTING travels beside the render answer, because
		// the edit form needs it to show what is currently chosen.
		if c.CoverAssetId == nil || c.CoverAssetId.String() != chosen {
			t.Errorf("cover_asset_id = %v, want %s — the edit UI populates its picker "+
				"from this field, not from `covers`", c.CoverAssetId, chosen)
		}
		return
	}
	t.Fatalf("collection %s not in its owner's `mine` listing", colID)
}

// TestCollectionCover_ClearRevertsToMosaic — `clear_cover: true` removes
// the choice and the derived mosaic comes back.
//
// It is a companion BOOLEAN rather than `cover_asset_id: null` because a
// partial update cannot express "remove" by sending null: null is
// already "leave alone" for every other property on CollectionUpdate,
// and the generated struct decodes absent and null to the same nil. This
// is the shape metadata's `clear_default` already settled. Sending the
// two together is refused rather than resolved — asserted below, because
// a server that silently preferred one would ship a "clear" that never
// happened.
func TestCollectionCover_ClearRevertsToMosaic(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_cover_clear", "visibility": "private"})

	member := ccRenderableAsset(t, pool, ccOwner, "ct_clear_member", "public")
	ccPinAssetViaPost(t, pool, colID, member, time.Now().Add(-time.Hour))
	chosen := ccRenderableAsset(t, pool, ccOwner, "ct_clear_chosen", "public")

	ccSetCover(t, router, colID, chosen)
	if got := ccCovers(t, router, colID); len(got) != 1 || got[0] != chosen {
		t.Fatalf("precondition: covers = %v, want [%s]", got, chosen)
	}

	if rr := patchJSON(t, router, "/collections/"+colID,
		map[string]any{"clear_cover": true}); rr.Code != http.StatusOK {
		t.Fatalf("clear_cover: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := ccCovers(t, router, colID); len(got) != 1 || got[0] != member {
		t.Errorf("after clear_cover covers = %v, want the derived mosaic [%s]", got, member)
	}
	var stored *string
	if err := pool.QueryRow(context.Background(),
		`SELECT cover_asset_id::TEXT FROM collections WHERE id = $1`, colID).Scan(&stored); err != nil {
		t.Fatalf("read back cover_asset_id: %v", err)
	}
	// Asserting the PERSISTED value, not the response body: the handler
	// echoes the row it just wrote, so a body assertion would pass on an
	// implementation whose CASE never fired.
	if stored != nil {
		t.Errorf("cover_asset_id persisted as %q after clear_cover, want NULL", *stored)
	}

	if rr := patchJSON(t, router, "/collections/"+colID, map[string]any{
		"cover_asset_id": chosen, "clear_cover": true,
	}); rr.Code != http.StatusBadRequest {
		t.Errorf("cover_asset_id + clear_cover together: status=%d, want 400 — two "+
			"intentions in one body, and picking either silently discards the other",
			rr.Code)
	}
}

// TestCollectionCover_WithheldOverrideFallsBackToMosaic is THE case.
//
// The curator may picture a restricted asset they own and pins it as the
// cover. A stranger reading the same public collection may not picture
// it. The stranger must get the DERIVED MOSAIC — not the withheld asset
// (a leak) and not an empty array (a blank tile, #1026's defect through
// a new door).
//
// The owner leg is not decoration: without it, an implementation that
// dropped every override on the floor would pass the stranger leg
// outright.
func TestCollectionCover_WithheldOverrideFallsBackToMosaic(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	strangerRouter, _ := makeRouter(t, pool, ccStranger /*admin=*/, false)

	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_cover_withheld", "visibility": "public",
	})

	// A member EVERYONE may picture, so "the mosaic" is a non-empty
	// answer and "fell back" is distinguishable from "returned nothing".
	open := ccRenderableAsset(t, pool, ccOwner, "ct_wo_open", "public")
	ccPinAssetViaPost(t, pool, colID, open, time.Now().Add(-time.Hour))

	// Restricted and owned by the curator: they may picture it (owner
	// branch of the picture plane), the stranger may not.
	secret := ccRenderableAsset(t, pool, ccOwner, "ct_wo_secret", "restricted")
	ccSetCover(t, ownerRouter, colID, secret)

	if got := ccCovers(t, ownerRouter, colID); len(got) != 1 || got[0] != secret {
		t.Fatalf("the curator's own view = %v, want their chosen cover [%s]", got, secret)
	}

	got := ccCovers(t, strangerRouter, colID)
	for _, g := range got {
		if g == secret {
			t.Fatalf("the chosen cover %s reached a caller who may not picture it (covers=%v) "+
				"— an override must not be a second, weaker door to an asset", secret, got)
		}
	}
	if len(got) != 1 || got[0] != open {
		t.Errorf("stranger covers = %v, want the derived mosaic [%s]. An empty array here "+
			"is a BLANK TILE, which is exactly the crowding defect #1026 fixed; a "+
			"withheld cover must FALL BACK, never render nothing", got, open)
	}
}

// TestCollectionCover_WriteRefusesAnAssetTheCuratorCannotPicture — the
// write gate is the PICTURE plane, matching the read path.
//
// Both legs return the SAME 400. Distinguishing "no such asset" from
// "not yours to look at" would make this endpoint an existence oracle:
// a curator could enumerate ids and read the difference as "this one
// exists and is hidden from me". So the second leg asserts the status
// codes are equal rather than merely both being 4xx.
func TestCollectionCover_WriteRefusesAnAssetTheCuratorCannotPicture(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_cover_refuse", "visibility": "private",
	})

	secret := ccRenderableAsset(t, pool, ccStranger, "ct_refuse_secret", "restricted")
	rrHidden := patchJSON(t, ownerRouter, "/collections/"+colID,
		map[string]any{"cover_asset_id": secret})
	if rrHidden.Code != http.StatusBadRequest {
		t.Errorf("pointing at an asset the curator cannot picture: status=%d, want 400 "+
			"(body=%s)", rrHidden.Code, rrHidden.Body.String())
	}

	rrMissing := patchJSON(t, ownerRouter, "/collections/"+colID,
		map[string]any{"cover_asset_id": uuid.New().String()})
	if rrMissing.Code != rrHidden.Code {
		t.Errorf("a nonexistent asset gave %d and a withheld one gave %d — the two must be "+
			"indistinguishable, or the difference tells the caller the hidden id exists",
			rrMissing.Code, rrHidden.Code)
	}

	// Nothing was written by either refusal.
	var stored *string
	if err := pool.QueryRow(context.Background(),
		`SELECT cover_asset_id::TEXT FROM collections WHERE id = $1`, colID).Scan(&stored); err != nil {
		t.Fatalf("read back cover_asset_id: %v", err)
	}
	if stored != nil {
		t.Errorf("cover_asset_id persisted as %q after a refused write, want NULL", *stored)
	}
}

// TestCollectionCover_DeletedAssetRevertsToMosaic drives the FK's
// ON DELETE SET NULL. Hard-deleting the chosen asset must leave the
// collection on its derived mosaic rather than pointing at a row that
// is gone.
//
// RESTRICT would instead make one collection's curation choice block an
// unrelated asset's deletion, which is why the constraint is worth a
// test of its own rather than being assumed from the DDL.
func TestCollectionCover_DeletedAssetRevertsToMosaic(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_cover_fk", "visibility": "private"})

	member := ccRenderableAsset(t, pool, ccOwner, "ct_fk_member", "public")
	ccPinAssetViaPost(t, pool, colID, member, time.Now().Add(-time.Hour))
	chosen := ccRenderableAsset(t, pool, ccOwner, "ct_fk_chosen", "public")
	ccSetCover(t, router, colID, chosen)

	if _, err := pool.Exec(context.Background(),
		`DELETE FROM assets WHERE id = $1`, chosen); err != nil {
		t.Fatalf("hard-delete the chosen cover asset: %v — ON DELETE SET NULL should "+
			"allow this; RESTRICT would fail here", err)
	}
	var stored *string
	if err := pool.QueryRow(context.Background(),
		`SELECT cover_asset_id::TEXT FROM collections WHERE id = $1`, colID).Scan(&stored); err != nil {
		t.Fatalf("read back cover_asset_id: %v", err)
	}
	if stored != nil {
		t.Errorf("cover_asset_id = %q after its asset was deleted, want NULL", *stored)
	}
	if got := ccCovers(t, router, colID); len(got) != 1 || got[0] != member {
		t.Errorf("covers = %v after the chosen asset was deleted, want the derived "+
			"mosaic [%s]", got, member)
	}
}

// TestCollectionCover_SoftDeletedOverrideFallsBackButKeepsThePointer —
// the other deletion. A SOFT-deleted asset is dropped by the read path's
// `deleted_at IS NULL` conjunct, so the mosaic answers; but the pointer
// itself stays put, so restoring the asset restores the cover.
//
// The FK cannot express this (soft-delete is an UPDATE), which is
// exactly why it is worth pinning: the two deletions take different
// doors to the same fallback.
func TestCollectionCover_SoftDeletedOverrideFallsBackButKeepsThePointer(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_cover_soft", "visibility": "private"})

	member := ccRenderableAsset(t, pool, ccOwner, "ct_soft_member", "public")
	ccPinAssetViaPost(t, pool, colID, member, time.Now().Add(-time.Hour))
	chosen := ccRenderableAsset(t, pool, ccOwner, "ct_soft_chosen", "public")
	ccSetCover(t, router, colID, chosen)

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, chosen); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	if got := ccCovers(t, router, colID); len(got) != 1 || got[0] != member {
		t.Errorf("covers = %v while the chosen cover is soft-deleted, want the mosaic [%s]",
			got, member)
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = NULL WHERE id = $1`, chosen); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := ccCovers(t, router, colID); len(got) != 1 || got[0] != chosen {
		t.Errorf("covers = %v after restoring the chosen asset, want [%s] — the pointer "+
			"survives a soft delete, so the cover must come back", got, chosen)
	}
}

// TestCollectionCover_OverrideWithoutAColRenditionFallsBack — the write
// gate deliberately does NOT require a `col` variant, because renditions
// are produced asynchronously and refusing a just-uploaded asset would
// be an error the curator cannot act on. The read path carries that
// slack: until the rendition exists, the mosaic answers.
//
// Without this, "accepted at write, invisible at read" would be an
// accepted-but-inert setting with nothing asserting the interim
// behaviour is a usable tile rather than a blank one.
func TestCollectionCover_OverrideWithoutAColRenditionFallsBack(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_cover_norend", "visibility": "private"})

	member := ccRenderableAsset(t, pool, ccOwner, "ct_norend_member", "public")
	ccPinAssetViaPost(t, pool, colID, member, time.Now().Add(-time.Hour))

	pending := ccRenderableAsset(t, pool, ccOwner, "ct_norend_pending", "public")
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM storage_variants WHERE variant_key = 'col' AND object_hash =
		     (SELECT file_hash FROM assets WHERE id = $1)`, pending); err != nil {
		t.Fatalf("drop the col variant: %v", err)
	}

	// Accepted: the picture plane says yes, and the missing rendition is
	// a fact about right now, not about this caller's rights.
	ccSetCover(t, router, colID, pending)
	if got := ccCovers(t, router, colID); len(got) != 1 || got[0] != member {
		t.Errorf("covers = %v with a cover that has no `col` rendition yet, want the "+
			"mosaic [%s] — CollectionCover promises the rendition exists", got, member)
	}
}

// ---------------------------------------------------------------------------
// #1147 — the mature axis on the derived picture
// ---------------------------------------------------------------------------
//
// Every test above this line asks "may this caller SEE this asset". The
// mature axis asks a different question — "has this caller OPTED IN" —
// and ADR 0090 §1 is explicit that the two are independent in both
// directions. A PUBLIC asset can be mature, which is what makes this a
// leak rather than a rounding error: the fixtures below are public,
// readable by anybody, and withheld from a disqualified viewer on the
// mature axis alone.
//
// Every list surface was gated by #1117. The derived-PICTURE surfaces
// were not, so a mature asset pinned into a public collection rendered
// its real `col` rendition inside the cover mosaic on the SAME /search
// response that correctly dropped the asset's own hit — a full
// thumbnail, stronger than the thumbhash #1066 withheld.
//
// Every test here is a PAIR. The withheld leg alone passes on a composer
// that shows the mosaic to nobody, which is why the qualified leg is not
// decoration: it is the half that proves the conjunct is the mature rule
// and not an outage.

// ccMarkMature flips the one bit under test on an already-planted asset.
//
// A separate step rather than a parameter on ccRenderableAsset, matching
// that helper's own note about `sensitivity` being its only knob: a
// fixture that differs from its control in exactly one column is what
// makes the pair's conclusion about the mature axis and nothing else.
//
// It also exercises the real derivation. `posts.mature` is maintained by
// trigger from the asset, so marking an asset that a post carries is how
// the post-half fixtures below get their flag — never by writing
// `posts.mature` directly, which would test a value the product does not
// produce.
func ccMarkMature(t *testing.T, pool *pgxpool.Pool, assetID string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = TRUE WHERE id = $1`, assetID)
	if err != nil {
		t.Fatalf("mark asset %s mature: %v", assetID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("mark asset %s mature: %d rows affected, want 1 — the fixture is not "+
			"the row the assertion is about", assetID, tag.RowsAffected())
	}
}

// ccQualified is the viewer who has cleared all three of ADR 0090 §2's
// conjuncts. Spelled out at the call site rather than hidden behind a
// helper because a test that drops one of the three still reads as
// "qualified" and would quietly assert the withheld behaviour twice.
var ccQualified = visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}

// ccMatureRouter wraps makeRouter's router in the one piece of middleware
// the production stack runs and the test harness does not: the resolved
// mature viewer on the request context.
//
// It wraps rather than reaching inside makeRouter because the ordering is
// the thing being reproduced — visibility.WithMatureViewer runs AFTER
// identity resolution in http/server.go, and a helper that set it first
// would be testing an arrangement the server never builds.
//
// Note what the UNWRAPPED router therefore is: a caller with no viewer on
// the context at all, which visibility.MatureFromContext answers as the
// DISQUALIFIED viewer. That is deliberate and is why every test above
// this section keeps working unchanged — their fixtures are non-mature,
// and MatureItemVisible's first branch says a non-mature item is visible
// on this axis to everybody.
func ccMatureRouter(t *testing.T, pool *pgxpool.Pool, userRef int64, v visibility.MatureViewer) chi.Router {
	t.Helper()
	inner, _ := makeRouter(t, pool, userRef /*admin=*/, false)
	outer := chi.NewRouter()
	outer.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(visibility.WithMatureViewer(r.Context(), v)))
		})
	})
	outer.Mount("/", inner)
	return outer
}

// TestCollectionCovers_MatureMemberWithheldFromDisqualifiedViewer is
// #1147 as reported, as a pair.
//
// The mature member is PUBLIC and owned by a stranger. Public, so no
// sensitivity conjunct can be what hides it; a stranger's, so the owner
// exemption cannot be what shows it. The only thing separating the two
// legs is the reader's opt-in.
//
// The assertion is on the SERVED PAYLOAD, for the reason
// TestCollectionCovers_WithheldMemberContributesNothing states: a row
// count cannot tell a withheld tile from a shipped id with a flag on it,
// and a cover id IS the picture — the client renders
// /assets/{id}/variants/col from it with nothing further to ask.
func TestCollectionCovers_MatureMemberWithheldFromDisqualifiedViewer(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_mature", "visibility": "public",
	})

	base := time.Now().Add(-time.Hour)
	adult := ccRenderableAsset(t, pool, ccStranger, "ct_mat_adult", "public")
	ccMarkMature(t, pool, adult)
	plain := ccRenderableAsset(t, pool, ccStranger, "ct_mat_plain", "public")

	ccPinAssetViaPost(t, pool, colID, adult, base)
	ccPinAssetViaPost(t, pool, colID, plain, base.Add(time.Minute))

	// LEAK ARM. A reader who has not opted in, on a collection anyone may
	// read, holding a member anyone may read.
	out := ccCovers(t, ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{}), colID)
	for _, g := range out {
		if g == adult {
			t.Errorf("the mature member %s rendered in the mosaic of a viewer who never "+
				"opted in (covers=%v). The asset is PUBLIC, so no sensitivity conjunct "+
				"was ever going to catch this — the picture plane has to carry the "+
				"mature axis itself (#1147, ADR 0090 §3)", adult, out)
		}
	}
	if len(out) != 1 || out[0] != plain {
		t.Errorf("disqualified covers = %v, want [%s] — the mature member is ABSENT and "+
			"the rest of the mosaic closes up behind it", out, plain)
	}

	// CONTROL ARM. Same collection, same members, opposite viewer. This
	// is the half that separates "the axis works" from "the mosaic is
	// broken": a composer that withheld from everybody passes the leak
	// arm and fails here.
	in := ccCovers(t, ccMatureRouter(t, pool, ccOwner, ccQualified), colID)
	want := []string{adult, plain}
	if strings.Join(in, ",") != strings.Join(want, ",") {
		t.Errorf("qualified covers = %v, want %v — a reader who opted in, on an instance "+
			"that allows it, gets the whole mosaic in the curator's order", in, want)
	}
}

// TestCollectionCovers_MatureOwnerStillSeesTheirOwnTile pins the
// exemption, which is an asymmetry on purpose (ADR 0090 §2): an artist
// must be able to see their own work.
//
// Without it, an artist who labelled their own piece and never opted in
// would find their own collection's tile gone — access to content they
// own destroyed by a display preference. The exemption lives inside
// MatureFilterSQL's `NULLIF(owner, 0)` comparison, so this also pins that
// the composer passes a real owner ref rather than the anonymous
// sentinel.
func TestCollectionCovers_MatureOwnerStillSeesTheirOwnTile(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_mature_own", "visibility": "private",
	})

	mine := ccRenderableAsset(t, pool, ccOwner, "ct_mat_mine", "public")
	ccMarkMature(t, pool, mine)
	ccPinAssetViaPost(t, pool, colID, mine, time.Now().Add(-time.Hour))

	got := ccCovers(t, ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{}), colID)
	if len(got) != 1 || got[0] != mine {
		t.Errorf("covers = %v, want [%s] — the OWNER of a mature asset sees it whether or "+
			"not they opted in, and whether or not the instance allows it. Switching a "+
			"display preference must not take an artist's own work away from them", got, mine)
	}
}

// TestCollectionCovers_MatureOverrideFallsBackToMosaic is ADR 0088's
// fallback obligation on the new axis, and it is the case that would
// silently not work.
//
// The curator pins their OWN mature asset as the chosen cover — allowed,
// by the owner exemption. A disqualified reader must get the DERIVED
// MOSAIC: not the mature asset (a leak) and not an empty array (a blank
// tile, which is #1026's crowding defect arriving through a third door).
//
// An implementation that spliced the conjunct into the `renderable` CTE
// only — which is what the issue literally asked for — passes every
// other test in this section and leaks here.
func TestCollectionCovers_MatureOverrideFallsBackToMosaic(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_cover_mature_override", "visibility": "public",
	})

	// A member everyone may picture, so "fell back" is distinguishable
	// from "returned nothing".
	open := ccRenderableAsset(t, pool, ccOwner, "ct_mo_open", "public")
	ccPinAssetViaPost(t, pool, colID, open, time.Now().Add(-time.Hour))

	adult := ccRenderableAsset(t, pool, ccOwner, "ct_mo_adult", "public")
	ccMarkMature(t, pool, adult)
	ccSetCover(t, ownerRouter, colID, adult)

	// The curator's own view: the exemption applies, so the override is
	// the sole tile. Without this leg an implementation that dropped
	// every override on the floor passes the one below.
	if got := ccCovers(t, ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{}), colID); //
	len(got) != 1 || got[0] != adult {
		t.Fatalf("the curator's own view = %v, want their chosen cover [%s] — they own it",
			got, adult)
	}

	got := ccCovers(t, ccMatureRouter(t, pool, ccStranger, visibility.MatureViewer{}), colID)
	for _, g := range got {
		if g == adult {
			t.Fatalf("the mature chosen cover %s reached a disqualified viewer (covers=%v). "+
				"The pointer is a SECOND door to one asset's pixels and it must clear the "+
				"same bar as a member", adult, got)
		}
	}
	if len(got) != 1 || got[0] != open {
		t.Errorf("disqualified covers = %v, want the derived mosaic [%s]. An empty array "+
			"here is a BLANK TILE — ADR 0088's fallback obligation says a withheld cover "+
			"falls back to the mosaic, never to nothing", got, open)
	}

	// And the control: the same stranger, opted in, gets the override.
	if in := ccCovers(t, ccMatureRouter(t, pool, ccStranger, ccQualified), colID); //
	len(in) != 1 || in[0] != adult {
		t.Errorf("qualified stranger covers = %v, want the chosen cover [%s]", in, adult)
	}
}

// TestCollectionCovers_MatureOnlyCollectionYieldsNoTileNotABlank is the
// other half of the fallback obligation: a collection whose ONLY
// renderable member is mature.
//
// There is nothing to fall back TO, and the right answer is the one a
// collection with no renderable member already gets — an empty array,
// which every surface paints as its own "no cover". What must NOT happen
// is a tile slot occupied by an id the caller cannot render, which is
// the shape the deleted client card consumed and the shape a row-count
// assertion cannot see.
func TestCollectionCovers_MatureOnlyCollectionYieldsNoTileNotABlank(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_mature_only", "visibility": "public",
	})

	adult := ccRenderableAsset(t, pool, ccStranger, "ct_mono_adult", "public")
	ccMarkMature(t, pool, adult)
	ccPinAssetViaPost(t, pool, colID, adult, time.Now().Add(-time.Hour))

	got := ccCovers(t, ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{}), colID)
	if len(got) != 0 {
		t.Errorf("covers = %v, want an EMPTY array. The one renderable member is mature "+
			"and this viewer did not opt in, so the honest answer is the same one a "+
			"collection with no renderable member gives — never a slot holding an id "+
			"the caller cannot paint", got)
	}

	if in := ccCovers(t, ccMatureRouter(t, pool, ccOwner, ccQualified), colID); //
	len(in) != 1 || in[0] != adult {
		t.Errorf("qualified covers = %v, want [%s] — the empty answer above must be the "+
			"mature axis and not an empty collection", in, adult)
	}
}

// ccAddPostMember makes an asset a real MEMBER of a post, which is what
// fires the `post_assets` trigger that maintains `posts.mature`.
//
// Written directly rather than through the add endpoint because the
// derivation is the property under test: a fixture that set
// `posts.mature` by hand would assert against a value the product never
// produces, and would go on passing if the trigger were dropped.
func ccAddPostMember(t *testing.T, pool *pgxpool.Pool, postID, assetID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_assets (post_id, asset_id) VALUES ($1, $2)`,
		postID, assetID); err != nil {
		t.Fatalf("add post member: %v", err)
	}
}

// ccAssertPostMature reads the DERIVED flag back, so a test whose real
// subject is the tile cannot pass or fail for the wrong reason.
func ccAssertPostMature(t *testing.T, pool *pgxpool.Pool, postID string, want bool) {
	t.Helper()
	var got bool
	if err := pool.QueryRow(context.Background(),
		`SELECT mature FROM posts WHERE id = $1`, postID).Scan(&got); err != nil {
		t.Fatalf("read back the derived post flag: %v", err)
	}
	if got != want {
		t.Fatalf("posts.mature = %v for post %s, want %v — the fixture is not the state "+
			"the assertion below is about, so everything after this line would be "+
			"testing something the product does not produce", got, postID, want)
	}
}

// TestCollectionCovers_MaturePostContributesNoTile covers the POST half,
// which is a different table with a different owner column and a DERIVED
// flag.
//
// # The fixture is the whole test
//
// The post is mature because of a MEMBER, and its cover picture is NOT
// mature. That separation is deliberate and it is what makes this test
// mean anything: with a mature cover, the asset conjunct on the tile
// catches the row and the post conjunct could be deleted with every
// assertion still green. (Confirmed by deleting it — the first draft of
// this fixture proved nothing.)
//
// So this is the case that justifies the post half existing at all: the
// tile's own asset is perfectly ordinary, and the only thing wrong with
// painting it is that it stands in for a MEMBER the viewer cannot see.
// The feed hides that post; /collections/{id}/posts hides that post; a
// mosaic tile for it would put it back on screen.
func TestCollectionCovers_MaturePostContributesNoTile(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_mature_post", "visibility": "public",
	})

	base := time.Now().Add(-time.Hour)
	plain := ccRenderableAsset(t, pool, ccStranger, "ct_mp_plain", "public")
	ccPinPost(t, pool, colID, ccPost(t, pool, ccStranger, "public", plain, ""), base)

	// An ORDINARY cover on a post made mature by a member nobody sees in
	// the mosaic.
	cover := ccRenderableAsset(t, pool, ccStranger, "ct_mp_cover", "public")
	postID := ccPost(t, pool, ccStranger, "public", cover, "")
	member := ccRenderableAsset(t, pool, ccStranger, "ct_mp_adult_member", "public")
	ccAddPostMember(t, pool, postID, member)
	// Marked AFTER the membership exists, so the ASSET trigger is what
	// propagates the flag — the path an operator labelling an
	// already-published asset takes, and the one 00052 added the second
	// trigger for.
	ccMarkMature(t, pool, member)
	ccAssertPostMature(t, pool, postID, true)
	ccPinPost(t, pool, colID, postID, base.Add(time.Minute))

	got := ccCovers(t, ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{}), colID)
	for _, g := range got {
		if g == cover {
			t.Errorf("the mature post's cover %s rendered for a disqualified viewer "+
				"(covers=%v). The cover asset is NOT mature — the post is, and a tile "+
				"is a summary of members this viewer can see", cover, got)
		}
	}
	if len(got) != 1 || got[0] != plain {
		t.Errorf("disqualified covers = %v, want [%s]", got, plain)
	}

	in := ccCovers(t, ccMatureRouter(t, pool, ccOwner, ccQualified), colID)
	want := []string{plain, cover}
	if strings.Join(in, ",") != strings.Join(want, ",") {
		t.Errorf("qualified covers = %v, want %v", in, want)
	}
}

// TestCollectionCovers_MaturePostCoverIsDerived is migration 00054, and
// its subject is the DERIVATION rather than the tile.
//
// 00052 derived `posts.mature` from `post_assets` alone. But a post's
// cover need not be a member — `cover_thumbnail_asset_id` is documented
// as "an optional standalone thumbnail (not a post member)" — so a post
// whose COVER was mature and whose members were not computed `false` and
// sailed through every conjunct on every surface that trusts the column:
// the browse feed, /search's post arm, the featured rail's cover
// lateral, and this mosaic. It was also the first picture each of them
// paints.
//
// The fixture is that exact post: a mature standalone thumbnail, no
// members at all. The flag assertion is the real one; the tile
// assertions below it are the consequence.
func TestCollectionCovers_MaturePostCoverIsDerived(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_covers_mature_thumb", "visibility": "public",
	})

	base := time.Now().Add(-time.Hour)
	plain := ccRenderableAsset(t, pool, ccStranger, "ct_mt_plain", "public")
	ccPinPost(t, pool, colID, ccPost(t, pool, ccStranger, "public", plain, ""), base)

	member := ccRenderableAsset(t, pool, ccStranger, "ct_mt_member", "public")
	thumb := ccRenderableAsset(t, pool, ccStranger, "ct_mt_adult_thumb", "public")
	ccMarkMature(t, pool, thumb)
	// The standalone thumbnail wins over cover_asset_id on a feed card,
	// so it is the picture this post actually shows.
	postID := ccPost(t, pool, ccStranger, "public", member, thumb)
	ccAssertPostMature(t, pool, postID, true)
	ccPinPost(t, pool, colID, postID, base.Add(time.Minute))

	got := ccCovers(t, ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{}), colID)
	for _, g := range got {
		if g == thumb {
			t.Errorf("the mature standalone cover %s rendered for a disqualified viewer "+
				"(covers=%v)", thumb, got)
		}
	}
	if len(got) != 1 || got[0] != plain {
		t.Errorf("disqualified covers = %v, want [%s]", got, plain)
	}

	// The derivation must also come BACK DOWN. An operator who mislabels
	// an asset and corrects it must not leave the post stuck mature —
	// that would be a one-way door, and #1114's whole premise is that the
	// label is editable.
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET mature = FALSE WHERE id = $1`, thumb); err != nil {
		t.Fatalf("un-mark the cover: %v", err)
	}
	ccAssertPostMature(t, pool, postID, false)
	if back := ccCovers(t, ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{}), colID); //
	len(back) != 2 {
		t.Errorf("covers = %v after the label was removed, want both tiles — the "+
			"derivation has to fall as well as rise", back)
	}
}

// TestCollectionCover_WriteRefusesAMatureAssetTheCuratorCannotPicture —
// the write gate mirrors the read, which is the property
// CallerMayPictureAsset exists to hold.
//
// Not because pinning a mature cover would leak: the read path withholds
// it per viewer regardless. It is the existence oracle. A disqualified
// curator cannot see a stranger's mature asset in ANY listing, so a write
// gate that accepted one would confirm by id exactly the fact every
// listing withholds — and the refusal is byte-identical to the one a
// nonexistent id collects, which is what makes it not an oracle.
func TestCollectionCover_WriteRefusesAMatureAssetTheCuratorCannotPicture(t *testing.T) {
	pool := ccSetup(t)
	ownerRouter, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, ownerRouter, map[string]any{
		"name": "ct_cover_mature_write", "visibility": "private",
	})

	adult := ccRenderableAsset(t, pool, ccStranger, "ct_mw_adult", "public")
	ccMarkMature(t, pool, adult)

	disqualified := ccMatureRouter(t, pool, ccOwner, visibility.MatureViewer{})
	rr := patchJSON(t, disqualified, "/collections/"+colID,
		map[string]any{"cover_asset_id": adult})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("pointing at a mature asset as a disqualified curator: status=%d, want "+
			"400 (body=%s)", rr.Code, rr.Body.String())
	}
	// The PERSISTED value, not the response: a 400 whose write went
	// through anyway is not a refusal, and a status assertion cannot
	// tell the difference.
	var stored *string
	if err := pool.QueryRow(context.Background(),
		`SELECT cover_asset_id::TEXT FROM collections WHERE id = $1`, colID).Scan(&stored); err != nil {
		t.Fatalf("read back cover_asset_id: %v", err)
	}
	if stored != nil {
		t.Errorf("cover_asset_id persisted as %q after a refused write, want NULL", *stored)
	}

	// The control: the same curator, opted in, may point at it.
	ccSetCover(t, ccMatureRouter(t, pool, ccOwner, ccQualified), colID, adult)
}
