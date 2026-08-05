// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/vector"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Handler is the raw http.Handler for the unified GET /search
// endpoint. Mounted inside /api/v1 so the auth-resolver middleware
// has already populated auth.Identity on the request context.
type Handler struct {
	Service *Service
	Logger  *slog.Logger
}

// ServeHTTP implements http.Handler.
//
// Wire shape:
//
//	GET /search?q=cat&types=asset,collection,post&limit=25&cursor=eyJ...
//
// Response:
//
//	{
//	  "hits": [ { "type", "id", "title", "summary", "score", ... } ],
//	  "next_cursor": "eyJ...",
//	  "total_count": 42,
//	  "total_count_capped": false,
//	  "types_matched": ["asset","collection","post"]
//	}
//
// Errors are JSON: { "error": "<code>" } with HTTP 400 / 401 / 500.
// Anonymous callers are allowed (per plan decision 11); visibility
// gating reduces the anonymous view to public entities only.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	q := r.URL.Query().Get("q")
	dslInput := r.URL.Query().Get("dsl")
	if q == "" && dslInput == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query_required"})
		return
	}
	types, err := ParseTypes(r.URL.Query().Get("types"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_types"})
		return
	}
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	cursor, err := DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_cursor"})
		return
	}

	query := Query{
		Text:   q,
		Types:  types,
		Limit:  limit,
		Cursor: cursor,
	}
	if id := auth.IdentityFromContext(r.Context()); id != nil {
		ref := id.UserRef
		query.CallerUserRef = &ref
		// #899 — the capabilities used to stop here. Only the ref was
		// copied across, so the engine had no way to tell a
		// content.read.all holder from a stranger and the sensitivity
		// rule could not be expressed at all (search/doc.go called it
		// "deliberately deferred").
		query.Caps = visibility.ResolveContentCaps(func(code string) bool { return id.Can(code) })
		// #873 — the post plane needs its own answer: posts.admin opens
		// the `private` tier in the post read rule, which search now
		// composes in full instead of a narrower copy of it.
		query.PostCaps = visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) })
	}

	// Phase 1.16.B-3 — if the caller supplied a `dsl=` param
	// (advanced-mode query), parse + compile + resolve any
	// similar_to:<uuid> to an embedding hint. The Engine's hybrid
	// path picks it up via Query.SimilarityHint.
	if dslInput != "" {
		if err := h.applyDSL(r, &query, dslInput); err != nil {
			if de, ok := err.(dsl.DSLError); ok {
				status := http.StatusBadRequest
				payload := map[string]any{"error": "dsl_error", "kind": int(de.Kind), "message": de.Message}
				if len(de.ValidFields) > 0 {
					payload["valid_fields"] = de.ValidFields
				}
				writeJSON(w, status, payload)
				return
			}
			if errors.Is(err, vector.ErrNotEmbedded) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "similar_to_asset_not_embedded"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dsl_parse_error"})
			return
		}
	}

	res, err := h.Service.Execute(r.Context(), query)
	if err != nil {
		if errors.Is(err, ErrEmptyQuery) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query_required"})
			return
		}
		if h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelWarn, "search.error",
				slog.String("err", err.Error()),
				slog.String("q", q),
			)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	// Render hits with the compact JSON projection.
	hits := make([]json.RawMessage, 0, len(res.Hits))
	for _, hit := range res.Hits {
		hits = append(hits, MarshalHitJSON(hit))
	}
	types_matched := make([]string, 0, len(res.TypesMatched))
	for _, t := range res.TypesMatched {
		types_matched = append(types_matched, string(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hits":               hits,
		"next_cursor":        EncodeCursor(res.NextCursor),
		"total_count":        res.TotalCount,
		"total_count_capped": res.TotalCountCapped,
		"types_matched":      types_matched,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// applyDSL parses + compiles the caller's DSL string and mutates
// `query` with the extracted vector hint (via the search
// Service's embedding fetcher). Returns dsl.DSLError for parse
// failures, vector.ErrNotEmbedded when similar_to references an
// asset without a stored embedding.
func (h *Handler) applyDSL(r *http.Request, query *Query, input string) error {
	parsed, err := dsl.Parse(input)
	if err != nil {
		return err
	}
	compiled, err := dsl.Compile(parsed)
	if err != nil {
		return err
	}
	// Fold DSL free-text back into query.Text if the caller only
	// supplied `dsl=` — the Engine's BM25 path still consumes Text.
	// (Advanced DSL like tag:foo returns an empty tsQuery + Filters;
	// we don't have a Filters plumbing at Engine layer today, so
	// the compiled TSQuery is currently informational — feature
	// flag for a later revision.)
	if query.Text == "" && compiled.TSQuery != "" {
		// A synthetic reconstruction of the free-text intent so
		// the Engine's plainto_tsquery path still works. For
		// pure similar_to (empty TSQuery), keep Text empty and
		// let the hybrid path drive.
		query.Text = input
	}
	if compiled.SimilarToAssetID == "" {
		return nil
	}
	// Resolve the anchor asset's embedding.
	assetID, perr := uuid.Parse(compiled.SimilarToAssetID)
	if perr != nil {
		return dsl.DSLError{Kind: dsl.SyntaxError, Message: "similar_to: value must be a UUID"}
	}
	if h.Service == nil || h.Service.Vector() == nil {
		return errors.New("search: vector fetcher not wired")
	}
	// Visibility gate: the target asset itself must be visible
	// to the caller. Otherwise a restricted asset's ID would leak
	// its neighbourhood to callers who can't see the source.
	pred, err := visibility.Filter(r.Context(), visibility.EntityAsset, visibility.NewCaller(query.CallerUserRef))
	if err != nil {
		return err
	}
	frag, args := pred.ToSQL("", 1)
	var visible bool
	if err := h.Service.Pool().QueryRow(r.Context(), `
		SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1`+frag+`)
	`, append([]any{assetID}, args...)...).Scan(&visible); err != nil {
		return err
	}
	if !visible {
		return vector.ErrNotEmbedded
	}
	anchor, verr := h.Service.Vector().FetchAssetEmbedding(r.Context(), assetID)
	if verr != nil {
		return verr
	}
	query.SimilarityHint = anchor.Raw
	query.SimilarityHintProvider = anchor.Provider
	query.SimilarityHintModel = anchor.Model
	query.SimilarityHintModality = anchor.Modality
	query.SimilarityHintID = "asset:" + compiled.SimilarToAssetID
	if query.HybridWeight <= 0 {
		query.HybridWeight = compiled.HybridWeightSuggestion
	}
	return nil
}
