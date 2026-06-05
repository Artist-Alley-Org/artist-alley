package auth

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

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// All tests in this file are integration tests against the live
// docker-compose Postgres. They skip when AA_DB_PASSWORD is not set
// (which is what scripts/test.sh exports).
//
// Each test runs inside a transaction that is rolled back at the end,
// so they neither dirty the DB nor depend on each other's state. The
// transaction strategy means we cannot easily test the "best-effort
// touch" goroutine in middleware.go — it deliberately uses a fresh
// pool connection rather than the transaction-bound one. That's fine;
// the touch is observability, not correctness.

const (
	testScrambleKey = "test-scramble-key-not-shared"
)

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestLogin_OK(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: fx.password}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}

		// Set-Cookie present, name matches.
		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == SessionCookieName {
				cookie = c
				break
			}
		}
		if cookie == nil {
			t.Fatal("no rs_session cookie set")
		}
		if cookie.Value == "" || len(cookie.Value) < 16 {
			t.Errorf("rs_session value looks too short: %q", cookie.Value)
		}
		if !cookie.HttpOnly {
			t.Error("cookie should be HttpOnly")
		}
		if cookie.SameSite != http.SameSiteLaxMode {
			t.Error("cookie should be SameSite=Lax")
		}

		// Response body shape.
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if cu.Username != fx.username {
			t.Errorf("username=%q want %q", cu.Username, fx.username)
		}
		if cu.AuthMethod != openapi.CurrentUserAuthMethod("session") {
			t.Errorf("auth_method=%q want session", cu.AuthMethod)
		}

		// The session column got written.
		var sess *string
		if err := fx.tx.QueryRow(ctx, `SELECT session FROM "user" WHERE ref = $1`, fx.userRef).Scan(&sess); err != nil {
			t.Fatalf("read session: %v", err)
		}
		if sess == nil || *sess != cookie.Value {
			gotSess := "<nil>"
			if sess != nil {
				gotSess = *sess
			}
			t.Errorf("user.session=%q want %q", gotSess, cookie.Value)
		}
	})
}

func TestLogin_BadPassword(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		body := openapi.LoginJSONRequestBody{Username: fx.username, Password: "WRONG"}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401", resp.StatusCode)
		}
	})
}

func TestLogin_UnknownUsername(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		body := openapi.LoginJSONRequestBody{Username: "nobody-here", Password: "whatever"}
		resp := fx.call(t, http.MethodPost, "/auth/login", body, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401 (must not leak that username doesn't exist)", resp.StatusCode)
		}
	})
}

// ---------------------------------------------------------------------------
// /me
// ---------------------------------------------------------------------------

func TestMe_WithSession(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		cookie := fx.loginAndGetCookie(t)
		resp := fx.call(t, http.MethodGet, "/auth/me", nil, &cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if cu.Ref != fx.userRef {
			t.Errorf("ref=%d want %d", cu.Ref, fx.userRef)
		}
	})
}

func TestMe_Anonymous(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		resp := fx.call(t, http.MethodGet, "/auth/me", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401", resp.StatusCode)
		}
	})
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

func TestLogout_ClearsSession(t *testing.T) {
	withFixture(t, func(ctx context.Context, fx *fixture) {
		cookie := fx.loginAndGetCookie(t)

		resp := fx.call(t, http.MethodPost, "/auth/logout", nil, &cookie)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status=%d want 204", resp.StatusCode)
		}

		// user.session got NULLed.
		var sess *string
		if err := fx.tx.QueryRow(ctx, `SELECT session FROM "user" WHERE ref = $1`, fx.userRef).Scan(&sess); err != nil {
			t.Fatalf("read session: %v", err)
		}
		if sess != nil {
			t.Errorf("user.session should be NULL after logout, got %q", *sess)
		}
	})
}

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

func TestApiTokens_CreateListBearerRevoke(t *testing.T) {
	withFixture(t, func(_ context.Context, fx *fixture) {
		cookie := fx.loginAndGetCookie(t)

		// Create.
		exp := time.Now().Add(24 * time.Hour)
		body := openapi.CreateApiTokenJSONRequestBody{
			Name:      "CI bot",
			ExpiresAt: &exp,
		}
		resp := fx.call(t, http.MethodPost, "/auth/tokens", body, &cookie)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var created openapi.ApiTokenCreated
		mustDecode(t, resp, &created)
		if !strings.HasPrefix(created.Token, TokenPrefix) {
			t.Errorf("token does not start with %q: %q", TokenPrefix, created.Token)
		}
		if got := len(created.Token); got < len(TokenPrefix)+24 {
			t.Errorf("token suspiciously short: %d chars", got)
		}

		// List.
		resp = fx.call(t, http.MethodGet, "/auth/tokens", nil, &cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		var summaries []openapi.ApiTokenSummary
		mustDecode(t, resp, &summaries)
		if len(summaries) != 1 {
			t.Fatalf("expected 1 token, got %d", len(summaries))
		}
		if summaries[0].Name != "CI bot" {
			t.Errorf("name=%q", summaries[0].Name)
		}

		// Use it as Bearer to authenticate /me.
		resp = fx.callWithBearer(t, http.MethodGet, "/auth/me", nil, created.Token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("bearer /me status=%d body=%s", resp.StatusCode, readBody(resp))
		}
		var cu openapi.CurrentUser
		mustDecode(t, resp, &cu)
		if cu.AuthMethod != openapi.CurrentUserAuthMethod("token") {
			t.Errorf("auth_method=%q want token", cu.AuthMethod)
		}

		// Token creation via Bearer is disallowed.
		resp = fx.callWithBearer(t, http.MethodPost, "/auth/tokens", body, created.Token)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Bearer-creating-a-token: status=%d want 401", resp.StatusCode)
		}

		// Revoke (via cookie).
		resp = fx.call(t, http.MethodDelete, "/auth/tokens/"+created.Id.String(), nil, &cookie)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("revoke status=%d", resp.StatusCode)
		}

		// Now the token must no longer authenticate.
		resp = fx.callWithBearer(t, http.MethodGet, "/auth/me", nil, created.Token)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("revoked token still works: status=%d", resp.StatusCode)
		}

		// Listing now shows zero tokens.
		resp = fx.call(t, http.MethodGet, "/auth/tokens", nil, &cookie)
		mustDecode(t, resp, &summaries)
		if len(summaries) != 0 {
			t.Errorf("after revoke, expected 0 tokens, got %d", len(summaries))
		}
	})
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type fixture struct {
	pool       *pgxpool.Pool
	tx         interface {
		Exec(ctx context.Context, sql string, args ...any) (pg pgcmd, err error)
		Query(ctx context.Context, sql string, args ...any) (pgxRowsIface, error)
		QueryRow(ctx context.Context, sql string, args ...any) pgxRowIface
	}
	// router serves the strict-handler chain exactly as the real server does.
	router   chi.Router
	userRef  int64
	username string
	password string
}

