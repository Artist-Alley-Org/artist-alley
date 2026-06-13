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
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
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
	auditRec := audit.NewRecorder(pool, logger)
	if err := bootstrap.Run(ctx, pool, bootstrap.Config{
		ScrambleKey:         cfg.ScrambleKey,
		AdminPath:           cfg.BootstrapAdminPath,
		DefaultAdminEnabled: cfg.BootstrapDefaultAdmin,
	}, logger, auditRec); err != nil {
		return err
	}

	// Phase 1.22.I-b safety net: backfill federation keypairs for
	// any pre-existing approved user that lacks one. Covers two
	// real cases the three in-tx callers (bootstrap, /setup,
	// /admin/seed/users) DON'T:
	//
	//   * Users created BEFORE 1.22.I-b shipped (instances
	//     upgraded forward carry pre-existing user rows).
	//   * Users created via test fixtures or direct DB INSERTs
	//     that bypass the three wired paths.
	//
	// Steady-state happy path: one query returns zero rows + the
	// sweep exits silently. Real backfill: per-user audit row +
	// a single summary boot-log line. The sweep runs BEFORE
	// srv.Run so the master key + pool aren't competing with
	// request handling on the rare bulk-mint boot.
	if _, err := userkeys.BackfillMissingKeys(ctx, pool, logger,
		auditRec.FederationUserKeyGeneratedSystem,
	); err != nil {
		// Return error means something catastrophic (DB
		// unreachable, master-key rotation broke wrapping) —
		// surface to the caller so the boot fails loudly.
		// Per-user mint failures are counted in stats + logged
		// but DON'T propagate; the sweep continues.
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
