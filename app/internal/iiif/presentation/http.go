// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package presentation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// FederationResolver is the narrow surface the http.Handler
// consumes; production wires in iiif/federation.Resolver. Kept as
// an interface so tests inject a stub without a live DB.
type FederationResolver interface {
	CanvasBaseFor(ctx context.Context, peerID uuid.UUID, assetID uuid.UUID) string
	ImageBaseFor(ctx context.Context, peerID uuid.UUID, assetID uuid.UUID) string
}

// Counter is the health-hook interface. Nil-safe: the handler
// checks for nil before every call. Latency is per-request wall
// time; the health snapshot rolls up p50/p95/p99.
type Counter interface {
	RecordManifestRequest(entityType EntityType, statusCode int, latency time.Duration)
	RecordManifestCacheHit()
	RecordManifestCacheMiss()
	RecordFederatedCanvas()
}

// Handler serves the Presentation API 3.0 endpoints. Mounted
// alongside 1.54.A's Image API routes under the same /iiif/3/
// prefix by the boot wire.
type Handler struct {
	Loader     *Loader
	Builder    *Builder
	Federation FederationResolver
	Cache      *Cache
	Counter    Counter
	Logger     *slog.Logger
}

// Mount attaches the routes to r. Chi routes stay raw (per
// B-1..B-5 pattern; no strict-server shim).
func (h *Handler) Mount(r chi.Router) {
	r.Get("/iiif/3/asset/{id}/manifest.json", h.serveAssetManifest)
	r.Get("/iiif/3/collection/{id}/manifest.json", h.serveCollectionManifest)
}

func (h *Handler) serveAssetManifest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.record(EntityAsset, http.StatusNotFound, time.Since(start))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	isAnon := auth.IdentityFromContext(r.Context()) == nil
	rb := h.Builder.ForRequest(PublicBaseURL(r))
	ref, err := h.loadWithCache(r.Context(), EntityAsset, id, isAnon, func() (any, error) {
		e, err := h.Loader.LoadAsset(r.Context(), id, isAnon)
		if err != nil {
			return nil, err
		}
		e = h.applyFederation(r.Context(), e)
		return rb.BuildAssetManifest(e, isAnon)
	})
	if err != nil {
		h.mapError(w, err, EntityAsset, start)
		return
	}
	h.record(EntityAsset, http.StatusOK, time.Since(start))
	writeManifest(w, ref)
}

func (h *Handler) serveCollectionManifest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.record(EntityCollection, http.StatusNotFound, time.Since(start))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	isAnon := auth.IdentityFromContext(r.Context()) == nil
	rb := h.Builder.ForRequest(PublicBaseURL(r))
	ref, err := h.loadWithCache(r.Context(), EntityCollection, id, isAnon, func() (any, error) {
		c, err := h.Loader.LoadCollection(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if isAnon && (c.Sensitivity == SensitivityRestricted || c.Sensitivity == SensitivityTeam) {
			return nil, ErrRestricted
		}
		members, err := h.Loader.LoadCollectionMembers(r.Context(), id, isAnon, 200)
		if err != nil {
			return nil, err
		}
		for i := range members {
			members[i] = h.applyFederation(r.Context(), members[i])
		}
		return rb.BuildCollectionManifest(c, members, isAnon)
	})
	if err != nil {
		h.mapError(w, err, EntityCollection, start)
		return
	}
	h.record(EntityCollection, http.StatusOK, time.Since(start))
	writeManifest(w, ref)
}

// loadWithCache serves from the manifest cache when fresh.
func (h *Handler) loadWithCache(ctx context.Context, kind EntityType, id uuid.UUID, isAnon bool, build func() (any, error)) (any, error) {
	if h.Cache == nil {
		return build()
	}
	key := h.Cache.Key(kind, id, isAnon)
	if v, ok := h.Cache.Get(key); ok {
		if h.Counter != nil {
			h.Counter.RecordManifestCacheHit()
		}
		return v, nil
	}
	if h.Counter != nil {
		h.Counter.RecordManifestCacheMiss()
	}
	v, err := build()
	if err != nil {
		return nil, err
	}
	h.Cache.Put(key, v)
	return v, nil
}

// applyFederation resolves federated peer URLs onto the entity's
// RemoteCanvasBase / RemoteImageBase fields. Local assets pass
// through unchanged.
//
// Empty resolver results (peer not found) are silently tolerated —
// the builder falls back to local URLs. This keeps a broken peer
// directory from blocking manifest rendering.
func (h *Handler) applyFederation(ctx context.Context, e EntityRef) EntityRef {
	if e.OriginServerID == nil || h.Federation == nil {
		return e
	}
	if h.Counter != nil {
		h.Counter.RecordFederatedCanvas()
	}
	e.RemoteCanvasBase = h.Federation.CanvasBaseFor(ctx, *e.OriginServerID, e.ID)
	e.RemoteImageBase = h.Federation.ImageBaseFor(ctx, *e.OriginServerID, e.ID)
	return e
}

func (h *Handler) mapError(w http.ResponseWriter, err error, kind EntityType, start time.Time) {
	switch {
	case errors.Is(err, ErrRestricted), errors.Is(err, ErrNotFound):
		h.record(kind, http.StatusNotFound, time.Since(start))
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	default:
		h.record(kind, http.StatusInternalServerError, time.Since(start))
		if h.Logger != nil {
			h.Logger.LogAttrs(context.Background(), slog.LevelWarn,
				"iiif.presentation.error",
				slog.String("err", err.Error()))
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
	}
}

func (h *Handler) record(kind EntityType, status int, latency time.Duration) {
	if h.Counter != nil {
		h.Counter.RecordManifestRequest(kind, status, latency)
	}
}

// writeManifest emits the manifest with the standard IIIF
// content type + CORS-friendly headers Mirador expects. Since
// Presentation manifests are the entry point for embedded viewers
// on third-party sites, permissive CORS is by-design.
func writeManifest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json;profile=\"http://iiif.io/api/presentation/3/context.json\"")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// PublicBaseURL reconstructs the externally-visible base URL the
// request came in on (scheme + host). Mirrors 1.54.A's own
// publicBaseURL at app/internal/iiif/http.go:239 verbatim so both
// APIs derive URLs the same way — a fix / follow-up on one applies
// cleanly to the other. Honours X-Forwarded-{Proto,Host} for
// reverse-proxied deployments.
func PublicBaseURL(r *http.Request) string {
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
