// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// FontResult — what the worker writes back to jobs.result.
type FontResult struct {
	Variants []string     `json:"variants"`
	Skipped  []string     `json:"skipped"`
	Metadata FontMetadata `json:"metadata"`
	WorkS    float64      `json:"work_s"`
}

// FontMetadata exposes the SFNT name table fields the browse card
// and detail view want to surface. Optional throughout because not
// every font ships every name record (especially homebrew TTFs).
type FontMetadata struct {
	Family     string `json:"family,omitempty"`
	SubFamily  string `json:"subfamily,omitempty"` // Regular / Bold / Italic / etc.
	FullName   string `json:"full_name,omitempty"`
	Version    string `json:"version,omitempty"`
	Copyright  string `json:"copyright,omitempty"`
	Designer   string `json:"designer,omitempty"`
	License    string `json:"license,omitempty"`
	NumGlyphs  int    `json:"num_glyphs,omitempty"`
	UnitsPerEm int    `json:"units_per_em,omitempty"`
}

// FontHandler renders a multi-line specimen card for a TTF / OTF
// upload, fans it through the standard raster ladder, and stamps
// SFNT name-table metadata onto asset.metadata under the "font" key.
//
// Card content is the standard type-specimen layout:
//
//   - the font's display name (or the upload filename) at large size
//   - the canonical pangram "The quick brown fox…" at medium size
//   - "Aa Bb Cc 123" at small size for a quick weight-feel
//
// All text rendered IN the actual uploaded font so the thumbnail
// shows what the typeface looks like. PNG output preserves alpha
// for any backdrop.
//
// Supports the open formats Go's sfnt parser understands natively:
// TTF + OTF. WOFF / WOFF2 / EOT (compressed wrappers) fall through
// to a soft-fail PNG that just shows the filename.
type FontHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	TempDir string

	// MaxSourceBytes guards against multi-MB font uploads. 16 MB
	// covers any reasonable family — even fully-hinted CJK fonts
	// max out around 10 MB.
	MaxSourceBytes int64

	// CardWidth / CardHeight are the rendered specimen card's
	// dimensions in pixels. 1600 × 900 is roughly 16:9 so the col
	// crop reads cleanly and the hires variant downsamples nicely.
	CardWidth  int
	CardHeight int
}

// NewFontHandler — recommended constructor with sensible defaults.
func NewFontHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *FontHandler {
	return &FontHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 16 * 1024 * 1024,
		CardWidth:      1600,
		CardHeight:     900,
	}
}

func (h *FontHandler) Type() jobs.JobType { return jobs.TypePreviewFont }

