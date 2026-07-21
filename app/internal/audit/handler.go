// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Admin audit-log viewer (Phase 1.17.K).
//
// This file implements the read side of the audit subsystem — the
// write side is the Recorder in events.go. Writes are best-effort
// and hot-path; reads are admin-only and cold-path.
//
// HTTPHandler is wired into the apiServer in app/internal/http/api.go.
// Cap gate: system.audit.read (seeded by migration 00041).

package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// HTTPHandler implements the admin-facing read endpoints.
type HTTPHandler struct {
	queries *Queries
	logger  *slog.Logger
}

// NewHTTPHandler builds the read-side handler. Separate constructor
// from NewRecorder so the apiServer can wire each independently.
func NewHTTPHandler(pool *pgxpool.Pool, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{queries: New(pool), logger: logger}
}

// defaultListLimit and maxListLimit cap how many rows a single
// request can fetch. The viewer's "load more" button drives the
// cursor — there's never a reason to go above ~500 in one shot.
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// Capabilities for the audit surface.
//
// The split is the point (#425): capAuditRead admits a caller to the
// log, capAuditPIIRead additionally admits them to the personal data
// in it. An auditor role holds the first; a compliance or
// incident-response role holds both. system.admin satisfies either as
// a wildcard in Identity.Can, so it needs no grant.
const (
	capAuditRead    = "system.audit.read"
	capAuditPIIRead = "system.audit.pii.read"
)

// ListAdminAuditEvents — GET /admin/audit.
func (h *HTTPHandler) ListAdminAuditEvents(
	ctx context.Context,
	req openapi.ListAdminAuditEventsRequestObject,
) (openapi.ListAdminAuditEventsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListAdminAuditEvents401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAuditRead) {
		return openapi.ListAdminAuditEvents403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capAuditRead + " capability required"},
		}, nil
	}

	limit := defaultListLimit
	if req.Params.Limit != nil && *req.Params.Limit > 0 {
		limit = *req.Params.Limit
		if limit > maxListLimit {
			limit = maxListLimit
		}
	}

	cursorAt, cursorID, err := decodeAuditCursor(req.Params.Cursor)
	if err != nil {
		// Bad cursor — treat as "no cursor" rather than 400, so a
		// stale browser tab gracefully falls back to the newest page.
		cursorAt, cursorID = pgtype.Timestamptz{}, pgtype.UUID{}
	}

	params := ListAuditEventsParams{
		EventType:      strPtrOrNil(req.Params.EventType),
		ActorUserRef:   req.Params.ActorUserRef,
		SubjectUserRef: req.Params.SubjectUserRef,
		Since:          timeToTs(req.Params.Since),
		Until:          timeToTs(req.Params.Until),
		CursorAt:       cursorAt,
		CursorID:       cursorID,
		// Fetch one extra row so we know whether a next page exists
		// without an extra round-trip.
		Lim: int32(limit + 1),
	}

	rows, err := h.queries.ListAuditEvents(ctx, params)
	if err != nil {
		h.logger.LogAttrs(ctx, slog.LevelError, "audit.list.error", slog.String("err", err.Error()))
		return nil, fmt.Errorf("audit: list: %w", err)
	}

	var nextCursor *string
	if len(rows) > limit {
		last := rows[limit-1]
		c := encodeAuditCursor(last.OccurredAt, last.ID)
		nextCursor = &c
		rows = rows[:limit]
	}

	total, err := h.queries.CountAuditEvents(ctx, CountAuditEventsParams{
		EventType:      params.EventType,
		ActorUserRef:   params.ActorUserRef,
		SubjectUserRef: params.SubjectUserRef,
		Since:          params.Since,
		Until:          params.Until,
	})
	if err != nil {
		// Count failures shouldn't drop the page — log + return 0.
		h.logger.LogAttrs(ctx, slog.LevelWarn, "audit.count.error", slog.String("err", err.Error()))
		total = 0
	}

	out := openapi.AuditEventList{
		Items:      make([]openapi.AuditEvent, 0, len(rows)),
		Total:      total,
		NextCursor: nextCursor,
	}
	// #425 — actor IPs are personal data and need their own capability
	// on top of system.audit.read. Resolved once for the page rather
	// than per row: the answer cannot change mid-response, and
	// per-row evaluation would invite a future caller to vary it.
	includeIP := id.Can(capAuditPIIRead)
	for _, r := range rows {
		out.Items = append(out.Items, toOpenAPI(r, includeIP))
	}
	return openapi.ListAdminAuditEvents200JSONResponse(out), nil
}

