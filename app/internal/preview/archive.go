// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Archive preview worker — extracts a Manifest of every entry in
// a zip / tar / tar.gz file and persists it as
// metadata.archive on the asset row. The ArchiveView reads this
// directly off the asset GET response to render the file-tree
// browser; per-entry extraction streams via a dedicated handler
// in app/internal/assets/archive.go (no preview job needed for
// individual files).
//
// No raster rendering today — the browse-card placeholder ("ZIP",
// "TAR" badge on a typed-doc tile) is fine. Future enhancement:
// render an entry-count + first-file hint onto a card via the
// same bitmap-font path text.go uses.

package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/archive"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

type ArchivePayload struct {
	AssetID       uuid.UUID `json:"asset_id"`
	FileHash      string    `json:"file_hash"`
	FileExtension string    `json:"file_extension"`
}

type ArchiveResult struct {
	Variants  []string          `json:"variants,omitempty"`
	Skipped   []string          `json:"skipped,omitempty"`
	Manifest  *archive.Manifest `json:"manifest,omitempty"`
	WorkS     float64           `json:"work_s"`
}

type ArchiveHandler struct {
	Pool      *pgxpool.Pool
	Storage   *storage.Service
	SysConfig *sysconfig.Store
	Logger    *slog.Logger
	// MaxSourceBytes — 4 GiB cap matches the audiobook handler.
	// ZIP parsing is constant-cost in archive size (only the
	// trailing central directory matters); TAR scanning is O(N)
	// so a multi-GB tar with millions of small entries can take
	// minutes. The MaxEntries cap (10k entries, in archive/) bails
	// the scan early.
	MaxSourceBytes int64
	MaxJobDuration time.Duration
}

func NewArchiveHandler(pool *pgxpool.Pool, st *storage.Service, sc *sysconfig.Store, logger *slog.Logger) *ArchiveHandler {
	return &ArchiveHandler{
		Pool: pool, Storage: st, SysConfig: sc, Logger: logger,
		MaxSourceBytes: 4 * 1024 * 1024 * 1024,
		MaxJobDuration: 5 * time.Minute,
	}
}

func (h *ArchiveHandler) Type() jobs.JobType { return jobs.TypePreviewArchive }

func (h *ArchiveHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	started := time.Now()

	var p ArchivePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.archive: bad payload: %w", err)}
	}
	if p.FileHash == "" {
		return nil, &jobs.TerminalError{Err: errors.New("preview.archive: file_hash is required")}
	}
	format := archive.ParseFormat(p.FileExtension)
	if format == "" {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("preview.archive: unsupported extension %q", p.FileExtension)}
	}

	jobCtx, cancel := context.WithTimeout(ctx, h.MaxJobDuration)
	defer cancel()

	h.markProcessingArchive(jobCtx, p.AssetID)

	manifest, err := h.extractManifest(jobCtx, p.FileHash, format)
	if err != nil {
		h.markFailedArchive(jobCtx, p.AssetID, err.Error())
		return nil, &jobs.TerminalError{Err: err}
	}

	if err := h.persistArchiveMetadata(jobCtx, p.AssetID, manifest); err != nil {
		h.Logger.LogAttrs(jobCtx, slog.LevelWarn, "preview.archive.persist_failed",
			slog.String("asset_id", p.AssetID.String()),
			slog.String("err", err.Error()))
	}
	h.markReadyArchive(jobCtx, p.AssetID)

	return json.Marshal(ArchiveResult{
		Manifest: manifest,
		WorkS:    time.Since(started).Seconds(),
	})
}

// extractManifest streams the asset bytes from storage + hands
// them to the right archive parser. ZIP gets a bytes.Reader so
// archive/zip can read the central directory at the end; TAR
// streams forward.
func (h *ArchiveHandler) extractManifest(ctx context.Context, hash, format string) (*archive.Manifest, error) {
	rc, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	if info != nil && info.Size > h.MaxSourceBytes {
		return nil, fmt.Errorf("source %d bytes > cap %d", info.Size, h.MaxSourceBytes)
	}
	// ZIP + 7z both need ReaderAt + size — slurp into memory (capped).
	// For huge archives we could DownloadRange the header tail only;
	// defer until we see ones in the wild that overflow the cap.
	if format == "zip" || format == "7z" {
		buf, err := io.ReadAll(io.LimitReader(rc, h.MaxSourceBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		if int64(len(buf)) > h.MaxSourceBytes {
			return nil, fmt.Errorf("source exceeded cap during read")
		}
		if format == "zip" {
			return archive.ManifestZIP(bytes.NewReader(buf), int64(len(buf)))
		}
		return archive.ManifestSevenZip(bytes.NewReader(buf), int64(len(buf)))
	}
	if format == "rar" {
		return archive.ManifestRAR(rc)
	}
	// TAR / TAR.GZ / TAR.BZ2 / TAR.XZ — stream-only.
	compressed := ""
	if strings.HasPrefix(format, "tar.") {
		compressed = strings.TrimPrefix(format, "tar.")
	}
	return archive.ManifestTAR(rc, compressed)
}

func (h *ArchiveHandler) persistArchiveMetadata(ctx context.Context, id uuid.UUID, m *archive.Manifest) error {
	payload, err := json.Marshal(map[string]any{"archive": m})
	if err != nil {
		return err
	}
	return assets.New(h.Pool).MergeAssetMetadata(ctx, assets.MergeAssetMetadataParams{
		ID:       pgtype.UUID{Bytes: id, Valid: true},
		Metadata: payload,
	})
}

func (h *ArchiveHandler) markProcessingArchive(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetProcessing(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.archive.mark_processing_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *ArchiveHandler) markReadyArchive(ctx context.Context, id uuid.UUID) {
	if err := assets.New(h.Pool).MarkAssetReady(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.archive.mark_ready_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}
func (h *ArchiveHandler) markFailedArchive(ctx context.Context, id uuid.UUID, msg string) {
	if err := assets.New(h.Pool).MarkAssetFailed(ctx, assets.MarkAssetFailedParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ProcessingError: msg,
	}); err != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "preview.archive.mark_failed_failed",
			slog.String("asset_id", id.String()),
			slog.String("err", err.Error()))
	}
}

// archiveExtsHandler — mirrored on the assets dispatcher side so
// uploads route to the archive preview type. Stays in lock-step
// with archive.SupportedExtensions().
var archiveExtsHandler = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, e := range archive.SupportedExtensions() {
		m[e] = struct{}{}
	}
	return m
}()

func IsArchiveExt(ext string) bool {
	_, ok := archiveExtsHandler[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return ok
}
