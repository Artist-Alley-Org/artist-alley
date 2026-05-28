package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Worker is an in-process goroutine that polls the queue, claims jobs
// it has a handler for, and runs them. Multiple Workers can run in
// the same process to parallelise CPU-bound work.
//
// External farm workers / federated peers don't use this — they go
// through the HTTP /jobs/claim surface. The two paths share the same
// claim semantics, so the queue stays consistent whatever the worker
// shape.
type Worker struct {
	Service *Service
	Logger  *slog.Logger

	// ID is a stable string we record in jobs.claimed_by. It usually
	// includes the local instance id + a worker ordinal so the admin
	// view can tell workers apart. The default is
	// "aa://local/{ordinal}" assigned by Pool.
	ID string

	// Types is the subset of registered handlers this worker will
	// process. Empty = every registered handler.
	Types []JobType

	// PollInterval is how long to wait between empty polls. The
	// queue is a polling loop (not LISTEN/NOTIFY) because workers
	// may live in another process / machine — a uniform model is
	// simpler than two code paths. 2s is plenty fresh for users;
	// the asset row also goes through cache.Registry invalidation
	// the moment a variant lands so the FE can react immediately.
	PollInterval time.Duration

	// HeartbeatEvery controls the lease-renewal cadence. Must be
	// less than Service.LeaseSeconds.
	HeartbeatEvery time.Duration
}

// Run polls the queue until ctx is cancelled. Each iteration claims
// at most one job, runs it under a heartbeat loop, and reports the
// outcome.
func (w *Worker) Run(ctx context.Context) {
	if w.PollInterval == 0 {
		w.PollInterval = 2 * time.Second
	}
	if w.HeartbeatEvery == 0 {
		w.HeartbeatEvery = time.Duration(w.Service.LeaseSeconds/2) * time.Second
	}
	w.Logger.LogAttrs(ctx, slog.LevelInfo, "jobs.worker.start",
		slog.String("worker_id", w.ID),
		slog.Any("types", w.Types),
	)
	defer w.Logger.LogAttrs(ctx, slog.LevelInfo, "jobs.worker.stop", slog.String("worker_id", w.ID))

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Bounded per-iteration context so a stuck claim query
		// doesn't block forever on a momentary DB hiccup.
		claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		job, err := w.Service.ClaimNext(claimCtx, w.ID, w.Types)
		cancel()

		if err != nil {
			w.Logger.LogAttrs(ctx, slog.LevelWarn, "jobs.worker.claim_error",
				slog.String("worker_id", w.ID),
				slog.String("err", err.Error()),
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.PollInterval):
			}
			continue
		}
		if job == nil {
			// Queue empty for our scope. Back off.
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.PollInterval):
			}
			continue
		}

		w.runOne(ctx, job)
	}
}

// runOne executes a single claimed job with a heartbeat loop. The
// Handler runs in a separate goroutine; the heartbeat keeps the
// lease alive while the work proceeds.
func (w *Worker) runOne(ctx context.Context, job *Claim) {
	h, ok := w.Service.Registry.Handler(job.Type)
	if !ok {
		// The job was claimed even though we have no handler
		// registered — possible if a peer / admin enqueued a type
		// we don't know yet. Fail terminal so it doesn't loop.
		_ = w.Service.Fail(ctx, job.ID, w.ID,
			fmt.Sprintf("no handler registered for type %q", job.Type), true)
		return
	}

	w.Logger.LogAttrs(ctx, slog.LevelDebug, "jobs.worker.start_job",
		slog.String("worker_id", w.ID),
		slog.String("job_id", job.ID.String()),
		slog.String("job_type", string(job.Type)),
		slog.Int64("attempt", int64(job.Attempts)),
	)

	// Run the handler under a sub-context cancelled when the lease
	// expires. The heartbeat extends the lease alongside.
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg     sync.WaitGroup
		result json.RawMessage
		runErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("panic: %v", r)
			}
		}()
		result, runErr = h.Handle(jobCtx, job)
	}()

	// Heartbeat loop until the handler returns.
	heartbeat := time.NewTicker(w.HeartbeatEvery)
	defer heartbeat.Stop()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	for {
		select {
		case <-done:
			heartbeat.Stop()
			w.report(ctx, job, result, runErr)
			return
		case <-heartbeat.C:
			if err := w.Service.Heartbeat(ctx, job.ID, w.ID); err != nil {
				if errors.Is(err, ErrLeaseLost) {
					w.Logger.LogAttrs(ctx, slog.LevelWarn, "jobs.worker.lease_lost",
						slog.String("worker_id", w.ID),
						slog.String("job_id", job.ID.String()),
					)
					cancel() // tell the handler to bail
					<-done
					return
				}
				w.Logger.LogAttrs(ctx, slog.LevelWarn, "jobs.worker.heartbeat_error",
					slog.String("worker_id", w.ID),
					slog.String("err", err.Error()),
				)
			}
		case <-ctx.Done():
			cancel()
			<-done
			// Worker is shutting down: report failure so the
			// watchdog won't have to.
			_ = w.Service.Fail(ctx, job.ID, w.ID, "worker shutdown", false)
			return
		}
	}
}

