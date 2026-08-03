// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// In-package tests for the AuthConfig <-> openapi.AuthConfig
// converters (#712). No database — these are the pure translation
// layer, and a dropped field here is invisible everywhere else: the
// endpoint answers 200 with a body that looks fine, and the setting
// just never changes.
//
// That is exactly what had happened to self_registration. The admin
// auth page has shipped the three controls since 1.19.C and posts
// them, but authToAPI never returned the block and apiToAuth never
// read it, so the checkbox reverted on every reload and
// auth.self_registration.enabled could only be set by writing
// system_config by hand — which is why /register could not be opened
// at all.

package sysconfig

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func TestAuthConverters_SelfRegistrationRoundTrips(t *testing.T) {
	in := AuthConfig{
		SelfRegistration: SelfRegistrationConfig{
			Enabled:                  true,
			RequireEmailVerification: false,
			DefaultRole:              "Newcomer",
		},
	}

	api := authToAPI(in)
	if api.SelfRegistration == nil {
		t.Fatal("authToAPI dropped self_registration entirely")
	}
	if api.SelfRegistration.Enabled == nil || !*api.SelfRegistration.Enabled {
		t.Error("authToAPI lost self_registration.enabled")
	}
	if api.SelfRegistration.RequireEmailVerification == nil || *api.SelfRegistration.RequireEmailVerification {
		t.Error("authToAPI lost self_registration.require_email_verification")
	}
	if api.SelfRegistration.DefaultRole == nil || *api.SelfRegistration.DefaultRole != "Newcomer" {
		t.Error("authToAPI lost self_registration.default_role")
	}

	back := apiToAuth(api, AuthConfig{})
	if back.SelfRegistration != in.SelfRegistration {
		t.Errorf("round trip changed self_registration: %+v -> %+v", in.SelfRegistration, back.SelfRegistration)
	}
}

func TestApiToAuth_SelfRegistrationDefaultsClosed(t *testing.T) {
	// Absent block: full-replace semantics, and the safe direction for
	// a switch that opens an install to strangers is closed.
	if got := apiToAuth(openapi.AuthConfig{}, AuthConfig{}); got.SelfRegistration.Enabled {
		t.Error("an omitted self_registration block must read as disabled")
	}

	// Present but partial: only the fields the caller sent move.
	enabled := true
	partial := openapi.AuthConfig{}
	partial.SelfRegistration = &struct {
		DefaultRole              *string `json:"default_role,omitempty"`
		Enabled                  *bool   `json:"enabled,omitempty"`
		RequireEmailVerification *bool   `json:"require_email_verification,omitempty"`
	}{Enabled: &enabled}

	got := apiToAuth(partial, AuthConfig{})
	if !got.SelfRegistration.Enabled {
		t.Error("self_registration.enabled=true was not applied")
	}
	if got.SelfRegistration.DefaultRole != "" {
		t.Errorf("default_role invented a value: %q", got.SelfRegistration.DefaultRole)
	}
}

// ---------------------------------------------------------------------------
// #718 — SSO provider secrets are write-only, plus the merge that makes
// that survivable
// ---------------------------------------------------------------------------

const (
	ssoClientSecret = "oauth-client-secret-718"
	ssoBindPassword = "ldap-bind-password-718"
	ssoPrivateKey   = "-----BEGIN PRIVATE KEY-----saml-sp-718"
	ssoProviderID   = "33333333-3333-4333-8333-333333333718"
)

// One provider carrying all three secrets at once. A real install
// wouldn't mix LDAP and SAML fields on a `google` provider, but the
// converters are kind-agnostic and this makes every branch assertable
// in one pass.
func ssoStored() AuthConfig {
	return AuthConfig{SSOProviders: []SSOProvider{{
		ID:          ssoProviderID,
		Kind:        SSOKindGoogle,
		Enabled:     true,
		DisplayName: "Corp SSO",
		Config: SSOProviderConfig{
			ClientID:         "client-id-is-public",
			ClientSecret:     ssoClientSecret,
			RedirectURI:      "https://example.org/auth/callback",
			Scopes:           []string{"openid", "email"},
			ServerURL:        "ldaps://ldap.example.org:636",
			BaseDN:           "ou=people,dc=example,dc=org",
			BindDN:           "cn=svc,dc=example,dc=org",
			BindPassword:     ssoBindPassword,
			UserSearchFilter: "(uid=%s)",
			IDPMetadataURL:   "https://idp.example.org/metadata",
			IDPCertificate:   "-----BEGIN CERTIFICATE-----public",
			SPEntityID:       "https://example.org/saml",
			SPPrivateKey:     ssoPrivateKey,
		},
	}}}
}

