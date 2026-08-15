// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #938 — delegating publication through `assets.publish`,
// `assets.archive` and `assets.unarchive`.
//
// The three codes were seeded in 00001, granted to a role there, shown
// in the admin capability list, and consulted by NOTHING. Changing an
// asset's `status` was owner-or-system.admin, so publication could not
// be delegated at all: an operator who granted `assets.publish` had
// delegated nothing and had no way to discover that. Every positive
// case in this file returned 403 before the handler change — that is
// the point of the file, and the reason each one asserts the PERSISTED
// status rather than the response code alone. A gate that answers 200
// without writing, or 403 after writing, is indistinguishable from a
// correct one if you only read the status line.
//
// Two properties matter more than the individual cases and each has its
// own section below:
//
//   - The CLOSURE works. A grant on a PARENT team must reach an asset
//     owned by a DESCENDANT team. The seeded database has no team
//     hierarchy at all (11 self-referential team_closure rows), so this
//     file builds its own through team_parents and lets the 00001
//     trigger materialise the closure — a test against a flat hierarchy
//     has not tested the closure.
//
//   - The verbs do NOT leak into each other. `assets.archive` must not
//     publish. `→ active` is the transition that makes an asset
//     anonymously readable (visibility/predicate.go), so a second route
//     into it would silently turn some other verb into a publication
//     right, and the separation #936 drew would be decorative.
//
// It reuses maFixture from mutation_authz_test.go, which loads
// identities through auth.Resolver.LoadIdentity — so scoped
// capabilities arrive closure-expanded FROM THE DATABASE rather than
// from a literal the test wrote, and the role path below is resolved by
// the same recursive walk production uses.
package assets_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// pubTransition drives a status-only PATCH and asserts BOTH the
// response class and the persisted value. `want` is the status the row
// must carry afterwards, which for a refusal is the status it started
// with.
func pubTransition(
	f *maFixture,
	ctx context.Context,
	id uuid.UUID,
	to openapi.AssetUpdateStatus,
	allow bool,
	want string,
	why string,
) {
	f.t.Helper()
	resp := f.setStatus(ctx, id, to)
	if allow && !isUpdate200(resp) {
		f.t.Errorf("%s: want 200, got %T", why, resp)
	}
	if !allow && !isUpdate403(resp) {
		f.t.Errorf("%s: want 403, got %T", why, resp)
	}
	if got := f.status(id); got != want {
		verb := "did not write"
		if !allow {
			verb = "WROTE the row despite refusing"
		}
		f.t.Errorf("%s: %s: status = %q, want %q", why, verb, got, want)
	}
}

// pubRole grants `codes` through a ROLE assigned on `team`, which is
// the path a grants-only capability derivation would silently miss:
// a team-scoped role assignment writes ZERO rows in
// user_capability_grants. The resolver reaches it by walking
// roles.parent_id recursively while carrying user_roles.team_id, and
// these three verbs are granted exactly this way in the baseline.
func pubRole(f *maFixture, userRef int64, team *uuid.UUID, codes ...string) {
	f.t.Helper()
	roleID := uuid.New()
	name := "pub_role_" + roleID.String()[:8]
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO roles (id, name) VALUES ($1, $2)`, roleID, name,
	); err != nil {
		f.t.Fatalf("seed role: %v", err)
	}
	for _, c := range codes {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO role_capabilities (role_id, capability_code) VALUES ($1, $2)`,
			roleID, c,
		); err != nil {
			f.t.Fatalf("seed role_capability %s: %v", c, err)
		}
	}
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_roles (user_ref, role_id, team_id) VALUES ($1, $2, $3)`,
		userRef, roleID, teamArg,
	); err != nil {
		f.t.Fatalf("assign role: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM user_roles WHERE role_id = $1`, roleID)
		_, _ = f.pool.Exec(c, `DELETE FROM role_capabilities WHERE role_id = $1`, roleID)
		_, _ = f.pool.Exec(c, `DELETE FROM roles WHERE id = $1`, roleID)
	})
}

// pubRevoke subtracts a capability at an exact (code, team) pair. The
// resolver applies revokes BEFORE the closure expansion, so this must
// beat a grant on the same pair.
func pubRevoke(f *maFixture, userRef int64, code string, team *uuid.UUID) {
	f.t.Helper()
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_capability_revokes (user_ref, capability_code, team_id) VALUES ($1, $2, $3)`,
		userRef, code, teamArg,
	); err != nil {
		f.t.Fatalf("revoke %s: %v", code, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM user_capability_revokes WHERE user_ref = $1 AND capability_code = $2`,
			userRef, code)
	})
}

