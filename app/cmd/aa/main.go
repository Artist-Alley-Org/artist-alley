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
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
	"github.com/mscrnt/artist-alley/app/internal/debugsrv"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	aahttp "github.com/mscrnt/artist-alley/app/internal/http"
	"github.com/mscrnt/artist-alley/app/internal/logging"
	"github.com/mscrnt/artist-alley/app/internal/memlimit"
	"github.com/mscrnt/artist-alley/app/internal/memwatch"
	"github.com/mscrnt/artist-alley/app/internal/preview"
	"github.com/mscrnt/artist-alley/app/internal/seed"
	"github.com/mscrnt/artist-alley/app/internal/storage"
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
	// `aa rebuild-previews ...` re-enqueues preview jobs for existing
	// assets with force set, so a renderer fix reaches the catalogue
	// that predates it (#760). Enqueue-only; the server's worker pool
	// does the work.
	if len(os.Args) > 1 && os.Args[1] == "rebuild-previews" {
		if err := runRebuildPreviews(os.Args[2:]); err != nil {
			slog.Error("rebuild-previews failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	// `aa sweep-fixtures ...` removes rows left behind by dogfood and
	// integration runs in a long-lived dev database (#1245). Dry run by
	// default; -apply is required to delete.
	if len(os.Args) > 1 && os.Args[1] == "sweep-fixtures" {
		if err := runSweepFixtures(os.Args[2:]); err != nil {
			slog.Error("sweep-fixtures failed", slog.String("err", err.Error()))
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
	profile := fs.String("profile", seed.ProfileFull,
		"catalogue selection profile: 'full' seeds everything (the demo path); "+
			"'ci' selects a coverage-complete subset — greedy set-cover over posts "+
			"plus a depth floor — and FAILS if the catalogue cannot supply a "+
			"required coverage class rather than seeding a fixture that quietly "+
			"cannot exercise it")
	coverageDepth := fs.Int("coverage-depth", 0,
		"with --profile ci: minimum posts per collection and assets per extension, "+
			"bounded by what the catalogue holds (0 = built-in default). This, not "+
			"the set-cover, is what sizes the seed")
	reset := fs.Bool("reset", false, "clear seed content before loading: the content tables are truncated, and the tables that hold BOTH seeded and shipped/operator rows (field_definition, notifications, scheduled_actions, …) are swept for rows the truncate orphaned. The shipped field catalogue and the bootstrap admin survive")
	previews := fs.Bool("previews", true,
		"enqueue a preview job per asset so the seed produces derivatives "+
			"(card thumbnails, video sprites); false = fast metadata-only seed")
	forcePreviews := fs.Bool("force-previews", false,
		"re-render variants that already exist instead of skipping them. "+
			"--reset does NOT erase the content-addressed variant store, so a "+
			"re-seed of the same dataset normally re-uses every existing render; "+
			"use this when the renderer changed and the stored ones are stale")
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

	// Own the schema prerequisite (#574). `aa seed` is specified to
	// write straight to postgres + storage with NO running server, so
	// it cannot assume something else has migrated — but until now only
	// run() (the serve path) called Migrate. The seeder's first phase,
	// resolveLookups, reads user / workflow_states / asset_types, so
	// against an unmigrated database it died on
	// `relation "user" does not exist`.
	//
	// In the nightly that showed up as a RACE rather than a hard
	// failure: `docker compose up -d` returns before the app finishes
	// migrating, and the seed runs via `run --rm --no-deps` which
	// explicitly skips dependency waiting. Whichever site lost the race
	// seeded nothing and every UI spec then asserted against an empty
	// instance.
	//
	// Migrate is idempotent (goose version table) and now takes an
	// advisory lock, so this is safe whether the server already
	// migrated, is migrating right now, or was never started. Must come
	// before db.Open + seed.Reset, both of which assume tables.
	if err := db.Migrate(ctx, cfg); err != nil {
		return fmt.Errorf("seed: migrate: %w", err)
	}
	logger.Info("seed.migrate.done")

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The OTHER half of the same prerequisite (#574). Migrating alone
	// only moves the failure: resolveLookups' very first statement
	// resolves the bootstrap admin (the seed's collections are owned by
	// it), and the admin is created by bootstrap.Run — which, like
	// Migrate, only ever ran on the serve path. A seed that won the
	// schema race but lost the bootstrap race would swap
	// `relation "user" does not exist` for `resolve admin: no rows` and
	// still leave an empty instance.
	//
	// Same call the server makes, same config. Idempotent (no-ops when
	// an admin already exists) and explicitly race-safe — it re-checks
	// inside its transaction precisely so two processes can't both
	// create the admin.
	//
	// nil recorder: the seed has no audit recorder wired, which just
	// skips the key_generated audit row. The keypair is still minted.
	if err := bootstrap.Run(ctx, pool, bootstrap.Config{
		ScrambleKey:         cfg.ScrambleKey,
		AdminPath:           cfg.BootstrapAdminPath,
		DefaultAdminEnabled: cfg.BootstrapDefaultAdmin,
	}, logger, nil); err != nil {
		return fmt.Errorf("seed: bootstrap admin: %w", err)
	}

	if *reset {
		if err := seed.Reset(ctx, pool, bootstrap.DefaultUsername); err != nil {
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
		Profile:       *profile,
		CoverageDepth: *coverageDepth,
		AdminUsername: bootstrap.DefaultUsername,
		Logger:        logger,
		Previews:      *previews,
		ForcePreviews: *forcePreviews,
	})
	counts, err := runner.Run(ctx)
	if err != nil {
		return err
	}

	// The reset + reseed above have committed. Tell any already-running
	// instance to flush its in-memory caches so it stops serving
	// pre-reset data without needing a restart (#845). This is the whole
	// point of --reset for a live deployment: the seeder is a separate
	// process with no cache Registry, so it broadcasts a wildcard flush
	// over the same NOTIFY channel the caches already LISTEN on. Only on
	// --reset — a plain re-seed layers onto existing data and does not
	// invalidate the world. Best-effort: a failed NOTIFY must not fail an
	// otherwise-successful seed, and a pre-wildcard binary ignores it.
	if *reset {
		if err := cache.EmitFlushAll(ctx, pool); err != nil {
			logger.Warn("seed.cache_flush.failed", "err", err.Error())
		} else {
			logger.Info("seed.cache_flush.sent")
		}
	}

	logger.Info("seed.complete",
		"users", counts.Users, "teams", counts.Teams,
		"collections", counts.Collections, "assets", counts.Assets,
		"posts", counts.Posts, "comments", counts.Comments)
	fmt.Printf("seed complete: users=%d teams=%d collections=%d assets=%d posts=%d comments=%d\n",
		counts.Users, counts.Teams, counts.Collections, counts.Assets, counts.Posts, counts.Comments)
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

	// Bound the Go heap to the container's own cgroup ceiling BEFORE
	// anything starts allocating in earnest (#781). The runtime does
	// not read cgroup limits, so without this GOGC paces from the
	// live-heap ratio alone and the heap grows through the container
	// limit into an OOM kill. Derived, never hardcoded — the ceiling
	// lives in compose and differs per environment.
	memRes := memlimit.Apply(cfg.GoMemLimitRatio)
	// One line, always, whichever path was taken — and the value read
	// BACK OUT of the runtime rather than the one we meant to set
	// (#888). Two messages ("applied" / "not_applied") meant an
	// operator grepping a failed run had to know both names and could
	// not tell "derived correctly" from "silently left alone" without
	// reading prose.
	memwatch.LogLimit(context.Background(), logger, memwatch.LimitReport{
		Effective:   memRes.Effective,
		SourceKind:  memRes.Kind,
		Detail:      memRes.Source,
		CgroupLimit: memRes.CgroupLimit,
		Ratio:       memRes.Ratio,
	})
	// The preview pipeline's resample budget derives from the ceiling
	// applied just above (#887), so it is stated on the line after it.
	// Nothing configures this directly — which is exactly why it has to
	// be readable: a derived bound nobody can see is indistinguishable
	// from no bound at all when the next storm is being diagnosed.
	logger.LogAttrs(context.Background(), slog.LevelInfo, "preview.scale_budget",
		slog.Int64("bytes", preview.ScaleBudgetBytes()),
		slog.Int64("gomemlimit_bytes", memRes.Effective),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Memory instrumentation (#888). Started before migrations so the
	// sample series covers the whole process lifetime — a boot that
	// dies during a heavy migration is exactly the kind of death that
	// currently leaves nothing behind. Runs until ctx is cancelled.
	if mw := memwatch.New(cfg.MemWatch, logger); mw.Enabled() {
		go mw.Run(ctx)
	}

	// Opt-in pprof on its own listener — off unless AA_PPROF_ADDR is
	// set, and never mounted on the application router. See
	// internal/debugsrv for why the gate is structural.
	if dbg := debugsrv.New(cfg.PprofAddr, logger); dbg.Enabled() {
		if err := dbg.Start(ctx); err != nil {
			return fmt.Errorf("pprof listener: %w", err)
		}
	}

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
