// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1572 gate (first slice): an Undo of a Like, from the production
// emitter on B to the owner's production inbox on A.
//
// B's user likes A's post (#1611 path) and then undoes it. The Undo is
// built by the production emitter (emit.UndoLike, naming the Like's
// activity id) and recorded by the production activity writer; B's own
// outbox dispatcher and resolver address it to exactly the recipients
// the Like was queued for; B's delivery worker encrypts it to the same
// target user and signs it; A's own inbox and dispatcher validate it
// (an Undo of a Like only), bind it to the Like by peer, signed actor and
// id, and retract it. Grants are made and ended through A's real admin
// API.
//
// TEST FIXTURES, stated:
//
//   - Arrival order: a BEFORE INSERT trigger on B's disposable database
//     parks the delivery rows of chosen activities (next_attempt_at =
//     infinity) until the test releases them, so B's own worker delivers
//     them in the order a subtest needs. The rows, the envelopes and the
//     receiving path are the production ones.
//   - A delivery the production sender would never queue (another
//     actor's Undo of bob's Like, an Undo naming a foreign object, an
//     Undo of a Follow) gets a delivery row inserted on B's disposable
//     database, so B's own worker signs, seals and sends it as that
//     actor. What A does with it is the production path.
//   - A wrong-peer Undo and the concurrent-write check call A's
//     production social write (social.Handler.UndoRemoteLike /
//     InsertRemoteLike) directly on A's disposable database: a third
//     instance is not booted, and the inbox dispatcher is a single
//     goroutine, so concurrent writes are driven at the write itself.
//   - A transient failure is a renamed table on A's disposable database.
//   - Keys are bootstrapped as in the #1604 / #1611 gates.
//
// LIMITATION, stated: no page or API on B can like or unlike A's post
// (#1574, #1608); the test calls the production emitters with A's post
// as the explicit target. No user-facing Undo of a remote Like is
// claimed.

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

type undoGate struct {
	*likeGate
}

func newUndoGate(t *testing.T) *undoGate {
	t.Helper()
	g := &undoGate{likeGate: newLikeGate(t)}
	ctx := context.Background()
	// Fixture on B's disposable database: park the delivery rows of held
	// activities until released.
	for _, sql := range []string{
		`CREATE TABLE IF NOT EXISTS test_held_activities (activity_uri text PRIMARY KEY)`,
		`CREATE OR REPLACE FUNCTION test_hold_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM test_held_activities h JOIN activities a ON a.activity_uri = h.activity_uri
			            WHERE a.id = NEW.activity_id) THEN
				NEW.next_attempt_at := 'infinity';
			END IF;
			RETURN NEW;
		END $$`,
		`DROP TRIGGER IF EXISTS test_hold_outbox ON federation_outbox`,
		`CREATE TRIGGER test_hold_outbox BEFORE INSERT ON federation_outbox FOR EACH ROW EXECUTE FUNCTION test_hold_outbox()`,
	} {
		if _, err := g.b.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, sql := range []string{`DROP TRIGGER IF EXISTS test_hold_outbox ON federation_outbox`,
			`DROP FUNCTION IF EXISTS test_hold_outbox()`, `DROP TABLE IF EXISTS test_held_activities`} {
			_, _ = g.b.pool.Exec(context.Background(), sql)
		}
	})
	return g
}

func (g *undoGate) actorCtx(a likeActor) emit.ActorContext {
	return emit.ActorContext{UserRef: a.ref, Username: a.name, BaseURL: g.b.url}
}

func (g *undoGate) likeEm(a likeActor, post uuid.UUID, uri string) emit.Emission {
	return emit.Like(g.actorCtx(a), emit.PostRef{ID: post.String(), Title: "Liked post", AuthorURI: g.adminURIOnA,
		TargetURI: uri, OwnerPeerID: g.peerAonB.String()})
}

func (g *undoGate) undoEm(a likeActor, likeID string, post uuid.UUID) emit.Emission {
	return emit.UndoLike(g.actorCtx(a), likeID, emit.PostRef{ID: post.String(), AuthorURI: g.adminURIOnA})
}

// record writes em through B's production activity writer, parking its
// deliveries first when hold is set; returns the activity id.
func (g *undoGate) record(em emit.Emission, hold bool) string {
	g.t.Helper()
	ctx := context.Background()
	if hold {
		if _, err := g.b.pool.Exec(ctx, `INSERT INTO test_held_activities VALUES ($1)`, em.Activity.ActivityURI); err != nil {
			g.t.Fatal(err)
		}
	}
	if err := pgx.BeginFunc(ctx, g.b.pool, func(tx pgx.Tx) error {
		_, err := g.writer.RecordActivity(ctx, tx, em.Activity)
		return err
	}); err != nil {
		g.t.Fatal(err)
	}
	return em.Activity.ActivityURI
}