// ---------------------------------------------------------------------------
// The red-first case: publication becomes delegable
// ---------------------------------------------------------------------------

// A team-scoped `assets.publish` holder who is neither the owner nor a
// system admin publishes a colleague's draft. This returned 403 before
// #938 — canSetAssetStatus was owner-or-system.admin and no capability
// could reach it.
//
// The holder is given the publication verb ONLY: no `assets.admin`, no
// role, no ownership. That is deliberate. Publication and content
// management are separate rights, and a design where the verb only
// works alongside `assets.admin` would still leave `assets.publish`
// inert on its own — the same "grantable but confers nothing" defect
// this issue exists to close.
func TestAssetPublication_ScopedGrantMayPublish(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &team)

	draft := f.asset(&artist, &team, "draft")
	pubTransition(f, f.identity(lead), draft, "active", true, "active",
		"team-scoped assets.publish holder publishing a colleague's draft")
}

// The same grant conferred through a ROLE assigned on the team. This is
// the resolution path a hand-rolled derivation from
// user_capability_grants would miss entirely and silently — the row
// count in that table for this user is zero — and it is how the
// baseline confers these three verbs.
func TestAssetPublication_TeamScopedRoleMayPublish(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	pubRole(f, lead, &team, assets.CapAssetsPublish)

	var grants int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM user_capability_grants WHERE user_ref = $1`, lead,
	).Scan(&grants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 0 {
		t.Fatalf("fixture broken: role path wrote %d user_capability_grants rows", grants)
	}

	draft := f.asset(&artist, &team, "draft")
	pubTransition(f, f.identity(lead), draft, "active", true, "active",
		"team-scoped ROLE conferring assets.publish")
}

// A GLOBAL holder reaches a team-less asset, which no scoped grant can.
func TestAssetPublication_GlobalGrantReachesTeamlessAsset(t *testing.T) {
	f := newMAFixture(t)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, nil)

	draft := f.asset(&artist, nil, "draft")
	pubTransition(f, f.identity(lead), draft, "active", true, "active",
		"global assets.publish holder on a team-less asset")
}

// ---------------------------------------------------------------------------
// The closure
// ---------------------------------------------------------------------------

// A grant on a PARENT team permits the transition on an asset owned by
// a DESCENDANT team, with the caller a member of NEITHER.
//
// The hierarchy is built here — teams "division" > "studio" > "strike"
// linked through team_parents, with team_closure materialised by the
// 00001 trigger — because the seeded database has none. A grant on the
// grandparent reaching a grandchild's asset also proves the closure is
// transitive rather than one level deep.
func TestAssetPublication_ParentTeamGrantReachesDescendantAsset(t *testing.T) {
	f := newMAFixture(t)
	division := f.team("division", nil)
	studio := f.team("studio", &division)
	strike := f.team("strike", &studio)

	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &division)

	// The caller is a member of no team at all; the grant's scope is
	// what reaches the asset, not membership.
	var memberships int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM team_memberships WHERE user_ref = $1`, lead,
	).Scan(&memberships); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if memberships != 0 {
		t.Fatalf("fixture broken: caller is in %d teams", memberships)
	}

	child := f.asset(&artist, &studio, "draft")
	pubTransition(f, f.identity(lead), child, "active", true, "active",
		"grant on the parent team, asset owned by the child")

	grandchild := f.asset(&artist, &strike, "draft")
	pubTransition(f, f.identity(lead), grandchild, "active", true, "active",
		"grant on the grandparent team, asset owned by the grandchild")
}

// The closure does not run UPWARDS. A grant on a child team confers
// nothing on the parent's assets.
func TestAssetPublication_ChildTeamGrantDoesNotReachParentAsset(t *testing.T) {
	f := newMAFixture(t)
	division := f.team("division", nil)
	studio := f.team("studio", &division)

	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &studio)

	parentAsset := f.asset(&artist, &division, "draft")
	pubTransition(f, f.identity(lead), parentAsset, "active", false, "draft",
		"grant on the child team, asset owned by the parent")
}

// ---------------------------------------------------------------------------
// Verb isolation — the assertion that proves this is gated PER VERB
// ---------------------------------------------------------------------------

