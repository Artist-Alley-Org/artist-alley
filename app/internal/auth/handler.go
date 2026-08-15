// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package auth implements the artist-alley authentication layer:
// login, logout, /me, and personal access tokens.
//
// The HTTP contract is the relevant slice of `app/api/openapi.yaml`;
// generated types and the StrictServerInterface live in
// `app/internal/openapi`.
//
// Layout:
//
//	queries.sql            -- sqlc input
//	queries.sql.go,        -- sqlc generated; regenerate via
//	  db.go, models.go         scripts/generate.sh
//	password.go            -- legacy-compatible HMAC-then-bcrypt hashing
//	session.go             -- session-token + API-token generation
//	                          and the rs_session cookie helpers
//	middleware.go          -- ResolveIdentity + RequireAuth middlewares,
//	                          Identity stored in request ctx
//	handler.go             -- strict-server method implementations
//	*_test.go              -- integration tests against live Postgres
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"golang.org/x/crypto/bcrypt"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Handler implements the auth-related slice of openapi.StrictServerInterface.
type Handler struct {
	Pool        *pgxpool.Pool
	Logger      *slog.Logger
	ScrambleKey string
	SessionDays int // how long the session cookie lives; matches legacy default

	Sessions *SessionManager
	Limiter  *LoginLimiter
	Audit    auditRecorder

	// CacheReg is consulted by mutation paths (e.g. SetUserGlobalRole)
	// to broadcast caps invalidations to the Resolver's cache. nil is
	// safe — invalidation becomes a no-op, the cache eventually evicts
	// the stale entry via LRU pressure.
	CacheReg *cache.Registry

	// Policy is the password complexity source (Phase 1.17.D). Nil-
	// safe — handler falls back to the zero policy (no min length,
	// no complexity), which means "anything goes" for installs that
	// haven't configured one.
	Policy passwordPolicySource

	// Providers is the identity-provider registry consulted by Login()
	// to dispatch credentials to the right backend (password vs LDAP
	// vs ...). Nil-safe — when nil, Login falls back to the legacy
	// in-handler password flow so tests that pre-date the registry
	// keep working without explicit wiring. Production boot always
	// attaches a non-nil registry.
	Providers *Registry

	// RegisterDeps holds the cross-package seams the self-
	// registration surface (Phase 1.19.C) needs — email sender,
	// site context, the sysconfig knobs. Zero-value disables the
	// path (the handler returns 403 "self-registration is
	// disabled"). Boot wires via SetRegistrationSurface. Named
	// with the -Deps suffix to disambiguate from the
	// strict-server Register METHOD that handles POST /register.
	RegisterDeps RegisterSurface

	// LockoutMgr is the Phase 1.19.D persistent per-username lockout
	// layer. Composes with (does not replace) the in-process
	// LoginLimiter. Nil disables the layer entirely — the
	// LoginLimiter alone survives.
	LockoutMgr LockoutManager

	// LockoutIPSalt salts the IP-subnet hash carried in
	// auth.lockout.triggered audit rows. Read from sysconfig at boot;
	// rotating breaks historical audit correlation but the trade
	// is deliberate (a leaked salt shouldn't linger). Zero string
	// disables the hash (audit still fires with empty ip_subnet_hash).
	LockoutIPSalt string

	// DemoMode mirrors config.Config.DemoMode (env AA_DEMO_MODE). The
	// public demo runs every visitor on ONE shared account, so any
	// "your own data" surface is really "everyone's data" (#567).
	// Session listing narrows to the requesting session and
	// self-service revoke is limited to that same session.
	//
	// Presentational demo affordances stay where they were; this is
	// the first place the flag changes an authorization answer, and it
	// does so only to make "self" mean one person again. Off = zero
	// behavioural change.
	DemoMode bool

	tokenPrefix string // overridable in tests
}

// LockoutManager is the narrow surface auth.Handler consumes from the
// lockout package. Declared as an interface so this file doesn't
// import lockout directly (composition happens at boot).
// IncrementFailedLogin returns only error — the Manager itself owns
// the audit emit on the threshold-crossing attempt; the handler
// doesn't need to inspect the counter.
type LockoutManager interface {
	IsLockedOut(ctx context.Context, userRef int64) (bool, error)
	IncrementFailedLogin(ctx context.Context, userRef int64, ipSubnetHash string) error
	ResetFailedLogin(ctx context.Context, userRef int64) error
}

// SetLockoutManager attaches the lockout manager post-construction.
// Same pattern as SetPasswordPolicySource.
func (h *Handler) SetLockoutManager(m LockoutManager, ipSalt string) {
	h.LockoutMgr = m
	h.LockoutIPSalt = ipSalt
}

// SetPasswordPolicySource attaches the policy lookup post-
// construction so api.go's existing NewHandler call signature
// stays stable. Same pattern as users.Handler.SetAuditRecorder.
func (h *Handler) SetPasswordPolicySource(p passwordPolicySource) {
	h.Policy = p
}

// SetProviderRegistry attaches the identity-provider registry. Same
// post-construction pattern as SetPasswordPolicySource so api.go's
// NewHandler signature stays stable.
func (h *Handler) SetProviderRegistry(r *Registry) {
	h.Providers = r
}

// auditRecorder is the subset of audit.Recorder that the auth handler
// uses. Declared as an interface here so the auth package doesn't
// import the audit package directly — keeps the dependency arrow
// shallow and makes tests trivial to stub.
type auditRecorder interface {
	LoginSucceeded(ctx context.Context, req *http.Request, userRef int64, sessionID string)
	LoginFailed(ctx context.Context, req *http.Request, attemptedUsername string, userRef *int64, reason string)
	LoginRateLimited(ctx context.Context, req *http.Request, attemptedUsername, key string)
	Logout(ctx context.Context, req *http.Request, userRef int64, sessionID string)
	SessionRevoked(ctx context.Context, req *http.Request, userRef, actorUserRef int64, sessionID, reason string)
	PasswordChanged(ctx context.Context, req *http.Request, userRef int64, sessionsRevoked int)
	PasswordReset(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, reason string)
	CapabilityGranted(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID, note string)
	CapabilityRevoked(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID, note string)
	CapabilityGrantRemoved(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID string)
	CapabilityRevokeRemoved(ctx context.Context, req *http.Request, subjectUserRef, actorUserRef int64, capability, teamID string)
	ImpersonationStarted(ctx context.Context, req *http.Request, targetUserRef, adminUserRef int64, sessionID, reason string)
	ImpersonationEnded(ctx context.Context, req *http.Request, targetUserRef, adminUserRef int64, sessionID string)
	UserRegistered(ctx context.Context, req *http.Request, userRef int64, emailAddr string)
	UserEmailVerified(ctx context.Context, req *http.Request, userRef int64)
	// Phase 1.19.D — lockout audit surface.
	AuthLockoutTriggered(ctx context.Context, req *http.Request, userRef int64, failedCount, threshold, durationMinutes int32, ipSubnetHash string)
	AuthLockoutCleared(ctx context.Context, req *http.Request, userRef int64, actorUserRef *int64, priorFailedCount int32, source string)
}

