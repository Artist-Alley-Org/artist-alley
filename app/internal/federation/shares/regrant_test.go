// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1586 - an expired share must not block a new grant, and a 409 must
// always carry the exact conflicting row.
//
// federation_shares_active_uniq_idx holds a grant tuple's slot for every
// row with revoked_at IS NULL, expired ones included. Before the fix a
// regrant after expiry answered 409 until the hourly sweeper revoked the
// old row, and the 409 body was a zero-valued share because the lookup
// behind it skipped expired rows. Reproduced live in #1233's three
// instance test (rev345, S6).
//
// Everything here goes through the production admin handler and the
// production sweeper on a real database. Skips without AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// --- helpers ------------------------------------------------------------

func grantBody(objectID, peerID uuid.UUID, expires *time.Time) *openapi.FederationShareCreate {
	return &openapi.FederationShareCreate{
		ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
		ObjectId:   objectID,
		PeerId:     peerID,
		Scope:      openapi.FederationShareCreateScope(federation.ShareScopeView),
		ExpiresAt:  expires,
	}
}

func grant(t *testing.T, h *shares.AdminHandler, ctx context.Context, body *openapi.FederationShareCreate) openapi.GrantFederationShareResponseObject {
	t.Helper()
	resp, err := h.GrantFederationShare(ctx, openapi.GrantFederationShareRequestObject{Body: body})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return resp
}

func mustCreated(t *testing.T, label string, resp openapi.GrantFederationShareResponseObject) openapi.FederationShare {
	t.Helper()
	c, ok := resp.(openapi.GrantFederationShare201JSONResponse)
	if !ok {
		t.Fatalf("%s: want 201, got %T %+v", label, resp, resp)
	}
	return openapi.FederationShare(c)
}

func mustConflictWith(t *testing.T, label string, resp openapi.GrantFederationShareResponseObject, want uuid.UUID, grantor int64) {
	t.Helper()
	c, ok := resp.(openapi.GrantFederationShare409JSONResponse)
	if !ok {
		t.Fatalf("%s: want 409, got %T %+v", label, resp, resp)
	}
	if c.Id == (uuid.UUID{}) {
		t.Fatalf("%s: 409 with a zero-valued share body: %+v", label, c)
	}
	if c.Id != want || c.GrantorUserRef != grantor {
		t.Fatalf("%s: 409 names share %s of grantor %d, want %s of grantor %d", label, c.Id, c.GrantorUserRef, want, grantor)
	}
}

// expireNow moves a share's expiry into the past by the database clock,
// the way time passing would. Nothing in production writes expires_at
// on expiry, so this is the only state change that happens.
func expireNow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE federation_shares SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire: %v", err)
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, q string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func liveRowsForTuple(t *testing.T, fx *adminFixture, objectID uuid.UUID) int {
	return scalar[int](t, fx.pool, `SELECT count(*) FROM federation_shares
		WHERE grantor_user_ref=$1 AND object_id=$2 AND peer_id=$3 AND revoked_at IS NULL`,
		fx.grantorRef, objectID, fx.peerID)
}

func revokedAuditsFor(t *testing.T, fx *adminFixture, shareID uuid.UUID) int {
	return scalar[int](t, fx.pool, `SELECT count(*) FROM audit_events
		WHERE event_type='federation.share.revoked' AND metadata->>'share_id' = $1`, shareID.String())
}

// --- tests ----------------------------------------------------------------