// An `assets.archive` holder may archive and may NOT publish.
//
// This is the load-bearing test of the whole change. Collapsing the
// three verbs into one "may set status" check would pass every other
// positive case in this file and fail only here — and `→ active` is
// exactly the transition that makes an asset anonymously readable, so
// the collapse would hand a publication right to everyone trusted only
// to retire content.
func TestAssetPublication_ArchiveHolderMayNotPublish(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	archivist := f.user("archivist")
	f.grant(archivist, assets.CapAssetsArchive, &team)
	ctx := f.identity(archivist)

	// The verb they hold: active → archived, and draft → archived.
	live := f.asset(&artist, &team, "active")
	pubTransition(f, ctx, live, "archived", true, "archived",
		"assets.archive holder retiring a published asset")

	draft := f.asset(&artist, &team, "draft")
	pubTransition(f, ctx, draft, "archived", true, "archived",
		"assets.archive holder archiving a draft")

	// The verb they do NOT hold: anything into active.
	otherDraft := f.asset(&artist, &team, "draft")
	pubTransition(f, ctx, otherDraft, "active", false, "draft",
		"assets.archive holder publishing a draft")

	archived := f.asset(&artist, &team, "archived")
	pubTransition(f, ctx, archived, "active", false, "archived",
		"assets.archive holder un-archiving to active")

	// Nor may they retract a published asset to draft — that is the
	// publish decision pointing the other way.
	live2 := f.asset(&artist, &team, "active")
	pubTransition(f, ctx, live2, "draft", false, "active",
		"assets.archive holder retracting to draft")
}

// A `assets.publish` holder may publish and retract, and may NOT
// archive: the two verbs are independent in both directions.
func TestAssetPublication_PublishHolderMayNotArchive(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &team)
	ctx := f.identity(lead)

	draft := f.asset(&artist, &team, "draft")
	pubTransition(f, ctx, draft, "active", true, "active",
		"assets.publish holder publishing")

	// Retraction — active → draft — is the publish decision reversed.
	pubTransition(f, ctx, draft, "draft", true, "draft",
		"assets.publish holder retracting their publication")

	live := f.asset(&artist, &team, "active")
	pubTransition(f, ctx, live, "archived", false, "active",
		"assets.publish holder archiving")
}

// `assets.unarchive` alone takes an asset OUT of the archive to draft,
// and cannot take it to active — reaching active is a publication and
// needs `assets.publish` as well. That conjunction is what stops
// unarchive from being a second, quieter route into the state the
// anonymous read branch tests for.
func TestAssetPublication_UnarchiveAloneReachesDraftNotActive(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	restorer := f.user("restorer")
	f.grant(restorer, assets.CapAssetsUnarchive, &team)
	ctx := f.identity(restorer)

	toActive := f.asset(&artist, &team, "archived")
	pubTransition(f, ctx, toActive, "active", false, "archived",
		"assets.unarchive holder restoring straight to active")

	toDraft := f.asset(&artist, &team, "archived")
	pubTransition(f, ctx, toDraft, "draft", true, "draft",
		"assets.unarchive holder restoring to draft")
}

// Holding BOTH publish and unarchive completes archived → active. Also
// pins the other direction of the conjunction: publish ALONE cannot do
// it either, so neither verb is redundant.
func TestAssetPublication_ArchivedToActiveNeedsBothVerbs(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")

	publisher := f.user("publisher")
	f.grant(publisher, assets.CapAssetsPublish, &team)
	onlyPublish := f.asset(&artist, &team, "archived")
	pubTransition(f, f.identity(publisher), onlyPublish, "active", false, "archived",
		"assets.publish alone on archived -> active")

	both := f.user("both")
	f.grant(both, assets.CapAssetsPublish, &team)
	f.grant(both, assets.CapAssetsUnarchive, &team)
	asset := f.asset(&artist, &team, "archived")
	pubTransition(f, f.identity(both), asset, "active", true, "active",
		"assets.publish + assets.unarchive on archived -> active")
}

// ---------------------------------------------------------------------------
// The other boundary: publication is not content management
// ---------------------------------------------------------------------------

// A publication verb confers NO power to rewrite the asset. The two
// planes are gated separately in UpdateAsset and neither implies the
// other; #936's boundary is symmetric.
func TestAssetPublication_PublishHolderMayNotEditOrDelete(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &team)
	ctx := f.identity(lead)

	a := f.asset(&artist, &team, "draft")
	f.refusedUpdate(ctx, a, "assets.publish holder editing a title")
	f.refusedDelete(ctx, a, "assets.publish holder deleting")
}

