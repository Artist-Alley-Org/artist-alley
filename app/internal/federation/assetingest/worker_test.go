// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 inbox route and worker, against a live database: the production
// inbox dispatcher (BuildRegistry, with this package's Worker as its
// AssetScheduler) applies a stored aa:AssetVersion row by scheduling it,
// transferring nothing; the Worker imports pending records through the
// production casfetch client from the controllable origin. The HTTP
// receipt in front of the dispatcher is inbox/asset_inbox_test.go and
// the two-instance proof twostack/asset_inbox_gate_test.go.

package assetingest_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

type workerFx struct {
	*fx
	cfg    assetingest.Config
	worker *assetingest.Worker
	disp   *inbox.Dispatcher
	q      *inbox.Queries
}

// newWorkerFx is the receiver fixture with a casfetch client patient
// enough for a held origin, a Worker and the production dispatcher.
func newWorkerFx(t *testing.T, cfg assetingest.Config, wcfg assetingest.WorkerConfig) *workerFx {
	t.Helper()
	f := newFx(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := audit.NewRecorder(f.pool, logger)
	local := func(context.Context) string { return localBase }
	f.client = casfetch.New(casfetch.Deps{
		Pool: f.pool, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: casfetch.DatabaseGrants{Store: f.grants},
		Audit: rec, Logger: logger, Transport: f.origin.srv.Client().Transport,
		Config: casfetch.Config{
			IdleTimeout: 30 * time.Second, MinOverall: 60 * time.Second, LeaseTTL: 3 * time.Second,
			Heartbeat: time.Second, WaitPoll: 50 * time.Millisecond, TempDir: t.TempDir(), LocalBaseURL: local,
		},
	})
	f.ingest = assetingest.New(assetingest.Deps{
		Pool: f.pool, Storage: f.svc, Peers: peers{pool: f.pool}, Grants: f.grants, Fetcher: f.client,
		Audit: rec, Logger: logger, Config: cfg,
	})
	w := &workerFx{fx: f, cfg: cfg, q: inbox.New(f.pool)}
	w.worker = w.newWorker(wcfg)
	w.disp = inbox.NewDispatcher(inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5}, w.q,
		func(ctx context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			p, err := peers{pool: f.pool}.ByID(ctx, id)
			return inbox.PeerInfo{ID: p.ID, InstanceURL: p.InstanceURL, Enabled: p.Enabled, Connected: p.Connected,
				Capabilities: p.Capabilities}, err
		}, nil, logger)
	w.disp.SetRegistry(inbox.BuildRegistry(w.disp, logger))
	w.disp.SetLocalBaseURL(local)
	w.disp.SetAssetScheduler(w.worker)
	return w
}

// newWorker is another worker on the same service: what a restarted
// process builds.
func (w *workerFx) newWorker(wcfg assetingest.WorkerConfig) *assetingest.Worker {
	return assetingest.NewWorker(w.ingest, wcfg, func() casfetch.Signer { return w.signer })
}

