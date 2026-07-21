// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package audit

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// ExportFormat is csv or ndjson.
type ExportFormat string

const (
	FormatCSV    ExportFormat = "csv"
	FormatNDJSON ExportFormat = "ndjson"
)

// exportColumns is the canonical column set + order. A caller's subset
// is filtered against this, preserving the caller's order.
var exportColumns = []string{
	"id", "event_type", "occurred_at", "actor_user_ref",
	"subject_user_ref", "ip", "user_agent", "metadata", "legal_hold",
}

func knownColumn(c string) bool {
	for _, k := range exportColumns {
		if k == c {
			return true
		}
	}
	return false
}

// SelectColumns resolves a comma-separated subset to a validated,
// ordered column list. Empty / all-unknown falls back to every column.
func SelectColumns(spec string) []string {
	if spec == "" {
		return append([]string(nil), exportColumns...)
	}
	var out []string
	start := 0
	for i := 0; i <= len(spec); i++ {
		if i == len(spec) || spec[i] == ',' {
			name := trimSpace(spec[start:i])
			if knownColumn(name) {
				out = append(out, name)
			}
			start = i + 1
		}
	}
	if len(out) == 0 {
		return append([]string(nil), exportColumns...)
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// ExportCursor is the keyset position between pages.
type ExportCursor struct {
	At time.Time
	ID uuid.UUID
	// set is false before the first page.
	set bool
}

// PageFetcher pulls one keyset page after the cursor, up to limit rows.
// Injected so the streamer can be tested with a synthetic source and
// the real handler can back it with the DB query.
type PageFetcher func(ctx context.Context, cur ExportCursor, limit int) ([]ExportAuditEventsPageRow, error)

// ExportPageSize is one keyset page. The streamer holds at most this
// many rows in memory at once — the property that makes a 10M-row
// export run in bounded memory.
const ExportPageSize = 1000

// StreamExport returns a reader that lazily paginates the audit log and
// formats it as CSV or NDJSON.
//
// The whole point is that it does NOT buffer: an io.Pipe connects a
// producer goroutine to the returned reader, and PipeWriter.Write
// blocks until the consumer (the HTTP response's io.Copy) drains it, so
// only one page is ever resident. A naive SELECT-all-into-slice would
// fail the 10M-row requirement; this cannot, because it never holds
// more than ExportPageSize rows.
//
// includeIP is the #425 redaction decision: false blanks the ip column
// even when the caller selected it, so the export never leaks what the
// list view withholds.
func StreamExport(ctx context.Context, format ExportFormat, columns []string, includeIP bool, fetch PageFetcher) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := writeExport(ctx, pw, format, columns, includeIP, fetch)
		// CloseWithError(nil) == Close; a mid-stream error propagates to
		// the reader so a truncated export surfaces rather than looking
		// complete.
		_ = pw.CloseWithError(err)
	}()
	return pr
}

func writeExport(ctx context.Context, w io.Writer, format ExportFormat, columns []string, includeIP bool, fetch PageFetcher) error {
	if format == FormatNDJSON {
		return writeNDJSON(ctx, w, columns, includeIP, fetch)
	}
	return writeCSV(ctx, w, columns, includeIP, fetch)
}

func writeCSV(ctx context.Context, w io.Writer, columns []string, includeIP bool, fetch PageFetcher) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(columns); err != nil { // header row
		return err
	}
	err := eachRow(ctx, fetch, func(r ExportAuditEventsPageRow) error {
		cells := make([]string, len(columns))
		for i, col := range columns {
			cells[i] = csvCell(r, col, includeIP)
		}
		return cw.Write(cells)
	})
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func writeNDJSON(ctx context.Context, w io.Writer, columns []string, includeIP bool, fetch PageFetcher) error {
	enc := json.NewEncoder(w)
	return eachRow(ctx, fetch, func(r ExportAuditEventsPageRow) error {
		obj := make(map[string]any, len(columns))
		for _, col := range columns {
			obj[col] = jsonCell(r, col, includeIP)
		}
		return enc.Encode(obj) // Encode appends a newline → NDJSON
	})
}

// eachRow drives the keyset cursor forward one page at a time, calling
// fn per row. The short-page check is the termination condition.
func eachRow(ctx context.Context, fetch PageFetcher, fn func(ExportAuditEventsPageRow) error) error {
	var cur ExportCursor
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := fetch(ctx, cur, ExportPageSize)
		if err != nil {
			return err
		}
		for _, r := range page {
			if err := fn(r); err != nil {
				return err
			}
		}
		if len(page) < ExportPageSize {
			return nil
		}
		last := page[len(page)-1]
		cur = ExportCursor{At: last.OccurredAt.Time, ID: uuid.UUID(last.ID.Bytes), set: true}
	}
}

// actorCell renders the actor column, honouring DSAR tombstoning: a
// row whose numeric ref was cleared but carries an actor_tombstone
// pseudonym reads as `deleted-user-{id}` (ADR 0024).
func actorCell(r ExportAuditEventsPageRow) string {
	if r.ActorUserRef != nil {
		return strconv.FormatInt(*r.ActorUserRef, 10)
	}
	if len(r.Metadata) > 0 {
		var m map[string]any
		if json.Unmarshal(r.Metadata, &m) == nil {
			if v, ok := m["actor_tombstone"].(string); ok {
				return v
			}
		}
	}
	return ""
}

func csvCell(r ExportAuditEventsPageRow, col string, includeIP bool) string {
	switch col {
	case "id":
		return uuid.UUID(r.ID.Bytes).String()
	case "event_type":
		return r.EventType
	case "occurred_at":
		return r.OccurredAt.Time.UTC().Format(time.RFC3339Nano)
	case "actor_user_ref":
		return actorCell(r)
	case "subject_user_ref":
		if r.SubjectUserRef != nil {
			return strconv.FormatInt(*r.SubjectUserRef, 10)
		}
		return ""
	case "ip":
		if includeIP && r.Ip != nil {
			return r.Ip.String()
		}
		return ""
	case "user_agent":
		if r.UserAgent != nil {
			return *r.UserAgent
		}
		return ""
	case "metadata":
		if len(r.Metadata) > 0 {
			return string(r.Metadata)
		}
		return ""
	case "legal_hold":
		return strconv.FormatBool(r.LegalHold)
	}
	return ""
}

func jsonCell(r ExportAuditEventsPageRow, col string, includeIP bool) any {
	switch col {
	case "id":
		return uuid.UUID(r.ID.Bytes).String()
	case "event_type":
		return r.EventType
	case "occurred_at":
		return r.OccurredAt.Time.UTC().Format(time.RFC3339Nano)
	case "actor_user_ref":
		if r.ActorUserRef != nil {
			return *r.ActorUserRef
		}
		if a := actorCell(r); a != "" {
			return a // tombstone pseudonym
		}
		return nil
	case "subject_user_ref":
		if r.SubjectUserRef != nil {
			return *r.SubjectUserRef
		}
		return nil
	case "ip":
		if includeIP && r.Ip != nil {
			return r.Ip.String()
		}
		return nil
	case "user_agent":
		if r.UserAgent != nil {
			return *r.UserAgent
		}
		return nil
	case "metadata":
		if len(r.Metadata) > 0 {
			return json.RawMessage(r.Metadata)
		}
		return nil
	case "legal_hold":
		return r.LegalHold
	}
	return nil
}
