// Package http wires up the artist-alley HTTP server: middleware, routes,
// graceful shutdown.
package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/http/handlers"
	"github.com/mscrnt/artist-alley/app/internal/http/middleware"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
	storages3 "github.com/mscrnt/artist-alley/app/internal/storage/s3"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// Server bundles the [http.Server] with its dependencies so the
// run-loop can shut it down cleanly.
type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	pool    *pgxpool.Pool
	srv     *http.Server
	workers *jobs.Pool // background worker pool; nil if disabled
}

// New builds a Server with routes wired up but not yet listening.
// Call [Server.Run] to start serving.
func New(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, version string) (*Server, error) {
	backend, err := buildStorageBackend(cfg)
	if err != nil {
		return nil, fmt.Errorf("storage backend: %w", err)
	}
	storageSvc := storage.NewService(backend, pool)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "storage.ready",
		slog.String("backend", backend.Name()),
	)

	r := chi.NewRouter()

	// Order matters: RequestID first so it shows up in every later log.
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(logger))
	r.Use(middleware.Recover(logger))
	r.Use(middleware.VariantCache)

	health := &handlers.Health{
		Pool:    pool,
		Version: version,
		Started: time.Now(),
	}
	r.Method(http.MethodGet, "/healthz", health)
	r.Method(http.MethodGet, "/readyz", health) // same handler for now

	// Shared auth subsystem: one SessionManager + LoginLimiter for the
	// whole process; one audit Recorder feeds both login attempts and
	// every other security-relevant event later phases will plug in.
	sessions := auth.NewSessionManager(pool)
	limiter := auth.NewLoginLimiter()
	auditRec := audit.NewRecorder(pool, logger)
	sysCfg := sysconfig.NewStore(pool)

	// Cross-domain cache registry. Subscribes to Postgres NOTIFY
	// on channel "cache_invalidate" so peer instances (and DB
	// triggers like the asset_field_value rebuild) can drop our
	// stale LRU entries. Per-domain Caches register themselves
	// when their handlers are constructed below.
	cacheReg := cache.NewRegistry(pool, logger)
	if err := cacheReg.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("cache registry: %w", err)
	}

	// Background-job infrastructure (Phase 1.18.A). The Registry is
	// process-wide so any package that wants to add a job type
	// registers a Handler before workers start. The Service wraps the
	// jobs table; the Pool spins up in-process workers that drain it.
	// External / federated workers reuse the same Service via the
	// /jobs/claim HTTP surface.
	jobRegistry := jobs.NewRegistry()
	jobSvc := jobs.NewService(pool, logger, jobRegistry)

	// Register preview handlers. preview.raster ships in 1.18.A;
	// preview.video adds the HLS / poster / scrub-sprite pipeline
	// in 1.18.B-1 (with GPU-encoder auto-detection at boot);
	// preview.model adds the Blender-headless turntable in 1.18.B-11.
	// SVG joins the raster handler (extension lives in rasterExts).
	// pdf / font still pending.
	jobRegistry.Register(preview.NewRasterHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewVideoHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewModelHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewAudioHandler(pool, storageSvc, sysCfg, logger))

	// /api/v1 — endpoints derive from the OpenAPI spec at
	// app/api/openapi.yaml. apiServer composes every feature package
	// into a single struct that satisfies openapi.StrictServerInterface.
	resolver := auth.NewResolver(pool, logger, sessions, cacheReg)
	r.Route("/api/v1", func(r chi.Router) {
		// Resolve identity (cookie or Bearer token) for every request
		// under /api/v1. Anonymous requests still pass through — each
		// handler decides whether to require auth (the OpenAPI spec
		// records the security requirements; codegen-enforced
		// authorization comes in Phase 1.3).
		r.Use(resolver.ResolveIdentity)

		// HLS variants live at multi-segment keys like
		// `hls/master.m3u8`, `hls/720p/seg00012.ts`. chi's default
		// `{variant}` param is non-greedy ([^/]+), so the openapi-
		// derived route only matches single-segment variants. Register
		// a wildcard ahead of the strict handler that streams any
		// `hls/*` / `turntable/*` / `views/*` variant straight from
		// storage. The middleware/VariantCache still picks these up by
		// path. Same handler shape, different prefix per route.
		r.Get("/assets/{id}/variants/hls/*",
			handlers.NewPathVariantHandler(pool, storageSvc, logger, "hls").ServeHTTP)
		r.Get("/assets/{id}/variants/turntable/*",
			handlers.NewPathVariantHandler(pool, storageSvc, logger, "turntable").ServeHTTP)
		r.Get("/assets/{id}/variants/views/*",
			handlers.NewPathVariantHandler(pool, storageSvc, logger, "views").ServeHTTP)

		impl := newAPIServer(pool, logger, cfg, storageSvc, sessions, limiter, auditRec, sysCfg, cacheReg, jobSvc, backend.Name())
		strict := openapi.NewStrictHandler(impl, nil)
		openapi.HandlerFromMux(strict, r)
	})

	// Worker pool. Sized to NumCPU/2 so we don't starve the request
	// pipeline on small hosts. Empty Types = drain every registered
	// type — keeping the wiring trivial for single-process installs.
	workers := &jobs.Pool{
		Service: jobSvc,
		Logger:  logger,
		Size:    workerPoolSize(cfg),
		Types:   nil,
	}

	// Static frontend (SvelteKit + Tailwind) — mounted last so the
	// API routes above take precedence. In dev (no embed_web tag)
	// this is a no-op; developers run `npm run dev` (the `web`
	// service in docker-compose) on :5173. In prod (embed_web tag)
	// the SvelteKit build is //go:embed-ded and served from /.
	// See app/internal/http/static_{dev,embed}.go.
	mountStaticFrontend(r)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "frontend.mode",
		slog.Bool("embedded", hasEmbeddedFrontend()),
	)

	return &Server{
		cfg:    cfg,
		logger: logger,
		pool:   pool,
		srv: &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		workers: workers,
	}, nil
}

