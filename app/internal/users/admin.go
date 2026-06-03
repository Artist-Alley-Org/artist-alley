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
// user.approved column (1=active, 0=pending, 2=disabled per RS
// convention; preserved verbatim so the Phase 1.17.B lifecycle
// migration is additive rather than rewriting old rows).
//
// The (nil, true) return covers "no filter" — caller passes no
// status, every row matches.
func approvedFromStatus(s *openapi.ListAdminUsersParamsStatus) (*int64, bool) {
	if s == nil {
		return nil, true
	}
	switch *s {
	case openapi.ListAdminUsersParamsStatusActive:
		v := int64(1)
		return &v, true
	case openapi.ListAdminUsersParamsStatusPending:
		v := int64(0)
		return &v, true
	case openapi.ListAdminUsersParamsStatusDisabled:
		v := int64(2)
		return &v, true
	}
	return nil, false
}

// approvedFromUpdateStatus mirrors approvedFromStatus but takes
// the *AdminUserStatusUpdateStatus enum the update endpoint
// receives. Same numeric mapping — kept separate because the
// openapi generator emits distinct types per endpoint.
func approvedFromUpdateStatus(s openapi.AdminUserStatusUpdateStatus) (int64, bool) {
	switch s {
	case openapi.AdminUserStatusUpdateStatusActive:
		return 1, true
	case openapi.AdminUserStatusUpdateStatusPending:
		return 0, true
	case openapi.AdminUserStatusUpdateStatusDisabled:
		return 2, true
	}
	return 0, false
}

// statusFromApprovedResult is the AdminUserStatusResult-enum variant
// of statusFromApproved. Same mapping; openapi generator emits a
// distinct enum type per response schema.
func statusFromApprovedResult(approved int64) openapi.AdminUserStatusResultStatus {
	switch approved {
	case 1:
		return openapi.AdminUserStatusResultStatusActive
	case 0:
		return openapi.AdminUserStatusResultStatusPending
	}
	return openapi.AdminUserStatusResultStatusDisabled
}

// statusFromApprovedResultPrevious — separate type because the
// openapi generator emits a distinct enum per JSON property even
// when the underlying values are identical.
func statusFromApprovedResultPrevious(approved int64) openapi.AdminUserStatusResultPreviousStatus {
	switch approved {
	case 1:
		return openapi.AdminUserStatusResultPreviousStatusActive
	case 0:
		return openapi.AdminUserStatusResultPreviousStatusPending
	}
	return openapi.AdminUserStatusResultPreviousStatusDisabled
}

// statusFromApproved is the inverse — the column value coming
// back from the DB becomes the API enum. Unknown / out-of-range
// values surface as "disabled" defensively so a bad row never
// shows as "active".
func statusFromApproved(approved int64) openapi.AdminUserStatus {
	switch approved {
	case 1:
		return openapi.AdminUserStatusActive
	case 0:
		return openapi.AdminUserStatusPending
	}
	return openapi.AdminUserStatusDisabled
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
		lastRef = r.RsUserID

		row := openapi.AdminUser{
			Ref:         r.RsUserID,
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

// SetAdminUserStatus moves a user through the lifecycle states
// (Phase 1.17.B). The mutation is idempotent — re-sending the
// current status returns 200 with `changed: false` rather than an
// error, so admin tooling that drives this from a checkbox toggle
// doesn't trip on a no-op.
//
// Audit + cache invalidation fire only when the row actually
// changed; idempotent calls skip both.
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
	newApproved, ok := approvedFromUpdateStatus(req.Body.Status)
	if !ok {
		return openapi.SetAdminUserStatus400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid status"},
		}, nil
	}

	q := New(h.Pool)
	row, err := q.UpdateUserStatus(ctx, UpdateUserStatusParams{
		UserRef:   req.Ref,
		NewStatus: newApproved,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetAdminUserStatus404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("users: update status: %w", err)
	}

	prevApproved := row.PrevStatus
	resultStatus := statusFromApprovedResult(newApproved)
	prevResultStatus := statusFromApprovedResultPrevious(prevApproved)

	if row.Changed {
		// Per-user profile cache could have the old status baked in
		// (the public profile doesn't expose it today, but the admin
		// detail view + future "is this user disabled" badges will).
		// Invalidate so the next read repopulates.
		if h.byRef != nil {
			h.byRef.Invalidate(ctx, strconv.FormatInt(req.Ref, 10))
		}
		if h.Audit != nil {
			reason := ""
			if req.Body.Reason != nil {
				reason = *req.Body.Reason
			}
			h.Audit.UserStatusChanged(ctx, auth.RequestFromContext(ctx), req.Ref, caller.UserRef, prevApproved, newApproved, reason)
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

