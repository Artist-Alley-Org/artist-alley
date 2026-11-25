// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
// distinguish A-side rows from B-side rows via peer_id. Two
// independent instances are internal/federation/twostack's
// proof, not this test's.
//
// # What is measured (#1577 rev800)
//
// The Like is a valid remote Like (#1611): it names the liked
// post's URI on B and B's peer row, it is addressed to the post's
// owner on B, and A holds a live view grant received from B for
// that post and that liker (inboundgrants.Store.ApplyShare, checked
// by the production Store.Allows adapter boot installs). B's own
// share of the post to A is what B's inbound gate checks. Every
// identity is the seeded one: the liker's /users/<username> URI on
// A, signed with the key B resolves for exactly that actor.
//
// The clock starts immediately before the Like is written to the
// local ledger and committed, and only after the fixture's own work
// is drained by the production dispatcher and each of the three
// workers has been SEEN (a test-only query tracer on their tagged
// contexts) to have armed its LISTEN and finished an EMPTY boot
// scan, so no boot scan and no scan already running can pick the
// Like up: it reaches the receiver only through the NOTIFY chain
// (or the 30s backstop, which fails the 1s budget). One absolute
// deadline, startedAt+1s, bounds every observation, and the pass
// condition is exactly one Like of that post by that actor on B,
// bound to that activity's id.
//
// Requires a dedicated, disposable test database (scripts/test.sh
// forwards AA_E2E_ISOLATED into its test container; the database it
// creates is the isolation). Skips without AA_DB_PASSWORD.

