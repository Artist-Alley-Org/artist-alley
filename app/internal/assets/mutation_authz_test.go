// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Authorisation tests for the asset mutation surface (#930) and for
// who may undo a soft delete (#931).
//
// Before this, UpdateAsset and DeleteAsset checked only that the caller
// was authenticated. Every "must be 403" case here PASSED with 200/204
// against the previous handler — that is the point of the file, and the
// reason each of them asserts the STATE as well as the status. A gate
// that answers 403 after writing the row is a gate that does not exist,
// and a status-only assertion cannot tell the two apart.
package assets_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// maFixture is one isolated world per test: a handler, a pool, and
// helpers that seed users, teams, grants and assets with cleanup
// registered. Everything it creates is namespaced by a per-test UUID so
// parallel packages sharing the database can't collide.
type maFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    *assets.Handler
	res  *auth.Resolver
	ctx  context.Context
}

func newMAFixture(t *testing.T) *maFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := storage.NewService(backend, pool)
	h := assets.NewHandler(pool, svc, logger, nil, nil, nil)
	// Restore runs through the softdelete service; without it
	// RestoreAsset returns an "unwired" error rather than exercising
	// the gate this file is about.
	h.SoftDelete = softdelete.NewService(pool, audit.NewRecorder(pool, logger))
	return &maFixture{
		t:    t,
		pool: pool,
		h:    h,
		res:  &auth.Resolver{Pool: pool, Logger: logger},
		ctx:  context.Background(),
	}
}

// user seeds a real row in "user" and returns its ref. A real row is
// required because user_capability_grants.user_ref is FK-constrained.
func (f *maFixture) user(label string) int64 {
	f.t.Helper()
	var ref int64
	name := "ma-" + label + "-" + uuid.NewString()[:8]
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		name,
	).Scan(&ref); err != nil {
		f.t.Fatalf("seed user %q: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// team seeds a team. Passing a parent inserts the team_parents edge, and
// the 00015 trigger materialises team_closure from it — which is what
// makes the descendant case in TestAssetMutation_ScopedGrant real rather
// than an assertion about a map the test built.
func (f *maFixture) team(label string, parent *uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	slug := "ma_team_" + id.String()[:8] + "_" + label
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`,
		id, slug, label,
	); err != nil {
		f.t.Fatalf("seed team %q: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	if parent != nil {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO team_parents (parent_id, child_id) VALUES ($1, $2)`,
			*parent, id,
		); err != nil {
			f.t.Fatalf("link team %q: %v", label, err)
		}
	}
	return id
}

// grant gives userRef a capability, globally when team is nil.
func (f *maFixture) grant(userRef int64, code string, team *uuid.UUID) {
	f.t.Helper()
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_capability_grants (user_ref, capability_code, team_id) VALUES ($1, $2, $3)`,
		userRef, code, teamArg,
	); err != nil {
		f.t.Fatalf("grant %s to %d: %v", code, userRef, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = $2`,
			userRef, code)
	})
}

