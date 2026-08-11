// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #935 — an asset write invalidates the caches OTHER packages keep.
//
// # The shape of this class of bug
//
// An asset write touches the assets row and nothing else. But other
// domains cache answers DERIVED from that row, keyed on something that
// is not the asset:
//
//	posts/             the whole rendered post, joined asset payloads
//	                   included, keyed on the post (#920)
//	iiif/presentation  the manifest, which carries the asset's title and
//	                   description, keyed on its own domain (#935)
//	subtitles/         the track list, keyed on the asset but read
//	                   through without ever consulting it (#935)
//
// Nothing in Postgres can tell an in-process LRU that its answer went
// out of date, so the stale value survives until the process restarts.
// The restart is what makes this so easy to miss in manual testing, and
// it is why every test in this file asserts the stale value FIRST and
// then the fresh one THROUGH THE SAME HANDLER INSTANCE. A test that
// rebuilt the handler between the write and the read would pass against
// completely unwired code.
//
// # What was actually unwired
//
//   - UpdateAsset invalidated NOTHING. Not the manifest, and not even
//     the posts cache #920 had already wired into delete and restore.
//     Renaming an asset left every post holding it, and its manifest,
//     serving the old title. Reachable by any owner clicking Save.
//   - presentation.InvalidateAssetOn had zero non-test callers.
//   - subtitles.InvalidateForAsset had zero non-test callers, while its
//     package doc asserted that "the assets/ HardDelete path explicitly
//     calls" it. There is no HardDelete in assets/ at all — the hard
//     delete is softdelete's retention GC — and the call that does
//     exist in assets/ is posts.InvalidateForAsset, a different
//     function with the same name.
//
// Skips without AA_DB_PASSWORD.

package assets_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/iiif/presentation"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
)

// cfWorld is one process: ONE cache.Registry shared by every handler,
// exactly as the composition root builds it. Sharing the registry is
// the whole point — a per-handler registry would make cross-package
// invalidation vacuously succeed.
type cfWorld struct {
	t         *testing.T
	pool      *pgxpool.Pool
	registry  *cache.Registry
	assets    *assets.Handler
	posts     *posts.Handler
	subtitles *subtitles.Handler
	manifests *presentation.Cache
	iiif      http.Handler
	owner     int64
}

func newCFWorld(t *testing.T) *cfWorld {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := cache.NewRegistry(pool, logger)

	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	ah := assets.NewHandler(pool, storage.NewService(backend, pool), logger, nil, registry, nil)
	manifests := presentation.NewCache(registry)
	// The boot wire, reproduced. Stated plainly because it is this
	// file's one real limitation: these tests exercise the handler's
	// behaviour GIVEN the seam is wired, so deleting the
	// SetManifestCache call from internal/http/api.go would not turn
	// any of them red. The same is true of the OnAssetsHardDeleted
	// fan-out below. Both are single lines at the composition root and
	// are cited in the #935 close-out for that reason.
	ah.SetManifestCache(manifests)

	iiifH := &presentation.Handler{
		Loader:  presentation.NewLoader(pool),
		Builder: presentation.NewBuilder(presentation.BuilderConfig{SiteBaseURL: "https://test.example"}),
		Cache:   manifests,
		Logger:  logger,
	}
	r := chi.NewRouter()
	iiifH.Mount(r)

	var owner int64
	name := "cf-owner-" + uuid.NewString()[:8]
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`, name).Scan(&owner); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, owner)
	})

	return &cfWorld{
		t:         t,
		pool:      pool,
		registry:  registry,
		assets:    ah,
		posts:     posts.NewHandler(pool, logger, registry),
		subtitles: subtitles.NewHandler(pool, registry, logger),
		manifests: manifests,
		iiif:      r,
		owner:     owner,
	}
}

// asset seeds a public, active, ready asset owned by the fixture's
// owner — the shape an anonymous manifest request can actually reach.
func (w *cfWorld) asset(title string) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if _, err := w.pool.Exec(context.Background(),
		`INSERT INTO assets (id, owner_user_ref, title, description, asset_type, status, sensitivity, processing_status)
		 VALUES ($1,$2,$3,'',(SELECT MIN(ref) FROM asset_types),'active','public','ready')`,
		id, w.owner, title); err != nil {
		w.t.Fatalf("seed asset: %v", err)
	}
	w.t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id) })
	return id
}

// post seeds a post holding the given assets.
func (w *cfWorld) post(members ...uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if _, err := w.pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1,$2,'cache-fanout post','public')`,
		id, w.owner); err != nil {
		w.t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := w.pool.Exec(context.Background(),
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`, id, m, i); err != nil {
			w.t.Fatalf("seed membership: %v", err)
		}
	}
	w.t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM post_assets WHERE post_id=$1`, id)
		_, _ = w.pool.Exec(context.Background(), `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

// patchTitle drives the REAL PATCH /assets/{id} as the owner.
func (w *cfWorld) patchTitle(assetID uuid.UUID, title string) {
	w.t.Helper()
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: w.owner, AuthMethod: "session"})
	resp, err := w.assets.UpdateAsset(ctx, openapi.UpdateAssetRequestObject{
		Id:   openapi_types.UUID(assetID),
		Body: &openapi.AssetUpdate{Title: &title},
	})
	if err != nil {
		w.t.Fatalf("UpdateAsset: %v", err)
	}
	if _, ok := resp.(openapi.UpdateAsset200JSONResponse); !ok {
		w.t.Fatalf("UpdateAsset returned %T, want 200", resp)
	}
}

// postMemberTitle reads the title the POST serves for one of its
// members, through the posts handler's cached read path.
func (w *cfWorld) postMemberTitle(postID, assetID uuid.UUID) string {
	w.t.Helper()
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: w.owner, AuthMethod: "session"})
	resp, err := w.posts.GetPost(ctx, openapi.GetPostRequestObject{Id: openapi_types.UUID(postID)})
	if err != nil {
		w.t.Fatalf("GetPost: %v", err)
	}
	got, ok := resp.(openapi.GetPost200JSONResponse)
	if !ok {
		w.t.Fatalf("GetPost returned %T, want 200", resp)
	}
	for _, m := range got.Members {
		if uuid.UUID(m.AssetId) != assetID {
			continue
		}
		if m.Asset == nil || m.Asset.Title == nil {
			w.t.Fatalf("member %v carries no asset title", assetID)
		}
		return *m.Asset.Title
	}
	w.t.Fatalf("asset %v is not a member of post %v", assetID, postID)
	return ""
}

// manifestLabel does a real anonymous GET of the IIIF manifest through
// the mounted route, so the cache key comes from the read path itself
// rather than from a test's guess at it.
func (w *cfWorld) manifestLabel(assetID uuid.UUID) string {
	w.t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/iiif/3/asset/"+assetID.String()+"/manifest.json", nil)
	w.iiif.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("manifest GET returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Label map[string][]string `json:"label"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		w.t.Fatalf("decode manifest: %v", err)
	}
	for _, vals := range body.Label {
		if len(vals) > 0 {
			return vals[0]
		}
	}
	w.t.Fatalf("manifest carries no label: %s", rec.Body.String())
	return ""
}

