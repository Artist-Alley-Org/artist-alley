// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
)

// #1574 slice 3: an announcement bound before its grant waits for it
// (awaiting_grant), fetching nothing and using no attempt; the grant's
// arrival wakes exactly that peer's and asset's records; an existing grant
// row refuses at once; the wait is bounded.

// withWake installs the production arrival hook on the fixture's grant
// store (production wires it in internal/http).
func (f *fx) withWake() *fx {
	f.grants.SetArrivalHook(inboundgrants.ArrivalHook{InTx: assetingest.WakeAwaiting})
	return f
}

func (f *fx) signerFor() assetingest.SignerResolver {
	return func(string, string) (casfetch.Signer, error) { return f.signer, nil }
}

// due reports whether the record is due now (next_attempt_at <= now()).
func (f *fx) due(id uuid.UUID) bool {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM federation_received_assets WHERE id = $1 AND next_attempt_at <= now()`, id) == 1
}

func (f *fx) ledgerReasons(id uuid.UUID) []string {
	f.t.Helper()
	var out []string
	for _, r := range f.ledger(id) {
		out = append(out, r.eventType+"/"+r.outcome+"/"+str(r.reason))
	}
	return out
}

func TestAwait_AnnouncementBeforeGrantCompletesWhenTheGrantArrives(t *testing.T) {
	f := newFx(t).withWake()
	ctx := context.Background()
	a := f.newAsset(2)
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant || out.State != assetingest.StatePending {
		t.Fatalf("before the grant: %+v %v", out, err)
	}
	// Checked again while it waits: still waiting, no attempt counted,
	// nothing fetched, one ledger row for the wait.
	for i := 0; i < 3; i++ {
		res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
		if assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant || res.State != assetingest.StatePending {
			t.Fatalf("check %d: %+v %v", i, res, err)
		}
	}
	if n := f.count(`SELECT attempts FROM federation_received_assets WHERE id = $1`, out.RecordID); n != 0 {
		t.Fatalf("%d attempts counted while waiting", n)
	}
	if f.origin.heads.Load() != 0 || f.origin.getCount(a.hash[0]) != 0 {
		t.Fatal("the origin was asked while waiting for the grant")
	}
	if f.due(out.RecordID) {
		t.Fatal("a waiting record is due at once")
	}
	if got := f.ledgerReasons(out.RecordID); len(got) != 1 || got[0] != "ingestion/warning/awaiting_grant" {
		t.Fatalf("ledger while waiting: %v", got)
	}

	// The grant arrives: the record is due at once, and completes.
	f.grant(f.peerID, a.uri, nil)
	if !f.due(out.RecordID) {
		t.Fatal("the grant did not wake the waiting record")
	}
	res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
	if err != nil || res.State != assetingest.StateComplete {
		t.Fatalf("after the grant: %+v %v", res, err)
	}
	if n := f.count(`SELECT attempts FROM federation_received_assets WHERE id = $1`, out.RecordID); n != 1 {
		t.Fatalf("%d attempts after completing", n)
	}
	got := f.ledgerReasons(out.RecordID)
	if len(got) != 4 || got[0] != "ingestion/warning/awaiting_grant" || got[3] != "ingestion/success/" {
		t.Fatalf("ledger: %v", got)
	}
}

func TestAwait_TheWakeIsExact(t *testing.T) {
	f := newFx(t).withWake()
	a, b := f.newAsset(1), f.newAsset(1)
	outA, _ := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	outB, _ := f.receive(f.envelope(b, b.member(f.version(b), 1, "public")))
	if st, r := f.state(outA.RecordID); st != assetingest.StatePending || r != assetingest.ReasonAwaitingGrant {
		t.Fatalf("a: %s/%s", st, r)
	}
	// Another peer's grant for a's URI wakes nothing of this peer's.
	other := newOrigin(t)
	f.grant(f.addPeer(other.srv.URL, bothProfiles), a.uri, nil)
	if f.due(outA.RecordID) || f.due(outB.RecordID) {
		t.Fatal("another peer's grant woke this peer's records")
	}
	// A grant for a wakes a, not b.
	f.grant(f.peerID, a.uri, nil)
	if !f.due(outA.RecordID) {
		t.Fatal("the grant for a did not wake a")
	}
	if f.due(outB.RecordID) {
		t.Fatal("the grant for a woke b")
	}
}

func TestAwait_AnExistingGrantRowRefusesAtOnce(t *testing.T) {
	t.Run("revoked before the announcement", func(t *testing.T) {
		f := newFx(t).withWake()
		a := f.newAsset(1)
		f.revoke(f.peerID, a.uri, f.grant(f.peerID, a.uri, nil))
		_, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		f.nothingUsable(a.hash...)
	})
	t.Run("a share ended on arrival wakes a waiting record, which is refused", func(t *testing.T) {
		f := newFx(t).withWake()
		ctx := context.Background()
		a := f.newAsset(1)
		out, _ := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		past := time.Now().Add(-time.Hour)
		f.grant(f.peerID, a.uri, &past) // expired on arrival
		if !f.due(out.RecordID) {
			t.Fatal("a grant row ended on arrival did not wake the record")
		}
		res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if res.State != assetingest.StateRejected || f.origin.heads.Load() != 0 {
			t.Fatalf("outcome %+v, origin HEADs %d", res, f.origin.heads.Load())
		}
		got := f.ledgerReasons(out.RecordID)
		if len(got) != 2 || got[1] != "ingestion/failure/no_inbound_grant" {
			t.Fatalf("ledger: %v", got)
		}
	})
	t.Run("a revoke recorded before its share (tombstone), then the share", func(t *testing.T) {
		f := newFx(t).withWake()
		ctx := context.Background()
		a := f.newAsset(1)
		out, _ := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		share := f.origin.srv.URL + "/activities/share-" + uuid.NewString()
		f.revoke(f.peerID, a.uri, share) // tombstone; no grant row yet
		if st, r := f.state(out.RecordID); st != assetingest.StatePending || r != assetingest.ReasonAwaitingGrant {
			t.Fatalf("after the early revoke: %s/%s", st, r)
		}
		if _, err := f.grants.ApplyShare(ctx, inboundgrants.Share{
			PeerID: f.peerID, ActivityURI: share, GrantorActorURI: f.origin.srv.URL + "/users/alice",
			ObjectKind: federation.ShareObjectKindAsset, ObjectURI: a.uri, Scope: federation.ShareScopeView, AllowOriginal: true,
		}); err != nil {
			t.Fatal(err)
		}
		res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if res.State != assetingest.StateRejected {
			t.Fatalf("outcome %+v", res)
		}
	})
}

func TestAwait_TheWaitIsBounded(t *testing.T) {
	f := newFx(t, assetingest.Config{GrantWait: time.Second}).withWake()
	ctx := context.Background()
	a := f.newAsset(1)
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant {
		t.Fatalf("before the timeout: %v", err)
	}
	// Never scheduled past the wait.
	if n := f.count(`SELECT count(*) FROM federation_received_assets
		WHERE id = $1 AND next_attempt_at <= received_at + interval '1 second'`, out.RecordID); n != 1 {
		t.Fatal("the next check is scheduled past the wait")
	}
	time.Sleep(1200 * time.Millisecond)
	res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
	wantReason(t, err, casfetch.ReasonNoInboundGrant)
	if res.State != assetingest.StateRejected || f.origin.heads.Load() != 0 {
		t.Fatalf("after the wait: %+v", res)
	}
	if n := f.count(`SELECT attempts FROM federation_received_assets WHERE id = $1`, out.RecordID); n != 0 {
		t.Fatalf("%d attempts counted", n)
	}
	got := f.ledgerReasons(out.RecordID)
	if len(got) != 2 || got[0] != "ingestion/warning/awaiting_grant" || got[1] != "ingestion/failure/no_inbound_grant" {
		t.Fatalf("ledger: %v", got)
	}
	f.nothingUsable(a.hash...)
}

// TestAwait_NoLostWake: a check and a grant racing either see each other
// or leave the record due now; a waiting record is never left scheduled
// for later with its grant already recorded.
func TestAwait_NoLostWake(t *testing.T) {
	f := newFx(t).withWake()
	ctx := context.Background()
	for i := 0; i < 15; i++ {
		a := f.newAsset(1)
		out, _ := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		// Due now, so the check below runs its decision.
		if _, err := f.pool.Exec(ctx, `UPDATE federation_received_assets SET next_attempt_at = now() WHERE id = $1`, out.RecordID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = f.ingest.Process(ctx, out.RecordID, f.signerFor()) }()
		go func() { defer wg.Done(); f.grant(f.peerID, a.uri, nil) }()
		wg.Wait()
		if st, r := f.state(out.RecordID); st == assetingest.StatePending && r == assetingest.ReasonAwaitingGrant && !f.due(out.RecordID) {
			t.Fatalf("round %d: the grant is recorded but the record waits for later", i)
		}
	}
}

// TestAwait_AGrantDuringTheDecisionIsNotLost: a grant applied while the
// wait is being decided (after it found no grant row) cannot slip between
// the decision and its record. The decision holds the per-peer lock
// ApplyShare takes, so the share waits for it, then finds the record
// waiting and makes it due.
func TestAwait_AGrantDuringTheDecisionIsNotLost(t *testing.T) {
	f := newFx(t).withWake()
	ctx := context.Background()
	a := f.newAsset(1)
	out, _ := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if _, err := f.pool.Exec(ctx, `UPDATE federation_received_assets SET next_attempt_at = now() WHERE id = $1`, out.RecordID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	once := false
	assetingest.SetInAwait(f.ingest, func() {
		if once {
			return
		}
		once = true
		go func() { defer close(done); f.grant(f.peerID, a.uri, nil) }()
		// Give the share every chance to commit inside the decision.
		time.Sleep(300 * time.Millisecond)
	})
	res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
	if assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant {
		t.Fatalf("the decision: %+v %v", res, err)
	}
	<-done
	if !f.due(out.RecordID) {
		t.Fatal("the grant applied during the decision is recorded, but the record waits for later")
	}
}

// shareTo applies an aa:Share from the fixture's peer: kind, object URI,
// a target user ("" for the peer as a whole), and an optional expiry (a
// past one stores the grant ended on arrival).
func (f *fx) shareTo(kind federation.ShareObjectKind, objectURI, target string, expires ...time.Time) {
	f.t.Helper()
	var exp *time.Time
	if len(expires) > 0 {
		exp = &expires[0]
	}
	if _, err := f.grants.ApplyShare(context.Background(), inboundgrants.Share{
		PeerID: f.peerID, ActivityURI: f.origin.srv.URL + "/activities/share-" + uuid.NewString(),
		GrantorActorURI: f.origin.srv.URL + "/users/alice", ObjectKind: kind, ObjectURI: objectURI,
		Scope: federation.ShareScopeView, TargetUserURI: target, ExpiresAt: exp,
		// The original-file permission exists only for assets and
		// collections (federation_inbound_grants_allow_original_kind_check).
		AllowOriginal: kind == federation.ShareObjectKindAsset || kind == federation.ShareObjectKindCollection,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// localUserURI is a real local user's actor URI on the receiver, so a
// targeted share binds (a live grant to that one user).
func (f *fx) localUserURI() string {
	f.t.Helper()
	var name string
	if err := f.pool.QueryRow(context.Background(), `SELECT username FROM "user" WHERE ref = $1`, f.userRef).Scan(&name); err != nil {
		f.t.Fatal(err)
	}
	return localBase + "/users/" + name
}

// waiting asserts the record is pending awaiting_grant with no attempt
// and nothing asked of the origin.
func (f *fx) waiting(id uuid.UUID, a *asset) {
	f.t.Helper()
	if st, r := f.state(id); st != assetingest.StatePending || r != assetingest.ReasonAwaitingGrant {
		f.t.Fatalf("stored %s/%s, want pending/awaiting_grant", st, r)
	}
	if n := f.count(`SELECT attempts FROM federation_received_assets WHERE id = $1`, id); n != 0 {
		f.t.Fatalf("%d attempts", n)
	}
	if f.origin.heads.Load() != 0 || f.origin.getCount(a.hash[0]) != 0 {
		f.t.Fatal("the origin was asked")
	}
}

// TestAwait_OnlyThePeerWideAssetTupleDecides (rev599): the import is
// fetched as this instance, so only a grant of the asset to the peer as a
// whole decides or wakes it. A grant to one local user, or of another
// kind for the same URI, does neither; the peer-wide grant still does.
func TestAwait_OnlyThePeerWideAssetTupleDecides(t *testing.T) {
	t.Run("a targeted share stored first leaves it waiting; the peer-wide share completes it", func(t *testing.T) {
		f := newFx(t).withWake()
		ctx := context.Background()
		a := f.newAsset(1)
		f.shareTo(federation.ShareObjectKindAsset, a.uri, f.localUserURI())
		if n := f.count(`SELECT count(*) FROM federation_inbound_grants WHERE object_uri = $1 AND state = 'live' AND recipient_user_ref IS NOT NULL`, a.uri); n != 1 {
			t.Fatalf("the targeted share is not a live grant to the user (%d)", n)
		}
		out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		if assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant {
			t.Fatalf("with only a targeted grant: %+v %v", out, err)
		}
		f.waiting(out.RecordID, a)
		f.shareTo(federation.ShareObjectKindAsset, a.uri, "")
		if !f.due(out.RecordID) {
			t.Fatal("the peer-wide share did not wake the record")
		}
		if res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor()); err != nil || res.State != assetingest.StateComplete {
			t.Fatalf("after the peer-wide share: %+v %v", res, err)
		}
	})
	t.Run("a targeted share during the wait neither wakes nor refuses it", func(t *testing.T) {
		f := newFx(t).withWake()
		ctx := context.Background()
		a := f.newAsset(1)
		out, _ := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		f.shareTo(federation.ShareObjectKindAsset, a.uri, f.localUserURI())
		if f.due(out.RecordID) {
			t.Fatal("a targeted share woke the record")
		}
		if res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor()); assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant || res.State != assetingest.StatePending {
			t.Fatalf("after a targeted share: %+v %v", res, err)
		}
		f.waiting(out.RecordID, a)
	})
	// Rows of another kind for the same URI, ended on arrival (so no grant
	// path authorises anything): they must neither refuse the wait as a
	// terminal decision nor wake it. (A LIVE grant of another kind for the
	// same URI is matched by inboundgrants.AllowsTx's direct object_uri
	// clause, which does not check the kind; that live-grant path is
	// unchanged here and reported separately.)
	t.Run("another kind with the same URI neither decides nor wakes it", func(t *testing.T) {
		f := newFx(t).withWake()
		ctx := context.Background()
		a := f.newAsset(1)
		past := time.Now().Add(-time.Hour)
		f.shareTo(federation.ShareObjectKindCollection, a.uri, "", past)
		out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		if assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant {
			t.Fatalf("with only an ended collection row of the URI: %+v %v", out, err)
		}
		f.shareTo(federation.ShareObjectKindPost, a.uri, "", past)
		if f.due(out.RecordID) {
			t.Fatal("a share of another kind woke the record")
		}
		if res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor()); assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant || res.State != assetingest.StatePending {
			t.Fatalf("after other-kind shares: %+v %v", res, err)
		}
		f.waiting(out.RecordID, a)
	})
	t.Run("the peer-wide asset tuple, revoked, still refuses at once", func(t *testing.T) {
		f := newFx(t).withWake()
		a := f.newAsset(1)
		f.shareTo(federation.ShareObjectKindAsset, a.uri, f.localUserURI()) // irrelevant to the decision
		f.revoke(f.peerID, a.uri, f.grant(f.peerID, a.uri, nil))
		_, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		f.nothingUsable(a.hash...)
	})
}

// TestAwait_ALiveWrongKindGrantDoesNotAuthoriseTheOriginal (#1574 review,
// rev600): a live peer-wide collection-kind grant carrying allow_original
// whose object URI equals the announced asset's authorises no import (it
// covers no asset's original), so the record waits for its asset grant,
// fetching nothing; the asset grant then wakes it and it completes.
func TestAwait_ALiveWrongKindGrantDoesNotAuthoriseTheOriginal(t *testing.T) {
	f := newFx(t).withWake()
	ctx := context.Background()
	a := f.newAsset(1)
	f.shareTo(federation.ShareObjectKindCollection, a.uri, "")
	if n := f.count(`SELECT count(*) FROM federation_inbound_grants WHERE object_uri = $1 AND object_kind = 'collection'
		AND state = 'live' AND allow_original AND target_user_uri IS NULL`, a.uri); n != 1 {
		t.Fatalf("the wrong-kind grant is not live, peer-wide and carrying the original (%d)", n)
	}
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if assetingest.ReasonOf(err) != assetingest.ReasonAwaitingGrant || out.State != assetingest.StatePending {
		t.Fatalf("with only a live collection grant naming the asset: %+v %v", out, err)
	}
	f.waiting(out.RecordID, a)
	if f.due(out.RecordID) {
		t.Fatal("the wrong-kind grant left the record due")
	}
	// The asset grant, from another grantor of the origin (one grantor
	// cannot hold two live grants for one URI: the store ends both).
	if _, err := f.grants.ApplyShare(ctx, inboundgrants.Share{
		PeerID: f.peerID, ActivityURI: f.origin.srv.URL + "/activities/share-" + uuid.NewString(),
		GrantorActorURI: f.origin.srv.URL + "/users/bob", ObjectKind: federation.ShareObjectKindAsset, ObjectURI: a.uri,
		Scope: federation.ShareScopeView, AllowOriginal: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !f.due(out.RecordID) {
		t.Fatal("the asset grant did not wake the record")
	}
	res, err := f.ingest.Process(ctx, out.RecordID, f.signerFor())
	if err != nil || res.State != assetingest.StateComplete {
		t.Fatalf("after the asset grant: %+v %v", res, err)
	}
	var kind string
	if err := f.pool.QueryRow(ctx, `SELECT g.object_kind FROM federation_received_assets r
		JOIN federation_inbound_grants g ON g.id = r.authorizing_grant_id WHERE r.id = $1`, out.RecordID).Scan(&kind); err != nil || kind != "asset" {
		t.Fatalf("authorised by a %q grant (%v), want the asset grant", kind, err)
	}
}
