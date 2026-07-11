// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.B — lock-it-in cascade contract test.
//
// The recordingRevoker test in session_cascade_test.go pins the
// CALL surface (handler fires the closure on the right transitions).
// This test pins the ACTUAL CONTRACT end-to-end:
//
//   * seed an active user with two real session rows (via the real
//     SessionManager.Issue path — same call site that production
//     uses on /auth/login)
//   * transition the user to disabled via SetAdminUserStatus
//   * verify both session rows have revoked_at set (no active
//     sessions remain for the subject)
//
// If a future refactor breaks the cascade — say, swaps the
// SessionRevokerFn closure for a no-op stub — the unit test still
// passes because it uses a fake revoker. This test fails. That's
// the load-bearing barrier the brief asked for.

package users_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

func TestCascade_DisableUser_RevokesRealSessions(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	subject := seedUserWithApproved(t, pool, int64(users.UserStateActive))
	admin := seedAdminUserForStateTests(t, pool, "real-cascade")
	// Sibling admin so the subject's own admin removal (if subject
	// IS an admin) wouldn't trip the last-admin invariant — for
	// THIS test the subject isn't an admin, but we keep the helper
	// to mirror the production-style invariant guarantees.
	_ = seedAdminUserForStateTests(t, pool, "real-cascade-sibling")

	// Seed two real sessions through the SessionManager — same call
	// site /auth/login uses. Two so the test catches a "first row
	// revoked but second leaks through" regression.
	sm := auth.NewSessionManager(pool)
	req1 := httptest.NewRequest("POST", "/auth/login", nil)
	if _, _, err := sm.Issue(ctx, subject, req1); err != nil {
		t.Fatalf("seed session 1: %v", err)
	}
	req2 := httptest.NewRequest("POST", "/auth/login", nil)
	if _, _, err := sm.Issue(ctx, subject, req2); err != nil {
		t.Fatalf("seed session 2: %v", err)
	}

	active := countActiveSessions(t, pool, subject)
	if active != 2 {
		t.Fatalf("pre-cascade active session count = %d, want 2", active)
	}

	// Wire the real SessionRevoker — same closure api.go installs
	// in production.
	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h.SetAuditRecorder(&recordingAudit{})
	h.SetSessionRevoker(sm.RevokeAllForUser)

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}
	if _, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusDisabled},
		}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if remaining := countActiveSessions(t, pool, subject); remaining != 0 {
		t.Errorf("post-disable active session count = %d, want 0 (cascade should revoke all)", remaining)
	}
}

func TestCascade_ArchiveUser_RevokesRealSessions(t *testing.T) {
	// Same shape as the disable test — archive must also revoke.
	// Mirrors the operator-locked constraint: any state out of
	// active kills sessions.
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	subject := seedUserWithApproved(t, pool, int64(users.UserStateActive))
	admin := seedAdminUserForStateTests(t, pool, "real-archive")
	_ = seedAdminUserForStateTests(t, pool, "real-archive-sibling")

	sm := auth.NewSessionManager(pool)
	req := httptest.NewRequest("POST", "/auth/login", nil)
	if _, _, err := sm.Issue(ctx, subject, req); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h.SetAuditRecorder(&recordingAudit{})
	h.SetSessionRevoker(sm.RevokeAllForUser)

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}
	if _, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusArchived},
		}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if remaining := countActiveSessions(t, pool, subject); remaining != 0 {
		t.Errorf("post-archive active session count = %d, want 0", remaining)
	}
}

func TestCascade_ApproveUser_DoesNotRevokeSessions(t *testing.T) {
	// Negative test. Pending → active is a GAIN of auth ability;
	// existing sessions (rare but possible if the user was
	// re-pended after a prior active life) should survive.
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	subject := seedUserWithApproved(t, pool, int64(users.UserStatePending))
	admin := seedAdminUserForStateTests(t, pool, "no-cascade")

	sm := auth.NewSessionManager(pool)
	req := httptest.NewRequest("POST", "/auth/login", nil)
	if _, _, err := sm.Issue(ctx, subject, req); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h.SetAuditRecorder(&recordingAudit{})
	h.SetSessionRevoker(sm.RevokeAllForUser)

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}
	if _, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
		}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	if remaining := countActiveSessions(t, pool, subject); remaining != 1 {
		t.Errorf("post-approve active session count = %d, want 1 (approve should NOT cascade-revoke)", remaining)
	}
}

func countActiveSessions(t *testing.T, pool *pgxpool.Pool, userRef int64) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE user_ref = $1 AND revoked_at IS NULL`,
		userRef,
	).Scan(&n); err != nil {
		t.Fatalf("countActiveSessions: %v", err)
	}
	return n
}