// passwordPolicySource is the minimal interface the password
// handlers need to enforce complexity. Implemented by
// *sysconfig.Store; the interface form keeps auth from importing
// the sysconfig package directly so tests can stub it cheaply.
type passwordPolicySource interface {
	GetPasswordPolicy(ctx context.Context) (PasswordPolicy, error)
}

// PasswordPolicy mirrors sysconfig.PasswordPolicy without the
// import. Fields kept in lockstep — when sysconfig grows a knob,
// add it here too.
type PasswordPolicy struct {
	MinLength      int
	RequireUpper   bool
	RequireNumber  bool
	RequireSymbol  bool
	DisallowCommon bool
	MaxAgeDays     int
}

// nopAudit is the default Audit when none is wired up — useful in
// older tests that haven't been updated to pass a recorder.
type nopAudit struct{}

func (nopAudit) LoginSucceeded(context.Context, *http.Request, int64, string)       {}
func (nopAudit) LoginFailed(context.Context, *http.Request, string, *int64, string) {}
func (nopAudit) LoginRateLimited(context.Context, *http.Request, string, string)    {}
func (nopAudit) Logout(context.Context, *http.Request, int64, string)               {}
func (nopAudit) SessionRevoked(context.Context, *http.Request, int64, int64, string, string) {
}
func (nopAudit) PasswordChanged(context.Context, *http.Request, int64, int)         {}
func (nopAudit) PasswordReset(context.Context, *http.Request, int64, int64, string) {}
func (nopAudit) CapabilityGranted(context.Context, *http.Request, int64, int64, string, string, string) {
}
func (nopAudit) CapabilityRevoked(context.Context, *http.Request, int64, int64, string, string, string) {
}
func (nopAudit) CapabilityGrantRemoved(context.Context, *http.Request, int64, int64, string, string) {
}
func (nopAudit) CapabilityRevokeRemoved(context.Context, *http.Request, int64, int64, string, string) {
}
func (nopAudit) ImpersonationStarted(context.Context, *http.Request, int64, int64, string, string) {
}
func (nopAudit) ImpersonationEnded(context.Context, *http.Request, int64, int64, string) {
}
func (nopAudit) UserRegistered(context.Context, *http.Request, int64, string) {}
func (nopAudit) UserEmailVerified(context.Context, *http.Request, int64)      {}
func (nopAudit) AuthLockoutTriggered(context.Context, *http.Request, int64, int32, int32, int32, string) {
}
func (nopAudit) AuthLockoutCleared(context.Context, *http.Request, int64, *int64, int32, string) {}

// NewHandler constructs the auth handler. If sessionDays is <= 0 the
// default of 7 days (matching the legacy rs_setcookie default) is used. The
// session manager, login limiter, and audit recorder are required for
// production wiring; pass nil for any of them in tests to get a no-op
// fallback.
//
// cacheReg may be nil — in that case role-mutation paths don't
// broadcast caps invalidations and the Resolver's cache eventually
// evicts stale entries through LRU pressure. Production wires it.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger, scrambleKey string, sessionDays int, sessions *SessionManager, limiter *LoginLimiter, audit auditRecorder, cacheReg *cache.Registry) *Handler {
	if sessionDays <= 0 {
		sessionDays = 7
	}
	if sessions == nil {
		sessions = NewSessionManager(pool)
	}
	if limiter == nil {
		limiter = NewLoginLimiter()
	}
	if audit == nil {
		audit = nopAudit{}
	}
	return &Handler{
		Pool:        pool,
		Logger:      logger,
		ScrambleKey: scrambleKey,
		SessionDays: sessionDays,
		Sessions:    sessions,
		Limiter:     limiter,
		Audit:       audit,
		CacheReg:    cacheReg,
	}
}

// ---------------------------------------------------------------------------
// /auth/login
// ---------------------------------------------------------------------------

func (h *Handler) Login(
	ctx context.Context,
	req openapi.LoginRequestObject,
) (openapi.LoginResponseObject, error) {
	if req.Body == nil {
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "missing credentials"},
		}, nil
	}
	username := strings.TrimSpace(req.Body.Username)
	password := req.Body.Password
	if username == "" || password == "" {
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "missing credentials"},
		}, nil
	}

	// Provider selection: explicit name from the request, defaulting to
	// "password". An unknown provider name returns 401 with the same
	// "invalid credentials" body as a bad password — a probing client
	// can't enumerate which enterprise providers (LDAP, SAML aliases)
	// are registered on this install.
	providerName := "password"
	if req.Body.Provider != nil && strings.TrimSpace(*req.Body.Provider) != "" {
		providerName = strings.TrimSpace(*req.Body.Provider)
	}

	// Rate limit: two buckets, by IP and by attempted username. Either
	// trip rejects the attempt. The IP key isolates a noisy network
	// from drowning out other users; the username key isolates a
	// single account from being brute-forced from many IPs.
	httpReq := RequestFromContext(ctx)
	ipKey := "ip:" + clientIPKey(httpReq)
	userKey := "user:" + strings.ToLower(username)
	if !h.Limiter.Allow(ipKey) {
		h.Audit.LoginRateLimited(ctx, httpReq, username, ipKey)
		return loginRateLimitedResponse{}, nil
	}
	if !h.Limiter.Allow(userKey) {
		h.Audit.LoginRateLimited(ctx, httpReq, username, userKey)
		return loginRateLimitedResponse{}, nil
	}

	// totpCandidate is the optional second-factor input the
	// caller can supply on the first request. Empty when the
	// user is logging in for the first time + the frontend
	// hasn't seen a 2fa_required response yet.
	totpCandidate := ""
	if req.Body.TotpCode != nil {
		totpCandidate = strings.TrimSpace(*req.Body.TotpCode)
	}

	// Dispatch to the identity-provider registry when wired. The
	// registry handles the credential check; the handler stays in
	// charge of session minting, account-state gating, and audit.
	if h.Providers != nil {
		return h.loginViaRegistry(ctx, providerName, username, password, totpCandidate, httpReq, ipKey, userKey)
	}

	// Legacy path: no registry attached (tests). Inline password flow,
	// equivalent to the pre-registry behaviour. New code MUST attach a
	// registry; this branch is preserved for the auth package's own
	// tests which construct Handler directly.
	return h.loginInlinePassword(ctx, username, password, totpCandidate, httpReq, ipKey, userKey)
}

