// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Feature-flag helpers + the Source interface every dependent
// package consumes.
//
// The goal: when we ship LDAP / SAML / multi-tenant / etc., the
// capability layer (auth.Identity.Can) can ask "does this install's
// license include the required feature?" without auth needing to
// know how licensing works under the hood. Source is the bridge.
//
// Wiring:
//   - main.go constructs *State via NewState(path)
//   - main.go passes State (implementing Source) into auth.NewHandler,
//     capabilities resolver, plugin loader, etc.
//   - Each dependent package only sees the Source surface — small,
//     stable, test-stubbable.
//
// For pure license-only checks (no per-user RBAC — e.g. registering
// /admin/system/auth/ldap endpoints), handlers call HasFeature
// directly. For mixed checks (per-user RBAC plus per-install feature),
// the capability table is annotated with the required feature and
// auth.Identity.Can consults Source automatically. The mixed path
// lands in a follow-up PR; this commit ships the Source contract.

package licensing

import "time"

// Source is the read-only surface other packages depend on. Always
// returns sane defaults even when no license is loaded — community
// mode is the floor.
type Source interface {
	// Tier returns the active license tier: "pro" | "enterprise" |
	// "complementary" | "plugin" | "custom" | "community". Community
	// means no license file installed; the app's built-in defaults
	// apply.
	Tier() string

	// HasFeature returns true when the current license includes the
	// named feature code. When no license is installed, returns true
	// for community-tier features (core, thumbnails, ai_enrichment,
	// collab, api_access) and false for enterprise-only (sso_ldap,
	// sso_saml, multi_tenant, audit_export, high_availability,
	// priority_support).
	HasFeature(feature string) bool

	// Features returns the snapshot list of all features the
	// current license includes. Useful for the /license/status UI
	// + diagnostics; callers should prefer HasFeature for routing.
	Features() []string

	// Status returns the snapshot the admin UI renders + handlers
	// inspect for cap-enforcement. Always safe to call; returns a
	// "community" status when no license is loaded.
	Status() Status
}

// Status is what handlers + the admin UI see when they ask "what's
// the license state?". Fields beyond the bare tier/features are
// included so the status page doesn't need a second round-trip.
type Status struct {
	// Loaded is true when a real license file is mounted + verified.
	// false → community mode.
	Loaded bool `json:"loaded"`

	Tier     string   `json:"tier"`
	Features []string `json:"features"`

	// Owner / Org / LID surface in the admin UI for support routing.
	// Empty in community mode.
	Owner string `json:"owner,omitempty"`
	Org   string `json:"org,omitempty"`
	LID   string `json:"lid,omitempty"`

	// Caps: when nil → unlimited (enterprise / dev). When set,
	// rendered against current usage in the UI.
	Seats          *int64 `json:"seats,omitempty"`
	SeatWindowDays int    `json:"seat_window_days,omitempty"`
	AssetCap       *int64 `json:"asset_cap,omitempty"`

	// Temporal — RFC3339 strings for the wire (JSON friendly), epoch
	// available via the embedded raw claims if a caller wants it.
	NotBefore string `json:"nbf,omitempty"`
	Expires   string `json:"exp,omitempty"`
	IssuedAt  string `json:"iat,omitempty"`

	// DaysUntilExpiry — derived, surfaced for the warning banner in
	// the admin UI (red at <=7, amber at <=30). Negative when
	// expired. Zero in community mode (no expiry).
	DaysUntilExpiry int `json:"days_until_expiry"`

	// LastError holds the last verification error if the loaded
	// license failed to verify (e.g. expired, wrong issuer).
	// Surfaced verbatim in the admin UI so operators can react.
	LastError string `json:"last_error,omitempty"`

	// Issuer string from the license envelope — admin can confirm
	// they installed a file from the canonical authority.
	Issuer string `json:"iss,omitempty"`

	// Source path — where the verifier loaded the .lic from.
	// Useful for diagnostics.
	Path string `json:"path,omitempty"`

	// OrgBindingRequired is true when the loaded license carries an
	// `org_pubkey` claim and therefore requires the customer-held
	// org.key file to activate. Community / trial / dev licenses leave
	// this false.
	OrgBindingRequired bool `json:"org_binding_required"`

	// OrgBound is true when OrgBindingRequired AND the on-disk org.key
	// derives to a public key that matches the license's `org_pubkey`.
	// When OrgBindingRequired is false, this is also true (no binding
	// to verify → trivially satisfied) so cap-enforcement logic can
	// treat "bound" as the green path uniformly.
	OrgBound bool `json:"org_bound"`

	// OrgBindingError carries the binding failure message when
	// OrgBindingRequired is true but OrgBound is false. Echoed verbatim
	// into the admin UI so operators see exactly which step failed
	// (missing file / bad format / public-key mismatch).
	OrgBindingError string `json:"org_binding_error,omitempty"`

	// OrgKeyPath is the on-disk path the verifier consults for org.key.
	// Surfaced so the admin UI can echo it back when the cross-binding
	// fails ("we looked at <path>"). Empty when the install has no
	// configured org-key path.
	OrgKeyPath string `json:"org_key_path,omitempty"`
}

// communityFeatureSet is the feature list any artist-alley install
// gets without a license. Mirrors the community defaults baked into
// the license server's tier model — but lives HERE as the
// authoritative client-side fallback (the server agrees, but the
// app's community mode is what runs when no .lic is present).
//
// Match the contract documented in artist-alley-license-server/README.md
// § Tier model + § Anti-piracy architecture.
var communityFeatureSet = []string{
	"core",
	"thumbnails",
	"ai_enrichment",
	"collab",
	"api_access",
}

// hasFeatureIn is the constant-time-ish lookup used by both the
// real Source impl and tests. Iteration is fine — feature lists
// are tiny.
func hasFeatureIn(feature string, features []string) bool {
	for _, f := range features {
		if f == feature {
			return true
		}
	}
	return false
}

// communityStatus builds the synthetic Status returned when no
// license file is loaded. Single source of truth for the
// community defaults the artist-alley app enforces. Community mode
// has no cross-binding (OrgBound=true, OrgBindingRequired=false) so
// every Source consumer can treat OrgBound as the green flag without
// special-casing community.
func communityStatus() Status {
	return Status{
		Loaded:          false,
		Tier:            "community",
		Features:        append([]string(nil), communityFeatureSet...),
		DaysUntilExpiry: 0,
		OrgBound:        true,
	}
}

// epochToISO renders a Unix-seconds timestamp as RFC3339, or returns
// "" when the input is zero (e.g. unset community status).
func epochToISO(epoch int64) string {
	if epoch == 0 {
		return ""
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}

// daysUntil computes whole-day delta from now to `epoch`. Negative
// when `epoch` is in the past.
func daysUntil(epoch int64) int {
	if epoch == 0 {
		return 0
	}
	delta := time.Until(time.Unix(epoch, 0))
	// Round toward zero so "X days left" doesn't display 30 when
	// only 29.4 are left.
	return int(delta / (24 * time.Hour))
}