// The disclosure half: authToAPI returns the non-secret config in full
// and the three credentials not at all — omitted, not blanked, with a
// *_set boolean so the UI can still tell "unset" from "on file".
func TestAuthToAPI_OmitsProviderSecrets(t *testing.T) {
	api := authToAPI(ssoStored())
	if len(api.SsoProviders) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(api.SsoProviders))
	}
	cfg := api.SsoProviders[0].Config
	if cfg == nil {
		t.Fatal("config dropped entirely — editing a provider must not become a retype-everything")
	}

	if cfg.ClientSecret != nil {
		t.Errorf("client_secret present in the response: %q", *cfg.ClientSecret)
	}
	if cfg.BindPassword != nil {
		t.Errorf("bind_password present in the response: %q", *cfg.BindPassword)
	}
	if cfg.SpPrivateKey != nil {
		t.Errorf("sp_private_key present in the response: %q", *cfg.SpPrivateKey)
	}

	for name, got := range map[string]*bool{
		"client_secret_set":  cfg.ClientSecretSet,
		"bind_password_set":  cfg.BindPasswordSet,
		"sp_private_key_set": cfg.SpPrivateKeySet,
	} {
		if got == nil || !*got {
			t.Errorf("%s should be true when a secret is stored: %v", name, got)
		}
	}

	// Field-level redaction, not row-level: everything an admin would
	// otherwise have to retype still comes back — including the two
	// look-alikes that are deliberately NOT secrets.
	if cfg.ClientId == nil || *cfg.ClientId != "client-id-is-public" {
		t.Errorf("client_id dropped: %v", cfg.ClientId)
	}
	if cfg.BindDn == nil || *cfg.BindDn != "cn=svc,dc=example,dc=org" {
		t.Errorf("bind_dn is an identifier, not a credential, and must be returned: %v", cfg.BindDn)
	}
	if cfg.IdpCertificate == nil || *cfg.IdpCertificate != "-----BEGIN CERTIFICATE-----public" {
		t.Errorf("idp_certificate is public material and must be returned: %v", cfg.IdpCertificate)
	}
	if cfg.BaseDn == nil || cfg.ServerUrl == nil || cfg.UserSearchFilter == nil ||
		cfg.IdpMetadataUrl == nil || cfg.SpEntityId == nil || cfg.RedirectUri == nil ||
		cfg.Scopes == nil {
		t.Errorf("non-secret config fields dropped: %+v", cfg)
	}
}

// *_set is false — not absent — when nothing is stored.
func TestAuthToAPI_SecretSetFalseWhenUnset(t *testing.T) {
	in := AuthConfig{SSOProviders: []SSOProvider{{
		ID: ssoProviderID, Kind: SSOKindGithub, DisplayName: "Bare",
	}}}
	cfg := authToAPI(in).SsoProviders[0].Config
	if cfg == nil {
		t.Fatal("config missing")
	}
	for name, got := range map[string]*bool{
		"client_secret_set":  cfg.ClientSecretSet,
		"bind_password_set":  cfg.BindPasswordSet,
		"sp_private_key_set": cfg.SpPrivateKeySet,
	} {
		if got == nil {
			t.Errorf("%s absent — the UI cannot tell unset from on-file", name)
		} else if *got {
			t.Errorf("%s true with nothing stored", name)
		}
	}
}

