// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // cover may be GIF
	_ "image/jpeg" // covers are usually JPEG
	_ "image/png"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
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

// EPUB worker.
//
// An EPUB is just a ZIP archive containing:
//
//   mimetype                       — always "application/epub+zip"
//   META-INF/container.xml         — points at the OPF (package file)
//   <OPF>                          — package manifest + Dublin Core metadata
//   <chapter files>                — XHTML
//   <images/, fonts/, css/>        — assets the chapters reference
//
// We extract:
//   1. Dublin Core metadata (title, creator, language, publisher, date,
//      description, subject, identifier) → JSONB on the asset.
//   2. The cover image, via three discovery paths in priority order:
//        a. OPF <meta name="cover" content="ID"/>, then find the
//           manifest item with that id and grab its href.
//        b. Any manifest item with properties="cover-image" (EPUB 3).
//        c. Any archive file whose path matches /cover\.(png|jpe?g)/i.
//      → fan the bytes through the raster ladder.
//
// Worker is pure-Go (no shell-out). Stays simple, fast, and runs in
// the same container without extra apt packages.

// EPUBPayload — JSON body for a preview.ebook job.
type EPUBPayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

// EPUBMetadata: Dublin Core fields lifted out of the OPF. All
// optional — different publishers populate different subsets, and
// hand-rolled ebooks often only set title + creator.
type EPUBMetadata struct {
	Title       string   `json:"title,omitempty"`
	Creator     string   `json:"creator,omitempty"`  // author
	Language    string   `json:"language,omitempty"` // BCP47 (en, en-US, fr-FR, …)
	Publisher   string   `json:"publisher,omitempty"`
	Date        string   `json:"date,omitempty"`
	Description string   `json:"description,omitempty"` // may be HTML — we strip in the view body
	Subject     []string `json:"subject,omitempty"`     // genres / tags
	ISBN        string   `json:"isbn,omitempty"`        // extracted from dc:identifier when format is isbn:XXX
	Rights      string   `json:"rights,omitempty"`
	HasCover    bool     `json:"has_cover,omitempty"`
}

// EPUBResult — what the worker writes back to jobs.result.
type EPUBResult struct {
	Variants []string     `json:"variants"`
	Skipped  []string     `json:"skipped"`
	Metadata EPUBMetadata `json:"metadata"`
	WorkS    float64      `json:"work_s"`
}

// EPUBHandler implements jobs.Handler for preview.ebook jobs.
type EPUBHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	TempDir string

	// MaxSourceBytes guards against pathologically large ebooks (rare,
	// but a fixed-layout / image-heavy graphic novel can run 100s of
	// MB). 256 MB matches the PDF worker's cap.
	MaxSourceBytes int64

	// MaxJobDuration caps wallclock. Pure-Go cover extraction on a
	// well-formed EPUB is sub-second; the cap exists for pathological
	// zip bombs.
	MaxJobDuration time.Duration
}

// NewEPUBHandler — recommended constructor with sensible defaults.
func NewEPUBHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *EPUBHandler {
	return &EPUBHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 256 * 1024 * 1024,
		MaxJobDuration: 60 * time.Second,
	}
}

func (h *EPUBHandler) Type() jobs.JobType { return jobs.TypePreviewEbook }

// Handle implements jobs.Handler.
func (h *EPUBHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p EPUBPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.ebook: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.ebook: file_hash is required")}
	}
	if !isEbookExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.ebook: extension %q not supported", p.FileExtension)}
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

	result := EPUBResult{}

	meta, coverBytes, err := h.extractEPUB(src)
	if err != nil {
		// Hard fail — if we can't even open the zip, the file is
		// corrupt and there's nothing to ship.
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: fmt.Errorf("epub: %w", err)}
	}
	meta.HasCover = len(coverBytes) > 0
	result.Metadata = meta

	if len(coverBytes) > 0 {
		if h.variantExists(jobCtx, p.FileHash, "col") &&
			h.variantExists(jobCtx, p.FileHash, "preview") &&
			h.variantExists(jobCtx, p.FileHash, "screen") &&
			h.variantExists(jobCtx, p.FileHash, "hires") {
			result.Skipped = append(result.Skipped, "raster")
		} else if err := h.fanCoverToLadder(jobCtx, p.FileHash, coverBytes); err != nil {
			h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.ebook.fan_failed",
				slog.String("asset_id", p.AssetID.String()),
				slog.String("err", err.Error()))
		} else {
			result.Variants = append(result.Variants, "raster")
		}
	} else {
		h.Logger.LogAttrs(jobCtx, slog.LevelInfo, "preview.ebook.no_cover",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("title", meta.Title))
	}

	if err := h.persistMetadata(jobCtx, p.AssetID, meta); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.ebook.persist_meta_failed",
			slog.String("err", err.Error()))
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// Extraction — pure-Go ZIP + XML walk
// ---------------------------------------------------------------------------

