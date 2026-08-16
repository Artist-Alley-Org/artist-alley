// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/google/uuid"
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/assettype"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/auth/lockout"
	"github.com/mscrnt/artist-alley/app/internal/brushpacks"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/federation/directory"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/licensing"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/setup"
	"github.com/mscrnt/artist-alley/app/internal/sitetext"
	"github.com/mscrnt/artist-alley/app/internal/social"
	"github.com/mscrnt/artist-alley/app/internal/social/mention"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/tags"
	"github.com/mscrnt/artist-alley/app/internal/teams"
	"github.com/mscrnt/artist-alley/app/internal/trash"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	aiadmin "github.com/mscrnt/artist-alley/app/internal/ai/admin"
	aiembeddings "github.com/mscrnt/artist-alley/app/internal/ai/embeddings"
	aijobs "github.com/mscrnt/artist-alley/app/internal/ai/jobs"
	mcpadmin "github.com/mscrnt/artist-alley/app/internal/ai/mcp_admin"
	mcpdispatch "github.com/mscrnt/artist-alley/app/internal/ai/mcp_dispatch"
	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
	aicliplocal "github.com/mscrnt/artist-alley/app/internal/ai/providers/cliplocal"
	mcpserver "github.com/mscrnt/artist-alley/app/internal/ai/providers/mcp_server"
	aiwhisperlocal "github.com/mscrnt/artist-alley/app/internal/ai/providers/whisper_local"
	aitranscribe "github.com/mscrnt/artist-alley/app/internal/ai/transcribe"
	"github.com/mscrnt/artist-alley/app/internal/aiedit"
	"github.com/mscrnt/artist-alley/app/internal/aiedit/providers/comfyuimcp"
	assetmetadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	exifext "github.com/mscrnt/artist-alley/app/internal/asset/metadata/exif"
	iptcext "github.com/mscrnt/artist-alley/app/internal/asset/metadata/iptc"
	pdfext "github.com/mscrnt/artist-alley/app/internal/asset/metadata/pdf"
	rawpkg "github.com/mscrnt/artist-alley/app/internal/asset/metadata/raw"
	xmpext "github.com/mscrnt/artist-alley/app/internal/asset/metadata/xmp"
	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/featured"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/p2p"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	"github.com/mscrnt/artist-alley/app/internal/iiif"
	contentsearch "github.com/mscrnt/artist-alley/app/internal/iiif/content_search"
	iiiffederation "github.com/mscrnt/artist-alley/app/internal/iiif/federation"
	"github.com/mscrnt/artist-alley/app/internal/iiif/presentation"
	iiifredirect "github.com/mscrnt/artist-alley/app/internal/iiif/redirect"
	"github.com/mscrnt/artist-alley/app/internal/messages"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
	"github.com/mscrnt/artist-alley/app/internal/requests"
	"github.com/mscrnt/artist-alley/app/internal/richtext"
	"github.com/mscrnt/artist-alley/app/internal/scheduledactions"
	"github.com/mscrnt/artist-alley/app/internal/search"
	searchdiskusage "github.com/mscrnt/artist-alley/app/internal/search/disk_usage"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/feedback"
	"github.com/mscrnt/artist-alley/app/internal/search/reindex"
	"github.com/mscrnt/artist-alley/app/internal/search/saved"
	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	searchvector "github.com/mscrnt/artist-alley/app/internal/search/vector"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualbackfill"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualembed"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualstore"
	"github.com/mscrnt/artist-alley/app/internal/seed"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
	"github.com/mscrnt/artist-alley/app/internal/userprefs"
	"github.com/mscrnt/artist-alley/app/internal/users"
	"github.com/mscrnt/artist-alley/app/internal/workflow"
)

// apiServer aggregates every feature package's handler into the single
// struct openapi.StrictServerInterface expects.
//
// Each method here is a one-line forward to the package that owns the
// implementation. The boilerplate is intentional: the alternative
// (struct embedding) collides because both feature packages export a
// `Handler` type, and we prefer explicit dispatch over magic anyway.
type apiServer struct {
	// pool + cacheReg captured on the server so the cross-domain
	// invalidator helpers (Phase 1.55.U-2 §7.2 owner-profile
	// invalidation on collection writes) can reach the DB + the
	// cache.Registry without threading them through every method
	// signature.
	pool     *pgxpool.Pool
	cacheReg *cache.Registry

	// version is main.Version (git tag baked in via ldflags, or "dev"),
	// set out-of-band in server.New after construction. Surfaced by
	// GetBuildInfo for the admin About page (#406).
	version string

	auth         *auth.Handler
	resourceType *assettype.Handler
	storage      *storage.Handler
	assets       *assets.Handler
	metadata     *metadata.Handler
	collections  *collections.Handler
	posts        *posts.Handler
	teams        *teams.Handler
	// #1123 — tag follows. Separate from `posts` (which owns post_tags)
	// because the follow is a bookmark rather than a post fact, and
	// separate from `teams` for the reason team follows are separate
	// from memberships: the shapes rhyme, the tables do not.
	tags   *tags.Handler
	users  *users.Handler
	social *social.Handler
	// #937 — GET /account/trash. Reads across assets/posts/collections
	// but owns none of them; see the package doc for why it is one
	// endpoint and not three.
	trash      *trash.Handler
	setup      *setup.Handler
	workflow   *workflow.Handler
	sysconfigH *sysconfig.Handler
	jobs       *jobs.HTTPHandler
	brushpacks *brushpacks.Handler
	audit      *audit.HTTPHandler
	// #600 — GET /account/activity. Reads audit_events scoped to the
	// caller. Separate from `audit` because that one is admin-gated;
	// see audit/activity.go for why they are two structs.
	activity           *audit.AccountHandler
	scheduledActions   *scheduledactions.HTTPHandler
	licensing          *licensing.Handler
	userprefs          *userprefs.Handler
	aiAdmin            *aiadmin.Handler      // Phase 1.14.A inference subsystem admin surface
	aiBridge           ai.Bridge             // Phase 1.14.A-bridge — read/write seam for AI handlers
	aiRouter           *ai.Router            // Phase 1.14.B — typed inference dispatch w/ registered providers
	mcpRegistry        *mcpregistry.Registry // Phase 1.53.A — MCP server registration CRUD + cache
	mcpDispatch        *mcpdispatch.Dispatcher
	mcpHealth          *mcpdispatch.HealthChecker
	mcpProviders       *mcpProviderTable
	mcpAdmin           *mcpadmin.Handler
	notifications      *notifications.Handler
	messages           *messages.Handler
	activities         *activities.Writer
	activitiesAdmin    *activities.AdminHandler
	peers              *peer.Registry
	peersAdmin         *peer.AdminHandler
	peersHandshake     *peer.AdminHandshakeHandler
	peersPublic        *peer.PublicHandler
	fedIdentity        *identity.Manager
	fedEngine          *peer.Engine
	directories        *directory.Registry
	directoriesAdmin   *directory.AdminHandler
	directoryPoller    *directory.Poller
	p2pRegistry        *p2p.Registry
	p2pAdmin           *p2p.AdminHandler
	sharesRegistry     *shares.Registry
	sharesAdmin        *shares.AdminHandler
	sharesSweeper      *shares.Sweeper
	inboxHandler       *inbox.Handler
	inboxDispatcher    *inbox.Dispatcher
	outboxDispatcher   *outbox.Dispatcher
	outboxDelivery     *outbox.Worker
	outboxAdmin        *outbox.AdminHandler
	userKeysSweeper    *userkeys.Sweeper
	userKeysAdmin      *userkeys.AdminHandler
	capabilitySweeper  *auth.CapabilitySweeper
	requests           *requests.Handler
	requestsHTTP       *requests.HTTPHandler
	featuredHTTP       *featured.HTTPHandler
	featuredDomain     *featured.Handler
	sitetextHTTP       *sitetext.HTTPHandler
	emailTemplatesHTTP *email.HTTPHandler
	subtitles          *subtitles.Handler
	subtitlesHTTP      *subtitles.HTTPHandler
	aieditHTTP         *aiedit.HTTPHandler
	metaCounter        *assetmetadata.Counter
	metaAdmin          *assetmetadata.AdminHandler
	jobsSvc            *jobs.Service
	jobsAdmin          *jobs.AdminHandler
	storageAdmin       *storage.AdminHandler
	sysCfg             *sysconfig.Store
	seedAdmin          *seed.AdminHandler
	// Phase 1.16.B-1 — unified search foundation. Nil when boot
	// intentionally disables /search (tests that spin up a
	// minimal server without the search subsystem).
	searchService *search.Service
	// Phase 1.16.B-2 — facets + suggestions + save-as-collection.
	// All three share the searchService's pool + counter; nil when
	// searchService is nil.
	facetDispatcher *facet.Dispatcher
	suggestService  *suggest.Service
	// Phase 1.16.B-4 — saved searches. Handler owns the HTTP
	// CRUD; the coordinator + run job handlers are registered
	// separately in Server.Run and consume the same Store +
	// Executor + Notifier.
	savedSearchStore    *saved.Store
	savedSearchExecutor *saved.Executor
	savedSearchNotifier *saved.Notifier
	savedSearchHandler  *saved.Handler
	// Phase 1.16.B-5 — reindex + disk-usage + admin saved-
	// searches. All three surface admin routes; reindex also
	// registers a job type.
	reindexStore     *reindex.Store
	reindexHandler   *reindex.Handler
	diskUsageCache   *searchdiskusage.Cache
	diskUsageHandler *searchdiskusage.Handler
	savedSearchAdmin *saved.AdminHandler
	// Phase 1.16.B-5-followup — search-result feedback loop
	// (closes #184). Service + user handler + admin handler wired
	// unconditionally at boot; runtime kill switch honoured via
	// sysconfig.search.feedback.enabled (checked per-request inside
	// the Service).
	feedbackStore   *feedback.Store
	feedbackService *feedback.Service
	feedbackHandler *feedback.Handler
	feedbackAdmin   *feedback.AdminHandler
	// Phase 1.16.B-3-followup — CLIP visual encoder sidecar activation
	// (closes #183). Zero-valued visualProvider means the feature is
	// disabled (sysconfig.search.visual.enabled=false OR sidecar
	// unreachable at boot); by_image.ByImageHandler treats a nil
	// Provider as the pre-existing 501 sidecar_not_installed stub
	// path. Populated by newAPIServer's visual-provider bootstrap.
	visualProvider       visualprovider.Provider
	visualMaxUploadBytes int64
	// Phase 1.16.B-3-followup-4 — admin visual-embedding backfill trigger
	// (closes #200). Store + Handler are always constructed so the
	// admin surface is discoverable; the trigger endpoint returns 503
	// when visualProvider is nil (matching the by-image handler's
	// stub-when-unregistered semantics). Job registration happens
	// alongside reindex.Job below.
	visualBackfillStore   *visualbackfill.Store
	visualBackfillHandler *visualbackfill.Handler
	visualBackfillCfg     visualBackfillJobConfig
	// Phase 1.16.B-3-followup-2 — async upload-hook visual-embed
	// (closes #201). Sibling to the backfill trigger above:
	// backfill handles pre-existing assets, this handles new
	// uploads. Counter is process-shared so the /admin/search/health
	// gauge accessor + the Job's Handle both write to the same
	// atomic surface. Job registered alongside backfill.Job below.
	visualEmbedCounter    *visualembed.Counter
	visualEmbedDispatcher *visualembed.Dispatcher
	visualEmbedJob        *visualembed.Job
	visualEmbedCfg        visualEmbedJobConfig
	// Phase 1.55.C-1 — soft-delete Service (Restore + HardDeletePast
	// per entity). Exposed on apiServer so the assets/posts/collections
	// handlers can call Restore directly rather than going through a
	// second layer. The gc CoordinatorJob is registered against jobSvc.
	softdeleteSvc *softdelete.Service
	// Phase 1.54.B — IIIF Presentation API 3.0 + Content Search 2.0.
	// One HealthCounter is fanned out to all three sub-package
	// Counter surfaces (presentation, content_search, redirect) and
	// exposes /admin/iiif/health via the shared healthhandler shim.
	iiifCounter              *iiif.HealthCounter
	iiifManifestCache        *presentation.Cache
	iiifFedResolver          *iiiffederation.Resolver
	iiifLoader               *presentation.Loader
	iiifBuilder              *presentation.Builder
	iiifPresHandler          *presentation.Handler
	iiifContentSearchHandler *contentsearch.Handler
	iiifRedirectHandler      *iiifredirect.Handler
}

