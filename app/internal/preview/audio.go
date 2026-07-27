// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
	"net/http"
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
	"github.com/mscrnt/artist-alley/app/internal/cue"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/nfo"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
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
	Variants []string      `json:"variants"`
	Skipped  []string      `json:"skipped"`
	Metadata AudioMetadata `json:"metadata"`
	WorkS    float64       `json:"work_s"`
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

	// HasCover signals that the audio file ships an embedded album
	// picture (APIC frame for ID3, METADATA_BLOCK_PICTURE for Vorbis,
	// covr atom for MP4). When true, the worker has extracted it to
	// the "cover" variant and the frontend can fetch
	// /assets/{id}/variants/cover. False (omitted) means no embedded
	// art — the frontend falls back to the waveform-only display.
	HasCover bool `json:"has_cover,omitempty"`

	// Chapters extracted from the container — m4b's chpl atom,
	// MP4 nero-chap track, ID3v2 CHAP frames, Vorbis CHAPTER tags.
	// Populated via ffprobe -show_chapters. Empty for files that
	// don't carry chapter metadata (a regular song or sound effect).
	// The frontend uses this to drive the audiobook reader's
	// chapter strip + the side-panel chapter list; falls back to a
	// single-chapter rendering for chapterless audio.
	Chapters []AudioChapter `json:"chapters,omitempty"`

	// Album metadata folded from a Kodi/Jellyfin album.nfo companion
	// when one is attached to the asset. Carries the canonical
	// audiobook info (book title / author / runtime / sibling
	// track titles + durations / MusicBrainz IDs) so the player can
	// show "Track 3 of 6 · The Dark Tower V" even when individual
	// MP3 ID3 tags are sparse. Empty when no companion exists.
	Album *AlbumInfo `json:"album,omitempty"`

	// ChapterSource is a single-word label the frontend can render
	// in the stats panel. "container" = atoms from the source file
	// itself, "cue" = parsed from a .cue companion. Empty when no
	// chapters were extracted.
	ChapterSource string `json:"chapter_source,omitempty"`

	// SubtitleTracks is a list of subtitle / caption tracks the file
	// ships (rare in audio, common in video — but we surface it on
	// both since the field is shared via the metadata JSONB). Currently
	// populated only from EMBEDDED tracks the container carries; the
	// upcoming Whisper STT phase will write generated tracks into the
	// same slot keyed by their own VTT variant ("sub.<lang>.vtt").
	// The frontend renders one <track kind="subtitles"> per entry.
	SubtitleTracks []SubtitleTrack `json:"subtitle_tracks,omitempty"`
}

// AudioChapter is one chapter entry as ffprobe -show_chapters
// reports it. Start / End are seconds (float). Title comes from
// the chapter's TAGS.title; empty string when missing (we render
// "Chapter N" in that case at the frontend).
type AudioChapter struct {
	ID     int     `json:"id"`
	StartS float64 `json:"start_s"`
	EndS   float64 `json:"end_s"`
	Title  string  `json:"title,omitempty"`
}

// AlbumInfo is the projected album metadata for an audiobook —
// folded from a Kodi-style album.nfo companion or, in a future
// commit, fetched from an online catalogue (Audible / OpenLibrary /
// MusicBrainz). All fields optional.
type AlbumInfo struct {
	Title       string       `json:"title,omitempty"`
	Artist      string       `json:"artist,omitempty"`
	AlbumArtist string       `json:"album_artist,omitempty"`
	Genre       string       `json:"genre,omitempty"`
	Year        string       `json:"year,omitempty"`
	Summary     string       `json:"summary,omitempty"`
	RuntimeS    float64      `json:"runtime_s,omitempty"`
	MBAlbumID   string       `json:"mb_album_id,omitempty"`
	MBArtistID  string       `json:"mb_artist_id,omitempty"`
	MBReleaseID string       `json:"mb_release_id,omitempty"`
	Tracks      []AlbumTrack `json:"tracks,omitempty"`
}

// AlbumTrack is one entry in an album's track listing. Position
// is 1-based.
type AlbumTrack struct {
	Position  int     `json:"position"`
	Title     string  `json:"title,omitempty"`
	DurationS float64 `json:"duration_s,omitempty"`
}

