// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
)

// ---------------------------------------------------------------------------
// #827 — reconcile on skip.
//
// The state under test is not "a job ran twice". It is bytes on the
// storage backend with NO storage_variants row describing them, which is
// what recreating the database over a surviving storage volume produces
// — i.e. what restoring a backup produces. In that state every preview
// job finds its bytes, skips, and reports `done`, while the API's
// preview_available (DB-first, by design: the client must never request
// bytes the server did not announce) says false for every asset in the
// install, and every card stays blurred forever.
//
// So the tests below BUILD that state directly and assert the row comes
// back. "The job reported done" is what the bug already does.
// ---------------------------------------------------------------------------

// previewAvailableSQL is the exact predicate assets.enrichAssetDerived
// computes preview_available from, and list_page.go joins. Duplicated
// here on purpose: asserting against the real expression is what makes
// this test about the user-visible flag rather than about a row.
const previewAvailableSQL = `SELECT EXISTS (
	SELECT 1 FROM storage_variants WHERE object_hash = $1 AND variant_key = 'col'
)`

func (r *previewTestRig) previewAvailable(t *testing.T, hash string) bool {
	t.Helper()
	var ok bool
	if err := r.pool.QueryRow(context.Background(), previewAvailableSQL, hash).Scan(&ok); err != nil {
		t.Fatalf("preview_available probe: %v", err)
	}
	return ok
}

// dropVariantRowsKeepingBytes is the split brain, constructed. Nothing
// is removed from the backend — only the database's record of it.
func (r *previewTestRig) dropVariantRowsKeepingBytes(t *testing.T, hash string) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`DELETE FROM storage_variants WHERE object_hash = $1`, hash); err != nil {
		t.Fatalf("drop variant rows: %v", err)
	}
}

func (r *previewTestRig) variantRows(t *testing.T, hash string) map[string]string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(),
		`SELECT variant_key, content_type FROM storage_variants WHERE object_hash = $1`, hash)
	if err != nil {
		t.Fatalf("list variant rows: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, ct string
		if err := rows.Scan(&k, &ct); err != nil {
			t.Fatalf("scan variant row: %v", err)
		}
		out[k] = ct
	}
	return out
}

// runTextPreview drives a real jobs.Handler end to end. The text handler
// is the one preview pipeline with no external binary at all — it
// renders its card in pure Go — so it exercises the shared ladder and
// skip paths without a shim standing between the test and the code.
func runTextPreview(t *testing.T, rig *previewTestRig, assetID uuid.UUID, hash string, force bool) TextResult {
	t.Helper()
	h := NewTextHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	payload, err := json.Marshal(dispatch.NewPayload(assetID, hash, ptr("txt"), force))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	raw, err := h.Handle(t.Context(), &jobs.Claim{
		ID: uuid.New(), Type: h.Type(), Payload: payload, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("text preview: %v", err)
	}
	var out TextResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func TestReconcileOnSkip_HealsSplitBrainVariantRows(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "txt",
		[]byte("the quick brown fox\njumps over the lazy dog\n"))

	// 1. A normal render. Bytes AND rows.
	runTextPreview(t, rig, assetID, hash, false)
	before := rig.variantRows(t, hash)
	if _, ok := before["col"]; !ok {
		t.Fatalf("first render produced no col row; got %v", before)
	}
	if !rig.previewAvailable(t, hash) {
		t.Fatal("preview_available is false straight after a successful render")
	}

	// 2. The database is recreated; the storage volume is not.
	rig.dropVariantRowsKeepingBytes(t, hash)
	if rig.previewAvailable(t, hash) {
		t.Fatal("test setup did not actually produce the split-brain state")
	}
	if _, err := rig.storage.Backend.Stat(t.Context(), hash, "col"); err != nil {
		t.Fatalf("setup removed the BYTES too; the bug under test is bytes-without-rows: %v", err)
	}

	// 3. Re-queue, unforced — the ordinary operator gesture, and the
	//    path that used to report `done` and change nothing.
	res := runTextPreview(t, rig, assetID, hash, false)
	if len(res.Skipped) == 0 {
		t.Fatalf("expected the ladder to SKIP (bytes are present), got variants=%v skipped=%v",
			res.Variants, res.Skipped)
	}

	after := rig.variantRows(t, hash)
	for key := range before {
		if _, ok := after[key]; !ok {
			t.Errorf("variant row %q was not reconciled; rows now: %v", key, after)
		}
	}
	if !rig.previewAvailable(t, hash) {
		t.Fatal("preview_available is STILL false after a skipping preview job — " +
			"the skip path did not heal the row (#827)")
	}
}

// The content type a reconcile records must be the one the renderer
// recorded, not the one Stat reports.
//
// This is the part of #827 that the brief got wrong: "the Stat it
// already performed has what uploadFile would have written" is true of
// the size and false of the content type. The FS backend — the default,
// and what dev and CI both run — has nowhere to keep a content type and
// answers "application/octet-stream" for every object it stats. Trusting
// it would file every healed thumbnail under the wrong type, and
// storage_variants.content_type is exactly what the admin storage
// breakdown groups on.
func TestReconcileOnSkip_RecordsTheRenderersContentTypeNotStats(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "txt", []byte("content types matter\n"))

	runTextPreview(t, rig, assetID, hash, false)
	want := rig.variantRows(t, hash)

	rig.dropVariantRowsKeepingBytes(t, hash)
	runTextPreview(t, rig, assetID, hash, false)
	got := rig.variantRows(t, hash)

	for key, wantCT := range want {
		gotCT, ok := got[key]
		if !ok {
			t.Errorf("row %q missing after reconcile", key)
			continue
		}
		if gotCT != wantCT {
			t.Errorf("reconciled %q as %q, the renderer wrote %q", key, gotCT, wantCT)
		}
		if gotCT == "application/octet-stream" {
			t.Errorf("row %q reconciled to the FS backend's placeholder type", key)
		}
	}
}

