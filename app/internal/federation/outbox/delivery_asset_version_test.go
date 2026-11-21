// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package outbox_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
)

// #1574 sender through the real delivery worker: an aa:AssetVersion is
// held back until its grant's aa:Share was sent to the peer, then sent
// once, plaintext, signed, with the aa:asset built from the sealed
// version; a grant revoked or expired before that, or an asset no longer
// public or team, is never announced.

const avLocal = "https://local.example"

type avStub struct {
	mu   sync.Mutex
	got  []json.RawMessage // every envelope accepted, in arrival order
	srv  *httptest.Server
	hits int
	// unavailable answers a singleton POST of this activity type 503.
	unavailable string
}

func newAVStub(t *testing.T) *avStub {
	s := &avStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.hits++
		switch r.URL.Path {
		case "/federation/inbox/batch":
			var batch struct {
				Envelopes []json.RawMessage `json:"envelopes"`
			}
			_ = json.Unmarshal(body, &batch)
			type result struct {
				ActivityURI string `json:"activity_uri"`
				Status      string `json:"status"`
			}
			var out struct {
				Results []result `json:"results"`
			}
			for _, e := range batch.Envelopes {
				s.got = append(s.got, e)
				var id struct{ ID string }
				_ = json.Unmarshal(e, &id)
				out.Results = append(out.Results, result{ActivityURI: id.ID, Status: "accepted"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case "/federation/inbox":
			var v struct{ Type string }
			_ = json.Unmarshal(body, &v)
			if s.unavailable != "" && v.Type == s.unavailable {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			s.got = append(s.got, body)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// types lists the received envelopes' types.
func (s *avStub) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.got {
		var v struct{ Type string }
		_ = json.Unmarshal(e, &v)
		out = append(out, v.Type)
	}
	return out
}

func (s *avStub) last() map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m map[string]json.RawMessage
	_ = json.Unmarshal(s.got[len(s.got)-1], &m)
	return m
}

type avFx struct {
	t      *testing.T
	pool   *pgxpool.Pool
	stub   *avStub
	owner  int64
	peer   uuid.UUID
	worker *outbox.Worker
	writer *activities.Writer
}

func newAVFx(t *testing.T) *avFx {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()
	f := &avFx{t: t, pool: pool, stub: newAVStub(t),
		writer: activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)}
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Alice', 1) RETURNING ref`,
		"av-"+randHex(4)).Scan(&f.owner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, trust_tier,
		    encryption_policy, enabled, status, handshake_by_user_ref, capabilities)
		VALUES ($1, 'AV peer', '', 'connected', 'plaintext', TRUE, 'connected', $2, $3::jsonb) RETURNING id`,
		f.stub.srv.URL, f.owner, bothAssetProfiles).Scan(&f.peer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, f.peer)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, f.owner)
	})
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.worker = outbox.NewWorker(outbox.DeliveryConfig{Interval: time.Hour, BatchSize: 100, RequestTimeout: 5 * time.Second},
		pool, &outbox.IdentitySigner{PrivateKey: priv, KeyURL: avLocal + "/federation/instance#main-key"},
		func(_ context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			return outbox.PeerInfo{ID: id, InstanceURL: f.stub.srv.URL, Enabled: true, Connected: true}, nil
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wireTestActorSigner(t, f.worker)
	f.worker.SetAssetVersionMember(assetsend.Source{Pool: pool}.Member)
	f.worker.SetAssetWithdrawalMember(assetsend.Source{Pool: pool}.Tombstone)
	return f
}

type avAsset struct {
	id, version uuid.UUID
	hash        string
}

func (f *avFx) newAsset(tier string, sealed bool) avAsset {
	f.t.Helper()
	ctx := context.Background()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	a := avAsset{id: uuid.New(), hash: hex.EncodeToString(b)}
	for _, q := range []string{
		`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 3, 'image/png', 'fs')`,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'original', 3, 'image/png', $1, 3, 'write', 'verified')`,
	} {
		if _, err := f.pool.Exec(ctx, q, a.hash); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		VALUES ($1, 'AV', 1, $2, $3, 3, $4) RETURNING current_version_id`, a.id, f.owner, a.hash, tier).Scan(&a.version); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, a.id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, a.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, a.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, a.hash)
	})
	if sealed {
		if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, a.version)
			return err
		}); err != nil {
			f.t.Fatal(err)
		}
	}
	return a
}

type avGrant struct {
	share, shareActivity, announcement uuid.UUID
	announcementURI                    string
}

// grant writes an original-file share of a with its aa:Share and the
// announcement, as the grant handler does, and queues both for the peer
// as the dispatcher does (the aa:Share's row due at once when shareDue).
func (f *avFx) grant(a avAsset, shareDue bool) avGrant {
	f.t.Helper()
	ctx := context.Background()
	g := avGrant{share: uuid.New()}
	assetURI := avLocal + "/assets/" + a.id.String()
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		payload, _ := json.Marshal(map[string]any{"share_id": g.share.String(), "object_kind": "asset", "object_id": a.id.String(),
			"object_uri": assetURI, "scope": "view", "peer_url": "https://receiver.example", "allow_original": true})
		if err := tx.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, object_uri,
			    object_kind, object_local_id, payload)
			VALUES ($1, 'aa:Share', $2, $3, $4, 'asset', $5, $6) RETURNING id`, avLocal+"/activities/"+uuid.NewString(),
			avLocal+"/users/alice", f.owner, assetURI, a.id.String(), payload).Scan(&g.shareActivity); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id, scope,
			    granted_activity_id, allow_original) VALUES ($1, $2, 'asset', $3, $4, 'view', $5, TRUE)`,
			g.share, f.owner, a.id, f.peer, g.shareActivity); err != nil {
			return err
		}
		id, err := assetsend.Record(ctx, tx, f.writer, assetsend.Grant{ShareID: g.share, ShareActivityID: g.shareActivity,
			AssetID: a.id, PeerID: f.peer, PeerURL: f.stub.srv.URL, BaseURL: avLocal,
			Username: func(context.Context, int64) string { return "alice" }})
		g.announcement = id
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	if g.announcement == uuid.Nil {
		f.t.Fatal("nothing announced")
	}
	if err := f.pool.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE id = $1`, g.announcement).Scan(&g.announcementURI); err != nil {
		f.t.Fatal(err)
	}
	due := "now()"
	if !shareDue {
		due = "now() + interval '1 hour'"
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO federation_outbox (activity_id, peer_id, sensitivity, next_attempt_at)
		VALUES ($1, $2, 'public', `+due+`), ($3, $2, 'public', now())`, g.shareActivity, f.peer, g.announcement); err != nil {
		f.t.Fatal(err)
	}
	return g
}

func (f *avFx) row(activity uuid.UUID) (status string, attempts int, due bool) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT status, attempts, next_attempt_at <= now() FROM federation_outbox
		WHERE activity_id = $1 AND peer_id = $2`, activity, f.peer).Scan(&status, &attempts, &due); err != nil {
		f.t.Fatal(err)
	}
	return status, attempts, due
}

