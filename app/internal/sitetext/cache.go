// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sitetext

import (
	"context"
	"log/slog"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Domain is this package's cache-domain identifier. Must be unique
// across the process-wide [cache.Registry]; dotted-namespace to match
// the existing convention ("metadata.extraction_config", "role.name").
const Domain = "sitetext.overrides"

// CacheKeyAll is the single key the whole override map lives under.
//
// One entry, rebuilt wholesale, is the right shape here for the same
// reason it is for the extraction-config list: the data is small (an
// operator overrides a handful of strings, not thousands), every read
// wants all of it, and a per-key cache would need the reader to know
// which keys exist before it could ask.
const CacheKeyAll = "all"

// Overrides is the resolved override map: language → key → value.
//
// Nested by language first because that is the axis every consumer
// selects on — the client picks its active locale once and then does
// key lookups inside it.
type Overrides map[string]map[string]string

// Cache is the sitetext domain's slice of the process cache.
type Cache struct {
	Map *cache.Cache[Overrides]

	logger *slog.Logger
}

// NewCache registers the override map with the process registry. Size 2
// because the domain holds exactly one entry; the spare slot costs
// nothing and avoids an eviction if a second scope ever appears.
func NewCache(registry *cache.Registry, logger *slog.Logger) *Cache {
	return &Cache{
		Map:    cache.Register[Overrides](registry, Domain, 2),
		logger: logger,
	}
}

// Invalidate drops the cached map locally and broadcasts a pg_notify so
// peer instances drop theirs too.
//
// Called after every override write and revert. This is what makes an
// edit visible on the next read WITHOUT a restart — on this instance
// because the local LRU entry is gone, and on every other instance
// because cache.Registry's LISTEN goroutine receives the broadcast and
// purges the same domain. Site text is a surface an operator edits and
// then immediately reloads a page to check, so "eventually, after a
// deploy" would read as the feature not working.
//
// NOTIFY errors are logged and swallowed: the local drop always
// succeeds, and a missed broadcast degrades to one peer serving a stale
// string until its next write — a soft observability problem, not a
// reason to fail the operator's save.
func (c *Cache) Invalidate(ctx context.Context) {
	if err := c.Map.Invalidate(ctx, CacheKeyAll); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn,
			"sitetext.cache.invalidate_error",
			slog.String("err", err.Error()),
		)
	}
}
