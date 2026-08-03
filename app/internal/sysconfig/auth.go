// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
// kinds. Per-kind configuration lives in SSOProvider.Config, a typed
// struct — see SSOProviderConfig for why it is not a free JSON blob
// any more.
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

	// Per-kind config. Which subset applies is decided by Kind.
	Config SSOProviderConfig `json:"config"`
}

// SSOProviderConfig is the typed per-kind provider configuration.
//
// This used to be a `map[string]any` and that was the bug (#718): the
// read path copied it into the response verbatim, so every stored LDAP
// bind password, OAuth client secret and SAML SP private key went to
// anyone holding `system.config.read` — a strictly weaker capability
// than the `system.auth.write` needed to set one.
//
// Naming every field is what makes the fix possible rather than a
// guess. The three secrets below are write-only: authToAPI zeroes them
// and reports a `*_set` boolean instead, and apiToAuth merges the
// stored value back in when the caller omits it. A free-form remainder
// would defeat that by construction — the server cannot know which
// unknown key holds a credential — which is why there isn't one. That
// is also why denylisting known secret names was rejected: it fails
// open on the next field somebody adds.
//
// Nothing reads these at login time yet; SSO enforcement is a later
// phase. Names follow the upstream protocol vocabulary (RFC 6749,
// RFC 4511, SAML 2.0) so the integration inherits them.
type SSOProviderConfig struct {
	// --- OAuth 2.0 / OIDC: Kind google | github | x ---

	// ClientID is public by definition — it travels in the
	// authorization URL — so it is returned on read.
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is SECRET. Never returned; see the type doc.
	ClientSecret string   `json:"client_secret,omitempty"`
	RedirectURI  string   `json:"redirect_uri,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`

	// --- LDAP: Kind ldap ---

	ServerURL string `json:"server_url,omitempty"`
	StartTLS  bool   `json:"start_tls,omitempty"`
	BaseDN    string `json:"base_dn,omitempty"`
	// BindDN is an identifier, not a credential — returned on read.
	BindDN string `json:"bind_dn,omitempty"`
	// BindPassword is SECRET. Never returned; see the type doc.
	BindPassword     string `json:"bind_password,omitempty"`
	UserSearchFilter string `json:"user_search_filter,omitempty"`

	// --- SAML 2.0: Kind saml ---

	IDPMetadataURL string `json:"idp_metadata_url,omitempty"`
	IDPEntityID    string `json:"idp_entity_id,omitempty"`
	// IDPCertificate is public material — the IdP publishes it in its
	// own metadata — so it is returned on read. The SP's private key
	// below is not.
	IDPCertificate string `json:"idp_certificate,omitempty"`
	SPEntityID     string `json:"sp_entity_id,omitempty"`
	SPACSURL       string `json:"sp_acs_url,omitempty"`
	// SPPrivateKey is SECRET. Never returned; see the type doc.
	SPPrivateKey string `json:"sp_private_key,omitempty"`
}

// PasswordPolicy is the local-account complexity policy. Applies only
// to passwords stored on `user.password`; SSO logins bypass it
// entirely.
type PasswordPolicy struct {
	MinLength      int  `json:"min_length"`
	RequireUpper   bool `json:"require_upper"`
	RequireNumber  bool `json:"require_number"`
	RequireSymbol  bool `json:"require_symbol"`
	DisallowCommon bool `json:"disallow_common"` // reject "password", "123456", etc.
	MaxAgeDays     int  `json:"max_age_days"`    // 0 = no expiry
}

// AuthConfig is the full auth settings payload stored under KeyAuth.
type AuthConfig struct {
	// PasswordPolicy field name matches the "password" sensitive
	// pattern in the Phase 1.17.D changeset helper and is stripped
	// from the diff automatically. Operators see "auth config
	// updated" and read the new policy via the API. Known MVP
	// limitation; addressable with a per-field audit:"include"
	// opt-in tag in a follow-up if the false positive matters.
	PasswordPolicy PasswordPolicy `json:"password_policy"`
	// SSOProviders carries per-provider SSOProviderConfig values
	// that may include an OAuth client_secret, an LDAP bind
	// password or a SAML SP private key. Stripped from the
	// changeset for the same reason as AIConfig.Providers — the
	// diff helper isn't slice-element-aware. Operators see "auth
	// config updated"; provider list edits are visible via the API.
	SSOProviders []SSOProvider `json:"sso_providers" audit:"-"`
	// SelfRegistration controls whether anonymous callers can
	// create their own accounts via /auth/register. Phase 1.19.C.
	// Default zero-valued (Enabled=false) — operators opt in.
	SelfRegistration SelfRegistrationConfig `json:"self_registration"`
	// Lockout is the persistent per-username lockout policy.
	// Phase 1.19.D. Composes with the in-process LoginLimiter
	// (memory-only rate limiter). Zero values fall back to
	// DefaultLockoutThreshold + DefaultLockoutDurationMinutes.
	Lockout LockoutPolicy `json:"lockout"`
}

// LockoutPolicy is the operator-tunable lockout config. Defaults
// match OWASP guidance for public-facing auth. Operators tune per
// their threat model.
type LockoutPolicy struct {
	// Threshold is the failed-attempt count that triggers lockout.
	// Zero disables lockout (relies on LoginLimiter alone; NOT
	// recommended for public installs). 1..1000.
	Threshold int32 `json:"threshold"`
	// DurationMinutes is how long the lockout persists once
	// triggered. Zero falls back to default; 1..1440 (24h max).
	DurationMinutes int32 `json:"duration_minutes"`
}

// DefaultLockoutThreshold + DefaultLockoutDurationMinutes match the
// lockout package's DefaultConfig. Kept in sync deliberately — the
// login handler wires a PolicyProvider that reads AuthConfig.Lockout
// and falls back here on zero values.
const (
	DefaultLockoutThreshold       int32 = 5
	DefaultLockoutDurationMinutes int32 = 15
)

// SelfRegistrationConfig is the operator-tunable knob set for the
// /auth/register surface. The endpoint refuses with 403 when
// Enabled is false, so the existence of /auth/register on a closed
// install is harmless.
type SelfRegistrationConfig struct {
	// Enabled is the master switch. Default false — admins must
	// opt-in explicitly; the surface is dormant otherwise.
	Enabled bool `json:"enabled"`

	// RequireEmailVerification refuses login until the user's
	// email is confirmed via the verification link. Default true
	// when self-registration is on; flipping it off should be a
	// deliberate choice (e.g. closed-network installs where
	// outbound SMTP isn't configured + admin-supplied invite is
	// the only signup path).
	RequireEmailVerification bool `json:"require_email_verification"`

	// DefaultRole is the seeded role name freshly-registered users
	// are assigned. Default "Base"; operators can point it at a
	// stricter role for moderated communities.
	DefaultRole string `json:"default_role"`
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
	if v.Lockout.Threshold < 0 || v.Lockout.Threshold > 1000 {
		return fmt.Errorf("sysconfig: lockout.threshold must be 0..1000, got %d", v.Lockout.Threshold)
	}
	if v.Lockout.DurationMinutes < 0 || v.Lockout.DurationMinutes > 1440 {
		return fmt.Errorf("sysconfig: lockout.duration_minutes must be 0..1440, got %d", v.Lockout.DurationMinutes)
	}
	return s.setKey(ctx, KeyAuth, v)
}
