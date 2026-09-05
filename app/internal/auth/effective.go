// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// ResolveEffectiveIdentity re-derives a caller's EFFECTIVE capabilities
// — global holdings and closure-expanded team-scoped grants — straight
// from the given handle, bypassing every cache.
//
// # Why an operation would ever want this
//
// The Identity a handler receives was resolved when the request
// arrived, and its capability sets come from the per-user LRU that
// [Resolver.loadCapabilities] reads. That is exactly right for the
// ordinary case: a capability check is a question about the caller, and
// a few seconds of cache staleness on an operation that changes one row
// is a cost nobody would pay to remove.
//
// A BULK mutation is the case where that stops being true. It reaches
// up to a thousand records under one authorisation decision, it commits
// them as one outcome, and its authority checks are the only thing
// standing between an operator whose grant was revoked and a thousand
// writes they may no longer make. Re-reading the grants inside the
// transaction that performs the writes is what makes "the caller's
// effective verdict and the mutation it authorises are atomic" a
// statement about the database rather than about a cache.
//
// Pass the transaction handle, not the pool. Read under the same
// snapshot as the writes, the verdict cannot be stale by the time they
// land: a revocation that committed before this read is SEEN, and one
// that commits after it is ordered after this whole transaction, which
// is the serial order the batch's atomicity contract permits.
//
// # Effective, never raw
//
// This is deliberately the same derivation the resolver performs and
// not a hand-rolled read of `user_capability_grants`: a team-scoped
// ROLE assignment produces ZERO rows in that table, so any comparison
// built on it would miss precisely the path that matters, and a caller
// who lost one direct grant while a role still confers the capability
// would be wrongly refused. The question is "does this caller hold it
// now", never "is their grant set byte-identical to before".
//
// The returned Identity carries the ORIGINAL's user ref, username and
// auth method — this re-resolves authority, not who the caller is.
func ResolveEffectiveIdentity(ctx context.Context, db DBTX, base *Identity) (*Identity, error) {
	if base == nil {
		return nil, nil
	}
	fresh := &Identity{
		UserRef:    base.UserRef,
		Username:   base.Username,
		AuthMethod: base.AuthMethod,
	}
	rows, err := New(db).EffectiveScopedCapabilitiesForUser(ctx, base.UserRef)
	if err != nil {
		// Deliberately an ERROR rather than the resolver's log-and-
		// continue. The resolver degrades to an unprivileged identity
		// because a transient failure must not fail an ordinary page
		// load; here the caller is about to commit a batch, and
		// silently downgrading their authority mid-transaction would
		// turn a database blip into a partial-looking result that
		// nobody could explain. The batch refuses instead.
		return nil, fmt.Errorf("auth: re-resolve effective capabilities: %w", err)
	}

	globalSet := make(map[string]struct{}, len(rows))
	scoped := make(map[string]map[uuid.UUID]struct{})
	for _, row := range rows {
		if !row.TeamID.Valid {
			globalSet[row.Code] = struct{}{}
			continue
		}
		team := uuid.UUID(row.TeamID.Bytes)
		set, ok := scoped[row.Code]
		if !ok {
			set = make(map[uuid.UUID]struct{})
			scoped[row.Code] = set
		}
		set[team] = struct{}{}
	}
	caps := make([]string, 0, len(globalSet))
	for code := range globalSet {
		caps = append(caps, code)
	}
	sort.Strings(caps)
	fresh.Capabilities = caps
	fresh.scopedCaps = scoped
	return fresh, nil
}
