// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// ---------------------------------------------------------------------------
// preview.gif (#832)
//
// THE PROBLEM. `gif` sat in dispatch.ImageExts, so every GIF took the
// raster path, and the raster path is Go's image/gif — which decodes the
// FIRST FRAME and returns. For an animated GIF that is not a thumbnail,
// it is a screenshot of the moment before anything happened: for the
// seed's Pong capture, an empty court. And because no sprite sheet was
// ever written, the hover scrub that every video and every 3D model gets
// simply did not exist for the one format on the site whose entire point
// is that it moves.
//
// THE SHAPE. A GIF is two formats wearing one extension, and which one
// you have is a property of the BYTES, not the name. dispatch.PlanForExt
// is extension-only by construction — four enqueue sites call it and not
// one of them can afford to open the file — so the router cannot make
// this call. It routes every GIF here unconditionally and this handler
// makes the call, which it can afford to because it has already staged
// the file.
//
//	still     -> the raster ladder, and stop. A still GIF must not cost
//	             a sprite job, so it does not get one: no ffmpeg process
//	             is spawned on this path at all.
//	animated  -> the raster ladder from a SELECTED frame (not frame one),
//	             plus the same sprite sheet + WebVTT preview.video writes.
//
// WHY NOT A BRANCH IN preview.raster. That handler is the one family in
// the tree that is entirely Go-native — no subprocess, no binary
// dependency — and it is on the path of every JPEG and PNG in the
// system. Teaching it to shell out to ffmpeg for one extension would put
// that dependency in front of all of them. A distinct job type also gets
// a distinct concurrency cap (migration 00027), which a branch could not.
// ---------------------------------------------------------------------------

// GifResult is what the handler writes back to jobs.result.
//
// Animated and Frames are the diagnostic pair: they are the decision the
// handler made and the evidence it made it on, so "why does this GIF have
// no sprite sheet?" is answerable from the admin queue view without
// re-downloading the asset.
type GifResult struct {
	Animated  bool     `json:"animated"`
	Frames    int      `json:"frames"`
	Variants  []string `json:"variants"`
	Skipped   []string `json:"skipped"`
	DurationS float64  `json:"duration_s,omitempty"`
	WorkS     float64  `json:"work_s"`
}

// GifPayload is the shared job body — see dispatch.Payload.
type GifPayload = dispatch.Payload

// GifHandler renders GIFs: a raster ladder always, a hover-scrub sprite
// sheet when the file actually moves.
//
// Idempotent on (hash, variant_key) like every other preview handler —
// every output is checked against the storage backend before it is
// regenerated, and a skipped variant is still reconciled into
// storage_variants (#827).
type GifHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	// FFmpegPath / FFprobePath override the executable lookups. Empty =
	// use `ffmpeg` / `ffprobe` from PATH. Only consulted on the animated
	// path.
	FFmpegPath  string
	FFprobePath string

	// TempDir is where the source is staged. Empty = os.TempDir().
	TempDir string

	// MaxSourceBytes guards against a pathological upload. 200 MB,
	// matching preview.raster — a GIF is an image, and the multi-GB
	// ceiling preview.video needs for ProRes has no business here.
	MaxSourceBytes int64

	// MaxJobDuration is the per-job wallclock cap. A GIF is seconds of
	// silent low-resolution video; 15 minutes is a runaway, not a long
	// encode.
	MaxJobDuration time.Duration
}

// NewGifHandler — recommended constructor with sensible defaults.
func NewGifHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *GifHandler {
	return &GifHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 200 * 1024 * 1024,
		MaxJobDuration: 15 * time.Minute,
	}
}

func (h *GifHandler) Type() jobs.JobType { return jobs.TypePreviewGif }

