// End-to-end paired-peer test — the latency-contract test for
// the gold-standard sub-1s federation guarantee per spec
// §3.5. Phase 1.22.D-b-6.
//
// # The contract
//
// docs/spec/federation/v1.md §3.5 locks in:
//
//   "Federation activity propagation targets sub-second
//   end-to-end through LISTEN/NOTIFY on activities,
//   federation_outbox, and federation_inbox. Tickers (30s
//   default each) are correctness backstop only."
//
// This test IS the contract. The test uses PRODUCTION tick
// intervals — NO fixture tuning — so if any future commit:
//   - removes a LISTEN/NOTIFY trigger,
//   - removes a SetRawPool wiring call at boot,
//   - or raises the dispatcher/delivery tickers above 1s
// the test fails. Test failure means the latency guarantee
// regressed; the response is to restore the LISTEN/NOTIFY
// primitive, NOT to tune the test intervals (per the
// 2026-06-07 design thread that locked this in).
//
// # Fixture shape
//
// Single process; both halves wired in one test:
//   - Studio A (sender): outbox dispatcher + delivery worker.
//   - Studio B (receiver): real HTTP server with the inbox
//     handler + inbox dispatcher + social.InsertRemoteLike
//     hooked up.
//
// The wire IS HTTP. The federation_outbox + federation_inbox
// + the LISTEN/NOTIFY triggers all run against the same real
// Postgres DB (the test fixture cleans up afterwards). The
// social.Handler instance is shared between both halves —
// in production A and B are separate processes with separate
// DBs; here we share the DB but the federation tables
// distinguish A-side rows from B-side rows via peer_id.
//
// Skips without AA_DB_PASSWORD.

package federation_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
	"github.com/mscrnt/artist-alley/app/internal/social"
)

