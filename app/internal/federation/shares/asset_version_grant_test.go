// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 sender: an original-file share of an asset, to a peer that
// negotiated archival-1 and original-transfer-1, also records the
// aa:AssetVersion that announces the asset's current version, in the
// grant's own transaction; nothing else does. Skips without
// AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

var assetProfiles = []string{federation.ProfileShareLifecycle1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1}

type announcement struct {
	id, actorURI, objectURI string
	actorRef                int64
	payload                 map[string]string
}

// announcements lists the aa:AssetVersion activities recorded for asset.
func (fx *adminFixture) announcements(t *testing.T, asset uuid.UUID) []announcement {
	t.Helper()
	rows, err := fx.pool.Query(context.Background(), `SELECT id::text, actor_uri, object_uri, actor_user_ref, payload
		FROM activities WHERE activity_type = 'aa:AssetVersion' AND object_local_id = $1`, asset.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []announcement
	for rows.Next() {
		var a announcement
		var raw []byte
		if err := rows.Scan(&a.id, &a.actorURI, &a.objectURI, &a.actorRef, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM activities WHERE activity_type = 'aa:AssetVersion' AND object_local_id = $1`, asset.String())
	})
	return out
}

func (fx *adminFixture) grantBody(t *testing.T, body openapi.FederationShareCreate) openapi.GrantFederationShare201JSONResponse {
	t.Helper()
	resp, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	created, ok := resp.(openapi.GrantFederationShare201JSONResponse)
	if !ok {
		t.Fatalf("grant refused: %T %+v", resp, resp)
	}
	return created
}

func TestAdminGrant_OriginalAssetShareAnnouncesTheCurrentVersion(t *testing.T) {
	fx := newAdminFixture(t)
	fx.negotiate(t, fx.peerID, assetProfiles...)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	created := fx.grantBody(t, openapi.FederationShareCreate{
		ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindAsset), ObjectId: asset,
		PeerId: fx.peerID, Scope: openapi.FederationShareCreateScope(federation.ShareScopeView), AllowOriginal: boolPtr(true),
	})
	got := fx.announcements(t, asset)
	if len(got) != 1 {
		t.Fatalf("%d announcements, want 1", len(got))
	}
	a := got[0]
	var shareActivity, version uuid.UUID
	if err := fx.pool.QueryRow(context.Background(), `SELECT s.granted_activity_id, a.current_version_id
		FROM federation_shares s, assets a WHERE s.id = $1 AND a.id = $2`, created.Id, asset).Scan(&shareActivity, &version); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"share_id": created.Id.String(), "share_activity_id": shareActivity.String(),
		"asset_id": asset.String(), "version_id": version.String()}
	if len(a.payload) != len(want) {
		t.Fatalf("payload %v, want exactly %v", a.payload, want)
	}
	for k, v := range want {
		if a.payload[k] != v {
			t.Errorf("payload %s = %q, want %q", k, a.payload[k], v)
		}
	}
	// The owner announces its own asset; the object is the asset URI.
	if a.actorRef != fx.grantorRef || a.actorURI != "https://local.example/users/local-user" ||
		a.objectURI != "https://local.example/assets/"+asset.String() {
		t.Errorf("actor %d %q object %q", a.actorRef, a.actorURI, a.objectURI)
	}
	// After the aa:Share, in the same transaction (one creation time).
	var same bool
	if err := fx.pool.QueryRow(context.Background(), `SELECT (SELECT created_at FROM activities WHERE id = $1) =
		(SELECT created_at FROM activities WHERE id = $2::uuid)`, shareActivity, a.id).Scan(&same); err != nil || !same {
		t.Errorf("not recorded in the grant's transaction (%v)", err)
	}
}

func TestAdminGrant_NothingElseAnnouncesAnAsset(t *testing.T) {
	fx := newAdminFixture(t)
	fx.negotiate(t, fx.peerID, assetProfiles...)
	share := func(asset uuid.UUID, allow bool, target *string) {
		t.Helper()
		fx.grantBody(t, openapi.FederationShareCreate{
			ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindAsset), ObjectId: asset,
			PeerId: fx.peerID, Scope: openapi.FederationShareCreateScope(federation.ShareScopeView),
			AllowOriginal: boolPtr(allow), TargetUserUrl: target,
		})
	}
	none := func(name string, asset uuid.UUID) {
		t.Helper()
		if n := len(fx.announcements(t, asset)); n != 0 {
			t.Errorf("%s: %d announcements", name, n)
		}
	}

	a := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	share(a, false, nil)
	none("a share without the original", a)

	b := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	user := "https://test-peer.example/users/bob"
	share(b, true, &user)
	none("a share to one recipient", b)

	origin := uuid.New()
	if _, err := fx.pool.Exec(context.Background(), `INSERT INTO federation_peers (id, instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, $2, 'origin', 'k', TRUE, 'connected', $3)`, origin, "https://origin-"+randHex(t, 4)+".example", fx.grantorRef); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, origin)
	})
	c := fx.newOriginalAssetFrom(t, fx.grantorRef, "public", origHash(t), &origin)
	share(c, true, nil)
	none("a received copy", c)

	// A peer without both profiles: the grant is made, nothing is announced.
	for _, caps := range [][]string{
		{federation.ProfileShareLifecycle1, federation.ProfileOriginalTransfer1},
		{federation.ProfileShareLifecycle1, federation.ProfileArchival1},
	} {
		fx.negotiate(t, fx.peerID, caps...)
		d := fx.newOriginalAsset(t, fx.grantorRef, "team", origHash(t))
		share(d, true, nil)
		none("a peer with only "+caps[1], d)
	}
}
