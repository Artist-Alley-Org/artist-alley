// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1238 — scheduled publication, driven through the REAL reaper.
//
// # The pair is the assertion
//
// Publishing a post moves `posts.state_id` and writes the federation
// activity that tells peers the post exists, in one transaction (ADR
// 0044). An executor that wrote the column itself would publish the
// post on this instance and on no other — silently, with a green suite,
// because a state assertion alone passes on exactly that bug. So every
// success case here asserts BOTH, and the federation half is asserted
// first: it is the half a shortcut removes.
//
// # Why the real reaper and the real posts handler
//
// The executor runs inside a savepoint of the reaper's claim
// transaction, and the publication core opens its own. A fake publisher
// would prove the executor calls something; it would not prove that the
// something commits, that the workflow edge exists, that the capability
// gate lets this actor through, or that the activity row lands. Those
// are the parts that were never wired together before this sprint.
//
// # The fire-time table
//
// Decided at planning; each row has a case below, including the two
// ways to reach the no-op cell (a second schedule, and an author who
// publishes manually first).
//
// Skips without AA_DB_PASSWORD.

package scheduledactions

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
	"github.com/mscrnt/artist-alley/app/internal/workflow"
)

const spBaseURL = "https://sched-publish.example"

// spFixture is one test's world: a real author who may publish, a real
// posts handler wired the way boot wires it, and a reaper that owns it
// as its Publisher.
type spFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *Store
	posts  *posts.Handler
	reaper *ReaperJob
	author int64
}

func newSPFixture(t *testing.T) *spFixture {
	t.Helper()
	pool := saPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	h := posts.NewHandler(pool, logger, cache.NewRegistry(pool, logger))
	h.SetActivitiesWriter(activities.NewWriter(pool, logger, nil),
		func(context.Context) string { return spBaseURL })
	h.SetWorkflow(workflow.NewService(pool, logger))
	// The REAL resolver, so the actor's capabilities and username come
	// from the database exactly as they do at boot. A literal identity
	// here would assert against a fixture rather than against the gate.
	h.SetActorLoader(&auth.Resolver{Pool: pool, Logger: logger})

	f := &spFixture{t: t, pool: pool, store: NewStore(pool), posts: h}
	f.reaper = &ReaperJob{
		Pool:      pool,
		Rec:       audit.NewRecorder(pool, logger),
		Notifier:  &fakeNotifier{},
		Publisher: h,
		Logger:    logger,
	}
	f.author = f.user()
	return f
}

// user plants a real account holding `posts.publish` — what every
// registered account holds (migration 00059 grants it to the Base
// role). Granted directly because this fixture builds users by INSERT
// and reads them back through the real resolver.
func (f *spFixture) user() int64 {
	f.t.Helper()
	var ref int64
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		"sp-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		f.t.Fatalf("seed user: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code, team_id)
		 VALUES ($1, 'posts.publish', NULL)`, ref); err != nil {
		f.t.Fatalf("grant posts.publish: %v", err)
	}
	f.t.Cleanup(func() {
		testdb.Purge(f.t, f.pool, ref,
			`DELETE FROM user_capability_grants WHERE user_ref = $1`,
			`DELETE FROM activities WHERE actor_user_ref = $1`,
			`DELETE FROM "user" WHERE ref = $1`,
		)
	})
	return ref
}

// post plants one post in the named publication state.
func (f *spFixture) post(draft bool) uuid.UUID {
	f.t.Helper()
	code := visibility.PostPublishedStateCode
	if draft {
		code = visibility.PostDraftStateCode
	}
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility, state_id)
		 VALUES ($1, $2, 'sp probe', 'sp body', 'public',
		         (SELECT id FROM workflow_states WHERE domain = $3 AND code = $4))`,
		id, f.author, visibility.PostWorkflowDomain, code); err != nil {
		f.t.Fatalf("seed post: %v", err)
	}
	f.t.Cleanup(func() {
		testdb.Purge(f.t, f.pool, id,
			`DELETE FROM workflow_audit WHERE resource_id = $1`,
			`DELETE FROM posts WHERE id = $1`,
		)
	})
	return id
}

// schedulePublish enqueues one due publication for postID.
func (f *spFixture) schedulePublish(postID uuid.UUID, when time.Duration) ScheduledAction {
	f.t.Helper()
	author := f.author
	a := schedule(f.t, f.store, ScheduleInput{
		Action: ActionChangeState, TargetKind: TargetPost, TargetID: postID.String(),
		Params:       map[string]any{"to_state": visibility.PostPublishedStateCode},
		ScheduledFor: at(when), CreatedBy: &author,
	})
	f.t.Cleanup(func() {
		testdb.Purge(f.t, f.pool, uuid.UUID(a.ID.Bytes).String(),
			`DELETE FROM audit_events WHERE metadata->>'scheduled_action_id' = $1`)
	})
	return a
}

