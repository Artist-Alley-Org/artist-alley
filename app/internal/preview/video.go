// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// VideoResult — what the worker writes back to jobs.result for the
// admin queue view.
type VideoResult struct {
	Encoder   string   `json:"encoder"`
	Variants  []string `json:"variants"`
	Skipped   []string `json:"skipped"`
	DurationS float64  `json:"duration_s"`
	Probe     Probe    `json:"probe"`
	WorkS     float64  `json:"work_s"`
}

// Probe captures the source-video metadata that drives encode
// decisions (ladder cap, fps for sprite cadence, etc).
type Probe struct {
	DurationS float64 `json:"duration_s"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	FPS       float64 `json:"fps"`
	HasAudio  bool    `json:"has_audio"`
}

// VideoHandler runs ffmpeg/ffprobe to produce HLS streams + a poster
// + a scrub sprite sheet for a single asset. Frame-accurate scrub on
// the frontend is paired with this output: ~2s GOP + 100-sprite grid
// + the `requestVideoFrameCallback` API.
//
// Idempotent on (hash, variant_key) — every output is checked against
// the storage backend before re-encoding.
type VideoHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	// FFmpegPath / FFprobePath override the executable lookups. Empty
	// = use `ffmpeg` / `ffprobe` from PATH.
	FFmpegPath  string
	FFprobePath string

	// TempDir is where intermediate files are staged. Empty =
	// os.TempDir(). For 4K + 120fps sources this needs to be on a
	// disk with several GB of free space.
	TempDir string

	// MaxSourceBytes guards against multi-GB ProRes uploads. Default
	// 5 GB.
	MaxSourceBytes int64

	// MaxJobDuration is the per-job wallclock cap. ffmpeg's exec
	// context is derived from this. Default 2h covers a typical 4K
	// 10-minute encode comfortably.
	MaxJobDuration time.Duration

	// Encoder is the cached probe result — populated lazily on the
	// first Handle call. Atomic-ish via the once.
	encoderOnce sync.Once
	encoder     EncoderProfile
}

// NewVideoHandler — recommended constructor with sensible defaults.
func NewVideoHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *VideoHandler {
	return &VideoHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 5 * 1024 * 1024 * 1024,
		MaxJobDuration: 2 * time.Hour,
	}
}

func (h *VideoHandler) Type() jobs.JobType { return jobs.TypePreviewVideo }

// ---------------------------------------------------------------------------
// Encoder detection
// ---------------------------------------------------------------------------

// EncoderProfile names the libavcodec encoder we should hand ffmpeg
// for the H.264 ladder. Higher = preferred. The worker picks the
// highest-rank profile the local ffmpeg actually has compiled.
type EncoderProfile struct {
	Name       string // "h264_nvenc", "h264_qsv", "h264_vaapi", "h264_videotoolbox", "libx264"
	Kind       string // "gpu" or "cpu"
	ExtraArgs  []string
	RankAtBoot int // diagnostic, set at probe time
}

func (h *VideoHandler) detectEncoder(ctx context.Context) EncoderProfile {
	h.encoderOnce.Do(func() {
		bin := h.ffmpegBin()
		out, err := exec.CommandContext(ctx, bin, "-hide_banner", "-encoders").CombinedOutput()
		if err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.video.encoders_probe_failed",
				slog.String("err", err.Error()))
			h.encoder = EncoderProfile{Name: "libx264", Kind: "cpu",
				ExtraArgs: []string{"-preset", "veryfast", "-crf", "21"}}
			return
		}
		s := string(out)
		// Preference order: VideoToolbox > NVENC > QSV > VAAPI > libx264.
		// We don't trust the encoders list alone — `h264_qsv` is built
		// into the alpine binary but needs an Intel DRI device the
		// container may not have. Each candidate gets a tiny test
		// encode; first one that actually produces bytes wins.
		candidates := []EncoderProfile{
			{Name: "h264_nvenc", Kind: "gpu",
				ExtraArgs: []string{"-preset", "p5", "-tune", "hq", "-rc:v", "vbr", "-cq", "20"}},
			{Name: "h264_qsv", Kind: "gpu",
				ExtraArgs: []string{"-preset", "medium", "-global_quality", "22"}},
			{Name: "h264_videotoolbox", Kind: "gpu",
				ExtraArgs: []string{"-q:v", "60"}},
			{Name: "libx264", Kind: "cpu",
				ExtraArgs: []string{"-preset", "veryfast", "-crf", "21"}},
		}
		for _, c := range candidates {
			if !strings.Contains(s, " "+c.Name+" ") {
				continue
			}
			if !testEncoder(ctx, bin, c.Name) {
				h.Logger.LogAttrs(ctx, slog.LevelDebug, "preview.video.encoder.probe_failed",
					slog.String("encoder", c.Name))
				continue
			}
			h.encoder = c
			h.Logger.LogAttrs(ctx, slog.LevelInfo, "preview.video.encoder.detected",
				slog.String("encoder", c.Name),
				slog.String("kind", c.Kind),
			)
			return
		}
		h.encoder = EncoderProfile{Name: "libx264", Kind: "cpu",
			ExtraArgs: []string{"-preset", "veryfast", "-crf", "21"}}
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.video.encoder.fallback_libx264",
			slog.String("reason", "no candidate produced a frame at probe time"))
	})
	return h.encoder
}

// testEncoder runs a 1-frame synthetic encode through the named
// encoder. Returns true only on a clean exit. Used to weed out
// encoders the ffmpeg binary lists but can't actually run (missing
// kernel module, DRI device, NVIDIA library, etc.).
func testEncoder(ctx context.Context, bin, encoder string) bool {
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, bin,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=size=128x72:rate=24:duration=0.5",
		"-c:v", encoder, "-pix_fmt", "yuv420p",
		"-frames:v", "4",
		"-f", "null", "-")
	return cmd.Run() == nil
}

// ---------------------------------------------------------------------------
// Handle
// ---------------------------------------------------------------------------

func (h *VideoHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()
	var p VideoPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.video: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.video: file_hash is required")}
	}
	if !isVideoExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.video: extension %q is not a video format", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	h.markProcessing(jobCtx, p.AssetID)
	enc := h.detectEncoder(jobCtx)

	work, cleanup, err := h.stage(jobCtx, p.FileHash)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}
	defer cleanup()

	probe, err := h.probe(jobCtx, work.sourcePath)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}

	result := VideoResult{
		Encoder: enc.Name,
		Probe:   probe,
	}

	// --- poster + raster ladder (col / preview / screen / hires) --------
	// We always run this path because the ladder may be missing even
	// when the poster itself already exists (e.g. earlier encodes
	// pre-dating poster-derived thumbnails). The inner helpers skip
	// what's already on the backend.
	//
	// On an install running the cheap poster job (#818) this is normally
	// the skip branch: preview.video.poster has already put the poster
	// and the whole raster ladder in place by the time this job is
	// claimed, and the expensive handler must not render either again.
	rendered, err := h.writePoster(jobCtx, p.AssetID, work, p.FileHash, probe, p.Force)
	if err != nil {
		return nil, fmt.Errorf("preview.video: poster: %w", err)
	}
	if rendered {
		result.Variants = append(result.Variants, "poster")
	} else {
		result.Skipped = append(result.Skipped, "poster")
	}

	// --- HLS ladder -------------------------------------------------------
	if variantDone(jobCtx, h.Storage, p.FileHash, "hls/master.m3u8", p.Force) {
		result.Skipped = append(result.Skipped, "hls")
	} else if err := h.writeHLS(jobCtx, work, p.FileHash, probe, enc); err != nil {
		return nil, fmt.Errorf("preview.video: hls: %w", err)
	} else {
		result.Variants = append(result.Variants, "hls")
	}

	// --- scrub sprite + VTT ----------------------------------------------
	if variantDone(jobCtx, h.Storage, p.FileHash, "sprites.jpg", p.Force) {
		result.Skipped = append(result.Skipped, "sprites")
		// The sheet and its cue file are written together and are
		// useless apart, so the sentinel's reconcile has to cover both
		// (#827) — otherwise a healed install has a row for the pixels
		// and none for the geometry that reads them.
		variantDone(jobCtx, h.Storage, p.FileHash, "sprites.vtt", false)
	} else if err := h.writeSprites(jobCtx, work, p.FileHash, probe); err != nil {
		return nil, fmt.Errorf("preview.video: sprites: %w", err)
	} else {
		result.Variants = append(result.Variants, "sprites")
	}

	h.markReady(jobCtx, p.AssetID)
	result.DurationS = probe.DurationS
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// Staging — copy source to a temp dir so ffmpeg can seek freely.
// ---------------------------------------------------------------------------

type workDir struct {
	dir        string
	sourcePath string
}

func (h *VideoHandler) stage(ctx context.Context, hash string) (workDir, func(), error) {
	return stageSource(ctx, h.Storage, h.TempDir, "aa-video-*", hash, h.MaxSourceBytes)
}

// stageSource copies an asset's original bytes into a fresh temp dir so
// ffmpeg can seek freely over a real file. Shared by every handler that
// shells out to ffmpeg (video, video.poster, gif) — the staging dance is
// identical and the only thing that differs is the temp-dir prefix.
func stageSource(ctx context.Context, st *storage.Service, tempDir, pattern, hash string, maxBytes int64) (workDir, func(), error) {
	base := tempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return workDir{}, nil, fmt.Errorf("stage: mkdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	rc, info, err := st.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: download: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > maxBytes {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: source %d bytes > cap %d", info.Size, maxBytes)
	}

	srcPath := filepath.Join(dir, "src.bin")
	f, err := os.Create(srcPath)
	if err != nil {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: create: %w", err)
	}
	if _, err := io.CopyN(f, rc, maxBytes+1); err != nil && !errors.Is(err, io.EOF) {
		_ = f.Close()
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: copy: %w", err)
	}
	_ = f.Close()
	return workDir{dir: dir, sourcePath: srcPath}, cleanup, nil
}

// ---------------------------------------------------------------------------
// Probe
// ---------------------------------------------------------------------------

func (h *VideoHandler) probe(ctx context.Context, path string) (Probe, error) {
	return probeMedia(ctx, h.ffprobeBin(), path)
}

// probeMedia reads a staged file's duration, frame shape and fps.
//
// Not video-only despite the Probe type's name: ffprobe demuxes an
// animated GIF as a video stream with a real duration and avg_frame_rate
// (verified against the seed's 29.97s 260x212 capture), which is exactly
// what the sprite cadence needs. preview.gif calls this rather than
// carrying a second probe that would answer the same question slightly
// differently.
func probeMedia(ctx context.Context, ffprobeBin, path string) (Probe, error) {
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return Probe{}, fmt.Errorf("ffprobe: %w", err)
	}
	var raw struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			AvgFrameRate string `json:"avg_frame_rate"`
			RFrameRate   string `json:"r_frame_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Probe{}, fmt.Errorf("ffprobe parse: %w", err)
	}
	var p Probe
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if p.Width == 0 {
				p.Width = s.Width
				p.Height = s.Height
				p.FPS = parseRatio(s.AvgFrameRate)
				if p.FPS == 0 {
					p.FPS = parseRatio(s.RFrameRate)
				}
			}
		case "audio":
			p.HasAudio = true
		}
	}
	if d, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil {
		p.DurationS = d
	}
	return p, nil
}

