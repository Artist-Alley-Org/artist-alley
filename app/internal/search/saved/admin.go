// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package saved

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// AdminHandler is the admin surface for saved-search management.
// Phase 1.16.B-5 addition. All routes require the "system.admin"
// capability.
type AdminHandler struct {
	Store  *Store
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Mount attaches the admin routes to r.
func (h *AdminHandler) Mount(r chi.Router) {
	r.Get("/admin/saved-searches", h.list)
	r.Delete("/admin/saved-searches/{id}", h.deleteRow)
	r.Post("/admin/saved-searches/{id}/dismiss-error", h.dismissError)
}

// list returns saved-searches across ALL users with optional
// filters. Params:
//
//	limit          — page cap (default 50, max 200)
//	owner_user_ref — filter to one owner
//	has_failure    — true=only failing rows, false=only healthy
//	                 (matches AdminListSavedSearchesParams)
func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	limit := int32(50)
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 32); err == nil {
			limit = int32(n)
		}
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	params := AdminListSavedSearchesParams{Limit: limit}
	if s := r.URL.Query().Get("owner_user_ref"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			params.OwnerUserRef = &n
		}
	}
	if s := r.URL.Query().Get("has_failure"); s != "" {
		v := s == "true" || s == "1"
		params.HasFailure = &v
	}

	rows, err := New(h.Pool).AdminListSavedSearches(r.Context(), params)
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"admin.saved_search.list", slog.String("err", err.Error()))
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		items = append(items, adminRowToJSON(rowFromSQLC(s)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// deleteRow removes any saved-search by ID. No owner-check
// (admins can delete anyone's row).
func (h *AdminHandler) deleteRow(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_id"})
		return
	}
	if err := h.Store.Delete(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"admin.saved_search.delete", slog.String("err", err.Error()))
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dismissError is the "acknowledge this failure" action from the
// admin failures queue. Since the current schema doesn't track
// per-row error state (Phase 1.16.B-4 only stores hash + IDs),
// dismiss-error's effect is a coarse enable-toggle: it re-enables
// a paused row (nothing else to clear) OR pauses a row so it
// stops surfacing in the failures list. Documented in the UI
// tooltip.
func (h *AdminHandler) dismissError(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_id"})
		return
	}
	// Fetch → decide → patch. Zero-error toggle: if the row is
	// enabled + surfacing in failures, disable it (admin can
	// re-enable via the user's own page). If disabled, no-op.
	row, err := h.Store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	if !row.Enabled {
		writeJSON(w, http.StatusOK, adminRowToJSON(row))
		return
	}
	f := false
	updated, err := h.Store.Update(r.Context(), id, UpdateParams{Enabled: &f})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, adminRowToJSON(updated))
}

// CountFailing returns the current failing-row count. Used by the
// admin nav badge; cheap enough to call per request but boot wire
// caches it separately when the nav polls at high frequency.
func (h *AdminHandler) CountFailing(ctx context.Context) (int64, error) {
	return New(h.Pool).AdminCountFailingSavedSearches(ctx)
}

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

func adminRowToJSON(r Row) map[string]any {
	out := map[string]any{
		"id":                      r.ID.String(),
		"owner_user_ref":          r.OwnerUserRef,
		"name":                    r.Name,
		"dsl":                     r.DSL,
		"notify_channel":          r.NotifyChannel,
		"notify_interval_minutes": r.NotifyIntervalMinutes,
		"enabled":                 r.Enabled,
		"created_at":              r.CreatedAt.Format(time.RFC3339Nano),
		"updated_at":              r.UpdatedAt.Format(time.RFC3339Nano),
	}
	if r.LastRunAt != nil {
		out["last_run_at"] = r.LastRunAt.Format(time.RFC3339Nano)
	}
	if r.LastNotifiedAt != nil {
		out["last_notified_at"] = r.LastNotifiedAt.Format(time.RFC3339Nano)
	}
	if len(r.LastResultIDs) > 0 {
		out["last_hit_count"] = len(r.LastResultIDs)
	}
	return out
}

// jsonUsage keeps encoding/json imported for future admin
// endpoints (bulk actions accept JSON bodies).
var _ = json.Marshal
