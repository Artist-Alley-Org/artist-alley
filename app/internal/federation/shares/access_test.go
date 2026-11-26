// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: CanPeerFetchAccessCopy, the origin's access-copy decision,
// against a live database. Every case builds real rows: a sealed version
// holding a promoted, verified access copy exactly as migration 00085
// requires (the copy is an object of its own, verified, recorded as a
// variant of the version's original, and pinned by a promotion), this
// origin's announcement claim to the peer, and real shares; then it asks
// the production function. Nothing here re-implements the rule. All
// fixtures are ordinary rows; no network is used.
//
// Skips without AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

const accessSize = int64(1024)

// accessProfiles is every profile derived-transfer-1 needs in the stored
// intersection (§16.7).
var accessProfiles = []string{
	string(peer.CapArchival1), string(peer.CapOriginalTransfer1),
	string(peer.CapShareLifecycle1), string(peer.CapDerivedTransfer1),
}

// accessCopy is a local asset whose version 1 is sealed with one promoted,
// verified access copy.
type accessCopy struct {
	asset    uuid.UUID
	version  uuid.UUID
	original string // the version's original hash
	copy     string // the access copy's hash
}

// newAccessCopy builds an asset owned by owner, at tier, whose version 1
// is sealed with its original and one access copy, through the controlled
// seal (asset_version_seal), so every 00085 check runs on it.
func (fx *gateFixture) newAccessCopy(t *testing.T, owner int64, tier string) accessCopy {
	t.Helper()
	ctx := context.Background()
	a := accessCopy{asset: uuid.New(), original: origHash(t), copy: origHash(t)}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, $2, 'image/png', 'fs')`,
			[]any{a.original, origSize}},
		{`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'original', $2, 'image/png', $1, $2, 'write', 'verified')`,
			[]any{a.original, origSize}},
		// The promoted copy: an object of its own, verified.
		{`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, $2, 'image/webp', 'fs')`,
			[]any{a.copy, accessSize}},
		{`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'original', $2, 'image/webp', $1, $2, 'write', 'verified')`,
			[]any{a.copy, accessSize}},
		// The original's verified rendition whose digest the copy carries;
		// the 00085 trigger records its provenance.
		{`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'hires', $3, 'image/webp', $2, $3, 'write', 'verified')`,
			[]any{a.original, a.copy, accessSize}},
		{`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, 'version_promote', $2)`,
			[]any{a.copy, "access-test-" + a.asset.String()}},
	} {
		if _, err := fx.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("fixture %q: %v", q.sql[:40], err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, a.asset)
		_, _ = fx.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = ANY($1)`, []string{a.original, a.copy})
		_, _ = fx.pool.Exec(c, `DELETE FROM jobs WHERE type = 'asset.version_reconcile' AND payload->>'object_hash' = ANY($1)`,
			[]string{a.original, a.copy})
		_, _ = fx.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = ANY($1)`, []string{a.original, a.copy})
		_, _ = fx.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = ANY($1)`, []string{a.original, a.copy})
	})
	if err := fx.pool.QueryRow(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		VALUES ($1, 'Access Test', 1, $2, $3, $4, $5) RETURNING current_version_id`,
		a.asset, owner, a.original, origSize, tier).Scan(&a.version); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	entry := `[{"role":"access","object_hash":"` + a.copy + `","size_bytes":` + strconv.FormatInt(accessSize, 10) +
		`,"media_type":"image/webp","derived_from_hash":"` + a.original + `","derived_variant_key":"hires"}]`
	if err := pgx.BeginFunc(ctx, fx.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT asset_version_seal($1, $2::jsonb)`, a.version, entry)
		return err
	}); err != nil {
		t.Fatalf("seal with an access copy: %v", err)
	}
	return a
}