// asset seeds an asset. owner may be nil (assets.owner_user_ref is
// NULLABLE — trap 1), team may be nil (assets.team_id is NULLABLE —
// trap 3).
func (f *maFixture) asset(owner *int64, team *uuid.UUID, status string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	// Random hash: the 00016 per-user dedup unique index rejects two
	// assets from the same owner over identical bytes.
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
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO assets (id, title, asset_type, owner_user_ref, team_id, status,
		                    file_hash, file_extension, file_size_bytes, sensitivity)
		VALUES ($1, 'ma-original', 1, $2, $3, $4, $5, 'png', 1024, 'public')`,
		id, owner, teamArg, status, hashHex,
	); err != nil {
		f.t.Fatalf("seed asset: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hashHex)
	})
	return id
}

// identity loads the Identity the middleware would produce, so scoped
// grants arrive closure-expanded from the database rather than from a
// literal this test wrote.
func (f *maFixture) identity(userRef int64) context.Context {
	f.t.Helper()
	return auth.WithIdentity(f.ctx, f.res.LoadIdentity(f.ctx, userRef))
}

func (f *maFixture) anonCtx() context.Context {
	return auth.WithIdentity(f.ctx, &auth.Identity{UserRef: 0, AuthMethod: "anonymous"})
}

// ---------------------------------------------------------------------------
// Assertions — every refusal check asserts STATE, not just status
// ---------------------------------------------------------------------------

func (f *maFixture) title(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT title FROM assets WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("read title: %v", err)
	}
	return s
}

func (f *maFixture) status(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT status FROM assets WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("read status: %v", err)
	}
	return s
}

func (f *maFixture) isDeleted(id uuid.UUID) bool {
	f.t.Helper()
	var deleted bool
	if err := f.pool.QueryRow(f.ctx,
		`SELECT deleted_at IS NOT NULL FROM assets WHERE id = $1`, id,
	).Scan(&deleted); err != nil {
		f.t.Fatalf("read deleted_at: %v", err)
	}
	return deleted
}

func (f *maFixture) deletedBy(id uuid.UUID) *int64 {
	f.t.Helper()
	var ref *int64
	if err := f.pool.QueryRow(f.ctx,
		`SELECT deleted_by_user_ref FROM assets WHERE id = $1`, id,
	).Scan(&ref); err != nil {
		f.t.Fatalf("read deleted_by_user_ref: %v", err)
	}
	return ref
}

// update runs a title-only PATCH and reports the response.
func (f *maFixture) update(ctx context.Context, id uuid.UUID, title string) openapi.UpdateAssetResponseObject {
	f.t.Helper()
	resp, err := f.h.UpdateAsset(ctx, openapi.UpdateAssetRequestObject{
		Id:   openapi_types.UUID(id),
		Body: &openapi.AssetUpdate{Title: &title},
	})
	if err != nil {
		f.t.Fatalf("UpdateAsset: %v", err)
	}
	return resp
}

func (f *maFixture) setStatus(ctx context.Context, id uuid.UUID, status openapi.AssetUpdateStatus) openapi.UpdateAssetResponseObject {
	f.t.Helper()
	resp, err := f.h.UpdateAsset(ctx, openapi.UpdateAssetRequestObject{
		Id:   openapi_types.UUID(id),
		Body: &openapi.AssetUpdate{Status: &status},
	})
	if err != nil {
		f.t.Fatalf("UpdateAsset(status): %v", err)
	}
	return resp
}

func (f *maFixture) del(ctx context.Context, id uuid.UUID) openapi.DeleteAssetResponseObject {
	f.t.Helper()
	resp, err := f.h.DeleteAsset(ctx, openapi.DeleteAssetRequestObject{Id: openapi_types.UUID(id)})
	if err != nil {
		f.t.Fatalf("DeleteAsset: %v", err)
	}
	return resp
}

func (f *maFixture) restore(ctx context.Context, id uuid.UUID) openapi.RestoreAssetResponseObject {
	f.t.Helper()
	resp, err := f.h.RestoreAsset(ctx, openapi.RestoreAssetRequestObject{Id: openapi_types.UUID(id)})
	if err != nil {
		f.t.Fatalf("RestoreAsset: %v", err)
	}
	return resp
}

// refusedUpdate asserts 403 AND that the row is untouched.
func (f *maFixture) refusedUpdate(ctx context.Context, id uuid.UUID, why string) {
	f.t.Helper()
	before := f.title(id)
	resp := f.update(ctx, id, "ma-rewritten-by-"+why)
	if _, ok := resp.(openapi.UpdateAsset403JSONResponse); !ok {
		f.t.Errorf("%s: want 403 from UpdateAsset, got %T", why, resp)
	}
	if after := f.title(id); after != before {
		f.t.Errorf("%s: refused update still WROTE the row: title %q -> %q", why, before, after)
	}
}

// refusedDelete asserts 403 AND that deleted_at is still NULL.
func (f *maFixture) refusedDelete(ctx context.Context, id uuid.UUID, why string) {
	f.t.Helper()
	resp := f.del(ctx, id)
	if _, ok := resp.(openapi.DeleteAsset403JSONResponse); !ok {
		f.t.Errorf("%s: want 403 from DeleteAsset, got %T", why, resp)
	}
	if f.isDeleted(id) {
		f.t.Errorf("%s: refused delete still SOFT-DELETED the row", why)
	}
}

func (f *maFixture) allowedUpdate(ctx context.Context, id uuid.UUID, who string) {
	f.t.Helper()
	want := "ma-edited-by-" + who
	resp := f.update(ctx, id, want)
	if _, ok := resp.(openapi.UpdateAsset200JSONResponse); !ok {
		f.t.Fatalf("%s: want 200 from UpdateAsset, got %T", who, resp)
	}
	if got := f.title(id); got != want {
		f.t.Errorf("%s: update returned 200 but did not write: title = %q", who, got)
	}
}

func (f *maFixture) allowedDelete(ctx context.Context, id uuid.UUID, who string) {
	f.t.Helper()
	resp := f.del(ctx, id)
	if _, ok := resp.(openapi.DeleteAsset204Response); !ok {
		f.t.Fatalf("%s: want 204 from DeleteAsset, got %T", who, resp)
	}
	if !f.isDeleted(id) {
		f.t.Errorf("%s: delete returned 204 but deleted_at is still NULL", who)
	}
}

// ---------------------------------------------------------------------------
// #930 — the hole itself
// ---------------------------------------------------------------------------

// A plain authenticated user who neither owns the asset nor holds any
// grant is the exact caller who could rewrite and delete the whole
// instance before this change. Both assertions here were 200 / 204 on
// the previous handler.
func TestAssetMutation_StrangerIsRefused(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	stranger := f.user("stranger")
	id := f.asset(&owner, nil, "active")

	f.refusedUpdate(f.identity(stranger), id, "stranger")
	f.refusedDelete(f.identity(stranger), id, "stranger")
}

// The owner's own asset stays fully theirs — the fix must not be a
// lockout.
func TestAssetMutation_OwnerMayEditAndDelete(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	id := f.asset(&owner, nil, "active")

	ctx := f.identity(owner)
	f.allowedUpdate(ctx, id, "owner")
	f.allowedDelete(ctx, id, "owner")
}

// The owner's requirement in as many words: "Members shouldn't be able
// to change other member['s work]". Sharing a team is not authority
// over a colleague's file; the capability is.
func TestAssetMutation_PlainTeamMemberMayNotManageColleaguesAsset(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	owner := f.user("owner")
	colleague := f.user("colleague")
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2), ($1, $3)`,
		team, owner, colleague,
	); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}
	id := f.asset(&owner, &team, "active")

	f.refusedUpdate(f.identity(colleague), id, "plain team member")
	f.refusedDelete(f.identity(colleague), id, "plain team member")
}

