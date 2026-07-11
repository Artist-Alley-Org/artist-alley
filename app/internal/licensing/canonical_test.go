// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package licensing

import (
	"testing"
)

// Canonical JSON output is the wire contract with the TypeScript
// license server. If THIS test diverges, every signed license stops
// verifying. The expected strings below are what the TS server's
// canonicalJSON({...}) produces for the same inputs.

func TestCanonicalJSON_SortsKeys(t *testing.T) {
	in := map[string]any{
		"b": 1,
		"a": 2,
		"c": map[string]any{
			"z": 3,
			"y": 4,
		},
	}
	got, err := canonicalBytes(in)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want := `{"a":2,"b":1,"c":{"y":4,"z":3}}`
	if string(got) != want {
		t.Errorf("canonical sort mismatch\n  got:  %s\n  want: %s", got, want)
	}
}

func TestCanonicalJSON_PreservesNull(t *testing.T) {
	in := map[string]any{
		"a": nil,
		"b": 1,
		"c": nil,
	}
	got, err := canonicalBytes(in)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want := `{"a":null,"b":1,"c":null}`
	if string(got) != want {
		t.Errorf("canonical null mismatch\n  got:  %s\n  want: %s", got, want)
	}
}

func TestCanonicalJSON_PreservesArrayOrder(t *testing.T) {
	in := map[string]any{
		"xs": []any{3, 1, 2},
	}
	got, err := canonicalBytes(in)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want := `{"xs":[3,1,2]}`
	if string(got) != want {
		t.Errorf("array order mismatch\n  got:  %s\n  want: %s", got, want)
	}
}

func TestCanonicalJSON_NoHTMLEscaping(t *testing.T) {
	in := map[string]any{
		"owner": "admin@studio.com<test>",
	}
	got, err := canonicalBytes(in)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	// TS JSON.stringify with default escape set keeps `<` and `>`
	// as literal characters. Go's encoder html-escapes them by
	// default — we explicitly disable that. This guards the disable
	// from being accidentally reverted.
	want := `{"owner":"admin@studio.com<test>"}`
	if string(got) != want {
		t.Errorf("html escape leak\n  got:  %s\n  want: %s", got, want)
	}
}

func TestCanonicalLicenseMap_BoundDomainsNullVsEmpty(t *testing.T) {
	// The TS canonical preserves the difference between null and [].
	// Our Go encoder must do the same — a license signed with
	// bound_domains:null differs cryptographically from one signed
	// with bound_domains:[].
	c := LicenseClaims{
		V: 1, KID: "k", LID: "l", Product: "p", Tier: "t",
		Owner: "o", Org: "or", NotBefore: 1, Expires: 2, IssuedAt: 1,
		Features: []string{"core"},
		BoundDomains: nil, // → null
		Issuer: "i",
	}
	gotNull, err := canonicalBytes(canonicalLicenseMap(c))
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if !containsBytes(gotNull, []byte(`"bound_domains":null`)) {
		t.Errorf("expected bound_domains:null in: %s", gotNull)
	}

	c.BoundDomains = []string{} // → []
	gotEmpty, err := canonicalBytes(canonicalLicenseMap(c))
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if !containsBytes(gotEmpty, []byte(`"bound_domains":[]`)) {
		t.Errorf("expected bound_domains:[] in: %s", gotEmpty)
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
