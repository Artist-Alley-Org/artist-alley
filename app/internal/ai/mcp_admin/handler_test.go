// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.53.A admin handler tests. Integration tests against the
// live postgres compose stack — same cadence as mcp_registry tests
// (and the rest of the AI subsystem). The handler itself is thin
// pass-through; tests focus on (a) the auth gate, (b) the codegen
// shape mapping (request → registry params → response), and (c)
// duplicate / not-found error shapes.

package mcpadmin_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	mcpadmin "github.com/mscrnt/artist-alley/app/internal/ai/mcp_admin"
	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// ---------------------------------------------------------------------------
// Auth gate
// ---------------------------------------------------------------------------

func TestListMCPClients_RequiresAuth(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })

	h := mcpadmin.NewHandler(freshRegistry(t, pool))
	resp, err := h.ListMCPClients(context.Background(), openapi.ListMCPClientsRequestObject{})
	if err != nil {
		t.Fatalf("ListMCPClients: %v", err)
	}
	if _, ok := resp.(openapi.ListMCPClients401JSONResponse); !ok {
		t.Errorf("got %T, want ListMCPClients401JSONResponse", resp)
	}
}

func TestListMCPClients_RequiresCapability(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })

	h := mcpadmin.NewHandler(freshRegistry(t, pool))
	ctx := withIdentity(context.Background(), []string{}) // authed, no caps

	resp, err := h.ListMCPClients(ctx, openapi.ListMCPClientsRequestObject{})
	if err != nil {
		t.Fatalf("ListMCPClients: %v", err)
	}
	if _, ok := resp.(openapi.ListMCPClients403JSONResponse); !ok {
		t.Errorf("got %T, want ListMCPClients403JSONResponse", resp)
	}
}

// ---------------------------------------------------------------------------
// Register → list round-trip
// ---------------------------------------------------------------------------

func TestRegisterMCPClient_PersistsAndListsBack(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	h := mcpadmin.NewHandler(freshRegistry(t, pool))
	ctx := withIdentity(context.Background(), []string{mcpadmin.CapMCPClientAdmin})

	const name = "test_admin_register_listback"
	cleanupServer(t, pool, name)

	body := openapi.MCPServerCreate{
		Name:         name,
		Url:          "http://example.test/mcp",
		Transport:    ptr(openapi.MCPServerCreateTransportHttp),
		AuthKind:     ptr(openapi.MCPServerCreateAuthKindNone),
		PrivacyClass: ptr(openapi.MCPServerCreatePrivacyClassCloud),
	}
	created, err := h.RegisterMCPClient(ctx, openapi.RegisterMCPClientRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("RegisterMCPClient: %v", err)
	}
	ok, _ := created.(openapi.RegisterMCPClient201JSONResponse)
	if openapi.MCPServer(ok).Name != name {
		t.Fatalf("got %T = %+v, want 201 with name=%q", created, created, name)
	}

	listed, err := h.ListMCPClients(ctx, openapi.ListMCPClientsRequestObject{})
	if err != nil {
		t.Fatalf("ListMCPClients: %v", err)
	}
	list, _ := listed.(openapi.ListMCPClients200JSONResponse)
	found := false
	for _, s := range list.Items {
		if s.Name == name {
			found = true
		}
	}
	if !found {
		t.Errorf("registered server %q not in list", name)
	}
}

// ---------------------------------------------------------------------------
// Register: duplicate name → 409
// ---------------------------------------------------------------------------

