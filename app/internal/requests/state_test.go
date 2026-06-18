// Phase 1.17.E — typed RequestState matrix unit tests.
//
// Pure-Go (no Postgres). Integration coverage for state writes +
// the lifecycle handlers lives in handler_test.go.

package requests

import (
	"errors"
	"testing"
)

func TestRequestState_String(t *testing.T) {
	for _, c := range []struct {
		s    RequestState
		want string
	}{
		{RequestStatePending, "pending"},
		{RequestStateGranted, "granted"},
		{RequestStateDenied, "denied"},
		{RequestStateExpired, "expired"},
	} {
		if got := c.s.String(); got != c.want {
			t.Errorf("String(%v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestRequestState_IsKnown(t *testing.T) {
	for _, s := range []RequestState{
		RequestStatePending, RequestStateGranted, RequestStateDenied, RequestStateExpired,
	} {
		if !s.IsKnown() {
			t.Errorf("IsKnown(%s) = false, want true", s)
		}
	}
	for _, s := range []RequestState{"", "approved", "rejected", "PENDING", "garbage"} {
		if s.IsKnown() {
			t.Errorf("IsKnown(%q) = true, want false", s)
		}
	}
}

func TestRequestState_IsTerminal(t *testing.T) {
	terminal := []RequestState{RequestStateDenied, RequestStateExpired}
	nonTerminal := []RequestState{RequestStatePending, RequestStateGranted}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("IsTerminal(%s) = false, want true", s)
		}
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("IsTerminal(%s) = true, want false", s)
		}
	}
}

func TestValidateTransition_ValidPaths(t *testing.T) {
	valid := []struct{ from, to RequestState }{
		{RequestStatePending, RequestStateGranted}, // approve
		{RequestStatePending, RequestStateDenied},  // reject
		{RequestStateGranted, RequestStateExpired}, // sweeper cascade
	}
	for _, c := range valid {
		if err := ValidateTransition(c.from, c.to); err != nil {
			t.Errorf("ValidateTransition(%s, %s) = %v, want nil", c.from, c.to, err)
		}
	}
}

func TestValidateTransition_BackwardsRejected(t *testing.T) {
	// Resurrection is forbidden — admins reissue via a new
	// resource_request row, not by walking an existing row
	// backwards. Keeps the audit timeline linear.
	invalid := []struct{ from, to RequestState }{
		{RequestStateGranted, RequestStatePending},
		{RequestStateGranted, RequestStateDenied},
		{RequestStateDenied, RequestStatePending},
		{RequestStateDenied, RequestStateGranted},
		{RequestStateDenied, RequestStateExpired},
		{RequestStateExpired, RequestStatePending},
		{RequestStateExpired, RequestStateGranted},
		{RequestStateExpired, RequestStateDenied},
	}
	for _, c := range invalid {
		err := ValidateTransition(c.from, c.to)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("ValidateTransition(%s, %s) = %v, want ErrInvalidTransition", c.from, c.to, err)
		}
	}
}

func TestValidateTransition_PendingToExpired_Rejected(t *testing.T) {
	// The sweeper walks granted→expired only. A pending request
	// that goes stale stays pending; auto-deny-on-stale is a
	// future polish-phase decision.
	err := ValidateTransition(RequestStatePending, RequestStateExpired)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("pending→expired = %v, want ErrInvalidTransition", err)
	}
}

func TestValidateTransition_SelfRejected(t *testing.T) {
	// Self-transition rejected — 1.17.A's idempotent-self-transition
	// pattern doesn't apply here; an approver re-deciding the same
	// outcome should see the existing decision (409 at the API),
	// not silently succeed.
	for _, s := range []RequestState{
		RequestStatePending, RequestStateGranted, RequestStateDenied, RequestStateExpired,
	} {
		err := ValidateTransition(s, s)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("ValidateTransition(%s, %s) = %v, want ErrInvalidTransition", s, s, err)
		}
	}
}

func TestValidateTransition_UnknownState(t *testing.T) {
	for _, c := range []struct{ from, to RequestState }{
		{"garbage", RequestStatePending},
		{RequestStatePending, "garbage"},
		{"", RequestStatePending},
	} {
		err := ValidateTransition(c.from, c.to)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("ValidateTransition(%q, %q) = %v, want ErrInvalidTransition", c.from, c.to, err)
		}
	}
}
