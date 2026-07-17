// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package lockout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PolicyProvider reads the current (threshold, duration) from
// sysconfig. Injected so operators can retune at runtime without a
// restart. Defaults live in Config below; the login handler wires the
// sysconfig read-through at boot.
type PolicyProvider func(ctx context.Context) Config

// Config is the sysconfig-driven policy. Defaults match OWASP guidance
// for public-facing auth (5 attempts, 15 minute lockout); operators
// tune per their threat model via /admin/system/auth-config.
type Config struct {
	Threshold       int32
	DurationMinutes int32
}

// DefaultConfig is the fallback policy used when the sysconfig PolicyProvider
// is nil (unit tests + isolated startup ordering). NOT authoritative in
// production — the login handler always supplies the boot-wired provider.
var DefaultConfig = Config{
	Threshold:       5,
	DurationMinutes: 15,
}

// Result is the outcome of one call to IncrementFailedLogin. The
// caller uses TriggeredLockout to decide whether this is the failed
// attempt that CROSSED the threshold (audit exactly once per lockout
// window; not once per subsequent locked attempt).
type Result struct {
	// FailedCount is the counter AFTER the increment.
	FailedCount int32
	// LockoutUntil is the current deadline. Zero-value time.Time
	// when the account isn't locked.
	LockoutUntil time.Time
	// TriggeredLockout is true when THIS call was the one that
	// wrote a fresh lockout_until (i.e. crossed the threshold).
	// Attackers pounding a locked account after this get
	// TriggeredLockout=false so the caller can emit exactly one
	// audit event per lockout window.
	TriggeredLockout bool
}

// Manager encapsulates all the DB-facing lockout ops. Wraps sqlc-
// generated Queries so tests can inject a fake pool. Nil-safe on
// AuditRecorder / Cache; the login handler wires both at boot.
type Manager struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// Policy reads (threshold, duration) fresh each call. When nil
	// falls back to DefaultConfig.
	Policy PolicyProvider
	// Cache is the LockoutStatusCache; nil disables the fast path.
	Cache *Cache
	// Auditor emits auth.lockout.triggered + auth.lockout.cleared.
	// Nil-safe: no audit emit when unset.
	Auditor AuditEmitter
}

// AuditEmitter is the narrow audit surface Manager consumes. Kept as
// an interface so the auth package (which owns audit.Recorder) can
// bridge without a cycle.
type AuditEmitter interface {
	LockoutTriggered(ctx context.Context, userRef int64, failedCount, threshold, durationMinutes int32, ipSubnetHash string)
	LockoutCleared(ctx context.Context, userRef int64, adminUserRef *int64, priorFailedCount int32, source string)
}

// NewManager constructs a Manager. Auditor + Cache are optional;
// Policy nil falls back to DefaultConfig at each call. Named
// NewManager (not New) to avoid collision with sqlc's generated
// `New(DBTX)` constructor in the same package.
func NewManager(pool *pgxpool.Pool, logger *slog.Logger) *Manager {
	return &Manager{Pool: pool, Logger: logger}
}

// IsLockedOut checks the persistent state. Hits the cache first when
// wired; falls through to DB on miss. Returns (false, nil) for
// unknown user_ref (never-seen users can't be locked).
//
// Auto-clear semantics: lockout_until in the past reads as unlocked.
// The stale timestamp stays in the row until overwritten; no sweeper.
func (m *Manager) IsLockedOut(ctx context.Context, userRef int64) (bool, error) {
	if m.Cache != nil {
		if entry, ok := m.Cache.Get(userRef); ok {
			return entry.LockoutUntil.After(time.Now()), nil
		}
	}
	q := New(m.Pool)
	row, err := q.GetLockoutState(ctx, userRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lockout: get state ref=%d: %w", userRef, err)
	}
	deadline := timestamptzToTime(row.LockoutUntil)
	if m.Cache != nil {
		m.Cache.Put(userRef, CachedState{
			FailedCount:  row.FailedLoginCount,
			LockoutUntil: deadline,
		})
	}
	return deadline.After(time.Now()), nil
}

