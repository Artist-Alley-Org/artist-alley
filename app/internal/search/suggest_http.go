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
//	GET /search/suggest?prefix=sub&limit=10
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
	if id := auth.IdentityFromContext(r.Context()); id != nil {
		ref := id.UserRef
		callerRef = &ref
		// #899 — a completion is an asset TITLE, so this surface needs
		// the content plane and therefore the capabilities that
		// short-circuit it. Without them a demo-viewer holding
		// content.read.all would lose completions for a catalogue it
		// can fully read.
		caps = visibility.ResolveContentCaps(func(code string) bool { return id.Can(code) })
	}

	req := suggest.Request{
		Prefix: prefix,
		Caller: visibility.NewCaller(callerRef),
		Caps:   caps,
		Limit:  limit,
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