func newAPIServer(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, storageSvc *storage.Service, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder, sysCfg *sysconfig.Store, cacheReg *cache.Registry, jobSvc *jobs.Service, licState *licensing.State, storageBackend string) *apiServer {
	s := &apiServer{
		pool:             pool,
		cacheReg:         cacheReg,
		auth:             authHandlerWithPolicy(pool, logger, cfg, sessions, limiter, auditRec, cacheReg, sysCfg),
		resourceType:     assettype.NewHandler(pool, logger),
		storage:          storage.NewHandler(storageSvc, logger),
		assets:           assets.NewHandler(pool, storageSvc, logger, jobSvc, cacheReg, sysCfg),
		subtitles:        subtitles.NewHandler(pool, cacheReg, logger),
		metadata:         metadata.NewHandler(pool, logger, cacheReg),
		collections:      collections.NewHandler(pool, logger, cacheReg),
		posts:            posts.NewHandler(pool, logger, cacheReg),
		teams:            teams.NewHandler(pool, logger, cacheReg),
		tags:             tags.NewHandler(pool, logger),
		users:            usersHandlerWithAudit(pool, logger, cacheReg, auditRec, sessions),
		social:           social.NewHandler(pool, logger, cacheReg),
		trash:            trash.NewHandler(pool, sysCfg, logger),
		setup:            setup.NewHandler(pool, logger, cfg, sysCfg, storageBackend, auditRec),
		workflow:         workflow.NewHandler(pool, logger, cacheReg),
		sysconfigH:       sysconfigHandlerWithAudit(pool, sysCfg, logger, auditRec, cacheReg, cfg.DemoMode, storageSvc),
		jobs:             jobs.NewHTTPHandler(jobSvc, logger),
		jobsSvc:          jobSvc,
		brushpacks:       brushpacks.NewHandler(brushpacks.NewService(pool, storageSvc.Backend)),
		audit:            audit.NewHTTPHandler(pool, logger),
		activity:         audit.NewAccountHandler(pool, logger),
		scheduledActions: scheduledactions.NewHTTPHandler(scheduledactions.NewStore(pool), logger),
		licensing:        licensing.NewHandler(licState, logger),
		userprefs:        userprefs.NewHandler(pool, logger, cacheReg),
		aiAdmin:          newAIAdminHandler(pool, cacheReg),
	}
	// Phase 1.14.A-bridge — assemble the AI bridge aggregator from
	// the assets.Handler (real reader + tag writer) + stubs for the
	// not-yet-shipped writers. Future AI job handlers + admin reconcile
	// surface depend on this struct; assembling it once here keeps
	// the wire site discoverable and lets reviewers see in one place
	// which writers are stubbed vs concrete.
	//
	//   - CaptionWriter   stub  → assets caption schema follow-up
	//   - EmbeddingWriter concrete (Phase 1.14.B) — best-effort load;
	//     falls back to stub on dim-registry failure so boot doesn't
	//     wedge on a misconfigured config row
	//   - TranscriptWriter stub → Phase 1.14.C (Whisper)
	var embedWriter ai.EmbeddingWriter = ai.NewStubEmbeddingWriter()
	var embedReader *aiembeddings.Reader
	if w, err := aiembeddings.NewWriter(context.Background(), pool, logger); err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"ai.embeddings.writer.load.failed",
			slog.String("err", err.Error()),
			slog.String("impact", "EmbeddingWriter returns ErrNotImplementedYet until ai.embedding.dim_registry is fixed"),
		)
	} else {
		embedWriter = w
		// Reader shares the writer's dim_registry so a model bump
		// is visible to both without re-construction.
		embedReader = aiembeddings.NewReader(pool, w.DimRegistry())
	}
	// Inject the reader into the assets handler via the consumer-
	// defined SimilarReader seam — keeps embeddings out of assets'
	// import graph. Adapter converts the local SimilarNeighbour
	// type to the embeddings package's Neighbour.
	if embedReader != nil {
		s.assets.SetSimilarReader(similarReaderAdapter{r: embedReader})
	}
	// Phase 1.14.C — concrete TranscriptWriter. Writes pre-marshalled
	// VTT bytes to storage + upserts the asset_subtitle_tracks row
	// with source_format='whisper'. The orchestration that produces
	// the VTT (extract → chunk → router → stitch → marshal) lives in
	// transcribe.Handler and is invoked by the ai.transcribe job
	// handler — Writer is the smaller bridge contract that any
	// caller with VTT bytes in hand can use.
	transcribeStorage := aitranscribe.NewStorageAdapter(storageSvc)
	transcriptWriter := aitranscribe.NewWriter(transcribeStorage, s.subtitles, logger)

	s.aiBridge = ai.Bridge{
		Lookup:           s.assets,
		TagWriter:        s.assets,
		CaptionWriter:    ai.NewStubCaptionWriter(),
		EmbeddingWriter:  embedWriter,
		TranscriptWriter: transcriptWriter,
	}

	// Phase 1.14.B — wire the AI router + register the clip_local
	// embedding provider. Loader + Caches are constructed identically
	// to newAIAdminHandler (both share the same on-disk config table);
	// they would dedup if held by a shared subsystem object, but for
	// now the doubled construction is cheap (cache.Registry handles
	// the dedup at the row level).
	// Phase 1.16.B-1 — unified search foundation. Engine + Cache
	// (registered with the shared cache.Registry so peer instances
	// receive purges over the existing LISTEN/NOTIFY channel) +
	// Counter (surfaced via /admin/search/health).
	searchCache := search.NewCache(cacheReg, 0, 0, logger)
	searchCounter := search.NewCounter(0)
	searchCounter.SetCacheStatsProvider(func() search.CacheStatsSnapshot { return searchCache.Stats() })
	s.searchService = search.NewService(search.NewEngine(pool), searchCache, searchCounter).
		WithVector(searchvector.NewFetcher(pool))

	// Phase 1.16.B-2 — facet aggregators + trigram suggestions.
	s.facetDispatcher = facet.NewDispatcher(pool, logger)
	s.suggestService = suggest.NewService(pool)

	// Phase 1.16.B-4 — saved searches + email-on-match. Store +
	// Executor + Notifier + Handler + the two job handlers
	// (coordinator, run). Coordinator's initial enqueue happens in
	// server.Run alongside the route mount so a boot that skips
	// HTTP (embedded worker mode) still boots the workers cleanly.
	s.savedSearchStore = saved.NewStore(pool)
	s.savedSearchExecutor = saved.NewExecutor(pool, s.searchService.Engine(), searchvector.NewFetcher(pool))
	if jobSvc != nil {
		s.savedSearchNotifier = saved.NewNotifier(jobSvc)
	}
	// Phase 1.16.B-5 — surface run-now events into the shared
	// search Counter (deferred from B-4). Handler's Counter is
	// nil-safe; the adapter forwards RecordRunResult /
	// RecordDeltaHit / RecordNotificationSent onto the search
	// Counter's Result classes.
	savedCounterAdapter := s.searchService.Counter().AsSavedSearchCounter()
	s.savedSearchHandler = &saved.Handler{
		Store:    s.savedSearchStore,
		Executor: s.savedSearchExecutor,
		Notifier: s.savedSearchNotifier,
		SiteURL:  savedSiteURL(context.Background(), sysCfg),
		Logger:   logger,
		Counter:  savedCounterAdapter,
	}
	// Phase 1.16.B-5 — reindex store + admin handler; job
	// handler registered separately below with the other job
	// handlers.
	s.reindexStore = reindex.NewStore(pool)
	s.reindexHandler = &reindex.Handler{
		Store:  s.reindexStore,
		JobSvc: jobSvc,
		Logger: logger,
	}
	// Phase 1.16.B-5-followup — search feedback loop. Service is
	// wired against a sysconfig-derived Config each call so the
	// runtime toggle (search.feedback.enabled) takes effect on the
	// next request without a restart. Visibility floor uses the same
	// pool-backed asset lookup the /search handler uses.
	s.feedbackStore = feedback.NewStore(pool)
	s.feedbackService = feedback.NewService(
		s.feedbackStore,
		feedbackConfigAdapter{store: sysCfg},
		feedback.PoolVisibility{Pool: pool},
		s.searchService.Counter().AsFeedbackCounter(),
	)
	s.feedbackHandler = &feedback.Handler{
		Service:     s.feedbackService,
		Logger:      logger,
		ScrambleKey: cfg.ScrambleKey,
	}
	s.feedbackAdmin = &feedback.AdminHandler{
		Service: s.feedbackService,
		Auditor: auditRec,
		Logger:  logger,
	}
	// Phase 1.16.B-3-followup-4 — visual-embedding backfill store +
	// admin handler; Job registered alongside reindex.Job below. The
	// Provider pointer is populated by the visual-provider bootstrap
	// block later in this function; the Handler's start endpoint reads
	// it live so the 503-when-unregistered check reflects boot outcome.
	s.visualBackfillStore = visualbackfill.NewStore(pool)
	s.visualBackfillHandler = &visualbackfill.Handler{
		Store:       s.visualBackfillStore,
		JobSvc:      jobSvc,
		Logger:      logger,
		VisualStore: visualstore.New(pool),
		// Provider assigned below after the bootstrap block resolves
		// whether the sidecar is reachable.
	}
	// Phase 1.16.B-3-followup-2 — visualembed Counter + Dispatcher +
	// Job constructed always (nil-safe on Provider) so the /admin/
	// search/health gauge surface + upload-path seam are wired
	// regardless of whether the sidecar registers at boot. Provider
	// + RateLimiter + MaxAttempts fields are assigned inside the
	// provider-bootstrap block below.
	s.visualEmbedCounter = visualembed.NewCounter()
	s.visualEmbedDispatcher = &visualembed.Dispatcher{
		Jobs:    jobSvc,
		Logger:  logger,
		Counter: s.visualEmbedCounter,
		EnabledGetter: func(ctx context.Context) (bool, error) {
			cfg, err := sysCfg.GetSearch(ctx)
			if err != nil {
				return false, err
			}
			return cfg.Visual.Enabled && cfg.Visual.AutoEmbedOnUpload, nil
		},
	}
	s.visualEmbedJob = &visualembed.Job{
		Assets:      visualEmbedAssetAdapter{pool: pool},
		VisualStore: visualstore.New(pool),
		Storage:     visualEmbedStorageAdapter{svc: storageSvc},
		Logger:      logger,
		Counter:     s.visualEmbedCounter,
	}
	// Inject the dispatcher into the assets handler via the consumer-
	// defined VisualEmbedDispatcher seam — keeps visualembed out of
	// the assets package's import graph. Same setter pattern as
	// SetSimilarReader above.
	if s.assets != nil {
		s.assets.SetVisualEmbedDispatcher(visualEmbedDispatcherAdapter{d: s.visualEmbedDispatcher})
	}
	// Phase 1.16.B-5 — disk-usage snapshot + admin handler.
	s.diskUsageCache = searchdiskusage.NewCache(pool, logger)
	s.diskUsageHandler = &searchdiskusage.Handler{
		Cache:  s.diskUsageCache,
		Logger: logger,
	}
	// Phase 1.16.B-5 — pg_stat gauges surface via the shared
	// search Counter. Bridge callback reads the cached snapshot
	// and returns the four gauge fields the health Notes[]
	// section renders.
	s.searchService.Counter().SetGaugeStatsProvider(func() map[string]int64 {
		ctx := context.Background()
		snap, _ := s.diskUsageCache.Get(ctx)
		out := map[string]int64{
			"assets_pending_embedding":    snap.AssetsPendingEmbedding,
			"asset_embedding_row_count":   snap.AssetEmbeddingRowCount,
			"asset_embedding_index_bytes": snap.EmbeddingIndexBytes,
			"saved_search_active":         snap.SavedSearchActive,
			"saved_search_rows":           snap.SavedSearchRows,
			"search_reindex_history_rows": snap.SearchReindexHistoryRows,
		}
		// Phase 1.16.B-3-followup-4 — visual-embedding backfill
		// gauges (closes #200). Best-effort: query failures show as
		// the "-1 = unknown" sentinel so the admin dashboard doesn't
		// hide the tile when the DB briefly misbehaves.
		if s.visualBackfillHandler != nil && s.visualBackfillHandler.VisualStore != nil {
			if n, err := s.visualBackfillHandler.VisualStore.CountVisualEmbeddingBacklog(ctx, visualembed.ImageExtensions()); err == nil {
				out["visual_embedding_backlog"] = n
			} else {
				out["visual_embedding_backlog"] = -1
			}
			if n, err := s.visualBackfillHandler.VisualStore.CountAssetVisualEmbeddings(ctx); err == nil {
				out["visual_embedding_total"] = n
			} else {
				out["visual_embedding_total"] = -1
			}
		}
		if s.visualBackfillStore != nil {
			if _, err := s.visualBackfillStore.ActiveRun(ctx); err == nil {
				out["visual_backfill_active"] = 1
			} else {
				out["visual_backfill_active"] = 0
			}
		}
		// Phase 1.16.B-3-followup-2 — visualembed counter surface
		// (closes #201). Six keys: 4 result counters + rate-limit-
		// wait meter + in-flight gauge. Kept out of the shared
		// search.Counter latency window so embed durations don't
		// skew p50/p95/p99 for search queries.
		if s.visualEmbedCounter != nil {
			for k, v := range s.visualEmbedCounter.Snapshot() {
				out[k] = v
			}
		}
		// Phase 1.16.B-5-followup — feedback active voter gauge
		// (closes #184). Read from the feedback service so the
		// aggregation window (sysconfig-tunable) drives the
		// denominator. Best-effort: query failure surfaces -1.
		if s.feedbackService != nil {
			if n, err := s.feedbackService.ActiveVoters(ctx); err == nil {
				out["search_feedback_active_voters"] = n
			} else {
				out["search_feedback_active_voters"] = -1
			}
		}
		return out
	})
	// Phase 1.16.B-5 — admin saved-searches surface. Same Store
	// + Pool as the user CRUD; distinct auth (system.admin) +
	// distinct routes.
	s.savedSearchAdmin = &saved.AdminHandler{
		Store:  s.savedSearchStore,
		Pool:   pool,
		Logger: logger,
	}

	// Phase 1.54.B — IIIF Presentation API 3.0 + Content Search 2.0
	// + 2.0→3.0 redirect. Single HealthCounter fan-out to all three
	// Phase 1.16.B-3-followup — CLIP visual-encoder sidecar bootstrap.
	// Only touched when sysconfig.search.visual.enabled=true; when
	// unreachable-at-boot the provider stays nil + ByImageHandler
	// serves the 501 sidecar_not_installed stub (backward-compatible
	// with 1.16.B-3). Boot log records the outcome for operators.
	if searchCfg, cfgErr := sysCfg.GetSearch(context.Background()); cfgErr == nil && searchCfg.Visual.Enabled {
		s.visualMaxUploadBytes = int64(searchCfg.Visual.MaxUploadBytes)
		provider := visualprovider.New(
			searchCfg.Visual.SidecarURL,
			time.Duration(searchCfg.Visual.TimeoutMs)*time.Millisecond,
		)
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
		health, bErr := provider.Bootstrap(bootCtx)
		bootCancel()
		switch {
		case bErr != nil:
			logger.LogAttrs(context.Background(), slog.LevelWarn,
				"search.visual.provider.bootstrap_failed",
				slog.String("url", searchCfg.Visual.SidecarURL),
				slog.String("err", bErr.Error()))
		case health.Status != "ok":
			logger.LogAttrs(context.Background(), slog.LevelWarn,
				"search.visual.provider.sidecar_not_ready",
				slog.String("url", searchCfg.Visual.SidecarURL),
				slog.String("status", health.Status),
				slog.String("err", health.Error))
		default:
			s.visualProvider = provider
			if s.visualBackfillHandler != nil {
				s.visualBackfillHandler.Provider = provider
			}
			if s.visualEmbedDispatcher != nil {
				s.visualEmbedDispatcher.Provider = provider
			}
			if s.visualEmbedJob != nil {
				s.visualEmbedJob.Provider = provider
			}
			logger.LogAttrs(context.Background(), slog.LevelInfo,
				"search.visual.provider.registered",
				slog.String("url", searchCfg.Visual.SidecarURL),
				slog.String("model", health.Model),
				slog.Int("dim", health.Dim))
		}
		// Snapshot the backfill knobs so the Job registration site
		// downstream has the operator-tuned values. Reading them here
		// keeps the provider bootstrap block as the single point of
		// truth for search.visual sysconfig consumption.
		s.visualBackfillCfg = visualBackfillJobConfig{
			BatchSize:           int32(searchCfg.Visual.BackfillBatchSize),
			RateLimitPerSecond:  searchCfg.Visual.BackfillRateLimitPerSecond,
			TransientRetryCount: int32(searchCfg.Visual.BackfillTransientRetryCount),
		}
		// Phase 1.16.B-3-followup-2 — same snapshot pattern for the
		// visualembed knobs. Rate limiter is process-shared across
		// every job of this type (single Job struct instance ⇒
		// single limiter). MaxAttempts passes through as
		// (1 + retryCount) — jobs framework counts TOTAL attempts.
		s.visualEmbedCfg = visualEmbedJobConfig{
			RateLimitPerSecond: searchCfg.Visual.AutoEmbedRateLimitPerSecond,
			MaxAttempts:        1 + searchCfg.Visual.AutoEmbedRetryCount,
		}
		if s.visualEmbedJob != nil {
			s.visualEmbedJob.RateLimiter = rate.NewLimiter(rate.Limit(s.visualEmbedCfg.RateLimitPerSecond), 1)
		}
		if s.visualEmbedDispatcher != nil {
			s.visualEmbedDispatcher.MaxAttempts = s.visualEmbedCfg.MaxAttempts
		}
	}

	// sub-package Counter interfaces + healthhandler shim. Manifest
	// cache is registered with cacheReg so peer instances receive
	// invalidations via the existing LISTEN/NOTIFY channel.
	s.iiifCounter = iiif.NewHealthCounter(0)
	s.iiifManifestCache = presentation.NewCache(cacheReg)
	// #935 — an asset PATCH / delete / restore changes the manifest
	// (LoadAsset selects title + description and applies EntityAsset's
	// row predicate), so the assets handler has to be able to evict it.
	// A setter because the cache is built here, after the handler.
	s.assets.SetManifestCache(s.iiifManifestCache)
	s.iiifFedResolver = iiiffederation.NewResolver(pool)
	s.iiifLoader = presentation.NewLoader(pool)
	s.iiifBuilder = presentation.NewBuilder(presentation.BuilderConfig{
		SiteBaseURL: savedSiteURL(context.Background(), sysCfg),
		// Provider fields default via NewBuilder; operators customise
		// via a follow-up sysconfig entry (tracked in the PR body).
	})
	s.iiifPresHandler = &presentation.Handler{
		Loader:     s.iiifLoader,
		Builder:    s.iiifBuilder,
		Federation: s.iiifFedResolver,
		Cache:      s.iiifManifestCache,
		Counter:    s.iiifCounter,
		Logger:     logger,
	}
	s.iiifContentSearchHandler = &contentsearch.Handler{
		Pool:        pool,
		Engine:      s.searchService.Engine(),
		Pairs:       iiifPairAdapter{loader: s.iiifLoader},
		SiteBaseURL: savedSiteURL(context.Background(), sysCfg),
		Counter:     s.iiifCounter,
		Logger:      logger,
	}
	s.iiifRedirectHandler = &iiifredirect.Handler{
		Counter: s.iiifCounter,
	}

	aiCaches := ai.NewCaches(cacheReg)
	aiLoader := ai.NewLoader(pool, aiCaches)
	aiCallAuditor := ai.NewCallAuditor(pool, logger)
	aiBudget := ai.NewTracker(pool, aiCaches, aiLoader, aiCallAuditor)
	s.aiRouter = ai.NewRouter(aiLoader, aiBudget, aiCallAuditor)
	// clip_local is the seed default for ai.routing.embed; register
	// it unconditionally so a fresh install has at least one embed
	// path. Operator overrides (alternate base URL / model / API key)
	// land via the admin UI in a follow-up phase.
	s.aiRouter.Register(aicliplocal.NewProvider(aicliplocal.Config{}, aiCallAuditor))
	// Phase 1.14.C — register the whisper_local transcription
	// provider. Same shape as clip_local: a sibling container per
	// ADR 0034; the operator's enabled=false default in the
	// system_config registration (migration 00001) means the
	// admin UI gates the runtime call until the operator flips it.
	s.aiRouter.Register(aiwhisperlocal.NewProvider(aiwhisperlocal.Config{}, aiCallAuditor))

	// Load AI privacy policy once — the MCP dispatcher + the embed
	// job handler below both consume it. Best-effort; defaults to a
	// no-clamp policy if the config row is missing (pre-migration
	// state) so boot doesn't wedge.
	aiPrivacyCfg, _ := aiLoader.Load(context.Background())

	// Phase 1.53.A — MCP-client subsystem. Each enabled MCP server
	// becomes one ai.Provider in the router (so audit + cost +
	// privacy machinery applies uniformly); the dispatcher exposes
	// the generic mcp.invoke(server, tool, args) entry point with
	// the guard chain (caller cap → tool whitelist + per-tool cap →
	// privacy → budget → call → audit). Health-check goroutine
	// runs per server; spawned in Server.Run alongside the other
	// background workers.
	s.mcpRegistry = mcpregistry.NewRegistry(pool, cacheReg, logger)
	s.mcpProviders = newMCPProviderTable()
	if mcpServers, err := s.mcpRegistry.ListEnabledServers(context.Background()); err == nil {
		for _, mc := range mcpServers {
			prov := mcpserver.NewProvider(mcpserver.Config{
				Name:               mc.Name,
				URL:                mc.URL,
				AuthKind:           mc.AuthKind,
				AuthSecret:         mc.AuthSecretRef, // resolved-in-place for v1; vault lookup is a follow-up
				AuthHeaderName:     mc.AuthHeaderName,
				PrivacyClass:       mc.PrivacyClass,
				RateLimitPerSecond: float64(mc.RateLimitPerSecond),
				RateLimitBurst:     int(mc.RateLimitPerSecond), // burst = per-second by default
			}, aiCallAuditor)
			s.aiRouter.Register(prov)
			s.mcpProviders.add(mc.Name, prov)
		}
	} else {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"mcp.bootwire.list_failed",
			slog.String("err", err.Error()),
			slog.String("impact", "no MCP servers will be available until DB recovers"),
		)
	}
	s.mcpDispatch = mcpdispatch.New(s.mcpRegistry, s.mcpProviders,
		aiBudget, aiCallAuditor, aiPrivacyCfg.Privacy, logger)
	s.mcpHealth = mcpdispatch.NewHealthChecker(s.mcpRegistry, s.mcpProviders, logger)
	s.mcpAdmin = mcpadmin.NewHandler(s.mcpRegistry)

	// Phase 1.14.E-1 — aiedit subsystem boot wire.
	//
	// The ComfyUI-via-MCP provider takes the dispatcher + the
	// operator-configured server name (read once at boot from
	// sysconfig; operator re-points → app restart). The job
	// handler wires the provider against the source-asset reader +
	// derivative writer; both adapters live below. The HTTP
	// handler validates the request synchronously + enqueues the
	// async job.
	aieditCfg, _ := sysCfg.GetAIEdit(context.Background())
	aieditProvider := comfyuimcp.NewProvider(s.mcpDispatch, aieditCfg.ImageEditServer)
	if jobSvc != nil {
		aieditWriter := aiedit.NewDefaultDerivativeWriter(storageSvc, pool)
		jobSvc.Registry.Register(aiedit.NewImg2ImgJobHandler(
			aieditProvider,
			aieditSourceAdapter{pool: pool, storage: storageSvc},
			aieditWriter,
		))
	}
	s.aieditHTTP = aiedit.NewHTTPHandler(
		aieditAssetAdapter{pool: pool},
		aieditConfigAdapter{store: sysCfg},
		jobSvc,
	)

	// Phase 1.14.B — register ai.embed job handler so the worker
	// pool can drain ai.embed jobs enqueued by the asset upload
	// fanout. Uses the aiPrivacyCfg loaded above the MCP block.
	defaultEmbedModel := "nomic-embed-text"
	if jobSvc != nil {
		jobSvc.Registry.Register(aijobs.NewEmbedHandler(
			s.aiRouter,
			s.assets, // ai.AssetLookup (bridge)
			s.aiBridge.EmbeddingWriter,
			aiPrivacyCfg.Privacy,
			defaultEmbedModel,
		))

		// Phase 1.14.C — ai.transcribe handler. The full
		// extract→chunk→route→stitch→VTT→subtitle pipeline lives in
		// transcribe.Handler; the job handler is a thin wrapper that
		// parses the payload + classifies errors. Operator's
		// chunker config (system_config seeds from 00001) flows in
		// via the Config struct.
		transcribeOrch := aitranscribe.NewHandler(
			transcribeStorage, // same storage adapter as the Writer
			s.subtitles,
			s.aiRouter,
			s.assets,
			aiPrivacyCfg.Privacy,
			logger,
			"",                    // tempDir — defaults to os.TempDir()
			aitranscribe.Config{}, // empty → handler picks 25/5 defaults
		)
		jobSvc.Registry.Register(aijobs.NewTranscribeHandler(
			transcribeOrchestratorAdapter{orch: transcribeOrch},
		))
	}

	// Phase 1.18.A-2 — metadata-extraction job handler. Wires
	// the EXIF extractor + the apply layer against DB-backed
	// concrete impls (see meta*Adapter types at the bottom of
	// this file). One ExtractJobHandler per process; the upload
	// handler enqueues metadata.extract jobs per image upload.
	if jobSvc != nil {
		metaSrc := metaSourceAdapter{pool: pool, storage: storageSvc}
		metaLookup := metaAssetAdapter{pool: pool}
		metaCfg := metaConfigAdapter{pool: pool}
		metaValues := metaValueReaderAdapter{pool: pool}
		metaWriter := metaValueWriterAdapter{pool: pool, meta: s.metadata, logger: logger}
		metaFailures := metaFailureAdapter{pool: pool}
		metaApplier := assetmetadata.NewApplier(metaCfg, metaValues, metaWriter, metaFailures)
		// Phase 1.18.A-2 follow-up B (commit 2) — extraction
		// counter wired into the job handler. Surfaced via
		// /admin/metadata-extraction/health below.
		s.metaCounter = assetmetadata.NewCounter()
		jobSvc.Registry.Register(assetmetadata.NewExtractJobHandler(
			metaSrc,
			metaLookup,
			metaApplier,
			metaFailures,
			// Order matters: EXIF first (largest catalog), then IPTC + XMP
			// (overlapping semantics in different namespaces). Operators
			// resolve same-field conflicts via the per-field extraction-
			// config picker shipped in 1.18.A-2 PR-B. PDF + raw extractors
			// (Phase 1.18.A-3.B) own disjoint MIME ranges so their relative
			// order doesn't matter; placed last for readability.
			[]assetmetadata.Extractor{
				exifext.New(),
				iptcext.New(),
				xmpext.New(),
				pdfext.New(),
				rawpkg.New(),
			},
			logger,
		).
			WithCounter(s.metaCounter).
			WithAssetAttributes(assetmetadata.NewPoolAssetAttributeWriter(pool)).
			WithPreviewVariants(assetmetadata.NewStoragePreviewVariantWriter(pool, storageSvc)))
		// Phase 1.18.A-2 follow-up B (commit 4) — coordinator job
		// for operator-initiated re-extract sweeps. Walks active
		// image assets matching the scope + enqueues one
		// metadata.extract child job per eligible asset.
		jobSvc.Registry.Register(assetmetadata.NewBackfillJobHandler(
			pool,
			metaExtractEnqueuer{svc: jobSvc},
			logger,
		))

		// Phase 1.16.B-4 — saved-search coordinator + per-row
		// run handler. Coordinator self-re-enqueues via
		// EnqueueOpts.ScheduledFor; the initial enqueue below
		// kicks off the tick loop at boot.
		if s.savedSearchStore != nil && s.savedSearchExecutor != nil {
			savedCounter := s.searchService.Counter().AsSavedSearchCounter()
			jobSvc.Registry.Register(&saved.CoordinatorJob{
				Store:   s.savedSearchStore,
				Jobs:    jobSvc,
				Logger:  logger,
				Counter: savedCounter,
			})
			jobSvc.Registry.Register(&saved.RunJob{
				Store:    s.savedSearchStore,
				Executor: s.savedSearchExecutor,
				Notifier: s.savedSearchNotifier,
				SiteURL:  savedSiteURL(context.Background(), sysCfg),
				Logger:   logger,
				Counter:  savedCounter,
			})
			// Initial coordinator kick — grid-aligned + idempotency-
			// keyed (same helpers as the self-reschedule) so a boot on
			// each restart collapses onto the already-pending tick
			// instead of stacking a duplicate coordinator (#292).
			nextTick := saved.NextCoordinatorTick(time.Now(), 0)
			if _, err := jobSvc.Enqueue(context.Background(), saved.JobTypeCoordinator, saved.CoordinatorPayload{}, jobs.EnqueueOpts{
				ScheduledFor:   &nextTick,
				IdempotencyKey: saved.CoordinatorTickKey(nextTick),
			}); err != nil {
				logger.LogAttrs(context.Background(), slog.LevelWarn,
					"saved.coordinator.initial_enqueue_error",
					slog.String("err", err.Error()),
				)
			}
		}

		// Phase 1.55.C-1 — soft-delete gc coordinator. Nightly pass
		// hard-deletes rows past sysconfig retention across assets /
		// posts / collections + users in UserStateArchived. Reads
		// sysconfig every tick so operator retention changes take
		// effect on the next pass without a restart. Self-re-enqueues
		// via EnqueueOpts.ScheduledFor; EnsureScheduled kicks the
		// initial tick at boot (idempotent via next-tick timestamp key).
		{
			sdSvc := softdelete.NewService(pool, auditRec)
			// #935 — the hard-delete cache fan-out. This is the one
			// asset write that reaches other domains through the
			// SCHEMA: ON DELETE CASCADE on asset_subtitle_tracks and
			// post_assets empties rows in packages the GC has never
			// heard of, and the in-process LRUs those packages keep go
			// on answering from the pre-delete world until a restart.
			//
			// Listed here, at the composition root, rather than
			// scattered into softdelete/ — one place to read off which
			// caches a hard delete touches, and no dependency
			// inversion on three domain packages.
			//
			// Best-effort by construction: every callee is nil-safe and
			// returns at most an error we log. The delete has already
			// committed; a cache miss-to-evict must not turn a
			// completed GC pass into a failed job.
			sdSvc.OnAssetsHardDeleted = func(ctx context.Context, ids []uuid.UUID) {
				for _, id := range ids {
					// Subtitle tracks: CASCADE wiped the rows, but
					// GetForAsset is read-through and would keep
					// serving the cached slice. This is the call site
					// subtitles/handler.go has claimed in prose since
					// 1.18.B-3 while having zero callers.
					subtitles.InvalidateForAsset(s.subtitles, id)
					// The IIIF manifest is built from the asset row
					// that no longer exists.
					if err := presentation.InvalidateAssetOn(ctx, s.iiifManifestCache, id); err != nil {
						logger.LogAttrs(ctx, slog.LevelWarn, "softdelete.gc.manifest_cache.invalidate.error",
							slog.String("asset_id", id.String()),
							slog.String("err", err.Error()),
						)
					}
					// #920 on a third path. Its two wired call sites
					// are the SOFT delete and the restore; the CASCADE
					// on post_assets drops the membership here too, and
					// nothing was evicting the holding posts.
					if err := posts.InvalidateForAsset(ctx, cacheReg, pool, id); err != nil {
						logger.LogAttrs(ctx, slog.LevelWarn, "softdelete.gc.posts_cache.invalidate.error",
							slog.String("asset_id", id.String()),
							slog.String("err", err.Error()),
						)
					}
				}
			}
			jobSvc.Registry.Register(&softdelete.CoordinatorJob{
				Service:   sdSvc,
				Sysconfig: sysCfg,
				Jobs:      jobSvc,
				Logger:    logger,
			})
			// #467 — nightly audit retention purge. Recurring job on the
			// same queue (NOT the per-target scheduled-action engine — a
			// category purge is a bulk set-delete). Wakes at 03:00 UTC,
			// self-re-enqueues. Reads audit_retention_policy each run so
			// operator changes take effect without a restart.
			jobSvc.Registry.Register(&audit.RetentionJob{
				Pool: pool, Jobs: jobSvc, Rec: auditRec, Logger: logger, GCHourUTC: 3,
			})
			if err := audit.EnsureRetentionScheduled(context.Background(), jobSvc, 3); err != nil {
				logger.LogAttrs(context.Background(), slog.LevelWarn,
					"audit.retention.initial_enqueue_error",
					slog.String("err", err.Error()),
				)
			}
			if err := softdelete.EnsureScheduled(context.Background(), jobSvc, sysCfg); err != nil {
				logger.LogAttrs(context.Background(), slog.LevelWarn,
					"softdelete.gc.initial_enqueue_error",
					slog.String("err", err.Error()),
				)
			}
			s.softdeleteSvc = sdSvc
			// Wire audit + softdelete on the 3 entity handlers so
			// their DELETE handler fires soft_deleted audit + their
			// Restore endpoint can delegate to sdSvc. Handlers still
			// tolerate nil (tests construct without wiring these);
			// production always sets them together with sdSvc.
			s.assets.Audit = auditRec
			s.assets.SoftDelete = sdSvc
			s.posts.Audit = auditRec
			s.posts.SoftDelete = sdSvc
			s.collections.Audit = auditRec
			s.collections.SoftDelete = sdSvc
		}

		// Phase 1.16.B-5 — reindex coordinator + one-shot boot
		// backfill for pre-B-5 federated assets missing embeddings.
		if s.reindexStore != nil {
			jobSvc.Registry.Register(&reindex.Job{
				Pool:   pool,
				Store:  s.reindexStore,
				JobSvc: jobSvc,
				Logger: logger,
			})
			if err := reindex.FederationBackfillOnBoot(context.Background(), pool, s.reindexStore, jobSvc, logger); err != nil {
				logger.LogAttrs(context.Background(), slog.LevelWarn,
					"reindex.boot_backfill.error",
					slog.String("err", err.Error()),
				)
			}
		}
		// Phase 1.16.B-3-followup-4 — visual-embedding backfill
		// coordinator. Always registered so a resumed run finishes
		// even after a boot that lost provider registration mid-flight
		// (the Handle method fails fast when Provider is nil, marks
		// the run "failed", and returns a TerminalError). Storage
		// accessor adapts the storage.Service through the narrow
		// interface the job needs.
		if s.visualBackfillStore != nil {
			jobSvc.Registry.Register(&visualbackfill.Job{
				Pool:                pool,
				Store:               s.visualBackfillStore,
				VisualStore:         visualstore.New(pool),
				Storage:             visualBackfillStorageAdapter{svc: storageSvc},
				Provider:            s.visualProvider,
				Logger:              logger,
				BatchSize:           s.visualBackfillCfg.BatchSize,
				RateLimitPerSecond:  s.visualBackfillCfg.RateLimitPerSecond,
				TransientRetryCount: s.visualBackfillCfg.TransientRetryCount,
			})
		}
		// Phase 1.16.B-3-followup-2 — async visualembed job. Same
		// registration pattern as backfill; Job field values were
		// populated in the provider-bootstrap block above (Provider
		// + RateLimiter). Nil-safe: if the sidecar isn't registered
		// the Handle method returns a transient error and the jobs
		// framework retries until MaxAttempts.
		if s.visualEmbedJob != nil {
			jobSvc.Registry.Register(s.visualEmbedJob)
		}
	}
	// Phase 1.18.A-2 follow-up B (commit 3) — admin failures-queue
	// surface. Always wired so the queue is readable even on
	// installs where the extract job is currently disabled (rows
	// from prior runs stay for audit).
	s.metaAdmin = assetmetadata.NewAdminHandler(pool)
	s.jobsAdmin = jobs.NewAdminHandler(pool)
	s.storageAdmin = storage.NewAdminHandler(pool).WithJobs(jobSvc)
	s.sysCfg = sysCfg

	// Notifications writer + handler (Phase 1.17.I2). The writer is
	// the cross-package entry point every emitter (social, posts,
	// licensing in I2-b, L in 1.17.L) calls. Permission gates
	// (block-checker + prefs-resolver) inject post-construction so
	// the social + userprefs handlers exist first.
	notifWriter := notifications.NewWriter(pool, logger, nil, nil, nil, cacheReg)
	notifWriter.SetBlockChecker(socialBlockAdapter{h: s.social})
	notifWriter.SetPrefsResolver(userprefsPrefsAdapter{h: s.userprefs})
	s.notifications = notifications.NewHandler(pool, logger, notifWriter)
	// Plumb the writer back into the social handler so its
	// comment/like/follow paths can fire notifications. The adapter
	// converts the social-package primitive-args contract into the
	// notifications.Input struct.
	s.social.SetNotifier(socialNotifyAdapter{w: notifWriter})

	// @-mention notifications (Phase 1.55.X). One shared service —
	// parse+resolve+notify — injected into both the posts and comments
	// write paths. Reuses the same notify adapter so mentions flow
	// through the existing block + channel-pref gating, and the
	// resolver's username→ref cache registers on the shared registry.
	mentionSvc := mention.NewService(
		mention.NewResolver(pool, cacheReg),
		socialNotifyAdapter{w: notifWriter},
		logger,
	)
	s.social.SetMentions(mentionSvc)
	s.posts.SetMentions(mentionSvc)

	// #875 — "someone shared a post with you". Same adapter over the
	// same writer, so a share notification goes through the identical
	// block + channel-preference gating every other verb does.
	s.posts.SetNotifier(socialNotifyAdapter{w: notifWriter})

	// #891 — the browse feed's per-user content filters. Reads through
	// the userprefs handler's own LRU (the same one ChannelsFor uses),
	// so a feed page costs a cache hit rather than a query, and a PATCH
	// to /account/preferences invalidates it process-wide + across peers.
	s.posts.SetFeedFilters(userprefsFeedFilterAdapter{h: s.userprefs})

	// Messages handler (Phase 1.17.I-a). Same wiring pattern as
	// notifications + social: nil-constructed for cache, deps
	// injected post-construction.
	s.messages = messages.NewHandler(pool, logger, cacheReg)
	s.messages.SetBlockChecker(socialBlockAdapter{h: s.social})
	s.messages.SetNotifier(socialNotifyAdapter{w: notifWriter})
	s.messages.SetUserExister(socialUserExistsAdapter{h: s.social})

	// Activities ledger writer (Phase 1.22.A-bis-1/2 per ADR 0044).
	// The writer is the cross-package recorder every social
	// handler calls via WithEmission. Reuses the same notification
	// adapter so post-commit notification fan-out fires through
	// the existing notifications.Writer (which already enforces
	// block + channel-pref gating).
	s.activities = activities.NewWriter(pool, logger, cacheReg)
	s.activities.SetNotifier(socialNotifyAdapter{w: notifWriter})

	// #40 sprint 1 — the scheduled-action reaper. A recurring job on
	// the queue that drains due scheduled_actions through five audited
	// executors and self-re-enqueues. Registered here because it needs
	// the notification writer built just above; seeded once at boot via
	// EnsureScheduled (idempotent). The reaper writes audit rows
	// tx-bound with each domain change.
	jobSvc.Registry.Register(&scheduledactions.ReaperJob{
		Pool:     pool,
		Jobs:     jobSvc,
		Rec:      auditRec,
		Notifier: socialNotifyAdapter{w: notifWriter},
		Logger:   logger,
	})
	if err := scheduledactions.EnsureScheduled(context.Background(), jobSvc); err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"scheduled_actions.reap.initial_enqueue_error",
			slog.String("err", err.Error()),
		)
	}
	s.activitiesAdmin = activities.NewAdminHandler(s.activities)

	// Federation peer registry (Phase 1.22.B-a). Two cache
	// domains — per-instance-URL + enabled-snapshot — register
	// with the shared cache.Registry so federated replicas
	// invalidate in lockstep on every peer mutation.
	s.peers = peer.NewRegistry(pool, logger, cacheReg)
	s.peersAdmin = peer.NewAdminHandler(s.peers)

	// Federation instance identity (Phase 1.22.B-b). Singleton
	// Ed25519 keypair, atrest-encrypted, loaded once at boot.
	// Best-effort load — if atrest isn't initialised yet, the
	// federation HTTP surface returns 503 for handshake POSTs +
	// instance doc reads until Load succeeds. We don't fail boot
	// because dev environments without AA_MASTER_KEY should
	// still come up for non-federation features.
	s.fedIdentity = identity.NewManager(pool, logger)
	if _, err := s.fedIdentity.Load(context.Background()); err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"federation.identity.load.failed",
			slog.String("err", err.Error()),
			slog.String("impact", "/federation/instance + handshake POSTs will return 503"),
		)
	}

	// Handshake engine + public + admin surfaces. baseURL +
	// display name read live from sysconfig per request — admin
	// edits show up immediately in the actor doc.
	s.fedEngine = peer.NewEngine(s.peers, s.fedIdentity, nil)
	s.fedEngine.SetLocalBaseURL(sysconfigBaseURLFn(sysCfg)(context.Background()))
	s.fedEngine.SetLocalDisplayName(sysconfigSiteNameFn(sysCfg)(context.Background()))
	s.peersHandshake = peer.NewAdminHandshakeHandler(s.peers, s.fedEngine)
	s.peersPublic = peer.NewPublicHandler(
		s.fedIdentity,
		s.fedEngine,
		s.peers,
		sysconfigBaseURLFn(sysCfg),
		sysconfigSiteNameFn(sysCfg),
	)

	// Peer-of-peer discovery (Phase 1.22.B-d). Suggestions
	// registry + admin handler share the peer registry so dedup
	// against our own peers can happen at query time.
	s.p2pRegistry = p2p.NewRegistry(pool, logger, s.peers, cacheReg)
	p2pClient := p2p.NewClient()
	s.p2pAdmin = p2p.NewAdminHandler(s.p2pRegistry, p2pClient)

	// Federation shares (Phase 1.22.C). Registry caches the
	// per-object active-shares snapshot (the inbox-filter hot
	// path). Admin handler grants/revokes via the write-ahead-
	// audit invariant (audit row commits in the SAME tx as the
	// share row + the aa:Share/aa:Unshare activity row).
	s.sharesRegistry = shares.NewRegistry(pool, logger, cacheReg)
	s.sharesAdmin = shares.NewAdminHandler(
		s.sharesRegistry,
		s.activities,
		auditRec,
		// The owner map lives in the shares package (#893) so the
		// grant path here and the transitive gate path answer "who
		// owns this object" from one place; see shares/owner.go.
		shares.NewObjectOwnerResolver(pool),
		peerLookupFor(s.peers),
		sysconfigBaseURLFn(sysCfg),
		usernameResolverFor(s.users),
	)
	// 1.22.C-d defederation-preview deps: count pending
	// handshakes for the peer + count cached suggestions sourced
	// from the peer + resolve display name/URL for the modal
	// header. Closures over the existing registries.
	s.sharesAdmin.SetDefederationDeps(
		pendingHandshakeCounterFor(pool),
		suggestionCounterFor(pool),
		peerDisplayFor(s.peers),
	)
	// 1.22.C-d expiry sweeper — periodic goroutine, started in
	// Server.Run() alongside the directory poller. Defaults to
	// 1-hour cadence + 500-row batches per the design.
	s.sharesSweeper = shares.NewSweeper(
		shares.DefaultSweeperConfig(),
		s.sharesRegistry,
		s.activities,
		auditRec,
		peerLookupFor(s.peers),
		sysconfigBaseURLFn(sysCfg),
		usernameResolverFor(s.users),
		logger,
	)

	// Federation inbox handler (Phase 1.22.D-a). Mounted via a
	// direct chi route in routes.go (not via openapi/strict-
	// server) — the inbox handler needs raw http.Request +
	// ResponseWriter for body draining, Signature-header
	// parsing, and Retry-After response control which the
	// strict-server shape hides.
	s.inboxHandler = inbox.NewHandler(inbox.HandlerDeps{
		Pool:         inbox.New(pool),
		Lookup:       inboxPeerLookupFor(s.peers),
		Logger:       logger,
		LocalBaseURL: sysconfigBaseURLFn(sysCfg),
		RejectAudit:  inboxRejectAuditFor(auditRec),
	})

	// Federation inbox dispatcher (1.22.D-a-4). Worker
	// goroutine that drains pending federation_inbox rows +
	// invokes the per-verb handler. Started in Server.Run
	// alongside the directory poller + shares sweeper. The
	// SocialPoster + RemoteActorUpserter contracts are wired
	// AFTER construction so the import edges stay clean
	// (inbox does NOT import social).
	s.inboxDispatcher = inbox.NewDispatcher(
		inbox.DefaultDispatcherConfig(),
		inbox.New(pool),
		inboxDispatchPeerLookup(s.peers),
		nil,
		logger,
	)
	s.inboxDispatcher.SetSocialPoster(inboxSocialPosterAdapter{h: s.social})
	// Phase 1.22.I-c: the upsert path now ALSO persists the
	// inbound aa:encryptionPublicKey block via remote.Handler.
	// Boot constructs the Handler with the cache registry so
	// cross-process invalidation fires on every key write; the
	// Recorder is the same one shared across federation surfaces
	// so the resulting federation.remote_actor.key_updated row
	// lands alongside the existing federation audit events.
	remoteHandler := remote.NewHandler(pool, logger, cacheReg)
	s.inboxDispatcher.SetRemoteActorUpserter(remote.NewUpserter(pool, remoteHandler, auditRec, logger))
	s.inboxDispatcher.SetRegistry(inbox.BuildRegistry(s.inboxDispatcher, logger))
	// LISTEN/NOTIFY on federation_inbox_pending — gold-standard
	// sub-1s latency per 1.22.D-b-6 G1. The dispatcher's 30s
	// ticker is correctness backstop only.
	s.inboxDispatcher.SetRawPool(pool)
	// Phase 1.22.I-f stage-4 decrypt-branch wiring. All three
	// hooks nil-safe at the dispatcher level; when any is unwired
	// the encrypted-row path rejects with reject_reason=
	// decrypt_failed + a typed audit reason. Plaintext traffic
	// is unaffected. The remoteHandler reuses the same instance
	// the upserter cached above so a key-write the inbox just
	// stored is immediately visible to the decrypt walk.
	s.inboxDispatcher.SetSenderEncKey(func(ctx context.Context, actorURI string) ([]byte, int32, error) {
		k, err := remoteHandler.GetEncryptionKey(ctx, actorURI)
		if err != nil {
			return nil, 0, err
		}
		return k.Key[:], k.Version, nil
	})
	s.inboxDispatcher.SetRecipientUserRef(recipientUserRefFor(pool))
	s.inboxDispatcher.SetAudit(auditRec)
	// Phase 1.22.I-i — activates the I-h receiver-side
	// encryption policy gate. The lookup resolves "asset"-kind
	// objects to their sensitivity tier (migration 00001); other
	// kinds pass through (SensitivityNotFound). When the gate
	// fires (plaintext envelope + restricted/embargo target),
	// the dispatcher marks the row rejected with reason
	// encryption_required + audits via
	// FederationInboxEncryptionRequiredRejected.
	s.inboxDispatcher.SetSensitivityLookup(inboxSensitivityLookup(pool))

	// Federation OUTBOX dispatcher (Phase 1.22.D-b). Drains
	// activities ledger rows into per-recipient federation_outbox
	// rows via LISTEN/NOTIFY (sub-100ms latency) + 30s ticker
	// backstop. Started in Server.Run alongside the inbox
	// dispatcher.
	//
	// Encryption-supported is hard-false until Phase 1.22.I
	// ships X25519 keypair-per-user; restricted/embargo
	// sensitivity activities emission-skip with audit.
	outboxResolver := outbox.NewResolver(pool, cacheReg,
		func(context.Context) bool { return false })
	// Phase 1.22.I-d capability gate. Dormant in production
	// traffic at I-d (no caller sets Input.RequiresEncryption);
	// the wiring lands now so I-e can flip the flag without
	// touching boot.
	outboxResolver.SetPeerSupportsEncryption(func(ctx context.Context, peerID uuid.UUID) bool {
		p, err := s.peers.ByID(ctx, peerID)
		if err != nil {
			return false
		}
		return p.Capabilities.SupportsE2E()
	})
	outboxResolver.SetEmissionSkippedForPeer(
		func(ctx context.Context, peerID uuid.UUID, reason outbox.SkippedReason, verb string) {
			auditRec.FederationEmissionSkippedForPeer(ctx, peerID.String(), string(reason), verb)
		})
	s.outboxDispatcher = outbox.NewDispatcher(
		outbox.DefaultDispatcherConfig(),
		pool,
		outboxResolver,
		logger,
	)
	s.outboxDispatcher.SetSkippedAudit(auditRec.EmissionSkipped)
	s.outboxDispatcher.SetVisibilityLookup(outboxVisibilityLookup(pool))

	// Federation user-keys retained-row sweeper (Phase 1.22.I-h).
	// Reaps federation_user_keys rows past their retained_until
	// grace window. Ticks every userkeys.SweepTickDefault (1h);
	// audit hook fires federation.user.key_retained_expired once
	// per non-zero reap. Goroutine starts in Server.Run alongside
	// the dispatchers.
	s.userKeysSweeper = userkeys.NewSweeper(
		pool, logger, auditRec.FederationUserKeyRetainedExpired, 0,
	)

	// Phase 1.17.C capability-expiry sweeper. Mirrors userkeys
	// (same tick cadence; same boot/shutdown lifecycle). Reaps
	// expired user_capability_grants + user_capability_revokes;
	// emits per-row audit; broadcasts cache invalidation for every
	// affected user_ref so the resolver picks up the change on the
	// next authz check.
	s.capabilitySweeper = auth.NewCapabilitySweeper(
		pool, logger,
		auditRec.CapabilityGrantExpiredSwept,
		auditRec.CapabilityRevokeExpiredSwept,
		func(ctx context.Context, userRef int64) {
			auth.InvalidateUserCaps(ctx, cacheReg, userRef)
		},
		0,
	)
	// Phase 1.17.E — wire the request-cascade hook so a grant
	// reaped with a non-NULL request_ref also flips the linked
	// resource_request row to expired. Best-effort by contract;
	// failure here logs but doesn't fail the sweep.
	s.requests = requests.NewHandler(pool, logger, cacheReg)
	s.requests.SetAuditRecorder(auditRec)
	s.requests.SetNotifier(socialNotifyAdapter{w: notifWriter})
	// #931 — granting a restoration appeal performs the restore. The
	// adapter lives here rather than in requests/ for the same reason
	// OnAssetsHardDeleted does: the per-kind CACHE fan-out is a fact
	// about which domains cache an asset, and the composition root is
	// where this codebase keeps that.
	//
	// nil-safe: s.softdeleteSvc is only set when the GC block ran, and
	// a Handler with no restorer denies-and-submits normally but
	// refuses to GRANT an appeal (ErrRestoreUnwired) rather than
	// reporting success over an item nothing put back.
	if s.softdeleteSvc != nil {
		s.requests.SetRestorer(restoreAdapter{
			sd:          s.softdeleteSvc,
			assets:      s.assets,
			collections: s.collections,
		})
	}
	s.requestsHTTP = requests.NewHTTPHandler(s.requests, logger)
	s.capabilitySweeper.SetRequestCascade(s.requests.MarkExpired)

	// Admin-curated featured-content list (GitHub #341). Thin,
	// system.admin-gated CRUD over the featured_items table.
	s.featuredDomain = featured.NewHandler(pool, logger)
	s.featuredHTTP = featured.NewHTTPHandler(s.featuredDomain, logger)

	// Operator overrides of shipped UI strings (#794, ADR 0081 §1).
	// The whole map lives behind ONE cache entry registered with the
	// process registry, so a write on this instance invalidates locally
	// and pg_notifies every peer — the boot payload updates without a
	// restart on any of them.
	s.sitetextHTTP = sitetext.NewHTTPHandler(
		sitetext.NewHandler(pool, sitetext.NewCache(cacheReg, logger), logger),
		logger,
	)

	// Operator-authored email templates (#795, ADR 0081 §2). One store
	// is both the render-time override source (installed process-wide so
	// email.Render resolves against it) AND the admin HTTP surface — a
	// single cache registration, so a save invalidates the same entry
	// the next send reads. Installing here rather than in server.go keeps
	// the cache registered exactly once.
	emailTemplateStore := email.NewTemplateStore(pool, email.NewCache(cacheReg, logger), logger)
	email.UseTemplateStore(emailTemplateStore)
	s.emailTemplatesHTTP = email.NewHTTPHandler(emailTemplateStore, logger)

	// Federation user-keys admin + self-rotation HTTP surface
	// (Phase 1.22.I-h). Three endpoints: /account/security/rotate-
	// federation-keys, /admin/federation/key-health,
	// /admin/federation/users/{ref}/rotate-keys.
	s.userKeysAdmin = userkeys.NewAdminHandler(pool, auditRec, logger)

	// Phase 1.18.B-3 subtitle tracks HTTP surface. Three
	// endpoints under /assets/{id}/subtitle-tracks (list / upload /
	// delete). Storage adapter bridges to the same CAS used by
	// original asset bytes.
	s.subtitlesHTTP = subtitles.NewHTTPHandler(
		s.subtitles,
		subtitles.NewStorageAdapter(storageSvc),
		func(ctx context.Context, assetID uuid.UUID, lang string) (uuid.UUID, error) {
			payload := subtitles.BurnPayload{AssetID: assetID, Lang: lang}
			return jobSvc.Enqueue(ctx, jobs.TypeSubtitleBurn, payload, jobs.EnqueueOpts{})
		},
		logger,
	)

	// Federation outbox + inbox admin surface (Phase 1.22.D-c).
	// Owns /admin/federation/outbox + /inbox + the re-queue +
	// cascade-cancel actions. Audit hooks wired to the
	// pool-bound audit.Recorder methods.
	s.outboxAdmin = outbox.NewAdminHandler(
		pool,
		inbox.New(pool),
		auditRec.OutboxRequeued,
		auditRec.PeerCascadeCancelled,
		logger,
	)

	// Demo-seed loader admin endpoints (post-1.22.D dogfood
	// unblock). Gated on system.admin; not surfaced in admin UI.
	// Apply-side script (seed/SEED_INSTRUCTIONS.md) is the only
	// expected caller.
	s.seedAdmin = seed.NewAdminHandler(
		pool,
		auditRec.SeedTimestampsBackfilled,
		auditRec.SeedCommentCreated,
		auditRec.SeedUserCreated,
		// Password hasher closes over the legacy scramble key
		// — same path the setup flow + the bootstrap package
		// use for the initial admin's password.
		func(plaintext string) (string, error) {
			return auth.HashPassword(plaintext, cfg.ScrambleKey)
		},
		// Recorder is wired so SeedCreateUser's federation
		// keypair generation (1.22.I-b) lands a tx-bound
		// federation.user.key_generated audit row alongside the
		// keypair insert.
		auditRec,
	)

	// Federation outbox DELIVERY worker (Phase 1.22.D-b-4).
	// Drains federation_outbox rows → POST /federation/inbox on
	// the recipient peer. HTTP/2 connection pooling +
	// HTTP-Signature signed by the instance Ed25519 key.
	//
	// Signer is resolved LAZILY via a deferred-signer wrapper —
	// the instance identity is loaded by the identity manager
	// AFTER newAPIServer runs (first-run setup may have to
	// generate it). The worker skips delivery cycles when the
	// signer is unavailable + retries on the next tick.
	baseURLFn := sysconfigBaseURLFn(sysCfg)
	signer := &deferredIdentitySigner{
		identity: s.fedIdentity,
		baseURL:  baseURLFn,
	}
	s.outboxDelivery = outbox.NewWorker(
		outbox.DefaultDeliveryConfig(),
		pool,
		signer,
		outboxDeliveryPeerLookup(s.peers),
		logger,
	)
	// Phase 1.22.I-e per-recipient encryption hooks. Three nil-
	// safe setters; missing any input falls back to the existing
	// 1.22.D plaintext path. Production traffic at I-e doesn't
	// actually encrypt because CapNaClBox is removed from
	// KnownCapabilities until I-f ships (rollout coordination
	// per ADR 0049 §Track B); the wiring lands now so I-f's PR
	// is a one-line capability re-add + re-pair trigger.
	s.outboxDelivery.SetPeerSupportsE2E(func(ctx context.Context, peerID uuid.UUID) bool {
		p, err := s.peers.ByID(ctx, peerID)
		if err != nil {
			return false
		}
		return p.Capabilities.SupportsE2E()
	})
	s.outboxDelivery.SetRecipientEncKey(func(ctx context.Context, actorURI string) ([]byte, int32, error) {
		k, err := remoteHandler.GetEncryptionKey(ctx, actorURI)
		if err != nil {
			return nil, 0, err
		}
		return k.Key[:], k.Version, nil
	})
	s.outboxDelivery.SetAudit(auditRec)
	// Cross-package "who owns this post?" lookup so inbound
	// Like/Comment can fire the post-author notification
	// without social importing posts (which already imports
	// social for the follow checker → cycle if reversed).
	s.social.SetPostTargetLookup(postTargetLookupFor(pool))

	// Directory subscriber (Phase 1.22.B-c). The Registry +
	// AdminHandler land here; the background Poller starts in
	// Run() so test fixtures that don't need it can skip it.
	s.directories = directory.NewRegistry(pool, logger, cacheReg)
	dirClient := directory.NewClient(logger)
	s.directoriesAdmin = directory.NewAdminHandler(s.directories, dirClient)
	// Wire publish-side deps (1.22.B-c-bis). nil-safe: subscribe-only
	// installs still work; only the publish endpoints fail with a
	// clear 503 when identity/base-URL aren't configured.
	s.directoriesAdmin.SetPublishDeps(s.fedIdentity, sysconfigBaseURLFn(sysCfg))
	s.directoryPoller = directory.NewPoller(s.directories, dirClient, logger, 5*time.Minute)
	// UsernameResolver: the username-by-ref lookup federation
	// emitters use to build actor URIs. *users.Handler already
	// caches UserPublic; ResolveUsername reuses that cache so the
	// federation hot path (Like/Follow/DM/Block emissions) doesn't
	// slam the user table on every emit. Per docs/spec/federation/
	// v1.md §8.4 usernames are immutable from the federation
	// perspective so cached values stay correct for the actor's
	// lifetime.
	s.activities.SetUsernameResolver(s.users)
	// Handlers that emit need a baseURL to mint actor + activity
	// URIs. Plumb the sysconfig.Site getter so the URL respects
	// runtime config changes (admin updates site.base_url → next
	// emit uses the new value, no restart).
	s.posts.SetActivitiesWriter(s.activities, sysconfigBaseURLFn(sysCfg))
	s.social.SetActivitiesWriter(s.activities, sysconfigBaseURLFn(sysCfg))
	s.messages.SetActivitiesWriter(s.activities, sysconfigBaseURLFn(sysCfg))
	s.collections.SetActivitiesWriter(s.activities, sysconfigBaseURLFn(sysCfg))
	s.users.SetActivitiesWriter(s.activities, sysconfigBaseURLFn(sysCfg))
	// Phase 1.9.B — let collections.Create probe required collection
	// fields + seed initial values via the metadata package.
	s.collections.SetMetadataGate(collectionsMetadataGateAdapter{md: s.metadata})

	// #591 — one cached reader of the operator's CONFIGURED preview
	// ladder, shared by every surface that reports ladder_available.
	// Built once so all four agree on what the ladder is: four
	// independently-constructed readers would be four caches that could
	// disagree for a request or two after an operator edits the config,
	// and a client that saw preview_available and ladder_available
	// disagree across two endpoints would have no way to resolve it.
	ladderReader := sysconfig.NewPreviewLadderReader(sysCfg, cacheReg, logger)
	s.assets.SetPreviewLadder(ladderReader)
	s.posts.SetPreviewLadder(ladderReader)
	s.collections.SetPreviewLadder(ladderReader)
	s.featuredDomain.SetPreviewLadder(ladderReader)
	// #850 — a search hit now carries the same card payload a browse row
	// does, so the search engine is the fifth surface that has to report
	// ladder_available, and it reads the SAME configured ladder. A
	// separately-constructed reader here would be a fifth cache that
	// could disagree with browse for a request or two after an operator
	// edits the config — and the tile the user is looking at would flip
	// between the responsive srcset and the square `col` crop depending
	// on which page they reached it from.
	if s.searchService != nil {
		s.searchService.Engine().SetPreviewLadder(ladderReader)
	}
	return s
}

