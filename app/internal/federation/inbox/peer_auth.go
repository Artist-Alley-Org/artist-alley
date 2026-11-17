// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
)

// ErrPeerUnauthenticated is AuthenticatePeerRequest's answer for a
// request that is not signed by an enabled, connected peer.
var ErrPeerUnauthenticated = errors.New("inbox: request is not signed by an enabled, connected peer")

// AuthenticatePeerRequest checks a request's instance HTTP Signature
// the same way PostInbox stages 2-5 do: the keyId resolves to a
// paired peer, the peer is enabled and connected, and the signature
// (covering request target, host, date and the digest of body)
// verifies under the peer's instance key. Used by endpoints other
// than the inbox that only paired peers may call, such as the actor
// document (#1526).
//
// Returns an error wrapping ErrPeerUnauthenticated for every
// authentication failure, and any other error for a lookup failure
// that may clear on retry.
func AuthenticatePeerRequest(ctx context.Context, r *http.Request, body []byte, lookup PeerLookup, now time.Time) (PeerInfo, error) {
	params, err := httpsig.ParseSignatureHeader(r.Header.Get("Signature"))
	if err != nil {
		return PeerInfo{}, fmt.Errorf("%w: %v", ErrPeerUnauthenticated, err)
	}
	peer, err := lookup.ByKeyID(ctx, params.KeyID)
	if err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			return PeerInfo{}, fmt.Errorf("%w: unknown keyId", ErrPeerUnauthenticated)
		}
		return PeerInfo{}, err
	}
	if !peer.Enabled || !peer.Connected {
		return PeerInfo{}, fmt.Errorf("%w: peer disabled or not connected", ErrPeerUnauthenticated)
	}
	resolve := func(keyID string) (ed25519.PublicKey, error) {
		if keyID != params.KeyID || len(peer.InstancePublicKey) == 0 {
			return nil, httpsig.ErrUnknownKey
		}
		return peer.InstancePublicKey, nil
	}
	if _, err := httpsig.Verify(r, body, resolve, now); err != nil {
		return PeerInfo{}, fmt.Errorf("%w: %v", ErrPeerUnauthenticated, err)
	}
	return peer, nil
}