// ListAdminAuditEventTypes — GET /admin/audit/event_types.
func (h *HTTPHandler) ListAdminAuditEventTypes(
	ctx context.Context,
	_ openapi.ListAdminAuditEventTypesRequestObject,
) (openapi.ListAdminAuditEventTypesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListAdminAuditEventTypes401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAuditRead) {
		return openapi.ListAdminAuditEventTypes403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capAuditRead + " capability required"},
		}, nil
	}
	items, err := h.queries.ListAuditEventTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit: list types: %w", err)
	}
	if items == nil {
		items = []string{}
	}
	return openapi.ListAdminAuditEventTypes200JSONResponse{Items: items}, nil
}

// toOpenAPI converts the sqlc row to the wire-shape AuditEvent.
//
// includeIP carries the system.audit.pii.read decision (#425). The
// redaction lives HERE, in the one function that knows how to build
// this wire shape, rather than in the handler after the fact — a
// mapper that can emit a field is the right place to decide whether it
// should. Any future caller has to answer the question to compile.
//
// The parameter's zero value is the SAFE one on purpose: a caller who
// gets this wrong omits the IP rather than leaking it.
func toOpenAPI(r AuditEvent, includeIP bool) openapi.AuditEvent {
	out := openapi.AuditEvent{
		Id:             openapi_types.UUID(r.ID.Bytes),
		EventType:      r.EventType,
		OccurredAt:     r.OccurredAt.Time,
		SubjectUserRef: r.SubjectUserRef,
		ActorUserRef:   r.ActorUserRef,
		UserAgent:      r.UserAgent,
		Metadata:       map[string]any{},
	}
	// Omitted, not blanked: the field is absent from the JSON entirely
	// for a caller without the capability, so there is no empty column
	// implying "no IP recorded" when one exists.
	if includeIP && r.Ip != nil {
		s := r.Ip.String()
		out.Ip = &s
	}
	if len(r.Metadata) > 0 {
		// Unmarshal the JSONB blob. Failures fall through to the
		// empty map — the viewer renders "(unparseable metadata)" in
		// that case, which is still useful for debugging.
		_ = json.Unmarshal(r.Metadata, &out.Metadata)
	}
	return out
}

// cursorPayload is the opaque shape we base64url-encode for the
// `cursor` query param. Keeping the shape internal lets us evolve it
// without bumping the API contract — clients treat it as opaque.
type cursorPayload struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func encodeAuditCursor(at pgtype.Timestamptz, id pgtype.UUID) string {
	p := cursorPayload{At: at.Time.UTC(), ID: uuid.UUID(id.Bytes).String()}
	raw, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAuditCursor(s *string) (pgtype.Timestamptz, pgtype.UUID, error) {
	if s == nil || *s == "" {
		return pgtype.Timestamptz{}, pgtype.UUID{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(*s))
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("cursor decode: %w", err)
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("cursor unmarshal: %w", err)
	}
	u, err := uuid.Parse(p.ID)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("cursor uuid: %w", err)
	}
	if p.At.IsZero() {
		return pgtype.Timestamptz{}, pgtype.UUID{}, errors.New("cursor missing timestamp")
	}
	return pgtype.Timestamptz{Time: p.At, Valid: true},
		pgtype.UUID{Bytes: u, Valid: true},
		nil
}

func timeToTs(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func strPtrOrNil(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	v := *s
	return &v
}
