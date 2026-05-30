// Package workflow implements the state-machine slice of the API
// (ADR 0010 Layer 7). Every state change goes through Service.Transition
// — direct UPDATE of state_id is reserved for resource creation (set
// to the initial state) and explicit migration / seed code.
//
// The state machine itself is data-driven: workflow_states + workflow_transitions
// define what's allowed. Configurable per-domain via the migration /
// admin endpoint, so different asset types (Photo vs Video vs Post)
// can have completely different workflows.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// ResourceKind identifies which kind of resource a transition targets.
// Matches the values used in workflow_audit.resource_kind.
type ResourceKind string

const (
	KindPost  ResourceKind = "post"
	KindAsset ResourceKind = "asset"
)

// PostDomain is the workflow domain string for the Post entity.
// Constant today; reserved as a function for symmetry with AssetDomain.
const PostDomain = "post"

// AssetDomain returns the workflow domain string for an asset of the
// given RS asset_type. Format: "asset:<ref>". A future plugin asset
// kind could extend this convention (e.g. "asset:plugin:custom").
func AssetDomain(resourceTypeRef int64) string {
	return "asset:" + strconv.FormatInt(resourceTypeRef, 10)
}

// Errors returned by Transition. Handlers map these to HTTP codes.
var (
	// ErrTransitionNotAllowed: (from_state, to_state) is not a row in
	// workflow_transitions. The caller asked for an illegal move.
	// Handler maps to 400.
	ErrTransitionNotAllowed = errors.New("workflow: transition not allowed")

	// ErrInsufficientCapability: the caller authenticated but lacks
	// the required capability (with team scope if requires_team_scope).
	// Handler maps to 403.
	ErrInsufficientCapability = errors.New("workflow: caller lacks required capability")

	// ErrResourceNotFound: the resource id doesn't resolve, or the
	// resource has been soft-deleted. Handler maps to 404.
	ErrResourceNotFound = errors.New("workflow: resource not found")

	// ErrInvalidKind: caller passed a ResourceKind that isn't post or
	// asset. Programming error; handler maps to 500.
	ErrInvalidKind = errors.New("workflow: unknown resource kind")

	// ErrNoteRequired: the target state has requires_note=true and the
	// caller passed an empty note. Handler maps to 400. Patterned on
	// RSE's archive_states.more_notes_flag — a transition into a
	// "rejected" / "needs work" state without a reason is almost
	// always a bug or a confused user, so we reject loudly rather
	// than silently storing a blank note.
	ErrNoteRequired = errors.New("workflow: target state requires a note")
)

// Service is the only sanctioned entry point for resource state changes.
type Service struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

func NewService(pool *pgxpool.Pool, logger *slog.Logger) *Service {
	return &Service{Pool: pool, Logger: logger}
}

// InitialStateID returns the initial state for a domain. Resource
// creation code calls this to populate state_id with the entry point.
// Returns uuid.Nil + pgx.ErrNoRows if the domain has no initial state
// configured (a setup gap).
func (s *Service) InitialStateID(ctx context.Context, domain string) (uuid.UUID, error) {
	row, err := New(s.Pool).GetInitialState(ctx, domain)
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.UUID(row.ID.Bytes), nil
}

// Transition moves the resource to toStateID, enforcing the workflow's
// from→to edge list and the per-transition capability gate.
//
// Algorithm:
//  1. Read the resource's current (state_id, team_id) in one query.
//  2. Look up the workflow_transitions row for (current, to). Absent →
//     ErrTransitionNotAllowed.
//  3. If required_capability is set, check the caller. If
//     requires_team_scope, wrap with auth.InTeam(resource_team_id);
//     if the resource has no team_id and requires_team_scope, only
//     globally-held caps pass. (A team-scoped check on a team-less
//     resource has no scope to match against.)
//  4. Inside a transaction: write workflow_audit row, then UPDATE the
//     resource's state_id. Both succeed together or neither does.
//
// Caller may pass a nil note; it's stored as the empty string.
func (s *Service) Transition(
	ctx context.Context,
	kind ResourceKind,
	resourceID uuid.UUID,
	toStateID uuid.UUID,
	caller *auth.Identity,
	note string,
) error {
	if caller == nil {
		return ErrInsufficientCapability
	}

	pgResID := pgtype.UUID{Bytes: resourceID, Valid: true}
	pgToID := pgtype.UUID{Bytes: toStateID, Valid: true}

	q := New(s.Pool)

	// 1. Load the resource's current state + team scope.
	var fromState pgtype.UUID
	var teamID pgtype.UUID
	switch kind {
	case KindPost:
		row, err := q.GetPostState(ctx, pgResID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrResourceNotFound
			}
			return fmt.Errorf("workflow: load post state: %w", err)
		}
		fromState = row.StateID
		teamID = row.TeamID
	case KindAsset:
		row, err := q.GetAssetState(ctx, pgResID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrResourceNotFound
			}
			return fmt.Errorf("workflow: load asset state: %w", err)
		}
		fromState = row.StateID
		teamID = row.TeamID
	default:
		return ErrInvalidKind
	}

	// 2. Validate the transition exists.
	trans, err := q.FindTransition(ctx, FindTransitionParams{
		FromStateID: fromState, // pgtype.UUID; Valid=false renders as NULL
		ToStateID:   pgToID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTransitionNotAllowed
		}
		return fmt.Errorf("workflow: find transition: %w", err)
	}

	// 2.5 If the target state requires a note (e.g. a rejection
	//     state), reject empty/whitespace-only notes before we touch
	//     the capability check — the user gets the more actionable
	//     400 rather than burning a permission round-trip first.
	toState, err := q.GetState(ctx, pgToID)
	if err != nil {
		return fmt.Errorf("workflow: load target state: %w", err)
	}
	if toState.RequiresNote && strings.TrimSpace(note) == "" {
		return ErrNoteRequired
	}

	// 3. Capability check.
	if trans.RequiredCapability != nil && *trans.RequiredCapability != "" {
		var canIt bool
		if trans.RequiresTeamScope && teamID.Valid {
			canIt = caller.Can(*trans.RequiredCapability, auth.InTeam(uuid.UUID(teamID.Bytes)))
		} else {
			canIt = caller.Can(*trans.RequiredCapability)
		}
		if !canIt {
			return ErrInsufficientCapability
		}
	}

	// 4. Audit + update in one transaction so we never write audit
	//    without the state change (or vice versa).
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("workflow: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := New(tx)

	actor := caller.UserRef
	if err := tq.InsertWorkflowAudit(ctx, InsertWorkflowAuditParams{
		ResourceKind:   string(kind),
		ResourceID:     pgResID,
		FromStateID:    fromState,
		ToStateID:      pgToID,
		ActorRsUserID:  &actor,
		Note:           note,
	}); err != nil {
		return fmt.Errorf("workflow: insert audit: %w", err)
	}

	var rows int64
	switch kind {
	case KindPost:
		rows, err = tq.UpdatePostState(ctx, UpdatePostStateParams{
			ID:      pgResID,
			StateID: pgToID,
		})
	case KindAsset:
		rows, err = tq.UpdateAssetState(ctx, UpdateAssetStateParams{
			ID:      pgResID,
			StateID: pgToID,
		})
	}
	if err != nil {
		return fmt.Errorf("workflow: update resource state: %w", err)
	}
	if rows == 0 {
		// The resource vanished between the GetXxxState read and the
		// UPDATE — race with soft-delete. Surface as not-found so the
		// handler returns 404 rather than 500.
		return ErrResourceNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("workflow: commit: %w", err)
	}
	return nil
}
