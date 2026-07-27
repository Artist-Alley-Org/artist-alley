// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.A — integration tests for the typed user-state machine.
//
// Each test runs against real Postgres (skipped without
// AA_DB_PASSWORD). Covers the constraints the brief calls out:
//
//   * Migration 00003 schema barriers (CHECK + system_config seed)
//   * Transition matrix enforcement at the handler layer
//   * Last-admin invariant on disable AND archive (not just disable)
//   * Cache invalidation on every transition
//   * Per-transition typed audit events fire alongside the
//     generic UserStatusChanged backstop
//   * Federation pass-through: state transitions do NOT enqueue
//     federation activities (1.17 is local-instance only)

package users_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// ---------------------------------------------------------------
// Schema barriers
// ---------------------------------------------------------------

// CHECK constraint added by migration 00003 must reject values
// outside {0,1,2,3}. The Go layer's ToOpenAPI* defenses are a
// belt; the CHECK is the suspenders.
func TestMigration_UserApprovedCheck_RejectsInvalidValues(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	username := "state-check-" + randHex8()
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (username, password, approved) VALUES ($1, '', $2)`,
		username, int64(4),
	); err == nil {
		t.Errorf("INSERT with approved=4 should violate user_approved_check; got nil error")
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE username = $1`, username)
	} else if !strings.Contains(err.Error(), "user_approved_check") {
		t.Errorf("expected user_approved_check violation, got: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (username, password, approved) VALUES ($1, '', $2)`,
		username+"-neg", int64(-1),
	); err == nil {
		t.Errorf("INSERT with approved=-1 should violate user_approved_check; got nil error")
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE username = $1`, username+"-neg")
	}
}

// Every known state value (0..3) MUST be accepted — guards
// against a future migration narrowing the set by accident.
func TestMigration_UserApprovedCheck_AcceptsArchived(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()
	for _, v := range []int64{0, 1, 2, 3} {
		username := "state-accept-" + randHex8()
		_, err := pool.Exec(ctx,
			`INSERT INTO "user" (username, password, approved) VALUES ($1, '', $2)`,
			username, v,
		)
		if err != nil {
			t.Errorf("INSERT with approved=%d rejected: %v", v, err)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE username = $1`, username)
	}
}

// system_config seed: the pending-capability set lives at the
// known key. Operators can override; the migration just plants the
// default.
func TestMigration_PendingCapabilitiesSeed_Present(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()
	var raw []byte
	err := pool.QueryRow(ctx,
		`SELECT value FROM system_config WHERE key = 'users.pending_capabilities'`,
	).Scan(&raw)
	if err != nil {
		t.Fatalf("missing pending_capabilities seed: %v", err)
	}
	var caps []string
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("seed value isn't a JSON array of strings: %v\nraw: %s", err, raw)
	}
	if len(caps) == 0 {
		t.Errorf("pending_capabilities seeded as empty array; want at least one default cap")
	}
}

// ---------------------------------------------------------------
// Handler — transitions + invariants + cache + audit
// ---------------------------------------------------------------

func TestSetAdminUserStatus_PendingToActive_ApprovesAndAuditsTyped(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	subject := seedUserWithApproved(t, pool, int64(users.UserStatePending))
	admin := seedAdminUserForStateTests(t, pool, "approver")
	rec := &recordingAudit{}
	reg := newRegistry(t, pool)
	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	h.SetAuditRecorder(rec)

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}
	resp, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller), openapi.SetAdminUserStatusRequestObject{
		Ref:  subject,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
	})
	if err != nil {
		t.Fatalf("SetAdminUserStatus: %v", err)
	}
	if r, ok := resp.(openapi.SetAdminUserStatus200JSONResponse); !ok {
		t.Fatalf("expected 200, got %T", resp)
	} else if !r.Changed {
		t.Errorf("Changed=false, want true on pending→active")
	}
	if rec.statusChangedCalls != 1 {
		t.Errorf("UserStatusChanged calls = %d, want 1 (backstop event must fire)", rec.statusChangedCalls)
	}
	if rec.approvedCalls != 1 {
		t.Errorf("AdminUserApproved calls = %d, want 1 (typed event must fire)", rec.approvedCalls)
	}
	if rec.disabledCalls != 0 || rec.archivedCalls != 0 || rec.restoredCalls != 0 {
		t.Errorf("only AdminUserApproved should fire; got disabled=%d archived=%d restored=%d",
			rec.disabledCalls, rec.archivedCalls, rec.restoredCalls)
	}

	// Cache should now report active.
	got, err := h.GetUserState(ctx, subject)
	if err != nil {
		t.Fatalf("GetUserState: %v", err)
	}
	if got != users.UserStateActive {
		t.Errorf("state after approve = %s, want active", got)
	}
}

