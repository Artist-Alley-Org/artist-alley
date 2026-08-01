// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
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
// Grid-agnostic so the 3D turntable's 6x6 sheet gets the same guard as
// the video scrub's 10x10 (#811).
func assertVTTTilesSheet(t *testing.T, sheetW, sheetH, cols, rows int, rects []spriteRect) (cellW, cellH int) {
	t.Helper()
	if len(rects) == 0 {
		t.Fatal("vtt carried no #xywh cues")
	}
	if sheetW%cols != 0 || sheetH%rows != 0 {
		t.Fatalf("sheet %dx%d does not divide into a %dx%d grid",
			sheetW, sheetH, cols, rows)
	}
	cellW, cellH = sheetW/cols, sheetH/rows

	for i, r := range rects {
		wantX, wantY := (i%cols)*cellW, (i/cols)*cellH
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
	if len(rects) == cols*rows {
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
		// These are what spriteCellSize computes: fit the box, then round
		// the edge UP to even.
		//
		// They are deliberately NOT claimed to be byte-identical to
		// ffmpeg's own output. At spriteCellBox=160 every case here was
		// exact, so the question never came up; at 240 a 16:9 source
		// computes 135, and which even neighbour `force_divisible_by=2`
		// picks turns out to be ffmpeg-VERSION dependent — 5.1 (the
		// runtime image) rounds down to 134, 6.1 rounds up to 136, on the
		// same filter string and the same source. Measured on a real
		// stack while verifying #811.
		//
		// That is precisely why the VTT measures the generated sheet
		// (#796) instead of recomputing the cell from the probe: this
		// function is the fallback for when there is no sheet to measure,
		// and being a pixel off on a path that is already degraded beats
		// pinning the suite to one ffmpeg build.
		{"16:9 1080p", 1920, 1080, 240, 136},
		{"16:9 720p", 1280, 720, 240, 136},
		{"portrait 9:16", 1080, 1920, 136, 240},
		{"portrait 3:4", 1536, 2048, 180, 240},
		{"square", 1080, 1080, 240, 240},
		{"16:10", 1920, 1200, 240, 150},
		// Bounded on both axes: an ultrawide cannot produce a wide sheet
		// and a 1:8 tower cannot produce a tall one.
		{"ultrawide 32:9", 3840, 1080, 240, 68},
		{"1:8 tower", 200, 1600, 30, 240},
		// Pathological. The floor is 2 (ffmpeg's own force_divisible_by
		// floor), not 0 — a 0px cell would make the sheet undecodable.
		{"1000:1 banner", 10000, 10, 240, 2},
		{"1:1000 needle", 10, 10000, 2, 240},
		// Degenerate probe: no video stream / unparsed dimensions. Falls
		// back to the 16:9 fit of the box rather than dividing by zero.
		{"zero width", 0, 1080, 240, 136},
		{"zero height", 1920, 0, 240, 136},
		{"both zero", 0, 0, 240, 136},
		{"negative", -1920, -1080, 240, 136},
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
// #811 — the degenerate-probe fallback must track spriteCellBox.
//
// It is the one cell size in this file that is not computed from the box
// at call time, so it is the one that goes stale when the box moves. It
// shipped as a literal `160x90` and would have survived the 160 -> 240
// change unnoticed: nothing else in the package reads it, and the only
// way to reach it in production is a container ffprobe finds no video
// stream in — rare enough that a wrong crop rectangle there would sit
// undetected indefinitely.
//
// Asserting the RELATIONSHIP rather than the numbers means the next
// change to the box cannot reintroduce the staleness.
// ---------------------------------------------------------------------------

func TestSpriteFallbackTracksTheBox(t *testing.T) {
	// A 16:9 probe and no probe at all must produce the same cell —
	// 16:9 is precisely what the fallback stands in for.
	wantW, wantH := spriteCellSize(1920, 1080)
	gotW, gotH := spriteCellSize(0, 0)
	if gotW != wantW || gotH != wantH {
		t.Errorf("degenerate probe gives a %dx%d cell but a 16:9 probe gives %dx%d — "+
			"the fallback is a stale literal, not a fit of spriteCellBox=%d",
			gotW, gotH, wantW, wantH, spriteCellBox)
	}
	if spriteFallbackW != spriteCellBox {
		t.Errorf("spriteFallbackW = %d, want spriteCellBox = %d — the long edge of a "+
			"16:9 fit is the box itself", spriteFallbackW, spriteCellBox)
	}
	if spriteFallbackW*9 != spriteFallbackH*16 {
		t.Errorf("fallback %dx%d is not 16:9", spriteFallbackW, spriteFallbackH)
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
		// The shim writes a sheet of the size the TEST names, so these are
		// plausible real sheets rather than derived ones. Portrait first,
		// because that is the shape #761 was about.
		{"portrait", 1360, 2400, Probe{DurationS: 30, Width: 1080, Height: 1920}},
		// The common case.
		{"landscape", 2400, 1360, Probe{DurationS: 30, Width: 1920, Height: 1080}},
		{"square", 2400, 2400, Probe{DurationS: 30, Width: 1080, Height: 1080}},
		// The rotation trap: a phone clip whose coded frame is landscape
		// but which decodes portrait. The probe says 16:9, the sheet is
		// 9:16. Only a handler that measures the sheet gets this right.
		{"rotated source", 1360, 2400, Probe{DurationS: 30, Width: 1920, Height: 1080}},
		// No usable probe dimensions at all — the sheet is still the
		// authority.
		{"degenerate probe", 1360, 2400, Probe{DurationS: 30}},
		// A sheet at the OLD cell size. The shim writes whatever the test
		// asks for, so this stands in for a sheet rendered before #811
		// and not yet re-rendered: the VTT must describe the bytes that
		// exist, not the constant the binary now carries.
		{"pre-#811 sheet", 1600, 900, Probe{DurationS: 30, Width: 1920, Height: 1080}},
		// The same 16:9 source as "landscape", but the height ffmpeg 5.1
		// actually produces (1340, cell 240x134) rather than the 1360 of
		// ffmpeg 6.1. Both are real; the handler must not care which.
		{"landscape, other ffmpeg rounding", 2400, 1340, Probe{DurationS: 30, Width: 1920, Height: 1080}},
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
			cellW, cellH := assertVTTTilesSheet(t, sheetW, sheetH, spriteCols, spriteRows, rects)

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

	// The assertions are on the SHAPE of the sheet, not on two literal
	// numbers, because the numbers are not portable (#811). A 16:9 source
	// in a 240 box computes 240x135, and `force_divisible_by=2` resolves
	// that odd edge differently per ffmpeg build: 5.1 (the runtime image)
	// produces a 2400x1340 sheet, 6.1 produces 2400x1360, same filter
	// string, same source. Pinning either number turns this test into a
	// report on which ffmpeg the runner has.
	//
	// What must hold on every build is what the frontend actually
	// depends on: the box bounds both axes, the long edge is the box, the
	// cell keeps the source's aspect ratio, and the VTT tiles the sheet
	// exactly. All four are checked below.
	cases := []struct {
		name       string
		srcW, srcH int
	}{
		{"landscape 16:9", 640, 360},
		{"portrait 9:16", 360, 640},
		{"square", 480, 480},
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

			vtt, err := os.ReadFile(filepath.Join(work.dir, "sprites.vtt"))
			if err != nil {
				t.Fatalf("read vtt: %v", err)
			}
			rects := parseSpriteVTT(t, string(vtt))
			if len(rects) != spriteCols*spriteRows {
				t.Fatalf("got %d cues, want %d", len(rects), spriteCols*spriteRows)
			}
			cellW, cellH := assertVTTTilesSheet(t, sheetW, sheetH, spriteCols, spriteRows, rects)

			// The #761 property, restated for the bigger box (#811): the
			// cell keeps the SOURCE's aspect ratio. `force_divisible_by=2`
			// can move an edge by one pixel, so allow 2% — enough to
			// absorb the rounding, nowhere near enough to hide a squash
			// (the original bug turned 0.56 into 1.78).
			srcAR := float64(tc.srcW) / float64(tc.srcH)
			cellAR := float64(cellW) / float64(cellH)
			if cellAR < srcAR*0.98 || cellAR > srcAR*1.02 {
				t.Errorf("a %dx%d source (AR %.4f) produced %dx%d cells (AR %.4f) — squashed",
					tc.srcW, tc.srcH, srcAR, cellW, cellH, cellAR)
			}
			// The box bounds both axes, and the source's LONG edge is
			// spent in full — a cell that fits but wastes half the box is
			// the regression this issue was about.
			if cellW > spriteCellBox || cellH > spriteCellBox {
				t.Errorf("%dx%d cell escapes the %dpx box", cellW, cellH, spriteCellBox)
			}
			if long := max(cellW, cellH); long != spriteCellBox {
				t.Errorf("a %dx%d source produced a %dx%d cell — the long edge is %d, "+
					"not the %dpx box, so the sheet is smaller than it was asked for",
					tc.srcW, tc.srcH, cellW, cellH, long, spriteCellBox)
			}

			t.Logf("%dx%d source -> %dx%d sheet, %dx%d cells", tc.srcW, tc.srcH, sheetW, sheetH, cellW, cellH)
		})
	}
}

// ---------------------------------------------------------------------------
// #811 — the 3D turntable sheet.
//
// The turntable half of the scrub had no geometry test at all: the video
// path is guarded end-to-end, the model path was guarded by nothing but
// the constant's own comment. Both halves moved 160 -> 240 together, so
// both halves get the same proof — sheet dimensions read off the bytes
// that were stored, and cues that tile them.
//
// This one also checks the SOURCE frames are larger than the cell. The
// worker renders at 512px, so 240 is still a downscale; had it not been,
// the change would have bought blur instead of detail.
// ---------------------------------------------------------------------------

func TestModelWriteSprites_SheetGeometry(t *testing.T) {
	rig := newPreviewTestRig(t)
	_, hash := rig.seedPreviewAsset(t, "glb", []byte("stand-in; writeSprites reads the frames dir"))

	const frameRes = 512 // ModelHandler.Res default
	if frameRes <= modelSpriteCell {
		t.Fatalf("turntable frames render at %d but cells are %d — the sheet would be upscaled",
			frameRes, modelSpriteCell)
	}

	framesDir := t.TempDir()
	total := modelSpriteCols * modelSpriteRows
	for i := 0; i < total; i++ {
		img := image.NewRGBA(image.Rect(0, 0, frameRes, frameRes))
		c := cellColor(i%modelSpriteCols, i/modelSpriteCols)
		for y := 0; y < frameRes; y++ {
			for x := 0; x < frameRes; x++ {
				img.Set(x, y, c)
			}
		}
		f, err := os.Create(filepath.Join(framesDir, fmt.Sprintf("frame_%04d.png", i)))
		if err != nil {
			t.Fatalf("create frame %d: %v", i, err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatalf("encode frame %d: %v", i, err)
		}
		_ = f.Close()
	}

	h := NewModelHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	if err := h.writeSprites(t.Context(), hash, framesDir); err != nil {
		t.Fatalf("writeSprites: %v", err)
	}

	sheetBytes := readStoredVariant(t, rig, hash, "sprites.jpg")
	cfg, _, err := image.DecodeConfig(bytes.NewReader(sheetBytes))
	if err != nil {
		t.Fatalf("decode stored sheet: %v", err)
	}
	wantEdge := modelSpriteCols * modelSpriteCell
	if cfg.Width != wantEdge || cfg.Height != wantEdge {
		t.Fatalf("turntable sheet is %dx%d, want %dx%d (%d cells x %dpx)",
			cfg.Width, cfg.Height, wantEdge, wantEdge, modelSpriteCols, modelSpriteCell)
	}

	rects := parseSpriteVTT(t, string(readStoredVariant(t, rig, hash, "sprites.vtt")))
	if len(rects) != total {
		t.Fatalf("got %d cues, want %d — the frame count moved", len(rects), total)
	}
	assertVTTTilesSheet(t, cfg.Width, cfg.Height, modelSpriteCols, modelSpriteRows, rects)

	// Content: cue i must crop the frame painted for position i, not a
	// neighbour. A size-only check passes a stride bug.
	img, err := jpeg.Decode(bytes.NewReader(sheetBytes))
	if err != nil {
		t.Fatalf("decode sheet: %v", err)
	}
	for i, r := range rects {
		gotCol, gotRow := cellIndexAt(t, img, r.x+r.w/2, r.y+r.h/2)
		if wantCol, wantRow := i%modelSpriteCols, i/modelSpriteCols; gotCol != wantCol || gotRow != wantRow {
			t.Fatalf("cue %d crops grid (%d,%d), want (%d,%d)", i, gotCol, gotRow, wantCol, wantRow)
		}
	}

	t.Logf("turntable sheet %dx%d, %d cells of %dpx, %d bytes",
		cfg.Width, cfg.Height, total, modelSpriteCell, len(sheetBytes))
}

func readStoredVariant(t *testing.T, rig *previewTestRig, hash, variant string) []byte {
	t.Helper()
	rc, _, err := rig.storage.Backend.Get(t.Context(), hash, variant)
	if err != nil {
		t.Fatalf("get %s: %v", variant, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", variant, err)
	}
	return b
}
