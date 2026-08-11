// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// KeyBrowseViews — system_config key for the install-wide set of
// browse layouts the operator offers (#709).
//
// ABSENT MEANS ALL FIVE, which is why this is a system_config key and
// not a seeded row. Every install that predates this feature has
// consented to nothing; the only safe reading of "no row" is the
// behaviour those installs already have, and that is the value you get
// for free by storing nothing at all. getKey leaves the struct
// zero-valued on pgx.ErrNoRows, so [BrowseViewsConfig.Resolved] treats
// a nil slice as the full set rather than as the empty one.
//
// Do not "fix" this by seeding a default row in a migration: a seeded
// row freezes today's five modes into every install's database, so
// adding a sixth mode later would ship it disabled everywhere with no
// way to tell an operator's deliberate exclusion from a stale row.
const KeyBrowseViews = "browse_views"

// BrowseViewMode is one browse layout, matching the `ViewMode` union in
// web/src/lib/stores/browseView.svelte.ts. The two lists are a contract:
// a mode named here that the frontend does not know renders nothing, and
// a mode the frontend offers that is missing here can never be disabled.
type BrowseViewMode string

const (
	BrowseViewGrid      BrowseViewMode = "grid"
	BrowseViewMasonry   BrowseViewMode = "masonry"
	BrowseViewThumbnail BrowseViewMode = "thumbnail"
	BrowseViewList      BrowseViewMode = "list"
	BrowseViewFeed      BrowseViewMode = "feed"
)

// AllBrowseViewModes is the shipped set, in the order the switcher
// renders them. This is the canonical order: [BrowseViewsConfig.Resolved]
// sorts through it rather than preserving whatever order a caller sent,
// so the switcher cannot be reordered by writing the config — layout
// order is a frontend decision, availability is the operator's.
var AllBrowseViewModes = []BrowseViewMode{
	BrowseViewGrid,
	BrowseViewMasonry,
	BrowseViewThumbnail,
	BrowseViewList,
	BrowseViewFeed,
}

// ValidBrowseViewMode reports whether m is a shipped mode.
func ValidBrowseViewMode(m BrowseViewMode) bool {
	for _, k := range AllBrowseViewModes {
		if k == m {
			return true
		}
	}
	return false
}

// BrowseViewsConfig is the payload stored under KeyBrowseViews.
//
// An allowlist of enabled modes rather than a denylist of disabled
// ones, because the question the frontend asks is "what may I offer?"
// — and an allowlist answers it without the frontend needing its own
// copy of the shipped set to subtract from. It also means a mode added
// in a later release is OFF for an operator who has curated their
// install, which is the conservative direction: a new layout appearing
// unannounced in a curated install is a surprise, a new layout the
// operator has to opt into is a release note.
type BrowseViewsConfig struct {
	// Enabled is the operator's chosen set. Nil (absent key) means
	// "everything shipped" — see [BrowseViewsConfig.Resolved]. Never
	// empty: [Store.SetBrowseViews] refuses the empty set, so a nil
	// slice is unambiguously "unconfigured" rather than "all off".
	Enabled []BrowseViewMode `json:"enabled"`
}

// Resolved returns the effective enabled set: the stored allowlist
// filtered to shipped modes and put back in canonical order, or all
// five when nothing is configured.
//
// FAILS OPEN, deliberately, in both degenerate cases — an absent key
// and a stored set that survives no filter (every named mode retired by
// a later release). The failure being guarded is "the operator cannot
// browse their own install", and there is no security property on the
// other side of the trade: which layout buttons exist is chrome, not
// access. An install that renders all five is working; an install that
// renders none is bricked.
func (c BrowseViewsConfig) Resolved() []BrowseViewMode {
	if len(c.Enabled) == 0 {
		return append([]BrowseViewMode(nil), AllBrowseViewModes...)
	}
	want := make(map[BrowseViewMode]struct{}, len(c.Enabled))
	for _, m := range c.Enabled {
		want[m] = struct{}{}
	}
	out := make([]BrowseViewMode, 0, len(AllBrowseViewModes))
	for _, m := range AllBrowseViewModes {
		if _, ok := want[m]; ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return append([]BrowseViewMode(nil), AllBrowseViewModes...)
	}
	return out
}

// Enables reports whether m is offered under this config.
func (c BrowseViewsConfig) Enables(m BrowseViewMode) bool {
	for _, k := range c.Resolved() {
		if k == m {
			return true
		}
	}
	return false
}

