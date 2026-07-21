// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package scheduledactions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// capAdmin gates the scheduled-actions admin surface. ADR 0020 makes
// this system.admin: scheduling and cancelling actions that delete
// assets or flip sensitivity is an operator-level power, not a
// delegable read cap.
const capAdmin = "system.admin"

// HTTPHandler serves the two admin operations.
type HTTPHandler struct {
	store  *Store
	logger *slog.Logger
}

// NewHTTPHandler builds the admin handler over a Store.
func NewHTTPHandler(store *Store, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{store: store, logger: logger}
}

// ListScheduledActions — GET /admin/scheduled-actions.
func (h *HTTPHandler) ListScheduledActions(
	ctx context.Context,
	req openapi.ListScheduledActionsRequestObject,
) (openapi.ListScheduledActionsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListScheduledActions401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.ListScheduledActions403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capAdmin + " capability required"},
		}, nil
	}

	in := ListInput{Limit: 50}
	if req.Params.Limit != nil {
		in.Limit = int32(*req.Params.Limit)
	}
	if req.Params.State != nil {
		s := string(*req.Params.State)
		in.State = &s
	}
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		at, err := decodeCursor(*req.Params.Cursor)
		if err == nil {
			in.CursorCreatedAt = pgtype.Timestamptz{Time: at, Valid: true}
		}
	}

	rows, err := h.store.List(ctx, in)
	if err != nil {
		return nil, err
	}
	out := openapi.ScheduledActionList{Items: make([]openapi.ScheduledAction, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, toAPI(r))
	}
	// Only offer a next cursor when the page filled — a short page is
	// the last one.
	if len(rows) == int(in.Limit) {
		last := rows[len(rows)-1]
		cur := encodeCursor(last.CreatedAt.Time)
		out.NextCursor = &cur
	}
	return openapi.ListScheduledActions200JSONResponse(out), nil
}

// CancelScheduledAction — POST /admin/scheduled-actions/{id}/cancel.
func (h *HTTPHandler) CancelScheduledAction(
	ctx context.Context,
	req openapi.CancelScheduledActionRequestObject,
) (openapi.CancelScheduledActionResponseObject, error) {
	identity := auth.IdentityFromContext(ctx)
	if identity == nil {
		return openapi.CancelScheduledAction401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !identity.Can(capAdmin) {
		return openapi.CancelScheduledAction403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capAdmin + " capability required"},
		}, nil
	}

	id := uuid.UUID(req.Id)

	// Cancel first, then read back. Cancel guards on state='pending', so
	// rows-affected distinguishes the two non-success cases cleanly:
	// existence is checked by the read, cancellability by the update.
	cancelled, err := h.store.Cancel(ctx, id)
	if err != nil {
		return nil, err
	}
	row, err := h.store.Get(ctx, id)
	if err != nil {
		// Get returns ErrNoRows when the id doesn't exist.
		return openapi.CancelScheduledAction404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "scheduled action not found"},
		}, nil
	}
	if !cancelled {
		// The row exists but wasn't pending — terminal, not cancellable.
		return openapi.CancelScheduledAction409JSONResponse{
			Error: "action is " + row.State + " and cannot be cancelled",
		}, nil
	}
	return openapi.CancelScheduledAction200JSONResponse(toAPI(row)), nil
}

// toAPI maps a DB row to the wire shape.
func toAPI(r ScheduledAction) openapi.ScheduledAction {
	out := openapi.ScheduledAction{
		Id:           uuid.UUID(r.ID.Bytes),
		Action:       openapi.ScheduledActionAction(r.Action),
		TargetKind:   openapi.ScheduledActionTargetKind(r.TargetKind),
		TargetId:     r.TargetID,
		ScheduledFor: r.ScheduledFor.Time,
		State:        openapi.ScheduledActionState(r.State),
		Error:        r.Error,
		CreatedBy:    r.CreatedBy,
		CreatedAt:    r.CreatedAt.Time,
	}
	if r.ExecutedAt.Valid {
		t := r.ExecutedAt.Time
		out.ExecutedAt = &t
	}
	if len(r.Params) > 0 {
		var m map[string]any
		if err := json.Unmarshal(r.Params, &m); err == nil {
			out.Params = &m
		}
	}
	return out
}

func encodeCursor(t time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano)))
}

func decodeCursor(s string) (time.Time, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(b))
}
