// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: the explicit access-copy grant (allow_access_copies) on the
// existing admin share API and the typed Registry paths, against a real
// database through the production handler. It is independent of scope and
// of allow_original, off unless set, only on asset and collection shares,
// and refused for a direct asset outside public and team. A live duplicate
// answers 409 with the exact stored row and both its flags; a regrant
// after expiry takes only the newly supplied flags. The grant is local
// only in this slice: nothing here changes what is sent to a peer.
//
// Skips without AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// grantFlags grants kind/id to the fixture's peer at remix with the two
// byte flags as given (nil = omitted).
func (fx *adminFixture) grantFlags(t *testing.T, kind federation.ShareObjectKind, id uuid.UUID, original, access *bool) openapi.GrantFederationShareResponseObject {
	t.Helper()
	resp, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind:        openapi.FederationShareCreateObjectKind(kind),
			ObjectId:          id,
			PeerId:            fx.peerID,
			Scope:             openapi.FederationShareCreateScope(federation.ShareScopeRemix),
			AllowOriginal:     original,
			AllowAccessCopies: access,
		},
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return resp
}

type storedGrant struct {
	rows, liveRows, grantedAudits, shareActivities int
	original, access                               bool // of the live row
	auditedOriginal, auditedAccess                 any  // of the latest granted audit
	auditCorrelation, rowActivity                  string
}

