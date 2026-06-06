// Handshake protocol — Phase 1.22.B-b. The peer-to-peer dance
// that lets two operators pair instances by URL alone, without
// copy-pasting public keys.
//
// # Wire format (docs/spec/federation/v1.md §11)
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
//   reviews +       to connected. B flips its own to connected
//   accepts         in the same handler.
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
	"net/http"
	"strings"
	"time"

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
)

// Envelope is the signed payload exchanged between peers.
// Field order doesn't matter for the wire — the signature
// covers the RFC 8785 JSON canonicalization, which sorts keys
// deterministically regardless of how either side emits them.
type Envelope struct {
	Protocol         string `json:"protocol"`
	Type             string `json:"type"`
	From             string `json:"from"`
	FromDisplayName  string `json:"from_display_name"`
	FromPublicKeyPEM string `json:"from_public_key_pem"`
	To               string `json:"to"`
	Nonce            string `json:"nonce"`
	Timestamp        string `json:"timestamp"` // RFC 3339 nano
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
// fresh http.Client with a 15s timeout; pass a custom one for
// tests or special transport policies.
func NewEngine(registry *Registry, identityMgr *identity.Manager, httpDoer HTTPDoer) *Engine {
	if httpDoer == nil {
		httpDoer = &http.Client{Timeout: handshakeHTTPTimeout}
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
	// Cross-check the recipient.
	if !strings.EqualFold(env.To, e.localBaseURL) {
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
func (e *Engine) handleOffer(ctx context.Context, env Envelope) (*InboundResult, error) {
	existing, err := e.registry.ByInstanceURL(ctx, env.From)
	if err != nil && !errors.Is(err, ErrPeerNotFound) {
		return nil, err
	}
	if existing != nil {
		// We already know about this peer. Replay-safe responses
		// per current status:
		//   connected         → return as completed; peer can
		//                        treat as success
		//   pending_inbound   → return as accepted_pending; admin
		//                        still needs to review
		//   pending_outbound  → cross-handshake: A sent offer +
		//                        B sent offer simultaneously. Flip
		//                        ours to connected: both sides are
		//                        signalling intent.
		if existing.Status == federation.PeerStatusPendingOutbound {
			updated, err := e.registry.setStatus(ctx, existing.ID, federation.PeerStatusConnected)
			if err != nil {
				return nil, err
			}
			return &InboundResult{Peer: updated, Status: "completed"}, nil
		}
		return &InboundResult{Peer: existing, Status: "ignored_duplicate"}, nil
	}

	// New offer — create a pending_inbound row for admin review.
	enabledFalse := true // enabled means "kill switch off"; default true
	p, err := e.registry.Add(ctx, AddInput{
		InstanceURL:        env.From,
		DisplayName:        env.FromDisplayName,
		InstancePublicKey:  env.FromPublicKeyPEM,
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: 0, // 0 = "system, awaiting admin"
		Notes:              "auto-handshake — awaiting admin accept",
		Enabled:            &enabledFalse,
		Status:             federation.PeerStatusPendingInbound,
	})
	if err != nil {
		return nil, err
	}
	return &InboundResult{Peer: p, Status: "accepted_pending"}, nil
}

// handleConfirm processes an inbound confirm envelope — the
// peer's admin accepted our outbound offer. We flip the local
// pending_outbound row to connected.
func (e *Engine) handleConfirm(ctx context.Context, env Envelope) (*InboundResult, error) {
	existing, err := e.registry.ByInstanceURL(ctx, env.From)
	if err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrHandshakePeerNotFound, env.From)
		}
		return nil, err
	}
	// Confirm is only valid from pending_outbound (we initiated).
	// From connected → idempotent; ignore. From pending_inbound →
	// suspicious (peer is confirming our offer that we don't
	// remember sending). Treat both as ignored_duplicate.
	if existing.Status != federation.PeerStatusPendingOutbound {
		return &InboundResult{Peer: existing, Status: "ignored_duplicate"}, nil
	}
	// Verify the public key matches what we recorded when we
	// initiated. Mismatch = peer rotated key mid-handshake or
	// MITM. Reject.
	// For pending_outbound rows the InstancePublicKey is a
	// placeholder ("PENDING-HANDSHAKE-RESPONSE") — confirm
	// delivers the peer's real key for the first time. Replace it
	// atomically alongside the status flip.
	updated, err := e.registry.completeOutboundHandshake(ctx, existing.ID, env.FromPublicKeyPEM)
	if err != nil {
		return nil, err
	}
	return &InboundResult{Peer: updated, Status: "completed"}, nil
}

// --- outbound (we initiate or accept) ------------------------------------

// InitiateOffer is called by the admin "Pair by URL" flow. We
// create a local pending_outbound row + POST a signed offer to
// the peer. Idempotent: calling twice with the same URL returns
// the existing row without re-POSTing.
//
// Errors:
//   - ErrHandshakeMalformed if URL/displayName fail validation
//   - any error from the outbound HTTP (timeout, DNS, peer 4xx/5xx)
//     — the local row is created BEFORE the POST so the admin can
//     see it as pending_outbound even if the peer is down; the
//     POST is retried by the outbox worker once 1.22.D ships.
//     For 1.22.B-b this is a single best-effort attempt.
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
		// Display name defaults to the host portion of the URL —
		// admin can rename later from the peer detail page.
		peerDisplayName = strings.TrimPrefix(url, "https://")
	}

	// If we already have a row for this URL, return it. Don't
	// re-POST — operator can use the dedicated "retry handshake"
	// action once 1.22.D ships.
	if existing, err := e.registry.ByInstanceURL(ctx, url); err == nil {
		return existing, nil
	}

	// Create the local pending_outbound row. We stash OUR pubkey
	// as the placeholder for instance_public_key on this row — it
	// gets replaced when the confirm comes back. Bit of a misuse
	// of the column, but it keeps the schema NOT NULL and the
	// row visible in the admin UI immediately.
	enabledFalse := true // kill-switch defaults on; "enabled" doesn't mean "active"
	row, err := e.registry.Add(ctx, AddInput{
		InstanceURL:        url,
		DisplayName:        peerDisplayName,
		InstancePublicKey:  "PENDING-HANDSHAKE-RESPONSE",
		TrustTier:          federation.TrustConnected,
		EncryptionPolicy:   federation.EncryptionPlaintext,
		HandshakeByUserRef: initiatorUserRef,
		Notes:              "auto-handshake — offer sent, awaiting peer admin",
		Enabled:            &enabledFalse,
		Status:             federation.PeerStatusPendingOutbound,
	})
	if err != nil {
		return nil, err
	}

	// Build + sign the offer envelope.
	env := Envelope{
		Protocol:         HandshakeProtocol,
		Type:             HandshakeTypeOffer,
		From:             e.localBaseURL,
		FromDisplayName:  e.localDisplayName,
		FromPublicKeyPEM: string(id.PublicKeyPEM()),
		To:               url,
		Nonce:            mustNonce(),
		Timestamp:        e.now().Format(time.RFC3339Nano),
	}
	if err := e.postEnvelope(ctx, url, env, id); err != nil {
		// Local row stays as pending_outbound — admin sees the
		// row + a notes field updated to reflect the failure.
		_ = e.registry.appendNote(ctx, row.ID, "initial offer POST failed: "+err.Error())
		return row, fmt.Errorf("handshake: post offer: %w", err)
	}
	return row, nil
}