// extractEPUB opens the .epub as a zip, parses the OPF, returns the
// Dublin Core metadata + the cover image bytes (nil if no cover).
func (h *EPUBHandler) extractEPUB(src string) (EPUBMetadata, []byte, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return EPUBMetadata{}, nil, fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	// Index files by their path for cheap lookup. EPUB paths are
	// case-sensitive per the spec but we lookup case-sensitively too.
	files := make(map[string]*zip.File, len(r.File))
	for _, f := range r.File {
		files[f.Name] = f
	}

	// Step 1: container.xml → OPF path.
	containerXML, err := readZipFile(files["META-INF/container.xml"])
	if err != nil {
		return EPUBMetadata{}, nil, fmt.Errorf("container.xml: %w", err)
	}
	opfPath, err := parseContainerXML(containerXML)
	if err != nil {
		return EPUBMetadata{}, nil, fmt.Errorf("container.xml parse: %w", err)
	}

	// Step 2: OPF → metadata + manifest.
	opfBytes, err := readZipFile(files[opfPath])
	if err != nil {
		return EPUBMetadata{}, nil, fmt.Errorf("opf %s: %w", opfPath, err)
	}
	meta, manifest, err := parseOPF(opfBytes)
	if err != nil {
		return EPUBMetadata{}, nil, fmt.Errorf("opf parse: %w", err)
	}

	// Step 3: find cover. The OPF holds the cover ID via either
	// `<meta name="cover" content="ID"/>` (EPUB 2/3 hybrid) or a
	// manifest item carrying properties="cover-image" (EPUB 3
	// canonical). Fall back to a path-pattern match.
	opfDir := path.Dir(opfPath)
	var coverHref string
	for _, item := range manifest.Items {
		if strings.Contains(item.Properties, "cover-image") {
			coverHref = item.Href
			break
		}
	}
	if coverHref == "" && manifest.CoverID != "" {
		for _, item := range manifest.Items {
			if item.ID == manifest.CoverID {
				coverHref = item.Href
				break
			}
		}
	}

	// Hrefs in the OPF are relative to the OPF's directory and
	// percent-encoded; resolve them against opfDir.
	var coverBytes []byte
	if coverHref != "" {
		coverBytes = readCover(files, opfDir, coverHref)
	}
	if len(coverBytes) == 0 {
		// Last resort: any file named cover.{jpg,jpeg,png,webp} in
		// the archive. Helps ebooks where the OPF doesn't bind a
		// cover at all (rare but happens with hand-rolled exports).
		for name, f := range files {
			low := strings.ToLower(path.Base(name))
			if low == "cover.jpg" || low == "cover.jpeg" || low == "cover.png" || low == "cover.webp" {
				b, err := readZipFile(f)
				if err == nil {
					coverBytes = b
					break
				}
			}
		}
	}

	return meta, coverBytes, nil
}

