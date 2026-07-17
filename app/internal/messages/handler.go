// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package messages implements direct-message (DM) HTTP surface
// for Phase 1.17.I-a (feat/user-surfaces).
//
// Endpoints (rooted at /api/v1):
//
//   GET    /account/messages                  inbox: one row per peer
//   GET    /account/messages/unread-count     envelope-pill backing
//   GET    /account/messages/{peer_ref}       thread between me + peer
//   POST   /account/messages/{peer_ref}       send DM
//   POST   /account/messages/{peer_ref}/read  mark thread as read
//
// Permission-aware: send-DM consults a blockChecker (local
// interface; concrete *social.Handler injected at boot) and
// refuses with 403 when either party has blocked the other. On
// success the send path fires a `direct_message_received`
// notification through a notifier seam (also local interface,
// concrete *notifications.Writer injected at boot) — the writer
// independently re-runs the block + channel-pref gates, so a user
// who muted the verb still receives the DM but no bell ping.
//
// Caching: per-recipient unread-DM count behind
// messages.unread_dm_count LRU. Every write that affects the
// count (send to me, mark-read by me) invalidates, and the
// cache.Registry NOTIFY broadcasts so federated peers drop their
// stale copies in the same beat.

package messages

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const maxListLimit = 200

// cacheDomainUnreadDM holds per-recipient unread DM counts. The
// envelope-icon pill on every page render is this query.
const cacheDomainUnreadDM = "messages.unread_dm_count"

// blockChecker mirrors notifications' shape — local interface so
// this package doesn't import social directly. social.Handler.HasBlockBetween
// is the concrete impl, wired at boot.
type blockChecker interface {
	HasBlockBetween(ctx context.Context, a, b int64) (bool, error)
}

// notifier mirrors social's shape — primitive-args interface;
// notifications.Writer wrapped in an adapter at boot is the impl.
type notifier interface {
	Notify(ctx context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, payload map[string]any) error
}

// userExister gates send-DM with a 404 when the recipient doesn't
// exist. Same shape social.Handler uses; reusing the social adapter
// at boot would be possible but keeping a tiny local one avoids
// pulling social just for this lookup.
type userExister interface {
	UserExists(ctx context.Context, ref int64) (bool, error)
}

// Handler is the openapi-strict adapter.
type Handler struct {
	Pool     *pgxpool.Pool
	Logger   *slog.Logger
	registry *cache.Registry

	blocks blockChecker
	notify notifier
	users  userExister

	unreadCount *cache.Cache[int64]

	// Activities ledger writer + baseURL resolver (Phase 1.22.A-bis-2
	// per ADR 0044). When wired, SendDirectMessage routes through
	// h.activities.WithEmission so the DM row + Create(Note) activity
	// commit atomically and the direct_message_received notification
	// fires after commit. nil-safe pre-ADR-0044 fallback for tests.
	activities *activities.Writer
	baseURLFn  func(ctx context.Context) string
}

// NewHandler wires the handler + the per-recipient unread cache.
// The block/notify/user-exister deps are nil at construction and
// must be injected via setters before requests fire — same
// post-construction pattern auth.Handler uses for its registry.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Handler {
	h := &Handler{Pool: pool, Logger: logger, registry: registry}
	if registry != nil {
		// 10k entries comfortably fits the active-users-in-last-30-days
		// population at typical install sizes; entries are ~24 bytes
		// (int64 + bookkeeping) so the LRU sits under 1MB at capacity.
		h.unreadCount = cache.Register[int64](registry, cacheDomainUnreadDM, 10_000)
	}
	return h
}

func (h *Handler) SetBlockChecker(b blockChecker) { h.blocks = b }
func (h *Handler) SetNotifier(n notifier)         { h.notify = n }
func (h *Handler) SetUserExister(u userExister)   { h.users = u }

// SetActivitiesWriter installs the federation activity-ledger
// writer + baseURL resolver per ADR 0044. Mirrors the equivalent
// setters on posts.Handler + social.Handler.
func (h *Handler) SetActivitiesWriter(w *activities.Writer, baseURLFn func(ctx context.Context) string) {
	h.activities = w
	h.baseURLFn = baseURLFn
}