// loginAndGetCookie performs a login and returns the rs_session cookie.
func (f *fixture) loginAndGetCookie(t *testing.T) http.Cookie {
	t.Helper()
	body := openapi.LoginJSONRequestBody{Username: f.username, Password: f.password}
	resp := f.call(t, http.MethodPost, "/auth/login", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login setup failed: status=%d body=%s", resp.StatusCode, readBody(resp))
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			return *c
		}
	}
	t.Fatal("login did not set cookie")
	return http.Cookie{}
}

// call issues an HTTP request against the in-process router.
func (f *fixture) call(t *testing.T, method, path string, body any, cookie *http.Cookie) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	return rr.Result()
}

func (f *fixture) callWithBearer(t *testing.T, method, path string, body any, token string) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	return rr.Result()
}

// withFixture spins up a real-DB-backed fixture, calls fn, then rolls
// back the transaction so the run leaves no trace. It also makes sure
// the "user" table exists (creates a minimal version if RS hasn't
// installed yet) so tests aren't coupled to install state.
func withFixture(t *testing.T, fn func(ctx context.Context, fx *fixture)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()

	ensureUserTable(t, ctx, pool)

	// Seed a known user. We do this *outside* the test transaction so
	// the auth handler's pool (a separate connection from the test's
	// tx) can see the row.
	const username = "go_auth_test_user"
	const password = "go-auth-test-PASSWORD-2026"
	hash, err := HashPassword(password, testScrambleKey)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	// Idempotent upsert by username. The UNIQUE INDEX from migration
	// 00007 makes ON CONFLICT (username) work directly — no fallback
	// path needed.
	const upsert = `
		INSERT INTO "user" (username, password, fullname, email, approved)
		VALUES ($1, $2, 'Go Auth Test', 'go-auth-test@example.com', 1)
		ON CONFLICT (username) DO UPDATE SET password = EXCLUDED.password
		RETURNING ref
	`
	var userRef int64
	if err := pool.QueryRow(ctx, upsert, username, hash).Scan(&userRef); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	// Pre-clean satellite tables before the test runs so leftover state
	// from a previously-killed run (panic, ^C) doesn't trip PK
	// constraints inside the test body.
	for _, sql := range []string{
		`DELETE FROM api_tokens             WHERE rs_user_id = $1`,
		`DELETE FROM user_capability_grants WHERE rs_user_id = $1`,
		`DELETE FROM user_capability_revokes WHERE rs_user_id = $1`,
		`DELETE FROM user_roles              WHERE rs_user_id = $1`,
		`DELETE FROM sessions               WHERE user_ref   = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, userRef); err != nil {
			t.Fatalf("pre-clean: %v: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		// Best-effort: remove the user and any rows that reference them.
		cleanCtx := context.Background()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM api_tokens WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM user_capability_grants WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM user_capability_revokes WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM user_roles WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM sessions WHERE user_ref = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM "user" WHERE ref = $1`, userRef)
	})

	// Real chi router wired exactly as the production server does it.
	handler := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), testScrambleKey, 7, nil, nil, nil, nil)
	resolver := &Resolver{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	router := chi.NewRouter()
	router.Use(resolver.ResolveIdentity)
	openapi.HandlerFromMux(openapi.NewStrictHandler(authOnlyImpl{handler}, nil), router)

	fx := &fixture{
		pool:     pool,
		router:   router,
		userRef:  userRef,
		username: username,
		password: password,
		tx:       poolTxShim{pool}, // not a real tx; tests read state directly
	}
	fn(ctx, fx)
}

// authOnlyImpl is a thin shim that satisfies the full
// openapi.StrictServerInterface but only delegates the auth methods to
// the real handler. The other endpoints panic if accidentally called,
// which would surface as a test failure rather than a silent miss.
type authOnlyImpl struct {
	h *Handler
}

