// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ldapauth

import (
	"context"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func TestProvider_InterfaceContract(t *testing.T) {
	p := New("ldap-eng", "Engineering LDAP")
	if got := p.Name(); got != "ldap-eng" {
		t.Errorf("Name = %q, want %q", got, "ldap-eng")
	}
	if got := p.DisplayName(); got != "Engineering LDAP" {
		t.Errorf("DisplayName = %q, want %q", got, "Engineering LDAP")
	}
	if got := p.Kind(); got != auth.KindLDAP {
		t.Errorf("Kind = %q, want %q", got, auth.KindLDAP)
	}
	if got := p.RequiredLicenseFeature(); got != LicenseFeature {
		t.Errorf("RequiredLicenseFeature = %q, want %q", got, LicenseFeature)
	}
	if !p.SupportsPassword() {
		t.Error("SupportsPassword must be true for LDAP")
	}
}

// New() with empty args fills sensible defaults — boot wiring that
// forgets to name the provider still gets a working registration.
func TestProvider_NewDefaults(t *testing.T) {
	p := New("", "")
	if p.Name() != "ldap" {
		t.Errorf("default Name = %q, want ldap", p.Name())
	}
	if p.DisplayName() != "LDAP" {
		t.Errorf("default DisplayName = %q, want LDAP", p.DisplayName())
	}
}

// Stub Authenticate must return ErrProviderUnimplemented so the
// login handler can map it to 501 (distinct from 401 "bad creds").
func TestProvider_AuthenticateReturnsUnimplemented(t *testing.T) {
	p := New("ldap", "LDAP")
	_, err := p.Authenticate(context.Background(), "alice", "secret")
	if !errors.Is(err, auth.ErrProviderUnimplemented) {
		t.Errorf("Authenticate err = %v, want ErrProviderUnimplemented", err)
	}
}