func unreadKey(ref int64) string { return strconv.FormatInt(ref, 10) }

// invalidateUnread drops the recipient's cached count + NOTIFY
// broadcasts to federated peers. Best-effort logging.
func (h *Handler) invalidateUnread(ctx context.Context, ref int64) {
	if h.unreadCount == nil {
		return
	}
	if err := h.unreadCount.Invalidate(ctx, unreadKey(ref)); err != nil && h.Logger != nil {
		h.Logger.Warn("messages.cache.invalidate.error",
			slog.Int64("recipient", ref),
			slog.String("err", err.Error()),
		)
	}
}

// ListMyDirectMessageThreads — GET /account/messages.
func (h *Handler) ListMyDirectMessageThreads(
	ctx context.Context,
	req openapi.ListMyDirectMessageThreadsRequestObject,
) (openapi.ListMyDirectMessageThreadsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListMyDirectMessageThreads401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
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
	rows, err := New(h.Pool).ListMyThreads(ctx, ListMyThreadsParams{
		RecipientUserRef: id.UserRef,
		Limit:            int32(limit),
	})
	if err != nil {
		return nil, err
	}
	threads := make([]openapi.DirectMessageThread, 0, len(rows))
	for _, r := range rows {
		threads = append(threads, openapi.DirectMessageThread{
			PeerUserRef:       r.PeerUserRef,
			PeerUsername:      derefString(r.PeerUsername),
			PeerDisplayName:   r.PeerDisplayName,
			PeerAvatarUrl:     r.PeerAvatarUrl,
			LastMessageId:     uuid.UUID(r.LastMessageID.Bytes),
			LastSenderUserRef: r.LastSenderUserRef,
			LastBody:          r.LastBody,
			LastSentAt:        r.LastSentAt.Time,
			LastReadAt:        timePtr(r.LastReadAt),
			UnreadCount:       r.UnreadCount,
		})
	}
	return openapi.ListMyDirectMessageThreads200JSONResponse(openapi.DirectMessageThreadList{
		Threads: threads,
	}), nil
}