// reconcileLadderRows must heal EVERY anchor rung, not just `col`.
//
// This is the 3D handler's case, found on a live split-brain restore:
// its re-queue sentinel asks about `col`, the sprite sheet, a turntable
// frame, a view and the iso source, and never about `preview`, `screen`
// or `hires`. Healing only what the sentinel asked about brought 26
// assets back with preview_available true and ladder_available false —
// bytes for all four rungs, rows for one.
func TestReconcileLadderRows_HealsEveryAnchorRungNotJustCol(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "txt", []byte("four rungs\n"))
	runTextPreview(t, rig, assetID, hash, false)

	rig.dropVariantRowsKeepingBytes(t, hash)
	reconcileLadderRows(t.Context(), rig.storage, hash)

	got := rig.variantRows(t, hash)
	for _, key := range ladderAnchorRungs {
		if _, ok := got[key]; !ok {
			t.Errorf("anchor rung %q not reconciled; rows: %v", key, got)
		}
	}
}

// A reconcile must not resurrect a row for an object the database has
// never heard of: storage_variants has an FK to storage_objects, and a
// preview job that was only trying to SKIP must not fail on a constraint.
func TestReconcileVariant_DeclinesWhenTheObjectRowIsGone(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "txt", []byte("orphaned bytes\n"))
	runTextPreview(t, rig, assetID, hash, false)

	rig.dropVariantRowsKeepingBytes(t, hash)
	if _, err := rig.pool.Exec(t.Context(),
		`DELETE FROM assets WHERE id = $1`, assetID); err != nil {
		t.Fatalf("delete asset: %v", err)
	}
	if _, err := rig.pool.Exec(t.Context(),
		`DELETE FROM storage_objects WHERE hash = $1`, hash); err != nil {
		t.Fatalf("delete storage_object: %v", err)
	}

	healed, err := rig.storage.ReconcileVariant(t.Context(), hash, "col", nil)
	if err != nil {
		t.Fatalf("reconcile against a missing object row must not error: %v", err)
	}
	if healed {
		t.Error("reconcile inserted a variant row for an object that does not exist")
	}
}

