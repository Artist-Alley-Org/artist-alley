// End-to-end tests for the five worked examples from the
// 1.22.C design proposal §9. Phase 1.22.C-f.
//
// Each test exercises a full user-visible flow through the real
// admin handler + real Registry + real DB, asserting the
// write-ahead-audit invariant + the cache invariant + the
// container-resolution rule + the recipient-derivative-
// preservation rule.
//
// Tests are self-contained — each builds its own fixture (peer,
// grantor, activity placeholder) and cleans up on completion so
// they're safe to run in parallel against the shared dev DB.
//
// Skips without AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// --- Example #1: share a collection ----------------------------------

func TestWorkedExample_ShareCollection_GrantsImplicitAssetAccess(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()

	// Insert a collection with three assets — simulating Alice's
	// "Concept Sketches" from the design's §9.1 example.
	collectionID := uuid.New()
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO collections (id, owner_user_ref, name, visibility)
		 VALUES ($1, $2, 'Concept Sketches', 'explicit-share')`,
		collectionID, fx.grantorRef,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, collectionID)
	})
	assetIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for _, aID := range assetIDs {
		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO assets (id, title, asset_type, owner_user_ref, has_image)
			 VALUES ($1, 'asset', 1, $2, FALSE)`, aID, fx.grantorRef); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.pool.Exec(ctx,
			`INSERT INTO collection_resources (collection_id, asset_id) VALUES ($1, $2)`,
			collectionID, aID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM collection_resources WHERE collection_id = $1`, collectionID)
		for _, aID := range assetIDs {
			_, _ = fx.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, aID)
		}
	})

	// Alice grants Bob view on the collection.
	bob := "https://studio-b.example/users/bob"
	resp, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind:    openapi.FederationShareCreateObjectKind(federation.ShareObjectKindCollection),
			ObjectId:      collectionID,
			PeerId:        fx.peerID,
			TargetUserUrl: &bob,
			Scope:         openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.(openapi.GrantFederationShare201JSONResponse); !ok {
		t.Fatalf("grant: expected 201, got %T", resp)
	}

	// Per §3.1: each of the 3 assets MUST resolve to allowed via
	// container fallback (no explicit per-asset share rows).
	for _, aID := range assetIDs {
		dec, err := fx.reg.CanPeerAccess(ctx, shares.AccessRequest{
			PeerID:        fx.peerID,
			UserURL:       bob,
			ObjectKind:    federation.ShareObjectKindAsset,
			ObjectID:      aID,
			RequiredScope: federation.ShareScopeView,
			PeerEnabled:   true,
			PeerConnected: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !dec.Allowed {
			t.Errorf("§9.1: asset %s in shared collection should be accessible (reason=%q)", aID, dec.Reason)
		}
		if dec.Share == nil || dec.Share.ObjectKind != federation.ShareObjectKindCollection {
			t.Errorf("§9.1: matched share should be the collection-level grant, not a per-asset row")
		}
	}

	// And the audit row + aa:Share activity are both present —
	// the write-ahead invariant.
	assertActivityType(t, fx.pool, fx.grantorRef, "aa:Share", 1)
	assertAuditCount(t, fx.pool, fx.grantorRef, audit.EventFederationShareGranted, 1)
}

// --- Example #2: unshare a collection --------------------------------

func TestWorkedExample_UnshareCollection_RevokesAccess_PreservesDerivatives(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()

	// Set up: collection + 1 asset + share to Bob.
	collectionID := uuid.New()
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO collections (id, owner_user_ref, name, visibility)
		 VALUES ($1, $2, 'X', 'explicit-share')`, collectionID, fx.grantorRef); err != nil {
		t.Fatal(err)
	}
	assetID := uuid.New()
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, has_image)
		 VALUES ($1, 'asset', 1, $2, FALSE)`, assetID, fx.grantorRef); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO collection_resources (collection_id, asset_id) VALUES ($1, $2)`,
		collectionID, assetID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM collection_resources WHERE collection_id = $1`, collectionID)
		_, _ = fx.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, assetID)
		_, _ = fx.pool.Exec(c, `DELETE FROM collections WHERE id = $1`, collectionID)
	})

	bob := "https://studio-b.example/users/bob"
	grantResp, _ := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind:    openapi.FederationShareCreateObjectKind(federation.ShareObjectKindCollection),
			ObjectId:      collectionID,
			PeerId:        fx.peerID,
			TargetUserUrl: &bob,
			Scope:         openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})
	created := grantResp.(openapi.GrantFederationShare201JSONResponse)

	// Simulate Bob's local comment on the asset by inserting a
	// recipient-side activity row (Bob's instance would write
	// this). This represents the "annotations / comments on the
	// shared design doc" the design §4.3 says we MUST preserve.
	bobActorURI := bob
	bobCommentURI := "https://studio-b.example/comments/" + uuid.New().String()
	if _, err := fx.pool.Exec(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, payload, source)
		 VALUES ($1, 'Create', $2, '{}'::jsonb, 'https://studio-b.example')`,
		bobCommentURI, bobActorURI); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM activities WHERE activity_uri = $1`, bobCommentURI)
	})

	// Alice revokes.
	revResp, err := fx.handler.RevokeFederationShare(fx.ctx(), openapi.RevokeFederationShareRequestObject{
		Id: created.Id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := revResp.(openapi.RevokeFederationShare204Response); !ok {
		t.Fatalf("revoke: expected 204, got %T", revResp)
	}

	// Access to the asset must now be denied (cache invalidated).
	dec, _ := fx.reg.CanPeerAccess(ctx, shares.AccessRequest{
		PeerID:        fx.peerID,
		UserURL:       bob,
		ObjectKind:    federation.ShareObjectKindAsset,
		ObjectID:      assetID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("§9.2: post-revoke, asset should be inaccessible via collection share")
	}

	// Bob's comment activity row MUST still exist — that's the
	// "recipient derivative work preserved" rule from §4.3.
	var bobCommentCount int
	_ = fx.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activities WHERE activity_uri = $1`, bobCommentURI,
	).Scan(&bobCommentCount)
	if bobCommentCount != 1 {
		t.Error("§4.3: recipient's comment activity must be preserved across unshare")
	}

	// aa:Unshare + share.revoked audit both committed.
	assertActivityType(t, fx.pool, fx.grantorRef, "aa:Unshare", 1)
	assertAuditCount(t, fx.pool, fx.grantorRef, audit.EventFederationShareRevoked, 1)
}

