// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Capability grant / revoke override endpoints (Phase 1.17.F).
//
// Five operations on the admin user-detail page:
//
//   GET    /admin/users/{ref}/capabilities             — list both override lists
//   POST   /admin/users/{ref}/grants                   — add (or refresh) a grant
//   DELETE /admin/users/{ref}/grants/{capability}      — remove a grant
//   POST   /admin/users/{ref}/revokes                  — add (or refresh) a revoke
//   DELETE /admin/users/{ref}/revokes/{capability}     — remove a revoke
//
// Role-derived capabilities are NOT included here — those live on
// the role editor surface. This file only handles the per-user
// additive / subtractive deltas attached directly to the user via
// user_capability_grants / user_capability_revokes.
//
// Every mutation broadcasts via auth.InvalidateUserCaps so the
// Resolver's caps cache (and any federated peer's caps cache) drops
// the stale entry. The very next request from the subject user
// recomputes their effective set.

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ListAdminUserCapabilities returns the per-user grant + revoke
// override lists. Read-gated on users.read so the admin user-detail
// page can show them even to staff without users.write.
func (h *Handler) ListAdminUserCapabilities(
	ctx context.Context,
	req openapi.ListAdminUserCapabilitiesRequestObject,
) (openapi.ListAdminUserCapabilitiesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListAdminUserCapabilities401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("users.read") {
		return openapi.ListAdminUserCapabilities403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.read capability required"},
		}, nil
	}

	q := New(h.Pool)
	if _, err := q.GetUserPasswordHashByRef(ctx, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ListAdminUserCapabilities404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("auth: get user: %w", err)
	}

	grants, err := q.ListUserGrants(ctx, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("auth: list grants: %w", err)
	}
	revokes, err := q.ListUserRevokes(ctx, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("auth: list revokes: %w", err)
	}

	out := openapi.UserCapabilityOverrides{
		Grants:  make([]openapi.UserCapabilityOverride, 0, len(grants)),
		Revokes: make([]openapi.UserCapabilityOverride, 0, len(revokes)),
	}
	for _, g := range grants {
		out.Grants = append(out.Grants, openapi.UserCapabilityOverride{
			Capability: g.CapabilityCode,
			TeamId:     pgUUIDToOpenAPI(g.TeamID),
			Note:       strPtrOrNil(g.Note),
			GrantedBy:  g.GrantedByUserRef,
			GrantedAt:  g.GrantedAt.Time,
			ExpiresAt:  pgTimestampPtr(g.ExpiresAt),
		})
	}
	for _, r := range revokes {
		out.Revokes = append(out.Revokes, openapi.UserCapabilityOverride{
			Capability: r.CapabilityCode,
			TeamId:     pgUUIDToOpenAPI(r.TeamID),
			Note:       strPtrOrNil(r.Note),
			GrantedBy:  r.RevokedByUserRef,
			GrantedAt:  r.RevokedAt.Time,
			ExpiresAt:  pgTimestampPtr(r.ExpiresAt),
		})
	}
	return openapi.ListAdminUserCapabilities200JSONResponse(out), nil
}

// AddAdminUserGrant inserts (or refreshes) a per-user GRANT.
func (h *Handler) AddAdminUserGrant(
	ctx context.Context,
	req openapi.AddAdminUserGrantRequestObject,
) (openapi.AddAdminUserGrantResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddAdminUserGrant401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("users.write") {
		return openapi.AddAdminUserGrant403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.write capability required"},
		}, nil
	}
	if req.Body == nil || req.Body.Capability == "" {
		return openapi.AddAdminUserGrant400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "capability is required"},
		}, nil
	}
	// Phase 1.17.C — time-bound expiry. Reject past values so an
	// operator can't accidentally land a pre-stale grant that the
	// sweeper would reap on its next tick (silent grant-that-never-
	// was). Future or NULL only.
	expiresAt, perr := parseExpiryParam(req.Body.ExpiresAt)
	if perr != nil {
		return openapi.AddAdminUserGrant400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: perr.Error()},
		}, nil
	}

	q := New(h.Pool)
	if _, err := q.GetUserPasswordHashByRef(ctx, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddAdminUserGrant404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("auth: get user: %w", err)
	}

	teamUUID := openAPIToPgUUID(req.Body.TeamId)
	note := ""
	if req.Body.Note != nil {
		note = *req.Body.Note
	}
	if err := q.InsertUserGrant(ctx, InsertUserGrantParams{
		UserRef:          req.Ref,
		CapabilityCode:    req.Body.Capability,
		TeamID:            teamUUID,
		GrantedByUserRef: &caller.UserRef,
		Note:              note,
		ExpiresAt:         expiresAt,
	}); err != nil {
		return nil, fmt.Errorf("auth: insert grant: %w", err)
	}

	InvalidateUserCaps(ctx, h.CacheReg, req.Ref)

	if h.Audit != nil {
		h.Audit.CapabilityGranted(ctx, RequestFromContext(ctx),
			req.Ref, caller.UserRef,
			req.Body.Capability, uuidToStrOrEmpty(req.Body.TeamId), note)
	}

	return openapi.AddAdminUserGrant204Response{}, nil
}