func TestDelivery_AssetVersion_AfterItsShareThenOnce(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	a := f.newAsset("public", true)
	g := f.grant(a, true)

	// One round: the aa:Share goes; the announcement, built while its
	// aa:Share was still queued, is held back, then made due by the send.
	f.worker.RunOnce(ctx)
	if got := f.stub.types(); len(got) != 1 || got[0] != string(federation.ActivityAAShare) {
		t.Fatalf("first round sent %v, want the aa:Share only", got)
	}
	if st, attempts, due := f.row(g.announcement); st != "queued" || attempts != 1 || !due {
		t.Fatalf("announcement after the first round: %s, %d attempts, due %v", st, attempts, due)
	}
	f.worker.RunOnce(ctx)
	if got := f.stub.types(); len(got) != 2 || got[1] != string(federation.ActivityAAAssetVersion) {
		t.Fatalf("second round: %v", got)
	}
	if st, _, _ := f.row(g.announcement); st != "sent" {
		t.Fatalf("announcement %s", st)
	}
	env := f.stub.last()
	var id, actor, object string
	_ = json.Unmarshal(env["id"], &id)
	_ = json.Unmarshal(env["actor"], &actor)
	_ = json.Unmarshal(env["object"], &object)
	if id != g.announcementURI || actor != avLocal+"/users/alice" || object != avLocal+"/assets/"+a.id.String() {
		t.Fatalf("envelope id %s actor %s object %s", id, actor, object)
	}
	if _, sealed := env["encryption"]; sealed {
		t.Fatal("a public announcement was sealed")
	}
	if _, ok := env["signature"]; !ok {
		t.Fatal("the announcement is not actor-signed")
	}
	for _, local := range []string{"share_id", "share_activity_id", "asset_id", "version_id", federation.AssetVersionMemberKey} {
		if _, ok := env[local]; ok {
			t.Errorf("the local member %q is on the wire", local)
		}
	}
	m, err := federation.ParseArchivalAsset(env[federation.MemberArchivalAsset])
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != object || m.Version.Number != 1 || len(m.Files) != 1 || m.Files[0].ContentURI != "cas://"+a.hash ||
		m.Files[0].Checksums[0].Value != a.hash || *m.Files[0].Size != 3 || m.Sensitivity != "public" {
		t.Fatalf("aa:asset %+v", m)
	}
	// Nothing more to send.
	f.worker.RunOnce(ctx)
	if n := len(f.stub.types()); n != 2 {
		t.Fatalf("%d envelopes after a third round", n)
	}
}

