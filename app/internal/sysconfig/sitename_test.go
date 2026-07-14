// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig_test

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func TestSiteNameOrDefault(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back", "", sysconfig.DefaultSiteName},
		{"blank whitespace falls back", "   ", sysconfig.DefaultSiteName},
		{"operator value wins", "Acme Art Reviews", "Acme Art Reviews"},
		{"value with spaces preserved", "  Studio 7  ", "  Studio 7  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sysconfig.SiteNameOrDefault(c.in); got != c.want {
				t.Fatalf("SiteNameOrDefault(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	if sysconfig.DefaultSiteName != "Artist Alley" {
		t.Fatalf("DefaultSiteName = %q, want %q (product name, no hyphen)", sysconfig.DefaultSiteName, "Artist Alley")
	}
}
