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
	"github.com/mscrnt/artist-alley/app/internal/audiobook"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/http/handlers"
	"github.com/mscrnt/artist-alley/app/internal/http/middleware"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/ldapauth"
	"github.com/mscrnt/artist-alley/app/internal/licensing"
	"github.com/mscrnt/artist-alley/app/internal/preview"
	"github.com/mscrnt/artist-alley/app/internal/samlauth"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
	storages3 "github.com/mscrnt/artist-alley/app/internal/storage/s3"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/tenancy"
)

// Server bundles the [http.Server] with its dependencies so the
// run-loop can shut it down cleanly.
type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	pool    *pgxpool.Pool
	srv     *http.Server
	workers *jobs.Pool // background worker pool; nil if disabled

	// lifecycleCtx bounds background goroutines (licensing re-verify
	// ticker, etc.) to the server's lifetime. Cancelled by Run() at
	// shutdown so the goroutines exit cleanly rather than leaking
	// past process tear-down (or test cleanup).
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
}

// New builds a Server with routes wired up but not yet listening.
// Call [Server.Run] to start serving.
func New(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, version string) (*Server, error) {
	// Bound long-lived background goroutines to a single lifecycle
	// context cancelled at shutdown. Stash on the Server so Run() can
	// cancel it after the HTTP listener stops accepting.
	serverCtx, serverCancel := context.WithCancel(context.Background())

	backend, err := buildStorageBackend(cfg)
	if err != nil {
		serverCancel()
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

	// License state — verifies the .lic file at cfg.LicensePath (if
	// any), caches the resulting Status, and exposes a Source
	// interface every dependent package consults to check feature
	// flags. Community mode (no file) is non-error; status surfaces
	// "loaded: false" + the built-in community defaults.
	licState := licensing.NewState(cfg.LicensePath, cfg.OrgKeyPath, logger)

	// Wire the licensing.State into auth.Identity.Can() so capability
	// checks consult the install's licensed feature set. Without this
	// bridge, caps gated by required_license_feature would deny under
	// every install (the "no source installed" failsafe in
	// capLicenseAllows). The map of cap → feature is loaded from the
	// capabilities table on the next line.
	auth.SetLicenseSource(licState)
	if features, err := loadCapLicenseFeatures(context.Background(), pool); err != nil {
		// Non-fatal: degrade to "no license-gated caps" rather than
		// failing the whole API server. Identity.Can() will allow
		// every cap on the license axis (RBAC still enforced). Logged
		// loudly so the misconfiguration surfaces.
		logger.Error("load capability license features", "err", err)
	} else {
		auth.SetCapLicenseFeatures(features)
	}

	// 24h re-verify ticker. Defense-in-depth: a runtime patch that
	// freezes the cached Status has to also kill this goroutine, or
	// the next tick re-reads the .lic + org.key and overwrites the
	// cache. Bound to the server's shutdown context — graceful Stop()
	// cancels it cleanly.
	licState.StartReverify(serverCtx, 24*time.Hour)

	// Identity-provider registry. Built-in PasswordProvider is
	// unconditional; enterprise providers (LDAP, SAML, ...) attach
	// only when the install's license declares the matching feature.
	//
	// The registry is HOT-SWAPPABLE: licState.OnReload below registers
	// a callback that re-runs buildProviders on every successful .lic
	// upload / re-verify, so an admin who buys an enterprise upgrade
	// can install the new .lic and immediately use LDAP without
	// bouncing the process. Existing user sessions stay live.
	//
	// SAML routes are mounted unconditionally below — the handlers
	// look the provider up from the registry at request time, so the
	// route 404s cleanly when SAML isn't licensed and starts serving
	// the moment a license-with-sso_saml lands.
	providers := auth.NewRegistry()
	if err := providers.Replace(buildProviders(pool, cfg, licState, logger), licState); err != nil {
		logger.Error("identity provider initial build failed", "err", err)
	}
	licState.OnReload(func(_ licensing.Status) {
		if err := providers.Replace(buildProviders(pool, cfg, licState, logger), licState); err != nil {
			logger.Error("identity provider hot-rebuild failed", "err", err)
		} else {
			logger.Info("identity provider registry rebuilt after license reload")
		}
	})

	// Multi-tenant manager. Returns nil when the install lacks
	// multi_tenant; that nil is the canonical "feature unavailable"
	// state every consumer checks for. Federation stays free (per
	// user direction — small communities can self-organize), so we
	// don't construct anything for it here.
	tenantMgr := tenancy.NewManager(licState, logger)
	_ = tenantMgr // 1.18 work — the admin handlers + middleware land then

	// Cross-domain cache registry. Subscribes to Postgres NOTIFY
	// on channel "cache_invalidate" so peer instances (and DB
	// triggers like the asset_field_value rebuild) can drop our
	// stale LRU entries. Per-domain Caches register themselves
	// when their handlers are constructed below.
	cacheReg := cache.NewRegistry(pool, logger)
	if err := cacheReg.Start(context.Background()); err != nil {
		serverCancel()
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
	jobRegistry.Register(preview.NewPDFHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewFontHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewEPUBHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewEPSHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewPSDHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewComicHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewTextHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(preview.NewArchiveHandler(pool, storageSvc, sysCfg, logger))
	// Audiobook post-processing — Phase B-2 stubs. Registered so
	// the dispatcher knows the type names + the admin queue page
	// renders them; the actual ffmpeg work is a TODO in
	// app/internal/audiobook/jobs.go.
	jobRegistry.Register(audiobook.NewMergeHandler(pool, storageSvc, sysCfg, logger))
	jobRegistry.Register(audiobook.NewDecryptHandler(pool, storageSvc, sysCfg, logger))

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

		impl := newAPIServer(pool, logger, cfg, storageSvc, sessions, limiter, auditRec, sysCfg, cacheReg, jobSvc, licState, backend.Name())
		// Hand the provider registry to the auth handler now that
		// both exist. Done out-of-band rather than threading through
		// newAPIServer's positional args — same shape as the
		// password-policy + audit-recorder setters above.
		impl.auth.SetProviderRegistry(providers)
		strict := openapi.NewStrictHandler(impl, nil)
		openapi.HandlerFromMux(strict, r)

		// SAML redirect-flow routes. Always mounted, but the handlers
		// look the provider up from the (hot-swappable) registry at
		// request time — unlicensed installs get a clean 404, and the
		// moment a license-with-sso_saml lands via /admin/license/upload
		// they start serving without a process restart. Trade against
		// the strict "no-route-on-Community" pattern was deliberate:
		// the cap-bridge + chain-of-trust + re-verify ticker remain
		// the load-bearing defense lines, and hot-swap is worth more
		// to operators than a one-byte-harder patcher target here.
		samlRouter := newSAMLRouter(providers)
		r.Get("/auth/saml/login", samlRouter.BeginLogin)
		r.Post("/auth/saml/acs", samlRouter.ConsumeAssertion)
		r.Get("/auth/saml/metadata", samlRouter.Metadata)

		// /assets/{id}/file with Range support so <audio>/<video>
		// can seek into the middle of a large media asset. The
		// openapi-derived handler streams the whole body in one
		// shot; that's fine for downloads but kills seeking on
		// audiobook .m4b / video .mp4. Registered AFTER the
		// HandlerFromMux call so chi's last-write-wins semantics
		// route GET (and the new HEAD) through this handler instead.
		fileH := handlers.NewAssetFileHandler(pool, storageSvc, logger)
		r.Get("/assets/{id}/file", fileH.ServeHTTP)
		r.Head("/assets/{id}/file", fileH.ServeHTTP)

		// /assets/{id}/archive/entry?path=... — stream a single
		// entry out of a ZIP / TAR archive without extracting the
		// whole thing. The manifest already lives on
		// metadata.archive (populated by the preview.archive job);
		// this handler is the per-click data-plane.
		archEntryH := handlers.NewArchiveEntryHandler(pool, storageSvc, logger)
		r.Get("/assets/{id}/archive/entry", archEntryH.ServeHTTP)

		// /assets/{id}/archive/bundle.zip — re-package every entry of
		// the source archive into a single ZIP the browser downloads.
		// Powers the "Extract all" button in ArchiveView's panel; lets
		// the user grab the contents of a TAR / 7z / RAR / tar.xz in
		// a format every OS opens natively.
		archBundleH := handlers.NewArchiveBundleHandler(pool, storageSvc, logger)
		r.Get("/assets/{id}/archive/bundle.zip", archBundleH.ServeHTTP)
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
		workers:         workers,
		lifecycleCtx:    serverCtx,
		lifecycleCancel: serverCancel,
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
			s.lifecycleCancel()
			return err
		}
		s.lifecycleCancel()
		s.logger.LogAttrs(ctx, slog.LevelInfo, "http.shutdown.done")
		return nil
	}
}

// buildProviders constructs the full identity-provider list for the
// current license state. Called once at boot and again on every
// licensing.State swap (via OnReload) so enterprise providers attach
// + detach as licenses are uploaded / expire without a process
// restart.
//
// Order matters only for logging output; sorting happens in
// Registry.List for read-side stability.
func buildProviders(pool *pgxpool.Pool, cfg config.Config, licState *licensing.State, logger *slog.Logger) []auth.IdentityProvider {
	out := []auth.IdentityProvider{
		auth.NewPasswordProvider(pool, cfg.ScrambleKey),
	}
	if licState.HasFeature(ldapauth.LicenseFeature) {
		out = append(out, ldapauth.New("ldap", "LDAP / Active Directory"))
		logger.Info("identity provider: ldap registered (stub impl)")
	} else {
		logger.Info("identity provider: ldap absent (sso_ldap feature not in license)")
	}
	if licState.HasFeature(samlauth.LicenseFeature) {
		out = append(out, samlauth.New("saml", "SAML 2.0 SSO"))
		logger.Info("identity provider: saml registered (stub impl)")
	} else {
		logger.Info("identity provider: saml absent (sso_saml feature not in license)")
	}
	return out
}

// samlRouter wires the SAML redirect-flow HTTP routes to a
// per-request lookup against the (hot-swappable) provider registry.
// When the SAML provider isn't currently registered (Community
// install, or .lic doesn't declare sso_saml), each handler 404s —
// no client distinguishes that from a route that was never mounted
// in the first place. The moment an admin uploads a license that
// includes sso_saml, the OnReload callback rebuilds the registry and
// these routes start serving without any process intervention.
type samlRouter struct {
	providers *auth.Registry
}

func newSAMLRouter(r *auth.Registry) *samlRouter { return &samlRouter{providers: r} }

func (s *samlRouter) resolve() (samlauth.RedirectFlowHandler, bool) {
	p, ok := s.providers.Get("saml")
	if !ok {
		return nil, false
	}
	rfh, ok := p.(samlauth.RedirectFlowHandler)
	return rfh, ok
}

func (s *samlRouter) BeginLogin(w http.ResponseWriter, r *http.Request) {
	if h, ok := s.resolve(); ok {
		h.BeginLogin(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *samlRouter) ConsumeAssertion(w http.ResponseWriter, r *http.Request) {
	if h, ok := s.resolve(); ok {
		h.ConsumeAssertion(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *samlRouter) Metadata(w http.ResponseWriter, r *http.Request) {
	if h, ok := s.resolve(); ok {
		h.Metadata(w, r)
		return
	}
	http.NotFound(w, r)
}

// loadCapLicenseFeatures fetches the cap → required_license_feature
// map from the capabilities table for handoff to auth.SetCapLicenseFeatures.
// Only caps with a non-NULL required_license_feature are returned, so
// the in-memory map is tight even on a large cap table.
func loadCapLicenseFeatures(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	q := auth.New(pool)
	rows, err := q.LoadCapabilityLicenseFeatures(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.RequiredLicenseFeature == nil {
			continue // defensive: SQL filters NULLs but the type is pointer
		}
		out[r.Code] = *r.RequiredLicenseFeature
	}
	return out, nil
}
