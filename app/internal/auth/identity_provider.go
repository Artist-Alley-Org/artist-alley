package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Identity-provider plumbing (Phase 1.17.P-foundation).
//
// Authentication backends — password (built-in), LDAP, SAML, OIDC,
// etc. — each satisfy IdentityProvider and register themselves with a
// process-wide Registry at boot. The login HTTP handler dispatches
// against the registry rather than hard-coding password as the only
// path. Provider construction is gated on the install's license:
// enterprise providers (sso_ldap, sso_saml) only get registered when
// licensing.Source.HasFeature reports the matching feature flag, so
// an unlicensed install has literally no SSO surface — no provider in
// the registry, no route in the router (the SAML callback routes
// aren't even mounted), no DB tables for SSO config.
//
// This is the "construction-site gate" pattern: a runtime patch that
// flips HasFeature to true gets nothing back, because the wiring
// block never ran. The cap-feature bridge from 1.17.O-2 is the
// independent second wall — even a forged provider can't be
// configured by admins without bypassing the capability gate on
// `system.sso.ldap.write` / `system.sso.saml.write`.

// ProviderKind enumerates the kinds of identity providers the login
// surface knows how to render and dispatch to. Stable JSON enum
// values — these end up in /auth/providers responses.
type ProviderKind string

const (
	// KindPassword is the built-in username/password flow. Always
	// registered; never license-gated. Routes via POST /auth/login.
	KindPassword ProviderKind = "password"
	// KindLDAP is an LDAP/AD bind provider. Routes via POST /auth/login
	// with provider="<name>". License-gated on sso_ldap.
	KindLDAP ProviderKind = "ldap"
	// KindSAML is a SAML 2.0 SP-initiated SSO provider. Uses redirect
	// flows (GET begin / POST ACS callback) rather than password POST.
	// License-gated on sso_saml.
	KindSAML ProviderKind = "saml"
)

// AuthResult is what an IdentityProvider returns on successful
// credential verification. The login handler turns this into a session
// + cookie. UserRef points at our local "user" table; providers that
// need to provision new accounts (LDAP/SAML JIT) populate JIT and the
// handler creates the row before issuing the session.
type AuthResult struct {
	// UserRef is the resolved local user ref. Zero is only valid when
	// JIT is non-nil (the handler must provision before issuing).
	UserRef int64

	// JIT, when non-nil, asks the handler to provision a new local
	// user row with the provided spec before issuing the session.
	// Used by LDAP/SAML providers that want federated identity to
	// auto-create local rows on first login. Hook reserved for the
	// 1.18 provider impls; the foundation here just defines the shape.
	JIT *JITUser
}

// JITUser is the spec an IdentityProvider passes when asking the
// login handler to provision a new local account. All fields are
// optional except Username — the handler uses sensible defaults
// when fields are empty.
type JITUser struct {
	Username string
	Email    string
	Fullname string
	// ExternalSubject is the provider's stable identifier for this
	// principal (LDAP DN, SAML NameID, OIDC sub). Persisted alongside
	// the user row so subsequent logins resolve back to the same
	// local user even when Username changes upstream.
	ExternalSubject string
}

// ErrProviderUnimplemented is returned by stub providers — the
// foundation is in place but the impl lands in a later phase. The
// login handler maps this to a 501 with a "feature licensed but not
// yet implemented" message so admins distinguish "we need to upgrade
// our license" (provider not registered → 401) from "the binary
// doesn't have the impl yet" (registered → 501).
var ErrProviderUnimplemented = errors.New("auth: identity provider not yet implemented in this binary")

// ErrProviderUnsupportedMethod is what password-style providers
// return when they're asked to authenticate via a flow they don't
// support (e.g. POSTing password to a SAML provider).
var ErrProviderUnsupportedMethod = errors.New("auth: identity provider does not support this authentication method")

// ErrInvalidCredentials is the generic "bad username or password"
// return from password-style providers. Mapped to 401 by the handler
// with a constant message that does NOT leak whether the username
// existed (anti-enumeration).
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// IdentityProvider is the minimal interface every login backend
// satisfies. Concrete providers can extend with kind-specific
// interfaces (RedirectFlow for SAML/OIDC, etc.) — the handler asserts
// for those when routing the matching HTTP verbs.
type IdentityProvider interface {
	// Name is the stable identifier used in API payloads + DB rows.
	// MUST be lowercase, [a-z0-9_-]+, unique within a Registry.
	// Examples: "password", "ldap", "ldap-engineering", "saml-okta".
	Name() string

	// DisplayName is the human-readable label rendered on the login
	// screen ("Sign in with Okta"). Localised on the frontend side
	// from the i18n catalog — server returns the English default.
	DisplayName() string

	// Kind tells the UI how to render this provider (a password form
	// vs. a "Sign in with X" button that triggers a redirect flow).
	Kind() ProviderKind

	// RequiredLicenseFeature is the license feature flag that gates
	// this provider's registration. Empty for community providers
	// (the built-in PasswordProvider). Non-empty values are checked
	// against licensing.Source.HasFeature at registry-attach time;
	// a missing feature aborts registration with an error.
	RequiredLicenseFeature() string

	// SupportsPassword reports whether this provider accepts
	// (username, password) credentials posted to /auth/login. False
	// for redirect-flow providers (SAML, OIDC) — they get routed via
	// kind-specific endpoints instead.
	SupportsPassword() bool

	// Authenticate runs the credential check. Called by the login
	// handler only when SupportsPassword is true. Providers do NOT
	// handle rate limiting, cookie minting, or audit logging — those
	// stay in the handler so all providers get them uniformly.
	//
	// Return values:
	//   - AuthResult{UserRef: x} for an existing local user
	//   - AuthResult{JIT: &spec} to ask the handler to provision
	//   - ErrInvalidCredentials for a bad password / bind failure
	//   - ErrProviderUnimplemented for stub builds
	//   - any other error → handler returns 500
	Authenticate(ctx context.Context, username, password string) (AuthResult, error)
}

