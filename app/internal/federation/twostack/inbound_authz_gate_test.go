// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1570 gate: inbound likes and comments between two production
// instances. A holds the targets and grants through its real admin API.
// B's actors send signed Like / Create(Note) envelopes, signed with B's
// production instance key (HTTP Signature) and the actor's own key
// (pinned by A from B's real actor document), to A's real inbox over
// TLS; A's own inbox dispatcher applies them through the production
// SocialPoster and share gate. No production instance emits a like on a
// remote object yet (that needs #1574's received objects), so the test
// builds B's envelopes; everything that decides runs as deployed.

package twostack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

type authzGate struct {
	*cluster
	bPriv    []byte // B's instance key (raw ed25519)
	peerBonA uuid.UUID
}

type remoteActor struct {
	uri    string
	keyPEM string
}

func newAuthzGate(t *testing.T) *authzGate {
	t.Helper()
	c := newCluster(t)
	c.runWorkers(c.a)
	id, err := identity.NewManager(c.b.pool, c.logger).Load(context.Background())
	if err != nil {
		t.Fatalf("B identity: %v", err)
	}
	return &authzGate{cluster: c, bPriv: id.PrivateKey(), peerBonA: c.peerID(c.a, c.b)}
}

// actorOnB creates a user on B with its production signing key; A pins
// it from B's actor document on first use.
func (g *authzGate) actorOnB(name string) remoteActor {
	g.t.Helper()
	ctx := context.Background()
	username := name + uuid.NewString()[:6]
	var ref int64
	if err := g.b.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, name).Scan(&ref); err != nil {
		g.t.Fatal(err)
	}
	km, err := users.NewHandler(g.b.pool, g.logger, nil).EnsureActorKeyMaterial(ctx, ref, g.b.url, username)
	if err != nil {
		g.t.Fatal(err)
	}
	pemKey, err := km.DecryptSigningPrivateKey()
	if err != nil {
		g.t.Fatal(err)
	}
	return remoteActor{uri: g.b.url + "/users/" + username, keyPEM: string(pemKey)}
}

// send POSTs one activity from actor to A's inbox and returns its id and
// A's HTTP answer.
func (g *authzGate) send(actor remoteActor, typ, object string, members map[string]any) (string, int) {
	g.t.Helper()
	id, body := g.envelope(actor, typ, object, members)
	return id, g.post(body)
}

// envelope builds and signs one activity from actor, returning its id
// and its exact bytes.
func (g *authzGate) envelope(actor remoteActor, typ, object string, members map[string]any) (string, []byte) {
	g.t.Helper()
	id := g.b.url + "/activities/" + uuid.NewString()
	env := &federation.Envelope{Context: federation.ContextV1, Type: federation.ActivityType(typ), ID: id,
		Actor: actor.uri, Object: object, Published: time.Now().UTC().Truncate(time.Second)}
	if len(members) > 0 {
		env.Extra = map[string]json.RawMessage{}
		for k, v := range members {
			b, _ := json.Marshal(v)
			env.Extra[k] = b
		}
	}
	if err := env.Sign([]byte(actor.keyPEM), actor.uri+"#main-key"); err != nil {
		g.t.Fatal(err)
	}
	body, err := env.Marshal()
	if err != nil {
		g.t.Fatal(err)
	}
	return id, body
}

func (g *authzGate) post(body []byte) int {
	g.t.Helper()
	return g.postWith(g.signHeaders(body), body)
}

// signHeaders signs one delivery of body with B's instance key and
// returns the request headers (Date, Digest, Signature, ...).
func (g *authzGate) signHeaders(body []byte) http.Header {
	g.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.a.url+"/federation/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("Host", req.URL.Host)
	if err := httpsig.SignAndAttach(req, body, g.b.url+"/federation/instance#main-key", g.bPriv); err != nil {
		g.t.Fatal(err)
	}
	return req.Header
}