// The feature half: an art director scoped to a team manages that
// team's files — and only that team's.
//
// The DESCENDANT case is what proves the closure expansion is really
// being consulted. A gate that compared assets.team_id to the granted
// team_id for equality would pass the "own team" case and fail here.
func TestAssetMutation_ScopedGrantReachesTeamAndDescendantsOnly(t *testing.T) {
	f := newMAFixture(t)
	parent := f.team("division", nil)
	child := f.team("squad", &parent)
	unrelated := f.team("other-division", nil)

	director := f.user("director")
	f.grant(director, assets.CapAssetsAdmin, &parent)

	artist := f.user("artist")
	inParent := f.asset(&artist, &parent, "active")
	inChild := f.asset(&artist, &child, "active")
	elsewhere := f.asset(&artist, &unrelated, "active")

	ctx := f.identity(director)
	f.allowedUpdate(ctx, inParent, "director-in-scope")
	f.allowedUpdate(ctx, inChild, "director-in-descendant")
	f.refusedUpdate(ctx, elsewhere, "director-out-of-scope")

	f.allowedDelete(ctx, inChild, "director-in-descendant")
	f.refusedDelete(ctx, elsewhere, "director-out-of-scope")
}

// A global grant is unscoped and reaches everything, including assets
// with no team at all.
func TestAssetMutation_GlobalGrantReachesEverything(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	moderator := f.user("moderator")
	f.grant(moderator, assets.CapAssetsAdmin, nil)
	artist := f.user("artist")

	ctx := f.identity(moderator)
	f.allowedUpdate(ctx, f.asset(&artist, &team, "active"), "global-grant-teamed")
	f.allowedUpdate(ctx, f.asset(&artist, nil, "active"), "global-grant-teamless")
}

