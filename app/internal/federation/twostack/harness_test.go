// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package twostack_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
	apphttp "github.com/mscrnt/artist-alley/app/internal/http"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/fs"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// databases returns the two dedicated database names, skipping without
// a test database and FAILING when either dedicated name is missing,
// when they are equal, or when either is the general suite database.
//
// Both instances run on their OWN disposable databases (AA_DB_NAME_A,
// AA_DB_NAME_B), never on AA_DB_NAME: the gate creates peers, users,
// grants, assets and storage rows that the rest of the suite must never
// see, and the instances share nothing but the process.
func databases(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("AA_DB_PASSWORD") == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	// The general suite database, resolved exactly as every other
	// integration test resolves it (same safe default, same refusal of a
	// non-disposable name).
	general := testdb.Name(t)
	a := os.Getenv("AA_DB_NAME_A")
	b := os.Getenv("AA_DB_NAME_B")
	// Every check runs before any connection or migration: the gate
	// migrates both databases and writes to them, so a hand-set name
	// pointing at a real database (the dev database, say) must stop here.
	switch {
	case a == "" || b == "":
		t.Fatal("AA_DB_NAME_A and AA_DB_NAME_B must both be set: the two-instance gate needs two dedicated migrated databases (see scripts/test.sh)")
	case len(a) > maxIdentifierBytes || len(b) > maxIdentifierBytes:
		t.Fatalf("AA_DB_NAME_A (%d bytes) or AA_DB_NAME_B (%d bytes) exceeds Postgres' %d-byte identifier limit; a truncated name could collide with another database",
			len(a), len(b), maxIdentifierBytes)
	case !testdb.IsDisposable(a) || !testdb.IsDisposable(b):
		t.Fatalf("refusing to run the two-instance gate on AA_DB_NAME_A=%q, AA_DB_NAME_B=%q: both must be disposable test databases with a %q segment (see scripts/test.sh)",
			a, b, "_test")
	case a == b:
		t.Fatalf("AA_DB_NAME_A and AA_DB_NAME_B name the same database %q", a)
	case a == general || b == general:
		t.Fatalf("the two-instance gate must not run on the general test database %q (AA_DB_NAME_A=%q, AA_DB_NAME_B=%q)", general, a, b)
	}
	return a, b
}

// maxIdentifierBytes is Postgres' identifier limit (NAMEDATALEN - 1).
const maxIdentifierBytes = 63

