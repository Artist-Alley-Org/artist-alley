package http

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/resourcetype"
	"github.com/mscrnt/artist-alley/app/internal/storage"
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
	assets       *assets.Handler
}

func newAPIServer(pool *pgxpool.Pool, logger *slog.Logger, scrambleKey string, storageSvc *storage.Service, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder) *apiServer {
	return &apiServer{
		auth:         auth.NewHandler(pool, logger, scrambleKey, 0, sessions, limiter, auditRec),
		resourceType: resourcetype.NewHandler(pool, logger),
		assets:       assets.NewHandler(storageSvc, logger),
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

// --- assets ----------------------------------------------------------------

func (s *apiServer) UploadAsset(ctx context.Context, req openapi.UploadAssetRequestObject) (openapi.UploadAssetResponseObject, error) {
	return s.assets.UploadAsset(ctx, req)
}

func (s *apiServer) DownloadAssetOriginal(ctx context.Context, req openapi.DownloadAssetOriginalRequestObject) (openapi.DownloadAssetOriginalResponseObject, error) {
	return s.assets.DownloadAssetOriginal(ctx, req)
}

func (s *apiServer) DownloadAssetVariant(ctx context.Context, req openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	return s.assets.DownloadAssetVariant(ctx, req)
}