package federation_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/social"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// TestFederation_EndToEnd_ProductionDefaults_SubSecond is the
// latency-contract test for spec §3.5. PRODUCTION tick intervals
// on every layer (no fixture tuning); LISTEN/NOTIFY is the
// load-bearing latency primitive on all three queues
// (activities, federation_outbox, federation_inbox).
//
// Hard 1-second budget from the local ledger write of the Like to
// the receiver-side Like row materialising.
//
// If this test fails, do NOT tune the intervals to make it
// pass — that would silently lower the gold-standard guarantee.
// Instead: investigate the LISTEN/NOTIFY chain (triggers in
// migration 00001, SetRawPool wiring in api.go,
// dispatcher/delivery wake-channel plumbing).
func TestFederation_EndToEnd_ProductionDefaults_SubSecond(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	// The three workers below scan whole queues, so this test needs
	// a database no other process dispatches from: the disposable test
	// database scripts/test.sh creates (it forwards AA_E2E_ISOLATED=1
	// into the test container). Missing → skip with a clear reason.
	if os.Getenv("AA_E2E_ISOLATED") == "" {
		t.Skip("AA_E2E_ISOLATED not set; skipping latency-contract test " +
			"(it needs a dedicated disposable test database: run it through " +
			"scripts/test.sh, which sets and forwards AA_E2E_ISOLATED=1)")
	}
	obs := newE2EObserver()
	pool := openE2EPool(t, obs)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	var bInboxHandler *inbox.Handler
	bRouter := chi.NewRouter()
	bRouter.Post("/federation/inbox", func(w http.ResponseWriter, r *http.Request) { bInboxHandler.PostInbox(w, r) })
	bRouter.Post("/federation/inbox/batch", func(w http.ResponseWriter, r *http.Request) { bInboxHandler.PostInboxBatch(w, r) })
	// Over TLS: federation URIs are https (federation.OriginOf), so a
	// Like of a post on a plain-http receiver has no recipient. The
	// delivery worker trusts B's test certificate the way production
	// trusts a peer's, through the system roots: SSL_CERT_FILE names the
	// httptest root, as internal/federation/twostack's TestMain does. It
	// is set before anything in this process verifies a certificate, and
	// checked below, before the clock.
	bSrv := httptest.NewTLSServer(bRouter)
	t.Cleanup(bSrv.Close)
	bBaseURL := bSrv.URL
	rootPath := filepath.Join(t.TempDir(), "httptest-root.pem")
	if err := os.WriteFile(rootPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bSrv.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write the httptest root: %v", err)
	}
	t.Setenv("SSL_CERT_FILE", rootPath)

	// --- Studio A (sender) keypair, used by both A's outbox
	// signer + B's inbox HTTP-Sig verifier --------------------
	aPub, aPriv, _ := ed25519.GenerateKey(rand.Reader)
	aBaseURL := "https://studio-a-" + randHex(4) + ".example"
	aKeyURL := aBaseURL + "/federation/instance#main-key"

	// --- Users: B's post owner, and the liker on A. Their actor
	// URIs are their real /users/<username> URIs on their instance.
	bOwnerName := "e2e-b-owner-" + randHex(4)
	bOwnerRef, bPostID := seedUserAndPost(t, pool, bOwnerName)
	likerName := "e2e-a-liker-" + randHex(4)
	likerRef, _ := seedUserAndPost(t, pool, likerName)
	bOwnerURI := bBaseURL + "/users/" + bOwnerName
	likerURI := aBaseURL + "/users/" + likerName
	postURI := bBaseURL + "/posts/" + bPostID.String()

	// --- Peers. B's record of A (A's pubkey from the prior
	// handshake), and A's record of B (target: the httptest server).
	var aPeerID, bPeerID uuid.UUID
	aPubPEM, _ := federation.PublicKeyToPEM(aPub)
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Studio A', $2, 'connected', 'plaintext', TRUE, 'connected', $3)
		 RETURNING id`,
		aBaseURL, string(aPubPEM), bOwnerRef,
	).Scan(&aPeerID); err != nil {
		t.Fatalf("seed peer A: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Studio B', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		bBaseURL, likerRef,
	).Scan(&bPeerID); err != nil {
		t.Fatalf("seed peer B: %v", err)
	}

	// Cleanup AFTER seeding so the IDs are in scope. It runs after
	// the workers are stopped and joined (registered later, so run
	// earlier), so no worker writes while these delete.
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id IN ($1, $2)`, aPeerID, bPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_inbox WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_remote_actors WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_remote_likes WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id IN ($1, $2)`, aPeerID, bPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_inbound_grants WHERE peer_id = $1`, bPeerID)
		_, _ = pool.Exec(c, `DELETE FROM likes WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM comments WHERE peer_id = $1`, aPeerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref IN ($1, $2)`, likerRef, bOwnerRef)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id IN ($1, $2)`, aPeerID, bPeerID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE author_user_ref IN ($1, $2)`, likerRef, bOwnerRef)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref IN ($1, $2)`, likerRef, bOwnerRef)
	})

	writer := activities.NewWriter(pool, logger, nil)

	// --- B's inbound gate (#1570): B's own live, peer-wide view share
	// of its post to the peer it calls A. federation_shares requires
	// the share's granting activity; it is recorded here as a local
	// aa:Share that names no share row (this single-DB fixture's FK
	// record, not a grant B sends), so the production dispatcher skips
	// it in the predrain below and it never reaches the outbox.
	var bShareActivity *activities.Record
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ownerRef := bOwnerRef
		var err error
		bShareActivity, err = writer.RecordActivity(ctx, tx, activities.Input{
			Type:         federation.ActivityAAShare,
			ActivityURI:  bBaseURL + "/activities/" + uuid.NewString(),
			ActorUserRef: &ownerRef,
			ActorURI:     bOwnerURI,
			Object:       &activities.ObjectRef{URI: postURI, Kind: activities.ObjectKindPost, LocalID: bPostID.String()},
			Payload:      map[string]any{"fixture": "single-DB inbound share FK record"},
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id)
			 VALUES ($1, 'post', $2, $3, 'view', $4)`,
			bOwnerRef, bPostID, aPeerID, bShareActivity.ID)
		return err
	}); err != nil {
		t.Fatalf("seed B's inbound share: %v", err)
	}

	// --- A's received grant (#1611): the live view grant B gave the
	// liker on its post, applied by the real inbound grant store, and
	// the production Allows adapter api.go installs on the resolver.
	aGrants := inboundgrants.New(pool, func(context.Context) string { return aBaseURL }, nil, logger)
	applied, err := aGrants.ApplyShare(ctx, inboundgrants.Share{
		PeerID:          bPeerID,
		ActivityURI:     bBaseURL + "/activities/" + uuid.NewString(),
		GrantorActorURI: bOwnerURI,
		ObjectKind:      federation.ShareObjectKindPost,
		ObjectURI:       postURI,
		TargetUserURI:   likerURI,
		Scope:           federation.ShareScopeView,
	})
	if err != nil || applied.Outcome != inboundgrants.OutcomeLive {
		t.Fatalf("apply A's received grant: %+v %v", applied, err)
	}
	if _, err := aGrants.Allows(ctx, inboundgrants.Query{PeerID: bPeerID, ActorURI: likerURI, ObjectURI: postURI}); err != nil {
		t.Fatalf("A's received grant does not cover the Like: %v", err)
	}
	receivedGrant := func(ctx context.Context, peerID uuid.UUID, actorURI, objectURI string) (bool, error) {
		_, err := aGrants.Allows(ctx, inboundgrants.Query{PeerID: peerID, ActorURI: actorURI, ObjectURI: objectURI})
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, inboundgrants.ErrNoGrant):
			return false, nil
		}
		return false, err
	}

	// The liked post's public tier and visibility, for THIS post only:
	// the Like's object_local_id below is the single-DB local mirror of
	// B's post (see likeInput), which is what the dispatcher looks up.
	sensitivity := func(ctx context.Context, kind string, id uuid.UUID) (outbox.Sensitivity, error) {
		if kind != "post" || id != bPostID {
			return "", fmt.Errorf("e2e: no tier for %s %s", kind, id)
		}
		return outbox.SensitivityPublic, nil
	}
	visibility := func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		if kind != "post" || id != bPostID {
			return "", fmt.Errorf("e2e: no visibility for %s %s", kind, id)
		}
		var v string
		if err := pool.QueryRow(ctx, `SELECT visibility FROM posts WHERE id = $1`, id).Scan(&v); err != nil {
			return "", err
		}
		return outbox.Visibility(v), nil
	}
	skips := &e2eSkips{}
	newDispatcher := func(reg *cache.Registry) *outbox.Dispatcher {
		r := outbox.NewResolver(pool, reg, func(context.Context) bool { return false })
		r.SetReceivedGrants(receivedGrant)
		// PRODUCTION DEFAULTS. LISTEN/NOTIFY on activities
		// (migration 00001) is the primary wake signal; 30s ticker
		// is correctness backstop only.
		d := outbox.NewDispatcher(outbox.DefaultDispatcherConfig(), pool, r, logger)
		d.SetVisibilityLookup(visibility)
		d.SetSensitivityLookup(sensitivity)
		d.SetSkippedAudit(skips.record)
		return d
	}
	aOutboxReg := cache.NewRegistry(pool, logger)
	t.Cleanup(aOutboxReg.Stop)

	// --- Predrain, OUTSIDE the clock: the production dispatcher
	// handles every pending local activity (this fixture's own setup
	// record, and any history), so the measured workers start on empty
	// queues. Bounded; the settled state is asserted, never forced.
	pre := newDispatcher(aOutboxReg)
	settled := false
	for range 200 {
		if n, _, _ := pre.RunOnce(ctx); n == 0 {
			settled = true
			break
		}
	}
	backlog := e2eBacklog(ctx, pool)
	if !settled || backlog.err != nil || backlog.work != 0 || backlog.dueOutbox != 0 || backlog.pendingInbox != 0 {
		t.Fatalf("SETUP: the database did not settle before the clock: drained=%v work=%d due outbox=%d pending inbox=%d err=%v",
			settled, backlog.work, backlog.dueOutbox, backlog.pendingInbox, backlog.err)
	}

	// The system roots trust B (a GET of a POST-only route: no side
	// effect, 405 from the router), through a fresh default transport.
	probe := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	resp, err := probe.Get(bBaseURL + "/federation/inbox")
	if err != nil {
		t.Fatalf("SETUP: B's certificate is not trusted through the system roots: %v", err)
	}
	_ = resp.Body.Close()
	probe.CloseIdleConnections()

	// --- Studio B: inbox handler + dispatcher. lookupPeer maps A's
	// HTTP-Sig keyID to A's seeded peer row; the receiver's base URL is
	// the server's own, so the object host check runs.
	bInboxHandler = inbox.NewHandler(inbox.HandlerDeps{
		Pool:         inbox.New(pool),
		Lookup:       &e2eByKeyID{keyID: aKeyURL, peerID: aPeerID, peerURL: aBaseURL, pubKey: aPub},
		Logger:       logger,
		LocalBaseURL: func(context.Context) string { return bBaseURL },
	})
	// #1526: the liker on A signs with this key; B verifies against it
	// (a stub resolver stands in for the actor-document fetch, which
	// the http production-wiring tests cover). Exactly that actor.
	likerSignPub, likerSignPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bInboxHandler.SetActorKeyResolver(e2eActorKeys{likerURI: likerSignPub})
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
	bInboxDispatcher.SetSocialPoster(&e2eSocialPoster{h: bSocial, shares: shares.NewRegistry(pool, logger, bSocialReg)})
	bInboxDispatcher.SetRemoteActorUpserter(remote.NewUpserter(pool, nil, nil, logger))
	bInboxDispatcher.SetRegistry(inbox.BuildRegistry(bInboxDispatcher, logger))
	bInboxDispatcher.SetRawPool(pool) // ← LISTEN federation_inbox_pending

	// --- Studio A: outbox dispatcher + delivery worker.
	aOutboxDispatcher := newDispatcher(aOutboxReg)
	// Delivery worker on A — PRODUCTION DEFAULTS. LISTEN/NOTIFY
	// on federation_outbox (migration 00001) is the primary
	// wake signal; 30s ticker is correctness backstop only.
	aSigner := &outbox.IdentitySigner{PrivateKey: aPriv, KeyURL: aKeyURL}
	aDelivery := outbox.NewWorker(
		outbox.DefaultDeliveryConfig(),
		pool, aSigner,
		func(_ context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			if id != bPeerID {
				return outbox.PeerInfo{}, fmt.Errorf("e2e: unknown peer %s", id)
			}
			return outbox.PeerInfo{ID: id, InstanceURL: bBaseURL, Enabled: true, Connected: true}, nil
		},
		logger,
	)
	likerSignPEM, err := federation.PrivateKeyToPEM(likerSignPriv)
	if err != nil {
		t.Fatal(err)
	}
	aDelivery.SetActorSigningKey(func(_ context.Context, ref int64, actorURI string) ([]byte, error) {
		if ref != likerRef || actorURI != likerURI {
			return nil, fmt.Errorf("e2e: no signing key for actor %d %q", ref, actorURI)
		}
		return append([]byte(nil), likerSignPEM...), nil
	})
	aDelivery.SetSensitivityLookup(sensitivity)

	// --- Start the three production Run loops, each on a context
	// tagged with its identity for the observer. Stopped and JOINED
	// before any fixture cleanup (this cleanup is registered last, so
	// it runs first), even when an assertion fails.
	runCtx, cancelRun := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, w := range []struct {
		tag string
		run func(context.Context)
	}{
		{e2eWorkerOutbox, aOutboxDispatcher.Run},
		{e2eWorkerDelivery, aDelivery.Run},
		{e2eWorkerInbox, bInboxDispatcher.Run},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(context.WithValue(runCtx, e2eWorkerKey{}, w.tag))
		}()
	}
	var stopOnce sync.Once
	stopWorkers := func() { stopOnce.Do(func() { cancelRun(); wg.Wait() }) }
	t.Cleanup(stopWorkers)

	// Readiness: each worker's LISTEN acknowledged and its boot scan
	// seen to return nothing (the outbox claim also rolled back), so
	// nothing that ran before the clock can deliver the Like.
	select {
	case <-obs.ready:
	case <-obs.bad:
		stopWorkers()
		t.Fatalf("SETUP: a worker was not ready on an empty queue: %s", obs.state())
	case <-ctx.Done():
		stopWorkers()
		t.Fatalf("SETUP: the workers were not seen ready: %s", obs.state())
	}
	t.Logf("READY: %s", obs.state())

	// --- THE DEMO ---
	// The liker on A likes B's post. The ledger row's NOTIFY wakes the
	// outbox dispatcher → outbox row (NOTIFY) → delivery worker → POST
	// to B → inbox row (NOTIFY) → B's inbox dispatcher → Like row.
	likeURI := aBaseURL + "/activities/" + uuid.NewString()
	in := likeInput(likerRef, likerURI, likeURI, postURI, bPostID, bOwnerURI, bPeerID)
	startedAt := time.Now()
	deadline := startedAt.Add(time.Second)
	var like *activities.Record
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		like, err = writer.RecordActivity(ctx, tx, in)
		return err
	}); err != nil {
		stopWorkers()
		t.Fatalf("record the Like: %v", err)
	}

	// Observe, inside the one absolute deadline, exactly the Like of
	// that post by that actor, bound to that activity id. Checked
	// right away and after every query a worker completes.
	dctx, dcancel := context.WithDeadline(ctx, deadline)
	var (
		found      bool
		elapsed    time.Duration
		likes      int
		bound      int
		queryErr   error
		overcounts []string
	)
	check := func() bool {
		err := pool.QueryRow(dctx, `SELECT
			(SELECT count(*) FROM likes WHERE target_kind = 'post' AND target_id = $1 AND peer_id = $2 AND actor_uri = $3),
			(SELECT count(*) FROM federation_remote_likes WHERE peer_id = $2 AND actor_uri = $3 AND activity_uri = $4
			    AND target_kind = 'post' AND target_id = $1 AND binding = 'received')`,
			bPostID, aPeerID, likerURI, likeURI).Scan(&likes, &bound)
		if err != nil {
			if dctx.Err() == nil {
				queryErr = err
			}
			return false
		}
		if likes > 1 || bound > 1 {
			overcounts = append(overcounts, fmt.Sprintf("likes=%d bound=%d", likes, bound))
		}
		return likes == 1 && bound == 1
	}
wait:
	for {
		if check() {
			found, elapsed = true, time.Since(startedAt)
			break
		}
		select {
		case <-obs.progress:
		case <-dctx.Done():
			break wait
		}
	}
	if !found {
		elapsed = time.Since(startedAt)
	}
	dcancel()
	stopWorkers()

	if !found || elapsed >= time.Second || queryErr != nil || len(overcounts) != 0 {
		e2eDiag(t, pool, like.ID, likeURI, likerURI, aPeerID, bPeerID, bPostID)
		t.Logf("dispatcher skips: %s", skips.state())
		t.Logf("observer after: %s", obs.state())
		t.Fatalf("LATENCY CONTRACT: the paired-peer Like did not arrive within 1s: found=%v elapsed=%v likes=%d bound=%d query error=%v overcounts=%v",
			found, elapsed, likes, bound, queryErr, overcounts)
	}
	t.Logf("observer after: %s", obs.state())
	t.Logf("paired-peer Like delivered + materialised in %v (target: under 1s)", elapsed)
}

// likeInput is the measured Like as the local ledger records a remote
// Like (emit.Like's remote branch, #1611): its object is the post's URI
// on B, addressed to the post's owner there, naming the target and the
// owning peer. FIXTURE: Object.LocalID is the single-DB local mirror of
// B's post (both halves share one database), which emit.Like leaves
// empty for a remote post; the dispatcher looks the tier up by it,
// public for exactly that post. Without it the tier is unresolved and,
// with no encryption here, the production dispatcher correctly refuses
// to send. This is a latency fixture, not proof of the emitter or the
// UI; internal/federation/twostack covers those across two instances.
func likeInput(likerRef int64, likerURI, likeURI, postURI string, postID uuid.UUID, ownerURI string, ownerPeer uuid.UUID) activities.Input {
	ref := likerRef
	return activities.Input{
		Type:         federation.ActivityLike,
		ActivityURI:  likeURI,
		ActorUserRef: &ref,
		ActorURI:     likerURI,
		Object:       &activities.ObjectRef{URI: postURI, Kind: activities.ObjectKindPost, LocalID: postID.String()},
		To:           []string{ownerURI},
		Payload: map[string]any{
			"target_uri":     postURI,
			"target_peer_id": ownerPeer.String(),
		},
	}
}

// --- observer ---------------------------------------------------------

// The three measured workers, as their Run contexts are tagged.
const (
	e2eWorkerOutbox   = "outbox-dispatcher"
	e2eWorkerDelivery = "delivery-worker"
	e2eWorkerInbox    = "inbox-dispatcher"
)

type e2eWorkerKey struct{}

type e2eQueryKey struct{}

type e2eQuery struct {
	worker string
	sql    string
}

// e2eObserver is a test-only pgx.QueryTracer on the test's pool. It
// reads only the queries of the three tagged workers and changes
// nothing: no SQL, result or timing is altered and no worker is woken.
// It records, per worker, the successful LISTEN of its channel and the
// result of its first (boot) scan, and signals progress after every
// worker query so the test re-checks the receiver.
type e2eObserver struct {
	mu         sync.Mutex
	listen     map[string]bool
	boot       map[string]string // worker → "empty" or why not
	claimEmpty bool              // the outbox boot claim returned nothing
	rolledBack bool              // ... and its transaction ended
	scans      map[string]int
	errs       []string
	ready      chan struct{}
	readyOnce  sync.Once
	bad        chan struct{}
	badOnce    sync.Once
	progress   chan struct{}
}

func newE2EObserver() *e2eObserver {
	return &e2eObserver{
		listen: map[string]bool{}, boot: map[string]string{}, scans: map[string]int{},
		ready: make(chan struct{}), bad: make(chan struct{}), progress: make(chan struct{}, 1),
	}
}

// The workers' LISTEN statements and scan queries (sqlc names).
var (
	e2eListen = map[string]string{
		e2eWorkerOutbox:   "LISTEN federation_dispatch_pending",
		e2eWorkerDelivery: "LISTEN federation_outbox_pending",
		e2eWorkerInbox:    "LISTEN federation_inbox_pending",
	}
	e2eScan = map[string]string{
		e2eWorkerOutbox:   "-- name: ClaimDispatchWork :many",
		e2eWorkerDelivery: "-- name: ListDueOutbox :many",
		e2eWorkerInbox:    "-- name: ListPendingInbox :many",
	}
)

func (o *e2eObserver) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	worker, _ := ctx.Value(e2eWorkerKey{}).(string)
	if worker == "" {
		return ctx
	}
	return context.WithValue(ctx, e2eQueryKey{}, e2eQuery{worker: worker, sql: data.SQL})
}

func (o *e2eObserver) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	q, ok := ctx.Value(e2eQueryKey{}).(e2eQuery)
	if !ok {
		return
	}
	defer func() {
		select {
		case o.progress <- struct{}{}:
		default:
		}
	}()
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case q.sql == e2eListen[q.worker]:
		if data.Err != nil {
			o.errs = append(o.errs, q.worker+" LISTEN: "+data.Err.Error())
			return
		}
		o.listen[q.worker] = true
	case strings.Contains(q.sql, e2eScan[q.worker]):
		o.scans[q.worker]++
		if o.scans[q.worker] != 1 {
			return
		}
		switch {
		case data.Err != nil:
			o.boot[q.worker] = "error: " + data.Err.Error()
		case data.CommandTag.RowsAffected() != 0:
			o.boot[q.worker] = fmt.Sprintf("not empty: %s", data.CommandTag.String())
		default:
			o.boot[q.worker] = "empty"
			if q.worker == e2eWorkerOutbox {
				o.claimEmpty = true
			}
		}
		if o.boot[q.worker] != "empty" {
			o.badOnce.Do(func() { close(o.bad) })
			return
		}
	case q.worker == e2eWorkerOutbox && o.claimEmpty && !o.rolledBack && (q.sql == "rollback" || q.sql == "commit"):
		if data.Err != nil {
			o.errs = append(o.errs, "outbox boot "+q.sql+": "+data.Err.Error())
			return
		}
		o.rolledBack = true
	default:
		return
	}
	if o.isReady() {
		o.readyOnce.Do(func() { close(o.ready) })
	}
}

// isReady reports, with o.mu held, whether every listener is armed and
// every boot scan finished empty.
func (o *e2eObserver) isReady() bool {
	for _, w := range []string{e2eWorkerOutbox, e2eWorkerDelivery, e2eWorkerInbox} {
		if !o.listen[w] || o.boot[w] != "empty" {
			return false
		}
	}
	return o.rolledBack && len(o.errs) == 0
}

func (o *e2eObserver) state() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return fmt.Sprintf("listen=%v boot=%v outbox boot claim ended=%v scans=%v errors=%v", o.listen, o.boot, o.rolledBack, o.scans, o.errs)
}

// e2eSkips records the dispatchers' skip decisions (the production
// audit hook, wired to a test sink), for the failure diagnostics.
type e2eSkips struct {
	mu   sync.Mutex
	rows []string
}

func (s *e2eSkips) record(_ context.Context, activityID, activityType, objectKind, objectID, visibility, sensitivity, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) < 20 {
		s.rows = append(s.rows, fmt.Sprintf("%s %s %s/%s vis=%s sen=%s reason=%s", activityID, activityType, objectKind, objectID, visibility, sensitivity, reason))
	}
}

func (s *e2eSkips) state() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("%v", s.rows)
}

// e2eBacklogState is what the measured workers would find at boot.
type e2eBacklogState struct {
	work, dueOutbox, pendingInbox int
	err                           error
}

func e2eBacklog(ctx context.Context, pool *pgxpool.Pool) e2eBacklogState {
	var b e2eBacklogState
	b.err = pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM federation_dispatch_work),
		(SELECT count(*) FROM federation_outbox WHERE status = 'queued' AND next_attempt_at <= now()),
		(SELECT count(*) FROM federation_inbox WHERE status = 'pending')`).Scan(&b.work, &b.dueOutbox, &b.pendingInbox)
	return b
}