// system.admin is the global override on every path in this file.
func TestAssetMutation_SystemAdminOverridesEverywhere(t *testing.T) {
	f := newMAFixture(t)
	admin := f.user("admin")
	f.grant(admin, auth.SuperAdminCapability, nil)
	artist := f.user("artist")
	team := f.team("studio", nil)

	ctx := f.identity(admin)
	f.allowedUpdate(ctx, f.asset(&artist, &team, "active"), "system-admin")
	f.allowedUpdate(ctx, f.asset(nil, nil, "active"), "system-admin-null-owner")
	f.allowedDelete(ctx, f.asset(&artist, nil, "active"), "system-admin")

	// And the publication lever, which assets.admin does not confer.
	pub := f.asset(&artist, &team, "draft")
	if resp := f.setStatus(ctx, pub, "active"); !isUpdate200(resp) {
		t.Errorf("system.admin should be able to publish; got %T", resp)
	}
	if got := f.status(pub); got != "active" {
		t.Errorf("system.admin publish did not write: status = %q", got)
	}
}

// ---------------------------------------------------------------------------
// The three data-model traps
// ---------------------------------------------------------------------------

// Trap 1 — assets.owner_user_ref is NULLABLE. A NULL owner must match
// NOBODY. The failure modes are a nil dereference (panic) and treating
// "unowned" as "unclaimed, therefore anyone's".
func TestAssetMutation_NullOwnerIsManageableByGlobalAdminOnly(t *testing.T) {
	f := newMAFixture(t)
	id := f.asset(nil, nil, "active")

	somebody := f.user("somebody")
	f.refusedUpdate(f.identity(somebody), id, "null-owner vs stranger")
	f.refusedDelete(f.identity(somebody), id, "null-owner vs stranger")

	// An anonymous caller is stopped one step earlier, at the 401
	// guard, and must likewise leave the row alone. NULL owner + ref-0
	// caller is the pair that a naive `*ownerRef == id.UserRef` would
	// have had to get wrong twice over.
	if resp := f.update(f.anonCtx(), id, "ma-anon-null-owner"); !isUpdate401(resp) {
		t.Errorf("anonymous vs null-owner asset: want 401, got %T", resp)
	}
	if got := f.title(id); got != "ma-original" {
		t.Errorf("anonymous caller wrote a null-owner asset: title = %q", got)
	}

	admin := f.user("admin")
	f.grant(admin, auth.SuperAdminCapability, nil)
	f.allowedUpdate(f.identity(admin), id, "null-owner vs system-admin")
}

// Trap 2 — the anonymous sentinel carries UserRef 0. An asset OWNED by
// ref 0 must not hand ownership to every anonymous visitor, which is
// exactly what a bare `*ownerRef == id.UserRef` would do.
func TestAssetMutation_AnonymousIsRefusedEverywhere(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	normal := f.asset(&owner, nil, "active")

	anon := f.anonCtx()
	if resp := f.update(anon, normal, "ma-anon"); !isUpdate401(resp) {
		t.Errorf("anonymous update: want 401, got %T", resp)
	}
	if got := f.title(normal); got != "ma-original" {
		t.Errorf("anonymous update wrote the row: title = %q", got)
	}
	if resp := f.del(anon, normal); !isDelete401(resp) {
		t.Errorf("anonymous delete: want 401, got %T", resp)
	}
	if f.isDeleted(normal) {
		t.Error("anonymous delete soft-deleted the row")
	}

	// The sentinel-collision case: an asset whose owner column IS 0.
	// The read path documents this hazard (visibility/content.go); the
	// mutation path has to refuse it too.
	var zero int64
	sentinelOwned := f.asset(&zero, nil, "active")
	if resp := f.update(anon, sentinelOwned, "ma-anon-owns-it"); !isUpdate401(resp) {
		t.Errorf("anonymous vs ref-0-owned asset: want 401, got %T", resp)
	}
	if got := f.title(sentinelOwned); got != "ma-original" {
		t.Errorf("anonymous caller was handed ownership of a ref-0 asset: title = %q", got)
	}
}

// Trap 3 — assets.team_id is NULLABLE. A team-less asset has no scope
// for InTeam to check, and must fall back to owner-or-global rather
// than to "no scope required, so anyone passes".
func TestAssetMutation_TeamlessAssetIsNotWritableByScopedGrant(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	director := f.user("director")
	f.grant(director, assets.CapAssetsAdmin, &team)

	artist := f.user("artist")
	teamless := f.asset(&artist, nil, "active")

	ctx := f.identity(director)
	f.refusedUpdate(ctx, teamless, "team-scoped grant vs team-less asset")
	f.refusedDelete(ctx, teamless, "team-scoped grant vs team-less asset")
}

