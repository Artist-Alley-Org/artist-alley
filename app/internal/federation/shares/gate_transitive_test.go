// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #893 — a share on a collection confers scope on a member only if
// the SHARE'S GRANTOR could have shared that member directly.
//
// These are written as the attack, and the headline test has two
// halves on purpose: denying everything passes the escalation half
// and silently breaks federation sharing, so the grantor's OWN
// members are asserted in the same run.
//
// Red-first evidence: with the grantor-authority filter removed
// from matchContainingCollectionShares, the foreign-member
// assertions fail while the own-member ones keep passing.
//
// Fixture rows extend gateFixture (gate_test.go). Everything
// registers its own cleanup; t.Cleanup's LIFO order means members
// go before assets before users.

package shares_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

func (fx *gateFixture) newUser(t *testing.T) int64 {
	t.Helper()
	var ref int64
	if err := fx.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		"share-test-"+randHex(t, 4), "Share Test Other",
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM user_capability_grants WHERE user_ref = $1`, ref)
		_, _ = fx.pool.Exec(c, `DELETE FROM federation_shares WHERE grantor_user_ref = $1`, ref)
		_, _ = fx.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// makeAdmin gives the user a global system.admin capability — the
// same grant shape AdminHandler.GrantFederationShare's owner-check
// bypass reads.
func (fx *gateFixture) makeAdmin(t *testing.T, userRef int64) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code) VALUES ($1, 'system.admin')`,
		userRef,
	); err != nil {
		t.Fatalf("grant system.admin: %v", err)
	}
}

