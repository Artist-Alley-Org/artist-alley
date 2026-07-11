// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

// Privacy gate — decides whether a given asset's inference call may
// route to any provider OR must stay on the operator-configured
// local set. Pure logic; no I/O. The router consumes
// ClassifyPrivacy + FilterLocalOnly to narrow its candidate set
// before walking the routing/fallback preference order.
//
// Operator policy lives in two system_config keys:
//
//   ai.privacy.lock_sensitive_to_local — bool
//       When true, restricted + embargo assets are clamped to
//       local providers. Default true on fresh install (see
//       migration 00009). Operator can disable via the admin UI
//       with a confirm dialog (admin_ai_privacy page).
//
//   ai.privacy.local_providers — []string
//       Provider names considered "local" for the gate. Defaults
//       to ["ollama", "vllm", "whisper_local", "clip_local"] —
//       any future local provider added by an operator add-on
//       (per ADR 0034) must register its name here too.

// SensitivityTier is the asset's privacy tier. Mirrors the
// assets.sensitivity CHECK constraint (public / team / restricted /
// embargo) from migration 00001's baseline schema. Re-declared here
// as a typed string so the ai package doesn't need to import the
// assets package (which would create a cycle).
type SensitivityTier string

const (
	SensitivityPublic     SensitivityTier = "public"
	SensitivityTeam       SensitivityTier = "team"
	SensitivityRestricted SensitivityTier = "restricted"
	SensitivityEmbargo    SensitivityTier = "embargo"
)

// PrivacyPolicy is the runtime snapshot of the two operator-config
// keys. The cost/config layer keeps this fresh via the cache; gate
// callers receive a copy.
type PrivacyPolicy struct {
	LockSensitiveToLocal bool
	LocalProviders       []string // provider names considered local
}

// ClassifyPrivacy maps (sensitivity, policy) → PrivacyClass.
// Public + team assets always allow any provider; restricted +
// embargo flip to local-only when the lock is engaged. If the
// operator disables the lock entirely, sensitivity stops mattering
// at this layer (the operator has accepted the trade-off explicitly
// via the admin UI confirm dialog).
func ClassifyPrivacy(s SensitivityTier, policy PrivacyPolicy) PrivacyClass {
	if !policy.LockSensitiveToLocal {
		return PrivacyClassAny
	}
	switch s {
	case SensitivityRestricted, SensitivityEmbargo:
		return PrivacyClassLocalOnly
	}
	return PrivacyClassAny
}

// FilterLocalOnly returns the subset of provider names that appear
// in the operator's LocalProviders allow-list. Used by the router
// to narrow candidates when PrivacyClass is LocalOnly.
//
// If the filtered set is empty, the router surfaces
// ErrNoProviderAvailable wrapped in a ProviderError{Class:
// ErrClassPrivacy} so the operator dashboard can show the actionable
// "N calls blocked by privacy policy" signal.
func FilterLocalOnly(candidates []string, policy PrivacyPolicy) []string {
	if len(candidates) == 0 {
		return nil
	}
	local := make(map[string]struct{}, len(policy.LocalProviders))
	for _, n := range policy.LocalProviders {
		local[n] = struct{}{}
	}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := local[c]; ok {
			out = append(out, c)
		}
	}
	return out
}
