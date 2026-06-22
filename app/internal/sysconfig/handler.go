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

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Capability codes. Stable strings; seeded by migration 00023.
const (
	CapConfigRead       = "system.config.read"
	CapConfigWrite      = "system.config.write"
	CapAuthWrite        = "system.auth.write"
	CapAIWrite          = "system.ai.write"
	CapAppearanceWrite  = "system.appearance.write"
	CapSystemAdmin      = "system.admin"
)

// Handler implements the system-config slice of the API.
type Handler struct {
	Pool   *pgxpool.Pool
	Store  *Store
	Logger *slog.Logger
	// Audit is the typed recorder for Phase 1.17.D field-level
	// changesets. nil-safe — when unwired (test fixtures), audit
	// emit is silently skipped. Production attaches the pool-bound
	// *audit.Recorder via SetAuditRecorder at boot.
	Audit *audit.Recorder
}

// NewHTTPHandler returns a Handler wired against an existing Store —
// `NewHandler` is reserved by some packages for the Store factory, so
// the HTTP surface uses the longer name.
func NewHTTPHandler(pool *pgxpool.Pool, store *Store, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Store: store, Logger: logger}
}

// SetAuditRecorder wires the audit pipeline post-construction so
// the api.go composition can keep its existing NewHTTPHandler call
// without growing a positional argument. Mirrors users.SetAuditRecorder.
// Safe to call once at startup.
func (h *Handler) SetAuditRecorder(rec *audit.Recorder) {
	h.Audit = rec
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
	id, denied := h.requireCap(ctx, CapConfigWrite)
	if denied != nil {
		return siteConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateSiteConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	// Phase 1.17.D — snapshot the current value so the changeset
	// diff has a `before`. If the read fails, log + proceed
	// without a before (RecordChange will diff against the zero
	// value). The operator-action signal is preserved either way.
	before, beforeErr := h.Store.GetSite(ctx)
	site := apiToSite(*req.Body)
	if err := h.Store.SetSite(ctx, site); err != nil {
		return nil, fmt.Errorf("sysconfig: set site: %w", err)
	}
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*Site)(nil)
		}
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminSiteConfigUpdated,
			nil, actor,
			beforeArg, &site, nil)
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
	id, denied := h.requireCap(ctx, CapConfigWrite)
	if denied != nil {
		return smtpConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateSMTPConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	before, beforeErr := h.Store.GetSMTP(ctx)
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
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*SMTP)(nil)
		}
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminSMTPConfigUpdated,
			nil, actor,
			beforeArg, &smtp, nil)
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
	id, denied := h.requireCap(ctx, CapAuthWrite)
	if denied != nil {
		return authConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateAuthConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	before, beforeErr := h.Store.GetAuth(ctx)
	cfg := apiToAuth(*req.Body)
	if err := h.Store.SetAuth(ctx, cfg); err != nil {
		return openapi.UpdateAuthConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*AuthConfig)(nil)
		}
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminAuthConfigUpdated,
			nil, actor,
			beforeArg, &cfg, nil)
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
	id, denied := h.requireCap(ctx, CapAIWrite)
	if denied != nil {
		return aiConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateAIConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	before, beforeErr := h.Store.GetAI(ctx)
	cfg := apiToAI(*req.Body)
	if err := h.Store.SetAI(ctx, cfg); err != nil {
		return openapi.UpdateAIConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*AIConfig)(nil)
		}
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminAIConfigUpdated,
			nil, actor,
			beforeArg, &cfg, nil)
	}
	return openapi.UpdateAIConfig200JSONResponse(aiToAPI(cfg)), nil
}

// ---------------------------------------------------------------------------
// AI image-edit (Phase 1.14.E-1)
// ---------------------------------------------------------------------------
//
// Lives in this handler (not aiedit's own) so the
// /admin/system/aiedit endpoint composes with the same auth gate +
// audit + denial machinery as the other system config surfaces. The
// aiedit package owns the runtime endpoint (POST
// /assets/{id}/edit/img2img); sysconfig owns the operator settings.

func (h *Handler) GetAIEditConfig(
	ctx context.Context,
	_ openapi.GetAIEditConfigRequestObject,
) (openapi.GetAIEditConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return aiEditConfigDenial(denied), nil
	}
	cfg, err := h.Store.GetAIEdit(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get aiedit: %w", err)
	}
	return openapi.GetAIEditConfig200JSONResponse(aiEditToAPI(cfg)), nil
}

func (h *Handler) UpdateAIEditConfig(
	ctx context.Context,
	req openapi.UpdateAIEditConfigRequestObject,
) (openapi.UpdateAIEditConfigResponseObject, error) {
	id, denied := h.requireCap(ctx, CapAIWrite)
	if denied != nil {
		return aiEditConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateAIEditConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	before, beforeErr := h.Store.GetAIEdit(ctx)
	cfg := apiToAIEdit(*req.Body)
	if err := h.Store.SetAIEdit(ctx, cfg); err != nil {
		return openapi.UpdateAIEditConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*AIEditConfig)(nil)
		}
		actor := &id.UserRef
		// Re-uses EventAdminAIConfigUpdated event kind — aiedit
		// settings are conceptually part of the AI surface for the
		// audit feed even though they live under a separate key.
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminAIConfigUpdated,
			nil, actor,
			beforeArg, &cfg, nil)
	}
	return openapi.UpdateAIEditConfig200JSONResponse(aiEditToAPI(cfg)), nil
}

// ---------------------------------------------------------------------------
// Appearance
// ---------------------------------------------------------------------------
//
// Appearance has two read endpoints:
//   * GET /admin/system/appearance     — admin-only, full settings shape
//   * GET /appearance                  — public, used by the frontend at
//                                        boot to pick fonts before login.
// Only the admin write endpoint exists for mutation.

func (h *Handler) GetAppearanceConfig(
	ctx context.Context,
	_ openapi.GetAppearanceConfigRequestObject,
) (openapi.GetAppearanceConfigResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return appearanceConfigDenial(denied), nil
	}
	cfg, err := h.Store.GetAppearance(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get appearance: %w", err)
	}
	return openapi.GetAppearanceConfig200JSONResponse(appearanceToAPI(cfg)), nil
}

func (h *Handler) UpdateAppearanceConfig(
	ctx context.Context,
	req openapi.UpdateAppearanceConfigRequestObject,
) (openapi.UpdateAppearanceConfigResponseObject, error) {
	id, denied := h.requireCap(ctx, CapAppearanceWrite)
	if denied != nil {
		return appearanceConfigUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateAppearanceConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	before, beforeErr := h.Store.GetAppearance(ctx)
	cfg := apiToAppearance(*req.Body)
	if err := h.Store.SetAppearance(ctx, cfg); err != nil {
		return openapi.UpdateAppearanceConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*AppearanceConfig)(nil)
		}
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminAppearanceConfigUpdated,
			nil, actor,
			beforeArg, &cfg, nil)
	}
	return openapi.UpdateAppearanceConfig200JSONResponse(appearanceToAPI(cfg)), nil
}

// GetPublicAppearance is the unauthenticated read used by the frontend
// to pick which fonts to load before the user has signed in.
func (h *Handler) GetPublicAppearance(
	ctx context.Context,
	_ openapi.GetPublicAppearanceRequestObject,
) (openapi.GetPublicAppearanceResponseObject, error) {
	cfg, err := h.Store.GetAppearance(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get public appearance: %w", err)
	}
	return openapi.GetPublicAppearance200JSONResponse(appearanceToAPI(cfg)), nil
}