// --- Example #3: follow a user ---------------------------------------

func TestWorkedExample_FollowUser_CreatesUserKindShareRow(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()

	// Per §9.3: when Alice accepts Bob's Follow, the handler
	// creates a federation_shares row with object_kind=user.
	// We simulate the "Alice clicked Accept" step by directly
	// granting a user-kind share via the admin handler — that's
	// the same code path the future Accept(Follow) inbox handler
	// will call.
	//
	// (The handler API currently rejects object_kind=user from
	// the public POST so the operator-facing UI can't grant
	// follower-shares directly. The Registry method DOES support
	// it; this test goes through Registry.Insert + simulates
	// the activity row separately to match what Accept(Follow)
	// will do.)

	bobActor := "https://studio-b.example/users/bob"
	aliceUUID := uuid.New() // stand-in for Alice's user UUID per the spec
	// Manually emit the aa:Share activity (Accept(Follow) flow
	// would do this via WithEmission).
	activityID := emitPlaceholderActivity(t, fx.pool, fx.grantorRef, "aa:Share")
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM activities WHERE id = $1`, activityID)
	})

	share, err := fx.reg.Insert(ctx, shares.InsertInput{
		GrantorUserRef:    fx.grantorRef,
		ObjectKind:        federation.ShareObjectKindUser,
		ObjectID:          aliceUUID,
		PeerID:            fx.peerID,
		TargetUserURL:     &bobActor,
		Scope:             federation.ShareScopeView,
		GrantedActivityID: activityID,
		Notes:             "Accept(Follow) → follower share",
	})
	if err != nil {
		t.Fatalf("insert follower share: %v", err)
	}

	// Per §9.3: a followers-tier post by Alice now resolves via
	// this row at outbox time. Verify the dispatcher's lookup
	// finds it via ListByObject(user, aliceUUID).
	rows, err := fx.reg.ListByObject(ctx, federation.ShareObjectKindUser, aliceUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != share.ID {
		t.Errorf("§9.3: outbox dispatcher lookup should return the Bob follower-share row")
	}
	if rows[0].ObjectKind != federation.ShareObjectKindUser {
		t.Error("§9.3: follower-share row must have object_kind=user (no separate followers table)")
	}

	// Unfollow → revoke the same row (per the design §4.1).
	revoked, err := fx.reg.Revoke(ctx, share.ID, activityID)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.RevokedAt.Valid {
		t.Error("§9.3: Undo(Follow) → revoked_at set on the user-share row")
	}
	// Post-revoke lookup returns zero — Bob no longer counts as
	// a follower.
	rows, _ = fx.reg.ListByObject(ctx, federation.ShareObjectKindUser, aliceUUID)
	if len(rows) != 0 {
		t.Errorf("§9.3: post-unfollow, follower-share lookup should be empty; got %d rows", len(rows))
	}
}

// --- Example #4: embargo expiry --------------------------------------

func TestWorkedExample_EmbargoExpiry_SweeperEmitsUnshare(t *testing.T) {
	fx := newAdminFixture(t)
	ctx := context.Background()

	// Build a sweeper around the admin fixture's pool. Same shape
	// the boot wires.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := activities.NewWriter(fx.pool, logger, nil)
	auditRec := audit.NewRecorder(fx.pool, logger)
	peerLookup := func(ctx context.Context, id uuid.UUID) (shares.PeerInfo, error) {
		return shares.PeerInfo{
			ID:          fx.peerID,
			InstanceURL: "https://studio-b.example",
			Enabled:     true,
			Connected:   true,
		}, nil
	}
	instanceURLFn := func(ctx context.Context) string { return "https://studio-a.example" }
	usernameFn := func(ctx context.Context, ref int64) string { return "alice" }
	sw := shares.NewSweeper(shares.SweeperConfig{Interval: time.Hour, BatchSize: 100},
		fx.reg, writer, auditRec, peerLookup, instanceURLFn, usernameFn, logger)

	// Alice shares an asset with Bob, embargo expires in the past
	// (simulating a share that ticked over expiry between checks).
	bob := "https://studio-b.example/users/bob"
	past := pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true}
	share, err := fx.reg.Insert(ctx, shares.InsertInput{
		GrantorUserRef:    fx.grantorRef,
		ObjectKind:        federation.ShareObjectKindAsset,
		ObjectID:          uuid.New(),
		PeerID:            fx.peerID,
		TargetUserURL:     &bob,
		Scope:             federation.ShareScopeView,
		ExpiresAt:         &past,
		GrantedActivityID: fx.activityID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pre-sweep: inbox gate already rejects with `expired` per
	// the design §5 (the load-bearing layer).
	dec, _ := fx.reg.CanPeerAccess(ctx, shares.AccessRequest{
		PeerID:        fx.peerID,
		UserURL:       bob,
		ObjectKind:    federation.ShareObjectKindAsset,
		ObjectID:      share.ObjectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("§9.4: pre-sweep gate must already reject expired share at the inbox")
	}

	// Run the sweeper — this is the §9.4 step that proactively
	// emits aa:Unshare so the recipient purges cached bytes.
	revoked, errs := sw.SweepOnce(ctx)
	if len(errs) > 0 {
		t.Fatalf("sweep errors: %v", errs)
	}
	if revoked < 1 {
		t.Error("§9.4: sweeper should have revoked at least the expired row")
	}

	// Verify the aa:Unshare landed + the audit row says reason=expired.
	var unshareCount int
	_ = fx.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activities WHERE activity_type='aa:Unshare' AND actor_user_ref = $1`,
		fx.grantorRef,
	).Scan(&unshareCount)
	if unshareCount == 0 {
		t.Error("§9.4: sweeper must emit aa:Unshare to peer's inbox")
	}
	var auditCount int
	_ = fx.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events
		 WHERE event_type=$1 AND metadata->>'reason'='expired' AND actor_user_ref=$2`,
		audit.EventFederationShareRevoked, fx.grantorRef,
	).Scan(&auditCount)
	if auditCount == 0 {
		t.Error("§9.4: expiry audit row must carry reason=expired")
	}
}

// --- Example #5: peer defederation cascade ---------------------------

func TestWorkedExample_DefederationCascade_PreviewSummary(t *testing.T) {
	fx := newAdminFixture(t)
	fx.handler.SetDefederationDeps(
		func(ctx context.Context, peerID uuid.UUID) (int, error) { return 0, nil },
		func(ctx context.Context, peerID uuid.UUID) (int, error) { return 0, nil },
		func(ctx context.Context, peerID uuid.UUID) (string, string, error) {
			return "Studio B", "https://studio-b.example", nil
		},
	)

	// Alice has 2 posts + 1 collection + 1 asset shared with Bob's
	// peer — sums to the "47 shares" example from §9.5 scaled down.
	bob := "https://studio-b.example/users/bob"
	for i := 0; i < 2; i++ {
		_, _ = fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
			Body: &openapi.FederationShareCreate{
				ObjectKind:    openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
				ObjectId:      uuid.New(),
				PeerId:        fx.peerID,
				TargetUserUrl: &bob,
				Scope:         openapi.FederationShareCreateScope(federation.ShareScopeView),
			},
		})
	}
	_, _ = fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind:    openapi.FederationShareCreateObjectKind(federation.ShareObjectKindCollection),
			ObjectId:      uuid.New(),
			PeerId:        fx.peerID,
			TargetUserUrl: &bob,
			Scope:         openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})
	_, _ = fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind:    openapi.FederationShareCreateObjectKind(federation.ShareObjectKindAsset),
			ObjectId:      uuid.New(),
			PeerId:        fx.peerID,
			TargetUserUrl: &bob,
			Scope:         openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})

	// Preview — per §9.5 step 2, this is what the admin sees
	// BEFORE clicking through.
	resp, err := fx.handler.PreviewFederationPeerDefederation(fx.ctx(),
		openapi.PreviewFederationPeerDefederationRequestObject{Id: fx.peerID})
	if err != nil {
		t.Fatal(err)
	}
	preview, ok := resp.(openapi.PreviewFederationPeerDefederation200JSONResponse)
	if !ok {
		t.Fatalf("preview: expected 200, got %T", resp)
	}
	if preview.TotalActiveShares != 4 {
		t.Errorf("§9.5: total_active_shares: got %d want 4", preview.TotalActiveShares)
	}
	if preview.SharesByKind["post"] != 2 {
		t.Errorf("§9.5: posts: got %d want 2", preview.SharesByKind["post"])
	}
	if preview.SharesByKind["collection"] != 1 {
		t.Errorf("§9.5: collections: got %d want 1", preview.SharesByKind["collection"])
	}
	if preview.SharesByKind["asset"] != 1 {
		t.Errorf("§9.5: assets: got %d want 1", preview.SharesByKind["asset"])
	}
	if preview.PeerDisplayName != "Studio B" {
		t.Errorf("§9.5: peer display: got %q", preview.PeerDisplayName)
	}
}

// --- helpers ---------------------------------------------------------

func emitPlaceholderActivity(t *testing.T, pool *pgxpool.Pool, actorRef int64, typ string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload, source)
		 VALUES ($1, $2, $3, $4, '{}'::jsonb, 'local') RETURNING id`,
		"https://local.example/activities/"+uuid.New().String(),
		typ,
		"https://local.example/users/u",
		actorRef,
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertActivityType(t *testing.T, pool *pgxpool.Pool, actorRef int64, typ string, atLeast int) {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM activities WHERE actor_user_ref=$1 AND activity_type=$2`,
		actorRef, typ,
	).Scan(&n)
	if n < atLeast {
		t.Errorf("activities[%s]: got %d want >= %d", typ, n, atLeast)
	}
}

func assertAuditCount(t *testing.T, pool *pgxpool.Pool, actorRef int64, eventType string, atLeast int) {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events WHERE actor_user_ref=$1 AND event_type=$2`,
		actorRef, eventType,
	).Scan(&n)
	if n < atLeast {
		t.Errorf("audit[%s]: got %d want >= %d", eventType, n, atLeast)
	}
}
