// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #875 + #876 — a share becomes discoverable, and stops disclosing the
// guest list.
//
// #667 made a share WORK: the post read rule grew a post_acls disjunct,
// so a grantee can read the post. It left two holes, and this file covers
// both:
//
//   - #875: nothing told the recipient. AddPostAcl emitted no
//     notification, and the browse feed pinned visibility to `org-only`
//     when no ?visibility= was sent (which no frontend surface sends), so
//     the shared post never entered the recipient's grid either. A share
//     only worked if the sharer ALSO sent a link out of band. The
//     notification is #875's; the grid is #1193's, which made the browse
//     default the union of the shared tiers — `explicit-share` among
//     them — so both halves of that sentence are now false.
//   - #876: gating the ACL list on "can read the post" meant a grantee
//     could enumerate the rest of the guest list, plus who granted each
//     row and when it expires.
//
// Two assertions here are load-bearing and are written to fail loudly if
// the fix regresses into a weaker shape:
//
//   - TestListPostAcls_GranteeIsRefusedButStillReadsThePost asserts BOTH
//     directions on one fixture. "The grantee gets 403" alone would pass
//     just as happily if #667 were reverted and the grantee could not
//     read the post at all — the fix would look done while the feature
//     it protects was dead.
//   - TestListPosts_GrantsReachTheDefaultFeed drives the default browse
//     response BEFORE any grant exists and again AFTER granting the same
//     caller every tier. It used to require the two to be byte-identical
//     — #875's position was that a share belongs in a notification and on
//     its own page, not in the busiest grid in the app. #1193 overturned
//     that half: the browse default is the union of the shared tiers, so
//     a granted post now appears on the wall as well. The test kept its
//     shape and reversed its expectation, because "the grant changed
//     nothing" and "the grant did not work" are indistinguishable
//     otherwise.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from the lv* set in list_visibility_test.go
// and the acl* set in acl_read_test.go. Nothing here has an FK to the
// user table, so no user rows are needed.
const (
	shAuthor    int64 = 8750001 // owns the posts and does the sharing
	shGrantee   int64 = 8750002 // the person shared with
	shStranger  int64 = 8750003 // signed in, no relationship
	shModerator int64 = 8750004 // posts.admin
)

var shGranteePrincipal = strconv.FormatInt(shGrantee, 10)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// notifyCall records one Notify invocation. Captured in full rather than
// counted, because "a notification fired" is not the claim — "the RIGHT
// person got the right verb pointing at the right post" is.
type notifyCall struct {
	recipient  int64
	actor      *int64
	verb       string
	targetKind string
	targetID   string
	payload    map[string]any
}

// fakeNotifier stands in for the notifications.Writer. `err` makes it
// fail on demand, which is how the "a notify failure must not fail the
// grant" claim is tested — the alternative (breaking the real writer) is
// not reachable from this package.
type fakeNotifier struct {
	calls []notifyCall
	err   error
}

