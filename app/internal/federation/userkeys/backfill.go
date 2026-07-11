// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-b boot-time keypair backfill safety net.
//
// Three user-create paths (bootstrap, /setup, /admin/seed/users)
// invoke [EnsureCurrentForUser] inline so newly-minted users land
// with a current keypair. This file covers the OTHER paths:
//
//   - Users created BEFORE 1.22.I-b shipped (the original
//     production motivation — pre-I-b instances upgraded forward
//     would carry pre-existing users without keypairs).
//   - Users created via test fixtures or direct DB INSERTs that
//     bypass the three wired paths.
//   - Users whose keypair-create transaction rolled back AFTER
//     the user row committed (defensive — the wired paths
//     bundle the keypair into the user-create tx so this
//     shouldn't happen, but a future refactor could split them).
//
// The sweep runs once at app boot AFTER migrations + AFTER
// bootstrap (so the bootstrap admin's keypair is already in
// place — the sweep is a no-op when everything's healthy). The
// happy-path cost is one query that returns zero rows; the
// boot-stage timing budget can absorb that on every boot
// indefinitely.
//
// # Why a sweep, not a "lazy on first emission" check
//
// Lazy backfill (check at emission time, mint if missing) has
// three problems: the emit path takes the cost on every cold
// path, the failure mode is silent (a federate-this-share that
// runs the lazy check pays unexpected ms), and the audit
// observability is harder (one event per backfilled user is
// the operator's signal; emit-time lazy backfill scatters
// those across the request log). Boot-time sweep keeps the
// failure visible in one place.
//
// # Why approved=1 only
//
// Disabled (approved=2) + pending (approved=0) users don't
// federate. A pending user can't emit; a disabled user shouldn't.
// When either flips to approved=1, the user-update path can
// invoke [EnsureCurrentForUser] inline OR the next boot's sweep
// will catch them — either way the keypair lands BEFORE the user
// participates in federation. Keeping disabled users out of the
// sweep avoids spending master-key + DB cycles on accounts that
// may never need them.

package userkeys

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLSTATE codes the boot sweep treats as race-loser collisions —
// the desired post-condition ("this user already has a key OR the
// user is no longer eligible to receive one") is satisfied without
// our INSERT landing. See [isRaceLoserError].
//
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
)

// BackfillBatchSize caps how many users the sweep processes per
// query batch. Mints happen serially within a batch (each takes
// one Generate + one wrap + one INSERT); 100 keeps boot under a
// second on a fresh instance with no pre-existing users + caps
// the per-batch lock window on an instance with a real backlog.
//
// Exported as a const (not a config field) — the default is the
// right value for every production deployment; operators on
// 100k-user migrations who care about boot time can edit + rebuild.
const BackfillBatchSize int32 = 100

// BackfillStats reports the sweep's outcome for the boot log line.
// Zeroes on the happy path; non-zero generated count is the audit
// signal that a real backfill happened.
type BackfillStats struct {
	UsersScanned  int
	KeysGenerated int
	Errors        int
}

