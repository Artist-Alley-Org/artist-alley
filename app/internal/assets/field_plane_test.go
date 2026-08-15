// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #939 / ADR 0064 — a capability that permits MUTATION confers the
// FIELD plane for the objects it governs, and never the BINARY plane.
//
// The decision this file pins is a two-sided one, so every test that
// asserts the widening also asserts the refusal. A suite that checked
// only "the director can now see the title" would pass unchanged on a
// build that also handed them the original file, and that build is the
// failure this issue exists to prevent.
//
// It reuses maFixture from mutation_authz_test.go deliberately: that
// fixture builds a REAL team hierarchy through team_parents and lets
// the 00001 trigger materialise team_closure, and loads identities
// through auth.Resolver.LoadIdentity so scoped grants arrive
// closure-expanded from the database. The seeded database has no team
// hierarchy at all (11 self-referential closure rows), so a test that
// did not build its own would assert nothing about the closure.
package assets_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// fpTitle / fpDescription / fpTag are the field-plane payload. Each is
// asserted positively on the widened path and negatively on every
// refusal path, so "the placeholder leaked a title" and "the director
// was shown nothing" are distinguishable failures.
const (
	fpTitle       = "fp-embargoed-concept-board"
	fpDescription = "fp-description-that-must-not-leak"
	fpTag         = "fp-tag-that-must-not-leak"
)

// fpAsset seeds a RESTRICTED asset carrying a full field-plane payload
// — title, description, a tag and a thumbhash — plus a `col` variant so
// preview_available has something to be true about.
//
// Separate from maFixture.asset because that helper seeds a bare
// public row: with no description, no tag and no thumbhash there is
// nothing for "the fields arrive and the picture does not" to be
// asserted against, and preview_available would be false for the wrong
// reason.
func fpAsset(f *maFixture, owner *int64, team *uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	hb := make([]byte, 16)
	_, _ = rand.Read(hb)
	hashHex := hex.EncodeToString(sha256.New().Sum(hb))[:64]
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING`,
		hashHex,
	); err != nil {
		f.t.Fatalf("seed storage_object: %v", err)
	}
	// A servable `col` rung, so preview_available is gated by the rule
	// under test rather than by the absence of a variant.
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type)
		VALUES ($1, 'col', 512, 'image/webp') ON CONFLICT DO NOTHING`,
		hashHex,
	); err != nil {
		f.t.Fatalf("seed storage_variant: %v", err)
	}
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO assets (id, title, description, asset_type, owner_user_ref, team_id,
		                    status, processing_status, file_hash, file_extension,
		                    file_size_bytes, sensitivity, thumbhash)
		VALUES ($1, $2, $3, 1, $4, $5, 'active', 'ready', $6, 'png', 1024,
		        'restricted', '\x0102030405'::bytea)`,
		id, fpTitle, fpDescription, owner, teamArg, hashHex,
	); err != nil {
		f.t.Fatalf("seed restricted asset: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO asset_tag (asset_id, tag) VALUES ($1, $2)`, id, fpTag,
	); err != nil {
		f.t.Fatalf("seed asset tag: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM asset_tag WHERE asset_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, hashHex)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hashHex)
	})
	return id
}

// fpGet runs GET /assets/{id} through the handler and returns the
// asset payload.
func fpGet(f *maFixture, ctx context.Context, id uuid.UUID) openapi.Asset {
	f.t.Helper()
	resp, err := f.h.GetAsset(ctx, openapi.GetAssetRequestObject{Id: openapi_types.UUID(id)})
	if err != nil {
		f.t.Fatalf("GetAsset: %v", err)
	}
	ok, isOK := resp.(openapi.GetAsset200JSONResponse)
	if !isOK {
		f.t.Fatalf("GetAsset: want 200, got %T", resp)
	}
	return openapi.Asset(ok)
}

