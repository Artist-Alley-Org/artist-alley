// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/users"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
	"github.com/mscrnt/artist-alley/app/internal/workflow"
)

// ---------------------------------------------------------------------------
// Publication — the deliberate act (ADR 0091, #1161)
// ---------------------------------------------------------------------------
//
// A post is the unit of publication, and until v0.11.0 it had no way to
// be anything else: `published` is the `post` domain's initial state,
// every seeded post was in it, `wip` held zero rows, and no product code
// mentioned either. Publication was therefore not an act — it was a
// side effect of the upload finishing.
//
// This file is the act. Two endpoints, both of which go through the
// workflow state machine rather than writing `state_id` themselves:
//
//   - PublishPost   wip → published
//   - UnpublishPost published → wip
//
// # Why the state machine and not an UPDATE
//
// The edges already existed (migration 00001) and had never been
// traversed, because workflow.Service.Transition had no callers at all.
// Going through it buys three things a direct UPDATE would have had to
// re-invent, badly:
//
//   - the EDGE LIST decides which moves exist, so "publish an already
//     published post" is refused by data rather than by an if;
//   - `posts.publish` gates the move as INSTANCE POLICY — revoke it
//     from the Base role and publication becomes an approval step
//     across the whole install, with no code change;
//   - every move lands a `workflow_audit` row. Who unpublished this,
//     and when, is a question a moderated instance will be asked.
//
// # Two gates, deliberately different
//
// The capability answers "may this caller publish AT ALL". Authorship
// answers "may this caller publish THIS post", and the two endpoints
// answer it differently on purpose:
//
//   - PUBLISH widens who can reach the post, so it takes the narrow
//     gate PATCH's `visibility` field takes — author, GLOBAL
//     posts.admin, or system.admin (canWidenPostAccess, #930). A
//     team-scoped posts.admin holder may edit a colleague's post and
//     may not publish it, exactly as they may not re-tier it.
//   - UNPUBLISH narrows it, so it takes the ordinary mutation gate
//     (canMutatePost). "A management capability that can tidy up but
//     not hand out is the right asymmetry" is already this package's
//     rule for ACLs; taking a post down is tidying up.
//
// # What does NOT change
//
// Unpublishing keeps the post whole: title, description, tags, members,
// cover, visibility tier, likes, comments. ADR 0091 decision 6 is
// explicit that this is the point — "I want this off the site for now"
// must not be a destructive act — and it is why the only column either
// endpoint writes is `state_id`. Member assets are untouched either
// way: storage and publication are separate lifecycles.
//
// # A third entry, and no third implementation (#1238)
//
// Scheduled publication needs this body from a background job that has
// no request and therefore no context identity. MovePostPublication is
// that entry: it loads the actor named on the scheduled row and hands
// it to movePublicationAs, the same function the endpoints reach after
// reading their caller off the context.
//
// It is an ENTRY, not a bypass. The read gate, the 404-vs-403 ordering,
// the authorship gate and the `posts.publish` capability all apply to
// it exactly as they apply to a request — and they are evaluated when
// the action FIRES, so an operator who revokes publication rights has
// stopped tomorrow's scheduled publish as well as today's button.
//
// The alternative was an UPDATE in the reaper's own queries. That is
// the defect this arrangement exists to make impossible: it would move
// `state_id` without the activity beside it, and the post would be
// published on this instance and on no other, silently.

// publicationTargets holds the two state ids this file moves between,
// resolved from the database on each call. Not cached: see
// GetWorkflowStateIDByCode in queries.sql for why a stale UUID here
// would make every post look like a draft.
type publicationTargets struct {
	published pgtype.UUID
	draft     pgtype.UUID
}

