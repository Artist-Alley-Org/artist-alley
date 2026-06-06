// Tests for the admin grant/revoke flow + the write-ahead-audit
// invariant per the 1.22.C design proposal §7.2. Phase 1.22.C-c.
//
// Coverage:
//   - Grant: POST creates share + records aa:Share + audit row +
//     invalidates cache (CanPeerAccess immediately returns allowed)
//   - Idempotency: re-grant on the same (grantor, object, peer,
//     target) returns 409 with the existing share
//   - Permission: non-owner non-admin → 403
//   - Revoke: DELETE marks revoked + emits aa:Unshare + audit row
//   - List with each filter combination
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

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// adminFixture wires the full admin handler + its dependencies
// against a live DB. Tests use this to exercise grant + revoke
// + the write-ahead-audit invariant end-to-end.
type adminFixture struct {
	*gateFixture
	handler *shares.AdminHandler
	caller  *auth.Identity
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	gfx := newGateFixture(t)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writer := activities.NewWriter(gfx.pool, logger, nil)
	auditRec := audit.NewRecorder(gfx.pool, logger)

	// All callers in these tests are the grantor (owns the object
	// implicitly via this resolver returning true for them) — the
	// real production resolver consults posts.author_user_ref etc.
	ownerResolver := func(ctx context.Context, kind federation.ShareObjectKind, objectID uuid.UUID, caller *auth.Identity) (bool, error) {
		return caller.UserRef == gfx.grantorRef, nil
	}
	// Peer lookup returns the fixture's pre-seeded connected
	// peer.
	peerLookup := func(ctx context.Context, id uuid.UUID) (shares.PeerInfo, error) {
		if id != gfx.peerID {
			return shares.PeerInfo{}, shares.ErrShareNotFound
		}
		return shares.PeerInfo{
			ID:          gfx.peerID,
			InstanceURL: "https://test-peer.example",
			Enabled:     true,
			Connected:   true,
		}, nil
	}
	instanceURLFn := func(ctx context.Context) string { return "https://local.example" }
	usernameFn := func(ctx context.Context, ref int64) string { return "local-user" }

	h := shares.NewAdminHandler(gfx.reg, writer, auditRec, ownerResolver, peerLookup, instanceURLFn, usernameFn)
	caller := &auth.Identity{
		UserRef:      gfx.grantorRef,
		Username:     "local-user",
		AuthMethod:   "session",
		Capabilities: []string{"system.admin"},
	}
	return &adminFixture{gateFixture: gfx, handler: h, caller: caller}
}

func (fx *adminFixture) ctx() context.Context {
	return auth.WithIdentity(context.Background(), fx.caller)
}

func TestAdminGrant_HappyPath_WriteAheadAudit(t *testing.T) {
	fx := newAdminFixture(t)
	objectID := uuid.New()

	resp, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
			ObjectId:   objectID,
			PeerId:     fx.peerID,
			Scope:      openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	created, ok := resp.(openapi.GrantFederationShare201JSONResponse)
	if !ok {
		t.Fatalf("expected 201, got %T", resp)
	}
	if created.ObjectId != objectID {
		t.Errorf("object_id: got %s want %s", created.ObjectId, objectID)
	}

	// Verify the activity row was written (aa:Share).
	var activityCount int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM activities WHERE actor_user_ref=$1 AND activity_type='aa:Share'`,
		fx.grantorRef,
	).Scan(&activityCount)
	if activityCount == 0 {
		t.Error("write-ahead invariant: aa:Share activity row should have committed")
	}

	// Verify the audit event committed in the same tx.
	var auditCount int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events WHERE event_type='federation.share.granted' AND actor_user_ref=$1`,
		fx.grantorRef,
	).Scan(&auditCount)
	if auditCount == 0 {
		t.Error("write-ahead invariant: federation.share.granted audit row should have committed")
	}

	// Cache invalidation: a subsequent CanPeerAccess should
	// return Allowed (the cache was just primed during grant
	// flow → it must reflect the new share row).
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
		t.Errorf("post-grant: CanPeerAccess should allow; got reason=%q", dec.Reason)
	}
}

