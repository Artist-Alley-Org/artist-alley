// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCapRequestBody(t *testing.T) {
	const limit = 16
	var seen []byte
	reached := false
	h := CapRequestBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, c := range []struct {
		name          string
		body          string
		contentLength int64 // -1 = unknown (chunked)
		want          int
	}{
		{"at the limit", strings.Repeat("a", limit), limit, http.StatusNoContent},
		{"one over", strings.Repeat("a", limit+1), limit + 1, http.StatusRequestEntityTooLarge},
		{"one over, length unknown", strings.Repeat("a", limit+1), -1, http.StatusRequestEntityTooLarge},
		{"declared over, short body", "a", limit + 1, http.StatusRequestEntityTooLarge},
		{"empty", "", 0, http.StatusNoContent},
	} {
		t.Run(c.name, func(t *testing.T) {
			reached, seen = false, nil
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(c.body)))
			req.ContentLength = c.contentLength
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d", rec.Code, c.want)
			}
			if c.want == http.StatusNoContent {
				if !reached || string(seen) != c.body {
					t.Fatalf("next saw %q (reached %v), want the whole body", seen, reached)
				}
				return
			}
			if reached {
				t.Fatal("an oversized body reached the next handler")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" || !strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("413 body %q content-type %q", rec.Body.String(), ct)
			}
		})
	}
}
