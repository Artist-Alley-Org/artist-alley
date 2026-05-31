package setup_test

import (
	"context"
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

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/setup"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

const testScrambleKey = "setup-test-scramble-key"

// TestSetupFlow_HappyPath exercises the full create-initial-admin flow
// end-to-end: status reports needs_setup, complete creates the admin
// + role + site + smtp config in one transaction, subsequent status
// reports no-longer-needed, and repeated complete returns 409.
func TestSetupFlow_HappyPath(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		// 1. needs_setup = true on a clean install
		resp := fx.call(t, http.MethodGet, "/setup/status", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET status: %d body=%s", resp.StatusCode, readBody(resp))
		}
		var status openapi.SetupStatus
		mustDecode(t, resp, &status)
		if !status.NeedsSetup {
			t.Fatalf("expected needs_setup=true on clean fixture; got false")
		}
		if status.Deployment.DbName != fx.dbName {
			t.Errorf("deployment.db_name=%q want %q", status.Deployment.DbName, fx.dbName)
		}
		if status.Defaults.SiteName != "artist-alley" {
			t.Errorf("defaults.site_name=%q want artist-alley", status.Defaults.SiteName)
		}

		// 2. complete setup
		body := openapi.CompleteSetupJSONRequestBody{
			Admin: openapi.InitialAdminRequest{
				Username: fx.adminUsername,
				Password: "setup-test-PASSWORD-2026",
				Email:    "setup-test@example.com",
				Fullname: strPtr("Setup Test Admin"),
			},
			Site: openapi.SiteConfig{
				Name:    "Setup Test Site",
				BaseUrl: strPtr("https://setup-test.example.com"),
			},
			Smtp: &openapi.SMTPConfig{
				Host:        "smtp.example.com",
				Port:        587,
				Encryption:  openapi.SMTPConfigEncryptionStarttls,
				FromAddress: "noreply@setup-test.example.com",
				Username:    strPtr("smtp-user"),
				Password:    strPtr("smtp-pass"),
			},
		}
		resp = fx.call(t, http.MethodPost, "/setup/complete", body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST complete: %d body=%s", resp.StatusCode, readBody(resp))
		}
		var created openapi.CurrentUser
		mustDecode(t, resp, &created)
		if created.Username != fx.adminUsername {
			t.Errorf("created.username=%q want %q", created.Username, fx.adminUsername)
		}
		if created.AuthMethod != "session" {
			t.Errorf("created.auth_method=%q want session", created.AuthMethod)
		}

		// 3. needs_setup flipped to false
		resp = fx.call(t, http.MethodGet, "/setup/status", nil)
		mustDecode(t, resp, &status)
		if status.NeedsSetup {
			t.Errorf("expected needs_setup=false after complete; got true")
		}

		// 4. system_config rows landed
		site, err := fx.sysCfg.GetSite(ctx)
		if err != nil {
			t.Fatalf("GetSite: %v", err)
		}
		if site.Name != "Setup Test Site" {
			t.Errorf("site.name=%q want Setup Test Site", site.Name)
		}
		if site.BaseURL != "https://setup-test.example.com" {
			t.Errorf("site.base_url=%q want https://setup-test.example.com", site.BaseURL)
		}
		smtp, err := fx.sysCfg.GetSMTP(ctx)
		if err != nil {
			t.Fatalf("GetSMTP: %v", err)
		}
		if smtp.Host != "smtp.example.com" || smtp.Port != 587 {
			t.Errorf("smtp host/port mismatch: %+v", smtp)
		}
		if smtp.Encryption != sysconfig.SMTPEncryptionStartTLS {
			t.Errorf("smtp.encryption=%q want starttls", smtp.Encryption)
		}

		// 5. user is assigned the Admin role
		var roleName string
		if err := fx.pool.QueryRow(ctx,
			`SELECT r.name FROM user_roles ur JOIN roles r ON r.id = ur.role_id
			 JOIN "user" u ON u.ref = ur.rs_user_id WHERE u.username = $1`,
			fx.adminUsername).Scan(&roleName); err != nil {
			t.Fatalf("lookup role: %v", err)
		}
		if roleName != "Admin" {
			t.Errorf("user assigned role %q want Admin", roleName)
		}

		// 6. repeated complete is 409
		resp = fx.call(t, http.MethodPost, "/setup/complete", body)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("re-complete: %d want 409 body=%s", resp.StatusCode, readBody(resp))
		}
	})
}

