// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #432 — the IIIF content-plane sensitivity gate is NOT redundant with
// the row-visibility predicate, and these tests pin that it is the SOLE
// enforcement on the manifest route.
//
// The issue asked whether P0a (the row predicate, ADR 0063) made this
// gate redundant. It did not — but the reason #432 gave has since been
// fixed out from under it, and the corrected reason is worth stating.
//
// #432's answer was "the manifest route never invokes the predicate at
// all": LoadAsset / LoadCollectionMembers read by id with `deleted_at
// IS NULL` and nothing else. That was true, and it was also a bug —
// the missing row plane is #661, and loader.go now splices the
// predicate into every query.
//
// The gate still stays, on the ORIGINAL two-planes argument: the
// AUTHENTICATED EntityAsset predicate is soft-delete only (deliberately
// — ADR 0063), so the row plane admits a signed-in caller to a
// restricted asset's existence and these checks are what decide the
// anonymous manifest's contents. Remove them and the sabotage
// direction of these tests fires.
//
// Pure unit tests: BuildAssetManifest takes an EntityRef and a bool, so
// no database is involved — the assertion is on the OUTCOME (404 / stub
// / full) exactly as the brief requires, not on code shape.

package presentation

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testBuilder() *Builder {
	return NewBuilder(BuilderConfig{
		SiteBaseURL: "https://art.example.test",
		Provider:    Provider{Label: EN("Test"), Type: "Agent"},
	})
}

func assetRef(s Sensitivity) EntityRef {
	return EntityRef{ID: uuid.New(), Kind: EntityAsset, Title: "secret-title", Sensitivity: s}
}

// TestAssetManifest_AnonymousRestricted_Is404 pins that a restricted
// asset refuses (the HTTP layer maps ErrRestricted to 404, never 403,
// so existence is not confirmed).
func TestAssetManifest_AnonymousRestricted_Is404(t *testing.T) {
	b := testBuilder()
	for _, tier := range []Sensitivity{SensitivityRestricted, SensitivityTeam} {
		m, err := b.BuildAssetManifest(assetRef(tier), true)
		if !errors.Is(err, ErrRestricted) {
			t.Errorf("%s anonymous: err=%v, want ErrRestricted", tier, err)
		}
		if m != nil {
			t.Errorf("%s anonymous: a manifest was returned; a refused asset must yield nil", tier)
		}
	}
}

// TestAssetManifest_AnonymousEmbargo_IsStub pins that an embargoed
// asset returns a stub — a label with no canvases and no metadata, so
// the title is acknowledged but nothing renderable leaks (ADR 0020).
func TestAssetManifest_AnonymousEmbargo_IsStub(t *testing.T) {
	b := testBuilder()
	future := time.Now().Add(48 * time.Hour)
	ref := assetRef(SensitivityPublic) // embargo gates on the date, independent of tier
	ref.EmbargoUntil = &future

	m, err := b.BuildAssetManifest(ref, true)
	if err != nil {
		t.Fatalf("embargo anonymous: unexpected err %v", err)
	}
	if m == nil {
		t.Fatal("embargo anonymous: nil manifest; want a stub")
	}
	if len(m.Items) != 0 {
		t.Errorf("embargo stub carries %d canvases; a stub must have none", len(m.Items))
	}
	if len(m.Metadata) != 0 {
		t.Errorf("embargo stub carries %d metadata pairs; a stub must leak none", len(m.Metadata))
	}
}

// TestAssetManifest_AnonymousPublic_IsFull confirms the gate does not
// over-refuse: a public asset renders a full manifest with canvases.
func TestAssetManifest_AnonymousPublic_IsFull(t *testing.T) {
	b := testBuilder()
	m, err := b.BuildAssetManifest(assetRef(SensitivityPublic), true)
	if err != nil {
		t.Fatalf("public anonymous: unexpected err %v", err)
	}
	if m == nil || len(m.Items) == 0 {
		t.Fatalf("public anonymous: expected a full manifest with canvases, got %+v", m)
	}
}

// TestAssetManifest_Authenticated_SeesRestricted is the other side of
// the plane split: an authenticated caller reaches the manifest of a
// restricted asset. The content-plane gate is scoped to ANONYMOUS
// callers, because the row plane already admitted this caller to the
// asset's existence — the two planes answer different questions.
func TestAssetManifest_Authenticated_SeesRestricted(t *testing.T) {
	b := testBuilder()
	m, err := b.BuildAssetManifest(assetRef(SensitivityRestricted), false)
	if err != nil {
		t.Fatalf("authenticated restricted: unexpected err %v", err)
	}
	if m == nil || len(m.Items) == 0 {
		t.Fatal("authenticated restricted: expected a full manifest")
	}
}

// TestCollectionManifest_AnonymousDropsRestrictedMembers pins the
// per-member filter — the sole gate keeping a restricted member out of
// an anonymous collection manifest, since LoadCollectionMembers applies
// no predicate.
func TestCollectionManifest_AnonymousDropsRestrictedMembers(t *testing.T) {
	b := testBuilder()
	parent := EntityRef{ID: uuid.New(), Kind: EntityCollection, Title: "coll", Sensitivity: SensitivityPublic}
	members := []EntityRef{
		assetRef(SensitivityPublic),
		assetRef(SensitivityRestricted),
		assetRef(SensitivityTeam),
		assetRef(SensitivityPublic),
	}
	cm, err := b.BuildCollectionManifest(parent, members, true)
	if err != nil {
		t.Fatalf("anonymous collection: unexpected err %v", err)
	}
	if len(cm.Items) != 2 {
		t.Errorf("anonymous collection listed %d members; want 2 (the public ones) — "+
			"restricted/team members must be dropped", len(cm.Items))
	}
}