func TestRegrant_AfterRealExpiry_SucceedsWithoutTheSweeper(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := fx.ctx()
	objectID := uuid.New()

	// A share that really expires (no clock tampering): 1.5s TTL.
	soon := time.Now().Add(1500 * time.Millisecond)
	first := mustCreated(t, "first grant", grant(t, fx.handler, ctx, grantBody(objectID, fx.peerID, &soon)))
	for !scalar[bool](t, fx.pool, `SELECT NOW() > expires_at FROM federation_shares WHERE id=$1`, first.Id) {
		time.Sleep(100 * time.Millisecond)
	}

	// No sweeper has run. The regrant must succeed.
	second := mustCreated(t, "regrant after expiry", grant(t, fx.handler, ctx, grantBody(objectID, fx.peerID, nil)))
	if second.Id == first.Id {
		t.Fatalf("regrant returned the expired share %s", first.Id)
	}

	// The old row is kept, closed the way the sweeper closes one: an
	// aa:Unshare by the grantor and a federation.share.revoked audit
	// with reason "expired".
	var (
		revoked       bool
		unshareID     uuid.UUID
		unshareType   string
		unshareActor  int64
		unshareCreate time.Time
	)
	if err := fx.pool.QueryRow(context.Background(), `
		SELECT s.revoked_at IS NOT NULL, a.id, a.activity_type, a.actor_user_ref, a.created_at
		  FROM federation_shares s JOIN activities a ON a.id = s.revoked_activity_id
		 WHERE s.id = $1`, first.Id).Scan(&revoked, &unshareID, &unshareType, &unshareActor, &unshareCreate); err != nil {
		t.Fatalf("old row's revoke activity: %v", err)
	}
	if !revoked || unshareType != "aa:Unshare" || unshareActor != fx.grantorRef {
		t.Fatalf("old row: revoked=%v activity=%s actor=%d; want revoked by an aa:Unshare from the grantor", revoked, unshareType, unshareActor)
	}
	var reason, retiredBy, corr string
	if err := fx.pool.QueryRow(context.Background(), `
		SELECT metadata->>'reason', COALESCE(metadata->>'retired_by',''), metadata->>'correlation_id'
		  FROM audit_events WHERE event_type='federation.share.revoked' AND metadata->>'share_id'=$1`,
		first.Id.String()).Scan(&reason, &retiredBy, &corr); err != nil {
		t.Fatalf("old row's revoke audit: %v", err)
	}
	if reason != "expired" || retiredBy != "regrant" || corr != unshareID.String() {
		t.Fatalf("revoke audit reason=%q retired_by=%q correlation=%q; want expired/regrant/%s", reason, retiredBy, corr, unshareID)
	}

	// The new grant has its own aa:Share and its own granted audit, and
	// both name the share id the row is stored under.
	var shareActivity uuid.UUID
	var shareCreate time.Time
	var payloadShareID string
	if err := fx.pool.QueryRow(context.Background(), `
		SELECT a.id, a.created_at, a.payload->>'share_id'
		  FROM federation_shares s JOIN activities a ON a.id = s.granted_activity_id
		 WHERE s.id = $1 AND a.activity_type = 'aa:Share'`, second.Id).Scan(&shareActivity, &shareCreate, &payloadShareID); err != nil {
		t.Fatalf("new row's aa:Share: %v", err)
	}
	if payloadShareID != second.Id.String() {
		t.Fatalf("aa:Share payload names share %s, the row is %s", payloadShareID, second.Id)
	}
	if n := scalar[int](t, fx.pool, `SELECT count(*) FROM audit_events
		WHERE event_type='federation.share.granted' AND metadata->>'share_id'=$1 AND metadata->>'correlation_id'=$2`,
		second.Id.String(), shareActivity.String()); n != 1 {
		t.Fatalf("granted audits naming the new share and its aa:Share: %d, want 1", n)
	}
	if !unshareCreate.Before(shareCreate) {
		t.Fatalf("ledger order: aa:Unshare at %v must precede the new aa:Share at %v", unshareCreate, shareCreate)
	}

	// History kept, one live grant.
	if total := scalar[int](t, fx.pool, `SELECT count(*) FROM federation_shares WHERE grantor_user_ref=$1 AND object_id=$2`,
		fx.grantorRef, objectID); total != 2 {
		t.Fatalf("rows for the tuple: %d, want 2 (expired kept + new)", total)
	}
	if n := liveRowsForTuple(t, fx, objectID); n != 1 {
		t.Fatalf("live rows: %d, want 1", n)
	}
	dec, err := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID: fx.peerID, ObjectKind: federation.ShareObjectKindPost, ObjectID: objectID,
		RequiredScope: federation.ShareScopeView, PeerEnabled: true, PeerConnected: true,
	})
	if err != nil || !dec.Allowed || dec.Share == nil || dec.Share.ID != second.Id {
		t.Fatalf("gate after regrant: %+v err=%v; want allowed by %s", dec, err, second.Id)
	}
	// A later sweep has nothing left to do for this tuple.
	if n := revokedAuditsFor(t, fx, first.Id); n != 1 {
		t.Fatalf("revoked audits for the expired share: %d, want exactly 1", n)
	}
}

