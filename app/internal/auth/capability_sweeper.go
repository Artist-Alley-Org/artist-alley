// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.C — Background sweeper for time-bound capability
// grants + revokes.
//
// Mirrors the userkeys.Sweeper pattern (Phase 1.22.I-h): a single
// goroutine that ticks every SweepTickDefault, runs one DELETE
// pass per table, emits a per-row audit event for every reaped
// row, and broadcasts a cache invalidation for each affected
// user_ref so the resolver picks up the change on the next
// authz check.
//
// # Why per-row audit (not per-sweep)
//
// The userkeys sweeper emits one audit row carrying a count
// because every reaped row is the same kind (an expired retained
// federation key). Capability grants are not interchangeable —
// reaping (alice, posts.publish) and (bob, users.write) are
// operationally distinct events. Per-row audit means the audit
// log alone reconstructs each grant's lifecycle (created → expired
// → swept) without joining against the (now-deleted) source row.
//
// # Last-admin guard at sweep time
//
// Reaping a system.admin grant could leave the system with zero
// active admins (e.g., a one-admin install where the sole admin
// grant has expires_at). The sweeper enforces the same invariant
// as the handler: if reaping would leave zero admins, the row
// stays in place and the sweeper logs a "stuck open" WARN so the
// operator notices and extends/replaces. Commit 3 wires this
// guard.

package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CapabilitySweepTickDefault is the production tick interval.
// Match the userkeys precedent (1h) for symmetry — operators
// looking at the codebase see a single cadence for "background
// expiry reaping" across both surfaces.
const CapabilitySweepTickDefault = 1 * time.Hour

// CapabilitySweepAuditFn fires once per reaped row. Pool-bound;
// nil-safe. Two callbacks because grants and revokes are distinct
// audit event types — the audit layer's Recorder has separate
// methods (CapabilityGrantExpiredSwept / CapabilityRevokeExpiredSwept)
// and the sweeper threads the callback per kind to keep the audit
// shape unambiguous.
type CapabilitySweepAuditFn func(ctx context.Context, userRef int64, capability, teamID string, expiredAt time.Time)

// CapabilityCacheInvalidateFn broadcasts cache eviction for a
// single user. Production wires this to auth.InvalidateUserCaps;
// tests pass a recording stub.
type CapabilityCacheInvalidateFn func(ctx context.Context, userRef int64)

// RequestCascadeFn is the Phase 1.17.E request-cascade hook.
// Called by SweepOnce once per reaped grant whose request_ref
// column is non-NULL, with the request_id + the grant's reaped
// expires_at. Best-effort by contract — a failure here logs at
// WARN but does not roll back the grant reap or fail the sweep
// (the grant is already gone by the time this fires).
//
// Production wires this to requests.Handler.MarkExpired.
type RequestCascadeFn func(ctx context.Context, requestID uuid.UUID, expiredAt time.Time) error

// CapabilitySweeper is the background goroutine that reaps
// expired user_capability_grants + user_capability_revokes rows.
// Construct via NewCapabilitySweeper; call Run inside a goroutine;
// cancel the context to stop.
type CapabilitySweeper struct {
	pool           *pgxpool.Pool
	logger         *slog.Logger
	auditGrant     CapabilitySweepAuditFn
	auditRevoke    CapabilitySweepAuditFn
	invalidateCaps CapabilityCacheInvalidateFn
	cascadeRequest RequestCascadeFn
	tickEvery      time.Duration
}

// NewCapabilitySweeper builds a CapabilitySweeper. Pass the same
// pool the rest of the app uses; logger may be nil; tickEvery <=
// 0 falls back to CapabilitySweepTickDefault.
//
// The two audit callbacks may be nil (sweeper still reaps + logs
// + invalidates caches, just no audit row). invalidateCaps may
// also be nil (the resolver will see the empty result on its
// next miss + repopulate, but cross-instance peers will be stale
// until their own LRU evicts).
//
// Phase 1.17.E adds SetRequestCascade (post-construction setter)
// for the request-expiry cascade. Keeping it off the constructor
// avoids churning the existing 1.17.C call site.
func NewCapabilitySweeper(
	pool *pgxpool.Pool,
	logger *slog.Logger,
	auditGrant CapabilitySweepAuditFn,
	auditRevoke CapabilitySweepAuditFn,
	invalidateCaps CapabilityCacheInvalidateFn,
	tickEvery time.Duration,
) *CapabilitySweeper {
	if tickEvery <= 0 {
		tickEvery = CapabilitySweepTickDefault
	}
	return &CapabilitySweeper{
		pool:           pool,
		logger:         logger,
		auditGrant:     auditGrant,
		auditRevoke:    auditRevoke,
		invalidateCaps: invalidateCaps,
		tickEvery:      tickEvery,
	}
}

