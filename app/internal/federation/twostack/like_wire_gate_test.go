// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1611 gate: a Like of another instance's post, from the production
// emitter on B to the owner's production inbox on A.
//
// B's user likes A's post. The activity is built by the production
// emitter (emit.Like with an explicit target and owning peer) and
// recorded by the production activity writer on B; B's own outbox
// dispatcher and resolver address it to A only while B holds a live
// grant A sent it (federation_inbound_grants, #1571) covering that user;
// B's delivery worker encrypts it to the post's author and signs it;
// A's own inbox and dispatcher decrypt, validate, authorise against A's
// share (#1570) and write it. Grants are made and ended through A's real
// admin API and reach B as aa:Share / aa:Unshare.
//
// LIMITATION, stated: B has no representation of A's post (received
// objects are #1574), so no page or API action on B can name it yet
// (#1608). The test calls the production emitter with A's post URI and
// A's peer id as the explicit target. No UI interoperability is
// claimed.
//
// TEST-ONLY key bootstrap, as in the #1604 gate: each user's encryption
// key and each side's cache of the other's public key are seeded (a Like
// of a remote object has no local sensitivity tier, so it is sent
// encrypted or not at all, and key exchange is not #1611).

package twostack_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/activities/emit"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

type likeGate struct {
	*cluster
	peerAonB, peerBonA uuid.UUID
	adminURIOnA        string
	bob, eve           likeActor
	writer             *activities.Writer
}

type likeActor struct {
	ref       int64
	name, uri string
}

func (g *likeGate) userOnB(name string) likeActor {
	g.t.Helper()
	ctx := context.Background()
	a := likeActor{name: name + uuid.NewString()[:6]}
	if err := g.b.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		a.name, name).Scan(&a.ref); err != nil {
		g.t.Fatal(err)
	}
	if _, err := users.NewHandler(g.b.pool, g.logger, nil).EnsureActorKeyMaterial(ctx, a.ref, g.b.url, a.name); err != nil {
		g.t.Fatal(err)
	}
	a.uri = g.b.url + "/users/" + a.name
	cacheRemoteKey(g.t, g.a, g.peerBonA, a.uri, encKey(g.t, g.b, a.ref))
	return a
}

func newLikeGate(t *testing.T) *likeGate {
	t.Helper()
	c := newCluster(t)
	g := &likeGate{cluster: c, peerAonB: c.peerID(c.b, c.a), peerBonA: c.peerID(c.a, c.b),
		writer: activities.NewWriter(c.b.pool, c.logger, nil)}
	var adminName string
	if err := c.a.pool.QueryRow(context.Background(), `SELECT username FROM "user" WHERE ref = $1`, c.a.admin).Scan(&adminName); err != nil {
		t.Fatal(err)
	}
	g.adminURIOnA = c.a.url + "/users/" + adminName
	cacheRemoteKey(t, c.b, g.peerAonB, g.adminURIOnA, encKey(t, c.a, c.a.admin))
	g.bob, g.eve = g.userOnB("bob"), g.userOnB("eve")
	c.runWorkers(c.a)
	c.runWorkers(c.b)
	return g
}

func (g *likeGate) postOnA() (uuid.UUID, string) {
	g.t.Helper()
	id := uuid.New()
	if _, err := g.a.pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Liked post', 'explicit-share')`, id, g.a.admin); err != nil {
		g.t.Fatal(err)
	}
	return id, g.a.url + "/posts/" + id.String()
}

// shareOnA grants post to B through A's admin API and waits for B to
// hold the grant live. target is a B actor URI, or "" for peer-wide.
func (g *likeGate) shareOnA(post uuid.UUID, postURI, scope, target string, expires *time.Time) string {
	g.t.Helper()
	body := map[string]any{"object_kind": "post", "object_id": post, "peer_id": g.peerBonA, "scope": scope}
	if target != "" {
		body["target_user_url"] = target
	}
	if expires != nil {
		body["expires_at"] = expires.UTC().Format(time.RFC3339Nano)
	}
	var out struct {
		ID string `json:"id"`
	}
	if st := g.api(g.a, http.MethodPost, "/admin/federation/shares", body, &out); st != 201 {
		g.t.Fatalf("share: %d", st)
	}
	g.waitGrantOnB(postURI, target, "live")
	return out.ID
}

