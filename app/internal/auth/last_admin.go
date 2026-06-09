// Last-admin invariant helper. Guards the admin user-mutation
// endpoints (deactivate / demote / explicit-revoke / add-
// explicit-revoke) so an operator can never accidentally lock
// every admin out of the system — the bootstrap admin from
// internal/bootstrap is enough to keep one always present, but
// only if the management endpoints refuse to remove the last
// holder.

package auth

import (
	"context"
	"errors"
)

// ErrLastAdmin is the sentinel admin handlers translate into
// a 409 Conflict. The error message is operator-facing and
// MAY surface to the admin UI verbatim.
var ErrLastAdmin = errors.New("refusing to leave the system with zero admins; grant system.admin to another user first")

// EnsureNotLastAdmin returns ErrLastAdmin when the proposed
// operation would leave zero approved users holding the
// system.admin capability. Callers invoke this BEFORE
// mutating; the function makes no DB writes.
//
// The check is conservative: it asks "would removing this
// user's system.admin leave zero admins?" — if so, refuse.
// Operations that DON'T remove system.admin from this user
// (e.g. setUserRole where the new role ALSO grants
// system.admin) can skip this guard; the guard is only
// relevant when the operation strips the capability.
//
// Pool-bound (NOT tx-bound) by design — the count + the
// would-strip check happen in two queries; a race with a
// concurrent admin add could theoretically allow the
// operation, but the next admin write would catch the
// invariant violation on its own check. The mutation handlers
// invoke this guard first, then perform their write; the
// trade-off is "guard is best-effort, eventual consistency"
// vs "wrap every admin mutation in serialisable tx" — the
// former is fine because the bootstrap admin is always
// recoverable from /var/lib/artist-alley/bootstrap-admin.txt.
func EnsureNotLastAdmin(ctx context.Context, q *Queries, userRef int64) error {
	holds, err := q.UserHoldsSystemAdmin(ctx, userRef)
	if err != nil {
		return err
	}
	if holds == 0 {
		// User doesn't hold admin → operation is fine.
		return nil
	}
	total, err := q.CountSystemAdmins(ctx)
	if err != nil {
		return err
	}
	if total <= 1 {
		return ErrLastAdmin
	}
	return nil
}