func (h *GifHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()
	var p GifPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.gif: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.gif: file_hash is required")}
	}
	if !isGifExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.gif: extension %q is not a GIF", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	h.markProcessing(jobCtx, p.AssetID)

	work, cleanup, err := stageSource(jobCtx, h.Storage, h.TempDir, "aa-gif-*", p.FileHash, h.MaxSourceBytes)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}
	defer cleanup()

	frames, err := countGIFFrames(work.sourcePath)
	if err != nil {
		// Not a GIF, or one truncated past the point of reading. Either
		// way no retry will change it.
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.gif: %w", err)}
	}
	result := GifResult{Frames: frames, Animated: frames > 1}

	logAttrs(h.Logger, jobCtx, slog.LevelDebug, "preview.gif.classified",
		slog.String("file_hash", p.FileHash),
		slog.Int("frames", frames),
		slog.Bool("animated", result.Animated))

	if !result.Animated {
		// STILL. The whole job is the raster ladder from the one frame
		// the file has — the same work preview.raster would have done,
		// and deliberately nothing more. No probe, no ffmpeg, no sheet.
		src, err := decodeImageFile(work.sourcePath)
		if err != nil {
			h.markFailed(jobCtx, p.AssetID, err.Error())
			return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.gif: decode still: %w", err)}
		}
		if err := fanToLadder(jobCtx, ladderInput{
			Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
			AssetID: p.AssetID, Hash: p.FileHash, Src: src, Kind: "gif", Source: "still",
			Overwrite: p.Force,
		}); err != nil {
			return nil, fmt.Errorf("preview.gif: ladder: %w", err)
		}
		result.Variants = append(result.Variants, "ladder")
		h.markReady(jobCtx, p.AssetID)
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	// ANIMATED. From here the file is treated as the short silent video
	// it is, through the same helpers preview.video uses.
	probe, err := probeMedia(jobCtx, h.ffprobeBin(), work.sourcePath)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.gif: probe: %w", err)}
	}
	result.DurationS = probe.DurationS

	rendered, err := writeTimelinePoster(jobCtx, timelinePosterInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		FFmpegBin: h.ffmpegBin(), Kind: "gif",
		AssetID: p.AssetID, Hash: p.FileHash, Work: work, Probe: probe, Force: p.Force,
	})
	if err != nil {
		return nil, fmt.Errorf("preview.gif: poster: %w", err)
	}
	if rendered {
		result.Variants = append(result.Variants, "poster")
	} else {
		result.Skipped = append(result.Skipped, "poster")
	}

	switch {
	case variantDone(jobCtx, h.Storage, p.FileHash, "sprites.jpg", p.Force):
		result.Skipped = append(result.Skipped, "sprites")
		// Sheet and cue file are written together and are useless apart,
		// so the skip has to reconcile both (#827).
		variantDone(jobCtx, h.Storage, p.FileHash, "sprites.vtt", false)
	case probe.DurationS <= 0:
		// ffprobe could not measure the animation — a GIF whose frames
		// all declare a zero delay, which some exporters emit. There is
		// no timeline to lay cues on, so there is no honest sheet to
		// write. The ladder above already landed, so the card is fine;
		// only the hover preview is missing, and the frontend gates on
		// the cue file's existence rather than on the extension (#835),
		// so a missing sheet degrades to "no scrub" and not to a 404.
		result.Skipped = append(result.Skipped, "sprites")
		logAttrs(h.Logger, jobCtx, slog.LevelWarn, "preview.gif.sprites_skipped_no_duration",
			slog.String("file_hash", p.FileHash),
			slog.Int("frames", frames))
	default:
		if err := writeSpriteSheet(jobCtx, h.ffmpegBin(), work, probe, func(ctx context.Context, key, path, ct string) error {
			return putVariantFile(ctx, h.Pool, h.Storage, p.FileHash, key, path, ct)
		}); err != nil {
			return nil, fmt.Errorf("preview.gif: sprites: %w", err)
		}
		result.Variants = append(result.Variants, "sprites")
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// Frame counting
// ---------------------------------------------------------------------------

// countGIFFrames reports how many frames a GIF file declares.
//
// It walks the block structure rather than decoding: every frame in a
// GIF is introduced by an Image Descriptor (0x2C), so counting those
// answers "does this move?" without inflating a single pixel. Go's
// image/gif offers no cheaper answer than gif.DecodeAll, which decodes
// EVERY frame into memory — for the seed's 717-frame capture that is
// hundreds of megabytes to learn one boolean. ffprobe would also answer,
// but spawning a process to decide whether we need to spawn a process is
// the wrong shape, and it would make the still path — the common one —
// pay for the animated path's dependency.
//
// Tolerant where the spec is and strict where it is not: a truncated
// file returns the frames it did contain (a GIF that stops mid-stream
// still has usable leading frames, and the decoders in every browser
// render them), but a file that does not start with a GIF header is an
// error, because that is a mis-typed upload rather than a damaged one.
func countGIFFrames(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return countGIFFramesIn(bufio.NewReaderSize(f, 64*1024))
}

func countGIFFramesIn(r io.Reader) (int, error) {
	var header [6]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, fmt.Errorf("gif: read header: %w", err)
	}
	if string(header[:3]) != "GIF" {
		return 0, fmt.Errorf("gif: not a GIF (magic %q)", string(header[:3]))
	}

	// Logical Screen Descriptor: width, height, packed, background,
	// pixel aspect. Bit 7 of `packed` flags a Global Color Table whose
	// size is 3 * 2^(N+1) bytes, N being the low three bits.
	var lsd [7]byte
	if _, err := io.ReadFull(r, lsd[:]); err != nil {
		return 0, fmt.Errorf("gif: read screen descriptor: %w", err)
	}
	if lsd[4]&0x80 != 0 {
		if err := skipBytes(r, int64(3)<<((lsd[4]&0x07)+1)); err != nil {
			return 0, fmt.Errorf("gif: skip global color table: %w", err)
		}
	}

	frames := 0
	for {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			// EOF here is a file with no trailer — truncated, or written
			// by an encoder that omitted it. Report what we counted.
			return frames, nil
		}
		switch b[0] {
		case 0x3B: // trailer — clean end of stream
			return frames, nil

		case 0x21: // extension: a label byte then data sub-blocks
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return frames, nil
			}
			if err := skipGIFSubBlocks(r); err != nil {
				return frames, nil
			}

		case 0x2C: // image descriptor — THIS is a frame
			frames++
			var desc [9]byte
			if _, err := io.ReadFull(r, desc[:]); err != nil {
				return frames, nil
			}
			if desc[8]&0x80 != 0 { // local color table
				if err := skipBytes(r, int64(3)<<((desc[8]&0x07)+1)); err != nil {
					return frames, nil
				}
			}
			if _, err := io.ReadFull(r, b[:]); err != nil { // LZW min code size
				return frames, nil
			}
			if err := skipGIFSubBlocks(r); err != nil {
				return frames, nil
			}

		case 0x00:
			// A stray block terminator. Out of spec at this position but
			// emitted by some old encoders; skipping it is what every
			// decoder in the wild does, and erroring would refuse to
			// preview a file every browser renders.

		default:
			// An unrecognised introducer means we have lost sync with the
			// block structure — anything counted after this point would be
			// noise. Stop and report the frames we are sure of.
			return frames, nil
		}
	}
}

