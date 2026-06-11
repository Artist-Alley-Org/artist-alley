// Paired-peer end-to-end test per the §5.5 Q1 lock-in.
// Phase 1.22.D-a-5.
//
// # What this test proves
//
// Two paired instances exchange a Like + a Comment through
// the full wire layer in a single process: the receiver is
// the live handler + dispatcher + social.Handler; the sender
// is a fixture that signs envelopes with a real ed25519
// keypair and POSTs them through the receiver's HTTP handler.
//
// The transport is httptest (no TCP), but everything from
// HTTP-Signature verify through the federation_inbox INSERT
// through the dispatcher's RunOnce through social.Handler's
// InsertRemoteLike + InsertRemoteComment + Notifier exercise
// runs against the real DB.
//
// # Assertions per round
//
// - federation_inbox row landed + transitioned pending → processed
// - federation_remote_actors row exists for the remote actor
//   with the display-hint payload
// - likes row exists with peer_id + actor_uri populated
//   (Like round) / comments row exists with peer_id + actor_uri
//   + activity_uri populated (Comment round)
// - spy Notifier was called with verb='post.like' / 'post.comment',
//   payload.is_remote=true, payload.actor_peer_id matches
//
// Skips without AA_DB_PASSWORD.

package inbox_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
	"github.com/mscrnt/artist-alley/app/internal/social"
)

// pairedFixture spans both halves of the paired-peer interaction:
// the receiver (live handler + dispatcher + social.Handler) and
// the sender (just a keypair + the helpers to build signed
// envelopes targeting the receiver's post).
type pairedFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	logger *slog.Logger

	// receiver side ("us" — Studio A)
	localBaseURL string
	postOwnerRef int64
	postID       uuid.UUID
	handler      *inbox.Handler
	dispatcher   *inbox.Dispatcher
	socialH      *social.Handler
	notifier     *spyNotifier

	// sender side ("them" — Studio B)
	senderPeerID  uuid.UUID
	senderBaseURL string
	senderKeyID   string
	senderPriv    ed25519.PrivateKey
	senderActor   string // https://senderBaseURL/users/bob
}

// spyNotifier records every Notify call so the test can assert
// on the notification firing.
type spyNotifier struct {
	mu      sync.Mutex
	entries []spyNotifyEntry
}

type spyNotifyEntry struct {
	recipient  int64
	actor      *int64
	verb       string
	targetKind string
	targetID   string
	payload    map[string]any
}

func (s *spyNotifier) Notify(_ context.Context, recipient int64, actor *int64, verb, kind, id string, payload map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, spyNotifyEntry{recipient, actor, verb, kind, id, payload})
	return nil
}

func (s *spyNotifier) last() *spyNotifyEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil
	}
	e := s.entries[len(s.entries)-1]
	return &e
}