// announce records, as assetsend.recordClaims does before sending, that
// version n of a was announced to peerID with files (role, sha256).
func (fx *gateFixture) announce(t *testing.T, a accessCopy, peerID uuid.UUID, n int64, files ...[2]string) {
	t.Helper()
	ctx := context.Background()
	type claim struct {
		Role   string `json:"role"`
		SHA256 string `json:"sha256"`
	}
	list := make([]claim, 0, len(files))
	for _, f := range files {
		list = append(list, claim{Role: f[0], SHA256: f[1]})
	}
	raw, _ := json.Marshal(list)
	var activity uuid.UUID
	if err := fx.pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload, source, object_local_id)
		 VALUES ($1, $2, 'https://local.example/users/grantor', $3, '{}'::jsonb, 'local', $4) RETURNING id`,
		"https://local.example/activities/"+uuid.NewString(), string(federation.ActivityAAAssetVersion), fx.grantorRef, a.asset.String(),
	).Scan(&activity); err != nil {
		t.Fatalf("insert announcement activity: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM activities WHERE id = $1`, activity) // the claim cascades
	})
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO federation_asset_announcement_claims (activity_id, peer_id, asset_id, version_number, files)
		 VALUES ($1, $2, $3, $4, $5::jsonb)`, activity, peerID, a.asset, n, string(raw)); err != nil {
		t.Fatalf("insert announcement claim: %v", err)
	}
}

// announced is announce of version 1 with its original and its access copy,
// as the sender lists them.
func (fx *gateFixture) announced(t *testing.T, a accessCopy, peerID uuid.UUID) {
	t.Helper()
	fx.announce(t, a, peerID, 1, [2]string{"original", a.original}, [2]string{"access", a.copy})
}

// grantAccessCopies sets the access-copy grant on a share row directly:
// this slice adds no setter, so only a test can turn it on.
func (fx *gateFixture) grantAccessCopies(t *testing.T, s *shares.Share) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE federation_shares SET allow_access_copies = TRUE WHERE id = $1`, s.ID); err != nil {
		t.Fatalf("set allow_access_copies: %v", err)
	}
}

// accessShare inserts a share of the input and, when access, turns on its
// access-copy grant.
func (fx *gateFixture) accessShare(t *testing.T, in shares.InsertInput, access bool) *shares.Share {
	t.Helper()
	s := fx.insertShare(t, in)
	if access {
		fx.grantAccessCopies(t, s)
	}
	return s
}

// newAccessPeer adds another connected, enabled peer with all four
// profiles negotiated.
func (fx *gateFixture) newAccessPeer(t *testing.T) uuid.UUID {
	t.Helper()
	id := fx.newPeer(t)
	fx.negotiate(t, id, accessProfiles...)
	return id
}

func (fx *gateFixture) fetchAccessCopy(t *testing.T, a accessCopy, req shares.AccessCopyFetchRequest) shares.AccessCopyFetchDecision {
	t.Helper()
	if req.PeerID == (uuid.UUID{}) {
		req.PeerID = fx.peerID
	}
	if req.AssetID == (uuid.UUID{}) {
		req.AssetID = a.asset
	}
	if req.Version == 0 {
		req.Version = 1
	}
	if req.SHA256 == "" {
		req.SHA256 = a.copy
	}
	if req.Size == 0 {
		req.Size = accessSize
	}
	dec, err := fx.reg.CanPeerFetchAccessCopy(context.Background(), req)
	if err != nil {
		t.Fatalf("CanPeerFetchAccessCopy: %v", err)
	}
	return dec
}

func wantAccessDeny(t *testing.T, dec shares.AccessCopyFetchDecision, want shares.RejectReason) {
	t.Helper()
	if dec.Allowed {
		t.Fatalf("allowed (share %v), want denied with %s", dec.Share, want)
	}
	if dec.Reason != want {
		t.Fatalf("reason = %q, want %q", dec.Reason, want)
	}
}

func wantAccessAllow(t *testing.T, dec shares.AccessCopyFetchDecision, share *shares.Share) {
	t.Helper()
	if !dec.Allowed {
		t.Fatalf("denied with %q, want allowed", dec.Reason)
	}
	if dec.Share == nil || dec.Share.ID != share.ID {
		t.Fatalf("authorising share = %v, want %s", dec.Share, share.ID)
	}
	if !dec.Share.AllowAccessCopies {
		t.Fatal("the authorising share does not carry allow_access_copies")
	}
}

