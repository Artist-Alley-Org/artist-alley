// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Admin handlers for the outbox + inbox queue views per
// 1.22.D-c. Owned by the outbox package since they read from
// both federation_outbox + federation_inbox + emit the
// re-queue / cascade-cancel state changes.

package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// AdminHandler owns the /admin/federation/outbox + /inbox +
// re-queue + cascade-cancel surface.
type AdminHandler struct {
	pool      *pgxpool.Pool
	outboxQ   *Queries
	inboxQ    *inbox.Queries
	auditReq  AuditReqHook
	auditCasc AuditCascadeHook
	logger    *slog.Logger
}

// AuditReqHook is the cross-package contract for emitting
// federation.outbox.requeued. Boot wires it to
// audit.Recorder.OutboxRequeued. nil-safe (skipped).
type AuditReqHook func(ctx context.Context, req *http.Request, actorUserRef int64, outboxID, peerID, activityID, priorLastError string)

// AuditCascadeHook is the cross-package contract for emitting
// federation.peer.cascade_cancelled. Boot wires it to
// audit.Recorder.PeerCascadeCancelled. nil-safe.
type AuditCascadeHook func(ctx context.Context, req *http.Request, actorUserRef int64, peerID string, cancelledCount int)

// NewAdminHandler constructs the admin handler. inboxQ is the
// cross-package read-side handle for /admin/federation/inbox;
// the outbox package owns the admin surface for both queues to
// keep the federation-traffic mental model in one place.
func NewAdminHandler(
	pool *pgxpool.Pool,
	inboxQ *inbox.Queries,
	auditReq AuditReqHook,
	auditCasc AuditCascadeHook,
	logger *slog.Logger,
) *AdminHandler {
	return &AdminHandler{
		pool:      pool,
		outboxQ:   New(pool),
		inboxQ:    inboxQ,
		auditReq:  auditReq,
		auditCasc: auditCasc,
		logger:    logger,
	}
}

// --- list endpoints ----------------------------------------------------

