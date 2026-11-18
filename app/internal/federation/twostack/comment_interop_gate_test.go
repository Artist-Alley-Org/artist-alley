// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1604 gate: a comment Create from one production instance applied by
// another, end to end.
//
// B's user comments on A's post. The activity is built by the
// production emitter (emit.CreateComment) and recorded by the
// production activity writer on B; B's own outbox dispatcher, resolver
// and delivery worker address it to A (the target's origin), encrypt it
// to the post author and sign it; A's own inbox and dispatcher decrypt,
// validate, authorise against A's share (#1570) and write it.
//
// LIMITATION, stated: B has no representation of A's post (received
// objects are #1574), so no UI or HTTP comment form on B can name it yet.
// The test calls the production emitter with A's post URI as the
// explicit target (CommentRef.TargetURI). No UI interoperability is
// claimed.
//
// TEST-ONLY key bootstrap: each user's own encryption key and each
// side's cache of the other's public key are seeded (a comment has no
// sensitivity tier of its own, so it is sent encrypted or not at all,
// and key exchange is not #1604). Everything else runs as deployed.

package twostack_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

type commentGate struct {
	*cluster
	peerAonB, peerBonA uuid.UUID
	adminURIOnA        string
	bob                int64
	bobName, bobURI    string
	writer             *activities.Writer
}

// encKey gives a local user a current encryption key and returns its
// public half (TEST-ONLY bootstrap, see the file comment).
func encKey(t *testing.T, in *instance, userRef int64) []byte {
	t.Helper()
	pub, wrapped, err := userkeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.pool.Exec(context.Background(),
		`INSERT INTO federation_user_keys (user_ref, version, algorithm, public_key, private_key_enc, is_current)
		 VALUES ($1, 1, 'naclbox-x25519-v1', $2, $3, TRUE)`, userRef, pub, wrapped); err != nil {
		t.Fatal(err)
	}
	return pub
}

// cacheRemoteKey records another instance's actor public key in in's
// remote-actor cache (TEST-ONLY bootstrap).
func cacheRemoteKey(t *testing.T, in *instance, peerID uuid.UUID, actorURI string, pub []byte) {
	t.Helper()
	if _, err := in.pool.Exec(context.Background(),
		`INSERT INTO federation_remote_actors (actor_uri, peer_id, encryption_public_key, encryption_public_key_version, encryption_public_key_updated_at)
		 VALUES ($1, $2, $3, 1, now())
		 ON CONFLICT (actor_uri) DO UPDATE SET encryption_public_key = EXCLUDED.encryption_public_key,
		   encryption_public_key_version = 1, encryption_public_key_updated_at = now()`, actorURI, peerID, pub); err != nil {
		t.Fatal(err)
	}
}

func newCommentGate(t *testing.T) *commentGate {
	t.Helper()
	c := newCluster(t)
	ctx := context.Background()
	g := &commentGate{cluster: c, peerAonB: c.peerID(c.b, c.a), peerBonA: c.peerID(c.a, c.b),
		writer: activities.NewWriter(c.b.pool, c.logger, nil)}
	var adminName string
	if err := c.a.pool.QueryRow(ctx, `SELECT username FROM "user" WHERE ref = $1`, c.a.admin).Scan(&adminName); err != nil {
		t.Fatal(err)
	}
	g.adminURIOnA = c.a.url + "/users/" + adminName
	g.bobName = "bob" + uuid.NewString()[:6]
	if err := c.b.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Bob', 1) RETURNING ref`,
		g.bobName).Scan(&g.bob); err != nil {
		t.Fatal(err)
	}
	if _, err := users.NewHandler(c.b.pool, c.logger, nil).EnsureActorKeyMaterial(ctx, g.bob, c.b.url, g.bobName); err != nil {
		t.Fatal(err)
	}
	g.bobURI = c.b.url + "/users/" + g.bobName
	adminPub := encKey(t, c.a, c.a.admin)
	bobPub := encKey(t, c.b, g.bob)
	cacheRemoteKey(t, c.b, g.peerAonB, g.adminURIOnA, adminPub)
	cacheRemoteKey(t, c.a, g.peerBonA, g.bobURI, bobPub)
	c.runWorkers(c.a)
	c.runWorkers(c.b)
	return g
}