// collectionsMetadataGateAdapter bridges metadata.Handler to
// collections.MetadataGate so the two packages can stay decoupled —
// neither imports the other. Phase 1.9.B helper.
type collectionsMetadataGateAdapter struct {
	md *metadata.Handler
}

func (a collectionsMetadataGateAdapter) RequiredCollectionFields(ctx context.Context) ([]collections.RequiredField, error) {
	rows, err := a.md.ListRequiredCollectionFieldsRaw(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]collections.RequiredField, 0, len(rows))
	for _, r := range rows {
		out = append(out, collections.RequiredField{
			ID:    uuid.UUID(r.ID.Bytes),
			Code:  r.Code,
			Label: r.Label,
			Type:  r.Type,
		})
	}
	return out, nil
}

func (a collectionsMetadataGateAdapter) UpsertCollectionFieldValueInTx(
	ctx context.Context,
	tx pgx.Tx,
	collectionID, fieldID uuid.UUID,
	raw collections.CollectionFieldValueInput,
	callerRef int64,
) error {
	var options []string
	if raw.ValueOptions != nil {
		options = *raw.ValueOptions
	}
	return a.md.SeedCollectionFieldValueInTx(
		ctx, tx, collectionID, fieldID,
		raw.ValueText, raw.ValueNum, raw.ValueDate,
		options, raw.ValueRef, callerRef,
	)
}

// sysconfigBaseURLFn returns a base-URL resolver closure that
// reads the current Site.BaseURL from sysconfig on each call.
// Cheap because sysconfig itself is cached; ensures emits pick
// up admin-time URL changes without a restart. Empty string
// fallback when sysconfig hasn't been initialized (tests).
func sysconfigBaseURLFn(sysCfg *sysconfig.Store) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		if sysCfg == nil {
			return ""
		}
		site, err := sysCfg.GetSite(ctx)
		if err != nil {
			return ""
		}
		return site.BaseURL
	}
}

// sysconfigSiteNameFn returns a closure resolving sysconfig.Site.Name —
// used as the federation instance display name (Phase 1.22.B-b).
// Empty-string fallback matches sysconfigBaseURLFn for test paths.
func sysconfigSiteNameFn(sysCfg *sysconfig.Store) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		if sysCfg == nil {
			return ""
		}
		site, err := sysCfg.GetSite(ctx)
		if err != nil {
			return ""
		}
		return site.Name
	}
}

// socialUserExistsAdapter satisfies messages' userExister via
// *social.Handler — the public UserExists method is the cached,
// cross-package-safe entry point.
type socialUserExistsAdapter struct{ h *social.Handler }

func (a socialUserExistsAdapter) UserExists(ctx context.Context, ref int64) (bool, error) {
	return a.h.UserExists(ctx, ref)
}

// --- 1.22.C-c federation_shares adapters ---------------------------------

// peerLookupFor wraps peer.Registry.ByID with the projection
// shapes shares needs: id, instance_url, enabled flag, and
// "connected status" derived from PeerStatus.
func peerLookupFor(reg *peer.Registry) shares.PeerLookup {
	return func(ctx context.Context, id uuid.UUID) (shares.PeerInfo, error) {
		p, err := reg.ByID(ctx, id)
		if err != nil {
			return shares.PeerInfo{}, err
		}
		return shares.PeerInfo{
			ID:          p.ID,
			InstanceURL: p.InstanceURL,
			Enabled:     p.Enabled,
			Connected:   p.Status == federation.PeerStatusConnected,
		}, nil
	}
}

// usernameResolverFor wraps users.Handler.ResolveUsername (the
// existing cache-fronted lookup that the federation hot path
// already uses).
func usernameResolverFor(uh *users.Handler) func(ctx context.Context, ref int64) string {
	return func(ctx context.Context, ref int64) string {
		return uh.ResolveUsername(ctx, ref)
	}
}

// --- 1.22.C-d defederation-preview adapters -----------------------------

// pendingHandshakeCounterFor counts pending_outbound +
// pending_inbound rows for one peer. Used by the cascade-preview
// endpoint to render "3 pending handshakes will be cancelled."
// Plain SQL because the peer registry doesn't expose the count
// directly — adding a Registry method would be over-fitting to
// the one caller.
func pendingHandshakeCounterFor(pool *pgxpool.Pool) shares.PendingHandshakeCounter {
	return func(ctx context.Context, peerID uuid.UUID) (int, error) {
		var n int
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*)::INT FROM federation_peers
			 WHERE id = $1 AND status IN ('pending_outbound', 'pending_inbound')`,
			peerID,
		).Scan(&n)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
}

// suggestionCounterFor counts peer-of-peer suggestions sourced
// from a given peer (will be dropped when the peer's row is
// deleted via FK CASCADE on federation_peer_suggestions).
func suggestionCounterFor(pool *pgxpool.Pool) shares.SuggestionCounter {
	return func(ctx context.Context, peerID uuid.UUID) (int, error) {
		var n int
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*)::INT FROM federation_peer_suggestions WHERE source_peer_id = $1`,
			peerID,
		).Scan(&n)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
}

// --- 1.22.D-a federation inbox adapters --------------------------------

// inboxPeerLookupFor wraps peer.Registry's URL-based lookup into
// the inbox handler's PeerLookup contract. The httpsig keyId is
// typically a URL like `https://peer.example/instance#main-key`
// — we trim the fragment + match against the peer's instance_url
// via the existing cache-fronted ByInstanceURL.
func inboxPeerLookupFor(reg *peer.Registry) inbox.PeerLookup {
	return inboxPeerLookupAdapter{reg: reg}
}

type inboxPeerLookupAdapter struct{ reg *peer.Registry }

func (a inboxPeerLookupAdapter) ByKeyID(ctx context.Context, keyID string) (inbox.PeerInfo, error) {
	// Strip the fragment (the "#main-key" part) to recover the
	// base instance URL the peer is registered as.
	base := keyID
	if idx := strings.Index(keyID, "#"); idx >= 0 {
		base = keyID[:idx]
	}
	// The signer constructs keyURL as
	//   `<site.base_url>/federation/instance#main-key`
	// (see deferredIdentitySigner.keyURL). Strip the canonical
	// path tail in full so the base matches the peer's
	// instance_url. The longer suffix is tried first; the
	// shorter `/instance` form is kept for peers that publish
	// their key at `<instance_url>/instance` directly.
	base = strings.TrimSuffix(base, "/federation/instance")
	base = strings.TrimSuffix(base, "/instance")

	p, err := a.reg.ByInstanceURL(ctx, base)
	if err != nil {
		return inbox.PeerInfo{}, inbox.ErrPeerNotFound
	}
	pub, err := federation.PublicKeyFromPEM([]byte(p.InstancePublicKey))
	if err != nil {
		return inbox.PeerInfo{}, inbox.ErrPeerNotFound
	}
	return inbox.PeerInfo{
		ID:                p.ID,
		InstanceURL:       p.InstanceURL,
		InstancePublicKey: pub,
		Enabled:           p.Enabled,
		Connected:         p.Status == federation.PeerStatusConnected,
	}, nil
}

// deferredIdentitySigner resolves the instance identity at Sign
// time rather than at boot time, so the delivery worker can be
// constructed before the identity manager has loaded the key.
// First-run setup generates the identity AFTER newAPIServer
// runs; the worker just no-ops cycles until it shows up.
type deferredIdentitySigner struct {
	identity *identity.Manager
	baseURL  func(ctx context.Context) string
}

func (s *deferredIdentitySigner) Sign(req *http.Request, body []byte) error {
	id, err := s.identity.Get()
	if err != nil || id == nil {
		return fmt.Errorf("federation identity not yet loaded: %w", err)
	}
	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}
	return httpsig.SignAndAttach(req, body, s.keyURL(req.Context()), id.PrivateKey())
}

func (s *deferredIdentitySigner) KeyID() string {
	return s.keyURL(context.Background())
}

func (s *deferredIdentitySigner) keyURL(ctx context.Context) string {
	base := s.baseURL(ctx)
	return strings.TrimRight(base, "/") + "/federation/instance#main-key"
}

// outboxDeliveryPeerLookup returns the closure the delivery
// worker uses to resolve a peer's URL + enabled/connected
// status at send time. Wraps peer.Registry.ByID.
func outboxDeliveryPeerLookup(reg *peer.Registry) outbox.PeerLookup {
	return func(ctx context.Context, peerID uuid.UUID) (outbox.PeerInfo, error) {
		p, err := reg.ByID(ctx, peerID)
		if err != nil {
			return outbox.PeerInfo{}, err
		}
		return outbox.PeerInfo{
			ID:          p.ID,
			InstanceURL: p.InstanceURL,
			Enabled:     p.Enabled,
			Connected:   p.Status == federation.PeerStatusConnected,
		}, nil
	}
}

// outboxVisibilityLookup returns the per-object visibility
// closure the outbox dispatcher uses to drive the resolver.
// Currently only handles posts; extends per-domain when assets
// + collections grow visibility surfaces.
func outboxVisibilityLookup(pool *pgxpool.Pool) outbox.VisibilityLookup {
	return func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		switch kind {
		case "post":
			var v string
			err := pool.QueryRow(ctx,
				`SELECT visibility FROM posts WHERE id = $1 AND deleted_at IS NULL`,
				id,
			).Scan(&v)
			if err != nil {
				return "", nil // unknown post → no recipients
			}
			return outbox.Visibility(v), nil
		}
		// Unknown / unsupported kind → caller treats as
		// VisibilityPrivate (local-only).
		return "", nil
	}
}

// inboxSensitivityLookup returns the per-object sensitivity
// closure the inbox dispatcher's stage-3.5 encryption policy
// gate (1.22.I-h, activated at I-i) consults to decide whether
// a plaintext envelope targeting a local object should be
// rejected with reject_reason=encryption_required.
//
// Resolves only "asset" today — the federation arc's primary
// restricted-tier target. Future kinds (post, collection, …)
// land here as those domains grow sensitivity columns; the
// gate's SensitivityNotFound fallback keeps the dispatcher
// permissive for unrecognised kinds (matches the brief's
// scope-discipline notes).
//
// Lookup errors map to SensitivityNotFound so a missing
// local object is pass-through (the activity's domain handler
// will reject downstream with a more specific reason if the
// object is required). DB errors propagate so the dispatcher
// can fail-the-row + retry on the next tick.
func inboxSensitivityLookup(pool *pgxpool.Pool) inbox.SensitivityLookup {
	return func(ctx context.Context, objectKind string, objectID uuid.UUID) (inbox.Sensitivity, error) {
		switch objectKind {
		case "asset":
			var tier string
			err := pool.QueryRow(ctx,
				`SELECT sensitivity FROM assets WHERE id = $1 AND deleted_at IS NULL`,
				objectID,
			).Scan(&tier)
			if err != nil {
				// pgx.ErrNoRows is the common pre-share case;
				// other errors surface to the dispatcher (which
				// fails-the-row, not rejects). Conflating the
				// two would either silently leak or noisily
				// reject. Distinguish.
				if errors.Is(err, pgx.ErrNoRows) {
					return inbox.SensitivityNotFound, nil
				}
				return "", fmt.Errorf("asset sensitivity lookup: %w", err)
			}
			return inbox.Sensitivity(tier), nil
		}
		// Unknown / unsupported kind — gate passes through.
		// Post + collection + comment land here today; they
		// gain sensitivity columns in their own phases.
		return inbox.SensitivityNotFound, nil
	}
}

// inboxSocialPosterAdapter bridges inbox.SocialPoster (which
// uses inbox.RemoteCommentInput) to social.Handler's
// equivalent shape. The two types are structurally identical;
// duplicated because social can't import inbox (the dispatcher
// already imports social via the SocialPoster contract;
// reversing the edge would cycle).
type inboxSocialPosterAdapter struct{ h *social.Handler }

func (a inboxSocialPosterAdapter) InsertRemoteLike(ctx context.Context, targetKind string, targetID uuid.UUID, peerID uuid.UUID, actorURI string) (bool, error) {
	return a.h.InsertRemoteLike(ctx, targetKind, targetID, peerID, actorURI)
}

func (a inboxSocialPosterAdapter) InsertRemoteComment(ctx context.Context, in inbox.RemoteCommentInput) (uuid.UUID, bool, error) {
	return a.h.InsertRemoteComment(ctx, social.RemoteCommentInput{
		TargetKind:  in.TargetKind,
		TargetID:    in.TargetID,
		ParentID:    in.ParentID,
		PeerID:      in.PeerID,
		ActorURI:    in.ActorURI,
		ActivityURI: in.ActivityURI,
		Body:        in.Body,
	})
}

// inboxDispatchPeerLookup wraps peer.Registry.ByID with the
// inbox.PeerInfo projection the dispatcher needs (URL +
// instance public key — the latter unused by the dispatcher
// for now but kept for future per-actor signature verify in
// 1.22.I).
func inboxDispatchPeerLookup(reg *peer.Registry) func(ctx context.Context, peerID uuid.UUID) (inbox.PeerInfo, error) {
	return func(ctx context.Context, peerID uuid.UUID) (inbox.PeerInfo, error) {
		p, err := reg.ByID(ctx, peerID)
		if err != nil {
			return inbox.PeerInfo{}, err
		}
		var pub []byte
		// PEM parse can fail (placeholder during pending
		// handshake). Tolerate — the dispatcher doesn't need
		// the pubkey for 1.22.D-a-4.
		return inbox.PeerInfo{
			ID:                p.ID,
			InstanceURL:       p.InstanceURL,
			InstancePublicKey: pub,
			Enabled:           p.Enabled,
			Connected:         p.Status == federation.PeerStatusConnected,
		}, nil
	}
}

// recipientUserRefFor returns the inbox-dispatcher hook that
// resolves a recipient actor URI (envelope.To[0]) to the local
// user_ref the receiver-key walk needs. Phase 1.22.I-f.
//
// Actor URIs follow `<base>/users/<username>` per the federation
// spec §8.4. The resolver picks the last non-empty path segment
// + queries auth.users by name; same-host check is the upstream
// inbox-side stage 8 (already validated).
//
// Direct SQL because the inbox package doesn't import auth (would
// pull in HTTP middleware deps the dispatcher doesn't need) +
// the query is single-column, single-row, no joins. Cache hit
// path lives in *users.Handler.UserPublic for the social hot
// path; the dispatcher is off the hot path so an uncached lookup
// per encrypted envelope is acceptable.
func recipientUserRefFor(pool *pgxpool.Pool) inbox.RecipientUserRefFunc {
	return func(ctx context.Context, actorURI string) (int64, error) {
		username := usernameFromActorURI(actorURI)
		if username == "" {
			return 0, errors.New("api: actor URI has no username segment")
		}
		var ref int64
		err := pool.QueryRow(ctx,
			`SELECT ref FROM "user" WHERE LOWER(username) = LOWER($1) LIMIT 1`,
			username,
		).Scan(&ref)
		if err != nil {
			return 0, err
		}
		return ref, nil
	}
}

// usernameFromActorURI parses the last `/users/<username>` segment
// out of an actor URI. Returns "" when the URI doesn't carry the
// expected shape so the caller can surface a typed error.
func usernameFromActorURI(uri string) string {
	const marker = "/users/"
	idx := strings.LastIndex(uri, marker)
	if idx < 0 {
		return ""
	}
	tail := uri[idx+len(marker):]
	// Strip trailing slash + any query/fragment defensively.
	if i := strings.IndexAny(tail, "/?#"); i >= 0 {
		tail = tail[:i]
	}
	return tail
}

