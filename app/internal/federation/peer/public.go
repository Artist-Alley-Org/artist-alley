// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Public (unauthenticated) federation endpoints — Phase
// 1.22.B-b. Two handlers:
//
//   GET  /api/v1/federation/instance          actor doc (pubkey, URL, name)
//   POST /api/v1/federation/peers/handshake   peer-to-peer handshake
//
// Both are reachable WITHOUT login/cookies/tokens — the federation
// protocol bootstraps trust before any auth context exists.

package peer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// PublicHandler is the openapi-strict adapter for the public
// federation endpoints. Constructed at boot with the identity
// manager + handshake engine + a localBaseURL/displayName
// resolver (so it can compose the actor doc on each request
// rather than caching a baseURL that might be reconfigured).
type PublicHandler struct {
	identity *identity.Manager
	engine   *Engine
	registry *Registry // needed for /federation/peers/visible

	// localBaseURLFn + localDisplayNameFn read live values per
	// request — sysconfig is the source of truth + already cached
	// at that layer. We don't pre-resolve at construction so
	// admin edits to the site name show up immediately in the
	// actor doc.
	localBaseURLFn     func(ctx context.Context) string
	localDisplayNameFn func(ctx context.Context) string
}

// NewPublicHandler wires the public federation HTTP surface.
func NewPublicHandler(
	idMgr *identity.Manager,
	eng *Engine,
	reg *Registry,
	baseURL func(ctx context.Context) string,
	displayName func(ctx context.Context) string,
) *PublicHandler {
	return &PublicHandler{
		identity:           idMgr,
		engine:             eng,
		registry:           reg,
		localBaseURLFn:     baseURL,
		localDisplayNameFn: displayName,
	}
}

// GetFederationPeersVisible — GET /federation/peers/visible.
// Public unauthenticated endpoint (1.22.B-d). Returns the union
// of peers we've opted to expose via share_in_visible_list.
//
// Per ADR 0043 these are advisory — the receiving peer treats
// them as discovery hints, not trust statements.
func (h *PublicHandler) GetFederationPeersVisible(
	ctx context.Context,
	_ openapi.GetFederationPeersVisibleRequestObject,
) (openapi.GetFederationPeersVisibleResponseObject, error) {
	visible, err := h.registry.VisibleSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.FederationVisiblePeer, 0, len(visible))
	for _, p := range visible {
		items = append(items, openapi.FederationVisiblePeer{
			InstanceUrl:       p.InstanceURL,
			DisplayName:       p.DisplayName,
			InstancePublicKey: p.InstancePublicKey,
			Fingerprint:       fingerprintFromPEM(p.InstancePublicKey),
		})
	}
	return openapi.GetFederationPeersVisible200JSONResponse(openapi.FederationVisiblePeerList{
		Peers: items,
	}), nil
}

// fingerprintFromPEM extracts the Ed25519 fingerprint by parsing
// the PEM. Empty string on parse failure (legacy/placeholder
// keys for in-flight pairings).
func fingerprintFromPEM(pem string) string {
	pub, err := federation.PublicKeyFromPEM([]byte(pem))
	if err != nil {
		return ""
	}
	return federation.PublicKeyFingerprint(pub)
}

// GetFederationInstance — GET /federation/instance.
//
// Returns 503 (and an Error body) when the instance identity
// hasn't been generated yet — that's the first-boot race window
// between Manager.Load and HTTP listener start.
func (h *PublicHandler) GetFederationInstance(
	ctx context.Context,
	_ openapi.GetFederationInstanceRequestObject,
) (openapi.GetFederationInstanceResponseObject, error) {
	id, err := h.identity.Get()
	if err != nil {
		return openapi.GetFederationInstance503JSONResponse{
			Error: "federation instance identity not yet generated",
		}, nil
	}
	url := ""
	if h.localBaseURLFn != nil {
		url = h.localBaseURLFn(ctx)
	}
	name := ""
	if h.localDisplayNameFn != nil {
		name = h.localDisplayNameFn(ctx)
	}
	doc := openapi.FederationInstanceDoc{
		InstanceUrl:     url,
		DisplayName:     name,
		PublicKeyPem:    string(id.PublicKeyPEM()),
		Fingerprint:     id.Fingerprint(),
		ProtocolVersion: "aa-fed/v1",
	}
	return openapi.GetFederationInstance200JSONResponse(doc), nil
}

