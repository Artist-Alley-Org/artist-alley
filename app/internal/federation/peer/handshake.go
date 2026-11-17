// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Handshake protocol — Phase 1.22.B-b. The peer-to-peer dance
// that lets two operators pair instances by URL alone, without
// copy-pasting public keys.
//
// # Wire format (docs/spec/federation/v1.md §11)
//
// Both envelopes are POSTed to HandshakePath on the receiving
// instance's URL; the answer is a HandshakeAccepted JSON object.
//
//   {
//     "envelope": {
//       "protocol": "aa-handshake/v1",
//       "type": "offer" | "confirm",
//       "from": "https://studio-a.example",
//       "from_display_name": "Studio A",
//       "from_public_key_pem": "-----BEGIN PUBLIC KEY-----...",
//       "to": "https://studio-b.example",
//       "nonce": "<hex>",
//       "timestamp": "2026-06-05T16:00:00Z"
//     },
//     "signature": "<base64 Ed25519 sig over RFC-8785-canonical envelope>"
//   }
//
// # State machine
//
//   A initiates -> POST offer to B -> B creates pending_inbound
//                  (A also creates pending_outbound locally)
//   B's admin    -> POST confirm to A -> A flips pending_outbound
//   reviews +       to connected. B goes connected only once A
//   accepts         has answered the confirm "completed".
//
// Crossing offers (both sides pending_outbound) and a re-offer from
// a peer we are connected to are answered by confirming back to
// the offerer first; see handleOffer.
//
// # Trust model (the bootstrap problem)
//
// The handshake POST is unauthenticated at the TRANSPORT layer
// (we don't have HTTP-Sig yet for unknown peers). The envelope
// is self-signed by the offered public_key — so we know the
// payload wasn't tampered in flight, but we DON'T yet know that
// public_key belongs to the claimed instance. TOFU: the
// receiving admin reviews the offered fingerprint + decides
// whether to accept. 1.22.B-c adds DNS-TXT verification as the
// stronger trust bootstrap.

package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
)

// Protocol identifiers — published as constants so future
// version-bumps + interop tests reference a single source.
const (
	HandshakeProtocol      = "aa-handshake/v1"
	HandshakeTypeOffer     = "offer"
	HandshakeTypeConfirm   = "confirm"
	handshakeMaxSkew       = 5 * time.Minute
	handshakeHTTPTimeout   = 15 * time.Second
	handshakeNonceBytes    = 16
	handshakeBodyByteLimit = 64 * 1024

	// HandshakePath is the one handshake endpoint, relative to a
	// peer's instance URL: the OpenAPI servers base (/api/v1) plus
	// the postFederationHandshake path. There is no root alias
	// (#1592: the client used to POST to /federation/peers/handshake
	// at the root, which no build mounts).
	HandshakePath = "/api/v1/federation/peers/handshake"

	// handshakeAnswerByteLimit bounds how much of a peer's answer
	// is read. A HandshakeAccepted object is under 100 bytes.
	handshakeAnswerByteLimit = 4 << 10

	// handshakeQuoteLimit bounds how much of a refusal body is
	// quoted back to the operator.
	handshakeQuoteLimit = 256

	// pendingKeyPlaceholder stands in for the peer's key on a
	// pending_outbound row until the peer's confirm delivers it.
	pendingKeyPlaceholder = "PENDING-HANDSHAKE-RESPONSE"

	// HandshakeAccepted statuses (openapi.yaml HandshakeAccepted).
	handshakeAcceptedPending  = "accepted_pending"
	handshakeCompleted        = "completed"
	handshakeIgnoredDuplicate = "ignored_duplicate"
)

