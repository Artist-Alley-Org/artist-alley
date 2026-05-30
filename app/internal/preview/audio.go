package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// AudioPayload — JSON body for a preview.audio job. Matches the
// shape every other preview.* worker uses so the dispatcher in
// assets/handler.go just hands the same struct over.
type AudioPayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

// AudioResult — what the worker writes back into jobs.result for
// the admin queue page.
type AudioResult struct {
	Variants    []string      `json:"variants"`
	Skipped     []string      `json:"skipped"`
	Metadata    AudioMetadata `json:"metadata"`
	WorkS       float64       `json:"work_s"`
}

// AudioMetadata is the extracted track info the asset page will
// display + the search indexer can consume. All fields are optional
// because not every container ships them (e.g. raw WAV has no tags
// at all).
type AudioMetadata struct {
	DurationS    float64           `json:"duration_s"`
	Codec        string            `json:"codec,omitempty"`
	BitrateKbps  int               `json:"bitrate_kbps,omitempty"`
	SampleRateHz int               `json:"sample_rate_hz,omitempty"`
	Channels     int               `json:"channels,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// AudioHandler renders a waveform poster + fans it through the
// standard raster ladder, then writes audio metadata (duration,
// codec, ID3/Vorbis tags, etc) onto the asset's metadata JSONB.
//
// Pipeline:
//   1. Stage source bytes to a temp file (ffmpeg / ffprobe need a
//      seekable path)
//   2. ffprobe → JSON → AudioMetadata
//   3. ffmpeg's `showwavespic` filter → wide PNG waveform
//   4. fan PNG through col / preview / screen / hires (same encoder
//      used by preview.raster, so transparent waveforms preserve
//      alpha automatically)
//   5. mark asset ready + write metadata
//
// The original audio file itself is the asset's `original` variant;
// the frontend's <audio> player streams that directly.
type AudioHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	FFmpegPath  string
	FFprobePath string

	TempDir string

	// MaxSourceBytes guards against multi-GB lossless uploads.
	// Default 500 MB — comfortable for a 90-min FLAC at 1411 kbps.
	MaxSourceBytes int64

	// MaxJobDuration caps the per-job wallclock. ffmpeg's showwavespic
	// runs in ~1-2s per minute of audio on CPU, so 10 minutes covers
	// most uploads with plenty of headroom.
	MaxJobDuration time.Duration

	// WaveformWidth / WaveformHeight set the source-resolution waveform
	// PNG that gets fanned into the variant ladder. 2048 × 384 gives a
	// strong silhouette in screen / hires variants while col stays
	// readable post-downscale.
	WaveformWidth  int
	WaveformHeight int

	// Foreground / Background hex colors for the waveform render. We
	// pick deep indigo on transparent so the variant chain's
	// alpha-preserving encoder lets the card backdrop show through.
	WaveformColor      string // e.g. "0x4f46e5"
	WaveformBackground string // "0x00000000" = transparent
}

// NewAudioHandler — recommended constructor with sensible defaults.
func NewAudioHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *AudioHandler {
	return &AudioHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 500 * 1024 * 1024,
		MaxJobDuration: 10 * time.Minute,
		WaveformWidth:  2048,
		WaveformHeight: 384,
		// Lavc colour pickup: "<rrggbb>@<aa>" where aa is opacity.
		// "ffffff@0.92" reads well on both dark and light backdrops.
		WaveformColor:      "ffffff@0.92",
		WaveformBackground: "00000000",
	}
}

func (h *AudioHandler) Type() jobs.JobType { return jobs.TypePreviewAudio }

// Handle implements jobs.Handler.
func (h *AudioHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p AudioPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.audio: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.audio: file_hash is required")}
	}
	if !isAudioExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.audio: extension %q is not a supported audio format", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	h.markProcessing(jobCtx, p.AssetID)

	src, cleanup, err := h.stage(jobCtx, p.FileHash, p.FileExtension)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}
	defer cleanup()

	result := AudioResult{}

	// ffprobe metadata. Soft-fail: a bad audio file still gets a
	// waveform render below (with empty metadata) rather than aborting
	// the whole job.
	meta, err := h.probeMetadata(jobCtx, src)
	if err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.audio.probe_failed",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("err", err.Error()))
	}
	result.Metadata = meta

	// Cheap re-queue path: if every variant already exists, skip
	// the (cheap) waveform render.
	if h.variantExists(jobCtx, p.FileHash, "col") &&
		h.variantExists(jobCtx, p.FileHash, "preview") &&
		h.variantExists(jobCtx, p.FileHash, "screen") &&
		h.variantExists(jobCtx, p.FileHash, "hires") {
		result.Skipped = append(result.Skipped, "raster")
	} else {
		wavePath := filepath.Join(filepath.Dir(src), "wave.png")
		if err := h.renderWaveform(jobCtx, src, wavePath); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.audio.waveform_failed",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", err.Error()))
		} else if err := h.fanWaveformToLadder(jobCtx, p.FileHash, wavePath); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.audio.fan_failed",
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "raster")
		}
	}

	// Persist metadata onto the asset so the post detail page can
	// show duration / artist / title without re-probing.
	if err := h.persistMetadata(jobCtx, p.AssetID, meta); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.audio.persist_meta_failed",
			slog.String("err", err.Error()))
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// pipeline steps
// ---------------------------------------------------------------------------

// stage downloads the original audio bytes to a temp file. ffmpeg
// needs a seekable input — streaming via stdin works for some
// containers but breaks for MP4/M4A which need to read the moov atom
// from the back of the file.
func (h *AudioHandler) stage(ctx context.Context, hash, ext string) (string, func(), error) {
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-audio-*")
	if err != nil {
		return "", nil, fmt.Errorf("stage: mkdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage: download: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		cleanup()
		return "", nil, fmt.Errorf("stage: source %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}

	srcPath := filepath.Join(dir, "src."+strings.ToLower(strings.TrimPrefix(ext, ".")))
	f, err := os.Create(srcPath)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage: create: %w", err)
	}
	if _, err := io.CopyN(f, rc, h.MaxSourceBytes+1); err != nil && !errors.Is(err, io.EOF) {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("stage: copy: %w", err)
	}
	_ = f.Close()
	return srcPath, cleanup, nil
}

// probeMetadata runs ffprobe -show_format -show_streams and folds
// the JSON into an AudioMetadata. Codec / sample-rate / channels
// come from the first audio stream; tags come from the format-level
// `tags` block (Vorbis / ID3 / MP4 atom).
func (h *AudioHandler) probeMetadata(ctx context.Context, src string) (AudioMetadata, error) {
	cmd := exec.CommandContext(ctx, h.ffprobeBin(),
		"-hide_banner", "-loglevel", "error",
		"-print_format", "json",
		"-show_format", "-show_streams",
		src,
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return AudioMetadata{}, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var raw struct {
		Format struct {
			Duration   string            `json:"duration"`
			BitRate    string            `json:"bit_rate"`
			Tags       map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			CodecType  string            `json:"codec_type"`
			CodecName  string            `json:"codec_name"`
			SampleRate string            `json:"sample_rate"`
			Channels   int               `json:"channels"`
			Tags       map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return AudioMetadata{}, fmt.Errorf("ffprobe parse: %w", err)
	}

	meta := AudioMetadata{}
	if raw.Format.Duration != "" {
		meta.DurationS, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	}
	if raw.Format.BitRate != "" {
		if br, err := strconv.Atoi(raw.Format.BitRate); err == nil {
			meta.BitrateKbps = br / 1000
		}
	}

	// Format-level tags are the canonical place for ID3/Vorbis
	// fields (TITLE, ARTIST, ALBUM, etc); stream-level tags
	// usually duplicate them. Merge with format winning on conflict.
	tags := map[string]string{}
	for _, s := range raw.Streams {
		if s.CodecType != "audio" {
			continue
		}
		if meta.Codec == "" {
			meta.Codec = s.CodecName
		}
		if meta.SampleRateHz == 0 && s.SampleRate != "" {
			meta.SampleRateHz, _ = strconv.Atoi(s.SampleRate)
		}
		if meta.Channels == 0 {
			meta.Channels = s.Channels
		}
		for k, v := range s.Tags {
			if v != "" {
				tags[strings.ToLower(k)] = v
			}
		}
	}
	for k, v := range raw.Format.Tags {
		if v != "" {
			tags[strings.ToLower(k)] = v
		}
	}
	if len(tags) > 0 {
		meta.Tags = tags
	}
	return meta, nil
}

// renderWaveform calls ffmpeg's `showwavespic` filter once to produce
// a single PNG. One full-file render at WaveformWidth × WaveformHeight;
// downstream resizing happens in the existing variant ladder.
//
// Encoder args:
//   - filter colors: "<color>:bg=<bg>" picks foreground + background
//   - split_channels=0: collapse stereo into one combined wave (a
//     stereo display reads as two thin bands and adds visual noise
//     in tiny thumbnails)
//   - -frames:v 1: showwavespic is a single-frame filter; the
//     explicit cap shortcuts cases where ffmpeg would otherwise
//     keep the pipeline open
func (h *AudioHandler) renderWaveform(ctx context.Context, src, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir wave: %w", err)
	}
	filter := fmt.Sprintf("showwavespic=s=%dx%d:colors=%s:split_channels=0",
		h.WaveformWidth, h.WaveformHeight, h.WaveformColor)
	cmd := exec.CommandContext(ctx, h.ffmpegBin(),
		"-hide_banner", "-loglevel", "error",
		"-i", src,
		"-filter_complex", filter,
		"-frames:v", "1",
		"-y", outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 600 {
			tail = "..." + tail[len(tail)-600:]
		}
		return fmt.Errorf("ffmpeg wave: %w: %s", err, tail)
	}
	return nil
}

// fanWaveformToLadder decodes the PNG once, then writes each
// configured variant through the shared encoder so transparent
// waveforms preserve alpha automatically.
func (h *AudioHandler) fanWaveformToLadder(ctx context.Context, hash, wavePath string) error {
	cfg, err := h.SysConfig.GetPreviews(ctx)
	if err != nil {
		return fmt.Errorf("load preview config: %w", err)
	}
	f, err := os.Open(wavePath)
	if err != nil {
		return fmt.Errorf("open wave: %w", err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode wave: %w", err)
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
		ctype, err := encodeImage(&buf, dst, v)
		if err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.encode_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			return fmt.Errorf("backend put waveform variant %s: %w", v.Key, err)
		}
		_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  hash,
			VariantKey:  v.Key,
			SizeBytes:   int64(buf.Len()),
			ContentType: ctype,
			Metadata:    []byte("{}"),
		})
	}
	return nil
}

// persistMetadata merges the probed audio metadata into the asset's
// `metadata` JSONB column under an "audio" key. Existing fields stay
// put — we only own our own namespace.
func (h *AudioHandler) persistMetadata(ctx context.Context, id uuid.UUID, meta AudioMetadata) error {
	payload, err := json.Marshal(map[string]any{"audio": meta})
	if err != nil {
		return err
	}
	q := assets.New(h.Pool)
	return q.MergeAssetMetadata(ctx, assets.MergeAssetMetadataParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		Metadata: payload,
	})
}

func (h *AudioHandler) variantExists(ctx context.Context, hash, key string) bool {
	_, err := h.Storage.Backend.Stat(ctx, hash, key)
	return err == nil
}

func (h *AudioHandler) ffmpegBin() string {
	if h.FFmpegPath != "" {
		return h.FFmpegPath
	}
	return "ffmpeg"
}
func (h *AudioHandler) ffprobeBin() string {
	if h.FFprobePath != "" {
		return h.FFprobePath
	}
	return "ffprobe"
}

func (h *AudioHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *AudioHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *AudioHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

// audioExts: extensions the preview.audio handler accepts. Mirrors
// the frontend AUDIO_EXTS in web/src/lib/components/viewers/controller.ts.
var audioExts = map[string]struct{}{
	"mp3": {}, "wav": {}, "flac": {}, "ogg": {}, "oga": {},
	"m4a": {}, "aac": {}, "opus": {},
}

func isAudioExt(ext string) bool {
	_, ok := audioExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}
