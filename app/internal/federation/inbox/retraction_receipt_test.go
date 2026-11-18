// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1572: the receipt and dispatch gates admit an Undo only as an Undo of
// a Like, and a Delete only as a Delete of a comment. Every other Undo or
// Delete stays unsupported_activity, recorded and answered exactly like
// an unapplied verb (#1569); an admitted retraction names the sender's
// own activity, so it is never classified as a local object.

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

// retractionEnvelope is a signed Undo, Delete or Update (typ) from the
// fixture's actor. objectType "" omits the member; object "" names an
// activity on the sender; extra adds members (an Update's content).
func (fx *fixture) retractionEnvelope(typ federation.ActivityType, objectType, object string, extra ...map[string]any) []byte {
	if object == "" {
		object = fx.peerURL + "/activities/" + uuid.NewString()
	}
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      string(typ),
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
	for _, m := range extra {
		for k, v := range m {
			env[k] = v
		}
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
	ok := fx.retractionEnvelope(federation.ActivityUndo, "Like", "")
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
		env := fx.retractionEnvelope(federation.ActivityUndo, ot, "")
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
		"no object_type":             fx.retractionEnvelope(federation.ActivityUndo, "", ""),
		"object on another origin":   fx.retractionEnvelope(federation.ActivityUndo, "Like", "https://elsewhere.example/activities/"+uuid.NewString()),
		"object is a receiver post":  fx.retractionEnvelope(federation.ActivityUndo, "Like", "https://receiver.example/posts/"+uuid.NewString()),
		"object with a query string": fx.retractionEnvelope(federation.ActivityUndo, "Like", fx.peerURL+"/activities/x?y=1"),
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
	follow := fx.retractionEnvelope(federation.ActivityUndo, "Follow", "")
	like := fx.retractionEnvelope(federation.ActivityUndo, "Like", "")
	res := fx.postBatch(follow, like)
	if len(res) != 2 || res[0].Status != "rejected" || res[0].Reason != string(federation.InboxStatusUnsupportedActivity) ||
		res[1].Status != "accepted" {
		t.Fatalf("batch results %+v", res)
	}
	fx.assertStoredUnsupported(activityIDOf(t, follow), follow)
}

// countingUndoPoster counts every social write a dispatched Undo reaches.
type countingUndoPoster struct{ likes, undos, deletes, updates atomic.Int32 }

func (p *countingUndoPoster) InsertRemoteLike(context.Context, string, uuid.UUID, uuid.UUID, string, string) (bool, error) {
	p.likes.Add(1)
	return true, nil
}
func (p *countingUndoPoster) UndoRemoteLike(context.Context, inbox.RemoteUndoLikeInput) (bool, error) {
	p.undos.Add(1)
	return true, nil
}
func (p *countingUndoPoster) DeleteRemoteComment(context.Context, inbox.RemoteDeleteCommentInput) (bool, error) {
	p.deletes.Add(1)
	return true, nil
}
func (p *countingUndoPoster) UpdateRemoteComment(context.Context, inbox.RemoteUpdateCommentInput) (bool, error) {
	p.updates.Add(1)
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
		env := fx.retractionEnvelope(federation.ActivityUndo, ot, "")
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
	row := seed(fx.retractionEnvelope(federation.ActivityUndo, "Like", ""))
	if got := d.DispatchRow(context.Background(), row); got != inbox.OutcomeProcessed {
		t.Fatalf("Undo of a Like: outcome %v", got)
	}
	if poster.undos.Load() != 1 {
		t.Fatalf("Undo of a Like reached the social write %d times", poster.undos.Load())
	}
}

func TestPostInbox_DeleteOfCommentOnly(t *testing.T) {
	fx := newFixture(t)
	del := func(ot, object string) []byte { return fx.retractionEnvelope(federation.ActivityDelete, ot, object) }

	// A Delete of a comment is accepted and stored with no local object
	// reference: its object is the sender's Create.
	ok := del("Note", "")
	if rr := fx.postSingle(ok); rr.Code != http.StatusAccepted {
		t.Fatalf("Delete of a comment: %d %s", rr.Code, rr.Body.String())
	}
	row, found := fx.storedRow(activityIDOf(t, ok))
	if !found || row.Status != "pending" || row.ObjectKind != nil || row.ObjectID.Valid {
		t.Fatalf("stored %+v (found %v); want pending with no local object reference", row, found)
	}
	if rr := fx.postSingle(ok); rr.Code != http.StatusOK {
		t.Errorf("duplicate Delete: %d, want the 200 idempotent receipt", rr.Code)
	}

	// A Delete of anything else is unsupported, recorded, same answer
	// on redelivery.
	for _, ot := range []string{"aa:Post", "aa:Asset", "aa:Collection", "Tombstone", "Image"} {
		env := del(ot, "")
		id := activityIDOf(t, env)
		for i := 0; i < 2; i++ {
			rr := fx.postSingle(env)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Delete of %s delivery %d: %d %s", ot, i, rr.Code, rr.Body.String())
			}
			assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
		}
		fx.assertStoredUnsupported(id, env)
	}

	// Malformed Deletes are refused before anything is stored.
	for name, env := range map[string][]byte{
		"no object_type":            del("", ""),
		"object on another origin":  del("Note", "https://elsewhere.example/activities/"+uuid.NewString()),
		"object is a local comment": del("Note", "https://receiver.example/comments/"+uuid.NewString()),
		"object with a fragment":    del("Note", fx.peerURL+"/activities/x#f"),
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

	post := del("aa:Post", "")
	note := del("Note", "")
	res := fx.postBatch(post, note)
	if len(res) != 2 || res[0].Status != "rejected" || res[0].Reason != string(federation.InboxStatusUnsupportedActivity) ||
		res[1].Status != "accepted" {
		t.Fatalf("batch results %+v", res)
	}
}

// A pending Delete whose members say it is not of a comment is rejected
// at dispatch and never reaches the social write; a Delete of a comment
// reaches it with the envelope's own peer, actor, object and id.
func TestDispatcher_DeleteOfCommentOnly(t *testing.T) {
	fx := newFixture(t)
	poster := &countingUndoPoster{}
	d := fx.newDispatcher(nil)
	d.SetSocialPoster(poster)
	seed := func(env []byte) inbox.FederationInbox {
		row, err := fx.q.InsertInbox(context.Background(), inbox.InsertInboxParams{
			ActivityUri: activityIDOf(t, env), PeerID: pgUUID(fx.peerID), ActorUri: fx.peerURL + "/users/alice",
			ActivityType: string(federation.ActivityDelete), EnvelopeJson: env, HttpSigKey: fx.peerKeyID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	for _, ot := range []string{"aa:Post", "Tombstone", ""} {
		env := fx.retractionEnvelope(federation.ActivityDelete, ot, "")
		if got := d.DispatchRow(context.Background(), seed(env)); got != inbox.OutcomeRejected {
			t.Errorf("Delete of %q: outcome %v, want rejected", ot, got)
		}
	}
	if n := poster.deletes.Load(); n != 0 {
		t.Fatalf("a Delete that is not of a comment reached the social write %d times", n)
	}
	if got := d.DispatchRow(context.Background(), seed(fx.retractionEnvelope(federation.ActivityDelete, "Note", ""))); got != inbox.OutcomeProcessed {
		t.Fatalf("Delete of a comment: outcome %v", got)
	}
	if poster.deletes.Load() != 1 {
		t.Fatalf("Delete of a comment reached the social write %d times", poster.deletes.Load())
	}
}

func TestPostInbox_UpdateOfCommentOnly(t *testing.T) {
	fx := newFixture(t)
	upd := func(ot, object string, extra map[string]any) []byte {
		return fx.retractionEnvelope(federation.ActivityUpdate, ot, object, extra)
	}
	content := map[string]any{"content": "edited text"}

	ok := upd("Note", "", content)
	if rr := fx.postSingle(ok); rr.Code != http.StatusAccepted {
		t.Fatalf("Update of a comment: %d %s", rr.Code, rr.Body.String())
	}
	row, found := fx.storedRow(activityIDOf(t, ok))
	if !found || row.Status != "pending" || row.ObjectKind != nil || row.ObjectID.Valid {
		t.Fatalf("stored %+v (found %v); want pending with no local object reference", row, found)
	}
	if rr := fx.postSingle(ok); rr.Code != http.StatusOK {
		t.Errorf("duplicate Update: %d, want the 200 idempotent receipt", rr.Code)
	}
	for _, ot := range []string{"aa:Post", "aa:Asset", "Person", "Collection"} {
		env := upd(ot, "", content)
		rr := fx.postSingle(env)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("Update of %s: %d %s", ot, rr.Code, rr.Body.String())
		}
		assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
		fx.assertStoredUnsupported(activityIDOf(t, env), env)
	}
	for name, env := range map[string][]byte{
		"no object_type":            upd("", "", content),
		"no content":                upd("Note", "", nil),
		"blank content":             upd("Note", "", map[string]any{"content": "   "}),
		"inReplyTo is not a member": upd("Note", "", map[string]any{"content": "x", "inReplyTo": fx.peerURL + "/activities/p"}),
		"object on another origin":  upd("Note", "https://elsewhere.example/activities/"+uuid.NewString(), content),
	} {
		rr := fx.postSingle(env)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
			continue
		}
		if n := fx.countRows(activityIDOf(t, env)); n != 0 {
			t.Errorf("%s: %d rows stored", name, n)
		}
	}
}
