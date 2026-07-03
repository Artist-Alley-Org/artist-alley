package reindex

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
)

// Handler mounts the admin reindex CRUD endpoints. All routes
// require the "system.admin" capability.
type Handler struct {
	Store  *Store
	JobSvc *jobs.Service
	Logger *slog.Logger
}

// Mount attaches the routes to r.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/admin/search/reindex", h.start)
	r.Get("/admin/search/reindex/runs", h.list)
	r.Get("/admin/search/reindex/runs/{id}", h.get)
	r.Post("/admin/search/reindex/runs/{id}/cancel", h.cancel)
}

type startRequest struct {
	Scope  string `json:"scope"`
	Target string `json:"target"`
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if req.Target == "" {
		req.Target = string(TargetBoth)
	}
	if !ValidTarget(req.Target) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_target"})
		return
	}
	scope, err := ParseScope(req.Scope)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_scope", "message": err.Error()})
		return
	}

	id := auth.IdentityFromContext(r.Context())
	var startedBy *int64
	if id != nil {
		ref := id.UserRef
		startedBy = &ref
	}
	row, err := h.Store.Start(r.Context(), StartParams{
		Scope:     scope,
		Target:    Target(req.Target),
		StartedBy: startedBy,
	})
	if err != nil {
		if errors.Is(err, ErrActiveRunExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run_in_progress"})
			return
		}
		h.logError(r, "reindex.start", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	// Enqueue the coordinator immediately (no ScheduledFor); the
	// job framework picks it up on the next tick.
	if _, err := h.JobSvc.Enqueue(r.Context(), JobTypeReindex, Payload{RunID: row.ID}, jobs.EnqueueOpts{}); err != nil {
		h.logError(r, "reindex.enqueue", err)
		// Complete-mark the row so the admin UI shows the failure
		// + a second Start doesn't hit the "already in progress"
		// gate.
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
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 32); err == nil {
			limit = int32(n)
		}
	}
	rows, err := h.Store.List(r.Context(), limit)
	if err != nil {
		h.logError(r, "reindex.list", err)
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
		h.logError(r, "reindex.get", err)
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
		h.logError(r, "reindex.cancel", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		"target":     r.Target,
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
