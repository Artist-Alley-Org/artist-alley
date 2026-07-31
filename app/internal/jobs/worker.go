// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// typeCapGate is the narrow surface Worker needs from Pool for per-
// type concurrency capping. *Pool satisfies it. Defined as an
// interface so Worker can be tested without a real Pool.
type typeCapGate interface {
	tryReserve(types []JobType) []JobType
	releaseReserved(reserved []JobType, keep JobType) bool
	release(t JobType)
	// hasCaps reports whether any per-type cap is configured, so an
	// unrestricted worker knows whether it must consult the gate at all.
	hasCaps() bool
}

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

	// Gate optionally caps concurrent execution per job type. nil
	// means no per-type caps (the legacy behaviour).
	Gate typeCapGate
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

		// Per-type concurrency cap: figure out which types we may
		// claim this poll, honouring the gate. claimScope has already
		// RESERVED a slot on each capped type it returns, so every exit
		// path below has to hand those back — see releaseReserved.
		// If everything is saturated, back off without hitting the DB.
		claimTypes, saturated := w.claimScope()
		if saturated {
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.PollInterval):
			}
			continue
		}

		// Bounded per-iteration context so a stuck claim query
		// doesn't block forever on a momentary DB hiccup.
		claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		job, err := w.Service.ClaimNext(claimCtx, w.ID, claimTypes)
		cancel()

		if err != nil {
			if w.Gate != nil {
				w.Gate.releaseReserved(claimTypes, "")
			}
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
			if w.Gate != nil {
				w.Gate.releaseReserved(claimTypes, "")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.PollInterval):
			}
			continue
		}

		// Hand back every reservation except the one for the type we
		// actually claimed; that one stays held until the job is done,
		// which is what bounds concurrent execution for the type.
		held := false
		if w.Gate != nil {
			held = w.Gate.releaseReserved(claimTypes, job.Type)
		}
		w.runOne(ctx, job)
		if held {
			w.Gate.release(job.Type)
		}
	}
}

// claimScope returns the job types this worker may claim right now and
// whether it is fully saturated (every candidate type is at its cap).
//
//   - Type-restricted worker (Types != nil): its own types, filtered by
//     the gate down to those still under their cap.
//   - Unrestricted worker (Types == nil) WITH per-type caps configured:
//     every registered type, filtered by the gate so a type at its cap
//     is skipped this poll. This is what makes the seeded
//     jobs.type_concurrency.<type> limits take effect in a single-
//     process install (#278) — the gate is otherwise never consulted
//     for the unrestricted pool.
//   - Unrestricted worker with no caps: a nil scope, which ClaimNext
//     treats as "any type", skipping the gate entirely (the common,
//     cheapest path — and it avoids scoping to the registry, so jobs of
//     an unregistered type still get claimed + failed-terminal as before).
func (w *Worker) claimScope() (types []JobType, saturated bool) {
	if w.Gate == nil {
		return w.Types, false
	}
	scope := w.Types
	if len(scope) == 0 {
		if !w.Gate.hasCaps() {
			return nil, false
		}
		// Unrestricted + capped: the claim universe is the registered
		// handlers, so the gate can exclude any type at its cap.
		scope = w.Service.Registry.Types()
		if len(scope) == 0 {
			return nil, false
		}
	}
	eligible := w.Gate.tryReserve(scope)
	if len(eligible) == 0 {
		return nil, true
	}
	return eligible, false
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
	w.logOutputs(ctx, job, result)
	if err := w.Service.Complete(ctx, job.ID, w.ID, result); err != nil && !errors.Is(err, ErrLeaseLost) {
		w.Logger.LogAttrs(ctx, slog.LevelError, "jobs.worker.report_complete_error",
			slog.String("err", err.Error()),
		)
	}
}

// outputSummary is the part of a handler's result JSON the worker can
// read without knowing which handler produced it. Every preview handler
// reports what it wrote (`generated` or `variants`) and what it left
// alone (`skipped`); handlers that report neither decode to zero fields
// and are not logged.
type outputSummary struct {
	Generated []string `json:"generated"`
	Variants  []string `json:"variants"`
	Skipped   []string `json:"skipped"`
}

// logOutputs states what a completed job actually produced (#760).
//
// A successful job used to log NOTHING, which is how "590 preview jobs,
// all done, zero failures" came to be indistinguishable from 590 jobs
// that skipped every output and wrote nothing at all. The handlers
// already recorded the difference in their result JSON — it was just
// stored in a column nobody reads and never said out loud.
//
// One place rather than eleven: the fields are a convention every
// preview handler already follows, and per-handler logging is the same
// duplication that let the skip logic drift in the first place.
func (w *Worker) logOutputs(ctx context.Context, job *Claim, result json.RawMessage) {
	if w.Logger == nil || len(result) == 0 {
		return
	}
	var s outputSummary
	if err := json.Unmarshal(result, &s); err != nil {
		return
	}
	wrote := len(s.Generated) + len(s.Variants)
	if wrote == 0 && len(s.Skipped) == 0 {
		return
	}
	w.Logger.LogAttrs(ctx, slog.LevelInfo, "jobs.worker.outputs",
		slog.String("job_id", job.ID.String()),
		slog.String("job_type", string(job.Type)),
		slog.Int("wrote", wrote),
		slog.Int("skipped", len(s.Skipped)),
		// The keys, not just the count: "skipped 5" does not tell an
		// operator whether the card thumbnail was one of them.
		slog.String("skipped_keys", strings.Join(s.Skipped, ",")),
	)
}

