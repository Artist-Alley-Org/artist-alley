// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
)

// #1577 production wiring: an asset edit through the real handler queues
// metadata work in its transaction (migration 00100), and the server's
// own runner records it, with no manual RunOnce: pending work on start,
// a later edit while running, nothing after the server stops, the rest
// on a fresh server. This is the local runtime, not the signed
// two-instance profile gate; the profile is still not advertised.

type metaWorkEnv struct {
	t            *testing.T
	ctx          context.Context
	pool         *pgxpool.Pool
	asset        uuid.UUID
	owner, other string // session tokens
}

const metaWorkProfiles = `["share-lifecycle-1","archival-1","original-transfer-1","asset-metadata-1"]`

func newMetaWorkEnv(t *testing.T) *metaWorkEnv {
	t.Helper()
	pool, _ := openServerPool(t)
	initAtrestForOutboxWiring(t)
	ctx := context.Background()
	e := &metaWorkEnv{t: t, ctx: ctx, pool: pool}
	prefix := "mw-" + uuid.NewString()[:8]

	// The origin the runner reads, put back afterwards.
	var site []byte
	hadSite := pool.QueryRow(ctx, `SELECT value FROM system_config WHERE key = 'site'`).Scan(&site) == nil
	e.exec(`INSERT INTO system_config (key, value) VALUES ('site', jsonb_build_object('name', 'Local', 'base_url', $1::text))
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, wiringBaseURL)
	t.Cleanup(func() {
		if hadSite {
			_, _ = pool.Exec(ctx, `UPDATE system_config SET value = $1 WHERE key = 'site'`, site)
		} else {
			_, _ = pool.Exec(ctx, `DELETE FROM system_config WHERE key = 'site'`)
		}
	})

	var ownerRef, otherRef int64
	for _, u := range []struct {
		name string
		ref  *int64
	}{{prefix + "-owner", &ownerRef}, {prefix + "-other", &otherRef}} {
		if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Metadata Work', 1) RETURNING ref`,
			u.name).Scan(u.ref); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sessions WHERE user_ref = ANY($1)`, []int64{ownerRef, otherRef})
		_, _ = pool.Exec(ctx, `DELETE FROM activities WHERE actor_user_ref = ANY($1)`, []int64{ownerRef, otherRef})
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE ref = ANY($1)`, []int64{ownerRef, otherRef})
	})
	for _, s := range []struct {
		ref   int64
		token *string
	}{{ownerRef, &e.owner}, {otherRef, &e.other}} {
		tok, _, err := auth.NewSessionManager(pool).Issue(ctx, s.ref, httptest.NewRequest(http.MethodGet, "/", nil))
		if err != nil {
			t.Fatal(err)
		}
		*s.token = tok
	}

	// A local peer inbox, so nothing leaves the test.
	inbox := &fakeInbox{}
	srv := httptest.NewServer(http.HandlerFunc(inbox.handler))
	t.Cleanup(srv.Close)
	var peer uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status,
		    handshake_by_user_ref, capabilities)
		VALUES ($1, 'receiver', 'k', TRUE, 'connected', $2, $3::jsonb) RETURNING id`, srv.URL, ownerRef, metaWorkProfiles).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM federation_peers WHERE id = $1`, peer) })

	// A public sealed asset of the owner's, granted to the peer with its
	// announcement recorded; both delivered, so its version is named.
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	hash := hex.EncodeToString(b)
	e.exec(`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 3, 'image/png', 'fs')`, hash)
	e.exec(`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'original', 3, 'image/png', $1, 3, 'write', 'verified')`, hash)
	e.asset = uuid.New()
	var version uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		VALUES ($1, 'Zero', 1, $2, $3, 3, 'public') RETURNING current_version_id`, e.asset, ownerRef, hash).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, e.asset)
		_, _ = pool.Exec(ctx, `DELETE FROM storage_pins WHERE object_hash = $1`, hash)
		_, _ = pool.Exec(ctx, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = pool.Exec(ctx, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	writer := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	share := uuid.New()
	var shareActivity, announcement uuid.UUID
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
			VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb) RETURNING id`, wiringBaseURL+"/activities/"+uuid.NewString(),
			wiringBaseURL+"/users/"+prefix+"-owner", ownerRef).Scan(&shareActivity); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original)
			VALUES ($1, $2, 'asset', $3, $4, 'view', $5, TRUE)`, share, ownerRef, e.asset, peer, shareActivity); err != nil {
			return err
		}
		var err error
		announcement, err = assetsend.Record(ctx, tx, writer, assetsend.Grant{ShareID: share, ShareActivityID: shareActivity,
			AssetID: e.asset, PeerID: peer, PeerURL: srv.URL, BaseURL: wiringBaseURL,
			Username: func(context.Context, int64) string { return prefix + "-owner" }})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if announcement == uuid.Nil {
		t.Fatal("nothing announced")
	}
	// The announcement is built (its claims recorded, as the delivery
	// worker does before sending) and both rows are sent, so the version
	// is named and a flags Update reaches the peer too.
	var objectURI string
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT object_uri, payload FROM activities WHERE id = $1`, announcement).Scan(&objectURI, &payload); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'sent')`, shareActivity, peer)
	if _, err := (assetsend.Source{Pool: pool}).Member(ctx, announcement, peer, objectURI, payload); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO federation_outbox (activity_id, peer_id, status) VALUES ($1, $2, 'sent')`, announcement, peer)
	// The ledger is shared test state: start from this test's work only.
	clear := func() { _, _ = pool.Exec(ctx, `DELETE FROM federation_metadata_work`) }
	clear()
	t.Cleanup(clear)
	return e
}

