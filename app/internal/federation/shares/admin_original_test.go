// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1573: the original-byte grant through the admin API (grant, list,
// grant context), against a live database and the production handler.
//
// Skips without AA_DB_PASSWORD.

package shares_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func boolPtr(b bool) *bool { return &b }

func (fx *adminFixture) grant(t *testing.T, kind federation.ShareObjectKind, id uuid.UUID, allow *bool) openapi.GrantFederationShareResponseObject {
	t.Helper()
	resp, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{
		Body: &openapi.FederationShareCreate{
			ObjectKind:    openapi.FederationShareCreateObjectKind(kind),
			ObjectId:      id,
			PeerId:        fx.peerID,
			Scope:         openapi.FederationShareCreateScope(federation.ShareScopeRemix),
			AllowOriginal: allow,
		},
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return resp
}

// storedFlag reads the row and the granted audit for the object.
func (fx *adminFixture) storedFlag(t *testing.T, id uuid.UUID) (row bool, audited any, rows int) {
	t.Helper()
	ctx := context.Background()
	if err := fx.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_or(allow_original), FALSE) FROM federation_shares WHERE object_id = $1`, id,
	).Scan(&rows, &row); err != nil {
		t.Fatalf("read share: %v", err)
	}
	var meta []byte
	err := fx.pool.QueryRow(ctx,
		`SELECT metadata FROM audit_events WHERE event_type = 'federation.share.granted' AND metadata->>'object_id' = $1`,
		id.String()).Scan(&meta)
	if err == nil {
		var m map[string]any
		_ = json.Unmarshal(meta, &m)
		audited = m["allow_original"]
	}
	return row, audited, rows
}

// Default off: a grant that does not mention allow_original stores
// false, answers false and audits false, even at the strongest scope.
func TestAdminGrant_AllowOriginalDefaultsFalse(t *testing.T) {
	fx := newAdminFixture(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	resp := fx.grant(t, federation.ShareObjectKindAsset, asset, nil)
	created, ok := resp.(openapi.GrantFederationShare201JSONResponse)
	if !ok {
		t.Fatalf("expected 201, got %T %+v", resp, resp)
	}
	if created.AllowOriginal {
		t.Fatal("response allow_original = true for a grant that did not ask for it")
	}
	row, audited, _ := fx.storedFlag(t, asset)
	if row || audited != false {
		t.Fatalf("stored allow_original = %v, audited = %v; want false / false", row, audited)
	}
}

func TestAdminGrant_AllowOriginalOnPublicAndTeamAsset(t *testing.T) {
	for _, tier := range []string{"public", "team"} {
		t.Run(tier, func(t *testing.T) {
			fx := newAdminFixture(t)
			asset := fx.newOriginalAsset(t, fx.grantorRef, tier, origHash(t))
			resp := fx.grant(t, federation.ShareObjectKindAsset, asset, boolPtr(true))
			created, ok := resp.(openapi.GrantFederationShare201JSONResponse)
			if !ok {
				t.Fatalf("expected 201, got %T %+v", resp, resp)
			}
			if !created.AllowOriginal {
				t.Fatal("response allow_original = false")
			}
			row, audited, _ := fx.storedFlag(t, asset)
			if !row || audited != true {
				t.Fatalf("stored = %v, audited = %v; want true / true", row, audited)
			}
			// The list shows the stored choice.
			kind := openapi.ListFederationSharesParamsObjectKind(federation.ShareObjectKindAsset)
			list, err := fx.handler.ListFederationShares(fx.ctx(), openapi.ListFederationSharesRequestObject{
				Params: openapi.ListFederationSharesParams{ObjectKind: &kind, ObjectId: &asset},
			})
			if err != nil {
				t.Fatal(err)
			}
			items := list.(openapi.ListFederationShares200JSONResponse).Items
			if len(items) != 1 || !items[0].AllowOriginal {
				t.Fatalf("list = %+v, want one share with allow_original", items)
			}
		})
	}
}

// Restricted and embargoed assets are refused at grant time, and nothing
// is written: no share, no activity, no audit.
func TestAdminGrant_AllowOriginalRefusedForDisallowedTier(t *testing.T) {
	for _, tier := range []string{"restricted", "embargo"} {
		t.Run(tier, func(t *testing.T) {
			fx := newAdminFixture(t)
			asset := fx.newOriginalAsset(t, fx.grantorRef, tier, origHash(t))
			resp := fx.grant(t, federation.ShareObjectKindAsset, asset, boolPtr(true))
			bad, ok := resp.(openapi.GrantFederationShare400JSONResponse)
			if !ok {
				t.Fatalf("expected 400, got %T", resp)
			}
			if !strings.HasPrefix(bad.Error, "sensitivity_refused") {
				t.Errorf("error = %q, want sensitivity_refused", bad.Error)
			}
			if _, audited, rows := fx.storedFlag(t, asset); rows != 0 || audited != nil {
				t.Fatalf("refused grant wrote %d share row(s), audit %v", rows, audited)
			}
			// The same asset without the flag is still grantable.
			if _, ok := fx.grant(t, federation.ShareObjectKindAsset, asset, boolPtr(false)).(openapi.GrantFederationShare201JSONResponse); !ok {
				t.Fatal("grant without allow_original refused")
			}
		})
	}
}

func TestAdminGrant_AllowOriginalRefusedForKindsWithoutBytes(t *testing.T) {
	fx := newAdminFixture(t)
	for _, kind := range []federation.ShareObjectKind{
		federation.ShareObjectKindPost, federation.ShareObjectKindWorkspace, federation.ShareObjectKindBrandKit,
	} {
		id := uuid.New()
		resp := fx.grant(t, kind, id, boolPtr(true))
		if _, ok := resp.(openapi.GrantFederationShare400JSONResponse); !ok {
			t.Errorf("%s: expected 400, got %T", kind, resp)
		}
		if _, _, rows := fx.storedFlag(t, id); rows != 0 {
			t.Errorf("%s: refused grant wrote %d row(s)", kind, rows)
		}
	}
	// Collections may carry it (each member is checked at fetch time).
	coll := fx.newCollection(t, fx.grantorRef)
	if _, ok := fx.grant(t, federation.ShareObjectKindCollection, coll, boolPtr(true)).(openapi.GrantFederationShare201JSONResponse); !ok {
		t.Fatal("collection grant with allow_original refused")
	}
}

func TestAdminGrant_AllowOriginalOnMissingAsset404(t *testing.T) {
	fx := newAdminFixture(t)
	resp := fx.grant(t, federation.ShareObjectKindAsset, uuid.New(), boolPtr(true))
	if _, ok := resp.(openapi.GrantFederationShare404JSONResponse); !ok {
		t.Fatalf("expected 404, got %T", resp)
	}
}

// The original-file grant travels only to a peer that negotiated
// original-transfer-1 (#1571), as aa:originalTransfer. The local aa:Share
// record keeps it (allow_original, the audit payload), and every other
// peer is sent the share without it: no member named for it, and the
// origin refuses every original fetch from such a peer anyway.
func TestAdminGrant_AllowOriginalOnTheWireOnlyWithTheProfile(t *testing.T) {
	fx := newAdminFixture(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	if _, ok := fx.grant(t, federation.ShareObjectKindAsset, asset, boolPtr(true)).(openapi.GrantFederationShare201JSONResponse); !ok {
		t.Fatal("grant refused")
	}
	var payload []byte
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT payload FROM activities
		  WHERE activity_type = 'aa:Share' AND actor_user_ref = $1 AND payload::text LIKE '%' || $2 || '%'`,
		fx.grantorRef, asset.String()).Scan(&payload); err != nil {
		t.Fatalf("read aa:Share: %v", err)
	}
	lifecycle := func(t string) bool { return t == federation.ProfileShareLifecycle1 }
	both := func(t string) bool { return lifecycle(t) || t == federation.ProfileOriginalTransfer1 }
	wire, err := federation.WireMembers(federation.ActivityAAShare, "asset", payload, lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	for name := range wire {
		if strings.Contains(strings.ToLower(name), "original") {
			t.Errorf("a peer without original-transfer-1 is sent %q", name)
		}
	}
	wire, err = federation.WireMembers(federation.ActivityAAShare, "asset", payload, both)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire[federation.MemberOriginalTransfer]) != "true" {
		t.Errorf("with original-transfer-1: %s = %s, want true", federation.MemberOriginalTransfer, wire[federation.MemberOriginalTransfer])
	}
	if _, ok := wire["allow_original"]; ok {
		t.Error("the local member name is on the wire")
	}
}

