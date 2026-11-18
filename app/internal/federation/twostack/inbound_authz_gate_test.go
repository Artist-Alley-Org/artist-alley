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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	return id, g.post(body)
}

func (g *authzGate) post(body []byte) int {
	g.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.a.url+"/federation/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("Host", req.URL.Host)
	if err := httpsig.SignAndAttach(req, body, g.b.url+"/federation/instance#main-key", g.bPriv); err != nil {
		g.t.Fatal(err)
	}
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
}
