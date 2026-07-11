// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package redirect handles IIIF 2.0 → 3.0 URL rewrites so viewers
// still pointing at legacy 2.0 grammar (Mirador 2.x + old
// UniversalViewer builds) get a 301 to the 3.0 canonical URL rather
// than a 404. Phase 1.54.B.
//
// The mounted prefix is /iiif/2/... — completely disjoint from
// /iiif/3/... so route dispatch is unambiguous. Every 2.0 URL
// rewrites to its 3.0 equivalent:
//
//	/iiif/2/{id}/manifest        → /iiif/3/asset/{id}/manifest.json
//	/iiif/2/{id}/info.json       → /iiif/3/{id}/info.json
//	/iiif/2/{id}/full/full/0/    → /iiif/3/{id}/full/max/0/  (2.0 "full" size → 3.0 "max")
//	/iiif/2/{id}/full/pct:...    (unchanged shape, delegated to 3.0)
//
// The 2.0 grammar has one incompatible difference from 3.0 worth
// rewriting: the "full" size keyword became "max" between versions.
// Everything else in the URL grammar is a spec-compatible superset,
// so the redirect is safe.
//
// Not implemented (out of scope for MVP):
//   - Semantic response-body translation (2.0 manifests have
//     different JSON shape). We ONLY translate the URL grammar;
//     the response body is always 3.0 JSON. This means legacy
//     Mirador 2.x will hit the 3.0 manifest and either upgrade its
//     parser OR fail — the operator's call. Content Search 1.0 →
//     2.0 has the same story.
package redirect

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Handler is the 2.0 → 3.0 rewrite handler.
type Handler struct {
	// Counter records rewrite hits per rewrite type for the
	// admin/iiif/health dashboard. Nil-safe.
	Counter Counter
}

// Counter tracks legacy-URL rewrites.
type Counter interface {
	RecordLegacyRewrite(kind string)
}

// Mount attaches the routes to r under /iiif/2/. Chi routes stay
// raw per B-1..B-5 pattern.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/iiif/2/{id}/manifest", h.rewriteManifest)
	r.Get("/iiif/2/{id}/info.json", h.rewriteInfoJSON)
	// Image API tile requests: /iiif/2/{id}/{region}/{size}/{rotation}/{quality}.{format}
	// Chi doesn't do path-variable-count routing so we use a
	// wildcard tail + parse the rest ourselves.
	r.Get("/iiif/2/{id}/*", h.rewriteImage)
}

func (h *Handler) rewriteManifest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	target := "/iiif/3/asset/" + id + "/manifest.json"
	h.record("manifest")
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func (h *Handler) rewriteInfoJSON(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	target := "/iiif/3/" + id + "/info.json"
	h.record("info")
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// rewriteImage translates the tile URL grammar. The only rewrite
// needed at the URL layer is `full` → `max` in the size segment;
// all other segments (region, rotation, quality.format) are
// unchanged between 2.0 and 3.0.
//
// The wildcard tail is expected to be exactly:
//
//	{region}/{size}/{rotation}/{quality}.{format}
//
// Any tail that doesn't match this shape is rewritten as-is and
// the 3.0 handler decides how to respond (typically 404).
func (h *Handler) rewriteImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tail := chi.URLParam(r, "*")
	parts := strings.Split(tail, "/")
	// Rewrite `full` in the size slot (parts[1]) to `max` per
	// Image API 3.0 grammar. Other slots pass through.
	if len(parts) >= 2 && parts[1] == "full" {
		parts[1] = "max"
	}
	target := "/iiif/3/" + id + "/" + strings.Join(parts, "/")
	h.record("image")
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func (h *Handler) record(kind string) {
	if h.Counter != nil {
		h.Counter.RecordLegacyRewrite(kind)
	}
}
