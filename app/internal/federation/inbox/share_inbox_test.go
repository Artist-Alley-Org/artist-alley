// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// #1571: aa:Share / aa:Unshare through the real inbox (singleton and
// batch) and dispatcher, under share-lifecycle-1 and without it.

const shareLocal = "https://local.example"

// withProfiles rebuilds the fixture's handler for a peer whose stored
// intersection is caps, on a receiver whose origin is shareLocal.
func (fx *fixture) withProfiles(caps ...string) {
	fx.handler = inbox.NewHandler(inbox.HandlerDeps{
		Pool: fx.q,
		Lookup: &staticPeerLookup{peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID, pubKey: fx.peerPub,
			enabled: true, connected: true, capabilities: caps},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		RejectAudit:  fx.rejects.audit,
		LocalBaseURL: func(context.Context) string { return shareLocal },
	})
	fx.handler.SetActorKeyResolver(fx.actorKeys)
}

func (fx *fixture) shareEnvelope(typ federation.ActivityType, id, object string, members map[string]any) []byte {
	fx.t.Helper()
	if id == "" {
		id = fx.peerURL + "/activities/" + uuid.NewString()
	}
	if object == "" {
		object = fx.peerURL + "/assets/" + uuid.NewString()
	}
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      string(typ),
		"id":        id,
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    object,
		"signature": map[string]string{"type": "Ed25519", "publicKey": fx.peerURL + "/users/alice#main-key", "value": "AAAAAAAAAAAA"},
	}
	for k, v := range members {
		env[k] = v
	}
	b, err := json.Marshal(env)
	if err != nil {
		fx.t.Fatal(err)
	}
	return fx.signEnvelope(b)
}

func grantMembers(over map[string]any) map[string]any {
	m := map[string]any{"object_kind": "asset", "target_peer": shareLocal, "scope": "view"}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func TestInbox_ShareWithoutProfile_IsUnsupported_SingletonAndBatch(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles("nacl-box", "archival-1") // anything but share-lifecycle-1
	for _, typ := range []federation.ActivityType{federation.ActivityAAShare, federation.ActivityAAUnshare, federation.ActivityAARevokeShare} {
		members := grantMembers(nil)
		if typ != federation.ActivityAAShare {
			members = map[string]any{"object_kind": "asset", "target_peer": shareLocal, "aa:revokes": fx.peerURL + "/activities/g1"}
		}
		single := fx.shareEnvelope(typ, "", "", members)
		if rr := fx.postSingle(single); rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s singleton: %d %s", typ, rr.Code, rr.Body.String())
		}
		fx.assertStoredUnsupported(activityIDOf(t, single), single)
		batched := fx.shareEnvelope(typ, "", "", members)
		res := fx.postBatch(batched)
		if res[0].Status != "rejected" || res[0].Reason != string(federation.InboxStatusUnsupportedActivity) {
			t.Fatalf("%s batch: %+v", typ, res[0])
		}
		fx.assertStoredUnsupported(activityIDOf(t, batched), batched)
	}
}

func TestInbox_ShareUnderProfile_AcceptedAndStoredWithoutALocalObject(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(federation.ProfileShareLifecycle1)
	single := fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(nil))
	if rr := fx.postSingle(single); rr.Code != http.StatusAccepted {
		t.Fatalf("singleton: %d %s", rr.Code, rr.Body.String())
	}
	row, ok := fx.storedRow(activityIDOf(t, single))
	if !ok || row.Status != "pending" || row.ObjectKind != nil {
		t.Fatalf("stored %+v ok=%v; want pending with no local object reference", row, ok)
	}
	batched := fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"aa:supersedes": fx.peerURL + "/activities/g0"}))
	if res := fx.postBatch(batched); res[0].Status != "accepted" {
		t.Fatalf("batch: %+v", res[0])
	}
}