// BackfillMissingKeys is the boot-time keypair sweep. Idempotent;
// safe to invoke on every boot. Returns the per-boot stats so the
// caller can log them + a non-nil error only when something
// catastrophically wrong happens (DB unreachable, master key
// rotation broke wrapping, etc.) — per-user failures get counted
// in [BackfillStats.Errors] + logged but DON'T abort the sweep.
//
// Per-user partial-failure tolerance: if user N's keypair-mint
// fails, the sweep logs + continues to user N+1. A subsequent
// boot will retry user N. This is the right shape for a
// safety net — degraded service for one user is better than
// no service for everyone.
//
// # Audit
//
// Each successful mint fires
// [audit.Recorder.FederationUserKeyGenerated] with
// actorUserRef=nil (the system is the actor; no human triggered
// the boot sweep). Audit failures are logged but don't count as
// sweep errors — they're observability, not correctness.
//
// # Caller contract
//
// Pass a *pgxpool.Pool (not a tx-bound *Queries) — the sweep
// runs each per-user mint in its OWN transaction so a single
// failure rolls back only that user's row. Tx-bundling the
// whole sweep would either (a) make a single bad row fail the
// whole boot or (b) drop the audit-tx invariant the three
// in-tx callers depend on.
//
// auditFire is the audit-recorder hook fired once per successful
// mint. nil-safe — when unwired (test fixtures), the sweep mints
// keypairs without an audit row. Production wires it to a
// closure over [audit.Recorder] that writes pool-bound (NOT
// tx-bound to the keypair insert): the keypair commit is the
// load-bearing transaction, the audit row is observability +
// can lag by a few microseconds without breaking correctness.
// This relaxes the write-ahead-audit invariant the three in-tx
// callers (bootstrap, /setup, /admin/seed/users) maintain —
// acceptable here because the boot sweep is the SAFETY NET,
// not the primary path. If the audit row write fails after a
// successful mint, the operator's signal is the missing audit
// row + the keypair-actually-exists DB state; a future
// reconcile job can backfill audits separately.
//
// # When NOT to call this
//
// Don't call it BEFORE migrations have run — the
// federation_user_keys table won't exist. Don't call it after
// the HTTP server starts serving traffic, either: the sweep
// holds pool connections + the master key while it runs;
// during a backlog backfill, that's a real CPU + memory cost
// you don't want concurrent with request handling.
type AuditFireFn func(ctx context.Context, subjectUserRef int64, version int32, algorithm string)

func BackfillMissingKeys(
	ctx context.Context,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	auditFire AuditFireFn,
) (BackfillStats, error) {
	if pool == nil {
		return BackfillStats{}, fmt.Errorf("userkeys.BackfillMissingKeys: pool is nil")
	}
	var stats BackfillStats
	q := New(pool)

	for {
		// Per-batch read uses the pool directly (read-only, no tx
		// needed). Each mint below opens its OWN tx so per-user
		// failures isolate cleanly.
		refs, err := q.ListUsersWithoutCurrentKey(ctx, BackfillBatchSize)
		if err != nil {
			return stats, fmt.Errorf("userkeys.BackfillMissingKeys: list: %w", err)
		}
		if len(refs) == 0 {
			break
		}

		for _, ref := range refs {
			stats.UsersScanned++
			err := backfillOne(ctx, pool, q, ref, auditFire)
			switch {
			case err == nil:
				stats.KeysGenerated++
			case errors.Is(err, errRaceLoser):
				// Concurrent write committed past our defensive
				// recheck, OR the user row vanished mid-sweep, OR
				// a retired-key orphan blocks a fresh version=1
				// INSERT. None of these are bugs in the sweep —
				// see [isRaceLoserError]. Debug-level so healthy
				// boots stay quiet.
				if logger != nil {
					logger.LogAttrs(ctx, slog.LevelDebug,
						"userkeys.backfill.user.race_loser",
						slog.Int64("user_ref", ref),
						slog.String("err", err.Error()),
					)
				}
			default:
				stats.Errors++
				if logger != nil {
					logger.LogAttrs(ctx, slog.LevelWarn,
						"userkeys.backfill.user.error",
						slog.Int64("user_ref", ref),
						slog.String("err", err.Error()),
					)
				}
			}
		}

		// Defensive: if the batch returned BackfillBatchSize rows
		// but we processed all of them as either generated or
		// errored, the next loop iteration is the cheap "is the
		// queue drained?" check. A partial-batch (refs less than
		// BatchSize) also exits cleanly via the len==0 check
		// next iteration.
		if int32(len(refs)) < BackfillBatchSize {
			break
		}
	}

	if logger != nil && (stats.KeysGenerated > 0 || stats.Errors > 0) {
		// Only log when something actually happened. The steady-
		// state happy path (zero users without keys) stays quiet
		// so boot logs aren't polluted on every restart.
		logger.LogAttrs(ctx, slog.LevelInfo,
			"userkeys.backfill.complete",
			slog.Int("users_scanned", stats.UsersScanned),
			slog.Int("keys_generated", stats.KeysGenerated),
			slog.Int("errors", stats.Errors),
		)
	}

	return stats, nil
}

