// Package ldapauth is the LDAP / Active Directory identity-provider
// surface. Enterprise-gated on license feature "sso_ldap".
//
// This phase (1.17.P-foundation) ships the package shape + provider
// stub so the registry + boot wiring + admin UI all exist end-to-end.
// The actual bind/search logic against an LDAP server lands in the
// follow-on phase that takes plugins/simpleldap (and the user's
// improved Reference_Plugins/simpleldap fork) as design references —
// clean-room reimplementation, no copied code.
//
// What the stub does today:
//   - Satisfies auth.IdentityProvider so the registry happily holds it.
//   - Reports RequiredLicenseFeature("sso_ldap") so the registry
//     refuses to attach it on a Community install.
//   - Authenticate returns auth.ErrProviderUnimplemented so an admin
//     who configures it sees "licensed but not yet implemented"
//     rather than a 401 that would suggest credentials are wrong.
//
// Why a separate package (not auth/provider_ldap.go): when the real
// LDAP bind code lands it will pull in a forked go-ldap dependency
// (per the dep-fork audit memory). Keeping the boundary clean now
// means the auth package never grows transitive LDAP imports on
// Community builds.

package ldapauth

import (
	"context"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// LicenseFeature is the feature flag string a license must include
// for the LDAP provider to register. Mirrored in the embedded
// community feature set as ABSENT — community installs cannot
// register this provider, even with a hand-edited config.
const LicenseFeature = "sso_ldap"

// Provider is the stub LDAP IdentityProvider. Real bind/search
// connection state lives here in the impl phase; the foundation just
// carries Name/DisplayName so the registry + login UI render the
// provider correctly.
type Provider struct {
	name        string
	displayName string
}

// New builds a provider with a registry-unique name (so an install
// can register multiple LDAP servers later: "ldap-eng", "ldap-art")
// and the human-readable label shown on the login screen.
func New(name, displayName string) *Provider {
	if name == "" {
		name = "ldap"
	}
	if displayName == "" {
		displayName = "LDAP"
	}
	return &Provider{name: name, displayName: displayName}
}

// Name implements auth.IdentityProvider.
func (p *Provider) Name() string { return p.name }

// DisplayName implements auth.IdentityProvider.
func (p *Provider) DisplayName() string { return p.displayName }

// Kind implements auth.IdentityProvider.
func (*Provider) Kind() auth.ProviderKind { return auth.KindLDAP }

// RequiredLicenseFeature implements auth.IdentityProvider.
func (*Provider) RequiredLicenseFeature() string { return LicenseFeature }

// SupportsPassword implements auth.IdentityProvider. LDAP authenticates
// via the same username/password POST as the built-in provider — the
// handler runs the bind on the server's behalf.
func (*Provider) SupportsPassword() bool { return true }

// Authenticate implements auth.IdentityProvider. Stub until the impl
// phase wires real bind/search; returns ErrProviderUnimplemented so
// the login handler emits 501 "licensed but not yet implemented"
// rather than 401 "bad credentials".
func (*Provider) Authenticate(_ context.Context, _, _ string) (auth.AuthResult, error) {
	return auth.AuthResult{}, auth.ErrProviderUnimplemented
}

// Compile-time interface check.
var _ auth.IdentityProvider = (*Provider)(nil)
