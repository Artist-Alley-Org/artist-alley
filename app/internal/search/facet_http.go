// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// FacetHandler is the http.Handler for GET /search/facets. Uses the
// facet.Dispatcher to run seeded aggregators in parallel.
type FacetHandler struct {
	Dispatcher *facet.Dispatcher
	Logger     *slog.Logger
}

// ServeHTTP implements http.Handler.
//
// Wire shape:
//
//	GET /search/facets?q=cat&facets=asset_type,tag,owner
//
// Response:
//
//	{
//	  "facets": {
//	    "asset_type": {"type":"asset_type","buckets":[{"value":"1","label":"image","count":42}]},
//	    "tag":        {"type":"tag","buckets":[{"value":"pet","count":18}]}
//	  }
//	}
//
// Anonymous callers get facet counts across the public subset only
// (via the shared visibility.Filter helper).
func (h *FacetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	q := r.URL.Query().Get("q")

	// facets= is optional; empty = all seeded facets.
	var types []facet.FacetType
	if raw := r.URL.Query().Get("facets"); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			t = strings.TrimSpace(strings.ToLower(t))
			if t == "" {
				continue
			}
			ft, ok := facet.ParseFacetType(t)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_facet_type"})
				return
			}
			types = append(types, ft)
		}
	}

	var callerRef *int64
	var caps visibility.ContentCaps
	if id := auth.IdentityFromContext(r.Context()); id != nil {
		ref := id.UserRef
		callerRef = &ref
		caps = visibility.ResolveContentCaps(func(code string) bool { return id.Can(code) })
	}

	req := facet.Request{
		QueryText: q,
		Facets:    types,
		Caller:    visibility.NewCaller(callerRef),
		Caps:      caps,
	}
	resp := h.Dispatcher.Run(r.Context(), req)
	writeJSON(w, http.StatusOK, resp)
}