// loginViaRegistry runs the registry-dispatched login path. Provider
// is looked up by name; password-style providers are invoked via
// Authenticate. Unknown providers + redirect-only providers map to
// 401 with the canonical "invalid credentials" body so we don't leak
// which enterprise providers are configured.
func (h *Handler) loginViaRegistry(
	ctx context.Context,
	providerName, username, password, totpCandidate string,
	httpReq *http.Request,
	ipKey, userKey string,
) (openapi.LoginResponseObject, error) {
	p, ok := h.Providers.Get(providerName)
	if !ok {
		h.Audit.LoginFailed(ctx, httpReq, username, nil, "unknown_provider:"+providerName)
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
		}, nil
	}
	if !p.SupportsPassword() {
		// SAML/OIDC etc. — don't expose the provider kind to the
		// caller; same 401 shape as bad credentials.
		h.Audit.LoginFailed(ctx, httpReq, username, nil, "provider_unsupported_method:"+providerName)
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
		}, nil
	}

	// Phase 1.19.D — persistent per-username lockout gate. We look up
	// the user_ref BEFORE credential verification so we can short-
	// circuit locked accounts without ever consulting bcrypt. Timing
	// is preserved because we still run a fixed-work bcrypt against a
	// dummy hash on the locked path (see anti-enumeration test).
	//
	// Anonymous / unknown-username attempts don't hit this path —
	// they fall through to the provider's Authenticate which returns
	// ErrInvalidCredentials without exposing existence. The LoginLimiter's
	// `user:` bucket handles enumeration protection for unknown names.
	var lockedUserRef int64
	if h.LockoutMgr != nil {
		if ref, ok := h.lookupUserRefByUsername(ctx, username); ok {
			lockedUserRef = ref
			locked, lerr := h.LockoutMgr.IsLockedOut(ctx, ref)
			if lerr != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.lockout.check_error",
					slog.Int64("user_ref", ref), slog.String("err", lerr.Error()))
				// Fail-open on lockout error: the LoginLimiter still
				// protects; a DB blip shouldn't lock legitimate users
				// out of their accounts.
			} else if locked {
				// Preserve bcrypt-timing: run the provider's auth to
				// consume the same wall-clock work factor even though
				// we'll discard the result. Prevents timing side-
				// channel between locked-401 and wrong-password-401.
				_, _ = p.Authenticate(ctx, username, "\x00lockout-dummy-\x00")
				h.Audit.LoginFailed(ctx, httpReq, username, &ref, "account_locked")
				return openapi.Login401JSONResponse{
					UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
				}, nil
			}
		}
	}

	result, err := p.Authenticate(ctx, username, password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		h.Audit.LoginFailed(ctx, httpReq, username, nil, "bad_credentials:"+providerName)
		// Bump the persistent failed-attempt counter when we have a
		// real user_ref (username resolved above). Unknown usernames
		// don't accumulate — that's the LoginLimiter's job.
		if h.LockoutMgr != nil && lockedUserRef != 0 {
			ipHash := ipSubnetHash(httpReq, h.LockoutIPSalt)
			if ierr := h.LockoutMgr.IncrementFailedLogin(ctx, lockedUserRef, ipHash); ierr != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.lockout.increment_error",
					slog.Int64("user_ref", lockedUserRef), slog.String("err", ierr.Error()))
			}
			// The Manager already emitted auth.lockout.triggered
			// exactly once on the crossing attempt; nothing else to
			// do here beyond the standard bad_credentials audit
			// above. Non-trigger increments produce no additional
			// audit noise.
		}
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
		}, nil
	case errors.Is(err, ErrProviderUnsupportedMethod):
		h.Audit.LoginFailed(ctx, httpReq, username, nil, "provider_unsupported_method:"+providerName)
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
		}, nil
	case errors.Is(err, ErrProviderUnimplemented):
		// Distinct from 401: the provider IS registered (license has the
		// feature) but the binary doesn't yet ship the impl. Surfaces in
		// the admin UI as "build pending" rather than "wrong password".
		h.Audit.LoginFailed(ctx, httpReq, username, nil, "provider_unimplemented:"+providerName)
		return openapi.Login501JSONResponse{
			Error: "identity provider " + providerName + " is licensed but not yet implemented in this binary",
		}, nil
	case err != nil:
		return nil, err
	}

	// JIT provisioning hook reserved for 1.18 — when the result asks
	// for it, the handler will create a local user row before issuing
	// the session. For now the stubs never return JIT, so a
	// programming error (UserRef==0 && JIT==nil) deserves a loud 500.
	if result.UserRef == 0 {
		return nil, errors.New("auth: provider returned zero UserRef without JIT request")
	}

	q := New(h.Pool)
	user, err := q.FindUserByRef(ctx, result.UserRef)
	if err != nil {
		return nil, err
	}
	// Phase 1.17.A — single-gate. Replaces the inline approved +
	// expires checks scattered between the two login paths. The
	// typed UserState gate routes pending users to the "waiting
	// for approval" UX, and disabled/archived to a uniform 401
	// (the audit metadata reason carries the typed state).
	if resp, err := AssertCanAuthenticateUser(ctx, h.Audit, httpReq, username, userForAuthnGate{
		Ref: user.Ref, Approved: user.Approved, AccountExpires: user.AccountExpires,
	}); err != nil {
		return nil, err
	} else if resp != nil {
		return resp, nil
	}

	// Phase 1.19.B — second-factor gate.
	switch h.CheckTOTPForLogin(ctx, user.Ref, totpCandidate) {
	case TOTPGateRequired:
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "2fa_required")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "2fa_required"},
		}, nil
	case TOTPGateInvalid:
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "invalid_2fa_code")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid_2fa_code"},
		}, nil
	}

	// Phase 1.19.C — email-verification gate. Refuses login when
	// the user signed up via /auth/register and hasn't clicked
	// the link yet AND the install requires verification. Admin-
	// created users have email_verified_at backfilled to NOW(),
	// so this only ever fires for fresh self-registrations.
	if h.shouldGateOnEmailVerify(ctx, user.EmailVerifiedAt.Valid) {
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "email_not_verified")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "email_not_verified"},
		}, nil
	}

	token, sessionInfo, err := h.Sessions.Issue(ctx, user.Ref, httpReq)
	if err != nil {
		return nil, err
	}
	h.Limiter.Forget(ipKey)
	h.Limiter.Forget(userKey)
	// Phase 1.19.D — clear the persistent failed-attempt counter on
	// successful auth. Runs AFTER Sessions.Issue so a session-mint
	// failure doesn't strand the counter at zero. Best-effort: a
	// reset failure logs but doesn't fail the login.
	if h.LockoutMgr != nil {
		if rerr := h.LockoutMgr.ResetFailedLogin(ctx, user.Ref); rerr != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.lockout.reset_error",
				slog.Int64("user_ref", user.Ref), slog.String("err", rerr.Error()))
		}
	}
	h.Audit.LoginSucceeded(ctx, httpReq, user.Ref, sessionInfo.ID.String())

	current := identityToCurrentUser(&Identity{
		UserRef:    user.Ref,
		Username:   strFromPtr(user.Username),
		Fullname:   user.Fullname,
		Email:      user.Email,
		Usergroup:  user.Usergroup,
		AuthMethod: "session",
	})
	// Signing in IS the moment a cross-device preference has to prove
	// itself — it is the first thing a user does on a second machine.
	// Sign-in is a client-side navigation, so this response is the only
	// chance to deliver these before the browse page paints.
	h.hydrateSessionUser(ctx, user.Ref, &current)
	return loginSetCookieResponse{
		token:       token,
		sessionDays: h.SessionDays,
		body:        current,
	}, nil
}

