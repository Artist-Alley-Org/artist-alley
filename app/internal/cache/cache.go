// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package cache is artist-alley's two-tier caching plumbing
// per ADR 0013: in-process LRU + Postgres LISTEN/NOTIFY for
// cross-instance invalidation, no Redis pre-MVP.
//
// Each data domain (field_definition, role, asset_by_id, …) gets
// its own typed Cache[V] registered with a single process-wide
// Registry. Writes that modify cached data call Cache.Invalidate
// to drop the local entry AND broadcast to peer instances via
// pg_notify on channel "cache_invalidate". Other instances receive
// the NOTIFY, look up the matching Cache by domain name, and drop
// their copy too. No code changes are needed in the consuming
// package when a second instance comes online.
//
// Cache keys are strings. Consumers convert UUIDs / int64 / etc.
// to a stable string representation before lookup. The simplification
// avoids generic-key gymnastics with NOTIFY payload encoding.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChannelName is the single Postgres NOTIFY channel every cache
// domain shares. Splitting per-domain channels was considered and
// rejected — the per-event filtering cost is dwarfed by the
// connection overhead of N LISTEN goroutines.
const ChannelName = "cache_invalidate"

// WildcardDomain is the reserved domain that flushes EVERY registered
// cache on a receiving instance, not just one domain. It exists so a
// separate process — the seeder, which owns no Registry and cannot
// enumerate the ~50 domains a serving instance registers — can tell
// running instances to drop all their in-memory caches at once after
// `aa seed --reset` rewrites the database (#845). Routing is on the
// Domain field (dispatch already switches on it); the Op field is left
// unset. A pre-wildcard binary treats "*" as an unknown domain and
// safely no-ops, which is acceptable pre-release.
const WildcardDomain = "*"

// Payload is the NOTIFY message format. Producers must marshal
// this exact shape; the metadata handler does so already.
type Payload struct {
	Domain string `json:"domain"`
	Key    string `json:"key,omitempty"`
	Op     string `json:"op,omitempty"`
}

// Cache is an in-process LRU keyed on string, typed on V.
type Cache[V any] struct {
	name     string
	lru      *lru.Cache[string, V]
	registry *Registry
}

// Get returns (value, true) on hit, (zero, false) on miss.
func (c *Cache[V]) Get(key string) (V, bool) { return c.lru.Get(key) }

// Add stores key->v without broadcasting. Useful for "I just read
// this from the DB, populate the cache" paths.
func (c *Cache[V]) Add(key string, v V) { c.lru.Add(key, v) }

// Invalidate drops the local entry AND broadcasts to peers. Call
// this AFTER the write transaction commits — a NOTIFY before commit
// would race readers into seeing the pre-commit state.
//
// Returns the NOTIFY error if the broadcast fails; the local
// invalidation is always best-effort and completes regardless.
func (c *Cache[V]) Invalidate(ctx context.Context, key string) error {
	c.lru.Remove(key)
	return c.registry.Emit(ctx, c.name, key)
}

// InvalidateAll drops every entry locally AND broadcasts a domain-
// wide purge. Used for bulk operations (import, schema reload).
func (c *Cache[V]) InvalidateAll(ctx context.Context) error {
	c.lru.Purge()
	return c.registry.Emit(ctx, c.name, "")
}

// Len returns the current in-memory entry count. Useful in tests.
func (c *Cache[V]) Len() int { return c.lru.Len() }

// invalidator is the type-erased view of a Cache that the Registry
// uses to dispatch incoming NOTIFYs.
type invalidator interface {
	invalidate(key string)
	purge()
}

func (c *Cache[V]) invalidate(key string) { c.lru.Remove(key) }
func (c *Cache[V]) purge()                { c.lru.Purge() }

// Registry owns the single LISTEN connection and dispatches
// invalidations to per-domain Cache instances.
type Registry struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	mu      sync.RWMutex
	domains map[string]invalidator

	startedOnce sync.Once
	stopOnce    sync.Once
	stop        chan struct{}
}

// NewRegistry constructs a Registry. Call Register[V] to attach a
// Cache, then Start(ctx) to begin listening for invalidations.
func NewRegistry(pool *pgxpool.Pool, logger *slog.Logger) *Registry {
	return &Registry{
		Pool:    pool,
		Logger:  logger,
		domains: make(map[string]invalidator),
		stop:    make(chan struct{}),
	}
}

// Register creates a Cache[V] bound to the given domain name and
// attaches it to the registry. size is the LRU max-entries.
//
// Domain names are conventionally dotted-namespace (e.g.
// "field_definition.id", "role.name") so a single broadcast
// invalidation can target a specific key-shape within a data
// domain.
func Register[V any](r *Registry, name string, size int) *Cache[V] {
	l, err := lru.New[string, V](size)
	if err != nil {
		// hashicorp/golang-lru only errors on non-positive size,
		// which our callers shouldn't pass. Panic loud to surface
		// the misconfiguration at boot.
		panic(fmt.Sprintf("cache.Register(%q): %v", name, err))
	}
	c := &Cache[V]{name: name, lru: l, registry: r}
	r.mu.Lock()
	r.domains[name] = c
	r.mu.Unlock()
	return c
}

// Emit publishes a cross-instance NOTIFY. Called by Cache.Invalidate
// and Cache.InvalidateAll; producers outside this package (e.g.,
// the metadata handler emitting from trigger-side writes) can call
// it directly too.
func (r *Registry) Emit(ctx context.Context, domain, key string) error {
	return publish(ctx, r.Pool, Payload{Domain: domain, Key: key, Op: "upsert"})
}

