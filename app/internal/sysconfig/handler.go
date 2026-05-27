// Package sysconfig HTTP surface — read/write the per-install
// settings (site, SMTP, auth, AI) via the admin API.
//
// All endpoints are double-gated:
//   * Authenticated caller (401 if anonymous).
//   * Capability check (403 if not held). The wildcard `system.admin`
//     short-circuits every check; the fine-grained caps
//     (`system.config.read`, `system.config.write`, `system.auth.write`,
//     `system.ai.write`) exist so future roles can mutate one section
//     without seeing keys in another.
//
// No HTTP-layer caching today — the settings are admin-only and read
// rarely. The auth + AI configs WILL get the cache.Registry treatment
// when the login flow / AI client starts reading them on every
// request; that's a follow-up phase.

package sysconfig

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Capability codes. Stable strings; seeded by migration 00023.
const (
	CapConfigRead  = "system.config.read"
	CapConfigWrite = "system.config.write"
	CapAuthWrite   = "system.auth.write"
	CapAIWrite     = "system.ai.write"
	CapSystemAdmin = "system.admin"
)

// Handler implements the system-config slice of the API.
type Handler struct {
	Pool   *pgxpool.Pool
	Store  *Store
	Logger *slog.Logger
}

// NewHTTPHandler returns a Handler wired against an existing Store —
// `NewHandler` is reserved by some packages for the Store factory, so
// the HTTP surface uses the longer name.
func NewHTTPHandler(pool *pgxpool.Pool, store *Store, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Store: store, Logger: logger}
}

// requireCap rejects callers who lack the required capability OR
// `system.admin`. Returns nil when the caller is authorised and may
// proceed; otherwise the first return is a 401/403 response object
// that the caller should return as-is.
func (h *Handler) requireCap(ctx context.Context, cap string) (*auth.Identity, error401or403) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return nil, errUnauthenticated{}
	}
	if !id.Can(cap) && !id.Can(CapSystemAdmin) {
		return nil, errForbidden{Cap: cap}
	}
	return id, nil
}

// Sentinel result types so each operation can convert them to its
// own openapi response shape.
type error401or403 interface{ is401or403() }
type errUnauthenticated struct{}
type errForbidden struct{ Cap string }

func (errUnauthenticated) is401or403() {}
func (errForbidden) is401or403()       {}

// ---------------------------------------------------------------------------
// Site
// ---------------------------------------------------------------------------

func (h *Handler) GetSiteConfig(
	ctx context.Context,
	_ openapi.GetSiteConfigRequestObject,
) (openapi.GetSiteConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return siteConfigDenial(denied), nil
	}
	site, err := h.Store.GetSite(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get site: %w", err)
	}
	return openapi.GetSiteConfig200JSONResponse(siteToAPI(site)), nil
}

func (h *Handler) UpdateSiteConfig(
	ctx context.Context,
	req openapi.UpdateSiteConfigRequestObject,
) (openapi.UpdateSiteConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigWrite); denied != nil {
		return siteConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateSiteConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	site := apiToSite(*req.Body)
	if err := h.Store.SetSite(ctx, site); err != nil {
		return nil, fmt.Errorf("sysconfig: set site: %w", err)
	}
	return openapi.UpdateSiteConfig200JSONResponse(siteToAPI(site)), nil
}

// ---------------------------------------------------------------------------
// SMTP
// ---------------------------------------------------------------------------

func (h *Handler) GetSMTPConfig(
	ctx context.Context,
	_ openapi.GetSMTPConfigRequestObject,
) (openapi.GetSMTPConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return smtpConfigDenial(denied), nil
	}
	smtp, err := h.Store.GetSMTP(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get smtp: %w", err)
	}
	return openapi.GetSMTPConfig200JSONResponse(smtpToAPI(smtp)), nil
}

func (h *Handler) UpdateSMTPConfig(
	ctx context.Context,
	req openapi.UpdateSMTPConfigRequestObject,
) (openapi.UpdateSMTPConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigWrite); denied != nil {
		return smtpConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateSMTPConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	smtp, err := apiToSMTP(*req.Body)
	if err != nil {
		return openapi.UpdateSMTPConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if err := h.Store.SetSMTP(ctx, smtp); err != nil {
		return openapi.UpdateSMTPConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	return openapi.UpdateSMTPConfig200JSONResponse(smtpToAPI(smtp)), nil
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func (h *Handler) GetAuthConfig(
	ctx context.Context,
	_ openapi.GetAuthConfigRequestObject,
) (openapi.GetAuthConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return authConfigDenial(denied), nil
	}
	cfg, err := h.Store.GetAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get auth: %w", err)
	}
	return openapi.GetAuthConfig200JSONResponse(authToAPI(cfg)), nil
}

func (h *Handler) UpdateAuthConfig(
	ctx context.Context,
	req openapi.UpdateAuthConfigRequestObject,
) (openapi.UpdateAuthConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapAuthWrite); denied != nil {
		return authConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateAuthConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	cfg := apiToAuth(*req.Body)
	if err := h.Store.SetAuth(ctx, cfg); err != nil {
		return openapi.UpdateAuthConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	return openapi.UpdateAuthConfig200JSONResponse(authToAPI(cfg)), nil
}

// ---------------------------------------------------------------------------
// AI
// ---------------------------------------------------------------------------

func (h *Handler) GetAIConfig(
	ctx context.Context,
	_ openapi.GetAIConfigRequestObject,
) (openapi.GetAIConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return aiConfigDenial(denied), nil
	}
	cfg, err := h.Store.GetAI(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get ai: %w", err)
	}
	return openapi.GetAIConfig200JSONResponse(aiToAPI(cfg)), nil
}

func (h *Handler) UpdateAIConfig(
	ctx context.Context,
	req openapi.UpdateAIConfigRequestObject,
) (openapi.UpdateAIConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapAIWrite); denied != nil {
		return aiConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateAIConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	cfg := apiToAI(*req.Body)
	if err := h.Store.SetAI(ctx, cfg); err != nil {
		return openapi.UpdateAIConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	return openapi.UpdateAIConfig200JSONResponse(aiToAPI(cfg)), nil
}