// postTargetLookupFor returns the "who owns this post?" closure
// the inbound Like/Comment handlers use for notification routing.
// Implemented as a direct SQL read so social doesn't import
// posts (cycle: posts imports social for the follow checker).
func postTargetLookupFor(pool *pgxpool.Pool) social.PostTargetLookup {
	return func(ctx context.Context, postID uuid.UUID) (int64, bool, error) {
		var ref int64
		err := pool.QueryRow(ctx,
			`SELECT author_user_ref FROM posts WHERE id = $1 AND deleted_at IS NULL`,
			postID,
		).Scan(&ref)
		if err != nil {
			// pgx.ErrNoRows → just "not found"; bubble other
			// errors to the caller (they treat them as transient).
			if err.Error() == "no rows in result set" {
				return 0, false, nil
			}
			return 0, false, err
		}
		return ref, true, nil
	}
}

// inboxRejectAuditFor wraps audit.Recorder.ActivityRejected to
// match the inbox handler's hook signature. activityType is
// pulled from the env when we have it (post-stage-8 rejections);
// for early-stage rejections (stages 2-7) it'd be unknown — we
// pass the reason itself as a fallback so the audit row is still
// queryable.
func inboxRejectAuditFor(rec *audit.Recorder) func(ctx context.Context, peerID uuid.UUID, reason federation.InboxStatus, activityURI, msg string) {
	return func(ctx context.Context, peerID uuid.UUID, reason federation.InboxStatus, activityURI, msg string) {
		rec.ActivityRejected(ctx,
			peerID.String(),
			"", // sourceUserURL — not yet extracted in the inbox path
			"", // activityType — same
			"", // objectKind
			"", // objectID
			string(reason),
			activityURI,
		)
	}
}

// peerDisplayFor resolves peer display_name + URL for the
// preview modal header. Wraps peer.Registry.ByID.
func peerDisplayFor(reg *peer.Registry) shares.PeerDisplay {
	return func(ctx context.Context, id uuid.UUID) (string, string, error) {
		p, err := reg.ByID(ctx, id)
		if err != nil {
			return "", "", err
		}
		return p.DisplayName, p.InstanceURL, nil
	}
}

// --- cross-package adapters for the notifications wiring ----------

// mcpProviderTable is the concrete ProviderRegistry the dispatcher +
// health-check goroutine read from. Holds a copy of every registered
// *mcpserver.Provider, keyed by operator-chosen name. Same lifetime
// as the apiServer — providers are created at boot from the
// mcp_server_registration rows + don't mutate at runtime in v1.
// (Hot-reload on registry changes is a follow-up; for now the
// operator restarts the app to add a new server.)
type mcpProviderTable struct {
	byName map[string]*mcpserver.Provider
}

func newMCPProviderTable() *mcpProviderTable {
	return &mcpProviderTable{byName: map[string]*mcpserver.Provider{}}
}

func (t *mcpProviderTable) add(name string, p *mcpserver.Provider) {
	t.byName[name] = p
}

func (t *mcpProviderTable) Provider(name string) (*mcpserver.Provider, bool) {
	p, ok := t.byName[name]
	return p, ok
}

// transcribeOrchestratorAdapter bridges aitranscribe.Handler to the
// aijobs.TranscribeOrchestrator consumer-defined interface. The
// dual-type pattern keeps ai/jobs out of ai/transcribe's import
// graph (and vice versa) — the boot wire is the only place both
// types meet.
type transcribeOrchestratorAdapter struct {
	orch *aitranscribe.Handler
}

func (a transcribeOrchestratorAdapter) TranscribeAsset(
	ctx context.Context,
	assetID uuid.UUID,
	opts aijobs.TranscribeOrchestratorOpts,
) (aijobs.TranscribeOrchestratorResult, error) {
	track, err := a.orch.TranscribeAsset(ctx, assetID, aitranscribe.TranscribeOpts{
		LanguageHint:  opts.LanguageHint,
		ForceModel:    opts.ForceModel,
		SubtitleLabel: opts.SubtitleLabel,
	})
	if err != nil {
		return aijobs.TranscribeOrchestratorResult{}, err
	}
	// Subtitle FileHash isn't a meaningful number; report the
	// VTT-bytes count as 0 (the orchestrator doesn't bubble that
	// up today). Future enhancement: surface size_bytes from the
	// storage_objects row. Language is the load-bearing field for
	// the job's result payload.
	return aijobs.TranscribeOrchestratorResult{
		Language: track.Lang,
		VTTBytes: 0,
	}, nil
}

// similarReaderAdapter bridges the embeddings package's Reader to
// the assets package's consumer-defined SimilarReader interface.
// The dual-type pattern keeps the two packages decoupled — assets
// can't import embeddings (cycle risk + scope creep), so we wrap
// here at the boot wire.
type similarReaderAdapter struct{ r *aiembeddings.Reader }

func (a similarReaderAdapter) HasEmbedding(ctx context.Context, anchorID uuid.UUID, provider, model, modality string) (bool, error) {
	return a.r.HasEmbedding(ctx, anchorID, provider, model, modality)
}

func (a similarReaderAdapter) FindSimilarByAnchor(ctx context.Context, anchorID uuid.UUID, provider, model, modality string, limit int) ([]assets.SimilarNeighbour, error) {
	ns, err := a.r.FindSimilarByAnchor(ctx, anchorID, provider, model, modality, limit)
	if err != nil {
		return nil, err
	}
	out := make([]assets.SimilarNeighbour, 0, len(ns))
	for _, n := range ns {
		out = append(out, assets.SimilarNeighbour{AssetID: n.AssetID, Distance: n.Distance})
	}
	return out, nil
}

// socialBlockAdapter satisfies notifications' blockChecker via
// *social.Handler — the public HasBlockBetween method is the cached,
// cross-package-safe entry point.
type socialBlockAdapter struct{ h *social.Handler }

func (a socialBlockAdapter) HasBlockBetween(ctx context.Context, x, y int64) (bool, error) {
	return a.h.HasBlockBetween(ctx, x, y)
}

// userprefsPrefsAdapter satisfies notifications' prefsResolver via
// *userprefs.Handler. The handler exposes a cross-package
// ChannelsFor that loads prefs (via cache when available) and
// resolves the channel list for the verb — falling back to the
// per-event system default when the user has no override.
type userprefsPrefsAdapter struct{ h *userprefs.Handler }

func (a userprefsPrefsAdapter) ChannelsFor(ctx context.Context, ref int64, verb string) ([]string, error) {
	return a.h.ChannelsFor(ctx, ref, verb)
}

func (a userprefsPrefsAdapter) CadenceFor(ctx context.Context, ref int64, verb string) (string, error) {
	return a.h.CadenceFor(ctx, ref, verb)
}

// userprefsFeedFilterAdapter satisfies posts' feedFilterReader via
// *userprefs.Handler (#891). Separate from userprefsPrefsAdapter above
// because the two seams answer to different consumers — notifications
// asks "which channels for this verb", posts asks "does this reader
// want restricted members shown" — and bundling them would make the
// posts package depend on a notification-shaped interface.
type userprefsFeedFilterAdapter struct{ h *userprefs.Handler }

func (a userprefsFeedFilterAdapter) ShowRestrictedFeedMembers(ctx context.Context, ref int64) (bool, error) {
	return a.h.ShowRestrictedFeedMembers(ctx, ref)
}

// socialNotifyAdapter satisfies the social package's Notifier
// interface by wrapping the notifications.Writer's typed Input.
type socialNotifyAdapter struct{ w *notifications.Writer }

func (a socialNotifyAdapter) Notify(ctx context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, payload map[string]any) error {
	return a.w.Notify(ctx, notifications.Input{
		RecipientUserRef: recipient,
		ActorUserRef:     actor,
		Verb:             verb,
		TargetKind:       targetKind,
		TargetID:         targetID,
		Payload:          payload,
	})
}

// restoreAdapter satisfies the requests package's restorer interface:
// "put this thing back" for any of the three soft-deletable kinds
// (#931).
//
// Two jobs, and the second is the one worth reading twice.
//
//  1. Dispatch to the matching softdelete primitive — the SAME call the
//     per-kind restore endpoints make, so an appeal and a self-restore
//     write identical state and emit identical audit.
//
//  2. Run the per-kind cache fan-out those endpoints run. This is the
//     part a "just call softdelete" adapter would silently omit, and
//     omitting it is #920: the caches were evicted on DELETE and
//     re-populated WITHOUT the item, so an asset restored without this
//     stays missing from its posts and from its IIIF manifest until the
//     process restarts — while the decider sees a 200 and the DB looks
//     right. The evictions are obtained from the domain handlers rather
//     than restated here, so a domain that grows a new cache updates
//     one place.
//
// Per kind, from the restore endpoints:
//
//	asset      → posts + IIIF manifest (assets.InvalidateAfterRestore)
//	collection → the by-id cache (collections.InvalidateAfterRestore)
//	post       → nothing. posts.Handler.RestorePost evicts nothing, so
//	             neither does this.
type restoreAdapter struct {
	sd          *softdelete.Service
	assets      *assets.Handler
	collections *collections.Handler
}

func (a restoreAdapter) Restore(
	ctx context.Context,
	req *http.Request,
	kind requests.TargetKind,
	id uuid.UUID,
	actorUserRef int64,
) error {
	var err error
	switch kind {
	case requests.TargetKindAsset:
		err = a.sd.RestoreAsset(ctx, req, id, actorUserRef)
	case requests.TargetKindPost:
		err = a.sd.RestorePost(ctx, req, id, actorUserRef)
	case requests.TargetKindCollection:
		err = a.sd.RestoreCollection(ctx, req, id, actorUserRef)
	default:
		return fmt.Errorf("restoreAdapter: unknown kind %q", kind)
	}

	switch {
	case err == nil:
	case errors.Is(err, softdelete.ErrNotDeleted):
		// Already live. Success for the appeal — the requester asked
		// for the item back and it is back — but there is nothing to
		// evict, because whoever restored it ran this fan-out already.
		return requests.ErrTargetAlreadyLive
	case errors.Is(err, softdelete.ErrNotFound):
		// Hard-deleted out from under the pending appeal by the
		// retention GC. Distinct from the case above: nothing comes
		// back, so the decision must not report success.
		return requests.ErrTargetGone
	default:
		return err
	}

	switch kind {
	case requests.TargetKindAsset:
		if a.assets != nil {
			a.assets.InvalidateAfterRestore(ctx, id)
		}
	case requests.TargetKindCollection:
		if a.collections != nil {
			a.collections.InvalidateAfterRestore(ctx, id)
		}
	case requests.TargetKindPost:
		// Nothing, deliberately — see the type comment.
	}
	return nil
}

// usersHandlerWithAudit constructs the users handler + attaches the
// audit recorder. Split out so api.go's struct literal stays
// expression-shaped without an inline statement block (gofmt
// rejects that), and so a future "users handler needs LDAP-bind
// hook too" addition lands in one spot.
func usersHandlerWithAudit(pool *pgxpool.Pool, logger *slog.Logger, cacheReg *cache.Registry, auditRec *audit.Recorder, sessions *auth.SessionManager) *users.Handler {
	h := users.NewHandler(pool, logger, cacheReg)
	h.SetAuditRecorder(auditRec)
	// Phase 1.17.A — wire the session-revocation cascade so a
	// transition out of UserStateActive kills every active session
	// for the subject. nil-safe at the call site; sessions is
	// always non-nil in production boot.
	if sessions != nil {
		h.SetSessionRevoker(sessions.RevokeAllForUser)
	}
	return h
}

// sysconfigHandlerWithAudit mirrors usersHandlerWithAudit — wires
// the audit recorder so Phase 1.17.D's RecordChange call sites in
// the Update* config handlers have somewhere to emit to.
func sysconfigHandlerWithAudit(pool *pgxpool.Pool, store *sysconfig.Store, logger *slog.Logger, auditRec *audit.Recorder, cacheReg *cache.Registry, demoMode bool, storageSvc *storage.Service) *sysconfig.Handler {
	h := sysconfig.NewHTTPHandler(pool, store, logger)
	h.SetAuditRecorder(auditRec)
	// #517 — the instance logo is the one setting whose value is a
	// blob, so this handler needs the byte plane as well as the
	// config store.
	h.SetStorage(storageSvc)
	// #445 — the public-mode write invalidates the auth middleware's
	// cached read of the flag. Without this the toggle appears inert
	// until the cache entry ages out.
	h.CacheReg = cacheReg
	// #709 — the public /browse-views read sits on the frontend's boot
	// path, so it reads through the same NOTIFY-fed cache registry the
	// admin write above invalidates.
	h.SetBrowseViewsReader(sysconfig.NewBrowseViewsReader(store, cacheReg, logger))
	h.DemoMode = demoMode
	return h
}

// authHandlerWithPolicy mirrors usersHandlerWithAudit — composes the
// post-construction setters so the existing positional NewHandler
// signature doesn't need to grow another arg.
func authHandlerWithPolicy(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder, cacheReg *cache.Registry, sysCfg *sysconfig.Store) *auth.Handler {
	h := auth.NewHandler(pool, logger, cfg.ScrambleKey, 0, sessions, limiter, auditRec, cacheReg)
	h.SetPasswordPolicySource(passwordPolicyAdapter{store: sysCfg})
	// Phase 1.19.D — wire the persistent per-username lockout manager.
	// Cache lives on the shared cache.Registry so LISTEN/NOTIFY
	// broadcasts across instances. Policy reads from sysconfig on
	// every call so operator retunes take effect without restart.
	// IP-subnet-hash salt piggybacks on the auth scramble key —
	// same secret bucket, rotated together, and not surfaced to
	// audit consumers (only the digest is written).
	lockoutCache := lockout.NewCache(cacheReg)
	lockoutMgr := lockout.NewManager(pool, logger)
	lockoutMgr.Cache = lockoutCache
	lockoutMgr.Policy = lockoutPolicyAdapter{store: sysCfg}.Get
	lockoutMgr.Auditor = lockoutAuditAdapter{rec: auditRec}
	h.SetLockoutManager(lockoutMgr, cfg.ScrambleKey)
	// #567 — shared-account demo installs scope session reads/revokes
	// to the requesting session.
	h.DemoMode = cfg.DemoMode
	return h
}

// lockoutPolicyAdapter bridges *sysconfig.Store → lockout.PolicyProvider.
// Zero-valued knobs (unset by operator) fall back to lockout.DefaultConfig
// via the manager's currentPolicy nil-check.
type lockoutPolicyAdapter struct{ store *sysconfig.Store }

func (a lockoutPolicyAdapter) Get(ctx context.Context) lockout.Config {
	cfg, err := a.store.GetAuth(ctx)
	if err != nil {
		return lockout.DefaultConfig
	}
	return lockout.Config{
		Threshold:       cfg.Lockout.Threshold,
		DurationMinutes: cfg.Lockout.DurationMinutes,
	}
}

// lockoutAuditAdapter bridges *audit.Recorder → lockout.AuditEmitter.
// The Manager doesn't have an *http.Request in scope (it runs from
// the query path); we pass nil, which the Recorder's ctxFromRequest
// helper treats as a no-request event (ip / user_agent fields
// omitted from the audit row).
type lockoutAuditAdapter struct{ rec *audit.Recorder }

func (a lockoutAuditAdapter) LockoutTriggered(ctx context.Context, userRef int64, failedCount, threshold, durationMinutes int32, ipSubnetHash string) {
	if a.rec == nil {
		return
	}
	a.rec.AuthLockoutTriggered(ctx, nil, userRef, failedCount, threshold, durationMinutes, ipSubnetHash)
}

func (a lockoutAuditAdapter) LockoutCleared(ctx context.Context, userRef int64, adminUserRef *int64, priorFailedCount int32, source string) {
	if a.rec == nil {
		return
	}
	a.rec.AuthLockoutCleared(ctx, nil, userRef, adminUserRef, priorFailedCount, source)
}

// passwordPolicyAdapter bridges *sysconfig.Store → the auth handler's
// passwordPolicySource interface. Lives here (not in auth or sysconfig)
// to keep the package dependency graph unidirectional.
type passwordPolicyAdapter struct{ store *sysconfig.Store }

func (a passwordPolicyAdapter) GetPasswordPolicy(ctx context.Context) (auth.PasswordPolicy, error) {
	cfg, err := a.store.GetAuth(ctx)
	if err != nil {
		return auth.PasswordPolicy{}, err
	}
	return auth.PasswordPolicy{
		MinLength:      cfg.PasswordPolicy.MinLength,
		RequireUpper:   cfg.PasswordPolicy.RequireUpper,
		RequireNumber:  cfg.PasswordPolicy.RequireNumber,
		RequireSymbol:  cfg.PasswordPolicy.RequireSymbol,
		DisallowCommon: cfg.PasswordPolicy.DisallowCommon,
		MaxAgeDays:     cfg.PasswordPolicy.MaxAgeDays,
	}, nil
}

// visualBackfillJobConfig carries the sysconfig-derived Job knobs from
// the boot-time bootstrap block down to the Job registration site.
// Kept as a struct on the apiServer so the two code sites read the
// same shape (Job takes int32 batch size + float64 rps + int32 retry).
type visualBackfillJobConfig struct {
	BatchSize           int32
	RateLimitPerSecond  float64
	TransientRetryCount int32
}

// visualBackfillStorageAdapter bridges *storage.Service → the narrow
// visualbackfill.StorageAccessor interface. Lives here so the job
// package stays free of a storage import.
type visualBackfillStorageAdapter struct{ svc *storage.Service }

func (a visualBackfillStorageAdapter) Download(ctx context.Context, hash, variant string) (io.ReadCloser, visualbackfill.StorageObjectInfo, error) {
	rc, info, err := a.svc.Download(ctx, hash, variant)
	if err != nil {
		return nil, visualbackfill.StorageObjectInfo{}, err
	}
	out := visualbackfill.StorageObjectInfo{}
	if info != nil {
		out.ContentType = info.ContentType
		out.SizeBytes = info.Size
	}
	return rc, out, nil
}

// visualEmbedJobConfig carries the sysconfig-derived visualembed Job
// knobs from the boot-time bootstrap block down to the Job
// registration site. Parallel shape to visualBackfillJobConfig above.
type visualEmbedJobConfig struct {
	RateLimitPerSecond float64
	MaxAttempts        int
}

// visualEmbedStorageAdapter bridges *storage.Service → the narrow
// visualembed.StorageAccessor interface. Same pattern as
// visualBackfillStorageAdapter (kept separate so a future protocol
// divergence between backfill + auto-embed doesn't cross-contaminate).
type visualEmbedStorageAdapter struct{ svc *storage.Service }

func (a visualEmbedStorageAdapter) Download(ctx context.Context, hash, variant string) (io.ReadCloser, visualembed.StorageObjectInfo, error) {
	rc, info, err := a.svc.Download(ctx, hash, variant)
	if err != nil {
		return nil, visualembed.StorageObjectInfo{}, err
	}
	out := visualembed.StorageObjectInfo{}
	if info != nil {
		out.ContentType = info.ContentType
		out.SizeBytes = info.Size
	}
	return rc, out, nil
}

// visualEmbedAssetAdapter bridges the pool → the narrow
// visualembed.AssetLookup interface. Reads file_hash + file_extension +
// deleted_at with a single SELECT so the job's Handle path adds one
// row-scan per execution.
//
// file_extension rather than has_image (#579): image-ness is the file
// FORMAT, and the column this used to select had no writer, so the job
// it feeds rejected every asset as "not an image".
type visualEmbedAssetAdapter struct{ pool *pgxpool.Pool }

func (a visualEmbedAssetAdapter) Get(ctx context.Context, id uuid.UUID) (visualembed.AssetRecord, error) {
	var (
		fileHash  *string
		fileExt   *string
		deletedAt *time.Time
	)
	err := a.pool.QueryRow(ctx, `
		SELECT file_hash, file_extension, deleted_at
		  FROM assets
		 WHERE id = $1
	`, id).Scan(&fileHash, &fileExt, &deletedAt)
	if err != nil {
		return visualembed.AssetRecord{}, err
	}
	return visualembed.AssetRecord{
		FileHash:      fileHash,
		FileExtension: fileExt,
		Deleted:       deletedAt != nil,
	}, nil
}

// feedbackConfigAdapter bridges *sysconfig.Store → the narrow
// feedback.ConfigProvider interface. Reads the search key + hoists
// the Feedback subsection out with defaults applied by GetSearch.
// The pointer-Enabled semantic (nil = default on) is resolved via
// sysconfig.FeedbackConfig.FeedbackEnabled().
type feedbackConfigAdapter struct{ store *sysconfig.Store }

func (a feedbackConfigAdapter) Get(ctx context.Context) (feedback.Config, error) {
	cfg, err := a.store.GetSearch(ctx)
	if err != nil {
		return feedback.Config{}, err
	}
	return feedback.Config{
		Enabled:               cfg.Feedback.FeedbackEnabled(),
		MaxPerUserPerDay:      cfg.Feedback.MaxPerUserPerDay,
		AggregationWindowDays: cfg.Feedback.AggregationWindowDays,
	}, nil
}

// visualEmbedDispatcherAdapter bridges *visualembed.Dispatcher →
// assets.VisualEmbedDispatcher. The interface + local input type
// live in assets/handler.go (consumer-defined seam); this adapter
// converts between the two so the assets package doesn't import
// visualembed.
type visualEmbedDispatcherAdapter struct{ d *visualembed.Dispatcher }

func (a visualEmbedDispatcherAdapter) Dispatch(ctx context.Context, in assets.VisualEmbedInput) {
	if a.d == nil {
		return
	}
	a.d.Dispatch(ctx, visualembed.DispatchInput{
		AssetID:       in.AssetID,
		FileExtension: in.FileExtension,
	})
}

// --- jobs ------------------------------------------------------------------

func (s *apiServer) ClaimJobs(ctx context.Context, req openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	return s.jobs.ClaimJobs(ctx, req)
}
func (s *apiServer) GetJob(ctx context.Context, req openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	return s.jobs.GetJob(ctx, req)
}
func (s *apiServer) HeartbeatJob(ctx context.Context, req openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	return s.jobs.HeartbeatJob(ctx, req)
}
func (s *apiServer) CompleteJob(ctx context.Context, req openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	return s.jobs.CompleteJob(ctx, req)
}
func (s *apiServer) FailJob(ctx context.Context, req openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	return s.jobs.FailJob(ctx, req)
}

// --- auth ------------------------------------------------------------------

func (s *apiServer) Login(ctx context.Context, req openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	return s.auth.Login(ctx, req)
}

func (s *apiServer) ListIdentityProviders(ctx context.Context, req openapi.ListIdentityProvidersRequestObject) (openapi.ListIdentityProvidersResponseObject, error) {
	return s.auth.ListIdentityProviders(ctx, req)
}

func (s *apiServer) Logout(ctx context.Context, req openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	return s.auth.Logout(ctx, req)
}

func (s *apiServer) GetCurrentUser(ctx context.Context, req openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	return s.auth.GetCurrentUser(ctx, req)
}

func (s *apiServer) ListApiTokens(ctx context.Context, req openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	return s.auth.ListApiTokens(ctx, req)
}

func (s *apiServer) CreateApiToken(ctx context.Context, req openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	return s.auth.CreateApiToken(ctx, req)
}

func (s *apiServer) RevokeApiToken(ctx context.Context, req openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	return s.auth.RevokeApiToken(ctx, req)
}
func (s *apiServer) ListMySessions(ctx context.Context, req openapi.ListMySessionsRequestObject) (openapi.ListMySessionsResponseObject, error) {
	return s.auth.ListMySessions(ctx, req)
}
func (s *apiServer) RevokeMySession(ctx context.Context, req openapi.RevokeMySessionRequestObject) (openapi.RevokeMySessionResponseObject, error) {
	return s.auth.RevokeMySession(ctx, req)
}
func (s *apiServer) ListAdminUserSessions(ctx context.Context, req openapi.ListAdminUserSessionsRequestObject) (openapi.ListAdminUserSessionsResponseObject, error) {
	return s.auth.ListAdminUserSessions(ctx, req)
}
func (s *apiServer) RevokeAdminUserSession(ctx context.Context, req openapi.RevokeAdminUserSessionRequestObject) (openapi.RevokeAdminUserSessionResponseObject, error) {
	return s.auth.RevokeAdminUserSession(ctx, req)
}
func (s *apiServer) ChangeMyPassword(ctx context.Context, req openapi.ChangeMyPasswordRequestObject) (openapi.ChangeMyPasswordResponseObject, error) {
	return s.auth.ChangeMyPassword(ctx, req)
}
func (s *apiServer) AdminResetUserPassword(ctx context.Context, req openapi.AdminResetUserPasswordRequestObject) (openapi.AdminResetUserPasswordResponseObject, error) {
	return s.auth.AdminResetUserPassword(ctx, req)
}
func (s *apiServer) AdminImpersonateUser(ctx context.Context, req openapi.AdminImpersonateUserRequestObject) (openapi.AdminImpersonateUserResponseObject, error) {
	return s.auth.AdminImpersonateUser(ctx, req)
}
func (s *apiServer) AdminUnlockAccount(ctx context.Context, req openapi.AdminUnlockAccountRequestObject) (openapi.AdminUnlockAccountResponseObject, error) {
	return s.auth.AdminUnlockAccount(ctx, req)
}
func (s *apiServer) EndImpersonation(ctx context.Context, req openapi.EndImpersonationRequestObject) (openapi.EndImpersonationResponseObject, error) {
	return s.auth.EndImpersonation(ctx, req)
}
func (s *apiServer) GetMyTOTP(ctx context.Context, req openapi.GetMyTOTPRequestObject) (openapi.GetMyTOTPResponseObject, error) {
	return s.auth.GetMyTOTP(ctx, req)
}
func (s *apiServer) EnrollMyTOTP(ctx context.Context, req openapi.EnrollMyTOTPRequestObject) (openapi.EnrollMyTOTPResponseObject, error) {
	return s.auth.EnrollMyTOTP(ctx, req)
}
func (s *apiServer) ConfirmMyTOTP(ctx context.Context, req openapi.ConfirmMyTOTPRequestObject) (openapi.ConfirmMyTOTPResponseObject, error) {
	return s.auth.ConfirmMyTOTP(ctx, req)
}
func (s *apiServer) DisableMyTOTP(ctx context.Context, req openapi.DisableMyTOTPRequestObject) (openapi.DisableMyTOTPResponseObject, error) {
	return s.auth.DisableMyTOTP(ctx, req)
}
func (s *apiServer) RegenerateMyRecoveryCodes(ctx context.Context, req openapi.RegenerateMyRecoveryCodesRequestObject) (openapi.RegenerateMyRecoveryCodesResponseObject, error) {
	return s.auth.RegenerateMyRecoveryCodes(ctx, req)
}
func (s *apiServer) Register(ctx context.Context, req openapi.RegisterRequestObject) (openapi.RegisterResponseObject, error) {
	return s.auth.Register(ctx, req)
}
func (s *apiServer) VerifyEmail(ctx context.Context, req openapi.VerifyEmailRequestObject) (openapi.VerifyEmailResponseObject, error) {
	return s.auth.VerifyEmail(ctx, req)
}
func (s *apiServer) ResendVerificationEmail(ctx context.Context, req openapi.ResendVerificationEmailRequestObject) (openapi.ResendVerificationEmailResponseObject, error) {
	return s.auth.ResendVerificationEmail(ctx, req)
}

func (s *apiServer) ListCapabilities(ctx context.Context, req openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	return s.auth.ListCapabilities(ctx, req)
}

func (s *apiServer) ListRoles(ctx context.Context, req openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	return s.auth.ListRoles(ctx, req)
}

func (s *apiServer) GetMyCapabilities(ctx context.Context, req openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	return s.auth.GetMyCapabilities(ctx, req)
}

func (s *apiServer) SetUserRole(ctx context.Context, req openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	return s.auth.SetUserRole(ctx, req)
}

func (s *apiServer) ListAdminUserCapabilities(ctx context.Context, req openapi.ListAdminUserCapabilitiesRequestObject) (openapi.ListAdminUserCapabilitiesResponseObject, error) {
	return s.auth.ListAdminUserCapabilities(ctx, req)
}
func (s *apiServer) AddAdminUserGrant(ctx context.Context, req openapi.AddAdminUserGrantRequestObject) (openapi.AddAdminUserGrantResponseObject, error) {
	return s.auth.AddAdminUserGrant(ctx, req)
}
func (s *apiServer) RemoveAdminUserGrant(ctx context.Context, req openapi.RemoveAdminUserGrantRequestObject) (openapi.RemoveAdminUserGrantResponseObject, error) {
	return s.auth.RemoveAdminUserGrant(ctx, req)
}
func (s *apiServer) AddAdminUserRevoke(ctx context.Context, req openapi.AddAdminUserRevokeRequestObject) (openapi.AddAdminUserRevokeResponseObject, error) {
	return s.auth.AddAdminUserRevoke(ctx, req)
}
func (s *apiServer) RemoveAdminUserRevoke(ctx context.Context, req openapi.RemoveAdminUserRevokeRequestObject) (openapi.RemoveAdminUserRevokeResponseObject, error) {
	return s.auth.RemoveAdminUserRevoke(ctx, req)
}

// --- asset_types --------------------------------------------------------