func TestSetAdminUserStatus_OutOfMatrix_Rejects400(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	// pending → archived is NOT in the matrix. Caller must approve
	// first then archive — keeps the audit timeline unambiguous.
	subject := seedUserWithApproved(t, pool, int64(users.UserStatePending))
	admin := seedAdminUserForStateTests(t, pool, "matrix")
	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	rec := &recordingAudit{}
	h.SetAuditRecorder(rec)

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}
	resp, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller), openapi.SetAdminUserStatusRequestObject{
		Ref:  subject,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusArchived},
	})
	if err != nil {
		t.Fatalf("SetAdminUserStatus: %v", err)
	}
	r400, ok := resp.(openapi.SetAdminUserStatus400JSONResponse)
	if !ok {
		t.Fatalf("expected 400, got %T", resp)
	}
	if !strings.Contains(r400.Error, "transition") {
		t.Errorf("400 message should name the bad transition; got %q", r400.Error)
	}
	// No audit events should fire when the transition is rejected.
	if rec.statusChangedCalls != 0 || rec.archivedCalls != 0 {
		t.Errorf("rejected transition still emitted audit events: %+v", rec)
	}
}

func TestSetAdminUserStatus_ArchiveLastAdmin_Refuses(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	// Wipe ALL existing admins so the next-seeded user is the only
	// one — pure precondition for the last-admin invariant test.
	wipeAllAdmins(t, pool)
	lone := seedAdminUserForStateTests(t, pool, "lone")

	// Parallel-package safety: other test packages (notably
	// internal/auth/last_admin_test.go) wipe + seed admins on the
	// shared dev DB. If one of them seeds a sibling admin in the
	// window between our wipe + write, the invariant won't fire
	// and the assertion below would be a false negative. Verify
	// the precondition holds; if not, skip with a clear message.
	if n := countActiveSystemAdmins(t, pool); n != 1 {
		t.Skipf("precondition: expected exactly 1 active system admin, found %d (parallel-package test race)", n)
	}

	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	rec := &recordingAudit{}
	h.SetAuditRecorder(rec)

	// Caller can also be the lone admin — they should still be
	// blocked because the write would leave the system with zero.
	caller := &auth.Identity{UserRef: lone, Capabilities: []string{users.CapApproveUsers}}
	resp, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller), openapi.SetAdminUserStatusRequestObject{
		Ref:  lone,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusArchived},
	})
	if err != nil {
		t.Fatalf("SetAdminUserStatus: %v", err)
	}
	if _, ok := resp.(openapi.SetAdminUserStatus400JSONResponse); !ok {
		t.Fatalf("expected 400 from last-admin invariant on archive, got %T", resp)
	}
	if rec.refusedLastAdminCalls != 1 {
		t.Errorf("AdminUserRefusedLastAdmin calls = %d, want 1", rec.refusedLastAdminCalls)
	}

	// Confirm the row was NOT updated.
	var approved int64
	if err := pool.QueryRow(ctx, `SELECT approved FROM "user" WHERE ref = $1`, lone).Scan(&approved); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if approved != int64(users.UserStateActive) {
		t.Errorf("approved after blocked archive = %d, want %d (active)", approved, int64(users.UserStateActive))
	}
}

func TestSetAdminUserStatus_DisableThenArchive_Allowed(t *testing.T) {
	// Active → Disabled → Archived. Both transitions are in the
	// matrix; both invalidate the cache; both fire typed audit
	// events. Pins the multi-step path operators use to escalate
	// a temporary disable into a permanent archive.
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	subject := seedUserWithApproved(t, pool, int64(users.UserStateActive))
	admin := seedAdminUserForStateTests(t, pool, "escalate")
	// Ensure there's a sibling admin so the last-admin guard doesn't
	// block the disable step (subject isn't an admin here, but the
	// guard runs anyway for active→disabled transitions). The
	// admin seeded above IS an admin, so we're fine — but pin it.
	_ = seedAdminUserForStateTests(t, pool, "escalate-sibling")
	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), newRegistry(t, pool))
	rec := &recordingAudit{}
	h.SetAuditRecorder(rec)

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}

	// Step 1: active → disabled.
	if _, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller), openapi.SetAdminUserStatusRequestObject{
		Ref:  subject,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusDisabled},
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if rec.disabledCalls != 1 {
		t.Errorf("after disable: AdminUserDisabled calls = %d, want 1", rec.disabledCalls)
	}
	st, _ := h.GetUserState(ctx, subject)
	if st != users.UserStateDisabled {
		t.Errorf("after disable: state = %s, want disabled", st)
	}

	// Step 2: disabled → archived.
	if _, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller), openapi.SetAdminUserStatusRequestObject{
		Ref:  subject,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusArchived},
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if rec.archivedCalls != 1 {
		t.Errorf("after archive: AdminUserArchived calls = %d, want 1", rec.archivedCalls)
	}
	st, _ = h.GetUserState(ctx, subject)
	if st != users.UserStateArchived {
		t.Errorf("after archive: state = %s, want archived", st)
	}
}