// postStateID resolves one of the `post` domain's states by code. The
// codes come from the visibility package, which is where the READ rule
// names the same rows — one spelling of the identity, so the writer and
// the gate cannot come to mean different states.
func (h *Handler) postStateID(ctx context.Context, code string) (pgtype.UUID, error) {
	id, err := New(h.Pool).GetWorkflowStateIDByCode(ctx, GetWorkflowStateIDByCodeParams{
		Domain: visibility.PostWorkflowDomain,
		Code:   code,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("posts: resolve %q state: %w", code, err)
	}
	return id, nil
}

// publicationStates resolves both.
func (h *Handler) publicationStates(ctx context.Context) (publicationTargets, error) {
	pub, err := h.postStateID(ctx, visibility.PostPublishedStateCode)
	if err != nil {
		return publicationTargets{}, err
	}
	dr, err := h.postStateID(ctx, visibility.PostDraftStateCode)
	if err != nil {
		return publicationTargets{}, err
	}
	return publicationTargets{published: pub, draft: dr}, nil
}

// createStateID is the state a brand-new post is born in — `wip` for a
// draft, the domain's INITIAL state otherwise.
//
// The published side asks for the initial state rather than for
// `published` by name, because "where does a new resource start" is a
// question the workflow data already answers (`is_initial`, one row per
// domain, enforced by a partial unique index) and an install that moved
// its entry point should be obeyed rather than overridden. The draft
// side names `wip` because that IS the draft state — ADR 0091's second
// amendment identifies the two — and no initial-state configuration
// makes "start as a draft" mean anything else.
func (h *Handler) createStateID(ctx context.Context, draft bool) (pgtype.UUID, error) {
	if draft {
		return h.postStateID(ctx, visibility.PostDraftStateCode)
	}
	var out pgtype.UUID
	row, err := New(h.Pool).GetPostInitialStateID(ctx, visibility.PostWorkflowDomain)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A domain with no initial state is a setup gap, not a
			// runtime condition — but answering with NULL would create
			// a post the fail-closed read rule then hides from
			// everybody including its author. Fall back to the state
			// this ADR names, so a mis-seeded install publishes rather
			// than swallows.
			return h.postStateID(ctx, visibility.PostPublishedStateCode)
		}
		return out, fmt.Errorf("posts: resolve initial post state: %w", err)
	}
	return row, nil
}

// isDraftState reports whether a post's `state_id` means "not
// published". The complement of visibility.postPublishedExpr, and it
// must stay the complement: the SQL rule asks `state_id = published`,
// so anything else — `wip`, NULL, a state a later domain edit adds — is
// unpublished on both sides. Both name the published row through the
// same (domain, code) key, so there is one identity even though there
// are two evaluators.
func isDraftState(stateID, publishedID pgtype.UUID) bool {
	if !publishedID.Valid {
		// The published state row is missing — a broken install. The
		// read rule hides every post in this situation; saying "draft"
		// here at least makes the API agree with what the reader sees.
		return true
	}
	return !stateID.Valid || stateID.Bytes != publishedID.Bytes
}

// ---------------------------------------------------------------------------
// PublishPost
// ---------------------------------------------------------------------------

func (h *Handler) PublishPost(
	ctx context.Context,
	req openapi.PublishPostRequestObject,
) (openapi.PublishPostResponseObject, error) {
	res, err := h.movePublication(ctx, uuid.UUID(req.Id), true)
	if err != nil {
		return nil, err
	}
	switch res.status {
	case 401:
		return openapi.PublishPost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: res.message},
		}, nil
	case 403:
		return openapi.PublishPost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: res.message},
		}, nil
	case 404:
		return openapi.PublishPost404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: res.message},
		}, nil
	case 409:
		return openapi.PublishPost409JSONResponse{Error: res.message}, nil
	}
	return openapi.PublishPost200JSONResponse(*res.post), nil
}

// ---------------------------------------------------------------------------
// UnpublishPost
// ---------------------------------------------------------------------------

func (h *Handler) UnpublishPost(
	ctx context.Context,
	req openapi.UnpublishPostRequestObject,
) (openapi.UnpublishPostResponseObject, error) {
	res, err := h.movePublication(ctx, uuid.UUID(req.Id), false)
	if err != nil {
		return nil, err
	}
	switch res.status {
	case 401:
		return openapi.UnpublishPost401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: res.message},
		}, nil
	case 403:
		return openapi.UnpublishPost403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: res.message},
		}, nil
	case 404:
		return openapi.UnpublishPost404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: res.message},
		}, nil
	case 409:
		return openapi.UnpublishPost409JSONResponse{Error: res.message}, nil
	}
	return openapi.UnpublishPost200JSONResponse(*res.post), nil
}

// publicationResult is the shared outcome of the two endpoints, mapped
// to each operation's own response union by the wrappers above. One
// body, because publish and unpublish differ in three places — the
// target state, the authorship gate and the activity they emit — and
// everything else about them is identical. Two copies would have been
// two places for the 404-vs-403 ordering to drift, and that ordering is
// what keeps these endpoints from being post-existence probes.
type publicationResult struct {
	status  int
	message string
	post    *openapi.Post

	// noop distinguishes the TWO refusals that both answer 409: "the
	// post is already in the state you asked for" (this flag) and "this
	// instance's edge list has no such move" (not). On the wire they are
	// the same status with different prose, which is right for a caller
	// who asked for a move; a STANDING instruction — a scheduled
	// publication (#1238) — has to tell them apart, because finding the
	// world already in the state it was told to reach is success and a
	// missing edge is not.
	noop bool
}

