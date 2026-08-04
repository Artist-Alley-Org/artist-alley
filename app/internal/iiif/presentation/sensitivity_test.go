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

	"github.com/mscrnt/artist-alley/app/internal/visibility"
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

// memberRefFor builds a member EntityRef the way LoadCollectionMembers
// does — including the MemberReadable flag, decided by
// visibility.FieldsReadable rather than restated here (#883). The
// builder consults ONLY that flag now, so a hand-built ref that skips it
// is dropped; that fail-closed default is deliberate and this helper is
// what keeps the unit tests honest about it.
func memberRefFor(s Sensitivity, caller visibility.Caller) EntityRef {
	r := assetRef(s)
	// A per-tier title, because assetRef gives every ref the same one and
	// a leak assertion cannot tell a legitimately-published public label
	// from a withheld restricted one when they are identical.
	r.Title = "title-of-" + string(s)
	r.MemberReadable = visibility.FieldsReadable(visibility.FieldsRow{
		Sensitivity:      string(s),
		Status:           "active",
		ProcessingStatus: "ready",
	}, caller, nil)
	return r
}

// TestCollectionManifest_AnonymousDropsRestrictedMembers pins the
// per-member filter — the gate keeping a restricted member out of an
// anonymous collection manifest.
func TestCollectionManifest_AnonymousDropsRestrictedMembers(t *testing.T) {
	b := testBuilder()
	anon := visibility.NewCaller(nil)
	parent := EntityRef{ID: uuid.New(), Kind: EntityCollection, Title: "coll", Sensitivity: SensitivityPublic}
	members := []EntityRef{
		memberRefFor(SensitivityPublic, anon),
		memberRefFor(SensitivityRestricted, anon),
		memberRefFor(SensitivityTeam, anon),
		memberRefFor(SensitivityPublic, anon),
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

// TestCollectionManifest_AuthenticatedDropsRestrictedMembers is the case
// the check above never covered (#883). Until this issue the per-member
// filter ran only `if isAnonymous`, so a signed-in caller received the
// restricted member's TITLE as its label. The parallel structure is the
// point: same members, same expectation, different caller.
func TestCollectionManifest_AuthenticatedDropsRestrictedMembers(t *testing.T) {
	b := testBuilder()
	ref := int64(8835001)
	stranger := visibility.NewCaller(&ref)
	parent := EntityRef{ID: uuid.New(), Kind: EntityCollection, Title: "coll", Sensitivity: SensitivityPublic}
	members := []EntityRef{
		memberRefFor(SensitivityPublic, stranger),
		memberRefFor(SensitivityRestricted, stranger),
		memberRefFor(SensitivityTeam, stranger),
		memberRefFor(SensitivityPublic, stranger),
	}
	cm, err := b.BuildCollectionManifest(parent, members, false)
	if err != nil {
		t.Fatalf("authenticated collection: unexpected err %v", err)
	}
	if len(cm.Items) != 2 {
		t.Errorf("signed-in stranger's collection listed %d members; want 2 — a restricted "+
			"member's title used to ship as its label to any authenticated caller", len(cm.Items))
	}
	for _, it := range cm.Items {
		for _, lang := range it.Label {
			for _, v := range lang {
				if v == "title-of-restricted" || v == "title-of-team" {
					t.Errorf("a gated member's title leaked as a manifest label: %v", it)
				}
			}
		}
	}
}

// TestCollectionManifest_UnflaggedMemberIsDropped pins the fail-closed
// default directly: a member ref that never went through the loader has
// MemberReadable false and must not be published, however public its
// tier looks.
func TestCollectionManifest_UnflaggedMemberIsDropped(t *testing.T) {
	b := testBuilder()
	parent := EntityRef{ID: uuid.New(), Kind: EntityCollection, Title: "coll", Sensitivity: SensitivityPublic}
	cm, err := b.BuildCollectionManifest(parent, []EntityRef{assetRef(SensitivityPublic)}, false)
	if err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	if len(cm.Items) != 0 {
		t.Error("a member that never passed through LoadCollectionMembers was published — " +
			"the gate must fail closed on the zero value")
	}
}