// A PATCH that carries a title ALONGSIDE a permitted transition is
// refused as a whole, and writes NEITHER. Gating the two planes
// separately must not let a publication holder smuggle a content edit
// through in the same request.
func TestAssetPublication_StatusPlusTitleIsRefusedEntirely(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &team)

	a := f.asset(&artist, &team, "draft")
	beforeTitle := f.title(a)

	title := "pub-smuggled-title"
	status := openapi.AssetUpdateStatus("active")
	resp, err := f.h.UpdateAsset(f.identity(lead), openapi.UpdateAssetRequestObject{
		Id:   openapi_types.UUID(a),
		Body: &openapi.AssetUpdate{Title: &title, Status: &status},
	})
	if err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if !isUpdate403(resp) {
		t.Errorf("title + status from a publication-only holder: want 403, got %T", resp)
	}
	if got := f.title(a); got != beforeTitle {
		t.Errorf("refused combined PATCH still wrote the title: %q -> %q", beforeTitle, got)
	}
	if got := f.status(a); got != "draft" {
		t.Errorf("refused combined PATCH still wrote the status: %q", got)
	}
}

// An `assets.admin` holder — content management — still may not
// publish. This is the #936 boundary, re-asserted from this side: the
// three verbs did not quietly get folded into `assets.admin` while
// being wired.
func TestAssetPublication_AssetsAdminAloneStillMayNotPublish(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	director := f.user("director")
	f.grant(director, assets.CapAssetsAdmin, &team)
	ctx := f.identity(director)

	draft := f.asset(&artist, &team, "draft")
	pubTransition(f, ctx, draft, "active", false, "draft",
		"assets.admin holder publishing a colleague's draft")

	// But the content plane is untouched: they may still edit.
	f.allowedUpdate(ctx, draft, "assets.admin holder")
}

// ---------------------------------------------------------------------------
// Negative controls that must still fail
// ---------------------------------------------------------------------------

// No grant at all.
func TestAssetPublication_NoGrantIsRefused(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	stranger := f.user("stranger")

	draft := f.asset(&artist, &team, "draft")
	pubTransition(f, f.identity(stranger), draft, "active", false, "draft",
		"authenticated caller with no capability")
}

// A grant scoped to team A does nothing on team B's asset. The two
// teams are unrelated — no closure edge in either direction.
func TestAssetPublication_GrantOnAnotherTeamIsRefused(t *testing.T) {
	f := newMAFixture(t)
	teamA := f.team("alpha", nil)
	teamB := f.team("bravo", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &teamA)

	draft := f.asset(&artist, &teamB, "draft")
	pubTransition(f, f.identity(lead), draft, "active", false, "draft",
		"grant scoped to team A, asset owned by team B")
}

// A team-scoped grant confers nothing on a TEAM-LESS asset. The scoped
// disjunct is skipped rather than treated as "no scope required,
// therefore anyone passes" — the same trap canMutateAsset documents.
func TestAssetPublication_ScopedGrantDoesNotReachTeamlessAsset(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &team)

	draft := f.asset(&artist, nil, "draft")
	pubTransition(f, f.identity(lead), draft, "active", false, "draft",
		"team-scoped grant vs a team-less asset")
}

// A REVOKED grant confers nothing. The resolver subtracts
// user_capability_revokes at the exact (code, team) pair before the
// closure expansion, so the revoke must beat the grant it shadows.
func TestAssetPublication_RevokedGrantConfersNothing(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	lead := f.user("lead")
	f.grant(lead, assets.CapAssetsPublish, &team)
	pubRevoke(f, lead, assets.CapAssetsPublish, &team)

	draft := f.asset(&artist, &team, "draft")
	pubTransition(f, f.identity(lead), draft, "active", false, "draft",
		"grant shadowed by a revoke at the same (code, team)")
}

// A NULL owner matches nobody. Only system.admin reaches an ownerless
// asset, and a publication grant does not make its holder the owner.
func TestAssetPublication_NullOwnerMatchesNobody(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	lead := f.user("lead")

	orphan := f.asset(nil, &team, "draft")
	// The grant still reaches it — the asset has a team — so this
	// asserts the grant path, not a nil-owner refusal.
	pubTransition(f, f.identity(lead), orphan, "active", false, "draft",
		"no capability, NULL-owner asset")

	f.grant(lead, assets.CapAssetsPublish, &team)
	pubTransition(f, f.identity(lead), orphan, "active", true, "active",
		"team-scoped assets.publish on a NULL-owner asset in that team")
}

