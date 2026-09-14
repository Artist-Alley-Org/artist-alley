// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1119 sprint 21e: an author schedules their OWN post through the
// existing engine.
//
// # What is driven
//
// The REAL author HTTP handlers (author.go) over the REAL posts handler
// as the authority, the REAL resolver for identities, and the REAL
// reaper for fire time: the same fixture post_publish_test.go built for
// #1238, which is the point. A scheduled publication made by an author
// is #1238's row reached through a narrower door, and the tests here
// are about the door: who may open it, what they may see through it,
// and what the database holds afterwards.
//
// # What is asserted
//
// Persisted rows, never the response echo. `created_by` is read back
// from scheduled_actions; the post's state from posts; the federation
// half from activities. A handler that answered 200 and wrote the
// wrong creator, or no row, passes a body assertion and fails these.
//
// # The headline regression is elsewhere
//
// Every test in this file names the new handlers, so none of them can
// compile against the baseline, which makes none of them the
// red-before proof. That proof is the standalone Playwright spec
// (post-schedule-1119.spec.ts), which PUTs the route over the wire
// and reads the answer: 404 on the baseline, 200 and a pending row
// after.
//
// Skips without AA_DB_PASSWORD.

package scheduledactions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// authorFixture is spFixture plus the author HTTP surface.
type authorFixture struct {
	*spFixture
	h *HTTPHandler
}

func newAuthorFixture(t *testing.T) *authorFixture {
	t.Helper()
	f := newSPFixture(t)
	h := NewHTTPHandler(f.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.SetPostAuthority(f.posts)
	return &authorFixture{spFixture: f, h: h}
}

// as returns a context carrying userRef's REAL identity, loaded through
// the resolver so its capabilities are the database's answer now.
func (f *authorFixture) as(userRef int64) context.Context {
	f.t.Helper()
	id := (&auth.Resolver{Pool: f.pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).
		LoadIdentity(context.Background(), userRef)
	if id == nil {
		f.t.Fatalf("no identity for %d", userRef)
	}
	return auth.WithIdentity(context.Background(), id)
}

// put drives PUT /posts/{id}/publication-schedule and returns the
// response object for the caller to type-switch on.
func (f *authorFixture) put(ctx context.Context, post uuid.UUID, when time.Time) openapi.SetPostPublicationScheduleResponseObject {
	f.t.Helper()
	resp, err := f.h.SetPostPublicationSchedule(ctx, openapi.SetPostPublicationScheduleRequestObject{
		Id:   openapi_types.UUID(post),
		Body: &openapi.SetPostPublicationScheduleJSONRequestBody{ScheduledFor: when},
	})
	if err != nil {
		f.t.Fatalf("PUT publication-schedule: %v", err)
	}
	return resp
}

// putOK is put expecting 200, registering the row's audit cleanup.
func (f *authorFixture) putOK(ctx context.Context, post uuid.UUID, when time.Time) openapi.PostPublicationSchedule {
	f.t.Helper()
	resp := f.put(ctx, post, when)
	ok, is := resp.(openapi.SetPostPublicationSchedule200JSONResponse)
	if !is {
		f.t.Fatalf("PUT returned %T (%s), want 200", resp, describe(resp))
	}
	id := uuid.UUID(ok.Id)
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM audit_events WHERE metadata->>'scheduled_action_id' = $1`, id.String())
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM scheduled_actions WHERE id = $1`, id)
	})
	return openapi.PostPublicationSchedule(ok)
}

func (f *authorFixture) get(ctx context.Context, post uuid.UUID) openapi.GetPostPublicationScheduleResponseObject {
	f.t.Helper()
	resp, err := f.h.GetPostPublicationSchedule(ctx, openapi.GetPostPublicationScheduleRequestObject{
		Id: openapi_types.UUID(post),
	})
	if err != nil {
		f.t.Fatalf("GET publication-schedule: %v", err)
	}
	return resp
}

// status is get expecting 200, returning the schedule or nil.
func (f *authorFixture) status(ctx context.Context, post uuid.UUID) *openapi.PostPublicationSchedule {
	f.t.Helper()
	resp := f.get(ctx, post)
	ok, is := resp.(openapi.GetPostPublicationSchedule200JSONResponse)
	if !is {
		f.t.Fatalf("GET returned %T (%s), want 200", resp, describe(resp))
	}
	return ok.Schedule
}