func parseRatio(s string) float64 {
	if s == "" {
		return 0
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	a, errA := strconv.ParseFloat(parts[0], 64)
	b, errB := strconv.ParseFloat(parts[1], 64)
	if errA != nil || errB != nil || b == 0 {
		return 0
	}
	return a / b
}

// ---------------------------------------------------------------------------
// Poster
// ---------------------------------------------------------------------------

// posterCandidateFractions are the points in a clip, as fractions of
// its duration, that the poster is drawn from — in order of preference.
//
// A FRACTION, NOT A FIXED OFFSET (#810). The old rule was `at := 1.0`,
// with the fraction used only as a short-clip guard, so on anything
// longer than four seconds the poster was always the frame one second
// in. One second into a film is the fade-in. Measured mean luma at
// t=1.0s across the seed dataset: sintel-2010-1080p.mkv 0.0 — a
// literally black card — tears-of-steel 15.0, big-buck-bunny 49.3,
// xonotic 93.0, which is why gameplay capture looked fine and films did
// not. Ten percent in is past every title card we have.
//
// Three of them because one is a guess. The extras are only ever
// visited when the first lands somewhere dark; see selectPoster.
var posterCandidateFractions = []float64{0.10, 0.25, 0.50}

// posterThumbnailFrames is the window the ffmpeg `thumbnail` filter
// scores at each candidate offset. It buffers this many frames, picks
// the one furthest from their average histogram, and emits it — which is
// what turns "the frame that happens to be at 10%" into "the most
// distinctive frame near 10%", at the cost of decoding the window.
//
// 60 is ~2.5 seconds at 24fps: wide enough to step over a cut or a
// dissolve, narrow enough that the decode stays well under a second for
// 1080p. It is deliberately not the 100 the sprite sheet uses — that
// grid samples the whole clip and can afford to; this runs in the cheap
// poster job whose entire justification is finishing fast.
const posterThumbnailFrames = 60

// posterMinMeanLuma is the mean luminance, on 0..1, below which a
// candidate frame is treated as "the card would look broken" and the
// next offset is tried.
//
// 16/255. Above it sit every deliberately dark frame we measured
// (tears-of-steel's t=1.0s frame is 15.0/255, i.e. just under — which is
// the right side of the line for a frame that reads as black on a
// browse grid). Below it is a fade, a slate, or a letterboxed gap.
const posterMinMeanLuma = 16.0 / 255.0

// posterPick is the outcome of selectPoster: the chosen frame, where it
// came from, and how bright it turned out.
type posterPick struct {
	path  string
	img   image.Image
	atS   float64
	luma  float64
	tries int
}

// writePoster puts a poster frame on the backend and fans the raster
// ladder from it. Reports whether it actually rendered one.
func (h *VideoHandler) writePoster(ctx context.Context, assetID uuid.UUID, w workDir, hash string, probe Probe, force bool) (bool, error) {
	return writeTimelinePoster(ctx, timelinePosterInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		FFmpegBin: h.ffmpegBin(), Kind: "video",
		AssetID: assetID, Hash: hash, Work: w, Probe: probe, Force: force,
	})
}

