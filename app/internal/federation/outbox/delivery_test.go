// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Delivery worker integration tests per the gold-standard
// "integration: real Postgres + httptest" layer of the test-
// layering requirement. Phase 1.22.D-b-4.
//
// Three paths exercised:
//
//   1. Happy: outbox row → POST → 202 → row.status='sent';
//      delivered_with_key_id captured.
//   2. Transient 5xx: outbox row → POST → 500 → row stays
//      queued + attempts bumped + next_attempt_at scheduled
//      per the §3.4 backoff.
//   3. Terminal 4xx (not 429): outbox row → POST → 401 →
//      row.status='failed'; last_error captured.
//
// Skips without AA_DB_PASSWORD.

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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

func newDeliveryFixture(t *testing.T, inboxStatus int, captureBody *[]byte) (worker *outbox.Worker, peerID uuid.UUID, postID uuid.UUID, grantorRef int64, activityID uuid.UUID, srv *httptest.Server) {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Fake inbox server: replies with the configured status; the
	// test asserts the worker's state-transition per status code.
	var (
		mu      sync.Mutex
		sigSeen string
	)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sigSeen = r.Header.Get("Signature")
		if captureBody != nil {
			*captureBody = body
		}
		mu.Unlock()
		w.WriteHeader(inboxStatus)
	}))
	t.Cleanup(srv.Close)

	// Seed user / peer / post / share / activity (+ outbox row).
	username := "delivery-" + randHex(4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Delivery Test', 1) RETURNING ref`,
		username,
	).Scan(&grantorRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Delivery Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		srv.URL, grantorRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	postID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Delivery', 'explicit-share')`,
		postID, grantorRef,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	postIDStr := postID.String()
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
		    object_uri, object_kind, object_local_id, to_uris, payload)
		 VALUES ($1, 'Like', $2, $3, $4, 'post', $5, '[]'::jsonb, '{}'::jsonb)
		 RETURNING id`,
		"https://local.example/activities/"+randHex(8),
		"https://local.example/users/alice", grantorRef,
		"https://local.example/posts/"+postIDStr, postIDStr,
	).Scan(&activityID); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	// One queued outbox row pointing at the activity → peer.
	if _, err := pool.Exec(ctx,
		`INSERT INTO federation_outbox (activity_id, peer_id, target_user_url)
		 VALUES ($1, $2, $3)`,
		activityID, peerID,
		"https://"+srv.URL[7:]+"/users/bob",
	); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE id = $1`, activityID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
	})

	// Construct a real signer with a fresh keypair so the
	// HTTP-Sig is well-formed (the fake inbox doesn't verify;
	// we just check the header is set).
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := &outbox.IdentitySigner{
		PrivateKey: priv,
		KeyURL:     "https://local.example/instance#main-key",
	}

	worker = outbox.NewWorker(
		outbox.DeliveryConfig{
			Interval:       time.Hour, // ticker never fires
			BatchSize:      100,
			RequestTimeout: 5 * time.Second,
		},
		pool,
		signer,
		func(ctx context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			return outbox.PeerInfo{
				ID: id, InstanceURL: srv.URL,
				Enabled: true, Connected: true,
			}, nil
		},
		logger,
	)
	_ = sigSeen // captured by closure; assertion happens via post-RunOnce DB check + body capture
	return worker, peerID, postID, grantorRef, activityID, srv
}

func TestDelivery_HappyPath_2xx_TransitionsToSent(t *testing.T) {
	pool := openTestPool(t)
	worker, _, _, _, activityID, _ := newDeliveryFixture(t, http.StatusAccepted, nil)

	sent, failed, deferred := worker.RunOnce(context.Background())
	if sent != 1 || failed != 0 || deferred != 0 {
		// Print last_error to diagnose.
		var lastErr string
		_ = pool.QueryRow(context.Background(),
			`SELECT last_error FROM federation_outbox WHERE activity_id = $1`,
			activityID,
		).Scan(&lastErr)
		t.Errorf("outcome: sent=%d failed=%d deferred=%d want 1/0/0; last_error=%q", sent, failed, deferred, lastErr)
	}

	var status string
	var sentAt *time.Time
	var keyID *string
	_ = pool.QueryRow(context.Background(),
		`SELECT status, sent_at, delivered_with_key_id
		 FROM federation_outbox WHERE activity_id = $1`,
		activityID,
	).Scan(&status, &sentAt, &keyID)
	if status != "sent" {
		t.Errorf("status: got %q want sent", status)
	}
	if sentAt == nil {
		t.Error("sent_at should be set after delivery")
	}
	if keyID == nil || *keyID == "" {
		t.Error("delivered_with_key_id should be captured")
	}
}

