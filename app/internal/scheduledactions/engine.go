// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package scheduledactions is the generic scheduled-action engine
// (#40 sprint 1, ADR 0020).
//
// It is a DECLARATIVE layer over the jobs queue, not a second deferred
// runner: the queue already runs future-dated work. What this adds is a
// durable, listable, cancellable record of WHAT is scheduled
// (scheduled_actions), executed by a recurring reaper job (reaper.go)
// through five small audited executors (executor.go).
//
// The consumers this foundation exists for — privacy retention (#44),
// subscription expiry (#51), audit retention (#52) — all name this as
// their executor. The NDA-specific consumers (blur, reveal-with-
// logging, embargo auto-lift) are later sprints of #40.
//
// # What an executor may act on is not what the table may hold (#1238)
//
// The five verbs and the four target kinds do not form twenty
// instructions. Most arms are asset-only; `change_state` also reaches a
// post, through the publication core rather than through SQL of its
// own; `notify` reaches all four. executorTargets is that map, derived
// from the arms, and Store.Schedule refuses anything outside it AT
// SCHEDULE TIME — because a row nothing can execute is not a deferred
// instruction, it is a deferred error report.
package scheduledactions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Action is one of the five scheduled-action verbs (ADR 0020). The DB
// CHECK constraint is the backstop; these constants + Validate give a
// clean error before the write.
type Action string

const (
	ActionRestrict          Action = "restrict"
	ActionDelete            Action = "delete"
	ActionChangeState       Action = "change_state"
	ActionChangeSensitivity Action = "change_sensitivity"
	ActionNotify            Action = "notify"
)

// TargetKind is the polymorphic target's entity type.
type TargetKind string

const (
	TargetAsset      TargetKind = "asset"
	TargetPost       TargetKind = "post"
	TargetCollection TargetKind = "collection"
	TargetUser       TargetKind = "user"
)

// State is the lifecycle of a scheduled action.
const (
	StatePending   = "pending"
	StateDone      = "done"
	StateCancelled = "cancelled"
	StateFailed    = "failed"
)

func validActions() map[Action]bool {
	return map[Action]bool{
		ActionRestrict: true, ActionDelete: true, ActionChangeState: true,
		ActionChangeSensitivity: true, ActionNotify: true,
	}
}

func validTargets() map[TargetKind]bool {
	return map[TargetKind]bool{
		TargetAsset: true, TargetPost: true, TargetCollection: true, TargetUser: true,
	}
}

// executorTargets is which target kinds each action can ACTUALLY be run
// against, derived one row at a time from the executors in executor.go.
// It is not a policy table and must never become one: if an arm grows a
// target, the row grows with it, and if a row here claims a target no
// arm handles, the claim is a fail-at-fire-time bug wearing a
// validation function's clothes.
//
// Derivation, arm by arm:
//
//   - restrict, change_sensitivity → changeSensitivity, whose first act
//     is assetTarget (executor.go). Asset only.
//   - delete → softDelete, likewise assetTarget. Asset only.
//   - change_state → assetTarget for the asset domain, and since #1238
//     the post publication arm as well. Asset + post.
//   - notify → notifyRecipient, which accepts ANY target kind when
//     params.recipient names the addressee, and falls back to reading
//     the recipient off a `user` target when it does not. So all four,
//     and this is the reason `collection` and `user` stay in
//     validTargets rather than being narrowed out of it: notify is a
//     real consumer of both. Whether the params carry a recipient is a
//     PARAMS question, answered at fire time like every other params
//     question (see Store.Schedule's note) — the pair itself is
//     supported.
//
// Every (action, target) NOT in this table is refused by Schedule. That
// closes #1238's class: before it, `validTargets` accepted `post` for
// every verb while three of the four arms refused it, so scheduling one
// enqueued cleanly and failed months later with nobody watching.
func executorTargets() map[Action]map[TargetKind]bool {
	assetOnly := map[TargetKind]bool{TargetAsset: true}
	return map[Action]map[TargetKind]bool{
		ActionRestrict:          assetOnly,
		ActionChangeSensitivity: assetOnly,
		ActionDelete:            assetOnly,
		ActionChangeState:       {TargetAsset: true, TargetPost: true},
		ActionNotify: {
			TargetAsset: true, TargetPost: true,
			TargetCollection: true, TargetUser: true,
		},
	}
}

