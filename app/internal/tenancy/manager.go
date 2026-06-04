// Package tenancy is the multi-tenant manager surface, enterprise-
// gated on license feature "multi_tenant".
//
// IMPORTANT scope distinction (user direction, recorded in
// memory `project_phase_1_17_inflight`):
//
//   - **Federation** (peer-to-peer instances that gossip metadata,
//     share collections across independently-administered hosts) is
//     NOT gated. Small communities can self-organize freely. The
//     federation primitives live elsewhere (origin_server_id columns,
//     cross-instance API surfaces); this package doesn't touch them.
//
//   - **Multi-tenant** (single binary hosting multiple isolated
//     tenant namespaces under one operator, with per-tenant admins
//     and storage quotas) IS gated. That's a vendor scaling tool, not
//     a community feature. THIS package is the enforcement seam.
//
// Phase 1.17.P-foundation ships the package + Manager shape + the
// boot-time gate. Tenant CRUD, per-tenant routing middleware, and
// storage isolation land in 1.18.

package tenancy

import (
	"context"
	"errors"
	"log/slog"
)

// LicenseFeature is the feature flag string a license must include
// for the multi-tenant Manager to be instantiated. The community
// feature set in licensing/features.go deliberately excludes this.
const LicenseFeature = "multi_tenant"

// Manager is the multi-tenant control surface. Returned non-nil only
// when the install holds LicenseFeature; nil otherwise. Every caller
// that wants to do tenant-aware work MUST check for nil first and
// degrade to single-tenant behaviour.
//
// The fields are deliberately unexported in this stub — the real impl
// in 1.18 will own a tenants table, per-tenant cache, etc.
type Manager struct {
	logger *slog.Logger
}

// ErrNotLicensed is returned by Manager methods when the install
// doesn't hold the multi_tenant feature. Callers that ignore the nil
// guard on construction and try to call a Manager method get a
// loudly-typed error instead of a panic.
var ErrNotLicensed = errors.New("tenancy: multi_tenant feature not licensed on this install")

// LicenseSource is the slice of licensing.Source this package needs.
// Mirrors the auth.LicenseSource pattern so we keep the dependency
// arrow pointing INTO this package (licensing → tenancy via this
// interface) and never the other way around. The licensing.State
// type satisfies this implicitly via HasFeature.
type LicenseSource interface {
	HasFeature(name string) bool
}

// NewManager returns the multi-tenant Manager when the install holds
// LicenseFeature, otherwise nil. The boot wiring in http/server.go
// captures the returned value into a package-level slot consumed by
// the (future) tenant-aware middleware + admin handlers.
//
// Returning nil rather than a "disabled" Manager is deliberate:
//   - Callers see a clear "feature unavailable" state without an
//     extra Enabled() method.
//   - A patcher who flips HasFeature has to also synthesize a
//     non-nil Manager AND wire it into every consumer — significant
//     surgery, not a one-byte runtime patch.
func NewManager(src LicenseSource, logger *slog.Logger) *Manager {
	if src == nil || !src.HasFeature(LicenseFeature) {
		if logger != nil {
			logger.Info("tenancy: multi_tenant feature absent; manager disabled")
		}
		return nil
	}
	if logger != nil {
		logger.Info("tenancy: multi_tenant feature present; manager enabled")
	}
	return &Manager{logger: logger}
}

// Enabled is a convenience for `m != nil` so call sites read better:
// `if mgr.Enabled() { ... }` over `if mgr != nil { ... }`. Safe to
// call on a nil receiver — Go nil-method-on-nil-receiver semantics.
func (m *Manager) Enabled() bool { return m != nil }

// ListTenants is the stub the future admin UI hits to render the
// tenant list. Returns ErrNotLicensed when the Manager is nil so
// callers see a typed error rather than a nil-pointer panic.
//
// The real impl in 1.18 will hit a `tenants` table seeded by
// migration 0004X. For now: feature exists, surface exists, body
// returns ErrNotImplemented when the Manager IS enabled but the impl
// hasn't landed — that's two distinct errors so the admin UI knows
// whether to nag about licensing or about an in-progress build.
func (m *Manager) ListTenants(_ context.Context) ([]Tenant, error) {
	if !m.Enabled() {
		return nil, ErrNotLicensed
	}
	return nil, ErrNotImplemented
}

// ErrNotImplemented is returned by Manager methods that exist as
// foundation stubs but whose impl lands in a follow-on phase.
var ErrNotImplemented = errors.New("tenancy: multi-tenant operation not yet implemented in this binary")

// Tenant is the public projection of a tenant row. Stable shape so
// the admin UI + Go callers can be written ahead of the impl.
type Tenant struct {
	ID          string
	Slug        string
	DisplayName string
	// Quotas + per-tenant config will land here in the impl phase.
}
