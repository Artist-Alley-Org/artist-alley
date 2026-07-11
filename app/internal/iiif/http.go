// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package iiif

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AssetLookup resolves an asset id to the bits the IIIF surface
// needs (file_hash + has_image + EXIF pixel dimensions). Tests
// inject in-memory stubs; boot wires against the live DB pool
// via NewLookupFromPool.
type AssetLookup interface {
	GetIIIFAsset(ctx context.Context, id uuid.UUID) (IIIFAsset, error)
}

// VariantLister returns the install's configured pre-baked
// preview ladder. Backed by sysconfig.Store in production;
// stubbed in tests.
type VariantLister interface {
	ListIIIFVariants(ctx context.Context) ([]VariantSize, error)
}

// VariantStreamer streams the bytes of one variant + its
// content type. Backed by storage.Service in production.
type VariantStreamer interface {
	OpenVariant(ctx context.Context, hash, variantKey string) (io.ReadCloser, int64, string, error)
}

// IIIFAsset is the small projection of an asset row the IIIF
// handler reads.
type IIIFAsset struct {
	FileHash    string
	HasImage    bool
	PixelWidth  int
	PixelHeight int
}

// ErrAssetNotFound is the typed not-found from AssetLookup. The
// handler maps to 404.
var ErrAssetNotFound = errors.New("iiif: asset not found")

// Handler is the IIIF Image API 3.0 Level 0 HTTP surface.
type Handler struct {
	Lookup    AssetLookup
	Variants  VariantLister
	Streamer  VariantStreamer
	RequireID func(*http.Request) bool // returns true when an Identity is on the context
	Logger    *slog.Logger
}

// NewHandler wires the handler with sensible defaults. RequireID
// is the auth gate — when nil, requests pass through (useful for
// tests). Production mounts the handler under a middleware that
// runs the resolver; RequireID then checks for an Identity in
// context.
func NewHandler(lookup AssetLookup, variants VariantLister, streamer VariantStreamer, logger *slog.Logger) *Handler {
	return &Handler{
		Lookup:   lookup,
		Variants: variants,
		Streamer: streamer,
		Logger:   logger,
	}
}

// Mount registers /iiif/3/{id}/info.json + the image-request
// URL under the given chi router. Production calls this from
// server.go inside the /api/v1 group so the auth resolver
// middleware runs ahead.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/iiif/3/{id}/info.json", h.serveInfo)
	// Five segments: region / size / rotation / qualityDotFormat.
	r.Get("/iiif/3/{id}/{region}/{size}/{rotation}/{qualityDotFormat}", h.serveImage)
}

