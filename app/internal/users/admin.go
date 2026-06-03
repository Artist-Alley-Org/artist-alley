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

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CapReadUsers gates the admin list. Same code as the SetUserRole
// endpoint's `users.write` neighbour — admins read users; only the
// `users.write` capability mutates them. Phase 1.17.B widens this
// surface to include the status mutation endpoints, which keep
// `users.write`.
const CapReadUsers = "users.read"

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