// readCover decodes the (possibly percent-encoded) href and reads
// the bytes. Tries the literal path first, then path-decoded.
func readCover(files map[string]*zip.File, opfDir, href string) []byte {
	abs := path.Join(opfDir, href)
	if f, ok := files[abs]; ok {
		if b, err := readZipFile(f); err == nil {
			return b
		}
	}
	if decoded, err := url.PathUnescape(href); err == nil {
		abs2 := path.Join(opfDir, decoded)
		if f, ok := files[abs2]; ok {
			if b, err := readZipFile(f); err == nil {
				return b
			}
		}
	}
	return nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	if f == nil {
		return nil, errors.New("file not found in archive")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Cap individual file reads at 64 MB. Even an oversized cover
	// (full-bleed graphic novel page) shouldn't exceed this.
	return io.ReadAll(io.LimitReader(rc, 64*1024*1024))
}

// ---------------------------------------------------------------------------
// XML parsing
// ---------------------------------------------------------------------------

type containerXML struct {
	XMLName   xml.Name `xml:"container"`
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

func parseContainerXML(b []byte) (string, error) {
	var c containerXML
	if err := xml.Unmarshal(b, &c); err != nil {
		return "", err
	}
	for _, r := range c.Rootfiles {
		if r.FullPath != "" {
			return r.FullPath, nil
		}
	}
	return "", errors.New("no rootfile in container.xml")
}

// opfPackage models the subset of the OPF we care about. We use the
// generic name attributes to be namespace-tolerant — different EPUB
// versions / publishers use different default namespaces, and the
// stdlib xml decoder's namespace handling is famously prickly.
type opfPackage struct {
	XMLName  xml.Name `xml:"package"`
	Metadata opfMeta  `xml:"metadata"`
	Manifest opfMani  `xml:"manifest"`
}

type opfMeta struct {
	Title       []opfText     `xml:"title"`
	Creator     []opfText     `xml:"creator"`
	Language    []opfText     `xml:"language"`
	Publisher   []opfText     `xml:"publisher"`
	Date        []opfText     `xml:"date"`
	Description []opfText     `xml:"description"`
	Subject     []opfText     `xml:"subject"`
	Identifier  []opfText     `xml:"identifier"`
	Rights      []opfText     `xml:"rights"`
	Metas       []opfMetaItem `xml:"meta"`
}

type opfText struct {
	Value string `xml:",chardata"`
}

type opfMetaItem struct {
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	Property string `xml:"property,attr"`
	Refines  string `xml:"refines,attr"`
	Value    string `xml:",chardata"`
}

type opfMani struct {
	Items []opfItem `xml:"item"`
	// CoverID is filled by walking Metadata.Metas for
	// <meta name="cover" content="ID"/>. Lives on the manifest
	// struct (not metadata) because that's where the lookup happens.
	CoverID string `xml:"-"`
}

type opfItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

func parseOPF(b []byte) (EPUBMetadata, opfMani, error) {
	var p opfPackage
	if err := xml.Unmarshal(b, &p); err != nil {
		return EPUBMetadata{}, opfMani{}, err
	}
	meta := EPUBMetadata{
		Title:       firstText(p.Metadata.Title),
		Creator:     firstText(p.Metadata.Creator),
		Language:    firstText(p.Metadata.Language),
		Publisher:   firstText(p.Metadata.Publisher),
		Date:        firstText(p.Metadata.Date),
		Description: firstText(p.Metadata.Description),
		Rights:      firstText(p.Metadata.Rights),
	}
	for _, s := range p.Metadata.Subject {
		v := strings.TrimSpace(s.Value)
		if v != "" {
			meta.Subject = append(meta.Subject, v)
		}
	}
	// ISBN can show up in any dc:identifier; the standard prefix is
	// "isbn:" but we also try urn:isbn: and bare 13-digit numbers.
	for _, id := range p.Metadata.Identifier {
		v := strings.TrimSpace(id.Value)
		low := strings.ToLower(v)
		if strings.HasPrefix(low, "isbn:") {
			meta.ISBN = strings.TrimPrefix(v, "isbn:")
			meta.ISBN = strings.TrimPrefix(meta.ISBN, "ISBN:")
			break
		}
		if strings.HasPrefix(low, "urn:isbn:") {
			meta.ISBN = strings.TrimPrefix(v, "urn:isbn:")
			meta.ISBN = strings.TrimPrefix(meta.ISBN, "URN:ISBN:")
			break
		}
	}
	mani := p.Manifest
	for _, m := range p.Metadata.Metas {
		if strings.EqualFold(m.Name, "cover") && m.Content != "" {
			mani.CoverID = m.Content
			break
		}
	}
	return meta, mani, nil
}

func firstText(xs []opfText) string {
	for _, x := range xs {
		v := strings.TrimSpace(x.Value)
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// pipeline plumbing — mirrors PDFHandler.stage / fanPosterToLadder etc.
// ---------------------------------------------------------------------------

func (h *EPUBHandler) stage(ctx context.Context, hash string) (string, func(), error) {
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-epub-*")
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
	src := filepath.Join(dir, "src.epub")
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

func (h *EPUBHandler) fanCoverToLadder(ctx context.Context, hash string, coverBytes []byte) error {
	cfg, err := h.SysConfig.GetPreviews(ctx)
	if err != nil {
		return fmt.Errorf("load preview config: %w", err)
	}
	src, _, err := image.Decode(bytes.NewReader(coverBytes))
	if err != nil {
		return fmt.Errorf("decode cover: %w", err)
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
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.ebook.encode_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			return fmt.Errorf("backend put ebook variant %s: %w", v.Key, err)
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

func (h *EPUBHandler) persistMetadata(ctx context.Context, id uuid.UUID, meta EPUBMetadata) error {
	payload, err := json.Marshal(map[string]any{"ebook": meta})
	if err != nil {
		return err
	}
	return assets.New(h.Pool).MergeAssetMetadata(ctx, assets.MergeAssetMetadataParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		Metadata: payload,
	})
}

func (h *EPUBHandler) variantExists(ctx context.Context, hash, key string) bool {
	_, err := h.Storage.Backend.Stat(ctx, hash, key)
	return err == nil
}

func (h *EPUBHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.ebook.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *EPUBHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.ebook.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *EPUBHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.ebook.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

func isEbookExt(ext string) bool {
	_, ok := dispatch.EbookExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}
