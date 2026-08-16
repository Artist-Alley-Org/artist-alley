// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1132 — the post read gate on `GET /posts/{id}/comments`.
//
// # The shape of the assertion, and why it is a PAIR
//
// The bug was `postExists`: a presence check standing in for a read
// rule. A single-caller assertion cannot see that. "Author reads the
// thread on their own private post" passes on the broken handler and on
// the fixed one, and so does "the thread has three comments". What
// separates them is the SAME post read by TWO callers with opposite
// verdicts — so every tier below is driven twice, once by someone the
// rule admits and once by someone it refuses, and the two answers are
// asserted against each other rather than against a constant.
//
// The public arm is not filler. A gate that refuses everyone passes
// every refusal case in this file while collapsing the endpoint into
// "your own posts only", which is a different bug with the same test
// output. `TestCommentGate_PairPerTier` therefore requires the stranger
// to SUCCEED on public and on org-only (the walled-garden default tier
// — an authenticated stranger reads it, and a gate copied from
// `author_user_ref = caller` would fail here), and to be refused on
// private / followers / explicit-share.
//
// # The refusal must be indistinguishable from absence
//
// A 403, a distinct message, or any difference in the response template
// turns the endpoint into a UUID-existence oracle: point it at a
// candidate id and read the status. So the refusal is asserted to be
// byte-identical to what a nonexistent UUID produces, not merely
// "non-200".
//
// # The body, not just the status
//
// Every refusal also asserts that the response carries NO comment
// bodies. A handler that returned 404 alongside a populated payload
// would pass a status-only assertion, and the leak this issue is about
// is the BODIES.
//
// Skips without AA_DB_PASSWORD, like the other social integration tests.

package social

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	cgAuthor   int64 = 11320001 // authors every post + every comment
	cgStranger int64 = 11320002 // authenticated, follows nobody, holds nothing
	cgFollower int64 = 11320003 // follows cgAuthor and nothing else
)

func cgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := cgEnv("AA_DB_HOST", "postgres")
	port := cgEnv("AA_DB_PORT", "5432")
	user := cgEnv("AA_DB_USER", "artist_alley")
	name := cgEnv("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func cgEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func cgHandler(pool *pgxpool.Pool) *Handler {
	return NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func cgCtx(ref int64) context.Context {
	return auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: ref, AuthMethod: "session"})
}

// cgSeedPost plants one post at `tier` with one comment on it, and
// returns the post id. The comment body is unique per post so a leak
// into the wrong response is identifiable rather than merely counted.
func cgSeedPost(t *testing.T, pool *pgxpool.Pool, tier string) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility)
		 VALUES ($1, $2, $3, $4)`,
		postID, cgAuthor, "cg "+tier, tier); err != nil {
		t.Fatalf("seed %s post: %v", tier, err)
	}
	body := "secret conversation about the " + tier + " post"
	commentID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO comments (id, target_kind, target_id, root_id, depth, author_user_ref, body)
		 VALUES ($1, 'post', $2, $1, 0, $3, $4)`,
		commentID, postID, cgAuthor, body); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM comments WHERE target_id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM post_acls WHERE post_id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
	})
	return postID, body
}

// cgList drives the REAL handler, not the query underneath it — the
// hole was in the handler's gate, so a query-level assertion would have
// tested the wrong layer.
func cgList(t *testing.T, h *Handler, ctx context.Context, postID uuid.UUID) openapi.ListPostCommentsResponseObject {
	t.Helper()
	resp, err := h.ListPostComments(ctx, openapi.ListPostCommentsRequestObject{
		Id: openapi_types.UUID(postID),
	})
	if err != nil {
		t.Fatalf("ListPostComments: %v", err)
	}
	return resp
}

// cgBodies pulls every comment body out of a response, whatever its
// shape. A 404 carries none; a 200 carries the thread. Returning the
// BODIES rather than a count is what lets a refusal assert "nothing
// about this conversation reached the caller".
func cgBodies(resp openapi.ListPostCommentsResponseObject) []string {
	ok, is200 := resp.(openapi.ListPostComments200JSONResponse)
	if !is200 {
		return nil
	}
	out := make([]string, 0, len(ok.Items))
	for _, c := range ok.Items {
		out = append(out, c.Body)
	}
	return out
}

// TestCommentGate_PairPerTier is the same-post-opposite-verdicts pair,
// run once per visibility tier the database's own CHECK constraint
// admits. `wantStranger` is the whole point: it differs by tier, so a
// gate that is uniformly wrong in either direction fails somewhere.
func TestCommentGate_PairPerTier(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)

	// The follow edge that makes the `followers` tier decidable. Seeded
	// once; cgFollower follows nobody else.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`, cgFollower, cgAuthor); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref = $1`, cgFollower)
	})

	cases := []struct {
		tier string
		// The AUTHOR always reads their own thread — that arm is the
		// control, and it is what makes a refusal below a gate rather
		// than a broken endpoint.
		wantStranger bool
		wantFollower bool
	}{
		// Readable by everyone signed in, so the gate must NOT refuse.
		{tier: "public", wantStranger: true, wantFollower: true},
		{tier: "org-only", wantStranger: true, wantFollower: true},
		// The three withholding tiers.
		{tier: "private", wantStranger: false, wantFollower: false},
		{tier: "followers", wantStranger: false, wantFollower: true},
		{tier: "explicit-share", wantStranger: false, wantFollower: false},
	}

	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			postID, body := cgSeedPost(t, pool, tc.tier)

			// Arm 1 — the author. Always reads the thread.
			got := cgBodies(cgList(t, h, cgCtx(cgAuthor), postID))
			if len(got) != 1 || got[0] != body {
				t.Fatalf("author on %s: got bodies %q, want exactly [%q] "+
					"(the control arm — a refusal here means the endpoint is broken, not gated)",
					tc.tier, got, body)
			}

			// Arm 2 — the stranger. Opposite verdict on four of five tiers.
			assertArm(t, h, postID, cgCtx(cgStranger), "stranger", tc.tier, tc.wantStranger, body)

			// Arm 3 — the follower. Differs from the stranger on exactly
			// one tier, which is what proves the rule consults the follow
			// graph rather than a tier allow-list.
			assertArm(t, h, postID, cgCtx(cgFollower), "follower", tc.tier, tc.wantFollower, body)
		})
	}
}

