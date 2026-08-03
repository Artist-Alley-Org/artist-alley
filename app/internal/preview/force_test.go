// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// ---------------------------------------------------------------------------
// #760 — a forced re-render must actually rewrite the bytes.
//
// WHAT THESE TESTS MUST NOT DO: assert that the job returned without an
// error. A job that skips every output and writes nothing returns
// exactly that — `done`, no error, a tidy result — and it is the whole
// defect. The only assertion that distinguishes fixed from broken is
// "the stored variant is no longer the bytes that were there before".
//
// So each test plants a SENTINEL: a variant whose bytes could not
// possibly be produced by the handler (a flat magenta square, standing
// in for the real thing — Blender's missing-texture colour, which is
// what 590 stale 3D thumbnails in the dev catalogue actually look
// like). Then:
//
//   force=false → the sentinel must SURVIVE   (the cheap re-queue still
//                                              works; a reseed must not
//                                              become a full catalogue
//                                              re-render)
//   force=true  → the sentinel must be GONE   (the fix)
// ---------------------------------------------------------------------------

// sentinelBytes is a 32×32 magenta PNG: not something any handler in
// this package renders, so its presence proves nothing overwrote it and
// its absence proves something did.
func sentinelBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.NRGBA{R: 255, G: 0, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode sentinel: %v", err)
	}
	return buf.Bytes()
}

// plantStaleVariant writes the sentinel into a variant slot and records
// the row, i.e. reproduces "this asset already has a rendered preview,
// and it is wrong".
func plantStaleVariant(t *testing.T, rig *previewTestRig, hash, key string, body []byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := rig.storage.Backend.Put(ctx, hash, key, bytes.NewReader(body)); err != nil {
		t.Fatalf("plant %s: %v", key, err)
	}
	if err := storage.New(rig.pool).UpsertVariant(ctx, storage.UpsertVariantParams{
		ObjectHash: hash, VariantKey: key,
		SizeBytes: int64(len(body)), ContentType: "image/png",
		Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("upsert %s: %v", key, err)
	}
}

// variantBytes reads a stored variant back.
func variantBytes(t *testing.T, rig *previewTestRig, hash, key string) []byte {
	t.Helper()
	rc, _, err := rig.storage.Backend.Get(context.Background(), hash, key)
	if err != nil {
		t.Fatalf("get variant %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read variant %s: %v", key, err)
	}
	return b
}

// variantTimestamps reads the row's created_at / updated_at pair, which
// is the DB-side evidence that a re-render happened (migration 00019).
func variantTimestamps(t *testing.T, rig *previewTestRig, hash, key string) (time.Time, time.Time) {
	t.Helper()
	var created, updated time.Time
	if err := rig.pool.QueryRow(context.Background(),
		`SELECT created_at, updated_at FROM storage_variants
		  WHERE object_hash = $1 AND variant_key = $2`, hash, key,
	).Scan(&created, &updated); err != nil {
		t.Fatalf("read variant timestamps for %s: %v", key, err)
	}
	return created, updated
}

// pngAsset builds a decodable source image for the raster handler.
func pngAsset(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 256, 192))
	for y := 0; y < 192; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode source: %v", err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Raster — the handler every image in the catalogue routes through, and
// the one whose ladder loop the other ten share via fanToLadder.
// ---------------------------------------------------------------------------

func TestPreviewForce_RasterOverwritesStaleVariant(t *testing.T) {
	rig := newPreviewTestRig(t)
	ctx := t.Context()

	assetID, hash := rig.seedPreviewAsset(t, "png", pngAsset(t))
	sentinel := sentinelBytes(t)
	plantStaleVariant(t, rig, hash, "col", sentinel)
	createdBefore, updatedBefore := variantTimestamps(t, rig, hash, "col")

	h := NewRasterHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)

	// --- the ordinary re-queue: must NOT re-render ---------------------
	plain, _ := json.Marshal(RasterPayload{
		AssetID: assetID, FileHash: hash, FileExtension: "png",
	})
	raw, err := h.Handle(ctx, &jobs.Claim{
		ID: uuid.New(), Type: jobs.TypePreviewRaster, Payload: plain,
	})
	if err != nil {
		t.Fatalf("preview.raster (force=false): %v", err)
	}
	var res RasterResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !contains(res.Skipped, "col") {
		t.Errorf("force=false did not report `col` as skipped; skipped=%v generated=%v",
			res.Skipped, res.Generated)
	}
	if got := variantBytes(t, rig, hash, "col"); !bytes.Equal(got, sentinel) {
		t.Fatal("force=false re-rendered an existing variant — every reseed would " +
			"now re-render the whole catalogue")
	}

	// --- forced: MUST re-render ---------------------------------------
	// A second's gap so an updated_at that genuinely moved is
	// distinguishable from one that merely has the same value.
	time.Sleep(1100 * time.Millisecond)
	forced, _ := json.Marshal(RasterPayload{
		AssetID: assetID, FileHash: hash, FileExtension: "png", Force: true,
	})
	raw, err = h.Handle(ctx, &jobs.Claim{
		ID: uuid.New(), Type: jobs.TypePreviewRaster, Payload: forced,
	})
	if err != nil {
		t.Fatalf("preview.raster (force=true): %v", err)
	}
	res = RasterResult{}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode forced result: %v", err)
	}
	if !contains(res.Generated, "col") {
		t.Errorf("force=true did not report `col` as generated; generated=%v skipped=%v",
			res.Generated, res.Skipped)
	}
	if contains(res.Skipped, "col") {
		t.Errorf("force=true still reported `col` as skipped: %v", res.Skipped)
	}

	// The assertion that fails on the unfixed code. Not "the job
	// succeeded" — it always did.
	if got := variantBytes(t, rig, hash, "col"); bytes.Equal(got, sentinel) {
		t.Fatal("force=true left the stale bytes in place (#760): the job reported " +
			"success and the thumbnail did not change")
	}

	createdAfter, updatedAfter := variantTimestamps(t, rig, hash, "col")
	if !createdAfter.Equal(createdBefore) {
		t.Errorf("created_at moved on a re-render (%v -> %v); it records the FIRST render",
			createdBefore, createdAfter)
	}
	if !updatedAfter.After(updatedBefore) {
		t.Errorf("updated_at did not advance (%v -> %v): the database cannot show that "+
			"the bytes were rewritten", updatedBefore, updatedAfter)
	}
}

