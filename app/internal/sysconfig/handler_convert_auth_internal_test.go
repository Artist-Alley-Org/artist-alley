// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// In-package tests for the AuthConfig <-> openapi.AuthConfig
// converters (#712). No database — these are the pure translation
// layer, and a dropped field here is invisible everywhere else: the
// endpoint answers 200 with a body that looks fine, and the setting
// just never changes.
//
// That is exactly what had happened to self_registration. The admin
// auth page has shipped the three controls since 1.19.C and posts
// them, but authToAPI never returned the block and apiToAuth never
// read it, so the checkbox reverted on every reload and
// auth.self_registration.enabled could only be set by writing
// system_config by hand — which is why /register could not be opened
// at all.

package sysconfig

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func TestAuthConverters_SelfRegistrationRoundTrips(t *testing.T) {
	in := AuthConfig{
		SelfRegistration: SelfRegistrationConfig{
			Enabled:                  true,
			RequireEmailVerification: false,
			DefaultRole:              "Newcomer",
		},
	}

	api := authToAPI(in)
	if api.SelfRegistration == nil {
		t.Fatal("authToAPI dropped self_registration entirely")
	}
	if api.SelfRegistration.Enabled == nil || !*api.SelfRegistration.Enabled {
		t.Error("authToAPI lost self_registration.enabled")
	}
	if api.SelfRegistration.RequireEmailVerification == nil || *api.SelfRegistration.RequireEmailVerification {
		t.Error("authToAPI lost self_registration.require_email_verification")
	}
	if api.SelfRegistration.DefaultRole == nil || *api.SelfRegistration.DefaultRole != "Newcomer" {
		t.Error("authToAPI lost self_registration.default_role")
	}

	back := apiToAuth(api)
	if back.SelfRegistration != in.SelfRegistration {
		t.Errorf("round trip changed self_registration: %+v -> %+v", in.SelfRegistration, back.SelfRegistration)
	}
}

func TestApiToAuth_SelfRegistrationDefaultsClosed(t *testing.T) {
	// Absent block: full-replace semantics, and the safe direction for
	// a switch that opens an install to strangers is closed.
	if got := apiToAuth(openapi.AuthConfig{}); got.SelfRegistration.Enabled {
		t.Error("an omitted self_registration block must read as disabled")
	}

	// Present but partial: only the fields the caller sent move.
	enabled := true
	partial := openapi.AuthConfig{}
	partial.SelfRegistration = &struct {
		DefaultRole              *string `json:"default_role,omitempty"`
		Enabled                  *bool   `json:"enabled,omitempty"`
		RequireEmailVerification *bool   `json:"require_email_verification,omitempty"`
	}{Enabled: &enabled}

	got := apiToAuth(partial)
	if !got.SelfRegistration.Enabled {
		t.Error("self_registration.enabled=true was not applied")
	}
	if got.SelfRegistration.DefaultRole != "" {
		t.Errorf("default_role invented a value: %q", got.SelfRegistration.DefaultRole)
	}
}
