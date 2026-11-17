// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Expiry sweeper job — Phase 1.22.C-d, locked in per reviewer's
// answer §12.5 #4 (NOT optional). Periodic background goroutine
// that finds active shares whose expires_at has passed, revokes them
// and records an aa:Unshare, meant to make recipients purge their cached
// bytes. Delivering that aa:Unshare and applying it on the peer is not
// wired yet (#1571): the outbox routes only to live shares, so it has no
// recipient once the share is revoked, and no receiver applies it.
//
// # Why the sweeper is load-bearing
//
// The inbox-filter (1.22.C-b) already gates per-request on
// expires_at, so a new inbound activity against an expired share
// gets rejected at the wire. BUT: the recipient's local cache
// of the asset bytes is still there. Without proactive
// aa:Unshare emission, the recipient could indefinitely hold
// bytes they no longer have access to.
//
// The sweeper is meant to close this gap: per tick, walk expired-active
// rows and revoke each (recording aa:Unshare via the existing
// write-ahead-audit flow) so the receiver's inbox handler can drop the
// cached asset. The local half (revoke + activity + audit) works; the
// remote half waits on #1571. The revoke is conditional on the row still
// being expired (RevokeExpiredShare), so a share whose expiry was
// extended after the listing is left alone.
//
// # Idempotency + restart safety
//
// Per the reviewer's "chunked, never single-tx" rule:
//   - One row at a time within each tick (each revoke is its
//     own tx); the SQL filter `revoked_at IS NULL` makes the
//     loop naturally restart-safe (already-revoked rows drop
//     out of the next ListExpiringShares).
//   - Tick limit caps wall-clock per sweep (500 rows / tick).
//   - Cross-process: multiple replicas running the sweeper
//     converge — each successful revoke is final; SELECTs from
//     other replicas just see fewer rows next time.
//
// # Caching
//
// Each revoke goes through the same Registry.Revoke method the
// admin handler calls, so the per-object cache invalidation
// fires automatically + cross-process via cache.Registry NOTIFY.

package shares

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// SweeperConfig controls the cadence + batch size. Boot supplies
// defaults; tests override per case.
type SweeperConfig struct {
	// Interval between scans. 1 hour matches the design's
	// "best-effort housekeeping" intent — the inbox-filter
	// expiry check is the load-bearing protection; the sweeper
	// is just for purging recipient caches.
	Interval time.Duration

	// BatchSize is the per-tick row cap. The chunked-revoke
	// pattern bounds per-tick load + lets the loop pick up
	// where it left off across restarts.
	BatchSize int32
}

// DefaultSweeperConfig returns the boot defaults.
func DefaultSweeperConfig() SweeperConfig {
	return SweeperConfig{
		Interval:  1 * time.Hour,
		BatchSize: 500,
	}
}

// Sweeper is the periodic goroutine. Start once at boot via Run;
// Run blocks until ctx is cancelled.
type Sweeper struct {
	cfg           SweeperConfig
	registry      *Registry
	activities    *activities.Writer
	auditRec      *audit.Recorder
	logger        *slog.Logger
	lookupPeer    PeerLookup
	instanceURLFn func(ctx context.Context) string
	usernameFn    func(ctx context.Context, userRef int64) string

	mu      sync.Mutex
	running bool
}

// NewSweeper wires the sweeper. The five callbacks mirror the
// AdminHandler's dependencies (the sweeper emits aa:Unshare just
// like the admin revoke path does).
func NewSweeper(
	cfg SweeperConfig,
	registry *Registry,
	writer *activities.Writer,
	auditRec *audit.Recorder,
	lookupPeer PeerLookup,
	instanceURLFn func(ctx context.Context) string,
	usernameFn func(ctx context.Context, userRef int64) string,
	logger *slog.Logger,
) *Sweeper {
	if cfg.Interval <= 0 {
		cfg.Interval = 1 * time.Hour
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > 5000 {
		cfg.BatchSize = 500
	}
	return &Sweeper{
		cfg:           cfg,
		registry:      registry,
		activities:    writer,
		auditRec:      auditRec,
		logger:        logger,
		lookupPeer:    lookupPeer,
		instanceURLFn: instanceURLFn,
		usernameFn:    usernameFn,
	}
}

// Run blocks until ctx is cancelled. Runs SweepOnce on a ticker.
// Safe to call once per process; subsequent calls log + return.
func (s *Sweeper) Run(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		if s.logger != nil {
			s.logger.Warn("shares.sweeper: Run called more than once")
		}
		return
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	if s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "shares.sweeper.start",
			slog.Duration("interval", s.cfg.Interval),
			slog.Int("batch_size", int(s.cfg.BatchSize)),
		)
	}
	// Run once at startup so newly-expired-during-downtime rows
	// don't wait a full interval.
	s.SweepOnce(ctx)

	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce processes one batch of expired shares. Returns
// (revokedCount, errs) — errors don't abort the batch (one bad
// row shouldn't block the rest); the caller can log + continue.
//
// Exported so tests can drive the sweep deterministically.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, []error) {
	expired, err := s.registry.ListExpiring(ctx, s.cfg.BatchSize)
	if err != nil {
		if s.logger != nil {
			s.logger.LogAttrs(ctx, slog.LevelWarn, "shares.sweeper.list.error",
				slog.String("err", err.Error()),
			)
		}
		return 0, []error{err}
	}
	if len(expired) == 0 {
		return 0, nil
	}
	if s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "shares.sweeper.tick",
			slog.Int("expired_count", len(expired)),
		)
	}
	revoked := 0
	var errs []error
	for i := range expired {
		share := &expired[i]
		if err := s.sweepOne(ctx, share); err != nil {
			errs = append(errs, err)
			if s.logger != nil {
				s.logger.LogAttrs(ctx, slog.LevelWarn, "shares.sweeper.row.error",
					slog.String("share_id", share.ID.String()),
					slog.String("err", err.Error()),
				)
			}
			continue
		}
		revoked++
	}
	if s.logger != nil && revoked > 0 {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "shares.sweeper.done",
			slog.Int("revoked", revoked),
			slog.Int("errors", len(errs)),
		)
	}
	return revoked, errs
}

// sweepOne revokes one expired share. The flow (RecordActivity
// aa:Unshare -> flip revoked -> audit "expired", one tx, then cache
// invalidation) lives in expiryRetirer.retire, shared with the grant
// path so a regrant closes an expired row exactly the way this does
// (#1586). A row another sweeper, admin revoke or regrant already
// closed comes back retired=false and is skipped without a duplicate
// activity.
func (s *Sweeper) sweepOne(ctx context.Context, share *Share) error {
	_, err := expiryRetirer{
		registry:      s.registry,
		activities:    s.activities,
		auditRec:      s.auditRec,
		lookupPeer:    s.lookupPeer,
		instanceURLFn: s.instanceURLFn,
		usernameFn:    s.usernameFn,
	}.retire(ctx, share, "auto-revoked by expiry sweeper", map[string]any{"sweeper": true})
	return err
}

// keep federation import live for the catalogue reference in
// docs even when the file's body doesn't directly use it.
var _ = federation.ShareScopeView
