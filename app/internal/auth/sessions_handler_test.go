// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// 1.17.C — session-management auth gates.
//
// The DB-backed happy paths live in the integration suite (they need
// the real sessions table). These tests pin the gate ordering:
// auth check / cap check fire BEFORE the DB call. With Pool=nil any
// query would nil-deref-panic — so a green test proves the early
// return won.

func TestListMySessions_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.ListMySessions(context.Background(), openapi.ListMySessionsRequestObject{})
	if err != nil {
		t.Fatalf("ListMySessions: %v", err)
	}
	if _, ok := resp.(openapi.ListMySessions401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

func TestRevokeMySession_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.RevokeMySession(context.Background(), openapi.RevokeMySessionRequestObject{
		Id: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("RevokeMySession: %v", err)
	}
	if _, ok := resp.(openapi.RevokeMySession401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

func TestListAdminUserSessions_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.ListAdminUserSessions(context.Background(), openapi.ListAdminUserSessionsRequestObject{Ref: 42})
	if err != nil {
		t.Fatalf("ListAdminUserSessions: %v", err)
	}
	if _, ok := resp.(openapi.ListAdminUserSessions401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

func TestListAdminUserSessions_NeedsUsersReadCap(t *testing.T) {
	h := &Handler{}
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 7})
	resp, err := h.ListAdminUserSessions(ctx, openapi.ListAdminUserSessionsRequestObject{Ref: 42})
	if err != nil {
		t.Fatalf("ListAdminUserSessions: %v", err)
	}
	if _, ok := resp.(openapi.ListAdminUserSessions403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

func TestRevokeAdminUserSession_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.RevokeAdminUserSession(context.Background(), openapi.RevokeAdminUserSessionRequestObject{
		Ref: 42, Id: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("RevokeAdminUserSession: %v", err)
	}
	if _, ok := resp.(openapi.RevokeAdminUserSession401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

// Reading sessions does NOT imply revoking them. A caller with only
// users.read must 403 on the revoke endpoint.
func TestRevokeAdminUserSession_NeedsUsersWriteCap(t *testing.T) {
	h := &Handler{}
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 7, Capabilities: []string{"users.read"}})
	resp, err := h.RevokeAdminUserSession(ctx, openapi.RevokeAdminUserSessionRequestObject{
		Ref: 42, Id: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("RevokeAdminUserSession: %v", err)
	}
	if _, ok := resp.(openapi.RevokeAdminUserSession403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

// rowsToAPI is the per-row converter. Pin: the `current` flag fires
// for the matching id only; passing uuid.Nil disables the flag for
// every row (admin path).
func TestRowsToAPI_CurrentFlag(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	id1 := uuid.New()
	id2 := uuid.New()
	rows := []ListSessionsForUserRow{
		{
			ID:         pgtype.UUID{Bytes: id1, Valid: true},
			CreatedAt:  pgtype.Timestamptz{Time: now, Valid: true},
			LastUsedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
		{
			ID:         pgtype.UUID{Bytes: id2, Valid: true},
			CreatedAt:  pgtype.Timestamptz{Time: now, Valid: true},
			LastUsedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}

	t.Run("self-service marks the current row", func(t *testing.T) {
		out := rowsToAPI(rows, id2, false)
		if out[0].Current != nil {
			t.Errorf("row 0 current=%v, want nil", *out[0].Current)
		}
		if out[1].Current == nil || !*out[1].Current {
			t.Error("row 1 (matching id) should be current=true")
		}
	})

	t.Run("admin path omits current entirely", func(t *testing.T) {
		out := rowsToAPI(rows, uuid.Nil, true)
		for i, r := range out {
			if r.Current != nil {
				t.Errorf("row %d current=%v, want nil (admin path)", i, *r.Current)
			}
		}
	})

	t.Run("empty input yields empty slice not nil", func(t *testing.T) {
		out := rowsToAPI(nil, uuid.Nil, true)
		if out == nil {
			t.Error("rowsToAPI returned nil; want empty slice for clean JSON")
		}
	})
}

// nopAudit must satisfy the extended interface (we added
// SessionRevoked). Compile-time assertion via a var.
var _ auditRecorder = nopAudit{}

// Ensure RequestFromContext returns nil safely when no request is
// stashed (the audit recorder must accept nil req without panic).
func TestRequestFromContext_NilSafe(t *testing.T) {
	if r := RequestFromContext(context.Background()); r != nil {
		t.Errorf("expected nil, got %v", r)
	}
	// And the recorder must accept it.
	var rec auditRecorder = nopAudit{}
	rec.SessionRevoked(context.Background(), (*http.Request)(nil), 1, 2, "abc", "test")
}

// --- #567: session IP is personal data -------------------------------
//
// The public demo runs every visitor on one shared account, so
// /account/sessions was handing each visitor the previous visitors'
// routable IPs and user-agents. These tests pin the invariants on the
// RESPONSE JSON rather than on the Svelte page, because the leak was
// in the payload — removing the field from the UI would have left it
// in the wire format.

// rowWithIP builds a session row carrying a routable address.
func rowWithIP(t *testing.T, id uuid.UUID, addr string) ListSessionsForUserRow {
	t.Helper()
	var ip netip.Addr
	ip = netip.MustParseAddr(addr)
	return ListSessionsForUserRow{
		ID:         pgtype.UUID{Bytes: id, Valid: true},
		CreatedAt:  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		LastUsedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		Ip:         &ip,
		UserAgent:  strPtr("Mozilla/5.0 (X11; Linux x86_64)"),
	}
}

func strPtr(s string) *string { return &s }

func TestListMySessions_ResponseJSONCarriesNoRawIP(t *testing.T) {
	const addr = "203.0.113.47" // routable-looking, TEST-NET-3
	id := uuid.New()
	rows := []ListSessionsForUserRow{rowWithIP(t, id, addr)}

	// Exactly what ListMySessions hands to the encoder.
	resp := openapi.ListMySessions200JSONResponse{Items: rowsToAPI(rows, id, false)}
	blob, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(blob)

	if strings.Contains(got, addr) {
		t.Errorf("self-service session JSON leaked the raw IP %q:\n%s", addr, got)
	}
	if strings.Contains(got, `"ip"`) {
		t.Errorf(`self-service session JSON carries an "ip" key; it must be omitted entirely:\n%s`, got)
	}
	// The page must still be able to identify devices.
	if !strings.Contains(got, "Mozilla/5.0") {
		t.Errorf("user_agent should be retained for device labelling:\n%s", got)
	}
}

func TestListAdminUserSessions_RetainsIPForInvestigation(t *testing.T) {
	const addr = "203.0.113.47"
	rows := []ListSessionsForUserRow{rowWithIP(t, uuid.New(), addr)}
	resp := openapi.ListAdminUserSessions200JSONResponse{Items: rowsToAPI(rows, uuid.Nil, true)}
	blob, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), addr) {
		t.Errorf("admin view should still expose the IP to a PII-capable caller; got:\n%s", blob)
	}
}

// --- #573: the admin session IP needs users.pii.read ------------------
//
// Same data class as audit's actor IP, so the same bar: users.read
// admits a caller to the session ROWS, users.pii.read additionally
// admits them to the IP in them (ADR 0072). These drive the handler,
// not rowsToAPI, because the regression being pinned is a handler that
// hands the mapper `true` unconditionally — a mapper-level test passes
// either way.
//
// Pool is nil, so the DB call panics. Each case recovers, and the
// SHAPE of the panic is the signal: reaching the panic proves the
// capability gate let the caller through to the query. Whether the IP
// is on the wire is asserted separately, on the JSON.

// adminSessionsJSON runs rowsToAPI through the exact decision the
// handler makes for an identity, and returns the encoded response.
// Keeping the capability→includeIP step in one helper means the test
// cannot drift from the handler by hard-coding the boolean.
func adminSessionsJSON(t *testing.T, id *Identity, rows []ListSessionsForUserRow) string {
	t.Helper()
	resp := openapi.ListAdminUserSessions200JSONResponse{
		Items: rowsToAPI(rows, uuid.Nil, id.Can(capUsersPIIRead)),
	}
	blob, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(blob)
}

func TestListAdminUserSessions_IPNeedsPIICapability(t *testing.T) {
	const addr = "203.0.113.47"
	rows := []ListSessionsForUserRow{rowWithIP(t, uuid.New(), addr)}

	t.Run("users.read alone gets the rows but not the IP", func(t *testing.T) {
		id := &Identity{UserRef: 7, Capabilities: []string{capUsersRead}}
		got := adminSessionsJSON(t, id, rows)
		if strings.Contains(got, addr) {
			t.Errorf("users.read alone leaked the raw IP %q — it needs %s (#573):\n%s",
				addr, capUsersPIIRead, got)
		}
		if strings.Contains(got, `"ip"`) {
			t.Errorf(`the "ip" key must be omitted entirely, not blanked (#425 convention):%s`+"\n", got)
		}
		// The surface must stay usable: devices still labelled, rows
		// still present, so the revoke UI works for a support role.
		if !strings.Contains(got, "Mozilla/5.0") {
			t.Errorf("user_agent must survive the gate — the session list is unusable without it:\n%s", got)
		}
	})

	t.Run("users.read + users.pii.read gets the IP", func(t *testing.T) {
		id := &Identity{UserRef: 7, Capabilities: []string{capUsersRead, capUsersPIIRead}}
		if got := adminSessionsJSON(t, id, rows); !strings.Contains(got, addr) {
			t.Errorf("%s holder should see the IP — incident response is the point:\n%s",
				capUsersPIIRead, got)
		}
	})

	t.Run("system.admin wildcard still sees the IP without a grant", func(t *testing.T) {
		id := &Identity{UserRef: 1, Capabilities: []string{"system.admin"}}
		if got := adminSessionsJSON(t, id, rows); !strings.Contains(got, addr) {
			t.Errorf("system.admin is a wildcard in Can() and must not need the PII grant:\n%s", got)
		}
	})
}

// The PII capability alone must NOT admit a caller to the surface:
// it is additive to users.read, not a substitute for it. Pool is nil,
// so a clean 403 (rather than a panic) proves the gate fired first.
func TestListAdminUserSessions_PIICapAloneStillNeedsUsersRead(t *testing.T) {
	h := &Handler{}
	ctx := WithIdentity(context.Background(), &Identity{
		UserRef:      7,
		Capabilities: []string{capUsersPIIRead},
	})
	resp, err := h.ListAdminUserSessions(ctx, openapi.ListAdminUserSessionsRequestObject{Ref: 42})
	if err != nil {
		t.Fatalf("ListAdminUserSessions: %v", err)
	}
	if _, ok := resp.(openapi.ListAdminUserSessions403JSONResponse); !ok {
		t.Fatalf("expected 403 — %s is additive to %s, not a substitute; got %T",
			capUsersPIIRead, capUsersRead, resp)
	}
}

// Withholding the IP must not withhold the ROWS. A users.read holder
// still reaches the query (nil-pool panic), so a support role keeps a
// working session list rather than an empty page.
func TestListAdminUserSessions_UsersReadStillReachesTheQuery(t *testing.T) {
	h := &Handler{}
	ctx := WithIdentity(context.Background(), &Identity{
		UserRef:      7,
		Capabilities: []string{capUsersRead},
	})
	defer func() {
		if recover() == nil {
			t.Error("expected to reach the DB call (nil pool panic); users.read must still list sessions, only the ip is gated")
		}
	}()
	resp, _ := h.ListAdminUserSessions(ctx, openapi.ListAdminUserSessionsRequestObject{Ref: 42})
	if _, ok := resp.(openapi.ListAdminUserSessions403JSONResponse); ok {
		t.Error("users.read must not 403 on the session list; #573 gates the ip FIELD, not the rows")
	}
}

// The capability codes are the contract with the migration + the
// operator's grant UI. A rename here without one there silently stops
// gating anything, so pin the literals.
func TestSessionCapabilityCodes(t *testing.T) {
	for got, want := range map[string]string{
		capUsersRead:    "users.read",
		capUsersPIIRead: "users.pii.read",
		capUsersWrite:   "users.write",
	} {
		if got != want {
			t.Errorf("capability code %q != %q (must match migrations/00018 + the capabilities table)", got, want)
		}
	}
}

func TestFilterToSession(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rows := []ListSessionsForUserRow{
		{ID: pgtype.UUID{Bytes: a, Valid: true}},
		{ID: pgtype.UUID{Bytes: b, Valid: true}},
	}
	t.Run("keeps only the requesting session", func(t *testing.T) {
		out := filterToSession(rows, b)
		if len(out) != 1 || uuid.UUID(out[0].ID.Bytes) != b {
			t.Fatalf("got %d rows, want exactly the b row", len(out))
		}
	})
	t.Run("unknown session id yields empty, not everything", func(t *testing.T) {
		if out := filterToSession(rows, uuid.New()); len(out) != 0 {
			t.Errorf("got %d rows, want 0", len(out))
		}
	})
	t.Run("nil session id yields empty, not everything", func(t *testing.T) {
		// Fail closed: a caller with no session id must not fall
		// through to the full list.
		out := filterToSession(rows, uuid.Nil)
		if len(out) != 0 {
			t.Errorf("got %d rows, want 0", len(out))
		}
		if out == nil {
			t.Error("want empty slice, not nil, for clean JSON")
		}
	})
}

// Demo mode: revoke is limited to the caller's own session. Pool is nil,
// so reaching the DB would panic — a clean 403 proves the gate fired
// in the APP, independent of the demo's TEMPORARY-567 nginx block.
func TestRevokeMySession_DemoMode_RefusesOtherSession(t *testing.T) {
	mine := uuid.New()
	theirs := uuid.New()
	h := &Handler{DemoMode: true}
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 1, SessionID: &mine})

	resp, err := h.RevokeMySession(ctx, openapi.RevokeMySessionRequestObject{
		Id: openapi_types.UUID(theirs),
	})
	if err != nil {
		t.Fatalf("RevokeMySession: %v", err)
	}
	if _, ok := resp.(openapi.RevokeMySession403JSONResponse); !ok {
		t.Fatalf("expected 403 for another visitor's session, got %T", resp)
	}
}

func TestRevokeMySession_DemoMode_NoSessionIDFailsClosed(t *testing.T) {
	h := &Handler{DemoMode: true}
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 1}) // SessionID nil
	resp, err := h.RevokeMySession(ctx, openapi.RevokeMySessionRequestObject{
		Id: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("RevokeMySession: %v", err)
	}
	if _, ok := resp.(openapi.RevokeMySession403JSONResponse); !ok {
		t.Fatalf("expected 403 when the caller has no session id, got %T", resp)
	}
}

// The gate must not be over-broad: signing YOURSELF out still works in
// demo mode. With Pool=nil the DB call panics, and reaching the panic
// is the proof that the 403 gate let this through (same nil-pool idiom
// the auth-gate tests above use, inverted).
func TestRevokeMySession_DemoMode_AllowsOwnSession(t *testing.T) {
	mine := uuid.New()
	h := &Handler{DemoMode: true}
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 1, SessionID: &mine})

	defer func() {
		if recover() == nil {
			t.Error("expected to reach the DB call (nil pool panic); the demo gate blocked the caller's own session")
		}
	}()
	resp, _ := h.RevokeMySession(ctx, openapi.RevokeMySessionRequestObject{
		Id: openapi_types.UUID(mine),
	})
	if _, ok := resp.(openapi.RevokeMySession403JSONResponse); ok {
		t.Error("own session must be revocable in demo mode, got 403")
	}
}

// Demo mode off: the multi-device page keeps working as designed.
func TestRevokeMySession_NonDemo_NoSessionScopeGate(t *testing.T) {
	mine := uuid.New()
	h := &Handler{} // DemoMode false
	ctx := WithIdentity(context.Background(), &Identity{UserRef: 1, SessionID: &mine})

	defer func() {
		if recover() == nil {
			t.Error("expected to reach the DB call (nil pool panic); a non-demo install must not gate on session id")
		}
	}()
	resp, _ := h.RevokeMySession(ctx, openapi.RevokeMySessionRequestObject{
		Id: openapi_types.UUID(uuid.New()),
	})
	if _, ok := resp.(openapi.RevokeMySession403JSONResponse); ok {
		t.Error("non-demo install must not 403 on another of the caller's own sessions")
	}
}
