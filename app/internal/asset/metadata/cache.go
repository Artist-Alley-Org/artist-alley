// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"log/slog"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Cache holds the two LRU caches the metadata subsystem maintains:
//
//   - ExtractionConfig: the "which field-definitions are wired to
//     extract from EXIF/IPTC/XMP and how" list. Read on every
//     extract job + every backfill batch. Invalidated whenever
//     the field-definition admin handler updates an extraction
//     config. Single global key (the list rebuilds wholesale on
//     any field-def change — small data, simple invalidator).
//
//   - FailureCount: the admin badge count "N pending extraction
//     failures". Read on every page-load that renders the admin
//     navigation bar. Invalidated on extraction_failure insert +
//     bulk dismiss.
//
// Per project_caching_pattern: every domain has its own typed
// LRU registered with the process-wide [cache.Registry] so
// cross-instance pg_notify invalidations route correctly.
type Cache struct {
	ExtractionConfig *cache.Cache[[]FieldExtractionConfig]
	FailureCount     *cache.Cache[int]

	logger *slog.Logger
}

// Cache-domain identifiers — must be unique across the whole
// app's cache.Registry. Dotted-namespace to match the existing
// convention (e.g. "field_definition.id").
const (
	DomainExtractionConfig = "metadata.extraction_config"
	DomainFailureCount     = "metadata.failure_count"
)

// CacheKeyAll is the canonical key both caches use when a value
// covers the whole population (the config list + the global
// pending-failure count). Per-asset / per-format keys are not
// useful for either domain today.
const CacheKeyAll = "all"

// NewCache wires both caches into the registry. The LRU sizes are
// tiny because both domains hold ONE entry each (the
// CacheKeyAll value); leaving room for two entries handles future
// per-scope expansion without re-registering.
func NewCache(registry *cache.Registry, logger *slog.Logger) *Cache {
	return &Cache{
		ExtractionConfig: cache.Register[[]FieldExtractionConfig](registry, DomainExtractionConfig, 4),
		FailureCount:     cache.Register[int](registry, DomainFailureCount, 4),
		logger:           logger,
	}
}

// InvalidateExtractionConfig drops the cached extraction-config
// list locally + broadcasts to peers. Called by the
// field-definition admin handler after any PATCH/DELETE that
// could change which fields are wired to extraction.
//
// Logs (and swallows) NOTIFY errors — the local invalidation
// always succeeds, and a missed cross-instance broadcast becomes
// stale-config-by-one-extract-job. Soft observability surface.
func (c *Cache) InvalidateExtractionConfig(ctx context.Context) {
	if err := c.ExtractionConfig.Invalidate(ctx, CacheKeyAll); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn,
			"metadata.cache.extraction_config.invalidate_error",
			slog.String("err", err.Error()),
		)
	}
}

// InvalidateFailureCount drops the cached pending-failure count.
// Called from the extract-job handler after recording a new
// failure + from the admin bulk-dismiss path.
func (c *Cache) InvalidateFailureCount(ctx context.Context) {
	if err := c.FailureCount.Invalidate(ctx, CacheKeyAll); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn,
			"metadata.cache.failure_count.invalidate_error",
			slog.String("err", err.Error()),
		)
	}
}
