// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Remote actor signing keys for per-actor envelope verification
// (#1526; spec §5.3, erratum E1). Implements inbox.ActorKeyResolver.
//
// Discovery: the key comes from the actor document the SENDING peer
// serves at the actor URI (GET <instance>/users/<username>), fetched
// with this instance's signed HTTP request (the same instance key
// pairing established). Only an actor URI that inbox.ActorBoundToPeer
// accepts for the authenticated peer is ever fetched, so the request
// always goes to that peer's own paired origin: no redirects are
// followed, the timeout is short and the body is capped. Nothing here
// fetches an arbitrary URL.
//
// Pinning: the first valid key is pinned on federation_remote_actors
// (trust on first use) together with the peer it came from, and is
// never replaced automatically. A later actor document with another
// key does not change the pin; envelopes signed with that key fail
// sig_invalid. Key rotation is not implemented (#1581): an operator
// clears the three signing_* columns of the actor row to re-pin.
//
// Concurrency: lookups for the same peer + actor share one fetch
// (singleflight), and the pin insert never overwrites a pinned key,
// so concurrent first deliveries all end up verifying against the one
// key that was pinned.

package remote

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

const (
	// ActorDocumentTimeout bounds one actor document fetch.
	ActorDocumentTimeout = 3 * time.Second
	// ActorDocumentMaxBytes caps the actor document body.
	ActorDocumentMaxBytes = 64 << 10
)

// RequestSigner signs an outbound request with this instance's key.
// Boot passes the same signer the outbox delivery worker uses.
type RequestSigner interface {
	Sign(req *http.Request, body []byte) error
}

// ActorDocument is the minimal actor document of spec §8.3 / §5.2:
// the actor id and its Ed25519 public key at <id>#main-key.
type ActorDocument struct {
	ID                string              `json:"id"`
	Type              string              `json:"type"`
	PreferredUsername string              `json:"preferredUsername"`
	PublicKey         ActorDocumentPubKey `json:"publicKey"`
}

// ActorDocumentPubKey is the publicKey block of an actor document.
type ActorDocumentPubKey struct {
	ID           string `json:"id"`
	Owner        string `json:"owner"`
	PublicKeyPEM string `json:"publicKeyPem"`
}

// SigningKeyResolver resolves and pins remote actor signing keys.
type SigningKeyResolver struct {
	q      *Queries
	signer RequestSigner
	client *http.Client
	logger *slog.Logger
	flight singleflight.Group
}

