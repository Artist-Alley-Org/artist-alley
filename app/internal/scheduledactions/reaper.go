// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package scheduledactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// JobTypeReap is the recurring reaper. It runs ON the jobs queue and
// self-re-enqueues, so no external cron is needed — the same pattern as
// soft_delete.gc. One-in-flight is enforced by the idempotency key on
// the next-tick timestamp.
const JobTypeReap jobs.JobType = "scheduled_actions.reap"

// ReapInterval is how often the reaper wakes.
//
// ADR 0020 describes "a daily job", but that illustration is written
// for day-granular retention recipes. This engine also backs
// reveal-on-a-timestamp (#40) and subscription-expiry (#51), where a
// 24h lag between scheduled_for and execution is unacceptable — an
// asset set to reveal at a marketing-supplied time should surface near
// that time, not up to a day later. A 5-minute tick bounds the lag to
// ~5 min at the cost of 288 lightweight job rows a day, which the jobs
// watchdog purges. This is a refinement of the ADR's cadence, not a
// change to its table/executor design.
const ReapInterval = 5 * time.Minute

// maxPerTick bounds one reaper run so a large backlog of due actions
// can't wedge a worker goroutine indefinitely; the remainder is picked
// up on the next tick (or immediately, since a full batch re-enqueues
// with no delay — see Handle).
const maxPerTick = 500

// ReapPayload is the (empty) job body. All state is in the table.
type ReapPayload struct{}

// ReaperJob implements jobs.Handler for JobTypeReap.
type ReaperJob struct {
	Pool     *pgxpool.Pool
	Jobs     *jobs.Service
	Rec      *audit.Recorder
	Notifier Notifier
	Logger   *slog.Logger
}

// Type implements jobs.Handler.
func (h *ReaperJob) Type() jobs.JobType { return JobTypeReap }

// Handle runs one reaper tick: drain up to maxPerTick due actions, then
// re-enqueue the next tick.
func (h *ReaperJob) Handle(ctx context.Context, _ *jobs.Claim) (json.RawMessage, error) {
	exec := &executor{rec: h.Rec, notifier: h.Notifier}
	done, failed := 0, 0
	for done+failed < maxPerTick {
		outcome, claimed, err := h.processOne(ctx, exec)
		if err != nil {
			// A tx/infra error (not an executor failure) — surface it so
			// the job retries. Executor failures are captured on the row
			// as state=failed and do NOT bubble here.
			return nil, fmt.Errorf("scheduled_actions.reap: %w", err)
		}
		if !claimed {
			break
		}
		switch outcome {
		case StateDone:
			done++
		case StateFailed:
			failed++
		}
	}

	// If we hit the batch cap there may be more due right now; re-enqueue
	// immediately. Otherwise wake on the normal interval.
	drainedFull := done+failed >= maxPerTick
	next := time.Now().UTC().Add(ReapInterval)
	if drainedFull {
		next = time.Now().UTC()
	}
	if _, err := h.Jobs.Enqueue(ctx, JobTypeReap, ReapPayload{}, jobs.EnqueueOpts{
		ScheduledFor:   &next,
		IdempotencyKey: reapKey(next),
	}); err != nil {
		return nil, fmt.Errorf("scheduled_actions.reap: re-enqueue: %w", err)
	}

	result := map[string]any{"done": done, "failed": failed, "next_tick_at": next}
	b, _ := json.Marshal(result)
	return b, nil
}

// processOne claims and executes a single due action in one transaction.
//
// The transaction holds the row lock from claim through mark, so an
// overlapping reaper can never double-execute (it SKIP-LOCKs past a
// locked row). The executor runs inside a SAVEPOINT: on success the
// domain change + audit row + mark-done all commit together; on failure
// the savepoint rolls back the domain change, and the outer transaction
// still records state=failed with the error plus a failed audit row.
// Either way the action reaches a terminal state and never retries in a
// loop.
//
// Returns (outcome, claimed, err): claimed=false means no due action
// was available; err is reserved for tx/infra failures, never an
// executor failure (those are captured on the row).
func (h *ReaperJob) processOne(ctx context.Context, exec *executor) (outcome string, claimed bool, err error) {
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a commit

	q := New(tx)
	action, err := q.ClaimDueAction(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	// Run the domain change + its audit row inside a savepoint so a
	// failure undoes the domain write but keeps the claim lock, letting
	// us record state=failed on the outer tx.
	execErr := h.runInSavepoint(ctx, tx, exec, action)

	if execErr == nil {
		if err := q.MarkActionDone(ctx, action.ID); err != nil {
			return "", false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return StateDone, true, nil
	}

	// Executor failed: capture the error on the row + a failed audit
	// event, on the OUTER tx (the savepoint that held the domain change
	// was already rolled back).
	msg := execErr.Error()
	if err := q.MarkActionFailed(ctx, MarkActionFailedParams{ID: action.ID, Error: &msg}); err != nil {
		return "", false, err
	}
	h.Rec.WriteInTx(ctx, audit.New(tx), audit.EventScheduledActionFailed, nil, nil, map[string]any{
		"scheduled_action_id": uuidText(action.ID),
		"action":              action.Action,
		"target_kind":         action.TargetKind,
		"target_id":           action.TargetID,
		"error":               msg,
	})
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	if h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn, "scheduled_actions.reap.action_failed",
			slog.String("id", uuidText(action.ID)),
			slog.String("action", action.Action),
			slog.String("err", msg),
		)
	}
	return StateFailed, true, nil
}

// runInSavepoint executes one action inside a nested transaction
// (pgx models a savepoint as tx.Begin on a Tx). On error the savepoint
// rolls back, undoing the executor's domain write while leaving the
// outer tx — and its claim lock — intact.
func (h *ReaperJob) runInSavepoint(ctx context.Context, tx pgx.Tx, exec *executor, action ScheduledAction) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := exec.execute(ctx, New(sp), audit.New(sp), action); err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

func reapKey(t time.Time) string {
	return "scheduled_actions.reap." + t.Format(time.RFC3339)
}

// EnsureScheduled seeds one reaper tick if none is pending. Idempotent —
// safe on every boot; the idempotency key collapses duplicate seeds.
func EnsureScheduled(ctx context.Context, jobsSvc *jobs.Service) error {
	next := time.Now().UTC().Add(ReapInterval)
	if _, err := jobsSvc.Enqueue(ctx, JobTypeReap, ReapPayload{}, jobs.EnqueueOpts{
		ScheduledFor:   &next,
		IdempotencyKey: reapKey(next),
	}); err != nil {
		return fmt.Errorf("scheduledactions: bootstrap reaper: %w", err)
	}
	return nil
}