// workerPoolSize picks a sensible default — NumCPU/2 capped at 8 so a
// 64-core host doesn't spin up a thundering herd against the DB.
// Override knob can land on cfg in 1.18.E if any install needs it.
func workerPoolSize(cfg config.Config) int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// buildStorageBackend picks the storage.Backend implementation named
// by cfg.StorageBackend. Backend-specific config is validated by the
// implementation's constructor.
func buildStorageBackend(cfg config.Config) (storage.Backend, error) {
	switch cfg.StorageBackend {
	case "fs", "":
		return storagefs.New(cfg.StorageFSRoot)
	case "s3":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return storages3.New(ctx, storages3.Config{
			Bucket:       cfg.StorageS3Bucket,
			Region:       cfg.StorageS3Region,
			Endpoint:     cfg.StorageS3Endpoint,
			AccessKey:    cfg.StorageS3AccessKey,
			SecretKey:    cfg.StorageS3SecretKey,
			UsePathStyle: cfg.StorageS3UsePathStyle,
		})
	default:
		return nil, fmt.Errorf("unknown AA_STORAGE_BACKEND=%q (expected fs|s3)", cfg.StorageBackend)
	}
}

// Run starts the listener and blocks until ctx is cancelled or the
// server itself errors. On cancellation it performs a graceful shutdown
// bounded by 15 seconds.
func (s *Server) Run(ctx context.Context) error {
	// Start the in-process worker pool first so jobs are draining
	// before any HTTP traffic enqueues new work. instanceID is just
	// the hostname for now; once federation lands it'll come from
	// system_config (origin_server_id).
	if s.workers != nil {
		instanceID, _ := os.Hostname()
		if instanceID == "" {
			instanceID = "local"
		}
		s.workers.Start(ctx, instanceID)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "jobs.pool.start",
			slog.Int("workers", s.workers.Size),
		)
		defer s.workers.Stop()
	}

	listenErr := make(chan error, 1)
	go func() {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "http.listen",
			slog.String("addr", s.cfg.HTTPAddr),
		)
		err := s.srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.logger.LogAttrs(ctx, slog.LevelInfo, "http.shutdown.start", slog.Any("cause", ctx.Err()))
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.logger.LogAttrs(ctx, slog.LevelError, "http.shutdown.error", slog.Any("err", err))
			return err
		}
		s.logger.LogAttrs(ctx, slog.LevelInfo, "http.shutdown.done")
		return nil
	}
}