func newPairedFixture(t *testing.T) *pairedFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openInboxTestPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// --- receiver-side fixtures ---
	localBaseURL := "https://studio-a-" + randHex(4) + ".example"

	// Alice (post owner) — local user.
	ctx := context.Background()
	aliceUsername := "alice-" + randHex(4)
	var aliceRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		aliceUsername, "Alice",
	).Scan(&aliceRef); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// One local post by Alice.
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, description, visibility)
		 VALUES ($1, $2, $3, $4, 'org-only')`,
		postID, aliceRef, "Alice's post", "test fixture",
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}

	// --- sender-side keypair + peer row ---
	senderBaseURL := "https://studio-b-" + randHex(4) + ".example"
	senderPub, senderPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	senderKeyID := senderBaseURL + "/instance#main-key"
	senderActor := senderBaseURL + "/users/bob"

	senderPubPEM, err := federation.PublicKeyToPEM(senderPub)
	if err != nil {
		t.Fatal(err)
	}
	var senderPeerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Studio B', $2, 'connected', 'plaintext', TRUE, 'connected', $3)
		 RETURNING id`,
		senderBaseURL, string(senderPubPEM), aliceRef,
	).Scan(&senderPeerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_remote_actors WHERE peer_id = $1`, senderPeerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_inbox WHERE peer_id = $1`, senderPeerID)
		_, _ = pool.Exec(c, `DELETE FROM likes WHERE peer_id = $1`, senderPeerID)
		_, _ = pool.Exec(c, `DELETE FROM comments WHERE peer_id = $1`, senderPeerID)
		_, _ = pool.Exec(c, `DELETE FROM notifications WHERE recipient_user_ref = $1`, aliceRef)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, senderPeerID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, aliceRef)
	})

	// --- wire receiver-side handler + dispatcher + social.Handler ---
	cacheReg := cache.NewRegistry(pool, logger)
	t.Cleanup(cacheReg.Stop)

	socialH := social.NewHandler(pool, logger, cacheReg)
	notifier := &spyNotifier{}
	socialH.SetNotifier(notifier)
	socialH.SetPostTargetLookup(func(ctx context.Context, postRef uuid.UUID) (int64, bool, error) {
		var ref int64
		err := pool.QueryRow(ctx,
			`SELECT author_user_ref FROM posts WHERE id = $1 AND deleted_at IS NULL`,
			postRef,
		).Scan(&ref)
		if err != nil {
			return 0, false, nil
		}
		return ref, true, nil
	})

	handler := inbox.NewHandler(inbox.HandlerDeps{
		Pool: inbox.New(pool),
		Lookup: &pairedPeerLookup{
			peerID:   senderPeerID,
			peerURL:  senderBaseURL,
			keyID:    senderKeyID,
			pubKey:   senderPub,
		},
		Logger:       logger,
		LocalBaseURL: func(context.Context) string { return localBaseURL },
	})

	dispatcher := inbox.NewDispatcher(
		inbox.DispatcherConfig{
			Interval:    time.Hour, // ticker never fires; test drives RunOnce
			BatchSize:   100,
			MaxAttempts: 5,
		},
		inbox.New(pool),
		func(ctx context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			return inbox.PeerInfo{
				ID:          id,
				InstanceURL: senderBaseURL,
				Enabled:     true,
				Connected:   true,
			}, nil
		},
		nil,
		logger,
	)
	dispatcher.SetSocialPoster(&socialPosterE2EAdapter{h: socialH})
	dispatcher.SetRemoteActorUpserter(remote.NewUpserter(pool, nil, nil, logger))
	dispatcher.SetRegistry(inbox.BuildRegistry(dispatcher, logger))

	return &pairedFixture{
		t:             t,
		pool:          pool,
		logger:        logger,
		localBaseURL:  localBaseURL,
		postOwnerRef:  aliceRef,
		postID:        postID,
		handler:       handler,
		dispatcher:    dispatcher,
		socialH:       socialH,
		notifier:      notifier,
		senderPeerID:  senderPeerID,
		senderBaseURL: senderBaseURL,
		senderKeyID:   senderKeyID,
		senderPriv:    senderPriv,
		senderActor:   senderActor,
	}
}

// socialPosterE2EAdapter bridges inbox.SocialPoster to
// social.Handler. Same pattern as the api.go adapter, defined
// in the test so the test package doesn't import http.
type socialPosterE2EAdapter struct{ h *social.Handler }

func (a *socialPosterE2EAdapter) InsertRemoteLike(ctx context.Context, kind string, id, peerID uuid.UUID, actorURI string) (bool, error) {
	return a.h.InsertRemoteLike(ctx, kind, id, peerID, actorURI)
}

func (a *socialPosterE2EAdapter) InsertRemoteComment(ctx context.Context, in inbox.RemoteCommentInput) (uuid.UUID, bool, error) {
	return a.h.InsertRemoteComment(ctx, social.RemoteCommentInput{
		TargetKind:  in.TargetKind,
		TargetID:    in.TargetID,
		ParentID:    in.ParentID,
		PeerID:      in.PeerID,
		ActorURI:    in.ActorURI,
		ActivityURI: in.ActivityURI,
		Body:        in.Body,
	})
}

type pairedPeerLookup struct {
	peerID  uuid.UUID
	peerURL string
	keyID   string
	pubKey  ed25519.PublicKey
}

func (l *pairedPeerLookup) ByKeyID(_ context.Context, keyID string) (inbox.PeerInfo, error) {
	if keyID != l.keyID {
		return inbox.PeerInfo{}, inbox.ErrPeerNotFound
	}
	return inbox.PeerInfo{
		ID:                l.peerID,
		InstanceURL:       l.peerURL,
		InstancePublicKey: l.pubKey,
		Enabled:           true,
		Connected:         true,
	}, nil
}

// --- envelope builders ----------------------------------------------------

func (fx *pairedFixture) buildLikeEnvelope() (envelopeJSON []byte, activityURI string) {
	activityURI = fx.senderBaseURL + "/activities/" + uuid.NewString()
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Like",
		"id":        activityURI,
		"actor":     fx.senderActor,
		"published": time.Now().UTC().Format(time.RFC3339),
		// object MUST point at the receiver's canonical URL.
		"object": fx.localBaseURL + "/posts/" + fx.postID.String(),
		// display hints — the outbox dispatcher (1.22.D-b) will
		// add these; here we ship them inline so the test
		// exercises the upsert.
		"actorDisplayName": "Bob from Studio B",
		"actorAvatarUrl":   fx.senderBaseURL + "/avatars/bob.png",
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.senderActor + "#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	b, _ := json.Marshal(env)
	return b, activityURI
}

