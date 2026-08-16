// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CapImpersonate is the capability gate for the impersonation
// surface. Seeded by migration 00001 to the Admin role ONLY;
// per project memory it must not appear on any other seeded role.
const CapImpersonate = "auth.impersonate"

// AdminImpersonateUser starts an impersonation session for the
// target user. Returns a cookie-setting response so the very next
// request runs under the target's identity.
func (h *Handler) AdminImpersonateUser(
	ctx context.Context,
	req openapi.AdminImpersonateUserRequestObject,
) (openapi.AdminImpersonateUserResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AdminImpersonateUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapImpersonate) {
		return openapi.AdminImpersonateUser403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "auth.impersonate capability required"},
		}, nil
	}
	// Defense-in-depth: never chain. If the caller's session was
	// itself an impersonation, refuse — the audit trail would
	// have two layers of misattribution.
	if caller.IsImpersonating() {
		return openapi.AdminImpersonateUser400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "cannot impersonate while already impersonating; end the current impersonation first",
			},
		}, nil
	}
	// Refuse self-impersonation — no value, all confusion.
	if req.Ref == caller.UserRef {
		return openapi.AdminImpersonateUser400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "cannot impersonate yourself"},
		}, nil
	}

	// Target lookup + admin-on-admin guard.
	target, err := h.loadImpersonationTarget(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AdminImpersonateUser404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("auth: load impersonation target: %w", err)
	}
	if target.IsAdmin {
		return openapi.AdminImpersonateUser403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
				Error: "target holds system.admin — impersonating other admins is refused (no privilege-escalation chain)",
			},
		}, nil
	}

	httpReq := RequestFromContext(ctx)
	token, info, err := h.Sessions.IssueImpersonation(ctx, caller.UserRef, req.Ref, 0, httpReq)
	if err != nil {
		return nil, fmt.Errorf("auth: issue impersonation: %w", err)
	}

	if h.Audit != nil {
		reason := ""
		if req.Body != nil && req.Body.Reason != nil {
			reason = *req.Body.Reason
		}
		h.Audit.ImpersonationStarted(ctx, httpReq, req.Ref, caller.UserRef, info.ID.String(), reason)
	}

	expires := info.CreatedAt
	if info.ExpiresAt != nil {
		expires = *info.ExpiresAt
	}
	return impersonateSetCookieResponse{
		token:       token,
		sessionDays: 0, // sessions expires_at carries the hard cap; cookie is browser-scope only
		body: openapi.AdminImpersonateResult{
			TargetUserRef:  target.UserRef,
			TargetUsername: target.Username,
			SessionId:      openapi_types.UUID(info.ID),
			ExpiresAt:      expires,
		},
	}, nil
}

// EndImpersonation revokes the current impersonation session +
// issues a fresh session for the original admin. The cookie
// rotates to the admin's new session so the response leaves the
// caller signed in as themselves.
func (h *Handler) EndImpersonation(
	ctx context.Context,
	_ openapi.EndImpersonationRequestObject,
) (openapi.EndImpersonationResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.EndImpersonation401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.IsImpersonating() {
		return openapi.EndImpersonation400JSONResponse{
			Error: "current session is not an impersonation",
		}, nil
	}
	adminRef := *caller.ImpersonatedBy
	targetRef := caller.UserRef // target = whoever the impersonation session is bound to

	httpReq := RequestFromContext(ctx)
	// Revoke the current (impersonation) session BEFORE we mint
	// the new admin session — order matters: if minting fails for
	// any reason, the impersonation session is already gone and
	// the user falls back to the login screen rather than
	// staying-as-target.
	if caller.SessionID != nil {
		if err := h.Sessions.Revoke(ctx, *caller.SessionID); err != nil && h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn,
				"auth.impersonation_end.revoke_error",
				slog.String("err", err.Error()),
			)
		}
	}

	token, _, err := h.Sessions.Issue(ctx, adminRef, httpReq)
	if err != nil {
		return nil, fmt.Errorf("auth: re-issue admin session: %w", err)
	}

	// Look up the admin username for the response payload so the
	// frontend doesn't have to round-trip /auth/me before
	// updating its banner.
	var adminUsername string
	_ = h.Pool.QueryRow(ctx, `SELECT COALESCE(username, '') FROM "user" WHERE ref = $1`, adminRef).
		Scan(&adminUsername)

	if h.Audit != nil {
		sessID := ""
		if caller.SessionID != nil {
			sessID = caller.SessionID.String()
		}
		h.Audit.ImpersonationEnded(ctx, httpReq, targetRef, adminRef, sessID)
	}

	return endImpersonationSetCookieResponse{
		token:       token,
		sessionDays: 0,
		body: openapi.EndImpersonationResult{
			AdminUserRef:  adminRef,
			AdminUsername: adminUsername,
		},
	}, nil
}

// impersonationTarget is the small projection the impersonate
// path needs.
type impersonationTarget struct {
	UserRef  int64
	Username string
	IsAdmin  bool
}

// loadImpersonationTarget fetches the target's username + a
// boolean flag for whether they hold system.admin globally.
func (h *Handler) loadImpersonationTarget(ctx context.Context, ref int64) (impersonationTarget, error) {
	var out impersonationTarget
	var username *string
	err := h.Pool.QueryRow(ctx,
		`SELECT username FROM "user" WHERE ref = $1`, ref,
	).Scan(&username)
	if err != nil {
		return out, err
	}
	out.UserRef = ref
	if username != nil {
		out.Username = *username
	}
	// system.admin check via the effective-cap query — global
	// capability with NULL team_id is what we care about. Reuse
	// the existing query so closure expansion + role inheritance
	// are honoured.
	q := New(h.Pool)
	caps, err := q.EffectiveScopedCapabilitiesForUser(ctx, ref)
	if err != nil {
		return out, err
	}
	for _, c := range caps {
		if c.Code == SuperAdminCapability && !c.TeamID.Valid {
			out.IsAdmin = true
			break
		}
	}
	return out, nil
}

// --- cookie-setting response shims ---

// impersonateSetCookieResponse implements
// openapi.AdminImpersonateUserResponseObject and rotates the
// session cookie to the new impersonation token on the way out.
type impersonateSetCookieResponse struct {
	token       string
	sessionDays int
	body        openapi.AdminImpersonateResult
}

func (r impersonateSetCookieResponse) VisitAdminImpersonateUserResponse(w http.ResponseWriter) error {
	WriteSessionCookie(w, &http.Request{}, r.token, r.sessionDays)
	return openapi.AdminImpersonateUser200JSONResponse(r.body).VisitAdminImpersonateUserResponse(w)
}

// endImpersonationSetCookieResponse mirrors the above for the
// end-impersonation path; rotates the cookie back to a fresh
// admin session token.
type endImpersonationSetCookieResponse struct {
	token       string
	sessionDays int
	body        openapi.EndImpersonationResult
}

func (r endImpersonationSetCookieResponse) VisitEndImpersonationResponse(w http.ResponseWriter) error {
	WriteSessionCookie(w, &http.Request{}, r.token, r.sessionDays)
	return openapi.EndImpersonation200JSONResponse(r.body).VisitEndImpersonationResponse(w)
}

// Force-use of uuid + openapi_types to satisfy the linter when
// the generated types use them.
var (
	_ = uuid.Nil
)