// Anonymous is unchanged: refused before the gate is ever consulted.
func TestAssetPublication_AnonymousIsRefused(t *testing.T) {
	f := newMAFixture(t)
	artist := f.user("artist")
	draft := f.asset(&artist, nil, "draft")

	status := openapi.AssetUpdateStatus("active")
	resp, err := f.h.UpdateAsset(f.anonCtx(), openapi.UpdateAssetRequestObject{
		Id:   openapi_types.UUID(draft),
		Body: &openapi.AssetUpdate{Status: &status},
	})
	if err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if _, ok := resp.(openapi.UpdateAsset401JSONResponse); !ok {
		t.Errorf("anonymous status change: want 401, got %T", resp)
	}
	if got := f.status(draft); got != "draft" {
		t.Errorf("anonymous status change WROTE the row: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Preserved behaviour
// ---------------------------------------------------------------------------

// The owner keeps every transition, with no capability at all.
func TestAssetPublication_OwnerKeepsEveryTransition(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	ctx := f.identity(owner)

	a := f.asset(&owner, nil, "draft")
	pubTransition(f, ctx, a, "active", true, "active", "owner draft -> active")
	pubTransition(f, ctx, a, "draft", true, "draft", "owner active -> draft")
	pubTransition(f, ctx, a, "archived", true, "archived", "owner draft -> archived")
	pubTransition(f, ctx, a, "active", true, "active", "owner archived -> active")
	pubTransition(f, ctx, a, "archived", true, "archived", "owner active -> archived")
	pubTransition(f, ctx, a, "draft", true, "draft", "owner archived -> draft")
}

// system.admin keeps every transition, including on a team-less,
// NULL-owner asset.
func TestAssetPublication_SystemAdminKeepsEveryTransition(t *testing.T) {
	f := newMAFixture(t)
	admin := f.user("admin")
	f.grant(admin, "system.admin", nil)
	ctx := f.identity(admin)

	orphan := f.asset(nil, nil, "draft")
	pubTransition(f, ctx, orphan, "active", true, "active", "system.admin publishing an orphan")
	pubTransition(f, ctx, orphan, "archived", true, "archived", "system.admin archiving an orphan")
	pubTransition(f, ctx, orphan, "active", true, "active", "system.admin un-archiving an orphan")
}

// A PATCH that echoes the CURRENT status back is not a transition, and
// is not refused. The gate compares against the stored value, so a
// client that round-trips the whole asset object does not need the
// publication verb to save an unrelated edit.
func TestAssetPublication_EchoingCurrentStatusIsNotATransition(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	director := f.user("director")
	// assets.admin ONLY — no publication verb anywhere.
	f.grant(director, assets.CapAssetsAdmin, &team)
	ctx := f.identity(director)

	live := f.asset(&artist, &team, "active")
	pubTransition(f, ctx, live, "active", true, "active",
		"assets.admin holder echoing the current status")

	// And the same echo alongside a real content edit.
	title := "pub-echo-with-edit"
	status := openapi.AssetUpdateStatus("active")
	resp, err := f.h.UpdateAsset(ctx, openapi.UpdateAssetRequestObject{
		Id:   openapi_types.UUID(live),
		Body: &openapi.AssetUpdate{Title: &title, Status: &status},
	})
	if err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if !isUpdate200(resp) {
		t.Errorf("echo + title edit from an assets.admin holder: want 200, got %T", resp)
	}
	if got := f.title(live); got != title {
		t.Errorf("edit alongside a status echo did not write: title = %q", got)
	}
}

// The 403 names the capability the caller is missing. An operator
// reading "forbidden" has no way to know which of three verbs to grant,
// and the codes are already visible to them in the admin capability
// list.
func TestAssetPublication_RefusalNamesTheCapability(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	artist := f.user("artist")
	stranger := f.user("stranger")
	// Give them assets.admin so the refusal comes from the publication
	// gate rather than the content gate.
	f.grant(stranger, assets.CapAssetsAdmin, &team)

	archived := f.asset(&artist, &team, "archived")
	resp := f.setStatus(f.identity(stranger), archived, "active")
	forbidden, ok := resp.(openapi.UpdateAsset403JSONResponse)
	if !ok {
		t.Fatalf("want 403, got %T", resp)
	}
	msg := forbidden.Error
	for _, want := range []string{assets.CapAssetsPublish, assets.CapAssetsUnarchive} {
		if !strings.Contains(msg, want) {
			t.Errorf("403 message %q does not name %q", msg, want)
		}
	}
}