// movePublication is the HTTP entry: resolve the request's identity,
// run the shared body, and — only on success — render the post for the
// caller who asked.
//
// The split below is #1238's seam and nothing more. Everything that
// decides an OUTCOME (the read gate, the 404-vs-403 ordering, the
// authorship gates, the no-op refusal, the transaction) lives in
// movePublicationAs, unchanged; what stays here is the two things that
// only make sense for an HTTP request — reading the caller off the
// context, and hydrating a response body for them.
func (h *Handler) movePublication(
	ctx context.Context,
	postID uuid.UUID,
	publish bool,
) (publicationResult, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() {
		return publicationResult{status: 401, message: "authentication required"}, nil
	}
	res, err := h.movePublicationAs(ctx, caller, postID, publish)
	if err != nil || res.status != 200 {
		return res, err
	}
	pgID := pgtype.UUID{Bytes: postID, Valid: true}
	full, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		return publicationResult{}, err
	}
	if err := h.enrichForCaller(ctx, full); err != nil {
		return publicationResult{}, err
	}
	res.post = full
	return res, nil
}

// MovePostPublication is the publication core's non-HTTP entry point:
// publish (or unpublish) a post on behalf of actorUserRef, with no
// request and no context identity behind it. Today's one caller is the
// scheduled-action reaper (#1238); the arm exists so a scheduled
// publication goes through THIS body rather than writing `state_id`
// itself, which would move the post without telling any peer about it.
//
// The gates are not relaxed for it. The actor is loaded through the
// real resolver, so its capabilities are the ones the database says it
// has at FIRE time — an operator who revoked `posts.publish` between
// the schedule and the fire has stopped the publication, which is the
// whole point of that capability being instance policy.
//
// Returns moved=false with a nil error for the idempotent no-op: the
// post is already in the state the instruction asked for. That is
// SUCCESS for a standing instruction (see publicationResult.noop), and
// it is the cell of #1238's fire-time table that two schedules for the
// same post, and an author who publishes manually first, both land in.
func (h *Handler) MovePostPublication(
	ctx context.Context,
	postID uuid.UUID,
	publish bool,
	actorUserRef int64,
) (bool, error) {
	if h.actorLoader == nil {
		// Refuse rather than synthesise an identity. A hand-built actor
		// would carry no capabilities and no username, which would fail
		// the workflow gate and mint a federation actor URI ending in
		// "/users/" — a fabricated caller is worse than an outage.
		return false, errors.New("posts: actor loader not wired")
	}
	if actorUserRef == 0 {
		return false, errors.New("posts: publication needs an actor to attribute")
	}
	actor := h.actorLoader.LoadIdentity(ctx, actorUserRef)
	if actor == nil || actor.IsAnonymous() || actor.Username == "" {
		return false, fmt.Errorf("posts: no usable identity for actor %d", actorUserRef)
	}
	res, err := h.movePublicationAs(ctx, actor, postID, publish)
	if err != nil {
		return false, err
	}
	switch {
	case res.status == 200:
		return true, nil
	case res.noop:
		return false, nil
	default:
		return false, fmt.Errorf("posts: publication refused (%d): %s", res.status, res.message)
	}
}

