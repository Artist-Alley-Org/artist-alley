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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
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
	fixtures := fs.Bool("fixtures", false,
		"also seed the dogfood suite's one-time substrate: four login-capable "+
			"principals and four admin-owned plates the specs used to create for "+
			"themselves on every fresh database and could never delete (there is no "+
			"user-delete endpoint, and asset/post DELETE is a soft delete). Off by "+
			"default — these accounts have committed passwords, so the public demo "+
			"must not have them. Reads seed/profiles/dataset.fixtures.json")
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
	bootstrapCfg := bootstrap.Config{
		ScrambleKey:         cfg.ScrambleKey,
		AdminPath:           cfg.BootstrapAdminPath,
		DefaultAdminEnabled: cfg.BootstrapDefaultAdmin,
	}
	// SERIALIZED for the same reason resetContent is: this is the
	// SEED-invoked bootstrap, running in a process documented to operate
	// against a live instance. It may create the admin and assign the
	// global Admin role, which is an authority mutation like any other.
	if err := func() error {
		release, err := auth.AcquireStructuralAuthorityLock(ctx, pool)
		if err != nil {
			return err
		}
		defer release()
		return bootstrap.Run(ctx, pool, bootstrapCfg, logger, nil)
	}(); err != nil {
		return fmt.Errorf("seed: bootstrap admin: %w", err)
	}

	if *reset {
		if err := resetContent(ctx, pool, bootstrapCfg, logger); err != nil {
			return err
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
		Fixtures:      *fixtures,
		// Same hasher the setup flow and the bootstrap package use, so a
		// seeded principal's password verifies through the ordinary login
		// path (api.go wires the identical closure for the seed admin
		// endpoints).
		HashPassword: func(plaintext string) (string, error) {
			return auth.HashPassword(plaintext, cfg.ScrambleKey)
		},
	})
	// SERIALIZED across the runner too. Its fixture pass calls
	// SeedSetUserGlobalRole, and it writes team_closure self-rows — both
	// change what an existing principal's effective authority resolves
	// to, and this process is documented to run against a live server.
	//
	// A SEPARATE, NON-OVERLAPPING acquisition rather than one lock held
	// across the whole command: the three spans do not nest, and holding
	// one lock from the first bootstrap through the last seeded asset
	// would block every batch apply for the entire reseed rather than
	// for the parts that actually touch authority.
	counts, err := func() (seed.Counts, error) {
		release, lerr := auth.AcquireStructuralAuthorityLock(ctx, pool)
		if lerr != nil {
			return seed.Counts{}, lerr
		}
		defer release()
		return runner.Run(ctx)
	}()
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
		"posts", counts.Posts, "comments", counts.Comments,
		"posts_drifted", counts.PostsDrifted,
		"posts_orphaned", counts.PostsOrphaned)
	fmt.Printf("seed complete: users=%d teams=%d collections=%d assets=%d posts=%d comments=%d\n",
		counts.Users, counts.Teams, counts.Collections, counts.Assets, counts.Posts, counts.Comments)
	// ⛔ THE LAST LINE IS THE ONE PEOPLE READ, so it does not get to say
	// "complete" and stop there when the run knowingly left the
	// catalogue unapplied (#1320). Row counts cannot carry this: the
	// rows are all present, and it is the values inside them that are
	// stale. The exit code stays 0 on purpose; see postdrift.go.
	if counts.PostsDrifted > 0 || counts.PostsOrphaned > 0 {
		fmt.Printf("  NOT a clean reseed: %d post(s) still disagree with the "+
			"catalogue, %d duplicated under an older id.\n"+
			"  See the warning above. `aa seed --reset` is the only thing "+
			"that rebuilds them.\n",
			counts.PostsDrifted, counts.PostsOrphaned)
	}
	return nil
}