func TestDelivery_AssetVersion_NeverForWhatIsNoLongerGranted(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Each announcement's aa:Share is already sent; then the grant changes.
	cases := map[string]func(a avAsset, g avGrant){
		"a revoked share": func(_ avAsset, g avGrant) {
			exec(`UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = $2 WHERE id = $1`, g.share, g.shareActivity)
		},
		"an expired share": func(_ avAsset, g avGrant) {
			exec(`UPDATE federation_shares SET expires_at = now() - interval '1 minute' WHERE id = $1`, g.share)
		},
		"an asset made restricted": func(a avAsset, _ avGrant) {
			exec(`UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, a.id)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a := f.newAsset("public", true)
			g := f.grant(a, false)
			exec(`UPDATE federation_outbox SET status = 'sent' WHERE activity_id = $1`, g.shareActivity)
			change(a, g)
			before := len(f.stub.types())
			f.worker.RunOnce(ctx)
			if st, _, _ := f.row(g.announcement); st != "failed" {
				t.Fatalf("announcement %s, want failed for good", st)
			}
			if len(f.stub.types()) != before {
				t.Fatal("the peer was sent something")
			}
		})
	}
	t.Run("a draft version waits", func(t *testing.T) {
		a := f.newAsset("public", false)
		g := f.grant(a, false)
		exec(`UPDATE federation_outbox SET status = 'sent' WHERE activity_id = $1`, g.shareActivity)
		f.worker.RunOnce(ctx)
		if st, attempts, due := f.row(g.announcement); st != "queued" || attempts != 1 || due {
			t.Fatalf("a draft: %s, %d attempts, due %v; want held back under the backoff", st, attempts, due)
		}
	})
}

// unrelatedShare queues an aa:Share of a post to the peer, due now: an
// activity that has nothing to do with any announcement.
func (f *avFx) unrelatedShare() uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	post := uuid.New()
	payload, _ := json.Marshal(map[string]any{"share_id": uuid.NewString(), "object_kind": "post", "object_id": post.String(),
		"scope": "view", "peer_url": "https://receiver.example"})
	var id uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, object_uri,
		    object_kind, object_local_id, payload)
		VALUES ($1, 'aa:Share', $2, $3, $4, 'post', $5, $6) RETURNING id`, avLocal+"/activities/"+uuid.NewString(),
		avLocal+"/users/alice", f.owner, avLocal+"/posts/"+post.String(), post.String(), payload).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO federation_outbox (activity_id, peer_id, sensitivity) VALUES ($1, $2, 'public')`,
		id, f.peer); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// retry is an outbox row's attempts and next attempt time.
func (f *avFx) retry(activity uuid.UUID) (attempts int, next time.Time) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT attempts, next_attempt_at FROM federation_outbox
		WHERE activity_id = $1 AND peer_id = $2`, activity, f.peer).Scan(&attempts, &next); err != nil {
		f.t.Fatal(err)
	}
	return attempts, next
}

// A retry recorded after an announcement's aa:Share was sent (its version
// still a draft, a transfer that failed) keeps its backoff whatever else
// is sent: only the aa:Share's own send releases an announcement.
func TestDelivery_AssetVersion_OtherSendsNeverShortenARetry(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(f *avFx) avGrant{
		"a draft version": func(f *avFx) avGrant {
			return f.grant(f.newAsset("public", false), false)
		},
		"a transient failure": func(f *avFx) avGrant {
			f.stub.mu.Lock()
			f.stub.unavailable = string(federation.ActivityAAAssetVersion)
			f.stub.mu.Unlock()
			return f.grant(f.newAsset("public", true), false)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAVFx(t)
			g := setup(f)
			if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET status = 'sent' WHERE activity_id = $1`, g.shareActivity); err != nil {
				t.Fatal(err)
			}
			f.worker.RunOnce(ctx)
			attempts, next := f.retry(g.announcement)
			if st, _, due := f.row(g.announcement); st != "queued" || attempts != 1 || due {
				t.Fatalf("after its own attempt: %s, %d attempts, due %v", st, attempts, due)
			}
			// Unrelated sends, two rounds of them.
			for i := 0; i < 2; i++ {
				other := f.unrelatedShare()
				f.worker.RunOnce(ctx)
				if st, _, _ := f.row(other); st != "sent" {
					t.Fatalf("the unrelated activity is %s", st)
				}
			}
			a2, n2 := f.retry(g.announcement)
			if a2 != attempts || !n2.Equal(next) {
				t.Fatalf("another send moved the retry: %d attempts at %s, was %d at %s", a2, n2, attempts, next)
			}
			for _, typ := range f.stub.types() {
				if typ == string(federation.ActivityAAAssetVersion) {
					t.Fatal("the announcement was sent")
				}
			}
		})
	}
}

// The release follows the aa:Share's own send, in a later round as well
// as in the same batch (TestDelivery_AssetVersion_AfterItsShareThenOnce).
func TestDelivery_AssetVersion_ReleasedWhenItsShareIsSentLater(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	g := f.grant(f.newAsset("public", true), false) // the aa:Share is not due yet
	f.worker.RunOnce(ctx)
	if st, attempts, due := f.row(g.announcement); st != "queued" || attempts != 1 || due {
		t.Fatalf("waiting for its aa:Share: %s, %d attempts, due %v", st, attempts, due)
	}
	if len(f.stub.types()) != 0 {
		t.Fatal("something was sent before the aa:Share")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET next_attempt_at = now() WHERE activity_id = $1`, g.shareActivity); err != nil {
		t.Fatal(err)
	}
	f.worker.RunOnce(ctx) // the aa:Share alone, a singleton POST
	if got := f.stub.types(); len(got) != 1 || got[0] != string(federation.ActivityAAShare) {
		t.Fatalf("sent %v, want the aa:Share", got)
	}
	if st, attempts, due := f.row(g.announcement); st != "queued" || attempts != 1 || !due {
		t.Fatalf("after its aa:Share was sent: %s, %d attempts, due %v; want released", st, attempts, due)
	}
	f.worker.RunOnce(ctx)
	if got := f.stub.types(); len(got) != 2 || got[1] != string(federation.ActivityAAAssetVersion) {
		t.Fatalf("sent %v", got)
	}
}

// #1575: an asset's withdrawal through the real delivery worker. It waits
// for the peer's announcement, is released when that is sent, then goes
// once with only the aa:tombstone built from the last sealed version.

// withdraw soft-deletes a and records its withdrawals, as the delete
// handler does, then queues each to the peer due now, as the dispatcher
// does.
func (f *avFx) withdraw(a avAsset) []uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	var ids []uuid.UUID
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE assets SET deleted_at = now() WHERE id = $1`, a.id); err != nil {
			return err
		}
		var err error
		ids, err = assetsend.RecordWithdrawals(ctx, tx, f.writer, assetsend.Deleted{AssetID: a.id, BaseURL: avLocal,
			Username: func(context.Context, int64) string { return "alice" }})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := f.pool.Exec(ctx, `INSERT INTO federation_outbox (activity_id, peer_id, sensitivity) VALUES ($1, $2, 'public')`,
			id, f.peer); err != nil {
			f.t.Fatal(err)
		}
	}
	return ids
}

// inFlight has the announcement of g built for the peer, as the delivery
// worker does right before its request, and leaves its row queued: a
// delivery in flight. Its aa:Share is marked delivered first, as the
// member requires.
func (f *avFx) inFlight(g avGrant) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET status = 'sent' WHERE activity_id = $1 AND peer_id = $2`,
		g.shareActivity, f.peer); err != nil {
		f.t.Fatal(err)
	}
	var object string
	var payload []byte
	if err := f.pool.QueryRow(ctx, `SELECT object_uri, payload FROM activities WHERE id = $1`, g.announcement).Scan(&object, &payload); err != nil {
		f.t.Fatal(err)
	}
	if _, err := (assetsend.Source{Pool: f.pool}).Member(ctx, g.announcement, f.peer, object, payload); err != nil {
		f.t.Fatalf("build the announcement: %v", err)
	}
}