func (s *apiServer) ListAssetTypes(ctx context.Context, req openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	return s.resourceType.ListAssetTypes(ctx, req)
}
func (s *apiServer) ListAssetTypeAcls(ctx context.Context, req openapi.ListAssetTypeAclsRequestObject) (openapi.ListAssetTypeAclsResponseObject, error) {
	return s.resourceType.ListAssetTypeAcls(ctx, req)
}
func (s *apiServer) AddAssetTypeAcl(ctx context.Context, req openapi.AddAssetTypeAclRequestObject) (openapi.AddAssetTypeAclResponseObject, error) {
	return s.resourceType.AddAssetTypeAcl(ctx, req)
}
func (s *apiServer) RemoveAssetTypeAcl(ctx context.Context, req openapi.RemoveAssetTypeAclRequestObject) (openapi.RemoveAssetTypeAclResponseObject, error) {
	return s.resourceType.RemoveAssetTypeAcl(ctx, req)
}

// --- storage (raw byte plane) ----------------------------------------------

func (s *apiServer) UploadStorageObject(ctx context.Context, req openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	return s.storage.UploadStorageObject(ctx, req)
}

func (s *apiServer) DownloadStorageObject(ctx context.Context, req openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	return s.storage.DownloadStorageObject(ctx, req)
}

func (s *apiServer) DownloadStorageObjectVariant(ctx context.Context, req openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	return s.storage.DownloadStorageObjectVariant(ctx, req)
}

// --- assets (entity layer) -------------------------------------------------

func (s *apiServer) CreateAsset(ctx context.Context, req openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	resp, err := s.assets.CreateAsset(ctx, req)
	// New asset: no IIIF manifest existed to invalidate. Search
	// cache still needs the drop for the new-row-appears case.
	s.invalidateSearchOnAssetWrite(ctx, uuid.Nil, err)
	return resp, err
}

func (s *apiServer) ListAssets(ctx context.Context, req openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	return s.assets.ListAssets(ctx, req)
}

func (s *apiServer) GetAsset(ctx context.Context, req openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	resp, err := s.assets.GetAsset(ctx, req)
	if err != nil {
		return nil, err
	}
	// Phase 1.18.B-3: splice subtitle_tracks into the response.
	// Read-through is cheap (cache-fronted, returns [] for
	// non-applicable assets), so no asset-kind pre-check here.
	if r, ok := resp.(openapi.GetAsset200JSONResponse); ok && s.subtitles != nil {
		tracks, terr := s.subtitles.GetForAsset(ctx, uuid.UUID(req.Id))
		if terr == nil && len(tracks) > 0 {
			apiTracks := make([]openapi.SubtitleTrack, len(tracks))
			for i, t := range tracks {
				apiTracks[i] = subtitles.TrackToAPI(t)
			}
			r.SubtitleTracks = &apiTracks
			return r, nil
		}
	}
	return resp, nil
}

func (s *apiServer) UpdateAsset(ctx context.Context, req openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	resp, err := s.assets.UpdateAsset(ctx, req)
	s.invalidateSearchOnAssetWrite(ctx, uuid.UUID(req.Id), err)
	return resp, err
}

func (s *apiServer) DeleteAsset(ctx context.Context, req openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	resp, err := s.assets.DeleteAsset(ctx, req)
	s.invalidateSearchOnAssetWrite(ctx, uuid.UUID(req.Id), err)
	return resp, err
}

// RestoreAsset — Phase 1.55.C-1b. Delegates to the assets handler
// which enforces admin capability + calls softdelete.Service.
func (s *apiServer) RestoreAsset(ctx context.Context, req openapi.RestoreAssetRequestObject) (openapi.RestoreAssetResponseObject, error) {
	resp, err := s.assets.RestoreAsset(ctx, req)
	// Restore un-hides a row; the same search-cache invalidator
	// applies (the row is once again live in query results).
	s.invalidateSearchOnAssetWrite(ctx, uuid.UUID(req.Id), err)
	return resp, err
}

// invalidateSearchOnAssetWrite drops the query-result cache when a
// successful asset write commits. Broadcasts to peers over the
// existing cache_invalidate LISTEN/NOTIFY channel. Skipped on
// error so a failed request doesn't churn the cache. Nil-safe when
// the search subsystem is disabled.
//
// Phase 1.54.B: also drops the IIIF ManifestCache for the asset so
// the next /iiif/3/asset/{id}/manifest.json request re-renders with
// the updated metadata / GPS / sensitivity / embargo state. Same
// LISTEN/NOTIFY channel — peers receive the invalidation over the
// same cache_invalidate broadcast, targeted at the iiif.presentation
// domain rather than the search-cache domain.
//
// Phase 1.55.U-2 §7.1: targeted per-asset IIIF invalidation. When
// assetID is non-Nil, only that asset's IIIF manifest entries drop
// (both anonymous + authenticated variants). Nil signals "no
// specific asset" (CreateAsset) — no IIIF manifest can exist yet
// so InvalidateAsset would be a no-op; skip the IIIF call. Prior
// implementation blindly bulk-purged the entire IIIF ManifestCache
// on every asset write, which wiped every other asset's + every
// collection's manifest for no functional reason.
func (s *apiServer) invalidateSearchOnAssetWrite(ctx context.Context, assetID uuid.UUID, err error) {
	if err != nil {
		return
	}
	if s.searchService != nil && s.searchService.Cache() != nil {
		s.searchService.Cache().InvalidateOnAssetWrite(ctx, assetID)
	}
	if s.iiifManifestCache != nil && assetID != uuid.Nil {
		_ = s.iiifManifestCache.InvalidateAsset(ctx, assetID)
	}
}

// invalidateSearchOnCollectionWrite — same as above for collections.
//
// Phase 1.55.U-2 §7.2: targeted per-collection IIIF invalidation.
// Prior implementation bulk-purged the ManifestCache on every
// collection write, wiping every asset's manifest along with the
// collections. Because collection manifests may include member
// asset manifests via IIIF Collection embedding, targeted
// invalidation drops the specific collection's entries only; the
// member assets keep their independent cache slots. When
// collectionID is Nil (create-with-no-known-id path), the IIIF call
// is skipped — no manifest can exist for a not-yet-created
// collection.
func (s *apiServer) invalidateSearchOnCollectionWrite(ctx context.Context, collectionID uuid.UUID, err error) {
	if err != nil {
		return
	}
	if s.searchService != nil && s.searchService.Cache() != nil {
		s.searchService.Cache().InvalidateOnCollectionWrite(ctx, collectionID)
	}
	if s.iiifManifestCache != nil && collectionID != uuid.Nil {
		_ = s.iiifManifestCache.InvalidateCollection(ctx, collectionID)
	}
}

// invalidateSearchOnPostWrite — same for posts. Posts have no IIIF
// Presentation surface (yet); the ManifestCache is untouched.
func (s *apiServer) invalidateSearchOnPostWrite(ctx context.Context, err error) {
	if err != nil || s.searchService == nil || s.searchService.Cache() == nil {
		return
	}
	s.searchService.Cache().InvalidateOnPostWrite(ctx, uuid.Nil)
}

// invalidateOwnerProfileOnCollectionWrite drops the create-time
// owner's profile cache so their "N collections owned" tile
// refreshes. Phase 1.55.U-2 §7.2: mirrors the posts.InvalidateProfile
// call posts/handler.go already fires on post creation. The
// req.Body carries the owner_user_ref for the new collection.
//
// Nil-safe: if the create failed OR the body doesn't identify an
// owner (Body-nil in odd request shapes), silently skip. The
// profile cache TTL is short enough that a missed invalidator
// isn't a functional bug — the page eventually reflects reality.
func (s *apiServer) invalidateOwnerProfileOnCollectionWrite(ctx context.Context, body *openapi.CollectionCreate, err error) {
	if err != nil || body == nil || s.cacheReg == nil {
		return
	}
	// CollectionCreate has no explicit owner_user_ref (the handler
	// stamps the caller). Read the identity out of the request
	// context.
	id := auth.IdentityFromContext(ctx)
	if id == nil || id.IsAnonymous() {
		return
	}
	users.InvalidateProfile(ctx, s.cacheReg, id.UserRef)
}

// invalidateOwnerProfileOnCollectionDelete looks up the collection's
// owner_user_ref BEFORE the row disappears (soft-delete keeps the
// row; the query still succeeds against the current schema which
// admits soft-deleted rows via the admin-fallback path). Fires the
// same InvalidateProfile helper as the create path so profile
// counts stay in sync. Phase 1.55.U-2 §7.2.
//
// Best-effort: DB lookup failures skip the invalidator without
// error propagation. Callers are already past the write; the
// invalidator is optimistic UX + cache freshness, not correctness.
func (s *apiServer) invalidateOwnerProfileOnCollectionDelete(ctx context.Context, collectionID uuid.UUID, err error) {
	if err != nil || collectionID == uuid.Nil || s.cacheReg == nil || s.pool == nil {
		return
	}
	var ownerRef int64
	if lookupErr := s.pool.QueryRow(ctx,
		`SELECT owner_user_ref FROM collections WHERE id = $1`, collectionID,
	).Scan(&ownerRef); lookupErr != nil {
		return
	}
	users.InvalidateProfile(ctx, s.cacheReg, ownerRef)
}

func (s *apiServer) DownloadAssetFile(ctx context.Context, req openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	return s.assets.DownloadAssetFile(ctx, req)
}

// Phase 1.18.B-3 subtitle tracks.
func (s *apiServer) ListSubtitleTracks(ctx context.Context, req openapi.ListSubtitleTracksRequestObject) (openapi.ListSubtitleTracksResponseObject, error) {
	return s.subtitlesHTTP.ListSubtitleTracks(ctx, req)
}

func (s *apiServer) UploadSubtitleTrack(ctx context.Context, req openapi.UploadSubtitleTrackRequestObject) (openapi.UploadSubtitleTrackResponseObject, error) {
	return s.subtitlesHTTP.UploadSubtitleTrack(ctx, req)
}

func (s *apiServer) DeleteSubtitleTrack(ctx context.Context, req openapi.DeleteSubtitleTrackRequestObject) (openapi.DeleteSubtitleTrackResponseObject, error) {
	return s.subtitlesHTTP.DeleteSubtitleTrack(ctx, req)
}

func (s *apiServer) BurnSubtitleTrack(ctx context.Context, req openapi.BurnSubtitleTrackRequestObject) (openapi.BurnSubtitleTrackResponseObject, error) {
	return s.subtitlesHTTP.BurnSubtitleTrack(ctx, req)
}

func (s *apiServer) GenerateImg2ImgVariation(ctx context.Context, req openapi.GenerateImg2ImgVariationRequestObject) (openapi.GenerateImg2ImgVariationResponseObject, error) {
	return s.aieditHTTP.GenerateImg2ImgVariation(ctx, req)
}

func (s *apiServer) GetAIEditConfig(ctx context.Context, req openapi.GetAIEditConfigRequestObject) (openapi.GetAIEditConfigResponseObject, error) {
	return s.sysconfigH.GetAIEditConfig(ctx, req)
}

func (s *apiServer) UpdateAIEditConfig(ctx context.Context, req openapi.UpdateAIEditConfigRequestObject) (openapi.UpdateAIEditConfigResponseObject, error) {
	return s.sysconfigH.UpdateAIEditConfig(ctx, req)
}

func (s *apiServer) DownloadAssetVariant(ctx context.Context, req openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	return s.assets.DownloadAssetVariant(ctx, req)
}

func (s *apiServer) AddAssetTags(ctx context.Context, req openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	return s.assets.AddAssetTags(ctx, req)
}

func (s *apiServer) RecreateAssetPreview(ctx context.Context, req openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	return s.assets.RecreateAssetPreview(ctx, req)
}

func (s *apiServer) ListSimilarAssets(ctx context.Context, req openapi.ListSimilarAssetsRequestObject) (openapi.ListSimilarAssetsResponseObject, error) {
	return s.assets.ListSimilarAssets(ctx, req)
}

// ---------------------------------------------------------------------------
// Phase 1.53.A — MCP-client admin endpoints
// ---------------------------------------------------------------------------

func (s *apiServer) ListMCPClients(ctx context.Context, req openapi.ListMCPClientsRequestObject) (openapi.ListMCPClientsResponseObject, error) {
	return s.mcpAdmin.ListMCPClients(ctx, req)
}
func (s *apiServer) RegisterMCPClient(ctx context.Context, req openapi.RegisterMCPClientRequestObject) (openapi.RegisterMCPClientResponseObject, error) {
	return s.mcpAdmin.RegisterMCPClient(ctx, req)
}
func (s *apiServer) UpdateMCPClient(ctx context.Context, req openapi.UpdateMCPClientRequestObject) (openapi.UpdateMCPClientResponseObject, error) {
	return s.mcpAdmin.UpdateMCPClient(ctx, req)
}
func (s *apiServer) DeleteMCPClient(ctx context.Context, req openapi.DeleteMCPClientRequestObject) (openapi.DeleteMCPClientResponseObject, error) {
	return s.mcpAdmin.DeleteMCPClient(ctx, req)
}
func (s *apiServer) ListMCPClientToolGrants(ctx context.Context, req openapi.ListMCPClientToolGrantsRequestObject) (openapi.ListMCPClientToolGrantsResponseObject, error) {
	return s.mcpAdmin.ListMCPClientToolGrants(ctx, req)
}
func (s *apiServer) UpsertMCPClientToolGrant(ctx context.Context, req openapi.UpsertMCPClientToolGrantRequestObject) (openapi.UpsertMCPClientToolGrantResponseObject, error) {
	return s.mcpAdmin.UpsertMCPClientToolGrant(ctx, req)
}
func (s *apiServer) DeleteMCPClientToolGrant(ctx context.Context, req openapi.DeleteMCPClientToolGrantRequestObject) (openapi.DeleteMCPClientToolGrantResponseObject, error) {
	return s.mcpAdmin.DeleteMCPClientToolGrant(ctx, req)
}

func (s *apiServer) RemoveAssetTag(ctx context.Context, req openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	return s.assets.RemoveAssetTag(ctx, req)
}

func (s *apiServer) ListAssetCompanions(ctx context.Context, req openapi.ListAssetCompanionsRequestObject) (openapi.ListAssetCompanionsResponseObject, error) {
	return s.assets.ListAssetCompanions(ctx, req)
}
func (s *apiServer) AddAssetCompanion(ctx context.Context, req openapi.AddAssetCompanionRequestObject) (openapi.AddAssetCompanionResponseObject, error) {
	return s.assets.AddAssetCompanion(ctx, req)
}
func (s *apiServer) DownloadAssetCompanion(ctx context.Context, req openapi.DownloadAssetCompanionRequestObject) (openapi.DownloadAssetCompanionResponseObject, error) {
	return s.assets.DownloadAssetCompanion(ctx, req)
}
func (s *apiServer) RemoveAssetCompanion(ctx context.Context, req openapi.RemoveAssetCompanionRequestObject) (openapi.RemoveAssetCompanionResponseObject, error) {
	return s.assets.RemoveAssetCompanion(ctx, req)
}

func (s *apiServer) ListAssetAlternates(ctx context.Context, req openapi.ListAssetAlternatesRequestObject) (openapi.ListAssetAlternatesResponseObject, error) {
	return s.assets.ListAssetAlternates(ctx, req)
}
func (s *apiServer) AddAssetAlternate(ctx context.Context, req openapi.AddAssetAlternateRequestObject) (openapi.AddAssetAlternateResponseObject, error) {
	return s.assets.AddAssetAlternate(ctx, req)
}
func (s *apiServer) DownloadAssetAlternate(ctx context.Context, req openapi.DownloadAssetAlternateRequestObject) (openapi.DownloadAssetAlternateResponseObject, error) {
	return s.assets.DownloadAssetAlternate(ctx, req)
}
func (s *apiServer) RemoveAssetAlternate(ctx context.Context, req openapi.RemoveAssetAlternateRequestObject) (openapi.RemoveAssetAlternateResponseObject, error) {
	return s.assets.RemoveAssetAlternate(ctx, req)
}

func (s *apiServer) GetEpubSpine(ctx context.Context, req openapi.GetEpubSpineRequestObject) (openapi.GetEpubSpineResponseObject, error) {
	return s.assets.GetEpubSpine(ctx, req)
}
func (s *apiServer) GetEpubChapter(ctx context.Context, req openapi.GetEpubChapterRequestObject) (openapi.GetEpubChapterResponseObject, error) {
	return s.assets.GetEpubChapter(ctx, req)
}
func (s *apiServer) GetEpubResource(ctx context.Context, req openapi.GetEpubResourceRequestObject) (openapi.GetEpubResourceResponseObject, error) {
	return s.assets.GetEpubResource(ctx, req)
}
func (s *apiServer) SearchEpub(ctx context.Context, req openapi.SearchEpubRequestObject) (openapi.SearchEpubResponseObject, error) {
	return s.assets.SearchEpub(ctx, req)
}

// --- metadata --------------------------------------------------------------

func (s *apiServer) ListFields(ctx context.Context, req openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	return s.metadata.ListFields(ctx, req)
}
func (s *apiServer) CreateField(ctx context.Context, req openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	return s.metadata.CreateField(ctx, req)
}
func (s *apiServer) GetField(ctx context.Context, req openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	return s.metadata.GetField(ctx, req)
}
func (s *apiServer) UpdateField(ctx context.Context, req openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	return s.metadata.UpdateField(ctx, req)
}
func (s *apiServer) ArchiveField(ctx context.Context, req openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	return s.metadata.ArchiveField(ctx, req)
}
func (s *apiServer) SetFieldExtraction(ctx context.Context, req openapi.SetFieldExtractionRequestObject) (openapi.SetFieldExtractionResponseObject, error) {
	return s.metadata.SetFieldExtraction(ctx, req)
}
func (s *apiServer) ListFieldDefaultOverrides(ctx context.Context, req openapi.ListFieldDefaultOverridesRequestObject) (openapi.ListFieldDefaultOverridesResponseObject, error) {
	return s.metadata.ListFieldDefaultOverrides(ctx, req)
}
func (s *apiServer) SetFieldDefaultOverride(ctx context.Context, req openapi.SetFieldDefaultOverrideRequestObject) (openapi.SetFieldDefaultOverrideResponseObject, error) {
	return s.metadata.SetFieldDefaultOverride(ctx, req)
}
func (s *apiServer) DeleteFieldDefaultOverride(ctx context.Context, req openapi.DeleteFieldDefaultOverrideRequestObject) (openapi.DeleteFieldDefaultOverrideResponseObject, error) {
	return s.metadata.DeleteFieldDefaultOverride(ctx, req)
}
func (s *apiServer) GetAssetFields(ctx context.Context, req openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	return s.metadata.GetAssetFields(ctx, req)
}
func (s *apiServer) SetAssetFieldValue(ctx context.Context, req openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	return s.metadata.SetAssetFieldValue(ctx, req)
}
func (s *apiServer) ClearAssetFieldValue(ctx context.Context, req openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	return s.metadata.ClearAssetFieldValue(ctx, req)
}
func (s *apiServer) GetAssetFieldValueHistory(ctx context.Context, req openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	return s.metadata.GetAssetFieldValueHistory(ctx, req)
}

// Phase 1.9.B — collection metadata HTTP surface. The handlers
// route to the metadata package so the asset + collection field
// value paths share one cache + audit + capability discipline.
func (s *apiServer) GetCollectionFields(ctx context.Context, req openapi.GetCollectionFieldsRequestObject) (openapi.GetCollectionFieldsResponseObject, error) {
	return s.metadata.GetCollectionFields(ctx, req)
}
func (s *apiServer) SetCollectionFieldValue(ctx context.Context, req openapi.SetCollectionFieldValueRequestObject) (openapi.SetCollectionFieldValueResponseObject, error) {
	return s.metadata.SetCollectionFieldValue(ctx, req)
}
func (s *apiServer) ClearCollectionFieldValue(ctx context.Context, req openapi.ClearCollectionFieldValueRequestObject) (openapi.ClearCollectionFieldValueResponseObject, error) {
	return s.metadata.ClearCollectionFieldValue(ctx, req)
}
func (s *apiServer) GetCollectionFieldValueHistory(ctx context.Context, req openapi.GetCollectionFieldValueHistoryRequestObject) (openapi.GetCollectionFieldValueHistoryResponseObject, error) {
	return s.metadata.GetCollectionFieldValueHistory(ctx, req)
}

// --- collections -----------------------------------------------------------

func (s *apiServer) ListCollections(ctx context.Context, req openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	return s.collections.ListCollections(ctx, req)
}
func (s *apiServer) CreateCollection(ctx context.Context, req openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	resp, err := s.collections.CreateCollection(ctx, req)
	// New collection: no IIIF manifest to invalidate; owner-profile
	// cache does need dropping so the profile page's "N collections"
	// count refreshes (Phase 1.55.U-2 §7.2).
	s.invalidateSearchOnCollectionWrite(ctx, uuid.Nil, err)
	s.invalidateOwnerProfileOnCollectionWrite(ctx, req.Body, err)
	return resp, err
}
func (s *apiServer) GetCollection(ctx context.Context, req openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	return s.collections.GetCollection(ctx, req)
}
func (s *apiServer) UpdateCollection(ctx context.Context, req openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	resp, err := s.collections.UpdateCollection(ctx, req)
	s.invalidateSearchOnCollectionWrite(ctx, uuid.UUID(req.Id), err)
	return resp, err
}
func (s *apiServer) DeleteCollection(ctx context.Context, req openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	resp, err := s.collections.DeleteCollection(ctx, req)
	s.invalidateSearchOnCollectionWrite(ctx, uuid.UUID(req.Id), err)
	// Owner's collection count drops on delete; profile cache
	// must invalidate too so the profile page reflects the change
	// (Phase 1.55.U-2 §7.2).
	s.invalidateOwnerProfileOnCollectionDelete(ctx, uuid.UUID(req.Id), err)
	return resp, err
}

// RestoreCollection — Phase 1.55.C-1b.
func (s *apiServer) RestoreCollection(ctx context.Context, req openapi.RestoreCollectionRequestObject) (openapi.RestoreCollectionResponseObject, error) {
	resp, err := s.collections.RestoreCollection(ctx, req)
	s.invalidateSearchOnCollectionWrite(ctx, uuid.UUID(req.Id), err)
	// Restore un-hides the collection — owner-profile count needs
	// to reflect its return (Phase 1.55.U-2 §7.2).
	s.invalidateOwnerProfileOnCollectionDelete(ctx, uuid.UUID(req.Id), err)
	return resp, err
}
func (s *apiServer) ListCollectionResources(ctx context.Context, req openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	return s.collections.ListCollectionResources(ctx, req)
}
func (s *apiServer) AddCollectionResource(ctx context.Context, req openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	return s.collections.AddCollectionResource(ctx, req)
}
func (s *apiServer) RemoveCollectionResource(ctx context.Context, req openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	return s.collections.RemoveCollectionResource(ctx, req)
}

// The /collections/{id}/posts trio delegates to POSTS, not collections
// (#882). The payload is a hydrated Post and the gate is the post read
// rule; both live there, and the collection half is obtained from
// collections.ResolveMemberWrite rather than restated. See the file
// header on posts/collection_posts.go for the full argument.
func (s *apiServer) ListCollectionPosts(ctx context.Context, req openapi.ListCollectionPostsRequestObject) (openapi.ListCollectionPostsResponseObject, error) {
	return s.posts.ListCollectionPosts(ctx, req)
}
func (s *apiServer) AddCollectionPost(ctx context.Context, req openapi.AddCollectionPostRequestObject) (openapi.AddCollectionPostResponseObject, error) {
	return s.posts.AddCollectionPost(ctx, req)
}
func (s *apiServer) RemoveCollectionPost(ctx context.Context, req openapi.RemoveCollectionPostRequestObject) (openapi.RemoveCollectionPostResponseObject, error) {
	return s.posts.RemoveCollectionPost(ctx, req)
}
func (s *apiServer) ListCollectionAcls(ctx context.Context, req openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	return s.collections.ListCollectionAcls(ctx, req)
}
func (s *apiServer) AddCollectionAcl(ctx context.Context, req openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	return s.collections.AddCollectionAcl(ctx, req)
}
func (s *apiServer) RemoveCollectionAcl(ctx context.Context, req openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	return s.collections.RemoveCollectionAcl(ctx, req)
}

// --- social ---------------------------------------------------------------

func (s *apiServer) GetPostLike(ctx context.Context, req openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	return s.social.GetPostLike(ctx, req)
}
func (s *apiServer) LikePost(ctx context.Context, req openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	return s.social.LikePost(ctx, req)
}
func (s *apiServer) UnlikePost(ctx context.Context, req openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	return s.social.UnlikePost(ctx, req)
}
func (s *apiServer) ListPostComments(ctx context.Context, req openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	return s.social.ListPostComments(ctx, req)
}
func (s *apiServer) CreatePostComment(ctx context.Context, req openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	return s.social.CreatePostComment(ctx, req)
}
func (s *apiServer) DeleteComment(ctx context.Context, req openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	return s.social.DeleteComment(ctx, req)
}
func (s *apiServer) ListPostWhiteboards(ctx context.Context, req openapi.ListPostWhiteboardsRequestObject) (openapi.ListPostWhiteboardsResponseObject, error) {
	return s.social.ListPostWhiteboards(ctx, req)
}
func (s *apiServer) CreatePostWhiteboard(ctx context.Context, req openapi.CreatePostWhiteboardRequestObject) (openapi.CreatePostWhiteboardResponseObject, error) {
	return s.social.CreatePostWhiteboard(ctx, req)
}
func (s *apiServer) LintAsset(ctx context.Context, req openapi.LintAssetRequestObject) (openapi.LintAssetResponseObject, error) {
	return s.assets.LintAsset(ctx, req)
}
func (s *apiServer) ListAssetTextAnnotations(ctx context.Context, req openapi.ListAssetTextAnnotationsRequestObject) (openapi.ListAssetTextAnnotationsResponseObject, error) {
	return s.social.ListAssetTextAnnotations(ctx, req)
}
func (s *apiServer) CreateAssetTextAnnotation(ctx context.Context, req openapi.CreateAssetTextAnnotationRequestObject) (openapi.CreateAssetTextAnnotationResponseObject, error) {
	return s.social.CreateAssetTextAnnotation(ctx, req)
}
func (s *apiServer) UpdateTextAnnotation(ctx context.Context, req openapi.UpdateTextAnnotationRequestObject) (openapi.UpdateTextAnnotationResponseObject, error) {
	return s.social.UpdateTextAnnotation(ctx, req)
}

// --- account trash ---------------------------------------------------------

func (s *apiServer) ListMyTrash(ctx context.Context, req openapi.ListMyTrashRequestObject) (openapi.ListMyTrashResponseObject, error) {
	return s.trash.ListMyTrash(ctx, req)
}

// --- account activity ------------------------------------------------------

func (s *apiServer) ListMyActivity(ctx context.Context, req openapi.ListMyActivityRequestObject) (openapi.ListMyActivityResponseObject, error) {
	return s.activity.ListMyActivity(ctx, req)
}

// --- posts -----------------------------------------------------------------

func (s *apiServer) ListPosts(ctx context.Context, req openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	return s.posts.ListPosts(ctx, req)
}
func (s *apiServer) ListPostsSharedWithMe(ctx context.Context, req openapi.ListPostsSharedWithMeRequestObject) (openapi.ListPostsSharedWithMeResponseObject, error) {
	return s.posts.ListPostsSharedWithMe(ctx, req)
}
func (s *apiServer) GetPostsByAsset(ctx context.Context, req openapi.GetPostsByAssetRequestObject) (openapi.GetPostsByAssetResponseObject, error) {
	return s.posts.GetPostsByAsset(ctx, req)
}
func (s *apiServer) CreatePost(ctx context.Context, req openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	resp, err := s.posts.CreatePost(ctx, req)
	s.invalidateSearchOnPostWrite(ctx, err)
	return resp, err
}
func (s *apiServer) GetPost(ctx context.Context, req openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	return s.posts.GetPost(ctx, req)
}
func (s *apiServer) UpdatePost(ctx context.Context, req openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	resp, err := s.posts.UpdatePost(ctx, req)
	s.invalidateSearchOnPostWrite(ctx, err)
	return resp, err
}
func (s *apiServer) DeletePost(ctx context.Context, req openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	resp, err := s.posts.DeletePost(ctx, req)
	s.invalidateSearchOnPostWrite(ctx, err)
	return resp, err
}

// RestorePost — Phase 1.55.C-1b.
func (s *apiServer) RestorePost(ctx context.Context, req openapi.RestorePostRequestObject) (openapi.RestorePostResponseObject, error) {
	resp, err := s.posts.RestorePost(ctx, req)
	s.invalidateSearchOnPostWrite(ctx, err)
	return resp, err
}
func (s *apiServer) AddPostAsset(ctx context.Context, req openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	return s.posts.AddPostAsset(ctx, req)
}
func (s *apiServer) RemovePostAsset(ctx context.Context, req openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	return s.posts.RemovePostAsset(ctx, req)
}
func (s *apiServer) ListPostAcls(ctx context.Context, req openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	return s.posts.ListPostAcls(ctx, req)
}
func (s *apiServer) AddPostAcl(ctx context.Context, req openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	return s.posts.AddPostAcl(ctx, req)
}
func (s *apiServer) RemovePostAcl(ctx context.Context, req openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	return s.posts.RemovePostAcl(ctx, req)
}

// --- teams -----------------------------------------------------------------

