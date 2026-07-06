package feedback

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolVisibility is the production VisibilityChecker. Mirrors the
// asset predicate the /search handler already ships (Phase 1.16.B-2):
// non-deleted assets are visible to any authenticated caller. Team +
// owner ACLs are applied downstream at asset-detail projection time,
// so the CanSee gate here only needs to gate soft-delete + existence.
//
// Feedback submits go through this gate so an attacker can't harvest
// UUID existence signal by attempting feedback on random IDs.
type PoolVisibility struct {
	Pool *pgxpool.Pool
}

// CanSee returns true if the asset exists and is not soft-deleted.
// Anonymous callers never reach here — the HTTP handler 401s before
// invoking the service. `userRef` is accepted for interface parity
// with future team-scoped visibility rules; not used today.
func (p PoolVisibility) CanSee(ctx context.Context, userRef int64, assetID uuid.UUID) (bool, error) {
	var exists bool
	err := p.Pool.QueryRow(ctx, `
		SELECT EXISTS(
		    SELECT 1 FROM assets
		     WHERE id = $1 AND deleted_at IS NULL
		)
	`, assetID).Scan(&exists)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return exists, nil
}