func TestGrant_DuplicateLiveGrant_409CarriesTheExactRow(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := fx.ctx()
	objectID := uuid.New()
	first := mustCreated(t, "first", grant(t, fx.handler, ctx, grantBody(objectID, fx.peerID, nil)))
	before := scalar[int](t, fx.pool, `SELECT count(*) FROM activities WHERE actor_user_ref=$1 AND activity_type='aa:Share'`, fx.grantorRef)
	mustConflictWith(t, "duplicate", grant(t, fx.handler, ctx, grantBody(objectID, fx.peerID, nil)), first.Id, fx.grantorRef)
	after := scalar[int](t, fx.pool, `SELECT count(*) FROM activities WHERE actor_user_ref=$1 AND activity_type='aa:Share'`, fx.grantorRef)
	if after != before {
		t.Fatalf("a 409 left %d aa:Share activities behind", after-before)
	}
	// A share that has NOT expired yet still blocks: only expiry frees
	// the slot.
	later := time.Now().Add(time.Hour)
	other := uuid.New()
	timed := mustCreated(t, "timed", grant(t, fx.handler, ctx, grantBody(other, fx.peerID, &later)))
	mustConflictWith(t, "duplicate of an unexpired timed share", grant(t, fx.handler, ctx, grantBody(other, fx.peerID, nil)), timed.Id, fx.grantorRef)
}

func TestGrant_409NeverNamesAnotherGrantorsShare(t *testing.T) {
	fx := newAdminFixture(t)
	objectID := uuid.New()
	var otherRef int64
	if err := fx.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1,'Other Grantor',1) RETURNING ref`,
		"share-other-"+uuid.NewString()[:8]).Scan(&otherRef); err != nil {
		t.Fatalf("seed second grantor: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM federation_shares WHERE grantor_user_ref=$1`, otherRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref=$1`, otherRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM "user" WHERE ref=$1`, otherRef)
	})
	otherCtx := auth.WithIdentity(context.Background(), &auth.Identity{
		UserRef: otherRef, Username: "other", AuthMethod: "session", Capabilities: []string{"system.admin"},
	})

	// Same object, peer and recipient, different grantors: the index
	// keys on the grantor, so both may hold a live share.
	b := mustCreated(t, "other grantor", grant(t, fx.handler, otherCtx, grantBody(objectID, fx.peerID, nil)))
	a := mustCreated(t, "fixture grantor", grant(t, fx.handler, fx.ctx(), grantBody(objectID, fx.peerID, nil)))
	mustConflictWith(t, "fixture grantor again", grant(t, fx.handler, fx.ctx(), grantBody(objectID, fx.peerID, nil)), a.Id, fx.grantorRef)
	mustConflictWith(t, "other grantor again", grant(t, fx.handler, otherCtx, grantBody(objectID, fx.peerID, nil)), b.Id, otherRef)
}

