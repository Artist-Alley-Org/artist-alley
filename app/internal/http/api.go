package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"log/slog"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/brushpacks"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/i18n"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/assettype"
	"github.com/mscrnt/artist-alley/app/internal/setup"
	"github.com/mscrnt/artist-alley/app/internal/social"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/licensing"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/teams"
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation/directory"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/p2p"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	"github.com/mscrnt/artist-alley/app/internal/seed"
	"github.com/mscrnt/artist-alley/app/internal/requests"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
	"github.com/mscrnt/artist-alley/app/internal/ai"
	aiembeddings "github.com/mscrnt/artist-alley/app/internal/ai/embeddings"
	aijobs "github.com/mscrnt/artist-alley/app/internal/ai/jobs"
	aicliplocal "github.com/mscrnt/artist-alley/app/internal/ai/providers/cliplocal"
	aiwhisperlocal "github.com/mscrnt/artist-alley/app/internal/ai/providers/whisper_local"
	aitranscribe "github.com/mscrnt/artist-alley/app/internal/ai/transcribe"
	aiadmin "github.com/mscrnt/artist-alley/app/internal/ai/admin"
	"github.com/mscrnt/artist-alley/app/internal/messages"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
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
	auth         *auth.Handler
	resourceType *assettype.Handler
	storage      *storage.Handler
	assets       *assets.Handler
	metadata     *metadata.Handler
	collections  *collections.Handler
	posts        *posts.Handler
	teams        *teams.Handler
	users        *users.Handler
	social       *social.Handler
	setup        *setup.Handler
	workflow     *workflow.Handler
	sysconfigH   *sysconfig.Handler
	i18n         *i18n.Handler
	jobs         *jobs.HTTPHandler
	brushpacks   *brushpacks.Handler
	audit         *audit.HTTPHandler
	licensing     *licensing.Handler
	userprefs     *userprefs.Handler
	aiAdmin       *aiadmin.Handler // Phase 1.14.A inference subsystem admin surface
	aiBridge      ai.Bridge        // Phase 1.14.A-bridge — read/write seam for AI handlers
	aiRouter      *ai.Router       // Phase 1.14.B — typed inference dispatch w/ registered providers
	notifications   *notifications.Handler
	messages        *messages.Handler
	activities      *activities.Writer
	activitiesAdmin *activities.AdminHandler
	peers           *peer.Registry
	peersAdmin      *peer.AdminHandler
	peersHandshake  *peer.AdminHandshakeHandler
	peersPublic     *peer.PublicHandler
	fedIdentity     *identity.Manager
	fedEngine       *peer.Engine
	directories      *directory.Registry
	directoriesAdmin *directory.AdminHandler
	directoryPoller  *directory.Poller
	p2pRegistry      *p2p.Registry
	p2pAdmin         *p2p.AdminHandler
	sharesRegistry   *shares.Registry
	sharesAdmin      *shares.AdminHandler
	sharesSweeper    *shares.Sweeper
	inboxHandler     *inbox.Handler
	inboxDispatcher  *inbox.Dispatcher
	outboxDispatcher *outbox.Dispatcher
	outboxDelivery   *outbox.Worker
	outboxAdmin      *outbox.AdminHandler
	userKeysSweeper    *userkeys.Sweeper
	userKeysAdmin      *userkeys.AdminHandler
	capabilitySweeper  *auth.CapabilitySweeper
	requests           *requests.Handler
	requestsHTTP       *requests.HTTPHandler
	subtitles        *subtitles.Handler
	subtitlesHTTP    *subtitles.HTTPHandler
	seedAdmin        *seed.AdminHandler
}

