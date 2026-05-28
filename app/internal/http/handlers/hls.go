package handlers

import (
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

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// HLSHandler serves multi-segment HLS variants
// (`hls/master.m3u8`, `hls/720p/playlist.m3u8`, `hls/480p/seg00000.ts`,
// …). The OpenAPI-derived variant route only matches single-segment
// variant keys because chi's default param is non-greedy; this handler
// is registered ahead of the strict handler to catch the rest.
type HLSHandler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Logger  *slog.Logger
}

func NewHLSHandler(pool *pgxpool.Pool, st *storage.Service, logger *slog.Logger) *HLSHandler {
	return &HLSHandler{Pool: pool, Storage: st, Logger: logger}
}

func (h *HLSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if auth.IdentityFromContext(r.Context()) == nil {
		http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		return
	}

	assetID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, `{"error":"invalid asset id"}`, http.StatusBadRequest)
		return
	}
	rest := chi.URLParam(r, "*")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	variant := "hls/" + rest
	if err := storage.ValidateVariantKey(variant); err != nil {
		http.Error(w, `{"error":"invalid variant key"}`, http.StatusBadRequest)
		return
	}

	hash, ok, err := h.resolveHash(r.Context(), assetID)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, info, err := h.Storage.Download(r.Context(), hash, variant)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, "hls.download.error",
			slog.String("asset_id", assetID.String()),
			slog.String("variant", variant),
			slog.String("err", err.Error()),
		)
		http.Error(w, `{"error":"download failed"}`, http.StatusInternalServerError)
		return
	}
	defer body.Close()

	ct := contentTypeForVariant(variant)
	if info != nil && info.ContentType != "" && info.ContentType != "application/octet-stream" {
		ct = info.ContentType
	}
	w.Header().Set("Content-Type", ct)
	if info != nil && info.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (h *HLSHandler) resolveHash(ctx context.Context, id uuid.UUID) (string, bool, error) {
	row, err := assets.New(h.Pool).GetAsset(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	if row.FileHash == nil || *row.FileHash == "" {
		return "", false, nil
	}
	return *row.FileHash, true, nil
}

func contentTypeForVariant(v string) string {
	switch {
	case strings.HasSuffix(v, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.HasSuffix(v, ".ts"):
		return "video/mp2t"
	case strings.HasSuffix(v, ".m4s"):
		return "video/iso.segment"
	case strings.HasSuffix(v, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(v, ".vtt"):
		return "text/vtt"
	default:
		return "application/octet-stream"
	}
}
