// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #931 — the restoration appeal, driven end to end and refused at every
// edge.
//
// The happy path is one sequence:
//
//	an admin deletes an artist's work
//	  → the artist's trash offers an appeal, not a dead end
//	  → the artist files it
//	  → the DELETER finds it in a queue they can reach with no
//	    capability at all
//	  → granting it PUTS THE ITEM BACK
//	  → and writes no capability grant.
//
// That last clause is the one this file exists for. The access branch
// of Grant copies `requested_capability` verbatim into
// user_capability_grants — ADR 0064 and migration 00035 both name it as
// the escalation surface, contained today only by keeping the decide
// gate narrow. #931 widens the decide gate to a NEW principal (the
// deleter), so the containment has to be re-proved rather than assumed,
// and "the marker confers nothing" is not the proof: that is a claim
// about the capability, while `count(*) FROM user_capability_grants`
// unchanged is a claim about the code.
//
// # Every refusal here is proven CONSTRUCTIBLE first
//
// A negative control that cannot fire is worse than none — it reads as
// coverage. So each one builds its precondition through a production
// path and asserts the state exists before asserting the refusal. The
// sharpest case is
// TestRestoreAppeal_DeleterCannotDecideAnUnrelatedCapability: it needs a
// resource_request naming `system.admin` against a soft-deleted asset,
// and that row is filed through POST /assets/{id}/request-access, which
// accepts an explicit capability and does not require the asset to be
// live. If that endpoint ever stops accepting one, this test must fail
// loudly rather than pass vacuously — hence the explicit 201 check on
// the setup.
//
// # Assertions read the DATABASE, not the response
//
// A decide handler returns the row it just wrote, so a body assertion
// passes on a bug that wrote the wrong thing (#946). Every "the item
// stayed deleted" / "the request stayed pending" check below re-reads
// the table.

package requests_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/requests"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// realRestorer wires the PRODUCTION softdelete primitives behind the
// requests package's restorer interface — the same three calls the
// per-kind restore endpoints make.
//
// Real, not a fake, on purpose: the interesting assertion is "the row's
// deleted_at is NULL afterwards", and a stub that recorded the call
// would let a Grant that never restored anything pass. The cache
// fan-out half of the composition root's adapter is deliberately absent
// here — it has no observable effect on a pool-level assertion, and it
// is covered where it is observable, in the browser.
type realRestorer struct{ sd *softdelete.Service }

func (r realRestorer) Restore(
	ctx context.Context, req *http.Request,
	kind requests.TargetKind, id uuid.UUID, actorUserRef int64,
) error {
	var err error
	switch kind {
	case requests.TargetKindAsset:
		err = r.sd.RestoreAsset(ctx, req, id, actorUserRef)
	case requests.TargetKindPost:
		err = r.sd.RestorePost(ctx, req, id, actorUserRef)
	case requests.TargetKindCollection:
		err = r.sd.RestoreCollection(ctx, req, id, actorUserRef)
	default:
		return requests.ErrTargetGone
	}
	switch {
	case err == nil:
		return nil
	case isNotDeleted(err):
		return requests.ErrTargetAlreadyLive
	case isNotFound(err):
		return requests.ErrTargetGone
	default:
		return err
	}
}

func isNotDeleted(err error) bool { return errorsIs(err, softdelete.ErrNotDeleted) }
func isNotFound(err error) bool   { return errorsIs(err, softdelete.ErrNotFound) }

func errorsIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// appealHandler builds the HTTP surface with a real restorer attached.
func appealHandler(t *testing.T, pool *pgxpool.Pool, noter *notifierRecording) *requests.HTTPHandler {
	t.Helper()
	dom := newHandlerE(t, pool, newAuditRec(), noter)
	dom.SetRestorer(realRestorer{sd: softdelete.NewService(pool, nil)})
	return requests.NewHTTPHandler(dom, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// identityWith puts a signed-in user holding the named capabilities on
// the context. The plain `identity` helper (access_loop_test.go) is
// capability-less; this one exists for the disjuncts that must NOT
// help — share.grant in particular.
func identityWith(ctx context.Context, ref int64, caps ...string) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{
		UserRef: ref, AuthMethod: "session", Capabilities: caps,
	})
}

// ---------------------------------------------------------------------------
// Fixtures — one per soft-deletable kind, each with a real deleter
// ---------------------------------------------------------------------------

