package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// PasswordProvider is the built-in username/password identity
// provider. Always registered in the Registry — it's the floor every
// install gets, license or no license. The login handler dispatches
// to this provider when the request omits the `provider` field
// (back-compat) or sets it to "password".
//
// Lookups go through queries.sql / FindUserByUsername; the credential
// check uses the same legacy-compatible HMAC-then-bcrypt scheme as the
// pre-existing handler (see password.go). This struct deliberately
// holds zero additional state — it's a thin shim that lets the
// password path satisfy IdentityProvider so the registry has one
// uniform dispatch shape across community and enterprise providers.
type PasswordProvider struct {
	pool        *pgxpool.Pool
	scrambleKey string
}

// NewPasswordProvider wires the provider to its dependencies. Called
// once at boot. The pool must be the same one auth.Handler uses —
// shared connection limits and statement caches.
func NewPasswordProvider(pool *pgxpool.Pool, scrambleKey string) *PasswordProvider {
	return &PasswordProvider{pool: pool, scrambleKey: scrambleKey}
}

// Name implements IdentityProvider.
func (*PasswordProvider) Name() string { return "password" }

// DisplayName implements IdentityProvider.
func (*PasswordProvider) DisplayName() string { return "Password" }

// Kind implements IdentityProvider.
func (*PasswordProvider) Kind() ProviderKind { return KindPassword }

// RequiredLicenseFeature implements IdentityProvider. PasswordProvider
// is unconditionally available — every install gets it.
func (*PasswordProvider) RequiredLicenseFeature() string { return "" }

// SupportsPassword implements IdentityProvider.
func (*PasswordProvider) SupportsPassword() bool { return true }

// Authenticate implements IdentityProvider. Resolves username → user
// row, then runs the HMAC-then-bcrypt check. Account-state validation
// (approved? expired?) lives in the login handler, NOT here — the
// handler emits the audit events for those rejections with the
// right reason codes, and other providers need the same treatment.
//
// Returns ErrInvalidCredentials for both "no such user" and "bad
// password" so the handler's 401 response can't be used to enumerate
// usernames. The handler's audit log distinguishes the two via the
// failure-reason it emits.
func (p *PasswordProvider) Authenticate(ctx context.Context, username, password string) (AuthResult, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return AuthResult{}, ErrInvalidCredentials
	}
	q := New(p.pool)
	user, err := q.FindUserByUsername(ctx, &username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AuthResult{}, ErrInvalidCredentials
		}
		return AuthResult{}, err
	}
	if user.Password == nil {
		return AuthResult{}, ErrInvalidCredentials
	}
	if err := VerifyPassword(password, *user.Password, p.scrambleKey); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return AuthResult{}, ErrInvalidCredentials
		}
		return AuthResult{}, err
	}
	return AuthResult{UserRef: user.Ref}, nil
}

// Compile-time interface check — if PasswordProvider drifts away from
// IdentityProvider, this fails the build at the registration site
// rather than at runtime.
var _ IdentityProvider = (*PasswordProvider)(nil)
