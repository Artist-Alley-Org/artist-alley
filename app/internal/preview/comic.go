package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
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

// Comic-book archive worker (.cbz / .cbr / .cb7).
//
// A comic book archive is just an archive of numbered images:
//   page01.jpg, page02.jpg, …, page99.jpg
//
// Cover is whatever sorts first lexically (almost always the first
// page). We extract that one image and fan it through the standard
// raster ladder.
//
// Format dispatch:
//
//   .cbz  → stdlib archive/zip (zero shell-out)
//   .cbr  → unar (free unarchiver from Debian's main repo; handles
//           RAR3 + RAR5 without the non-free unrar dependency)
//   .cb7  → unar (7-Zip archive)
//
// .cbt (tar) variants exist in the wild but are vanishingly rare;
// queued here when we add them.

type ComicPayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

// ComicMetadata: just the page count + cover filename. Comic Book
// archives sometimes ship a ComicInfo.xml sidecar (created by
// ComicRack and friends) with rich metadata (title, series, issue,
// year, writer, penciler, etc); parsing that lands in a follow-up.
type ComicMetadata struct {
	PageCount   int    `json:"page_count,omitempty"`
	CoverPath   string `json:"cover_path,omitempty"`
	ArchiveKind string `json:"archive_kind,omitempty"` // "zip" | "rar" | "7z"
}

type ComicResult struct {
	Variants []string      `json:"variants"`
	Skipped  []string      `json:"skipped"`
	Metadata ComicMetadata `json:"metadata"`
	WorkS    float64       `json:"work_s"`
}

type ComicHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger

	UnarPath string
	TempDir  string

	// MaxSourceBytes — graphic novels can hit 500+ MB. 1 GB cap.
	MaxSourceBytes int64

	// MaxJobDuration — unar on the first file of a 200-page archive
	// is ~1-2s. 60s ceiling for pathological cases.
	MaxJobDuration time.Duration
}

func NewComicHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *ComicHandler {
	return &ComicHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 1024 * 1024 * 1024,
		MaxJobDuration: 60 * time.Second,
	}
}

func (h *ComicHandler) Type() jobs.JobType { return jobs.TypePreviewComic }

func (h *ComicHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p ComicPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.comic: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.comic: file_hash is required")}
	}
	if !isComicExt(p.FileExtension) {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.comic: extension %q not supported", p.FileExtension)}
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

	result := ComicResult{}

	meta, coverBytes, err := h.extractCover(jobCtx, src, p.FileExtension)
	if err != nil {
		h.markFailed(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.comic: %w", err)}
	}
	result.Metadata = meta

	if len(coverBytes) == 0 {
		// Archive opened fine but had no image entries. Mark ready
		// so the asset isn't stuck in processing; the placeholder
		// thumbnail kicks in client-side.
		h.Logger.LogAttrs(jobCtx, slog.LevelInfo, "preview.comic.no_pages",
			slog.String("asset_id", p.AssetID.String()))
		h.markReady(jobCtx, p.AssetID)
		result.WorkS = time.Since(started).Seconds()
		return json.Marshal(result)
	}

	if h.variantExists(jobCtx, p.FileHash, "col") &&
		h.variantExists(jobCtx, p.FileHash, "preview") &&
		h.variantExists(jobCtx, p.FileHash, "screen") &&
		h.variantExists(jobCtx, p.FileHash, "hires") {
		result.Skipped = append(result.Skipped, "raster")
	} else if err := h.fanCoverToLadder(jobCtx, p.FileHash, coverBytes); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.comic.fan_failed",
			slog.String("err", err.Error()))
	} else {
		result.Variants = append(result.Variants, "raster")
	}

	if err := h.persistMetadata(jobCtx, p.AssetID, meta); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.comic.persist_meta_failed",
			slog.String("err", err.Error()))
	}

	h.markReady(jobCtx, p.AssetID)
	result.WorkS = time.Since(started).Seconds()
	return json.Marshal(result)
}

// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------

// extractCover dispatches by extension: CBZ → stdlib zip, CBR/CB7 →
// unar shell-out. Returns (metadata, cover bytes, err). cover bytes
// is nil if the archive opened OK but contained no images.
func (h *ComicHandler) extractCover(ctx context.Context, src, ext string) (ComicMetadata, []byte, error) {
	e := strings.ToLower(strings.TrimPrefix(ext, "."))
	switch e {
	case "cbz":
		return h.extractCoverZIP(src)
	case "cbr", "cb7":
		return h.extractCoverViaUnar(ctx, src, e)
	}
	return ComicMetadata{}, nil, fmt.Errorf("unsupported comic ext %q", e)
}

// extractCoverZIP walks the archive, picks the lexically-first image
// entry, and returns its bytes.
func (h *ComicHandler) extractCoverZIP(src string) (ComicMetadata, []byte, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return ComicMetadata{}, nil, fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	meta := ComicMetadata{ArchiveKind: "zip"}
	var images []*zip.File
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if isComicPage(f.Name) {
			images = append(images, f)
		}
	}
	if len(images) == 0 {
		return meta, nil, nil
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Name < images[j].Name })
	cover := images[0]
	meta.CoverPath = cover.Name
	meta.PageCount = len(images)

	rc, err := cover.Open()
	if err != nil {
		return meta, nil, fmt.Errorf("open cover %s: %w", cover.Name, err)
	}
	defer rc.Close()
	// Cap defensive read — a single comic page above 64 MB is
	// suspicious (raw scan of a poster maybe). The fan ladder
	// downsamples afterward anyway.
	buf, err := io.ReadAll(io.LimitReader(rc, 64*1024*1024))
	if err != nil {
		return meta, nil, fmt.Errorf("read cover: %w", err)
	}
	return meta, buf, nil
}