// supportedTargetList renders one action's targets in a stable order for
// an error message. Sorted so the same refusal reads the same way twice.
func supportedTargetList(a Action) string {
	targets := executorTargets()[a]
	names := make([]string, 0, len(targets))
	for k := range targets {
		names = append(names, string(k))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Store is the create/cancel/list surface. Other epics call Schedule to
// enqueue future work; the admin surface calls List + Cancel.
type Store struct {
	pool *pgxpool.Pool
	q    *Queries
}

// NewStore builds a Store over the pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: New(pool)}
}

// ScheduleInput is one action to schedule.
type ScheduleInput struct {
	Action       Action
	TargetKind   TargetKind
	TargetID     string
	Params       map[string]any
	ScheduledFor pgtype.Timestamptz
	CreatedBy    *int64 // nil for system-scheduled actions
}

// Schedule validates and inserts one pending action.
//
// Validation is here rather than left entirely to the CHECK
// constraints so a bad action verb or target kind is a clear 400 at the
// call site, not an opaque 23514. The params blob is intentionally NOT
// schema-validated at insert time: each executor validates its own
// params when it runs, so a params typo surfaces as a captured failure
// on that one action rather than blocking the schedule.
//
// THE PAIR IS VALIDATED, THOUGH, and that is the #1238 fix. A params
// typo is one action's problem, discovered when it runs, and the row is
// still a coherent instruction. An (action, target) no executor can act
// on is not an instruction at all — it is a row that will sit pending
// for however long the operator asked, then fail at a moment nobody is
// watching, on a target that may have been the whole point of the
// schedule. Everything needed to know that at insert time is already
// known at insert time, so it is refused at insert time.
func (s *Store) Schedule(ctx context.Context, in ScheduleInput) (ScheduledAction, error) {
	if !validActions()[in.Action] {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: unknown action %q", in.Action)
	}
	if !validTargets()[in.TargetKind] {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: unknown target kind %q", in.TargetKind)
	}
	if !executorTargets()[in.Action][in.TargetKind] {
		return ScheduledAction{}, fmt.Errorf(
			"scheduledactions: no executor can run %q against a %q target (supported: %s)",
			in.Action, in.TargetKind, supportedTargetList(in.Action))
	}
	// A post's publication is an ACT by somebody: the executor runs it
	// through the same body the endpoint uses, whose gates are the
	// actor's capabilities and whose federation activity is emitted in
	// the actor's name. `created_by` is nullable because a retention
	// delete has nobody behind it; a publication cannot borrow that.
	// Refused here rather than at fire time for the same reason the pair
	// is.
	if in.Action == ActionChangeState && in.TargetKind == TargetPost && in.CreatedBy == nil {
		return ScheduledAction{}, fmt.Errorf(
			"scheduledactions: a scheduled post state change needs created_by — " +
				"it publishes as that user and federates in their name")
	}
	if in.TargetID == "" {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: target id required")
	}
	if !in.ScheduledFor.Valid {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: scheduled_for required")
	}
	params := in.Params
	if params == nil {
		params = map[string]any{}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: marshal params: %w", err)
	}
	return s.q.CreateScheduledAction(ctx, CreateScheduledActionParams{
		Action:       string(in.Action),
		TargetKind:   string(in.TargetKind),
		TargetID:     in.TargetID,
		Params:       raw,
		ScheduledFor: in.ScheduledFor,
		CreatedBy:    in.CreatedBy,
	})
}

// Cancel marks a pending action cancelled. Returns true when a pending
// action was cancelled, false when there was nothing cancellable (the
// id is unknown, or the action already fired / was already cancelled).
// The caller maps false to 404/409 as fits its surface.
func (s *Store) Cancel(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.q.CancelScheduledAction(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return false, fmt.Errorf("scheduledactions: cancel: %w", err)
	}
	return n > 0, nil
}

// ListInput filters the admin list.
type ListInput struct {
	State           *string // nil = all states
	CursorCreatedAt pgtype.Timestamptz
	Limit           int32
}

// List returns actions newest-first for the admin surface.
func (s *Store) List(ctx context.Context, in ListInput) ([]ScheduledAction, error) {
	if in.Limit <= 0 || in.Limit > 200 {
		in.Limit = 50
	}
	rows, err := s.q.ListScheduledActions(ctx, ListScheduledActionsParams{
		State:           in.State,
		CursorCreatedAt: in.CursorCreatedAt,
		RowLimit:        in.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("scheduledactions: list: %w", err)
	}
	return rows, nil
}

// Get returns one action by id.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (ScheduledAction, error) {
	return s.q.GetScheduledAction(ctx, pgtype.UUID{Bytes: id, Valid: true})
}