func (s *apiServer) ListTeams(ctx context.Context, req openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	return s.teams.ListTeams(ctx, req)
}
func (s *apiServer) CreateTeam(ctx context.Context, req openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	return s.teams.CreateTeam(ctx, req)
}
func (s *apiServer) GetTeam(ctx context.Context, req openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	return s.teams.GetTeam(ctx, req)
}
func (s *apiServer) UpdateTeam(ctx context.Context, req openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	return s.teams.UpdateTeam(ctx, req)
}
func (s *apiServer) DeleteTeam(ctx context.Context, req openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	return s.teams.DeleteTeam(ctx, req)
}
func (s *apiServer) SetTeamHero(ctx context.Context, req openapi.SetTeamHeroRequestObject) (openapi.SetTeamHeroResponseObject, error) {
	return s.teams.SetTeamHero(ctx, req)
}
func (s *apiServer) ListTeamParents(ctx context.Context, req openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	return s.teams.ListTeamParents(ctx, req)
}
func (s *apiServer) AddTeamParent(ctx context.Context, req openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	return s.teams.AddTeamParent(ctx, req)
}
func (s *apiServer) RemoveTeamParent(ctx context.Context, req openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	return s.teams.RemoveTeamParent(ctx, req)
}
func (s *apiServer) ListTeamMembers(ctx context.Context, req openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	return s.teams.ListTeamMembers(ctx, req)
}
func (s *apiServer) AddTeamMember(ctx context.Context, req openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	return s.teams.AddTeamMember(ctx, req)
}
func (s *apiServer) RemoveTeamMember(ctx context.Context, req openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	return s.teams.RemoveTeamMember(ctx, req)
}
func (s *apiServer) FollowTeam(ctx context.Context, req openapi.FollowTeamRequestObject) (openapi.FollowTeamResponseObject, error) {
	return s.teams.FollowTeam(ctx, req)
}
func (s *apiServer) UnfollowTeam(ctx context.Context, req openapi.UnfollowTeamRequestObject) (openapi.UnfollowTeamResponseObject, error) {
	return s.teams.UnfollowTeam(ctx, req)
}
func (s *apiServer) GetMyFollowedTeams(ctx context.Context, req openapi.GetMyFollowedTeamsRequestObject) (openapi.GetMyFollowedTeamsResponseObject, error) {
	return s.teams.GetMyFollowedTeams(ctx, req)
}
func (s *apiServer) FollowTag(ctx context.Context, req openapi.FollowTagRequestObject) (openapi.FollowTagResponseObject, error) {
	return s.tags.FollowTag(ctx, req)
}
func (s *apiServer) UnfollowTag(ctx context.Context, req openapi.UnfollowTagRequestObject) (openapi.UnfollowTagResponseObject, error) {
	return s.tags.UnfollowTag(ctx, req)
}
func (s *apiServer) GetMyFollowedTags(ctx context.Context, req openapi.GetMyFollowedTagsRequestObject) (openapi.GetMyFollowedTagsResponseObject, error) {
	return s.tags.GetMyFollowedTags(ctx, req)
}
func (s *apiServer) GetMyTeams(ctx context.Context, req openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	return s.teams.GetMyTeams(ctx, req)
}

// The featured-team slot in the teams rail (#1084). Pathed under
// /featured but handled here: it returns TEAMS, and a team is only
// correct once teams.attachHeroes has re-derived its picture.
func (s *apiServer) ListFeaturedTeams(ctx context.Context, req openapi.ListFeaturedTeamsRequestObject) (openapi.ListFeaturedTeamsResponseObject, error) {
	return s.teams.ListFeaturedTeams(ctx, req)
}

// --- users -----------------------------------------------------------------

func (s *apiServer) GetUserPublicByRef(ctx context.Context, req openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	return s.users.GetUserPublicByRef(ctx, req)
}
func (s *apiServer) GetUserPublicByUsername(ctx context.Context, req openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	return s.users.GetUserPublicByUsername(ctx, req)
}
func (s *apiServer) GetUserPublicByRefPath(ctx context.Context, req openapi.GetUserPublicByRefPathRequestObject) (openapi.GetUserPublicByRefPathResponseObject, error) {
	return s.users.GetUserPublicByRefPath(ctx, req)
}
func (s *apiServer) UpdateUserProfile(ctx context.Context, req openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	return s.users.UpdateUserProfile(ctx, req)
}
func (s *apiServer) ListAdminUsers(ctx context.Context, req openapi.ListAdminUsersRequestObject) (openapi.ListAdminUsersResponseObject, error) {
	return s.users.ListAdminUsers(ctx, req)
}
func (s *apiServer) SetAdminUserStatus(ctx context.Context, req openapi.SetAdminUserStatusRequestObject) (openapi.SetAdminUserStatusResponseObject, error) {
	return s.users.SetAdminUserStatus(ctx, req)
}

// --- self-edit gates (Phase 1.17.F) --------------------------------------

func (s *apiServer) GetSelfEditGates(ctx context.Context, req openapi.GetSelfEditGatesRequestObject) (openapi.GetSelfEditGatesResponseObject, error) {
	return s.users.GetSelfEditGates(ctx, req)
}
func (s *apiServer) GetAdminUserGates(ctx context.Context, req openapi.GetAdminUserGatesRequestObject) (openapi.GetAdminUserGatesResponseObject, error) {
	return s.users.GetAdminUserGates(ctx, req)
}
func (s *apiServer) UpdateAdminUserGates(ctx context.Context, req openapi.UpdateAdminUserGatesRequestObject) (openapi.UpdateAdminUserGatesResponseObject, error) {
	return s.users.UpdateAdminUserGates(ctx, req)
}

// --- resource requests (Phase 1.17.E) ------------------------------------

func (s *apiServer) RequestAssetAccess(ctx context.Context, req openapi.RequestAssetAccessRequestObject) (openapi.RequestAssetAccessResponseObject, error) {
	return s.requestsHTTP.RequestAssetAccess(ctx, req)
}
func (s *apiServer) RequestRestore(ctx context.Context, req openapi.RequestRestoreRequestObject) (openapi.RequestRestoreResponseObject, error) {
	return s.requestsHTTP.RequestRestore(ctx, req)
}
func (s *apiServer) ListOwnRequests(ctx context.Context, req openapi.ListOwnRequestsRequestObject) (openapi.ListOwnRequestsResponseObject, error) {
	return s.requestsHTTP.ListOwnRequests(ctx, req)
}
func (s *apiServer) ListIncomingRequests(ctx context.Context, req openapi.ListIncomingRequestsRequestObject) (openapi.ListIncomingRequestsResponseObject, error) {
	return s.requestsHTTP.ListIncomingRequests(ctx, req)
}
func (s *apiServer) ListAdminRequests(ctx context.Context, req openapi.ListAdminRequestsRequestObject) (openapi.ListAdminRequestsResponseObject, error) {
	return s.requestsHTTP.ListAdminRequests(ctx, req)
}
func (s *apiServer) DecideAdminRequest(ctx context.Context, req openapi.DecideAdminRequestRequestObject) (openapi.DecideAdminRequestResponseObject, error) {
	return s.requestsHTTP.DecideAdminRequest(ctx, req)
}

// --- operator string overrides (#794) -------------------------------------

func (s *apiServer) GetSiteText(ctx context.Context, req openapi.GetSiteTextRequestObject) (openapi.GetSiteTextResponseObject, error) {
	return s.sitetextHTTP.GetSiteText(ctx, req)
}
func (s *apiServer) SetSiteText(ctx context.Context, req openapi.SetSiteTextRequestObject) (openapi.SetSiteTextResponseObject, error) {
	return s.sitetextHTTP.SetSiteText(ctx, req)
}
func (s *apiServer) DeleteSiteText(ctx context.Context, req openapi.DeleteSiteTextRequestObject) (openapi.DeleteSiteTextResponseObject, error) {
	return s.sitetextHTTP.DeleteSiteText(ctx, req)
}

// --- operator-authored email templates (#795) -----------------------------

func (s *apiServer) GetEmailTemplates(ctx context.Context, req openapi.GetEmailTemplatesRequestObject) (openapi.GetEmailTemplatesResponseObject, error) {
	return s.emailTemplatesHTTP.GetEmailTemplates(ctx, req)
}
func (s *apiServer) SetEmailTemplate(ctx context.Context, req openapi.SetEmailTemplateRequestObject) (openapi.SetEmailTemplateResponseObject, error) {
	return s.emailTemplatesHTTP.SetEmailTemplate(ctx, req)
}
func (s *apiServer) DeleteEmailTemplate(ctx context.Context, req openapi.DeleteEmailTemplateRequestObject) (openapi.DeleteEmailTemplateResponseObject, error) {
	return s.emailTemplatesHTTP.DeleteEmailTemplate(ctx, req)
}

// --- featured content (GitHub #341) ---------------------------------------

func (s *apiServer) GetPublicFeaturedRail(ctx context.Context, req openapi.GetPublicFeaturedRailRequestObject) (openapi.GetPublicFeaturedRailResponseObject, error) {
	return s.featuredHTTP.GetPublicFeaturedRail(ctx, req)
}
func (s *apiServer) ListFeaturedItems(ctx context.Context, req openapi.ListFeaturedItemsRequestObject) (openapi.ListFeaturedItemsResponseObject, error) {
	return s.featuredHTTP.ListFeaturedItems(ctx, req)
}
func (s *apiServer) AddFeaturedItem(ctx context.Context, req openapi.AddFeaturedItemRequestObject) (openapi.AddFeaturedItemResponseObject, error) {
	return s.featuredHTTP.AddFeaturedItem(ctx, req)
}
func (s *apiServer) RemoveFeaturedItem(ctx context.Context, req openapi.RemoveFeaturedItemRequestObject) (openapi.RemoveFeaturedItemResponseObject, error) {
	return s.featuredHTTP.RemoveFeaturedItem(ctx, req)
}
func (s *apiServer) ReorderFeaturedItems(ctx context.Context, req openapi.ReorderFeaturedItemsRequestObject) (openapi.ReorderFeaturedItemsResponseObject, error) {
	return s.featuredHTTP.ReorderFeaturedItems(ctx, req)
}

// --- setup -----------------------------------------------------------------

func (s *apiServer) GetSetupStatus(ctx context.Context, req openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	return s.setup.GetSetupStatus(ctx, req)
}

func (s *apiServer) CompleteSetup(ctx context.Context, req openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	return s.setup.CompleteSetup(ctx, req)
}

// GetBuildInfo returns the running server version (#406). Anonymous —
// the version is the git tag baked in via ldflags (or "dev"), which is
// not sensitive; the admin About page renders it in place of a stub.
func (s *apiServer) GetBuildInfo(_ context.Context, _ openapi.GetBuildInfoRequestObject) (openapi.GetBuildInfoResponseObject, error) {
	return openapi.GetBuildInfo200JSONResponse{Version: s.version}, nil
}

// --- workflow --------------------------------------------------------------

func (s *apiServer) ListWorkflowStates(ctx context.Context, req openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	return s.workflow.ListWorkflowStates(ctx, req)
}

// --- sysconfig (admin) -----------------------------------------------------

func (s *apiServer) GetSiteConfig(ctx context.Context, req openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	return s.sysconfigH.GetSiteConfig(ctx, req)
}
func (s *apiServer) UpdateSiteConfig(ctx context.Context, req openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	return s.sysconfigH.UpdateSiteConfig(ctx, req)
}
func (s *apiServer) GetSMTPConfig(ctx context.Context, req openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	return s.sysconfigH.GetSMTPConfig(ctx, req)
}
func (s *apiServer) UpdateSMTPConfig(ctx context.Context, req openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	return s.sysconfigH.UpdateSMTPConfig(ctx, req)
}
func (s *apiServer) SendSMTPTestEmail(ctx context.Context, req openapi.SendSMTPTestEmailRequestObject) (openapi.SendSMTPTestEmailResponseObject, error) {
	return s.sysconfigH.SendSMTPTestEmail(ctx, req)
}
func (s *apiServer) GetAuthConfig(ctx context.Context, req openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	return s.sysconfigH.GetAuthConfig(ctx, req)
}
func (s *apiServer) UpdateAuthConfig(ctx context.Context, req openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	return s.sysconfigH.UpdateAuthConfig(ctx, req)
}
func (s *apiServer) GetAIConfig(ctx context.Context, req openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	return s.sysconfigH.GetAIConfig(ctx, req)
}
func (s *apiServer) UpdateAIConfig(ctx context.Context, req openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	return s.sysconfigH.UpdateAIConfig(ctx, req)
}

// Phase 1.14.A — AI inference subsystem admin endpoints. Routed to
// the dedicated ai/admin package which composes the typed Loader +
// validator + cost rollup. Distinct from the Phase 1.16 GetAIConfig
// surface above (that's the raw provider-list stub; this is the
// inference config that drives the router + privacy gate + budget
// tracker).
//
// aiAdmin is nil-safe (the field is populated only when the AI
// subsystem boots successfully); the methods return 503 when it
// isn't wired so the admin UI can surface "AI subsystem
// unavailable" rather than a 500.
func (s *apiServer) GetAIInferenceConfig(ctx context.Context, req openapi.GetAIInferenceConfigRequestObject) (openapi.GetAIInferenceConfigResponseObject, error) {
	if s.aiAdmin == nil {
		return openapi.GetAIInferenceConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "AI subsystem not initialised"},
		}, nil
	}
	return s.aiAdmin.GetAIInferenceConfig(ctx, req)
}
func (s *apiServer) UpdateAIInferenceConfig(ctx context.Context, req openapi.UpdateAIInferenceConfigRequestObject) (openapi.UpdateAIInferenceConfigResponseObject, error) {
	if s.aiAdmin == nil {
		return openapi.UpdateAIInferenceConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "AI subsystem not initialised"},
		}, nil
	}
	return s.aiAdmin.UpdateAIInferenceConfig(ctx, req)
}
func (s *apiServer) GetAIUsage(ctx context.Context, req openapi.GetAIUsageRequestObject) (openapi.GetAIUsageResponseObject, error) {
	if s.aiAdmin == nil {
		return openapi.GetAIUsage401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "AI subsystem not initialised"},
		}, nil
	}
	return s.aiAdmin.GetAIUsage(ctx, req)
}
func (s *apiServer) GetAppearanceConfig(ctx context.Context, req openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	return s.sysconfigH.GetAppearanceConfig(ctx, req)
}
func (s *apiServer) UpdateAppearanceConfig(ctx context.Context, req openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	return s.sysconfigH.UpdateAppearanceConfig(ctx, req)
}
func (s *apiServer) UploadInstanceLogo(ctx context.Context, req openapi.UploadInstanceLogoRequestObject) (openapi.UploadInstanceLogoResponseObject, error) {
	return s.sysconfigH.UploadInstanceLogo(ctx, req)
}
func (s *apiServer) SelectInstanceLogo(ctx context.Context, req openapi.SelectInstanceLogoRequestObject) (openapi.SelectInstanceLogoResponseObject, error) {
	return s.sysconfigH.SelectInstanceLogo(ctx, req)
}
func (s *apiServer) DeleteInstanceLogo(ctx context.Context, req openapi.DeleteInstanceLogoRequestObject) (openapi.DeleteInstanceLogoResponseObject, error) {
	return s.sysconfigH.DeleteInstanceLogo(ctx, req)
}
func (s *apiServer) GetPublicInstanceLogo(ctx context.Context, req openapi.GetPublicInstanceLogoRequestObject) (openapi.GetPublicInstanceLogoResponseObject, error) {
	return s.sysconfigH.GetPublicInstanceLogo(ctx, req)
}
func (s *apiServer) GetPublicPreviewLadder(ctx context.Context, req openapi.GetPublicPreviewLadderRequestObject) (openapi.GetPublicPreviewLadderResponseObject, error) {
	return s.sysconfigH.GetPublicPreviewLadder(ctx, req)
}

func (s *apiServer) GetPublicAppearance(ctx context.Context, req openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	return s.sysconfigH.GetPublicAppearance(ctx, req)
}
func (s *apiServer) GetPublicMode(ctx context.Context, req openapi.GetPublicModeRequestObject) (openapi.GetPublicModeResponseObject, error) {
	return s.sysconfigH.GetPublicMode(ctx, req)
}
func (s *apiServer) UpdatePublicMode(ctx context.Context, req openapi.UpdatePublicModeRequestObject) (openapi.UpdatePublicModeResponseObject, error) {
	return s.sysconfigH.UpdatePublicMode(ctx, req)
}

func (s *apiServer) GetBrowseViews(ctx context.Context, req openapi.GetBrowseViewsRequestObject) (openapi.GetBrowseViewsResponseObject, error) {
	return s.sysconfigH.GetBrowseViews(ctx, req)
}
func (s *apiServer) UpdateBrowseViews(ctx context.Context, req openapi.UpdateBrowseViewsRequestObject) (openapi.UpdateBrowseViewsResponseObject, error) {
	return s.sysconfigH.UpdateBrowseViews(ctx, req)
}
func (s *apiServer) GetPublicBrowseViews(ctx context.Context, req openapi.GetPublicBrowseViewsRequestObject) (openapi.GetPublicBrowseViewsResponseObject, error) {
	return s.sysconfigH.GetPublicBrowseViews(ctx, req)
}

// --- audit viewer (Phase 1.17.K) ------------------------------------------

func (s *apiServer) ListScheduledActions(ctx context.Context, req openapi.ListScheduledActionsRequestObject) (openapi.ListScheduledActionsResponseObject, error) {
	return s.scheduledActions.ListScheduledActions(ctx, req)
}
func (s *apiServer) CancelScheduledAction(ctx context.Context, req openapi.CancelScheduledActionRequestObject) (openapi.CancelScheduledActionResponseObject, error) {
	return s.scheduledActions.CancelScheduledAction(ctx, req)
}
func (s *apiServer) ExportAuditEvents(ctx context.Context, req openapi.ExportAuditEventsRequestObject) (openapi.ExportAuditEventsResponseObject, error) {
	return s.audit.ExportAuditEvents(ctx, req)
}
func (s *apiServer) ListAdminAuditEvents(ctx context.Context, req openapi.ListAdminAuditEventsRequestObject) (openapi.ListAdminAuditEventsResponseObject, error) {
	return s.audit.ListAdminAuditEvents(ctx, req)
}
func (s *apiServer) ListAdminAuditEventTypes(ctx context.Context, req openapi.ListAdminAuditEventTypesRequestObject) (openapi.ListAdminAuditEventTypesResponseObject, error) {
	return s.audit.ListAdminAuditEventTypes(ctx, req)
}

// --- licensing (Phase 1.17.O) ---------------------------------------------

func (s *apiServer) GetAdminLicenseStatus(ctx context.Context, req openapi.GetAdminLicenseStatusRequestObject) (openapi.GetAdminLicenseStatusResponseObject, error) {
	return s.licensing.GetAdminLicenseStatus(ctx, req)
}

func (s *apiServer) ValidateAdminLicense(ctx context.Context, req openapi.ValidateAdminLicenseRequestObject) (openapi.ValidateAdminLicenseResponseObject, error) {
	return s.licensing.ValidateAdminLicense(ctx, req)
}

func (s *apiServer) UploadAdminLicense(ctx context.Context, req openapi.UploadAdminLicenseRequestObject) (openapi.UploadAdminLicenseResponseObject, error) {
	return s.licensing.UploadAdminLicense(ctx, req)
}

// --- userprefs (Phase 1.17.G) ----------------------------------------------

func (s *apiServer) GetAccountPreferences(ctx context.Context, req openapi.GetAccountPreferencesRequestObject) (openapi.GetAccountPreferencesResponseObject, error) {
	return s.userprefs.GetAccountPreferences(ctx, req)
}

func (s *apiServer) PatchAccountPreferences(ctx context.Context, req openapi.PatchAccountPreferencesRequestObject) (openapi.PatchAccountPreferencesResponseObject, error) {
	return s.userprefs.PatchAccountPreferences(ctx, req)
}

// --- social graph (Phase 1.17.G2) -----------------------------------------

func (s *apiServer) FollowUser(ctx context.Context, req openapi.FollowUserRequestObject) (openapi.FollowUserResponseObject, error) {
	return s.social.FollowUser(ctx, req)
}

func (s *apiServer) UnfollowUser(ctx context.Context, req openapi.UnfollowUserRequestObject) (openapi.UnfollowUserResponseObject, error) {
	return s.social.UnfollowUser(ctx, req)
}

func (s *apiServer) ListUserFollowers(ctx context.Context, req openapi.ListUserFollowersRequestObject) (openapi.ListUserFollowersResponseObject, error) {
	return s.social.ListUserFollowers(ctx, req)
}

func (s *apiServer) ListUserFollowing(ctx context.Context, req openapi.ListUserFollowingRequestObject) (openapi.ListUserFollowingResponseObject, error) {
	return s.social.ListUserFollowing(ctx, req)
}

func (s *apiServer) GetUserRelationship(ctx context.Context, req openapi.GetUserRelationshipRequestObject) (openapi.GetUserRelationshipResponseObject, error) {
	return s.social.GetUserRelationship(ctx, req)
}

func (s *apiServer) BlockUser(ctx context.Context, req openapi.BlockUserRequestObject) (openapi.BlockUserResponseObject, error) {
	return s.social.BlockUser(ctx, req)
}

func (s *apiServer) UnblockUser(ctx context.Context, req openapi.UnblockUserRequestObject) (openapi.UnblockUserResponseObject, error) {
	return s.social.UnblockUser(ctx, req)
}

func (s *apiServer) ListMyBlocked(ctx context.Context, req openapi.ListMyBlockedRequestObject) (openapi.ListMyBlockedResponseObject, error) {
	return s.social.ListMyBlocked(ctx, req)
}

// --- notifications (Phase 1.17.I2) ----------------------------------------

func (s *apiServer) ListMyNotifications(ctx context.Context, req openapi.ListMyNotificationsRequestObject) (openapi.ListMyNotificationsResponseObject, error) {
	return s.notifications.ListMyNotifications(ctx, req)
}

func (s *apiServer) GetMyUnreadNotificationCount(ctx context.Context, req openapi.GetMyUnreadNotificationCountRequestObject) (openapi.GetMyUnreadNotificationCountResponseObject, error) {
	return s.notifications.GetMyUnreadNotificationCount(ctx, req)
}

func (s *apiServer) MarkNotificationRead(ctx context.Context, req openapi.MarkNotificationReadRequestObject) (openapi.MarkNotificationReadResponseObject, error) {
	return s.notifications.MarkNotificationRead(ctx, req)
}

func (s *apiServer) MarkAllMyNotificationsRead(ctx context.Context, req openapi.MarkAllMyNotificationsReadRequestObject) (openapi.MarkAllMyNotificationsReadResponseObject, error) {
	return s.notifications.MarkAllMyNotificationsRead(ctx, req)
}

// --- federation directories (Phase 1.22.B-c) -----------------------------

func (s *apiServer) ListFederationDirectories(ctx context.Context, req openapi.ListFederationDirectoriesRequestObject) (openapi.ListFederationDirectoriesResponseObject, error) {
	return s.directoriesAdmin.ListFederationDirectories(ctx, req)
}

func (s *apiServer) SubscribeFederationDirectory(ctx context.Context, req openapi.SubscribeFederationDirectoryRequestObject) (openapi.SubscribeFederationDirectoryResponseObject, error) {
	return s.directoriesAdmin.SubscribeFederationDirectory(ctx, req)
}

func (s *apiServer) UnsubscribeFederationDirectory(ctx context.Context, req openapi.UnsubscribeFederationDirectoryRequestObject) (openapi.UnsubscribeFederationDirectoryResponseObject, error) {
	return s.directoriesAdmin.UnsubscribeFederationDirectory(ctx, req)
}

func (s *apiServer) PollFederationDirectory(ctx context.Context, req openapi.PollFederationDirectoryRequestObject) (openapi.PollFederationDirectoryResponseObject, error) {
	return s.directoriesAdmin.PollFederationDirectory(ctx, req)
}

func (s *apiServer) ListFederationDirectoryEntries(ctx context.Context, req openapi.ListFederationDirectoryEntriesRequestObject) (openapi.ListFederationDirectoryEntriesResponseObject, error) {
	return s.directoriesAdmin.ListFederationDirectoryEntries(ctx, req)
}

func (s *apiServer) RequestFederationDirectoryPublishChallenge(ctx context.Context, req openapi.RequestFederationDirectoryPublishChallengeRequestObject) (openapi.RequestFederationDirectoryPublishChallengeResponseObject, error) {
	return s.directoriesAdmin.RequestFederationDirectoryPublishChallenge(ctx, req)
}

func (s *apiServer) RegisterFederationDirectoryPublishListing(ctx context.Context, req openapi.RegisterFederationDirectoryPublishListingRequestObject) (openapi.RegisterFederationDirectoryPublishListingResponseObject, error) {
	return s.directoriesAdmin.RegisterFederationDirectoryPublishListing(ctx, req)
}

// --- federation public + handshake (Phase 1.22.B-b) ----------------------

func (s *apiServer) GetFederationInstance(ctx context.Context, req openapi.GetFederationInstanceRequestObject) (openapi.GetFederationInstanceResponseObject, error) {
	return s.peersPublic.GetFederationInstance(ctx, req)
}

func (s *apiServer) GetFederationPeersVisible(ctx context.Context, req openapi.GetFederationPeersVisibleRequestObject) (openapi.GetFederationPeersVisibleResponseObject, error) {
	return s.peersPublic.GetFederationPeersVisible(ctx, req)
}

func (s *apiServer) ListFederationPeerSuggestions(ctx context.Context, req openapi.ListFederationPeerSuggestionsRequestObject) (openapi.ListFederationPeerSuggestionsResponseObject, error) {
	return s.p2pAdmin.ListFederationPeerSuggestions(ctx, req)
}

func (s *apiServer) RefreshFederationPeerSuggestions(ctx context.Context, req openapi.RefreshFederationPeerSuggestionsRequestObject) (openapi.RefreshFederationPeerSuggestionsResponseObject, error) {
	return s.p2pAdmin.RefreshFederationPeerSuggestions(ctx, req)
}

// --- federation shares admin (Phase 1.22.C-c) ---------------------------

func (s *apiServer) ListFederationShares(ctx context.Context, req openapi.ListFederationSharesRequestObject) (openapi.ListFederationSharesResponseObject, error) {
	return s.sharesAdmin.ListFederationShares(ctx, req)
}

func (s *apiServer) GrantFederationShare(ctx context.Context, req openapi.GrantFederationShareRequestObject) (openapi.GrantFederationShareResponseObject, error) {
	return s.sharesAdmin.GrantFederationShare(ctx, req)
}

func (s *apiServer) RevokeFederationShare(ctx context.Context, req openapi.RevokeFederationShareRequestObject) (openapi.RevokeFederationShareResponseObject, error) {
	return s.sharesAdmin.RevokeFederationShare(ctx, req)
}

// --- federation outbox + inbox admin (Phase 1.22.D-c) -------------------

func (s *apiServer) ListFederationOutbox(ctx context.Context, req openapi.ListFederationOutboxRequestObject) (openapi.ListFederationOutboxResponseObject, error) {
	f := outbox.AdminListOutboxFilter{}
	if req.Params.PeerId != nil {
		pid := uuid.UUID(*req.Params.PeerId)
		f.PeerID = &pid
	}
	if req.Params.Status != nil {
		s := string(*req.Params.Status)
		f.Status = &s
	}
	if req.Params.ActivityType != nil {
		s := *req.Params.ActivityType
		f.ActivityType = &s
	}
	if req.Params.Since != nil {
		t := *req.Params.Since
		f.Since = &t
	}
	if req.Params.Limit != nil {
		f.Limit = int32(*req.Params.Limit)
	}
	if req.Params.Cursor != nil {
		if t, id, ok := outbox.DecodeCursor(*req.Params.Cursor); ok {
			f.CursorCreatedAt = &t
			f.CursorID = &id
		}
	}
	rows, nextCursor, err := s.outboxAdmin.ListOutboxForAdmin(ctx, f)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.FederationOutbox, 0, len(rows))
	for _, r := range rows {
		items = append(items, adminOutboxToAPI(r))
	}
	resp := openapi.ListFederationOutbox200JSONResponse{
		Items: items,
	}
	if nextCursor != "" {
		resp.NextCursor = &nextCursor
	}
	return resp, nil
}

func (s *apiServer) RequeueFederationOutbox(ctx context.Context, req openapi.RequeueFederationOutboxRequestObject) (openapi.RequeueFederationOutboxResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.RequeueFederationOutbox401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	updated, err := s.outboxAdmin.RequeueOutbox(ctx, nil, caller.UserRef, uuid.UUID(req.Id))
	if err != nil {
		switch {
		case errors.Is(err, outbox.ErrOutboxNotFound):
			return openapi.RequeueFederationOutbox404JSONResponse{Error: "outbox row not found"}, nil
		case errors.Is(err, outbox.ErrOutboxNotFailed):
			return openapi.RequeueFederationOutbox409JSONResponse{Error: "row is not in status=failed; re-queue refused per idempotency guard"}, nil
		}
		return nil, err
	}
	return openapi.RequeueFederationOutbox200JSONResponse(adminOutboxToAPI(updated)), nil
}