// Envelope is the signed payload exchanged between peers.
// Field order doesn't matter for the wire — the signature
// covers the RFC 8785 JSON canonicalization, which sorts keys
// deterministically regardless of how either side emits them.
//
// Phase 1.22.I-d (v0.4 wire bump): adds the optional
// SupportedCapabilities field. Pre-v0.4 peers (offer/confirm
// without the field) treated as advertising an empty set;
// negotiation runs + the peer surfaces in
// [Registry.ListPeersMissingCapabilities] for re-pairing.
type Envelope struct {
	Protocol         string `json:"protocol"`
	Type             string `json:"type"`
	From             string `json:"from"`
	FromDisplayName  string `json:"from_display_name"`
	FromPublicKeyPEM string `json:"from_public_key_pem"`
	To               string `json:"to"`
	Nonce            string `json:"nonce"`
	Timestamp        string `json:"timestamp"` // RFC 3339 nano

	// SupportedCapabilities (Phase 1.22.I-d) — the sender's
	// advertised capability set. The receiver intersects this
	// with its own KnownCapabilities + persists the result on
	// federation_peers.capabilities. omitempty so handshake
	// envelopes from pre-v0.4 peers round-trip cleanly through
	// the canonical-JSON signature check (an absent field is
	// not in the signed bytes, so a v0.4 receiver verifying a
	// v0.3 sender's signature passes).
	SupportedCapabilities CapabilitySet `json:"supported_capabilities,omitempty"`
}

// signedEnvelope is the on-wire JSON shape.
type signedEnvelope struct {
	Envelope  Envelope `json:"envelope"`
	Signature string   `json:"signature"` // base64 std
}

// Errors callers + the HTTP handler may distinguish on.
var (
	// ErrHandshakeProtocol — envelope.protocol doesn't match
	// HandshakeProtocol. Future versions may bump; for now we
	// reject anything else.
	ErrHandshakeProtocol = errors.New("handshake: protocol mismatch")

	// ErrHandshakeType — envelope.type is neither offer nor confirm.
	ErrHandshakeType = errors.New("handshake: unknown type")

	// ErrHandshakeWrongRecipient — envelope.to doesn't match our
	// configured base URL. Defends against accidental cross-instance
	// posting (operator types the wrong URL) + against an attacker
	// forwarding a signed envelope addressed to someone else.
	ErrHandshakeWrongRecipient = errors.New("handshake: addressed to a different instance")

	// ErrHandshakeStale — timestamp is outside the skew window.
	// Replay defence: a captured offer can't be used hours later.
	ErrHandshakeStale = errors.New("handshake: timestamp outside skew window")

	// ErrHandshakeBadSig — envelope's signature didn't verify
	// against the embedded public key.
	ErrHandshakeBadSig = errors.New("handshake: signature verification failed")

	// ErrHandshakeMalformed — envelope is missing required fields
	// or carries garbage in a typed field.
	ErrHandshakeMalformed = errors.New("handshake: malformed envelope")

	// ErrHandshakePeerNotFound — incoming confirm refers to a peer
	// URL we never initiated to. Possible attack: random instance
	// sending us confirm noise.
	ErrHandshakePeerNotFound = errors.New("handshake: no pending outbound for this peer")

	// ErrHandshakeKeyConflict: an envelope from a URL we already
	// hold a key for carries a different key. Refused, never
	// stored: re-keying a peer is a defederate and re-pair (#1592).
	ErrHandshakeKeyConflict = errors.New("handshake: peer key differs from the key on record")

	// ErrHandshakeConfirmBack: an offer that needed our confirm
	// back to the offerer (crossing offers, or a re-offer from a
	// connected peer) could not be completed because that confirm
	// was not accepted. Nothing was recorded.
	ErrHandshakeConfirmBack = errors.New("handshake: could not confirm back to the offering instance")

	// ErrHandshakeAnswer: the peer's answer to our offer or
	// confirm is not a valid acceptance (wrong status code, not
	// JSON, malformed, missing or unexpected fields, a redirect).
	ErrHandshakeAnswer = errors.New("handshake: peer's answer is not an acceptance")
)

// HTTPDoer is the http.Client subset the outbound transport
// needs. Lets tests inject a roundtripper that records requests
// without spinning up a server.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Engine owns the handshake protocol — the outbound transport,
// the envelope minter, the inbound verifier. Constructed once
// at boot with the instance Identity + the peer Registry.
type Engine struct {
	registry *Registry
	identity *identity.Manager
	http     HTTPDoer
	now      func() time.Time

	// localBaseURL is THIS instance's federation URL — what we
	// stamp into envelope.from + check against envelope.to. Set
	// at boot via SetLocalBaseURL.
	localBaseURL string

	// localDisplayName is THIS instance's display name for the
	// receiving admin's UI. Set at boot via SetLocalDisplayName.
	localDisplayName string
}