func (fx *gateFixture) newCollection(t *testing.T, ownerRef int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO collections (id, owner_user_ref, name, visibility)
		 VALUES ($1, $2, 'Test Coll', 'explicit-share')`,
		id, ownerRef,
	); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

func (fx *gateFixture) newAsset(t *testing.T, ownerRef int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO assets (id, title, asset_type, owner_user_ref) VALUES ($1, 'Test Asset', 1, $2)`,
		id, ownerRef,
	); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func (fx *gateFixture) addMember(t *testing.T, collectionID, assetID uuid.UUID) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO collection_resources (collection_id, asset_id) VALUES ($1, $2)`,
		collectionID, assetID,
	); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(),
			`DELETE FROM collection_resources WHERE collection_id = $1 AND asset_id = $2`,
			collectionID, assetID)
	})
}

// gateAsset asks the gate for view scope on one asset.
func (fx *gateFixture) gateAsset(t *testing.T, assetID uuid.UUID) shares.AccessDecision {
	t.Helper()
	dec, err := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindAsset,
		ObjectID:      assetID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if err != nil {
		t.Fatalf("CanPeerAccess: %v", err)
	}
	return dec
}

// TestCanPeerAccess_ContainerFallback_ForeignMemberGetsNoScope is
// the escalation itself: put someone else's asset in my collection,
// share the collection with a peer (which I am entitled to do — I
// own the collection), and see whether the peer gets scope over an
// object that was never mine to share.
func TestCanPeerAccess_ContainerFallback_ForeignMemberGetsNoScope(t *testing.T) {
	fx := newGateFixture(t)

	victimRef := fx.newUser(t)
	collectionID := fx.newCollection(t, fx.grantorRef)
	ownAsset := fx.newAsset(t, fx.grantorRef)
	foreignAsset := fx.newAsset(t, victimRef)
	fx.addMember(t, collectionID, ownAsset)
	fx.addMember(t, collectionID, foreignAsset)

	// The grantor shares the COLLECTION — permitted; they own it.
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection,
		ObjectID:   collectionID,
		Scope:      federation.ShareScopeView,
	})

	// Half one — the attack must fail.
	if dec := fx.gateAsset(t, foreignAsset); dec.Allowed {
		t.Errorf("escalation: peer reached an asset owned by user %d via a share granted by user %d (share=%v)",
			victimRef, fx.grantorRef, dec.Share)
	} else if dec.Reason != shares.RejectReasonGrantorNotOwner {
		t.Errorf("foreign member reason: got %q want %q", dec.Reason, shares.RejectReasonGrantorNotOwner)
	}

	// Half two — federation sharing still works. A fix that denies
	// everything passes half one and breaks the feature.
	dec := fx.gateAsset(t, ownAsset)
	if !dec.Allowed {
		t.Fatalf("regression: the grantor's OWN member should still be reachable; got reason=%q", dec.Reason)
	}
	if dec.Share == nil || dec.Share.ObjectKind != federation.ShareObjectKindCollection {
		t.Error("own member: matched share should be the collection-level grant")
	}
}

// The check is per member, not per collection: a foreign member
// must not ride in on the fact that the same collection also holds
// members the grantor does own.
func TestCanPeerAccess_ContainerFallback_PerMemberNotPerCollection(t *testing.T) {
	fx := newGateFixture(t)

	victimRef := fx.newUser(t)
	collectionID := fx.newCollection(t, fx.grantorRef)
	foreignAsset := fx.newAsset(t, victimRef)
	fx.addMember(t, collectionID, fx.newAsset(t, fx.grantorRef))
	fx.addMember(t, collectionID, foreignAsset)
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection,
		ObjectID:   collectionID,
		Scope:      federation.ShareScopeRemix,
	})

	if dec := fx.gateAsset(t, foreignAsset); dec.Allowed {
		t.Error("a shared collection's own-member grant must not spill onto a foreign member")
	}
}

// A member stays reachable through any container the grantor could
// have shared it through. Two collections hold the same asset: one
// shared by a stranger, one shared by the asset's owner. Only the
// owner's grant counts, and it must not be masked by the
// stranger's.
func TestCanPeerAccess_ContainerFallback_OwnerGrantNotMaskedByStranger(t *testing.T) {
	fx := newGateFixture(t)

	ownerRef := fx.newUser(t)
	asset := fx.newAsset(t, ownerRef)

	strangerColl := fx.newCollection(t, fx.grantorRef)
	ownerColl := fx.newCollection(t, ownerRef)
	fx.addMember(t, strangerColl, asset)
	fx.addMember(t, ownerColl, asset)

	// Stranger (the fixture grantor) shares their collection.
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection,
		ObjectID:   strangerColl,
		Scope:      federation.ShareScopeView,
	})
	if dec := fx.gateAsset(t, asset); dec.Allowed {
		t.Fatal("stranger's collection share must confer nothing on an asset they don't own")
	}

	// The asset's own owner shares THEIR collection.
	fx.insertShare(t, shares.InsertInput{
		GrantorUserRef: ownerRef,
		ObjectKind:     federation.ShareObjectKindCollection,
		ObjectID:       ownerColl,
		Scope:          federation.ShareScopeView,
	})
	dec := fx.gateAsset(t, asset)
	if !dec.Allowed {
		t.Fatalf("owner's own collection share should confer scope on their asset; got reason=%q", dec.Reason)
	}
	if dec.Share.ObjectID != ownerColl {
		t.Errorf("matched share should be the owner's collection %v; got %v", ownerColl, dec.Share.ObjectID)
	}
}

// Two shares on ONE collection, ranked so the unauthorised
// grantor's sorts first. Filtering after pickBestMatch would let it
// mask the owner's own lower-ranked grant; filtering before does
// not.
func TestCanPeerAccess_ContainerFallback_UnauthorizedShareDoesNotMaskOwnerShare(t *testing.T) {
	fx := newGateFixture(t)

	ownerRef := fx.newUser(t)
	asset := fx.newAsset(t, ownerRef)
	coll := fx.newCollection(t, fx.grantorRef)
	fx.addMember(t, coll, asset)

	// Stranger's share ranks HIGHER (remix > view).
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection,
		ObjectID:   coll,
		Scope:      federation.ShareScopeRemix,
	})
	// Owner's share ranks lower but is the one with authority.
	fx.insertShare(t, shares.InsertInput{
		GrantorUserRef: ownerRef,
		ObjectKind:     federation.ShareObjectKindCollection,
		ObjectID:       coll,
		Scope:          federation.ShareScopeView,
	})

	dec := fx.gateAsset(t, asset)
	if !dec.Allowed {
		t.Fatalf("owner's own grant must survive a higher-ranked unauthorised one; got reason=%q", dec.Reason)
	}
	if dec.Share.GrantorUserRef != ownerRef {
		t.Errorf("matched share grantor: got %d want %d", dec.Share.GrantorUserRef, ownerRef)
	}
	if dec.Share.Scope != federation.ShareScopeView {
		t.Errorf("matched share scope: got %q want %q", dec.Share.Scope, federation.ShareScopeView)
	}
}

// system.admin is what let a grantor share an object they don't own
// in the first place (GrantFederationShare bypasses the owner check
// for it), so the transitive grant honours the same bypass —
// otherwise an admin-granted collection share becomes a share of
// nothing.
func TestCanPeerAccess_ContainerFallback_AdminGrantorReachesForeignMember(t *testing.T) {
	fx := newGateFixture(t)

	ownerRef := fx.newUser(t)
	adminRef := fx.newUser(t)
	fx.makeAdmin(t, adminRef)

	coll := fx.newCollection(t, ownerRef)
	asset := fx.newAsset(t, ownerRef)
	fx.addMember(t, coll, asset)

	fx.insertShare(t, shares.InsertInput{
		GrantorUserRef: adminRef,
		ObjectKind:     federation.ShareObjectKindCollection,
		ObjectID:       coll,
		Scope:          federation.ShareScopeView,
	})

	dec := fx.gateAsset(t, asset)
	if !dec.Allowed {
		t.Fatalf("an admin's collection share should still reach members; got reason=%q", dec.Reason)
	}
	if dec.Share.GrantorUserRef != adminRef {
		t.Errorf("matched share grantor: got %d want %d", dec.Share.GrantorUserRef, adminRef)
	}
}

// An asset with no resolvable local owner (owner_user_ref is
// nullable — federated mirrors and system-imported rows) is
// nobody's to re-share, so the transitive grant fails closed on it.
func TestCanPeerAccess_ContainerFallback_NullOwnerMemberFailsClosed(t *testing.T) {
	fx := newGateFixture(t)

	coll := fx.newCollection(t, fx.grantorRef)
	assetID := uuid.New()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO assets (id, title, asset_type) VALUES ($1, 'Ownerless', 1)`, assetID,
	); err != nil {
		t.Fatalf("insert ownerless asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, assetID)
	})
	fx.addMember(t, coll, assetID)
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection,
		ObjectID:   coll,
		Scope:      federation.ShareScopeView,
	})

	if dec := fx.gateAsset(t, assetID); dec.Allowed {
		t.Error("an asset with no local owner must not be transitively shareable")
	}
}

