package resourcetype

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// TestListResourceTypes_Live exercises the handler at the
// StrictServerInterface level: feed in the generated request type,
// inspect the generated response type, no HTTP marshalling.
func TestListResourceTypes_Live(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()
	ensureResourceTypeSeed(t, ctx, pool)

	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	resp, err := h.ListResourceTypes(ctx, openapi.ListResourceTypesRequestObject{})
	if err != nil {
		t.Fatalf("ListResourceTypes: %v", err)
	}

	ok, isOk := resp.(openapi.ListResourceTypes200JSONResponse)
	if !isOk {
		t.Fatalf("expected ListResourceTypes200JSONResponse, got %T", resp)
	}
	if len(ok) < 4 {
		t.Fatalf("expected at least 4 seeded rows, got %d", len(ok))
	}

	want := map[int64]string{1: "Photo", 2: "Document", 3: "Video", 4: "Audio"}
	for ref, name := range want {
		found := false
		for _, rt := range ok {
			if rt.Ref == ref {
				if rt.Name == nil || *rt.Name != name {
					got := "<nil>"
					if rt.Name != nil {
						got = *rt.Name
					}
					t.Errorf("ref=%d: expected name=%q, got %q", ref, name, got)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing seeded ref=%d (%s)", ref, name)
		}
	}
}

// TestListResourceTypes_HTTP exercises the handler at the HTTP layer:
// mount it on a chi router exactly the way the real server does, fire
// a request, and verify the wire bytes match the OpenAPI contract.
func TestListResourceTypes_HTTP(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()
	ensureResourceTypeSeed(t, ctx, pool)

	// The StrictServerInterface now spans every endpoint group; we
	// pad the missing slices with the codegen-supplied Unimplemented
	// (which returns 501 for unsupported methods at the strict-server
	// layer). For the route under test that's a non-issue because
	// only ListResourceTypes is exercised.
	impl := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	strict := openapi.NewStrictHandler(rtOnly{h: impl}, nil)

	router := chi.NewRouter()
	openapi.HandlerFromMux(strict, router)

	req := httptest.NewRequest(http.MethodGet, "/resource_types", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q", ct)
	}

	var rows []openapi.ResourceType
	if err := json.NewDecoder(rr.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) < 4 {
		t.Fatalf("expected at least 4 rows, got %d", len(rows))
	}
}

// rtOnly is a StrictServerInterface implementation that forwards
// ListResourceTypes to the real handler and panics for every other
// method, so a wrong-test-route bug surfaces immediately instead of
// silently returning a "not implemented" response.
type rtOnly struct{ h *Handler }

func (r rtOnly) ListResourceTypes(ctx context.Context, req openapi.ListResourceTypesRequestObject) (openapi.ListResourceTypesResponseObject, error) {
	return r.h.ListResourceTypes(ctx, req)
}
func (rtOnly) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from resourcetype test shim")
}
func (rtOnly) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from resourcetype test shim")
}
func (rtOnly) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from resourcetype test shim")
}
func (rtOnly) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from resourcetype test shim")
}
func (rtOnly) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from resourcetype test shim")
}
func (rtOnly) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from resourcetype test shim")
}
func (rtOnly) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from resourcetype test shim")
}
func (rtOnly) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from resourcetype test shim")
}
func (rtOnly) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from resourcetype test shim")
}
func (rtOnly) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from resourcetype test shim")
}
func (rtOnly) UploadStorageObject(context.Context, openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	panic("UploadStorageObject called from resourcetype test shim")
}
func (rtOnly) DownloadStorageObject(context.Context, openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	panic("DownloadStorageObject called from resourcetype test shim")
}
func (rtOnly) DownloadStorageObjectVariant(context.Context, openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	panic("DownloadStorageObjectVariant called from resourcetype test shim")
}
func (rtOnly) CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	panic("CreateAsset called from resourcetype test shim")
}
func (rtOnly) ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	panic("ListAssets called from resourcetype test shim")
}
func (rtOnly) GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	panic("GetAsset called from resourcetype test shim")
}
func (rtOnly) UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	panic("UpdateAsset called from resourcetype test shim")
}
func (rtOnly) DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	panic("DeleteAsset called from resourcetype test shim")
}
func (rtOnly) DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	panic("DownloadAssetFile called from resourcetype test shim")
}
func (rtOnly) DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	panic("DownloadAssetVariant called from resourcetype test shim")
}
func (rtOnly) AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	panic("AddAssetTags called from resourcetype test shim")
}
func (rtOnly) RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	panic("RemoveAssetTag called from resourcetype test shim")
}
func (rtOnly) GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from resourcetype test shim")
}
func (rtOnly) CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from resourcetype test shim")
}
func (rtOnly) ListWorkflowStates(context.Context, openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from resourcetype test shim")
}
func (rtOnly) ListFields(context.Context, openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	panic("ListFields called from resourcetype test shim")
}
func (rtOnly) CreateField(context.Context, openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	panic("CreateField called from resourcetype test shim")
}
func (rtOnly) GetField(context.Context, openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	panic("GetField called from resourcetype test shim")
}
func (rtOnly) UpdateField(context.Context, openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	panic("UpdateField called from resourcetype test shim")
}
func (rtOnly) ArchiveField(context.Context, openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	panic("ArchiveField called from resourcetype test shim")
}
func (rtOnly) GetAssetFields(context.Context, openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	panic("GetAssetFields called from resourcetype test shim")
}
func (rtOnly) SetAssetFieldValue(context.Context, openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	panic("SetAssetFieldValue called from resourcetype test shim")
}
func (rtOnly) ClearAssetFieldValue(context.Context, openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	panic("ClearAssetFieldValue called from resourcetype test shim")
}
func (rtOnly) GetAssetFieldValueHistory(context.Context, openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	panic("GetAssetFieldValueHistory called from resourcetype test shim")
}
func (rtOnly) ListCollections(context.Context, openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	panic("ListCollections called from resourcetype test shim")
}
func (rtOnly) CreateCollection(context.Context, openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	panic("CreateCollection called from resourcetype test shim")
}
func (rtOnly) GetCollection(context.Context, openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	panic("GetCollection called from resourcetype test shim")
}
func (rtOnly) UpdateCollection(context.Context, openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	panic("UpdateCollection called from resourcetype test shim")
}
func (rtOnly) DeleteCollection(context.Context, openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	panic("DeleteCollection called from resourcetype test shim")
}
func (rtOnly) ListCollectionResources(context.Context, openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	panic("ListCollectionResources called from resourcetype test shim")
}
func (rtOnly) AddCollectionResource(context.Context, openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	panic("AddCollectionResource called from resourcetype test shim")
}
func (rtOnly) RemoveCollectionResource(context.Context, openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	panic("RemoveCollectionResource called from resourcetype test shim")
}
func (rtOnly) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from resourcetype test shim")
}
func (rtOnly) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from resourcetype test shim")
}
func (rtOnly) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from resourcetype test shim")
}
func (rtOnly) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from resourcetype test shim")
}
func (rtOnly) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from resourcetype test shim")
}
func (rtOnly) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from resourcetype test shim")
}
func (rtOnly) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from resourcetype test shim")
}
func (rtOnly) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from resourcetype test shim")
}
func (rtOnly) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from resourcetype test shim")
}
func (rtOnly) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from resourcetype test shim")
}
func (rtOnly) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from resourcetype test shim")
}
func (rtOnly) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from resourcetype test shim")
}
func (rtOnly) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from resourcetype test shim")
}
func (rtOnly) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from resourcetype test shim")
}
func (rtOnly) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from resourcetype test shim")
}
func (rtOnly) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from resourcetype test shim")
}
func (rtOnly) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from resourcetype test shim")
}
func (rtOnly) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from resourcetype test shim")
}
func (rtOnly) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from resourcetype test shim")
}
func (rtOnly) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from resourcetype test shim")
}
func (rtOnly) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from resourcetype test shim")
}
func (rtOnly) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from resourcetype test shim")
}
func (rtOnly) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from resourcetype test shim")
}
func (rtOnly) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from resourcetype test shim")
}
func (rtOnly) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from resourcetype test shim")
}
func (rtOnly) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from resourcetype test shim")
}
func (rtOnly) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from resourcetype test shim")
}
func (rtOnly) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from resourcetype test shim")
}
func (rtOnly) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from resourcetype test shim")
}
func (rtOnly) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from resourcetype test shim")
}
func (rtOnly) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from resourcetype test shim")
}
func (rtOnly) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from resourcetype test shim")
}
func (rtOnly) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from resourcetype test shim")
}
func (rtOnly) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from resourcetype test shim")
}

