// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package disk_usage implements Phase 1.16.B-5's GET
// /admin/search/disk-usage endpoint + the pg_stat-backed gauges
// the health snapshot surfaces.
//
// Two consumers:
//
//   - The disk-usage endpoint itself renders the full breakdown
//     (tsvector bytes per entity, embedding table + index sizes,
//     cache footprint, saved-search + reindex row counts).
//   - The health snapshot's Notes[] section reads the gauge
//     subset (assets_pending_embedding, asset_embedding_row_count,
//     asset_embedding_index_size_mb, saved_search_active_gauge).
//
// Both paths share the same Snapshot builder + a 30-second-TTL
// single-entry LRU (DiskUsageCache) so pg_relation_size + count(*)
// queries don't run on every admin page load.
package disk_usage

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTTL is the freshness window for the cached snapshot.
// pg_relation_size can be seconds-slow on large corpora; the
// cache absorbs the load while operators refresh their dashboards.
const DefaultTTL = 30 * time.Second

// Snapshot is the full disk-usage projection.
type Snapshot struct {
	// TsvectorBytes maps entity table name to the bytes stored
	// in that table's tsvector column. Missing entities contribute
	// zero.
	TsvectorBytes map[string]int64 `json:"tsvector_bytes"`

	// Embedding sizes for the pgvector table + HNSW index.
	EmbeddingTableBytes int64 `json:"embedding_table_bytes"`
	EmbeddingIndexBytes int64 `json:"embedding_index_bytes"`

	// AssetsPendingEmbedding counts assets missing an
	// asset_embedding_d768 row. Surfaced as a gauge on
	// /admin/search/health.
	AssetsPendingEmbedding int64 `json:"assets_pending_embedding"`

	// AssetEmbeddingRowCount is the total row count in
	// asset_embedding_d768. Gauge on /admin/search/health.
	AssetEmbeddingRowCount int64 `json:"asset_embedding_row_count"`

	// SavedSearchRows is the total row count in saved_search.
	SavedSearchRows int64 `json:"saved_search_rows"`

	// SavedSearchActive counts enabled saved-searches. Gauge on
	// /admin/search/health.
	SavedSearchActive int64 `json:"saved_search_active"`

	// SearchReindexHistoryRows is the total row count in
	// search_reindex_run.
	SearchReindexHistoryRows int64 `json:"search_reindex_history_rows"`

	// SnapshotAt is the wall-clock time the snapshot was
	// computed. Clients render as "N seconds stale" when the
	// cache serves an older copy.
	SnapshotAt time.Time `json:"snapshot_at"`
}

// Cache is a single-entry TTL cache for the Snapshot. Global not
// per-user — the snapshot is admin-facing infrastructure info.
type Cache struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	TTL    time.Duration

	mu      sync.Mutex
	current *Snapshot
	err     error
}

// NewCache constructs a cache with the default TTL.
func NewCache(pool *pgxpool.Pool, logger *slog.Logger) *Cache {
	return &Cache{Pool: pool, Logger: logger, TTL: DefaultTTL}
}

// Get returns the cached snapshot if fresh, otherwise recomputes
// via computeSnapshot. Errors returned inline; a single failed
// probe doesn't kill the whole snapshot — the field stays zero
// and the error surfaces on subsequent Get calls.
func (c *Cache) Get(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil && time.Since(c.current.SnapshotAt) < c.effectiveTTL() {
		return *c.current, c.err
	}
	snap, err := computeSnapshot(ctx, c.Pool, c.Logger)
	if err == nil {
		snap.SnapshotAt = time.Now()
	}
	c.current = &snap
	c.err = err
	return snap, err
}

// Refresh forces a recompute regardless of the cached freshness
// window. Called from the /admin/search/disk-usage?refresh=true
// path.
func (c *Cache) Refresh(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, err := computeSnapshot(ctx, c.Pool, c.Logger)
	if err == nil {
		snap.SnapshotAt = time.Now()
	}
	c.current = &snap
	c.err = err
	return snap, err
}

func (c *Cache) effectiveTTL() time.Duration {
	if c.TTL <= 0 {
		return DefaultTTL
	}
	return c.TTL
}

// computeSnapshot runs every query the Snapshot depends on. Each
// query wraps its errors + doesn't abort the whole snapshot on
// individual failure — a missing table on an older schema still
// produces a partial snapshot.
func computeSnapshot(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (Snapshot, error) {
	snap := Snapshot{TsvectorBytes: map[string]int64{}}

	// tsvector per-entity bytes via pg_column_size aggregate. Not
	// perfectly stable — pg_column_size sums the on-disk
	// representation which can drift with TOAST — but close enough
	// for operator-facing ballpark numbers.
	for _, tbl := range []string{"assets", "collections", "posts"} {
		var bytes int64
		err := pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT COALESCE(SUM(pg_column_size(search_text)), 0)::BIGINT
			  FROM %s
		`, tbl)).Scan(&bytes)
		if err != nil {
			logQuietly(ctx, logger, "disk_usage.tsvector."+tbl, err)
			continue
		}
		snap.TsvectorBytes[tbl] = bytes
	}

	// Embedding table + index sizes via pg_relation_size.
	if err := pool.QueryRow(ctx,
		`SELECT pg_relation_size('asset_embedding_d768')::BIGINT`,
	).Scan(&snap.EmbeddingTableBytes); err != nil {
		logQuietly(ctx, logger, "disk_usage.embedding_table_size", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(pg_relation_size(indexrelid))::BIGINT, 0)
		   FROM pg_index i
		   JOIN pg_class c ON c.oid = i.indrelid
		  WHERE c.relname = 'asset_embedding_d768'`,
	).Scan(&snap.EmbeddingIndexBytes); err != nil {
		logQuietly(ctx, logger, "disk_usage.embedding_index_size", err)
	}

	// Pending-embedding + total counts.
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM assets
		 WHERE deleted_at IS NULL
		   AND id NOT IN (SELECT asset_id FROM asset_embedding_d768)
	`).Scan(&snap.AssetsPendingEmbedding); err != nil {
		logQuietly(ctx, logger, "disk_usage.assets_pending_embedding", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM asset_embedding_d768`,
	).Scan(&snap.AssetEmbeddingRowCount); err != nil {
		logQuietly(ctx, logger, "disk_usage.embedding_row_count", err)
	}

	// Saved-search counts + reindex history.
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM saved_search`,
	).Scan(&snap.SavedSearchRows); err != nil {
		logQuietly(ctx, logger, "disk_usage.saved_search_rows", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM saved_search WHERE enabled = TRUE`,
	).Scan(&snap.SavedSearchActive); err != nil {
		logQuietly(ctx, logger, "disk_usage.saved_search_active", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM search_reindex_run`,
	).Scan(&snap.SearchReindexHistoryRows); err != nil {
		logQuietly(ctx, logger, "disk_usage.reindex_history_rows", err)
	}
	return snap, nil
}

func logQuietly(ctx context.Context, logger *slog.Logger, op string, err error) {
	if logger == nil {
		return
	}
	logger.LogAttrs(ctx, slog.LevelWarn, op, slog.String("err", err.Error()))
}
