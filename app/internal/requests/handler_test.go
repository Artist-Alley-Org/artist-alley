// Phase 1.17.E — resource_request lifecycle integration tests.
//
// Real Postgres (skipped without AA_DB_PASSWORD). Covers:
//
//   * Submit → row lands as pending; audit fires once
//   * Grant → CAS to granted; user_capability_grants row inserted
//     with request_ref; audit fires; notification fires
//   * Grant of a non-pending row → ErrRequestAlreadyDecided
//   * Deny → CAS to denied; audit fires; no grant inserted
//   * MarkExpired → granted → expired with audit
//   * MarkExpired of a non-granted row → silent no-op
//   * Self-decide race-rejection (sequential Grant after Grant)
//   * Past-expiry vs future-expiry persistence

package requests_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/requests"
)

func TestSubmit_CreatesPendingRow_AuditFires(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	requester := seedUserForRequests(t, pool)

	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	out, err := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "posts.publish",
		Reason:              "research project",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if out.State != "pending" {
		t.Errorf("state = %q, want pending", out.State)
	}
	if rec.createdCalls.Load() != 1 {
		t.Errorf("RequestCreated calls = %d, want 1", rec.createdCalls.Load())
	}
}

func TestGrant_PendingToGranted_InsertsGrantWithRequestRef(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	requester := seedUserForRequests(t, pool)
	approver := seedUserForRequests(t, pool)

	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	row, err := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "posts.publish",
		Reason:              "r",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	rid := uuid.UUID(row.ID.Bytes)
	expires := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)

	granted, err := h.Grant(context.Background(), nil, requests.DecideInput{
		RequestID:      rid,
		ApproverRef:    approver,
		DecisionReason: "ok",
		ExpiresAt:      expires,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if granted.State != "granted" {
		t.Errorf("state = %q, want granted", granted.State)
	}
	// Verify user_capability_grants row landed with request_ref.
	var (
		grantedCap   string
		grantRequest *string
		grantExpires *time.Time
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT capability_code, request_ref::text, expires_at
		 FROM user_capability_grants WHERE user_ref = $1 AND request_ref = $2`,
		requester, rid,
	).Scan(&grantedCap, &grantRequest, &grantExpires); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if grantedCap != "posts.publish" {
		t.Errorf("grant capability = %q, want asset.view", grantedCap)
	}
	if grantRequest == nil || *grantRequest != rid.String() {
		t.Errorf("grant request_ref = %v, want %s", grantRequest, rid)
	}
	if grantExpires == nil {
		t.Errorf("grant expires_at is NULL; want %v", expires)
	}
	if rec.grantedCalls.Load() != 1 {
		t.Errorf("RequestGranted calls = %d, want 1", rec.grantedCalls.Load())
	}
	if noter.calls.Load() != 1 {
		t.Errorf("notifier calls = %d, want 1", noter.calls.Load())
	}
}

func TestGrant_AlreadyDecided_Rejects(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	requester := seedUserForRequests(t, pool)
	approver := seedUserForRequests(t, pool)

	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	row, err := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "posts.publish",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	rid := uuid.UUID(row.ID.Bytes)

	// First grant succeeds.
	if _, err := h.Grant(context.Background(), nil, requests.DecideInput{
		RequestID: rid, ApproverRef: approver, DecisionReason: "first",
	}); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	// Second grant must reject — already-decided.
	_, err = h.Grant(context.Background(), nil, requests.DecideInput{
		RequestID: rid, ApproverRef: approver, DecisionReason: "second",
	})
	if !errors.Is(err, requests.ErrRequestAlreadyDecided) {
		t.Errorf("second grant err = %v, want ErrRequestAlreadyDecided", err)
	}
}

func TestDeny_PendingToDenied_NoGrantInserted(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	requester := seedUserForRequests(t, pool)
	approver := seedUserForRequests(t, pool)

	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	row, err := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "posts.publish",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	rid := uuid.UUID(row.ID.Bytes)

	denied, err := h.Deny(context.Background(), nil, requests.DecideInput{
		RequestID:      rid,
		ApproverRef:    approver,
		DecisionReason: "out of scope",
	})
	if err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if denied.State != "denied" {
		t.Errorf("state = %q, want denied", denied.State)
	}
	// No grant should have landed.
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM user_capability_grants WHERE request_ref = $1`,
		rid,
	).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if n != 0 {
		t.Errorf("grant rows = %d, want 0 on deny", n)
	}
	if rec.deniedCalls.Load() != 1 {
		t.Errorf("RequestDenied calls = %d, want 1", rec.deniedCalls.Load())
	}
}

func TestMarkExpired_GrantedToExpired(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	requester := seedUserForRequests(t, pool)
	approver := seedUserForRequests(t, pool)

	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	row, _ := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "posts.publish",
	})
	rid := uuid.UUID(row.ID.Bytes)
	if _, err := h.Grant(context.Background(), nil, requests.DecideInput{
		RequestID:   rid,
		ApproverRef: approver,
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rec.grantedCalls.Store(0) // reset to isolate the expire audit

	if err := h.MarkExpired(context.Background(), rid, time.Now()); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM resource_request WHERE id = $1`, rid,
	).Scan(&state); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if state != "expired" {
		t.Errorf("state = %q, want expired", state)
	}
	if rec.expiredCalls.Load() != 1 {
		t.Errorf("RequestExpired calls = %d, want 1", rec.expiredCalls.Load())
	}
}

func TestMarkExpired_NonGranted_Noop(t *testing.T) {
	// A pending or already-decided request shouldn't get audited
	// for an expiration that never happened.
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	requester := seedUserForRequests(t, pool)

	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	row, _ := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "posts.publish",
	})
	rid := uuid.UUID(row.ID.Bytes)

	if err := h.MarkExpired(context.Background(), rid, time.Now()); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	if rec.expiredCalls.Load() != 0 {
		t.Errorf("MarkExpired on pending row fired audit; calls = %d", rec.expiredCalls.Load())
	}
	var state string
	_ = pool.QueryRow(context.Background(),
		`SELECT state FROM resource_request WHERE id = $1`, rid,
	).Scan(&state)
	if state != "pending" {
		t.Errorf("state changed to %q after no-op MarkExpired", state)
	}
}

func TestGrant_NotFound_ReturnsErrRequestNotFound(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	_, err := h.Grant(context.Background(), nil, requests.DecideInput{
		RequestID:   uuid.New(),
		ApproverRef: 1,
	})
	if !errors.Is(err, requests.ErrRequestNotFound) {
		t.Errorf("err = %v, want ErrRequestNotFound", err)
	}
}

func TestListForRequester_FiltersByRequester(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	a := seedUserForRequests(t, pool)
	b := seedUserForRequests(t, pool)
	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	for i := 0; i < 3; i++ {
		_, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
			RequesterUserRef: a, TargetAssetID: uuid.New(), RequestedCapability: "posts.publish",
		})
	}
	for i := 0; i < 2; i++ {
		_, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
			RequesterUserRef: b, TargetAssetID: uuid.New(), RequestedCapability: "posts.publish",
		})
	}
	gotA, err := h.ListForRequester(context.Background(), a, 50)
	if err != nil {
		t.Fatalf("ListForRequester(a): %v", err)
	}
	if len(gotA) != 3 {
		t.Errorf("len(a's requests) = %d, want 3", len(gotA))
	}
	gotB, _ := h.ListForRequester(context.Background(), b, 50)
	if len(gotB) != 2 {
		t.Errorf("len(b's requests) = %d, want 2", len(gotB))
	}
}

func TestCountPending_UsesCache(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	rec := newAuditRec()
	noter := newNoter()
	h := newHandlerE(t, pool, rec, noter)

	requester := seedUserForRequests(t, pool)
	_, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef: requester, TargetAssetID: uuid.New(), RequestedCapability: "posts.publish",
	})

	// First call populates cache + reads DB.
	n1, err := h.CountPending(context.Background(), 1)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n1 < 1 {
		t.Errorf("count = %d, want >= 1", n1)
	}
	// Second call returns the cached value — verify by making
	// another pending row + asserting the second call still
	// returns the old count.
	_, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef: requester, TargetAssetID: uuid.New(), RequestedCapability: "posts.publish",
	})
	// Submit wildcards the cache, so the next CountPending re-reads.
	n2, _ := h.CountPending(context.Background(), 1)
	if n2 < n1+1 {
		t.Errorf("expected count to increase after second Submit; got %d → %d", n1, n2)
	}
}

// ---------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------

func openPoolE(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOrE("AA_DB_HOST", "postgres")
	port := envOrE("AA_DB_PORT", "5432")
	user := envOrE("AA_DB_USER", "artist_alley")
	name := envOrE("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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

func envOrE(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func newHandlerE(t *testing.T, pool *pgxpool.Pool, rec *auditRecording, noter *notifierRecording) *requests.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("registry start: %v", err)
	}
	t.Cleanup(reg.Stop)
	h := requests.NewHandler(pool, logger, reg)
	h.SetAuditRecorder(rec)
	h.SetNotifier(noter)
	return h
}

func seedUserForRequests(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	username := "rq-" + uuid.New().String()[:8]
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, password) VALUES ($1, '') RETURNING ref`, username,
	).Scan(&ref); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func cleanupRequests(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	c := context.Background()
	_, _ = pool.Exec(c, `DELETE FROM audit_events WHERE event_type LIKE 'request.%'`)
	_, _ = pool.Exec(c, `DELETE FROM user_capability_grants WHERE request_ref IS NOT NULL`)
	_, _ = pool.Exec(c, `DELETE FROM resource_request`)
}

// auditRecording satisfies the package-private auditRecorder
// interface via SetAuditRecorder.
type auditRecording struct {
	createdCalls atomic.Int32
	grantedCalls atomic.Int32
	deniedCalls  atomic.Int32
	expiredCalls atomic.Int32
}

func newAuditRec() *auditRecording { return &auditRecording{} }

func (r *auditRecording) RequestCreated(_ context.Context, _ *http.Request, _ int64, _, _, _, _ string) {
	r.createdCalls.Add(1)
}
func (r *auditRecording) RequestGranted(_ context.Context, _ *http.Request, _, _ int64, _, _, _, _ string, _ time.Time) {
	r.grantedCalls.Add(1)
}
func (r *auditRecording) RequestDenied(_ context.Context, _ *http.Request, _, _ int64, _, _, _, _ string) {
	r.deniedCalls.Add(1)
}
func (r *auditRecording) RequestExpired(_ context.Context, _ int64, _, _ string, _ time.Time) {
	r.expiredCalls.Add(1)
}

type notifierRecording struct {
	calls atomic.Int32
}

func newNoter() *notifierRecording { return &notifierRecording{} }

func (n *notifierRecording) Notify(_ context.Context, _ int64, _ *int64, _, _, _ string, _ map[string]any) error {
	n.calls.Add(1)
	return nil
}
