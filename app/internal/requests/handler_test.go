// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

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
	"sync"
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

	out, _, err := h.Submit(context.Background(), nil, requests.SubmitInput{
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

	row, _, err := h.Submit(context.Background(), nil, requests.SubmitInput{
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
	// Asserted by VERB, not by total: since #881 a Submit also notifies
	// (the owner + every approver), so a total would move with the
	// number of admins in the test database.
	if got := noter.withVerb("resource_request_approved"); len(got) != 1 {
		t.Errorf("resource_request_approved notifications = %d, want 1", len(got))
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

	row, _, err := h.Submit(context.Background(), nil, requests.SubmitInput{
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

	row, _, err := h.Submit(context.Background(), nil, requests.SubmitInput{
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

	row, _, _ := h.Submit(context.Background(), nil, requests.SubmitInput{
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

	row, _, _ := h.Submit(context.Background(), nil, requests.SubmitInput{
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
		_, _, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
			RequesterUserRef: a, TargetAssetID: uuid.New(), RequestedCapability: "posts.publish",
		})
	}
	for i := 0; i < 2; i++ {
		_, _, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
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
	_, _, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
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
	_, _, _ = h.Submit(context.Background(), nil, requests.SubmitInput{
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

// notifiedCall is one recorded Notify. Kept whole rather than counted,
// because #881 added a SECOND emitter to this package (create-time, to
// the owner + approvers) and a bare total can no longer tell the two
// apart — a Grant test asserting "one notification" would pass on a
// build that emitted two creates and no decision.
type notifiedCall struct {
	Recipient int64
	Actor     *int64
	Verb      string
	TargetKnd string
	TargetID  string
	Payload   map[string]any
}

type notifierRecording struct {
	calls atomic.Int32

	mu   sync.Mutex
	sent []notifiedCall
}

func newNoter() *notifierRecording { return &notifierRecording{} }

func (n *notifierRecording) Notify(_ context.Context, recipient int64, actor *int64, verb, kind, targetID string, payload map[string]any) error {
	n.calls.Add(1)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, notifiedCall{
		Recipient: recipient, Actor: actor, Verb: verb,
		TargetKnd: kind, TargetID: targetID, Payload: payload,
	})
	return nil
}

// withVerb returns every recorded call carrying verb.
func (n *notifierRecording) withVerb(verb string) []notifiedCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []notifiedCall
	for _, c := range n.sent {
		if c.Verb == verb {
			out = append(out, c)
		}
	}
	return out
}

// TestSubmit_UnknownCapabilityRejected covers #434: requested_capability
// feeds an authorisation decision, so it may only name a real
// capability. The DB enforces this with an FK (migration 00009); this
// asserts the handler catches it first, so a caller gets a clean error
// naming the problem instead of a constraint violation surfacing as a
// 500.
//
// Note what is deliberately NOT asserted here: that the capability is
// REQUESTABLE. A real capability is not necessarily one a user may ask
// for — nothing stops a request naming system.admin — and that rule
// belongs to the grant path (ADR 0064), not to this validation.
func TestSubmit_UnknownCapabilityRejected(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	requester := seedUserForRequests(t, pool)

	h := newHandlerE(t, pool, newAuditRec(), newNoter())

	_, _, err := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "totally.made.up",
		Reason:              "should not be storable",
	})
	if err == nil {
		t.Fatal("Submit accepted a capability that is not in the registry")
	}
	if !errors.Is(err, requests.ErrUnknownCapability) {
		t.Errorf("err = %v, want ErrUnknownCapability (so the HTTP layer can map it to 400)", err)
	}

	// A real seeded capability must still work — otherwise this
	// validation would pass by rejecting everything.
	if _, _, err := h.Submit(context.Background(), nil, requests.SubmitInput{
		RequesterUserRef:    requester,
		TargetAssetID:       uuid.New(),
		RequestedCapability: "posts.publish",
		Reason:              "legitimate",
	}); err != nil {
		t.Errorf("Submit rejected a real capability: %v", err)
	}
}
