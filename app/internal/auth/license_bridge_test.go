// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import "testing"

// Tests for the license ↔ capability bridge from Phase 1.17.O-2.
// These pin the load-bearing security invariant introduced in that
// phase: "SuperAdmin authority is USER-level; enterprise feature
// gating is INSTALL-level — SuperAdmin on an install whose active
// .lic doesn't include `sso_ldap` MUST NOT reach a capability marked
// required_license_feature=sso_ldap".
//
// "Whose .lic doesn't include the feature" rather than "unlicensed"
// is deliberate: trial licenses, free-tier licenses, and paid
// licenses all flow through the same verifier — the gate is whether
// the feature flag is present in the verified envelope, NOT whether
// money changed hands. Try-then-upgrade works the moment a customer
// uploads a trial .lic that includes the feature; expiry re-locks
// the gate the next time the 24h re-verify ticker runs.
//
// A regression here would silently grant enterprise features to
// installs whose loaded .lic doesn't include them, so this file
// pins the invariant in tests where a renamed helper or inverted
// check would fail loudly.
//
// Each test resets the package-level license-bridge state on
// teardown so the cases stay hermetic regardless of run order.

// licenseStub satisfies the LicenseSource interface for tests. The
// embedded map lists features the install holds; absent keys mean
// "not licensed".
type licenseStub map[string]bool

func (s licenseStub) HasFeature(name string) bool { return s[name] }

// withLicenseBridge installs a (source, capFeatures) pair for the
// duration of t, then restores the previous state. Lets each test
// drive the bridge without bleeding into others.
func withLicenseBridge(t *testing.T, src LicenseSource, caps map[string]string) {
	t.Helper()
	licMu.Lock()
	prevSrc := licSource
	prevCaps := capLicenseFeatures
	licMu.Unlock()
	SetLicenseSource(src)
	SetCapLicenseFeatures(caps)
	t.Cleanup(func() {
		licMu.Lock()
		licSource = prevSrc
		capLicenseFeatures = prevCaps
		licMu.Unlock()
	})
}

// THE security-critical case: a SuperAdmin identity asking for a
// cap that maps to required_license_feature=sso_ldap must be denied
// when the install's license doesn't include sso_ldap. SuperAdmin's
// wildcard branch in Can() runs AFTER capLicenseAllows by design;
// this test fails if anyone re-orders those branches.
func TestIdentityCan_SuperAdminBlockedByMissingLicenseFeature(t *testing.T) {
	withLicenseBridge(t,
		licenseStub{},
		map[string]string{
			"system.sso.ldap.write": "sso_ldap",
		},
	)
	id := &Identity{
		UserRef:      1,
		Capabilities: []string{SuperAdminCapability},
	}
	if id.Can("system.sso.ldap.write") {
		t.Fatal("SuperAdmin on a Community install must NOT reach sso_ldap caps")
	}
}

// Mirror of the above, but with the feature licensed: SuperAdmin
// reaches the gated cap. Confirms the gate isn't a permanent deny —
// licensing flips it on cleanly.
func TestIdentityCan_SuperAdminAllowedWhenFeaturePresent(t *testing.T) {
	withLicenseBridge(t,
		licenseStub{"sso_ldap": true},
		map[string]string{
			"system.sso.ldap.write": "sso_ldap",
		},
	)
	id := &Identity{
		UserRef:      1,
		Capabilities: []string{SuperAdminCapability},
	}
	if !id.Can("system.sso.ldap.write") {
		t.Fatal("SuperAdmin with sso_ldap licensed must reach sso_ldap caps")
	}
}

// A regular user holding the cap explicitly is blocked by the same
// install-level gate. The license check has to apply BEFORE the
// RBAC check, otherwise an admin who hand-grants `system.sso.ldap.write`
// to a non-admin would let them bypass the license.
func TestIdentityCan_DirectGrantBlockedByMissingLicenseFeature(t *testing.T) {
	withLicenseBridge(t,
		licenseStub{},
		map[string]string{
			"system.sso.ldap.write": "sso_ldap",
		},
	)
	id := &Identity{
		UserRef:      42,
		Capabilities: []string{"system.sso.ldap.write"},
	}
	if id.Can("system.sso.ldap.write") {
		t.Fatal("direct cap grant must NOT bypass install-level license gate")
	}
}

// A cap that's NOT in the gate map passes the license check
// unconditionally — the bridge only blocks caps that explicitly
// declare a required_license_feature. Regression test for "default
// allow" semantics on the bridge.
func TestIdentityCan_UnmappedCapNotAffected(t *testing.T) {
	withLicenseBridge(t,
		licenseStub{},
		map[string]string{
			"system.sso.ldap.write": "sso_ldap",
		},
	)
	id := &Identity{
		UserRef:      1,
		Capabilities: []string{"users.read"},
	}
	if !id.Can("users.read") {
		t.Fatal("unmapped cap must not be blocked by the license bridge")
	}
}

// When a cap is gated but no LicenseSource is installed (a
// programming error in production, but possible in tests), the
// bridge fails closed — the cap is denied. Documented in
// capLicenseAllows; pinning here so a future refactor that "helpfully"
// defaults to allow-on-nil-source would fail loudly.
func TestIdentityCan_NilSourceFailsClosed(t *testing.T) {
	withLicenseBridge(t,
		nil,
		map[string]string{
			"system.sso.ldap.write": "sso_ldap",
		},
	)
	id := &Identity{
		UserRef:      1,
		Capabilities: []string{SuperAdminCapability},
	}
	if id.Can("system.sso.ldap.write") {
		t.Fatal("gated cap with nil LicenseSource must fail closed (deny)")
	}
}

// Empty capLicenseFeatures map means "no caps are license-gated";
// every cap goes straight to the RBAC layer. Important because the
// production bridge replaces the map on startup — if loadCapLicenseFeatures
// returned an empty result we don't want every Can() check to deny.
func TestIdentityCan_EmptyMapAllowsAllRBAC(t *testing.T) {
	withLicenseBridge(t, licenseStub{}, map[string]string{})
	id := &Identity{
		UserRef:      1,
		Capabilities: []string{"users.read"},
	}
	if !id.Can("users.read") {
		t.Fatal("empty gate map must not block RBAC-granted caps")
	}
}

// Defensive: an empty-string `required_license_feature` value in the
// map (which shouldn't be possible — the loader filters nulls — but
// could slip through a typo) is treated as "no gate", not as "gate
// on the empty-named feature". Pins the "" check inside capLicenseAllows.
func TestIdentityCan_EmptyFeatureStringTreatedAsUngated(t *testing.T) {
	withLicenseBridge(t,
		licenseStub{},
		map[string]string{
			"users.read": "",
		},
	)
	id := &Identity{
		UserRef:      1,
		Capabilities: []string{"users.read"},
	}
	if !id.Can("users.read") {
		t.Fatal("empty-string required feature must be treated as ungated")
	}
}

// Anonymous + empty-code paths still hard-deny — adding the license
// bridge in front of them must NOT change those existing semantics.
// Pinning together so a refactor that adds an early-return on
// capLicenseAllows can't accidentally make these pass.
func TestIdentityCan_AnonymousAndEmptyCodeStillDenied(t *testing.T) {
	withLicenseBridge(t, licenseStub{}, nil)
	var nilID *Identity
	if nilID.Can("users.read") {
		t.Fatal("nil Identity must deny")
	}
	id := &Identity{UserRef: 1, Capabilities: []string{SuperAdminCapability}}
	if id.Can("") {
		t.Fatal("empty code must deny even for SuperAdmin")
	}
}
