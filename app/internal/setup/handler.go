// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package setup implements the first-run installer endpoints:
// /setup/status and /setup/complete. It only does anything while the
// system has zero system.admin users; once the first admin is
// created, the endpoints return 409 forever after.
//
// Day-to-day user administration (creating other users, editing
// profiles, password reset) is NOT this package's job — those land in
// Phase 1.6.B under /admin/users and friends.
package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// adminRoleName is the seeded role we assign to the first admin. The
// role + its system.admin capability come from migration 00001.
const adminRoleName = "Admin"

// minPasswordLen mirrors the OpenAPI spec.
const minPasswordLen = 8

// Handler implements the setup-related slice of openapi.StrictServerInterface.
type Handler struct {
	Pool        *pgxpool.Pool
	Logger      *slog.Logger
	ScrambleKey string
	Cfg         config.Config // for SetupDefaults + deployment-info readout
	SysConfig   *sysconfig.Store

	// StorageBackendName is "fs" | "s3" | ... — surfaced read-only on
	// the setup page so the admin can confirm what's wired up.
	StorageBackendName string

	// Recorder is the optional audit sink for setup-time events
	// (currently: federation.user.key_generated when the wizard
	// creates the first admin's federation keypair per 1.22.I-b).
	// nil-safe — when unset, the keypair still lands; only the
	// audit row is skipped.
	Recorder *audit.Recorder
}

// NewHandler constructs a setup handler. The audit recorder is
// optional; pass nil to skip the federation.user.key_generated
// audit row at first-boot. Production wiring passes one in.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, cfg config.Config, sys *sysconfig.Store, storageBackend string, recorder *audit.Recorder) *Handler {
	return &Handler{
		Pool:               pool,
		Logger:             logger,
		ScrambleKey:        cfg.ScrambleKey,
		Cfg:                cfg,
		SysConfig:          sys,
		StorageBackendName: storageBackend,
		Recorder:           recorder,
	}
}

// GetSetupStatus implements GET /setup/status. Always 200; returns
// whether setup is still needed plus the deployment readout and any
// env-var prefills.
func (h *Handler) GetSetupStatus(
	ctx context.Context,
	_ openapi.GetSetupStatusRequestObject,
) (openapi.GetSetupStatusResponseObject, error) {
	needs, err := h.needsSetup(ctx)
	if err != nil {
		return nil, err
	}
	// Public mode rides along here (#416) because the frontend's root
	// layout calls this endpoint FIRST on every navigation, before it
	// knows whether a session exists — so the flag is in hand at the
	// moment the routing decision is made, with no second request on
	// the hot path.
	//
	// Fails to false on a read error, matching the middleware's own
	// posture: a transient blip should read as "not public" and send a
	// signed-out visitor to the sign-in page, never the reverse. This
	// is a rendering hint regardless — auth/middleware.go enforces the
	// same setting independently, so a stale or wrong value here
	// cannot grant access, only misdraw a page.
	publicMode := false
	// Whether self-service signup is open rides along for the same
	// reason (#712): the root layout has to decide whether a signed-out
	// visitor may stay on /register before it knows whether a session
	// exists, and this is the one call it has already made by then.
	// Without it /register is unreachable to the only people who would
	// ever want it.
	//
	// ONLY the boolean. This endpoint is unauthenticated, so the rest
	// of SelfRegistrationConfig — the email-verification requirement,
	// the default role — stays behind the admin auth-settings surface.
	//
	// Fails to false like public mode does, and for the same reason:
	// POST /auth/register checks the setting itself and 403s when it is
	// off, so a wrong value here can only misdraw a page, never open a
	// closed install.
	selfRegistration := false
	if h.SysConfig != nil {
		if pm, pmErr := h.SysConfig.GetPublicMode(ctx); pmErr != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "setup.status.public_mode_read_failed",
				slog.String("err", pmErr.Error()))
		} else {
			publicMode = pm.Enabled
		}
		if a, aErr := h.SysConfig.GetAuth(ctx); aErr != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "setup.status.self_registration_read_failed",
				slog.String("err", aErr.Error()))
		} else {
			selfRegistration = a.SelfRegistration.Enabled
		}
	}

	d := h.Cfg.SetupDefaults
	return openapi.GetSetupStatus200JSONResponse{
		NeedsSetup:              needs,
		PublicMode:              publicMode,
		SelfRegistrationEnabled: selfRegistration,
		Deployment: openapi.SetupDeploymentInfo{
			DbHost:         h.Cfg.DBHost,
			DbPort:         h.Cfg.DBPort,
			DbName:         h.Cfg.DBName,
			StorageBackend: h.StorageBackendName,
		},
		Defaults: openapi.SetupDefaults{
			AdminUsername:   d.AdminUsername,
			AdminEmail:      d.AdminEmail,
			AdminFullname:   d.AdminFullname,
			SiteName:        d.SiteName,
			SiteBaseUrl:     d.SiteBaseURL,
			SmtpHost:        d.SMTPHost,
			SmtpPort:        d.SMTPPort,
			SmtpEncryption:  openapi.SetupDefaultsSmtpEncryption(normaliseEncryption(d.SMTPEncryption)),
			SmtpUsername:    d.SMTPUsername,
			SmtpFromAddress: d.SMTPFromAddr,
		},
	}, nil
}

