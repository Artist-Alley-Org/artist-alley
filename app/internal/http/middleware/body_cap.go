// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CapRequestBody refuses a request whose body is larger than limit
// bytes with 413 and a JSON `{"error": ...}` body, and otherwise hands
// the next handler the buffered body.
//
// It exists for strict-server operations, whose generated adapter
// decodes the whole body before the operation's own code runs: a cap
// inside the operation would come after the bytes were already read.
// The answer depends on the request bytes alone, never on stored data,
// so it reveals nothing to a caller who is not yet authorized.
//
// The body is read through a limit of limit+1 bytes, so an oversized
// body costs at most that much memory whatever Content-Length claims.
func CapRequestBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeTooLarge(w, limit)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "could not read the request body"})
				return
			}
			if int64(len(body)) > limit {
				writeTooLarge(w, limit)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			next.ServeHTTP(w, r)
		})
	}
}

func writeTooLarge(w http.ResponseWriter, limit int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": fmt.Sprintf("request body exceeds %d bytes", limit),
	})
}
