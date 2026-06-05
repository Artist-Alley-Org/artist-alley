package assettype

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

// TestListAssetTypes_Live exercises the handler at the
// StrictServerInterface level: feed in the generated request type,
// inspect the generated response type, no HTTP marshalling.
func TestListAssetTypes_Live(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()
	ensureAssetTypeSeed(t, ctx, pool)

	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	resp, err := h.ListAssetTypes(ctx, openapi.ListAssetTypesRequestObject{})
	if err != nil {
		t.Fatalf("ListAssetTypes: %v", err)
	}

	ok, isOk := resp.(openapi.ListAssetTypes200JSONResponse)
	if !isOk {
		t.Fatalf("expected ListAssetTypes200JSONResponse, got %T", resp)
	}
	if len(ok) < 4 {
		t.Fatalf("expected at least 4 seeded rows, got %d", len(ok))
	}

	want := map[int64]string{1: "Image", 2: "Document", 3: "Video", 4: "Audio"}
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

// TestListAssetTypes_HTTP exercises the handler at the HTTP layer:
// mount it on a chi router exactly the way the real server does, fire
// a request, and verify the wire bytes match the OpenAPI contract.
func TestListAssetTypes_HTTP(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()
	ensureAssetTypeSeed(t, ctx, pool)

	// The StrictServerInterface now spans every endpoint group; we
	// pad the missing slices with the codegen-supplied Unimplemented
	// (which returns 501 for unsupported methods at the strict-server
	// layer). For the route under test that's a non-issue because
	// only ListAssetTypes is exercised.
	impl := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	strict := openapi.NewStrictHandler(rtOnly{h: impl}, nil)

	router := chi.NewRouter()
	openapi.HandlerFromMux(strict, router)

	req := httptest.NewRequest(http.MethodGet, "/asset_types", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q", ct)
	}

	var rows []openapi.AssetType
	if err := json.NewDecoder(rr.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) < 4 {
		t.Fatalf("expected at least 4 rows, got %d", len(rows))
	}
}

// rtOnly is a StrictServerInterface implementation that forwards
// ListAssetTypes to the real handler and panics for every other
// method, so a wrong-test-route bug surfaces immediately instead of
// silently returning a "not implemented" response.
type rtOnly struct{ h *Handler }

func (r rtOnly) ListAssetTypes(ctx context.Context, req openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	return r.h.ListAssetTypes(ctx, req)
}
func (rtOnly) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from assettype test shim")
}
func (rtOnly) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from assettype test shim")
}
func (rtOnly) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from assettype test shim")
}
func (rtOnly) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from assettype test shim")
}
func (rtOnly) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from assettype test shim")
}
func (rtOnly) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from assettype test shim")
}
func (rtOnly) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from assettype test shim")
}
func (rtOnly) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from assettype test shim")
}
func (rtOnly) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from assettype test shim")
}
func (rtOnly) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from assettype test shim")
}
func (rtOnly) UploadStorageObject(context.Context, openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	panic("UploadStorageObject called from assettype test shim")
}
func (rtOnly) DownloadStorageObject(context.Context, openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	panic("DownloadStorageObject called from assettype test shim")
}
func (rtOnly) DownloadStorageObjectVariant(context.Context, openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	panic("DownloadStorageObjectVariant called from assettype test shim")
}
func (rtOnly) CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	panic("CreateAsset called from assettype test shim")
}
func (rtOnly) ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	panic("ListAssets called from assettype test shim")
}
func (rtOnly) GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	panic("GetAsset called from assettype test shim")
}
func (rtOnly) UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	panic("UpdateAsset called from assettype test shim")
}
func (rtOnly) DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	panic("DeleteAsset called from assettype test shim")
}
func (rtOnly) DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	panic("DownloadAssetFile called from assettype test shim")
}
func (rtOnly) DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	panic("DownloadAssetVariant called from assettype test shim")
}
func (rtOnly) AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	panic("AddAssetTags called from assettype test shim")
}
func (rtOnly) RecreateAssetPreview(context.Context, openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	panic("RecreateAssetPreview called from assettype test shim")
}
func (rtOnly) RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	panic("RemoveAssetTag called from assettype test shim")
}
func (rtOnly) ListAssetCompanions(context.Context, openapi.ListAssetCompanionsRequestObject) (openapi.ListAssetCompanionsResponseObject, error) {
	panic("ListAssetCompanions called from assettype test shim")
}
func (rtOnly) AddAssetCompanion(context.Context, openapi.AddAssetCompanionRequestObject) (openapi.AddAssetCompanionResponseObject, error) {
	panic("AddAssetCompanion called from assettype test shim")
}
func (rtOnly) DownloadAssetCompanion(context.Context, openapi.DownloadAssetCompanionRequestObject) (openapi.DownloadAssetCompanionResponseObject, error) {
	panic("DownloadAssetCompanion called from assettype test shim")
}
func (rtOnly) RemoveAssetCompanion(context.Context, openapi.RemoveAssetCompanionRequestObject) (openapi.RemoveAssetCompanionResponseObject, error) {
	panic("RemoveAssetCompanion called from assettype test shim")
}
func (rtOnly) ListAssetAlternates(context.Context, openapi.ListAssetAlternatesRequestObject) (openapi.ListAssetAlternatesResponseObject, error) {
	panic("ListAssetAlternates called from assettype test shim")
}
func (rtOnly) AddAssetAlternate(context.Context, openapi.AddAssetAlternateRequestObject) (openapi.AddAssetAlternateResponseObject, error) {
	panic("AddAssetAlternate called from assettype test shim")
}
func (rtOnly) DownloadAssetAlternate(context.Context, openapi.DownloadAssetAlternateRequestObject) (openapi.DownloadAssetAlternateResponseObject, error) {
	panic("DownloadAssetAlternate called from assettype test shim")
}
func (rtOnly) RemoveAssetAlternate(context.Context, openapi.RemoveAssetAlternateRequestObject) (openapi.RemoveAssetAlternateResponseObject, error) {
	panic("RemoveAssetAlternate called from assettype test shim")
}
func (rtOnly) GetEpubSpine(context.Context, openapi.GetEpubSpineRequestObject) (openapi.GetEpubSpineResponseObject, error) {
	panic("GetEpubSpine called from assettype test shim")
}
func (rtOnly) GetEpubChapter(context.Context, openapi.GetEpubChapterRequestObject) (openapi.GetEpubChapterResponseObject, error) {
	panic("GetEpubChapter called from assettype test shim")
}
func (rtOnly) GetEpubResource(context.Context, openapi.GetEpubResourceRequestObject) (openapi.GetEpubResourceResponseObject, error) {
	panic("GetEpubResource called from assettype test shim")
}