func (fx *pairedFixture) buildCommentEnvelope(body string) (envelopeJSON []byte, activityURI string) {
	activityURI = fx.senderBaseURL + "/activities/" + uuid.NewString()
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      "Create",
		"id":        activityURI,
		"actor":     fx.senderActor,
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    fx.localBaseURL + "/posts/" + fx.postID.String(),
		// Note content sits in top-level `content` per the inbox
		// dispatcher's extractCommentPayload preference order.
		"content":          body,
		"actorDisplayName": "Bob from Studio B",
		"actorAvatarUrl":   fx.senderBaseURL + "/avatars/bob.png",
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.senderActor + "#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	b, _ := json.Marshal(env)
	return b, activityURI
}

func (fx *pairedFixture) deliverToInbox(envelope []byte) *httptest.ResponseRecorder {
	req, err := http.NewRequest(http.MethodPost,
		fx.localBaseURL+"/federation/inbox", nil)
	if err != nil {
		fx.t.Fatal(err)
	}
	req.Header.Set("Host", "studio-a.example")
	req.Body = io.NopCloser(bytesReader(envelope))
	if err := httpsig.SignAndAttach(req, envelope, fx.senderKeyID, fx.senderPriv); err != nil {
		fx.t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	return rr
}

// --- the two e2e tests ----------------------------------------------------

func TestE2E_RemoteLike_DispatchesAndPersists(t *testing.T) {
	fx := newPairedFixture(t)
	envelope, activityURI := fx.buildLikeEnvelope()

	// 1. POST to inbox → 202.
	rr := fx.deliverToInbox(envelope)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("inbox POST: %d body=%s", rr.Code, rr.Body.String())
	}

	// 2. federation_inbox row landed with status=pending.
	var status, gotURI string
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT status, activity_uri FROM federation_inbox WHERE peer_id = $1`,
		fx.senderPeerID,
	).Scan(&status, &gotURI)
	if status != "pending" {
		t.Errorf("post-POST status: got %q want pending", status)
	}
	if gotURI != activityURI {
		t.Errorf("activity_uri: got %q want %q", gotURI, activityURI)
	}

	// 3. Drive the dispatcher.
	processed, rejected, failed := fx.dispatcher.RunOnce(context.Background())
	if processed != 1 || rejected != 0 || failed != 0 {
		t.Fatalf("dispatch: processed=%d rejected=%d failed=%d (want 1/0/0)",
			processed, rejected, failed)
	}

	// 4. Row transitioned to processed.
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT status FROM federation_inbox WHERE activity_uri = $1`,
		activityURI,
	).Scan(&status)
	if status != "processed" {
		t.Errorf("post-dispatch status: got %q want processed", status)
	}

	// 5. likes row exists with peer + actor populated.
	var likeCount int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM likes
		 WHERE target_kind = 'post' AND target_id = $1
		   AND peer_id = $2 AND actor_uri = $3`,
		fx.postID, fx.senderPeerID, fx.senderActor,
	).Scan(&likeCount)
	if likeCount != 1 {
		t.Errorf("expected 1 remote like row; got %d", likeCount)
	}

	// 6. remote_actors cache populated with display info.
	var displayName, avatarURL string
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT display_name, avatar_url FROM federation_remote_actors WHERE actor_uri = $1`,
		fx.senderActor,
	).Scan(&displayName, &avatarURL)
	if displayName != "Bob from Studio B" {
		t.Errorf("display_name: got %q want Bob from Studio B", displayName)
	}
	if avatarURL == "" {
		t.Error("avatar_url should be populated from display hints")
	}

	// 7. Notifier fired with is_remote=true.
	last := fx.notifier.last()
	if last == nil {
		t.Fatal("notifier should have been called")
	}
	if last.verb != "post.like" {
		t.Errorf("notify verb: got %q want post.like", last.verb)
	}
	if last.actor != nil {
		t.Error("notify actor: should be nil for remote (actor_uri in payload)")
	}
	if last.recipient != fx.postOwnerRef {
		t.Errorf("notify recipient: got %d want %d (post owner)", last.recipient, fx.postOwnerRef)
	}
	if got, _ := last.payload["is_remote"].(bool); !got {
		t.Errorf("notify payload.is_remote: got %v want true", last.payload["is_remote"])
	}
	if got, _ := last.payload["actor_peer_id"].(string); got != fx.senderPeerID.String() {
		t.Errorf("notify payload.actor_peer_id: got %q want %q", got, fx.senderPeerID.String())
	}
}