// --- helpers ---------------------------------------------------------

// openE2EPool is the test's pool, with the observer as its query tracer
// and room for the three LISTEN connections beside the workers' scans,
// the HTTP handler and the test's own queries.
func openE2EPool(t *testing.T, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	host := envOrE2E("AA_DB_HOST", "postgres")
	port := envOrE2E("AA_DB_PORT", "5432")
	user := envOrE2E("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("pool config: unparseable connection settings")
	}
	cfg.MaxConns = 20
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var got string
	if err := pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&got); err != nil || got != name {
		t.Fatalf("pool: connected to %q (%v), want %q", got, err, name)
	}
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
			Enabled:           true, Connected: true,
		}, nil
	}
	return inbox.PeerInfo{}, inbox.ErrPeerNotFound
}

// e2eSocialPoster mirrors the api.go adapter with the real share gate
// (#1570).
type e2eSocialPoster struct {
	h      *social.Handler
	shares *shares.Registry
}

func (a *e2eSocialPoster) authorizer(verb federation.ActivityType, kind string, id, peerID uuid.UUID, actor string) social.InboundAuthorizer {
	return func(ctx context.Context, tx pgx.Tx) error {
		err := a.shares.AuthorizeInboundWrite(ctx, tx, shares.InboundWrite{
			Verb: verb, PeerID: peerID, ActorURI: actor, TargetKind: federation.ShareObjectKind(kind), TargetID: id})
		switch {
		case errors.Is(err, shares.ErrInboundUnknownTarget):
			return fmt.Errorf("%w: %w", inbox.ErrUnknownObject, err)
		case errors.Is(err, shares.ErrInboundUnshared):
			return fmt.Errorf("%w: %w", inbox.ErrUnsharedObject, err)
		}
		return err
	}
}