// extractCoverViaUnar lists the archive contents to find the cover,
// then extracts that one file. Two unar calls instead of one because
// `unar -o ... -e <pattern>` doesn't have a clean way to extract
// only "the first image" without knowing its name.
func (h *ComicHandler) extractCoverViaUnar(ctx context.Context, src, ext string) (ComicMetadata, []byte, error) {
	meta := ComicMetadata{}
	switch ext {
	case "cbr":
		meta.ArchiveKind = "rar"
	case "cb7":
		meta.ArchiveKind = "7z"
	}

	// `lsar <archive>` prints a header line ("path: FORMAT") followed
	// by one entry path per line. (-p is the password flag, not
	// "print paths" — different tool family from unzip's -p.)
	listCmd := exec.CommandContext(ctx, h.lsarBin(), src)
	var listOut, listErr bytes.Buffer
	listCmd.Stdout = &listOut
	listCmd.Stderr = &listErr
	if err := listCmd.Run(); err != nil {
		tail := strings.TrimSpace(listErr.String())
		if len(tail) > 300 {
			tail = "..." + tail[len(tail)-300:]
		}
		return meta, nil, fmt.Errorf("lsar: %w: %s", err, tail)
	}

	var pages []string
	for i, line := range strings.Split(listOut.String(), "\n") {
		if i == 0 {
			continue // header line (path: FORMAT)
		}
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if isComicPage(name) {
			pages = append(pages, name)
		}
	}
	if len(pages) == 0 {
		return meta, nil, nil
	}
	sort.Strings(pages)
	cover := pages[0]
	meta.CoverPath = cover
	meta.PageCount = len(pages)

	// Extract to a temp dir, then read the file back.
	dir, err := os.MkdirTemp(h.TempDir, "aa-comic-extract-*")
	if err != nil {
		return meta, nil, fmt.Errorf("mkdir extract: %w", err)
	}
	defer os.RemoveAll(dir)

	extractCmd := exec.CommandContext(ctx, h.unarBin(),
		"-o", dir, // output dir
		"-q",   // quiet (no progress text on stdout)
		"-D",   // don't create a wrapping subdirectory
		src,
		cover,
	)
	var exErr bytes.Buffer
	extractCmd.Stderr = &exErr
	if err := extractCmd.Run(); err != nil {
		tail := strings.TrimSpace(exErr.String())
		if len(tail) > 300 {
			tail = "..." + tail[len(tail)-300:]
		}
		return meta, nil, fmt.Errorf("unar: %w: %s", err, tail)
	}

	// unar preserves the archive's path structure under -D=off. With
	// -D, the file lands at <dir>/<basename(cover)>. Some archive
	// layouts still nest the file though, so walk to find it.
	var coverPath string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(p) == filepath.Base(cover) {
			coverPath = p
			return io.EOF // stop early
		}
		return nil
	})
	if coverPath == "" {
		return meta, nil, fmt.Errorf("extracted file not found in %s", dir)
	}
	buf, err := os.ReadFile(coverPath)
	if err != nil {
		return meta, nil, fmt.Errorf("read extracted cover: %w", err)
	}
	return meta, buf, nil
}

// isComicPage matches an archive entry path to a likely page image.
// Comics ship in jpg/png almost exclusively; webp + bmp included for
// completeness. We exclude OS metadata files (__MACOSX, Thumbs.db,
// .DS_Store, ComicInfo.xml) so they don't sort to position 0.
func isComicPage(name string) bool {
	base := path.Base(name)
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
		return false
	}
	if strings.Contains(name, "__MACOSX/") {
		return false
	}
	ext := strings.ToLower(path.Ext(base))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// pipeline plumbing
// ---------------------------------------------------------------------------

func (h *ComicHandler) stage(ctx context.Context, hash, ext string) (string, func(), error) {
	base := h.TempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "aa-comic-*")
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
		e = "cbz"
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

func (h *ComicHandler) fanCoverToLadder(ctx context.Context, hash string, coverBytes []byte) error {
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
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.comic.encode_failed",
				slog.String("variant", v.Key),
				slog.String("err", err.Error()))
			continue
		}
		if _, err := h.Storage.Backend.Put(ctx, hash, v.Key, bytes.NewReader(buf.Bytes())); err != nil {
			return fmt.Errorf("backend put comic variant %s: %w", v.Key, err)
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

func (h *ComicHandler) persistMetadata(ctx context.Context, id uuid.UUID, meta ComicMetadata) error {
	payload, err := json.Marshal(map[string]any{"comic": meta})
	if err != nil {
		return err
	}
	return assets.New(h.Pool).MergeAssetMetadata(ctx, assets.MergeAssetMetadataParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		Metadata: payload,
	})
}

func (h *ComicHandler) variantExists(ctx context.Context, hash, key string) bool {
	_, err := h.Storage.Backend.Stat(ctx, hash, key)
	return err == nil
}

func (h *ComicHandler) unarBin() string {
	if h.UnarPath != "" {
		return h.UnarPath
	}
	return "unar"
}
func (h *ComicHandler) lsarBin() string {
	// lsar ships in the same Debian unar package.
	return "lsar"
}

func (h *ComicHandler) markProcessing(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.comic.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *ComicHandler) markReady(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.comic.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *ComicHandler) markFailed(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.comic.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

var comicExts = map[string]struct{}{
	"cbz": {},
	"cbr": {},
	"cb7": {},
}

func isComicExt(ext string) bool {
	_, ok := comicExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}
