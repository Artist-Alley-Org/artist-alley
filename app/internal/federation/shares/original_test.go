// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1573: CanPeerFetchOriginal, the origin's original-byte decision,
// against a live database. Every case builds real rows (peer, user,
// asset, collection, membership, share) and asks the production
// function; nothing here re-implements the rule.
//
// Skips without AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

const origSize = int64(4096)

// origHash returns a fresh 64-hex hash so identical-hash cases are
// explicit and no case collides with another test's assets.
func origHash(t *testing.T) string {
	t.Helper()
	return randHex(t, 32)
}

// negotiate stores the peer's negotiated capabilities, as the handshake
// would. CanPeerFetchOriginal reads this column, never a caller flag.
func (fx *gateFixture) negotiate(t *testing.T, peerID uuid.UUID, caps ...string) {
	t.Helper()
	raw, _ := json.Marshal(caps)
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE federation_peers SET capabilities = $2::jsonb, capabilities_negotiated_at = now() WHERE id = $1`,
		peerID, string(raw)); err != nil {
		t.Fatalf("set capabilities: %v", err)
	}
}

// fixturePeerURL is the instance URL of the fixture's peer.
func (fx *gateFixture) peerURL(t *testing.T, peerID uuid.UUID) string {
	t.Helper()
	var u string
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT instance_url FROM federation_peers WHERE id = $1`, peerID).Scan(&u); err != nil {
		t.Fatalf("read peer url: %v", err)
	}
	return u
}

