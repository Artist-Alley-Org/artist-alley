// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #930 — canMutatePost only ever consulted GLOBAL grants, so an art
// director whose posts.admin was scoped to one team could not manage
// that team's posts. #931 — nothing but system.admin could undo a
// delete, including the author's own.
//
// These are in-package so the gate functions can be driven directly,
// but the IDENTITIES come from the real resolver
// (auth.Resolver.LoadIdentity) against real rows. That is the
// load-bearing part: a hand-built auth.Identity literal cannot carry a
// team-scoped grant at all, so a test written that way would assert
// against a fixture it wrote rather than against the team_closure
// expansion the resolver performs. The descendant case is the one that
// proves the difference.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

type pmFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	res  *auth.Resolver
	ctx  context.Context
}

func newPMFixture(t *testing.T) *pmFixture {
	t.Helper()
	pool := previewPool(t)
	return &pmFixture{
		t:    t,
		pool: pool,
		res:  &auth.Resolver{Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		ctx:  context.Background(),
	}
}

func (f *pmFixture) user(label string) int64 {
	f.t.Helper()
	var ref int64
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		"pm-"+label+"-"+uuid.NewString()[:8],
	).Scan(&ref); err != nil {
		f.t.Fatalf("seed user %q: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	// Every real signed-in account holds `posts.publish` — registration
	// puts it on the Base role and migration 00059 grants it there
	// (#1161). A fixture user without it is a caller registration cannot
	// produce, and CreatePost refuses to publish for one, so the team
	// gate's cases would fail on the wrong axis entirely.
	//
	// Granted GLOBALLY and directly rather than through a role, because
	// this fixture builds users by INSERT and its identities come back
	// through the real auth.Resolver; a global grant is the shortest
	// path to "an ordinary user" that the resolver will actually see.
	f.grant(ref, "posts.publish", nil)
	return ref
}

func (f *pmFixture) team(label string, parent *uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`,
		id, "pm_team_"+id.String()[:8], label,
	); err != nil {
		f.t.Fatalf("seed team %q: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	if parent != nil {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO team_parents (parent_id, child_id) VALUES ($1, $2)`, *parent, id,
		); err != nil {
			f.t.Fatalf("link team %q: %v", label, err)
		}
	}
	return id
}

func (f *pmFixture) grant(userRef int64, code string, team *uuid.UUID) {
	f.t.Helper()
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_capability_grants (user_ref, capability_code, team_id) VALUES ($1, $2, $3)`,
		userRef, code, teamArg,
	); err != nil {
		f.t.Fatalf("grant %s: %v", code, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = $2`, userRef, code)
	})
}

func (f *pmFixture) identity(userRef int64) *auth.Identity {
	f.t.Helper()
	return f.res.LoadIdentity(f.ctx, userRef)
}

