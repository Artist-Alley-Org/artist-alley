// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// registerUsernamePattern enforces the username shape promised in
// the OpenAPI spec — 3-32 chars, [a-zA-Z0-9_-]. Server-authoritative;
// the frontend's identical regex is UX only.
var registerUsernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

// registerVerificationTTL is the lifetime of a fresh email-
// verification link. 1h is the sweet spot — long enough that
// "I'll click it later" works after dinner, short enough that a
// leaked link in someone else's inbox is briefly useful.
const registerVerificationTTL = 1 * time.Hour

// RegisterSurface bundles the cross-package seams the registration
// handler needs. Boot wires this; tests fake it. All seams use
// plain function types so this package stays free of the email +
// sysconfig dependency edges (the boot wire converts those
// packages' types into these closures).
type RegisterSurface struct {
	// SendVerification dispatches the verification email. Caller
	// has already minted the token + computed the URL; this
	// function just renders + sends. Nil-safe — when unwired the
	// handler logs + skips so the registration row still lands
	// (operator can resend via /auth/resend-verification later).
	SendVerification SendVerificationFn

	// SiteForVerify returns the branding the verify URL needs
	// (base URL + display name for the email body). Nil-safe:
	// falls back to an empty base + the username for the name.
	SiteForVerify func(ctx context.Context) (SiteForVerify, error)

	// RegistrationPolicy returns the current self-registration
	// config — Enabled gate, RequireEmailVerification flag,
	// DefaultRole. Nil-safe: when unwired the handler treats
	// registration as disabled (refuses 403).
	RegistrationPolicy func(ctx context.Context) (RegistrationConfig, error)
}

// SendVerificationFn is the closure boot wires using the email
// package's Render + Sender. Keeps the email-package dep edge
// pointing at boot, not at auth.
type SendVerificationFn func(ctx context.Context, to, recipientName, verifyURL, expiresIn string) error

// SiteForVerify mirrors email.SiteContext without the import —
// just the per-instance branding bits the verify link + email
// body care about.
type SiteForVerify struct {
	Name string
	URL  string
}

// RegistrationConfig mirrors the sysconfig.SelfRegistration shape
// without importing sysconfig (closes the import cycle).
type RegistrationConfig struct {
	Enabled                  bool
	RequireEmailVerification bool
	DefaultRole              string
}

// SetRegistrationSurface wires the registration dependencies
// post-construction (same pattern as SetAuditRecorder).
func (h *Handler) SetRegistrationSurface(s RegisterSurface) { h.RegisterDeps = s }