// RemoveAdminUserGrant deletes a per-user GRANT by composite key
// (subject + capability + team_id). 404 when no matching row exists.
func (h *Handler) RemoveAdminUserGrant(
	ctx context.Context,
	req openapi.RemoveAdminUserGrantRequestObject,
) (openapi.RemoveAdminUserGrantResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemoveAdminUserGrant401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("users.write") {
		return openapi.RemoveAdminUserGrant403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.write capability required"},
		}, nil
	}

	q := New(h.Pool)
	teamUUID := openAPIToPgUUID(req.Params.TeamId)

	// Last-admin invariant: removing a global system.admin
	// grant may strip the user of admin powers (if they don't
	// hold it via a role). Check BEFORE the delete so the
	// pre-deletion count is meaningful.
	if req.Capability == "system.admin" && !teamUUID.Valid {
		if err := EnsureNotLastAdmin(ctx, q, req.Ref); err != nil {
			if errors.Is(err, ErrLastAdmin) {
				return openapi.RemoveAdminUserGrant400JSONResponse{Error: err.Error()}, nil
			}
			return nil, fmt.Errorf("auth: last-admin guard: %w", err)
		}
	}

	n, err := q.DeleteUserGrant(ctx, DeleteUserGrantParams{
		UserRef:       req.Ref,
		CapabilityCode: req.Capability,
		TeamID:         teamUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: delete grant: %w", err)
	}
	if n == 0 {
		return openapi.RemoveAdminUserGrant404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "grant not found"},
		}, nil
	}

	InvalidateUserCaps(ctx, h.CacheReg, req.Ref)

	if h.Audit != nil {
		h.Audit.CapabilityGrantRemoved(ctx, RequestFromContext(ctx),
			req.Ref, caller.UserRef,
			req.Capability, uuidToStrOrEmpty(req.Params.TeamId))
	}

	return openapi.RemoveAdminUserGrant204Response{}, nil
}

// AddAdminUserRevoke inserts (or refreshes) a per-user REVOKE.
// Symmetric to AddAdminUserGrant.
func (h *Handler) AddAdminUserRevoke(
	ctx context.Context,
	req openapi.AddAdminUserRevokeRequestObject,
) (openapi.AddAdminUserRevokeResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AddAdminUserRevoke401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("users.write") {
		return openapi.AddAdminUserRevoke403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.write capability required"},
		}, nil
	}
	if req.Body == nil || req.Body.Capability == "" {
		return openapi.AddAdminUserRevoke400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "capability is required"},
		}, nil
	}
	// Phase 1.17.C — same expiry validation as the grant path.
	expiresAt, perr := parseExpiryParam(req.Body.ExpiresAt)
	if perr != nil {
		return openapi.AddAdminUserRevoke400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: perr.Error()},
		}, nil
	}

	q := New(h.Pool)
	if _, err := q.GetUserPasswordHashByRef(ctx, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AddAdminUserRevoke404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("auth: get user: %w", err)
	}

	teamUUID := openAPIToPgUUID(req.Body.TeamId)

	// Last-admin invariant: adding a REVOKE for system.admin
	// nullifies the user's admin powers (per resolution: any
	// active revoke overrides both role-derived + grant-derived).
	// Refuse when this would leave zero admins. Only the global-
	// scope revoke (team_id IS NULL) affects the global
	// system.admin count; team-scoped revokes don't.
	if req.Body.Capability == "system.admin" && !teamUUID.Valid {
		if err := EnsureNotLastAdmin(ctx, q, req.Ref); err != nil {
			if errors.Is(err, ErrLastAdmin) {
				return openapi.AddAdminUserRevoke400JSONResponse{
					BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
				}, nil
			}
			return nil, fmt.Errorf("auth: last-admin guard: %w", err)
		}
	}

	note := ""
	if req.Body.Note != nil {
		note = *req.Body.Note
	}
	if err := q.InsertUserRevoke(ctx, InsertUserRevokeParams{
		UserRef:          req.Ref,
		CapabilityCode:    req.Body.Capability,
		TeamID:            teamUUID,
		RevokedByUserRef: &caller.UserRef,
		Note:              note,
		ExpiresAt:         expiresAt,
	}); err != nil {
		return nil, fmt.Errorf("auth: insert revoke: %w", err)
	}

	InvalidateUserCaps(ctx, h.CacheReg, req.Ref)

	if h.Audit != nil {
		h.Audit.CapabilityRevoked(ctx, RequestFromContext(ctx),
			req.Ref, caller.UserRef,
			req.Body.Capability, uuidToStrOrEmpty(req.Body.TeamId), note)
	}

	return openapi.AddAdminUserRevoke204Response{}, nil
}

