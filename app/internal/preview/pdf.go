package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
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

// PDFPayload — JSON body for a preview.pdf job.
type PDFPayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

// PDFResult — what the worker writes back to jobs.result.
type PDFResult struct {
	Variants []string    `json:"variants"`
	Skipped  []string    `json:"skipped"`
	Metadata PDFMetadata `json:"metadata"`
	WorkS    float64     `json:"work_s"`
}

// PDFMetadata: PDF metadata extracted via pdfinfo. Fields all
// optional because some authoring tools strip metadata or never
// populate it (e.g. scanned PDFs).
type PDFMetadata struct {
	NumPages int    `json:"num_pages,omitempty"`
	Title    string `json:"title,omitempty"`
	Author   string `json:"author,omitempty"`
	Subject  string `json:"subject,omitempty"`
	Keywords string `json:"keywords,omitempty"`
	Creator  string `json:"creator,omitempty"`
	Producer string `json:"producer,omitempty"`
	Version  string `json:"version,omitempty"`
	Encrypted bool  `json:"encrypted,omitempty"`
}

// PDFHandler renders the first page of a PDF as a poster + fans it
// through the standard raster ladder, then writes pdfinfo metadata
// onto the asset's metadata JSONB.
//
// Pipeline:
//   1. Stage source to temp file (pdftoppm needs a path)
//   2. pdfinfo → PDFMetadata
//   3. pdftoppm -png -f 1 -l 1 -r 144 → first-page PNG at 144 DPI
//   4. fan PNG through col / preview / screen / hires
//   5. mark asset ready + write metadata
//
// 144 DPI gives a 1224 × 1584 image for a US letter PDF — large
// enough that the hires variant (4096 longest) gets a near-1:1
// downsample, small enough that the render is sub-second.
type PDFHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	PdftoppmPath string
	PdfinfoPath  string

	TempDir string

	// MaxSourceBytes guards against multi-GB uploads. 256 MB covers
	// any reasonable document; full-resolution image PDFs from
	// scanners can push into this range.
	MaxSourceBytes int64

	// MaxJobDuration caps the per-job wallclock. pdftoppm on a
	// well-formed PDF page is ~0.3 s; the cap exists to bail on
	// pathological inputs that lock pdftoppm into a loop.
	MaxJobDuration time.Duration

	// DPI for the first-page render. 144 = 2× standard 72 DPI; for
	// US letter that's 1224×1584 → comfortable headroom for the
	// downsample to col / preview / screen / hires.
	DPI int
}

// NewPDFHandler — recommended constructor with sensible defaults.
func NewPDFHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *PDFHandler {
	return &PDFHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 256 * 1024 * 1024,
		MaxJobDuration: 2 * time.Minute,
		DPI:            144,
	}
}

func (h *PDFHandler) Type() jobs.JobType { return jobs.TypePreviewPDF }

// Handle implements jobs.Handler.
func (h *PDFHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p PDFPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.pdf: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.pdf: file_hash is required")}
	}
	if !strings.EqualFold(strings.TrimPrefix(p.FileExtension, "."), "pdf") {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.pdf: extension %q is not pdf", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	h.markProcessing(jobCtx, p.AssetID)

	src, cleanup, err := h.stage(jobCtx, p.FileHash)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}
	defer cleanup()

	result := PDFResult{}

	// pdfinfo metadata. Soft-fail: a rendered first page is the
	// primary deliverable; missing metadata is decorative.
	meta, err := h.probeMetadata(jobCtx, src)
	if err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.pdf.probe_failed",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("err", err.Error()))
	}
	result.Metadata = meta

	if h.variantExists(jobCtx, p.FileHash, "col") &&
		h.variantExists(jobCtx, p.FileHash, "preview") &&
		h.variantExists(jobCtx, p.FileHash, "screen") &&
		h.variantExists(jobCtx, p.FileHash, "hires") {
		result.Skipped = append(result.Skipped, "raster")
	} else {
		posterPath := filepath.Join(filepath.Dir(src), "page1.png")
		if err := h.renderFirstPage(jobCtx, src, posterPath); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.pdf.render_failed",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", err.Error()))
		} else if err := h.fanPosterToLadder(jobCtx, p.FileHash, posterPath); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.pdf.fan_failed",
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "raster")
		}
	}

	if err := h.persistMetadata(jobCtx, p.AssetID, meta); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.pdf.persist_meta_failed",
			slog.String("err", err.Error()))
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// pipeline steps
// ---------------------------------------------------------------------------

