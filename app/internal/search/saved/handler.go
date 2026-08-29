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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search"
	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
)

// Handler wires the CRUD HTTP endpoints. Owner-check happens in
// every method that mutates or reads a specific row.
type Handler struct {
	Store    *Store
	Executor *Executor
	Notifier *Notifier
	SiteURL  string
	Logger   *slog.Logger

	// Counter surfaces run-now events into the shared search
	// Counter (per pre-audit Q5 finding — B-4 deferred these).
	// Nil-safe.
	Counter CoordinatorCounter
}

// Mount attaches the routes to r. Called by the boot wire under
// /api/v1 so auth-resolver middleware has already run.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/search/saved", h.create)
	r.Get("/search/saved", h.list)
	r.Get("/search/saved/{id}", h.get)
	r.Patch("/search/saved/{id}", h.patch)
	r.Delete("/search/saved/{id}", h.deleteRow)
	r.Post("/search/saved/{id}/run-now", h.runNow)
}

// --- create ---------------------------------------------------------------

type createRequest struct {
	Name string `json:"name"`
	// DSL is the QUERY EXPRESSION the caller had on screen: their typed
	// text, or the advanced panel's hand-written DSL. It is not the whole
	// query — see Filters.
	DSL string `json:"dsl"`
	// Filters is the active facet selection, in the same
	// `dimension:value` wire form every GET surface takes and the same
	// form the SIBLING BUTTON on that page already posts (#907's
	// save-as-collection). #1368.
	//
	// # ⛔ WHY THE SELECTION TRAVELS AS TOKENS AND IS SERIALISED HERE
	//
	// The stored query stays ONE canonical DSL string — there is no
	// second persisted representation, no merge rule and no precedence
	// question. What arrives on the wire is a different matter, and it
	// is these tokens rather than a DSL string the browser assembled,
	// for a reason facet.ParseSelection already wrote down when it
	// REJECTED `dsl=` as the rail's wire shape: "the frontend would have
	// to splice UI state into a hand-written query string, re-quoting
	// values that contain a space or a colon". A browser-side quoter is
	// a SECOND implementation of the lexer's grammar, in a language that
	// cannot derive it from the lexer, which is precisely the shape ADR
	// 0093 decision 3 refuses. So the tokens travel and [search.ComposeDSL]
	// — one implementation, beside the lexer whose rules it satisfies —
	// writes the canonical string that lands in the column.
	//
	// A save with no filters posts an empty list and stores exactly what
	// it stored before.
	Filters               []string `json:"filters"`
	NotifyChannel         string   `json:"notify_channel"`
	NotifyIntervalMinutes int      `json:"notify_interval_minutes"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	// #1368 — a query is an expression, a selection, or both. It used to
	// be "expression required", which was correct while the selection had
	// nowhere to travel and is wrong now that it does: `filter=tag:sketch`
	// with no typed text is a complete, runnable search on /search, so
	// refusing to save it would be this endpoint disagreeing with the page
	// its button sits on.
	if req.Name == "" || (req.DSL == "" && len(req.Filters) == 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name_and_dsl_required"})
		return
	}
	if req.NotifyChannel == "" {
		req.NotifyChannel = NotifyChannelEmail
	}
	if req.NotifyIntervalMinutes <= 0 {
		req.NotifyIntervalMinutes = 60
	}
	// #1368 — compose the ONE canonical query that gets stored: the
	// caller's expression as a single parenthesised operand, conjuncted
	// with their active selection. Rejecting an unparseable selection
	// here is what stops a saved search from being persisted WIDER than
	// the page it was saved from, which is the defect this issue is.
	selection, err := facet.ParseSelection(req.Filters)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_filter"})
		return
	}
	canonical, err := search.ComposeDSL(req.DSL, selection)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "filter_not_representable", "message": err.Error()})
		return
	}
	// Validate the composed DSL now so an unparseable query never lands
	// in the table + wedges the coordinator later. ⛔ The COMPOSED string,
	// not the caller's expression: the stored value is what the executor
	// will parse, and validating the half of it that was posted would
	// leave the other half unchecked.
	if _, err := dsl.Parse(canonical); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dsl_parse_error", "message": err.Error()})
		return
	}
	// ComposeDSL trims, so a body carrying only whitespace reaches here
	// as the empty string — which the column forbids and the coordinator
	// could not run.
	if canonical == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name_and_dsl_required"})
		return
	}

	row, err := h.Store.Create(r.Context(), CreateParams{
		OwnerUserRef:          id.UserRef,
		Name:                  req.Name,
		DSL:                   canonical,
		NotifyChannel:         req.NotifyChannel,
		NotifyIntervalMinutes: req.NotifyIntervalMinutes,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrMaxPerUser):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max_per_user_exceeded"})
		case errors.Is(err, ErrNameConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "name_conflict"})
		case errors.Is(err, ErrIntervalTooSmall):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interval_below_floor"})
		case errors.Is(err, ErrInvalidNotifyChannel):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_notify_channel"})
		default:
			h.logError(r.Context(), "saved.create", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, rowToJSON(row))
}

// --- list -----------------------------------------------------------------

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}
	limit := int32(50)
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 32); err == nil {
			limit = int32(n)
		}
	}
	rows, err := h.Store.List(r.Context(), id.UserRef, limit)
	if err != nil {
		h.logError(r.Context(), "saved.list", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// --- get ------------------------------------------------------------------

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	row, ok := h.fetchOwned(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rowToJSON(row))
}

// --- patch ----------------------------------------------------------------

type patchRequest struct {
	Name                  *string `json:"name"`
	DSL                   *string `json:"dsl"`
	NotifyChannel         *string `json:"notify_channel"`
	NotifyIntervalMinutes *int    `json:"notify_interval_minutes"`
	Enabled               *bool   `json:"enabled"`
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request) {
	row, ok := h.fetchOwned(w, r)
	if !ok {
		return
	}
	var req patchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if req.DSL != nil {
		if _, err := dsl.Parse(*req.DSL); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dsl_parse_error", "message": err.Error()})
			return
		}
	}
	updated, err := h.Store.Update(r.Context(), row.ID, UpdateParams{
		Name:                  req.Name,
		DSL:                   req.DSL,
		NotifyChannel:         req.NotifyChannel,
		NotifyIntervalMinutes: req.NotifyIntervalMinutes,
		Enabled:               req.Enabled,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		case errors.Is(err, ErrNameConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "name_conflict"})
		case errors.Is(err, ErrIntervalTooSmall):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interval_below_floor"})
		case errors.Is(err, ErrInvalidNotifyChannel):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_notify_channel"})
		default:
			h.logError(r.Context(), "saved.patch", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		}
		return
	}
	writeJSON(w, http.StatusOK, rowToJSON(updated))
}

// --- delete ---------------------------------------------------------------

func (h *Handler) deleteRow(w http.ResponseWriter, r *http.Request) {
	row, ok := h.fetchOwned(w, r)
	if !ok {
		return
	}
	if err := h.Store.Delete(r.Context(), row.ID); err != nil {
		h.logError(r.Context(), "saved.delete", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- run-now --------------------------------------------------------------

func (h *Handler) runNow(w http.ResponseWriter, r *http.Request) {
	row, ok := h.fetchOwned(w, r)
	if !ok {
		return
	}
	res, err := h.Executor.Run(r.Context(), row)
	if err != nil {
		h.logError(r.Context(), "saved.run_now.execute", err)
		if h.Counter != nil {
			h.Counter.RecordRunResult("error")
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "execute_failed"})
		return
	}
	delta := ComputeDelta(row, res)
	sent := false
	if h.Notifier != nil {
		emitted, nerr := h.Notifier.Emit(r.Context(), row, delta, res, h.SiteURL)
		if nerr != nil {
			h.logError(r.Context(), "saved.run_now.notify", nerr)
		}
		sent = emitted
	}
	if _, err := h.Store.RecordRun(r.Context(), row.ID, res.Hash, res.HitIDs, sent); err != nil {
		h.logError(r.Context(), "saved.run_now.record", err)
	}
	// Phase 1.16.B-5 — surface run-now events into the shared
	// search Counter so /admin/search/health reflects operator +
	// user-triggered runs alongside scheduled coordinator runs.
	if h.Counter != nil {
		if delta.HashChanged {
			h.Counter.RecordDeltaHit()
		}
		if sent {
			h.Counter.RecordNotificationSent()
		}
		if len(res.HitIDs) == 0 {
			h.Counter.RecordRunResult("empty")
		} else {
			h.Counter.RecordRunResult("hit")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hit_count":    len(res.HitIDs),
		"added_count":  len(delta.Added),
		"hash_changed": delta.HashChanged,
		"notified":     sent,
	})
}

// --- helpers --------------------------------------------------------------

// fetchOwned pulls the row + confirms the caller owns it. Writes
// the appropriate HTTP error on failure and returns ok=false.
func (h *Handler) fetchOwned(w http.ResponseWriter, r *http.Request) (Row, bool) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return Row{}, false
	}
	rowID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_id"})
		return Row{}, false
	}
	row, err := h.Store.Get(r.Context(), rowID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return Row{}, false
		}
		h.logError(r.Context(), "saved.fetch", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return Row{}, false
	}
	if row.OwnerUserRef != id.UserRef {
		// 404 not 403 — don't leak that the row exists.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return Row{}, false
	}
	return row, true
}

func (h *Handler) logError(ctx context.Context, op string, err error) {
	if h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, op,
			slog.String("err", err.Error()),
		)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func rowToJSON(r Row) map[string]any {
	out := map[string]any{
		"id":                      r.ID.String(),
		"name":                    r.Name,
		"dsl":                     r.DSL,
		"notify_channel":          r.NotifyChannel,
		"notify_interval_minutes": r.NotifyIntervalMinutes,
		"enabled":                 r.Enabled,
		"created_at":              r.CreatedAt,
		"updated_at":              r.UpdatedAt,
	}
	if r.LastRunAt != nil {
		out["last_run_at"] = r.LastRunAt
	}
	if r.LastNotifiedAt != nil {
		out["last_notified_at"] = r.LastNotifiedAt
	}
	if len(r.LastResultIDs) > 0 {
		out["last_hit_count"] = len(r.LastResultIDs)
	}
	return out
}
