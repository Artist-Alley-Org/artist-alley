// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #711 — a registered MCP server's auth secret is write-only. It is
// never returned on any read, and an unrelated save never wipes it.
//
// Same construction as sysconfig/ai_secret_writeonly_test.go and the
// ADR 0072 PII tests: real DB, real router, assertions on the RAW
// response JSON. Decoding into openapi.MCPServer would erase the
// distinction under test (present-and-empty vs absent) and would also
// round-trip a leak straight past the assertion.
//
// Despite the `auth_secret_ref` name, the column holds the live
// bearer token / header value — the admin page's own hint text says
// so — which is why it gets the SMTP-password treatment rather than
// being treated as an opaque pointer.

package mcpadmin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	mcpadmin "github.com/mscrnt/artist-alley/app/internal/ai/mcp_admin"
	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

const (
	mcpSecretSentinel = "BEARER-SECRET-STORED-711"
	mcpSecretRotated  = "BEARER-ROTATED-711"
	mcpSecretName     = "test_711_write_only_secret"
)

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

type secretShim struct {
	*strictservershim.PanicShim
	h *mcpadmin.Handler
}

func (s secretShim) ListMCPClients(ctx context.Context, req openapi.ListMCPClientsRequestObject) (openapi.ListMCPClientsResponseObject, error) {
	return s.h.ListMCPClients(ctx, req)
}

func (s secretShim) RegisterMCPClient(ctx context.Context, req openapi.RegisterMCPClientRequestObject) (openapi.RegisterMCPClientResponseObject, error) {
	return s.h.RegisterMCPClient(ctx, req)
}

func (s secretShim) UpdateMCPClient(ctx context.Context, req openapi.UpdateMCPClientRequestObject) (openapi.UpdateMCPClientResponseObject, error) {
	return s.h.UpdateMCPClient(ctx, req)
}

func secretRouter(h *mcpadmin.Handler, caps ...string) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: 1, AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(
		openapi.NewStrictHandler(secretShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// seedServer registers one server WITH a stored secret, through the
// real create path, and returns its id. Production can reach this
// state — an operator typing a bearer token into the register form is
// exactly how it arises.
func seedServer(t *testing.T, pool *pgxpool.Pool, registry *mcpregistry.Registry) string {
	t.Helper()
	cleanupServer(t, pool, mcpSecretName)
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM mcp_server_registration WHERE name = $1`, mcpSecretName)

	created, err := registry.Insert(context.Background(), mcpregistry.InsertParams{
		Name:                 mcpSecretName,
		URL:                  "http://example.test/mcp",
		Transport:            "http",
		AuthKind:             "bearer",
		AuthSecretRef:        mcpSecretSentinel,
		PrivacyClass:         "cloud",
		RateLimitPerSecond:   2,
		RateLimitPerMinute:   60,
		HealthCheckIntervalS: 60,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	return created.ID.String()
}

func storedSecret(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var out *string
	if err := pool.QueryRow(context.Background(),
		`SELECT auth_secret_ref FROM mcp_server_registration WHERE name = $1`,
		mcpSecretName).Scan(&out); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if out == nil {
		return ""
	}
	return *out
}

// rawServer pulls the seeded registration out of the list response as
// a RAW map, so the assertion sees exactly what a client sees.
func rawServer(t *testing.T, router chi.Router, id string) (string, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/ai/mcp-clients", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/ai/mcp-clients = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	for _, it := range decoded.Items {
		if it["id"] == id {
			return body, it
		}
	}
	t.Fatalf("server %s missing from list: %s", id, body)
	return "", nil
}

func patchServer(t *testing.T, router chi.Router, id string, body openapi.MCPServerUpdate) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/admin/ai/mcp-clients/"+id, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH /admin/ai/mcp-clients/%s = %d, body=%s", id, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// ---------------------------------------------------------------------------
// The invariant
// ---------------------------------------------------------------------------

func TestListMCPClients_NeverReturnsStoredSecret(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	registry := freshRegistry(t, pool)
	h := mcpadmin.NewHandler(registry)
	id := seedServer(t, pool, registry)

	// mcp.client.admin is the only cap on this surface, and even it
	// does not read the secret back. system.admin is added to prove
	// it is not an exemption either.
	router := secretRouter(h, mcpadmin.CapMCPClientAdmin, auth.SuperAdminCapability)
	body, row := rawServer(t, router, id)

	if strings.Contains(body, mcpSecretSentinel) {
		t.Fatalf("stored auth secret returned in the list response: %s", body)
	}
	if _, present := row["auth_secret_ref"]; present {
		t.Errorf("auth_secret_ref present in the response (omitted, not blanked): %v", row)
	}
	if set, _ := row["auth_secret_set"].(bool); !set {
		t.Errorf("auth_secret_set should be true when a secret is stored: %v", row)
	}
	// Field-level, not row-level: everything else still comes back.
	if row["url"] != "http://example.test/mcp" || row["auth_kind"] != "bearer" {
		t.Errorf("non-secret fields dropped: %v", row)
	}
}

func TestRegisterMCPClient_ResponseNeverEchoesSecret(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	registry := freshRegistry(t, pool)
	h := mcpadmin.NewHandler(registry)
	cleanupServer(t, pool, mcpSecretName)
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM mcp_server_registration WHERE name = $1`, mcpSecretName)

	router := secretRouter(h, mcpadmin.CapMCPClientAdmin)
	create := openapi.MCPServerCreate{
		Name:          mcpSecretName,
		Url:           "http://example.test/mcp",
		AuthKind:      ptr(openapi.MCPServerCreateAuthKindBearer),
		AuthSecretRef: ptr(mcpSecretSentinel),
	}
	raw, err := json.Marshal(create)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/ai/mcp-clients", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST = %d, body=%s", rr.Code, rr.Body.String())
	}
	// The operator just typed it, but the 201 body still must not
	// carry it — this response is as loggable as any other.
	if strings.Contains(rr.Body.String(), mcpSecretSentinel) {
		t.Errorf("register response echoed the secret: %s", rr.Body.String())
	}
	if got := storedSecret(t, pool); got != mcpSecretSentinel {
		t.Errorf("secret not persisted: got %q", got)
	}
}

