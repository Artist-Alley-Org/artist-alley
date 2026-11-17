// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1526: SigningKeyResolver against a fake paired peer. The peer's
// actor document endpoint verifies our instance HTTP signature, counts
// fetches and serves whatever the test configures.

package remote_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/remote"
)

// instanceSigner signs requests with this test instance's key.
type instanceSigner struct {
	keyID string
	priv  ed25519.PrivateKey
}

func (s instanceSigner) Sign(req *http.Request, body []byte) error {
	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}
	return httpsig.SignAndAttach(req, body, s.keyID, s.priv)
}

type fakePeer struct {
	t       *testing.T
	srv     *httptest.Server
	peer    inbox.PeerInfo
	ourPub  ed25519.PublicKey
	fetches atomic.Int32
	// serve answers a verified GET for actorURI.
	mu    sync.Mutex
	serve func(w http.ResponseWriter, r *http.Request, actorURI string)
	// sigErr records a request whose instance signature failed.
	sigErr atomic.Value
}

func (p *fakePeer) handler(w http.ResponseWriter, r *http.Request) {
	p.fetches.Add(1)
	if _, err := httpsig.Verify(r, []byte{}, func(string) (ed25519.PublicKey, error) { return p.ourPub, nil }, time.Now()); err != nil {
		p.sigErr.Store(err.Error())
		http.Error(w, "unsigned", http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	serve := p.serve
	p.mu.Unlock()
	serve(w, r, p.srv.URL+r.URL.Path)
}

func (p *fakePeer) setServe(f func(w http.ResponseWriter, r *http.Request, actorURI string)) {
	p.mu.Lock()
	p.serve = f
	p.mu.Unlock()
}

// serveKey answers with a well-formed actor document for key.
func serveKey(key ed25519.PublicKey) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, _ *http.Request, actorURI string) {
		pemBytes, _ := federation.PublicKeyToPEM(key)
		_ = json.NewEncoder(w).Encode(remote.ActorDocument{
			ID: actorURI, Type: "Person",
			PublicKey: remote.ActorDocumentPubKey{ID: actorURI + "#main-key", Owner: actorURI, PublicKeyPEM: string(pemBytes)},
		})
	}
}

// newSigningFixture seeds a paired peer at a fake server and returns a
// resolver signing as "us".
func newSigningFixture(t *testing.T) (*fakePeer, *remote.SigningKeyResolver, *pgxpool.Pool) {
	t.Helper()
	pool := openPool(t)
	ctx := context.Background()
	ourPub, ourPriv, _ := ed25519.GenerateKey(rand.Reader)
	fp := &fakePeer{t: t, ourPub: ourPub}
	fp.srv = httptest.NewServer(http.HandlerFunc(fp.handler))
	t.Cleanup(fp.srv.Close)

	var ownerRef int64
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username) VALUES ($1) RETURNING ref`,
		"sigkey_owner_"+randHex(t, 4)).Scan(&ownerRef); err != nil {
		t.Fatalf("owner: %v", err)
	}
	peerID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO federation_peers (id, instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		VALUES ($1, $2, 'SIGKEY', '-----BEGIN PUBLIC KEY-----\nfake\n-----END PUBLIC KEY-----',
		        'connected', 'plaintext', TRUE, 'connected', $3)`,
		peerID, fp.srv.URL, ownerRef); err != nil {
		t.Fatalf("peer: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ownerRef)
	})
	fp.peer = inbox.PeerInfo{ID: peerID, InstanceURL: fp.srv.URL, Enabled: true, Connected: true}
	r := remote.NewSigningKeyResolver(pool, instanceSigner{keyID: "https://us.example/federation/instance#main-key", priv: ourPriv}, nil)
	return fp, r, pool
}

