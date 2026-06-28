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

	"github.com/chai2010/webp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.n16f.net/thumbhash"
	xdraw "golang.org/x/image/draw"

	// Decoder registrations for image.Decode.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	metadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/exif"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/orientation"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/raw"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// exifExtractor is the single Phase 1.18.A-2 EXIF extractor used
// for variant-time orientation lookup. Stateless + concurrency-
// safe; one instance per process is enough.
var exifExtractor = exif.New()

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

	src, sourceBytes, err := h.loadSourceWithMeta(ctx, p.FileHash, p.FileExtension)
	if err != nil {
		h.markFailed(ctx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}

	// Phase 1.18.A-2: apply EXIF orientation before any resize /
	// encode. Source bytes are NEVER modified — we rotate the
	// decoded pixels, and stdlib jpeg/png/webp encoders don't
	// write EXIF tags, so the variants are implicitly
	// orientation=1. Failure to extract orientation (no EXIF /
	// unsupported format) leaves the image as-is — best-effort
	// only.
	if len(sourceBytes) > 0 {
		if result, err := exifExtractor.Extract(ctx, bytes.NewReader(sourceBytes), mimeForExt(p.FileExtension)); err == nil {
			if result.Orientation > 1 && result.Orientation <= 8 {
				src = orientation.RotateFromEXIF(src, result.Orientation)
			}
		}
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
	img, _, err := h.loadSourceWithMeta(ctx, hash, ext)
	return img, err
}

// loadSourceWithMeta is the EXIF-aware variant of loadSourceWithExt.
// Returns both the decoded image AND the raw source bytes so the
// caller can extract metadata (orientation, ICC) without a
// second download round-trip. The bytes are nil for formats that
// take a separate decode path (SVG, HDR/EXR) — those don't carry
// EXIF orientation anyway.
//
// For typical preview-pipeline sources (under MaxSourceBytes,
// usually 32 MB), buffering the bytes in memory adds a few MB of
// transient overhead per variant job. Variant generation is
// CPU-bound after this point, so the memory cost is dwarfed by
// the resize/encode passes.
func (h *RasterHandler) loadSourceWithMeta(ctx context.Context, hash, ext string) (image.Image, []byte, error) {
	e := strings.ToLower(strings.TrimPrefix(ext, "."))

	// Raw camera files (CR2 / NEF / DNG / ARW / RW2) carry an
	// embedded JPEG preview baked by the camera ISP. The metadata
	// pipeline extracts that preview to the `embedded-preview`
	// storage variant; the raster pipeline uses it directly instead
	// of trying to demosaic the raw (which would need libraw / CGo).
	//
	// We fall through to ExtractPreviews on the original bytes if
	// the variant isn't yet on storage — happens during backfill
	// before the metadata.extract job has had a chance to run, or
	// when metadata.extract failed for an unrelated reason. The
	// extra cost is one IFD walk, which is microseconds.
	if isRawExt(e) {
		img, err := h.loadRawPreview(ctx, hash, e)
		return img, nil, err
	}

	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		return nil, nil, fmt.Errorf("download original: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		return nil, nil, fmt.Errorf("source too large: %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}
	switch e {
	case "svg":
		img, err := decodeSVG(io.LimitReader(rc, h.MaxSourceBytes+1))
		return img, nil, err
	case "hdr", "exr", "pic":
		// HDR / EXR / Radiance .pic carry float-per-channel pixel
		// data — image.Decode doesn't have a stdlib decoder, and
		// even with one we'd need a tone-map to downscale to the
		// 8-bit-per-channel range every other variant expects.
		// ffmpeg handles both in one call: see decodeHDR.
		img, err := decodeHDR(io.LimitReader(rc, h.MaxSourceBytes+1), e)
		return img, nil, err
	}
	// Buffer the bytes so we can both decode + return them for
	// EXIF extraction without re-downloading.
	raw, err := io.ReadAll(io.LimitReader(rc, h.MaxSourceBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read source: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("decode: %w", err)
	}
	return img, raw, nil
}

// mimeForExt maps the file extension to the MIME type the
// metadata extractor recognises. Returns "application/octet-stream"
// for unknown extensions so the extractor returns
// ErrUnsupportedFormat cleanly (orientation extraction is then
// a no-op, source passes through un-rotated).
func mimeForExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "tif", "tiff":
		return "image/tiff"
	case "webp":
		return "image/webp"
	}
	return "application/octet-stream"
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
	case sysconfig.PreviewFormatWebP:
		// Lossless mode automatically when the source has actual
		// transparency — VP8 lossy bands the alpha channel on hard
		// edges (SVG strokes, transparent PNG icons). Lossy is the
		// size win for opaque photos / waveforms.
		opts := &webp.Options{
			Lossless: hasAlpha(img),
			Quality:  float32(v.Quality),
		}
		if err := webp.Encode(w, img, opts); err != nil {
			return "", err
		}
		return "image/webp", nil
	default:
		// JPEG. Can't carry alpha. If the source actually uses
		// transparency we promote the variant to PNG so it renders
		// correctly against any backdrop instead of getting silently
		// flattened over white. Opaque sources stay on JPEG for the
		// size win.
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
// assets.imageExts (kept in sync by convention). Includes raw-camera
// formats (CR2/NEF/DNG/ARW/RW2) — handled via the embedded-preview
// fallback in loadRawPreview rather than image.Decode.
var rasterExts = map[string]struct{}{
	"jpg": {}, "jpeg": {}, "png": {}, "gif": {}, "bmp": {},
	"tif": {}, "tiff": {}, "webp": {},
	"svg": {},
	"hdr": {}, "exr": {}, "pic": {},
	// Raw camera (Phase 1.18.A-3.B):
	"cr2": {}, "nef": {}, "dng": {}, "arw": {}, "rw2": {},
}

func isRasterExt(ext string) bool {
	_, ok := rasterExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

// rawExts is the raw-camera subset of rasterExts. Used by the
// loadSourceWithMeta dispatch to route raws through
// loadRawPreview instead of the standard image.Decode path.
var rawExts = map[string]struct{}{
	"cr2": {}, "nef": {}, "dng": {}, "arw": {}, "rw2": {},
}

func isRawExt(ext string) bool {
	_, ok := rawExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

// rawExtractor is the shared pure-Go raw preview extractor.
// Stateless + concurrency-safe; one per process is enough.
var rawExtractor = raw.New()

// loadRawPreview returns a decoded image source for raw-camera
// uploads. Tries the persisted `embedded-preview` variant first
// (cheap, ~1 ms storage read); falls back to extracting the
// preview inline from the original raw bytes if the variant
// isn't present yet (the metadata.extract job stamps it, but the
// raster pipeline shouldn't fail just because that job hasn't
// run yet — e.g., during backfill).
func (h *RasterHandler) loadRawPreview(ctx context.Context, hash, ext string) (image.Image, error) {
	if rc, _, err := h.Storage.Download(ctx, hash, metadata.EmbeddedPreviewVariantKey); err == nil {
		defer rc.Close()
		blob, err := io.ReadAll(io.LimitReader(rc, h.MaxSourceBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read embedded-preview: %w", err)
		}
		img, _, err := image.Decode(bytes.NewReader(blob))
		if err != nil {
			return nil, fmt.Errorf("decode embedded-preview: %w", err)
		}
		return img, nil
	}

	// Fallback: extract inline from the original. Slower path but
	// keeps the pipeline working when the metadata job hasn't
	// stamped the variant yet. We don't persist here — the
	// metadata.extract job is the right place to do that so the
	// embedded preview shows up in every consumer including
	// federation peers.
	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		return nil, fmt.Errorf("download raw original: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		return nil, fmt.Errorf("raw source too large: %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}
	srcBytes, err := io.ReadAll(io.LimitReader(rc, h.MaxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read raw original: %w", err)
	}
	mime := raw.MimeTypeForExt(ext)
	if mime == "" {
		return nil, fmt.Errorf("raw: no MIME mapping for extension %q", ext)
	}
	res, err := rawExtractor.Extract(ctx, bytes.NewReader(srcBytes), mime)
	if err != nil {
		return nil, fmt.Errorf("raw extract: %w", err)
	}
	if len(res.PreviewImageBytes) == 0 {
		return nil, fmt.Errorf("raw extract produced no preview bytes")
	}
	img, _, err := image.Decode(bytes.NewReader(res.PreviewImageBytes))
	if err != nil {
		return nil, fmt.Errorf("decode raw preview: %w", err)
	}
	return img, nil
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
