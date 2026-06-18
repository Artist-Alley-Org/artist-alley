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

// SweepOnce executes one DELETE pass per table, fires per-row
// audit, and broadcasts cache invalidation for every affected
// user. Returns (grantsReaped, revokesReaped). Exported so admin
// tooling + tests can drive a single sweep without spinning up
// the Run loop.
//
// Errors are logged at WARN + counted as 0; the call never
// propagates. The next tick retries.
func (s *CapabilitySweeper) SweepOnce(ctx context.Context) (int64, int64) {
	q := New(s.pool)

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

	if len(grants) == 0 && len(revokes) == 0 {
		return 0, 0 // happy steady state; stay quiet
	}

	// Per-row audit + cache invalidation. Use a set keyed by
	// user_ref so a user with N reaped rows gets exactly one
	// invalidate broadcast (LISTEN/NOTIFY churn is real on big
	// installs; dedup at the source).
	affected := make(map[int64]struct{}, len(grants)+len(revokes))
	for _, g := range grants {
		teamID := pgUUIDStr(g.TeamID)
		if s.auditGrant != nil {
			s.auditGrant(ctx, g.UserRef, g.CapabilityCode, teamID, g.ExpiresAt.Time)
		}
		affected[g.UserRef] = struct{}{}
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

	if s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelInfo,
			"auth.capability_sweeper.reaped",
			slog.Int("grants", len(grants)),
			slog.Int("revokes", len(revokes)),
			slog.Int("affected_users", len(affected)),
		)
	}

	return int64(len(grants)), int64(len(revokes))
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