func TestDelivery_AssetWithdrawal_AfterItsAnnouncementThenOnce(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	a := f.newAsset("public", true)
	g := f.grant(a, true)
	exec(`UPDATE federation_outbox SET status = 'sent' WHERE activity_id = $1`, g.shareActivity)
	// The announcement is built, and its request still in flight, when the
	// asset is deleted.
	exec(`UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id = $1`, g.announcement)
	f.inFlight(g)
	ids := f.withdraw(a)
	if len(ids) != 1 {
		t.Fatalf("%d withdrawals, want 1", len(ids))
	}
	wd := ids[0]

	f.worker.RunOnce(ctx)
	if len(f.stub.types()) != 0 {
		t.Fatalf("sent %v before the announcement", f.stub.types())
	}
	// Held back without counting an attempt (rev629: waiting is not a
	// failed attempt).
	if st, attempts, due := f.row(wd); st != "queued" || attempts != 0 || due {
		t.Fatalf("withdrawal before its announcement: %s, %d attempts, due %v", st, attempts, due)
	}
	// The in-flight request completes (marked sent here, as its own
	// round would); an unrelated send does not release the withdrawal.
	exec(`UPDATE federation_outbox SET status = 'sent' WHERE activity_id = $1`, g.announcement)
	exec(`UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id = $1`, wd)
	other := f.unrelatedShare()
	f.worker.RunOnce(ctx)
	if st, _, due := f.row(wd); st != "queued" || due {
		t.Fatalf("an unrelated send released the withdrawal: %s due %v", st, due)
	}
	_ = other

	// Released by its own announcement's send, in that round.
	b := f.newAsset("public", true)
	gb := f.grant(b, true)
	exec(`UPDATE federation_outbox SET status = 'sent' WHERE activity_id = $1`, gb.shareActivity)
	f.worker.RunOnce(ctx) // b's announcement is sent
	if st, _, _ := f.row(gb.announcement); st != "sent" {
		t.Fatalf("b's announcement %s", st)
	}
	exec(`UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour', status = 'queued' WHERE activity_id = $1`, gb.announcement)
	idsB := f.withdraw(b)
	exec(`UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id = $1`, idsB[0])
	exec(`UPDATE federation_outbox SET next_attempt_at = now() WHERE activity_id = $1`, gb.announcement)
	exec(`UPDATE assets SET deleted_at = NULL WHERE id = $1`, b.id) // so its announcement can still be built
	before := len(f.stub.types())
	f.worker.RunOnce(ctx)
	if got := f.stub.types(); len(got) != before+1 || got[len(got)-1] != string(federation.ActivityAAAssetVersion) {
		t.Fatalf("sent %v", got)
	}
	if st, _, due := f.row(idsB[0]); st != "queued" || !due {
		t.Fatalf("withdrawal after its announcement was sent: %s due %v; want released", st, due)
	}
	f.worker.RunOnce(ctx)
	got := f.stub.types()
	if got[len(got)-1] != string(federation.ActivityDelete) {
		t.Fatalf("sent %v, want the Delete last", got)
	}
	if st, _, _ := f.row(idsB[0]); st != "sent" {
		t.Fatalf("withdrawal %s", st)
	}
	env := f.stub.last()
	var object string
	_ = json.Unmarshal(env["object"], &object)
	if object != avLocal+"/assets/"+b.id.String() {
		t.Fatalf("object %s", object)
	}
	if _, ok := env["signature"]; !ok {
		t.Fatal("the withdrawal is not actor-signed")
	}
	for _, local := range []string{"asset_id", "versions", "announcements", "version_id", "announcement_activity_id", "target_peer_id", "deleted",
		federation.AssetTombstoneMemberKey, federation.MemberObjectType} {
		if _, ok := env[local]; ok {
			t.Errorf("%q is on the wire", local)
		}
	}
	ts, err := federation.ParseArchivalTombstone(env[federation.MemberArchivalTombstone])
	if err != nil {
		t.Fatal(err)
	}
	if ts.ID != object || ts.LastVersion != object+"/versions/1" || len(ts.Files) != 1 || ts.Files[0].ContentURI != "cas://"+b.hash {
		t.Fatalf("aa:tombstone %+v", ts)
	}
	// Once.
	n := len(f.stub.types())
	f.worker.RunOnce(ctx)
	if len(f.stub.types()) != n {
		t.Fatal("sent again")
	}
}