// seedAsset inserts a live asset owned by ownerRef.
func seedAsset(t *testing.T, pool *pgxpool.Pool, ownerRef int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO assets (title, asset_type, owner_user_ref, status)
		 VALUES ($1, (SELECT MIN(ref) FROM asset_types), $2, 'active') RETURNING id`,
		"appeal-asset-"+uuid.New().String()[:8], ownerRef).Scan(&id); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

func seedPost(t *testing.T, pool *pgxpool.Pool, authorRef int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO posts (title, author_user_ref) VALUES ($1, $2) RETURNING id`,
		"appeal-post-"+uuid.New().String()[:8], authorRef).Scan(&id); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id) })
	return id
}

func seedCollection(t *testing.T, pool *pgxpool.Pool, ownerRef int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO collections (name, owner_user_ref) VALUES ($1, $2) RETURNING id`,
		"appeal-coll-"+uuid.New().String()[:8], ownerRef).Scan(&id); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id) })
	return id
}

// softDelete marks a row deleted BY a specific user, which is the fact
// the whole gate turns on. Writes the same three columns every
// production delete handler writes.
func softDelete(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID, deleterRef int64, reason string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE `+table+` SET deleted_at = NOW(), deleted_by_user_ref = $2, deleted_reason = $3
		  WHERE id = $1`, id, deleterRef, reason)
	if err != nil {
		t.Fatalf("soft delete %s: %v", table, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("soft delete %s affected %d rows, want 1", table, tag.RowsAffected())
	}
}

// ---------------------------------------------------------------------------
// Readers — every assertion below reads the DB, never the response echo
// ---------------------------------------------------------------------------

func isSoftDeleted(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID) bool {
	t.Helper()
	var deleted bool
	if err := pool.QueryRow(context.Background(),
		`SELECT deleted_at IS NOT NULL FROM `+table+` WHERE id = $1`, id).Scan(&deleted); err != nil {
		t.Fatalf("read %s deleted_at: %v", table, err)
	}
	return deleted
}

func requestState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM resource_request WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("read request state: %v", err)
	}
	return state
}