// shouldGateOnEmailVerify reports whether login should be refused
// because the install requires email verification AND the user
// hasn't verified yet. Soft-fail open on a missing RegisterDeps
// (closed install / tests) so non-self-registered users keep
// working.
func (h *Handler) shouldGateOnEmailVerify(ctx context.Context, alreadyVerified bool) bool {
	if alreadyVerified {
		return false
	}
	if h.RegisterDeps.RegistrationPolicy == nil {
		return false
	}
	cfg, err := h.RegisterDeps.RegistrationPolicy(ctx)
	if err != nil {
		return false
	}
	return cfg.RequireEmailVerification
}

// loginInlinePassword is the pre-registry password flow, kept for
// tests that construct Handler directly without a registry. Production
// boot always attaches a registry, so this path is dead code in
// normal operation.
func (h *Handler) loginInlinePassword(
	ctx context.Context,
	username, password, totpCandidate string,
	httpReq *http.Request,
	ipKey, userKey string,
) (openapi.LoginResponseObject, error) {
	q := New(h.Pool)
	user, err := q.FindUserByUsername(ctx, &username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.Audit.LoginFailed(ctx, httpReq, username, nil, "unknown_user")
			return openapi.Login401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
			}, nil
		}
		return nil, err
	}
	if user.Password == nil {
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "no_password_set")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
		}, nil
	}
	if err := VerifyPassword(password, *user.Password, h.ScrambleKey); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "bad_password")
			return openapi.Login401JSONResponse{
				UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid credentials"},
			}, nil
		}
		return nil, err
	}
	// Phase 1.17.A — single-gate. Same call site as the registry path.
	if resp, err := AssertCanAuthenticateUser(ctx, h.Audit, httpReq, username, userForAuthnGate{
		Ref: user.Ref, Approved: user.Approved, AccountExpires: user.AccountExpires,
	}); err != nil {
		return nil, err
	} else if resp != nil {
		return resp, nil
	}
	// Phase 1.19.B — second-factor gate (same shape as registry path).
	switch h.CheckTOTPForLogin(ctx, user.Ref, totpCandidate) {
	case TOTPGateRequired:
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "2fa_required")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "2fa_required"},
		}, nil
	case TOTPGateInvalid:
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "invalid_2fa_code")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "invalid_2fa_code"},
		}, nil
	}
	// Phase 1.19.C — email-verification gate (mirrors registry path).
	if h.shouldGateOnEmailVerify(ctx, user.EmailVerifiedAt.Valid) {
		h.Audit.LoginFailed(ctx, httpReq, username, &user.Ref, "email_not_verified")
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "email_not_verified"},
		}, nil
	}
	token, sessionInfo, err := h.Sessions.Issue(ctx, user.Ref, httpReq)
	if err != nil {
		return nil, err
	}
	h.Limiter.Forget(ipKey)
	h.Limiter.Forget(userKey)
	// Phase 1.19.D — clear the persistent failed-attempt counter on
	// successful auth. Runs AFTER Sessions.Issue so a session-mint
	// failure doesn't strand the counter at zero. Best-effort: a
	// reset failure logs but doesn't fail the login.
	if h.LockoutMgr != nil {
		if rerr := h.LockoutMgr.ResetFailedLogin(ctx, user.Ref); rerr != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.lockout.reset_error",
				slog.Int64("user_ref", user.Ref), slog.String("err", rerr.Error()))
		}
	}
	h.Audit.LoginSucceeded(ctx, httpReq, user.Ref, sessionInfo.ID.String())
	current := identityToCurrentUser(&Identity{
		UserRef:    user.Ref,
		Username:   strFromPtr(user.Username),
		Fullname:   user.Fullname,
		Email:      user.Email,
		Usergroup:  user.Usergroup,
		AuthMethod: "session",
	})
	// Same as the password path above — a provider sign-in lands a user
	// on a fresh device just as often, so it needs the same payload.
	h.hydrateSessionUser(ctx, user.Ref, &current)
	return loginSetCookieResponse{
		token:       token,
		sessionDays: h.SessionDays,
		body:        current,
	}, nil
}