func TestDelivery_AssetWithdrawal_NeverWhenTheAnnouncementReachedNothing(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			a := f.newAsset("public", true)
			g := f.grant(a, true)
			f.inFlight(g)
			ids := f.withdraw(a)
			if len(ids) != 1 {
				t.Fatalf("%d withdrawals, want 1", len(ids))
			}
			if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET status = $2 WHERE activity_id = $1`, g.announcement, status); err != nil {
				t.Fatal(err)
			}
			before := len(f.stub.types())
			f.worker.RunOnce(ctx)
			for _, typ := range f.stub.types()[before:] {
				if typ == string(federation.ActivityDelete) {
					t.Fatal("a withdrawal was sent for an announcement that reached nothing")
				}
			}
			if st, _, _ := f.row(ids[0]); st != "failed" {
				t.Fatalf("withdrawal %s, want failed for good", st)
			}
		})
	}
}

// rev628: a withdrawal still queued when the retention purge removes the
// asset and its versions is sent unchanged, with the delivery-time tier
// recheck wired as in production (the asset's tier, pgx.ErrNoRows once
// it is gone).
func TestDelivery_AssetWithdrawal_SentAfterTheAssetIsPurged(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	f.worker.SetSensitivityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Sensitivity, error) {
		if kind != "asset" {
			return "", errors.New("unsupported kind")
		}
		var tier string
		if err := f.pool.QueryRow(ctx, `SELECT sensitivity FROM assets WHERE id = $1`, id).Scan(&tier); err != nil {
			return "", err
		}
		return outbox.Sensitivity(tier), nil
	})
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	a := f.newAsset("public", true)
	g := f.grant(a, true)
	f.worker.RunOnce(ctx)
	f.worker.RunOnce(ctx)
	if st, _, _ := f.row(g.announcement); st != "sent" {
		t.Fatalf("announcement %s", st)
	}
	ids := f.withdraw(a)
	// Held back, then the asset is purged through the production service.
	exec(`UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id = $1`, ids[0])
	exec(`UPDATE assets SET deleted_at = now() - interval '3 days' WHERE id = $1`, a.id)
	if n, err := softdelete.NewService(f.pool, nil).HardDeletePastAssets(ctx, 1, 1000); err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM asset_versions WHERE asset_id = $1`, a.id).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the purge left %d versions (%v)", left, err)
	}
	exec(`UPDATE federation_outbox SET next_attempt_at = now() WHERE activity_id = $1`, ids[0])
	before := len(f.stub.types())
	f.worker.RunOnce(ctx)
	if st, _, _ := f.row(ids[0]); st != "sent" {
		var lastErr *string
		_ = f.pool.QueryRow(ctx, `SELECT last_error FROM federation_outbox WHERE activity_id = $1`, ids[0]).Scan(&lastErr)
		t.Fatalf("withdrawal after the purge: %s (%v)", st, lastErr)
	}
	got := f.stub.types()
	if len(got) != before+1 || got[len(got)-1] != string(federation.ActivityDelete) {
		t.Fatalf("sent %v", got)
	}
	env := f.stub.last()
	if _, sealed := env["encryption"]; sealed {
		t.Fatal("unexpectedly sealed")
	}
	ts, err := federation.ParseArchivalTombstone(env[federation.MemberArchivalTombstone])
	if err != nil {
		t.Fatal(err)
	}
	object := avLocal + "/assets/" + a.id.String()
	if ts.ID != object || ts.LastVersion != object+"/versions/1" || len(ts.Files) != 1 || ts.Files[0].Checksums[0].Value != a.hash {
		t.Fatalf("aa:tombstone after the purge %+v", ts)
	}
}

