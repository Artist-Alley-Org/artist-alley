// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The received asset worker (#1574): the durable, bounded half of
// receiving an aa:AssetVersion.
//
// The inbox dispatcher schedules (Worker.ScheduleAsset → Service.Schedule):
// one short transaction binds the announcement to a received asset
// record, and a pending record is the import task. Nothing else is
// queued: the record is written atomically with the decision, so there
// is no task without a record and no record without its task, and the
// uniqueness of records (one per peer and activity, one live per peer
// and asset) bounds the tasks the same way.
//
// The worker runs the tasks (Service.Process), at most
// WorkerConfig.Concurrency at a time, on their own goroutines, so a slow
// or unavailable origin holds one slot and never the inbox. A task is
// claimed by moving its record's next_attempt_at a lease ahead, under
// FOR UPDATE SKIP LOCKED; a heartbeat keeps the lease while the task
// runs. A transient failure sets next_attempt_at by a capped backoff, up
// to Config.MaxAttempts; a final one rejects the record. A process that
// stops mid-task leaves the record pending: its lease lapses and the
// next worker (at boot, it scans first) claims it again; the stopped
// attempt's ingest pins are released by casfetch.Reconcile, and nothing
// it fetched is held under the record.
//
// It is not a jobs.Service type on purpose: a job cannot be enqueued in
// the transaction that binds the record, and any authenticated worker
// token may claim any job type over /jobs/claim, which would let an
// outside caller hold an import away from the only process that can run
// it.

package assetingest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// WorkerConfig bounds the worker.
type WorkerConfig struct {
	// Concurrency is how many imports run at once.
	Concurrency int
	// Interval is the backstop scan for due records. Schedule wakes the
	// worker at once; the scan finds retries that came due and records a
	// stopped process left behind.
	Interval time.Duration
}

// DefaultWorkerConfig returns production bounds.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{Concurrency: 2, Interval: 30 * time.Second}
}

// Worker schedules announcements from the inbox and runs their imports.
type Worker struct {
	svc    *Service
	cfg    WorkerConfig
	signer func() casfetch.Signer
	wake   chan struct{}

	mu      sync.Mutex
	running bool
}

var _ inbox.AssetScheduler = (*Worker)(nil)

// NewWorker builds the worker. signer returns this instance's identity
// (casfetch.InstanceSigner): every record it schedules is fetched as the
// instance, so only a peer-wide grant (one naming no recipient) allows
// it; a grant to one local user needs that user's signature (§8.5) and
// is not used by this worker.
func NewWorker(svc *Service, cfg WorkerConfig, signer func() casfetch.Signer) *Worker {
	def := DefaultWorkerConfig()
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = def.Concurrency
	}
	if cfg.Interval <= 0 {
		cfg.Interval = def.Interval
	}
	return &Worker{svc: svc, cfg: cfg, signer: signer, wake: make(chan struct{}, 1)}
}

// Wake asks the worker to look for due records now. Never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// ScheduleAsset binds one aa:AssetVersion the inbox dispatcher accepted
// to its record (inbox.AssetScheduler) and wakes the worker for a
// pending one. No file is transferred here.
func (w *Worker) ScheduleAsset(ctx context.Context, peerID uuid.UUID, env *federation.Envelope) (inbox.AssetScheduled, error) {
	signer := w.signer()
	if signer.KeyID == "" {
		return inbox.AssetScheduled{Retryable: true}, errors.New("assetingest: this instance has no signing identity yet")
	}
	out, err := w.svc.Schedule(ctx, Announcement{PeerID: peerID, Envelope: env, Signer: signer})
	res := inbox.AssetScheduled{RecordID: out.RecordID, State: out.State, Reason: out.Reason, Retryable: Retryable(err)}
	if res.Reason == "" {
		res.Reason = ReasonOf(err)
	}
	if err == nil && out.State == StatePending {
		w.Wake()
	}
	return res, err
}

// Run claims and runs due records until ctx ends, then waits for the
// imports it started. Safe to call once per process.
func (w *Worker) Run(ctx context.Context) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		w.log(ctx, slog.LevelWarn, "assetingest.worker: Run called more than once")
		return
	}
	w.running = true
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.running = false; w.mu.Unlock() }()

	slots := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		// First at boot: records a stopped process left are due again.
		if free := cap(slots) - len(slots); free > 0 {
			ids, err := w.claim(ctx, free)
			if err != nil && ctx.Err() == nil {
				w.log(ctx, slog.LevelWarn, "assetingest.worker: claim", "error", err)
			}
			for _, id := range ids {
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-slots; w.Wake() }()
					_, _ = w.run(ctx, id)
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.wake:
		}
	}
}

// RunOnce claims up to Concurrency due records, runs them concurrently
// and returns their outcomes once all are done. For tests and
// operators; production uses Run.
func (w *Worker) RunOnce(ctx context.Context) ([]Outcome, error) {
	ids, err := w.claim(ctx, w.cfg.Concurrency)
	if err != nil {
		return nil, err
	}
	out := make([]Outcome, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], _ = w.run(ctx, id)
		}()
	}
	wg.Wait()
	return out, nil
}

// claim takes up to n due pending records for a lease.
func (w *Worker) claim(ctx context.Context, n int) ([]uuid.UUID, error) {
	rows, err := w.svc.d.Pool.Query(ctx, `UPDATE federation_received_assets
		SET next_attempt_at = now() + make_interval(secs => $2)
		WHERE id IN (SELECT id FROM federation_received_assets
		              WHERE state = 'pending' AND next_attempt_at <= now()
		              ORDER BY next_attempt_at, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id`, n, w.svc.cfg.Lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// run processes one claimed record, keeping its lease while it runs.
func (w *Worker) run(ctx context.Context, id uuid.UUID) (Outcome, error) {
	hbCtx, stop := context.WithCancel(ctx)
	var hb sync.WaitGroup
	hb.Add(1)
	go func() {
		defer hb.Done()
		t := time.NewTicker(max(w.svc.cfg.Lease/3, time.Second))
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if _, err := w.svc.d.Pool.Exec(hbCtx, `UPDATE federation_received_assets
					SET next_attempt_at = now() + make_interval(secs => $2)
					WHERE id = $1 AND state = 'pending'`, id, w.svc.cfg.Lease.Seconds()); err != nil && hbCtx.Err() == nil {
					w.log(ctx, slog.LevelWarn, "assetingest.worker: heartbeat", "record_id", id.String(), "error", err)
				}
			}
		}
	}()
	out, err := w.svc.Process(ctx, id, w.resolve)
	stop()
	hb.Wait()
	return out, err
}

// resolve: a record is fetched only as the identity it was bound with,
// and this worker binds only as the instance.
func (w *Worker) resolve(keyID, actorURI string) (casfetch.Signer, error) {
	s := w.signer()
	if actorURI != "" || keyID == "" || keyID != s.KeyID {
		return casfetch.Signer{}, errors.New("assetingest: the record was bound for a signer this instance does not hold")
	}
	return s, nil
}

func (w *Worker) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if w.svc.d.Logger != nil {
		w.svc.d.Logger.Log(ctx, level, msg, args...)
	}
}
