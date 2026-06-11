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

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/bootstrap"
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

	// At-rest crypto: federation instance identity (1.22.B-b) +
	// per-user encryption keys (1.22.A) wrap their private keys
	// with the AA_MASTER_KEY-derived AES-256-GCM cipher. Boot
	// fails LOUDLY if the key is missing — federation can't sign
	// without it, and we won't silently fall back to plaintext.
	// Operators without federation needs can set a throwaway key;
	// bootstrap.sh generates one if missing.
	if err := atrest.Init(); err != nil {
		return err
	}
	logger.Info("atrest initialised")

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("db connected")

	// First-boot bootstrap: create the default admin when none
	// exists. Idempotent — no-op on subsequent boots. Runs
	// AFTER migrations + the DB pool are up so we have the
	// schema + a working connection.
	if err := bootstrap.Run(ctx, pool, bootstrap.Config{
		ScrambleKey:         cfg.ScrambleKey,
		AdminPath:           cfg.BootstrapAdminPath,
		DefaultAdminEnabled: cfg.BootstrapDefaultAdmin,
	}, logger, audit.NewRecorder(pool, logger)); err != nil {
		return err
	}

	srv, err := aahttp.New(cfg, logger, pool, Version)
	if err != nil {
		return err
	}
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