// newAccessFixture: a peer with all four profiles, a public asset owned by
// the grantor with a sealed access copy, announced to that peer.
func newAccessFixture(t *testing.T) (*gateFixture, accessCopy) {
	t.Helper()
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, accessProfiles...)
	a := fx.newAccessCopy(t, fx.grantorRef, "public")
	fx.announced(t, a, fx.peerID)
	return fx, a
}

func TestCanPeerFetchAccessCopy_DirectGrantAllowsPublicAndTeam(t *testing.T) {
	fx, a := newAccessFixture(t)
	s := fx.accessShare(t, assetShare(a.asset, federation.ShareScopeView, false), true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), s)
	// Hash comparison accepts upper-case hex, as for originals.
	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{SHA256: strings.ToUpper(a.copy)}), s)

	team := fx.newAccessCopy(t, fx.grantorRef, "team")
	fx.announced(t, team, fx.peerID)
	ts := fx.accessShare(t, assetShare(team.asset, federation.ShareScopeView, false), true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, team, shares.AccessCopyFetchRequest{}), ts)
}

func TestCanPeerFetchAccessCopy_ContainerGrant(t *testing.T) {
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, accessProfiles...)
	coll := fx.newCollection(t, fx.grantorRef)
	own := fx.newAccessCopy(t, fx.grantorRef, "public")
	stranger := fx.newUser(t)
	foreign := fx.newAccessCopy(t, stranger, "public")
	restricted := fx.newAccessCopy(t, fx.grantorRef, "restricted")
	for _, a := range []accessCopy{own, foreign, restricted} {
		fx.announced(t, a, fx.peerID)
		fx.addMember(t, coll, a.asset)
	}
	cs := fx.accessShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection, ObjectID: coll, Scope: federation.ShareScopeView,
	}, true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, own, shares.AccessCopyFetchRequest{}), cs)
	wantAccessDeny(t, fx.fetchAccessCopy(t, foreign, shares.AccessCopyFetchRequest{}), shares.RejectReasonGrantorNotOwner)
	wantAccessDeny(t, fx.fetchAccessCopy(t, restricted, shares.AccessCopyFetchRequest{}), shares.RejectReasonSensitivityRefused)

	// Removing the member ends the container route.
	if _, err := fx.pool.Exec(context.Background(),
		`DELETE FROM collection_resources WHERE collection_id = $1 AND asset_id = $2`, coll, own.asset); err != nil {
		t.Fatal(err)
	}
	wantAccessDeny(t, fx.fetchAccessCopy(t, own, shares.AccessCopyFetchRequest{}), shares.RejectReasonNoShareRow)
}

func TestCanPeerFetchAccessCopy_AdminGrantorReachesForeignMember(t *testing.T) {
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, accessProfiles...)
	admin := fx.newUser(t)
	fx.makeAdmin(t, admin)
	coll := fx.newCollection(t, admin)
	a := fx.newAccessCopy(t, fx.grantorRef, "public")
	fx.announced(t, a, fx.peerID)
	fx.addMember(t, coll, a.asset)
	cs := fx.accessShare(t, shares.InsertInput{
		GrantorUserRef: admin,
		ObjectKind:     federation.ShareObjectKindCollection, ObjectID: coll, Scope: federation.ShareScopeView,
	}, true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), cs)
}

