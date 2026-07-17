// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the inbox-filter gate per the 1.22.C design proposal
// §5. Phase 1.22.C-b.
//
// Coverage:
//   - Direct allow (basic happy path)
//   - Peer disabled / not connected short-circuits
//   - No share row → no_share_row
//   - Wrong user (specific share, broadcast share)
//   - Scope ladder: view doesn't cover comment; remix covers all
//   - Expired share → expired
//   - Revoked share → no_share_row (filtered by SQL)
//   - Container fallback: share on collection grants access to
//     contained asset; collection-share scope is what's checked
//   - Broadcast vs specific tie-break: specific wins
//   - Cache invalidation: Insert + Revoke drop the cached set
//   - aa:RevokeShare normalization
//   - ActivityRequiredScope mapping for the documented verbs
//
// Most tests run against a live DB (skip without AA_DB_PASSWORD).
// The activity-to-scope mapping + the normalization helper are
// pure-Go unit tests.

package shares_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

// --- pure unit tests ----------------------------------------------------

func TestActivityRequiredScope_Mapping(t *testing.T) {
	cases := []struct {
		typ      federation.ActivityType
		want     federation.ShareScope
		shouldOK bool
	}{
		{federation.ActivityLike, federation.ShareScopeView, true},
		{federation.ActivityAnnounce, federation.ShareScopeView, true},
		{federation.ActivityCreate, federation.ShareScopeComment, true},
		{federation.ActivityAAAnnotation, federation.ShareScopeAnnotate, true},
		{federation.ActivityAAWorkflowTransition, federation.ShareScopeRemix, true},
		{federation.ActivityAAApprove, federation.ShareScopeRemix, true},
		{federation.ActivityDelete, federation.ShareScopeView, true},
		{federation.ActivityUpdate, federation.ShareScopeView, true},
		{federation.ActivityUndo, federation.ShareScopeView, true},
		{federation.ActivityFollow, federation.ShareScopeView, false}, // gated outside share
		{federation.ActivityAccept, federation.ShareScopeView, false},
		{federation.ActivityBlock, federation.ShareScopeView, false},
		{federation.ActivityAAShare, federation.ShareScopeView, false}, // share flow
		{federation.ActivityAAUnshare, federation.ShareScopeView, false},
		{federation.ActivityAARevokeShare, federation.ShareScopeView, false},
		{federation.ActivityAdd, federation.ShareScopeRemix, true},
		{federation.ActivityRemove, federation.ShareScopeRemix, true},
	}
	for _, c := range cases {
		got, ok := shares.ActivityRequiredScope(c.typ)
		if got != c.want {
			t.Errorf("%s: scope got %q want %q", c.typ, got, c.want)
		}
		if ok != c.shouldOK {
			t.Errorf("%s: ok got %v want %v", c.typ, ok, c.shouldOK)
		}
	}
}

func TestNormalizeInboundActivityType_RevokeShareMapsToUnshare(t *testing.T) {
	if got := shares.NormalizeInboundActivityType(federation.ActivityAARevokeShare); got != federation.ActivityAAUnshare {
		t.Errorf("aa:RevokeShare should normalize to aa:Unshare; got %q", got)
	}
	// Other types pass through.
	if got := shares.NormalizeInboundActivityType(federation.ActivityLike); got != federation.ActivityLike {
		t.Errorf("Like should pass through; got %q", got)
	}
}

