package search

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// EncodeCursor turns a *Cursor into the opaque base64-JSON string
// the /search endpoint returns as next_cursor. Returns "" for nil
// (no next page). Never returns an error — cursor payloads are
// tiny fixed-shape structs that JSON always accepts.
func EncodeCursor(c *Cursor) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor is the inverse of EncodeCursor. Empty input returns
// (nil, nil) — the "first page" case. Malformed input returns
// ErrBadCursor so the handler surfaces a 400 rather than
// pretending the request was for the first page.
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
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%w: json unmarshal: %v", ErrBadCursor, err)
	}
	if _, ok := ParseHitType(string(c.LastType)); !ok {
		return nil, fmt.Errorf("%w: unknown last_type %q", ErrBadCursor, c.LastType)
	}
	return &c, nil
}

// ErrBadCursor is the sentinel returned by DecodeCursor when the
// input can't be parsed. Callers map to HTTP 400.
var ErrBadCursor = errors.New("search: malformed cursor")
