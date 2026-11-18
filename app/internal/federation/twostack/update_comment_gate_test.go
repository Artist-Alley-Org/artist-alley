// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1572 gate (third slice): an edit (Update) of a comment, from B's
// production comment edit API to the owner's production inbox on A.
//
// B's user comments on A's post (as in the Delete gate) and edits it
// through B's real PATCH /comments/{id}: B's handler binds the Update to
// the comment's Create (its id and `to`) with a published time strictly
// after the comment's previous edit, B's dispatcher and resolver address
// it to exactly the Create's recipients, B's worker seals and signs it,
// and A's inbox and dispatcher validate it (an Update of a comment only),
// bind it to the comment by peer, signed actor and Create id, authorise
// it against A's share at comment scope, and apply it in published order.
//
// TEST FIXTURES, stated (as in the Delete gate): B's local comment row is
// a fixture (#1608); arrival order uses the parking trigger; deliveries
// the production sender would never queue get a delivery row on B; a
// wrong-peer Update and the concurrent-write check call A's production
// social write directly; a transient failure is a trigger on A that
// refuses comment updates while a flag row exists.

package twostack_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/social"
)

// editOnB edits comment through B's real API as a and returns the Update
// activity id B recorded.
func (g *deleteGate) editOnB(a likeActor, comment uuid.UUID, body string) string {
	g.t.Helper()
	if st := g.apiAs(g.b, g.tokenFor(a), http.MethodPatch, "/comments/"+comment.String(), map[string]string{"body": body}); st != http.StatusOK {
		g.t.Fatalf("PATCH /comments/%s as %s: %d", comment, a.name, st)
	}
	var id string
	if err := g.b.pool.QueryRow(context.Background(), `SELECT activity_uri FROM activities
		WHERE activity_type = 'Update' AND object_local_id = $1 ORDER BY published_at DESC LIMIT 1`, comment.String()).Scan(&id); err != nil {
		g.t.Fatal(err)
	}
	return id
}

// updateEm is the production emitter's Update of comment, naming create.
func (g *deleteGate) updateEm(a likeActor, comment, post uuid.UUID, create, body string, published time.Time) emit.Emission {
	return emit.UpdateComment(g.actorCtx(a), comment.String(), post.String(), create, []string{g.adminURIOnA}, body, published)
}

// textOnA is A's comment for a Create id: state ("live", "deleted", "")
// and body.
func (g *deleteGate) textOnA(create string) (string, string, bool) {
	g.t.Helper()
	var deleted, edited bool
	var body string
	err := g.a.pool.QueryRow(context.Background(), `SELECT deleted_at IS NOT NULL, body, edited_at IS NOT NULL FROM comments WHERE activity_uri = $1`,
		create).Scan(&deleted, &body, &edited)
	if err != nil {
		return "", "", false
	}
	if deleted {
		return "deleted", body, edited
	}
	return "live", body, edited
}

func (g *deleteGate) wantText(t *testing.T, create, state, body string) {
	t.Helper()
	if s, b, _ := g.textOnA(create); s != state || b != body {
		t.Fatalf("A holds comment %q with body %q; want %q with %q", s, b, state, body)
	}
}

func (g *deleteGate) pendingOnA(actorURI, create string) int {
	return g.countOn(g.a, `SELECT count(*) FROM federation_remote_comment_pending_edits
		WHERE peer_id = $1 AND actor_uri = $2 AND create_activity_uri = $3`, g.peerBonA, actorURI, create)
}