// ListIdentityProviders implements GET /auth/providers. Anonymous —
// the list is intentionally public so the login screen renders the
// SSO buttons even before authentication. Stable JSON shape: name +
// display_name + kind + supports_password.
func (h *Handler) ListIdentityProviders(
	_ context.Context,
	_ openapi.ListIdentityProvidersRequestObject,
) (openapi.ListIdentityProvidersResponseObject, error) {
	var items []openapi.IdentityProviderSummary
	if h.Providers != nil {
		for _, p := range h.Providers.List() {
			items = append(items, openapi.IdentityProviderSummary{
				Name:             p.Name(),
				DisplayName:      p.DisplayName(),
				Kind:             openapi.IdentityProviderSummaryKind(p.Kind()),
				SupportsPassword: p.SupportsPassword(),
			})
		}
	}
	if items == nil {
		// Even legacy/test setups that don't attach a registry should
		// see at least the password row so the login UI renders.
		items = []openapi.IdentityProviderSummary{{
			Name:             "password",
			DisplayName:      "Password",
			Kind:             openapi.IdentityProviderSummaryKind(KindPassword),
			SupportsPassword: true,
		}}
	}
	return openapi.ListIdentityProviders200JSONResponse{Providers: items}, nil
}

// clientIPKey returns the request's best-guess client IP as a string,
// falling back to "unknown" if there's no usable address. Only used as
// a rate-limiter key, so a coarse fallback is fine.
func clientIPKey(r *http.Request) string {
	addr := addrFromRequest(r)
	if addr == nil {
		return "unknown"
	}
	return addr.String()
}

// loginRateLimitedResponse implements openapi.LoginResponseObject and
// writes a 429 with a JSON body. The strict-server codegen only knows
// about the 200/401 responses we declared in the spec, so we emit the
// 429 by hand.
type loginRateLimitedResponse struct{}