func openPool(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"),
		os.Getenv("AA_DB_PASSWORD"), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var atrestOnce sync.Once

func initAtrest(t *testing.T) {
	t.Helper()
	atrestOnce.Do(func() {
		if atrest.Initialised() {
			return
		}
		key := make([]byte, atrest.MasterKeyLen)
		_, _ = rand.Read(key)
		if err := atrest.InitWithKey(key); err != nil {
			t.Fatalf("atrest: %v", err)
		}
	})
}

// instance is one production server on its own database and storage.
type instance struct {
	t     *testing.T
	name  string
	pool  *pgxpool.Pool
	srv   *apphttp.Server
	ts    *httptest.Server
	url   string
	root  string
	token string // admin bearer token (system.admin)
	admin int64

	handler atomic.Pointer[http.Handler]
	front   atomic.Pointer[func(http.ResponseWriter, *http.Request, http.Handler)]

	casGETs  atomic.Int32
	casHEADs atomic.Int32
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := in.handler.Load()
	if h == nil {
		http.Error(w, "booting", http.StatusServiceUnavailable)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/federation/assets/") {
		switch r.Method {
		case http.MethodGet:
			in.casGETs.Add(1)
		case http.MethodHead:
			in.casHEADs.Add(1)
		}
		if f := in.front.Load(); f != nil {
			(*f)(w, r, *h)
			return
		}
	}
	(*h).ServeHTTP(w, r)
}

func (in *instance) setFront(f func(http.ResponseWriter, *http.Request, http.Handler)) {
	if f == nil {
		in.front.Store(nil)
		return
	}
	in.front.Store(&f)
}

type cluster struct {
	t      *testing.T
	a, b   *instance
	logger *slog.Logger
	tr     *http.Transport // trusts the httptest certificate
}

// newCluster boots A and B, each on its own database and storage root,
// and pairs them through the real handshake (B offers, A's admin
// accepts).
func newCluster(t *testing.T) *cluster {
	t.Helper()
	dbA, dbB := databases(t)
	initAtrest(t)
	c := &cluster{t: t, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.a = c.boot("A", dbA)
	c.b = c.boot("B", dbB)
	if c.a.url == c.b.url {
		t.Fatal("instances share a URL")
	}
	c.pair()
	return c
}

func (c *cluster) boot(name, dbName string) *instance {
	t := c.t
	t.Helper()
	ctx := context.Background()
	port, _ := strconv.Atoi(env("AA_DB_PORT", "5432"))
	if err := db.Migrate(ctx, config.Config{
		DBHost: env("AA_DB_HOST", "postgres"), DBPort: port, DBUser: env("AA_DB_USER", "artist_alley"),
		DBPassword: os.Getenv("AA_DB_PASSWORD"), DBName: dbName, DBSSLMode: "disable",
	}); err != nil {
		t.Fatalf("%s: migrate: %v", name, err)
	}
	pool := openPool(t, dbName)
	in := &instance{t: t, name: name, pool: pool, root: t.TempDir()}
	in.ts = httptest.NewUnstartedServer(in)
	in.ts.StartTLS()
	t.Cleanup(in.ts.Close)
	in.url = in.ts.URL
	if c.tr == nil {
		roots := x509.NewCertPool()
		roots.AddCert(in.ts.Certificate())
		c.tr = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
		// The production handshake engine and actor-key fetcher use the
		// default transport: trust the same certificate there.
		dt := http.DefaultTransport.(*http.Transport)
		prev := dt.TLSClientConfig
		dt.TLSClientConfig = &tls.Config{RootCAs: roots}
		t.Cleanup(func() { dt.CloseIdleConnections(); dt.TLSClientConfig = prev })
	}

	// Each database is its own instance: its own base URL, identity,
	// peers, users and storage rows. Earlier runs' peer rows for these
	// ephemeral URLs are irrelevant (fresh ports), but stale rows from
	// a previous gate run on the same database are removed.
	if err := sysconfig.NewStore(pool).SetSite(ctx, sysconfig.Site{Name: "Stack " + name, BaseURL: in.url}); err != nil {
		t.Fatalf("%s: site: %v", name, err)
	}
	cfg := config.Config{StorageBackend: "fs", StorageFSRoot: in.root, HTTPAddr: "127.0.0.1:0"}
	s, err := apphttp.New(cfg, c.logger, pool, "twostack")
	if err != nil {
		t.Fatalf("%s: New: %v", name, err)
	}
	t.Cleanup(s.Close)
	in.srv = s
	h := s.Handler()
	in.handler.Store(&h)

	// An admin with an API token, for the real admin API.
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Gate Admin', 1) RETURNING ref`,
		"gate-"+strings.ToLower(name)+"-"+uuid.NewString()[:8]).Scan(&in.admin); err != nil {
		t.Fatalf("%s: admin: %v", name, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_capability_grants (user_ref, capability_code) VALUES ($1, 'system.admin')`, in.admin); err != nil {
		t.Fatalf("%s: grant admin: %v", name, err)
	}
	tok, _ := auth.NewAPIToken()
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_tokens (user_ref, name, token_hash) VALUES ($1, 'twostack gate', $2)`,
		in.admin, auth.HashAPIToken(tok)); err != nil {
		t.Fatalf("%s: token: %v", name, err)
	}
	in.token = tok
	return in
}

// api calls the instance's real admin API as its admin.
func (c *cluster) api(in *instance, method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, in.url+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+in.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: c.tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		c.t.Fatalf("%s %s %s: %v", in.name, method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	if resp.StatusCode >= 400 && out == nil {
		c.t.Logf("%s %s %s -> %d %s", in.name, method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode
}

// pair runs the production handshake: B initiates, A's admin accepts.
func (c *cluster) pair() {
	t := c.t
	t.Helper()
	if st := c.api(c.b, http.MethodPost, "/admin/federation/peers/initiate",
		map[string]string{"instance_url": c.a.url}, nil); st >= 300 {
		t.Fatalf("B initiate: %d", st)
	}
	var pending struct {
		Items []struct {
			ID          string `json:"id"`
			InstanceURL string `json:"instance_url"`
		} `json:"items"`
	}
	c.api(c.a, http.MethodGet, "/admin/federation/peers/pending-inbound", nil, &pending)
	id := ""
	for _, p := range pending.Items {
		if p.InstanceURL == c.b.url {
			id = p.ID
		}
	}
	if id == "" {
		t.Fatalf("A has no pending request from B (%+v)", pending)
	}
	if st := c.api(c.a, http.MethodPost, "/admin/federation/peers/"+id+"/accept", nil, nil); st >= 300 {
		t.Fatalf("A accept: %d", st)
	}
	for _, side := range []struct{ in, other *instance }{{c.a, c.b}, {c.b, c.a}} {
		var status string
		var caps []string
		if err := side.in.pool.QueryRow(context.Background(),
			`SELECT status, ARRAY(SELECT jsonb_array_elements_text(capabilities)) FROM federation_peers WHERE instance_url = $1`,
			side.other.url).Scan(&status, &caps); err != nil {
			t.Fatalf("%s: peer row for %s: %v", side.in.name, side.other.name, err)
		}
		if status != "connected" {
			t.Fatalf("%s sees %s as %q", side.in.name, side.other.name, status)
		}
	}
}

// peerID is in's row id for other.
func (c *cluster) peerID(in, other *instance) uuid.UUID {
	c.t.Helper()
	var id uuid.UUID
	if err := in.pool.QueryRow(context.Background(),
		`SELECT id FROM federation_peers WHERE instance_url = $1`, other.url).Scan(&id); err != nil {
		c.t.Fatal(err)
	}
	return id
}

// storageOf opens in's storage service (same root and database the
// server uses).
func (c *cluster) storageOf(in *instance) *storage.Service {
	be, err := fs.New(in.root)
	if err != nil {
		c.t.Fatal(err)
	}
	return storage.NewService(be, in.pool)
}

func (c *cluster) capabilities(in, other *instance) []string {
	var caps []string
	_ = in.pool.QueryRow(context.Background(),
		`SELECT ARRAY(SELECT jsonb_array_elements_text(capabilities)) FROM federation_peers WHERE instance_url = $1`,
		other.url).Scan(&caps)
	return caps
}