func (g *likeGate) revokeOnA(shareID, postURI, target string) {
	g.t.Helper()
	if st := g.api(g.a, http.MethodDelete, "/admin/federation/shares/"+shareID, nil, nil); st != 204 {
		g.t.Fatalf("revoke: %d", st)
	}
	g.waitGrantOnB(postURI, target, "ended")
}

// waitGrantOnB waits until B's newest grant for (object, recipient) is
// in state.
func (g *likeGate) waitGrantOnB(objectURI, target, state string) {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var (
		lastState string
		lastErr   error
	)
	for time.Now().Before(deadline) {
		var got string
		err := g.b.pool.QueryRow(context.Background(), `SELECT state FROM federation_inbound_grants
			WHERE peer_id = $1 AND object_uri = $2 AND coalesce(target_user_uri, '') = $3
			ORDER BY received_at DESC, id DESC LIMIT 1`, g.peerAonB, objectURI, target).Scan(&got)
		if err == nil && got == state {
			return
		}
		lastState, lastErr = got, err
		time.Sleep(50 * time.Millisecond)
	}
	// The read-only failure snapshot correlates this object's own share
	// activities (metadata_stall_diagnostics_test.go). shareTrail's
	// peer-wide history is not object-scoped and is not repeated here.
	g.t.Fatalf("B never held the grant for %s (%q) %s; %s", objectURI, target, state,
		grantDiagnostics(g, objectURI, target, state, lastGrant(lastState, lastErr)))
}

// shareTrail is where A's share activities for objectURI went: A's
// delivery rows to B, B's inbox rows from A, and B's grants, with their
// statuses and errors, for a failure message.
func (g *likeGate) shareTrail(objectURI string) string {
	ctx := context.Background()
	var out []string
	collect := func(label string, in *instance, q string, args ...any) {
		rows, err := in.pool.Query(ctx, q, args...)
		if err != nil {
			out = append(out, label+": "+err.Error())
			return
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err == nil {
				out = append(out, label+": "+line)
				n++
			}
		}
		if n == 0 {
			out = append(out, label+": none")
		}
	}
	collect("A outbox", g.a, `SELECT a.activity_type || ' ' || o.status || ' attempts=' || o.attempts ||
			' next=' || o.next_attempt_at || ' err=' || coalesce(o.last_error, '') || ' refused=' || coalesce(o.refused_reason, '')
		  FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
		 WHERE o.peer_id = $1 AND a.activity_type IN ('aa:Share', 'aa:Unshare') ORDER BY o.created_at`, g.peerBonA)
	collect("B inbox", g.b, `SELECT activity_type || ' ' || status || ' reason=' || coalesce(reject_reason, '') ||
			' attempts=' || dispatch_attempts || ' err=' || last_error
		  FROM federation_inbox WHERE peer_id = $1 AND activity_type IN ('aa:Share', 'aa:Unshare') ORDER BY received_at`, g.peerAonB)
	collect("B grants", g.b, `SELECT state || ' ' || coalesce(end_reason, '') || ' target=' || coalesce(target_user_uri, '')
		  FROM federation_inbound_grants WHERE peer_id = $1 AND object_uri = $2 ORDER BY received_at`, g.peerAonB, objectURI)
	// Whether A's outbox dispatcher reached the share activities at all,
	// and whether it skipped them (and why).
	collect("A dispatcher", g.a, `SELECT 'undispatched local activities=' || count(*)
		  FROM activities a, federation_dispatch_state s
		 WHERE a.source = 'local' AND (s.last_dispatched_activity_id IS NULL OR (a.created_at, a.id) >
		       ((SELECT created_at FROM activities WHERE id = s.last_dispatched_activity_id), s.last_dispatched_activity_id))`)
	collect("A skipped", g.a, `SELECT a.activity_type || ' ' || coalesce(e.metadata->>'reason', '')
		  FROM audit_events e JOIN activities a ON a.id::text = e.metadata->>'activity_id'
		 WHERE e.event_type = 'federation.emission.skipped' AND a.activity_type IN ('aa:Share', 'aa:Unshare')
		   AND a.created_at > now() - interval '5 minutes' ORDER BY e.occurred_at`)
	return strings.Join(out, "; ") + "; " + g.peerRows()
}