// GetBrowseViews returns the configured browse-view set, or the zero
// value (which resolves to all five) when the key is absent.
func (s *Store) GetBrowseViews(ctx context.Context) (BrowseViewsConfig, error) {
	var out BrowseViewsConfig
	if err := s.getKey(ctx, KeyBrowseViews, &out); err != nil {
		return BrowseViewsConfig{}, err
	}
	return out, nil
}

// SetBrowseViews writes the browse-view set after validating it.
//
// THE EMPTY SET IS REFUSED HERE, at the store, not only in the admin
// UI — same shape as the last-admin invariant, which is enforced on
// every path that can reach it rather than on the one the UI happens to
// use. An operator who disables all five makes browse unreachable for
// every user on the install, and the recovery path is a database shell.
// A validator that lives in the frontend is not a validator.
//
// Unknown modes are refused rather than silently dropped: a typo that
// filtered down to a smaller set than the operator asked for would
// disable a layout without saying so, and a payload of nothing but
// typos would otherwise pass this check and then fail open to all five
// — accepted, inert, and disagreeing with what the operator saved.
func (s *Store) SetBrowseViews(ctx context.Context, v BrowseViewsConfig) error {
	if len(v.Enabled) == 0 {
		return fmt.Errorf("sysconfig: browse_views must enable at least one mode")
	}
	seen := make(map[BrowseViewMode]struct{}, len(v.Enabled))
	canonical := make([]BrowseViewMode, 0, len(v.Enabled))
	for _, m := range v.Enabled {
		if !ValidBrowseViewMode(m) {
			return fmt.Errorf("sysconfig: browse_views: unknown mode %q", m)
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		canonical = append(canonical, m)
	}
	// Persist in canonical order so the stored document is stable
	// regardless of the order the admin UI serialised its checkboxes
	// in — otherwise the audit changeset shows a diff on every save
	// that reordered nothing.
	v.Enabled = BrowseViewsConfig{Enabled: canonical}.Resolved()
	return s.setKey(ctx, KeyBrowseViews, v)
}

// cacheDomainBrowseViews is the NOTIFY domain for the browse-view set.
// Single-entry cache, so the key is a constant.
const (
	cacheDomainBrowseViews = "sysconfig.browse_views"
	browseViewsCacheKey    = "enabled"
)

// BrowseViewsReader is a cached read of the enabled browse-view set.
//
// Cached for the same reason the public-mode flag is: the public read
// endpoint sits on the frontend's boot path, so every cold page load on
// the install pays for it, and it changes about as often as an operator
// changes their mind.
//
// FAILS OPEN on a read error — all five modes — which is the opposite
// of PublicModeReader's fail-closed and deliberately so. There is no
// exposure behind this switch: it decides which layout buttons render,
// all of which serve the same rows through the same visibility
// predicate. Failing closed here would mean a transient database blip
// leaves users with a browse page that offers no way to look at
// anything.
type BrowseViewsReader func(ctx context.Context) BrowseViewsConfig

// NewBrowseViewsReader builds the cached reader. A nil registry yields
// an uncached reader (every call hits the database) rather than an
// error — test fixtures pass nil and correctness does not depend on
// the cache.
func NewBrowseViewsReader(s *Store, registry *cache.Registry, logger *slog.Logger) BrowseViewsReader {
	var c *cache.Cache[BrowseViewsConfig]
	if registry != nil {
		c = cache.Register[BrowseViewsConfig](registry, cacheDomainBrowseViews, 1)
	}
	return func(ctx context.Context) BrowseViewsConfig {
		if c != nil {
			if v, ok := c.Get(browseViewsCacheKey); ok {
				return v
			}
		}
		cfg, err := s.GetBrowseViews(ctx)
		if err != nil {
			if logger != nil {
				logger.LogAttrs(ctx, slog.LevelWarn, "sysconfig.browse_views.read_failed",
					slog.String("err", err.Error()))
			}
			// Deliberately NOT cached: caching a failure would pin the
			// install on the fallback until the next write.
			return BrowseViewsConfig{}
		}
		if c != nil {
			c.Add(browseViewsCacheKey, cfg)
		}
		return cfg
	}
}

// InvalidateBrowseViews drops the cached set locally and broadcasts to
// peers. Called after the admin write commits.
//
// InvalidateNow rather than Emit because the write and the next read
// are the same request chain in practice: the admin UI saves and the
// browser immediately re-fetches the public endpoint to redraw the
// switcher, which a NOTIFY round trip through Postgres is not fast
// enough to beat.
func InvalidateBrowseViews(ctx context.Context, registry *cache.Registry) {
	if registry == nil {
		return
	}
	_ = registry.InvalidateNow(ctx, cacheDomainBrowseViews, browseViewsCacheKey)
}