// The thumbhash half of #827.
//
// The existing preview.thumbhash_backfill sweep cannot reach these
// assets: its population is `EXISTS (… storage_variants … 'col')`, so in
// the split-brain state it selects nothing and reports a clean run. The
// per-asset heal on the skip path is what closes them.
func TestReconcileOnSkip_BackfillsThumbhashFromAStoredRung(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "txt", []byte("blur me\n"))

	runTextPreview(t, rig, assetID, hash, false)
	if rig.thumbhashOf(t, assetID) == nil {
		t.Fatal("first render stamped no thumbhash; the rest of this test is meaningless")
	}

	// Lose both the rows and the hash, keep the bytes.
	rig.dropVariantRowsKeepingBytes(t, hash)
	if _, err := rig.pool.Exec(t.Context(),
		`UPDATE assets SET thumbhash = NULL WHERE id = $1`, assetID); err != nil {
		t.Fatalf("clear thumbhash: %v", err)
	}

	res := runTextPreview(t, rig, assetID, hash, false)
	if len(res.Skipped) == 0 {
		t.Fatalf("expected a skip, got variants=%v", res.Variants)
	}
	if rig.thumbhashOf(t, assetID) == nil {
		t.Error("thumbhash was not backfilled on the skip path — the card will " +
			"flash blank instead of fading up (#827/#645)")
	}
}

// ---------------------------------------------------------------------------
// #810 — the poster must not be a black frame.
// ---------------------------------------------------------------------------

func TestPosterOffsets_AreFractionsOfDurationNotAFlatSecond(t *testing.T) {
	cases := []struct {
		name  string
		dur   float64
		first float64
	}{
		// 14m48s. The flat 1.0s rule put the poster inside sintel's
		// fade-in and measured 0.0 mean luma — a literally black card.
		{"feature length", 888, 88.8},
		{"short clip", 3, 0.3},
		{"unmeasurable", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := posterOffsets(tc.dur)
			if len(got) == 0 {
				t.Fatal("no candidate offsets")
			}
			if diff := got[0] - tc.first; diff > 0.001 || diff < -0.001 {
				t.Errorf("first offset = %.3f, want %.3f", got[0], tc.first)
			}
			if tc.dur > 0 && len(got) < 2 {
				t.Error("a single candidate cannot recover from landing on a dark frame")
			}
			for i := 1; i < len(got); i++ {
				if got[i] <= got[i-1] {
					t.Errorf("offsets are not increasing: %v", got)
				}
			}
		})
	}
}

func TestMeanLuma_SeparatesABlackCardFromAPicture(t *testing.T) {
	black := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			black.Set(x, y, color.RGBA{A: 255})
		}
	}
	if l := meanLuma(black); l >= posterMinMeanLuma {
		t.Errorf("an all-black frame measured %.4f, at or above the %.4f floor", l, posterMinMeanLuma)
	}

	mid := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			mid.Set(x, y, color.RGBA{R: 90, G: 120, B: 60, A: 255})
		}
	}
	if l := meanLuma(mid); l < posterMinMeanLuma {
		t.Errorf("an ordinary mid-tone frame measured %.4f, below the %.4f floor", l, posterMinMeanLuma)
	}
}

// The ffmpeg invocation itself, checked at the exec boundary so it runs
// in CI where ffmpeg is absent.
//
// Two things must be true of it and neither is visible in the produced
// bytes when a shim writes them: the seek must be a fraction of the
// duration (never the old flat 1.0), and the frame must be chosen by the
// `thumbnail` filter rather than taken blind.
func TestSelectPoster_AsksFFmpegForAScoredFrameAtAFraction(t *testing.T) {
	rig := newPreviewTestRig(t)
	shimDir := t.TempDir()
	fixture := filepath.Join(shimDir, "frame.jpg")
	writeFlatJPEG(t, fixture, 640, 360, color.RGBA{R: 120, G: 130, B: 110, A: 255})
	argsLog := filepath.Join(shimDir, "args.txt")

	h := NewVideoHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.FFmpegPath = writeShim(t, shimDir, "ffmpeg", `
printf '%s\n' "$@" >> `+argsLog+`
for last; do :; done
cp `+fixture+` "$last"
`)

	work := workDir{dir: t.TempDir(), sourcePath: filepath.Join(t.TempDir(), "src.mkv")}
	pick, err := h.selectPoster(t.Context(), work, Probe{DurationS: 888, Width: 1920, Height: 1080})
	if err != nil {
		t.Fatalf("selectPoster: %v", err)
	}
	if pick.tries != 1 {
		t.Errorf("a bright first candidate should end the search, took %d tries", pick.tries)
	}
	if pick.atS < 80 || pick.atS > 100 {
		t.Errorf("poster taken at %.1fs of an 888s clip; expected ~10%%", pick.atS)
	}

	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read shim args: %v", err)
	}
	if !strings.Contains(string(args), fmt.Sprintf("thumbnail=%d", posterThumbnailFrames)) {
		t.Errorf("ffmpeg was not asked to score a window of frames; args:\n%s", args)
	}
	if strings.Contains(string(args), "\n1.000\n") {
		t.Errorf("the flat one-second seek is back; args:\n%s", args)
	}
}