func (g *undoGate) undo(a likeActor, likeID string, post uuid.UUID) string {
	return g.record(g.undoEm(a, likeID, post), false)
}

// parked waits until B queued activity's delivery rows (parked).
func (g *undoGate) parked(activity string) {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if g.countOn(g.b, `SELECT count(*) FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
			WHERE a.activity_uri = $1 AND o.next_attempt_at = 'infinity'`, activity) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("B never queued %s", activity)
}

// release lets B's worker deliver activity's parked rows now.
func (g *undoGate) release(activities ...string) {
	g.t.Helper()
	ctx := context.Background()
	for _, a := range activities {
		if _, err := g.b.pool.Exec(ctx, `UPDATE federation_outbox SET next_attempt_at = now()
			WHERE activity_id = (SELECT id FROM activities WHERE activity_uri = $1)`, a); err != nil {
			g.t.Fatal(err)
		}
		if _, err := g.b.pool.Exec(ctx, `DELETE FROM test_held_activities WHERE activity_uri = $1`, a); err != nil {
			g.t.Fatal(err)
		}
	}
	if _, err := g.b.pool.Exec(ctx, `SELECT pg_notify('federation_outbox_pending', '')`); err != nil {
		g.t.Fatal(err)
	}
}

// forceSend queues a delivery to A for an activity the production
// resolver would not address (a fixture row on B), so B's own worker
// signs, seals and sends it.
func (g *undoGate) forceSend(activity string) {
	g.t.Helper()
	if _, err := g.b.pool.Exec(context.Background(), `INSERT INTO federation_outbox (activity_id, peer_id, target_user_url, sensitivity)
		SELECT id, $2, $3, 'unresolved' FROM activities WHERE activity_uri = $1`, activity, g.peerAonB, g.adminURIOnA); err != nil {
		g.t.Fatal(err)
	}
}

// recordOnA is A's record of a Like id: "binding/live|undone", or "".
func (g *undoGate) recordOnA(actorURI, likeID string) string {
	g.t.Helper()
	var binding string
	var undone bool
	err := g.a.pool.QueryRow(context.Background(), `SELECT binding, undone_at IS NOT NULL FROM federation_remote_likes
		WHERE peer_id = $1 AND actor_uri = $2 AND activity_uri = $3`, g.peerBonA, actorURI, likeID).Scan(&binding, &undone)
	if err != nil {
		return ""
	}
	if undone {
		return binding + "/undone"
	}
	return binding + "/live"
}