func (a *e2eSocialPoster) InsertRemoteLike(ctx context.Context, kind string, id, peerID uuid.UUID, actorURI, activityURI string) (bool, error) {
	return a.h.InsertRemoteLike(ctx, kind, id, peerID, actorURI, activityURI, a.authorizer(federation.ActivityLike, kind, id, peerID, actorURI))
}

func (a *e2eSocialPoster) UpdateRemoteComment(ctx context.Context, in inbox.RemoteUpdateCommentInput) (bool, error) {
	return a.h.UpdateRemoteComment(ctx, social.RemoteUpdateCommentInput{PeerID: in.PeerID, ActorURI: in.ActorURI,
		CreateActivityURI: in.CreateActivityURI, UpdateActivityURI: in.UpdateActivityURI, Content: in.Content, Published: in.Published,
		Authorize: func(kind string, id uuid.UUID) social.InboundAuthorizer {
			return a.authorizer(federation.ActivityCreate, kind, id, in.PeerID, in.ActorURI)
		}})
}

func (a *e2eSocialPoster) DeleteRemoteComment(ctx context.Context, in inbox.RemoteDeleteCommentInput) (bool, error) {
	return a.h.DeleteRemoteComment(ctx, social.RemoteDeleteCommentInput{PeerID: in.PeerID, ActorURI: in.ActorURI,
		CreateActivityURI: in.CreateActivityURI, DeleteActivityURI: in.DeleteActivityURI})
}