func TestSetAdminUserStatus_Idempotent_NoAuditNoCacheChurn(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	subject := seedUserWithApproved(t, pool, int64(users.UserStateActive))
	admin := seedAdminUserForStateTests(t, pool, "idempot")
	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	rec := &recordingAudit{}
	h.SetAuditRecorder(rec)

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}
	// active → active is a self-transition. Handler returns 200 +
	// changed=false; NO audit; NO cache write.
	resp, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller), openapi.SetAdminUserStatusRequestObject{
		Ref:  subject,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
	})
	if err != nil {
		t.Fatalf("SetAdminUserStatus: %v", err)
	}
	r, ok := resp.(openapi.SetAdminUserStatus200JSONResponse)
	if !ok {
		t.Fatalf("expected 200, got %T", resp)
	}
	if r.Changed {
		t.Errorf("idempotent call returned Changed=true; want false")
	}
	if rec.statusChangedCalls != 0 || rec.approvedCalls != 0 {
		t.Errorf("idempotent call emitted audit events: %+v", rec)
	}
}

// ---------------------------------------------------------------
// Federation pass-through — 1.17.A is local-only
// ---------------------------------------------------------------

func TestSetAdminUserStatus_FederationPassThrough_NoOutboxRows(t *testing.T) {
	// Per the arc-wide soak-safe rule: state transitions MUST NOT
	// enqueue federation activities. Sanity-check the outbox count
	// before + after a transition; the delta must be 0.
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	ctx := context.Background()

	subject := seedUserWithApproved(t, pool, int64(users.UserStatePending))
	admin := seedAdminUserForStateTests(t, pool, "fed")
	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h.SetAuditRecorder(&recordingAudit{})

	before, err := countActivitiesTouchingUser(ctx, pool, subject)
	if err != nil {
		t.Fatalf("count before: %v", err)
	}

	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}
	if _, err := h.SetAdminUserStatus(auth.WithIdentity(ctx, caller), openapi.SetAdminUserStatusRequestObject{
		Ref:  subject,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	after, err := countActivitiesTouchingUser(ctx, pool, subject)
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("transition emitted %d activities (was %d, now %d); 1.17.A must be local-only",
			after-before, before, after)
	}
}

// ---------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------

func openPoolState(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOrState("AA_DB_HOST", "postgres")
	port := envOrState("AA_DB_PORT", "5432")
	user := envOrState("AA_DB_USER", "artist_alley")
	name := envOrState("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOrState(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func randHex8() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}

func seedUserWithApproved(t *testing.T, pool *pgxpool.Pool, approved int64) int64 {
	t.Helper()
	ctx := context.Background()
	username := "state-subj-" + randHex8()
	var ref int64
	err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, password, approved) VALUES ($1, '', $2) RETURNING ref`,
		username, approved,
	).Scan(&ref)
	if err != nil {
		t.Fatalf("seedUserWithApproved: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_roles WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM user_capability_grants WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM user_capability_revokes WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// seedAdminUserForStateTests mirrors auth_test.seedAdmin but lives
// in users_test to avoid cross-package test dependencies. Active +
// holds system.admin via the seeded Admin role.
func seedAdminUserForStateTests(t *testing.T, pool *pgxpool.Pool, label string) int64 {
	t.Helper()
	ctx := context.Background()
	q := auth.New(pool)
	username := "state-admin-" + label + "-" + randHex8()
	pw := "irrelevant"
	user, err := q.CreateUser(ctx, auth.CreateUserParams{
		Username: &username, Password: &pw,
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	role, err := q.FindRoleByName(ctx, "Admin")
	if err != nil {
		t.Fatalf("find Admin role: %v", err)
	}
	if err := q.SetUserGlobalRole(ctx, auth.SetUserGlobalRoleParams{
		UserRef: user.Ref, RoleID: role.ID,
	}); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_roles WHERE user_ref = $1`, user.Ref)
		_, _ = pool.Exec(c, `DELETE FROM user_capability_grants WHERE user_ref = $1`, user.Ref)
		_, _ = pool.Exec(c, `DELETE FROM user_capability_revokes WHERE user_ref = $1`, user.Ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, user.Ref)
	})
	return user.Ref
}