func newAPIServer(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, storageSvc *storage.Service, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder, sysCfg *sysconfig.Store, cacheReg *cache.Registry, jobSvc *jobs.Service, licState *licensing.State, storageBackend string) *apiServer {
	s := &apiServer{
		auth:         authHandlerWithPolicy(pool, logger, cfg, sessions, limiter, auditRec, cacheReg, sysCfg),
		resourceType: assettype.NewHandler(pool, logger),
		storage:      storage.NewHandler(storageSvc, logger),
		assets:       assets.NewHandler(pool, storageSvc, logger, jobSvc, cacheReg),
		subtitles:    subtitles.NewHandler(pool, cacheReg, logger),
		metadata:     metadata.NewHandler(pool, logger, cacheReg),
		collections:  collections.NewHandler(pool, logger, cacheReg),
		posts:        posts.NewHandler(pool, logger, cacheReg),
		teams:        teams.NewHandler(pool, logger, cacheReg),
		users:        usersHandlerWithAudit(pool, logger, cacheReg, auditRec, sessions),
		social:       social.NewHandler(pool, logger, cacheReg),
		setup:        setup.NewHandler(pool, logger, cfg, sysCfg, storageBackend, auditRec),
		workflow:     workflow.NewHandler(pool, logger, cacheReg),
		sysconfigH:   sysconfigHandlerWithAudit(pool, sysCfg, logger, auditRec),
		i18n:         i18n.NewHandler(logger),
		jobs:         jobs.NewHTTPHandler(jobSvc, logger),
		brushpacks:   brushpacks.NewHandler(brushpacks.NewService(pool, storageSvc.Backend)),
		audit:        audit.NewHTTPHandler(pool, logger),
		licensing:    licensing.NewHandler(licState, logger),
		userprefs:    userprefs.NewHandler(pool, logger, cacheReg),
		aiAdmin:      newAIAdminHandler(pool, cacheReg),
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
	// system_config registration (migration 00012) means the
	// admin UI gates the runtime call until the operator flips it.
	s.aiRouter.Register(aiwhisperlocal.NewProvider(aiwhisperlocal.Config{}, aiCallAuditor))

	// Phase 1.14.B — register ai.embed job handler so the worker
	// pool can drain ai.embed jobs enqueued by the asset upload
	// fanout. Privacy policy + default model load best-effort from
	// system_config; defaults if the rows aren't present yet
	// (pre-migration state) so boot doesn't wedge.
	aiPrivacyCfg, _ := aiLoader.Load(context.Background())
	defaultEmbedModel := "nomic-embed-text"
	if jobSvc != nil {
		jobSvc.Registry.Register(aijobs.NewEmbedHandler(
			s.aiRouter,
			s.assets,         // ai.AssetLookup (bridge)
			s.aiBridge.EmbeddingWriter,
			aiPrivacyCfg.Privacy,
			defaultEmbedModel,
		))

		// Phase 1.14.C — ai.transcribe handler. The full
		// extract→chunk→route→stitch→VTT→subtitle pipeline lives in
		// transcribe.Handler; the job handler is a thin wrapper that
		// parses the payload + classifies errors. Operator's
		// chunker config (system_config seeds from 00012) flows in
		// via the Config struct.
		transcribeOrch := aitranscribe.NewHandler(
			transcribeStorage,    // same storage adapter as the Writer
			s.subtitles,
			s.aiRouter,
			s.assets,
			aiPrivacyCfg.Privacy,
			logger,
			"", // tempDir — defaults to os.TempDir()
			aitranscribe.Config{}, // empty → handler picks 25/5 defaults
		)
		jobSvc.Registry.Register(aijobs.NewTranscribeHandler(
			transcribeOrchestratorAdapter{orch: transcribeOrch},
		))
	}

	// Wire the social-graph seam into posts so visibility='followers'
	// gating consults the new follows table (Phase 1.17.G2). Done
	// post-construction since the two handlers are siblings in the
	// struct literal and a direct cross-reference there would be
	// awkward to read.
	s.posts.SetFollowChecker(s.social)

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
		ownerResolverFor(pool),
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
	// objects to their sensitivity tier (migration 00014); other
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
	s.requestsHTTP = requests.NewHTTPHandler(s.requests, logger)
	s.capabilitySweeper.SetRequestCascade(s.requests.MarkExpired)

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

// ownerResolverFor returns the shares.ObjectOwnerResolver closure
// boot wires into the shares admin handler. Checks the per-
// domain owner columns: posts.author_user_ref,
// collections.owner_user_ref, assets.owner_user_ref. Unknown
// kinds default to "no" — system.admin still wins at the caller
// because the resolver short-circuits before this is called.
func ownerResolverFor(pool *pgxpool.Pool) shares.ObjectOwnerResolver {
	return func(ctx context.Context, kind federation.ShareObjectKind, objectID uuid.UUID, caller *auth.Identity) (bool, error) {
		var column, table string
		switch kind {
		case federation.ShareObjectKindPost:
			table, column = "posts", "author_user_ref"
		case federation.ShareObjectKindCollection:
			table, column = "collections", "owner_user_ref"
		case federation.ShareObjectKindAsset:
			table, column = "assets", "owner_user_ref"
		default:
			// workspace + brand_kit tables don't exist yet;
			// user-kind shares are server-internal (Accept(Follow)
			// path). Reject ownership claims for these.
			return false, nil
		}
		var ownerRef int64
		err := pool.QueryRow(ctx,
			"SELECT "+column+" FROM "+table+" WHERE id = $1",
			objectID,
		).Scan(&ownerRef)
		if err != nil {
			return false, nil // unknown object → reject (caller maps to 404)
		}
		return ownerRef == caller.UserRef, nil
	}
}

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
func sysconfigHandlerWithAudit(pool *pgxpool.Pool, store *sysconfig.Store, logger *slog.Logger, auditRec *audit.Recorder) *sysconfig.Handler {
	h := sysconfig.NewHTTPHandler(pool, store, logger)
	h.SetAuditRecorder(auditRec)
	return h
}

// authHandlerWithPolicy mirrors usersHandlerWithAudit — composes the
// post-construction setters so the existing positional NewHandler
// signature doesn't need to grow another arg.
func authHandlerWithPolicy(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder, cacheReg *cache.Registry, sysCfg *sysconfig.Store) *auth.Handler {
	h := auth.NewHandler(pool, logger, cfg.ScrambleKey, 0, sessions, limiter, auditRec, cacheReg)
	h.SetPasswordPolicySource(passwordPolicyAdapter{store: sysCfg})
	return h
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
	return s.assets.CreateAsset(ctx, req)
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
	return s.assets.UpdateAsset(ctx, req)
}

func (s *apiServer) DeleteAsset(ctx context.Context, req openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	return s.assets.DeleteAsset(ctx, req)
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
	return s.collections.CreateCollection(ctx, req)
}
func (s *apiServer) GetCollection(ctx context.Context, req openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	return s.collections.GetCollection(ctx, req)
}
func (s *apiServer) UpdateCollection(ctx context.Context, req openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	return s.collections.UpdateCollection(ctx, req)
}
func (s *apiServer) DeleteCollection(ctx context.Context, req openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	return s.collections.DeleteCollection(ctx, req)
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

// --- posts -----------------------------------------------------------------

func (s *apiServer) ListPosts(ctx context.Context, req openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	return s.posts.ListPosts(ctx, req)
}
func (s *apiServer) CreatePost(ctx context.Context, req openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	return s.posts.CreatePost(ctx, req)
}
func (s *apiServer) GetPost(ctx context.Context, req openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	return s.posts.GetPost(ctx, req)
}
func (s *apiServer) UpdatePost(ctx context.Context, req openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	return s.posts.UpdatePost(ctx, req)
}
func (s *apiServer) DeletePost(ctx context.Context, req openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	return s.posts.DeletePost(ctx, req)
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
func (s *apiServer) GetMyTeams(ctx context.Context, req openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	return s.teams.GetMyTeams(ctx, req)
}

// --- users -----------------------------------------------------------------

func (s *apiServer) GetUserPublicByRef(ctx context.Context, req openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	return s.users.GetUserPublicByRef(ctx, req)
}
func (s *apiServer) GetUserPublicByUsername(ctx context.Context, req openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	return s.users.GetUserPublicByUsername(ctx, req)
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
func (s *apiServer) ListOwnRequests(ctx context.Context, req openapi.ListOwnRequestsRequestObject) (openapi.ListOwnRequestsResponseObject, error) {
	return s.requestsHTTP.ListOwnRequests(ctx, req)
}
func (s *apiServer) ListAdminRequests(ctx context.Context, req openapi.ListAdminRequestsRequestObject) (openapi.ListAdminRequestsResponseObject, error) {
	return s.requestsHTTP.ListAdminRequests(ctx, req)
}
func (s *apiServer) DecideAdminRequest(ctx context.Context, req openapi.DecideAdminRequestRequestObject) (openapi.DecideAdminRequestResponseObject, error) {
	return s.requestsHTTP.DecideAdminRequest(ctx, req)
}

// --- setup -----------------------------------------------------------------

func (s *apiServer) GetSetupStatus(ctx context.Context, req openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	return s.setup.GetSetupStatus(ctx, req)
}

func (s *apiServer) CompleteSetup(ctx context.Context, req openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	return s.setup.CompleteSetup(ctx, req)
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
func (s *apiServer) GetPublicAppearance(ctx context.Context, req openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	return s.sysconfigH.GetPublicAppearance(ctx, req)
}

// --- i18n ------------------------------------------------------------------

func (s *apiServer) ListLocales(ctx context.Context, req openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	return s.i18n.ListLocales(ctx, req)
}

// --- audit viewer (Phase 1.17.K) ------------------------------------------

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
		PeerId:          openapi_types.UUID(req.Id),
		CancelledCount:  count,
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
