// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #981 — `GET /account/trash?scope=deleted_by_me`, and the hole it
// closes.
//
// THE HOLE. auth.CanRestoreDeleted grants the undo to whoever performed
// the delete. The trash listing was scoped to the OWNER. So a team lead
// who removed a colleague's asset held a restore right with nothing to
// point it at: the row showed in the OWNER's trash, where it renders
// non-restorable (they did not delete it), and in nobody's as
// restorable. #937 shipped the listing and #936 shipped the rule, and
// between them the case fell through.
//
// WHAT THIS FILE REFUSES TO ASSERT FROM A LITERAL. The lead's authority
// here is a TEAM-SCOPED `assets.admin` grant, and the whole question is
// whether the delete that grant permits is then findable. So:
//
//   - the team hierarchy is real — `team_parents` rows, with
//     `team_closure` materialised by the 00001 trigger, the pattern
//     field_plane_test.go established. The seeded database has no
//     hierarchy at all, so a test that did not build one would be
//     asserting nothing about scope.
//   - identities are loaded through auth.Resolver.LoadIdentity, so the
//     scoped grant arrives closure-expanded from the database rather
//     than from a capability slice this test wrote. A hand-built
//     Identity would prove the listing works for a caller the real
//     middleware might never produce.
//   - the DELETE is the real endpoint through the strict server, not a
//     hand-written UPDATE. `deleted_by_user_ref` is what the new scope
//     selects on, and a test that stamped that column itself would pass
//     against a handler that had stopped writing it.
//
// Every membership assertion carries its positive control, for the
// reason handler_test.go states: an endpoint returning an empty page
// for everybody satisfies every "X must not appear" check perfectly.
package trash_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// DeleteAsset completes the shim: this file drives the real delete
// rather than writing deleted_by_user_ref itself.
func (s shimImpl) DeleteAsset(ctx context.Context, req openapi.DeleteAssetRequestObject) (openapi.DeleteAssetResponseObject, error) {
	return s.assets.DeleteAsset(ctx, req)
}

// --- fixture extensions ----------------------------------------------------

// resolver builds the identity loader the middleware uses, so a
// team-scoped grant arrives pre-expanded through team_closure.
func (f *fixture) resolver() *auth.Resolver {
	return &auth.Resolver{Pool: f.pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// routerAsResolved is routerAs with the identity LOADED rather than
// declared. The difference is the whole point of this file: `caps ...string`
// can only express a global grant, and the caller under test holds a
// scoped one.
func (f *fixture) routerAsResolved(userRef int64) chi.Router {
	f.t.Helper()
	res := f.resolver()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := res.LoadIdentity(req.Context(), userRef)
			next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(f.shim, nil), r)
	return r
}

func (f *fixture) getTrashResolved(userRef int64, query string) openapi.TrashList {
	f.t.Helper()
	rr := httptest.NewRecorder()
	url := "/account/trash"
	if query != "" {
		url += "?" + query
	}
	f.routerAsResolved(userRef).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK {
		f.t.Fatalf("GET %s as %d = %d, body=%s", url, userRef, rr.Code, rr.Body.String())
	}
	var page openapi.TrashList
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		f.t.Fatalf("decode trash page: %v (body=%s)", err, rr.Body.String())
	}
	return page
}