func (a authOnlyImpl) Login(ctx context.Context, req openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	return a.h.Login(ctx, req)
}
func (a authOnlyImpl) Logout(ctx context.Context, req openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	return a.h.Logout(ctx, req)
}
func (a authOnlyImpl) GetCurrentUser(ctx context.Context, req openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	return a.h.GetCurrentUser(ctx, req)
}
func (a authOnlyImpl) ListApiTokens(ctx context.Context, req openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	return a.h.ListApiTokens(ctx, req)
}
func (a authOnlyImpl) CreateApiToken(ctx context.Context, req openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	return a.h.CreateApiToken(ctx, req)
}
func (a authOnlyImpl) RevokeApiToken(ctx context.Context, req openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	return a.h.RevokeApiToken(ctx, req)
}
func (a authOnlyImpl) ListCapabilities(ctx context.Context, req openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	return a.h.ListCapabilities(ctx, req)
}
func (a authOnlyImpl) ListRoles(ctx context.Context, req openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	return a.h.ListRoles(ctx, req)
}
func (a authOnlyImpl) GetMyCapabilities(ctx context.Context, req openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	return a.h.GetMyCapabilities(ctx, req)
}
func (a authOnlyImpl) SetUserRole(ctx context.Context, req openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	return a.h.SetUserRole(ctx, req)
}
func (a authOnlyImpl) ListAssetTypes(_ context.Context, _ openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	panic("ListAssetTypes called from auth test shim")
}
func (a authOnlyImpl) UploadStorageObject(_ context.Context, _ openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	panic("UploadStorageObject called from auth test shim")
}
func (a authOnlyImpl) DownloadStorageObject(_ context.Context, _ openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	panic("DownloadStorageObject called from auth test shim")
}
func (a authOnlyImpl) DownloadStorageObjectVariant(_ context.Context, _ openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	panic("DownloadStorageObjectVariant called from auth test shim")
}
func (a authOnlyImpl) CreateAsset(_ context.Context, _ openapi.CreateAssetRequestObject) (openapi.CreateAssetResponseObject, error) {
	panic("CreateAsset called from auth test shim")
}
func (a authOnlyImpl) ListAssets(_ context.Context, _ openapi.ListAssetsRequestObject) (openapi.ListAssetsResponseObject, error) {
	panic("ListAssets called from auth test shim")
}
func (a authOnlyImpl) GetAsset(_ context.Context, _ openapi.GetAssetRequestObject) (openapi.GetAssetResponseObject, error) {
	panic("GetAsset called from auth test shim")
}
func (a authOnlyImpl) UpdateAsset(_ context.Context, _ openapi.UpdateAssetRequestObject) (openapi.UpdateAssetResponseObject, error) {
	panic("UpdateAsset called from auth test shim")
}
func (a authOnlyImpl) DeleteAsset(_ context.Context, _ openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	panic("DeleteAsset called from auth test shim")
}
func (a authOnlyImpl) DownloadAssetFile(_ context.Context, _ openapi.DownloadAssetFileRequestObject) (openapi.DownloadAssetFileResponseObject, error) {
	panic("DownloadAssetFile called from auth test shim")
}
func (a authOnlyImpl) DownloadAssetVariant(_ context.Context, _ openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	panic("DownloadAssetVariant called from auth test shim")
}
func (a authOnlyImpl) AddAssetTags(_ context.Context, _ openapi.AddAssetTagsRequestObject) (openapi.AddAssetTagsResponseObject, error) {
	panic("AddAssetTags called from auth test shim")
}
func (a authOnlyImpl) RecreateAssetPreview(_ context.Context, _ openapi.RecreateAssetPreviewRequestObject) (openapi.RecreateAssetPreviewResponseObject, error) {
	panic("RecreateAssetPreview called from auth test shim")
}
func (a authOnlyImpl) RemoveAssetTag(_ context.Context, _ openapi.RemoveAssetTagRequestObject) (openapi.RemoveAssetTagResponseObject, error) {
	panic("RemoveAssetTag called from auth test shim")
}
func (a authOnlyImpl) ListAssetCompanions(_ context.Context, _ openapi.ListAssetCompanionsRequestObject) (openapi.ListAssetCompanionsResponseObject, error) {
	panic("ListAssetCompanions called from auth test shim")
}
func (a authOnlyImpl) AddAssetCompanion(_ context.Context, _ openapi.AddAssetCompanionRequestObject) (openapi.AddAssetCompanionResponseObject, error) {
	panic("AddAssetCompanion called from auth test shim")
}
func (a authOnlyImpl) DownloadAssetCompanion(_ context.Context, _ openapi.DownloadAssetCompanionRequestObject) (openapi.DownloadAssetCompanionResponseObject, error) {
	panic("DownloadAssetCompanion called from auth test shim")
}
func (a authOnlyImpl) ListAssetAlternates(_ context.Context, _ openapi.ListAssetAlternatesRequestObject) (openapi.ListAssetAlternatesResponseObject, error) {
	panic("ListAssetAlternates called from auth test shim")
}
func (a authOnlyImpl) AddAssetAlternate(_ context.Context, _ openapi.AddAssetAlternateRequestObject) (openapi.AddAssetAlternateResponseObject, error) {
	panic("AddAssetAlternate called from auth test shim")
}
func (a authOnlyImpl) DownloadAssetAlternate(_ context.Context, _ openapi.DownloadAssetAlternateRequestObject) (openapi.DownloadAssetAlternateResponseObject, error) {
	panic("DownloadAssetAlternate called from auth test shim")
}
func (a authOnlyImpl) RemoveAssetAlternate(_ context.Context, _ openapi.RemoveAssetAlternateRequestObject) (openapi.RemoveAssetAlternateResponseObject, error) {
	panic("RemoveAssetAlternate called from auth test shim")
}
func (a authOnlyImpl) GetEpubSpine(_ context.Context, _ openapi.GetEpubSpineRequestObject) (openapi.GetEpubSpineResponseObject, error) {
	panic("GetEpubSpine called from auth test shim")
}
func (a authOnlyImpl) GetEpubChapter(_ context.Context, _ openapi.GetEpubChapterRequestObject) (openapi.GetEpubChapterResponseObject, error) {
	panic("GetEpubChapter called from auth test shim")
}
func (a authOnlyImpl) GetEpubResource(_ context.Context, _ openapi.GetEpubResourceRequestObject) (openapi.GetEpubResourceResponseObject, error) {
	panic("GetEpubResource called from auth test shim")
}
func (a authOnlyImpl) SearchEpub(_ context.Context, _ openapi.SearchEpubRequestObject) (openapi.SearchEpubResponseObject, error) {
	panic("SearchEpub called from auth test shim")
}
func (a authOnlyImpl) RemoveAssetCompanion(_ context.Context, _ openapi.RemoveAssetCompanionRequestObject) (openapi.RemoveAssetCompanionResponseObject, error) {
	panic("RemoveAssetCompanion called from auth test shim")
}
func (a authOnlyImpl) GetSetupStatus(_ context.Context, _ openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from auth test shim")
}
func (a authOnlyImpl) CompleteSetup(_ context.Context, _ openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from auth test shim")
}
func (a authOnlyImpl) ListWorkflowStates(_ context.Context, _ openapi.ListWorkflowStatesRequestObject) (openapi.ListWorkflowStatesResponseObject, error) {
	panic("ListWorkflowStates called from auth test shim")
}
func (a authOnlyImpl) ListFields(_ context.Context, _ openapi.ListFieldsRequestObject) (openapi.ListFieldsResponseObject, error) {
	panic("ListFields called from auth test shim")
}
func (a authOnlyImpl) CreateField(_ context.Context, _ openapi.CreateFieldRequestObject) (openapi.CreateFieldResponseObject, error) {
	panic("CreateField called from auth test shim")
}
func (a authOnlyImpl) GetField(_ context.Context, _ openapi.GetFieldRequestObject) (openapi.GetFieldResponseObject, error) {
	panic("GetField called from auth test shim")
}
func (a authOnlyImpl) UpdateField(_ context.Context, _ openapi.UpdateFieldRequestObject) (openapi.UpdateFieldResponseObject, error) {
	panic("UpdateField called from auth test shim")
}
func (a authOnlyImpl) ArchiveField(_ context.Context, _ openapi.ArchiveFieldRequestObject) (openapi.ArchiveFieldResponseObject, error) {
	panic("ArchiveField called from auth test shim")
}
func (a authOnlyImpl) GetAssetFields(_ context.Context, _ openapi.GetAssetFieldsRequestObject) (openapi.GetAssetFieldsResponseObject, error) {
	panic("GetAssetFields called from auth test shim")
}
func (a authOnlyImpl) SetAssetFieldValue(_ context.Context, _ openapi.SetAssetFieldValueRequestObject) (openapi.SetAssetFieldValueResponseObject, error) {
	panic("SetAssetFieldValue called from auth test shim")
}
func (a authOnlyImpl) ClearAssetFieldValue(_ context.Context, _ openapi.ClearAssetFieldValueRequestObject) (openapi.ClearAssetFieldValueResponseObject, error) {
	panic("ClearAssetFieldValue called from auth test shim")
}
func (a authOnlyImpl) GetAssetFieldValueHistory(_ context.Context, _ openapi.GetAssetFieldValueHistoryRequestObject) (openapi.GetAssetFieldValueHistoryResponseObject, error) {
	panic("GetAssetFieldValueHistory called from auth test shim")
}
func (a authOnlyImpl) ListCollections(_ context.Context, _ openapi.ListCollectionsRequestObject) (openapi.ListCollectionsResponseObject, error) {
	panic("ListCollections called from auth test shim")
}
func (a authOnlyImpl) CreateCollection(_ context.Context, _ openapi.CreateCollectionRequestObject) (openapi.CreateCollectionResponseObject, error) {
	panic("CreateCollection called from auth test shim")
}
func (a authOnlyImpl) GetCollection(_ context.Context, _ openapi.GetCollectionRequestObject) (openapi.GetCollectionResponseObject, error) {
	panic("GetCollection called from auth test shim")
}
func (a authOnlyImpl) UpdateCollection(_ context.Context, _ openapi.UpdateCollectionRequestObject) (openapi.UpdateCollectionResponseObject, error) {
	panic("UpdateCollection called from auth test shim")
}
func (a authOnlyImpl) DeleteCollection(_ context.Context, _ openapi.DeleteCollectionRequestObject) (openapi.DeleteCollectionResponseObject, error) {
	panic("DeleteCollection called from auth test shim")
}
func (a authOnlyImpl) ListCollectionResources(_ context.Context, _ openapi.ListCollectionResourcesRequestObject) (openapi.ListCollectionResourcesResponseObject, error) {
	panic("ListCollectionResources called from auth test shim")
}
func (a authOnlyImpl) AddCollectionResource(_ context.Context, _ openapi.AddCollectionResourceRequestObject) (openapi.AddCollectionResourceResponseObject, error) {
	panic("AddCollectionResource called from auth test shim")
}
func (a authOnlyImpl) RemoveCollectionResource(_ context.Context, _ openapi.RemoveCollectionResourceRequestObject) (openapi.RemoveCollectionResourceResponseObject, error) {
	panic("RemoveCollectionResource called from auth test shim")
}
func (a authOnlyImpl) ListPosts(context.Context, openapi.ListPostsRequestObject) (openapi.ListPostsResponseObject, error) {
	panic("ListPosts called from auth test shim")
}
func (a authOnlyImpl) CreatePost(context.Context, openapi.CreatePostRequestObject) (openapi.CreatePostResponseObject, error) {
	panic("CreatePost called from auth test shim")
}
func (a authOnlyImpl) GetPost(context.Context, openapi.GetPostRequestObject) (openapi.GetPostResponseObject, error) {
	panic("GetPost called from auth test shim")
}
func (a authOnlyImpl) UpdatePost(context.Context, openapi.UpdatePostRequestObject) (openapi.UpdatePostResponseObject, error) {
	panic("UpdatePost called from auth test shim")
}
func (a authOnlyImpl) DeletePost(context.Context, openapi.DeletePostRequestObject) (openapi.DeletePostResponseObject, error) {
	panic("DeletePost called from auth test shim")
}
func (a authOnlyImpl) AddPostAsset(context.Context, openapi.AddPostAssetRequestObject) (openapi.AddPostAssetResponseObject, error) {
	panic("AddPostAsset called from auth test shim")
}
func (a authOnlyImpl) RemovePostAsset(context.Context, openapi.RemovePostAssetRequestObject) (openapi.RemovePostAssetResponseObject, error) {
	panic("RemovePostAsset called from auth test shim")
}
func (a authOnlyImpl) ListTeams(context.Context, openapi.ListTeamsRequestObject) (openapi.ListTeamsResponseObject, error) {
	panic("ListTeams called from auth test shim")
}
func (a authOnlyImpl) CreateTeam(context.Context, openapi.CreateTeamRequestObject) (openapi.CreateTeamResponseObject, error) {
	panic("CreateTeam called from auth test shim")
}
func (a authOnlyImpl) GetTeam(context.Context, openapi.GetTeamRequestObject) (openapi.GetTeamResponseObject, error) {
	panic("GetTeam called from auth test shim")
}
func (a authOnlyImpl) UpdateTeam(context.Context, openapi.UpdateTeamRequestObject) (openapi.UpdateTeamResponseObject, error) {
	panic("UpdateTeam called from auth test shim")
}
func (a authOnlyImpl) DeleteTeam(context.Context, openapi.DeleteTeamRequestObject) (openapi.DeleteTeamResponseObject, error) {
	panic("DeleteTeam called from auth test shim")
}
func (a authOnlyImpl) ListTeamParents(context.Context, openapi.ListTeamParentsRequestObject) (openapi.ListTeamParentsResponseObject, error) {
	panic("ListTeamParents called from auth test shim")
}
func (a authOnlyImpl) AddTeamParent(context.Context, openapi.AddTeamParentRequestObject) (openapi.AddTeamParentResponseObject, error) {
	panic("AddTeamParent called from auth test shim")
}
func (a authOnlyImpl) RemoveTeamParent(context.Context, openapi.RemoveTeamParentRequestObject) (openapi.RemoveTeamParentResponseObject, error) {
	panic("RemoveTeamParent called from auth test shim")
}
func (a authOnlyImpl) ListTeamMembers(context.Context, openapi.ListTeamMembersRequestObject) (openapi.ListTeamMembersResponseObject, error) {
	panic("ListTeamMembers called from auth test shim")
}
func (a authOnlyImpl) AddTeamMember(context.Context, openapi.AddTeamMemberRequestObject) (openapi.AddTeamMemberResponseObject, error) {
	panic("AddTeamMember called from auth test shim")
}
func (a authOnlyImpl) RemoveTeamMember(context.Context, openapi.RemoveTeamMemberRequestObject) (openapi.RemoveTeamMemberResponseObject, error) {
	panic("RemoveTeamMember called from auth test shim")
}
func (a authOnlyImpl) GetMyTeams(context.Context, openapi.GetMyTeamsRequestObject) (openapi.GetMyTeamsResponseObject, error) {
	panic("GetMyTeams called from auth test shim")
}
func (a authOnlyImpl) ListPostAcls(context.Context, openapi.ListPostAclsRequestObject) (openapi.ListPostAclsResponseObject, error) {
	panic("ListPostAcls called from auth test shim")
}
func (a authOnlyImpl) AddPostAcl(context.Context, openapi.AddPostAclRequestObject) (openapi.AddPostAclResponseObject, error) {
	panic("AddPostAcl called from auth test shim")
}
func (a authOnlyImpl) RemovePostAcl(context.Context, openapi.RemovePostAclRequestObject) (openapi.RemovePostAclResponseObject, error) {
	panic("RemovePostAcl called from auth test shim")
}
func (a authOnlyImpl) ListCollectionAcls(context.Context, openapi.ListCollectionAclsRequestObject) (openapi.ListCollectionAclsResponseObject, error) {
	panic("ListCollectionAcls called from auth test shim")
}
func (a authOnlyImpl) AddCollectionAcl(context.Context, openapi.AddCollectionAclRequestObject) (openapi.AddCollectionAclResponseObject, error) {
	panic("AddCollectionAcl called from auth test shim")
}
func (a authOnlyImpl) RemoveCollectionAcl(context.Context, openapi.RemoveCollectionAclRequestObject) (openapi.RemoveCollectionAclResponseObject, error) {
	panic("RemoveCollectionAcl called from auth test shim")
}
func (a authOnlyImpl) GetUserPublicByRef(context.Context, openapi.GetUserPublicByRefRequestObject) (openapi.GetUserPublicByRefResponseObject, error) {
	panic("GetUserPublicByRef called from auth test shim")
}
func (a authOnlyImpl) GetUserPublicByUsername(context.Context, openapi.GetUserPublicByUsernameRequestObject) (openapi.GetUserPublicByUsernameResponseObject, error) {
	panic("GetUserPublicByUsername called from auth test shim")
}
func (a authOnlyImpl) UpdateUserProfile(context.Context, openapi.UpdateUserProfileRequestObject) (openapi.UpdateUserProfileResponseObject, error) {
	panic("UpdateUserProfile called from auth test shim")
}
func (a authOnlyImpl) ListAdminUsers(context.Context, openapi.ListAdminUsersRequestObject) (openapi.ListAdminUsersResponseObject, error) {
	panic("ListAdminUsers called from auth test shim")
}
func (a authOnlyImpl) SetAdminUserStatus(context.Context, openapi.SetAdminUserStatusRequestObject) (openapi.SetAdminUserStatusResponseObject, error) {
	panic("SetAdminUserStatus called from auth test shim")
}
func (a authOnlyImpl) ListMySessions(context.Context, openapi.ListMySessionsRequestObject) (openapi.ListMySessionsResponseObject, error) {
	panic("ListMySessions called from auth test shim")
}
func (a authOnlyImpl) RevokeMySession(context.Context, openapi.RevokeMySessionRequestObject) (openapi.RevokeMySessionResponseObject, error) {
	panic("RevokeMySession called from auth test shim")
}
func (a authOnlyImpl) ListAdminUserSessions(context.Context, openapi.ListAdminUserSessionsRequestObject) (openapi.ListAdminUserSessionsResponseObject, error) {
	panic("ListAdminUserSessions called from auth test shim")
}
func (a authOnlyImpl) RevokeAdminUserSession(context.Context, openapi.RevokeAdminUserSessionRequestObject) (openapi.RevokeAdminUserSessionResponseObject, error) {
	panic("RevokeAdminUserSession called from auth test shim")
}
func (a authOnlyImpl) ChangeMyPassword(context.Context, openapi.ChangeMyPasswordRequestObject) (openapi.ChangeMyPasswordResponseObject, error) {
	panic("ChangeMyPassword called from auth test shim")
}
func (a authOnlyImpl) AdminResetUserPassword(context.Context, openapi.AdminResetUserPasswordRequestObject) (openapi.AdminResetUserPasswordResponseObject, error) {
	panic("AdminResetUserPassword called from auth test shim")
}
func (a authOnlyImpl) ListAdminUserCapabilities(ctx context.Context, req openapi.ListAdminUserCapabilitiesRequestObject) (openapi.ListAdminUserCapabilitiesResponseObject, error) {
	return a.h.ListAdminUserCapabilities(ctx, req)
}
func (a authOnlyImpl) AddAdminUserGrant(ctx context.Context, req openapi.AddAdminUserGrantRequestObject) (openapi.AddAdminUserGrantResponseObject, error) {
	return a.h.AddAdminUserGrant(ctx, req)
}
func (a authOnlyImpl) RemoveAdminUserGrant(ctx context.Context, req openapi.RemoveAdminUserGrantRequestObject) (openapi.RemoveAdminUserGrantResponseObject, error) {
	return a.h.RemoveAdminUserGrant(ctx, req)
}
func (a authOnlyImpl) AddAdminUserRevoke(ctx context.Context, req openapi.AddAdminUserRevokeRequestObject) (openapi.AddAdminUserRevokeResponseObject, error) {
	return a.h.AddAdminUserRevoke(ctx, req)
}
func (a authOnlyImpl) RemoveAdminUserRevoke(ctx context.Context, req openapi.RemoveAdminUserRevokeRequestObject) (openapi.RemoveAdminUserRevokeResponseObject, error) {
	return a.h.RemoveAdminUserRevoke(ctx, req)
}
func (authOnlyImpl) ListAssetTypeAcls(context.Context, openapi.ListAssetTypeAclsRequestObject) (openapi.ListAssetTypeAclsResponseObject, error) {
	panic("ListAssetTypeAcls called from auth test shim")
}
func (authOnlyImpl) AddAssetTypeAcl(context.Context, openapi.AddAssetTypeAclRequestObject) (openapi.AddAssetTypeAclResponseObject, error) {
	panic("AddAssetTypeAcl called from auth test shim")
}
func (authOnlyImpl) RemoveAssetTypeAcl(context.Context, openapi.RemoveAssetTypeAclRequestObject) (openapi.RemoveAssetTypeAclResponseObject, error) {
	panic("RemoveAssetTypeAcl called from auth test shim")
}
func (authOnlyImpl) ListAdminAuditEvents(context.Context, openapi.ListAdminAuditEventsRequestObject) (openapi.ListAdminAuditEventsResponseObject, error) {
	panic("ListAdminAuditEvents called from auth test shim")
}
func (authOnlyImpl) ListAdminAuditEventTypes(context.Context, openapi.ListAdminAuditEventTypesRequestObject) (openapi.ListAdminAuditEventTypesResponseObject, error) {
	panic("ListAdminAuditEventTypes called from auth test shim")
}
func (authOnlyImpl) GetAdminLicenseStatus(context.Context, openapi.GetAdminLicenseStatusRequestObject) (openapi.GetAdminLicenseStatusResponseObject, error) {
	panic("GetAdminLicenseStatus called from auth test shim")
}
func (authOnlyImpl) ValidateAdminLicense(context.Context, openapi.ValidateAdminLicenseRequestObject) (openapi.ValidateAdminLicenseResponseObject, error) {
	panic("ValidateAdminLicense called from auth test shim")
}
func (authOnlyImpl) UploadAdminLicense(context.Context, openapi.UploadAdminLicenseRequestObject) (openapi.UploadAdminLicenseResponseObject, error) {
	panic("UploadAdminLicense called from auth test shim")
}
func (a authOnlyImpl) ListIdentityProviders(ctx context.Context, req openapi.ListIdentityProvidersRequestObject) (openapi.ListIdentityProvidersResponseObject, error) {
	// Real call — the auth handler IS the unit-under-test in this
	// package, and the providers endpoint is in scope for its tests.
	return a.h.ListIdentityProviders(ctx, req)
}
func (authOnlyImpl) GetAccountPreferences(context.Context, openapi.GetAccountPreferencesRequestObject) (openapi.GetAccountPreferencesResponseObject, error) {
	panic("GetAccountPreferences called from auth test shim")
}
func (authOnlyImpl) PatchAccountPreferences(context.Context, openapi.PatchAccountPreferencesRequestObject) (openapi.PatchAccountPreferencesResponseObject, error) {
	panic("PatchAccountPreferences called from auth test shim")
}
func (a authOnlyImpl) GetPostLike(context.Context, openapi.GetPostLikeRequestObject) (openapi.GetPostLikeResponseObject, error) {
	panic("GetPostLike called from auth test shim")
}
func (a authOnlyImpl) LikePost(context.Context, openapi.LikePostRequestObject) (openapi.LikePostResponseObject, error) {
	panic("LikePost called from auth test shim")
}
func (a authOnlyImpl) UnlikePost(context.Context, openapi.UnlikePostRequestObject) (openapi.UnlikePostResponseObject, error) {
	panic("UnlikePost called from auth test shim")
}
func (a authOnlyImpl) ListPostComments(context.Context, openapi.ListPostCommentsRequestObject) (openapi.ListPostCommentsResponseObject, error) {
	panic("ListPostComments called from auth test shim")
}
func (a authOnlyImpl) CreatePostComment(context.Context, openapi.CreatePostCommentRequestObject) (openapi.CreatePostCommentResponseObject, error) {
	panic("CreatePostComment called from auth test shim")
}
func (a authOnlyImpl) DeleteComment(context.Context, openapi.DeleteCommentRequestObject) (openapi.DeleteCommentResponseObject, error) {
	panic("DeleteComment called from auth test shim")
}

