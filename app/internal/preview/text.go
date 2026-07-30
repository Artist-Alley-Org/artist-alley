// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// Text-file worker — renders the first N lines of a plain-text asset
// onto a terminal-style card and fans it through the standard raster
// ladder. Covers .txt today; .md / .log / .csv are obvious follow-ups
// once we want to ship syntax highlighting or wrapping.
//
// Renderer is basicfont.Face7x13 from x/image — bundled with the Go
// toolchain (no asset deps), monospace, and looks like a terminal so
// users immediately read it as "this is a text file" in the browse
// grid. 7×13 px glyphs on a 1024-px-tall card fit ~70 lines of ~140
// chars each, which is plenty of preview for a README / changelog.

// TextMetadata: line + byte counts. Reading the file once anyway,
// surface the cheap stats.
type TextMetadata struct {
	LineCount   int   `json:"line_count,omitempty"`
	ByteCount   int64 `json:"byte_count,omitempty"`
	IsTruncated bool  `json:"is_truncated,omitempty"`
}

type TextResult struct {
	Variants []string     `json:"variants"`
	Skipped  []string     `json:"skipped"`
	Metadata TextMetadata `json:"metadata"`
	WorkS    float64      `json:"work_s"`
}

type TextHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	TempDir string

	// MaxSourceBytes — 16 MB. Anything bigger than that as plain
	// text is probably a log file or a binary mislabelled as .txt;
	// reject loud.
	MaxSourceBytes int64

	// MaxJobDuration — bytes → pixels → PNG ladder is sub-second.
	// Cap at 30s for any pathological cold-cache I/O.
	MaxJobDuration time.Duration

	// PreviewLines — how many lines of the file to render onto the
	// card. 60 fits nicely in a 1024px-tall card with the 13px
	// glyphs + some line padding.
	PreviewLines int

	// CardWidth / CardHeight — the rendered card's pixel dimensions.
	// 1024×1024 fits the raster ladder's hires variant without
	// over-supplying detail.
	CardWidth  int
	CardHeight int
}

func NewTextHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *TextHandler {
	return &TextHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 16 * 1024 * 1024,
		MaxJobDuration: 30 * time.Second,
		PreviewLines:   60,
		CardWidth:      1024,
		CardHeight:     1024,
	}
}

func (h *TextHandler) Type() jobs.JobType { return jobs.TypePreviewText }

func (h *TextHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p TextPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.text: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.text: file_hash is required")}
	}
	if !isTextExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.text: extension %q not supported", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	h.markProcessing(jobCtx, p.AssetID)

	body, cleanup, err := h.stage(jobCtx, p.FileHash)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}
	defer cleanup()

	result := TextResult{}

	lines, meta := h.readPreview(body)
	result.Metadata = meta

	if ladderDone(jobCtx, h.Storage, p.FileHash, p.Force) {
		result.Skipped = append(result.Skipped, "raster")
	} else {
		img := h.renderCard(lines, meta)
		if err := h.fanCardToLadder(jobCtx, p.AssetID, p.FileHash, img, p.Force); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.text.fan_failed",
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "raster")
		}
	}

	if err := h.persistMetadata(jobCtx, p.AssetID, meta); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.text.persist_meta_failed",
			slog.String("err", err.Error()))
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// Read + sanitize
// ---------------------------------------------------------------------------

// readPreview returns up to PreviewLines lines + a byte/line total.
// Truncates lines wider than what the card can render so the line
// scanner doesn't gobble a giant minified one-liner. Sanitises tabs
// to spaces and strips control codes so the bitmap font doesn't
// render garbled boxes for unprintable bytes.
func (h *TextHandler) readPreview(body []byte) ([]string, TextMetadata) {
	meta := TextMetadata{ByteCount: int64(len(body))}
	// Max chars per line on the card: width / 7 (glyph advance) with
	// a 10-px left margin → roughly (CardWidth-20)/7. Cap generously
	// so wider terminals look readable.
	maxCharsPerLine := (h.CardWidth - 20) / 7

	lines := make([]string, 0, h.PreviewLines)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1*1024*1024)
	for scanner.Scan() {
		meta.LineCount++
		if len(lines) < h.PreviewLines {
			lines = append(lines, sanitizeLine(scanner.Text(), maxCharsPerLine))
		}
	}
	// Note: if scanner errors (oversized line), the line scanner
	// stops but we still have whatever we accumulated. Good enough
	// for a preview — log-perfect rendering isn't the goal.
	if meta.LineCount > h.PreviewLines {
		meta.IsTruncated = true
	}
	return lines, meta
}