// TestCompleteSetup_InputValidation rejects bad input with 400 before
// touching the DB.
func TestCompleteSetup_InputValidation(t *testing.T) {
	// Note: we don't test "bad email" via this struct path — the
	// generated `openapi_types.Email` field validates format at
	// JSON-marshal time, so a junk email never reaches the server.
	// The server-side mail.ParseAddress check is dead-code-at-API but
	// kept as defence-in-depth.
	cases := []struct {
		name       string
		body       openapi.CompleteSetupJSONRequestBody
		wantStatus int
		wantErrSub string
	}{
		{
			name: "short password",
			body: openapi.CompleteSetupJSONRequestBody{
				Admin: openapi.InitialAdminRequest{Username: "x", Password: "short7c", Email: "ok@example.com"},
				Site:  openapi.SiteConfig{Name: "S"},
			},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "password must be at least",
		},
		{
			name: "empty username",
			body: openapi.CompleteSetupJSONRequestBody{
				Admin: openapi.InitialAdminRequest{Username: "   ", Password: "longenough-2026", Email: "ok@example.com"},
				Site:  openapi.SiteConfig{Name: "S"},
			},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "username is required",
		},
		{
			name: "empty site name",
			body: openapi.CompleteSetupJSONRequestBody{
				Admin: openapi.InitialAdminRequest{Username: "x", Password: "longenough-2026", Email: "ok@example.com"},
				Site:  openapi.SiteConfig{Name: " "},
			},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "site name",
		},
		{
			name: "smtp host set but bad encryption",
			body: openapi.CompleteSetupJSONRequestBody{
				Admin: openapi.InitialAdminRequest{Username: "x", Password: "longenough-2026", Email: "ok@example.com"},
				Site:  openapi.SiteConfig{Name: "S"},
				Smtp: &openapi.SMTPConfig{
					Host: "smtp.example.com", Port: 587,
					Encryption: openapi.SMTPConfigEncryption("blarghtls"),
					FromAddress: "x@x.com",
				},
			},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "smtp encryption",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFixture(t, func(ctx context.Context, fx *fixture) {
				resp := fx.call(t, http.MethodPost, "/setup/complete", tc.body)
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("status=%d want %d body=%s", resp.StatusCode, tc.wantStatus, readBody(resp))
				}
				var errResp openapi.Error
				mustDecode(t, resp, &errResp)
				if !strings.Contains(errResp.Error, tc.wantErrSub) {
					t.Errorf("error %q does not contain %q", errResp.Error, tc.wantErrSub)
				}

				// DB unchanged: status still reports needs_setup=true.
				stResp := fx.call(t, http.MethodGet, "/setup/status", nil)
				var st openapi.SetupStatus
				mustDecode(t, stResp, &st)
				if !st.NeedsSetup {
					t.Errorf("rejection should not have created any admin")
				}
			})
		})
	}
}

// TestSetupStatus_ReportsEnvDefaults confirms AA_SETUP_DEFAULT_*
// values get surfaced to the form.
func TestSetupStatus_ReportsEnvDefaults(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		fx.cfg.SetupDefaults.AdminUsername = "preferred_admin"
		fx.cfg.SetupDefaults.SiteName = "Studio X"
		fx.cfg.SetupDefaults.SMTPPort = 2525
		// Rebuild the handler so it picks up the new cfg (the fixture
		// constructed it before we mutated cfg).
		fx.installHandler()

		resp := fx.call(t, http.MethodGet, "/setup/status", nil)
		var st openapi.SetupStatus
		mustDecode(t, resp, &st)
		if st.Defaults.AdminUsername != "preferred_admin" {
			t.Errorf("admin_username default not propagated: %q", st.Defaults.AdminUsername)
		}
		if st.Defaults.SiteName != "Studio X" {
			t.Errorf("site_name default not propagated: %q", st.Defaults.SiteName)
		}
		if st.Defaults.SmtpPort != 2525 {
			t.Errorf("smtp_port default not propagated: %d", st.Defaults.SmtpPort)
		}
	})
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type fixture struct {
	pool          *pgxpool.Pool
	router        chi.Router
	sysCfg        *sysconfig.Store
	cfg           config.Config
	adminUsername string
	dbName        string
}

