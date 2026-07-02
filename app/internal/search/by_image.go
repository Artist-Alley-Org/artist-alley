package search

import (
	"log/slog"
	"net/http"
	"time"
)

// ByImageHandler reserves the POST /search/by-image URL. Boot
// injects the search Counter so the /admin/search/health surface
// counts every reserved-endpoint hit even before the sidecar
// ships — operators see demand signal for prioritising the
// follow-up. Phase
// 1.16.B-3 documented divergence: the underlying "CLIP image
// encoder" isn't wired today — pre-audit Q3 revealed that the
// existing "clip_local" provider actually calls Ollama's
// nomic-embed-text (a TEXT-only model). A genuine reverse-image
// path needs a Python CLIP-visual sidecar (deferred to a
// follow-up phase per the arc plan).
//
// Returning a structured 501 here means:
//
//   - Clients + tests can pin the reservation
//   - Frontend surfaces (/search/advanced reverse-image dropzone)
//     can render a "not yet enabled" state instead of failing on a
//     404 that suggests the endpoint doesn't exist
//   - The URL, response shape, + rate-limit budget are all
//     reserved when the sidecar ships
type ByImageHandler struct {
	Logger  *slog.Logger
	Counter *Counter
}

// ServeHTTP always returns 501 with a structured error body
// pointing operators at the follow-up.
func (h *ByImageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if h.Counter != nil {
		h.Counter.Record(ResultByImageNotImplemented, time.Since(start))
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error":          "sidecar_not_installed",
		"message":        "reverse-image search requires the CLIP visual-encoder sidecar; ships in a follow-up phase. similar_to:<uuid> queries via the DSL work today for existing embeddings.",
		"reserved_since": "1.16.B-3",
	})
}
