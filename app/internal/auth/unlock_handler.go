package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// CapUnlock is the capability gate for admin-driven account unlock.
// Seeded by migration 00025 to the Admin role ONLY. Per-user grants
// via user_capability_grants remain available if operators want a
// scoped helper role.
const CapUnlock = "auth.unlock"

// AdminUnlockAccount clears the persistent per-username lockout state
// for the target user. Phase 1.19.D.
//
// Idempotent: calling on an already-unlocked user returns 200 with
// unlocked=false; no audit fires for the no-op case. The Manager's
// AdminUnlock uses UPDATE ... WHERE (failed_login_count > 0 OR
// lockout_until IS NOT NULL) RETURNING; zero-row return means the
// user was already clean.
func (h *Handler) AdminUnlockAccount(
	ctx context.Context,
	req openapi.AdminUnlockAccountRequestObject,
) (openapi.AdminUnlockAccountResponseObject, error) {
	caller := IdentityFromContext(ctx)
	if caller == nil {
		return openapi.AdminUnlockAccount401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !caller.Can(CapUnlock) {
		return openapi.AdminUnlockAccount403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "auth.unlock capability required"},
		}, nil
	}

	// Verify the target user exists. FindUserByRef returns pgx.ErrNoRows
	// on miss; we surface as 404 rather than silently no-op'ing so the
	// admin UI can distinguish "target vanished" from "already unlocked".
	q := New(h.Pool)
	if _, err := q.FindUserByRef(ctx, req.Ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.AdminUnlockAccount404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "user not found"},
			}, nil
		}
		return nil, fmt.Errorf("auth: load unlock target: %w", err)
	}

	if h.LockoutMgr == nil {
		// Lockout subsystem not wired. Return the idempotent no-op
		// shape rather than a 500 so an operator on a not-yet-1.19.D
		// install still sees a sane response.
		return openapi.AdminUnlockAccount200JSONResponse{
			UserRef:          req.Ref,
			Unlocked:         false,
			PriorFailedCount: 0,
		}, nil
	}

	unlockable, ok := h.LockoutMgr.(adminUnlocker)
	if !ok {
		// LockoutMgr is the narrow interface without AdminUnlock;
		// tests may inject a stub without it. Same graceful shape.
		return openapi.AdminUnlockAccount200JSONResponse{
			UserRef:          req.Ref,
			Unlocked:         false,
			PriorFailedCount: 0,
		}, nil
	}

	prior, unlocked, err := unlockable.AdminUnlock(ctx, req.Ref, caller.UserRef)
	if err != nil {
		return nil, fmt.Errorf("auth: admin unlock ref=%d: %w", req.Ref, err)
	}
	return openapi.AdminUnlockAccount200JSONResponse{
		UserRef:          req.Ref,
		Unlocked:         unlocked,
		PriorFailedCount: prior,
	}, nil
}

// adminUnlocker is the extra method on the concrete lockout.Manager
// that AdminUnlockAccount needs beyond the narrow LockoutManager
// interface. Kept as a separate interface so the login-path
// LockoutManager stays minimal + tests can stub either surface.
type adminUnlocker interface {
	AdminUnlock(ctx context.Context, userRef, adminUserRef int64) (int32, bool, error)
}
