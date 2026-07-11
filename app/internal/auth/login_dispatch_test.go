// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Tests for the registry-dispatched Login path added in Phase
// 1.17.P-foundation. The load-bearing property here is
// anti-enumeration: a probing client must NOT be able to tell
// "unknown provider name" from "bad password" from "this provider
// doesn't support the password form". All three return the SAME 401
// shape with the constant "invalid credentials" body.
//
// The bad-password branch needs a real DB row to exercise, so we
// pin it via the existing integration-style handler tests in
// handler_test.go. This file targets the short-circuit branches that
// don't touch the DB: those run inline in loginViaRegistry before
// any DB query, which means we can drive them with a nil pool +
// stub providers and still observe the response shape.

// dispatchTestHandler builds a minimal Handler wired with a
// Registry, no DB pool, no SessionManager. Sufficient for the
// pre-DB short-circuit branches we want to exercise.
func dispatchTestHandler(reg *Registry) *Handler {
	return &Handler{
		Limiter:   NewLoginLimiter(),
		Audit:     nopAudit{},
		Providers: reg,
	}
}

func mustLogin(t *testing.T, h *Handler, body *openapi.LoginJSONRequestBody) openapi.LoginResponseObject {
	t.Helper()
	resp, err := h.Login(context.Background(), openapi.LoginRequestObject{Body: body})
	if err != nil {
		t.Fatalf("Login returned err: %v", err)
	}
	return resp
}

// Unknown provider name → same 401 + "invalid credentials" body as a
// bad password would produce. A probing client cannot tell whether
// the install has an "ldap-okta" provider configured.
func TestLogin_UnknownProviderReturns401WithCanonicalBody(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&stubProvider{
		name:             "password",
		displayName:      "Password",
		kind:             KindPassword,
		supportsPassword: true,
	}, nil)
	h := dispatchTestHandler(reg)

	provider := "definitely-not-registered"
	resp := mustLogin(t, h, &openapi.LoginJSONRequestBody{
		Username: "alice",
		Password: "doesnotmatter",
		Provider: &provider,
	})
	r401, ok := resp.(openapi.Login401JSONResponse)
	if !ok {
		t.Fatalf("expected Login401JSONResponse, got %T", resp)
	}
	if r401.Error != "invalid credentials" {
		t.Errorf("body.Error = %q, want canonical anti-enumeration string %q",
			r401.Error, "invalid credentials")
	}
}

// A registered provider that doesn't support the password form
// (SAML, OIDC) must also return the canonical 401 shape — not 405
// "method not allowed" or 400 "not supported". 405 + 400 would leak
// the existence of the provider via response code.
func TestLogin_RedirectFlowProviderReturns401WithCanonicalBody(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&stubProvider{
		name:             "password",
		displayName:      "Password",
		kind:             KindPassword,
		supportsPassword: true,
	}, nil)
	reg.MustRegister(&stubProvider{
		name:             "saml-okta",
		displayName:      "Okta",
		kind:             KindSAML,
		supportsPassword: false,
	}, licenseStub{"sso_saml": true})
	h := dispatchTestHandler(reg)

	provider := "saml-okta"
	resp := mustLogin(t, h, &openapi.LoginJSONRequestBody{
		Username: "alice",
		Password: "doesnotmatter",
		Provider: &provider,
	})
	r401, ok := resp.(openapi.Login401JSONResponse)
	if !ok {
		t.Fatalf("expected Login401JSONResponse, got %T", resp)
	}
	if r401.Error != "invalid credentials" {
		t.Errorf("body.Error = %q, want canonical %q", r401.Error, "invalid credentials")
	}
}

// A provider that returns ErrProviderUnimplemented (stub LDAP) maps
// to the new 501 response — distinct from 401. This is the path that
// lets the admin UI render "build pending" instead of "wrong
// password" when the license has the feature but the binary doesn't
// yet ship the impl.
func TestLogin_UnimplementedProviderReturns501(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&stubProvider{
		name:             "password",
		displayName:      "Password",
		kind:             KindPassword,
		supportsPassword: true,
	}, nil)
	reg.MustRegister(&stubProvider{
		name:             "ldap",
		displayName:      "LDAP",
		kind:             KindLDAP,
		supportsPassword: true,
		authErr:          ErrProviderUnimplemented,
	}, licenseStub{"sso_ldap": true})
	h := dispatchTestHandler(reg)

	provider := "ldap"
	resp := mustLogin(t, h, &openapi.LoginJSONRequestBody{
		Username: "alice",
		Password: "doesnotmatter",
		Provider: &provider,
	})
	if _, ok := resp.(openapi.Login501JSONResponse); !ok {
		t.Fatalf("expected Login501JSONResponse for unimplemented stub, got %T", resp)
	}
}

// trackingProvider satisfies IdentityProvider and records the
// last username it was asked to authenticate. Lets a test prove
// "the dispatcher routed to THIS provider" without mocking the type.
type trackingProvider struct {
	name           string
	supportsPasswd bool
	lastUsername   string
	called         bool
	returnErr      error
}

func (p *trackingProvider) Name() string                   { return p.name }
func (p *trackingProvider) DisplayName() string            { return p.name }
func (p *trackingProvider) Kind() ProviderKind             { return KindPassword }
func (p *trackingProvider) RequiredLicenseFeature() string { return "" }
func (p *trackingProvider) SupportsPassword() bool         { return p.supportsPasswd }
func (p *trackingProvider) Authenticate(_ context.Context, username, _ string) (AuthResult, error) {
	p.called = true
	p.lastUsername = username
	return AuthResult{}, p.returnErr
}

// Provider name omitted (the back-compat case) defaults to
// "password". This pins the "omit provider → use password" behaviour
// so a future refactor that defaults to something else (or requires
// the field) would fail.
func TestLogin_OmittedProviderDefaultsToPassword(t *testing.T) {
	pw := &trackingProvider{name: "password", supportsPasswd: true, returnErr: ErrInvalidCredentials}
	other := &trackingProvider{name: "other", supportsPasswd: true, returnErr: ErrInvalidCredentials}
	reg := NewRegistry()
	reg.MustRegister(pw, nil)
	reg.MustRegister(other, nil)
	h := dispatchTestHandler(reg)

	mustLogin(t, h, &openapi.LoginJSONRequestBody{
		Username: "alice",
		Password: "x",
	})

	if !pw.called {
		t.Error("password provider must be called when provider field omitted")
	}
	if other.called {
		t.Error("non-default provider must not be called when provider field omitted")
	}
	if pw.lastUsername != "alice" {
		t.Errorf("password.lastUsername = %q, want alice", pw.lastUsername)
	}
}

// Provider field with whitespace-only string falls back to
// "password" too — anti-foot-gun. The empty-string trim happens in
// the dispatch path.
func TestLogin_WhitespaceProviderDefaultsToPassword(t *testing.T) {
	pw := &trackingProvider{name: "password", supportsPasswd: true, returnErr: ErrInvalidCredentials}
	reg := NewRegistry()
	reg.MustRegister(pw, nil)
	h := dispatchTestHandler(reg)

	provider := "   "
	mustLogin(t, h, &openapi.LoginJSONRequestBody{
		Username: "alice",
		Password: "x",
		Provider: &provider,
	})

	if !pw.called {
		t.Error("whitespace-only provider field must fall through to password default")
	}
}
