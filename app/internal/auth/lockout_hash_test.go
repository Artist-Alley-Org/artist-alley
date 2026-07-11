// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIPSubnetHash_SameSubnetDifferentIP_MatchesHash — two IPs
// within the same /24 (IPv4) OR /56 (IPv6) subnet produce the
// same salted hash. Records threat class without becoming a
// per-IP audit log.
func TestIPSubnetHash_SameSubnetDifferentIP_MatchesHash(t *testing.T) {
	salt := "test-salt"
	cases := []struct {
		name  string
		a, b  string
		match bool
	}{
		{"same_v4_subnet", "203.0.113.10", "203.0.113.99", true},
		{"different_v4_subnet", "203.0.113.10", "198.51.100.10", false},
		{"same_v6_subnet", "2001:db8:1234:5600::1", "2001:db8:1234:5600::ff:ff", true},
		{"different_v6_subnet", "2001:db8:1234:5600::1", "2001:db8:1234:5700::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ra := reqFor(tc.a)
			rb := reqFor(tc.b)
			ha := ipSubnetHash(ra, salt)
			hb := ipSubnetHash(rb, salt)
			if ha == "" || hb == "" {
				t.Fatalf("empty hash: %q %q", ha, hb)
			}
			if (ha == hb) != tc.match {
				t.Fatalf("hash match=%v want %v (a=%q b=%q)", ha == hb, tc.match, ha, hb)
			}
		})
	}
}

// TestIPSubnetHash_EmptySalt_ReturnsEmpty — no salt configured
// means no hash surfaces (audit still fires with empty
// ip_subnet_hash).
func TestIPSubnetHash_EmptySalt_ReturnsEmpty(t *testing.T) {
	if got := ipSubnetHash(reqFor("203.0.113.10"), ""); got != "" {
		t.Fatalf("empty salt should return empty hash; got %q", got)
	}
}

// TestIPSubnetHash_NilRequest_ReturnsEmpty — no request means no
// IP means no hash.
func TestIPSubnetHash_NilRequest_ReturnsEmpty(t *testing.T) {
	if got := ipSubnetHash(nil, "salt"); got != "" {
		t.Fatalf("nil request should return empty hash; got %q", got)
	}
}

// TestIPSubnetHash_SaltRotation_ChangesHash — rotating the salt
// breaks correlation of historical audits, as documented.
func TestIPSubnetHash_SaltRotation_ChangesHash(t *testing.T) {
	req := reqFor("203.0.113.10")
	h1 := ipSubnetHash(req, "salt-a")
	h2 := ipSubnetHash(req, "salt-b")
	if h1 == "" || h2 == "" {
		t.Fatalf("empty hash: %q %q", h1, h2)
	}
	if h1 == h2 {
		t.Fatalf("expected different hashes after salt rotation; got same %q", h1)
	}
}

func reqFor(ip string) *http.Request {
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.RemoteAddr = ip + ":1234"
	return r
}