// THE data-loss guard, and the reason this fix is a merge and not just
// a redaction. The round trip is literally what the admin page does:
// load, edit one unrelated field, PATCH the whole provider list back.
// The secrets are not in that body — the read no longer returns them —
// so if "absent" meant "clear it", one display-name edit would destroy
// every stored SSO credential (#708's shape).
func TestAuthConverters_RoundTripKeepsSecretsTheBodyOmits(t *testing.T) {
	stored := ssoStored()
	body := authToAPI(stored)
	body.SsoProviders[0].DisplayName = "Corp SSO (renamed)"

	got := apiToAuth(body, stored)
	if len(got.SSOProviders) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(got.SSOProviders))
	}
	p := got.SSOProviders[0]
	if p.ID != ssoProviderID {
		t.Fatalf("provider id changed: %q", p.ID)
	}
	if p.DisplayName != "Corp SSO (renamed)" {
		t.Errorf("the non-secret edit did not apply: %q", p.DisplayName)
	}
	if p.Config.ClientSecret != ssoClientSecret {
		t.Errorf("round trip wiped client_secret: %q", p.Config.ClientSecret)
	}
	if p.Config.BindPassword != ssoBindPassword {
		t.Errorf("round trip wiped bind_password: %q", p.Config.BindPassword)
	}
	if p.Config.SPPrivateKey != ssoPrivateKey {
		t.Errorf("round trip wiped sp_private_key: %q", p.Config.SPPrivateKey)
	}
	// The non-secret half has to survive too, or the merge is hiding a
	// different flavour of the same data loss.
	if p.Config.ClientID != "client-id-is-public" || p.Config.BindDN != "cn=svc,dc=example,dc=org" ||
		p.Config.IDPCertificate != "-----BEGIN CERTIFICATE-----public" ||
		len(p.Config.Scopes) != 2 {
		t.Errorf("round trip lost non-secret config: %+v", p.Config)
	}
}

// An explicitly empty string means "keep", same as absent — that is
// what a form binding a blank input posts.
func TestApiToAuth_EmptySecretKeepsStoredValue(t *testing.T) {
	stored := ssoStored()
	body := authToAPI(stored)
	blank := ""
	body.SsoProviders[0].Config.ClientSecret = &blank
	body.SsoProviders[0].Config.BindPassword = &blank
	body.SsoProviders[0].Config.SpPrivateKey = &blank

	p := apiToAuth(body, stored).SSOProviders[0]
	if p.Config.ClientSecret != ssoClientSecret || p.Config.BindPassword != ssoBindPassword ||
		p.Config.SPPrivateKey != ssoPrivateKey {
		t.Errorf("empty-string secrets wiped the stored values: %+v", p.Config)
	}
}

// The merge must not become "ignore the field", or rotation is
// impossible.
func TestApiToAuth_SuppliedSecretRotates(t *testing.T) {
	stored := ssoStored()
	body := authToAPI(stored)
	rotated := "rotated-client-secret"
	body.SsoProviders[0].Config.ClientSecret = &rotated

	p := apiToAuth(body, stored).SSOProviders[0]
	if p.Config.ClientSecret != rotated {
		t.Errorf("rotation did not apply: %q", p.Config.ClientSecret)
	}
	if p.Config.BindPassword != ssoBindPassword {
		t.Errorf("rotating one secret disturbed another: %q", p.Config.BindPassword)
	}
}

// A provider added in the same save has no ID yet, so there is nothing
// to merge — and it must not inherit the existing provider's secrets.
func TestApiToAuth_NewProviderInheritsNothing(t *testing.T) {
	stored := ssoStored()
	body := authToAPI(stored)
	body.SsoProviders = append(body.SsoProviders, body.SsoProviders[0])
	body.SsoProviders[1].Id = nil
	body.SsoProviders[1].DisplayName = "Second"

	got := apiToAuth(body, stored)
	if len(got.SSOProviders) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(got.SSOProviders))
	}
	if got.SSOProviders[0].Config.ClientSecret != ssoClientSecret {
		t.Errorf("existing provider lost its secret: %q", got.SSOProviders[0].Config.ClientSecret)
	}
	fresh := got.SSOProviders[1].Config
	if fresh.ClientSecret != "" || fresh.BindPassword != "" || fresh.SPPrivateKey != "" {
		t.Errorf("new provider inherited secrets it was never given: %+v", fresh)
	}
	if got.SSOProviders[1].ID == "" || got.SSOProviders[1].ID == ssoProviderID {
		t.Errorf("new provider should get a fresh id, got %q", got.SSOProviders[1].ID)
	}
}