func (g *commentGate) postOnA() (uuid.UUID, string) {
	g.t.Helper()
	id := uuid.New()
	if _, err := g.a.pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Shared post', 'explicit-share')`, id, g.a.admin); err != nil {
		g.t.Fatal(err)
	}
	return id, g.a.url + "/posts/" + id.String()
}

func (g *commentGate) shareOnA(post uuid.UUID, scope string) string {
	g.t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	if st := g.api(g.a, http.MethodPost, "/admin/federation/shares",
		map[string]any{"object_kind": "post", "object_id": post, "peer_id": g.peerBonA, "scope": scope}, &out); st != 201 {
		g.t.Fatalf("share: %d", st)
	}
	return out.ID
}

// comment emits bob's comment on target through the production
// emitter and activity writer on B, and returns the Create activity id.
func (g *commentGate) comment(post uuid.UUID, target, parentURI, body string) string {
	g.t.Helper()
	em := emit.CreateComment(emit.ActorContext{UserRef: g.bob, Username: g.bobName, BaseURL: g.b.url}, emit.CommentRef{
		ID: uuid.NewString(), PostID: post.String(), PostTitle: "Shared post", PostAuthorURI: g.adminURIOnA,
		Body: body, TargetURI: target, ParentURI: parentURI,
	})
	if err := pgx.BeginFunc(context.Background(), g.b.pool, func(tx pgx.Tx) error {
		_, err := g.writer.RecordActivity(context.Background(), tx, em.Activity)
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return em.Activity.ActivityURI
}

// onA waits for A's inbox to settle activity and returns status/reason.
func (g *commentGate) onA(activity string) string {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		var reason *string
		if err := g.a.pool.QueryRow(context.Background(),
			`SELECT status, reject_reason FROM federation_inbox WHERE activity_uri = $1`, activity).Scan(&status, &reason); err == nil && status != "pending" {
			if reason != nil {
				return status + "/" + *reason
			}
			return status
		}
		time.Sleep(50 * time.Millisecond)
	}
	var st, lastErr string
	_ = g.b.pool.QueryRow(context.Background(), `SELECT o.status, o.last_error FROM federation_outbox o
		JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, activity).Scan(&st, &lastErr)
	g.t.Fatalf("A never settled %s (B outbox: %s %q)", activity, st, lastErr)
	return ""
}

func (g *commentGate) outboxStatus(activity string) string {
	var st string
	_ = g.b.pool.QueryRow(context.Background(), `SELECT o.status FROM federation_outbox o
		JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, activity).Scan(&st)
	return st
}

func TestTwoStack_CommentInterop(t *testing.T) {
	g := newCommentGate(t)
	ctx := context.Background()
	post, postURI := g.postOnA()
	g.shareOnA(post, "comment")

	var topID uuid.UUID
	top := g.comment(post, postURI, "", "Nice lighting.")
	t.Run("top-level comment lands on A under a comment share", func(t *testing.T) {
		if got := g.onA(top); got != "processed" {
			t.Fatalf("top-level: %s", got)
		}
		var parent *uuid.UUID
		var depth int
		var actor, body string
		if err := g.a.pool.QueryRow(ctx, `SELECT id, parent_id, depth, actor_uri, body FROM comments
			WHERE activity_uri = $1 AND target_kind = 'post' AND target_id = $2`, top, post).Scan(&topID, &parent, &depth, &actor, &body); err != nil {
			t.Fatalf("comment row on A: %v", err)
		}
		if parent != nil || depth != 0 || actor != g.bobURI || body != "Nice lighting." {
			t.Fatalf("row: parent %v depth %d actor %s body %q", parent, depth, actor, body)
		}
	})

	t.Run("reply names its parent by the parent's Create id", func(t *testing.T) {
		reply := g.comment(post, postURI, top, "Agreed.")
		if got := g.onA(reply); got != "processed" {
			t.Fatalf("reply: %s", got)
		}
		var parent uuid.UUID
		var depth int
		if err := g.a.pool.QueryRow(ctx, `SELECT parent_id, depth FROM comments WHERE activity_uri = $1`, reply).Scan(&parent, &depth); err != nil {
			t.Fatal(err)
		}
		if parent != topID || depth != 1 {
			t.Fatalf("reply parent %s depth %d, want %s depth 1", parent, depth, topID)
		}
	})

	t.Run("exactly one notification per comment, also after a redelivery", func(t *testing.T) {
		// B sends the top-level Create again: its row back to queued,
		// then delivered a second time by B's own worker (its 30s
		// ticker backstop picks it up; no INSERT wakes it).
		var before time.Time
		if err := g.b.pool.QueryRow(ctx, `UPDATE federation_outbox SET status = 'queued', next_attempt_at = now()
			WHERE activity_id = (SELECT id FROM activities WHERE activity_uri = $1) RETURNING last_attempt_at`, top).Scan(&before); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			var st string
			var last time.Time
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status, o.last_attempt_at FROM federation_outbox o
				JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, top).Scan(&st, &last)
			if st == "sent" && last.After(before) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("B never redelivered the Create (status %q, last attempt %s, before %s)", st, last, before)
			}
			time.Sleep(100 * time.Millisecond)
		}
		var rows, notes int
		_ = g.a.pool.QueryRow(ctx, `SELECT count(*) FROM comments WHERE activity_uri = $1`, top).Scan(&rows)
		_ = g.a.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE recipient_user_ref = $1 AND verb = 'post.comment'
			AND target_id = $2`, g.a.admin, post.String()).Scan(&notes)
		if rows != 1 || notes != 2 {
			t.Fatalf("after a redelivery: %d rows for the top-level comment, %d notifications (want 1 and 2: one per comment)", rows, notes)
		}
	})

	t.Run("a reply to a parent A has not received, or on another target, is refused", func(t *testing.T) {
		// Reordered: the parent's Create id was never delivered.
		unseen := g.b.url + "/activities/" + uuid.NewString()
		r1 := g.comment(post, postURI, unseen, "early reply")
		if got := g.onA(r1); got != "rejected/unknown_object" {
			t.Fatalf("reply to an unseen parent: %s", got)
		}
		// Cross-target: the parent is a comment on another post.
		other, otherURI := g.postOnA()
		g.shareOnA(other, "comment")
		elsewhere := g.comment(other, otherURI, "", "on the other post")
		if got := g.onA(elsewhere); got != "processed" {
			t.Fatalf("other post comment: %s", got)
		}
		r2 := g.comment(post, postURI, elsewhere, "cross-target reply")
		if got := g.onA(r2); got != "rejected/unknown_object" {
			t.Fatalf("cross-target reply: %s", got)
		}
		var n int
		_ = g.a.pool.QueryRow(ctx, `SELECT count(*) FROM comments WHERE activity_uri IN ($1, $2)`, r1, r2).Scan(&n)
		if n != 0 {
			t.Fatalf("%d refused replies were written (re-parented?)", n)
		}
	})

	t.Run("view share, revoked share: refused; private and missing targets get the same receipt", func(t *testing.T) {
		viewOnly, viewURI := g.postOnA()
		g.shareOnA(viewOnly, "view")
		c1 := g.comment(viewOnly, viewURI, "", "under view")
		if got := g.onA(c1); got != "rejected/unshared_object" {
			t.Fatalf("view share: %s", got)
		}
		revoked, revokedURI := g.postOnA()
		sid := g.shareOnA(revoked, "comment")
		if st := g.api(g.a, http.MethodDelete, "/admin/federation/shares/"+sid, nil, nil); st != 204 {
			t.Fatalf("revoke: %d", st)
		}
		c2 := g.comment(revoked, revokedURI, "", "after revoke")
		if got := g.onA(c2); got != "rejected/unshared_object" {
			t.Fatalf("revoked share: %s", got)
		}
		private, privateURI := g.postOnA() // never shared
		missing := uuid.New()
		c3 := g.comment(private, privateURI, "", "private")
		c4 := g.comment(missing, g.a.url+"/posts/"+missing.String(), "", "missing")
		if got := g.onA(c3); got != "rejected/unshared_object" {
			t.Fatalf("private: %s", got)
		}
		if got := g.onA(c4); got != "rejected/unknown_object" {
			t.Fatalf("missing: %s", got)
		}
		// B's delivery saw the same answer for both: sent (A's 202).
		if a, b := g.outboxStatus(c3), g.outboxStatus(c4); a != "sent" || b != a {
			t.Fatalf("sender receipts differ: private %q, missing %q", a, b)
		}
	})

	t.Run("a target on no paired instance is never sent", func(t *testing.T) {
		stray := g.comment(post, "https://elsewhere.invalid/posts/"+uuid.NewString(), "", "nowhere")
		time.Sleep(2 * time.Second)
		if st := g.outboxStatus(stray); st != "" {
			t.Fatalf("queued for a peer: %q", st)
		}
	})
}
