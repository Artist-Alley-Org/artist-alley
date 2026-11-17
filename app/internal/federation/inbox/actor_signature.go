// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Per-actor envelope signature verification (#1526; spec §5.3, §5.6
// erratum E1, §6.4). Runs in both PostInbox and PostInboxBatch after
// the structural signature check and the algorithm allowlist, and
// before anything about the envelope is recorded: the stored-id
// answer (#1569) still comes first, and a rejected envelope is never
// stored.
//
// The instance HTTP Signature (stages 2-5) authenticates the PEER.
// This stage authenticates the ACTOR inside the envelope:
//
//  1. Binding. env.Actor must be "<peer instance URL>/users/<one
//     segment>": the same scheme and host as the peer whose instance
//     key signed the request. Otherwise unknown_actor, decided
//     before any key lookup, so no fetch is ever made for a foreign
//     URI and a paired peer cannot speak for another instance's
//     users.
//  2. Key id. signature.publicKey must equal env.Actor + "#main-key"
//     (§5.3); otherwise unknown_key.
//  3. Encoding. The value must be base64url (no padding) of exactly
//     64 bytes; otherwise sig_malformed. The pre-#1526 placeholder
//     ("AAAAAAAAAAAA", 9 bytes) lands here: an un-upgraded peer is
//     refused until it signs.
//  4. Key. The ActorKeyResolver returns the actor's pinned key, or
//     fetches and pins it from the peer's actor document. A missing
//     or invalid document is unknown_actor; a fetch that may succeed
//     later is a retryable 503 and nothing is stored.
//  5. Verify. Ed25519 over the RFC 8785 canonical form of the
//     envelope without its signature; failure is sig_invalid.
//
// There is no fallback: no resolver wired, a cache miss or a fetch
// failure never lets an envelope through unverified.

package inbox

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// ActorKeyResolver returns the Ed25519 public key an actor's
// envelopes must verify against. actorURI has already passed
// ActorBoundToPeer for peer. Implementations return an error
// wrapping ErrActorUnknown when the actor has no valid actor document
// on that peer (or is pinned to a different peer), and any other
// error for a failure that may clear on retry.
type ActorKeyResolver interface {
	SigningKey(ctx context.Context, peer PeerInfo, actorURI string) (ed25519.PublicKey, error)
}

// ErrActorUnknown is the resolver's "this actor has no usable key on
// that peer" answer. Maps to unknown_actor.
var ErrActorUnknown = errors.New("inbox: actor has no valid actor document on the sending peer")

// actorKeyTransientError is stage 10b's retryable outcome: the key
// could not be obtained right now. PostInbox answers 503 with
// Retry-After; the batch handler turns it into its whole-batch 503.
type actorKeyTransientError struct{ err error }

func (e actorKeyTransientError) Error() string {
	return "actor signing key temporarily unavailable: " + e.err.Error()
}
func (e actorKeyTransientError) Unwrap() error { return e.err }

// SetActorKeyResolver wires the actor key source. Required: without
// it every envelope is answered with a retryable 503.
func (h *Handler) SetActorKeyResolver(r ActorKeyResolver) { h.actorKeys = r }

// maxActorSegmentLen caps the username segment of an actor URI.
const maxActorSegmentLen = 128

// reservedActorSegments are /users/<x> paths the web app owns; they
// are never actor URIs (see the actor document route).
var reservedActorSegments = map[string]struct{}{
	"by-ref":      {},
	"by-username": {},
}

// ValidActorSegment reports whether s can be the username segment of
// an actor URI: 1 to maxActorSegmentLen characters from
// [A-Za-z0-9._@+-], and not a reserved web-app path.
func ValidActorSegment(s string) bool {
	if s == "" || len(s) > maxActorSegmentLen {
		return false
	}
	if _, reserved := reservedActorSegments[s]; reserved {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '@' || c == '+' || c == '-':
		default:
			return false
		}
	}
	return s != "." && s != ".."
}

// ActorBoundToPeer reports whether actorURI is exactly
// "<instanceURL>/users/<segment>" for the peer registered at
// instanceURL: same scheme, same host and port, no user info, query,
// fragment or escaped path, and a valid segment.
func ActorBoundToPeer(instanceURL, actorURI string) bool {
	pu, err := url.Parse(strings.TrimRight(instanceURL, "/"))
	if err != nil || pu.Scheme == "" || pu.Host == "" {
		return false
	}
	au, err := url.Parse(actorURI)
	if err != nil || au.Opaque != "" || au.User != nil || au.RawQuery != "" ||
		au.Fragment != "" || au.RawPath != "" || au.ForceQuery {
		return false
	}
	if !strings.EqualFold(au.Scheme, pu.Scheme) || !strings.EqualFold(au.Host, pu.Host) {
		return false
	}
	seg, ok := strings.CutPrefix(au.Path, strings.TrimRight(pu.Path, "/")+"/users/")
	return ok && ValidActorSegment(seg)
}

// verifyActorSignature runs stage 10b. It returns nil when the
// envelope is signed by its actor, a rejection for a terminal
// failure, or an actorKeyTransientError.
func (h *Handler) verifyActorSignature(ctx context.Context, peer PeerInfo, env *federation.Envelope) error {
	if !ActorBoundToPeer(peer.InstanceURL, env.Actor) {
		return reject(federation.InboxStatusUnknownActor, http.StatusForbidden,
			"actor is not a /users/ actor of the sending peer")
	}
	if env.Signature.PublicKey != env.Actor+"#main-key" {
		return reject(federation.InboxStatusUnknownKey, http.StatusUnauthorized,
			"signature.publicKey is not the actor's #main-key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(env.Signature.Value)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return reject(federation.InboxStatusSigMalformed, http.StatusBadRequest,
			"signature value is not 64 base64url-encoded bytes")
	}
	if h.actorKeys == nil {
		return actorKeyTransientError{err: errors.New("no actor key resolver wired")}
	}
	pub, err := h.actorKeys.SigningKey(ctx, peer, env.Actor)
	if err != nil {
		if errors.Is(err, ErrActorUnknown) {
			return reject(federation.InboxStatusUnknownActor, http.StatusUnprocessableEntity,
				"actor document unavailable or invalid: "+err.Error())
		}
		return actorKeyTransientError{err: err}
	}
	msg, err := env.CanonicalSigningBytes()
	if err != nil {
		return reject(federation.InboxStatusSigMalformed, http.StatusBadRequest,
			fmt.Sprintf("envelope cannot be canonicalised: %v", err))
	}
	if err := federation.Verify(pub, msg, sig); err != nil {
		return reject(federation.InboxStatusSigInvalid, http.StatusUnauthorized,
			"envelope signature does not verify under the actor's key")
	}
	return nil
}
