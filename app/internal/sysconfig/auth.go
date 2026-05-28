package sysconfig

import (
	"context"
	"fmt"
)

// KeyAuth — system_config key for the authentication / SSO settings
// (password policy + 3rd-party identity providers). Read/written via
// the Store's getter and setter below.
const KeyAuth = "auth"

// SSOProviderKind enumerates the supported single-sign-on identity
// kinds. Per-kind configuration lives in SSOProvider.Config as a free
// JSON blob — concrete shapes are validated by the integration code
// that actually performs the login, not here. We're a settings store.
type SSOProviderKind string

const (
	SSOKindLDAP   SSOProviderKind = "ldap"
	SSOKindSAML   SSOProviderKind = "saml"
	SSOKindGoogle SSOProviderKind = "google"
	SSOKindGithub SSOProviderKind = "github"
	SSOKindX      SSOProviderKind = "x"
)

func validSSOKind(k SSOProviderKind) bool {
	switch k {
	case SSOKindLDAP, SSOKindSAML, SSOKindGoogle, SSOKindGithub, SSOKindX:
		return true
	default:
		return false
	}
}

// SSOProvider is a single configured identity provider. Multiple
// providers of the same kind are allowed (e.g. two LDAP servers, or
// "GitHub for org A" + "GitHub for org B" with different OAuth
// clients).
type SSOProvider struct {
	// Stable id (uuid) chosen by the admin UI when the provider is
	// added. Used as the `state` parameter on OAuth redirects and
	// as the row key for editing. Empty on a freshly-added entry —
	// the handler fills it in.
	ID string `json:"id"`

	Kind        SSOProviderKind `json:"kind"`
	Enabled     bool            `json:"enabled"`
	DisplayName string          `json:"display_name"`

	// Per-kind opaque config. For LDAP: server URL, base DN, bind
	// creds, search filter. For SAML: IdP metadata URL, SP entity ID,
	// cert. For OAuth providers (Google/GitHub/X): client_id,
	// client_secret, redirect_uri, scopes. We don't strongly type
	// these here — the integration code in Phase 1.18 owns the
	// schemas and validates at use time.
	Config map[string]any `json:"config,omitempty"`
}

// PasswordPolicy is the local-account complexity policy. Applies only
// to passwords stored on `user.password`; SSO logins bypass it
// entirely.
type PasswordPolicy struct {
	MinLength       int  `json:"min_length"`
	RequireUpper    bool `json:"require_upper"`
	RequireNumber   bool `json:"require_number"`
	RequireSymbol   bool `json:"require_symbol"`
	DisallowCommon  bool `json:"disallow_common"` // reject "password", "123456", etc.
	MaxAgeDays      int  `json:"max_age_days"`    // 0 = no expiry
}

// AuthConfig is the full auth settings payload stored under KeyAuth.
type AuthConfig struct {
	PasswordPolicy PasswordPolicy `json:"password_policy"`
	SSOProviders   []SSOProvider  `json:"sso_providers"`
}

// GetAuth returns the auth config or, if unset, an empty AuthConfig
// (zero-valued PasswordPolicy + nil SSO providers).
func (s *Store) GetAuth(ctx context.Context) (AuthConfig, error) {
	var out AuthConfig
	if err := s.getKey(ctx, KeyAuth, &out); err != nil {
		return AuthConfig{}, err
	}
	return out, nil
}

// SetAuth validates and writes the auth config.
func (s *Store) SetAuth(ctx context.Context, v AuthConfig) error {
	if v.PasswordPolicy.MinLength < 0 || v.PasswordPolicy.MinLength > 256 {
		return fmt.Errorf("sysconfig: password min_length must be 0..256, got %d", v.PasswordPolicy.MinLength)
	}
	if v.PasswordPolicy.MaxAgeDays < 0 || v.PasswordPolicy.MaxAgeDays > 36500 {
		return fmt.Errorf("sysconfig: password max_age_days must be 0..36500, got %d", v.PasswordPolicy.MaxAgeDays)
	}
	for i, p := range v.SSOProviders {
		if !validSSOKind(p.Kind) {
			return fmt.Errorf("sysconfig: sso_providers[%d]: unknown kind %q", i, p.Kind)
		}
		if p.DisplayName == "" {
			return fmt.Errorf("sysconfig: sso_providers[%d]: display_name is required", i)
		}
	}
	return s.setKey(ctx, KeyAuth, v)
}