func (rtOnly) SearchEpub(context.Context, openapi.SearchEpubRequestObject) (openapi.SearchEpubResponseObject, error) {
	panic("SearchEpub called from assettype test shim")
}
func (rtOnly) GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from assettype test shim")
}
func (rtOnly) CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from assettype test shim")
}
func (rtOnly) ListWorkflowStates(context.Context, openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from assettype test shim")
}
func (rtOnly) ListFields(context.Context, openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	panic("ListFields called from assettype test shim")
}
func (rtOnly) CreateField(context.Context, openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	panic("CreateField called from assettype test shim")
}
func (rtOnly) GetField(context.Context, openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	panic("GetField called from assettype test shim")
}
func (rtOnly) UpdateField(context.Context, openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	panic("UpdateField called from assettype test shim")
}
func (rtOnly) ArchiveField(context.Context, openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	panic("ArchiveField called from assettype test shim")
}
func (rtOnly) GetAssetFields(context.Context, openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	panic("GetAssetFields called from assettype test shim")
}
func (rtOnly) SetAssetFieldValue(context.Context, openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	panic("SetAssetFieldValue called from assettype test shim")
}
func (rtOnly) ClearAssetFieldValue(context.Context, openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	panic("ClearAssetFieldValue called from assettype test shim")
}
func (rtOnly) GetAssetFieldValueHistory(context.Context, openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	panic("GetAssetFieldValueHistory called from assettype test shim")
}
func (rtOnly) ListCollections(context.Context, openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	panic("ListCollections called from assettype test shim")
}
func (rtOnly) CreateCollection(context.Context, openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	panic("CreateCollection called from assettype test shim")
}
func (rtOnly) GetCollection(context.Context, openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	panic("GetCollection called from assettype test shim")
}
func (rtOnly) UpdateCollection(context.Context, openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	panic("UpdateCollection called from assettype test shim")
}
func (rtOnly) DeleteCollection(context.Context, openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	panic("DeleteCollection called from assettype test shim")
}
func (rtOnly) ListCollectionResources(context.Context, openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	panic("ListCollectionResources called from assettype test shim")
}
func (rtOnly) AddCollectionResource(context.Context, openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	panic("AddCollectionResource called from assettype test shim")
}
func (rtOnly) RemoveCollectionResource(context.Context, openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	panic("RemoveCollectionResource called from assettype test shim")
}
func (rtOnly) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from assettype test shim")
}
func (rtOnly) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from assettype test shim")
}
func (rtOnly) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from assettype test shim")
}
func (rtOnly) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from assettype test shim")
}
func (rtOnly) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from assettype test shim")
}
func (rtOnly) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from assettype test shim")
}
func (rtOnly) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from assettype test shim")
}
func (rtOnly) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from assettype test shim")
}
func (rtOnly) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from assettype test shim")
}
func (rtOnly) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from assettype test shim")
}
func (rtOnly) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from assettype test shim")
}
func (rtOnly) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from assettype test shim")
}
func (rtOnly) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from assettype test shim")
}
func (rtOnly) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from assettype test shim")
}
func (rtOnly) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from assettype test shim")
}
func (rtOnly) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from assettype test shim")
}
func (rtOnly) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from assettype test shim")
}
func (rtOnly) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from assettype test shim")
}
func (rtOnly) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from assettype test shim")
}
func (rtOnly) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from assettype test shim")
}
func (rtOnly) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from assettype test shim")
}
func (rtOnly) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from assettype test shim")
}
func (rtOnly) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from assettype test shim")
}
func (rtOnly) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from assettype test shim")
}
func (rtOnly) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from assettype test shim")
}
func (rtOnly) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from assettype test shim")
}
func (rtOnly) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from assettype test shim")
}
func (rtOnly) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from assettype test shim")
}
func (rtOnly) ListAdminUsers(context.Context, openapi.ListAdminUsersRequestObject) (openapi.ListAdminUsersResponseObject, error) {
	panic("ListAdminUsers called from assettype rtOnly test shim")
}
func (rtOnly) SetAdminUserStatus(context.Context, openapi.SetAdminUserStatusRequestObject) (openapi.SetAdminUserStatusResponseObject, error) {
	panic("SetAdminUserStatus called from assettype rtOnly test shim")
}
func (rtOnly) ListMySessions(context.Context, openapi.ListMySessionsRequestObject) (openapi.ListMySessionsResponseObject, error) {
	panic("ListMySessions called from assettype rtOnly test shim")
}
func (rtOnly) RevokeMySession(context.Context, openapi.RevokeMySessionRequestObject) (openapi.RevokeMySessionResponseObject, error) {
	panic("RevokeMySession called from assettype rtOnly test shim")
}
func (rtOnly) ListAdminUserSessions(context.Context, openapi.ListAdminUserSessionsRequestObject) (openapi.ListAdminUserSessionsResponseObject, error) {
	panic("ListAdminUserSessions called from assettype rtOnly test shim")
}
func (rtOnly) RevokeAdminUserSession(context.Context, openapi.RevokeAdminUserSessionRequestObject) (openapi.RevokeAdminUserSessionResponseObject, error) {
	panic("RevokeAdminUserSession called from assettype rtOnly test shim")
}
func (rtOnly) ChangeMyPassword(context.Context, openapi.ChangeMyPasswordRequestObject) (openapi.ChangeMyPasswordResponseObject, error) {
	panic("ChangeMyPassword called from assettype rtOnly test shim")
}
func (rtOnly) AdminResetUserPassword(context.Context, openapi.AdminResetUserPasswordRequestObject) (openapi.AdminResetUserPasswordResponseObject, error) {
	panic("AdminResetUserPassword called from assettype rtOnly test shim")
}
func (rtOnly) ListAdminUserCapabilities(context.Context, openapi.ListAdminUserCapabilitiesRequestObject) (openapi.ListAdminUserCapabilitiesResponseObject, error) {
	panic("ListAdminUserCapabilities called from assettype rtOnly test shim")
}
func (rtOnly) AddAdminUserGrant(context.Context, openapi.AddAdminUserGrantRequestObject) (openapi.AddAdminUserGrantResponseObject, error) {
	panic("AddAdminUserGrant called from assettype rtOnly test shim")
}
func (rtOnly) RemoveAdminUserGrant(context.Context, openapi.RemoveAdminUserGrantRequestObject) (openapi.RemoveAdminUserGrantResponseObject, error) {
	panic("RemoveAdminUserGrant called from assettype rtOnly test shim")
}
func (rtOnly) AddAdminUserRevoke(context.Context, openapi.AddAdminUserRevokeRequestObject) (openapi.AddAdminUserRevokeResponseObject, error) {
	panic("AddAdminUserRevoke called from assettype rtOnly test shim")
}
func (rtOnly) RemoveAdminUserRevoke(context.Context, openapi.RemoveAdminUserRevokeRequestObject) (openapi.RemoveAdminUserRevokeResponseObject, error) {
	panic("RemoveAdminUserRevoke called from assettype rtOnly test shim")
}
func (r rtOnly) ListAssetTypeAcls(ctx context.Context, req openapi.ListAssetTypeAclsRequestObject) (openapi.ListAssetTypeAclsResponseObject, error) {
	return r.h.ListAssetTypeAcls(ctx, req)
}
func (r rtOnly) AddAssetTypeAcl(ctx context.Context, req openapi.AddAssetTypeAclRequestObject) (openapi.AddAssetTypeAclResponseObject, error) {
	return r.h.AddAssetTypeAcl(ctx, req)
}
func (r rtOnly) RemoveAssetTypeAcl(ctx context.Context, req openapi.RemoveAssetTypeAclRequestObject) (openapi.RemoveAssetTypeAclResponseObject, error) {
	return r.h.RemoveAssetTypeAcl(ctx, req)
}
func (rtOnly) ListAdminAuditEvents(context.Context, openapi.ListAdminAuditEventsRequestObject) (openapi.ListAdminAuditEventsResponseObject, error) {
	panic("ListAdminAuditEvents called from assettype rtOnly test shim")
}
func (rtOnly) ListAdminAuditEventTypes(context.Context, openapi.ListAdminAuditEventTypesRequestObject) (openapi.ListAdminAuditEventTypesResponseObject, error) {
	panic("ListAdminAuditEventTypes called from assettype rtOnly test shim")
}
func (rtOnly) GetAdminLicenseStatus(context.Context, openapi.GetAdminLicenseStatusRequestObject) (openapi.GetAdminLicenseStatusResponseObject, error) {
	panic("GetAdminLicenseStatus called from assettype rtOnly test shim")
}
func (rtOnly) ValidateAdminLicense(context.Context, openapi.ValidateAdminLicenseRequestObject) (openapi.ValidateAdminLicenseResponseObject, error) {
	panic("ValidateAdminLicense called from assettype rtOnly test shim")
}
func (rtOnly) UploadAdminLicense(context.Context, openapi.UploadAdminLicenseRequestObject) (openapi.UploadAdminLicenseResponseObject, error) {
	panic("UploadAdminLicense called from assettype rtOnly test shim")
}
func (rtOnly) ListIdentityProviders(context.Context, openapi.ListIdentityProvidersRequestObject) (openapi.ListIdentityProvidersResponseObject, error) {
	panic("ListIdentityProviders called from assettype rtOnly test shim")
}
func (rtOnly) GetAccountPreferences(context.Context, openapi.GetAccountPreferencesRequestObject) (openapi.GetAccountPreferencesResponseObject, error) {
	panic("GetAccountPreferences called from assettype rtOnly test shim")
}
func (rtOnly) PatchAccountPreferences(context.Context, openapi.PatchAccountPreferencesRequestObject) (openapi.PatchAccountPreferencesResponseObject, error) {
	panic("PatchAccountPreferences called from assettype rtOnly test shim")
}
func (rtOnly) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from assettype test shim")
}
func (rtOnly) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from assettype test shim")
}
func (rtOnly) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from assettype test shim")
}
func (rtOnly) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from assettype test shim")
}
func (rtOnly) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from assettype test shim")
}
func (rtOnly) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from assettype test shim")
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