func (loginRateLimitedResponse) VisitLoginResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"too many attempts; try again shortly"}`))
	return nil
}

// loginSetCookieResponse implements openapi.LoginResponseObject and
// sets the rs_session cookie on the way out. Custom response type
// because the generated 200 response doesn't know about cookies.
type loginSetCookieResponse struct {
	token       string
	sessionDays int
	body        openapi.CurrentUser
}

func (r loginSetCookieResponse) VisitLoginResponse(w http.ResponseWriter) error {
	// Build a synthetic *http.Request so the secure-cookie heuristics
	// work uniformly. The actual request scheme/header is unavailable
	// at this layer because of the strict-server abstraction.
	WriteSessionCookie(w, &http.Request{}, r.token, r.sessionDays)
	return openapi.Login200JSONResponse(r.body).VisitLoginResponse(w)
}

// ---------------------------------------------------------------------------
// /auth/logout
// ---------------------------------------------------------------------------

func (h *Handler) Logout(
	ctx context.Context,
	_ openapi.LogoutRequestObject,
) (openapi.LogoutResponseObject, error) {
	// Resolve from the request context (set by the resolver middleware).
	// If no identity, still clear the cookie and return 204 — logout is
	// idempotent. We revoke by cookie value (not user ref) so this only
	// kills *this* session, leaving other devices logged in.
	httpReq := RequestFromContext(ctx)
	cookie := ""
	if httpReq != nil {
		cookie = SessionCookieValue(httpReq)
	}
	if cookie != "" {
		if err := h.Sessions.RevokeByToken(ctx, cookie); err != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.logout.revoke.error",
				slog.String("err", err.Error()))
			// fall through; still expire cookie
		}
	}
	if id := IdentityFromContext(ctx); id != nil && id.AuthMethod == "session" {
		h.Audit.Logout(ctx, httpReq, id.UserRef, "")
	}
	return logoutClearCookieResponse{}, nil
}

type logoutClearCookieResponse struct{}

func (logoutClearCookieResponse) VisitLogoutResponse(w http.ResponseWriter) error {
	ClearSessionCookie(w, &http.Request{})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------------------------------------------------------------------------
// /auth/me
// ---------------------------------------------------------------------------

func (h *Handler) GetCurrentUser(
	ctx context.Context,
	_ openapi.GetCurrentUserRequestObject,
) (openapi.GetCurrentUserResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetCurrentUser401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	cu := identityToCurrentUser(id)

	// If this session was minted via /admin/users/{ref}/impersonate,
	// hydrate the impersonator's ref + username so the frontend
	// can render its "you are acting as @target" banner without
	// an extra round-trip.
	if id.ImpersonatedBy != nil {
		var adminUsername *string
		if err := h.Pool.QueryRow(ctx,
			`SELECT username FROM "user" WHERE ref = $1`, *id.ImpersonatedBy,
		).Scan(&adminUsername); err == nil && adminUsername != nil {
			cu.ImpersonatedBy = &struct {
				Ref      int64  `json:"ref"`
				Username string `json:"username"`
			}{
				Ref:      *id.ImpersonatedBy,
				Username: *adminUsername,
			}
		}
	}

	h.hydrateSessionUser(ctx, id.UserRef, &cu)

	return openapi.GetCurrentUser200JSONResponse(cu), nil
}

// hydrateSessionUser fills every field on a CurrentUser that does not
// come straight off the Identity: the stored preferences (language,
// theme, default views) and the caller's resolved capability set.
//
// THIS is the one function every CurrentUser producer calls. It exists
// so that "did this endpoint remember to populate X?" is a question
// with one answer for all of X, rather than one answer per field. Both
// halves of it were shipped incomplete exactly once — the preferences
// in #706, the capabilities in #871 — and in both cases the endpoint
// that forgot was the one nobody thought of as a session endpoint. A
// new field on CurrentUser belongs behind this call, not at the call
// sites.
func (h *Handler) hydrateSessionUser(ctx context.Context, userRef int64, cu *openapi.CurrentUser) {
	if h.Pool == nil || cu == nil {
		return
	}
	h.hydrateAccountPrefs(ctx, userRef, cu)
	h.hydrateCapabilities(ctx, userRef, cu)
}

// hydrateCapabilities fills CurrentUser.capabilities with the user's
// resolved GLOBAL capability codes (#871).
//
// Why this rides the session response at all: the SPA's auth store
// flips `ready` the moment /auth/me or /auth/login resolves, and the
// admin shell's gate reads `ready` and the capability set in the same
// breath. Capabilities that arrive on a second request
// (GET /auth/me/capabilities) therefore arrive strictly after the gate
// has already decided, and the gate's answer without them is "you
// don't have permission" — shown to a real administrator, in red,
// until the follow-up landed and it silently corrected itself. There
// is no ordering fix for that on the client; the only fix is for the
// answer to be in the response the decision is made from.
//
// The context short-circuit is not an optimisation detail, it is the
// correct source: on /auth/me the resolver middleware has ALREADY
// resolved this exact set for this exact request (and cached it), so
// re-deriving it from the DB would be both slower and a second chance
// to disagree with the identity the request is actually running as.
// It is guarded on the ref matching because POST /auth/login is
// reachable while holding somebody else's cookie — there the refs
// differ and we fall through to the query, which is what makes the
// response describe the account that just signed in rather than the
// one that was already signed in.
//
// Scoped (per-team) capabilities are deliberately excluded. The wire
// field is a flat list of codes with no room to say "…but only inside
// team X", and a scoped code flattened into it would read as global —
// i.e. the UI would offer a control that 403s. Global-only is the
// shape GET /auth/me/capabilities already publishes.
//
// Still best-effort, matching hydrateAccountPrefs: a failed lookup does
// not fail the session call. Failing the call would be worse than the
// bug — /auth/me is the boot gate for the WHOLE app, so a capability
// blip would lock a user out of the browse page rather than out of
// /admin.
//
// What changed in #956 is not the direction, it is the expressiveness.
// Failing closed on rights is right and stays; the response now also
// says WHICH closed door the client is looking at, via
// `capabilities_status`:
//
//   - resolved   — `capabilities` is authoritative. An empty list means
//     the account genuinely holds nothing.
//   - unavailable — the lookup failed. `capabilities` is omitted and
//     carries no information about this account.
//
// Before that field, those two were one wire shape. A transient
// resolver error handed an administrator an empty capability set, and
// web/src/routes/admin/+layout.svelte rendered "You don't have
// permission to view this page." — a sentence that is true for a
// powerless account and false for a database blip, with nothing on the
// wire to tell them apart. Neither the operator reading the panel nor a
// test asserting on it could distinguish them, which is why the nightly
// that hit this took four triage passes to narrow. The client still
// grants nothing on `unavailable`; it just stops calling it a
// permission decision.
//
// EVERY branch below assigns the status. That is the invariant this
// function owns: `CurrentUser.CapabilitiesStatus` is a required wire
// field whose Go zero value ("") is not a member of the enum, so a
// branch that returns without setting it ships an invalid response.
// TestSession_CapabilitiesStatusOnEveryProducer pins it.
func (h *Handler) hydrateCapabilities(ctx context.Context, userRef int64, cu *openapi.CurrentUser) {
	if id := IdentityFromContext(ctx); id != nil && id.UserRef == userRef && id.Capabilities != nil {
		caps := append([]string{}, id.Capabilities...)
		cu.Capabilities = &caps
		cu.CapabilitiesStatus = openapi.Resolved
		return
	}
	caps, err := New(h.Pool).EffectiveCapabilitiesForUser(ctx, userRef)
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.caps.session_hydrate.error",
				slog.Int64("user_ref", userRef), slog.String("err", err.Error()))
		}
		// Leave Capabilities nil. `unavailable` is what makes that
		// absence mean "unknown" rather than "none" — an empty slice
		// here would be a claim we cannot support.
		cu.CapabilitiesStatus = openapi.Unavailable
		return
	}
	if caps == nil {
		caps = []string{}
	}
	cu.Capabilities = &caps
	cu.CapabilitiesStatus = openapi.Resolved
}

// hydrateAccountPrefs fills the four stored-preference fields on a
// CurrentUser: language, theme, default views, and feed filters.
// Reached through [Handler.hydrateSessionUser], which is what producers
// call.
//
// EVERY endpoint that returns a CurrentUser must call this, and that
// is the whole reason it is a method rather than four lines inlined in
// /auth/me. `CurrentUser` is one schema used by both /auth/me and
// /auth/login, so a login response that omits these fields is not a
// smaller response — it is the declared schema, returned with three
// documented fields silently empty.
//
// That is exactly what shipped and what #706 review caught: the browse
// store and the theme store both read these off the session, both
// correctly no-opped when they were absent, and the account
// preferences therefore did not apply until the user happened to
// trigger a full page load. `language` had the same hole and nobody
// had noticed, because a locale that only takes effect on the second
// page load looks like a slow render rather than a bug.
//
// The frontend cannot paper over this with a follow-up /auth/me: the
// point of carrying the fields on the session response is that they
// arrive BEFORE first paint, and a second round-trip lands after it.
//
// The anchor sub-select is what makes the two LEFT JOINs safe: a user
// can have preferences without a profile row or the reverse (they are
// written by different surfaces), and joining from either table would
// drop the other's values whenever its own row happens to be missing.
// Anchoring on the ref means exactly one row comes back, with NULLs
// standing in for whichever side has nothing yet.
//
// Best-effort by design. These are render hints on the call that gates
// the entire app, so a failure leaves them nil and the client falls
// back to its built-in defaults — it never fails the login or the
// session check.
//
// One producer deliberately does NOT go through hydrateSessionUser:
// setup.Handler's /setup/complete, which mints the very first admin.
// It lives in another package and would need an auth-handler
// dependency plumbed in to fill a response the client immediately
// discards — the setup page calls auth.refresh() (i.e. /auth/me) the
// line after it lands, so the session the operator actually browses on
// is fully hydrated regardless. It is still a real omission rather
// than a provable no-op: /setup/complete commits the admin's role
// assignment before it builds the response, so the capability list it
// leaves absent is one that exists.
//
// What #956 changed is that the omission is now DECLARED. That handler
// sets capabilities_status: unavailable, so anything that ever starts
// reading its body as a session gets "this response cannot tell you
// your rights" — a retry — instead of a silently empty set that the
// /admin gate would render as "you don't have permission". It is also
// why that status field is required rather than "absent means
// unknown": `capabilities` is legitimately absent there while the
// account demonstrably holds things, so absence alone could never have
// carried the meaning.
func (h *Handler) hydrateAccountPrefs(ctx context.Context, userRef int64, cu *openapi.CurrentUser) {
	var lang, theme string
	var viewsJSON, filtersJSON []byte
	err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(p.language, ''),
		       COALESCE(p.theme, ''),
		       COALESCE(up.default_views, '{}'::jsonb),
		       COALESCE(up.feed_filters, '{}'::jsonb)
		FROM (SELECT $1::bigint AS user_ref) k
		LEFT JOIN user_profiles    p  ON p.user_ref  = k.user_ref
		LEFT JOIN user_preferences up ON up.user_ref = k.user_ref`,
		userRef,
	).Scan(&lang, &theme, &viewsJSON, &filtersJSON)
	if err != nil {
		// pgx.ErrNoRows is fine; we leave the fields nil.
		return
	}
	if lang != "" {
		l := lang
		cu.Language = &l
	}
	if theme != "" {
		t := openapi.CurrentUserTheme(theme)
		cu.Theme = &t
	}
	if v, ok := decodeDefaultViews(viewsJSON); ok {
		cu.DefaultViews = &v
	}
	if f, ok := decodeFeedFilters(filtersJSON); ok {
		cu.FeedFilters = &f
	}
}