// Every profile is required, and so is the peer's own state, all read from
// its row.
func TestCanPeerFetchAccessCopy_PeerAndEveryProfile(t *testing.T) {
	fx, a := newAccessFixture(t)
	s := fx.accessShare(t, assetShare(a.asset, federation.ShareScopeView, false), true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), s)

	for _, missing := range accessProfiles {
		t.Run("without "+missing, func(t *testing.T) {
			var rest []string
			for _, p := range accessProfiles {
				if p != missing {
					rest = append(rest, p)
				}
			}
			fx.negotiate(t, fx.peerID, rest...)
			wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonProfileNotNegotiated)
		})
	}
	fx.negotiate(t, fx.peerID)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonProfileNotNegotiated)
	fx.negotiate(t, fx.peerID, accessProfiles...)

	if _, err := fx.pool.Exec(context.Background(), `UPDATE federation_peers SET status = 'pending_outbound' WHERE id = $1`, fx.peerID); err != nil {
		t.Fatal(err)
	}
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonPeerNotConnected)
	if _, err := fx.pool.Exec(context.Background(), `UPDATE federation_peers SET status = 'connected', enabled = FALSE WHERE id = $1`, fx.peerID); err != nil {
		t.Fatal(err)
	}
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonPeerDisabled)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{PeerID: uuid.New()}), shares.RejectReasonUnknownPeer)
}

func TestCanPeerFetchAccessCopy_WrongPeerAndRecipient(t *testing.T) {
	fx, a := newAccessFixture(t)
	base := fx.peerURL(t, fx.peerID)
	alice := base + "/users/alice"
	in := assetShare(a.asset, federation.ShareScopeView, false)
	in.TargetUserURL = &alice
	s := fx.accessShare(t, in, true)

	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{ActorURL: alice}), s)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{ActorURL: base + "/users/bob"}), shares.RejectReasonWrongUser)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonWrongUser)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{ActorURL: "https://elsewhere.example/users/alice"}),
		shares.RejectReasonActorNotOnPeer)

	// Another peer, even with every profile and its own announcement, has
	// no share.
	other := fx.newAccessPeer(t)
	fx.announced(t, a, other)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{PeerID: other}), shares.RejectReasonNoShareRow)
}

// The original-file grant never authorises an access copy, at any scope.
func TestCanPeerFetchAccessCopy_OriginalGrantNeverPermitsAccess(t *testing.T) {
	for _, scope := range []federation.ShareScope{
		federation.ShareScopeView, federation.ShareScopeComment,
		federation.ShareScopeAnnotate, federation.ShareScopeRemix,
	} {
		t.Run(string(scope), func(t *testing.T) {
			fx, a := newAccessFixture(t)
			fx.insertShare(t, assetShare(a.asset, scope, true)) // allow_original only
			wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonAccessCopyNotGranted)
			// The same share still authorises the original.
			if dec := fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: a.asset, SHA256: a.original}); !dec.Allowed {
				t.Fatalf("the original-file grant no longer authorises the original: %s", dec.Reason)
			}
		})
	}
}

// The access-copy grant never authorises the original, and the original
// path never authorises an access copy's bytes.
func TestCanPeerFetchAccessCopy_AccessGrantNeverPermitsOriginal(t *testing.T) {
	fx, a := newAccessFixture(t)
	s := fx.accessShare(t, assetShare(a.asset, federation.ShareScopeRemix, false), true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), s)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: a.asset, SHA256: a.original}), shares.RejectReasonOriginalNotGranted)
	wantDeny(t, fx.fetchOriginal(t, shares.OriginalFetchRequest{AssetID: a.asset, SHA256: a.copy, Size: accessSize}), shares.RejectReasonHashMismatch)
}

// A higher-ranked share without the access flag must not mask a lower-ranked
// one that has it, and must not lend it to another actor.
func TestCanPeerFetchAccessCopy_UnflaggedShareDoesNotMaskFlagged(t *testing.T) {
	fx, a := newAccessFixture(t)
	base := fx.peerURL(t, fx.peerID)
	alice, bob := base+"/users/alice", base+"/users/bob"
	fx.insertShare(t, assetShare(a.asset, federation.ShareScopeRemix, true)) // anyone on the peer, original only
	flagged := assetShare(a.asset, federation.ShareScopeView, false)
	flagged.TargetUserURL = &alice
	fs := fx.accessShare(t, flagged, true)

	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{ActorURL: alice}), fs)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{ActorURL: bob}), shares.RejectReasonAccessCopyNotGranted)
}

