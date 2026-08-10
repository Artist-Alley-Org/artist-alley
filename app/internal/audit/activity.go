// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// GET /account/activity — the caller's own slice of the audit log
// (#600).
//
// WHY IT LIVES IN THIS PACKAGE AND NOT IN ONE OF ITS OWN. `trash` is a
// package because its projection spans assets, posts and collections
// and belongs to none of them; importing all three to select five
// columns would have been the wrong dependency. Here the opposite holds
// — audit_events has exactly one owner, and this package is it. A
// separate package would mean a second module writing raw SQL against a
// table it does not own, next to the sqlc queries that do.
//
// WHY A SEPARATE HANDLER STRUCT INSIDE IT. HTTPHandler's doc comment
// says "admin-facing read endpoints", and every method on it opens by
// checking system.audit.read. This endpoint is neither: it is
// account-facing, it gates on nothing but being signed in, and it
// returns a deliberately smaller projection. Hanging it off the same
// struct would put a method with no capability check among five that
// have one — an invitation to read the pattern and skip the gate on the
// next admin method added. Two structs, two rules, no ambiguity.
//
// The projection's reasoning lives in the openapi description, where a
// client author will read it. What follows here is the enforcement.

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Page-size bounds for /account/activity. They match the openapi
// schema's minimum/maximum/default and /account/trash's, so a client
// that knows one account listing knows this one.
const (
	defaultActivityLimit = 50
	maxActivityLimit     = 200
)

// AccountHandler serves the caller-scoped activity listing.
type AccountHandler struct {
	queries *Queries
	logger  *slog.Logger
}

// NewAccountHandler builds the account-facing read handler. Separate
// constructor from NewHTTPHandler for the reason the file header gives.
func NewAccountHandler(pool *pgxpool.Pool, logger *slog.Logger) *AccountHandler {
	return &AccountHandler{queries: New(pool), logger: logger}
}

// ListMyActivity implements GET /account/activity.
func (h *AccountHandler) ListMyActivity(
	ctx context.Context,
	req openapi.ListMyActivityRequestObject,
) (openapi.ListMyActivityResponseObject, error) {
	// Refused before the query, not filtered by it. Ref 0 is the
	// anonymous sentinel; it is not a principal, and `actor_user_ref =
	// 0` is a predicate that would happily match rows if any ever
	// carried that ref. The same guard, for the same reason, opens
	// trash.ListMyTrash.
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() || caller.UserRef == 0 {
		return openapi.ListMyActivity401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	limit := defaultActivityLimit
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > maxActivityLimit {
			limit = maxActivityLimit
		}
	}

	cursorAt, cursorID, err := decodeAuditCursor(req.Params.Cursor)
	if err != nil {
		// 400, unlike the admin viewer which silently falls back to the
		// newest page. That fallback suits a log reader whose tab may
		// have gone stale; here a cursor the server cannot read means
		// the page the user asked for is not the page they would get,
		// and quietly restarting the list looks like duplicated rows.
		return openapi.ListMyActivity400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid cursor"},
		}, nil
	}

	rows, err := h.queries.ListMyActivity(ctx, ListMyActivityParams{
		Caller:   caller.UserRef,
		CursorAt: cursorAt,
		CursorID: cursorID,
		// One extra row so "last page" and "exactly a full page" are
		// distinguishable without a second round-trip.
		Lim: int32(limit + 1),
	})
	if err != nil {
		h.logger.LogAttrs(ctx, slog.LevelError, "activity.list.error", slog.String("err", err.Error()))
		return nil, fmt.Errorf("audit: list activity: %w", err)
	}

	out := openapi.ActivityList{Items: make([]openapi.ActivityEvent, 0, limit)}
	var lastAt pgtype.Timestamptz
	var lastID pgtype.UUID
	for i, r := range rows {
		if i >= limit {
			break
		}
		out.Items = append(out.Items, toActivityEvent(r))
		lastAt, lastID = r.OccurredAt, r.ID
	}
	if len(rows) > limit {
		next := encodeAuditCursor(lastAt, lastID)
		out.NextCursor = &next
	}
	return openapi.ListMyActivity200JSONResponse(out), nil
}

// toActivityEvent maps one row to the wire shape.
//
// The metadata decision is made HERE as well as in the query, and the
// duplication is deliberate — the same discipline toOpenAPI states for
// the admin IP: a mapper that can emit a field is the right place to
// decide whether it should. The query already selects NULL for
// on_my_account rows, so this branch is unreachable today; it exists so
// that a future edit which changes the SELECT cannot turn a projection
// rule into a leak without also editing the function whose job is to
// enforce it.
//
// Absent, not empty. `Metadata` is a pointer so an on_my_account row
// omits the key entirely rather than shipping `{}` — an empty object
// would assert the event had no detail, when the truth is that its
// detail belongs to whoever acted. Same shape of promise the admin
// projection keeps for `ip`.
//
// Nothing here can emit an actor, a subject, an ip or a user agent: the
// row type does not carry them. That is the point of the query's column
// list — the enforcement is structural, not a filter someone has to
// remember to apply.
func toActivityEvent(r ListMyActivityRow) openapi.ActivityEvent {
	out := openapi.ActivityEvent{
		Id:         openapi_types.UUID(r.ID.Bytes),
		EventType:  r.EventType,
		OccurredAt: r.OccurredAt.Time,
		Role:       openapi.OnMyAccount,
	}
	if !r.ByMe {
		return out
	}
	out.Role = openapi.ByMe
	if len(r.Metadata) > 0 {
		meta := map[string]any{}
		// A payload we cannot parse is dropped rather than passed
		// through raw. The admin viewer can afford to render
		// "(unparseable metadata)" for debugging; an account page has
		// nothing to say about a blob it cannot read, and shipping the
		// bytes anyway would hand a client something no schema covers.
		if err := json.Unmarshal(r.Metadata, &meta); err == nil {
			out.Metadata = &meta
		}
	}
	return out
}