func TestRegisterMCPClient_DuplicateName_Returns409(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	h := mcpadmin.NewHandler(freshRegistry(t, pool))
	ctx := withIdentity(context.Background(), []string{mcpadmin.CapMCPClientAdmin})

	const name = "test_admin_register_dup"
	cleanupServer(t, pool, name)

	body := openapi.MCPServerCreate{Name: name, Url: "http://example.test/mcp"}
	if _, err := h.RegisterMCPClient(ctx, openapi.RegisterMCPClientRequestObject{Body: &body}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	resp, err := h.RegisterMCPClient(ctx, openapi.RegisterMCPClientRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if _, ok := resp.(openapi.RegisterMCPClient409JSONResponse); !ok {
		t.Errorf("got %T, want RegisterMCPClient409JSONResponse", resp)
	}
}

// ---------------------------------------------------------------------------
// Update: missing ID → 404
// ---------------------------------------------------------------------------

func TestUpdateMCPClient_UnknownID_Returns404(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	h := mcpadmin.NewHandler(freshRegistry(t, pool))
	ctx := withIdentity(context.Background(), []string{mcpadmin.CapMCPClientAdmin})

	enabled := true
	body := openapi.MCPServerUpdate{Enabled: &enabled}
	resp, err := h.UpdateMCPClient(ctx, openapi.UpdateMCPClientRequestObject{
		Id:   uuid.MustParse("deadbeef-dead-beef-dead-beefdeadbeef"),
		Body: &body,
	})
	if err != nil {
		t.Fatalf("UpdateMCPClient: %v", err)
	}
	if _, ok := resp.(openapi.UpdateMCPClient404JSONResponse); !ok {
		t.Errorf("got %T, want UpdateMCPClient404JSONResponse", resp)
	}
}

// ---------------------------------------------------------------------------
// Tool-grant lifecycle: upsert → list → delete
// ---------------------------------------------------------------------------

func TestToolGrant_UpsertListDelete(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	h := mcpadmin.NewHandler(freshRegistry(t, pool))
	ctx := withIdentity(context.Background(), []string{mcpadmin.CapMCPClientAdmin})

	const name = "test_admin_toolgrant"
	cleanupServer(t, pool, name)

	regBody := openapi.MCPServerCreate{Name: name, Url: "http://example.test/mcp"}
	regResp, err := h.RegisterMCPClient(ctx, openapi.RegisterMCPClientRequestObject{Body: &regBody})
	if err != nil {
		t.Fatalf("RegisterMCPClient: %v", err)
	}
	server, _ := regResp.(openapi.RegisterMCPClient201JSONResponse)
	serverID := uuid.UUID(server.Id)

	addlCap := "mcp.client.images.read"
	upsertBody := openapi.MCPServerToolGrantUpsert{
		AdditionalCapability: &addlCap,
		CostEstimateMicros:   1500,
		Enabled:              true,
	}
	upsertResp, err := h.UpsertMCPClientToolGrant(ctx, openapi.UpsertMCPClientToolGrantRequestObject{
		Id: server.Id, Tool: "img2img", Body: &upsertBody,
	})
	if err != nil {
		t.Fatalf("UpsertMCPClientToolGrant: %v", err)
	}
	grant, ok := upsertResp.(openapi.UpsertMCPClientToolGrant200JSONResponse)
	if !ok {
		t.Fatalf("got %T, want UpsertMCPClientToolGrant200JSONResponse", upsertResp)
	}
	if openapi.MCPServerToolGrant(grant).ToolName != "img2img" {
		t.Errorf("ToolName = %q, want img2img", openapi.MCPServerToolGrant(grant).ToolName)
	}

	listResp, err := h.ListMCPClientToolGrants(ctx, openapi.ListMCPClientToolGrantsRequestObject{Id: server.Id})
	if err != nil {
		t.Fatalf("ListMCPClientToolGrants: %v", err)
	}
	list, _ := listResp.(openapi.ListMCPClientToolGrants200JSONResponse)
	if len(list) != 1 || list[0].ToolName != "img2img" {
		t.Errorf("list = %+v, want one img2img grant", list)
	}

	delResp, err := h.DeleteMCPClientToolGrant(ctx, openapi.DeleteMCPClientToolGrantRequestObject{
		Id: server.Id, Tool: "img2img",
	})
	if err != nil {
		t.Fatalf("DeleteMCPClientToolGrant: %v", err)
	}
	if _, ok := delResp.(openapi.DeleteMCPClientToolGrant204Response); !ok {
		t.Errorf("got %T, want DeleteMCPClientToolGrant204Response", delResp)
	}

	// Direct DB read — registry cache could mask removal.
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM mcp_server_tool_grant WHERE server_id = $1 AND tool_name = 'img2img'`, serverID).Scan(&n)
	if n != 0 {
		t.Errorf("post-delete grant count = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Delete server: 204 + cascades to grants
// ---------------------------------------------------------------------------

func TestDeleteMCPClient_Returns204(t *testing.T) {
	pool := openPool(t)
	t.Cleanup(func() { pool.Close() })
	h := mcpadmin.NewHandler(freshRegistry(t, pool))
	ctx := withIdentity(context.Background(), []string{mcpadmin.CapMCPClientAdmin})

	const name = "test_admin_delete"
	cleanupServer(t, pool, name)

	regBody := openapi.MCPServerCreate{Name: name, Url: "http://example.test/mcp"}
	regResp, err := h.RegisterMCPClient(ctx, openapi.RegisterMCPClientRequestObject{Body: &regBody})
	if err != nil {
		t.Fatalf("RegisterMCPClient: %v", err)
	}
	server, _ := regResp.(openapi.RegisterMCPClient201JSONResponse)

	delResp, err := h.DeleteMCPClient(ctx, openapi.DeleteMCPClientRequestObject{Id: server.Id})
	if err != nil {
		t.Fatalf("DeleteMCPClient: %v", err)
	}
	if _, ok := delResp.(openapi.DeleteMCPClient204Response); !ok {
		t.Errorf("got %T, want DeleteMCPClient204Response", delResp)
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
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
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
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func freshRegistry(t *testing.T, pool *pgxpool.Pool) *mcpregistry.Registry {
	t.Helper()
	return mcpregistry.NewRegistry(pool, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func cleanupServer(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM mcp_server_registration WHERE name = $1`, name)
	})
}

func withIdentity(ctx context.Context, caps []string) context.Context {
	id := &auth.Identity{UserRef: 1, AuthMethod: "session", Capabilities: caps}
	return auth.WithIdentity(ctx, id)
}

func ptr[T any](v T) *T { return &v }
