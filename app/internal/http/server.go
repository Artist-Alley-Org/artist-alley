// Package http wires up the artist-alley HTTP server: middleware, routes,
// graceful shutdown.
package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/audiobook"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/email/digest"
	"github.com/mscrnt/artist-alley/app/internal/http/handlers"
	"github.com/mscrnt/artist-alley/app/internal/http/middleware"
	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/ldapauth"
	"github.com/mscrnt/artist-alley/app/internal/licensing"
	"github.com/mscrnt/artist-alley/app/internal/observability/healthhandler"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/preview"
	"github.com/mscrnt/artist-alley/app/internal/samlauth"
	"github.com/mscrnt/artist-alley/app/internal/search"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
	storages3 "github.com/mscrnt/artist-alley/app/internal/storage/s3"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
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
	api     *apiServer // aggregate API handler (federation poller is owned here)

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
	sysCfg := sysconfig.NewStore(pool).WithEncrypter(atrest.PackageEncrypter{})

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
	// Subtitle burn — Phase 1.18.B-3 stub; ffmpeg integration
	// deferred to 1.18.B-3-b.
	jobRegistry.Register(subtitles.NewBurnHandler(pool, storageSvc, sysCfg, logger))

	// Email substrate (Phase 1.19.A-1). Pick the Sender mode from
	// AA_EMAIL_MODE (smtp|capture|disabled; default smtp). The
	// notification.email job is what the notifications writer
	// already enqueues whenever a recipient's prefs include
	// "email" — without a handler the rows sit pending forever.
	emailMode := email.PickMode(logger)
	emailSender := email.BuildSender(emailMode, func(ctx context.Context) (email.Config, error) {
		s, err := sysCfg.GetSMTP(ctx)
		if err != nil {
			return email.Config{}, err
		}
		return email.Config{
			Host: s.Host, Port: s.Port,
			Encryption: email.Encryption(string(s.Encryption)),
			Username:   s.Username, Password: s.Password,
			FromAddr: s.FromAddr,
		}, nil
	}, logger)
	emailSite := func(ctx context.Context) (email.SiteContext, error) {
		s, err := sysCfg.GetSite(ctx)
		if err != nil {
			return email.SiteContext{}, err
		}
		return email.SiteContext{Name: s.Name, URL: s.BaseURL}, nil
	}
	jobRegistry.Register(email.NewNotificationJobHandler(pool, emailSender, emailSite, cfg.ScrambleKey, logger))

	// Digest coordinator (Phase 1.55.Y). One hour-ticking job batches
	// non-immediate notification emails per user. Timing knobs read
	// from sysconfig per tick (default 08:00 UTC daily + Monday 08:00
	// UTC weekly). Registered here; the initial enqueue that kicks the
	// self-perpetuating loop fires alongside the other coordinators.
	jobRegistry.Register(&digest.Coordinator{
		Pool:        pool,
		Sender:      emailSender,
		Jobs:        jobSvc,
		Logger:      logger,
		ScrambleKey: cfg.ScrambleKey,
		SiteFn: func(c context.Context) digest.SiteContext {
			if s, err := emailSite(c); err == nil {
				return digest.SiteContext{Name: s.Name, URL: s.URL}
			}
			return digest.SiteContext{}
		},
		CfgFn: func(c context.Context) digest.Config {
			d, err := sysCfg.GetDigest(c)
			if err != nil {
				return digest.Config{DailyHourUTC: 8, WeeklyDay: time.Monday, WeeklyHourUTC: 8}
			}
			return digest.Config{
				DailyHourUTC:  d.DailyHourUTC,
				WeeklyDay:     time.Weekday(d.WeeklyDay),
				WeeklyHourUTC: d.WeeklyHourUTC,
			}
		},
	})
	// Kick the digest loop: enqueue the first coordinator run at the
	// top of the next hour. Idempotency-keyed so a restart doesn't
	// stack duplicate coordinators.
	{
		next := time.Now().UTC().Truncate(time.Hour).Add(time.Hour)
		if _, err := jobSvc.Enqueue(serverCtx, digest.JobTypeCoordinator, struct{}{}, jobs.EnqueueOpts{
			ScheduledFor:   &next,
			IdempotencyKey: "email.digest.coordinator." + next.Format(time.RFC3339),
		}); err != nil {
			logger.LogAttrs(serverCtx, slog.LevelWarn, "digest.coordinator.initial_enqueue_error",
				slog.String("err", err.Error()))
		}
	}

	// Bind the email seam onto the sysconfig.Handler so its
	// /admin/system/smtp/test surface can render+send via the
	// boot-configured Sender. Done up-front (before apiServer
	// construction) so the handler delegate finds it ready.
	emailDeps := &sysconfig.EmailDeps{
		Sender: emailSender,
		Mode:   emailMode,
		Site:   emailSite,
	}

	// /api/v1 — endpoints derive from the OpenAPI spec at
	// app/api/openapi.yaml. apiServer composes every feature package
	// into a single struct that satisfies openapi.StrictServerInterface.
	resolver := auth.NewResolver(pool, logger, sessions, cacheReg)
	// Hoisted so we can stash on Server below (the federation
	// directory poller lives on this struct + Run() needs it).
	var impl *apiServer
	// Phase 1.54.C — hoisted 1.54.A IIIF Image API handler so the
	// dual-mount block after /api/v1 can register it at the root
	// router too. Constructed inside the /api/v1 callback (needs
	// nothing from that scope beyond the outer-scope deps, but the
	// existing site keeps the initialisation local).
	var iiifRootHandler *iiif.Handler
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

		impl = newAPIServer(pool, logger, cfg, storageSvc, sessions, limiter, auditRec, sysCfg, cacheReg, jobSvc, licState, backend.Name())
		// Hand the provider registry to the auth handler now that
		// both exist. Done out-of-band rather than threading through
		// newAPIServer's positional args — same shape as the
		// password-policy + audit-recorder setters above.
		impl.auth.SetProviderRegistry(providers)
		// Bind the email seam onto the sysconfig handler so the
		// /admin/system/smtp/test endpoint can render + drive the
		// boot-configured Sender.
		impl.sysconfigH.SetEmail(emailDeps)
		// Phase 1.19.C — self-service registration surface. The
		// handler refuses 403 when auth.self_registration.enabled
		// is false (the default), so wiring is harmless on
		// closed installs. The seam types live in auth so the
		// auth package stays clear of the email + sysconfig
		// dependency edges; the closures below adapt at the
		// boundary.
		impl.auth.SetRegistrationSurface(auth.RegisterSurface{
			SendVerification: func(ctx context.Context, to, recipientName, verifyURL, expiresIn string) error {
				site, _ := emailSite(ctx)
				msg, err := email.Render(email.TemplateRegisterVerify, []string{to}, map[string]any{
					"site_name":      site.Name,
					"site_url":       site.URL,
					"recipient_name": recipientName,
					"verify_url":     verifyURL,
					"expires_in":     expiresIn,
				})
				if err != nil {
					return err
				}
				return emailSender.Send(ctx, msg)
			},
			SiteForVerify: func(ctx context.Context) (auth.SiteForVerify, error) {
				s, err := emailSite(ctx)
				if err != nil {
					return auth.SiteForVerify{}, err
				}
				return auth.SiteForVerify{Name: s.Name, URL: s.URL}, nil
			},
			RegistrationPolicy: func(ctx context.Context) (auth.RegistrationConfig, error) {
				cfg, err := sysCfg.GetAuth(ctx)
				if err != nil {
					return auth.RegistrationConfig{}, err
				}
				return auth.RegistrationConfig{
					Enabled:                  cfg.SelfRegistration.Enabled,
					RequireEmailVerification: cfg.SelfRegistration.RequireEmailVerification,
					DefaultRole:              cfg.SelfRegistration.DefaultRole,
				}, nil
			},
		})
		strict := openapi.NewStrictHandler(impl, nil)
		openapi.HandlerFromMux(strict, r)

		// Phase 1.18.A-2 follow-up B (commit 2) — generic
		// /admin/{subsystem}/health pattern. First user:
		// metadata-extraction. Future subsystems (email in
		// 1.19.A, search in 1.16, iiif in 1.54) plug in here
		// one line each.
		if impl.metaCounter != nil {
			r.Method(http.MethodGet, "/admin/metadata-extraction/health",
				healthhandler.HandlerFor("metadata-extraction", impl.metaCounter, "system.admin"))
		}

		// Phase 1.16.B-1 — unified /search endpoint. Mounted as
		// a raw chi route inside /api/v1 so auth-resolver
		// middleware has run; the endpoint is anonymous-safe
		// (visibility gate reduces the anonymous view to public
		// entities). /admin/search/health uses the same
		// healthhandler shim as metadata-extraction.
		if impl.searchService != nil {
			r.Method(http.MethodGet, "/search", &search.Handler{
				Service: impl.searchService,
				Logger:  logger,
			})
			if impl.searchService.Counter() != nil {
				r.Method(http.MethodGet, "/admin/search/health",
					healthhandler.HandlerFor("search",
						impl.searchService.Counter().AsSnapshot(),
						"system.admin"))
			}
		}

		// Phase 1.16.B-2 — facets + suggestions + save-as-collection.
		// Same raw-chi pattern as B-1's /search: mounted under
		// /api/v1 so auth-resolver has run; endpoints are anonymous-
		// safe (visibility.Filter reduces the anonymous view).
		if impl.facetDispatcher != nil {
			r.Method(http.MethodGet, "/search/facets", &search.FacetHandler{
				Dispatcher: impl.facetDispatcher,
				Logger:     logger,
			})
		}
		if impl.suggestService != nil {
			r.Method(http.MethodGet, "/search/suggest", &search.SuggestHandler{
				Service: impl.suggestService,
				Logger:  logger,
			})
		}
		if impl.searchService != nil {
			r.Method(http.MethodPost, "/search/save-as-collection", &search.SaveAsCollectionHandler{
				Service: impl.searchService,
				Pool:    pool,
			})
			// Phase 1.16.B-3 (reserved) + 1.16.B-3-followup (activated
			// when the CLIP visual-encoder sidecar is installed +
			// sysconfig.search.visual.enabled=true). ByImageHandler's
			// Provider field is nil when either condition isn't met,
			// preserving the 501 sidecar_not_installed stub body.
			// Populated at boot in newAPIServer if the sysconfig knob
			// is on + the sidecar responds to /health.
			r.Method(http.MethodPost, "/search/by-image", &search.ByImageHandler{
				Logger:         logger,
				Counter:        impl.searchService.Counter(),
				Provider:       impl.visualProvider,
				Pool:           pool,
				MaxUploadBytes: impl.visualMaxUploadBytes,
			})
		}

		// Phase 1.55.Y — RFC 8058 one-click unsubscribe. Public: the
		// signed token in ?token= is the authorization, no session
		// required. GET serves an HTML confirmation for humans clicking
		// the email link; POST is the mail-client one-click target that
		// the List-Unsubscribe header points at. Registered under
		// /api/v1 so that header URL resolves here.
		{
			unsub := &unsubscribeHandler{scrambleKey: cfg.ScrambleKey, prefs: impl.userprefs, logger: logger}
			r.Method(http.MethodGet, "/unsubscribe", unsub)
			r.Method(http.MethodPost, "/unsubscribe", unsub)
		}

		// Phase 1.16.B-4 — saved searches. CRUD mounts via the
		// handler's Mount method so all six routes share the
		// same auth-resolver middleware + owner-check helper.
		if impl.savedSearchHandler != nil {
			impl.savedSearchHandler.Mount(r)
		}

		// Phase 1.16.B-5-followup — search feedback loop (closes #184).
		// User-facing endpoints inside /api/v1 (authenticated); admin
		// aggregation + abuse-review mount alongside reindex below.
		if impl.feedbackHandler != nil {
			impl.feedbackHandler.Mount(r)
		}

		// Phase 1.16.B-5 — arc close: reindex + disk-usage +
		// admin saved-searches surfaces. All admin-cap gated;
		// mount via handler's Mount (chi routes) or direct route
		// registration for the single-route disk-usage handler.
		if impl.reindexHandler != nil {
			impl.reindexHandler.Mount(r)
		}
		// Phase 1.16.B-3-followup-4 — admin visual-embedding backfill
		// trigger (closes #200). Same admin-gated Mount shape as
		// reindex; trigger returns 503 when the visual provider isn't
		// registered so operators diagnose the sysconfig/sidecar gap
		// before enqueueing a run that would immediately fail.
		if impl.visualBackfillHandler != nil {
			impl.visualBackfillHandler.Mount(r)
		}
		// Phase 1.16.B-5-followup — admin feedback surfaces
		// (aggregation + per-user abuse review). Gated on
		// "system.admin" inside the handler.
		if impl.feedbackAdmin != nil {
			impl.feedbackAdmin.Mount(r)
		}
		if impl.diskUsageHandler != nil {
			r.Method(http.MethodGet, "/admin/search/disk-usage", impl.diskUsageHandler)
		}
		if impl.savedSearchAdmin != nil {
			impl.savedSearchAdmin.Mount(r)
		}

		// Phase 1.54.A — IIIF Image API 3.0 Level 0. Mounted
		// inside /api/v1 so the auth resolver middleware above
		// has already run; RequireID just checks the resolved
		// Identity. The URL grammar is per-segment so it can't
		// be expressed in OpenAPI; mounted as raw chi routes.
		//
		// Also dual-mounted at the ROOT router (see block below,
		// outside the /api/v1 group) so third-party IIIF viewers
		// (Mirador, Universal Viewer, OpenSeadragon) reach the same
		// handlers at the URLs publicBaseURL(r) actually emits
		// (`/iiif/3/...` — no `/api/v1` prefix). Phase 1.54.C.
		iiifRootHandler = &iiif.Handler{
			Lookup:   iiif.PoolLookup{Pool: pool},
			Variants: iiifVariantLister{store: sysCfg},
			Streamer: iiifStreamer{storage: storageSvc},
			Logger:   logger,
			RequireID: func(r *http.Request) bool {
				return auth.IdentityFromContext(r.Context()) != nil
			},
		}
		iiifRootHandler.Mount(r)

		// Phase 1.54.B — IIIF Presentation API 3.0 + Content Search
		// 2.0 + legacy 2.0 → 3.0 URL rewrites. Same dual-mount
		// treatment as 1.54.A: register inside /api/v1 (this block)
		// AND at the root router (block below, outside /api/v1) so
		// external viewers reach the same handlers at the emitted
		// URL shape.
		if impl != nil && impl.iiifPresHandler != nil {
			impl.iiifPresHandler.Mount(r)
		}
		if impl != nil && impl.iiifContentSearchHandler != nil {
			impl.iiifContentSearchHandler.Mount(r)
		}
		if impl != nil && impl.iiifRedirectHandler != nil {
			impl.iiifRedirectHandler.Mount(r)
		}
		// /admin/iiif/health — shared healthhandler shim. Same
		// pattern as /admin/search/health + /admin/metadata-
		// extraction/health.
		if impl != nil && impl.iiifCounter != nil {
			r.Method(http.MethodGet, "/admin/iiif/health",
				healthhandler.HandlerFor("iiif", impl.iiifCounter, "system.admin"))
		}

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

	// Phase 1.54.C — IIIF root-URL alias for third-party viewer
	// compatibility. The 1.54.A + 1.54.B handlers emit URLs at
	// `/iiif/{2,3}/...` (via publicBaseURL(r)) but were originally
	// mounted only inside /api/v1, so external Mirador / UV /
	// OpenSeadragon requests fell through to the SPA 404. Dual-mount
	// them at the root router with the same ResolveIdentity middleware
	// the /api/v1 group runs so auth-gated assets still resolve their
	// caller identity.
	//
	// Chi's route dispatch is deterministic on prefix: /iiif/{2,3}/*
	// hits this group; /api/v1/iiif/... hits the group above. Both
	// share the same handler instances.
	//
	// Alternative was an nginx rewrite (/iiif/3/ → /api/v1/iiif/3/)
	// but the prod embed_web deployment shape (used by ui-pr CI + any
	// operator running the docker image standalone) has no nginx —
	// only Go dual-mount reaches both deployment topologies. See #188.
	if iiifRootHandler != nil {
		r.Group(func(r chi.Router) {
			r.Use(resolver.ResolveIdentity)
			iiifRootHandler.Mount(r)
			if impl != nil {
				if impl.iiifPresHandler != nil {
					impl.iiifPresHandler.Mount(r)
				}
				if impl.iiifContentSearchHandler != nil {
					impl.iiifContentSearchHandler.Mount(r)
				}
				if impl.iiifRedirectHandler != nil {
					impl.iiifRedirectHandler.Mount(r)
				}
			}
		})
	}

	// Federation inbox (Phase 1.22.D-a) — mounted at ROOT, not under
	// /api/v1. The federation delivery worker constructs URLs as
	// `peer.InstanceURL + "/federation/inbox"` (no /api/v1 prefix —
	// see internal/federation/outbox/delivery.go); the e2e tests
	// mount inbox at root via httptest the same way. Direct chi mount
	// because the handler needs raw http.Request + ResponseWriter for
	// body draining + Signature header parsing + Retry-After response
	// control which the strict-server shape hides. Public endpoint
	// authed via HTTP-Signature on the request itself, not session/
	// bearer — so it doesn't pick up the /api/v1 ResolveIdentity
	// middleware.
	if impl != nil && impl.inboxHandler != nil {
		r.Post("/federation/inbox", impl.inboxHandler.PostInbox)
		// Batched variant per spec §10.4 — same handler; amortises
		// HTTP-Sig + TLS overhead across up to 50 envelopes per
		// request. Phase 1.22.D-b-5.
		r.Post("/federation/inbox/batch", impl.inboxHandler.PostInboxBatch)
	}

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
		api:             impl,
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

	// Federation directory poller (Phase 1.22.B-c). Walks
	// subscribed directories every 5 minutes; per-directory cadence
	// is read from the row's poll_interval_seconds column. Stop
	// via context cancellation alongside the HTTP server.
	if s.api != nil && s.api.directoryPoller != nil {
		go s.api.directoryPoller.Run(ctx)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "directory.poller.start")
	}

	// Federation shares expiry sweeper (Phase 1.22.C-d). Periodic
	// goroutine that emits aa:Unshare for expired-active shares
	// so recipients purge cached bytes. Stop via ctx cancellation.
	if s.api != nil && s.api.sharesSweeper != nil {
		go s.api.sharesSweeper.Run(ctx)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "shares.sweeper.start")
	}

	// Federation inbox dispatcher (Phase 1.22.D-a-4). Drains
	// pending federation_inbox rows + invokes the per-verb
	// handler. Stop via ctx cancellation.
	if s.api != nil && s.api.inboxDispatcher != nil {
		go s.api.inboxDispatcher.Run(ctx)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "inbox.dispatcher.start")
	}

	// Federation OUTBOX dispatcher (Phase 1.22.D-b). LISTEN/
	// NOTIFY-driven fan-out from activities → federation_outbox.
	// Sub-100ms latency via the trigger from migration 00005;
	// 30s ticker is correctness backstop only.
	if s.api != nil && s.api.outboxDispatcher != nil {
		go s.api.outboxDispatcher.Run(ctx)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.dispatcher.start")
	}

	// Federation outbox DELIVERY worker (Phase 1.22.D-b-4).
	// Drains federation_outbox rows + POSTs to recipient peer's
	// /federation/inbox. nil when the instance identity hasn't
	// been generated yet (first-run before /setup completes).
	if s.api != nil && s.api.outboxDelivery != nil {
		go s.api.outboxDelivery.Run(ctx)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "outbox.delivery.start")
	}

	// Federation user-keys retained-row sweeper (Phase 1.22.I-h).
	// Reaps federation_user_keys rows whose retained_until grace
	// window has elapsed. Boot-time first sweep covers
	// expirations accumulated during downtime; then ticks every
	// SweepTickDefault (1h) until ctx cancels. The audit hook
	// fires federation.user.key_retained_expired once per
	// non-zero reap.
	if s.api != nil && s.api.userKeysSweeper != nil {
		go s.api.userKeysSweeper.Run(ctx)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "userkeys.sweeper.start")
	}
	// Phase 1.17.C capability-expiry sweeper. Same lifecycle —
	// starts here, runs until ctx cancels, log a start event so
	// boot observability is consistent across both background
	// reapers.
	if s.api != nil && s.api.capabilitySweeper != nil {
		go s.api.capabilitySweeper.Run(ctx)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "auth.capability_sweeper.start")
	}
	// Phase 1.53.A — MCP-client per-server health-check supervisor.
	// Spawns one polling goroutine per currently-enabled MCP server;
	// supervisor + children exit when ctx cancels.
	if s.api != nil && s.api.mcpHealth != nil {
		go func() {
			if err := s.api.mcpHealth.Run(ctx); err != nil {
				s.logger.LogAttrs(ctx, slog.LevelWarn, "mcp.healthcheck.exit",
					slog.String("err", err.Error()))
			}
		}()
		s.logger.LogAttrs(ctx, slog.LevelInfo, "mcp.healthcheck.start")
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

// iiifVariantLister adapts sysconfig.Store into the
// iiif.VariantLister interface. Maps each PreviewVariant to a
// VariantSize, flagging Cover-fit ones so the IIIF info.json
// generator excludes them from the proportional sizes block (it
// uses them for the square-crop region only).
type iiifVariantLister struct {
	store *sysconfig.Store
}

func (l iiifVariantLister) ListIIIFVariants(ctx context.Context) ([]iiif.VariantSize, error) {
	cfg, err := l.store.GetPreviews(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]iiif.VariantSize, 0, len(cfg.Variants))
	for _, v := range cfg.Variants {
		out = append(out, iiif.VariantSize{
			Key:    v.Key,
			MaxDim: v.MaxDim,
			Cover:  v.Fit == sysconfig.PreviewFitCover,
		})
	}
	return out, nil
}

// iiifStreamer adapts storage.Service into the iiif.VariantStreamer
// interface. The /file vs /variants distinction the asset handler
// makes doesn't apply here — IIIF Level 0 only ever serves
// pre-baked variants.
type iiifStreamer struct {
	storage *storage.Service
}

func (s iiifStreamer) OpenVariant(ctx context.Context, hash, key string) (io.ReadCloser, int64, string, error) {
	body, info, err := s.storage.Download(ctx, hash, key)
	if err != nil {
		return nil, 0, "", err
	}
	return body, info.Size, info.ContentType, nil
}