// decodeFeedFilters parses the user_preferences.feed_filters blob into
// the wire type (#891). Reports false when every key is at its zero
// value, so /auth/me omits the object for the overwhelming majority of
// accounts rather than shipping an object of falses — which is also what
// keeps this key invisible to every session on the build's defaults.
//
// #921 inverted the default and this held with no change of SHAPE, only
// of which accounts are the majority: the key is now `show_restricted`
// and it is still the opted-IN accounts that carry an object. Preserve
// that. "Omit when nil-or-false" is what makes an absent object and a
// default account the same fact on the wire.
//
// Same "render hint, never fail the call" posture as decodeDefaultViews
// above, and for the sharper reason: this field's whole job is to let
// the browse page EXPLAIN its feed. Failing /auth/me over an unreadable
// preferences column would lock the user out of the page where they
// could change the setting.
func decodeFeedFilters(raw []byte) (openapi.UserPreferencesFeedFilters, bool) {
	var f openapi.UserPreferencesFeedFilters
	if len(raw) == 0 {
		return f, false
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return openapi.UserPreferencesFeedFilters{}, false
	}
	if f.ShowRestricted == nil || !*f.ShowRestricted {
		return openapi.UserPreferencesFeedFilters{}, false
	}
	return f, true
}

// decodeDefaultViews parses the user_preferences.default_views blob
// into the wire type, dropping any selection this build no longer
// serves. Reports false when nothing survives, so /auth/me omits the
// key entirely rather than shipping an empty object.
//
// It decodes straight into the GENERATED type rather than a local
// struct, which is what keeps the vocabulary in one place: the enum
// members and the Valid() methods below both come from
// UserPreferencesViews in openapi.yaml, so a value removed there
// starts being dropped here with no second list to remember to edit.
// (The obvious alternative — importing userprefs, which owns the same
// rule for GET /account/preferences — is an import cycle: userprefs
// depends on this package for IdentityFromContext.)
//
// A malformed blob is treated as "no selections". This is a render
// hint on the session endpoint, and failing /auth/me — the call that
// gates the entire app — over an unreadable preferences column would
// lock a user out of the page where they could fix it.
func decodeDefaultViews(raw []byte) (openapi.UserPreferencesViews, bool) {
	var v openapi.UserPreferencesViews
	if len(raw) == 0 {
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return openapi.UserPreferencesViews{}, false
	}
	if v.HomeTab != nil && (*v.HomeTab == "" || !v.HomeTab.Valid()) {
		v.HomeTab = nil
	}
	if v.BrowseLayout != nil && (*v.BrowseLayout == "" || !v.BrowseLayout.Valid()) {
		v.BrowseLayout = nil
	}
	if v.BrowseSort != nil && (*v.BrowseSort == "" || !v.BrowseSort.Valid()) {
		v.BrowseSort = nil
	}
	if v.HomeTab == nil && v.BrowseLayout == nil && v.BrowseSort == nil {
		return openapi.UserPreferencesViews{}, false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// /auth/tokens (list, create)
// ---------------------------------------------------------------------------

func (h *Handler) ListApiTokens(
	ctx context.Context,
	_ openapi.ListApiTokensRequestObject,
) (openapi.ListApiTokensResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListApiTokens401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListApiTokensForUser(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	out := make(openapi.ListApiTokens200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.ApiTokenSummary{
			Id:         openapi_types.UUID(r.ID.Bytes),
			Name:       r.Name,
			Scopes:     append([]string{}, r.Scopes...),
			CreatedAt:  r.CreatedAt.Time,
			ExpiresAt:  ptrTime(r.ExpiresAt),
			LastUsedAt: ptrTime(r.LastUsedAt),
		})
	}
	return out, nil
}

func (h *Handler) CreateApiToken(
	ctx context.Context,
	req openapi.CreateApiTokenRequestObject,
) (openapi.CreateApiTokenResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.CreateApiToken401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	// Spec disallows creating tokens via a token-authenticated session
	// (cookie-only). The OpenAPI spec records this but we must enforce
	// at runtime too — codegen doesn't.
	if id.AuthMethod != "session" {
		return openapi.CreateApiToken401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "token creation requires session auth"},
		}, nil
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Name) == "" {
		return openapi.CreateApiToken400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "name is required"},
		}, nil
	}

	plaintext, err := NewAPIToken()
	if err != nil {
		return nil, err
	}
	scopes := []string{}
	if req.Body.Scopes != nil {
		scopes = *req.Body.Scopes
	}
	params := CreateApiTokenParams{
		UserRef:   id.UserRef,
		Name:      strings.TrimSpace(req.Body.Name),
		TokenHash: HashAPIToken(plaintext),
		Scopes:    scopes,
	}
	if req.Body.ExpiresAt != nil {
		params.ExpiresAt = pgtype.Timestamptz{Time: *req.Body.ExpiresAt, Valid: true}
	}

	q := New(h.Pool)
	row, err := q.CreateApiToken(ctx, params)
	if err != nil {
		return nil, err
	}

	return openapi.CreateApiToken201JSONResponse(openapi.ApiTokenCreated{
		Id:         openapi_types.UUID(row.ID.Bytes),
		Name:       row.Name,
		Scopes:     append([]string{}, row.Scopes...),
		CreatedAt:  row.CreatedAt.Time,
		ExpiresAt:  ptrTime(row.ExpiresAt),
		LastUsedAt: ptrTime(row.LastUsedAt),
		Token:      plaintext,
	}), nil
}

// ---------------------------------------------------------------------------
// /auth/tokens/{id} (revoke)
// ---------------------------------------------------------------------------

func (h *Handler) RevokeApiToken(
	ctx context.Context,
	req openapi.RevokeApiTokenRequestObject,
) (openapi.RevokeApiTokenResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.RevokeApiToken401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	n, err := q.RevokeApiToken(ctx, RevokeApiTokenParams{
		ID:      pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true},
		UserRef: id.UserRef,
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return openapi.RevokeApiToken404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "token not found"},
		}, nil
	}
	return openapi.RevokeApiToken204Response{}, nil
}

// ---------------------------------------------------------------------------
// /auth/capabilities, /auth/roles, /auth/me/capabilities, /auth/users/{ref}/role
// ---------------------------------------------------------------------------