// skipGIFSubBlocks consumes a data sub-block chain: length-prefixed runs
// terminated by a zero-length block.
func skipGIFSubBlocks(r io.Reader) error {
	for {
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return err
		}
		if n[0] == 0 {
			return nil
		}
		if err := skipBytes(r, int64(n[0])); err != nil {
			return err
		}
	}
}

func skipBytes(r io.Reader, n int64) error {
	_, err := io.CopyN(io.Discard, r, n)
	return err
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

func (h *GifHandler) ffmpegBin() string {
	if h.FFmpegPath != "" {
		return h.FFmpegPath
	}
	return "ffmpeg"
}

func (h *GifHandler) ffprobeBin() string {
	if h.FFprobePath != "" {
		return h.FFprobePath
	}
	return "ffprobe"
}

func (h *GifHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		logAttrs(h.Logger, ctx, slog.LevelWarn, "preview.gif.mark_processing_failed",
			slog.String("asset_id", id.String()), slog.String("err", err.Error()))
	}
}

func (h *GifHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		logAttrs(h.Logger, ctx, slog.LevelWarn, "preview.gif.mark_ready_failed",
			slog.String("asset_id", id.String()), slog.String("err", err.Error()))
	}
}

func (h *GifHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		logAttrs(h.Logger, ctx, slog.LevelWarn, "preview.gif.mark_failed_failed",
			slog.String("asset_id", id.String()), slog.String("err", err.Error()))
	}
}

// isGifExt reports whether preview.gif can decode ext. Reads
// dispatch.GifExts — the set that ROUTES work here — rather than a
// private copy, per #362.
func isGifExt(ext string) bool {
	return dispatch.Has(dispatch.GifExts, ext)
}
