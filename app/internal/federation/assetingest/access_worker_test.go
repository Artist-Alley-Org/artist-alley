// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: the bounded access-copy worker (access_worker.go, migration
// 00105) against a live database, the fs storage backend, the production
// casfetch client and inbound grant record, and the origin stub. Pinned:
//
//   - Worker.Run, wired with the production client, receives every access
//     copy of a completed version, with no change to the original path;
//     a Fetcher that cannot recheck access requests runs no access work;
//   - an original import completes while an access transfer is stalled;
//   - per-file outcomes: a transient failure is retried and recorded once
//     its attempts are spent, a permanent one at once; other files and the
//     record are untouched;
//   - a lapsed claim writes nothing (fenced), and its pin is released;
//   - a decided copy is not fetched again and gets no second pin;
//   - no access-copy grant or no profile: nothing admitted, no contact;
//   - a refusal at the final HEAD, a revoke there, or a downgrade at
//     recording records the failure and holds nothing;
//   - supersession or withdrawal while a copy is in flight leaves no file
//     result and no pin;
//   - the outstanding, concurrency and discovery bounds hold across
//     workers; a stopped attempt is released and retried.
//
// Skips without AA_DB_PASSWORD.

package assetingest_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/cas"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// hookedFetcher is the production client with callbacks around the access
// steps (tests only).
type hookedFetcher struct {
	c             *casfetch.Client
	afterFetch    func()
	beforeRecheck func()
	afterRecheck  func()
	fetches       atomic.Int32
	rechecks      atomic.Int32
}

func (h *hookedFetcher) Fetch(ctx context.Context, req casfetch.Request) (*casfetch.Result, error) {
	res, err := h.c.Fetch(ctx, req)
	if req.Class == cas.ClassAccess {
		h.fetches.Add(1)
		if h.afterFetch != nil {
			h.afterFetch()
		}
	}
	return res, err
}

func (h *hookedFetcher) RecheckAccess(ctx context.Context, req casfetch.Request) error {
	h.rechecks.Add(1)
	if h.beforeRecheck != nil {
		h.beforeRecheck()
	}
	err := h.c.RecheckAccess(ctx, req)
	if h.afterRecheck != nil {
		h.afterRecheck()
	}
	return err
}

// stalledFetcher is a transfer that never finishes on its own: it waits
// for its context to end, so only a cancellation stops it (tests only).
type stalledFetcher struct {
	started chan struct{}
	ended   chan error
}

func (s *stalledFetcher) Fetch(ctx context.Context, _ casfetch.Request) (*casfetch.Result, error) {
	close(s.started)
	<-ctx.Done()
	s.ended <- ctx.Err()
	return nil, ctx.Err()
}

func (s *stalledFetcher) RecheckAccess(context.Context, casfetch.Request) error { return nil }

// fetchOnly is an older Fetcher: it cannot recheck access requests.
type fetchOnly struct{ c *casfetch.Client }

func (f fetchOnly) Fetch(ctx context.Context, req casfetch.Request) (*casfetch.Result, error) {
	return f.c.Fetch(ctx, req)
}

func accessWorkerConfig() assetingest.WorkerConfig {
	return assetingest.WorkerConfig{Concurrency: 2, Interval: 50 * time.Millisecond,
		AccessConcurrency: 4, AccessGlobalConcurrency: 4, AccessMaxOutstanding: 16, AccessDiscoveryPage: 64,
		AccessMaxAttempts: 3, AccessInterval: 50 * time.Millisecond}
}

// service builds a Service like the fixture's with fetcher (and cfg).
func (x *accessFx) service(fetcher assetingest.Fetcher, cfg ...assetingest.Config) *assetingest.Service {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var c assetingest.Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return assetingest.New(assetingest.Deps{Pool: x.pool, Storage: x.svc, Peers: peers{pool: x.pool}, Grants: x.grants,
		Fetcher: fetcher, Audit: audit.NewRecorder(x.pool, logger), Logger: logger, Config: c})
}

func (x *accessFx) worker(svc *assetingest.Service, cfg assetingest.WorkerConfig) *assetingest.Worker {
	return assetingest.NewWorker(svc, cfg, func() casfetch.Signer { return x.signer })
}

func (x *accessFx) workRows() int {
	return x.count(`SELECT count(*) FROM federation_received_access_work WHERE received_asset_id = $1`, x.id)
}

