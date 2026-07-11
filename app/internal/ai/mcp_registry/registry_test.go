// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package mcpregistry_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
)

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

func freshRegistry(t *testing.T, pool *pgxpool.Pool) *mcpregistry.Registry {
	return mcpregistry.NewRegistry(pool, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func cleanupServer(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM mcp_server_registration WHERE name = $1`, name)
	})
}

func sampleInsert(name string) mcpregistry.InsertParams {
	return mcpregistry.InsertParams{
		Name:                 name,
		URL:                  "http://example.test/mcp",
		Transport:            "http",
		AuthKind:             "none",
		PrivacyClass:         "cloud",
		Enabled:              false,
		RateLimitPerSecond:   2,
		RateLimitPerMinute:   60,
		HealthCheckIntervalS: 60,
	}
}

func TestRegistry_Insert_PersistsAndReadsBack(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so closing the pool inside the
	// Cleanup (instead of defer) keeps cleanupServer's DELETE alive
	// long enough to actually run. Otherwise the pool is closed
	// first and the DELETE silently no-ops, leaving stale rows that
	// trip the duplicate-name guard on the next run.
	t.Cleanup(func() { pool.Close() })
	r := freshRegistry(t, pool)

	const name = "test_registry_insert"
	cleanupServer(t, pool, name)

	created, err := r.Insert(context.Background(), sampleInsert(name))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if created.Name != name {
		t.Errorf("created.Name = %q", created.Name)
	}
	if created.ID.String() == "00000000-0000-0000-0000-000000000000" {
		t.Errorf("ID not populated: %v", created.ID)
	}

	got, err := r.GetServerByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetServerByID: %v", err)
	}
	if got.Name != name {
		t.Errorf("read-back name = %q", got.Name)
	}
	if got.PrivacyClass != "cloud" {
		t.Errorf("read-back privacy = %q", got.PrivacyClass)
	}
	if got.Enabled {
		t.Errorf("read-back enabled should be false (default)")
	}
}

func TestRegistry_Insert_DuplicateName_ReturnsSentinel(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so closing the pool inside the
	// Cleanup (instead of defer) keeps cleanupServer's DELETE alive
	// long enough to actually run. Otherwise the pool is closed
	// first and the DELETE silently no-ops, leaving stale rows that
	// trip the duplicate-name guard on the next run.
	t.Cleanup(func() { pool.Close() })
	r := freshRegistry(t, pool)

	const name = "test_registry_duplicate"
	cleanupServer(t, pool, name)

	if _, err := r.Insert(context.Background(), sampleInsert(name)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := r.Insert(context.Background(), sampleInsert(name))
	if !errors.Is(err, mcpregistry.ErrDuplicateName) {
		t.Errorf("got %v, want ErrDuplicateName", err)
	}
}

func TestRegistry_GetServerByID_NotFound_ReturnsSentinel(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so closing the pool inside the
	// Cleanup (instead of defer) keeps cleanupServer's DELETE alive
	// long enough to actually run. Otherwise the pool is closed
	// first and the DELETE silently no-ops, leaving stale rows that
	// trip the duplicate-name guard on the next run.
	t.Cleanup(func() { pool.Close() })
	r := freshRegistry(t, pool)

	missing := mustParseUUID(t, "deadbeef-dead-beef-dead-beefdeadbeef")
	_, err := r.GetServerByID(context.Background(), missing)
	if !errors.Is(err, mcpregistry.ErrServerNotFound) {
		t.Errorf("got %v, want ErrServerNotFound", err)
	}
}

func TestRegistry_Update_PartialFields(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so closing the pool inside the
	// Cleanup (instead of defer) keeps cleanupServer's DELETE alive
	// long enough to actually run. Otherwise the pool is closed
	// first and the DELETE silently no-ops, leaving stale rows that
	// trip the duplicate-name guard on the next run.
	t.Cleanup(func() { pool.Close() })
	r := freshRegistry(t, pool)

	const name = "test_registry_update"
	cleanupServer(t, pool, name)

	created, err := r.Insert(context.Background(), sampleInsert(name))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	enabled := true
	newURL := "http://new.test/mcp"
	updated, err := r.Update(context.Background(), mcpregistry.UpdateParams{
		ID:      created.ID,
		Enabled: &enabled,
		URL:     &newURL,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !updated.Enabled {
		t.Errorf("Enabled not updated")
	}
	if updated.URL != newURL {
		t.Errorf("URL = %q, want %q", updated.URL, newURL)
	}
	// Untouched fields preserved.
	if updated.PrivacyClass != created.PrivacyClass {
		t.Errorf("PrivacyClass mutated unexpectedly: %q vs %q",
			updated.PrivacyClass, created.PrivacyClass)
	}
}

func TestRegistry_UpdateHealthStatus_SetsAndClears(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so closing the pool inside the
	// Cleanup (instead of defer) keeps cleanupServer's DELETE alive
	// long enough to actually run. Otherwise the pool is closed
	// first and the DELETE silently no-ops, leaving stale rows that
	// trip the duplicate-name guard on the next run.
	t.Cleanup(func() { pool.Close() })
	r := freshRegistry(t, pool)

	const name = "test_registry_health"
	cleanupServer(t, pool, name)

	created, err := r.Insert(context.Background(), sampleInsert(name))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// First poll: unreachable.
	if err := r.UpdateHealthStatus(context.Background(), created.ID, "unreachable", "connection refused"); err != nil {
		t.Fatalf("UpdateHealthStatus(unreachable): %v", err)
	}
	s, _ := r.GetServerByID(context.Background(), created.ID)
	if s.LastHealthStatus != "unreachable" || s.LastHealthError != "connection refused" {
		t.Errorf("status/error not stored: status=%q err=%q", s.LastHealthStatus, s.LastHealthError)
	}

	// Recovery: clears error.
	if err := r.UpdateHealthStatus(context.Background(), created.ID, "healthy", ""); err != nil {
		t.Fatalf("UpdateHealthStatus(healthy): %v", err)
	}
	s, _ = r.GetServerByID(context.Background(), created.ID)
	if s.LastHealthStatus != "healthy" || s.LastHealthError != "" {
		t.Errorf("after recovery: status=%q err=%q", s.LastHealthStatus, s.LastHealthError)
	}
}

func TestRegistry_Delete_CascadesToToolGrants(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so closing the pool inside the
	// Cleanup (instead of defer) keeps cleanupServer's DELETE alive
	// long enough to actually run. Otherwise the pool is closed
	// first and the DELETE silently no-ops, leaving stale rows that
	// trip the duplicate-name guard on the next run.
	t.Cleanup(func() { pool.Close() })
	r := freshRegistry(t, pool)

	const name = "test_registry_delete_cascade"
	cleanupServer(t, pool, name)

	created, err := r.Insert(context.Background(), sampleInsert(name))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := r.UpsertToolGrant(context.Background(), mcpregistry.UpsertToolGrantInput{
		ServerID:           created.ID,
		ToolName:           "img2img",
		CostEstimateMicros: 1000,
		Enabled:            true,
	}); err != nil {
		t.Fatalf("UpsertToolGrant: %v", err)
	}
	grants, _ := r.ListToolGrants(context.Background(), created.ID)
	if len(grants) != 1 {
		t.Fatalf("pre-delete tool grants = %d, want 1", len(grants))
	}

	if err := r.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Direct DB read — registry's cache would return stale data.
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM mcp_server_tool_grant WHERE server_id = $1`, created.ID).Scan(&n)
	if n != 0 {
		t.Errorf("post-delete tool grants = %d, want 0 (cascade should drop)", n)
	}
}

func TestRegistry_ListEnabledServers_FiltersByFlag(t *testing.T) {
	pool := openPool(t)
	// t.Cleanup runs AFTER defers, so closing the pool inside the
	// Cleanup (instead of defer) keeps cleanupServer's DELETE alive
	// long enough to actually run. Otherwise the pool is closed
	// first and the DELETE silently no-ops, leaving stale rows that
	// trip the duplicate-name guard on the next run.
	t.Cleanup(func() { pool.Close() })
	r := freshRegistry(t, pool)

	const enabledName = "test_registry_list_enabled_on"
	const disabledName = "test_registry_list_enabled_off"
	cleanupServer(t, pool, enabledName)
	cleanupServer(t, pool, disabledName)

	on := sampleInsert(enabledName)
	on.Enabled = true
	if _, err := r.Insert(context.Background(), on); err != nil {
		t.Fatalf("Insert(on): %v", err)
	}
	if _, err := r.Insert(context.Background(), sampleInsert(disabledName)); err != nil {
		t.Fatalf("Insert(off): %v", err)
	}

	got, err := r.ListEnabledServers(context.Background())
	if err != nil {
		t.Fatalf("ListEnabledServers: %v", err)
	}
	found := map[string]bool{}
	for _, s := range got {
		found[s.Name] = true
	}
	if !found[enabledName] {
		t.Errorf("enabled server not in results: %+v", found)
	}
	if found[disabledName] {
		t.Errorf("disabled server leaked into ListEnabledServers: %+v", found)
	}
}

// mustParseUUID helper.
func mustParseUUID(t *testing.T, s string) [16]byte {
	t.Helper()
	id, err := uuidParse(s)
	if err != nil {
		t.Fatalf("parse uuid: %v", err)
	}
	return id
}

func uuidParse(s string) ([16]byte, error) {
	// Hand-rolled to avoid pulling the uuid package just for one
	// constant in tests. Format: 8-4-4-4-12 hex.
	var out [16]byte
	hexIdx := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' {
			continue
		}
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			return out, errInvalidUUID
		}
		if hexIdx%2 == 0 {
			out[hexIdx/2] = v << 4
		} else {
			out[hexIdx/2] |= v
		}
		hexIdx++
	}
	if hexIdx != 32 {
		return out, errInvalidUUID
	}
	return out, nil
}

var errInvalidUUID = errInvalid("invalid uuid")

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
