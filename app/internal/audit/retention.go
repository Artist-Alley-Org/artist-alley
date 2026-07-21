// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// DefaultRetention is the floor applied to any category with no
// explicit policy row: 7 years, the legal minimum in most
// jurisdictions (ADR 0032).
const DefaultRetention = 7 * 365 * 24 * time.Hour

// JobTypeRetention is the nightly retention purge.
//
// DESIGN NOTE (the #467 design call): ADR 0032 says retention "runs via
// the scheduled-action engine". That engine (#466) is PER-TARGET — one
// action, one entity — and category retention is a BULK set-purge
// (DELETE ... WHERE category = X AND occurred_at < cutoff AND NOT
// legal_hold), which is the wrong shape for it. The right fit is the
// recurring-job infrastructure the engine's reaper itself rides: this
// is a recurring job on the same queue, self-re-enqueueing like
// soft_delete.gc. A considered refinement of the ADR's wording, still
// recurring and engine-adjacent, not a divergence from intent.
const JobTypeRetention jobs.JobType = "audit.retention"

// retentionPurgeBatch caps one DELETE so a large backlog can't wedge a
// worker in one giant transaction; the purge loops until a category's
// batch comes back short.
const retentionPurgeBatch = 5000

// RetentionPayload is the (empty) job body — all state is the policy
// table + the clock.
type RetentionPayload struct{}

// RetentionJob implements jobs.Handler for JobTypeRetention.
type RetentionJob struct {
	Pool      *pgxpool.Pool
	Jobs      *jobs.Service
	Rec       *Recorder
	Logger    *slog.Logger
	GCHourUTC int // nightly wake hour
}

// Type implements jobs.Handler.
func (h *RetentionJob) Type() jobs.JobType { return JobTypeRetention }

// Handle runs one nightly purge across every category, then
// re-enqueues for the next night.
func (h *RetentionJob) Handle(ctx context.Context, _ *jobs.Claim) (json.RawMessage, error) {
	total, perCat, err := h.purgeAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit.retention: %w", err)
	}

	next := nextWakeHour(time.Now().UTC(), h.GCHourUTC)
	if _, err := h.Jobs.Enqueue(ctx, JobTypeRetention, RetentionPayload{}, jobs.EnqueueOpts{
		ScheduledFor:   &next,
		IdempotencyKey: "audit.retention." + next.Format(time.RFC3339),
	}); err != nil {
		return nil, fmt.Errorf("audit.retention: re-enqueue: %w", err)
	}

	result := map[string]any{"deleted": total, "per_category": perCat, "next_tick_at": next}
	b, _ := json.Marshal(result)
	return b, nil
}

// PurgeAll runs the purge once across all categories. Exported so a
// test (and any future manual admin trigger) can drive it without the
// job wrapper. Returns the total deleted and a per-category breakdown.
func (h *RetentionJob) purgeAll(ctx context.Context) (int64, map[string]int64, error) {
	q := New(h.Pool)

	// Load explicit policies; everything else uses DefaultRetention.
	policies, err := q.ListRetentionPolicies(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("load policies: %w", err)
	}
	retentionFor := map[string]time.Duration{}
	for _, p := range policies {
		if d, ok := intervalToDuration(p.Retention); ok {
			retentionFor[p.Category] = d
		}
	}

	cats, err := q.DistinctAuditCategories(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("list categories: %w", err)
	}

	var total int64
	perCat := map[string]int64{}
	now := time.Now().UTC()
	for _, cptr := range cats {
		if cptr == nil || *cptr == "" {
			continue
		}
		cat := *cptr
		ret := DefaultRetention
		if d, ok := retentionFor[cat]; ok {
			ret = d
		}
		cutoff := now.Add(-ret)

		var deleted int64
		for {
			n, err := q.PurgeAuditCategoryBatch(ctx, PurgeAuditCategoryBatchParams{
				Category: cat,
				Cutoff:   pgtype.Timestamptz{Time: cutoff, Valid: true},
				Lim:      retentionPurgeBatch,
			})
			if err != nil {
				return total, perCat, fmt.Errorf("purge %q: %w", cat, err)
			}
			deleted += n
			if n < retentionPurgeBatch {
				break
			}
		}
		if deleted > 0 {
			total += deleted
			perCat[cat] = deleted
			// The purge is auditable (ADR 0032). One event per category
			// that actually deleted rows — a no-op category writes
			// nothing, so the log isn't spammed nightly with zeros.
			h.Rec.RetentionPurged(ctx, cat, deleted, cutoff)
		}
	}
	return total, perCat, nil
}

// intervalToDuration converts a pgtype.Interval (months/days/micros)
// to a Duration. Retention intervals are always day/year granular, so
// months are treated as 30 days and years as 365 — exact enough for a
// nightly cutoff, which is itself day-granular.
func intervalToDuration(iv pgtype.Interval) (time.Duration, bool) {
	if !iv.Valid {
		return 0, false
	}
	d := time.Duration(iv.Microseconds) * time.Microsecond
	d += time.Duration(iv.Days) * 24 * time.Hour
	d += time.Duration(iv.Months) * 30 * 24 * time.Hour
	return d, true
}

// nextWakeHour returns the next UTC timestamp at hour = hourUTC.
func nextWakeHour(now time.Time, hourUTC int) time.Time {
	today := time.Date(now.Year(), now.Month(), now.Day(), hourUTC, 0, 0, 0, time.UTC)
	if now.Before(today) {
		return today
	}
	return today.Add(24 * time.Hour)
}

// EnsureRetentionScheduled seeds one purge tick if none is pending.
// Idempotent — safe on every boot.
func EnsureRetentionScheduled(ctx context.Context, jobsSvc *jobs.Service, gcHourUTC int) error {
	next := nextWakeHour(time.Now().UTC(), gcHourUTC)
	if _, err := jobsSvc.Enqueue(ctx, JobTypeRetention, RetentionPayload{}, jobs.EnqueueOpts{
		ScheduledFor:   &next,
		IdempotencyKey: "audit.retention." + next.Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("audit: bootstrap retention: %w", err)
	}
	return nil
}

// Tombstone anonymizes a user's actor identity across all their audit
// events for a GDPR DSAR (ADR 0024). The rows are PRESERVED; only the
// numeric ref is cleared and a `deleted-user-{ref}` pseudonym recorded.
// Returns the number of rows rewritten.
func Tombstone(ctx context.Context, pool *pgxpool.Pool, userRef int64) (int64, error) {
	return New(pool).TombstoneActor(ctx, fmt.Sprint(userRef))
}
