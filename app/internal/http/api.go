package http

import (
	"context"
	"time"
	"log/slog"

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
	"github.com/mscrnt/artist-alley/app/internal/federation/p2p"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
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
}

func newAPIServer(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, storageSvc *storage.Service, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder, sysCfg *sysconfig.Store, cacheReg *cache.Registry, jobSvc *jobs.Service, licState *licensing.State, storageBackend string) *apiServer {
	s := &apiServer{
		auth:         authHandlerWithPolicy(pool, logger, cfg, sessions, limiter, auditRec, cacheReg, sysCfg),
		resourceType: assettype.NewHandler(pool, logger),
		storage:      storage.NewHandler(storageSvc, logger),
		assets:       assets.NewHandler(pool, storageSvc, logger, jobSvc, cacheReg),
		metadata:     metadata.NewHandler(pool, logger, cacheReg),
		collections:  collections.NewHandler(pool, logger, cacheReg),
		posts:        posts.NewHandler(pool, logger, cacheReg),
		teams:        teams.NewHandler(pool, logger, cacheReg),
		users:        usersHandlerWithAudit(pool, logger, cacheReg, auditRec),
		social:       social.NewHandler(pool, logger, cacheReg),
		setup:        setup.NewHandler(pool, logger, cfg, sysCfg, storageBackend),
		workflow:     workflow.NewHandler(pool, logger, cacheReg),
		sysconfigH:   sysconfig.NewHTTPHandler(pool, sysCfg, logger),
		i18n:         i18n.NewHandler(logger),
		jobs:         jobs.NewHTTPHandler(jobSvc, logger),
		brushpacks:   brushpacks.NewHandler(brushpacks.NewService(pool, storageSvc.Backend)),
		audit:        audit.NewHTTPHandler(pool, logger),
		licensing:    licensing.NewHandler(licState, logger),
		userprefs:    userprefs.NewHandler(pool, logger, cacheReg),
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
	return s
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

// --- cross-package adapters for the notifications wiring ----------

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
func usersHandlerWithAudit(pool *pgxpool.Pool, logger *slog.Logger, cacheReg *cache.Registry, auditRec *audit.Recorder) *users.Handler {
	h := users.NewHandler(pool, logger, cacheReg)
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
	return s.assets.GetAsset(ctx, req)
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

func (s *apiServer) DownloadAssetVariant(ctx context.Context, req openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	return s.assets.DownloadAssetVariant(ctx, req)
}

func (s *apiServer) AddAssetTags(ctx context.Context, req openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	return s.assets.AddAssetTags(ctx, req)
}

func (s *apiServer) RecreateAssetPreview(ctx context.Context, req openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	return s.assets.RecreateAssetPreview(ctx, req)
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