// ---------------------------------------------------------------------------
// 3D — the reported case. 590 assets in the dev catalogue carry `col`
// variants rendered on 2026-07-16, before the three.js worker (#498) and
// before Blender left the image (#500), so every one of them is stale
// and every re-queue skipped it.
//
// The handler's early exit is its own code, not the shared ladder's: it
// checks five sentinel keys (col / sprites / turntable / views / iso)
// and returns before the renderer runs. So the ladder being force-aware
// proves nothing here — this drives the real Handle.
// ---------------------------------------------------------------------------

func TestPreviewForce_ModelReRendersDespiteCompleteVariantSet(t *testing.T) {
	rig := newPreviewTestRig(t)
	ctx := t.Context()

	assetID, hash := rig.seedPreviewAsset(t, "glb", []byte("glTF stand-in; the shim doesn't parse it"))
	sentinel := sentinelBytes(t)

	// Exactly the state the 590 are in: the full anchor set present, so
	// the handler's cheap-requeue path fires.
	for _, key := range []string{
		"col", "preview", "screen", "hires",
		"sprites.jpg", "turntable/0000.png", "views/top.png", "iso/source.png",
	} {
		plantStaleVariant(t, rig, hash, key, sentinel)
	}

	shimDir := t.TempDir()
	fixture := filepath.Join(shimDir, "frame-fixture.png")
	writeTurntablePNG(t, fixture, 256)

	workerDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workerDir, "node_modules", "three"), 0o755); err != nil {
		t.Fatal(err)
	}
	workerScript := filepath.Join(workerDir, "worker.mjs")
	if err := os.WriteFile(workerScript, []byte("// stand-in"), 0o644); err != nil {
		t.Fatal(err)
	}
	node := writeShim(t, shimDir, "node", `
while [ $# -gt 0 ]; do
  case "$1" in
    --output)
      mkdir -p "$2/turntable" "$2/views"
      cp `+fixture+` "$2/poster.png"
      cp `+fixture+` "$2/turntable/frame_0000.png"
      cp `+fixture+` "$2/views/top.png"
      cp `+fixture+` "$2/views/bottom.png"
      shift 2 ;;
    *) shift ;;
  esac
done
exit 0
`)

	h := NewModelHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.NodePath = node
	h.ThreeJSScript = workerScript
	h.TempDir = t.TempDir()
	h.Frames = 1

	// --- the ordinary re-queue: the documented cheap path -------------
	plain, _ := json.Marshal(ModelPayload{
		AssetID: assetID, FileHash: hash, FileExtension: "glb",
	})
	raw, err := h.Handle(ctx, &jobs.Claim{
		ID: uuid.New(), Type: jobs.TypePreview3D, Payload: plain,
	})
	if err != nil {
		t.Fatalf("preview.3d (force=false): %v", err)
	}
	var res ModelResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(res.Variants) != 0 {
		t.Errorf("force=false wrote variants %v; the complete-set early exit should have fired",
			res.Variants)
	}
	if got := variantBytes(t, rig, hash, "col"); !bytes.Equal(got, sentinel) {
		t.Fatal("force=false re-rendered a complete 3D variant set")
	}

	// The state every pre-#760 3D asset is in: a blur-up placeholder
	// encoded from the render that is about to be replaced. For a 3D
	// asset the ladder source is the RENDER, not the uploaded bytes, so
	// a corrected render must produce a different hash — unlike a raster
	// asset, whose source cannot change under a stable hash.
	staleTh := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}
	if _, err := rig.pool.Exec(ctx,
		`UPDATE assets SET thumbhash = $2 WHERE id = $1`, assetID, staleTh); err != nil {
		t.Fatalf("plant stale thumbhash: %v", err)
	}

	// --- forced: MUST re-render --------------------------------------
	forced, _ := json.Marshal(ModelPayload{
		AssetID: assetID, FileHash: hash, FileExtension: "glb", Force: true,
	})
	if _, err := h.Handle(ctx, &jobs.Claim{
		ID: uuid.New(), Type: jobs.TypePreview3D, Payload: forced,
	}); err != nil {
		t.Fatalf("preview.3d (force=true): %v", err)
	}

	// Every stale key must be gone, not just the card thumbnail: a fix
	// that repainted `col` and left the turntable would still ship a
	// magenta scrub strip.
	for _, key := range []string{
		"col", "preview", "screen", "hires",
		"sprites.jpg", "turntable/0000.png", "views/top.png", "iso/source.png",
	} {
		if got := variantBytes(t, rig, hash, key); bytes.Equal(got, sentinel) {
			t.Errorf("force=true left stale bytes at %q (#760)", key)
		}
	}

	// The card paints the placeholder FIRST. Leaving it makes a
	// corrected thumbnail fade up out of the magenta one it replaced.
	if got := rig.thumbhashOf(t, assetID); bytes.Equal(got, staleTh) {
		t.Error("force=true left the thumbhash encoded from the stale render (#760)")
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