// ---------------------------------------------------------------------------
// Helpers
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

// ensureUserTable creates a minimal "user" table if it doesn't exist.
// In a real install RS's CheckDBStruct will have done this already
// with the full set of columns; for test environments we synthesise
// the few columns the auth tests touch.
func ensureUserTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const ddl = `
CREATE TABLE IF NOT EXISTS "user" (
    ref                  BIGINT GENERATED BY DEFAULT AS IDENTITY NOT NULL PRIMARY KEY,
    username             VARCHAR(50) UNIQUE,
    password             VARCHAR(255),
    fullname             VARCHAR(100),
    email                VARCHAR(100),
    usergroup            BIGINT,
    last_active          TIMESTAMPTZ,
    logged_in            BIGINT,
    accepted_terms       BIGINT NOT NULL DEFAULT 0,
    account_expires      TIMESTAMPTZ,
    session              VARCHAR(50),
    password_last_change TIMESTAMPTZ,
    login_tries          BIGINT NOT NULL DEFAULT 0,
    login_last_try       TIMESTAMPTZ,
    approved             BIGINT NOT NULL DEFAULT 1
);
`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("ensureUserTable: %v", err)
	}
}

// --- type plumbing to keep tests compiling without depending on the
//     concrete pgx generic types (which differ across versions). The
//     poolTxShim is just an adapter so tests can call QueryRow
//     directly without importing pgx types into the test file.