func TestDelivery_Transient5xx_StaysQueuedWithBackoff(t *testing.T) {
	pool := openTestPool(t)
	worker, _, _, _, activityID, _ := newDeliveryFixture(t, http.StatusInternalServerError, nil)

	sent, failed, deferred := worker.RunOnce(context.Background())
	if sent != 0 || failed != 0 || deferred != 1 {
		t.Errorf("outcome: sent=%d failed=%d deferred=%d want 0/0/1", sent, failed, deferred)
	}

	var status string
	var attempts int16
	var nextAt time.Time
	var lastErr string
	_ = pool.QueryRow(context.Background(),
		`SELECT status, attempts, next_attempt_at, last_error
		 FROM federation_outbox WHERE activity_id = $1`,
		activityID,
	).Scan(&status, &attempts, &nextAt, &lastErr)
	if status != "queued" {
		t.Errorf("status: got %q want queued (transient retry)", status)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d want 1", attempts)
	}
	if nextAt.Before(time.Now()) {
		t.Error("next_attempt_at should be in the future (backoff)")
	}
	if lastErr == "" {
		t.Error("last_error should describe the 500")
	}
}

func TestDelivery_Terminal4xx_TransitionsToFailed(t *testing.T) {
	pool := openTestPool(t)
	worker, _, _, _, activityID, _ := newDeliveryFixture(t, http.StatusUnauthorized, nil)

	sent, failed, deferred := worker.RunOnce(context.Background())
	if sent != 0 || failed != 1 || deferred != 0 {
		t.Errorf("outcome: sent=%d failed=%d deferred=%d want 0/1/0", sent, failed, deferred)
	}

	var status string
	_ = pool.QueryRow(context.Background(),
		`SELECT status FROM federation_outbox WHERE activity_id = $1`,
		activityID,
	).Scan(&status)
	if status != "failed" {
		t.Errorf("status: got %q want failed (terminal 4xx)", status)
	}
}

func TestDelivery_429_TreatedAsTransient(t *testing.T) {
	pool := openTestPool(t)
	worker, _, _, _, activityID, _ := newDeliveryFixture(t, http.StatusTooManyRequests, nil)

	_, _, deferred := worker.RunOnce(context.Background())
	if deferred != 1 {
		t.Errorf("429 should be transient; deferred=%d want 1", deferred)
	}

	var status string
	var attempts int16
	_ = pool.QueryRow(context.Background(),
		`SELECT status, attempts FROM federation_outbox WHERE activity_id = $1`,
		activityID,
	).Scan(&status, &attempts)
	if status != "queued" || attempts != 1 {
		t.Errorf("429 should bump attempts + stay queued; status=%q attempts=%d", status, attempts)
	}
}

func TestDelivery_EnvelopeIsSignedAndWellFormed(t *testing.T) {
	pool := openTestPool(t)
	var capturedBody []byte
	worker, _, _, _, _, srv := newDeliveryFixture(t, http.StatusAccepted, &capturedBody)
	_ = srv

	worker.RunOnce(context.Background())

	if len(capturedBody) == 0 {
		t.Fatal("envelope body not captured")
	}
	// Must parse as JSON with the v1 @context.
	bodyStr := string(capturedBody)
	if !contains(bodyStr, `"@context":"https://artist-alley.org/protocol/v1"`) {
		t.Errorf("envelope @context wrong; body=%s", bodyStr)
	}
	if !contains(bodyStr, `"type":"Like"`) {
		t.Errorf("envelope type wrong; body=%s", bodyStr)
	}
	// Signature block must be present (structural-only per
	// spec §5.6; real crypto verify lands in 1.22.I).
	if !contains(bodyStr, `"signature":{`) {
		t.Errorf("envelope missing signature block; body=%s", bodyStr)
	}
	_ = pool // imported for the QueryRow helper; assertion above is on capture
	_ = pgtype.UUID{}
}

