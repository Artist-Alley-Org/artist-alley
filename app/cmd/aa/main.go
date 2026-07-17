// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Binary aa is the artist-alley server entry point.
//
// It boots config, logging, the Postgres pool, and the HTTP server in
// that order, then blocks until the process is signalled to exit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/mscrnt/artist-alley/app/internal/seed"
	"github.com/mscrnt/artist-alley/app/internal/storage"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Version is overwritten at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	// `aa seed ...` is a one-shot DB-direct loader subcommand (issue
	// #321) that populates an instance from a site dataset without a
	// running server. Everything else falls through to the server.
	if len(os.Args) > 1 && os.Args[1] == "seed" {
		if err := runSeed(os.Args[2:]); err != nil {
			slog.Error("seed failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		// At this point logging may already be set up; if not, fall
		// back to stderr.
		slog.Error("startup failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// runSeed parses the seed flags, opens a pool + storage backend the
// same way the server does, optionally resets the content tables, and
// runs the seeder. It does NOT boot the HTTP server.
func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	site := fs.String("site", "", "populated site root (MANIFEST.json + posts.json + bytes)")
	catalogue := fs.String("catalogue", "seed/profiles", "catalogue directory (seed/profiles)")
	limitPerExt := fs.Int("limit-per-extension", 0, "keep at most N assets per file_extension (0 = no limit)")
	reset := fs.Bool("reset", false, "TRUNCATE seed content tables before loading")
	previews := fs.Bool("previews", true,
		"enqueue a preview job per asset so the seed produces derivatives "+
			"(card thumbnails, video sprites); false = fast metadata-only seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *site == "" {
		return errors.New("seed: --site is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.Setup(cfg.LogLevel, cfg.LogFormat)

	// User keypair minting in applyUsers wraps private keys with the
	// AA_MASTER_KEY-derived cipher — init it up front, same as the
	// server boot path.
	if err := atrest.Init(); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	if *reset {
		if err := resetSeedTables(ctx, pool, bootstrap.DefaultUsername); err != nil {
			return fmt.Errorf("seed reset: %w", err)
		}
		logger.Info("seed.reset.done")
	}

	backend, err := aahttp.BuildStorageBackend(cfg)
	if err != nil {
		return fmt.Errorf("seed: storage backend: %w", err)
	}
	storageSvc := storage.NewService(backend, pool)

	runner := seed.NewRunner(pool, storageSvc, seed.Options{
		SiteRoot:      *site,
		CatalogueRoot: *catalogue,
		LimitPerExt:   *limitPerExt,
		AdminUsername: bootstrap.DefaultUsername,
		Logger:        logger,
		Previews:      *previews,
	})
	counts, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	logger.Info("seed.complete",
		"users", counts.Users, "teams", counts.Teams,
		"collections", counts.Collections, "assets", counts.Assets,
		"posts", counts.Posts, "comments", counts.Comments)
	fmt.Printf("seed complete: users=%d teams=%d collections=%d assets=%d posts=%d comments=%d\n",
		counts.Users, counts.Teams, counts.Collections, counts.Assets, counts.Posts, counts.Comments)
	return nil
}

// resetSeedTables clears the content tables so a re-seed starts from a
// clean slate. Baseline lookups (workflow_states, asset_types) and the
// bootstrap admin survive; every fictional user + all their content is
// removed. CASCADE handles the join/dependent tables.
func resetSeedTables(ctx context.Context, pool *pgxpool.Pool, adminUsername string) error {
	// storage_objects is deliberately NOT truncated (#355). The content
	// store is content-addressed and asset-independent: storage_objects
	// / storage_variants are keyed by object_hash and describe what is
	// physically on the storage volume, which a DB truncate obviously
	// does not erase. Truncating them (CASCADE also took storage_variants
	// + storage_pins) desynced the two — the blobs stayed on disk while
	// their rows vanished, and because the preview handlers skip any
	// variant whose blob already exists, a re-seed would then regenerate
	// nothing and leave the instance with originals and zero variant
	// rows: /variants/col 404s, exactly the bug this issue is about.
	//
	// Leaving them alone is also idempotent: the seed's asset ids are
	// stable (from MANIFEST.json), so a re-seed re-uploads the same
	// hashes, dedups onto the existing objects, and re-pins identically.
	const truncate = `TRUNCATE
	    assets, posts, comments, collections, field_definition
	    RESTART IDENTITY CASCADE`
	if _, err := pool.Exec(ctx, truncate); err != nil {
		return err
	}
	// teams gets a per-row DELETE, NOT a slot in the TRUNCATE above.
	// TRUNCATE ... CASCADE empties dependent tables WHOLESALE, and
	// user_roles / user_capability_grants / user_capability_revokes all
	// carry a team_id FK — so naming teams in the TRUNCATE wiped every
	// role and capability grant in the install, including the bootstrap
	// admin's GLOBAL (team_id IS NULL) role. The instance then answered
	// needs_setup=true and nobody could log in, after every --reset.
	//
	// A per-row DELETE follows the same FK with ON DELETE CASCADE but
	// only removes the rows that actually reference a deleted team, so
	// global roles + grants survive.
	if _, err := pool.Exec(ctx, `DELETE FROM teams`); err != nil {
		return err
	}
	// Fictional users (everyone but the bootstrap admin). Their
	// federation keys + profiles cascade.
	if _, err := pool.Exec(ctx, `DELETE FROM "user" WHERE username <> $1`, adminUsername); err != nil {
		return err
	}
	// Preview jobs for the content we just truncated (#355). Every
	// preview job names an asset_id; with the assets gone they'd churn
	// through their retries and land in `failed`. Now that a seed
	// dispatches one per asset, skipping this would orphan a whole
	// dataset's worth on every --reset.
	if _, err := pool.Exec(ctx, `DELETE FROM jobs WHERE type LIKE 'preview.%'`); err != nil {
		return err
	}
	return nil
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

	// Phase 1.55.B §4.4 — schema-freshness boot check. Refuses to
	// start on SchemaUnappliedMigrations (defensive against goose
	// bugs or partial-apply states — Migrate above already ran).
	// Logs WARN on SchemaUnknownNewerSchema (old binary against a
	// newer DB — usually a bad rollback) but continues booting so
	// the operator can surface the warning via /admin/system/health.
	freshness, err := db.CheckSchemaFreshness(ctx, pool)
	if err != nil {
		return fmt.Errorf("schema freshness check: %w", err)
	}
	switch freshness.Status {
	case db.SchemaOK:
		logger.LogAttrs(ctx, slog.LevelInfo, "schema freshness ok",
			slog.Int64("db_max", freshness.DBMaxVer),
			slog.Int64("embedded_max", freshness.EmbeddedMaxVer),
		)
	case db.SchemaUnappliedMigrations:
		logger.LogAttrs(ctx, slog.LevelError, "schema freshness refuse to start",
			slog.String("status", freshness.Status.String()),
			slog.Int64("db_max", freshness.DBMaxVer),
			slog.Int64("embedded_max", freshness.EmbeddedMaxVer),
			slog.String("warning", freshness.Warning),
		)
		return fmt.Errorf("schema freshness: %s", freshness.Warning)
	case db.SchemaUnknownNewerSchema:
		logger.LogAttrs(ctx, slog.LevelWarn, "schema freshness warning",
			slog.String("status", freshness.Status.String()),
			slog.Int64("db_max", freshness.DBMaxVer),
			slog.Int64("embedded_max", freshness.EmbeddedMaxVer),
			slog.String("warning", freshness.Warning),
		)
	}

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