// stored reads every row, audit and aa:Share activity for the object.
func (fx *adminFixture) stored(t *testing.T, id uuid.UUID) storedGrant {
	t.Helper()
	ctx := context.Background()
	var g storedGrant
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL),
		        COALESCE(bool_or(allow_original) FILTER (WHERE revoked_at IS NULL), FALSE),
		        COALESCE(bool_or(allow_access_copies) FILTER (WHERE revoked_at IS NULL), FALSE),
		        COALESCE(max(granted_activity_id::text) FILTER (WHERE revoked_at IS NULL), '')
		   FROM federation_shares WHERE object_id = $1`, id,
	).Scan(&g.rows, &g.liveRows, &g.original, &g.access, &g.rowActivity); err != nil {
		t.Fatalf("read shares: %v", err)
	}
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE event_type = 'federation.share.granted' AND metadata->>'object_id' = $1`,
		id.String()).Scan(&g.grantedAudits); err != nil {
		t.Fatal(err)
	}
	var meta []byte
	err := fx.pool.QueryRow(ctx,
		`SELECT metadata FROM audit_events WHERE event_type = 'federation.share.granted' AND metadata->>'object_id' = $1
		 ORDER BY occurred_at DESC LIMIT 1`, id.String()).Scan(&meta)
	if err == nil {
		var m map[string]any
		_ = json.Unmarshal(meta, &m)
		g.auditedOriginal, g.auditedAccess = m["allow_original"], m["allow_access_copies"]
		g.auditCorrelation, _ = m["correlation_id"].(string)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*) FROM activities WHERE activity_type = 'aa:Share' AND actor_user_ref = $1
		   AND id IN (SELECT granted_activity_id FROM federation_shares WHERE object_id = $2)`,
		fx.grantorRef, id).Scan(&g.shareActivities); err != nil {
		t.Fatal(err)
	}
	return g
}

func (fx *adminFixture) listFor(t *testing.T, kind federation.ShareObjectKind, id uuid.UUID) []openapi.FederationShare {
	t.Helper()
	k := openapi.ListFederationSharesParamsObjectKind(kind)
	list, err := fx.handler.ListFederationShares(fx.ctx(), openapi.ListFederationSharesRequestObject{
		Params: openapi.ListFederationSharesParams{ObjectKind: &k, ObjectId: &id},
	})
	if err != nil {
		t.Fatal(err)
	}
	return list.(openapi.ListFederationShares200JSONResponse).Items
}

func mustCreatedShare(t *testing.T, resp openapi.GrantFederationShareResponseObject) openapi.FederationShare {
	t.Helper()
	c, ok := resp.(openapi.GrantFederationShare201JSONResponse)
	if !ok {
		t.Fatalf("expected 201, got %T %+v", resp, resp)
	}
	return openapi.FederationShare(c)
}

// Omitted: false everywhere, also beside an original-file grant and at the
// strongest scope. Neither grant implies the other.
func TestAdminGrant_AllowAccessCopiesDefaultsFalse(t *testing.T) {
	for _, c := range []struct {
		name     string
		original *bool
	}{{"nothing set", nil}, {"original only", boolPtr(true)}} {
		t.Run(c.name, func(t *testing.T) {
			fx := newAdminFixture(t)
			asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
			created := mustCreatedShare(t, fx.grantFlags(t, federation.ShareObjectKindAsset, asset, c.original, nil))
			if created.AllowAccessCopies {
				t.Fatal("response allow_access_copies = true for a grant that did not ask for it")
			}
			if created.AllowOriginal != (c.original != nil) {
				t.Fatalf("response allow_original = %v", created.AllowOriginal)
			}
			g := fx.stored(t, asset)
			if g.access || g.auditedAccess != false || g.auditedOriginal != (c.original != nil) {
				t.Fatalf("stored access=%v, audited access=%v original=%v", g.access, g.auditedAccess, g.auditedOriginal)
			}
			if items := fx.listFor(t, federation.ShareObjectKindAsset, asset); len(items) != 1 || items[0].AllowAccessCopies {
				t.Fatalf("list = %+v, want one share without allow_access_copies", items)
			}
		})
	}
}

// Access only, and both: stored, answered, listed and audited exactly as
// asked, in the grant's own transaction.
func TestAdminGrant_AllowAccessCopiesAloneAndWithOriginal(t *testing.T) {
	for _, c := range []struct {
		name     string
		tier     string
		original bool
	}{{"access only, public", "public", false}, {"both, team", "team", true}} {
		t.Run(c.name, func(t *testing.T) {
			fx := newAdminFixture(t)
			asset := fx.newOriginalAsset(t, fx.grantorRef, c.tier, origHash(t))
			created := mustCreatedShare(t, fx.grantFlags(t, federation.ShareObjectKindAsset, asset, boolPtr(c.original), boolPtr(true)))
			if !created.AllowAccessCopies || created.AllowOriginal != c.original {
				t.Fatalf("response access=%v original=%v, want true / %v", created.AllowAccessCopies, created.AllowOriginal, c.original)
			}
			g := fx.stored(t, asset)
			if !g.access || g.original != c.original || g.auditedAccess != true || g.auditedOriginal != c.original {
				t.Fatalf("stored access=%v original=%v; audited access=%v original=%v", g.access, g.original, g.auditedAccess, g.auditedOriginal)
			}
			if g.rows != 1 || g.grantedAudits != 1 || g.shareActivities != 1 || g.auditCorrelation != g.rowActivity {
				t.Fatalf("rows=%d audits=%d activities=%d correlation=%s row activity=%s; want one of each, the audit naming the row's activity",
					g.rows, g.grantedAudits, g.shareActivities, g.auditCorrelation, g.rowActivity)
			}
			items := fx.listFor(t, federation.ShareObjectKindAsset, asset)
			if len(items) != 1 || !items[0].AllowAccessCopies || items[0].AllowOriginal != c.original || items[0].Id != created.Id {
				t.Fatalf("list = %+v", items)
			}
		})
	}
}

// A live duplicate answers 409 with the exact stored row and both its
// flags, whatever this request asked for, and writes nothing.
func TestAdminGrant_AccessCopies409CarriesTheStoredFlags(t *testing.T) {
	fx := newAdminFixture(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	first := mustCreatedShare(t, fx.grantFlags(t, federation.ShareObjectKindAsset, asset, nil, boolPtr(true)))
	before := fx.stored(t, asset)

	for _, ask := range []struct{ original, access *bool }{
		{boolPtr(true), boolPtr(false)}, {nil, nil}, {boolPtr(true), boolPtr(true)},
	} {
		resp := fx.grantFlags(t, federation.ShareObjectKindAsset, asset, ask.original, ask.access)
		conflict, ok := resp.(openapi.GrantFederationShare409JSONResponse)
		if !ok {
			t.Fatalf("expected 409, got %T", resp)
		}
		if conflict.Id != first.Id || !conflict.AllowAccessCopies || conflict.AllowOriginal {
			t.Fatalf("409 = id %s access %v original %v; want %s, true, false (the stored row)",
				conflict.Id, conflict.AllowAccessCopies, conflict.AllowOriginal, first.Id)
		}
	}
	if after := fx.stored(t, asset); after != before {
		t.Fatalf("a refused duplicate wrote something: before %+v after %+v", before, after)
	}
}

// After expiry a regrant takes only what it supplies: the access flag of
// the expired row is not inherited, and the old row is closed, not edited.
func TestAdminGrant_AccessCopiesRegrantAfterExpiryTakesOnlyTheNewFlags(t *testing.T) {
	fx := newAdminFixture(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	old := mustCreatedShare(t, fx.grantFlags(t, federation.ShareObjectKindAsset, asset, nil, boolPtr(true)))
	expireNow(t, fx.pool, old.Id)

	regrant := mustCreatedShare(t, fx.grantFlags(t, federation.ShareObjectKindAsset, asset, boolPtr(true), nil))
	if regrant.Id == old.Id || regrant.AllowAccessCopies || !regrant.AllowOriginal {
		t.Fatalf("regrant id %s access %v original %v; want a new row, false, true", regrant.Id, regrant.AllowAccessCopies, regrant.AllowOriginal)
	}
	g := fx.stored(t, asset)
	if g.rows != 2 || g.liveRows != 1 || g.access || !g.original {
		t.Fatalf("rows=%d live=%d live access=%v original=%v; want 2 / 1 / false / true", g.rows, g.liveRows, g.access, g.original)
	}
	var oldAccess, oldRevoked bool
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT allow_access_copies, revoked_at IS NOT NULL FROM federation_shares WHERE id = $1`, old.Id,
	).Scan(&oldAccess, &oldRevoked); err != nil {
		t.Fatal(err)
	}
	if !oldAccess || !oldRevoked {
		t.Fatalf("expired row access=%v revoked=%v; want it kept as it was (true) and closed", oldAccess, oldRevoked)
	}
}

