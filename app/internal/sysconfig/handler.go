// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Capability codes. Stable strings; seeded by migration 00001.
const (
	CapConfigRead      = "system.config.read"
	CapConfigWrite     = "system.config.write"
	CapAuthWrite       = "system.auth.write"
	CapAIWrite         = "system.ai.write"
	CapAppearanceWrite = "system.appearance.write"
	CapSystemAdmin     = "system.admin"
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

	// Email holds the boot-time email seam used by the
	// /admin/system/smtp/test endpoint. nil-safe — when unwired
	// (test fixtures that don't exercise email), SendSMTPTestEmail
	// returns a 500 explaining the boot wire is missing.
	Email *EmailDeps

	// CacheReg is the process cache registry, used to broadcast the
	// public-mode invalidation after a toggle write (#445). nil-safe —
	// InvalidatePublicMode no-ops, which in an uncached fixture is
	// correct because the reader has no cache to stale out.
	CacheReg *cache.Registry

	// Storage is the byte plane for the instance logo (#517) — the one
	// setting in this package whose value is a blob rather than a
	// short string. nil-safe: unwired, the logo write endpoints refuse
	// with a 400 and the read endpoint reports "no logo", so a fixture
	// that doesn't exercise logos needs no storage backend.
	Storage *storage.Service

	// DemoMode mirrors config.Config.DemoMode (env AA_DEMO_MODE). When
	// true it's surfaced on the public /appearance boot payload so the
	// login card and read-only banner can render. Defaults to the zero
	// value (false) — a normal install never advertises demo mode.
	DemoMode bool

	// BrowseViews is the cached read of the operator's enabled browse
	// layouts (#709), used by the public boot-path endpoint. nil-safe:
	// unwired, GetPublicBrowseViews reads the store directly, which is
	// correct-but-uncached — the cache is a performance property here,
	// not a correctness one, so a fixture needs no registry.
	BrowseViews BrowseViewsReader
}

// SetBrowseViewsReader wires the cached browse-view read
// post-construction, the same way SetAuditRecorder wires audit. Boot
// calls this after the cache registry exists.
func (h *Handler) SetBrowseViewsReader(r BrowseViewsReader) { h.BrowseViews = r }

// EmailDeps bundles the email-related dependencies the handler
// needs for the test-send surface. Held behind a single struct so
// the boot wire passes one [Handler.SetEmail] call.
type EmailDeps struct {
	Sender email.Sender
	Mode   email.Mode
	Site   email.SiteContextProvider
}

// SetEmail wires the email seam post-construction. Boot calls this
// after building the Sender so the sysconfig handler can render
// the admin_test template + drive the configured Sender.
func (h *Handler) SetEmail(d *EmailDeps) { h.Email = d }

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
	site := apiToSite(*req.Body, before)
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
	// Load-bearing, not just audit input: apiToSMTP merges the stored
	// password in from `before`. On a read failure, merging against a
	// zero SMTP would blank the password on an unrelated-field save —
	// the same class of bug as #708's font PATCH clearing the logo.
	before, err := h.Store.GetSMTP(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get smtp for merge: %w", err)
	}
	smtp, err := apiToSMTP(*req.Body, before)
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
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminSMTPConfigUpdated,
			nil, actor,
			&before, &smtp, nil)
	}
	return openapi.UpdateSMTPConfig200JSONResponse(smtpToAPI(smtp)), nil
}

