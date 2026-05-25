package http

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/resourcetype"
	"github.com/mscrnt/artist-alley/app/internal/setup"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
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
	resourceType *resourcetype.Handler
	storage      *storage.Handler
	assets       *assets.Handler
	setup        *setup.Handler
}

func newAPIServer(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, storageSvc *storage.Service, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder, sysCfg *sysconfig.Store, storageBackend string) *apiServer {
	return &apiServer{
		auth:         auth.NewHandler(pool, logger, cfg.ScrambleKey, 0, sessions, limiter, auditRec),
		resourceType: resourcetype.NewHandler(pool, logger),
		storage:      storage.NewHandler(storageSvc, logger),
		assets:       assets.NewHandler(pool, storageSvc, logger),
		setup:        setup.NewHandler(pool, logger, cfg, sysCfg, storageBackend),
	}
}

// --- auth ------------------------------------------------------------------

func (s *apiServer) Login(ctx context.Context, req openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	return s.auth.Login(ctx, req)
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

// --- resource_types --------------------------------------------------------

func (s *apiServer) ListResourceTypes(ctx context.Context, req openapi.ListResourceTypesRequestObject) (openapi.ListResourceTypesResponseObject, error) {
	return s.resourceType.ListResourceTypes(ctx, req)
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

func (s *apiServer) RemoveAssetTag(ctx context.Context, req openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	return s.assets.RemoveAssetTag(ctx, req)
}

// --- setup -----------------------------------------------------------------

func (s *apiServer) GetSetupStatus(ctx context.Context, req openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	return s.setup.GetSetupStatus(ctx, req)
}

func (s *apiServer) CompleteSetup(ctx context.Context, req openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	return s.setup.CompleteSetup(ctx, req)
}