// RemoveAdminUserRevoke deletes a per-user REVOKE by composite key.
func (h *Handler) RemoveAdminUserRevoke(
	ctx context.Context,
	req openapi.RemoveAdminUserRevokeRequestObject,
) (openapi.RemoveAdminUserRevokeResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RemoveAdminUserRevoke401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("users.write") {
		return openapi.RemoveAdminUserRevoke403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.write capability required"},
		}, nil
	}

	q := New(h.Pool)
	teamUUID := openAPIToPgUUID(req.Params.TeamId)
	n, err := q.DeleteUserRevoke(ctx, DeleteUserRevokeParams{
		UserRef:       req.Ref,
		CapabilityCode: req.Capability,
		TeamID:         teamUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: delete revoke: %w", err)
	}
	if n == 0 {
		return openapi.RemoveAdminUserRevoke404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "revoke not found"},
		}, nil
	}

	InvalidateUserCaps(ctx, h.CacheReg, req.Ref)

	if h.Audit != nil {
		h.Audit.CapabilityRevokeRemoved(ctx, RequestFromContext(ctx),
			req.Ref, caller.UserRef,
			req.Capability, uuidToStrOrEmpty(req.Params.TeamId))
	}

	return openapi.RemoveAdminUserRevoke204Response{}, nil
}

// pgUUIDToOpenAPI converts a pgtype.UUID (nullable) to the
// openapi_types.UUID pointer the response shape expects. Invalid
// pg UUIDs map to nil — the OpenAPI field is `nullable: true`.
func pgUUIDToOpenAPI(u pgtype.UUID) *openapi_types.UUID {
	if !u.Valid {
		return nil
	}
	v := openapi_types.UUID(u.Bytes)
	return &v
}

// openAPIToPgUUID converts the request-body / query-param UUID
// pointer to the pgtype.UUID the sqlc params expect. Nil means
// "global override" — stored as NULL in team_id.
func openAPIToPgUUID(u *openapi_types.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}

// uuidToStrOrEmpty returns the canonical hyphenated UUID string,
// or "" when the pointer is nil. Used as the audit-event team_id
// argument — audit prefers strings to keep the metadata blob
// JSON-friendly.
func uuidToStrOrEmpty(u *openapi_types.UUID) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// strPtrOrNil returns a pointer to s when s is non-empty, nil
// otherwise. Used to keep the OpenAPI `note` field out of the
// response when no note exists (it's `omitempty`).
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseExpiryParam normalises the optional expires_at from a request
// body into the pgtype.Timestamptz the sqlc layer wants. Phase 1.17.C —
// time-bound grants and revokes share this validation:
//
//   * nil pointer → permanent (Valid=false → INSERT NULL)
//   * future timestamp → time-bound (Valid=true)
//   * past timestamp → ErrPastExpiry; caller maps to 400
//
// A past value is operator error: the sweeper would reap the row on
// its next tick, producing a silent grant-that-never-was. Better to
// reject up-front with a clear message than to land a stale row.
func parseExpiryParam(p *time.Time) (pgtype.Timestamptz, error) {
	if p == nil {
		return pgtype.Timestamptz{}, nil
	}
	if !p.After(time.Now()) {
		return pgtype.Timestamptz{}, ErrPastExpiry
	}
	return pgtype.Timestamptz{Time: *p, Valid: true}, nil
}

// ErrPastExpiry is returned by parseExpiryParam when the caller
// supplied an expires_at value in the past. The grant/revoke
// handlers surface this as HTTP 400 with the message in the body.
var ErrPastExpiry = errors.New("expires_at must be in the future")

// pgTimestampPtr unwraps a pgtype.Timestamptz to *time.Time —
// returns nil when the row's column is SQL NULL (Valid=false)
// so the openapi `expires_at` field renders as null / omitted.
func pgTimestampPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