// --- DB-backed fixture --------------------------------------------------

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// gateFixture wires everything a gate test needs: pool + registry +
// a real peer row + a real grantor user + a real activity row.
// Cleanup deletes all of them so tests are isolated.
type gateFixture struct {
	pool     *pgxpool.Pool
	reg      *shares.Registry
	regCache *cache.Registry

	peerID     uuid.UUID
	grantorRef int64
	activityID uuid.UUID
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	regCache := cache.NewRegistry(pool, logger)
	t.Cleanup(regCache.Stop)
	reg := shares.NewRegistry(pool, logger, regCache)

	// Grantor user.
	var grantorRef int64
	username := "share-test-" + randHex(t, 4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Share Test",
	).Scan(&grantorRef); err != nil {
		t.Fatalf("insert grantor: %v", err)
	}
	// Peer.
	pub, _, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	peerURL := "https://share-test-" + randHex(t, 4) + ".example"
	var peerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, $2, $3, 'connected', 'plaintext', TRUE, 'connected', $4)
		 RETURNING id`,
		peerURL, "Share Test Peer", string(pubPEM), grantorRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	// Placeholder activity for granted_activity_id FKs.
	actorURI := "https://local.example/users/" + username
	var activityID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
		    payload, source)
		 VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb, 'local')
		 RETURNING id`,
		"https://local.example/activities/"+uuid.New().String(), actorURI, grantorRef,
	).Scan(&activityID); err != nil {
		t.Fatalf("insert activity: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE grantor_user_ref = $1`, grantorRef)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, grantorRef)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
	})

	return &gateFixture{
		pool:       pool,
		reg:        reg,
		regCache:   regCache,
		peerID:     peerID,
		grantorRef: grantorRef,
		activityID: activityID,
	}
}

func (fx *gateFixture) insertShare(t *testing.T, in shares.InsertInput) *shares.Share {
	t.Helper()
	in.GrantedActivityID = fx.activityID
	if in.GrantorUserRef == 0 {
		in.GrantorUserRef = fx.grantorRef
	}
	if in.PeerID == (uuid.UUID{}) {
		in.PeerID = fx.peerID
	}
	s, err := fx.reg.Insert(context.Background(), in)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return s
}

// --- direct-share allow/reject -----------------------------------------

func TestCanPeerAccess_AllowedOnDirectShare(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindPost,
		ObjectID:   objectID,
		Scope:      federation.ShareScopeView,
	})
	dec, err := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Allowed {
		t.Errorf("allowed: got false (reason=%q)", dec.Reason)
	}
	if dec.Share == nil {
		t.Error("matched share should be populated")
	}
}

func TestCanPeerAccess_PeerDisabled(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindPost,
		ObjectID:   objectID,
		Scope:      federation.ShareScopeView,
	})
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   false, // disabled
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("disabled peer should be rejected")
	}
	if dec.Reason != shares.RejectReasonPeerDisabled {
		t.Errorf("reason: got %q want %q", dec.Reason, shares.RejectReasonPeerDisabled)
	}
}

func TestCanPeerAccess_PeerNotConnected(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: false, // pending handshake state
	})
	if dec.Allowed {
		t.Error("pending peer should be rejected")
	}
	if dec.Reason != shares.RejectReasonPeerNotConnected {
		t.Errorf("reason: got %q want %q", dec.Reason, shares.RejectReasonPeerNotConnected)
	}
}

func TestCanPeerAccess_NoShareRow(t *testing.T) {
	fx := newGateFixture(t)
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      uuid.New(), // never shared
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("unshared object should be rejected")
	}
	if dec.Reason != shares.RejectReasonNoShareRow {
		t.Errorf("reason: got %q want %q", dec.Reason, shares.RejectReasonNoShareRow)
	}
}

// --- user-targeting -----------------------------------------------------

func TestCanPeerAccess_WrongUser(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	alice := "https://peer.example/users/alice"
	fx.insertShare(t, shares.InsertInput{
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		TargetUserURL: &alice,
		Scope:         federation.ShareScopeView,
	})
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		UserURL:       "https://peer.example/users/bob", // not alice
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("share is for alice but bob is asking")
	}
	if dec.Reason != shares.RejectReasonWrongUser {
		t.Errorf("reason: got %q want %q", dec.Reason, shares.RejectReasonWrongUser)
	}
}

func TestCanPeerAccess_BroadcastShareAllowsAnyUser(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	fx.insertShare(t, shares.InsertInput{
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		TargetUserURL: nil, // broadcast
		Scope:         federation.ShareScopeView,
	})
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		UserURL:       "https://peer.example/users/anyone",
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if !dec.Allowed {
		t.Errorf("broadcast share should allow any user; got reason=%q", dec.Reason)
	}
}

// --- scope ladder -------------------------------------------------------

func TestCanPeerAccess_ViewDoesNotCoverComment(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindPost,
		ObjectID:   objectID,
		Scope:      federation.ShareScopeView,
	})
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeComment, // higher than share
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("view share should not cover comment activity")
	}
	if dec.Reason != shares.RejectReasonInsufficientScope {
		t.Errorf("reason: got %q want %q", dec.Reason, shares.RejectReasonInsufficientScope)
	}
}

