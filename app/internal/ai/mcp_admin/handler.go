// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package mcpadmin is the HTTP surface for MCP-client registration
// (Phase 1.53.A):
//
//   GET    /admin/ai/mcp-clients              → list
//   POST   /admin/ai/mcp-clients              → register
//   PATCH  /admin/ai/mcp-clients/{id}         → update
//   DELETE /admin/ai/mcp-clients/{id}         → unregister
//   GET    /admin/ai/mcp-clients/{id}/tools         → list grants
//   PUT    /admin/ai/mcp-clients/{id}/tools/{tool}  → upsert grant
//   DELETE /admin/ai/mcp-clients/{id}/tools/{tool}  → remove grant
//
// Every endpoint gates on `mcp.client.admin` (seeded for Admin in
// migration 00013). Business logic lives in mcp_registry; this
// package handles auth + OpenAPI shape mapping only.

package mcpadmin

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	openapitypes "github.com/oapi-codegen/runtime/types"

	mcpregistry "github.com/mscrnt/artist-alley/app/internal/ai/mcp_registry"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CapMCPClientAdmin is the capability gating every endpoint here.
const CapMCPClientAdmin = "mcp.client.admin"

// Handler wires the admin endpoints to mcp_registry. Constructed by
// apiServer; nil-safe for tests that don't exercise these endpoints.
type Handler struct {
	registry *mcpregistry.Registry
}

// NewHandler builds an admin handler.
func NewHandler(registry *mcpregistry.Registry) *Handler {
	return &Handler{registry: registry}
}

// ---------------------------------------------------------------------------
// Auth helper
// ---------------------------------------------------------------------------

func (h *Handler) require(ctx context.Context) (*auth.Identity, *string) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		s := "authentication required"
		return nil, &s
	}
	if !id.Can(CapMCPClientAdmin) && !id.Can(auth.SuperAdminCapability) {
		s := CapMCPClientAdmin + " capability required"
		return nil, &s
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// GET /admin/ai/mcp-clients
// ---------------------------------------------------------------------------

func (h *Handler) ListMCPClients(
	ctx context.Context,
	_ openapi.ListMCPClientsRequestObject,
) (openapi.ListMCPClientsResponseObject, error) {
	if _, errMsg := h.require(ctx); errMsg != nil {
		return errResponseList(*errMsg, ctx), nil
	}
	servers, err := h.registry.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.MCPServer, 0, len(servers))
	for _, s := range servers {
		items = append(items, serverToAPI(s))
	}
	return openapi.ListMCPClients200JSONResponse(openapi.MCPServerList{Items: items}), nil
}

// ---------------------------------------------------------------------------
// POST /admin/ai/mcp-clients
// ---------------------------------------------------------------------------

func (h *Handler) RegisterMCPClient(
	ctx context.Context,
	req openapi.RegisterMCPClientRequestObject,
) (openapi.RegisterMCPClientResponseObject, error) {
	id, errMsg := h.require(ctx)
	if errMsg != nil {
		return errResponseRegister(*errMsg, ctx), nil
	}
	if req.Body == nil {
		return openapi.RegisterMCPClient400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}

	actor := id.UserRef
	created, err := h.registry.Insert(ctx, mcpregistry.InsertParams{
		Name:                 req.Body.Name,
		URL:                  req.Body.Url,
		Transport:            defaultStr(toStringPtr(req.Body.Transport), "http"),
		AuthKind:             defaultStr(toStringPtr(req.Body.AuthKind), "none"),
		AuthSecretRef:        derefStr(req.Body.AuthSecretRef),
		AuthHeaderName:       derefStr(req.Body.AuthHeaderName),
		PrivacyClass:         defaultStr(toStringPtr(req.Body.PrivacyClass), "cloud"),
		Enabled:              derefBool(req.Body.Enabled, false),
		RateLimitPerSecond:   int32(derefInt(req.Body.RateLimitPerSecond, 2)),
		RateLimitPerMinute:   int32(derefInt(req.Body.RateLimitPerMinute, 60)),
		HealthCheckIntervalS: int32(derefInt(req.Body.HealthCheckIntervalS, 60)),
		RegisteredByUserRef:  &actor,
	})
	if err != nil {
		if errors.Is(err, mcpregistry.ErrDuplicateName) {
			return openapi.RegisterMCPClient409JSONResponse{
				ConflictJSONResponse: openapi.ConflictJSONResponse{Error: "server name already registered"},
			}, nil
		}
		return nil, err
	}
	return openapi.RegisterMCPClient201JSONResponse(serverToAPI(created)), nil
}

// ---------------------------------------------------------------------------
// PATCH /admin/ai/mcp-clients/{id}
// ---------------------------------------------------------------------------

