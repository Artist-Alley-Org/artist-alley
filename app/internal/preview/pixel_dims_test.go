// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #757 — masonry rendered every tile square because nothing wrote the
// dimensions the layout sizes tiles from.
//
// The whole client-side chain was built and merged green: the
// pixel_width / pixel_height field definitions (#618), the browse-payload
// projection (#640), the column bucketer's height prediction (#651), the
// tile floor (#652) and the per-asset `aspect-ratio` on the tile wrapper.
// Every one of those has a passing test. `asset_field_value` held ZERO
// pixel_width rows across all 1946 assets, so every tile fell through to
// the 1:1 last resort and the feature was invisible.
//
// WHY THIS TEST ASSERTS VARIETY, NOT PRESENCE. "the dimensions are
// non-null" passes on an implementation that writes a single constant —
// and a constant is precisely what the bug looked like from the
// layout's side. ADR 0068 carries this rule because the 3D smoke test
// asserted "the render is not transparent" and an entirely untextured
// catalogue shipped green behind it, twice. So the assertion is that
// three handlers driven through their REAL Handle path produce three
// DIFFERENT ratios, and the wide one lands where arithmetic says it
// must.
//
// WHY THE AUDIO CASE IS NOT OPTIONAL. Half the catalogue has no source
// pixels at all — a 3D model, a font, an audio file. Those are the
// assets whose tiles were most obviously wrong (a waveform is ~16:3;
// square is a caricature of it) and they are the ones an EXIF-shaped
// fix cannot reach. The waveform is also a free oracle: the audio
// handler renders at a fixed WaveformWidth x WaveformHeight, so the
// correct recorded ratio is knowable in advance — 5.33 — rather than
// merely "not 1".
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// pixelDimsOf reads back what the preview job recorded, through the
// same field_definition codes the browse projection reads. Returns
// (0, 0) when the asset has no recorded pair, which is the pre-fix
// state for every asset in the catalogue.
func (r *previewTestRig) pixelDimsOf(t *testing.T, id uuid.UUID) (int, int) {
	t.Helper()
	var w, h int
	err := r.pool.QueryRow(context.Background(), `
		SELECT COALESCE(MAX(v.value_num) FILTER (WHERE f.code = $2), 0)::INT,
		       COALESCE(MAX(v.value_num) FILTER (WHERE f.code = $3), 0)::INT
		  FROM asset_field_value v
		  JOIN field_definition f ON f.id = v.field_id
		 WHERE v.asset_id = $1 AND f.code IN ($2, $3)`,
		id, pixeldims.Width, pixeldims.Height).Scan(&w, &h)
	if err != nil {
		t.Fatalf("read pixel dims: %v", err)
	}
	return w, h
}

// setByOf reports the provenance recorded alongside the width. The
// asset_field_value CHECK constraint admits a fixed vocabulary, and
// 'computed' is the one that means "the system measured this"; a write
// landing as 'exif' would be claiming a tag was read off a file that,
// for most of this catalogue, has no tags at all.
func (r *previewTestRig) setByOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var by string
	if err := r.pool.QueryRow(context.Background(), `
		SELECT v.set_by FROM asset_field_value v
		  JOIN field_definition f ON f.id = v.field_id
		 WHERE v.asset_id = $1 AND f.code = $2`,
		id, pixeldims.Width).Scan(&by); err != nil {
		t.Fatalf("read set_by: %v", err)
	}
	return by
}

// solidPNG renders a w x h plate. Content is irrelevant here — the
// SHAPE is the whole subject — but it is not flat, so the thumbhash
// stamp running off the same image still has something to encode.
func solidPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x % 251), G: uint8(y % 241), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// threeJSShim builds the node stand-in plus the worker-script directory
// the real availability probe insists on. Lifted from the #645 model
// test — the shim has to satisfy threeJSAvailable, not bypass it.
func threeJSShim(t *testing.T, framePath string) (nodePath, scriptPath string) {
	t.Helper()
	workerDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workerDir, "node_modules", "three"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath = filepath.Join(workerDir, "worker.mjs")
	if err := os.WriteFile(scriptPath, []byte("// stand-in; the node shim never reads it"), 0o644); err != nil {
		t.Fatal(err)
	}
	nodePath = writeShim(t, t.TempDir(), "node", `
while [ $# -gt 0 ]; do
  case "$1" in
    --output)
      mkdir -p "$2/turntable" "$2/views"
      cp `+framePath+` "$2/poster.png"
      cp `+framePath+` "$2/turntable/frame_0000.png"
      cp `+framePath+` "$2/views/top.png"
      cp `+framePath+` "$2/views/bottom.png"
      shift 2 ;;
    *) shift ;;
  esac
done
exit 0
`)
	return nodePath, scriptPath
}

