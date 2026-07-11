// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"errors"
	"testing"
)

// Tests for the IdentityProvider registry. The license-gated
// Register branch is the load-bearing security property here:
// providers that require a feature must not register on a Community
// install, no matter how confused the boot wiring is.

// stubProvider lets each test build a small IdentityProvider without
// touching the real PasswordProvider / LDAP / SAML packages.
type stubProvider struct {
	name             string
	displayName      string
	kind             ProviderKind
	requiredFeature  string
	supportsPassword bool
	authErr          error
	authResult       AuthResult
}

func (p *stubProvider) Name() string                   { return p.name }
func (p *stubProvider) DisplayName() string            { return p.displayName }
func (p *stubProvider) Kind() ProviderKind             { return p.kind }
func (p *stubProvider) RequiredLicenseFeature() string { return p.requiredFeature }
func (p *stubProvider) SupportsPassword() bool         { return p.supportsPassword }
func (p *stubProvider) Authenticate(_ context.Context, _, _ string) (AuthResult, error) {
	return p.authResult, p.authErr
}

// stubSource lets a test pretend the license includes (or excludes)
// any feature set.
type stubSource map[string]bool

func (s stubSource) HasFeature(name string) bool { return s[name] }

// A community-tier provider (no required feature) always registers,
// even with a nil source.
func TestRegistry_CommunityProviderRegistersWithNilSource(t *testing.T) {
	r := NewRegistry()
	p := &stubProvider{name: "password", displayName: "Password", kind: KindPassword}
	if err := r.Register(p, nil); err != nil {
		t.Fatalf("unexpected register error: %v", err)
	}
	if got := r.List(); len(got) != 1 || got[0].Name() != "password" {
		t.Fatalf("List = %+v, want [password]", got)
	}
}

// An enterprise provider WITHOUT the license feature must NOT
// register, and the error message must name the missing feature so
// the boot log + operator immediately see why.
func TestRegistry_EnterpriseProviderRejectedWithoutFeature(t *testing.T) {
	r := NewRegistry()
	p := &stubProvider{name: "ldap", requiredFeature: "sso_ldap"}
	err := r.Register(p, stubSource{})
	if err == nil {
		t.Fatal("expected error for missing feature, got nil")
	}
	if r.Has("ldap") {
		t.Fatal("provider must not be in registry when its license feature is absent")
	}
}

// Same provider WITH the feature registers cleanly.
func TestRegistry_EnterpriseProviderAcceptedWithFeature(t *testing.T) {
	r := NewRegistry()
	p := &stubProvider{name: "ldap", requiredFeature: "sso_ldap"}
	if err := r.Register(p, stubSource{"sso_ldap": true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Has("ldap") {
		t.Fatal("provider must be registered")
	}
}

// A nil LicenseSource passed alongside an enterprise provider is the
// same as "no features" — refuse to register. This guards the case
// where boot wiring forgets to thread the license through.
func TestRegistry_EnterpriseProviderRejectedWithNilSource(t *testing.T) {
	r := NewRegistry()
	p := &stubProvider{name: "saml", requiredFeature: "sso_saml"}
	if err := r.Register(p, nil); err == nil {
		t.Fatal("expected error when source is nil for enterprise provider")
	}
}

// Duplicate names must not silently overwrite — the registry has to
// surface the conflict so the operator sees the misconfig at boot.
func TestRegistry_DuplicateNameRejected(t *testing.T) {
	r := NewRegistry()
	p1 := &stubProvider{name: "ldap-engineering", requiredFeature: "sso_ldap"}
	p2 := &stubProvider{name: "ldap-engineering", requiredFeature: "sso_ldap"}
	src := stubSource{"sso_ldap": true}
	if err := r.Register(p1, src); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(p2, src); err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
}

// List returns providers sorted by name so the login UI render is
// stable across restarts.
func TestRegistry_ListSortedByName(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubProvider{name: "saml-okta", requiredFeature: "sso_saml"}, stubSource{"sso_saml": true})
	r.Register(&stubProvider{name: "ldap"}, nil)
	r.Register(&stubProvider{name: "password"}, nil)
	got := r.List()
	want := []string{"ldap", "password", "saml-okta"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, p := range got {
		if p.Name() != want[i] {
			t.Errorf("[%d] = %s, want %s", i, p.Name(), want[i])
		}
	}
}

// Get returns false for unknown providers — the login handler relies
// on this for its "unknown provider → 401" path.
func TestRegistry_GetMissingReturnsFalse(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("nope"); ok {
		t.Fatal("expected (nil, false) for missing provider")
	}
}

// A nil provider passed to Register is a programming error and must
// fail loudly rather than silently no-op.
func TestRegistry_NilProviderRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil, nil); err == nil {
		t.Fatal("expected error for nil provider")
	}
}

