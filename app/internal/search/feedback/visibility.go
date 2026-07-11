// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package feedback

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// PoolVisibility is the production VisibilityChecker. Delegates to
// the shared visibility.CanSee helper (Phase 1.16.B-followup #185
// retrofit) so the "asset exists AND is visible to caller" gate lives
// in one place across the app rather than duplicated per subsystem.
//
// Feedback submits go through this gate so an attacker can't harvest
// UUID existence signal by attempting feedback on random IDs. Both
// "asset doesn't exist" and "asset is soft-deleted" collapse to the
// same false/no-error return, and the HTTP layer translates that
// into a single 403 hit_not_visible response — no distinguishable
// signal that leaks existence.
//
// Historical context: prior to the retrofit this type held its own
// inline `SELECT EXISTS(SELECT 1 FROM assets WHERE id = $1 AND
// deleted_at IS NULL)` query. The generated SQL from
// visibility.CanSee(EntityAsset, ...) matches that shape byte-for-
// byte (modulo whitespace), which is why the retrofit is a drop-in
// with no snapshot changes.
type PoolVisibility struct {
	Pool *pgxpool.Pool
}

// CanSee returns true if the asset exists and is visible to the
// caller. Currently synonymous with "exists AND non-deleted" because
// the visibility.EntityAsset predicate only enforces the soft-delete
// gate today; when that predicate grows sensitivity or team-scope
// semantics, this call site inherits them without further changes.
//
// `userRef` becomes the caller identity passed into visibility.Filter.
// The current EntityAsset predicate ignores it, but future extensions
// (team-scoped, share-based) will consume it.
func (p PoolVisibility) CanSee(ctx context.Context, userRef int64, assetID uuid.UUID) (bool, error) {
	caller := visibility.Caller{UserRef: userRef, IsAnonymous: userRef == visibility.AnonymousCaller}
	return visibility.CanSee(ctx, p.Pool, visibility.EntityAsset, caller, assetID)
}
