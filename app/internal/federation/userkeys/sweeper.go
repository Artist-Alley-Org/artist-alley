// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-h retained-key sweeper.
//
// Reaps federation_user_keys rows whose retained_until grace
// window has elapsed. Runs as a background goroutine started at
// boot alongside the outbox + inbox dispatchers; same lifecycle,
// same context-cancel shutdown.
//
// # Why a sweeper, not on-access cleanup
//
// On-access cleanup (delete-on-read in the inbox decrypt path)
// has three problems: (1) it pushes write activity into a
// read-path that should be purely a SELECT; (2) it makes the
// time-to-reap non-deterministic — a retained key with no
// in-flight envelopes targeting it could linger for months;
// (3) it complicates the audit story (which decrypt attempt
// "owns" the reap?). A periodic sweep keeps the lifecycle clean
// + the operator can predict the reap cadence.
//
// # Why 1 hour, not 10 minutes or 1 day
//
// One hour matches the granularity an admin cares about for
// "the previous keypair is gone from disk" while keeping the
// per-tick DB pressure near-zero (partial index on
// retained_until WHERE non-null keeps the SELECT-and-DELETE
// sub-millisecond on a healthy instance). Faster ticks burn
// pool connections for no observable benefit; slower ticks
// blur the operator's mental model of "rotation grace window
// is 30 days, then it's gone."
//
// # Why an initial sweep at boot
//
// The instance may have been DOWN past the previous tick's
// schedule (operator restart, crash recovery, ungraceful
// shutdown). A boot-time first sweep catches retained keys
// that expired during downtime so the next "real" tick isn't
// the one that finally reaps a week-old expired row.
//
// # Why no jitter
//
// Single-instance: one sweeper per binary, no thundering-herd
// concern. If federation grows to multi-instance (the
// federated-remote-workers arc), the sweeper coordinator will
// pick exactly one instance to run the sweep (DB lease or
// leader election) — at that point jitter is irrelevant
// because there's only one runner anyway. Keeping the loop
// simple here.

package userkeys

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SweepTickDefault is the production tick interval. Tests can
// override via [Sweeper.tickEvery] to drive multiple ticks per
// test second. Exported as a const so an operator who needs to
// tune it knows where to look.
const SweepTickDefault = 1 * time.Hour

// SweepAuditFireFn fires once per non-zero reap with the number
// of rows deleted. Pool-bound; nil-safe. The audit event is
// federation.user.key_retained_expired; the count is the audit
// row's metadata field.
type SweepAuditFireFn func(ctx context.Context, reapedCount int64)

// Sweeper is the background goroutine that reaps expired
// retained keys. Construct via [NewSweeper]; call [Sweeper.Run]
// inside a goroutine; cancel the context to stop.
type Sweeper struct {
	pool      *pgxpool.Pool
	logger    *slog.Logger
	auditFire SweepAuditFireFn
	tickEvery time.Duration
}

// NewSweeper builds a Sweeper. Pass the same pool the rest of
// the app uses; the logger may be nil (production wires it; test
// fixtures often skip).
//
// auditFire fires once per non-zero reap; nil-safe (skips the
// audit emit when unwired).
//
// tickEvery <= 0 falls back to [SweepTickDefault]. Tests pass
// short intervals (1*time.Millisecond) to validate the loop;
// production passes 0 to accept the default.
func NewSweeper(
	pool *pgxpool.Pool,
	logger *slog.Logger,
	auditFire SweepAuditFireFn,
	tickEvery time.Duration,
) *Sweeper {
	if tickEvery <= 0 {
		tickEvery = SweepTickDefault
	}
	return &Sweeper{
		pool:      pool,
		logger:    logger,
		auditFire: auditFire,
		tickEvery: tickEvery,
	}
}

// Run blocks until ctx is cancelled. Sweeps once at boot, then
// every tickEvery interval. A sweep failure logs at WARN +
// continues — the next tick retries. Persistent failure (DB
// permanently down) would burn ticks; the operator observes via
// the WARN log + the missing federation.user.key_retained_expired
// events.
//
// Called from cmd/aa/main.go as `go sweeper.Run(ctx)`. The
// graceful-shutdown path cancels ctx before pool.Close() so the
// in-flight sweep completes its DELETE before the pool unwinds.
func (s *Sweeper) Run(ctx context.Context) {
	// Initial sweep — covers expirations accumulated during
	// downtime since the last running instance ticked.
	s.sweepOnce(ctx)

	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce executes one DELETE pass + emits audit/log on
// non-zero reap. Always returns; never propagates errors —
// callers want the loop to keep ticking through transient DB
// hiccups.
func (s *Sweeper) sweepOnce(ctx context.Context) {
	count, err := New(s.pool).SweepExpiredRetainedKeys(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.LogAttrs(ctx, slog.LevelWarn,
				"userkeys.sweeper.error",
				slog.String("err", err.Error()),
			)
		}
		return
	}
	if count == 0 {
		return // happy steady state; stay quiet
	}

	if s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelInfo,
			"userkeys.sweeper.reaped",
			slog.Int64("count", count),
		)
	}
	if s.auditFire != nil {
		s.auditFire(ctx, count)
	}
}

// SweepOnce is the exported handle test fixtures + admin-tool
// surfaces can call to drive a single sweep without spinning
// up the Run loop. Returns the reaped count + any error
// (unlike sweepOnce which only logs).
func (s *Sweeper) SweepOnce(ctx context.Context) (int64, error) {
	count, err := New(s.pool).SweepExpiredRetainedKeys(ctx)
	if err != nil {
		return 0, fmt.Errorf("userkeys.SweepOnce: %w", err)
	}
	if count > 0 && s.auditFire != nil {
		s.auditFire(ctx, count)
	}
	return count, nil
}
