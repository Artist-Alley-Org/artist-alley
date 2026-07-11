// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package saved

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// JobTypeCoordinator is the periodic wake-up job that walks due
// saved-search rows + enqueues per-row [JobTypeRun] children.
const JobTypeCoordinator jobs.JobType = "saved_search.notify_coordinator"

// JobTypeRun is the per-saved-search execution job. Consumes one
// row, runs it, computes delta, persists, notifies.
const JobTypeRun jobs.JobType = "saved_search.notify_run"

// DefaultCoordinatorWakeSeconds is the fallback wake cadence
// when sysconfig search.saved_search.coordinator_wake_seconds is
// unset. 60s is aggressive but bounded because the walk uses the
// partial-index-covered ListDue query.
const DefaultCoordinatorWakeSeconds = 60

// DefaultCoordinatorBatchSize caps rows one tick processes so a
// large table can't wedge one goroutine.
const DefaultCoordinatorBatchSize = 100

// CoordinatorPayload is the JSON body of a coordinator job. Empty
// today (all state comes from the DB); reserved for future
// per-tick knobs.
type CoordinatorPayload struct{}

// CoordinatorJob implements jobs.Handler for JobTypeCoordinator.
// Self-re-enqueues at the end of every tick via
// EnqueueOpts.ScheduledFor so no external cron is needed.
type CoordinatorJob struct {
	Store         *Store
	Jobs          *jobs.Service
	Logger        *slog.Logger
	WakeSeconds   int
	BatchSize     int32
	Counter       CoordinatorCounter
}

// CoordinatorCounter is the observability hook. Nil-safe.
type CoordinatorCounter interface {
	RecordCoordinatorTick(dueCount int)
	RecordRunResult(result string)
	RecordNotificationSent()
	RecordDeltaHit()
}

// Type implements jobs.Handler.
func (h *CoordinatorJob) Type() jobs.JobType { return JobTypeCoordinator }

// Handle walks due rows in one batch + enqueues per-row runs +
// re-enqueues itself for the next wake cadence.
func (h *CoordinatorJob) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	batchSize := h.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultCoordinatorBatchSize
	}
	due, err := h.Store.ListDue(ctx, batchSize)
	if err != nil {
		return nil, fmt.Errorf("saved.coordinator: list due: %w", err)
	}
	if h.Counter != nil {
		h.Counter.RecordCoordinatorTick(len(due))
	}
	enqueued := 0
	for _, row := range due {
		if _, err := h.Jobs.Enqueue(ctx, JobTypeRun, RunPayload{SavedSearchID: row.ID}, jobs.EnqueueOpts{
			// Idempotency: (saved_search_id, last_run_at) — a
			// tick that runs before the previous notify_run
			// wrote a fresh last_run_at doesn't double-enqueue.
			IdempotencyKey: coordinatorIdempotencyKey(row),
		}); err != nil {
			if h.Logger != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn,
					"saved.coordinator.enqueue_error",
					slog.String("saved_search_id", row.ID.String()),
					slog.String("err", err.Error()),
				)
			}
			continue
		}
		enqueued++
	}

	// Self re-enqueue for the next tick.
	wake := h.WakeSeconds
	if wake <= 0 {
		wake = DefaultCoordinatorWakeSeconds
	}
	nextTick := time.Now().Add(time.Duration(wake) * time.Second)
	if _, err := h.Jobs.Enqueue(ctx, JobTypeCoordinator, CoordinatorPayload{}, jobs.EnqueueOpts{
		ScheduledFor: &nextTick,
		// A single-in-flight coordinator invariant — the
		// idempotency key uses "next-tick-time" so multiple ticks
		// scheduled at once collapse to one row.
		IdempotencyKey: "saved_search.coordinator." + nextTick.UTC().Format(time.RFC3339),
	}); err != nil {
		return nil, fmt.Errorf("saved.coordinator: reenqueue: %w", err)
	}

	result := map[string]any{
		"due_count":      len(due),
		"enqueued_count": enqueued,
		"next_tick_at":   nextTick,
	}
	b, _ := json.Marshal(result)
	return b, nil
}

// RunPayload is the per-saved-search execution job body.
type RunPayload struct {
	SavedSearchID uuid.UUID `json:"saved_search_id"`
}

// RunJob implements jobs.Handler for JobTypeRun.
type RunJob struct {
	Store    *Store
	Executor *Executor
	Notifier *Notifier
	SiteURL  string
	Logger   *slog.Logger
	Counter  CoordinatorCounter
}

// Type implements jobs.Handler.
func (h *RunJob) Type() jobs.JobType { return JobTypeRun }

// Handle loads the row, runs the search, computes delta, persists
// state, notifies (if delta + email). Errors classify per
// jobs.IsTerminal:
//
//   - row deleted between coordinator + run  →  TerminalError
//   - DSL parse failure                       →  TerminalError
//     (fixing takes an admin edit, not a retry)
//   - Engine transient failure                →  retryable
//   - Store write transient failure           →  retryable
func (h *RunJob) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p RunPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("saved.run: parse payload: %w", err)}
	}
	row, err := h.Store.Get(ctx, p.SavedSearchID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, &jobs.TerminalError{Err: fmt.Errorf("saved.run: row %s gone", p.SavedSearchID)}
		}
		return nil, fmt.Errorf("saved.run: fetch row: %w", err)
	}
	if !row.Enabled {
		if h.Counter != nil {
			h.Counter.RecordRunResult("disabled")
		}
		return jsonMarshalOk(map[string]any{"skipped": "disabled"}), nil
	}

	res, err := h.Executor.Run(ctx, row)
	if err != nil {
		if h.Counter != nil {
			h.Counter.RecordRunResult("error")
		}
		return nil, fmt.Errorf("saved.run: execute: %w", err)
	}
	delta := ComputeDelta(row, res)

	sent := false
	if delta.HashChanged {
		if h.Counter != nil {
			h.Counter.RecordDeltaHit()
		}
	}
	if h.Notifier != nil {
		emitted, err := h.Notifier.Emit(ctx, row, delta, res, h.SiteURL)
		if err != nil {
			if h.Logger != nil {
				h.Logger.LogAttrs(ctx, slog.LevelWarn,
					"saved.run.notify_error",
					slog.String("saved_search_id", row.ID.String()),
					slog.String("err", err.Error()),
				)
			}
		}
		sent = emitted
		if sent && h.Counter != nil {
			h.Counter.RecordNotificationSent()
		}
	}

	if _, err := h.Store.RecordRun(ctx, row.ID, res.Hash, res.HitIDs, sent); err != nil {
		return nil, fmt.Errorf("saved.run: record: %w", err)
	}
	if h.Counter != nil {
		if len(res.HitIDs) == 0 {
			h.Counter.RecordRunResult("empty")
		} else {
			h.Counter.RecordRunResult("hit")
		}
	}
	return jsonMarshalOk(map[string]any{
		"hit_count":   len(res.HitIDs),
		"added_count": len(delta.Added),
		"notified":    sent,
	}), nil
}

// coordinatorIdempotencyKey builds a key so a re-fired coordinator
// tick doesn't enqueue the same row twice for the same window.
func coordinatorIdempotencyKey(row Row) string {
	// Use last_run_at (or "never") as the window discriminator —
	// once notify_run writes a fresh last_run_at, the next
	// coordinator tick will produce a NEW key and enqueue a new
	// child. Same window → same key → deduped.
	window := "never"
	if row.LastRunAt != nil {
		window = row.LastRunAt.UTC().Format(time.RFC3339)
	}
	return "saved_search.run:" + row.ID.String() + ":" + window
}

func jsonMarshalOk(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