// --- test helpers -----------------------------------------------------------

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// ensureResourceTypeSeed makes sure the four canonical RS resource
// types exist. Defensive — the table is normally created and seeded by
// CheckDBStruct on the PHP side during install.
func ensureResourceTypeSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	const ensure = `
INSERT INTO resource_type (ref, name, icon)
VALUES (1, 'Photo', 'image'),
       (2, 'Document', 'file-text'),
       (3, 'Video', 'video'),
       (4, 'Audio', 'music')
ON CONFLICT (ref) DO NOTHING
`
	if _, err := pool.Exec(ctx, ensure); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (rtOnly) GetSiteConfig(context.Context, openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	panic("GetSiteConfig called from resourcetype test shim")
}
func (rtOnly) UpdateSiteConfig(context.Context, openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from resourcetype test shim")
}
func (rtOnly) GetSMTPConfig(context.Context, openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from resourcetype test shim")
}
func (rtOnly) UpdateSMTPConfig(context.Context, openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from resourcetype test shim")
}
func (rtOnly) GetAuthConfig(context.Context, openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from resourcetype test shim")
}
func (rtOnly) UpdateAuthConfig(context.Context, openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from resourcetype test shim")
}
func (rtOnly) GetAIConfig(context.Context, openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from resourcetype test shim")
}
func (rtOnly) UpdateAIConfig(context.Context, openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from resourcetype test shim")
}
func (rtOnly) ListLocales(context.Context, openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from resourcetype test shim")
}

func (rtOnly) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from resourcetype test shim")
}
func (rtOnly) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from resourcetype test shim")
}
func (rtOnly) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from resourcetype test shim")
}

// --- jobs stubs (Phase 1.18.A) -------------------------------------------
func (rtOnly) ClaimJobs(context.Context, openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	panic("ClaimJobs called from test shim")
}
func (rtOnly) GetJob(context.Context, openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	panic("GetJob called from test shim")
}
func (rtOnly) HeartbeatJob(context.Context, openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	panic("HeartbeatJob called from test shim")
}
func (rtOnly) CompleteJob(context.Context, openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	panic("CompleteJob called from test shim")
}
func (rtOnly) FailJob(context.Context, openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	panic("FailJob called from test shim")
}
