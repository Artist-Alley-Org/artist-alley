// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1580: what the sender puts on the wire. The local activity payload
// is the audit record and may hold operator-only data (a share's
// notes) and local ids; a delivered envelope carries only the members
// spec v1 defines for its activity, in plaintext and inside the
// encryption block alike. An activity type v1 does not federate is
// not enqueued, and a row queued for one is never sent.

package outbox_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

const secretNote = "operator-only note SECRET-1580"

// privateShareMembers must never leave the instance.
var privateShareMembers = []string{"notes", "notes_private", "share_id", "object_id", "object_uri", "peer_url", "target_user_url"}

// envelopeBase is the §3 member set every envelope may carry.
var envelopeBase = map[string]bool{
	"@context": true, "type": true, "id": true, "actor": true, "published": true,
	"to": true, "cc": true, "object": true, "encryption": true, "signature": true,
	"aa:encryptionPublicKey": true,
}

type capture struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *capture) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...)
}

// envelopeFor returns the delivered envelope with this id, from a
// single or a batch POST; nil when none was sent.
func (c *capture) envelopeFor(t *testing.T, activityURI string) map[string]json.RawMessage {
	t.Helper()
	for _, b := range c.all() {
		var batch struct {
			Envelopes []json.RawMessage `json:"envelopes"`
		}
		candidates := []json.RawMessage{b}
		if json.Unmarshal(b, &batch) == nil && len(batch.Envelopes) > 0 {
			candidates = batch.Envelopes
		}
		for _, raw := range candidates {
			var env map[string]json.RawMessage
			if json.Unmarshal(raw, &env) != nil {
				continue
			}
			var id string
			_ = json.Unmarshal(env["id"], &id)
			if id == activityURI {
				return env
			}
		}
	}
	return nil
}

type world struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	logger  *slog.Logger
	cap     *capture
	srv     *httptest.Server
	userRef int64
	actor   emit.ActorContext
	peerID  uuid.UUID
	postID  uuid.UUID
	writer  *activities.Writer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, ctx: context.Background(), pool: openTestPool(t), cap: &capture{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.cap.mu.Lock()
		w.cap.bodies = append(w.cap.bodies, b)
		w.cap.mu.Unlock()
		rw.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(w.srv.Close)

	username := "wire-" + randHex(4)
	if err := w.pool.QueryRow(w.ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Wire Test', 1) RETURNING ref`, username,
	).Scan(&w.userRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	w.actor = emit.ActorContext{UserRef: w.userRef, Username: username, BaseURL: "https://local.example"}
	if err := w.pool.QueryRow(w.ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref, capabilities)
		 VALUES ($1, 'Wire Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2, '["share-lifecycle-1"]'::jsonb) RETURNING id`,
		w.srv.URL, w.userRef,
	).Scan(&w.peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	w.postID = uuid.New()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Wire', 'explicit-share')`,
		w.postID, w.userRef,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	reg := cache.NewRegistry(w.pool, w.logger)
	t.Cleanup(reg.Stop)
	w.writer = activities.NewWriter(w.pool, w.logger, reg)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = w.pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id = $1`, w.peerID)
		_, _ = w.pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id = $1`, w.peerID)
		_, _ = w.pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, w.userRef)
		_, _ = w.pool.Exec(c, `DELETE FROM posts WHERE id = $1`, w.postID)
		_, _ = w.pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, w.peerID)
		_, _ = w.pool.Exec(c, `DELETE FROM federation_user_keys WHERE user_ref = $1`, w.userRef)
		_, _ = w.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, w.userRef)
		_, _ = w.pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
	})
	return w
}