// Cache coherence. The constraint depends on data the per-object
// share cache does not hold (membership, ownership), so it is read
// per call. Changing either flips the verdict with no share write —
// and therefore no cache invalidation anywhere. If this ever fails,
// someone folded the answer into the LRU and "{kind}:{uuid}" stopped
// identifying one value.
func TestCanPeerAccess_ContainerFallback_MembershipAndOwnershipAreNotCached(t *testing.T) {
	fx := newGateFixture(t)
	ctx := context.Background()

	victimRef := fx.newUser(t)
	coll := fx.newCollection(t, fx.grantorRef)
	asset := fx.newAsset(t, fx.grantorRef)
	fx.addMember(t, coll, asset)
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection,
		ObjectID:   coll,
		Scope:      federation.ShareScopeView,
	})

	// Prime: allowed, and the collection's share-set is now cached.
	if dec := fx.gateAsset(t, asset); !dec.Allowed {
		t.Fatalf("baseline should allow; got reason=%q", dec.Reason)
	}

	// Ownership change alone — no share row touched.
	if _, err := fx.pool.Exec(ctx,
		`UPDATE assets SET owner_user_ref = $1 WHERE id = $2`, victimRef, asset,
	); err != nil {
		t.Fatal(err)
	}
	if dec := fx.gateAsset(t, asset); dec.Allowed {
		t.Error("after the member changed owner the peer must lose scope; a cached verdict was served")
	}
	if _, err := fx.pool.Exec(ctx,
		`UPDATE assets SET owner_user_ref = $1 WHERE id = $2`, fx.grantorRef, asset,
	); err != nil {
		t.Fatal(err)
	}
	if dec := fx.gateAsset(t, asset); !dec.Allowed {
		t.Errorf("ownership restored: peer should regain scope; got reason=%q", dec.Reason)
	}

	// Membership change alone — also no share row touched.
	if _, err := fx.pool.Exec(ctx,
		`DELETE FROM collection_resources WHERE collection_id = $1 AND asset_id = $2`, coll, asset,
	); err != nil {
		t.Fatal(err)
	}
	if dec := fx.gateAsset(t, asset); dec.Allowed {
		t.Error("after removal from the shared collection the peer must lose scope")
	}
}
