// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// preview.gif (#832) + the cue-count rule the frontend scrub reads (#835).
//
// The two halves are tested together because they meet in one place: the
// number of cues `sprites.vtt` declares. The backend's job is to declare
// exactly the cells it filled; the frontend's job is to cycle exactly
// what is declared. Either half alone leaves the bug.

package preview

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
)

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// GIF used to land on preview.raster, whose only possible output for an
// animated file is frame one. It now routes to its own handler — and
// unlike video it must NOT be split into two jobs, because there is no
// expensive half to defer.
func TestPlanForExt_RoutesGifToItsOwnUnsplitJob(t *testing.T) {
	ext := "gif"
	if jt := dispatch.JobTypeForExt(&ext); jt != jobs.TypePreviewGif {
		t.Fatalf("gif routes to %q, want %q", jt, jobs.TypePreviewGif)
	}
	steps := dispatch.PlanForExt(&ext, jobs.PriorityNormal)
	if len(steps) != 1 {
		t.Fatalf("gif plan is %d steps (%+v), want exactly 1 — the split is video-only", len(steps), steps)
	}
	if steps[0].Type != jobs.TypePreviewGif || steps[0].Priority != jobs.PriorityNormal {
		t.Errorf("gif plan is %+v, want one preview.gif at the caller's priority", steps[0])
	}
}

// GIF must stay in ImageExts. It is a subset route, not a move: three
// unrelated consumers (assetTypeFor, isImageExt, needsProcessing) read
// that set to mean "this is an image", and pulling gif out to express
// "does not use preview.raster" would silently reclassify every GIF in
// the library as an untyped asset.
func TestGifStaysAnImageEverywhereButTheRouter(t *testing.T) {
	if !dispatch.Has(dispatch.ImageExts, "gif") {
		t.Error("gif left ImageExts — assetTypeFor/isImageExt/needsProcessing all key off that set")
	}
	if !dispatch.Has(dispatch.GifExts, "gif") {
		t.Error("gif is not in GifExts — nothing would route it to preview.gif")
	}
}

// The cap the migration seeds has to name the type the registry actually
// registers. A typo there is silent — the cap simply never applies.
func TestGifJobType_MatchesTheSeededConcurrencyKey(t *testing.T) {
	const key = "jobs.type_concurrency.preview.gif"
	if got := "jobs.type_concurrency." + string(jobs.TypePreviewGif); got != key {
		t.Errorf("migration 00027 seeds %q, the handler registers %q", key, got)
	}
}

// ---------------------------------------------------------------------------
// Frame counting — the decision the router cannot make
// ---------------------------------------------------------------------------

func TestCountGIFFrames(t *testing.T) {
	cases := []struct {
		name   string
		body   []byte
		frames int
	}{
		{"single frame", buildGIF(t, 1), 1},
		{"two frames", buildGIF(t, 2), 2},
		{"many frames", buildGIF(t, 37), 37},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := countGIFFramesIn(bytes.NewReader(tc.body))
			if err != nil {
				t.Fatalf("countGIFFrames: %v", err)
			}
			if got != tc.frames {
				t.Errorf("counted %d frames, want %d", got, tc.frames)
			}
		})
	}
}