func (h *Handler) ListCapabilities(
	ctx context.Context,
	_ openapi.ListCapabilitiesRequestObject,
) (openapi.ListCapabilitiesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListCapabilities401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("caps.read") {
		return openapi.ListCapabilities403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "caps.read capability required"},
		}, nil
	}
	q := New(h.Pool)
	rows, err := q.ListCapabilities(ctx)
	if err != nil {
		return nil, err
	}
	out := make(openapi.ListCapabilities200JSONResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.Capability{Code: r.Code, Description: r.Description})
	}
	return out, nil
}

func (h *Handler) ListRoles(
	ctx context.Context,
	_ openapi.ListRolesRequestObject,
) (openapi.ListRolesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListRoles401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("roles.read") {
		return openapi.ListRoles403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "roles.read capability required"},
		}, nil
	}
	q := New(h.Pool)
	roles, err := q.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	out := make(openapi.ListRoles200JSONResponse, 0, len(roles))
	for _, r := range roles {
		caps, err := q.ListRoleCapabilities(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		role := openapi.Role{
			Id:           openapi_types.UUID(r.ID.Bytes),
			Name:         r.Name,
			Description:  r.Description,
			Capabilities: caps,
		}
		if r.ParentID.Valid {
			p := openapi_types.UUID(r.ParentID.Bytes)
			role.ParentId = &p
		}
		out = append(out, role)
	}
	return out, nil
}

func (h *Handler) GetMyCapabilities(
	ctx context.Context,
	_ openapi.GetMyCapabilitiesRequestObject,
) (openapi.GetMyCapabilitiesResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetMyCapabilities401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	q := New(h.Pool)
	// The API surface currently exposes a single "role" field; with the
	// multi-role model (00001) we surface the user's first GLOBAL role
	// assignment for that field — team-scoped assignments aren't
	// representable here. The 1.7.B-7 OpenAPI widening will switch this
	// to a roles[] list with optional team scope per entry.
	roleRows, err := q.AssignedRolesForUser(ctx, id.UserRef)
	if err != nil {
		return nil, err
	}
	var roleOpenapi *openapi.Role
	for _, r := range roleRows {
		if r.TeamID.Valid {
			continue // skip team-scoped assignments for the single-role field
		}
		caps, err := q.ListRoleCapabilities(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		ro := openapi.Role{
			Id:           openapi_types.UUID(r.ID.Bytes),
			Name:         r.Name,
			Description:  r.Description,
			Capabilities: caps,
		}
		if r.ParentID.Valid {
			p := openapi_types.UUID(r.ParentID.Bytes)
			ro.ParentId = &p
		}
		roleOpenapi = &ro
		break
	}

	grants, err := h.fetchSimpleCapList(ctx, `SELECT capability_code FROM user_capability_grants WHERE user_ref = $1 ORDER BY capability_code`, id.UserRef)
	if err != nil {
		return nil, err
	}
	revokes, err := h.fetchSimpleCapList(ctx, `SELECT capability_code FROM user_capability_revokes WHERE user_ref = $1 ORDER BY capability_code`, id.UserRef)
	if err != nil {
		return nil, err
	}

	caps := append([]string{}, id.Capabilities...) // copy
	resp := openapi.EffectiveCapabilities{
		UserRef:      id.UserRef,
		Capabilities: caps,
		Role:         roleOpenapi,
		Grants:       &grants,
		Revokes:      &revokes,
	}
	return openapi.GetMyCapabilities200JSONResponse(resp), nil
}

func (h *Handler) SetUserRole(
	ctx context.Context,
	req openapi.SetUserRoleRequestObject,
) (openapi.SetUserRoleResponseObject, error) {
	id := IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetUserRole401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can("users.write") {
		return openapi.SetUserRole403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "users.write capability required"},
		}, nil
	}
	if req.Body == nil {
		return nil, errors.New("missing body") // strict-server returns 500; the spec says this is required so codegen normally rejects nil
	}

	q := New(h.Pool)
	roleUUID := pgtype.UUID{Bytes: uuid.UUID(req.Body.RoleId), Valid: true}
	// 404 if the role doesn't exist.
	if _, err := q.GetRole(ctx, roleUUID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.SetUserRole404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "role not found"},
			}, nil
		}
		return nil, err
	}

	// Last-admin invariant: if the new role does NOT grant
	// system.admin AND the target user is the last admin,
	// refuse. Swap to a role that ALSO grants system.admin is
	// allowed (the admin count stays >= 1).
	if grants, err := q.RoleGrantsSystemAdmin(ctx, roleUUID); err != nil {
		return nil, err
	} else if grants == 0 {
		if err := EnsureNotLastAdmin(ctx, q, req.Ref); err != nil {
			if errors.Is(err, ErrLastAdmin) {
				return openapi.SetUserRole400JSONResponse{Error: err.Error()}, nil
			}
			return nil, err
		}
	}

	// Sets the user's GLOBAL role (replaces any existing global
	// assignment; leaves team-scoped assignments intact). The admin
	// endpoint shape hasn't changed; only the storage semantics did.
	if err := q.SetUserGlobalRole(ctx, SetUserGlobalRoleParams{
		UserRef:           req.Ref,
		RoleID:            roleUUID,
		AssignedByUserRef: &id.UserRef,
	}); err != nil {
		return nil, err
	}
	// The caller's effective caps just changed — broadcast so the
	// Resolver's caps cache (this instance + every federated peer)
	// drops the stale entry. The very next request from this user
	// gets fresh caps.
	InvalidateUserCaps(ctx, h.CacheReg, req.Ref)
	return openapi.SetUserRole204Response{}, nil
}

// fetchSimpleCapList runs a single-column-of-text query and collects
// the results. Small helper used by GetMyCapabilities for the grant
// and revoke lists; not worth its own sqlc entry.
func (h *Handler) fetchSimpleCapList(ctx context.Context, sql string, userRef int64) ([]string, error) {
	rows, err := h.Pool.Query(ctx, sql, userRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func identityToCurrentUser(id *Identity) openapi.CurrentUser {
	cu := openapi.CurrentUser{
		Ref:        id.UserRef,
		Username:   id.Username,
		Fullname:   id.Fullname,
		Email:      id.Email,
		Usergroup:  id.Usergroup,
		AuthMethod: openapi.CurrentUserAuthMethod(id.AuthMethod),
	}
	return cu
}

func strFromPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
