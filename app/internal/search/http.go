package search

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/auth"
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
	if q == "" {
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