// TestFederation_EndToEnd_ProductionDefaults_SubSecond is the
// latency-contract test for spec §3.5. PRODUCTION tick intervals
// on every layer (no fixture tuning); LISTEN/NOTIFY is the
// load-bearing latency primitive on all three queues
// (activities, federation_outbox, federation_inbox).
//
// Hard 1-second deadline from the local activity INSERT to
// the receiver-side likes row materialising.
//
// If this test fails, do NOT tune the intervals to make it
// pass — that would silently lower the gold-standard guarantee.
// Instead: investigate the LISTEN/NOTIFY chain (triggers in
// migration 00005 + 00006, SetRawPool wiring in api.go,
// dispatcher/delivery wake-channel plumbing).
func TestFederation_EndToEnd_ProductionDefaults_SubSecond(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	// This test requires DB isolation: a concurrently-running
	// app container's outbox dispatcher would race the test's
	// dispatcher on the shared federation_dispatch_state cursor,
	// causing the new Like activity to fall outside the test's
	// scan window. The user/script harness is responsible for
	// stopping the app container before running this test
	// (or running it in CI where only one process touches the
	// DB). The harness sets AA_E2E_ISOLATED=1 to opt in;
	// missing → skip with a clear reason.
	if os.Getenv("AA_E2E_ISOLATED") == "" {
		t.Skip("AA_E2E_ISOLATED not set; skipping latency-contract test " +
			"(stop the dev app container + set AA_E2E_ISOLATED=1 to run; " +
			"see test comment for the contract this guards)")
	}
	pool := openE2EPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Quiet by default — flip to slog.LevelDebug + os.Stderr
	// when debugging a deadline miss.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// --- Studio B (receiver): real HTTP server with inbox -------
	bSocialReg := cache.NewRegistry(pool, logger)
	t.Cleanup(bSocialReg.Stop)
	bSocial := social.NewHandler(pool, logger, bSocialReg)
	bSocial.SetPostTargetLookup(func(ctx context.Context, postID uuid.UUID) (int64, bool, error) {
		var ref int64
		err := pool.QueryRow(ctx,
			`SELECT author_user_ref FROM posts WHERE id = $1 AND deleted_at IS NULL`,
			postID,
		).Scan(&ref)
		if err != nil {
			return 0, false, nil
		}
		return ref, true, nil
	})

	// --- Studio A (sender) keypair, used by both A's outbox
	// signer + B's inbox HTTP-Sig verifier --------------------
	aPub, aPriv, _ := ed25519.GenerateKey(rand.Reader)
	aBaseURL := "https://studio-a-" + randHex(4) + ".example"
	aKeyURL := aBaseURL + "/federation/instance#main-key"

	// --- Studio B setup: seed user (post owner) + register A
	// as a peer (B knows A's pubkey from the prior handshake) -
	bUserRef, bPostID := seedUserAndPost(t, pool, "studio-b-alice-"+randHex(4))
	var aPeerID uuid.UUID
	aPubPEM, _ := federation.PublicKeyToPEM(aPub)
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Studio A', $2, 'connected', 'plaintext', TRUE, 'connected', $3)
		 RETURNING id`,
		aBaseURL, string(aPubPEM), bUserRef,
	).Scan(&aPeerID); err != nil {
		t.Fatalf("seed peer A: %v", err)
	}

	// B's inbox handler. lookupPeer maps A's HTTP-Sig keyID
	// to A's seeded peer row.
	bInboxHandler := inbox.NewHandler(inbox.HandlerDeps{
		Pool: inbox.New(pool),
		Lookup: &e2eByKeyID{keyID: aKeyURL, peerID: aPeerID, peerURL: aBaseURL, pubKey: aPub},
		Logger:       logger,
		LocalBaseURL: func(context.Context) string { return "" }, // skip host check (we're flexible in tests)
	})
	// Inbox dispatcher with PRODUCTION DEFAULTS — 30s ticker
	// is correctness backstop only; LISTEN/NOTIFY (via
	// SetRawPool below) is the primary signal per spec §3.5.
	bInboxDispatcher := inbox.NewDispatcher(
		inbox.DefaultDispatcherConfig(),
		inbox.New(pool),
		func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			return inbox.PeerInfo{ID: id, InstanceURL: aBaseURL, Enabled: true, Connected: true}, nil
		},
		nil, logger,
	)
	bInboxDispatcher.SetSocialPoster(&e2eSocialPoster{h: bSocial})
	bInboxDispatcher.SetRemoteActorUpserter(remote.NewUpserter(pool))
	bInboxDispatcher.SetRegistry(inbox.BuildRegistry(bInboxDispatcher, logger))
	bInboxDispatcher.SetRawPool(pool) // ← LISTEN federation_inbox_pending
	go bInboxDispatcher.Run(ctx)

	// B's HTTP server: mounts /federation/inbox + /batch.
	bRouter := chi.NewRouter()
	bRouter.Post("/federation/inbox", bInboxHandler.PostInbox)
	bRouter.Post("/federation/inbox/batch", bInboxHandler.PostInboxBatch)
	bSrv := httptest.NewServer(bRouter)
	t.Cleanup(bSrv.Close)

	// --- Studio A setup: outbox dispatcher + delivery worker --
	// Re-use the SAME federation_peers row as B (since both
	// halves share the DB in this test) — A's peer registry
	// pointing at B is THE row we'd seed in production.
	aliceRef, _ := seedUserAndPost(t, pool, "studio-a-bob-"+randHex(4))
	// One more peer row: B-as-seen-from-A (target is the
	// httptest server's URL).
	var bPeerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Studio B', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		bSrv.URL, aliceRef,
	).Scan(&bPeerID); err != nil {
		t.Fatalf("seed peer B: %v", err)
	}

	// Cleanup AFTER seeding so the IDs are in scope.
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id IN ($1, $2)`, aPeerID, bPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_inbox WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_remote_actors WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id IN ($1, $2)`, aPeerID, bPeerID)
		_, _ = pool.Exec(c, `DELETE FROM likes WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM comments WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref IN ($1, $2)`, aliceRef, bUserRef)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id IN ($1, $2)`, aPeerID, bPeerID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE author_user_ref IN ($1, $2)`, aliceRef, bUserRef)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref IN ($1, $2)`, aliceRef, bUserRef)
		_, _ = pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
	})

	// Seed a share grant on B's side: A has been granted view
	// access to B's post (via target_user_url that names A's
	// user). The recipient resolver on A reads this when it
	// fans out the Like.
	var grantActivityID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb) RETURNING id`,
		bSrv.URL+"/activities/"+randHex(8),
		bSrv.URL+"/users/bob", bUserRef,
	).Scan(&grantActivityID); err != nil {
		t.Fatalf("seed grant activity: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO federation_shares (
		    grantor_user_ref, object_kind, object_id, peer_id,
		    target_user_url, scope, granted_activity_id
		 ) VALUES ($1, 'post', $2, $3, $4, 'view', $5)`,
		aliceRef, bPostID, bPeerID, // A's view: "I share my notion of B's post with the peer I call B"
		bSrv.URL+"/users/bob",
		grantActivityID,
	); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	// Skip the historical activities by advancing the cursor
	// to the last existing activity id BEFORE starting the
	// dispatchers — otherwise the first RunOnce races us and
	// can process up to BatchSize=100 old rows, advancing the
	// cursor past my synthetic position, causing the new Like
	// to fall outside the next scan window.
	var lastExistingID *uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM activities ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&lastExistingID)
	if lastExistingID != nil {
		_, _ = pool.Exec(ctx,
			`UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1, last_dispatched_at = NOW() WHERE id = 1`,
			lastExistingID,
		)
	} else {
		// No prior activities at all — leave cursor NULL.
		_, _ = pool.Exec(ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
	}

	// Outbox dispatcher on A (LISTEN/NOTIFY-driven).
	aOutboxReg := cache.NewRegistry(pool, logger)
	t.Cleanup(aOutboxReg.Stop)
	aResolver := outbox.NewResolver(pool, aOutboxReg, func(context.Context) bool { return false })
	// PRODUCTION DEFAULTS. LISTEN/NOTIFY on activities
	// (migration 00005) is the primary wake signal; 30s ticker
	// is correctness backstop only.
	aOutboxDispatcher := outbox.NewDispatcher(
		outbox.DefaultDispatcherConfig(),
		pool, aResolver, logger,
	)
	aOutboxDispatcher.SetVisibilityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		return outbox.VisibilityExplicitShare, nil
	})
	go aOutboxDispatcher.Run(ctx)

	// Delivery worker on A — PRODUCTION DEFAULTS. LISTEN/NOTIFY
	// on federation_outbox (migration 00006) is the primary
	// wake signal; 30s ticker is correctness backstop only.
	aSigner := &outbox.IdentitySigner{PrivateKey: aPriv, KeyURL: aKeyURL}
	aDelivery := outbox.NewWorker(
		outbox.DefaultDeliveryConfig(),
		pool, aSigner,
		func(_ context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			return outbox.PeerInfo{ID: id, InstanceURL: bSrv.URL, Enabled: true, Connected: true}, nil
		},
		logger,
	)
	go aDelivery.Run(ctx)

	// Wait for the LISTEN goroutines (outbox dispatcher's +
	// inbox dispatcher's + delivery's) to ESTABLISH their
	// dedicated pool connections + LISTEN. Without this, a
	// NOTIFY fired immediately after the goroutine start can
	// land before LISTEN is armed and the 30s ticker backstop
	// becomes the only signal. 500ms is empirically sufficient
	// on the test rig.
	time.Sleep(500 * time.Millisecond)

	// --- THE DEMO ---
	// Alice (Studio A) emits a Like activity on B's post. The
	// activity ledger row triggers NOTIFY → outbox dispatcher
	// → delivery worker → POST to B → B's inbox dispatcher →
	// likes row on B's side.
	startedAt := time.Now()
	var likeActivityID uuid.UUID
	bPostIDStr := bPostID.String()
	likeURI := aBaseURL + "/activities/" + uuid.NewString()
	likeObjectURI := bSrv.URL + "/posts/" + bPostIDStr
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
		    object_uri, object_kind, object_local_id, to_uris, payload)
		 VALUES ($1, 'Like', $2, $3, $4, 'post', $5, '[]'::jsonb, '{}'::jsonb)
		 RETURNING id`,
		likeURI, aBaseURL+"/users/alice", aliceRef,
		likeObjectURI, bPostIDStr,
	).Scan(&likeActivityID); err != nil {
		t.Fatalf("emit Like: %v", err)
	}

	// Poll for the likes row on B's side. Hard deadline: 1s.
	deadline := time.Now().Add(1 * time.Second)
	var likeCount int
	for time.Now().Before(deadline) {
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM likes WHERE target_kind = 'post' AND target_id = $1 AND peer_id = $2`,
			bPostID, aPeerID,
		).Scan(&likeCount)
		if likeCount > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	elapsed := time.Since(startedAt)

	if likeCount != 1 {
		// Diagnostic dump on failure.
		dumpDiag(t, pool, likeURI, aPeerID, bPeerID, bPostID)
		t.Fatalf("paired-peer Like did not arrive within 1s; elapsed=%v likeCount=%d", elapsed, likeCount)
	}
	t.Logf("paired-peer Like delivered + materialised in %v (target: under 1s)", elapsed)
}

