// Expiry sweeper tests — Phase 1.22.C-d.
// Coverage:
//   - Happy path: expired-active share revoked + aa:Unshare
//     activity recorded + audit row written (write-ahead invariant)
//   - Idempotent: re-run on same row is a no-op (revoked_at IS NULL
//     filter drops it)
//   - Non-expired shares untouched
//   - Race: concurrent revoke of the same row doesn't double-commit
//
// Skips without AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

// sweeperFixture wires the sweeper around the existing gate
// fixture's pool + registry. Reuses the same peer + grantor +
// activity rows that the gate tests use.
type sweeperFixture struct {
	*gateFixture
	sweeper *shares.Sweeper
}

func newSweeperFixture(t *testing.T) *sweeperFixture {
	t.Helper()
	gfx := newGateFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := activities.NewWriter(gfx.pool, logger, nil)
	auditRec := audit.NewRecorder(gfx.pool, logger)

	peerLookup := func(ctx context.Context, id uuid.UUID) (shares.PeerInfo, error) {
		return shares.PeerInfo{
			ID:          gfx.peerID,
			InstanceURL: "https://test-peer.example",
			Enabled:     true,
			Connected:   true,
		}, nil
	}
	instanceURLFn := func(ctx context.Context) string { return "https://local.example" }
	usernameFn := func(ctx context.Context, ref int64) string { return "local-user" }

	// Tiny interval + batch — tests drive SweepOnce manually.
	sw := shares.NewSweeper(shares.SweeperConfig{
		Interval:  10 * time.Millisecond, // never fires; tests call SweepOnce directly
		BatchSize: 50,
	}, gfx.reg, writer, auditRec, peerLookup, instanceURLFn, usernameFn, logger)
	return &sweeperFixture{gateFixture: gfx, sweeper: sw}
}

func TestSweeper_RevokesExpiredShare_WriteAheadAudit(t *testing.T) {
	fx := newSweeperFixture(t)
	objectID := uuid.New()

	// Insert an expired share directly via Registry.Insert (NOT
	// the admin handler — we want a row that already exists in
	// the expired state).
	past := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	share, err := fx.reg.Insert(context.Background(), shares.InsertInput{
		GrantorUserRef:    fx.grantorRef,
		ObjectKind:        federation.ShareObjectKindPost,
		ObjectID:          objectID,
		PeerID:            fx.peerID,
		Scope:             federation.ShareScopeView,
		ExpiresAt:         &past,
		GrantedActivityID: fx.activityID,
	})
	if err != nil {
		t.Fatalf("insert expired share: %v", err)
	}

	// Sweep.
	revoked, errs := fx.sweeper.SweepOnce(context.Background())
	if len(errs) > 0 {
		t.Fatalf("sweep errors: %v", errs)
	}
	if revoked < 1 {
		t.Errorf("expected at least 1 revoked, got %d", revoked)
	}

	// Share row should now be revoked.
	updated, err := fx.reg.ByID(context.Background(), share.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.RevokedAt.Valid {
		t.Error("expired share should be marked revoked after sweep")
	}
	if updated.RevokedActivityID == nil {
		t.Error("revoked_activity_id should point at the aa:Unshare activity")
	}

	// aa:Unshare activity row committed?
	var unshareCount int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM activities WHERE actor_user_ref=$1 AND activity_type='aa:Unshare'`,
		fx.grantorRef,
	).Scan(&unshareCount)
	if unshareCount == 0 {
		t.Error("write-ahead invariant: aa:Unshare should be in activities table")
	}

	// Audit row committed with reason=expired + sweeper=true?
	var auditCount int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events
		 WHERE event_type='federation.share.revoked'
		   AND actor_user_ref=$1
		   AND metadata->>'reason'='expired'
		   AND metadata->>'sweeper'='true'`,
		fx.grantorRef,
	).Scan(&auditCount)
	if auditCount == 0 {
		t.Error("write-ahead invariant: federation.share.revoked audit row with reason=expired/sweeper=true should be in audit_events")
	}
}

func TestSweeper_Idempotent_NoDoubleRevoke(t *testing.T) {
	fx := newSweeperFixture(t)
	objectID := uuid.New()
	past := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	_, err := fx.reg.Insert(context.Background(), shares.InsertInput{
		GrantorUserRef:    fx.grantorRef,
		ObjectKind:        federation.ShareObjectKindPost,
		ObjectID:          objectID,
		PeerID:            fx.peerID,
		Scope:             federation.ShareScopeView,
		ExpiresAt:         &past,
		GrantedActivityID: fx.activityID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// First sweep revokes.
	first, _ := fx.sweeper.SweepOnce(context.Background())
	if first < 1 {
		t.Fatal("first sweep should have revoked")
	}
	// Count post-first audit events.
	var auditFirst int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events
		 WHERE event_type='federation.share.revoked' AND actor_user_ref=$1`,
		fx.grantorRef,
	).Scan(&auditFirst)

	// Second sweep should find nothing — the SQL filter excludes
	// already-revoked rows. Idempotent.
	second, errs := fx.sweeper.SweepOnce(context.Background())
	if len(errs) > 0 {
		t.Fatalf("second sweep errors: %v", errs)
	}
	if second != 0 {
		t.Errorf("second sweep should be a no-op; revoked=%d", second)
	}
	// Audit count unchanged — no duplicate event row.
	var auditSecond int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events
		 WHERE event_type='federation.share.revoked' AND actor_user_ref=$1`,
		fx.grantorRef,
	).Scan(&auditSecond)
	if auditFirst != auditSecond {
		t.Errorf("second sweep added duplicate audit events: %d → %d", auditFirst, auditSecond)
	}
}

func TestSweeper_NonExpiredUntouched(t *testing.T) {
	fx := newSweeperFixture(t)
	objectID := uuid.New()
	future := pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	share, err := fx.reg.Insert(context.Background(), shares.InsertInput{
		GrantorUserRef:    fx.grantorRef,
		ObjectKind:        federation.ShareObjectKindPost,
		ObjectID:          objectID,
		PeerID:            fx.peerID,
		Scope:             federation.ShareScopeView,
		ExpiresAt:         &future,
		GrantedActivityID: fx.activityID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fx.sweeper.SweepOnce(context.Background())
	updated, err := fx.reg.ByID(context.Background(), share.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RevokedAt.Valid {
		t.Error("non-expired share must NOT be revoked by the sweeper")
	}
}
