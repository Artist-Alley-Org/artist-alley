// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package healthhandler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// fakeCounter is a stub Counter for tests.
type fakeCounter struct {
	snap SubsystemHealth
}

func (f fakeCounter) Snapshot() SubsystemHealth { return f.snap }

func TestHandlerFor_NoIdentity_Returns401(t *testing.T) {
	h := HandlerFor("test", fakeCounter{}, "system.admin")
	_, status := SnapshotFromContext(context.Background(), h)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func TestHandlerFor_MissingCapability_Returns403(t *testing.T) {
	h := HandlerFor("test", fakeCounter{}, "system.admin")
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: 1, Capabilities: []string{"something.else"}})
	_, status := SnapshotFromContext(ctx, h)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
}

func TestHandlerFor_AdminCap_Returns200_WithSubsystemSet(t *testing.T) {
	counter := fakeCounter{snap: SubsystemHealth{
		CounterTotal: 42,
		ByResult:     map[string]int64{"success": 40, "error": 2},
	}}
	h := HandlerFor("metadata-extraction", counter, "system.admin")
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: 1, Capabilities: []string{"system.admin"}})

	snap, status := SnapshotFromContext(ctx, h)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if snap == nil {
		t.Fatal("nil snapshot on 200")
	}
	if snap.Subsystem != "metadata-extraction" {
		t.Errorf("Subsystem = %q, want metadata-extraction (handler MUST override)", snap.Subsystem)
	}
	if snap.CounterTotal != 42 {
		t.Errorf("CounterTotal = %d, want 42", snap.CounterTotal)
	}
	if snap.ByResult["success"] != 40 {
		t.Errorf("by_result[success] = %d, want 40", snap.ByResult["success"])
	}
}

func TestHandlerFor_SuperAdmin_Bypasses_RequiredCap(t *testing.T) {
	// A user with auth.SuperAdminCapability gets through even
	// when they don't have the specific required cap.
	h := HandlerFor("test", fakeCounter{}, "custom.specific.cap")
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: 1, Capabilities: []string{auth.SuperAdminCapability}})
	_, status := SnapshotFromContext(ctx, h)
	if status != http.StatusOK {
		t.Errorf("super-admin should bypass; status = %d", status)
	}
}

func TestHandlerFor_RendersLastSuccessAndFailure(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	counter := fakeCounter{snap: SubsystemHealth{
		ByResult:    map[string]int64{"success": 1},
		LastSuccess: &now,
	}}
	h := HandlerFor("test", counter, "")
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{UserRef: 1})
	snap, status := SnapshotFromContext(ctx, h)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if snap.LastSuccess == nil || !snap.LastSuccess.Equal(now) {
		t.Errorf("LastSuccess = %v, want %v", snap.LastSuccess, now)
	}
}

func TestHandlerFor_EmptyRequiredCap_OnlyGatesOnIdentity(t *testing.T) {
	// requiredCap="" means "any authenticated user can read".
	// 1.19.A's email/health uses system.admin; but the shim
	// shouldn't FORCE a cap if a future subsystem wants
	// public-authenticated reads.
	h := HandlerFor("test", fakeCounter{}, "")
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: 1, Capabilities: []string{}})
	_, status := SnapshotFromContext(ctx, h)
	if status != http.StatusOK {
		t.Errorf("empty requiredCap should permit any authed caller; status = %d", status)
	}
}
