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

// ccPinAsset / ccPinPost make a membership row at an EXPLICIT added_at,
// which is the axis the mosaic orders on. Written directly rather than
// through the add endpoints because the interleave is the property under
// test and `now()` cannot express it.
func ccPinAsset(t *testing.T, pool *pgxpool.Pool, colID, assetID string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_resources (collection_id, asset_id, sort_order, pinned, added_at)
		 VALUES ($1, $2, 0, TRUE, $3)`, colID, assetID, at); err != nil {
		t.Fatalf("pin asset: %v", err)
	}
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

// TestCollectionCovers_InterleavesBothKinds pins the ordering decision:
// added_at ascending across BOTH membership tables. A composer that
// concatenated (all assets, then all posts) returns the same SET and the
// wrong arrangement, and the arrangement is the curation.
func TestCollectionCovers_InterleavesBothKinds(t *testing.T) {
	pool := ccSetup(t)
	router, _ := makeRouter(t, pool, ccOwner /*admin=*/, false)
	colID := mustCreate(t, router, map[string]any{"name": "ct_covers_interleave", "visibility": "private"})

	base := time.Now().Add(-time.Hour)
	a1 := ccRenderableAsset(t, pool, ccOwner, "ct_il_a1", "public")
	p1 := ccRenderableAsset(t, pool, ccOwner, "ct_il_p1", "public")
	a2 := ccRenderableAsset(t, pool, ccOwner, "ct_il_a2", "public")
	p2 := ccRenderableAsset(t, pool, ccOwner, "ct_il_p2", "public")

	ccPinAsset(t, pool, colID, a1, base)
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", p1, ""), base.Add(1*time.Minute))
	ccPinAsset(t, pool, colID, a2, base.Add(2*time.Minute))
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", p2, ""), base.Add(3*time.Minute))

	got := strings.Join(ccCovers(t, router, colID), ",")
	want := strings.Join([]string{a1, p1, a2, p2}, ",")
	if got != want {
		t.Errorf("covers = %v\nwant   %v\nthe two membership tables are one shelf; "+
			"added_at is the curator's arrangement across both", got, want)
	}
}

// TestCollectionCovers_DeduplicatesOneAssetReachedTwice — an asset can
// be a cover candidate by several routes at once: pinned directly AND
// carried as a post's cover, or shared as the cover of two posts. Seed
// data has plenty of both.
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
	ccPinAsset(t, pool, colID, shared, base)
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", shared, ""), base.Add(time.Minute))
	ccPinPost(t, pool, colID, ccPost(t, pool, ccOwner, "public", shared, ""), base.Add(2*time.Minute))
	ccPinAsset(t, pool, colID, other, base.Add(3*time.Minute))

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

	// Four withheld members at the head, alternating kinds so a
	// per-table bug cannot hide behind the other table.
	hidden := make([]string, 0, 4)
	for i := 0; i < 2; i++ {
		a := ccRenderableAsset(t, pool, ccStranger, "ct_crowd_hidden_asset", "restricted")
		ccPinAsset(t, pool, colID, a, at(2*i))
		hidden = append(hidden, a)

		pc := ccRenderableAsset(t, pool, ccStranger, "ct_crowd_hidden_post_cover", "public")
		ccPinPost(t, pool, colID, ccPost(t, pool, ccStranger, "private", pc, ""), at(2*i+1))
		hidden = append(hidden, pc)
	}

	// Four renderable members behind them.
	want := make([]string, 0, 4)
	for i := 0; i < 2; i++ {
		a := ccRenderableAsset(t, pool, ccOwner, "ct_crowd_ok_asset", "public")
		ccPinAsset(t, pool, colID, a, at(4+2*i))
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

	ccPinAsset(t, pool, colID, open, base)
	ccPinAsset(t, pool, colID, secretAsset, base.Add(time.Minute))
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
	ccPinAsset(t, pool, colID, draft, base)
	ccPinAsset(t, pool, colID, restricted, base.Add(time.Minute))
	ccPinAsset(t, pool, colID, open, base.Add(2*time.Minute))

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
	ccPinAsset(t, pool, colID, secret, base)
	ccPinAsset(t, pool, colID, open, base.Add(time.Minute))

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
	ccPinAsset(t, pool, colID, m1, base)
	ccPinAsset(t, pool, colID, m2, base.Add(time.Minute))
	ccPinAsset(t, pool, colID, m3, base.Add(2*time.Minute))

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
	ccPinAsset(t, pool, colID, member, time.Now().Add(-time.Hour))
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
	ccPinAsset(t, pool, colID, open, time.Now().Add(-time.Hour))

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
	ccPinAsset(t, pool, colID, member, time.Now().Add(-time.Hour))
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
	ccPinAsset(t, pool, colID, member, time.Now().Add(-time.Hour))
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
	ccPinAsset(t, pool, colID, member, time.Now().Add(-time.Hour))

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