// ---------------------------------------------------------------------------
// PATCH — the path that invalidated nothing at all
// ---------------------------------------------------------------------------

// TestUpdateAsset_EvictsHoldingPosts is the live staleness bug neither
// #935 nor #920 names: PATCH /assets/{id} had no invalidation call of
// any kind, so #920's fix covered delete and restore and left the edit
// path — the one users actually hit — serving the old title forever.
func TestUpdateAsset_EvictsHoldingPosts(t *testing.T) {
	w := newCFWorld(t)

	assetID := w.asset("title before the edit")
	postID := w.post(assetID)

	// Populate. This is the read the bug served stale.
	if got := w.postMemberTitle(postID, assetID); got != "title before the edit" {
		t.Fatalf("post serves %q before the edit, want the seeded title — fixture is wrong", got)
	}

	w.patchTitle(assetID, "title after the edit")

	// No restart, no new handler, no new registry: the same instance
	// that answered above answers again.
	if got := w.postMemberTitle(postID, assetID); got != "title after the edit" {
		t.Errorf("post still serves %q after PATCH, want %q — UpdateAsset is not "+
			"evicting the posts holding this asset, and only a process restart "+
			"would clear it", got, "title after the edit")
	}
}

// TestUpdateAsset_EvictsManifest is the same defect on the IIIF side.
// presentation.LoadAsset selects the asset's title straight into the
// manifest label, and the built manifest is cached under its own
// domain that no asset write was touching.
func TestUpdateAsset_EvictsManifest(t *testing.T) {
	w := newCFWorld(t)

	assetID := w.asset("manifest label before")

	if got := w.manifestLabel(assetID); got != "manifest label before" {
		t.Fatalf("manifest label is %q before the edit, want the seeded title — "+
			"fixture is wrong", got)
	}

	w.patchTitle(assetID, "manifest label after")

	if got := w.manifestLabel(assetID); got != "manifest label after" {
		t.Errorf("manifest still reads %q after PATCH, want %q — the manifest cache "+
			"was never evicted by an asset write. presentation.InvalidateAssetOn "+
			"existed for exactly this and had zero callers", got, "manifest label after")
	}
}