func (f *authorFixture) cancel(ctx context.Context, post uuid.UUID) openapi.CancelPostPublicationScheduleResponseObject {
	f.t.Helper()
	resp, err := f.h.CancelPostPublicationSchedule(ctx, openapi.CancelPostPublicationScheduleRequestObject{
		Id: openapi_types.UUID(post),
	})
	if err != nil {
		f.t.Fatalf("DELETE publication-schedule: %v", err)
	}
	return resp
}

// describe renders a response for a failure message: the error prose
// where there is one, so a wrong status says why.
func describe(resp any) string {
	switch r := resp.(type) {
	case openapi.SetPostPublicationSchedule400JSONResponse:
		return r.Error
	case openapi.SetPostPublicationSchedule401JSONResponse:
		return r.Error
	case openapi.SetPostPublicationSchedule403JSONResponse:
		return r.Error
	case openapi.SetPostPublicationSchedule404JSONResponse:
		return r.Error
	case openapi.SetPostPublicationSchedule409JSONResponse:
		return r.Error
	case openapi.GetPostPublicationSchedule401JSONResponse:
		return r.Error
	case openapi.GetPostPublicationSchedule403JSONResponse:
		return r.Error
	case openapi.GetPostPublicationSchedule404JSONResponse:
		return r.Error
	case openapi.CancelPostPublicationSchedule401JSONResponse:
		return r.Error
	case openapi.CancelPostPublicationSchedule403JSONResponse:
		return r.Error
	case openapi.CancelPostPublicationSchedule404JSONResponse:
		return r.Error
	}
	return ""
}

// persisted is the scheduled_actions row as the DATABASE has it.
type persisted struct {
	ID           uuid.UUID
	Action       string
	TargetKind   string
	TargetID     string
	ToState      string
	State        string
	Origin       string
	CreatedBy    *int64
	ScheduledFor time.Time
}

// rowsFor reads every scheduled_actions row targeting the post, oldest
// first. Every cardinality assertion in this file goes through here.
func (f *authorFixture) rowsFor(post uuid.UUID) []persisted {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT id, action, target_kind, target_id, COALESCE(params->>'to_state', ''),
		        state, origin, created_by, scheduled_for
		   FROM scheduled_actions WHERE target_id = $1 ORDER BY created_at ASC, id ASC`, post.String())
	if err != nil {
		f.t.Fatalf("read rows: %v", err)
	}
	defer rows.Close()
	var out []persisted
	for rows.Next() {
		var p persisted
		if err := rows.Scan(&p.ID, &p.Action, &p.TargetKind, &p.TargetID, &p.ToState,
			&p.State, &p.Origin, &p.CreatedBy, &p.ScheduledFor); err != nil {
			f.t.Fatalf("scan row: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// pendingFor returns the pending rows for a post by origin.
func (f *authorFixture) pendingFor(post uuid.UUID, origin string) []persisted {
	f.t.Helper()
	var out []persisted
	for _, r := range f.rowsFor(post) {
		if r.State == StatePending && r.Origin == origin {
			out = append(out, r)
		}
	}
	return out
}

// makeDue backdates a pending row so the reaper's claim (`scheduled_for
// <= NOW()`) picks it up on the next drain. The author surface refuses
// a past time, so this is how "the time has come" is reached without
// waiting for it; the reaper is not told anything, it just finds a due
// row the way it would in five minutes.
func (f *authorFixture) makeDue(id uuid.UUID) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE scheduled_actions SET scheduled_for = NOW() - interval '1 second' WHERE id = $1`, id); err != nil {
		f.t.Fatalf("backdate: %v", err)
	}
}

func (f *authorFixture) revokePublish(userRef int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = 'posts.publish'`,
		userRef); err != nil {
		f.t.Fatalf("revoke: %v", err)
	}
	auth.InvalidateUserCaps(context.Background(), nil, userRef)
}