func TestE2E_RemoteLike_IdempotentOnRetry(t *testing.T) {
	// A second delivery of the same envelope (same id) → 200 OK
	// no-op + the inbox row count + likes count stay at 1.
	fx := newPairedFixture(t)
	envelope, _ := fx.buildLikeEnvelope()
	// First delivery.
	fx.deliverToInbox(envelope)
	fx.dispatcher.RunOnce(context.Background())
	// Second delivery (replay). Replay cache may hit; if not, the
	// activity_uri UNIQUE catches it at INSERT time. Either way:
	// no double-dispatch.
	rr := fx.deliverToInbox(envelope)
	if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
		t.Errorf("retry: got %d want 200 or 202 (both are idempotent receipts)", rr.Code)
	}
	// Drive dispatcher again — should be no-op (no pending rows).
	processed, _, _ := fx.dispatcher.RunOnce(context.Background())
	if processed != 0 {
		t.Errorf("second dispatch: processed=%d want 0", processed)
	}
	// Like row count still 1.
	var n int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM likes WHERE peer_id = $1`, fx.senderPeerID,
	).Scan(&n)
	if n != 1 {
		t.Errorf("likes row count: got %d want 1 (idempotent)", n)
	}
}

func TestE2E_RemoteComment_DispatchesAndPersists(t *testing.T) {
	fx := newPairedFixture(t)
	envelope, activityURI := fx.buildCommentEnvelope("Nice work, Alice!")

	rr := fx.deliverToInbox(envelope)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("inbox POST: %d body=%s", rr.Code, rr.Body.String())
	}

	processed, _, _ := fx.dispatcher.RunOnce(context.Background())
	if processed != 1 {
		t.Fatalf("dispatch: processed=%d want 1", processed)
	}

	// Comments row exists with peer_id + actor_uri + activity_uri.
	var (
		bodyOut     string
		peerID      pgtype.UUID
		gotActor    *string
		gotActivity *string
		authorRef   *int64
	)
	err := fx.pool.QueryRow(context.Background(),
		`SELECT body, peer_id, actor_uri, activity_uri, author_user_ref
		 FROM comments WHERE activity_uri = $1`,
		activityURI,
	).Scan(&bodyOut, &peerID, &gotActor, &gotActivity, &authorRef)
	if err != nil {
		t.Fatalf("comment row lookup: %v", err)
	}
	if bodyOut != "Nice work, Alice!" {
		t.Errorf("comment body: got %q", bodyOut)
	}
	if uuid.UUID(peerID.Bytes) != fx.senderPeerID {
		t.Errorf("comment peer_id: got %v want %v", peerID, fx.senderPeerID)
	}
	if gotActor == nil || *gotActor != fx.senderActor {
		t.Errorf("comment actor_uri: got %v want %s", gotActor, fx.senderActor)
	}
	if authorRef != nil {
		t.Errorf("comment author_user_ref: should be nil for remote; got %v", *authorRef)
	}

	// Notifier fired with is_remote=true.
	last := fx.notifier.last()
	if last == nil || last.verb != "post.comment" {
		t.Errorf("notifier: got %+v want verb=post.comment", last)
	}
	if got, _ := last.payload["is_remote"].(bool); !got {
		t.Error("notify payload.is_remote should be true for remote comment")
	}
}

func TestE2E_RemoteComment_IdempotentOnRetry(t *testing.T) {
	fx := newPairedFixture(t)
	envelope, _ := fx.buildCommentEnvelope("idempotency check")
	fx.deliverToInbox(envelope)
	fx.dispatcher.RunOnce(context.Background())
	// Replay.
	fx.deliverToInbox(envelope)
	fx.dispatcher.RunOnce(context.Background())
	// One comment row total.
	var n int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM comments WHERE peer_id = $1`, fx.senderPeerID,
	).Scan(&n)
	if n != 1 {
		t.Errorf("comments row count: got %d want 1 (idempotent)", n)
	}
	// Notifier called once total.
	fx.notifier.mu.Lock()
	count := len(fx.notifier.entries)
	fx.notifier.mu.Unlock()
	if count != 1 {
		t.Errorf("notifier call count: got %d want 1 (idempotent)", count)
	}
}

// --- small helpers --------------------------------------------------------

func openInboxTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
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

func bytesReader(b []byte) interface {
	io.Reader
} {
	return &readerWrapper{data: b}
}

type readerWrapper struct {
	data []byte
	pos  int
}

func (r *readerWrapper) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