// #708's shape. The edit form can no longer pre-fill the field, so an
// ordinary "flip enabled" save arrives with auth_secret_ref absent or
// empty. Neither may clear the stored credential.
func TestUpdateMCPClient_UnrelatedFieldSaveKeepsSecret(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	registry := freshRegistry(t, pool)
	h := mcpadmin.NewHandler(registry)
	id := seedServer(t, pool, registry)
	router := secretRouter(h, mcpadmin.CapMCPClientAdmin)

	// Omitted entirely.
	respBody := patchServer(t, router, id, openapi.MCPServerUpdate{Enabled: ptr(true)})
	if strings.Contains(respBody, mcpSecretSentinel) {
		t.Errorf("PATCH response echoed the secret: %s", respBody)
	}
	if got := storedSecret(t, pool); got != mcpSecretSentinel {
		t.Errorf("enabled-only save wiped the secret: got %q, want %q", got, mcpSecretSentinel)
	}

	// Present but empty — what a form bound to a blank input posts.
	patchServer(t, router, id, openapi.MCPServerUpdate{
		Url:           ptr("http://moved.test/mcp"),
		AuthSecretRef: ptr(""),
	})
	if got := storedSecret(t, pool); got != mcpSecretSentinel {
		t.Errorf("empty-string auth_secret_ref wiped the secret: got %q", got)
	}
	// The non-secret edit still applied — "keep the secret" must not
	// degrade into "ignore the request".
	_, row := rawServer(t, router, id)
	if row["url"] != "http://moved.test/mcp" {
		t.Errorf("url edit did not apply: %v", row)
	}
}

func TestUpdateMCPClient_NewSecretReplacesStored(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	registry := freshRegistry(t, pool)
	h := mcpadmin.NewHandler(registry)
	id := seedServer(t, pool, registry)
	router := secretRouter(h, mcpadmin.CapMCPClientAdmin)

	patchServer(t, router, id, openapi.MCPServerUpdate{AuthSecretRef: ptr(mcpSecretRotated)})
	if got := storedSecret(t, pool); got != mcpSecretRotated {
		t.Errorf("rotation did not apply: got %q, want %q", got, mcpSecretRotated)
	}
}

func TestListMCPClients_AuthSecretSetFalseWhenUnset(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	registry := freshRegistry(t, pool)
	h := mcpadmin.NewHandler(registry)
	cleanupServer(t, pool, mcpSecretName)
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM mcp_server_registration WHERE name = $1`, mcpSecretName)

	created, err := registry.Insert(context.Background(), mcpregistry.InsertParams{
		Name: mcpSecretName, URL: "http://example.test/mcp",
		Transport: "http", AuthKind: "none", PrivacyClass: "local",
		RateLimitPerSecond: 2, RateLimitPerMinute: 60, HealthCheckIntervalS: 60,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	router := secretRouter(h, mcpadmin.CapMCPClientAdmin)
	_, row := rawServer(t, router, created.ID.String())
	set, ok := row["auth_secret_set"].(bool)
	if !ok {
		t.Fatalf("auth_secret_set missing: %v", row)
	}
	if set {
		t.Errorf("auth_secret_set true with no secret stored: %v", row)
	}
}
