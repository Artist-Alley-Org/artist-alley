// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// GET /fields/import/receipts[/{receipt_id}] (#1576 S4): read-only history
// ---------------------------------------------------------------------------
//
// Raw handlers, like preview and apply, so that authorization is decided
// before the query or the path is looked at: generated parameter binding
// would answer a malformed limit or id with 400 to anyone. Any
// fields.admin or system.admin operator reads every local receipt; that
// grants nothing on apply, whose preview stays actor-bound.
//
// Read-only: the list is one SELECT and the detail two (the receipt, then
// its items); no audit entry, no preview touched. Receipts and their items
// are never updated, so the detail needs no transaction.
//
// The cursor is an opaque position, not a credential: it is accepted only
// in the exact canonical spelling this server writes for a position, but a
// well-formed cursor naming any position is simply that position. It is
// not signed and proves nothing about who made it.

const (
	receiptPageDefault   = 25
	receiptPageMax       = 100
	receiptCursorMaxSize = 512
	receiptCursorPrefix  = "v1."
)

var pageLimitToken = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)

// historyGate answers 401 or 403 and reports false, before any input is
// read.
func historyGate(w http.ResponseWriter, r *http.Request) bool {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeImportJSON(w, http.StatusUnauthorized, openapi.Error{Error: "authentication required"})
		return false
	}
	if !canAdminFields(id) {
		writeImportJSON(w, http.StatusForbidden, openapi.Error{Error: "field admin capability required"})
		return false
	}
	return true
}

func encodeReceiptCursor(at time.Time, id uuid.UUID) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return receiptCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

var errBadCursor = errors.New("cursor is not a well-formed history cursor")

