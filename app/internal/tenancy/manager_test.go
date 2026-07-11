// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package tenancy

import (
	"context"
	"errors"
	"testing"
)

type stubSource map[string]bool

func (s stubSource) HasFeature(name string) bool { return s[name] }

// Without the multi_tenant feature, NewManager returns nil — the
// canonical "feature unavailable" state every consumer checks for.
// Enabled() on the nil receiver MUST also work (Go nil-method
// semantics) so call sites can guard without an extra is-nil check.
func TestNewManager_NoLicenseReturnsNil(t *testing.T) {
	m := NewManager(stubSource{}, nil)
	if m != nil {
		t.Fatal("expected nil Manager without multi_tenant feature")
	}
	if m.Enabled() {
		t.Fatal("Enabled() on nil Manager must be false")
	}
}

// Nil LicenseSource is the same as "no features" — Manager stays nil.
func TestNewManager_NilSourceReturnsNil(t *testing.T) {
	if NewManager(nil, nil) != nil {
		t.Fatal("expected nil Manager with nil source")
	}
}

// With the feature present, the Manager is non-nil + Enabled().
func TestNewManager_LicensedReturnsManager(t *testing.T) {
	m := NewManager(stubSource{LicenseFeature: true}, nil)
	if m == nil {
		t.Fatal("expected non-nil Manager with multi_tenant feature")
	}
	if !m.Enabled() {
		t.Fatal("Enabled() must be true on a real Manager")
	}
}

// ListTenants on a disabled manager returns ErrNotLicensed (not a
// panic). The future admin handlers rely on this typed error to
// decide between "needs license" and "needs build" 4xx responses.
func TestListTenants_NotLicensed(t *testing.T) {
	var m *Manager
	_, err := m.ListTenants(context.Background())
	if !errors.Is(err, ErrNotLicensed) {
		t.Fatalf("err = %v, want ErrNotLicensed", err)
	}
}

// ListTenants on an enabled-but-not-yet-implemented Manager returns
// ErrNotImplemented — distinct from ErrNotLicensed.
func TestListTenants_NotImplemented(t *testing.T) {
	m := NewManager(stubSource{LicenseFeature: true}, nil)
	_, err := m.ListTenants(context.Background())
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented", err)
	}
}
