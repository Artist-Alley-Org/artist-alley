// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// #1574: aa:AssetVersion through the real inbox and dispatcher. It is
// applied only for a peer with archival-1 AND original-transfer-1, its
// object must be the sender's own asset, and the dispatcher hands it to
// the asset receiver (an AssetScheduler) and transfers nothing itself.
// The receiver and its worker against a real origin are tested in
// assetingest (worker_test.go) and twostack.

var assetProfiles = []string{federation.ProfileArchival1, federation.ProfileOriginalTransfer1}

// assetMember is a valid archival-1 aa:asset member naming object.
func assetMember(object string) map[string]any {
	hash := strings.Repeat("ab", 32)
	return map[string]any{federation.MemberArchivalAsset: map[string]any{
		"id":      object,
		"version": map[string]any{"id": object + "/versions/1", "number": 1, "created": "2026-11-20T10:00:00Z", "predecessor": nil},
		"files": []map[string]any{{"role": "original", "contentUri": "cas://" + hash,
			"checksums": []map[string]string{{"algorithm": "sha256", "value": hash}}, "size": 3, "mediaType": "image/png"}},
		"sensitivity": "public", "mature": false,
	}}
}

func (fx *fixture) assetEnvelope(object string, members map[string]any) []byte {
	fx.t.Helper()
	if object == "" {
		object = fx.peerURL + "/assets/" + uuid.NewString()
	}
	if members == nil {
		members = assetMember(object)
	}
	return fx.shareEnvelope(federation.ActivityAAAssetVersion, "", object, members)
}

// fakeScheduler records every call and answers with res / err.
type fakeScheduler struct {
	mu    sync.Mutex
	calls []string // activity ids
	peers []uuid.UUID
	res   inbox.AssetScheduled
	err   error
}

func (s *fakeScheduler) ScheduleAsset(_ context.Context, peerID uuid.UUID, env *federation.Envelope) (inbox.AssetScheduled, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, env.ID)
	s.peers = append(s.peers, peerID)
	return s.res, s.err
}

func (s *fakeScheduler) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }

func TestInbox_AssetVersion_AppliedOnlyUnderBothProfiles(t *testing.T) {
	fx := newFixture(t)
	cases := []struct {
		name   string
		caps   []string
		status int
		stored bool
	}{
		// aa:asset without archival-1: a profile member, refused unstored.
		{"no profile", nil, http.StatusUnprocessableEntity, false},
		{"original-transfer-1 only", []string{federation.ProfileOriginalTransfer1}, http.StatusUnprocessableEntity, false},
		// archival-1 alone: the member is valid but the type is not
		// applied, so it is recorded unsupported (§7.6).
		{"archival-1 only", []string{federation.ProfileArchival1, federation.ProfileShareLifecycle1}, http.StatusUnprocessableEntity, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx.withProfiles(c.caps...)
			env := fx.assetEnvelope("", nil)
			if rr := fx.postSingle(env); rr.Code != c.status {
				t.Fatalf("status %d (%s), want %d", rr.Code, rr.Body.String(), c.status)
			}
			if c.stored {
				fx.assertStoredUnsupported(activityIDOf(t, env), env)
			} else if n := fx.countRows(activityIDOf(t, env)); n != 0 {
				t.Fatalf("%d rows stored", n)
			}
		})
	}
	t.Run("both profiles", func(t *testing.T) {
		fx.withProfiles(assetProfiles...)
		env := fx.assetEnvelope("", nil)
		if rr := fx.postSingle(env); rr.Code != http.StatusAccepted {
			t.Fatalf("status %d (%s)", rr.Code, rr.Body.String())
		}
		row, ok := fx.storedRow(activityIDOf(t, env))
		if !ok || row.Status != "pending" || row.ObjectKind != nil {
			t.Fatalf("stored %+v ok=%v; want pending with no local object reference", row, ok)
		}
		if res := fx.postBatch(fx.assetEnvelope("", nil)); res[0].Status != "accepted" {
			t.Fatalf("batch: %+v", res[0])
		}
	})
}

