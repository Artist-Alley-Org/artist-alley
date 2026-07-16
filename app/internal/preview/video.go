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

// VideoPayload is the JSON body for a preview.video job.
type VideoPayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

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
	posterExists := h.variantExists(jobCtx, p.FileHash, "poster")
	if err := h.writePoster(jobCtx, work, p.FileHash, probe); err != nil {
		return nil, fmt.Errorf("preview.video: poster: %w", err)
	}
	if posterExists {
		result.Skipped = append(result.Skipped, "poster")
	} else {
		result.Variants = append(result.Variants, "poster")
	}

	// --- HLS ladder -------------------------------------------------------
	if h.variantExists(jobCtx, p.FileHash, "hls/master.m3u8") {
		result.Skipped = append(result.Skipped, "hls")
	} else if err := h.writeHLS(jobCtx, work, p.FileHash, probe, enc); err != nil {
		return nil, fmt.Errorf("preview.video: hls: %w", err)
	} else {
		result.Variants = append(result.Variants, "hls")
	}

	// --- scrub sprite + VTT ----------------------------------------------
	if h.variantExists(jobCtx, p.FileHash, "sprites.jpg") {
		result.Skipped = append(result.Skipped, "sprites")
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
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-video-*")
	if err != nil {
		return workDir{}, nil, fmt.Errorf("stage: mkdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: download: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: source %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}

	srcPath := filepath.Join(dir, "src.bin")
	f, err := os.Create(srcPath)
	if err != nil {
		cleanup()
		return workDir{}, nil, fmt.Errorf("stage: create: %w", err)
	}
	if _, err := io.CopyN(f, rc, h.MaxSourceBytes+1); err != nil && !errors.Is(err, io.EOF) {
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
	cmd := exec.CommandContext(ctx, h.ffprobeBin(),
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

func (h *VideoHandler) writePoster(ctx context.Context, w workDir, hash string, probe Probe) error {
	posterPath := filepath.Join(w.dir, "poster.jpg")
	if h.variantExists(ctx, hash, "poster") {
		// Re-extract from source to drive the raster ladder without
		// re-uploading the poster bytes. Fast: one I-frame seek.
		at := 1.0
		if probe.DurationS > 0 && probe.DurationS < 4 {
			at = probe.DurationS * 0.1
		}
		cmd := exec.CommandContext(ctx, h.ffmpegBin(),
			"-hide_banner", "-loglevel", "error", "-y",
			"-ss", fmt.Sprintf("%.3f", at),
			"-i", w.sourcePath,
			"-frames:v", "1",
			"-vf", "scale='min(4096,iw)':'-2'",
			"-q:v", "2",
			posterPath,
		)
		if err := runFFmpeg(cmd); err != nil {
			return err
		}
		return h.writePosterVariants(ctx, hash, posterPath)
	}

	at := 1.0
	if probe.DurationS > 0 && probe.DurationS < 4 {
		at = probe.DurationS * 0.1
	}
	cmd := exec.CommandContext(ctx, h.ffmpegBin(),
		"-hide_banner", "-loglevel", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", at),
		"-i", w.sourcePath,
		"-frames:v", "1",
		// Cap at 4096 so we have headroom for `hires`; the sysconfig
		// variant ladder downsamples from there.
		"-vf", "scale='min(4096,iw)':'-2'",
		"-q:v", "2",
		posterPath,
	)
	if err := runFFmpeg(cmd); err != nil {
		return err
	}
	if err := h.uploadFile(ctx, hash, "poster", posterPath, "image/jpeg"); err != nil {
		return err
	}

	// Drive the raster variant ladder (col / preview / screen / hires)
	// from the poster frame so videos render the same shape as images
	// across every browse + post-detail surface. This is what makes a
	// video card actually look like a card instead of a placeholder.
	return h.writePosterVariants(ctx, hash, posterPath)
}

// writePosterVariants decodes the poster JPEG and runs it through the
// sysconfig.PreviewConfig ladder, reusing the raster handler's
// resizeFor/encodeImage helpers (same package). Variants that already
// exist on the backend are skipped — re-runs are cheap.
func (h *VideoHandler) writePosterVariants(ctx context.Context, hash, posterPath string) error {
	if h.SysConfig == nil {
		return nil // tests may omit; not fatal.
	}
	cfg, err := h.SysConfig.GetPreviews(ctx)
	if err != nil {
		return fmt.Errorf("poster variants: load config: %w", err)
	}

	f, err := os.Open(posterPath)
	if err != nil {
		return fmt.Errorf("poster variants: open: %w", err)
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("poster variants: decode: %w", err)
	}

	for _, v := range cfg.Variants {
		if v.Key == storage.VariantOriginal {
			continue
		}
		if h.variantExists(ctx, hash, v.Key) {
			continue
		}
		dst := resizeFor(src, v)
		var buf bytes.Buffer
		contentType, err := encodeImage(&buf, dst, v)
		if err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.video.poster_variant_encode_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.video.poster_variant_put_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  hash,
			VariantKey:  v.Key,
			SizeBytes:   int64(buf.Len()),
			ContentType: contentType,
			Metadata:    []byte("{}"),
		})
	}
	return nil
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
	spriteCols   = 10
	spriteRows   = 10
	spriteWidth  = 160
	spriteHeight = 90
)

func (h *VideoHandler) writeSprites(ctx context.Context, w workDir, hash string, probe Probe) error {
	if probe.DurationS <= 0 {
		return errors.New("sprites: probe duration is zero")
	}
	totalCells := spriteCols * spriteRows
	// Cell interval in seconds.
	interval := probe.DurationS / float64(totalCells)
	if interval < 0.2 {
		interval = 0.2
	}
	spriteOut := filepath.Join(w.dir, "sprites.jpg")
	// `select='not(mod(n,N))'` is fragile across containers; using
	// `fps=` is more deterministic.
	fps := 1.0 / interval
	cmd := exec.CommandContext(ctx, h.ffmpegBin(),
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", w.sourcePath,
		"-vf", fmt.Sprintf("fps=%f,scale=%d:%d,tile=%dx%d",
			fps, spriteWidth, spriteHeight, spriteCols, spriteRows),
		"-frames:v", "1",
		"-q:v", "5",
		spriteOut,
	)
	if err := runFFmpeg(cmd); err != nil {
		return err
	}
	if err := h.uploadFile(ctx, hash, "sprites.jpg", spriteOut, "image/jpeg"); err != nil {
		return err
	}

	// WebVTT mapping each cell to its time range.
	var vtt bytes.Buffer
	vtt.WriteString("WEBVTT\n\n")
	for i := 0; i < totalCells; i++ {
		start := float64(i) * interval
		end := float64(i+1) * interval
		if end > probe.DurationS {
			end = probe.DurationS
		}
		x := (i % spriteCols) * spriteWidth
		y := (i / spriteCols) * spriteHeight
		fmt.Fprintf(&vtt, "%s --> %s\nsprites.jpg#xywh=%d,%d,%d,%d\n\n",
			vttTime(start), vttTime(end), x, y, spriteWidth, spriteHeight)
		if start >= probe.DurationS {
			break
		}
	}
	vttPath := filepath.Join(w.dir, "sprites.vtt")
	if err := os.WriteFile(vttPath, vtt.Bytes(), 0o644); err != nil {
		return err
	}
	return h.uploadFile(ctx, hash, "sprites.vtt", vttPath, "text/vtt")
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

func (h *VideoHandler) variantExists(ctx context.Context, hash, key string) bool {
	_, err := h.Storage.Backend.Stat(ctx, hash, key)
	return err == nil
}

func (h *VideoHandler) uploadFile(ctx context.Context, hash, key, path, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", key, err)
	}
	defer f.Close()
	if _, err := h.Storage.Backend.Put(ctx, hash, key, f); err != nil {
		return fmt.Errorf("backend put %s: %w", key, err)
	}
	info, err := f.Stat()
	if err == nil {
		_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
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
