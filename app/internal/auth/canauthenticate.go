// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.A — assertCanAuthenticate single-gate.
//
// Both login paths (loginViaRegistry + loginInlinePassword) used
// to in-line the user.Approved == 1 + account_expires checks. The
// gates were correct but copy-pasted; a third login path (SSO via
// 1.18, JIT provisioning via 1.18, etc.) would multiply the
// surface where a defect could creep in.
//
// AssertCanAuthenticate is the single function every login path
// now calls. It returns:
//
//   * (nil, nil) — user may proceed; caller issues the session.
//   * (resp, nil) — user is rejected; caller returns resp directly.
//     resp is a typed 401/501 Login*Response with a stable error
//     message; the audit row has already been written.
//   * (nil, err) — infrastructure failure; caller surfaces as 500.
//
// The audit row is written here so every rejection reason is
// uniformly tagged. The returned response carries a human-friendly
// (but reason-aware) error string the frontend renders on the
// login form.
//
// # State-machine awareness
//
// AssertCanAuthenticate routes through the typed user-state
// machine in internal/users. Pending users authenticate (they
// need to view the "waiting for approval" page); disabled and
// archived users do not. This matches UserState.CanAuthenticate
// — the typed predicate is the source of truth for the policy,
// and AssertCanAuthenticate adapts it to the HTTP layer.

package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// userForAuthnGate carries just the columns the gate inspects.
// Both FindUserByRef + FindUserByUsername populate compatible
// shapes; the gate accepts an explicit struct so callers don't
// have to pick which row-type to pass.
type userForAuthnGate struct {
	Ref            int64
	Approved       int64
	AccountExpires pgtype.Timestamptz
}

// AssertCanAuthenticateUser inspects a user's lifecycle state and
// returns a non-nil Login*Response on rejection. The audit row
// is written before this function returns so the rejection is
// always logged regardless of how the caller handles the response.
//
// rec may be nil (some test paths construct Handler without an
// audit recorder); when nil, the audit calls degrade to no-ops
// and only the response is returned.
//
// Returning the typed openapi.LoginResponseObject means the caller
// can `return resp, nil` directly — the strict-server contract
// dispatches the right HTTP status from the response type.
//
// reason is the audit metadata reason code (`"not_approved"`,
// `"account_expired"`, etc.) — kept stable across both login
// paths so log searches don't have to OR the variants.
func AssertCanAuthenticateUser(
	ctx context.Context,
	rec auditWriter,
	httpReq *http.Request,
	username string,
	u userForAuthnGate,
) (openapi.LoginResponseObject, error) {
	state := UserState(u.Approved)
	if !state.CanAuthenticate() {
		// Disabled or archived — same external semantics (401)
		// so an attacker can't probe the difference, but the
		// audit metadata reason carries the typed state for
		// operator triage.
		reason := "not_approved"
		switch state {
		case UserStateDisabled:
			reason = "disabled"
		case UserStateArchived:
			reason = "archived"
		}
		if rec != nil {
			rec.LoginFailed(ctx, httpReq, username, &u.Ref, reason)
		}
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "account is not approved"},
		}, nil
	}
	if u.AccountExpires.Valid && u.AccountExpires.Time.Before(time.Now()) {
		if rec != nil {
			rec.LoginFailed(ctx, httpReq, username, &u.Ref, "account_expired")
		}
		return openapi.Login401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "account has expired"},
		}, nil
	}
	return nil, nil
}

// UserState is the typed user-lifecycle state surfaced from the
// users package. We redeclare it here as a thin alias to avoid an
// auth → users import cycle (users depends on auth for Identity).
// The IntValue contract matches the users.UserState definition
// 1:1 by const value — the column is shared.
//
// Keep in sync with internal/users/userstate.go. The CHECK constraint
// added in migration 00003 is the schema-side load-bearing
// barrier; the typed predicate here is the call-site one.
type UserState int64

const (
	UserStatePending  UserState = 0
	UserStateActive   UserState = 1
	UserStateDisabled UserState = 2
	UserStateArchived UserState = 3
)

// CanAuthenticate mirrors users.UserState.CanAuthenticate. Defined
// locally so the auth package doesn't depend on users. Pending and
// active users may login; disabled and archived may not.
func (s UserState) CanAuthenticate() bool {
	switch s {
	case UserStateActive, UserStatePending:
		return true
	}
	return false
}

// auditWriter is the subset of *audit.Recorder this gate needs.
// Interface form so the gate can be tested without dragging the
// full audit package's DB dependency in. Production wires
// *audit.Recorder; tests substitute a recording fake.
type auditWriter interface {
	LoginFailed(ctx context.Context, req *http.Request, username string, userRef *int64, reason string)
}
