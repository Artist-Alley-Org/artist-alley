// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1292 -- `GET /posts?mature=not_mature` is the browse footer's Mature
// row, which is ADR 0090's LAYER 3: the view.
//
// # The three layers, and why only the third one is here
//
// Layer 1 is the instance switch, layer 2 is the account opt-in, and
// together they are the qualification predicate this package already
// tests through visibility.MatureFilterSQL. Layer 3 is a reader those
// two have ALREADY said yes to, saying "not in these results, right
// now".
//
// So every case below runs as a QUALIFIED viewer, because a
// disqualified one cannot see a mature post with or without the
// parameter and would pass an inverted implementation, an ignored
// parameter and a correct one alike. That is the shape of a
// single-arm test that reads green on the bug, and
// TestMatureViewFilter_CannotWiden is the one arm that runs
// disqualified, for the opposite reason: to prove the parameter is
// not a way in.
//
// # The two asymmetries with the GATE, pinned here because they are
// # decisions rather than consequences
//
//	the OWNER exemption   the gate has one; this does NOT
//	the ADMIN waiver      the gate has one; this does NOT
//
// Both exist on the gate so that an operator's switch cannot take
// content away from the artist who owns it or the moderator who has to
// look at it. Nothing is being taken away here: the reader asked for
// this themselves, and one untick gives it straight back. A filter that
// silently kept your own mature work on a wall you just asked to clear
// is a control that refuses to do the one thing it says it does.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs, distinct from every other file's so parallel runs
// cannot see each other's fixtures. posts.author_user_ref has no FK.
const (
	mvfAuthor int64 = 12920001
	mvfOther  int64 = 12920002
)

// mvfResolver answers the mature axis for one ref and refuses every
// other, so "the caller was resolved as themselves" is asserted rather
// than assumed: a handler that resolved for nobody lands on the
// disqualified viewer and every case below changes its answer.
type mvfResolver struct {
	wantRef int64
	answer  visibility.MatureViewer
}

func (r mvfResolver) ResolveMature(
	_ context.Context, c visibility.Caller,
) (visibility.MatureViewer, error) {
	if c.UserRef != r.wantRef {
		return visibility.AnonymousMatureViewer, errors.New("posts: resolver asked about the wrong caller")
	}
	return r.answer, nil
}

// mvfQualified is the three-conjunct yes.
var mvfQualified = visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}

