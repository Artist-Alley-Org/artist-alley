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
//	GET /search/facets?q=cat&facets=asset_type,tag,owner&filter=tag:sketch
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

	// #907 — the ACTIVE selection, in the same wire shape /search takes,
	// so the client sends one list to both endpoints. Counts that ignore
	// the filter are the same defect as facets that cannot filter, one
	// level up: the rail would describe a corpus the grid beside it is
	// no longer showing.
	selection, err := facet.ParseSelection(r.URL.Query()["filter"])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_filter"})
		return
	}

	var callerRef *int64
	var caps visibility.ContentCaps
	var postCaps visibility.PostCaps
	var mutCaps visibility.AssetMutationCaps
	if id := auth.IdentityFromContext(r.Context()); id != nil {
		ref := id.UserRef
		callerRef = &ref
		caps = visibility.ResolveContentCaps(func(code string) bool { return id.Can(code) })
		// #873 — the tag facet counts through posts, so it needs the
		// capability that opens the post read rule's `private` tier.
		postCaps = visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) })
		// #1056 — the asset-mutation scope. This handler resolved the
		// other two and stopped, so the aggregators had no way to express
		// the ADR 0064 field-plane disjunct and were pinned to the
		// content plane; the Engine's filter conjunct was pinned with
		// them to keep the two agreeing. Resolved at the SAME edge as the
		// other two, exactly as the Engine's handler does.
		mutCaps = visibility.ResolveAssetMutationCaps(
			func(code string) bool { return id.Can(code) },
			id.ScopedTeams(visibility.AssetsAdmin),
		)
	}

	req := facet.Request{
		QueryText:    q,
		Facets:       types,
		Selection:    selection,
		Caller:       visibility.NewCaller(callerRef),
		Caps:         caps,
		PostCaps:     postCaps,
		MutationCaps: mutCaps,
		// #1117 — the mature axis, off the request context. Outside the
		// identity branch above for the reason /search reads it outside
		// its own: an anonymous caller has a definite answer here (the
		// disqualified viewer), not an absent one.
		Mature: visibility.MatureFromContext(r.Context()),
	}
	resp := h.Dispatcher.Run(r.Context(), req)
	writeJSON(w, http.StatusOK, resp)
}
