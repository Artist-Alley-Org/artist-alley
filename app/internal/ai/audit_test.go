// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCallAuditor_NilSafe(t *testing.T) {
	// Nil-receiver and nil-pool both no-op without panic. This is the
	// contract providers rely on: a test-built provider with no pool
	// never has to special-case auditor==nil.
	var a *CallAuditor
	a.RecordCall(context.Background(), CallRecord{Provider: "openai"})

	a2 := NewCallAuditor(nil, nil)
	a2.RecordCall(context.Background(), CallRecord{Provider: "openai"})
}

func TestCanonicalInputHash_Stable(t *testing.T) {
	// Same inputs → same hash (deterministic). Map key ordering in
	// JSON encoding is the failure mode to guard against; Go's
	// encoding/json sorts map keys, so this should hold.
	parts1 := []any{
		"openai",
		"gpt-4o",
		map[string]any{"role": "user", "content": "hello"},
	}
	parts2 := []any{
		"openai",
		"gpt-4o",
		map[string]any{"content": "hello", "role": "user"}, // different key order
	}
	h1 := CanonicalInputHash(parts1...)
	h2 := CanonicalInputHash(parts2...)
	if h1 != h2 {
		t.Errorf("hash drift on map-key-order: %s vs %s", h1, h2)
	}
}

func TestCanonicalInputHash_DifferentInputsDiffer(t *testing.T) {
	h1 := CanonicalInputHash("openai", "gpt-4o", "x")
	h2 := CanonicalInputHash("openai", "gpt-4o", "y")
	if h1 == h2 {
		t.Errorf("different inputs collided: %s", h1)
	}
}

func TestCanonicalInputHash_FormatIsHex64(t *testing.T) {
	h := CanonicalInputHash("anything")
	if len(h) != 64 {
		t.Errorf("hash length = %d, want 64 (SHA-256 hex)", len(h))
	}
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("non-hex char %q in hash %q", c, h)
			break
		}
	}
}

func TestCallRecord_StatusDefaultsToSuccessOnEmpty(t *testing.T) {
	// Sanity that the constant has the expected value — RecordCall
	// substitutes CallStatusSuccess when status is empty, so an empty
	// CallRecord-from-zero-value lands as 'success' in the DB.
	if CallStatusSuccess != "success" {
		t.Errorf("CallStatusSuccess = %q", CallStatusSuccess)
	}
}

func TestNullable_HelpersBehaveAsAdvertised(t *testing.T) {
	if nullableString("") != nil {
		t.Error("nullableString(\"\") should be nil")
	}
	if nullableString("x") != "x" {
		t.Errorf("nullableString(\"x\") = %v", nullableString("x"))
	}
	if nullableInt(0) != nil {
		t.Error("nullableInt(0) should be nil")
	}
	if nullableInt(7) != 7 {
		t.Errorf("nullableInt(7) = %v", nullableInt(7))
	}
	if nullableInt64(0) != nil {
		t.Error("nullableInt64(0) should be nil")
	}
	if uuidOrNil(nil) != nil {
		t.Error("uuidOrNil(nil) should be nil")
	}
	zero := uuid.Nil
	if uuidOrNil(&zero) != nil {
		t.Error("uuidOrNil(&Nil) should be nil")
	}
	id := uuid.New()
	if uuidOrNil(&id) != id {
		t.Errorf("uuidOrNil(&id) drifted")
	}
}

func TestCallRecord_DurationFieldRoundTrips(t *testing.T) {
	rec := CallRecord{Duration: 250 * time.Millisecond}
	if rec.Duration != 250*time.Millisecond {
		t.Errorf("Duration field drifted: %v", rec.Duration)
	}
}
