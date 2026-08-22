// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package scheduledactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Notifier is the executor's view of the notification subsystem —
// exactly the socialNotifyAdapter shape boot already has, so wiring is
// a one-liner and this package takes no dependency on internal/
// notifications. nil = notifications unavailable; a notify action then
// fails loudly rather than silently no-op'ing.
type Notifier interface {
	Notify(ctx context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, payload map[string]any) error
}

// Publisher is the executor's view of the post publication core — the
// one method posts.Handler exposes for callers with no HTTP request
// behind them, so wiring is a one-liner and this package takes no
// dependency on internal/posts.
//
// ⛔ Why this seam exists at all, rather than an UPDATE in queries.sql
// beside the asset arms: publishing a post moves `posts.state_id` AND
// writes the federation activity that tells peers the post exists, in
// ONE transaction (ADR 0044, posts/publication.go). An executor that
// wrote the column directly would publish the post on this instance and
// on no other, silently, with every test still green. So the post arm
// owns no SQL — it calls the same body the endpoint calls.
//
// nil = the core is unwired; a post state change then fails loudly
// rather than falling back to a write that skips federation.
type Publisher interface {
	// MovePostPublication publishes (publish=true) or unpublishes the
	// post on behalf of actorUserRef. moved=false with a nil error means
	// the post was ALREADY in that state — an idempotent no-op, not a
	// failure.
	MovePostPublication(ctx context.Context, postID uuid.UUID, publish bool, actorUserRef int64) (bool, error)
}

// actionParams is the union of every executor's params. Each executor
// reads only the fields it needs; unknown/empty fields are ignored.
// Validated per-executor at run time, deliberately not at schedule time
// (see Store.Schedule) so a params typo is a captured failure on one
// action, not a rejected schedule.
type actionParams struct {
	To        string `json:"to"`          // change_sensitivity: target tier
	ToState   string `json:"to_state"`    // change_state: workflow-state code
	ToStateID string `json:"to_state_id"` // change_state: explicit state uuid
	Reason    string `json:"reason"`      // delete / general context
	Verb      string `json:"verb"`        // notify: notification verb
	Recipient *int64 `json:"recipient"`   // notify: overrides a user target
}

// executor runs one action's domain change AND writes its audit row,
// both on the same tx-bound handles, so a torn write is impossible: an
// action is marked done iff its domain change and its audit row both
// committed. Each case ends by calling recordExecuted — removing that
// call from any one executor is what the sabotage test catches.
type executor struct {
	rec       *audit.Recorder
	notifier  Notifier
	publisher Publisher
}

// execute dispatches one action. q + auditQ are bound to the SAME
// transaction (a savepoint of the reaper's claim tx), so everything an
// executor writes rolls back together on error.
func (e *executor) execute(ctx context.Context, q *Queries, auditQ *audit.Queries, a ScheduledAction) error {
	var p actionParams
	if len(a.Params) > 0 {
		if err := json.Unmarshal(a.Params, &p); err != nil {
			return fmt.Errorf("bad params: %w", err)
		}
	}
	switch Action(a.Action) {
	case ActionChangeSensitivity:
		return e.changeSensitivity(ctx, q, auditQ, a, p.To)
	case ActionRestrict:
		// restrict is change_sensitivity to the named 'restricted' tier.
		// It exists as its own verb because "restrict this on date X" is
		// the recipe operators think in; the params can stay empty.
		return e.changeSensitivity(ctx, q, auditQ, a, "restricted")
	case ActionDelete:
		return e.softDelete(ctx, q, auditQ, a, p.Reason)
	case ActionChangeState:
		return e.changeState(ctx, q, auditQ, a, p)
	case ActionNotify:
		return e.notify(ctx, auditQ, a, p)
	default:
		return fmt.Errorf("unknown action %q", a.Action)
	}
}