func (s *apiServer) ListFederationInbox(ctx context.Context, req openapi.ListFederationInboxRequestObject) (openapi.ListFederationInboxResponseObject, error) {
	f := outbox.AdminListInboxFilter{}
	if req.Params.PeerId != nil {
		pid := uuid.UUID(*req.Params.PeerId)
		f.PeerID = &pid
	}
	if req.Params.Status != nil {
		st := string(*req.Params.Status)
		f.Status = &st
	}
	if req.Params.ActivityType != nil {
		st := *req.Params.ActivityType
		f.ActivityType = &st
	}
	if req.Params.Since != nil {
		t := *req.Params.Since
		f.Since = &t
	}
	if req.Params.Limit != nil {
		f.Limit = int32(*req.Params.Limit)
	}
	if req.Params.Cursor != nil {
		if t, id, ok := outbox.DecodeCursor(*req.Params.Cursor); ok {
			f.CursorReceivedAt = &t
			f.CursorID = &id
		}
	}
	rows, nextCursor, err := s.outboxAdmin.ListInboxForAdmin(ctx, f)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.FederationInboxRow, 0, len(rows))
	for _, r := range rows {
		items = append(items, adminInboxToAPI(r))
	}
	resp := openapi.ListFederationInbox200JSONResponse{
		Items: items,
	}
	if nextCursor != "" {
		resp.NextCursor = &nextCursor
	}
	return resp, nil
}

func (s *apiServer) CancelFederationPeerPending(ctx context.Context, req openapi.CancelFederationPeerPendingRequestObject) (openapi.CancelFederationPeerPendingResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.CancelFederationPeerPending401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	count, err := s.outboxAdmin.CancelPendingForPeer(ctx, nil, caller.UserRef, uuid.UUID(req.Id))
	if err != nil {
		return nil, err
	}
	return openapi.CancelFederationPeerPending200JSONResponse{
		PeerId:         openapi_types.UUID(req.Id),
		CancelledCount: count,
	}, nil
}

// nonEmptyStringPtr returns nil for empty strings; otherwise &s.
// Used to project Go's empty-string defaults to JSON null for
// optional openapi fields.
func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// adminOutboxToAPI projects the admin row into the openapi shape.
func adminOutboxToAPI(r outbox.AdminOutboxRow) openapi.FederationOutbox {
	out := openapi.FederationOutbox{
		Id:            openapi_types.UUID(r.ID),
		ActivityId:    openapi_types.UUID(r.ActivityID),
		PeerId:        openapi_types.UUID(r.PeerID),
		Status:        openapi.FederationOutboxStatus(r.Status),
		Attempts:      int(r.Attempts),
		NextAttemptAt: r.NextAttemptAt,
		LastError:     nonEmptyStringPtr(r.LastError),
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
	if r.TargetUserURL != nil {
		out.TargetUserUrl = r.TargetUserURL
	}
	if r.LastAttemptAt != nil {
		out.LastAttemptAt = r.LastAttemptAt
	}
	if r.SentAt != nil {
		out.SentAt = r.SentAt
	}
	if r.DeliveredWithKeyID != nil {
		out.DeliveredWithKeyId = r.DeliveredWithKeyID
	}
	return out
}

// adminInboxToAPI projects the admin inbox row into the openapi shape.
func adminInboxToAPI(r outbox.AdminInboxRow) openapi.FederationInboxRow {
	out := openapi.FederationInboxRow{
		Id:               openapi_types.UUID(r.ID),
		ActivityUri:      r.ActivityURI,
		PeerId:           openapi_types.UUID(r.PeerID),
		ActorUri:         r.ActorURI,
		ActivityType:     r.ActivityType,
		HttpSigKey:       nonEmptyStringPtr(r.HTTPSigKey),
		Status:           openapi.FederationInboxRowStatus(r.Status),
		DispatchAttempts: int(r.DispatchAttempts),
		ReceivedAt:       r.ReceivedAt,
	}
	if r.ObjectKind != nil {
		out.ObjectKind = r.ObjectKind
	}
	if r.ObjectID != nil {
		oid := openapi_types.UUID(*r.ObjectID)
		out.ObjectId = &oid
	}
	if r.RejectReason != nil {
		out.RejectReason = r.RejectReason
	}
	if r.LastAttemptAt != nil {
		out.LastAttemptAt = r.LastAttemptAt
	}
	if r.LastError != nil {
		out.LastError = r.LastError
	}
	if r.ProcessedAt != nil {
		out.ProcessedAt = r.ProcessedAt
	}
	if r.CorrelationActivityID != nil {
		cid := openapi_types.UUID(*r.CorrelationActivityID)
		out.CorrelationActivityId = &cid
	}
	return out
}

// --- metadata-extraction admin (Phase 1.18.A-2 follow-up B) -----------

func (s *apiServer) ListMetadataExtractionFailures(ctx context.Context, req openapi.ListMetadataExtractionFailuresRequestObject) (openapi.ListMetadataExtractionFailuresResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListMetadataExtractionFailures401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	// Read gate: the dedicated read cap, or system.admin (#356).
	// Dismissing a failure stays system.admin-only.
	if !caller.Can(assetmetadata.CapExtractionRead) && !caller.Can("system.admin") {
		return openapi.ListMetadataExtractionFailures403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: assetmetadata.CapExtractionRead + " required"},
		}, nil
	}
	f := assetmetadata.ListFailuresFilter{}
	if req.Params.ErrorKind != nil {
		v := *req.Params.ErrorKind
		f.ErrorKind = &v
	}
	if req.Params.Format != nil {
		v := *req.Params.Format
		f.Format = &v
	}
	if req.Params.Limit != nil {
		f.Limit = int32(*req.Params.Limit)
	}
	if req.Params.Offset != nil {
		f.Offset = int32(*req.Params.Offset)
	}
	rows, total, err := s.metaAdmin.ListFailures(ctx, f)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.ExtractionFailure, 0, len(rows))
	for _, r := range rows {
		items = append(items, metaFailureToAPI(r))
	}
	return openapi.ListMetadataExtractionFailures200JSONResponse{
		Items: items,
		Total: total,
	}, nil
}

func (s *apiServer) DismissMetadataExtractionFailure(ctx context.Context, req openapi.DismissMetadataExtractionFailureRequestObject) (openapi.DismissMetadataExtractionFailureResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.DismissMetadataExtractionFailure401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.DismissMetadataExtractionFailure403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	if err := s.metaAdmin.DismissFailure(ctx, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, assetmetadata.ErrFailureNotFound) {
			return openapi.DismissMetadataExtractionFailure404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "extraction failure not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.DismissMetadataExtractionFailure204Response{}, nil
}

func (s *apiServer) ListMetadataExtractionBackfills(ctx context.Context, req openapi.ListMetadataExtractionBackfillsRequestObject) (openapi.ListMetadataExtractionBackfillsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ListMetadataExtractionBackfills401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.ListMetadataExtractionBackfills403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	var limit int32
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	rows, err := s.metaAdmin.ListRecentBackfills(ctx, limit)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.MetadataBackfillRun, 0, len(rows))
	for _, r := range rows {
		items = append(items, metaBackfillToAPI(r))
	}
	return openapi.ListMetadataExtractionBackfills200JSONResponse{Items: items}, nil
}

func (s *apiServer) StartMetadataExtractionBackfill(ctx context.Context, req openapi.StartMetadataExtractionBackfillRequestObject) (openapi.StartMetadataExtractionBackfillResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.StartMetadataExtractionBackfill401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.StartMetadataExtractionBackfill403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	p := assetmetadata.BackfillStartParams{
		StartedBy: &caller.UserRef,
	}
	if req.Body != nil {
		if req.Body.AssetTypeRef != nil {
			v := *req.Body.AssetTypeRef
			p.Scope.AssetTypeRef = &v
		}
		if req.Body.AssetTypeRefs != nil {
			p.Scope.AssetTypeRefs = append(p.Scope.AssetTypeRefs, *req.Body.AssetTypeRefs...)
		}
		if req.Body.FileExtensions != nil {
			p.Scope.FileExtensions = append(p.Scope.FileExtensions, *req.Body.FileExtensions...)
		}
		if req.Body.IncludeNonImage != nil {
			p.Scope.IncludeNonImage = *req.Body.IncludeNonImage
		}
	}
	row, err := s.metaAdmin.StartBackfill(ctx, s.jobsSvc, p)
	if err != nil {
		return nil, err
	}
	resp := openapi.StartMetadataExtractionBackfill200JSONResponse(metaBackfillToAPI(row))
	return resp, nil
}

func (s *apiServer) GetMetadataExtractionBackfill(ctx context.Context, req openapi.GetMetadataExtractionBackfillRequestObject) (openapi.GetMetadataExtractionBackfillResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.GetMetadataExtractionBackfill401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.GetMetadataExtractionBackfill403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	row, err := s.metaAdmin.GetBackfill(ctx, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, assetmetadata.ErrBackfillNotFound) {
			return openapi.GetMetadataExtractionBackfill404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "backfill run not found"},
			}, nil
		}
		return nil, err
	}
	resp := openapi.GetMetadataExtractionBackfill200JSONResponse(metaBackfillToAPI(row))
	return resp, nil
}

func (s *apiServer) CancelMetadataExtractionBackfill(ctx context.Context, req openapi.CancelMetadataExtractionBackfillRequestObject) (openapi.CancelMetadataExtractionBackfillResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.CancelMetadataExtractionBackfill401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.CancelMetadataExtractionBackfill403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	if err := s.metaAdmin.CancelBackfill(ctx, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, assetmetadata.ErrBackfillNotFound) {
			return openapi.CancelMetadataExtractionBackfill404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "backfill run not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.CancelMetadataExtractionBackfill204Response{}, nil
}

func metaBackfillToAPI(r assetmetadata.BackfillRunRow) openapi.MetadataBackfillRun {
	out := openapi.MetadataBackfillRun{
		Id:               openapi_types.UUID(r.ID),
		Total:            r.Total,
		Processed:        r.Processed,
		Succeeded:        r.Succeeded,
		Failed:           r.Failed,
		StartedAt:        r.StartedAt,
		StartedByUserRef: r.StartedByUserRef,
	}
	if r.Scope.AssetTypeRef != nil {
		v := *r.Scope.AssetTypeRef
		out.AssetTypeRef = &v
	}
	if r.CompletedAt != nil {
		t := *r.CompletedAt
		out.CompletedAt = &t
	}
	if r.CancelledAt != nil {
		t := *r.CancelledAt
		out.CancelledAt = &t
	}
	return out
}

// --- jobs admin read surface (#400, v0.4.0 Sprint 0) ------------------
//
// All three GETs are gated on jobs.CapJobsRead (or system.admin) and
// are strictly read-only — no requeue/cancel path exists under this cap
// (Sprint 1, #401). Mirrors the metadata-extraction admin gate above.

// --- storage (admin reads, #402) -------------------------------------------

// storageReadDenied mirrors jobsReadDenied: the read cap OR the
// system.admin wildcard opens the storage admin surface.
func (s *apiServer) storageReadDenied(ctx context.Context) bool {
	caller := auth.IdentityFromContext(ctx)
	return caller == nil || (!caller.Can(storage.CapStorageRead) && !caller.Can("system.admin"))
}

// storageAdminDenied gates the sweep MUTATION (triggering a scan) on
// system.admin, mirroring the jobs requeue/cancel split: reads on the
// read cap, actions on the wildcard.
func (s *apiServer) storageAdminDenied(ctx context.Context) bool {
	caller := auth.IdentityFromContext(ctx)
	return caller == nil || !caller.Can("system.admin")
}

func (s *apiServer) ListStorageSweeps(ctx context.Context, req openapi.ListStorageSweepsRequestObject) (openapi.ListStorageSweepsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListStorageSweeps401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.storageReadDenied(ctx) {
		return openapi.ListStorageSweeps403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: storage.CapStorageRead + " required"},
		}, nil
	}
	var limit, offset int32
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	if req.Params.Offset != nil {
		offset = int32(*req.Params.Offset)
	}
	runs, err := s.storageAdmin.ListSweepRuns(ctx, req.Params.Kind, limit, offset)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.StorageSweep, 0, len(runs))
	for _, r := range runs {
		items = append(items, sweepToAPI(r))
	}
	return openapi.ListStorageSweeps200JSONResponse{Items: items}, nil
}

func (s *apiServer) TriggerStorageSweep(ctx context.Context, req openapi.TriggerStorageSweepRequestObject) (openapi.TriggerStorageSweepResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.TriggerStorageSweep401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.storageAdminDenied(ctx) {
		return openapi.TriggerStorageSweep403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.TriggerStorageSweep400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "body required"},
		}, nil
	}
	kind := string(req.Body.Kind)
	ref := caller.UserRef
	runID, err := s.storageAdmin.TriggerSweep(ctx, kind, &ref)
	if err != nil {
		return openapi.TriggerStorageSweep400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	runs, err := s.storageAdmin.ListSweepRuns(ctx, &kind, 1, 0)
	if err != nil || len(runs) == 0 {
		return openapi.TriggerStorageSweep202JSONResponse{Id: runID, Kind: kind, Status: "running"}, nil
	}
	return openapi.TriggerStorageSweep202JSONResponse(sweepToAPI(runs[0])), nil
}

func (s *apiServer) ListStorageSweepFindings(ctx context.Context, req openapi.ListStorageSweepFindingsRequestObject) (openapi.ListStorageSweepFindingsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListStorageSweepFindings401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.storageReadDenied(ctx) {
		return openapi.ListStorageSweepFindings403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: storage.CapStorageRead + " required"},
		}, nil
	}
	var limit, offset int32
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	if req.Params.Offset != nil {
		offset = int32(*req.Params.Offset)
	}
	rows, total, err := s.storageAdmin.ListSweepFindings(ctx, req.Id, req.Params.Finding, limit, offset)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.StorageSweepFinding, 0, len(rows))
	for _, r := range rows {
		items = append(items, openapi.StorageSweepFinding{
			Id:         r.ID,
			Finding:    openapi.StorageSweepFindingFinding(r.Finding),
			ObjectHash: r.ObjectHash,
			VariantKey: r.VariantKey,
			Detail:     r.Detail,
			DetectedAt: r.DetectedAt,
			ResolvedAt: r.ResolvedAt,
		})
	}
	return openapi.ListStorageSweepFindings200JSONResponse{Items: items, Total: total}, nil
}

func sweepToAPI(r storage.SweepRun) openapi.StorageSweep {
	return openapi.StorageSweep{
		Id:             r.ID,
		Kind:           r.Kind,
		Status:         openapi.StorageSweepStatus(r.Status),
		ObjectsScanned: r.ObjectsScanned,
		FindingsCount:  r.FindingsCount,
		StartedAt:      r.StartedAt,
		FinishedAt:     r.FinishedAt,
		Error:          r.Error,
	}
}

func (s *apiServer) GetStorageUsage(ctx context.Context, _ openapi.GetStorageUsageRequestObject) (openapi.GetStorageUsageResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetStorageUsage401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.storageReadDenied(ctx) {
		return openapi.GetStorageUsage403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: storage.CapStorageRead + " required"},
		}, nil
	}
	u, err := s.storageAdmin.GetUsage(ctx)
	if err != nil {
		return nil, err
	}
	cts := make([]openapi.StorageContentTypeBucket, 0, len(u.ByContentType))
	for _, r := range u.ByContentType {
		cts = append(cts, openapi.StorageContentTypeBucket{
			ContentType:  r.ContentType,
			VariantCount: r.VariantCount,
			TotalBytes:   r.TotalBytes,
		})
	}
	backends := make([]openapi.StorageBackendBucket, 0, len(u.ByBackend))
	for _, r := range u.ByBackend {
		backends = append(backends, openapi.StorageBackendBucket{
			Backend:     r.Backend,
			ObjectCount: r.ObjectCount,
		})
	}
	return openapi.GetStorageUsage200JSONResponse{
		ObjectCount:     u.ObjectCount,
		VariantCount:    u.VariantCount,
		TotalBytes:      u.TotalBytes,
		OriginalBytes:   u.OriginalBytes,
		DerivativeBytes: u.DerivativeBytes,
		ByContentType:   cts,
		ByBackend:       backends,
	}, nil
}

func (s *apiServer) ListStorageVariantFamilies(ctx context.Context, _ openapi.ListStorageVariantFamiliesRequestObject) (openapi.ListStorageVariantFamiliesResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListStorageVariantFamilies401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.storageReadDenied(ctx) {
		return openapi.ListStorageVariantFamilies403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: storage.CapStorageRead + " required"},
		}, nil
	}
	rows, err := s.storageAdmin.ListFamilies(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.StorageVariantFamily, 0, len(rows))
	for _, r := range rows {
		items = append(items, openapi.StorageVariantFamily{
			Family:       r.Family,
			VariantCount: r.VariantCount,
			DistinctKeys: r.DistinctKeys,
			ObjectCount:  r.ObjectCount,
			TotalBytes:   r.TotalBytes,
			NewestAt:     r.NewestAt,
		})
	}
	return openapi.ListStorageVariantFamilies200JSONResponse{Items: items}, nil
}

func (s *apiServer) jobsReadDenied(ctx context.Context) bool {
	caller := auth.IdentityFromContext(ctx)
	return caller == nil || (!caller.Can(jobs.CapJobsRead) && !caller.Can("system.admin"))
}

func (s *apiServer) ListJobs(ctx context.Context, req openapi.ListJobsRequestObject) (openapi.ListJobsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListJobs401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.jobsReadDenied(ctx) {
		return openapi.ListJobs403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: jobs.CapJobsRead + " required"},
		}, nil
	}
	f := jobs.ListJobsFilter{}
	if req.Params.Status != nil {
		v := *req.Params.Status
		f.Status = &v
	}
	if req.Params.Type != nil {
		v := *req.Params.Type
		f.Type = &v
	}
	if req.Params.Limit != nil {
		f.Limit = int32(*req.Params.Limit)
	}
	if req.Params.Offset != nil {
		f.Offset = int32(*req.Params.Offset)
	}
	rows, total, err := s.jobsAdmin.ListJobs(ctx, f)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.JobSummary, 0, len(rows))
	for _, r := range rows {
		items = append(items, jobSummaryToAPI(r))
	}
	return openapi.ListJobs200JSONResponse{Items: items, Total: total}, nil
}

func (s *apiServer) ListJobWorkers(ctx context.Context, req openapi.ListJobWorkersRequestObject) (openapi.ListJobWorkersResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListJobWorkers401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.jobsReadDenied(ctx) {
		return openapi.ListJobWorkers403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: jobs.CapJobsRead + " required"},
		}, nil
	}
	rows, err := s.jobsAdmin.ListActiveWorkers(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.JobWorker, 0, len(rows))
	for _, r := range rows {
		items = append(items, openapi.JobWorker{
			ClaimedBy:      r.ClaimedBy,
			JobId:          openapi_types.UUID(r.JobID),
			Type:           r.Type,
			Priority:       int(r.Priority),
			Attempts:       int(r.Attempts),
			ClaimedAt:      r.ClaimedAt,
			LeaseExpiresAt: r.LeaseExpiresAt,
			LeaseStale:     r.LeaseStale,
		})
	}
	return openapi.ListJobWorkers200JSONResponse{Items: items}, nil
}

func (s *apiServer) ListJobStatusCounts(ctx context.Context, req openapi.ListJobStatusCountsRequestObject) (openapi.ListJobStatusCountsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListJobStatusCounts401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if s.jobsReadDenied(ctx) {
		return openapi.ListJobStatusCounts403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: jobs.CapJobsRead + " required"},
		}, nil
	}
	rows, err := s.jobsAdmin.StatusCounts(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.JobStatusCount, 0, len(rows))
	for _, r := range rows {
		items = append(items, openapi.JobStatusCount{Type: r.Type, Status: r.Status, Count: r.Count})
	}
	return openapi.ListJobStatusCounts200JSONResponse{Items: items}, nil
}

func jobSummaryToAPI(r jobs.JobRow) openapi.JobSummary {
	out := openapi.JobSummary{
		Id:             openapi_types.UUID(r.ID),
		Type:           r.Type,
		Status:         openapi.JobSummaryStatus(r.Status),
		Priority:       int(r.Priority),
		Attempts:       int(r.Attempts),
		MaxAttempts:    int(r.MaxAttempts),
		ClaimedBy:      r.ClaimedBy,
		ClaimedAt:      r.ClaimedAt,
		LeaseExpiresAt: r.LeaseExpiresAt,
		LastError:      r.LastError,
		ScheduledFor:   r.ScheduledFor,
		EnqueuedAt:     r.EnqueuedAt,
		StartedAt:      r.StartedAt,
		FinishedAt:     r.FinishedAt,
		AgeSeconds:     r.AgeSeconds,
	}
	if r.OriginServerID != nil {
		id := openapi_types.UUID(*r.OriginServerID)
		out.OriginServerId = &id
	}
	return out
}

// --- jobs admin actions (#401, v0.4.0 Sprint 1) -----------------------
//
// Reads (scheduled, concurrency) gate on jobs.CapJobsRead OR system.admin
// via jobsReadDenied above. MUTATIONS (requeue/cancel, set-concurrency)
// gate on system.admin only — mirrors the metadata-extraction split
// (reads on the read cap, writes on system.admin).

func (s *apiServer) adminDenied(ctx context.Context) bool {
	caller := auth.IdentityFromContext(ctx)
	return caller == nil || !caller.Can("system.admin")
}

func (s *apiServer) RequeueJob(ctx context.Context, req openapi.RequeueJobRequestObject) (openapi.RequeueJobResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.RequeueJob401JSONResponse{UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"}}, nil
	}
	if s.adminDenied(ctx) {
		return openapi.RequeueJob403JSONResponse{ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"}}, nil
	}
	switch err := s.jobsAdmin.RequeueJob(ctx, uuid.UUID(req.Id)); {
	case err == nil:
		return openapi.RequeueJob204Response{}, nil
	case errors.Is(err, jobs.ErrJobNotFound):
		return openapi.RequeueJob404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "job not found"}}, nil
	case errors.Is(err, jobs.ErrJobNotActionable):
		return openapi.RequeueJob409JSONResponse{ConflictJSONResponse: openapi.ConflictJSONResponse{Error: "job is not failed or cancelled; cannot requeue"}}, nil
	default:
		return nil, err
	}
}

func (s *apiServer) CancelJob(ctx context.Context, req openapi.CancelJobRequestObject) (openapi.CancelJobResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.CancelJob401JSONResponse{UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"}}, nil
	}
	if s.adminDenied(ctx) {
		return openapi.CancelJob403JSONResponse{ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"}}, nil
	}
	switch err := s.jobsAdmin.CancelJob(ctx, uuid.UUID(req.Id)); {
	case err == nil:
		return openapi.CancelJob204Response{}, nil
	case errors.Is(err, jobs.ErrJobNotFound):
		return openapi.CancelJob404JSONResponse{NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "job not found"}}, nil
	case errors.Is(err, jobs.ErrJobNotActionable):
		return openapi.CancelJob409JSONResponse{ConflictJSONResponse: openapi.ConflictJSONResponse{Error: "job is running or already finished; cannot cancel"}}, nil
	default:
		return nil, err
	}
}

func (s *apiServer) ListScheduledJobs(ctx context.Context, req openapi.ListScheduledJobsRequestObject) (openapi.ListScheduledJobsResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListScheduledJobs401JSONResponse{UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"}}, nil
	}
	if s.jobsReadDenied(ctx) {
		return openapi.ListScheduledJobs403JSONResponse{ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: jobs.CapJobsRead + " required"}}, nil
	}
	var limit, offset int32 = 50, 0
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	if req.Params.Offset != nil {
		offset = int32(*req.Params.Offset)
	}
	rows, total, err := s.jobsAdmin.ListScheduledJobs(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.ScheduledJob, 0, len(rows))
	for _, r := range rows {
		items = append(items, openapi.ScheduledJob{
			Id:           openapi_types.UUID(r.ID),
			Type:         r.Type,
			Status:       r.Status,
			Priority:     int(r.Priority),
			Attempts:     int(r.Attempts),
			MaxAttempts:  int(r.MaxAttempts),
			ScheduledFor: r.ScheduledFor,
			EnqueuedAt:   r.EnqueuedAt,
			DueInSeconds: r.DueInSeconds,
		})
	}
	return openapi.ListScheduledJobs200JSONResponse{Items: items, Total: total}, nil
}

func (s *apiServer) ListJobConcurrency(ctx context.Context, req openapi.ListJobConcurrencyRequestObject) (openapi.ListJobConcurrencyResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.ListJobConcurrency401JSONResponse{UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"}}, nil
	}
	if s.jobsReadDenied(ctx) {
		return openapi.ListJobConcurrency403JSONResponse{ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: jobs.CapJobsRead + " required"}}, nil
	}
	caps, err := s.sysCfg.GetJobTypeConcurrency(ctx)
	if err != nil {
		return nil, err
	}
	// Canonical type set = registered handlers, unioned with any type
	// that has a cap configured but no live handler (a stale/edited row
	// still shows so the operator can clear it).
	seen := map[string]struct{}{}
	items := make([]openapi.JobConcurrency, 0)
	for _, t := range s.jobsSvc.Registry.Types() {
		ts := string(t)
		seen[ts] = struct{}{}
		items = append(items, openapi.JobConcurrency{Type: ts, Cap: caps[ts]})
	}
	for t, c := range caps {
		if _, ok := seen[t]; !ok {
			items = append(items, openapi.JobConcurrency{Type: t, Cap: c})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Type < items[j].Type })
	return openapi.ListJobConcurrency200JSONResponse{Items: items, AppliesOnRestart: true}, nil
}

func (s *apiServer) SetJobConcurrency(ctx context.Context, req openapi.SetJobConcurrencyRequestObject) (openapi.SetJobConcurrencyResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.SetJobConcurrency401JSONResponse{UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"}}, nil
	}
	if s.adminDenied(ctx) {
		return openapi.SetJobConcurrency403JSONResponse{ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"}}, nil
	}
	if req.Body == nil {
		return openapi.SetJobConcurrency400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"}}, nil
	}
	if req.Body.Cap < 0 || req.Body.Cap > 64 {
		return openapi.SetJobConcurrency400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "cap must be 0-64 (0 = uncapped)"}}, nil
	}
	if err := s.sysCfg.SetJobTypeConcurrency(ctx, req.Type, req.Body.Cap); err != nil {
		return nil, err
	}
	return openapi.SetJobConcurrency204Response{}, nil
}

func metaFailureToAPI(r assetmetadata.FailureRow) openapi.ExtractionFailure {
	out := openapi.ExtractionFailure{
		Id:         openapi_types.UUID(r.ID),
		AssetId:    openapi_types.UUID(r.AssetID),
		Format:     r.Format,
		ErrorKind:  r.ErrorKind,
		Message:    r.Message,
		OccurredAt: r.OccurredAt,
	}
	if r.FieldKey != "" {
		fk := r.FieldKey
		out.FieldKey = &fk
	}
	if len(r.RawValue) > 0 {
		var v any
		if err := json.Unmarshal(r.RawValue, &v); err == nil {
			out.RawValue = v
		}
	}
	if r.DismissedAt != nil {
		t := *r.DismissedAt
		out.DismissedAt = &t
	}
	return out
}

// --- demo-seed loader (post-1.22.D dogfood unblock) -------------------

func (s *apiServer) SeedBackfillTimestamps(ctx context.Context, req openapi.SeedBackfillTimestampsRequestObject) (openapi.SeedBackfillTimestampsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.SeedBackfillTimestamps401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.SeedBackfillTimestamps403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	if req.Body == nil || len(req.Body.Items) == 0 {
		return openapi.SeedBackfillTimestamps400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "items required"},
		}, nil
	}
	items := make([]seed.TimestampItem, 0, len(req.Body.Items))
	for _, it := range req.Body.Items {
		row := seed.TimestampItem{
			Kind:      seed.TimestampKind(it.Kind),
			ID:        uuid.UUID(it.Id),
			CreatedAt: it.CreatedAt,
		}
		if it.UpdatedAt != nil {
			row.UpdatedAt = it.UpdatedAt
		}
		items = append(items, row)
	}
	result, err := s.seedAdmin.BackfillTimestamps(ctx, nil, caller.UserRef, items)
	if err != nil {
		if errors.Is(err, seed.ErrTimestampsBatchTooLarge) {
			return openapi.SeedBackfillTimestamps400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		}
		return nil, err
	}
	return openapi.SeedBackfillTimestamps200JSONResponse{
		AssetUpdated:     result.AssetUpdated,
		PostUpdated:      result.PostUpdated,
		CommentUpdated:   result.CommentUpdated,
		SkippedUnknownId: result.SkippedUnknownID,
	}, nil
}

func (s *apiServer) SeedCreateUser(ctx context.Context, req openapi.SeedCreateUserRequestObject) (openapi.SeedCreateUserResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.SeedCreateUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.SeedCreateUser403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	if req.Body == nil || req.Body.Username == "" {
		return openapi.SeedCreateUser400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "username required"},
		}, nil
	}
	in := seed.UserInput{
		Username: req.Body.Username,
		Approved: true,
	}
	if req.Body.Fullname != nil {
		in.Fullname = req.Body.Fullname
	}
	if req.Body.Email != nil {
		s := string(*req.Body.Email)
		in.Email = &s
	}
	if req.Body.Password != nil {
		in.Password = req.Body.Password
	}
	if req.Body.Usergroup != nil {
		in.Usergroup = req.Body.Usergroup
	}
	if req.Body.Approved != nil {
		in.Approved = *req.Body.Approved
	}
	if req.Body.CreatedAt != nil {
		in.CreatedAt = req.Body.CreatedAt
	}
	result, err := s.seedAdmin.CreateUser(ctx, nil, caller.UserRef, in)
	if err != nil {
		if errors.Is(err, seed.ErrPasswordHasherNotWired) {
			return openapi.SeedCreateUser400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		}
		return nil, err
	}
	resp := openapi.SeedUserResult{
		Ref:            result.Ref,
		Username:       result.Username,
		AlreadyExisted: result.AlreadyExisted,
	}
	if result.AlreadyExisted {
		return openapi.SeedCreateUser200JSONResponse(resp), nil
	}
	return openapi.SeedCreateUser201JSONResponse(resp), nil
}

