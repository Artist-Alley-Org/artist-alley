// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Session management endpoints (Phase 1.17.C).
//
// Four operations:
//   - GET    /account/sessions               — caller's own
//   - DELETE /account/sessions/{id}          — caller's own, ownership-checked
//   - GET    /admin/users/{ref}/sessions     — admin (users.read;
//     the row's `ip` additionally needs users.pii.read, #573)
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

// Capabilities for the session surfaces.
//
// capUsersRead admits a caller to another user's session list;
// capUsersPIIRead additionally admits them to the personal data in it
// (#573). Same split, same shape, same reasoning as audit's
// system.audit.read / system.audit.pii.read (#425) — a raw IP is a
// separately-grantable data class, not a free rider on the area's read
// capability (ADR 0072). system.admin satisfies either as a wildcard in
// Identity.Can, so it needs no grant.
//
// capUsersWrite gates revoke, which is a write, not a read of personal
// data — unchanged.
const (
	capUsersRead    = "users.read"
	capUsersPIIRead = "users.pii.read"
	capUsersWrite   = "users.write"
)

// rowsToAPI builds the openapi.SessionRow slice from the sqlc rows.
// `currentID` marks one row as current; pass uuid.Nil to omit the
// flag entirely (admin path).
//
// includeIP gates the raw client IP — PERSONAL DATA (#567, #573). It is
// false unconditionally on the self-service path, and on the admin path
// carries the users.pii.read decision.
//
// The parameter's zero value is the SAFE one on purpose, matching
// audit's toOpenAPI: a caller who gets this wrong omits the IP rather
// than leaking it.
//
// Omitted, not blanked, matching the convention audit/handler.go
// settled on for actor IPs (#425): a field that is absent means "you
// may not see it" can never be confused with a recorded-but-empty
// value, and consumers already tolerate absence because `ip` is
// nullable.
func rowsToAPI(rows []ListSessionsForUserRow, currentID uuid.UUID, includeIP bool) []openapi.SessionRow {
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
		if includeIP && r.Ip != nil {
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
	// Demo mode: the install runs on ONE shared account, so "the
	// caller's own sessions" is really every visitor's sessions —
	// their device labels and login times, readable by the next
	// visitor (#567). Scope the list to the session actually making
	// this request.
	//
	// This is a self-scoped GET on the account's own data, so neither
	// ADR 0060 layer catches it: the edge allows reads, and the role
	// legitimately permits an account to read itself. The gap is that
	// "itself" stopped meaning one person.
	//
	// Forward-compatible with ADR 0045: a per-visitor ephemeral
	// sandbox is single-user by construction and would show exactly
	// one session anyway, so this narrows to the same answer 0045
	// arrives at rather than encoding a demo-only special case.
	if h.DemoMode {
		rows = filterToSession(rows, currentID)
	}
	// includeIP=false: never expose the raw client IP on the
	// self-service path (#567).
	return openapi.ListMySessions200JSONResponse{Items: rowsToAPI(rows, currentID, false)}, nil
}

// filterToSession narrows rows to the single session matching id.
// Returns an empty (non-nil) slice when id is uuid.Nil or absent, so
// the response is always a well-formed empty list rather than a leak
// by fallthrough.
func filterToSession(rows []ListSessionsForUserRow, id uuid.UUID) []ListSessionsForUserRow {
	out := make([]ListSessionsForUserRow, 0, 1)
	if id == uuid.Nil {
		return out
	}
	for _, r := range rows {
		if uuid.UUID(r.ID.Bytes) == id {
			out = append(out, r)
		}
	}
	return out
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
	sessUUID := uuid.UUID(req.Id)
	// Demo mode: on a shared account every OTHER session belongs to a
	// different visitor, so self-service revoke is a "sign everyone
	// else out" button (#567). Allow only the session authenticating
	// this request — i.e. logging yourself out, which is harmless.
	//
	// Enforced in the app on purpose. The demo's edge block is
	// TEMPORARY-567 and comes off at the v0.7.0 tag; more importantly
	// this is a self-service gap the edge was never meant to cover.
	// ADR 0060 keeps write-blocking at the edge as layer 1 and that
	// stays exactly as decided — this is the app-side authorization
	// layer answering a question the edge cannot see.
	if h.DemoMode {
		if caller.SessionID == nil || *caller.SessionID != sessUUID {
			return openapi.RevokeMySession403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
					Error: "demo mode: only the current session may be revoked",
				},
			}, nil
		}
	}
	q := New(h.Pool)
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
	if !id.Can(capUsersRead) {
		return openapi.ListAdminUserSessions403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capUsersRead + " capability required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListSessionsForUser(ctx, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("auth: list user sessions: %w", err)
	}
	// Admin path: never set the current-marker (admin is looking at
	// someone else's sessions).
	//
	// The IP rides on users.pii.read, NOT on users.read (#573). #567
	// left it on users.read and flagged the asymmetry against audit,
	// where actor IPs need system.audit.pii.read on top of
	// system.audit.read (#425) — same data class, two bars. Raised to
	// match audit rather than lowering audit to match this: locating a
	// suspicious session is still a legitimate need, but it is a
	// distinct one from "may read user records", and a missing grant
	// withholding data fails safer than the reverse.
	//
	// Resolved once for the page rather than per row, same as audit's
	// list view: the answer cannot change mid-response, and per-row
	// evaluation would invite a future caller to vary it.
	includeIP := id.Can(capUsersPIIRead)
	return openapi.ListAdminUserSessions200JSONResponse{Items: rowsToAPI(rows, uuid.Nil, includeIP)}, nil
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
	if !caller.Can(capUsersWrite) {
		return openapi.RevokeAdminUserSession403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capUsersWrite + " capability required"},
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