// ---------------------------------------------------------------------------
// The escalation boundary
// ---------------------------------------------------------------------------

// assets.admin is content management, not a disclosure decision.
// `status` decides whether an anonymous reader can see the row at all
// (visibility/predicate.go demands status='active'), so a grant holder
// who could flip a colleague's draft to active could publish their
// unfinished work without ever being given a publication capability.
func TestAssetMutation_GrantHolderMayNotChangePublicationStatus(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	director := f.user("director")
	f.grant(director, assets.CapAssetsAdmin, &team)
	artist := f.user("artist")
	draft := f.asset(&artist, &team, "draft")

	ctx := f.identity(director)

	// The grant DOES reach this asset for ordinary edits...
	f.allowedUpdate(ctx, draft, "director")

	// ...and does NOT reach its publication status.
	resp := f.setStatus(ctx, draft, "active")
	if _, ok := resp.(openapi.UpdateAsset403JSONResponse); !ok {
		t.Errorf("grant holder publishing a colleague's draft: want 403, got %T", resp)
	}
	if got := f.status(draft); got != "draft" {
		t.Errorf("refused publish still WROTE the row: status = %q", got)
	}

	// Retraction is the same lever pointing the other way.
	live := f.asset(&artist, &team, "active")
	if resp := f.setStatus(ctx, live, "draft"); !isUpdate403(resp) {
		t.Errorf("grant holder retracting a colleague's asset: want 403, got %T", resp)
	}
	if got := f.status(live); got != "active" {
		t.Errorf("refused retraction still WROTE the row: status = %q", got)
	}

	// A no-op that merely echoes the current status is not a CHANGE and
	// is not refused — the boundary is about changing reachability.
	if resp := f.setStatus(ctx, live, "active"); !isUpdate200(resp) {
		t.Errorf("echoing the current status should pass; got %T", resp)
	}
}

// The owner keeps their own publication lever.
func TestAssetMutation_OwnerMayChangeOwnPublicationStatus(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	draft := f.asset(&owner, nil, "draft")

	if resp := f.setStatus(f.identity(owner), draft, "active"); !isUpdate200(resp) {
		t.Fatalf("owner publishing their own asset: want 200, got %T", resp)
	}
	if got := f.status(draft); got != "active" {
		t.Errorf("owner publish did not write: status = %q", got)
	}
}

// ---------------------------------------------------------------------------
// #931 — who may undo a delete
// ---------------------------------------------------------------------------

// Every soft-delete through the handler records its actor. Without
// this column the restore rule has nothing to decide on.
func TestAssetDelete_RecordsDeletedBy(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	id := f.asset(&owner, nil, "active")

	f.allowedDelete(f.identity(owner), id, "owner")
	got := f.deletedBy(id)
	if got == nil || *got != owner {
		t.Fatalf("deleted_by_user_ref = %v, want %d", got, owner)
	}
}

// The self-service half: you deleted it, you get it back. Before this,
// RestoreAsset was system.admin only, so nothing could be self-restored
// at all.
func TestAssetRestore_DeleterMayUndoTheirOwnDelete(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	id := f.asset(&owner, nil, "active")
	ctx := f.identity(owner)

	f.allowedDelete(ctx, id, "owner")
	if resp := f.restore(ctx, id); !isRestore204(resp) {
		t.Fatalf("owner restoring their own delete: want 204, got %T", resp)
	}
	if f.isDeleted(id) {
		t.Error("restore returned 204 but deleted_at is still set")
	}
}

// The other half of the owner's rule: "unless deleted by an admin. Then
// they would need to request for restoration." An owner must not be
// able to silently reverse a moderation action.
func TestAssetRestore_OwnerMayNotUndoAnAdminsDelete(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	admin := f.user("admin")
	f.grant(admin, auth.SuperAdminCapability, nil)
	id := f.asset(&owner, nil, "active")

	f.allowedDelete(f.identity(admin), id, "admin")

	resp := f.restore(f.identity(owner), id)
	if _, ok := resp.(openapi.RestoreAsset403JSONResponse); !ok {
		t.Errorf("owner undoing an admin's delete: want 403, got %T", resp)
	}
	if !f.isDeleted(id) {
		t.Error("refused restore still CLEARED deleted_at")
	}
	// The admin who did it can still undo it.
	if resp := f.restore(f.identity(admin), id); !isRestore204(resp) {
		t.Errorf("admin undoing their own delete: want 204, got %T", resp)
	}
}