// Register implements POST /auth/register.
func (h *Handler) Register(
	ctx context.Context,
	req openapi.RegisterRequestObject,
) (openapi.RegisterResponseObject, error) {
	if req.Body == nil {
		return openapi.Register400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	policy, err := h.loadRegistrationPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if !policy.Enabled {
		return openapi.Register403JSONResponse{Error: "self-registration is disabled on this install"}, nil
	}

	username := strings.TrimSpace(req.Body.Username)
	emailAddr := strings.ToLower(strings.TrimSpace(string(req.Body.Email)))
	password := req.Body.Password
	fullname := ""
	if req.Body.Fullname != nil {
		fullname = strings.TrimSpace(*req.Body.Fullname)
	}

	if !registerUsernamePattern.MatchString(username) {
		return openapi.Register400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "username must be 3-32 chars [a-zA-Z0-9_-]"},
		}, nil
	}
	if emailAddr == "" || !strings.Contains(emailAddr, "@") {
		return openapi.Register400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "valid email is required"},
		}, nil
	}
	if h.Policy != nil {
		pp, err := h.Policy.GetPasswordPolicy(ctx)
		if err != nil {
			return nil, fmt.Errorf("auth: load password policy: %w", err)
		}
		if msg := validatePasswordPolicy(password, pp); msg != "" {
			return openapi.Register400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
			}, nil
		}
	}

	// Rate limit: per-IP + per-email so a single attacker can't
	// hammer + a single email can't be enumerated. The error
	// shape matches the login rate-limit branch.
	httpReq := RequestFromContext(ctx)
	ipKey := "register-ip:" + clientIPKey(httpReq)
	emailKey := "register-email:" + emailAddr
	if !h.Limiter.Allow(ipKey) || !h.Limiter.Allow(emailKey) {
		return openapi.Register429JSONResponse{
			TooManyRequestsJSONResponse: openapi.TooManyRequestsJSONResponse{Error: "too many attempts; try again shortly"},
		}, nil
	}

	hash, err := HashPassword(password, h.ScrambleKey)
	if err != nil {
		return nil, fmt.Errorf("auth: hash: %w", err)
	}

	q := New(h.Pool)
	pwPtr := &hash
	usernamePtr := &username
	emailPtr := &emailAddr
	fullnamePtr := &fullname
	if fullname == "" {
		fullnamePtr = nil
	}
	row, err := q.CreateUserForRegistration(ctx, CreateUserForRegistrationParams{
		Username: usernamePtr,
		Password: pwPtr,
		Email:    emailPtr,
		Fullname: fullnamePtr,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return openapi.Register409JSONResponse{Error: "username or email already in use"}, nil
		}
		return nil, fmt.Errorf("auth: create user: %w", err)
	}

	// Assign the configured default role so the new user inherits
	// the right cap set. Soft-failure (warn + continue) when the
	// role isn't found — the user can still log in once verified;
	// an admin can assign a role manually.
	if role, err := q.FindRoleByName(ctx, defaultRoleOrBase(policy.DefaultRole)); err == nil {
		_ = q.SetUserGlobalRole(ctx, SetUserGlobalRoleParams{UserRef: row.Ref, RoleID: role.ID})
	} else if h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "auth.register.role_assign_skipped",
			slog.Int64("user_ref", row.Ref),
			slog.String("role", policy.DefaultRole),
			slog.String("err", err.Error()),
		)
	}

	if !policy.RequireEmailVerification {
		// Mark verified immediately + issue a session.
		if err := q.MarkUserEmailVerified(ctx, row.Ref); err != nil {
			return nil, fmt.Errorf("auth: mark verified: %w", err)
		}
		token, _, err := h.Sessions.Issue(ctx, row.Ref, httpReq)
		if err != nil {
			return nil, err
		}
		return registerSetCookieResponse{
			token:       token,
			sessionDays: h.SessionDays,
			body: openapi.CurrentUser{
				Ref:        row.Ref,
				Username:   derefOr(row.Username, username),
				Email:      row.Email,
				AuthMethod: openapi.CurrentUserAuthMethod("session"),
			},
		}, nil
	}

	// Verification required — mint a token + send the email.
	plain, hashed, err := mintVerificationToken()
	if err != nil {
		return nil, fmt.Errorf("auth: mint verification token: %w", err)
	}
	if err := q.InsertEmailVerificationToken(ctx, InsertEmailVerificationTokenParams{
		UserRef:   row.Ref,
		TokenHash: hashed,
		Purpose:   "register",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(registerVerificationTTL), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("auth: insert verification token: %w", err)
	}

	h.sendVerificationEmail(ctx, emailAddr, derefOr(row.Username, username), plain)

	if h.Audit != nil {
		h.Audit.UserRegistered(ctx, httpReq, row.Ref, emailAddr)
	}
	return openapi.Register202JSONResponse{
		UserRef: row.Ref,
		Email:   openapi_email(emailAddr),
		Message: "Account created. Check your email for a verification link.",
	}, nil
}

// VerifyEmail implements POST /auth/verify-email.
func (h *Handler) VerifyEmail(
	ctx context.Context,
	req openapi.VerifyEmailRequestObject,
) (openapi.VerifyEmailResponseObject, error) {
	if req.Body == nil || strings.TrimSpace(req.Body.Token) == "" {
		return openapi.VerifyEmail400JSONResponse{Error: "token is required"}, nil
	}
	hashed := hashVerificationToken(strings.TrimSpace(req.Body.Token))

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("auth: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	q := New(tx)

	tok, err := q.FindActiveEmailVerificationToken(ctx, hashed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.VerifyEmail400JSONResponse{Error: "token is invalid, expired, or already used"}, nil
		}
		return nil, fmt.Errorf("auth: find token: %w", err)
	}
	if err := q.ConsumeEmailVerificationToken(ctx, tok.ID); err != nil {
		return nil, fmt.Errorf("auth: consume token: %w", err)
	}
	if err := q.MarkUserEmailVerified(ctx, tok.UserRef); err != nil {
		return nil, fmt.Errorf("auth: mark verified: %w", err)
	}

	user, err := q.FindUserByRef(ctx, tok.UserRef)
	if err != nil {
		return nil, fmt.Errorf("auth: load user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("auth: commit: %w", err)
	}

	if h.Audit != nil {
		h.Audit.UserEmailVerified(ctx, RequestFromContext(ctx), user.Ref)
	}
	return openapi.VerifyEmail200JSONResponse{
		UserRef:  user.Ref,
		Username: derefOr(user.Username, ""),
		Email:    openapi_email(derefOr(user.Email, "")),
	}, nil
}

// ResendVerificationEmail implements POST /auth/resend-verification.
// Always returns 202, regardless of whether the address matches —
// anti-enumeration measure.
func (h *Handler) ResendVerificationEmail(
	ctx context.Context,
	req openapi.ResendVerificationEmailRequestObject,
) (openapi.ResendVerificationEmailResponseObject, error) {
	if req.Body == nil {
		return openapi.ResendVerificationEmail400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	emailAddr := strings.ToLower(strings.TrimSpace(string(req.Body.Email)))
	if emailAddr == "" || !strings.Contains(emailAddr, "@") {
		return openapi.ResendVerificationEmail400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "valid email is required"},
		}, nil
	}

	httpReq := RequestFromContext(ctx)
	if !h.Limiter.Allow("register-resend-ip:"+clientIPKey(httpReq)) ||
		!h.Limiter.Allow("register-resend-email:"+emailAddr) {
		return openapi.ResendVerificationEmail429JSONResponse{
			TooManyRequestsJSONResponse: openapi.TooManyRequestsJSONResponse{Error: "too many attempts; try again shortly"},
		}, nil
	}

	q := New(h.Pool)
	user, err := q.FindUserByEmail(ctx, emailAddr)
	if err != nil {
		// Anti-enumeration: return 202 even when no match.
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.ResendVerificationEmail202Response{}, nil
		}
		return nil, fmt.Errorf("auth: lookup user: %w", err)
	}
	if user.EmailVerifiedAt.Valid {
		// Already verified — no email to send. Same 202.
		return openapi.ResendVerificationEmail202Response{}, nil
	}

	plain, hashed, err := mintVerificationToken()
	if err != nil {
		return nil, fmt.Errorf("auth: mint resend token: %w", err)
	}
	if err := q.InsertEmailVerificationToken(ctx, InsertEmailVerificationTokenParams{
		UserRef:   user.Ref,
		TokenHash: hashed,
		Purpose:   "register",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(registerVerificationTTL), Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("auth: insert resend token: %w", err)
	}
	h.sendVerificationEmail(ctx, emailAddr, derefOr(user.Username, ""), plain)
	return openapi.ResendVerificationEmail202Response{}, nil
}

