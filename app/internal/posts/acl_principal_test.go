// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #916 — the ACL API used to accept a username and write a dead grant.
//
// `AclCreate.principal_id` is TEXT with no format and no pattern, and
// the handler stored it verbatim. The read rule compares that column
// against `$n::BIGINT::TEXT`, so a username matched nothing, forever.
// The caller got 204. The only code that noticed was notifyShare, which
// parses the same value and, on failure, logged
// `posts.acl.notify.bad_principal` and returned — after the row was
// already written.
//
// The load-bearing assertion in this file is NOT the status code. It is
// that NO ROW WAS WRITTEN: a handler that 400s after inserting would
// pass a status-only test while leaving exactly the garbage #916 is
// about.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	apAuthor  int64 = 9160001
	apGrantee int64 = 9160002
)

var apGranteePrincipal = strconv.FormatInt(apGrantee, 10)

// aclRowsFor counts post_acls rows for a post regardless of principal,
// so "wrote something we did not expect" is visible too — a count
// scoped to the principal we sent would miss a handler that wrote the
// row under a normalised or defaulted id.
func aclRowsFor(t *testing.T, h *Handler, postID uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM post_acls WHERE post_id = $1`, postID).Scan(&n); err != nil {
		t.Fatalf("count post_acls: %v", err)
	}
	return n
}

func addACL(t *testing.T, h *Handler, actor int64, postID uuid.UUID,
	principalType, principalID string,
) openapi.AddPostAclResponseObject {
	t.Helper()
	resp, err := h.AddPostAcl(
		auth.WithIdentity(t.Context(), &auth.Identity{UserRef: actor, AuthMethod: "session"}),
		openapi.AddPostAclRequestObject{
			Id: openapi_types.UUID(postID),
			Body: &openapi.AclCreate{
				PrincipalType: openapi.AclCreatePrincipalType(principalType),
				PrincipalId:   principalID,
				Permission:    openapi.AclCreatePermission("read"),
			},
		},
	)
	if err != nil {
		t.Fatalf("AddPostAcl(%s/%s): %v", principalType, principalID, err)
	}
	t.Cleanup(func() {
		_, _ = h.Pool.Exec(t.Context(), `DELETE FROM post_acls WHERE post_id=$1`, postID)
	})
	return resp
}

// The bug, stated as a test. Before the fix every one of these returned
// 204 and left a row behind.
func TestAddPostAcl_RejectsNonNumericUserPrincipal(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	fake := &fakeNotifier{}
	h.SetNotifier(fake)

	for _, bad := range []string{"alice", "", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", "12.5", "-1"} {
		t.Run("principal_id="+strconv.Quote(bad), func(t *testing.T) {
			postID := seedTierPost(t, pool, apAuthor, "explicit-share")
			resp := addACL(t, h, apAuthor, postID, "user", bad)

			if _, is := resp.(openapi.AddPostAcl400JSONResponse); !is {
				t.Fatalf("AddPostAcl returned %T, want 400 — the API accepted a "+
					"principal_id no read rule can ever match", resp)
			}
			// The half a status-only assertion would miss.
			if n := aclRowsFor(t, h, postID); n != 0 {
				t.Fatalf("post_acls holds %d rows after a rejected grant, want 0 — "+
					"the handler 400s but still writes", n)
			}
		})
	}
	if len(fake.calls) != 0 {
		t.Errorf("rejected grants produced %d notifications, want 0", len(fake.calls))
	}
}

// The other half of the fix: a valid numeric ref must still work, end
// to end. Driven as the whole loop rather than just the endpoint —
// row written, notification sent, and the grantee can actually READ the
// post — because "stopped accepting bad input" is worthless if it also
// stopped accepting good input.
func TestAddPostAcl_NumericPrincipalStillGrantsAndNotifies(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	fake := &fakeNotifier{}
	h.SetNotifier(fake)

	postID := seedTierPost(t, pool, apAuthor, "explicit-share")

	// Before: the grantee cannot read it.
	gate, err := h.postReadable(t.Context(), &auth.Identity{UserRef: apGrantee, AuthMethod: "session"}, postID)
	if err != nil {
		t.Fatalf("postReadable before grant: %v", err)
	}
	if gate {
		t.Fatal("the grantee could already read an explicit-share post with no grant; " +
			"the rest of this test would prove nothing")
	}

	resp := addACL(t, h, apAuthor, postID, "user", apGranteePrincipal)
	if _, is := resp.(openapi.AddPostAcl204Response); !is {
		t.Fatalf("AddPostAcl returned %T, want 204 — the validator rejected a valid user ref", resp)
	}
	if n := aclRowsFor(t, h, postID); n != 1 {
		t.Fatalf("post_acls holds %d rows, want 1 — the grant did not commit", n)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("the grantee was notified %d times, want 1", len(fake.calls))
	}
	if got := fake.calls[0].recipient; got != apGrantee {
		t.Errorf("notified %d, want the grantee %d", got, apGrantee)
	}

	// After: the grant is real, not merely present.
	gate, err = h.postReadable(t.Context(), &auth.Identity{UserRef: apGrantee, AuthMethod: "session"}, postID)
	if err != nil {
		t.Fatalf("postReadable after grant: %v", err)
	}
	if !gate {
		t.Error("the grantee still cannot read the post after a 204 grant — " +
			"the row was written but the read rule does not match it")
	}
}

// No read-path widening. The fix is at the write boundary; it must not
// have made anyone readable who was not readable before.
func TestAddPostAcl_NonGranteeStillCannotRead(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	h.SetNotifier(&fakeNotifier{})

	postID := seedTierPost(t, pool, apAuthor, "explicit-share")
	addACL(t, h, apAuthor, postID, "user", apGranteePrincipal)

	const stranger int64 = 9160003
	gate, err := h.postReadable(t.Context(), &auth.Identity{UserRef: stranger, AuthMethod: "session"}, postID)
	if err != nil {
		t.Fatalf("postReadable: %v", err)
	}
	if gate {
		t.Error("a user with no grant can read the post — the read path was widened")
	}
}

// An expiring grant still round-trips through the validator unchanged.
// Guards against the validation being bolted on in a way that drops the
// rest of the body.
func TestAddPostAcl_ValidatorPreservesExpiry(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	h.SetNotifier(&fakeNotifier{})

	postID := seedTierPost(t, pool, apAuthor, "explicit-share")
	exp := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	resp, err := h.AddPostAcl(
		auth.WithIdentity(t.Context(), &auth.Identity{UserRef: apAuthor, AuthMethod: "session"}),
		openapi.AddPostAclRequestObject{
			Id: openapi_types.UUID(postID),
			Body: &openapi.AclCreate{
				PrincipalType: "user",
				PrincipalId:   apGranteePrincipal,
				Permission:    "read",
				ExpiresAt:     &exp,
			},
		},
	)
	if err != nil {
		t.Fatalf("AddPostAcl: %v", err)
	}
	t.Cleanup(func() { _, _ = h.Pool.Exec(t.Context(), `DELETE FROM post_acls WHERE post_id=$1`, postID) })
	if _, is := resp.(openapi.AddPostAcl204Response); !is {
		t.Fatalf("AddPostAcl returned %T, want 204", resp)
	}
	var got *time.Time
	if err := h.Pool.QueryRow(t.Context(),
		`SELECT expires_at FROM post_acls WHERE post_id=$1`, postID).Scan(&got); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if got == nil || !got.UTC().Truncate(time.Second).Equal(exp) {
		t.Errorf("expires_at = %v, want %v", got, exp)
	}
}
