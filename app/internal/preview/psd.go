// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// PSD worker — Photoshop (.psd / .psb).
//
// Two-stage extraction:
//
//   1. Try the embedded thumbnail (Image Resource ID 1036). Adobe
//      writes a flattened JPEG into the resource block when you save
//      from Photoshop proper. Cheap, ~zero CPU.
//
//   2. Fall back to ImageMagick's `convert input.psd[0] -flatten png:-`,
//      which renders the merged composite. Handles the common case
//      where the PSD was saved by Illustrator / Inkscape / icon-kit
//      exporters that skip the thumbnail resource. ~25MB apt install
//      (imagemagick), already in the runtime image.
//
// Both paths produce a PNG → fan through the standard raster ladder.

// PSDResult — what the worker writes back to jobs.result. `source`
// records which extraction path produced the bytes so we can monitor
// how often we hit each (and whether the fast embedded-thumbnail
// path is paying for itself).
type PSDResult struct {
	Variants []string `json:"variants"`
	Skipped  []string `json:"skipped"`
	Source   string   `json:"source"` // "embedded" | "imagemagick"
	WorkS    float64  `json:"work_s"`
}

type PSDHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	ConvertPath string // ImageMagick `convert` binary
	TempDir     string

	// MaxSourceBytes — PSD can balloon (1 GB layered compositions
	// are real). 512 MB cap; bigger files fail loud.
	MaxSourceBytes int64

	// MaxJobDuration — embedded path is sub-second; ImageMagick on
	// a 200-layer PSD can take 30s+. 90s ceiling.
	MaxJobDuration time.Duration
}

func NewPSDHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *PSDHandler {
	return &PSDHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 512 * 1024 * 1024,
		MaxJobDuration: 90 * time.Second,
	}
}

func (h *PSDHandler) Type() jobs.JobType { return jobs.TypePreviewPSD }