// SendSMTPTestEmail renders the admin_test template + sends it
// through the boot-configured email.Sender. Synchronous so the
// operator sees the outcome (sent / not-configured / SMTP error)
// in one round-trip instead of digging through the job queue.
func (h *Handler) SendSMTPTestEmail(
	ctx context.Context,
	req openapi.SendSMTPTestEmailRequestObject,
) (openapi.SendSMTPTestEmailResponseObject, error) {
	id, denied := h.requireCap(ctx, CapConfigWrite)
	if denied != nil {
		switch e := denied.(type) {
		case errUnauthenticated:
			_ = e
			return openapi.SendSMTPTestEmail401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
			}, nil
		case errForbidden:
			return openapi.SendSMTPTestEmail403JSONResponse{
				ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
			}, nil
		}
		return nil, fmt.Errorf("sysconfig: unknown denial type")
	}
	if h.Email == nil || h.Email.Sender == nil {
		return openapi.SendSMTPTestEmail400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "email subsystem not wired"},
		}, nil
	}

	// Recipient — explicit override or the caller's own email.
	to := ""
	if req.Body != nil && req.Body.To != nil {
		to = strings.TrimSpace(string(*req.Body.To))
	}
	if to == "" {
		caller, err := h.lookupCallerEmail(ctx, id.UserRef)
		if err != nil {
			return nil, fmt.Errorf("sysconfig: lookup caller email: %w", err)
		}
		if caller == "" {
			return openapi.SendSMTPTestEmail400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{
					Error: "no recipient: caller has no email on file; pass `to` to override",
				},
			}, nil
		}
		to = caller
	}

	// Build the data map. Site context is best-effort — if the
	// Store hasn't been written yet the template renders with
	// empty placeholders and the operator sees the misconfiguration
	// in the captured/delivered body.
	site := email.SiteContext{}
	if h.Email.Site != nil {
		if sc, err := h.Email.Site(ctx); err == nil {
			site = sc
		}
	}
	data := map[string]any{
		"site_name":      site.Name,
		"site_url":       site.URL,
		"recipient_name": to,
		"triggered_by":   triggeredByLabel(id),
		"triggered_at":   time.Now().UTC().Format(time.RFC3339),
	}
	msg, err := email.Render(ctx, email.TemplateAdminTest, []string{to}, data)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: render admin_test: %w", err)
	}

	if err := h.Email.Sender.Send(ctx, msg); err != nil {
		if errors.Is(err, email.ErrNotConfigured) {
			return openapi.SendSMTPTestEmail503JSONResponse{Error: "SMTP host is not configured"}, nil
		}
		// Surface the underlying error to the operator — this is an
		// admin-only diagnostic surface, exposing the relay error
		// message is the whole point.
		return openapi.SendSMTPTestEmail400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}

	mode := openapi.SMTPTestResultMode(string(h.Email.Mode))
	human := senderModeSummary(h.Email.Mode)
	return openapi.SendSMTPTestEmail200JSONResponse{
		Sent:      true,
		Mode:      mode,
		Recipient: to,
		Message:   &human,
	}, nil
}

func (h *Handler) lookupCallerEmail(ctx context.Context, ref int64) (string, error) {
	var email *string
	err := h.Pool.QueryRow(ctx, `SELECT email FROM "user" WHERE ref = $1`, ref).Scan(&email)
	if err != nil {
		return "", err
	}
	if email == nil {
		return "", nil
	}
	return strings.TrimSpace(*email), nil
}

func triggeredByLabel(id *auth.Identity) string {
	if id == nil {
		return "anonymous"
	}
	if id.Email != nil && *id.Email != "" {
		return *id.Email
	}
	if id.Username != "" {
		return "@" + id.Username
	}
	return fmt.Sprintf("user_ref=%d", id.UserRef)
}

func senderModeSummary(m email.Mode) string {
	switch m {
	case email.ModeCapture:
		return "captured locally; not delivered (AA_EMAIL_MODE=capture)"
	case email.ModeDisabled:
		return "logged + dropped (AA_EMAIL_MODE=disabled)"
	default:
		return "handed to SMTP relay"
	}
}

