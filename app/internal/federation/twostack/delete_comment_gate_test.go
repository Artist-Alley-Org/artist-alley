// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1572 gate (second slice): a Delete of a comment, from B's production
// comment delete API to the owner's production inbox on A.
//
// B's user comments on A's post (the #1604 path): the Create is built by
// the production emitter and recorded by B's activity writer, and B holds
// the comment as its own local row. B's user then deletes it through B's
// real DELETE /comments/{id}: B's handler binds the Delete to that Create
// (its id and its `to`), B's dispatcher and resolver address it to
// exactly the Create's recipients, B's worker seals it to the same user
// and signs it, and A's inbox and dispatcher validate it (a Delete of a
// comment only), bind it to the comment by peer, signed actor and Create
// id, and soft-delete it. Grants are made and ended through A's real
// admin API.
//
// TEST FIXTURES, stated (as in the Undo gate, undo_like_gate_test.go):
//
//   - B's local comment row is inserted on B's disposable database: no
//     page or API on B can comment on another instance's post yet (#1608).
//   - Each acting user on B gets an API token and comments.delete.own.
//   - Arrival order uses the Undo gate's parking trigger on B.
//   - Deliveries the production sender would never queue (another actor's
//     Delete, a Delete of a foreign object, a Delete of a post) get a
//     delivery row inserted on B, so B's own worker signs and sends them.
//   - A wrong-peer Delete and the concurrent-write check call A's
//     production social write directly on A's disposable database.
//   - A transient failure is a trigger on A's disposable database that
//     refuses a comment update while a flag row exists.

package twostack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/social"
)

type deleteGate struct {
	*undoGate
	tokens map[int64]string
}

func newDeleteGate(t *testing.T) *deleteGate {
	t.Helper()
	// bob's and eve's keys are bootstrapped on both sides by the like gate.
	return &deleteGate{undoGate: newUndoGate(t), tokens: map[int64]string{}}
}

// tokenFor gives a B user an API token, comments.delete.own and
// posts.comment (the capability an edit needs, #1572).
func (g *deleteGate) tokenFor(a likeActor) string {
	g.t.Helper()
	if tok, ok := g.tokens[a.ref]; ok {
		return tok
	}
	ctx := context.Background()
	if _, err := g.b.pool.Exec(ctx, `INSERT INTO user_capability_grants (user_ref, capability_code)
		VALUES ($1, 'comments.delete.own'), ($1, 'posts.comment')`, a.ref); err != nil {
		g.t.Fatal(err)
	}
	tok, _ := auth.NewAPIToken()
	if _, err := g.b.pool.Exec(ctx, `INSERT INTO api_tokens (user_ref, name, token_hash) VALUES ($1, 'delete gate', $2)`,
		a.ref, auth.HashAPIToken(tok)); err != nil {
		g.t.Fatal(err)
	}
	g.tokens[a.ref] = tok
	return tok
}

