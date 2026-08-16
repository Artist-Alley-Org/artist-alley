// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// SuggestHandler is the http.Handler for GET /search/suggest.
type SuggestHandler struct {
	Service *suggest.Service
	Logger  *slog.Logger
}

// ServeHTTP implements http.Handler.
//
// Wire shape:
//
//	GET /search/suggest?prefix=sub&limit=10&scope=browse
//
// Response:
//
//	{"suggestions":[{"value":"subtitle","kind":"tag","similarity":0.75}]}
func (h *SuggestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prefix_required"})
		return
	}
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}

	var callerRef *int64
	var caps visibility.ContentCaps
	var postCaps visibility.PostCaps
	var mutCaps visibility.AssetMutationCaps
	var collCaps visibility.CapabilityChecker
	if id := auth.IdentityFromContext(r.Context()); id != nil {
		ref := id.UserRef
		callerRef = &ref
		// #899 — a completion is an asset TITLE, so this surface needs
		// the content plane and therefore the capabilities that
		// short-circuit it. Without them a demo-viewer holding
		// content.read.all would lose completions for a catalogue it
		// can fully read.
		caps = visibility.ResolveContentCaps(func(code string) bool { return id.Can(code) })
		// #873 — and the post plane, which decides which post TITLES
		// exist to complete at all. #1075 — and which TAGS, too: the tag
		// source ran with no caller at all until then.
		postCaps = visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) })
		// #1064 — the asset-mutation scope, resolved at the SAME edge as
		// the other two, exactly as the Engine's handler does. A title is
		// a FIELD, so the completion answers on the field plane and a
		// team-scoped assets.admin holder completes what /search already
		// matches for them.
		mutCaps = visibility.ResolveAssetMutationCaps(
			func(code string) bool { return id.Can(code) },
			id.ScopedTeams(visibility.AssetsAdmin),
		)
		// #1078 — the collection read rule's admin arm. Passed as a
		// raw checker because visibility.CanReadCollection takes one,
		// and this surface has to give a system.admin the same answer
		// the collection page does.
		collCaps = func(code string) bool { return id.Can(code) }
	}

	req := suggest.Request{
		Prefix:         prefix,
		Caller:         visibility.NewCaller(callerRef),
		Caps:           caps,
		PostCaps:       postCaps,
		MutationCaps:   mutCaps,
		CollectionCaps: collCaps,
		// #1117 — the mature axis, off the request context, outside the
		// identity branch for the reason its siblings on /search and
		// /search/facets are.
		Mature: visibility.MatureFromContext(r.Context()),
		// #1155 — which corpus the caller's commit will be executed
		// against. The nav box derives it from the same predicate that
		// decides where a commit navigates; an absent or unknown value
		// means the wider /search corpus, which is the pre-#1155
		// behaviour and never withholds a completion silently.
		Scope: suggest.ParseScope(r.URL.Query().Get("scope")),
		Limit: limit,
	}
	resp, err := h.Service.Suggest(r.Context(), req)
	if err != nil {
		if errors.Is(err, suggest.ErrEmptyPrefix) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prefix_required"})
			return
		}
		if h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"search.suggest.error",
				slog.String("err", err.Error()),
			)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
