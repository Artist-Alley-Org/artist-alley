// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// cursorWire is the JSON shape a [Cursor] crosses the wire in.
//
// # ⛔ THE TWO SHAPES ARE DECIDED HERE, NOT BY TAGS ON THE STRUCT
//
// A relevance cursor has been `{"s":<score>,"i":"<uuid>","t":"<type>"}`
// since ADR 0056 §1, `s` always present, and clients carry it back
// verbatim; #1173 sprint 25b adds a recent cursor beside it and must not
// move a byte of the old one. Pointers with omitempty give exactly that:
// [EncodeCursor] sets `s` on a relevance cursor (so a zero score is still
// written) and `o` plus `ts` on a recent cursor, and the field order
// below is the order encoding/json writes, so the relevance shape is
// byte-identical to what a `{s,i,t}` struct produced.
//
// `ts` is unix MICROSECONDS: the precision TIMESTAMPTZ stores, so the
// value bound back into the keyset is exactly the value the row carried,
// with no rounding on either side of the wire.
type cursorWire struct {
	Order   CursorOrder `json:"o,omitempty"`
	Score   *float64    `json:"s,omitempty"`
	Recency *int64      `json:"ts,omitempty"`
	ID      uuid.UUID   `json:"i"`
	Type    HitType     `json:"t"`
}

// EncodeCursor turns a *Cursor into the opaque base64-JSON string
// the /search endpoint returns as next_cursor. Returns "" for nil
// (no next page). Never returns an error — cursor payloads are
// tiny fixed-shape structs that JSON always accepts.
func EncodeCursor(c *Cursor) string {
	if c == nil {
		return ""
	}
	w := cursorWire{ID: c.LastID, Type: c.LastType}
	if c.Order == OrderRecent {
		w.Order = OrderRecent
		micros := c.LastRecency.UnixMicro()
		w.Recency = &micros
	} else {
		score := c.LastScore
		w.Score = &score
	}
	b, _ := json.Marshal(w)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor is the inverse of EncodeCursor. Empty input returns
// (nil, nil) — the "first page" case. Malformed input returns
// ErrBadCursor so the handler surfaces a 400 rather than
// pretending the request was for the first page.
//
// # What is validated here, and what is not (#1173, sprint 25b)
//
// STRUCTURE: base64, JSON, a known type, a known order discriminator, a
// recent cursor carrying its timestamp, and no timestamp on a cursor
// that claims no order. A cursor without a discriminator is a relevance
// cursor, which is what every cursor minted before 25b is.
//
// NOT the order's fit to the query: whether a recent cursor was handed
// to a relevance query (or the reverse) is only knowable once the
// `dsl=` and `filter=` parameters have been folded into the final
// selection, so that check runs after composition, at the HTTP edge and
// again inside [Engine.Run]. See [ErrCursorOrder].
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Accept the standard-alphabet form too so clients that
		// use base64.StdEncoding don't hit a spurious parse
		// error. This is a common paste-a-cursor-back scenario.
		if raw2, err2 := base64.StdEncoding.DecodeString(s); err2 == nil {
			raw = raw2
		} else {
			return nil, fmt.Errorf("%w: base64 decode: %v", ErrBadCursor, err)
		}
	}
	var w cursorWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: json unmarshal: %v", ErrBadCursor, err)
	}
	if _, ok := ParseHitType(string(w.Type)); !ok {
		return nil, fmt.Errorf("%w: unknown last_type %q", ErrBadCursor, w.Type)
	}
	c := &Cursor{Order: w.Order, LastID: w.ID, LastType: w.Type}
	switch w.Order {
	case OrderRelevance:
		if w.Recency != nil {
			return nil, fmt.Errorf("%w: a timestamp without an order", ErrBadCursor)
		}
		if w.Score != nil {
			c.LastScore = *w.Score
		}
	case OrderRecent:
		if w.Recency == nil {
			return nil, fmt.Errorf("%w: a recent cursor without its timestamp", ErrBadCursor)
		}
		c.LastRecency = time.UnixMicro(*w.Recency).UTC()
	default:
		return nil, fmt.Errorf("%w: unknown order %q", ErrBadCursor, w.Order)
	}
	return c, nil
}

// ErrBadCursor is the sentinel returned by DecodeCursor when the
// input can't be parsed. Callers map to HTTP 400.
var ErrBadCursor = errors.New("search: malformed cursor")

// ErrCursorOrder is returned when a cursor is positioned in one order
// and the query it accompanies runs in the other (#1173, sprint 25b): a
// recent cursor on a relevance query, or a relevance cursor on a
// `last:N` query. It wraps [ErrBadCursor], so every handler that maps
// that to `invalid_cursor` maps this the same way without learning a
// second sentinel.
//
// Checked after the final query is composed (the HTTP edge, once
// `dsl=` and `filter=` have folded) and again, fail-closed, at
// [Engine.Run]'s entry for programmatic callers.
var ErrCursorOrder = fmt.Errorf("%w: cursor order does not match the query's order", ErrBadCursor)

// checkCursorOrder is the one spelling of the order/query fit.
func checkCursorOrder(q Query) error {
	if q.Cursor == nil {
		return nil
	}
	_, recent := q.Filters.RecentWindow()
	if q.Cursor.Recent() != recent {
		return ErrCursorOrder
	}
	return nil
}
