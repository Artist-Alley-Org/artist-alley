// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CapJobsRead gates the read-only admin view of the job queue (#400),
// so an auditor role can watch the async pipeline (jobs by status/type,
// active workers, live counts) without the system.admin wildcard. There
// is NO write path under this cap — requeue/cancel/concurrency-edit stay
// system.admin (Sprint 1, #401). The gate itself lives at the HTTP layer
// (internal/http/api.go), mirroring the metadata-extraction admin
// surface (#356).
const CapJobsRead = "system.jobs.read"

// AdminHandler owns the read-only admin queries for the jobs queue. The
// worker path (Enqueue/Claim/Complete/Fail) writes rows; this handler is
// the operator's mirror surface. It never mutates a job.
type AdminHandler struct {
	pool *pgxpool.Pool
	q    *Queries
}

// NewAdminHandler wires the jobs admin handler off the pool, matching
// metadata.NewAdminHandler.
func NewAdminHandler(pool *pgxpool.Pool) *AdminHandler {
	return &AdminHandler{pool: pool, q: New(pool)}
}

// ListJobsFilter mirrors the listJobs query-param shape. Nil pointer
// fields mean "no filter". Limit is capped 1-200 by ListJobs.
type ListJobsFilter struct {
	Status *string
	Type   *string
	Limit  int32
	Offset int32
}

// JobRow is the admin-side projection of one job row in plain Go types
// (no pgtype), so the HTTP layer can marshal directly. Payload + result
// are deliberately omitted — the queue view shows metadata, not job
// bodies, which can be large and carry sensitive fields.
type JobRow struct {
	ID             uuid.UUID
	Type           string
	Status         string
	Priority       int32
	Attempts       int32
	MaxAttempts    int32
	ClaimedBy      *string
	ClaimedAt      *time.Time
	LeaseExpiresAt *time.Time
	LastError      *string
	OriginServerID *uuid.UUID
	ScheduledFor   *time.Time
	EnqueuedAt     *time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	AgeSeconds     int64
}

// ListJobs returns the filtered + paginated page along with the TOTAL
// count under the same filter (for the UI's pager), mirroring
// metadata.AdminHandler.ListFailures.
func (h *AdminHandler) ListJobs(ctx context.Context, f ListJobsFilter) ([]JobRow, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}

	rows, err := h.q.AdminListJobs(ctx, AdminListJobsParams{
		Limit:  f.Limit,
		Offset: f.Offset,
		Status: f.Status,
		Type:   f.Type,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("jobs.AdminHandler.ListJobs: list: %w", err)
	}
	out := make([]JobRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, jobRowFromDB(r))
	}

	total, err := h.q.AdminCountJobs(ctx, AdminCountJobsParams{Status: f.Status, Type: f.Type})
	if err != nil {
		return nil, 0, fmt.Errorf("jobs.AdminHandler.ListJobs: count: %w", err)
	}
	return out, total, nil
}

// WorkerRow is one busy worker holding one running job.
type WorkerRow struct {
	ClaimedBy      string
	JobID          uuid.UUID
	Type           string
	Priority       int32
	Attempts       int32
	ClaimedAt      *time.Time
	LeaseExpiresAt *time.Time
	LeaseStale     bool
}

// ListActiveWorkers returns one row per running job — the operator's
// view of what each worker is holding and whether its lease has gone
// stale (RequeueStuckJobs will reclaim a stale one).
func (h *AdminHandler) ListActiveWorkers(ctx context.Context) ([]WorkerRow, error) {
	rows, err := h.q.AdminListActiveWorkers(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs.AdminHandler.ListActiveWorkers: %w", err)
	}
	out := make([]WorkerRow, 0, len(rows))
	for _, r := range rows {
		w := WorkerRow{
			Type:       r.Type,
			Priority:   r.Priority,
			Attempts:   r.Attempts,
			LeaseStale: r.LeaseStale,
		}
		if r.ClaimedBy != nil {
			w.ClaimedBy = *r.ClaimedBy
		}
		if r.JobID.Valid {
			w.JobID = uuid.UUID(r.JobID.Bytes)
		}
		w.ClaimedAt = tsPtr(r.ClaimedAt)
		w.LeaseExpiresAt = tsPtr(r.LeaseExpiresAt)
		out = append(out, w)
	}
	return out, nil
}

// StatusCount is one (type, status) → count bucket.
type StatusCount struct {
	Type   string
	Status string
	Count  int64
}

// StatusCounts returns the live per-(type, status) counts — reuses the
// existing CountJobsByStatus query so the "live" tile and the admin
// aggregate share one source.
func (h *AdminHandler) StatusCounts(ctx context.Context) ([]StatusCount, error) {
	rows, err := h.q.CountJobsByStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs.AdminHandler.StatusCounts: %w", err)
	}
	out := make([]StatusCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, StatusCount{Type: r.Type, Status: r.Status, Count: r.Count})
	}
	return out, nil
}

func jobRowFromDB(r AdminListJobsRow) JobRow {
	out := JobRow{
		Type:        r.Type,
		Status:      r.Status,
		Priority:    r.Priority,
		Attempts:    r.Attempts,
		MaxAttempts: r.MaxAttempts,
		ClaimedBy:   r.ClaimedBy,
		LastError:   r.LastError,
		AgeSeconds:  r.AgeSeconds,
	}
	if r.ID.Valid {
		out.ID = uuid.UUID(r.ID.Bytes)
	}
	if r.OriginServerID.Valid {
		id := uuid.UUID(r.OriginServerID.Bytes)
		out.OriginServerID = &id
	}
	out.ClaimedAt = tsPtr(r.ClaimedAt)
	out.LeaseExpiresAt = tsPtr(r.LeaseExpiresAt)
	out.ScheduledFor = tsPtr(r.ScheduledFor)
	out.EnqueuedAt = tsPtr(r.EnqueuedAt)
	out.StartedAt = tsPtr(r.StartedAt)
	out.FinishedAt = tsPtr(r.FinishedAt)
	return out
}

// tsPtr converts a nullable pgtype.Timestamptz to *time.Time (nil when
// the DB value is NULL) so the HTTP layer emits a JSON null / omitted
// field rather than a zero time.
func tsPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
