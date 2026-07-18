// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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
