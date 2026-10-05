// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package scheduledactions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// The author seam (#1119 sprint 21e)
// ---------------------------------------------------------------------------
//
// An author scheduling their own post's publication is the SAME
// instruction #1238 gave operators: a `change_state` action on a post
// target, params `{"to_state": "published"}`, fired by the reaper
// through the Publisher (the posts publication core), with `created_by`
// naming who the publication is attributed to. Nothing about how the
// row executes is different, and nothing here touches execution.
//
// What is different is who may WRITE the row and what they may see of
// it afterwards. The generic surface in http.go is `system.admin` (ADR
// 0020's own cap ruling), and it stays so. This file is a second,
// narrower door beside it:
//
//   - authority comes from the post, not from a capability on this
//     table. PostAuthority is the publication core's own answer to
//     "may this caller schedule this post now", and it applies the
//     endpoints' 404-before-403 ordering, authorship, the instance's
//     `posts.publish` policy and the post's current state. This
//     package asks; it does not re-derive.
//   - `created_by` is the requesting caller. Never an admin surrogate,
//     never a service identity: the row publishes AS this user and
//     federates in their name, and the identity it publishes as is
//     loaded again when it fires (posts.MovePostPublication), so a
//     revoked capability or a deleted post stops it then.
//   - the rows this seam wrote are the only rows it can read or
//     cancel. They are marked origin = 'author' (migration 00069), and
//     every query below is scoped to that origin and to the caller, so
//     an operator's scheduled takedown of the same post is invisible
//     here and an author cannot cancel it.
//   - ONE pending author schedule per post, held by the database. A
//     PUT that finds the caller's own pending row replaces it in the
//     same transaction; two PUTs racing are serialised by the partial
//     unique index, and the loser is told to re-read.
//
// The timing contract is the reaper's (ReapInterval, ~5 minutes) and
// is not restated as a tighter promise anywhere on this surface.

// PostAuthority is the publication core's schedule-time gate, wired at
// boot from posts.Handler. Declared here rather than imported so this
// package keeps the dependency direction it already has: it reaches
// posts only through interfaces, because post_publish_test.go (an
// in-package test that builds the real posts handler) would otherwise
// be an import cycle. The answer is a plain (status, message) pair
// rather than a struct for the same reason: posts.Handler satisfies
// this with no adapter, and there is no second type to keep in step.
type PostAuthority interface {
	// PublicationScheduleGate answers whether caller may act on the
	// author publication schedule of postID: forCreate=true for making
	// one (all gates), false for reading or cancelling one (read gate
	// and authorship only, so a revoked capability does not strand a
	// standing instruction its author can no longer withdraw). status
	// is 200 to proceed, else the HTTP refusal and its prose.
	PublicationScheduleGate(ctx context.Context, caller *auth.Identity, postID uuid.UUID, forCreate bool) (status int, message string, err error)
}

// authorityVerdict is the gate's answer, carried to the renderers.
type authorityVerdict struct {
	Status  int
	Message string
}

// ErrAuthorScheduleConflict is returned by ScheduleAuthorPostPublication
// when another pending author schedule for the post won the insert
// race. The caller maps it to 409.
var ErrAuthorScheduleConflict = errors.New("scheduledactions: a pending publication schedule for this post already exists")

// ErrAuthorScheduleNotFuture is returned when scheduled_for is not
// strictly after the database's clock. The caller maps it to 400.
var ErrAuthorScheduleNotFuture = errors.New("scheduledactions: scheduled_for must be in the future")

// AuthorPublicationInput is one author's instruction.
type AuthorPublicationInput struct {
	PostID       uuid.UUID
	ScheduledFor time.Time
	// CreatedBy is the requesting caller's user ref. Zero is refused:
	// the row publishes as this user.
	CreatedBy int64
}