// revokeShare ends g's share, as an aa:Unshare does here.
func (f *avFx) revokeShare(g avGrant) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = $2 WHERE id = $1`,
		g.share, g.shareActivity); err != nil {
		f.t.Fatal(err)
	}
}

// newVersion seals version n of a over fresh stored bytes and makes it
// current, as a replacement upload does.
func (f *avFx) newVersion(a *avAsset, n int64) {
	f.t.Helper()
	ctx := context.Background()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	hash := hex.EncodeToString(b)
	for _, q := range []string{
		`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 3, 'image/png', 'fs')`,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'original', 3, 'image/png', $1, 3, 'write', 'verified')`,
	} {
		if _, err := f.pool.Exec(ctx, q, hash); err != nil {
			f.t.Fatal(err)
		}
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	v := uuid.New()
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT asset_version_create($1, $2, $3, $4, 'replacement', $5, 'png', NULL, 'unknown', NULL)`,
			a.id, v, n, a.version, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET current_version_id = $2, file_hash = $3 WHERE id = $1`, a.id, v, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, v)
		return err
	}); err != nil {
		f.t.Fatalf("version %d: %v", n, err)
	}
	a.version, a.hash = v, hash
}

// sentTombstone is the last envelope's aa:tombstone, which must be a
// Delete.
func (f *avFx) sentTombstone() *federation.ArchivalTombstone {
	f.t.Helper()
	got := f.stub.types()
	if len(got) == 0 || got[len(got)-1] != string(federation.ActivityDelete) {
		f.t.Fatalf("sent %v, want the Delete last", got)
	}
	ts, err := federation.ParseArchivalTombstone(f.stub.last()[federation.MemberArchivalTombstone])
	if err != nil {
		f.t.Fatal(err)
	}
	return ts
}

