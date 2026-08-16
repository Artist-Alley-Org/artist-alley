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

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// AssetLookup resolves an asset id to the bits the IIIF surface
// needs (file_hash + stored variant keys + EXIF pixel dimensions). Tests
// inject in-memory stubs; boot wires against the live DB pool
// via NewLookupFromPool.
type AssetLookup interface {
	GetIIIFAsset(ctx context.Context, id uuid.UUID, caller visibility.Caller) (IIIFAsset, error)
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
//
// HasImage IS DELIBERATELY ABSENT (#614). IIIF used to gate both
// endpoints on assets.has_image, a column that is DEFAULT false NOT NULL
// with no writer anywhere in the tree — so the gate was true for every
// asset ever created and the whole Image API returned 404 for a full
// release without one report, because nothing exercised it. Removing the
// field from this projection is the fix AND the guard: a future change
// cannot re-consult the column here without first re-adding it, which is
// a visible edit rather than a silent condition.
//
// What replaces it is VariantKeys — what is actually on disk — because
// "can IIIF serve this?" is a question about stored rasters, and that is
// already computed correctly elsewhere (featured/rail.go, #610's
// ladder_available). A denormalised boolean was the wrong shape for it
// even when it had a writer.
type IIIFAsset struct {
	FileHash string
	// VariantKeys are the storage variant keys present for FileHash.
	// The handler intersects these with the operator's CONFIGURED
	// variants to decide servability — see servableVariant.
	VariantKeys []string
	PixelWidth  int
	PixelHeight int
}

// servableVariant reports whether ANY configured IIIF variant is
// actually stored for this asset.
//
// ANY, not ALL, and the difference matters. Resolve serves `region=full`
// from the contain rungs and `region=square` from a cover rung, picking
// per request — so an asset missing one rung is still servable at the
// others, and requiring the complete ladder (#610's LadderSatisfiedSQL)
// would 404 the whole asset over a single missing size. The per-request
// misses are already handled downstream and more precisely: Resolve
// returns 501 for a size this install cannot produce, and OpenVariant
// 404s a variant that is configured but not yet written.
//
// Intersected with the CONFIGURED list rather than "any row exists"
// because Resolve only ever picks from the configured variants — a
// stale variant left behind by a config change is bytes on disk that no
// request can reach, and must not make the asset look servable.
func servableVariant(stored []string, configured []VariantSize) bool {
	if len(stored) == 0 || len(configured) == 0 {
		return false
	}
	have := make(map[string]struct{}, len(stored))
	for _, k := range stored {
		have[k] = struct{}{}
	}
	for _, v := range configured {
		if v.MaxDim <= 0 {
			continue
		}
		if _, ok := have[v.Key]; ok {
			return true
		}
	}
	return false
}

// ErrAssetNotFound is the typed not-found from AssetLookup. The
// handler maps to 404.
var ErrAssetNotFound = errors.New("iiif: asset not found")

// Handler is the IIIF Image API 3.0 Level 0 HTTP surface.
type Handler struct {
	Lookup   AssetLookup
	Variants VariantLister
	Streamer VariantStreamer
	Logger   *slog.Logger
	// Content gates the tile BYTES on visibility.CanReadContent (ADR
	// 0064, #476). The Lookup predicate gates row EXISTENCE; this gates
	// the bytes, which is a separate grant — a restricted/team/embargo
	// asset a caller may list must still 404 its tiles. A *pgxpool.Pool
	// satisfies this in production; DB-backed tests pass the real pool.
	Content visibility.ContentPool
}

// NewHandler wires the handler with sensible defaults. Anonymous
// admission is the mounting middleware's job (the public-mode gate,
// #445) — the handler no longer runs its own identity check; the
// visibility predicate inside GetIIIFAsset decides which rows a caller,
// anonymous or not, may resolve (#460). The content pool gates the tile
// bytes (#476).
func NewHandler(lookup AssetLookup, variants VariantLister, streamer VariantStreamer, content visibility.ContentPool, logger *slog.Logger) *Handler {
	return &Handler{
		Lookup:   lookup,
		Variants: variants,
		Streamer: streamer,
		Content:  content,
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
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed asset id")
		return
	}
	ctx := r.Context()
	asset, err := h.Lookup.GetIIIFAsset(ctx, id, callerFrom(r))
	if errors.Is(err, ErrAssetNotFound) {
		writeJSONError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		h.warn(ctx, "iiif.info.lookup_error", "id", idStr, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	// Variants are loaded BEFORE the servability gate, because the gate
	// is now a question about them (#614): "is any configured variant
	// stored for this asset?" rather than "is a column true?".
	variants, err := h.Variants.ListIIIFVariants(ctx)
	if err != nil {
		h.warn(ctx, "iiif.info.variants_error", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "variants unavailable")
		return
	}
	if asset.FileHash == "" || !servableVariant(asset.VariantKeys, variants) {
		writeJSONError(w, http.StatusNotFound, "asset has no image to serve")
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
	// Row plane: GetIIIFAsset gates EXISTENCE via the visibility
	// predicate (an unreadable row 404s here).
	asset, err := h.Lookup.GetIIIFAsset(ctx, id, callerFrom(r))
	if errors.Is(err, ErrAssetNotFound) {
		writeJSONError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		h.warn(ctx, "iiif.image.lookup_error", "id", idStr, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	// Content plane (ADR 0064, #476): resolving the row only proves the
	// caller may SEE the asset. Streaming its tiles is a separate grant —
	// a restricted/team/embargo asset the caller can list must still 404
	// its bytes. 404 not 403, same shape as the row-plane miss, so this
	// plane never confirms a restricted asset exists. Mirrors
	// handlers.requireContentAccess / assets.contentCaller.
	caller, caps := contentCaller(r)
	if allowed, cerr := visibility.CanReadContent(ctx, h.Content, caller, caps, id,
		visibility.MatureFromContext(ctx)); cerr != nil || !allowed {
		if cerr != nil {
			h.warn(ctx, "iiif.image.content_gate_error", "id", idStr, "err", cerr.Error())
		}
		writeJSONError(w, http.StatusNotFound, "asset not found")
		return
	}
	variants, err := h.Variants.ListIIIFVariants(ctx)
	if err != nil {
		h.warn(ctx, "iiif.image.variants_error", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "variants unavailable")
		return
	}
	// Servability gate (#614) — see servableVariant. Kept BELOW the
	// content gate above so the ordering of the two planes is unchanged:
	// a caller who may not read the content still gets the same 404 it
	// always did, and never learns whether rasters exist.
	if asset.FileHash == "" || !servableVariant(asset.VariantKeys, variants) {
		writeJSONError(w, http.StatusNotFound, "asset has no image to serve")
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

// contentCaller builds the (caller, caps) pair CanReadContent needs from
// the request identity — the same derivation handlers.requireContentAccess
// and assets.contentCaller use. Anonymous carries a nil capability
// checker (it holds no capabilities and is never admin), which
// CanReadContent handles.
func contentCaller(r *http.Request) (visibility.Caller, visibility.CapabilityChecker) {
	if id := auth.IdentityFromContext(r.Context()); id != nil {
		return visibility.NewCaller(&id.UserRef), func(code string) bool { return id.Can(code) }
	}
	return visibility.NewCaller(nil), nil
}

// callerFrom builds the visibility caller for the request (row plane).
// Anonymous (nil identity) resolves against the anonymous predicate; an
// authenticated request carries its user ref. The predicate, not this
// helper, decides row visibility.
func callerFrom(r *http.Request) visibility.Caller {
	caller, _ := contentCaller(r)
	return caller
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
//
// The row is resolved through the SINGLE visibility predicate (ADR
// 0063), exactly as the browse path does (#460). Before this, IIIF was
// a SECOND row-existence path: it resolved any non-deleted asset by id
// with no visibility check, so a request to info.json could confirm a
// non-public asset existed and read its pixel dimensions. Now the
// predicate is spliced in — an anonymous caller resolves only
// public+active+ready assets, an authenticated caller resolves any
// non-deleted asset (the deferred authenticated-sensitivity rule, same
// as browse) — and a row the caller may not see comes back as
// ErrAssetNotFound, which the handler maps to 404 (never 403: the row
// plane must not confirm a hidden asset exists).
func (l PoolLookup) GetIIIFAsset(ctx context.Context, id uuid.UUID, caller visibility.Caller) (IIIFAsset, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return IIIFAsset{}, err
	}
	// $1 = id; the predicate's args start at $2.
	visFrag, visArgs := pred.ToSQL("a", 1)
	q := `
		SELECT a.file_hash,
		       -- What is actually stored, not a column claiming what
		       -- ought to be (#614). One subquery in the same round
		       -- trip; the handler intersects it with the configured
		       -- variant list.
		       COALESCE(ARRAY(SELECT sv.variant_key
		                        FROM storage_variants sv
		                       WHERE sv.object_hash = a.file_hash), '{}')::text[] AS variant_keys,
		       COALESCE(w.value_num, 0)::INT AS pixel_width,
		       COALESCE(h.value_num, 0)::INT AS pixel_height
		  FROM assets a
		  LEFT JOIN asset_field_value w
		         ON w.asset_id = a.id
		        AND w.field_id = (SELECT id FROM field_definition WHERE code = 'pixel_width' LIMIT 1)
		  LEFT JOIN asset_field_value h
		         ON h.asset_id = a.id
		        AND h.field_id = (SELECT id FROM field_definition WHERE code = 'pixel_height' LIMIT 1)
		 WHERE a.id = $1` + visFrag
	var out IIIFAsset
	var fileHash *string
	args := append([]any{id}, visArgs...)
	err = l.Pool.QueryRow(ctx, q, args...).Scan(&fileHash, &out.VariantKeys, &out.PixelWidth, &out.PixelHeight)
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