// ServeHandshake is POST /api/v1/federation/peers/handshake as the
// production router mounts it (#1592): a raw handler registered over
// the strict one, because the signature covers the sender's bytes.
// The strict adapter hands PostFederationHandshake the body decoded
// into the generated HandshakeEnvelope, and re-encoding that drops
// every member the schema does not list (supported_capabilities among
// them), so no current envelope would verify. The answers are the
// operation's own 200 / 400 / 503 responses.
func (h *PublicHandler) ServeHandshake(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, handshakeBodyByteLimit+1))
	if err != nil {
		_ = openapi.PostFederationHandshake400JSONResponse{Error: "could not read the request body"}.
			VisitPostFederationHandshakeResponse(w)
		return
	}
	resp, err := h.handshake(r.Context(), body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_ = resp.VisitPostFederationHandshakeResponse(w)
}

// PostFederationHandshake is the strict-server form of the same
// operation, required by StrictServerInterface. The router serves
// ServeHandshake instead (see above); this re-encodes the decoded
// body, which only verifies for an envelope whose every member is in
// the schema.
func (h *PublicHandler) PostFederationHandshake(
	ctx context.Context,
	req openapi.PostFederationHandshakeRequestObject,
) (openapi.PostFederationHandshakeResponseObject, error) {
	if req.Body == nil {
		return openapi.PostFederationHandshake400JSONResponse{
			Error: "request body required",
		}, nil
	}
	body, err := json.Marshal(req.Body)
	if err != nil {
		return openapi.PostFederationHandshake400JSONResponse{
			Error: "could not re-encode envelope: " + err.Error(),
		}, nil
	}
	return h.handshake(ctx, body)
}

// handshake verifies and applies one signed envelope (its exact
// bytes) and maps the outcome to the operation's responses.
func (h *PublicHandler) handshake(ctx context.Context, body []byte) (openapi.PostFederationHandshakeResponseObject, error) {
	if _, err := h.identity.Get(); err != nil {
		return openapi.PostFederationHandshake503JSONResponse{
			Error: "federation instance identity not yet generated",
		}, nil
	}
	if len(body) == 0 {
		return openapi.PostFederationHandshake400JSONResponse{
			Error: "request body required",
		}, nil
	}
	result, err := h.engine.HandleInbound(ctx, body)
	if err != nil {
		// Every ErrHandshake* maps to 400: the caller sent
		// something we couldn't trust or complete. Opaque DB errors
		// bubble out as 500.
		if errors.Is(err, ErrHandshakeProtocol) ||
			errors.Is(err, ErrHandshakeType) ||
			errors.Is(err, ErrHandshakeWrongRecipient) ||
			errors.Is(err, ErrHandshakeStale) ||
			errors.Is(err, ErrHandshakeBadSig) ||
			errors.Is(err, ErrHandshakeMalformed) ||
			errors.Is(err, ErrHandshakePeerNotFound) ||
			errors.Is(err, ErrHandshakeKeyConflict) ||
			errors.Is(err, ErrHandshakeConfirmBack) {
			return openapi.PostFederationHandshake400JSONResponse{Error: err.Error()}, nil
		}
		return nil, err
	}
	return openapi.PostFederationHandshake200JSONResponse(openapi.HandshakeAccepted{
		Status: openapi.HandshakeAcceptedStatus(result.Status),
		PeerId: result.Peer.ID,
	}), nil
}