// assertFieldsWithheld pins the #899 placeholder: the caller is shown
// the marker and the owner's name, and NOT one field of the payload.
func assertFieldsWithheld(t *testing.T, a openapi.Asset, why string) {
	t.Helper()
	if !a.Restricted {
		t.Errorf("%s: want restricted=true placeholder, got restricted=%v", why, a.Restricted)
	}
	if a.Title != nil && *a.Title == fpTitle {
		t.Errorf("%s: LEAKED the title to a caller the read rule refuses", why)
	}
	if a.Description != nil && *a.Description == fpDescription {
		t.Errorf("%s: LEAKED the description", why)
	}
	for _, tag := range derefTags(a.Tags) {
		if tag == fpTag {
			t.Errorf("%s: LEAKED a tag", why)
		}
	}
	if a.Thumbhash != nil {
		t.Errorf("%s: LEAKED the thumbhash (a thumbhash IS a blur)", why)
	}
}

// assertPictureWithheld pins the half of ADR 0064 that is easiest to
// ship broken: the FIELDS arrived, so the caller passed FieldsReadable,
// and the picture must STILL be absent. Before #939 split the two
// predicates, every one of these rode the single `readable` boolean and
// would have been handed over with the title.
func assertPictureWithheld(t *testing.T, a openapi.Asset, why string) {
	t.Helper()
	if a.Thumbhash != nil {
		t.Errorf("%s: thumbhash was served to a caller refused the bytes — "+
			"ADR 0064 puts the blur on the binary side", why)
	}
	if a.PreviewAvailable != nil && *a.PreviewAvailable {
		t.Errorf("%s: preview_available=true, but the binary handlers refuse this caller — "+
			"that is a 403 the client walks straight into", why)
	}
	if a.LadderAvailable != nil && *a.LadderAvailable {
		t.Errorf("%s: ladder_available=true on gated bytes", why)
	}
	if a.ScrubAvailable != nil && *a.ScrubAvailable {
		t.Errorf("%s: scrub_available=true on gated bytes", why)
	}
}

// assertBytesRefused drives the three BINARY endpoints the decision
// says must not move. They answer 404 rather than 403 by design (#433),
// so this asserts the refusal type explicitly rather than "not 200" —
// a handler that 500s is not a handler that refused.
func assertBytesRefused(t *testing.T, f *maFixture, ctx context.Context, id uuid.UUID, why string) {
	t.Helper()
	fileResp, err := f.h.DownloadAssetFile(ctx, openapi.DownloadAssetFileRequestObject{
		Id: openapi_types.UUID(id),
	})
	if err != nil {
		t.Fatalf("%s: DownloadAssetFile: %v", why, err)
	}
	if _, refused := fileResp.(openapi.DownloadAssetFile404JSONResponse); !refused {
		t.Errorf("%s: /file handed over the ORIGINAL to a field-plane caller (got %T)", why, fileResp)
	}
	varResp, err := f.h.DownloadAssetVariant(ctx, openapi.DownloadAssetVariantRequestObject{
		Id:      openapi_types.UUID(id),
		Variant: "original",
	})
	if err != nil {
		t.Fatalf("%s: DownloadAssetVariant: %v", why, err)
	}
	if _, refused := varResp.(openapi.DownloadAssetVariant404JSONResponse); !refused {
		t.Errorf("%s: /variants/original handed over the bytes (got %T)", why, varResp)
	}
	colResp, err := f.h.DownloadAssetVariant(ctx, openapi.DownloadAssetVariantRequestObject{
		Id:      openapi_types.UUID(id),
		Variant: "col",
	})
	if err != nil {
		t.Fatalf("%s: DownloadAssetVariant(col): %v", why, err)
	}
	if _, refused := colResp.(openapi.DownloadAssetVariant404JSONResponse); !refused {
		t.Errorf("%s: /variants/col handed over a rendition (got %T)", why, colResp)
	}
}

func derefTags(t *[]string) []string {
	if t == nil {
		return nil
	}
	return *t
}

// ---------------------------------------------------------------------------
// ⭐ The decision, both halves, in one test
// ---------------------------------------------------------------------------