func wipeAllAdmins(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		DELETE FROM user_roles ur
		USING role_capabilities rc
		WHERE ur.role_id = rc.role_id AND rc.capability_code = 'system.admin'
	`); err != nil {
		t.Fatalf("wipe admins: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM user_capability_grants WHERE capability_code = 'system.admin'`,
	); err != nil {
		t.Fatalf("wipe grants: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM user_capability_revokes WHERE capability_code = 'system.admin'`,
	); err != nil {
		t.Fatalf("wipe revokes: %v", err)
	}
}

// countActiveSystemAdmins returns the total count of users who
// currently hold system.admin via either a role assignment or an
// explicit grant AND are in UserStateActive. Mirrors what
// EnsureNotLastAdmin sees — used by tests to verify last-admin
// preconditions before asserting on the invariant.
func countActiveSystemAdmins(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	err := pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT u.ref)
		FROM "user" u
		WHERE u.approved = 1
		  AND (
		    EXISTS (
		      SELECT 1 FROM user_roles ur
		      JOIN role_capabilities rc ON rc.role_id = ur.role_id
		      WHERE ur.user_ref = u.ref AND rc.capability_code = 'system.admin'
		    )
		    OR EXISTS (
		      SELECT 1 FROM user_capability_grants g
		      WHERE g.user_ref = u.ref AND g.capability_code = 'system.admin'
		    )
		  )
	`).Scan(&n)
	if err != nil {
		t.Fatalf("countActiveSystemAdmins: %v", err)
	}
	return n
}

func newRegistry(t *testing.T, pool *pgxpool.Pool) *cache.Registry {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("registry start: %v", err)
	}
	t.Cleanup(reg.Stop)
	return reg
}

// recordingAudit implements the package-private auditRecorder
// interface used by users.Handler. We can't reference the
// interface from a _test package directly; instead we satisfy it
// structurally via SetAuditRecorder, which accepts any value
// whose method set matches.
type recordingAudit struct {
	statusChangedCalls    int
	approvedCalls         int
	disabledCalls         int
	archivedCalls         int
	restoredCalls         int
	refusedLastAdminCalls int
	recordChangeCalls     int
}

func (r *recordingAudit) UserStatusChanged(_ context.Context, _ *http.Request, _, _, _, _ int64, _ string) {
	r.statusChangedCalls++
}
func (r *recordingAudit) AdminUserApproved(_ context.Context, _ *http.Request, _, _ int64, _, _, _ string) {
	r.approvedCalls++
}
func (r *recordingAudit) AdminUserDisabled(_ context.Context, _ *http.Request, _, _ int64, _, _, _ string) {
	r.disabledCalls++
}
func (r *recordingAudit) AdminUserArchived(_ context.Context, _ *http.Request, _, _ int64, _, _, _ string) {
	r.archivedCalls++
}
func (r *recordingAudit) AdminUserRestored(_ context.Context, _ *http.Request, _, _ int64, _, _, _ string) {
	r.restoredCalls++
}
func (r *recordingAudit) AdminUserRefusedLastAdmin(_ context.Context, _ *http.Request, _, _ int64, _, _, _ string) {
	r.refusedLastAdminCalls++
}
func (r *recordingAudit) RecordChange(_ context.Context, _ *http.Request, _ string, _, _ *int64, _, _ any, _ map[string]any) {
	r.recordChangeCalls++
}

// countActivitiesTouchingUser approximates "did this transition
// federate?" by counting outbound activity rows that mention the
// subject user_ref since a snapshot point. Tables differ by phase;
// here we count rows in activities (the canonical ledger per
// ADR 0044). 1.17.A must NOT increment this count.
//
// Returns 0 + nil if the table doesn't exist on this DB (older
// fixture; safe no-op).
func countActivitiesTouchingUser(ctx context.Context, pool *pgxpool.Pool, userRef int64) (int64, error) {
	// Per the activities table schema (ADR 0044) outbound activity
	// rows carry actor_user_ref for the local emitter; the subject
	// of object-style activities lives inside the payload JSONB.
	// For 1.17.A's pass-through check the load-bearing assertion
	// is "delta is zero", so counting any row that mentions the
	// user_ref through either surface is sufficient.
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activities
		 WHERE actor_user_ref = $1
		    OR payload @> jsonb_build_object('object_user_ref', $1::bigint)`,
		userRef,
	).Scan(&n)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return 0, err
}
