// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #881 — the request-access loop, driven end to end.
//
// One sequence, not four disconnected assertions:
//
//	a viewer who cannot read a restricted asset submits a request
//	  → the OWNER is notified
//	  → the owner finds it in their own queue
//	  → the owner decides it WITHOUT holding share.grant
//	  → and the requester still cannot read the asset.
//
// That last step is deliberate and is pinned here on purpose. See
// TestAccessLoop_GrantDoesNotWidenReads.
//
// Real Postgres (skipped without AA_DB_PASSWORD), same as the sibling
// lifecycle tests. These build the HTTP layer rather than only the
// domain handler, because the ownership disjunct IS an HTTP-layer gate
// and a domain-only test would assert nothing about it.

package requests_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/requests"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

func TestAccessLoop_AskNotifiesTheOwnerWhoCanThenDecide(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	viewer := seedUserForRequests(t, pool)
	assetID := seedRestrictedAsset(t, pool, owner)

	noter := newNoter()
	dom := newHandlerE(t, pool, newAuditRec(), noter)
	h := requests.NewHTTPHandler(dom, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The premise the whole feature rests on: this viewer genuinely
	// cannot read the asset. Without this, everything below would be
	// a workflow around a restriction that does not exist — the
	// "can production reach this?" failure that killed IIIF and EXIF.
	if readable(t, pool, assetID, viewer) {
		t.Fatal("fixture is not restricted: the viewer can already read the asset, so there is nothing to request")
	}

	// --- 1. The ask, from the placeholder. No capability in the body:
	// the button never names one, and the server stamps the marker.
	resp, err := h.RequestAssetAccess(identity(ctx, viewer), openapi.RequestAssetAccessRequestObject{
		Id:   openapi_types.UUID(assetID),
		Body: &openapi.RequestAssetAccessJSONRequestBody{Reason: strptr("for the zine layout")},
	})
	if err != nil {
		t.Fatalf("RequestAssetAccess: %v", err)
	}
	created, ok := resp.(openapi.RequestAssetAccess201JSONResponse)
	if !ok {
		t.Fatalf("first ask returned %T, want 201", resp)
	}
	if created.RequestedCapability != requests.CapAccessRequest {
		t.Errorf("requested_capability = %q, want the inert marker %q",
			created.RequestedCapability, requests.CapAccessRequest)
	}
	requestID := uuid.UUID(created.Id)

	// --- 2. The owner was told. Before #881 nothing fired here at all.
	sent := noter.withVerb("resource_request_received_to_approve")
	if len(sent) == 0 {
		t.Fatal("nobody was notified that a request arrived; the approver queue fills in silence")
	}
	var ownerNote *notifiedCall
	for i := range sent {
		if sent[i].Recipient == owner {
			ownerNote = &sent[i]
		}
		if sent[i].Recipient == viewer {
			t.Error("the requester was notified of their own request")
		}
	}
	if ownerNote == nil {
		t.Fatalf("the asset's owner (ref %d) was not among the %d recipients", owner, len(sent))
	}
	if ownerNote.TargetKnd != "request" || ownerNote.TargetID != requestID.String() {
		t.Errorf("notification target = (%q, %q), want (request, %s)",
			ownerNote.TargetKnd, ownerNote.TargetID, requestID)
	}

	// --- 3. The owner finds it in a queue they can actually reach.
	// NOT /admin/requests — this identity holds nothing.
	inbox, err := h.ListIncomingRequests(identity(ctx, owner), openapi.ListIncomingRequestsRequestObject{})
	if err != nil {
		t.Fatalf("ListIncomingRequests: %v", err)
	}
	queue, ok := inbox.(openapi.ListIncomingRequests200JSONResponse)
	if !ok {
		t.Fatalf("ListIncomingRequests returned %T, want 200", inbox)
	}
	if !containsRequest(queue.Items, requestID) {
		t.Fatalf("the owner's queue does not contain the request against their own asset")
	}
	// And it is THEIR queue, not everyone's: a bystander sees nothing.
	other := seedUserForRequests(t, pool)
	otherInbox, _ := h.ListIncomingRequests(identity(ctx, other), openapi.ListIncomingRequestsRequestObject{})
	if q, ok := otherInbox.(openapi.ListIncomingRequests200JSONResponse); ok && containsRequest(q.Items, requestID) {
		t.Error("a user who owns nothing sees another owner's incoming request")
	}

	// --- 4. The owner decides, holding no capability at all.
	decided, err := h.DecideAdminRequest(identity(ctx, owner), openapi.DecideAdminRequestRequestObject{
		Id:   openapi_types.UUID(requestID),
		Body: &openapi.DecideAdminRequestJSONRequestBody{Decision: openapi.Granted},
	})
	if err != nil {
		t.Fatalf("DecideAdminRequest as owner: %v", err)
	}
	granted, ok := decided.(openapi.DecideAdminRequest200JSONResponse)
	if !ok {
		t.Fatalf("owner decide returned %T, want 200 — the owner cannot decide a request on their own asset", decided)
	}
	if granted.State != openapi.ResourceRequestState("granted") {
		t.Errorf("state = %q, want granted", granted.State)
	}

	// --- 5. …and the requester STILL cannot read the asset.
	if readable(t, pool, assetID, viewer) {
		t.Error("a granted request made the asset readable; #912 is the change that is allowed to do that")
	}
}

// TestAccessLoop_OwnerDisjunct_RedFirst is the red-first half of the
// gate change, expressed as the two facts that must BOTH hold:
//
//   - a non-owner without share.grant is refused (what the gate used to
//     do to everyone, including the owner)
//   - the owner is admitted (what changed)
//
// Run against the pre-#881 gate, the second case fails. That is the
// point: without it, "the owner may decide" is a claim no test makes.
func TestAccessLoop_OwnerDisjunct_RedFirst(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	viewer := seedUserForRequests(t, pool)
	stranger := seedUserForRequests(t, pool)
	assetID := seedRestrictedAsset(t, pool, owner)

	dom := newHandlerE(t, pool, newAuditRec(), newNoter())
	h := requests.NewHTTPHandler(dom, slog.New(slog.NewTextHandler(io.Discard, nil)))

	requestID := ask(t, h, ctx, viewer, assetID)

	// A stranger with no capability: refused. Ownership is the gate,
	// not "being signed in".
	got, err := h.DecideAdminRequest(identity(ctx, stranger), openapi.DecideAdminRequestRequestObject{
		Id:   openapi_types.UUID(requestID),
		Body: &openapi.DecideAdminRequestJSONRequestBody{Decision: openapi.Granted},
	})
	if err != nil {
		t.Fatalf("DecideAdminRequest as stranger: %v", err)
	}
	if _, forbidden := got.(openapi.DecideAdminRequest403JSONResponse); !forbidden {
		t.Errorf("stranger decide returned %T, want 403", got)
	}

	// The owner, still holding nothing: admitted.
	got, err = h.DecideAdminRequest(identity(ctx, owner), openapi.DecideAdminRequestRequestObject{
		Id:   openapi_types.UUID(requestID),
		Body: &openapi.DecideAdminRequestJSONRequestBody{Decision: openapi.Denied},
	})
	if err != nil {
		t.Fatalf("DecideAdminRequest as owner: %v", err)
	}
	if _, okResp := got.(openapi.DecideAdminRequest200JSONResponse); !okResp {
		t.Errorf("owner decide returned %T, want 200", got)
	}
}

// TestAccessLoop_OwnerMayNotDecideAnArbitraryCapability is the reason
// the owner disjunct is capability-scoped.
//
// requested_capability is requester-controlled input (ADR 0064). If an
// owner could decide ANY request against their asset, any account could
// submit `system.admin` against a stranger's artwork and talk the artist
// into granting it from a panel that looks like it is about a picture.
// Widening the gate without this restriction would have shipped a
// privilege-escalation route, not a workflow.
func TestAccessLoop_OwnerMayNotDecideAnArbitraryCapability(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	attacker := seedUserForRequests(t, pool)
	assetID := seedRestrictedAsset(t, pool, owner)

	dom := newHandlerE(t, pool, newAuditRec(), newNoter())
	h := requests.NewHTTPHandler(dom, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The attacker names the wildcard explicitly. The endpoint accepts
	// the submission — any authenticated user may ask for anything, and
	// that is 1.17.E's documented behaviour.
	resp, err := h.RequestAssetAccess(identity(ctx, attacker), openapi.RequestAssetAccessRequestObject{
		Id: openapi_types.UUID(assetID),
		Body: &openapi.RequestAssetAccessJSONRequestBody{
			Capability: strptr("system.admin"),
			Reason:     strptr("please approve, it is just for the artwork"),
		},
	})
	if err != nil {
		t.Fatalf("RequestAssetAccess: %v", err)
	}
	created, ok := resp.(openapi.RequestAssetAccess201JSONResponse)
	if !ok {
		t.Fatalf("submit returned %T, want 201", resp)
	}

	// What must NOT happen: the owner being able to say yes to it.
	got, err := h.DecideAdminRequest(identity(ctx, owner), openapi.DecideAdminRequestRequestObject{
		Id:   created.Id,
		Body: &openapi.DecideAdminRequestJSONRequestBody{Decision: openapi.Granted},
	})
	if err != nil {
		t.Fatalf("DecideAdminRequest: %v", err)
	}
	if _, forbidden := got.(openapi.DecideAdminRequest403JSONResponse); !forbidden {
		t.Fatalf("an asset owner was allowed to grant %q: got %T, want 403",
			"system.admin", got)
	}

	// And no grant landed.
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM user_capability_grants WHERE user_ref = $1 AND capability_code = 'system.admin'`,
		attacker,
	).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if n != 0 {
		t.Errorf("system.admin grants for the attacker = %d, want 0", n)
	}
}

// TestAccessLoop_GrantDoesNotWidenReads pins the boundary of this
// sprint, deliberately.
//
// Approving a request does not unlock the asset and CANNOT with the
// schema as it stands: user_capability_grants has no per-object scope
// (its narrowing column is team_id), and visibility.ContentReadable
// consults exactly system.admin and content.read.all. So a grant is
// either too narrow to do anything or wide enough to open every
// restricted asset on the instance — neither is "you may view this one".
//
// That is ADR 0064's documented deferral and is tracked as #912. This
// test exists so the day someone wires the marker capability into a read
// gate, it fails here rather than silently opening a catalogue.
func TestAccessLoop_GrantDoesNotWidenReads(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	viewer := seedUserForRequests(t, pool)
	assetID := seedRestrictedAsset(t, pool, owner)

	dom := newHandlerE(t, pool, newAuditRec(), newNoter())
	h := requests.NewHTTPHandler(dom, slog.New(slog.NewTextHandler(io.Discard, nil)))

	requestID := ask(t, h, ctx, viewer, assetID)
	if _, err := h.DecideAdminRequest(identity(ctx, owner), openapi.DecideAdminRequestRequestObject{
		Id:   openapi_types.UUID(requestID),
		Body: &openapi.DecideAdminRequestJSONRequestBody{Decision: openapi.Granted},
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// The grant row exists…
	var grantedCap string
	if err := pool.QueryRow(ctx,
		`SELECT capability_code FROM user_capability_grants WHERE request_ref = $1`, requestID,
	).Scan(&grantedCap); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if grantedCap != requests.CapAccessRequest {
		t.Errorf("granted capability = %q, want %q", grantedCap, requests.CapAccessRequest)
	}

	// …and confers nothing. If this assertion ever fails, the change
	// that broke it is #912's work and needs #912's design decisions —
	// chiefly which capabilities a user may REQUEST, which ADR 0064
	// says is the question the grant path is blocked on.
	if readable(t, pool, assetID, viewer) {
		t.Fatal("the granted marker capability made a restricted asset readable — see #912 and ADR 0064")
	}
}

// TestAccessLoop_RepeatAskCoalesces covers the duplicate-request rule.
func TestAccessLoop_RepeatAskCoalesces(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	viewer := seedUserForRequests(t, pool)
	assetID := seedRestrictedAsset(t, pool, owner)

	noter := newNoter()
	dom := newHandlerE(t, pool, newAuditRec(), noter)
	h := requests.NewHTTPHandler(dom, slog.New(slog.NewTextHandler(io.Discard, nil)))

	first := ask(t, h, ctx, viewer, assetID)

	// Same ask again: 200 with the SAME row, and no second ping.
	before := len(noter.withVerb("resource_request_received_to_approve"))
	resp, err := h.RequestAssetAccess(identity(ctx, viewer), openapi.RequestAssetAccessRequestObject{
		Id:   openapi_types.UUID(assetID),
		Body: &openapi.RequestAssetAccessJSONRequestBody{},
	})
	if err != nil {
		t.Fatalf("second ask: %v", err)
	}
	same, ok := resp.(openapi.RequestAssetAccess200JSONResponse)
	if !ok {
		t.Fatalf("second ask returned %T, want 200 (the existing request)", resp)
	}
	if uuid.UUID(same.Id) != first {
		t.Errorf("second ask created a new request %s; want the existing %s", uuid.UUID(same.Id), first)
	}
	if after := len(noter.withVerb("resource_request_received_to_approve")); after != before {
		t.Errorf("approvers were re-notified for a repeat ask (%d → %d)", before, after)
	}

	// Denial is terminal for the ROW, not for the person: after a no,
	// asking again files a NEW request. Anything else would turn one
	// refusal into a permanent one with nothing telling the user so.
	if _, err := h.DecideAdminRequest(identity(ctx, owner), openapi.DecideAdminRequestRequestObject{
		Id:   openapi_types.UUID(first),
		Body: &openapi.DecideAdminRequestJSONRequestBody{Decision: openapi.Denied},
	}); err != nil {
		t.Fatalf("deny: %v", err)
	}
	third, err := h.RequestAssetAccess(identity(ctx, viewer), openapi.RequestAssetAccessRequestObject{
		Id:   openapi_types.UUID(assetID),
		Body: &openapi.RequestAssetAccessJSONRequestBody{},
	})
	if err != nil {
		t.Fatalf("ask after denial: %v", err)
	}
	fresh, ok := third.(openapi.RequestAssetAccess201JSONResponse)
	if !ok {
		t.Fatalf("ask after denial returned %T, want 201 — a denial must not silence the requester forever", third)
	}
	if uuid.UUID(fresh.Id) == first {
		t.Error("ask after denial resurrected the denied row; the state machine forbids that")
	}
}

// TestAccessLoop_NotificationPayloadIsAllowListed holds the create-time
// notification to the same discipline as the placeholder itself.
//
// An ALLOW-LIST, not a deny-list. The requester may not see this
// asset's title, and the notification about their request must not
// carry one either — nor a filename, nor the free-text reason. A
// deny-list ("assert no `title` key") passes on the day someone adds
// `asset_title`, which is exactly how the withheld-payload defects in
// #892 and #899 happened.
func TestAccessLoop_NotificationPayloadIsAllowListed(t *testing.T) {
	pool := openPoolE(t)
	defer cleanupRequests(t, pool)
	ctx := context.Background()

	owner := seedUserForRequests(t, pool)
	viewer := seedUserForRequests(t, pool)
	assetID := seedRestrictedAsset(t, pool, owner)

	noter := newNoter()
	dom := newHandlerE(t, pool, newAuditRec(), noter)
	h := requests.NewHTTPHandler(dom, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := h.RequestAssetAccess(identity(ctx, viewer), openapi.RequestAssetAccessRequestObject{
		Id: openapi_types.UUID(assetID),
		Body: &openapi.RequestAssetAccessJSONRequestBody{
			Reason: strptr("secret-reason-marker"),
		},
	}); err != nil {
		t.Fatalf("RequestAssetAccess: %v", err)
	}

	permitted := map[string]bool{
		"request_id": true, // the thing to act on
		"capability": true, // what was asked for
		"asset_id":   true, // already in the placeholder's allow-list (#899)
	}
	sent := noter.withVerb("resource_request_received_to_approve")
	if len(sent) == 0 {
		t.Fatal("no create-time notification to inspect")
	}
	for _, c := range sent {
		raw, err := json.Marshal(c.Payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		for k := range m {
			if !permitted[k] {
				t.Errorf("notification payload carries %q, which is not permitted "+
					"(allowed: request_id, capability, asset_id). Payload: %s", k, raw)
			}
		}
		// The reason is free text the REQUESTER wrote. It is fine in the
		// queue, where the per-row gate applies; it is not fine in a
		// notification that fans out to every approver on the instance.
		if string(raw) != "" && containsSubstring(string(raw), "secret-reason-marker") {
			t.Errorf("the requester's free-text reason reached the notification payload: %s", raw)
		}
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// seedRestrictedAsset inserts an asset owned by ownerRef at a
// sensitivity tier no other user can read.
//
// 'restricted' is a real tier in ADR 0020's vocabulary, and
// visibility.ContentReadable admits only owner / public / team — so a
// restricted asset with no team is readable by its owner alone. The
// fixture is checked against the real predicate in the test body rather
// than assumed, because a fixture that is not actually restricted would
// make every assertion below vacuous.
func seedRestrictedAsset(t *testing.T, pool *pgxpool.Pool, ownerRef int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	// asset_type FKs asset_types(ref); resolve a real one rather than
	// hardcoding a number the seed is free to change.
	if err := pool.QueryRow(ctx,
		`INSERT INTO assets (title, asset_type, owner_user_ref, sensitivity, status)
		 VALUES ($1, (SELECT MIN(ref) FROM asset_types), $2, 'restricted', 'active')
		 RETURNING id`,
		"rq-restricted-"+uuid.New().String()[:8], ownerRef,
	).Scan(&id); err != nil {
		t.Fatalf("seed restricted asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

// readable answers the real question — may this caller receive this
// asset's bytes — through the production predicate, not a re-statement
// of it.
func readable(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID, callerRef int64) bool {
	t.Helper()
	ok, err := visibility.CanReadContent(context.Background(), pool,
		visibility.NewCaller(&callerRef), visibility.ContentCaps{}.Checker(), assetID)
	if err != nil {
		t.Fatalf("CanReadContent: %v", err)
	}
	return ok
}

// identity puts a capability-less signed-in user on the context. Every
// caller in this file is deliberately plain: the point of #881 is what
// someone holding NOTHING can do.
func identity(ctx context.Context, ref int64) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{UserRef: ref, AuthMethod: "session"})
}

// ask submits the placeholder's request and returns the new id.
func ask(t *testing.T, h *requests.HTTPHandler, ctx context.Context, viewer int64, assetID uuid.UUID) uuid.UUID {
	t.Helper()
	resp, err := h.RequestAssetAccess(identity(ctx, viewer), openapi.RequestAssetAccessRequestObject{
		Id:   openapi_types.UUID(assetID),
		Body: &openapi.RequestAssetAccessJSONRequestBody{},
	})
	if err != nil {
		t.Fatalf("RequestAssetAccess: %v", err)
	}
	created, ok := resp.(openapi.RequestAssetAccess201JSONResponse)
	if !ok {
		t.Fatalf("RequestAssetAccess returned %T, want 201", resp)
	}
	return uuid.UUID(created.Id)
}

func containsRequest(items []openapi.ResourceRequest, id uuid.UUID) bool {
	for _, it := range items {
		if uuid.UUID(it.Id) == id {
			return true
		}
	}
	return false
}

func containsSubstring(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}

func strptr(s string) *string { return &s }
