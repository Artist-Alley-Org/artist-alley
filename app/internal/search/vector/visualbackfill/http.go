package visualbackfill

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualstore"
)

// Handler mounts the admin visual-backfill CRUD endpoints. All routes
// require the "system.admin" capability.
type Handler struct {
	Store       *Store
	JobSvc      *jobs.Service
	Logger      *slog.Logger
	VisualStore *visualstore.Queries
	// Provider surfaces whether the sidecar is registered. The trigger
	// endpoint 503s when nil so operators diagnose the sysconfig gap
	// before enqueueing a run that would immediately fail.
	Provider visualprovider.Provider
}

// Mount attaches the routes to r.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/admin/search/visual-backfill", h.start)
	r.Get("/admin/search/visual-backfill/runs", h.list)
	r.Get("/admin/search/visual-backfill/runs/{id}", h.get)
	r.Post("/admin/search/visual-backfill/runs/{id}/cancel", h.cancel)
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	if h.Provider == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "provider_not_registered",
			"message": "visual encoder sidecar isn't registered — enable search.visual.enabled in sysconfig and verify the sidecar is reachable at boot",
		})
		return
	}

	id := auth.IdentityFromContext(r.Context())
	var startedBy *int64
	if id != nil {
		ref := id.UserRef
		startedBy = &ref
	}
	// Snapshot the backlog so the run row carries total_estimated for
	// the admin UI progress bar. Best-effort; a failure here doesn't
	// block the run — the counter shows "0 / unknown" instead.
	var total *int64
	if h.VisualStore != nil {
		if n, err := h.VisualStore.CountVisualEmbeddingBacklog(r.Context()); err == nil {
			total = &n
		} else {
			h.logError(r, "visualbackfill.count_backlog", err)
		}
	}
	row, err := h.Store.Start(r.Context(), StartParams{
		Scope:          Scope{Kind: ScopeAll},
		TotalEstimated: total,
		StartedBy:      startedBy,
	})
	if err != nil {
		if errors.Is(err, ErrActiveRunExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run_in_progress"})
			return
		}
		h.logError(r, "visualbackfill.start", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	if _, err := h.JobSvc.Enqueue(r.Context(), JobTypeVisualBackfill, Payload{RunID: row.ID}, jobs.EnqueueOpts{}); err != nil {
		h.logError(r, "visualbackfill.enqueue", err)
		_ = h.Store.Complete(r.Context(), row.ID, "enqueue failed: "+err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "enqueue_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, rowToJSON(row))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	limit := int32(20)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = int32(n)
		}
	}
	rows, err := h.Store.List(r.Context(), limit)
	if err != nil {
		h.logError(r, "visualbackfill.list", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, rowToJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_id"})
		return
	}
	row, err := h.Store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		h.logError(r, "visualbackfill.get", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, rowToJSON(row))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_id"})
		return
	}
	if err := h.Store.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		h.logError(r, "visualbackfill.cancel", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling"})
}

// --- helpers ---------------------------------------------------

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return false
	}
	if !id.Can("system.admin") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin_required"})
		return false
	}
	return true
}

func (h *Handler) logError(r *http.Request, op string, err error) {
	if h.Logger != nil {
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, op,
			slog.String("err", err.Error()))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func rowToJSON(r Row) map[string]any {
	out := map[string]any{
		"id":         r.ID.String(),
		"scope":      r.Scope,
		"processed":  r.Processed,
		"succeeded":  r.Succeeded,
		"failed":     r.Failed,
		"started_at": r.StartedAt.Format(time.RFC3339Nano),
		"is_active":  r.IsActive(),
	}
	if r.CompletedAt != nil {
		out["completed_at"] = r.CompletedAt.Format(time.RFC3339Nano)
	}
	if r.CancelledAt != nil {
		out["cancelled_at"] = r.CancelledAt.Format(time.RFC3339Nano)
	}
	if r.TotalEstimated != nil {
		out["total_estimated"] = *r.TotalEstimated
	}
	if r.StartedByUserRef != nil {
		out["started_by_user_ref"] = *r.StartedByUserRef
	}
	if r.LastError != nil {
		out["last_error"] = *r.LastError
	}
	return out
}