func (x *accessFx) accessFiles(state string) int {
	return x.count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1 AND role = 'access' AND state = $2`, x.id, state)
}

func (x *accessFx) exec(q string, args ...any) {
	x.t.Helper()
	if _, err := x.pool.Exec(context.Background(), q, args...); err != nil {
		x.t.Fatal(err)
	}
}

// eventually waits (bounded) for cond, failing with what.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func outcomes(rs []assetingest.AccessResult) map[int]string {
	m := map[int]string{}
	for _, r := range rs {
		m[r.Position] = r.Outcome
	}
	return m
}

func (x *accessFx) wantNoAccessPins(hashes ...string) {
	x.t.Helper()
	for _, h := range hashes {
		if n := x.pins(h, casfetch.IngestPinType); n != 0 {
			x.t.Fatalf("hash %s: %d ingest pins left", h[:12], n)
		}
	}
}

// Worker.Run with the production client: every access copy of the
// completed version, nothing else changed.
func TestAccessWorker_ProductionPathVerifies(t *testing.T) {
	x := newAccessFx(t, true)
	before := x.snapshot(x.id, -1)
	w := x.worker(x.ingest, accessWorkerConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	eventually(t, "the three access copies verified", func() bool { return x.accessFiles("verified") == 3 })
	cancel()
	<-done
	if x.workRows() != 0 || x.pins(x.hB, assetingest.PinSubjectType) != 1 || x.pins(x.hC, assetingest.PinSubjectType) != 1 {
		t.Fatalf("work rows %d, record pins B %d C %d", x.workRows(), x.pins(x.hB, assetingest.PinSubjectType), x.pins(x.hC, assetingest.PinSubjectType))
	}
	x.wantNoAccessPins(x.hB, x.hC, x.a.hash[0])
	if r := x.row(x.id, posPreservation); r.state != "not_transferred" || r.reason != nil {
		t.Fatalf("preservation %+v", r)
	}
	if r := x.row(x.id, posOriginal); r.state != "verified" {
		t.Fatalf("original %+v", r)
	}
	// The record row is unchanged by its access copies.
	var after string
	if err := x.pool.QueryRow(context.Background(), `SELECT row_to_json(r)::text FROM federation_received_assets r WHERE id = $1`, x.id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if len(before) < len(after) || before[:len(after)] != after {
		t.Fatalf("the record changed:\n%s\n%s", before, after)
	}
}

// A Fetcher that cannot recheck access requests runs no access work; the
// original path is as before.
func TestAccessWorker_OlderFetcherRunsNoAccessWork(t *testing.T) {
	x := newAccessFx(t, true)
	w := x.worker(x.service(fetchOnly{c: x.client}), accessWorkerConfig())
	if _, err := w.AdmitAccessWork(context.Background()); err != assetingest.ErrAccessUnavailable {
		t.Fatalf("admit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	// An original import still runs.
	y := x.newAsset(1)
	x.grant(x.peerID, y.uri, nil)
	out, err := x.schedule(x.envelope(y, y.member(x.version(y), 1, "public")))
	if err != nil || out.State != assetingest.StatePending {
		t.Fatalf("schedule: %+v %v", out, err)
	}
	eventually(t, "the original import", func() bool { st, _ := x.state(out.RecordID); return st == assetingest.StateComplete })
	cancel()
	<-done
	if x.workRows() != 0 || x.accessFiles("not_transferred") != 3 {
		t.Fatal("access work ran without an access-capable fetcher")
	}
}

// An original import completes while an access transfer is stalled; the
// stalled one times out, is retried, and completes.
func TestAccessWorker_OriginalCompletesWhileAccessStalled(t *testing.T) {
	x := newAccessFx(t, true)
	x.origin.setMode(x.hB, "hold")
	w := x.worker(x.ingest, accessWorkerConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "the access transfer held at the origin", func() bool { return x.origin.inflight.Load() == 1 })
	y := x.newAsset(1)
	x.grant(x.peerID, y.uri, nil)
	out, err := x.schedule(x.envelope(y, y.member(x.version(y), 1, "public")))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the original import while access is held", func() bool { st, _ := x.state(out.RecordID); return st == assetingest.StateComplete })
	stalled := x.origin.inflight.Load()
	x.origin.setMode(x.hB, "")
	close(x.origin.release)
	if stalled != 1 {
		t.Logf("the held transfer ended before the original completed (inflight %d); the original did not wait for it either way", stalled)
	}
	// The stalled transfer, retried (forced due), completes.
	eventually(t, "B verified after the stall", func() bool {
		x.exec(`UPDATE federation_received_access_work SET next_attempt_at = now() WHERE received_asset_id = $1 AND claim_token IS NULL`, x.id)
		return x.row(x.id, posAccessB).state == "verified"
	})
	x.wantNoAccessPins(x.hB)
}

// Per-file outcomes: transient (retried, then recorded once spent) and
// permanent (recorded at once); the other files and the record are not
// affected.
func TestAccessWorker_FailureRetryAndExhaustion(t *testing.T) {
	x := newAccessFx(t, true)
	cfg := accessWorkerConfig()
	cfg.AccessMaxAttempts = 2
	w := x.worker(x.ingest, cfg)
	ctx := context.Background()
	x.origin.setMode(x.hC, "unavailable") // 503: transient
	rs, err := w.RunAccessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(rs); got[posAccessB] != assetingest.AccessVerified || got[posAccessC] != assetingest.AccessRescheduled || got[posAccessDup] != assetingest.AccessVerified {
		t.Fatalf("first pass %v", got)
	}
	var attempts int
	var last string
	if err := x.pool.QueryRow(ctx, `SELECT attempts, last_reason FROM federation_received_access_work WHERE received_asset_id = $1 AND position = $2`,
		x.id, posAccessC).Scan(&attempts, &last); err != nil || attempts != 1 || last != casfetch.ReasonHTTPStatus {
		t.Fatalf("rescheduled row: attempts %d last %q (%v)", attempts, last, err)
	}
	if r := x.row(x.id, posAccessC); r.state != "not_transferred" || r.reason != nil {
		t.Fatalf("a transient failure was recorded: %+v", r)
	}
	// Not due yet: nothing runs.
	if rs, _ := w.RunAccessOnce(ctx); len(rs) != 0 {
		t.Fatalf("ran before it was due: %+v", rs)
	}
	x.exec(`UPDATE federation_received_access_work SET next_attempt_at = now() WHERE received_asset_id = $1`, x.id)
	rs, err = w.RunAccessOnce(ctx)
	if err != nil || len(rs) != 1 || rs[0].Outcome != assetingest.AccessFailed || rs[0].Reason != casfetch.ReasonHTTPStatus {
		t.Fatalf("second attempt %+v %v", rs, err)
	}
	if r := x.row(x.id, posAccessC); r.state != "not_transferred" || r.reason == nil || *r.reason != casfetch.ReasonHTTPStatus {
		t.Fatalf("spent attempts not recorded: %+v", r)
	}
	if x.workRows() != 0 {
		t.Fatal("decided work left a row")
	}
	// A recorded file is not discovered again.
	x.origin.setMode(x.hC, "")
	if n, err := w.AdmitAccessWork(ctx); err != nil || n != 0 {
		t.Fatalf("a decided file was admitted again: %d %v", n, err)
	}
	if st, _ := x.state(x.id); st != assetingest.StateComplete {
		t.Fatalf("the record is %s", st)
	}
	x.wantNoAccessPins(x.hB, x.hC)
}

func TestAccessWorker_PermanentFailureRecordedAtOnce(t *testing.T) {
	x := newAccessFx(t, true)
	w := x.worker(x.ingest, accessWorkerConfig())
	x.origin.setMode(x.hC, "tamper")
	rs, err := w.RunAccessOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(rs); got[posAccessC] != assetingest.AccessFailed || got[posAccessB] != assetingest.AccessVerified {
		t.Fatalf("outcomes %v", got)
	}
	if r := x.row(x.id, posAccessC); r.reason == nil || *r.reason != casfetch.ReasonFixityMismatch {
		t.Fatalf("row %+v", r)
	}
	if x.workRows() != 0 {
		t.Fatal("work left")
	}
	x.wantNoAccessPins(x.hC)
}

// A claim that lapsed writes nothing; the claim that replaced it records.
func TestAccessWorker_LostClaimIsFenced(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	cfg := accessWorkerConfig()
	a, b := x.worker(x.ingest, cfg), x.worker(x.ingest, cfg)
	if _, err := a.AdmitAccessWork(ctx); err != nil {
		t.Fatal(err)
	}
	stale, err := a.ClaimAccessWork(ctx, 1)
	if err != nil || len(stale) != 1 {
		t.Fatalf("claim: %+v %v", stale, err)
	}
	// The first worker stalls past its lease.
	x.exec(`UPDATE federation_received_access_work SET claim_expires_at = now() - interval '1 second'
		WHERE received_asset_id = $1 AND position = $2`, stale[0].RecordID, stale[0].Position)
	fresh, err := b.ClaimAccessWork(ctx, 1)
	if err != nil || len(fresh) != 1 || fresh[0].Position != stale[0].Position || fresh[0].Token == stale[0].Token {
		t.Fatalf("reclaim: %+v %v", fresh, err)
	}
	// The stale worker runs its whole transfer, and writes nothing.
	got, err := a.RunAccessClaim(ctx, stale[0])
	if err != nil || got.Outcome != assetingest.AccessStale {
		t.Fatalf("stale claim: %+v %v", got, err)
	}
	if r := x.row(x.id, stale[0].Position); r.state != "not_transferred" {
		t.Fatalf("a stale claim recorded: %+v", r)
	}
	x.wantNoAccessPins(x.hB, x.hC, x.a.hash[0])
	got, err = b.RunAccessClaim(ctx, fresh[0])
	if err != nil || got.Outcome != assetingest.AccessVerified {
		t.Fatalf("fresh claim: %+v %v", got, err)
	}
	// And the stale token cannot finish anything after it either.
	if got, err := a.RunAccessClaim(ctx, stale[0]); err != nil || got.Outcome != assetingest.AccessStale {
		t.Fatalf("stale claim again: %+v %v", got, err)
	}
}

// A decided copy is not fetched again and gets no second pin.
func TestAccessWorker_DuplicateWorkNoSecondPin(t *testing.T) {
	x := newAccessFx(t, true)
	h := &hookedFetcher{c: x.client}
	w := x.worker(x.service(h), accessWorkerConfig())
	ctx := context.Background()
	if _, err := w.RunAccessOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fetches := h.fetches.Load()
	pins := x.recordPins(x.id)
	// The same work admitted again (a replayed or racing admission): the
	// next admission pass purges it as decided, before any claim.
	x.exec(`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, $2)`, x.id, posAccessB)
	if rs, err := w.RunAccessOnce(ctx); err != nil || len(rs) != 0 || x.workRows() != 0 {
		t.Fatalf("a decided row was not purged: %+v %v (%d rows)", rs, err, x.workRows())
	}
	// Claimed before any purge: the run finds the copy verified and
	// fetches nothing.
	x.exec(`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, $2)`, x.id, posAccessB)
	claims, err := w.ClaimAccessWork(ctx, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	if got, err := w.RunAccessClaim(ctx, claims[0]); err != nil || got.Outcome != assetingest.AccessDuplicate {
		t.Fatalf("replay %+v %v", got, err)
	}
	if h.fetches.Load() != fetches || x.recordPins(x.id) != pins || x.workRows() != 0 {
		t.Fatalf("fetched %d→%d, record pins %d→%d", fetches, h.fetches.Load(), pins, x.recordPins(x.id))
	}
	x.wantNoAccessPins(x.hB)
}

// No access-copy grant (an original grant is not one) or no profile:
// nothing admitted, nothing asked of the origin. A grant revoked after
// admission is refused before contact.
func TestAccessWorker_NoAuthorityNoContact(t *testing.T) {
	for name, change := range map[string]func(x *accessFx){
		"original-only grant": func(x *accessFx) {
			x.revoke(x.peerID, x.a.uri, x.share)
			x.grantFor(x.a.uri, true, false)
		},
		"derived-transfer-1 not negotiated": func(x *accessFx) {
			x.setCaps(`["share-lifecycle-1", "archival-1", "original-transfer-1"]`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newAccessFx(t, true)
			change(x)
			heads := x.origin.heads.Load()
			w := x.worker(x.ingest, accessWorkerConfig())
			if n, err := w.AdmitAccessWork(context.Background()); err != nil || n != 0 {
				t.Fatalf("admitted %d (%v)", n, err)
			}
			if rs, err := w.RunAccessOnce(context.Background()); err != nil || len(rs) != 0 {
				t.Fatalf("ran %+v %v", rs, err)
			}
			if x.origin.heads.Load() != heads || x.origin.getCount(x.hB) != 0 {
				t.Fatal("the origin was contacted")
			}
		})
	}
	t.Run("revoked after admission", func(t *testing.T) {
		x := newAccessFx(t, true)
		w := x.worker(x.ingest, accessWorkerConfig())
		ctx := context.Background()
		if n, err := w.AdmitAccessWork(ctx); err != nil || n != 3 {
			t.Fatalf("admitted %d (%v)", n, err)
		}
		heads := x.origin.heads.Load()
		x.revoke(x.peerID, x.a.uri, x.share)
		rs, err := w.RunAccessOnce(ctx)
		if err != nil || len(rs) != 3 {
			t.Fatalf("%+v %v", rs, err)
		}
		for _, r := range rs {
			if r.Outcome != assetingest.AccessFailed || r.Reason != casfetch.ReasonNoInboundGrant {
				t.Fatalf("result %+v", r)
			}
		}
		if x.origin.heads.Load() != heads {
			t.Fatal("the origin was contacted after the revoke")
		}
	})
}

// The final HEAD refuses, the grant ends at it, or the peer is downgraded
// before recording: the failure is recorded, nothing is held.
func TestAccessWorker_RevokeOrDowngradeAtTheEnd(t *testing.T) {
	cases := map[string]struct {
		set  func(x *accessFx, h *hookedFetcher)
		want string
	}{
		"origin refuses the final HEAD": {func(x *accessFx, h *hookedFetcher) {
			h.beforeRecheck = func() { x.origin.setMode(x.hB, "missing") }
		}, casfetch.ReasonOriginRefused},
		"access grant revoked at the final HEAD": {func(x *accessFx, h *hookedFetcher) {
			h.beforeRecheck = func() { x.revoke(x.peerID, x.a.uri, x.share) }
		}, casfetch.ReasonNoInboundGrant},
		"downgraded before recording": {func(x *accessFx, h *hookedFetcher) {
			h.afterRecheck = func() { x.setCaps(`["share-lifecycle-1", "archival-1", "original-transfer-1"]`) }
		}, casfetch.ReasonRevokedDuringFetch},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			x := newAccessFx(t, true)
			cfg := accessWorkerConfig()
			cfg.AccessConcurrency = 1
			h := &hookedFetcher{c: x.client}
			tc.set(x, h)
			w := x.worker(x.service(h), cfg)
			ctx := context.Background()
			x.exec(`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, $2)`, x.id, posAccessB)
			claims, err := w.ClaimAccessWork(ctx, 1)
			if err != nil || len(claims) != 1 {
				t.Fatalf("claim %+v %v", claims, err)
			}
			got, err := w.RunAccessClaim(ctx, claims[0])
			if err != nil || got.Outcome != assetingest.AccessFailed || got.Reason != tc.want {
				t.Fatalf("%+v %v, want failed %s", got, err, tc.want)
			}
			if r := x.row(x.id, posAccessB); r.reason == nil || *r.reason != tc.want || r.state != "not_transferred" {
				t.Fatalf("row %+v", r)
			}
			if x.pins(x.hB, assetingest.PinSubjectType) != 0 || x.workRows() != 0 {
				t.Fatal("held after a refusal")
			}
			x.wantNoAccessPins(x.hB)
		})
	}
}

// Supersession or withdrawal while a copy is in flight: no file result,
// no pin, the work dropped.
func TestAccessWorker_SupersessionOrWithdrawalInFlight(t *testing.T) {
	cases := map[string]func(x *accessFx){
		"superseded": func(x *accessFx) {
			files, _ := x.originals(1)
			if out, err := x.receive(x.announceV(x.a, 2, files)); err != nil || out.State != assetingest.StateComplete {
				x.t.Errorf("v2: %+v %v", out, err)
			}
		},
		"withdrawn": func(x *accessFx) {
			if _, err := x.withdraw(x.deleteOf(x.a, x.tombstone(x.a, ""))); err != nil {
				x.t.Errorf("withdraw: %v", err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			x := newAccessFx(t, true)
			h := &hookedFetcher{c: x.client}
			var once atomic.Bool
			h.afterFetch = func() {
				if once.CompareAndSwap(false, true) {
					change(x)
				}
			}
			w := x.worker(x.service(h), accessWorkerConfig())
			ctx := context.Background()
			x.exec(`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, $2)`, x.id, posAccessB)
			claims, err := w.ClaimAccessWork(ctx, 1)
			if err != nil || len(claims) != 1 {
				t.Fatalf("claim %+v %v", claims, err)
			}
			got, err := w.RunAccessClaim(ctx, claims[0])
			if err != nil || got.Outcome != assetingest.AccessDropped {
				t.Fatalf("%+v %v, want dropped", got, err)
			}
			if r := x.row(x.id, posAccessB); r.state != "not_transferred" || r.reason != nil || r.verifiedAt != nil {
				t.Fatalf("a file result for a version no longer current: %+v", r)
			}
			if x.pins(x.hB, assetingest.PinSubjectType) != 0 || x.workRows() != 0 {
				t.Fatal("held for a version no longer current")
			}
			x.wantNoAccessPins(x.hB)
			// Nothing of it is admitted again.
			if n, err := w.AdmitAccessWork(ctx); err != nil || n != 0 {
				t.Fatalf("admitted %d (%v) after %s", n, err, name)
			}
		})
	}
}

// The outstanding, concurrency and discovery bounds, across workers.
func TestAccessWorker_LimitsAcrossWorkers(t *testing.T) {
	ctx := context.Background()
	t.Run("outstanding", func(t *testing.T) {
		x := newAccessFx(t, true)
		cfg := accessWorkerConfig()
		cfg.AccessMaxOutstanding = 2
		a, b := x.worker(x.ingest, cfg), x.worker(x.ingest, cfg)
		if n, _ := a.AdmitAccessWork(ctx); n != 2 {
			t.Fatalf("admitted %d, want the bound 2", n)
		}
		if n, _ := b.AdmitAccessWork(ctx); n != 0 || x.workRows() != 2 {
			t.Fatalf("a second worker admitted past the bound: %d (%d rows)", n, x.workRows())
		}
		if _, err := a.RunAccessOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if n, _ := b.AdmitAccessWork(ctx); n != 1 {
			t.Fatalf("the third entry was not admitted later: %d", n)
		}
	})
	t.Run("global concurrency", func(t *testing.T) {
		x := newAccessFx(t, true)
		cfg := accessWorkerConfig()
		cfg.AccessGlobalConcurrency = 1
		a, b := x.worker(x.ingest, cfg), x.worker(x.ingest, cfg)
		if _, err := a.AdmitAccessWork(ctx); err != nil {
			t.Fatal(err)
		}
		ca, err := a.ClaimAccessWork(ctx, 5)
		if err != nil || len(ca) != 1 {
			t.Fatalf("claimed %d (%v), want 1", len(ca), err)
		}
		if cb, err := b.ClaimAccessWork(ctx, 5); err != nil || len(cb) != 0 {
			t.Fatalf("a second worker claimed %d while one claim runs", len(cb))
		}
		if got, err := a.RunAccessClaim(ctx, ca[0]); err != nil || got.Outcome != assetingest.AccessVerified {
			t.Fatalf("%+v %v", got, err)
		}
		if cb, err := b.ClaimAccessWork(ctx, 5); err != nil || len(cb) != 1 {
			t.Fatalf("claimed %d after the first finished, want 1", len(cb))
		}
	})
	t.Run("discovery page", func(t *testing.T) {
		x := newAccessFx(t, true)
		cfg := accessWorkerConfig()
		cfg.AccessDiscoveryPage = 1
		w := x.worker(x.ingest, cfg)
		total := 0
		for i := 0; i < 8 && total < 3; i++ {
			n, err := w.AdmitAccessWork(ctx)
			if err != nil || n > 1 {
				t.Fatalf("pass %d admitted %d (%v), want at most one per page", i, n, err)
			}
			total += n
		}
		if total != 3 || x.workRows() != 3 {
			t.Fatalf("admitted %d over the passes (%d rows), want 3", total, x.workRows())
		}
	})
}

// A stopped attempt (shutdown mid-transfer) is released, its attempt not
// counted and its pin not kept; the next run completes it, and running
// again changes nothing.
func TestAccessWorker_StoppedAttemptReleasedAndRetried(t *testing.T) {
	x := newAccessFx(t, true)
	cfg := accessWorkerConfig()
	w := x.worker(x.ingest, cfg)
	bg := context.Background()
	x.origin.setMode(x.hB, "hold")
	x.exec(`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, $2)`, x.id, posAccessB)
	claims, err := w.ClaimAccessWork(bg, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	ctx, cancel := context.WithCancel(bg)
	res := make(chan assetingest.AccessResult, 1)
	go func() { r, _ := w.RunAccessClaim(ctx, claims[0]); res <- r }()
	eventually(t, "the transfer held", func() bool { return x.origin.inflight.Load() == 1 })
	cancel()
	if got := <-res; got.Outcome != assetingest.AccessReleased {
		t.Fatalf("stopped attempt: %+v", got)
	}
	var attempts int
	var claim *string
	if err := x.pool.QueryRow(bg, `SELECT attempts, claim_token::text FROM federation_received_access_work WHERE received_asset_id = $1 AND position = $2`,
		x.id, posAccessB).Scan(&attempts, &claim); err != nil || attempts != 0 || claim != nil {
		t.Fatalf("released row: attempts %d claim %v (%v)", attempts, claim, err)
	}
	x.wantNoAccessPins(x.hB)
	x.origin.setMode(x.hB, "")
	close(x.origin.release)
	rs, err := w.RunAccessOnce(bg)
	if err != nil || outcomes(rs)[posAccessB] != assetingest.AccessVerified {
		t.Fatalf("retry %+v %v", rs, err)
	}
	// Idempotent: nothing left to do, nothing changes.
	pins := x.recordPins(x.id)
	if rs, err := w.RunAccessOnce(bg); err != nil || len(rs) != 0 {
		t.Fatalf("again %+v %v", rs, err)
	}
	if x.recordPins(x.id) != pins || x.workRows() != 0 {
		t.Fatal("a rerun changed the pins or left work")
	}
	x.wantNoAccessPins(x.hB, x.hC, x.a.hash[0])
}

// claimOne admits B alone and claims it.
func (x *accessFx) claimOne(w *assetingest.Worker, attempts int, last string) assetingest.AccessClaim {
	x.t.Helper()
	var lr any
	if last != "" {
		lr = last
	}
	x.exec(`INSERT INTO federation_received_access_work (received_asset_id, position, attempts, last_reason) VALUES ($1, $2, $3, $4)`,
		x.id, posAccessB, attempts, lr)
	claims, err := w.ClaimAccessWork(context.Background(), 1)
	if err != nil || len(claims) != 1 {
		x.t.Fatalf("claim %+v %v", claims, err)
	}
	return claims[0]
}

type claimRow struct {
	token   string
	expires time.Time
	expired bool
}

func (x *accessFx) claimRow() claimRow {
	x.t.Helper()
	var r claimRow
	if err := x.pool.QueryRow(context.Background(), `SELECT claim_token::text, claim_expires_at, claim_expires_at <= clock_timestamp()
		FROM federation_received_access_work WHERE received_asset_id = $1 AND position = $2`, x.id, posAccessB).Scan(&r.token, &r.expires, &r.expired); err != nil {
		x.t.Fatal(err)
	}
	return r
}

// The fence judges expiry by the database's clock AFTER waiting for the
// asset lock: a claim that expires while the recording transaction waits
// (begun while it was live) writes nothing.
func TestAccessWorker_ExpiryWhileBlockedOnLockIsRefused(t *testing.T) {
	x := newAccessFx(t, true)
	w := x.worker(x.ingest, accessWorkerConfig())
	ctx := context.Background()
	c := x.claimOne(w, 0, "")
	// Hold the asset's lock (as a concurrent complete or Withdraw would).
	tx, err := x.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1574))`, "asset "+x.peerID.String()+" "+x.a.uri); err != nil {
		t.Fatal(err)
	}
	res := make(chan assetingest.AccessResult, 1)
	go func() { r, _ := w.RunAccessClaim(ctx, c); res <- r }()
	eventually(t, "the recording transaction waiting for the asset lock", func() bool {
		return x.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`) >= 1
	})
	// The claim expires while the transaction waits.
	x.exec(`UPDATE federation_received_access_work SET claim_expires_at = clock_timestamp() - interval '1 millisecond'
		WHERE received_asset_id = $1 AND position = $2`, x.id, posAccessB)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-res; got.Outcome != assetingest.AccessStale {
		t.Fatalf("%+v, want stale", got)
	}
	if r := x.row(x.id, posAccessB); r.state != "not_transferred" || r.reason != nil {
		t.Fatalf("an expired claim recorded: %+v", r)
	}
	if x.pins(x.hB, assetingest.PinSubjectType) != 0 {
		t.Fatal("an expired claim pinned")
	}
	x.wantNoAccessPins(x.hB)
	// A fresh claim of the same row completes.
	claims, err := w.ClaimAccessWork(ctx, 1)
	if err != nil || len(claims) != 1 || claims[0].Token == c.Token {
		t.Fatalf("reclaim %+v %v", claims, err)
	}
	if got, err := w.RunAccessClaim(ctx, claims[0]); err != nil || got.Outcome != assetingest.AccessVerified {
		t.Fatalf("fresh claim %+v %v", got, err)
	}
}

// A stale claim does no storage or origin work at all.
func TestAccessWorker_StaleClaimDoesNoWork(t *testing.T) {
	x := newAccessFx(t, true)
	h := &hookedFetcher{c: x.client}
	w := x.worker(x.service(h), accessWorkerConfig())
	c := x.claimOne(w, 0, "")
	x.exec(`UPDATE federation_received_access_work SET claim_expires_at = clock_timestamp() - interval '1 second'
		WHERE received_asset_id = $1 AND position = $2`, x.id, posAccessB)
	heads := x.origin.heads.Load()
	got, err := w.RunAccessClaim(context.Background(), c)
	if err != nil || got.Outcome != assetingest.AccessStale {
		t.Fatalf("%+v %v", got, err)
	}
	if h.fetches.Load() != 0 || h.rechecks.Load() != 0 || x.origin.heads.Load() != heads || x.origin.getCount(x.hB) != 0 {
		t.Fatal("a stale claim fetched or asked the origin")
	}
}

// The heartbeat never extends a claim that is no longer this token's or
// that has expired; finding it lost cancels the transfer at once.
func TestAccessWorker_HeartbeatFindsTheClaimLost(t *testing.T) {
	for name, lose := range map[string]string{
		"replaced": `UPDATE federation_received_access_work SET claim_token = gen_random_uuid()
			WHERE received_asset_id = $1 AND position = $2`,
		"expired": `UPDATE federation_received_access_work SET claim_expires_at = clock_timestamp() - interval '1 millisecond'
			WHERE received_asset_id = $1 AND position = $2`,
	} {
		t.Run(name, func(t *testing.T) {
			x := newAccessFx(t, true)
			// A short lease: the heartbeat runs every half second. The
			// transfer stalls until its context is cancelled, which only
			// the heartbeat's loss can do here (the caller's never ends).
			stall := &stalledFetcher{started: make(chan struct{}), ended: make(chan error, 1)}
			svc := x.service(stall, assetingest.Config{Lease: 1500 * time.Millisecond})
			w := x.worker(svc, accessWorkerConfig())
			c := x.claimOne(w, 0, "")
			res := make(chan assetingest.AccessResult, 1)
			start := time.Now()
			go func() { r, _ := w.RunAccessClaim(context.Background(), c); res <- r }()
			awaitStarted(t, stall.started)
			x.exec(lose, x.id, posAccessB)
			before := x.claimRow()
			var got assetingest.AccessResult
			select {
			case got = <-res:
			case <-time.After(10 * time.Second):
				t.Fatal("the attempt kept running after its claim was lost")
			}
			t.Logf("attempt ended %v after start", time.Since(start).Round(10*time.Millisecond))
			if got.Outcome != assetingest.AccessStale {
				t.Fatalf("%+v, want stale", got)
			}
			after := x.claimRow()
			if after.token != before.token || !after.expires.Equal(before.expires) {
				t.Fatalf("the heartbeat changed a lost claim: %+v → %+v", before, after)
			}
			if name == "expired" && !after.expired {
				t.Fatal("the heartbeat extended an expired claim")
			}
			if r := x.row(x.id, posAccessB); r.state != "not_transferred" || r.reason != nil {
				t.Fatalf("a lost claim recorded: %+v", r)
			}
			if err := <-stall.ended; err != context.Canceled {
				t.Fatalf("the transfer ended with %v, want its cancellation", err)
			}
			x.wantNoAccessPins(x.hB)
		})
	}
}

// At the 100 attempt cap: a row claimed with its attempts already spent
// is decided (with its last transient reason, or lease_lost), with no
// contact; the hundredth attempt itself still runs; an ordinary restart
// (a lapsed claim on an early attempt) retries and completes.
func TestAccessWorker_ExhaustedAtTheCapAndOrdinaryRestart(t *testing.T) {
	cfg := accessWorkerConfig()
	cfg.AccessMaxAttempts = 100
	ctx := context.Background()
	for name, tc := range map[string]struct{ last, want string }{
		"with a last reason": {casfetch.ReasonTimeout, casfetch.ReasonTimeout},
		"without one":        {"", casfetch.ReasonLeaseLost},
	} {
		t.Run("spent "+name, func(t *testing.T) {
			x := newAccessFx(t, true)
			h := &hookedFetcher{c: x.client}
			w := x.worker(x.service(h), cfg)
			c := x.claimOne(w, 100, tc.last)
			if !c.Exhausted || c.Attempts != 100 {
				t.Fatalf("claim %+v, want exhausted at 100", c)
			}
			got, err := w.RunAccessClaim(ctx, c)
			if err != nil || got.Outcome != assetingest.AccessFailed || got.Reason != tc.want {
				t.Fatalf("%+v %v, want failed %s", got, err, tc.want)
			}
			if r := x.row(x.id, posAccessB); r.reason == nil || *r.reason != tc.want {
				t.Fatalf("row %+v", r)
			}
			if h.fetches.Load() != 0 || x.workRows() != 0 {
				t.Fatal("an exhausted claim fetched, or left its row")
			}
		})
	}
	t.Run("the hundredth attempt runs", func(t *testing.T) {
		x := newAccessFx(t, true)
		w := x.worker(x.ingest, cfg)
		c := x.claimOne(w, 99, casfetch.ReasonTimeout)
		if c.Exhausted || c.Attempts != 100 {
			t.Fatalf("claim %+v", c)
		}
		if got, err := w.RunAccessClaim(ctx, c); err != nil || got.Outcome != assetingest.AccessVerified {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("the hundredth attempt fails transiently", func(t *testing.T) {
		x := newAccessFx(t, true)
		w := x.worker(x.ingest, cfg)
		x.origin.setMode(x.hB, "unavailable")
		c := x.claimOne(w, 99, casfetch.ReasonTimeout)
		if got, err := w.RunAccessClaim(ctx, c); err != nil || got.Outcome != assetingest.AccessFailed || got.Reason != casfetch.ReasonHTTPStatus {
			t.Fatalf("%+v %v, want the spent transient recorded", got, err)
		}
	})
	t.Run("ordinary restart", func(t *testing.T) {
		x := newAccessFx(t, true)
		w := x.worker(x.ingest, accessWorkerConfig())
		first := x.claimOne(w, 0, "")
		// The process stops; its claim lapses.
		x.exec(`UPDATE federation_received_access_work SET claim_expires_at = clock_timestamp() - interval '1 second'
			WHERE received_asset_id = $1 AND position = $2`, x.id, posAccessB)
		claims, err := x.worker(x.ingest, accessWorkerConfig()).ClaimAccessWork(ctx, 1)
		if err != nil || len(claims) != 1 || claims[0].Exhausted || claims[0].Attempts != 2 || claims[0].Token == first.Token {
			t.Fatalf("reclaim %+v %v", claims, err)
		}
		if got, err := w.RunAccessClaim(ctx, claims[0]); err != nil || got.Outcome != assetingest.AccessVerified {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func awaitStarted(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the transfer did not start")
	}
}

// lockWaiters counts lock requests waiting, of the given lock types, or on
// the named relation.
func (x *accessFx) lockWaiters(q string, args ...any) int {
	return x.count(`SELECT count(*) FROM pg_locks l LEFT JOIN pg_class c ON c.oid = l.relation WHERE NOT l.granted AND (`+q+`)`, args...)
}

// A renewal blocked on the claim row (another transaction holds it) is
// bound to the claim's local deadline: at it, the heartbeat gives up, the
// stalled transfer is cancelled, and the claim is never extended, also
// once the row is released.
func TestAccessWorker_HeartbeatBoundedWhenTheClaimRowIsHeld(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	stall := &stalledFetcher{started: make(chan struct{}), ended: make(chan error, 1)}
	svc := x.service(stall, assetingest.Config{Lease: 1500 * time.Millisecond})
	w := x.worker(svc, accessWorkerConfig())
	c := x.claimOne(w, 0, "")
	res := make(chan assetingest.AccessResult, 1)
	go func() { r, _ := w.RunAccessClaim(ctx, c); res <- r }()
	awaitStarted(t, stall.started)
	// Hold the claim row, as a contending transaction would.
	tx, err := x.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT 1 FROM federation_received_access_work WHERE received_asset_id = $1 AND position = $2 FOR UPDATE`,
		x.id, posAccessB); err != nil {
		t.Fatal(err)
	}
	held := x.claimRow()
	eventually(t, "the heartbeat's renewal waiting on the held claim row", func() bool {
		return x.lockWaiters(`l.locktype IN ('transactionid', 'tuple')`) >= 1
	})
	select {
	case err := <-stall.ended:
		if err != context.Canceled {
			t.Fatalf("the transfer ended with %v, want its cancellation", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the transfer kept running past the claim's deadline while its renewal was blocked")
	}
	if got := <-res; got.Outcome != assetingest.AccessStale {
		t.Fatalf("%+v, want stale", got)
	}
	// The cancelled renewal's server-side wait may outlive the client
	// until the row is released (the server does not see the client go
	// while it waits on a lock); it sent no UPDATE and no COMMIT, and its
	// connection is discarded, so it can change nothing. Release the row,
	// let that wait end, then check nothing moved.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the cancelled renewal's wait to end after the release", func() bool {
		return x.lockWaiters(`l.locktype IN ('transactionid', 'tuple')`) == 0
	})
	after := x.claimRow()
	if after.token != held.token || !after.expires.Equal(held.expires) || !after.expired {
		t.Fatalf("the claim was changed after the row was released: %+v → %+v", held, after)
	}
	if r := x.row(x.id, posAccessB); r.state != "not_transferred" || r.reason != nil {
		t.Fatalf("a stale outcome: %+v", r)
	}
	if x.pins(x.hB, assetingest.PinSubjectType) != 0 {
		t.Fatal("a stale pin")
	}
	x.wantNoAccessPins(x.hB)
}