// timelinePosterInput is everything writeTimelinePoster needs. Handler
// structs differ, so the shared step takes explicit dependencies — same
// reasoning as ladderInput in ladder.go.
type timelinePosterInput struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	FFmpegBin string
	// Kind names the pipeline for log keys and for the ladder's
	// provenance: "video" or "gif".
	Kind string

	AssetID uuid.UUID
	Hash    string
	Work    workDir
	Probe   Probe
	Force   bool
}

// writeTimelinePoster picks a representative frame from a staged
// timeline source, stores it as the `poster` variant, and fans the
// raster ladder from it. Reports whether it actually rendered one.
//
// Shared with preview.gif (#832) rather than reimplemented there. An
// animated GIF needs the SAME frame selection a video does, and for the
// same reason: frame one of a screen capture is usually the empty window
// before anything happens, which is precisely the "black card" case
// #810's fraction-plus-luma rule exists to walk past. Decoding frame one
// with image/gif would have been three lines and would have shipped a
// library of blank thumbnails.
func writeTimelinePoster(ctx context.Context, in timelinePosterInput) (bool, error) {
	fan := func(src image.Image) error {
		if src == nil {
			return errors.New("poster variants: nil source image")
		}
		return fanToLadder(ctx, ladderInput{
			Pool: in.Pool, Storage: in.Storage, SysConfig: in.SysConfig, Logger: in.Logger,
			AssetID: in.AssetID, Hash: in.Hash, Src: src, Kind: in.Kind, Source: "poster",
			Overwrite: in.Force,
		})
	}

	if variantDone(ctx, in.Storage, in.Hash, "poster", in.Force) {
		// The poster bytes already exist, but the raster ladder may not
		// — an encode predating poster-derived thumbnails has one and
		// not the other — so the ladder still has to run.
		//
		// It runs from the STORED poster, read back, rather than from a
		// fresh extract of the source. Re-extracting was cheap and
		// wrong: it is not guaranteed to return the same frame (with
		// #810's selection it frequently will not), so the `col` a card
		// shows would drift away from the `poster` the player shows for
		// the same asset, from one requeue to the next, with nothing
		// re-uploaded to explain it.
		src, err := decodeStoredVariant(ctx, in.Storage, in.Hash, "poster", defaultMaxVariantBytes)
		if err == nil {
			return false, fan(src)
		}
		// Unreadable poster bytes — the backend has something under the
		// key that will not decode. Fall through and render a new one.
		logAttrs(in.Logger, ctx, slog.LevelWarn, "preview."+in.Kind+".poster_readback_failed",
			slog.String("file_hash", in.Hash),
			slog.String("err", err.Error()))
	}

	pick, err := selectPosterFrame(ctx, in.FFmpegBin, in.Work, in.Probe)
	if err != nil {
		return false, err
	}
	logAttrs(in.Logger, ctx, slog.LevelDebug, "preview."+in.Kind+".poster_selected",
		slog.String("file_hash", in.Hash),
		slog.Float64("at_s", pick.atS),
		slog.Float64("mean_luma", pick.luma),
		slog.Int("tries", pick.tries))

	if err := putVariantFile(ctx, in.Pool, in.Storage, in.Hash, "poster", pick.path, "image/jpeg"); err != nil {
		return false, err
	}

	// Drive the raster variant ladder (col / preview / screen / hires)
	// from the poster frame so videos render the same shape as images
	// across every browse + post-detail surface. This is what makes a
	// video card actually look like a card instead of a placeholder.
	return true, fan(pick.img)
}