// TestDeleteAndRestoreAsset_EvictManifest covers the other two write
// paths. The manifest read applies EntityAsset's ROW predicate, so a
// soft delete has to make the manifest disappear and a restore has to
// bring it back — neither of which happens if the cache still holds the
// pre-write answer.
func TestDeleteAndRestoreAsset_EvictManifest(t *testing.T) {
	w := newCFWorld(t)

	// Restore runs through the softdelete service; without it
	// RestoreAsset returns "unwired" instead of exercising the path.
	w.assets.SoftDelete = softdelete.NewService(w.pool, nil)

	assetID := w.asset("delete-restore label")
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: w.owner, AuthMethod: "session"})

	// Populate the cache while the asset is live.
	if got := w.manifestLabel(assetID); got != "delete-restore label" {
		t.Fatalf("fixture: manifest label is %q", got)
	}

	resp, err := w.assets.DeleteAsset(ctx, openapi.DeleteAssetRequestObject{Id: openapi_types.UUID(assetID)})
	if err != nil {
		t.Fatalf("DeleteAsset: %v", err)
	}
	if _, ok := resp.(openapi.DeleteAsset204Response); !ok {
		t.Fatalf("DeleteAsset returned %T, want 204", resp)
	}

	rec := httptest.NewRecorder()
	w.iiif.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/iiif/3/asset/"+assetID.String()+"/manifest.json", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("the manifest for a soft-deleted asset still returns 200 — the cached "+
			"pre-delete copy is being served, body: %s", rec.Body.String())
	}

	rresp, err := w.assets.RestoreAsset(ctx, openapi.RestoreAssetRequestObject{Id: openapi_types.UUID(assetID)})
	if err != nil {
		t.Fatalf("RestoreAsset: %v", err)
	}
	if _, ok := rresp.(openapi.RestoreAsset204Response); !ok {
		t.Fatalf("RestoreAsset returned %T, want 204", rresp)
	}
	if got := w.manifestLabel(assetID); got != "delete-restore label" {
		t.Errorf("the manifest is still missing after a restore (got label %q) — restore "+
			"needs the same eviction delete does, or the 404 the delete cached sticks", got)
	}
}

// ---------------------------------------------------------------------------
// Hard delete — the CASCADE nobody told the caches about
// ---------------------------------------------------------------------------

// TestHardDeleteFanout_EvictsSubtitlesAndManifest drives the real GC
// pass with the production hook installed and asserts the two caches
// its CASCADEs empty.
//
// subtitles.GetForAsset is read-through and never consults the asset,
// so after the row is destroyed it goes on returning the pre-delete
// track slice from its LRU with nothing left in the database to
// contradict it. That is the exact hole the package doc has described
// since Phase 1.18.B-3, next to a claim that assets/ was already
// calling this. It was not.
func TestHardDeleteFanout_EvictsSubtitlesAndManifest(t *testing.T) {
	w := newCFWorld(t)

	assetID := w.asset("hard-delete label")
	if _, err := w.pool.Exec(context.Background(),
		`INSERT INTO asset_subtitle_tracks (asset_id, lang, label, file_hash, source_format)
		 VALUES ($1,'en','English','deadbeef','vtt')`, assetID); err != nil {
		t.Fatalf("seed track: %v", err)
	}

	// Populate BOTH caches from the live row.
	tracks, err := w.subtitles.GetForAsset(context.Background(), assetID)
	if err != nil {
		t.Fatalf("GetForAsset: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("fixture: %d tracks before the delete, want 1", len(tracks))
	}
	if got := w.manifestLabel(assetID); got != "hard-delete label" {
		t.Fatalf("fixture: manifest label is %q", got)
	}

	// The production fan-out, reproduced from api.go.
	svc := softdelete.NewService(w.pool, nil)
	svc.OnAssetsHardDeleted = func(ctx context.Context, ids []uuid.UUID) {
		for _, id := range ids {
			subtitles.InvalidateForAsset(w.subtitles, id)
			_ = presentation.InvalidateAssetOn(ctx, w.manifests, id)
			_ = posts.InvalidateForAsset(ctx, w.registry, w.pool, id)
		}
	}

	// Age the soft delete past the retention window so the GC claims
	// it. retentionDays must be >= 1, so backdate by two days.
	if _, err := w.pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = NOW() - INTERVAL '2 days' WHERE id = $1`, assetID); err != nil {
		t.Fatalf("backdate delete: %v", err)
	}

	// RED-FIRST: the row is on its way out and both caches still
	// answer from the world where it existed. Without this the
	// assertions below could pass on a fixture that never cached
	// anything.
	if got, _ := w.subtitles.GetForAsset(context.Background(), assetID); len(got) != 1 {
		t.Fatal("the subtitle cache was not populated — this fixture proves nothing")
	}

	n, err := svc.HardDeletePastAssets(context.Background(), 1, 100)
	if err != nil {
		t.Fatalf("HardDeletePastAssets: %v", err)
	}
	if n < 1 {
		t.Fatalf("GC deleted %d assets, want at least 1 — the pass never claimed the row", n)
	}

	// CASCADE removed the rows; the question is whether the cache noticed.
	after, err := w.subtitles.GetForAsset(context.Background(), assetID)
	if err != nil {
		t.Fatalf("GetForAsset after GC: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("subtitles still serves %d track(s) for a hard-deleted asset — the "+
			"CASCADE emptied the table and the LRU kept answering", len(after))
	}

	rec := httptest.NewRecorder()
	w.iiif.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/iiif/3/asset/"+assetID.String()+"/manifest.json", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("the manifest for a hard-deleted asset still returns 200: %s", rec.Body.String())
	}
}