// Force-use of openapi_types alias to satisfy the linter when the
// generated types reuse it. Defensive — no actual call needed.
var _ openapi_types.Email

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
	// The stored config is now load-bearing, not just audit input:
	// apiToAuth merges each provider's on-file secrets in from it. A
	// read failure must abort rather than write a config with every
	// OAuth client secret, LDAP bind password and SAML private key
	// blanked (#718). Same reasoning as UpdateAIConfig below, and the
	// reason the previously-tolerated beforeErr is now fatal.
	before, err := h.Store.GetAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get auth for merge: %w", err)
	}
	cfg := apiToAuth(*req.Body, before)
	if err := h.Store.SetAuth(ctx, cfg); err != nil {
		return openapi.UpdateAuthConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if h.Audit != nil {
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminAuthConfigUpdated,
			nil, actor,
			&before, &cfg, nil)
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
	// The stored config is now load-bearing, not just audit input:
	// apiToAI merges the on-file API keys in from it. A read failure
	// must abort rather than write a config with every key blanked.
	before, err := h.Store.GetAI(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get ai for merge: %w", err)
	}
	cfg := apiToAI(*req.Body, before)
	if err := h.Store.SetAI(ctx, cfg); err != nil {
		return openapi.UpdateAIConfig400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	if h.Audit != nil {
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminAIConfigUpdated,
			nil, actor,
			&before, &cfg, nil)
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
	return openapi.GetAppearanceConfig200JSONResponse(h.appearanceAdminAPI(ctx, cfg)), nil
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
	// Carry the logo forward. This endpoint is a whole-object replace
	// and the logo fields are read-only on it, so without this an
	// admin saving a font would silently reset the install's brand
	// mark and orphan every pinned entry in the recent list.
	if beforeErr == nil {
		cfg.ActiveLogo = before.ActiveLogo
		cfg.LogoHistory = before.LogoHistory
	}
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
	return openapi.UpdateAppearanceConfig200JSONResponse(h.appearanceAdminAPI(ctx, cfg)), nil
}

// GetPublicPreviewLadder reads the configured preview rungs (#591).
//
// It is the companion to the per-asset `ladder_available` flag: that
// says an asset HAS the whole ladder, this says what the ladder IS. A
// client needs both to build a responsive srcset, and without this it
// would have to hardcode the four default keys — the exact assumption
// the flag exists to remove.
//
// "Public" here means PUBLIC-MODE GOVERNED, not unauthenticated: the
// route is registered in auth.PublicSurfaceRoutes, so it serves
// anonymous callers on a public install and 401s on a private one. The
// handler itself performs no auth check because the middleware has
// already decided — do not read the absence of one here as the endpoint
// being open.
func (h *Handler) GetPublicPreviewLadder(
	ctx context.Context,
	_ openapi.GetPublicPreviewLadderRequestObject,
) (openapi.GetPublicPreviewLadderResponseObject, error) {
	cfg, err := h.Store.GetPreviews(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get preview ladder: %w", err)
	}
	rungs := make([]openapi.PreviewLadderRung, 0, len(cfg.Variants))
	for _, v := range cfg.Variants {
		r := openapi.PreviewLadderRung{
			Key:    v.Key,
			Fit:    openapi.PreviewLadderRungFit(v.Fit),
			MaxDim: int(v.MaxDim),
		}
		// Quality is deliberately NOT exposed: it is an encoder knob with
		// no client-side use, and every field published here becomes a
		// contract to keep.
		f := openapi.PreviewLadderRungFormat(v.Format)
		r.Format = &f
		rungs = append(rungs, r)
	}
	return openapi.GetPublicPreviewLadder200JSONResponse{Variants: rungs}, nil
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
	out := appearanceToAPI(cfg)
	// Ride the site display name along the public boot path so the
	// frontend can render the wordmark / titles before sign-in. Read
	// from the Site config (edited via /admin/system/site); falls back
	// to the default when unset. Read-only here — the appearance write
	// path never persists it.
	name := DefaultSiteName
	if site, siteErr := h.Store.GetSite(ctx); siteErr == nil {
		name = SiteNameOrDefault(site.Name)
	}
	out.SiteName = &name
	// Advertise demo mode along the same boot path so the login card
	// and read-only banner can react without a second fetch. Only ever
	// true when AA_DEMO_MODE=1 was set at boot.
	demo := h.DemoMode
	out.DemoMode = &demo
	return openapi.GetPublicAppearance200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// Public mode (#445)
// ---------------------------------------------------------------------------
//
// Its own endpoint rather than a field on SiteConfig, because
// UpdateSiteConfig is a whole-object replace: folding the flag in there
// would mean an admin renaming the site from a stale form silently
// turns public access off (or on). A setting that changes who can read
// the install must not be collateral damage of an unrelated save.
//
// Read is gated on system.config.read and write on system.config.write,
// matching the other system settings. There is no separate
// `system.public_mode.write` cap — inventing one would imply a role
// that can publish the install without being able to change the rest of
// its configuration, and no such role exists.

func (h *Handler) GetPublicMode(
	ctx context.Context,
	_ openapi.GetPublicModeRequestObject,
) (openapi.GetPublicModeResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return publicModeDenial(denied), nil
	}
	cfg, err := h.Store.GetPublicMode(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get public mode: %w", err)
	}
	return openapi.GetPublicMode200JSONResponse{Enabled: cfg.Enabled}, nil
}

func (h *Handler) UpdatePublicMode(
	ctx context.Context,
	req openapi.UpdatePublicModeRequestObject,
) (openapi.UpdatePublicModeResponseObject, error) {
	id, denied := h.requireCap(ctx, CapConfigWrite)
	if denied != nil {
		return publicModeUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdatePublicMode400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	before, beforeErr := h.Store.GetPublicMode(ctx)
	cfg := PublicModeConfig{Enabled: req.Body.Enabled}
	if err := h.Store.SetPublicMode(ctx, cfg); err != nil {
		return nil, fmt.Errorf("sysconfig: set public mode: %w", err)
	}
	// Invalidate BEFORE returning, so the admin's next request already
	// sees the new state. The auth middleware reads this flag through a
	// process-local cache fed by NOTIFY; without this the toggle would
	// appear to do nothing until the entry aged out.
	InvalidatePublicMode(ctx, h.CacheReg)
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*PublicModeConfig)(nil)
		}
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminPublicModeUpdated,
			nil, actor,
			beforeArg, &cfg, nil)
	}
	return openapi.UpdatePublicMode200JSONResponse{Enabled: cfg.Enabled}, nil
}

