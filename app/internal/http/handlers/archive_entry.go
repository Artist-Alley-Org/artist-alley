package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/archive"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// ArchiveEntryHandler serves a single entry out of a ZIP / TAR
// archive without ever extracting the whole thing. The user picks
// an entry in the ArchiveView's file tree, the frontend hits
// GET /assets/{id}/archive/entry?path=<relpath>, and this handler
// streams just that file's decompressed bytes back.
//
// Per-archive-kind cost:
//   * ZIP: O(1) — central directory parse + seek to local header.
//     We slurp the whole archive into memory (capped) since
//     archive/zip needs io.ReaderAt; future optimisation:
//     DownloadRange the tail + the entry's data range only.
//   * TAR: O(entries before target) — must scan forward until we
//     hit the matching header. A 1k-entry tar feels instant; a
//     100k-entry tar will take seconds per click. Defer optimisation
//     until we see one in the wild.
//
// MIME inference reuses the same extension table other media
// surfaces use (DocView opens text/code inline; ImageView opens
// pictures; everything else gets a download prompt via the
// Content-Disposition header in the future).
type ArchiveEntryHandler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Logger  *slog.Logger

	// MaxArchiveBytes caps the in-memory ZIP slurp. 4 GiB matches
	// the audiobook + archive preview caps.
	MaxArchiveBytes int64
	// MaxEntryBytes caps a single entry read. Stops a 10 GB tar
	// member from blowing the response writer; we surface a 416
	// "too large — download original archive" instead.
	MaxEntryBytes int64
}

func NewArchiveEntryHandler(pool *pgxpool.Pool, st *storage.Service, logger *slog.Logger) *ArchiveEntryHandler {
	return &ArchiveEntryHandler{
		Pool:            pool,
		Storage:         st,
		Logger:          logger,
		MaxArchiveBytes: 4 * 1024 * 1024 * 1024,
		MaxEntryBytes:   512 * 1024 * 1024,
	}
}

func (h *ArchiveEntryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if auth.IdentityFromContext(r.Context()) == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return
	}
	assetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, `{"error":"invalid asset id"}`, http.StatusBadRequest)
		return
	}
	entryPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if entryPath == "" {
		http.Error(w, `{"error":"missing path query parameter"}`, http.StatusBadRequest)
		return
	}
	// Reject path-traversal attempts up-front — entries are always
	// rooted inside the archive; nothing outside can ever match.
	if strings.HasPrefix(entryPath, "/") || strings.Contains(entryPath, "..") {
		http.Error(w, `{"error":"invalid entry path"}`, http.StatusBadRequest)
		return
	}

	hash, ext, ok, err := h.resolveHashExt(r.Context(), assetID)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	format := archive.ParseFormat(ext)
	if format == "" {
		http.Error(w, `{"error":"asset is not an archive"}`, http.StatusUnprocessableEntity)
		return
	}

	body, info, err := h.Storage.Download(r.Context(), hash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, "archive_entry.download_failed",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()))
		http.Error(w, `{"error":"download failed"}`, http.StatusInternalServerError)
		return
	}
	defer body.Close()
	if info != nil && info.Size > h.MaxArchiveBytes {
		http.Error(w, `{"error":"archive too large to browse — download original"}`, http.StatusRequestEntityTooLarge)
		return
	}

	var rc io.ReadCloser
	var size int64
	switch {
	case format == "zip":
		raw, err := io.ReadAll(io.LimitReader(body, h.MaxArchiveBytes+1))
		if err != nil {
			http.Error(w, `{"error":"archive read failed"}`, http.StatusInternalServerError)
			return
		}
		if int64(len(raw)) > h.MaxArchiveBytes {
			http.Error(w, `{"error":"archive exceeds size cap"}`, http.StatusRequestEntityTooLarge)
			return
		}
		var entryRC io.ReadCloser
		entryRC, zf, err := archive.OpenZIPEntry(bytes.NewReader(raw), int64(len(raw)), entryPath)
		if err != nil {
			http.Error(w, `{"error":"entry not found"}`, http.StatusNotFound)
			return
		}
		if entryRC == nil {
			// Directory entry — nothing to stream.
			http.Error(w, `{"error":"entry is a directory"}`, http.StatusBadRequest)
			return
		}
		rc = entryRC
		size = int64(zf.UncompressedSize64)
	case strings.HasPrefix(format, "tar"):
		compressed := ""
		if format != "tar" {
			compressed = strings.TrimPrefix(format, "tar.")
		}
		entryRC, hdr, err := archive.OpenTARStreamEntry(body, compressed, entryPath)
		if err != nil {
			http.Error(w, `{"error":"entry not found"}`, http.StatusNotFound)
			return
		}
		rc = entryRC
		size = hdr.Size
	default:
		http.Error(w, `{"error":"format not handled"}`, http.StatusUnprocessableEntity)
		return
	}
	defer rc.Close()

	if size > h.MaxEntryBytes {
		http.Error(w, `{"error":"entry too large to stream — download original archive"}`, http.StatusRequestEntityTooLarge)
		return
	}

	// MIME inference from the entry's extension. Falls back to
	// octet-stream so the browser at least offers download.
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(entryPath)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	// Cache by archive hash + entry path — the archive's bytes are
	// content-addressed so the same (hash, path) pair always yields
	// the same bytes.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Etag", fmt.Sprintf(`"/api/v1/assets/%s/archive/entry?%s"`, assetID.String(), entryPath))
	if _, err := io.Copy(w, io.LimitReader(rc, h.MaxEntryBytes)); err != nil {
		// Connection closed mid-stream — log + move on. Headers
		// already flushed; not much else to do.
		h.Logger.LogAttrs(r.Context(), slog.LevelDebug, "archive_entry.copy_short",
			slog.String("err", err.Error()))
	}
}

func (h *ArchiveEntryHandler) resolveHashExt(ctx context.Context, id uuid.UUID) (string, string, bool, error) {
	row, err := assets.New(h.Pool).GetAsset(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	if row.FileHash == nil || *row.FileHash == "" {
		return "", "", false, nil
	}
	ext := ""
	if row.FileExtension != nil {
		ext = strings.ToLower(strings.TrimPrefix(*row.FileExtension, "."))
	}
	return *row.FileHash, ext, true, nil
}
