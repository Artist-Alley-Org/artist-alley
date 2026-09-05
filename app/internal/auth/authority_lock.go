// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthorityLockSpace is the first key of the transaction-scoped advisory
// lock that serializes a caller's EFFECTIVE AUTHORITY against the
// mutations that authority permits (#1173, #1119).
//
// ⛔ IT IS DELIBERATELY NOT 1173. `displayConditionLockSpace` already
// uses 1173 with small integer subject keys (1 = asset, 2 = collection),
// and this lock's second key is a USER REF — so sharing the space would
// make user 1 collide with the asset display-condition graph lock and
// user 2 with the collection one. Two unrelated invariants would then
// block each other for no reason, and one of them would look flaky.
//
// A registry, so the next lock does not have to rediscover this:
//
//	1173 · display-condition graph  (keys: 1 asset, 2 collection)
//	1119 · effective authority      (keys: user ref, 0 structural)
const AuthorityLockSpace int32 = 1119

// AuthorityStructuralKey is the second key used for authority mutations
// whose blast radius is NOT one known user: the expiry sweeper, which
// reaps grants across every user at once, and a team-closure change,
// which alters how every scoped grant expands.
//
// Zero is free for this. User ref 0 is the anonymous sentinel, is never
// a principal, and can never be the caller of an operation that reads
// its own authority — [authorityKey] documents the one way a real ref
// could ever reduce to it and why that is harmless.
const AuthorityStructuralKey int32 = 0

// authorityKey reduces a user ref to the advisory lock's second key.
//
// A bigint truncated to int4. That is safe by construction rather than
// by luck: two refs colliding would mean two users SHARING one lock,
// which over-serializes and can only ever cost a little concurrency. It
// cannot cause a lock to be missed, which is the only failure that would
// matter. The same reasoning covers a ref that reduces to
// [AuthorityStructuralKey]: it would queue behind structural mutations
// too, which is stricter than required and still correct.
func authorityKey(userRef int64) int32 { return int32(userRef) }

// LockAuthorityForUpdate serializes a mutation of ONE user's effective
// authority against any operation currently acting on that authority.
//
// Call it INSIDE a transaction and BEFORE the write. Transaction-scoped,
// so COMMIT or ROLLBACK releases it and an early return cannot leak it.
//
// ⛔ CALLERS ARE ENUMERATED IN ADR 0019, not summarised as "everything
// that writes authority". Some writers are exempt by construction — a
// role assigned to a user created in the same request, the bootstrap
// that runs before the server serves — and an accurate list with stated
// reasons is worth more than a universal claim that has to be true.
//
// That claim has now been wrong twice, both times from classifying by
// PACKAGE OR COMMAND NAME instead of by CALL SITE. First it missed a
// live writer in another package. Then it wrote off `aa seed` as
// offline, when that command is documented to run against a live server
// and its reset empties the authority tables outright. The exemption
// belongs to the call site: `bootstrap.Run` is exempt at server startup
// and NOT exempt when `aa seed` invokes it.
func LockAuthorityForUpdate(ctx context.Context, db DBTX, userRef int64) error {
	if err := New(db).LockAuthorityExclusive(ctx, LockAuthorityExclusiveParams{
		LockSpace:    AuthorityLockSpace,
		AuthorityKey: authorityKey(userRef),
	}); err != nil {
		return fmt.Errorf("auth: lock authority for user %d: %w", userRef, err)
	}
	return nil
}

// LockStructuralAuthorityForUpdate serializes a mutation whose effect on
// authority is not confined to one user — the expiry sweep, a team
// re-parenting — against every operation currently acting on any
// caller's authority.
func LockStructuralAuthorityForUpdate(ctx context.Context, db DBTX) error {
	if err := New(db).LockAuthorityExclusive(ctx, LockAuthorityExclusiveParams{
		LockSpace:    AuthorityLockSpace,
		AuthorityKey: AuthorityStructuralKey,
	}); err != nil {
		return fmt.Errorf("auth: lock structural authority: %w", err)
	}
	return nil
}

// LockAuthorityShared is THE READER SIDE, and the reason the whole
// mechanism exists.
//
// An operation that reads a caller's effective authority and then
// performs writes that verdict authorizes must call this INSIDE its
// transaction and BEFORE the read. It takes the caller's own key and the
// structural key, so both a targeted grant change and a sweep or closure
// change are excluded for as long as the verdict is being relied upon.
//
// Order is fixed — caller key, then structural — and no mutator takes
// both, so there is no cycle to deadlock on.
func LockAuthorityShared(ctx context.Context, db DBTX, userRef int64) error {
	q := New(db)
	if err := q.LockAuthorityShared(ctx, LockAuthoritySharedParams{
		LockSpace:    AuthorityLockSpace,
		AuthorityKey: authorityKey(userRef),
	}); err != nil {
		return fmt.Errorf("auth: share-lock authority for user %d: %w", userRef, err)
	}
	if err := q.LockAuthorityShared(ctx, LockAuthoritySharedParams{
		LockSpace:    AuthorityLockSpace,
		AuthorityKey: AuthorityStructuralKey,
	}); err != nil {
		return fmt.Errorf("auth: share-lock structural authority: %w", err)
	}
	return nil
}

// AcquireStructuralAuthorityLock takes the EXCLUSIVE structural
// authority lock at SESSION scope, on a connection of its own, and
// returns the function that releases it.
//
// # Why session scope, when everything else here is transaction scoped
//
// The transaction-scoped variants are right for a writer whose whole
// authority mutation is one transaction. `aa seed` is not that. Its
// reset runs a TRUNCATE and a series of DELETEs as separate autocommit
// statements, then hands off to the bootstrap restoration, then to the
// runner's own writes — and every one of those steps changes authority.
// A transaction-scoped lock would be released at the end of whichever
// statement took it, leaving the rest of the span unprotected.
//
// ⛔ THAT WOULD BE THE A76 DEFECT IN A NEW COSTUME: a lock that exists,
// that the production path really takes, and that does not span the
// thing it claims to protect. So the lock is held on a dedicated
// connection for the whole span and released explicitly.
//
// Session and transaction advisory locks share one lock space and
// conflict with each other exactly as you would expect; the scope only
// decides WHEN a lock is released, never who it excludes. So this
// blocks the transaction-scoped SHARED half a batch takes, which is the
// point.
//
// ⚠️ OPERATIONAL CONSEQUENCE, stated because an operator will meet it:
// while a seed holds this, every batch metadata apply WAITS. A full
// reseed takes minutes, so an apply that starts during one will most
// likely exhaust its request deadline and fail rather than queue to
// completion. It fails CLOSED — nothing is written — which is the safe
// direction, and an operator running `aa seed --reset` against a live
// deployment is already accepting that the instance's content is being
// replaced underneath it. It is still a real change in behaviour and
// belongs in the release notes rather than in a surprise.
func AcquireStructuralAuthorityLock(ctx context.Context, pool *pgxpool.Pool) (func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: acquire connection for structural authority lock: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1::INT, $2::INT)`,
		AuthorityLockSpace, AuthorityStructuralKey); err != nil {
		conn.Release()
		return nil, fmt.Errorf("auth: take structural authority lock: %w", err)
	}
	return func() {
		// context.WithoutCancel: the release must run even when the
		// caller's context is already cancelled, or a cancelled seed
		// would leave the lock held until the connection is reaped and
		// every batch apply would wait on a process that has gone.
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1::INT, $2::INT)`,
			AuthorityLockSpace, AuthorityStructuralKey); err != nil {
			_ = err // the connection release below drops the lock anyway
		}
		conn.Release()
	}, nil
}