func (h *PSDHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p PSDPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.psd: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.psd: file_hash is required")}
	}
	if !isPSDExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.psd: extension %q not supported", p.FileExtension)}
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

	result := PSDResult{}

	if ladderDone(jobCtx, h.Storage, p.FileHash, p.Force) {
		result.Skipped = append(result.Skipped, "raster")
		// The rungs were already there, so nothing was rendered and
		// nothing reached the ladder step that normally stamps the
		// blur-up placeholder. Read one back instead of re-rendering
		// (#827).
		healThumbhashOnSkip(jobCtx, ladderInput{
			Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
			AssetID: p.AssetID, Hash: p.FileHash, Kind: "psd",
		})
		h.markReady(jobCtx, p.AssetID)
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	posterPath := filepath.Join(filepath.Dir(src), "poster.png")

	// Fast path: embedded thumbnail. Most professional Photoshop
	// saves include this — Adobe writes a flattened ~256px JPEG into
	// Image Resource ID 1036.
	if err := h.extractEmbeddedThumbnail(src, posterPath); err == nil {
		result.Source = "embedded"
	} else {
		h.Logger.LogAttrs(jobCtx, slog.LevelDebug, "preview.psd.no_embedded_thumbnail",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("err", err.Error()))
		// Slow path: ImageMagick renders the flattened composite.
		if err := h.renderViaImageMagick(jobCtx, src, posterPath); err != nil {
			h.markFailed(jobCtx, p.AssetID, err.Error())
			return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.psd: imagemagick: %w", err)}
		}
		result.Source = "imagemagick"
	}

	if err := h.fanPosterToLadder(jobCtx, p.AssetID, p.FileHash, posterPath, p.Force); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.psd.fan_failed",
			slog.String("err", err.Error()))
	} else {
		result.Variants = append(result.Variants, "raster")
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// Embedded thumbnail (Image Resource ID 1036)
// ---------------------------------------------------------------------------

// extractEmbeddedThumbnail walks the PSD's Image Resources section
// looking for resource ID 1036 (thumbnail v5/v6). The block layout:
//
//	8BIM signature (4 bytes)
//	resource id  (u16 BE)
//	pascal name  (1-byte length + name bytes, padded to even total)
//	data length  (u32 BE)
//	data         (data length bytes, padded to even)
//
// Resource 1036 data starts with a 28-byte header (format, width,
// height, widthBytes, totalSize, compressedSize, bitsPerPixel,
// planes), then the JPEG data. We decode the JPEG, re-encode as
// PNG, and write it to outPath so the downstream fanPosterToLadder
// shares the PDF/EPS code path.
func (h *PSDHandler) extractEmbeddedThumbnail(psdPath, outPath string) error {
	f, err := os.Open(psdPath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	// Header: skip past sig+version+reserved+channels+h+w+depth+mode
	// (26 bytes total) to the colorModeDataLength field.
	header := make([]byte, 26)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	if string(header[:4]) != "8BPS" {
		return errors.New("not a PSD file (bad signature)")
	}

	var cmdLen uint32
	if err := binary.Read(f, binary.BigEndian, &cmdLen); err != nil {
		return fmt.Errorf("read color mode len: %w", err)
	}
	if _, err := io.CopyN(io.Discard, f, int64(cmdLen)); err != nil {
		return fmt.Errorf("skip color mode: %w", err)
	}

	var irLen uint32
	if err := binary.Read(f, binary.BigEndian, &irLen); err != nil {
		return fmt.Errorf("read image resources len: %w", err)
	}
	// Cap defensive reads — a malformed file claiming 4 GB of
	// image resources shouldn't drag us into a runaway loop.
	if irLen > 64*1024*1024 {
		return fmt.Errorf("image resources length %d exceeds 64 MB", irLen)
	}
	resBlock := make([]byte, irLen)
	if _, err := io.ReadFull(f, resBlock); err != nil {
		return fmt.Errorf("read image resources: %w", err)
	}

	off := 0
	for off+4 <= len(resBlock) {
		if string(resBlock[off:off+4]) != "8BIM" {
			return errors.New("malformed image resources (missing 8BIM)")
		}
		off += 4
		if off+2 > len(resBlock) {
			return errors.New("truncated resource id")
		}
		rid := binary.BigEndian.Uint16(resBlock[off:])
		off += 2
		// Pascal-padded name: 1-byte length, then bytes, then pad
		// so the total (length + name) is even.
		if off+1 > len(resBlock) {
			return errors.New("truncated resource name")
		}
		nameLen := int(resBlock[off])
		off += 1
		// Round (1 + nameLen) up to even.
		total := 1 + nameLen
		pad := 0
		if total%2 != 0 {
			pad = 1
		}
		off += nameLen + pad
		if off+4 > len(resBlock) {
			return errors.New("truncated data length")
		}
		dataLen := binary.BigEndian.Uint32(resBlock[off:])
		off += 4
		if off+int(dataLen) > len(resBlock) {
			return errors.New("truncated resource data")
		}
		data := resBlock[off : off+int(dataLen)]
		off += int(dataLen)
		if int(dataLen)%2 != 0 {
			off++ // even-padding
		}
		if rid == 1036 {
			// Skip the 28-byte thumbnail header; the rest is JPEG.
			if len(data) < 28 {
				return errors.New("thumbnail resource too small for header")
			}
			jpegData := data[28:]
			img, err := jpeg.Decode(bytes.NewReader(jpegData))
			if err != nil {
				return fmt.Errorf("decode embedded jpeg: %w", err)
			}
			out, err := os.Create(outPath)
			if err != nil {
				return fmt.Errorf("create poster: %w", err)
			}
			defer out.Close()
			return png.Encode(out, img)
		}
	}
	return errors.New("no embedded thumbnail (resource 1036 absent)")
}

// ---------------------------------------------------------------------------
// ImageMagick fallback
// ---------------------------------------------------------------------------

// renderViaImageMagick shells out to `convert`. `input[0]` selects the
// merged composite frame (PSDs store this at index 0 by convention).
// `-flatten` ensures we get an opaque PNG even if the merged composite
// somehow has transparency. `-strip` removes metadata so the PNG is
// minimal.
func (h *PSDHandler) renderViaImageMagick(ctx context.Context, src, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	cmd := exec.CommandContext(ctx, h.convertBin(),
		src+"[0]",
		"-flatten",
		"-strip",
		outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 500 {
			tail = "..." + tail[len(tail)-500:]
		}
		return fmt.Errorf("convert: %w: %s", err, tail)
	}
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 {
		return errors.New("convert produced no output")
	}
	return nil
}

// ---------------------------------------------------------------------------
// pipeline plumbing (mirrors PDFHandler)
// ---------------------------------------------------------------------------

func (h *PSDHandler) stage(ctx context.Context, hash, ext string) (string, func(), error) {
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-psd-*")
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
	e := strings.ToLower(strings.TrimPrefix(ext, "."))
	if e == "" {
		e = "psd"
	}
	src := filepath.Join(dir, "src."+e)
	f, err := os.Create(src)
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
	return src, cleanup, nil
}

func (h *PSDHandler) fanPosterToLadder(ctx context.Context, assetID uuid.UUID, hash, posterPath string, force bool) error {
	f, err := os.Open(posterPath)
	if err != nil {
		return fmt.Errorf("open poster: %w", err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode poster: %w", err)
	}
	return fanToLadder(ctx, ladderInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		AssetID: assetID, Hash: hash, Src: src, Kind: "psd",
		Overwrite: force,
	})
}

func (h *PSDHandler) convertBin() string {
	if h.ConvertPath != "" {
		return h.ConvertPath
	}
	return "convert"
}

func (h *PSDHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.psd.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *PSDHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.psd.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *PSDHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.psd.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

func isPSDExt(ext string) bool {
	_, ok := dispatch.PSDExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}