// record stores an emitted activity (and, through afterRecord, any
// rows that must share its transaction) and returns its id and URI.
func (w *world) record(in activities.Input, afterRecord func(tx pgx.Tx, activityID uuid.UUID) error) (uuid.UUID, string) {
	w.t.Helper()
	tx, err := w.pool.Begin(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	defer tx.Rollback(w.ctx) //nolint:errcheck
	rec, err := w.writer.RecordActivity(w.ctx, tx, in)
	if err != nil {
		w.t.Fatalf("record %s: %v", in.Type, err)
	}
	if afterRecord != nil {
		if err := afterRecord(tx, rec.ID); err != nil {
			w.t.Fatalf("after record: %v", err)
		}
	}
	if err := tx.Commit(w.ctx); err != nil {
		w.t.Fatal(err)
	}
	return rec.ID, in.ActivityURI
}

// dispatch runs the production dispatcher (explicit-share post,
// public tier) until it has passed activityID.
func (w *world) dispatch(activityID uuid.UUID, spy *auditSpy) {
	w.t.Helper()
	_, _ = w.pool.Exec(w.ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
	reg := cache.NewRegistry(w.pool, w.logger)
	w.t.Cleanup(reg.Stop)
	d := outbox.NewDispatcher(outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100}, w.pool,
		outbox.NewResolver(w.pool, reg, func(context.Context) bool { return true }), w.logger)
	d.SetSkippedAudit(spy.record)
	d.SetSensitivityLookup(fixedSensitivity(outbox.SensitivityPublic))
	d.SetVisibilityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		var v string
		if err := w.pool.QueryRow(ctx, `SELECT visibility FROM posts WHERE id = $1`, id).Scan(&v); err != nil {
			return "", err
		}
		return outbox.Visibility(v), nil
	})
	for i := 0; i < 50; i++ {
		if n, _, _ := d.RunOnce(w.ctx); n == 0 {
			return
		}
		var passed bool
		_ = w.pool.QueryRow(w.ctx, `SELECT EXISTS (SELECT 1 FROM federation_outbox WHERE activity_id = $1)
		    OR (SELECT last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1) = $1`, activityID).Scan(&passed)
		if passed {
			return
		}
	}
}

// worker is the production delivery worker aimed at the capture
// server; supportsE2E switches the per-recipient encryption on.
func (w *world) worker(supportsE2E bool, recipientPub []byte) *outbox.Worker {
	w.t.Helper()
	wk := outbox.NewWorker(outbox.DeliveryConfig{Interval: time.Hour, BatchSize: 100, RequestTimeout: 5 * time.Second},
		w.pool, &outbox.IdentitySigner{PrivateKey: freshEd25519(w.t), KeyURL: "https://local.example/instance#main-key"},
		func(_ context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			return outbox.PeerInfo{ID: id, InstanceURL: w.srv.URL, Enabled: true, Connected: true}, nil
		}, w.logger)
	wireTestActorSigner(w.t, wk)
	if supportsE2E {
		wireHooks(w.t, wk, true, recipientPub)
	}
	return wk
}

// queue inserts an outbox row for activityID as the dispatcher would.
func (w *world) queue(activityID uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(w.ctx,
		`INSERT INTO federation_outbox (activity_id, peer_id, target_user_url, sensitivity)
		 VALUES ($1, $2, $3, 'public') RETURNING id`,
		activityID, w.peerID, "https://"+w.srv.URL[len("http://"):]+"/users/bob",
	).Scan(&id); err != nil {
		w.t.Fatalf("queue: %v", err)
	}
	return id
}

// httpsURL is the capture server's origin in the https form every
// real peer URL has (normalizeInstanceURL); delivery reaches the
// server through the worker's peer lookup, not through this value.
func (w *world) httpsURL() string { return "https://" + w.srv.URL[len("http://"):] }

func freshEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func memberNames(env map[string]json.RawMessage, skipBase bool) []string {
	var out []string
	for k := range env {
		if skipBase && envelopeBase[k] {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (w *world) share() (uuid.UUID, string) {
	w.t.Helper()
	expires := time.Now().Add(72 * time.Hour).UTC()
	shareID := uuid.New()
	ref := emit.ShareRef{
		ShareID: shareID, ObjectKind: federation.ShareObjectKindPost, ObjectID: w.postID,
		PeerURL: w.httpsURL(), TargetUserURL: "https://" + w.srv.URL[len("http://"):] + "/users/bob",
		Scope: federation.ShareScopeView, ExpiresAt: &expires, Notes: secretNote,
	}
	em := emit.Share(w.actor, ref)
	return w.record(em.Activity, func(tx pgx.Tx, activityID uuid.UUID) error {
		_, err := tx.Exec(w.ctx,
			`INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id,
			    target_user_url, scope, expires_at, notes, granted_activity_id)
			 VALUES ($1, $2, 'post', $3, $4, $5, 'view', $6, $7, $8)`,
			shareID, w.userRef, w.postID, w.peerID, ref.TargetUserURL, expires, secretNote, activityID)
		return err
	})
}

func assertNoPrivateShareMembers(t *testing.T, where string, members map[string]json.RawMessage, raw []byte) {
	t.Helper()
	for _, k := range privateShareMembers {
		if _, ok := members[k]; ok {
			t.Errorf("%s carries the local-only member %q", where, k)
		}
	}
	if strings.Contains(string(raw), "SECRET-1580") {
		t.Errorf("%s carries the operator's private share note", where)
	}
	for _, k := range []string{"object_kind", "target_peer", "scope"} {
		if _, ok := members[k]; !ok {
			t.Errorf("%s lacks the §7.5.1 member %q (members %v)", where, k, memberNames(members, true))
		}
	}
}

// A real share, granted with operator notes on an explicit-share post
// and fanned out by the production dispatcher, is delivered without
// the notes or any local id, in plaintext and encrypted.
func TestShareDelivery_PrivateMembersNeverLeave(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			var senderPub, recipientPub, recipientPriv []byte
			if encrypted {
				senderPub = seedSenderKeypair(t, w.ctx, w.pool, w.userRef)
				recipientPub, recipientPriv = freshRecipientKeypair(t)
			}
			activityID, uri := w.share()
			w.dispatch(activityID, &auditSpy{})
			var queued int
			_ = w.pool.QueryRow(w.ctx, `SELECT count(*) FROM federation_outbox WHERE activity_id = $1`, activityID).Scan(&queued)
			if queued != 1 {
				t.Fatalf("the share was not enqueued for the peer (rows %d)", queued)
			}
			w.worker(encrypted, recipientPub).RunOnce(w.ctx)

			env := w.cap.envelopeFor(t, uri)
			if env == nil {
				t.Fatalf("the share was not delivered (%d bodies)", len(w.cap.all()))
			}
			raw, _ := json.Marshal(env)
			if !encrypted {
				assertNoPrivateShareMembers(t, "plaintext envelope", env, raw)
			} else {
				if _, ok := env["encryption"]; !ok {
					t.Fatal("expected an encrypted envelope")
				}
				for _, k := range memberNames(env, true) {
					t.Errorf("encrypted envelope carries %q in clear", k)
				}
				if strings.Contains(string(raw), "SECRET-1580") {
					t.Error("encrypted envelope carries the note in clear")
				}
				var block federation.EncryptionBlock
				if err := json.Unmarshal(env["encryption"], &block); err != nil {
					t.Fatal(err)
				}
				pt, err := federation.DecryptActivityPayload(block.Ciphertext, block.Nonce, recipientPriv, senderPub)
				if err != nil {
					t.Fatalf("decrypt: %v", err)
				}
				var sealed map[string]json.RawMessage
				if err := json.Unmarshal(pt, &sealed); err != nil {
					t.Fatal(err)
				}
				assertNoPrivateShareMembers(t, "sealed payload", sealed, pt)
			}

			// The local audit record keeps the note.
			var note string
			_ = w.pool.QueryRow(w.ctx, `SELECT payload->>'notes' FROM activities WHERE id = $1`, activityID).Scan(&note)
			if note != secretNote {
				t.Errorf("the local activity payload lost the note (%q)", note)
			}
		})
	}
}

