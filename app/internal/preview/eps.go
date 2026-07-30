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
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// EPS worker — rasterize the first page of an Encapsulated PostScript
// file via Ghostscript, then fan the resulting PNG through the standard
// raster ladder. Routes both .eps and .ps source extensions since the
// container format is identical (EPS is PS + a BoundingBox comment +
// a single-page constraint).
//
// Why Ghostscript: it's the canonical PostScript interpreter, ships
// in Debian's main repo (~30MB install), handles every EPS variant in
// the wild (Illustrator, CorelDRAW, Inkscape, cairo-generated, etc).
// MuPDF's mutool can't render .ps; ImageMagick's `convert` wraps
// Ghostscript anyway, so shelling out directly removes a hop.

// EPSResult — what the worker writes back to jobs.result.
type EPSResult struct {
	Variants []string `json:"variants"`
	Skipped  []string `json:"skipped"`
	WorkS    float64  `json:"work_s"`
}

// EPSHandler implements jobs.Handler for preview.eps.
type EPSHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	GhostscriptPath string
	TempDir         string

	// MaxSourceBytes — defensive cap. Real EPS files almost always
	// fit under 32 MB (icons / logos / vector art); the 128 MB
	// ceiling covers print-ready vector compositions.
	MaxSourceBytes int64

	// MaxJobDuration — Ghostscript can hang on pathological inputs
	// with extreme recursion. 60s ceiling matches the PDF worker.
	MaxJobDuration time.Duration

	// DPI for the rasterized PNG. 144 = 2× standard 72 DPI; on a
	// typical 8.5×11" virtual page that's 1224×1584, plenty of
	// headroom for the col/preview/screen/hires fan.
	DPI int
}

// NewEPSHandler — recommended constructor with sensible defaults.
func NewEPSHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *EPSHandler {
	return &EPSHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 128 * 1024 * 1024,
		MaxJobDuration: 60 * time.Second,
		DPI:            144,
	}
}

func (h *EPSHandler) Type() jobs.JobType { return jobs.TypePreviewEPS }

// Handle implements jobs.Handler.
func (h *EPSHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p EPSPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.eps: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.eps: file_hash is required")}
	}
	if !isEPSExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.eps: extension %q not supported", p.FileExtension)}
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

	result := EPSResult{}

	if ladderDone(jobCtx, h.Storage, p.FileHash, p.Force) {
		result.Skipped = append(result.Skipped, "raster")
	} else {
		posterPath := filepath.Join(filepath.Dir(src), "page1.png")
		if err := h.rasterize(jobCtx, src, posterPath); err != nil {
			h.markFailed(jobCtx, p.AssetID, err.Error())
			return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.eps: rasterize: %w", err)}
		}
		if err := h.fanPosterToLadder(jobCtx, p.AssetID, p.FileHash, posterPath, p.Force); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.eps.fan_failed",
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "raster")
		}
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// pipeline steps
// ---------------------------------------------------------------------------

func (h *EPSHandler) stage(ctx context.Context, hash, ext string) (string, func(), error) {
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-eps-*")
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
		e = "eps"
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

// rasterize shells out to Ghostscript. -dEPSCrop crops the output to
// the EPS BoundingBox (without it Ghostscript pads with letter-size
// whitespace). -dSAFER restricts file operations (PostScript can in
// principle read/write arbitrary files; we run untrusted input).
// -dFirstPage / -dLastPage limit work to page 1 — EPS by definition
// is a single page, but multi-page .ps inputs would otherwise render
// the whole document.
func (h *EPSHandler) rasterize(ctx context.Context, src, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	cmd := exec.CommandContext(ctx, h.ghostscriptBin(),
		"-dNOPAUSE", "-dBATCH", "-dSAFER", "-dQUIET",
		"-dEPSCrop",
		"-dFirstPage=1", "-dLastPage=1",
		"-sDEVICE=png16m",
		"-r"+strconv.Itoa(h.DPI),
		"-sOutputFile="+outPath,
		src,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 500 {
			tail = "..." + tail[len(tail)-500:]
		}
		return fmt.Errorf("ghostscript: %w: %s", err, tail)
	}
	// Sanity: ensure ghostscript actually wrote something.
	st, err := os.Stat(outPath)
	if err != nil || st.Size() == 0 {
		return errors.New("ghostscript produced no output")
	}
	return nil
}

func (h *EPSHandler) fanPosterToLadder(ctx context.Context, assetID uuid.UUID, hash, posterPath string, force bool) error {
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
		AssetID: assetID, Hash: hash, Src: src, Kind: "eps",
		Overwrite: force,
	})
}

func (h *EPSHandler) ghostscriptBin() string {
	if h.GhostscriptPath != "" {
		return h.GhostscriptPath
	}
	return "gs"
}

func (h *EPSHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.eps.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *EPSHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.eps.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *EPSHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.eps.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

func isEPSExt(ext string) bool {
	_, ok := dispatch.EPSExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}