// AdminOutboxRow is the projection the admin UI consumes —
// includes the joined activity_type for filter rendering.
type AdminOutboxRow struct {
	ID                  uuid.UUID
	ActivityID          uuid.UUID
	PeerID              uuid.UUID
	TargetUserURL       *string
	Status              string
	Attempts            int16
	NextAttemptAt       time.Time
	LastAttemptAt       *time.Time
	LastError           string
	SentAt              *time.Time
	DeliveredWithKeyID  *string
	ActivityType        string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// AdminListOutboxFilter mirrors the spec's query-param shape.
type AdminListOutboxFilter struct {
	PeerID            *uuid.UUID
	Status            *string
	ActivityType      *string
	Since             *time.Time
	Limit             int32 // capped 1-500; default 100
	CursorCreatedAt   *time.Time
	CursorID          *uuid.UUID
}

// ListOutboxForAdmin returns the filtered + paginated list +
// the next cursor (empty when there's no further page).
func (h *AdminHandler) ListOutboxForAdmin(ctx context.Context, f AdminListOutboxFilter) ([]AdminOutboxRow, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	params := ListOutboxForAdminParams{LimitN: limit}
	if f.PeerID != nil {
		params.PeerID = pgtype.UUID{Bytes: *f.PeerID, Valid: true}
	}
	if f.Status != nil {
		params.Status = f.Status
	}
	if f.ActivityType != nil {
		params.ActivityType = f.ActivityType
	}
	if f.Since != nil {
		params.Since = pgtype.Timestamptz{Time: *f.Since, Valid: true}
	}
	if f.CursorCreatedAt != nil && f.CursorID != nil {
		params.CursorCreatedAt = pgtype.Timestamptz{Time: *f.CursorCreatedAt, Valid: true}
		params.CursorID = pgtype.UUID{Bytes: *f.CursorID, Valid: true}
	}
	rows, err := h.outboxQ.ListOutboxForAdmin(ctx, params)
	if err != nil {
		return nil, "", err
	}
	out := make([]AdminOutboxRow, 0, len(rows))
	for _, r := range rows {
		row := AdminOutboxRow{
			ID:                  uuid.UUID(r.ID.Bytes),
			ActivityID:          uuid.UUID(r.ActivityID.Bytes),
			PeerID:              uuid.UUID(r.PeerID.Bytes),
			TargetUserURL:       r.TargetUserUrl,
			Status:              r.Status,
			Attempts:            r.Attempts,
			NextAttemptAt:       r.NextAttemptAt.Time,
			LastError:           r.LastError,
			ActivityType:        r.ActivityType,
			CreatedAt:           r.CreatedAt.Time,
			UpdatedAt:           r.UpdatedAt.Time,
			DeliveredWithKeyID:  r.DeliveredWithKeyID,
		}
		if r.LastAttemptAt.Valid {
			t := r.LastAttemptAt.Time
			row.LastAttemptAt = &t
		}
		if r.SentAt.Valid {
			t := r.SentAt.Time
			row.SentAt = &t
		}
		out = append(out, row)
	}
	nextCursor := ""
	if int32(len(out)) == limit && len(out) > 0 {
		last := out[len(out)-1]
		nextCursor = encodeOutboxCursor(last.CreatedAt, last.ID)
	}
	return out, nextCursor, nil
}

// AdminInboxRow mirrors the openapi.FederationInboxRow shape.
type AdminInboxRow struct {
	ID                      uuid.UUID
	ActivityURI             string
	PeerID                  uuid.UUID
	ActorURI                string
	ActivityType            string
	ObjectKind              *string
	ObjectID                *uuid.UUID
	HTTPSigKey              string
	Status                  string
	RejectReason            *string
	DispatchAttempts        int32
	LastAttemptAt           *time.Time
	LastError               *string
	ReceivedAt              time.Time
	ProcessedAt             *time.Time
	CorrelationActivityID   *uuid.UUID
}

type AdminListInboxFilter struct {
	PeerID             *uuid.UUID
	Status             *string
	ActivityType       *string
	Since              *time.Time
	Limit              int32
	CursorReceivedAt   *time.Time
	CursorID           *uuid.UUID
}

func (h *AdminHandler) ListInboxForAdmin(ctx context.Context, f AdminListInboxFilter) ([]AdminInboxRow, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	params := inbox.ListInboxForAdminParams{LimitN: limit}
	if f.PeerID != nil {
		params.PeerID = pgtype.UUID{Bytes: *f.PeerID, Valid: true}
	}
	if f.Status != nil {
		params.Status = f.Status
	}
	if f.ActivityType != nil {
		params.ActivityType = f.ActivityType
	}
	if f.Since != nil {
		params.Since = pgtype.Timestamptz{Time: *f.Since, Valid: true}
	}
	if f.CursorReceivedAt != nil && f.CursorID != nil {
		params.CursorReceivedAt = pgtype.Timestamptz{Time: *f.CursorReceivedAt, Valid: true}
		params.CursorID = pgtype.UUID{Bytes: *f.CursorID, Valid: true}
	}
	rows, err := h.inboxQ.ListInboxForAdmin(ctx, params)
	if err != nil {
		return nil, "", err
	}
	out := make([]AdminInboxRow, 0, len(rows))
	for _, r := range rows {
		row := AdminInboxRow{
			ID:               uuid.UUID(r.ID.Bytes),
			ActivityURI:      r.ActivityUri,
			PeerID:           uuid.UUID(r.PeerID.Bytes),
			ActorURI:         r.ActorUri,
			ActivityType:     r.ActivityType,
			ObjectKind:       r.ObjectKind,
			HTTPSigKey:       r.HttpSigKey,
			Status:           r.Status,
			RejectReason:     r.RejectReason,
			DispatchAttempts: r.DispatchAttempts,
			LastError:        nullableStr(r.LastError),
			ReceivedAt:       r.ReceivedAt.Time,
		}
		if r.ObjectID.Valid {
			oid := uuid.UUID(r.ObjectID.Bytes)
			row.ObjectID = &oid
		}
		if r.LastAttemptAt.Valid {
			t := r.LastAttemptAt.Time
			row.LastAttemptAt = &t
		}
		if r.ProcessedAt.Valid {
			t := r.ProcessedAt.Time
			row.ProcessedAt = &t
		}
		if r.CorrelationActivityID.Valid {
			cid := uuid.UUID(r.CorrelationActivityID.Bytes)
			row.CorrelationActivityID = &cid
		}
		out = append(out, row)
	}
	nextCursor := ""
	if int32(len(out)) == limit && len(out) > 0 {
		last := out[len(out)-1]
		nextCursor = encodeInboxCursor(last.ReceivedAt, last.ID)
	}
	return out, nextCursor, nil
}

// --- mutating endpoints -----------------------------------------------

// ErrOutboxNotFound is returned when re-queue targets an unknown row.
var ErrOutboxNotFound = errors.New("admin: outbox row not found")

// ErrOutboxNotFailed is returned when re-queue targets a row that
// isn't in status=failed — the idempotency guard refuses re-queue
// of currently-delivering / sent / cancelled rows.
var ErrOutboxNotFailed = errors.New("admin: outbox row is not in status=failed")

// RequeueOutbox flips a failed row to queued + attempts=0 +
// fires the federation.outbox.requeued audit. Returns the
// updated row.
//
// Idempotency: refuses if the row isn't in status=failed. This
// prevents:
//   - Re-queueing a queued row → no-op race; delivery worker
//     might already be processing it; flipping attempts=0
//     would lose the partial-attempt history.
//   - Re-queueing a sent row → duplicate-deliver to the peer.
//   - Re-queueing a cancelled row → operator wanted that cancel
//     to stick; resurrecting via re-queue is suspicious.
//
// The admin UI's confirmation prompt SHOULD warn before
// re-queueing rows whose last_error references a non-retriable
// §12.1 reason — but that decision is UI-only; the backend
// proceeds either way once the operator confirms.
func (h *AdminHandler) RequeueOutbox(ctx context.Context, req *http.Request, actorUserRef int64, outboxID uuid.UUID) (AdminOutboxRow, error) {
	// Snapshot the current state for the audit (prior_last_error)
	// + the idempotency check.
	current, err := h.outboxQ.GetOutboxByID(ctx, pgtype.UUID{Bytes: outboxID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AdminOutboxRow{}, ErrOutboxNotFound
		}
		return AdminOutboxRow{}, err
	}
	if current.Status != "failed" {
		return AdminOutboxRow{}, ErrOutboxNotFailed
	}
	priorErr := current.LastError

	// Flip via the existing RequeueFailedOutbox query (1.22.D-b-1).
	rows, err := h.outboxQ.RequeueFailedOutbox(ctx, current.ID)
	if err != nil {
		return AdminOutboxRow{}, err
	}
	if rows == 0 {
		// Race: another admin re-queued between our snapshot and
		// the UPDATE. Idempotent — treat as success but return
		// the current state.
		if h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.admin.requeue.race",
				slog.String("outbox_id", outboxID.String()))
		}
	}

	// Re-read for the response. After-the-fact audit per the
	// pool-bound pattern — the audit row records the operator
	// decision; it doesn't need to be tx-atomic with the
	// re-queue (the re-queue is its own atomic UPDATE).
	updated, err := h.outboxQ.GetOutboxByID(ctx, current.ID)
	if err != nil {
		return AdminOutboxRow{}, err
	}
	if h.auditReq != nil {
		h.auditReq(ctx, req, actorUserRef,
			outboxID.String(),
			uuid.UUID(updated.PeerID.Bytes).String(),
			uuid.UUID(updated.ActivityID.Bytes).String(),
			priorErr,
		)
	}
	return adminOutboxFromQuery(updated, ""), nil
}