func withFixture(t *testing.T, fn func(ctx context.Context, fx *fixture)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()

	fx := &fixture{
		pool:          pool,
		sysCfg:        sysconfig.NewStore(pool),
		dbName:        envOr("AA_DB_NAME", "artist_alley"),
		adminUsername: "setup_test_admin_" + uniqueSuffix(),
		cfg: config.Config{
			DBHost:      envOr("AA_DB_HOST", "postgres"),
			DBPort:      5432,
			DBName:      envOr("AA_DB_NAME", "artist_alley"),
			ScrambleKey: testScrambleKey,
			SetupDefaults: config.SetupDefaults{
				SiteName:       "artist-alley",
				SMTPPort:       587,
				SMTPEncryption: "starttls",
			},
		},
	}

	// Pre-clean — a previous killed run could have left this user.
	fx.cleanupAdmin(ctx)
	t.Cleanup(func() { fx.cleanupAdmin(context.Background()) })

	fx.installHandler()
	fn(ctx, fx)
}

func (f *fixture) installHandler() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := setup.NewHandler(f.pool, logger, f.cfg, f.sysCfg, "fs")
	router := chi.NewRouter()
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{h: h}, nil), router)
	f.router = router
}

// cleanupAdmin returns the DB to needs_setup=true: removes every
// user_role row that grants system.admin (so CountSystemAdmins drops
// to zero) plus this fixture's specific test user. This is by design
// destructive of any *interactively* created admin — the setup-flow
// test inherently requires a clean "no admin" state, and the
// integration test environment (docker-compose live DB) is shared
// with the interactive user. Re-run the setup wizard from the
// browser after the test suite if you need a real admin back.
func (f *fixture) cleanupAdmin(ctx context.Context) {
	// Drop the role assignment from every admin so needs_setup flips
	// back to true. We leave the user rows alone (the user could
	// re-grant later) except for our specific test user.
	_, _ = f.pool.Exec(ctx, `
		DELETE FROM user_roles
		WHERE role_id IN (
		    SELECT DISTINCT rc.role_id FROM role_capabilities rc
		    WHERE rc.capability_code = 'system.admin'
		)`)
	_, _ = f.pool.Exec(ctx, `DELETE FROM "user" WHERE username LIKE 'setup_test_admin_%'`)
	_, _ = f.pool.Exec(ctx, `DELETE FROM system_config WHERE key IN ('site','smtp')`)
}

func (f *fixture) call(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		bodyReader = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	return rr.Result()
}

// shimImpl satisfies the full StrictServerInterface but only the
// setup methods route to the real handler. Everything else panics
// so a misrouted call surfaces loudly.
type shimImpl struct{ h *setup.Handler }

func (s shimImpl) GetSetupStatus(ctx context.Context, req openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	return s.h.GetSetupStatus(ctx, req)
}
func (s shimImpl) CompleteSetup(ctx context.Context, req openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	return s.h.CompleteSetup(ctx, req)
}
func (shimImpl) ListWorkflowStates(context.Context, openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from setup test shim")
}