// newPeer adds a second connected, enabled peer with the profile
// negotiated.
func (fx *gateFixture) newPeer(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := fx.pool.QueryRow(context.Background(),
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Other Peer', 'k', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		"https://other-peer-"+randHex(t, 4)+".example", fx.grantorRef,
	).Scan(&id); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id = $1`, id)
		_, _ = fx.pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, id)
	})
	fx.negotiate(t, id, string(peer.CapOriginalTransfer1))
	return id
}

// newOriginalAsset inserts an asset with a stored original: the
// storage object row (the size the decision binds to) and the asset.
func (fx *gateFixture) newOriginalAsset(t *testing.T, ownerRef int64, tier, hash string) uuid.UUID {
	t.Helper()
	return fx.newOriginalAssetFrom(t, ownerRef, tier, hash, nil)
}

// newOriginalAssetFrom is newOriginalAsset for an asset whose origin is
// another instance (origin non-nil): a received copy.
func (fx *gateFixture) newOriginalAssetFrom(t *testing.T, ownerRef int64, tier, hash string, origin *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	tag, err := fx.pool.Exec(context.Background(),
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, $2, 'fs') ON CONFLICT (hash) DO NOTHING`,
		hash, origSize)
	if err != nil {
		t.Fatalf("insert storage object: %v", err)
	}
	if tag.RowsAffected() == 1 {
		t.Cleanup(func() {
			_, _ = fx.pool.Exec(context.Background(), `DELETE FROM storage_objects WHERE hash = $1`, hash)
		})
	}
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity, origin_server_id)
		 VALUES ($1, 'Original Test', 1, $2, $3, $4, $5, $6)`,
		id, ownerRef, hash, origSize, tier, origin,
	); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func (fx *gateFixture) fetchOriginal(t *testing.T, req shares.OriginalFetchRequest) shares.OriginalFetchDecision {
	t.Helper()
	if req.PeerID == (uuid.UUID{}) {
		req.PeerID = fx.peerID
	}
	if req.Size == 0 {
		req.Size = origSize
	}
	dec, err := fx.reg.CanPeerFetchOriginal(context.Background(), req)
	if err != nil {
		t.Fatalf("CanPeerFetchOriginal: %v", err)
	}
	return dec
}

func wantDeny(t *testing.T, dec shares.OriginalFetchDecision, want shares.RejectReason) {
	t.Helper()
	if dec.Allowed {
		t.Fatalf("allowed (share %v), want denied with %s", dec.Share, want)
	}
	if dec.Reason != want {
		t.Fatalf("reason = %q, want %q", dec.Reason, want)
	}
}

func wantAllow(t *testing.T, dec shares.OriginalFetchDecision, share *shares.Share) {
	t.Helper()
	if !dec.Allowed {
		t.Fatalf("denied with %q, want allowed", dec.Reason)
	}
	if dec.Share == nil || dec.Share.ID != share.ID {
		t.Fatalf("authorising share = %v, want %s", dec.Share, share.ID)
	}
}

// originalFixture: a negotiated peer and a public asset owned by the
// grantor.
func newOriginalFixture(t *testing.T) (*gateFixture, uuid.UUID, string) {
	t.Helper()
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, string(peer.CapOriginalTransfer1))
	hash := origHash(t)
	return fx, fx.newOriginalAsset(t, fx.grantorRef, "public", hash), hash
}

func assetShare(id uuid.UUID, scope federation.ShareScope, allow bool) shares.InsertInput {
	return shares.InsertInput{
		ObjectKind:    federation.ShareObjectKindAsset,
		ObjectID:      id,
		Scope:         scope,
		AllowOriginal: allow,
	}
}

func TestCanPeerFetchOriginal_DirectGrantAllowsPublicAndTeam(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	s := fx.insertShare(t, assetShare(asset, federation.ShareScopeView, true))
	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}), s)
	// Hash comparison is case-insensitive hex.
	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: strings.ToUpper(hash)}), s)

	teamHash := origHash(t)
	team := fx.newOriginalAsset(t, fx.grantorRef, "team", teamHash)
	ts := fx.insertShare(t, assetShare(team, federation.ShareScopeView, true))
	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: team, SHA256: teamHash}), ts)
}

// No scope implies the original, remix included; the same share still
// passes the interaction gate for view.
func TestCanPeerFetchOriginal_ScopeNeverImpliesOriginal(t *testing.T) {
	for _, scope := range []federation.ShareScope{
		federation.ShareScopeView, federation.ShareScopeComment,
		federation.ShareScopeAnnotate, federation.ShareScopeRemix,
	} {
		t.Run(string(scope), func(t *testing.T) {
			fx, asset, hash := newOriginalFixture(t)
			fx.insertShare(t, assetShare(asset, scope, false))
			wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}),
				shares.RejectReasonOriginalNotGranted)
			if dec := fx.gateAsset(t, asset); !dec.Allowed {
				t.Fatalf("the same %s share no longer passes the view gate: %s", scope, dec.Reason)
			}
		})
	}
}

// A higher-ranked share without the flag must not mask a lower-ranked
// one that has it, and must not lend it to another actor.
func TestCanPeerFetchOriginal_UnflaggedShareDoesNotMaskFlagged(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	base := fx.peerURL(t, fx.peerID)
	alice, bob := base+"/users/alice", base+"/users/bob"
	fx.insertShare(t, assetShare(asset, federation.ShareScopeRemix, false)) // anyone on the peer, no flag
	flagged := assetShare(asset, federation.ShareScopeView, true)
	flagged.TargetUserURL = &alice
	fs := fx.insertShare(t, flagged)

	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, ActorURL: alice}), fs)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, ActorURL: bob}),
		shares.RejectReasonOriginalNotGranted)
}

func TestCanPeerFetchOriginal_ActorBinding(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	base := fx.peerURL(t, fx.peerID)
	alice := base + "/users/alice"
	in := assetShare(asset, federation.ShareScopeView, true)
	in.TargetUserURL = &alice
	s := fx.insertShare(t, in)

	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, ActorURL: alice}), s)
	// Another actor of the same peer.
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, ActorURL: base + "/users/bob"}),
		shares.RejectReasonWrongUser)
	// An instance-level request cannot use a share that names a user.
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}),
		shares.RejectReasonWrongUser)
	// The same path on another origin is not this peer's actor.
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, ActorURL: "https://elsewhere.example/users/alice"}),
		shares.RejectReasonActorNotOnPeer)
}

func TestCanPeerFetchOriginal_WrongPeer(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	fx.insertShare(t, assetShare(asset, federation.ShareScopeRemix, true))
	other := fx.newPeer(t)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{PeerID: other, AssetID: asset, SHA256: hash}),
		shares.RejectReasonNoShareRow)
}

func TestCanPeerFetchOriginal_ExpiredAndRevoked(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	in := assetShare(asset, federation.ShareScopeView, true)
	past := pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	in.ExpiresAt = &past
	fx.insertShare(t, in)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}), shares.RejectReasonExpired)

	asset2Hash := origHash(t)
	asset2 := fx.newOriginalAsset(t, fx.grantorRef, "public", asset2Hash)
	s := fx.insertShare(t, assetShare(asset2, federation.ShareScopeView, true))
	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset2, SHA256: asset2Hash}), s)
	if _, err := fx.reg.Revoke(context.Background(), s.ID, fx.activityID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset2, SHA256: asset2Hash}), shares.RejectReasonNoShareRow)
}

// The decision reads federation_shares, not the per-object cache: a
// revoke written straight to the row (no cache invalidation at all) is
// seen at once.
func TestCanPeerFetchOriginal_ReadsTheRowNotTheCache(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	s := fx.insertShare(t, assetShare(asset, federation.ShareScopeView, true))
	// Warm the cache through the gate.
	if dec := fx.gateAsset(t, asset); !dec.Allowed {
		t.Fatalf("gate: %s", dec.Reason)
	}
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = $2 WHERE id = $1`,
		s.ID, fx.activityID); err != nil {
		t.Fatalf("revoke row: %v", err)
	}
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}), shares.RejectReasonNoShareRow)
}

