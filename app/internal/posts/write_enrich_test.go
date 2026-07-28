// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #655 — a written post must be the same post a read returns.
//
// CreatePost and UpdatePost re-read through fetchFullPost and returned
// that verbatim, without the enrichPreview pass every read path makes.
// Four fields accumulated in the hole — preview_available (#471),
// ladder_available (#610), pixel_width / pixel_height (#640) and
// thumbhash (#648) — because none of them can be carried by the cached
// ListPostAssets row: two are per-caller and two are written outside it.
//
// The test is written as an EQUIVALENCE rather than a field checklist on
// purpose. A checklist pins the four fields that exist today and says
// nothing about the fifth; "the create response equals the read
// response" holds for every field enrichPreview will ever derive, so the
// next one cannot go missing quietly the way these did.
//
// Skips without AA_DB_PASSWORD. Shares previewPool / seedPreviewAsset /
// ctxAs with preview_enrich_test.go.

package posts

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// weLadder is the "operator-configured" ladder for this test. Two
// invented rung keys, not the install defaults — ladder_available is
// computed against whatever the operator configured (#610), and a test
// that used the real default names would pass on a hardcoded rung list
// too.
var weLadder = []string{"we_small", "we_large"}

// seedLadderVariants gives an asset's object every rung in weLadder, so
// LadderSatisfiedSQL reports true for it.
func seedLadderVariants(t *testing.T, h *Handler, assetID uuid.UUID) {
	t.Helper()
	hash := peHash(assetID)
	for _, key := range weLadder {
		if _, err := h.Pool.Exec(context.Background(),
			`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1,$2,1)
			 ON CONFLICT (object_hash, variant_key) DO NOTHING`, hash, key); err != nil {
			t.Fatalf("seed ladder variant %s: %v", key, err)
		}
	}
}