// decodeReceiptCursor accepts only the exact spelling encodeReceiptCursor
// writes: after decoding, the position is encoded again and must equal the
// cursor byte for byte. That refuses embedded newlines (which the base64
// decoder skips), padding, and alternative trailing bits.
func decodeReceiptCursor(c string) (time.Time, uuid.UUID, error) {
	if len(c) > receiptCursorMaxSize || !strings.HasPrefix(c, receiptCursorPrefix) {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(c, receiptCursorPrefix))
	if err != nil {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok || !canonicalUUID.MatchString(id) {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || t.UTC().Format(time.RFC3339Nano) != at {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	u := uuid.MustParse(id)
	if encodeReceiptCursor(t, u) != c {
		return time.Time{}, uuid.UUID{}, errBadCursor
	}
	return t, u, nil
}

func badHistoryRequest(w http.ResponseWriter, msg string) {
	writeImportJSON(w, http.StatusBadRequest, openapi.Error{Error: msg})
}

// ServeImportReceipts is GET /fields/import/receipts.
func (h *Handler) ServeImportReceipts(w http.ResponseWriter, r *http.Request) {
	if !historyGate(w, r) {
		return
	}
	limit := receiptPageDefault
	var after *struct {
		at time.Time
		id uuid.UUID
	}
	// ParseQuery, not URL.Query: Query drops whatever does not parse (a
	// bad escape, a semicolon separator) and would answer the first page.
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		badHistoryRequest(w, "the query string is malformed")
		return
	}
	for key, vals := range query {
		if len(vals) != 1 {
			badHistoryRequest(w, fmt.Sprintf("query parameter %q is given more than once", key))
			return
		}
		switch key {
		case "limit":
			if !pageLimitToken.MatchString(vals[0]) {
				badHistoryRequest(w, "limit must be an integer from 1 to 100")
				return
			}
			n, _ := strconv.Atoi(vals[0])
			if n > receiptPageMax {
				badHistoryRequest(w, "limit must be an integer from 1 to 100")
				return
			}
			limit = n
		case "cursor":
			at, id, err := decodeReceiptCursor(vals[0])
			if err != nil {
				badHistoryRequest(w, err.Error())
				return
			}
			after = &struct {
				at time.Time
				id uuid.UUID
			}{at, id}
		default:
			badHistoryRequest(w, fmt.Sprintf("unknown query parameter %q", key))
			return
		}
	}
	params := ListFieldSchemaImportReceiptsParams{Lim: int32(limit + 1)}
	if after != nil {
		params.AfterAt = pgtype.Timestamptz{Time: after.at, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: after.id, Valid: true}
	}
	rows, err := New(h.Pool).ListFieldSchemaImportReceipts(r.Context(), params)
	if err != nil {
		h.historyError(w, r, err)
		return
	}
	page := openapi.FieldSchemaImportReceiptPage{Items: make([]openapi.FieldSchemaImportReceiptSummary, 0, limit)}
	for i, row := range rows {
		if i == limit {
			last := rows[limit-1]
			c := encodeReceiptCursor(last.AppliedAt.Time, uuid.UUID(last.ID.Bytes))
			page.NextCursor = &c
			break
		}
		page.Items = append(page.Items, openapi.FieldSchemaImportReceiptSummary{
			ReceiptId:       openapi_types.UUID(uuid.UUID(row.ID.Bytes)),
			AppliedAt:       row.AppliedAt.Time.UTC(),
			ActorRef:        row.ActorRef,
			Format:          row.Format,
			DocumentDigest:  row.DocumentDigest,
			DecisionsDigest: row.DecisionsDigest,
			SourceLabel:     row.SourceLabel,
			SourceUri:       row.SourceUri,
			Changed:         int(row.Changed),
			FieldCount:      int(row.FieldCount),
		})
	}
	writeImportJSON(w, http.StatusOK, page)
}

// ServeImportReceipt is GET /fields/import/receipts/{receipt_id}. The
// router passes the raw path segment.
func (h *Handler) ServeImportReceipt(w http.ResponseWriter, r *http.Request, rawID string) {
	if !historyGate(w, r) {
		return
	}
	if len(r.URL.RawQuery) > 0 {
		badHistoryRequest(w, "this operation takes no query parameters")
		return
	}
	if !canonicalUUID.MatchString(rawID) {
		badHistoryRequest(w, "receipt_id must be a lower-case UUID")
		return
	}
	id := pgtype.UUID{Bytes: uuid.MustParse(rawID), Valid: true}
	q := New(h.Pool)
	rec, err := q.GetFieldSchemaImportReceipt(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeImportJSON(w, http.StatusNotFound, openapi.Error{Error: "no such receipt"})
		return
	}
	if err != nil {
		h.historyError(w, r, err)
		return
	}
	items, err := q.ListFieldSchemaImportReceiptItems(r.Context(), id)
	if err != nil {
		h.historyError(w, r, err)
		return
	}
	out := openapi.FieldSchemaImportReceiptRecord{
		ReceiptId:       openapi_types.UUID(uuid.UUID(rec.ID.Bytes)),
		PreviewId:       openapi_types.UUID(uuid.UUID(rec.PreviewID.Bytes)),
		AppliedAt:       rec.AppliedAt.Time.UTC(),
		ActorRef:        rec.ActorRef,
		Format:          rec.Format,
		DocumentDigest:  rec.DocumentDigest,
		DecisionsDigest: rec.DecisionsDigest,
		SourceLabel:     rec.SourceLabel,
		SourceUri:       rec.SourceUri,
		Changed:         int(rec.Changed),
		Fields:          make([]openapi.FieldSchemaImportReceiptRecordItem, 0, len(items)),
	}
	for _, it := range items {
		item := openapi.FieldSchemaImportReceiptRecordItem{
			Code: it.Code, Decision: it.Decision, Outcome: it.Outcome, IncomingFieldDigest: it.IncomingFieldDigest,
		}
		if it.FieldID.Valid {
			fid := openapi_types.UUID(uuid.UUID(it.FieldID.Bytes))
			item.FieldId = &fid
		}
		out.Fields = append(out.Fields, item)
	}
	writeImportJSON(w, http.StatusOK, out)
}

func (h *Handler) historyError(w http.ResponseWriter, r *http.Request, err error) {
	if h.Logger != nil {
		h.Logger.LogAttrs(r.Context(), slog.LevelError, "metadata.import_history.error", slog.String("err", err.Error()))
	}
	writeImportJSON(w, http.StatusInternalServerError, openapi.Error{Error: "internal error"})
}