// sanitizeLine replaces tabs with spaces, strips control characters,
// and truncates to maxCols. Used to keep the bitmap font from
// rendering box-drawing surprises for unprintable input.
func sanitizeLine(s string, maxCols int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\r':
			// drop — DOS line endings already split by Scanner
		case unicode.IsPrint(r) || r == ' ':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if maxCols > 0 && len(out) > maxCols {
		out = out[:maxCols-1] + "…"
	}
	return out
}

// ---------------------------------------------------------------------------
// Card render
// ---------------------------------------------------------------------------

func (h *TextHandler) renderCard(lines []string, meta TextMetadata) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, h.CardWidth, h.CardHeight))
	bg := color.RGBA{R: 0x14, G: 0x17, B: 0x1C, A: 0xFF}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = bg.R
		img.Pix[i+1] = bg.G
		img.Pix[i+2] = bg.B
		img.Pix[i+3] = bg.A
	}

	face := basicfont.Face7x13
	textColor := color.RGBA{R: 0xE0, G: 0xE5, B: 0xEC, A: 0xFF}
	dimColor := color.RGBA{R: 0x70, G: 0x78, B: 0x82, A: 0xFF}

	// Layout: 10-px margins, 16-px line height (13 glyph + 3 leading).
	const (
		marginX = 10
		startY  = 20
		lineH   = 16
	)
	y := startY
	for _, line := range lines {
		drawTextRow(img, face, line, marginX, y, textColor)
		y += lineH
		if y+lineH > h.CardHeight-30 {
			break
		}
	}
	// Footer with truncation note when the file extends past the
	// preview window — gives a real sense of file size at a glance.
	if meta.IsTruncated {
		footer := fmt.Sprintf("… %d more lines (%d bytes)", meta.LineCount-h.PreviewLines, meta.ByteCount)
		drawTextRow(img, face, footer, marginX, h.CardHeight-12, dimColor)
	}
	return img
}

// drawTextRow draws a single string of text at (x, y) using the
// given font face. y is the baseline.
func drawTextRow(img *image.RGBA, face font.Face, s string, x, y int, c color.Color) {
	if s == "" {
		return
	}
	d := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	d.DrawString(s)
}

// ---------------------------------------------------------------------------
// pipeline plumbing
// ---------------------------------------------------------------------------

func (h *TextHandler) stage(ctx context.Context, hash string) ([]byte, func(), error) {
	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		return nil, nil, fmt.Errorf("stage: download: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		return nil, nil, fmt.Errorf("stage: source %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}
	// Cap reads so an out-of-spec Stat doesn't let us slurp gigabytes.
	body, err := io.ReadAll(io.LimitReader(rc, h.MaxSourceBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("stage: read: %w", err)
	}
	if int64(len(body)) > h.MaxSourceBytes {
		return nil, nil, fmt.Errorf("stage: source exceeded %d bytes during read", h.MaxSourceBytes)
	}
	return body, func() {}, nil
}

func (h *TextHandler) fanCardToLadder(ctx context.Context, assetID uuid.UUID, hash string, src image.Image, force bool) error {
	return fanToLadder(ctx, ladderInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		AssetID: assetID, Hash: hash, Src: src, Kind: "text",
		Overwrite: force,
	})
}

func (h *TextHandler) persistMetadata(ctx context.Context, id uuid.UUID, meta TextMetadata) error {
	payload, err := json.Marshal(map[string]any{"text": meta})
	if err != nil {
		return err
	}
	return assets.New(h.Pool).MergeAssetMetadata(ctx, assets.MergeAssetMetadataParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		Metadata: payload,
	})
}

func (h *TextHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.text.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *TextHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.text.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *TextHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.text.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

func isTextExt(ext string) bool {
	_, ok := dispatch.TextExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

// guard against an unused-import-in-some-builds nit: TempDir isn't
// referenced from stage (we read into memory rather than disk-stage).
// Leaving the field for future symmetry with the other handlers but
// shut the linter up.
var _ = filepath.Join