// seedPixelDims writes pixel_width / pixel_height into asset_field_value
// — where the EXIF pass puts them, and where pixeldims reads them from
// (they are NOT columns on `assets`).
func seedPixelDims(t *testing.T, h *Handler, assetID uuid.UUID, w, hgt int) {
	t.Helper()
	for code, v := range map[string]int{"pixel_width": w, "pixel_height": hgt} {
		if _, err := h.Pool.Exec(context.Background(),
			`INSERT INTO asset_field_value (asset_id, field_id, value_num, set_by)
			 SELECT $1, fd.id, $2::double precision, 'exif'
			   FROM field_definition fd WHERE fd.code = $3
			 ON CONFLICT (asset_id, field_id) DO UPDATE SET value_num = EXCLUDED.value_num`,
			assetID, v, code); err != nil {
			t.Fatalf("seed %s: %v", code, err)
		}
	}
	t.Cleanup(func() {
		_, _ = h.Pool.Exec(context.Background(),
			`DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
	})
}

// weEnriched is the slice of an asset payload that only enrichPreview
// can fill. Comparing this struct is the equivalence assertion.
type weEnriched struct {
	PreviewAvailable bool
	LadderAvailable  bool
	PixelWidth       *int32
	PixelHeight      *int32
	Thumbhash        *string
}

func (e weEnriched) String() string {
	return fmt.Sprintf("preview_available=%t ladder_available=%t pixel_width=%s pixel_height=%s thumbhash=%s",
		e.PreviewAvailable, e.LadderAvailable,
		intPtrStr(e.PixelWidth), intPtrStr(e.PixelHeight), strPtrStr(e.Thumbhash))
}

func intPtrStr(p *int32) string {
	if p == nil {
		return "<null>"
	}
	return strconv.Itoa(int(*p))
}

func strPtrStr(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// weFields extracts the enriched slice for one member of a post.
func weFields(t *testing.T, p *openapi.Post, assetID uuid.UUID) weEnriched {
	t.Helper()
	for _, m := range p.Members {
		if uuid.UUID(m.Asset.Id) != assetID {
			continue
		}
		return weEnriched{
			PreviewAvailable: m.Asset.PreviewAvailable,
			LadderAvailable:  m.Asset.LadderAvailable,
			PixelWidth:       m.Asset.PixelWidth,
			PixelHeight:      m.Asset.PixelHeight,
			Thumbhash:        m.Asset.Thumbhash,
		}
	}
	t.Fatalf("asset %v is not a member of post %v", assetID, p.Id)
	return weEnriched{}
}

func sameEnriched(a, b weEnriched) bool {
	eqInt := func(x, y *int32) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return *x == *y
	}
	eqStr := func(x, y *string) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return *x == *y
	}
	return a.PreviewAvailable == b.PreviewAvailable &&
		a.LadderAvailable == b.LadderAvailable &&
		eqInt(a.PixelWidth, b.PixelWidth) &&
		eqInt(a.PixelHeight, b.PixelHeight) &&
		eqStr(a.Thumbhash, b.Thumbhash)
}

// wireWriteHandler builds the handler the way boot does — ladder reader
// + activities writer + a baseURL resolver, all of which the write paths
// dereference.
func wireWriteHandler(t *testing.T) *Handler {
	t.Helper()
	pool := previewPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, logger, cache.NewRegistry(pool, logger))
	h.SetPreviewLadder(func(ctx context.Context) []string { return weLadder })
	h.SetActivitiesWriter(activities.NewWriter(pool, logger, nil),
		func(ctx context.Context) string { return "https://test.example" })
	return h
}

// TestCreatePostResponseMatchesRead is the #655 regression. It FAILS on
// the pre-fix code with preview_available=false / ladder_available=false
// / pixel_width=<null> / thumbhash=<null> on the create side against a
// fully-populated read side.
func TestCreatePostResponseMatchesRead(t *testing.T) {
	h := wireWriteHandler(t)

	assetID := seedPreviewAsset(t, h.Pool, "public", true) // + `col`
	seedLadderVariants(t, h, assetID)
	raw := []byte{0x10, 0x20, 0x30, 0x40}
	setThumbhash(t, h.Pool, assetID, raw)
	seedPixelDims(t, h, assetID, 1920, 1080)

	ctx := ctxAs(pePostOwner)
	title := "we create"
	resp, err := h.CreatePost(ctx, openapi.CreatePostRequestObject{
		Body: &openapi.PostCreate{
			Title:   &title,
			Members: []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(assetID)}},
		},
	})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	created, ok := resp.(openapi.CreatePost201JSONResponse)
	if !ok {
		t.Fatalf("CreatePost returned %T, want 201", resp)
	}
	post := openapi.Post(created)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = h.Pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, uuid.UUID(post.Id))
		_, _ = h.Pool.Exec(c, `DELETE FROM posts WHERE id = $1`, uuid.UUID(post.Id))
	})

	// The read the frontend does immediately afterwards.
	readResp, err := h.GetPost(ctx, openapi.GetPostRequestObject{Id: post.Id})
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	read200, ok := readResp.(openapi.GetPost200JSONResponse)
	if !ok {
		t.Fatalf("GetPost returned %T, want 200", readResp)
	}
	readPost := openapi.Post(read200)

	// Precondition: the READ side must actually carry values, or the
	// equivalence would pass on two empty shapes. This is the
	// "can production reach this fixture?" guard.
	want := weFields(t, &readPost, assetID)
	if !want.PreviewAvailable || !want.LadderAvailable ||
		want.PixelWidth == nil || want.PixelHeight == nil || want.Thumbhash == nil {
		t.Fatalf("precondition: the read path itself returned an unenriched member (%s) — "+
			"the fixture is wrong, not the write path", want)
	}
	if want.Thumbhash != nil && *want.Thumbhash != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("precondition: read thumbhash = %q, want base64 of the seeded bytes", *want.Thumbhash)
	}

	got := weFields(t, &post, assetID)
	if !sameEnriched(got, want) {
		t.Errorf("POST /posts response ≠ GET /posts/{id} response (#655)\n  create: %s\n  read:   %s",
			got, want)
	}
}

// TestUpdatePostResponseMatchesRead is the same equivalence on PATCH.
func TestUpdatePostResponseMatchesRead(t *testing.T) {
	h := wireWriteHandler(t)

	assetID := seedPreviewAsset(t, h.Pool, "public", true)
	seedLadderVariants(t, h, assetID)
	setThumbhash(t, h.Pool, assetID, []byte{0xaa, 0xbb, 0xcc, 0xdd})
	seedPixelDims(t, h, assetID, 800, 600)
	postID := seedPreviewPost(t, h.Pool, assetID)

	ctx := ctxAs(pePostOwner)
	newTitle := "we update"
	resp, err := h.UpdatePost(ctx, openapi.UpdatePostRequestObject{
		Id:   openapi_types.UUID(postID),
		Body: &openapi.PostUpdate{Title: &newTitle},
	})
	if err != nil {
		t.Fatalf("UpdatePost: %v", err)
	}
	updated, ok := resp.(openapi.UpdatePost200JSONResponse)
	if !ok {
		t.Fatalf("UpdatePost returned %T, want 200", resp)
	}
	patched := openapi.Post(updated)

	readResp, err := h.GetPost(ctx, openapi.GetPostRequestObject{Id: openapi_types.UUID(postID)})
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	read200, ok := readResp.(openapi.GetPost200JSONResponse)
	if !ok {
		t.Fatalf("GetPost returned %T, want 200", readResp)
	}
	readPost := openapi.Post(read200)

	want := weFields(t, &readPost, assetID)
	if !want.PreviewAvailable || !want.LadderAvailable ||
		want.PixelWidth == nil || want.PixelHeight == nil || want.Thumbhash == nil {
		t.Fatalf("precondition: the read path returned an unenriched member (%s)", want)
	}
	got := weFields(t, &patched, assetID)
	if !sameEnriched(got, want) {
		t.Errorf("PATCH /posts/{id} response ≠ GET /posts/{id} response (#655)\n  patch: %s\n  read:  %s",
			got, want)
	}
}

// TestWritePathDoesNotPoisonCache is the cache half of #655.
//
// enrichPreview mutates a FRESH copy of Members and leaves the cached
// backing array alone (see its doc comment); the write paths populate
// that cache via fetchFullPost. If a write ever enriched IN PLACE, the
// cache would hold one caller's per-caller flags — so this reads the
// post back as a DIFFERENT caller and pins that the flags are re-derived
// rather than served from whatever the writer left behind.
func TestWritePathDoesNotPoisonCache(t *testing.T) {
	h := wireWriteHandler(t)

	// Restricted: only the owner may see a preview. If a create by the
	// owner seeded the cache with an enriched shape, the stranger's read
	// would inherit `true`.
	restricted := seedPreviewAsset(t, h.Pool, "restricted", true)
	seedLadderVariants(t, h, restricted)
	setThumbhash(t, h.Pool, restricted, []byte{0x01, 0x02})
	seedPixelDims(t, h, restricted, 640, 480)

	title := "we cache"
	resp, err := h.CreatePost(ctxAs(pePostOwner), openapi.CreatePostRequestObject{
		Body: &openapi.PostCreate{
			Title:   &title,
			Members: []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(restricted)}},
		},
	})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	created := openapi.Post(resp.(openapi.CreatePost201JSONResponse))
	t.Cleanup(func() {
		c := context.Background()
		_, _ = h.Pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, uuid.UUID(created.Id))
		_, _ = h.Pool.Exec(c, `DELETE FROM posts WHERE id = $1`, uuid.UUID(created.Id))
	})

	if f := weFields(t, &created, restricted); !f.PreviewAvailable {
		t.Fatalf("precondition: the owner's create should see the restricted member (%s)", f)
	}

	// The cached entry must still carry the baked-in false.
	cached, ok := h.byID.Get(uuid.UUID(created.Id).String())
	if !ok {
		t.Fatal("post was not cached; cannot verify the write path left it clean")
	}
	if f := weFields(t, &cached, restricted); f.PreviewAvailable || f.LadderAvailable {
		t.Errorf("POISONED: the create path wrote per-caller flags into the shared cache (%s)", f)
	}

	// The owner reads it back — served from that cache, and still
	// enriched. (#655 acceptance 4: no unenriched cached shape.)
	ownerRead, err := h.GetPost(ctxAs(pePostOwner), openapi.GetPostRequestObject{Id: created.Id})
	if err != nil {
		t.Fatalf("GetPost owner: %v", err)
	}
	ownerPost := openapi.Post(ownerRead.(openapi.GetPost200JSONResponse))
	if f := weFields(t, &ownerPost, restricted); !f.PreviewAvailable || !f.LadderAvailable ||
		f.PixelWidth == nil || f.Thumbhash == nil {
		t.Errorf("read after create returned a cached unenriched shape (%s)", f)
	}

	// A stranger must NOT inherit the author's readability.
	strangerRead, err := h.GetPost(ctxAs(peStranger), openapi.GetPostRequestObject{Id: created.Id})
	if err != nil {
		t.Fatalf("GetPost stranger: %v", err)
	}
	strangerPost := openapi.Post(strangerRead.(openapi.GetPost200JSONResponse))
	if f := weFields(t, &strangerPost, restricted); f.PreviewAvailable {
		t.Errorf("LEAK: a stranger inherited the author's preview_available from the write path (%s)", f)
	}
}