func (f *authorFixture) grantPublish(userRef int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code, team_id) VALUES ($1, 'posts.publish', NULL)`,
		userRef); err != nil {
		f.t.Fatalf("grant: %v", err)
	}
	auth.InvalidateUserCaps(context.Background(), nil, userRef)
}

func inAnHour() time.Time { return time.Now().Add(time.Hour).Truncate(time.Second) }

// ---------------------------------------------------------------------------
// Schedule time
// ---------------------------------------------------------------------------

// TestAuthorSchedule_PersistsTheRealCaller is the row the whole seam
// is judged by. The instruction that lands has to be #1238's shape
// exactly, created by THIS user, and readable back through the status
// surface as the same row.
func TestAuthorSchedule_PersistsTheRealCaller(t *testing.T) {
	f := newAuthorFixture(t)
	post := f.post(true)
	when := inAnHour()

	if rows := f.rowsFor(post); len(rows) != 0 {
		t.Fatalf("fixture post already has %d scheduled rows", len(rows))
	}
	got := f.putOK(f.as(f.author), post, when)

	rows := f.rowsFor(post)
	if len(rows) != 1 {
		t.Fatalf("scheduled_actions holds %d rows for the post, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.ID != uuid.UUID(got.Id) {
		t.Errorf("response id %s is not the persisted row %s", got.Id, r.ID)
	}
	if r.CreatedBy == nil || *r.CreatedBy != f.author {
		t.Errorf("persisted created_by=%v, want the requesting author %d", r.CreatedBy, f.author)
	}
	if r.Origin != "author" {
		t.Errorf("persisted origin=%q, want author", r.Origin)
	}
	if r.Action != string(ActionChangeState) || r.TargetKind != string(TargetPost) || r.ToState != visibility.PostPublishedStateCode {
		t.Errorf("persisted instruction (%s, %s, to_state=%s) is not a publish", r.Action, r.TargetKind, r.ToState)
	}
	if r.State != StatePending {
		t.Errorf("persisted state=%q, want pending", r.State)
	}
	if !r.ScheduledFor.Equal(when) {
		t.Errorf("persisted scheduled_for=%s, want %s", r.ScheduledFor, when)
	}
	if got.CreatedBy != f.author || uuid.UUID(got.PostId) != post {
		t.Errorf("response created_by=%d post_id=%s, want %d %s", got.CreatedBy, got.PostId, f.author, post)
	}

	// Status is the persisted row, not a memory of the request.
	s := f.status(f.as(f.author), post)
	if s == nil || uuid.UUID(s.Id) != r.ID || s.State != openapi.PostPublicationScheduleStatePending {
		t.Errorf("status=%+v, want the pending row %s", s, r.ID)
	}

	// Future: the reaper leaves it alone.
	if done, failed := f.drain(); done != 0 || failed != 0 {
		t.Errorf("a future schedule was executed (done=%d failed=%d)", done, failed)
	}
	if got := f.stateCode(post); got != visibility.PostDraftStateCode {
		t.Errorf("post state=%q, want still a draft", got)
	}
}

// TestAuthorSchedule_Refusals is the schedule-time table: each row is
// a refusal the author surface makes BEFORE writing anything.
func TestAuthorSchedule_Refusals(t *testing.T) {
	t.Run("unauthenticated", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		if _, ok := f.put(context.Background(), post, inAnHour()).(openapi.SetPostPublicationSchedule401JSONResponse); !ok {
			t.Error("PUT with no identity was not 401")
		}
		if _, ok := f.get(context.Background(), post).(openapi.GetPostPublicationSchedule401JSONResponse); !ok {
			t.Error("GET with no identity was not 401")
		}
		if _, ok := f.cancel(context.Background(), post).(openapi.CancelPostPublicationSchedule401JSONResponse); !ok {
			t.Error("DELETE with no identity was not 401")
		}
		if n := len(f.rowsFor(post)); n != 0 {
			t.Errorf("%d rows written by refused requests", n)
		}
	})

	t.Run("nonexistent post is 404", func(t *testing.T) {
		f := newAuthorFixture(t)
		ghost := uuid.New()
		if _, ok := f.put(f.as(f.author), ghost, inAnHour()).(openapi.SetPostPublicationSchedule404JSONResponse); !ok {
			t.Error("PUT on a nonexistent post was not 404")
		}
		if _, ok := f.get(f.as(f.author), ghost).(openapi.GetPostPublicationSchedule404JSONResponse); !ok {
			t.Error("GET on a nonexistent post was not 404")
		}
	})

	t.Run("a stranger's draft is 404, not 403: the surface is not an existence oracle", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true) // the author's draft; a draft is readable by its author only
		stranger := f.user()
		if _, ok := f.put(f.as(stranger), post, inAnHour()).(openapi.SetPostPublicationSchedule404JSONResponse); !ok {
			t.Error("PUT on somebody else's unreadable draft was not 404")
		}
		if _, ok := f.get(f.as(stranger), post).(openapi.GetPostPublicationSchedule404JSONResponse); !ok {
			t.Error("GET on somebody else's unreadable draft was not 404")
		}
		if n := len(f.rowsFor(post)); n != 0 {
			t.Errorf("%d rows written by a stranger", n)
		}
	})

	t.Run("a readable post by somebody else is 403", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(false) // public + published: readable by anyone
		stranger := f.user()
		// 403 and not 409: authorship is asked before the post's state,
		// so a stranger learns nothing about whether it is a draft.
		resp := f.put(f.as(stranger), post, inAnHour())
		if r, ok := resp.(openapi.SetPostPublicationSchedule403JSONResponse); !ok || !strings.Contains(r.Error, "author") {
			t.Errorf("PUT by a non-author returned %T (%s), want 403 naming authorship", resp, describe(resp))
		}
		if _, ok := f.get(f.as(stranger), post).(openapi.GetPostPublicationSchedule403JSONResponse); !ok {
			t.Error("GET by a non-author was not 403")
		}
		if _, ok := f.cancel(f.as(stranger), post).(openapi.CancelPostPublicationSchedule403JSONResponse); !ok {
			t.Error("DELETE by a non-author was not 403")
		}
	})

	t.Run("the author without posts.publish is 403", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		f.revokePublish(f.author)
		resp := f.put(f.as(f.author), post, inAnHour())
		if r, ok := resp.(openapi.SetPostPublicationSchedule403JSONResponse); !ok || !strings.Contains(r.Error, "posts.publish") {
			t.Errorf("PUT without the capability returned %T (%s), want 403 naming posts.publish", resp, describe(resp))
		}
		if n := len(f.rowsFor(post)); n != 0 {
			t.Errorf("%d rows written without the capability", n)
		}
		// Reading the (absent) status is still theirs to do.
		if s := f.status(f.as(f.author), post); s != nil {
			t.Errorf("status=%+v, want nil", s)
		}
	})

	t.Run("a soft-deleted post is 404", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		if _, err := f.pool.Exec(context.Background(), `UPDATE posts SET deleted_at = NOW() WHERE id = $1`, post); err != nil {
			t.Fatalf("soft-delete: %v", err)
		}
		if _, ok := f.put(f.as(f.author), post, inAnHour()).(openapi.SetPostPublicationSchedule404JSONResponse); !ok {
			t.Error("PUT on the author's own deleted post was not 404")
		}
	})

	t.Run("an already-published post is 409", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(false)
		resp := f.put(f.as(f.author), post, inAnHour())
		if r, ok := resp.(openapi.SetPostPublicationSchedule409JSONResponse); !ok || !strings.Contains(r.Error, "already published") {
			t.Errorf("PUT on a published post returned %T (%s), want 409", resp, describe(resp))
		}
		if n := len(f.rowsFor(post)); n != 0 {
			t.Errorf("%d rows written for a published post", n)
		}
	})

	t.Run("past or present is 400", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		for name, when := range map[string]time.Time{
			"an hour ago": time.Now().Add(-time.Hour),
			"now":         time.Now(),
		} {
			resp := f.put(f.as(f.author), post, when)
			if _, ok := resp.(openapi.SetPostPublicationSchedule400JSONResponse); !ok {
				t.Errorf("PUT for %s returned %T (%s), want 400", name, resp, describe(resp))
			}
		}
		if n := len(f.rowsFor(post)); n != 0 {
			t.Errorf("%d rows written for a non-future time", n)
		}
		// And the generic engine still takes a due-now instruction from
		// an operator: future-only is this surface's rule, not the table's.
		author := f.author
		a := schedule(t, f.store, ScheduleInput{
			Action: ActionChangeState, TargetKind: TargetPost, TargetID: post.String(),
			Params:       map[string]any{"to_state": visibility.PostPublishedStateCode},
			ScheduledFor: at(-time.Minute), CreatedBy: &author,
		})
		if f.row(a).State != StatePending {
			t.Error("the generic Store.Schedule no longer accepts a past time")
		}
	})
}

// ---------------------------------------------------------------------------
// Fire time
// ---------------------------------------------------------------------------

// TestAuthorSchedule_FireTime is #1238's outcome table reached through
// an AUTHOR-made row, plus the row the author surface adds: the
// capability is revoked after scheduling and the fire is refused while
// the author's cancel still works.
func TestAuthorSchedule_FireTime(t *testing.T) {
	t.Run("a due draft publishes through the publication core", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		a := f.putOK(f.as(f.author), post, inAnHour())
		f.makeDue(uuid.UUID(a.Id))

		done, failed := f.drain()
		if done != 1 || failed != 0 {
			t.Fatalf("drain done=%d failed=%d, want 1/0", done, failed)
		}
		if got := f.stateCode(post); got != visibility.PostPublishedStateCode {
			t.Errorf("post state=%q, want published", got)
		}
		if got := f.activityTypes(post); len(got) != 1 || got[0] != "Create" {
			t.Errorf("federation activities=%v, want [Create]", got)
		}
		if got := f.actorURI(post); !strings.HasPrefix(got, spBaseURL+"/users/") || got == spBaseURL+"/users/" {
			t.Errorf("actor_uri=%q, want the author's handle", got)
		}
		rows := f.rowsFor(post)
		if len(rows) != 1 || rows[0].State != StateDone {
			t.Errorf("rows=%+v, want one done row", rows)
		}
		// Status no longer reports it: it is not pending.
		if s := f.status(f.as(f.author), post); s != nil {
			t.Errorf("status after execution=%+v, want nil", s)
		}
		// NO DOUBLE EXECUTION: a second pass finds nothing to claim.
		if done, failed := f.drain(); done != 0 || failed != 0 {
			t.Errorf("second drain done=%d failed=%d, want 0/0", done, failed)
		}
		if got := f.activityTypes(post); len(got) != 1 {
			t.Errorf("activities after second drain=%v, want still one", got)
		}
	})

	t.Run("published by hand before it fires: idempotent no-op success", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		a := f.putOK(f.as(f.author), post, inAnHour())
		f.manualPublish(post)
		f.makeDue(uuid.UUID(a.Id))
		if done, failed := f.drain(); done != 1 || failed != 0 {
			t.Fatalf("drain done=%d failed=%d, want 1/0", done, failed)
		}
		if got := f.activityTypes(post); len(got) != 1 {
			t.Errorf("activities=%v, want exactly the manual one", got)
		}
		row := stateOf(t, f.store, pgUUID(uuid.UUID(a.Id)))
		if row.State != StateDone {
			t.Errorf("action state=%q, want done", row.State)
		}
	})

	t.Run("soft-deleted after scheduling: failed, post untouched", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		a := f.putOK(f.as(f.author), post, inAnHour())
		if _, err := f.pool.Exec(context.Background(), `UPDATE posts SET deleted_at = NOW() WHERE id = $1`, post); err != nil {
			t.Fatalf("soft-delete: %v", err)
		}
		f.makeDue(uuid.UUID(a.Id))
		if done, failed := f.drain(); done != 0 || failed != 1 {
			t.Fatalf("drain done=%d failed=%d, want 0/1", done, failed)
		}
		if got := f.stateCode(post); got != visibility.PostDraftStateCode {
			t.Errorf("a deleted post's state moved to %q", got)
		}
		if n := len(f.activityTypes(post)); n != 0 {
			t.Errorf("%d activities for a deleted post", n)
		}
	})

	t.Run("unpublished again before it fires: publishes, the instruction stands", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		a := f.putOK(f.as(f.author), post, inAnHour())
		f.manualPublish(post)
		// Back to draft through the real endpoint.
		resp, err := f.posts.UnpublishPost(f.as(f.author), openapi.UnpublishPostRequestObject{Id: openapi_types.UUID(post)})
		if err != nil {
			t.Fatalf("unpublish: %v", err)
		}
		if _, ok := resp.(openapi.UnpublishPost200JSONResponse); !ok {
			t.Fatalf("unpublish returned %T", resp)
		}
		f.makeDue(uuid.UUID(a.Id))
		if done, failed := f.drain(); done != 1 || failed != 0 {
			t.Fatalf("drain done=%d failed=%d, want 1/0", done, failed)
		}
		if got := f.stateCode(post); got != visibility.PostPublishedStateCode {
			t.Errorf("post state=%q, want published", got)
		}
		// Create, Delete (the unpublish), Create (the fire).
		if got := f.activityTypes(post); len(got) != 3 || got[2] != "Create" {
			t.Errorf("activities=%v, want [Create Delete Create]", got)
		}
	})

	t.Run("capability revoked after scheduling: refused at fire time, still cancellable", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		a := f.putOK(f.as(f.author), post, inAnHour())
		f.revokePublish(f.author)

		// The author may still see and withdraw the instruction. Read
		// it first, then leave it standing to watch it fail.
		if s := f.status(f.as(f.author), post); s == nil || uuid.UUID(s.Id) != uuid.UUID(a.Id) {
			t.Errorf("status without the capability=%+v, want the pending row", s)
		}

		f.makeDue(uuid.UUID(a.Id))
		if done, failed := f.drain(); done != 0 || failed != 1 {
			t.Fatalf("drain done=%d failed=%d, want 0/1", done, failed)
		}
		row := stateOf(t, f.store, pgUUID(uuid.UUID(a.Id)))
		if row.State != StateFailed || row.Error == nil || !strings.Contains(*row.Error, "posts.publish") {
			t.Errorf("row state=%q error=%v, want failed naming posts.publish", row.State, row.Error)
		}
		if got := f.stateCode(post); got != visibility.PostDraftStateCode {
			t.Errorf("post state=%q, want still a draft: a schedule must not outlive the right to publish", got)
		}
		if n := len(f.activityTypes(post)); n != 0 {
			t.Errorf("%d activities from a refused fire", n)
		}

		// A second author-made row, made while the capability was
		// held, can be withdrawn after it is lost.
		f.grantPublish(f.author)
		b := f.putOK(f.as(f.author), post, inAnHour())
		f.revokePublish(f.author)
		resp := f.cancel(f.as(f.author), post)
		ok, is := resp.(openapi.CancelPostPublicationSchedule200JSONResponse)
		if !is || uuid.UUID(ok.Id) != uuid.UUID(b.Id) {
			t.Errorf("cancel without the capability returned %T (%s), want 200 for row %s", resp, describe(resp), b.Id)
		}
		if n := len(f.pendingFor(post, "author")); n != 0 {
			t.Errorf("%d author rows still pending after cancel", n)
		}
	})
}

// ---------------------------------------------------------------------------
// One pending author schedule per post
// ---------------------------------------------------------------------------

func TestAuthorSchedule_OnePendingPerPost(t *testing.T) {
	t.Run("N=1: a second PUT replaces the first in one transaction", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		first := f.putOK(f.as(f.author), post, inAnHour())
		later := inAnHour().Add(time.Hour)
		second := f.putOK(f.as(f.author), post, later)
		if second.Id == first.Id {
			t.Fatal("the replacement reused the first row's id")
		}
		rows := f.rowsFor(post)
		if len(rows) != 2 {
			t.Fatalf("rows=%d, want 2 (one cancelled, one pending)", len(rows))
		}
		pending := f.pendingFor(post, "author")
		if len(pending) != 1 || pending[0].ID != uuid.UUID(second.Id) || !pending[0].ScheduledFor.Equal(later) {
			t.Errorf("pending=%+v, want only the second row at %s", pending, later)
		}
		for _, r := range rows {
			if r.ID == uuid.UUID(first.Id) && r.State != StateCancelled {
				t.Errorf("first row state=%q, want cancelled", r.State)
			}
		}
		if s := f.status(f.as(f.author), post); s == nil || s.Id != second.Id {
			t.Errorf("status=%+v, want the second row", s)
		}
	})

	t.Run("N=2 overlapping: the second waits on the index and loses", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		ctx := context.Background()

		// The first inserter, held open: its row is uncommitted, so a
		// concurrent cancel-then-insert sees nothing to cancel and
		// collides on the partial unique index.
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var heldID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO scheduled_actions (action, target_kind, target_id, params, scheduled_for, created_by, origin)
			 VALUES ('change_state','post',$1,'{"to_state":"published"}',NOW() + interval '1 hour',$2,'author')
			 RETURNING id`, post.String(), f.author).Scan(&heldID); err != nil {
			t.Fatalf("held insert: %v", err)
		}
		t.Cleanup(func() { _, _ = f.pool.Exec(ctx, `DELETE FROM scheduled_actions WHERE id = $1`, heldID) })

		type outcome struct {
			row ScheduledAction
			err error
		}
		results := make(chan outcome, 1)
		go func() {
			row, err := f.store.ScheduleAuthorPostPublication(ctx, AuthorPublicationInput{
				PostID: post, ScheduledFor: inAnHour(), CreatedBy: f.author,
			})
			results <- outcome{row, err}
		}()

		// OBSERVED WAIT, not a sleep-and-hope: the second inserter must
		// still be blocked while the first holds its row.
		select {
		case r := <-results:
			t.Fatalf("the second inserter returned (%+v, %v) while the first was uncommitted; the index is not holding it", r.row.ID, r.err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		r := <-results
		if !errors.Is(r.err, ErrAuthorScheduleConflict) {
			t.Fatalf("second inserter err=%v, want ErrAuthorScheduleConflict", r.err)
		}
		pending := f.pendingFor(post, "author")
		if len(pending) != 1 || pending[0].ID != heldID {
			t.Errorf("pending=%+v, want exactly the first inserter's row %s", pending, heldID)
		}
	})

	t.Run("N=8 concurrent PUTs: every answer is 200 or 409, exactly one row pending", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		ctx := f.as(f.author)
		const n = 8
		var wg sync.WaitGroup
		codes := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp, err := f.h.SetPostPublicationSchedule(ctx, openapi.SetPostPublicationScheduleRequestObject{
					Id:   openapi_types.UUID(post),
					Body: &openapi.SetPostPublicationScheduleJSONRequestBody{ScheduledFor: inAnHour().Add(time.Duration(i) * time.Minute)},
				})
				if err != nil {
					codes[i] = -1
					return
				}
				switch resp.(type) {
				case openapi.SetPostPublicationSchedule200JSONResponse:
					codes[i] = 200
				case openapi.SetPostPublicationSchedule409JSONResponse:
					codes[i] = 409
				default:
					codes[i] = 0
				}
			}(i)
		}
		wg.Wait()
		t.Cleanup(func() {
			_, _ = f.pool.Exec(context.Background(), `DELETE FROM scheduled_actions WHERE target_id = $1`, post.String())
		})
		wins := 0
		for i, c := range codes {
			switch c {
			case 200:
				wins++
			case 409:
			default:
				t.Errorf("attempt %d answered %d, want 200 or 409", i, c)
			}
		}
		if wins < 1 {
			t.Errorf("no attempt succeeded: codes=%v", codes)
		}
		if pending := f.pendingFor(post, "author"); len(pending) != 1 {
			t.Errorf("%d author rows pending after %d concurrent PUTs, want exactly 1", len(pending), n)
		}
		if s := f.status(ctx, post); s == nil {
			t.Error("status is empty after a successful PUT")
		}
	})

	t.Run("cancel leaves none pending, and a later schedule is accepted", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		a := f.putOK(f.as(f.author), post, inAnHour())
		resp := f.cancel(f.as(f.author), post)
		if ok, is := resp.(openapi.CancelPostPublicationSchedule200JSONResponse); !is || ok.Id != a.Id || ok.State != openapi.PostPublicationScheduleStateCancelled {
			t.Fatalf("cancel returned %T (%s)", resp, describe(resp))
		}
		if n := len(f.pendingFor(post, "author")); n != 0 {
			t.Errorf("%d pending after cancel", n)
		}
		if s := f.status(f.as(f.author), post); s != nil {
			t.Errorf("status after cancel=%+v, want nil", s)
		}
		// Cancelling again: nothing pending, 404 with its own prose.
		if r, is := f.cancel(f.as(f.author), post).(openapi.CancelPostPublicationSchedule404JSONResponse); !is || !strings.Contains(r.Error, "no pending") {
			t.Errorf("second cancel did not say there was nothing pending: %+v", r)
		}
		// The cancelled row does not fire.
		f.makeDue(uuid.UUID(a.Id))
		if done, failed := f.drain(); done != 0 || failed != 0 {
			t.Errorf("a cancelled row was claimed (done=%d failed=%d)", done, failed)
		}
		if got := f.stateCode(post); got != visibility.PostDraftStateCode {
			t.Errorf("post state=%q after a cancelled schedule, want draft", got)
		}
		b := f.putOK(f.as(f.author), post, inAnHour())
		if pending := f.pendingFor(post, "author"); len(pending) != 1 || pending[0].ID != uuid.UUID(b.Id) {
			t.Errorf("pending after re-schedule=%+v, want only %s", pending, b.Id)
		}
	})

	t.Run("after a terminal execution a later valid schedule is accepted", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		a := f.putOK(f.as(f.author), post, inAnHour())
		f.makeDue(uuid.UUID(a.Id))
		f.drain()
		if got := f.stateCode(post); got != visibility.PostPublishedStateCode {
			t.Fatalf("fixture did not publish (state=%q)", got)
		}
		// Published now, so a schedule is 409 until it is a draft again.
		if _, is := f.put(f.as(f.author), post, inAnHour()).(openapi.SetPostPublicationSchedule409JSONResponse); !is {
			t.Error("scheduling a published post after execution was not 409")
		}
		resp, err := f.posts.UnpublishPost(f.as(f.author), openapi.UnpublishPostRequestObject{Id: openapi_types.UUID(post)})
		if err != nil {
			t.Fatalf("unpublish: %v", err)
		}
		if _, ok := resp.(openapi.UnpublishPost200JSONResponse); !ok {
			t.Fatalf("unpublish returned %T", resp)
		}
		b := f.putOK(f.as(f.author), post, inAnHour())
		if pending := f.pendingFor(post, "author"); len(pending) != 1 || pending[0].ID != uuid.UUID(b.Id) {
			t.Errorf("pending=%+v, want only the new row %s beside the done one", pending, b.Id)
		}
	})

	t.Run("different posts each hold their own", func(t *testing.T) {
		f := newAuthorFixture(t)
		p1, p2 := f.post(true), f.post(true)
		a1 := f.putOK(f.as(f.author), p1, inAnHour())
		a2 := f.putOK(f.as(f.author), p2, inAnHour())
		if a1.Id == a2.Id {
			t.Fatal("two posts share one schedule row")
		}
		if s := f.status(f.as(f.author), p1); s == nil || s.Id != a1.Id {
			t.Errorf("p1 status=%+v, want %s", s, a1.Id)
		}
		if s := f.status(f.as(f.author), p2); s == nil || s.Id != a2.Id {
			t.Errorf("p2 status=%+v, want %s", s, a2.Id)
		}
		f.cancel(f.as(f.author), p1)
		if n := len(f.pendingFor(p1, "author")); n != 0 {
			t.Errorf("p1 still has %d pending", n)
		}
		if pending := f.pendingFor(p2, "author"); len(pending) != 1 || pending[0].ID != uuid.UUID(a2.Id) {
			t.Errorf("cancelling p1 disturbed p2: pending=%+v", pending)
		}
	})

	t.Run("an operator's schedule for the same post is neither blocked, shown, nor cancelled", func(t *testing.T) {
		f := newAuthorFixture(t)
		post := f.post(true)
		operator := f.user()
		// Two generic rows for one post: the engine's multiplicity is
		// unchanged by the author index.
		op1 := schedule(t, f.store, ScheduleInput{
			Action: ActionChangeState, TargetKind: TargetPost, TargetID: post.String(),
			Params: map[string]any{"to_state": visibility.PostPublishedStateCode}, ScheduledFor: at(time.Hour), CreatedBy: &operator,
		})
		op2 := schedule(t, f.store, ScheduleInput{
			Action: ActionChangeState, TargetKind: TargetPost, TargetID: post.String(),
			Params: map[string]any{"to_state": visibility.PostDraftStateCode}, ScheduledFor: at(2 * time.Hour), CreatedBy: &operator,
		})
		if op1.Origin != "generic" || op2.Origin != "generic" {
			t.Fatalf("generic rows carry origin %q/%q, want generic", op1.Origin, op2.Origin)
		}
		// The author's own row still inserts beside them.
		a := f.putOK(f.as(f.author), post, inAnHour())
		if s := f.status(f.as(f.author), post); s == nil || s.Id != a.Id {
			t.Errorf("status=%+v, want the author's own row %s and not an operator's", s, a.Id)
		}
		// Author cancel touches only the author row.
		if _, is := f.cancel(f.as(f.author), post).(openapi.CancelPostPublicationSchedule200JSONResponse); !is {
			t.Error("author cancel did not succeed")
		}
		if n := len(f.pendingFor(post, "generic")); n != 2 {
			t.Errorf("operator rows pending=%d after the author's cancel, want 2 untouched", n)
		}
		if n := len(f.pendingFor(post, "author")); n != 0 {
			t.Errorf("author rows pending=%d, want 0", n)
		}
		// And with only operator rows standing, status is empty and a
		// second cancel has nothing of the author's to cancel.
		if s := f.status(f.as(f.author), post); s != nil {
			t.Errorf("status shows an operator row: %+v", s)
		}
		if _, is := f.cancel(f.as(f.author), post).(openapi.CancelPostPublicationSchedule404JSONResponse); !is {
			t.Error("author cancel reached something that was not the author's")
		}
	})
}

// TestAuthorSchedule_CreatedByIsNeverSubstituted pins the Store against
// a zero creator: the row publishes as this user, so there is no
// default to fall back to.
func TestAuthorSchedule_CreatedByIsNeverSubstituted(t *testing.T) {
	f := newAuthorFixture(t)
	post := f.post(true)
	_, err := f.store.ScheduleAuthorPostPublication(context.Background(), AuthorPublicationInput{
		PostID: post, ScheduledFor: inAnHour(), CreatedBy: 0,
	})
	if err == nil {
		t.Fatal("a zero created_by was accepted")
	}
	if n := len(f.rowsFor(post)); n != 0 {
		t.Errorf("%d rows written with no creator", n)
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
