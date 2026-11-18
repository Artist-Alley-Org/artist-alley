// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/social"
)

// #1570: inbound likes and comments through the real inbox, dispatcher,
// social writes and share gate (the fixture's adapter mirrors api.go).

// unsharedPost is a second post of the owner, shared with nobody.
func (fx *pairedFixture) unsharedPost(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Unshared', 'private')`,
		id, fx.postOwnerRef); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id) })
	return id
}

func (fx *pairedFixture) envelopeFor(typ, object string, members map[string]any) ([]byte, string) {
	id := fx.senderBaseURL + "/activities/" + uuid.NewString()
	env := map[string]any{
		"@context": federation.ContextV1, "type": typ, "id": id, "actor": fx.senderActor,
		"published": time.Now().UTC().Format(time.RFC3339), "object": object,
		"signature": map[string]string{"type": "Ed25519", "publicKey": fx.senderActor + "#main-key", "value": "AAAAAAAAAAAA"},
	}
	for k, v := range members {
		env[k] = v
	}
	b, _ := json.Marshal(env)
	return signEnvelopeWith(fx.t, b, fx.actorPriv), id
}

func (fx *pairedFixture) postURL(id uuid.UUID) string {
	return fx.localBaseURL + "/posts/" + id.String()
}

func (fx *pairedFixture) deliverBatch(envs ...[]byte) []map[string]string {
	fx.t.Helper()
	raws := make([]json.RawMessage, len(envs))
	for i, e := range envs {
		raws[i] = e
	}
	body, _ := json.Marshal(map[string]any{"envelopes": raws})
	req, _ := http.NewRequest(http.MethodPost, fx.localBaseURL+"/federation/inbox/batch", bytes.NewReader(body))
	req.Header.Set("Host", "studio-a.example")
	if err := httpsig.SignAndAttach(req, body, fx.senderKeyID, fx.senderPriv); err != nil {
		fx.t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	fx.handler.PostInboxBatch(rr, req)
	if rr.Code != http.StatusOK {
		fx.t.Fatalf("batch: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Results []map[string]string `json:"results"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out.Results
}

func (fx *pairedFixture) rowStatus(activityURI string) (status, reason string) {
	var r *string
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT status, reject_reason FROM federation_inbox WHERE activity_uri = $1`, activityURI).Scan(&status, &r)
	if r != nil {
		reason = *r
	}
	return status, reason
}

func (fx *pairedFixture) count(sql string, args ...any) int {
	var n int
	if err := fx.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		fx.t.Fatal(err)
	}
	return n
}

func (fx *pairedFixture) nothingWritten(t *testing.T) {
	t.Helper()
	if n := fx.count(`SELECT count(*) FROM likes WHERE peer_id = $1`, fx.senderPeerID); n != 0 {
		t.Fatalf("%d likes written", n)
	}
	if n := fx.count(`SELECT count(*) FROM comments WHERE peer_id = $1`, fx.senderPeerID); n != 0 {
		t.Fatalf("%d comments written", n)
	}
	if fx.notifier.last() != nil {
		t.Fatalf("notification fired: %+v", fx.notifier.last())
	}
	if n := fx.count(`SELECT count(*) FROM federation_remote_actors WHERE peer_id = $1`, fx.senderPeerID); n != 0 {
		t.Fatal("the actor cache was updated before any authority")
	}
}

// The answer to the sender is the same whether the target is shared,
// private and unshared, or does not exist: the decision is made at
// dispatch and never sent. Singleton and batch.
func TestInboundAuthz_ResponseRevealsNothing(t *testing.T) {
	fx := newPairedFixture(t)
	private := fx.unsharedPost(t)
	targets := map[string]string{
		"shared": fx.postURL(fx.postID), "private": fx.postURL(private), "missing": fx.postURL(uuid.New()),
	}
	var codes []int
	var bodies []string
	var batch []string
	for _, name := range []string{"shared", "private", "missing"} {
		env, _ := fx.envelopeFor("Like", targets[name], nil)
		rr := fx.deliverToInbox(env)
		codes, bodies = append(codes, rr.Code), append(bodies, rr.Body.String())
		benv, _ := fx.envelopeFor("Like", targets[name], nil)
		res := fx.deliverBatch(benv)
		batch = append(batch, res[0]["status"]+"/"+res[0]["reason"])
	}
	for i := 1; i < 3; i++ {
		if codes[i] != codes[0] || bodies[i] != bodies[0] || batch[i] != batch[0] {
			t.Fatalf("answers differ: codes %v bodies %q batch %v", codes, bodies, batch)
		}
	}
	if codes[0] != http.StatusAccepted || batch[0] != "accepted/" {
		t.Fatalf("answers %v %v", codes, batch)
	}
}

func TestInboundAuthz_UnsharedAndUnknownTargetsWriteNothing(t *testing.T) {
	fx := newPairedFixture(t)
	private := fx.unsharedPost(t)
	cases := []struct {
		name, typ, object string
		members           map[string]any
		reason            federation.InboxStatus
	}{
		{"like on an unshared post", "Like", fx.postURL(private), nil, federation.InboxStatusUnsharedObject},
		{"comment on an unshared post", "Create", fx.postURL(private), map[string]any{"object_type": "Note", "content": "hi"}, federation.InboxStatusUnsharedObject},
		{"like on a missing post", "Like", fx.postURL(uuid.New()), nil, federation.InboxStatusUnknownObject},
		{"like on a kind without likes", "Like", fx.localBaseURL + "/collections/" + uuid.NewString(), nil, federation.InboxStatusUnknownObject},
		{"comment on a kind without comments", "Create", fx.localBaseURL + "/workspaces/" + uuid.NewString(), map[string]any{"object_type": "Note", "content": "hi"}, federation.InboxStatusUnknownObject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, uri := fx.envelopeFor(c.typ, c.object, c.members)
			if rr := fx.deliverToInbox(env); rr.Code != http.StatusAccepted {
				t.Fatalf("inbox: %d", rr.Code)
			}
			fx.dispatcher.RunOnce(context.Background())
			if st, r := fx.rowStatus(uri); st != "rejected" || r != string(c.reason) {
				t.Fatalf("row %s/%s, want rejected/%s", st, r, c.reason)
			}
			fx.nothingWritten(t)
			// A redelivery gets the stored row's idempotent answer
			// and changes nothing.
			if rr := fx.deliverToInbox(env); rr.Code != http.StatusOK {
				t.Fatalf("redelivery: %d", rr.Code)
			}
			fx.nothingWritten(t)
		})
	}
}

func TestInboundAuthz_RevokedShareStopsTheNextWrite(t *testing.T) {
	fx := newPairedFixture(t)
	ctx := context.Background()
	env, uri := fx.buildLikeEnvelope()
	fx.deliverToInbox(env)
	fx.dispatcher.RunOnce(ctx)
	if st, _ := fx.rowStatus(uri); st != "processed" {
		t.Fatalf("shared like: %s", st)
	}
	if _, err := fx.pool.Exec(ctx, `UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = granted_activity_id WHERE peer_id = $1`, fx.senderPeerID); err != nil {
		t.Fatal(err)
	}
	env2, uri2 := fx.buildCommentEnvelope("after the revoke")
	fx.deliverToInbox(env2)
	fx.dispatcher.RunOnce(ctx)
	if st, r := fx.rowStatus(uri2); st != "rejected" || r != string(federation.InboxStatusUnsharedObject) {
		t.Fatalf("after revoke: %s/%s", st, r)
	}
	if n := fx.count(`SELECT count(*) FROM comments WHERE peer_id = $1`, fx.senderPeerID); n != 0 {
		t.Fatal("a comment landed after the revoke")
	}
}

// A reply is never re-parented: a missing parent, a parent on another
// target, or a parent that is not this instance's comment refuses it.
func TestInboundAuthz_ReplyParentMustBeALiveCommentOnTheTarget(t *testing.T) {
	fx := newPairedFixture(t)
	ctx := context.Background()
	other := fx.unsharedPost(t)
	var onOther uuid.UUID
	if err := fx.pool.QueryRow(ctx, `INSERT INTO comments (id, target_kind, target_id, root_id, depth, author_user_ref, body, body_html)
		VALUES (gen_random_uuid(), 'post', $1, gen_random_uuid(), 0, $2, 'elsewhere', '') RETURNING id`, other, fx.postOwnerRef).Scan(&onOther); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), `DELETE FROM comments WHERE id = $1`, onOther) })
	note := func(inReplyTo string) map[string]any {
		return map[string]any{"object_type": "Note", "content": "reply", "inReplyTo": inReplyTo}
	}
	for name, c := range map[string]struct {
		inReplyTo string
		reason    federation.InboxStatus
	}{
		"missing parent":           {fx.localBaseURL + "/comments/" + uuid.NewString(), federation.InboxStatusUnknownObject},
		"parent on another post":   {fx.localBaseURL + "/comments/" + onOther.String(), federation.InboxStatusUnknownObject},
		"parent on another origin": {fx.senderBaseURL + "/comments/" + uuid.NewString(), federation.InboxStatusInvalidMember},
		"parent not a comment":     {fx.localBaseURL + "/posts/" + uuid.NewString(), federation.InboxStatusInvalidMember},
	} {
		t.Run(name, func(t *testing.T) {
			env, uri := fx.envelopeFor("Create", fx.postURL(fx.postID), note(c.inReplyTo))
			fx.deliverToInbox(env)
			fx.dispatcher.RunOnce(ctx)
			if st, r := fx.rowStatus(uri); st != "rejected" || r != string(c.reason) {
				t.Fatalf("row %s/%s, want rejected/%s", st, r, c.reason)
			}
			fx.nothingWritten(t)
		})
	}
	// inReplyTo naming the post itself is a top-level comment; a reply
	// to it lands under it.
	top, topURI := fx.envelopeFor("Create", fx.postURL(fx.postID), note(fx.postURL(fx.postID)))
	fx.deliverToInbox(top)
	fx.dispatcher.RunOnce(ctx)
	var topID uuid.UUID
	if err := fx.pool.QueryRow(ctx, `SELECT id FROM comments WHERE activity_uri = $1 AND parent_id IS NULL`, topURI).Scan(&topID); err != nil {
		t.Fatalf("top-level comment: %v", err)
	}
	reply, replyURI := fx.envelopeFor("Create", fx.postURL(fx.postID), note(fx.localBaseURL+"/comments/"+topID.String()))
	fx.deliverToInbox(reply)
	fx.dispatcher.RunOnce(ctx)
	var parent uuid.UUID
	var depth int
	if err := fx.pool.QueryRow(ctx, `SELECT parent_id, depth FROM comments WHERE activity_uri = $1`, replyURI).Scan(&parent, &depth); err != nil || parent != topID || depth != 1 {
		t.Fatalf("reply: parent %s depth %d err %v", parent, depth, err)
	}
}

// failingPoster: the authorizer cannot decide (a database error). The
// row stays pending for a retry and nothing is written; the retry, once
// the gate answers, writes exactly once.
type failingPoster struct {
	h    *social.Handler
	fail bool
	real inbox.SocialPoster
}

func (p *failingPoster) InsertRemoteLike(ctx context.Context, kind string, id, peerID uuid.UUID, actor string) (bool, error) {
	if p.fail {
		return p.h.InsertRemoteLike(ctx, kind, id, peerID, actor, func(context.Context, pgx.Tx) error {
			return errors.New("simulated: could not read federation_shares")
		})
	}
	return p.real.InsertRemoteLike(ctx, kind, id, peerID, actor)
}

func (p *failingPoster) InsertRemoteComment(ctx context.Context, in inbox.RemoteCommentInput) (uuid.UUID, bool, error) {
	return p.real.InsertRemoteComment(ctx, in)
}

func TestInboundAuthz_LookupErrorWritesNothingAndRetries(t *testing.T) {
	fx := newPairedFixture(t)
	ctx := context.Background()
	poster := &failingPoster{h: fx.socialH, fail: true, real: fx.poster}
	fx.dispatcher.SetSocialPoster(poster)
	env, uri := fx.buildLikeEnvelope()
	fx.deliverToInbox(env)
	_, _, failed := fx.dispatcher.RunOnce(ctx)
	if failed != 1 {
		t.Fatalf("failed = %d", failed)
	}
	if st, _ := fx.rowStatus(uri); st != "pending" {
		t.Fatalf("row %s after a lookup error, want pending (retry)", st)
	}
	fx.nothingWritten(t)
	poster.fail = false
	fx.dispatcher.RunOnce(ctx)
	if st, _ := fx.rowStatus(uri); st != "processed" {
		t.Fatalf("retry: %s", st)
	}
	if n := fx.count(`SELECT count(*) FROM likes WHERE peer_id = $1`, fx.senderPeerID); n != 1 {
		t.Fatalf("%d likes after the retry", n)
	}
	// And a duplicate delivery after that is a no-op.
	fx.deliverToInbox(env)
	fx.dispatcher.RunOnce(ctx)
	if n := fx.count(`SELECT count(*) FROM likes WHERE peer_id = $1`, fx.senderPeerID); n != 1 {
		t.Fatalf("%d likes after a replay", n)
	}
}

// A social write without an authorizer never writes.
func TestInboundAuthz_SocialRefusesWithoutAnAuthorizer(t *testing.T) {
	fx := newPairedFixture(t)
	if _, err := fx.socialH.InsertRemoteLike(context.Background(), "post", fx.postID, fx.senderPeerID, fx.senderActor, nil); !errors.Is(err, social.ErrNoAuthorizer) {
		t.Fatalf("nil authorizer: %v", err)
	}
	if _, _, err := fx.socialH.InsertRemoteComment(context.Background(), social.RemoteCommentInput{
		TargetKind: "post", TargetID: fx.postID, PeerID: fx.senderPeerID, ActorURI: fx.senderActor,
		ActivityURI: fx.senderBaseURL + "/activities/x", Body: "b"}); !errors.Is(err, social.ErrNoAuthorizer) {
		t.Fatalf("nil authorizer: %v", err)
	}
	fx.nothingWritten(t)
}
