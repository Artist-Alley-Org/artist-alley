// Admin-side user surface (Phase 1.17.A).
//
// The handler in handler.go is profile-shaped (public read + owner
// edit). This file owns the admin list — paginated, capability-
// gated, surfaces operational columns (status, last_active, primary
// role, auth_origin) the public profile would never expose.
//
// No list-level cache: every user write would invalidate every
// filter combination's page key, defeating the point. The per-user
// `byRef` cache already hot-paths repeat single-user reads on the
// detail page. The list query is index-backed (migration 00037)
// with cursor pagination, so 100k-user instances answer in <100ms.

package users

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CapReadUsers gates the admin list. Same code as the SetUserRole
// endpoint's `users.write` neighbour — admins read users; only the
// `users.write` capability mutates them.
const CapReadUsers = "users.read"

// CapApproveUsers gates the lifecycle state machine (Phase 1.17.B).
// Distinct from users.write so a future "User Approver" role can
// move accounts through pending → active → disabled without
// inheriting role-assignment + grant/revoke rights.
const CapApproveUsers = "users.approve"

// approvedFromStatus maps the API status enum onto the underlying
// user.approved column (1=active, 0=pending, 2=disabled per legacy
// convention; preserved verbatim so the Phase 1.17.B lifecycle
// migration is additive rather than rewriting old rows).
//
// The (nil, true) return covers "no filter" — caller passes no
// status, every row matches.
func approvedFromStatus(s *openapi.ListAdminUsersParamsStatus) (*int64, bool) {
	if s == nil {
		return nil, true
	}
	st, ok := FromOpenAPIListStatus(*s)
	if !ok {
		return nil, false
	}
	v := int64(st)
	return &v, true
}

// Phase 1.17.A — the legacy switch-on-int helpers
// (approvedFromUpdateStatus / statusFromApprovedResult / etc.)
// have been replaced by the typed mappings in userstate.go. The
// shims below keep the existing call sites compiling while pointing
// at the typed source-of-truth, so a future cleanup can drop them
// in a single change once every consumer has migrated.

func approvedFromUpdateStatus(s openapi.AdminUserStatusUpdateStatus) (int64, bool) {
	if st, ok := FromOpenAPIUpdateStatus(s); ok {
		return int64(st), true
	}
	return 0, false
}

func statusFromApprovedResult(approved int64) openapi.AdminUserStatusResultStatus {
	return ToOpenAPIResultStatus(UserState(approved))
}

func statusFromApprovedResultPrevious(approved int64) openapi.AdminUserStatusResultPreviousStatus {
	return ToOpenAPIResultPreviousStatus(UserState(approved))
}

func statusFromApproved(approved int64) openapi.AdminUserStatus {
	return ToOpenAPIListStatus(UserState(approved))
}

// encodeAdminUserCursor packs (created_at, ref) into an opaque
// pagination token. Same shape as assets.encodeCursor — RFC3339Nano
// + "|" + decimal ref, base64-url. Keeps the wire shape stable
// across pages and means a re-issued cursor from yesterday still
// resumes correctly even if newer users were inserted in between
// (those just appear on the page before — never lost).
func encodeAdminUserCursor(t time.Time, ref int64) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(ref, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAdminUserCursor(s string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("decode: %w", err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, errors.New("bad cursor shape")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("parse time: %w", err)
	}
	ref, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("parse ref: %w", err)
	}
	return t, ref, nil
}