// deliver stores e as the receipt stage would (a pending inbox row from
// the peer) and has the dispatcher apply it, returning the outcome and
// how long the dispatch took.
func (w *workerFx) deliver(e *federation.Envelope) (inbox.DispatchOutcome, time.Duration) {
	w.t.Helper()
	body, err := e.Marshal()
	if err != nil {
		w.t.Fatal(err)
	}
	row, err := w.q.InsertInbox(context.Background(), inbox.InsertInboxParams{
		ActivityUri: e.ID, PeerID: pgtype.UUID{Bytes: w.peerID, Valid: true}, ActorUri: e.Actor,
		ActivityType: string(e.Type), EnvelopeJson: body, HttpSigKey: w.origin.srv.URL + "/federation/instance#main-key",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	start := time.Now()
	out := w.disp.DispatchRow(context.Background(), row)
	return out, time.Since(start)
}

func (w *workerFx) inboxStatus(activityURI string) string {
	w.t.Helper()
	var status string
	if err := w.pool.QueryRow(context.Background(), `SELECT status FROM federation_inbox WHERE activity_uri = $1`, activityURI).Scan(&status); err != nil {
		w.t.Fatal(err)
	}
	return status
}

// recordOf is the record bound to an activity.
func (w *workerFx) recordOf(activityURI string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`,
		w.peerID, activityURI).Scan(&id); err != nil {
		w.t.Fatalf("no record for %s: %v", activityURI, err)
	}
	return id
}

// runOnce runs the worker once and returns how many records it ran.
func (w *workerFx) runOnce() []assetingest.Outcome {
	w.t.Helper()
	out, err := w.worker.RunOnce(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

// makeDue moves a record's next attempt to now, as time passing would.
func (w *workerFx) makeDue(id uuid.UUID) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(), `UPDATE federation_received_assets SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
		w.t.Fatal(err)
	}
}

// waitFor polls cond until it holds or 30s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_InboxSchedulesAndTheWorkerImports(t *testing.T) {
	w := newWorkerFx(t, assetingest.Config{}, assetingest.WorkerConfig{Concurrency: 2})
	a := w.newAsset(2)
	w.grant(w.peerID, a.uri, nil)
	e := w.envelope(a, a.member(w.version(a), 1, "public"))

	if out, _ := w.deliver(e); out != inbox.OutcomeProcessed || w.inboxStatus(e.ID) != "processed" {
		t.Fatalf("dispatch: %v", out)
	}
	id := w.recordOf(e.ID)
	if st, _ := w.state(id); st != assetingest.StatePending {
		t.Fatalf("scheduled record is %s", st)
	}
	for _, h := range a.hash {
		if w.origin.getCount(h) != 0 || w.pins(h, assetingest.PinSubjectType) != 0 {
			t.Fatal("the dispatcher transferred or pinned a file")
		}
	}
	if w.origin.heads.Load() != 0 {
		t.Fatal("the dispatcher asked the origin")
	}

	outs := w.runOnce()
	if len(outs) != 1 || outs[0].State != assetingest.StateComplete || outs[0].RecordID != id {
		t.Fatalf("worker: %+v", outs)
	}
	for _, h := range a.hash {
		if w.origin.getCount(h) != 1 || w.pins(h, assetingest.PinSubjectType) != 1 || w.pins(h, "federation_ingest") != 0 {
			t.Fatalf("hash %s: %d GETs, %d record pins", h[:12], w.origin.getCount(h), w.pins(h, assetingest.PinSubjectType))
		}
		if n := w.count(`SELECT count(*) FROM assets WHERE file_hash = $1`, h); n != 0 {
			t.Fatal("a complete record created an assets row")
		}
	}

	// A second activity announcing the same version and file set adds no
	// task and fetches nothing; neither does the worker running again.
	again := w.envelope(a, a.member(w.version(a), 1, "public"))
	if out, _ := w.deliver(again); out != inbox.OutcomeProcessed {
		t.Fatalf("second announcement: %v", out)
	}
	if n := w.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2`, w.peerID, a.uri); n != 1 {
		t.Fatalf("%d records for one version", n)
	}
	if outs := w.runOnce(); len(outs) != 0 {
		t.Fatalf("the worker ran %d tasks for a complete record", len(outs))
	}
	if out, err := w.worker.ScheduleAsset(context.Background(), w.peerID, e); err != nil || out.State != assetingest.StateComplete || out.RecordID != id {
		t.Fatalf("the same activity scheduled again: %+v %v", out, err)
	}
	for _, h := range a.hash {
		if w.origin.getCount(h) != 1 {
			t.Fatal("a complete record was fetched again")
		}
	}
}

// A slow origin holds worker slots, never the dispatcher; the worker
// never runs more imports at once than its bound.
func TestWorker_SlowSourceNeverBlocksTheInbox(t *testing.T) {
	const slots = 2
	w := newWorkerFx(t, assetingest.Config{}, assetingest.WorkerConfig{Concurrency: slots, Interval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.worker.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	var held []*asset
	var acts []string
	for i := 0; i < slots+2; i++ {
		a := w.newAsset(1)
		w.origin.setMode(a.hash[0], "hold")
		w.grant(w.peerID, a.uri, nil)
		e := w.envelope(a, a.member(w.version(a), 1, "public"))
		out, took := w.deliver(e)
		if out != inbox.OutcomeProcessed || took > 5*time.Second {
			t.Fatalf("dispatch %d: %v after %v", i, out, took)
		}
		held = append(held, a)
		acts = append(acts, e.ID)
		if i == 0 {
			waitFor(t, "the first import to reach the origin", func() bool { return w.origin.inflight.Load() == 1 })
		}
	}
	waitFor(t, "the worker to fill its slots", func() bool { return w.origin.inflight.Load() >= slots })
	// Other inbound work goes on while every slot is held.
	fast := w.newAsset(1)
	w.grant(w.peerID, fast.uri, nil)
	fe := w.envelope(fast, fast.member(w.version(fast), 1, "public"))
	if out, took := w.deliver(fe); out != inbox.OutcomeProcessed || took > 5*time.Second {
		t.Fatalf("unrelated dispatch while the origin is slow: %v after %v", out, took)
	}
	time.Sleep(200 * time.Millisecond)
	if n := w.origin.inflight.Load(); n != slots {
		t.Fatalf("%d imports in flight, the bound is %d", n, slots)
	}
	close(w.origin.release)
	for _, act := range append(acts, fe.ID) {
		id := w.recordOf(act)
		waitFor(t, "record "+id.String()+" to complete", func() bool { st, _ := w.state(id); return st == assetingest.StateComplete })
	}
	if m := w.origin.maxInflight.Load(); m > slots {
		t.Fatalf("%d imports ran at once, the bound is %d", m, slots)
	}
	for _, a := range held {
		if w.pins(a.hash[0], assetingest.PinSubjectType) != 1 {
			t.Fatal("a held import completed without its pin")
		}
	}
}

func TestWorker_GrantRevokedBeforeWorkGetsNothing(t *testing.T) {
	w := newWorkerFx(t, assetingest.Config{}, assetingest.WorkerConfig{})
	a := w.newAsset(1)
	share := w.grant(w.peerID, a.uri, nil)
	e := w.envelope(a, a.member(w.version(a), 1, "public"))
	if out, _ := w.deliver(e); out != inbox.OutcomeProcessed {
		t.Fatalf("dispatch: %v", out)
	}
	w.revoke(w.peerID, a.uri, share)
	outs := w.runOnce()
	if len(outs) != 1 || outs[0].State != assetingest.StateRejected || outs[0].Reason != casfetch.ReasonNoInboundGrant {
		t.Fatalf("worker: %+v", outs)
	}
	if w.origin.heads.Load() != 0 || w.origin.getCount(a.hash[0]) != 0 {
		t.Fatal("the origin was asked under a revoked grant")
	}
	// Refused by the task's own re-check, before the fetch client (which
	// would refuse too, and audit it).
	if n := w.count(`SELECT count(*) FROM audit_events WHERE event_type LIKE 'federation.cas.%' AND metadata->>'asset_id' = $1`, a.id.String()); n != 0 {
		t.Fatalf("the worker handed a revoked grant to the fetch client (%d federation.cas events)", n)
	}
	w.nothingUsable(a.hash...)
}

// The peer row is read again when the task runs: a profile dropped or a
// peer disabled after scheduling gets nothing.
func TestWorker_PeerRecheckedAtProcessing(t *testing.T) {
	w := newWorkerFx(t, assetingest.Config{MaxAttempts: 2}, assetingest.WorkerConfig{})
	schedule := func() (*asset, uuid.UUID) {
		a := w.newAsset(1)
		w.grant(w.peerID, a.uri, nil)
		e := w.envelope(a, a.member(w.version(a), 1, "public"))
		if out, _ := w.deliver(e); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v", out)
		}
		return a, w.recordOf(e.ID)
	}
	setPeer := func(sql string) {
		if _, err := w.pool.Exec(context.Background(), sql, w.peerID); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a dropped profile", func(t *testing.T) {
		a, id := schedule()
		setPeer(`UPDATE federation_peers SET capabilities = '["share-lifecycle-1", "archival-1"]'::jsonb WHERE id = $1`)
		defer setPeer(`UPDATE federation_peers SET capabilities = '` + bothProfiles + `'::jsonb WHERE id = $1`)
		w.runOnce()
		if st, reason := w.state(id); st != assetingest.StateRejected || reason != casfetch.ReasonProfile {
			t.Fatalf("record %s/%s", st, reason)
		}
		if w.origin.getCount(a.hash[0]) != 0 {
			t.Fatal("fetched without the profile")
		}
		w.nothingUsable(a.hash...)
	})

	t.Run("a disabled peer, until attempts run out", func(t *testing.T) {
		a, id := schedule()
		setPeer(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`)
		defer setPeer(`UPDATE federation_peers SET enabled = TRUE WHERE id = $1`)
		w.runOnce()
		if st, reason := w.state(id); st != assetingest.StatePending || reason != casfetch.ReasonPeerUnavailable {
			t.Fatalf("first attempt: %s/%s", st, reason)
		}
		w.makeDue(id)
		w.runOnce()
		if st, reason := w.state(id); st != assetingest.StateRejected || reason != casfetch.ReasonPeerUnavailable {
			t.Fatalf("after the last attempt: %s/%s", st, reason)
		}
		if w.origin.getCount(a.hash[0]) != 0 {
			t.Fatal("fetched from a disabled peer")
		}
		w.nothingUsable(a.hash...)
	})
}

