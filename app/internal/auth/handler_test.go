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
		if cookie.SameSite != http.SameSiteStrictMode {
			t.Error("cookie should be SameSite=Strict")
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

	// Idempotent upsert by username.
	const upsert = `
		INSERT INTO "user" (username, password, fullname, email, approved)
		VALUES ($1, $2, 'Go Auth Test', 'go-auth-test@example.com', 1)
		ON CONFLICT (username) DO UPDATE SET password = EXCLUDED.password
		RETURNING ref
	`
	// Some installs don't have a UNIQUE constraint on username, in
	// which case ON CONFLICT fails. Fall back to delete+insert.
	var userRef int64
	if err := pool.QueryRow(ctx, upsert, username, hash).Scan(&userRef); err != nil {
		if _, err2 := pool.Exec(ctx, `DELETE FROM "user" WHERE username = $1`, username); err2 != nil {
			t.Fatalf("cleanup before insert: %v", err2)
		}
		const insert = `
			INSERT INTO "user" (username, password, fullname, email, approved)
			VALUES ($1, $2, 'Go Auth Test', 'go-auth-test@example.com', 1)
			RETURNING ref
		`
		if err := pool.QueryRow(ctx, insert, username, hash).Scan(&userRef); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	t.Cleanup(func() {
		// Best-effort: remove the user and any rows that reference them.
		cleanCtx := context.Background()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM api_tokens WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM user_capability_grants WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM user_capability_revokes WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM user_role WHERE rs_user_id = $1`, userRef)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM "user" WHERE ref = $1`, userRef)
	})

	// Real chi router wired exactly as the production server does it.
	handler := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), testScrambleKey, 7)
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
func (a authOnlyImpl) ListResourceTypes(_ context.Context, _ openapi.ListResourceTypesRequestObject) (openapi.ListResourceTypesResponseObject, error) {
	panic("ListResourceTypes called from auth test shim")
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