func TestInbox_ShareUnderProfile_RefusesWrongOriginsAndUnnegotiatedOriginal(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(federation.ProfileShareLifecycle1)
	cases := []struct {
		name   string
		env    []byte
		status int
		reason federation.InboxStatus
	}{
		{"another instance's object", fx.shareEnvelope(federation.ActivityAAShare, "", "https://third.example/assets/"+uuid.NewString(), grantMembers(nil)),
			http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"this instance's own object", fx.shareEnvelope(federation.ActivityAAShare, "", shareLocal+"/assets/"+uuid.NewString(), grantMembers(nil)),
			http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"addressed to another instance", fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"target_peer": "https://third.example"})),
			http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"forged reference", fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"aa:supersedes": "https://third.example/activities/g0"})),
			http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"edit scope", fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"scope": "edit"})),
			http.StatusBadRequest, federation.InboxStatusInvalidMember},
		{"original without original-transfer-1", fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"aa:originalTransfer": true})),
			http.StatusUnprocessableEntity, federation.InboxStatusProfileNotNegotiated},
		{"unshare that names no grant", fx.shareEnvelope(federation.ActivityAAUnshare, "", "", map[string]any{"object_kind": "asset", "target_peer": shareLocal}),
			http.StatusBadRequest, federation.InboxStatusInvalidMember},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := fx.postSingle(c.env)
			if rr.Code != c.status {
				t.Fatalf("status %d (%s), want %d", rr.Code, rr.Body.String(), c.status)
			}
			if n := fx.countRows(activityIDOf(t, c.env)); n != 0 {
				t.Fatalf("%d rows stored for a refused envelope", n)
			}
			if last := fx.rejects.last(); last == nil || last.reason != c.reason || last.activityURI != activityIDOf(t, c.env) {
				t.Errorf("last reject audited %+v, want %s for this envelope", last, c.reason)
			}
		})
	}
}