// ScheduleAuthorPostPublication records the caller's standing
// instruction to publish PostID at ScheduledFor, replacing the caller's
// own pending one if there is one.
//
// The whole thing is ONE transaction: cancel-own, then insert. Under
// READ COMMITTED a concurrent transaction's uncommitted insert is
// invisible to the cancel, so two racers both cancel nothing and both
// insert; the unique index scheduled_actions_author_pending_post_idx
// then blocks the second until the first commits and raises 23505,
// which becomes ErrAuthorScheduleConflict. That is the race-safety: it
// lives in the index, not in a read that could be stale by the time it
// is acted on.
//
// Future-only is decided against NOW() in the same transaction rather
// than against the Go clock, so the refusal and the row agree about
// what "now" was. This is an AUTHOR-surface rule: Store.Schedule keeps
// accepting a past time for admin and system callers, whose due-now
// action is a legitimate instruction (see reaper_test).
func (s *Store) ScheduleAuthorPostPublication(ctx context.Context, in AuthorPublicationInput) (ScheduledAction, error) {
	if in.CreatedBy == 0 {
		return ScheduledAction{}, errors.New("scheduledactions: an author schedule needs the requesting user")
	}
	if in.PostID == uuid.Nil {
		return ScheduledAction{}, errors.New("scheduledactions: post id required")
	}
	if in.ScheduledFor.IsZero() {
		return ScheduledAction{}, ErrAuthorScheduleNotFuture
	}
	params, err := json.Marshal(map[string]any{"to_state": visibility.PostPublishedStateCode})
	if err != nil {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: marshal params: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var future bool
	if err := tx.QueryRow(ctx, `SELECT $1::timestamptz > NOW()`, in.ScheduledFor).Scan(&future); err != nil {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: clock: %w", err)
	}
	if !future {
		return ScheduledAction{}, ErrAuthorScheduleNotFuture
	}

	q := New(tx)
	createdBy := in.CreatedBy
	if _, err := q.CancelPendingAuthorPostPublication(ctx, CancelPendingAuthorPostPublicationParams{
		TargetID: in.PostID.String(), CreatedBy: &createdBy,
	}); err != nil {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: replace pending author schedule: %w", err)
	}
	row, err := q.CreateAuthorPostPublication(ctx, CreateAuthorPostPublicationParams{
		TargetID:     in.PostID.String(),
		Params:       params,
		ScheduledFor: pgtype.Timestamptz{Time: in.ScheduledFor, Valid: true},
		CreatedBy:    &createdBy,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "scheduled_actions_author_pending_post_idx" {
			return ScheduledAction{}, ErrAuthorScheduleConflict
		}
		return ScheduledAction{}, fmt.Errorf("scheduledactions: create author schedule: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ScheduledAction{}, fmt.Errorf("scheduledactions: commit: %w", err)
	}
	return row, nil
}

// PendingAuthorPostPublication returns the caller's pending author
// schedule for the post, or found=false when there is none.
func (s *Store) PendingAuthorPostPublication(ctx context.Context, postID uuid.UUID, createdBy int64) (ScheduledAction, bool, error) {
	row, err := s.q.GetPendingAuthorPostPublication(ctx, GetPendingAuthorPostPublicationParams{
		TargetID: postID.String(), CreatedBy: &createdBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ScheduledAction{}, false, nil
	}
	if err != nil {
		return ScheduledAction{}, false, fmt.Errorf("scheduledactions: pending author schedule: %w", err)
	}
	return row, true, nil
}

// CancelAuthorPostPublication cancels the caller's own pending author
// schedule for the post. found=false means nothing was pending. It
// cannot reach a row with any other origin or creator: the statement
// is scoped, not the caller's good manners.
func (s *Store) CancelAuthorPostPublication(ctx context.Context, postID uuid.UUID, createdBy int64) (ScheduledAction, bool, error) {
	rows, err := s.q.CancelPendingAuthorPostPublication(ctx, CancelPendingAuthorPostPublicationParams{
		TargetID: postID.String(), CreatedBy: &createdBy,
	})
	if err != nil {
		return ScheduledAction{}, false, fmt.Errorf("scheduledactions: cancel author schedule: %w", err)
	}
	if len(rows) == 0 {
		return ScheduledAction{}, false, nil
	}
	return rows[0], true, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// SetPostAuthority installs the publication core's schedule-time gate.
// Post-construction setter so boot order stays linear; nil means the
// author surface refuses rather than guessing at authority.
func (h *HTTPHandler) SetPostAuthority(a PostAuthority) { h.postAuthority = a }

// authorGate resolves the request identity and runs the post gate. The
// three handlers differ only in what they do when the verdict is 200.
func (h *HTTPHandler) authorGate(ctx context.Context, postID uuid.UUID, forCreate bool) (*auth.Identity, authorityVerdict, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() {
		return nil, authorityVerdict{Status: 401, Message: "authentication required"}, nil
	}
	if h.postAuthority == nil {
		return nil, authorityVerdict{}, errors.New("scheduledactions: post authority not wired")
	}
	status, msg, err := h.postAuthority.PublicationScheduleGate(ctx, caller, postID, forCreate)
	if err != nil {
		return nil, authorityVerdict{}, err
	}
	return caller, authorityVerdict{Status: status, Message: msg}, nil
}

// GetPostPublicationSchedule is GET /posts/{id}/publication-schedule.
func (h *HTTPHandler) GetPostPublicationSchedule(
	ctx context.Context,
	req openapi.GetPostPublicationScheduleRequestObject,
) (openapi.GetPostPublicationScheduleResponseObject, error) {
	postID := uuid.UUID(req.Id)
	caller, v, err := h.authorGate(ctx, postID, false)
	if err != nil {
		return nil, err
	}
	switch v.Status {
	case 401:
		return openapi.GetPostPublicationSchedule401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: v.Message},
		}, nil
	case 403:
		return openapi.GetPostPublicationSchedule403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: v.Message},
		}, nil
	case 404:
		return openapi.GetPostPublicationSchedule404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: v.Message},
		}, nil
	}
	row, found, err := h.store.PendingAuthorPostPublication(ctx, postID, caller.UserRef)
	if err != nil {
		return nil, err
	}
	out := openapi.PostPublicationScheduleStatus{}
	if found {
		s := toAuthorAPI(row)
		out.Schedule = &s
	}
	return openapi.GetPostPublicationSchedule200JSONResponse(out), nil
}