// CancelPendingForPeer marks every queued row for the peer as
// cancelled in ONE transaction + fires a SINGLE
// federation.peer.cascade_cancelled audit per the
// 1.22.D-c §7.4 single-audit invariant.
//
// Returns the count of cancelled rows.
func (h *AdminHandler) CancelPendingForPeer(ctx context.Context, req *http.Request, actorUserRef int64, peerID uuid.UUID) (int, error) {
	cancelled, err := h.outboxQ.CancelOutboxByPeer(ctx, pgtype.UUID{Bytes: peerID, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("cancel by peer: %w", err)
	}
	if h.auditCasc != nil {
		h.auditCasc(ctx, req, actorUserRef, peerID.String(), int(cancelled))
	}
	if h.logger != nil {
		h.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.admin.cascade_cancel",
			slog.String("peer_id", peerID.String()),
			slog.Int64("cancelled", cancelled),
			slog.Int64("actor_user_ref", actorUserRef),
		)
	}
	return int(cancelled), nil
}

// --- helpers ----------------------------------------------------------

func adminOutboxFromQuery(r FederationOutbox, activityType string) AdminOutboxRow {
	row := AdminOutboxRow{
		ID:                 uuid.UUID(r.ID.Bytes),
		ActivityID:         uuid.UUID(r.ActivityID.Bytes),
		PeerID:             uuid.UUID(r.PeerID.Bytes),
		TargetUserURL:      r.TargetUserUrl,
		Status:             r.Status,
		Attempts:           r.Attempts,
		NextAttemptAt:      r.NextAttemptAt.Time,
		LastError:          r.LastError,
		ActivityType:       activityType,
		CreatedAt:          r.CreatedAt.Time,
		UpdatedAt:          r.UpdatedAt.Time,
		DeliveredWithKeyID: r.DeliveredWithKeyID,
	}
	if r.LastAttemptAt.Valid {
		t := r.LastAttemptAt.Time
		row.LastAttemptAt = &t
	}
	if r.SentAt.Valid {
		t := r.SentAt.Time
		row.SentAt = &t
	}
	return row
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Cursor encoding: base64url(timestamp.RFC3339Nano + "|" + id).
// Opaque to the client; deterministic across requests.
func encodeOutboxCursor(t time.Time, id uuid.UUID) string {
	return t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
}
func encodeInboxCursor(t time.Time, id uuid.UUID) string {
	return t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
}

// DecodeCursor parses the opaque cursor string back into
// (timestamp, id). Returns ok=false on malformed input — caller
// treats as "no cursor."
func DecodeCursor(s string) (time.Time, uuid.UUID, bool) {
	if s == "" {
		return time.Time{}, uuid.UUID{}, false
	}
	sep := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			sep = i
			break
		}
	}
	if sep < 0 {
		return time.Time{}, uuid.UUID{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s[:sep])
	if err != nil {
		return time.Time{}, uuid.UUID{}, false
	}
	id, err := uuid.Parse(s[sep+1:])
	if err != nil {
		return time.Time{}, uuid.UUID{}, false
	}
	return t, id, true
}