// apiAs calls in's API as token's user, with body as JSON when given,
// and returns the status.
func (g *deleteGate) apiAs(in *instance, token, method, path string, body ...any) int {
	g.t.Helper()
	var rd io.Reader
	if len(body) > 0 {
		b, _ := json.Marshal(body[0])
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, in.url+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: g.tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// commentOnB stores a's comment on target as B's own local row and
// records its Create through B's production emitter and writer, parked
// when hold is set. Returns the comment id and the Create id.
func (g *deleteGate) commentOnB(a likeActor, post uuid.UUID, target string, hold bool) (uuid.UUID, string) {
	g.t.Helper()
	id := uuid.New()
	if _, err := g.b.pool.Exec(context.Background(), `INSERT INTO comments (id, root_id, target_kind, target_id, body, author_user_ref)
		VALUES ($1, $1, 'post', $2, 'a comment to delete', $3)`, id, post, a.ref); err != nil {
		g.t.Fatal(err)
	}
	em := emit.CreateComment(g.actorCtx(a), emit.CommentRef{ID: id.String(), PostID: post.String(), PostTitle: "Liked post",
		PostAuthorURI: g.adminURIOnA, Body: "a comment to delete", TargetURI: target})
	return id, g.record(em, hold)
}

// deleteOnB deletes comment through B's real API as a, and returns the
// Delete activity id B recorded.
func (g *deleteGate) deleteOnB(a likeActor, comment uuid.UUID) string {
	g.t.Helper()
	if st := g.apiAs(g.b, g.tokenFor(a), http.MethodDelete, "/comments/"+comment.String()); st != http.StatusNoContent {
		g.t.Fatalf("DELETE /comments/%s as %s: %d", comment, a.name, st)
	}
	var id string
	if err := g.b.pool.QueryRow(context.Background(), `SELECT activity_uri FROM activities
		WHERE activity_type = 'Delete' AND object_local_id = $1 ORDER BY created_at DESC LIMIT 1`, comment.String()).Scan(&id); err != nil {
		g.t.Fatal(err)
	}
	return id
}

// commentOnA is A's comment for a Create id: "live", "deleted" or "".
func (g *deleteGate) commentOnA(create string) string {
	g.t.Helper()
	var deleted bool
	var body string
	err := g.a.pool.QueryRow(context.Background(), `SELECT deleted_at IS NOT NULL, body FROM comments WHERE activity_uri = $1`, create).Scan(&deleted, &body)
	if err != nil {
		return ""
	}
	if deleted {
		if body != "" {
			return "deleted-with-body"
		}
		return "deleted"
	}
	return "live"
}

// wantComment checks A's comment state, the post's comment_count and the
// post author's comment notifications.
func (g *deleteGate) wantComment(t *testing.T, post uuid.UUID, create, state string, count, notes int) {
	t.Helper()
	got := g.commentOnA(create)
	c := g.countOn(g.a, `SELECT comment_count FROM posts WHERE id = $1`, post)
	n := g.countOn(g.a, `SELECT count(*) FROM notifications WHERE verb = 'post.comment' AND target_id = $1`, post.String())
	if got != state || c != count || n != notes {
		t.Fatalf("A holds comment %q, comment_count %d, %d notifications; want %q, %d, %d", got, c, n, state, count, notes)
	}
}

func (g *deleteGate) tombstoneOnA(actorURI, create string) bool {
	g.t.Helper()
	return g.countOn(g.a, `SELECT count(*) FROM federation_remote_comment_tombstones
		WHERE peer_id = $1 AND actor_uri = $2 AND create_activity_uri = $3`, g.peerBonA, actorURI, create) == 1
}

func TestTwoStack_DeleteComment(t *testing.T) {
	g := newDeleteGate(t)
	ctx := context.Background()
	onA := func(t *testing.T, activity, want string) {
		t.Helper()
		if got := g.onA(activity); got != want {
			t.Fatalf("%s on A: %s, want %s", activity, got, want)
		}
	}

	t.Run("the author deletes through B's API: soft-deleted, count down, notification kept; a replay and a second Delete change nothing", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")
		g.wantComment(t, post, create, "live", 1, 1)

		del := g.deleteOnB(g.bob, comment)
		onA(t, del, "processed")
		g.wantComment(t, post, create, "deleted", 0, 1)
		// Sent where the Create went, sealed to the same user; A got the
		// Create id as the object.
		if c, d := g.outboxOnB(create), g.outboxOnB(del); c != d || c != "sent|"+g.adminURIOnA+"|unresolved|encrypted=true" {
			t.Fatalf("delivery rows: Create %q, Delete %q", c, d)
		}
		var object string
		if err := g.a.pool.QueryRow(ctx, `SELECT envelope_json->>'object' FROM federation_inbox WHERE activity_uri = $1`, del).Scan(&object); err != nil || object != create {
			t.Fatalf("A received object %q (%v), want the Create id", object, err)
		}

		// Exact replay: answered from A's stored row.
		var before time.Time
		if err := g.b.pool.QueryRow(ctx, `UPDATE federation_outbox SET status = 'queued', next_attempt_at = now()
			WHERE activity_id = (SELECT id FROM activities WHERE activity_uri = $1) RETURNING last_attempt_at`, del).Scan(&before); err != nil {
			t.Fatal(err)
		}
		g.release()
		deadline := time.Now().Add(90 * time.Second)
		for {
			var st string
			var last time.Time
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status, o.last_attempt_at FROM federation_outbox o
				JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, del).Scan(&st, &last)
			if st == "sent" && last.After(before) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("B never redelivered the Delete (%q)", st)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, del); n != 1 {
			t.Fatalf("%d inbox rows for one Delete", n)
		}
		// A second, distinct Delete (B's API answers 404 for a deleted
		// comment, so the production emitter builds it).
		again := g.record(emit.DeleteComment(g.actorCtx(g.bob), comment.String(), post.String(), create, []string{g.adminURIOnA}), false)
		onA(t, again, "processed")
		g.wantComment(t, post, create, "deleted", 0, 1)
	})

	t.Run("a Delete before its Create: the Create arrives and creates nothing", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, true)
		g.parked(create)
		del := g.deleteOnB(g.bob, comment)
		onA(t, del, "processed")
		if !g.tombstoneOnA(g.bob.uri, create) {
			t.Fatal("A recorded no tombstone for the Delete")
		}
		g.release(create)
		onA(t, create, "processed")
		g.wantComment(t, post, create, "", 0, 0)
	})

	t.Run("a comment a moderator on A already deleted: local only on A, then the author's Delete is a no-op", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")
		var local uuid.UUID
		if err := g.a.pool.QueryRow(ctx, `SELECT id FROM comments WHERE activity_uri = $1`, create).Scan(&local); err != nil {
			t.Fatal(err)
		}
		if st := g.api(g.a, http.MethodDelete, "/comments/"+local.String(), nil, nil); st != http.StatusNoContent {
			t.Fatalf("A's moderator delete: %d", st)
		}
		g.wantComment(t, post, create, "deleted", 0, 1)
		var modDelete string
		if err := g.a.pool.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE activity_type = 'Delete' AND object_local_id = $1`,
			local.String()).Scan(&modDelete); err != nil {
			t.Fatal(err)
		}
		var reason string
		deadline := time.Now().Add(60 * time.Second)
		for reason == "" && time.Now().Before(deadline) {
			_ = g.a.pool.QueryRow(ctx, `SELECT e.metadata->>'reason' FROM audit_events e JOIN activities a ON a.id::text = e.metadata->>'activity_id'
				WHERE e.event_type = 'federation.emission.skipped' AND a.activity_uri = $1`, modDelete).Scan(&reason)
			time.Sleep(50 * time.Millisecond)
		}
		if reason != "activity_not_federated" || g.countOn(g.a, `SELECT count(*) FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
			WHERE a.activity_uri = $1`, modDelete) != 0 {
			t.Fatalf("A's moderator Delete of a remote comment: skipped %q", reason)
		}
		onA(t, g.deleteOnB(g.bob, comment), "processed")
		g.wantComment(t, post, create, "deleted", 0, 1)
	})

	t.Run("after a revoke a new comment is refused, and the Delete still applies", func(t *testing.T) {
		post, uri := g.postOnA()
		sid := g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")
		g.revokeOnA(sid, uri, "")
		_, late := g.commentOnB(g.bob, post, uri, false)
		onA(t, late, "rejected/unshared_object")
		onA(t, g.deleteOnB(g.bob, comment), "processed")
		g.wantComment(t, post, create, "deleted", 0, 1)
	})

	t.Run("a deleted target: the Delete still applies", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")
		if _, err := g.a.pool.Exec(ctx, `UPDATE posts SET deleted_at = now() WHERE id = $1`, post); err != nil {
			t.Fatal(err)
		}
		onA(t, g.deleteOnB(g.bob, comment), "processed")
		g.wantComment(t, post, create, "deleted", 0, 1)
	})

	t.Run("another actor, peer or origin cannot delete the comment, and every answer is the same", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")

		// B never sends eve's Delete naming bob's Create: her Create was
		// queued for nobody.
		eveDel := g.record(emit.DeleteComment(g.actorCtx(g.eve), comment.String(), post.String(), create, []string{g.adminURIOnA}), false)
		if got := g.skippedOnB(eveDel); got != "recipient_set_empty" {
			t.Fatalf("eve's Delete of bob's comment: skipped %q", got)
		}
		g.forceSend(eveDel)
		onA(t, eveDel, "processed")
		unseen := g.record(emit.DeleteComment(g.actorCtx(g.bob), uuid.NewString(), post.String(),
			g.b.url+"/activities/"+uuid.NewString(), []string{g.adminURIOnA}), false)
		if got := g.skippedOnB(unseen); got != "recipient_set_empty" {
			t.Fatalf("a Delete of an unknown Create: skipped %q", got)
		}
		g.forceSend(unseen)
		onA(t, unseen, "processed")
		if a, b := g.outboxOnB(eveDel), g.outboxOnB(unseen); a != b {
			t.Fatalf("receipts differ: eve %q, unseen %q", a, b)
		}
		g.wantComment(t, post, create, "live", 1, 1)
		if !g.tombstoneOnA(g.eve.uri, create) {
			t.Fatal("eve's Delete stored no tombstone of her own")
		}

		// A Create id on another origin is refused at receipt.
		foreign := g.record(emit.DeleteComment(g.actorCtx(g.bob), comment.String(), post.String(),
			"https://elsewhere.example/activities/"+uuid.NewString(), []string{g.adminURIOnA}), false)
		g.skippedOnB(foreign)
		g.forceSend(foreign)
		deadline := time.Now().Add(60 * time.Second)
		for {
			var st string
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
				WHERE a.activity_uri = $1`, foreign).Scan(&st)
			if st == "failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the foreign-object Delete never failed (%q)", st)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, foreign); n != 0 {
			t.Fatal("A stored a Delete of a foreign object")
		}

		// Another peer of A naming bob's Create id (A's production write).
		var other uuid.UUID
		if err := g.a.pool.QueryRow(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
			trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
			VALUES ($1, 'C', 'x', 'connected', 'plaintext', TRUE, 'connected', $2) RETURNING id`,
			"https://c-"+uuid.NewString()[:8]+".example", g.a.admin).Scan(&other); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = g.a.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, other)
		})
		deleted, err := social.NewHandler(g.a.pool, g.logger, nil).DeleteRemoteComment(ctx, social.RemoteDeleteCommentInput{
			PeerID: other, ActorURI: g.bob.uri, CreateActivityURI: create, DeleteActivityURI: "https://c.example/activities/d"})
		if err != nil || deleted {
			t.Fatalf("another peer's Delete: deleted %v, %v", deleted, err)
		}
		g.wantComment(t, post, create, "live", 1, 1)
	})

	t.Run("a moderator's Delete on B and a Delete of a post stay local, and are never applied", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")
		// B's admin deletes bob's comment: local only, never sent as bob
		// or as the admin.
		if st := g.api(g.b, http.MethodDelete, "/comments/"+comment.String(), nil, nil); st != http.StatusNoContent {
			t.Fatalf("B's moderator delete: %d", st)
		}
		var mod string
		if err := g.b.pool.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE activity_type = 'Delete' AND object_local_id = $1`,
			comment.String()).Scan(&mod); err != nil {
			t.Fatal(err)
		}
		if got := g.skippedOnB(mod); got != "activity_not_federated" {
			t.Fatalf("B's moderator Delete: skipped %q", got)
		}
		g.wantComment(t, post, create, "live", 1, 1)

		postDel := g.record(emit.DeletePost(g.actorCtx(g.bob), uuid.NewString(), "x"), false)
		if got := g.skippedOnB(postDel); got != "activity_not_federated" {
			t.Fatalf("a Delete of a post: skipped %q", got)
		}
		g.forceSend(postDel)
		deadline := time.Now().Add(60 * time.Second)
		for {
			var st string
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
				WHERE a.activity_uri = $1`, postDel).Scan(&st)
			if st == "failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("B's row for a Delete of a post never closed (%q)", st)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, postDel); n != 0 {
			t.Fatal("a Delete of a post reached A")
		}
	})

	t.Run("a transient failure at A is retried, then applied", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")
		for _, sql := range []string{
			`CREATE TABLE IF NOT EXISTS test_fail_comment_update (x int)`,
			`INSERT INTO test_fail_comment_update VALUES (1)`,
			`CREATE OR REPLACE FUNCTION test_fail_comment_update() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF EXISTS (SELECT 1 FROM test_fail_comment_update) THEN
					RAISE EXCEPTION 'test: comment update refused';
				END IF;
				RETURN NEW;
			END $$`,
			`CREATE TRIGGER test_fail_comment_update BEFORE UPDATE ON comments FOR EACH ROW EXECUTE FUNCTION test_fail_comment_update()`,
		} {
			if _, err := g.a.pool.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			for _, sql := range []string{`DROP TRIGGER IF EXISTS test_fail_comment_update ON comments`,
				`DROP FUNCTION IF EXISTS test_fail_comment_update()`, `DROP TABLE IF EXISTS test_fail_comment_update`} {
				_, _ = g.a.pool.Exec(context.Background(), sql)
			}
		})
		del := g.deleteOnB(g.bob, comment)
		deadline := time.Now().Add(60 * time.Second)
		for {
			var st, lastErr string
			var attempts int
			_ = g.a.pool.QueryRow(ctx, `SELECT status, dispatch_attempts, last_error FROM federation_inbox WHERE activity_uri = $1`,
				del).Scan(&st, &attempts, &lastErr)
			if st == "pending" && attempts >= 1 && lastErr != "" {
				break
			}
			if st != "" && st != "pending" {
				t.Fatalf("the Delete settled %q during a database failure; want it pending for retry", st)
			}
			if time.Now().After(deadline) {
				t.Fatalf("A never attempted the Delete (%q, %d)", st, attempts)
			}
			time.Sleep(50 * time.Millisecond)
		}
		g.wantComment(t, post, create, "live", 1, 1)
		if _, err := g.a.pool.Exec(ctx, `DELETE FROM test_fail_comment_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := g.a.pool.Exec(ctx, `SELECT pg_notify('federation_inbox_pending', '')`); err != nil {
			t.Fatal(err)
		}
		onA(t, del, "processed")
		g.wantComment(t, post, create, "deleted", 0, 1)
	})

	t.Run("concurrent Create and Delete never leave a live comment", func(t *testing.T) {
		// Two-instance: the Create and its Delete released at once.
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, true)
		del := g.record(emit.DeleteComment(g.actorCtx(g.bob), comment.String(), post.String(), create, []string{g.adminURIOnA}), true)
		g.parked(create)
		g.parked(del)
		g.release(create, del)
		onA(t, create, "processed")
		onA(t, del, "processed")
		if got := g.commentOnA(create); got != "" && got != "deleted" {
			t.Fatalf("after a concurrent Create and Delete A holds %q", got)
		}
		if c := g.countOn(g.a, `SELECT comment_count FROM posts WHERE id = $1`, post); c != 0 {
			t.Fatalf("comment_count %d", c)
		}

		// A's production write, driven concurrently: a Create and its
		// Delete at once, for many rounds. The authorizer admits all: what
		// is checked is the comment and tombstone invariant.
		sh := social.NewHandler(g.a.pool, g.logger, nil)
		allow := func(context.Context, pgx.Tx) error { return nil }
		for round := 0; round < 25; round++ {
			p, _ := g.postOnA()
			cr := fmt.Sprintf("%s/activities/cc%d-%s", g.b.url, round, uuid.NewString()[:8])
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				if _, _, err := sh.InsertRemoteComment(ctx, social.RemoteCommentInput{TargetKind: "post", TargetID: p,
					PeerID: g.peerBonA, ActorURI: g.bob.uri, ActivityURI: cr, Body: "racing", Authorize: allow}); err != nil {
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				if _, err := sh.DeleteRemoteComment(ctx, social.RemoteDeleteCommentInput{PeerID: g.peerBonA, ActorURI: g.bob.uri,
					CreateActivityURI: cr, DeleteActivityURI: cr + "/delete"}); err != nil {
					errs <- err
				}
			}()
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("round %d: %v", round, err)
			}
			live := g.countOn(g.a, `SELECT count(*) FROM comments WHERE activity_uri = $1 AND deleted_at IS NULL`, cr)
			count := g.countOn(g.a, `SELECT comment_count FROM posts WHERE id = $1`, p)
			rows := g.countOn(g.a, `SELECT count(*) FROM comments WHERE activity_uri = $1`, cr)
			tomb := g.countOn(g.a, `SELECT count(*) FROM federation_remote_comment_tombstones WHERE create_activity_uri = $1`, cr)
			// Either the Delete won (a tombstone, no comment) or the Create
			// did (a deleted comment, no tombstone); never a live comment.
			if live != 0 || count != 0 || rows+tomb != 1 {
				t.Fatalf("round %d: %d live, comment_count %d, %d rows, %d tombstones", round, live, count, rows, tomb)
			}
		}
	})

	t.Run("a comment on a local post: its Delete has nobody to go to", func(t *testing.T) {
		local := uuid.New()
		if _, err := g.b.pool.Exec(ctx, `INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Local', 'public')`,
			local, g.b.admin); err != nil {
			t.Fatal(err)
		}
		comment, create := g.commentOnB(g.bob, local, "", false)
		if got := g.skippedOnB(create); got != "recipient_set_empty" {
			t.Fatalf("a comment on a local post: skipped %q", got)
		}
		del := g.deleteOnB(g.bob, comment)
		if got := g.skippedOnB(del); got != "recipient_set_empty" {
			t.Fatalf("its Delete: skipped %q", got)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri IN ($1, $2)`, create, del); n != 0 {
			t.Fatal("a local comment or its Delete reached A")
		}
	})
}