// --- helpers ---------------------------------------------------------

func openE2EPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	host := envOrE2E("AA_DB_HOST", "postgres")
	port := envOrE2E("AA_DB_PORT", "5432")
	user := envOrE2E("AA_DB_USER", "artist_alley")
	name := envOrE2E("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOrE2E(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}

func seedUserAndPost(t *testing.T, pool *pgxpool.Pool, username string) (int64, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var userRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $1, 1) RETURNING ref`,
		username,
	).Scan(&userRef); err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, $3, 'explicit-share')`,
		postID, userRef, "Post for "+username,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	return userRef, postID
}

type e2eByKeyID struct {
	keyID   string
	peerID  uuid.UUID
	peerURL string
	pubKey  ed25519.PublicKey
}

func (l *e2eByKeyID) ByKeyID(_ context.Context, keyID string) (inbox.PeerInfo, error) {
	// Accept either the exact stored keyID or a variant where
	// the suffix differs (httpsig URL parsing can normalise).
	if keyID == l.keyID || strings.HasPrefix(keyID, l.peerURL) {
		return inbox.PeerInfo{
			ID: l.peerID, InstanceURL: l.peerURL,
			InstancePublicKey: l.pubKey,
			Enabled: true, Connected: true,
		}, nil
	}
	return inbox.PeerInfo{}, inbox.ErrPeerNotFound
}