func TestRegrant_ConcurrentAfterExpiry_OneWinnerExactConflicts(t *testing.T) {
	fx := newAdminFixture(t)
	objectID := uuid.New()
	old := mustCreated(t, "old", grant(t, fx.handler, fx.ctx(), grantBody(objectID, fx.peerID, nil)))
	expireNow(t, fx.pool, old.Id)

	const n = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		resps []openapi.GrantFederationShareResponseObject
		errs  []error
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
				Body: grantBody(objectID, fx.peerID, nil),
			})
			mu.Lock()
			resps, errs = append(resps, r), append(errs, err)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	var winner uuid.UUID
	created := 0
	for i, r := range resps {
		if errs[i] != nil {
			t.Fatalf("concurrent regrant %d: error %v", i, errs[i])
		}
		if c, ok := r.(openapi.GrantFederationShare201JSONResponse); ok {
			created++
			winner = c.Id
		}
	}
	if created != 1 {
		t.Fatalf("concurrent regrants: %d created, want exactly 1", created)
	}
	for i, r := range resps {
		if _, ok := r.(openapi.GrantFederationShare201JSONResponse); ok {
			continue
		}
		mustConflictWith(t, "loser", r, winner, fx.grantorRef)
		_ = i
	}
	if got := liveRowsForTuple(t, fx, objectID); got != 1 {
		t.Fatalf("live rows after concurrent regrant: %d, want 1", got)
	}
	// The expired row was closed exactly once: one aa:Unshare, one audit.
	if got := revokedAuditsFor(t, fx, old.Id); got != 1 {
		t.Fatalf("revoked audits for the expired share: %d, want exactly 1", got)
	}
	if got := scalar[int](t, fx.pool, `SELECT count(*) FROM activities
		WHERE activity_type='aa:Unshare' AND payload->>'share_id'=$1`, old.Id.String()); got != 1 {
		t.Fatalf("aa:Unshare activities for the expired share: %d, want exactly 1", got)
	}
}

func TestRegrant_RacingTheSweeper_ClosesTheExpiredRowOnce(t *testing.T) {
	fx := newAdminFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	peerLookup := func(ctx context.Context, id uuid.UUID) (shares.PeerInfo, error) {
		return shares.PeerInfo{ID: fx.peerID, InstanceURL: "https://test-peer.example", Enabled: true, Connected: true}, nil
	}
	sw := shares.NewSweeper(shares.SweeperConfig{Interval: time.Hour, BatchSize: 50}, fx.reg,
		activities.NewWriter(fx.pool, logger, nil), audit.NewRecorder(fx.pool, logger), peerLookup,
		func(context.Context) string { return "https://local.example" },
		func(context.Context, int64) string { return "local-user" }, logger)

	for round := 0; round < 5; round++ {
		objectID := uuid.New()
		old := mustCreated(t, "old", grant(t, fx.handler, fx.ctx(), grantBody(objectID, fx.peerID, nil)))
		expireNow(t, fx.pool, old.Id)

		var wg sync.WaitGroup
		var resp openapi.GrantFederationShareResponseObject
		var gerr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, _ = sw.SweepOnce(context.Background()) }()
		go func() {
			defer wg.Done()
			<-start
			resp, gerr = fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
				Body: grantBody(objectID, fx.peerID, nil),
			})
		}()
		close(start)
		wg.Wait()
		if gerr != nil {
			t.Fatalf("round %d: regrant racing the sweeper: %v", round, gerr)
		}
		mustCreated(t, "regrant racing the sweeper", resp)
		if got := revokedAuditsFor(t, fx, old.Id); got != 1 {
			t.Fatalf("round %d: revoked audits for the expired share: %d, want exactly 1", round, got)
		}
		if got := liveRowsForTuple(t, fx, objectID); got != 1 {
			t.Fatalf("round %d: live rows: %d, want 1", round, got)
		}
	}
}

