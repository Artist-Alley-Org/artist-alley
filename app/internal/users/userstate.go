// Phase 1.17.A — typed user-state machine.
//
// The `user.approved` column is BIGINT (legacy from the ResourceSpace
// fork) and pre-1.17.A had int magic 0/1/2 scattered through the code.
// 1.17.A keeps the column shape (no churn — pre-MVP volatility lets
// us defer the TEXT-enum migration to a polish phase) but introduces
// typed Go constants + a transition matrix + a single set of helpers
// so call sites stop comparing against raw ints.
//
// Adds a fourth state: UserStateArchived (= 3). Archived users are
// indistinguishable from disabled from the authentication boundary
// (both rejected), but archived is operator-facing "this user has
// left the org" and disabled is "temporarily revoked" — the auth
// gate treats them identically, the admin UI treats them differently
// (archived users are hidden from the default list, disabled show).
//
// # Why the column stays BIGINT
//
// A TEXT-enum migration would force every read path on the user
// table to widen its scanning, regenerate every sqlc query, and
// break the openapi enum types that the frontend already consumes.
// Pre-MVP, the existing int values are stable enough; the typed
// Go layer gives us the gold-standard call-site ergonomics without
// the schema churn. ADR 0046 (append-only migrations) still applies
// at the constraint level (00003 adds a CHECK that pins the legal
// set to {0,1,2,3}).
//
// # Transition matrix
//
//   pending  → active                       (ApproveUser)
//   active   → disabled | archived          (DisableUser, ArchiveUser)
//   disabled → active | archived            (RestoreUser, ArchiveUser)
//   archived → active                       (RestoreUser)
//
// Self-transitions (active→active, etc.) are idempotent — the
// handler returns 200 with changed=false rather than rejecting.
// Out-of-matrix transitions (e.g. pending→archived, archived→
// disabled) are rejected with ErrInvalidTransition; admin tooling
// must route through active to reach those terminal states. This
// keeps the audit log unambiguous (every archive is preceded by
// an explicit Disable or by an explicit Archive from active).

package users

