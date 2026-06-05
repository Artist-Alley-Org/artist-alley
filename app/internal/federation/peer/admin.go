// HTTP admin surface for the federation peers registry —
// /admin/federation/peers CRUD. Phase 1.22.B-a.
//
// Gated on system.admin per ADR 0044's audit surface convention.
// A finer-grained federation.admin capability split is reserved
// for the moderation phase but not needed at v1.

package peer

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// AdminHandler is the openapi-strict adapter for the four
// federation peers admin endpoints.
type AdminHandler struct {
	registry *Registry
}

// NewAdminHandler wires the admin handler to the registry.
func NewAdminHandler(r *Registry) *AdminHandler {
	return &AdminHandler{registry: r}
}

const capAdmin = "system.admin"

// ListFederationPeers — GET /admin/federation/peers.
func (h *AdminHandler) ListFederationPeers(
	ctx context.Context,
	req openapi.ListFederationPeersRequestObject,
) (openapi.ListFederationPeersResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListFederationPeers401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.ListFederationPeers403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	limit := 50
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	peers, err := h.registry.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.FederationPeer, len(peers))
	for i, p := range peers {
		items[i] = peerToAPI(p)
	}
	return openapi.ListFederationPeers200JSONResponse(openapi.FederationPeerList{Items: items}), nil
}

// GetFederationPeer — GET /admin/federation/peers/{id}.
func (h *AdminHandler) GetFederationPeer(
	ctx context.Context,
	req openapi.GetFederationPeerRequestObject,
) (openapi.GetFederationPeerResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetFederationPeer401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.GetFederationPeer403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	p, err := h.registry.ByID(ctx, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			return openapi.GetFederationPeer404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "peer not found"},
			}, nil
		}
		return nil, err
	}
	out := peerToAPI(*p)
	return openapi.GetFederationPeer200JSONResponse(out), nil
}

// CreateFederationPeer — POST /admin/federation/peers.
//
// Maps:
//   - ErrInstanceURLInvalid / ErrInstancePublicKeyInvalid /
//     ErrTrustTierInvalid / ErrEncryptionPolicyInvalid → 400
//   - UNIQUE-violation on instance_url → 409
//   - everything else → 500
func (h *AdminHandler) CreateFederationPeer(
	ctx context.Context,
	req openapi.CreateFederationPeerRequestObject,
) (openapi.CreateFederationPeerResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateFederationPeer401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.CreateFederationPeer403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.CreateFederationPeer400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}

	// Defaults — openapi defaults aren't autowired into request bodies.
	tier := federation.TrustConnected
	if req.Body.TrustTier != nil {
		tier = federation.TrustTier(*req.Body.TrustTier)
	}
	enc := federation.EncryptionPlaintext
	if req.Body.EncryptionPolicy != nil {
		enc = federation.EncryptionPolicy(*req.Body.EncryptionPolicy)
	}
	notes := ""
	if req.Body.Notes != nil {
		notes = *req.Body.Notes
	}
	var enabled *bool
	if req.Body.Enabled != nil {
		v := *req.Body.Enabled
		enabled = &v
	}

	p, err := h.registry.Add(ctx, AddInput{
		InstanceURL:        req.Body.InstanceUrl,
		DisplayName:        req.Body.DisplayName,
		InstancePublicKey:  req.Body.InstancePublicKey,
		TrustTier:          tier,
		EncryptionPolicy:   enc,
		HandshakeByUserRef: id.UserRef,
		Notes:              notes,
		Enabled:            enabled,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInstanceURLInvalid),
			errors.Is(err, ErrInstancePublicKeyInvalid),
			errors.Is(err, ErrTrustTierInvalid),
			errors.Is(err, ErrEncryptionPolicyInvalid):
			return openapi.CreateFederationPeer400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		}
		if isUniqueViolation(err) {
			return openapi.CreateFederationPeer409JSONResponse{Error: "a peer with this instance_url already exists"}, nil
		}
		return nil, err
	}
	out := peerToAPI(*p)
	return openapi.CreateFederationPeer201JSONResponse(out), nil
}

// UpdateFederationPeer — PATCH /admin/federation/peers/{id}.
func (h *AdminHandler) UpdateFederationPeer(
	ctx context.Context,
	req openapi.UpdateFederationPeerRequestObject,
) (openapi.UpdateFederationPeerResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UpdateFederationPeer401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.UpdateFederationPeer403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.UpdateFederationPeer400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}
	in := UpdateInput{
		DisplayName: req.Body.DisplayName,
		Enabled:     req.Body.Enabled,
		Notes:       req.Body.Notes,
	}
	if req.Body.TrustTier != nil {
		v := federation.TrustTier(*req.Body.TrustTier)
		in.TrustTier = &v
	}
	if req.Body.EncryptionPolicy != nil {
		v := federation.EncryptionPolicy(*req.Body.EncryptionPolicy)
		in.EncryptionPolicy = &v
	}
	p, err := h.registry.Update(ctx, uuid.UUID(req.Id), in)
	if err != nil {
		switch {
		case errors.Is(err, ErrPeerNotFound):
			return openapi.UpdateFederationPeer404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "peer not found"},
			}, nil
		case errors.Is(err, ErrTrustTierInvalid),
			errors.Is(err, ErrEncryptionPolicyInvalid):
			return openapi.UpdateFederationPeer400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		}
		return nil, err
	}
	out := peerToAPI(*p)
	return openapi.UpdateFederationPeer200JSONResponse(out), nil
}

// DeleteFederationPeer — DELETE /admin/federation/peers/{id}.
func (h *AdminHandler) DeleteFederationPeer(
	ctx context.Context,
	req openapi.DeleteFederationPeerRequestObject,
) (openapi.DeleteFederationPeerResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DeleteFederationPeer401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.DeleteFederationPeer403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if err := h.registry.Delete(ctx, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			return openapi.DeleteFederationPeer404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "peer not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.DeleteFederationPeer204Response{}, nil
}

// --- helpers -------------------------------------------------------------

func peerToAPI(p Peer) openapi.FederationPeer {
	out := openapi.FederationPeer{
		Id:                  p.ID,
		InstanceUrl:         p.InstanceURL,
		DisplayName:         p.DisplayName,
		InstancePublicKey:   p.InstancePublicKey,
		TrustTier:           openapi.FederationPeerTrustTier(p.TrustTier),
		EncryptionPolicy:    openapi.FederationPeerEncryptionPolicy(p.EncryptionPolicy),
		Enabled:             p.Enabled,
		HandshakeByUserRef:  p.HandshakeByUserRef,
		Notes:               p.Notes,
	}
	if p.HandshakeAt.Valid {
		out.HandshakeAt = p.HandshakeAt.Time
	}
	if p.LastSeenAt.Valid {
		t := p.LastSeenAt.Time
		out.LastSeenAt = &t
	}
	return out
}

// isUniqueViolation detects a Postgres unique-constraint
// violation (SQLSTATE 23505) so the handler can map it to 409.
// Kept inline here because peer is the only consumer; if more
// packages need it we promote to a shared helper.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// Fallback string match for drivers that wrap without the
	// typed error — unlikely but defensive.
	return strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}