func grantCount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_capability_grants`).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

// appeal files a restoration request and returns the response, so each
// caller can assert the status it expects.
func appeal(t *testing.T, h *requests.HTTPHandler, ctx context.Context, kind string, id uuid.UUID, reason string) openapi.RequestRestoreResponseObject {
	t.Helper()
	body := &openapi.RequestRestoreJSONRequestBody{}
	if reason != "" {
		body.Reason = &reason
	}
	resp, err := h.RequestRestore(ctx, openapi.RequestRestoreRequestObject{
		Kind: openapi.RequestRestoreParamsKind(kind),
		Id:   openapi_types.UUID(id),
		Body: body,
	})
	if err != nil {
		t.Fatalf("RequestRestore(%s): %v", kind, err)
	}
	return resp
}

func decide(t *testing.T, h *requests.HTTPHandler, ctx context.Context, requestID uuid.UUID, d openapi.DecideAdminRequestJSONBodyDecision) openapi.DecideAdminRequestResponseObject {
	t.Helper()
	resp, err := h.DecideAdminRequest(ctx, openapi.DecideAdminRequestRequestObject{
		Id:   openapi_types.UUID(requestID),
		Body: &openapi.DecideAdminRequestJSONRequestBody{Decision: d},
	})
	if err != nil {
		t.Fatalf("DecideAdminRequest: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

// TestRestoreAppeal_SubmitGuards walks the four guards in the order the
// handler applies them, and each precondition is built rather than
// imagined.
func TestRestoreAppeal_SubmitGuards(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	stranger := seedUserForRequests(t, pool)
	h := appealHandler(t, pool, newNoter())

	// ── anonymous ────────────────────────────────────────────────────
	deleted := seedAsset(t, pool, owner)
	softDelete(t, pool, "assets", deleted, admin, "moderated")
	if got := appeal(t, h, ctx, "asset", deleted, ""); !is401(got) {
		t.Errorf("anonymous appeal returned %T, want 401", got)
	}

	// ── the row is LIVE: nothing to appeal ───────────────────────────
	// Constructible by definition — this is every asset on the
	// instance. 404 rather than a 400 explaining the row is fine:
	// "there is no deleted thing here" is the same answer as "not
	// yours", and splitting them would make the endpoint a probe.
	live := seedAsset(t, pool, owner)
	if isSoftDeleted(t, pool, "assets", live) {
		t.Fatal("fixture is deleted; the live-row guard cannot be reached")
	}
	if got := appeal(t, h, identity(ctx, owner), "asset", live, ""); !is404(got) {
		t.Errorf("appeal on a live row returned %T, want 404", got)
	}

	// ── not the owner ────────────────────────────────────────────────
	if got := appeal(t, h, identity(ctx, stranger), "asset", deleted, ""); !is404(got) {
		t.Errorf("stranger's appeal returned %T, want 404 — and it must not be "+
			"distinguishable from a row that does not exist", got)
	}

	// ── the caller could just restore it ─────────────────────────────
	// Reachable whenever an owner deletes their own work: they own it
	// AND they are the deleter, so CanRestoreDeleted says yes.
	mine := seedAsset(t, pool, owner)
	softDelete(t, pool, "assets", mine, owner, "changed my mind")
	if got := appeal(t, h, identity(ctx, owner), "asset", mine, ""); !is409(got) {
		t.Errorf("self-deleted row appeal returned %T, want 409 — asking someone "+
			"to undo what you can undo yourself is a queue item with no answer", got)
	}

	// ── the happy path, and the coalesce ─────────────────────────────
	first := appeal(t, h, identity(ctx, owner), "asset", deleted, "the licence is fine")
	created, ok := first.(openapi.RequestRestore201JSONResponse)
	if !ok {
		t.Fatalf("owner's appeal returned %T, want 201", first)
	}
	if created.RequestedCapability != requests.CapRestoreRequest {
		t.Errorf("requested_capability = %q, want the appeal marker %q",
			created.RequestedCapability, requests.CapRestoreRequest)
	}
	if string(created.TargetKind) != "asset" {
		t.Errorf("target_kind = %q, want asset", created.TargetKind)
	}

	second := appeal(t, h, identity(ctx, owner), "asset", deleted, "asked again")
	repeat, ok := second.(openapi.RequestRestore200JSONResponse)
	if !ok {
		t.Fatalf("second identical appeal returned %T, want 200 (coalesced)", second)
	}
	if repeat.Id != created.Id {
		t.Errorf("coalesce returned a different row (%s vs %s)", repeat.Id, created.Id)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM resource_request
		  WHERE requester_user_ref = $1 AND target_id = $2
		    AND requested_capability = $3`,
		owner, deleted, requests.CapRestoreRequest).Scan(&n); err != nil {
		t.Fatalf("count appeals: %v", err)
	}
	if n != 1 {
		t.Errorf("%d appeal rows persisted for one ask asked twice, want 1", n)
	}
}

// TestRestoreAppeal_AllThreeKinds pins that a post and a collection can
// be appealed and restored, not only an asset — the whole point of
// target_kind.
func TestRestoreAppeal_AllThreeKinds(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	h := appealHandler(t, pool, newNoter())

	cases := []struct {
		kind  string
		table string
		id    uuid.UUID
	}{
		{"asset", "assets", seedAsset(t, pool, owner)},
		{"post", "posts", seedPost(t, pool, owner)},
		{"collection", "collections", seedCollection(t, pool, owner)},
	}

	before := grantCount(t, pool)

	for _, c := range cases {
		softDelete(t, pool, c.table, c.id, admin, "moderated")

		got := appeal(t, h, identity(ctx, owner), c.kind, c.id, "please")
		created, ok := got.(openapi.RequestRestore201JSONResponse)
		if !ok {
			t.Fatalf("%s: appeal returned %T, want 201", c.kind, got)
		}

		// The deleter decides, holding NOTHING.
		if resp := decide(t, h, identity(ctx, admin), uuid.UUID(created.Id), openapi.Granted); !is200Decide(resp) {
			t.Fatalf("%s: deleter's grant returned %T, want 200", c.kind, resp)
		}

		if isSoftDeleted(t, pool, c.table, c.id) {
			t.Errorf("%s: still soft-deleted after a granted appeal — the decision "+
				"said yes and nothing happened", c.kind)
		}
		if s := requestState(t, pool, uuid.UUID(created.Id)); s != "granted" {
			t.Errorf("%s: request state = %q, want granted", c.kind, s)
		}
	}

	// THE assertion (#881). Three grants, no capability grants.
	if after := grantCount(t, pool); after != before {
		t.Errorf("user_capability_grants went %d → %d across three restore grants; "+
			"the appeal branch must never reach the grant INSERT — that INSERT "+
			"copies requested_capability verbatim and is the escalation surface "+
			"ADR 0064 names", before, after)
	}
}

