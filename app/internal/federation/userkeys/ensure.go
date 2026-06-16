package userkeys

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EnsureCurrentForUser is the idempotent "this user has a current
// X25519 keypair" guarantee. Three user-create paths invoke it
// (bootstrap, /setup, /admin/seed/users) so that every user reaches
// a steady state with exactly one current key — the precondition
// every later-phase consumer (I-c actor profile, I-e outbox
// encryption, I-f inbox decryption) assumes.
//
// Behaviour
//
//   - Existing current key → no-op, returns (true, nil).
//   - No current key → mint via [Generate], insert at version=1,
//     returns (false, nil).
//   - Any other DB or crypto failure → returns (_, err) so the
//     transaction the caller wrapped this in rolls back. Users
//     shipping without a federation key would be a permanent
//     hole; better to refuse the user-create than to half-create.
//
// The first return value tells the caller whether to emit the
// audit event — only the create path fires
// [audit.EventFederationUserKeyGenerated]; the "already had one"
// path doesn't, because nothing changed.
//
// Concurrency
//
// The check-then-insert pattern races against a concurrent
// EnsureCurrentForUser for the same user. The migration's partial
// UNIQUE INDEX is the tiebreaker: the loser's insert fails with a
// unique violation, which this function detects + retries by
// re-fetching the current row that the winner committed. The
// idempotency invariant holds across either outcome.
//
// Callers should pass a transaction-bound *Queries (constructed
// via [New] with a pgx.Tx) so the key insert participates in the
// user-create transaction's atomicity.
func EnsureCurrentForUser(ctx context.Context, q *Queries, userRef int64) (alreadyHadKey bool, err error) {
	// Fast path: the user already has a current key. Most
	// re-runs hit this branch (bootstrap is idempotent across
	// boots, seed re-runs against an existing dataset, etc.).
	if _, err := q.GetCurrentUserKey(ctx, userRef); err == nil {
		return true, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("userkeys: check current: %w", err)
	}

	// Mint fresh and insert at version=1.
	pub, wrapped, err := Generate()
	if err != nil {
		return false, fmt.Errorf("userkeys: generate: %w", err)
	}

	_, err = q.InsertUserKey(ctx, InsertUserKeyParams{
		UserRef:        userRef,
		Version:       1,
		Algorithm:     Algorithm,
		PublicKey:     pub,
		PrivateKeyEnc: wrapped,
		IsCurrent:     true,
	})
	if err == nil {
		return false, nil
	}

	// Concurrent EnsureCurrentForUser for the same user committed
	// first; their row won the partial UNIQUE INDEX. Confirm by
	// re-fetching the current row + return as the no-op path. If
	// the re-fetch fails the original insert error is the more
	// informative surface.
	if isUniqueViolation(err) {
		if _, gErr := q.GetCurrentUserKey(ctx, userRef); gErr == nil {
			return true, nil
		}
	}
	return false, fmt.Errorf("userkeys: insert: %w", err)
}

// isUniqueViolation reports whether err is a Postgres unique-key
// violation (SQLSTATE 23505). Kept local to this file rather than
// imported broadly because the only spot that needs it is the
// EnsureCurrentForUser concurrency race-resolver path.
func isUniqueViolation(err error) bool {
	type pgerr interface {
		SQLState() string
	}
	var pe pgerr
	if errors.As(err, &pe) {
		return pe.SQLState() == "23505"
	}
	return false
}