// NewEngine wires the handshake engine. http defaults to a
// fresh http.Client with a 15s timeout that never follows a
// redirect (a redirect would re-POST the envelope elsewhere and
// hide a wrong instance URL; the 3xx is refused as an answer);
// pass a custom one for tests or special transport policies.
func NewEngine(registry *Registry, identityMgr *identity.Manager, httpDoer HTTPDoer) *Engine {
	if httpDoer == nil {
		httpDoer = &http.Client{
			Timeout: handshakeHTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Engine{
		registry: registry,
		identity: identityMgr,
		http:     httpDoer,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// SetLocalBaseURL configures the instance URL we identify as.
// Wired at boot from sysconfig.Site.BaseURL.
func (e *Engine) SetLocalBaseURL(url string) { e.localBaseURL = url }

// SetLocalDisplayName configures the instance display name —
// what peers' admin UIs show for "request from <name>".
func (e *Engine) SetLocalDisplayName(name string) { e.localDisplayName = name }

// --- inbound (receive an envelope from a peer) ---------------------------

// InboundResult tells the HTTP handler what state the protocol
// is in after processing an incoming envelope.
type InboundResult struct {
	Peer   *Peer
	Status string // "accepted_pending" | "completed" | "ignored_duplicate"
}

// HandleInbound is the entry point for POST /federation/peers/handshake.
// Returns one of the ErrHandshake* errors on protocol violation, or
// an opaque error for transient failures (DB down, etc.). On success
// the HTTP handler should return 200 with a small JSON body the
// caller can use to learn what state it ended up in.
func (e *Engine) HandleInbound(ctx context.Context, body []byte) (*InboundResult, error) {
	env, err := e.parseAndVerify(body)
	if err != nil {
		return nil, err
	}
	// Cross-check the recipient, both sides normalised (env.To
	// already is); an unusable local base URL matches nothing.
	local, lerr := normalizeInstanceURL(e.localBaseURL)
	if lerr != nil || !strings.EqualFold(env.To, local) {
		return nil, fmt.Errorf("%w: envelope.to=%q local=%q", ErrHandshakeWrongRecipient, env.To, e.localBaseURL)
	}

	switch env.Type {
	case HandshakeTypeOffer:
		return e.handleOffer(ctx, *env)
	case HandshakeTypeConfirm:
		return e.handleConfirm(ctx, *env)
	default:
		return nil, fmt.Errorf("%w: %q", ErrHandshakeType, env.Type)
	}
}

// handleOffer processes an inbound offer envelope. Idempotent on
// (from_url): re-receiving the same offer doesn't double-insert.
//
// Phase 1.22.I-d: every offer carries the sender's advertised
// capability set; we intersect with our own KnownCapabilities +
// persist the result on the peer row so the I-e/I-g dispatch
// gate has the bilateral view. Runs on both the new-peer and
// duplicate paths: re-handshakes refresh the cached intersection.
//
// An existing record decides the answer (#1592):
//
//   - pending_inbound: the same offer again (its answer was lost,
//     or the offerer re-sent it). accepted_pending; the admin still
//     decides.
//   - pending_outbound: offers crossed, both operators asked to
//     pair. The offerer is pending_outbound too and needs our key,
//     which only a confirm carries, so we confirm back to it first;
//     only once it answers "completed" do we store its offered key
//     and go connected. Answer completed.
//   - connected: the offerer lost its record of us (or re-pairs to
//     renegotiate capabilities). Same confirm back; answer completed.
//
// A known key that differs from the offered one is refused
// (ErrHandshakeKeyConflict) and nothing is changed: re-keying a
// peer is a defederate and re-pair, never a silent replacement.
func (e *Engine) handleOffer(ctx context.Context, env Envelope) (*InboundResult, error) {
	existing, err := e.registry.ByInstanceURL(ctx, env.From)
	if err != nil && !errors.Is(err, ErrPeerNotFound) {
		return nil, err
	}
	if existing == nil {
		// New offer: create a pending_inbound row for admin review.
		enabledFalse := true // enabled means "kill switch off"; default true
		p, err := e.registry.Add(ctx, AddInput{
			InstanceURL:        env.From,
			DisplayName:        env.FromDisplayName,
			InstancePublicKey:  env.FromPublicKeyPEM,
			TrustTier:          federation.TrustConnected,
			EncryptionPolicy:   federation.EncryptionPlaintext,
			HandshakeByUserRef: 0, // 0 = "system, awaiting admin"
			Notes:              "auto-handshake: awaiting admin accept",
			Enabled:            &enabledFalse,
			Status:             federation.PeerStatusPendingInbound,
		})
		if err != nil {
			return nil, err
		}
		if err := e.storeCapabilities(ctx, p.ID, env); err != nil {
			return nil, err
		}
		return &InboundResult{Peer: p, Status: handshakeAcceptedPending}, nil
	}

	switch existing.Status {
	case federation.PeerStatusPendingInbound:
		if !samePeerKey(existing.InstancePublicKey, env.FromPublicKeyPEM) {
			return nil, fmt.Errorf("%w: offer from %s", ErrHandshakeKeyConflict, env.From)
		}
		if err := e.storeCapabilities(ctx, existing.ID, env); err != nil {
			return nil, err
		}
		return &InboundResult{Peer: existing, Status: handshakeAcceptedPending}, nil

	case federation.PeerStatusPendingOutbound, federation.PeerStatusConnected:
		if existing.Status == federation.PeerStatusConnected &&
			!samePeerKey(existing.InstancePublicKey, env.FromPublicKeyPEM) {
			return nil, fmt.Errorf("%w: offer from %s", ErrHandshakeKeyConflict, env.From)
		}
		if err := e.confirmTo(ctx, env.From); err != nil {
			_ = e.registry.appendNote(ctx, existing.ID, "confirm back to a crossing offer failed: "+err.Error())
			return nil, fmt.Errorf("%w: %s: %v", ErrHandshakeConfirmBack, env.From, err)
		}
		updated := existing
		if existing.Status == federation.PeerStatusPendingOutbound {
			if updated, err = e.registry.completeOutboundHandshake(ctx, existing.ID, env.FromPublicKeyPEM); err != nil {
				return nil, err
			}
		}
		if err := e.storeCapabilities(ctx, updated.ID, env); err != nil {
			return nil, err
		}
		return &InboundResult{Peer: updated, Status: handshakeCompleted}, nil
	}
	return &InboundResult{Peer: existing, Status: handshakeIgnoredDuplicate}, nil
}

// handleConfirm processes an inbound confirm envelope: the peer's
// admin accepted our outbound offer (or the peer is confirming back
// to our crossing offer). We flip the local pending_outbound row to
// connected with the peer's real key.
//
// A confirm for a row that is already connected with the same key
// is a replay whose answer was lost; it answers completed again so
// the peer's retried accept converges. A different key is refused.
// A confirm for a pending_inbound row (a confirm of an offer we do
// not remember sending) stays ignored_duplicate.
func (e *Engine) handleConfirm(ctx context.Context, env Envelope) (*InboundResult, error) {
	existing, err := e.registry.ByInstanceURL(ctx, env.From)
	if err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrHandshakePeerNotFound, env.From)
		}
		return nil, err
	}
	switch existing.Status {
	case federation.PeerStatusPendingOutbound:
		// The row's key is the placeholder; the confirm delivers the
		// peer's real key for the first time. Replaced atomically
		// alongside the status flip.
		updated, err := e.registry.completeOutboundHandshake(ctx, existing.ID, env.FromPublicKeyPEM)
		if err != nil {
			return nil, err
		}
		if err := e.storeCapabilities(ctx, updated.ID, env); err != nil {
			return nil, err
		}
		return &InboundResult{Peer: updated, Status: handshakeCompleted}, nil
	case federation.PeerStatusConnected:
		if !samePeerKey(existing.InstancePublicKey, env.FromPublicKeyPEM) {
			return nil, fmt.Errorf("%w: confirm from %s", ErrHandshakeKeyConflict, env.From)
		}
		if err := e.storeCapabilities(ctx, existing.ID, env); err != nil {
			return nil, err
		}
		return &InboundResult{Peer: existing, Status: handshakeCompleted}, nil
	}
	return &InboundResult{Peer: existing, Status: handshakeIgnoredDuplicate}, nil
}

// storeCapabilities records the intersection of our
// KnownCapabilities with what env advertises. Phase 1.22.I-d: only
// when the peer sent the field (even as an empty array); a nil
// slice is a pre-I-d peer, left with capabilities_negotiated_at NULL
// so ListPeersMissingCapabilities still surfaces it for re-pairing.
func (e *Engine) storeCapabilities(ctx context.Context, id uuid.UUID, env Envelope) error {
	if env.SupportedCapabilities == nil {
		return nil
	}
	return e.registry.SetCapabilities(ctx, id, Intersect(KnownCapabilities, env.SupportedCapabilities))
}

// samePeerKey reports whether two PEM-encoded Ed25519 public keys
// are the same key. Unparsable input never matches.
func samePeerKey(a, b string) bool {
	ka, errA := federation.PublicKeyFromPEM([]byte(a))
	kb, errB := federation.PublicKeyFromPEM([]byte(b))
	return errA == nil && errB == nil && ka.Equal(kb)
}

// --- outbound (we initiate or accept) ------------------------------------

// InitiateOffer is called by the admin "Pair by URL" flow. We
// create a local pending_outbound row + POST a signed offer to
// the peer.
//
// Calling it again for a URL whose row is still pending_outbound
// re-sends the offer (new nonce) on that same row: that is the
// retry after a failed offer or a lost answer (#1592). A row in any
// other state is returned as is, with nothing sent.
//
// Success means the peer answered with a valid HandshakeAccepted:
// accepted_pending (its admin decides), or completed when offers
// crossed, which is believed only once the peer's confirm has made
// our row connected with its real key.
//
// Errors:
//   - ErrInstanceURLInvalid if the URL fails validation
//   - a transport error, or ErrHandshakeAnswer for an answer that is
//     not an acceptance. The row (created before the POST so the
//     admin sees it even if the peer is down) stays pending_outbound,
//     and the reason is appended to its notes.
func (e *Engine) InitiateOffer(
	ctx context.Context,
	peerURL, peerDisplayName string,
	initiatorUserRef int64,
) (*Peer, error) {
	id, err := e.identity.Get()
	if err != nil {
		return nil, err
	}
	url, err := normalizeInstanceURL(peerURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(peerDisplayName) == "" {
		// Display name defaults to the host portion of the URL;
		// admin can rename later from the peer detail page.
		peerDisplayName = strings.TrimPrefix(url, "https://")
	}

	row, err := e.registry.ByInstanceURL(ctx, url)
	switch {
	case err == nil && row.Status != federation.PeerStatusPendingOutbound:
		return row, nil
	case err == nil:
		// Still pending_outbound: re-send on the same row.
	case errors.Is(err, ErrPeerNotFound):
		// Create the local pending_outbound row. The peer's key is a
		// placeholder until its confirm delivers the real one. Bit of
		// a misuse of the column, but it keeps the schema NOT NULL
		// and the row visible in the admin UI immediately.
		enabledFalse := true // kill-switch defaults on; "enabled" doesn't mean "active"
		row, err = e.registry.Add(ctx, AddInput{
			InstanceURL:        url,
			DisplayName:        peerDisplayName,
			InstancePublicKey:  pendingKeyPlaceholder,
			TrustTier:          federation.TrustConnected,
			EncryptionPolicy:   federation.EncryptionPlaintext,
			HandshakeByUserRef: initiatorUserRef,
			Notes:              "auto-handshake: offer sent, awaiting peer admin",
			Enabled:            &enabledFalse,
			Status:             federation.PeerStatusPendingOutbound,
		})
		if err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	// Build + sign the offer envelope. SupportedCapabilities is
	// Phase 1.22.I-d's wire bump: receivers intersect with their
	// own KnownCapabilities + persist the result (see handleOffer).
	env := Envelope{
		Protocol:              HandshakeProtocol,
		Type:                  HandshakeTypeOffer,
		From:                  e.localBaseURL,
		FromDisplayName:       e.localDisplayName,
		FromPublicKeyPEM:      string(id.PublicKeyPEM()),
		To:                    url,
		Nonce:                 mustNonce(),
		Timestamp:             e.now().Format(time.RFC3339Nano),
		SupportedCapabilities: KnownCapabilities,
	}
	status, err := e.postEnvelope(ctx, url, env, id)
	if err == nil && status == handshakeCompleted {
		// Offers crossed and the peer says the pairing is complete.
		// That is true here only if the peer's confirm back has
		// already made our row connected with its real key.
		cur, gerr := e.registry.ByID(ctx, row.ID)
		switch {
		case gerr != nil:
			err = gerr
		case cur.Status != federation.PeerStatusConnected || cur.InstancePublicKey == pendingKeyPlaceholder:
			err = fmt.Errorf("%w: the peer answered %q, but its confirmation has not reached this instance; pair by URL again to re-send",
				ErrHandshakeAnswer, handshakeCompleted)
		default:
			row = cur
		}
	}
	if err != nil {
		_ = e.registry.appendNote(ctx, row.ID, "offer failed: "+err.Error())
		if cur, gerr := e.registry.ByID(ctx, row.ID); gerr == nil {
			row = cur
		}
		return row, fmt.Errorf("handshake: post offer: %w", err)
	}
	return row, nil
}

// AcceptInbound is called by the admin "Accept pending request"
// flow. POSTs a confirm envelope to the peer and flips our
// pending_inbound row to connected only once the peer has answered
// it "completed" (#1592). A failed confirm leaves the row
// pending_inbound, with the reason in its notes, so the admin can
// accept again; the peer answers a repeated confirm "completed".
func (e *Engine) AcceptInbound(ctx context.Context, p Peer) error {
	if _, err := e.identity.Get(); err != nil {
		return err
	}
	if p.Status != federation.PeerStatusPendingInbound {
		return fmt.Errorf("handshake: peer not in pending_inbound (current=%s)", p.Status)
	}
	if err := e.confirmTo(ctx, p.InstanceURL); err != nil {
		_ = e.registry.appendNote(ctx, p.ID, "confirm failed, request still pending: "+err.Error())
		return fmt.Errorf("handshake: post confirm: %w", err)
	}
	if _, err := e.registry.setStatus(ctx, p.ID, federation.PeerStatusConnected); err != nil {
		return err
	}
	return nil
}

// confirmTo POSTs a signed confirm (advertising KnownCapabilities)
// to peerURL and succeeds only on a "completed" answer.
func (e *Engine) confirmTo(ctx context.Context, peerURL string) error {
	id, err := e.identity.Get()
	if err != nil {
		return err
	}
	env := Envelope{
		Protocol:              HandshakeProtocol,
		Type:                  HandshakeTypeConfirm,
		From:                  e.localBaseURL,
		FromDisplayName:       e.localDisplayName,
		FromPublicKeyPEM:      string(id.PublicKeyPEM()),
		To:                    peerURL,
		Nonce:                 mustNonce(),
		Timestamp:             e.now().Format(time.RFC3339Nano),
		SupportedCapabilities: KnownCapabilities,
	}
	_, err = e.postEnvelope(ctx, peerURL, env, id)
	return err
}

// --- envelope helpers ----------------------------------------------------

// handshakeEnvelopeMembers is every member a handshake/v1 envelope
// may carry. Anything else is refused, signed or not: under v1 an
// unknown member is never silently dropped (#1592).
var handshakeEnvelopeMembers = map[string]bool{
	"protocol": true, "type": true, "from": true, "from_display_name": true,
	"from_public_key_pem": true, "to": true, "nonce": true, "timestamp": true,
	"supported_capabilities": true,
}

// parseAndVerify validates the on-wire JSON shape + signature +
// timestamp + protocol identifiers. Returns the inner envelope
// on success, with from and to normalised (normalizeInstanceURL).
//
// The signature is checked over the RFC 8785 canonical form of the
// envelope exactly as received (#1592), not over a re-encoding of
// the decoded struct, which would drop any member the struct does not
// know. Canonicalising also refuses duplicate member names, so the
// verified bytes and the decoded fields cannot be read two ways; the
// member allowlists are exact, because the typed decode matches names
// case-insensitively.
func (e *Engine) parseAndVerify(body []byte) (*Envelope, error) {
	if len(body) > handshakeBodyByteLimit {
		return nil, ErrHandshakeMalformed
	}
	if _, err := federation.Canonicalize(body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil || wire == nil {
		return nil, fmt.Errorf("%w: not a JSON object", ErrHandshakeMalformed)
	}
	for _, k := range sortedKeys(wire) {
		if k != "envelope" && k != "signature" {
			return nil, fmt.Errorf("%w: unsupported member %s", ErrHandshakeMalformed, quoteForOperator(k))
		}
	}
	raw, ok := wire["envelope"]
	if !ok {
		return nil, fmt.Errorf("%w: no envelope", ErrHandshakeMalformed)
	}
	var sigB64 string
	if err := json.Unmarshal(wire["signature"], &sigB64); err != nil || sigB64 == "" {
		return nil, fmt.Errorf("%w: signature missing or not a string", ErrHandshakeMalformed)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, fmt.Errorf("%w: envelope is not a JSON object", ErrHandshakeMalformed)
	}
	for _, k := range sortedKeys(members) {
		if !handshakeEnvelopeMembers[k] {
			return nil, fmt.Errorf("%w: unsupported envelope member %s", ErrHandshakeMalformed, quoteForOperator(k))
		}
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	if env.Protocol != HandshakeProtocol {
		return nil, fmt.Errorf("%w: %q", ErrHandshakeProtocol, env.Protocol)
	}
	if env.From == "" || env.FromPublicKeyPEM == "" || env.To == "" || env.Nonce == "" || env.Timestamp == "" {
		return nil, ErrHandshakeMalformed
	}
	ts, err := time.Parse(time.RFC3339Nano, env.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("%w: bad timestamp", ErrHandshakeMalformed)
	}
	if delta := e.now().Sub(ts); delta < -handshakeMaxSkew || delta > handshakeMaxSkew {
		return nil, ErrHandshakeStale
	}
	pub, err := federation.PublicKeyFromPEM([]byte(env.FromPublicKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("%w: bad from_public_key_pem", ErrHandshakeMalformed)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, fmt.Errorf("%w: signature not base64", ErrHandshakeMalformed)
	}
	canonical, err := federation.Canonicalize(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	if err := federation.Verify(pub, canonical, sig); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeBadSig, err)
	}
	// Normalised only now, after the signature held over the URLs as
	// sent; every lookup, insert and confirm back uses these forms.
	if env.From, err = normalizeInstanceURL(env.From); err != nil {
		return nil, fmt.Errorf("%w: bad from URL", ErrHandshakeMalformed)
	}
	if env.To, err = normalizeInstanceURL(env.To); err != nil {
		return nil, fmt.Errorf("%w: bad to URL", ErrHandshakeMalformed)
	}
	return &env, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// canonicalize produces the RFC 8785 canonical JSON form of the
// envelope. Same algorithm used elsewhere in the federation
// stack so signatures interoperate.
func canonicalize(env Envelope) ([]byte, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return federation.Canonicalize(raw)
}

// postEnvelope signs env, POSTs it to peerURL+HandshakePath and
// returns the answer's status, which is one this envelope type
// accepts: accepted_pending or completed for an offer, completed for
// a confirm. Anything else is ErrHandshakeAnswer with a sanitised
// reason an operator can act on (#1592).
func (e *Engine) postEnvelope(ctx context.Context, peerURL string, env Envelope, id *identity.Identity) (string, error) {
	canonical, err := canonicalize(env)
	if err != nil {
		return "", err
	}
	sig := id.Sign(canonical)
	body, err := json.Marshal(signedEnvelope{
		Envelope:  env,
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return "", err
	}
	target := peerURL + HandshakePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "artist-alley/handshake/v1")
	resp, err := e.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	status, err := readHandshakeAnswer(resp, env.Type)
	if err != nil {
		return "", fmt.Errorf("%w: POST %s: %s", ErrHandshakeAnswer, target, err.Error())
	}
	return status, nil
}

// readHandshakeAnswer validates a peer's answer: exactly 200,
// application/json, one JSON object of at most
// handshakeAnswerByteLimit bytes with a known status and a non-nil
// peer_id, and a status this envelope type accepts. Unknown extra
// fields are tolerated (the schema does not forbid them; §3.2's
// strictness governs envelopes, not HTTP answers). Never reads more
// than the limit plus one byte.
func readHandshakeAnswer(resp *http.Response, envType string) (string, error) {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return "", fmt.Errorf("peer answered HTTP %d, a redirect to %s; a handshake does not follow redirects, so check the instance URL",
			resp.StatusCode, quoteForOperator(resp.Header.Get("Location")))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("peer answered HTTP %d%s", resp.StatusCode, bodyExcerpt(resp.Body))
	}
	ct := resp.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
		return "", fmt.Errorf("peer answered HTTP 200 with Content-Type %s, not application/json: not an Artist Alley handshake endpoint (wrong instance URL, or a proxy serving a web page)",
			quoteForOperator(ct))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, handshakeAnswerByteLimit+1))
	if err != nil {
		return "", fmt.Errorf("reading the answer: %s", quoteForOperator(err.Error()))
	}
	if len(raw) > handshakeAnswerByteLimit {
		return "", fmt.Errorf("the answer is larger than %d bytes", handshakeAnswerByteLimit)
	}
	var a struct {
		Status *string `json:"status"`
		PeerID *string `json:"peer_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&a); err != nil {
		return "", fmt.Errorf("malformed answer: %s", quoteForOperator(err.Error()))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("malformed answer: trailing data after the JSON object")
	}
	if a.Status == nil {
		return "", errors.New("the answer has no status")
	}
	if a.PeerID == nil {
		return "", errors.New("the answer has no peer_id")
	}
	if pid, err := uuid.Parse(*a.PeerID); err != nil {
		return "", fmt.Errorf("the answer's peer_id %s is not a UUID", quoteForOperator(*a.PeerID))
	} else if pid == uuid.Nil {
		return "", errors.New("the answer's peer_id is the nil UUID")
	}
	status := *a.Status
	switch status {
	case handshakeAcceptedPending, handshakeCompleted, handshakeIgnoredDuplicate:
	default:
		return "", fmt.Errorf("the answer's status %s is not a handshake status", quoteForOperator(status))
	}
	switch {
	case envType == HandshakeTypeOffer && (status == handshakeAcceptedPending || status == handshakeCompleted):
		return status, nil
	case envType == HandshakeTypeConfirm && status == handshakeCompleted:
		return status, nil
	case envType == HandshakeTypeOffer && status == handshakeIgnoredDuplicate:
		return "", fmt.Errorf("the peer answered %q: it already holds a record of this instance that an offer does not reopen; its operator must remove it before pairing again",
			status)
	}
	return "", fmt.Errorf("the peer answered %q to a %s, not %q", status, envType, handshakeCompleted)
}

// bodyExcerpt quotes the start of a refusal body for the operator:
// at most handshakeQuoteLimit bytes, control characters removed.
func bodyExcerpt(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, handshakeQuoteLimit+1))
	if len(b) == 0 {
		return ""
	}
	cut := len(b) > handshakeQuoteLimit
	if cut {
		b = b[:handshakeQuoteLimit]
	}
	out := ": " + sanitizeForOperator(string(b))
	if cut {
		out += " (truncated)"
	}
	return out
}

// quoteForOperator quotes a peer-supplied value, bounded and with
// control characters removed.
func quoteForOperator(s string) string {
	if len(s) > handshakeQuoteLimit {
		s = s[:handshakeQuoteLimit]
	}
	return strconv.Quote(sanitizeForOperator(s))
}

func sanitizeForOperator(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// mustNonce returns a hex-encoded random nonce. Panics only on
// crypto/rand failure, which is unrecoverable.
func mustNonce() string {
	b := make([]byte, handshakeNonceBytes)
	if _, err := rand.Read(b); err != nil {
		panic("handshake: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// setStatus + appendNote live on peer.go now (Registry helpers).
// handshake.go only orchestrates the protocol; the registry
// methods own the DB writes + cache invalidation.