// ---------------------------------------------------------------------------
// Decide — refusals, asserted on persisted state
// ---------------------------------------------------------------------------

// TestRestoreAppeal_DecideRefusals is the gate, from four directions.
//
// The share.grant case is the one that is NEW policy rather than
// inherited: that capability decides every other kind of request on the
// instance, and it must not decide this one. Authority over sharing is
// not authority over moderation.
func TestRestoreAppeal_DecideRefusals(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	stranger := seedUserForRequests(t, pool)
	sharer := seedUserForRequests(t, pool)
	h := appealHandler(t, pool, newNoter())

	assetID := seedAsset(t, pool, owner)
	softDelete(t, pool, "assets", assetID, admin, "moderated")
	created := appeal(t, h, identity(ctx, owner), "asset", assetID, "mine").(openapi.RequestRestore201JSONResponse)
	reqID := uuid.UUID(created.Id)

	refusals := []struct {
		name string
		ctx  context.Context
		why  string
	}{
		{
			"the requester deciding their own appeal",
			identity(ctx, owner),
			"the owner IS the requester here; an owner disjunct would let anyone " +
				"whose work was moderated approve its return",
		},
		{
			"a stranger",
			identity(ctx, stranger),
			"ownership of nothing and no capability decides nothing",
		},
		{
			"a share.grant holder who did not delete it",
			identityWith(ctx, sharer, requests.CapShareGrant),
			"share.grant decides every OTHER request on the instance; it must not " +
				"decide this one, or the delete gate becomes decorative",
		},
	}

	for _, r := range refusals {
		t.Run(r.name, func(t *testing.T) {
			got := decide(t, h, r.ctx, reqID, openapi.Granted)
			if _, forbidden := got.(openapi.DecideAdminRequest403JSONResponse); !forbidden {
				t.Errorf("decide returned %T, want 403 — %s", got, r.why)
			}
			// Persisted state, not the response: a handler that
			// answered 403 after already restoring would pass a
			// body-only assertion.
			if s := requestState(t, pool, reqID); s != "pending" {
				t.Errorf("request state = %q after a refused decision, want pending", s)
			}
			if !isSoftDeleted(t, pool, "assets", assetID) {
				t.Error("the asset was restored by a decision that was refused")
			}
		})
	}

	// And the person it IS addressed to: admitted, holding nothing.
	if got := decide(t, h, identity(ctx, admin), reqID, openapi.Granted); !is200Decide(got) {
		t.Fatalf("the deleter's decision returned %T, want 200", got)
	}
	if isSoftDeleted(t, pool, "assets", assetID) {
		t.Error("granted, but the asset is still soft-deleted in the database")
	}
}

// TestRestoreAppeal_DeleterCannotDecideAnUnrelatedCapability is the
// payload-scoping control (#881's lesson, restated for the new
// principal).
//
// The new disjunct is scoped to the marker's PAYLOAD, not to
// "restore-ish requests" and not to "requests about deleted things". A
// deleter who could decide any request naming their deleted asset would
// be a second route to granting `system.admin` — the same trap, wearing
// a moderation hat.
//
// CONSTRUCTIBILITY FIRST. The row is filed through
// POST /assets/{id}/request-access, which accepts an explicit
// capability (admin tooling predates the button) and does not require
// the asset to be live. The 201 assertion below is load-bearing: if
// that endpoint ever stops accepting a named capability, this test must
// fail rather than quietly stop testing anything.
func TestRestoreAppeal_DeleterCannotDecideAnUnrelatedCapability(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	schemer := seedUserForRequests(t, pool)
	h := appealHandler(t, pool, newNoter())

	assetID := seedAsset(t, pool, owner)
	softDelete(t, pool, "assets", assetID, admin, "moderated")

	sysadmin := requests.CapSystemAdmin
	filed, err := h.RequestAssetAccess(identity(ctx, schemer), openapi.RequestAssetAccessRequestObject{
		Id: openapi_types.UUID(assetID),
		Body: &openapi.RequestAssetAccessJSONRequestBody{
			Capability: &sysadmin,
			Reason:     strptr("looks routine"),
		},
	})
	if err != nil {
		t.Fatalf("RequestAssetAccess: %v", err)
	}
	created, ok := filed.(openapi.RequestAssetAccess201JSONResponse)
	if !ok {
		t.Fatalf("could not construct a system.admin request against a deleted asset "+
			"(got %T). If that is now impossible the hazard is gone — but this test "+
			"must be rewritten, not left passing vacuously", filed)
	}
	reqID := uuid.UUID(created.Id)
	if created.RequestedCapability != requests.CapSystemAdmin {
		t.Fatalf("fixture names %q, not system.admin — nothing dangerous is being tested",
			created.RequestedCapability)
	}

	before := grantCount(t, pool)

	// The deleter holds nothing and did not own the asset. Their new
	// authority is over APPEALS, and this is not one.
	got := decide(t, h, identity(ctx, admin), reqID, openapi.Granted)
	if _, forbidden := got.(openapi.DecideAdminRequest403JSONResponse); !forbidden {
		t.Errorf("the deleter decided a system.admin request and got %T, want 403 — "+
			"the restore disjunct must be scoped to the marker's payload, not to "+
			"every request about something they deleted", got)
	}
	if s := requestState(t, pool, reqID); s != "pending" {
		t.Errorf("request state = %q, want pending", s)
	}
	if after := grantCount(t, pool); after != before {
		t.Errorf("user_capability_grants went %d → %d — a system.admin grant was written", before, after)
	}
}

