// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/archive"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// ArchiveBundleHandler streams the entire contents of any supported
// archive (TAR / 7z / RAR / TAR.GZ / TAR.XZ / TAR.BZ2 / ZIP) as a
// freshly-built ZIP — the "Extract all" button in the ArchiveView.
//
// We re-package even ZIP sources because the user's expectation is
// "one click → I get a zip"; the original ZIP file is also available
// via the asset's plain download endpoint, so this overlap is fine.
//
// Streaming end-to-end: tar/rar walk forward through the source +
// pipe each entry into a `zip.Writer` wired straight to the response
// writer, so memory stays bounded regardless of archive size. ZIP +
// 7z sources need ReaderAt — they get slurped into a bounded buffer
// before the zip walk.
type ArchiveBundleHandler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Logger  *slog.Logger

	// MaxArchiveBytes caps the in-memory slurp for ZIP + 7z sources
	// (which need ReaderAt). Matches the entry handler + preview-job
	// cap.
	MaxArchiveBytes int64
}

func NewArchiveBundleHandler(pool *pgxpool.Pool, st *storage.Service, logger *slog.Logger) *ArchiveBundleHandler {
	return &ArchiveBundleHandler{
		Pool:            pool,
		Storage:         st,
		Logger:          logger,
		MaxArchiveBytes: 4 * 1024 * 1024 * 1024,
	}
}

func (h *ArchiveBundleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, "archive_bundle.download_failed",
			slog.String("asset_id", assetID.String()),
			slog.String("err", err.Error()))
		http.Error(w, `{"error":"download failed"}`, http.StatusInternalServerError)
		return
	}
	defer body.Close()
	if info != nil && info.Size > h.MaxArchiveBytes {
		http.Error(w, `{"error":"archive too large to bundle — download original"}`, http.StatusRequestEntityTooLarge)
		return
	}

	filename := suggestBundleName(assetID, ext)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	// Bundling is deterministic for content-addressed sources but the
	// writer doesn't emit fixed offsets (entry order matches source
	// walk); ETag would only be safe if we precomputed the output.
	// Skip cache headers — browsers shouldn't re-cache big downloads.
	w.Header().Set("Cache-Control", "no-store")

	zw := zip.NewWriter(w)
	defer zw.Close()

	switch {
	case format == "zip" || format == "7z":
		raw, err := io.ReadAll(io.LimitReader(body, h.MaxArchiveBytes+1))
		if err != nil {
			h.bundleErr(r.Context(), assetID, "read", err)
			return
		}
		if int64(len(raw)) > h.MaxArchiveBytes {
			h.bundleErr(r.Context(), assetID, "cap", fmt.Errorf("archive exceeds size cap"))
			return
		}
		if err := archive.WriteBundleZipReaderAt(bytes.NewReader(raw), int64(len(raw)), format, zw); err != nil {
			h.bundleErr(r.Context(), assetID, "walk", err)
			return
		}
	default:
		if err := archive.WriteBundleZip(body, format, zw); err != nil {
			h.bundleErr(r.Context(), assetID, "walk", err)
			return
		}
	}
}

func (h *ArchiveBundleHandler) bundleErr(ctx context.Context, id uuid.UUID, stage string, err error) {
	// Headers are already flushed by the time we reach the walk — we
	// can't switch to a 500. Log + abort; the browser sees a truncated
	// zip and shows a corruption error, which is the right signal.
	h.Logger.LogAttrs(ctx, slog.LevelWarn, "archive_bundle.failed",
		slog.String("asset_id", id.String()),
		slog.String("stage", stage),
		slog.String("err", err.Error()))
}

func (h *ArchiveBundleHandler) resolveHashExt(ctx context.Context, id uuid.UUID) (string, string, bool, error) {
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

// suggestBundleName builds a friendly filename for the Save-as
// dialog. We don't have the original upload filename here (would
// need a second query); the asset id prefix + "bundle" is fine.
func suggestBundleName(id uuid.UUID, ext string) string {
	short := id.String()
	if len(short) > 8 {
		short = short[:8]
	}
	if ext == "" {
		ext = "archive"
	}
	return fmt.Sprintf("%s-%s-bundle.zip", short, ext)
}