// CompleteSetup implements POST /setup/complete.
//
// All writes happen inside one transaction: admin user, role
// assignment, site config, optional SMTP config. Either all four
// land or none do.
func (h *Handler) CompleteSetup(
	ctx context.Context,
	req openapi.CompleteSetupRequestObject,
) (openapi.CompleteSetupResponseObject, error) {
	needs, err := h.needsSetup(ctx)
	if err != nil {
		return nil, err
	}
	if !needs {
		return openapi.CompleteSetup409JSONResponse{
			Error: "setup already complete; an administrator exists",
		}, nil
	}
	if req.Body == nil {
		return openapi.CompleteSetup400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}

	// --- input validation ---
	adminIn := req.Body.Admin
	username := strings.TrimSpace(adminIn.Username)
	if username == "" {
		return badInput("username is required"), nil
	}
	if len(username) > 50 {
		return badInput("username must be 50 characters or fewer"), nil
	}
	if len(adminIn.Password) < minPasswordLen {
		return badInput(fmt.Sprintf("password must be at least %d characters", minPasswordLen)), nil
	}
	emailStr := strings.TrimSpace(string(adminIn.Email))
	if _, mailErr := mail.ParseAddress(emailStr); mailErr != nil {
		return badInput("admin email is not a valid address"), nil
	}

	siteIn := req.Body.Site
	siteName := strings.TrimSpace(siteIn.Name)
	if siteName == "" {
		return badInput("site name is required"), nil
	}
	if len(siteName) > 100 {
		return badInput("site name must be 100 characters or fewer"), nil
	}
	siteBaseURL := ""
	if siteIn.BaseUrl != nil && *siteIn.BaseUrl != "" {
		raw := strings.TrimSpace(*siteIn.BaseUrl)
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return badInput("site base_url must be an absolute http(s) URL"), nil
		}
		// Normalise: strip trailing slash so we can concatenate paths
		// without doubling.
		siteBaseURL = strings.TrimRight(raw, "/")
	}

	var smtp sysconfig.SMTP
	if req.Body.Smtp != nil {
		sm := req.Body.Smtp
		smtp.Host = strings.TrimSpace(sm.Host)
		smtp.Port = sm.Port
		smtp.Encryption = sysconfig.SMTPEncryption(string(sm.Encryption))
		smtp.FromAddr = strings.TrimSpace(sm.FromAddress)
		if sm.Username != nil {
			smtp.Username = *sm.Username
		}
		if sm.Password != nil {
			smtp.Password = *sm.Password
		}
		// Only validate the SMTP block when host is provided; an empty
		// host means "no SMTP yet, leave this blank".
		if smtp.Host != "" {
			switch smtp.Encryption {
			case sysconfig.SMTPEncryptionNone, sysconfig.SMTPEncryptionStartTLS, sysconfig.SMTPEncryptionTLS:
			default:
				return badInput("smtp encryption must be none|starttls|tls"), nil
			}
			if smtp.Port <= 0 || smtp.Port > 65535 {
				return badInput("smtp port must be 1..65535"), nil
			}
			if smtp.FromAddr == "" {
				return badInput("smtp from_address is required when smtp.host is set"), nil
			}
			if _, mailErr := mail.ParseAddress(smtp.FromAddr); mailErr != nil {
				return badInput("smtp from_address is not a valid email"), nil
			}
		}
	}

	// --- transactional writes ---
	hash, err := auth.HashPassword(adminIn.Password, h.ScrambleKey)
	if err != nil {
		return nil, fmt.Errorf("setup: hash password: %w", err)
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("setup: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := auth.New(tx)

	// Re-check inside the tx so two parallel setup calls can't both
	// commit an admin.
	count, err := q.CountSystemAdmins(ctx)
	if err != nil {
		return nil, fmt.Errorf("setup: recheck admin count: %w", err)
	}
	if count > 0 {
		return openapi.CompleteSetup409JSONResponse{
			Error: "setup already complete; an administrator exists",
		}, nil
	}

	var fullnamePtr *string
	if adminIn.Fullname != nil {
		s := strings.TrimSpace(*adminIn.Fullname)
		if s != "" {
			fullnamePtr = &s
		}
	}
	// usergroup 3 is the seeded "Super Admin" group from the legacy
	// dbstruct/data_usergroup.txt — it carries the `a` permission
	// (and the full set of capability codes) legacy-rendered pages check
	// to expose the admin panel, browse bar, and per-asset-type
	// access. The Go side still uses roles+capabilities; this is the
	// legacy-coexistence twin.
	superAdminGroup := int64(3)
	userRow, err := q.CreateUser(ctx, auth.CreateUserParams{
		Username:  &username,
		Password:  &hash,
		Fullname:  fullnamePtr,
		Email:     &emailStr,
		Usergroup: &superAdminGroup,
		Lang:      nil,
	})
	if err != nil {
		return nil, fmt.Errorf("setup: insert user: %w", err)
	}

	adminRole, err := q.FindRoleByName(ctx, adminRoleName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("setup: %q role not found — was migration 00002 applied?", adminRoleName)
		}
		return nil, fmt.Errorf("setup: lookup admin role: %w", err)
	}
	if err := q.SetUserGlobalRole(ctx, auth.SetUserGlobalRoleParams{
		UserRef:           userRow.Ref,
		RoleID:            adminRole.ID,
		AssignedByUserRef: nil, // bootstrap; no actor
	}); err != nil {
		return nil, fmt.Errorf("setup: assign admin role: %w", err)
	}

	// Federation keypair (Phase 1.22.I-b). Lives in the same tx
	// as the user create + role assignment so the wizard never
	// commits a user without a current key. Idempotent — re-run
	// only happens on a setup-flow retry against a re-created
	// user (rare), and EnsureCurrentForUser is a no-op when a
	// current key is already present.
	ukq := userkeys.New(tx)
	alreadyHadKey, err := userkeys.EnsureCurrentForUser(ctx, ukq, userRow.Ref)
	if err != nil {
		return nil, fmt.Errorf("setup: ensure federation user key: %w", err)
	}
	if !alreadyHadKey && h.Recorder != nil {
		// First-boot setup has no prior principal — same convention
		// the role assignment uses (AssignedByUserRef: nil above).
		h.Recorder.FederationUserKeyGenerated(ctx, audit.New(tx), userRow.Ref, nil, 1, userkeys.Algorithm)
	}

	if err := h.SysConfig.SetSiteAndSMTPTx(ctx, tx, sysconfig.Site{
		Name:    siteName,
		BaseURL: siteBaseURL,
	}, smtp); err != nil {
		return nil, fmt.Errorf("setup: write system config: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("setup: commit: %w", err)
	}

	h.Logger.LogAttrs(ctx, slog.LevelInfo, "setup.complete",
		slog.Int64("user_ref", userRow.Ref),
		slog.String("username", username),
		slog.String("site_name", siteName),
		slog.Bool("smtp_configured", smtp.Host != ""),
	)

	// `unavailable`, and it is the honest answer rather than a
	// placeholder (#956). This handler lives outside package auth and
	// deliberately does not resolve capabilities (see
	// auth.Handler.hydrateCapabilities' doc for why), so this response
	// cannot say what the new admin holds — even though the transaction
	// above has already committed the role that grants it.
	//
	// Saying so out loud is what makes the documented hazard safe. The
	// setup page discards this body and calls auth.refresh() on the next
	// line, so nothing reads it today; if anything ever does, it now
	// gets "I cannot tell you" — a retry — instead of the empty
	// capability set that reads as "you have no permission" at the
	// /admin gate.
	resp := openapi.CurrentUser{
		Ref:        userRow.Ref,
		AuthMethod: "session",
		// #1116 — the install was created by the transaction above and
		// has never configured the mature switch, so the answer is
		// sysconfig.KeyMatureContent's absent-means-allowed default. It
		// is stated rather than left to Go's zero value because the zero
		// value would say the OPPOSITE ("this install forbids mature
		// content"), and this producer is the one that does not go
		// through auth.hydrateSessionUser.
		MatureContentAllowed: true,
		CapabilitiesStatus:   openapi.Unavailable,
	}
	if userRow.Username != nil {
		resp.Username = *userRow.Username
	}
	resp.Fullname = userRow.Fullname
	resp.Email = userRow.Email
	resp.Usergroup = userRow.Usergroup
	return openapi.CompleteSetup201JSONResponse(resp), nil
}

// needsSetup returns true when zero users have a role granting
// system.admin. The cheap COUNT lives in auth.Queries.
func (h *Handler) needsSetup(ctx context.Context) (bool, error) {
	q := auth.New(h.Pool)
	n, err := q.CountSystemAdmins(ctx)
	if err != nil {
		return false, fmt.Errorf("setup: count admins: %w", err)
	}
	return n == 0, nil
}

func badInput(msg string) openapi.CompleteSetup400JSONResponse {
	return openapi.CompleteSetup400JSONResponse{
		BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
	}
}

// normaliseEncryption lowercases the env-provided value and falls
// back to "starttls" for anything we don't recognise. Defensive — the
// admin form will overwrite this anyway.
func normaliseEncryption(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return "none"
	case "tls", "ssl", "smtps":
		return "tls"
	case "starttls", "", "auto":
		return "starttls"
	default:
		return "starttls"
	}
}

// Compile-time assertion that we implement the relevant slice of the
// StrictServerInterface — catches drift when codegen signatures change.
var _ interface {
	GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error)
	CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error)
} = (*Handler)(nil)