// movePublicationAs is the body both entries share: every gate, the
// transaction, and the invalidations. The caller is passed EXPLICITLY
// rather than read off the context, which is the only reason a reaper
// with no HTTP identity can reach it.
func (h *Handler) movePublicationAs(
	ctx context.Context,
	caller *auth.Identity,
	postID uuid.UUID,
	publish bool,
) (publicationResult, error) {
	if h.workflow == nil {
		// Refuse rather than fall back to a direct state write. A
		// fallback would skip the capability gate and the audit row,
		// which is the entire reason this goes through the machine.
		return publicationResult{}, errors.New("posts: workflow service not wired")
	}
	pgID := pgtype.UUID{Bytes: postID, Valid: true}

	// READ GATE FIRST, and the order is load-bearing. A caller who may
	// not read the post gets the same 404 a nonexistent id gets, so
	// neither endpoint can be used to ask which post UUIDs exist —
	// the same discipline GetPost and the cover gates already keep.
	// Only after the post is established as readable does a refusal
	// become a 403, which tells the caller something they already knew.
	//
	// The gate admits a DRAFT to its author (visibility.PostReadable
	// passes IncludeDrafts), which is what makes publishing one
	// possible at all.
	readable, err := h.postReadable(ctx, caller, postID)
	if err != nil {
		return publicationResult{}, fmt.Errorf("posts: publication read gate: %w", err)
	}
	if !readable {
		return publicationResult{status: 404, message: "post not found"}, nil
	}

	cur, err := New(h.Pool).GetPost(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return publicationResult{status: 404, message: "post not found"}, nil
		}
		return publicationResult{}, fmt.Errorf("posts: publication load: %w", err)
	}

	if publish {
		// Widening. Narrow gate — see the file header.
		if !canWidenPostAccess(caller, cur.AuthorUserRef) {
			return publicationResult{
				status:  403,
				message: "publishing a post is reserved to its author",
			}, nil
		}
	} else if !canMutatePost(caller, cur.AuthorUserRef, cur.TeamID) {
		return publicationResult{status: 403, message: "not the post author"}, nil
	}

	states, err := h.publicationStates(ctx)
	if err != nil {
		return publicationResult{}, err
	}
	target := states.draft
	if publish {
		target = states.published
	}
	// Refuse the no-op BEFORE the state machine does. The edge list
	// would refuse published → published anyway (there is no such row),
	// but only with ErrTransitionNotAllowed, which cannot distinguish
	// "already there" from "that move does not exist" — and the first
	// deserves a message that says so.
	if isDraftState(cur.StateID, states.published) == !publish {
		msg := "post is already published"
		if !publish {
			msg = "post is not published"
		}
		return publicationResult{status: 409, message: msg, noop: true}, nil
	}

	// The state move and the federation activity in ONE transaction
	// (ADR 0044). Publishing is the moment the post enters the world,
	// so it is the moment peers are told; unpublishing withdraws it,
	// so peers are told to drop their copy — a `Delete`, because that
	// is the vocabulary a peer already acts on, and re-publishing emits
	// a fresh `Create`. Emitting nothing on the way down would leave a
	// remote instance serving a post its author took off this one.
	actorCtx := emit.ActorContext{
		UserRef:  caller.UserRef,
		Username: caller.Username,
		BaseURL:  h.baseURLFn(ctx),
	}
	var em emit.Emission
	if publish {
		em = emit.CreatePost(actorCtx, emit.PostRef{
			ID:            postID.String(),
			Title:         cur.Title,
			AuthorUserRef: cur.AuthorUserRef,
			AuthorURI:     actorCtx.URI(),
		}, emit.PostVisibility(cur.Visibility))
	} else {
		em = emit.DeletePost(actorCtx, postID.String(), cur.Title)
	}

	err = h.activities.WithEmission(ctx, activities.EmissionInput{
		Activity: em.Activity,
	}, func(tx pgx.Tx) error {
		return h.workflow.TransitionInTx(ctx, tx, workflow.KindPost, postID,
			uuid.UUID(target.Bytes), caller, "")
	})
	switch {
	case err == nil:
	case errors.Is(err, workflow.ErrTransitionNotAllowed):
		// The 409 above catches the two no-ops this API can produce, so
		// reaching here means the instance's own edge list has been
		// edited to remove the move. Say which move, not "bad request".
		return publicationResult{
			status:  409,
			message: "this instance's post workflow does not allow that transition",
		}, nil
	case errors.Is(err, workflow.ErrInsufficientCapability):
		// INSTANCE POLICY, not authorship — authorship was settled
		// above. An operator has taken `posts.publish` away from this
		// caller's roles, which is the supported way to make
		// publication an approval step.
		return publicationResult{
			status:  403,
			message: "publishing requires the posts.publish capability",
		}, nil
	case errors.Is(err, workflow.ErrResourceNotFound):
		return publicationResult{status: 404, message: "post not found"}, nil
	default:
		return publicationResult{}, fmt.Errorf("posts: publication transition: %w", err)
	}

	h.cacheInvalidate(ctx, pgID)
	// The author's cached profile carries a post_count that counts only
	// what a reader may see, so a post entering or leaving the shared
	// surfaces moves it. Both entries need this, which is why it is here
	// and not in the HTTP wrapper: a scheduled publication that left a
	// stale count behind would be a post on the site the profile denies.
	users.InvalidateProfile(ctx, h.registry, cur.AuthorUserRef)

	return publicationResult{status: 200}, nil
}