// postWith POSTs body to A's inbox with exactly these headers.
func (g *authzGate) postWith(h http.Header, body []byte) int {
	g.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.a.url+"/federation/inbox", bytes.NewReader(body))
	req.Header = h.Clone()
	resp, err := (&http.Client{Transport: g.tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// outcome waits for A's dispatcher to settle the activity and returns
// status/reason.
func (g *authzGate) outcome(activityURI string) string {
	g.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		var reason *string
		err := g.a.pool.QueryRow(context.Background(),
			`SELECT status, reject_reason FROM federation_inbox WHERE activity_uri = $1`, activityURI).Scan(&status, &reason)
		if err == nil && status != "pending" {
			if reason != nil {
				return status + "/" + *reason
			}
			return status
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("A never settled %s", activityURI)
	return ""
}

func (g *authzGate) newPost() (uuid.UUID, string) {
	g.t.Helper()
	id := uuid.New()
	if _, err := g.a.pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Gate post', 'explicit-share')`, id, g.a.admin); err != nil {
		g.t.Fatal(err)
	}
	return id, g.a.url + "/posts/" + id.String()
}

func (g *authzGate) share(kind string, id uuid.UUID, scope, target string) string {
	g.t.Helper()
	body := map[string]any{"object_kind": kind, "object_id": id, "peer_id": g.peerBonA, "scope": scope}
	if target != "" {
		body["target_user_url"] = target
	}
	var out struct {
		ID string `json:"id"`
	}
	if st := g.api(g.a, http.MethodPost, "/admin/federation/shares", body, &out); st != 201 {
		g.t.Fatalf("share %s %s: %d", kind, id, st)
	}
	return out.ID
}

func (g *authzGate) revokeShare(id string) {
	g.t.Helper()
	if st := g.api(g.a, http.MethodDelete, "/admin/federation/shares/"+id, nil, nil); st != 204 {
		g.t.Fatalf("revoke: %d", st)
	}
}

func (g *authzGate) countOnA(sql string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.a.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

func note(text string) map[string]any { return map[string]any{"object_type": "Note", "content": text} }

// delivery is one signed activity exactly as it went over the wire: the
// same activity id, envelope bytes and HTTP request headers (Date,
// Digest, Signature), so resending it is a true replay, not a new
// activity.
type delivery struct {
	id     string
	body   []byte
	header http.Header
}

// deliver signs and POSTs one activity, keeping it for replay.
func (g *authzGate) deliver(actor remoteActor, typ, object string, members map[string]any) (delivery, int) {
	g.t.Helper()
	id, body := g.envelope(actor, typ, object, members)
	d := delivery{id: id, body: body, header: g.signHeaders(body)}
	return d, g.postWith(d.header, d.body)
}

// replay resends the identical delivery.
func (g *authzGate) replay(d delivery) int {
	g.t.Helper()
	return g.postWith(d.header, d.body)
}

// inboxState is A's stored row for an activity: how many rows carry the
// id, and the first row's status, reason, attempts and last attempt.
func (g *authzGate) inboxState(activityURI string) string {
	g.t.Helper()
	var s string
	if err := g.a.pool.QueryRow(context.Background(), `SELECT count(*) OVER () || ' ' || status || '/' || coalesce(reject_reason, '')
		|| ' attempts=' || dispatch_attempts || ' last=' || coalesce(last_attempt_at::text, '-')
		FROM federation_inbox WHERE activity_uri = $1`, activityURI).Scan(&s); err != nil {
		g.t.Fatalf("inbox row %s: %v", activityURI, err)
	}
	return s
}

// commentsAndNotes counts A's comment rows for an activity and the
// post-author notifications naming those comments.
func (g *authzGate) commentsAndNotes(activityURI string) (rows, notes int) {
	g.t.Helper()
	rows = g.countOnA(`SELECT count(*) FROM comments WHERE activity_uri = $1`, activityURI)
	notes = g.countOnA(`SELECT count(*) FROM notifications n JOIN comments c ON n.payload->>'comment_id' = c.id::text
		WHERE n.verb = 'post.comment' AND c.activity_uri = $1`, activityURI)
	return rows, notes
}

// settleFence sends an activity that A refuses without writing (its
// target does not exist) and waits for it to settle: A's dispatcher has
// run after everything sent before it.
func (g *authzGate) settleFence(actor remoteActor) {
	g.t.Helper()
	id, _ := g.send(actor, "Like", g.a.url+"/posts/"+uuid.NewString(), nil)
	if got := g.outcome(id); got != "rejected/unknown_object" {
		g.t.Fatalf("fence: %s", got)
	}
}

// waitBlocked waits until a backend on A's database whose query matches
// like is waiting for a lock held by blocker, and returns its pid.
func (g *authzGate) waitBlocked(like string, blocker int) int {
	g.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := g.a.pool.QueryRow(context.Background(), `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1
			  AND $2::int = ANY (pg_blocking_pids(pid)) LIMIT 1`, like, blocker).Scan(&pid)
		if err == nil {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.t.Fatalf("no backend running %q waited on pid %d", like, blocker)
	return 0
}

// revokeAsync revokes a share through A's admin API from a goroutine
// and delivers the HTTP status (or -1 on a transport error).
func (g *authzGate) revokeAsync(id string) <-chan int {
	out := make(chan int, 1)
	req, _ := http.NewRequest(http.MethodDelete, g.a.url+"/api/v1/admin/federation/shares/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+g.a.token)
	go func() {
		resp, err := (&http.Client{Transport: g.tr, Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			out <- -1
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		out <- resp.StatusCode
	}()
	return out
}

func TestTwoStack_InboundAuthorization(t *testing.T) {
	g := newAuthzGate(t)
	bob, eve := g.actorOnB("bob"), g.actorOnB("eve")

	t.Run("no grant: refused, nothing written, same answer", func(t *testing.T) {
		post, uri := g.newPost()
		like, code := g.send(bob, "Like", uri, nil)
		missing, code2 := g.send(bob, "Like", g.a.url+"/posts/"+uuid.NewString(), nil)
		if code != http.StatusAccepted || code2 != code {
			t.Fatalf("answers: unshared %d, missing %d", code, code2)
		}
		if got := g.outcome(like); got != "rejected/unshared_object" {
			t.Fatalf("unshared: %s", got)
		}
		if got := g.outcome(missing); got != "rejected/unknown_object" {
			t.Fatalf("missing: %s", got)
		}
		if n := g.countOnA(`SELECT count(*) FROM likes WHERE target_id = $1`, post); n != 0 {
			t.Fatalf("%d likes", n)
		}
	})

	t.Run("direct grant: view admits a like, not a comment", func(t *testing.T) {
		post, uri := g.newPost()
		sid := g.share("post", post, "view", "")
		like, _ := g.send(bob, "Like", uri, nil)
		if got := g.outcome(like); got != "processed" {
			t.Fatalf("like under view: %s", got)
		}
		c, _ := g.send(bob, "Create", uri, note("under view"))
		if got := g.outcome(c); got != "rejected/unshared_object" {
			t.Fatalf("comment under view: %s", got)
		}
		// Replay: the stored row answers, nothing doubles.
		_, _ = g.send(bob, "Like", uri, nil)
		g.revokeShare(sid)
		g.share("post", post, "comment", "")
		c2, _ := g.send(bob, "Create", uri, note("under comment"))
		if got := g.outcome(c2); got != "processed" {
			t.Fatalf("comment under comment: %s", got)
		}
		if n := g.countOnA(`SELECT count(*) FROM likes WHERE target_id = $1 AND actor_uri = $2`, post, bob.uri); n != 1 {
			t.Fatalf("%d likes by bob", n)
		}
		if n := g.countOnA(`SELECT count(*) FROM comments WHERE target_id = $1`, post); n != 1 {
			t.Fatalf("%d comments", n)
		}
	})

	t.Run("actor-specific grant: only that actor", func(t *testing.T) {
		post, uri := g.newPost()
		g.share("post", post, "comment", bob.uri)
		ok, _ := g.send(bob, "Create", uri, note("bob"))
		no, _ := g.send(eve, "Create", uri, note("eve"))
		if got := g.outcome(ok); got != "processed" {
			t.Fatalf("bob: %s", got)
		}
		if got := g.outcome(no); got != "rejected/unshared_object" {
			t.Fatalf("eve: %s", got)
		}
	})

	t.Run("revoked and expired grants refuse the next write", func(t *testing.T) {
		post, uri := g.newPost()
		sid := g.share("post", post, "view", "")
		g.revokeShare(sid)
		like, _ := g.send(bob, "Like", uri, nil)
		if got := g.outcome(like); got != "rejected/unshared_object" {
			t.Fatalf("revoked: %s", got)
		}
		post2, uri2 := g.newPost()
		body := map[string]any{"object_kind": "post", "object_id": post2, "peer_id": g.peerBonA, "scope": "view",
			"expires_at": time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano)}
		if st := g.api(g.a, http.MethodPost, "/admin/federation/shares", body, nil); st != 201 {
			t.Fatalf("expiring share: %d", st)
		}
		time.Sleep(3 * time.Second)
		like2, _ := g.send(bob, "Like", uri2, nil)
		if got := g.outcome(like2); got != "rejected/unshared_object" {
			t.Fatalf("expired: %s", got)
		}
	})

	t.Run("transitive collection share, bounded by the grantor's ownership", func(t *testing.T) {
		ctx := context.Background()
		coll := uuid.New()
		if _, err := g.a.pool.Exec(ctx, `INSERT INTO collections (id, owner_user_ref, name, visibility) VALUES ($1, $2, 'Gate coll', 'explicit-share')`,
			coll, g.a.admin); err != nil {
			t.Fatal(err)
		}
		own, foreign := uuid.New(), uuid.New()
		var stranger int64
		if err := g.a.pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Stranger', 1) RETURNING ref`,
			"stranger-"+uuid.NewString()[:6]).Scan(&stranger); err != nil {
			t.Fatal(err)
		}
		for asset, owner := range map[uuid.UUID]int64{own: g.a.admin, foreign: stranger} {
			if _, err := g.a.pool.Exec(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref) VALUES ($1, 'Gate asset', 1, $2)`, asset, owner); err != nil {
				t.Fatal(err)
			}
			if _, err := g.a.pool.Exec(ctx, `INSERT INTO collection_resources (collection_id, asset_id) VALUES ($1, $2)`, coll, asset); err != nil {
				t.Fatal(err)
			}
		}
		// A's admin owns the collection and the "own" asset; the share
		// is theirs.
		g.share("collection", coll, "view", "")
		ownLike, _ := g.send(bob, "Like", g.a.url+"/assets/"+own.String(), nil)
		if got := g.outcome(ownLike); got != "processed" {
			t.Fatalf("own member: %s", got)
		}
		// The grantor holds system.admin, which may share anything, so
		// the foreign member is reachable too. Without it the
		// ownership bound (#893) refuses the member the grantor does
		// not own.
		if _, err := g.a.pool.Exec(ctx, `DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = 'system.admin'`, g.a.admin); err != nil {
			t.Fatal(err)
		}
		foreignLike, _ := g.send(bob, "Like", g.a.url+"/assets/"+foreign.String(), nil)
		if got := g.outcome(foreignLike); got != "rejected/unshared_object" {
			t.Fatalf("foreign member: %s", got)
		}
		if _, err := g.a.pool.Exec(ctx, `INSERT INTO user_capability_grants (user_ref, capability_code) VALUES ($1, 'system.admin')`, g.a.admin); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("concurrent revoke and writes: each write lands wholly before the revoke or not at all", func(t *testing.T) {
		post, uri := g.newPost()
		sid := g.share("post", post, "comment", "")
		const n = 12
		ids := make([]string, n)
		// The first half is applied while the share is live.
		for i := 0; i < n/2; i++ {
			ids[i], _ = g.send(bob, "Create", uri, note("before "+uuid.NewString()[:4]))
		}
		for i := 0; i < n/2; i++ {
			if got := g.outcome(ids[i]); got != "processed" {
				t.Fatalf("before the revoke: %s", got)
			}
		}
		// The second half races the revoke.
		var wg sync.WaitGroup
		for i := n / 2; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ids[i], _ = g.send(bob, "Create", uri, note("racing "+uuid.NewString()[:4]))
			}(i)
		}
		g.revokeShare(sid)
		wg.Wait()
		processed := 0
		for _, id := range ids {
			switch got := g.outcome(id); got {
			case "processed":
				processed++
			case "rejected/unshared_object":
			default:
				t.Fatalf("racing comment: %s", got)
			}
		}
		if c := g.countOnA(`SELECT count(*) FROM comments WHERE target_id = $1`, post); c != processed {
			t.Fatalf("%d comments for %d processed activities", c, processed)
		}
		// Every comment row belongs to a processed activity; after the
		// revoke has committed, nothing more lands.
		after, _ := g.send(bob, "Create", uri, note("after"))
		if got := g.outcome(after); got != "rejected/unshared_object" {
			t.Fatalf("after the revoke: %s", got)
		}
		if processed < n/2 {
			t.Fatalf("only %d processed; the first half was applied before the revoke", processed)
		}
		t.Logf("%d of %d comments landed (%d before the revoke, %d of the racing half)", processed, n, n/2, processed-n/2)
	})

	t.Run("exact replay: the stored row answers, one comment, one notification, live and after the revoke", func(t *testing.T) {
		post, uri := g.newPost()
		sid := g.share("post", post, "comment", "")
		c, code := g.deliver(bob, "Create", uri, note("replayed"))
		if code != http.StatusAccepted {
			t.Fatalf("first delivery: %d", code)
		}
		if got := g.outcome(c.id); got != "processed" {
			t.Fatalf("first delivery: %s", got)
		}
		stored := g.inboxState(c.id)
		// The identical request (same id, bytes and signature headers)
		// is answered from the stored row; it is not re-applied.
		if code := g.replay(c); code != http.StatusOK {
			t.Fatalf("live replay: %d, want 200 from the stored row", code)
		}
		g.settleFence(bob)
		if got := g.inboxState(c.id); got != stored {
			t.Fatalf("live replay changed the stored row: %s, was %s", got, stored)
		}
		if rows, notes := g.commentsAndNotes(c.id); rows != 1 || notes != 1 {
			t.Fatalf("live replay: %d comments, %d notifications", rows, notes)
		}
		// After the revoke the same delivery still gets the stored answer,
		// and nothing is applied, undone or re-decided.
		g.revokeShare(sid)
		if code := g.replay(c); code != http.StatusOK {
			t.Fatalf("replay after the revoke: %d", code)
		}
		g.settleFence(bob)
		if got := g.inboxState(c.id); got != stored {
			t.Fatalf("replay after the revoke changed the stored row: %s, was %s", got, stored)
		}
		if rows, notes := g.commentsAndNotes(c.id); rows != 1 || notes != 1 {
			t.Fatalf("replay after the revoke: %d comments, %d notifications", rows, notes)
		}
		// A refused activity replays as itself too, and the replay answer
		// does not tell the sender whether the target exists.
		unshared, _ := g.deliver(bob, "Create", uri, note("after the revoke"))
		missing, _ := g.deliver(bob, "Create", g.a.url+"/posts/"+uuid.NewString(), note("no such post"))
		if got := g.outcome(unshared.id); got != "rejected/unshared_object" {
			t.Fatalf("unshared: %s", got)
		}
		if got := g.outcome(missing.id); got != "rejected/unknown_object" {
			t.Fatalf("missing: %s", got)
		}
		su, sm := g.inboxState(unshared.id), g.inboxState(missing.id)
		cu, cm := g.replay(unshared), g.replay(missing)
		if cu != http.StatusOK || cm != cu {
			t.Fatalf("replay answers: unshared %d, missing %d", cu, cm)
		}
		g.settleFence(bob)
		if got := g.inboxState(unshared.id); got != su {
			t.Fatalf("unshared replay: %s, was %s", got, su)
		}
		if got := g.inboxState(missing.id); got != sm {
			t.Fatalf("missing replay: %s, was %s", got, sm)
		}
		if n := g.countOnA(`SELECT count(*) FROM comments WHERE target_id = $1`, post); n != 1 {
			t.Fatalf("%d comments on the post", n)
		}
		if n := g.countOnA(`SELECT count(*) FROM notifications WHERE verb = 'post.comment' AND target_id = $1`, post.String()); n != 1 {
			t.Fatalf("%d comment notifications on the post", n)
		}
	})

	t.Run("a write holding the grant row lock lands wholly before a competing revoke", func(t *testing.T) {
		ctx := context.Background()
		post, uri := g.newPost()
		sid := g.share("post", post, "comment", "")
		// Test fixture on A's disposable database: hold every comment
		// insert, so A's write transaction stops after its authorisation
		// has locked the share row FOR SHARE and before it inserts.
		hold, err := g.a.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = hold.Rollback(ctx)
			}
		}
		t.Cleanup(release)
		var holdPID int
		if err := hold.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holdPID); err != nil {
			t.Fatal(err)
		}
		if _, err := hold.Exec(ctx, `LOCK TABLE comments IN SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		held, _ := g.send(bob, "Create", uri, note("held"))
		writer := g.waitBlocked("%INSERT INTO comments%", holdPID)
		// The revoke's UPDATE of the share row now waits for the writer,
		// which holds that row FOR SHARE.
		revoked := g.revokeAsync(sid)
		g.waitBlocked("%UPDATE federation_shares%", writer)
		// A second write arrives while the revoke waits.
		behind, _ := g.send(bob, "Create", uri, note("behind the revoke"))
		select {
		case st := <-revoked:
			t.Fatalf("the revoke finished (%d) while the write held the share row", st)
		case <-time.After(300 * time.Millisecond):
		}
		if n := g.countOnA(`SELECT count(*) FROM comments WHERE target_id = $1`, post); n != 0 {
			t.Fatalf("%d comments before the held write committed", n)
		}
		release()
		select {
		case st := <-revoked:
			if st != http.StatusNoContent {
				t.Fatalf("revoke: %d", st)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the revoke never finished")
		}
		if got := g.outcome(held); got != "processed" {
			t.Fatalf("held write: %s", got)
		}
		if rows, notes := g.commentsAndNotes(held); rows != 1 || notes != 1 {
			t.Fatalf("held write: %d comments, %d notifications", rows, notes)
		}
		// The write queued behind the revoke either landed wholly before
		// it or was refused; with the revoke committed, the next is refused.
		behindOutcome := g.outcome(behind)
		after, _ := g.send(bob, "Create", uri, note("after"))
		if got := g.outcome(after); got != "rejected/unshared_object" {
			t.Fatalf("after the revoke: %s", got)
		}
		want := map[string]int{held: 1, behind: 0, after: 0}
		switch behindOutcome {
		case "processed":
			want[behind] = 1
		case "rejected/unshared_object":
		default:
			t.Fatalf("write behind the revoke: %s", behindOutcome)
		}
		for id, n := range want {
			if rows, notes := g.commentsAndNotes(id); rows != n || notes != n {
				t.Fatalf("%s: %d comments, %d notifications, want %d", id, rows, notes, n)
			}
		}
		if n := g.countOnA(`SELECT count(*) FROM comments WHERE target_id = $1`, post); n != want[held]+want[behind] {
			t.Fatalf("%d comments on the post", n)
		}
		t.Logf("held write processed before the revoke; the write behind it: %s", behindOutcome)
	})

	t.Run("a failed grant lookup writes nothing; the same activity applies on retry", func(t *testing.T) {
		ctx := context.Background()
		post, uri := g.newPost()
		g.share("post", post, "comment", "")
		// Fault fixture on A's disposable database only: the share table
		// is renamed away, so the authorisation's share lookup fails
		// with a database error (the target and peer lookups still work).
		rename := func(from, to string) {
			t.Helper()
			if err := pgx.BeginFunc(ctx, g.a.pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `ALTER TABLE `+from+` RENAME TO `+to)
				return err
			}); err != nil {
				t.Fatalf("rename %s: %v", from, err)
			}
		}
		rename("federation_shares", "federation_shares_r1570_fault")
		restored := false
		restore := func() {
			if !restored {
				restored = true
				rename("federation_shares_r1570_fault", "federation_shares")
			}
		}
		t.Cleanup(restore)
		c, code := g.deliver(bob, "Create", uri, note("retried"))
		if code != http.StatusAccepted {
			t.Fatalf("receipt during the fault: %d", code)
		}
		// A's dispatcher tries it, fails, and keeps it pending.
		deadline := time.Now().Add(30 * time.Second)
		var status, lastErr string
		var attempts int
		for {
			if err := g.a.pool.QueryRow(ctx, `SELECT status, dispatch_attempts, coalesce(last_error, '')
				FROM federation_inbox WHERE activity_uri = $1`, c.id).Scan(&status, &attempts, &lastErr); err != nil {
				t.Fatal(err)
			}
			if attempts > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("A never attempted the activity")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if status != "pending" || !strings.Contains(lastErr, "federation_shares") {
			t.Fatalf("after the failed lookup: %s, attempts %d, last error %q; want pending on the share lookup", status, attempts, lastErr)
		}
		// The same delivery replayed during the fault: the stored row
		// answers; still nothing written.
		if code := g.replay(c); code != http.StatusOK {
			t.Fatalf("replay during the fault: %d", code)
		}
		if rows, notes := g.commentsAndNotes(c.id); rows != 0 || notes != 0 {
			t.Fatalf("during the fault: %d comments, %d notifications", rows, notes)
		}
		t.Logf("during the fault: pending after %d attempt(s): %s", attempts, lastErr)
		restore()
		// A's own retry (its dispatcher's backstop tick) applies the SAME
		// activity, once.
		if got := g.outcome(c.id); got != "processed" {
			t.Fatalf("after the lookup works again: %s", got)
		}
		if rows, notes := g.commentsAndNotes(c.id); rows != 1 || notes != 1 {
			t.Fatalf("after the retry: %d comments, %d notifications", rows, notes)
		}
		if n := g.countOnA(`SELECT dispatch_attempts FROM federation_inbox WHERE activity_uri = $1`, c.id); n <= attempts {
			t.Fatalf("processed at attempt %d, not after the failed attempt(s) %d", n, attempts)
		}
		if code := g.replay(c); code != http.StatusOK {
			t.Fatalf("replay after the retry: %d", code)
		}
		g.settleFence(bob)
		if rows, notes := g.commentsAndNotes(c.id); rows != 1 || notes != 1 {
			t.Fatalf("after the retry and a replay: %d comments, %d notifications", rows, notes)
		}
	})
}