func assertArm(
	t *testing.T,
	h *Handler,
	postID uuid.UUID,
	ctx context.Context,
	who, tier string,
	want bool,
	body string,
) {
	t.Helper()
	resp := cgList(t, h, ctx, postID)
	got := cgBodies(resp)
	if want {
		if len(got) != 1 || got[0] != body {
			t.Errorf("%s on %s: got bodies %q, want exactly [%q]", who, tier, got, body)
		}
		if _, ok := resp.(openapi.ListPostComments200JSONResponse); !ok {
			t.Errorf("%s on %s: response is %T, want a 200", who, tier, resp)
		}
		return
	}
	// Refused. The status must be the SAME 404 an absent post produces
	// — asserted against a freshly-minted UUID rather than against a
	// hardcoded literal, so a change to the message keeps the two in
	// step or fails here.
	absent := cgList(t, h, ctx, uuid.New())
	if resp != absent {
		t.Errorf("%s on %s: refusal is %#v, absent-post is %#v — a distinguishable "+
			"refusal makes this endpoint a UUID-existence oracle", who, tier, resp, absent)
	}
	if len(got) != 0 {
		t.Errorf("%s on %s: refusal LEAKED comment bodies %q — the bodies are the payload "+
			"this gate exists to withhold", who, tier, got)
	}
}

// TestCommentGate_AnonymousArm drives the rule's anonymous branch
// directly.
//
// The handler 401s an anonymous caller before the gate runs, so an
// end-to-end assertion here would prove only that the 401 is still
// there — true before the fix too. What actually matters is that the
// gate itself takes the anonymous branch correctly, because that is the
// branch a public-mode allowlist entry would switch on (#709), and it
// must already be right on the day someone adds one. So this asserts
// BOTH: the endpoint's 401, and the gate's own verdict underneath it.
func TestCommentGate_AnonymousArm(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)
	ctx := context.Background()

	publicPost, _ := cgSeedPost(t, pool, "public")
	orgPost, _ := cgSeedPost(t, pool, "org-only")
	privatePost, _ := cgSeedPost(t, pool, "private")

	// The gate, with no identity at all.
	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"public", publicPost, true},
		// org-only is the walled-garden tier: ANY authenticated user,
		// and no anonymous one. This is the arm that separates "the
		// anonymous branch runs" from "the anonymous branch is the
		// authenticated one with a zero ref".
		{"org-only", orgPost, false},
		{"private", privatePost, false},
	} {
		got, err := h.postReadable(ctx, nil, tc.id)
		if err != nil {
			t.Fatalf("postReadable(anonymous, %s): %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("postReadable(anonymous, %s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	// And the endpoint's own answer to an anonymous caller: 401, with
	// no thread, on the PUBLIC post — the one the gate above admits. If
	// comments are ever put on the public-mode allowlist this assertion
	// is the one that has to be revisited deliberately.
	resp := cgList(t, h, ctx, publicPost)
	if _, ok := resp.(openapi.ListPostComments401JSONResponse); !ok {
		t.Errorf("anonymous listing: got %T, want a 401", resp)
	}
	if got := cgBodies(resp); len(got) != 0 {
		t.Errorf("anonymous listing leaked bodies %q", got)
	}
}

// TestCommentGate_ExplicitShareGrant is the fifth tier's positive arm,
// and it is separate because it needs a row rather than a caller.
//
// Without it, `explicit-share` in the table above is indistinguishable
// from "a tier nobody can ever read" — which a gate that simply omitted
// the ACL disjunct would also produce. ADR 0010 L6: an ACL GRANTS,
// never restricts.
func TestCommentGate_ExplicitShareGrant(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)

	postID, body := cgSeedPost(t, pool, "explicit-share")

	// Refused before the grant…
	if got := cgBodies(cgList(t, h, cgCtx(cgStranger), postID)); len(got) != 0 {
		t.Fatalf("pre-grant: stranger read %q", got)
	}

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_acls (post_id, principal_type, principal_id, permission)
		 VALUES ($1, 'user', $2::BIGINT::TEXT, 'read')`,
		postID, cgStranger); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	// …and admitted after it, on the same post, by the same caller.
	got := cgBodies(cgList(t, h, cgCtx(cgStranger), postID))
	if len(got) != 1 || got[0] != body {
		t.Errorf("post-grant: stranger got %q, want exactly [%q] — the ACL disjunct "+
			"is missing from the gate", got, body)
	}
}