// Restore authority must match delete authority, or this sprint just
// moves #931's asymmetry up one level: a team admin who can delete
// under a scoped grant must be able to undo it.
func TestAssetRestore_TeamAdminMayUndoTheirOwnDelete(t *testing.T) {
	f := newMAFixture(t)
	parent := f.team("division", nil)
	child := f.team("squad", &parent)
	director := f.user("director")
	f.grant(director, assets.CapAssetsAdmin, &parent)
	artist := f.user("artist")
	id := f.asset(&artist, &child, "active")

	ctx := f.identity(director)
	f.allowedDelete(ctx, id, "team-admin")
	if got := f.deletedBy(id); got == nil || *got != director {
		t.Fatalf("deleted_by_user_ref = %v, want %d", got, director)
	}
	if resp := f.restore(ctx, id); !isRestore204(resp) {
		t.Fatalf("team admin undoing their own delete: want 204, got %T", resp)
	}
	if f.isDeleted(id) {
		t.Error("restore returned 204 but deleted_at is still set")
	}
}

// A row whose deleter is unknown — deleted before migration 00037, or
// removed by a system-scheduled retention action whose created_by is
// NULL — falls back to system.admin. Fail closed.
func TestAssetRestore_NullDeletedByIsSystemAdminOnly(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("owner")
	id := f.asset(&owner, nil, "active")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE assets SET deleted_at = NOW(), deleted_by_user_ref = NULL WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("seed legacy delete: %v", err)
	}

	if resp := f.restore(f.identity(owner), id); !isRestore403(resp) {
		t.Errorf("owner restoring a NULL-deleter row: want 403, got %T", resp)
	}
	if !f.isDeleted(id) {
		t.Error("refused restore still CLEARED deleted_at")
	}

	admin := f.user("admin")
	f.grant(admin, auth.SuperAdminCapability, nil)
	if resp := f.restore(f.identity(admin), id); !isRestore204(resp) {
		t.Errorf("system.admin restoring a NULL-deleter row: want 204, got %T", resp)
	}
}

// An unrelated grant holder did not delete it, so they do not get to
// undo it — restore keys on the deleter, not on standing authority.
func TestAssetRestore_UnrelatedGrantHolderIsRefused(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("studio", nil)
	owner := f.user("owner")
	directorA := f.user("director-a")
	directorB := f.user("director-b")
	f.grant(directorA, assets.CapAssetsAdmin, &team)
	f.grant(directorB, assets.CapAssetsAdmin, &team)
	id := f.asset(&owner, &team, "active")

	f.allowedDelete(f.identity(directorA), id, "director-a")
	if resp := f.restore(f.identity(directorB), id); !isRestore403(resp) {
		t.Errorf("a different grant holder undoing A's delete: want 403, got %T", resp)
	}
	if !f.isDeleted(id) {
		t.Error("refused restore still CLEARED deleted_at")
	}
}

// ---------------------------------------------------------------------------
// Response-type predicates (keeps the assertions above readable)
// ---------------------------------------------------------------------------

func isUpdate200(r openapi.UpdateAssetResponseObject) bool {
	_, ok := r.(openapi.UpdateAsset200JSONResponse)
	return ok
}

func isUpdate401(r openapi.UpdateAssetResponseObject) bool {
	_, ok := r.(openapi.UpdateAsset401JSONResponse)
	return ok
}

func isUpdate403(r openapi.UpdateAssetResponseObject) bool {
	_, ok := r.(openapi.UpdateAsset403JSONResponse)
	return ok
}

func isDelete401(r openapi.DeleteAssetResponseObject) bool {
	_, ok := r.(openapi.DeleteAsset401JSONResponse)
	return ok
}

func isRestore204(r openapi.RestoreAssetResponseObject) bool {
	_, ok := r.(openapi.RestoreAsset204Response)
	return ok
}

func isRestore403(r openapi.RestoreAssetResponseObject) bool {
	_, ok := r.(openapi.RestoreAsset403JSONResponse)
	return ok
}