func TestSigningKey_FetchesSignedThenPins(t *testing.T) {
	fp, r, pool := newSigningFixture(t)
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	fp.setServe(serveKey(key))
	actor := fp.srv.URL + "/users/alice"

	got, err := r.SigningKey(context.Background(), fp.peer, actor)
	if err != nil {
		t.Fatalf("SigningKey: %v (peer saw signature error: %v)", err, fp.sigErr.Load())
	}
	if !got.Equal(key) {
		t.Error("wrong key")
	}
	var pinned []byte
	var keyID string
	if err := pool.QueryRow(context.Background(),
		`SELECT signing_public_key, signing_key_id FROM federation_remote_actors WHERE actor_uri = $1`, actor,
	).Scan(&pinned, &keyID); err != nil || !ed25519.PublicKey(pinned).Equal(key) || keyID != actor+"#main-key" {
		t.Errorf("pin row: key match=%v keyID=%q err=%v", ed25519.PublicKey(pinned).Equal(key), keyID, err)
	}
	// Second lookup is served from the pin.
	if _, err := r.SigningKey(context.Background(), fp.peer, actor); err != nil {
		t.Fatal(err)
	}
	if n := fp.fetches.Load(); n != 1 {
		t.Errorf("fetches: %d want 1", n)
	}
}

// A pinned key is never replaced: the peer serving a new key changes
// nothing (rotation is #1581).
func TestSigningKey_PinNeverRotates(t *testing.T) {
	fp, r, _ := newSigningFixture(t)
	first, _, _ := ed25519.GenerateKey(rand.Reader)
	second, _, _ := ed25519.GenerateKey(rand.Reader)
	actor := fp.srv.URL + "/users/alice"
	fp.setServe(serveKey(first))
	if _, err := r.SigningKey(context.Background(), fp.peer, actor); err != nil {
		t.Fatal(err)
	}
	fp.setServe(serveKey(second))
	got, err := r.SigningKey(context.Background(), fp.peer, actor)
	if err != nil || !got.Equal(first) {
		t.Errorf("after the peer changed key: got first=%v err=%v; the pin must hold", got.Equal(first), err)
	}
	// A second resolver (another process) also sees the pin.
	_, ourPriv, _ := ed25519.GenerateKey(rand.Reader)
	r2 := remote.NewSigningKeyResolver(openPool(t), instanceSigner{keyID: "k", priv: ourPriv}, nil)
	got, err = r2.SigningKey(context.Background(), fp.peer, actor)
	if err != nil || !got.Equal(first) {
		t.Errorf("fresh resolver: got first=%v err=%v", got.Equal(first), err)
	}
	if n := fp.fetches.Load(); n != 1 {
		t.Errorf("fetches: %d want 1", n)
	}
}

func TestSigningKey_ConcurrentFirstLookups_OneFetch(t *testing.T) {
	fp, r, _ := newSigningFixture(t)
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	gate := make(chan struct{})
	fp.setServe(func(w http.ResponseWriter, req *http.Request, actorURI string) {
		<-gate
		serveKey(key)(w, req, actorURI)
	})
	actor := fp.srv.URL + "/users/alice"
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := r.SigningKey(context.Background(), fp.peer, actor)
			if err == nil && !got.Equal(key) {
				err = errors.New("wrong key")
			}
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := fp.fetches.Load(); n != 1 {
		t.Errorf("fetches: %d want 1 (coalesced)", n)
	}
}

// A URI that is not an actor of the peer is refused with no request at
// all: the resolver never fetches an arbitrary URL.
func TestSigningKey_ForeignURI_NoFetch(t *testing.T) {
	fp, r, _ := newSigningFixture(t)
	fp.setServe(serveKey(ed25519.PublicKey(make([]byte, 32))))
	for _, uri := range []string{
		"https://evil.example/users/alice",
		fp.srv.URL + "/admin",
		fp.srv.URL + "/users/alice/../../admin",
		fp.srv.URL + "/users/alice?redirect=https://evil.example",
	} {
		if _, err := r.SigningKey(context.Background(), fp.peer, uri); !errors.Is(err, inbox.ErrActorUnknown) {
			t.Errorf("%s: err=%v want ErrActorUnknown", uri, err)
		}
	}
	if n := fp.fetches.Load(); n != 0 {
		t.Errorf("fetches: %d want 0", n)
	}
}

