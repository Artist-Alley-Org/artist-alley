package auth

import "sync"

// This file is the auth ↔ licensing seam. The licensing package owns the
// concrete State that loads + verifies the install's .lic file; this
// package owns Identity.Can() and the per-cap mapping table. To keep
// the dependency one-way (licensing imports auth, never the reverse —
// the handler in licensing/handler.go already does), auth declares a
// minimal interface here and wires the concrete State as that interface
// at boot.
//
// The mapping (cap code → required license feature) is loaded from the
// capabilities.required_license_feature column at startup and refreshed
// when role/grant invalidations fire — never queried per-Can() call.
// Per-call DB hits would be a regression against the in-process cap
// cache that Resolver already maintains.

// LicenseSource is the slice of licensing.State that Can() consults
// when deciding whether an install is licensed to use a capability.
// The licensing package implements this on *licensing.State; tests can
// substitute a stand-in without pulling the verifier package.
//
// Returning true for an unknown feature is intentionally NOT part of
// the contract — license sources MUST be authoritative or absent.
// A nil source means "no license loaded" → community-tier behaviour,
// which the cap-mapping layer handles separately (see capLicenseAllows).
type LicenseSource interface {
	HasFeature(name string) bool
}

var (
	licMu                sync.RWMutex
	licSource            LicenseSource
	capLicenseFeatures   map[string]string // cap code -> required license feature; entry absent = no license dep
)

// SetLicenseSource installs the LicenseSource consulted by Can(). Call
// once at boot; safe to call again to swap the source (tests). Passing
// nil disables license gating entirely — caps that name a feature will
// fall back to community behaviour, which currently means: the cap is
// denied unless its feature is in the community-tier baseline set on
// the licensing package side. Without a source we can't ask, so a
// nil-source install treats every license-gated cap as denied. That's
// the safe default; production must always install a source.
func SetLicenseSource(src LicenseSource) {
	licMu.Lock()
	licSource = src
	licMu.Unlock()
}

// SetCapLicenseFeatures replaces the cap → feature map. Call at boot
// after loading capabilities.required_license_feature, and again from
// the cap-cache invalidation path if an admin ever mutates the column
// at runtime (we don't expose that today — the column is migration-
// owned — but the hook is here so future RBAC tooling can wire it).
//
// A nil or empty map means no cap requires a license feature → every
// Can() check passes the license gate on the strength of RBAC alone.
func SetCapLicenseFeatures(m map[string]string) {
	licMu.Lock()
	if m == nil {
		capLicenseFeatures = nil
	} else {
		// Copy so callers can't mutate our table after handoff.
		clone := make(map[string]string, len(m))
		for k, v := range m {
			clone[k] = v
		}
		capLicenseFeatures = clone
	}
	licMu.Unlock()
}

// capLicenseAllows reports whether the install's active license permits
// the given capability code. Logic:
//
//   - If the cap has no required_license_feature entry → allow.
//   - If the cap has a feature requirement and no LicenseSource is
//     installed → deny. Without a source we can't prove the install
//     holds the feature; failing closed is the safe call. (In practice
//     this only matters in tests that forget to install a source; the
//     production wiring always installs one — community-mode included,
//     because the licensing.State falls back to a community Source
//     even when no .lic file is present.)
//   - Otherwise → defer to LicenseSource.HasFeature.
//
// This check is install-level, not user-level: it runs BEFORE the
// SuperAdmin shortcut in Can() so a SuperAdmin on a Community install
// still cannot invoke a cap that requires (say) sso_ldap. SuperAdmin
// is about USER authorisation; license features are about INSTALL
// authorisation. A user with every RBAC cap in the system still cannot
// reach features the install didn't pay for.
func capLicenseAllows(code string) bool {
	licMu.RLock()
	feature, gated := capLicenseFeatures[code]
	src := licSource
	licMu.RUnlock()
	if !gated || feature == "" {
		return true
	}
	if src == nil {
		return false
	}
	return src.HasFeature(feature)
}