func (shimImpl) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from setup test shim")
}
func (shimImpl) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from setup test shim")
}
func (shimImpl) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from setup test shim")
}
func (shimImpl) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from setup test shim")
}
func (shimImpl) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from setup test shim")
}
func (shimImpl) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from setup test shim")
}
func (shimImpl) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from setup test shim")
}
func (shimImpl) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from setup test shim")
}
func (shimImpl) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from setup test shim")
}
func (shimImpl) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from setup test shim")
}
func (shimImpl) ListAssetTypes(context.Context, openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	panic("ListAssetTypes called from setup test shim")
}
func (shimImpl) UploadStorageObject(context.Context, openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	panic("UploadStorageObject called from setup test shim")
}
func (shimImpl) DownloadStorageObject(context.Context, openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	panic("DownloadStorageObject called from setup test shim")
}
func (shimImpl) DownloadStorageObjectVariant(context.Context, openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	panic("DownloadStorageObjectVariant called from setup test shim")
}
func (shimImpl) CreateAsset(context.Context, openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	panic("CreateAsset called from setup test shim")
}
func (shimImpl) ListAssets(context.Context, openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	panic("ListAssets called from setup test shim")
}
func (shimImpl) GetAsset(context.Context, openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	panic("GetAsset called from setup test shim")
}
func (shimImpl) UpdateAsset(context.Context, openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	panic("UpdateAsset called from setup test shim")
}
func (shimImpl) DeleteAsset(context.Context, openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	panic("DeleteAsset called from setup test shim")
}
func (shimImpl) DownloadAssetFile(context.Context, openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	panic("DownloadAssetFile called from setup test shim")
}
func (shimImpl) DownloadAssetVariant(context.Context, openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	panic("DownloadAssetVariant called from setup test shim")
}
func (shimImpl) AddAssetTags(context.Context, openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	panic("AddAssetTags called from setup test shim")
}
func (shimImpl) RecreateAssetPreview(context.Context, openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	panic("RecreateAssetPreview called from setup test shim")
}
func (shimImpl) RemoveAssetTag(context.Context, openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	panic("RemoveAssetTag called from setup test shim")
}
func (shimImpl) ListAssetCompanions(context.Context, openapi.ListAssetCompanionsRequestObject) (openapi.ListAssetCompanionsResponseObject, error) {
	panic("ListAssetCompanions called from setup test shim")
}
func (shimImpl) AddAssetCompanion(context.Context, openapi.AddAssetCompanionRequestObject) (openapi.AddAssetCompanionResponseObject, error) {
	panic("AddAssetCompanion called from setup test shim")
}
func (shimImpl) DownloadAssetCompanion(context.Context, openapi.DownloadAssetCompanionRequestObject) (openapi.DownloadAssetCompanionResponseObject, error) {
	panic("DownloadAssetCompanion called from setup test shim")
}
func (shimImpl) RemoveAssetCompanion(context.Context, openapi.RemoveAssetCompanionRequestObject) (openapi.RemoveAssetCompanionResponseObject, error) {
	panic("RemoveAssetCompanion called from setup test shim")
}
func (shimImpl) ListFields(context.Context, openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	panic("ListFields called from setup test shim")
}
func (shimImpl) CreateField(context.Context, openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	panic("CreateField called from setup test shim")
}
func (shimImpl) GetField(context.Context, openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	panic("GetField called from setup test shim")
}
func (shimImpl) UpdateField(context.Context, openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	panic("UpdateField called from setup test shim")
}
func (shimImpl) ArchiveField(context.Context, openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	panic("ArchiveField called from setup test shim")
}
func (shimImpl) GetAssetFields(context.Context, openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	panic("GetAssetFields called from setup test shim")
}
func (shimImpl) SetAssetFieldValue(context.Context, openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	panic("SetAssetFieldValue called from setup test shim")
}
func (shimImpl) ClearAssetFieldValue(context.Context, openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	panic("ClearAssetFieldValue called from setup test shim")
}
func (shimImpl) GetAssetFieldValueHistory(context.Context, openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	panic("GetAssetFieldValueHistory called from setup test shim")
}
func (shimImpl) ListCollections(context.Context, openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	panic("ListCollections called from setup test shim")
}
func (shimImpl) CreateCollection(context.Context, openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	panic("CreateCollection called from setup test shim")
}
func (shimImpl) GetCollection(context.Context, openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	panic("GetCollection called from setup test shim")
}
func (shimImpl) UpdateCollection(context.Context, openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	panic("UpdateCollection called from setup test shim")
}
func (shimImpl) DeleteCollection(context.Context, openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	panic("DeleteCollection called from setup test shim")
}
func (shimImpl) ListCollectionResources(context.Context, openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	panic("ListCollectionResources called from setup test shim")
}
func (shimImpl) AddCollectionResource(context.Context, openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	panic("AddCollectionResource called from setup test shim")
}
func (shimImpl) RemoveCollectionResource(context.Context, openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	panic("RemoveCollectionResource called from setup test shim")
}
func (shimImpl) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from setup test shim")
}
func (shimImpl) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from setup test shim")
}
func (shimImpl) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from setup test shim")
}
func (shimImpl) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from setup test shim")
}
func (shimImpl) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from setup test shim")
}
func (shimImpl) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from setup test shim")
}
func (shimImpl) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from setup test shim")
}
func (shimImpl) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from setup_test test shim")
}
func (shimImpl) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from setup_test test shim")
}
func (shimImpl) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from setup_test test shim")
}
func (shimImpl) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from setup_test test shim")
}
func (shimImpl) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from setup_test test shim")
}
func (shimImpl) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from setup_test test shim")
}
func (shimImpl) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from setup_test test shim")
}
func (shimImpl) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from setup_test test shim")
}
func (shimImpl) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from setup_test test shim")
}
func (shimImpl) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from setup_test test shim")
}
func (shimImpl) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from setup_test test shim")
}
func (shimImpl) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from setup_test test shim")
}
func (shimImpl) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from setup_test test shim")
}
func (shimImpl) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from setup_test test shim")
}
func (shimImpl) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from setup_test test shim")
}
func (shimImpl) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from setup_test test shim")
}
func (shimImpl) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from setup_test test shim")
}
func (shimImpl) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from setup_test test shim")
}
func (shimImpl) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from setup_test test shim")
}
func (shimImpl) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from setup_test test shim")
}
func (shimImpl) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from setup_test test shim")
}
func (shimImpl) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from setup_test test shim")
}
func (shimImpl) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from setup_test test shim")
}
func (shimImpl) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from setup_test test shim")
}
func (shimImpl) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from setup_test test shim")
}
func (shimImpl) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from setup_test test shim")
}
func (shimImpl) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from setup_test test shim")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(b)
}