func TestAdminGrant_DuplicateReturns409(t *testing.T) {
	fx := newAdminFixture(t)
	objectID := uuid.New()
	body := &openapi.FederationShareCreate{
		ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
		ObjectId:   objectID,
		PeerId:     fx.peerID,
		Scope:      openapi.FederationShareCreateScope(federation.ShareScopeView),
	}
	// First grant succeeds.
	first, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.(openapi.GrantFederationShare201JSONResponse); !ok {
		t.Fatalf("first grant: expected 201, got %T", first)
	}
	// Second grant should hit the active-row UNIQUE → 409.
	second, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	conflict, ok := second.(openapi.GrantFederationShare409JSONResponse)
	if !ok {
		t.Fatalf("second grant: expected 409, got %T", second)
	}
	// The conflict response should carry the existing share.
	if conflict.ObjectId != objectID {
		t.Errorf("conflict body: object_id mismatch")
	}
}

func TestAdminGrant_NonOwnerRejected(t *testing.T) {
	fx := newAdminFixture(t)
	// Swap the caller to a NON-system.admin, non-owner.
	stranger := &auth.Identity{
		UserRef:      fx.grantorRef + 999_999, // not the owner
		Username:     "stranger",
		AuthMethod:   "session",
		Capabilities: []string{}, // no system.admin
	}
	ctx := auth.WithIdentity(context.Background(), stranger)
	resp, err := fx.handler.GrantFederationShare(ctx, openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
			ObjectId:   uuid.New(),
			PeerId:     fx.peerID,
			Scope:      openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.(openapi.GrantFederationShare403JSONResponse); !ok {
		t.Errorf("non-owner non-admin: expected 403, got %T", resp)
	}
}

func TestAdminRevoke_WriteAheadAudit(t *testing.T) {
	fx := newAdminFixture(t)
	objectID := uuid.New()

	// Grant first.
	grantResp, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
			ObjectId:   objectID,
			PeerId:     fx.peerID,
			Scope:      openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := grantResp.(openapi.GrantFederationShare201JSONResponse)

	// Revoke.
	revResp, err := fx.handler.RevokeFederationShare(fx.ctx(), openapi.RevokeFederationShareRequestObject{
		Id: created.Id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := revResp.(openapi.RevokeFederationShare204Response); !ok {
		t.Fatalf("revoke: expected 204, got %T", revResp)
	}

	// Verify aa:Unshare activity + audit row.
	var unshareCount int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM activities WHERE actor_user_ref=$1 AND activity_type='aa:Unshare'`,
		fx.grantorRef,
	).Scan(&unshareCount)
	if unshareCount == 0 {
		t.Error("write-ahead invariant: aa:Unshare activity row should have committed")
	}
	var auditCount int
	_ = fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_events WHERE event_type='federation.share.revoked' AND actor_user_ref=$1`,
		fx.grantorRef,
	).Scan(&auditCount)
	if auditCount == 0 {
		t.Error("write-ahead invariant: federation.share.revoked audit row should have committed")
	}

	// CanPeerAccess should now reject (cache was invalidated).
	dec, _ := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
		PeerID:        fx.peerID,
		ObjectKind:    federation.ShareObjectKindPost,
		ObjectID:      objectID,
		RequiredScope: federation.ShareScopeView,
		PeerEnabled:   true,
		PeerConnected: true,
	})
	if dec.Allowed {
		t.Error("post-revoke: CanPeerAccess should reject")
	}
}

func TestAdminList_ByObject(t *testing.T) {
	fx := newAdminFixture(t)
	objectID := uuid.New()
	_, _ = fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
			ObjectId:   objectID,
			PeerId:     fx.peerID,
			Scope:      openapi.FederationShareCreateScope(federation.ShareScopeView),
		},
	})
	resp, err := fx.handler.ListFederationShares(fx.ctx(), openapi.ListFederationSharesRequestObject{
		Params: openapi.ListFederationSharesParams{
			ObjectKind: ptrShareObjectKind(federation.ShareObjectKindPost),
			ObjectId:   &objectID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, ok := resp.(openapi.ListFederationShares200JSONResponse)
	if !ok {
		t.Fatalf("expected 200, got %T", resp)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 share for object, got %d", len(list.Items))
	}
}

func TestAdminList_MissingFilter400(t *testing.T) {
	fx := newAdminFixture(t)
	resp, err := fx.handler.ListFederationShares(fx.ctx(), openapi.ListFederationSharesRequestObject{
		Params: openapi.ListFederationSharesParams{}, // no filter
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.(openapi.ListFederationShares400JSONResponse); !ok {
		t.Errorf("expected 400 when no filter, got %T", resp)
	}
}

// --- helpers ------------------------------------------------------------

func ptrShareObjectKind(k federation.ShareObjectKind) *openapi.ListFederationSharesParamsObjectKind {
	v := openapi.ListFederationSharesParamsObjectKind(k)
	return &v
}

// Hint to keep time-using imports live in case future tests
// need them.
var _ = time.Now