// TestFieldPlane_MutationCapabilityConfersFieldsNotBytes is the whole
// of #939 in one assertion set, and it is deliberately not split:
// a build that grants the fields AND the bytes must fail here, and it
// would pass a test that only checked the fields.
//
// It also carries the CLOSURE assertion. The grant is on the PARENT
// team; the asset belongs to a CHILD team, and the caller is a member
// of neither. Nothing but the team_closure expansion performed by the
// auth resolver can connect the two, so a flat hierarchy — which is all
// the seeded database has — could not make this pass.
func TestFieldPlane_MutationCapabilityConfersFieldsNotBytes(t *testing.T) {
	f := newMAFixture(t)

	parent := f.team("fp-parent", nil)
	child := f.team("fp-child", &parent)

	owner := f.user("fp-owner")
	director := f.user("fp-director")

	asset := fpAsset(f, &owner, &child)

	// RED FIRST: before the grant, the director is a stranger to a
	// restricted asset and sees the placeholder. If this half ever
	// stops holding, the test below proves nothing — it would be
	// asserting that a caller who could already read can still read.
	assertFieldsWithheld(t, fpGet(f, f.identity(director), asset),
		"before the grant")

	// The grant: `assets.admin` on the PARENT team only.
	f.grant(director, "assets.admin", &parent)

	ctx := f.identity(director)
	got := fpGet(f, ctx, asset)

	// Half one — the FIELDS arrive.
	if got.Restricted {
		t.Fatalf("still the placeholder: a scoped assets.admin holder must see " +
			"the asset they are permitted to edit (ADR 0064)")
	}
	if got.Title == nil || *got.Title != fpTitle {
		t.Errorf("title: got %v, want %q", got.Title, fpTitle)
	}
	if got.Description == nil || *got.Description != fpDescription {
		t.Errorf("description: got %v, want %q", got.Description, fpDescription)
	}
	var foundTag bool
	for _, tag := range derefTags(got.Tags) {
		if tag == fpTag {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("tags: %v does not carry %q", derefTags(got.Tags), fpTag)
	}

	// Half two — the PICTURE does not.
	assertPictureWithheld(t, got, "team-scoped assets.admin holder")

	// Half three — the BYTES do not.
	assertBytesRefused(t, f, ctx, asset, "team-scoped assets.admin holder")
}

// ---------------------------------------------------------------------------
// Negative controls — each must STILL see the placeholder
// ---------------------------------------------------------------------------

// TestFieldPlane_NoGrantStillWithheld — an ordinary signed-in user is
// unaffected by #939. This is the control that proves the widening is
// the capability's doing and not a hole opened in the tier rule.
func TestFieldPlane_NoGrantStillWithheld(t *testing.T) {
	f := newMAFixture(t)
	parent := f.team("fp-parent", nil)
	child := f.team("fp-child", &parent)
	owner := f.user("fp-owner")
	stranger := f.user("fp-stranger")
	asset := fpAsset(f, &owner, &child)

	assertFieldsWithheld(t, fpGet(f, f.identity(stranger), asset), "no grant at all")
	assertBytesRefused(t, f, f.identity(stranger), asset, "no grant at all")
}

// TestFieldPlane_GrantOnAnotherTeamConfersNothing — the scope is a
// scope. A grant on team B must not reach team A's asset, and the
// closure must not be walked in the wrong direction: the grant here is
// on a SIBLING, so neither an ancestor nor a descendant relationship
// exists to be mistakenly followed.
func TestFieldPlane_GrantOnAnotherTeamConfersNothing(t *testing.T) {
	f := newMAFixture(t)
	root := f.team("fp-root", nil)
	teamA := f.team("fp-a", &root)
	teamB := f.team("fp-b", &root)
	owner := f.user("fp-owner")
	director := f.user("fp-director-b")
	asset := fpAsset(f, &owner, &teamA)

	f.grant(director, "assets.admin", &teamB)

	assertFieldsWithheld(t, fpGet(f, f.identity(director), asset),
		"grant scoped to a SIBLING team")
	assertBytesRefused(t, f, f.identity(director), asset,
		"grant scoped to a SIBLING team")
}

// TestFieldPlane_GrantOnDescendantDoesNotReachAncestor — the closure
// runs one way. A grant on the CHILD confers nothing over an asset
// owned by the PARENT; getting this backwards would silently widen
// every grant into a grant over the whole tree above it.
func TestFieldPlane_GrantOnDescendantDoesNotReachAncestor(t *testing.T) {
	f := newMAFixture(t)
	parent := f.team("fp-parent", nil)
	child := f.team("fp-child", &parent)
	owner := f.user("fp-owner")
	director := f.user("fp-director-child")
	asset := fpAsset(f, &owner, &parent)

	f.grant(director, "assets.admin", &child)

	assertFieldsWithheld(t, fpGet(f, f.identity(director), asset),
		"grant on a DESCENDANT of the asset's team")
}

// TestFieldPlane_RevokeConfersNothing — the auth resolver subtracts
// user_capability_revokes at the exact (user, code, team) tuple,
// NULLs-not-distinct, BEFORE the closure expansion. So a grant that has
// been revoked must confer no field plane.
//
// This is the assertion that would have failed had the capability been
// re-derived in SQL from user_capability_grants alone: the grant row is
// still there, and only the resolver's subtraction removes it.
func TestFieldPlane_RevokeConfersNothing(t *testing.T) {
	f := newMAFixture(t)
	parent := f.team("fp-parent", nil)
	child := f.team("fp-child", &parent)
	owner := f.user("fp-owner")
	director := f.user("fp-director-revoked")
	asset := fpAsset(f, &owner, &child)

	f.grant(director, "assets.admin", &parent)
	// Sanity: the grant works before it is revoked, or the revoke
	// assertion below would be vacuous.
	if fpGet(f, f.identity(director), asset).Restricted {
		t.Fatal("vacuous: the grant did not confer the field plane in the first place")
	}

	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_capability_revokes (user_ref, capability_code, team_id)
		 VALUES ($1, 'assets.admin', $2)`, director, parent,
	); err != nil {
		t.Fatalf("seed revoke: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM user_capability_revokes WHERE user_ref = $1`, director)
	})

	assertFieldsWithheld(t, fpGet(f, f.identity(director), asset), "after the revoke")
	assertBytesRefused(t, f, f.identity(director), asset, "after the revoke")
}