// ---------------------------------------------------------------------------
// The three handlers, driven for real, then compared.
// ---------------------------------------------------------------------------

type dimSample struct {
	name  string
	w, h  int
	about float64 // expected width/height; 0 skips the exact check
}

// TestPreview_RecordsLadderSourceDimensions is the #757 regression.
//
// One test rather than three, because the property that was broken is a
// RELATION between assets — "tiles in a masonry have different heights"
// — and no per-handler assertion can state it. Three separate green
// per-handler tests are exactly what the codebase already had.
func TestPreview_RecordsLadderSourceDimensions(t *testing.T) {
	rig := newPreviewTestRig(t)
	var got []dimSample

	// ── raster: a plain non-square PNG through preview.raster ────────
	// The raster handler keeps its own variant loop (it reports
	// generated / skipped rungs and hard-fails on a rung error), so it
	// is the one handler that does NOT reach the shared ladder — and
	// therefore the one a fix applied only to fanToLadder would
	// silently miss.
	t.Run("raster", func(t *testing.T) {
		id, hash := rig.seedPreviewAsset(t, "png", solidPNG(t, 900, 300))

		h := NewRasterHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
		payload, _ := json.Marshal(RasterPayload{
			AssetID: id, FileHash: hash, FileExtension: "png",
		})
		if _, err := h.Handle(t.Context(), &jobs.Claim{
			ID: uuid.New(), Type: jobs.TypePreviewRaster, Payload: payload,
		}); err != nil {
			t.Fatalf("preview.raster Handle: %v", err)
		}
		w, hh := rig.pixelDimsOf(t, id)
		if w != 900 || hh != 300 {
			t.Errorf("raster recorded %dx%d, want 900x300 (#757)", w, hh)
		}
		if by := rig.setByOf(t, id); by != pixeldims.SetBy {
			t.Errorf("raster recorded set_by=%q, want %q", by, pixeldims.SetBy)
		}
		got = append(got, dimSample{"raster", w, hh, 3})
	})

	// ── audio: no source pixels at all ───────────────────────────────
	// The case an EXIF-shaped fix cannot reach, and the free oracle:
	// the waveform render is a fixed 2048x384, so 5.33 is arithmetic,
	// not a measurement.
	t.Run("audio_waveform", func(t *testing.T) {
		id, hash := rig.seedPreviewAsset(t, "ogg", []byte("not really ogg, the shims don't care"))

		h := NewAudioHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
		shimDir := t.TempDir()
		fixture := filepath.Join(shimDir, "wave-fixture.png")
		writeWaveformPNG(t, fixture, h.WaveformWidth, h.WaveformHeight)

		h.FFprobePath = writeShim(t, shimDir, "ffprobe", `
echo '{"format":{"duration":"12.5","format_name":"ogg","bit_rate":"128000","tags":{}},"streams":[{"codec_type":"audio","codec_name":"vorbis","sample_rate":"44100","channels":2}]}'
`)
		h.FFmpegPath = writeShim(t, shimDir, "ffmpeg", `
for last; do :; done
cp `+fixture+` "$last"
`)
		h.TempDir = t.TempDir()

		payload, _ := json.Marshal(AudioPayload{
			AssetID: id, FileHash: hash, FileExtension: "ogg",
		})
		if _, err := h.Handle(t.Context(), &jobs.Claim{
			ID: uuid.New(), Type: jobs.TypePreviewAudio, Payload: payload,
		}); err != nil {
			t.Fatalf("preview.audio Handle: %v", err)
		}
		w, hh := rig.pixelDimsOf(t, id)
		if w != h.WaveformWidth || hh != h.WaveformHeight {
			t.Errorf("audio recorded %dx%d, want %dx%d — a waveform tile is ~16:3, and "+
				"this is the bucket a source-pixel fix cannot reach (#757)",
				w, hh, h.WaveformWidth, h.WaveformHeight)
		}
		got = append(got, dimSample{"audio", w, hh, 16.0 / 3.0})
	})

	// ── 3D turntable: legitimately square ────────────────────────────
	// Included precisely BECAUSE it is 1:1. It pins the honest square,
	// so the distinctness check below is measuring real shapes rather
	// than counting handlers.
	t.Run("model_turntable", func(t *testing.T) {
		id, hash := rig.seedPreviewAsset(t, "glb", []byte("glTF stand-in; the worker shim doesn't parse it"))

		frame := filepath.Join(t.TempDir(), "frame-fixture.png")
		writeTurntablePNG(t, frame, 512)
		node, script := threeJSShim(t, frame)

		h := NewModelHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
		h.NodePath = node
		h.ThreeJSScript = script
		h.TempDir = t.TempDir()
		h.Frames = 1

		payload, _ := json.Marshal(ModelPayload{
			AssetID: id, FileHash: hash, FileExtension: "glb",
		})
		if _, err := h.Handle(t.Context(), &jobs.Claim{
			ID: uuid.New(), Type: jobs.TypePreview3D, Payload: payload,
		}); err != nil {
			t.Fatalf("preview.3d Handle: %v", err)
		}
		w, hh := rig.pixelDimsOf(t, id)
		if w != 512 || hh != 512 {
			t.Errorf("3D recorded %dx%d, want 512x512 (#757)", w, hh)
		}
		got = append(got, dimSample{"model", w, hh, 1})
	})

	// ── the relation ─────────────────────────────────────────────────
	if len(got) < 2 {
		t.Fatal("not enough handlers ran to compare ratios; the subtests above failed")
	}
	distinct := map[float64]string{}
	for _, s := range got {
		if s.w <= 0 || s.h <= 0 {
			t.Errorf("%s recorded a non-positive pair %dx%d — nothing downstream can "+
				"divide by that, so the tile falls back to square exactly as before (#757)",
				s.name, s.w, s.h)
			continue
		}
		ratio := math.Round(float64(s.w)/float64(s.h)*100) / 100
		if s.about > 0 && math.Abs(ratio-s.about) > 0.02 {
			t.Errorf("%s ratio %.2f, want ~%.2f", s.name, ratio, s.about)
		}
		if prev, dup := distinct[ratio]; dup {
			t.Errorf("%s and %s both recorded ratio %.2f — a single constant passes an "+
				"is-not-null assertion and still renders a wall of squares (ADR 0068)",
				s.name, prev, ratio)
		}
		distinct[ratio] = s.name
	}
	if len(distinct) < 2 {
		t.Fatalf("preview jobs produced %d distinct tile ratio(s); masonry needs varied "+
			"heights or it is a grid wearing a masonry's name (#757)", len(distinct))
	}
}