// selectPoster extracts the best poster frame it can find and returns it
// decoded.
//
// The rule is "a representative frame near a fraction of the duration,
// and not a black one". ffmpeg's `thumbnail` filter supplies the first
// half — it scores a window of frames and hands back the most
// distinctive — and it is the right tool, but it is not sufficient on
// its own: it picks the best frame in the window it is given, and a
// window that lies entirely inside a fade-from-black contains no good
// frame to pick. So the offsets are walked in order and the mean
// luminance of each result decides whether to stop.
//
// The measurement is free of extra process spawns: the JPEG has to be
// decoded anyway to drive the ladder, so the check reads the image that
// was already going to be decoded.
//
// A clip that is dark ALL THE WAY THROUGH — night footage, a black-slug
// intro across the first half — still gets a poster: the brightest
// candidate seen wins once the list runs out. Refusing to produce one
// would trade a dark card for no card.
func (h *VideoHandler) selectPoster(ctx context.Context, w workDir, probe Probe) (posterPick, error) {
	return selectPosterFrame(ctx, h.ffmpegBin(), w, probe)
}

// selectPosterFrame is selectPoster's implementation, free of the video
// handler so preview.gif can reuse the same selection rule (#832).
func selectPosterFrame(ctx context.Context, ffmpegBin string, w workDir, probe Probe) (posterPick, error) {
	offsets := posterOffsets(probe.DurationS)
	var best posterPick
	var lastErr error
	for i, at := range offsets {
		path := filepath.Join(w.dir, fmt.Sprintf("poster-%d.jpg", i))
		if err := extractPosterFrame(ctx, ffmpegBin, w.sourcePath, path, at); err != nil {
			lastErr = err
			continue
		}
		img, err := decodeImageFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		pick := posterPick{path: path, img: img, atS: at, luma: meanLuma(img), tries: i + 1}
		if pick.luma > best.luma || best.img == nil {
			best = pick
		}
		if pick.luma >= posterMinMeanLuma {
			best.tries = i + 1
			return best, nil
		}
	}
	if best.img == nil {
		if lastErr == nil {
			lastErr = errors.New("no candidate offsets")
		}
		return posterPick{}, fmt.Errorf("poster: no usable frame: %w", lastErr)
	}
	best.tries = len(offsets)
	return best, nil
}

// posterOffsets turns a duration into the ordered seek points to try.
//
// A zero or absent duration (a container ffprobe could not measure)
// falls back to the start of the file: the thumbnail filter still scans
// forward from there, so the result is "the most distinctive of the
// first 60 frames" rather than "frame 0", which is the best available
// answer when there is no duration to take a fraction of.
func posterOffsets(durationS float64) []float64 {
	if durationS <= 0 {
		return []float64{0}
	}
	out := make([]float64, 0, len(posterCandidateFractions))
	for _, f := range posterCandidateFractions {
		out = append(out, durationS*f)
	}
	return out
}

// extractFrame writes one JPEG from the source at the given offset.
//
// `-ss` before `-i` is an input seek — ffmpeg jumps to the preceding
// keyframe rather than decoding from zero, which is what keeps this
// affordable on a feature-length source.
func (h *VideoHandler) extractFrame(ctx context.Context, sourcePath, outPath string, at float64) error {
	return extractPosterFrame(ctx, h.ffmpegBin(), sourcePath, outPath, at)
}

