package disk_usage

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// Handler serves GET /admin/search/disk-usage. Admin-cap gated.
type Handler struct {
	Cache  *Cache
	Logger *slog.Logger
}

// ServeHTTP implements http.Handler.
//
// Wire shape:
//
//	GET /admin/search/disk-usage[?refresh=true]
//
// Response:
//
//	{
//	  "tsvector_bytes": {"assets": N, "collections": N, "posts": N},
//	  "embedding_table_bytes": N,
//	  "embedding_index_bytes": N,
//	  "assets_pending_embedding": N,
//	  "asset_embedding_row_count": N,
//	  "saved_search_rows": N,
//	  "saved_search_active": N,
//	  "search_reindex_history_rows": N,
//	  "snapshot_at": "<RFC3339>"
//	}
//
// refresh=true bypasses the 30-second cache. Any admin can call
// force-refresh; there's no per-user rate limit on the endpoint
// itself (though the sub-queries are expensive enough that
// admins don't naturally spam it).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}
	if !id.Can("system.admin") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin_required"})
		return
	}

	var (
		snap Snapshot
		err  error
	)
	if r.URL.Query().Get("refresh") == "true" {
		snap, err = h.Cache.Refresh(r.Context())
	} else {
		snap, err = h.Cache.Get(r.Context())
	}
	if err != nil {
		// Partial snapshots still render; individual query
		// failures are logged inside computeSnapshot. A hard error
		// here means the whole computation aborted, which is rare
		// (usually a Ctx cancel).
		if h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"disk_usage.serve",
				slog.String("err", err.Error()))
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