func (h *PDFHandler) stage(ctx context.Context, hash string) (string, func(), error) {
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-pdf-*")
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
	src := filepath.Join(dir, "src.pdf")
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

// probeMetadata runs pdfinfo and parses its line-oriented output.
// Format: "Key:    value" with the keys we look up below.
func (h *PDFHandler) probeMetadata(ctx context.Context, src string) (PDFMetadata, error) {
	cmd := exec.CommandContext(ctx, h.pdfinfoBin(), src)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return PDFMetadata{}, fmt.Errorf("pdfinfo: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	meta := PDFMetadata{}
	for _, line := range strings.Split(out.String(), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		switch key {
		case "Pages":
			if n, err := strconv.Atoi(val); err == nil {
				meta.NumPages = n
			}
		case "Title":
			meta.Title = val
		case "Author":
			meta.Author = val
		case "Subject":
			meta.Subject = val
		case "Keywords":
			meta.Keywords = val
		case "Creator":
			meta.Creator = val
		case "Producer":
			meta.Producer = val
		case "PDF version":
			meta.Version = val
		case "Encrypted":
			meta.Encrypted = !strings.EqualFold(val, "no")
		}
	}
	return meta, nil
}

// renderFirstPage shells out to pdftoppm. -f 1 -l 1 limits to page 1
// (cheaper than rendering every page when we only want a thumbnail).
// -png picks the encoder; -r sets DPI.
func (h *PDFHandler) renderFirstPage(ctx context.Context, src, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// pdftoppm writes to <prefix>-1.png by spec. Trim the .png so
	// the suffix lands where we want it.
	prefix := strings.TrimSuffix(outPath, ".png")
	cmd := exec.CommandContext(ctx, h.pdftoppmBin(),
		"-png", "-f", "1", "-l", "1",
		"-r", strconv.Itoa(h.DPI),
		src, prefix,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 500 {
			tail = "..." + tail[len(tail)-500:]
		}
		return fmt.Errorf("pdftoppm: %w: %s", err, tail)
	}
	// pdftoppm pads with leading zeros once you ask for it, but
	// with -f 1 -l 1 the output is `prefix-1.png` (no padding).
	// Rename to our requested name so the caller has a stable
	// path.
	actual := prefix + "-1.png"
	if _, err := os.Stat(actual); err != nil {
		// Some builds drop the -1 when only one page exists.
		alt := prefix + ".png"
		if _, err := os.Stat(alt); err == nil {
			return os.Rename(alt, outPath)
		}
		return fmt.Errorf("pdftoppm produced no output (looked at %s, %s)", actual, alt)
	}
	return os.Rename(actual, outPath)
}

func (h *PDFHandler) fanPosterToLadder(ctx context.Context, hash, posterPath string) error {
	cfg, err := h.SysConfig.GetPreviews(ctx)
	if err != nil {
		return fmt.Errorf("load preview config: %w", err)
	}
	f, err := os.Open(posterPath)
	if err != nil {
		return fmt.Errorf("open poster: %w", err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode poster: %w", err)
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
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.pdf.encode_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			return fmt.Errorf("backend put pdf variant %s: %w", v.Key, err)
		}
		_ = storage.New(h.Pool).UpsertVariant(ctx, storage.UpsertVariantParams{
			ObjectHash:  hash,
			VariantKey:  v.Key,
			SizeBytes:   int64(buf.Len()),
			ContentType: ctype,
			Metadata:    []byte("{}"),
		})
	}
	// Compile-time poke at image to keep the import-or-warn dance
	// honest for future edits that touch the decode path.
	var _ image.Image = src
	return nil
}

func (h *PDFHandler) persistMetadata(ctx context.Context, id uuid.UUID, meta PDFMetadata) error {
	payload, err := json.Marshal(map[string]any{"pdf": meta})
	if err != nil {
		return err
	}
	return assets.New(h.Pool).MergeAssetMetadata(ctx, assets.MergeAssetMetadataParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		Metadata: payload,
	})
}

func (h *PDFHandler) variantExists(ctx context.Context, hash, key string) bool {
	_, err := h.Storage.Backend.Stat(ctx, hash, key)
	return err == nil
}

func (h *PDFHandler) pdftoppmBin() string {
	if h.PdftoppmPath != "" {
		return h.PdftoppmPath
	}
	return "pdftoppm"
}
func (h *PDFHandler) pdfinfoBin() string {
	if h.PdfinfoPath != "" {
		return h.PdfinfoPath
	}
	return "pdfinfo"
}

func (h *PDFHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.pdf.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *PDFHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.pdf.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *PDFHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.pdf.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

// pdfExts: only one entry today. Kept as a map for symmetry with
// other handler dispatch tables + so isPDFExt has the same shape.
var pdfExts = map[string]struct{}{"pdf": {}}

func isPDFExt(ext string) bool {
	_, ok := pdfExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}

// _ = unused vars surface here so the imports stay honest even
// during refactors.
var _ = []any{&PDFMetadata{}}