// extractPosterFrame is extractFrame's implementation, free of the
// handler — see selectPosterFrame.
func extractPosterFrame(ctx context.Context, ffmpegBin, sourcePath, outPath string, at float64) error {
	return runFFmpeg(exec.CommandContext(ctx, ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", at),
		"-i", sourcePath,
		// thumbnail first, then scale: the filter's histogram scoring
		// should see the frames as shot. Cap at 4096 so `hires` has
		// headroom; the sysconfig variant ladder downsamples from there.
		"-vf", fmt.Sprintf("thumbnail=%d,scale='min(4096,iw)':'-2'", posterThumbnailFrames),
		"-frames:v", "1",
		"-q:v", "2",
		outPath,
	))
}

// meanLuma is the average Rec.601 luminance of an image, on 0..1.
//
// Subsampled: a 4096px poster is 8M+ pixels and image.At is an interface
// call per pixel, so a full pass would cost more than the ffmpeg run it
// is checking. ~4096 samples on a regular grid is far more than enough
// to tell "black card" from "picture" — the distinction this exists to
// draw is between 0.0 and 0.2, not between 0.19 and 0.20.
func meanLuma(img image.Image) float64 {
	if img == nil {
		return 0
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return 0
	}
	const samplesPerAxis = 64
	stepX := max(1, w/samplesPerAxis)
	stepY := max(1, h/samplesPerAxis)
	var sum float64
	var n int
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			r, g, bl, _ := img.At(x, y).RGBA()
			sum += (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 65535.0
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// decodeImageFile decodes a file the handler just wrote.
func decodeImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

// writePosterVariants runs the poster frame through the shared ladder
// step, which writes each rung and stamps the asset's thumbhash and
// pixel dimensions from it. Rungs already on the backend are skipped —
// and, since #827, reconciled — so re-runs are cheap.
//
// Takes the DECODED image, not a path: both callers already hold one
// (the render path decoded it to measure its luminance, the skip path
// decoded it out of storage), and decoding a 4096px JPEG twice to keep a
// path-shaped signature is not a trade worth making.
//
// A failed Put here fails the job (it used to log + continue). The job
// system retries up to max_attempts, and the alternative — a video
// reporting success with no `col` — is the col-404 class the model
// handler already fixed for itself.
func (h *VideoHandler) writePosterVariants(ctx context.Context, assetID uuid.UUID, hash string, src image.Image, force bool) error {
	if src == nil {
		return errors.New("poster variants: nil source image")
	}
	return fanToLadder(ctx, ladderInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		AssetID: assetID, Hash: hash, Src: src, Kind: "video", Source: "poster",
		Overwrite: force,
	})
}

// ---------------------------------------------------------------------------
// HLS ladder
// ---------------------------------------------------------------------------

type rendition struct {
	Name         string
	Height       int
	VideoBitkbps int
	AudioBitkbps int
	Bandwidth    int // for master playlist; ~1.2x bitrate
}

// renditionsFor picks the ladder based on the source resolution. We
// never upscale.
func renditionsFor(probe Probe) []rendition {
	var out []rendition
	if probe.Height >= 480 {
		out = append(out, rendition{Name: "480p", Height: 480, VideoBitkbps: 1200, AudioBitkbps: 96, Bandwidth: 1500_000})
	}
	if probe.Height >= 720 {
		out = append(out, rendition{Name: "720p", Height: 720, VideoBitkbps: 3000, AudioBitkbps: 128, Bandwidth: 3500_000})
	}
	if probe.Height >= 1080 {
		out = append(out, rendition{Name: "1080p", Height: 1080, VideoBitkbps: 6000, AudioBitkbps: 160, Bandwidth: 7000_000})
	}
	if len(out) == 0 {
		// Source smaller than 480p — encode at native resolution.
		out = append(out, rendition{Name: "src", Height: probe.Height, VideoBitkbps: 1000, AudioBitkbps: 96, Bandwidth: 1300_000})
	}
	return out
}

func (h *VideoHandler) writeHLS(ctx context.Context, w workDir, hash string, probe Probe, enc EncoderProfile) error {
	rends := renditionsFor(probe)
	hlsDir := filepath.Join(w.dir, "hls")
	if err := os.MkdirAll(hlsDir, 0o755); err != nil {
		return err
	}

	// Encode each rendition separately. Multi-output ffmpeg invocations
	// are faster but harder to reason about with mixed encoders; a per-
	// rendition pass is straightforward and idempotent.
	for _, r := range rends {
		rendDir := filepath.Join(hlsDir, r.Name)
		if err := os.MkdirAll(rendDir, 0o755); err != nil {
			return err
		}
		playlistPath := filepath.Join(rendDir, "playlist.m3u8")
		segmentPattern := filepath.Join(rendDir, "seg%05d.ts")
		args := []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-i", w.sourcePath,
			"-vf", fmt.Sprintf("scale=-2:%d", r.Height),
			"-c:v", enc.Name,
		}
		args = append(args, enc.ExtraArgs...)
		args = append(args,
			"-b:v", fmt.Sprintf("%dk", r.VideoBitkbps),
			"-maxrate", fmt.Sprintf("%dk", r.VideoBitkbps*12/10),
			"-bufsize", fmt.Sprintf("%dk", r.VideoBitkbps*2),
			"-profile:v", "main",
			"-pix_fmt", "yuv420p",
			"-g", "60", "-keyint_min", "60", "-sc_threshold", "0",
			// Drop subtitle streams: mkv containers commonly carry
			// embedded SRT/ASS subtitles which ffmpeg's HLS muxer
			// would otherwise emit as a parallel _vtt.m3u8 with
			// hundreds of tiny .vtt segments — bloats storage and
			// the master.m3u8 doesn't reference them anyway.
			"-sn",
		)
		if probe.HasAudio {
			args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", r.AudioBitkbps))
		} else {
			args = append(args, "-an")
		}
		args = append(args,
			"-hls_time", "4",
			"-hls_playlist_type", "vod",
			"-hls_segment_filename", segmentPattern,
			"-f", "hls",
			playlistPath,
		)
		if err := runFFmpeg(exec.CommandContext(ctx, h.ffmpegBin(), args...)); err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
	}

	// Master playlist points at the per-rendition manifests with
	// their RESOLUTION + BANDWIDTH for the player's ABR logic.
	var master bytes.Buffer
	master.WriteString("#EXTM3U\n")
	master.WriteString("#EXT-X-VERSION:6\n")
	for _, r := range rends {
		fmt.Fprintf(&master,
			"#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\n%s/playlist.m3u8\n",
			r.Bandwidth, probe.Width*r.Height/probe.Height, r.Height, r.Name,
		)
	}
	masterPath := filepath.Join(hlsDir, "master.m3u8")
	if err := os.WriteFile(masterPath, master.Bytes(), 0o644); err != nil {
		return err
	}

	// Upload everything. Master last so partial uploads don't 200 the
	// player into trying to fetch missing segments.
	if err := h.uploadHLSTree(ctx, hash, hlsDir); err != nil {
		return err
	}
	if err := h.uploadFile(ctx, hash, "hls/master.m3u8", masterPath, "application/vnd.apple.mpegurl"); err != nil {
		return err
	}
	return nil
}