func TestCanPeerFetchOriginal_TierCap(t *testing.T) {
	for _, tier := range []string{"restricted", "embargo"} {
		t.Run(tier, func(t *testing.T) {
			fx := newGateFixture(t)
			fx.negotiate(t, fx.peerID, string(peer.CapOriginalTransfer1))
			hash := origHash(t)
			asset := fx.newOriginalAsset(t, fx.grantorRef, tier, hash)
			// Written below the handler, which would have refused it.
			fx.insertShare(t, assetShare(asset, federation.ShareScopeRemix, true))
			wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}),
				shares.RejectReasonSensitivityRefused)
		})
	}
}

func TestCanPeerFetchOriginal_HashAndSizeBindToThisAsset(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	fx.insertShare(t, assetShare(asset, federation.ShareScopeView, true))
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: origHash(t)}), shares.RejectReasonHashMismatch)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, Size: origSize + 1}), shares.RejectReasonSizeMismatch)

	// Identical bytes on another asset (another owner: the hash is
	// unique only per owner) authorise nothing for that asset.
	other := fx.newUser(t)
	twin := fx.newOriginalAsset(t, other, "public", hash)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: twin, SHA256: hash}), shares.RejectReasonNoShareRow)
}

// The size is the stored object's, not the asset row's copy of it.
func TestCanPeerFetchOriginal_SizeIsTheStoredObjects(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	s := fx.insertShare(t, assetShare(asset, federation.ShareScopeView, true))
	if _, err := fx.pool.Exec(context.Background(), `UPDATE assets SET file_size_bytes = 1 WHERE id = $1`, asset); err != nil {
		t.Fatal(err)
	}
	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, Size: origSize}), s)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash, Size: 1}), shares.RejectReasonSizeMismatch)
}

// A copy received from another instance is not ours to send.
//
// #1596: the copy is remote from the moment its row exists, as a received
// asset is. A local asset with bytes is versioned and can no longer be
// turned into a remote one after the fact, which is how this test used
// to build it.
func TestCanPeerFetchOriginal_OnlyTheOrigin(t *testing.T) {
	fx, _, _ := newOriginalFixture(t)
	hash := origHash(t)
	received := fx.newOriginalAssetFrom(t, fx.grantorRef, "public", hash, &fx.peerID)
	fx.insertShare(t, assetShare(received, federation.ShareScopeView, true))
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: received, SHA256: hash}), shares.RejectReasonNotOrigin)
}

func TestCanPeerFetchOriginal_MissingDeletedOrNoOriginal(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	fx.insertShare(t, assetShare(asset, federation.ShareScopeView, true))
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: uuid.New(), SHA256: hash}), shares.RejectReasonUnknownObject)
	if _, err := fx.pool.Exec(context.Background(), `UPDATE assets SET deleted_at = now() WHERE id = $1`, asset); err != nil {
		t.Fatal(err)
	}
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}), shares.RejectReasonUnknownObject)

	bare := fx.newAsset(t, fx.grantorRef) // no stored original
	fx.insertShare(t, assetShare(bare, federation.ShareScopeView, true))
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: bare, SHA256: hash}), shares.RejectReasonUnknownObject)
}

// Peer state comes from the row. There is no caller flag to set.
func TestCanPeerFetchOriginal_PeerStateFromTheRow(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	fx.insertShare(t, assetShare(asset, federation.ShareScopeView, true))
	req := shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}

	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{PeerID: uuid.New(), AssetID: asset, SHA256: hash}), shares.RejectReasonUnknownPeer)

	fx.negotiate(t, fx.peerID, string(peer.CapNaClBox))
	wantDeny(t, fx.fetchOriginal(t, req), shares.RejectReasonProfileNotNegotiated)
	fx.negotiate(t, fx.peerID) // empty intersection
	wantDeny(t, fx.fetchOriginal(t, req), shares.RejectReasonProfileNotNegotiated)
	fx.negotiate(t, fx.peerID, string(peer.CapOriginalTransfer1))

	set := func(sql string) {
		t.Helper()
		if _, err := fx.pool.Exec(context.Background(), sql, fx.peerID); err != nil {
			t.Fatal(err)
		}
	}
	set(`UPDATE federation_peers SET status = 'pending_outbound' WHERE id = $1`)
	wantDeny(t, fx.fetchOriginal(t, req), shares.RejectReasonPeerNotConnected)
	set(`UPDATE federation_peers SET status = 'connected', enabled = FALSE WHERE id = $1`)
	wantDeny(t, fx.fetchOriginal(t, req), shares.RejectReasonPeerDisabled)
}