// in makes t the test this gate's helpers report to: a subtest's own t,
// so a helper's Fatal ends that subtest rather than calling FailNow on
// its parent from the subtest's goroutine. It returns the restore.
func (g *likeGate) in(t *testing.T) func() {
	prev := g.t
	g.t = t
	return func() { g.t = prev }
}

// like records actor's Like of target, owned by ownerPeer, through the
// production emitter and activity writer on B; returns the activity id.
func (g *likeGate) like(actor likeActor, post uuid.UUID, target, ownerPeer string) string {
	g.t.Helper()
	em := emit.Like(emit.ActorContext{UserRef: actor.ref, Username: actor.name, BaseURL: g.b.url}, emit.PostRef{
		ID: post.String(), Title: "Liked post", AuthorURI: g.adminURIOnA, TargetURI: target, OwnerPeerID: ownerPeer,
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
func (g *likeGate) onA(activity string) string {
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
	_ = g.b.pool.QueryRow(context.Background(), `SELECT o.status, coalesce(o.last_error, '') FROM federation_outbox o
		JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, activity).Scan(&st, &lastErr)
	g.t.Fatalf("A never settled %s (B outbox: %q %q)", activity, st, lastErr)
	return ""
}

// skippedOnB waits for B's dispatcher to skip activity and returns the
// audited reason; it fails if anything was queued for it.
func (g *likeGate) skippedOnB(activity string) string {
	g.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var reason string
		err := g.b.pool.QueryRow(ctx, `SELECT e.metadata->>'reason' FROM audit_events e
			JOIN activities a ON a.id::text = e.metadata->>'activity_id'
			WHERE e.event_type = 'federation.emission.skipped' AND a.activity_uri = $1`, activity).Scan(&reason)
		if err == nil {
			if n := g.countOn(g.b, `SELECT count(*) FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
				WHERE a.activity_uri = $1`, activity); n != 0 {
				g.t.Fatalf("%s skipped (%s) but %d delivery rows queued", activity, reason, n)
			}
			return reason
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("B never resolved %s", activity)
	return ""
}

func (g *likeGate) countOn(in *instance, sql string, args ...any) int {
	g.t.Helper()
	var n int
	if err := in.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

// stateOnA is what A holds for a post: remote like rows by the actor,
// the post's like_count, and like notifications to its author.
func (g *likeGate) stateOnA(post uuid.UUID, actorURI string) (rows, count, notes int) {
	g.t.Helper()
	rows = g.countOn(g.a, `SELECT count(*) FROM likes WHERE target_kind = 'post' AND target_id = $1 AND peer_id = $2 AND actor_uri = $3`,
		post, g.peerBonA, actorURI)
	count = g.countOn(g.a, `SELECT like_count FROM posts WHERE id = $1`, post)
	notes = g.countOn(g.a, `SELECT count(*) FROM notifications WHERE verb = 'post.like' AND target_id = $1`, post.String())
	return rows, count, notes
}

func (g *likeGate) wantOnA(t *testing.T, post uuid.UUID, actorURI string, rows, count, notes int) {
	t.Helper()
	if r, c, n := g.stateOnA(post, actorURI); r != rows || c != count || n != notes {
		t.Fatalf("A holds %d like rows, like_count %d, %d notifications; want %d, %d, %d", r, c, n, rows, count, notes)
	}
}

func TestTwoStack_LikeWire(t *testing.T) {
	g := newLikeGate(t)
	ctx := context.Background()
	owner := g.peerAonB.String()

	t.Run("allowed: the owner applies it once, also on a redelivery and a second Like", func(t *testing.T) {
		defer g.in(t)()
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		like := g.like(g.bob, post, uri, owner)
		if got := g.onA(like); got != "processed" {
			t.Fatalf("like: %s", got)
		}
		// The signed object is the canonical URI on A, and the envelope id
		// is the activity id B keeps (#1572's handle).
		var object string
		if err := g.a.pool.QueryRow(ctx, `SELECT envelope_json->>'object' FROM federation_inbox WHERE activity_uri = $1`, like).Scan(&object); err != nil {
			t.Fatal(err)
		}
		if object != uri {
			t.Fatalf("wire object %q, want %q", object, uri)
		}
		if n := g.countOn(g.b, `SELECT count(*) FROM activities WHERE activity_uri = $1 AND activity_type = 'Like' AND object_uri = $2`, like, uri); n != 1 {
			t.Fatalf("B's ledger holds %d Like rows for the activity", n)
		}
		g.wantOnA(t, post, g.bob.uri, 1, 1, 1)

		// B redelivers the same activity: answered from A's stored row.
		var before time.Time
		if err := g.b.pool.QueryRow(ctx, `UPDATE federation_outbox SET status = 'queued', next_attempt_at = now()
			WHERE activity_id = (SELECT id FROM activities WHERE activity_uri = $1) RETURNING last_attempt_at`, like).Scan(&before); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			var st string
			var last time.Time
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status, o.last_attempt_at FROM federation_outbox o
				JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, like).Scan(&st, &last)
			if st == "sent" && last.After(before) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("B never redelivered the Like (%q, last %s, before %s)", st, last, before)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, like); n != 1 {
			t.Fatalf("%d inbox rows for one activity", n)
		}
		g.wantOnA(t, post, g.bob.uri, 1, 1, 1)

		// A second Like by the same user is applied idempotently.
		again := g.like(g.bob, post, uri, owner)
		if got := g.onA(again); got != "processed" {
			t.Fatalf("second like: %s", got)
		}
		g.wantOnA(t, post, g.bob.uri, 1, 1, 1)
	})

	t.Run("wrong peer, foreign origin or no grant for the object: never queued", func(t *testing.T) {
		defer g.in(t)()
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		other, otherURI := g.postOnA() // A's post, not shared with B
		cases := map[string]string{
			"unknown peer":           g.like(g.bob, post, uri, uuid.NewString()),
			"peer of another origin": g.like(g.bob, post, "https://elsewhere.example/posts/"+post.String(), owner),
			"object not granted":     g.like(g.bob, other, otherURI, owner),
		}
		for name, id := range cases {
			if got := g.skippedOnB(id); got != "no_received_grant" {
				t.Errorf("%s: skipped %q, want no_received_grant", name, got)
			}
			if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, id); n != 0 {
				t.Errorf("%s reached A", name)
			}
		}
		g.wantOnA(t, post, g.bob.uri, 0, 0, 0)
		g.wantOnA(t, other, g.bob.uri, 0, 0, 0)
	})

	t.Run("a grant bound to one user covers only that user; downgrade and revoke stop the next Like", func(t *testing.T) {
		defer g.in(t)()
		post, uri := g.postOnA()
		wide := g.shareOnA(post, uri, "comment", "", nil)
		if got := g.onA(g.like(g.bob, post, uri, owner)); got != "processed" {
			t.Fatalf("under the peer-wide grant: %s", got)
		}
		// Downgrade: the peer-wide grant ends; eve alone is granted.
		g.revokeOnA(wide, uri, "")
		g.shareOnA(post, uri, "view", g.eve.uri, nil)
		if got := g.skippedOnB(g.like(g.bob, post, uri, owner)); got != "no_received_grant" {
			t.Fatalf("bob after the downgrade: skipped %q", got)
		}
		eveLike := g.like(g.eve, post, uri, owner)
		if got := g.onA(eveLike); got != "processed" {
			t.Fatalf("eve under her grant: %s", got)
		}
		g.wantOnA(t, post, g.bob.uri, 1, 2, 2)
		g.wantOnA(t, post, g.eve.uri, 1, 2, 2)
	})

	t.Run("revoked and expired grants stop the next Like", func(t *testing.T) {
		defer g.in(t)()
		post, uri := g.postOnA()
		sid := g.shareOnA(post, uri, "view", "", nil)
		g.revokeOnA(sid, uri, "")
		if got := g.skippedOnB(g.like(g.bob, post, uri, owner)); got != "no_received_grant" {
			t.Fatalf("revoked: skipped %q", got)
		}
		post2, uri2 := g.postOnA()
		exp := time.Now().Add(3 * time.Second)
		g.shareOnA(post2, uri2, "view", "", &exp)
		time.Sleep(time.Until(exp) + time.Second)
		if got := g.skippedOnB(g.like(g.bob, post2, uri2, owner)); got != "no_received_grant" {
			t.Fatalf("expired: skipped %q", got)
		}
		g.wantOnA(t, post, g.bob.uri, 0, 0, 0)
		g.wantOnA(t, post2, g.bob.uri, 0, 0, 0)
	})

	t.Run("a Like of a local post stays local, shared or not", func(t *testing.T) {
		defer g.in(t)()
		// B's own post, shared with A by B's admin: liking it through B's
		// production API sends nothing (public visibility or a share of a
		// local object is never a reason to send a Like anywhere).
		local := uuid.New()
		if _, err := g.b.pool.Exec(ctx, `INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Local', 'public')`,
			local, g.b.admin); err != nil {
			t.Fatal(err)
		}
		if st := g.api(g.b, http.MethodPost, "/admin/federation/shares",
			map[string]any{"object_kind": "post", "object_id": local, "peer_id": g.peerAonB, "scope": "view"}, nil); st != 201 {
			t.Fatalf("share on B: %d", st)
		}
		if st := g.api(g.b, http.MethodPost, "/posts/"+local.String()+"/like", nil, nil); st >= 300 {
			t.Fatalf("like through B's API: %d", st)
		}
		var like string
		if err := g.b.pool.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE activity_type = 'Like' AND object_local_id = $1`,
			local.String()).Scan(&like); err != nil {
			t.Fatal(err)
		}
		if got := g.skippedOnB(like); got != "recipient_set_empty" {
			t.Fatalf("local like: skipped %q, want recipient_set_empty", got)
		}
		if n := g.countOn(g.a, `SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, like); n != 0 {
			t.Fatal("a Like of B's own post reached A")
		}
	})

	t.Run("refused on arrival: same receipt whether private or missing, nothing written", func(t *testing.T) {
		defer g.in(t)()
		// Test fixture on A's disposable database: A's share ended without
		// its aa:Unshare having reached B yet, and a target deleted on A
		// while B's grant is live. B sends both; A refuses both.
		private, privURI := g.postOnA()
		g.shareOnA(private, privURI, "view", "", nil)
		if _, err := g.a.pool.Exec(ctx, `UPDATE federation_shares SET revoked_at = now() WHERE object_id = $1`, private); err != nil {
			t.Fatal(err)
		}
		missing, missURI := g.postOnA()
		g.shareOnA(missing, missURI, "view", "", nil)
		if _, err := g.a.pool.Exec(ctx, `UPDATE posts SET deleted_at = now() WHERE id = $1`, missing); err != nil {
			t.Fatal(err)
		}
		lp, lm := g.like(g.bob, private, privURI, owner), g.like(g.bob, missing, missURI, owner)
		if got := g.onA(lp); got != "rejected/unshared_object" {
			t.Fatalf("private: %s", got)
		}
		if got := g.onA(lm); got != "rejected/unknown_object" {
			t.Fatalf("missing: %s", got)
		}
		receipt := func(id string) string {
			var st, lastErr string
			_ = g.b.pool.QueryRow(ctx, `SELECT o.status, coalesce(o.last_error, '') FROM federation_outbox o
				JOIN activities a ON a.id = o.activity_id WHERE a.activity_uri = $1`, id).Scan(&st, &lastErr)
			return st + "|" + lastErr
		}
		if rp, rm := receipt(lp), receipt(lm); rp != rm || !strings.HasPrefix(rp, "sent|") {
			t.Fatalf("B's receipts differ or failed: private %q, missing %q", rp, rm)
		}
		g.wantOnA(t, private, g.bob.uri, 0, 0, 0)
		g.wantOnA(t, missing, g.bob.uri, 0, 0, 0)
	})

	t.Run("a disabled owner peer is not sent to", func(t *testing.T) {
		defer g.in(t)()
		post, uri := g.postOnA()
		g.shareOnA(post, uri, "view", "", nil)
		if st := g.api(g.b, http.MethodPatch, "/admin/federation/peers/"+owner, map[string]any{"enabled": false}, nil); st != 200 {
			t.Fatalf("disable: %d", st)
		}
		t.Cleanup(func() {
			g.api(g.b, http.MethodPatch, "/admin/federation/peers/"+owner, map[string]any{"enabled": true}, nil)
		})
		if got := g.skippedOnB(g.like(g.bob, post, uri, owner)); got != "no_received_grant" {
			t.Fatalf("disabled peer: skipped %q", got)
		}
		g.wantOnA(t, post, g.bob.uri, 0, 0, 0)
	})
}