func (h *VideoHandler) uploadHLSTree(ctx context.Context, hash, hlsDir string) error {
	return filepath.WalkDir(hlsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(p) == "master.m3u8" {
			return nil
		}
		rel, err := filepath.Rel(hlsDir, p)
		if err != nil {
			return err
		}
		key := "hls/" + filepath.ToSlash(rel)
		ct := "application/octet-stream"
		switch filepath.Ext(p) {
		case ".m3u8":
			ct = "application/vnd.apple.mpegurl"
		case ".ts":
			ct = "video/mp2t"
		}
		return h.uploadFile(ctx, hash, key, p, ct)
	})
}

// ---------------------------------------------------------------------------
// Scrub sprite sheet + WebVTT
// ---------------------------------------------------------------------------

const (
	spriteCols = 10
	spriteRows = 10

	// spriteCellBox bounds BOTH edges of a single scrub cell. The cell
	// is fitted inside this square with the source's aspect ratio
	// preserved (#761), so:
	//
	//   16:9  -> 240x134 or 240x136 (see below)
	//   9:16  -> 134x240 or 136x240
	//   1:1   -> 240x240
	//
	// and the whole sheet is therefore bounded at 2400x2400 whatever
	// the source shape — an ultrawide or a 1:8 panorama cannot blow the
	// sheet out to several thousand pixels on one axis.
	//
	// The 16:9 short edge is not a fixed number: it computes to 135,
	// which is odd, and `force_divisible_by=2` resolves that per ffmpeg
	// BUILD — 5.1 (the runtime image) rounds down to 134, 6.1 rounds up
	// to 136, same filter, same source. Nothing downstream may assume
	// either: the VTT measures the sheet that was written (#796) rather
	// than recomputing it, and the frontend measures it again off the
	// image it paints.
	//
	// 240, not 160 (#811): the card shows a 320px still and swaps it for
	// this cell on hover, so a 160px cell was visibly softer than the
	// image it replaced. Measured over a 51-sheet seed, 240 buys 1.5x
	// linear resolution for 1.8x the sheet bytes — JPEG does not scale
	// with pixel count, so this came in under the 2.25x the decision
	// budgeted. Matching the still at 320 would have cost ~4x, and
	// motion masks the remaining gap.
	spriteCellBox = 240

	// spriteFallbackW/H are the cell dimensions used only when the real
	// sheet cannot be measured AND the probe carries no usable source
	// dimensions. The 16:9 fit of the box — the shape the handler emitted
	// unconditionally before #761 — DERIVED from spriteCellBox rather
	// than written out, so raising the box cannot leave a stale cell
	// behind here (it very nearly did in #811).
	spriteFallbackW = spriteCellBox
	spriteFallbackH = spriteCellBox * 9 / 16
)

