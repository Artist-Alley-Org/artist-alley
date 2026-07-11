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

// PathVariantHandler serves variants whose key contains slashes
// (`hls/master.m3u8`, `turntable/0007.png`, `views/top.png`, …). The
// OpenAPI-derived `{variant}` chi param is non-greedy by default so
// only single-segment keys hit the strict server; we register this
// handler ahead of it under wildcard routes to catch the rest.
//
// Prefix is the path segment that owns the route (e.g. 'hls',
// 'turntable', 'views') — the handler prepends 'Prefix + "/"' to
// the wildcard remainder to form the full variant key.
type PathVariantHandler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Logger  *slog.Logger
	Prefix  string
}

// HLSHandler is the legacy name we still use for the hls wildcard
// route. Identical to PathVariantHandler but Prefix is fixed.
type HLSHandler = PathVariantHandler

func NewHLSHandler(pool *pgxpool.Pool, st *storage.Service, logger *slog.Logger) *PathVariantHandler {
	return NewPathVariantHandler(pool, st, logger, "hls")
}

// NewPathVariantHandler binds a handler to (pool, storage, prefix).
// The same shape powers every nested-variant route — hls, turntable,
// views — so adding a new one is a single r.Get() line at registration.
func NewPathVariantHandler(pool *pgxpool.Pool, st *storage.Service, logger *slog.Logger, prefix string) *PathVariantHandler {
	return &PathVariantHandler{Pool: pool, Storage: st, Logger: logger, Prefix: prefix}
}

func (h *PathVariantHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	prefix := h.Prefix
	if prefix == "" {
		prefix = "hls"
	}
	variant := prefix + "/" + rest
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

func (h *PathVariantHandler) resolveHash(ctx context.Context, id uuid.UUID) (string, bool, error) {
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
