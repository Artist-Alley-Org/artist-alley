// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package feedback

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// Handler mounts the user-facing feedback endpoints. Authenticated
// callers only; anonymous returns 401.
type Handler struct {
	Service *Service
	Logger  *slog.Logger
	// ScrambleKey is the shared HMAC salt for the IP subnet hash
	// (matches the 1.19.D lockout pattern). Empty disables the hash
	// (audit rows land with ip_hash NULL).
	ScrambleKey string
}

// Mount attaches POST /search/feedback + DELETE /search/feedback/{id}.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/search/feedback", h.submit)
	r.Delete("/search/feedback/{id}", h.delete)
}

type submitRequest struct {
	DSL         string    `json:"dsl"`
	HitAssetID  uuid.UUID `json:"hit_asset_id"`
	HitPosition int32     `json:"hit_position"`
	Direction   string    `json:"direction"`
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}

	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if req.DSL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dsl_required"})
		return
	}
	if req.HitAssetID == uuid.Nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hit_asset_id_required"})
		return
	}
	if req.HitPosition < 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hit_position_must_be_positive"})
		return
	}
	if !ValidDirection(req.Direction) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_direction"})
		return
	}

	res, err := h.Service.Submit(r.Context(), SubmitParams{
		UserRef:     id.UserRef,
		DSL:         req.DSL,
		HitAssetID:  req.HitAssetID,
		HitPosition: req.HitPosition,
		Direction:   Direction(req.Direction),
		IPHash:      auth.IPSubnetHash(r, h.ScrambleKey, "search.feedback.v1:"),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrDisabled):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "feedback_disabled"})
			return
		case errors.Is(err, ErrRateLimited):
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
			return
		case errors.Is(err, ErrHitNotVisible):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "hit_not_visible"})
			return
		}
		h.logError(r, "feedback.submit", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        res.ID.String(),
		"direction": string(res.Direction),
		"flipped":   res.Flipped,
	})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}
	rowID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_id"})
		return
	}
	if err := h.Service.DeleteOwn(r.Context(), rowID, id.UserRef); err != nil {
		switch {
		case errors.Is(err, ErrDisabled):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "feedback_disabled"})
			return
		case errors.Is(err, ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		h.logError(r, "feedback.delete", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers --------------------------------------------------

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