// ensureAssetTypeSeed makes sure the four canonical RS asset
// types exist. Defensive — the table is normally created and seeded by
// CheckDBStruct on the PHP side during install.
func ensureAssetTypeSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	const ensure = `
INSERT INTO asset_types (ref, name, icon)
VALUES (1, 'Image', 'image'),
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
	panic("GetSiteConfig called from assettype test shim")
}
func (rtOnly) UpdateSiteConfig(context.Context, openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from assettype test shim")
}
func (rtOnly) GetSMTPConfig(context.Context, openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from assettype test shim")
}
func (rtOnly) UpdateSMTPConfig(context.Context, openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from assettype test shim")
}
func (rtOnly) GetAuthConfig(context.Context, openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from assettype test shim")
}
func (rtOnly) UpdateAuthConfig(context.Context, openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from assettype test shim")
}
func (rtOnly) GetAIConfig(context.Context, openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from assettype test shim")
}
func (rtOnly) UpdateAIConfig(context.Context, openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from assettype test shim")
}
func (rtOnly) ListLocales(context.Context, openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from assettype test shim")
}

func (rtOnly) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from assettype test shim")
}
func (rtOnly) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from assettype test shim")
}
func (rtOnly) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from assettype test shim")
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

func (rtOnly) ListPostWhiteboards(context.Context, openapi.ListPostWhiteboardsRequestObject) (openapi.ListPostWhiteboardsResponseObject, error) {
	panic("ListPostWhiteboards called from assettype test shim")
}

func (rtOnly) CreatePostWhiteboard(context.Context, openapi.CreatePostWhiteboardRequestObject) (openapi.CreatePostWhiteboardResponseObject, error) {
	panic("CreatePostWhiteboard called from assettype test shim")
}

// --- brush packs stubs (Phase 1.21c) -------------------------------------
func (rtOnly) ListBrushPacks(context.Context, openapi.ListBrushPacksRequestObject) (openapi.ListBrushPacksResponseObject, error) {
	panic("ListBrushPacks called from rtOnly test shim")
}
func (rtOnly) ImportBrushPack(context.Context, openapi.ImportBrushPackRequestObject) (openapi.ImportBrushPackResponseObject, error) {
	panic("ImportBrushPack called from rtOnly test shim")
}
func (rtOnly) GetBrushPack(context.Context, openapi.GetBrushPackRequestObject) (openapi.GetBrushPackResponseObject, error) {
	panic("GetBrushPack called from rtOnly test shim")
}
func (rtOnly) DeleteBrushPack(context.Context, openapi.DeleteBrushPackRequestObject) (openapi.DeleteBrushPackResponseObject, error) {
	panic("DeleteBrushPack called from rtOnly test shim")
}
func (rtOnly) GetBrushPackStamp(context.Context, openapi.GetBrushPackStampRequestObject) (openapi.GetBrushPackStampResponseObject, error) {
	panic("GetBrushPackStamp called from rtOnly test shim")
}
func (rtOnly)ListAssetTextAnnotations(context.Context, openapi.ListAssetTextAnnotationsRequestObject) (openapi.ListAssetTextAnnotationsResponseObject, error) {
	panic("ListAssetTextAnnotations called from assettype rtOnly test shim")
}
func (rtOnly)CreateAssetTextAnnotation(context.Context, openapi.CreateAssetTextAnnotationRequestObject) (openapi.CreateAssetTextAnnotationResponseObject, error) {
	panic("CreateAssetTextAnnotation called from assettype rtOnly test shim")
}
func (rtOnly)UpdateTextAnnotation(context.Context, openapi.UpdateTextAnnotationRequestObject) (openapi.UpdateTextAnnotationResponseObject, error) {
	panic("UpdateTextAnnotation called from assettype rtOnly test shim")
}
func (rtOnly)LintAsset(context.Context, openapi.LintAssetRequestObject) (openapi.LintAssetResponseObject, error) {
	panic("LintAsset called from assettype rtOnly test shim")
}
func (rtOnly) FollowUser(context.Context, openapi.FollowUserRequestObject) (openapi.FollowUserResponseObject, error) {
	panic("FollowUser called from assettype test shim")
}
func (rtOnly) UnfollowUser(context.Context, openapi.UnfollowUserRequestObject) (openapi.UnfollowUserResponseObject, error) {
	panic("UnfollowUser called from assettype test shim")
}
func (rtOnly) ListUserFollowers(context.Context, openapi.ListUserFollowersRequestObject) (openapi.ListUserFollowersResponseObject, error) {
	panic("ListUserFollowers called from assettype test shim")
}
func (rtOnly) ListUserFollowing(context.Context, openapi.ListUserFollowingRequestObject) (openapi.ListUserFollowingResponseObject, error) {
	panic("ListUserFollowing called from assettype test shim")
}
func (rtOnly) GetUserRelationship(context.Context, openapi.GetUserRelationshipRequestObject) (openapi.GetUserRelationshipResponseObject, error) {
	panic("GetUserRelationship called from assettype test shim")
}
func (rtOnly) BlockUser(context.Context, openapi.BlockUserRequestObject) (openapi.BlockUserResponseObject, error) {
	panic("BlockUser called from assettype test shim")
}
func (rtOnly) UnblockUser(context.Context, openapi.UnblockUserRequestObject) (openapi.UnblockUserResponseObject, error) {
	panic("UnblockUser called from assettype test shim")
}
func (rtOnly) ListMyBlocked(context.Context, openapi.ListMyBlockedRequestObject) (openapi.ListMyBlockedResponseObject, error) {
	panic("ListMyBlocked called from assettype test shim")
}
func (rtOnly) ListMyNotifications(context.Context, openapi.ListMyNotificationsRequestObject) (openapi.ListMyNotificationsResponseObject, error) {
	panic("ListMyNotifications called from assettype test shim")
}
func (rtOnly) GetMyUnreadNotificationCount(context.Context, openapi.GetMyUnreadNotificationCountRequestObject) (openapi.GetMyUnreadNotificationCountResponseObject, error) {
	panic("GetMyUnreadNotificationCount called from assettype test shim")
}
func (rtOnly) MarkNotificationRead(context.Context, openapi.MarkNotificationReadRequestObject) (openapi.MarkNotificationReadResponseObject, error) {
	panic("MarkNotificationRead called from assettype test shim")
}
func (rtOnly) MarkAllMyNotificationsRead(context.Context, openapi.MarkAllMyNotificationsReadRequestObject) (openapi.MarkAllMyNotificationsReadResponseObject, error) {
	panic("MarkAllMyNotificationsRead called from assettype test shim")
}
func (rtOnly) ListMyDirectMessageThreads(context.Context, openapi.ListMyDirectMessageThreadsRequestObject) (openapi.ListMyDirectMessageThreadsResponseObject, error) {
	panic("ListMyDirectMessageThreads called from assettype test shim")
}
func (rtOnly) GetMyUnreadDirectMessageCount(context.Context, openapi.GetMyUnreadDirectMessageCountRequestObject) (openapi.GetMyUnreadDirectMessageCountResponseObject, error) {
	panic("GetMyUnreadDirectMessageCount called from assettype test shim")
}
func (rtOnly) ListDirectMessageThread(context.Context, openapi.ListDirectMessageThreadRequestObject) (openapi.ListDirectMessageThreadResponseObject, error) {
	panic("ListDirectMessageThread called from assettype test shim")
}
func (rtOnly) SendDirectMessage(context.Context, openapi.SendDirectMessageRequestObject) (openapi.SendDirectMessageResponseObject, error) {
	panic("SendDirectMessage called from assettype test shim")
}
func (rtOnly) MarkDirectMessageThreadRead(context.Context, openapi.MarkDirectMessageThreadReadRequestObject) (openapi.MarkDirectMessageThreadReadResponseObject, error) {
	panic("MarkDirectMessageThreadRead called from assettype test shim")
}
func (rtOnly) ListAdminActivities(context.Context, openapi.ListAdminActivitiesRequestObject) (openapi.ListAdminActivitiesResponseObject, error) {
	panic("ListAdminActivities called from assettype test shim")
}
