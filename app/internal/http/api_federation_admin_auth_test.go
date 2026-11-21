// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Authorization of the federation outbox and inbox admin routes, at the
// handler. The server under test has no outbox admin store at all, so any
// store access before the gate panics on the nil handler; the panic is
// caught and reported as "reached the store".
//
//   - The two lists answer only federation.read (the seeded Auditor role,
//     #356 / #958) or system.admin, like the other federation admin read
//     surfaces: no identity => 401, an identity holding neither => 403.
//     Every filter and the cursor are set, so the gate has to come before
//     parsing too.
//   - The two writes (re-queue a failed outbox row, cancel a peer's queued
//     rows) stay on system.admin, as #356 keeps every write: no identity
//     => 401, anything less than system.admin => 403, the Auditor's
//     federation.read included.
//
// The wire contract with real rows (admin success, filters, cursors, the
// writes' side effects and audit rows) is the two-instance gate in
// app/internal/federation/twostack/admin_auth_gate_test.go.

package http

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func TestFederationAdminLists_RefuseBeforeTheStore(t *testing.T) {
	s := &apiServer{} // outboxAdmin is nil: touching the store panics

	peer := openapi_types.UUID(uuid.New())
	since := time.Now().Add(-time.Hour)
	limit := 5
	verb := "aa:Share"
	cursor := "not-a-cursor"
	outStatus := openapi.ListFederationOutboxParamsStatus("queued")
	inStatus := openapi.ListFederationInboxParamsStatus("pending")
	outReq := openapi.ListFederationOutboxRequestObject{Params: openapi.ListFederationOutboxParams{
		PeerId: &peer, Status: &outStatus, ActivityType: &verb, Since: &since, Limit: &limit, Cursor: &cursor}}
	inReq := openapi.ListFederationInboxRequestObject{Params: openapi.ListFederationInboxParams{
		PeerId: &peer, Status: &inStatus, ActivityType: &verb, Since: &since, Limit: &limit, Cursor: &cursor}}

	// status runs one handler call and names its answer: 401, 403, or
	// -1 when the call reached the (absent) store.
	status := func(t *testing.T, call func() (any, error)) (code int) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("reached the store before refusing: %v", r)
				code = -1
			}
		}()
		r, err := call()
		if err != nil {
			t.Fatalf("err %v", err)
		}
		switch v := r.(type) {
		case openapi.ListFederationOutbox401JSONResponse:
			if v.Error == "" {
				t.Error("401 without an error message")
			}
			return 401
		case openapi.ListFederationInbox401JSONResponse:
			if v.Error == "" {
				t.Error("401 without an error message")
			}
			return 401
		case openapi.ListFederationOutbox403JSONResponse:
			if v.Error == "" {
				t.Error("403 without an error message")
			}
			return 403
		case openapi.ListFederationInbox403JSONResponse:
			if v.Error == "" {
				t.Error("403 without an error message")
			}
			return 403
		}
		t.Errorf("unexpected answer %T", r)
		return 0
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"anonymous", context.Background(), 401},
		{"member without capabilities", withCaps(), 403},
		// Every other read capability, the Auditor's included: only
		// federation.read opens these two lists.
		{"other admin read capabilities", withCaps("featured.read", "requests.read", "system.activities.read",
			"system.license.read", "system.metadata_extraction.read", "system.jobs.read", "system.storage.read",
			"system.config.read"), 403},
	} {
		t.Run(c.name+"/outbox", func(t *testing.T) {
			if got := status(t, func() (any, error) { return s.ListFederationOutbox(c.ctx, outReq) }); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
		t.Run(c.name+"/inbox", func(t *testing.T) {
			if got := status(t, func() (any, error) { return s.ListFederationInbox(c.ctx, inReq) }); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestFederationAdminOutboxWrites_RefuseBeforeTheStore(t *testing.T) {
	s := &apiServer{} // outboxAdmin is nil: touching the store panics
	row := openapi_types.UUID(uuid.New())
	peer := openapi_types.UUID(uuid.New())

	// status runs one handler call and names its answer: 401, 403, or
	// -1 when the call reached the (absent) store.
	status := func(t *testing.T, call func() (any, error)) (code int) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("reached the store before refusing: %v", r)
				code = -1
			}
		}()
		r, err := call()
		if err != nil {
			t.Fatalf("err %v", err)
		}
		switch v := r.(type) {
		case openapi.RequeueFederationOutbox401JSONResponse:
			if v.Error == "" {
				t.Error("401 without an error message")
			}
			return 401
		case openapi.CancelFederationPeerPending401JSONResponse:
			if v.Error == "" {
				t.Error("401 without an error message")
			}
			return 401
		case openapi.RequeueFederationOutbox403JSONResponse:
			if v.Error == "" {
				t.Error("403 without an error message")
			}
			return 403
		case openapi.CancelFederationPeerPending403JSONResponse:
			if v.Error == "" {
				t.Error("403 without an error message")
			}
			return 403
		}
		t.Errorf("unexpected answer %T", r)
		return 0
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"anonymous", context.Background(), 401},
		{"member without capabilities", withCaps(), 403},
		{"auditor", withCaps("featured.read", "federation.read", "requests.read", "system.activities.read",
			"system.license.read", "system.metadata_extraction.read"), 403},
		{"every read capability", withCaps("featured.read", "federation.read", "requests.read", "system.activities.read",
			"system.license.read", "system.metadata_extraction.read", "system.jobs.read", "system.storage.read",
			"system.config.read"), 403},
	} {
		t.Run(c.name+"/requeue", func(t *testing.T) {
			if got := status(t, func() (any, error) {
				return s.RequeueFederationOutbox(c.ctx, openapi.RequeueFederationOutboxRequestObject{Id: row})
			}); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
		t.Run(c.name+"/cancel-pending", func(t *testing.T) {
			if got := status(t, func() (any, error) {
				return s.CancelFederationPeerPending(c.ctx, openapi.CancelFederationPeerPendingRequestObject{Id: peer})
			}); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}