// NewSigningKeyResolver wires the resolver. signer is required: a
// resolver without it cannot fetch and answers every miss as a
// transient failure.
func NewSigningKeyResolver(pool *pgxpool.Pool, signer RequestSigner, logger *slog.Logger) *SigningKeyResolver {
	return &SigningKeyResolver{
		q:      New(pool),
		signer: signer,
		logger: logger,
		client: &http.Client{
			Timeout: ActorDocumentTimeout,
			// Never follow a redirect: the document must be served
			// at the actor URI itself, on the paired origin.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SetFetchTimeout overrides the fetch timeout (tests).
func (r *SigningKeyResolver) SetFetchTimeout(d time.Duration) { r.client.Timeout = d }

// errTransient marks a failure that may clear on retry.
var errTransient = errors.New("remote: actor document temporarily unavailable")

// SigningKey implements inbox.ActorKeyResolver.
func (r *SigningKeyResolver) SigningKey(ctx context.Context, peer inbox.PeerInfo, actorURI string) (ed25519.PublicKey, error) {
	if !inbox.ActorBoundToPeer(peer.InstanceURL, actorURI) {
		return nil, fmt.Errorf("%w: %s is not an actor of peer %s", inbox.ErrActorUnknown, actorURI, peer.InstanceURL)
	}
	if key, ok, err := r.pinned(ctx, peer.ID, actorURI); err != nil || ok {
		return key, err
	}
	v, err, _ := r.flight.Do(peer.ID.String()+" "+actorURI, func() (any, error) {
		if key, ok, err := r.pinned(ctx, peer.ID, actorURI); err != nil || ok {
			return key, err
		}
		key, err := r.fetch(ctx, actorURI)
		if err != nil {
			return nil, err
		}
		return r.pin(ctx, peer.ID, actorURI, key)
	})
	if err != nil {
		return nil, err
	}
	return v.(ed25519.PublicKey), nil
}

// pinned returns the pinned key for actorURI. ok=false with a nil
// error means nothing is pinned yet. A key pinned under a different
// peer is unknown_actor.
func (r *SigningKeyResolver) pinned(ctx context.Context, peerID uuid.UUID, actorURI string) (ed25519.PublicKey, bool, error) {
	row, err := r.q.GetRemoteActorSigningKey(ctx, actorURI)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: read pinned key: %v", errTransient, err)
	}
	if row.SigningPublicKey == nil {
		return nil, false, nil
	}
	if uuid.UUID(row.PeerID.Bytes) != peerID {
		return nil, false, fmt.Errorf("%w: %s is pinned to another peer", inbox.ErrActorUnknown, actorURI)
	}
	if len(row.SigningPublicKey) != ed25519.PublicKeySize {
		return nil, false, fmt.Errorf("%w: pinned key for %s has the wrong length", inbox.ErrActorUnknown, actorURI)
	}
	return ed25519.PublicKey(row.SigningPublicKey), true, nil
}

// pin stores key unless a key is already pinned, and returns the key
// that is pinned afterwards (ours, or the one that won).
func (r *SigningKeyResolver) pin(ctx context.Context, peerID uuid.UUID, actorURI string, key ed25519.PublicKey) (ed25519.PublicKey, error) {
	keyID := actorURI + "#main-key"
	row, err := r.q.PinRemoteActorSigningKey(ctx, PinRemoteActorSigningKeyParams{
		ActorUri:         actorURI,
		PeerID:           pgtype.UUID{Bytes: peerID, Valid: true},
		SigningPublicKey: key,
		SigningKeyID:     &keyID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A key was pinned first; it wins.
		won, ok, err := r.pinned(ctx, peerID, actorURI)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: pin of %s refused and nothing pinned", errTransient, actorURI)
		}
		return won, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: pin key: %v", errTransient, err)
	}
	if r.logger != nil {
		r.logger.LogAttrs(ctx, slog.LevelInfo, "federation.remote_actor.signing_key_pinned",
			slog.String("actor", actorURI), slog.String("peer_id", peerID.String()),
			slog.String("fingerprint", federation.PublicKeyFingerprint(key)))
	}
	return ed25519.PublicKey(row.SigningPublicKey), nil
}

// fetch GETs and validates the actor document at actorURI.
func (r *SigningKeyResolver) fetch(ctx context.Context, actorURI string) (ed25519.PublicKey, error) {
	if r.signer == nil {
		return nil, fmt.Errorf("%w: no request signer wired", errTransient)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, actorURI, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", inbox.ErrActorUnknown, err)
	}
	req.Header.Set("Accept", "application/json")
	if err := r.signer.Sign(req, []byte{}); err != nil {
		return nil, fmt.Errorf("%w: sign request: %v", errTransient, err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errTransient, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("%w: actor document %s: HTTP %d", inbox.ErrActorUnknown, actorURI, resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("%w: actor document %s redirects (HTTP %d); redirects are not followed",
			inbox.ErrActorUnknown, actorURI, resp.StatusCode)
	default:
		return nil, fmt.Errorf("%w: actor document %s: HTTP %d", errTransient, actorURI, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ActorDocumentMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read actor document: %v", errTransient, err)
	}
	if len(body) > ActorDocumentMaxBytes {
		return nil, fmt.Errorf("%w: actor document %s exceeds %d bytes", inbox.ErrActorUnknown, actorURI, ActorDocumentMaxBytes)
	}
	var doc ActorDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: actor document %s is not JSON: %v", inbox.ErrActorUnknown, actorURI, err)
	}
	if doc.ID != actorURI || doc.PublicKey.ID != actorURI+"#main-key" || doc.PublicKey.Owner != actorURI {
		return nil, fmt.Errorf("%w: actor document %s names id %q, key %q, owner %q",
			inbox.ErrActorUnknown, actorURI, doc.ID, doc.PublicKey.ID, doc.PublicKey.Owner)
	}
	key, err := federation.PublicKeyFromPEM([]byte(doc.PublicKey.PublicKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("%w: actor document %s key: %v", inbox.ErrActorUnknown, actorURI, err)
	}
	return key, nil
}

// Compile-time check against the inbox contract.
var _ inbox.ActorKeyResolver = (*SigningKeyResolver)(nil)