// Each federated activity is delivered with exactly its v1 members;
// local-only payload members (titles, depths, local ids) stay home.
func TestWireProjection_FederatedTypes(t *testing.T) {
	w := newWorld(t)
	post := emit.PostRef{ID: w.postID.String(), Title: "Wire", AuthorUserRef: w.userRef, AuthorURI: w.actor.URI()}
	cases := []struct {
		name string
		in   activities.Input
		want []string
	}{
		{"like", emit.Like(w.actor, post).Activity, nil},
		{"comment", emit.CreateComment(w.actor, emit.CommentRef{
			ID: uuid.NewString(), PostID: w.postID.String(), PostTitle: "Wire", PostAuthorRef: w.userRef,
			PostAuthorURI: w.actor.URI(), Body: "Lovely.", Depth: 0,
		}).Activity, []string{"content", "inReplyTo", "object_type"}},
		{"unshare", emit.Unshare(w.actor, emit.ShareRef{
			ShareID: uuid.New(), ObjectKind: federation.ShareObjectKindPost, ObjectID: w.postID,
			PeerURL: w.httpsURL(), Scope: federation.ShareScopeView, Notes: secretNote,
		}, "https://local.example/activities/"+uuid.NewString()).Activity, []string{"aa:revokes", "object_kind", "target_peer"}},
	}
	wk := w.worker(false, nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, uri := w.record(c.in, nil)
			w.queue(id)
			wk.RunOnce(w.ctx)
			env := w.cap.envelopeFor(t, uri)
			if env == nil {
				t.Fatal("not delivered")
			}
			got := memberNames(env, true)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("activity members %v, want %v", got, c.want)
			}
			raw, _ := json.Marshal(env)
			if strings.Contains(string(raw), "SECRET-1580") {
				t.Error("private note on the wire")
			}
		})
	}
}