// serveInfo emits the Image Information Document.
func (h *Handler) serveInfo(w http.ResponseWriter, r *http.Request) {
	if !h.authed(r) {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed asset id")
		return
	}
	ctx := r.Context()
	asset, err := h.Lookup.GetIIIFAsset(ctx, id)
	if errors.Is(err, ErrAssetNotFound) {
		writeJSONError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		h.warn(ctx, "iiif.info.lookup_error", "id", idStr, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !asset.HasImage || asset.FileHash == "" {
		writeJSONError(w, http.StatusNotFound, "asset has no image to serve")
		return
	}
	variants, err := h.Variants.ListIIIFVariants(ctx)
	if err != nil {
		h.warn(ctx, "iiif.info.variants_error", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "variants unavailable")
		return
	}
	infoURL := publicBaseURL(r) + "/iiif/3/" + idStr
	info, err := BuildInfo(infoURL, asset.PixelWidth, asset.PixelHeight, variants)
	if err != nil {
		if errors.Is(err, ErrUnsupportedAsset) {
			writeJSONError(w, http.StatusNotFound, "asset has no recorded pixel dimensions; run the EXIF extractor")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// IIIF Image API requires the response Content-Type to be
	// `application/ld+json` per §5.1; some clients accept
	// application/json with a `profile=` parameter as a fallback.
	w.Header().Set("Content-Type", "application/ld+json; profile=\""+Context+"\"")
	// CORS so a IIIF viewer hosted on a different origin can
	// fetch info.json + the image bytes. The auth gate above
	// runs against the cookie so this is safe.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(info)
}

// serveImage decodes the image URL + streams the resolved
// variant bytes through. Caller-Content-Type is whatever the
// storage layer recorded (typically image/webp).
func (h *Handler) serveImage(w http.ResponseWriter, r *http.Request) {
	if !h.authed(r) {
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed asset id")
		return
	}
	req, err := ParseImageRequest(
		chi.URLParam(r, "region"),
		chi.URLParam(r, "size"),
		chi.URLParam(r, "rotation"),
		chi.URLParam(r, "qualityDotFormat"),
	)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	asset, err := h.Lookup.GetIIIFAsset(ctx, id)
	if errors.Is(err, ErrAssetNotFound) {
		writeJSONError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		h.warn(ctx, "iiif.image.lookup_error", "id", idStr, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !asset.HasImage || asset.FileHash == "" {
		writeJSONError(w, http.StatusNotFound, "asset has no image to serve")
		return
	}
	variants, err := h.Variants.ListIIIFVariants(ctx)
	if err != nil {
		h.warn(ctx, "iiif.image.variants_error", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "variants unavailable")
		return
	}
	match, err := Resolve(req, asset.PixelWidth, asset.PixelHeight, variants)
	switch {
	case errors.Is(err, ErrSizeNotAvailable):
		// IIIF §4.5: 501 Not Implemented for well-formed but
		// unsupported requests in a lower compliance level.
		writeJSONError(w, http.StatusNotImplemented, err.Error())
		return
	case errors.Is(err, ErrBadRequest):
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	body, size, contentType, err := h.Streamer.OpenVariant(ctx, asset.FileHash, match.Variant.Key)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "variant not stored; run the preview pipeline")
		return
	}
	defer body.Close()

	if contentType == "" {
		contentType = "image/webp"
	}
	w.Header().Set("Content-Type", contentType)
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	}
	// CORS — same rationale as info.json.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// authed reports whether the caller passes the auth gate.
// Defaults to "allow" when RequireID is unwired (tests that
// just want to exercise the URL grammar).
func (h *Handler) authed(r *http.Request) bool {
	if h.RequireID == nil {
		return true
	}
	return h.RequireID(r)
}

func (h *Handler) warn(ctx context.Context, msg string, args ...any) {
	if h.Logger == nil {
		return
	}
	h.Logger.WarnContext(ctx, msg, args...)
}

// publicBaseURL reconstructs the externally-visible base URL the
// request came in on (scheme + host). Honours X-Forwarded-{Proto,
// Host} so reverse-proxied deployments get the right base. Caller
// composes the asset path on top.
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = strings.SplitN(v, ",", 2)[0]
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = strings.SplitN(v, ",", 2)[0]
	}
	return scheme + "://" + host
}

// writeJSONError emits a small {error: "..."} body so curl /
// fetch consumers get a consistent shape across success +
// failure paths. Tests assert against the same shape.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------
// Production wiring helpers
// ---------------------------------------------------------------------------

// PoolLookup is the live-DB implementation of AssetLookup.
// One query pulls the asset row + the two pixel-dimension field
// values (joined from asset_field_value on the canonical field
// codes "pixel_width" / "pixel_height"). Returns ErrAssetNotFound
// when the asset row doesn't exist.
type PoolLookup struct {
	Pool *pgxpool.Pool
}

// GetIIIFAsset implements AssetLookup.
func (l PoolLookup) GetIIIFAsset(ctx context.Context, id uuid.UUID) (IIIFAsset, error) {
	const q = `
		SELECT a.file_hash,
		       a.has_image,
		       COALESCE(w.value_num, 0)::INT AS pixel_width,
		       COALESCE(h.value_num, 0)::INT AS pixel_height
		  FROM assets a
		  LEFT JOIN asset_field_value w
		         ON w.asset_id = a.id
		        AND w.field_id = (SELECT id FROM field_definition WHERE code = 'pixel_width' LIMIT 1)
		  LEFT JOIN asset_field_value h
		         ON h.asset_id = a.id
		        AND h.field_id = (SELECT id FROM field_definition WHERE code = 'pixel_height' LIMIT 1)
		 WHERE a.id = $1
		   AND a.deleted_at IS NULL
	`
	var out IIIFAsset
	var fileHash *string
	err := l.Pool.QueryRow(ctx, q, id).Scan(&fileHash, &out.HasImage, &out.PixelWidth, &out.PixelHeight)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IIIFAsset{}, ErrAssetNotFound
		}
		return IIIFAsset{}, err
	}
	if fileHash != nil {
		out.FileHash = *fileHash
	}
	return out, nil
}