// --- helpers ----------------------------------------------------------------

func (h *Handler) loadRegistrationPolicy(ctx context.Context) (RegistrationConfig, error) {
	if h.RegisterDeps.RegistrationPolicy == nil {
		return RegistrationConfig{}, nil
	}
	return h.RegisterDeps.RegistrationPolicy(ctx)
}

// sendVerificationEmail dispatches via the boot-wired closure.
// Soft-failure: a send error logs but doesn't fail registration
// — the operator can use the resend endpoint or the future
// "force verify" admin path.
func (h *Handler) sendVerificationEmail(ctx context.Context, to, recipientName, token string) {
	if h.RegisterDeps.SendVerification == nil {
		if h.Logger != nil {
			h.Logger.Warn("auth.register.email_skipped_no_sender",
				"recipient", to,
			)
		}
		return
	}
	site := SiteForVerify{}
	if h.RegisterDeps.SiteForVerify != nil {
		if sc, err := h.RegisterDeps.SiteForVerify(ctx); err == nil {
			site = sc
		}
	}
	verifyURL := buildVerifyURL(site.URL, token)
	if err := h.RegisterDeps.SendVerification(ctx, to, recipientName, verifyURL, "1 hour"); err != nil && h.Logger != nil {
		h.Logger.Warn("auth.register.send_error", "err", err.Error())
	}
}

// buildVerifyURL composes the user-facing verify link the email
// carries. Falls back to a token-bare URL when site_url is unset
// (dev environment without sysconfig site row populated); the
// frontend still extracts the `token` query param to POST against
// /auth/verify-email.
func buildVerifyURL(baseURL, token string) string {
	v := url.Values{}
	v.Set("token", token)
	if baseURL == "" {
		return "/auth/verify?" + v.Encode()
	}
	return strings.TrimRight(baseURL, "/") + "/auth/verify?" + v.Encode()
}

// mintVerificationToken returns the plaintext (32-byte URL-safe
// base64) + its sha256 hash. The plaintext only ever travels in
// the email body; only the hash hits the DB.
func mintVerificationToken() (plain string, hashed []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("rand: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(plain))
	return plain, sum[:], nil
}

func hashVerificationToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

func defaultRoleOrBase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Base"
	}
	return s
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func ptrEmail(p *string) openapi_types.Email {
	if p == nil {
		return openapi_types.Email("")
	}
	return openapi_types.Email(*p)
}

// openapi_email is a one-line helper that keeps the
// openapi_types.Email type token short at the call site.
func openapi_email(s string) openapi_types.Email { return openapi_types.Email(s) }

// isUniqueViolation reports whether err is a Postgres 23505
// (unique_violation), so the handler can return 409 instead of
// 500 on a duplicate username/email.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	return false
}

// --- cookie-setting shim ---

// registerSetCookieResponse implements
// openapi.RegisterResponseObject. The 200 path (verification
// off) rotates the cookie to a freshly-minted session so the
// caller is signed in immediately.
type registerSetCookieResponse struct {
	token       string
	sessionDays int
	body        openapi.CurrentUser
}

func (r registerSetCookieResponse) VisitRegisterResponse(w http.ResponseWriter) error {
	WriteSessionCookie(w, &http.Request{}, r.token, r.sessionDays)
	return openapi.Register200JSONResponse(r.body).VisitRegisterResponse(w)
}
