// Defederation cascade-preview handler — Phase 1.22.C-d.
// Backs the modal in the 1.22.C design proposal §8.5.
//
// The actual cascade (the chunked-job that revokes 500 shares
// per tx + emits aa:Unshare per row + DELETEs the peer row)
// lives with the peer-admin handler — this file is just the
// READ-ONLY summary that the admin reviews BEFORE confirming.

package shares

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// PendingHandshakeCounter + SuggestionCounter are the two
// cross-package adapters the preview needs. Boot wires them
// against peer.Registry + p2p.Registry respectively so the
// shares package doesn't import those packages directly.
type PendingHandshakeCounter func(ctx context.Context, peerID uuid.UUID) (int, error)
type SuggestionCounter func(ctx context.Context, peerID uuid.UUID) (int, error)
type PeerDisplay func(ctx context.Context, peerID uuid.UUID) (displayName, url string, err error)

// SetDefederationDeps wires the cross-package counters that the
// preview endpoint needs. Boot calls this once; nil-safe (the
// preview endpoint returns 503 if not wired).
func (h *AdminHandler) SetDefederationDeps(
	pending PendingHandshakeCounter,
	suggestions SuggestionCounter,
	peerDisplay PeerDisplay,
) {
	h.pendingHandshakeCounter = pending
	h.suggestionCounter = suggestions
	h.peerDisplay = peerDisplay
}

// PreviewFederationPeerDefederation — GET /admin/federation/peers/{id}/defederation-preview.
func (h *AdminHandler) PreviewFederationPeerDefederation(
	ctx context.Context,
	req openapi.PreviewFederationPeerDefederationRequestObject,
) (openapi.PreviewFederationPeerDefederationResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.PreviewFederationPeerDefederation401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.PreviewFederationPeerDefederation403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if h.pendingHandshakeCounter == nil || h.suggestionCounter == nil || h.peerDisplay == nil {
		return nil, errors.New("shares.PreviewDefederation: cross-package deps not wired (call SetDefederationDeps at boot)")
	}
	peerID := uuid.UUID(req.Id)

	// Peer display first — if the peer doesn't exist, surface
	// 404 before the other counts do anything.
	displayName, peerURL, err := h.peerDisplay(ctx, peerID)
	if err != nil {
		return openapi.PreviewFederationPeerDefederation404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "peer not found"},
		}, nil
	}

	// The three counts. None of them block each other; ran
	// serially because they're each indexed sub-ms reads.
	breakdown, err := h.registry.CountsByPeerBreakdown(ctx, peerID)
	if err != nil {
		return nil, fmt.Errorf("shares.PreviewDefederation: breakdown: %w", err)
	}
	total := int64(0)
	for _, n := range breakdown {
		total += n
	}
	pending, err := h.pendingHandshakeCounter(ctx, peerID)
	if err != nil {
		return nil, fmt.Errorf("shares.PreviewDefederation: pending handshakes: %w", err)
	}
	suggestions, err := h.suggestionCounter(ctx, peerID)
	if err != nil {
		return nil, fmt.Errorf("shares.PreviewDefederation: suggestions: %w", err)
	}

	out := openapi.FederationDefederationPreview{
		PeerId:            uuid.UUID(peerID),
		PeerDisplayName:   displayName,
		PeerUrl:           peerURL,
		TotalActiveShares: total,
		PendingHandshakes: pending,
		CachedSuggestions: suggestions,
		SharesByKind:      countsToInt64Map(breakdown),
	}
	return openapi.PreviewFederationPeerDefederation200JSONResponse(out), nil
}

func countsToInt64Map(in map[federation.ShareObjectKind]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}
