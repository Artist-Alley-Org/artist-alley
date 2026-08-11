// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #916 — principal_id is a REFERENCE, and the boundary now says so.
//
// These are pure unit tests: no DB, no handler. The integration half —
// that a rejected grant writes no row, and that a valid one still
// grants and still notifies — lives in posts/acl_principal_test.go,
// because "returns 400" and "wrote nothing" are different claims and
// only the second one distinguishes this fix from a handler that 400s
// after inserting.

package acls_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/acls"
)

func TestValidatePrincipalRef(t *testing.T) {
	cases := []struct {
		name          string
		principalType string
		principalID   string
		wantErr       bool
		errContains   string
	}{
		// The bug, stated: a username was accepted here and became a
		// row the read rule could never match.
		{"username for a user principal", "user", "alice", true, "numeric user ref"},
		{"empty", "user", "", true, "required"},
		{"a UUID where a user ref belongs", "user", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", true, "numeric user ref"},
		{"negative ref", "user", "-1", true, "positive"},
		{"zero ref", "user", "0", true, "positive"},
		{"float", "user", "12.5", true, "numeric user ref"},
		{"leading space", "user", " 12", true, "numeric user ref"},
		{"valid user ref", "user", "12", false, ""},
		{"valid large user ref", "user", "9007199254740993", false, ""},

		{"valid role uuid", "role", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", false, ""},
		{"valid team uuid", "team", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", false, ""},
		{"role name instead of uuid", "role", "editors", true, "must be a UUID"},
		{"numeric ref for a role", "role", "12", true, "must be a UUID"},
		{"team name instead of uuid", "team", "design", true, "must be a UUID"},

		{"unknown principal type", "group", "12", true, "user|role|team"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := acls.ValidatePrincipalRef(tc.principalType, tc.principalID)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidatePrincipalRef(%q,%q) = nil, want an error",
					tc.principalType, tc.principalID)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidatePrincipalRef(%q,%q) = %v, want nil",
					tc.principalType, tc.principalID, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.errContains) {
				t.Errorf("error %q does not mention %q — the caller has to be able to act on it",
					err, tc.errContains)
			}
		})
	}
}

// Content ACLs honour `user` and nothing else, because their read rules
// gate on principal_type='user' before looking at the id. A role or
// team grant on a post is inert in exactly the way a username is, so it
// is refused rather than stored.
func TestValidateContentPrincipal_RejectsInertTypes(t *testing.T) {
	for _, pt := range []string{"role", "team"} {
		err := acls.ValidateContentPrincipal(pt, "3f2504e0-4f89-11d3-9a0c-0305e82c3301")
		if err == nil {
			t.Fatalf("ValidateContentPrincipal(%q, <valid uuid>) = nil; a grant that "+
				"confers nothing must not be accepted", pt)
		}
		if !errors.Is(err, acls.ErrPrincipalInert) {
			t.Errorf("ValidateContentPrincipal(%q,…) = %v, want ErrPrincipalInert so "+
				"callers can distinguish inert from malformed", pt, err)
		}
	}
}

func TestValidateContentPrincipal_UserPathMatchesRefRules(t *testing.T) {
	if err := acls.ValidateContentPrincipal("user", "12"); err != nil {
		t.Fatalf("valid user ref rejected: %v", err)
	}
	err := acls.ValidateContentPrincipal("user", "alice")
	if err == nil {
		t.Fatal("username accepted on a content ACL")
	}
	if errors.Is(err, acls.ErrPrincipalInert) {
		t.Error("a malformed user ref must not report as ErrPrincipalInert — " +
			"the type is honoured, the value is wrong")
	}
}

// asset_type_acls honours all three principal types (its access queries
// resolve role and team membership), so the shape validator must keep
// admitting them. This guards against someone collapsing the two
// validators into one on the theory that they are the same rule.
func TestValidatePrincipalRef_AdmitsRoleAndTeamForAssetTypes(t *testing.T) {
	const u = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	for _, pt := range []string{"user", "role", "team"} {
		id := u
		if pt == "user" {
			id = "12"
		}
		if err := acls.ValidatePrincipalRef(pt, id); err != nil {
			t.Errorf("ValidatePrincipalRef(%q,%q) = %v; asset_type_acls honours all "+
				"three types, so the shape validator must admit them", pt, id, err)
		}
	}
}