// A truncated GIF still has usable leading frames — every browser
// renders them — so the scan reports what it read rather than refusing
// the file. A file that is not a GIF at all is a different thing: that
// is a mis-typed upload, and returning "0 frames, no error" would send
// it down the still path to fail confusingly later.
func TestCountGIFFrames_TruncatedVsNotAGIF(t *testing.T) {
	full := buildGIF(t, 5)
	// Chop the trailer and part of the last frame's data.
	truncated := full[:len(full)-40]
	n, err := countGIFFramesIn(bytes.NewReader(truncated))
	if err != nil {
		t.Fatalf("a truncated GIF must not error: %v", err)
	}
	if n < 1 {
		t.Errorf("truncated GIF counted %d frames, want at least the leading ones", n)
	}

	if _, err := countGIFFramesIn(bytes.NewReader([]byte("\x89PNG\r\n\x1a\n and then some"))); err == nil {
		t.Error("a PNG counted as a GIF without error — a mis-typed upload must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Cue count (#835)
// ---------------------------------------------------------------------------

// The interval floor means a short clip cannot fill the grid, and
// ffmpeg's `tile` filter pads what it did not fill with black. Declaring
// a cue for a padding cell is the whole of #835: the client cannot tell
// padding from a dark frame, so it scrubbed through the black.
func TestSpriteCueCount_DeclaresOnlyTheCellsFFmpegFills(t *testing.T) {
	const grid = spriteCols * spriteRows // 100

	cases := []struct {
		name      string
		durationS float64
		want      int
	}{
		// The reproducer from the issue: a ~5s clip at the 0.2s floor
		// fills 25 cells, so 75 of the old 100 cues pointed at padding.
		{"5s clip at the interval floor", 5.0, 25},
		{"2s clip", 2.0, 10},
		// Just past a cell boundary — the extra frame IS emitted, so it
		// must be declared.
		{"5.04s clip", 5.04, 26},
		// Long enough that duration/100 clears the floor: the grid fills
		// and nothing changes from the pre-#835 behaviour.
		{"30s clip fills the grid", 30.0, grid},
		{"10 minutes fills the grid", 600.0, grid},
		// Exactly at the floor's crossover.
		{"20s clip is the shortest that fills", 20.0, grid},
		// Degenerate: a single sub-interval clip still gets one cell, not
		// zero (a sheet with no cues is a scrub that never starts).
		{"sub-interval clip", 0.05, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interval := tc.durationS / float64(grid)
			if interval < spriteMinInterval {
				interval = spriteMinInterval
			}
			if got := spriteCueCount(tc.durationS, interval, grid); got != tc.want {
				t.Errorf("a %.2fs clip declares %d cues, want %d", tc.durationS, got, tc.want)
			}
		})
	}
}

// ceil, never round-up-past-the-frames: over-declaring shows a black
// cell, which is the bug. One frame short is invisible.
func TestSpriteCueCount_NeverExceedsTheGrid(t *testing.T) {
	const grid = 100
	for _, d := range []float64{0.001, 1, 19.9, 20, 20.1, 1e6} {
		interval := d / float64(grid)
		if interval < spriteMinInterval {
			interval = spriteMinInterval
		}
		if n := spriteCueCount(d, interval, grid); n < 1 || n > grid {
			t.Errorf("duration %v produced %d cues, outside [1,%d]", d, n, grid)
		}
	}
}

// ---------------------------------------------------------------------------
// The handler, end to end
// ---------------------------------------------------------------------------

// A STILL GIF MUST NOT COST A SPRITE JOB. Asserted by the strongest
// available evidence: the ffmpeg and ffprobe shims record every
// invocation, and on this path there must be none at all. Checking only
// that no `sprites.jpg` landed would also pass if the handler ran a full
// ffmpeg pass and then threw the result away.
func TestGifJob_StillGetsTheLadderAndSpawnsNoFFmpeg(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "gif", buildGIF(t, 1))

	h := NewGifHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	shimDir := t.TempDir()
	ffmpegLog := filepath.Join(shimDir, "ffmpeg-called")
	ffprobeLog := filepath.Join(shimDir, "ffprobe-called")
	h.FFmpegPath = writeShim(t, shimDir, "ffmpeg", "echo called >> "+ffmpegLog+"\nexit 1\n")
	h.FFprobePath = writeShim(t, shimDir, "ffprobe", "echo called >> "+ffprobeLog+"\nexit 1\n")

	res := runGifJob(t, h, assetID, hash)
	if res.Animated {
		t.Fatalf("a one-frame GIF was classified as animated (frames=%d)", res.Frames)
	}
	if res.Frames != 1 {
		t.Errorf("frames = %d, want 1", res.Frames)
	}

	for _, log := range []string{ffmpegLog, ffprobeLog} {
		if _, err := os.Stat(log); err == nil {
			t.Errorf("%s was invoked for a STILL gif — the still path must be Go-native", filepath.Base(log))
		}
	}
	for _, key := range []string{"sprites.jpg", "sprites.vtt", "poster"} {
		if variantExists(t, rig, hash, key) {
			t.Errorf("a still GIF produced a %q variant; it should get the raster ladder and nothing else", key)
		}
	}
	// It DID get the ladder — otherwise "no sprite work" would be
	// trivially satisfied by a handler that does nothing.
	if !variantExists(t, rig, hash, "col") {
		t.Error("a still GIF got no `col` rung — the card would be blank")
	}
}

// An animated GIF is a short silent video and gets the same sheet + cue
// file every other moving format gets. This is the half that never
// existed: routed to preview.raster, an animated GIF produced frame one
// and no sheet at all.
func TestGifJob_AnimatedGetsASpriteSheetAndCueFile(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "gif", buildGIF(t, 60))

	h := NewGifHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	shimDir := t.TempDir()
	// A 30s 260x212 animation — the shape of the seed's Pong capture,
	// which is what ffprobe really reports for a GIF (verified against
	// the file: duration 29.97, 260x212, avg_frame_rate 25/1).
	h.FFprobePath = writeShim(t, shimDir, "ffprobe", `cat <<'EOF'
{"streams":[{"codec_type":"video","width":260,"height":212,
  "avg_frame_rate":"25/1","r_frame_rate":"25/1"}],
 "format":{"duration":"29.970000"}}
EOF
`)
	sheet := filepath.Join(shimDir, "sheet.jpg")
	writeSpriteSheetJPEG(t, sheet, 2400, 1960)
	poster := filepath.Join(shimDir, "poster.jpg")
	writeFlatJPEG(t, poster, 260, 212, color.RGBA{R: 200, G: 180, B: 60, A: 255})
	// One shim serves both calls: the poster extract asks for a single
	// frame, the sheet pass asks for the tile. Both write to the last
	// argument, so the output name distinguishes them.
	h.FFmpegPath = writeShim(t, shimDir, "ffmpeg", `
for last; do :; done
case "$last" in
  *sprites.jpg) cp `+sheet+` "$last" ;;
  *)            cp `+poster+` "$last" ;;
esac
`)

	res := runGifJob(t, h, assetID, hash)
	if !res.Animated {
		t.Fatalf("a 60-frame GIF was classified as still (frames=%d)", res.Frames)
	}
	for _, key := range []string{"sprites.jpg", "sprites.vtt", "col"} {
		if !variantExists(t, rig, hash, key) {
			t.Errorf("animated GIF produced no %q variant", key)
		}
	}

	// The cue file has to describe the sheet that was actually written,
	// which is the property the frontend now depends on entirely.
	vtt := readVariant(t, rig, hash, "sprites.vtt")
	// 29.97s over a 100-cell grid clears the 0.2s floor, so the grid
	// fills and there are 100 cues.
	if n := bytes.Count(vtt, []byte("-->")); n != spriteCols*spriteRows {
		t.Errorf("cue file declares %d cues for a 29.97s clip, want %d", n, spriteCols*spriteRows)
	}
	// 2400x1960 over a 10x10 grid = 240x196 cells.
	if !bytes.Contains(vtt, []byte("sprites.jpg#xywh=0,0,240,196")) {
		t.Errorf("cue 0 does not describe the 240x196 cells of the sheet on disk:\n%s", firstLines(vtt, 4))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// buildGIF writes a real n-frame GIF. Real bytes rather than a canned
// fixture because the thing under test is a walk of the block structure,
// and a hand-rolled byte string would only ever prove the walk agrees
// with whoever wrote the string.
func buildGIF(t *testing.T, frames int) []byte {
	t.Helper()
	g := &gif.GIF{}
	for i := 0; i < frames; i++ {
		pal := color.Palette{color.Black, color.RGBA{R: uint8(i * 3), G: 120, B: 200, A: 255}}
		img := image.NewPaletted(image.Rect(0, 0, 16, 12), pal)
		for p := range img.Pix {
			img.Pix[p] = uint8(p % 2)
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 4)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

func runGifJob(t *testing.T, h *GifHandler, assetID uuid.UUID, hash string) GifResult {
	t.Helper()
	payload, err := json.Marshal(dispatch.NewPayload(assetID, hash, strPtr("gif"), false))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	raw, err := h.Handle(t.Context(), &jobs.Claim{Payload: payload})
	if err != nil {
		t.Fatalf("preview.gif Handle: %v", err)
	}
	var res GifResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return res
}

func strPtr(s string) *string { return &s }

func variantExists(t *testing.T, rig *previewTestRig, hash, key string) bool {
	t.Helper()
	rc, _, err := rig.storage.Backend.Get(t.Context(), hash, key)
	if err != nil {
		return false
	}
	_ = rc.Close()
	return true
}

func firstLines(b []byte, n int) []byte {
	lines := bytes.SplitN(b, []byte("\n"), n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return bytes.Join(lines, []byte("\n"))
}
