package mcpdispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	mcpdispatch "github.com/mscrnt/artist-alley/app/internal/ai/mcp_dispatch"
	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
	mcpserver "github.com/mscrnt/artist-alley/app/internal/ai/providers/mcp_server"
	"github.com/mscrnt/artist-alley/app/internal/auth"
)

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

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

// fakeServer + fakeProviderRegistry let us stand up a real registered
// server pointing at an httptest mux without wiring the full router.
type fakeProviderRegistry map[string]*mcpserver.Provider

func (f fakeProviderRegistry) Provider(name string) (*mcpserver.Provider, bool) {
	p, ok := f[name]
	return p, ok
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func cleanupServer(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM mcp_server_registration WHERE name = $1`, name)
	})
}

// seedServer registers an enabled-by-default server pointing at the
// given httptest URL and returns its ID + a freshly-built provider.
func seedServer(t *testing.T, pool *pgxpool.Pool, reg *mcpregistry.Registry, name, url, privacy string) (uuid.UUID, *mcpserver.Provider) {
	t.Helper()
	cleanupServer(t, pool, name)
	s, err := reg.Insert(context.Background(), mcpregistry.InsertParams{
		Name:                 name,
		URL:                  url,
		Transport:            "http",
		AuthKind:             "none",
		PrivacyClass:         privacy,
		Enabled:              true,
		RateLimitPerSecond:   100,
		RateLimitPerMinute:   6000,
		HealthCheckIntervalS: 60,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	prov := mcpserver.NewProvider(mcpserver.Config{
		Name: name, URL: url, AuthKind: "none", PrivacyClass: privacy,
		RateLimitPerSecond: 100,
	}, nil)
	return s.ID, prov
}

func okMCPHandler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}},
		})
	})
}

func registerToolGrant(t *testing.T, reg *mcpregistry.Registry, serverID uuid.UUID, tool, addlCap string, costMicros int64, enabled bool) {
	t.Helper()
	if _, err := reg.UpsertToolGrant(context.Background(), mcpregistry.UpsertToolGrantInput{
		ServerID:             serverID,
		ToolName:             tool,
		AdditionalCapability: addlCap,
		CostEstimateMicros:   costMicros,
		Enabled:              enabled,
	}); err != nil {
		t.Fatalf("upsert tool grant: %v", err)
	}
}

func identityWith(caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: 42, Username: "test", Capabilities: caps}
}

// ---------------------------------------------------------------------------
// Guard chain tests
// ---------------------------------------------------------------------------

func TestInvoke_HappyPath_ReturnsResult(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	mock := httptest.NewServer(okMCPHandler())
	defer mock.Close()

	id, prov := seedServer(t, pool, reg, "test_dispatch_happy", mock.URL, "cloud")
	registerToolGrant(t, reg, id, "img2img", "", 0, true)

	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_happy": prov},
		nil, nil, ai.PrivacyPolicy{}, discardLogger())
	res, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_happy",
		Tool:       "img2img",
		Caller:     identityWith("mcp.client.use"),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !contains(string(res), "ok") {
		t.Errorf("result missing tool response; got %s", res)
	}
}

func TestInvoke_ServerDisabled_Refused(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	id, prov := seedServer(t, pool, reg, "test_dispatch_disabled", "http://nowhere.test", "cloud")
	registerToolGrant(t, reg, id, "img2img", "", 0, true)
	// Flip enabled→false.
	off := false
	if _, err := reg.Update(context.Background(), mcpregistry.UpdateParams{ID: id, Enabled: &off}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_disabled": prov},
		nil, nil, ai.PrivacyPolicy{}, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_disabled",
		Tool:       "img2img",
		Caller:     identityWith("mcp.client.use"),
	})
	if !errors.Is(err, mcpdispatch.ErrServerDisabled) {
		t.Errorf("got %v, want ErrServerDisabled", err)
	}
}

func TestInvoke_NoServerCap_Refused(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	id, prov := seedServer(t, pool, reg, "test_dispatch_nocap", "http://nowhere.test", "cloud")
	registerToolGrant(t, reg, id, "img2img", "", 0, true)

	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_nocap": prov},
		nil, nil, ai.PrivacyPolicy{}, discardLogger())
	// Identity with no caps at all.
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_nocap",
		Tool:       "img2img",
		Caller:     identityWith(),
	})
	if !errors.Is(err, mcpdispatch.ErrMissingCapability) {
		t.Errorf("got %v, want ErrMissingCapability", err)
	}
}

func TestInvoke_NoToolCap_Refused(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	mock := httptest.NewServer(okMCPHandler())
	defer mock.Close()

	id, prov := seedServer(t, pool, reg, "test_dispatch_nocap_tool", mock.URL, "cloud")
	// Tool requires an extra cap (mcp.client.images.write) that
	// caller doesn't hold.
	registerToolGrant(t, reg, id, "img2img", "mcp.client.images.write", 0, true)

	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_nocap_tool": prov},
		nil, nil, ai.PrivacyPolicy{}, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_nocap_tool",
		Tool:       "img2img",
		Caller:     identityWith("mcp.client.use"), // missing .images.write
	})
	if !errors.Is(err, mcpdispatch.ErrMissingCapability) {
		t.Errorf("got %v, want ErrMissingCapability for missing tool cap", err)
	}
}

func TestInvoke_ToolNotWhitelisted_Refused(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	mock := httptest.NewServer(okMCPHandler())
	defer mock.Close()

	_, prov := seedServer(t, pool, reg, "test_dispatch_notool", mock.URL, "cloud")
	// No tool grants seeded.

	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_notool": prov},
		nil, nil, ai.PrivacyPolicy{}, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_notool",
		Tool:       "img2img",
		Caller:     identityWith("mcp.client.use"),
	})
	if !errors.Is(err, mcpdispatch.ErrToolNotWhitelisted) {
		t.Errorf("got %v, want ErrToolNotWhitelisted", err)
	}
}

func TestInvoke_ToolDisabled_Refused(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	mock := httptest.NewServer(okMCPHandler())
	defer mock.Close()

	id, prov := seedServer(t, pool, reg, "test_dispatch_tooloff", mock.URL, "cloud")
	registerToolGrant(t, reg, id, "img2img", "", 0, false) // disabled

	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_tooloff": prov},
		nil, nil, ai.PrivacyPolicy{}, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_tooloff",
		Tool:       "img2img",
		Caller:     identityWith("mcp.client.use"),
	})
	if !errors.Is(err, mcpdispatch.ErrToolDisabled) {
		t.Errorf("got %v, want ErrToolDisabled", err)
	}
}

func TestInvoke_PrivacyBlocked_CloudServer_RestrictedAsset(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	mock := httptest.NewServer(okMCPHandler())
	defer mock.Close()

	id, prov := seedServer(t, pool, reg, "test_dispatch_privacy", mock.URL, "cloud")
	registerToolGrant(t, reg, id, "img2img", "", 0, true)

	policy := ai.PrivacyPolicy{LockSensitiveToLocal: true, LocalProviders: []string{"some_local"}}
	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_privacy": prov},
		nil, nil, policy, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName:  "test_dispatch_privacy",
		Tool:        "img2img",
		Caller:      identityWith("mcp.client.use"),
		Sensitivity: ai.SensitivityRestricted,
	})
	if !errors.Is(err, mcpdispatch.ErrPrivacyBlocked) {
		t.Errorf("got %v, want ErrPrivacyBlocked", err)
	}
}

func TestInvoke_Privacy_LocalServer_RestrictedAsset_Allowed(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	mock := httptest.NewServer(okMCPHandler())
	defer mock.Close()

	id, prov := seedServer(t, pool, reg, "test_dispatch_local_privacy", mock.URL, "local")
	registerToolGrant(t, reg, id, "img2img", "", 0, true)

	policy := ai.PrivacyPolicy{LockSensitiveToLocal: true, LocalProviders: []string{"test_dispatch_local_privacy"}}
	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_local_privacy": prov},
		nil, nil, policy, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName:  "test_dispatch_local_privacy",
		Tool:        "img2img",
		Caller:      identityWith("mcp.client.use"),
		Sensitivity: ai.SensitivityRestricted,
	})
	if err != nil {
		t.Errorf("local server should accept restricted asset; got %v", err)
	}
}

// stubBudget always blocks; lets us verify the gate fires.
type stubBudget struct{}

func (stubBudget) CheckBudgetBefore(_ context.Context, provider string, est int64) error {
	return &ai.ProviderError{
		Class: ai.ErrClassBudget, Provider: provider,
		Wrapped: errors.New("ai_budget_exhausted"),
	}
}

func TestInvoke_BudgetExhausted_Refused(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	mock := httptest.NewServer(okMCPHandler())
	defer mock.Close()

	id, prov := seedServer(t, pool, reg, "test_dispatch_budget", mock.URL, "cloud")
	registerToolGrant(t, reg, id, "img2img", "", 10000, true)

	d := mcpdispatch.New(reg, fakeProviderRegistry{"test_dispatch_budget": prov},
		stubBudget{}, nil, ai.PrivacyPolicy{}, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_budget",
		Tool:       "img2img",
		Caller:     identityWith("mcp.client.use"),
	})
	if !errors.Is(err, mcpdispatch.ErrBudgetExhausted) {
		t.Errorf("got %v, want ErrBudgetExhausted", err)
	}
}

func TestInvoke_ServerNotInProviderRegistry_BootWireBug(t *testing.T) {
	pool := openPool(t)
	// Close via t.Cleanup so the cleanupServer DELETE runs against
	// an open pool (Cleanup fires AFTER defers).
	t.Cleanup(func() { pool.Close() })
	reg := mcpregistry.NewRegistry(pool, nil, discardLogger())

	id, _ := seedServer(t, pool, reg, "test_dispatch_wirebug", "http://nowhere.test", "cloud")
	registerToolGrant(t, reg, id, "img2img", "", 0, true)

	// Empty provider registry — registration exists but no runtime
	// provider; surfaces the boot-wire-bug message.
	d := mcpdispatch.New(reg, fakeProviderRegistry{}, nil, nil, ai.PrivacyPolicy{}, discardLogger())
	_, err := d.Invoke(context.Background(), mcpdispatch.InvokeOpts{
		ServerName: "test_dispatch_wirebug",
		Tool:       "img2img",
		Caller:     identityWith("mcp.client.use"),
	})
	if err == nil || !contains(err.Error(), "boot-wire bug") {
		t.Errorf("got %v, want boot-wire-bug error", err)
	}
}

// contains is a tiny helper instead of pulling in strings on each
// test file.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