// spriteCellSize fits a srcW x srcH frame inside the spriteCellBox
// square, preserving aspect ratio.
//
// It mirrors what `scale=BOX:BOX:force_original_aspect_ratio=decrease:
// force_divisible_by=2` asks ffmpeg to do, and exists as the fallback
// for the VTT geometry when the generated sheet cannot be measured.
// Degenerate probes (a container ffprobe reports no video stream for,
// so Width/Height are 0) fall back to the historical 16:9 cell rather
// than dividing by zero.
func spriteCellSize(srcW, srcH int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		// evenCell here too: the derived 16:9 fit of the box is not
		// guaranteed to be even (240 -> 135), and an odd cell edge is the
		// one thing every other path in this file rules out.
		return evenCell(spriteFallbackW), evenCell(spriteFallbackH)
	}
	cw, ch := spriteCellBox, spriteCellBox
	if srcW >= srcH {
		ch = int(math.Round(spriteCellBox * float64(srcH) / float64(srcW)))
	} else {
		cw = int(math.Round(spriteCellBox * float64(srcW) / float64(srcH)))
	}
	return evenCell(cw), evenCell(ch)
}

// evenCell clamps a cell edge into [2, spriteCellBox] and rounds it up
// to an even number. Odd dimensions are rejected outright by some
// encoders and quietly resampled by others; 2 is the floor because
// `force_divisible_by=2` is ffmpeg's own floor for a pathological
// aspect ratio (a 1000:1 render would otherwise compute a 0px edge).
func evenCell(v int) int {
	if v < 2 {
		return 2
	}
	if v > spriteCellBox {
		v = spriteCellBox
	}
	return v + v%2
}

func (h *VideoHandler) writeSprites(ctx context.Context, w workDir, hash string, probe Probe) error {
	return writeSpriteSheet(ctx, h.ffmpegBin(), w, probe, func(ctx context.Context, key, path, ct string) error {
		return h.uploadFile(ctx, hash, key, path, ct)
	})
}

// spriteUploader puts one generated file on the backend under a variant
// key. Every ffmpeg-driven handler already has one (they differ only in
// which asset hash they close over), so the shared sheet writer takes it
// rather than a handler.
type spriteUploader func(ctx context.Context, key, path, contentType string) error

// spriteCueCount is how many of the grid's cells the sheet will
// ACTUALLY contain a frame in — and therefore how many cues the VTT is
// allowed to declare.
//
// It is not always the full grid, and the difference is the whole of
// #835. `interval` has a floor of spriteMinInterval, so a clip shorter
// than grid×floor seconds cannot fill the grid: ffmpeg's `fps` filter
// emits ceil(duration/interval) frames, `tile` pads the rest of the
// sheet with black, and a consumer that assumes cols×rows frames scrubs
// through the padding. A 5s clip fills 25 of 100 cells, so three
// quarters of its hover preview was blank.
//
// ceil, not round or floor: the fps filter's output frame k lands at
// t = k/rate, so every k with k/rate < duration produces a frame, which
// is exactly ceil(duration/interval) frames. Where that lands on a
// boundary (duration an exact multiple of the interval) the answer is
// one LOW, never one high — under-declaring loses a frame nobody misses,
// over-declaring shows a black cell, which is the bug.
func spriteCueCount(durationS, interval float64, gridCells int) int {
	if durationS <= 0 || interval <= 0 {
		return 0
	}
	n := int(math.Ceil(durationS / interval))
	if n > gridCells {
		n = gridCells
	}
	if n < 1 {
		n = 1
	}
	return n
}

// spriteMinInterval is the closest together two scrub cells may sample.
// Below it a sheet stops being a summary of the clip and becomes a
// low-framerate copy of it — 100 cells five frames apart is 4 seconds of
// a 4-second clip.
const spriteMinInterval = 0.2