// GetMyUnreadDirectMessageCount — GET /account/messages/unread-count.
// Hot read; cache-backed so the envelope pill on every page render
// doesn't slam the DB.
func (h *Handler) GetMyUnreadDirectMessageCount(
	ctx context.Context,
	_ openapi.GetMyUnreadDirectMessageCountRequestObject,
) (openapi.GetMyUnreadDirectMessageCountResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetMyUnreadDirectMessageCount401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	count, err := h.unreadCountFor(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	return openapi.GetMyUnreadDirectMessageCount200JSONResponse(openapi.DirectMessageUnreadCount{
		Count: count,
	}), nil
}

// unreadCountFor returns the cached value when warm, falls back to
// the DB on miss + populates. Exposed in case future cross-package
// consumers want the count without an HTTP round-trip.
func (h *Handler) unreadCountFor(ctx context.Context, ref int64) (int64, error) {
	if h.unreadCount != nil {
		if hit, ok := h.unreadCount.Get(unreadKey(ref)); ok {
			return hit, nil
		}
	}
	count, err := New(h.Pool).CountMyUnreadDirectMessages(ctx, ref)
	if err != nil {
		return 0, err
	}
	if h.unreadCount != nil {
		h.unreadCount.Add(unreadKey(ref), count)
	}
	return count, nil
}

// ListDirectMessageThread — GET /account/messages/{peer_ref}.
func (h *Handler) ListDirectMessageThread(
	ctx context.Context,
	req openapi.ListDirectMessageThreadRequestObject,
) (openapi.ListDirectMessageThreadResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListDirectMessageThread401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.PeerRef == id.UserRef {
		// Self-thread query is nonsensical (the CHECK constraint
		// on direct_messages would forbid the rows anyway). Return
		// empty rather than 400 — the inbox doesn't render a
		// self-card so the only way to reach this is hand-crafted.
		return openapi.ListDirectMessageThread200JSONResponse(openapi.DirectMessageList{}), nil
	}
	if err := h.assertPeerExists(ctx, req.PeerRef); err != nil {
		if errors.Is(err, errUserNotFound) {
			return openapi.ListDirectMessageThread404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, err
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
	var cursorTs pgtype.Timestamptz
	var cursorID pgtype.UUID
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, idv, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListDirectMessageThread401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: idv, Valid: true}
	}
	fetch := limit + 1
	rows, err := New(h.Pool).ListThreadWithPeer(ctx, ListThreadWithPeerParams{
		SenderUserRef:    id.UserRef,
		RecipientUserRef: req.PeerRef,
		CursorSentAt:     cursorTs,
		CursorID:         cursorID,
		RowLimit:         fetch,
	})
	if err != nil {
		return nil, err
	}
	items := make([]openapi.DirectMessage, 0, limit)
	var lastTime time.Time
	var lastID uuid.UUID
	for i, r := range rows {
		if int32(i) >= limit {
			break
		}
		items = append(items, dmRowToAPI(r))
		lastTime = r.SentAt.Time
		lastID = uuid.UUID(r.ID.Bytes)
	}
	var nextCursor *string
	if int32(len(rows)) > limit {
		c := encodeCursor(lastTime, lastID)
		nextCursor = &c
	}
	return openapi.ListDirectMessageThread200JSONResponse(openapi.DirectMessageList{
		Items:      items,
		NextCursor: nextCursor,
	}), nil
}

// SendDirectMessage — POST /account/messages/{peer_ref}.
//
// Two permission gates: self-DM rejection + block-edge rejection.
// The notification writer downstream re-runs the block check; this
// gate exists here so we don't write a DM row the recipient is
// just going to be unaware of.
func (h *Handler) SendDirectMessage(
	ctx context.Context,
	req openapi.SendDirectMessageRequestObject,
) (openapi.SendDirectMessageResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SendDirectMessage401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SendDirectMessage400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}
	body := strings.TrimSpace(req.Body.Body)
	if body == "" {
		return openapi.SendDirectMessage400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "message body is empty"},
		}, nil
	}
	if req.PeerRef == id.UserRef {
		return openapi.SendDirectMessage400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "cannot DM yourself"},
		}, nil
	}
	if err := h.assertPeerExists(ctx, req.PeerRef); err != nil {
		if errors.Is(err, errUserNotFound) {
			return openapi.SendDirectMessage404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, err
	}
	if h.blocks != nil {
		blocked, err := h.blocks.HasBlockBetween(ctx, id.UserRef, req.PeerRef)
		if err != nil {
			return nil, err
		}
		if blocked {
			return openapi.SendDirectMessage403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{
					Error: "message blocked by an active block edge between the two users",
				},
			}, nil
		}
	}
	// Gold-standard path (1.22.B-cleanup: legacy fallback removed).
	// WithEmissionFn wraps InsertDirectMessage + Create(Note)
	// activity + direct_message_received notification in one
	// transactional unit. Cache invalidation fires post-commit.
	recipientRef := emit.UserRef{
		UserRef: req.PeerRef,
		URI:     h.actorURIForUserRef(ctx, req.PeerRef),
	}
	var saved DirectMessage
	err := h.activities.WithEmissionFn(ctx, func(tx pgx.Tx) (activities.EmissionInput, error) {
		r, err := New(tx).InsertDirectMessage(ctx, InsertDirectMessageParams{
			SenderUserRef:    id.UserRef,
			RecipientUserRef: req.PeerRef,
			Body:             body,
		})
		if err != nil {
			return activities.EmissionInput{}, err
		}
		saved = r
		em := emit.DirectMessage(
			h.senderContext(ctx, id),
			recipientRef,
			uuid.UUID(r.ID.Bytes).String(),
			body,
		)
		return activities.EmissionInput{
			Activity:      em.Activity,
			Notifications: convertNotifications(em.Notifications),
		}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("messages: send dm: %w", err)
	}
	h.invalidateUnread(ctx, req.PeerRef)
	return openapi.SendDirectMessage201JSONResponse(dmRowToAPI(saved)), nil
}