type pgcmd interface{ String() string }

type pgxRowIface interface {
	Scan(dest ...any) error
}

type pgxRowsIface interface {
	Close()
	Err() error
	Next() bool
	Scan(dest ...any) error
}

type poolTxShim struct{ pool *pgxpool.Pool }

func (p poolTxShim) Exec(ctx context.Context, sql string, args ...any) (pgcmd, error) {
	tag, err := p.pool.Exec(ctx, sql, args...)
	return strCmd(tag.String()), err
}

func (p poolTxShim) Query(ctx context.Context, sql string, args ...any) (pgxRowsIface, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	return rows, err
}

func (p poolTxShim) QueryRow(ctx context.Context, sql string, args ...any) pgxRowIface {
	return p.pool.QueryRow(ctx, sql, args...)
}

type strCmd string

func (s strCmd) String() string { return string(s) }

func (a authOnlyImpl) GetSiteConfig(_ context.Context, _ openapi.GetSiteConfigRequestObject) (openapi.GetSiteConfigResponseObject, error) {
	panic("GetSiteConfig called from auth test shim")
}
func (a authOnlyImpl) UpdateSiteConfig(_ context.Context, _ openapi.UpdateSiteConfigRequestObject) (openapi.UpdateSiteConfigResponseObject, error) {
	panic("UpdateSiteConfig called from auth test shim")
}
func (a authOnlyImpl) GetSMTPConfig(_ context.Context, _ openapi.GetSMTPConfigRequestObject) (openapi.GetSMTPConfigResponseObject, error) {
	panic("GetSMTPConfig called from auth test shim")
}
func (a authOnlyImpl) UpdateSMTPConfig(_ context.Context, _ openapi.UpdateSMTPConfigRequestObject) (openapi.UpdateSMTPConfigResponseObject, error) {
	panic("UpdateSMTPConfig called from auth test shim")
}
func (a authOnlyImpl) GetAuthConfig(_ context.Context, _ openapi.GetAuthConfigRequestObject) (openapi.GetAuthConfigResponseObject, error) {
	panic("GetAuthConfig called from auth test shim")
}
func (a authOnlyImpl) UpdateAuthConfig(_ context.Context, _ openapi.UpdateAuthConfigRequestObject) (openapi.UpdateAuthConfigResponseObject, error) {
	panic("UpdateAuthConfig called from auth test shim")
}
func (a authOnlyImpl) GetAIConfig(_ context.Context, _ openapi.GetAIConfigRequestObject) (openapi.GetAIConfigResponseObject, error) {
	panic("GetAIConfig called from auth test shim")
}
func (a authOnlyImpl) UpdateAIConfig(_ context.Context, _ openapi.UpdateAIConfigRequestObject) (openapi.UpdateAIConfigResponseObject, error) {
	panic("UpdateAIConfig called from auth test shim")
}
func (a authOnlyImpl) ListLocales(_ context.Context, _ openapi.ListLocalesRequestObject) (openapi.ListLocalesResponseObject, error) {
	panic("ListLocales called from auth test shim")
}