// SubtitleTrack describes one subtitle / caption stream available
// for a media asset. Format is the source codec name ("subrip" /
// "webvtt" / "ass" / "mov_text" — the upcoming Whisper pipeline
// will always emit "webvtt"). VariantKey, when set, points at the
// storage backend key where the track's VTT bytes live (the worker
// extracts container-embedded subs out to standalone VTT variants
// the frontend can <track src> directly); when empty it signals
// "embedded but not yet extracted" — the asset detail can hint at
// it but the frontend can't render it without a fetch.
type SubtitleTrack struct {
	Index      int    `json:"index"`
	Lang       string `json:"lang,omitempty"`
	Title      string `json:"title,omitempty"`
	Format     string `json:"format,omitempty"`
	VariantKey string `json:"variant_key,omitempty"`
	// Source distinguishes container-embedded tracks from generated
	// ones (e.g. Whisper STT). Drives a small "AI"/"CC" pill in the
	// player UI without baking provider names into the frontend.
	Source string `json:"source,omitempty"` // "embedded" | "whisper" | "manual"
}

// AudioHandler renders a waveform poster + fans it through the
// standard raster ladder, then writes audio metadata (duration,
// codec, ID3/Vorbis tags, etc) onto the asset's metadata JSONB.
//
// Pipeline:
//  1. Stage source bytes to a temp file (ffmpeg / ffprobe need a
//     seekable path)
//  2. ffprobe → JSON → AudioMetadata
//  3. ffmpeg's `showwavespic` filter → wide PNG waveform
//  4. fan PNG through col / preview / screen / hires (same encoder
//     used by preview.raster, so transparent waveforms preserve
//     alpha automatically)
//  5. mark asset ready + write metadata
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
		// Audiobooks routinely run 500MB–4GB (m4b at 64-128kbps for 10-30h).
		// 4 GiB covers the long tail without opening us up to truly silly
		// uploads; ffprobe + cover extraction are both stream-based so the
		// cap only matters for the staging copy.
		MaxSourceBytes: 4 * 1024 * 1024 * 1024,
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
	if len(meta.Chapters) > 0 {
		meta.ChapterSource = "container"
	}

	// Companion fold — when the asset has a .nfo / .cue sidecar
	// attached, parse it and stamp the additional metadata. Best-
	// effort: a bad sidecar logs a warning but never aborts the job.
	h.foldCompanions(jobCtx, p.AssetID, &meta)
	result.Metadata = meta

	// Cheap re-queue path: if every variant already exists, skip
	// the (cheap) waveform render.
	//
	// Note this is the one preview handler whose re-queue never
	// reaches fanToLadder, so it is also the one whose re-queue does
	// NOT heal a missing thumbhash (#645). Healing existing assets is
	// the thumbhash-backfill job's remit, not this short-circuit's —
	// re-rendering a waveform on every requeue to recover a 30-byte
	// hash would be the wrong trade.
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
		} else if err := h.fanWaveformToLadder(jobCtx, p.AssetID, p.FileHash, wavePath); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.audio.fan_failed",
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "raster")
		}
	}

	// Extract embedded album art (cover) when ffprobe flagged one.
	// Soft-fail: the asset is still usable without the cover, and
	// the waveform display alone is acceptable (SoundCloud-style).
	// Skip when the variant already exists (re-queue idempotency
	// matches the raster ladder above).
	if meta.HasCover && !h.variantExists(jobCtx, p.FileHash, "cover") {
		if err := h.extractCover(jobCtx, src, p.FileHash); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.audio.cover_failed",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", err.Error()))
			// Demote HasCover so the metadata doesn't promise a
			// variant the frontend can't actually fetch.
			meta.HasCover = false
			result.Metadata = meta
		} else {
			result.Variants = append(result.Variants, "cover")
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
	// One ffprobe call covers everything we need — adding -show_chapters
	// just appends a "chapters" key to the JSON. Audiobook .m4b files
	// take ~50ms to probe; chapter atoms are tiny compared to the
	// streams list.
	cmd := exec.CommandContext(ctx, h.ffprobeBin(),
		"-hide_banner", "-loglevel", "error",
		"-print_format", "json",
		"-show_format", "-show_streams", "-show_chapters",
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
			Duration string            `json:"duration"`
			BitRate  string            `json:"bit_rate"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Index       int               `json:"index"`
			CodecType   string            `json:"codec_type"`
			CodecName   string            `json:"codec_name"`
			SampleRate  string            `json:"sample_rate"`
			Channels    int               `json:"channels"`
			Tags        map[string]string `json:"tags"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Chapters []struct {
			ID        int               `json:"id"`
			StartTime string            `json:"start_time"`
			EndTime   string            `json:"end_time"`
			Tags      map[string]string `json:"tags"`
		} `json:"chapters"`
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
	// Walks every stream once: audio streams fold into codec /
	// sample-rate / channels; attached_pic dispositions flip
	// HasCover; subtitle streams populate SubtitleTracks (extracted
	// from container — Whisper-generated tracks land via a different
	// path in a later phase). One pass keeps it cheap.
	tags := map[string]string{}
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "audio":
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
		case "video":
			// In audio containers a video stream is almost always the
			// attached album picture (APIC/METADATA_BLOCK_PICTURE/covr).
			// Disposition.attached_pic is ffprobe's canonical signal.
			if s.Disposition.AttachedPic == 1 {
				meta.HasCover = true
			}
		case "subtitle":
			meta.SubtitleTracks = append(meta.SubtitleTracks, SubtitleTrack{
				Index:  s.Index,
				Lang:   s.Tags["language"],
				Title:  s.Tags["title"],
				Format: s.CodecName,
				Source: "embedded",
			})
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

	// Fold chapter atoms. Both m4b's native chpl table + MP3's ID3v2
	// CHAP frames surface through the same shape under ffprobe's
	// -show_chapters. Title comes from chapter-level TAGS.title;
	// when blank we leave it empty and let the frontend render
	// "Chapter N".
	if len(raw.Chapters) > 0 {
		chapters := make([]AudioChapter, 0, len(raw.Chapters))
		for i, c := range raw.Chapters {
			start, _ := strconv.ParseFloat(c.StartTime, 64)
			end, _ := strconv.ParseFloat(c.EndTime, 64)
			id := c.ID
			if id == 0 {
				id = i + 1
			}
			title := ""
			for k, v := range c.Tags {
				if strings.EqualFold(k, "title") && v != "" {
					title = v
					break
				}
			}
			chapters = append(chapters, AudioChapter{
				ID: id, StartS: start, EndS: end, Title: title,
			})
		}
		meta.Chapters = chapters
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

// extractCover pulls the embedded album picture out of an audio
// file (APIC for ID3, METADATA_BLOCK_PICTURE for Vorbis/FLAC,
// covr atom for MP4/M4A) and writes it to the "cover" variant.
//
// ffmpeg invocation:
//
//	-an: drop audio streams from the output (we only want the picture)
//	-vn implicit-off: we DO want the video stream (the cover)
//	-frames:v 1: cap at one frame so multi-page TIFF-style covers
//	  don't bloat the output
//	-c copy: pass the embedded picture through without re-encoding —
//	  the embedded format is already JPEG or PNG in 99% of cases and
//	  re-encoding loses quality for no gain. The "cover" variant's
//	  content-type is set from the codec we read out of ffprobe.
//
// Output sits in the same temp dir as the waveform render so the
// existing cleanup defers it cleanly.
func (h *AudioHandler) extractCover(ctx context.Context, src, hash string) error {
	dir := filepath.Dir(src)
	// We don't yet know whether the embedded picture is JPEG or PNG;
	// ffmpeg's -c copy preserves the original encoding. Write to a
	// generic name and sniff the bytes after to set content-type.
	outPath := filepath.Join(dir, "cover.bin")
	cmd := exec.CommandContext(ctx, h.ffmpegBin(),
		"-hide_banner", "-loglevel", "error",
		"-i", src,
		"-an",
		"-map", "0:v?",
		"-frames:v", "1",
		"-c", "copy",
		"-y", outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 400 {
			tail = "..." + tail[len(tail)-400:]
		}
		return fmt.Errorf("ffmpeg cover: %w: %s", err, tail)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Errorf("read cover: %w", err)
	}
	if len(data) == 0 {
		return errors.New("cover extracted but empty")
	}
	ctype := http.DetectContentType(data)
	if !strings.HasPrefix(ctype, "image/") {
		// Defensive: -map 0:v? matches non-image attached pics too
		// (rare, but possible — e.g. video clips embedded as cover).
		// Skip rather than serve them as cover art.
		return fmt.Errorf("cover bytes not an image: %s", ctype)
	}

	if _, err := h.Storage.Backend.Put(ctx, hash, "cover", bytes.NewReader(data)); err != nil {
		return fmt.Errorf("backend put cover: %w", err)
	}
	_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
		ObjectHash:  hash,
		VariantKey:  "cover",
		SizeBytes:   int64(len(data)),
		ContentType: ctype,
		Metadata:    []byte("{}"),
	})
	return nil
}

// fanWaveformToLadder decodes the PNG once, then hands it to the shared
// ladder step, which writes each configured variant (alpha preserved —
// the waveform renders white-on-transparent) and stamps the asset's
// thumbhash.
func (h *AudioHandler) fanWaveformToLadder(ctx context.Context, assetID uuid.UUID, hash, wavePath string) error {
	f, err := os.Open(wavePath)
	if err != nil {
		return fmt.Errorf("open wave: %w", err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode wave: %w", err)
	}
	return fanToLadder(ctx, ladderInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		AssetID: assetID, Hash: hash, Src: src, Kind: "audio",
	})
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

func isAudioExt(ext string) bool {
	_, ok := dispatch.AudioExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

// foldCompanions pulls the asset's companion files, looks for a
// .nfo / .cue sidecar, and folds parsed values into meta. Errors
// at any step are logged + ignored — the asset is still usable
// without the sidecar info.
func (h *AudioHandler) foldCompanions(ctx context.Context, assetID uuid.UUID, meta *AudioMetadata) {
	pgID := pgtype.UUID{Bytes: assetID, Valid: true}
	rows, err := assets.New(h.Pool).ListAssetCompanions(ctx, pgID)
	if err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.companions_list_failed",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()))
		return
	}
	for _, c := range rows {
		path := strings.ToLower(c.CompanionPath)
		switch {
		case strings.HasSuffix(path, ".nfo"):
			data, err := h.downloadCompanion(ctx, c.ObjectHash)
			if err != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.companion_download_failed",
					slog.String("path", c.CompanionPath), slog.String("err", err.Error()))
				continue
			}
			album, err := nfo.ParseAlbum(data)
			if err != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.nfo_parse_failed",
					slog.String("path", c.CompanionPath), slog.String("err", err.Error()))
				continue
			}
			meta.Album = nfoToAlbum(album)
		case strings.HasSuffix(path, ".cue"):
			// Only fall back to .cue when the container didn't ship
			// chapters of its own. Audible m4b exports usually do; the
			// .cue is redundant + the timecodes can disagree by a
			// fraction of a second.
			if len(meta.Chapters) > 0 {
				continue
			}
			data, err := h.downloadCompanion(ctx, c.ObjectHash)
			if err != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.companion_download_failed",
					slog.String("path", c.CompanionPath), slog.String("err", err.Error()))
				continue
			}
			sheet, err := cue.Parse(data)
			if err != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.audio.cue_parse_failed",
					slog.String("path", c.CompanionPath), slog.String("err", err.Error()))
				continue
			}
			meta.Chapters = cueToChapters(sheet, meta.DurationS)
			meta.ChapterSource = "cue"
		}
	}
}

