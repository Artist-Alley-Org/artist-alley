// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package samlauth is the SAML 2.0 SP-initiated SSO surface.
// Enterprise-gated on license feature "sso_saml".
//
// This phase ships the foundation only: the provider stub + an
// interface for the redirect flow handlers the real impl will
// register. SAML's protocol is fundamentally redirect-based (the
// browser bounces between SP and IdP via signed POSTs), so unlike
// password/LDAP it can't reuse POST /auth/login — it needs:
//
//   - GET  /auth/saml/{name}/login   begin the SP-initiated flow
//   - POST /auth/saml/{name}/acs     assertion consumer service
//   - GET  /auth/saml/{name}/metadata SP metadata XML
//
// Those routes are mounted by http/server.go IFF this provider is
// registered, which only happens when the install holds sso_saml.
// On Community installs the routes do not exist server-side at all
// — a request to /auth/saml/* gets a chi 404, not "license required".
// That's the "no-route" defense-in-depth pattern.
//
// Same clean-room rule as ldapauth: plugins/simplesaml is a reference
// blueprint; the real impl is original code.

package samlauth

import (
	"context"
	"net/http"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// LicenseFeature is the feature flag string a license must include
// for the SAML provider to register.
const LicenseFeature = "sso_saml"

// Provider is the stub SAML IdentityProvider. Real metadata + IdP
// trust state lives here in the impl phase.
type Provider struct {
	name        string
	displayName string
}

// New builds a provider with a registry-unique name + login-screen
// label. Multiple SAML IdPs can be registered by giving each a unique
// name ("saml-okta", "saml-google") — the route mux uses the name as
// a path segment.
func New(name, displayName string) *Provider {
	if name == "" {
		name = "saml"
	}
	if displayName == "" {
		displayName = "SAML"
	}
	return &Provider{name: name, displayName: displayName}
}

// Name implements auth.IdentityProvider.
func (p *Provider) Name() string { return p.name }

// DisplayName implements auth.IdentityProvider.
func (p *Provider) DisplayName() string { return p.displayName }

// Kind implements auth.IdentityProvider.
func (*Provider) Kind() auth.ProviderKind { return auth.KindSAML }

// RequiredLicenseFeature implements auth.IdentityProvider.
func (*Provider) RequiredLicenseFeature() string { return LicenseFeature }

// SupportsPassword implements auth.IdentityProvider. SAML uses
// redirect flows — POSTing password to /auth/login with
// provider="saml" is a misconfigured client and gets a 405.
func (*Provider) SupportsPassword() bool { return false }

// Authenticate implements auth.IdentityProvider. Always returns
// ErrProviderUnsupportedMethod — SAML clients are routed via the
// redirect endpoints, not the password POST. Documenting it here as
// a hard error rather than a silent no-op so a misconfigured client
// (e.g. a script POSTing provider="saml-okta") sees an explicit
// rejection instead of a 401.
func (*Provider) Authenticate(_ context.Context, _, _ string) (auth.AuthResult, error) {
	return auth.AuthResult{}, auth.ErrProviderUnsupportedMethod
}

// Compile-time interface check.
var _ auth.IdentityProvider = (*Provider)(nil)

// RedirectFlowHandler is the extra interface SAML providers satisfy
// for the kind-specific HTTP routes the boot wiring mounts. The
// foundation stubs all three to 501 so the routes exist + can be
// smoke-tested end-to-end; the real impl swaps these to real
// metadata/AuthnRequest/AssertionConsumer logic.
type RedirectFlowHandler interface {
	BeginLogin(w http.ResponseWriter, r *http.Request)
	ConsumeAssertion(w http.ResponseWriter, r *http.Request)
	Metadata(w http.ResponseWriter, r *http.Request)
}

// BeginLogin implements RedirectFlowHandler — stub.
func (*Provider) BeginLogin(w http.ResponseWriter, _ *http.Request) {
	stubResponse(w, "saml_begin_login")
}

// ConsumeAssertion implements RedirectFlowHandler — stub.
func (*Provider) ConsumeAssertion(w http.ResponseWriter, _ *http.Request) {
	stubResponse(w, "saml_consume_assertion")
}

// Metadata implements RedirectFlowHandler — stub.
func (*Provider) Metadata(w http.ResponseWriter, _ *http.Request) {
	stubResponse(w, "saml_metadata")
}

// stubResponse writes a 501 with a stable JSON body so the admin UI
// can render a "licensed but not yet implemented" badge without
// having to parse the message string.
func stubResponse(w http.ResponseWriter, op string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"error":"not_implemented","op":"` + op + `","phase":"1.17.P-foundation"}`))
}

// Compile-time interface check for the redirect-flow shape.
var _ RedirectFlowHandler = (*Provider)(nil)
