package http

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
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
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/teams"
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
}

func newAPIServer(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, storageSvc *storage.Service, sessions *auth.SessionManager, limiter *auth.LoginLimiter, auditRec *audit.Recorder, sysCfg *sysconfig.Store, cacheReg *cache.Registry, jobSvc *jobs.Service, storageBackend string) *apiServer {
	return &apiServer{
		auth:         auth.NewHandler(pool, logger, cfg.ScrambleKey, 0, sessions, limiter, auditRec, cacheReg),
		resourceType: assettype.NewHandler(pool, logger),
		storage:      storage.NewHandler(storageSvc, logger),
		assets:       assets.NewHandler(pool, storageSvc, logger, jobSvc, cacheReg),
		metadata:     metadata.NewHandler(pool, logger, cacheReg),
		collections:  collections.NewHandler(pool, logger, cacheReg),
		posts:        posts.NewHandler(pool, logger, cacheReg),
		teams:        teams.NewHandler(pool, logger, cacheReg),
		users:        users.NewHandler(pool, logger, cacheReg),
		social:       social.NewHandler(pool, logger),
		setup:        setup.NewHandler(pool, logger, cfg, sysCfg, storageBackend),
		workflow:     workflow.NewHandler(pool, logger, cacheReg),
		sysconfigH:   sysconfig.NewHTTPHandler(pool, sysCfg, logger),
		i18n:         i18n.NewHandler(logger),
		jobs:         jobs.NewHTTPHandler(jobSvc, logger),
	}
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

// --- asset_types --------------------------------------------------------

func (s *apiServer) ListAssetTypes(ctx context.Context, req openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	return s.resourceType.ListAssetTypes(ctx, req)
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