// deleteAsset drives DELETE /assets/{id} with an optional JSON body and
// returns the status. `body` empty means no body at all — the shape a
// self-delete sends.
func (f *fixture) deleteAsset(userRef int64, id uuid.UUID, body string) int {
	f.t.Helper()
	rr := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodDelete, "/assets/"+id.String(), nil)
	} else {
		req = httptest.NewRequest(http.MethodDelete, "/assets/"+id.String(), bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	f.routerAsResolved(userRef).ServeHTTP(rr, req)
	return rr.Code
}

// team seeds a team, linking it under `parent` through team_parents so
// the 00001 trigger materialises the closure.
func (f *fixture) team(label string, parent *uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`,
		id, "trash_team_"+id.String()[:8], label,
	); err != nil {
		f.t.Fatalf("seed team %q: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	if parent != nil {
		if _, err := f.pool.Exec(context.Background(),
			`INSERT INTO team_parents (parent_id, child_id) VALUES ($1, $2)`, *parent, id,
		); err != nil {
			f.t.Fatalf("link team %q: %v", label, err)
		}
	}
	return id
}

// grant gives userRef a capability, scoped to `team` when non-nil.
func (f *fixture) grant(userRef int64, code string, team *uuid.UUID) {
	f.t.Helper()
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO user_capability_grants (user_ref, capability_code, team_id) VALUES ($1,$2,$3)`,
		userRef, code, teamArg,
	); err != nil {
		f.t.Fatalf("grant %s to %d: %v", code, userRef, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM user_capability_grants WHERE user_ref=$1 AND capability_code=$2`,
			userRef, code)
	})
}

// liveAsset seeds a LIVE asset owned by `owner` inside `team`. Distinct
// from the fixture's `asset` helper, which seeds team-less rows and can
// pre-stamp the delete — neither of which suits a test whose subject is
// a real delete performed under a team-scoped grant.
func (f *fixture) liveAsset(owner int64, team *uuid.UUID, title string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	// Random hash: the 00001 per-owner dedup index rejects two assets
	// from one owner over identical bytes.
	hb := make([]byte, 16)
	_, _ = rand.Read(hb)
	hashHex := hex.EncodeToString(sha256.New().Sum(hb))[:64]
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING`, hashHex,
	); err != nil {
		f.t.Fatalf("seed storage_object: %v", err)
	}
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, asset_type, owner_user_ref, team_id, status,
		                    processing_status, sensitivity, file_hash, file_extension,
		                    file_size_bytes)
		VALUES ($1,$2,(SELECT MIN(ref) FROM asset_types),$3,$4,'active','ready','public',
		        $5,'png',1024)`,
		id, title, owner, teamArg, hashHex,
	); err != nil {
		f.t.Fatalf("seed live asset %q: %v", title, err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hashHex)
	})
	return id
}

// deletedReason reads the column back. Asserted server-side because the
// listing deliberately does not return it — the round trip has to be
// checked where the value actually lands.
func (f *fixture) deletedReason(id uuid.UUID) *string {
	f.t.Helper()
	var s *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT deleted_reason FROM assets WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("read deleted_reason: %v", err)
	}
	return s
}

func (f *fixture) deletedBy(id uuid.UUID) *int64 {
	f.t.Helper()
	var ref *int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT deleted_by_user_ref FROM assets WHERE id = $1`, id).Scan(&ref); err != nil {
		f.t.Fatalf("read deleted_by_user_ref: %v", err)
	}
	return ref
}

