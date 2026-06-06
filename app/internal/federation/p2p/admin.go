// Admin HTTP surface for peer-of-peer discovery — Phase 1.22.B-d.

package p2p

import (
	"context"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const capAdmin = "system.admin"

// AdminHandler is the openapi-strict adapter for the suggestions
// endpoints.
type AdminHandler struct {
	registry *Registry
	client   *Client
}

// NewAdminHandler wires the admin surface.
func NewAdminHandler(r *Registry, c *Client) *AdminHandler {
	return &AdminHandler{registry: r, client: c}
}

// ListFederationPeerSuggestions — GET /admin/federation/suggestions.
func (h *AdminHandler) ListFederationPeerSuggestions(
	ctx context.Context,
	req openapi.ListFederationPeerSuggestionsRequestObject,
) (openapi.ListFederationPeerSuggestionsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListFederationPeerSuggestions401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.ListFederationPeerSuggestions403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	limit := int32(200)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	suggestions, err := h.registry.ListSuggestions(ctx, limit)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.FederationPeerSuggestion, len(suggestions))
	for i, s := range suggestions {
		items[i] = openapi.FederationPeerSuggestion{
			Id:                   s.ID,
			SourcePeerId:         s.SourcePeerID,
			SourceDisplayName:    s.SourceDisplayName,
			SourceUrl:            s.SourceURL,
			SuggestedUrl:         s.SuggestedURL,
			SuggestedDisplayName: s.SuggestedDisplayName,
			SuggestedPublicKey:   s.SuggestedPublicKey,
			SuggestedFingerprint: s.SuggestedFingerprint,
		}
		if s.CachedAt.Valid {
			items[i].CachedAt = s.CachedAt.Time
		}
	}
	return openapi.ListFederationPeerSuggestions200JSONResponse(openapi.FederationPeerSuggestionList{
		Items: items,
	}), nil
}

// RefreshFederationPeerSuggestions — POST /admin/federation/
// suggestions/refresh.
func (h *AdminHandler) RefreshFederationPeerSuggestions(
	ctx context.Context,
	_ openapi.RefreshFederationPeerSuggestionsRequestObject,
) (openapi.RefreshFederationPeerSuggestionsResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RefreshFederationPeerSuggestions401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.RefreshFederationPeerSuggestions403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	outcomes := h.client.RefreshAll(ctx, h.registry)
	rows := make([]struct {
		Count             int                 `json:"count"`
		Error             *string             `json:"error,omitempty"`
		SourceDisplayName string              `json:"source_display_name"`
		SourcePeerId      openapi_types.UUID  `json:"source_peer_id"`
		SourceUrl         string              `json:"source_url"`
	}, len(outcomes))
	for i, o := range outcomes {
		rows[i].Count = o.Count
		rows[i].SourceDisplayName = o.SourceDisplayName
		rows[i].SourcePeerId = openapi_types.UUID(o.SourcePeerID)
		rows[i].SourceUrl = o.SourceURL
		if o.Error != "" {
			e := o.Error
			rows[i].Error = &e
		}
	}
	return openapi.RefreshFederationPeerSuggestions200JSONResponse(openapi.FederationPeerSuggestionRefreshResult{
		Outcomes: rows,
	}), nil
}