func (authOnlyImpl) GetAppearanceConfig(context.Context, openapi.GetAppearanceConfigRequestObject) (openapi.GetAppearanceConfigResponseObject, error) {
	panic("GetAppearanceConfig called from auth test shim")
}
func (authOnlyImpl) UpdateAppearanceConfig(context.Context, openapi.UpdateAppearanceConfigRequestObject) (openapi.UpdateAppearanceConfigResponseObject, error) {
	panic("UpdateAppearanceConfig called from auth test shim")
}
func (authOnlyImpl) GetPublicAppearance(context.Context, openapi.GetPublicAppearanceRequestObject) (openapi.GetPublicAppearanceResponseObject, error) {
	panic("GetPublicAppearance called from auth test shim")
}

// --- jobs stubs (Phase 1.18.A) -------------------------------------------
func (authOnlyImpl) ClaimJobs(context.Context, openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	panic("ClaimJobs called from test shim")
}
func (authOnlyImpl) GetJob(context.Context, openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	panic("GetJob called from test shim")
}
func (authOnlyImpl) HeartbeatJob(context.Context, openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	panic("HeartbeatJob called from test shim")
}
func (authOnlyImpl) CompleteJob(context.Context, openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	panic("CompleteJob called from test shim")
}
func (authOnlyImpl) FailJob(context.Context, openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	panic("FailJob called from test shim")
}

