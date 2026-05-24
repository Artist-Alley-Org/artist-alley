// Package logging configures the process-wide [slog.Logger].
//
// We expose a single Setup() that builds a logger from config and
// installs it as the slog default. Every other package logs through
// slog.* (or a Logger received via dependency injection), never via
// fmt.Print*.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Setup builds a logger from the requested level/format and registers
// it as slog.Default. Returns the logger so callers can attach static
// fields (e.g., "service=aa") at process boot.
func Setup(level, format string) *slog.Logger {
	return setupTo(os.Stdout, level, format)
}

func setupTo(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: parseLevel(level),
		// Replace time field with RFC3339Nano for easier log parsing.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.String(slog.TimeKey, a.Value.Time().UTC().Format("2006-01-02T15:04:05.000000000Z"))
			}
			return a
		},
	}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case "text":
		handler = slog.NewTextHandler(w, opts)
	default:
		handler = slog.NewJSONHandler(w, opts)
	}

	logger := slog.New(handler).With(slog.String("service", "aa"))
	slog.SetDefault(logger)
	return logger
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	default:
		fmt.Fprintf(os.Stderr, "logging: unknown level %q, defaulting to info\n", s)
		return slog.LevelInfo
	}
}