func mustDecode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func strPtr(s string) *string { return &s }

// uniqueSuffix returns a short random-ish string for usernames so
// parallel test runs don't trip the user.username UNIQUE constraint.
func uniqueSuffix() string {
	return time.Now().Format("150405.000") // HH:MM:SS.mmm
}

func (shimImpl) GetSiteConfig(context.Context, openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	panic("GetSiteConfig called from setup test shim")
}
func (shimImpl) UpdateSiteConfig(context.Context, openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from setup test shim")
}
func (shimImpl) GetSMTPConfig(context.Context, openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from setup test shim")
}
func (shimImpl) UpdateSMTPConfig(context.Context, openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from setup test shim")
}
func (shimImpl) GetAuthConfig(context.Context, openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from setup test shim")
}
func (shimImpl) UpdateAuthConfig(context.Context, openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from setup test shim")
}
func (shimImpl) GetAIConfig(context.Context, openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from setup test shim")
}
func (shimImpl) UpdateAIConfig(context.Context, openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from setup test shim")
}
func (shimImpl) ListLocales(context.Context, openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from setup test shim")
}

func (shimImpl) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from setup test shim")
}
func (shimImpl) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from setup test shim")
}
func (shimImpl) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from setup test shim")
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

func (shimImpl) ListPostWhiteboards(context.Context, openapi.ListPostWhiteboardsRequestObject) (openapi.ListPostWhiteboardsResponseObject, error) {
	panic("ListPostWhiteboards called from setup_test test shim")
}

func (shimImpl) CreatePostWhiteboard(context.Context, openapi.CreatePostWhiteboardRequestObject) (openapi.CreatePostWhiteboardResponseObject, error) {
	panic("CreatePostWhiteboard called from setup_test test shim")
}
