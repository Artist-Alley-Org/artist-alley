// Phase 1.17.A — session-revocation cascade integration tests.
//
// Verifies that SetAdminUserStatus fires the session-revoker
// closure exactly when the transition moves a user OUT of
// UserStateActive (disable / archive / the should-never-happen
// active→pending) and skips it for every other case.

package users_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// recordingRevoker counts calls + remembers the last user_ref so
// tests can assert "fired for the right subject N times".
type recordingRevoker struct {
	calls   atomic.Int32
	lastRef atomic.Int64
}

func (r *recordingRevoker) Revoke(_ context.Context, userRef int64) (int64, error) {
	r.calls.Add(1)
	r.lastRef.Store(userRef)
	return 0, nil // count of sessions doesn't matter here
}

func TestSessionCascade_FiresOnDisable(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	subject := seedUserWithApproved(t, pool, int64(users.UserStateActive))
	admin := seedAdminUserForStateTests(t, pool, "cascade-disable")
	// Sibling admin so the last-admin guard doesn't trip.
	_ = seedAdminUserForStateTests(t, pool, "cascade-disable-sibling")

	rev := &recordingRevoker{}
	h := newHandlerWithRevoker(t, pool, rev)
	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}

	if _, err := h.SetAdminUserStatus(auth.WithIdentity(context.Background(), caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusDisabled},
		}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if rev.calls.Load() != 1 {
		t.Errorf("revoker calls = %d, want 1 on active→disabled", rev.calls.Load())
	}
	if rev.lastRef.Load() != subject {
		t.Errorf("revoker subject = %d, want %d", rev.lastRef.Load(), subject)
	}
}

func TestSessionCascade_FiresOnArchive(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	subject := seedUserWithApproved(t, pool, int64(users.UserStateActive))
	admin := seedAdminUserForStateTests(t, pool, "cascade-archive")
	_ = seedAdminUserForStateTests(t, pool, "cascade-archive-sibling")

	rev := &recordingRevoker{}
	h := newHandlerWithRevoker(t, pool, rev)
	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}

	if _, err := h.SetAdminUserStatus(auth.WithIdentity(context.Background(), caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusArchived},
		}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if rev.calls.Load() != 1 {
		t.Errorf("revoker calls = %d, want 1 on active→archived", rev.calls.Load())
	}
}

func TestSessionCascade_DoesNotFireOnApprove(t *testing.T) {
	// pending → active. User just GAINED auth ability; existing
	// sessions (typically zero, but possibly cookies from a prior
	// active life if the operator un-disabled them) should NOT be
	// killed. The cascade only fires for transitions OUT OF active.
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	subject := seedUserWithApproved(t, pool, int64(users.UserStatePending))
	admin := seedAdminUserForStateTests(t, pool, "cascade-approve")

	rev := &recordingRevoker{}
	h := newHandlerWithRevoker(t, pool, rev)
	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}

	if _, err := h.SetAdminUserStatus(auth.WithIdentity(context.Background(), caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
		}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if rev.calls.Load() != 0 {
		t.Errorf("revoker fired on approve; calls = %d, want 0", rev.calls.Load())
	}
}

func TestSessionCascade_DoesNotFireOnIdempotentNoOp(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	subject := seedUserWithApproved(t, pool, int64(users.UserStateActive))
	admin := seedAdminUserForStateTests(t, pool, "cascade-idem")

	rev := &recordingRevoker{}
	h := newHandlerWithRevoker(t, pool, rev)
	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}

	// active → active = no-op. No cascade.
	if _, err := h.SetAdminUserStatus(auth.WithIdentity(context.Background(), caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
		}); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	if rev.calls.Load() != 0 {
		t.Errorf("revoker fired on idempotent no-op; calls = %d, want 0", rev.calls.Load())
	}
}

func TestSessionCascade_DoesNotFireOnRestore(t *testing.T) {
	// disabled → active is a RESTORE — user is regaining auth
	// ability, not losing it. Existing sessions (which shouldn't
	// exist because they were cascaded when the user was disabled)
	// would be welcome to live on. Cascade does not fire.
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPoolState(t, pwd)
	subject := seedUserWithApproved(t, pool, int64(users.UserStateDisabled))
	admin := seedAdminUserForStateTests(t, pool, "cascade-restore")

	rev := &recordingRevoker{}
	h := newHandlerWithRevoker(t, pool, rev)
	caller := &auth.Identity{UserRef: admin, Capabilities: []string{users.CapApproveUsers}}

	if _, err := h.SetAdminUserStatus(auth.WithIdentity(context.Background(), caller),
		openapi.SetAdminUserStatusRequestObject{
			Ref:  subject,
			Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
		}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rev.calls.Load() != 0 {
		t.Errorf("revoker fired on restore; calls = %d, want 0", rev.calls.Load())
	}
}

func newHandlerWithRevoker(t *testing.T, pool *pgxpool.Pool, rev *recordingRevoker) *users.Handler {
	t.Helper()
	h := users.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h.SetAuditRecorder(&recordingAudit{})
	h.SetSessionRevoker(rev.Revoke)
	return h
}