// --- grant context -------------------------------------------------------

func (fx *adminFixture) grantContext(t *testing.T, ctx context.Context, kind federation.ShareObjectKind, id uuid.UUID) openapi.GetFederationShareGrantContextResponseObject {
	t.Helper()
	resp, err := fx.handler.GetFederationShareGrantContext(ctx, openapi.GetFederationShareGrantContextRequestObject{
		Params: openapi.GetFederationShareGrantContextParams{
			ObjectKind: openapi.GetFederationShareGrantContextParamsObjectKind(kind),
			ObjectId:   id,
		},
	})
	if err != nil {
		t.Fatalf("grant context: %v", err)
	}
	return resp
}

func (fx *adminFixture) setRightsField(t *testing.T, asset uuid.UUID, code, value string) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_text)
		 SELECT $1, id, $3 FROM field_definition WHERE code = $2 AND subject_kind = 'asset'`,
		asset, code, value); err != nil {
		t.Fatalf("set %s: %v", code, err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM asset_field_value WHERE asset_id = $1`, asset)
	})
}

func TestGrantContext_Asset(t *testing.T) {
	fx := newAdminFixture(t)
	public := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	fx.setRightsField(t, public, "copyright", "© 2026 Test Studio")
	fx.setRightsField(t, public, "credit", "Photo: Test Studio")
	got, ok := fx.grantContext(t, fx.ctx(), federation.ShareObjectKindAsset, public).(openapi.GetFederationShareGrantContext200JSONResponse)
	if !ok {
		t.Fatal("expected 200")
	}
	if !got.OriginalAllowed || got.OriginalRefusal != nil {
		t.Errorf("public asset: original_allowed=%v refusal=%v", got.OriginalAllowed, got.OriginalRefusal)
	}
	if got.Sensitivity == nil || *got.Sensitivity != "public" || got.Title == nil || *got.Title != "Original Test" {
		t.Errorf("tier/title = %v / %v", got.Sensitivity, got.Title)
	}
	if got.CopyrightNotice == nil || *got.CopyrightNotice != "© 2026 Test Studio" || got.Credit == nil || *got.Credit != "Photo: Test Studio" {
		t.Errorf("copyright_notice = %v, credit = %v", got.CopyrightNotice, got.Credit)
	}

	restricted := fx.newOriginalAsset(t, fx.grantorRef, "restricted", origHash(t))
	got, _ = fx.grantContext(t, fx.ctx(), federation.ShareObjectKindAsset, restricted).(openapi.GetFederationShareGrantContext200JSONResponse)
	if got.OriginalAllowed || got.OriginalRefusal == nil || *got.OriginalRefusal != "sensitivity_refused" {
		t.Errorf("restricted asset: original_allowed=%v refusal=%v", got.OriginalAllowed, got.OriginalRefusal)
	}
	if got.CopyrightNotice != nil || got.Credit != nil {
		t.Errorf("restricted asset without those fields: copyright_notice = %v, credit = %v", got.CopyrightNotice, got.Credit)
	}

	if _, ok := fx.grantContext(t, fx.ctx(), federation.ShareObjectKindAsset, uuid.New()).(openapi.GetFederationShareGrantContext404JSONResponse); !ok {
		t.Error("missing asset: expected 404")
	}
}