func (e *executor) assetTarget(a ScheduledAction) (pgtype.UUID, error) {
	if TargetKind(a.TargetKind) != TargetAsset {
		return pgtype.UUID{}, fmt.Errorf("action %q requires an asset target, got %q", a.Action, a.TargetKind)
	}
	id, err := uuid.Parse(a.TargetID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("target id %q is not a uuid: %w", a.TargetID, err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

func (e *executor) changeSensitivity(ctx context.Context, q *Queries, auditQ *audit.Queries, a ScheduledAction, to string) error {
	valid := map[string]bool{"public": true, "team": true, "restricted": true, "embargo": true}
	if !valid[to] {
		return fmt.Errorf("change_sensitivity: invalid tier %q", to)
	}
	id, err := e.assetTarget(a)
	if err != nil {
		return err
	}
	old, err := q.ExecAssetChangeSensitivity(ctx, ExecAssetChangeSensitivityParams{ID: id, Sensitivity: to})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("change_sensitivity: asset %s not found or deleted", a.TargetID)
	}
	if err != nil {
		return fmt.Errorf("change_sensitivity: %w", err)
	}
	e.recordExecuted(ctx, auditQ, a, nil, map[string]any{
		"field": "sensitivity", "old": old, "new": to,
	})
	return nil
}

func (e *executor) softDelete(ctx context.Context, q *Queries, auditQ *audit.Queries, a ScheduledAction, reason string) error {
	id, err := e.assetTarget(a)
	if err != nil {
		return err
	}
	var reasonArg *string
	if reason != "" {
		reasonArg = &reason
	}
	// The second soft-delete path for assets (the other is
	// assets.Handler.DeleteAsset), and it has to record a deleter too
	// or a scheduled delete would land with deleted_by_user_ref NULL
	// while an interactive one recorded it. a.CreatedBy is the user who
	// scheduled the action, and is itself nil for a system-scheduled
	// retention delete — which fails closed: nobody self-restores a row
	// nobody is recorded as having deleted (#931).
	n, err := q.ExecAssetSoftDelete(ctx, ExecAssetSoftDeleteParams{
		ID:               id,
		DeletedReason:    reasonArg,
		DeletedByUserRef: a.CreatedBy,
	})
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	// rows==0 means the asset was already soft-deleted or is gone.
	// Deleting an already-deleted thing is idempotent SUCCESS, not a
	// failure — this executor backs trash-retention, where the target
	// legitimately may have been removed already. Recorded with
	// skipped=true so the trail is honest about the no-op.
	e.recordExecuted(ctx, auditQ, a, nil, map[string]any{
		"field": "deleted_at", "skipped": n == 0, "reason": reason,
	})
	return nil
}

func (e *executor) changeState(ctx context.Context, q *Queries, auditQ *audit.Queries, a ScheduledAction, p actionParams) error {
	// A post's state is its PUBLICATION, and publication is not a column
	// write on this codebase — see Publisher. Everything else about the
	// arm (params shape, audit row, terminal outcome) is the same.
	if TargetKind(a.TargetKind) == TargetPost {
		return e.changePostState(ctx, q, auditQ, a, p)
	}
	id, err := e.assetTarget(a)
	if err != nil {
		return err
	}
	var stateID pgtype.UUID
	switch {
	case p.ToStateID != "":
		sid, perr := uuid.Parse(p.ToStateID)
		if perr != nil {
			return fmt.Errorf("change_state: to_state_id %q is not a uuid: %w", p.ToStateID, perr)
		}
		stateID = pgtype.UUID{Bytes: sid, Valid: true}
	case p.ToState != "":
		// Resolve the code within the asset workflow domain.
		sid, rerr := q.ResolveWorkflowStateByCode(ctx, ResolveWorkflowStateByCodeParams{Domain: "asset", Code: p.ToState})
		if errors.Is(rerr, pgx.ErrNoRows) {
			return fmt.Errorf("change_state: no asset state with code %q", p.ToState)
		}
		if rerr != nil {
			return fmt.Errorf("change_state: resolve %q: %w", p.ToState, rerr)
		}
		stateID = sid
	default:
		return fmt.Errorf("change_state: params need to_state (code) or to_state_id (uuid)")
	}
	oldStateID, err := q.ExecAssetChangeState(ctx, ExecAssetChangeStateParams{ID: id, StateID: stateID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("change_state: asset %s not found or deleted", a.TargetID)
	}
	if err != nil {
		return fmt.Errorf("change_state: %w", err)
	}
	e.recordExecuted(ctx, auditQ, a, nil, map[string]any{
		"field": "state_id", "old": uuidText(oldStateID), "new": uuidText(stateID),
	})
	return nil
}

// changePostState is the post arm of change_state: scheduled
// publication (#1238).
//
// The fire-time outcome table, decided at planning and implemented
// here rather than re-derived:
//
//	draft                         → publish through the shared body, Done
//	already published             → no-op SUCCESS, Done, skipped=true in
//	                                the audit extra
//	soft-deleted                  → StateFailed, reason recorded (the
//	                                read gate answers "not found" for a
//	                                deleted post, and so do we)
//	unpublished again after being
//	scheduled                     → publishes. A schedule is a STANDING
//	                                INSTRUCTION until it is cancelled;
//	                                unpublish is not a cancel, and the
//	                                cancel surface already exists.
//
// The first two rows are the same cell of the table reached two ways —
// two schedules for one post, or an author who publishes manually
// between the schedule and the fire.
//
// ⚠️ TWO TRANSACTIONS, AND THAT IS THE CORRECT SHAPE. The asset arms
// write on `q`, a savepoint of the reaper's claim tx. This one cannot:
// the publication core opens its own transaction, because the state
// move and the federation activity have to commit together and that
// pairing belongs to the core, not to whoever called it. So the outcome
// here is "the post is published, and this row may or may not have been
// marked done yet". A crash in that window leaves the action pending,
// it fires again, and the second fire lands on the already-published
// row of the table above: no-op success. The idempotence is not a
// consolation for the split — it is what makes the split safe.
func (e *executor) changePostState(ctx context.Context, q *Queries, auditQ *audit.Queries, a ScheduledAction, p actionParams) error {
	if e.publisher == nil {
		return fmt.Errorf("change_state: post publication core not wired")
	}
	id, err := uuid.Parse(a.TargetID)
	if err != nil {
		return fmt.Errorf("change_state: target id %q is not a uuid: %w", a.TargetID, err)
	}
	// Store.Schedule refuses a post state change with no created_by, so
	// reaching here without one means a row that predates the guard or
	// was written round it. Fail rather than pick an actor.
	if a.CreatedBy == nil {
		return fmt.Errorf("change_state: a post state change has no created_by to act as")
	}
	publish, code, err := e.postPublicationTarget(ctx, q, p)
	if err != nil {
		return err
	}
	moved, err := e.publisher.MovePostPublication(ctx, id, publish, *a.CreatedBy)
	if err != nil {
		return fmt.Errorf("change_state: %w", err)
	}
	// skipped=true is the same word softDelete uses for its idempotent
	// no-op, so one query over the audit trail answers "did this action
	// change anything" across every arm.
	e.recordExecuted(ctx, auditQ, a, nil, map[string]any{
		"field": "state_id", "new": code, "skipped": !moved,
		"actor_user_ref": *a.CreatedBy,
	})
	return nil
}

// postPublicationTarget resolves the params' requested state within the
// POST workflow domain and reports which direction of the publication
// core it means.
//
// Both params spellings are honoured — `to_state` (a code, ADR 0020's
// own example shape) and `to_state_id` (an explicit uuid) — resolved
// through the same (domain, code) rows the read rule and the endpoints
// name, so there is one identity for "published" across all three.
//
// A post state that is neither is an error rather than a direct write:
// the `post` domain holds exactly the two states publication is made
// of, and an instance that adds a third has not thereby told us what
// moving a post there should say to its peers.
func (e *executor) postPublicationTarget(ctx context.Context, q *Queries, p actionParams) (publish bool, code string, err error) {
	resolve := func(c string) (pgtype.UUID, error) {
		return q.ResolveWorkflowStateByCode(ctx, ResolveWorkflowStateByCodeParams{
			Domain: visibility.PostWorkflowDomain, Code: c,
		})
	}
	switch {
	case p.ToState != "":
		switch p.ToState {
		case visibility.PostPublishedStateCode:
			return true, p.ToState, nil
		case visibility.PostDraftStateCode:
			return false, p.ToState, nil
		}
		if _, rerr := resolve(p.ToState); errors.Is(rerr, pgx.ErrNoRows) {
			return false, "", fmt.Errorf("change_state: no post state with code %q", p.ToState)
		} else if rerr != nil {
			return false, "", fmt.Errorf("change_state: resolve %q: %w", p.ToState, rerr)
		}
		return false, "", fmt.Errorf(
			"change_state: post state %q is not a publication state (%q or %q)",
			p.ToState, visibility.PostPublishedStateCode, visibility.PostDraftStateCode)
	case p.ToStateID != "":
		sid, perr := uuid.Parse(p.ToStateID)
		if perr != nil {
			return false, "", fmt.Errorf("change_state: to_state_id %q is not a uuid: %w", p.ToStateID, perr)
		}
		want := pgtype.UUID{Bytes: sid, Valid: true}
		for _, c := range []string{visibility.PostPublishedStateCode, visibility.PostDraftStateCode} {
			got, rerr := resolve(c)
			if errors.Is(rerr, pgx.ErrNoRows) {
				continue
			}
			if rerr != nil {
				return false, "", fmt.Errorf("change_state: resolve %q: %w", c, rerr)
			}
			if got.Valid && got.Bytes == want.Bytes {
				return c == visibility.PostPublishedStateCode, c, nil
			}
		}
		return false, "", fmt.Errorf(
			"change_state: to_state_id %s is not the post domain's %q or %q state",
			p.ToStateID, visibility.PostPublishedStateCode, visibility.PostDraftStateCode)
	default:
		return false, "", fmt.Errorf("change_state: params need to_state (code) or to_state_id (uuid)")
	}
}

func (e *executor) notify(ctx context.Context, auditQ *audit.Queries, a ScheduledAction, p actionParams) error {
	if e.notifier == nil {
		return fmt.Errorf("notify: notification subsystem not wired")
	}
	recipient, err := notifyRecipient(a, p)
	if err != nil {
		return err
	}
	verb := p.Verb
	if verb == "" {
		verb = "scheduled_action"
	}
	payload := map[string]any{"reason": p.Reason}
	// actor nil: a scheduled notification is system-initiated. Not
	// tx-bound (the notifications writer is pool-bound), which is fine —
	// a notification has no domain state to roll back; it is the whole
	// side effect of this action.
	if err := e.notifier.Notify(ctx, recipient, nil, verb, string(a.TargetKind), a.TargetID, payload); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	subj := recipient
	e.recordExecuted(ctx, auditQ, a, &subj, map[string]any{"verb": verb, "recipient": recipient})
	return nil
}

// notifyRecipient resolves who the notification goes to: an explicit
// params.recipient wins, else a user target.
func notifyRecipient(a ScheduledAction, p actionParams) (int64, error) {
	if p.Recipient != nil {
		return *p.Recipient, nil
	}
	if TargetKind(a.TargetKind) == TargetUser {
		var ref int64
		if _, err := fmt.Sscan(a.TargetID, &ref); err != nil {
			return 0, fmt.Errorf("notify: user target id %q is not a ref: %w", a.TargetID, err)
		}
		return ref, nil
	}
	return 0, fmt.Errorf("notify: no recipient — set params.recipient or target a user")
}

// recordExecuted writes the tx-bound success audit row for one action.
// Called at the end of every executor; the sabotage test removes it
// from one and asserts the audit row disappears. subject is the user
// the event is about (the notify recipient) or nil for asset-plane
// actions that are about a thing, not a person.
func (e *executor) recordExecuted(ctx context.Context, auditQ *audit.Queries, a ScheduledAction, subject *int64, extra map[string]any) {
	meta := map[string]any{
		"scheduled_action_id": uuidText(a.ID),
		"action":              a.Action,
		"target_kind":         a.TargetKind,
		"target_id":           a.TargetID,
	}
	for k, v := range extra {
		meta[k] = v
	}
	e.rec.WriteInTx(ctx, auditQ, audit.EventScheduledActionExecuted, subject, nil, meta)
}

func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