func TestCanPeerFetchAccessCopy_ExpiredAndRevoked(t *testing.T) {
	fx, a := newAccessFixture(t)
	in := assetShare(a.asset, federation.ShareScopeView, false)
	past := pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	in.ExpiresAt = &past
	fx.accessShare(t, in, true)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonExpired)

	b := fx.newAccessCopy(t, fx.grantorRef, "public")
	fx.announced(t, b, fx.peerID)
	s := fx.accessShare(t, assetShare(b.asset, federation.ShareScopeView, false), true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, b, shares.AccessCopyFetchRequest{}), s)
	// A revoke written straight to the row is seen at once: nothing is
	// cached.
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = $2 WHERE id = $1`, s.ID, fx.activityID); err != nil {
		t.Fatalf("revoke row: %v", err)
	}
	wantAccessDeny(t, fx.fetchAccessCopy(t, b, shares.AccessCopyFetchRequest{}), shares.RejectReasonNoShareRow)
}

// The grantor-owner rule applies to direct shares too.
func TestCanPeerFetchAccessCopy_DirectGrantorNoLongerOwner(t *testing.T) {
	fx, a := newAccessFixture(t)
	fx.accessShare(t, assetShare(a.asset, federation.ShareScopeView, false), true)
	newOwner := fx.newUser(t)
	if _, err := fx.pool.Exec(context.Background(), `UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, a.asset, newOwner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `UPDATE assets SET owner_user_ref = $2 WHERE id = $1`, a.asset, fx.grantorRef)
	})
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonGrantorNotOwner)
}

func TestCanPeerFetchAccessCopy_LiveLocalAssetAndTier(t *testing.T) {
	fx, a := newAccessFixture(t)
	fx.accessShare(t, assetShare(a.asset, federation.ShareScopeView, false), true)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{AssetID: uuid.New()}), shares.RejectReasonUnknownObject)

	for _, tier := range []string{"restricted", "embargo"} {
		b := fx.newAccessCopy(t, fx.grantorRef, tier)
		fx.announced(t, b, fx.peerID)
		fx.accessShare(t, assetShare(b.asset, federation.ShareScopeView, false), true)
		wantAccessDeny(t, fx.fetchAccessCopy(t, b, shares.AccessCopyFetchRequest{}), shares.RejectReasonSensitivityRefused)
	}

	// A copy received from another instance is never served onward. (The
	// database refuses to turn a versioned local asset into a received one,
	// so this is a received asset from the start.)
	received := fx.newOriginalAssetFrom(t, fx.grantorRef, "public", origHash(t), &fx.peerID)
	fx.accessShare(t, assetShare(received, federation.ShareScopeView, false), true)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{AssetID: received}), shares.RejectReasonNotOrigin)

	if _, err := fx.pool.Exec(context.Background(), `UPDATE assets SET deleted_at = now() WHERE id = $1`, a.asset); err != nil {
		t.Fatal(err)
	}
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonUnknownObject)
}

// The request binds one exact file of one exact sealed version of this
// asset. Identical bytes elsewhere, the original's hash, another size or
// another version authorise nothing.
func TestCanPeerFetchAccessCopy_ExactVersionHashAndSize(t *testing.T) {
	fx, a := newAccessFixture(t)
	s := fx.accessShare(t, assetShare(a.asset, federation.ShareScopeView, false), true)
	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), s)

	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{Version: 2}), shares.RejectReasonVersionNotFound)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{Version: -1}), shares.RejectReasonVersionNotFound)
	// The version's original is not an access copy.
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{SHA256: a.original, Size: origSize}), shares.RejectReasonCopyNotListed)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{Size: accessSize + 1}), shares.RejectReasonCopyNotListed)

	// Another asset's sealed, announced copy named against this asset.
	other := fx.newAccessCopy(t, fx.grantorRef, "public")
	fx.announced(t, other, fx.peerID)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{SHA256: other.copy}), shares.RejectReasonCopyNotListed)

	// An unsealed version holds nothing to serve.
	draft := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	fx.accessShare(t, assetShare(draft, federation.ShareScopeView, false), true)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{AssetID: draft}), shares.RejectReasonVersionNotFound)
}