func TestGrantContext_KindsAndPermission(t *testing.T) {
	fx := newAdminFixture(t)
	coll := fx.newCollection(t, fx.grantorRef)
	got, ok := fx.grantContext(t, fx.ctx(), federation.ShareObjectKindCollection, coll).(openapi.GetFederationShareGrantContext200JSONResponse)
	if !ok || !got.OriginalAllowed {
		t.Errorf("collection: %+v", got)
	}
	for _, kind := range []federation.ShareObjectKind{federation.ShareObjectKindWorkspace, federation.ShareObjectKindBrandKit} {
		got, ok := fx.grantContext(t, fx.ctx(), kind, uuid.New()).(openapi.GetFederationShareGrantContext200JSONResponse)
		if !ok || got.OriginalAllowed || got.OriginalRefusal == nil || *got.OriginalRefusal != "kind_without_bytes" {
			t.Errorf("%s: %+v", kind, got)
		}
	}
	if _, ok := fx.grantContext(t, fx.ctx(), federation.ShareObjectKindCollection, uuid.New()).(openapi.GetFederationShareGrantContext404JSONResponse); !ok {
		t.Error("missing collection: expected 404")
	}

	// A caller who neither owns the asset nor holds system.admin is
	// refused, as on the grant path.
	other := fx.newUser(t)
	asset := fx.newOriginalAsset(t, fx.grantorRef, "public", origHash(t))
	stranger := auth.WithIdentity(context.Background(), &auth.Identity{UserRef: other, Username: "stranger", AuthMethod: "session"})
	if _, ok := fx.grantContext(t, stranger, federation.ShareObjectKindAsset, asset).(openapi.GetFederationShareGrantContext403JSONResponse); !ok {
		t.Error("non-owner: expected 403")
	}
	if _, ok := fx.grantContext(t, context.Background(), federation.ShareObjectKindAsset, asset).(openapi.GetFederationShareGrantContext401JSONResponse); !ok {
		t.Error("anonymous: expected 401")
	}
}

// The grant-context enums are nullable, and null is carried by
// nullability, never by an enum member: an enum listing `null` made
// oapi-codegen emit a "<nil>" value that Valid() accepted.
func TestGrantContextEnums_NoNilMember(t *testing.T) {
	if openapi.FederationShareGrantContextSensitivity("<nil>").Valid() {
		t.Error(`sensitivity accepts "<nil>"`)
	}
	if openapi.FederationShareGrantContextOriginalRefusal("<nil>").Valid() {
		t.Error(`original_refusal accepts "<nil>"`)
	}
	for _, v := range []string{"public", "team", "restricted", "embargo"} {
		if !openapi.FederationShareGrantContextSensitivity(v).Valid() {
			t.Errorf("sensitivity refuses %q", v)
		}
	}
	for _, v := range []string{"kind_without_bytes", "sensitivity_refused"} {
		if !openapi.FederationShareGrantContextOriginalRefusal(v).Valid() {
			t.Errorf("original_refusal refuses %q", v)
		}
	}
	// A refusal is null, not an empty or placeholder string, when the
	// original is allowed: absent from the JSON.
	raw, err := json.Marshal(openapi.FederationShareGrantContext{OriginalAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"original_refusal", "sensitivity", "copyright_notice", "credit"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("unset %s serialised: %s", k, raw)
		}
	}
}
