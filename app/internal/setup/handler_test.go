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
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
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
			 JOIN "user" u ON u.ref = ur.user_ref WHERE u.username = $1`,
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
	// Snapshot of every system.admin-granting user_roles row that
	// existed before the test wiped them. Repopulated post-test so
	// the dev environment's interactive admin keeps working without
	// a manual re-INSERT after every Go test run.
	savedAdmins []savedUserRole
}

type savedUserRole struct {
	userRef int64
	roleID  string
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
	// Defers run LIFO: pool.Close() is registered FIRST so it runs
	// LAST, ensuring the admin snapshot-restore (registered after)
	// still has a live pool to talk to. t.Cleanup runs even later
	// than function defers, so it can't be used here.
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

	// Snapshot any pre-existing admins BEFORE we wipe them, then
	// restore at function exit so the dev environment's interactive
	// admin survives unchanged. Pre-clean still has to drop everything
	// to satisfy the test's needs_setup=true gate.
	//
	// Use defer (not t.Cleanup): t.Cleanup runs after function defers,
	// by which time `defer pool.Close()` above has shut the pool. Our
	// restore needs a live pool, so it has to ride a defer that fires
	// BEFORE pool close — which means registering it AFTER pool.Close's
	// defer so LIFO order works in our favor.
	fx.snapshotAdmins(ctx)
	fx.cleanupAdmin(ctx)
	defer func() {
		bgCtx := context.Background()
		fx.cleanupAdmin(bgCtx)
		fx.restoreAdmins(bgCtx)
	}()

	fx.installHandler()
	fn(ctx, fx)
}

func (f *fixture) installHandler() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := setup.NewHandler(f.pool, logger, f.cfg, f.sysCfg, "fs")
	router := chi.NewRouter()
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	f.router = router
}

// snapshotAdmins records every system.admin-granting user_roles row
// that currently exists so restoreAdmins can put them back at the
// end of the test. Without this, every run of `./scripts/test.sh
// --go` left the developer's interactive admin demoted.
func (f *fixture) snapshotAdmins(ctx context.Context) {
	rows, err := f.pool.Query(ctx, `
		SELECT ur.user_ref, ur.role_id::text
		FROM user_roles ur
		WHERE ur.role_id IN (
		    SELECT DISTINCT rc.role_id FROM role_capabilities rc
		    WHERE rc.capability_code = 'system.admin'
		)
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r savedUserRole
		if err := rows.Scan(&r.userRef, &r.roleID); err != nil {
			continue
		}
		f.savedAdmins = append(f.savedAdmins, r)
	}
}

// restoreAdmins re-INSERTs the rows captured by snapshotAdmins. Run
// AFTER cleanupAdmin so the test's own admin doesn't get spared.
// Tolerates rows that were since deleted (e.g. user removed in the
// test) — ON CONFLICT DO NOTHING and ignoring errors keeps the test
// teardown best-effort.
func (f *fixture) restoreAdmins(ctx context.Context) {
	for _, r := range f.savedAdmins {
		_, _ = f.pool.Exec(ctx, `
			INSERT INTO user_roles (user_ref, role_id)
			VALUES ($1, $2::uuid)
			ON CONFLICT DO NOTHING
		`, r.userRef, r.roleID)
	}
}

// cleanupAdmin returns the DB to needs_setup=true: removes every
// user_role row that grants system.admin (so CountSystemAdmins drops
// to zero) plus this fixture's specific test user. The destruction
// is paired with snapshotAdmins / restoreAdmins above so the dev
// admin gets re-INSERTed at the very end — Go test runs no longer
// require a manual user_roles re-INSERT to log back in.
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

// shimImpl satisfies the full StrictServerInterface — overrides
// only the setup methods this test exercises; every other
// operation panics via the embedded *strictservershim.PanicShim
// so a misrouted call surfaces loudly.
type shimImpl struct {
	*strictservershim.PanicShim
	h *setup.Handler
}

func (s shimImpl) GetSetupStatus(ctx context.Context, req openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	return s.h.GetSetupStatus(ctx, req)
}
func (s shimImpl) CompleteSetup(ctx context.Context, req openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	return s.h.CompleteSetup(ctx, req)
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