// The copy's own object must be verified NOW: a recorded mismatch, missing
// bytes, an unverifiable state, or a verified digest of other bytes refuses
// it, though the sealed version still lists it.
func TestCanPeerFetchAccessCopy_CopyMustBeVerifiedNow(t *testing.T) {
	for _, c := range []struct {
		name, sql string
	}{
		{"mismatch", `UPDATE storage_variants SET fixity_status = 'mismatch' WHERE object_hash = $1 AND variant_key = 'original'`},
		{"missing", `UPDATE storage_variants SET fixity_status = 'missing' WHERE object_hash = $1 AND variant_key = 'original'`},
		{"unverifiable", `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL,
			content_size_bytes = NULL, digest_source = NULL WHERE object_hash = $1 AND variant_key = 'original'`},
		{"verified at another digest", `UPDATE storage_variants SET content_sha256 = repeat('e', 64)
			WHERE object_hash = $1 AND variant_key = 'original'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx, a := newAccessFixture(t)
			s := fx.accessShare(t, assetShare(a.asset, federation.ShareScopeView, false), true)
			wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), s)
			if _, err := fx.pool.Exec(context.Background(), c.sql, a.copy); err != nil {
				t.Fatalf("record %s: %v", c.name, err)
			}
			wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonCopyNotVerified)
		})
	}
}

// The origin serves only a file it recorded announcing, for that version,
// to that peer, as wire role access. A claim is never a grant: with the
// claim and no grant the answer is still a refusal.
func TestCanPeerFetchAccessCopy_PerPeerAnnouncement(t *testing.T) {
	fx := newGateFixture(t)
	fx.negotiate(t, fx.peerID, accessProfiles...)
	a := fx.newAccessCopy(t, fx.grantorRef, "public")
	s := fx.accessShare(t, assetShare(a.asset, federation.ShareScopeView, false), true)
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonCopyNotAnnounced)

	// Claims that do not cover the file for this peer and version.
	other := fx.newAccessPeer(t)
	fx.announced(t, a, other)                                          // another peer
	fx.announce(t, a, fx.peerID, 2, [2]string{"access", a.copy})       // another version
	fx.announce(t, a, fx.peerID, 1, [2]string{"original", a.copy})     // another role
	fx.announce(t, a, fx.peerID, 1, [2]string{"access", origHash(t)})  // another file
	fx.announce(t, a, fx.peerID, 1, [2]string{"original", a.original}) // the original only
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonCopyNotAnnounced)

	fx.announced(t, a, fx.peerID)
	wantAccessAllow(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), s)

	// The claim alone authorises nothing.
	if _, err := fx.pool.Exec(context.Background(),
		`UPDATE federation_shares SET allow_access_copies = FALSE WHERE id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	wantAccessDeny(t, fx.fetchAccessCopy(t, a, shares.AccessCopyFetchRequest{}), shares.RejectReasonAccessCopyNotGranted)
}

// The Share mapper carries the new flag from every full-row read, and a new
// share starts with it off.
func TestShare_AllowAccessCopiesDefaultsOffAndMaps(t *testing.T) {
	fx, a := newAccessFixture(t)
	s := fx.insertShare(t, assetShare(a.asset, federation.ShareScopeView, true))
	if s.AllowAccessCopies {
		t.Fatal("a new share reads allow_access_copies = true")
	}
	fx.grantAccessCopies(t, s)
	got, err := fx.reg.ByID(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowAccessCopies || !got.AllowOriginal {
		t.Fatalf("read back allow_access_copies=%v allow_original=%v, want true / true", got.AllowAccessCopies, got.AllowOriginal)
	}
}
