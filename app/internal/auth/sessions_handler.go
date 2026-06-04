// Session management endpoints (Phase 1.17.C).
//
// Four operations:
//   - GET    /account/sessions               — caller's own
//   - DELETE /account/sessions/{id}          — caller's own, ownership-checked
//   - GET    /admin/users/{ref}/sessions     — admin (users.read)
//   - DELETE /admin/users/{ref}/sessions/{id} — admin (users.write)
//
// The list endpoint marks the row that's authenticating the current
// request as `current: true` on the self-service path so the UI can
// hide its own revoke button. The admin equivalent omits the flag
// (admins are viewing someone else's sessions, not their own).
//
// Audit: every admin-initiated revoke records an EventSessionRevoked
// row with the actor and reason. Self-service revokes also audit so
// "user signed out of their phone" is reconstructable later.

package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// rowsToAPI builds the openapi.SessionRow slice from the sqlc rows.
// `currentID` marks one row as current; pass uuid.Nil to omit the
// flag entirely (admin path).
func rowsToAPI(rows []ListSessionsForUserRow, currentID uuid.UUID) []openapi.SessionRow {
	out := make([]openapi.SessionRow, 0, len(rows))
	mark := currentID != uuid.Nil
	for _, r := range rows {
		row := openapi.SessionRow{
			Id:         openapi_types.UUID(r.ID.Bytes),
			CreatedAt:  r.CreatedAt.Time,
			LastUsedAt: r.LastUsedAt.Time,
		}
		if r.ExpiresAt.Valid {
			t := r.ExpiresAt.Time
			row.ExpiresAt = &t
		}
		if r.Ip != nil {
			s := r.Ip.String()
			row.Ip = &s
		}
		if r.UserAgent != nil {
			ua := *r.UserAgent
			row.UserAgent = &ua
		}
		if mark && uuid.UUID(r.ID.Bytes) == currentID {
			t := true
			row.Current = &t
		}
		out = append(out, row)
	}
	return out
}

// ListMySessions returns the caller's own active sessions.
func (h *Handler) ListMySessions(
	ctx context.Context,
	_ openapi.ListMySessionsRequestObject,
) (openapi.ListMySessionsResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListMySessions401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListSessionsForUser(ctx, id.UserRef)
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	currentID := uuid.Nil
	if id.SessionID != nil {
		currentID = *id.SessionID
	}
	return openapi.ListMySessions200JSONResponse{Items: rowsToAPI(rows, currentID)}, nil
}

// RevokeMySession revokes one of the caller's own sessions.
// Ownership-checked at the SQL level so a forged id from another
// user returns 404 (not 403) — existence isn't leaked.
func (h *Handler) RevokeMySession(
	ctx context.Context,
	req openapi.RevokeMySessionRequestObject,
) (openapi.RevokeMySessionResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RevokeMySession401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	sessUUID := uuid.UUID(req.Id)
	n, err := q.RevokeSessionForUser(ctx, RevokeSessionForUserParams{
		ID:      pgtype.UUID{Bytes: sessUUID, Valid: true},
		UserRef: caller.UserRef,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: revoke session: %w", err)
	}
	if n == 0 {
		return openapi.RevokeMySession404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "session not found"},
		}, nil
	}
	if h.Audit != nil {
		h.Audit.SessionRevoked(ctx, RequestFromContext(ctx), caller.UserRef, caller.UserRef, sessUUID.String(), "self-service")
	}
	return openapi.RevokeMySession204Response{}, nil
}

// ListAdminUserSessions returns a user's active sessions (admin).
func (h *Handler) ListAdminUserSessions(
	ctx context.Context,
	req openapi.ListAdminUserSessionsRequestObject,
) (openapi.ListAdminUserSessionsResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListAdminUserSessions401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("users.read") {
		return openapi.ListAdminUserSessions403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.read capability required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListSessionsForUser(ctx, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("auth: list user sessions: %w", err)
	}
	// Admin path: never set the current-marker (admin is looking at
	// someone else's sessions).
	return openapi.ListAdminUserSessions200JSONResponse{Items: rowsToAPI(rows, uuid.Nil)}, nil
}

// RevokeAdminUserSession revokes a specific user's session (admin).
func (h *Handler) RevokeAdminUserSession(
	ctx context.Context,
	req openapi.RevokeAdminUserSessionRequestObject,
) (openapi.RevokeAdminUserSessionResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RevokeAdminUserSession401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("users.write") {
		return openapi.RevokeAdminUserSession403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.write capability required"},
		}, nil
	}
	q := New(h.Pool)
	sessUUID := uuid.UUID(req.Id)
	// Ownership-checked: revoke only when the session id belongs to
	// the target user. Stops an admin-id-guess from killing the
	// wrong user's session if they got the path wrong.
	n, err := q.RevokeSessionForUser(ctx, RevokeSessionForUserParams{
		ID:      pgtype.UUID{Bytes: sessUUID, Valid: true},
		UserRef: req.Ref,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.RevokeAdminUserSession404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "session not found"},
			}, nil
		}
		return nil, fmt.Errorf("auth: admin revoke session: %w", err)
	}
	if n == 0 {
		return openapi.RevokeAdminUserSession404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "session not found"},
		}, nil
	}
	if h.Audit != nil {
		h.Audit.SessionRevoked(ctx, RequestFromContext(ctx), req.Ref, caller.UserRef, sessUUID.String(), "admin")
	}
	return openapi.RevokeAdminUserSession204Response{}, nil
}