// The retry, driven at the exec boundary: a shim that hands back a black
// frame for the first two offsets and a picture for the third must end on
// the picture.
func TestSelectPoster_SeeksPastAFadeFromBlack(t *testing.T) {
	rig := newPreviewTestRig(t)
	shimDir := t.TempDir()
	dark := filepath.Join(shimDir, "dark.jpg")
	bright := filepath.Join(shimDir, "bright.jpg")
	writeFlatJPEG(t, dark, 320, 180, color.RGBA{R: 2, G: 2, B: 2, A: 255})
	writeFlatJPEG(t, bright, 320, 180, color.RGBA{R: 140, G: 150, B: 130, A: 255})
	counter := filepath.Join(shimDir, "n")

	h := NewVideoHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.FFmpegPath = writeShim(t, shimDir, "ffmpeg", `
n=$(cat `+counter+` 2>/dev/null || echo 0)
n=$((n+1))
echo $n > `+counter+`
for last; do :; done
if [ "$n" -ge 3 ]; then cp `+bright+` "$last"; else cp `+dark+` "$last"; fi
`)

	work := workDir{dir: t.TempDir(), sourcePath: filepath.Join(t.TempDir(), "src.mkv")}
	pick, err := h.selectPoster(t.Context(), work, Probe{DurationS: 600})
	if err != nil {
		t.Fatalf("selectPoster: %v", err)
	}
	if pick.luma < posterMinMeanLuma {
		t.Errorf("settled on a frame at %.4f mean luma, below the %.4f floor", pick.luma, posterMinMeanLuma)
	}
	if pick.tries != 3 {
		t.Errorf("took %d tries, expected to walk all three offsets", pick.tries)
	}
}

// A clip that is dark from end to end still gets a poster. Refusing to
// produce one would trade a dark card for no card.
func TestSelectPoster_StillProducesAPosterForAnAllDarkClip(t *testing.T) {
	rig := newPreviewTestRig(t)
	shimDir := t.TempDir()
	dark := filepath.Join(shimDir, "dark.jpg")
	writeFlatJPEG(t, dark, 320, 180, color.RGBA{R: 3, G: 3, B: 3, A: 255})

	h := NewVideoHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.FFmpegPath = writeShim(t, shimDir, "ffmpeg", `
for last; do :; done
cp `+dark+` "$last"
`)
	work := workDir{dir: t.TempDir(), sourcePath: filepath.Join(t.TempDir(), "src.mkv")}
	pick, err := h.selectPoster(t.Context(), work, Probe{DurationS: 120})
	if err != nil {
		t.Fatalf("an all-dark clip must still yield a poster: %v", err)
	}
	if pick.img == nil {
		t.Fatal("no image returned")
	}
}

// The end-to-end truth: real ffmpeg, a real fade-from-black, real
// rendered variants. Skipped where ffmpeg is absent (the Go test image),
// which is why the shim tests above exist — but this is the one that
// proves the filter string does what the shims only assume.
func TestSelectPoster_RealFFmpeg_FadeInIsNotChosen(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; the shim tests cover the invocation contract")
	}
	rig := newPreviewTestRig(t)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.mp4")

	// 12 seconds: black for the first 4, a bright moving pattern after.
	// This is sintel's shape in miniature — at the old flat 1.0s offset
	// the poster is inside the black.
	if out, err := exec.Command(bin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=640x360:d=4:r=24",
		"-f", "lavfi", "-i", "testsrc2=s=640x360:d=8:r=24",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]",
		"-map", "[v]", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		srcPath,
	).CombinedOutput(); err != nil {
		t.Skipf("could not synthesise a test clip: %v\n%s", err, out)
	}

	h := NewVideoHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	work := workDir{dir: dir, sourcePath: srcPath}

	// What the old rule produced, measured, so the assertion below is a
	// comparison and not an article of faith. Spelled out rather than
	// routed through extractFrame, because extractFrame now carries the
	// thumbnail filter that is half of the fix.
	oldPath := filepath.Join(dir, "old.jpg")
	if out, err := exec.Command(bin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", "1.000", "-i", srcPath,
		"-frames:v", "1", "-vf", "scale='min(4096,iw)':'-2'", "-q:v", "2",
		oldPath,
	).CombinedOutput(); err != nil {
		t.Fatalf("extract at the old offset: %v\n%s", err, out)
	}
	oldImg, err := decodeImageFile(oldPath)
	if err != nil {
		t.Fatalf("decode old frame: %v", err)
	}
	oldLuma := meanLuma(oldImg)
	if oldLuma >= posterMinMeanLuma {
		t.Skipf("the synthesised clip is not dark at t=1.0s (%.4f); nothing to prove", oldLuma)
	}

	pick, err := h.selectPoster(t.Context(), work, Probe{DurationS: 12, Width: 640, Height: 360})
	if err != nil {
		t.Fatalf("selectPoster: %v", err)
	}
	if pick.luma < posterMinMeanLuma {
		t.Errorf("poster mean luma %.4f (old rule: %.4f) — still a black card", pick.luma, oldLuma)
	}
	if pick.atS <= 1.0 {
		t.Errorf("poster taken at %.3fs; the fade runs to 4s", pick.atS)
	}
}