// writeSpriteSheet renders the hover-scrub sheet + its WebVTT cue file
// for any ffmpeg-readable timeline source and hands both to `upload`.
//
// Shared by preview.video and preview.gif (#832): an animated GIF is a
// short silent video, and giving it a second, subtly different sprite
// writer is how the two drift. The only thing either caller supplies is
// a staged file, a probe, and where to put the output.
func writeSpriteSheet(ctx context.Context, ffmpegBin string, w workDir, probe Probe, upload spriteUploader) error {
	if probe.DurationS <= 0 {
		return errors.New("sprites: probe duration is zero")
	}
	totalCells := spriteCols * spriteRows
	// Cell interval in seconds.
	interval := probe.DurationS / float64(totalCells)
	if interval < spriteMinInterval {
		interval = spriteMinInterval
	}
	spriteOut := filepath.Join(w.dir, "sprites.jpg")
	// `select='not(mod(n,N))'` is fragile across containers; using
	// `fps=` is more deterministic.
	fps := 1.0 / interval
	// The box + force_original_aspect_ratio=decrease form (#761) is
	// deliberately NOT `scale=<computed w>:<computed h>` from the probe:
	// ffmpeg applies the container's display matrix before the
	// filtergraph, so a phone-shot portrait clip arrives here with
	// probe.Width/Height of 1920x1080 (coded) but decodes as 1080x1920.
	// Letting ffmpeg fit the real decoded frame gets rotated sources
	// right; a probe-derived cell size would still squash them.
	cmd := exec.CommandContext(ctx, ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", w.sourcePath,
		"-vf", fmt.Sprintf(
			"fps=%f,scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,tile=%dx%d",
			fps, spriteCellBox, spriteCellBox, spriteCols, spriteRows),
		"-frames:v", "1",
		"-q:v", "5",
		spriteOut,
	)
	if err := runFFmpeg(cmd); err != nil {
		return err
	}
	if err := upload(ctx, "sprites.jpg", spriteOut, "image/jpeg"); err != nil {
		return err
	}

	// The VTT's cell size is MEASURED off the sheet that was just
	// written, never recomputed from the same inputs. A stated cell
	// size that disagrees with the real one by even a pixel makes every
	// hover thumbnail crop a sliding, wrong region — plausible-looking
	// on some frames and visibly wrong on others, which is harder to
	// spot than the squash this issue started as. Measuring makes the
	// two impossible to diverge.
	cellW, cellH := measureSpriteCell(spriteOut, probe)

	// WebVTT mapping each POPULATED cell to its time range.
	//
	// The count comes from spriteCueCount, not from the grid (#835). The
	// VTT is the only thing that knows which cells the tile filter
	// actually filled — the sheet itself is always cols×rows with black
	// padding, and nothing downstream can tell padding from a dark frame.
	// Declaring cues for cells that were never written is what made a
	// short clip's hover preview three-quarters blank.
	//
	// The old loop ran the full grid and broke on `start >= duration`
	// AFTER writing that cue, which both over-declared by one and left a
	// zero-length cue at the end.
	cues := spriteCueCount(probe.DurationS, interval, totalCells)
	var vtt bytes.Buffer
	vtt.WriteString("WEBVTT\n\n")
	for i := 0; i < cues; i++ {
		start := float64(i) * interval
		end := float64(i+1) * interval
		if end > probe.DurationS {
			end = probe.DurationS
		}
		x := (i % spriteCols) * cellW
		y := (i / spriteCols) * cellH
		fmt.Fprintf(&vtt, "%s --> %s\nsprites.jpg#xywh=%d,%d,%d,%d\n\n",
			vttTime(start), vttTime(end), x, y, cellW, cellH)
	}
	vttPath := filepath.Join(w.dir, "sprites.vtt")
	if err := os.WriteFile(vttPath, vtt.Bytes(), 0o644); err != nil {
		return err
	}
	return upload(ctx, "sprites.vtt", vttPath, "text/vtt")
}

// measureSpriteCell reads the generated sheet's real pixel dimensions
// and divides them by the grid to get the true cell size. Falls back to
// the probe-derived fit only if the sheet cannot be decoded or does not
// divide cleanly into the grid — in which case the sheet is already
// suspect and the historical geometry is the least-surprising answer.
func measureSpriteCell(path string, probe Probe) (int, int) {
	fallbackW, fallbackH := spriteCellSize(probe.Width, probe.Height)
	f, err := os.Open(path)
	if err != nil {
		return fallbackW, fallbackH
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return fallbackW, fallbackH
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width%spriteCols != 0 || cfg.Height%spriteRows != 0 {
		return fallbackW, fallbackH
	}
	return cfg.Width / spriteCols, cfg.Height / spriteRows
}

func vttTime(s float64) string {
	if s < 0 {
		s = 0
	}
	h := int(s) / 3600
	m := (int(s) % 3600) / 60
	sec := s - float64(int(s)/60*60)
	return fmt.Sprintf("%02d:%02d:%06.3f", h, m, sec)
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

func (h *VideoHandler) ffmpegBin() string {
	if h.FFmpegPath != "" {
		return h.FFmpegPath
	}
	return "ffmpeg"
}
func (h *VideoHandler) ffprobeBin() string {
	if h.FFprobePath != "" {
		return h.FFprobePath
	}
	return "ffprobe"
}

func (h *VideoHandler) uploadFile(ctx context.Context, hash, key, path, contentType string) error {
	return putVariantFile(ctx, h.Pool, h.Storage, hash, key, path, contentType)
}

// putVariantFile streams a generated file onto the storage backend under
// a variant key and records the row that makes it servable. The
// handler-free form so preview.gif can hand the same closure to the
// shared poster + sprite writers.
func putVariantFile(ctx context.Context, pool *pgxpool.Pool, st *storage.Service, hash, key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", key, err)
	}
	defer f.Close()
	if _, err := st.Backend.Put(ctx, hash, key, f); err != nil {
		return fmt.Errorf("backend put %s: %w", key, err)
	}
	info, err := f.Stat()
	if err == nil {
		_ = storage.New(pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  hash,
			VariantKey:  key,
			SizeBytes:   info.Size(),
			ContentType: contentType,
			Metadata:    []byte("{}"),
		})
	}
	return nil
}

func runFFmpeg(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (h *VideoHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.video.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *VideoHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.video.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *VideoHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.video.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

// isVideoExt reports whether preview.video can decode ext.
//
// It reads dispatch.VideoExts — the very set that ROUTES work here —
// rather than a private copy kept in sync "by convention" (#362). That
// copy had silently drifted 7 entries behind the router, so f4v, insv,
// lrv, m2ts, mts, mxf and vob were routed here and then hard-rejected,
// and those assets never got a preview. One set, read by both the
// router and the handler, cannot drift; preview_dispatch_test.go
// asserts it.
//
// All seven are demuxable by the ffmpeg this handler already shells out
// to (mxf; mts/m2ts via mpegts; vob via mpeg-ps; f4v via flv; lrv +
// insv are mp4) — the set was simply under-declared. Verified against
// the runtime image's `ffmpeg -formats`.
func isVideoExt(ext string) bool {
	return dispatch.Has(dispatch.VideoExts, ext)
}
