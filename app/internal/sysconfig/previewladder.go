// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"log/slog"
	"sort"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// The preview LADDER — the set of variant keys an operator has
// configured — read once per request and cached, for the
// `ladder_available` API flag (#591).
//
// WHY THIS IS NOT A CONSTANT. The obvious implementation of
// `ladder_available` is `EXISTS(col) AND EXISTS(preview) AND
// EXISTS(screen) AND EXISTS(hires)`, and it is wrong. The ladder is
// operator-configurable: preview.go iterates `cfg.Variants` from
// GetPreviews, and DefaultPreviewConfig() is a DEFAULT, not the
// contract. An operator who drops `hires` to save storage would make a
// hardcoded four-rung check false for every asset forever — silently
// disabling responsive images fleet-wide with nothing in the logs and
// no failing test. Conversely an operator who ADDS a rung would have
// the flag claim a complete ladder that the client then can't rely on.
// So the check is computed against whatever is configured, and the
// configured list travels into the query as a parameter.
//
// WHY A CACHE RATHER THAN A CONFIG READ PER QUERY. The flag is computed
// on browse's hot list paths — the posts feed, the assets list, the
// collection contents — so a `system_config` SELECT per request is not
// acceptable. This is the same problem PublicModeReader solves, and it
// uses the same solution: a one-entry LRU on the shared NOTIFY registry.
// The read is an in-process map lookup after the first call.
//
// The invalidation half of that pattern is NOT wired, because there is
// nothing to wire it to yet: no endpoint writes the preview config. See
// InvalidatePreviewLadder below for where the call belongs when one
// lands. Until then the ladder changes only by direct database edit,
// after which a node serves its cached copy until the entry ages out.
//
// FAILS CLOSED, and the closed direction is "no ladder". A read error
// yields an empty list, which makes `ladder_available` false, which
// makes the client fall back to the single `col` rung it already knows
// exists. The failure mode is "responsive images stop until config is
// readable again" — not "the client requests rungs that may not be
// there", which is the 404 class #471 removed.

const (
	cacheDomainPreviewLadder = "sysconfig.preview_ladder"
	previewLadderCacheKey    = "keys"
)

// PreviewLadderReader returns the configured preview variant keys,
// sorted, for use as a query parameter. An empty result means "unknown"
// and callers MUST treat it as no-ladder rather than as a satisfied
// (vacuous) check — see LadderSatisfiedSQL.
type PreviewLadderReader func(ctx context.Context) []string

// NewPreviewLadderReader builds the cached reader. A nil registry yields
// an uncached reader rather than an error, so test fixtures can pass nil
// — correctness never depends on the cache, only latency does.
func NewPreviewLadderReader(s *Store, registry *cache.Registry, logger *slog.Logger) PreviewLadderReader {
	var c *cache.Cache[[]string]
	if registry != nil {
		c = cache.Register[[]string](registry, cacheDomainPreviewLadder, 1)
	}
	return func(ctx context.Context) []string {
		if c != nil {
			if v, ok := c.Get(previewLadderCacheKey); ok {
				return v
			}
		}
		cfg, err := s.GetPreviews(ctx)
		if err != nil {
			if logger != nil {
				logger.LogAttrs(ctx, slog.LevelWarn, "sysconfig.preview_ladder.read_failed",
					slog.String("err", err.Error()))
			}
			// Deliberately NOT cached: caching a failure would pin the
			// install's responsive images off until the next config write.
			return nil
		}
		keys := make([]string, 0, len(cfg.Variants))
		for _, v := range cfg.Variants {
			if v.Key != "" {
				keys = append(keys, v.Key)
			}
		}
		// Sorted so the value is stable across reads — it is a cache
		// entry and a query parameter, and an unstable ordering would
		// make both harder to reason about in a log or a test.
		sort.Strings(keys)
		if c != nil {
			c.Add(previewLadderCacheKey, keys)
		}
		return keys
	}
}

// InvalidatePreviewLadder broadcasts a cache invalidation for the
// configured ladder.
//
// IT HAS NO CALLER TODAY, and that is not an oversight to be fixed by
// wiring it somewhere plausible. There is no preview-config WRITE
// endpoint yet — Store.SetPreviews exists but nothing outside tests
// calls it — so there is no commit point for an invalidation to follow.
// A call inserted now would either sit on a path nobody takes or, worse,
// look like working invalidation while doing nothing.
//
// WHEN THAT ENDPOINT LANDS, call this from the HTTP handler that writes
// the config, immediately after Store.SetPreviews returns and BEFORE the
// response, exactly as UpdatePublicMode does:
//
//	if err := h.Store.SetPreviews(ctx, cfg); err != nil { ... }
//	InvalidatePreviewLadder(ctx, h.CacheReg)
//
// Handler-level, not store-level: sysconfig.Store is {Pool, enc} and
// deliberately holds no registry, while Handler already carries
// CacheReg. Adding a registry to the Store to move this earlier would
// widen the store's dependencies for one caller.
//
// Skipping the call does not corrupt anything — the cache is a one-entry
// LRU and the stale ladder ages out — but until it aged out an operator
// who added or removed a rung would watch their change appear to do
// nothing, which is the same bug UpdatePublicMode's comment describes.
func InvalidatePreviewLadder(ctx context.Context, registry *cache.Registry) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, cacheDomainPreviewLadder, previewLadderCacheKey)
}

// LadderSatisfiedSQL is the SQL fragment computing "every configured
// rung exists for this asset", given a `text[]` placeholder holding the
// ladder and an expression for the asset's file hash.
//
// The `cardinality(...) > 0` guard is load-bearing, not defensive
// noise. Without it an EMPTY ladder makes the comparison `0 = 0` — true
// — so a failed config read would report a COMPLETE ladder for every
// asset in the install and the client would confidently request rungs
// that do not exist. The guard turns the unknown case into false, which
// is the direction that costs a feature rather than a wall of 404s.
//
// Kept as one exported string rather than copied into each of the five
// query sites so the guard cannot be dropped in one of them: that class
// of "fixed in three of four places" divergence is what ADR 0063 exists
// to prevent.
// The outer COALESCE is equally load-bearing. A Go nil slice binds as
// SQL NULL, not as an empty array, so `cardinality(NULL)` is NULL and
// three-valued logic propagates NULL through the whole expression —
// which then fails to scan into a bool at all. Both "unknown" shapes,
// the empty array and the NULL, must land on the same answer: false.
func LadderSatisfiedSQL(hashExpr, ladderParam string) string {
	return `COALESCE(
            cardinality(` + ladderParam + `::text[]) > 0 AND ` + hashExpr + ` IS NOT NULL
            AND (SELECT COUNT(DISTINCT sv.variant_key) FROM storage_variants sv
                  WHERE sv.object_hash = ` + hashExpr + `
                    AND sv.variant_key = ANY(` + ladderParam + `::text[]))
                = cardinality(` + ladderParam + `::text[])
          , false)`
}