// TestFieldPlane_AnonymousUnaffected — anonymous holds no capability
// and carries the UserRef 0 sentinel, which must never match anything.
func TestFieldPlane_AnonymousUnaffected(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("fp-team", nil)
	owner := f.user("fp-owner")
	asset := fpAsset(f, &owner, &team)

	resp, err := f.h.GetAsset(f.anonCtx(), openapi.GetAssetRequestObject{
		Id: openapi_types.UUID(asset),
	})
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	// Anonymous fails the ROW plane for a restricted asset, so a 404 is
	// the correct answer; a 200 is only acceptable if it is the
	// placeholder.
	if ok, is200 := resp.(openapi.GetAsset200JSONResponse); is200 {
		assertFieldsWithheld(t, openapi.Asset(ok), "anonymous")
	}
	assertBytesRefused(t, f, f.anonCtx(), asset, "anonymous")
}

// TestFieldPlane_GlobalGrantAlsoConfersFieldsNotBytes — the same rule
// through the other door. A GLOBAL `assets.admin` reaches a team-less
// asset (where no scoped grant can apply) and must still be refused the
// bytes: the widening is a property of the CAPABILITY, not of the
// scope it was granted at.
func TestFieldPlane_GlobalGrantAlsoConfersFieldsNotBytes(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("fp-owner")
	director := f.user("fp-director-global")
	// No team at all — the trap that must not fall back to "no scope
	// required, therefore anyone passes".
	asset := fpAsset(f, &owner, nil)

	assertFieldsWithheld(t, fpGet(f, f.identity(director), asset), "before the global grant")

	f.grant(director, "assets.admin", nil)
	ctx := f.identity(director)
	got := fpGet(f, ctx, asset)

	if got.Restricted || got.Title == nil || *got.Title != fpTitle {
		t.Fatalf("global assets.admin did not confer the field plane: restricted=%v title=%v",
			got.Restricted, got.Title)
	}
	assertPictureWithheld(t, got, "global assets.admin holder")
	assertBytesRefused(t, f, ctx, asset, "global assets.admin holder")
}

// TestFieldPlane_ScopedHolderGetsNothingFromATeamlessAsset — the third
// trap on maFixture.asset's list, seen from the read side. A
// team-scoped holder and an asset with no team have no scope in common,
// and MayMutate must not treat "no team to check" as "no check
// required".
func TestFieldPlane_ScopedHolderGetsNothingFromATeamlessAsset(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("fp-team", nil)
	owner := f.user("fp-owner")
	director := f.user("fp-director-scoped")
	asset := fpAsset(f, &owner, nil)

	f.grant(director, "assets.admin", &team)

	assertFieldsWithheld(t, fpGet(f, f.identity(director), asset),
		"team-scoped grant against a TEAM-LESS asset")
}