type e2eSocialPoster struct{ h *social.Handler }

func (a *e2eSocialPoster) InsertRemoteLike(ctx context.Context, kind string, id, peerID uuid.UUID, actorURI string) (bool, error) {
	return a.h.InsertRemoteLike(ctx, kind, id, peerID, actorURI)
}

func (a *e2eSocialPoster) InsertRemoteComment(ctx context.Context, in inbox.RemoteCommentInput) (uuid.UUID, bool, error) {
	return a.h.InsertRemoteComment(ctx, social.RemoteCommentInput{
		TargetKind: in.TargetKind, TargetID: in.TargetID, ParentID: in.ParentID,
		PeerID: in.PeerID, ActorURI: in.ActorURI, ActivityURI: in.ActivityURI,
		Body: in.Body,
	})
}

// dumpDiag prints diagnostic state on test failure so future
// debugging starts from a complete picture.
func dumpDiag(t *testing.T, pool *pgxpool.Pool, likeURI string, aPeerID, bPeerID, bPostID uuid.UUID) {
	ctx := context.Background()
	var info []string
	rows, _ := pool.Query(ctx,
		`SELECT activity_id, peer_id, status, attempts, last_error
		 FROM federation_outbox WHERE peer_id = $1`, bPeerID)
	for rows.Next() {
		var aID, pID uuid.UUID
		var status, lastErr string
		var attempts int
		_ = rows.Scan(&aID, &pID, &status, &attempts, &lastErr)
		info = append(info, fmt.Sprintf("outbox{a=%s p=%s status=%s att=%d err=%q}", aID, pID, status, attempts, lastErr))
	}
	rows.Close()
	rows2, _ := pool.Query(ctx,
		`SELECT activity_uri, status, reject_reason, last_error FROM federation_inbox WHERE peer_id = $1`,
		aPeerID)
	for rows2.Next() {
		var uri, status string
		var rr, le *string
		_ = rows2.Scan(&uri, &status, &rr, &le)
		info = append(info, fmt.Sprintf("inbox{uri=%s status=%s rr=%v le=%v}", uri, status, rr, le))
	}
	rows2.Close()
	for _, s := range info {
		t.Log(s)
	}
	_ = http.StatusOK
	_ = httptest.NewServer
	_ = sync.Mutex{}
}