// stateCode reads the post's PERSISTED publication state back from the
// row — never the handler's echo of its own write.
func (f *spFixture) stateCode(postID uuid.UUID) string {
	f.t.Helper()
	var code string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT ws.code FROM posts p JOIN workflow_states ws ON ws.id = p.state_id
		  WHERE p.id = $1`, postID).Scan(&code); err != nil {
		f.t.Fatalf("read post state: %v", err)
	}
	return code
}

// activityTypes lists the federation activities recorded against a post,
// oldest first. THE assertion of this file: a state move with no row
// here is a post published on this instance and on no other.
func (f *spFixture) activityTypes(postID uuid.UUID) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT activity_type FROM activities
		  WHERE object_local_id = $1 ORDER BY published_at ASC, id ASC`, postID.String())
	if err != nil {
		f.t.Fatalf("read activities: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatalf("scan activity: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// actorURI returns the actor handle the federation row carries. The
// scheduled path resolves its own identity, so this is where a
// half-loaded actor shows up — `{base}/users/` with nothing after it.
func (f *spFixture) actorURI(postID uuid.UUID) string {
	f.t.Helper()
	var uri string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT actor_uri FROM activities WHERE object_local_id = $1
		  ORDER BY published_at ASC, id ASC LIMIT 1`, postID.String()).Scan(&uri); err != nil {
		f.t.Fatalf("read actor uri: %v", err)
	}
	return uri
}

// auditExtra returns one executed audit row's metadata value, rendered
// for a failure message ("<absent>" when the key is not there) rather
// than as a pointer — a red assertion that prints 0x… tells whoever
// reads it nothing.
func (f *spFixture) auditExtra(a ScheduledAction, key string) string {
	f.t.Helper()
	var v *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT metadata->>$2 FROM audit_events
		  WHERE event_type = $3 AND metadata->>'scheduled_action_id' = $1`,
		uuid.UUID(a.ID.Bytes).String(), key,
		audit.EventScheduledActionExecuted).Scan(&v); err != nil {
		f.t.Fatalf("read audit %q: %v", key, err)
	}
	if v == nil {
		return "<absent>"
	}
	return *v
}

func (f *spFixture) drain() (int, int) { return drain(f.t, f.reaper) }

func (f *spFixture) row(a ScheduledAction) ScheduledAction {
	return stateOf(f.t, f.store, a.ID)
}

// manualPublish is the AUTHOR publishing through the ordinary HTTP
// endpoint — the real surface, not a second call to the scheduled arm.
// The race in the table is "somebody beat the schedule to it", and only
// the real endpoint proves the two paths agree about what happened.
func (f *spFixture) manualPublish(postID uuid.UUID) {
	f.t.Helper()
	id := (&auth.Resolver{Pool: f.pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).
		LoadIdentity(context.Background(), f.author)
	resp, err := f.posts.PublishPost(auth.WithIdentity(context.Background(), id),
		openapi.PublishPostRequestObject{Id: openapi_types.UUID(postID)})
	if err != nil {
		f.t.Fatalf("manual publish: %v", err)
	}
	if _, ok := resp.(openapi.PublishPost200JSONResponse); !ok {
		f.t.Fatalf("manual publish returned %T, want 200", resp)
	}
}

// ---------------------------------------------------------------------------
// The fire-time table
// ---------------------------------------------------------------------------

// TestScheduledPublish_DraftPublishesAndFederates is row 1, and the
// case that fails on every shortcut: a direct `UPDATE posts SET
// state_id` passes the state half and leaves activities empty.
func TestScheduledPublish_DraftPublishesAndFederates(t *testing.T) {
	f := newSPFixture(t)
	post := f.post(true)
	a := f.schedulePublish(post, -time.Second)

	if got := f.stateCode(post); got != visibility.PostDraftStateCode {
		t.Fatalf("fixture is not a draft (state=%q)", got)
	}
	f.drain()

	if got := f.row(a); got.State != StateDone {
		errText := ""
		if got.Error != nil {
			errText = *got.Error
		}
		t.Fatalf("action state=%q (%s), want done", got.State, errText)
	}
	if got := f.stateCode(post); got != visibility.PostPublishedStateCode {
		t.Errorf("post state=%q, want published", got)
	}
	// The half a column write would have skipped.
	if got := f.activityTypes(post); len(got) != 1 || got[0] != "Create" {
		t.Errorf("federation activities=%v, want exactly [Create] — a state move with no "+
			"activity publishes the post here and nowhere else", got)
	}
	// And it federates AS the scheduler, with a usable actor handle.
	if got, want := f.actorURI(post), spBaseURL+"/users/"; got == want || !strings.HasPrefix(got, want) {
		t.Errorf("actor_uri=%q, want %q + a username", got, want)
	}
	if got := f.auditExtra(a, "skipped"); got != "false" {
		t.Errorf("audit skipped=%s, want false — this fire changed the post", got)
	}
	if got := f.auditExtra(a, "actor_user_ref"); got == "<absent>" {
		t.Error("the audit row does not record who the publication was attributed to")
	}
}

// TestScheduledPublish_AlreadyPublishedIsANoOpSuccess is row 2, reached
// the two ways it happens in the wild. Both must be `done`, not
// `failed`: the instruction was "be published by then", and it is.
func TestScheduledPublish_AlreadyPublishedIsANoOpSuccess(t *testing.T) {
	t.Run("two schedules for the same post", func(t *testing.T) {
		f := newSPFixture(t)
		post := f.post(true)
		first := f.schedulePublish(post, -2*time.Second)
		second := f.schedulePublish(post, -time.Second)

		f.drain()

		for name, a := range map[string]ScheduledAction{"first": first, "second": second} {
			if got := f.row(a); got.State != StateDone {
				errText := ""
				if got.Error != nil {
					errText = *got.Error
				}
				t.Errorf("%s action state=%q (%s), want done", name, got.State, errText)
			}
		}
		if got := f.stateCode(post); got != visibility.PostPublishedStateCode {
			t.Errorf("post state=%q, want published", got)
		}
		// One publication, one activity. A second Create would announce
		// the same post to peers twice.
		if got := f.activityTypes(post); len(got) != 1 {
			t.Errorf("federation activities=%v, want exactly one", got)
		}
		if got := f.auditExtra(second, "skipped"); got != "true" {
			t.Errorf("the second fire recorded skipped=%s, want true — the trail has to be "+
				"honest that it changed nothing", got)
		}
	})

	t.Run("the author publishes manually first", func(t *testing.T) {
		f := newSPFixture(t)
		post := f.post(true)
		a := f.schedulePublish(post, -time.Second)

		// The race: a real publish through the real endpoint, between the
		// schedule and the fire.
		f.manualPublish(post)
		if got := f.stateCode(post); got != visibility.PostPublishedStateCode {
			t.Fatalf("manual publish did not land (state=%q)", got)
		}

		f.drain()

		if got := f.row(a); got.State != StateDone {
			errText := ""
			if got.Error != nil {
				errText = *got.Error
			}
			t.Errorf("action state=%q (%s), want done — losing a race to the author is not "+
				"a failure", got.State, errText)
		}
		if got := f.auditExtra(a, "skipped"); got != "true" {
			t.Errorf("audit skipped=%s, want true", got)
		}
		if got := f.activityTypes(post); len(got) != 1 {
			t.Errorf("federation activities=%v, want exactly one (the author's)", got)
		}
	})
}

// TestScheduledPublish_SoftDeletedFails is row 3. A deleted post is not
// a post the read gate will admit, and publishing one would resurrect
// it onto the shared surfaces from a schedule its author may have
// forgotten. Failed, with the reason on the row.
func TestScheduledPublish_SoftDeletedFails(t *testing.T) {
	f := newSPFixture(t)
	post := f.post(true)
	a := f.schedulePublish(post, -time.Second)

	if _, err := f.pool.Exec(context.Background(),
		`UPDATE posts SET deleted_at = NOW() WHERE id = $1`, post); err != nil {
		t.Fatalf("soft-delete post: %v", err)
	}

	f.drain()

	got := f.row(a)
	if got.State != StateFailed {
		t.Fatalf("action state=%q, want failed", got.State)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "not found") {
		t.Errorf("failure reason %v does not say the post was gone", got.Error)
	}
	if len(f.activityTypes(post)) != 0 {
		t.Error("a deleted post was announced to peers")
	}
	if f.stateCode(post) != visibility.PostDraftStateCode {
		t.Error("a deleted post's publication state moved")
	}
}

// TestScheduledPublish_UnpublishIsNotACancel is row 4, and the one that
// is a PRODUCT decision rather than a mechanism: a schedule is a
// standing instruction until it is cancelled, and unpublishing is not
// cancelling. The cancel surface already exists
// (POST /admin/scheduled-actions/{id}/cancel); taking a post down for
// the afternoon is not a request to forget next week's publication.
func TestScheduledPublish_UnpublishIsNotACancel(t *testing.T) {
	f := newSPFixture(t)
	// Published, then taken down again — the state the row describes.
	post := f.post(false)
	a := f.schedulePublish(post, -time.Second)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE posts SET state_id = (SELECT id FROM workflow_states
		     WHERE domain = $2 AND code = $3) WHERE id = $1`,
		post, visibility.PostWorkflowDomain, visibility.PostDraftStateCode); err != nil {
		t.Fatalf("unpublish post: %v", err)
	}

	f.drain()

	if got := f.row(a); got.State != StateDone {
		errText := ""
		if got.Error != nil {
			errText = *got.Error
		}
		t.Fatalf("action state=%q (%s), want done", got.State, errText)
	}
	if got := f.stateCode(post); got != visibility.PostPublishedStateCode {
		t.Errorf("post state=%q, want published — the schedule stands until cancelled", got)
	}
	if got := f.activityTypes(post); len(got) != 1 || got[0] != "Create" {
		t.Errorf("federation activities=%v, want [Create]", got)
	}
	if got := f.auditExtra(a, "skipped"); got != "false" {
		t.Errorf("audit skipped=%s, want false — this fire republished the post", got)
	}
}

// TestScheduledPublish_CancelStopsIt is the complement of the row
// above: the instruction IS standing, and cancelling is how it stops.
// Without this, "unpublish does not cancel" would read as "nothing
// cancels".
func TestScheduledPublish_CancelStopsIt(t *testing.T) {
	f := newSPFixture(t)
	post := f.post(true)
	a := f.schedulePublish(post, -time.Second)

	ok, err := f.store.Cancel(context.Background(), uuid.UUID(a.ID.Bytes))
	if err != nil || !ok {
		t.Fatalf("cancel: ok=%v err=%v", ok, err)
	}
	f.drain()

	if got := f.row(a).State; got != StateCancelled {
		t.Errorf("action state=%q, want cancelled", got)
	}
	if got := f.stateCode(post); got != visibility.PostDraftStateCode {
		t.Errorf("a cancelled publication still published the post (state=%q)", got)
	}
	if len(f.activityTypes(post)) != 0 {
		t.Error("a cancelled publication still told the peers")
	}
}

// TestScheduledPublish_RefusesWhenTheCoreIsUnwired pins the fail-loud
// posture the Publisher interface promises: no publisher, no fallback
// to a column write. The sabotage shape this catches is somebody
// "fixing" an unwired reaper by adding an UPDATE beside the asset arms.
func TestScheduledPublish_RefusesWhenTheCoreIsUnwired(t *testing.T) {
	f := newSPFixture(t)
	f.reaper.Publisher = nil
	post := f.post(true)
	a := f.schedulePublish(post, -time.Second)

	f.drain()

	if got := f.row(a); got.State != StateFailed {
		t.Errorf("action state=%q, want failed", got.State)
	}
	if got := f.stateCode(post); got != visibility.PostDraftStateCode {
		t.Errorf("the post was published without the publication core (state=%q)", got)
	}
	if len(f.activityTypes(post)) != 0 {
		t.Error("an activity was written without the publication core")
	}
}

// TestScheduledPublish_ActorWithoutTheCapabilityIsRefused pins the gate
// that makes publication instance policy: `posts.publish` is checked at
// FIRE time, against the database, so revoking it between the schedule
// and the fire stops the publication. A scheduled action that carried
// its own permission would be a way round the revocation.
func TestScheduledPublish_ActorWithoutTheCapabilityIsRefused(t *testing.T) {
	f := newSPFixture(t)
	post := f.post(true)
	a := f.schedulePublish(post, -time.Second)

	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = 'posts.publish'`,
		f.author); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	auth.InvalidateUserCaps(context.Background(), nil, f.author)

	f.drain()

	got := f.row(a)
	if got.State != StateFailed {
		t.Errorf("action state=%q, want failed — a revoked capability must stop the fire", got.State)
	}
	// Named, so this case cannot pass for some other reason — an
	// unreadable post and a missing capability are different bugs and
	// the 404-first ordering makes them easy to confuse.
	if got.Error == nil || !strings.Contains(*got.Error, "posts.publish") {
		t.Errorf("failure reason %v does not name the capability that stopped it", got.Error)
	}
	if got := f.stateCode(post); got != visibility.PostDraftStateCode {
		t.Errorf("post state=%q, want wip", got)
	}
	if len(f.activityTypes(post)) != 0 {
		t.Error("a refused publication still told the peers")
	}
}