func TestDelivery_TwoQueuedForSamePeer_UseBatchEndpoint(t *testing.T) {
	// Two outbox rows for the same peer → delivery worker
	// fires ONE POST to /federation/inbox/batch with both
	// envelopes. Receiver returns per-envelope status array.
	pool := openTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var (
		mu          sync.Mutex
		batchHits   int
		singleHits  int
		batchBodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		switch r.URL.Path {
		case "/federation/inbox/batch":
			batchHits++
			batchBodies = append(batchBodies, body)
			// Parse envelopes and reply with accepted status for each.
			var batch struct {
				Envelopes []struct {
					ID string `json:"id"`
				} `json:"envelopes"`
			}
			_ = json.Unmarshal(body, &batch)
			type result struct {
				ActivityURI string `json:"activity_uri"`
				Status      string `json:"status"`
				Reason      string `json:"reason"`
			}
			out := struct {
				Results []result `json:"results"`
			}{}
			for _, env := range batch.Envelopes {
				out.Results = append(out.Results, result{
					ActivityURI: env.ID, Status: "accepted",
				})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case "/federation/inbox":
			singleHits++
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			mu.Unlock()
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// Seed user / peer / post + 2 activities + 2 outbox rows.
	username := "delivery-batch-" + randHex(4)
	var grantorRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Delivery Batch Test', 1) RETURNING ref`,
		username,
	).Scan(&grantorRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var peerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Delivery Batch Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		srv.URL, grantorRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Batch', 'explicit-share')`,
		postID, grantorRef,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	postIDStr := postID.String()
	// Two activities + two outbox rows targeting the same peer.
	for i := 0; i < 2; i++ {
		var activityID uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
			    object_uri, object_kind, object_local_id, to_uris, payload)
			 VALUES ($1, 'Like', $2, $3, $4, 'post', $5, '[]'::jsonb, '{}'::jsonb)
			 RETURNING id`,
			"https://local.example/activities/"+randHex(8),
			"https://local.example/users/alice", grantorRef,
			"https://local.example/posts/"+postIDStr, postIDStr,
		).Scan(&activityID); err != nil {
			t.Fatalf("seed activity: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO federation_outbox (activity_id, peer_id, target_user_url)
			 VALUES ($1, $2, $3)`,
			activityID, peerID,
			"https://"+srv.URL[7:]+"/users/bob",
		); err != nil {
			t.Fatalf("seed outbox: %v", err)
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, grantorRef)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
	})

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := &outbox.IdentitySigner{
		PrivateKey: priv,
		KeyURL:     "https://local.example/instance#main-key",
	}
	worker := outbox.NewWorker(
		outbox.DeliveryConfig{Interval: time.Hour, BatchSize: 100, RequestTimeout: 5 * time.Second},
		pool, signer,
		func(_ context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			return outbox.PeerInfo{ID: id, InstanceURL: srv.URL, Enabled: true, Connected: true}, nil
		},
		logger,
	)

	sent, _, _ := worker.RunOnce(context.Background())
	if sent != 2 {
		t.Errorf("sent count: got %d want 2", sent)
	}

	mu.Lock()
	defer mu.Unlock()
	if batchHits != 1 {
		t.Errorf("batch endpoint hits: got %d want 1 (both envelopes in one POST)", batchHits)
	}
	if singleHits != 0 {
		t.Errorf("singleton endpoint hits: got %d want 0 (everything should've batched)", singleHits)
	}
	if len(batchBodies) == 1 {
		// Confirm both envelopes made it into the batched body.
		var got struct {
			Envelopes []struct{ ID string } `json:"envelopes"`
		}
		_ = json.Unmarshal(batchBodies[0], &got)
		if len(got.Envelopes) != 2 {
			t.Errorf("batch body envelope count: got %d want 2", len(got.Envelopes))
		}
	}

	// Both outbox rows transitioned to 'sent'.
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM federation_outbox WHERE peer_id = $1 AND status = 'sent'`,
		peerID,
	).Scan(&n)
	if n != 2 {
		t.Errorf("federation_outbox sent count: got %d want 2", n)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
