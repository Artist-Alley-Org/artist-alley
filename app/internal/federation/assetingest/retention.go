// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// Retention of the transfer and fixity event ledger (#1574 slice 2,
// migration 00089). A row is kept for the retention period and removed
// only by this daily sweep. The database enforces the same bound: its
// DELETE trigger refuses a row younger than the transaction's
// aa.federation_event_retention (default 365 days, never under 30), which
// the sweep sets from the operator's AA_FEDERATION_EVENT_RETENTION_DAYS.

// DefaultEventRetentionDays and MinEventRetentionDays bound
// AA_FEDERATION_EVENT_RETENTION_DAYS (and the database's own default and
// floor).
const (
	DefaultEventRetentionDays = 365
	MinEventRetentionDays     = 30
)

// JobTypeEventRetention is the daily ledger retention sweep.
const JobTypeEventRetention jobs.JobType = "federation.transfer_events.retention"

// eventRetentionBatch caps one DELETE, so a large backlog never holds
// one long transaction; the sweep loops until a batch comes back short.
const eventRetentionBatch = 5000

// SweepEventRetention deletes ledger rows older than days, in batches, and
// returns how many it removed. days below MinEventRetentionDays is raised
// to it (the trigger would refuse them anyway).
func SweepEventRetention(ctx context.Context, pool *pgxpool.Pool, days int) (int64, error) {
	if days < MinEventRetentionDays {
		days = MinEventRetentionDays
	}
	setting := strconv.Itoa(days) + " days"
	var total int64
	for {
		var n int64
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('aa.federation_event_retention', $1, true)`, setting); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `DELETE FROM federation_transfer_events WHERE id IN (
				SELECT id FROM federation_transfer_events
				 WHERE occurred_at < now() - federation_transfer_event_retention()
				 ORDER BY occurred_at LIMIT $1)`, eventRetentionBatch)
			n = tag.RowsAffected()
			return err
		})
		if err != nil {
			return total, fmt.Errorf("assetingest: event retention: %w", err)
		}
		total += n
		if n < eventRetentionBatch {
			return total, nil
		}
	}
}

// EventRetentionPayload is the (empty) job body.
type EventRetentionPayload struct{}

// EventRetentionJob implements jobs.Handler for JobTypeEventRetention: it
// sweeps, then schedules itself for the next day.
type EventRetentionJob struct {
	Pool    *pgxpool.Pool
	Jobs    *jobs.Service
	Logger  *slog.Logger
	Days    int // AA_FEDERATION_EVENT_RETENTION_DAYS
	HourUTC int // daily wake hour
}

// Type implements jobs.Handler.
func (h *EventRetentionJob) Type() jobs.JobType { return JobTypeEventRetention }

// Handle implements jobs.Handler.
func (h *EventRetentionJob) Handle(ctx context.Context, _ *jobs.Claim) (json.RawMessage, error) {
	n, err := SweepEventRetention(ctx, h.Pool, h.Days)
	if err != nil {
		return nil, err
	}
	next := nextWake(time.Now().UTC(), h.HourUTC)
	if err := enqueueEventRetention(ctx, h.Jobs, next); err != nil {
		return nil, err
	}
	if h.Logger != nil && n > 0 {
		h.Logger.InfoContext(ctx, "federation.transfer_events.retention", "deleted", n, "retention_days", h.Days)
	}
	b, _ := json.Marshal(map[string]any{"deleted": n, "retention_days": h.Days, "next_tick_at": next})
	return b, nil
}

// EnsureEventRetentionScheduled seeds the next sweep if none is pending.
// Idempotent; safe on every boot.
func EnsureEventRetentionScheduled(ctx context.Context, jobsSvc *jobs.Service, hourUTC int) error {
	return enqueueEventRetention(ctx, jobsSvc, nextWake(time.Now().UTC(), hourUTC))
}

func enqueueEventRetention(ctx context.Context, jobsSvc *jobs.Service, at time.Time) error {
	if _, err := jobsSvc.Enqueue(ctx, JobTypeEventRetention, EventRetentionPayload{}, jobs.EnqueueOpts{
		ScheduledFor:   &at,
		IdempotencyKey: string(JobTypeEventRetention) + "." + at.Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("assetingest: schedule event retention: %w", err)
	}
	return nil
}

// nextWake is the next hourUTC:00 strictly after now.
func nextWake(now time.Time, hourUTC int) time.Time {
	today := time.Date(now.Year(), now.Month(), now.Day(), hourUTC, 0, 0, 0, time.UTC)
	if now.Before(today) {
		return today
	}
	return today.Add(24 * time.Hour)
}