// rev629, 1: a regrant's second announcement of the version the peer
// already has, queued and going nowhere, does not hold the withdrawal
// back: it is sent at once naming that version, using one attempt.
func TestDelivery_AssetWithdrawal_AQueuedRegrantOfTheSentVersionDoesNotHoldItBack(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	a := f.newAsset("public", true)
	h1 := a.hash
	g1 := f.grant(a, true)
	f.worker.RunOnce(ctx)
	f.worker.RunOnce(ctx)
	if st, _, _ := f.row(g1.announcement); st != "sent" {
		t.Fatalf("first announcement %s", st)
	}
	f.revokeShare(g1)
	g2 := f.grant(a, false) // the same version 1 again; its aa:Share is held back
	if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id = $1`, g2.announcement); err != nil {
		t.Fatal(err)
	}
	ids := f.withdraw(a)
	f.worker.RunOnce(ctx)
	if st, attempts, _ := f.row(ids[0]); st != "sent" || attempts != 0 {
		t.Fatalf("withdrawal with a queued regrant of version 1: %s after %d failed attempts, want sent", st, attempts)
	}
	object := avLocal + "/assets/" + a.id.String()
	if ts := f.sentTombstone(); ts.LastVersion != object+"/versions/1" || ts.Files[0].Checksums[0].Value != h1 {
		t.Fatalf("tombstone %s", ts.LastVersion)
	}
}

// rev629, the broader dependency: waiting for a higher version's queued
// announcement counts no attempt, however many rounds it takes, so it
// never exhausts the withdrawal; when that announcement fails, the
// version already sent is named.
func TestDelivery_AssetWithdrawal_WaitingCountsNoAttempt(t *testing.T) {
	f := newAVFx(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	a := f.newAsset("public", true)
	h1 := a.hash
	g1 := f.grant(a, true)
	f.worker.RunOnce(ctx)
	f.worker.RunOnce(ctx)
	f.revokeShare(g1)
	f.newVersion(&a, 2)
	g2 := f.grant(a, false) // announces version 2; held back
	exec(`UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id IN ($1, $2)`, g2.announcement, g2.shareActivity)
	f.inFlight(g2) // built: its request may still reach the peer
	ids := f.withdraw(a)
	for round := 0; round < 8; round++ { // past the outbox's cap of 5 attempts
		exec(`UPDATE federation_outbox SET next_attempt_at = now() WHERE activity_id = $1`, ids[0])
		f.worker.RunOnce(ctx)
		if st, attempts, due := f.row(ids[0]); st != "queued" || due {
			t.Fatalf("round %d waiting for version 2: %s after %d attempts, due %v", round, st, attempts, due)
		}
	}
	if _, attempts, _ := f.row(ids[0]); attempts != 0 {
		t.Fatalf("waiting counted %d attempts", attempts)
	}
	for _, typ := range f.stub.types() {
		if typ == string(federation.ActivityDelete) {
			t.Fatal("the withdrawal was sent while version 2 might still reach the peer")
		}
	}
	// Version 2's announcement is given up: version 1 is named.
	exec(`UPDATE federation_outbox SET status = 'failed' WHERE activity_id = $1`, g2.announcement)
	exec(`UPDATE federation_outbox SET next_attempt_at = now() WHERE activity_id = $1`, ids[0])
	f.worker.RunOnce(ctx)
	if st, attempts, _ := f.row(ids[0]); st != "sent" || attempts != 0 {
		t.Fatalf("after version 2's announcement failed: %s, %d attempts", st, attempts)
	}
	object := avLocal + "/assets/" + a.id.String()
	if ts := f.sentTombstone(); ts.LastVersion != object+"/versions/1" || ts.Files[0].Checksums[0].Value != h1 {
		t.Fatalf("tombstone %s, want version 1", ts.LastVersion)
	}
}
