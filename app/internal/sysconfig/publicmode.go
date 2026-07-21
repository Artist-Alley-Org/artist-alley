// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"log/slog"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// KeyPublicMode — system_config key for the install-wide "serve the
// public surface to anonymous callers" switch (#445, ADR 0063/0064
// follow-on).
//
// ABSENT MEANS OFF, and that is the whole reason this is a
// system_config key rather than a migration-seeded column. Existing
// installs upgraded into a build carrying #415/#437/#438 have never
// consented to being public; a migration that seeded a row would have
// to pick a value for them, and the only safe value to pick is the one
// you get for free by storing nothing at all. getKey leaves the struct
// zero-valued on pgx.ErrNoRows, so a fresh DB and an upgraded DB both
// read Enabled=false without either of them having a row.
//
// Do not "fix" this by seeding a default row in a migration. The
// absence of the key IS the default, and it is the reason no install
// silently becomes public across an upgrade.
const KeyPublicMode = "public_mode"

// PublicModeConfig is the payload stored under KeyPublicMode.
//
// One boolean, deliberately. The temptation is to grow this into
// per-surface switches (public search but not public downloads,
// public collections but not public assets) — resist it here. Which
// routes the switch covers is a code decision recorded in
// auth.PublicSurfaceRoutes, reviewable in a diff; a per-surface config
// blob would move that decision into database state where nobody can
// review it and every combination becomes a supported configuration.
type PublicModeConfig struct {
	// Enabled: anonymous callers may reach the public surface (the
	// routes in auth.PublicSurfaceRoutes). Row-level visibility and
	// content-level sensitivity still apply on top — this switch
	// decides whether an anonymous request is admitted to the
	// handler at all, not what the handler then hands back.
	Enabled bool `json:"enabled"`
}

// GetPublicMode returns the public-mode config, or the zero value
// (disabled) when the key is absent.
func (s *Store) GetPublicMode(ctx context.Context) (PublicModeConfig, error) {
	var out PublicModeConfig
	if err := s.getKey(ctx, KeyPublicMode, &out); err != nil {
		return PublicModeConfig{}, err
	}
	return out, nil
}

// SetPublicMode writes the public-mode config.
func (s *Store) SetPublicMode(ctx context.Context, v PublicModeConfig) error {
	return s.setKey(ctx, KeyPublicMode, v)
}

// cacheDomainPublicMode is the NOTIFY domain for the public-mode flag.
// Single-entry cache, so the key is a constant.
const (
	cacheDomainPublicMode = "sysconfig.public_mode"
	publicModeCacheKey    = "enabled"
)

// PublicModeReader is a cached read of the public-mode flag, shaped for
// use as auth.Resolver.PublicMode.
//
// This is on the hot path — the auth middleware consults it on EVERY
// request, authenticated or not — so it cannot be a database round
// trip. The cache is a one-entry LRU fed by the same NOTIFY registry
// every other cache in the process uses, invalidated by the admin write
// path below, which means a toggle from the admin UI takes effect on
// the next request across every node without a restart.
//
// FAILS CLOSED. A read error returns false (public mode off), because
// the alternative is that a transient database blip publishes a private
// install. "The toggle appears stuck off" is a support ticket; "the
// install went public because the database hiccuped" is an incident.
type PublicModeReader func(ctx context.Context) bool

// NewPublicModeReader builds the cached reader. A nil registry yields
// an uncached reader (every call hits the database) rather than an
// error — test fixtures pass nil and correctness does not depend on
// the cache.
func NewPublicModeReader(s *Store, registry *cache.Registry, logger *slog.Logger) PublicModeReader {
	var c *cache.Cache[bool]
	if registry != nil {
		c = cache.Register[bool](registry, cacheDomainPublicMode, 1)
	}
	return func(ctx context.Context) bool {
		if c != nil {
			if v, ok := c.Get(publicModeCacheKey); ok {
				return v
			}
		}
		cfg, err := s.GetPublicMode(ctx)
		if err != nil {
			if logger != nil {
				logger.LogAttrs(ctx, slog.LevelWarn, "sysconfig.public_mode.read_failed",
					slog.String("err", err.Error()))
			}
			// Deliberately NOT cached: caching a failure would pin the
			// install closed until the next write.
			return false
		}
		if c != nil {
			c.Add(publicModeCacheKey, cfg.Enabled)
		}
		return cfg.Enabled
	}
}

// InvalidatePublicMode broadcasts a cache invalidation for the
// public-mode flag. Called after the admin write commits.
func InvalidatePublicMode(ctx context.Context, registry *cache.Registry) {
	if registry == nil {
		return
	}
	_ = registry.Emit(ctx, cacheDomainPublicMode, publicModeCacheKey)
}