// senderContext builds an emit.ActorContext for the authenticated
// sender. Same shape as social.actorContext but defined here to
// keep the messages package self-contained.
func (h *Handler) senderContext(ctx context.Context, caller *auth.Identity) emit.ActorContext {
	if h.baseURLFn == nil {
		return emit.ActorContext{UserRef: caller.UserRef, Username: caller.Username}
	}
	return emit.ActorContext{
		UserRef:  caller.UserRef,
		Username: caller.Username,
		BaseURL:  h.baseURLFn(ctx),
	}
}

// actorURIForUserRef resolves a target user's federation actor
// URI via the cached username resolver on h.activities. Same
// shape as social.actorURIForUserRef.
func (h *Handler) actorURIForUserRef(ctx context.Context, userRef int64) string {
	if h.baseURLFn == nil || h.activities == nil {
		return ""
	}
	base := h.baseURLFn(ctx)
	if base == "" {
		return ""
	}
	username := h.activities.ResolveUsername(ctx, userRef)
	if username == "" {
		return ""
	}
	return base + "/users/" + username
}

// convertNotifications adapts the emit subpackage's
// NotificationFanout slice into the activities.NotificationInput
// slice the dispatch helper consumes.
func convertNotifications(ns []emit.NotificationFanout) []activities.NotificationInput {
	if len(ns) == 0 {
		return nil
	}
	out := make([]activities.NotificationInput, len(ns))
	for i, n := range ns {
		out[i] = activities.NotificationInput{
			Recipient:  n.Recipient,
			Verb:       n.Verb,
			TargetKind: n.TargetKind,
			TargetID:   n.TargetID,
			Payload:    n.Payload,
		}
	}
	return out
}

// MarkDirectMessageThreadRead — POST /account/messages/{peer_ref}/read.
func (h *Handler) MarkDirectMessageThreadRead(
	ctx context.Context,
	req openapi.MarkDirectMessageThreadReadRequestObject,
) (openapi.MarkDirectMessageThreadReadResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.MarkDirectMessageThreadRead401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	count, err := New(h.Pool).MarkThreadRead(ctx, MarkThreadReadParams{
		RecipientUserRef: id.UserRef,
		SenderUserRef:    req.PeerRef,
	})
	if err != nil {
		return nil, err
	}
	if count > 0 {
		h.invalidateUnread(ctx, id.UserRef)
	}
	return openapi.MarkDirectMessageThreadRead200JSONResponse(openapi.DirectMessageMarkReadResult{
		MarkedCount: count,
	}), nil
}

// --- helpers ---------------------------------------------------------------

var errUserNotFound = errors.New("messages: user not found")

func (h *Handler) assertPeerExists(ctx context.Context, ref int64) error {
	if h.users == nil {
		// No exister wired (test path) — accept the peer.
		return nil
	}
	exists, err := h.users.UserExists(ctx, ref)
	if err != nil {
		return err
	}
	if !exists {
		return errUserNotFound
	}
	return nil
}

// dmRowToAPI converts a sqlc-generated DirectMessage row to the
// openapi wire shape. Used by both list + insert paths (sqlc
// generates the same struct from both queries since they return
// the same column set).
func dmRowToAPI(r DirectMessage) openapi.DirectMessage {
	out := openapi.DirectMessage{
		Id:               uuid.UUID(r.ID.Bytes),
		SenderUserRef:    r.SenderUserRef,
		RecipientUserRef: r.RecipientUserRef,
		Body:             r.Body,
		SentAt:           r.SentAt.Time,
	}
	if r.ReadAt.Valid {
		t := r.ReadAt.Time
		out.ReadAt = &t
	}
	return out
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// --- cursor (shared with notifications-style cursor shape) -----------------

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
		return time.Time{}, uuid.Nil, errors.New("messages: bad cursor")
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

// pgx errnorows is referenced by the assertPeerExists path that some
// future caller may swap in.
var _ = pgx.ErrNoRows