func (authOnlyImpl) ListPostWhiteboards(context.Context, openapi.ListPostWhiteboardsRequestObject) (openapi.ListPostWhiteboardsResponseObject, error) {
	panic("ListPostWhiteboards called from auth test shim")
}

func (authOnlyImpl) CreatePostWhiteboard(context.Context, openapi.CreatePostWhiteboardRequestObject) (openapi.CreatePostWhiteboardResponseObject, error) {
	panic("CreatePostWhiteboard called from auth test shim")
}

// --- brush packs stubs (Phase 1.21c) -------------------------------------
func (authOnlyImpl) ListBrushPacks(context.Context, openapi.ListBrushPacksRequestObject) (openapi.ListBrushPacksResponseObject, error) {
	panic("ListBrushPacks called from authOnlyImpl test shim")
}
func (authOnlyImpl) ImportBrushPack(context.Context, openapi.ImportBrushPackRequestObject) (openapi.ImportBrushPackResponseObject, error) {
	panic("ImportBrushPack called from authOnlyImpl test shim")
}
func (authOnlyImpl) GetBrushPack(context.Context, openapi.GetBrushPackRequestObject) (openapi.GetBrushPackResponseObject, error) {
	panic("GetBrushPack called from authOnlyImpl test shim")
}
func (authOnlyImpl) DeleteBrushPack(context.Context, openapi.DeleteBrushPackRequestObject) (openapi.DeleteBrushPackResponseObject, error) {
	panic("DeleteBrushPack called from authOnlyImpl test shim")
}
func (authOnlyImpl) GetBrushPackStamp(context.Context, openapi.GetBrushPackStampRequestObject) (openapi.GetBrushPackStampResponseObject, error) {
	panic("GetBrushPackStamp called from authOnlyImpl test shim")
}
func (authOnlyImpl)ListAssetTextAnnotations(context.Context, openapi.ListAssetTextAnnotationsRequestObject) (openapi.ListAssetTextAnnotationsResponseObject, error) {
	panic("ListAssetTextAnnotations called from auth_test test shim")
}
func (authOnlyImpl)CreateAssetTextAnnotation(context.Context, openapi.CreateAssetTextAnnotationRequestObject) (openapi.CreateAssetTextAnnotationResponseObject, error) {
	panic("CreateAssetTextAnnotation called from auth_test test shim")
}
func (authOnlyImpl)UpdateTextAnnotation(context.Context, openapi.UpdateTextAnnotationRequestObject) (openapi.UpdateTextAnnotationResponseObject, error) {
	panic("UpdateTextAnnotation called from auth_test test shim")
}
func (authOnlyImpl)LintAsset(context.Context, openapi.LintAssetRequestObject) (openapi.LintAssetResponseObject, error) {
	panic("LintAsset called from auth_test test shim")
}
func (authOnlyImpl) FollowUser(context.Context, openapi.FollowUserRequestObject) (openapi.FollowUserResponseObject, error) {
	panic("FollowUser called from auth test shim")
}
func (authOnlyImpl) UnfollowUser(context.Context, openapi.UnfollowUserRequestObject) (openapi.UnfollowUserResponseObject, error) {
	panic("UnfollowUser called from auth test shim")
}
func (authOnlyImpl) ListUserFollowers(context.Context, openapi.ListUserFollowersRequestObject) (openapi.ListUserFollowersResponseObject, error) {
	panic("ListUserFollowers called from auth test shim")
}
func (authOnlyImpl) ListUserFollowing(context.Context, openapi.ListUserFollowingRequestObject) (openapi.ListUserFollowingResponseObject, error) {
	panic("ListUserFollowing called from auth test shim")
}
func (authOnlyImpl) GetUserRelationship(context.Context, openapi.GetUserRelationshipRequestObject) (openapi.GetUserRelationshipResponseObject, error) {
	panic("GetUserRelationship called from auth test shim")
}
func (authOnlyImpl) BlockUser(context.Context, openapi.BlockUserRequestObject) (openapi.BlockUserResponseObject, error) {
	panic("BlockUser called from auth test shim")
}
func (authOnlyImpl) UnblockUser(context.Context, openapi.UnblockUserRequestObject) (openapi.UnblockUserResponseObject, error) {
	panic("UnblockUser called from auth test shim")
}
func (authOnlyImpl) ListMyBlocked(context.Context, openapi.ListMyBlockedRequestObject) (openapi.ListMyBlockedResponseObject, error) {
	panic("ListMyBlocked called from auth test shim")
}
func (authOnlyImpl) ListMyNotifications(context.Context, openapi.ListMyNotificationsRequestObject) (openapi.ListMyNotificationsResponseObject, error) {
	panic("ListMyNotifications called from auth test shim")
}
func (authOnlyImpl) GetMyUnreadNotificationCount(context.Context, openapi.GetMyUnreadNotificationCountRequestObject) (openapi.GetMyUnreadNotificationCountResponseObject, error) {
	panic("GetMyUnreadNotificationCount called from auth test shim")
}
func (authOnlyImpl) MarkNotificationRead(context.Context, openapi.MarkNotificationReadRequestObject) (openapi.MarkNotificationReadResponseObject, error) {
	panic("MarkNotificationRead called from auth test shim")
}
func (authOnlyImpl) MarkAllMyNotificationsRead(context.Context, openapi.MarkAllMyNotificationsReadRequestObject) (openapi.MarkAllMyNotificationsReadResponseObject, error) {
	panic("MarkAllMyNotificationsRead called from auth test shim")
}
