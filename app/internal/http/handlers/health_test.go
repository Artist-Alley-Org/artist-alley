// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestHealth_OK verifies that the /healthz handler returns a 200 with
// the expected JSON shape against a real Postgres. The test skips if
// the AA_DB_PASSWORD env var is not set (i.e., running outside the
// docker-compose stack).
func TestHealth_OK(t *testing.T) {
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
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	h := &Health{
		Pool:    pool,
		Version: "test",
		Started: time.Now().Add(-30 * time.Second),
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q", ct)
	}

	var resp healthResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status=%q", resp.Status)
	}
	if resp.Database != "ok" {
		t.Errorf("database=%q", resp.Database)
	}
	if resp.Version != "test" {
		t.Errorf("version=%q", resp.Version)
	}
}

// TestHealth_DBDown verifies that with an unreachable database, the
// handler reports degraded status and returns 503.
func TestHealth_DBDown(t *testing.T) {
	// Pool against a guaranteed-unreachable address.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "host=127.0.0.1 port=1 dbname=x user=x sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	h := &Health{Pool: pool, Version: "test", Started: time.Now()}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d (expected 503)", rr.Code)
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