func (a *e2eSocialPoster) UndoRemoteLike(ctx context.Context, in inbox.RemoteUndoLikeInput) (bool, error) {
	return a.h.UndoRemoteLike(ctx, social.RemoteUndoLikeInput{PeerID: in.PeerID, ActorURI: in.ActorURI,
		LikeActivityURI: in.LikeActivityURI, UndoActivityURI: in.UndoActivityURI})
}

func (a *e2eSocialPoster) InsertRemoteComment(ctx context.Context, in inbox.RemoteCommentInput) (uuid.UUID, bool, error) {
	return a.h.InsertRemoteComment(ctx, social.RemoteCommentInput{
		TargetKind: in.TargetKind, TargetID: in.TargetID, ParentID: in.ParentID, ParentActivityURI: in.ParentActivityURI,
		PeerID: in.PeerID, ActorURI: in.ActorURI, ActivityURI: in.ActivityURI,
		Body: in.Body, Authorize: a.authorizer(federation.ActivityCreate, in.TargetKind, in.TargetID, in.PeerID, in.ActorURI),
	})
}

// e2eDiag logs, after a failure, where the measured Like stopped: its
// ledger row and dispatch work, its outbox rows, the receiver's inbox
// rows for its id, the receiver's Like and binding rows, and the grants.
// Every query is bounded, checked (an error is reported, never read as
// zero) and NULL-safe; no payload, key or credential is printed.
func e2eDiag(t *testing.T, pool *pgxpool.Pool, likeID uuid.UUID, likeURI, likerURI string, aPeerID, bPeerID, postID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rowsOf := func(label, sql string, args ...any) {
		rows, err := pool.Query(ctx, sql, args...)
		if err != nil {
			t.Logf("diag %s: query error: %v", label, err)
			return
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				t.Logf("diag %s: scan error: %v", label, err)
				return
			}
			n++
			t.Logf("diag %s: %v", label, vals)
		}
		if err := rows.Err(); err != nil {
			t.Logf("diag %s: rows error: %v", label, err)
			return
		}
		if n == 0 {
			t.Logf("diag %s: none", label)
		}
	}
	rowsOf("activity", `SELECT activity_type, source, object_kind, object_local_id, created_at FROM activities WHERE id = $1`, likeID)
	rowsOf("dispatch work", `SELECT activity_id, activity_created_at FROM federation_dispatch_work WHERE activity_id = $1`, likeID)
	rowsOf("dispatch cursor", `SELECT last_dispatched_activity_id, last_dispatched_at FROM federation_dispatch_state WHERE id = 1`)
	rowsOf("outbox", `SELECT peer_id, status, attempts, sensitivity, last_error, refused_reason, created_at, sent_at
		FROM federation_outbox WHERE activity_id = $1 ORDER BY created_at LIMIT 10`, likeID)
	rowsOf("inbox", `SELECT peer_id, status, reject_reason, last_error, received_at, processed_at
		FROM federation_inbox WHERE activity_uri = $1 ORDER BY received_at LIMIT 10`, likeURI)
	rowsOf("likes", `SELECT peer_id, actor_uri, liked_at FROM likes WHERE target_kind = 'post' AND target_id = $1 LIMIT 10`, postID)
	rowsOf("remote like binding", `SELECT binding, target_kind, target_id, received_at FROM federation_remote_likes
		WHERE peer_id = $1 AND actor_uri = $2 AND activity_uri = $3 LIMIT 10`, aPeerID, likerURI, likeURI)
	rowsOf("received grant", `SELECT state, scope, object_kind, end_reason FROM federation_inbound_grants WHERE peer_id = $1 LIMIT 10`, bPeerID)
	rowsOf("B inbound share", `SELECT object_kind, scope, target_user_url IS NULL, revoked_at FROM federation_shares
		WHERE peer_id = $1 AND object_id = $2 LIMIT 10`, aPeerID, postID)
}

// e2eActorKeys is a fixed ActorKeyResolver for the e2e test.
type e2eActorKeys map[string]ed25519.PublicKey

func (k e2eActorKeys) SigningKey(_ context.Context, _ inbox.PeerInfo, actorURI string) (ed25519.PublicKey, error) {
	if pub, ok := k[actorURI]; ok {
		return pub, nil
	}
	return nil, inbox.ErrActorUnknown
}