func TestInbox_AssetVersion_RefusesForeignObjectAndBadMembers(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(assetProfiles...)
	other := "https://third.example/assets/" + uuid.NewString()
	own := fx.peerURL + "/assets/" + uuid.NewString()
	noAsset := map[string]any{}
	extra := assetMember(own)
	extra["content"] = "not an asset member"
	cases := []struct {
		name   string
		env    []byte
		status int
		reason federation.InboxStatus
	}{
		{"another instance's asset", fx.assetEnvelope(other, nil), http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"this receiver's own asset", fx.assetEnvelope(shareLocal+"/assets/"+uuid.NewString(), nil), http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"aa:asset names another object", fx.assetEnvelope(own, assetMember(fx.peerURL+"/assets/"+uuid.NewString())),
			http.StatusBadRequest, federation.InboxStatusArchivalInvalid},
		{"no aa:asset", fx.assetEnvelope(own, noAsset), http.StatusBadRequest, federation.InboxStatusArchivalInvalid},
		{"an undefined member", fx.assetEnvelope(own, extra), http.StatusBadRequest, federation.InboxStatusUnknownField},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rr := fx.postSingle(c.env); rr.Code != c.status {
				t.Fatalf("status %d (%s), want %d", rr.Code, rr.Body.String(), c.status)
			}
			if n := fx.countRows(activityIDOf(t, c.env)); n != 0 {
				t.Fatalf("%d rows stored for a refused envelope", n)
			}
			if last := fx.rejects.last(); last == nil || last.reason != c.reason {
				t.Errorf("last reject audited %+v, want %s", last, c.reason)
			}
		})
	}
}

func TestDispatch_AssetVersion_SchedulesWithoutTransferring(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(assetProfiles...)
	caps := assetProfiles
	dispatcher := func(s inbox.AssetScheduler) *inbox.Dispatcher {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		d := inbox.NewDispatcher(inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5}, fx.q,
			func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
				return inbox.PeerInfo{ID: id, InstanceURL: fx.peerURL, Enabled: true, Connected: true, Capabilities: caps}, nil
			}, nil, logger)
		d.SetRegistry(inbox.BuildRegistry(d, logger))
		d.SetLocalBaseURL(func(context.Context) string { return shareLocal })
		if s != nil {
			d.SetAssetScheduler(s)
		}
		return d
	}
	stored := func(t *testing.T) (inbox.FederationInbox, string) {
		t.Helper()
		env := fx.assetEnvelope("", nil)
		if rr := fx.postSingle(env); rr.Code != http.StatusAccepted {
			t.Fatalf("post: %d %s", rr.Code, rr.Body.String())
		}
		row, _ := fx.storedRow(activityIDOf(t, env))
		return row, activityIDOf(t, env)
	}
	reload := func(t *testing.T, id string) inbox.FederationInbox {
		t.Helper()
		row, ok := fx.storedRow(id)
		if !ok {
			t.Fatal("row gone")
		}
		return row
	}

	t.Run("a scheduled import marks the row processed once", func(t *testing.T) {
		s := &fakeScheduler{res: inbox.AssetScheduled{RecordID: uuid.New(), State: "pending"}}
		row, id := stored(t)
		if out := dispatcher(s).DispatchRow(context.Background(), row); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v (%s)", out, reload(t, id).LastError)
		}
		if s.count() != 1 || s.calls[0] != id || s.peers[0] != fx.peerID {
			t.Fatalf("scheduler calls %v peers %v", s.calls, s.peers)
		}
		if r := reload(t, id); r.Status != "processed" {
			t.Fatalf("row %s", r.Status)
		}
	})

	t.Run("a redelivery is answered from the stored row and schedules nothing", func(t *testing.T) {
		s := &fakeScheduler{res: inbox.AssetScheduled{RecordID: uuid.New(), State: "pending"}}
		env := fx.assetEnvelope("", nil)
		if rr := fx.postSingle(env); rr.Code != http.StatusAccepted {
			t.Fatalf("post: %d", rr.Code)
		}
		id := activityIDOf(t, env)
		d := dispatcher(s)
		if out := d.DispatchRow(context.Background(), reload(t, id)); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v", out)
		}
		for i := 0; i < 3; i++ {
			if rr := fx.postSingle(env); rr.Code != http.StatusOK {
				t.Fatalf("redelivery %d: %d", i, rr.Code)
			}
		}
		if r := reload(t, id); fx.countRows(id) != 1 || r.Status != "processed" || s.count() != 1 {
			t.Fatalf("rows %d (%s), scheduler calls %d", fx.countRows(id), r.Status, s.count())
		}
	})

	t.Run("a refusal the receiver recorded on a record is processed", func(t *testing.T) {
		s := &fakeScheduler{res: inbox.AssetScheduled{RecordID: uuid.New(), State: "rejected", Reason: "no_inbound_grant"},
			err: errors.New("no grant")}
		row, id := stored(t)
		if out := dispatcher(s).DispatchRow(context.Background(), row); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); r.Status != "processed" {
			t.Fatalf("row %s", r.Status)
		}
	})

	t.Run("a refusal without a record is rejected with its reason", func(t *testing.T) {
		s := &fakeScheduler{res: inbox.AssetScheduled{State: "rejected", Reason: "foreign_origin"}, err: errors.New("not the peer's asset")}
		row, id := stored(t)
		if out := dispatcher(s).DispatchRow(context.Background(), row); out != inbox.OutcomeRejected {
			t.Fatalf("dispatch: %v", out)
		}
		r := reload(t, id)
		if r.Status != "rejected" || r.RejectReason == nil || *r.RejectReason != string(federation.InboxStatusInvalidObject) ||
			!strings.Contains(r.LastError, "foreign_origin") {
			t.Fatalf("row %s %v %q", r.Status, r.RejectReason, r.LastError)
		}
	})

	t.Run("a transient failure stays pending for the dispatcher's retry", func(t *testing.T) {
		s := &fakeScheduler{res: inbox.AssetScheduled{Retryable: true, Reason: "storage_error"}, err: errors.New("db blip")}
		row, id := stored(t)
		if out := dispatcher(s).DispatchRow(context.Background(), row); out != inbox.OutcomeFailed {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); r.Status != "pending" || r.DispatchAttempts != 1 {
			t.Fatalf("row %s attempts %d", r.Status, r.DispatchAttempts)
		}
	})

	t.Run("an unwired receiver fails, never processed", func(t *testing.T) {
		row, id := stored(t)
		if out := dispatcher(nil).DispatchRow(context.Background(), row); out != inbox.OutcomeFailed {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); r.Status == "processed" {
			t.Fatal("processed without a receiver")
		}
	})

	t.Run("a profile gone by dispatch time schedules nothing", func(t *testing.T) {
		s := &fakeScheduler{res: inbox.AssetScheduled{RecordID: uuid.New(), State: "pending"}}
		row, id := stored(t)
		caps = []string{federation.ProfileArchival1}
		defer func() { caps = assetProfiles }()
		if out := dispatcher(s).DispatchRow(context.Background(), row); out != inbox.OutcomeRejected {
			t.Fatalf("dispatch: %v", out)
		}
		r := reload(t, id)
		if r.RejectReason == nil || *r.RejectReason != string(federation.InboxStatusUnsupportedActivity) || s.count() != 0 {
			t.Fatalf("reason %v, scheduler calls %d", r.RejectReason, s.count())
		}
	})
}