// The inbox is no oracle for local users: a grant for a user who exists
// and one for a name that does not get the same answer.
func TestInbox_ShareRecipientExistenceIsNotObservable(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(federation.ProfileShareLifecycle1)
	existing := fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"target_user": shareLocal + "/users/" + fx.localUser(t)}))
	missing := fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"target_user": shareLocal + "/users/nobody-" + randHex(4)}))
	a, b := fx.postSingle(existing), fx.postSingle(missing)
	if a.Code != b.Code || a.Body.String() != b.Body.String() || a.Code != http.StatusAccepted {
		t.Fatalf("existing user: %d %q; unknown user: %d %q", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
}

// localUser creates a user on the receiver and returns its name.
func (fx *fixture) localUser(t *testing.T) string {
	t.Helper()
	name := "inbox-share-" + randHex(4)
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'share recipient', 1)`, name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), `DELETE FROM "user" WHERE username = $1`, name) })
	return name
}

// The dispatcher applies a stored share to the grant record, refuses it
// if the peer's profile is gone by dispatch time, and an Unshare ends
// the grant it names.
func TestDispatch_ShareAppliesAndUnshareEnds_ProfileRecheckedAtDispatch(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(federation.ProfileShareLifecycle1)
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_inbound_grants WHERE peer_id = $1`, fx.peerID)
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_inbound_tombstones WHERE peer_id = $1`, fx.peerID)
	})
	store := inboundgrants.New(fx.pool, func(context.Context) string { return shareLocal }, nil, nil)
	caps := []string{federation.ProfileShareLifecycle1}
	newDispatcher := func() *inbox.Dispatcher {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		d := inbox.NewDispatcher(inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5}, fx.q,
			func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
				return inbox.PeerInfo{ID: id, InstanceURL: fx.peerURL, Enabled: true, Connected: true, Capabilities: caps}, nil
			}, nil, logger)
		d.SetRegistry(inbox.BuildRegistry(d, logger))
		d.SetInboundGrants(store)
		d.SetLocalBaseURL(func(context.Context) string { return shareLocal })
		return d
	}
	asset := fx.peerURL + "/assets/" + uuid.NewString()
	g1 := fx.peerURL + "/activities/" + uuid.NewString()
	share := fx.shareEnvelope(federation.ActivityAAShare, g1, asset, grantMembers(nil))
	if rr := fx.postSingle(share); rr.Code != http.StatusAccepted {
		t.Fatalf("share: %d", rr.Code)
	}
	row, _ := fx.storedRow(g1)
	if out := newDispatcher().DispatchRow(context.Background(), row); out != inbox.OutcomeProcessed {
		row, _ = fx.storedRow(g1)
		t.Fatalf("dispatch share: %v (%s)", out, row.LastError)
	}
	if _, err := store.Allows(context.Background(), inboundgrants.Query{PeerID: fx.peerID, ObjectURI: asset}); err != nil {
		t.Fatalf("applied grant does not authorise: %v", err)
	}

	// The profile is gone by dispatch time: a stored Unshare is refused
	// as unsupported and applies nothing.
	u1 := fx.peerURL + "/activities/" + uuid.NewString()
	unshare := fx.shareEnvelope(federation.ActivityAAUnshare, u1, asset,
		map[string]any{"object_kind": "asset", "target_peer": shareLocal, "aa:revokes": g1})
	if rr := fx.postSingle(unshare); rr.Code != http.StatusAccepted {
		t.Fatalf("unshare: %d", rr.Code)
	}
	row, _ = fx.storedRow(u1)
	caps = nil
	if out := newDispatcher().DispatchRow(context.Background(), row); out != inbox.OutcomeRejected {
		t.Fatalf("dispatch without the profile: %v", out)
	}
	row, _ = fx.storedRow(u1)
	if row.RejectReason == nil || *row.RejectReason != string(federation.InboxStatusUnsupportedActivity) {
		t.Fatalf("reject reason %v", row.RejectReason)
	}
	if _, err := store.Allows(context.Background(), inboundgrants.Query{PeerID: fx.peerID, ObjectURI: asset}); err != nil {
		t.Fatalf("an unapplied unshare ended the grant: %v", err)
	}

	// A fresh Unshare under the profile ends it.
	caps = []string{federation.ProfileShareLifecycle1}
	u2 := fx.peerURL + "/activities/" + uuid.NewString()
	unshare2 := fx.shareEnvelope(federation.ActivityAAUnshare, u2, asset,
		map[string]any{"object_kind": "asset", "target_peer": shareLocal, "aa:revokes": g1})
	if rr := fx.postSingle(unshare2); rr.Code != http.StatusAccepted {
		t.Fatalf("unshare 2: %d", rr.Code)
	}
	row, _ = fx.storedRow(u2)
	if out := newDispatcher().DispatchRow(context.Background(), row); out != inbox.OutcomeProcessed {
		t.Fatalf("dispatch unshare: %v", out)
	}
	if _, err := store.Allows(context.Background(), inboundgrants.Query{PeerID: fx.peerID, ObjectURI: asset}); err == nil {
		t.Fatal("the grant still authorises after its Unshare")
	}
}

// #1597 §16.7: aa:accessTransfer through the real inbox and dispatcher.
// At receipt and again at dispatch it is checked against the STORED
// intersection (the peer lookup's capability set), never against anything
// the envelope claims; the store records it independently of
// aa:originalTransfer, a successor without it records false, and the
// access class authorises only while the peer's stored capabilities hold
// every derived-transfer-1 profile.
func TestDispatch_ShareAccessTransfer_FromTheStoredIntersection(t *testing.T) {
	fx := newFixture(t)
	all := append([]string(nil), federation.DerivedTransferProfiles...)
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_inbound_grants WHERE peer_id = $1`, fx.peerID)
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_inbound_tombstones WHERE peer_id = $1`, fx.peerID)
	})
	setStoredCaps := func(caps []string) {
		raw, _ := json.Marshal(caps)
		if _, err := fx.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`,
			fx.peerID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	setStoredCaps(all)
	store := inboundgrants.New(fx.pool, func(context.Context) string { return shareLocal }, nil, nil)
	caps := all
	dispatch := func(id string) (inbox.DispatchOutcome, inbox.FederationInbox) {
		t.Helper()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		d := inbox.NewDispatcher(inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5}, fx.q,
			func(_ context.Context, pid uuid.UUID) (inbox.PeerInfo, error) {
				return inbox.PeerInfo{ID: pid, InstanceURL: fx.peerURL, Enabled: true, Connected: true, Capabilities: caps}, nil
			}, nil, logger)
		d.SetRegistry(inbox.BuildRegistry(d, logger))
		d.SetInboundGrants(store)
		d.SetLocalBaseURL(func(context.Context) string { return shareLocal })
		row, ok := fx.storedRow(id)
		if !ok {
			t.Fatalf("no stored row for %s", id)
		}
		out := d.DispatchRow(context.Background(), row)
		row, _ = fx.storedRow(id)
		return out, row
	}
	flags := func(id string) (original, access bool) {
		t.Helper()
		if err := fx.pool.QueryRow(context.Background(),
			`SELECT allow_original, allow_access_copies FROM federation_inbound_grants WHERE peer_id = $1 AND grant_activity_uri = $2`,
			fx.peerID, id).Scan(&original, &access); err != nil {
			t.Fatalf("read grant %s: %v", id, err)
		}
		return original, access
	}
	allows := func(asset, class string) error {
		_, err := store.Allows(context.Background(), inboundgrants.Query{PeerID: fx.peerID, ObjectURI: asset, Class: class})
		return err
	}

	// Receipt without the four profiles in the stored intersection: refused
	// as profile_not_negotiated, nothing stored. Without share-lifecycle-1
	// an aa:Share is not applied at all: 422 unsupported_activity, recorded
	// as rejected (§7.6), as for every share from such a peer.
	for _, missing := range all {
		var rest []string
		for _, p := range all {
			if p != missing {
				rest = append(rest, p)
			}
		}
		fx.withProfiles(rest...)
		env := fx.shareEnvelope(federation.ActivityAAShare, "", "", grantMembers(map[string]any{"aa:accessTransfer": true}))
		rr := fx.postSingle(env)
		if missing == federation.ProfileShareLifecycle1 {
			row, ok := fx.storedRow(activityIDOf(t, env))
			if rr.Code != http.StatusUnprocessableEntity || !ok || row.RejectReason == nil ||
				*row.RejectReason != string(federation.InboxStatusUnsupportedActivity) {
				t.Fatalf("without %s: %d, stored %v, reason %v; want 422 unsupported_activity, recorded rejected", missing, rr.Code, ok, row.RejectReason)
			}
			continue
		}
		if rr.Code != http.StatusUnprocessableEntity || fx.countRows(activityIDOf(t, env)) != 0 {
			t.Fatalf("without %s: %d, rows %d; want 422 and nothing stored", missing, rr.Code, fx.countRows(activityIDOf(t, env)))
		}
		if last := fx.rejects.last(); last == nil || last.reason != federation.InboxStatusProfileNotNegotiated {
			t.Fatalf("without %s: reject audited %+v", missing, last)
		}
	}

	// With all four: the access grant is recorded, alone.
	fx.withProfiles(all...)
	asset := fx.peerURL + "/assets/" + uuid.NewString()
	g1 := fx.peerURL + "/activities/" + uuid.NewString()
	if rr := fx.postSingle(fx.shareEnvelope(federation.ActivityAAShare, g1, asset, grantMembers(map[string]any{"aa:accessTransfer": true}))); rr.Code != http.StatusAccepted {
		t.Fatalf("share: %d %s", rr.Code, rr.Body.String())
	}
	if out, row := dispatch(g1); out != inbox.OutcomeProcessed {
		t.Fatalf("dispatch share: %v (%s)", out, row.LastError)
	}
	if original, access := flags(g1); original || !access {
		t.Fatalf("recorded allow_original=%v allow_access_copies=%v, want false / true", original, access)
	}
	if err := allows(asset, inboundgrants.ClassAccess); err != nil {
		t.Fatalf("the access grant does not authorise an access copy: %v", err)
	}
	if err := allows(asset, inboundgrants.ClassOriginal); err == nil {
		t.Fatal("the access grant authorises the original")
	}
	if err := allows(asset, ""); err != nil {
		t.Fatalf("the access grant does not authorise a non-byte use: %v", err)
	}

	// A successor without the member records false: nothing is inherited.
	g2 := fx.peerURL + "/activities/" + uuid.NewString()
	if rr := fx.postSingle(fx.shareEnvelope(federation.ActivityAAShare, g2, asset,
		grantMembers(map[string]any{"aa:supersedes": g1, "aa:originalTransfer": true}))); rr.Code != http.StatusAccepted {
		t.Fatalf("successor: %d %s", rr.Code, rr.Body.String())
	}
	if out, row := dispatch(g2); out != inbox.OutcomeProcessed {
		t.Fatalf("dispatch successor: %v (%s)", out, row.LastError)
	}
	if original, access := flags(g2); !original || access {
		t.Fatalf("successor recorded allow_original=%v allow_access_copies=%v, want true / false", original, access)
	}
	if err := allows(asset, inboundgrants.ClassAccess); err == nil {
		t.Fatal("the superseded access grant still authorises")
	}
	if err := allows(asset, inboundgrants.ClassOriginal); err != nil {
		t.Fatalf("the successor's original grant does not authorise: %v", err)
	}

	// Accepted at receipt, but the stored intersection lost a profile by
	// dispatch time: refused at dispatch, nothing recorded.
	g3 := fx.peerURL + "/activities/" + uuid.NewString()
	asset3 := fx.peerURL + "/assets/" + uuid.NewString()
	if rr := fx.postSingle(fx.shareEnvelope(federation.ActivityAAShare, g3, asset3, grantMembers(map[string]any{"aa:accessTransfer": true}))); rr.Code != http.StatusAccepted {
		t.Fatalf("share 3: %d", rr.Code)
	}
	caps = all[:3]
	if out, row := dispatch(g3); out != inbox.OutcomeRejected || row.RejectReason == nil ||
		*row.RejectReason != string(federation.InboxStatusProfileNotNegotiated) {
		t.Fatalf("dispatch without derived-transfer-1: %v reason %v", out, row.RejectReason)
	}
	var n int
	_ = fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_inbound_grants WHERE grant_activity_uri = $1`, g3).Scan(&n)
	if n != 0 {
		t.Fatalf("a refused share recorded %d grant row(s)", n)
	}
	caps = all

	// An ordinary revoke ends the access grant.
	g4 := fx.peerURL + "/activities/" + uuid.NewString()
	asset4 := fx.peerURL + "/assets/" + uuid.NewString()
	if rr := fx.postSingle(fx.shareEnvelope(federation.ActivityAAShare, g4, asset4, grantMembers(map[string]any{"aa:accessTransfer": true}))); rr.Code != http.StatusAccepted {
		t.Fatalf("share 4: %d", rr.Code)
	}
	if out, _ := dispatch(g4); out != inbox.OutcomeProcessed {
		t.Fatalf("dispatch share 4: %v", out)
	}
	if err := allows(asset4, inboundgrants.ClassAccess); err != nil {
		t.Fatalf("share 4 does not authorise: %v", err)
	}
	u4 := fx.peerURL + "/activities/" + uuid.NewString()
	if rr := fx.postSingle(fx.shareEnvelope(federation.ActivityAAUnshare, u4, asset4,
		map[string]any{"object_kind": "asset", "target_peer": shareLocal, "aa:revokes": g4})); rr.Code != http.StatusAccepted {
		t.Fatalf("unshare 4: %d", rr.Code)
	}
	if out, _ := dispatch(u4); out != inbox.OutcomeProcessed {
		t.Fatalf("dispatch unshare 4: %v", out)
	}
	if err := allows(asset4, inboundgrants.ClassAccess); err == nil {
		t.Fatal("the revoked access grant still authorises")
	}
}