// TestRestoreAppeal_DenyLeavesItDeleted. A denial is a real answer, and
// the item must stay where it is.
func TestRestoreAppeal_DenyLeavesItDeleted(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	noter := newNoter()
	h := appealHandler(t, pool, noter)

	assetID := seedAsset(t, pool, owner)
	softDelete(t, pool, "assets", assetID, admin, "moderated")
	created := appeal(t, h, identity(ctx, owner), "asset", assetID, "please").(openapi.RequestRestore201JSONResponse)

	if got := decide(t, h, identity(ctx, admin), uuid.UUID(created.Id), openapi.Denied); !is200Decide(got) {
		t.Fatalf("deny returned %T, want 200", got)
	}
	if s := requestState(t, pool, uuid.UUID(created.Id)); s != "denied" {
		t.Errorf("request state = %q, want denied", s)
	}
	if !isSoftDeleted(t, pool, "assets", assetID) {
		t.Error("the asset came back from a DENIED appeal")
	}

	// The requester is told. A denial nobody hears about is a request
	// that looks unanswered forever.
	var told bool
	for _, c := range noter.withVerb("resource_request_denied") {
		if c.Recipient == owner {
			told = true
		}
	}
	if !told {
		t.Error("the requester was not notified of the denial")
	}
}

// TestRestoreAppeal_ExpiryRefused. A performed restore cannot expire —
// nothing is scheduled to re-delete the item — so accepting the field
// would let a decider believe they had set a deadline.
func TestRestoreAppeal_ExpiryRefused(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	h := appealHandler(t, pool, newNoter())

	assetID := seedAsset(t, pool, owner)
	softDelete(t, pool, "assets", assetID, admin, "moderated")
	created := appeal(t, h, identity(ctx, owner), "asset", assetID, "").(openapi.RequestRestore201JSONResponse)

	future := time.Now().Add(48 * time.Hour)
	got, err := h.DecideAdminRequest(identity(ctx, admin), openapi.DecideAdminRequestRequestObject{
		Id: created.Id,
		Body: &openapi.DecideAdminRequestJSONRequestBody{
			Decision:  openapi.Granted,
			ExpiresAt: &future,
		},
	})
	if err != nil {
		t.Fatalf("DecideAdminRequest: %v", err)
	}
	if _, bad := got.(openapi.DecideAdminRequest400JSONResponse); !bad {
		t.Errorf("a restore grant with expires_at returned %T, want 400", got)
	}
	if s := requestState(t, pool, uuid.UUID(created.Id)); s != "pending" {
		t.Errorf("request state = %q after the refused grant, want pending", s)
	}
	if !isSoftDeleted(t, pool, "assets", assetID) {
		t.Error("the asset was restored by a decision that was rejected as malformed")
	}
}

