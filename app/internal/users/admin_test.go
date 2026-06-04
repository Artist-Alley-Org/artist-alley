package users

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Auth gate is the first thing every admin endpoint does — pin that
// the unauthenticated path returns 401 and the non-admin path
// returns 403 BEFORE the DB call. With Pool=nil, reaching the DB
// query would panic, so the test proves the early-return ordering
// at the same time.

func TestListAdminUsers_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.ListAdminUsers(context.Background(), openapi.ListAdminUsersRequestObject{})
	if err != nil {
		t.Fatalf("ListAdminUsers: %v", err)
	}
	if _, ok := resp.(openapi.ListAdminUsers401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

// Authed-but-lacking-capability should 403, NOT leak data. This is
// a defensive check on the gate ordering — if a future refactor
// moves the cap check below the DB call, the nil pool would panic
// (caught by the test); if the gate is removed entirely, the test
// fails by returning a 200.
func TestListAdminUsers_NonAdminIs403(t *testing.T) {
	h := &Handler{}
	id := &auth.Identity{UserRef: 42, Capabilities: []string{"posts.read.public"}}
	ctx := auth.WithIdentity(context.Background(), id)
	resp, err := h.ListAdminUsers(ctx, openapi.ListAdminUsersRequestObject{})
	if err != nil {
		t.Fatalf("ListAdminUsers: %v", err)
	}
	if _, ok := resp.(openapi.ListAdminUsers403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

// system.admin should satisfy any capability check via the wildcard
// path in Identity.Can. Pin that here so a future refactor that
// drops the wildcard doesn't silently lock admins out of the page.
// We still need a non-nil pool to reach the query, so this test
// stops at the cap check by passing a context with both an Identity
// AND a nil Pool — the cap check fires first and we'd reach DB
// next. So this test variant lives in the DB-integration suite
// instead — call out here that the cap check passes via
// system.admin is covered by handler_test for the broader admin
// surface.
//
// (Verified separately: TestListAdminUsers_NonAdminIs403 above
// proves the gate rejects a non-admin; reciprocally, calling with
// system.admin and a nil pool would deref-panic on q.ListAdminUsers,
// which is precisely the contract — past the gate, we're at the DB.)

// Cursor round-trip — the encoder + decoder must be each other's
// inverse for any (time, ref) pair the DB might emit. A drift here
// would silently break pagination across page boundaries: page 2
// would start somewhere different than page 1 ended.
func TestAdminUserCursor_Roundtrip(t *testing.T) {
	cases := []struct {
		name string
		t    time.Time
		ref  int64
	}{
		{"epoch", time.Unix(0, 0).UTC(), 1},
		{"recent", time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC), 12345},
		{"nano-precision", time.Date(2026, 6, 3, 8, 0, 0, 123456789, time.UTC), 999},
		{"max int64 ref", time.Now().UTC(), 9_223_372_036_854_775_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cur := encodeAdminUserCursor(c.t, c.ref)
			gotT, gotRef, err := decodeAdminUserCursor(cur)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !gotT.Equal(c.t) {
				t.Errorf("time mismatch: got %v want %v", gotT, c.t)
			}
			if gotRef != c.ref {
				t.Errorf("ref mismatch: got %d want %d", gotRef, c.ref)
			}
		})
	}
}

func TestAdminUserCursor_RejectsGarbage(t *testing.T) {
	cases := []string{
		"",                       // empty
		"not-base64-!@#",         // bad b64
		base64.RawURLEncoding.EncodeToString([]byte("only-one-part")), // missing |
		base64.RawURLEncoding.EncodeToString([]byte("bad-time|123")),  // unparseable time
		base64.RawURLEncoding.EncodeToString([]byte("2026-06-03T08:00:00Z|not-an-int")),
	}
	for _, c := range cases {
		_, _, err := decodeAdminUserCursor(c)
		if err == nil {
			t.Errorf("decodeAdminUserCursor(%q) = nil err, want error", c)
		}
	}
}

// status → approved mapping must match the RS convention exactly.
// A swap here (e.g., active=0 instead of 1) silently inverts every
// admin filter — pending users would show as active, etc.
func TestApprovedFromStatus(t *testing.T) {
	cases := []struct {
		in    openapi.ListAdminUsersParamsStatus
		want  int64
		valid bool
	}{
		{openapi.ListAdminUsersParamsStatusActive, 1, true},
		{openapi.ListAdminUsersParamsStatusPending, 0, true},
		{openapi.ListAdminUsersParamsStatusDisabled, 2, true},
	}
	for _, c := range cases {
		got, ok := approvedFromStatus(&c.in)
		if !ok {
			t.Errorf("approvedFromStatus(%q) ok=false, want true", c.in)
			continue
		}
		if got == nil {
			t.Errorf("approvedFromStatus(%q) nil, want %d", c.in, c.want)
			continue
		}
		if *got != c.want {
			t.Errorf("approvedFromStatus(%q) = %d, want %d", c.in, *got, c.want)
		}
	}
}

func TestApprovedFromStatus_Nil(t *testing.T) {
	got, ok := approvedFromStatus(nil)
	if !ok {
		t.Error("approvedFromStatus(nil) ok=false, want true (no-filter case)")
	}
	if got != nil {
		t.Errorf("approvedFromStatus(nil) = %v, want nil pointer", got)
	}
}

func TestStatusFromApproved(t *testing.T) {
	if got := statusFromApproved(1); got != openapi.AdminUserStatusActive {
		t.Errorf("approved=1 → %q, want active", got)
	}
	if got := statusFromApproved(0); got != openapi.AdminUserStatusPending {
		t.Errorf("approved=0 → %q, want pending", got)
	}
	if got := statusFromApproved(2); got != openapi.AdminUserStatusDisabled {
		t.Errorf("approved=2 → %q, want disabled", got)
	}
	// Out-of-range defensively maps to disabled — never to active.
	if got := statusFromApproved(99); got != openapi.AdminUserStatusDisabled {
		t.Errorf("approved=99 → %q, want disabled (defensive)", got)
	}
}

// 1.17.B — lifecycle mutation guards. The status mutation handler
// owns its own cap (users.approve), distinct from users.read. The
// guards must fire before the DB write so a misconfigured shim
// (Pool=nil) won't try to query — exact same pattern as the list
// endpoint's guards above.

func TestSetAdminUserStatus_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.SetAdminUserStatus(context.Background(), openapi.SetAdminUserStatusRequestObject{
		Ref:  42,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusActive},
	})
	if err != nil {
		t.Fatalf("SetAdminUserStatus: %v", err)
	}
	if _, ok := resp.(openapi.SetAdminUserStatus401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

// Caller has users.read but NOT users.approve — must 403, not
// silently allow. Separates the read-vs-mutate fence so a future
// "User Approver" role can't be inadvertently widened.
func TestSetAdminUserStatus_NeedsApproveCap(t *testing.T) {
	h := &Handler{}
	id := &auth.Identity{UserRef: 7, Capabilities: []string{CapReadUsers}} // read only
	ctx := auth.WithIdentity(context.Background(), id)
	resp, err := h.SetAdminUserStatus(ctx, openapi.SetAdminUserStatusRequestObject{
		Ref:  42,
		Body: &openapi.AdminUserStatusUpdate{Status: openapi.AdminUserStatusUpdateStatusDisabled},
	})
	if err != nil {
		t.Fatalf("SetAdminUserStatus: %v", err)
	}
	if _, ok := resp.(openapi.SetAdminUserStatus403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

// Body validation gates BEFORE the DB call. Missing body → 400 not
// 500; invalid enum → 400 not 500.
func TestSetAdminUserStatus_MissingBody(t *testing.T) {
	h := &Handler{}
	id := &auth.Identity{UserRef: 7, Capabilities: []string{CapApproveUsers}}
	ctx := auth.WithIdentity(context.Background(), id)
	resp, err := h.SetAdminUserStatus(ctx, openapi.SetAdminUserStatusRequestObject{Ref: 42, Body: nil})
	if err != nil {
		t.Fatalf("SetAdminUserStatus: %v", err)
	}
	if _, ok := resp.(openapi.SetAdminUserStatus400JSONResponse); !ok {
		t.Errorf("expected 400, got %T", resp)
	}
}

// Update-status enum bijection. The openapi generator emits a
// separate type per JSON property even for the same enum values,
// so the helper has to be a separate function from approvedFromStatus.
func TestApprovedFromUpdateStatus(t *testing.T) {
	cases := []struct {
		in   openapi.AdminUserStatusUpdateStatus
		want int64
	}{
		{openapi.AdminUserStatusUpdateStatusActive, 1},
		{openapi.AdminUserStatusUpdateStatusPending, 0},
		{openapi.AdminUserStatusUpdateStatusDisabled, 2},
	}
	for _, c := range cases {
		got, ok := approvedFromUpdateStatus(c.in)
		if !ok {
			t.Errorf("approvedFromUpdateStatus(%q) ok=false", c.in)
		}
		if got != c.want {
			t.Errorf("approvedFromUpdateStatus(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// Result-side enum mappings — both Status + PreviousStatus variants.
// A swap here would surface as silently incorrect badges in the
// admin chrome.
func TestStatusFromApprovedResult(t *testing.T) {
	if statusFromApprovedResult(1) != openapi.AdminUserStatusResultStatusActive {
		t.Error("approved=1 → not Active (result enum)")
	}
	if statusFromApprovedResult(0) != openapi.AdminUserStatusResultStatusPending {
		t.Error("approved=0 → not Pending (result enum)")
	}
	if statusFromApprovedResult(2) != openapi.AdminUserStatusResultStatusDisabled {
		t.Error("approved=2 → not Disabled (result enum)")
	}
	if statusFromApprovedResultPrevious(1) != openapi.AdminUserStatusResultPreviousStatusActive {
		t.Error("approved=1 → not Active (prev enum)")
	}
}