func (h *AudioHandler) downloadCompanion(ctx context.Context, hash string) ([]byte, error) {
	body, _, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	const maxCompanion = 2 * 1024 * 1024 // 2 MB cap — .nfo / .cue are tiny
	return io.ReadAll(io.LimitReader(body, maxCompanion))
}

func nfoToAlbum(a *nfo.Album) *AlbumInfo {
	if a == nil {
		return nil
	}
	out := &AlbumInfo{
		Title:       a.Title,
		Artist:      a.Artist,
		AlbumArtist: a.AlbumArtist,
		Genre:       a.Genre,
		Year:        a.Year,
		Summary:     a.Outline,
		MBAlbumID:   a.MBAlbumID,
		MBArtistID:  a.MBArtistID,
		MBReleaseID: a.MBReleaseID,
		// Kodi convention: <runtime> is minutes. Persist as seconds
		// so the frontend doesn't need to know the source format.
		RuntimeS: a.Runtime * 60,
	}
	if out.Summary == "" {
		out.Summary = a.Review
	}
	for _, t := range a.Tracks {
		out.Tracks = append(out.Tracks, AlbumTrack{
			Position:  t.Position,
			Title:     t.Title,
			DurationS: t.DurationS,
		})
	}
	return out
}

// cueToChapters projects a parsed CUE sheet onto our AudioChapter
// shape. End is the next track's start (or the audio's full
// duration for the last track when we have it).
func cueToChapters(sheet *cue.Sheet, durationS float64) []AudioChapter {
	if sheet == nil || len(sheet.Tracks) == 0 {
		return nil
	}
	out := make([]AudioChapter, 0, len(sheet.Tracks))
	for i, t := range sheet.Tracks {
		end := durationS
		if i+1 < len(sheet.Tracks) {
			end = sheet.Tracks[i+1].StartS
		}
		out = append(out, AudioChapter{
			ID:     t.Number,
			StartS: t.StartS,
			EndS:   end,
			Title:  t.Title,
		})
	}
	return out
}