func (e *metaWorkEnv) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, q, args...); err != nil {
		e.t.Fatal(err)
	}
}

// server is a new, not yet running, server and the URL of its handler.
func (e *metaWorkEnv) server() (*Server, string) {
	e.t.Helper()
	s, err := New(config.Config{StorageBackend: "fs", StorageFSRoot: e.t.TempDir(), HTTPAddr: "127.0.0.1:0"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), e.pool, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(s.Close)
	ts := httptest.NewServer(s.srv.Handler)
	e.t.Cleanup(ts.Close)
	return s, ts.URL
}

// run starts s; stop cancels it and waits for Run to return.
func (e *metaWorkEnv) run(s *Server) (stop func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			e.t.Fatal("Run did not stop")
		}
	}
	e.t.Cleanup(stop)
	return stop
}

func (e *metaWorkEnv) patch(url, token, body string) int {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url+"/api/v1/assets/"+e.asset.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func (e *metaWorkEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// work is the asset's pending metadata work.
func (e *metaWorkEnv) work() int {
	return e.count(`SELECT count(*) FROM federation_metadata_work WHERE asset_id = $1`, e.asset)
}

// updatesTitled counts the asset's metadata Updates whose capture names title.
func (e *metaWorkEnv) updatesTitled(title string) int {
	return e.count(`SELECT count(*) FROM activities WHERE source = 'local' AND activity_type = 'Update' AND object_local_id = $1
		AND payload ? 'metadata_capture' AND (payload->>'metadata_capture')::jsonb->'snapshot'->>'title' = $2`, e.asset.String(), title)
}

// flagUpdates counts the asset's flags-only Updates (#1575).
func (e *metaWorkEnv) flagUpdates() int {
	return e.count(`SELECT count(*) FROM activities WHERE source = 'local' AND activity_type = 'Update' AND object_local_id = $1
		AND NOT payload ? 'metadata_capture'`, e.asset.String())
}

func (e *metaWorkEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("%s did not happen", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMetadataWorkProduction_EditsAreProcessedByTheServer(t *testing.T) {
	e := newMetaWorkEnv(t)

	// Not running: an authorized edit commits its work, which waits.
	s1, url1 := e.server()
	if st := e.patch(url1, e.owner, `{"title":"One"}`); st != http.StatusOK {
		t.Fatalf("PATCH by the owner: %d", st)
	}
	if e.work() != 1 || e.updatesTitled("One") != 0 {
		t.Fatalf("before Run: %d rows, %d Updates", e.work(), e.updatesTitled("One"))
	}
	// Authorization is unchanged: another user may not edit, and a refused
	// edit queues nothing.
	if st := e.patch(url1, e.other, `{"title":"Refused"}`); st != http.StatusForbidden && st != http.StatusNotFound {
		t.Fatalf("PATCH by another user: %d", st)
	}
	// A flags-only edit records its flags Update and queues no metadata work.
	flags := e.flagUpdates()
	if st := e.patch(url1, e.owner, `{"ai_provenance":"assisted"}`); st != http.StatusOK {
		t.Fatalf("flags PATCH: %d", st)
	}
	if e.work() != 1 || e.flagUpdates() != flags+1 {
		t.Fatalf("after the flags edit: %d rows, %d flags Updates (was %d)", e.work(), e.flagUpdates(), flags)
	}

	// Started: the pending edit is recorded with no manual call.
	stop1 := e.run(s1)
	e.waitFor("the pending edit recorded on start", func() bool { return e.updatesTitled("One") == 1 && e.work() == 0 })
	// Running: a combined title and flags edit records both.
	flags = e.flagUpdates()
	if st := e.patch(url1, e.owner, `{"title":"Two","ai_provenance":"generated"}`); st != http.StatusOK {
		t.Fatalf("combined PATCH: %d", st)
	}
	e.waitFor("the live edit recorded", func() bool { return e.updatesTitled("Two") == 1 && e.work() == 0 })
	if e.flagUpdates() != flags+1 {
		t.Fatalf("the combined edit recorded %d flags Updates", e.flagUpdates()-flags)
	}
	stop1()

	// Stopped: an edit (through a server that is not running) waits, for
	// longer than the runner's idle interval.
	s2, url2 := e.server()
	if st := e.patch(url2, e.owner, `{"title":"Three"}`); st != http.StatusOK {
		t.Fatalf("PATCH while stopped: %d", st)
	}
	time.Sleep(assetsend.DefaultMetadataWorkIdle + time.Second)
	if e.work() != 1 || e.updatesTitled("Three") != 0 {
		t.Fatalf("after the server stopped: %d rows, %d Updates", e.work(), e.updatesTitled("Three"))
	}
	// A fresh server resumes it.
	stop2 := e.run(s2)
	e.waitFor("the waiting edit recorded by a fresh server", func() bool { return e.updatesTitled("Three") == 1 && e.work() == 0 })
	stop2()
	if n := e.count(`SELECT count(*) FROM assets WHERE id = $1 AND title = 'Three'`, e.asset); n != 1 {
		t.Fatal("the refused edit changed the title")
	}
}
