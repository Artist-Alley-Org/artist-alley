// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// AssetFileHandler serves /assets/{id}/file with HTTP Range support
// so the browser's <audio>/<video> elements can seek into the middle
// of a large media asset (audiobook .m4b, video .mp4, etc.) without
// downloading the whole file first.
//
// Registered ahead of the openapi-derived mux so it intercepts the
// route before the strict-server's body-stream-only response runs.
// The strict server kept the openapi semantics (auth gate +
// asset-not-found 404 + content-type) consistent with the file
// download API; this handler reuses those for non-Range requests
// and adds 206 Partial Content + Content-Range + Accept-Ranges for
// Range requests.
//
// Backend support: storage.Service.DownloadRange is implemented on
// both fs (os.File.Seek + LimitReader) and s3 (Range: header on
// GetObject) so the cost per range request is "open + seek + copy
// N bytes" with no buffering.
type AssetFileHandler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Logger  *slog.Logger
}

func NewAssetFileHandler(pool *pgxpool.Pool, st *storage.Service, logger *slog.Logger) *AssetFileHandler {
	return &AssetFileHandler{Pool: pool, Storage: st, Logger: logger}
}

func (h *AssetFileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if auth.IdentityFromContext(r.Context()) == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return
	}
	assetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, `{"error":"invalid asset id"}`, http.StatusBadRequest)
		return
	}

	// #433 — sensitivity gates CONTENT. The identity guard above
	// says WHO is asking; this says whether they may have the bytes.
	if !requireContentAccess(w, r, h.Pool, assetID) {
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

	// Get total size + content-type via a metadata-only call (HEAD-
	// like via Get; we close immediately if we don't need the bytes).
	body, info, err := h.Storage.Download(r.Context(), hash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, "asset_file.download.error",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()))
		http.Error(w, `{"error":"download failed"}`, http.StatusInternalServerError)
		return
	}
	size := info.Size
	ct := info.ContentType
	if ct == "" || ct == "application/octet-stream" {
		// Storage didn't capture a meaningful MIME (the upload
		// either omitted X-Content-Type or used the generic octet-
		// stream content type). Browsers refuse to play audio /
		// video sources advertised as octet-stream even when the
		// <source type=> hint matches — fall back to an
		// extension-based table for the media kinds we care about.
		if mapped := mimeForExt(ext); mapped != "" {
			ct = mapped
		} else if ct == "" {
			ct = "application/octet-stream"
		}
	}

	// Common headers — Accept-Ranges advertises seek support to the
	// browser. Content-Length is the FULL size for non-Range requests;
	// the Range branch below overrides with the slice size.
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", ct)
	// Same Cache-Control + ETag as the openapi handler used.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Etag", fmt.Sprintf(`"/api/v1/assets/%s/file"`, assetID.String()))

	rng := r.Header.Get("Range")
	if rng == "" || r.Method == http.MethodHead {
		// No Range — stream the full body. (Even though we requested
		// it above; the body's Reader is still valid.)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, body)
		}
		_ = body.Close()
		return
	}

	// Range request — close the full-body reader, parse the header,
	// and re-fetch just the slice via DownloadRange. Only single-range
	// requests are supported (browsers virtually never send multi-
	// range for media; they negotiate one slice at a time).
	_ = body.Close()
	start, end, ok := parseSingleByteRange(rng, size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	length := end - start + 1
	rangeBody, err := h.Storage.DownloadRange(r.Context(), hash, storage.VariantOriginal, start, length)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, "asset_file.download_range.error",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()))
		http.Error(w, `{"error":"download failed"}`, http.StatusInternalServerError)
		return
	}
	defer rangeBody.Close()

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.Copy(w, rangeBody)
}

func (h *AssetFileHandler) resolveHashExt(ctx context.Context, id uuid.UUID) (string, string, bool, error) {
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

// mimeForExt is a tiny extension→MIME table covering the media
// kinds the viewer plays directly (browsers gate audio/video
// playback on Content-Type even when the <source type> hint
// matches — falling back to an extension lookup avoids the
// "decode failed" path for storage that didn't capture MIME).
func mimeForExt(ext string) string {
	switch ext {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "flac":
		return "audio/flac"
	case "ogg", "oga":
		return "audio/ogg"
	case "opus":
		return "audio/opus"
	case "m4a", "m4b", "aac":
		return "audio/mp4"
	case "aax":
		// Audible source; the browser can't play it, but a
		// real MIME is still better than octet-stream.
		return "audio/vnd.audible.aax"
	case "mp4", "m4v":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mov":
		return "video/quicktime"
	case "mkv":
		return "video/x-matroska"
	}
	return ""
}

// parseSingleByteRange parses the first range in a "bytes=START-END"
// header. END may be missing ("bytes=START-") meaning "to end of
// file". START may be missing ("bytes=-N") meaning "last N bytes".
// Returns (start, end, ok) where end is inclusive (HTTP convention).
func parseSingleByteRange(header string, size int64) (int64, int64, bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimSpace(header[len(prefix):])
	// Multi-range — take only the first slice.
	if i := strings.Index(spec, ","); i >= 0 {
		spec = spec[:i]
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])

	if startStr == "" {
		// Suffix form: bytes=-N — last N bytes of file.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if start >= size {
		return 0, 0, false
	}
	if endStr == "" {
		return start, size - 1, true
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}
