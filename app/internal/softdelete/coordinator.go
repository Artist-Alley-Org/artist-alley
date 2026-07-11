// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package softdelete

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// JobTypeGC is the periodic hard-delete-past-retention job.
// Self-re-enqueues at the next configured wake-hour so no external
// cron is needed. One-in-flight enforced via idempotency key on
// the next-tick timestamp.
const JobTypeGC jobs.JobType = "soft_delete.gc"

// DefaultBatchSize caps the per-entity per-tick delete count so a
// large soft-delete backlog can't wedge one goroutine. 100 matches
// the saved-search coordinator's shape.
const DefaultBatchSize = 100

// CoordinatorPayload is the JSON body of a gc coordinator job.
// Empty today — all state comes from sysconfig at tick time.
type CoordinatorPayload struct{}

// CoordinatorJob implements jobs.Handler for JobTypeGC.
//
// Runs the four HardDeletePast* passes in order, sums the counts,
// and re-enqueues itself for the next configured wake hour.
// Reads sysconfig every tick so operator retention changes take
// effect on the next pass without a restart.
type CoordinatorJob struct {
	Service   *Service
	Sysconfig *sysconfig.Store
	Jobs      *jobs.Service
	Logger    *slog.Logger
}

// Type implements jobs.Handler.
func (h *CoordinatorJob) Type() jobs.JobType { return JobTypeGC }

// Handle runs one gc pass across all four entities.
func (h *CoordinatorJob) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	cfg, err := h.Sysconfig.GetSoftDelete(ctx)
	if err != nil {
		return nil, fmt.Errorf("softdelete.gc: read sysconfig: %w", err)
	}

	assetN, err := h.Service.HardDeletePastAssets(ctx, cfg.AssetRetentionDays, DefaultBatchSize)
	if err != nil {
		h.logErr(ctx, "assets", err)
	}
	postN, err := h.Service.HardDeletePastPosts(ctx, cfg.PostRetentionDays, DefaultBatchSize)
	if err != nil {
		h.logErr(ctx, "posts", err)
	}
	collN, err := h.Service.HardDeletePastCollections(ctx, cfg.CollectionRetentionDays, DefaultBatchSize)
	if err != nil {
		h.logErr(ctx, "collections", err)
	}
	userN, err := h.Service.HardDeletePastArchivedUsers(ctx, cfg.UserRetentionDays, DefaultBatchSize)
	if err != nil {
		h.logErr(ctx, "user", err)
	}

	// Self re-enqueue at the next configured wake-hour. Idempotency
	// key uses the next-tick timestamp so multiple ticks scheduled
	// at once collapse to one row (same pattern as saved-search
	// coordinator).
	nextTick := nextWakeHour(time.Now().UTC(), cfg.GCHourUTC)
	if _, err := h.Jobs.Enqueue(ctx, JobTypeGC, CoordinatorPayload{}, jobs.EnqueueOpts{
		ScheduledFor:   &nextTick,
		IdempotencyKey: "soft_delete.gc." + nextTick.Format(time.RFC3339),
	}); err != nil {
		return nil, fmt.Errorf("softdelete.gc: re-enqueue: %w", err)
	}

	result := map[string]any{
		"assets_deleted":      assetN,
		"posts_deleted":       postN,
		"collections_deleted": collN,
		"users_deleted":       userN,
		"next_tick_at":        nextTick,
	}
	b, _ := json.Marshal(result)
	return b, nil
}

func (h *CoordinatorJob) logErr(ctx context.Context, entity string, err error) {
	if h.Logger != nil {
		h.Logger.LogAttrs(ctx, slog.LevelWarn,
			"softdelete.gc.entity_error",
			slog.String("entity", entity),
			slog.String("err", err.Error()),
		)
	}
}

// nextWakeHour returns the next UTC timestamp at hour = hourUTC.
// If now is before today's hour, return today's; otherwise
// tomorrow's.
func nextWakeHour(now time.Time, hourUTC int) time.Time {
	today := time.Date(now.Year(), now.Month(), now.Day(), hourUTC, 0, 0, 0, time.UTC)
	if now.Before(today) {
		return today
	}
	return today.Add(24 * time.Hour)
}

// EnsureScheduled enqueues one initial gc coordinator tick if none
// is pending. Idempotent — safe to call on every boot. Uses the
// same idempotency key shape as the self-re-enqueue path so
// multiple boots don't stack duplicate coordinator rows.
func EnsureScheduled(ctx context.Context, jobsSvc *jobs.Service, sysconfigStore *sysconfig.Store) error {
	cfg, err := sysconfigStore.GetSoftDelete(ctx)
	if err != nil {
		return fmt.Errorf("softdelete: bootstrap gc: read sysconfig: %w", err)
	}
	nextTick := nextWakeHour(time.Now().UTC(), cfg.GCHourUTC)
	if _, err := jobsSvc.Enqueue(ctx, JobTypeGC, CoordinatorPayload{}, jobs.EnqueueOpts{
		ScheduledFor:   &nextTick,
		IdempotencyKey: "soft_delete.gc." + nextTick.Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("softdelete: bootstrap gc: enqueue: %w", err)
	}
	return nil
}