// InvalidateNow is Cache.Invalidate for a caller that does not hold the
// typed Cache: it drops the key from THIS process's cache immediately
// and then broadcasts to peers.
//
// Emit alone is not equivalent. A bare Emit reaches the local process
// only by round-tripping through Postgres and back down the LISTEN
// connection, so for a window after the write returns, this instance
// still serves the stale entry from its own LRU. That window is small
// but it is on the wrong side of "the write returned 204" — a client
// that writes and immediately reads can observe the pre-write value,
// which is exactly the class of bug #920 was.
//
// Cross-package invalidation helpers should prefer this over Emit
// whenever the write and the subsequent read can be the same request
// chain. Unknown domains are a no-op locally and still broadcast, since
// the domain may be registered on a peer but not here.
func (r *Registry) InvalidateNow(ctx context.Context, domain, key string) error {
	r.mu.RLock()
	inv := r.domains[domain]
	r.mu.RUnlock()
	if inv != nil {
		inv.invalidate(key)
	}
	return r.Emit(ctx, domain, key)
}

// EmitFlushAll publishes a wildcard cache-flush NOTIFY that purges every
// registered cache on every receiving instance. It takes a raw pool
// rather than a Registry so a process that owns no cache — the seeder —
// can broadcast it after `aa seed --reset` commits (#845). Marshals the
// same Payload shape Registry.Emit does; see WildcardDomain.
func EmitFlushAll(ctx context.Context, pool *pgxpool.Pool) error {
	return publish(ctx, pool, Payload{Domain: WildcardDomain})
}

// publish marshals p and fires it on the shared NOTIFY channel. Shared
// by Emit and EmitFlushAll so the wire format has a single source.
func publish(ctx context.Context, pool *pgxpool.Pool, p Payload) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("cache: marshal payload: %w", err)
	}
	if _, err := pool.Exec(ctx, "SELECT pg_notify($1, $2)", ChannelName, string(payload)); err != nil {
		return fmt.Errorf("cache: pg_notify: %w", err)
	}
	return nil
}

// Start spins up the LISTEN goroutine. Idempotent — calling twice
// is a no-op. Returns immediately; the goroutine runs until ctx
// is cancelled or Stop() is called.
func (r *Registry) Start(ctx context.Context) error {
	var startErr error
	r.startedOnce.Do(func() {
		conn, err := r.Pool.Acquire(ctx)
		if err != nil {
			startErr = fmt.Errorf("cache: acquire listen conn: %w", err)
			return
		}
		// Hijack removes the connection from the pool — we own its
		// lifecycle now. It would otherwise be reaped on idle.
		hijacked := conn.Hijack()
		if _, err := hijacked.Exec(ctx, "LISTEN "+ChannelName); err != nil {
			_ = hijacked.Close(ctx)
			startErr = fmt.Errorf("cache: LISTEN: %w", err)
			return
		}
		go r.listen(ctx, hijacked)
	})
	return startErr
}

// Stop signals the listen goroutine to exit. The underlying
// connection closes when WaitForNotification unwinds.
func (r *Registry) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
}

func (r *Registry) listen(ctx context.Context, conn *pgx.Conn) {
	// Best-effort close on exit; we never reconnect the same Conn
	// here, so leaking it isn't a concern.
	defer func() { _ = conn.Close(context.Background()) }()

	for {
		select {
		case <-r.stop:
			return
		case <-ctx.Done():
			return
		default:
		}

		// Use a short-lived per-wait context so Stop() can break
		// out of a long-running WaitForNotification.
		waitCtx, cancel := context.WithCancel(ctx)
		stopped := make(chan struct{})
		go func() {
			select {
			case <-r.stop:
				cancel()
			case <-stopped:
			}
		}()

		n, err := conn.WaitForNotification(waitCtx)
		close(stopped)
		cancel()
		if err != nil {
			if ctx.Err() != nil || isStopped(r.stop) {
				return
			}
			r.Logger.LogAttrs(ctx, slog.LevelWarn, "cache.listen.error",
				slog.String("err", err.Error()))
			// Brief back-off before retrying. The connection is
			// likely toast; a future improvement is to acquire a
			// fresh one and re-LISTEN, but for MVP we exit and rely
			// on a server restart. TODO: reconnect logic.
			time.Sleep(time.Second)
			return
		}

		var p Payload
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			r.Logger.LogAttrs(ctx, slog.LevelWarn, "cache.payload.unmarshal",
				slog.String("payload", n.Payload),
				slog.String("err", err.Error()))
			continue
		}
		r.dispatch(p)
	}
}

func (r *Registry) dispatch(p Payload) {
	if p.Domain == "" {
		return
	}
	if p.Domain == WildcardDomain {
		// Wildcard flush (#845): purge every registered invalidator so a
		// running instance drops all pre-reset caches without a restart.
		r.mu.RLock()
		for _, inv := range r.domains {
			inv.purge()
		}
		r.mu.RUnlock()
		return
	}
	r.mu.RLock()
	inv, ok := r.domains[p.Domain]
	r.mu.RUnlock()
	if !ok {
		// Not subscribed to this domain — that's fine, peer
		// instances may use domains we don't.
		return
	}
	if p.Key == "" {
		inv.purge()
	} else {
		inv.invalidate(p.Key)
	}
}

func isStopped(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