func (h *Handler) UpdateMCPClient(
	ctx context.Context,
	req openapi.UpdateMCPClientRequestObject,
) (openapi.UpdateMCPClientResponseObject, error) {
	if _, errMsg := h.require(ctx); errMsg != nil {
		return errResponseUpdate(*errMsg, ctx), nil
	}
	if req.Body == nil {
		return openapi.UpdateMCPClient404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "request body required"},
		}, nil
	}
	serverID := uuid.UUID(req.Id)
	params := mcpregistry.UpdateParams{ID: serverID}
	if req.Body.Url != nil {
		v := *req.Body.Url
		params.URL = &v
	}
	if req.Body.Transport != nil {
		v := string(*req.Body.Transport)
		params.Transport = &v
	}
	if req.Body.AuthKind != nil {
		v := string(*req.Body.AuthKind)
		params.AuthKind = &v
	}
	if req.Body.AuthSecretRef != nil {
		v := *req.Body.AuthSecretRef
		params.AuthSecretRef = &v
	}
	if req.Body.AuthHeaderName != nil {
		v := *req.Body.AuthHeaderName
		params.AuthHeaderName = &v
	}
	if req.Body.PrivacyClass != nil {
		v := string(*req.Body.PrivacyClass)
		params.PrivacyClass = &v
	}
	if req.Body.Enabled != nil {
		v := *req.Body.Enabled
		params.Enabled = &v
	}
	if req.Body.RateLimitPerSecond != nil {
		v := int32(*req.Body.RateLimitPerSecond)
		params.RateLimitPerSecond = &v
	}
	if req.Body.RateLimitPerMinute != nil {
		v := int32(*req.Body.RateLimitPerMinute)
		params.RateLimitPerMinute = &v
	}
	if req.Body.HealthCheckIntervalS != nil {
		v := int32(*req.Body.HealthCheckIntervalS)
		params.HealthCheckIntervalS = &v
	}
	updated, err := h.registry.Update(ctx, params)
	if err != nil {
		if errors.Is(err, mcpregistry.ErrServerNotFound) {
			return openapi.UpdateMCPClient404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "server not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.UpdateMCPClient200JSONResponse(serverToAPI(updated)), nil
}

// ---------------------------------------------------------------------------
// DELETE /admin/ai/mcp-clients/{id}
// ---------------------------------------------------------------------------

func (h *Handler) DeleteMCPClient(
	ctx context.Context,
	req openapi.DeleteMCPClientRequestObject,
) (openapi.DeleteMCPClientResponseObject, error) {
	if _, errMsg := h.require(ctx); errMsg != nil {
		return errResponseDelete(*errMsg, ctx), nil
	}
	serverID := uuid.UUID(req.Id)
	if err := h.registry.Delete(ctx, serverID); err != nil {
		return nil, err
	}
	return openapi.DeleteMCPClient204Response{}, nil
}

// ---------------------------------------------------------------------------
// GET /admin/ai/mcp-clients/{id}/tools
// ---------------------------------------------------------------------------

func (h *Handler) ListMCPClientToolGrants(
	ctx context.Context,
	req openapi.ListMCPClientToolGrantsRequestObject,
) (openapi.ListMCPClientToolGrantsResponseObject, error) {
	if _, errMsg := h.require(ctx); errMsg != nil {
		return errResponseListGrants(*errMsg, ctx), nil
	}
	serverID := uuid.UUID(req.Id)
	grants, err := h.registry.ListToolGrants(ctx, serverID)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.MCPServerToolGrant, 0, len(grants))
	for _, g := range grants {
		items = append(items, toolGrantToAPI(g))
	}
	return openapi.ListMCPClientToolGrants200JSONResponse(items), nil
}

// ---------------------------------------------------------------------------
// PUT /admin/ai/mcp-clients/{id}/tools/{tool}
// ---------------------------------------------------------------------------