// ListAdminUsers implements the openapi method.
func (h *Handler) ListAdminUsers(
	ctx context.Context,
	req openapi.ListAdminUsersRequestObject,
) (openapi.ListAdminUsersResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListAdminUsers401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapReadUsers) {
		return openapi.ListAdminUsers403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.read capability required"},
		}, nil
	}

	statusValue, ok := approvedFromStatus(req.Params.Status)
	if !ok {
		// Defensive: the openapi validator should already have
		// rejected, but if a generated-client somehow sends an
		// unknown enum we surface 400 here rather than silently
		// returning the whole table.
		return openapi.ListAdminUsers500JSONResponse{
			InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid status filter"},
		}, nil
	}

	var search *string
	if req.Params.Q != nil {
		s := strings.TrimSpace(*req.Params.Q)
		if s != "" {
			search = &s
		}
	}

	var (
		cursorTs  pgtype.Timestamptz
		cursorRef *int64
	)
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, ref, err := decodeAdminUserCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListAdminUsers500JSONResponse{
				InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorRef = &ref
	}

	limit := int64(50)
	if req.Params.Limit != nil && *req.Params.Limit > 0 {
		limit = int64(*req.Params.Limit)
	}
	if limit > 200 {
		limit = 200
	}

	q := New(h.Pool)

	// Fetch one extra to detect end-of-page; trim before returning.
	rows, err := q.ListAdminUsers(ctx, ListAdminUsersParams{
		StatusValue:     statusValue,
		Search:          search,
		CursorCreatedAt: cursorTs,
		CursorRef:       cursorRef,
		LimitN:          limit + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("users: list admin: %w", err)
	}

	total, err := q.CountAdminUsers(ctx, CountAdminUsersParams{
		StatusValue: statusValue,
		Search:      search,
	})
	if err != nil {
		return nil, fmt.Errorf("users: count admin: %w", err)
	}

	hasMore := int64(len(rows)) > limit
	if hasMore {
		rows = rows[:limit]
	}

	items := make([]openapi.AdminUser, 0, len(rows))
	var lastCreated time.Time
	var lastRef int64
	for _, r := range rows {
		display := r.DisplayName
		if display == "" {
			if r.Fullname != nil && *r.Fullname != "" {
				display = *r.Fullname
			} else if r.Username != nil {
				display = *r.Username
			}
		}
		username := ""
		if r.Username != nil {
			username = *r.Username
		}
		var createdAt time.Time
		if r.CreatedAt.Valid {
			createdAt = r.CreatedAt.Time
			lastCreated = createdAt
		}
		lastRef = r.UserRef

		row := openapi.AdminUser{
			Ref:         r.UserRef,
			Username:    username,
			DisplayName: display,
			Status:      statusFromApproved(r.Approved),
			CreatedAt:   createdAt,
			Fullname:    r.Fullname,
			Email:       r.Email,
			AvatarUrl:   r.AvatarUrl,
			AuthOrigin:  r.AuthOrigin,
			PrimaryRole: &r.PrimaryRole,
		}
		if r.LastActive.Valid {
			t := r.LastActive.Time
			row.LastActive = &t
		}
		if r.AccountExpires.Valid {
			t := r.AccountExpires.Time
			row.AccountExpires = &t
		}
		if r.LockoutUntil.Valid {
			t := r.LockoutUntil.Time
			row.LockoutUntil = &t
		}
		fc := r.FailedLoginCount
		row.FailedLoginCount = &fc
		if r.ProfileOriginServerID.Valid {
			id := openapi_types.UUID(r.ProfileOriginServerID.Bytes)
			row.ProfileOriginServerId = &id
		}
		items = append(items, row)
	}

	resp := openapi.AdminUserList{
		Items: items,
		Total: total,
	}
	if hasMore {
		c := encodeAdminUserCursor(lastCreated, lastRef)
		resp.NextCursor = &c
	}
	return openapi.ListAdminUsers200JSONResponse(resp), nil
}