// A collection grant reaches a member asset only if the grantor could
// have shared that asset directly (#893), and only with the flag.
func TestCanPeerFetchOriginal_ContainerGrant(t *testing.T) {
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, string(peer.CapOriginalTransfer1))
	coll := fx.newCollection(t, fx.grantorRef)
	ownHash, foreignHash := origHash(t), origHash(t)
	own := fx.newOriginalAsset(t, fx.grantorRef, "public", ownHash)
	stranger := fx.newUser(t)
	foreign := fx.newOriginalAsset(t, stranger, "public", foreignHash)
	fx.addMember(t, coll, own)
	fx.addMember(t, coll, foreign)

	cs := fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection, ObjectID: coll,
		Scope: federation.ShareScopeView, AllowOriginal: true,
	})
	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: own, SHA256: ownHash}), cs)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: foreign, SHA256: foreignHash}), shares.RejectReasonGrantorNotOwner)

	// A member's own restricted tier is refused even through the grant.
	restrictedHash := origHash(t)
	restricted := fx.newOriginalAsset(t, fx.grantorRef, "restricted", restrictedHash)
	fx.addMember(t, coll, restricted)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: restricted, SHA256: restrictedHash}), shares.RejectReasonSensitivityRefused)

	// Removing the member ends the container route.
	if _, err := fx.pool.Exec(context.Background(),
		`DELETE FROM collection_resources WHERE collection_id = $1 AND asset_id = $2`, coll, own); err != nil {
		t.Fatal(err)
	}
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: own, SHA256: ownHash}), shares.RejectReasonNoShareRow)
}

func TestCanPeerFetchOriginal_ContainerWithoutFlag(t *testing.T) {
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, string(peer.CapOriginalTransfer1))
	coll := fx.newCollection(t, fx.grantorRef)
	hash := origHash(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", hash)
	fx.addMember(t, coll, asset)
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection, ObjectID: coll, Scope: federation.ShareScopeRemix,
	})
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}), shares.RejectReasonOriginalNotGranted)
}

func TestCanPeerFetchOriginal_AdminGrantorReachesForeignMember(t *testing.T) {
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, string(peer.CapOriginalTransfer1))
	admin := fx.newUser(t)
	fx.makeAdmin(t, admin)
	coll := fx.newCollection(t, admin)
	hash := origHash(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", hash)
	fx.addMember(t, coll, asset)
	cs := fx.insertShare(t, shares.InsertInput{
		GrantorUserRef: admin,
		ObjectKind:     federation.ShareObjectKindCollection, ObjectID: coll,
		Scope: federation.ShareScopeView, AllowOriginal: true,
	})
	wantAllow(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}), cs)
}

// The grantor-owner rule applies to direct shares too: a grant made
// before the asset changed hands confers nothing afterwards.
func TestCanPeerFetchOriginal_DirectGrantorNoLongerOwner(t *testing.T) {
	fx, asset, hash := newOriginalFixture(t)
	fx.insertShare(t, assetShare(asset, federation.ShareScopeView, true))
	newOwner := fx.newUser(t)
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, asset, newOwner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Hand it back so the new owner's row can be deleted first.
		_, _ = fx.pool.Exec(context.Background(), `UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, asset, fx.grantorRef)
	})
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: asset, SHA256: hash}), shares.RejectReasonGrantorNotOwner)
}

func TestInsertInput_AllowOriginalKinds(t *testing.T) {
	for _, kind := range []federation.ShareObjectKind{
		federation.ShareObjectKindPost, federation.ShareObjectKindWorkspace,
		federation.ShareObjectKindBrandKit, federation.ShareObjectKindUser,
	} {
		in := shares.InsertInput{ObjectKind: kind, Scope: federation.ShareScopeView, AllowOriginal: true}
		if err := in.Validate(); err != shares.ErrOriginalKindInvalid {
			t.Errorf("%s with allow_original: Validate = %v, want ErrOriginalKindInvalid", kind, err)
		}
		in.AllowOriginal = false
		if err := in.Validate(); err != nil {
			t.Errorf("%s without allow_original: Validate = %v", kind, err)
		}
	}
	for _, kind := range []federation.ShareObjectKind{federation.ShareObjectKindAsset, federation.ShareObjectKindCollection} {
		in := shares.InsertInput{ObjectKind: kind, Scope: federation.ShareScopeView, AllowOriginal: true}
		if err := in.Validate(); err != nil {
			t.Errorf("%s with allow_original: Validate = %v", kind, err)
		}
	}
}

func TestOriginalTierAllowed(t *testing.T) {
	for tier, want := range map[string]bool{
		"public": true, "team": true, "restricted": false, "embargo": false, "": false, "secret": false,
	} {
		if got := shares.OriginalTierAllowed(tier); got != want {
			t.Errorf("OriginalTierAllowed(%q) = %v, want %v", tier, got, want)
		}
	}
}