// TestRestoreAppeal_AlreadyRestoredStillGrants. Someone else got there
// first — a second admin, or the deleter acting directly. The requested
// end-state holds, so the decision is a yes, and the reason records
// that this decider did not perform it.
func TestRestoreAppeal_AlreadyRestoredStillGrants(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	h := appealHandler(t, pool, newNoter())

	assetID := seedAsset(t, pool, owner)
	softDelete(t, pool, "assets", assetID, admin, "moderated")
	created := appeal(t, h, identity(ctx, owner), "asset", assetID, "").(openapi.RequestRestore201JSONResponse)

	// Restored out from under the pending appeal, through the same
	// primitive the restore endpoints use.
	if err := softdelete.NewService(pool, nil).RestoreAsset(ctx, nil, assetID, admin); err != nil {
		t.Fatalf("direct restore: %v", err)
	}
	if isSoftDeleted(t, pool, "assets", assetID) {
		t.Fatal("the direct restore did nothing; the already-live branch is unreachable")
	}

	// deleted_by_user_ref SURVIVES the restore, which is what keeps the
	// appeal decidable by the person it was addressed to.
	if got := decide(t, h, identity(ctx, admin), uuid.UUID(created.Id), openapi.Granted); !is200Decide(got) {
		t.Fatalf("granting an already-satisfied appeal returned %T, want 200", got)
	}
	if s := requestState(t, pool, uuid.UUID(created.Id)); s != "granted" {
		t.Errorf("request state = %q, want granted", s)
	}
	var reason string
	if err := pool.QueryRow(ctx,
		`SELECT decision_reason FROM resource_request WHERE id = $1`, created.Id).Scan(&reason); err != nil {
		t.Fatalf("read decision_reason: %v", err)
	}
	if !containsSubstring(reason, "already restored") {
		t.Errorf("decision_reason = %q; it should record that the item was already "+
			"live, so the decision does not claim work this decider did not do", reason)
	}
}

// TestRestoreAppeal_IncomingQueueIsDeleterKeyed. The queue that used to
// be owner-keyed must now also answer "what did I remove that someone
// wants back" — and must NOT hand an appeal to its own requester.
func TestRestoreAppeal_IncomingQueueIsDeleterKeyed(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	admin := seedUserForRequests(t, pool)
	h := appealHandler(t, pool, newNoter())

	postID := seedPost(t, pool, owner)
	softDelete(t, pool, "posts", postID, admin, "moderated")
	created := appeal(t, h, identity(ctx, owner), "post", postID, "").(openapi.RequestRestore201JSONResponse)
	reqID := uuid.UUID(created.Id)

	// The DELETER sees it, holding nothing.
	deleterQueue := incoming(t, h, identity(ctx, admin))
	if !containsRequest(deleterQueue, reqID) {
		t.Error("the deleter's queue does not contain the appeal addressed to them")
	}
	for _, it := range deleterQueue {
		if uuid.UUID(it.Id) == reqID && (it.DecidableByCaller == nil || !*it.DecidableByCaller) {
			t.Error("decidable_by_caller is false for the person the gate admits; " +
				"the flag and the gate must agree, or the UI hides a working button")
		}
	}

	// The REQUESTER does not — this is their own appeal.
	for _, it := range incoming(t, h, identity(ctx, owner)) {
		if uuid.UUID(it.Id) == reqID {
			t.Error("the requester's own appeal appeared in their decision queue")
		}
	}
}

func incoming(t *testing.T, h *requests.HTTPHandler, ctx context.Context) []openapi.ResourceRequest {
	t.Helper()
	got, err := h.ListIncomingRequests(ctx, openapi.ListIncomingRequestsRequestObject{})
	if err != nil {
		t.Fatalf("ListIncomingRequests: %v", err)
	}
	q, ok := got.(openapi.ListIncomingRequests200JSONResponse)
	if !ok {
		t.Fatalf("ListIncomingRequests returned %T, want 200", got)
	}
	return q.Items
}

// ---------------------------------------------------------------------------
// Response-type helpers
// ---------------------------------------------------------------------------

func is401(r openapi.RequestRestoreResponseObject) bool {
	_, ok := r.(openapi.RequestRestore401JSONResponse)
	return ok
}
func is404(r openapi.RequestRestoreResponseObject) bool {
	_, ok := r.(openapi.RequestRestore404JSONResponse)
	return ok
}
func is409(r openapi.RequestRestoreResponseObject) bool {
	_, ok := r.(openapi.RequestRestore409JSONResponse)
	return ok
}
func is200Decide(r openapi.DecideAdminRequestResponseObject) bool {
	_, ok := r.(openapi.DecideAdminRequest200JSONResponse)
	return ok
}