// SetAdminUserStatus moves a user through the typed lifecycle
// state machine (Phase 1.17.A). Idempotent — re-sending the
// current status returns 200 with `changed: false` rather than
// erroring, so admin tooling driven by a checkbox toggle doesn't
// trip on no-ops.
//
// Pipeline (every gate logged):
//
//  1. auth — caller present + holds CapApproveUsers.
//  2. body — status enum parses to a known UserState.
//  3. transition matrix — (from → to) is in the legal set per
//     ValidateTransition. Out-of-matrix rejected with 400.
//  4. last-admin invariant — RequiresLastAdminCheck gates
//     transitions OUT OF active (covers disable, archive,
//     and the should-never-happen active→pending). Refusal
//     emits AdminUserRefusedLastAdmin for alerting.
//  5. write — typed transition method on Recorder fires AFTER
//     commit + cache invalidation. Per-transition events
//     (admin.users.approved / .disabled / .archived / .restored)
//     run ALONGSIDE the generic user.status_changed for
//     backstop compatibility.
//
// Audit + cache invalidation fire only when the row actually
// changed; idempotent calls skip both. The state cache is
// invalidated even for the rare race where another instance
// transitioned the user concurrently — see InvalidateUserState.
func (h *Handler) SetAdminUserStatus(
	ctx context.Context,
	req openapi.SetAdminUserStatusRequestObject,
) (openapi.SetAdminUserStatusResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.SetAdminUserStatus401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapApproveUsers) {
		return openapi.SetAdminUserStatus403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.approve capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetAdminUserStatus400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	target, ok := FromOpenAPIUpdateStatus(req.Body.Status)
	if !ok {
		return openapi.SetAdminUserStatus400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid status"},
		}, nil
	}
	reason := ""
	if req.Body.Reason != nil {
		reason = *req.Body.Reason
	}

	// Resolve the current state via the cache-first path so the
	// transition-matrix check + the last-admin gate both see the
	// same value the write below will compare against. The PG-side
	// idempotency check in UpdateUserStatus is the load-bearing
	// race-free barrier (CAS via the WITH prior pattern); this
	// pre-read just lets us return a clean ErrInvalidTransition
	// before incurring the write.
	current, err := h.GetUserState(ctx, req.Ref)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetAdminUserStatus404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: read current state: %w", err)
	}
	if err := ValidateTransition(current, target); err != nil {
		return openapi.SetAdminUserStatus400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}

	// Last-admin invariant. Fires only on transitions OUT OF
	// active (per RequiresLastAdminCheck) — pending can't hold
	// admin, and disabled/archived users don't authenticate so
	// their admin grants don't count toward the active total.
	if current != target && RequiresLastAdminCheck(current, target) {
		if err := auth.EnsureNotLastAdmin(ctx, auth.New(h.Pool), req.Ref); err != nil {
			if errors.Is(err, auth.ErrLastAdmin) {
				if h.Audit != nil {
					h.Audit.AdminUserRefusedLastAdmin(
						ctx, auth.RequestFromContext(ctx),
						req.Ref, caller.UserRef,
						current.String(), target.String(), reason,
					)
				}
				return openapi.SetAdminUserStatus400JSONResponse{
					BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
				}, nil
			}
			return nil, fmt.Errorf("users: last-admin guard: %w", err)
		}
	}

	q := New(h.Pool)
	row, err := q.UpdateUserStatus(ctx, UpdateUserStatusParams{
		UserRef:   req.Ref,
		NewStatus: int64(target),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetAdminUserStatus404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: update status: %w", err)
	}

	prevState := UserState(row.PrevStatus)
	resultStatus := ToOpenAPIResultStatus(target)
	prevResultStatus := ToOpenAPIResultPreviousStatus(prevState)

	if row.Changed {
		// Per-user profile cache + state cache. Profile cache
		// because future admin views may bake state into the
		// payload; state cache because Commit 2's auth gate reads
		// it on every login.
		if h.byRef != nil {
			h.byRef.Invalidate(ctx, strconv.FormatInt(req.Ref, 10))
		}
		h.InvalidateUserState(ctx, req.Ref)

		// Session-revocation cascade (Phase 1.17.A). Any
		// transition OUT OF active means the user can no longer
		// authenticate, so existing sessions must die immediately
		// — otherwise a disabled user with an open browser tab
		// keeps API access until their cookie expires naturally.
		// nil-safe: when no revoker is wired (test fixtures), the
		// cascade is silently skipped.
		if h.sessionRevoker != nil && prevState == UserStateActive && target != UserStateActive {
			if n, err := h.sessionRevoker(ctx, req.Ref); err != nil {
				if h.Logger != nil {
					h.Logger.LogAttrs(ctx, slog.LevelWarn,
						"users.session.cascade.failed",
						slog.Int64("user_ref", req.Ref),
						slog.String("err", err.Error()),
					)
				}
			} else if n > 0 && h.Logger != nil {
				h.Logger.LogAttrs(ctx, slog.LevelInfo,
					"users.session.cascade.revoked",
					slog.Int64("user_ref", req.Ref),
					slog.Int64("count", n),
					slog.String("previous", prevState.String()),
					slog.String("next", target.String()),
				)
			}
		}

		if h.Audit != nil {
			httpReq := auth.RequestFromContext(ctx)
			// Generic backstop event — keeps existing consumers
			// (alerting rules, audit-log dashboards filtering on
			// user.status_changed) working unchanged.
			h.Audit.UserStatusChanged(ctx, httpReq, req.Ref, caller.UserRef, int64(prevState), int64(target), reason)
			// Typed per-transition event. Dispatch keyed off the
			// destination state — Approved when pending→active,
			// Restored when {disabled,archived}→active, etc.
			emitTypedTransition(ctx, h.Audit, httpReq, req.Ref, caller.UserRef, prevState, target, reason)
		}
	}

	resp := openapi.AdminUserStatusResult{
		Ref:            req.Ref,
		Status:         resultStatus,
		PreviousStatus: &prevResultStatus,
		Changed:        row.Changed,
	}
	return openapi.SetAdminUserStatus200JSONResponse(resp), nil
}

// emitTypedTransition fires the per-transition typed audit event
// that corresponds to the (from, to) pair. Approvals go pending →
// active; restores go {disabled,archived} → active; disables go
// active → disabled; archives go {active,disabled} → archived.
// Pairs outside that set are no-ops here — ValidateTransition
// blocked them upstream.
func emitTypedTransition(ctx context.Context, rec auditRecorder, req *http.Request, subjectUserRef, actorUserRef int64, from, to UserState, reason string) {
	prev := from.String()
	next := to.String()
	switch {
	case from == UserStatePending && to == UserStateActive:
		rec.AdminUserApproved(ctx, req, subjectUserRef, actorUserRef, prev, next, reason)
	case to == UserStateActive: // restore from disabled or archived
		rec.AdminUserRestored(ctx, req, subjectUserRef, actorUserRef, prev, next, reason)
	case to == UserStateDisabled:
		rec.AdminUserDisabled(ctx, req, subjectUserRef, actorUserRef, prev, next, reason)
	case to == UserStateArchived:
		rec.AdminUserArchived(ctx, req, subjectUserRef, actorUserRef, prev, next, reason)
	}
}