// Activity types v1 does not federate are refused at the sender: never
// enqueued by the dispatcher (audited activity_not_federated), and a
// row already queued for one is closed without a POST.
func TestWireProjection_UnfederatedTypes(t *testing.T) {
	w := newWorld(t)
	post := emit.PostRef{ID: w.postID.String(), Title: "Wire", AuthorUserRef: w.userRef, AuthorURI: w.actor.URI()}
	cases := map[string]activities.Input{
		"create_post":    emit.CreatePost(w.actor, post, emit.PostVisibilityPublic).Activity,
		"update_profile": emit.UpdateProfile(w.actor, emit.ProfileSnapshot{DisplayName: "A", Bio: "b"}).Activity,
		"undo_like":      emit.UndoLike(w.actor, "https://local.example/activities/x", w.postID.String()).Activity,
		"delete_comment": emit.DeleteComment(w.actor, uuid.NewString(), w.postID.String()).Activity,
	}
	wk := w.worker(false, nil)
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			id, uri := w.record(in, nil)
			rowID := w.queue(id)
			wk.RunOnce(w.ctx)
			if env := w.cap.envelopeFor(t, uri); env != nil {
				t.Errorf("an unfederated %s was delivered with members %v", in.Type, memberNames(env, true))
			}
			var status, lastErr string
			_ = w.pool.QueryRow(w.ctx, `SELECT status, COALESCE(last_error, '') FROM federation_outbox WHERE id = $1`, rowID).Scan(&status, &lastErr)
			if status != string(federation.OutboxStatusFailed) || !strings.Contains(lastErr, "activity_not_federated") {
				t.Errorf("queued row: %s / %q, want failed / activity_not_federated", status, lastErr)
			}
		})
	}

	// A malformed local payload is refused before signing (#1580 C1):
	// the row is closed, nothing is POSTed, the local record is kept.
	t.Run("malformed_share_payload", func(t *testing.T) {
		in := emit.Share(w.actor, emit.ShareRef{
			ShareID: uuid.New(), ObjectKind: federation.ShareObjectKindPost, ObjectID: w.postID,
			PeerURL: w.httpsURL(), Scope: federation.ShareScopeView, Notes: secretNote,
		}).Activity
		in.Payload["scope"] = true
		id, uri := w.record(in, nil)
		rowID := w.queue(id)
		wk.RunOnce(w.ctx)
		if env := w.cap.envelopeFor(t, uri); env != nil {
			t.Errorf("a share with a boolean scope was delivered: %v", memberNames(env, true))
		}
		var status, lastErr string
		_ = w.pool.QueryRow(w.ctx, `SELECT status, COALESCE(last_error, '') FROM federation_outbox WHERE id = $1`, rowID).Scan(&status, &lastErr)
		if status != string(federation.OutboxStatusFailed) || !strings.Contains(lastErr, "activity_not_federated") || !strings.Contains(lastErr, "scope") {
			t.Errorf("queued row: %s / %q, want failed / activity_not_federated naming scope", status, lastErr)
		}
		var scope, note string
		_ = w.pool.QueryRow(w.ctx, `SELECT payload->>'scope', payload->>'notes' FROM activities WHERE id = $1`, id).Scan(&scope, &note)
		if scope != "true" || note != secretNote {
			t.Errorf("local record changed: scope %q note %q", scope, note)
		}
	})

	// #1571: a share row queued for a peer whose stored intersection no
	// longer holds share-lifecycle-1 is closed without a POST.
	t.Run("share_to_a_peer_without_the_profile", func(t *testing.T) {
		w := newWorld(t)
		if _, err := w.pool.Exec(w.ctx, `UPDATE federation_peers SET capabilities = '[]'::jsonb WHERE id = $1`, w.peerID); err != nil {
			t.Fatal(err)
		}
		wk := w.worker(false, nil)
		for _, in := range []activities.Input{
			emit.Share(w.actor, emit.ShareRef{ShareID: uuid.New(), ObjectKind: federation.ShareObjectKindPost, ObjectID: w.postID,
				PeerURL: w.httpsURL(), Scope: federation.ShareScopeView}).Activity,
			emit.Unshare(w.actor, emit.ShareRef{ShareID: uuid.New(), ObjectKind: federation.ShareObjectKindPost, ObjectID: w.postID,
				PeerURL: w.httpsURL(), Scope: federation.ShareScopeView}, "https://local.example/activities/"+uuid.NewString()).Activity,
		} {
			id, uri := w.record(in, nil)
			rowID := w.queue(id)
			wk.RunOnce(w.ctx)
			if env := w.cap.envelopeFor(t, uri); env != nil {
				t.Errorf("%s delivered to a peer without share-lifecycle-1: %v", in.Type, memberNames(env, true))
			}
			var status, lastErr string
			_ = w.pool.QueryRow(w.ctx, `SELECT status, COALESCE(last_error, '') FROM federation_outbox WHERE id = $1`, rowID).Scan(&status, &lastErr)
			if status != string(federation.OutboxStatusFailed) || !strings.Contains(lastErr, "profile_not_negotiated") {
				t.Errorf("%s queued row: %s / %q, want failed / profile_not_negotiated", in.Type, status, lastErr)
			}
		}
	})

	t.Run("dispatcher_skips_create_post", func(t *testing.T) {
		w := newWorld(t)
		post := emit.PostRef{ID: w.postID.String(), Title: "Wire", AuthorUserRef: w.userRef, AuthorURI: w.actor.URI()}
		grant, _ := w.record(emit.Like(w.actor, post).Activity, nil)
		if _, err := w.pool.Exec(w.ctx,
			`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id)
			 VALUES ($1, 'post', $2, $3, 'view', $4)`, w.userRef, w.postID, w.peerID, grant); err != nil {
			t.Fatal(err)
		}
		id, _ := w.record(emit.CreatePost(w.actor, post, emit.PostVisibilityPublic).Activity, nil)
		spy := &auditSpy{}
		w.dispatch(id, spy)
		var n int
		_ = w.pool.QueryRow(w.ctx, `SELECT count(*) FROM federation_outbox WHERE activity_id = $1`, id).Scan(&n)
		if n != 0 {
			t.Errorf("Create(post) was enqueued (%d rows)", n)
		}
		if e := spy.findByActivityID(id.String()); e == nil || e.reason != "activity_not_federated" {
			t.Errorf("skip audit %+v, want activity_not_federated", e)
		}
	})
}
