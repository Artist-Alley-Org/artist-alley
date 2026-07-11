// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package presentation

import (
	"context"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Domain is the cache.Registry domain name for IIIF Presentation
// manifests. One domain covers both asset + collection manifests;
// the key encodes which is which. A single domain keeps
// cross-instance NOTIFY dispatch simple + matches the memory-cost
// budget better than two undersized LRUs.
const Domain = "iiif.presentation"

// DefaultCacheSize is the LRU max-entries. Sized to fit the
// working set of an active gallery browsing session (~2000 assets
// + ~200 collections) with room for anonymous + authenticated
// variants of each.
const DefaultCacheSize = 4096

// Cache wraps a cache.Cache[any] with typed helpers for the
// presentation package. Nil-safe: all methods no-op when the
// receiver is nil so callers don't need to guard.
type Cache struct {
	inner    *cache.Cache[any]
	registry *cache.Registry
}

// NewCache registers the manifest cache with the process-wide
// Registry. Call once at boot; share the returned *Cache across
// handlers.
func NewCache(reg *cache.Registry) *Cache {
	if reg == nil {
		return nil
	}
	return &Cache{
		inner:    cache.Register[any](reg, Domain, DefaultCacheSize),
		registry: reg,
	}
}

// Key composes the stable cache key. Includes the anonymous flag so
// the sensitivity gate can't leak: an anon variant of asset X is a
// different cache key from the authenticated variant.
func (c *Cache) Key(kind EntityType, id uuid.UUID, isAnonymous bool) string {
	tail := "::auth"
	if isAnonymous {
		tail = "::anon"
	}
	return string(kind) + "::" + id.String() + tail
}

// Get returns a cached manifest, or (nil, false) on miss / nil
// receiver.
func (c *Cache) Get(key string) (any, bool) {
	if c == nil || c.inner == nil {
		return nil, false
	}
	return c.inner.Get(key)
}

// Put stores a manifest without broadcasting. Broadcasting isn't
// needed on Put — every instance builds its own cached copy from
// its own DB read; the manifest is deterministic wrt the input row.
func (c *Cache) Put(key string, v any) {
	if c == nil || c.inner == nil {
		return
	}
	c.inner.Add(key, v)
}

// InvalidateAsset drops both the anonymous + authenticated variants
// for the given asset AND broadcasts to peers. Call after a write
// to the assets row, metadata field values, or any join affecting
// the manifest.
func (c *Cache) InvalidateAsset(ctx context.Context, id uuid.UUID) error {
	if c == nil || c.inner == nil {
		return nil
	}
	if err := c.inner.Invalidate(ctx, c.Key(EntityAsset, id, true)); err != nil {
		return err
	}
	return c.inner.Invalidate(ctx, c.Key(EntityAsset, id, false))
}

// InvalidateCollection drops both variants for a collection.
func (c *Cache) InvalidateCollection(ctx context.Context, id uuid.UUID) error {
	if c == nil || c.inner == nil {
		return nil
	}
	if err := c.inner.Invalidate(ctx, c.Key(EntityCollection, id, true)); err != nil {
		return err
	}
	return c.inner.Invalidate(ctx, c.Key(EntityCollection, id, false))
}

// InvalidateAll purges the whole manifest cache + broadcasts a
// domain-wide invalidation. Used by admin reindex / bulk metadata
// operations.
func (c *Cache) InvalidateAll(ctx context.Context) error {
	if c == nil || c.inner == nil {
		return nil
	}
	return c.inner.InvalidateAll(ctx)
}

// Len returns the current LRU entry count. Surfaced to the
// admin/iiif/health dashboard.
func (c *Cache) Len() int {
	if c == nil || c.inner == nil {
		return 0
	}
	return c.inner.Len()
}

// InvalidateAssetOn is the cross-package helper other domains
// invoke after their own writes commit (per the four-core-principles
// caching pattern). Accepts a nil *Cache without panicking.
//
// Wire this into assets.Handler as its manifest-cache dependency:
// after PATCH / DELETE / metadata update, call
// presentation.InvalidateAssetOn(ctx, cache, id).
func InvalidateAssetOn(ctx context.Context, c *Cache, id uuid.UUID) error {
	return c.InvalidateAsset(ctx, id)
}

// InvalidateCollectionOn is the collection counterpart.
func InvalidateCollectionOn(ctx context.Context, c *Cache, id uuid.UUID) error {
	return c.InvalidateCollection(ctx, id)
}