// Registry holds the set of identity providers active in this
// process. Constructed at boot, frozen after wiring — no runtime
// add/remove. A nil Registry is a programming error (the handler
// panics) rather than a silent fallback to password-only, so misuse
// surfaces loudly.
type Registry struct {
	mu        sync.RWMutex
	providers []IdentityProvider
	byName    map[string]IdentityProvider
}

// NewRegistry returns an empty Registry. Wire providers via Register
// at boot, then pass to handlers.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]IdentityProvider{}}
}

// Register adds a provider to the registry. Returns an error when:
//   - the name conflicts with an already-registered provider, OR
//   - the provider's RequiredLicenseFeature is non-empty and the
//     supplied LicenseSource doesn't include it.
//
// A nil source counts as "no features" for license-gating purposes —
// that's the community-mode default. Providers with no required
// feature register unconditionally.
func (r *Registry) Register(p IdentityProvider, src LicenseSource) error {
	if p == nil {
		return errors.New("auth: Register called with nil provider")
	}
	name := p.Name()
	if name == "" {
		return errors.New("auth: provider has empty Name()")
	}
	if feat := p.RequiredLicenseFeature(); feat != "" {
		if src == nil || !src.HasFeature(feat) {
			return fmt.Errorf("auth: provider %q requires license feature %q not present in this install", name, feat)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("auth: provider %q already registered", name)
	}
	r.byName[name] = p
	r.providers = append(r.providers, p)
	return nil
}

// MustRegister wraps Register and panics on failure. Use at boot for
// the built-in password provider where registration failure is a
// programming error, not a license decision. For license-gated
// providers (LDAP, SAML), use Register and propagate the error so the
// boot log captures "feature not licensed, skipping".
func (r *Registry) MustRegister(p IdentityProvider, src LicenseSource) {
	if err := r.Register(p, src); err != nil {
		panic(err)
	}
}

// Get returns the provider for a given name, or (nil, false) if
// absent. Callers use this to dispatch /auth/login when the request
// names a provider; "password" is the implicit default when the field
// is omitted.
func (r *Registry) Get(name string) (IdentityProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[name]
	return p, ok
}

// List returns a stable, sorted-by-name snapshot of the registered
// providers. Used by GET /auth/providers to render the login screen.
// Callers must not mutate the returned slice.
func (r *Registry) List() []IdentityProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]IdentityProvider, len(r.providers))
	copy(out, r.providers)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Has reports whether the named provider is registered. Convenience
// for boot wiring + per-request route handlers that look the provider
// up at request time so registry swaps take effect without a process
// restart.
func (r *Registry) Has(name string) bool {
	_, ok := r.Get(name)
	return ok
}

// Replace atomically swaps the registry's contents to the provided
// set. Used by the license-reload hook in licensing.State: when an
// admin uploads a new .lic via /admin/license/upload, the bridge in
// http/server.go rebuilds the provider list from the new feature set
// and hands it here. Old in-flight requests that captured a provider
// reference keep working; new requests see the new set.
//
// Each provider in `ps` is re-checked against src for its required
// license feature, so a half-built provider list can't sneak in. Any
// rejection aborts the entire swap (no partial update) and returns
// the error so the caller can log it and leave the previous state in
// place.
//
// Registration order is preserved; sorting happens in List() at
// read time.
func (r *Registry) Replace(ps []IdentityProvider, src LicenseSource) error {
	// Validate everything BEFORE touching the registry so a single
	// bad entry can't poison the swap.
	byName := make(map[string]IdentityProvider, len(ps))
	for _, p := range ps {
		if p == nil {
			return errors.New("auth: Replace got nil provider in list")
		}
		name := p.Name()
		if name == "" {
			return errors.New("auth: Replace got provider with empty Name()")
		}
		if feat := p.RequiredLicenseFeature(); feat != "" {
			if src == nil || !src.HasFeature(feat) {
				return fmt.Errorf("auth: Replace: provider %q requires license feature %q not present", name, feat)
			}
		}
		if _, dup := byName[name]; dup {
			return fmt.Errorf("auth: Replace: duplicate provider name %q in list", name)
		}
		byName[name] = p
	}
	r.mu.Lock()
	r.providers = append([]IdentityProvider(nil), ps...)
	r.byName = byName
	r.mu.Unlock()
	return nil
}