func teamScope(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// A posts.admin scoped to a parent team reaches that team AND its
// descendants, and nothing else. Before #930 the scoped grant reached
// NOTHING, because only id.Capabilities (globals) was consulted.
func TestCanMutatePost_ScopedGrantReachesTeamAndDescendantsOnly(t *testing.T) {
	f := newPMFixture(t)
	parent := f.team("division", nil)
	child := f.team("squad", &parent)
	unrelated := f.team("other", nil)

	director := f.user("director")
	f.grant(director, CapPostsAdmin, &parent)
	id := f.identity(director)

	author := f.user("author")

	if !canMutatePost(id, author, teamScope(&parent)) {
		t.Error("scoped posts.admin must reach a post in the granted team")
	}
	if !canMutatePost(id, author, teamScope(&child)) {
		t.Error("scoped posts.admin must reach a post in a DESCENDANT of the granted team — " +
			"this is the assertion that proves team_closure expansion is consulted")
	}
	if canMutatePost(id, author, teamScope(&unrelated)) {
		t.Error("scoped posts.admin must NOT reach a post in an unrelated team")
	}
}

// posts.team_id is NULLABLE. A post with no team has no scope for
// InTeam to check, and must fall back to author-or-global rather than
// to "no scope required, therefore anyone passes".
func TestCanMutatePost_TeamlessPostIsNotReachableByScopedGrant(t *testing.T) {
	f := newPMFixture(t)
	team := f.team("division", nil)
	director := f.user("director")
	f.grant(director, CapPostsAdmin, &team)
	author := f.user("author")

	if canMutatePost(f.identity(director), author, teamScope(nil)) {
		t.Error("a team-scoped grant must confer nothing over a post with team_id IS NULL")
	}
}

// The author and a global grant are unchanged by the scope work.
func TestCanMutatePost_AuthorAndGlobalGrantUnchanged(t *testing.T) {
	f := newPMFixture(t)
	author := f.user("author")
	moderator := f.user("moderator")
	f.grant(moderator, CapPostsAdmin, nil)
	team := f.team("division", nil)

	if !canMutatePost(f.identity(author), author, teamScope(&team)) {
		t.Error("the author must still be able to mutate their own post")
	}
	if !canMutatePost(f.identity(moderator), author, teamScope(&team)) {
		t.Error("a global posts.admin must still reach a teamed post")
	}
	if !canMutatePost(f.identity(moderator), author, teamScope(nil)) {
		t.Error("a global posts.admin must still reach a team-less post")
	}
}

// The anonymous sentinel carries UserRef 0. An `id.UserRef ==
// authorRef` comparison against a post authored by ref 0 would hand
// authorship to every anonymous visitor.
func TestCanMutatePost_AnonymousIsNeverTheAuthor(t *testing.T) {
	anon := &auth.Identity{UserRef: 0, AuthMethod: "anonymous"}
	if canMutatePost(anon, 0, pgtype.UUID{}) {
		t.Error("an anonymous caller must not be treated as the author of a ref-0 post")
	}
	if canMutatePost(nil, 0, pgtype.UUID{}) {
		t.Error("a nil identity must never be authorised")
	}
}

// The disclosure boundary. PostUpdate carries `visibility` and
// AddPostAcl writes a grant row; both change who can REACH the post, so
// opening canMutatePost to team-scoped grants would otherwise have
// handed a team lead the power to publish or share a colleague's work.
//
// Both endpoints call canWidenPostAccess. AddPostAcl used to call
// canMutatePost, which was safe only for as long as canMutatePost meant
// "author or a GLOBAL moderator" — widening it without moving this gate
// would have been the escalation, not the fix.
func TestCanWidenPostAccess_TeamScopedGrantIsExcluded(t *testing.T) {
	f := newPMFixture(t)
	team := f.team("division", nil)
	director := f.user("director")
	f.grant(director, CapPostsAdmin, &team)
	author := f.user("author")

	dirID := f.identity(director)
	if !canMutatePost(dirID, author, teamScope(&team)) {
		t.Fatal("precondition: the scoped grant should reach this post for ordinary edits")
	}
	if canWidenPostAccess(dirID, author) {
		t.Error("a TEAM-SCOPED posts.admin must not be able to change a post's visibility — " +
			"managing a team's posts is content management, not a disclosure decision")
	}

	// The author keeps it, and so does a global moderator.
	if !canWidenPostAccess(f.identity(author), author) {
		t.Error("the author must keep their own visibility control")
	}
	moderator := f.user("moderator")
	f.grant(moderator, CapPostsAdmin, nil)
	if !canWidenPostAccess(f.identity(moderator), author) {
		t.Error("a GLOBAL posts.admin is the instance moderator role and is unchanged by this")
	}
}

// #931 — restore keys on the deleter, not on standing authority.
func TestCanRestorePost_DeleterOrSystemAdminOnly(t *testing.T) {
	f := newPMFixture(t)
	author := f.user("author")
	other := f.user("other")
	admin := f.user("admin")
	f.grant(admin, CapSystemAdmin, nil)

	authorID := f.identity(author)
	otherID := f.identity(other)
	adminID := f.identity(admin)

	if !auth.CanRestoreDeleted(authorID, &author) {
		t.Error("you must be able to undo your own delete")
	}
	if auth.CanRestoreDeleted(authorID, &other) {
		t.Error("you must NOT be able to undo someone else's delete")
	}
	if auth.CanRestoreDeleted(authorID, nil) {
		t.Error("a NULL deleter must fail closed, not open")
	}
	if !auth.CanRestoreDeleted(adminID, nil) {
		t.Error("system.admin must be able to restore a row with no recorded deleter")
	}
	if !auth.CanRestoreDeleted(adminID, &other) {
		t.Error("system.admin must be able to restore anything")
	}
	if auth.CanRestoreDeleted(otherID, nil) {
		t.Error("an ordinary user must not restore a NULL-deleter row")
	}

	anon := &auth.Identity{UserRef: 0, AuthMethod: "anonymous"}
	var zero int64
	if auth.CanRestoreDeleted(anon, &zero) {
		t.Error("an anonymous caller must not be matched against a ref-0 deleter")
	}
}

// The soft-delete query itself must persist the deleter — the gate
// above decides nothing if the column is never written.
func TestSoftDeletePost_WritesDeletedBy(t *testing.T) {
	f := newPMFixture(t)
	author := f.user("author")

	var postID pgtype.UUID
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO posts (author_user_ref, title, description, visibility)
		 VALUES ($1, 'pm-delete-931', '', 'private') RETURNING id`,
		author,
	).Scan(&postID); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, postID)
	})

	if err := New(f.pool).SoftDeletePost(f.ctx, SoftDeletePostParams{
		ID:               postID,
		DeletedByUserRef: &author,
	}); err != nil {
		t.Fatalf("SoftDeletePost: %v", err)
	}

	var deletedBy *int64
	if err := f.pool.QueryRow(f.ctx,
		`SELECT deleted_by_user_ref FROM posts WHERE id = $1`, postID,
	).Scan(&deletedBy); err != nil {
		t.Fatalf("read deleted_by_user_ref: %v", err)
	}
	if deletedBy == nil || *deletedBy != author {
		t.Errorf("deleted_by_user_ref = %v, want %d", deletedBy, author)
	}
}