// outboxOnB is B's delivery row of activity: status|target user|tier.
func (g *undoGate) outboxOnB(activity string) string {
	g.t.Helper()
	var st, target, sen string
	var enc bool
	if err := g.b.pool.QueryRow(context.Background(), `SELECT o.status, coalesce(o.target_user_url, ''), coalesce(o.sensitivity, ''), o.was_encrypted
		FROM federation_outbox o JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, activity).Scan(&st, &target, &sen, &enc); err != nil {
		g.t.Fatal(err)
	}
	return fmt.Sprintf("%s|%s|%s|encrypted=%v", st, target, sen, enc)
}

func TestTwoStack_UndoLike(t *testing.T) {
	g := newUndoGate(t)
	ctx := context.Background()
	owner := g.peerAonB.String()
	onA := func(t *testing.T, activity, want string) {
		t.Helper()
		if got := g.onA(activity); got != want {
			t.Fatalf("%s on A: %s, want %s", activity, got, want)
		}
	}

	t.Run("Like then Undo: unliked, notification kept; an exact replay and a second Undo change nothing", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		like := g.like(g.bob, post, uri, owner)
		onA(t, like, "processed")
		g.wantOnA(t, post, g.bob.uri, 1, 1, 1)
		if got := g.recordOnA(g.bob.uri, like); got != "received/live" {
			t.Fatalf("A's record of the Like: %q", got)
		}

		undo := g.undo(g.bob, like, post)
		onA(t, undo, "processed")
		g.wantOnA(t, post, g.bob.uri, 0, 0, 1)
		if got := g.recordOnA(g.bob.uri, like); got != "received/undone" {
			t.Fatalf("A's record after the Undo: %q", got)
		}
		// The Undo went exactly where the Like went: the same peer and
		// target user, no less protected, encrypted.
		likeRow, undoRow := g.outboxOnB(like), g.outboxOnB(undo)
		if likeRow != undoRow || likeRow != "sent|"+g.adminURIOnA+"|unresolved|encrypted=true" {
			t.Fatalf("delivery rows: Like %q, Undo %q", likeRow, undoRow)
		}
		var object string
		var wasEncrypted bool
		if err := g.a.pool.QueryRow(ctx, `SELECT envelope_json->>'object', was_encrypted FROM federation_inbox WHERE activity_uri = $1`,
			undo).Scan(&object, &wasEncrypted); err != nil {
			t.Fatal(err)
		}
		if object != like || !wasEncrypted {
			t.Fatalf("A received object %q (encrypted %v); want the Like id, encrypted", object, wasEncrypted)
		}

		// B redelivers the same Undo: answered from A's stored row.
		var before time.Time
		if err := g.b.pool.QueryRow(ctx, `UPDATE federation_outbox SET status = 'queued', next_attempt_at = now()
			WHERE activity_id = (SELECT id FROM activities WHERE activity_uri = $1) RETURNING last_attempt_at`, undo).Scan(&before); err != nil {
			t.Fatal(err)
		}
		g.release()
		deadline := time.Now().Add(90 * time.Second)
		for {
			var st string
			var last time.Time
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status, o.last_attempt_at FROM federation_outbox o
				JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, undo).Scan(&st, &last)
			if st == "sent" && last.After(before) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("B never redelivered the Undo (%q)", st)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, undo); n != 1 {
			t.Fatalf("%d inbox rows for one Undo", n)
		}
		g.wantOnA(t, post, g.bob.uri, 0, 0, 1)

		// A second, distinct Undo of the same Like is a processed no-op.
		again := g.undo(g.bob, like, post)
		onA(t, again, "processed")
		g.wantOnA(t, post, g.bob.uri, 0, 0, 1)
		var firstUndo string
		if err := g.a.pool.QueryRow(ctx, `SELECT undo_activity_uri FROM federation_remote_likes
			WHERE peer_id = $1 AND actor_uri = $2 AND activity_uri = $3`, g.peerBonA, g.bob.uri, like).Scan(&firstUndo); err != nil || firstUndo != undo {
			t.Fatalf("the record names Undo %q (%v), want the first %q", firstUndo, err, undo)
		}
	})

	t.Run("every arrival order of Like1, Undo1, Like2 ends liked once", func(t *testing.T) {
		type step int
		const (
			l1 step = iota
			u1
			l2
		)
		orders := []struct {
			order []step
			notes int // times the like row was created: the post author's notifications
		}{
			{[]step{l1, u1, l2}, 2},
			{[]step{l1, l2, u1}, 1},
			{[]step{u1, l1, l2}, 1},
			{[]step{u1, l2, l1}, 1},
			{[]step{l2, l1, u1}, 1},
			{[]step{l2, u1, l1}, 1},
		}
		for _, o := range orders {
			post, uri := g.postOnA()
			g.shareOnA(post, uri, "view", "", nil)
			// Sent in the order Like1, Undo1, Like2; delivered in o.order.
			like1 := g.likeEm(g.bob, post, uri)
			undo1 := g.undoEm(g.bob, like1.Activity.ActivityURI, post)
			like2 := g.likeEm(g.bob, post, uri)
			ids := map[step]string{l1: g.record(like1, true), u1: g.record(undo1, true), l2: g.record(like2, true)}
			for _, id := range ids {
				g.parked(id)
			}
			for _, s := range o.order {
				g.release(ids[s])
				onA(t, ids[s], "processed")
			}
			if r, c, n := g.stateOnA(post, g.bob.uri); r != 1 || c != 1 || n != o.notes {
				t.Errorf("order %v: %d like rows, like_count %d, %d notifications; want 1, 1, %d", o.order, r, c, n, o.notes)
			}
			if a, b := g.recordOnA(g.bob.uri, ids[l1]), g.recordOnA(g.bob.uri, ids[l2]); a != "received/undone" || b != "received/live" {
				t.Errorf("order %v: records Like1 %q, Like2 %q", o.order, a, b)
			}
		}
	})

	t.Run("an Undo before its Like: the Like arrives undone, with no like and no notification", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		likeEm := g.likeEm(g.bob, post, uri)
		like := g.record(likeEm, true)
		g.parked(like)
		undo := g.undo(g.bob, like, post)
		onA(t, undo, "processed")
		if got := g.recordOnA(g.bob.uri, like); got != "tombstone/undone" {
			t.Fatalf("A's record before the Like: %q", got)
		}
		g.release(like)
		onA(t, like, "processed")
		g.wantOnA(t, post, g.bob.uri, 0, 0, 0)
		if got := g.recordOnA(g.bob.uri, like); got != "received/undone" {
			t.Fatalf("A's record after the Like: %q", got)
		}
	})

	t.Run("after a revoke a new Like is refused, and the Undo still applies", func(t *testing.T) {
		// Production revoke: B learns it, stops sending new Likes, and
		// still sends the Undo (it needs no grant).
		post, uri := g.postOnA()
		sid := g.shareOnA(post, uri, "view", "", nil)
		like := g.like(g.bob, post, uri, owner)
		onA(t, like, "processed")
		g.revokeOnA(sid, uri, "")
		if got := g.skippedOnB(g.like(g.bob, post, uri, owner)); got != "no_received_grant" {
			t.Fatalf("a new Like after the revoke: skipped %q", got)
		}
		onA(t, g.undo(g.bob, like, post), "processed")
		g.wantOnA(t, post, g.bob.uri, 0, 0, 1)

		// Fixture on A: the share ended before B learned it. A refuses
		// the new Like it still receives, and applies the Undo.
		post2, uri2 := g.postOnA()
		g.shareOnA(post2, uri2, "view", "", nil)
		like1 := g.like(g.bob, post2, uri2, owner)
		onA(t, like1, "processed")
		if _, err := g.a.pool.Exec(ctx, `UPDATE federation_shares SET revoked_at = now() WHERE object_id = $1`, post2); err != nil {
			t.Fatal(err)
		}
		onA(t, g.like(g.eve, post2, uri2, owner), "rejected/unshared_object")
		g.wantOnA(t, post2, g.eve.uri, 0, 1, 1)
		onA(t, g.undo(g.bob, like1, post2), "processed")
		g.wantOnA(t, post2, g.bob.uri, 0, 0, 1)
	})

	t.Run("a deleted target: the Undo still retracts", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		like := g.like(g.bob, post, uri, owner)
		onA(t, like, "processed")
		if _, err := g.a.pool.Exec(ctx, `UPDATE posts SET deleted_at = now() WHERE id = $1`, post); err != nil {
			t.Fatal(err)
		}
		onA(t, g.undo(g.bob, like, post), "processed")
		g.wantOnA(t, post, g.bob.uri, 0, 0, 1)
	})

	t.Run("another actor, peer or origin cannot remove the Like, and every answer is the same", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		like := g.like(g.bob, post, uri, owner)
		onA(t, like, "processed")

		// B's own sender never sends eve's Undo of bob's Like: her Like
		// was queued for nobody.
		eveUndo := g.undo(g.eve, like, post)
		if got := g.skippedOnB(eveUndo); got != "recipient_set_empty" {
			t.Fatalf("eve's Undo of bob's Like: skipped %q", got)
		}
		// Sent anyway (fixture row), signed by eve: A records eve's own
		// tombstone and bob stays liked. An Undo of an id A never saw, by
		// bob, is answered and recorded exactly the same way.
		g.forceSend(eveUndo)
		onA(t, eveUndo, "processed")
		unseen := g.record(g.undoEm(g.bob, g.b.url+"/activities/"+uuid.NewString(), post), false)
		if got := g.skippedOnB(unseen); got != "recipient_set_empty" {
			t.Fatalf("an Undo of an unknown Like: skipped %q", got)
		}
		g.forceSend(unseen)
		onA(t, unseen, "processed")
		if a, b := g.outboxOnB(eveUndo), g.outboxOnB(unseen); a != b {
			t.Fatalf("receipts differ: eve %q, unseen %q", a, b)
		}
		g.wantOnA(t, post, g.bob.uri, 1, 1, 1)
		if got := g.recordOnA(g.bob.uri, like); got != "received/live" {
			t.Fatalf("bob's record: %q", got)
		}
		if got := g.recordOnA(g.eve.uri, like); got != "tombstone/undone" {
			t.Fatalf("eve's record of bob's Like id: %q", got)
		}

		// An object on another origin is refused at receipt, before
		// anything is stored.
		foreign := g.record(g.undoEm(g.bob, "https://elsewhere.example/activities/"+uuid.NewString(), post), false)
		g.skippedOnB(foreign)
		g.forceSend(foreign)
		deadline := time.Now().Add(60 * time.Second)
		for !(func() bool {
			var st, lastErr string
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status, o.last_error FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
				WHERE a.activity_uri = $1`, foreign).Scan(&st, &lastErr)
			return st == "failed"
		})() {
			if time.Now().After(deadline) {
				t.Fatal("B's delivery of the foreign-object Undo never failed")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, foreign); n != 0 {
			t.Fatal("A stored an Undo of a foreign object")
		}

		// Another peer of A naming bob's Like id (A's production write;
		// a third peer row on A's disposable database).
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
		sh := social.NewHandler(g.a.pool, g.logger, nil)
		removed, err := sh.UndoRemoteLike(ctx, social.RemoteUndoLikeInput{PeerID: other, ActorURI: g.bob.uri,
			LikeActivityURI: like, UndoActivityURI: "https://c.example/activities/u"})
		if err != nil || removed {
			t.Fatalf("another peer's Undo: removed %v, %v", removed, err)
		}
		g.wantOnA(t, post, g.bob.uri, 1, 1, 1)
	})

	t.Run("an Undo of anything but a Like stays local, and is never applied", func(t *testing.T) {
		follow := emit.UndoFollow(g.actorCtx(g.bob), g.b.url+"/activities/"+uuid.NewString(), g.b.admin)
		block := emit.UndoBlock(g.actorCtx(g.bob), g.b.url+"/activities/"+uuid.NewString(), g.b.admin)
		for name, em := range map[string]emit.Emission{"Undo of a Follow": follow, "Undo of a Block": block} {
			id := g.record(em, false)
			if got := g.skippedOnB(id); got != "activity_not_federated" {
				t.Errorf("%s: skipped %q", name, got)
			}
			// A delivery row queued anyway is closed without sending.
			g.forceSend(id)
			deadline := time.Now().Add(60 * time.Second)
			for {
				var st, reason string
				_ = g.b.pool.QueryRow(ctx, `SELECT o.status, coalesce(o.refused_reason, o.last_error, '') FROM federation_outbox o
					JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, id).Scan(&st, &reason)
				if st == "failed" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s: B's row never closed (%q)", name, st)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, id); n != 0 {
				t.Errorf("%s reached A", name)
			}
		}
	})

	t.Run("a transient failure at A is retried, then applied", func(t *testing.T) {
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		like := g.like(g.bob, post, uri, owner)
		onA(t, like, "processed")
		if _, err := g.a.pool.Exec(ctx, `ALTER TABLE federation_remote_likes RENAME TO federation_remote_likes_held`); err != nil {
			t.Fatal(err)
		}
		renamed := true
		t.Cleanup(func() {
			if renamed {
				_, _ = g.a.pool.Exec(context.Background(), `ALTER TABLE federation_remote_likes_held RENAME TO federation_remote_likes`)
			}
		})
		undo := g.undo(g.bob, like, post)
		deadline := time.Now().Add(60 * time.Second)
		for {
			var st, lastErr string
			var attempts int
			_ = g.a.pool.QueryRow(ctx, `SELECT status, dispatch_attempts, last_error FROM federation_inbox WHERE activity_uri = $1`,
				undo).Scan(&st, &attempts, &lastErr)
			if st == "pending" && attempts >= 1 && lastErr != "" {
				break
			}
			if st != "" && st != "pending" {
				t.Fatalf("the Undo settled %q during a database failure; want it pending for retry", st)
			}
			if time.Now().After(deadline) {
				t.Fatalf("A never attempted the Undo (%q, %d)", st, attempts)
			}
			time.Sleep(50 * time.Millisecond)
		}
		g.wantOnA(t, post, g.bob.uri, 1, 1, 1)
		if _, err := g.a.pool.Exec(ctx, `ALTER TABLE federation_remote_likes_held RENAME TO federation_remote_likes`); err != nil {
			t.Fatal(err)
		}
		renamed = false
		// Wake A's dispatcher instead of waiting for its backstop tick.
		if _, err := g.a.pool.Exec(ctx, `SELECT pg_notify('federation_inbox_pending', '')`); err != nil {
			t.Fatal(err)
		}
		onA(t, undo, "processed")
		g.wantOnA(t, post, g.bob.uri, 0, 0, 1)
	})

	t.Run("concurrent deliveries and concurrent writes keep one like per live record", func(t *testing.T) {
		// Two-instance: Like2 and Undo1 released at once.
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		like1 := g.like(g.bob, post, uri, owner)
		onA(t, like1, "processed")
		undo1 := g.record(g.undoEm(g.bob, like1, post), true)
		like2 := g.record(g.likeEm(g.bob, post, uri), true)
		g.parked(undo1)
		g.parked(like2)
		g.release(undo1, like2)
		onA(t, undo1, "processed")
		onA(t, like2, "processed")
		// Liked once either way. The like row is created again (one more
		// notification, as for a local re-like) only when A applied the
		// Undo first.
		var undoFirst bool
		if err := g.a.pool.QueryRow(ctx, `SELECT (SELECT processed_at FROM federation_inbox WHERE activity_uri = $1) <
			(SELECT processed_at FROM federation_inbox WHERE activity_uri = $2)`, undo1, like2).Scan(&undoFirst); err != nil {
			t.Fatal(err)
		}
		notes := 1
		if undoFirst {
			notes = 2
		}
		g.wantOnA(t, post, g.bob.uri, 1, 1, notes)

		// A's production write, driven concurrently: four Likes each with
		// its Undo and one more Like, all at once, for many rounds. The
		// authorizer admits everything: what is checked is the record and
		// aggregate invariant under concurrency, not the share gate.
		sh := social.NewHandler(g.a.pool, g.logger, nil)
		allow := func(context.Context, pgx.Tx) error { return nil }
		for round := 0; round < 25; round++ {
			p, _ := g.postOnA()
			ids := make([]string, 5)
			for i := range ids {
				ids[i] = fmt.Sprintf("%s/activities/c%d-%d-%s", g.b.url, round, i, uuid.NewString()[:8])
			}
			var wg sync.WaitGroup
			errs := make(chan error, 9)
			start := make(chan struct{})
			for i, id := range ids {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					<-start
					if _, err := sh.InsertRemoteLike(ctx, "post", p, g.peerBonA, g.bob.uri, id, allow); err != nil {
						errs <- err
					}
				}(id)
				if i < 4 {
					wg.Add(1)
					go func(id string, n int) {
						defer wg.Done()
						<-start
						if _, err := sh.UndoRemoteLike(ctx, social.RemoteUndoLikeInput{PeerID: g.peerBonA, ActorURI: g.bob.uri,
							LikeActivityURI: id, UndoActivityURI: fmt.Sprintf("%s/activities/u%d-%d", g.b.url, round, n)}); err != nil {
							errs <- err
						}
					}(id, i)
				}
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("round %d: %v", round, err)
			}
			rows := g.countOn(g.a, `SELECT count(*) FROM likes WHERE target_kind = 'post' AND target_id = $1 AND peer_id = $2 AND actor_uri = $3`,
				p, g.peerBonA, g.bob.uri)
			count := g.countOn(g.a, `SELECT like_count FROM posts WHERE id = $1`, p)
			live := g.countOn(g.a, `SELECT count(*) FROM federation_remote_likes WHERE target_id = $1 AND undone_at IS NULL`, p)
			if rows != 1 || count != 1 || live != 1 {
				t.Fatalf("round %d: %d like rows, like_count %d, %d live records; want 1, 1, 1", round, rows, count, live)
			}
			for i, id := range ids[:4] {
				if got := g.recordOnA(g.bob.uri, id); got != "received/undone" {
					t.Fatalf("round %d: Like %d record %q", round, i, got)
				}
			}
		}
	})

	t.Run("a Like of a local post: its Undo has nobody to go to", func(t *testing.T) {
		local := uuid.New()
		if _, err := g.b.pool.Exec(ctx, `INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Local', 'public')`,
			local, g.b.admin); err != nil {
			t.Fatal(err)
		}
		if st := g.api(g.b, http.MethodPost, "/posts/"+local.String()+"/like", nil, nil); st >= 300 {
			t.Fatalf("like: %d", st)
		}
		if st := g.api(g.b, http.MethodDelete, "/posts/"+local.String()+"/like", nil, nil); st >= 300 {
			t.Fatalf("unlike: %d", st)
		}
		var undo string
		if err := g.b.pool.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE activity_type = 'Undo' AND payload->>'target_post_id' = $1`,
			local.String()).Scan(&undo); err != nil {
			t.Fatal(err)
		}
		if got := g.skippedOnB(undo); got != "recipient_set_empty" {
			t.Fatalf("local Undo: skipped %q", got)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, undo); n != 0 {
			t.Fatal("the Undo of a local Like reached A")
		}
	})
}
