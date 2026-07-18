// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CapJobsRead gates the read-only admin view of the job queue (#400),
// so an auditor role can watch the async pipeline (jobs by status/type,
// active workers, live counts) without the system.admin wildcard. The
// MUTATING admin actions (requeue/cancel, #401) gate on system.admin at
// the HTTP layer, mirroring the metadata-extraction admin surface
// (#356) — reads on the read cap, writes on system.admin.
const CapJobsRead = "system.jobs.read"

// ErrJobNotFound / ErrJobNotActionable let the HTTP layer distinguish a
// missing job (404) from one whose current status forbids the action
// (409) — e.g. requeuing a running job, or cancelling a done one.
var (
	ErrJobNotFound      = errors.New("jobs: job not found")
	ErrJobNotActionable = errors.New("jobs: job is not in a state that allows this action")
)

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

// RequeueJob sends a failed/cancelled job back to the pending pool
// (#401). Returns ErrJobNotActionable if the job exists but is running
// or already done (the WHERE guard matched no row), ErrJobNotFound if
// the id doesn't exist. Never touches a running job.
func (h *AdminHandler) RequeueJob(ctx context.Context, id uuid.UUID) error {
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	n, err := h.q.AdminRequeueJob(ctx, pgID)
	if err != nil {
		return fmt.Errorf("jobs.AdminHandler.RequeueJob: %w", err)
	}
	if n == 0 {
		return h.classifyNoAction(ctx, pgID)
	}
	return nil
}

// CancelJob moves a pending/failed job to cancelled (#401). Same
// error contract as RequeueJob. A running job cannot be cancelled this
// sprint (cooperative running-cancel is out of scope).
func (h *AdminHandler) CancelJob(ctx context.Context, id uuid.UUID) error {
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	n, err := h.q.AdminCancelJob(ctx, pgID)
	if err != nil {
		return fmt.Errorf("jobs.AdminHandler.CancelJob: %w", err)
	}
	if n == 0 {
		return h.classifyNoAction(ctx, pgID)
	}
	return nil
}

// classifyNoAction turns a zero-rows guarded UPDATE into the right
// error: the job doesn't exist (404) vs. it exists but its status
// forbade the action (409). The probe runs only on the miss path, so
// the happy path stays one query.
func (h *AdminHandler) classifyNoAction(ctx context.Context, id pgtype.UUID) error {
	var exists bool
	if err := h.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("jobs.AdminHandler.classifyNoAction: %w", err)
	}
	if !exists {
		return ErrJobNotFound
	}
	return ErrJobNotActionable
}

// ScheduledRow is one future-dated pending job.
type ScheduledRow struct {
	ID           uuid.UUID
	Type         string
	Status       string
	Priority     int32
	Attempts     int32
	MaxAttempts  int32
	ScheduledFor *time.Time
	EnqueuedAt   *time.Time
	DueInSeconds int64
}

// ListScheduledJobs returns future-dated pending work, paginated, plus
// the total (#401). Read-only — there is no scheduler to configure.
func (h *AdminHandler) ListScheduledJobs(ctx context.Context, limit, offset int32) ([]ScheduledRow, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := h.q.AdminListScheduledJobs(ctx, AdminListScheduledJobsParams{Limit: limit, Offset: offset})
	if err != nil {
		return nil, 0, fmt.Errorf("jobs.AdminHandler.ListScheduledJobs: list: %w", err)
	}
	out := make([]ScheduledRow, 0, len(rows))
	for _, r := range rows {
		sr := ScheduledRow{
			Type:         r.Type,
			Status:       r.Status,
			Priority:     r.Priority,
			Attempts:     r.Attempts,
			MaxAttempts:  r.MaxAttempts,
			DueInSeconds: r.DueInSeconds,
		}
		if r.ID.Valid {
			sr.ID = uuid.UUID(r.ID.Bytes)
		}
		sr.ScheduledFor = tsPtr(r.ScheduledFor)
		sr.EnqueuedAt = tsPtr(r.EnqueuedAt)
		out = append(out, sr)
	}
	total, err := h.q.AdminCountScheduledJobs(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("jobs.AdminHandler.ListScheduledJobs: count: %w", err)
	}
	return out, total, nil
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