// SetPostPublicationSchedule is PUT /posts/{id}/publication-schedule.
func (h *HTTPHandler) SetPostPublicationSchedule(
	ctx context.Context,
	req openapi.SetPostPublicationScheduleRequestObject,
) (openapi.SetPostPublicationScheduleResponseObject, error) {
	postID := uuid.UUID(req.Id)
	caller, v, err := h.authorGate(ctx, postID, true)
	if err != nil {
		return nil, err
	}
	switch v.Status {
	case 401:
		return openapi.SetPostPublicationSchedule401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: v.Message},
		}, nil
	case 403:
		return openapi.SetPostPublicationSchedule403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: v.Message},
		}, nil
	case 404:
		return openapi.SetPostPublicationSchedule404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: v.Message},
		}, nil
	case 409:
		return openapi.SetPostPublicationSchedule409JSONResponse{Error: v.Message}, nil
	}
	if req.Body == nil || req.Body.ScheduledFor.IsZero() {
		return openapi.SetPostPublicationSchedule400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "scheduled_for is required"},
		}, nil
	}
	row, err := h.store.ScheduleAuthorPostPublication(ctx, AuthorPublicationInput{
		PostID: postID, ScheduledFor: req.Body.ScheduledFor, CreatedBy: caller.UserRef,
	})
	switch {
	case errors.Is(err, ErrAuthorScheduleNotFuture):
		return openapi.SetPostPublicationSchedule400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "scheduled_for must be in the future"},
		}, nil
	case errors.Is(err, ErrAuthorScheduleConflict):
		return openapi.SetPostPublicationSchedule409JSONResponse{
			Error: "a publication schedule for this post was just created; reload to see it",
		}, nil
	case err != nil:
		return nil, err
	}
	return openapi.SetPostPublicationSchedule200JSONResponse(toAuthorAPI(row)), nil
}

// CancelPostPublicationSchedule is DELETE /posts/{id}/publication-schedule.
func (h *HTTPHandler) CancelPostPublicationSchedule(
	ctx context.Context,
	req openapi.CancelPostPublicationScheduleRequestObject,
) (openapi.CancelPostPublicationScheduleResponseObject, error) {
	postID := uuid.UUID(req.Id)
	caller, v, err := h.authorGate(ctx, postID, false)
	if err != nil {
		return nil, err
	}
	switch v.Status {
	case 401:
		return openapi.CancelPostPublicationSchedule401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: v.Message},
		}, nil
	case 403:
		return openapi.CancelPostPublicationSchedule403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: v.Message},
		}, nil
	case 404:
		return openapi.CancelPostPublicationSchedule404JSONResponse{Error: v.Message}, nil
	}
	row, found, err := h.store.CancelAuthorPostPublication(ctx, postID, caller.UserRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return openapi.CancelPostPublicationSchedule404JSONResponse{
			Error: "this post has no pending publication schedule",
		}, nil
	}
	return openapi.CancelPostPublicationSchedule200JSONResponse(toAuthorAPI(row)), nil
}

// toAuthorAPI renders the narrow author view of a row. The generic
// fields are fixed on this surface and are not repeated.
func toAuthorAPI(r ScheduledAction) openapi.PostPublicationSchedule {
	out := openapi.PostPublicationSchedule{
		Id:           uuid.UUID(r.ID.Bytes),
		ScheduledFor: r.ScheduledFor.Time,
		State:        openapi.PostPublicationScheduleState(r.State),
		CreatedAt:    r.CreatedAt.Time,
	}
	if pid, err := uuid.Parse(r.TargetID); err == nil {
		out.PostId = pid
	}
	if r.CreatedBy != nil {
		out.CreatedBy = *r.CreatedBy
	}
	return out
}