// TestPreview_RecordsRotatedShapeForAnOrientedPhoto is the #765
// regression, from the consumer's side.
//
// A phone photo shot in portrait is stored landscape with an EXIF
// Orientation tag telling the viewer to turn it. This package's
// architecture (asset/metadata/doc.go) never touches the source bytes —
// the source hash has to stay stable for content-addressed dedup — so
// the rotation is applied to the DECODED image inside the raster
// handler, immediately before the ladder is built. Every rung, and
// every tile a card draws, is therefore portrait.
//
// The EXIF extractor used to write pixel_width / pixel_height off
// image.DecodeConfig, which reports the STORED grid: landscape. Both
// writers aimed at the same two rows with extraction_mode='replace' and
// no precedence rule between them, and on the upload path the extract
// job is enqueued after the preview job — so the losing value was the
// right one, and a portrait photo reserved a landscape tile.
//
// WHY THE ASSERTION IS A TRANSPOSE, NOT A NUMBER. 48x96 is only
// meaningful next to the fixture's stored 96x48; the test states the
// relation so a fixture regenerated at a different size still asserts
// the right thing, and a square fixture — which would make this pass
// for free — is rejected outright.
func TestPreview_RecordsRotatedShapeForAnOrientedPhoto(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "asset", "metadata", "testdata",
		"orientation_6_landscape.jpg"))
	if err != nil {
		t.Fatalf("load rotated fixture: %v", err)
	}

	// ── the fixture really is rotated ────────────────────────────────
	// Asserted HERE, not assumed from the filename. A fixture that is
	// not actually rotated passes the outcome assertion below while
	// proving nothing, which is the failure mode #778 and #791 shipped.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode fixture config: %v", err)
	}
	if cfg.Width <= cfg.Height {
		t.Fatalf("fixture stored grid %dx%d is not landscape — a square or portrait "+
			"fixture makes the transpose invisible and this test vacuous (#765)",
			cfg.Width, cfg.Height)
	}
	res, err := exifExtractor.Extract(t.Context(), bytes.NewReader(raw), "image/jpeg")
	if err != nil {
		t.Fatalf("read fixture orientation: %v", err)
	}
	if res.Orientation != 6 && res.Orientation != 8 {
		t.Fatalf("fixture Orientation tag = %d — must be 6 or 8 (a quarter turn) or "+
			"nothing transposes and the assertion below is satisfied by the bug (#765)",
			res.Orientation)
	}

	// ── the outcome ──────────────────────────────────────────────────
	rig := newPreviewTestRig(t)
	id, hash := rig.seedPreviewAsset(t, "jpg", raw)

	h := NewRasterHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	payload, _ := json.Marshal(RasterPayload{
		AssetID: id, FileHash: hash, FileExtension: "jpg",
	})
	if _, err := h.Handle(t.Context(), &jobs.Claim{
		ID: uuid.New(), Type: jobs.TypePreviewRaster, Payload: payload,
	}); err != nil {
		t.Fatalf("preview.raster Handle: %v", err)
	}

	gotW, gotH := rig.pixelDimsOf(t, id)
	if gotW != cfg.Height || gotH != cfg.Width {
		t.Errorf("recorded %dx%d for a fixture stored %dx%d with a quarter-turn tag; "+
			"want the transpose %dx%d. Recording the stored pair tiles a portrait "+
			"phone photo as landscape (#765)",
			gotW, gotH, cfg.Width, cfg.Height, cfg.Height, cfg.Width)
	}
	if gotW >= gotH {
		t.Errorf("recorded pair %dx%d is not portrait — masonry reserves a landscape "+
			"tile for an image every rung of which is portrait (#765)", gotW, gotH)
	}
	if by := rig.setByOf(t, id); by != pixeldims.SetBy {
		t.Errorf("recorded set_by=%q, want %q — 'exif' would mean the pre-rotation "+
			"writer is back (#765)", by, pixeldims.SetBy)
	}
}