func (f *fixture) isDeleted(id uuid.UUID) bool {
	f.t.Helper()
	var deleted bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT deleted_at IS NOT NULL FROM assets WHERE id = $1`, id).Scan(&deleted); err != nil {
		f.t.Fatalf("read deleted_at: %v", err)
	}
	return deleted
}

// --- the lead / colleague world --------------------------------------------

// teamWorld is the shared setup: a parent team with a child under it,
// a lead holding assets.admin scoped to the PARENT, and a colleague who
// owns an asset in the CHILD. The delete therefore has to travel the
// closure to be permitted at all, which is the case a self-referential
// closure would silently pass.
type teamWorld struct {
	lead      int64
	colleague int64
	assetID   uuid.UUID
}

func newTeamWorld(t *testing.T, f *fixture) teamWorld {
	t.Helper()
	parent := f.team("division", nil)
	child := f.team("squad", &parent)
	lead := f.user("lead")
	colleague := f.user("colleague")
	f.grant(lead, visibility.AssetsAdmin, &parent)
	return teamWorld{
		lead:      lead,
		colleague: colleague,
		assetID:   f.liveAsset(colleague, &child, "colleagues-board"),
	}
}

// --- 1. the tab, and the two sides of the same row -------------------------

func TestDeletedByMe_LeadSeesTheirOwnDeleteAsRestorable(t *testing.T) {
	f := newFixture(t)
	w := newTeamWorld(t, f)

	// POSITIVE CONTROL for the whole premise: before the delete, the
	// lead's tab is empty of this row. If it were present here, every
	// assertion below would be about a listing that ignores its filter.
	if _, ok := idsOf(f.getTrashResolved(w.lead, "limit=200&scope=deleted_by_me"))[w.assetID]; ok {
		t.Fatal("the lead's deleted_by_me tab listed a LIVE asset; the scope must select " +
			"soft-deleted rows only")
	}

	if code := f.deleteAsset(w.lead, w.assetID, ""); code != http.StatusNoContent {
		t.Fatalf("lead DELETE /assets/{id} = %d, want 204 — the team-scoped grant should permit "+
			"it; without a real delete this test asserts nothing", code)
	}
	if !f.isDeleted(w.assetID) {
		t.Fatal("204 but deleted_at is still NULL")
	}
	if by := f.deletedBy(w.assetID); by == nil || *by != w.lead {
		t.Fatalf("deleted_by_user_ref = %v, want the lead (%d) — the new scope selects on this "+
			"column, so a handler that stopped writing it breaks the tab silently", by, w.lead)
	}

	// The lead's tab: present, and restorable.
	leadTab := idsOf(f.getTrashResolved(w.lead, "limit=200&scope=deleted_by_me"))
	item, ok := leadTab[w.assetID]
	if !ok {
		t.Fatal("the lead deleted this asset and cannot find it in their own 'deleted by me' " +
			"tab — this is exactly the hole #981 exists to close")
	}
	if !item.RestorableByCaller {
		t.Error("the deleter is told they may not restore their own delete; " +
			"auth.CanRestoreDeleted says otherwise, and the flag is supposed to be that rule")
	}

	// The OWNER's default tab: present, and NOT restorable. Both halves
	// of the same row, from the two sides.
	ownerTab := idsOf(f.getTrashResolved(w.colleague, "limit=200"))
	ownerItem, ok := ownerTab[w.assetID]
	if !ok {
		t.Fatal("the owner's trash lost an asset someone else deleted; it is recoverable by " +
			"request (#931) and must stay visible to them")
	}
	if ownerItem.RestorableByCaller {
		t.Error("the owner is offered a Restore button for a delete they did not perform")
	}
}

// The lead's own OWNED trash and their deleted_by_me tab are disjoint —
// the `owner IS DISTINCT FROM caller` conjunct. A row that appeared in
// both would double-list in a two-tab UI.
func TestDeletedByMe_ScopesAreDisjoint(t *testing.T) {
	f := newFixture(t)
	w := newTeamWorld(t, f)

	own := f.liveAsset(w.lead, nil, "leads-own-work")
	if code := f.deleteAsset(w.lead, own, ""); code != http.StatusNoContent {
		t.Fatalf("lead deleting their OWN asset = %d, want 204", code)
	}
	if code := f.deleteAsset(w.lead, w.assetID, ""); code != http.StatusNoContent {
		t.Fatalf("lead deleting the colleague's asset = %d, want 204", code)
	}

	owned := idsOf(f.getTrashResolved(w.lead, "limit=200"))
	byMe := idsOf(f.getTrashResolved(w.lead, "limit=200&scope=deleted_by_me"))

	if _, ok := owned[own]; !ok {
		t.Error("the lead's own deleted asset is missing from the default (owned) scope")
	}
	if _, ok := byMe[own]; ok {
		t.Error("a self-delete of a self-owned asset appeared in 'deleted by me' too — the two " +
			"tabs would double-list it")
	}
	if _, ok := byMe[w.assetID]; !ok {
		t.Error("the colleague's asset is missing from the lead's 'deleted by me'")
	}
	if _, ok := owned[w.assetID]; ok {
		t.Error("the default owner scope leaked an asset the caller does not own")
	}
}

// The tab is a history of the CALLER's acts, not a window on everyone's.
// A's tab must never carry B's deletions — including of the same asset
// class, in the same team.
func TestDeletedByMe_NeverShowsAnotherUsersDeletions(t *testing.T) {
	f := newFixture(t)
	parent := f.team("division", nil)
	leadA := f.user("lead-a")
	leadB := f.user("lead-b")
	colleague := f.user("colleague")
	f.grant(leadA, visibility.AssetsAdmin, &parent)
	f.grant(leadB, visibility.AssetsAdmin, &parent)

	byA := f.liveAsset(colleague, &parent, "deleted-by-a")
	byB := f.liveAsset(colleague, &parent, "deleted-by-b")
	if code := f.deleteAsset(leadA, byA, ""); code != http.StatusNoContent {
		t.Fatalf("A's delete = %d, want 204", code)
	}
	if code := f.deleteAsset(leadB, byB, ""); code != http.StatusNoContent {
		t.Fatalf("B's delete = %d, want 204", code)
	}

	aTab := idsOf(f.getTrashResolved(leadA, "limit=200&scope=deleted_by_me"))
	// POSITIVE CONTROL — A's own deletion IS there, so the absence check
	// below cannot be satisfied by an endpoint that returns nothing.
	if _, ok := aTab[byA]; !ok {
		t.Fatalf("positive control failed: A's own deletion is missing from A's tab (%d items)",
			len(aTab))
	}
	if _, ok := aTab[byB]; ok {
		t.Error("A's 'deleted by me' listed an asset B deleted — the scope is the caller's own " +
			"history, and its whole justification is that the caller already performed the act")
	}

	bTab := idsOf(f.getTrashResolved(leadB, "limit=200&scope=deleted_by_me"))
	if _, ok := bTab[byB]; !ok {
		t.Error("B's own deletion is missing from B's tab")
	}
	if _, ok := bTab[byA]; ok {
		t.Error("B's 'deleted by me' listed an asset A deleted")
	}
}

// The restore the tab advertises actually works, through the unchanged
// per-domain endpoint. The flag promising it is worthless if the
// endpoint disagrees — the property handler_test.go pins for the owned
// scope, checked here for the new one.
func TestDeletedByMe_RestoreThroughTheRealEndpoint(t *testing.T) {
	f := newFixture(t)
	w := newTeamWorld(t, f)

	if code := f.deleteAsset(w.lead, w.assetID, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", code)
	}
	// NEGATIVE FIRST, and it must be a real refusal: the OWNER may not
	// undo someone else's delete. If this passed, "restorable_by_caller
	// = false" on the owner's row would be decoration.
	if code := f.restoreAsset(w.colleague, w.assetID); code != http.StatusForbidden {
		t.Fatalf("owner restoring someone else's delete = %d, want 403", code)
	}
	if !f.isDeleted(w.assetID) {
		t.Fatal("the refused restore still cleared deleted_at — a gate that answers 403 after " +
			"writing is not a gate")
	}

	rr := httptest.NewRecorder()
	f.routerAsResolved(w.lead).ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/admin/assets/"+w.assetID.String()+"/restore", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("lead restoring their own delete = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if f.isDeleted(w.assetID) {
		t.Error("204 but deleted_at is still set")
	}
	if _, ok := idsOf(f.getTrashResolved(w.lead, "limit=200&scope=deleted_by_me"))[w.assetID]; ok {
		t.Error("a restored asset is still listed in 'deleted by me'; the tab must be current rows")
	}
}

// --- 2. the reason round trip ----------------------------------------------

// The dialog collects a reason when you are deleting someone else's
// work. This asserts it lands in the column #931's appeal flow reads —
// server-side, because the listing deliberately never returns it.
func TestDeleteReason_RoundTripsIntoDeletedReason(t *testing.T) {
	f := newFixture(t)
	w := newTeamWorld(t, f)

	const reason = "duplicate upload — the newer board supersedes it"
	if code := f.deleteAsset(w.lead, w.assetID, `{"reason":`+quote(reason)+`}`); code != http.StatusNoContent {
		t.Fatalf("delete with reason = %d, want 204", code)
	}
	got := f.deletedReason(w.assetID)
	if got == nil {
		t.Fatal("deleted_reason is NULL after a delete that supplied one — the capture half of " +
			"#931 is this issue's job, and it did not happen")
	}
	if *got != reason {
		t.Errorf("deleted_reason = %q, want %q", *got, reason)
	}

	// The counterpart: a delete with NO body leaves the column NULL
	// rather than an empty string. NULL means "none given"; "" would be
	// a reason that says nothing, which reads differently downstream.
	other := f.liveAsset(w.colleague, nil, "no-reason-given")
	if code := f.deleteAsset(w.colleague, other, ""); code != http.StatusNoContent {
		t.Fatalf("bodyless delete = %d, want 204", code)
	}
	if r := f.deletedReason(other); r != nil {
		t.Errorf("deleted_reason = %q after a bodyless delete, want NULL", *r)
	}
}

// --- 3. the permission negative, verified reachable -------------------------

// The affordance the client hides must correspond to a refusal the
// server actually makes. A stranger — authenticated, no capability, not
// the owner, not in the team — is refused, and the row is untouched.
//
// The positive control sits in the same test: the owner deletes the
// SAME asset immediately afterwards, so a build where DELETE 403s for
// everyone cannot pass this.
func TestDelete_StrangerIsRefusedAndTheRowSurvives(t *testing.T) {
	f := newFixture(t)
	owner := f.user("owner")
	stranger := f.user("stranger")
	id := f.liveAsset(owner, nil, "not-yours")

	if code := f.deleteAsset(stranger, id, ""); code != http.StatusForbidden {
		t.Fatalf("stranger DELETE = %d, want 403", code)
	}
	if f.isDeleted(id) {
		t.Fatal("the refused delete soft-deleted the row anyway")
	}
	if _, ok := idsOf(f.getTrashResolved(stranger, "limit=200&scope=deleted_by_me"))[id]; ok {
		t.Error("the refused delete still put a row in the stranger's 'deleted by me' tab")
	}

	// POSITIVE CONTROL — the same asset, the owner, 204.
	if code := f.deleteAsset(owner, id, ""); code != http.StatusNoContent {
		t.Fatalf("owner DELETE of their own asset = %d, want 204 — without this the refusal "+
			"above proves nothing", code)
	}
}

// A team-scoped grant over a DIFFERENT branch is not authority here.
// This is the sibling of the widening test: it is what stops
// "scope-aware" meaning "any grant, anywhere".
func TestDelete_ScopedGrantOverAnotherBranchIsRefused(t *testing.T) {
	f := newFixture(t)
	branchA := f.team("branch-a", nil)
	branchB := f.team("branch-b", nil)
	outsider := f.user("outsider")
	owner := f.user("owner")
	f.grant(outsider, visibility.AssetsAdmin, &branchA)

	id := f.liveAsset(owner, &branchB, "other-branch")
	if code := f.deleteAsset(outsider, id, ""); code != http.StatusForbidden {
		t.Fatalf("assets.admin scoped to branch A deleting a branch-B asset = %d, want 403", code)
	}
	if f.isDeleted(id) {
		t.Fatal("refused, but the row was soft-deleted")
	}

	// POSITIVE CONTROL — the same holder, an asset in their OWN branch.
	mine := f.liveAsset(owner, &branchA, "own-branch")
	if code := f.deleteAsset(outsider, mine, ""); code != http.StatusNoContent {
		t.Fatalf("assets.admin scoped to branch A deleting a branch-A asset = %d, want 204 — "+
			"without this the refusal above could be a grant that never works", code)
	}
}

// --- 4. the scope parameter itself -----------------------------------------

func TestListMyTrash_UnknownScopeIs400(t *testing.T) {
	f := newFixture(t)
	user := f.user("scope")

	rr := httptest.NewRecorder()
	f.routerAsResolved(user).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/account/trash?scope=everything", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown scope = %d, want 400 — silently falling back to the owned list would "+
			"answer 'you deleted nothing of anyone else's', which is a wrong answer wearing a 200",
			rr.Code)
	}
}

// Omitting the parameter must behave exactly as it did before #981.
func TestListMyTrash_DefaultScopeIsUnchanged(t *testing.T) {
	f := newFixture(t)
	w := newTeamWorld(t, f)

	own := f.liveAsset(w.lead, nil, "leads-own")
	if code := f.deleteAsset(w.lead, own, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", code)
	}
	if code := f.deleteAsset(w.lead, w.assetID, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", code)
	}

	bare := idsOf(f.getTrashResolved(w.lead, "limit=200"))
	explicit := idsOf(f.getTrashResolved(w.lead, "limit=200&scope=owned_by_me"))
	if len(bare) != len(explicit) {
		t.Fatalf("no-scope page has %d items, scope=owned_by_me has %d; the default must be the "+
			"old behaviour exactly", len(bare), len(explicit))
	}
	if _, ok := bare[own]; !ok {
		t.Error("the default page lost the caller's own deleted asset")
	}
	if _, ok := bare[w.assetID]; ok {
		t.Error("the default page gained a row it never used to carry")
	}
}

// quote renders a Go string as a JSON string literal, so a reason
// containing an em dash or a quote cannot break the request body.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