// TestRegrant_TwoInstances_GateAndOutboxFollowTheLiveRow uses two
// independent cache registries (two app processes): process 1 serves the
// admin API, process 2 only answers the inbox gate. Through expiry,
// regrant and revoke, process 2's (cached) gate and the outbox resolver
// must only ever authorise the live row.
func TestRegrant_TwoInstances_GateAndOutboxFollowTheLiveRow(t *testing.T) {
	fx := newAdminFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	newProc := func() (*cache.Registry, *shares.Registry) {
		c := cache.NewRegistry(fx.pool, logger)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { c.Stop(); cancel() })
		if err := c.Start(ctx); err != nil {
			t.Fatalf("cache start: %v", err)
		}
		return c, shares.NewRegistry(fx.pool, logger, c)
	}
	_, reg1 := newProc()
	reg2Cache, reg2 := newProc()
	peerLookup := func(ctx context.Context, id uuid.UUID) (shares.PeerInfo, error) {
		return shares.PeerInfo{ID: fx.peerID, InstanceURL: "https://test-peer.example", Enabled: true, Connected: true}, nil
	}
	h1 := shares.NewAdminHandler(reg1, activities.NewWriter(fx.pool, logger, nil), audit.NewRecorder(fx.pool, logger),
		func(context.Context, federation.ShareObjectKind, uuid.UUID, *auth.Identity) (bool, error) {
			return true, nil
		},
		peerLookup, func(context.Context) string { return "https://local.example" },
		func(context.Context, int64) string { return "local-user" })
	resolver := outbox.NewResolver(fx.pool, reg2Cache, func(context.Context) bool { return false })

	objectID := uuid.New()
	gate := func() shares.AccessDecision {
		t.Helper()
		d, err := reg2.CanPeerAccess(context.Background(), shares.AccessRequest{
			PeerID: fx.peerID, ObjectKind: federation.ShareObjectKindPost, ObjectID: objectID,
			RequiredScope: federation.ShareScopeView, PeerEnabled: true, PeerConnected: true,
		})
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		return d
	}
	recipients := func() []outbox.Recipient {
		t.Helper()
		r, err := resolver.Resolve(context.Background(), outbox.Input{
			// A verb routed by the explicit-share tier: since #1611 a Like
			// has its own route (the liked object's owner, under a received
			// grant), so it no longer exercises this one.
			Verb: "Announce", TargetKind: "post", TargetID: objectID, AuthorRef: fx.grantorRef,
			Visibility: outbox.VisibilityExplicitShare, Sensitivity: outbox.SensitivityPublic,
		})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return r.Recipients
	}
	waitGate := func(label string, ok func(shares.AccessDecision) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			d := gate()
			if ok(d) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: process 2 gate still %+v after 5s", label, d)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// A real TTL set at grant time, as production does it: expires_at
	// never changes afterwards, and the warm snapshot carries it.
	soon := time.Now().Add(1500 * time.Millisecond)
	old := mustCreated(t, "grant", grant(t, h1, fx.ctx(), grantBody(objectID, fx.peerID, &soon)))
	waitGate("after grant", func(d shares.AccessDecision) bool { return d.Allowed && d.Share != nil && d.Share.ID == old.Id })

	// Expiry: nothing is written that a cache could hear, and process
	// 2's snapshot is warm, yet neither the gate nor the outbox may
	// authorise the expired row.
	for !scalar[bool](t, fx.pool, `SELECT NOW() > expires_at FROM federation_shares WHERE id=$1`, old.Id) {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the gate reads the Go clock; let it pass expires_at too
	if d := gate(); d.Allowed {
		t.Fatalf("process 2 gate authorised an EXPIRED share: %+v", d)
	}
	if r := recipients(); len(r) != 0 {
		t.Fatalf("outbox resolved recipients from an EXPIRED share: %+v", r)
	}

	// Regrant on process 1: process 2 follows to the NEW row.
	fresh := mustCreated(t, "regrant", grant(t, h1, fx.ctx(), grantBody(objectID, fx.peerID, nil)))
	waitGate("after regrant", func(d shares.AccessDecision) bool { return d.Allowed && d.Share != nil && d.Share.ID == fresh.Id })
	if r := recipients(); len(r) != 1 || r[0].PeerID != fx.peerID {
		t.Fatalf("outbox after regrant: %+v, want exactly the peer", r)
	}

	// Revoke on process 1: process 2 stops authorising.
	if resp, err := h1.RevokeFederationShare(fx.ctx(), openapi.RevokeFederationShareRequestObject{Id: fresh.Id}); err != nil {
		t.Fatalf("revoke: %v", err)
	} else if _, ok := resp.(openapi.RevokeFederationShare204Response); !ok {
		t.Fatalf("revoke: %T", resp)
	}
	waitGate("after revoke", func(d shares.AccessDecision) bool { return !d.Allowed })
	if r := recipients(); len(r) != 0 {
		t.Fatalf("outbox after revoke: %+v, want none", r)
	}
}