func (f *fakeNotifier) Notify(
	_ context.Context,
	recipient int64,
	actor *int64,
	verb, targetKind, targetID string,
	payload map[string]any,
) error {
	f.calls = append(f.calls, notifyCall{
		recipient:  recipient,
		actor:      actor,
		verb:       verb,
		targetKind: targetKind,
		targetID:   targetID,
		payload:    payload,
	})
	return f.err
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// shGrantVia writes the grant through the REAL handler, which is what
// makes the notification assertions meaningful: aclGrant (acl_read_test)
// INSERTs directly and would bypass every line under test.
func shGrantVia(
	t *testing.T,
	h *Handler,
	actor int64,
	postID uuid.UUID,
	principalType, principalID string,
	expiresAt *time.Time,
) openapi.AddPostAclResponseObject {
	t.Helper()
	resp, err := h.AddPostAcl(
		auth.WithIdentity(t.Context(), lvIdentity(actor)),
		openapi.AddPostAclRequestObject{
			Id: openapi_types.UUID(postID),
			Body: &openapi.AclCreate{
				PrincipalType: openapi.AclCreatePrincipalType(principalType),
				PrincipalId:   principalID,
				Permission:    openapi.AclCreatePermission("read"),
				ExpiresAt:     expiresAt,
			},
		},
	)
	if err != nil {
		t.Fatalf("AddPostAcl(%s/%s): %v", principalType, principalID, err)
	}
	t.Cleanup(func() {
		_, _ = h.Pool.Exec(context.Background(),
			`DELETE FROM post_acls WHERE post_id=$1 AND principal_type=$2 AND principal_id=$3`,
			postID, principalType, principalID)
	})
	return resp
}

// shACLRowCount reads the table directly. The point of asking the
// database rather than the handler is that the handler is what's under
// test: "the grant committed" must not be answered by the same code path
// that might have failed to commit it.
func shACLRowCount(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID, principalID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM post_acls WHERE post_id=$1 AND principal_id=$2`,
		postID, principalID).Scan(&n); err != nil {
		t.Fatalf("count post_acls: %v", err)
	}
	return n
}

// shSharedWithMe drives the real GET /account/shared-posts handler and
// returns the ids it handed out.
func shSharedWithMe(t *testing.T, h *Handler, ref int64) map[uuid.UUID]bool {
	t.Helper()
	limit := maxListLimit
	resp, err := h.ListPostsSharedWithMe(
		auth.WithIdentity(t.Context(), lvIdentity(ref)),
		openapi.ListPostsSharedWithMeRequestObject{
			Params: openapi.ListPostsSharedWithMeParams{Limit: &limit},
		},
	)
	if err != nil {
		t.Fatalf("ListPostsSharedWithMe: %v", err)
	}
	ok, is := resp.(openapi.ListPostsSharedWithMe200JSONResponse)
	if !is {
		t.Fatalf("ListPostsSharedWithMe returned %T, want 200", resp)
	}
	out := make(map[uuid.UUID]bool, len(ok.Items))
	for _, p := range ok.Items {
		out[uuid.UUID(p.Id)] = true
	}
	return out
}

// shListAcls drives GET /posts/{id}/acls and returns the response object
// undecoded, so each caller can assert on the status it expects.
func shListAcls(t *testing.T, h *Handler, id *auth.Identity, postID uuid.UUID) openapi.ListPostAclsResponseObject {
	t.Helper()
	resp, err := h.ListPostAcls(
		auth.WithIdentity(t.Context(), id),
		openapi.ListPostAclsRequestObject{Id: openapi_types.UUID(postID)},
	)
	if err != nil {
		t.Fatalf("ListPostAcls: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// #876 — the guest list
// ---------------------------------------------------------------------------

// TestListPostAcls_GranteeIsRefusedButStillReadsThePost is the whole of
// #876 in one fixture, and the pairing is the test.
//
// A "grantee gets 403 from the ACL list" assertion on its own is not
// evidence the disclosure closed: it passes identically when the grantee
// cannot read the post at all — i.e. when #667 has been reverted, or
// when the grant simply did not take. So the same caller, in the same
// test, must still get a 200 from GET /posts/{id}. Only the guest list
// closed; the share itself is untouched.
//
// The post is at the `explicit-share` tier deliberately: it is the one
// tier where the grant is the ONLY thing granting read, so the 200 below
// cannot come from anywhere else.
func TestListPostAcls_GranteeIsRefusedButStillReadsThePost(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	aclGrant(t, pool, postID, "user", shGranteePrincipal, nil)

	grantee := lvIdentity(shGrantee)

	// Direction 1: the guest list is closed.
	if resp := shListAcls(t, h, grantee, postID); !isForbiddenACLList(resp) {
		t.Errorf("GET /posts/{id}/acls as the grantee returned %T, want 403 — "+
			"sharing a post still discloses who else holds a grant", resp)
	}

	// Direction 2: and the share still works. Without this the test
	// above would pass on a build where the grantee can read nothing.
	if !lvGetOK(t, h, grantee, openapi.Post{Id: openapi_types.UUID(postID)}) {
		t.Fatal("GET /posts/{id} refused the grantee — #876 was 'fixed' by breaking the read grant #667 shipped")
	}
	if !shSharedWithMe(t, h, shGrantee)[postID] {
		t.Error("the grantee's own 'Shared with me' surface lost the post — the ACL-list gate leaked into the read rule")
	}
}

// TestListPostAcls_ManagementCallersStillGetTheList pins the other side
// of the gate. Narrowing an endpoint is only correct if the people who
// need it kept it.
func TestListPostAcls_ManagementCallersStillGetTheList(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	aclGrant(t, pool, postID, "user", shGranteePrincipal, nil)

	for _, tc := range []struct {
		name string
		id   *auth.Identity
	}{
		{"author", lvIdentity(shAuthor)},
		{"posts.admin", lvIdentity(shModerator, CapPostsAdmin)},
		{"system.admin", lvIdentity(shModerator, CapSystemAdmin)},
	} {
		resp := shListAcls(t, h, tc.id, postID)
		rows, is := resp.(openapi.ListPostAcls200JSONResponse)
		if !is {
			t.Errorf("%s: GET /posts/{id}/acls returned %T, want 200", tc.name, resp)
			continue
		}
		if len(rows) != 1 {
			t.Errorf("%s: got %d ACL rows, want the 1 that was granted", tc.name, len(rows))
		}
	}
}

// TestListPostAcls_OrgOnlyReaderIsRefused covers the disclosure that
// predates #667 and was named in the deferral note: gating on read
// access meant ANY signed-in caller could enumerate the grants on ANY
// org-only post, because org-only is readable by every local user.
//
// canMutatePost closes that too, and it should: being signed in is not a
// management relationship with somebody else's post. The stranger's read
// access is asserted in the same breath so this cannot pass by the post
// having become invisible.
func TestListPostAcls_OrgOnlyReaderIsRefused(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	postID := seedTierPost(t, pool, shAuthor, "org-only")
	aclGrant(t, pool, postID, "user", shGranteePrincipal, nil)

	stranger := lvIdentity(shStranger)
	if !lvGetOK(t, h, stranger, openapi.Post{Id: openapi_types.UUID(postID)}) {
		t.Fatal("the org-only post is not readable by a signed-in stranger — fixture is wrong, the 403 below would be vacuous")
	}
	if resp := shListAcls(t, h, stranger, postID); !isForbiddenACLList(resp) {
		t.Errorf("GET /posts/{id}/acls as an unrelated signed-in caller returned %T, want 403", resp)
	}
}

func isForbiddenACLList(resp openapi.ListPostAclsResponseObject) bool {
	_, is := resp.(openapi.ListPostAcls403JSONResponse)
	return is
}

// ---------------------------------------------------------------------------
// #875(a) — the notification
// ---------------------------------------------------------------------------

// TestAddPostAcl_UserGrantNotifiesTheGrantee: one grant, one
// notification, addressed and targeted correctly.
func TestAddPostAcl_UserGrantNotifiesTheGrantee(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	fake := &fakeNotifier{}
	h.SetNotifier(fake)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	shGrantVia(t, h, shAuthor, postID, "user", shGranteePrincipal, nil)

	if len(fake.calls) != 1 {
		t.Fatalf("granting one user %d notifications, want exactly 1", len(fake.calls))
	}
	c := fake.calls[0]
	if c.recipient != shGrantee {
		t.Errorf("notified %d, want the grantee %d", c.recipient, shGrantee)
	}
	if c.actor == nil || *c.actor != shAuthor {
		t.Errorf("actor = %v, want the sharer %d", c.actor, shAuthor)
	}
	if c.verb != notifications.VerbPostSharedWithMe {
		t.Errorf("verb = %q, want %q", c.verb, notifications.VerbPostSharedWithMe)
	}
	// target_kind + target_id are what makes the bell clickable: the
	// inbox routes `post` targets to /posts/{target_id}. A notification
	// that says "someone shared a post" without saying WHICH is barely
	// better than silence.
	if c.targetKind != notifications.TargetKindPost {
		t.Errorf("target_kind = %q, want %q", c.targetKind, notifications.TargetKindPost)
	}
	if c.targetID != postID.String() {
		t.Errorf("target_id = %q, want the shared post %s", c.targetID, postID)
	}
	if c.payload[notifications.PayloadKeyPostTitle] == nil {
		t.Error("payload carries no post_title — the inbox card renders the verb with no subject")
	}
}

// TestAddPostAcl_RoleAndTeamGrantsAreRefused.
//
// BEHAVIOUR CHANGE (#916). This test previously asserted the opposite:
// that a role or team grant on a post is STORED and merely notifies
// nobody, on the reasoning that "notifies nobody is not licence to skip
// the grant". That reasoning was about the NOTIFY path — it is correct
// that a notifier failure must not roll back a real grant — but it was
// applied to a grant that was never real.
//
// A role or team grant on a post confers nothing and cannot come to
// confer anything without a schema-level change: both content read
// rules gate on `principal_type = 'user'` before they ever look at
// principal_id (visibility.PostLiveGrantSQL, the collection predicate),
// because role/team scoping on content is ADR 0010 Layer 5 and
// unimplemented. So the row was written, matched by nothing, and
// reported to the caller as 204.
//
// That is the SAME defect #916 is about — the API accepting a
// reference it knows confers no access and answering "no content" as
// though it had worked. Fixing it for usernames but not for role and
// team would be fixing half a bug. Pre-release there are no stored
// grants to preserve, and a 400 is trivially reversible where a table
// full of inert rows is not.
//
// Reverse this and the assertion below by deleting the validator call
// in AddPostAcl, if the decision goes the other way.
func TestAddPostAcl_RoleAndTeamGrantsAreRefused(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	fake := &fakeNotifier{}
	h.SetNotifier(fake)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	roleResp := shGrantVia(t, h, shAuthor, postID, "role", shGranteePrincipal, nil)
	teamResp := shGrantVia(t, h, shAuthor, postID, "team", shGranteePrincipal, nil)

	if _, is := roleResp.(openapi.AddPostAcl400JSONResponse); !is {
		t.Errorf("role grant returned %T, want 400 — a grant that confers nothing "+
			"must not be reported as success", roleResp)
	}
	if _, is := teamResp.(openapi.AddPostAcl400JSONResponse); !is {
		t.Errorf("team grant returned %T, want 400", teamResp)
	}
	if len(fake.calls) != 0 {
		t.Errorf("role/team grants produced %d notifications, want 0: %+v", len(fake.calls), fake.calls)
	}
	// The point of the fix: nothing was written. A status-only
	// assertion would pass on a handler that 400s AFTER inserting.
	if n := shACLRowCount(t, pool, postID, shGranteePrincipal); n != 0 {
		t.Errorf("post_acls holds %d rows after two refused grants, want 0 — "+
			"the handler rejected the request but still wrote", n)
	}
}

// TestAddPostAcl_NotifyFailureDoesNotFailTheGrant.
//
// The grant is the user's action and it has already committed by the
// time the notifier runs. Surfacing a notifier error would tell the
// author their share failed when the row is sitting in the table — and
// worse, invite a retry that writes a duplicate.
//
// The commit is verified by reading post_acls directly, not by trusting
// the 204: a handler that returned 204 without writing would pass a
// response-code-only assertion.
func TestAddPostAcl_NotifyFailureDoesNotFailTheGrant(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	fake := &fakeNotifier{err: errors.New("notification backend is down")}
	h.SetNotifier(fake)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	resp := shGrantVia(t, h, shAuthor, postID, "user", shGranteePrincipal, nil)

	if _, is := resp.(openapi.AddPostAcl204Response); !is {
		t.Fatalf("AddPostAcl returned %T, want 204 — a notifier error was turned into a failed share", resp)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("the notifier was called %d times, want 1 — the failure path was never exercised", len(fake.calls))
	}
	if n := shACLRowCount(t, pool, postID, shGranteePrincipal); n != 1 {
		t.Fatalf("post_acls holds %d rows, want 1 — the grant did not commit", n)
	}
	// And the grant is real, not merely present: the grantee can read.
	if !lvGetOK(t, h, lvIdentity(shGrantee), openapi.Post{Id: openapi_types.UUID(postID)}) {
		t.Error("the grantee cannot read the post the failed-notify grant wrote")
	}
}

// TestAddPostAcl_UnwiredNotifierStillGrants. nil-safe wiring, asserted
// rather than assumed — every other cross-package seam on this handler
// is nil in tests, and a nil-deref here would take out sharing entirely
// on any boot-order slip.
func TestAddPostAcl_UnwiredNotifierStillGrants(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool) // no SetNotifier

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	shGrantVia(t, h, shAuthor, postID, "user", shGranteePrincipal, nil)

	if n := shACLRowCount(t, pool, postID, shGranteePrincipal); n != 1 {
		t.Errorf("post_acls holds %d rows, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// #875(b) — the "Shared with me" surface
// ---------------------------------------------------------------------------

// TestSharedWithMe_LiveGrantAppearsExpiredDoesNot pins expiry in both
// directions on ONE fixture, for the same reason acl_read_test.go does:
// "an expired grant does not appear" passes just as happily when the
// surface returns nothing at all — when the query is broken, when
// principal_id never matches, when the endpoint 500s into an empty page.
// Pairing it with a future-dated grant on an otherwise identical post
// makes the pair discriminate. Delete the `expires_at` clause from
// visibility.PostLiveGrantSQL and the `past` row starts appearing;
// break the query and the `future` row stops.
func TestSharedWithMe_LiveGrantAppearsExpiredDoesNot(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	past := seedTierPost(t, pool, shAuthor, "explicit-share")
	future := seedTierPost(t, pool, shAuthor, "explicit-share")
	never := seedTierPost(t, pool, shAuthor, "explicit-share")
	// A fourth post at the same tier by the same author with NO grant,
	// so "the surface returned things" can never be satisfied by a query
	// that forgot the ACL predicate and listed the tier instead.
	ungranted := seedTierPost(t, pool, shAuthor, "explicit-share")

	yesterday := time.Now().Add(-24 * time.Hour)
	tomorrow := time.Now().Add(24 * time.Hour)
	aclGrant(t, pool, past, "user", shGranteePrincipal, &yesterday)
	aclGrant(t, pool, future, "user", shGranteePrincipal, &tomorrow)
	aclGrant(t, pool, never, "user", shGranteePrincipal, nil)

	shared := shSharedWithMe(t, h, shGrantee)
	for _, tc := range []struct {
		label  string
		postID uuid.UUID
		want   bool
	}{
		{"expires_at in the past", past, false},
		{"expires_at in the future", future, true},
		{"expires_at NULL", never, true},
		{"no grant at all", ungranted, false},
	} {
		if got := shared[tc.postID]; got != tc.want {
			t.Errorf("shared-with-me, %s: listed=%v want %v", tc.label, got, tc.want)
		}
	}
}

// TestSharedWithMe_RevokedGrantDisappears — the surface must track the
// table, not a snapshot of it. Asserted through the real revoke handler
// so the invalidation path is included.
func TestSharedWithMe_RevokedGrantDisappears(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	kept := seedTierPost(t, pool, shAuthor, "explicit-share")
	shGrantVia(t, h, shAuthor, postID, "user", shGranteePrincipal, nil)
	shGrantVia(t, h, shAuthor, kept, "user", shGranteePrincipal, nil)

	if !shSharedWithMe(t, h, shGrantee)[postID] {
		t.Fatal("the grant never appeared, so its disappearance proves nothing")
	}

	resp, err := h.RemovePostAcl(
		auth.WithIdentity(t.Context(), lvIdentity(shAuthor)),
		openapi.RemovePostAclRequestObject{
			Id:            openapi_types.UUID(postID),
			PrincipalType: "user",
			PrincipalId:   shGranteePrincipal,
			Permission:    "read",
		},
	)
	if err != nil {
		t.Fatalf("RemovePostAcl: %v", err)
	}
	if _, is := resp.(openapi.RemovePostAcl204Response); !is {
		t.Fatalf("RemovePostAcl returned %T, want 204", resp)
	}

	after := shSharedWithMe(t, h, shGrantee)
	if after[postID] {
		t.Error("a revoked grant is still on the 'Shared with me' surface")
	}
	// The second grant proves the revoke was surgical rather than the
	// surface having gone blank.
	if !after[kept] {
		t.Error("revoking one grant emptied the whole surface")
	}
}

// TestSharedWithMe_IsPerCaller — the grantee's shares are not everyone's
// shares. A missing principal predicate would hand every grant in the
// table to every caller, and every other test in this file would still
// pass.
func TestSharedWithMe_IsPerCaller(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	aclGrant(t, pool, postID, "user", shGranteePrincipal, nil)

	if shSharedWithMe(t, h, shStranger)[postID] {
		t.Error("a post granted to someone else appeared on the stranger's 'Shared with me'")
	}
	if !shSharedWithMe(t, h, shGrantee)[postID] {
		t.Error("the grantee's own share is missing — the fixture or the query is wrong")
	}
}

// TestSharedWithMe_SoftDeletedPostsAreNotShares. A deleted post is not
// shared content; the grant row outlives the post (no FK cascade fires
// on a soft delete) and would otherwise resurrect it here.
func TestSharedWithMe_SoftDeletedPostsAreNotShares(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	postID := seedTierPost(t, pool, shAuthor, "explicit-share")
	live := seedTierPost(t, pool, shAuthor, "explicit-share")
	aclGrant(t, pool, postID, "user", shGranteePrincipal, nil)
	aclGrant(t, pool, live, "user", shGranteePrincipal, nil)

	if !shSharedWithMe(t, h, shGrantee)[postID] {
		t.Fatal("the grant never appeared, so its disappearance proves nothing")
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE posts SET deleted_at = NOW() WHERE id = $1`, postID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	after := shSharedWithMe(t, h, shGrantee)
	if after[postID] {
		t.Error("a soft-deleted post is still listed as shared with the caller")
	}
	if !after[live] {
		t.Error("the surviving share vanished too — the filter is too wide")
	}
}

// TestSharedWithMe_AnonymousIsRefused. The surface is defined entirely
// by "who is asking"; there is no caller-supplied ref to fall back on,
// and a nil identity must 401 rather than reach the query with a zero
// ref (no real user ref is 0, but a grant row whose principal_id is "0"
// would then be handed to every anonymous request).
func TestSharedWithMe_AnonymousIsRefused(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	resp, err := h.ListPostsSharedWithMe(t.Context(), openapi.ListPostsSharedWithMeRequestObject{})
	if err != nil {
		t.Fatalf("ListPostsSharedWithMe: %v", err)
	}
	if _, is := resp.(openapi.ListPostsSharedWithMe401JSONResponse); !is {
		t.Errorf("anonymous caller got %T, want 401", resp)
	}
}

// ---------------------------------------------------------------------------
// Where a grant shows up (#875, amended by #1193)
// ---------------------------------------------------------------------------

// TestListPosts_GrantsReachTheDefaultFeed.
//
// #875 answered "a share is invisible" with a notification and a page of
// its own, and deliberately NOT by widening the browse feed: `GET /posts`
// with no query string meant "the org-only tier", full stop.
//
// #1193 overturned the tier default for an unrelated and much larger
// reason — a member's wall excluded every PUBLIC post on the instance —
// and the union it replaced org-only with contains `explicit-share`. So
// a granted post now does reach the wall, and this test asserts the new
// contract in both directions on one fixture:
//
//   - before any grant, the grantee's default feed has the org-only post
//     and NOT the explicit-share one. Without this the "after" assertion
//     would pass on a feed that never filtered at all.
//   - after granting every tier, the explicit-share, followers and
//     public posts are all on it, because the read rule's ACL disjunct
//     admits them and the display filter no longer subtracts them.
//   - `private` stays off it even fully granted. That is the half of the
//     old default #1193 kept, and it is the one assertion here that a
//     "just delete the filter" implementation fails.
//
// The grants are separately proved through the shared-with-me surface,
// so a build where AddPostAcl wrote nothing fails as a setup error
// rather than passing the negative assertions by accident.
func TestListPosts_GrantsReachTheDefaultFeed(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	ids := make(map[string]uuid.UUID, len(aclTiers))
	for _, tier := range aclTiers {
		ids[tier] = seedTierPost(t, pool, shAuthor, tier)
	}

	// The default feed: no visibility, no author, no filters at all —
	// exactly what the browse page sends.
	feed := func() map[uuid.UUID]bool {
		t.Helper()
		resp, err := h.ListPosts(
			auth.WithIdentity(t.Context(), lvIdentity(shGrantee)),
			openapi.ListPostsRequestObject{},
		)
		if err != nil {
			t.Fatalf("ListPosts: %v", err)
		}
		ok, is := resp.(openapi.ListPosts200JSONResponse)
		if !is {
			t.Fatalf("ListPosts returned %T, want 200", resp)
		}
		out := make(map[uuid.UUID]bool, len(ok.Items))
		for _, p := range ok.Items {
			out[uuid.UUID(p.Id)] = true
		}
		return out
	}

	before := feed()
	if !before[ids["org-only"]] {
		t.Fatal("the ungranted default feed does not contain the org-only fixture post — " +
			"the assertions below would be vacuous")
	}
	if before[ids["explicit-share"]] {
		t.Fatal("the explicit-share post is on the feed BEFORE any grant exists — the " +
			"fixture proves nothing about what a grant does")
	}

	for _, tier := range aclTiers {
		aclGrant(t, pool, ids[tier], "user", shGranteePrincipal, nil)
	}

	// The grants are real.
	shared := shSharedWithMe(t, h, shGrantee)
	for _, tier := range aclTiers {
		if !shared[ids[tier]] {
			t.Fatalf("the %s grant did not take — the feed comparison below proves nothing", tier)
		}
	}

	after := feed()
	for _, tier := range []string{"public", "org-only", "followers", "explicit-share"} {
		if !after[ids[tier]] {
			t.Errorf("a granted %s post is absent from the grantee's default wall — the "+
				"display filter is narrower than the read rule (#1193)", tier)
		}
	}
	if after[ids["private"]] {
		t.Error("a granted PRIVATE post reached the browse wall. The default filter is " +
			"the union of the SHARED tiers; dropping it entirely puts a moderator's " +
			"wall full of other people's drafts")
	}
}