// resetContent is what `aa seed --reset` actually does: clear the seeded
// content, then re-assert the bootstrap admin — because the clear takes
// the admin's authority with it (#1274).
//
// # Why bootstrap.Run is called TWICE and not moved
//
// The call before the reset is a prerequisite, not a convenience: the
// seeder's resolveLookups resolves the bootstrap admin as its very first
// statement (the seed's collections are owned by it), so a seed that
// found no admin would die with `resolve admin: no rows` (#574). Moving
// the call after the reset would reintroduce exactly that. Calling it
// again instead is free — Run no-ops when a system admin already exists,
// and re-checks inside its own transaction so parallel processes cannot
// both create one.
//
// # What the reset takes, and why #361's guard does not catch it
//
// seed.Reset TRUNCATEs `assets ... CASCADE`, and CASCADE follows every
// foreign key that POINTS AT a truncated table, transitively, whatever
// its ON DELETE action says. `teams.hero_asset_id` references `assets`
// (migration 00047), and `user_roles`, `user_capability_grants` and
// `user_capability_revokes` all reference `teams` — so truncating assets
// empties all three, including the bootstrap admin's GLOBAL (team_id IS
// NULL) role.
//
// That is the same end state #361 fixed by taking `teams` OUT of the
// TRUNCATE list, and the per-row `DELETE FROM teams` it left behind
// still guards the direct path. It guards only against NAMING teams,
// though; 00047 opened a transitive route to the same table a month
// after #361 closed the direct one, and nothing noticed. The symptom is
// specific and was reproduced before this fix: `admin` still logs in,
// `/api/v1/auth/me` answers with `"capabilities": []`, and every admin
// endpoint answers 403 until the SERVER is restarted and runs the same
// bootstrap itself (see run() below).
//
// # What this does NOT restore
//
// Only the bootstrap admin's global role, because that is all
// bootstrap.Run asserts. Every other row of `user_roles` /
// `user_capability_grants` / `user_capability_revokes` is gone for good.
// For the seeded fictional users that is a no-op — the reset deletes
// them outright and the reseed re-grants what it granted before — and
// for an operator-created user it is equally moot, because
// `DELETE FROM "user" WHERE username <> 'admin'` removes the user too.
// The one real loss is a GLOBAL capability grant held by `admin` itself
// (measured: one planted grant, gone and not rebuilt). On any instance
// `aa seed --reset` is meant for that is inert — the restored Admin role
// already carries everything a grant could add — but it is a loss, not a
// no-op, and it is stated here rather than left to be discovered.
//
// The chosen defence is this invariant and its test
// (reset_admin_test.go), NOT a check over the FK graph. Policing the
// cascade closure is precisely what failed here: 00047 added a legal
// foreign key and no rule about the graph would have objected.
func resetContent(
	ctx context.Context,
	pool *pgxpool.Pool,
	bootstrapCfg bootstrap.Config,
	logger *slog.Logger,
) error {
	// ⛔ SERIALIZED AGAINST IN-FLIGHT AUTHORITY READERS (#1173, #1119).
	//
	// `aa seed` is DESIGNED to run against a live instance — its own
	// migrate step is documented as safe "whether the server already
	// migrated, is migrating right now, or was never started", and
	// --reset broadcasts a wildcard cache flush precisely because a
	// server may be serving while this runs.
	//
	// And what it does here is the largest authority mutation in the
	// system. seed.Reset's TRUNCATE ... CASCADE and its `DELETE FROM
	// teams` empty user_roles, user_capability_grants and
	// user_capability_revokes wholesale; bootstrap.Run then puts the
	// admin's role back. A batch metadata edit that resolved its
	// verdict just before this would otherwise write under authority
	// that no longer exists.
	//
	// THE LOCK SPANS BOTH STEPS, not either one. Releasing it between
	// the reset and the restoration would leave a window in which the
	// authority tables are empty and a reader could act on that.
	//
	// ⭐ This is the SEED-INVOKED bootstrap.Run. The one in run() at
	// server startup is a different concurrency context and stays
	// exempt: it executes before the HTTP server accepts anything, so
	// there is no in-flight operation to serialize against. One
	// function, two call sites, two answers.
	release, err := auth.AcquireStructuralAuthorityLock(ctx, pool)
	if err != nil {
		return fmt.Errorf("seed reset: %w", err)
	}
	defer release()

	if err := seed.Reset(ctx, pool, bootstrap.DefaultUsername); err != nil {
		return fmt.Errorf("seed reset: %w", err)
	}
	if err := bootstrap.Run(ctx, pool, bootstrapCfg, logger, nil); err != nil {
		return fmt.Errorf("seed reset: restore bootstrap admin: %w", err)
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
