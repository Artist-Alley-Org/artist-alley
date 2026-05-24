// Package middleware contains HTTP middleware shared by every handler.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type ctxKey int

const requestIDKey ctxKey = iota

// HeaderRequestID is the canonical request-ID header. We accept it from
// upstream (e.g., nginx) and otherwise mint a fresh one.
const HeaderRequestID = "X-Request-Id"

// RequestID middleware tags every request with a stable identifier so
// logs across components correlate. The ID is exposed to handlers via
// [RequestIDFromContext] and echoed back on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the request ID tagged on ctx by the
// [RequestID] middleware, or the empty string if unset.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read should never fail; if it does we'd rather lose the
		// ID than crash the request.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
