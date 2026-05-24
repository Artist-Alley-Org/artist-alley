package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover catches panics in downstream handlers, logs them with a stack
// trace, and returns a 500. Without this, a panic in any handler would
// take the entire server process down.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.LogAttrs(r.Context(), slog.LevelError, "http.panic",
						slog.String("error", fmt.Sprint(rec)),
						slog.String("stack", string(debug.Stack())),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("request_id", RequestIDFromContext(r.Context())),
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
