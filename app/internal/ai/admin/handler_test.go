// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.A admin handler tests. Integration tests against the
// live postgres compose stack; matches the established cadence for
// other DB-backed packages.
package admin_test

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	aiadmin "github.com/mscrnt/artist-alley/app/internal/ai/admin"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

func TestGetConfig_RequiresAuth(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	router := makeRouter(t, pool, nil)

	resp := mustDo(t, router, http.MethodGet, "/admin/ai/config", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestGetConfig_RequiresAIAdminCap(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	router := makeRouter(t, pool, []string{}) // authed but no caps

	resp := mustDo(t, router, http.MethodGet, "/admin/ai/config", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestGetConfig_HappyPath_ReturnsSeededDefaults(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	router := makeRouter(t, pool, []string{aiadmin.CapAIAdmin})

	resp := mustDo(t, router, http.MethodGet, "/admin/ai/config", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	// The migration seeded routing defaults; should appear in the response.
	if !strings.Contains(body, `"tag":"ollama"`) {
		t.Errorf("default routing missing from response: %s", body)
	}
	if !strings.Contains(body, `"caption":"claude"`) {
		t.Errorf("default routing missing from response: %s", body)
	}
}

func TestPutConfig_ValidatorRejectsLockOnWithEmptyLocalList(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	router := makeRouter(t, pool, []string{aiadmin.CapAIAdmin})

	// lock_sensitive_to_local=true + local_providers=[] is the
	// archetypal blocking finding; the existing config stays in
	// place and the response carries the 422 with code.
	body := `{
		"enabled": true,
		"routing": {"complete":"claude","embed":"clip_local","tag":"ollama","caption":"claude","transcribe":"whisper_local"},
		"fallback_chains": {},
		"privacy": {"lock_sensitive_to_local": true, "local_providers": []},
		"default_budget": {"soft_warning_usd": 0, "hard_cap_usd": 0}
	}`
	resp := mustDo(t, router, http.MethodPut, "/admin/ai/config", body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	respBody := readBody(t, resp)
	if !strings.Contains(respBody, "privacy_lock_with_empty_local_list") {
		t.Errorf("findings missing: %s", respBody)
	}
}

func TestPutConfig_HappyPath_PersistsAndReturns200(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so reset BEFORE closing the pool
	// (otherwise the cleanup hits a closed pool and silently fails,
	// leaving ai.routing in the dirtied state for the next package
	// run — exactly the brittleness that flaked
	// TestGetConfig_HappyPath_ReturnsSeededDefaults locally).
	t.Cleanup(func() {
		resetConfigToDefaults(context.Background(), pool)
		pool.Close()
	})

	router := makeRouter(t, pool, []string{aiadmin.CapAIAdmin})

	body := `{
		"enabled": true,
		"routing": {"complete":"openai","embed":"clip_local","tag":"openai","caption":"openai","transcribe":"openai"},
		"fallback_chains": {"complete":["openai","claude"]},
		"privacy": {"lock_sensitive_to_local": true, "local_providers": ["ollama","clip_local"]},
		"default_budget": {"soft_warning_usd": 50, "hard_cap_usd": 200}
	}`
	resp := mustDo(t, router, http.MethodPut, "/admin/ai/config", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}

	// Re-read; the persisted values should round-trip.
	resp = mustDo(t, router, http.MethodGet, "/admin/ai/config", "")
	got := readBody(t, resp)
	if !strings.Contains(got, `"enabled":true`) {
		t.Errorf("enabled didn't persist: %s", got)
	}
	if !strings.Contains(got, `"hard_cap_usd":200`) {
		t.Errorf("budget didn't persist: %s", got)
	}
}

func TestGetUsage_EmptyPeriod_ReturnsZeroes(t *testing.T) {
	pool := openPool(t)
	defer pool.Close()
	router := makeRouter(t, pool, []string{aiadmin.CapAIAdmin})

	// Pick a billing period in the far past that won't have data.
	resp := mustDo(t, router, http.MethodGet, "/admin/ai/usage?billing_period=2020-01", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"billing_period":"2020-01"`) {
		t.Errorf("period not reflected: %s", body)
	}
	if !strings.Contains(body, `"total_cost_usd_micros":0`) {
		t.Errorf("expected zero total: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Test scaffold
// ---------------------------------------------------------------------------

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user + " dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// makeRouter builds the chi router with an injected identity (caps=nil
// for unauthed, caps=[] for authed-no-caps, caps=[CapAIAdmin] for
// the happy path).
func makeRouter(t *testing.T, pool *pgxpool.Pool, caps []string) chi.Router {
	t.Helper()
	caches := ai.NewCaches(nil) // no registry needed for tests
	loader := ai.NewLoader(pool, caches)
	h := aiadmin.NewHandler(pool, loader, caches)

	r := chi.NewRouter()
	if caps != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				id := &auth.Identity{UserRef: 1, AuthMethod: "session", Capabilities: caps}
				next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
			})
		})
	}
	openapi.HandlerFromMux(openapi.NewStrictHandler(adminShim{
		PanicShim: &strictservershim.PanicShim{},
		h:         h,
	}, nil), r)
	return r
}

// adminShim implements openapi.StrictServerInterface via PanicShim
// for the methods we don't touch, and delegates the three Phase
// 1.14.A endpoints to the handler.
type adminShim struct {
	*strictservershim.PanicShim
	h *aiadmin.Handler
}

func (s adminShim) GetAIInferenceConfig(ctx context.Context, req openapi.GetAIInferenceConfigRequestObject) (openapi.GetAIInferenceConfigResponseObject, error) {
	return s.h.GetAIInferenceConfig(ctx, req)
}
func (s adminShim) UpdateAIInferenceConfig(ctx context.Context, req openapi.UpdateAIInferenceConfigRequestObject) (openapi.UpdateAIInferenceConfigResponseObject, error) {
	return s.h.UpdateAIInferenceConfig(ctx, req)
}
func (s adminShim) GetAIUsage(ctx context.Context, req openapi.GetAIUsageRequestObject) (openapi.GetAIUsageResponseObject, error) {
	return s.h.GetAIUsage(ctx, req)
}

func mustDo(t *testing.T, router chi.Router, method, path, body string) *http.Response {
	t.Helper()
	var bodyReader interface{ Read(p []byte) (int, error) }
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	var req *http.Request
	if bodyReader != nil {
		req, _ = http.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest(method, path, nil)
	}
	rr := &recordingResponseWriter{}
	router.ServeHTTP(rr, req)
	return rr.toResponse()
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return ""
	}
	b := make([]byte, 8192)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}

// recordingResponseWriter is a minimal httptest.ResponseRecorder
// substitute that produces an http.Response so mustDo's return
// type stays clean.
type recordingResponseWriter struct {
	code int
	body []byte
	hdr  http.Header
}

func (w *recordingResponseWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *recordingResponseWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}
func (w *recordingResponseWriter) WriteHeader(code int) { w.code = code }
func (w *recordingResponseWriter) toResponse() *http.Response {
	if w.code == 0 {
		w.code = 200
	}
	return &http.Response{
		StatusCode: w.code,
		Header:     w.hdr,
		Body:       newBodyReader(w.body),
	}
}

func newBodyReader(b []byte) *bodyReader { return &bodyReader{b: b} }

type bodyReader struct {
	b   []byte
	pos int
}

func (r *bodyReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, nil // EOF-equivalent for our purposes (readBody handles partial)
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
func (r *bodyReader) Close() error { return nil }

// resetConfigToDefaults restores migration 00009's seeded values
// so a test that wrote a custom config doesn't pollute the next
// run.
func resetConfigToDefaults(ctx context.Context, pool *pgxpool.Pool) {
	pairs := map[string]string{
		"ai.enabled":                         `false`,
		"ai.routing":                         `{"tag":"ollama","caption":"claude","embed":"clip_local","transcribe":"whisper_local","complete":"claude"}`,
		"ai.fallback_chains":                 `{"complete":["claude","openai","ollama"],"embed":["clip_local","ollama","openai"],"transcribe":["whisper_local","openai"],"tag":["ollama","gemini","openai"],"caption":["claude","openai","ollama"]}`,
		"ai.privacy.lock_sensitive_to_local": `true`,
		"ai.privacy.local_providers":         `["ollama","vllm","whisper_local","clip_local"]`,
		"ai.budgets.default":                 `{"soft_warning_usd":0,"hard_cap_usd":0}`,
	}
	for k, v := range pairs {
		_, _ = pool.Exec(ctx,
			`INSERT INTO system_config (key, value) VALUES ($1, $2::jsonb)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
			k, v)
	}
	// Suppress slog unused-import if the file ever loses its other slog ref.
	_ = slog.LevelInfo
}
