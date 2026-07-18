// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// CapStorageRead gates the read-only admin view of storage usage
// (#402), so an auditor role can answer "what is using the disk"
// without the system.admin wildcard. Mirrors jobs.CapJobsRead: reads on
// the read cap, and every MUTATING storage tool (orphan sweep, checksum
// re-verify, reimport) stays system.admin in later sprints.
const CapStorageRead = "system.storage.read"

// AdminHandler owns the read-only aggregate queries for the storage
// admin surface. It never mutates an object, a variant, or a pin.
type AdminHandler struct {
	q *Queries
	// jobsSvc enqueues sweep jobs (#403). Nil in contexts that only
	// read (the S2 usage/variants surface), so TriggerSweep reports
	// unavailable rather than panicking.
	jobsSvc *jobs.Service
}

// WithJobs returns the handler wired to enqueue sweep jobs.
func (h *AdminHandler) WithJobs(s *jobs.Service) *AdminHandler {
	h.jobsSvc = s
	return h
}

// NewAdminHandler wires the storage admin handler off the pool,
// matching jobs.NewAdminHandler.
func NewAdminHandler(pool *pgxpool.Pool) *AdminHandler {
	return &AdminHandler{q: New(pool)}
}

// Usage is the whole-install rollup behind the `usage` tile.
//
// TotalBytes is deduplicated on-disk bytes: storage is content-
// addressed, so rows key on object_hash and a byte counted once is a
// byte stored once. It comes from storage_variants ALONE — that table
// already carries one row per object under variant_key='original'
// mirroring storage_objects.size_bytes, so adding storage_objects would
// double-count every original.
type Usage struct {
	ObjectCount     int64
	VariantCount    int64
	TotalBytes      int64
	OriginalBytes   int64
	DerivativeBytes int64
	ByContentType   []ContentTypeRow
	ByBackend       []BackendRow
}

// ContentTypeRow is one content-type bucket.
type ContentTypeRow struct {
	ContentType  string
	VariantCount int64
	TotalBytes   int64
}

// BackendRow is the object count held by one storage backend.
type BackendRow struct {
	Backend     string
	ObjectCount int64
}

// FamilyRow is one variant family — the segment of variant_key before
// the first '/' ('turntable/0028.png' -> 'turntable'), or the whole key
// when it has no '/' ('original', 'hires'). Grouping this way is what
// makes the tile readable: variant_key itself is high-cardinality
// (2090 distinct on dev, one per HLS segment and turntable frame),
// while the family grain is ~12 rows.
type FamilyRow struct {
	Family       string
	VariantCount int64
	DistinctKeys int64
	ObjectCount  int64
	TotalBytes   int64
	NewestAt     *time.Time
}

// GetUsage returns the install-wide storage rollup plus the
// content-type and backend breakdowns. All three are small aggregates
// (22 content types, a handful of backends on dev), so there is no
// paging.
func (h *AdminHandler) GetUsage(ctx context.Context) (Usage, error) {
	totals, err := h.q.AdminStorageTotals(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("storage.AdminHandler.GetUsage: totals: %w", err)
	}
	out := Usage{
		ObjectCount:     totals.ObjectCount,
		VariantCount:    totals.VariantCount,
		TotalBytes:      totals.TotalBytes,
		OriginalBytes:   totals.OriginalBytes,
		DerivativeBytes: totals.DerivativeBytes,
	}

	cts, err := h.q.AdminStorageByContentType(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("storage.AdminHandler.GetUsage: content types: %w", err)
	}
	out.ByContentType = make([]ContentTypeRow, 0, len(cts))
	for _, r := range cts {
		out.ByContentType = append(out.ByContentType, ContentTypeRow{
			ContentType:  r.ContentType,
			VariantCount: r.VariantCount,
			TotalBytes:   r.TotalBytes,
		})
	}

	backends, err := h.q.AdminStorageByBackend(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("storage.AdminHandler.GetUsage: backends: %w", err)
	}
	out.ByBackend = make([]BackendRow, 0, len(backends))
	for _, r := range backends {
		out.ByBackend = append(out.ByBackend, BackendRow{
			Backend:     r.Backend,
			ObjectCount: r.ObjectCount,
		})
	}
	return out, nil
}

// ListFamilies returns the per-family variant inventory, largest first.
func (h *AdminHandler) ListFamilies(ctx context.Context) ([]FamilyRow, error) {
	rows, err := h.q.AdminStorageByFamily(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage.AdminHandler.ListFamilies: %w", err)
	}
	out := make([]FamilyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, FamilyRow{
			Family:       r.Family,
			VariantCount: r.VariantCount,
			DistinctKeys: r.DistinctKeys,
			ObjectCount:  r.ObjectCount,
			TotalBytes:   r.TotalBytes,
			NewestAt:     tsPtr(r.NewestAt),
		})
	}
	return out, nil
}