func TestTwoStack_UpdateComment(t *testing.T) {
	g := newDeleteGate(t)
	ctx := context.Background()
	onA := func(t *testing.T, activity, want string) {
		t.Helper()
		if got := g.onA(activity); got != want {
			t.Fatalf("%s on A: %s, want %s", activity, got, want)
		}
	}
	newComment := func(t *testing.T, hold bool) (uuid.UUID, uuid.UUID, string) {
		t.Helper()
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, hold)
		return post, comment, create
	}

	t.Run("the author edits through B's API: new text and edited, count and notification unchanged; a replay and a second edit", func(t *testing.T) {
		post, comment, create := newComment(t, false)
		onA(t, create, "processed")
		upd := g.editOnB(g.bob, comment, "first edit")
		onA(t, upd, "processed")
		g.wantText(t, create, "live", "first edit")
		if _, _, edited := g.textOnA(create); !edited {
			t.Fatal("A's comment is not marked edited")
		}
		g.wantComment(t, post, create, "live", 1, 1)
		if c, u := g.outboxOnB(create), g.outboxOnB(upd); c != u || c != "sent|"+g.adminURIOnA+"|unresolved|encrypted=true" {
			t.Fatalf("delivery rows: Create %q, Update %q", c, u)
		}
		var object string
		if err := g.a.pool.QueryRow(ctx, `SELECT envelope_json->>'object' FROM federation_inbox WHERE activity_uri = $1`, upd).Scan(&object); err != nil || object != create {
			t.Fatalf("A received object %q (%v), want the Create id", object, err)
		}
		// B keeps its own text in step.
		var body string
		if err := g.b.pool.QueryRow(ctx, `SELECT body FROM comments WHERE id = $1`, comment).Scan(&body); err != nil || body != "first edit" {
			t.Fatalf("B's own comment %q (%v)", body, err)
		}

		// Exact replay of the Update: answered from A's stored row.
		var before time.Time
		if err := g.b.pool.QueryRow(ctx, `UPDATE federation_outbox SET status = 'queued', next_attempt_at = now()
			WHERE activity_id = (SELECT id FROM activities WHERE activity_uri = $1) RETURNING last_attempt_at`, upd).Scan(&before); err != nil {
			t.Fatal(err)
		}
		g.release()
		deadline := time.Now().Add(90 * time.Second)
		for {
			var st string
			var last time.Time
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status, o.last_attempt_at FROM federation_outbox o
				JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, upd).Scan(&st, &last)
			if st == "sent" && last.After(before) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("B never redelivered the Update (%q)", st)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, upd); n != 1 {
			t.Fatalf("%d inbox rows for one Update", n)
		}
		onA(t, g.editOnB(g.bob, comment, "second edit"), "processed")
		g.wantText(t, create, "live", "second edit")
		g.wantComment(t, post, create, "live", 1, 1)
	})

	t.Run("a delayed older edit never overwrites a newer one", func(t *testing.T) {
		post, comment, create := newComment(t, false)
		onA(t, create, "processed")
		older := g.record(g.updateEm(g.bob, comment, post, create, "older", time.Now().UTC()), true)
		g.parked(older)
		newer := g.editOnB(g.bob, comment, "newer") // B orders it after the parked one
		onA(t, newer, "processed")
		g.release(older)
		onA(t, older, "processed")
		g.wantText(t, create, "live", "newer")
	})

	t.Run("an edit before its Create is applied when the Create arrives", func(t *testing.T) {
		post, comment, create := newComment(t, true)
		g.parked(create)
		upd := g.editOnB(g.bob, comment, "edited before arrival")
		onA(t, upd, "processed")
		if g.pendingOnA(g.bob.uri, create) != 1 {
			t.Fatal("A kept no pending edit")
		}
		g.release(create)
		onA(t, create, "processed")
		g.wantText(t, create, "live", "edited before arrival")
		g.wantComment(t, post, create, "live", 1, 1)
		if g.pendingOnA(g.bob.uri, create) != 0 {
			t.Fatal("the pending edit outlived its Create")
		}
	})

	t.Run("deletion wins: an edit after a Delete, a Delete after a delayed edit, and an edit and Delete before the Create", func(t *testing.T) {
		// Edit after Delete: B's API refuses it; the emitter's is a no-op.
		post, comment, create := newComment(t, false)
		onA(t, create, "processed")
		onA(t, g.deleteOnB(g.bob, comment), "processed")
		if st := g.apiAs(g.b, g.tokenFor(g.bob), http.MethodPatch, "/comments/"+comment.String(), map[string]string{"body": "x"}); st != http.StatusNotFound {
			t.Fatalf("PATCH of a deleted comment: %d", st)
		}
		late := g.record(g.updateEm(g.bob, comment, post, create, "resurrected?", time.Now().UTC()), false)
		onA(t, late, "processed")
		g.wantText(t, create, "deleted", "")
		g.wantComment(t, post, create, "deleted", 0, 1)

		// Delete arrives before a delayed edit.
		post2, comment2, create2 := newComment(t, false)
		onA(t, create2, "processed")
		edit := g.record(g.updateEm(g.bob, comment2, post2, create2, "delayed edit", time.Now().UTC()), true)
		g.parked(edit)
		onA(t, g.deleteOnB(g.bob, comment2), "processed")
		g.release(edit)
		onA(t, edit, "processed")
		g.wantText(t, create2, "deleted", "")

		// An edit and a Delete both before the Create.
		_, comment3, create3 := newComment(t, true)
		g.parked(create3)
		onA(t, g.editOnB(g.bob, comment3, "never shown"), "processed")
		onA(t, g.deleteOnB(g.bob, comment3), "processed")
		if g.pendingOnA(g.bob.uri, create3) != 0 || !g.tombstoneOnA(g.bob.uri, create3) {
			t.Fatal("the Delete did not discard the pending edit and record a tombstone")
		}
		g.release(create3)
		onA(t, create3, "processed")
		if s, _, _ := g.textOnA(create3); s != "" {
			t.Fatalf("the Create after its edit and Delete left %q", s)
		}
	})

	t.Run("another actor, peer or origin cannot edit the comment", func(t *testing.T) {
		post, comment, create := newComment(t, false)
		onA(t, create, "processed")
		eveEdit := g.record(g.updateEm(g.eve, comment, post, create, "eve's words", time.Now().UTC()), false)
		if got := g.skippedOnB(eveEdit); got != "recipient_set_empty" {
			t.Fatalf("eve's edit of bob's comment: skipped %q", got)
		}
		g.forceSend(eveEdit)
		onA(t, eveEdit, "processed")
		g.wantText(t, create, "live", "a comment to delete")
		if g.pendingOnA(g.eve.uri, create) != 1 {
			t.Fatal("eve's edit was not kept under her own identity")
		}
		// A Create id on another origin is refused at receipt.
		foreign := g.record(g.updateEm(g.bob, comment, post, "https://elsewhere.example/activities/"+uuid.NewString(), "x", time.Now().UTC()), false)
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
				t.Fatalf("the foreign-object Update never failed (%q)", st)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, foreign); n != 0 {
			t.Fatal("A stored an Update of a foreign object")
		}
		// Another peer of A naming bob's Create id.
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
		applied, err := social.NewHandler(g.a.pool, g.logger, nil).UpdateRemoteComment(ctx, social.RemoteUpdateCommentInput{
			PeerID: other, ActorURI: g.bob.uri, CreateActivityURI: create, UpdateActivityURI: "https://c.example/activities/u",
			Content: "c's words", Published: time.Now()})
		if err != nil || applied {
			t.Fatalf("another peer's edit: applied %v, %v", applied, err)
		}
		g.wantText(t, create, "live", "a comment to delete")
	})

	t.Run("after a revoke an edit is refused and the text stays", func(t *testing.T) {
		post, uri := g.postOnA()
		sid := g.shareOnA(post, uri, "comment", "", nil)
		comment, create := g.commentOnB(g.bob, post, uri, false)
		onA(t, create, "processed")
		g.revokeOnA(sid, uri, "")
		onA(t, g.editOnB(g.bob, comment, "after revoke"), "rejected/unshared_object")
		g.wantText(t, create, "live", "a comment to delete")
	})

	t.Run("only the author edits: B's moderator and another user are refused, and A's moderator cannot edit a remote comment", func(t *testing.T) {
		_, comment, create := newComment(t, false)
		onA(t, create, "processed")
		before := g.countOn(g.b, `SELECT count(*) FROM activities WHERE activity_type = 'Update' AND object_local_id = $1`, comment.String())
		if st := g.apiAs(g.b, g.b.token, http.MethodPatch, "/comments/"+comment.String(), map[string]string{"body": "moderated"}); st != http.StatusForbidden {
			t.Fatalf("B's admin PATCH: %d, want 403", st)
		}
		if st := g.apiAs(g.b, g.tokenFor(g.eve), http.MethodPatch, "/comments/"+comment.String(), map[string]string{"body": "eve"}); st != http.StatusForbidden {
			t.Fatalf("eve's PATCH: %d, want 403", st)
		}
		if st := g.apiAs(g.b, g.tokenFor(g.bob), http.MethodPatch, "/comments/"+comment.String(), map[string]string{"body": "   "}); st != http.StatusBadRequest {
			t.Fatalf("a blank edit: %d, want 400", st)
		}
		if n := g.countOn(g.b, `SELECT count(*) FROM activities WHERE activity_type = 'Update' AND object_local_id = $1`, comment.String()); n != before {
			t.Fatalf("a refused edit recorded %d Update activities", n-before)
		}
		var local uuid.UUID
		if err := g.a.pool.QueryRow(ctx, `SELECT id FROM comments WHERE activity_uri = $1`, create).Scan(&local); err != nil {
			t.Fatal(err)
		}
		if st := g.apiAs(g.a, g.a.token, http.MethodPatch, "/comments/"+local.String(), map[string]string{"body": "A's words"}); st != http.StatusForbidden {
			t.Fatalf("A's admin PATCH of a remote comment: %d, want 403", st)
		}
		g.wantText(t, create, "live", "a comment to delete")
	})

	t.Run("a transient failure at A is retried, then applied", func(t *testing.T) {
		_, comment, create := newComment(t, false)
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
		upd := g.editOnB(g.bob, comment, "after the failure")
		deadline := time.Now().Add(60 * time.Second)
		for {
			var st, lastErr string
			var attempts int
			_ = g.a.pool.QueryRow(ctx, `SELECT status, dispatch_attempts, last_error FROM federation_inbox WHERE activity_uri = $1`,
				upd).Scan(&st, &attempts, &lastErr)
			if st == "pending" && attempts >= 1 && lastErr != "" {
				break
			}
			if st != "" && st != "pending" {
				t.Fatalf("the Update settled %q during a database failure; want it pending for retry", st)
			}
			if time.Now().After(deadline) {
				t.Fatalf("A never attempted the Update (%q, %d)", st, attempts)
			}
			time.Sleep(50 * time.Millisecond)
		}
		g.wantText(t, create, "live", "a comment to delete")
		if _, err := g.a.pool.Exec(ctx, `DELETE FROM test_fail_comment_update`); err != nil {
			t.Fatal(err)
		}
		if _, err := g.a.pool.Exec(ctx, `SELECT pg_notify('federation_inbox_pending', '')`); err != nil {
			t.Fatal(err)
		}
		onA(t, upd, "processed")
		g.wantText(t, create, "live", "after the failure")
	})

	t.Run("concurrent edits end on the newest, and a concurrent Delete always wins", func(t *testing.T) {
		post, comment, create := newComment(t, false)
		onA(t, create, "processed")
		base := time.Now().UTC().Truncate(time.Microsecond)
		e1 := g.record(g.updateEm(g.bob, comment, post, create, "e1", base.Add(1*time.Millisecond)), true)
		e2 := g.record(g.updateEm(g.bob, comment, post, create, "e2", base.Add(2*time.Millisecond)), true)
		g.parked(e1)
		g.parked(e2)
		g.release(e2, e1)
		onA(t, e1, "processed")
		onA(t, e2, "processed")
		g.wantText(t, create, "live", "e2")

		// A's production write, driven concurrently: three edits and a
		// Delete of one comment at once, for many rounds.
		sh := social.NewHandler(g.a.pool, g.logger, nil)
		allow := func(context.Context, pgx.Tx) error { return nil }
		for round := 0; round < 25; round++ {
			p, _ := g.postOnA()
			cr := fmt.Sprintf("%s/activities/uc%d-%s", g.b.url, round, uuid.NewString()[:8])
			if _, _, err := sh.InsertRemoteComment(ctx, social.RemoteCommentInput{TargetKind: "post", TargetID: p,
				PeerID: g.peerBonA, ActorURI: g.bob.uri, ActivityURI: cr, Body: "original", Authorize: allow}); err != nil {
				t.Fatal(err)
			}
			withDelete := round%2 == 0
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			start := make(chan struct{})
			for i := 1; i <= 3; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					if _, err := sh.UpdateRemoteComment(ctx, social.RemoteUpdateCommentInput{PeerID: g.peerBonA, ActorURI: g.bob.uri,
						CreateActivityURI: cr, UpdateActivityURI: fmt.Sprintf("%s/u%d", cr, i), Content: fmt.Sprintf("edit %d", i),
						Published: base.Add(time.Duration(i) * time.Millisecond),
						Authorize: func(string, uuid.UUID) social.InboundAuthorizer { return allow }}); err != nil {
						errs <- err
					}
				}(i)
			}
			if withDelete {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if _, err := sh.DeleteRemoteComment(ctx, social.RemoteDeleteCommentInput{PeerID: g.peerBonA, ActorURI: g.bob.uri,
						CreateActivityURI: cr, DeleteActivityURI: cr + "/delete"}); err != nil {
						errs <- err
					}
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("round %d: %v", round, err)
			}
			var deleted bool
			var body string
			if err := g.a.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL, body FROM comments WHERE activity_uri = $1`, cr).Scan(&deleted, &body); err != nil {
				t.Fatal(err)
			}
			if withDelete && (!deleted || body != "") {
				t.Fatalf("round %d: deleted %v body %q; the Delete must win", round, deleted, body)
			}
			if !withDelete && (deleted || body != "edit 3") {
				t.Fatalf("round %d: deleted %v body %q; want the newest edit", round, deleted, body)
			}
		}
	})

	t.Run("an edit of a comment on a local post goes nowhere", func(t *testing.T) {
		local := uuid.New()
		if _, err := g.b.pool.Exec(ctx, `INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Local', 'public')`,
			local, g.b.admin); err != nil {
			t.Fatal(err)
		}
		comment, create := g.commentOnB(g.bob, local, "", false)
		g.skippedOnB(create)
		upd := g.editOnB(g.bob, comment, "local edit")
		if got := g.skippedOnB(upd); got != "recipient_set_empty" {
			t.Fatalf("its edit: skipped %q", got)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, upd); n != 0 {
			t.Fatal("a local comment's edit reached A")
		}
	})
}
