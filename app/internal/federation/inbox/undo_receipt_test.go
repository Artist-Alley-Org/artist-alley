// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1572: the receipt and dispatch gates admit an Undo only as an Undo of
// a Like. Every other Undo stays unsupported_activity, recorded and
// answered exactly like an unapplied verb (#1569); an Undo of a Like
// names the sender's own activity, so it is never classified as a local
// object.

package inbox_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// undoEnvelope is a signed Undo from the fixture's actor. objectType ""
// omits the member; object "" names an activity on the sender.
func (fx *fixture) undoEnvelope(objectType, object string) []byte {
	if object == "" {
		object = fx.peerURL + "/activities/" + uuid.NewString()
	}
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      string(federation.ActivityUndo),
		"id":        fx.peerURL + "/activities/" + uuid.NewString(),
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    object,
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	if objectType != "" {
		env["object_type"] = objectType
	}
	b, err := json.Marshal(env)
	if err != nil {
		fx.t.Fatal(err)
	}
	return fx.signEnvelope(b)
}

func TestPostInbox_UndoOfLikeOnly(t *testing.T) {
	fx := newFixture(t)

	// An Undo of a Like is accepted and stored with no local object
	// reference: its object is the sender's Like.
	ok := fx.undoEnvelope("Like", "")
	if rr := fx.postSingle(ok); rr.Code != http.StatusAccepted {
		t.Fatalf("Undo of a Like: %d %s", rr.Code, rr.Body.String())
	}
	row, found := fx.storedRow(activityIDOf(t, ok))
	if !found || row.Status != "pending" || row.ObjectKind != nil || row.ObjectID.Valid {
		t.Fatalf("stored %+v (found %v); want pending with no local object reference", row, found)
	}
	if rr := fx.postSingle(ok); rr.Code != http.StatusOK {
		t.Errorf("duplicate Undo of a Like: %d, want the 200 idempotent receipt", rr.Code)
	}

	// Any other Undo is unsupported: 422, recorded rejected with its
	// envelope, and the same answer on every redelivery.
	for _, ot := range []string{"Follow", "Block", "Create", "Announce", "Accept"} {
		env := fx.undoEnvelope(ot, "")
		id := activityIDOf(t, env)
		for i := 0; i < 2; i++ {
			rr := fx.postSingle(env)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Undo of %s delivery %d: %d %s", ot, i, rr.Code, rr.Body.String())
			}
			assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
		}
		fx.assertStoredUnsupported(id, env)
	}

	// Malformed Undos are refused before anything is stored.
	for name, env := range map[string][]byte{
		"no object_type":             fx.undoEnvelope("", ""),
		"object on another origin":   fx.undoEnvelope("Like", "https://elsewhere.example/activities/"+uuid.NewString()),
		"object is a receiver post":  fx.undoEnvelope("Like", "https://receiver.example/posts/"+uuid.NewString()),
		"object with a query string": fx.undoEnvelope("Like", fx.peerURL+"/activities/x?y=1"),
	} {
		rr := fx.postSingle(env)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
			continue
		}
		assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusInvalidMember)
		if n := fx.countRows(activityIDOf(t, env)); n != 0 {
			t.Errorf("%s: %d rows stored", name, n)
		}
	}

	// The batch endpoint answers each the same way.
	follow := fx.undoEnvelope("Follow", "")
	like := fx.undoEnvelope("Like", "")
	res := fx.postBatch(follow, like)
	if len(res) != 2 || res[0].Status != "rejected" || res[0].Reason != string(federation.InboxStatusUnsupportedActivity) ||
		res[1].Status != "accepted" {
		t.Fatalf("batch results %+v", res)
	}
	fx.assertStoredUnsupported(activityIDOf(t, follow), follow)
}

// countingUndoPoster counts every social write a dispatched Undo reaches.
type countingUndoPoster struct{ likes, undos atomic.Int32 }

func (p *countingUndoPoster) InsertRemoteLike(context.Context, string, uuid.UUID, uuid.UUID, string, string) (bool, error) {
	p.likes.Add(1)
	return true, nil
}
func (p *countingUndoPoster) UndoRemoteLike(context.Context, inbox.RemoteUndoLikeInput) (bool, error) {
	p.undos.Add(1)
	return true, nil
}
func (p *countingUndoPoster) InsertRemoteComment(context.Context, inbox.RemoteCommentInput) (uuid.UUID, bool, error) {
	return uuid.UUID{}, false, nil
}

// A pending Undo whose sealed or stored members say it is not an Undo of
// a Like (an encrypted envelope's object_type is first seen here, after
// decryption) is rejected unsupported_activity at dispatch and never
// reaches the social write. An Undo of a Like reaches it with the
// envelope's own peer, actor, object and id.
func TestDispatcher_UndoOfLikeOnly(t *testing.T) {
	fx := newFixture(t)
	poster := &countingUndoPoster{}
	seed := func(env []byte) inbox.FederationInbox {
		row, err := fx.q.InsertInbox(context.Background(), inbox.InsertInboxParams{
			ActivityUri:  activityIDOf(t, env),
			PeerID:       pgUUID(fx.peerID),
			ActorUri:     fx.peerURL + "/users/alice",
			ActivityType: string(federation.ActivityUndo),
			EnvelopeJson: env,
			HttpSigKey:   fx.peerKeyID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	d := fx.newDispatcher(nil)
	d.SetSocialPoster(poster)
	for _, ot := range []string{"Follow", "Block", ""} {
		env := fx.undoEnvelope(ot, "")
		row := seed(env)
		if got := d.DispatchRow(context.Background(), row); got != inbox.OutcomeRejected {
			t.Errorf("Undo of %q: outcome %v, want rejected", ot, got)
		}
		stored, _ := fx.storedRow(activityIDOf(t, env))
		want := string(federation.InboxStatusUnsupportedActivity)
		if ot == "" {
			want = string(federation.InboxStatusInvalidMember)
		}
		if stored.Status != "rejected" || stored.RejectReason == nil || *stored.RejectReason != want || stored.ProcessedAt.Valid {
			t.Errorf("Undo of %q: %s/%v, want rejected/%s and never processed", ot, stored.Status, stored.RejectReason, want)
		}
	}
	if n := poster.undos.Load() + poster.likes.Load(); n != 0 {
		t.Fatalf("an Undo that is not of a Like reached the social write %d times", n)
	}
	row := seed(fx.undoEnvelope("Like", ""))
	if got := d.DispatchRow(context.Background(), row); got != inbox.OutcomeProcessed {
		t.Fatalf("Undo of a Like: outcome %v", got)
	}
	if poster.undos.Load() != 1 {
		t.Fatalf("Undo of a Like reached the social write %d times", poster.undos.Load())
	}
}