// ---------------------------------------------------------------------------
// #818 — the cheap poster job.
// ---------------------------------------------------------------------------

// videoShims installs ffprobe + ffmpeg stand-ins that report `dur`
// seconds and paint every requested output with a flat, clearly-bright
// JPEG. Returns the handler-ready paths.
func videoShims(t *testing.T, dur float64) (shimDir, ffmpeg, ffprobe string) {
	t.Helper()
	shimDir = t.TempDir()
	fixture := filepath.Join(shimDir, "frame.jpg")
	writeFlatJPEG(t, fixture, 640, 360, color.RGBA{R: 110, G: 140, B: 90, A: 255})
	ffprobe = writeShim(t, shimDir, "ffprobe", fmt.Sprintf(`
echo '{"format":{"duration":"%0.1f"},"streams":[{"codec_type":"video","width":640,"height":360,"avg_frame_rate":"24/1"}]}'
`, dur))
	ffmpeg = writeShim(t, shimDir, "ffmpeg", `
case "$*" in
  *-encoders*) echo " V....D h264_nvenc " ; echo " V..... libx264 " ; exit 0 ;;
esac
for last; do :; done
case "$last" in
  -) exit 0 ;;
  *.m3u8) printf '#EXTM3U\n' > "$last" ; exit 0 ;;
esac
cp `+fixture+` "$last"
`)
	return shimDir, ffmpeg, ffprobe
}