// SetRequestCascade wires the Phase 1.17.E request-cascade hook.
// Called from boot in app/internal/http/api.go with
// requests.Handler.MarkExpired. nil-safe — when unwired, the
// sweeper skips the cascade silently (the request row just stays
// granted while the grant disappears — observable to the operator
// but doesn't break anything).
//
// Safe to call once at startup.
func (s *CapabilitySweeper) SetRequestCascade(fn RequestCascadeFn) {
	s.cascadeRequest = fn
}

// Run blocks until ctx is cancelled. Sweeps once at boot, then
// every tickEvery interval. A sweep failure logs at WARN +
// continues — the next tick retries. Mirrors userkeys.Sweeper.Run.
//
// Called from app/internal/http/api.go as
// `go capabilitySweeper.Run(ctx)`.
func (s *CapabilitySweeper) Run(ctx context.Context) {
	// Initial sweep covers expirations accumulated during downtime.
	_, _ = s.SweepOnce(ctx)

	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.SweepOnce(ctx)
		}
	}
}

// SweepOnce executes one expiry pass: reaps every non-protected
// expired grant + revoke in a bulk DELETE, then walks the
// protected GLOBAL system.admin grant candidates per-row,
// enforcing the last-admin invariant. Per-row audit fires for
// every reaped row; cache invalidation broadcasts once per
// affected user (deduped — LISTEN/NOTIFY churn is real on big
// installs).
//
// Returns (grantsReaped, revokesReaped). The grantsReaped count
// includes both bulk-reaped and individually-reaped admin grants.
//
// Errors are logged at WARN + counted as 0 for the failing query;
// the call never propagates. The next tick retries.
func (s *CapabilitySweeper) SweepOnce(ctx context.Context) (int64, int64) {
	// ⛔ STRUCTURAL SERIALIZATION (#1173, #1119). This sweep reaps
	// expired grants and revokes across EVERY user at once, so it cannot
	// name the users it affects and cannot take their per-user locks.
	// It takes the structural key instead, which every authority READER
	// also holds shared — so a batch mid-flight cannot have its verdict
	// invalidated by a reap that commits underneath it.
	//
	// One transaction for the whole sweep, so the lock is held across
	// all of it and released by COMMIT. A failure inside degrades to the
	// same "log and retry next tick" contract this sweeper already has.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logWarn(ctx, "auth.capability_sweeper.begin.error", err)
		return 0, 0
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockStructuralAuthorityForUpdate(ctx, tx); err != nil {
		s.logWarn(ctx, "auth.capability_sweeper.lock.error", err)
		return 0, 0
	}
	q := New(tx)

	grants, err := q.SweepExpiredGrants(ctx)
	if err != nil {
		s.logWarn(ctx, "auth.capability_sweeper.grants.error", err)
		grants = nil
	}
	revokes, err := q.SweepExpiredRevokes(ctx)
	if err != nil {
		s.logWarn(ctx, "auth.capability_sweeper.revokes.error", err)
		revokes = nil
	}

	// Protected sweep: GLOBAL system.admin grants past expires_at
	// need per-row last-admin checking. Skip a reap that would
	// leave the system with zero active admins; log a WARN so
	// the operator can extend or replace the grant.
	adminCandidates, err := q.ListExpiredAdminGrants(ctx)
	if err != nil {
		s.logWarn(ctx, "auth.capability_sweeper.admin_candidates.error", err)
		adminCandidates = nil
	}
	adminReaped := make([]ListExpiredAdminGrantsRow, 0, len(adminCandidates))
	for _, c := range adminCandidates {
		remaining, err := q.CountActiveAdminsIfRowRemoved(ctx, c.UserRef)
		if err != nil {
			s.logWarn(ctx, "auth.capability_sweeper.admin_count.error", err)
			continue
		}
		if remaining == 0 {
			// Stuck open — keep the row, surface the situation.
			if s.logger != nil {
				s.logger.LogAttrs(ctx, slog.LevelWarn,
					"auth.capability_sweeper.last_admin.stuck_open",
					slog.Int64("user_ref", c.UserRef),
					slog.Time("expired_at", c.ExpiresAt.Time),
				)
			}
			continue
		}
		n, derr := q.DeleteUserGrant(ctx, DeleteUserGrantParams{
			UserRef:        c.UserRef,
			CapabilityCode: c.CapabilityCode,
			TeamID:         c.TeamID,
		})
		if derr != nil || n == 0 {
			if derr != nil {
				s.logWarn(ctx, "auth.capability_sweeper.admin_delete.error", derr)
			}
			continue
		}
		adminReaped = append(adminReaped, c)
	}

	if len(grants) == 0 && len(revokes) == 0 && len(adminReaped) == 0 {
		return 0, 0 // happy steady state; stay quiet
	}

	// ── COMMIT, THEN THE CONSEQUENCES ──────────────────────────────
	//
	// ⛔ EVERY EFFECT BELOW IS A BEST-EFFORT CONSEQUENCE OF A REAP THAT
	// HAS ALREADY HAPPENED, so none of them may run before the reap is
	// durable. Emitting "this grant was reaped" audit events and
	// request cascades and then rolling back would leave the system
	// asserting a change to durable state that never occurred — for
	// rows that are still present.
	//
	// The commit also RELEASES the structural authority lock, which is
	// transaction-scoped. That is deliberate: the callbacks reach out to
	// the audit recorder and the requests package on their own
	// connections, and holding a lock that excludes every authority
	// reader while they do so would put unrelated work behind an
	// external effect.
	//
	// Nothing below can un-reap anything. A callback that fails is
	// logged and the sweep stands, which is the contract this sweeper
	// has always had — post-commit is what makes that contract honest
	// rather than merely stated.
	if err := tx.Commit(ctx); err != nil {
		s.logWarn(ctx, "auth.capability_sweeper.commit.error", err)
		return 0, 0
	}

	affected := make(map[int64]struct{}, len(grants)+len(revokes)+len(adminReaped))
	for _, g := range grants {
		teamID := pgUUIDStr(g.TeamID)
		if s.auditGrant != nil {
			s.auditGrant(ctx, g.UserRef, g.CapabilityCode, teamID, g.ExpiresAt.Time)
		}
		affected[g.UserRef] = struct{}{}
		s.cascadeIfRequestLinked(ctx, g.RequestRef, g.ExpiresAt.Time)
	}
	for _, g := range adminReaped {
		teamID := pgUUIDStr(g.TeamID)
		if s.auditGrant != nil {
			s.auditGrant(ctx, g.UserRef, g.CapabilityCode, teamID, g.ExpiresAt.Time)
		}
		affected[g.UserRef] = struct{}{}
		s.cascadeIfRequestLinked(ctx, g.RequestRef, g.ExpiresAt.Time)
	}
	for _, r := range revokes {
		teamID := pgUUIDStr(r.TeamID)
		if s.auditRevoke != nil {
			s.auditRevoke(ctx, r.UserRef, r.CapabilityCode, teamID, r.ExpiresAt.Time)
		}
		affected[r.UserRef] = struct{}{}
	}

	if s.invalidateCaps != nil {
		for userRef := range affected {
			s.invalidateCaps(ctx, userRef)
		}
	}

	totalGrants := len(grants) + len(adminReaped)
	if s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelInfo,
			"auth.capability_sweeper.reaped",
			slog.Int("grants", totalGrants),
			slog.Int("revokes", len(revokes)),
			slog.Int("affected_users", len(affected)),
		)
	}

	return int64(totalGrants), int64(len(revokes))
}

// cascadeIfRequestLinked invokes the Phase 1.17.E request-cascade
// callback when the reaped grant has a request_ref. Failure logs
// at WARN but doesn't roll back the grant reap (the grant is
// already gone) or fail the sweep — best-effort by contract.
func (s *CapabilitySweeper) cascadeIfRequestLinked(ctx context.Context, requestRef pgtype.UUID, expiredAt time.Time) {
	if !requestRef.Valid || s.cascadeRequest == nil {
		return
	}
	rid := uuid.UUID(requestRef.Bytes)
	if err := s.cascadeRequest(ctx, rid, expiredAt); err != nil {
		s.logWarn(ctx, "auth.capability_sweeper.request_cascade.error", err)
	}
}

func (s *CapabilitySweeper) logWarn(ctx context.Context, msg string, err error) {
	if s.logger == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		return // shutdown path; don't log a "DB unavailable" spurious WARN
	}
	s.logger.LogAttrs(ctx, slog.LevelWarn, msg, slog.String("err", err.Error()))
}

// pgUUIDStr renders a pgtype.UUID's value as a stringy UUID for
// audit metadata. Returns "" for SQL NULL — matches the openapi
// convention where global (un-scoped) overrides have no team_id.
func pgUUIDStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
