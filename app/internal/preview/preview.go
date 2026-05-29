// Package preview implements the artist-alley preview-generation
// handlers — the per-format workers that produce sized variants for
// each uploaded asset.
//
// Phase 1.18.A only ships preview.raster (PNG/JPG/GIF/BMP/TIFF).
// Vector / video / audio / PDF / font / 3D land in 1.18.B–D and plug
// in as additional Handlers.
//
// The handlers register against jobs.Registry. Workers (in-process,
// external farm, federated peer) all funnel through the same
// jobs.Service → Handler.Handle path so the variant generation logic
// is owned in exactly one place.
//
// Caching & efficiency:
//
//   - Idempotent on (hash, variant_key). If a variant already exists
//     on the storage backend we skip the re-encode entirely — re-runs
//     are nearly free.
//   - One source decode per job, reused across every variant size.
//   - SkipUpscale (default true): if the original is smaller than a
//     variant's MaxDim we don't upscale; we just re-encode at the
//     native size to land bytes at the URL the FE expects.
//   - Variant URLs are content-addressed, so the variant responder
//     can ship long-lived Cache-Control headers (the variant won't
//     change for a given hash).
package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.n16f.net/thumbhash"
	xdraw "golang.org/x/image/draw"

	// Decoder registrations for image.Decode.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// RasterPayload is the JSON body of a preview.raster job. We carry
// the file_hash + file_extension explicitly so an external worker
// doesn't need to query the assets table.
type RasterPayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

// RasterResult is what the handler writes back to jobs.result on
// success. The admin UI surfaces this for diagnosis.
type RasterResult struct {
	Generated []string `json:"generated"`
	Skipped   []string `json:"skipped"` // already existed
	OriginalW int      `json:"original_w"`
	OriginalH int      `json:"original_h"`
	DurationS float64  `json:"duration_s"`
}

// RasterHandler decodes a single raster source, then writes every
// variant configured in sysconfig.PreviewConfig. Implements
// jobs.Handler.
type RasterHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	// MaxSourceBytes guards against memory explosions when someone
	// uploads a 2 GB TIFF. Defaults to 200 MB.
	MaxSourceBytes int64
}

// NewRasterHandler is the recommended constructor.
func NewRasterHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *RasterHandler {
	return &RasterHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 200 * 1024 * 1024,
	}
}

// Type implements jobs.Handler.
func (h *RasterHandler) Type() jobs.JobType { return jobs.TypePreviewRaster }