func (w *Worker) report(ctx context.Context, job *Claim, result json.RawMessage, runErr error) {
	if runErr != nil {
		w.Logger.LogAttrs(ctx, slog.LevelWarn, "jobs.worker.handler_failed",
			slog.String("worker_id", w.ID),
			slog.String("job_id", job.ID.String()),
			slog.String("job_type", string(job.Type)),
			slog.String("err", runErr.Error()),
			slog.Bool("terminal", IsTerminal(runErr)),
		)
		if err := w.Service.Fail(ctx, job.ID, w.ID, runErr.Error(), IsTerminal(runErr)); err != nil && !errors.Is(err, ErrLeaseLost) {
			w.Logger.LogAttrs(ctx, slog.LevelError, "jobs.worker.report_fail_error",
				slog.String("err", err.Error()),
			)
		}
		return
	}
	if err := w.Service.Complete(ctx, job.ID, w.ID, result); err != nil && !errors.Is(err, ErrLeaseLost) {
		w.Logger.LogAttrs(ctx, slog.LevelError, "jobs.worker.report_complete_error",
			slog.String("err", err.Error()),
		)
	}
}

// Pool spawns N workers sharing the same scope. Returns a cancel
// function that stops all of them on shutdown.
type Pool struct {
	Service *Service
	Logger  *slog.Logger
	Size    int
	Types   []JobType

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start spawns the workers + a watchdog goroutine. Idempotent.
func (p *Pool) Start(ctx context.Context, instanceID string) {
	if p.Size <= 0 {
		return
	}
	rootCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	for i := 0; i < p.Size; i++ {
		w := &Worker{
			Service: p.Service,
			Logger:  p.Logger,
			ID:      fmt.Sprintf("aa://%s/w%d", instanceID, i),
			Types:   p.Types,
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			w.Run(rootCtx)
		}()
	}

	// Watchdog: every 30s requeue any stuck rows + every hour purge
	// old completed rows.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		requeueT := time.NewTicker(30 * time.Second)
		purgeT := time.NewTicker(1 * time.Hour)
		defer requeueT.Stop()
		defer purgeT.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-requeueT.C:
				if n, err := p.Service.RequeueStuck(rootCtx); err != nil {
					p.Logger.LogAttrs(rootCtx, slog.LevelWarn, "jobs.watchdog.requeue_error",
						slog.String("err", err.Error()))
				} else if n > 0 {
					p.Logger.LogAttrs(rootCtx, slog.LevelInfo, "jobs.watchdog.requeued",
						slog.Int64("count", n))
				}
			case <-purgeT.C:
				if n, err := p.Service.PurgeOldDone(rootCtx); err != nil {
					p.Logger.LogAttrs(rootCtx, slog.LevelWarn, "jobs.watchdog.purge_error",
						slog.String("err", err.Error()))
				} else if n > 0 {
					p.Logger.LogAttrs(rootCtx, slog.LevelInfo, "jobs.watchdog.purged",
						slog.Int64("count", n))
				}
			}
		}
	}()
}

// Stop signals all workers + the watchdog to exit and waits for them.
func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}
