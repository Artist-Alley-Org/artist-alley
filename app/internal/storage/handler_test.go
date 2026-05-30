package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// End-to-end integration: real DB + fs backend, wired through the
// strict-handler chain just like the production server.
func TestUploadDownload_RoundTrip(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	pool := openPool(t, pwd)
	defer pool.Close()

	root := t.TempDir()
	backend, err := storagefs.New(root)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	svc.GCGracePeriod = time.Second

	const userRef int64 = 424242
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := storage.NewHandler(svc, logger)

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session"}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{handler}, nil), router)

	payload := make([]byte, 12*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(want[:])

	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM storage_pins WHERE pin_subject_type='user' AND pin_subject_id=$1`, "424242")
		_, _ = pool.Exec(ctx, `DELETE FROM storage_variants WHERE object_hash=$1`, wantHex)
		_, _ = pool.Exec(ctx, `DELETE FROM storage_objects WHERE hash=$1`, wantHex)
	})

	// --- upload ---
	req := httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var ur openapi.UploadResult
	if err := json.Unmarshal(rr.Body.Bytes(), &ur); err != nil {
		t.Fatalf("decode upload result: %v body=%s", err, rr.Body.String())
	}
	if ur.Hash != wantHex {
		t.Errorf("hash mismatch: got %s want %s", ur.Hash, wantHex)
	}
	if ur.Size != int64(len(payload)) {
		t.Errorf("size: got %d want %d", ur.Size, len(payload))
	}
	if ur.ContentType != "text/plain" {
		t.Errorf("content_type: got %q want text/plain", ur.ContentType)
	}
	if ur.Deduped {
		t.Errorf("first upload should not be deduped")
	}

	// --- second upload of same bytes: dedup hit ---
	req2 := httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader(payload))
	req2.Header.Set("Content-Type", "application/octet-stream")
	req2.Header.Set("X-Content-Type", "text/plain")
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second upload: status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var ur2 openapi.UploadResult
	if err := json.Unmarshal(rr2.Body.Bytes(), &ur2); err != nil {
		t.Fatalf("decode second upload: %v", err)
	}
	if !ur2.Deduped {
		t.Errorf("second upload of same bytes should report deduped=true")
	}

	// --- full download ---
	get := httptest.NewRequest(http.MethodGet, "/storage/objects/"+wantHex, nil)
	gr := httptest.NewRecorder()
	router.ServeHTTP(gr, get)
	if gr.Code != http.StatusOK {
		t.Fatalf("download: status=%d body=%s", gr.Code, gr.Body.String())
	}
	if !bytes.Equal(gr.Body.Bytes(), payload) {
		t.Errorf("download body did not match upload (%d vs %d bytes)", gr.Body.Len(), len(payload))
	}

	// --- range download (middle 100 bytes) ---
	rng := httptest.NewRequest(http.MethodGet, "/storage/objects/"+wantHex, nil)
	rng.Header.Set("Range", "bytes=10-109")
	rnr := httptest.NewRecorder()
	router.ServeHTTP(rnr, rng)
	if rnr.Code != http.StatusPartialContent {
		t.Fatalf("range download: status=%d body=%s", rnr.Code, rnr.Body.String())
	}
	if rnr.Body.Len() != 100 {
		t.Errorf("range body len=%d want 100", rnr.Body.Len())
	}
	if !bytes.Equal(rnr.Body.Bytes(), payload[10:110]) {
		t.Errorf("range body content mismatch")
	}

	// --- 404 for missing hash ---
	missing := strings.Repeat("0", 64)
	mr := httptest.NewRecorder()
	router.ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/storage/objects/"+missing, nil))
	if mr.Code != http.StatusNotFound {
		t.Errorf("missing hash: status=%d want 404", mr.Code)
	}

	// --- unauthenticated upload ---
	bareRouter := chi.NewRouter()
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{handler}, nil), bareRouter)
	ur3 := httptest.NewRecorder()
	bareRouter.ServeHTTP(ur3, httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader([]byte("x"))))
	if ur3.Code != http.StatusUnauthorized {
		t.Errorf("anonymous upload: status=%d want 401", ur3.Code)
	}
}

// shimImpl routes only storage methods to the real handler; every
// other StrictServerInterface method panics so a misrouted test
// surfaces loudly.
type shimImpl struct{ h *storage.Handler }

func (s shimImpl) UploadStorageObject(ctx context.Context, req openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	return s.h.UploadStorageObject(ctx, req)
}
func (s shimImpl) DownloadStorageObject(ctx context.Context, req openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	return s.h.DownloadStorageObject(ctx, req)
}
func (s shimImpl) DownloadStorageObjectVariant(ctx context.Context, req openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	return s.h.DownloadStorageObjectVariant(ctx, req)
}

func (shimImpl) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from storage test shim")
}
func (shimImpl) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from storage test shim")
}
func (shimImpl) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from storage test shim")
}
func (shimImpl) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from storage test shim")
}
func (shimImpl) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from storage test shim")
}
func (shimImpl) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from storage test shim")
}
func (shimImpl) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from storage test shim")
}
func (shimImpl) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from storage test shim")
}
func (shimImpl) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from storage test shim")
}
func (shimImpl) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from storage test shim")
}
func (shimImpl) ListAssetTypes(context.Context, openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	panic("ListAssetTypes called from storage test shim")
}
func (shimImpl) GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from storage test shim")
}
func (shimImpl) CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from storage test shim")
}
func (shimImpl) ListWorkflowStates(context.Context, openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from storage test shim")
}
func (shimImpl) CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	panic("CreateAsset called from storage test shim")
}
func (shimImpl) ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	panic("ListAssets called from storage test shim")
}
func (shimImpl) GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	panic("GetAsset called from storage test shim")
}
func (shimImpl) UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	panic("UpdateAsset called from storage test shim")
}
func (shimImpl) DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	panic("DeleteAsset called from storage test shim")
}
func (shimImpl) DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	panic("DownloadAssetFile called from storage test shim")
}
func (shimImpl) DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	panic("DownloadAssetVariant called from storage test shim")
}
func (shimImpl) AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	panic("AddAssetTags called from storage test shim")
}
func (shimImpl) RecreateAssetPreview(context.Context, openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	panic("RecreateAssetPreview called from storage test shim")
}
func (shimImpl) RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	panic("RemoveAssetTag called from storage test shim")
}
func (shimImpl) ListAssetCompanions(context.Context, openapi.ListAssetCompanionsRequestObject) (openapi.ListAssetCompanionsResponseObject, error) {
	panic("ListAssetCompanions called from storage test shim")
}
func (shimImpl) AddAssetCompanion(context.Context, openapi.AddAssetCompanionRequestObject) (openapi.AddAssetCompanionResponseObject, error) {
	panic("AddAssetCompanion called from storage test shim")
}
func (shimImpl) DownloadAssetCompanion(context.Context, openapi.DownloadAssetCompanionRequestObject) (openapi.DownloadAssetCompanionResponseObject, error) {
	panic("DownloadAssetCompanion called from storage test shim")
}
func (shimImpl) RemoveAssetCompanion(context.Context, openapi.RemoveAssetCompanionRequestObject) (openapi.RemoveAssetCompanionResponseObject, error) {
	panic("RemoveAssetCompanion called from storage test shim")
}
func (shimImpl) ListFields(context.Context, openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	panic("ListFields called from storage test shim")
}
func (shimImpl) CreateField(context.Context, openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	panic("CreateField called from storage test shim")
}
func (shimImpl) GetField(context.Context, openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	panic("GetField called from storage test shim")
}
func (shimImpl) UpdateField(context.Context, openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	panic("UpdateField called from storage test shim")
}
func (shimImpl) ArchiveField(context.Context, openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	panic("ArchiveField called from storage test shim")
}
func (shimImpl) GetAssetFields(context.Context, openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	panic("GetAssetFields called from storage test shim")
}
func (shimImpl) SetAssetFieldValue(context.Context, openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	panic("SetAssetFieldValue called from storage test shim")
}
func (shimImpl) ClearAssetFieldValue(context.Context, openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	panic("ClearAssetFieldValue called from storage test shim")
}
func (shimImpl) GetAssetFieldValueHistory(context.Context, openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	panic("GetAssetFieldValueHistory called from storage test shim")
}
func (shimImpl) ListCollections(context.Context, openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	panic("ListCollections called from storage test shim")
}
func (shimImpl) CreateCollection(context.Context, openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	panic("CreateCollection called from storage test shim")
}
func (shimImpl) GetCollection(context.Context, openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	panic("GetCollection called from storage test shim")
}
func (shimImpl) UpdateCollection(context.Context, openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	panic("UpdateCollection called from storage test shim")
}
func (shimImpl) DeleteCollection(context.Context, openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	panic("DeleteCollection called from storage test shim")
}
func (shimImpl) ListCollectionResources(context.Context, openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	panic("ListCollectionResources called from storage test shim")
}
func (shimImpl) AddCollectionResource(context.Context, openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	panic("AddCollectionResource called from storage test shim")
}
func (shimImpl) RemoveCollectionResource(context.Context, openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	panic("RemoveCollectionResource called from storage test shim")
}
func (shimImpl) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from storage test shim")
}
func (shimImpl) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from storage test shim")
}
func (shimImpl) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from storage test shim")
}
func (shimImpl) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from storage test shim")
}
func (shimImpl) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from storage test shim")
}
func (shimImpl) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from storage test shim")
}
func (shimImpl) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from storage test shim")
}
func (shimImpl) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from storage_test test shim")
}
func (shimImpl) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from storage_test test shim")
}
func (shimImpl) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from storage_test test shim")
}
func (shimImpl) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from storage_test test shim")
}
func (shimImpl) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from storage_test test shim")
}
func (shimImpl) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from storage_test test shim")
}
func (shimImpl) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from storage_test test shim")
}
func (shimImpl) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from storage_test test shim")
}
func (shimImpl) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from storage_test test shim")
}
func (shimImpl) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from storage_test test shim")
}
func (shimImpl) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from storage_test test shim")
}
func (shimImpl) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from storage_test test shim")
}
func (shimImpl) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from storage_test test shim")
}
func (shimImpl) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from storage_test test shim")
}
func (shimImpl) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from storage_test test shim")
}
func (shimImpl) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from storage_test test shim")
}
func (shimImpl) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from storage_test test shim")
}
func (shimImpl) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from storage_test test shim")
}
func (shimImpl) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from storage_test test shim")
}
func (shimImpl) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from storage_test test shim")
}
func (shimImpl) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from storage_test test shim")
}
func (shimImpl) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from storage_test test shim")
}
func (shimImpl) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from storage_test test shim")
}
func (shimImpl) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from storage_test test shim")
}
func (shimImpl) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from storage_test test shim")
}
func (shimImpl) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from storage_test test shim")
}
func (shimImpl) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from storage_test test shim")
}

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

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (shimImpl) GetSiteConfig(context.Context, openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	panic("GetSiteConfig called from storage test shim")
}
func (shimImpl) UpdateSiteConfig(context.Context, openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from storage test shim")
}
func (shimImpl) GetSMTPConfig(context.Context, openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from storage test shim")
}
func (shimImpl) UpdateSMTPConfig(context.Context, openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from storage test shim")
}
func (shimImpl) GetAuthConfig(context.Context, openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from storage test shim")
}
func (shimImpl) UpdateAuthConfig(context.Context, openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from storage test shim")
}
func (shimImpl) GetAIConfig(context.Context, openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from storage test shim")
}
func (shimImpl) UpdateAIConfig(context.Context, openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from storage test shim")
}
func (shimImpl) ListLocales(context.Context, openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from storage test shim")
}

func (shimImpl) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from storage test shim")
}
func (shimImpl) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from storage test shim")
}
func (shimImpl) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from storage test shim")
}

// --- jobs stubs (Phase 1.18.A) -------------------------------------------
func (shimImpl) ClaimJobs(context.Context, openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	panic("ClaimJobs called from test shim")
}
func (shimImpl) GetJob(context.Context, openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	panic("GetJob called from test shim")
}
func (shimImpl) HeartbeatJob(context.Context, openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	panic("HeartbeatJob called from test shim")
}
func (shimImpl) CompleteJob(context.Context, openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	panic("CompleteJob called from test shim")
}
func (shimImpl) FailJob(context.Context, openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	panic("FailJob called from test shim")
}