import (
	"errors"
	"fmt"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// UserState is the typed user lifecycle state. The underlying int
// values are pinned by the schema CHECK constraint added in
// migration 00003 and by the legacy ResourceSpace `approved` column
// convention (1 = active by default).
type UserState int64

const (
	// UserStatePending — account exists but admin hasn't approved.
	// Login is gated; pending users receive the restricted
	// capability set seeded in system_config (users.pending_capabilities).
	UserStatePending UserState = 0

	// UserStateActive — fully provisioned. Default for bootstrap
	// admin + every user the seed pipeline materialises.
	UserStateActive UserState = 1

	// UserStateDisabled — admin-revoked. Sessions cascade-invalidate
	// on transition into this state (Commit 2). Disabled users still
	// appear in the default admin list (operators may need to triage).
	UserStateDisabled UserState = 2

	// UserStateArchived — terminal "this user has left" state.
	// Same auth-gate effect as disabled (login refused) but hidden
	// from the default admin list. Reachable from active or
	// disabled; reversible via RestoreUser.
	UserStateArchived UserState = 3
)

// String renders the state in lowercase noun form per the arc-wide
// naming convention. Used in audit metadata + log lines.
func (s UserState) String() string {
	switch s {
	case UserStatePending:
		return "pending"
	case UserStateActive:
		return "active"
	case UserStateDisabled:
		return "disabled"
	case UserStateArchived:
		return "archived"
	default:
		return fmt.Sprintf("unknown(%d)", int64(s))
	}
}

// IsKnown reports whether s falls in the schema-permitted set. Used
// at the boundary (rows read from PG, payloads decoded from JSON)
// to defend against malformed data without panicking.
func (s UserState) IsKnown() bool {
	switch s {
	case UserStatePending, UserStateActive, UserStateDisabled, UserStateArchived:
		return true
	}
	return false
}

// CanAuthenticate reports whether a user in this state may complete
// the login handshake. Pending users CAN authenticate (they receive
// a restricted capability set so they can view the "waiting for
// approval" page); disabled + archived cannot. This is the single
// gate consumed by Commit 2's assertCanAuthenticate refactor.
func (s UserState) CanAuthenticate() bool {
	switch s {
	case UserStateActive, UserStatePending:
		return true
	case UserStateDisabled, UserStateArchived:
		return false
	}
	return false
}

// ErrInvalidTransition is returned by ValidateTransition when (from,
// to) isn't in the matrix above. Callers map this to HTTP 400 with
// a payload naming both states so the operator can self-diagnose.
var ErrInvalidTransition = errors.New("users: invalid state transition")

// ValidateTransition reports whether moving from → to is permitted.
// Self-transitions (from == to) return nil — they're idempotent at
// the handler layer (returns changed=false). Unknown states on
// either side return ErrInvalidTransition.
func ValidateTransition(from, to UserState) error {
	if !from.IsKnown() || !to.IsKnown() {
		return fmt.Errorf("%w: unknown state (from=%s, to=%s)",
			ErrInvalidTransition, from, to)
	}
	if from == to {
		return nil
	}
	switch from {
	case UserStatePending:
		// pending → active only. No direct pending→disabled
		// (operator should approve first then disable if they
		// realise it was a mistake — preserves audit clarity).
		if to == UserStateActive {
			return nil
		}
	case UserStateActive:
		// active → disabled | archived.
		if to == UserStateDisabled || to == UserStateArchived {
			return nil
		}
	case UserStateDisabled:
		// disabled → active | archived.
		if to == UserStateActive || to == UserStateArchived {
			return nil
		}
	case UserStateArchived:
		// archived → active only. Reaching disabled requires
		// going through active first (re-enable, then disable).
		if to == UserStateActive {
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s not permitted", ErrInvalidTransition, from, to)
}

// RequiresLastAdminCheck reports whether a transition out of `from`
// could leave the system with zero active system.admin holders, and
// thus needs to run EnsureNotLastAdmin before the row updates.
//
// The pessimistic answer is "anything leaving Active" — pending
// can't hold admin (gated by login), and disabled/archived users
// already can't authenticate, so transitions OUT OF active are the
// only ones that can reduce the active-admin count.
//
// (Active→Active is filtered earlier by the idempotency short-circuit
// in the handler; this helper is only consulted when from != to.)
func RequiresLastAdminCheck(from, to UserState) bool {
	return from == UserStateActive &&
		(to == UserStateDisabled || to == UserStateArchived || to == UserStatePending)
}

// FromOpenAPIUpdateStatus maps the openapi AdminUserStatusUpdate
// enum to the typed UserState. Returns (state, true) on a known
// value; (0, false) on garbage. Centralises the mapping so adding
// a 5th state down the line is a single-file edit.
func FromOpenAPIUpdateStatus(s openapi.AdminUserStatusUpdateStatus) (UserState, bool) {
	switch s {
	case openapi.AdminUserStatusUpdateStatusPending:
		return UserStatePending, true
	case openapi.AdminUserStatusUpdateStatusActive:
		return UserStateActive, true
	case openapi.AdminUserStatusUpdateStatusDisabled:
		return UserStateDisabled, true
	case openapi.AdminUserStatusUpdateStatusArchived:
		return UserStateArchived, true
	}
	return 0, false
}

// FromOpenAPIListStatus is the same mapping for the list-filter
// enum (a separate openapi type because the generator emits
// distinct enums per property).
func FromOpenAPIListStatus(s openapi.ListAdminUsersParamsStatus) (UserState, bool) {
	switch s {
	case openapi.ListAdminUsersParamsStatusPending:
		return UserStatePending, true
	case openapi.ListAdminUsersParamsStatusActive:
		return UserStateActive, true
	case openapi.ListAdminUsersParamsStatusDisabled:
		return UserStateDisabled, true
	case openapi.ListAdminUsersParamsStatusArchived:
		return UserStateArchived, true
	}
	return 0, false
}

// ToOpenAPIResultStatus is the inverse — column value → response
// enum on the status-update endpoint. Unknown values defensively
// surface as "disabled" so a bad row can't be reported as "active"
// to the admin UI.
func ToOpenAPIResultStatus(s UserState) openapi.AdminUserStatusResultStatus {
	switch s {
	case UserStatePending:
		return openapi.AdminUserStatusResultStatusPending
	case UserStateActive:
		return openapi.AdminUserStatusResultStatusActive
	case UserStateArchived:
		return openapi.AdminUserStatusResultStatusArchived
	}
	return openapi.AdminUserStatusResultStatusDisabled
}

// ToOpenAPIResultPreviousStatus mirrors ToOpenAPIResultStatus for
// the response's previous_status field (distinct openapi enum).
func ToOpenAPIResultPreviousStatus(s UserState) openapi.AdminUserStatusResultPreviousStatus {
	switch s {
	case UserStatePending:
		return openapi.AdminUserStatusResultPreviousStatusPending
	case UserStateActive:
		return openapi.AdminUserStatusResultPreviousStatusActive
	case UserStateArchived:
		return openapi.AdminUserStatusResultPreviousStatusArchived
	}
	return openapi.AdminUserStatusResultPreviousStatusDisabled
}

// ToOpenAPIListStatus is the inverse mapping for the admin-list
// `status` field on each row.
func ToOpenAPIListStatus(s UserState) openapi.AdminUserStatus {
	switch s {
	case UserStatePending:
		return openapi.AdminUserStatusPending
	case UserStateActive:
		return openapi.AdminUserStatusActive
	case UserStateArchived:
		return openapi.AdminUserStatusArchived
	}
	return openapi.AdminUserStatusDisabled
}