// mvfAsset plants one asset with a mature flag.
func mvfAsset(t *testing.T, pool *pgxpool.Pool, owner int64, mature bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                     processing_status, file_extension, mature)
		 VALUES ($1,$2,$3,1,'active','public','ready','png',$4)`,
		id, "mvf-asset", owner, mature); err != nil {
		t.Fatalf("seed asset (mature=%v): %v", mature, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
	})
	return id
}

// mvfPost plants a post around one member.
//
// ⚠️ THE MEMBERSHIP IS WRITTEN AFTER THE ASSET, like aiPost's, because
// `posts.mature` is DERIVED by a trigger on post_assets (ADR 0090 §4)
// and the recompute has to see a population that already carries the
// flag.
func mvfPost(t *testing.T, pool *pgxpool.Pool, author int64, member uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	at := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO posts (id, author_user_ref, title, description, visibility, posted_at, cover_asset_id)
		 VALUES ($1,$2,'mvf post','','public',$3,$4)`,
		id, author, at, member); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,0)`, id, member); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

// mvfCorpus is one mature post and one plain one by the same author,
// plus the assertion that the TRIGGER derived what every case assumes.
//
// ⚠️ THE DERIVATION CHECK IS NOT CEREMONY. `posts.mature` defaults to
// false, so a trigger that did not fire leaves the "mature" post
// looking plain, where "the filter removed it" FAILS and every other
// case here still PASSES. That reads as a filter bug and is not one.
type mvfCorpus struct {
	mature uuid.UUID
	plain  uuid.UUID
}

func newMVFCorpus(t *testing.T, pool *pgxpool.Pool, author int64) mvfCorpus {
	t.Helper()
	c := mvfCorpus{
		mature: mvfPost(t, pool, author, mvfAsset(t, pool, author, true)),
		plain:  mvfPost(t, pool, author, mvfAsset(t, pool, author, false)),
	}
	for _, want := range []struct {
		label string
		id    uuid.UUID
		flag  bool
	}{{"mature", c.mature, true}, {"plain", c.plain, false}} {
		var got bool
		if err := pool.QueryRow(t.Context(),
			`SELECT mature FROM posts WHERE id=$1`, want.id).Scan(&got); err != nil {
			t.Fatalf("read derived mature flag: %v", err)
		}
		if got != want.flag {
			t.Fatalf("the %s fixture derived posts.mature=%v, want %v -- the trigger did not "+
				"fire, so every case below would be asserting about the wrong corpus",
				want.label, got, want.flag)
		}
	}
	return c
}

// mvfFeed runs one feed page, scoped to one author so the assertions
// are about this file's corpus rather than about scroll depth on a
// shared database (see aiFeedAuthored for the argument).
//
// `admin` grants system.admin, which is the gate's waiver and must NOT
// be the view filter's.
func mvfFeed(
	t *testing.T, h *Handler, callerRef int64, exclude, admin bool,
) map[uuid.UUID]bool {
	t.Helper()
	resp := mvfFeedRaw(t, h, callerRef, exclude, admin, nil)
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts(caller=%d exclude=%v admin=%v) returned %T, want 200",
			callerRef, exclude, admin, resp)
	}
	out := make(map[uuid.UUID]bool, len(ok.Items))
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}

// mvfFeedRaw is mvfFeed without the 200 assertion -- the fail-closed
// case needs to inspect a non-200. `raw` overrides the wire value so a
// junk one can be driven through the same path.
func mvfFeedRaw(
	t *testing.T, h *Handler, callerRef int64, exclude, admin bool, raw *string,
) openapi.ListPostsResponseObject {
	t.Helper()
	id := &auth.Identity{UserRef: callerRef, AuthMethod: "session"}
	if admin {
		id.Capabilities = []string{CapSystemAdmin}
	}
	ctx := auth.WithIdentity(context.Background(), id)

	limit := 200
	author := mvfAuthor
	vis := openapi.ListPostsParamsVisibility("public")
	params := openapi.ListPostsParams{Limit: &limit, AuthorRef: &author, Visibility: &vis}
	switch {
	case raw != nil:
		v := openapi.ListPostsParamsMature(*raw)
		params.Mature = &v
	case exclude:
		v := openapi.ListPostsParamsMatureNotMature
		params.Mature = &v
	}

	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: params})
	if err != nil {
		t.Fatalf("ListPosts(caller=%d exclude=%v): %v", callerRef, exclude, err)
	}
	return resp
}

func mvfPresent(t *testing.T, what string, got map[uuid.UUID]bool, id uuid.UUID) {
	t.Helper()
	if !got[id] {
		t.Errorf("%s: post %v is MISSING", what, id)
	}
}

func mvfAbsent(t *testing.T, what string, got map[uuid.UUID]bool, id uuid.UUID) {
	t.Helper()
	if got[id] {
		t.Errorf("%s: post %v is PRESENT and must not be", what, id)
	}
}

// mvfHandler wires a handler whose resolver qualifies exactly one ref.
func mvfHandler(t *testing.T, pool *pgxpool.Pool, qualified int64) *Handler {
	t.Helper()
	h := peHandler(pool)
	h.SetMatureResolver(mvfResolver{wantRef: qualified, answer: mvfQualified})
	return h
}

// ⭐ THE CONTROL ARM. Absent parameter, qualified reader: BOTH posts.
//
// This is the case that says shipping layer 3 changed nothing for a
// reader who had already opted in, and it is the half a "make the
// filter work" implementation is most likely to break by defaulting the
// conjunct on. ADR 0090's amendment states it as a requirement rather
// than an observation: "Layer 3 defaults to included".
func TestMatureViewFilter_AbsentIsIncluded(t *testing.T) {
	pool := previewPool(t)
	h := mvfHandler(t, pool, mvfAuthor)
	c := newMVFCorpus(t, pool, mvfAuthor)

	got := mvfFeed(t, h, mvfAuthor, false, false)
	mvfPresent(t, "no ?mature= (the MATURE post)", got, c.mature)
	mvfPresent(t, "no ?mature= (the plain post)", got, c.plain)
}

// The filter itself: the mature post goes, the plain one stays.
//
// The second half is what separates a filter from an outage. A
// predicate written as a bare `mature` rather than `NOT mature`, or one
// that drops NULLs, empties the wall -- and "the mature post is gone"
// passes on both.
func TestMatureViewFilter_ExcludesTheMatureOne(t *testing.T) {
	pool := previewPool(t)
	h := mvfHandler(t, pool, mvfAuthor)
	c := newMVFCorpus(t, pool, mvfAuthor)

	got := mvfFeed(t, h, mvfAuthor, true, false)
	mvfAbsent(t, "?mature=not_mature (the MATURE post)", got, c.mature)
	mvfPresent(t, "⭐ ?mature=not_mature (the plain post must SURVIVE)", got, c.plain)
}

// ⭐ NO OWNER EXEMPTION. The caller here is the author of both fixtures,
// which is exactly the case visibility.MatureFilterSQL exempts -- and
// this filter does not.
//
// The gate exempts an owner so that an operator flipping the instance
// switch cannot destroy an artist's access to their own uploads. That
// argument does not reach here: the reader asked for this filter
// themselves and one untick gives it back. Without this case the
// obvious "reuse MatureFilterSQL with a disqualified viewer" shortcut
// passes everything else in this file.
func TestMatureViewFilter_AppliesToTheCallersOwnPosts(t *testing.T) {
	pool := previewPool(t)
	h := mvfHandler(t, pool, mvfAuthor)
	c := newMVFCorpus(t, pool, mvfAuthor)

	got := mvfFeed(t, h, mvfAuthor, true, false)
	mvfAbsent(t, "⭐ ?mature=not_mature as the AUTHOR (own mature post)", got, c.mature)
	mvfPresent(t, "?mature=not_mature as the AUTHOR (own plain post)", got, c.plain)
}

// ⭐ NO ADMIN WAIVER. `MatureAdmin` waives the GATE for system.admin,
// who has to be able to moderate what the instance switch hid. It must
// not waive this, or a moderator's own request to clear their feed
// would visibly do nothing.
//
// The caller is an admin who is ALSO not the author, so neither
// exemption on the gate is in play and the only thing that can remove
// the post is the view filter.
func TestMatureViewFilter_IsNotWaivedForAnAdmin(t *testing.T) {
	pool := previewPool(t)
	h := mvfHandler(t, pool, mvfOther)
	c := newMVFCorpus(t, pool, mvfAuthor)

	both := mvfFeed(t, h, mvfOther, false, true)
	mvfPresent(t, "admin, no ?mature= (the MATURE post)", both, c.mature)

	got := mvfFeed(t, h, mvfOther, true, true)
	mvfAbsent(t, "⭐ ?mature=not_mature as system.admin", got, c.mature)
	mvfPresent(t, "?mature=not_mature as system.admin (the plain post)", got, c.plain)
}

// ⛔ IT CANNOT WIDEN. A DISQUALIFIED reader gets the same page with the
// parameter and without it, and the mature post is in neither.
//
// The parameter's vocabulary has one value, so there is no spelling
// that asks for mature work -- but "the wire cannot express it" is a
// claim about the wire, and this is the claim about the query. An
// implementation that reached for the gate's own inputs, or that
// replaced the gate rather than composing beside it, fails here while
// passing every case above.
func TestMatureViewFilter_CannotWiden(t *testing.T) {
	pool := previewPool(t)
	// Nobody is qualified: the resolver answers for a ref that never
	// calls, so every caller lands on the disqualified viewer.
	h := mvfHandler(t, pool, mvfAuthor+9999)
	c := newMVFCorpus(t, pool, mvfAuthor)

	// mvfOther is neither the author nor an admin, so no exemption
	// applies and the gate alone decides.
	without := mvfFeed(t, h, mvfOther, false, false)
	with := mvfFeed(t, h, mvfOther, true, false)

	mvfAbsent(t, "disqualified, no ?mature= (the MATURE post)", without, c.mature)
	mvfAbsent(t, "⛔ disqualified, ?mature=not_mature (the MATURE post)", with, c.mature)
	mvfPresent(t, "disqualified, no ?mature= (the plain post)", without, c.plain)
	mvfPresent(t, "disqualified, ?mature=not_mature (the plain post)", with, c.plain)
}

// An unrecognised value is REFUSED, not ignored.
//
// Nothing in this stack enforces a query-parameter enum at bind time,
// so an unvalidated `?mature=yes` would fall through as "no filter" and
// hand a reader who asked to drop mature work a wall that still carries
// it. Doing the opposite of what was asked, silently, is worse on this
// axis than an error. Same answer `?ai=junk` gets, for the same reason.
func TestMatureViewFilter_UnknownValueIsRefused(t *testing.T) {
	pool := previewPool(t)
	h := mvfHandler(t, pool, mvfAuthor)
	newMVFCorpus(t, pool, mvfAuthor)

	for _, bad := range []string{"yes", "true", "mature", "exclude", ""} {
		v := bad
		resp := mvfFeedRaw(t, h, mvfAuthor, false, false, &v)
		if _, is := resp.(openapi.ListPosts400JSONResponse); !is {
			t.Errorf("?mature=%q returned %T, want 400 -- a tolerated value would render "+
				"as NO FILTER and quietly keep the mature work on the wall", bad, resp)
		}
	}
}
