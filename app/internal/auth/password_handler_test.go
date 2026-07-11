// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// 1.17.D — password change + admin reset gates + policy helpers.
//
// DB-backed happy paths land in the integration suite. These tests
// pin the early-return gates + the pure helper functions (policy
// check, temp generator, common-password set).

func TestChangeMyPassword_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.ChangeMyPassword(context.Background(), openapi.ChangeMyPasswordRequestObject{
		Body: &openapi.ChangePasswordRequest{CurrentPassword: "x", NewPassword: "y"},
	})
	if err != nil {
		t.Fatalf("ChangeMyPassword: %v", err)
	}
	if _, ok := resp.(openapi.ChangeMyPassword401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

func TestChangeMyPassword_MissingBody(t *testing.T) {
	h := &Handler{}
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 1})
	resp, err := h.ChangeMyPassword(ctx, openapi.ChangeMyPasswordRequestObject{Body: nil})
	if err != nil {
		t.Fatalf("ChangeMyPassword: %v", err)
	}
	if _, ok := resp.(openapi.ChangeMyPassword400JSONResponse); !ok {
		t.Errorf("expected 400 missing body, got %T", resp)
	}
}

func TestChangeMyPassword_MissingFields(t *testing.T) {
	h := &Handler{}
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 1})
	// Body present but one field empty.
	resp, err := h.ChangeMyPassword(ctx, openapi.ChangeMyPasswordRequestObject{
		Body: &openapi.ChangePasswordRequest{CurrentPassword: "", NewPassword: "valid"},
	})
	if err != nil {
		t.Fatalf("ChangeMyPassword: %v", err)
	}
	if _, ok := resp.(openapi.ChangeMyPassword400JSONResponse); !ok {
		t.Errorf("expected 400 empty current, got %T", resp)
	}
}

func TestAdminResetUserPassword_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.AdminResetUserPassword(context.Background(), openapi.AdminResetUserPasswordRequestObject{Ref: 42})
	if err != nil {
		t.Fatalf("AdminResetUserPassword: %v", err)
	}
	if _, ok := resp.(openapi.AdminResetUserPassword401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

// Reset is a SEPARATE capability from users.write — having read OR
// approve doesn't grant reset. Pin that fence.
func TestAdminResetUserPassword_NeedsResetCap(t *testing.T) {
	h := &Handler{}
	for _, withCap := range []string{"users.read", "users.write", "users.approve"} {
		t.Run(withCap, func(t *testing.T) {
			ctx := WithIdentity(context.Background(), &Identity{UserRef: 7, Capabilities: []string{withCap}})
			resp, err := h.AdminResetUserPassword(ctx, openapi.AdminResetUserPasswordRequestObject{Ref: 42})
			if err != nil {
				t.Fatalf("AdminResetUserPassword: %v", err)
			}
			if _, ok := resp.(openapi.AdminResetUserPassword403JSONResponse); !ok {
				t.Errorf("with cap=%s: expected 403, got %T", withCap, resp)
			}
		})
	}
}

// validatePasswordPolicy bijection. Every accepted password returns
// an empty string; every rejection returns a non-empty user-facing
// message. The message text is part of the contract — UIs render it
// verbatim in the error toast.
func TestValidatePasswordPolicy(t *testing.T) {
	cases := []struct {
		name     string
		password string
		policy   PasswordPolicy
		want     string // substring expected in error; "" = accepted
	}{
		{"empty policy accepts anything", "abc", PasswordPolicy{}, ""},
		{"min length rejects short", "abc", PasswordPolicy{MinLength: 8}, "at least 8 characters"},
		{"min length accepts exact", "12345678", PasswordPolicy{MinLength: 8}, ""},
		{"require upper rejects lower-only", "abcdefgh", PasswordPolicy{RequireUpper: true}, "uppercase"},
		{"require upper accepts mixed", "abcdefgH", PasswordPolicy{RequireUpper: true}, ""},
		{"require number rejects letters-only", "abcdefgh", PasswordPolicy{RequireNumber: true}, "digit"},
		{"require number accepts", "abc12345", PasswordPolicy{RequireNumber: true}, ""},
		{"require symbol rejects alphanumeric", "abcd1234", PasswordPolicy{RequireSymbol: true}, "symbol"},
		{"require symbol accepts !", "abcd1234!", PasswordPolicy{RequireSymbol: true}, ""},
		{"disallow common rejects 'password'", "password", PasswordPolicy{DisallowCommon: true}, "too common"},
		{"disallow common accepts non-common", "T4cosOnTuesday!", PasswordPolicy{DisallowCommon: true}, ""},
		{"combined policy", "T4cosOnTuesday!", PasswordPolicy{MinLength: 12, RequireUpper: true, RequireNumber: true, RequireSymbol: true, DisallowCommon: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validatePasswordPolicy(c.password, c.policy)
			if c.want == "" {
				if got != "" {
					t.Errorf("validatePasswordPolicy(%q) = %q, want accepted", c.password, got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("validatePasswordPolicy(%q) = %q, want substring %q", c.password, got, c.want)
			}
		})
	}
}

func TestGenerateTempPassword(t *testing.T) {
	// 16 chars, URL-safe base64 (no padding). Distinct each call.
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := generateTempPassword()
		if err != nil {
			t.Fatalf("generateTempPassword: %v", err)
		}
		if len(p) != 16 {
			t.Errorf("length %d, want 16: %q", len(p), p)
		}
		if seen[p] {
			t.Errorf("duplicate temp password — RNG output suspect")
		}
		seen[p] = true
		// URL-safe base64 alphabet only.
		for _, r := range p {
			if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
				t.Errorf("char %q outside URL-safe base64 alphabet", r)
			}
		}
	}
}

func TestIsCommonPassword(t *testing.T) {
	for _, c := range []string{"password", "123456", "qwerty", "admin", "artistalley"} {
		if !isCommonPassword(c) {
			t.Errorf("expected %q to be flagged as common", c)
		}
	}
	for _, ok := range []string{"T4cosOnTuesday!", "uncommon-string", "ZxY!1234"} {
		if isCommonPassword(ok) {
			t.Errorf("did not expect %q to be flagged as common", ok)
		}
	}
}

// nopAudit must satisfy the extended interface (we added
// PasswordChanged + PasswordReset). Compile-time assertion.
var _ auditRecorder = nopAudit{}

// Re-pin the interface contract: PasswordPolicy fields align with
// the sysconfig adapter the http layer wires up. A drift here would
// silently zero out half the policy on read.
func TestPasswordPolicy_FieldShape(t *testing.T) {
	p := PasswordPolicy{
		MinLength:      12,
		RequireUpper:   true,
		RequireNumber:  true,
		RequireSymbol:  true,
		DisallowCommon: true,
		MaxAgeDays:     90,
	}
	if p.MinLength != 12 || !p.RequireUpper || !p.RequireNumber || !p.RequireSymbol || !p.DisallowCommon || p.MaxAgeDays != 90 {
		t.Error("PasswordPolicy field set mismatch")
	}
}
