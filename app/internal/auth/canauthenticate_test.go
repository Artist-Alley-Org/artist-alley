// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.A — assertCanAuthenticate single-gate unit tests.
//
// Pure-Go (no Postgres). The integration coverage (login through
// the registry path with a real user row + a real audit recorder)
// lives in handler_test.go.

package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// recordingAudit captures every LoginFailed call so tests can
// assert reason codes without standing up the full audit pipeline.
type recordingAudit struct {
	calls []recordedAuditCall
}

type recordedAuditCall struct {
	username string
	userRef  *int64
	reason   string
}

func (r *recordingAudit) LoginFailed(_ context.Context, _ *http.Request, username string, userRef *int64, reason string) {
	r.calls = append(r.calls, recordedAuditCall{username: username, userRef: userRef, reason: reason})
}

func TestAssertCanAuthenticateUser_Active_AllowsThrough(t *testing.T) {
	rec := &recordingAudit{}
	resp, err := AssertCanAuthenticateUser(context.Background(), rec, nil, "alice", userForAuthnGate{
		Ref: 1, Approved: int64(UserStateActive),
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp != nil {
		t.Errorf("resp = %T, want nil (active users pass through)", resp)
	}
	if len(rec.calls) != 0 {
		t.Errorf("active user emitted %d audit calls; want 0", len(rec.calls))
	}
}

func TestAssertCanAuthenticateUser_Pending_AllowsThrough(t *testing.T) {
	// Pending users CAN authenticate so they can view the
	// "waiting for approval" page. Brief calls this out
	// explicitly — the restricted capability set (seeded in
	// migration 00003) determines what they can do once
	// signed in.
	rec := &recordingAudit{}
	resp, err := AssertCanAuthenticateUser(context.Background(), rec, nil, "alice", userForAuthnGate{
		Ref: 1, Approved: int64(UserStatePending),
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp != nil {
		t.Errorf("resp = %T, want nil (pending users authenticate, just with restricted caps)", resp)
	}
}

func TestAssertCanAuthenticateUser_Disabled_RejectsWithTypedReason(t *testing.T) {
	rec := &recordingAudit{}
	resp, err := AssertCanAuthenticateUser(context.Background(), rec, nil, "alice", userForAuthnGate{
		Ref: 1, Approved: int64(UserStateDisabled),
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if _, ok := resp.(openapi.Login401JSONResponse); !ok {
		t.Errorf("resp = %T, want Login401JSONResponse", resp)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(rec.calls))
	}
	if rec.calls[0].reason != "disabled" {
		t.Errorf("audit reason = %q, want %q", rec.calls[0].reason, "disabled")
	}
}

func TestAssertCanAuthenticateUser_Archived_RejectsWithTypedReason(t *testing.T) {
	rec := &recordingAudit{}
	resp, err := AssertCanAuthenticateUser(context.Background(), rec, nil, "alice", userForAuthnGate{
		Ref: 1, Approved: int64(UserStateArchived),
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if _, ok := resp.(openapi.Login401JSONResponse); !ok {
		t.Errorf("resp = %T, want Login401JSONResponse", resp)
	}
	if rec.calls[0].reason != "archived" {
		t.Errorf("audit reason = %q, want %q", rec.calls[0].reason, "archived")
	}
}

func TestAssertCanAuthenticateUser_ExpiredAccount_Rejects(t *testing.T) {
	// Active + account_expires in the past = reject. The state-
	// machine check passes but the expiry gate fires next.
	rec := &recordingAudit{}
	past := time.Now().Add(-24 * time.Hour)
	resp, err := AssertCanAuthenticateUser(context.Background(), rec, nil, "alice", userForAuthnGate{
		Ref:            1,
		Approved:       int64(UserStateActive),
		AccountExpires: pgtype.Timestamptz{Time: past, Valid: true},
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if _, ok := resp.(openapi.Login401JSONResponse); !ok {
		t.Errorf("resp = %T, want Login401JSONResponse", resp)
	}
	if rec.calls[0].reason != "account_expired" {
		t.Errorf("audit reason = %q, want %q", rec.calls[0].reason, "account_expired")
	}
}

func TestAssertCanAuthenticateUser_FutureExpiry_PassesThrough(t *testing.T) {
	// Active + account_expires in the future = pass.
	rec := &recordingAudit{}
	future := time.Now().Add(24 * time.Hour)
	resp, err := AssertCanAuthenticateUser(context.Background(), rec, nil, "alice", userForAuthnGate{
		Ref:            1,
		Approved:       int64(UserStateActive),
		AccountExpires: pgtype.Timestamptz{Time: future, Valid: true},
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp != nil {
		t.Errorf("resp = %T, want nil (future expiry should pass)", resp)
	}
}

func TestAssertCanAuthenticateUser_NilAuditRecorder_Safe(t *testing.T) {
	// Some test paths construct Handler without an audit recorder
	// (early test fixtures that predate the auditRecorder
	// interface). The gate must not panic.
	resp, err := AssertCanAuthenticateUser(context.Background(), nil, nil, "alice", userForAuthnGate{
		Ref: 1, Approved: int64(UserStateDisabled),
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if _, ok := resp.(openapi.Login401JSONResponse); !ok {
		t.Errorf("resp = %T, want Login401JSONResponse", resp)
	}
}

func TestUserState_CanAuthenticate_PinsPolicy(t *testing.T) {
	// Mirror of users.UserState.CanAuthenticate — the policy is
	// authoritative here too (avoiding the auth → users import
	// cycle). Pin the bijection so a future change can't accidentally
	// flip one without the other.
	cases := []struct {
		s    UserState
		want bool
	}{
		{UserStatePending, true},
		{UserStateActive, true},
		{UserStateDisabled, false},
		{UserStateArchived, false},
		{UserState(99), false},
	}
	for _, c := range cases {
		if got := c.s.CanAuthenticate(); got != c.want {
			t.Errorf("CanAuthenticate(%d) = %v, want %v", int64(c.s), got, c.want)
		}
	}
}
