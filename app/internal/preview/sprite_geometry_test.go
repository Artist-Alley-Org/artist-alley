// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// ---------------------------------------------------------------------------
// #761 — scrub sprite cells must keep the source's aspect ratio, and the
// WebVTT must describe the sheet that was ACTUALLY written.
//
// The original bug was cosmetic: `scale=160:90` with no
// force_original_aspect_ratio, so a portrait clip's thumbnails were
// squashed into landscape. The dangerous bug is the one a naive fix
// introduces — the same two numbers fed BOTH the ffmpeg filter and the
// `#xywh=` crop rectangles, so changing one and not the other makes
// every hover thumbnail crop a sliding, wrong region. That reads as
// "plausible but wrong frame", which is far harder to notice than a
// squash.
//
// So the assertions below are deliberately not "the constants are what I
// expect". They decode the real sheet and check that the VTT's rectangles
// tile that image exactly — cell size, stride, and final edge. A fix
// where the filter and the VTT disagree cannot pass, whichever half is
// wrong.
// ---------------------------------------------------------------------------

var xywhRe = regexp.MustCompile(`sprites\.jpg#xywh=(\d+),(\d+),(\d+),(\d+)`)

type spriteRect struct{ x, y, w, h int }

func parseSpriteVTT(t *testing.T, vtt string) []spriteRect {
	t.Helper()
	ms := xywhRe.FindAllStringSubmatch(vtt, -1)
	out := make([]spriteRect, 0, len(ms))
	for _, m := range ms {
		n := func(s string) int {
			v, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("vtt: bad number %q: %v", s, err)
			}
			return v
		}
		out = append(out, spriteRect{n(m[1]), n(m[2]), n(m[3]), n(m[4])})
	}
	return out
}

// assertVTTTilesSheet is the core guard. Given the decoded sheet size and
// the parsed rectangles it proves the VTT is a faithful description of the
// image: uniform cells, row-major stride, and a last cell whose far corner
// is the image's far corner — no slack, no overrun.
func assertVTTTilesSheet(t *testing.T, sheetW, sheetH int, rects []spriteRect) (cellW, cellH int) {
	t.Helper()
	if len(rects) == 0 {
		t.Fatal("vtt carried no #xywh cues")
	}
	if sheetW%spriteCols != 0 || sheetH%spriteRows != 0 {
		t.Fatalf("sheet %dx%d does not divide into a %dx%d grid",
			sheetW, sheetH, spriteCols, spriteRows)
	}
	cellW, cellH = sheetW/spriteCols, sheetH/spriteRows

	for i, r := range rects {
		wantX, wantY := (i%spriteCols)*cellW, (i/spriteCols)*cellH
		if r.w != cellW || r.h != cellH {
			t.Fatalf("cue %d states a %dx%d cell, but the sheet is %dx%d "+
				"so its cells are %dx%d — the VTT and the sheet have diverged (#761)",
				i, r.w, r.h, sheetW, sheetH, cellW, cellH)
		}
		if r.x != wantX || r.y != wantY {
			t.Fatalf("cue %d crops at (%d,%d), want (%d,%d) for a %dx%d cell",
				i, r.x, r.y, wantX, wantY, cellW, cellH)
		}
		if r.x+r.w > sheetW || r.y+r.h > sheetH {
			t.Fatalf("cue %d runs off the sheet: %d+%d x %d+%d exceeds %dx%d",
				i, r.x, r.w, r.y, r.h, sheetW, sheetH)
		}
	}

	// The last cell must land exactly on the sheet's far edge. This is
	// what separates "the arithmetic is self-consistent" from "the
	// arithmetic describes THIS image": a VTT built from stale constants
	// is internally consistent too, it just stops short of (or past) the
	// real edge.
	if len(rects) == spriteCols*spriteRows {
		last := rects[len(rects)-1]
		if last.x+last.w != sheetW || last.y+last.h != sheetH {
			t.Fatalf("last cue ends at (%d,%d) but the sheet is %dx%d — "+
				"the VTT does not tile the image",
				last.x+last.w, last.y+last.h, sheetW, sheetH)
		}
	}
	return cellW, cellH
}

func decodeSheetSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open sheet: %v", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode sheet config: %v", err)
	}
	return cfg.Width, cfg.Height
}

// ---------------------------------------------------------------------------
// The pure fit, including the degenerate and pathological inputs.
// ---------------------------------------------------------------------------

func TestSpriteCellSize(t *testing.T) {
	cases := []struct {
		name         string
		srcW, srcH   int
		wantW, wantH int
	}{
		// The common case must be byte-for-byte what shipped before
		// #761 — this is the no-regression pin.
		{"16:9 1080p", 1920, 1080, 160, 90},
		{"16:9 720p", 1280, 720, 160, 90},
		{"portrait 9:16", 1080, 1920, 90, 160},
		{"portrait 3:4", 1536, 2048, 120, 160},
		{"square", 1080, 1080, 160, 160},
		{"16:10", 1920, 1200, 160, 100},
		// Bounded on both axes: an ultrawide cannot produce a wide sheet
		// and a 1:8 tower cannot produce a tall one.
		{"ultrawide 32:9", 3840, 1080, 160, 46},
		{"1:8 tower", 200, 1600, 20, 160},
		// Pathological. The floor is 2 (ffmpeg's own force_divisible_by
		// floor), not 0 — a 0px cell would make the sheet undecodable.
		{"1000:1 banner", 10000, 10, 160, 2},
		{"1:1000 needle", 10, 10000, 2, 160},
		// Degenerate probe: no video stream / unparsed dimensions. Falls
		// back to the historical cell rather than dividing by zero.
		{"zero width", 0, 1080, 160, 90},
		{"zero height", 1920, 0, 160, 90},
		{"both zero", 0, 0, 160, 90},
		{"negative", -1920, -1080, 160, 90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := spriteCellSize(tc.srcW, tc.srcH)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("spriteCellSize(%d,%d) = %dx%d, want %dx%d",
					tc.srcW, tc.srcH, w, h, tc.wantW, tc.wantH)
			}
			if w%2 != 0 || h%2 != 0 {
				t.Errorf("spriteCellSize(%d,%d) = %dx%d — odd dimensions are "+
					"rejected by some encoders", tc.srcW, tc.srcH, w, h)
			}
			if w < 2 || h < 2 || w > spriteCellBox || h > spriteCellBox {
				t.Errorf("spriteCellSize(%d,%d) = %dx%d escapes [2,%d]",
					tc.srcW, tc.srcH, w, h, spriteCellBox)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The VTT is measured off the sheet, not recomputed alongside it.
//
// ffmpeg is faked at the exec boundary (it is not in the Go test image),
// and the fake writes a sheet of a size the TEST picked — not one derived
// from the handler's own geometry. That is the point: whatever bytes land
// on disk, the VTT must describe THEM. A handler that recomputed the cell
// size from the probe would sail through the portrait case here and still
// be wrong on any source whose real frame differs from its coded one.
// ---------------------------------------------------------------------------

// writeSpriteSheetJPEG paints a cols x rows grid where every cell is a
// distinct flat colour, so a crop rectangle can be checked for landing on
// the cell it claims — not merely for being the right size.
func writeSpriteSheetJPEG(t *testing.T, path string, sheetW, sheetH int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, sheetW, sheetH))
	cw, ch := sheetW/spriteCols, sheetH/spriteRows
	for row := 0; row < spriteRows; row++ {
		for col := 0; col < spriteCols; col++ {
			c := cellColor(col, row)
			for y := row * ch; y < (row+1)*ch; y++ {
				for x := col * cw; x < (col+1)*cw; x++ {
					img.Set(x, y, c)
				}
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	defer f.Close()
	// High quality: the colour-identity check below reads a pixel out of
	// a flat block, and the 25-step palette leaves ample headroom, but
	// there is no reason to fight the codec.
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode sheet: %v", err)
	}
}

// cellColor spaces the channels 25 apart — far outside JPEG's ringing on
// a flat block, so the nearest-step decode below is unambiguous.
func cellColor(col, row int) color.RGBA {
	return color.RGBA{R: uint8(col * 25), G: uint8(row * 25), B: 128, A: 255}
}

func cellIndexAt(t *testing.T, img image.Image, x, y int) (int, int) {
	t.Helper()
	r, g, _, _ := img.At(x, y).RGBA()
	step := func(v uint32) int { return int((float64(v>>8) + 12.5) / 25.0) }
	return step(r), step(g)
}

func TestWriteSprites_VTTDescribesTheSheetOnDisk(t *testing.T) {
	rig := newPreviewTestRig(t)

	cases := []struct {
		name           string
		sheetW, sheetH int
		probe          Probe
	}{
		// What the new filter really produces for a 1080x1920 source —
		// confirmed by TestWriteSprites_RealFFmpeg below.
		{"portrait", 900, 1600, Probe{DurationS: 30, Width: 1080, Height: 1920}},
		// The unchanged common case.
		{"landscape", 1600, 900, Probe{DurationS: 30, Width: 1920, Height: 1080}},
		{"square", 1600, 1600, Probe{DurationS: 30, Width: 1080, Height: 1080}},
		// The rotation trap: a phone clip whose coded frame is landscape
		// but which decodes portrait. The probe says 16:9, the sheet is
		// 9:16. Only a handler that measures the sheet gets this right.
		{"rotated source", 900, 1600, Probe{DurationS: 30, Width: 1920, Height: 1080}},
		// No usable probe dimensions at all — the sheet is still the
		// authority.
		{"degenerate probe", 900, 1600, Probe{DurationS: 30}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, hash := rig.seedPreviewAsset(t, "mp4", []byte("stand-in; the ffmpeg shim doesn't read it"))

			h := NewVideoHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
			shimDir := t.TempDir()
			fixture := filepath.Join(shimDir, "sheet-fixture.jpg")
			writeSpriteSheetJPEG(t, fixture, tc.sheetW, tc.sheetH)
			argsLog := filepath.Join(shimDir, "ffmpeg-args.txt")
			h.FFmpegPath = writeShim(t, shimDir, "ffmpeg", `
printf '%s\n' "$@" > `+argsLog+`
for last; do :; done
cp `+fixture+` "$last"
`)

			work := workDir{dir: t.TempDir(), sourcePath: filepath.Join(t.TempDir(), "src.mp4")}
			if err := h.writeSprites(t.Context(), work, hash, tc.probe); err != nil {
				t.Fatalf("writeSprites: %v", err)
			}

			// The filter must ASK ffmpeg to preserve the aspect ratio. The
			// tiling assertions below cannot see this on their own, because
			// the shim ignores the filter — and a regression that reverts
			// to `scale=160:90` would still produce a self-consistent VTT.
			args, err := os.ReadFile(argsLog)
			if err != nil {
				t.Fatalf("read shim args: %v", err)
			}
			wantFilter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2",
				spriteCellBox, spriteCellBox)
			if !regexp.MustCompile(regexp.QuoteMeta(wantFilter)).Match(args) {
				t.Fatalf("ffmpeg filter does not fit the source into the %dpx box; got:\n%s",
					spriteCellBox, args)
			}

			sheetPath := filepath.Join(work.dir, "sprites.jpg")
			sheetW, sheetH := decodeSheetSize(t, sheetPath)
			if sheetW != tc.sheetW || sheetH != tc.sheetH {
				t.Fatalf("sheet on disk is %dx%d, fixture was %dx%d", sheetW, sheetH, tc.sheetW, tc.sheetH)
			}

			vtt, err := os.ReadFile(filepath.Join(work.dir, "sprites.vtt"))
			if err != nil {
				t.Fatalf("read vtt: %v", err)
			}
			rects := parseSpriteVTT(t, string(vtt))
			if len(rects) != spriteCols*spriteRows {
				t.Fatalf("got %d cues, want %d (a 30s clip fills the grid)",
					len(rects), spriteCols*spriteRows)
			}
			cellW, cellH := assertVTTTilesSheet(t, sheetW, sheetH, rects)

			// Shape sanity on top of the tiling: a portrait sheet must
			// yield portrait cells.
			if (tc.sheetH > tc.sheetW) != (cellH > cellW) {
				t.Fatalf("sheet %dx%d produced %dx%d cells — orientation inverted",
					sheetW, sheetH, cellW, cellH)
			}

			// Content check: cue i must crop the cell painted for
			// position i. Size-only assertions pass a stride bug where
			// every thumbnail shows a neighbouring frame.
			f, err := os.Open(sheetPath)
			if err != nil {
				t.Fatalf("reopen sheet: %v", err)
			}
			defer f.Close()
			img, err := jpeg.Decode(f)
			if err != nil {
				t.Fatalf("decode sheet: %v", err)
			}
			for i, r := range rects {
				cx, cy := r.x+r.w/2, r.y+r.h/2
				gotCol, gotRow := cellIndexAt(t, img, cx, cy)
				wantCol, wantRow := i%spriteCols, i/spriteCols
				if gotCol != wantCol || gotRow != wantRow {
					t.Fatalf("cue %d crops the cell at grid (%d,%d), want (%d,%d) — "+
						"the crop rectangles are off the cell boundaries",
						i, gotCol, gotRow, wantCol, wantRow)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The end-to-end truth: real ffmpeg, real sources, real sheets.
//
// Skipped where ffmpeg is absent (the Go test image), which is why the
// shim test above exists as the always-on guard. Run it on a host with
// ffmpeg to prove the filter string actually does what the shim only
// assumes.
// ---------------------------------------------------------------------------

func TestWriteSprites_RealFFmpeg(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; the shim test covers the VTT/sheet contract")
	}
	rig := newPreviewTestRig(t)

	// 25s so the 0.2s interval floor doesn't truncate the cue list —
	// interval is duration/100, and the last-cell edge assertion needs a
	// full 10x10 grid.
	const durationS = 25

	cases := []struct {
		name                   string
		srcW, srcH             int
		wantSheetW, wantSheetH int
	}{
		{"landscape 16:9 is unchanged", 640, 360, 1600, 900},
		{"portrait 9:16", 360, 640, 900, 1600},
		{"square", 480, 480, 1600, 1600},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := t.TempDir()
			src := filepath.Join(srcDir, "src.mp4")
			gen := exec.CommandContext(t.Context(), bin,
				"-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi",
				"-i", fmt.Sprintf("testsrc=size=%dx%d:rate=10:duration=%d", tc.srcW, tc.srcH, durationS),
				"-c:v", "mpeg4", "-q:v", "5",
				src,
			)
			if out, err := gen.CombinedOutput(); err != nil {
				t.Fatalf("generate source: %v: %s", err, out)
			}

			_, hash := rig.seedPreviewAsset(t, "mp4", []byte("real ffmpeg run; bytes unused"))
			h := NewVideoHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
			work := workDir{dir: t.TempDir(), sourcePath: src}
			probe := Probe{DurationS: durationS, Width: tc.srcW, Height: tc.srcH}
			if err := h.writeSprites(t.Context(), work, hash, probe); err != nil {
				t.Fatalf("writeSprites: %v", err)
			}

			sheetW, sheetH := decodeSheetSize(t, filepath.Join(work.dir, "sprites.jpg"))
			if sheetW != tc.wantSheetW || sheetH != tc.wantSheetH {
				t.Fatalf("a %dx%d source produced a %dx%d sheet, want %dx%d",
					tc.srcW, tc.srcH, sheetW, sheetH, tc.wantSheetW, tc.wantSheetH)
			}

			vtt, err := os.ReadFile(filepath.Join(work.dir, "sprites.vtt"))
			if err != nil {
				t.Fatalf("read vtt: %v", err)
			}
			rects := parseSpriteVTT(t, string(vtt))
			if len(rects) != spriteCols*spriteRows {
				t.Fatalf("got %d cues, want %d", len(rects), spriteCols*spriteRows)
			}
			cellW, cellH := assertVTTTilesSheet(t, sheetW, sheetH, rects)
			t.Logf("%dx%d source -> %dx%d sheet, %dx%d cells", tc.srcW, tc.srcH, sheetW, sheetH, cellW, cellH)
		})
	}
}