// A direct asset outside public and team is refused at grant time, with
// nothing written; the original-file message is kept whenever the original
// grant is asked for.
func TestAdminGrant_AccessCopiesRefusedForDisallowedTier(t *testing.T) {
	for _, tier := range []string{"restricted", "embargo"} {
		t.Run(tier, func(t *testing.T) {
			fx := newAdminFixture(t)
			asset := fx.newOriginalAsset(t, fx.grantorRef, tier, origHash(t))
			for _, c := range []struct {
				original *bool
				want     string
			}{{nil, "sensitivity_refused: access copies"}, {boolPtr(true), "sensitivity_refused: original files"}} {
				bad, ok := fx.grantFlags(t, federation.ShareObjectKindAsset, asset, c.original, boolPtr(true)).(openapi.GrantFederationShare400JSONResponse)
				if !ok || !strings.HasPrefix(bad.Error, c.want) {
					t.Fatalf("original %v: %+v, want 400 %q", c.original != nil, bad, c.want)
				}
			}
			if g := fx.stored(t, asset); g.rows != 0 || g.grantedAudits != 0 {
				t.Fatalf("refused grant wrote %d row(s), %d audit(s)", g.rows, g.grantedAudits)
			}
			// Without either flag the asset is still grantable.
			if created := mustCreatedShare(t, fx.grantFlags(t, federation.ShareObjectKindAsset, asset, nil, nil)); created.AllowAccessCopies {
				t.Fatal("a plain grant reads allow_access_copies")
			}
		})
	}
	fx := newAdminFixture(t)
	if _, ok := fx.grantFlags(t, federation.ShareObjectKindAsset, uuid.New(), nil, boolPtr(true)).(openapi.GrantFederationShare404JSONResponse); !ok {
		t.Fatal("access-copy grant on a missing asset: want 404")
	}
}

// Only asset and collection shares carry it; other kinds are refused before
// anything is written, through the handler and through Registry.Insert.
func TestAdminGrant_AccessCopiesKinds(t *testing.T) {
	fx := newAdminFixture(t)
	for _, kind := range []federation.ShareObjectKind{
		federation.ShareObjectKindPost, federation.ShareObjectKindWorkspace, federation.ShareObjectKindBrandKit,
	} {
		id := uuid.New()
		bad, ok := fx.grantFlags(t, kind, id, nil, boolPtr(true)).(openapi.GrantFederationShare400JSONResponse)
		if !ok || bad.Error != shares.ErrAccessCopyKindInvalid.Error() {
			t.Errorf("%s: %+v, want 400 %q", kind, bad, shares.ErrAccessCopyKindInvalid)
		}
		if g := fx.stored(t, id); g.rows != 0 {
			t.Errorf("%s: refused grant wrote %d row(s)", kind, g.rows)
		}
		in := shares.InsertInput{ObjectKind: kind, Scope: federation.ShareScopeView, AllowAccessCopies: true}
		if err := in.Validate(); !errors.Is(err, shares.ErrAccessCopyKindInvalid) {
			t.Errorf("%s: Validate = %v, want ErrAccessCopyKindInvalid", kind, err)
		}
	}
	// The original-file kind error is unchanged and still takes precedence.
	in := shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, Scope: federation.ShareScopeView, AllowOriginal: true, AllowAccessCopies: true}
	if err := in.Validate(); !errors.Is(err, shares.ErrOriginalKindInvalid) {
		t.Errorf("both flags on a post: Validate = %v, want ErrOriginalKindInvalid", err)
	}
	coll := fx.newCollection(t, fx.grantorRef)
	if created := mustCreatedShare(t, fx.grantFlags(t, federation.ShareObjectKindCollection, coll, nil, boolPtr(true))); !created.AllowAccessCopies {
		t.Fatal("collection grant lost allow_access_copies")
	}
}

// Registry.Insert writes the typed flag, and the transactional gate's
// locked rows carry both flags without changing its decision.
func TestRegistry_AccessCopiesInsertAndLockedRows(t *testing.T) {
	fx := newGateFixture(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	in := assetShare(asset, federation.ShareScopeView, false)
	in.AllowAccessCopies = true
	s := fx.insertShare(t, in)
	if !s.AllowAccessCopies || s.AllowOriginal {
		t.Fatalf("inserted access=%v original=%v, want true / false", s.AllowAccessCopies, s.AllowOriginal)
	}
	tx, err := fx.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dec, err := fx.reg.CanPeerAccessTx(context.Background(), tx, shares.AccessRequest{
		PeerID: fx.peerID, ObjectKind: federation.ShareObjectKindAsset, ObjectID: asset, RequiredScope: federation.ShareScopeView,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dec.Allowed || dec.Share == nil || dec.Share.ID != s.ID || !dec.Share.AllowAccessCopies || dec.Share.AllowOriginal {
		t.Fatalf("gate decision %+v share %+v; want allowed through the inserted share with access=true original=false", dec, dec.Share)
	}
}