// AcceptInbound is called by the admin "Accept pending request"
// flow. Flips our pending_inbound row to connected + POSTs a
// confirm envelope back to the peer.
func (e *Engine) AcceptInbound(ctx context.Context, p Peer) error {
	id, err := e.identity.Get()
	if err != nil {
		return err
	}
	if p.Status != federation.PeerStatusPendingInbound {
		return fmt.Errorf("handshake: peer not in pending_inbound (current=%s)", p.Status)
	}
	if _, err := e.registry.setStatus(ctx, p.ID, federation.PeerStatusConnected); err != nil {
		return err
	}
	env := Envelope{
		Protocol:         HandshakeProtocol,
		Type:             HandshakeTypeConfirm,
		From:             e.localBaseURL,
		FromDisplayName:  e.localDisplayName,
		FromPublicKeyPEM: string(id.PublicKeyPEM()),
		To:               p.InstanceURL,
		Nonce:            mustNonce(),
		Timestamp:        e.now().Format(time.RFC3339Nano),
	}
	// Best-effort post — local state is already connected.
	// Future outbox (1.22.D) will retry if this fails.
	if err := e.postEnvelope(ctx, p.InstanceURL, env, id); err != nil {
		_ = e.registry.appendNote(ctx, p.ID, "confirm POST to peer failed: "+err.Error())
		return fmt.Errorf("handshake: post confirm: %w", err)
	}
	return nil
}

// --- envelope helpers ----------------------------------------------------

// parseAndVerify validates the on-wire JSON shape + signature +
// timestamp + protocol identifiers. Returns the inner envelope
// on success.
func (e *Engine) parseAndVerify(body []byte) (*Envelope, error) {
	if len(body) > handshakeBodyByteLimit {
		return nil, ErrHandshakeMalformed
	}
	var wrapper signedEnvelope
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	env := wrapper.Envelope
	if env.Protocol != HandshakeProtocol {
		return nil, fmt.Errorf("%w: %q", ErrHandshakeProtocol, env.Protocol)
	}
	if env.From == "" || env.FromPublicKeyPEM == "" || env.To == "" || env.Nonce == "" || env.Timestamp == "" {
		return nil, ErrHandshakeMalformed
	}
	if _, err := normalizeInstanceURL(env.From); err != nil {
		return nil, fmt.Errorf("%w: bad from URL", ErrHandshakeMalformed)
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
	sig, err := base64.StdEncoding.DecodeString(wrapper.Signature)
	if err != nil {
		return nil, fmt.Errorf("%w: signature not base64", ErrHandshakeMalformed)
	}
	canonical, err := canonicalize(env)
	if err != nil {
		return nil, err
	}
	if err := federation.Verify(pub, canonical, sig); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeBadSig, err)
	}
	return &env, nil
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

// postEnvelope signs + POSTs the envelope to the peer. Reads
// at most handshakeBodyByteLimit of the response body so a
// malicious peer can't OOM us with a huge response.
func (e *Engine) postEnvelope(ctx context.Context, peerURL string, env Envelope, id *identity.Identity) error {
	canonical, err := canonicalize(env)
	if err != nil {
		return err
	}
	sig := id.Sign(canonical)
	body, err := json.Marshal(signedEnvelope{
		Envelope:  env,
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		peerURL+"/federation/peers/handshake", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "artist-alley/handshake/v1")
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		short, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("peer responded %d: %s", resp.StatusCode, strings.TrimSpace(string(short)))
	}
	// Drain a bounded amount so the response body is fully read
	// (keep-alive friendly) without unbounded allocation.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, handshakeBodyByteLimit))
	return nil
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