// Handle implements jobs.Handler.
func (h *RasterHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p RasterPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.raster: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.raster: file_hash is required")}
	}
	if !isRasterExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.raster: extension %q is not a raster format", p.FileExtension)}
	}

	cfg, err := h.SysConfig.GetPreviews(ctx)
	if err != nil {
		return nil, fmt.Errorf("preview.raster: load config: %w", err)
	}

	h.markProcessing(ctx, p.AssetID)

	src, err := h.loadSourceWithExt(ctx, p.FileHash, p.FileExtension)
	if err != nil {
		h.markFailed(ctx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}

	srcBounds := src.Bounds()
	result := RasterResult{
		OriginalW: srcBounds.Dx(),
		OriginalH: srcBounds.Dy(),
	}

	for _, v := range cfg.Variants {
		if v.Key == storage.VariantOriginal {
			continue
		}
		if h.variantExists(ctx, p.FileHash, v.Key) {
			result.Skipped = append(result.Skipped, v.Key)
			continue
		}
		if err := h.writeVariant(ctx, p.FileHash, src, v); err != nil {
			h.markFailed(ctx, p.AssetID, fmt.Sprintf("variant %s: %v", v.Key, err))
			return nil, fmt.Errorf("preview.raster: variant %s: %w", v.Key, err)
		}
		result.Generated = append(result.Generated, v.Key)
	}

	// Backfill thumbhash on assets that don't already have one. We
	// already paid for the decode; computing thumbhash is sub-ms on
	// top, so this is free for any pipeline that ran the variants.
	// Best-effort: failure here doesn't fail the job.
	h.backfillThumbhash(ctx, p.AssetID, src)

	h.markReady(ctx, p.AssetID)
	result.DurationS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// pipeline steps
// ---------------------------------------------------------------------------

func (h *RasterHandler) loadSource(ctx context.Context, hash string) (image.Image, error) {
	return h.loadSourceWithExt(ctx, hash, "")
}

// loadSourceWithExt is the extension-aware variant. SVG sources are
// rasterised via oksvg + rasterx (pure Go, no cgo, no subprocess) at
// a 2048² square so the downstream variant chain has a high-DPI
// source to resize from. Other formats fall through to image.Decode.
func (h *RasterHandler) loadSourceWithExt(ctx context.Context, hash, ext string) (image.Image, error) {
	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		return nil, fmt.Errorf("download original: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		return nil, fmt.Errorf("source too large: %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}
	r := io.LimitReader(rc, h.MaxSourceBytes+1)
	if strings.EqualFold(strings.TrimPrefix(ext, "."), "svg") {
		return decodeSVG(r)
	}
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return img, nil
}

func (h *RasterHandler) variantExists(ctx context.Context, hash, key string) bool {
	_, err := h.Storage.Backend.Stat(ctx, hash, key)
	return err == nil
}

func (h *RasterHandler) writeVariant(ctx context.Context, hash string, src image.Image, v sysconfig.PreviewVariant) error {
	dst := resizeFor(src, v)
	var buf bytes.Buffer
	contentType, err := encodeImage(&buf, dst, v)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
		return fmt.Errorf("backend put: %w", err)
	}
	if err := storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
		ObjectHash:  hash,
		VariantKey:  v.Key,
		SizeBytes:   int64(buf.Len()),
		ContentType: contentType,
		Metadata:    []byte("{}"),
	}); err != nil {
		return fmt.Errorf("upsert variant row: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// resize + encode
// ---------------------------------------------------------------------------

func resizeFor(src image.Image, v sysconfig.PreviewVariant) image.Image {
	b := src.Bounds()
	w, hh := b.Dx(), b.Dy()
	if w <= 0 || hh <= 0 {
		return src
	}

	switch v.Fit {
	case sysconfig.PreviewFitCover:
		side := v.MaxDim
		if v.SkipUpscale && (w < side || hh < side) {
			side = minInt(w, hh)
		}
		if side <= 0 {
			return src
		}
		return resizeCover(src, side)
	default:
		longest := maxInt(w, hh)
		if v.SkipUpscale && longest <= v.MaxDim {
			return src
		}
		var dw, dh int
		if w >= hh {
			dw = v.MaxDim
			dh = (hh * v.MaxDim) / w
		} else {
			dh = v.MaxDim
			dw = (w * v.MaxDim) / hh
		}
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
		return resizeContain(src, dw, dh)
	}
}

func resizeContain(src image.Image, dw, dh int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

func resizeCover(src image.Image, side int) image.Image {
	b := src.Bounds()
	w, hh := b.Dx(), b.Dy()
	if w <= 0 || hh <= 0 || side <= 0 {
		return src
	}
	var sw, sh int
	if w < hh {
		sw = side
		sh = (hh * side) / w
	} else {
		sh = side
		sw = (w * side) / hh
	}
	scaled := image.NewRGBA(image.Rect(0, 0, sw, sh))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, b, draw.Over, nil)
	x0 := (sw - side) / 2
	y0 := (sh - side) / 2
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	cropped := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(cropped, cropped.Bounds(),
		scaled, image.Pt(x0, y0), draw.Src)
	return cropped
}

func encodeImage(w io.Writer, img image.Image, v sysconfig.PreviewVariant) (string, error) {
	switch v.Format {
	case sysconfig.PreviewFormatPNG:
		if err := png.Encode(w, img); err != nil {
			return "", err
		}
		return "image/png", nil
	default:
		// JPEG (also the fallback for WebP until 1.18.E ships the
		// encoder). JPEG can't carry alpha. If the source actually
		// uses transparency we promote the variant to PNG so it
		// renders correctly against any backdrop instead of getting
		// silently flattened over white. Opaque sources stay on JPEG
		// for the size win.
		if hasAlpha(img) {
			if err := png.Encode(w, img); err != nil {
				return "", err
			}
			return "image/png", nil
		}
		if err := jpeg.Encode(w, img, &jpeg.Options{Quality: v.Quality}); err != nil {
			return "", err
		}
		return "image/jpeg", nil
	}
}

func hasAlpha(img image.Image) bool {
	switch img.(type) {
	case *image.RGBA, *image.NRGBA, *image.RGBA64, *image.NRGBA64:
		// These types CAN carry alpha; check whether any sub-rect
		// pixel is actually non-opaque. For perf we sample every
		// 16th pixel — close enough for the white-flatten decision.
		b := img.Bounds()
		step := 16
		for y := b.Min.Y; y < b.Max.Y; y += step {
			for x := b.Min.X; x < b.Max.X; x += step {
				_, _, _, a := img.At(x, y).RGBA()
				if a < 0xffff {
					return true
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// asset-row updates
// ---------------------------------------------------------------------------

func (h *RasterHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	q := assets.New(h.Pool)
	if err := q.MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.raster.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()),
		)
	}
}

func (h *RasterHandler) markReady(ctx context.Context, id uuid.UUID) {
	q := assets.New(h.Pool)
	if err := q.MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.raster.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()),
		)
	}
}

// backfillThumbhash sets assets.thumbhash if the row currently has
// NULL. Best-effort — failure logs and continues.
func (h *RasterHandler) backfillThumbhash(ctx context.Context, id uuid.UUID, src image.Image) {
	tb := thumbhash.EncodeImage(src)
	if len(tb) == 0 {
		return
	}
	q := assets.New(h.Pool)
	if err := q.SetAssetThumbhashIfMissing(ctx, assets.SetAssetThumbhashIfMissingParams{
		ID:        pgtype.UUID{Bytes: id, Valid: true},
		Thumbhash: tb,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelDebug, "preview.raster.thumbhash_backfill_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()),
		)
	}
}

func (h *RasterHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	q := assets.New(h.Pool)
	if err := q.MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.raster.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()),
		)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// rasterExts is the extension allowlist for preview.raster. Mirrors
// assets.imageExts (kept in sync by convention).
var rasterExts = map[string]struct{}{
	"jpg": {}, "jpeg": {}, "png": {}, "gif": {}, "bmp": {},
	"tif": {}, "tiff": {}, "webp": {},
	"svg": {},
}

func isRasterExt(ext string) bool {
	_, ok := rasterExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