func TestSigningKey_BadDocuments_Unknown(t *testing.T) {
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	pemBytes, _ := federation.PublicKeyToPEM(key)
	for _, c := range []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request, actorURI string)
	}{
		{"404", func(w http.ResponseWriter, r *http.Request, _ string) { http.NotFound(w, r) }},
		{"410", func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(http.StatusGone) }},
		{"not JSON", func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = w.Write([]byte("<html>")) }},
		{"id mismatch", func(w http.ResponseWriter, _ *http.Request, actorURI string) {
			_ = json.NewEncoder(w).Encode(remote.ActorDocument{ID: actorURI + "x",
				PublicKey: remote.ActorDocumentPubKey{ID: actorURI + "#main-key", Owner: actorURI, PublicKeyPEM: string(pemBytes)}})
		}},
		{"key id mismatch", func(w http.ResponseWriter, _ *http.Request, actorURI string) {
			_ = json.NewEncoder(w).Encode(remote.ActorDocument{ID: actorURI,
				PublicKey: remote.ActorDocumentPubKey{ID: actorURI + "#other", Owner: actorURI, PublicKeyPEM: string(pemBytes)}})
		}},
		{"owner mismatch", func(w http.ResponseWriter, _ *http.Request, actorURI string) {
			_ = json.NewEncoder(w).Encode(remote.ActorDocument{ID: actorURI,
				PublicKey: remote.ActorDocumentPubKey{ID: actorURI + "#main-key", Owner: "https://evil.example/users/x", PublicKeyPEM: string(pemBytes)}})
		}},
		{"bad PEM", func(w http.ResponseWriter, _ *http.Request, actorURI string) {
			_ = json.NewEncoder(w).Encode(remote.ActorDocument{ID: actorURI,
				PublicKey: remote.ActorDocumentPubKey{ID: actorURI + "#main-key", Owner: actorURI, PublicKeyPEM: "nope"}})
		}},
		{"oversized", func(w http.ResponseWriter, _ *http.Request, _ string) {
			_, _ = w.Write([]byte(`{"id":"` + strings.Repeat("a", remote.ActorDocumentMaxBytes) + `"}`))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fp, r, pool := newSigningFixture(t)
			fp.setServe(c.serve)
			actor := fp.srv.URL + "/users/alice"
			if _, err := r.SigningKey(context.Background(), fp.peer, actor); !errors.Is(err, inbox.ErrActorUnknown) {
				t.Errorf("err=%v want ErrActorUnknown", err)
			}
			var n int
			_ = pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM federation_remote_actors WHERE actor_uri = $1 AND signing_public_key IS NOT NULL`, actor).Scan(&n)
			if n != 0 {
				t.Error("an invalid document was pinned")
			}
		})
	}
}

// A redirect is refused and its target is never contacted.
func TestSigningKey_Redirect_NotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
	}))
	t.Cleanup(target.Close)
	fp, r, _ := newSigningFixture(t)
	fp.setServe(func(w http.ResponseWriter, req *http.Request, _ string) {
		http.Redirect(w, req, target.URL+"/users/alice", http.StatusFound)
	})
	if _, err := r.SigningKey(context.Background(), fp.peer, fp.srv.URL+"/users/alice"); !errors.Is(err, inbox.ErrActorUnknown) {
		t.Errorf("err=%v want ErrActorUnknown", err)
	}
	if targetHits.Load() != 0 {
		t.Error("redirect target was contacted")
	}
}

// Failures that may clear are transient: never ErrActorUnknown (which
// the inbox answers terminally) and never a key.
func TestSigningKey_TransientFailures(t *testing.T) {
	for _, c := range []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request, actorURI string)
	}{
		{"500", func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(http.StatusInternalServerError) }},
		{"503", func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"timeout", func(w http.ResponseWriter, r *http.Request, _ string) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fp, r, _ := newSigningFixture(t)
			r.SetFetchTimeout(200 * time.Millisecond)
			fp.setServe(c.serve)
			key, err := r.SigningKey(context.Background(), fp.peer, fp.srv.URL+"/users/alice")
			if err == nil || key != nil || errors.Is(err, inbox.ErrActorUnknown) {
				t.Errorf("key=%v err=%v; want a transient error", key, err)
			}
		})
	}
}

// An actor pinned to another peer row is unknown for this one.
func TestSigningKey_PinnedToOtherPeer_Unknown(t *testing.T) {
	fp, r, _ := newSigningFixture(t)
	key, _, _ := ed25519.GenerateKey(rand.Reader)
	fp.setServe(serveKey(key))
	actor := fp.srv.URL + "/users/alice"
	if _, err := r.SigningKey(context.Background(), fp.peer, actor); err != nil {
		t.Fatal(err)
	}
	other := fp.peer
	other.ID = uuid.New()
	if _, err := r.SigningKey(context.Background(), other, actor); !errors.Is(err, inbox.ErrActorUnknown) {
		t.Errorf("err=%v want ErrActorUnknown", err)
	}
}