// A provider with an empty Name() is misconfigured.
func TestRegistry_EmptyNameRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&stubProvider{name: ""}, nil); err == nil {
		t.Fatal("expected error for empty name")
	}
}

// ErrProviderUnimplemented is a recognisable sentinel — important
// because the login handler maps it to a distinct 501 response. If a
// rename ever loses the "this is the stub path" property, the
// handler's switch would silently fall through to 500.
func TestErrProviderUnimplemented_IsSentinel(t *testing.T) {
	wrapped := errors.Join(ErrProviderUnimplemented, errors.New("extra context"))
	if !errors.Is(wrapped, ErrProviderUnimplemented) {
		t.Fatal("errors.Is must recognise wrapped ErrProviderUnimplemented")
	}
}

// Replace atomically swaps the registry's provider set. Pinning the
// happy path: a Community-mode registry with only "password" rebuilds
// into an enterprise-mode registry that also has "ldap" after a
// hypothetical license upload. This is the hot-swap path that lets
// admins upgrade from community → enterprise without restarting the
// process.
func TestRegistry_ReplaceSwapsProviderSet(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubProvider{name: "password", kind: KindPassword, supportsPassword: true}, nil)
	if _, ok := r.Get("ldap"); ok {
		t.Fatal("precondition: ldap must not be registered initially")
	}
	err := r.Replace([]IdentityProvider{
		&stubProvider{name: "password", kind: KindPassword, supportsPassword: true},
		&stubProvider{name: "ldap", kind: KindLDAP, requiredFeature: "sso_ldap", supportsPassword: true},
	}, licenseStub{"sso_ldap": true})
	if err != nil {
		t.Fatalf("Replace returned err: %v", err)
	}
	if _, ok := r.Get("ldap"); !ok {
		t.Fatal("ldap must be registered after Replace")
	}
	if _, ok := r.Get("password"); !ok {
		t.Fatal("password must still be registered after Replace")
	}
}

// The reverse direction: downgrade from enterprise → community
// (license expired or removed) must atomically drop the LDAP slot so
// subsequent /auth/login requests with provider="ldap" return the
// canonical 401 anti-enumeration response.
func TestRegistry_ReplaceCanShrinkRegistry(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubProvider{name: "password", kind: KindPassword, supportsPassword: true}, nil)
	r.MustRegister(&stubProvider{name: "ldap", kind: KindLDAP, requiredFeature: "sso_ldap", supportsPassword: true},
		licenseStub{"sso_ldap": true})

	// Now "lose" the license — replace with password only.
	err := r.Replace([]IdentityProvider{
		&stubProvider{name: "password", kind: KindPassword, supportsPassword: true},
	}, licenseStub{}) // no features
	if err != nil {
		t.Fatalf("Replace returned err: %v", err)
	}
	if _, ok := r.Get("ldap"); ok {
		t.Fatal("ldap must be removed after license downgrade")
	}
	if _, ok := r.Get("password"); !ok {
		t.Fatal("password must survive license downgrade")
	}
}

// Replace must re-validate license features against the new source.
// A test caller that tries to swap in a sso_ldap-gated provider
// without the feature gets the swap REJECTED entirely — the previous
// registry stays intact, no partial update.
func TestRegistry_ReplaceRejectsUnlicensedProvider(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubProvider{name: "password", kind: KindPassword, supportsPassword: true}, nil)

	err := r.Replace([]IdentityProvider{
		&stubProvider{name: "password", kind: KindPassword, supportsPassword: true},
		&stubProvider{name: "ldap", kind: KindLDAP, requiredFeature: "sso_ldap", supportsPassword: true},
	}, licenseStub{}) // no sso_ldap
	if err == nil {
		t.Fatal("Replace must reject unlicensed enterprise provider")
	}
	// Previous registry stays intact.
	if _, ok := r.Get("password"); !ok {
		t.Fatal("failed Replace must leave previous registry intact")
	}
	if _, ok := r.Get("ldap"); ok {
		t.Fatal("failed Replace must NOT partially add the rejected provider")
	}
}

// Replace with a duplicate-name list fails atomically — same
// failure-doesn't-corrupt property as the unlicensed case.
func TestRegistry_ReplaceRejectsDuplicateNames(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubProvider{name: "password", kind: KindPassword, supportsPassword: true}, nil)

	err := r.Replace([]IdentityProvider{
		&stubProvider{name: "password", kind: KindPassword, supportsPassword: true},
		&stubProvider{name: "password", kind: KindPassword, supportsPassword: true},
	}, nil)
	if err == nil {
		t.Fatal("expected error for duplicate provider names in Replace list")
	}
	if len(r.List()) != 1 {
		t.Fatalf("previous registry must stay intact, got %d providers", len(r.List()))
	}
}