func (h *Handler) UpsertMCPClientToolGrant(
	ctx context.Context,
	req openapi.UpsertMCPClientToolGrantRequestObject,
) (openapi.UpsertMCPClientToolGrantResponseObject, error) {
	if _, errMsg := h.require(ctx); errMsg != nil {
		return errResponseUpsertGrant(*errMsg, ctx), nil
	}
	if req.Body == nil {
		return openapi.UpsertMCPClientToolGrant404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "request body required"},
		}, nil
	}
	serverID := uuid.UUID(req.Id)
	addlCap := ""
	if req.Body.AdditionalCapability != nil {
		addlCap = *req.Body.AdditionalCapability
	}
	grant, err := h.registry.UpsertToolGrant(ctx, mcpregistry.UpsertToolGrantInput{
		ServerID:             serverID,
		ToolName:             req.Tool,
		AdditionalCapability: addlCap,
		CostEstimateMicros:   req.Body.CostEstimateMicros,
		Enabled:              req.Body.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpsertMCPClientToolGrant200JSONResponse(toolGrantToAPI(grant)), nil
}

// ---------------------------------------------------------------------------
// DELETE /admin/ai/mcp-clients/{id}/tools/{tool}
// ---------------------------------------------------------------------------

func (h *Handler) DeleteMCPClientToolGrant(
	ctx context.Context,
	req openapi.DeleteMCPClientToolGrantRequestObject,
) (openapi.DeleteMCPClientToolGrantResponseObject, error) {
	if _, errMsg := h.require(ctx); errMsg != nil {
		return errResponseDeleteGrant(*errMsg, ctx), nil
	}
	serverID := uuid.UUID(req.Id)
	if err := h.registry.DeleteToolGrant(ctx, serverID, req.Tool); err != nil {
		return nil, err
	}
	return openapi.DeleteMCPClientToolGrant204Response{}, nil
}

// ---------------------------------------------------------------------------
// Shape mappers
// ---------------------------------------------------------------------------

func serverToAPI(s mcpregistry.Server) openapi.MCPServer {
	out := openapi.MCPServer{
		Id:                   openapitypes.UUID(s.ID),
		Name:                 s.Name,
		Url:                  s.URL,
		Transport:            openapi.MCPServerTransport(s.Transport),
		AuthKind:             openapi.MCPServerAuthKind(s.AuthKind),
		PrivacyClass:         openapi.MCPServerPrivacyClass(s.PrivacyClass),
		Enabled:              s.Enabled,
		RateLimitPerSecond:   int(s.RateLimitPerSecond),
		RateLimitPerMinute:   int(s.RateLimitPerMinute),
		HealthCheckIntervalS: int(s.HealthCheckIntervalS),
		CreatedAt:            time.Time{}, // populated by row metadata in v2; v1 leaves zero
		UpdatedAt:            time.Time{},
	}
	if s.AuthSecretRef != "" {
		v := s.AuthSecretRef
		out.AuthSecretRef = &v
	}
	if s.AuthHeaderName != "" {
		v := s.AuthHeaderName
		out.AuthHeaderName = &v
	}
	if s.LastHealthStatus != "" {
		v := openapi.MCPServerLastHealthStatus(s.LastHealthStatus)
		out.LastHealthStatus = &v
	}
	if s.LastHealthError != "" {
		v := s.LastHealthError
		out.LastHealthError = &v
	}
	return out
}

func toolGrantToAPI(g mcpregistry.ToolGrant) openapi.MCPServerToolGrant {
	out := openapi.MCPServerToolGrant{
		ServerId:           openapitypes.UUID(g.ServerID),
		ToolName:           g.ToolName,
		CostEstimateMicros: g.CostEstimateMicros,
		Enabled:            g.Enabled,
	}
	if g.AdditionalCapability != "" {
		v := g.AdditionalCapability
		out.AdditionalCapability = &v
	}
	return out
}

// ---------------------------------------------------------------------------
// Auth-failure response helpers (one per operationID — codegen-typed)
// ---------------------------------------------------------------------------

func errResponseList(msg string, ctx context.Context) openapi.ListMCPClientsResponseObject {
	if isUnauth(ctx) {
		return openapi.ListMCPClients401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: msg},
		}
	}
	return openapi.ListMCPClients403JSONResponse{
		ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
	}
}

func errResponseRegister(msg string, ctx context.Context) openapi.RegisterMCPClientResponseObject {
	if isUnauth(ctx) {
		return openapi.RegisterMCPClient401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: msg},
		}
	}
	return openapi.RegisterMCPClient403JSONResponse{
		ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
	}
}

func errResponseUpdate(msg string, ctx context.Context) openapi.UpdateMCPClientResponseObject {
	if isUnauth(ctx) {
		return openapi.UpdateMCPClient401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: msg},
		}
	}
	return openapi.UpdateMCPClient403JSONResponse{
		ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
	}
}

func errResponseDelete(msg string, ctx context.Context) openapi.DeleteMCPClientResponseObject {
	if isUnauth(ctx) {
		return openapi.DeleteMCPClient401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: msg},
		}
	}
	return openapi.DeleteMCPClient403JSONResponse{
		ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
	}
}

func errResponseListGrants(msg string, ctx context.Context) openapi.ListMCPClientToolGrantsResponseObject {
	if isUnauth(ctx) {
		return openapi.ListMCPClientToolGrants401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: msg},
		}
	}
	return openapi.ListMCPClientToolGrants403JSONResponse{
		ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
	}
}

func errResponseUpsertGrant(msg string, ctx context.Context) openapi.UpsertMCPClientToolGrantResponseObject {
	if isUnauth(ctx) {
		return openapi.UpsertMCPClientToolGrant401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: msg},
		}
	}
	return openapi.UpsertMCPClientToolGrant403JSONResponse{
		ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
	}
}

func errResponseDeleteGrant(msg string, ctx context.Context) openapi.DeleteMCPClientToolGrantResponseObject {
	if isUnauth(ctx) {
		return openapi.DeleteMCPClientToolGrant401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: msg},
		}
	}
	return openapi.DeleteMCPClientToolGrant403JSONResponse{
		ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: msg},
	}
}

func isUnauth(ctx context.Context) bool {
	return auth.IdentityFromContext(ctx) == nil
}

// ---------------------------------------------------------------------------
// Pointer + default helpers
// ---------------------------------------------------------------------------

func toStringPtr[T ~string](p *T) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func defaultStr(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}
