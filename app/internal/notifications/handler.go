// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP handler for /account/notifications (Phase 1.17.I2).
//
// Read-side counterpart to Writer. The Writer + Handler share the
// pool + cache so the unread-count cache populated on reads is the
// same one Writer invalidates on writes.

package notifications

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const maxListLimit = 200

// Handler is the openapi-strict adapter for the read endpoints.
type Handler struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	writer *Writer // shared so unread-count invalidations propagate
}

// NewHandler wires the read handler. Shares the Writer's pool +
// cache instance so reads and writes coordinate on the same
// unread-count entries.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, writer *Writer) *Handler {
	return &Handler{pool: pool, logger: logger, writer: writer}
}

// ListMyNotifications — GET /account/notifications.
func (h *Handler) ListMyNotifications(
	ctx context.Context,
	req openapi.ListMyNotificationsRequestObject,
) (openapi.ListMyNotificationsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListMyNotifications401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	limit := int32(50)
	if req.Params.Limit != nil {
		l := *req.Params.Limit
		if l < 1 {
			l = 1
		}
		if l > maxListLimit {
			l = maxListLimit
		}
		limit = int32(l)
	}
	var onlyUnread *bool
	if req.Params.OnlyUnread != nil {
		onlyUnread = req.Params.OnlyUnread
	}
	var cursorTs pgtype.Timestamptz
	var cursorID pgtype.UUID
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, idv, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListMyNotifications401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: idv, Valid: true}
	}

	fetch := limit + 1
	rows, err := New(h.pool).ListMyNotifications(ctx, ListMyNotificationsParams{
		RecipientUserRef: id.UserRef,
		OnlyUnread:       onlyUnread,
		CursorCreatedAt:  cursorTs,
		CursorID:         cursorID,
		RowLimit:         fetch,
	})
	if err != nil {
		return nil, err
	}

	items := make([]openapi.Notification, 0, limit)
	var lastTime time.Time
	var lastID uuid.UUID
	for i, r := range rows {
		if int32(i) >= limit {
			// Don't emit the +1 row — it's the cursor probe.
			break
		}
		items = append(items, rowToAPI(r))
		lastTime = r.CreatedAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}
	var nextCursor *string
	if int32(len(rows)) > limit {
		c := encodeCursor(lastTime, lastID)
		nextCursor = &c
	}
	return openapi.ListMyNotifications200JSONResponse(openapi.NotificationList{
		Items:      items,
		NextCursor: nextCursor,
	}), nil
}

// GetMyUnreadNotificationCount — GET /account/notifications/unread-count.
//
// Hot path — every authenticated page render hits this for the bell
// badge. Backed by the cache.Registry-broadcast unread cache on
// the Writer (shared between handler + writer).
func (h *Handler) GetMyUnreadNotificationCount(
	ctx context.Context,
	_ openapi.GetMyUnreadNotificationCountRequestObject,
) (openapi.GetMyUnreadNotificationCountResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetMyUnreadNotificationCount401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	count, err := h.unreadCount(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	return openapi.GetMyUnreadNotificationCount200JSONResponse(openapi.NotificationUnreadCount{
		Count: count,
	}), nil
}

// unreadCount returns the cached value when available, falls back
// to the DB + populates the cache otherwise. Shared between the
// HTTP handler and (in future sub-phases) any internal consumer
// that wants the count without an HTTP round-trip.
func (h *Handler) unreadCount(ctx context.Context, ref int64) (int64, error) {
	if h.writer != nil && h.writer.unread != nil {
		if hit, ok := h.writer.unread.Get(unreadKey(ref)); ok {
			return hit, nil
		}
	}
	count, err := New(h.pool).CountMyUnreadNotifications(ctx, ref)
	if err != nil {
		return 0, err
	}
	if h.writer != nil && h.writer.unread != nil {
		h.writer.unread.Add(unreadKey(ref), count)
	}
	return count, nil
}

// MarkNotificationRead — POST /account/notifications/{id}/read.
func (h *Handler) MarkNotificationRead(
	ctx context.Context,
	req openapi.MarkNotificationReadRequestObject,
) (openapi.MarkNotificationReadResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.MarkNotificationRead401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	rows, err := New(h.pool).MarkNotificationRead(ctx, MarkNotificationReadParams{
		ID:               pgtype.UUID{Bytes: req.Id, Valid: true},
		RecipientUserRef: id.UserRef,
	})
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		// Either the row doesn't exist, it isn't ours, or it's
		// already read. The 404 path is the right code for all
		// three — clients shouldn't be able to enumerate which
		// notification IDs they own.
		return openapi.MarkNotificationRead404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "notification not found"},
		}, nil
	}
	h.writer.invalidateUnread(ctx, id.UserRef)
	return openapi.MarkNotificationRead204Response{}, nil
}

// MarkAllMyNotificationsRead — POST /account/notifications/read-all.
func (h *Handler) MarkAllMyNotificationsRead(
	ctx context.Context,
	_ openapi.MarkAllMyNotificationsReadRequestObject,
) (openapi.MarkAllMyNotificationsReadResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.MarkAllMyNotificationsRead401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{
				Error: "authentication required",
			},
		}, nil
	}
	count, err := New(h.pool).MarkAllMyNotificationsRead(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		h.writer.invalidateUnread(ctx, id.UserRef)
	}
	return openapi.MarkAllMyNotificationsRead200JSONResponse(openapi.NotificationMarkAllResult{
		MarkedCount: count,
	}), nil
}

// rowToAPI projects a sqlc row into the openapi wire shape.
// Currently doesn't denormalize actor name/avatar — that's a join
// the ListMyNotifications query doesn't do today. Frontend will
// resolve actor info via separate /users/{ref} fetches keyed on
// actor_user_ref; we'll fold the join into the SQL when actor
// names start dominating the inbox-load time.
func rowToAPI(r Notification) openapi.Notification {
	var readAt *time.Time
	if r.ReadAt.Valid {
		t := r.ReadAt.Time
		readAt = &t
	}
	var payload map[string]any
	if len(r.Payload) > 0 {
		_ = json.Unmarshal(r.Payload, &payload)
	}
	return openapi.Notification{
		Id:           uuid.UUID(r.ID.Bytes),
		ActorUserRef: r.ActorUserRef,
		Verb:         r.Verb,
		TargetKind:   r.TargetKind,
		TargetId:     r.TargetID,
		Payload:      &payload,
		ReadAt:       readAt,
		DeliveredAt:  r.DeliveredAt.Time,
		CreatedAt:    r.CreatedAt.Time,
	}
}

// Cursor encoding mirrors the posts package — (timestamp, uuid)
// pair base64-encoded. Opaque to the client.
func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := t.Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errBadCursor
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

// Sentinel — small enough to inline without errors package import.
type cursorErr string

func (e cursorErr) Error() string { return string(e) }

var errBadCursor cursorErr = "notifications: bad cursor"

// Compile-time guarantee that cache lookups can use the same
// key-format helper the Writer uses.
var _ = cache.Registry{}