// ---------------------------------------------------------------------------
// Browse views (#709) — which layouts the install offers
// ---------------------------------------------------------------------------
//
// Three endpoints for one setting, because they have two different
// audiences. The admin pair is capability-gated and is what the
// operator's settings panel talks to. GetPublicBrowseViews is what
// every client renders from: it is on the frontend's boot path, so it
// is cache-fronted and carries no capability check.
//
// Read is gated on system.config.read and write on system.config.write,
// matching the other system settings. There is no separate
// `system.browse_views.write` cap — a role that can curate the layout
// switcher but not touch the rest of the install's configuration does
// not exist.

func (h *Handler) GetBrowseViews(
	ctx context.Context,
	_ openapi.GetBrowseViewsRequestObject,
) (openapi.GetBrowseViewsResponseObject, error) {
	if _, denied := h.requireCap(ctx, CapConfigRead); denied != nil {
		return browseViewsDenial(denied), nil
	}
	cfg, err := h.Store.GetBrowseViews(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get browse views: %w", err)
	}
	return openapi.GetBrowseViews200JSONResponse(browseViewsToAPI(cfg)), nil
}

func (h *Handler) UpdateBrowseViews(
	ctx context.Context,
	req openapi.UpdateBrowseViewsRequestObject,
) (openapi.UpdateBrowseViewsResponseObject, error) {
	id, denied := h.requireCap(ctx, CapConfigWrite)
	if denied != nil {
		return browseViewsUpdateDenial(denied), nil
	}
	if req.Body == nil {
		return openapi.UpdateBrowseViews400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	before, beforeErr := h.Store.GetBrowseViews(ctx)
	cfg := browseViewsFromAPI(*req.Body)
	// SetBrowseViews owns the empty-set and unknown-mode refusals, and
	// it returns them as errors rather than persisting a repaired
	// value. Surfacing them as a 400 keeps the invariant in one place:
	// every caller that can reach the store gets the same refusal,
	// whether or not it came through this handler.
	if err := h.Store.SetBrowseViews(ctx, cfg); err != nil {
		return openapi.UpdateBrowseViews400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	// Invalidate BEFORE returning, so the browser's immediate re-fetch
	// of /browse-views already sees the new set. Without this the
	// switcher would keep drawing the disabled button until the entry
	// aged out, which reads as the save having done nothing.
	InvalidateBrowseViews(ctx, h.CacheReg)
	if h.Audit != nil {
		var beforeArg any = &before
		if beforeErr != nil {
			beforeArg = (*BrowseViewsConfig)(nil)
		}
		actor := &id.UserRef
		h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
			audit.EventAdminBrowseViewsUpdated,
			nil, actor,
			beforeArg, &cfg, nil)
	}
	// Echo the RESOLVED set rather than the request body, so the
	// operator's UI redraws from what was actually stored — canonical
	// order, duplicates dropped — instead of from what it happened to
	// send.
	return openapi.UpdateBrowseViews200JSONResponse(browseViewsToAPI(cfg)), nil
}

// GetPublicBrowseViews is the boot-path read every client renders from.
//
// "Public" here means PUBLIC-MODE GOVERNED, not unauthenticated: the
// route is registered in auth.PublicSurfaceRoutes, so it serves
// anonymous callers on a public install and 401s on a private one. The
// handler itself performs no auth check because the middleware has
// already decided — do not read the absence of one here as the endpoint
// being open.
//
// Reads through the cached reader rather than the store: this is on the
// cold-load path of every page on the install, and the value changes
// only when an operator changes their mind.
func (h *Handler) GetPublicBrowseViews(
	ctx context.Context,
	_ openapi.GetPublicBrowseViewsRequestObject,
) (openapi.GetPublicBrowseViewsResponseObject, error) {
	if h.BrowseViews != nil {
		return openapi.GetPublicBrowseViews200JSONResponse(
			browseViewsToAPI(h.BrowseViews(ctx))), nil
	}
	// Unwired (test fixtures) — fall back to the uncached read. The
	// cache is a performance property, not a correctness one.
	cfg, err := h.Store.GetBrowseViews(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get browse views: %w", err)
	}
	return openapi.GetPublicBrowseViews200JSONResponse(browseViewsToAPI(cfg)), nil
}