// tsPtr converts a nullable pgtype.Timestamptz to *time.Time (nil when
// the DB value is NULL) so the HTTP layer emits an omitted field rather
// than a zero time.
func tsPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// --- integrity sweeps (#403) -------------------------------------------------

// SweepRun is one sweep execution as the admin surface sees it.
type SweepRun struct {
	ID             uuid.UUID
	Kind           string
	Status         string
	ObjectsScanned int64
	FindingsCount  int64
	StartedAt      *time.Time
	FinishedAt     *time.Time
	Error          *string
}

// SweepFinding is one problem a sweep recorded.
type SweepFinding struct {
	ID         uuid.UUID
	Finding    string
	ObjectHash string
	VariantKey string
	Detail     string
	DetectedAt *time.Time
	ResolvedAt *time.Time
}

// TriggerSweep opens a run row and enqueues the first batch. The job
// re-enqueues itself with an advancing cursor until the scan completes,
// so this returns as soon as the work is queued rather than blocking on
// a full scan.
func (h *AdminHandler) TriggerSweep(ctx context.Context, kind string, byUser *int64) (uuid.UUID, error) {
	var jobType jobs.JobType
	switch kind {
	case "orphan_scan":
		jobType = JobOrphanScan
	case "checksum_verify":
		jobType = JobChecksumVerify
	default:
		return uuid.Nil, fmt.Errorf("storage: unknown sweep kind %q", kind)
	}
	if h.jobsSvc == nil {
		return uuid.Nil, errors.New("storage: sweeps unavailable (no job service)")
	}

	runID := uuid.New()
	if _, err := h.q.CreateSweepRun(ctx, CreateSweepRunParams{
		ID:                 pgtype.UUID{Bytes: runID, Valid: true},
		Kind:               kind,
		TriggeredByUserRef: byUser,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("storage.TriggerSweep: create run: %w", err)
	}
	payload, err := json.Marshal(sweepPayload{RunID: runID})
	if err != nil {
		return uuid.Nil, fmt.Errorf("storage.TriggerSweep: payload: %w", err)
	}
	if _, err := h.jobsSvc.Enqueue(ctx, jobType, json.RawMessage(payload), jobs.EnqueueOpts{}); err != nil {
		return uuid.Nil, fmt.Errorf("storage.TriggerSweep: enqueue: %w", err)
	}
	return runID, nil
}

// ListSweepRuns returns recent runs, newest first, optionally filtered
// by kind.
func (h *AdminHandler) ListSweepRuns(ctx context.Context, kind *string, limit, offset int32) ([]SweepRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := h.q.ListSweepRuns(ctx, ListSweepRunsParams{Kind: kind, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("storage.ListSweepRuns: %w", err)
	}
	out := make([]SweepRun, 0, len(rows))
	for _, r := range rows {
		run := SweepRun{
			Kind:           r.Kind,
			Status:         r.Status,
			ObjectsScanned: r.ObjectsScanned,
			FindingsCount:  r.FindingsCount,
			Error:          r.Error,
			StartedAt:      tsPtr(r.StartedAt),
			FinishedAt:     tsPtr(r.FinishedAt),
		}
		if r.ID.Valid {
			run.ID = uuid.UUID(r.ID.Bytes)
		}
		out = append(out, run)
	}
	return out, nil
}

// ListSweepFindings returns a page of findings for one run, plus the
// total under the same filter.
func (h *AdminHandler) ListSweepFindings(ctx context.Context, runID uuid.UUID, finding *string, limit, offset int32) ([]SweepFinding, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	pgRun := pgtype.UUID{Bytes: runID, Valid: true}
	rows, err := h.q.ListSweepFindings(ctx, ListSweepFindingsParams{
		RunID: pgRun, Finding: finding, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("storage.ListSweepFindings: %w", err)
	}
	out := make([]SweepFinding, 0, len(rows))
	for _, r := range rows {
		f := SweepFinding{
			Finding:    r.Finding,
			ObjectHash: r.ObjectHash,
			VariantKey: r.VariantKey,
			Detail:     r.Detail,
			DetectedAt: tsPtr(r.DetectedAt),
			ResolvedAt: tsPtr(r.ResolvedAt),
		}
		if r.ID.Valid {
			f.ID = uuid.UUID(r.ID.Bytes)
		}
		out = append(out, f)
	}
	total, err := h.q.CountSweepFindings(ctx, CountSweepFindingsParams{RunID: pgRun, Finding: finding})
	if err != nil {
		return nil, 0, fmt.Errorf("storage.ListSweepFindings: count: %w", err)
	}
	return out, total, nil
}
