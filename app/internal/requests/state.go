// Phase 1.17.E — typed request-state machine.
//
// resource_request.state is TEXT in Postgres with a CHECK
// constraint pinning the legal set to {pending, granted, denied,
// expired}. Callers go through this typed layer rather than
// comparing raw strings: no magic at call sites, no typo'd
// transitions, and ValidateTransition pins the matrix at the
// handler boundary.
//
// # Transition matrix
//
//   pending  → granted | denied             (admin decision)
//   granted  → expired                      (sweeper-time only)
//   denied   → (terminal)
//   expired  → (terminal)
//
// pending is the only non-terminal start state. The transition
// matrix forbids resurrection (denied → pending, expired → granted,
// etc.); admin tooling re-issues with a new resource_request row
// rather than walking a row backwards. Keeps the audit timeline
// linear.

package requests

import (
	"errors"
	"fmt"
)

// RequestState is the typed enumeration of resource_request.state
// values. The string form lands in the column verbatim and is
// what the schema CHECK constraint validates against.
type RequestState string

const (
	// RequestStatePending — created by the requester; awaiting
	// approver decision. The CapabilitySweeper does NOT expire
	// pending rows; that's an explicit operator decision in this
	// MVP scope (auto-deny-on-stale is a polish-phase follow-up).
	RequestStatePending RequestState = "pending"

	// RequestStateGranted — approver decided yes. Wrote a
	// user_capability_grants row with the request_ref back-
	// reference; the sweeper expires both together.
	RequestStateGranted RequestState = "granted"

	// RequestStateDenied — terminal "no". The requester gets a
	// notification with the denial reason; the audit row carries
	// the same reason for operator traceability.
	RequestStateDenied RequestState = "denied"

	// RequestStateExpired — terminal "the grant ran out". Only
	// reachable via the CapabilitySweeper's request-cascade
	// callback when an expires_at-bound grant reaps. Direct
	// pending→expired is forbidden (use Deny for that).
	RequestStateExpired RequestState = "expired"
)

// String renders the state in lowercase form for audit metadata,
// log lines, and the openapi response. Mirrors 1.17.A's
// UserState.String pattern.
func (s RequestState) String() string { return string(s) }

// IsKnown reports whether s is one of the four legal values. Used
// at the boundary (rows read from PG, payloads decoded from JSON)
// to defend against malformed data without panicking — defence in
// depth alongside the schema CHECK.
func (s RequestState) IsKnown() bool {
	switch s {
	case RequestStatePending, RequestStateGranted, RequestStateDenied, RequestStateExpired:
		return true
	}
	return false
}

// IsTerminal reports whether s admits no further transitions.
// granted is technically non-terminal (sweeper can expire it),
// but operators reasoning about "is this request still live?"
// usually want pending as the only non-terminal answer.
//
// Granted IS terminal from the approver's perspective — once they
// decide, the only further movement is the sweeper's
// cascade-expiry, which they don't initiate.
func (s RequestState) IsTerminal() bool {
	switch s {
	case RequestStateDenied, RequestStateExpired:
		return true
	}
	return false
}

// ErrInvalidTransition is returned by ValidateTransition when
// (from, to) isn't in the matrix. Handler maps to HTTP 400 /
// AlreadyDecided (409) per call site.
var ErrInvalidTransition = errors.New("requests: invalid state transition")

// ValidateTransition reports whether moving from → to is permitted.
// Self-transitions are rejected — a no-op decide should 409 rather
// than silently succeed (mirrors 1.17.A's idempotent pattern except
// that requests deliberately reject the same-state re-decide so the
// approver sees what's already there).
//
// Unknown states on either side return ErrInvalidTransition.
func ValidateTransition(from, to RequestState) error {
	if !from.IsKnown() || !to.IsKnown() {
		return fmt.Errorf("%w: unknown state (from=%s, to=%s)",
			ErrInvalidTransition, from, to)
	}
	if from == to {
		return fmt.Errorf("%w: already in state %s", ErrInvalidTransition, from)
	}
	switch from {
	case RequestStatePending:
		// pending → granted / denied (admin decisions)
		// pending → expired is NOT in the matrix; the sweeper
		// only walks granted→expired. A pending request that
		// goes stale stays pending until an admin decides.
		if to == RequestStateGranted || to == RequestStateDenied {
			return nil
		}
	case RequestStateGranted:
		// granted → expired (sweeper-only)
		if to == RequestStateExpired {
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s not permitted", ErrInvalidTransition, from, to)
}