// #1575: an origin's Delete of its asset carrying aa:tombstone is
// admitted under archival-1 and handed to the asset receiver (an
// AssetWithdrawer), never to the comment path.

func tombstoneMember(object string) map[string]any {
	hash := strings.Repeat("ab", 32)
	return map[string]any{federation.MemberArchivalTombstone: map[string]any{
		"id": object, "formerType": "aa:Asset", "deleted": "2026-11-21T09:00:00Z", "lastVersion": object + "/versions/1",
		"files": []map[string]any{{"role": "original", "contentUri": "cas://" + hash,
			"checksums": []map[string]string{{"algorithm": "sha256", "value": hash}}}},
	}}
}

type fakeWithdrawer struct {
	mu    sync.Mutex
	calls []string
	peers []uuid.UUID
	res   inbox.AssetScheduled
	err   error
}

func (w *fakeWithdrawer) WithdrawAsset(_ context.Context, peerID uuid.UUID, env *federation.Envelope) (inbox.AssetScheduled, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, env.ID)
	w.peers = append(w.peers, peerID)
	return w.res, w.err
}

func (w *fakeWithdrawer) count() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.calls) }

func TestInbox_AssetWithdrawal_AdmittedOnlyUnderArchival(t *testing.T) {
	fx := newFixture(t)
	own := func() string { return fx.peerURL + "/assets/" + uuid.NewString() }
	fx.withProfiles()
	o := own()
	env := fx.shareEnvelope(federation.ActivityDelete, "", o, tombstoneMember(o))
	if rr := fx.postSingle(env); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("without archival-1: %d (%s)", rr.Code, rr.Body.String())
	}
	if n := fx.countRows(activityIDOf(t, env)); n != 0 {
		t.Fatalf("%d rows stored without archival-1", n)
	}

	fx.withProfiles(assetProfiles...)
	for name, c := range map[string]struct {
		object  string
		members map[string]any
		reason  federation.InboxStatus
	}{
		"another instance's asset":      {"https://third.example/assets/x", tombstoneMember("https://third.example/assets/x"), federation.InboxStatusInvalidMember},
		"a tombstone of another object": {o, tombstoneMember(own()), federation.InboxStatusArchivalInvalid},
		"object_type beside it": {o, func() map[string]any {
			m := tombstoneMember(o)
			m["object_type"] = "Note"
			return m
		}(), federation.InboxStatusUnknownField},
	} {
		t.Run(name, func(t *testing.T) {
			env := fx.shareEnvelope(federation.ActivityDelete, "", c.object, c.members)
			if rr := fx.postSingle(env); rr.Code != http.StatusBadRequest {
				t.Fatalf("status %d (%s)", rr.Code, rr.Body.String())
			}
			if n := fx.countRows(activityIDOf(t, env)); n != 0 {
				t.Fatalf("%d rows stored", n)
			}
			if last := fx.rejects.last(); last == nil || last.reason != c.reason {
				t.Errorf("last reject %+v, want %s", last, c.reason)
			}
		})
	}
	env = fx.shareEnvelope(federation.ActivityDelete, "", o, tombstoneMember(o))
	if rr := fx.postSingle(env); rr.Code != http.StatusAccepted {
		t.Fatalf("a valid withdrawal: %d (%s)", rr.Code, rr.Body.String())
	}
	if row, ok := fx.storedRow(activityIDOf(t, env)); !ok || row.Status != "pending" {
		t.Fatalf("stored %+v ok=%v", row, ok)
	}
}

