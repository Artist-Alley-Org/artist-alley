// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.A — typed user-state machine unit tests.
//
// These run pure-Go (no Postgres). The integration coverage for
// state writes + the last-admin invariant lives in state_test.go.

package users

import (
	"errors"
	"testing"
)

func TestUserState_String(t *testing.T) {
	cases := []struct {
		s    UserState
		want string
	}{
		{UserStatePending, "pending"},
		{UserStateActive, "active"},
		{UserStateDisabled, "disabled"},
		{UserStateArchived, "archived"},
		{UserState(99), "unknown(99)"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("String(%d) = %q, want %q", int64(c.s), got, c.want)
		}
	}
}

func TestUserState_IsKnown(t *testing.T) {
	for _, s := range []UserState{
		UserStatePending, UserStateActive, UserStateDisabled, UserStateArchived,
	} {
		if !s.IsKnown() {
			t.Errorf("IsKnown(%s) = false, want true", s)
		}
	}
	for _, s := range []UserState{-1, 4, 99} {
		if s.IsKnown() {
			t.Errorf("IsKnown(%d) = true, want false", int64(s))
		}
	}
}

func TestUserState_CanAuthenticate(t *testing.T) {
	// Pending CAN authenticate — they receive the restricted
	// capability set so they can view the "waiting for approval"
	// page. Disabled + archived cannot.
	cases := []struct {
		s    UserState
		want bool
	}{
		{UserStatePending, true},
		{UserStateActive, true},
		{UserStateDisabled, false},
		{UserStateArchived, false},
		{UserState(99), false}, // unknown defaults to no
	}
	for _, c := range cases {
		if got := c.s.CanAuthenticate(); got != c.want {
			t.Errorf("CanAuthenticate(%s) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestValidateTransition_ValidPaths(t *testing.T) {
	valid := []struct{ from, to UserState }{
		// Self-transitions are idempotent.
		{UserStatePending, UserStatePending},
		{UserStateActive, UserStateActive},
		{UserStateDisabled, UserStateDisabled},
		{UserStateArchived, UserStateArchived},
		// Approval.
		{UserStatePending, UserStateActive},
		// Active → terminal.
		{UserStateActive, UserStateDisabled},
		{UserStateActive, UserStateArchived},
		// Disabled → restore or archive.
		{UserStateDisabled, UserStateActive},
		{UserStateDisabled, UserStateArchived},
		// Archived → restore.
		{UserStateArchived, UserStateActive},
	}
	for _, c := range valid {
		if err := ValidateTransition(c.from, c.to); err != nil {
			t.Errorf("ValidateTransition(%s, %s) = %v, want nil", c.from, c.to, err)
		}
	}
}

func TestValidateTransition_InvalidPaths(t *testing.T) {
	// Out-of-matrix transitions: admin tooling must route through
	// active to reach these. Each rejection keeps the audit log
	// linear (every archive preceded by an explicit Disable or
	// an explicit Archive from active).
	invalid := []struct{ from, to UserState }{
		// pending can't go straight to disabled / archived.
		{UserStatePending, UserStateDisabled},
		{UserStatePending, UserStateArchived},
		// active can't move backward to pending (would erase the
		// approval record — operator should Disable instead).
		{UserStateActive, UserStatePending},
		// disabled can't move backward to pending.
		{UserStateDisabled, UserStatePending},
		// archived must restore via active.
		{UserStateArchived, UserStatePending},
		{UserStateArchived, UserStateDisabled},
	}
	for _, c := range invalid {
		err := ValidateTransition(c.from, c.to)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("ValidateTransition(%s, %s) = %v, want ErrInvalidTransition", c.from, c.to, err)
		}
	}
}

func TestValidateTransition_UnknownState(t *testing.T) {
	for _, c := range []struct{ from, to UserState }{
		{UserState(99), UserStateActive},
		{UserStateActive, UserState(99)},
		{UserState(99), UserState(99)},
	} {
		err := ValidateTransition(c.from, c.to)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("ValidateTransition(%d, %d) = %v, want ErrInvalidTransition", int64(c.from), int64(c.to), err)
		}
	}
}

func TestRequiresLastAdminCheck_OnlyOutOfActive(t *testing.T) {
	// Only transitions OUT OF active can reduce the active-admin
	// count. Pending can't hold admin; disabled / archived already
	// can't authenticate so they don't contribute to the count.
	needsCheck := []struct{ from, to UserState }{
		{UserStateActive, UserStateDisabled},
		{UserStateActive, UserStateArchived},
		{UserStateActive, UserStatePending},
	}
	for _, c := range needsCheck {
		if !RequiresLastAdminCheck(c.from, c.to) {
			t.Errorf("RequiresLastAdminCheck(%s, %s) = false, want true", c.from, c.to)
		}
	}
	noCheck := []struct{ from, to UserState }{
		// Approvals can only ADD admin capability holders.
		{UserStatePending, UserStateActive},
		// Restoring an admin from disabled/archived re-adds them.
		{UserStateDisabled, UserStateActive},
		{UserStateArchived, UserStateActive},
		// Disabled / archived → archived doesn't change the
		// active count.
		{UserStateDisabled, UserStateArchived},
	}
	for _, c := range noCheck {
		if RequiresLastAdminCheck(c.from, c.to) {
			t.Errorf("RequiresLastAdminCheck(%s, %s) = true, want false", c.from, c.to)
		}
	}
}