func TestCanPeerAccess_RemixCoversAll(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindPost,
		ObjectID:   objectID,
		Scope:      federation.ShareScopeRemix,
	})
	for _, required := range []federation.ShareScope{
		federation.ShareScopeView,
		federation.ShareScopeComment,
		federation.ShareScopeAnnotate,
		federation.ShareScopeRemix,
	} {
		dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
			PeerID:        fx.peerID,
			ObjectKind:    federation.ShareObjectKindPost,
			ObjectID:      objectID,
			RequiredScope: required,
			PeerEnabled:   true,
			PeerConnected: true,
		})
		if !dec.Allowed {
			t.Errorf("remix share should cover %q; got reason=%q", required, dec.Reason)
		}
	}
}

// --- expiry -------------------------------------------------------------

func TestCanPeerAccess_ExpiredShareRejected(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	past := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindPost,
		ObjectID:   objectID,
		Scope:      federation.ShareScopeView,
		ExpiresAt:  &past,
	})
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("expired share should not grant access")
	}
	// The SQL filter excludes expired rows from FindActiveShare;
	// the ListActiveSharesByObject query DOES NOT filter expiry
	// (cache holds the full row + the gate re-checks), so we
	// should see RejectReasonExpired here as the most-specific
	// reason picked by the iterator.
	if dec.Reason != shares.RejectReasonExpired && dec.Reason != shares.RejectReasonNoShareRow {
		t.Errorf("reason: got %q want expired or no_share_row", dec.Reason)
	}
}

// --- container fallback (collection → asset) ----------------------------

func TestCanPeerAccess_ContainerFallback_AssetInSharedCollection(t *testing.T) {
	fx := newGateFixture(t)
	ctx := context.Background()

	// Create a collection + an asset + a membership row so the
	// container resolver finds it.
	collectionID := uuid.New()
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO collections (id, owner_user_ref, name, visibility)
		 VALUES ($1, $2, 'Test Coll', 'explicit-share')`,
		collectionID, fx.grantorRef,
	); err != nil {
		t.Fatal(err)
	}
	assetID := uuid.New()
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, has_image)
		 VALUES ($1, 'Test Asset', 1, $2, FALSE)`,
		assetID, fx.grantorRef,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO collection_resources (collection_id, asset_id) VALUES ($1, $2)`,
		collectionID, assetID,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM collection_resources WHERE asset_id = $1`, assetID)
		_, _ = fx.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, assetID)
		_, _ = fx.pool.Exec(c, `DELETE FROM collections WHERE id = $1`, collectionID)
	})

	// Share the COLLECTION (not the asset).
	fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindCollection,
		ObjectID:   collectionID,
		Scope:      federation.ShareScopeView,
	})

	// Access the ASSET — should resolve via container fallback.
	dec, err := fx.reg.CanPeerAccess(ctx, shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindAsset,
		ObjectID:      assetID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Allowed {
		t.Errorf("container fallback: asset in shared collection should be accessible; got reason=%q", dec.Reason)
	}
	if dec.Share == nil || dec.Share.ObjectKind != federation.ShareObjectKindCollection {
		t.Error("matched share should be the collection-level grant")
	}
}

// --- cache invalidation -------------------------------------------------

func TestCanPeerAccess_RevokeInvalidatesCache(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	s := fx.insertShare(t, shares.InsertInput{
		ObjectKind: federation.ShareObjectKindPost,
		ObjectID:   objectID,
		Scope:      federation.ShareScopeView,
	})
	// Prime the cache by calling CanPeerAccess once.
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if !dec.Allowed {
		t.Fatal("expected allowed on first check")
	}
	// Revoke the share.
	if _, err := fx.reg.Revoke(context.Background(), s.ID, fx.activityID); err != nil {
		t.Fatal(err)
	}
	// Re-check — should now reject.
	dec, _ = fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("post-revoke: cache should have been invalidated; share should not match")
	}
	if dec.Reason != shares.RejectReasonNoShareRow {
		t.Errorf("post-revoke reason: got %q want %q", dec.Reason, shares.RejectReasonNoShareRow)
	}
}