func (s *apiServer) SeedCreateComment(ctx context.Context, req openapi.SeedCreateCommentRequestObject) (openapi.SeedCreateCommentResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.SeedCreateComment401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can("system.admin") {
		return openapi.SeedCreateComment403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"},
		}, nil
	}
	if req.Body == nil || req.Body.Body == "" {
		return openapi.SeedCreateComment400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "body required"},
		}, nil
	}
	in := seed.CommentInput{
		TargetKind:    seed.CommentTargetKind(req.Body.TargetKind),
		TargetID:      uuid.UUID(req.Body.TargetId),
		AuthorUserRef: req.Body.AuthorUserRef,
		Body:          req.Body.Body,
	}
	if req.Body.Id != nil {
		id := uuid.UUID(*req.Body.Id)
		in.ID = &id
	}
	if req.Body.ParentId != nil {
		pid := uuid.UUID(*req.Body.ParentId)
		in.ParentID = &pid
	}
	if req.Body.BodyHtml != nil {
		in.BodyHTML = *req.Body.BodyHtml
	}
	if req.Body.AnnotationType != nil {
		s := string(*req.Body.AnnotationType)
		in.AnnotationType = &s
	}
	if req.Body.AnnotationData != nil {
		// AnnotationData is a freeform map per the openapi spec.
		// Re-marshal to []byte for sqlc's jsonb column.
		if b, err := json.Marshal(*req.Body.AnnotationData); err == nil {
			in.AnnotationData = b
		}
	}
	if req.Body.CreatedAt != nil {
		in.CreatedAt = req.Body.CreatedAt
	}
	result, err := s.seedAdmin.CreateComment(ctx, nil, caller.UserRef, in)
	if err != nil {
		switch {
		case errors.Is(err, seed.ErrTargetNotFound):
			return openapi.SeedCreateComment404JSONResponse{Error: "comment target not found"}, nil
		case errors.Is(err, seed.ErrAuthorNotFound):
			return openapi.SeedCreateComment404JSONResponse{Error: "forged author user not found"}, nil
		}
		return nil, err
	}
	apiComment := seedResultToAPI(result)
	if result.AlreadyExisted {
		return openapi.SeedCreateComment200JSONResponse(apiComment), nil
	}
	return openapi.SeedCreateComment201JSONResponse(apiComment), nil
}

func seedResultToAPI(r seed.CommentResult) openapi.Comment {
	out := openapi.Comment{
		Id:            openapi_types.UUID(r.ID),
		TargetKind:    openapi.CommentTargetKind(r.TargetKind),
		TargetId:      openapi_types.UUID(r.TargetID),
		RootId:        openapi_types.UUID(r.RootID),
		Depth:         int(r.Depth),
		AuthorUserRef: r.AuthorUserRef,
		Body:          r.Body,
		BodyHtml:      r.BodyHTML,
		LikeCount:     r.LikeCount,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
	if r.ParentID != nil {
		pid := openapi_types.UUID(*r.ParentID)
		out.ParentId = &pid
	}
	if r.AnnotationType != nil {
		at := openapi.CommentAnnotationType(*r.AnnotationType)
		out.AnnotationType = &at
	}
	if len(r.AnnotationData) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(r.AnnotationData, &m); err == nil {
			out.AnnotationData = &m
		}
	}
	return out
}

func (s *apiServer) PreviewFederationPeerDefederation(ctx context.Context, req openapi.PreviewFederationPeerDefederationRequestObject) (openapi.PreviewFederationPeerDefederationResponseObject, error) {
	return s.sharesAdmin.PreviewFederationPeerDefederation(ctx, req)
}

func (s *apiServer) PostFederationHandshake(ctx context.Context, req openapi.PostFederationHandshakeRequestObject) (openapi.PostFederationHandshakeResponseObject, error) {
	return s.peersPublic.PostFederationHandshake(ctx, req)
}

func (s *apiServer) InitiateFederationHandshake(ctx context.Context, req openapi.InitiateFederationHandshakeRequestObject) (openapi.InitiateFederationHandshakeResponseObject, error) {
	return s.peersHandshake.InitiateFederationHandshake(ctx, req)
}

func (s *apiServer) ListFederationPendingInbound(ctx context.Context, req openapi.ListFederationPendingInboundRequestObject) (openapi.ListFederationPendingInboundResponseObject, error) {
	return s.peersHandshake.ListFederationPendingInbound(ctx, req)
}

func (s *apiServer) AcceptFederationPeer(ctx context.Context, req openapi.AcceptFederationPeerRequestObject) (openapi.AcceptFederationPeerResponseObject, error) {
	return s.peersHandshake.AcceptFederationPeer(ctx, req)
}

// --- federation peers admin (Phase 1.22.B-a) -----------------------------

func (s *apiServer) ListFederationPeers(ctx context.Context, req openapi.ListFederationPeersRequestObject) (openapi.ListFederationPeersResponseObject, error) {
	return s.peersAdmin.ListFederationPeers(ctx, req)
}

func (s *apiServer) GetFederationPeer(ctx context.Context, req openapi.GetFederationPeerRequestObject) (openapi.GetFederationPeerResponseObject, error) {
	return s.peersAdmin.GetFederationPeer(ctx, req)
}

func (s *apiServer) CreateFederationPeer(ctx context.Context, req openapi.CreateFederationPeerRequestObject) (openapi.CreateFederationPeerResponseObject, error) {
	return s.peersAdmin.CreateFederationPeer(ctx, req)
}

func (s *apiServer) UpdateFederationPeer(ctx context.Context, req openapi.UpdateFederationPeerRequestObject) (openapi.UpdateFederationPeerResponseObject, error) {
	return s.peersAdmin.UpdateFederationPeer(ctx, req)
}

func (s *apiServer) DeleteFederationPeer(ctx context.Context, req openapi.DeleteFederationPeerRequestObject) (openapi.DeleteFederationPeerResponseObject, error) {
	return s.peersAdmin.DeleteFederationPeer(ctx, req)
}

// --- federation user-keys rotation + health (Phase 1.22.I-h) -------------

func (s *apiServer) RotateOwnFederationKeys(ctx context.Context, req openapi.RotateOwnFederationKeysRequestObject) (openapi.RotateOwnFederationKeysResponseObject, error) {
	return s.userKeysAdmin.RotateOwnFederationKeys(ctx, req)
}

func (s *apiServer) RotateUserFederationKeysAsAdmin(ctx context.Context, req openapi.RotateUserFederationKeysAsAdminRequestObject) (openapi.RotateUserFederationKeysAsAdminResponseObject, error) {
	return s.userKeysAdmin.RotateUserFederationKeysAsAdmin(ctx, req)
}

func (s *apiServer) GetFederationKeyHealth(ctx context.Context, req openapi.GetFederationKeyHealthRequestObject) (openapi.GetFederationKeyHealthResponseObject, error) {
	return s.userKeysAdmin.GetFederationKeyHealth(ctx, req)
}

// --- activities admin audit (Phase 1.22.A-bis-3b) ------------------------

func (s *apiServer) ListAdminActivities(ctx context.Context, req openapi.ListAdminActivitiesRequestObject) (openapi.ListAdminActivitiesResponseObject, error) {
	return s.activitiesAdmin.ListAdminActivities(ctx, req)
}

// --- direct messages (Phase 1.17.I-a) -------------------------------------

func (s *apiServer) ListMyDirectMessageThreads(ctx context.Context, req openapi.ListMyDirectMessageThreadsRequestObject) (openapi.ListMyDirectMessageThreadsResponseObject, error) {
	return s.messages.ListMyDirectMessageThreads(ctx, req)
}

func (s *apiServer) GetMyUnreadDirectMessageCount(ctx context.Context, req openapi.GetMyUnreadDirectMessageCountRequestObject) (openapi.GetMyUnreadDirectMessageCountResponseObject, error) {
	return s.messages.GetMyUnreadDirectMessageCount(ctx, req)
}

func (s *apiServer) ListDirectMessageThread(ctx context.Context, req openapi.ListDirectMessageThreadRequestObject) (openapi.ListDirectMessageThreadResponseObject, error) {
	return s.messages.ListDirectMessageThread(ctx, req)
}

func (s *apiServer) SendDirectMessage(ctx context.Context, req openapi.SendDirectMessageRequestObject) (openapi.SendDirectMessageResponseObject, error) {
	return s.messages.SendDirectMessage(ctx, req)
}

func (s *apiServer) MarkDirectMessageThreadRead(ctx context.Context, req openapi.MarkDirectMessageThreadReadRequestObject) (openapi.MarkDirectMessageThreadReadResponseObject, error) {
	return s.messages.MarkDirectMessageThreadRead(ctx, req)
}

// --- brush packs (Phase 1.21) ---------------------------------------------

func (s *apiServer) ListBrushPacks(ctx context.Context, req openapi.ListBrushPacksRequestObject) (openapi.ListBrushPacksResponseObject, error) {
	return s.brushpacks.ListBrushPacks(ctx, req)
}
func (s *apiServer) ImportBrushPack(ctx context.Context, req openapi.ImportBrushPackRequestObject) (openapi.ImportBrushPackResponseObject, error) {
	return s.brushpacks.ImportBrushPack(ctx, req)
}
func (s *apiServer) GetBrushPack(ctx context.Context, req openapi.GetBrushPackRequestObject) (openapi.GetBrushPackResponseObject, error) {
	return s.brushpacks.GetBrushPack(ctx, req)
}
func (s *apiServer) DeleteBrushPack(ctx context.Context, req openapi.DeleteBrushPackRequestObject) (openapi.DeleteBrushPackResponseObject, error) {
	return s.brushpacks.DeleteBrushPack(ctx, req)
}
func (s *apiServer) GetBrushPackStamp(ctx context.Context, req openapi.GetBrushPackStampRequestObject) (openapi.GetBrushPackStampResponseObject, error) {
	return s.brushpacks.GetBrushPackStamp(ctx, req)
}

// newAIAdminHandler builds the Phase 1.14.A AI inference admin
// surface. Constructs the Caches + Loader from the shared pool +
// cache registry and passes them into the ai/admin package. Safe
// to call before any AI provider is registered — the admin
// endpoints only read/write configuration; runtime inference
// requires the router (wired separately when providers register).
func newAIAdminHandler(pool *pgxpool.Pool, registry *cache.Registry) *aiadmin.Handler {
	caches := ai.NewCaches(registry)
	loader := ai.NewLoader(pool, caches)
	return aiadmin.NewHandler(pool, loader, caches)
}

// ---------------------------------------------------------------------------
// aiedit adapters (Phase 1.14.E-1)
// ---------------------------------------------------------------------------
//
// Three narrow adapter types so the aiedit package doesn't import
// the assets / sysconfig packages directly. Same dual-type pattern
// as transcribeOrchestratorAdapter above — boot wire is the only
// place all sides meet.

type aieditAssetAdapter struct {
	pool *pgxpool.Pool
}

func (a aieditAssetAdapter) AssetTypeAndSensitivity(
	ctx context.Context,
	id uuid.UUID,
) (assetType int64, sensitivity string, found bool, err error) {
	row := a.pool.QueryRow(ctx,
		`SELECT asset_type, COALESCE(sensitivity, 'public') FROM assets WHERE id = $1`,
		id,
	)
	err = row.Scan(&assetType, &sensitivity)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, "", false, nil
		}
		return 0, "", false, err
	}
	return assetType, sensitivity, true, nil
}

type aieditConfigAdapter struct {
	store *sysconfig.Store
}

func (a aieditConfigAdapter) ImageEditServer(ctx context.Context) (string, error) {
	cfg, err := a.store.GetAIEdit(ctx)
	if err != nil {
		return "", err
	}
	return cfg.ImageEditServer, nil
}

type aieditSourceAdapter struct {
	pool    *pgxpool.Pool
	storage *storage.Service
}

func (a aieditSourceAdapter) FetchSourceImage(
	ctx context.Context,
	id uuid.UUID,
) (data []byte, contentType string, title string, ownerUserRef *int64, ok bool, err error) {
	var (
		fileHash *string
		owner    *int64
	)
	row := a.pool.QueryRow(ctx,
		`SELECT file_hash, owner_user_ref, title FROM assets WHERE id = $1`,
		id,
	)
	if err := row.Scan(&fileHash, &owner, &title); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", "", nil, false, nil
		}
		return nil, "", "", nil, false, err
	}
	if fileHash == nil || *fileHash == "" {
		return nil, "", "", nil, false, fmt.Errorf("aiedit: source asset %s has no file bytes", id)
	}

	reader, info, err := a.storage.Backend.Get(ctx, *fileHash, storage.VariantOriginal)
	if err != nil {
		return nil, "", "", nil, false, fmt.Errorf("aiedit: read source bytes: %w", err)
	}
	defer reader.Close()

	buf, err := io.ReadAll(reader)
	if err != nil {
		return nil, "", "", nil, false, fmt.Errorf("aiedit: copy source bytes: %w", err)
	}
	ct := "application/octet-stream"
	if info != nil && info.ContentType != "" {
		ct = info.ContentType
	}
	return buf, ct, title, owner, true, nil
}

// ---------------------------------------------------------------------------
// asset/metadata adapters (Phase 1.18.A-2)
// ---------------------------------------------------------------------------
//
// Six narrow adapters so the asset/metadata package + its
// ExtractJobHandler don't import assets / metadata / storage
// directly. Same dual-type pattern as transcribeOrchestratorAdapter +
// the aiedit adapters above.

// iiifPairAdapter bridges presentation.Loader → contentsearch.AssetPairSource
// so the Content Search 2.0 handler can substring-scan the same metadata
// pairs the Presentation manifest surfaces. Flattens LangString labels/
// values into plain strings (Content Search matches on raw text).
type iiifPairAdapter struct {
	loader *presentation.Loader
}

func (a iiifPairAdapter) LoadMetadataPairs(ctx context.Context, assetID uuid.UUID, isAnonymous bool) ([]contentsearch.Pair, error) {
	pairs, err := a.loader.LoadMetadataPairs(ctx, assetID, isAnonymous)
	if err != nil {
		return nil, err
	}
	out := make([]contentsearch.Pair, 0, len(pairs))
	for _, p := range pairs {
		var label, value string
		if vs := p.Label["en"]; len(vs) > 0 {
			label = vs[0]
		}
		if vs := p.Value["en"]; len(vs) > 0 {
			value = vs[0]
		}
		out = append(out, contentsearch.Pair{Label: label, Value: value})
	}
	return out, nil
}

type metaSourceAdapter struct {
	pool    *pgxpool.Pool
	storage *storage.Service
}

func (a metaSourceAdapter) LoadSource(ctx context.Context, asset assetmetadata.AssetRef) (io.ReadCloser, string, error) {
	if asset.FileHash == "" {
		return nil, "", fmt.Errorf("asset %s has no file_hash", asset.ID)
	}
	rc, info, err := a.storage.Backend.Get(ctx, asset.FileHash, storage.VariantOriginal)
	if err != nil {
		return nil, "", fmt.Errorf("storage get: %w", err)
	}
	mime := asset.MimeType
	if mime == "" && info != nil {
		mime = info.ContentType
	}
	return rc, mime, nil
}

type metaAssetAdapter struct {
	pool *pgxpool.Pool
}

func (a metaAssetAdapter) GetAssetRef(ctx context.Context, id uuid.UUID) (assetmetadata.AssetRef, bool, error) {
	var (
		ownerRef *int64
		teamID   pgtype.UUID
		fileHash *string
		fileExt  *string
	)
	err := a.pool.QueryRow(ctx, `
		SELECT owner_user_ref, team_id, file_hash, file_extension
		  FROM assets WHERE id = $1
	`, id).Scan(&ownerRef, &teamID, &fileHash, &fileExt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return assetmetadata.AssetRef{}, false, nil
		}
		return assetmetadata.AssetRef{}, false, err
	}
	ref := assetmetadata.AssetRef{
		ID:           id,
		OwnerUserRef: ownerRef,
		FileHash:     deref(fileHash),
	}
	if teamID.Valid {
		t := uuid.UUID(teamID.Bytes)
		ref.OwningTeamID = &t
	}
	if fileExt != nil {
		ref.MimeType = mimeForExt(*fileExt)
	}
	return ref, true, nil
}

// savedSiteURL fetches the front-of-house base URL for permalink
// rendering in saved-search digest emails. Falls back to the
// caller's hostname (populated by nginx at request time) when the
// operator hasn't set the sysconfig Site entry yet — the digest
// still renders, just with a probably-wrong absolute URL until
// they configure it.
func savedSiteURL(ctx context.Context, sc *sysconfig.Store) string {
	if sc == nil {
		return ""
	}
	site, err := sc.GetSite(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimRight(site.BaseURL, "/")
}

// mimeForExt — local copy of preview/preview.go's helper so the
// metadata package doesn't have to import preview. Same six-format
// list the EXIF extractor handles.
func mimeForExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "tif", "tiff":
		return "image/tiff"
	case "webp":
		return "image/webp"
	case "pdf":
		return "application/pdf"
	}
	// Raw camera formats (Phase 1.18.A-3.B). Returns the canonical
	// mediatype the raw extractor's Supports() check looks for;
	// empty string means the extension isn't a known raw and we
	// fall through to the generic octet-stream default.
	if m := rawExtMimeShim(ext); m != "" {
		return m
	}
	return "application/octet-stream"
}

// rawExtMimeShim is a thin wrapper so we don't pull the raw package
// into the per-extension switch above. Keeps the mime mapping in one
// place (raw.MimeTypeForExt) while letting the http package own the
// dispatcher.
func rawExtMimeShim(ext string) string {
	return rawpkg.MimeTypeForExt(ext)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type metaConfigAdapter struct {
	pool *pgxpool.Pool
}

func (a metaConfigAdapter) ListExtractionConfig(ctx context.Context) ([]assetmetadata.FieldExtractionConfig, error) {
	// type + options ride along because the applier's write is
	// type-dependent: a select / tree / multi_select target stores a
	// vocabulary SLUG resolved out of options, not the label the file
	// carried, and a reference target is refused outright.
	//
	// open_vocabulary decides what happens to a term the vocabulary
	// does not have — dropped with a failure row on a closed field,
	// created on an open one (#830). Without it here the applier cannot
	// tell the two apart.
	rows, err := a.pool.Query(ctx, `
		SELECT id, extraction_source, extraction_mode, type, options, open_vocabulary
		  FROM field_definition
		 WHERE extraction_source != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []assetmetadata.FieldExtractionConfig{}
	for rows.Next() {
		var (
			id        pgtype.UUID
			source    string
			mode      string
			fieldType string
			options   []byte
			open      bool
		)
		if err := rows.Scan(&id, &source, &mode, &fieldType, &options, &open); err != nil {
			return nil, err
		}
		out = append(out, assetmetadata.FieldExtractionConfig{
			FieldID:        uuid.UUID(id.Bytes),
			Source:         assetmetadata.CanonicalField(source),
			Mode:           assetmetadata.ExtractionMode(mode),
			FieldType:      fieldType,
			Options:        options,
			OpenVocabulary: open,
		})
	}
	return out, rows.Err()
}

type metaValueReaderAdapter struct {
	pool *pgxpool.Pool
}

func (a metaValueReaderAdapter) GetAssetFieldValue(ctx context.Context, assetID, fieldID uuid.UUID) (assetmetadata.FieldValueSnapshot, bool, error) {
	var (
		valText *string
		valNum  *float64
		valDate pgtype.Timestamptz
		valOpts []string
		setBy   string
	)
	// set_by rides along because the applier's skip_if_set rule is a
	// provenance check, not a presence check (#793). Selecting the
	// three value columns and not this one is what made a default
	// indistinguishable from a human's edit.
	//
	// value_options joins them with #830. Without it a multi_select
	// row read back empty, so skip_if_set never fired and the
	// equal-value short-circuit never hit — every extraction pass over
	// the same file would have rewritten the same keywords.
	// A MIRRORED field (#822) has no row here and never will, so the probe
	// reads the COLUMN instead. Without this branch skip_if_set would see
	// "nothing there" for an asset that plainly has a title, and every
	// extraction pass would overwrite it — the exact defect skip_if_set
	// exists to prevent, reintroduced by the field having moved house.
	if col, ok, err := metadata.MirrorColumnForField(ctx, a.pool, fieldID); err != nil {
		return assetmetadata.FieldValueSnapshot{}, false, err
	} else if ok {
		v, rErr := metadata.ReadMirroredValue(ctx, a.pool, assetID, col)
		if rErr != nil {
			return assetmetadata.FieldValueSnapshot{}, false, rErr
		}
		if v == "" {
			return assetmetadata.FieldValueSnapshot{}, false, nil
		}
		// set_by is `manual`, and that is the honest reading rather than a
		// convenience: a column an operator can edit through the asset form
		// carries no provenance, so extraction must treat what it finds as
		// somebody's decision and leave it alone under skip_if_set.
		return assetmetadata.FieldValueSnapshot{ValueText: &v, SetBy: "manual"}, true, nil
	}

	err := a.pool.QueryRow(ctx, `
		SELECT value_text, value_num, value_date, value_options, set_by
		  FROM asset_field_value
		 WHERE asset_id = $1 AND field_id = $2
	`, assetID, fieldID).Scan(&valText, &valNum, &valDate, &valOpts, &setBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return assetmetadata.FieldValueSnapshot{}, false, nil
		}
		return assetmetadata.FieldValueSnapshot{}, false, err
	}
	out := assetmetadata.FieldValueSnapshot{
		ValueText:    valText,
		ValueNum:     valNum,
		ValueOptions: valOpts,
		SetBy:        setBy,
	}
	if valDate.Valid {
		t := valDate.Time
		out.ValueDate = &t
	}
	return out, true, nil
}

type metaValueWriterAdapter struct {
	pool *pgxpool.Pool
	// meta is held only for cache invalidation: a write that CREATES a
	// vocabulary term has changed a field definition, and every cached
	// copy of that definition — including the extraction-config list
	// this very pipeline reads — is now one write old.
	meta   *metadata.Handler
	logger *slog.Logger
}

// WriteAssetFieldValue persists one extraction-derived value.
//
// Bypasses the metadata.Handler HTTP shape (which checks identity
// caps) and goes at the sqlc queries directly — extraction is
// system-owned, there is no caller identity to check against. It does
// NOT bypass the transaction the API path uses, and since #830 it runs
// the same three statements in it:
//
//	term creation (open vocabularies only) → upsert → history append
//
// One transaction because they are one fact. Two would let an asset
// end up holding a slug for a term that no longer exists, or an audit
// row describing a write that rolled back.
func (a metaValueWriterAdapter) WriteAssetFieldValue(ctx context.Context, p assetmetadata.WriteAssetFieldValueParams) error {
	pgAsset := pgtype.UUID{Bytes: p.AssetID, Valid: true}
	pgField := pgtype.UUID{Bytes: p.FieldID, Valid: true}

	// A MIRRORED field (#822) is written by writing the column it declares.
	// One UPDATE, so no transaction and no history append — the same two
	// decisions the API path makes, obtained from the same helper rather
	// than restated. This is what lets #800 wire `title ← iptc_ObjectName`
	// at all: without it the write would hit the guard trigger.
	//
	// No caller gate here, deliberately. Extraction is system-owned and has
	// no identity to check — the same reason this adapter bypasses the HTTP
	// handler's capability checks for every other field. The gate belongs to
	// the caller-facing path, and that is where mirroredWriteRefusal lives.
	if col, ok, mErr := metadata.MirrorColumnForField(ctx, a.pool, p.FieldID); mErr != nil {
		return fmt.Errorf("metadata extraction: mirror lookup: %w", mErr)
	} else if ok {
		if p.Value.Kind != assetmetadata.ValueKindText {
			return fmt.Errorf("metadata extraction: field %s mirrors assets.%s and takes a text value", p.FieldID, col)
		}
		s := p.Value.Text
		if sanitised := richtext.SanitizeValueText(p.FieldType, &s); sanitised != nil {
			s = *sanitised
		}
		return metadata.WriteMirroredValue(ctx, a.pool, p.AssetID, col, s)
	}

	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("metadata extraction: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := metadata.New(tx)

	upsert := metadata.UpsertAssetFieldValueParams{
		AssetID: pgAsset,
		FieldID: pgField,
		SetBy:   p.SetBy,
	}
	var created []string
	switch p.Value.Kind {
	case assetmetadata.ValueKindText:
		s := p.Value.Text
		// Extraction reads whatever an uploaded file's IPTC/XMP block
		// says, which is attacker-controlled bytes. No shipped field
		// wires extraction to a rich_text field today, but
		// writableFieldTypes permits it, so the write side is closed
		// here too rather than left to the read side alone (#816).
		upsert.ValueText = richtext.SanitizeValueText(p.FieldType, &s)
	case assetmetadata.ValueKindNum:
		n := p.Value.Num
		upsert.ValueNum = &n
	case assetmetadata.ValueKindTime:
		upsert.ValueDate = pgtype.Timestamptz{Time: p.Value.Time, Valid: true}
	case assetmetadata.ValueKindTextList:
		upsert.ValueOptions = p.Value.Options
		if p.OpenVocabulary {
			// Entries the applier could not resolve are still the
			// file's own words. This turns them into terms — under a
			// row lock, against the LIVE options document, so two
			// concurrent extract jobs adding different keywords to the
			// same field both keep theirs.
			res, ensureErr := metadata.EnsureOpenVocabularyTerms(ctx, q, pgField, p.Value.Options)
			if ensureErr != nil {
				return fmt.Errorf("metadata extraction: vocabulary: %w", ensureErr)
			}
			upsert.ValueOptions = res.Slugs
			created = res.Created
		}
	}

	// The previous value, for the audit row's old_value. Read inside
	// the tx and before the upsert, which is the only order in which
	// it is the value being replaced.
	var oldJSON []byte
	prev, err := q.GetAssetFieldValue(ctx, metadata.GetAssetFieldValueParams{
		AssetID: pgAsset,
		FieldID: pgField,
	})
	switch {
	case err == nil:
		oldJSON, _ = metadata.ValueRowJSON(prev.ValueText, prev.ValueNum, prev.ValueDate,
			prev.ValueOptions, prev.ValueRef, p.FieldType)
	case errors.Is(err, pgx.ErrNoRows):
		// First value on this field. old_value stays NULL.
	default:
		return fmt.Errorf("metadata extraction: load previous: %w", err)
	}

	row, err := q.UpsertAssetFieldValue(ctx, upsert)
	if err != nil {
		return fmt.Errorf("metadata extraction: upsert: %w", err)
	}

	// The audit history. It is the ONLY surface on which an operator
	// can see that `iptc` and not a person put a value on an asset, so
	// an extraction that writes silently is an extraction nobody can
	// review. changed_by_user_ref stays NULL: no human did this.
	newJSON, _ := metadata.ValueRowJSON(row.ValueText, row.ValueNum, row.ValueDate,
		row.ValueOptions, row.ValueRef, p.FieldType)
	if err := q.AppendAssetFieldValueHistory(ctx, metadata.AppendAssetFieldValueHistoryParams{
		AssetID:  pgAsset,
		FieldID:  pgField,
		OldValue: oldJSON,
		NewValue: newJSON,
		SetBy:    p.SetBy,
	}); err != nil {
		return fmt.Errorf("metadata extraction: append history: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("metadata extraction: commit: %w", err)
	}

	if len(created) > 0 && a.meta != nil {
		// After the commit — a cache dropped before the write lands
		// repopulates with the pre-write document.
		a.meta.InvalidateFieldVocabulary(ctx, pgField)
		if a.logger != nil {
			a.logger.LogAttrs(ctx, slog.LevelInfo, "metadata.vocabulary.terms_created",
				slog.String("source", "extraction"),
				slog.String("field_id", uuid.UUID(pgField.Bytes).String()),
				slog.Int("count", len(created)),
				slog.String("terms", strings.Join(created, ",")),
			)
		}
	}
	return nil
}

// metaExtractEnqueuer adapts jobs.Service into the
// assetmetadata.ChildEnqueuer contract — when the backfill
// coordinator walks the asset list it asks for one extract
// child per id; this is the seam.
type metaExtractEnqueuer struct {
	svc *jobs.Service
}

func (e metaExtractEnqueuer) EnqueueExtract(ctx context.Context, assetID uuid.UUID) error {
	_, err := e.svc.Enqueue(ctx, assetmetadata.JobTypeExtract,
		assetmetadata.ExtractJobPayload{AssetID: assetID},
		jobs.EnqueueOpts{},
	)
	return err
}

type metaFailureAdapter struct {
	pool *pgxpool.Pool
}

func (a metaFailureAdapter) RecordExtractionFailure(ctx context.Context, p assetmetadata.RecordExtractionFailureParams) error {
	raw, err := json.Marshal(p.RawValue)
	if err != nil || raw == nil {
		raw = []byte(`null`)
	}
	_, err = a.pool.Exec(ctx, `
		INSERT INTO extraction_failure
		    (asset_id, format, error_kind, message, field_key, raw_value)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, p.AssetID, p.Format, p.ErrorKind, p.Message, string(p.FieldKey), raw)
	return err
}