// IncrementFailedLogin bumps the counter atomically. When the new
// count reaches Policy.Threshold, writes lockout_until = NOW() +
// Policy.DurationMinutes in the same UPDATE (Postgres row-level lock
// serialises concurrent bumps so exactly the threshold-th attempt
// crosses).
//
// Callers must NOT read-then-update; that's racy. This method is the
// single atomic writer.
//
// Emits auth.lockout.triggered exactly once — when this call crosses
// the threshold. Subsequent failed attempts while the user is
// already locked bump the counter but do NOT re-emit (avoids a
// per-attempt audit log for a pounded account).
func (m *Manager) IncrementFailedLogin(ctx context.Context, userRef int64, ipSubnetHash string) error {
	cfg := m.currentPolicy(ctx)
	q := New(m.Pool)
	row, err := q.IncrementFailedLogin(ctx, IncrementFailedLoginParams{
		Threshold:       cfg.Threshold,
		DurationMinutes: cfg.DurationMinutes,
		UserRef:         userRef,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Row doesn't exist. Never-seen user_ref — bail cleanly.
			return nil
		}
		return fmt.Errorf("lockout: increment ref=%d: %w", userRef, err)
	}
	deadline := timestamptzToTime(row.LockoutUntil)
	triggered := row.FailedLoginCount == cfg.Threshold && deadline.After(time.Now())
	if m.Cache != nil {
		m.Cache.Invalidate(userRef)
	}
	if triggered && m.Auditor != nil {
		m.Auditor.LockoutTriggered(ctx, userRef, row.FailedLoginCount, cfg.Threshold, cfg.DurationMinutes, ipSubnetHash)
	}
	return nil
}

// ResetFailedLogin clears the counter + deadline on successful auth.
// Callers MUST run this inside the successful-auth transaction (same
// tx as session-issue) so a failed session-create rolls the reset
// back too. WithTx allows the caller to thread its own txn handle.
func (m *Manager) ResetFailedLogin(ctx context.Context, userRef int64) error {
	q := New(m.Pool)
	if err := q.ResetFailedLogin(ctx, userRef); err != nil {
		return fmt.Errorf("lockout: reset ref=%d: %w", userRef, err)
	}
	if m.Cache != nil {
		m.Cache.Invalidate(userRef)
	}
	return nil
}

// ResetFailedLoginTx is the same as ResetFailedLogin but runs inside a
// caller-supplied transaction. The login handler wires this so a
// session-create rollback also rolls back the counter reset.
func (m *Manager) ResetFailedLoginTx(ctx context.Context, tx pgx.Tx, userRef int64) error {
	q := New(tx)
	if err := q.ResetFailedLogin(ctx, userRef); err != nil {
		return fmt.Errorf("lockout: reset (tx) ref=%d: %w", userRef, err)
	}
	if m.Cache != nil {
		m.Cache.Invalidate(userRef)
	}
	return nil
}

// AdminUnlock clears the counter + deadline on operator request.
// Returns (priorCount, unlocked, err) — unlocked=false means the
// user wasn't locked (idempotent no-op; caller can skip audit emit
// for the no-op case).
//
// Audits auth.lockout.cleared with actor=adminUserRef + subject=
// userRef when unlocked=true.
func (m *Manager) AdminUnlock(ctx context.Context, userRef int64, adminUserRef int64) (int32, bool, error) {
	q := New(m.Pool)
	prior, err := q.AdminUnlock(ctx, userRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already unlocked (or user_ref doesn't exist —
			// caller is responsible for pre-check).
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("lockout: admin unlock ref=%d: %w", userRef, err)
	}
	if m.Cache != nil {
		m.Cache.Invalidate(userRef)
	}
	if m.Auditor != nil {
		admin := adminUserRef
		m.Auditor.LockoutCleared(ctx, userRef, &admin, prior, "admin")
	}
	return prior, true, nil
}

// CountActiveLockouts returns the current number of users under
// active lockout (lockout_until > NOW()). Surfaces to the auth-health
// gauge; polled every 30s per B-5 dashboard cadence.
func (m *Manager) CountActiveLockouts(ctx context.Context) (int64, error) {
	q := New(m.Pool)
	n, err := q.CountActiveLockouts(ctx)
	if err != nil {
		return 0, fmt.Errorf("lockout: count active: %w", err)
	}
	return n, nil
}

// currentPolicy returns the effective policy for this call. Defaults
// when the injected provider is nil.
func (m *Manager) currentPolicy(ctx context.Context) Config {
	if m.Policy == nil {
		return DefaultConfig
	}
	cfg := m.Policy(ctx)
	if cfg.Threshold < 1 {
		cfg.Threshold = DefaultConfig.Threshold
	}
	if cfg.DurationMinutes < 1 {
		cfg.DurationMinutes = DefaultConfig.DurationMinutes
	}
	return cfg
}

// timestamptzToTime pulls a plain time.Time out of a pgtype.Timestamptz
// safely — the pgx-emitted type reads as zero-value time.Time when
// !Valid (i.e. NULL), which is what our caller wants (unlocked).
func timestamptzToTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}
