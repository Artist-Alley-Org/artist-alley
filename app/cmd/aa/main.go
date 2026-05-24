// Binary aa is the artist-alley server entry point.
//
// It boots config, logging, the Postgres pool, and the HTTP server in
// that order, then blocks until the process is signalled to exit.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
	aahttp "github.com/mscrnt/artist-alley/app/internal/http"
	"github.com/mscrnt/artist-alley/app/internal/logging"
)

// Version is overwritten at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if err := run(); err != nil {
		// At this point logging may already be set up; if not, fall
		// back to stderr.
		slog.Error("startup failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.Setup(cfg.LogLevel, cfg.LogFormat)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "boot",
		slog.String("version", Version),
		slog.String("http_addr", cfg.HTTPAddr),
		slog.String("db_host", cfg.DBHost),
		slog.Int("db_port", cfg.DBPort),
		slog.String("db_name", cfg.DBName),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := db.Migrate(ctx, cfg); err != nil {
		return err
	}
	logger.Info("migrations applied")

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("db connected")

	srv := aahttp.New(cfg, logger, pool, Version)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