func TestDispatch_AssetWithdrawal_GoesToTheAssetReceiver(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(assetProfiles...)
	dispatcher := func(w inbox.AssetWithdrawer) *inbox.Dispatcher {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		d := inbox.NewDispatcher(inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5}, fx.q,
			func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
				return inbox.PeerInfo{ID: id, InstanceURL: fx.peerURL, Enabled: true, Connected: true, Capabilities: assetProfiles}, nil
			}, nil, logger)
		d.SetRegistry(inbox.BuildRegistry(d, logger))
		d.SetLocalBaseURL(func(context.Context) string { return shareLocal })
		// No SocialPoster: a withdrawal never reaches the comment path.
		if w != nil {
			d.SetAssetWithdrawer(w)
		}
		return d
	}
	stored := func(t *testing.T) (inbox.FederationInbox, string) {
		t.Helper()
		o := fx.peerURL + "/assets/" + uuid.NewString()
		env := fx.shareEnvelope(federation.ActivityDelete, "", o, tombstoneMember(o))
		if rr := fx.postSingle(env); rr.Code != http.StatusAccepted {
			t.Fatalf("post: %d %s", rr.Code, rr.Body.String())
		}
		row, _ := fx.storedRow(activityIDOf(t, env))
		return row, activityIDOf(t, env)
	}
	reload := func(t *testing.T, id string) inbox.FederationInbox {
		t.Helper()
		row, ok := fx.storedRow(id)
		if !ok {
			t.Fatal("row gone")
		}
		return row
	}

	t.Run("applied: processed once", func(t *testing.T) {
		w := &fakeWithdrawer{res: inbox.AssetScheduled{RecordID: uuid.New(), State: "withdrawn"}}
		row, id := stored(t)
		if out := dispatcher(w).DispatchRow(context.Background(), row); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v (%s)", out, reload(t, id).LastError)
		}
		if w.count() != 1 || w.calls[0] != id || w.peers[0] != fx.peerID || reload(t, id).Status != "processed" {
			t.Fatalf("calls %v peers %v status %s", w.calls, w.peers, reload(t, id).Status)
		}
	})
	t.Run("a refusal is rejected with its reason", func(t *testing.T) {
		w := &fakeWithdrawer{res: inbox.AssetScheduled{Reason: "foreign_origin"}, err: errors.New("not the peer's asset")}
		row, id := stored(t)
		if out := dispatcher(w).DispatchRow(context.Background(), row); out != inbox.OutcomeRejected {
			t.Fatalf("dispatch: %v", out)
		}
		r := reload(t, id)
		if r.RejectReason == nil || *r.RejectReason != string(federation.InboxStatusInvalidObject) {
			t.Fatalf("row %s %v", r.Status, r.RejectReason)
		}
	})
	t.Run("a profile refusal is rejected as such", func(t *testing.T) {
		w := &fakeWithdrawer{res: inbox.AssetScheduled{Reason: "profile_not_negotiated"}, err: errors.New("no original-transfer-1")}
		row, id := stored(t)
		if out := dispatcher(w).DispatchRow(context.Background(), row); out != inbox.OutcomeRejected {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); r.RejectReason == nil || *r.RejectReason != string(federation.InboxStatusProfileNotNegotiated) {
			t.Fatalf("reason %v", r.RejectReason)
		}
	})
	t.Run("a transient failure stays pending", func(t *testing.T) {
		w := &fakeWithdrawer{res: inbox.AssetScheduled{Retryable: true, Reason: "storage_error"}, err: errors.New("db blip")}
		row, id := stored(t)
		if out := dispatcher(w).DispatchRow(context.Background(), row); out != inbox.OutcomeFailed {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); r.Status != "pending" || r.DispatchAttempts != 1 {
			t.Fatalf("row %s attempts %d", r.Status, r.DispatchAttempts)
		}
	})
	t.Run("an unwired receiver fails, never processed", func(t *testing.T) {
		row, id := stored(t)
		if out := dispatcher(nil).DispatchRow(context.Background(), row); out != inbox.OutcomeFailed {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); r.Status == "processed" {
			t.Fatal("processed without a receiver")
		}
	})
}