func runPosterJob(t *testing.T, rig *previewTestRig, assetID uuid.UUID, hash string, force bool) VideoPosterResult {
	t.Helper()
	_, ffmpeg, ffprobe := videoShims(t, 600)
	h := NewVideoPosterHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.FFmpegPath, h.FFprobePath, h.TempDir = ffmpeg, ffprobe, t.TempDir()
	payload, err := json.Marshal(dispatch.NewPayload(assetID, hash, ptr("mp4"), force))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := h.Handle(t.Context(), &jobs.Claim{
		ID: uuid.New(), Type: h.Type(), Payload: payload, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("poster job: %v", err)
	}
	var out VideoPosterResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// The whole feature, stated as a test: after the cheap job and BEFORE
// preview.video has run at all, the card has everything it needs.
func TestVideoPosterJob_MakesTheCardRenderableOnItsOwn(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "mp4", []byte("stand-in; the shims don't read it"))

	// The upload path leaves a freshly-created asset `pending`. (The
	// column's own default is 'ready', which the fixture inherits, so
	// this has to be set explicitly for the status assertion below to
	// mean anything.)
	if _, err := rig.pool.Exec(t.Context(),
		`UPDATE assets SET processing_status = 'pending' WHERE id = $1`, assetID); err != nil {
		t.Fatalf("set pending: %v", err)
	}

	res := runPosterJob(t, rig, assetID, hash, false)
	if !res.Rendered {
		t.Fatal("poster job reported nothing rendered on a fresh asset")
	}
	if !rig.previewAvailable(t, hash) {
		t.Error("preview_available is false after the poster job — the card is still blurred")
	}
	if rig.thumbhashOf(t, assetID) == nil {
		t.Error("no thumbhash after the poster job; the card flashes blank before the picture")
	}
	if _, err := rig.storage.Backend.Stat(t.Context(), hash, "poster"); err != nil {
		t.Errorf("no poster on the backend: %v", err)
	}

	// And it must leave processing_status alone. Marking the asset ready
	// would claim a playable stream it has not produced; marking it
	// failed would libel a job that has not run yet. The transitions
	// belong to preview.video, which is the job that finishes the asset.
	var status string
	if err := rig.pool.QueryRow(t.Context(),
		`SELECT processing_status FROM assets WHERE id = $1`, assetID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "pending" {
		t.Errorf("the poster job moved processing_status to %q; it has a picture but "+
			"no playable stream, and the full ladder owns that transition", status)
	}
}

// Idempotency across the two jobs: the expensive one must find the cheap
// one's poster and leave it alone.
func TestVideoJob_SkipsThePosterTheCheapJobAlreadyMade(t *testing.T) {
	rig := newPreviewTestRig(t)
	assetID, hash := rig.seedPreviewAsset(t, "mp4", []byte("stand-in"))

	runPosterJob(t, rig, assetID, hash, false)
	firstPoster := readVariant(t, rig, hash, "poster")

	_, ffmpeg, ffprobe := videoShims(t, 600)
	h := NewVideoHandler(rig.pool, rig.storage, rig.sysCfg, rig.logger)
	h.FFmpegPath, h.FFprobePath, h.TempDir = ffmpeg, ffprobe, t.TempDir()
	payload, err := json.Marshal(dispatch.NewPayload(assetID, hash, ptr("mp4"), false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := h.Handle(t.Context(), &jobs.Claim{
		ID: uuid.New(), Type: h.Type(), Payload: payload, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("video job: %v", err)
	}
	var res VideoResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !contains(res.Skipped, "poster") {
		t.Errorf("the full ladder re-rendered the poster; skipped=%v variants=%v",
			res.Skipped, res.Variants)
	}
	if got := readVariant(t, rig, hash, "poster"); !bytesEqual(got, firstPoster) {
		t.Error("the poster bytes changed — the card would swap pictures for no reason")
	}
}

func readVariant(t *testing.T, rig *previewTestRig, hash, key string) []byte {
	t.Helper()
	rc, _, err := rig.storage.Backend.Get(t.Context(), hash, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 4096)
	for {
		n, err := rc.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Routing: a video plans two jobs, everything else still plans one.
func TestPlanForExt_SplitsVideoAndLeavesEverythingElseAlone(t *testing.T) {
	video := "mkv"
	plan := dispatch.PlanForExt(&video, jobs.PriorityHigh)
	if len(plan) != 2 {
		t.Fatalf("video plan = %v, want the poster job plus the full ladder", plan)
	}
	if plan[0].Type != jobs.TypePreviewVideoPoster {
		t.Errorf("first step is %q; the cheap job must be enqueued first so it wins "+
			"the enqueued_at tiebreak at equal priority", plan[0].Type)
	}
	if plan[0].Priority != jobs.PriorityHigh {
		t.Errorf("poster priority = %d, want the caller's %d", plan[0].Priority, jobs.PriorityHigh)
	}
	if plan[1].Type != jobs.TypePreviewVideo || plan[1].Priority != jobs.PriorityBackfil {
		t.Errorf("second step = %v, want preview.video at PriorityBackfil", plan[1])
	}

	for _, ext := range []string{"png", "mp3", "pdf", "glb", "epub", "txt"} {
		e := ext
		p := dispatch.PlanForExt(&e, jobs.PriorityNormal)
		if len(p) != 1 || p[0].Type != dispatch.JobTypeForExt(&e) || p[0].Priority != jobs.PriorityNormal {
			t.Errorf("%s plan = %v, want a single unchanged step", ext, p)
		}
	}
}

// The cap the migration seeds has to name the type the registry actually
// registers. A typo there is silent — the cap simply never applies.
func TestVideoPosterJobType_MatchesTheSeededConcurrencyKey(t *testing.T) {
	const key = "jobs.type_concurrency.preview.video.poster"
	if got := "jobs.type_concurrency." + string(jobs.TypePreviewVideoPoster); got != key {
		t.Errorf("migration 00026 seeds %q, the handler registers %q", key, got)
	}
}

// writeFlatJPEG paints a solid-colour JPEG for the shims to hand back.
func writeFlatJPEG(t *testing.T, path string, w, h int, c color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}