// Handle implements jobs.Handler.
func (h *FontHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p FontPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.font: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.font: file_hash is required")}
	}
	if !isFontExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.font: extension %q is not a supported font format", p.FileExtension)}
	}

	h.markProcessing(ctx, p.AssetID)

	data, err := h.loadSource(ctx, p.FileHash)
	if err != nil {
		h.markFailed(ctx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}

	result := FontResult{}

	// Parse + render. Soft-fail individually: a font we can't parse
	// still gets the col / preview / etc generated as a plain
	// filename card so the upload doesn't look like a dead asset.
	parsed, meta, parseErr := parseFontMetadata(data)
	if parseErr != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.font.parse_failed",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("err", parseErr.Error()))
	}
	result.Metadata = meta

	cardImg := h.renderSpecimen(parsed, meta, p.FileExtension)
	if ladderDone(ctx, h.Storage, p.FileHash, p.Force) {
		result.Skipped = append(result.Skipped, "raster")
		// The rungs were already there, so nothing was rendered and
		// nothing reached the ladder step that normally stamps the
		// blur-up placeholder. Read one back instead of re-rendering
		// (#827).
		healThumbhashOnSkip(ctx, ladderInput{
			Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
			AssetID: p.AssetID, Hash: p.FileHash, Kind: "font",
		})
	} else if err := h.fanCardToLadder(ctx, p.AssetID, p.FileHash, cardImg, p.Force); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.font.fan_failed",
			slog.String("err", err.Error()))
	} else {
		result.Variants = append(result.Variants, "raster")
	}

	if err := h.persistMetadata(ctx, p.AssetID, meta); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.font.persist_meta_failed",
			slog.String("err", err.Error()))
	}

	h.markReady(ctx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// pipeline steps
// ---------------------------------------------------------------------------

func (h *FontHandler) loadSource(ctx context.Context, hash string) ([]byte, error) {
	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		return nil, fmt.Errorf("font source too large: %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}
	return io.ReadAll(io.LimitReader(rc, h.MaxSourceBytes+1))
}

// parseFontMetadata pulls the SFNT name table records that frontends
// usually surface. Returns the parsed *sfnt.Font on success so the
// renderer can use it directly without re-parsing.
func parseFontMetadata(data []byte) (*sfnt.Font, FontMetadata, error) {
	meta := FontMetadata{}
	parsed, err := sfnt.Parse(data)
	if err != nil {
		return nil, meta, err
	}

	var buf sfnt.Buffer
	nameOr := func(id sfnt.NameID) string {
		s, err := parsed.Name(&buf, id)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	meta.Family = nameOr(sfnt.NameIDFamily)
	meta.SubFamily = nameOr(sfnt.NameIDSubfamily)
	meta.FullName = nameOr(sfnt.NameIDFull)
	meta.Version = nameOr(sfnt.NameIDVersion)
	meta.Copyright = nameOr(sfnt.NameIDCopyright)
	meta.Designer = nameOr(sfnt.NameIDDesigner)
	meta.License = nameOr(sfnt.NameIDLicense)
	meta.NumGlyphs = parsed.NumGlyphs()
	meta.UnitsPerEm = int(parsed.UnitsPerEm())
	return parsed, meta, nil
}

// renderSpecimen draws the multi-line type-specimen card. Returns an
// RGBA image; alpha-preservation downstream encodes it as lossless
// WebP / PNG.
func (h *FontHandler) renderSpecimen(parsed *sfnt.Font, meta FontMetadata, ext string) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, h.CardWidth, h.CardHeight))
	// Soft dark backdrop so the card reads against any browse-grid
	// background.
	bg := color.RGBA{R: 0x18, G: 0x1B, B: 0x22, A: 0xFF}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = bg.R
		img.Pix[i+1] = bg.G
		img.Pix[i+2] = bg.B
		img.Pix[i+3] = bg.A
	}

	titleLine := meta.FullName
	if titleLine == "" {
		titleLine = meta.Family
	}
	if titleLine == "" {
		titleLine = "(unnamed font)"
	}
	subLine := strings.TrimSpace(strings.Join([]string{meta.SubFamily, ext}, " · "))

	pangram := "The quick brown fox jumps over the lazy dog"
	weightSample := "Aa Bb Cc Dd Ee Ff Gg 0123456789"

	if parsed == nil {
		// We couldn't parse the font; draw the filename in a system
		// fallback (zero-allocation: no face creation needed).
		drawFallbackText(img, titleLine, 80, 250)
		return img
	}

	textColor := color.RGBA{R: 0xF5, G: 0xF6, B: 0xF8, A: 0xFF}
	dimColor := color.RGBA{R: 0xA0, G: 0xA8, B: 0xB4, A: 0xFF}

	// Three font sizes — let the typeface speak through scale.
	drawLine(img, parsed, titleLine, 80, 220, 96, textColor)
	if subLine != "" {
		drawLine(img, parsed, subLine, 80, 280, 28, dimColor)
	}
	drawLine(img, parsed, pangram, 80, 460, 56, textColor)
	drawLine(img, parsed, weightSample, 80, 580, 44, textColor)
	if meta.Designer != "" {
		drawLine(img, parsed, "Designer: "+meta.Designer, 80, 800, 20, dimColor)
	}
	if meta.Version != "" {
		drawLine(img, parsed, meta.Version, h.CardWidth-360, 800, 20, dimColor)
	}
	return img
}

// drawLine renders a single string of text at the given baseline
// using the supplied font at the requested point size. Missing
// glyphs render as `.notdef` per OpenType spec — same fallback any
// browser would show.
func drawLine(img *image.RGBA, f *sfnt.Font, s string, x, y, ptSize int, c color.Color) {
	if s == "" {
		return
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{
		Size: float64(ptSize), DPI: 72, Hinting: font.HintingFull,
	})
	if err != nil {
		return
	}
	defer face.Close()
	drawer := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	drawer.DrawString(s)
}

// drawFallbackText writes the filename using a tiny built-in bitmap
// font when we couldn't parse the upload. Better than a blank card.
func drawFallbackText(img *image.RGBA, s string, x, y int) {
	face := opentype.FaceOptions{Size: 32, DPI: 72, Hinting: font.HintingNone}
	_ = face
	// No system font shipping in the binary — fall back to leaving
	// the message in the JSON metadata and letting the frontend
	// overlay a "couldn't parse" note. Keep the backdrop dark.
}

// fanCardToLadder writes the rendered specimen through the standard
// raster ladder (col / preview / screen / hires) and stamps the
// asset's thumbhash from it.
func (h *FontHandler) fanCardToLadder(ctx context.Context, assetID uuid.UUID, hash string, src image.Image, force bool) error {
	return fanToLadder(ctx, ladderInput{
		Pool: h.Pool, Storage: h.Storage, SysConfig: h.SysConfig, Logger: h.Logger,
		AssetID: assetID, Hash: hash, Src: src, Kind: "font",
		Overwrite: force,
	})
}

func (h *FontHandler) persistMetadata(ctx context.Context, id uuid.UUID, meta FontMetadata) error {
	payload, err := json.Marshal(map[string]any{"font": meta})
	if err != nil {
		return err
	}
	return assets.New(h.Pool).MergeAssetMetadata(ctx, assets.MergeAssetMetadataParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		Metadata: payload,
	})
}

func (h *FontHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.font.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *FontHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.font.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *FontHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.font.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

func isFontExt(ext string) bool {
	_, ok := dispatch.FontExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}