// A record is fetched only as the identity it was bound with.
func TestWorker_AnotherSignerGetsNothing(t *testing.T) {
	w := newWorkerFx(t, assetingest.Config{}, assetingest.WorkerConfig{})
	a := w.newAsset(1)
	w.grant(w.peerID, a.uri, nil)
	e := w.envelope(a, a.member(w.version(a), 1, "public"))
	if out, _ := w.deliver(e); out != inbox.OutcomeProcessed {
		t.Fatalf("dispatch: %v", out)
	}
	other := assetingest.NewWorker(w.ingest, assetingest.WorkerConfig{}, func() casfetch.Signer {
		return casfetch.ActorSigner(localBase+"/users/someone", nil)
	})
	outs, err := other.RunOnce(context.Background())
	if err != nil || len(outs) != 1 || outs[0].State != assetingest.StateRejected || outs[0].Reason != casfetch.ReasonForeignSigner {
		t.Fatalf("worker: %+v %v", outs, err)
	}
	if w.origin.heads.Load() != 0 || w.origin.getCount(a.hash[0]) != 0 {
		t.Fatal("fetched as another identity")
	}
	w.nothingUsable(a.hash...)
}

// An outage leaves the record pending behind a backoff; a stopped
// process leaves it pending with nothing held, and the next process's
// worker completes it.
func TestWorker_RetryAndRestart(t *testing.T) {
	t.Run("outage, backoff, retry", func(t *testing.T) {
		w := newWorkerFx(t, assetingest.Config{RetryBase: time.Hour, RetryMax: time.Hour}, assetingest.WorkerConfig{})
		a := w.newAsset(1)
		w.grant(w.peerID, a.uri, nil)
		e := w.envelope(a, a.member(w.version(a), 1, "public"))
		if out, _ := w.deliver(e); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v", out)
		}
		id := w.recordOf(e.ID)
		w.origin.setMode(a.hash[0], "unavailable")
		w.runOnce()
		if st, reason := w.state(id); st != assetingest.StatePending || reason != casfetch.ReasonHTTPStatus {
			t.Fatalf("outage: %s/%s", st, reason)
		}
		if n := w.count(`SELECT count(*) FROM federation_received_assets WHERE id = $1 AND next_attempt_at > now() + interval '50 minutes'`, id); n != 1 {
			t.Fatal("the retry is not behind its backoff")
		}
		if outs := w.runOnce(); len(outs) != 0 {
			t.Fatal("the worker retried before the backoff")
		}
		w.nothingUsable(a.hash...)
		w.origin.setMode(a.hash[0], "")
		w.makeDue(id)
		if outs := w.runOnce(); len(outs) != 1 || outs[0].State != assetingest.StateComplete {
			t.Fatalf("retry: %+v", outs)
		}
		if w.pins(a.hash[0], assetingest.PinSubjectType) != 1 {
			t.Fatal("the retried record does not hold its original")
		}
	})

	t.Run("a process stopped mid-transfer, then a new one", func(t *testing.T) {
		w := newWorkerFx(t, assetingest.Config{RetryBase: time.Hour, RetryMax: time.Hour},
			assetingest.WorkerConfig{Concurrency: 1, Interval: 50 * time.Millisecond})
		a := w.newAsset(1)
		w.grant(w.peerID, a.uri, nil)
		w.origin.setMode(a.hash[0], "hold")
		e := w.envelope(a, a.member(w.version(a), 1, "public"))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { w.worker.Run(ctx); close(done) }()
		if out, _ := w.deliver(e); out != inbox.OutcomeProcessed {
			cancel()
			t.Fatalf("dispatch: %v", out)
		}
		id := w.recordOf(e.ID)
		waitFor(t, "the transfer to start", func() bool { return w.origin.inflight.Load() == 1 })
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the worker did not stop")
		}
		if st, _ := w.state(id); st != assetingest.StatePending {
			t.Fatalf("a stopped process decided the record: %s", st)
		}
		if w.pins(a.hash[0], assetingest.PinSubjectType) != 0 || w.pins(a.hash[0], "federation_ingest") != 0 {
			t.Fatal("a stopped transfer left a pin")
		}
		// The next process: a new worker, the origin answering again.
		w.origin.setMode(a.hash[0], "")
		next := w.newWorker(assetingest.WorkerConfig{Concurrency: 1, Interval: 50 * time.Millisecond})
		ctx2, cancel2 := context.WithCancel(context.Background())
		done2 := make(chan struct{})
		go func() { next.Run(ctx2); close(done2) }()
		defer func() { cancel2(); <-done2 }()
		waitFor(t, "the next process to complete the record", func() bool { st, _ := w.state(id); return st == assetingest.StateComplete })
		if w.pins(a.hash[0], assetingest.PinSubjectType) != 1 || w.pins(a.hash[0], "federation_ingest") != 0 {
			t.Fatal("the completed record does not hold its original alone")
		}
	})

	t.Run("a lease is respected until it lapses", func(t *testing.T) {
		w := newWorkerFx(t, assetingest.Config{}, assetingest.WorkerConfig{})
		a := w.newAsset(1)
		w.grant(w.peerID, a.uri, nil)
		e := w.envelope(a, a.member(w.version(a), 1, "public"))
		if out, _ := w.deliver(e); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v", out)
		}
		id := w.recordOf(e.ID)
		// A process claimed it and died: its lease is still running.
		if _, err := w.pool.Exec(context.Background(), `UPDATE federation_received_assets SET next_attempt_at = now() + interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if outs := w.runOnce(); len(outs) != 0 {
			t.Fatal("a running lease was taken over")
		}
		w.makeDue(id)
		if outs := w.runOnce(); len(outs) != 1 || outs[0].State != assetingest.StateComplete {
			t.Fatalf("after the lease lapsed: %+v", outs)
		}
	})
}