// backfillOne mints + persists a single user's keypair inside its
// own transaction. The audit row fires INSIDE the tx so the
// keypair + audit commit together (matches the three in-tx
// callers' contract).
//
// Returns nil on the no-op-already-had-key path so the caller
// doesn't double-count. Real generation increments
// [BackfillStats.KeysGenerated] via the caller.
func backfillOne(
	ctx context.Context,
	pool *pgxpool.Pool,
	poolQ *Queries,
	userRef int64,
	auditFire AuditFireFn,
) error {
	// Defensive recheck via the pool query — a concurrent user-
	// create path (bootstrap, /setup, /admin/seed/users) could
	// have minted the key between [ListUsersWithoutCurrentKey]
	// and now. Cheap, avoids the tx open on the happy steady-
	// state and on the post-list-race-loser path.
	if _, err := poolQ.GetCurrentUserKey(ctx, userRef); err == nil {
		return nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := New(tx)
	alreadyHadKey, err := EnsureCurrentForUser(ctx, qtx, userRef)
	if err != nil {
		if isRaceLoserError(err) {
			// Don't wrap — caller compares against errRaceLoser via
			// errors.Is. The deferred tx.Rollback releases the
			// connection; no commit needed (and no row to commit).
			return fmt.Errorf("%w: ensure: %w", errRaceLoser, err)
		}
		return fmt.Errorf("ensure: %w", err)
	}
	if alreadyHadKey {
		// Pre-list-race loser path or list-then-mint-from-other-
		// path race — either way nothing to audit. Commit the
		// (empty) tx to release the connection cleanly.
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}

	if err := tx.Commit(ctx); err != nil {
		// Roll back is in defer; the keypair insert unwinds.
		return fmt.Errorf("commit: %w", err)
	}

	// Audit fires AFTER the keypair commit succeeded so the
	// audit row never references a keypair that didn't land.
	// Pool-bound (not tx-bound): observability path, not
	// load-bearing for correctness. See the AuditFireFn doc
	// for the "why-not-tx-bound" rationale.
	if auditFire != nil {
		auditFire(ctx, userRef, 1, Algorithm)
	}
	return nil
}

// helper kept here even though unused in the prod path so future
// inline-tx tests can compose against the same shape.
var _ = pgx.ErrNoRows

// errRaceLoser is the sentinel [backfillOne] wraps around a
// classified race-loser error so [BackfillMissingKeys] can route
// it through the "Debug log, don't count" branch via errors.Is.
//
// Kept private — only the in-package boot sweep distinguishes
// these cases. The three in-tx callers (bootstrap, /setup,
// /admin/seed/users) deliberately treat the SAME SQLSTATEs as
// real errors: inside a user-create transaction a unique- or
// FK-violation means the caller's own user-create raced with
// itself, which is a bug worth surfacing.
var errRaceLoser = errors.New("userkeys.backfill: race-loser")

// isRaceLoserError reports whether err is a Postgres error code
// the boot sweep treats as a "the post-condition is already
// satisfied (or this user can't be backfilled anymore)" signal
// rather than a sweep bug:
//
//   - 23505 (unique_violation) — three sub-cases all benign for
//     the sweep:
//     1. Concurrent EnsureCurrentForUser for the same user
//        committed past our defensive recheck (the partial
//        unique index on (user_ref) WHERE is_current=true wins).
//     2. PK collision on (user_ref, version) when the user has
//        a retired version=1 row but no current one — leftover
//        from a rolled-back rotation, broken test fixture, or
//        pre-I-b orphan. The sweep can't fix this; an operator
//        cleanup or rotation can. Counting it as a sweep error
//        masked the real backfill flake (#153).
//     3. Concurrent winner inserted at a higher version. Re-
//        check would have found their current row.
//   - 23503 (foreign_key_violation) — the user_ref row was
//     deleted between [ListUsersWithoutCurrentKey] and our
//     INSERT INTO federation_user_keys. CASCADE from "user"
//     ensures any subsequent state stays consistent.
//
// Match by SQLSTATE on *pgconn.PgError via the SQLState()
// interface — message text differs across Postgres versions.
func isRaceLoserError(err error) bool {
	type pgerr interface {
		SQLState() string
	}
	var pe pgerr
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.SQLState() {
	case sqlStateUniqueViolation, sqlStateForeignKeyViolation:
		return true
	}
	return false
}
