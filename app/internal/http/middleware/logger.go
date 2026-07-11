// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// Logger emits one structured log line per request. Captures status,
// latency, bytes written, request ID, method, path, and remote addr.
func Logger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "http.request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Int("bytes", sw.bytes),
				slog.Duration("dur", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
				slog.String("request_id", RequestIDFromContext(r.Context())),
			)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.wroteHeader {
		return
	}
	sw.status = code
	sw.wroteHeader = true
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.WriteHeader(http.StatusOK)
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.bytes += n
	return n, err
}
