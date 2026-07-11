// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Admin audit surface for the activities ledger — Phase
// 1.22.A-bis-3b. Gated on system.admin per ADR 0044.
//
// Hot-path / caching note: deliberately not LRU-cached. The
// filter combination space is unbounded (type × source × actor ×
// kind × since × cursor) so memoising on filter signature would
// blow the working set for low hit rate. The underlying query is
// indexed by every column the filters use (type, source, actor,
// object_kind) via the partial indexes set up in migration 00049,
// so cold reads are sub-ms in practice.

package activities

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// AdminHandler is the openapi-strict adapter for the admin audit
// endpoint. Constructed once at boot; safe for concurrent use.
type AdminHandler struct {
	writer *Writer
}

// NewAdminHandler wires the admin audit handler. Shares the
// Writer's pool so reads see committed activities the moment
// RecordActivity returns.
func NewAdminHandler(w *Writer) *AdminHandler {
	return &AdminHandler{writer: w}
}

const adminMaxLimit = 200

// ListAdminActivities — GET /admin/activities. Cap-gated;
// cursor-paginated; filters all optional.
func (h *AdminHandler) ListAdminActivities(
	ctx context.Context,
	req openapi.ListAdminActivitiesRequestObject,
) (openapi.ListAdminActivitiesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListAdminActivities401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	// system.admin is the v1 gate. A finer-grained
	// federation.admin capability is documented in ADR 0043's
	// audit surface but doesn't ship until the moderation phase.
	if !id.Can("system.admin") {
		return openapi.ListAdminActivities403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}

	limit := int32(50)
	if req.Params.Limit != nil {
		l := int32(*req.Params.Limit)
		if l < 1 {
			l = 1
		}
		if l > adminMaxLimit {
			l = adminMaxLimit
		}
		limit = l
	}

	params := ListActivitiesAdminParams{
		ActivityType:  req.Params.ActivityType,
		Source:        req.Params.Source,
		ActorUserRef:  req.Params.ActorUserRef,
		ObjectKind:    req.Params.ObjectKind,
		RowLimit:      limit,
	}
	if req.Params.Since != nil {
		params.Since = pgtype.Timestamptz{Time: *req.Params.Since, Valid: true}
	}
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, cid, err := decodeAdminCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListAdminActivities401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		params.CursorPublishedAt = pgtype.Timestamptz{Time: ts, Valid: true}
		params.CursorID = pgtype.UUID{Bytes: cid, Valid: true}
	}

	// Fetch limit+1 to know if there's another page without
	// running a separate COUNT.
	params.RowLimit = limit + 1
	rows, err := New(h.writer.Pool).ListActivitiesAdmin(ctx, params)
	if err != nil {
		return nil, err
	}

	items := make([]openapi.AdminActivity, 0, limit)
	var lastTime time.Time
	var lastID uuid.UUID
	for i, r := range rows {
		if int32(i) >= limit {
			break
		}
		items = append(items, adminRowToAPI(r))
		lastTime = r.PublishedAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}
	var nextCursor *string
	if int32(len(rows)) > limit {
		c := encodeAdminCursor(lastTime, lastID)
		nextCursor = &c
	}

	return openapi.ListAdminActivities200JSONResponse(openapi.AdminActivityList{
		Items:      items,
		NextCursor: nextCursor,
	}), nil
}

// adminRowToAPI projects an Activity row into the openapi
// AdminActivity wire shape. Same JSONB-decode plumbing as
// rowToRecord but emits the openapi.AdminActivity directly so
// the caller doesn't need a second hop.
func adminRowToAPI(r Activity) openapi.AdminActivity {
	out := openapi.AdminActivity{
		Id:           uuid.UUID(r.ID.Bytes),
		ActivityUri:  r.ActivityUri,
		ActivityType: r.ActivityType,
		ActorUri:     r.ActorUri,
		ActorUserRef: r.ActorUserRef,
		ObjectUri:    r.ObjectUri,
		ObjectKind:   r.ObjectKind,
		ObjectLocalId: r.ObjectLocalID,
		TargetUri:    r.TargetUri,
		SignatureValue:  r.SignatureValue,
		SignaturePubkey: r.SignaturePubkey,
		Source:       r.Source,
		PublishedAt:  r.PublishedAt.Time,
		CreatedAt:    r.CreatedAt.Time,
	}
	_ = json.Unmarshal(r.ToUris, &out.To)
	_ = json.Unmarshal(r.CcUris, &out.Cc)
	var payload map[string]any
	if err := json.Unmarshal(r.Payload, &payload); err == nil {
		out.Payload = &payload
	}
	return out
}

// encodeAdminCursor / decodeAdminCursor — same shape as the
// other cursors in the codebase (messages, notifications). Opaque
// base64url of "RFC3339Nano|uuid".
func encodeAdminCursor(t time.Time, id uuid.UUID) string {
	raw := t.Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAdminCursor(s string) (time.Time, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errBadAdminCursor
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return t, id, nil
}

var errBadAdminCursor = errors.New("activities: invalid admin cursor")