// An exhausted claim consumes no attempt, so neither an orderly stop nor
// a deferral refunds one: the spent count stays at the cap, and the next
// claim goes straight to the recorded outcome, with no transfer.
func TestAccessWorker_ExhaustedReleaseAndDeferKeepTheCount(t *testing.T) {
	cfg := accessWorkerConfig()
	cfg.AccessMaxAttempts = 100
	ctx := context.Background()
	type workRow struct {
		attempts int
		claimed  bool
		last     *string
		due      bool
	}
	read := func(x *accessFx) workRow {
		var r workRow
		if err := x.pool.QueryRow(ctx, `SELECT attempts, claim_token IS NOT NULL, last_reason, next_attempt_at <= clock_timestamp()
			FROM federation_received_access_work WHERE received_asset_id = $1 AND position = $2`, x.id, posAccessB).
			Scan(&r.attempts, &r.claimed, &r.last, &r.due); err != nil {
			x.t.Fatal(err)
		}
		return r
	}
	for name, interrupt := range map[string]func(x *accessFx, cancel context.CancelFunc){
		// The process stops while the claim waits to read its work.
		"orderly stop": func(_ *accessFx, cancel context.CancelFunc) { cancel() },
		// The read fails (its connection ends) with the process running.
		"deferred read": func(x *accessFx, _ context.CancelFunc) {
			x.exec(`SELECT pg_terminate_backend(l.pid) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
				WHERE NOT l.granted AND c.relname = 'federation_received_asset_files'`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newAccessFx(t, true)
			h := &hookedFetcher{c: x.client}
			w := x.worker(x.service(h), cfg)
			c := x.claimOne(w, 100, casfetch.ReasonTimeout)
			if !c.Exhausted || c.Attempts != 100 || read(x).attempts != 100 {
				t.Fatalf("claim %+v, row %+v: an exhausted claim consumed an attempt", c, read(x))
			}
			// Block the claim's read of its work.
			tx, err := x.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			if _, err := tx.Exec(ctx, `LOCK TABLE federation_received_asset_files IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			rctx, cancel := context.WithCancel(ctx)
			defer cancel()
			res := make(chan assetingest.AccessResult, 1)
			go func() { r, _ := w.RunAccessClaim(rctx, c); res <- r }()
			eventually(t, "the claim's read waiting on the file rows", func() bool {
				return x.lockWaiters(`c.relname = 'federation_received_asset_files'`) >= 1
			})
			interrupt(x, cancel)
			got := <-res
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			want := assetingest.AccessReleased
			if name == "deferred read" {
				want = assetingest.AccessRescheduled
			}
			r := read(x)
			if got.Outcome != want || r.attempts != 100 || r.claimed || r.last == nil || *r.last != casfetch.ReasonTimeout {
				t.Fatalf("%+v, row %+v: want %s with the count at 100 and the reason kept", got, r, want)
			}
			if name == "deferred read" && r.due {
				t.Fatal("a deferred row is due at once, not after its backoff")
			}
			// Reclaimed: straight to the recorded outcome, no transfer.
			x.exec(`UPDATE federation_received_access_work SET next_attempt_at = now() WHERE received_asset_id = $1 AND position = $2`, x.id, posAccessB)
			again := x.claimOneExisting(w)
			if !again.Exhausted || again.Attempts != 100 {
				t.Fatalf("reclaim %+v", again)
			}
			out, err := w.RunAccessClaim(ctx, again)
			if err != nil || out.Outcome != assetingest.AccessFailed || out.Reason != casfetch.ReasonTimeout {
				t.Fatalf("%+v %v, want failed timeout", out, err)
			}
			if h.fetches.Load() != 0 || x.workRows() != 0 {
				t.Fatal("an exhausted item was transferred, or left its row")
			}
		})
	}
}

// claimOneExisting claims the (already admitted) row.
func (x *accessFx) claimOneExisting(w *assetingest.Worker) assetingest.AccessClaim {
	x.t.Helper()
	claims, err := w.ClaimAccessWork(context.Background(), 1)
	if err != nil || len(claims) != 1 {
		x.t.Fatalf("claim %+v %v", claims, err)
	}
	return claims[0]
}