// Pool spawns N workers sharing the same scope. Returns a cancel
// function that stops all of them on shutdown.
//
// TypeConcurrency caps the maximum number of jobs running
// concurrently per type. Configured via system_config keys
// `jobs.type_concurrency.<type>` (seeded for ai.* in the baseline and
// for preview.3d / preview.video in migration 00004). Zero = no cap.
//
// The reservation is TAKEN in Pool.tryReserve, under the same lock
// that checks the cap, and released when runOne returns. That
// atomicity is the whole point of the gate (#777).
//
// It used to work the other way round: tryReserve only READ the
// counter and the increment happened in a separate confirmReservation
// call after the DB claim returned. Every worker that polled inside
// that window read the same stale count and passed the same gate, so
// the effective ceiling was the worker-pool size, not the cap. The
// old comment here called that "a tiny race window ... one extra job"
// — it was not bounded at one. CI run 30595183336 attempt 1 caught
// five workers (w0–w4) running preview.3d simultaneously against a
// cap of 2; all five renders then died at the same instant and the
// app stopped answering /api/v1/appearance for two minutes, which is
// the flake #777 was filed for.
type Pool struct {
	Service *Service
	Logger  *slog.Logger
	Size    int
	Types   []JobType

	// TypeConcurrency maps job type → max concurrent. Missing
	// entries (or zero values) mean no cap for that type.
	TypeConcurrency map[JobType]int

	cancel context.CancelFunc
	wg     sync.WaitGroup

	typeRunningMu sync.Mutex
	typeRunning   map[JobType]int
}

// capFor returns the effective cap for a type and whether one applies.
// A missing entry, a zero, or a negative all mean "uncapped" — the
// operator-facing convention, and the reason a
// `jobs.type_concurrency.<type> = 0` row is a no-op rather than a
// freeze. Every increment and decrement below agrees on this one
// predicate so a counter can never be taken without being returned.
func (p *Pool) capFor(t JobType) (int, bool) {
	c, ok := p.TypeConcurrency[t]
	if !ok || c <= 0 {
		return 0, false
	}
	return c, true
}

// tryReserve checks the per-type cap for the supplied types and TAKES
// a reservation on each capped type that still has room, returning the
// types the caller may claim. Caller (Worker.Run) feeds the result into
// Service.ClaimNext so the claim query only considers eligible types.
//
// The counter is incremented here, under the same lock as the check,
// so two workers racing on the last slot cannot both pass. Uncapped
// types are returned without a reservation — there is nothing to count.
//
// Every returned slice MUST be handed to releaseReserved once the claim
// resolves, naming the type actually claimed (or "" if none was), or
// the reservations leak and the type wedges at its cap forever.
//
// Returns ALL the input types if no per-type caps are configured
// (the common case for non-AI workers).
func (p *Pool) tryReserve(types []JobType) []JobType {
	if len(p.TypeConcurrency) == 0 {
		return types
	}
	p.typeRunningMu.Lock()
	defer p.typeRunningMu.Unlock()
	if p.typeRunning == nil {
		p.typeRunning = map[JobType]int{}
	}
	out := make([]JobType, 0, len(types))
	for _, t := range types {
		cap, capped := p.capFor(t)
		if !capped {
			out = append(out, t)
			continue
		}
		if p.typeRunning[t] < cap {
			p.typeRunning[t]++ // reservation taken, not just observed
			out = append(out, t)
		}
	}
	return out
}

// releaseReserved returns the reservations tryReserve took, keeping
// exactly one for `keep` — the type the worker actually claimed and is
// about to run. Pass an empty `keep` when the claim failed or the queue
// was empty, which returns all of them.
//
// Reports whether a reservation for `keep` was actually held. The
// caller must only call release() when this is true: an uncapped type
// never took a counter, and decrementing one it never incremented would
// hand a live slot away to a third worker.
//
// A worker reserves every eligible type on each poll because it cannot
// know which type the claim query will hand back. Holding those for the
// few milliseconds a claim takes makes other workers see the type as
// full, which is the conservative direction: it can briefly under-use a
// slot, never over-use one.
func (p *Pool) releaseReserved(reserved []JobType, keep JobType) bool {
	if len(reserved) == 0 || len(p.TypeConcurrency) == 0 {
		return false
	}
	p.typeRunningMu.Lock()
	defer p.typeRunningMu.Unlock()
	if p.typeRunning == nil {
		return false
	}
	kept := false
	for _, t := range reserved {
		if _, capped := p.capFor(t); !capped {
			continue
		}
		if !kept && t == keep {
			kept = true // this one stays held for the duration of the run
			continue
		}
		if p.typeRunning[t] > 0 {
			p.typeRunning[t]--
		}
	}
	return kept
}

// hasCaps reports whether any per-type concurrency cap is configured.
func (p *Pool) hasCaps() bool { return len(p.TypeConcurrency) > 0 }

// release decrements the running counter when a job finishes (or
// fails). Pairs with the single reservation releaseReserved kept.
func (p *Pool) release(t JobType) {
	if _, capped := p.capFor(t); !capped {
		return
	}
	p.typeRunningMu.Lock()
	defer p.typeRunningMu.Unlock()
	if p.typeRunning == nil {
		return
	}
	if p.typeRunning[t] > 0 {
		p.typeRunning[t]--
	}
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
			// Per-type concurrency cap (Phase 1.14.A). The gate is
			// always the Pool; a *type-restricted* worker consults
			// it (through the typeCapGate interface) before each
			// claim, while an unrestricted worker (Types == nil)
			// bypasses the gate and claims any registered type —
			// see Worker.Run.
			Gate: p,
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
