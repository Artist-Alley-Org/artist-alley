// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"context"
	"net/http"
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
		out := rowsToAPI(rows, id2)
		if out[0].Current != nil {
			t.Errorf("row 0 current=%v, want nil", *out[0].Current)
		}
		if out[1].Current == nil || !*out[1].Current {
			t.Error("row 1 (matching id) should be current=true")
		}
	})

	t.Run("admin path omits current entirely", func(t *testing.T) {
		out := rowsToAPI(rows, uuid.Nil)
		for i, r := range out {
			if r.Current != nil {
				t.Errorf("row %d current=%v, want nil (admin path)", i, *r.Current)
			}
		}
	})

	t.Run("empty input yields empty slice not nil", func(t *testing.T) {
		out := rowsToAPI(nil, uuid.Nil)
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