// TestPreview_RerecordsDimensionsOnForcedRebuild covers the backfill
// vehicle. `aa rebuild-previews` (#763) enqueues force=true jobs, which
// is how the 1946 assets that predate this change acquire dimensions —
// so a pair that only ever lands on a FIRST render would leave every
// existing install exactly as broken as before.
//
// It also pins the correctness half: a stale pair is worse than an
// absent one, because it is what the layout reserves space from before
// any bytes arrive, so a wrong pair mis-sizes the tile with total
// confidence and never corrects. A re-render that changes the shape
// must move the number.
func TestPreview_RerecordsDimensionsOnForcedRebuild(t *testing.T) {
	rig := newPreviewTestRig(t)
	id, hash := rig.seedPreviewAsset(t, "png", solidPNG(t, 640, 480))

	h := NewRasterHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	run := func(force bool) {
		t.Helper()
		payload, _ := json.Marshal(RasterPayload{
			AssetID: id, FileHash: hash, FileExtension: "png", Force: force,
		})
		if _, err := h.Handle(t.Context(), &jobs.Claim{
			ID: uuid.New(), Type: jobs.TypePreviewRaster, Payload: payload,
		}); err != nil {
			t.Fatalf("preview.raster Handle(force=%v): %v", force, err)
		}
	}
	run(false)
	if w, hh := rig.pixelDimsOf(t, id); w != 640 || hh != 480 {
		t.Fatalf("first render recorded %dx%d, want 640x480", w, hh)
	}

	// Replace the stored bytes with a differently-shaped image, exactly
	// as a replace-file or a renderer change would, then rebuild.
	if _, err := rig.storage.Backend.Put(t.Context(), hash, storage.VariantOriginal,
		bytes.NewReader(solidPNG(t, 300, 900))); err != nil {
		t.Fatalf("replace original: %v", err)
	}
	run(true)
	if w, hh := rig.pixelDimsOf(t, id); w != 300 || hh != 900 {
		t.Errorf("forced rebuild recorded %dx%d, want 300x900 — the tile would keep "+
			"reserving the old shape forever (#757/#763)", w, hh)
	}
}
